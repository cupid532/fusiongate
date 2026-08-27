package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStartupReconcileClosesOrphanRows(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	routes := []resolvedRoute{schedulerRoute(1, 1, "model", 1, 0)}
	a.runRoutes(rec, req, authKey{}, routes, "test", "", false, func(resolvedRoute, string, func()) attemptResult {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_server_error"}
	})
	a.flushLedgerWrites()

	var openBefore int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM request_ledger WHERE completed_at IS NULL").Scan(&openBefore); err != nil {
		t.Fatal(err)
	}

	a.reconcileStartupLedgerRows()
	a.flushLedgerWrites()

	var openAfter int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM request_ledger WHERE completed_at IS NULL").Scan(&openAfter); err != nil {
		t.Fatal(err)
	}
	if openAfter != 0 {
		t.Fatalf("open rows after startup reconcile: %d (was %d)", openAfter, openBefore)
	}
	var et string
	if err := a.db.QueryRow("SELECT COALESCE(MAX(error_type),'') FROM request_ledger").Scan(&et); err != nil {
		t.Fatal(err)
	}
	if openBefore > 0 && et != "gateway_interrupted" {
		t.Fatalf("error_type=%q, want gateway_interrupted", et)
	}
}

func TestPeriodicSweepOnlyTouchesExpiredRows(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	freshCreated := now()
	staleCreated := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	for _, tc := range []struct{ rid, created string }{
		{"fresh-1", freshCreated},
		{"stale-1", staleCreated},
	} {
		if _, err := a.db.Exec(
			"INSERT INTO request_ledger(request_id,gateway_request_id,created_at,public_model,upstream_model,protocol) VALUES(?,?,?,?,?,?)",
			tc.rid, tc.rid, tc.created, "m", "m", "openai_chat",
		); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.reconcileOpenLedgerRows(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.flushLedgerWrites()

	var freshET string
	if err := a.db.QueryRow("SELECT COALESCE(error_type,'') FROM request_ledger WHERE request_id='fresh-1'").Scan(&freshET); err != nil {
		t.Fatal(err)
	}
	if freshET != "" {
		t.Fatalf("fresh row was force-closed with %q", freshET)
	}
	var closed bool
	if err := a.db.QueryRow("SELECT completed_at IS NOT NULL FROM request_ledger WHERE request_id='stale-1'").Scan(&closed); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("expired row was not reconciled")
	}
}

func TestLedgerRowStaleThresholds(t *testing.T) {
	nowT := time.Now().UTC()
	cases := []struct {
		name      string
		createdAt time.Time
		timeoutMS int64
		wantStale bool
	}{
		{"fresh-no-timeout", nowT.Add(-5 * time.Minute), 0, false},
		{"aged-default-stale", nowT.Add(-31 * time.Minute), 0, true},
		{"within-explicit-timeout", nowT.Add(-90 * time.Second), 120_000, false},
		{"past-explicit-timeout", nowT.Add(-(121*time.Second + 90*time.Second)), 120_000, true},
		{"unparsable-created", time.Time{}, 0, false},
	}
	for _, tc := range cases {
		if got := ledgerRowStale(tc.createdAt.Format(time.RFC3339Nano), tc.timeoutMS, nowT); got != tc.wantStale {
			t.Fatalf("%s: stale=%v, want %v", tc.name, got, tc.wantStale)
		}
	}
}
