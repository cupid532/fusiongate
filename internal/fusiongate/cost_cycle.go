package fusiongate

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

// costCycle is the independent per-provider accumulation period shown in the
// console. It lives in its own table so that resetting it never touches
// request_ledger, which keeps the historical "usage and cost" statistics intact,
// and so that retention pruning of old ledger rows can never shrink the current
// period total.
type costCycle struct {
	StartedAt       string           `json:"started_at"`
	ResetReason     string           `json:"reset_reason"`
	OfficialResetAt string           `json:"official_reset_at,omitempty"`
	Requests        int64            `json:"requests"`
	PricedRequests  int64            `json:"priced_requests"`
	CostMicros      int64            `json:"cost_micros"`
	AdjustedMicros  int64            `json:"adjusted_micros"`
	SpendByCategory map[string]int64 `json:"spend_by_category"`
}

// cycleCategories is the fixed category order used by the cycle table columns.
var cycleCategories = []string{"openai", "claude", "grok", "gemini", "other"}

// queueCostCycleWrite folds one completed request into the provider's current
// cost cycle. It is queued on the same writer as the ledger so the counters move
// with the same FIFO ordering and never sit on the response hot path. The
// category buckets keep the raw upstream cost so the console can apply the
// channel's current multipliers at display time.
func (a *App) queueCostCycleWrite(providerID int64, providerType, upstreamModel string, usage Usage) {
	priced := 0
	category := "other"
	if usage.CostType != "unknown" {
		priced = 1
		category = balanceCategory(providerType, upstreamModel)
	}
	openai, claude, grok, gemini, other := int64(0), int64(0), int64(0), int64(0), int64(0)
	if priced == 1 {
		switch category {
		case "openai":
			openai = usage.CostMicros
		case "claude":
			claude = usage.CostMicros
		case "grok":
			grok = usage.CostMicros
		case "gemini":
			gemini = usage.CostMicros
		default:
			other = usage.CostMicros
		}
	}
	a.queueLedgerWrite(`INSERT INTO provider_cost_cycles(provider_id,started_at,reset_reason,requests,priced_requests,cost_micros,openai_micros,claude_micros,grok_micros,gemini_micros,other_micros)
VALUES(?,?,?,1,?,?,?,?,?,?,?)
ON CONFLICT(provider_id) DO UPDATE SET
requests=requests+1,priced_requests=priced_requests+excluded.priced_requests,cost_micros=cost_micros+excluded.cost_micros,
openai_micros=openai_micros+excluded.openai_micros,claude_micros=claude_micros+excluded.claude_micros,
grok_micros=grok_micros+excluded.grok_micros,gemini_micros=gemini_micros+excluded.gemini_micros,other_micros=other_micros+excluded.other_micros`,
		providerID, now(), "tracking_started", priced, usage.CostMicros, openai, claude, grok, gemini, other)
}

// resetCostCycle starts a fresh accumulation period for one provider. When
// officialResetAt is non-empty it is stored as the new official marker; an empty
// value preserves whatever marker was already stored, which is what manual
// balance additions want. All counters zero out; request_ledger is untouched.
func (a *App) resetCostCycle(ctx context.Context, providerID int64, reason, officialResetAt string) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO provider_cost_cycles(provider_id,started_at,reset_reason,official_reset_at)
VALUES(?,?,?,?)
ON CONFLICT(provider_id) DO UPDATE SET
started_at=excluded.started_at,reset_reason=excluded.reset_reason,
official_reset_at=CASE WHEN ?='' THEN official_reset_at ELSE excluded.official_reset_at END,
requests=0,priced_requests=0,cost_micros=0,openai_micros=0,claude_micros=0,grok_micros=0,gemini_micros=0,other_micros=0`,
		providerID, now(), reason, officialResetAt, officialResetAt)
	return err
}

// officialCodexResetAt returns the stable official reset identifier of the current
// Codex usage window. It prefers the primary window and falls back to the
// secondary one. A window derived from reset_after_seconds moves on every refresh,
// so it is deliberately not used: FusionGate would otherwise see a "change" on
// every quota fetch and reset the local cycle for no reason.
func officialCodexResetAt(quota *codexAccountQuota) string {
	if quota == nil {
		return ""
	}
	if quota.Primary != nil && quota.Primary.ResetKey != "" {
		return quota.Primary.ResetKey
	}
	if quota.Secondary != nil && quota.Secondary.ResetKey != "" {
		return quota.Secondary.ResetKey
	}
	return ""
}

// observeCodexQuota compares the account's official window marker with the one
// stored for the provider. The first observation only records the marker; a later
// change means the official quota period rolled over, so the local cost cycle is
// reset without touching the ledger. It returns true when a reset happened.
func (a *App) observeCodexQuota(ctx context.Context, providerID int64, quota *codexAccountQuota) (bool, error) {
	next := officialCodexResetAt(quota)
	if next == "" {
		return false, nil
	}
	var stored string
	err := a.db.QueryRowContext(ctx, `SELECT COALESCE(official_reset_at,'') FROM provider_cost_cycles WHERE provider_id=?`, providerID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		stored = ""
		err = nil
	}
	if err != nil {
		return false, err
	}
	if stored == "" {
		_, err = a.db.ExecContext(ctx, `INSERT INTO provider_cost_cycles(provider_id,started_at,reset_reason,official_reset_at)
