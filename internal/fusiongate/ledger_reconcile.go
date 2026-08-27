package fusiongate

import (
	"context"
	"time"
)

// The request ledger records observability rows, so its integrity matters more
// than latency: an open row (completed_at IS NULL) represents an attempt that
// began but never finalized. Normal failures always reach the finalize UPDATE,
// but abrupt process death or lost final writes leave rows open forever.
// Reconciliation closes them so the console keeps telling the truth: nothing
// runs forever.

const (
	// ledgerReconcileInterval controls how often expired open rows are swept.
	ledgerReconcileInterval = 10 * time.Minute
	// ledgerReconcileHardAge is the age beyond which an open ledger row cannot
	// be legitimate: stream idle timeouts and request timeouts already ended
	// every real attempt long before this. Sweeping uses it as a safety fuse.
	ledgerReconcileHardAge = 2 * time.Hour
	// ledgerStaleAfterDefault marks console rows stale when their channel has
	// no explicit request timeout configured.
	ledgerStaleAfterDefault = 30 * time.Minute
	// ledgerStaleGrace is added on top of an explicit request timeout before a
	// running row is presented as suspected-stalled in the console.
	ledgerStaleGrace = time.Minute
)

// reconcileStartupLedgerRows closes every open ledger row left behind by a
// previous process. Startup means nothing is executing anymore, so any open
// row is by definition abandoned work, not an in-flight request.
func (a *App) reconcileStartupLedgerRows() {
	a.queueLedgerWrite(
		"UPDATE request_ledger SET completed_at=?, success=0, status_code=0, error_type='gateway_interrupted' WHERE completed_at IS NULL",
		now(),
	)
}

// reconcileOpenLedgerRows closes open rows older than the hard-age fuse. Rows
// younger than that are left alone even if their final UPDATE was delayed.
func (a *App) reconcileOpenLedgerRows(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-ledgerReconcileHardAge).Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(ctx,
		"UPDATE request_ledger SET completed_at=?, success=0, status_code=0, error_type='ledger_stale_closed' WHERE completed_at IS NULL AND created_at < ?",
		now(), cutoff,
	)
	return err
}

// runLedgerReconcileLoop periodically closes open rows that exceeded the
// hard-age fuse while the process kept running (for example a blocked writer
// goroutine or a leaked finalize path).
func (a *App) runLedgerReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(ledgerReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.reconcileOpenLedgerRows(ctx); err != nil {
				a.log.Error("ledger reconcile sweep", "error", err)
			}
		}
	}
}

// ledgerRowStale reports whether an open row has outrun every plausible
// completion window and should be flagged as suspected-stalled in the console.
// providerTimeoutMS comes straight from the joined provider row (0 = unset).
func ledgerRowStale(createdAt string, providerTimeoutMS int64, serverNow time.Time) bool {
	start, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil || start.IsZero() {
		return false
	}
	limit := ledgerStaleAfterDefault
	if providerTimeoutMS > 0 {
		limit = time.Duration(providerTimeoutMS)*time.Millisecond + ledgerStaleGrace
	}
	return serverNow.Sub(start) > limit
}
