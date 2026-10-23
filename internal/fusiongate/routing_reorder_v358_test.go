package fusiongate

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// v358PriorityRoutes gives every provider a distinct current priority. The
// supplied order is deliberately independent of the task's historical order.
func v358PriorityRoutes(providerIDs ...int64) []resolvedRoute {
	routes := make([]resolvedRoute, 0, len(providerIDs))
	for index, id := range providerIDs {
		routes = append(routes, resolvedRoute{
			Route: Route{
				ID:            id,
				ProviderID:    id,
				PublicName:    "routing-reorder-model",
				UpstreamModel: "routing-reorder-model",
			},
			Provider: Provider{
				ID:       id,
				Type:     "openai_compatible",
				Priority: len(providerIDs) - index,
			},
			ProviderKeyID: id*100 + 1,
		})
	}
	return routes
}

func v358HistoricalSession(order []int64, bound int64) routingSession {
	return routingSession{
		TaskHash:      "routing-reorder-v358-task",
		TaskScope:     inferenceTaskScopeFor("routing-reorder-model", "openai_chat"),
		TaskSource:    "explicit",
		Order:         append([]int64(nil), order...),
		Cursor:        slices.Index(order, bound),
		ProviderID:    bound,
		ProviderKeyID: bound*100 + 1,
		Generation:    7,
	}
}

func v358ReachableProviders(t *testing.T, plan inferencePlan) []int64 {
	t.Helper()
	if plan.Start < 0 || plan.Start > len(plan.Channels) {
		t.Fatalf("invalid plan.Start=%d for %d channels", plan.Start, len(plan.Channels))
	}
	ids := make([]int64, 0, len(plan.Channels)-plan.Start)
	for _, channel := range plan.Channels[plan.Start:] {
		ids = append(ids, channel.ProviderID)
	}
	return ids
}

func v358AssertReachable(t *testing.T, plan inferencePlan, want []int64) {
	t.Helper()
	if got := v358ReachableProviders(t, plan); !slices.Equal(got, want) {
		t.Fatalf("reachable providers=%v, want %v (Start=%d, full plan=%v, Resumed=%v, Exhausted=%v)",
			got, want, plan.Start, storedOrderFromChannels(plan.Channels), plan.Resumed, plan.Exhausted)
	}
}

// A historical prefix names spent providers, not positions in the new priority
// order. A bound provider still present must lead; every other unvisited or new
// provider remains reachable after it, in current priority order.
func TestV358RoutingReorderReachableProviders(t *testing.T) {
	const a, b, c, d, e int64 = 11, 22, 33, 44, 55
	session := v358HistoricalSession([]int64{a, b, c}, b)
	for _, tc := range []struct {
		name      string
		live      []int64
		want      []int64
		exhausted bool
	}{
		{name: "unchanged", live: []int64{a, b, c}, want: []int64{b, c}},
		{name: "unvisited_promoted_above_spent_and_bound", live: []int64{c, a, b}, want: []int64{b, c}},
		{name: "spent_demoted_below_bound", live: []int64{c, b, a}, want: []int64{b, c}},
		{name: "bound_promoted_to_first", live: []int64{b, c, a}, want: []int64{b, c}},
		{name: "unvisited_promoted_just_above_bound", live: []int64{a, c, b}, want: []int64{b, c}},
		{name: "new_high_priority_channel", live: []int64{d, c, a, b}, want: []int64{b, d, c}},
		{name: "new_channels_above_and_below_bound", live: []int64{d, a, c, b, e}, want: []int64{b, d, c, e}},
		{name: "spent_temporarily_absent", live: []int64{c, b}, want: []int64{b, c}},
		{name: "bound_removed_unvisited_above_spent", live: []int64{c, a}, want: []int64{c}},
		{name: "bound_removed_with_new_channel", live: []int64{d, a, c}, want: []int64{d, c}},
		{name: "bound_and_spent_removed", live: []int64{c, d}, want: []int64{c, d}},
		{name: "bound_removed_only_new_channel", live: []int64{d}, want: []int64{d}},
		{name: "only_spent_candidates", live: []int64{a}, exhausted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildInferencePlan(v358PriorityRoutes(tc.live...), "/v1/chat/completions", session, true)
			v358AssertReachable(t, plan, tc.want)
			if plan.Exhausted != tc.exhausted {
				t.Errorf("Exhausted=%v, want %v", plan.Exhausted, tc.exhausted)
			}
			if !tc.exhausted && !plan.Resumed {
				t.Error("existing task binding must be reported as resumed even when its live prefix is empty")
			}
		})
	}
}

