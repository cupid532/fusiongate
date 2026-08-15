package fusiongate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxAnthropicBridgeEvent = 4 << 20

// anthropicMessagesOpenAI bridges the Anthropic Messages API to an
// OpenAI-compatible Chat Completions provider. Native Anthropic routes continue
// to use the transparent proxy path in messages().
func (a *App) anthropicMessagesOpenAI(w http.ResponseWriter, incoming *http.Request, body map[string]any, z resolvedRoute, rid string, stream bool, onFirstByte func()) attemptResult {
	encoded, err := anthropicMessagesRequestToOpenAI(body, z.Route.UpstreamModel, true, z.Provider.Type != "openai_compatible" && z.Provider.Type != "opencode")
	if err != nil {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
	}
	if !stream {
		return a.proxyUpstream(w, incoming, z, proxyOptions{
			Endpoint: "/v1/chat/completions", RawBody: encoded, UsageFormat: "openai", GatewayID: rid,
			SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferSSE: true,
			SSETransform: func(body []byte) ([]byte, string, Usage, error) {
				chat, _, usage, err := completedChatCompletionFromSSE(body)
				if err != nil {
					return nil, "", usage, err
				}
				completed, err := openAIAsAnthropicJSON(chat, z, rid)
				return completed, "application/json", usage, err
			},
			JSONTransform: func(body []byte) ([]byte, string, error) {
				completed, err := openAIAsAnthropicJSON(body, z, rid)
				return completed, "application/json", err
			},
		})
	}
	if err := a.ensureFreshProviderCredential(incoming.Context(), &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}
	}
	upstreamURL, err := joinURLQuery(z.Provider.BaseURL, "/v1/chat/completions", incoming.URL.RawQuery)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	ctx, cancel := context.WithCancel(incoming.Context())
	defer cancel()
	startTimeout := a.cfg.StreamStartTimeout
	if startTimeout <= 0 {
		startTimeout = DefaultStreamStartTimeout
	}
	idleTimeout := a.cfg.StreamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultFailoverIdleTimeout
	}
	outputDeadline := time.Now().Add(startTimeout)
	startTimer := time.AfterFunc(startTimeout, cancel)
	defer startTimer.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	// This path parses and rewrites the upstream body. Do not forward a client or
	// reverse proxy's explicit compression negotiation: net/http only performs
	// transparent gzip decompression when it owns the Accept-Encoding header.
	req.Header.Del("Accept-Encoding")
	if _, present := incoming.Header["User-Agent"]; !present {
		req.Header.Set("User-Agent", "")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "route_configuration_error", Err: err}
	}

	resp, err := a.doProviderRequest(req, z.Provider.IPPoolNodeID)
	if err != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		if ctx.Err() != nil || time.Now().After(outputDeadline) {
			return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
		}
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, err), Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, onFirstByte)

	if retryableStatus(resp.StatusCode) {
		startTimer.Stop()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		startTimer.Stop()
		w.Header().Set("X-FusionGate-Request-ID", rid)
		if w.Header().Get("request-id") == "" {
			w.Header().Set("request-id", rid)
		}
		return writeAnthropicBridgeError(w, resp)
	}
	startTimer.Stop()
	result := streamOpenAIAsAnthropic(w, resp.Body, z, rid, time.Until(outputDeadline), idleTimeout)
	if result.Err != nil && downstreamCanceled(incoming) {
		return attemptResult{Status: http.StatusBadGateway, Handled: result.Handled, Reason: "downstream_canceled", Err: result.Err, Usage: result.Usage}
	}
	return result
}

