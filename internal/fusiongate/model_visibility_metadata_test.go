package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelDiscoveryAggregatesOnlyPermittedRoutes(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	visible := insertTestProvider(t, a, "permitted", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	hidden := insertTestProvider(t, a, "restricted", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, visible, "public-model", "vendor/text", "chat", 0)
	insertTestRoute(t, a, hidden, "public-model", "vendor/image", "chat,image,reasoning:high", 0)
	key := insertProviderKeyForTest(t, a, hidden, "restricted-key", "restricted", "", providerKeyEgressInherit, nil, 1, 0)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='other-model' WHERE id=?`, key); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), authKey{AllowAll: true})
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != 200 || len(response.Data) != 1 {
		t.Fatalf("models status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	model := response.Data[0]
	if model["id"] != "public-model" || model["reasoning"] == true || len(anySlice(model["input_modalities"])) != 1 {
		t.Fatalf("unsupported route contaminated model metadata: %#v", model)
	}
}
