package fusiongate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderCreationDiscoversCandidatesWithoutCreatingRoutes(t *testing.T) {
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
			map[string]any{"id": "Chat-Large", "display_name": "Chat Large"},
			map[string]any{"id": "IMAGE-Alpha"},
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
			Status     string            `json:"status"`
			Discovered int               `json:"discovered"`
			Skipped    int               `json:"skipped"`
			Models     []discoveredModel `json:"models"`
		} `json:"model_discovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ModelDiscovery.Status != "ok" || response.ModelDiscovery.Discovered != 2 || response.ModelDiscovery.Skipped != 1 {
		t.Fatalf("discovery response = %#v", response.ModelDiscovery)
	}
	if len(response.ModelDiscovery.Models) != 2 || response.ModelDiscovery.Models[0].ID != "chat-large" || response.ModelDiscovery.Models[1].ID != "image-alpha" {
		t.Fatalf("candidate models = %#v", response.ModelDiscovery.Models)
	}
	if calls.Load() != 1 {
		t.Fatalf("model endpoint calls = %d", calls.Load())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, response.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("route count = %d; provider creation must not auto-import", count)
	}
}

func TestImportSelectedModelsIsSelectiveAndLowercasesAllModelIDs(t *testing.T) {
	var rootCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/models":
			rootCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"models": []any{
				map[string]any{"id": "Model-B"},
				map[string]any{"id": "MODEL-A"},
				map[string]any{"id": "model-c"},
				map[string]any{"id": "model-a"},
			}})
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

	discovery, err := a.discoverProviderModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if discovery.Discovered != 3 || len(discovery.Models) != 3 {
		t.Fatalf("discovery = %#v", discovery)
	}
	first, err := a.importSelectedModels(context.Background(), providerID, []string{"MODEL-B", "model-a", "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.importSelectedModels(context.Background(), providerID, []string{"model-a", "MODEL-B"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Selected != 2 || first.Added != 2 || first.Existing != 0 || second.Added != 0 || second.Existing != 2 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if rootCalls.Load() != 3 {
		t.Fatalf("root model endpoint calls = %d", rootCalls.Load())
	}

	rows, err := a.db.Query(`SELECT public_name,upstream_model FROM model_routes WHERE provider_id=? ORDER BY public_name`, providerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var publicName, upstreamModel string
		if err := rows.Scan(&publicName, &upstreamModel); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]string{publicName, upstreamModel})
	}
	want := [][2]string{{"model-a", "model-a"}, {"model-b", "model-b"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("routes = %#v, want %#v", got, want)
	}
}

func TestImportSelectedModelsRejectsModelsNotReturnedByUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "allowed-model"}}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "safe-import", "openai_compatible", upstream.URL+"/v1", "secret", 100, 100, "normalized", "any", 0, 3, 30)

	result, err := a.importSelectedModels(context.Background(), providerID, []string{"allowed-model", "invented-model"})
	if !errors.Is(err, errSelectedModelsUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if result.Missing != 1 {
		t.Fatalf("result = %#v", result)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, providerID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("route count = %d; stale selection must be atomic", count)
	}
}

func TestImportModelsEndpointOnlyAddsCheckedModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "Alpha-Model"},
			map[string]any{"id": "beta-model"},
		}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "endpoint", "openai_compatible", upstream.URL+"/v1", "secret", 100, 100, "normalized", "any", 0, 3, 30)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers/1/import-models", strings.NewReader(`{"models":["ALPHA-MODEL"]}`))
	rec := httptest.NewRecorder()
	a.providerByID(rec, req, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result modelImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 || result.Selected != 1 {
		t.Fatalf("result = %#v", result)
	}
	var publicName, upstreamModel string
	if err := a.db.QueryRow(`SELECT public_name,upstream_model FROM model_routes WHERE provider_id=?`, providerID).Scan(&publicName, &upstreamModel); err != nil {
		t.Fatal(err)
	}
	if publicName != "alpha-model" || upstreamModel != "alpha-model" {
		t.Fatalf("public=%q upstream=%q", publicName, upstreamModel)
	}
}

func TestManualRouteLowercasesPublicAndUpstreamModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "manual", "openai_compatible", "https://example.com/v1", "secret", 100, 100, "normalized", "any", 0, 3, 30)

	body := `{"provider_id":1,"public_name":"GPT-Custom","upstream_model":"GPT-Custom","capabilities":"chat,stream"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.routes(rec, req, adminCtx{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicName, upstreamModel string
	if err := a.db.QueryRow(`SELECT public_name,upstream_model FROM model_routes WHERE provider_id=?`, providerID).Scan(&publicName, &upstreamModel); err != nil {
		t.Fatal(err)
	}
	if publicName != "gpt-custom" || upstreamModel != "gpt-custom" {
		t.Fatalf("public=%q upstream=%q", publicName, upstreamModel)
	}
}

func TestParseGeminiModelsStripsPrefixAndSkipsEmbeddingOnlyModels(t *testing.T) {
	raw := []byte(`{
	  "models": [
	    {"name":"models/Gemini-2.5-Pro","displayName":"Gemini 2.5 Pro","supportedGenerationMethods":["generateContent","countTokens"]},
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
	if models[0].ID != "gemini-2.5-pro" || models[0].UpstreamID != "Gemini-2.5-Pro" || models[0].Capabilities != "chat,stream" {
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
