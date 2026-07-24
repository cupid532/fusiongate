package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayCORSPreflightDoesNotRequireAPIKey(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	req := httptest.NewRequest(http.MethodOptions, "/v1/images/generations", nil)
	req.Header.Set("Origin", "https://browser-client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,x-api-key,x-stainless-lang")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow origin=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, OPTIONS" {
		t.Fatalf("allow methods=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type,x-api-key,x-stainless-lang" {
		t.Fatalf("allow headers=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("allow private network=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Fatalf("max age=%q", got)
	}
	vary := strings.Join(rec.Header().Values("Vary"), ",")
	for _, expected := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if !strings.Contains(vary, expected) {
			t.Fatalf("Vary=%q does not contain %q", vary, expected)
		}
	}
}

func TestGatewayCORSAppliesToAuthenticationErrorsButNotAdmin(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	apiReq := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"cat"}`))
	apiReq.Header.Set("Origin", "https://browser-client.example")
	apiRec := httptest.NewRecorder()
	a.Router().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Fatalf("API status=%d body=%s", apiRec.Code, apiRec.Body.String())
	}
	if got := apiRec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("API error allow origin=%q", got)
	}

	adminReq := httptest.NewRequest(http.MethodOptions, "/api/admin/login", nil)
	adminReq.Header.Set("Origin", "https://browser-client.example")
	adminRec := httptest.NewRecorder()
	a.Router().ServeHTTP(adminRec, adminReq)
	if got := adminRec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("admin endpoint unexpectedly enabled CORS: %q", got)
	}
}

func TestImageGenerationWorksFromCrossOriginBrowserClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("upstream authorization=%q", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"url": "https://images.example/generated.png"}}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "image", "openai_compatible", upstream.URL, "upstream-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "gpt-image-test", "upstream-image", "image", 1)
	key := insertTestKey(t, a, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-test","prompt":"cat"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://browser-client.example")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("image status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("image allow origin=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "X-FusionGate-Request-ID") {
		t.Fatalf("expose headers=%q", got)
	}
	if !strings.Contains(rec.Body.String(), "generated.png") {
		t.Fatalf("image response=%s", rec.Body.String())
	}
}
