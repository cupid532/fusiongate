package fusiongate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImagePolicyExplicitOuterModelHonorsDisable(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertCodexOAuthTestProvider(t, a, "https://example.invalid")
	insertTestRoute(t, a, id, "chosen-text", "chosen-text", "chat,stream", 10)
	if _, err := a.db.Exec(`UPDATE providers SET image_mode='responses',image_outer_model='chosen-text' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	z := resolvedRoute{Provider: Provider{ID: id}, Route: Route{UpstreamModel: "old-model"}}
	got, err := a.imagePolicy(z)
	if err != nil || got.Route.UpstreamModel != "chosen-text" {
		t.Fatalf("got=%v err=%v", got.Route.UpstreamModel, err)
	}
	a.db.Exec(`UPDATE model_routes SET enabled=0 WHERE provider_id=? AND public_name='chosen-text'`, id)
	if _, err := a.imagePolicy(z); err == nil {
		t.Fatal("disabled outer model accepted")
	}
	a.db.Exec(`UPDATE providers SET image_mode='disabled' WHERE id=?`, id)
	if _, err := a.imagePolicy(z); err == nil {
		t.Fatal("disabled images accepted")
	}
}

func TestImagePolicyPublicExplicitOuterAndNoCrossProviderLeak(t *testing.T) {
	var toolCalls, nativeCalls int
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls++
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"model":"chosen-text"`) {
			t.Errorf("wrong outer model: %s", raw)
		}
		http.Error(w, "temporary", 502)
	}))
	defer tool.Close()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls++
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"model":"gpt-image-2"`) {
			t.Errorf("cross-provider model leak: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer native.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.db.Exec(`UPDATE settings SET value=? WHERE key='routing_strategy'`, StrategyPriorityFailover)
	toolID := insertCodexOAuthTestProvider(t, a, tool.URL)
	nativeID := insertTestProvider(t, a, "native-image", "openai_compatible", native.URL, "test", 1, 1, "normalized", "any", 0, 3, 30)
	a.db.Exec(`UPDATE providers SET priority=2,image_mode='responses',image_outer_model='chosen-text' WHERE id=?`, toolID)
	insertTestRoute(t, a, toolID, "gpt-image-2", "old-model", "image", 1)
	insertTestRoute(t, a, toolID, "chosen-text", "chosen-text", "chat,stream", 1)
	insertTestRoute(t, a, nativeID, "gpt-image-2", "gpt-image-2", "image", 1)
	key := insertTestKey(t, a, true)
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 || toolCalls < 1 || nativeCalls != 1 {
		t.Fatalf("status=%d calls=%d/%d body=%s", rec.Code, toolCalls, nativeCalls, rec.Body.String())
	}
	// A disabled outer model must skip the channel before any upstream call.
	before := toolCalls
	a.db.Exec(`UPDATE model_routes SET enabled=0 WHERE provider_id=? AND public_name='chosen-text'`, toolID)
	req = httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 || toolCalls != before {
		t.Fatalf("disabled outer model called upstream: status=%d calls=%d before=%d", rec.Code, toolCalls, before)
	}
}

func TestImagePolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		kind, mode, outer string
		valid             bool
	}{
		{"codex_oauth", "responses", "chosen-text", true},
		{"codex_oauth", "responses", "", false},
		{"codex_oauth", "native", "", false},
		{"openai_compatible", "native", "", true},
		{"openai_compatible", "responses", "chosen-text", false},
		{"codex_oauth", "disabled", "", true},
		{"codex_oauth", "unknown", "", false},
	} {
		if err := validateImagePolicy(tc.kind, tc.mode, tc.outer); (err == nil) != tc.valid {
			t.Errorf("%+v err=%v", tc, err)
		}
	}
	if _, err := codexImageRequest([]byte(`{"prompt":"cat"}`), ""); err == nil {
		t.Fatal("guessed outer model")
	}
}