func TestV358RoutingReorderAllHistoricalSpentProvidersStayExcluded(t *testing.T) {
	const a, b, c, d int64 = 11, 22, 33, 44
	session := v358HistoricalSession([]int64{a, b, c, d}, c)
	for _, tc := range []struct {
		name      string
		live      []int64
		want      []int64
		exhausted bool
	}{
		{name: "both_spent_demoted", live: []int64{d, c, b, a}, want: []int64{c, d}},
		{name: "one_spent_missing", live: []int64{d, c, a}, want: []int64{c, d}},
		{name: "bound_missing", live: []int64{d, b, a}, want: []int64{d}},
		{name: "only_reordered_spent_remain", live: []int64{b, a}, exhausted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildInferencePlan(v358PriorityRoutes(tc.live...), "/v1/chat/completions", session, true)
			v358AssertReachable(t, plan, tc.want)
			if plan.Exhausted != tc.exhausted {
				t.Errorf("Exhausted=%v, want %v", plan.Exhausted, tc.exhausted)
			}
		})
	}
}

func TestV358RoutingReorderFreshTasksKeepCurrentPriority(t *testing.T) {
	const a, b, c, d int64 = 11, 22, 33, 44
	oldTask := v358HistoricalSession([]int64{a, b, c}, b)
	for _, live := range [][]int64{{c, a, b}, {d, c, b, a}, {a}, {}} {
		// A caller with no loaded task session must not inherit any of the old
		// task's exclusions or binding, even on an otherwise identical request.
		plan := buildInferencePlan(v358PriorityRoutes(live...), "/v1/chat/completions", oldTask, false)
		v358AssertReachable(t, plan, live)
		if plan.Resumed || plan.Exhausted {
			t.Errorf("fresh task has Resumed=%v Exhausted=%v for live order %v", plan.Resumed, plan.Exhausted, live)
		}
	}
}

func TestV358RoutingReorderKeepsBoundCredentialPreference(t *testing.T) {
	const a, b, c int64 = 11, 22, 33
	session := v358HistoricalSession([]int64{a, b, c}, b)
	routes := v358PriorityRoutes(c, a, b)
	preferred := routes[len(routes)-1]
	preferred.ProviderKeyID++
	routes = append(routes, preferred)
	session.ProviderKeyID = preferred.ProviderKeyID

	plan := buildInferencePlan(routes, "/v1/chat/completions", session, true)
	v358AssertReachable(t, plan, []int64{b, c})
	if !plan.Resumed {
		t.Error("bound task was not marked resumed")
	}
	// The executor applies credential stickiness within the chosen channel;
	// do not require buildInferencePlan itself to reorder a channel's keys.
	keys := preferSessionKey(plan.Channels[plan.Start].Routes, session.ProviderKeyID)
	if len(keys) != 2 || keys[0].ProviderKeyID != preferred.ProviderKeyID || keys[1].ProviderKeyID != b*100+1 {
		t.Fatalf("bound provider lost its preferred key or fallback key: %+v", keys)
	}
	for _, key := range keys {
		if key.Provider.ID != b {
			t.Fatalf("key preference crossed provider boundary: provider=%d, want %d", key.Provider.ID, b)
		}
	}
}

