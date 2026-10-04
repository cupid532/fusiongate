package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Withdrawing one model's permission must drop that public route while leaving
// the models that are still selective untouched, and re-enabling must restore
// the route the same save recorded as an exclusion.
func TestModelPermissionRemovalDropsOnlyThatRoute(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "cline-like", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	key := insertProviderKeyForTest(t, a, p, "sk-test", "10.4", "", providerKeyEgressInherit, nil, 1, 0)
	save := func(models ...string) {
		t.Helper()
		if _, err := a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: key, ModelPolicy: "fallback", Models: models}}); err != nil {
			t.Fatal(err)
		}
	}
	save("cline-pass/keep", "cline-pass/drop")
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=?`, p).Scan(&n); err != nil || n != 2 {
		t.Fatalf("initial routes=%d err=%v", n, err)
	}
	save("cline-pass/keep")
	if err := a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=? AND LOWER(upstream_model)='cline-pass/drop'`, p).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unservable route kept count=%d err=%v", n, err)
	}
	if err := a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=? AND LOWER(upstream_model)='cline-pass/keep'`, p).Scan(&n); err != nil || n != 1 {
		t.Fatalf("servable route lost count=%d err=%v", n, err)
	}
	save("cline-pass/keep", "cline-pass/drop")
	if err := a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=?`, p).Scan(&n); err != nil || n != 2 {
		t.Fatalf("restored routes=%d err=%v", n, err)
	}
}

// A route whose upstream the Key configuration never mentions is an
// administrator's own mapping and must survive a permission sync untouched.
func TestUnknownUpstreamRouteSurvivesPermissionSync(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "hand-written", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	key := insertProviderKeyForTest(t, a, p, "sk-test", "key", "", providerKeyEgressInherit, nil, 1, 0)
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('hand-written',?,'Vendor/NotConfigured',?,?)`, p, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveProviderModelManagement(context.Background(), p, []providerModelManagementItem{{KeyID: key, ModelPolicy: "fallback", Models: []string{"cline-pass/keep"}}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=? AND public_name='hand-written'`, p).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hand-written route count=%d err=%v", n, err)
	}
}

func modelManagementFixture(t *testing.T) (*App, int64, int64) {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "route-manager", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('old-model',?,'Original-Model',?,?)`, p, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return a, p, id
}
func TestRouteCaseVariantDuplicatesAreRejected(t *testing.T) {
	a, p, id := modelManagementFixture(t)
	post := httptest.NewRecorder()
	a.routes(post, httptest.NewRequest(http.MethodPost, "/api/admin/routes", strings.NewReader(`{"provider_id":`+intString(p)+`,"public_name":"OLD-MODEL","upstream_model":"original-model"}`)), adminCtx{})
	if post.Code != http.StatusConflict {
		t.Fatalf("POST status=%d body=%s", post.Code, post.Body.String())
	}
	res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('old-model',?,'Other',?,?)`, p, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	otherID, _ := res.LastInsertId()
	patch := httptest.NewRecorder()
	a.routeByID(patch, httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(otherID), strings.NewReader(`{"upstream_model":"ORIGINAL-MODEL","priority":9}`)), adminCtx{})
	if patch.Code != http.StatusConflict {
		t.Fatalf("PATCH status=%d body=%s", patch.Code, patch.Body.String())
	}
	var model string
	if err := a.db.QueryRow(`SELECT upstream_model FROM model_routes WHERE id=?`, id).Scan(&model); err != nil || model != "Original-Model" {
		t.Fatalf("original model=%s err=%v", model, err)
	}
	var priority int
	if err := a.db.QueryRow(`SELECT priority FROM model_routes WHERE id=?`, otherID).Scan(&priority); err != nil || priority != 0 {
		t.Fatalf("rollback priority=%d err=%v", priority, err)
	}
}

