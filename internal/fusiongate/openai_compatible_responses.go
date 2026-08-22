package fusiongate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func compatibleResponsesBodyFromRequest(raw []byte, upstreamModel string) ([]byte, bool, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, false, err
	}
	downstreamStream, _ := source["stream"].(bool)
	messages := make([]any, 0)
	if instructions := strings.TrimSpace(asString(source["instructions"])); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	appendMessage := func(role string, content any) {
		if role == "developer" {
			role = "system"
		}
		if role != "system" && role != "assistant" && role != "tool" {
			role = "user"
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	switch input := source["input"].(type) {
	case string:
		appendMessage("user", input)
	case map[string]any:
		appendCompatibleResponseInput(&messages, input)
	case []any:
		for _, value := range input {
			if item, ok := value.(map[string]any); ok {
				appendCompatibleResponseInput(&messages, item)
			}
		}
	}
	if len(messages) == 0 {
		return nil, downstreamStream, errors.New("responses input could not be converted to chat messages")
	}
	body := map[string]any{
		"model": upstreamModel, "messages": messages, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "seed", "stop", "user", "service_tier"} {
		if value, ok := source[key]; ok {
			body[key] = value
		}
	}
	if value, ok := source["max_output_tokens"]; ok {
		body["max_completion_tokens"] = value
	}
	if reasoning := asMap(source["reasoning"]); reasoning != nil {
		if effort := strings.TrimSpace(asString(reasoning["effort"])); effort != "" {
			body["reasoning_effort"] = effort
		}
	}
	if text := asMap(source["text"]); text != nil {
		if format := asMap(text["format"]); format != nil {
			body["response_format"] = format
		}
	}
	if tools := compatibleChatTools(source["tools"]); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := compatibleChatToolChoice(source["tool_choice"]); choice != nil {
		body["tool_choice"] = choice
	}
	encoded, err := json.Marshal(body)
	return encoded, downstreamStream, err
}

func appendCompatibleResponseInput(messages *[]any, item map[string]any) {
	typeName := asString(item["type"])
	switch typeName {
	case "function_call_output":
		callID := firstNonEmpty(asString(item["call_id"]), asString(item["id"]))
		*messages = append(*messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": textContent(item["output"])})
		return
	case "function_call", "custom_tool_call":
		callID := firstNonEmpty(asString(item["call_id"]), asString(item["id"]))
		*messages = append(*messages, map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}}},
		})
		return
	case "reasoning", "item_reference":
		return
	}
	role := asString(item["role"])
	if role == "" {
		role = "user"
	}
	if role == "developer" {
		role = "system"
	}
	content := compatibleChatContent(item["content"])
	*messages = append(*messages, map[string]any{"role": role, "content": content})
}

func compatibleChatContent(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	parts := make([]any, 0)
	for _, value := range anySlice(value) {
		part, _ := value.(map[string]any)
		switch asString(part["type"]) {
		case "input_text", "output_text", "text":
			if text := asString(part["text"]); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		case "input_image", "image_url":
			imageURL := asString(part["image_url"])
			if nested := asMap(part["image_url"]); nested != nil {
				imageURL = asString(nested["url"])
			}
			if imageURL != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		}
	}
	if len(parts) == 0 {
		return textContent(value)
	}
	if len(parts) == 1 {
		if part := asMap(parts[0]); part != nil && part["type"] == "text" {
			return part["text"]
		}
	}
	return parts
}

