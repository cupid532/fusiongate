package fusiongate

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func fleetRoutes(providers, keysPerProvider int) []resolvedRoute {
	routes := make([]resolvedRoute, 0, providers*keysPerProvider)
	routeID := int64(0)
	for p := 1; p <= providers; p++ {
		for k := 0; k < keysPerProvider; k++ {
			routeID++
			z := resolvedRoute{
				Route:    Route{ID: routeID, ProviderID: int64(p), PublicName: "fleet-model", SortOrder: int(routeID)},
				Provider: Provider{ID: int64(p), Name: fmt.Sprintf("provider-%02d", p), Weight: 100, FailureThreshold: 3, CooldownSeconds: 30, SortOrder: p},
			}
			z.ProviderKeyID = int64(p*1000 + k)
			routes = append(routes, z)
		}
	}
	return routes
}

// Forty providers, several keys each, hammered concurrently. Every runtime invariant
// that scheduling depends on has to survive: in-flight counts return to zero, nothing
// goes negative, and no provider is starved or hoarded under adaptive selection.
func TestAdaptiveSchedulingAtFleetScale(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := fleetRoutes(40, 3)

	const workers = 32
	const perWorker = 150
	counts := make([]map[int64]int, workers)
	errors := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		counts[w] = map[int64]int{}
		wg.Add(1)
		go func(mine map[int64]int, seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < perWorker; i++ {
				z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
				if !ok {
					errors <- fmt.Errorf("worker %d found no route under fleet load", seed)
					return
				}
				mine[z.Provider.ID]++
				if rng.Intn(64) == 0 {
					time.Sleep(time.Microsecond)
				}
				releaseSelectedRoute(a, z)
			}
		}(counts[w], int64(w))
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	total := map[int64]int{}
	for _, m := range counts {
		for id, n := range m {
			total[id] += n
		}
	}
	requests := workers * perWorker
	if len(total) != 40 {
		t.Fatalf("only %d of 40 providers ever selected", len(total))
	}
	expected := requests / 40
	for id, n := range total {
		if n < expected/3 || n > expected*3 {
			t.Fatalf("provider %d took %d of %d requests (expected ~%d) — distribution collapsed", id, n, requests, expected)
		}
	}
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	for id, state := range a.providerStates {
		if state.Inflight != 0 {
			t.Fatalf("provider %d leaked in-flight count %d after all requests released", id, state.Inflight)
		}
		if state.Inflight < 0 {
			t.Fatalf("provider %d has negative in-flight count", id)
		}
	}
}

// Priority mode with a deep pile of broken providers: the failover cap bounds one
// request's attempts, but circuit breakers must keep the pile from being retried
// forever, so requests reach the healthy provider once the breakers have opened.
func TestPriorityFailoverClimbsOverBrokenFleet(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const broken = 30
	routes := make([]resolvedRoute, 0, broken+1)
	for p := 1; p <= broken; p++ {
		routes = append(routes, resolvedRoute{
			Route:    Route{ID: int64(p), ProviderID: int64(p), PublicName: "m", SortOrder: p},
			Provider: Provider{ID: int64(p), Priority: 100, SortOrder: p, Weight: 100, FailureThreshold: 3, CooldownSeconds: 300},
		})
	}
	healthy := resolvedRoute{
		Route:    Route{ID: 999, ProviderID: 999, PublicName: "m", SortOrder: 999},
		Provider: Provider{ID: 999, Priority: 1, SortOrder: 999, Weight: 100, FailureThreshold: 3, CooldownSeconds: 300},
	}
	routes = append(routes, healthy)
	plan := a.prepareRoutes(routes, StrategyPriorityFailover)

	// Simulate the retry storm: every attempt at a broken provider fails.
	reachedHealthy := 0
	for request := 0; request < 60; request++ {
		tried := map[int64]bool{}
		for attempt := 0; attempt < 8; attempt++ {
			z, _, ok := a.acquireRoute(plan, tried, StrategyPriorityFailover)
			if !ok {
				break
			}
			attemptID := z.AttemptID
			if attemptID == 0 {
				attemptID = z.Route.ID
			}
			tried[attemptID] = true
			if z.Provider.ID == 999 {
				reachedHealthy++
				a.completeRoute(z, attemptResult{Status: 200, Handled: true}, 50*time.Millisecond)
				break
			}
			a.completeRoute(z, attemptResult{Status: 502, Retryable: true, Reason: "upstream_server_error"}, 5*time.Millisecond)
		}
	}
	if reachedHealthy == 0 {
		t.Fatal("no request ever reached the healthy provider behind 30 broken ones")
	}
	// After breakers open, the healthy provider must serve essentially every request.
	late := 0
	for request := 0; request < 20; request++ {
		tried := map[int64]bool{}
		z, _, ok := a.acquireRoute(plan, tried, StrategyPriorityFailover)
		if !ok {
			t.Fatal("no route after breakers opened")
		}
		if z.Provider.ID == 999 {
			late++
		}
		a.completeRoute(z, attemptResult{Status: 200, Handled: true}, 50*time.Millisecond)
	}
	if late < 18 {
		t.Fatalf("healthy provider served only %d of 20 requests after the broken fleet's breakers opened", late)
	}
}

// The per-model rotation state must stay bounded when many models come and go.
func TestRotationStateStaysBoundedAcrossManyModels(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for m := 0; m < 200; m++ {
		model := fmt.Sprintf("model-%03d", m)
		routes := []resolvedRoute{
			{Route: Route{ID: int64(m*2 + 1), ProviderID: 1, PublicName: model}, Provider: Provider{ID: 1, Weight: 100}},
			{Route: Route{ID: int64(m*2 + 2), ProviderID: 2, PublicName: model}, Provider: Provider{ID: 2, Weight: 100}},
		}
		z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("expected route")
		}
		releaseSelectedRoute(a, z)
	}
	a.routeMu.Lock()
	models := len(a.smoothWeights)
	a.routeMu.Unlock()
	if models != 200 {
		t.Fatalf("smooth weight models = %d, want 200", models)
	}
	a.routeMu.Lock()
	for m := 0; m < 200; m++ {
		a.forgetRouteCursorsLocked(fmt.Sprintf("model-%03d", m))
	}
	remaining := len(a.smoothWeights)
	a.routeMu.Unlock()
	if remaining != 0 {
		t.Fatalf("rotation state leaked %d models after cleanup", remaining)
	}
}