func TestRouteEditPreservesUpstreamCaseAndKeyPermissions(t *testing.T) {
	a, p, id := modelManagementFixture(t)
	key := insertProviderKeyForTest(t, a, p, "fixture-key", "key", "", providerKeyEgressDirect, nil, 1, 0)
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='original-model' WHERE id=?`, key); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.routeByID(rec, httptest.NewRequest("PATCH", "/api/admin/routes/"+intString(id), strings.NewReader(`{"upstream_model":" Vendor/Code-V2 ","capabilities":"chat,stream,tools"}`)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var upstream, caps, allow string
	if err := a.db.QueryRow(`SELECT upstream_model,capabilities FROM model_routes WHERE id=?`, id).Scan(&upstream, &caps); err != nil {
		t.Fatal(err)
	}
	a.db.QueryRow(`SELECT model_allowlist FROM provider_api_keys WHERE id=?`, key).Scan(&allow)
	if upstream != "Vendor/Code-V2" || caps != "chat,stream,tools" || allow != "original-model" {
		t.Fatalf("upstream=%s caps=%s allow=%s", upstream, caps, allow)
	}
}
func TestRouteEditDuplicateRollsBack(t *testing.T) {
	a, p, id := modelManagementFixture(t)
	a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('old-model',?,'Duplicate',?,?)`, p, now(), now())
	rec := httptest.NewRecorder()
	a.routeByID(rec, httptest.NewRequest("PATCH", "/api/admin/routes/"+intString(id), strings.NewReader(`{"upstream_model":"Duplicate","priority":99}`)), adminCtx{})
	if rec.Code != 409 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var priority int
	a.db.QueryRow(`SELECT priority FROM model_routes WHERE id=?`, id).Scan(&priority)
	if priority != 0 {
		t.Fatalf("priority=%d", priority)
	}
}
func TestModelGroupRenamePreservesPermissionsAndAliases(t *testing.T) {
	a, p, _ := modelManagementFixture(t)
	a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES('old-model',?,'Other-Model',?,?)`, p, now(), now())
	a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('legacy','old-model',1,?,?)`, now(), now())
	for _, v := range []struct {
		name        string
		all         int
		allow, deny string
	}{{"allowed", 0, "old-model,unrelated", ""}, {"denied", 1, "", "old-*"}, {"unrelated", 0, "unrelated", ""}} {
		_, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,created_at) VALUES(?,?,?,?,?,?,?)`, v.name, v.name, v.name, v.all, v.allow, v.deny, now())
		if err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	a.renameModelGroup(rec, httptest.NewRequest("POST", "/api/admin/model-groups/rename", strings.NewReader(`{"old_name":"OLD-MODEL","new_name":"New-Model","keep_old_alias":true}`)), adminCtx{})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var count int
	a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE public_name='new-model'`).Scan(&count)
	if count != 2 {
		t.Fatalf("routes=%d", count)
	}
	var target string
	a.db.QueryRow(`SELECT target_model FROM model_aliases WHERE alias='old-model'`).Scan(&target)
	if target != "new-model" {
		t.Fatal(target)
	}
	for _, name := range []string{"allowed", "denied", "unrelated"} {
		var key authKey
		var all int
		a.db.QueryRow(`SELECT allow_all,allow_models,deny_models FROM api_keys WHERE name=?`, name).Scan(&all, &key.AllowModels, &key.DenyModels)
		key.AllowAll = all != 0
		want := name == "allowed"
		if modelAllowed(key, "new-model", "new-model") != want || modelAllowed(key, "old-model", "new-model") != want || modelAllowed(key, "legacy", "new-model") != want {
			t.Fatalf("permission changed %s %#v", name, key)
		}
		if name == "allowed" && !modelAllowed(key, "unrelated", "unrelated") {
			t.Fatal("unrelated permission lost")
		}
	}
}
func TestModelGroupRenameConflictsAreAtomic(t *testing.T) {
	for _, body := range []string{`{"old_name":"old-model","new_name":"taken","keep_old_alias":true}`, `{"old_name":"old-model","new_name":"new-model","keep_old_alias":true}`} {
		t.Run(body, func(t *testing.T) {
			a, _, _ := modelManagementFixture(t)
			a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('taken','old-model',1,?,?)`, now(), now())
			if strings.Contains(body, "new-model") {
				a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,created_at) VALUES('restricted','p','h',0,'old-model','new-*',?)`, now())
			}
			rec := httptest.NewRecorder()
			a.renameModelGroup(rec, httptest.NewRequest("POST", "/api/admin/model-groups/rename", strings.NewReader(body)), adminCtx{})
			if rec.Code != 409 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			var count int
			a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE public_name='old-model'`).Scan(&count)
			if count != 1 {
				t.Fatalf("routes changed after conflict=%d", count)
			}
		})
	}
}
func TestRouteModelPreviewCountsPolicyNotInventory(t *testing.T) {
	a, p, _ := modelManagementFixture(t)
	key := insertProviderKeyForTest(t, a, p, "fixture-key", "key", "", providerKeyEgressDirect, nil, 1, 0)
	a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist='allowed' WHERE id=?`, key)
	for _, model := range []string{"allowed", "not-allowed"} {
		rec := httptest.NewRecorder()
		a.routeModelPreview(rec, httptest.NewRequest(http.MethodPost, "/api/admin/routes/preview", strings.NewReader(`{"provider_id":`+intString(p)+`,"upstream_model":"`+model+`"}`)), adminCtx{})
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var result routeModelSupport
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want := 0
		if model == "allowed" {
			want = 1
		}
		if result.SupportedKeyCount != want || result.AvailableKeyCount != want {
			t.Fatalf("%s %+v", model, result)
		}
	}
}
