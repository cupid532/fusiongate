package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseDiscoveryModelsSupportsDynamicIdentifierShapes(t *testing.T) {
	raw := []byte(`{"data":[
		{"id":"legacy-id","name":"Legacy Name"},
		{"name":"name-id"},
		{"model":"model-string"},
		{"modelId":"camel-model-id"},
		{"model_id":"snake-model-id"},
		{"slug":"slug-id"},
		{"model":{"id":"future-object-id","name":"Future Object"}},
		{"id":"kept-when-model-id-is-an-object","model_id":{"future_field":"new-shape"}},
		{"id":"future-model-2027","model":{"future_field":"unknown"}}
	]}`)

	models, _, err := parseDiscoveryModels(raw, "openai_compatible")
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(models))
	for _, model := range models {
		got[model.ID] = true
	}
	for _, want := range []string{
		"legacy-id", "name-id", "model-string", "camel-model-id", "snake-model-id",
		"slug-id", "future-object-id", "kept-when-model-id-is-an-object", "future-model-2027",
	} {
		if !got[want] {
			t.Fatalf("parsed IDs=%v, missing %q", got, want)
		}
	}
}

func TestDiscoveryDynamicIdentifierFallbacks(t *testing.T) {
	for _, entry := range []string{
		`{"_meta":{"model":"Future-Chat"}}`,
		`{"_meta":{"modelId":"Future-Chat"}}`,
		`{"_meta":{"model_id":"Future-Chat"}}`,
		`{"model":{"model_id":"Future-Chat"}}`,
		`{"model":{"id":42,"slug":"Future-Chat"}}`,
		`{"model":{"name":"Future-Chat"}}`,
		`{"model":{"model":"Future-Chat"}}`,
		`{"model":{"modelId":"Future-Chat"}}`,
		`{"slug":"Future-Chat","model":42,"model_id":[]}`,
		`{"slug":"Future-Chat","model":[],"model_id":false}`,
		`{"slug":"Future-Chat","model":null,"model_id":null}`,
	} {
		t.Run(entry, func(t *testing.T) {
			models, _, err := parseDiscoveryModels([]byte("["+entry+"]"), "codex_oauth")
			if err != nil || len(models) != 1 || models[0].ID != "future-chat" || models[0].UpstreamID != "Future-Chat" {
				t.Fatalf("models=%#v err=%v", models, err)
			}
		})
	}
	models, _, err := parseDiscoveryModels([]byte(`[{"slug":"hidden","hidden":true},{"model_id":"hidden-meta","_meta":{"hidden":true}},{"slug":"visible","displayName":"Future Display"}]`), "codex_oauth")
	if err != nil || len(models) != 1 || models[0].DisplayName != "Future Display" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
}

func TestDiscoveryDynamicCatalogRefreshAndGrokEnrichment(t *testing.T) {
	t.Setenv("FUSIONGATE_CODEX_CLI_VERSION", " 9.99.0 ")
	for _, providerType := range []string{"codex_oauth", "grok_oauth"} {
		t.Run(providerType, func(t *testing.T) {
			catalog := `{"models":[{"slug":"Future-Chat-2099"}]}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					t.Errorf("path=%q", r.URL.Path)
				}
				if providerType == "codex_oauth" && (r.URL.Query().Get("client_version") != "9.99.0" || r.UserAgent() != "codex_cli_rs/9.99.0") {
					t.Errorf("Codex version not propagated: %s, %s", r.URL, r.UserAgent())
				}
				_, _ = w.Write([]byte(catalog))
			}))
			defer upstream.Close()
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			p := discoveryProvider{Type: providerType, BaseURL: upstream.URL, Credential: "test"}
			models, err := a.fetchDiscoveredModels(context.Background(), p)
			if err != nil || len(models) != 1 || models[0].ID != "future-chat-2099" || !matchesCapability(models[0].Capabilities, "chat") {
				t.Fatalf("models=%#v err=%v", models, err)
			}
			catalog = `{"models":[{"model":{"model_id":"Future-Chat-2100"}},{"id":"grok-4.6"}]}`
			models, err = a.fetchDiscoveredModels(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			joined := "," + strings.Join(ids, ",") + ","
			if !strings.Contains(joined, ",future-chat-2100,") || strings.Contains(joined, ",future-chat-2099,") {
				t.Fatalf("catalog not refreshed: %v", ids)
			}
			if providerType == "grok_oauth" && !strings.Contains(joined, ",grok-4.5,") {
				t.Fatalf("Grok enrichment missing: %v", ids)
			}
		})
	}
}
