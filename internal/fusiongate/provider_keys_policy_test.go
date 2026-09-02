package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderKeySupportsModelPolicies(t *testing.T) {
	inventory := map[string]bool{"gpt-a": true, "gpt-disabled": false}
	exclusions := map[string]bool{}
	cases := []struct {
		name, policy, allowlist, requested string
		want                               bool
	}{
		{"fallback inventory", "fallback", "", "gpt-a", true},
		{"fallback exclusion", "fallback", "", "gpt-excluded", false},
		{"allowlist match", "allowlist", "GPT-A, gpt-b", "gpt-b", true},
		{"allowlist unknown", "allowlist", "", "gpt-a", false},
		{"allowlist empty strict", "allowlist", " , ", "gpt-a", false},
		{"fallback disabled inventory", "fallback", "", "gpt-disabled", false},
	}
	if providerKeySupportsModel("fallback", "", "", "", "gpt-a", inventory, map[string]bool{"gpt-a": true}) {
		t.Fatal("excluded model unexpectedly matched")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerKeySupportsModel(tc.policy, tc.allowlist, "", "", tc.requested, inventory, exclusions); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestProviderModelManagementBatchIsTransactional(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "policy-batch", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := a.db.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=?`, providerID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	body := `{"keys":[{"key_id":` + intString(keyID) + `,"model_policy":"allowlist","model_allowlist":"gpt-a,gpt-b","models":["gpt-a"]},{"key_id":999999,"model_policy":"fallback"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID)+"/model-management", strings.NewReader(body))
	a.providerByID(rec, req, adminCtx{})
	if rec.Code == http.StatusOK {
		t.Fatalf("expected transaction failure: %s", rec.Body.String())
	}
	var policy, allowlist string
	if err := a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys WHERE id=?`, keyID).Scan(&policy, &allowlist); err != nil {
		t.Fatal(err)
	}
	if policy != "fallback" || allowlist != "" {
		t.Fatalf("partial batch committed policy=%q allowlist=%q", policy, allowlist)
	}
}

func TestProviderModelManagementBatchSavesInventoryAndReturnsStatuses(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "policy-save", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := a.db.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=?`, providerID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at) VALUES(?,?,?,?,1,?)`, keyID, "gpt-old", "old", "chat", now()); err != nil {
		t.Fatal(err)
	}
	body := `{"keys":[{"key_id":` + intString(keyID) + `,"model_policy":"allowlist","model_allowlist":"GPT-A, gpt-b","models":["gpt-a"]}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID)+"/model-management", strings.NewReader(body))
	a.providerByID(rec, req, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Keys []providerModelManagementStatus `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 1 || out.Keys[0].Status != "saved" {
		t.Fatalf("statuses=%+v", out.Keys)
	}
	var policy, allowlist string
	if err := a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys WHERE id=?`, keyID).Scan(&policy, &allowlist); err != nil {
		t.Fatal(err)
	}
	if policy != "allowlist" || allowlist != "gpt-a,gpt-b" {
		t.Fatalf("policy=%q allowlist=%q", policy, allowlist)
	}
	var oldEnabled, newEnabled int
	if err := a.db.QueryRow(`SELECT enabled FROM provider_api_key_models WHERE provider_key_id=? AND model='gpt-old'`, keyID).Scan(&oldEnabled); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT enabled FROM provider_api_key_models WHERE provider_key_id=? AND model='gpt-a'`, keyID).Scan(&newEnabled); err != nil {
		t.Fatal(err)
	}
	if oldEnabled != 0 || newEnabled != 1 {
		t.Fatalf("inventory old=%d new=%d", oldEnabled, newEnabled)
	}
}