func anthropicMessagesRequestToOpenAI(body map[string]any, upstreamModel string, stream, includeStreamUsage bool) ([]byte, error) {
	messages := make([]any, 0, 1)
	if system := strings.TrimSpace(textContent(body["system"])); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	incomingMessages, ok := body["messages"].([]any)
	if !ok {
		return nil, errors.New("messages must be an array")
	}
	for _, raw := range incomingMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("message must be an object")
		}
		role, _ := message["role"].(string)
		switch role {
		case "system":
			content := strings.TrimSpace(textContent(message["content"]))
			if content == "" {
				return nil, errors.New("system message content must contain text")
			}
			messages = append(messages, map[string]any{"role": "system", "content": content})
		case "assistant":
			converted, err := anthropicAssistantMessageToOpenAI(message["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted)
		case "user":
			converted, err := anthropicUserMessageToOpenAI(message["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted...)
		default:
			return nil, fmt.Errorf("unsupported message role %q", role)
		}
	}

	out := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   stream,
	}
	if maxTokens := num(body["max_tokens"]); maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, exists := body[key]; exists {
			out[key] = value
		}
	}
	if stops, ok := body["stop_sequences"].([]any); ok && len(stops) > 0 {
		out["stop"] = stops
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("tool must be an object")
			}
			function := map[string]any{"name": tool["name"], "parameters": tool["input_schema"]}
			if description, exists := tool["description"]; exists {
				function["description"] = description
			}
			converted = append(converted, map[string]any{"type": "function", "function": function})
		}
		out["tools"] = converted
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		switch choiceType, _ := choice["type"].(string); choiceType {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			name, _ := choice["name"].(string)
			if name == "" {
				return nil, errors.New("tool_choice.name is required")
			}
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		case "none":
			out["tool_choice"] = "none"
		}
		if disabled, _ := choice["disable_parallel_tool_use"].(bool); disabled {
			out["parallel_tool_calls"] = false
		}
	}
	if stream && includeStreamUsage {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

func anthropicAssistantMessageToOpenAI(content any) (map[string]any, error) {
	message := map[string]any{"role": "assistant"}
	if text, ok := content.(string); ok {
		message["content"] = text
		return message, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, errors.New("assistant content must be a string or array")
	}
	var text strings.Builder
	var toolCalls []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if value, _ := block["text"].(string); value != "" {
				text.WriteString(value)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			if id == "" || name == "" {
				return nil, errors.New("tool_use requires id and name")
			}
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}})
		}
	}
	message["content"] = text.String()
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message, nil
}

func anthropicUserMessageToOpenAI(content any) ([]any, error) {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, errors.New("user content must be a string or array")
	}
	out := make([]any, 0, len(blocks))
	pending := make([]any, 0, len(blocks))
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		contentValue := any(pending)
		allText := true
		var text strings.Builder
		for _, raw := range pending {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				allText = false
				break
			}
			value, _ := part["text"].(string)
			text.WriteString(value)
		}
		if allText {
			contentValue = text.String()
		}
		out = append(out, map[string]any{"role": "user", "content": contentValue})
		pending = nil
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			pending = append(pending, map[string]any{"type": "text", "text": block["text"]})
		case "image":
			image, err := anthropicImageToOpenAI(block)
			if err != nil {
				return nil, err
			}
			pending = append(pending, image)
		case "tool_result":
			flushPending()
			toolID, _ := block["tool_use_id"].(string)
			if toolID == "" {
				return nil, errors.New("tool_result.tool_use_id is required")
			}
			result := textContent(block["content"])
			if isError, _ := block["is_error"].(bool); isError {
				result = "Error: " + result
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": toolID, "content": result})
		}
	}
	flushPending()
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": ""})
	}
	return out, nil
}

func anthropicImageToOpenAI(block map[string]any) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, errors.New("image.source is required")
	}
	switch sourceType, _ := source["type"].(string); sourceType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType == "" || data == "" {
			return nil, errors.New("base64 image requires media_type and data")
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data}}, nil
	case "url":
		urlValue, _ := source["url"].(string)
		if urlValue == "" {
			return nil, errors.New("URL image requires url")
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": urlValue}}, nil
	default:
		return nil, fmt.Errorf("unsupported image source type %q", sourceType)
	}
}

func writeOpenAIAsAnthropic(w http.ResponseWriter, body io.Reader, z resolvedRoute, rid string) attemptResult {
	var source map[string]any
	if err := json.NewDecoder(body).Decode(&source); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
	}
	encoded, err := openAIAsAnthropicJSONMap(source, z, rid)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
	}
	usage := parseOpenAIUsage(source)
	cost(z, &usage)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(encoded)
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: usage, Reason: func() string {
		if writeErr != nil {
			return "downstream_write_error"
		}
		return ""
	}(), Err: writeErr}
}

func openAIAsAnthropicJSON(body []byte, z resolvedRoute, rid string) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, err
	}
	return openAIAsAnthropicJSONMap(source, z, rid)
}

func openAIAsAnthropicJSONMap(source map[string]any, z resolvedRoute, rid string) ([]byte, error) {
	choices, ok := source["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, errors.New("missing choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := make([]any, 0, 1)
	if text := openAIMessageText(message["content"]); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			arguments, _ := function["arguments"].(string)
			var input any = map[string]any{}
			if strings.TrimSpace(arguments) != "" && json.Unmarshal([]byte(arguments), &input) != nil {
				input = map[string]any{"_raw": arguments}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": function["name"], "input": input})
		}
	}
	stopReason := anthropicStopReason(choice["finish_reason"])
	return json.Marshal(map[string]any{
		"id": "msg_" + rid, "type": "message", "role": "assistant", "model": z.Route.PublicName,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": num(asMap(source["usage"])["prompt_tokens"]), "output_tokens": num(asMap(source["usage"])["completion_tokens"])},
	})
}

