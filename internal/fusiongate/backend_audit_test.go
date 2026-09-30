package fusiongate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func auditApp(t *testing.T) (*App, int64, int64) {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "audit-provider", "openai_compatible", "https://example.test", "audit-secret", 1, 100, "normalized", "any", 0, 3, 30)
	if err := a.migrateProviderAPIKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	var k int64
	if err := a.db.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=?`, p).Scan(&k); err != nil {
		t.Fatal(err)
	}
	return a, p, k
}

func auditExec(t *testing.T, a *App, query string, args ...any) {
	t.Helper()
	if _, err := a.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func auditInventory(t *testing.T, a *App, k int64, model string, enabled int) {
	t.Helper()
	auditExec(t, a, `INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at) VALUES(?,?,?,'chat,stream',?,?)`, k, model, model, enabled, now())
}

func auditExport(t *testing.T, a *App) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	a.providerBackupExport(rec, httptest.NewRequest("POST", "/api/admin/providers/export", strings.NewReader(`{}`)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func auditImport(t *testing.T, a *App, body []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.providerBackupImport(rec, httptest.NewRequest("POST", "/api/admin/providers/import", bytes.NewReader(body)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuditSingleKeyEgressBinding(t *testing.T) {
	a, p, k := auditApp(t)
	rec := httptest.NewRecorder()
	a.providerKeyByID(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"egress_mode":"direct"}`)), p, k, "")
	var policy, mode string
	if err := a.db.QueryRow(`SELECT model_policy,egress_mode FROM provider_api_keys WHERE id=?`, k).Scan(&policy, &mode); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || policy != "fallback" || mode != "direct" {
		t.Fatalf("PATCH=%d, model_policy=%q egress_mode=%q; want fallback/direct", rec.Code, policy, mode)
	}
}

func TestAuditKeyCreationPreservesPolicy(t *testing.T) {
	a, p, _ := auditApp(t)
	rec := httptest.NewRecorder()
	a.providerKeys(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"api_key":"second-audit-secret","model_policy":"allowlist","model_allowlist":"gpt-a"}`)), p)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rec.Code, rec.Body.String())
	}
	var policy, allowlist string
	if err := a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys WHERE provider_id=? ORDER BY id DESC LIMIT 1`, p).Scan(&policy, &allowlist); err != nil {
		t.Fatal(err)
	}
	if policy != "allowlist" || allowlist != "gpt-a" {
		t.Fatalf("accepted allowlist but stored policy=%q allowlist=%q", policy, allowlist)
	}
}

func TestAuditModelManagementUIPayloadRoutes(t *testing.T) {
	a, p, k := auditApp(t)
	body := `{"keys":[{"key_id":` + intString(k) + `,"model_policy":"allowlist","models":["gpt-a"],"exclude_models":[]}]}`
	rec := httptest.NewRecorder()
	a.providerByID(rec, httptest.NewRequest("PATCH", "/api/admin/providers/"+intString(p)+"/model-management", strings.NewReader(body)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("save=%d %s", rec.Code, rec.Body.String())
	}
	keys, err := a.selectProviderKeys(context.Background(), p, "gpt-a", nil, nil, true)
	if err != nil || len(keys) != 1 {
		t.Fatalf("saved selected model but candidates=%d err=%v", len(keys), err)
	}
}

func TestAuditModelManagementKeepsFallbackRoute(t *testing.T) {
	a, p, k := auditApp(t)
	insertProviderKeyForTest(t, a, p, "fixed-secret", "fixed-model-key", "manual-upstream", "inherit", nil, 1, 1)
	insertTestRoute(t, a, p, "manual-public", "manual-upstream", "chat", 0)
	if _, err := a.selectProviderKeys(context.Background(), p, "manual-upstream", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	_, err := a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: k, ModelPolicy: "fallback", Models: []string{"gpt-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=? AND public_name='manual-public'`, p).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("saving one key deleted a valid manual route served by an untouched fixed-model key")
	}
}

func TestAuditModelManagementEnablesExistingRoute(t *testing.T) {
	a, p, k := auditApp(t)
	id := insertTestRoute(t, a, p, "gpt-a", "gpt-a", "chat", 0)
	auditExec(t, a, `UPDATE model_routes SET enabled=0 WHERE id=?`, id)
	if _, err := a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: k, ModelPolicy: "fallback", Models: []string{"gpt-a"}}}); err != nil {
		t.Fatal(err)
	}
	var enabled int
	if err := a.db.QueryRow(`SELECT enabled FROM model_routes WHERE id=?`, id).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatal("selecting model left its existing route disabled")
	}
}

func TestAuditPasswordChangeSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.changePassword(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"current_password":"correct horse battery staple","new_password":"new-audit-password"}`)), adminCtx{})
	if rec.Code != 200 {
		a.Close()
		t.Fatalf("change=%d %s", rec.Code, rec.Body.String())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.AdminPassword = ""
	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("same deployment cannot restart after successful password change: %v", err)
	}
	restarted.Close()
}

func TestAuditPasswordChangeKeepsCurrentSession(t *testing.T) {
	a, _, _ := auditApp(t)
	login := httptest.NewRecorder()
	csrf := a.setAdminCookies(login, httptest.NewRequest("POST", "/", nil))
	req := httptest.NewRequest("POST", "/api/admin/password", strings.NewReader(`{"current_password":"correct horse battery staple","new_password":"new-audit-password"}`))
	req.Header.Set("X-CSRF-Token", csrf)
	for _, cookie := range login.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body.String())
	}
	if ctx, ok := a.adminAuth(req); !ok || ctx.CSRF != csrf {
		t.Fatal("password change invalidated the current session or its CSRF token")
	}
}

func TestAuditPasswordChangeRevokesOtherSession(t *testing.T) {
	a, _, _ := auditApp(t)
	rec := httptest.NewRecorder()
	a.setAdminCookies(rec, httptest.NewRequest("POST", "/", nil))
	oldRequest := httptest.NewRequest("GET", "/api/admin/session", nil)
	for _, cookie := range rec.Result().Cookies() {
		oldRequest.AddCookie(cookie)
	}
	change := httptest.NewRecorder()
	a.changePassword(change, httptest.NewRequest("POST", "/", strings.NewReader(`{"current_password":"correct horse battery staple","new_password":"new-audit-password"}`)), adminCtx{})
	if change.Code != 200 {
		t.Fatal(change.Body.String())
	}
	if _, ok := a.adminAuth(oldRequest); ok {
		t.Fatal("other pre-change administrator session still has full access")
	}
}

func TestAuditBackupPreservesModelRestrictions(t *testing.T) {
	a, _, k := auditApp(t)
	auditExec(t, a, `UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='gpt-a' WHERE id=?`, k)
	auditExec(t, a, `INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,'gpt-blocked',?)`, k, now())
	body := auditExport(t, a)
	target, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	auditImport(t, target, body)
	var policy, allowlist string
	var excluded int
	if err := target.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys`).Scan(&policy, &allowlist); err != nil {
		t.Fatal(err)
	}
	if err := target.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_model_exclusions`).Scan(&excluded); err != nil {
		t.Fatal(err)
	}
	if policy != "allowlist" || allowlist != "gpt-a" || excluded != 1 {
		t.Fatalf("restore lost restrictions: policy=%q allowlist=%q exclusions=%d", policy, allowlist, excluded)
	}
}

func TestAuditBackupRestoresEmptyInventory(t *testing.T) {
	a, _, k := auditApp(t)
	body := auditExport(t, a)
	auditInventory(t, a, k, "later-model", 1)
	auditExec(t, a, `INSERT INTO provider_api_key_model_health(provider_key_id,model,status,last_checked_at) VALUES(?,'later-model','healthy',?)`, k, now())
	auditImport(t, a, body)
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_models WHERE provider_key_id=?`, k).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("empty backup inventory kept %d post-backup inventory rows", count)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM provider_api_key_model_health WHERE provider_key_id=?`, k).Scan(&count); err != nil || count != 0 {
		t.Fatalf("restore kept stale health inventory: count=%d err=%v", count, err)
	}
}

func TestAuditBackupLegacyMergeKeepsMissingRestrictions(t *testing.T) {
	a, _, k := auditApp(t)
	body := auditExport(t, a)
	var backup providerBackupFile
	if err := json.Unmarshal(body, &backup); err != nil {
		t.Fatal(err)
	}
	backup.Version = 1
	backup.Providers[0].Archived = nil
	backup.Providers[0].RouteExclusions = nil
	key := &backup.Providers[0].Keys[0]
	key.ModelPolicy, key.ModelAllowlist, key.Models, key.ExcludeModels = "", nil, nil, nil
	auditExec(t, a, `UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='gpt-a' WHERE id=?`, k)
	auditInventory(t, a, k, "gpt-a", 1)
	auditExec(t, a, `INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,'blocked',?)`, k, now())
	auditExec(t, a, `UPDATE providers SET archived=1`)
	body, _ = json.Marshal(backup)
	auditImport(t, a, body)
	var policy, allowlist string
	var archived, count int
	if err := a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys WHERE id=?`, k).Scan(&policy, &allowlist); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT archived FROM providers`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM provider_api_key_models)+(SELECT COUNT(*) FROM provider_api_key_model_exclusions)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if policy != "allowlist" || allowlist != "gpt-a" || archived != 1 || count != 2 {
		t.Fatalf("legacy merge lost settings: policy=%s allowlist=%s archived=%d model records=%d", policy, allowlist, archived, count)
	}
}

func TestAuditBackupRouteExclusionsAndKeylessChannel(t *testing.T) {
	a, p, k := auditApp(t)
	auditExec(t, a, `INSERT INTO model_route_exclusions(provider_id,public_name,upstream_model,created_at) VALUES(?,'public-a','upstream-a',?)`, p, now())
	body := auditExport(t, a)
	auditExec(t, a, `DELETE FROM model_route_exclusions WHERE provider_id=?`, p)
	auditImport(t, a, body)
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_route_exclusions WHERE provider_id=?`, p).Scan(&count); err != nil || count != 1 {
		t.Fatalf("route exclusion round trip: count=%d err=%v", count, err)
	}
	auditExec(t, a, `DELETE FROM provider_api_keys WHERE id=?`, k)
	body = auditExport(t, a)
	target, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	auditImport(t, target, body)
	if err := target.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE name='audit-provider' AND multi_key_initialized=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("keyless channel round trip: count=%d err=%v", count, err)
	}
	if err := target.db.QueryRow(`SELECT COUNT(*) FROM provider_api_keys`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("keyless channel synthesized a credential: count=%d err=%v", count, err)
	}
}

