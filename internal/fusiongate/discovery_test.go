package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderCreationAutomaticallyDiscoversModels(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("authorization = %q", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "chat-large"},
			map[string]any{"id": "image-alpha"},
			map[string]any{"id": "text-embedding-3-small"},
		}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	body := `{"name":"automatic","type":"openai_compatible","baseURL":"` + upstream.URL + `/v1","credential":"upstream-secret","priority":100,"weight":100,"request_timeout_ms":5000,"failure_threshold":3,"cooldown_seconds":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.providers(rec, req, adminCtx{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID             int64 `json:"id"`
		ModelDiscovery struct {
			Status     string `json:"status"`
			Discovered int    `json:"discovered"`
			Added      int    `json:"added"`
			Skipped    int    `json:"skipped"`
		} `json:"model_discovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ModelDiscovery.Status != "ok" || response.ModelDiscovery.Discovered != 3 || response.ModelDiscovery.Added != 2 || response.ModelDiscovery.Skipped != 1 {
		t.Fatalf("discovery response = %#v", response.ModelDiscovery)
	}
	if calls.Load() != 1 {
		t.Fatalf("model endpoint calls = %d", calls.Load())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, response.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("route count = %d", count)
	}
	var imageCapabilities string
	if err := a.db.QueryRow(`SELECT capabilities FROM model_routes WHERE provider_id=? AND upstream_model='image-alpha'`, response.ID).Scan(&imageCapabilities); err != nil {
		t.Fatal(err)
	}
	if imageCapabilities != "image" {
		t.Fatalf("image capabilities = %q", imageCapabilities)
	}
}

func TestDiscoverModelsIsIdempotentAndFallsBackToRootModels(t *testing.T) {
	var rootCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/models":
			rootCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"models": []string{"model-b", "model-a", "model-a"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "fallback", "openai_compatible", upstream.URL, "secret", 100, 100, "normalized", "any", 0, 3, 30)

	first, err := a.discoverAndImportModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.discoverAndImportModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Added != 2 || first.Existing != 0 || second.Added != 0 || second.Existing != 2 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if rootCalls.Load() != 2 {
		t.Fatalf("root model endpoint calls = %d", rootCalls.Load())
	}
}

func TestParseGeminiModelsStripsPrefixAndSkipsEmbeddingOnlyModels(t *testing.T) {
	raw := []byte(`{
	  "models": [
	    {"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","supportedGenerationMethods":["generateContent","countTokens"]},
	    {"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}
	  ]
	}`)
	models, _, err := parseDiscoveryModels(raw, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ID != "gemini-2.5-pro" || models[0].Capabilities != "chat,stream" {
		t.Fatalf("generative model = %#v", models[0])
	}
	if models[1].ID != "text-embedding-004" || models[1].Capabilities != "unsupported" {
		t.Fatalf("embedding model = %#v", models[1])
	}
}

func TestModelDiscoveryErrorsRedactGeminiCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := upstream.URL
	upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	secret := "secret-with/special+characters"
	_, err = a.fetchDiscoveredModels(context.Background(), discoveryProvider{Type: "gemini", BaseURL: baseURL, Credential: secret})
	if err == nil {
		t.Fatal("expected discovery error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "secret-with%2Fspecial") {
		t.Fatalf("credential leaked in error: %s", err)
	}
}
