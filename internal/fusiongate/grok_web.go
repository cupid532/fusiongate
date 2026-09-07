package fusiongate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	grokWebBaseURL  = "https://grok.com"
	grokWebGateway  = "wss://grok.com/ws/mgw/"
	webPingInterval = 25 * time.Second
	webReadTimeout  = 120 * time.Second
)

var grokWebModels = map[string]grokWebModelSpec{
	"grok-chat-fast":          {upstreamModel: "grok-3-fast", tier: "basic"},
	"grok-chat-auto":          {upstreamModel: "grok-3", tier: "super"},
	"grok-chat-expert":        {upstreamModel: "grok-3", tier: "super"},
	"grok-chat-heavy":         {upstreamModel: "grok-3-heavy", tier: "heavy"},
	"grok-imagine-image-lite": {upstreamModel: "flux-schnell", tier: "basic", media: true},
	"grok-imagine-image":      {upstreamModel: "flux-1.1-pro", tier: "basic", media: true},
	"grok-imagine-image-2.0":  {upstreamModel: "aurora", tier: "basic", media: true},
	"grok-imagine-image-edit": {upstreamModel: "aurora-edit", tier: "basic", media: true},
	"grok-imagine-video":      {upstreamModel: "grok-video", tier: "basic", media: true},
}

type grokWebModelSpec struct {
	upstreamModel string
	tier          string
	media         bool
}

func (a *App) webChatProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, rid string, stream bool, onFirstByte func()) attemptResult {
	var chatBody map[string]any
	if err := json.Unmarshal(raw, &chatBody); err != nil {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
	}

	ssoToken := z.Credential
	userID := extractWebUserID(z)

	prompt, systemPrompt := extractChatMessages(chatBody)

	spec, ok := grokWebModels[z.Route.UpstreamModel]
	if !ok {
		spec = grokWebModelSpec{upstreamModel: z.Route.UpstreamModel, tier: "basic"}
	}

	wsURL := grokWebGateway + "?uid=" + userID
	headers := buildWebHeaders(ssoToken, userID)

	ctx, cancel := context.WithTimeout(r.Context(), webReadTimeout)
	defer cancel()

	ws, _, err := wsDialContext(ctx, wsURL, headers)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "websocket_connect_failed", Err: err}
	}
	defer ws.Close()

	conversationID := generateID()
	sessionCreateEvent := map[string]any{
		"type":      "session.create",
		"sessionId": conversationID,
		"config": map[string]any{
			"model":         spec.upstreamModel,
			"instructions":  systemPrompt,
			"disableSearch": false,
		},
	}
	if err := ws.WriteJSON(sessionCreateEvent); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "websocket_write_failed", Err: err}
	}

	messageEvent := map[string]any{
		"type":      "conversation.item.create",
		"sessionId": conversationID,
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": prompt},
			},
		},
	}
	if err := ws.WriteJSON(messageEvent); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "websocket_write_failed", Err: err}
	}

	responseCreateEvent := map[string]any{
		"type":      "response.create",
		"sessionId": conversationID,
	}
	if err := ws.WriteJSON(responseCreateEvent); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "websocket_write_failed", Err: err}
	}

	onFirstByte()

	if stream {
		return a.webStreamResponse(w, ws, z.Route.PublicName, rid, cancel)
	}
	return a.webBufferedResponse(w, ws, z.Route.PublicName, rid, cancel)
}

func (a *App) webStreamResponse(w http.ResponseWriter, ws *wsConn, publicModel, rid string, cancel context.CancelFunc) attemptResult {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	responseID := "chatcmpl-" + generateShortID()
	created := time.Now().Unix()

	pingTicker := time.NewTicker(webPingInterval)
	defer pingTicker.Stop()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-pingTicker.C:
				ws.WriteMessage(wsOpcodeText, []byte(`{"type":"ping"}`))
			case <-done:
				return
			}
		}
	}()

	defer close(done)

	for {
		ws.SetReadDeadline(time.Now().Add(webReadTimeout))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		events := parseWebEvents(msg)
		for _, event := range events {
			eventType, _ := event["type"].(string)
			switch eventType {
			case "response.output_text.delta":
				delta, _ := event["delta"].(string)
				if delta == "" {
					continue
				}
				chunk := map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": created, "model": publicModel,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}},
				}
				encoded, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", encoded)
				if flusher != nil {
					flusher.Flush()
				}

			case "response.completed", "response.done":
				chunk := map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": created, "model": publicModel,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				}
				encoded, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", encoded)
				fmt.Fprint(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				return attemptResult{Status: 200}
			}
		}
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return attemptResult{Status: 200}
}

