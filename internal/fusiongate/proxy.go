package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBufferedUpstreamBody = 32 << 20
const maxPendingStreamOutput = 1 << 20
const defaultFailoverStartTimeout = 12 * time.Second
const defaultFailoverIdleTimeout = 5 * time.Minute

type proxyOptions struct {
	Endpoint           string
	RawBody            []byte
	Stream             bool
	Transparent        bool
	UsageFormat        string
	GatewayID          string
	SafeTransportRetry bool
	OnFirstByte        func()
	UpstreamSSE        bool
	BufferSSE          bool
	SSETransform       func([]byte) ([]byte, string, Usage, error)
	JSONTransform      func([]byte) ([]byte, string, error)
	StreamTransform    func([]byte) ([]byte, error)
	OutputStartTimeout time.Duration
	IdleTimeout        time.Duration
}

type streamReadResult struct {
	data []byte
	err  error
}

func readStreamChunk(body io.Reader, size int, timeout time.Duration) ([]byte, error) {
	buffer := make([]byte, size)
	result := make(chan streamReadResult, 1)
	go func() {
		n, err := body.Read(buffer)
		result <- streamReadResult{data: buffer[:n], err: err}
	}()
	if timeout <= 0 {
		value := <-result
		return value.data, value.err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value.data, value.err
	case <-timer.C:
		return nil, context.DeadlineExceeded
	}
}

func readBodyWithContext(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	result := make(chan streamReadResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, limit))
		result <- streamReadResult{data: data, err: err}
	}()
	select {
	case value := <-result:
		return value.data, value.err
	case <-ctx.Done():
		// Closing an HTTP response body interrupts a blocked read. Wait for the
		// reader to exit so a slow non-streaming upstream cannot leak a goroutine.
		_ = body.Close()
		value := <-result
		return value.data, value.err
	}
}

type firstByteReadCloser struct {
	io.ReadCloser
	once        sync.Once
	onFirstByte func()
}

func (r *firstByteReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.onFirstByte != nil {
		r.once.Do(r.onFirstByte)
	}
	return n, err
}

func observeFirstByte(body io.ReadCloser, onFirstByte func()) io.ReadCloser {
	if body == nil || onFirstByte == nil {
		return body
	}
	return &firstByteReadCloser{ReadCloser: body, onFirstByte: onFirstByte}
}

// sseUsageObserver passively reads OpenAI-style SSE events for their final usage
// payload. It never changes the response bytes sent to the downstream client.
type sseUsageObserver struct {
	pending     []byte
	usage       Usage
	usageFormat string
}

const maxUsageSSEEvent = 1 << 20

func (o *sseUsageObserver) Write(p []byte) (int, error) {
	// Normalize only the observer copy so both LF and CRLF SSE delimiters work.
	o.pending = append(o.pending, bytes.ReplaceAll(p, []byte("\r"), nil)...)
	for {
		end := bytes.Index(o.pending, []byte("\n\n"))
		if end < 0 {
			if len(o.pending) > maxUsageSSEEvent {
				o.pending = o.pending[:0]
			}
			break
		}
		o.observeEvent(o.pending[:end])
		o.pending = o.pending[end+2:]
	}
	return len(p), nil
}

func (o *sseUsageObserver) finish() Usage {
	if len(o.pending) > 0 {
		o.observeEvent(o.pending)
		o.pending = nil
	}
	return o.usage
}

func (o *sseUsageObserver) observeEvent(event []byte) {
	if len(event) > maxUsageSSEEvent {
		return
	}
	var data []string
	for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	payload := strings.Join(data, "\n")
	if payload == "" || payload == "[DONE]" {
		return
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(payload), &decoded) != nil {
		return
	}
	var usage Usage
	switch o.usageFormat {
	case "anthropic":
		usage = parseAnthropicUsage(decoded)
	default:
		usage = parseOpenAIUsage(decoded)
	}
	if usage.Reported {
		mergeUsage(&o.usage, usage)
	}
}

