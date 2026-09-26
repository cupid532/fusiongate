package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixedProtocolRoutingAndProbe(t *testing.T) {
	for _, tc := range []struct {
		name, providerType, fixed, path, payload, response string
	}{
		{"chat bridge", "openai_compatible", protocolChat, "/v1/responses", `{"model":"fixed-model","input":"hello"}`, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`},
		{"responses bridge", "openai_compatible", protocolResponses, "/v1/chat/completions", `{"model":"fixed-model","messages":[{"role":"user","content":"hello"}]}`, codexCompletedSSEText("resp-fixed", "OK")},
		{"anthropic responses", "anthropic", protocolResponses, "/v1/responses", `{"model":"fixed-model","input":"hello"}`, codexCompletedSSEText("resp-anthropic", "OK")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path != map[string]string{protocolChat: "/v1/chat/completions", protocolResponses: "/v1/responses"}[tc.fixed] {
					http.NotFound(w, r)
					return
				}
				if tc.fixed == protocolChat {
					// The chat bridge consumes streamed completions.
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"id\":\"chat\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(tc.response))
				}
			}))
			defer upstream.Close()
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			id := insertTestProvider(t, a, "fixed", tc.providerType, upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
			if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference=? WHERE id=?`, tc.fixed, id); err != nil {
				t.Fatal(err)
			}
			insertTestRoute(t, a, id, "fixed-model", "upstream-model", "chat,stream", 1)
			key := insertTestKey(t, a, false)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.payload))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			a.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s paths=%v", rec.Code, rec.Body.String(), paths)
			}
			if len(paths) != 1 || paths[0] != map[string]string{protocolChat: "/v1/chat/completions", protocolResponses: "/v1/responses"}[tc.fixed] {
				t.Fatalf("paths=%v", paths)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != "fixed-model" {
				t.Fatalf("response=%v", got)
			}
			p := discoveryProvider{Type: tc.providerType, BaseURL: upstream.URL, Credential: "secret", ProtocolPolicy: protocolFixed, ProtocolPreference: tc.fixed}
			probe, err := NewHealthChecker(a, 0, 1).buildRouteProbeRequest(context.Background(), p, "upstream-model", "chat", "hi")
			if err != nil || probe.URL.Path != paths[0] {
				t.Fatalf("probe=%v err=%v", probe, err)
			}
		})
	}
}

func TestFixedResponsesDoesNotRetryAsChat(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected fallback to %s", r.URL.Path)
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "fixed-responses", "openai_compatible", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='responses' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, id, "fixed-model", "upstream-model", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fixed-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if calls != 1 {
		t.Fatalf("calls=%d status=%d", calls, rec.Code)
	}
}

func TestOpenCodeFixedDiscoveryAndHealthModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "claude-example"}, map[string]any{"id": "gpt-example"}}})
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "fixed-opencode", "opencode", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='responses' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, id, "claude", "claude-example", "chat,stream,protocol:anthropic", 2)
	insertTestRoute(t, a, id, "gpt", "gpt-example", "chat,stream,protocol:responses", 1)
	p, err := a.loadDiscoveryProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	models, err := a.fetchDiscoveredModels(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Capabilities != "unsupported" || models[1].Capabilities == "unsupported" {
		t.Fatalf("models=%+v", models)
	}
	checker := NewHealthChecker(a, 0, 1)
	if selected := checker.selectProbeModel(context.Background(), p); selected != "gpt-example" {
		t.Fatalf("selected=%s", selected)
	}
	probe, err := checker.buildProbeRequest(context.Background(), p, checker.buildProbeEndpoint(p, "gpt-example"), map[string]interface{}{"model": "gpt-example"})
	if err != nil || probe.URL.Path != "/v1/responses" {
		t.Fatalf("probe=%v err=%v", probe, err)
	}
}

func TestFixedProtocolMatrix(t *testing.T) {
	for _, tc := range []struct {
		typ, protocol string
		valid         bool
	}{
		{"openai", "chat", true}, {"openai", "responses", true}, {"openai", "messages", false},
		{"anthropic", "messages", true}, {"anthropic", "responses", true}, {"anthropic", "chat", false},
		{"opencode", "chat", true}, {"opencode", "responses", true}, {"opencode", "messages", true},
		{"gemini", "chat", false}, {"codex_oauth", "responses", false}, {"grok_oauth", "responses", false},
		{"openai", "chat,responses", false},
	} {
		if got := validProviderProtocol(tc.typ, protocolFixed, tc.protocol); got != tc.valid {
			t.Errorf("%s/%s got=%v", tc.typ, tc.protocol, got)
		}
	}
}

// A fixed text protocol does not change specialized media endpoints.
func TestFixedTextProtocolDoesNotDisableImages(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"url": "https://images.example/cat.png"}}})
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "fixed-media", "openai_compatible", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='responses' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, id, "image-model", "upstream-image", "image", 1)
	key := insertTestKey(t, a, true)
	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"image-model","prompt":"cat"}`, "test/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
