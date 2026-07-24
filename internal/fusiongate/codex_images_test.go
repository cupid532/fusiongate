package fusiongate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func insertCodexOAuthTestProvider(t *testing.T, a *App, baseURL string) int64 {
	t.Helper()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "test",
		AccessToken: "codex-access-secret", AccountID: "chatgpt-account-id",
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "codex image", 10, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET base_url=?,request_timeout_ms=5000 WHERE id=?`, baseURL, providerID); err != nil {
		t.Fatal(err)
	}
	return providerID
}

func TestCodexOAuthImageGenerationUsesResponsesImageTool(t *testing.T) {
	encodedImage := base64.StdEncoding.EncodeToString([]byte("valid-image-bytes"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer codex-access-secret" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "chatgpt-account-id" {
			t.Errorf("account header=%q", got)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator=%q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "codex_cli_rs/"+defaultCodexCLIVersion {
			t.Errorf("user-agent=%q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("accept=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-5.5" || body["stream"] != true || body["store"] != false {
			t.Errorf("request body=%#v", body)
		}
		if _, exists := body["quality"]; exists {
			t.Errorf("quality leaked to Codex request: %#v", body)
		}
		if _, exists := body["response_format"]; exists {
			t.Errorf("response_format leaked to Codex request: %#v", body)
		}
		inputs, _ := body["input"].([]any)
		if len(inputs) != 1 {
			t.Fatalf("input=%#v", body["input"])
		}
		input, _ := inputs[0].(map[string]any)
		if input["role"] != "user" || input["content"] != "draw a cat" {
			t.Errorf("input=%#v", input)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools=%#v", body["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["type"] != "image_generation" || tool["size"] != "1024x1024" || tool["output_format"] != "png" {
			t.Errorf("tool=%#v", tool)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"" + encodedImage + "\",\"revised_prompt\":\"a revised cat\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertCodexOAuthTestProvider(t, a, upstream.URL)
	insertTestRoute(t, a, providerID, "gpt-image-2", "gpt-5.5", "image", 10)
	key := insertTestKey(t, a, true)

	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw a cat","n":1,"size":"1024x1024","quality":"high","response_format":"b64_json","output_format":"png"}`, "browser-client/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-FusionGate-Request-ID"); got == "" {
		t.Fatal("missing request id")
	}
	var response struct {
		Data []struct {
			Base64        string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].Base64 != encodedImage || response.Data[0].RevisedPrompt != "a revised cat" {
		t.Fatalf("response=%#v", response)
	}
}

func TestCodexOAuthImageGenerationFansOutBatchConcurrently(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := calls.Add(1)
		encodedImage := base64.StdEncoding.EncodeToString([]byte("image-bytes-" + string(rune('A'+index-1))))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"" + encodedImage + "\",\"revised_prompt\":\"batch " + string(rune('0'+index)) + "\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertCodexOAuthTestProvider(t, a, upstream.URL)
	insertTestRoute(t, a, providerID, "gpt-image-2", "gpt-5.5", "image", 10)
	key := insertTestKey(t, a, true)

	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw cats","n":3,"response_format":"b64_json"}`, "test/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls=%d, want 3", calls.Load())
	}
	var response struct {
		Data []struct {
			Base64        string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 3 {
		t.Fatalf("response data=%#v", response.Data)
	}
	seen := map[string]bool{}
	for _, item := range response.Data {
		if item.Base64 == "" || seen[item.Base64] {
			t.Fatalf("duplicate or empty image payload: %#v", response.Data)
		}
		seen[item.Base64] = true
	}
}

func TestCodexOAuthImageGenerationRejectsUnsupportedShapeBeforeUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertCodexOAuthTestProvider(t, a, upstream.URL)
	insertTestRoute(t, a, providerID, "gpt-image-2", "gpt-5.5", "image", 10)
	key := insertTestKey(t, a, true)

	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw cats","n":11}`, "test/1")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 1 and 10") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d", calls)
	}

	rec = gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw cats","response_format":"url"}`, "test/1")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "b64_json") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls=%d after url rejection", calls)
	}
}

func TestParseCodexImageSSEDoesNotExposeTextFallbackAsImage(t *testing.T) {
	raw := []byte("data: {\"type\":\"response.output_text.done\",\"text\":\"<svg>not an image tool result</svg>\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	_, err := parseCodexImageSSE(raw)
	if err == nil || !strings.Contains(err.Error(), "without an image result") {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexOAuthImageGenerationFailsOverWhenPrimaryReturnsRetryableError(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "temporary", http.StatusBadGateway)
	}))
	defer primary.Close()
	encodedImage := base64.StdEncoding.EncodeToString([]byte("failover-image"))
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"" + encodedImage + "\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	defer backup.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.db.Exec(`UPDATE settings SET value=? WHERE key='routing_strategy'`, StrategyPriorityFailover); err != nil {
		t.Fatal(err)
	}
	primaryID := insertTestProvider(t, a, "input-like", "openai_compatible", primary.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	backupID := insertCodexOAuthTestProvider(t, a, backup.URL)
	if _, err := a.db.Exec(`UPDATE providers SET priority=1 WHERE id=?`, backupID); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, primaryID, "gpt-image-2", "gpt-image-2", "image", 1)
	insertTestRoute(t, a, backupID, "gpt-image-2", "gpt-5.5", "image", 1)
	key := insertTestKey(t, a, true)

	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"gpt-image-2","prompt":"draw a cat","n":1,"response_format":"b64_json"}`, "test/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
	if !strings.Contains(rec.Body.String(), encodedImage) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