// Exercise the actual persistence path without assuming the routing_sessions
// table layout: bind/load a real database, then use the router's production
// advance/noteSuccess hooks. Saving only the live plan silently forgets absent
// spent identities and lets them return on a later configuration change.
func TestV358RoutingReorderPersistsDepartedIdentityHistory(t *testing.T) {
	const a, b, c, d int64 = 11, 22, 33, 44
	for _, tc := range []struct {
		name     string
		stored   []int64
		bound    int64
		live     []int64
		selected int64
		advance  bool
		readded  []int64
		want     []int64
	}{
		{
			name:   "success_keeps_absent_spent_provider",
			stored: []int64{a, b, c}, bound: b,
			live: []int64{b, c}, selected: b,
			readded: []int64{b, a, c}, want: []int64{b, c},
		},
		{
			name:   "advance_keeps_absent_spent_provider",
			stored: []int64{a, b, c, d}, bound: b,
			live: []int64{b, c, d}, selected: c, advance: true,
			readded: []int64{b, c, a, d}, want: []int64{c, d},
		},
		{
			name:   "success_keeps_absent_bound_provider_spent_after_departure",
			stored: []int64{a, b, c, d}, bound: b,
			live: []int64{c, d}, selected: c,
			readded: []int64{c, a, b, d}, want: []int64{c, d},
		},
		{
			name:   "absent_unvisited_provider_is_not_spent",
			stored: []int64{a, b, c, d}, bound: b,
			live: []int64{b, d}, selected: b,
			readded: []int64{c, b, a, d}, want: []int64{b, c, d},
		},
		{
			name:   "advance_keeps_multiple_absent_spent_providers",
			stored: []int64{a, b, c, d}, bound: c,
			live: []int64{c, d}, selected: d, advance: true,
			readded: []int64{d, a, b, c}, want: []int64{d},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			session := v358HistoricalSession(tc.stored, tc.bound)
			if err := app.bindRoutingSession(request.Context(), session); err != nil {
				t.Fatal(err)
			}
			session, exists := app.loadRoutingSession(request.Context(), session.TaskHash, session.TaskScope)
			if !exists {
				t.Fatal("historical task binding did not round-trip through database")
			}
			run := inferenceRun{
				app: app, model: "routing-reorder-model", protocol: "openai_chat",
				taskHash: session.TaskHash, taskScope: session.TaskScope, taskSource: session.TaskSource,
				session: session, hasSession: true,
				plan: buildInferencePlan(v358PriorityRoutes(tc.live...), request.URL.Path, session, true),
			}
			selectedIndex := -1
			for index := run.plan.Start; index < len(run.plan.Channels); index++ {
				if run.plan.Channels[index].ProviderID == tc.selected {
					selectedIndex = index
					break
				}
			}
			if selectedIndex < 0 {
				t.Fatalf("selected provider %d is not reachable in initial plan %v", tc.selected, v358ReachableProviders(t, run.plan))
			}
			selectedRoute := run.plan.Channels[selectedIndex].Routes[0]
			if tc.advance {
				run.advance(request, selectedIndex)
			} else {
				run.noteSuccess(request, selectedRoute, selectedIndex)
			}
			saved, exists := app.loadRoutingSession(request.Context(), session.TaskHash, session.TaskScope)
			if !exists || saved.ProviderID != tc.selected {
				t.Fatalf("saved binding exists=%v provider=%d, want %d", exists, saved.ProviderID, tc.selected)
			}
			if !tc.advance && saved.ProviderKeyID != selectedRoute.ProviderKeyID {
				t.Errorf("success saved key=%d, want %d", saved.ProviderKeyID, selectedRoute.ProviderKeyID)
			}

			resumed := buildInferencePlan(v358PriorityRoutes(tc.readded...), request.URL.Path, saved, true)
			v358AssertReachable(t, resumed, tc.want)
			if !resumed.Resumed || resumed.Exhausted {
				t.Errorf("valid resumed task has Resumed=%v Exhausted=%v", resumed.Resumed, resumed.Exhausted)
			}
		})
	}
}

// Include every subset, not just permutations of one complete configuration:
// missing bound/spent/unvisited identities must all remain distinguishable.
func v358OrderedSubsets(ids []int64) [][]int64 {
	orders := [][]int64{{}}
	var visit func([]int64)
	visit = func(prefix []int64) {
		for _, id := range ids {
			if slices.Contains(prefix, id) {
				continue
			}
			next := append(slices.Clone(prefix), id)
			orders = append(orders, next)
			visit(next)
		}
	}
	visit(nil)
	return orders
}

