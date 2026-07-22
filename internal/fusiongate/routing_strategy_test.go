package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func releaseSelectedRoute(a *App, z resolvedRoute) {
	a.routeMu.Lock()
	if state := a.providerStates[z.Provider.ID]; state != nil && state.Inflight > 0 {
		state.Inflight--
		state.HalfOpenProbe = false
	}
	a.routeMu.Unlock()
}

func schedulerRoute(id, providerID int64, model string, priority, order int) resolvedRoute {
	return resolvedRoute{
		Route:    Route{ID: id, ProviderID: providerID, PublicName: model, Priority: priority, SortOrder: order},
		Provider: Provider{ID: providerID, Weight: 100, FailureThreshold: 3, CooldownSeconds: 30},
	}
}

func TestPriorityFailoverUsesDescendingPriorityThenDragOrder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{
		schedulerRoute(3, 3, "model", 4, 0),
		schedulerRoute(2, 2, "model", 5, 1),
		schedulerRoute(1, 1, "model", 5, 0),
	}
	plan := a.prepareRoutes(routes, StrategyPriorityFailover)
	tried := map[int64]bool{}
	var got []int64
	for range 3 {
		z, _, ok := a.acquireRoute(plan, tried, StrategyPriorityFailover)
		if !ok {
			t.Fatal("expected route")
		}
		got = append(got, z.Route.ID)
		tried[z.Route.ID] = true
		releaseSelectedRoute(a, z)
	}
	want := []int64{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority failover order = %v, want %v", got, want)
		}
	}
}

func TestOrderedRoundRobinRotatesByRequestAndKeepsFailoverOrder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{
		schedulerRoute(1, 1, "model", 99, 0),
		schedulerRoute(2, 2, "model", 1, 1),
		schedulerRoute(3, 3, "model", 50, 2),
	}
	var starts []int64
	for range 4 {
		plan := a.prepareRoutes(routes, StrategyOrderedRoundRobin)
		z, _, ok := a.acquireRoute(plan, map[int64]bool{}, StrategyOrderedRoundRobin)
		if !ok {
			t.Fatal("expected route")
		}
		starts = append(starts, z.Route.ID)
		releaseSelectedRoute(a, z)
	}
	want := []int64{1, 2, 3, 1}
	for i := range want {
		if starts[i] != want[i] {
			t.Fatalf("round robin starts = %v", starts)
		}
	}

	plan := a.prepareRoutes(routes, StrategyOrderedRoundRobin)
	first, _, _ := a.acquireRoute(plan, map[int64]bool{}, StrategyOrderedRoundRobin)
	releaseSelectedRoute(a, first)
	second, _, ok := a.acquireRoute(plan, map[int64]bool{first.Route.ID: true}, StrategyOrderedRoundRobin)
	if !ok {
		t.Fatal("expected failover candidate")
	}
	releaseSelectedRoute(a, second)
	if first.Route.ID != 2 || second.Route.ID != 3 {
		t.Fatalf("request-local failover = %d -> %d, want 2 -> 3", first.Route.ID, second.Route.ID)
	}
}

func TestAdaptivePrefersStableLowLatencyRoute(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{schedulerRoute(1, 1, "model", 0, 0), schedulerRoute(2, 2, "model", 0, 1)}
	a.routeMu.Lock()
	a.providerStates[1] = &providerRuntime{EWMALatencyMS: 80}
	a.providerStates[2] = &providerRuntime{EWMALatencyMS: 3000, ConsecutiveFailures: 2}
	a.routeMu.Unlock()
	counts := map[int64]int{}
	for range 300 {
		z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("expected adaptive route")
		}
		counts[z.Route.ID]++
		releaseSelectedRoute(a, z)
	}
	if counts[1] < counts[2]*8 {
		t.Fatalf("adaptive selection was not decisive enough: %v", counts)
	}
}