func TestAuditSingleKeyCombinedBindingsAndNode(t *testing.T) {
	a, p, k := auditApp(t)
	link, err := a.encrypt("socks5://proxy.example.com:1080")
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.db.Exec(`INSERT INTO ip_pool_nodes(name,protocol,server,share_link,enabled,local_port,status,created_at,updated_at) VALUES('audit-node','socks5','proxy.example.com:1080',?,1,22000,'ready',?,?)`, link, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	node, _ := res.LastInsertId()
	rec := httptest.NewRecorder()
	a.providerKeyByID(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"egress_mode":"node","ip_pool_node_id":`+intString(node)+`,"model_policy":"allowlist","model_allowlist":"gpt-a","models":["gpt-a"],"cost_multiplier":1.25}`)), p, k, "")
	var policy, allowlist, mode string
	var nodeID int64
	var cost float64
	if err := a.db.QueryRow(`SELECT model_policy,model_allowlist,egress_mode,ip_pool_node_id,cost_multiplier FROM provider_api_keys WHERE id=?`, k).Scan(&policy, &allowlist, &mode, &nodeID, &cost); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || policy != "allowlist" || allowlist != "gpt-a" || mode != "node" || nodeID != node || cost != 1.25 {
		t.Fatalf("combined patch: status=%d policy=%s list=%s egress=%s node=%d cost=%v", rec.Code, policy, allowlist, mode, nodeID, cost)
	}
}

func TestAuditModelManagementExplicitEmptyAllowlist(t *testing.T) {
	a, p, k := auditApp(t)
	auditExec(t, a, `UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='gpt-a' WHERE id=?`, k)
	rec := httptest.NewRecorder()
	a.providerByID(rec, httptest.NewRequest("PATCH", "/api/admin/providers/"+intString(p)+"/model-management", strings.NewReader(`{"keys":[{"key_id":`+intString(k)+`,"model_allowlist":""}]}`)), adminCtx{})
	var value string
	if err := a.db.QueryRow(`SELECT model_allowlist FROM provider_api_keys WHERE id=?`, k).Scan(&value); err != nil || value != "" || rec.Code != 200 {
		t.Fatalf("explicit empty allowlist: value=%q status=%d err=%v", value, rec.Code, err)
	}
}

func TestAuditStoredInvalidAccessSettingsFailClosed(t *testing.T) {
	a, _, _ := auditApp(t)
	raw := insertTestKey(t, a, false)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	key, _ := a.authenticateKey(req)
	for _, query := range []string{
		`UPDATE api_keys SET expires_at='not-a-date',budget_micros=0 WHERE id=?`,
		`UPDATE api_keys SET expires_at=NULL,budget_micros=-1 WHERE id=?`,
	} {
		auditExec(t, a, query, key.ID)
		if _, ok := a.authenticateKey(req); ok {
			t.Fatal("invalid stored access-key settings were accepted")
		}
	}
}

func TestAuditProviderEditDoesNotEraseModelPermissions(t *testing.T) {
	a, p, k := auditApp(t)
	auditInventory(t, a, k, "gpt-blocked", 0)
	if _, err := a.selectProviderKeys(context.Background(), p, "gpt-blocked", nil, nil, true); err == nil {
		t.Fatal("setup allowed blocked model")
	}
	rec := httptest.NewRecorder()
	a.providerUpdate(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"credential":"new-audit-secret"}`)), p)
	if rec.Code != 200 {
		t.Fatalf("update=%d %s", rec.Code, rec.Body.String())
	}
	if _, err := a.selectProviderKeys(context.Background(), p, "gpt-blocked", nil, nil, true); err == nil {
		t.Fatal("credential edit deleted disabled inventory and made blocked model eligible")
	}
}