func (a *App) webBufferedResponse(w http.ResponseWriter, ws *wsConn, publicModel, rid string, cancel context.CancelFunc) attemptResult {
	var content strings.Builder
	responseID := "chatcmpl-" + generateShortID()

	pingTicker := time.NewTicker(webPingInterval)
	defer pingTicker.Stop()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-pingTicker.C:
				ws.WriteMessage(wsOpcodeText, []byte(`{"type":"ping"}`))
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	for {
		ws.SetReadDeadline(time.Now().Add(webReadTimeout))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		events := parseWebEvents(msg)
		for _, event := range events {
			eventType, _ := event["type"].(string)
			switch eventType {
			case "response.output_text.delta":
				delta, _ := event["delta"].(string)
				content.WriteString(delta)
			case "response.completed", "response.done":
				result := map[string]any{
					"id": responseID, "object": "chat.completion", "created": time.Now().Unix(), "model": publicModel,
					"choices": []any{
						map[string]any{
							"index":         0,
							"message":       map[string]any{"role": "assistant", "content": content.String()},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-FusionGate-Route-ID", rid)
				w.WriteHeader(200)
				json.NewEncoder(w).Encode(result)
				return attemptResult{Status: 200}
			}
		}
	}

	result := map[string]any{
		"id": responseID, "object": "chat.completion", "created": time.Now().Unix(), "model": publicModel,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content.String()},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(result)
	return attemptResult{Status: 200}
}

func parseWebEvents(msg []byte) []map[string]any {
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(msg))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			break
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		var single map[string]any
		if json.Unmarshal(msg, &single) == nil {
			events = append(events, single)
		}
	}
	return events
}

func extractChatMessages(body map[string]any) (prompt, systemPrompt string) {
	messages, _ := body["messages"].([]any)
	var userParts []string
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		content := extractMessageContent(m)
		switch role {
		case "system":
			systemPrompt += content + "\n"
		case "user":
			userParts = append(userParts, content)
		case "assistant":
			userParts = append(userParts, "[assistant]: "+content)
		}
	}
	prompt = strings.Join(userParts, "\n")
	systemPrompt = strings.TrimSpace(systemPrompt)
	return
}

func extractMessageContent(m map[string]any) string {
	if s, ok := m["content"].(string); ok {
		return s
	}
	if parts, ok := m["content"].([]any); ok {
		var texts []string
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if pm["type"] == "text" {
					if t, ok := pm["text"].(string); ok {
						texts = append(texts, t)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func extractWebUserID(z resolvedRoute) string {
	if z.AuthCredential != nil && z.AuthCredential.AccountID != "" {
		return z.AuthCredential.AccountID
	}
	return ""
}

func buildWebHeaders(ssoToken, userID string) http.Header {
	headers := http.Header{}
	cookie := "sso=" + ssoToken + "; sso-rw=" + ssoToken
	if userID != "" {
		cookie += "; x-userid=" + userID
	}
	headers.Set("Cookie", cookie)
	headers.Set("Origin", grokWebBaseURL)
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
	return headers
}

func buildWebHTTPHeaders(ssoToken string) http.Header {
	headers := http.Header{}
	headers.Set("Cookie", "sso="+ssoToken+"; sso-rw="+ssoToken)
	headers.Set("Origin", grokWebBaseURL)
	headers.Set("Referer", grokWebBaseURL+"/")
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
	return headers
}

func (a *App) webImageProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, rid string, onFirstByte func()) attemptResult {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
	}

	prompt, _ := body["prompt"].(string)
	if prompt == "" {
		return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: fmt.Errorf("prompt is required")}
	}

	ssoToken := z.Credential
	headers := buildWebHTTPHeaders(ssoToken)
	headers.Set("x-xai-request-id", generateID())

	spec, ok := grokWebModels[z.Route.UpstreamModel]
	if !ok {
		spec = grokWebModelSpec{upstreamModel: z.Route.UpstreamModel, tier: "basic"}
	}

	imageReq := map[string]any{
		"modelName": spec.upstreamModel,
		"prompt":    prompt,
	}
	if n, ok := body["n"]; ok {
		imageReq["n"] = n
	}
	if size, ok := body["size"].(string); ok {
		parts := strings.SplitN(size, "x", 2)
		if len(parts) == 2 {
			imageReq["width"] = parts[0]
			imageReq["height"] = parts[1]
		}
	}

	reqBody, _ := json.Marshal(imageReq)
	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", grokWebBaseURL+"/rest/app-chat/images/generate", bytes.NewReader(reqBody))
	if err != nil {
		return attemptResult{Status: http.StatusInternalServerError, Reason: "request_build_failed", Err: err}
	}
	for k, vals := range headers {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := a.doProviderRequest(httpReq, ipPoolNodePtr(z.Provider))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_connect_failed", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "sso_auth_expired"}
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: "upstream_error", Err: fmt.Errorf("%s", string(respBody))}
	}

	onFirstByte()

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-FusionGate-Route-ID", rid)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return attemptResult{Status: resp.StatusCode}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateShortID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
