package fusiongate

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func passthroughFixture(t *testing.T, handlers ...http.HandlerFunc) (*App, string, func()) {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	key := insertTestKey(t, a, true)
	_, err = a.db.Exec(`UPDATE api_keys SET allow_audio=1`)
	if err != nil {
		t.Fatal(err)
	}
	servers := make([]*httptest.Server, 0, len(handlers))
	for i, handler := range handlers {
		server := httptest.NewServer(handler)
		servers = append(servers, server)
		id := insertTestProvider(t, a, fmt.Sprint("passthrough-", i), "openai_compatible", server.URL, "secret", len(handlers)-i, 1, "normalized", "any", 0, 5, 30)
		insertTestRoute(t, a, id, "native", "native", "chat,stream", 0)
	}
	return a, key, func() {
		for _, server := range servers {
			server.Close()
		}
		a.Close()
	}
}

func TestPassthroughPreservesWirePayloadAndHeaders(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	compressed := new(bytes.Buffer)
	gz := gzip.NewWriter(compressed)
	_, _ = gz.Write([]byte("opaque-body"))
	_ = gz.Close()
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = []string{r.URL.EscapedPath(), r.URL.RawQuery, string(raw), r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("Accept-Encoding")}
		mu.Unlock()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("X-Duplicate", "one")
		w.Header().Add("X-Duplicate", "two")
		w.WriteHeader(200)
		w.Write(compressed.Bytes())
	})
	defer done()
	body := []byte("{ \"model\" : \"native\", \"stream\":false,\"tool_choice\":{\"unknown\":1} }")
	req := httptest.NewRequest("POST", "/v1/chat/completions?q=1%2B2&q=two", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Cookie", "fg_admin=secret")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), compressed.Bytes()) || rec.Header().Get("Content-Encoding") != "gzip" || len(rec.Header().Values("X-Duplicate")) != 2 {
		t.Fatalf("status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.Bytes())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 6 || seen[0] != "/v1/chat/completions" || seen[1] != "q=1%2B2&q=two" || seen[2] != string(body) || seen[3] != "Bearer secret" || seen[4] != "" || seen[5] != "gzip" {
		t.Fatalf("upstream received %q", seen)
	}
}

// A channel is retried within its own budget before the request advances, so
// each failing channel is called exactly as many times as the shared channel
// budget allows. The channel that answers deliberately (422) ends the request.
func TestPassthroughFailoverAndTerminalResponse(t *testing.T) {
	// 401/403/404/429 are cheap to retry: the channel budget is 3 by default,
	// but a 401/403 isolates the credential, and 404 stops repeating the
	// endpoint, so those advance after a single call. A 404 first tries the same
	// request through the channel's other protocol, once.
	for _, tc := range []struct {
		status     int
		wantCalls  []string
		wantStatus int
		wantBody   string
	}{
		{401, []string{"A", "B"}, 422, "error B"},
		{403, []string{"A", "B"}, 422, "error B"},
		{404, []string{"A", "B"}, 422, "error B"},
		{408, []string{"A", "A", "A", "B"}, 422, "error B"},
		{425, []string{"A", "A", "A", "B"}, 422, "error B"},
		{429, []string{"A", "B"}, 422, "error B"},
		{503, []string{"A", "A", "A", "B"}, 422, "error B"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := []string{}
			a, key, done := passthroughFixture(t,
				func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, "A")
					w.Header().Set("X-Upstream", "A")
					w.WriteHeader(tc.status)
					w.Write([]byte("error A"))
				},
				func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, "B")
					w.Header().Set("X-Upstream", "B")
					w.WriteHeader(422)
					w.Write([]byte("error B"))
				},
			)
			defer done()
			rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"native"}`, "")
			if rec.Code != tc.wantStatus || rec.Body.String() != tc.wantBody || rec.Header().Get("X-Upstream") != "B" || strings.Join(calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("status=%d body=%q calls=%v want calls=%v", rec.Code, rec.Body.String(), calls, tc.wantCalls)
			}
		})
	}
}

func TestPassthroughDoesNotParseOutputs(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{200, ""}, {200, "event: unknown\ndata: ?\n\n"}, {302, "redirect"}, {400, "plain error"}, {422, "{broken"}} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) })
			defer done()
			rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}
