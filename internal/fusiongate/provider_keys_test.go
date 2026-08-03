package fusiongate

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func insertProviderKeyForTest(t *testing.T, a *App, providerID int64, raw, name, model, egress string, nodeID any, enabled, order int) int64 {
	t.Helper()
	encrypted, err := a.encrypt(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.db.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,ip_pool_node_id,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?, 'untested',?,?)`, providerID, encrypted, a.providerKeyFingerprint(raw), providerKeyHint(raw), name, model, egress, nodeID, enabled, order, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestLegacyAPIKeyProviderMigratesToDefaultCard(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "legacy-key-provider", "openai_compatible", "https://example.test", "sk-legacy-12345678", 1, 100, "normalized", "any", 0, 3, 30)
	// Tests insert legacy providers after startup; invoke the idempotent migration pass.
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count, initialized int
	var hint string
	if err := a.db.QueryRow(`SELECT multi_key_initialized,(SELECT COUNT(*) FROM provider_api_keys WHERE provider_id=providers.id),(SELECT key_hint FROM provider_api_keys WHERE provider_id=providers.id) FROM providers WHERE id=?`, providerID).Scan(&initialized, &count, &hint); err != nil {
		t.Fatal(err)
	}
	if initialized != 1 || count != 1 || hint != "sk-l...5678" {
		t.Fatalf("initialized=%d count=%d hint=%q", initialized, count, hint)
	}
	selected, err := a.selectProviderKey(context.Background(), providerID, "any-model", nil, nil, true)
	if err != nil || selected.Credential != "sk-legacy-12345678" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
}

func TestProviderKeySelectionUsesOrderModelAndEgressOverride(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "multi-key-provider", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1,default_model='gpt-default' WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	link, _ := a.encrypt("socks5://proxy.example.com:1080")
	res, err := a.db.Exec(`INSERT INTO ip_pool_nodes(name,protocol,server,share_link,enabled,local_port,status,created_at,updated_at) VALUES('Node A','socks5','proxy.example.com:1080',?,1,22000,'ready',?,?)`, link, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := res.LastInsertId()
	if _, err := a.db.Exec(`UPDATE providers SET ip_pool_node_id=? WHERE id=?`, nodeID, providerID); err != nil {
		t.Fatal(err)
	}
	insertProviderKeyForTest(t, a, providerID, "sk-disabled", "disabled", "", providerKeyEgressInherit, nil, 0, 0)
	insertProviderKeyForTest(t, a, providerID, "sk-mini-first", "mini", "gpt-mini", providerKeyEgressDirect, nil, 1, 1)
	insertProviderKeyForTest(t, a, providerID, "sk-mini-second", "mini 2", "gpt-mini", providerKeyEgressInherit, nil, 1, 2)
	insertProviderKeyForTest(t, a, providerID, "sk-default", "default", "", providerKeyEgressNode, nodeID, 1, 3)

	providerNode := nodeID
	mini, err := a.selectProviderKey(context.Background(), providerID, "gpt-mini", &providerNode, nil, true)
	if err != nil || mini.Credential != "sk-mini-first" || mini.IPPoolNodeID != nil {
		t.Fatalf("mini=%#v err=%v", mini, err)
	}
	inherited, err := a.selectProviderKey(context.Background(), providerID, "gpt-default", &providerNode, nil, true)
	if err != nil || inherited.Credential != "sk-default" || inherited.IPPoolNodeID == nil || *inherited.IPPoolNodeID != nodeID {
		t.Fatalf("default=%#v err=%v", inherited, err)
	}
	if _, err := a.selectProviderKey(context.Background(), providerID, "unsupported", &providerNode, nil, true); err == nil {
		t.Fatal("unsupported model unexpectedly selected a key")
	}
}

func TestResolveMultiKeyProviderReleasesRouteRowsBeforeKeySelection(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Force route scanning and key selection to share one SQLite connection.
	// resolve must close the route rows before selectProviderKey starts its query.
	a.db.SetMaxOpenConns(1)

	providerID := insertTestProvider(t, a, "resolve-multi-key", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "public-model", "upstream-model", "chat", 0)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	insertProviderKeyForTest(t, a, providerID, "sk-selected", "selected", "upstream-model", providerKeyEgressInherit, nil, 1, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	routes, err := a.resolve(ctx, "public-model", "chat")
	if err != nil {
		t.Fatalf("resolve multi-key route: %v", err)
	}
	if len(routes) != 1 || routes[0].Credential != "sk-selected" {
		t.Fatalf("routes=%#v", routes)
	}
}

func TestProviderKeyAdminMasksSecretsRejectsDuplicateAndDeletesLast(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "admin-key-provider", "openai_compatible", "https://example.test", "sk-original-abcdefgh", 1, 100, "normalized", "any", 0, 3, 30)
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	a.providerKeys(list, httptest.NewRequest(http.MethodGet, "/api/admin/providers/1/keys", nil), providerID)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "sk-original-abcdefgh") || !strings.Contains(list.Body.String(), "sk-o...efgh") {
		t.Fatalf("masked list status=%d body=%s", list.Code, list.Body.String())
	}

	duplicate := httptest.NewRecorder()
	a.providerKeys(duplicate, httptest.NewRequest(http.MethodPost, "/api/admin/providers/1/keys", strings.NewReader(`{"api_key":"sk-original-abcdefgh","name":"duplicate"}`)), providerID)
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), "duplicate_api_key") {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	created := httptest.NewRecorder()
	a.providerKeys(created, httptest.NewRequest(http.MethodPost, "/api/admin/providers/1/keys", strings.NewReader(`{"api_key":"sk-second-12345678","name":"备用","model":"GPT-MINI","egress_mode":"direct","enabled":false}`)), providerID)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var enabled int
	var model, mode string
	if err := a.db.QueryRow(`SELECT enabled,model,egress_mode FROM provider_api_keys WHERE id=?`, result.ID).Scan(&enabled, &model, &mode); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || model != "gpt-mini" || mode != "direct" {
		t.Fatalf("enabled=%d model=%q mode=%q", enabled, model, mode)
	}

	var firstID int64
	if err := a.db.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=? ORDER BY sort_order,id LIMIT 1`, providerID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	reveal := httptest.NewRecorder()
	revealReq := httptest.NewRequest(http.MethodPost, "/api/admin/providers/1/keys/1/reveal", strings.NewReader(`{}`))
	a.providerKeyByID(reveal, revealReq, providerID, firstID, "reveal")
	if reveal.Code != http.StatusOK || reveal.Header().Get("Cache-Control") != "no-store" || !strings.Contains(reveal.Body.String(), "sk-original-abcdefgh") {
		t.Fatalf("reveal status=%d cache=%q body=%s", reveal.Code, reveal.Header().Get("Cache-Control"), reveal.Body.String())
	}

	for _, keyID := range []int64{firstID, result.ID} {
		recorder := httptest.NewRecorder()
		a.providerKeyByID(recorder, httptest.NewRequest(http.MethodDelete, "/api/admin/providers/1/keys/"+intString(keyID), nil), providerID, keyID, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("delete key %d status=%d body=%s", keyID, recorder.Code, recorder.Body.String())
		}
	}
	var initialized int
	if err := a.db.QueryRow(`SELECT multi_key_initialized FROM providers WHERE id=?`, providerID).Scan(&initialized); err != nil || initialized != 1 {
		t.Fatalf("initialized=%d err=%v", initialized, err)
	}
	if _, err := a.selectProviderKey(context.Background(), providerID, "gpt-mini", nil, nil, true); err == nil {
		t.Fatal("deleted last card fell back to hidden legacy credential")
	}
}