func TestAuditSingleKeyPatchRollsBackOnModelFailure(t *testing.T) {
	a, p, k := auditApp(t)
	auditExec(t, a, `CREATE TRIGGER audit_fail_model BEFORE INSERT ON provider_api_key_models BEGIN SELECT RAISE(ABORT,'audit model failure'); END`)
	rec := httptest.NewRecorder()
	a.providerKeyByID(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"name":"changed-despite-error","models":["gpt-a"]}`)), p, k, "")
	if rec.Code != 500 {
		t.Fatalf("fault injection=%d %s", rec.Code, rec.Body.String())
	}
	var name string
	if err := a.db.QueryRow(`SELECT name FROM provider_api_keys WHERE id=?`, k).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name == "changed-despite-error" {
		t.Fatal("failed PATCH already committed key metadata before model error")
	}
}

func TestAuditBackupKeepsArchivedProvider(t *testing.T) {
	a, _, k := auditApp(t)
	auditExec(t, a, `UPDATE providers SET archived=1 WHERE id=(SELECT provider_id FROM provider_api_keys WHERE id=?)`, k)
	body := auditExport(t, a)
	target, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	auditImport(t, target, body)
	var archived int
	if err := target.db.QueryRow(`SELECT archived FROM providers`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Fatal("restoring backup reactivates archived enabled provider")
	}
}

func TestAuditBackupRejectsInvalidCostMultiplier(t *testing.T) {
	a, _, _ := auditApp(t)
	body := auditExport(t, a)
	var backup providerBackupFile
	if err := json.Unmarshal(body, &backup); err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{-1, 0, 1001} {
		backup.Providers[0].Keys[0].CostMultiplier = &value
		if err := validateProviderBackup(&backup, a.cfg); err == nil {
			t.Fatalf("backup accepts invalid key multiplier %v", value)
		}
	}
}

func TestAuditSingleKeyModelSaveSynchronizesRoute(t *testing.T) {
	a, p, k := auditApp(t)
	rec := httptest.NewRecorder()
	a.providerKeyByID(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"models":["gpt-a"]}`)), p, k, "")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var routeID int64
	err := a.db.QueryRow(`SELECT id FROM model_routes WHERE provider_id=? AND upstream_model='gpt-a'`, p).Scan(&routeID)
	if err == sql.ErrNoRows {
		t.Fatal("successful single-key model selection does not create a gateway route")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditModelManagementPreservesDiscoveredCapabilities(t *testing.T) {
	a, p, k := auditApp(t)
	auditInventory(t, a, k, "image-a", 0)
	auditExec(t, a, `UPDATE provider_api_key_models SET capabilities='image' WHERE provider_key_id=? AND model='image-a'`, k)
	if _, err := a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: k, Models: []string{"image-a"}}}); err != nil {
		t.Fatal(err)
	}
	var caps string
	if err := a.db.QueryRow(`SELECT capabilities FROM model_routes WHERE provider_id=? AND upstream_model='image-a'`, p).Scan(&caps); err != nil {
		t.Fatal(err)
	}
	if caps != "image" {
		t.Fatalf("known image model route has capabilities=%q instead of image", caps)
	}
}