func sseEventPayloads(body []byte) []string {
	normalized := bytes.ReplaceAll(body, []byte("\r"), nil)
	events := bytes.Split(normalized, []byte("\n\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) {
		events = events[:len(events)-1]
	}
	payloads := make([]string, 0, len(events))
	for _, event := range events {
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if payload := strings.Join(data, "\n"); payload != "" && payload != "[DONE]" {
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

// normalizeResponsesSSE makes upstream Responses frames safe for strict public
// API clients. Some gateways omit event lines and ChatGPT Codex adds private
// response fields that fail OpenCode's discriminated-union validation.
func normalizeResponsesSSE(body []byte) ([]byte, error) {
	normalized := bytes.ReplaceAll(body, []byte("\r"), nil)
	var out bytes.Buffer
	for _, event := range bytes.Split(normalized, []byte("\n\n")) {
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return nil, fmt.Errorf("invalid Responses SSE event: %w", err)
		}
		eventType := strings.TrimSpace(asString(decoded["type"]))
		if eventType == "" {
			return nil, errors.New("Responses SSE event is missing type")
		}
		if response := asMap(decoded["response"]); response != nil {
			delete(response, "moderation")
			delete(response, "tool_usage")
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", eventType, encoded)
	}
	return out.Bytes(), nil
}

type sseTransformWriter struct {
	dst       io.Writer
	transform func([]byte) ([]byte, error)
	pending   []byte
}

func (w *sseTransformWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, bytes.ReplaceAll(p, []byte("\r"), nil)...)
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		if end < 0 {
			break
		}
		event := append([]byte(nil), w.pending[:end+2]...)
		w.pending = w.pending[end+2:]
		transformed, err := w.transform(event)
		if err != nil {
			return len(p), err
		}
		if _, err := w.dst.Write(transformed); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (w *sseTransformWriter) Finish() error {
	if len(bytes.TrimSpace(w.pending)) == 0 {
		return nil
	}
	transformed, err := w.transform(w.pending)
	if err != nil {
		return err
	}
	_, err = w.dst.Write(transformed)
	w.pending = nil
	return err
}

func hasVisibleModelText(value map[string]any) bool {
	for _, key := range []string{"content", "delta", "text", "thinking", "reasoning", "reasoning_content", "refusal", "partial_json"} {
		if textContent(value[key]) != "" {
			return true
		}
	}
	return false
}

func hasSemanticStreamEvent(event map[string]any, format string) bool {
	if format == "anthropic" {
		if delta, _ := event["delta"].(map[string]any); hasVisibleModelText(delta) {
			return true
		}
		if block, _ := event["content_block"].(map[string]any); block["type"] == "tool_use" || hasVisibleModelText(block) {
			return true
		}
		return false
	}
	choices := anySlice(event["choices"])
	for _, value := range choices {
		choice, _ := value.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if hasVisibleModelText(delta) || len(anySlice(delta["tool_calls"])) > 0 || delta["function_call"] != nil || delta["audio"] != nil {
			return true
		}
		message, _ := choice["message"].(map[string]any)
		if hasVisibleModelText(message) || len(anySlice(message["tool_calls"])) > 0 || message["function_call"] != nil || message["audio"] != nil {
			return true
		}
	}
	eventType, _ := event["type"].(string)
	if (strings.Contains(eventType, "output_text") || strings.Contains(eventType, "reasoning") || strings.Contains(eventType, "thinking") || strings.Contains(eventType, "refusal")) && hasVisibleModelText(event) {
		return true
	}
	if item, _ := event["item"].(map[string]any); item != nil {
		if item["type"] == "reasoning" || item["type"] == "function_call" || item["type"] == "custom_tool_call" || item["type"] == "image_generation_call" {
			return true
		}
		for _, partValue := range anySlice(item["content"]) {
			part, _ := partValue.(map[string]any)
			if textContent(part["text"]) != "" || textContent(part["thinking"]) != "" {
				return true
			}
		}
	}
	return false
}

func semanticStreamProgress(body []byte, format string) int {
	progress := 0
	for _, payload := range sseEventPayloads(body) {
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if hasSemanticStreamEvent(event, format) {
			progress++
		}
	}
	return progress
}

func hasSemanticStreamOutput(body []byte, format string) bool {
	return semanticStreamProgress(body, format) > 0
}

type semanticStreamObserver struct {
	format  string
	pending []byte
}

func (o *semanticStreamObserver) observe(chunk []byte) bool {
	o.pending = append(o.pending, bytes.ReplaceAll(chunk, []byte("\r"), nil)...)
	progress := false
	for {
		end := bytes.Index(o.pending, []byte("\n\n"))
		if end < 0 {
			if len(o.pending) > maxUsageSSEEvent {
				o.pending = o.pending[len(o.pending)-maxUsageSSEEvent:]
			}
			break
		}
		event := o.pending[:end]
		o.pending = o.pending[end+2:]
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		payload := strings.Join(data, "\n")
		var decoded map[string]any
		if payload != "" && payload != "[DONE]" && json.Unmarshal([]byte(payload), &decoded) == nil && hasSemanticStreamEvent(decoded, o.format) {
			progress = true
		}
	}
	return progress
}

func hasSemanticJSONOutput(body []byte, format string) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if format == "anthropic" {
		for _, value := range anySlice(payload["content"]) {
			part, _ := value.(map[string]any)
			if part["type"] == "tool_use" || hasVisibleModelText(part) {
				return true
			}
		}
		return false
	}
	for _, value := range anySlice(payload["choices"]) {
		choice, _ := value.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if hasVisibleModelText(message) || len(anySlice(message["tool_calls"])) > 0 || message["function_call"] != nil || message["audio"] != nil {
			return true
		}
	}
	for _, value := range anySlice(payload["output"]) {
		item, _ := value.(map[string]any)
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" || item["type"] == "image_generation_call" {
			return true
		}
		for _, partValue := range anySlice(item["content"]) {
			part, _ := partValue.(map[string]any)
			if hasVisibleModelText(part) {
				return true
			}
		}
	}
	return len(anySlice(payload["data"])) > 0
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// These headers express gateway policy, not upstream application metadata. They
// are set before proxying by security and API middleware and must survive response
// forwarding unchanged.
var gatewayOwnedResponseHeaders = map[string]bool{
	"Access-Control-Allow-Credentials":     true,
	"Access-Control-Allow-Headers":         true,
	"Access-Control-Allow-Methods":         true,
	"Access-Control-Allow-Origin":          true,
	"Access-Control-Allow-Private-Network": true,
	"Access-Control-Expose-Headers":        true,
	"Access-Control-Max-Age":               true,
	"Cache-Control":                        true,
	"Content-Security-Policy":              true,
	"Referrer-Policy":                      true,
	"X-Content-Type-Options":               true,
	"X-Frame-Options":                      true,
}

func connectionHeaders(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return out
}

func copyUpstreamRequestHeaders(dst, src http.Header) {
	connectionSpecific := connectionHeaders(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonical] || connectionSpecific[canonical] {
			continue
		}
		switch strings.ToLower(canonical) {
		case "authorization", "x-api-key", "cookie", "host", "content-length", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via", "x-openai-internal-codex-responses-lite":
			continue
		}
		if strings.HasPrefix(strings.ToLower(canonical), "x-fusiongate-") {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func copyUpstreamResponseHeaders(dst, src http.Header) {
	connectionSpecific := connectionHeaders(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonical] || connectionSpecific[canonical] || gatewayOwnedResponseHeaders[canonical] || strings.EqualFold(canonical, "Set-Cookie") {
			continue
		}
		if canonical == "Vary" {
			for _, value := range values {
				dst.Add(canonical, value)
			}
			continue
		}
		dst.Del(canonical)
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

// joinEndpointPath appends an API endpoint to an upstream base path without
// duplicating the version prefix. OpenAI-compatible upstreams are commonly
// configured two ways: some store the bare origin (https://congee.pro) while
// others include the API version in the base URL (https://opencode.ai/zen/v1).
// When the base path already ends in /v1, /v1beta, or /api/v1 and the endpoint
// starts with that same version segment, the redundant prefix is dropped so the
// request lands on the correct upstream route instead of a doubled /v1/v1 path.
func joinEndpointPath(basePath, endpoint string) string {
	basePath = strings.TrimRight(basePath, "/")
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	for _, version := range []string{"/v1", "/v1beta", "/api/v1"} {
		if strings.HasSuffix(basePath, version) && strings.HasPrefix(endpoint, version+"/") {
			endpoint = strings.TrimPrefix(endpoint, version)
			break
		}
	}
	return basePath + endpoint
}

// upstreamIsEventStream reports whether an upstream response should be treated
// as a Server-Sent Events stream. Some OpenAI-compatible upstreams (notably the
// ChatGPT Codex backend) stream Responses SSE without a Content-Type header, and
// the HTTP transport may label the stream as plain text. When the caller
// explicitly requested SSE upstream (UpstreamSSE), trust that declaration and
// only refuse a payload that is clearly JSON.
func upstreamIsEventStream(contentType string, upstreamSSE bool) bool {
	trimmed := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(trimmed, "text/event-stream") {
		return true
	}
	return upstreamSSE && !strings.HasPrefix(trimmed, "application/json")
}

func joinURLQuery(base, endpoint, rawQuery string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = joinEndpointPath(u.Path, endpoint)
	if rawQuery != "" {
		if u.RawQuery == "" {
			u.RawQuery = rawQuery
		} else {
			u.RawQuery += "&" + rawQuery
		}
	}
	return u.String(), nil
}

func setProviderAuth(req *http.Request, z resolvedRoute) error {
	switch z.Provider.Type {
	case "openai", "grok", "openrouter", "openai_compatible", "codex_oauth", "grok_oauth":
		req.Header.Set("Authorization", "Bearer "+z.Credential)
		if z.Provider.Type == "codex_oauth" {
			if z.AuthCredential != nil && z.AuthCredential.AccountID != "" {
				req.Header.Set("ChatGPT-Account-ID", z.AuthCredential.AccountID)
			}
			setCodexClientHeaders(req.Header)
		}
		if z.Provider.Type == "grok_oauth" {
			setGrokClientHeaders(req.Header)
		}
	case "anthropic":
		req.Header.Set("x-api-key", z.Credential)
	case "claude_oauth":
		req.Header.Set("Authorization", "Bearer "+z.Credential)
		beta := req.Header.Get("Anthropic-Beta")
		if beta == "" {
			beta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"
		} else if !strings.Contains(beta, "oauth-2025-04-20") {
			beta += ",oauth-2025-04-20"
		}
		req.Header.Set("Anthropic-Beta", beta)
		if req.Header.Get("X-App") == "" {
			req.Header.Set("X-App", "cli")
		}
	case "gemini":
		query := req.URL.Query()
		query.Set("key", z.Credential)
		req.URL.RawQuery = query.Encode()
	default:
		return fmt.Errorf("provider type %q does not have an API credential adapter", z.Provider.Type)
	}
	return nil
}

func normalizedOpenAIBody(raw []byte, upstreamModel string, stream, includeStreamUsage bool) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = upstreamModel
	if stream {
		body["stream"] = true
	} else if _, exists := body["stream"]; exists {
		body["stream"] = false
	}
	if !includeStreamUsage {
		// The ChatGPT Codex backend rejects OpenAI API stream_options.
		delete(body, "stream_options")
	} else if stream {
		if streamOptions, ok := body["stream_options"].(map[string]any); ok {
			streamOptions["include_usage"] = true
		} else if _, exists := body["stream_options"]; !exists {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return json.Marshal(body)
}

func normalizedCompatibleChatBody(raw []byte) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if claude5Model(asString(body["model"])) {
		delete(body, "temperature")
		delete(body, "top_p")
	}
	for _, value := range anySlice(body["messages"]) {
		message, _ := value.(map[string]any)
		if message["role"] == "developer" {
			message["role"] = "system"
		}
	}
	normalizeCompatibleCacheControlTTL(body)
	return json.Marshal(body)
}

// collectCacheControls appends every cache_control object reachable from node,
// walking slices in order and descending only through "content" so the result
// keeps a deterministic, request-order sequence.
func collectCacheControls(node any, out *[]map[string]any) {
	switch value := node.(type) {
	case []any:
		for _, item := range value {
			collectCacheControls(item, out)
		}
	case map[string]any:
		if control, ok := value["cache_control"].(map[string]any); ok {
			*out = append(*out, control)
		}
		if content, ok := value["content"]; ok {
			collectCacheControls(content, out)
		}
	}
}

// applyCacheControlTTLOrder enforces Anthropic's rule that a ttl="1h"
// cache_control block must never follow a shorter one. Offending blocks are
// downgraded to "5m" rather than promoting the earlier blocks, so the request
// never caches anything for longer than the client asked for.
func applyCacheControlTTLOrder(blocks []map[string]any) bool {
	changed := false
	shortSeen := false
	for _, control := range blocks {
		if strings.EqualFold(strings.TrimSpace(asString(control["ttl"])), "1h") {
			if shortSeen {
				control["ttl"] = "5m"
				changed = true
			}
			continue
		}
		// An absent ttl defaults to the five minute cache.
		shortSeen = true
	}
	return changed
}

// normalizeCompatibleCacheControlTTL applies the TTL ordering rule to an
// OpenAI-compatible chat body. Upstreams that bridge to Anthropic lift
// system-role messages into the system array, so those are ordered ahead of the
// remaining conversation to match how the upstream will process them.
func normalizeCompatibleCacheControlTTL(body map[string]any) bool {
	var blocks []map[string]any
	collectCacheControls(body["tools"], &blocks)
	messages := anySlice(body["messages"])
	for _, value := range messages {
		if message, ok := value.(map[string]any); ok && message["role"] == "system" {
			collectCacheControls(message, &blocks)
		}
	}
	for _, value := range messages {
		if message, ok := value.(map[string]any); ok && message["role"] != "system" {
			collectCacheControls(message, &blocks)
		}
	}
	return applyCacheControlTTLOrder(blocks)
}

// normalizeAnthropicCacheControlTTL applies the TTL ordering rule to a native
// Anthropic Messages body, walking the documented processing order.
func normalizeAnthropicCacheControlTTL(body map[string]any) bool {
	var blocks []map[string]any
	collectCacheControls(body["tools"], &blocks)
	collectCacheControls(body["system"], &blocks)
	collectCacheControls(body["messages"], &blocks)
	return applyCacheControlTTLOrder(blocks)
}

func claude5Model(model string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(model)), "-")
	if len(parts) < 3 || parts[0] != "claude" || parts[2] != "5" {
		return false
	}
	switch parts[1] {
	case "fable", "haiku", "opus", "sonnet":
		return true
	default:
		return false
	}
}

func normalizedCodexResponsesBody(raw []byte, upstreamModel string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = upstreamModel
	switch input := body["input"].(type) {
	case string:
		body["input"] = []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": input,
			}},
		}}
	case map[string]any:
		body["input"] = []any{input}
	}
	// The ChatGPT Codex backend only accepts non-persistent streaming
	// Responses requests. FusionGate buffers the final completed event when
	// the downstream client requested a regular JSON response.
	body["store"] = false
	body["stream"] = true
	delete(body, "stream_options")
	// The ChatGPT Codex backend currently rejects the public Responses API
	// max_output_tokens field. Omit it rather than turning otherwise valid
	// OpenAI-compatible requests into HTTP 400 responses.
	delete(body, "max_output_tokens")
	return json.Marshal(body)
}

