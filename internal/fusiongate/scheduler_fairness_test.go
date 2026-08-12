package fusiongate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func schedulerKeyRoute(id, providerID, providerKeyID int64, model string, order int) resolvedRoute {
	z := schedulerRoute(id, providerID, model, 0, order)
	z.ProviderKeyID = providerKeyID
	return z
}

// A provider that exposes several API keys resolves to several routes. Adaptive
// selection must still weigh it as one upstream, otherwise multi-key providers win
// the rotation only because they were counted more often than single-key peers.
func TestAdaptiveDoesNotFavorMultiKeyProviders(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{
		schedulerKeyRoute(1, 1, 11, "model", 0),
		schedulerKeyRoute(2, 1, 12, "model", 1),
		schedulerKeyRoute(3, 1, 13, "model", 2),
		schedulerKeyRoute(4, 2, 21, "model", 3),
	}
	perProvider := map[int64]int{}
	for range 400 {
		z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("expected adaptive route")
		}
		perProvider[z.Provider.ID]++
		releaseSelectedRoute(a, z)
	}
	if perProvider[1] == 0 || perProvider[2] == 0 {
		t.Fatalf("adaptive selection starved a provider: %v", perProvider)
	}
	ratio := float64(perProvider[1]) / float64(perProvider[2])
	if ratio < 0.75 || ratio > 1.35 {
		t.Fatalf("three-key provider and single-key provider should share traffic evenly, got %v (ratio %.2f)", perProvider, ratio)
	}
}

// Cooldown only makes a circuit selectable again; something has to actually send the
// half-open probe. Under adaptive scoring the recovering provider still carries its
// failure penalty, so it must be probed explicitly instead of competing on score.
func TestAdaptiveProbesRecoveredProviderAfterCooldown(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routes := []resolvedRoute{
		schedulerRoute(1, 1, "model", 0, 0),
		schedulerRoute(2, 2, "model", 0, 1),
	}
	a.routeMu.Lock()
	a.providerStates[1] = &providerRuntime{EWMAFirstByteMS: 90}
	a.providerStates[2] = &providerRuntime{
		EWMAFirstByteMS:     4000,
		ConsecutiveFailures: 6,
		CircuitOpenUntil:    time.Now().Add(-time.Second),
	}
	a.routeMu.Unlock()

	z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
	if !ok {
		t.Fatal("expected adaptive route")
	}
	if z.Provider.ID != 2 {
		t.Fatalf("expected the provider leaving cooldown to receive the half-open probe, got provider %d", z.Provider.ID)
	}
	a.routeMu.Lock()
	probing := a.providerStates[2].HalfOpenProbe
	a.routeMu.Unlock()
	if !probing {
		t.Fatal("half-open probe flag was not reserved")
	}

	// Only one probe may be in flight, so the next request goes to the healthy peer.
	next, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
	if !ok {
		t.Fatal("expected a second adaptive route")
	}
	if next.Provider.ID != 1 {
		t.Fatalf("expected the healthy provider while a probe is in flight, got provider %d", next.Provider.ID)
	}
}

// The smooth weighted accumulator only distributes traffic correctly over a stable
// candidate set. One accumulator shared by every public model lets a busy model
// permanently bias a quiet one that happens to share an upstream.
func TestAdaptiveKeepsPerModelRotationIndependent(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	busy := []resolvedRoute{
		schedulerRoute(1, 1, "busy", 0, 0),
		schedulerRoute(2, 2, "busy", 0, 1),
	}
	quiet := []resolvedRoute{
		schedulerRoute(3, 1, "quiet", 0, 0),
		schedulerRoute(4, 2, "quiet", 0, 1),
	}
	for range 250 {
		z, _, ok := a.acquireRoute(busy, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("expected route for busy model")
		}
		releaseSelectedRoute(a, z)
	}
	counts := map[int64]int{}
	for range 200 {
		z, _, ok := a.acquireRoute(quiet, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("expected route for quiet model")
		}
		counts[z.Provider.ID]++
		releaseSelectedRoute(a, z)
	}
	if counts[1] == 0 || counts[2] == 0 {
		t.Fatalf("quiet model inherited a skewed rotation: %v", counts)
	}
	ratio := float64(counts[1]) / float64(counts[2])
	if ratio < 0.8 || ratio > 1.25 {
		t.Fatalf("quiet model rotation was biased by the busy model: %v (ratio %.2f)", counts, ratio)
	}
}

func TestProviderHealthFactorNeverPenalizesConfirmedReachable(t *testing.T) {
	reachable := providerHealthFactor(Provider{HealthCheckStatus: "reachable"})
	unknown := providerHealthFactor(Provider{})
	if reachable < unknown {
		t.Fatalf("confirmed-reachable provider scored %.3f, below never-probed provider %.3f", reachable, unknown)
	}
	if failed := providerHealthFactor(Provider{HealthCheckStatus: "unreachable"}); failed >= unknown {
		t.Fatalf("unreachable provider scored %.3f, expected below never-probed %.3f", failed, unknown)
	}
}

// A budget is an accounting limit, never a concurrency limit. Every request from a
// budgeted key must be admitted while the budget still has headroom, no matter how
// many are already in flight.
func TestBudgetedKeyNeverLimitsConcurrency(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := "fg_concurrent_budget_key"
	sum := sha256.Sum256([]byte(raw))
	if _, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_images,rpm_limit,budget_micros,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"budget", "fg_concur", hex.EncodeToString(sum[:]), 1, 1, 0, 1_000_000, now()); err != nil {
		t.Fatal(err)
	}

	const concurrency = 24
	entered := make(chan struct{}, concurrency)
	release := make(chan struct{})
	handler := a.api(func(w http.ResponseWriter, r *http.Request, _ authKey) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})

	results := make(chan int, concurrency)
	for range concurrency {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer "+raw)
			rec := httptest.NewRecorder()
			handler(rec, req)
			results <- rec.Code
		}()
	}
	// Every request must reach the handler concurrently; none may be turned away.
	for i := range concurrency {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("only %d of %d concurrent requests were admitted for a budgeted key", i, concurrency)
		}
	}
	close(release)
	for range concurrency {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("concurrent budgeted request returned %d, want 200", code)
		}
	}
}

// Spending past the budget is what stops a key, and it must report 402 rather than a
// rate-limit style rejection.
func TestBudgetedKeyStopsOnlyWhenBudgetIsSpent(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := "fg_spent_budget_key"
	sum := sha256.Sum256([]byte(raw))
	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_images,rpm_limit,budget_micros,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"budget", "fg_spent", hex.EncodeToString(sum[:]), 1, 1, 0, 1_000_000, now())
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := res.LastInsertId()
	// Settle the whole budget through the real accounting path.
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: "m"}, Provider: Provider{ID: 1}}
	a.endLedger(a.startLedger(authKey{ID: keyID}, route, "openai_chat", false, "127.0.0.1", "req_spent", "", 1, ""), route.Provider.ID, keyID, "openai", route.Route.UpstreamModel,
		true, 200, "", time.Now(), Usage{CostMicros: 1_000_000, CostType: "estimated", Reported: true})
	a.flushLedgerWrites()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("exhausted budget returned %d, want 402; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"budget_exceeded"`) {
		t.Fatalf("exhausted budget body=%s, want exact budget_exceeded code", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "budget_request_inflight") {
		t.Fatal("budget enforcement must never reject a request for being concurrent")
	}
}
