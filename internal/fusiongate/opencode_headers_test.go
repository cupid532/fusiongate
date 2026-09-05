package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func opencodeRoute() resolvedRoute {
	return resolvedRoute{Provider: Provider{ID: 42, Type: "opencode"}}
}

func incomingWith(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer fg-tenant-key")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestOpenCodeSessionPassesClientHeaderThrough(t *testing.T) {
	incoming := incomingWith(map[string]string{"x-opencode-session": "client-owned"})
	req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	applyOpenCodeRequestHeaders(req, opencodeRoute(), incoming, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if got := req.Header.Get("x-opencode-session"); got != "client-owned" {
		t.Fatalf("session=%q, want the client's own value untouched", got)
	}
}

func TestOpenCodeSessionReusesOtherVendorsSessionHeaders(t *testing.T) {
	for _, name := range []string{"session_id", "x-session-id", "conversation_id"} {
		incoming := incomingWith(map[string]string{name: "sess-" + name})
		req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/responses", nil)
		applyOpenCodeRequestHeaders(req, opencodeRoute(), incoming, []byte(`{}`))
		if got := req.Header.Get("x-opencode-session"); got != "sess-"+name {
			t.Fatalf("%s: session=%q", name, got)
		}
	}
}

func TestOpenCodeSessionFromBodyIdentifiers(t *testing.T) {
	cases := map[string]string{
		`{"prompt_cache_key":"cache-key-1","messages":[]}`:                                                                     "cache-key-1",
		`{"metadata":{"session_id":"meta-sess"},"messages":[]}`:                                                                "meta-sess",
		`{"metadata":{"user_id":"user_abc_account_11111111-2222_session_9f9f9f9f-aaaa-bbbb-cccc-dddddddddddd"},"messages":[]}`: "claude-9f9f9f9f-aaaa-bbbb-cccc-dddddddddddd",
	}
	for body, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/messages", nil)
		applyOpenCodeRequestHeaders(req, opencodeRoute(), incomingWith(nil), []byte(body))
		if got := req.Header.Get("x-opencode-session"); got != want {
			t.Fatalf("body %s: session=%q want %q", body, got, want)
		}
	}
}

func TestOpenCodeSessionFingerprintIsStableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"Plan the refactor"}]}`)
	turn2 := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"Plan the refactor"},{"role":"assistant","content":"1. …"},{"role":"user","content":"Now do step 1"}]}`)
	id := func(body []byte, auth string) string {
		incoming := incomingWith(nil)
		incoming.Header.Set("Authorization", auth)
		req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
		applyOpenCodeRequestHeaders(req, opencodeRoute(), incoming, body)
		return req.Header.Get("x-opencode-session")
	}
	first, second := id(turn1, "Bearer k1"), id(turn2, "Bearer k1")
	if first == "" || !strings.HasPrefix(first, "fg-") || len(first) != len("fg-")+32 {
		t.Fatalf("unexpected fingerprint %q", first)
	}
	if first != second {
		t.Fatalf("same conversation produced different sessions: %q vs %q", first, second)
	}
	if other := id(turn1, "Bearer k2"); other == first {
		t.Fatal("different gateway credentials must not share an upstream session")
	}
	different := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"Something else entirely"}]}`)
	if id(different, "Bearer k1") == first {
		t.Fatal("different conversations must not share a session")
	}
}

func TestOpenCodeSessionFingerprintCoversEveryRequestShape(t *testing.T) {
	// The same conversation expressed as Anthropic and as Responses must each
	// be stable on their own; they only need to be deterministic, not equal.
	anthropic := []byte(`{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`)
	responses := []byte(`{"instructions":"sys","input":[{"role":"user","content":[{"type":"input_text","text":"first"}]}]}`)
	for _, body := range [][]byte{anthropic, responses} {
		system, user := conversationFingerprint(mustJSONMap(t, body))
		if system != "sys" || user != "first" {
			t.Fatalf("%s → system=%q user=%q", body, system, user)
		}
	}
	if s, u := conversationFingerprint(mustJSONMap(t, []byte(`{"input":"bare prompt"}`))); s != "" || u != "bare prompt" {
		t.Fatalf("bare input → %q/%q", s, u)
	}
	// No conversation at all still yields an ID, anchored on the body.
	req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/embeddings", nil)
	applyOpenCodeRequestHeaders(req, opencodeRoute(), incomingWith(nil), []byte(`{"input":["a","b"]}`))
	if req.Header.Get("x-opencode-session") == "" {
		t.Fatal("embeddings request left without a session")
	}
}

func TestOpenCodeHeadersFillBlankUserAgentOnly(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "") // what the proxy does when the client sent none
	applyOpenCodeRequestHeaders(req, opencodeRoute(), incomingWith(nil), []byte(`{}`))
	if got := req.Header.Get("User-Agent"); got != "FusionGate/"+Version {
		t.Fatalf("User-Agent=%q", got)
	}
	req = httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "openai-node/4.0")
	applyOpenCodeRequestHeaders(req, opencodeRoute(), incomingWith(nil), []byte(`{}`))
	if got := req.Header.Get("User-Agent"); got != "openai-node/4.0" {
		t.Fatalf("real client User-Agent overwritten: %q", got)
	}
}

func TestOpenCodeHeadersLeaveOtherProvidersAlone(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://relay.test/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "")
	applyOpenCodeRequestHeaders(req, resolvedRoute{Provider: Provider{Type: "openai_compatible"}}, incomingWith(nil), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if req.Header.Get("x-opencode-session") != "" || req.Header.Get("User-Agent") != "" {
		t.Fatalf("non-OpenCode request was modified: %v", req.Header)
	}
}

func TestOpenCodeProbeHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
	setDiscoveryAuth(req, discoveryProvider{ID: 7, Type: "opencode", Credential: "tok"})
	if got := req.Header.Get("x-opencode-session"); got != "fusiongate-probe-7" {
		t.Fatalf("probe session=%q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "FusionGate/"+Version {
		t.Fatalf("probe User-Agent=%q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("auth lost: %q", got)
	}
	// setProviderAuth (the proxy path) gets the UA fallback too.
	req = httptest.NewRequest(http.MethodPost, "https://opencode.test/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "")
	if err := setProviderAuth(req, resolvedRoute{Provider: Provider{Type: "opencode"}, Credential: "tok"}); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "FusionGate/"+Version {
		t.Fatalf("proxy User-Agent=%q", got)
	}
}

func mustJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid JSON fixture %s: %v", raw, err)
	}
	return out
}

// End to end through the gateway: two turns of one conversation reach an
// OpenCode upstream with the same derived session and the client's own UA.
func TestOpenCodeGatewayRequestsCarrySessionHeader(t *testing.T) {
	var sessions, agents []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		agents = append(agents, r.Header.Get("User-Agent"))
		writeJSON(w, http.StatusOK, map[string]any{"id": "chat", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "opencode-session", "opencode", upstream.URL+"/v1", "opencode-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "glm-agent", "glm-5.2", "chat,stream,tools,protocol:chat", 1)
	key := insertTestKey(t, a, false)
	turn1 := `{"model":"glm-agent","messages":[{"role":"system","content":"terse"},{"role":"user","content":"plan it"}]}`
	turn2 := `{"model":"glm-agent","messages":[{"role":"system","content":"terse"},{"role":"user","content":"plan it"},{"role":"assistant","content":"1."},{"role":"user","content":"go"}]}`
	for _, body := range []string{turn1, turn2} {
		if rec := gatewayRequest(t, a, "/v1/chat/completions", key, body, "opencode/1"); rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if len(sessions) != 2 || sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("upstream saw sessions %q, want one stable non-empty id", sessions)
	}
	if agents[0] != "opencode/1" {
		t.Fatalf("client User-Agent not preserved: %q", agents)
	}
}