func TestAuditAccessKeyUpdateRejectsInvalidExpiry(t *testing.T) {
	a, _, _ := auditApp(t)
	raw := insertTestKey(t, a, false)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	key, ok := a.authenticateKey(req)
	if !ok {
		t.Fatal("setup key rejected")
	}
	auditExec(t, a, `UPDATE api_keys SET expires_at='2020-01-01T00:00:00Z' WHERE id=?`, key.ID)
	rec := httptest.NewRecorder()
	a.keyUpdate(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"expires_at":"not-a-date"}`)), intString(key.ID))
	if rec.Code != 400 {
		_, accepted := a.authenticateKey(req)
		t.Fatalf("invalid expiry PATCH=%d; previously expired key is accepted=%v", rec.Code, accepted)
	}
}

func TestAuditAccessKeyUpdateRejectsNegativeBudget(t *testing.T) {
	a, _, _ := auditApp(t)
	raw := insertTestKey(t, a, false)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	key, ok := a.authenticateKey(req)
	if !ok {
		t.Fatal("setup key rejected")
	}
	auditExec(t, a, `UPDATE api_keys SET budget_micros=100,spent_micros=100 WHERE id=?`, key.ID)
	rec := httptest.NewRecorder()
	a.keyUpdate(rec, httptest.NewRequest("PATCH", "/", strings.NewReader(`{"budget_usd":-1}`)), intString(key.ID))
	if rec.Code != 400 {
		key, _ = a.authenticateKey(req)
		t.Fatalf("negative budget PATCH=%d; budget_micros=%d disables budget check", rec.Code, key.BudgetMicros)
	}
}

func TestAuditControlAdminMutationRequiresCSRF(t *testing.T) {
	a, _, _ := auditApp(t)
	login := httptest.NewRecorder()
	a.setAdminCookies(login, httptest.NewRequest("POST", "/", nil))
	req := httptest.NewRequest("POST", "/api/admin/providers/import", strings.NewReader(`{}`))
	for _, cookie := range login.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "csrf_failed") {
		t.Fatalf("mutation without CSRF=%d %s", rec.Code, rec.Body.String())
	}
}

func TestAuditControlBackupImportRollsBackOnLaterConflict(t *testing.T) {
	a, _, _ := auditApp(t)
	var backup providerBackupFile
	if err := json.Unmarshal(auditExport(t, a), &backup); err != nil {
		t.Fatal(err)
	}
	p := backup.Providers[0]
	p.Name = "new-before-conflict"
	backup.Providers = append([]providerBackupProvider{p}, backup.Providers...)
	auditExec(t, a, `UPDATE providers SET auth_kind='oauth'`)
	body, _ := json.Marshal(backup)
	rec := httptest.NewRecorder()
	a.providerBackupImport(rec, httptest.NewRequest("POST", "/", bytes.NewReader(body)), adminCtx{})
	if rec.Code != 409 {
		t.Fatalf("conflict=%d %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE name='new-before-conflict'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("backup import committed first provider before later conflict")
	}
}
