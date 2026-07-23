package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		Route:    Route{ID: id, ProviderID: providerID, PublicName: model, SortOrder: order},
		Provider: Provider{ID: providerID, Priority: priority, Weight: 100, FailureThreshold: 3, CooldownSeconds: 30},
	}
}

func TestPriorityFailoverUsesProviderPriorityThenCreationOrder(t *testing.T) {
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

func TestSmartRoundRobinRotatesByRequestAndKeepsFailoverOrder(t *testing.T) {
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
		plan := a.prepareRoutes(routes, StrategySmartRoundRobin)
		z, _, ok := a.acquireRoute(plan, map[int64]bool{}, StrategySmartRoundRobin)
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

	plan := a.prepareRoutes(routes, StrategySmartRoundRobin)
	first, _, _ := a.acquireRoute(plan, map[int64]bool{}, StrategySmartRoundRobin)
	releaseSelectedRoute(a, first)
	second, _, ok := a.acquireRoute(plan, map[int64]bool{first.Route.ID: true}, StrategySmartRoundRobin)
	if !ok {
		t.Fatal("expected failover candidate")
	}
	releaseSelectedRoute(a, second)
	if first.Route.ID != 2 || second.Route.ID != 3 {
		t.Fatalf("request-local failover = %d -> %d, want 2 -> 3", first.Route.ID, second.Route.ID)
	}
}

func TestOrderedRoundRobinAlwaysStartsFromFirstAndFailsOverInOrder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{
		schedulerRoute(3, 3, "model", 1, 2),
		schedulerRoute(1, 1, "model", 1, 0),
		schedulerRoute(2, 2, "model", 1, 1),
	}
	for range 3 {
		plan := a.prepareRoutes(routes, StrategyOrderedRoundRobin)
		first, _, ok := a.acquireRoute(plan, map[int64]bool{}, StrategyOrderedRoundRobin)
		if !ok || first.Route.ID != 1 {
			t.Fatalf("ordered first=%d ok=%v, want route 1", first.Route.ID, ok)
		}
		releaseSelectedRoute(a, first)
		second, _, ok := a.acquireRoute(plan, map[int64]bool{first.Route.ID: true}, StrategyOrderedRoundRobin)
		if !ok || second.Route.ID != 2 {
			t.Fatalf("ordered second=%d ok=%v, want route 2", second.Route.ID, ok)
		}
		releaseSelectedRoute(a, second)
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

func TestLegacyRoutePauseRemainsBackwardCompatible(t *testing.T) {
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
