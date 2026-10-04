package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func manualModelsFixture(t *testing.T) (*App, int64, []int64) {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "manual-models", "openai_compatible", "https://no-models.example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	k1 := insertProviderKeyForTest(t, a, p, "fixture-manual-1", "one", "", providerKeyEgressDirect, nil, 1, 0)
	k2 := insertProviderKeyForTest(t, a, p, "fixture-manual-2", "two", "", providerKeyEgressDirect, nil, 1, 1)
	if _, err = a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, p); err != nil {
		t.Fatal(err)
	}
	return a, p, []int64{k1, k2}
}
func TestManualModelsAddSelectedKeysAndRouteWithoutDiscovery(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='' WHERE id=?`, keys[1]); err != nil {
		t.Fatal(err)
	}
	out, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: "Vendor/CaseSensitive", DisplayName: "My model", Capabilities: "chat,stream,tools"}}, CreateRoutes: true, PublicName: "my-coding"})
	if err != nil || out.Keys != 1 || out.RoutesCreated != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	var model, source string
	var count int
	if err = a.db.QueryRow(`SELECT model,model_source FROM provider_api_key_models WHERE provider_key_id=?`, keys[0]).Scan(&model, &source); err != nil {
		t.Fatal(err)
	}
	if model != "Vendor/CaseSensitive" || source != "manual" {
		t.Fatalf("model=%q source=%q", model, source)
	}
	if err = a.db.QueryRow(`SELECT count(*) FROM provider_api_key_models WHERE provider_key_id=?`, keys[1]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("untouched count=%d err=%v", count, err)
	}
	if err = a.db.QueryRow(`SELECT upstream_model FROM model_routes WHERE public_name='my-coding'`).Scan(&model); err != nil || model != "Vendor/CaseSensitive" {
		t.Fatalf("route model=%q err=%v", model, err)
	}
	routes, err := a.resolveRoutes(context.Background(), "my-coding", "inference", false)
	if err != nil || len(routes) != 1 || routes[0].ProviderKeyID != keys[0] {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
}
func TestManualModelsRenameSyncIncludesAllowlistOnlyKeys(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	_, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: "Old"}}, CreateRoutes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='Other, OLD ' WHERE id=?`, keys[1]); err != nil {
		t.Fatal(err)
	}
	_, err = a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], OriginalModel: "Old", Entries: []manualModelEntry{{Model: "New"}}, SyncRoutes: true})
	var e *manualModelError
	if !errors.As(err, &e) || e.status != 409 {
		t.Fatalf("expected allowlist-only conflict err=%v", err)
	}
	var upstream string
	if err = a.db.QueryRow(`SELECT upstream_model FROM model_routes WHERE public_name='old'`).Scan(&upstream); err != nil || upstream != "Old" {
		t.Fatalf("partial route rename upstream=%q err=%v", upstream, err)
	}
	if err = a.db.QueryRow(`SELECT model FROM provider_api_key_models WHERE provider_key_id=?`, keys[0]).Scan(&upstream); err != nil || upstream != "Old" {
		t.Fatalf("partial inventory rename model=%q err=%v", upstream, err)
	}
}