func TestV358RoutingReorderEveryPriorityPermutationAndRemoval(t *testing.T) {
	const a, b, c, d, newProvider int64 = 11, 22, 33, 44, 55
	liveOrders := v358OrderedSubsets([]int64{a, b, c, d, newProvider})
	historyOrders := v358OrderedSubsets([]int64{a, b, c, d})
	checked := 0
	for _, historical := range historyOrders {
		if len(historical) != 4 {
			continue
		}
		for boundIndex, bound := range historical {
			session := v358HistoricalSession(historical, bound)
			for _, live := range liveOrders {
				want := make([]int64, 0, len(live))
				if slices.Contains(live, bound) {
					want = append(want, bound)
				}
				for _, id := range live {
					if id != bound && !slices.Contains(historical[:boundIndex], id) {
						want = append(want, id)
					}
				}
				routes := v358PriorityRoutes(live...)
				// The caller's resolution order must not substitute for provider
				// priorities, which encode the live order in this test fixture.
				slices.Reverse(routes)
				plan := buildInferencePlan(routes, "/v1/chat/completions", session, true)
				got := v358ReachableProviders(t, plan)
				if !slices.Equal(got, want) {
					t.Fatalf("history=%v bound=%d live=%v: reachable=%v, want %v", historical, bound, live, got, want)
				}
				wantExhausted := len(live) > 0 && len(want) == 0
				if plan.Exhausted != wantExhausted {
					t.Fatalf("history=%v bound=%d live=%v: Exhausted=%v, want %v", historical, bound, live, plan.Exhausted, wantExhausted)
				}
				if len(live) > 0 && !plan.Resumed {
					t.Fatalf("history=%v bound=%d live=%v: task binding not marked resumed", historical, bound, live)
				}
				checked++
			}
		}
	}
	t.Logf("verified %d historical-order/binding/live-priority configurations", checked)
}

func TestV358RoutingReorderRepeatedConfigurationChangesKeepIdentityHistory(t *testing.T) {
	const a, b, c, d, e, f int64 = 11, 22, 33, 44, 55, 66
	app, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	session := v358HistoricalSession([]int64{a, b, c, d}, b)
	if err := app.bindRoutingSession(request.Context(), session); err != nil {
		t.Fatal(err)
	}

	for step, tc := range []struct {
		live     []int64
		want     []int64
		selected int64
		advance  bool
	}{
		// A and C disappear. A is spent; C is still unvisited.
		{live: []int64{d, b}, want: []int64{b, d}, selected: b},
		// C returns ahead of B, but B stays bound until its real failure.
		{live: []int64{c, e, b}, want: []int64{b, c, e}, selected: c, advance: true},
		// D was absent during that advance and must not become spent.
		{live: []int64{a, d, b, e, c}, want: []int64{c, d, e}, selected: c},
		// C now disappears. A successful E makes the departed C permanent.
		{live: []int64{a, e, b, d}, want: []int64{e, d}, selected: e},
		{live: []int64{d, c, f, a, b, e}, want: []int64{e, d, f}, selected: d, advance: true},
		{live: []int64{e, f, c, b, a, d}, want: []int64{d, f}, selected: d},
	} {
		t.Run(fmt.Sprintf("step_%d", step+1), func(t *testing.T) {
			saved, exists := app.loadRoutingSession(request.Context(), session.TaskHash, session.TaskScope)
			if !exists {
				t.Fatal("task binding disappeared")
			}
			plan := buildInferencePlan(v358PriorityRoutes(tc.live...), request.URL.Path, saved, true)
			v358AssertReachable(t, plan, tc.want)
			selectedIndex := -1
			for index := plan.Start; index < len(plan.Channels); index++ {
				if plan.Channels[index].ProviderID == tc.selected {
					selectedIndex = index
					break
				}
			}
			if selectedIndex < 0 {
				t.Fatalf("selected provider %d not reachable", tc.selected)
			}
			run := inferenceRun{
				app: app, model: "routing-reorder-model", protocol: "openai_chat",
				taskHash: saved.TaskHash, taskScope: saved.TaskScope, taskSource: saved.TaskSource,
				session: saved, hasSession: true, plan: plan,
			}
			if tc.advance {
				run.advance(request, selectedIndex)
			} else {
				run.noteSuccess(request, plan.Channels[selectedIndex].Routes[0], selectedIndex)
			}
		})
		if t.Failed() {
			return
		}
	}
	final, exists := app.loadRoutingSession(request.Context(), session.TaskHash, session.TaskScope)
	if !exists || final.ProviderID != d {
		t.Fatalf("final binding exists=%v provider=%d, want %d", exists, final.ProviderID, d)
	}
	v358AssertReachable(t, buildInferencePlan(v358PriorityRoutes(a, e, b, c), request.URL.Path, final, true), nil)
	if plan := buildInferencePlan(v358PriorityRoutes(a, e, b, c), request.URL.Path, final, true); !plan.Exhausted {
		t.Fatal("returning only spent providers did not exhaust the task")
	}
}

