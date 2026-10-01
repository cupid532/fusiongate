package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A codex_oauth auth file is the highest-priority channel. The client speaks
// Chat and sends ordinary sampling parameters, which the Codex backend rejects
// and the converter already drops. The route must stay usable rather than being
// excluded before any upstream call.
func TestCodexAuthFileIsUsableFromChatWithOrdinaryParams(t *testing.T) {
	var received map[string]any
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	})
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/chat/completions",
		`{"model":"public-model","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":4096,"stream":true}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received == nil {
		t.Fatal("the auth-file channel was never called")
	}
	for _, field := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens"} {
		if _, ok := received[field]; ok {
			t.Fatalf("Codex backend rejects %q but it was forwarded: %v", field, received)
		}
	}
}

// Anything that genuinely cannot be represented must still be refused before a
// call is made. A Responses tool the Chat shape cannot carry is the canonical case.
func TestBridgeStillRefusesUnrepresentableFeatures(t *testing.T) {
	if validateBridgeCapabilities(wireResponses, []byte(fidelitySensitiveCodexRequest)) == nil {
		t.Fatal("lossy conversion allowed")
	}
	raw := []byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)
	if err := validateBridgeRoute(wireResponses, wireChat, raw, "/v1/responses"); err == nil {
		t.Fatal("web_search tool was allowed to leak through the bridge")
	}
}

// A channel that was excluded before any call must not become a lasting task
// binding: the next turn of the same task has to start from the top again.
func TestPreflightSkipDoesNotPinTaskSession(t *testing.T) {
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	})
	defer done()

	// The auth file is priority 1 from the fixture; the relay sits below it and
	// serves the very same public model.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	relay := insertTestProvider(t, a, "relay", "openai_compatible", server.URL, "secret", 0, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, relay, "public-model", "public-model", "chat,stream", 0)

	send := func(taskID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-FusionGate-Task-ID", taskID)
		rec := httptest.NewRecorder()
		a.Router().ServeHTTP(rec, req)
		return rec
	}

	// Turn 1: a tool the Chat-to-Responses bridge cannot carry. The auth file is
	// excluded before any upstream call, so the relay answers instead.
	skipped := `{"model":"public-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`
	if rec := send("task-preflight", skipped); rec.Code != 200 {
		t.Fatalf("turn 1 status=%d body=%s", rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()

	var orderJSON string
	var providerID int64
	err := a.db.QueryRow(`SELECT order_json,provider_id FROM routing_sessions WHERE task_source='explicit'`).Scan(&orderJSON, &providerID)
	if err == nil {
		t.Fatalf("a pre-flight skip created a lasting task binding: order=%s provider=%d", orderJSON, providerID)
	}

	// Turn 2 of the same task, without the unrepresentable tool: the task must
	// still reach the higher-priority auth file rather than resuming past it.
	plain := `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`
	if rec := send("task-preflight", plain); rec.Code != 200 {
		t.Fatalf("turn 2 status=%d body=%s", rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()
	var served int64
	if err := a.db.QueryRow(`SELECT provider_id FROM request_ledger WHERE success=1 ORDER BY id DESC LIMIT 1`).Scan(&served); err != nil {
		t.Fatal(err)
	}
	if served == relay {
		t.Fatal("the task stayed pinned below the auth file after a pre-flight skip")
	}
}
