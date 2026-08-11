package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func settleCycleSpend(a *App, providerID int64, providerType, model string, requestID string, cost int64) {
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: model}, Provider: Provider{ID: providerID, Type: providerType}}
	attemptID := a.startLedger(authKey{ID: 1}, route, "openai_chat", false, "127.0.0.1", requestID, "", 1, "")
	a.endLedger(attemptID, providerID, 1, providerType, model, true, 200, "", time.Now(), Usage{CostMicros: cost, CostType: "estimated", Reported: true})
	a.flushLedgerWrites()
}

// Every completed request folds into the provider's independent cycle, and the
// console applies the channel's current multipliers to the raw category buckets.
func TestCostCycleAccumulatesThroughEndLedger(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "cycle relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	settleCycleSpend(a, providerID, "openai_compatible", "gpt-5.6-sol", "cycle_req_a", 500000)
	settleCycleSpend(a, providerID, "openai_compatible", "claude-sonnet-5", "cycle_req_b", 400000)

	cycle, err := a.readCostCycle(context.Background(), providerID, "openai_compatible", balanceMultipliers(2, 1.5, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil {
		t.Fatal("cycle missing")
	}
	if cycle.Requests != 2 || cycle.PricedRequests != 2 || cycle.CostMicros != 900000 {
		t.Fatalf("cycle=%#v", cycle)
	}
	if cycle.SpendByCategory["openai"] != 500000 || cycle.SpendByCategory["claude"] != 400000 {
		t.Fatalf("categories=%#v", cycle.SpendByCategory)
	}
	// 0.5*2 + 0.4*1.5 = $1.6
	if cycle.AdjustedMicros != 1_600_000 {
		t.Fatalf("adjusted=%d, want 1600000", cycle.AdjustedMicros)
	}
	if cycle.ResetReason != "tracking_started" || cycle.StartedAt == "" {
		t.Fatalf("cycle=%#v", cycle)
	}

	// The balance endpoint carries the same cycle for the console.
	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/providers/"+intString(providerID)+"/balance", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// Saving a balance starts a fresh accumulation period without touching the ledger.
func TestCostCycleManualBalanceSaveResets(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "balance reset relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	settleCycleSpend(a, providerID, "openai_compatible", "gpt-5.6-sol", "pre_balance", 300000)

	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"manual_balance_usd":5,"balance_multiplier_openai":2}`)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	cycle, err := a.readCostCycle(context.Background(), providerID, "openai_compatible", balanceMultipliers(2, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil || cycle.Requests != 0 || cycle.CostMicros != 0 || cycle.AdjustedMicros != 0 {
		t.Fatalf("cycle after balance save=%#v", cycle)
	}
	if cycle.ResetReason != "manual_balance_added" {
		t.Fatalf("reason=%q", cycle.ResetReason)
	}

	settleCycleSpend(a, providerID, "openai_compatible", "gpt-5.6-sol", "post_balance", 100000)
	cycle, err = a.readCostCycle(context.Background(), providerID, "openai_compatible", balanceMultipliers(2, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Requests != 1 || cycle.CostMicros != 100000 || cycle.AdjustedMicros != 200000 {
		t.Fatalf("cycle after new spend=%#v", cycle)
	}

	// The ledger history is untouched by the reset.
	var ledgerRequests int64
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE provider_id=?`, providerID).Scan(&ledgerRequests); err != nil {
		t.Fatal(err)
	}
	if ledgerRequests != 2 {
		t.Fatalf("ledger rows=%d, want 2", ledgerRequests)
	}
}

// The first quota observation only records the official window marker. A later
// change means the official quota period rolled over, so the local cycle resets.
func TestCostCycleOfficialQuotaRolloverResets(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "quota rollover", "codex_oauth", "https://chatgpt.com/backend-api/codex", "secret", 1, 100, "normalized", "any", 0, 3, 30)

	first := &codexAccountQuota{Primary: &codexUsageWindow{ResetAt: "2026-08-10T00:00:00Z", ResetKey: "2026-08-10T00:00:00Z"}}
	reset, err := a.observeCodexQuota(context.Background(), providerID, first)
	if err != nil || reset {
		t.Fatalf("first observe reset=%v err=%v", reset, err)
	}
	settleCycleSpend(a, providerID, "codex_oauth", "gpt-5.6-sol", "quota_req", 250000)

	reset, err = a.observeCodexQuota(context.Background(), providerID, first)
	if err != nil || reset {
		t.Fatalf("same-window observe reset=%v err=%v", reset, err)
	}
	cycle, err := a.readCostCycle(context.Background(), providerID, "codex_oauth", balanceMultipliers(1, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil || cycle.CostMicros != 250000 {
		t.Fatalf("cycle before rollover=%#v", cycle)
	}

	rolled := &codexAccountQuota{Primary: &codexUsageWindow{ResetAt: "2026-08-11T00:00:00Z", ResetKey: "2026-08-11T00:00:00Z"}}
	reset, err = a.observeCodexQuota(context.Background(), providerID, rolled)
	if err != nil || !reset {
		t.Fatalf("rollover observe reset=%v err=%v", reset, err)
	}
	cycle, err = a.readCostCycle(context.Background(), providerID, "codex_oauth", balanceMultipliers(1, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil || cycle.Requests != 0 || cycle.CostMicros != 0 || cycle.ResetReason != "official_quota_reset" || cycle.OfficialResetAt != "2026-08-11T00:00:00Z" {
		t.Fatalf("cycle after rollover=%#v", cycle)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE provider_id=?`, providerID).Scan(&cycle.Requests); err != nil {
		t.Fatal(err)
	}
	if cycle.Requests != 1 {
		t.Fatalf("ledger rows=%d, want 1 after official reset", cycle.Requests)
	}
}

// A window derived from reset_after_seconds moves on every refresh and must never
// trigger a cycle reset.
func TestCostCycleIgnoresDerivedResetWindow(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "derived window", "codex_oauth", "https://chatgpt.com/backend-api/codex", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	derived := &codexAccountQuota{Primary: &codexUsageWindow{ResetAt: time.Now().UTC().Add(5 * time.Hour).Format(time.RFC3339)}}
	reset, err := a.observeCodexQuota(context.Background(), providerID, derived)
	if err != nil || reset {
		t.Fatalf("derived observe reset=%v err=%v", reset, err)
	}
	var count int64
	var stored string
	if err := a.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(official_reset_at),'') FROM provider_cost_cycles WHERE provider_id=?`, providerID).Scan(&count, &stored); err != nil {
		t.Fatal(err)
	}
	if count != 0 || stored != "" {
		t.Fatalf("derived marker count=%d stored=%q, want none", count, stored)
	}
}

// Redeeming a reset card immediately zeroes the local cycle.
func TestCostCycleResetCardResets(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "reset card", "codex_oauth", "https://chatgpt.com/backend-api/codex", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	settleCycleSpend(a, providerID, "codex_oauth", "gpt-5.6-sol", "card_req", 500000)

	if err := a.resetCostCycle(context.Background(), providerID, "reset_card", "2026-08-11T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	cycle, err := a.readCostCycle(context.Background(), providerID, "codex_oauth", balanceMultipliers(1, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil || cycle.Requests != 0 || cycle.CostMicros != 0 || cycle.ResetReason != "reset_card" || cycle.OfficialResetAt != "2026-08-11T12:00:00Z" {
		t.Fatalf("cycle after reset card=%#v", cycle)
	}
}

// Providers that already track a manual balance are seeded from their existing
// baseline on upgrade, and everyone else starts accumulating at upgrade time.
func TestCostCycleSeedsProvidersOnUpgrade(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	providerID := insertTestProvider(t, a, "seed relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	past := "2026-01-01T00:00:00Z"
	older := "2025-12-01T00:00:00Z"
	if _, err := a.db.Exec(`UPDATE providers SET manual_balance_micros=5000000,balance_baseline_at=? WHERE id=?`, past, providerID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		requestID string
		stamp     string
		cost      int64
		model     string
	}{{"seed_old", older, 900000, "gpt-5.6-sol"}, {"seed_kept", past, 300000, "gpt-5.6-sol"}, {"seed_claude", past, 200000, "claude-sonnet-5"}} {
		if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,provider_id,created_at,completed_at,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?)`, row.requestID, providerID, row.stamp, row.stamp, row.model, row.model, "chat", row.cost, "estimated"); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cycle, err := reopened.readCostCycle(context.Background(), providerID, "openai_compatible", balanceMultipliers(2, 1.5, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle == nil || cycle.StartedAt != past || cycle.ResetReason != "manual_balance_added" {
		t.Fatalf("seeded cycle=%#v", cycle)
	}
	if cycle.Requests != 2 || cycle.PricedRequests != 2 || cycle.CostMicros != 500000 {
		t.Fatalf("seeded counters=%#v", cycle)
	}
	if cycle.SpendByCategory["openai"] != 300000 || cycle.SpendByCategory["claude"] != 200000 {
		t.Fatalf("seeded categories=%#v", cycle.SpendByCategory)
	}
	// 0.3*2 + 0.2*1.5 = $0.9
	if cycle.AdjustedMicros != 900000 {
		t.Fatalf("seeded adjusted=%d, want 900000", cycle.AdjustedMicros)
	}

	// Reopening again must not double-count the backfill.
	settleCycleSpend(reopened, providerID, "openai_compatible", "gpt-5.6-sol", "seed_later", 100000)
	cycle, err = reopened.readCostCycle(context.Background(), providerID, "openai_compatible", balanceMultipliers(2, 1.5, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Requests != 3 || cycle.CostMicros != 600000 {
		t.Fatalf("cycle after reopen=%#v", cycle)
	}
}
