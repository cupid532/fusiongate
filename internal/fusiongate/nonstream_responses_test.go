package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A codex_oauth channel must serve a NON-streaming /v1/responses request even
// though the ChatGPT Codex backend only accepts stream=true and store=false.
// The gateway normalises the request and buffers the forced SSE back into the
// single JSON document the client asked for.
func TestCodexNonStreamingResponsesNormalised(t *testing.T) {
	var mu sync.Mutex
	var upstreamBody map[string]any
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &upstreamBody)
		mu.Unlock()
		// The backend's own stream, as the real Codex API produces it.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"+
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	})
	defer done()

	// Non-streaming: no "stream" field at all, no "store" field.
	rec := bridgeRequest(t, a, key, "/v1/responses",
		`{"model":"public-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("non-streaming answer should be JSON, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":"completed"`) || !strings.Contains(rec.Body.String(), `"model":"public-model"`) {
		t.Fatalf("answer is not a completed Responses JSON document: %s", rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if upstreamBody == nil {
		t.Fatal("upstream was never called")
	}
	if upstreamBody["store"] != false {
		t.Fatalf("upstream received store=%v, want false", upstreamBody["store"])
	}
	if upstreamBody["stream"] != true {
		t.Fatalf("upstream received stream=%v, want true", upstreamBody["stream"])
	}
}
