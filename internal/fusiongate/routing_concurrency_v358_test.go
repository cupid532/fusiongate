package fusiongate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

const routingConcurrencyV358Path = "/v1/chat/completions"

// These tests exercise the real SQLite session store and the request lifecycle,
// but gate the completion callbacks rather than relying on upstream timing.
// Every concurrent run owns a snapshot taken before either callback can write.
type routingConcurrencyV358Fixture struct {
	app     *App
	routes  []resolvedRoute
	initial routingSession
	request *http.Request
}

func newRoutingConcurrencyV358Fixture(t *testing.T) *routingConcurrencyV358Fixture {
	t.Helper()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	for index, name := range []string{"A", "B", "C"} {
		providerID := insertTestProvider(t, a, name, "openai_compatible", "https://v358.invalid", "unused", 3-index, 1, "normalized", "any", 0, 3, 30)
		if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
			t.Fatal(err)
		}
		for key := range 2 {
			insertProviderKeyForTest(t, a, providerID, fmt.Sprintf("v358-%s-%d", name, key), fmt.Sprintf("%s-%d", name, key), "", "inherit", nil, 1, key)
		}
		insertTestRoute(t, a, providerID, "v358-model", "v358-model", "chat,stream", 0)
	}
	routes, err := a.resolveRoutes(context.Background(), "v358-model", "inference", false)
	if err != nil {
		t.Fatal(err)
	}
	plan := buildInferencePlan(routes, routingConcurrencyV358Path, routingSession{}, false)
	if len(plan.Channels) != 3 {
		t.Fatalf("fixture channels=%d, want 3", len(plan.Channels))
	}
	for _, channel := range plan.Channels {
		if len(channel.Routes) != 2 {
			t.Fatalf("fixture provider=%d keys=%d, want 2", channel.ProviderID, len(channel.Routes))
		}
	}
	request := httptest.NewRequest(http.MethodPost, routingConcurrencyV358Path, nil)
	request.Header.Set(taskIDHeader, t.Name())
	taskHash, taskSource, _ := inferenceTaskIdentity(request, authKey{ID: 358}, a.keyMaterial)
	initial := routingSession{
		TaskHash:      taskHash,
		TaskScope:     inferenceTaskScopeFor("v358-model", passthroughProtocol(routingConcurrencyV358Path)),
		TaskSource:    taskSource,
		Order:         storedOrderFromChannels(plan.Channels),
		Cursor:        0,
		ProviderID:    plan.Channels[0].ProviderID,
		ProviderKeyID: plan.Channels[0].Routes[0].ProviderKeyID,
		PlanID:        sessionPlanID(plan.Channels),
		Generation:    5,
	}
	if err := a.bindRoutingSession(request.Context(), initial); err != nil {
		t.Fatal(err)
	}
	return &routingConcurrencyV358Fixture{app: a, routes: routes, initial: initial, request: request}
}

func (f *routingConcurrencyV358Fixture) snapshot(t *testing.T) *inferenceRun {
	t.Helper()
	session, ok := f.app.loadRoutingSession(f.request.Context(), f.initial.TaskHash, f.initial.TaskScope)
	if !ok {
		t.Fatal("routing session disappeared")
	}
	return &inferenceRun{
		app:        f.app,
		model:      "v358-model",
		path:       routingConcurrencyV358Path,
		protocol:   passthroughProtocol(routingConcurrencyV358Path),
		plan:       buildInferencePlan(f.routes, routingConcurrencyV358Path, session, true),
		taskHash:   session.TaskHash,
		taskScope:  session.TaskScope,
		taskSource: session.TaskSource,
		session:    session,
		hasSession: true,
	}
}

func (f *routingConcurrencyV358Fixture) route(index, key int) resolvedRoute {
	plan := buildInferencePlan(f.routes, routingConcurrencyV358Path, routingSession{}, false)
	return plan.Channels[index].Routes[key]
}

