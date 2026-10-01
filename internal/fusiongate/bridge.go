package fusiongate

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Wire protocols a client can speak and a channel can serve.
//
// A request is passed through byte for byte whenever the channel speaks the
// client's protocol. Only when it does not is the request bridged, and every
// bridge goes through one intermediate form, Chat Completions, so each protocol
// needs exactly one converter in each direction instead of one per pair.
const (
	wireChat      = "chat"
	wireResponses = "responses"
	wireMessages  = "messages"
	wireGemini    = "gemini"
)

const bridgeAdapterPrefix = "bridge:"

// clientWireProtocol names the protocol of a public inference path, or "" for
// endpoints that are only ever passed through (images, audio, count_tokens...).
func clientWireProtocol(path string) string {
	switch path {
	case "/v1/chat/completions":
		return wireChat
	case "/v1/responses":
		return wireResponses
	case "/v1/messages":
		return wireMessages
	}
	if _, _, ok := geminiPathModel(path); ok {
		return wireGemini
	}
	return ""
}

// geminiPathModel parses /v1beta/models/{model}:{method} (and the /v1 form).
func geminiPathModel(path string) (string, string, bool) {
	var rest string
	switch {
	case strings.HasPrefix(path, "/v1beta/models/"):
		rest = strings.TrimPrefix(path, "/v1beta/models/")
	case strings.HasPrefix(path, "/v1/models/"):
		rest = strings.TrimPrefix(path, "/v1/models/")
	default:
		return "", "", false
	}
	model, method, ok := strings.Cut(rest, ":")
	if !ok || model == "" || strings.Contains(model, "/") {
		return "", "", false
	}
	switch method {
	case "generateContent", "streamGenerateContent", "countTokens":
		return model, method, true
	}
	return "", "", false
}

// typeWireProtocols lists the protocols a provider type may speak natively, in
// preference order. The first entry that is not the client's own protocol is
// the bridge target.
//
// OpenAI-compatible relays are listed with every protocol they commonly serve:
// the client's own protocol is always tried first, byte for byte, and a relay
// that turns out not to serve it is bridged to Chat Completions and remembered.
func typeWireProtocols(kind string) []string {
	switch kind {
	case "openai_compatible", "openrouter", "opencode":
		return []string{wireChat, wireResponses, wireMessages}
	case "openai", "grok":
		// Often configured for third-party relays that serve more than OpenAI.
		return []string{wireChat, wireResponses, wireMessages}
	case "grok_oauth":
		return []string{wireChat, wireResponses}
	case "anthropic", "anthropic_compatible":
		// Anthropic and most Claude relays also expose an OpenAI-compatible Chat
		// Completions endpoint; Messages stays the bridge target.
		return []string{wireMessages, wireChat, wireResponses}
	case "claude_oauth":
		return []string{wireMessages}
	case "codex_oauth":
		return []string{wireResponses}
	}
	return nil
}

// routeWireProtocols applies the channel's configured protocol policy. A fixed
// preference the channel type can speak is an operator override: the channel is
// only spoken to in that protocol and everything else is bridged.
func routeWireProtocols(z resolvedRoute) []string {
	natives := typeWireProtocols(z.Provider.Type)
	fixed := ""
	switch fixedRouteProtocol(z) {
	case protocolChat:
		fixed = wireChat
	case protocolResponses:
		fixed = wireResponses
	case protocolMessages:
		fixed = wireMessages
	}
	if fixed != "" && containsString(natives, fixed) {
		return []string{fixed}
	}
	if z.Provider.Type == "opencode" {
		// OpenCode serves each model on one protocol; that one is the bridge
		// target, the others are still tried natively first.
		preferred := wireChat
		switch opencodeRouteProtocol(z) {
		case opencodeProtocolResponses:
			preferred = wireResponses
		case opencodeProtocolAnthropic:
			preferred = wireMessages
		}
		ordered := []string{preferred}
		for _, protocol := range natives {
			if protocol != preferred {
				ordered = append(ordered, protocol)
			}
		}
		return ordered
	}
	return natives
}

func bridgeAdapter(client, target string) string {
	return bridgeAdapterPrefix + client + "_to_" + target
}

func parseBridgeAdapter(adapter string) (string, string, bool) {
	if !strings.HasPrefix(adapter, bridgeAdapterPrefix) {
		return "", "", false
	}
	return strings.Cut(strings.TrimPrefix(adapter, bridgeAdapterPrefix), "_to_")
}

// wireMemory remembers, per channel and upstream model, the protocols that
// channel demonstrably does not serve, so the next request goes straight to the
// bridge instead of paying for the failed native attempt again.
type wireMemory struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

const wireMemoryTTL = 30 * time.Minute

var protocolMemory = &wireMemory{entries: map[string]time.Time{}}

// The credential contributes a stable identity, never a token value: see
// wireCredentialIdentity. Keying on the token made every refresh start from an
// empty learning state.
func wireMemoryKey(z resolvedRoute, protocol string) string {
	identity := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", z.Provider.ID, z.Provider.Type, z.Provider.BaseURL, z.Provider.ProtocolPolicy, z.Provider.ProtocolPreference, wireCredentialIdentity(z), z.Route.UpstreamModel)
	return fmt.Sprintf("%x:%s", sha256.Sum256([]byte(identity)), protocol)
}

func (m *wireMemory) unsupported(z resolvedRoute, protocol string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := wireMemoryKey(z, protocol)
	until, ok := m.entries[key]
	if ok && time.Now().After(until) {
		delete(m.entries, key)
		return false
	}
	return ok
}

func (m *wireMemory) remember(z resolvedRoute, protocol string) {
	m.rememberUntil(z, protocol, time.Now().Add(wireMemoryTTL))
}

// rememberUntil installs a fact with an explicit lifetime, so the in-memory index
// and its persisted copy expire at the same instant. It reports whether the fact
// was new, which lets a caller that also persists it write once per state change
// instead of once per request.
func (m *wireMemory) rememberUntil(z resolvedRoute, protocol string, until time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := wireMemoryKey(z, protocol)
	if existing, ok := m.entries[key]; ok && time.Now().Before(existing) {
		return false
	}
	if len(m.entries) >= 4096 {
		for entry, expiry := range m.entries {
			if time.Now().After(expiry) {
				delete(m.entries, entry)
			}
		}
		if len(m.entries) >= 4096 {
			for entry := range m.entries {
				delete(m.entries, entry)
				break
			}
		}
	}
	m.entries[key] = until
	return true
}

// restore installs a fact learned by an earlier process. An entry whose lifetime
// already elapsed is ignored rather than installed and swept on first read.
func (m *wireMemory) restore(key string, until time.Time) {
	if !until.After(time.Now()) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = until
}