func TestManualModelsRenameAtomicConflictAndAffectedKeys(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	_, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys, Entries: []manualModelEntry{{Model: "Old"}}, CreateRoutes: true})
	if err != nil {
		t.Fatal(err)
	}
	var e *manualModelError
	for _, name := range []string{"New", "OLD"} {
		_, err = a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], OriginalModel: "Old", Entries: []manualModelEntry{{Model: name}}, SyncRoutes: true})
		if !errors.As(err, &e) || e.status != 409 {
			t.Fatalf("expected affected-key conflict new=%q err=%v", name, err)
		}
	}
	_, err = a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[1:], Entries: []manualModelEntry{{Model: "New"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys, OriginalModel: "Old", Entries: []manualModelEntry{{Model: "New"}}, SyncRoutes: true})
	if !errors.As(err, &e) || e.status != 409 {
		t.Fatalf("expected target conflict err=%v", err)
	}
	var count int
	if err = a.db.QueryRow(`SELECT count(*) FROM provider_api_key_models WHERE model='Old'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("partial rename count=%d err=%v", count, err)
	}
	out, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys, OriginalModel: "Old", Entries: []manualModelEntry{{Model: "FinalCase"}}, SyncRoutes: true})
	if err != nil || out.RoutesUpdated != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	var model string
	if err = a.db.QueryRow(`SELECT upstream_model FROM model_routes WHERE public_name='old'`).Scan(&model); err != nil || model != "FinalCase" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	if err = a.persistProviderKeyDiscovery(context.Background(), keys[0], []discoveredModel{{ID: "old", UpstreamID: "old", Capabilities: "chat"}}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow(`SELECT count(*) FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)='old'`, keys[0]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("old renamed model reappeared count=%d err=%v", count, err)
	}
}
func TestManualModelsDiscoveryPreservesManualAndDisabledMetadata(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	disabled := false
	_, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: "CustomCase", DisplayName: "Protected display", Capabilities: "chat,tools"}}, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.persistProviderKeyDiscovery(context.Background(), keys[0], []discoveredModel{{ID: "customcase", UpstreamID: "customcase", DisplayName: "Remote display", Capabilities: "chat,stream"}, {ID: "new", UpstreamID: "new", Capabilities: "chat"}}, 1, nil); err != nil {
		t.Fatal(err)
	}
	var model, display, caps, source string
	var enabled int
	if err = a.db.QueryRow(`SELECT model,display_name,capabilities,enabled,model_source FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)='customcase'`, keys[0]).Scan(&model, &display, &caps, &enabled, &source); err != nil {
		t.Fatal(err)
	}
	// Excluded disabled models stay untouched rather than being reintroduced.
	if model != "CustomCase" || display != "Protected display" || caps != "chat,tools" || enabled != 0 {
		t.Fatalf("model=%q display=%q caps=%q enabled=%d source=%q", model, display, caps, enabled, source)
	}
	_, err = a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], OriginalModel: "CustomCase", Entries: []manualModelEntry{{Model: "CustomCase", DisplayName: "Protected display", Capabilities: "chat,tools"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.persistProviderKeyDiscovery(context.Background(), keys[0], []discoveredModel{{ID: "customcase", UpstreamID: "customcase", DisplayName: "Remote display", Capabilities: "chat,stream"}}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow(`SELECT model,display_name,capabilities,enabled,model_source FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)='customcase'`, keys[0]).Scan(&model, &display, &caps, &enabled, &source); err != nil {
		t.Fatal(err)
	}
	if model != "CustomCase" || display != "Protected display" || caps != "chat,tools" || enabled != 1 || source != "both" {
		t.Fatalf("model=%q display=%q caps=%q enabled=%d source=%q", model, display, caps, enabled, source)
	}
	if err = a.persistProviderKeyDiscovery(context.Background(), keys[0], nil, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow(`SELECT model_source FROM provider_api_key_models WHERE provider_key_id=? AND model='CustomCase'`, keys[0]).Scan(&source); err != nil || source != "both" {
		t.Fatalf("source=%q err=%v", source, err)
	}
}
func TestManualModelsBackupRoundTripKeepsMetadata(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	_, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: "Vendor/OriginalCase", DisplayName: "My display", Capabilities: "chat,tools"}}, CreateRoutes: true, PublicName: "my-model"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.providerBackupExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/providers/export", strings.NewReader(`{}`)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	var backup providerBackupFile
	if err = json.Unmarshal(rec.Body.Bytes(), &backup); err != nil {
		t.Fatal(err)
	}
	m := backup.Providers[0].Keys[0].Models[0]
	if m.Model != "Vendor/OriginalCase" || m.ModelSource != "manual" || m.ManualDisplayName != "My display" || m.ManualCapabilities != "chat,tools" {
		t.Fatalf("exported model=%+v", m)
	}
	b, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	imported := httptest.NewRecorder()
	b.providerBackupImport(imported, httptest.NewRequest(http.MethodPost, "/api/admin/providers/import", bytes.NewReader(rec.Body.Bytes())), adminCtx{})
	if imported.Code != 200 {
		t.Fatalf("import status=%d body=%s", imported.Code, imported.Body.String())
	}
	var model, source, display, caps string
	if err = b.db.QueryRow(`SELECT model,model_source,manual_display_name,manual_capabilities FROM provider_api_key_models`).Scan(&model, &source, &display, &caps); err != nil {
		t.Fatal(err)
	}
	if model != m.Model || source != m.ModelSource || display != m.ManualDisplayName || caps != m.ManualCapabilities {
		t.Fatalf("restored model=%q source=%q display=%q caps=%q", model, source, display, caps)
	}
}

func TestManualModelsPermissionSaveDoesNotDeleteMappings(t *testing.T) {
	a, p, keys := manualModelsFixture(t)
	_, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: "KeptCase"}}, CreateRoutes: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: keys[0], ModelPolicy: "allowlist", Models: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var model, source string
	if err = a.db.QueryRow(`SELECT upstream_model FROM model_routes WHERE public_name='keptcase'`).Scan(&model); err != nil || model != "KeptCase" {
		t.Fatalf("mapping lost model=%q err=%v", model, err)
	}
	if err = a.db.QueryRow(`SELECT model_source FROM provider_api_key_models WHERE provider_key_id=?`, keys[0]).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("source=%q err=%v", source, err)
	}
	_, err = a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: keys[0], ModelPolicy: "allowlist", Models: []string{"KeptCase"}}})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = a.db.QueryRow(`SELECT count(*) FROM provider_api_key_models WHERE provider_key_id=?`, keys[0]).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate inventory n=%d err=%v", n, err)
	}
	if _, err = a.db.Exec(`UPDATE model_routes SET enabled=0 WHERE public_name='keptcase'`); err != nil {
		t.Fatal(err)
	}
	_, err = a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: keys[0], Models: []string{"KeptCase"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow(`SELECT enabled FROM model_routes WHERE public_name='keptcase'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("route unintentionally enabled=%d err=%v", n, err)
	}
}