// check also verifies the next request resumes correctly, not merely that a
// generation counter increased. A negative key means the test is cursor-only.
func (f *routingConcurrencyV358Fixture) check(t *testing.T, index int, keyID int64) routingSession {
	t.Helper()
	session, ok := f.app.loadRoutingSession(f.request.Context(), f.initial.TaskHash, f.initial.TaskScope)
	if !ok {
		t.Fatal("routing session disappeared")
	}
	wantProvider := f.route(index, 0).Provider.ID
	if session.ProviderID != wantProvider || session.Cursor != index {
		t.Errorf("persisted binding provider=%d cursor=%d generation=%d key=%d; want provider=%d cursor=%d (a completed stale request must not undo a proven advance)", session.ProviderID, session.Cursor, session.Generation, session.ProviderKeyID, wantProvider, index)
	}
	if keyID >= 0 && session.ProviderKeyID != keyID {
		t.Errorf("persisted key=%d on provider=%d generation=%d; want key=%d", session.ProviderKeyID, session.ProviderID, session.Generation, keyID)
	}
	plan := buildInferencePlan(f.routes, routingConcurrencyV358Path, session, true)
	if plan.Start >= len(plan.Channels) || plan.Channels[plan.Start].ProviderID != wantProvider {
		t.Errorf("next request starts at index=%d; want provider=%d without revisiting a spent channel", plan.Start, wantProvider)
	}
	return session
}

// Both callbacks are live before either is released. The done barrier forces
// each possible commit order without sleeps or a probabilistic scheduler race.
func routingConcurrencyV358CompleteInOrder(first, second func()) {
	ready := make(chan struct{}, 2)
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() {
		ready <- struct{}{}
		<-firstRelease
		first()
		close(firstDone)
	}()
	go func() {
		ready <- struct{}{}
		<-secondRelease
		second()
		close(secondDone)
	}()
	<-ready
	<-ready
	close(firstRelease)
	<-firstDone
	close(secondRelease)
	<-secondDone
}

func TestRoutingConcurrencyV358StaleSuccessCannotUndoAdvance(t *testing.T) {
	for _, successFirst := range []bool{false, true} {
		name := "advance_then_late_success"
		if successFirst {
			name = "success_then_equal_generation_advance"
		}
		t.Run(name, func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			advancing, succeeding := f.snapshot(t), f.snapshot(t)
			advance := func() { advancing.advance(f.request, 1) }
			success := func() { succeeding.noteSuccess(f.request, f.route(0, 0), 0) }
			if successFirst {
				routingConcurrencyV358CompleteInOrder(success, advance)
			} else {
				routingConcurrencyV358CompleteInOrder(advance, success)
			}
			// Both callbacks originate at A/g5. In V3.57 both write g6:
			// the forward write must win in BOTH orders, not just first/last.
			f.check(t, 1, -1)
		})
	}
}

func TestRoutingConcurrencyV358OrdinaryConcurrentSuccessesKeepKeyStickiness(t *testing.T) {
	for _, differentKeys := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("different_keys=%t/reverse=%t", differentKeys, reverse), func(t *testing.T) {
				f := newRoutingConcurrencyV358Fixture(t)
				first, second := f.snapshot(t), f.snapshot(t)
				firstRoute, secondRoute := f.route(0, 0), f.route(0, 0)
				if differentKeys {
					secondRoute = f.route(0, 1)
				}
				firstSuccess := func() { first.noteSuccess(f.request, firstRoute, 0) }
				secondSuccess := func() { second.noteSuccess(f.request, secondRoute, 0) }
				lastRoute := secondRoute
				if reverse {
					routingConcurrencyV358CompleteInOrder(secondSuccess, firstSuccess)
					lastRoute = firstRoute
				} else {
					routingConcurrencyV358CompleteInOrder(firstSuccess, secondSuccess)
				}
				// No channel departed: equal-generation successes on the bound
				// provider must still be able to remember a working credential.
				session := f.check(t, 0, lastRoute.ProviderKeyID)
				next := f.snapshot(t)
				keys := preferSessionKey(next.plan.Channels[next.plan.Start].Routes, session.ProviderKeyID)
				if keys[0].ProviderKeyID != lastRoute.ProviderKeyID {
					t.Errorf("next request prefers key=%d, want last working key=%d", keys[0].ProviderKeyID, lastRoute.ProviderKeyID)
				}
			})
		}
	}
}

func TestRoutingConcurrencyV358AdvanceDoesNotCarryPreviousProvidersKey(t *testing.T) {
	f := newRoutingConcurrencyV358Fixture(t)
	run := f.snapshot(t)
	run.advance(f.request, 1)
	// A's successful key is not evidence that any key on B has succeeded.
	f.check(t, 1, 0)
	working := f.route(1, 1)
	run.noteSuccess(f.request, working, 1)
	session := f.check(t, 1, working.ProviderKeyID)
	next := f.snapshot(t)
	keys := preferSessionKey(next.plan.Channels[next.plan.Start].Routes, session.ProviderKeyID)
	if keys[0].ProviderKeyID != working.ProviderKeyID {
		t.Errorf("advanced task prefers key=%d, want B's working key=%d", keys[0].ProviderKeyID, working.ProviderKeyID)
	}
}

