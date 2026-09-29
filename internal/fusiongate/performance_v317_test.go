package fusiongate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRouteLockNeverWaitsForHealthDatabase(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "lock", "openai", "https://example.com", "secret", 1, 1, "normalized", "any", 0, 5, 30)
	a.flushLedgerWrites()
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			conn.Close()
		}
	}()
	done := make(chan struct{})
	go func() {
		a.completeRoute(resolvedRoute{Provider: Provider{ID: id}, ProviderKeyID: 1}, attemptResult{Status: 200}, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		conn.Close()
		released = true
		t.Fatal("health completion waited for SQLite")
	}
	lock := make(chan struct{})
	go func() { a.routeMu.Lock(); a.routeMu.Unlock(); close(lock) }()
	select {
	case <-lock:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("route mutex held by database wait")
	}
	conn.Close()
	released = true
	a.flushLedgerWrites()
	// A stale persistence operation cannot override a newer health revision.
	_, err = a.db.Exec(`UPDATE providers SET status='degraded',health_revision=health_revision-1 WHERE id=? AND health_revision<0`, id)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err = a.db.QueryRow(`SELECT status FROM providers WHERE id=?`, id).Scan(&status); err != nil || status != "healthy" {
		t.Fatal(status, err)
	}
}

func TestFirstByteAndCompletionIgnoreSaturatedMetadataQueue(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	keyText := insertTestKey(t, a, true)
	_ = keyText
	providerID := insertTestProvider(t, a, "ledger", "openai", "https://example.com", "secret", 1, 1, "normalized", "any", 0, 5, 30)
	routeID := insertTestRoute(t, a, providerID, "m", "m", "chat", 0)
	route := resolvedRoute{Provider: Provider{ID: providerID}, Route: Route{ID: routeID, PublicName: "m", UpstreamModel: "m"}}
	a.flushLedgerWrites()
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			conn.Close()
		}
	}()
	id := a.startLedger(authKey{ID: 1}, route, "openai_chat", true, "127.0.0.1", "reserved", "", 1, "")
	for len(a.ledgerGeneralSlots) < cap(a.ledgerGeneralSlots) {
		a.queueLedgerWrite(`SELECT 1`)
	}
	started := time.Now()
	a.recordFirstByte(id, started)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("first-byte path blocked")
	}
	done := make(chan struct{})
	go func() {
		a.endLedger(id, providerID, 1, "openai", "m", true, 200, "", started, Usage{CostType: "actual", CostMicros: 17, Reported: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		conn.Close()
		released = true
		t.Fatal("reserved final accounting blocked by metadata")
	}
	conn.Close()
	released = true
	a.flushLedgerWrites()
	var success, spent int
	var first int64
	if err = a.db.QueryRow(`SELECT success,first_byte_ms FROM request_ledger WHERE request_id=?`, id).Scan(&success, &first); err != nil || success != 1 {
		t.Fatal(success, first, err)
	}
	if err = a.db.QueryRow(`SELECT spent_micros FROM api_keys WHERE id=1`).Scan(&spent); err != nil || spent != 17 {
		t.Fatal(spent, err)
	}
	if len(a.ledgerAttemptSlots) != 0 {
		t.Fatal("completion reservation leaked")
	}
}

func TestLedgerAdmissionBackpressureIsCancellable(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < cap(a.ledgerAttemptSlots); i++ {
		if !a.reserveLedgerCompletion(context.Background()) {
			t.Fatal("reserve")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.reserveLedgerCompletion(ctx) }()
	select {
	case <-done:
		t.Fatal("unbounded admission")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("admitted canceled request")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation stuck")
	}
	for len(a.ledgerAttemptSlots) > 0 {
		<-a.ledgerAttemptSlots
	}
}

func TestConcurrentGatewayDiagnosticsAndOverhead(t *testing.T) {
	for _, concurrency := range []int{1, 8, 32} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			})
			defer done()
			var wg sync.WaitGroup
			var mu sync.Mutex
			var elapsed []time.Duration
			for worker := 0; worker < concurrency; worker++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					for i := worker; i < 100; i += concurrency {
						started := time.Now()
						rec := bridgeRequest(t, a, key, "/v1/chat/completions", `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
						if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[DONE]") {
							t.Errorf("status %d", rec.Code)
						}
						mu.Lock()
						elapsed = append(elapsed, time.Since(started))
						mu.Unlock()
					}
				}(worker)
			}
			wg.Wait()
			a.flushLedgerWrites()
			sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
			t.Logf("concurrency=%d requests=%d total-latency-p95=%s", concurrency, len(elapsed), elapsed[94])
			var count int
			if err := a.db.QueryRow(`SELECT count(*) FROM request_ledger WHERE success=1 AND json_extract(diagnostics_json,'$.termination_reason')='completed' AND json_extract(diagnostics_json,'$.first_output_ms') IS NOT NULL`).Scan(&count); err != nil || count != 100 {
				t.Fatal(count, err)
			}
			if a.providerInflight(1) != 0 || len(a.ledgerAttemptSlots) != 0 {
				t.Fatal("resource leak")
			}
			// First-byte overhead is measured via transport diagnostics, not total model time.
			rows, err := a.db.Query(`SELECT first_byte_ms FROM request_ledger ORDER BY first_byte_ms`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var timings []int64
			for rows.Next() {
				var n int64
				rows.Scan(&n)
				timings = append(timings, n)
			}
			if timings[94] > 100 {
				t.Fatalf("local first-byte p95=%dms exceeds 100ms", timings[94])
			}
		})
	}
}

func TestLedgerCapacitySumsAllRows(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 2; i++ {
		_, err = a.db.Exec(`INSERT INTO request_ledger(request_id,public_model,created_at,upstream_model,protocol) VALUES(?,?,?,'m','test')`, fmt.Sprint(i), strings.Repeat("x", 1000), now())
		if err != nil {
			t.Fatal(err)
		}
	}
	bytes, rows, err := a.ledgerUsage()
	if err != nil || rows != 2 || bytes < 2000+2*ledgerRowOverhead {
		t.Fatal(bytes, rows, err)
	}
}

func TestDiagnosticWriterDoesNotTreatHeartbeatAsOutput(t *testing.T) {
	d := &attemptDiagnostics{start: time.Now()}
	w := &diagnosticWriter{ResponseWriter: httptest.NewRecorder(), diagnostics: d, stream: true}
	w.observer.onOutput = d.firstOutput
	w.WriteHeader(200)
	w.Write([]byte(": heartbeat\n\n"))
	if d.FirstOutputMS != nil {
		t.Fatal("heartbeat counted as output")
	}
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	if d.FirstOutputMS == nil {
		t.Fatal("output not observed")
	}
}

func TestProtocolPolicyDenialDoesNotDisableOtherAPIs(t *testing.T) {
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(403)
			g := gzip.NewWriter(w)
			fmt.Fprint(g, `{"error":{"code":"gateway_protocol_policy_denied"}}`)
			g.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/messages", `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 403 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()
	rec = bridgeRequest(t, a, key, "/v1/responses", `{"model":"public-model","stream":true,"input":"hi"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()
	var attempts, faults int
	a.db.QueryRow(`SELECT count(*) FROM inference_attempts`).Scan(&attempts)
	a.db.QueryRow(`SELECT count(*) FROM recovery_faults`).Scan(&faults)
	if attempts != 2 || faults != 0 {
		t.Fatal(attempts, faults)
	}
}

func TestActiveLongStreamIsNotReconciledAsAbandoned(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := a.startLedger(authKey{}, resolvedRoute{}, "openai_chat", true, "", "active-long", "", 1, "")
	a.flushLedgerWrites()
	_, err = a.db.Exec(`UPDATE request_ledger SET created_at=? WHERE request_id=?`, time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.reconcileOpenLedgerRows(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ended bool
	a.db.QueryRow(`SELECT completed_at IS NOT NULL FROM request_ledger WHERE request_id=?`, id).Scan(&ended)
	if ended {
		t.Fatal("active stream force-closed")
	}
	a.endLedger(id, 0, 0, "", "", true, 200, "", time.Now(), Usage{})
}

func TestCompressedProtocolErrorsPreserveBytesAndScope(t *testing.T) {
	for _, tc := range []struct {
		status              int
		code                string
		unsupported, denied bool
	}{
		{400, "gateway_provider_protocol_unavailable", true, false}, {403, "gateway_protocol_policy_denied", false, true}, {400, "unknown_parameter", false, false},
	} {
		var wire bytes.Buffer
		g := gzip.NewWriter(&wire)
		fmt.Fprintf(g, `{"error":{"code":%q}}`, tc.code)
		g.Close()
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(bytes.NewReader(wire.Bytes()))}
		if protocolUnsupportedSignal(resp) != tc.unsupported || protocolPolicyDenied(resp) != tc.denied {
			t.Fatal(tc)
		}
		remaining, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(remaining, wire.Bytes()) {
			t.Fatal("compressed bytes altered")
		}
	}
}

func TestHealthPersistenceRejectsReorderedSnapshots(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "health-order", "openai", "https://example.com", "secret", 1, 1, "normalized", "any", 0, 1, 30)
	z := resolvedRoute{Provider: Provider{ID: id, FailureThreshold: 1}}
	var old, new []ledgerWrite
	collect := func(dest *[]ledgerWrite) func(string, ...any) {
		return func(q string, args ...any) { *dest = append(*dest, ledgerWrite{query: q, args: args}) }
	}
	a.completeRouteWithWriter(z, attemptResult{Status: 503, Reason: "upstream_server_error"}, time.Second, collect(&old))
	a.completeRouteWithWriter(z, attemptResult{Status: 200}, time.Millisecond, collect(&new))
	for _, batch := range [][]ledgerWrite{new, old} {
		for _, w := range batch {
			a.queueLedgerWrite(w.query, w.args...)
		}
	}
	a.flushLedgerWrites()
	var state string
	var failures int
	if err = a.db.QueryRow(`SELECT status,consecutive_failures FROM providers WHERE id=?`, id).Scan(&state, &failures); err != nil || state != "healthy" || failures != 0 {
		t.Fatal(state, failures, err)
	}
}

func TestSlowWriterDoesNotDelayInFlightStreamCompletion(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	defer done()
	finished := make(chan int, 1)
	go func() {
		rec := bridgeRequest(t, a, key, "/v1/chat/completions", `{"model":"public-model","stream":true,"messages":[]}`)
		finished <- rec.Code
	}()
	<-entered
	a.flushLedgerWrites()
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for len(a.ledgerGeneralSlots) < cap(a.ledgerGeneralSlots) {
		a.queueLedgerWrite(`SELECT 1`)
	}
	close(release)
	select {
	case status := <-finished:
		if status != 200 {
			t.Error(status)
		}
	case <-time.After(time.Second):
		t.Error("in-flight stream waited for saturated SQLite writer")
	}
	conn.Close()
	a.flushLedgerWrites()
}

func TestProviderStreamTimeoutConfigurationRoundTrip(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rec := httptest.NewRecorder()
	a.providers(rec, httptest.NewRequest("POST", "/api/admin/providers", strings.NewReader(`{"name":"timeouts","type":"openai_compatible","baseURL":"https://example.com","credential":"test","auto_discover":false,"stream_start_timeout_ms":45000,"stream_idle_timeout_ms":90000}`)), adminCtx{})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var id int64
	if err = a.db.QueryRow(`SELECT id FROM providers WHERE name='timeouts'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, id, "m", "m", "chat", 0)
	routes, err := a.resolve(context.Background(), "m", "chat")
	if err != nil || len(routes) != 1 {
		t.Fatal(len(routes), err)
	}
	if routes[0].Provider.StreamStartTimeoutMS != 45000 || routes[0].Provider.StreamIdleTimeoutMS != 90000 {
		t.Fatal("timeouts not loaded")
	}
	list := httptest.NewRecorder()
	a.providers(list, httptest.NewRequest("GET", "/api/admin/providers", nil), adminCtx{})
	if !strings.Contains(list.Body.String(), `"stream_start_timeout_ms":45000`) {
		t.Fatal("timeouts missing from API")
	}
	exported := httptest.NewRecorder()
	a.providerBackupExport(exported, httptest.NewRequest("POST", "/api/admin/providers/export", strings.NewReader(`{}`)), adminCtx{})
	var backup providerBackupFile
	if json.Unmarshal(exported.Body.Bytes(), &backup) != nil || len(backup.Providers) != 1 || backup.Providers[0].StreamStartTimeoutMS != 45000 {
		t.Fatal("timeouts missing from backup")
	}
	reset := httptest.NewRecorder()
	a.providerUpdate(reset, httptest.NewRequest("PATCH", "/api/admin/providers/1", strings.NewReader(`{"stream_start_timeout_ms":0,"stream_idle_timeout_ms":0}`)), id)
	if reset.Code != 200 {
		t.Fatal(reset.Code, reset.Body.String())
	}
	var start, idle int
	a.db.QueryRow(`SELECT stream_start_timeout_ms,stream_idle_timeout_ms FROM providers WHERE id=?`, id).Scan(&start, &idle)
	if start != 0 || idle != 0 {
		t.Fatal("reset did not restore inheritance")
	}
}