func codexResponsesBodyFromChat(raw []byte, upstreamModel string) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		return nil, err
	}
	input := make([]any, 0)
	for _, value := range anySlice(chat["messages"]) {
		message, _ := value.(map[string]any)
		role, _ := message["role"].(string)
		if role == "tool" {
			callID, _ := message["tool_call_id"].(string)
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": textContent(message["content"])})
			continue
		}
		if role != "assistant" && role != "system" && role != "developer" {
			role = "user"
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		content := make([]any, 0)
		switch source := message["content"].(type) {
		case string:
			if source != "" {
				content = append(content, map[string]any{"type": partType, "text": source})
			}
		case []any:
			for _, partValue := range source {
				part, _ := partValue.(map[string]any)
				switch asString(part["type"]) {
				case "text", "input_text", "output_text", "":
					if text := asString(part["text"]); text != "" {
						content = append(content, map[string]any{"type": partType, "text": text})
					}
				case "image_url", "input_image":
					if role != "user" {
						return nil, errors.New("Codex image input is supported only in user messages")
					}
					imageURL := strings.TrimSpace(asString(part["image_url"]))
					if nested := asMap(part["image_url"]); nested != nil {
						imageURL = strings.TrimSpace(asString(nested["url"]))
					}
					if imageURL == "" {
						return nil, errors.New("image_url.url is required")
					}
					image := map[string]any{"type": "input_image", "image_url": imageURL}
					if detail := strings.TrimSpace(asString(part["detail"])); detail != "" {
						image["detail"] = detail
					} else if nested := asMap(part["image_url"]); nested != nil {
						if detail := strings.TrimSpace(asString(nested["detail"])); detail != "" {
							image["detail"] = detail
						}
					}
					content = append(content, image)
				}
			}
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"role": role, "content": content})
		}
		for _, callValue := range anySlice(message["tool_calls"]) {
			call, _ := callValue.(map[string]any)
			function, _ := call["function"].(map[string]any)
			input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": function["arguments"]})
		}
	}
	body := map[string]any{"model": upstreamModel, "input": input, "store": false, "stream": true}
	if effort := strings.TrimSpace(asString(chat["reasoning_effort"])); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	} else if reasoning := asMap(chat["reasoning"]); reasoning != nil {
		if effort := strings.TrimSpace(asString(reasoning["effort"])); effort != "" {
			body["reasoning"] = map[string]any{"effort": effort}
		}
	}
	if tools := anySlice(chat["tools"]); len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			if tool["type"] == "function" && function != nil {
				converted = append(converted, map[string]any{"type": "function", "name": function["name"], "description": function["description"], "parameters": function["parameters"]})
			}
		}
		if len(converted) > 0 {
			body["tools"] = converted
		}
	}
	return json.Marshal(body)
}