func TestRoutePausePriorityZeroPolicyAndReorderAPI(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "one", "openai_compatible", "http://one.test", "one", 1, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "two", "openai_compatible", "http://two.test", "two", 1, 1, "normalized", "any", 0, 3, 30)
	r1 := insertTestRoute(t, a, p1, "model", "one", "chat", 5)
	r2 := insertTestRoute(t, a, p2, "model", "two", "chat", 4)
	if _, err := a.db.Exec(`UPDATE model_routes SET sort_order=id`); err != nil {
		t.Fatal(err)
	}

	patch := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(r1), strings.NewReader(`{"enabled":false,"priority":0}`))
	rec := httptest.NewRecorder()
	a.routeByID(rec, patch, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var enabled, priority int
	if err := a.db.QueryRow(`SELECT enabled,priority FROM model_routes WHERE id=?`, r1).Scan(&enabled, &priority); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || priority != 0 {
		t.Fatalf("enabled=%d priority=%d", enabled, priority)
	}
	resolved, err := a.resolve(context.Background(), "model", "chat")
	if err != nil || len(resolved) != 1 || resolved[0].Route.ID != r2 {
		t.Fatalf("resolved after pause = %+v, %v", resolved, err)
	}

	policy := httptest.NewRequest(http.MethodPatch, "/api/admin/route-policies", strings.NewReader(`{"public_name":"model","strategy":"ordered_round_robin"}`))
	rec = httptest.NewRecorder()
	a.routePolicies(rec, policy, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("policy status=%d body=%s", rec.Code, rec.Body.String())
	}
	resolved, err = a.resolve(context.Background(), "model", "chat")
	if err != nil || routeStrategy(resolved) != StrategyOrderedRoundRobin {
		t.Fatalf("resolved strategy = %q, %v", routeStrategy(resolved), err)
	}

	orderBody := `{"public_name":"model","route_ids":[` + intString(r2) + `,` + intString(r1) + `]}`
	reorder := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/reorder", strings.NewReader(orderBody))
	rec = httptest.NewRecorder()
	a.reorderRoutes(rec, reorder, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rec.Code, rec.Body.String())
	}
	var firstID int64
	if err := a.db.QueryRow(`SELECT id FROM model_routes WHERE public_name='model' ORDER BY sort_order LIMIT 1`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if firstID != r2 {
		t.Fatalf("first route=%d want %d", firstID, r2)
	}

	invalid := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/reorder", strings.NewReader(`{"public_name":"model","route_ids":[`+intString(r1)+`,`+intString(r1)+`]}`))
	rec = httptest.NewRecorder()
	a.reorderRoutes(rec, invalid, adminCtx{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate reorder status=%d", rec.Code)
	}

	resume := httptest.NewRequest(http.MethodPatch, "/api/admin/routes/"+intString(r1), strings.NewReader(`{"enabled":true}`))
	rec = httptest.NewRecorder()
	a.routeByID(rec, resume, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status=%d", rec.Code)
	}
	resolved, err = a.resolve(context.Background(), "model", "chat")
	if err != nil || len(resolved) != 2 {
		t.Fatalf("resolved after resume = %d, %v", len(resolved), err)
	}
}

func TestInvalidRoutingStrategyRejected(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p := insertTestProvider(t, a, "one", "openai_compatible", "http://one.test", "one", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p, "model", "one", "chat", 0)
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/route-policies", strings.NewReader(`{"public_name":"model","strategy":"random"}`))
	rec := httptest.NewRecorder()
	a.routePolicies(rec, req, adminCtx{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoundRobinCursorConcurrentAccess(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{schedulerRoute(1, 1, "model", 0, 0), schedulerRoute(2, 2, "model", 0, 1)}
	done := make(chan struct{}, 20)
	for range 20 {
		go func() {
			for range 50 {
				_ = a.prepareRoutes(routes, StrategyOrderedRoundRobin)
			}
			done <- struct{}{}
		}()
	}
	for range 20 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("round robin cursor deadlocked")
		}
	}
}
func TestModelsHideRoutesWhoseProviderIsDisabled(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "enabled", "openai_compatible", "http://enabled.test", "one", 1, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "disabled", "openai_compatible", "http://disabled.test", "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "available-model", "available-model", "chat", 0)
	insertTestRoute(t, a, p2, "hidden-model", "hidden-model", "chat", 0)
	if _, err := a.db.Exec(`UPDATE providers SET enabled=0 WHERE id=?`, p2); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	a.models(rec, req, authKey{AllowAll: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "available-model") || strings.Contains(body, "hidden-model") {
		t.Fatalf("models response = %s", body)
	}
}