func (m *wireMemory) forget(z resolvedRoute, protocol string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := wireMemoryKey(z, protocol)
	if _, ok := m.entries[key]; !ok {
		return false
	}
	delete(m.entries, key)
	return true
}

// planInferenceAdapter chooses native passthrough or a bridge for one route.
// An empty result means native (or identity) passthrough.
func planInferenceAdapter(z resolvedRoute, path string) string {
	client := clientWireProtocol(path)
	if client == "" {
		return ""
	}
	natives := routeWireProtocols(z)
	if containsString(natives, client) && !protocolMemory.unsupported(z, client) {
		return ""
	}
	return bridgeTarget(z, client, natives)
}

// fallbackInferenceAdapter is the bridge to use after a native attempt showed
// the channel does not serve the client's protocol.
func fallbackInferenceAdapter(z resolvedRoute, path string) string {
	client := clientWireProtocol(path)
	if client == "" {
		return ""
	}
	return bridgeTarget(z, client, routeWireProtocols(z))
}

// bridgeTarget chooses the protocol to convert into.
//
// Two things decide, in this order: the channel type's own preference order, and
// what the route's capability evidence names. A declared protocol is tried first,
// because honouring a declaration is what the operator asked for and it skips a
// call that would otherwise be spent discovering the same thing.
//
// A declaration never *removes* the type's other protocols from consideration.
// Declaring a native Responses endpoint on an Anthropic-compatible channel is an
// operator adding an option, not replacing Messages, so treating the declaration
// as an exhaustive list could take away the one target that actually answers. A
// protocol that has really been proven missing is excluded anyway, by the learned
// fact, and that exclusion expires.
func bridgeTarget(z resolvedRoute, client string, natives []string) string {
	declared := make([]string, 0, len(natives))
	others := make([]string, 0, len(natives))
	for _, target := range natives {
		if target == client {
			continue
		}
		if routeServesProtocol(z, target) {
			declared = append(declared, target)
		} else {
			others = append(others, target)
		}
	}
	for _, group := range [][]string{declared, others} {
		for _, target := range group {
			if !protocolMemory.unsupported(z, target) {
				return bridgeAdapter(client, target)
			}
		}
	}
	// Every candidate is remembered as unsupported. Retry one rather than
	// refusing: the TTL exists so a channel that added an endpoint, or had one
	// restored, is found again without an operator reset.
	for _, group := range [][]string{declared, others} {
		if len(group) > 0 {
			return bridgeAdapter(client, group[0])
		}
	}
	return ""
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// protocolUnsupportedSignal decides whether a failed native attempt means "this
// channel does not serve this protocol" rather than "this request is wrong".
// The body is only inspected for the ambiguous statuses and is left readable.
func protocolUnsupportedSignal(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusNotFound, http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnsupportedMediaType, http.StatusInternalServerError:
	default:
		return false
	}
	body := protocolErrorPayload(resp)
	var rejection struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &rejection) == nil && resp.StatusCode == http.StatusBadRequest && rejection.Error.Code == "gateway_provider_protocol_unavailable" {
		return true
	}
	text := strings.ToLower(string(body))
	// Only endpoint-level evidence is learnable. Parameter/model/auth errors
	// and generic server failures must never change routing capabilities.
	if resp.StatusCode >= 500 {
		return false
	}
	if containsAny(text, "parameter", "field", "argument", "model", "api key", "rate limit") {
		return false
	}
	if containsAny(text, "unknown endpoint", "unsupported endpoint", "endpoint not supported", "endpoint not found", "invalid url", "cannot post /", "404 page not found", "unsupported protocol", "protocol not supported") {
		return true
	}
	// A 404 that carries a structured JSON error is the channel's own API
	// answering, so the route is what is missing: Cline replies to /v1/responses
	// with a bare {"error":"Not Found","success":false} and relays answer the
	// same way. Requiring one of the phrases above instead made those channels
	// hand the 404 to the client and stop bridging. Anything that is not a JSON
	// error object stays inconclusive, so an opaque page or a plain "error A"
	// still fails over rather than re-probing the same channel.
	return resp.StatusCode == http.StatusNotFound && json.Valid(body) && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{"))
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// protocolEvidenceStatus reports whether a failed call on a *declared* endpoint
// proves that endpoint is not there.
//
// Only unambiguous answers count. A 404, 405, 415 or 501 says the endpoint does
// not exist. A 400 or 422 says either that or that the request was wrong, and only
// the body can tell the two apart, so that decision is left to
// protocolUnsupportedSignal, which inspects it. A 5xx or a timeout is health
// evidence, not protocol evidence: the circuit breaker already counts it and opens
// the channel's circuit, and learning "this protocol is unsupported" from it would
// suppress an endpoint that is merely unwell while saying nothing true about its
// protocol support.
func protocolEvidenceStatus(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusUnsupportedMediaType:
		return true
	}
	return false
}

// --- request conversion: client protocol -> chat -> upstream protocol -------

func bridgeClientToChat(client string, raw []byte, path string) (map[string]any, bool, map[string]bool, error) {
	return bridgeClientToChatFor(client, raw, path, bridgeOptions{})
}

// bridgeClientToChatFor converts a client request into the intermediate Chat
// form, under what the caller has explicitly accepted losing.
func bridgeClientToChatFor(client string, raw []byte, path string, opts bridgeOptions) (map[string]any, bool, map[string]bool, error) {
	var (
		encoded []byte
		stream  bool
		err     error
	)
	if err := validateBridgeCapabilitiesFor(client, raw, opts); err != nil {
		return nil, false, nil, err
	}
	custom := map[string]bool{}
	switch client {
	case wireChat:
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, false, nil, err
		}
		if err := normalizeChatCacheKey(body); err != nil {
			return nil, false, nil, err
		}
		stream, _ = body["stream"].(bool)
		return body, stream, custom, nil
	case wireResponses:
		var normalized []byte
		normalized, err = normalizeResponsesForChat(raw, custom)
		if err == nil {
			encoded, stream, err = compatibleResponsesBodyFromRequest(normalized, "")
		}
	case wireMessages:
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, false, nil, err
		}
		stream, _ = body["stream"].(bool)
		encoded, err = anthropicMessagesRequestToOpenAI(body, "", true, true)
	case wireGemini:
		_, method, _ := geminiPathModel(path)
		stream = method == "streamGenerateContent"
		encoded, err = geminiRequestToChat(raw)
	default:
		return nil, false, nil, fmt.Errorf("protocol %q cannot be bridged", client)
	}
	if err != nil {
		return nil, stream, nil, err
	}
	var chat map[string]any
	if err := json.Unmarshal(encoded, &chat); err != nil {
		return nil, stream, nil, err
	}
	delete(chat, "service_tier")
	chat["messages"] = mergeAssistantToolCalls(anySlice(chat["messages"]))
	return chat, stream, custom, nil
}