func codexChatResponse(completed []byte, stream bool, publicModel string) ([]byte, string, error) {
	var response map[string]any
	if err := json.Unmarshal(completed, &response); err != nil {
		return nil, "", err
	}
	content := ""
	toolCalls := make([]any, 0)
	for _, value := range anySlice(response["output"]) {
		item, _ := value.(map[string]any)
		switch item["type"] {
		case "message":
			for _, partValue := range anySlice(item["content"]) {
				part, _ := partValue.(map[string]any)
				content += textContent(part["text"])
			}
		case "function_call", "custom_tool_call":
			toolCalls = append(toolCalls, map[string]any{"id": firstNonEmpty(asString(item["call_id"]), asString(item["id"])), "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
		}
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	usage := asMap(response["usage"])
	promptTokens := asInt64(usage["input_tokens"])
	completionTokens := asInt64(usage["output_tokens"])
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	id := firstNonEmpty(asString(response["id"]), "chatcmpl-codex")
	created := time.Now().Unix()
	if !stream {
		encoded, err := json.Marshal(map[string]any{"id": id, "object": "chat.completion", "created": created, "model": publicModel, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}}, "usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}})
		return encoded, "application/json", err
	}
	delta := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		delta["tool_calls"] = toolCalls
	}
	chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": publicModel, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}}, "usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil, "", err
	}
	return []byte("data: " + string(encoded) + "\n\ndata: [DONE]\n\n"), "text/event-stream", nil
}

func completedResponseFromSSE(body []byte) ([]byte, Usage, error) {
	normalized := bytes.ReplaceAll(body, []byte("\r"), nil)
	output := map[int]any{}
	var completed map[string]any
	for _, event := range bytes.Split(normalized, []byte("\n\n")) {
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(payload), &decoded) != nil {
			continue
		}
		switch decoded["type"] {
		case "response.output_item.done":
			index, ok := decoded["output_index"].(float64)
			item, itemOK := decoded["item"].(map[string]any)
			if ok && itemOK && index >= 0 && index == float64(int(index)) {
				output[int(index)] = item
			}
		case "response.completed":
			response, ok := decoded["response"].(map[string]any)
			if !ok {
				return nil, Usage{CostType: "unknown"}, fmt.Errorf("response.completed event did not include a response object")
			}
			completed = response
		}
	}
	if completed == nil {
		return nil, Usage{CostType: "unknown"}, fmt.Errorf("upstream stream ended without response.completed")
	}
	// The ChatGPT Codex backend emits completed output items as separate SSE
	// events but currently leaves response.completed.response.output empty.
	// Reassemble those items for downstream clients expecting normal JSON.
	if existing, ok := completed["output"].([]any); !ok || len(existing) == 0 {
		maxIndex := -1
		for index := range output {
			if index > maxIndex {
				maxIndex = index
			}
		}
		if maxIndex >= 0 {
			items := make([]any, maxIndex+1)
			for index, item := range output {
				items[index] = item
			}
			completed["output"] = items
		}
	}
	encoded, err := json.Marshal(completed)
	if err != nil {
		return nil, Usage{CostType: "unknown"}, err
	}
	return encoded, parseOpenAIUsage(completed), nil
}

