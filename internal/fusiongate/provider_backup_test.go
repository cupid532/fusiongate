package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderBackupExportIncludesKeysInventoryAndRoutes(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "backup-source", "openai_compatible", "https://backup.example.com", "sk-backup-primary-123456", 7, 80, "normalized", "any", 3, 4, 45)
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondID := insertProviderKeyForTest(t, a, providerID, "sk-backup-secondary-654321", "备用 Key", "glm-test", providerKeyEgressDirect, nil, 1, 2)
	if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at) VALUES(?,?,?,?,?)`, secondID, "glm-test", "GLM Test", "chat,stream,tools", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,cached_price_micros,output_price_micros,pricing_source,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "glm-public", providerID, "glm-test", "chat,stream,tools", 1, 9, 3, 100, 20, 300, "manual", now(), now()); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	a.providerBackupExport(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/providers/export", strings.NewReader(`{}`)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Disposition"), "fusiongate-providers-") || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers=%v", recorder.Header())
	}
	var backup providerBackupFile
	if err := json.Unmarshal(recorder.Body.Bytes(), &backup); err != nil {
		t.Fatal(err)
	}
	if backup.Format != providerBackupFormat || backup.Version != providerBackupVersion || !backup.ContainsSecrets || len(backup.Providers) != 1 {
		t.Fatalf("backup=%#v", backup)
	}
	provider := backup.Providers[0]
	if provider.Name != "backup-source" || provider.BaseURL != "https://backup.example.com" || len(provider.Keys) != 2 || len(provider.Routes) != 1 {
		t.Fatalf("provider=%#v", provider)
	}
	if provider.Keys[0].APIKey != "sk-backup-primary-123456" || provider.Keys[1].APIKey != "sk-backup-secondary-654321" {
		t.Fatalf("keys=%#v", provider.Keys)
	}
	if len(provider.Keys[1].Models) != 1 || provider.Keys[1].Models[0].Model != "glm-test" || provider.Routes[0].PublicName != "glm-public" {
		t.Fatalf("inventory/routes=%#v %#v", provider.Keys[1].Models, provider.Routes)
	}
}

func TestProviderBackupImportCreatesThenMergesWithoutDuplicates(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	backup := providerBackupFile{
		Format: providerBackupFormat, Version: providerBackupVersion, ContainsSecrets: true,
		Providers: []providerBackupProvider{{
			Name: "imported", Type: "openai_compatible", BaseURL: "https://import.example.com", Notes: "restored", Enabled: true,
			Priority: 5, Weight: 90, PassthroughMode: "normalized", ClientPolicy: "any", RequestTimeoutMS: 90000, FailureThreshold: 4, CooldownSeconds: 60,
			Keys: []providerBackupKey{
				{Name: "主 Key", APIKey: "sk-import-primary-123456", EgressMode: providerKeyEgressInherit, Enabled: true, SortOrder: 0, Models: []providerBackupKeyModel{{Model: "deepseek-test", DisplayName: "DeepSeek Test", Capabilities: "chat,stream"}}},
				{Name: "备用 Key", APIKey: "sk-import-secondary-654321", Model: "glm-test", EgressMode: providerKeyEgressDirect, Enabled: true, SortOrder: 1},
			},
			Routes: []providerBackupRoute{{PublicName: "deepseek-public", UpstreamModel: "deepseek-test", Capabilities: "chat,stream", Enabled: true, Priority: 10, InputPriceMicros: 100, OutputPriceMicros: 200, PricingSource: "manual"}},
		}},
	}
	body, _ := json.Marshal(backup)
	importOnce := func() providerBackupImportResult {
		recorder := httptest.NewRecorder()
		a.providerBackupImport(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/providers/import", bytes.NewReader(body)), adminCtx{})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var result providerBackupImportResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := importOnce()
	if first.ProvidersCreated != 1 || first.KeysCreated != 2 || first.RoutesCreated != 1 {
		t.Fatalf("first=%#v", first)
	}
	second := importOnce()
	if second.ProvidersUpdated != 1 || second.KeysUpdated != 2 || second.RoutesUpdated != 1 {
		t.Fatalf("second=%#v", second)
	}
	var providerID int64
	var encrypted []byte
	var providerCount, keyCount, routeCount, inventoryCount int
	if err := a.db.QueryRow(`SELECT id,credential FROM providers WHERE name='imported'`).Scan(&providerID, &encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := a.decrypt(encrypted)
	if err != nil || plain != "sk-import-primary-123456" {
		t.Fatalf("credential=%q err=%v", plain, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE name='imported'`).Scan(&providerCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_keys WHERE provider_id=?`, providerID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, providerID).Scan(&routeCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id IN (SELECT id FROM provider_api_keys WHERE provider_id=?)`, providerID).Scan(&inventoryCount); err != nil {
		t.Fatal(err)
	}
	if providerCount != 1 || keyCount != 2 || routeCount != 1 || inventoryCount != 1 {
		t.Fatalf("counts provider=%d keys=%d routes=%d inventory=%d", providerCount, keyCount, routeCount, inventoryCount)
	}
}

func TestProviderBackupImportRejectsInvalidOrOAuthConflict(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	bad := httptest.NewRecorder()
	a.providerBackupImport(bad, httptest.NewRequest(http.MethodPost, "/api/admin/providers/import", strings.NewReader(`{"format":"unknown","version":1,"providers":[]}`)), adminCtx{})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", bad.Code, bad.Body.String())
	}
	encrypted, _ := a.encrypt("oauth-token")
	if _, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,auth_kind,auth_source,enabled,priority,weight,status,created_at,updated_at) VALUES(?,?,?,?,?,'test',1,1,100,'healthy',?,?)`, "oauth-name", "codex_oauth", "https://chatgpt.com", encrypted, "oauth", now(), now()); err != nil {
		t.Fatal(err)
	}
	backup := providerBackupFile{Format: providerBackupFormat, Version: providerBackupVersion, Providers: []providerBackupProvider{{Name: "oauth-name", Type: "openai_compatible", BaseURL: "https://example.com", Enabled: true, Weight: 100, PassthroughMode: "normalized", ClientPolicy: "any", RequestTimeoutMS: 120000, FailureThreshold: 3, CooldownSeconds: 30, Keys: []providerBackupKey{{APIKey: "sk-not-oauth-123456", EgressMode: providerKeyEgressInherit, Enabled: true}}}}}
	body, _ := json.Marshal(backup)
	conflict := httptest.NewRecorder()
	a.providerBackupImport(conflict, httptest.NewRequest(http.MethodPost, "/api/admin/providers/import", bytes.NewReader(body)), adminCtx{})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}
