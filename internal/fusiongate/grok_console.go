package fusiongate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const consoleResponsesEndpoint = "/v1/responses"

func (a *App) consoleProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, rid, endpoint string, stream, safeTransportRetry bool, onFirstByte func()) attemptResult {
	session, err := a.getDPoPSession(r.Context(), z.Credential, ipPoolNodePtr(z.Provider))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "dpop_auth_failed", Err: err}
	}

	upstreamURL := consoleBaseURL + endpoint
	body := raw
	if z.Provider.PassthroughMode != "transparent" {
		var err error
		body, err = normalizedConsoleBody(raw, z.Route.UpstreamModel, stream)
		if err != nil {
			return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(body))
	if err != nil {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "request_build_failed", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("Origin", "https://console.x.ai")
	req.Header.Set("Referer", "https://console.x.ai/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")

	if err := setConsoleDPoPAuth(req, session); err != nil {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "dpop_proof_failed", Err: err}
	}

	resp, err := cloudflareBypassClient().Do(req)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_connect_failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		a.invalidateDPoPSession(z.Credential)
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "dpop_auth_expired"}
	}

	if resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "upstream_error", Err: fmt.Errorf("%s", string(respBody))}
	}

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return attemptResult{Status: resp.StatusCode, Reason: "upstream_error", Err: fmt.Errorf("%s", string(respBody))}
	}

	onFirstByte()

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	return attemptResult{Status: resp.StatusCode}
}

func (a *App) consoleChatProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, rid string, stream bool, onFirstByte func()) attemptResult {
	responsesBody, err := consoleChatToResponses(raw, z.Route.UpstreamModel, stream)
	if err != nil {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
	}

	session, err := a.getDPoPSession(r.Context(), z.Credential, ipPoolNodePtr(z.Provider))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "dpop_auth_failed", Err: err}
	}

	upstreamURL := consoleBaseURL + consoleResponsesEndpoint
	req, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(responsesBody))
	if err != nil {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "request_build_failed", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Origin", "https://console.x.ai")
	req.Header.Set("Referer", "https://console.x.ai/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")

	if err := setConsoleDPoPAuth(req, session); err != nil {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "dpop_proof_failed", Err: err}
	}

	resp, err := cloudflareBypassClient().Do(req)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_connect_failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		a.invalidateDPoPSession(z.Credential)
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "dpop_auth_expired"}
	}

	if resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "upstream_error", Err: fmt.Errorf("%s", string(respBody))}
	}

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return attemptResult{Status: resp.StatusCode, Reason: "upstream_error", Err: fmt.Errorf("%s", string(respBody))}
	}

	onFirstByte()

	if stream {
		return a.consoleStreamToChatStream(w, resp, z.Route.PublicName, rid)
	}
	return a.consoleResponseToChat(w, resp, z.Route.PublicName, rid)
}

func (a *App) consoleResponseToChat(w http.ResponseWriter, resp *http.Response, publicModel, rid string) attemptResult {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Reason: "upstream_read_failed", Err: err}
	}

	var responsesResp map[string]any
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Reason: "upstream_parse_failed", Err: err}
	}

	chatResp := responsesToChat(responsesResp, publicModel)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(chatResp)
	return attemptResult{Status: 200}
}

func (a *App) consoleStreamToChatStream(w http.ResponseWriter, resp *http.Response, publicModel, rid string) attemptResult {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(200)

	flusher, _ := w.(http.Flusher)

	var buf bytes.Buffer
	reader := io.Reader(resp.Body)
	scratch := make([]byte, 4096)
	for {
		n, err := reader.Read(scratch)
		if n > 0 {
			buf.Write(scratch[:n])
			for {
				line, lineErr := buf.ReadString('\n')
				if lineErr != nil {
					buf.WriteString(line)
					break
				}
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "event: ") {
					continue
				}
				if strings.HasPrefix(line, "event: ") {
					fmt.Fprintf(w, "%s\n", line)
					continue
				}
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					fmt.Fprint(w, "data: [DONE]\n\n")
					if flusher != nil {
						flusher.Flush()
					}
					continue
				}
				var event map[string]any
				if json.Unmarshal([]byte(data), &event) != nil {
					fmt.Fprintf(w, "data: %s\n\n", data)
					if flusher != nil {
						flusher.Flush()
					}
					continue
				}
				chatChunk := responsesEventToChatChunk(event, publicModel)
				if chatChunk != nil {
					encoded, _ := json.Marshal(chatChunk)
					fmt.Fprintf(w, "data: %s\n\n", encoded)
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return attemptResult{Status: 200}
}

func normalizedConsoleBody(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = upstreamModel
	if stream {
		body["stream"] = true
	}
	return json.Marshal(body)
}

func consoleChatToResponses(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	var chatBody map[string]any
	if err := json.Unmarshal(raw, &chatBody); err != nil {
		return nil, err
	}

	responsesBody := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}

	messages, _ := chatBody["messages"].([]any)
	var inputItems []any
	systemPrompt := ""
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "system" {
			systemPrompt += content + "\n"
			continue
		}
		inputItems = append(inputItems, map[string]any{
			"type":    "message",
			"role":    role,
			"content": content,
		})
	}

	if len(inputItems) > 0 {
		responsesBody["input"] = inputItems
	}
	if systemPrompt != "" {
		responsesBody["instructions"] = strings.TrimSpace(systemPrompt)
	}

	if temp, ok := chatBody["temperature"]; ok {
		responsesBody["temperature"] = temp
	}
	if topP, ok := chatBody["top_p"]; ok {
		responsesBody["top_p"] = topP
	}
	if maxTokens, ok := chatBody["max_tokens"]; ok {
		responsesBody["max_output_tokens"] = maxTokens
	}
	if maxCompletion, ok := chatBody["max_completion_tokens"]; ok {
		responsesBody["max_output_tokens"] = maxCompletion
	}

	if reasoningEffort, ok := chatBody["reasoning_effort"].(string); ok {
		responsesBody["reasoning"] = map[string]any{"effort": reasoningEffort}
	}

	return json.Marshal(responsesBody)
}

func responsesToChat(resp map[string]any, model string) map[string]any {
	content := ""
	if output, ok := resp["output"].([]any); ok {
		for _, item := range output {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "message" {
					if msgContent, ok := m["content"].([]any); ok {
						for _, c := range msgContent {
							if cm, ok := c.(map[string]any); ok {
								if cm["type"] == "output_text" {
									if text, ok := cm["text"].(string); ok {
										content += text
									}
								}
							}
						}
					}
				}
			}
		}
	}

	usage := map[string]any{
		"prompt_tokens":     num(resp["usage_prompt_tokens"]),
		"completion_tokens": num(resp["usage_completion_tokens"]),
		"total_tokens":      num(resp["usage_total_tokens"]),
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		usage["prompt_tokens"] = num(u["input_tokens"])
		usage["completion_tokens"] = num(u["output_tokens"])
		usage["total_tokens"] = num(u["input_tokens"]) + num(u["output_tokens"])
	}

	return map[string]any{
		"id":      resp["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
}

func responsesEventToChatChunk(event map[string]any, model string) map[string]any {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		return map[string]any{
			"id":      event["response_id"],
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"content": delta,
					},
				},
			},
		}
	case "response.completed":
		return map[string]any{
			"id":      event["response_id"],
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				},
			},
		}
	}
	return nil
}

func ipPoolNodePtr(p Provider) *int64 {
	if p.IPPoolNodeID != nil && *p.IPPoolNodeID > 0 {
		return p.IPPoolNodeID
	}
	return nil
}