func completedResponsesSSE(body []byte) ([]byte, string, Usage, error) {
	completed, usage, err := completedResponseFromSSE(body)
	return completed, "application/json", usage, err
}

type chatCompletionChoice struct {
	index        int
	message      map[string]any
	toolCalls    map[int]map[string]any
	finishReason any
	logprobs     any
}

func completedChatCompletionFromSSE(body []byte) ([]byte, string, Usage, error) {
	choices := map[int]*chatCompletionChoice{}
	usage := Usage{CostType: "unknown"}
	id := ""
	model := ""
	var created any
	var systemFingerprint any
	var serviceTier any
	var rawUsage map[string]any
	for _, payload := range sseEventPayloads(body) {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, "", usage, err
		}
		if upstreamError := asMap(chunk["error"]); upstreamError != nil {
			return nil, "", usage, fmt.Errorf("upstream stream error: %s", firstNonEmpty(asString(upstreamError["message"]), "unknown error"))
		}
		if value := asString(chunk["id"]); value != "" {
			id = value
		}
		if value := asString(chunk["model"]); value != "" {
			model = value
		}
		if value, ok := chunk["created"]; ok {
			created = value
		}
		if value, ok := chunk["system_fingerprint"]; ok {
			systemFingerprint = value
		}
		if value, ok := chunk["service_tier"]; ok {
			serviceTier = value
		}
		mergeUsage(&usage, parseOpenAIUsage(chunk))
		if value := asMap(chunk["usage"]); value != nil {
			rawUsage = cloneMap(value)
		}
		for _, rawChoice := range anySlice(chunk["choices"]) {
			piece := asMap(rawChoice)
			index := int(num(piece["index"]))
			choice := choices[index]
			if choice == nil {
				choice = &chatCompletionChoice{index: index, message: map[string]any{"role": "assistant", "content": nil}, toolCalls: map[int]map[string]any{}}
				choices[index] = choice
			}
			if reason, ok := piece["finish_reason"]; ok && reason != nil {
				choice.finishReason = reason
			}
			if logprobs, ok := piece["logprobs"]; ok && logprobs != nil {
				choice.logprobs = logprobs
			}
			delta := asMap(piece["delta"])
			if len(delta) == 0 {
				delta = asMap(piece["message"])
			}
			appendStringField(choice.message, "content", delta["content"])
			appendStringField(choice.message, "refusal", delta["refusal"])
			appendStringField(choice.message, "reasoning_content", delta["reasoning_content"])
			if role := asString(delta["role"]); role != "" {
				choice.message["role"] = role
			}
			if audio, ok := delta["audio"]; ok {
				choice.message["audio"] = audio
			}
			for _, rawCall := range anySlice(delta["tool_calls"]) {
				pieceCall := asMap(rawCall)
				callIndex := int(num(pieceCall["index"]))
				call := choice.toolCalls[callIndex]
				if call == nil {
					call = map[string]any{"type": "function", "function": map[string]any{}}
					choice.toolCalls[callIndex] = call
				}
				if callID := asStringValue(pieceCall["id"]); callID != "" {
					call["id"] = callID
				}
				if callType := asString(pieceCall["type"]); callType != "" {
					call["type"] = callType
				}
				function := asMap(call["function"])
				pieceFunction := asMap(pieceCall["function"])
				appendStringField(function, "name", pieceFunction["name"])
				appendStringField(function, "arguments", pieceFunction["arguments"])
			}
			if pieceFunction := asMap(delta["function_call"]); pieceFunction != nil {
				function := asMap(choice.message["function_call"])
				if function == nil {
					function = map[string]any{}
					choice.message["function_call"] = function
				}
				appendStringField(function, "name", pieceFunction["name"])
				appendStringField(function, "arguments", pieceFunction["arguments"])
			}
		}
	}
	if len(choices) == 0 {
		return nil, "", usage, errors.New("upstream stream ended without chat completion choices")
	}
	indices := make([]int, 0, len(choices))
	for index := range choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	completedChoices := make([]any, 0, len(indices))
	for _, index := range indices {
		choice := choices[index]
		if choice.finishReason == nil {
			return nil, "", usage, fmt.Errorf("upstream stream ended before choice %d completed", index)
		}
		if len(choice.toolCalls) > 0 {
			callIndices := make([]int, 0, len(choice.toolCalls))
			for callIndex := range choice.toolCalls {
				callIndices = append(callIndices, callIndex)
			}
			sort.Ints(callIndices)
			calls := make([]any, 0, len(callIndices))
			for _, callIndex := range callIndices {
				calls = append(calls, choice.toolCalls[callIndex])
			}
			choice.message["tool_calls"] = calls
		}
		completed := map[string]any{"index": choice.index, "message": choice.message, "finish_reason": choice.finishReason}
		if choice.logprobs != nil {
			completed["logprobs"] = choice.logprobs
		}
		completedChoices = append(completedChoices, completed)
	}
	response := map[string]any{"id": id, "object": "chat.completion", "model": model, "choices": completedChoices}
	if created != nil {
		response["created"] = created
	}
	if systemFingerprint != nil {
		response["system_fingerprint"] = systemFingerprint
	}
	if serviceTier != nil {
		response["service_tier"] = serviceTier
	}
	if usage.Reported {
		if rawUsage == nil {
			rawUsage = openAIUsagePayload(usage)
		}
		response["usage"] = rawUsage
	}
	encoded, err := json.Marshal(response)
	return encoded, "application/json", usage, err
}