func openAIMessageText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	var out strings.Builder
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part["type"] == "text" {
			if text, _ := part["text"].(string); text != "" {
				out.WriteString(text)
			}
		}
	}
	return out.String()
}

func anthropicStopReason(value any) any {
	reason, _ := value.(string)
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "content_filter":
		return "refusal"
	case "":
		return nil
	default:
		return reason
	}
}

type openAIToolStream struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Started   bool
	Index     int
}

type anthropicBridgeStream struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	model      string
	messageID  string
	textOpen   bool
	textIndex  int
	nextIndex  int
	tools      map[int]*openAIToolStream
	toolOrder  []int
	finish     any
	usage      Usage
	writeError error
}

type anthropicBridgeTimedReader struct {
	body      io.Reader
	remaining func() time.Duration
}

func (r *anthropicBridgeTimedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	timeout := r.remaining()
	if timeout <= 0 {
		return 0, context.DeadlineExceeded
	}
	chunk, err := readStreamChunk(r.body, len(p), timeout)
	return copy(p, chunk), err
}

func hasAnthropicBridgeSemanticOutput(event map[string]any) bool {
	for _, value := range anySlice(event["choices"]) {
		choice, _ := value.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text, _ := delta["content"].(string); text != "" {
			return true
		}
		if len(anySlice(delta["tool_calls"])) > 0 {
			return true
		}
	}
	return false
}

func streamOpenAIAsAnthropic(w http.ResponseWriter, body io.Reader, z resolvedRoute, rid string, startTimeout, idleTimeout time.Duration) attemptResult {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "streaming_unsupported", Err: errors.New("response writer does not support flushing")}
	}
	state := &anthropicBridgeStream{w: w, flusher: flusher, model: z.Route.PublicName, messageID: "msg_" + rid, textIndex: -1, tools: map[int]*openAIToolStream{}}
	if startTimeout <= 0 {
		return attemptResult{Status: http.StatusGatewayTimeout, Retryable: true, Reason: "upstream_output_timeout", Err: context.DeadlineExceeded}
	}
	if idleTimeout <= 0 {
		idleTimeout = defaultFailoverIdleTimeout
	}
	pending := make([]map[string]any, 0)
	committed := false
	sawPayload := false
	pendingBytes := 0
	deadline := time.Now().Add(startTimeout)

	commit := func() error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-FusionGate-Request-ID", rid)
		if w.Header().Get("request-id") == "" {
			w.Header().Set("request-id", rid)
		}
		w.WriteHeader(http.StatusOK)
		committed = true
		state.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": state.messageID, "type": "message", "role": "assistant", "model": state.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		}})
		if state.writeError != nil {
			return state.writeError
		}
		for _, event := range pending {
			state.consume(event)
			if state.writeError != nil {
				return state.writeError
			}
		}
		pending = nil
		deadline = time.Now().Add(idleTimeout)
		return nil
	}

	var dataLines []string
	finishEvent := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		sawPayload = true
		if payload == "[DONE]" {
			if !committed {
				return false, errors.New("upstream stream ended before model output")
			}
			return true, nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return false, fmt.Errorf("invalid upstream SSE event: %w", err)
		}
		if upstreamError, exists := event["error"]; exists && upstreamError != nil {
			encoded, _ := json.Marshal(upstreamError)
			return false, fmt.Errorf("upstream error event: %s", encoded)
		}
		semantic := hasAnthropicBridgeSemanticOutput(event)
		if !committed {
			pending = append(pending, event)
			if !semantic {
				return false, nil
			}
			return false, commit()
		}
		state.consume(event)
		if semantic {
			deadline = time.Now().Add(idleTimeout)
		}
		return false, state.writeError
	}

	fail := func(reason string, err error) attemptResult {
		if committed {
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
				reason = "upstream_stalled"
			}
			if state.writeError != nil {
				reason = "downstream_write_error"
			}
			if state.writeError == nil {
				state.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "upstream stream failed"}})
			}
			return attemptResult{Status: status, Handled: true, Reason: reason, Err: err, Usage: state.usage}
		}
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			reason = "upstream_output_timeout"
		}
		return attemptResult{Status: status, Retryable: true, Reason: reason, Err: err}
	}

	timedBody := &anthropicBridgeTimedReader{body: body, remaining: func() time.Duration { return time.Until(deadline) }}
	scanner := bufio.NewScanner(timedBody)
	scanner.Buffer(make([]byte, 64<<10), maxAnthropicBridgeEvent)
	stop := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !committed {
			pendingBytes += len(line) + 1
			if pendingBytes > maxPendingStreamOutput {
				return fail("upstream_no_output", fmt.Errorf("upstream sent more than %d bytes without model output", maxPendingStreamOutput))
			}
		}
		if line == "" {
			done, err := finishEvent()
			if err != nil {
				return fail("upstream_invalid_response", err)
			}
			if done {
				stop = true
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return fail("upstream_stream_interrupted", scanErr)
	}
	if !stop && len(dataLines) > 0 {
		if _, err := finishEvent(); err != nil {
			return fail("upstream_invalid_response", err)
		}
	}
	if !committed {
		reason := "upstream_no_output"
		if !sawPayload {
			reason = "upstream_empty_stream"
		}
		return fail(reason, io.EOF)
	}

	state.finishStream()
	if state.writeError != nil {
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: state.writeError, Usage: state.usage}
	}
	cost(z, &state.usage)
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: state.usage}
}