func compatibleChatTools(value any) []any {
	out := make([]any, 0)
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		if asString(tool["type"]) != "function" {
			continue
		}
		if function := asMap(tool["function"]); function != nil {
			out = append(out, tool)
			continue
		}
		function := map[string]any{"name": tool["name"]}
		if value, ok := tool["description"]; ok {
			function["description"] = value
		}
		if value, ok := tool["parameters"]; ok {
			function["parameters"] = value
		}
		if value, ok := tool["strict"]; ok {
			function["strict"] = value
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

func compatibleChatToolChoice(value any) any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return text
	}
	choice := asMap(value)
	if choice == nil || asString(choice["type"]) != "function" {
		return value
	}
	if function := asMap(choice["function"]); function != nil {
		return choice
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
}

func compatibleResponsesFromChat(body []byte, publicModel string, stream bool) ([]byte, string, error) {
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, "", err
	}
	choices := anySlice(chat["choices"])
	if len(choices) == 0 {
		return nil, "", errors.New("chat response contained no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message := asMap(choice["message"])
	if message == nil {
		return nil, "", errors.New("chat response contained no message")
	}
	id := asString(chat["id"])
	if strings.HasPrefix(id, "chatcmpl-") {
		id = "resp-" + strings.TrimPrefix(id, "chatcmpl-")
	}
	if id == "" {
		id = "resp-" + requestID()
	}
	created := asInt64(chat["created"])
	if created == 0 {
		created = time.Now().Unix()
	}
	output := make([]any, 0)
	if reasoning := firstNonEmpty(asString(message["reasoning_content"]), asString(message["reasoning"])); reasoning != "" {
		output = append(output, map[string]any{"id": "rs_" + requestID(), "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}}})
	}
	content := textContent(message["content"])
	if content != "" {
		output = append(output, map[string]any{"id": "msg_" + requestID(), "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}})
	}
	for _, value := range anySlice(message["tool_calls"]) {
		call, _ := value.(map[string]any)
		function := asMap(call["function"])
		if function == nil {
			continue
		}
		output = append(output, map[string]any{"id": firstNonEmpty(asString(call["id"]), "fc_"+requestID()), "type": "function_call", "status": "completed", "call_id": firstNonEmpty(asString(call["id"]), "call_"+requestID()), "name": function["name"], "arguments": firstNonEmpty(asString(function["arguments"]), "{}")})
	}
	finishReason := asString(choice["finish_reason"])
	status := "completed"
	var incomplete any
	if finishReason == "length" || finishReason == "content_filter" {
		status = "incomplete"
		reason := "max_output_tokens"
		if finishReason == "content_filter" {
			reason = "content_filter"
		}
		incomplete = map[string]any{"reason": reason}
	}
	usage := asMap(chat["usage"])
	inputTokens := asInt64(usage["prompt_tokens"])
	outputTokens := asInt64(usage["completion_tokens"])
	cachedTokens := asInt64(asMap(usage["prompt_tokens_details"])["cached_tokens"])
	reasoningTokens := asInt64(asMap(usage["completion_tokens_details"])["reasoning_tokens"])
	response := map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status, "model": publicModel,
		"output": output, "parallel_tool_calls": true,
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": cachedTokens},
			"output_tokens_details": map[string]any{"reasoning_tokens": reasoningTokens},
		},
	}
	if incomplete != nil {
		response["incomplete_details"] = incomplete
	}
	if !stream {
		encoded, err := json.Marshal(response)
		return encoded, "application/json", err
	}
	encoded, err := compatibleResponsesSSE(response)
	return encoded, "text/event-stream", err
}

func compatibleResponsesSSE(response map[string]any) ([]byte, error) {
	var out bytes.Buffer
	sequence := 0
	emit := func(eventType string, payload map[string]any) error {
		payload["type"] = eventType
		payload["sequence_number"] = sequence
		sequence++
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", eventType, encoded)
		return nil
	}
	created := cloneMap(response)
	created["status"] = "in_progress"
	created["output"] = []any{}
	if err := emit("response.created", map[string]any{"response": created}); err != nil {
		return nil, err
	}
	for index, value := range anySlice(response["output"]) {
		item, _ := value.(map[string]any)
		added := cloneMap(item)
		if item["type"] == "message" {
			added["content"] = []any{}
		}
		if err := emit("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
			return nil, err
		}
		if item["type"] == "message" {
			for contentIndex, partValue := range anySlice(item["content"]) {
				part, _ := partValue.(map[string]any)
				if err := emit("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": index, "content_index": contentIndex, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
					return nil, err
				}
				text := asString(part["text"])
				if text != "" {
					if err := emit("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": index, "content_index": contentIndex, "delta": text}); err != nil {
						return nil, err
					}
				}
				if err := emit("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": contentIndex, "text": text}); err != nil {
					return nil, err
				}
				if err := emit("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": contentIndex, "part": part}); err != nil {
					return nil, err
				}
			}
		}
		if err := emit("response.output_item.done", map[string]any{"output_index": index, "item": item}); err != nil {
			return nil, err
		}
	}
	if err := emit("response.completed", map[string]any{"response": response}); err != nil {
		return nil, err
	}
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes(), nil
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func (a *App) compatibleResponsesProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, gatewayID string, stream bool, safeTransportRetry bool, onFirstByte func()) attemptResult {
	body, downstreamStream, err := compatibleResponsesBodyFromRequest(raw, z.Route.UpstreamModel)
	if err != nil {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
	}
	if stream != downstreamStream {
		stream = downstreamStream
	}
	return a.proxyUpstream(w, r, z, proxyOptions{
		Endpoint: "/v1/chat/completions", RawBody: body, Stream: false, UsageFormat: "openai", GatewayID: gatewayID,
		SafeTransportRetry: safeTransportRetry, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferSSE: true,
		SSETransform: func(body []byte) ([]byte, string, Usage, error) {
			chat, _, usage, err := completedChatCompletionFromSSE(body)
			if err != nil {
				return nil, "", usage, err
			}
			completed, contentType, err := compatibleResponsesFromChat(chat, z.Route.PublicName, stream)
			return completed, contentType, usage, err
		},
		JSONTransform: func(body []byte) ([]byte, string, error) {
			return compatibleResponsesFromChat(body, z.Route.PublicName, stream)
		},
	})
}

func responsesProtocolFallbackStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestTimeout, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return true
	}
	return status >= 500
}

func shouldFallbackResponsesToChat(result attemptResult) bool {
	if result.Handled || !result.Retryable {
		return false
	}
	switch result.Reason {
	case "upstream_auth_error", "upstream_rate_limited", "downstream_canceled", "downstream_write_error":
		return false
	default:
		return true
	}
}

// responsesFirstCompatibleProxy preserves the downstream Responses contract while
// preferring the upstream Responses endpoint. If that endpoint fails before any
// response bytes are committed, the same route is retried through Chat Completions
// and bridged back into a Responses response. Authentication and rate-limit failures
// skip the protocol retry because changing endpoint cannot repair them.
func (a *App) responsesFirstCompatibleProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, gatewayID string, stream bool, safeTransportRetry bool, onFirstByte func()) attemptResult {
	w.Header().Set("X-FusionGate-Upstream-Protocol", "responses")
	result := a.openAIProxyWithRetryStatus(w, r, raw, z, gatewayID, "/v1/responses", stream, safeTransportRetry, onFirstByte, responsesProtocolFallbackStatus)
	if !shouldFallbackResponsesToChat(result) {
		return result
	}
	w.Header().Set("X-FusionGate-Upstream-Protocol", "chat")
	return a.compatibleResponsesProxy(w, r, raw, z, gatewayID, stream, safeTransportRetry, onFirstByte)
}