// normalizeResponsesForChat rewrites the Responses features that have no chat
// equivalent into ones that do. Custom (freeform) tools such as Codex's
// apply_patch become function tools taking one string argument; their calls
// and outputs become function calls and outputs. The names are recorded so the
// answer can be turned back into custom tool calls for the client.
func normalizeResponsesForChat(raw []byte, custom map[string]bool) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	tools := make([]any, 0)
	for _, value := range anySlice(source["tools"]) {
		tool := asMap(value)
		switch asString(tool["type"]) {
		case "custom":
			name := asString(tool["name"])
			custom[name] = true
			description := asString(tool["description"])
			if definition := asString(asMap(tool["format"])["definition"]); definition != "" {
				description = strings.TrimSpace(description + "\n\nThe input must follow this grammar:\n" + definition)
			}
			tools = append(tools, map[string]any{"type": "function", "name": name, "description": description, "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The raw tool input."}}, "required": []any{"input"},
			}})
		case "function":
			tools = append(tools, tool)
		}
	}
	source["tools"] = tools
	if choice := asMap(source["tool_choice"]); choice != nil && asString(choice["type"]) == "custom" {
		source["tool_choice"] = map[string]any{"type": "function", "name": choice["name"]}
	}
	if items, ok := source["input"].([]any); ok {
		for index, value := range items {
			item := asMap(value)
			switch asString(item["type"]) {
			case "custom_tool_call":
				arguments, _ := json.Marshal(map[string]any{"input": asString(item["input"])})
				items[index] = map[string]any{"type": "function_call", "call_id": firstNonEmpty(asString(item["call_id"]), asString(item["id"])), "name": item["name"], "arguments": string(arguments)}
			case "custom_tool_call_output":
				items[index] = map[string]any{"type": "function_call_output", "call_id": item["call_id"], "output": item["output"]}
			case "local_shell_call", "local_shell_call_output", "web_search_call", "image_generation_call", "compaction", "compaction_summary":
				items[index] = map[string]any{"type": "item_reference"}
			}
		}
	}
	if text := asMap(source["text"]); text != nil {
		format := asMap(text["format"])
		switch asString(format["type"]) {
		case "json_schema":
			schema := map[string]any{"name": firstNonEmpty(asString(format["name"]), "response"), "schema": format["schema"]}
			if strict, ok := format["strict"]; ok {
				schema["strict"] = strict
			}
			text["format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		case "json_object":
		default:
			delete(text, "format")
		}
	}
	return json.Marshal(source)
}

// mergeAssistantToolCalls folds consecutive assistant turns into one, so that
// parallel tool calls (one Responses item each) become one chat message whose
// tool results follow it, as chat upstreams require.
func mergeAssistantToolCalls(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, value := range messages {
		message := asMap(value)
		if n := len(out); n > 0 && asString(message["role"]) == "assistant" && len(anySlice(message["tool_calls"])) > 0 {
			previous := asMap(out[n-1])
			if asString(previous["role"]) == "assistant" {
				previous["tool_calls"] = append(anySlice(previous["tool_calls"]), anySlice(message["tool_calls"])...)
				if text := textContent(message["content"]); text != "" {
					previous["content"] = textContent(previous["content"]) + text
				}
				continue
			}
		}
		out = append(out, message)
	}
	return out
}

func bridgeUpstreamBody(target string, chat map[string]any, z resolvedRoute) ([]byte, string, error) {
	body := cloneMap(chat)
	body["model"] = z.Route.UpstreamModel
	body["stream"] = true
	switch target {
	case wireChat:
		body["stream_options"] = map[string]any{"include_usage": true}
		encoded, err := json.Marshal(body)
		return encoded, "/v1/chat/completions", err
	case wireResponses:
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		converted, err := chatToResponsesBody(encoded, z)
		return converted, inferenceUpstreamPath(z, "/v1/responses"), err
	case wireMessages:
		encoded, err := chatToAnthropicBody(body)
		return encoded, "/v1/messages", err
	}
	return nil, "", fmt.Errorf("protocol %q cannot be bridged to", target)
}

// chatToResponsesBody extends the Codex converter with the fields a Responses
// upstream needs: system text becomes instructions, and tool choice survives.
func chatToResponsesBody(chat []byte, z resolvedRoute) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(chat, &source); err != nil {
		return nil, err
	}
	var instructions []string
	messages := make([]any, 0)
	for _, value := range anySlice(source["messages"]) {
		message := asMap(value)
		if role := asString(message["role"]); role == "system" || role == "developer" {
			if text := strings.TrimSpace(textContent(message["content"])); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		messages = append(messages, value)
	}
	source["messages"] = messages
	stripped, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	encoded, err := codexResponsesBodyFromChat(stripped, z.Route.UpstreamModel)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		return nil, err
	}
	body["instructions"] = strings.Join(instructions, "\n\n")
	if cacheKey := source["prompt_cache_key"]; cacheKey != nil {
		body["prompt_cache_key"] = cacheKey
	}
	if choice := source["tool_choice"]; choice != nil {
		if named := asMap(asMap(choice)["function"]); named != nil {
			body["tool_choice"] = map[string]any{"type": "function", "name": named["name"]}
		} else if text, ok := choice.(string); ok {
			body["tool_choice"] = text
		}
	}
	if value, ok := source["parallel_tool_calls"]; ok {
		body["parallel_tool_calls"] = value
	}
	if z.Provider.Type != "codex_oauth" {
		for _, key := range []string{"temperature", "top_p"} {
			if value, ok := source[key]; ok {
				body[key] = value
			}
		}
		if value := firstPresent(source, "max_completion_tokens", "max_tokens"); value != nil {
			body["max_output_tokens"] = value
		}
	}
	return json.Marshal(body)
}

