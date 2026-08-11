package fusiongate

import (
	"testing"
	"time"
)

// The request ledger is observability data written from inside the response path,
// including from the first-byte callback. It must be queued rather than written
// synchronously, otherwise SQLite latency lands directly on the client's
// time-to-first-byte.
func TestLedgerWritesAreQueuedOffTheRequestPath(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	before := a.metrics.ledgerQueued.Load()
	key := authKey{ID: 1, Name: "test", Prefix: "fg_test"}
	route := resolvedRoute{
		Route:    Route{ID: 1, PublicName: "model", UpstreamModel: "upstream"},
		Provider: Provider{ID: 1, Name: "provider"},
	}
	attemptID := a.startLedger(key, route, "openai_chat", false, "127.0.0.1", "req_queued", "", 1, "")
	a.recordFirstByte(attemptID, time.Now().Add(-30*time.Millisecond))
	a.endLedger(attemptID, route.Provider.ID, key.ID, "openai", route.Route.UpstreamModel, true, 200, "", time.Now().Add(-50*time.Millisecond), Usage{Input: 7, Output: 11, Reported: true})

	if queued := a.metrics.ledgerQueued.Load() - before; queued != 4 {
		t.Fatalf("queued ledger writes = %d, want 4 (first-byte, cycle, key spend, completion — a synchronous write would not be counted)", queued)
	}

	a.flushLedgerWrites()
	var success, input, output int64
	var firstByte, completed any
	if err := a.db.QueryRow(`SELECT success,input_tokens,output_tokens,first_byte_ms,completed_at FROM request_ledger WHERE request_id=?`, attemptID).
		Scan(&success, &input, &output, &firstByte, &completed); err != nil {
		t.Fatalf("queued ledger writes were not applied after flush: %v", err)
	}
	if success != 1 || input != 7 || output != 11 {
		t.Fatalf("ledger row = success:%d input:%d output:%d", success, input, output)
	}
	if firstByte == nil {
		t.Fatal("first byte timing was not applied")
	}
	if completed == nil {
		t.Fatal("completion was not applied")
	}
	if errs := a.metrics.ledgerWriteErrors.Load(); errs != 0 {
		t.Fatalf("ledger writer reported %d errors", errs)
	}
}

// The UPDATEs address a row by request_id, so they are only correct if the writer
// applies queued statements in FIFO order behind the INSERT that created it.
func TestLedgerWriterPreservesOrderPerAttempt(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	key := authKey{ID: 1}
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: "u"}, Provider: Provider{ID: 1}}
	for i := range 50 {
		id := a.startLedger(key, route, "openai_chat", false, "127.0.0.1", "req_order", "", i+1, "")
		a.endLedger(id, route.Provider.ID, key.ID, "openai", route.Route.UpstreamModel, true, 200, "", time.Now(), Usage{Input: int64(i), Reported: true})
	}
	a.flushLedgerWrites()
	var rows, completedRows int
	if err := a.db.QueryRow(`SELECT COUNT(*),COUNT(completed_at) FROM request_ledger WHERE gateway_request_id='req_order'`).Scan(&rows, &completedRows); err != nil {
		t.Fatal(err)
	}
	if rows != 50 {
		t.Fatalf("inserted rows = %d, want 50", rows)
	}
	if completedRows != 50 {
		t.Fatalf("rows with a completion = %d, want 50 — an UPDATE overtook its INSERT", completedRows)
	}
}

// WAL allows readers to run while the writer works, which is the entire reason the
// gateway keeps a single writer connection. Reads must therefore not share it.
func TestReadPoolIsSeparateFromTheWriterConnection(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.readDB == nil {
		t.Fatal("no read pool was opened")
	}
	if a.reader() == a.db {
		t.Fatal("reads are still going through the writer connection")
	}
	if got := a.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer MaxOpenConnections = %d, want 1", got)
	}
	if got := a.readDB.Stats().MaxOpenConnections; got < 4 {
		t.Fatalf("read pool MaxOpenConnections = %d, want at least 4", got)
	}
}

// A closed App must drain what it queued instead of losing it.
func TestCloseDrainsQueuedLedgerWrites(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := authKey{ID: 1}
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: "u"}, Provider: Provider{ID: 1}}
	for i := range 20 {
		a.startLedger(key, route, "openai_chat", false, "127.0.0.1", "req_close", "", i+1, "")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var rows int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE gateway_request_id='req_close'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 20 {
		t.Fatalf("rows persisted across shutdown = %d, want 20", rows)
	}
}