func TestProviderKeyPatchPreservesSecretAndSupportsReorder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "patch-key-provider", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	id := insertProviderKeyForTest(t, a, providerID, "sk-keep-secret", "old", "", providerKeyEgressInherit, nil, 1, 9)
	recorder := httptest.NewRecorder()
	a.providerKeyByID(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/1/keys/1", strings.NewReader(`{"name":"主号","model":"gpt-x","enabled":false,"sort_order":1}`)), providerID, id, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var encrypted []byte
	var name, model string
	var enabled, order int
	if err := a.db.QueryRow(`SELECT credential,name,model,enabled,sort_order FROM provider_api_keys WHERE id=?`, id).Scan(&encrypted, &name, &model, &enabled, &order); err != nil {
		t.Fatal(err)
	}
	plain, err := a.decrypt(encrypted)
	if err != nil || plain != "sk-keep-secret" || name != "主号" || model != "gpt-x" || enabled != 0 || order != 1 {
		t.Fatalf("plain=%q name=%q model=%q enabled=%d order=%d err=%v", plain, name, model, enabled, order, err)
	}
}

func TestProviderKeySelectionDoesNotTrySecondKeyAfterUpstreamFailure(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "single-selection-provider", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	insertProviderKeyForTest(t, a, providerID, "sk-first", "first", "", providerKeyEgressInherit, nil, 1, 0)
	insertProviderKeyForTest(t, a, providerID, "sk-second", "second", "", providerKeyEgressInherit, nil, 1, 1)
	selected, err := a.selectProviderKey(context.Background(), providerID, "model", nil, nil, true)
	if err != nil || selected.Credential != "sk-first" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	// Selection is performed once while resolving the provider route. There is
	// no key iterator or retry cursor in resolvedRoute/runRoutes.
	if selected.ID == 0 {
		t.Fatal("selected card ID was not retained")
	}
}

