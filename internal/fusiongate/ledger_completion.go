package fusiongate

import "context"

// Reservations bound accounting backlog independently of ordinary metadata.
// Every admitted attempt owns room for its completion before it calls upstream.
const ledgerAttemptCapacity = 128

func (a *App) reserveLedgerCompletion(ctx context.Context) bool {
	if a.ledgerAttemptSlots == nil {
		return true
	}
	select {
	case a.ledgerAttemptSlots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (a *App) queueLedgerCompletion(batch []ledgerWrite, reserved bool) {
	a.ledgerMu.RLock()
	defer a.ledgerMu.RUnlock()
	write := ledgerWrite{batch: batch}
	if reserved {
		write.release = a.ledgerAttemptSlots
	}
	if a.ledgerWrites == nil || a.ledgerClosed {
		if err := a.applyLedgerWrite(write); err != nil {
			a.log.Error("ledger completion", "error", err)
		}
		if write.release != nil {
			<-write.release
		}
		return
	}
	if !reserved {
		a.ledgerGeneralSlots <- struct{}{}
		write.release = a.ledgerGeneralSlots
	}
	a.metrics.ledgerQueued.Add(1)
	a.ledgerWrites <- write // Reserved capacity: never waits for SQLite.
}
func (a *App) applyLedgerWrite(write ledgerWrite) error {
	if len(write.batch) > 0 {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, statement := range write.batch {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if write.query != "" {
		_, err := a.db.Exec(write.query, write.args...)
		return err
	}
	return nil
}