func TestRoutingConcurrencyV358StaleSuccessCohortsCannotRegainAuthority(t *testing.T) {
	for _, backupSucceeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("backup_succeeded=%t", backupSucceeded), func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			stale5 := f.snapshot(t)
			f.snapshot(t).noteSuccess(f.request, f.route(0, 0), 0)
			advancing, stale6 := f.snapshot(t), f.snapshot(t)
			f.snapshot(t).noteSuccess(f.request, f.route(0, 1), 0)
			stale7 := f.snapshot(t)
			// A newer success has changed the row, but cannot invalidate the
			// older in-flight request's proof that A must now be left behind.
			advancing.advance(f.request, 1)
			wantKey := int64(-1)
			if backupSucceeded {
				working := f.route(1, 1)
				advancing.noteSuccess(f.request, working, 1)
				wantKey = working.ProviderKeyID
			}
			f.check(t, 1, wantKey)
			for index, stale := range []*inferenceRun{stale5, stale6, stale7} {
				stale.noteSuccess(f.request, f.route(0, index%2), 0)
				// These are distinct requests that observed generations 5, 6
				// and 7, not repeated callbacks on one artificial request.
				f.check(t, 1, wantKey)
			}
		})
	}
}

func TestRoutingConcurrencyV358OlderAdvanceSurvivesNewerSameChannelSuccess(t *testing.T) {
	f := newRoutingConcurrencyV358Fixture(t)
	advancing := f.snapshot(t)
	for index := range 3 {
		f.snapshot(t).noteSuccess(f.request, f.route(0, index%2), 0)
	}
	// A/g8 is still A. An outstanding A/g5 request that exhausts A is
	// allowed to move forward, even though its proposed generation is only 6.
	advancing.advance(f.request, 1)
	f.check(t, 1, -1)
}

func TestRoutingConcurrencyV358DivergentAdvancesKeepFurthestProvenCursor(t *testing.T) {
	for _, furthestFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("furthest_first=%t", furthestFirst), func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			near, far := f.snapshot(t), f.snapshot(t)
			nearAdvance := func() { near.advance(f.request, 1) }
			farAdvance := func() { far.advance(f.request, 2) }
			if furthestFirst {
				routingConcurrencyV358CompleteInOrder(farAdvance, nearAdvance)
			} else {
				routingConcurrencyV358CompleteInOrder(nearAdvance, farAdvance)
			}
			f.check(t, 2, -1)
		})
	}
}

// Release all writers from the same barrier. The result must be independent of
// which goroutine obtains the single SQLite writer connection first.
func routingConcurrencyV358CompleteTogether(callbacks ...func()) {
	ready := make(chan struct{}, len(callbacks))
	release := make(chan struct{})
	done := make(chan struct{}, len(callbacks))
	for _, callback := range callbacks {
		go func() {
			ready <- struct{}{}
			<-release
			callback()
			done <- struct{}{}
		}()
	}
	for range callbacks {
		<-ready
	}
	close(release)
	for range callbacks {
		<-done
	}
}

func TestRoutingConcurrencyV358SimultaneousSessionWriters(t *testing.T) {
	for _, tc := range []struct {
		name     string
		advances bool
	}{
		{"same_provider_successes", false},
		{"near_and_far_advances_with_stale_successes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			// Reuse the real App but isolate each simultaneous wave by task.
			for wave := range 4 {
				f.initial.TaskHash = fmt.Sprintf("%s-wave-%d", t.Name(), wave)
				if err := f.app.bindRoutingSession(f.request.Context(), f.initial); err != nil {
					t.Fatal(err)
				}
				callbacks := make([]func(), 0, 8)
				for worker := range 8 {
					run := f.snapshot(t)
					if tc.advances && worker%3 != 0 {
						index := 1 + worker%2
						callbacks = append(callbacks, func() { run.advance(f.request, index) })
					} else {
						route := f.route(0, worker%2)
						callbacks = append(callbacks, func() { run.noteSuccess(f.request, route, 0) })
					}
				}
				routingConcurrencyV358CompleteTogether(callbacks...)
				if tc.advances {
					f.check(t, 2, 0)
				} else {
					session := f.check(t, 0, -1)
					if session.ProviderKeyID != f.route(0, 0).ProviderKeyID && session.ProviderKeyID != f.route(0, 1).ProviderKeyID {
						t.Errorf("same-provider writers lost all working credentials: key=%d", session.ProviderKeyID)
					}
				}
			}
		})
	}
}

