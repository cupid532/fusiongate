package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelAliasRoutesToCanonicalFailoverGroup(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		upstreamModel, _ = body["model"].(string)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-alias", "model": upstreamModel,
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "alias-upstream", "openai_compatible", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "glm5-2", "vendor-glm-5.2", "chat,stream", 1)
	if _, err := a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('/glm5.2','glm5-2',1,?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"/glm5.2","messages":[{"role":"user","content":"ping"}]}`, "opencode/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "vendor-glm-5.2" {
		t.Fatalf("upstream model=%q", upstreamModel)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response["model"] != "/glm5.2" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	a.flushLedgerWrites()
	var publicModel string
	if err := a.db.QueryRow(`SELECT public_model FROM request_ledger ORDER BY id DESC LIMIT 1`).Scan(&publicModel); err != nil {
		t.Fatal(err)
	}
	if publicModel != "/glm5.2" {
		t.Fatalf("ledger public model=%q", publicModel)
	}
}

func TestModelAliasPermissionsApplyToAliasAndCanonicalName(t *testing.T) {
	key := authKey{AllowModels: "/glm5.2", DenyModels: "glm5-2"}
	if modelAllowed(key, "/glm5.2", "glm5-2") {
		t.Fatal("canonical deny must block an allowed alias")
	}
	key = authKey{AllowModels: "glm5-2"}
	if !modelAllowed(key, "/glm5.2", "glm5-2") {
		t.Fatal("canonical allow should authorize its alias")
	}
}

func TestModelAliasAdminRejectsAliasChainsAndRouteConflicts(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "alias-admin", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "canonical", "upstream", "chat", 1)

	create := httptest.NewRecorder()
	a.modelAliases(create, httptest.NewRequest(http.MethodPost, "/api/admin/model-aliases", strings.NewReader(`{"alias":"shortcut","target_model":"canonical"}`)), adminCtx{})
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	chain := httptest.NewRecorder()
	a.modelAliases(chain, httptest.NewRequest(http.MethodPost, "/api/admin/model-aliases", strings.NewReader(`{"alias":"second","target_model":"shortcut"}`)), adminCtx{})
	if chain.Code != http.StatusBadRequest {
		t.Fatalf("chain status=%d body=%s", chain.Code, chain.Body.String())
	}
	conflict := httptest.NewRecorder()
	a.modelAliases(conflict, httptest.NewRequest(http.MethodPost, "/api/admin/model-aliases", strings.NewReader(`{"alias":"canonical","target_model":"canonical"}`)), adminCtx{})
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestModelAliasSkipsTransparentRouteThatCannotRewriteModel(t *testing.T) {
	routes := []resolvedRoute{
		{Route: Route{PublicName: "canonical", UpstreamModel: "canonical"}, Provider: Provider{PassthroughMode: "transparent"}},
		{Route: Route{PublicName: "canonical", UpstreamModel: "vendor-model"}, Provider: Provider{PassthroughMode: "normalized"}},
	}
	got := exposeRequestedModel(routes, "alias")
	if len(got) != 1 || got[0].Route.PublicName != "alias" || got[0].Route.UpstreamModel != "vendor-model" {
		t.Fatalf("routes=%#v", got)
	}
}

func TestModelAliasRejectsTransparentOnlyTarget(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "transparent-alias", "openai_compatible", "https://example.test", "secret", 1, 100, "transparent", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "canonical", "canonical", "chat", 1)
	rec := httptest.NewRecorder()
	a.modelAliases(rec, httptest.NewRequest(http.MethodPost, "/api/admin/model-aliases", strings.NewReader(`{"alias":"shortcut","target_model":"canonical"}`)), adminCtx{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "safely rewrite") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRouteCreationRejectsExistingAliasName(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "alias-route-conflict", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "canonical", "upstream", "chat", 1)
	if _, err := a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('shortcut','canonical',1,?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	body := `{"provider_id":` + intString(providerID) + `,"public_name":"shortcut","upstream_model":"other"}`
	a.routes(rec, httptest.NewRequest(http.MethodPost, "/api/admin/routes", strings.NewReader(body)), adminCtx{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRouteRenameMovesAliasesWithLastRoute(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "alias-rename", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	routeID := insertTestRoute(t, a, providerID, "old-model", "upstream", "chat", 1)
	if _, err := a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('shortcut','old-model',1,?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.routeByID(rec, httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(routeID), strings.NewReader(`{"public_name":"new-model"}`)), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var target string
	if err := a.db.QueryRow(`SELECT target_model FROM model_aliases WHERE alias='shortcut'`).Scan(&target); err != nil || target != "new-model" {
		t.Fatalf("target=%q err=%v", target, err)
	}
}

func TestProviderDeletionRemovesDanglingAliases(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "alias-delete", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "canonical", "upstream", "chat", 1)
	if _, err := a.db.Exec(`INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES('shortcut','canonical',1,?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.providerDelete(rec, providerID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var aliases int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_aliases WHERE alias='shortcut'`).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatalf("aliases=%d err=%v", aliases, err)
	}
}