func TestProviderAPIKeyForeignKeyRejectsMissingNode(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "node-validation-provider", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	recorder := httptest.NewRecorder()
	a.providerKeys(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/providers/1/keys", strings.NewReader(`{"api_key":"sk-node","egress_mode":"node","ip_pool_node_id":999}`)), providerID)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing node status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestEffectiveProviderKeyNodeModes(t *testing.T) {
	providerNode := sql.NullInt64{Int64: 10, Valid: true}
	keyNode := sql.NullInt64{Int64: 20, Valid: true}
	tests := []struct {
		mode string
		want *int64
	}{
		{providerKeyEgressInherit, int64Ptr(10)},
		{providerKeyEgressDirect, nil},
		{providerKeyEgressNode, int64Ptr(20)},
	}
	for _, test := range tests {
		got, _ := effectiveProviderKeyNode(test.mode, keyNode, providerNode)
		if (got == nil) != (test.want == nil) || got != nil && *got != *test.want {
			t.Fatalf("mode=%s got=%v want=%v", test.mode, got, test.want)
		}
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestProviderDiscoveryAggregatesAllKeysAndRoutesByInventory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer sk-first-discovery":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"shared"}]}`))
		case "Bearer sk-second-discovery":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"shared"}]}`))
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "inventory-provider", "openai_compatible", upstream.URL+"/v1", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1,default_model='' WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	firstID := insertProviderKeyForTest(t, a, providerID, "sk-first-discovery", "first", "", providerKeyEgressInherit, nil, 1, 0)
	secondID := insertProviderKeyForTest(t, a, providerID, "sk-second-discovery", "second", "", providerKeyEgressInherit, nil, 1, 1)

	result, err := a.discoverProviderModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 3 || len(result.Keys) != 2 || result.Keys[0].Discovered != 2 || result.Keys[1].Discovered != 2 {
		t.Fatalf("result=%#v", result)
	}
	var firstCount, secondCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id=?`, firstID).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id=?`, secondID).Scan(&secondCount); err != nil {
		t.Fatal(err)
	}
	if firstCount != 2 || secondCount != 2 {
		t.Fatalf("inventory counts first=%d second=%d", firstCount, secondCount)
	}
	selected, err := a.selectProviderKey(context.Background(), providerID, "model-b", nil, nil, true)
	if err != nil || selected.ID != secondID || selected.Credential != "sk-second-discovery" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	selected, err = a.selectProviderKey(context.Background(), providerID, "model-a", nil, nil, true)
	if err != nil || selected.ID != firstID {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
}

func TestProviderDiscoveryKeepsSuccessfulKeyWhenAnotherFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-working-discovery" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-working"}]}`))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "partial-inventory-provider", "openai_compatible", upstream.URL+"/v1", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	failedID := insertProviderKeyForTest(t, a, providerID, "sk-failed-discovery", "failed", "", providerKeyEgressInherit, nil, 1, 0)
	workingID := insertProviderKeyForTest(t, a, providerID, "sk-working-discovery", "working", "", providerKeyEgressInherit, nil, 1, 1)

	result, err := a.discoverProviderModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 1 || len(result.Keys) != 2 || result.Keys[0].KeyID != failedID || result.Keys[0].Error == "" || result.Keys[1].KeyID != workingID || result.Keys[1].Error != "" {
		t.Fatalf("result=%#v", result)
	}
}