func TestRoutingConcurrencyV358ConcurrentAdvancePreservesWorkingDestinationKey(t *testing.T) {
	f := newRoutingConcurrencyV358Fixture(t)
	early := f.snapshot(t)
	for range 2 {
		f.snapshot(t).noteSuccess(f.request, f.route(0, 0), 0)
	}
	late := f.snapshot(t)
	early.advance(f.request, 1)
	working := f.route(1, 1)
	early.noteSuccess(f.request, working, 1)
	f.check(t, 1, working.ProviderKeyID)
	// A's newer snapshot now proposes the SAME B generation but without a
	// successful B credential. That advance must not erase B's proven key.
	late.advance(f.request, 1)
	f.check(t, 1, working.ProviderKeyID)
}

func TestRoutingConcurrencyV358CanceledStoreReleasesTransaction(t *testing.T) {
	for _, midTransaction := range []bool{false, true} {
		t.Run(fmt.Sprintf("mid_transaction=%t", midTransaction), func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			ctx, cancel := context.WithCancel(f.request.Context())
			defer cancel()
			var authorizerReached atomic.Bool
			if midTransaction {
				// Cancel at SQLite's INSERT authorization point, after the
				// transaction has read its snapshot. No sleeps or SQL locks are
				// needed to hit the rollback path deterministically.
				registerProviderKeyAuthorizerForTest(t, f.app, func(op int, table, column, database string) int {
					if op == sqlite3.SQLITE_INSERT && table == "routing_sessions" && authorizerReached.CompareAndSwap(false, true) {
						cancel()
						return sqlite3.SQLITE_DENY
					}
					return sqlite3.SQLITE_OK
				})
			} else {
				cancel()
			}
			proposed := f.initial
			proposed.Cursor = 1
			proposed.ProviderID = f.route(1, 0).Provider.ID
			proposed.ProviderKeyID = 0
			proposed.Generation++
			err := f.app.bindRoutingSession(ctx, proposed)
			if midTransaction {
				if !authorizerReached.Load() {
					t.Error("store never reached the in-transaction cancellation barrier")
				}
				// The driver may report its authorizer denial or the context
				// cancellation; either must roll back without partial state.
				if err == nil {
					t.Error("canceled transaction unexpectedly committed")
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Errorf("canceled store error=%v, want context.Canceled", err)
			}
			f.check(t, 0, f.initial.ProviderKeyID)
			// Bound the subsequent write so a leaked transaction or writer
			// connection becomes a test failure, not a stuck test process.
			writeCtx, stop := context.WithTimeout(f.request.Context(), 3*time.Second)
			defer stop()
			if err := f.app.bindRoutingSession(writeCtx, proposed); err != nil {
				t.Fatalf("writer unavailable after canceled store: %v", err)
			}
			f.check(t, 1, 0)
		})
	}
}

func TestRoutingConcurrencyV358BindResolvesEqualGenerationByForwardEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first      int
		second     int
		generation int64
		want       int
	}{
		{"advance_then_stale_success", 1, 0, 6, 1},
		{"success_then_legitimate_advance", 0, 1, 6, 1},
		{"further_then_lesser_advance", 2, 1, 6, 2},
		{"lesser_then_further_advance", 1, 2, 6, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutingConcurrencyV358Fixture(t)
			for _, index := range []int{tc.first, tc.second} {
				session := f.initial
				session.Cursor = index
				session.ProviderID = f.route(index, 0).Provider.ID
				session.ProviderKeyID = f.route(index, 0).ProviderKeyID
				session.Generation = tc.generation
				if err := f.app.bindRoutingSession(f.request.Context(), session); err != nil {
					t.Fatal(err)
				}
			}
			// The SQL boundary must preserve this invariant even when callers
			// independently propose the same generation. Replacing >= with >
			// merely swaps which completion order loses.
			f.check(t, tc.want, -1)
		})
	}
}
