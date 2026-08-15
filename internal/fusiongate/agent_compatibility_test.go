package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenCodeProviderSupportsResponsesTools(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer opencode-secret" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call-1\",\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-open\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"upstream-open\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "opencode", "opencode", upstream.URL+"/v1", "opencode-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "open-model", "gpt-5.6-sol", "chat,stream,tools,protocol:responses", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"open-model","input":"inspect","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"stream":false}`, "opencode/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received["model"] != "gpt-5.6-sol" || len(anySlice(received["tools"])) != 1 {
		t.Fatalf("upstream request=%#v", received)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if asString(response["model"]) != "open-model" || asString(asMap(anySlice(response["output"])[0])["type"]) != "function_call" {
		t.Fatalf("response=%#v", response)
	}
}

func TestOpenCodeProviderUsesNativeMessagesForClaudeModels(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer opencode-secret" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers=%v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "msg_open", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 2, "output_tokens": 1}})
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "opencode-claude", "opencode", upstream.URL+"/v1", "opencode-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-agent", "claude-sonnet-4-6", "chat,stream,tools,protocol:anthropic", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages", key, `{"model":"claude-agent","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, "claude-cli/2.1.231")
	if rec.Code != http.StatusOK || received["model"] != "claude-sonnet-4-6" || !strings.Contains(rec.Body.String(), `"model":"claude-agent"`) {
		t.Fatalf("status=%d request=%#v body=%s", rec.Code, received, rec.Body.String())
	}
}

func TestOpenCodeProviderUsesChatCompletionsForOpenModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "chat_open", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "opencode-chat", "opencode", upstream.URL+"/v1", "opencode-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "glm-agent", "glm-5.2", "chat,stream,tools,protocol:chat", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"glm-agent","messages":[{"role":"user","content":"hello"}]}`, "opencode/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCodexResponsesCompactForwardsUnaryPayloadAndTurnState(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("x-codex-turn-state", "sticky-state")
		writeJSON(w, http.StatusOK, map[string]any{"output": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "summary"}}}}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "codex-compatible", "openai_compatible", upstream.URL, "secret", 1, 100, "normalized", "codex", 0, 3, 30)
	insertTestRoute(t, a, providerID, "gpt-agent", "gpt-upstream", "chat,stream,tools", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/responses/compact", key, `{"model":"gpt-agent","input":[{"role":"user","content":[]}],"instructions":"compact","parallel_tool_calls":true}`, "codex-cli/0.147.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received["model"] != "gpt-upstream" || rec.Header().Get("x-codex-turn-state") != "sticky-state" {
		t.Fatalf("request=%#v turn-state=%q", received, rec.Header().Get("x-codex-turn-state"))
	}
	if !strings.Contains(rec.Body.String(), `"output"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCodexOAuthResponsesCompactUsesBackendPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			t.Errorf("path=%q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"output": []any{map[string]any{"type": "message", "role": "user", "content": []any{}}}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", AccessToken: "access", AccountID: "account"}
	payload, _ := json.Marshal(credential)
	sealed, _ := a.encrypt(string(payload))
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,auth_kind,auth_source,enabled,priority,weight,status,passthrough_mode,client_policy,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "codex-oauth", "codex_oauth", upstream.URL, sealed, "oauth", "fusiongate_oauth", 1, 1, 100, "unknown", "normalized", "codex", 5000, 3, 30, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := res.LastInsertId()
	insertTestRoute(t, a, providerID, "gpt-agent", "gpt-upstream", "chat,stream,tools", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/responses/compact", key, `{"model":"gpt-agent","input":[{"role":"user","content":[]}]}`, "codex-cli/0.147.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicTokenCountAndErrorsUseClaudeProtocol(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "claude-compatible", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "claude_code", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-agent", "upstream-claude", "chat,stream,tools", 1)
	if _, err := a.db.Exec(`UPDATE providers SET status='degraded',consecutive_failures=2,last_error='offline' WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages/count_tokens", key, `{"model":"claude-agent","messages":[{"role":"user","content":"hello"}]}`, "claude-cli/2.1.231")
	if rec.Code != http.StatusOK || rec.Header().Get("request-id") == "" {
		t.Fatalf("status=%d request-id=%q body=%s", rec.Code, rec.Header().Get("request-id"), rec.Body.String())
	}
	var count map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil || num(count["input_tokens"]) < 1 {
		t.Fatalf("count=%#v err=%v", count, err)
	}
	var status, lastError string
	var failures int
	if err := a.db.QueryRow(`SELECT status,consecutive_failures,last_error FROM providers WHERE id=?`, providerID).Scan(&status, &failures, &lastError); err != nil || status != "degraded" || failures != 2 || lastError != "offline" {
		t.Fatalf("provider health changed status=%q failures=%d last_error=%q err=%v", status, failures, lastError, err)
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	bad.Header.Set("Authorization", "Bearer "+key)
	badRec := httptest.NewRecorder()
	a.Router().ServeHTTP(badRec, bad)
	var response map[string]any
	if err := json.Unmarshal(badRec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if badRec.Code != http.StatusBadRequest || response["type"] != "error" || response["request_id"] == "" || asMap(response["error"])["type"] != "invalid_request_error" {
		t.Fatalf("status=%d response=%#v headers=%v", badRec.Code, response, badRec.Header())
	}
}

func TestAnthropicTokenCountUsesNativeEndpoint(t *testing.T) {
	var model string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "anthropic-secret" {
			t.Errorf("x-api-key=%q", r.Header.Get("x-api-key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		model, _ = body["model"].(string)
		w.Header().Set("request-id", "req_upstream")
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": 37})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "anthropic-native", "anthropic", upstream.URL, "anthropic-secret", 1, 100, "normalized", "claude_code", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-agent", "claude-upstream", "chat,stream,tools", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages/count_tokens", key, `{"model":"claude-agent","messages":[{"role":"user","content":"hello"}]}`, "claude-cli/2.1.231")
	if rec.Code != http.StatusOK || model != "claude-upstream" || rec.Header().Get("request-id") != "req_upstream" {
		t.Fatalf("status=%d model=%q request-id=%q body=%s", rec.Code, model, rec.Header().Get("request-id"), rec.Body.String())
	}
}

func TestAnthropicNativeClientErrorUsesClaudeEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad tools"}}`)
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "anthropic-error", "anthropic", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-agent", "claude-upstream", "chat", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages", key, `{"model":"claude-agent","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "claude-cli/2.1.231")
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || response["type"] != "error" || response["request_id"] == "" || asMap(response["error"])["message"] != "bad tools" {
		t.Fatalf("status=%d response=%#v", rec.Code, response)
	}
}
