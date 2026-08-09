package fusiongate

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func budgetTestKey(t *testing.T, a *App, raw string, budget int64) int64 {
	t.Helper()
	sum := sha256.Sum256([]byte(raw))
	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_images,rpm_limit,budget_micros,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		"budget", raw[:8], hex.EncodeToString(sum[:]), 1, 1, 0, budget, now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func settleSpend(a *App, keyID int64, requestID string, cost int64) {
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: "m"}, Provider: Provider{ID: 1}}
	attemptID := a.startLedger(authKey{ID: keyID}, route, "openai_chat", false, "127.0.0.1", requestID, "", 1, "")
	a.endLedger(attemptID, keyID, true, 200, "", time.Now(), Usage{CostMicros: cost, CostType: "estimated", Reported: true})
	a.flushLedgerWrites()
}

// Spend accumulates on the key itself, so admission no longer has to sum the whole
// ledger on every request.
func TestKeySpendIsMaintainedOnTheKey(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	keyID := budgetTestKey(t, a, "fg_spend_counter", 1_000_000)
	settleSpend(a, keyID, "req_a", 300_000)
	settleSpend(a, keyID, "req_b", 250_000)
	spent, err := a.keySpentMicros(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if spent != 550_000 {
		t.Fatalf("spent = %d, want 550000", spent)
	}
}

// Retention prunes ledger rows after a year. Deriving spend from the ledger meant a
// budget silently refunded itself once its rows aged out; the running total must not.
func TestRetentionPruningDoesNotRefundBudget(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	keyID := budgetTestKey(t, a, "fg_spend_prune", 1_000_000)
	settleSpend(a, keyID, "req_old", 1_000_000)

	old := time.Now().UTC().AddDate(-2, 0, 0).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE request_ledger SET created_at=? WHERE gateway_request_id='req_old'`, old); err != nil {
		t.Fatal(err)
	}
	if err := a.pruneRequestLedger(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE gateway_request_id='req_old'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired ledger rows remaining = %d, want 0", remaining)
	}
	spent, err := a.keySpentMicros(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if spent != 1_000_000 {
		t.Fatalf("spend after pruning = %d, want 1000000 — pruning must not hand budget back", spent)
	}
}

// Databases created before the running total exists must have it seeded from their
// history, otherwise an upgrade would reset every budget to zero spent.
func TestUpgradeSeedsKeySpendFromExistingLedger(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keyID := budgetTestKey(t, a, "fg_spend_seed", 1_000_000)
	settleSpend(a, keyID, "req_history", 400_000)
	// Recreate the pre-upgrade shape: ledger history present, no running total.
	if _, err := a.db.Exec(`ALTER TABLE api_keys DROP COLUMN spent_micros`); err != nil {
		t.Skipf("this SQLite build cannot drop a column: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	spent, err := upgraded.keySpentMicros(keyID)
	if err != nil {
		t.Fatal(err)
	}
	if spent != 400_000 {
		t.Fatalf("seeded spend = %d, want 400000", spent)
	}
}