// Concurrent requests can observe different priorities for new channels. Reject
// a stale/unrelated binding without discarding its independent proof that other
// identities departed; otherwise those exhausted channels become reachable again.
func TestV358RoutingReorderRejectedBindingStillMergesDepartureEvidence(t *testing.T) {
	const a, b, c, x, y, unvisited int64 = 11, 22, 33, 44, 55, 66
	stored := v358HistoricalSession([]int64{a, b, c, x, y, unvisited}, c)
	stored.Generation = 20
	for _, tc := range []struct {
		name     string
		proposal routingSession
		want     []int64
	}{
		{
			name:     "stale_spent_target_with_additional_spent_channel",
			proposal: v358HistoricalSession([]int64{a, x, b, c, y, unvisited}, b),
			want:     []int64{c, y, unvisited},
		},
		{
			name:     "unrelated_target_with_additional_spent_channel",
			proposal: v358HistoricalSession([]int64{a, y, x, c, b, unvisited}, x),
			want:     []int64{c, x, unvisited},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeRoutingSession(stored, tc.proposal)
			if merged.ProviderID != stored.ProviderID || merged.ProviderKeyID != stored.ProviderKeyID {
				t.Fatalf("rejected proposal stole binding: provider=%d key=%d; want provider=%d key=%d",
					merged.ProviderID, merged.ProviderKeyID, stored.ProviderID, stored.ProviderKeyID)
			}
			plan := buildInferencePlan(v358PriorityRoutes(x, b, y, c, a, unvisited), "/v1/chat/completions", merged, true)
			v358AssertReachable(t, plan, tc.want)
		})
	}
}

func TestV358RoutingReorderConcurrentPrioritySnapshotsKeepAllDepartures(t *testing.T) {
	const a, b, c, x int64 = 11, 22, 33, 44
	app, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	initial := v358HistoricalSession([]int64{a, b, c}, a)
	if err := app.bindRoutingSession(request.Context(), initial); err != nil {
		t.Fatal(err)
	}
	newRun := func(live []int64) *inferenceRun {
		return &inferenceRun{
			app: app, model: "routing-reorder-model", protocol: "openai_chat",
			taskHash: initial.TaskHash, taskScope: initial.TaskScope, taskSource: initial.TaskSource,
			session: initial, hasSession: true,
			plan: buildInferencePlan(v358PriorityRoutes(live...), request.URL.Path, initial, true),
		}
	}
	// Both requests began on A, but a configuration update moved new X
	// between B and A while the first request was in flight.
	first := newRun([]int64{a, x, b, c})
	second := newRun([]int64{a, b, c, x})
	v358AssertReachable(t, first.plan, []int64{a, x, b, c})
	v358AssertReachable(t, second.plan, []int64{a, b, c, x})

	// Second request proves A and B spent and establishes C first.
	second.advance(request, 1)
	second.advance(request, 2)
	// First request then independently proves A and X spent. Its target B
	// has already departed, but that must not discard the new evidence on X.
	first.advance(request, 1)
	first.advance(request, 2)
	saved, exists := app.loadRoutingSession(request.Context(), initial.TaskHash, initial.TaskScope)
	if !exists || saved.ProviderID != c {
		t.Fatalf("concurrent reordered request stole established binding: exists=%v provider=%d, want %d", exists, saved.ProviderID, c)
	}
	plan := buildInferencePlan(v358PriorityRoutes(x, b, a, c), request.URL.Path, saved, true)
	v358AssertReachable(t, plan, []int64{c})
}
