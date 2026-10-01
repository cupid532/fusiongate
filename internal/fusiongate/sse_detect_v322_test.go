package fusiongate

import (
	"bufio"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The ChatGPT Codex backend returns a complete SSE stream with no Content-Type
// header. The bridge must still recognise it as a stream; reading an event
// stream as one JSON document is what produced upstream_invalid_response.
func TestSSEDetectedWithoutContentType(t *testing.T) {
	const stream = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"

	cases := []struct {
		name        string
		contentType string
		body        string
		wantSSE     bool
	}{
		{"no content-type, SSE body", "", stream, true},
		{"no content-type, JSON body", "", `{"choices":[{"message":{"content":"hi"}}]}`, false},
		{"explicit SSE", "text/event-stream", stream, true},
		{"explicit JSON", "application/json", `{"choices":[]}`, false},
		{"SSE comment first", "", ": keep-alive\n\n" + stream, true},
		{"sse with charset", "text/event-stream; charset=utf-8", stream, true},
		// Go's httptest auto-detects text/plain for an SSE body; some relays
		// mislabel a stream the same way.
		{"SSE mislabelled text/plain", "text/plain", stream, true},
		{"SSE mislabelled octet-stream", "application/octet-stream", stream, true},
	}

	for _, tc := range cases {
		resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
		if tc.contentType != "" {
			resp.Header.Set("Content-Type", tc.contentType)
		}
		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		got := mediaType == "text/event-stream"
		if !got {
			buffered := bufio.NewReaderSize(resp.Body, 4096)
			resp.Body = &bufferedReadCloser{Reader: buffered, Closer: resp.Body}
			got = looksLikeSSE(buffered)
		}
		if got != tc.wantSSE {
			t.Errorf("%s: got SSE=%v want %v", tc.name, got, tc.wantSSE)
		}
		// Detection must not consume the body.
		rest, _ := io.ReadAll(resp.Body)
		if string(rest) != tc.body {
			t.Errorf("%s: body was consumed/modified: %d bytes left of %d", tc.name, len(rest), len(tc.body))
		}
	}
}

// A real bridged call must succeed against a Codex-shaped upstream that sends
// no Content-Type, which is exactly what the live backend does.
func TestBridgeSucceedsWhenUpstreamOmitsContentType(t *testing.T) {
	const stream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"

	var mu sync.Mutex
	var gotBody map[string]any
	var calls int
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		_ = json.Unmarshal(raw, &gotBody)
		mu.Unlock()
		// Deliberately no Content-Type, exactly like the Codex backend.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, stream)
	})
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/chat/completions",
		`{"model":"public-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	mu.Lock()
	c := calls
	body := gotBody
	mu.Unlock()
	if c == 0 {
		t.Fatalf("upstream was never called; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s received=%v", rec.Code, rec.Body.String(), body)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"ok"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("bridged output wrong: %s", out)
	}
	if body == nil {
		t.Fatal("upstream never received the bridged request")
	}
}
