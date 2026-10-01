package fusiongate

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A 404 that carries a structured JSON error is the channel's API answering, so
// the route is what is missing: Cline replies to /v1/responses with
// {"error":"Not Found"} and relays answer the same way. Anything naming a model,
// a parameter or a credential stays a request-level error, and an opaque body
// stays inconclusive so the request still fails over.
func TestProtocolUnsupportedSignalAcceptsBareNotFound(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"cline bare 404", http.StatusNotFound, `{"error":"Not Found","success":false}`, true},
		{"json detail 404", http.StatusNotFound, `{"detail":"Not Found"}`, true},
		{"plain text 404", http.StatusNotFound, `404 page not found`, true},
		{"unknown endpoint", http.StatusBadRequest, `{"error":"unknown endpoint"}`, true},
		{"method not allowed", http.StatusMethodNotAllowed, ``, true},
		{"not implemented", http.StatusNotImplemented, ``, true},
		{"empty 404", http.StatusNotFound, ``, false},
		{"opaque 404 text", http.StatusNotFound, `error A`, false},
		{"non-object json 404", http.StatusNotFound, `"Not Found"`, false},
		{"missing model", http.StatusNotFound, `{"error":"model not found","success":false}`, false},
		{"missing model sentence", http.StatusNotFound, `{"error":{"message":"The model public-model does not exist"}}`, false},
		{"unsupported parameter", http.StatusNotFound, `{"error":"unsupported parameter temperature"}`, false},
		{"bad credential", http.StatusNotFound, `{"error":"invalid api key"}`, false},
		{"rate limited", http.StatusNotFound, `{"error":"rate limit exceeded"}`, false},
		{"server error keeps capabilities", http.StatusInternalServerError, `{"error":"invalid url"}`, false},
		{"unrelated status", http.StatusTooManyRequests, `{"error":"Not Found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			if got := protocolUnsupportedSignal(resp); got != tc.want {
				t.Fatalf("protocolUnsupportedSignal(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
			// Peeking for evidence must leave the client's bytes readable.
			rest, err := io.ReadAll(resp.Body)
			if err != nil || string(rest) != tc.body {
				t.Fatalf("body not preserved: %q (err=%v)", rest, err)
			}
		})
	}
}

// A channel that only serves Chat Completions, like Cline's API, answers the
// client's Responses request with a bare 404. The same channel has to be
// bridged instead of surfacing that 404 to the client.
func TestBareNotFoundBridgesToChatOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	chat := chatOnlyUpstream(t, &bodies, &mu, false)
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"Not Found","success":false}`)
			return
		}
		chat(w, r)
	})
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/responses", codexRequest)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	bridged := len(bodies)
	model := ""
	if bridged > 0 {
		model = asString(bodies[0]["model"])
	}
	mu.Unlock()
	if bridged == 0 || model != "upstream-model" {
		t.Fatalf("bridged chat calls = %d, model = %q", bridged, model)
	}
}

// A 404 that names the model is a request-level error. It must not be read as
// "this channel cannot speak Responses", so the request is not retried through
// another protocol and the client sees the upstream answer.
func TestModelNotFoundDoesNotBridge(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	chat := chatOnlyUpstream(t, &bodies, &mu, false)
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"model upstream-model not found"}}`)
			return
		}
		chat(w, r)
	})
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/responses", codexRequest)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	calls := len(bodies)
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("a model-level 404 must not be bridged, got %d chat calls", calls)
	}
}
