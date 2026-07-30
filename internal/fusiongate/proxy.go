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
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBufferedUpstreamBody = 32 << 20
const maxPendingStreamOutput = 1 << 20

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
	BufferResponsesSSE bool
	ResponsesTransform func([]byte) ([]byte, string, error)
	OutputStartTimeout time.Duration
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

func hasVisibleModelText(value map[string]any) bool {
	for _, key := range []string{"content", "delta", "text", "reasoning", "reasoning_content", "refusal", "partial_json"} {
		if textContent(value[key]) != "" {
			return true
		}
	}
	return false
}

func hasSemanticStreamOutput(body []byte, format string) bool {
	for _, payload := range sseEventPayloads(body) {
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if format == "anthropic" {
			if delta, _ := event["delta"].(map[string]any); hasVisibleModelText(delta) {
				return true
			}
			if block, _ := event["content_block"].(map[string]any); block["type"] == "tool_use" || hasVisibleModelText(block) {
				return true
			}
			continue
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
		if (strings.Contains(eventType, "output_text") || strings.Contains(eventType, "refusal")) && hasVisibleModelText(event) {
			return true
		}
		if item, _ := event["item"].(map[string]any); item != nil {
			if item["type"] == "function_call" || item["type"] == "custom_tool_call" || item["type"] == "image_generation_call" {
				return true
			}
			for _, partValue := range anySlice(item["content"]) {
				part, _ := partValue.(map[string]any)
				if textContent(part["text"]) != "" {
					return true
				}
			}
		}
	}
	return false
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
		case "authorization", "x-api-key", "cookie", "host", "content-length", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via":
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
		if hopByHopHeaders[canonical] || connectionSpecific[canonical] || strings.EqualFold(canonical, "Set-Cookie") {
			continue
		}
		dst.Del(canonical)
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func joinURLQuery(base, endpoint, rawQuery string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
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
				text := textContent(part)
				if text != "" {
					content = append(content, map[string]any{"type": partType, "text": text})
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
	ctx, cancel := providerContext(incoming.Context(), z.Provider)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, incoming.Method, upstreamURL, bytes.NewReader(options.RawBody))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	if _, present := incoming.Header["User-Agent"]; !present {
		// Suppress net/http's synthetic Go User-Agent when the real client sent none.
		req.Header.Set("User-Agent", "")
	}
	if !options.Transparent {
		req.Header.Set("Content-Type", "application/json")
		if options.UpstreamSSE {
			req.Header.Set("Accept", "text/event-stream")
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

	resp, err := a.client.Do(req)
	if err != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		reason := retryReason(0, err)
		return attemptResult{Status: http.StatusBadGateway, Retryable: options.SafeTransportRetry, Reason: reason, Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, options.OnFirstByte)

	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
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

	if options.BufferResponsesSSE {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBufferedUpstreamBody+1))
		if readErr != nil {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
			}
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_stream_interrupted", Err: readErr}
		}
		if len(body) > maxBufferedUpstreamBody {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_response_too_large", Err: fmt.Errorf("buffered Responses stream exceeded %d bytes", maxBufferedUpstreamBody)}
		}
		completed, usage, parseErr := completedResponseFromSSE(body)
		if parseErr != nil {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_stream", Err: parseErr}
		}
		cost(z, &usage)
		contentType := "application/json"
		if options.ResponsesTransform != nil {
			completed, contentType, parseErr = options.ResponsesTransform(completed)
			if parseErr != nil {
				return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: parseErr}
			}
		}
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
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
		outputStartTimeout := options.OutputStartTimeout
		if outputStartTimeout <= 0 {
			outputStartTimeout = 30 * time.Second
		}
		outputDeadline := time.Now().Add(outputStartTimeout)
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
					if errors.Is(readErr, context.DeadlineExceeded) {
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
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		var usageObserver *sseUsageObserver
		out := io.Writer(w)
		if options.UsageFormat != "" && !options.Transparent {
			usageObserver = &sseUsageObserver{usage: Usage{CostType: "unknown"}, usageFormat: options.UsageFormat}
			out = io.MultiWriter(w, usageObserver)
		}
		if pending.Len() > 0 {
			if _, err := out.Write(pending.Bytes()); err != nil {
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: err}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != io.EOF {
			if _, copyErr := io.Copy(out, resp.Body); copyErr != nil {
				if downstreamCanceled(incoming) {
					return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_canceled", Err: copyErr}
				}
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "upstream_stream_interrupted", Err: copyErr}
			}
		}
		usage := Usage{CostType: "unknown"}
		if usageObserver != nil {
			usage = usageObserver.finish()
			cost(z, &usage)
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Usage: usage}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBufferedUpstreamBody+1))
	if readErr != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
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
	copyUpstreamResponseHeaders(w.Header(), resp.Header)
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
