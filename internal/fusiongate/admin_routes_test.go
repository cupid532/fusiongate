package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteMappingMergesPublicFailoverGroup(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerOne := insertTestProvider(t, a, "grok-primary", "openai_compatible", "https://one.example/v1", "secret-one", 100, 100, "normalized", "any", 0, 3, 30)
	providerTwo := insertTestProvider(t, a, "grok-backup", "openai_compatible", "https://two.example/v1", "secret-two", 90, 100, "normalized", "any", 0, 3, 30)
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "grok-4.2-mult-xhigh", providerOne, "grok-4.2-mult-xhigh", "chat,stream", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	routeID, _ := res.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "grok-4.2", providerTwo, "grok-4.2-fast", "chat,stream", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?)`, "grok-4.2-mult-xhigh", StrategyPriorityFailover, stamp); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(routeID), strings.NewReader(`{"public_name":"GROK-4.2"}`))
	a.routeByID(recorder, req, adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		PublicName  string `json:"public_name"`
		GroupRoutes int    `json:"group_routes"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.PublicName != "grok-4.2" || response.GroupRoutes != 2 {
		t.Fatalf("response = %#v", response)
	}
	var publicName, upstreamModel string
	if err := a.db.QueryRow(`SELECT public_name,upstream_model FROM model_routes WHERE id=?`, routeID).Scan(&publicName, &upstreamModel); err != nil {
		t.Fatal(err)
	}
	if publicName != "grok-4.2" || upstreamModel != "grok-4.2-mult-xhigh" {
		t.Fatalf("mapped route = %q -> %q", publicName, upstreamModel)
	}
	resolved, err := a.resolve(context.Background(), "grok-4.2", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved routes = %d, want both mappings in one failover group", len(resolved))
	}
	if _, err := a.resolve(context.Background(), "grok-4.2-mult-xhigh", "chat"); err == nil {
		t.Fatal("old public model still resolves after mapping")
	}
	var oldPolicies int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM route_policies WHERE public_name='grok-4.2-mult-xhigh'`).Scan(&oldPolicies); err != nil {
		t.Fatal(err)
	}
	if oldPolicies != 0 {
		t.Fatalf("old policy remains: %d", oldPolicies)
	}
}

func TestRouteMappingRejectsExactDuplicateInTargetGroup(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "duplicate", "openai_compatible", "https://example.invalid/v1", "secret", 100, 100, "normalized", "any", 0, 3, 30)
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "long-name", providerID, "same-upstream", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	routeID, _ := res.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "unified", providerID, "same-upstream", stamp, stamp); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(routeID), strings.NewReader(`{"public_name":"unified"}`))
	a.routeByID(recorder, req, adminCtx{})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var publicName string
	if err := a.db.QueryRow(`SELECT public_name FROM model_routes WHERE id=?`, routeID).Scan(&publicName); err != nil {
		t.Fatal(err)
	}
	if publicName != "long-name" {
		t.Fatalf("conflicting route changed to %q", publicName)
	}
}

func TestReorderRoutesPersistsAndListsConfiguredPosition(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "ordered", "openai_compatible", "https://example.invalid/v1", "secret", 100, 100, "normalized", "any", 0, 3, 30)
	stamp := now()
	first, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,sort_order,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "model", providerID, "first", 0, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,sort_order,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "model", providerID, "second", 1, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	firstID, _ := first.LastInsertId()
	secondID, _ := second.LastInsertId()

	recorder := httptest.NewRecorder()
	body := `{"public_name":"MODEL","route_ids":[` + intString(secondID) + `,` + intString(firstID) + `]}`
	a.reorderRoutes(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/routes/reorder", strings.NewReader(body)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	list := httptest.NewRecorder()
	a.routes(list, httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil), adminCtx{})
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var routes []Route
	if err := json.Unmarshal(list.Body.Bytes(), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].ID != secondID || routes[0].SortOrder != 0 || routes[1].ID != firstID || routes[1].SortOrder != 1 {
		t.Fatalf("routes = %#v, want IDs [%d %d] with sequential positions", routes, secondID, firstID)
	}
}