VALUES(?,?,?,?)
ON CONFLICT(provider_id) DO UPDATE SET official_reset_at=excluded.official_reset_at`, providerID, now(), "tracking_started", next)
		return false, err
	}
	if stored == next {
		return false, nil
	}
	return true, a.resetCostCycle(ctx, providerID, "official_quota_reset", next)
}

// readCostCycle loads one provider's cycle and applies the current multipliers so
// the console can show the same adjusted figure used by the manual balance card.
// A nil result means no cycle row exists yet (the provider never completed a
// request and was not seeded), which the UI renders as "尚未开始累计".
func (a *App) readCostCycle(ctx context.Context, providerID int64, providerType string, multipliers map[string]float64) (*costCycle, error) {
	var cycle costCycle
	var openai, claude, grok, gemini, other int64
	err := a.db.QueryRowContext(ctx, `SELECT started_at,reset_reason,COALESCE(official_reset_at,''),requests,priced_requests,cost_micros,openai_micros,claude_micros,grok_micros,gemini_micros,other_micros FROM provider_cost_cycles WHERE provider_id=?`, providerID).
		Scan(&cycle.StartedAt, &cycle.ResetReason, &cycle.OfficialResetAt, &cycle.Requests, &cycle.PricedRequests, &cycle.CostMicros, &openai, &claude, &grok, &gemini, &other)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cycle.SpendByCategory = map[string]int64{"openai": openai, "claude": claude, "grok": grok, "gemini": gemini, "other": other}
	for _, category := range cycleCategories {
		cycle.AdjustedMicros += int64(math.Round(float64(cycle.SpendByCategory[category]) * multipliers[category]))
	}
	return &cycle, nil
}

// seedCostCycles runs once at startup: providers that already track a manual
// balance start their cycle at the existing baseline (so the console shows the
// same period the balance card already reports), and every other provider starts
// accumulating from the upgrade moment. Existing cycle rows are never overwritten.
func (a *App) seedCostCycles(ctx context.Context) error {
	type seedInfo struct {
		providerID   int64
		providerType string
		startedAt    string
	}
	var manual []seedInfo
	all := map[int64]seedInfo{}
	rows, err := a.db.QueryContext(ctx, `SELECT id,type,COALESCE(manual_balance_micros,-1),COALESCE(balance_baseline_at,'') FROM providers`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var providerType string
		var manualMicros sql.NullInt64
		var baseline sql.NullString
		if err := rows.Scan(&id, &providerType, &manualMicros, &baseline); err != nil {
			rows.Close()
			return err
		}
		info := seedInfo{providerID: id, providerType: providerType, startedAt: now()}
		if manualMicros.Valid && baseline.Valid && baseline.String != "" {
			info.startedAt = baseline.String
			manual = append(manual, info)
		}
		all[id] = info
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, info := range manual {
		requests, priced, cost, categoryMicros, seedErr := a.ledgerCycleCounters(ctx, info.providerID, info.providerType, info.startedAt)
		if seedErr != nil {
			return seedErr
		}
		if _, insertErr := a.db.ExecContext(ctx, `INSERT INTO provider_cost_cycles(provider_id,started_at,reset_reason,requests,priced_requests,cost_micros,openai_micros,claude_micros,grok_micros,gemini_micros,other_micros)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(provider_id) DO NOTHING`,
			info.providerID, info.startedAt, "manual_balance_added", requests, priced, cost,
			categoryMicros["openai"], categoryMicros["claude"], categoryMicros["grok"], categoryMicros["gemini"], categoryMicros["other"]); insertErr != nil {
			return insertErr
		}
	}
	seeded := map[int64]bool{}
	for _, info := range manual {
		seeded[info.providerID] = true
	}
	for _, info := range all {
		if seeded[info.providerID] {
			continue
		}
		if _, insertErr := a.db.ExecContext(ctx, `INSERT INTO provider_cost_cycles(provider_id,started_at,reset_reason) VALUES(?,?,?) ON CONFLICT(provider_id) DO NOTHING`, info.providerID, info.startedAt, "tracking_started"); insertErr != nil {
			return insertErr
		}
	}
	return nil
}

// ledgerCycleCounters aggregates one provider's completed ledger rows since a
// timestamp, bucketing raw cost by model category. It is the one-time backfill
// used when a manual-balance provider already had a baseline before this feature.
func (a *App) ledgerCycleCounters(ctx context.Context, providerID int64, providerType, since string) (int64, int64, int64, map[string]int64, error) {
	var requests, priced, costMicros int64
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN cost_type<>'unknown' THEN 1 ELSE 0 END),0),COALESCE(SUM(cost_micros),0) FROM request_ledger WHERE provider_id=? AND completed_at IS NOT NULL AND created_at>=?`, providerID, since).Scan(&requests, &priced, &costMicros); err != nil {
		return 0, 0, 0, nil, err
	}
	categoryMicros := map[string]int64{"openai": 0, "claude": 0, "grok": 0, "gemini": 0, "other": 0}
	rows, err := a.db.QueryContext(ctx, `SELECT upstream_model,SUM(cost_micros) FROM request_ledger WHERE provider_id=? AND completed_at IS NOT NULL AND created_at>=? AND cost_type<>'unknown' GROUP BY upstream_model`, providerID, since)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	for rows.Next() {
		var model string
		var micros int64
		if err := rows.Scan(&model, &micros); err != nil {
			rows.Close()
			return 0, 0, 0, nil, err
		}
		categoryMicros[balanceCategory(providerType, model)] += micros
	}
	if err := rows.Close(); err != nil {
		return 0, 0, 0, nil, err
	}
	return requests, priced, costMicros, categoryMicros, nil
}