func firstPresent(source map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := source[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

// chatToAnthropicBody converts a chat request into a Messages request.
func chatToAnthropicBody(chat map[string]any) ([]byte, error) {
	var system []string
	messages := make([]any, 0)
	appendBlocks := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(messages); n > 0 {
			if last := asMap(messages[n-1]); asString(last["role"]) == role {
				last["content"] = append(anySlice(last["content"]), blocks...)
				return
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	for _, value := range anySlice(chat["messages"]) {
		message := asMap(value)
		switch role := asString(message["role"]); role {
		case "system", "developer":
			if text := strings.TrimSpace(textContent(message["content"])); text != "" {
				system = append(system, text)
			}
		case "tool":
			appendBlocks("user", []any{map[string]any{"type": "tool_result", "tool_use_id": asString(message["tool_call_id"]), "content": textContent(message["content"])}})
		case "assistant":
			blocks := make([]any, 0)
			if text := textContent(message["content"]); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, rawCall := range anySlice(message["tool_calls"]) {
				call := asMap(rawCall)
				function := asMap(call["function"])
				var input any = map[string]any{}
				if arguments := strings.TrimSpace(asString(function["arguments"])); arguments != "" {
					if json.Unmarshal([]byte(arguments), &input) != nil {
						input = map[string]any{"input": arguments}
					}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": firstNonEmpty(asString(call["id"]), "toolu_"+requestID()), "name": function["name"], "input": input})
			}
			appendBlocks("assistant", blocks)
		default:
			appendBlocks("user", chatContentToAnthropic(message["content"]))
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("request contains no messages")
	}
	body := map[string]any{"model": chat["model"], "messages": messages, "stream": true, "max_tokens": int64(8192)}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	if value := firstPresent(chat, "max_completion_tokens", "max_tokens"); value != nil && num(value) > 0 {
		body["max_tokens"] = num(value)
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := chat[key]; ok {
			body[key] = value
		}
	}
	switch stop := chat["stop"].(type) {
	case string:
		body["stop_sequences"] = []any{stop}
	case []any:
		body["stop_sequences"] = stop
	}
	tools := make([]any, 0)
	for _, value := range anySlice(chat["tools"]) {
		function := asMap(asMap(value)["function"])
		if function == nil {
			continue
		}
		schema := function["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tool := map[string]any{"name": function["name"], "input_schema": schema}
		if description := asString(function["description"]); description != "" {
			tool["description"] = description
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		body["tools"] = tools
		switch choice := chat["tool_choice"].(type) {
		case string:
			switch choice {
			case "required":
				body["tool_choice"] = map[string]any{"type": "any"}
			case "none":
				body["tool_choice"] = map[string]any{"type": "none"}
			}
		case map[string]any:
			if name := asString(asMap(choice["function"])["name"]); name != "" {
				body["tool_choice"] = map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return json.Marshal(body)
}

func chatContentToAnthropic(content any) []any {
	if text, ok := content.(string); ok {
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": text}}
	}
	blocks := make([]any, 0)
	for _, value := range anySlice(content) {
		part := asMap(value)
		switch asString(part["type"]) {
		case "text", "input_text":
			if text := asString(part["text"]); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		case "image_url", "input_image":
			url := asString(part["image_url"])
			if nested := asMap(part["image_url"]); nested != nil {
				url = asString(nested["url"])
			}
			if mediaType, data, ok := parseDataURL(url); ok {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
			} else if url != "" {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}})
			}
		}
	}
	return blocks
}

func parseDataURL(value string) (string, string, bool) {
	rest, ok := strings.CutPrefix(value, "data:")
	if !ok {
		return "", "", false
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), data, true
}

// --- response conversion: upstream protocol -> chat SSE ----------------------

// bridgeUpstreamToChat exposes any upstream answer as a Chat Completions SSE
// stream. Streams are converted event by event, so the client sees output as
// soon as the upstream produces it.
//
// The transport framing is decided by the body whenever the header is not an
// explicit SSE declaration. The ChatGPT Codex backend answers a streaming
// Responses request with a complete SSE stream but sends no Content-Type at
// all, so trusting the header made every bridged Codex call read an event
// stream as one JSON document and fail as upstream_invalid_response. Relays
// that mislabel the stream (for example as text/plain) failed the same way.
// Sniffing cannot misfire on a JSON answer: it never begins with "data:",
// "event:" or an SSE comment.
func bridgeUpstreamToChat(target string, resp *http.Response, publicModel string) io.ReadCloser {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isSSE := mediaType == "text/event-stream"
	if !isSSE {
		// Buffer once so the peek cannot consume bytes the converter still needs.
		buffered := bufio.NewReaderSize(resp.Body, 4096)
		resp.Body = &bufferedReadCloser{Reader: buffered, Closer: resp.Body}
		isSSE = looksLikeSSE(buffered)
	}
	if isSSE {
		if target == wireChat {
			return resp.Body
		}
		reader, writer := io.Pipe()
		go func() {
			defer resp.Body.Close()
			emitter := &chatChunkEmitter{w: writer, id: "chatcmpl-" + requestID(), model: publicModel, created: time.Now().Unix()}
			var err error
			switch target {
			case wireResponses:
				err = emitter.fromResponsesSSE(resp.Body)
			case wireMessages:
				err = emitter.fromAnthropicSSE(resp.Body)
			default:
				err = fmt.Errorf("protocol %q cannot be bridged from", target)
			}
			if err == nil {
				_, err = io.WriteString(writer, "data: [DONE]\n\n")
			}
			writer.CloseWithError(err)
		}()
		return reader
	}
	reader, writer := io.Pipe()
	go func() {
		defer resp.Body.Close()
		emitter := &chatChunkEmitter{w: writer, id: "chatcmpl-" + requestID(), model: publicModel, created: time.Now().Unix()}
		err := emitter.fromCompleted(target, resp.Body)
		if err == nil {
			_, err = io.WriteString(writer, "data: [DONE]\n\n")
		}
		writer.CloseWithError(err)
	}()
	return reader
}

// looksLikeSSE reports whether a buffered response body begins with an SSE
// field line. Peek does not advance the reader, so the converter still sees the
// complete body. A JSON answer never starts with "data:" or "event:".
func looksLikeSSE(buffered *bufio.Reader) bool {
	head, err := buffered.Peek(4096)
	if len(head) == 0 {
		return false
	}
	_ = err
	line := head
	if index := bytes.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	trimmed := strings.TrimSpace(string(line))
	return strings.HasPrefix(trimmed, "data:") ||
		strings.HasPrefix(trimmed, "event:") ||
		strings.HasPrefix(trimmed, ":")
}

// bufferedReadCloser keeps the peeked bytes reachable by the reader while
// preserving the original body's Close.
type bufferedReadCloser struct {
	*bufio.Reader
	io.Closer
}

type chatChunkEmitter struct {
	w       io.Writer
	id      string
	model   string
	created int64
}

func (e *chatChunkEmitter) chunk(delta map[string]any, finish any, usage map[string]any) error {
	payload := map[string]any{"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model}
	if delta != nil || finish != nil {
		if delta == nil {
			delta = map[string]any{}
		}
		payload["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	} else {
		payload["choices"] = []any{}
	}
	if usage != nil {
		payload["usage"] = usage
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.w, "data: %s\n\n", encoded)
	return err
}

func (e *chatChunkEmitter) errorChunk(message string) error {
	encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
	_, err := fmt.Fprintf(e.w, "data: %s\n\n", encoded)
	return err
}

func chatUsage(input, cached, output, reasoning int64) map[string]any {
	return map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
}

// forEachSSEEvent calls fn with the event name and decoded data of every event.
func forEachSSEEvent(r io.Reader, fn func(event string, data map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxAnthropicBridgeEvent)
	event := ""
	var data []string
	flush := func() (bool, error) {
		defer func() { event, data = "", data[:0] }()
		payload := strings.TrimSpace(strings.Join(data, "\n"))
		if payload == "" {
			return false, nil
		}
		if payload == "[DONE]" {
			return fn("[DONE]", nil)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return false, fmt.Errorf("invalid upstream SSE event: %w", err)
		}
		return fn(event, decoded)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			done, err := flush()
			if err != nil || done {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// EOF without a fully framed terminal event is truncation, not success.
	return io.ErrUnexpectedEOF
}

func (e *chatChunkEmitter) fromResponsesSSE(r io.Reader) error {
	type call struct {
		index    int
		streamed bool
	}
	calls := map[string]*call{}
	nextCall := 0
	sawText := false
	return forEachSSEEvent(r, func(event string, data map[string]any) (bool, error) {
		if event == "[DONE]" {
			return true, io.ErrUnexpectedEOF
		}
		kind := firstNonEmpty(asString(data["type"]), event)
		switch kind {
		case "response.output_text.delta":
			if delta := asString(data["delta"]); delta != "" {
				sawText = true
				return false, e.chunk(map[string]any{"content": delta}, nil, nil)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if delta := asString(data["delta"]); delta != "" {
				return false, e.chunk(map[string]any{"reasoning_content": delta}, nil, nil)
			}
		case "response.output_item.added", "response.output_item.done":
			item := asMap(data["item"])
			if itemType := asString(item["type"]); itemType != "function_call" && itemType != "custom_tool_call" {
				if kind == "response.output_item.done" && asString(item["type"]) == "message" && !sawText {
					for _, part := range anySlice(item["content"]) {
						if text := asString(asMap(part)["text"]); text != "" {
							sawText = true
							if err := e.chunk(map[string]any{"content": text}, nil, nil); err != nil {
								return false, err
							}
						}
					}
				}
				return false, nil
			}
			id := firstNonEmpty(asString(item["id"]), asString(item["call_id"]))
			entry := calls[id]
			if entry == nil {
				entry = &call{index: nextCall}
				nextCall++
				calls[id] = entry
				start := map[string]any{"index": entry.index, "id": firstNonEmpty(asString(item["call_id"]), id), "type": "function", "function": map[string]any{"name": item["name"], "arguments": ""}}
				if err := e.chunk(map[string]any{"tool_calls": []any{start}}, nil, nil); err != nil {
					return false, err
				}
			}
			if kind == "response.output_item.done" && !entry.streamed {
				arguments := asString(item["arguments"])
				if asString(item["type"]) == "custom_tool_call" {
					encoded, _ := json.Marshal(map[string]any{"input": item["input"]})
					arguments = string(encoded)
				}
				if arguments != "" {
					entry.streamed = true
					return false, e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": entry.index, "function": map[string]any{"arguments": arguments}}}}, nil, nil)
				}
			}
		case "response.function_call_arguments.delta":
			entry := calls[asString(data["item_id"])]
			if entry == nil {
				return false, nil
			}
			if delta := asString(data["delta"]); delta != "" {
				entry.streamed = true
				return false, e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": entry.index, "function": map[string]any{"arguments": delta}}}}, nil, nil)
			}
		case "response.incomplete":
			return true, errors.New("upstream response incomplete")
		case "response.completed":
			response := asMap(data["response"])
			finish := "stop"
			if len(calls) > 0 {
				finish = "tool_calls"
			}
			if kind == "response.incomplete" || asString(response["status"]) == "incomplete" {
				finish = "length"
			}
			if err := e.chunk(nil, finish, nil); err != nil {
				return false, err
			}
			usage := asMap(response["usage"])
			if usage != nil {
				return true, e.chunk(nil, nil, chatUsage(num(usage["input_tokens"]), num(asMap(usage["input_tokens_details"])["cached_tokens"]), num(usage["output_tokens"]), num(asMap(usage["output_tokens_details"])["reasoning_tokens"])))
			}
			return true, nil
		case "response.failed", "error":

			return true, errors.New("upstream response failed")
		}
		return false, nil
	})
}

func (e *chatChunkEmitter) fromAnthropicSSE(r io.Reader) error {
	tools := map[int]int{}
	nextTool := 0
	var input, cacheRead, cacheCreation, output int64
	var finish any
	return forEachSSEEvent(r, func(event string, data map[string]any) (bool, error) {
		if event == "[DONE]" {
			return true, io.ErrUnexpectedEOF
		}
		switch firstNonEmpty(asString(data["type"]), event) {
		case "message_start":
			usage := asMap(asMap(data["message"])["usage"])
			input, cacheRead, cacheCreation = num(usage["input_tokens"]), num(usage["cache_read_input_tokens"]), num(usage["cache_creation_input_tokens"])
			output = num(usage["output_tokens"])
		case "content_block_start":
			block := asMap(data["content_block"])
			if asString(block["type"]) == "tool_use" {
				index := nextTool
				nextTool++
				tools[int(num(data["index"]))] = index
				start := map[string]any{"index": index, "id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": ""}}
				return false, e.chunk(map[string]any{"tool_calls": []any{start}}, nil, nil)
			}
			if text := asString(block["text"]); text != "" {
				return false, e.chunk(map[string]any{"content": text}, nil, nil)
			}
		case "content_block_delta":
			delta := asMap(data["delta"])
			switch asString(delta["type"]) {
			case "text_delta":
				return false, e.chunk(map[string]any{"content": asString(delta["text"])}, nil, nil)
			case "thinking_delta":
				return false, e.chunk(map[string]any{"reasoning_content": asString(delta["thinking"])}, nil, nil)
			case "input_json_delta":
				index, ok := tools[int(num(data["index"]))]
				if ok && asString(delta["partial_json"]) != "" {
					return false, e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": asString(delta["partial_json"])}}}}, nil, nil)
				}
			}
		case "message_delta":
			finish = chatFinishFromAnthropic(asString(asMap(data["delta"])["stop_reason"]))
			if usage := asMap(data["usage"]); usage != nil {
				if value := num(usage["output_tokens"]); value > 0 {
					output = value
				}
				if value := num(usage["input_tokens"]); value > 0 {
					input = value
				}
				if value := num(usage["cache_read_input_tokens"]); value > 0 {
					cacheRead = value
				}
			}
		case "message_stop":
			if finish == nil {
				finish = "stop"
			}
			if err := e.chunk(nil, finish, nil); err != nil {
				return false, err
			}
			return true, e.chunk(nil, nil, chatUsage(input+cacheRead+cacheCreation, cacheRead, output, 0))
		case "error":
			return true, errors.New("upstream stream failed")
		}
		return false, nil
	})
}

func chatFinishFromAnthropic(reason string) any {
	switch reason {
	case "":
		return nil
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// fromCompleted handles an upstream that answered a stream request with one
// JSON document.
func (e *chatChunkEmitter) fromCompleted(target string, r io.Reader) error {
	body, err := io.ReadAll(io.LimitReader(r, maxPassthroughBody))
	if err != nil {
		return err
	}
	var chat map[string]any
	switch target {
	case wireResponses:
		converted, _, err := codexChatResponse(body, false, e.model)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(converted, &chat); err != nil {
			return err
		}
		var source map[string]any
		_ = json.Unmarshal(body, &source)
		usage := asMap(source["usage"])
		chat["usage"] = chatUsage(num(usage["input_tokens"]), num(asMap(usage["input_tokens_details"])["cached_tokens"]), num(usage["output_tokens"]), num(asMap(usage["output_tokens_details"])["reasoning_tokens"]))
	case wireMessages:
		var source map[string]any
		if err := json.Unmarshal(body, &source); err != nil {
			return err
		}
		chat = anthropicMessageToChat(source)
	default:
		if err := json.Unmarshal(body, &chat); err != nil {
			return err
		}
	}
	choices := anySlice(chat["choices"])
	if len(choices) == 0 {
		return errors.New("upstream response contained no choices")
	}
	choice := asMap(choices[0])
	message := asMap(choice["message"])
	delta := map[string]any{"role": "assistant"}
	for _, key := range []string{"content", "reasoning_content"} {
		if text := asString(message[key]); text != "" {
			delta[key] = text
		}
	}
	calls := make([]any, 0)
	for index, value := range anySlice(message["tool_calls"]) {
		call := cloneMap(asMap(value))
		call["index"] = index
		calls = append(calls, call)
	}
	if len(calls) > 0 {
		delta["tool_calls"] = calls
	}
	finish := choice["finish_reason"]
	if finish == nil {
		finish = "stop"
	}
	if err := e.chunk(delta, finish, nil); err != nil {
		return err
	}
	if usage := asMap(chat["usage"]); usage != nil {
		return e.chunk(nil, nil, usage)
	}
	return nil
}

func anthropicMessageToChat(source map[string]any) map[string]any {
	var text, thinking strings.Builder
	calls := make([]any, 0)
	for _, value := range anySlice(source["content"]) {
		block := asMap(value)
		switch asString(block["type"]) {
		case "text":
			text.WriteString(asString(block["text"]))
		case "thinking":
			thinking.WriteString(asString(block["thinking"]))
		case "tool_use":
			arguments, _ := json.Marshal(block["input"])
			calls = append(calls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": string(arguments)}})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if thinking.Len() > 0 {
		message["reasoning_content"] = thinking.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	usage := asMap(source["usage"])
	cacheRead := num(usage["cache_read_input_tokens"])
	input := num(usage["input_tokens"]) + cacheRead + num(usage["cache_creation_input_tokens"])
	finish := chatFinishFromAnthropic(asString(source["stop_reason"]))
	if finish == nil {
		finish = "stop"
	}
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   chatUsage(input, cacheRead, num(usage["output_tokens"]), 0),
	}
}

// --- client writers: chat SSE -> client protocol -----------------------------

// lazyStream commits the downstream response on its first write, so an
// upstream that fails before producing anything can still be retried.
type lazyStream struct {
	w           http.ResponseWriter
	contentType string
	rid         string
	committed   bool
	err         error
}

func (s *lazyStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if !s.committed {
		header := s.w.Header()
		header.Set("Content-Type", s.contentType)
		header.Set("Cache-Control", "no-cache")
		header.Set("X-FusionGate-Request-ID", s.rid)
		header.Del("Content-Length")
		s.w.WriteHeader(http.StatusOK)
		s.committed = true
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
		return n, err
	}
	if flusher, ok := s.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, nil
}

// streamResult converts the end state of a bridged stream into an attempt result.
func (s *lazyStream) result(err error) attemptResult {
	if s.err != nil {
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: s.err}
	}
	if err != nil {
		if !s.committed {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
		}
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "upstream_stream_interrupted", Err: err}
	}
	if !s.committed {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_empty_stream", Err: io.EOF}
	}
	return attemptResult{Status: http.StatusOK, Handled: true}
}

// forEachChatChunk decodes a chat SSE stream. An upstream error chunk ends it
// with an error.
func forEachChatChunk(r io.Reader, fn func(chunk map[string]any) error) error {
	return forEachSSEEvent(r, func(event string, chunk map[string]any) (bool, error) {
		if event == "[DONE]" {
			return true, nil
		}
		if upstreamError := asMap(chunk["error"]); upstreamError != nil {
			return true, fmt.Errorf("upstream stream error: %s", firstNonEmpty(asString(upstreamError["message"]), "unknown error"))
		}
		return false, fn(chunk)
	})
}

func writeChatStream(w http.ResponseWriter, r io.Reader, rid string) attemptResult {
	out := &lazyStream{w: w, contentType: "text/event-stream", rid: rid}
	err := forEachSSEEvent(io.TeeReader(r, io.Discard), func(event string, chunk map[string]any) (bool, error) {
		if event == "[DONE]" {
			return true, nil
		}
		if upstreamError := asMap(chunk["error"]); upstreamError != nil {
			return true, fmt.Errorf("upstream stream error: %s", firstNonEmpty(asString(upstreamError["message"]), "unknown error"))
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprintf(out, "data: %s\n\n", encoded)
		return false, err
	})
	if err == nil && out.committed {
		_, err = io.WriteString(out, "data: [DONE]\n\n")
	}
	return out.result(err)
}

// responsesStream renders chat chunks as an incremental Responses event stream.
type responsesStream struct {
	out      *lazyStream
	model    string
	id       string
	created  int64
	custom   map[string]bool
	sequence int
	next     int
	items    map[int]map[string]any
	text     *responsesOpenItem
	reason   *responsesOpenItem
	calls    map[int]*responsesOpenItem
	order    []int
	finish   string
	usage    map[string]any
	started  bool
}

type responsesOpenItem struct {
	index  int
	id     string
	buffer strings.Builder
	name   string
	callID string
	custom bool
	done   bool
}

func (s *responsesStream) emit(kind string, payload map[string]any) error {
	payload["type"] = kind
	payload["sequence_number"] = s.sequence
	s.sequence++
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.out, "event: %s\ndata: %s\n\n", kind, encoded)
	return err
}

func (s *responsesStream) response(status string) map[string]any {
	output := make([]any, 0, len(s.items))
	indices := make([]int, 0, len(s.items))
	for index := range s.items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		output = append(output, s.items[index])
	}
	response := map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": status, "model": s.model, "output": output, "parallel_tool_calls": true}
	if s.usage != nil {
		response["usage"] = s.usage
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return response
}

func (s *responsesStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	created := s.response("in_progress")
	created["output"] = []any{}
	if err := s.emit("response.created", map[string]any{"response": created}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": created})
}

func (s *responsesStream) closeText() error {
	item := s.text
	if item == nil {
		return nil
	}
	s.text = nil
	text := item.buffer.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	if err := s.emit("response.output_text.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": 0, "text": text}); err != nil {
		return err
	}
	if err := s.emit("response.content_part.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": 0, "part": part}); err != nil {
		return err
	}
	done := map[string]any{"id": item.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
	s.items[item.index] = done
	return s.emit("response.output_item.done", map[string]any{"output_index": item.index, "item": done})
}

func (s *responsesStream) closeReasoning() error {
	item := s.reason
	if item == nil {
		return nil
	}
	s.reason = nil
	text := item.buffer.String()
	part := map[string]any{"type": "summary_text", "text": text}
	if err := s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": 0, "text": text}); err != nil {
		return err
	}
	if err := s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": 0, "part": part}); err != nil {
		return err
	}
	done := map[string]any{"id": item.id, "type": "reasoning", "status": "completed", "summary": []any{part}}
	s.items[item.index] = done
	return s.emit("response.output_item.done", map[string]any{"output_index": item.index, "item": done})
}

func (s *responsesStream) closeCall(item *responsesOpenItem) error {
	if item.done {
		return nil
	}
	item.done = true
	arguments := item.buffer.String()
	var done map[string]any
	if item.custom {
		var decoded map[string]any
		input := arguments
		if json.Unmarshal([]byte(arguments), &decoded) == nil {
			if value, ok := decoded["input"].(string); ok {
				input = value
			}
		}
		done = map[string]any{"id": item.id, "type": "custom_tool_call", "status": "completed", "call_id": item.callID, "name": item.name, "input": input}
	} else {
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if err := s.emit("response.function_call_arguments.done", map[string]any{"item_id": item.id, "output_index": item.index, "arguments": arguments}); err != nil {
			return err
		}
		done = map[string]any{"id": item.id, "type": "function_call", "status": "completed", "call_id": item.callID, "name": item.name, "arguments": arguments}
	}
	s.items[item.index] = done
	return s.emit("response.output_item.done", map[string]any{"output_index": item.index, "item": done})
}

func (s *responsesStream) consume(chunk map[string]any) error {
	if usage := asMap(chunk["usage"]); usage != nil {
		s.usage = map[string]any{
			"input_tokens": num(usage["prompt_tokens"]), "output_tokens": num(usage["completion_tokens"]),
			"total_tokens":          num(usage["prompt_tokens"]) + num(usage["completion_tokens"]),
			"input_tokens_details":  map[string]any{"cached_tokens": num(asMap(usage["prompt_tokens_details"])["cached_tokens"])},
			"output_tokens_details": map[string]any{"reasoning_tokens": num(asMap(usage["completion_tokens_details"])["reasoning_tokens"])},
		}
	}
	choices := anySlice(chunk["choices"])
	if len(choices) == 0 {
		return nil
	}
	choice := asMap(choices[0])
	if reason := asString(choice["finish_reason"]); reason != "" {
		s.finish = reason
	}
	delta := asMap(choice["delta"])
	reasoning := firstNonEmpty(asString(delta["reasoning_content"]), asString(delta["reasoning"]))
	content := asString(delta["content"])
	calls := anySlice(delta["tool_calls"])
	if reasoning == "" && content == "" && len(calls) == 0 {
		return nil
	}
	if err := s.start(); err != nil {
		return err
	}
	if reasoning != "" {
		if s.reason == nil {
			if err := s.closeText(); err != nil {
				return err
			}
			s.reason = &responsesOpenItem{index: s.next, id: "rs_" + requestID()}
			s.next++
			item := map[string]any{"id": s.reason.id, "type": "reasoning", "status": "in_progress", "summary": []any{}}
			s.items[s.reason.index] = item
			if err := s.emit("response.output_item.added", map[string]any{"output_index": s.reason.index, "item": item}); err != nil {
				return err
			}
			if err := s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": s.reason.id, "output_index": s.reason.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}); err != nil {
				return err
			}
		}
		s.reason.buffer.WriteString(reasoning)
		if err := s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": s.reason.id, "output_index": s.reason.index, "summary_index": 0, "delta": reasoning}); err != nil {
			return err
		}
	}
	if content != "" {
		if s.text == nil {
			if err := s.closeReasoning(); err != nil {
				return err
			}
			s.text = &responsesOpenItem{index: s.next, id: "msg_" + requestID()}
			s.next++
			item := map[string]any{"id": s.text.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			s.items[s.text.index] = item
			if err := s.emit("response.output_item.added", map[string]any{"output_index": s.text.index, "item": item}); err != nil {
				return err
			}
			if err := s.emit("response.content_part.added", map[string]any{"item_id": s.text.id, "output_index": s.text.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
				return err
			}
		}
		s.text.buffer.WriteString(content)
		if err := s.emit("response.output_text.delta", map[string]any{"item_id": s.text.id, "output_index": s.text.index, "content_index": 0, "delta": content}); err != nil {
			return err
		}
	}
	for _, value := range calls {
		call := asMap(value)
		index := int(num(call["index"]))
		function := asMap(call["function"])
		item := s.calls[index]
		if item == nil {
			if err := s.closeReasoning(); err != nil {
				return err
			}
			if err := s.closeText(); err != nil {
				return err
			}
			item = &responsesOpenItem{index: s.next, id: "fc_" + requestID(), callID: firstNonEmpty(asString(call["id"]), "call_"+requestID())}
			s.next++
			item.name = asString(function["name"])
			item.custom = s.custom[item.name]
			s.calls[index] = item
			s.order = append(s.order, index)
			added := map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": item.callID, "name": item.name, "arguments": ""}
			if item.custom {
				added = map[string]any{"id": item.id, "type": "custom_tool_call", "status": "in_progress", "call_id": item.callID, "name": item.name, "input": ""}
			}
			s.items[item.index] = added
			if err := s.emit("response.output_item.added", map[string]any{"output_index": item.index, "item": added}); err != nil {
				return err
			}
		}
		arguments := asString(function["arguments"])
		if arguments == "" {
			continue
		}
		item.buffer.WriteString(arguments)
		if !item.custom {
			if err := s.emit("response.function_call_arguments.delta", map[string]any{"item_id": item.id, "output_index": item.index, "delta": arguments}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *responsesStream) finishStream() error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}
	for _, index := range s.order {
		if err := s.closeCall(s.calls[index]); err != nil {
			return err
		}
	}
	status := "completed"
	kind := "response.completed"
	if s.finish == "length" || s.finish == "content_filter" {
		status, kind = "incomplete", "response.incomplete"
	}
	return s.emit(kind, map[string]any{"response": s.response(status)})
}

func writeResponsesStream(w http.ResponseWriter, r io.Reader, model, rid string, custom map[string]bool) attemptResult {
	out := &lazyStream{w: w, contentType: "text/event-stream", rid: rid}
	id := "resp_" + strings.TrimPrefix(rid, "req_")
	state := &responsesStream{out: out, model: model, id: id, created: time.Now().Unix(), custom: custom, items: map[int]map[string]any{}, calls: map[int]*responsesOpenItem{}}
	err := forEachChatChunk(r, state.consume)
	if err == nil {
		if !state.started && state.finish == "" {
			return out.result(errors.New("upstream stream ended before model output"))
		}
		err = state.finishStream()
	} else if out.committed && out.err == nil {
		_ = state.emit("response.failed", map[string]any{"response": map[string]any{"id": id, "object": "response", "status": "failed", "model": model, "error": map[string]any{"code": "server_error", "message": err.Error()}}})
	}
	return out.result(err)
}

// completedResponsesFromChat renders a completed chat answer as a Responses
// object, restoring custom tool calls the bridge carried as functions.
func completedResponsesFromChat(chat []byte, model string, custom map[string]bool) ([]byte, string, error) {
	encoded, contentType, err := compatibleResponsesFromChat(chat, model, false)
	if err != nil || len(custom) == 0 {
		return encoded, contentType, err
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, "", err
	}
	for index, value := range anySlice(response["output"]) {
		item := asMap(value)
		if asString(item["type"]) != "function_call" || !custom[asString(item["name"])] {
			continue
		}
		var decoded map[string]any
		input := asString(item["arguments"])
		if json.Unmarshal([]byte(input), &decoded) == nil {
			if value, ok := decoded["input"].(string); ok {
				input = value
			}
		}
		response["output"].([]any)[index] = map[string]any{"id": item["id"], "type": "custom_tool_call", "status": "completed", "call_id": item["call_id"], "name": item["name"], "input": input}
	}
	encoded, err = json.Marshal(response)
	return encoded, contentType, err
}

// --- the bridge attempt ------------------------------------------------------

// bridgeAttempt serves one request through a protocol bridge.
func (run *inferenceRun) bridgeAttempt(w http.ResponseWriter, r *http.Request, z resolvedRoute, client, target string, onFirstByte func()) (attemptResult, context.CancelFunc) {
	a := run.app
	noop := func() {}
	conversionStart := time.Now()
	opts := bridgeOptionsForRequest(r)
	chat, stream, custom, err := bridgeClientToChatFor(client, run.raw, run.path, opts)
	if err == nil {
		var body []byte
		var path string
		body, path, err = bridgeUpstreamBody(target, chat, z)
		if err == nil {
			// The request is going upstream through the bridge: record what the
			// conversion had to leave behind, so an operator can see it in the
			// console instead of attaching a capture proxy to a live client.
			a.auditBridgeDrops(z, client, chat, opts)
			if d := diagnosticFor(r.Context()); d != nil {
				d.conversion(time.Since(conversionStart))
			}
			result, cancel := a.inferenceSend(r, body, z, path, stream, onFirstByte, true)
			if result.Response == nil || result.Status >= 400 {
				return result, cancel
			}
			resp := result.Response
			usage := &sseUsageObserver{usageFormat: "openai"}
			chatStream := bridgeUpstreamToChat(target, resp, z.Route.PublicName)
			defer chatStream.Close()
			observed := io.TeeReader(chatStream, usage)
			out := run.writeBridged(w, client, observed, z, stream, custom)
			cancel() // Do not drain an open-ended upstream after conversion settles.
			out.Usage = usage.finish()
			if out.Usage.Reported {
				cost(z, &out.Usage)
			}
			return out, cancel
		}
	}
	// The client's own request could not be expressed in the channel's protocol.
	// That is a request problem, reported once in the client's own terms.
	a.auditBridgeRefusal(z, client, err)
	failRequest(w, r, http.StatusBadRequest, "invalid_request", "request could not be converted for this channel: "+err.Error())
	return attemptResult{Status: http.StatusBadRequest, Handled: true, Reason: "bridge_conversion_failed", Err: err}, noop
}

func (run *inferenceRun) writeBridged(w http.ResponseWriter, client string, chat io.Reader, z resolvedRoute, stream bool, custom map[string]bool) attemptResult {
	model := z.Route.PublicName
	if !stream {
		body, err := io.ReadAll(io.LimitReader(chat, maxPassthroughBody))
		if err != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_read_error", Err: err}
		}
		completed, _, _, err := completedChatCompletionFromSSE(body)
		if err != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
		}
		var encoded []byte
		switch client {
		case wireChat:
			encoded, _, err = chatJSONWithPublicModel(completed, model)
		case wireResponses:
			encoded, _, err = completedResponsesFromChat(completed, model, custom)
		case wireMessages:
			encoded, err = openAIAsAnthropicJSON(completed, z, run.gatewayID)
		case wireGemini:
			encoded, err = geminiResponseFromChat(completed, model)
		}
		if err != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-FusionGate-Request-ID", run.gatewayID)
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(encoded); err != nil {
			return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: err}
		}
		return attemptResult{Status: http.StatusOK, Handled: true}
	}
	switch client {
	case wireChat:
		return writeChatStream(w, chat, run.gatewayID)
	case wireResponses:
		return writeResponsesStream(w, chat, model, run.gatewayID, custom)
	case wireMessages:
		start, idle := run.app.streamTimeouts(z.Provider)
		return streamOpenAIAsAnthropic(w, chat, z, run.gatewayID, start, idle, true)
	case wireGemini:
		return writeGeminiStream(w, chat, model, run.gatewayID, run.geminiSSE)
	}
	return attemptResult{Status: http.StatusNotImplemented, Reason: "protocol_not_supported", Err: fmt.Errorf("client protocol %q", client)}
}

// This denial is scoped to the API protocol, not the credential. Return it
// verbatim without attempting to evade policy or cooling down unrelated APIs.
func protocolPolicyDenied(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	body := protocolErrorPayload(resp)
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Error.Code == "gateway_protocol_policy_denied"
}

// Inspect compressed errors without changing the native response body/headers.
// Both wire and decoded peeks are bounded; truncated/unknown encodings are not
// evidence of unsupported protocols.
func protocolErrorPayload(resp *http.Response) []byte {
	wire, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	resp.Body = &prefixedPassthroughBody{Reader: io.MultiReader(bytes.NewReader(wire), resp.Body), Closer: resp.Body}
	if err != nil || len(wire) > 64<<10 {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return wire
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
		if err != nil || len(decoded) > 64<<10 {
			return nil
		}
		return decoded
	default:
		return nil
	}
}
