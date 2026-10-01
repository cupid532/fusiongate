package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const jsonContractBody = `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`

// messagesOnlyFixture is a channel that only serves Messages, so a Chat client
// must be bridged to reach it.
func messagesOnlyFixture(t *testing.T, bodies *[]map[string]any, mu *sync.Mutex) (*App, string, func()) {
	t.Helper()
	a, key, done := bridgeFixture(t, "anthropic_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream received invalid JSON: %v", err)
			return
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		anthropicSSE(w)
	})
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='messages' WHERE name='bridge'`); err != nil {
		t.Fatal(err)
	}
	return a, key, done
}

// A structured-output contract is refused by default, on every channel, because a
// bridge that silently returned prose to a client parsing JSON would be worse than
// an error the client can act on.
func TestContractIsRefusedWithoutTheCallersConsent(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := messagesOnlyFixture(t, &bodies, &mu)
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/chat/completions", jsonContractBody)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "response_format") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 0 {
		t.Fatalf("a refused contract must not reach the channel: %v", bodies)
	}
}

// A caller that has said it can survive the loss gets served: the request is
// bridged, and the contract is dropped rather than sent in a shape the channel
// would misread.
func TestCallerConsentedContractIsBridgedAndDropped(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := messagesOnlyFixture(t, &bodies, &mu)
	defer done()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(jsonContractBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bridgeLossHeader, "response_format")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"hello"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(bodies) != 1 {
		mu.Unlock()
		t.Fatalf("upstream calls = %d", len(bodies))
	}
	body := bodies[0]
	mu.Unlock()
	if _, ok := body["response_format"]; ok {
		t.Fatalf("the unhonourable contract must be dropped, not forwarded: %v", body)
	}
	if body["model"] != "upstream-model" || num(body["max_tokens"]) <= 0 {
		t.Fatalf("the rest of the request must be unaffected: %v", body)
	}
}

// Only relaxable fields are honoured, and an unparsable list simply means
// nothing was consented to.
func TestContractConsentNamesOnlyRelaxableFields(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"response_format", true},
		{"Response_Format", true},
		{" response_format , n ", true},
		{"n", false},
		{"response_format_v2", false},
		{"", false},
		{"*", false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if tc.header != "" {
			req.Header.Set(bridgeLossHeader, tc.header)
		}
		if got := bridgeOptionsForRequest(req).acceptsLostField("response_format"); got != tc.want {
			t.Fatalf("header %q accepted=%v, want %v", tc.header, got, tc.want)
		}
	}
	// A relaxed field still cannot smuggle in a refusal that is not relaxable.
	err := validateBridgeCapabilitiesFor(wireChat, []byte(`{"model":"m","messages":[],"n":3,"response_format":{"type":"json_object"}}`), bridgeOptionsForRequest(consentRequest("response_format")))
	if err == nil || !strings.Contains(err.Error(), "n") {
		t.Fatalf("n>1 must stay refused: %v", err)
	}
}

func consentRequest(fields string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(bridgeLossHeader, fields)
	return req
}

// The consenting caller's loss is recorded as a drop, because knowingly serving a
// degraded answer is exactly what an operator needs to see.
func TestConsentedLossIsAudited(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.auditBridgeDrops(auditRoute(), wireChat, map[string]any{"response_format": map[string]any{"type": "json_object"}}, bridgeOptionsForRequest(consentRequest("response_format")))
	events := a.bridgeFieldEvents()
	if len(events) != 1 || events[0].Field != "response_format" || events[0].Disposition != bridgeDispositionDropped {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Reason == "" {
		t.Fatalf("a consented loss must say why it was taken: %+v", events[0])
	}
}