func TestProviderDiscoveryAggregatesEveryEnabledKeyAndRoutesByInventory(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("Authorization") {
		case "Bearer sk-one":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-common"},{"id":"model-one"}]}`))
		case "Bearer sk-two":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-common"},{"id":"model-two"}]}`))
		default:
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "inventory-provider", "openai_compatible", upstream.URL+"/v1", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1,default_model='' WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	keyOne := insertProviderKeyForTest(t, a, providerID, "sk-one", "one", "", providerKeyEgressInherit, nil, 1, 0)
	keyTwo := insertProviderKeyForTest(t, a, providerID, "sk-two", "two", "", providerKeyEgressInherit, nil, 1, 1)

	result, err := a.discoverProviderModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 3 || len(result.Keys) != 2 || result.Keys[0].Discovered != 2 || result.Keys[1].Discovered != 2 {
		t.Fatalf("discovery=%#v", result)
	}
	var oneCount, twoCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id=?`, keyOne).Scan(&oneCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id=?`, keyTwo).Scan(&twoCount); err != nil {
		t.Fatal(err)
	}
	if oneCount != 2 || twoCount != 2 {
		t.Fatalf("inventory counts one=%d two=%d", oneCount, twoCount)
	}
	selected, err := a.selectProviderKey(context.Background(), providerID, "model-two", nil, nil, true)
	if err != nil || selected.ID != keyTwo || selected.Credential != "sk-two" {
		t.Fatalf("model-two selected=%#v err=%v", selected, err)
	}
	selected, err = a.selectProviderKey(context.Background(), providerID, "model-one", nil, nil, true)
	if err != nil || selected.ID != keyOne || selected.Credential != "sk-one" {
		t.Fatalf("model-one selected=%#v err=%v", selected, err)
	}
	if _, err := a.selectProviderKey(context.Background(), providerID, "model-missing", nil, nil, true); err == nil {
		t.Fatal("discovered inventories unexpectedly allowed an unsupported model")
	}
}

func TestProviderDiscoveryKeepsSuccessfulKeysWhenAnotherKeyFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-good" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"model-good"}]}`))
			return
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "partial-inventory-provider", "openai_compatible", upstream.URL+"/v1", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	_, _ = a.db.Exec(`DELETE FROM provider_api_keys WHERE provider_id=?`, providerID)
	_, _ = a.db.Exec(`UPDATE providers SET multi_key_initialized=1,default_model='' WHERE id=?`, providerID)
	goodID := insertProviderKeyForTest(t, a, providerID, "sk-good", "good", "", providerKeyEgressInherit, nil, 1, 0)
	badID := insertProviderKeyForTest(t, a, providerID, "sk-bad", "bad", "", providerKeyEgressInherit, nil, 1, 1)

	result, err := a.discoverProviderModels(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 1 || len(result.Keys) != 2 || result.Keys[1].Error == "" {
		t.Fatalf("partial discovery=%#v", result)
	}
	var goodStatus, badStatus string
	if err := a.db.QueryRow(`SELECT status FROM provider_api_keys WHERE id=?`, goodID).Scan(&goodStatus); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT status FROM provider_api_keys WHERE id=?`, badID).Scan(&badStatus); err != nil {
		t.Fatal(err)
	}
	if goodStatus != "healthy" || badStatus != "failed" {
		t.Fatalf("good=%q bad=%q", goodStatus, badStatus)
	}
}
