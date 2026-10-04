package fusiongate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestManualModelMappingUsesConfiguredNamesAndBackup(t *testing.T) {
	var failed, backup atomic.Int32
	handler := func(expected string, calls *atomic.Int32, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			var model string
			_ = json.Unmarshal(body["model"], &model)
			if model != expected {
				t.Errorf("forwarded model=%q want=%q", model, expected)
			}
			if r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Errorf("unexpected selected key")
			}
			calls.Add(1)
			w.WriteHeader(status)
			if status == 500 {
				_, _ = w.Write([]byte(`{"error":{"code":"auth_unavailable"}}`))
			} else {
				_, _ = w.Write([]byte(`{"answer":"manual backup"}`))
			}
		}
	}
	a, key, done := passthroughFixture(t, handler("Vendor/Model-Primary", &failed, 500), handler("Vendor/Model-Backup", &backup, 200))
	defer done()
	if _, err := a.db.Exec(`DELETE FROM model_routes`); err != nil {
		t.Fatal(err)
	}
	for i, model := range []string{"Vendor/Model-Primary", "Vendor/Model-Backup"} {
		var providerID int64
		if err := a.db.QueryRow(`SELECT id FROM providers WHERE name=?`, fmt.Sprintf("passthrough-%d", i)).Scan(&providerID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
			t.Fatal(err)
		}
		keyID := insertProviderKeyForTest(t, a, providerID, "fixture-key", "manual", "", "inherit", nil, 1, 0)
		if _, err := a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist' WHERE id=?`, keyID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.saveManualModels(context.Background(), providerID, manualModelsInput{KeyIDs: []int64{keyID}, Entries: []manualModelEntry{{Model: model, Capabilities: "chat,stream"}}, CreateRoutes: true, PublicName: "custom-code"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"custom-code","messages":[{"role":"user","content":"test"}]}`, "")
	if rec.Code != 200 || rec.Body.String() != `{"answer":"manual backup"}` || failed.Load() != 1 || backup.Load() != 1 {
		t.Fatalf("status=%d body=%s failed=%d backup=%d", rec.Code, rec.Body.String(), failed.Load(), backup.Load())
	}
}