func (s *anthropicBridgeStream) consume(chunk map[string]any) {
	if usage := parseOpenAIUsage(chunk); usage.Reported {
		mergeUsage(&s.usage, usage)
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if reason := anthropicStopReason(choice["finish_reason"]); reason != nil {
		s.finish = reason
	}
	delta, _ := choice["delta"].(map[string]any)
	if text, _ := delta["content"].(string); text != "" {
		if !s.textOpen {
			s.textIndex = s.nextIndex
			s.nextIndex++
			s.textOpen = true
			s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
		}
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.textIndex, "delta": map[string]any{"type": "text_delta", "text": text}})
	}
	calls, _ := delta["tool_calls"].([]any)
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		index := int(num(call["index"]))
		tool := s.tools[index]
		if tool == nil {
			tool = &openAIToolStream{Index: -1}
			s.tools[index] = tool
			s.toolOrder = append(s.toolOrder, index)
		}
		if id, _ := call["id"].(string); id != "" {
			tool.ID = id
		}
		function, _ := call["function"].(map[string]any)
		if name, _ := function["name"].(string); name != "" {
			tool.Name += name
		}
		if arguments, _ := function["arguments"].(string); arguments != "" {
			tool.Arguments.WriteString(arguments)
			if !tool.Started {
				if s.textOpen {
					s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
					s.textOpen = false
				}
				tool.Index = s.nextIndex
				s.nextIndex++
				if tool.ID == "" {
					tool.ID = fmt.Sprintf("toolu_%s_%d", s.messageID, index)
				}
				s.event("content_block_start", map[string]any{"type": "content_block_start", "index": tool.Index, "content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}}})
				tool.Started = true
			}
			if tool.Started {
				s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": tool.Index, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}})
			}
		}
	}
}

func (s *anthropicBridgeStream) finishStream() {
	if s.textOpen {
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
		s.textOpen = false
	}
	for _, toolIndex := range s.toolOrder {
		tool := s.tools[toolIndex]
		if !tool.Started {
			tool.Index = s.nextIndex
			s.nextIndex++
			if tool.ID == "" {
				tool.ID = fmt.Sprintf("toolu_%s_%d", s.messageID, toolIndex)
			}
			s.event("content_block_start", map[string]any{"type": "content_block_start", "index": tool.Index, "content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}}})
			if arguments := tool.Arguments.String(); arguments != "" {
				s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": tool.Index, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}})
			}
		}
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.Index})
	}
	if s.finish == nil {
		if len(s.toolOrder) > 0 {
			s.finish = "tool_use"
		} else {
			s.finish = "end_turn"
		}
	}
	s.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": s.finish, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": s.usage.Output}})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *anthropicBridgeStream) event(name string, payload any) {
	if s.writeError != nil {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.writeError = err
		return
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, encoded); err != nil {
		s.writeError = err
		return
	}
	s.flusher.Flush()
}

func writeAnthropicBridgeError(w http.ResponseWriter, resp *http.Response) attemptResult {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	message := strings.TrimSpace(string(body))
	errorType := "api_error"
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) == nil {
		if upstreamError, ok := decoded["error"].(map[string]any); ok {
			if value, _ := upstreamError["message"].(string); value != "" {
				message = value
			}
			if value, _ := upstreamError["type"].(string); value != "" {
				errorType = value
			}
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	if errorType == "api_error" {
		errorType = anthropicErrorType(resp.StatusCode, "upstream_error")
	}
	id := firstNonEmpty(resp.Header.Get("request-id"), w.Header().Get("request-id"))
	if id == "" {
		id = requestID()
	}
	w.Header().Set("request-id", id)
	writeJSON(w, resp.StatusCode, map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}, "request_id": id})
	reason := retryReason(resp.StatusCode, nil)
	if readErr != nil {
		reason = "upstream_invalid_response"
	}
	return attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: readErr}
}