func appendStringField(target map[string]any, key string, value any) {
	text, ok := value.(string)
	if !ok || text == "" {
		return
	}
	target[key] = asStringValue(target[key]) + text
}

func asStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func openAIUsagePayload(usage Usage) map[string]any {
	return map[string]any{
		"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": usage.Input + usage.Output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": usage.Cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": usage.Reasoning},
	}
}

func completedAnthropicMessageFromSSE(body []byte) ([]byte, string, Usage, error) {
	message := map[string]any{"type": "message", "role": "assistant", "content": []any{}, "stop_reason": nil, "stop_sequence": nil}
	blocks := map[int]map[string]any{}
	partialJSON := map[int]*strings.Builder{}
	usageFields := map[string]int64{}
	stopped := false
	for _, payload := range sseEventPayloads(body) {
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, "", Usage{CostType: "unknown"}, err
		}
		switch asString(event["type"]) {
		case "error":
			upstreamError := asMap(event["error"])
			return nil, "", Usage{CostType: "unknown"}, fmt.Errorf("upstream stream error: %s", firstNonEmpty(asString(upstreamError["message"]), "unknown error"))
		case "message_start":
			started := asMap(event["message"])
			for _, key := range []string{"id", "type", "role", "model", "stop_reason", "stop_sequence"} {
				if value, ok := started[key]; ok {
					message[key] = value
				}
			}
			mergeNumericFields(usageFields, asMap(started["usage"]))
			for index, rawBlock := range anySlice(started["content"]) {
				blocks[index] = cloneMap(asMap(rawBlock))
			}
		case "content_block_start":
			index := int(num(event["index"]))
			blocks[index] = cloneMap(asMap(event["content_block"]))
		case "content_block_delta":
			index := int(num(event["index"]))
			block := blocks[index]
			if block == nil {
				block = map[string]any{}
				blocks[index] = block
			}
			delta := asMap(event["delta"])
			switch asString(delta["type"]) {
			case "text_delta":
				appendStringField(block, "text", delta["text"])
			case "thinking_delta":
				appendStringField(block, "thinking", delta["thinking"])
			case "signature_delta":
				appendStringField(block, "signature", delta["signature"])
			case "input_json_delta":
				builder := partialJSON[index]
				if builder == nil {
					builder = &strings.Builder{}
					partialJSON[index] = builder
				}
				builder.WriteString(asStringValue(delta["partial_json"]))
			default:
				for key, value := range delta {
					if key != "type" {
						block[key] = value
					}
				}
			}
		case "message_delta":
			for key, value := range asMap(event["delta"]) {
				message[key] = value
			}
			mergeNumericFields(usageFields, asMap(event["usage"]))
		case "message_stop":
			stopped = true
		}
	}
	if !stopped {
		return nil, "", Usage{CostType: "unknown"}, errors.New("upstream stream ended without message_stop")
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	content := make([]any, 0, len(indices))
	for _, index := range indices {
		block := blocks[index]
		if builder := partialJSON[index]; builder != nil {
			var input any
			if err := json.Unmarshal([]byte(builder.String()), &input); err != nil {
				return nil, "", Usage{CostType: "unknown"}, fmt.Errorf("invalid tool input JSON: %w", err)
			}
			block["input"] = input
		}
		content = append(content, block)
	}
	message["content"] = content
	usagePayload := make(map[string]any, len(usageFields))
	for key, value := range usageFields {
		usagePayload[key] = value
	}
	message["usage"] = usagePayload
	usage := parseAnthropicUsage(message)
	encoded, err := json.Marshal(message)
	return encoded, "application/json", usage, err
}

func mergeNumericFields(target map[string]int64, source map[string]any) {
	for key, value := range source {
		if number := num(value); number > target[key] {
			target[key] = number
		}
	}
}

func providerContext(parent context.Context, p Provider) (context.Context, context.CancelFunc) {
	timeout := p.RequestTimeoutMS
	if timeout <= 0 {
		timeout = 120000
	}
	return context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
}

func downstreamCanceled(r *http.Request) bool {
	return r.Context().Err() != nil
}

func (a *App) proxyUpstream(w http.ResponseWriter, incoming *http.Request, z resolvedRoute, options proxyOptions) attemptResult {
	if err := a.ensureFreshProviderCredential(incoming.Context(), &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}
	}
	if options.Transparent && z.Route.PublicName != z.Route.UpstreamModel {
		return attemptResult{Status: http.StatusServiceUnavailable, Retryable: true, Reason: "route_configuration_error", Err: fmt.Errorf("transparent routes require public_name to equal upstream_model")}
	}
	endpoint := options.Endpoint
	if z.Provider.Type == "codex_oauth" && strings.HasPrefix(endpoint, "/v1/") {
		endpoint = strings.TrimPrefix(endpoint, "/v1")
	}
	upstreamURL, err := joinURLQuery(z.Provider.BaseURL, endpoint, incoming.URL.RawQuery)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if options.Stream || options.BufferSSE {
		ctx, cancel = context.WithCancel(incoming.Context())
	} else {
		ctx, cancel = providerContext(incoming.Context(), z.Provider)
	}
	defer cancel()
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	startTimeout := options.OutputStartTimeout
	if startTimeout <= 0 {
		startTimeout = a.cfg.StreamStartTimeout
		if startTimeout <= 0 {
			startTimeout = defaultFailoverStartTimeout
		}
		if !options.Stream && !options.BufferSSE && z.Provider.RequestTimeoutMS > 0 {
			// A non-streaming request often does not receive headers until the full
			// reasoning pass has completed. Honor the provider timeout instead of
			// applying the much shorter streaming first-output deadline.
			startTimeout = time.Duration(z.Provider.RequestTimeoutMS) * time.Millisecond
		}
		if options.BufferSSE && z.Provider.RequestTimeoutMS > 0 {
			// Internal streaming must not make a non-streaming client less tolerant
			// than it was before the gateway forced stream=true upstream.
			providerTimeout := time.Duration(z.Provider.RequestTimeoutMS) * time.Millisecond
			if providerTimeout > startTimeout {
				startTimeout = providerTimeout
			}
		}
	}
	started := time.Now()
	startTimer := time.AfterFunc(startTimeout, cancelAttempt)
	defer startTimer.Stop()
	req, err := http.NewRequestWithContext(attemptCtx, incoming.Method, upstreamURL, bytes.NewReader(options.RawBody))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	if _, present := incoming.Header["User-Agent"]; !present {
		// Suppress net/http's synthetic Go User-Agent when the real client sent none.
		req.Header.Set("User-Agent", "")
	}
	if !options.Transparent {
		// Let net/http negotiate and transparently decode gzip itself. Forwarding
		// the downstream Accept-Encoding header disables automatic decoding, which
		// makes buffered JSON responses look like invalid or empty model output.
		req.Header.Del("Accept-Encoding")
		req.Header.Set("Content-Type", "application/json")
		if options.UpstreamSSE {
			req.Header.Set("Accept", "text/event-stream")
		} else if !options.Stream {
			req.Header.Set("Accept", "application/json")
		} else if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}
		if (z.Provider.Type == "anthropic" || z.Provider.Type == "claude_oauth") && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "route_configuration_error", Err: err}
	}

	resp, err := a.doProviderRequest(req, z.Provider.IPPoolNodeID)
	if err != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		if attemptCtx.Err() != nil && ctx.Err() == nil {
			return attemptResult{Status: http.StatusGatewayTimeout, Retryable: options.SafeTransportRetry, Reason: "upstream_timeout", Err: context.DeadlineExceeded}
		}
		reason := retryReason(0, err)
		return attemptResult{Status: http.StatusBadGateway, Retryable: options.SafeTransportRetry, Reason: reason, Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, options.OnFirstByte)

	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		startTimer.Stop()
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		startTimer.Stop()
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		reason := retryReason(resp.StatusCode, nil)
		if copyErr != nil {
			reason = "downstream_write_error"
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: copyErr}
	}

	if options.BufferSSE && upstreamIsEventStream(resp.Header.Get("Content-Type"), options.UpstreamSSE) {
		var buffered bytes.Buffer
		var readErr error
		observer := semanticStreamObserver{format: options.UsageFormat}
		outputDeadline := started.Add(startTimeout)
		for !hasSemanticStreamOutput(buffered.Bytes(), options.UsageFormat) {
			remaining := time.Until(outputDeadline)
			if remaining <= 0 {
				return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
			}
			chunk, err := readStreamChunk(resp.Body, 32<<10, remaining)
			if len(chunk) > 0 {
				_, _ = buffered.Write(chunk)
				if buffered.Len() > maxBufferedUpstreamBody {
					return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_response_too_large", Err: fmt.Errorf("buffered SSE response exceeded %d bytes", maxBufferedUpstreamBody)}
				}
				observer.observe(chunk)
			}
			readErr = err
			if readErr != nil {
				break
			}
		}
		startTimer.Stop()
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
			}
			if attemptCtx.Err() != nil && ctx.Err() == nil {
				return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
			}
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_stream_interrupted", Err: readErr}
		}
		idleTimeout := options.IdleTimeout
		if idleTimeout <= 0 {
			idleTimeout = a.cfg.StreamIdleTimeout
			if idleTimeout <= 0 {
				idleTimeout = defaultFailoverIdleTimeout
			}
		}
		idleDeadline := time.Now().Add(idleTimeout)
		for readErr == nil {
			remaining := time.Until(idleDeadline)
			if remaining <= 0 {
				return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_stalled", Err: context.DeadlineExceeded}
			}
			chunk, err := readStreamChunk(resp.Body, 32<<10, remaining)
			if len(chunk) > 0 {
				_, _ = buffered.Write(chunk)
				if buffered.Len() > maxBufferedUpstreamBody {
					return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_response_too_large", Err: fmt.Errorf("buffered SSE response exceeded %d bytes", maxBufferedUpstreamBody)}
				}
				if observer.observe(chunk) {
					idleDeadline = time.Now().Add(idleTimeout)
				}
			}
			readErr = err
		}
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
			}
			if errors.Is(readErr, context.DeadlineExceeded) {
				return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_stalled", Err: readErr}
			}
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_stream_interrupted", Err: readErr}
		}
		if options.SSETransform == nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: errors.New("buffered SSE transform is not configured")}
		}
		completed, contentType, usage, parseErr := options.SSETransform(buffered.Bytes())
		if parseErr != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_stream", Err: parseErr}
		}
		cost(z, &usage)
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(completed)))
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(completed)
		reason := ""
		if writeErr != nil {
			reason = "downstream_write_error"
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Usage: usage, Reason: reason, Err: writeErr}
	}
	if options.Stream {
		var pending bytes.Buffer
		var readErr error
		outputDeadline := started.Add(startTimeout)
		for !options.Transparent && !hasSemanticStreamOutput(pending.Bytes(), options.UsageFormat) {
			remaining := time.Until(outputDeadline)
			if remaining <= 0 {
				return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
			}
			var chunk []byte
			chunk, readErr = readStreamChunk(resp.Body, 32<<10, remaining)
			if len(chunk) > 0 {
				_, _ = pending.Write(chunk)
				if pending.Len() > maxPendingStreamOutput {
					return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_no_output", Err: fmt.Errorf("upstream sent more than %d bytes without model output", maxPendingStreamOutput)}
				}
			}
			if readErr != nil {
				if downstreamCanceled(incoming) {
					return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
				}
				if !hasSemanticStreamOutput(pending.Bytes(), options.UsageFormat) {
					if errors.Is(readErr, context.DeadlineExceeded) || (attemptCtx.Err() != nil && ctx.Err() == nil) {
						return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: readErr}
					}
					reason := "upstream_no_output"
					if pending.Len() == 0 {
						reason = "upstream_empty_stream"
					}
					return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: reason, Err: readErr}
				}
				break
			}
		}
		if options.Transparent {
			chunk, err := readStreamChunk(resp.Body, 32<<10, 0)
			if len(chunk) > 0 {
				_, _ = pending.Write(chunk)
			}
			readErr = err
			if len(chunk) == 0 && readErr != nil {
				return attemptResult{Status: http.StatusBadGateway, Retryable: options.SafeTransportRetry, Reason: "upstream_empty_stream", Err: readErr}
			}
		}
		startTimer.Stop()
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		if options.StreamTransform != nil && !options.Transparent {
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Encoding")
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		var usageObserver *sseUsageObserver
		out := io.Writer(w)
		if options.UsageFormat != "" && !options.Transparent {
			usageObserver = &sseUsageObserver{usage: Usage{CostType: "unknown"}, usageFormat: options.UsageFormat}
			out = io.MultiWriter(w, usageObserver)
		}
		var transformWriter *sseTransformWriter
		if options.StreamTransform != nil && !options.Transparent {
			// Observe the normalized public stream, not private upstream fields.
			transformWriter = &sseTransformWriter{dst: w, transform: options.StreamTransform}
			if usageObserver != nil {
				transformWriter.dst = io.MultiWriter(w, usageObserver)
			}
			out = transformWriter
		}
		w.WriteHeader(resp.StatusCode)
		if pending.Len() > 0 {
			if _, err := out.Write(pending.Bytes()); err != nil {
				reason := "downstream_write_error"
				if transformWriter != nil {
					reason = "upstream_invalid_stream"
				}
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: reason, Err: err}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != io.EOF {
			idleTimeout := options.IdleTimeout
			if idleTimeout <= 0 {
				idleTimeout = a.cfg.StreamIdleTimeout
				if idleTimeout <= 0 {
					idleTimeout = defaultFailoverIdleTimeout
				}
			}
			observer := semanticStreamObserver{format: options.UsageFormat}
			observer.observe(pending.Bytes())
			idleDeadline := time.Now().Add(idleTimeout)
			for {
				remaining := time.Until(idleDeadline)
				if remaining <= 0 {
					return attemptResult{Status: http.StatusGatewayTimeout, Handled: true, Reason: "upstream_stalled", Err: context.DeadlineExceeded}
				}
				chunk, copyErr := readStreamChunk(resp.Body, 32<<10, remaining)
				if len(chunk) > 0 {
					if observer.observe(chunk) {
						idleDeadline = time.Now().Add(idleTimeout)
					}
					if _, err := out.Write(chunk); err != nil {
						return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: err}
					}
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}
				if copyErr == nil {
					continue
				}
				if errors.Is(copyErr, io.EOF) {
					break
				}
				if downstreamCanceled(incoming) {
					return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_canceled", Err: copyErr}
				}
				if errors.Is(copyErr, context.DeadlineExceeded) {
					return attemptResult{Status: http.StatusGatewayTimeout, Handled: true, Reason: "upstream_stalled", Err: copyErr}
				}
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "upstream_stream_interrupted", Err: copyErr}
			}
		}
		if transformWriter != nil {
			if err := transformWriter.Finish(); err != nil {
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "upstream_invalid_stream", Err: err}
			}
		}
		usage := Usage{CostType: "unknown"}
		if usageObserver != nil {
			usage = usageObserver.finish()
			cost(z, &usage)
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Usage: usage}
	}

	body, readErr := readBodyWithContext(attemptCtx, resp.Body, maxBufferedUpstreamBody+1)
	startTimer.Stop()
	if readErr != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
		}
		if attemptCtx.Err() != nil && ctx.Err() == nil {
			return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
		}
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, readErr), Err: readErr}
	}
	if len(body) > maxBufferedUpstreamBody {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Del("Content-Length")
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(body)
		if writeErr == nil {
			_, writeErr = io.Copy(w, resp.Body)
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: "large_response_streamed", Err: writeErr, Usage: Usage{CostType: "unknown"}}
	}
	if !options.Transparent && options.UsageFormat != "" && !hasSemanticJSONOutput(body, options.UsageFormat) {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_no_output", Err: errors.New("upstream response contained no model output")}
	}
	usage := Usage{CostType: "unknown"}
	if options.UsageFormat != "" && !options.Transparent {
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			switch options.UsageFormat {
			case "anthropic":
				usage = parseAnthropicUsage(payload)
			default:
				usage = parseOpenAIUsage(payload)
			}
			cost(z, &usage)
		}
	}
	contentType := resp.Header.Get("Content-Type")
	if options.JSONTransform != nil {
		transformed, transformedType, transformErr := options.JSONTransform(body)
		if transformErr != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: transformErr}
		}
		body = transformed
		contentType = transformedType
	}
	copyUpstreamResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Encoding")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
	w.WriteHeader(resp.StatusCode)
	_, writeErr := w.Write(body)
	return attemptResult{Status: resp.StatusCode, Handled: true, Usage: usage, Reason: func() string {
		if writeErr != nil {
			return "downstream_write_error"
		}
		return ""
	}(), Err: writeErr}
}
