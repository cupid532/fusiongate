package fusiongate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInferenceChannelBudgetReservesBackup(t *testing.T) {
	for _, tc := range []struct{ configured, identities, remaining, later, want int }{
		{3, 9, 9, 1, 8}, {3, 1, 1, 1, 1}, {3, 1, 9, 1, 3},
		{3, 1, 2, 3, 1}, {3, 9, 0, 1, 0},
	} {
		if got := inferenceChannelBudget(tc.configured, tc.identities, tc.remaining, tc.later); got != tc.want {
			t.Fatalf("budget=%d want=%d for %+v", got, tc.want, tc)
		}
	}
	if got := inferenceCredentialCount([]resolvedRoute{{ProviderKeyID: 0}, {ProviderKeyID: 0}, {ProviderKeyID: 4}, {ProviderKeyID: 4}}); got != 2 {
		t.Fatalf("identities=%d want=2", got)
	}
}

func TestInferenceManyBrokenKeysStillReachBackup(t *testing.T) {
	var bad, backup atomic.Int32
	a, key, done := passthroughFixture(t,
		func(w http.ResponseWriter, r *http.Request) { bad.Add(1); w.WriteHeader(http.StatusUnauthorized) },
		func(w http.ResponseWriter, r *http.Request) { backup.Add(1); _, _ = w.Write([]byte("backup")) },
	)
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		insertProviderKeyForTest(t, a, 1, fmt.Sprintf("failed-%d", i), fmt.Sprint(i), "", providerKeyEgressInherit, nil, 1, i)
	}
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 200 || rec.Body.String() != "backup" || backup.Load() != 1 || bad.Load() > 8 {
		t.Fatalf("status=%d body=%q bad=%d backup=%d", rec.Code, rec.Body.String(), bad.Load(), backup.Load())
	}
}

func TestInferenceChannelWindowBoundsUnresponsiveAttempt(t *testing.T) {
	var backup atomic.Int32
	a, key, done := passthroughFixture(t,
		func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() },
		func(w http.ResponseWriter, r *http.Request) { backup.Add(1); _, _ = w.Write([]byte("backup")) },
	)
	defer done()
	if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('inference_settings','{"channel_window_seconds":1,"failover_window_seconds":5}') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 200 || rec.Body.String() != "backup" || backup.Load() != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("status=%d body=%q backup=%d elapsed=%s", rec.Code, rec.Body.String(), backup.Load(), time.Since(start))
	}
	a.flushLedgerWrites()
	var reason string
	if err := a.db.QueryRow(`SELECT error_type FROM request_ledger WHERE attempt=1`).Scan(&reason); err != nil || reason != "upstream_timeout" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
}

func TestInferenceStalledErrorBodyReachesBackup(t *testing.T) {
	var backup atomic.Int32
	a, key, done := passthroughFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
		func(w http.ResponseWriter, r *http.Request) { backup.Add(1); _, _ = w.Write([]byte("backup")) },
	)
	defer done()
	if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('inference_settings','{"channel_window_seconds":1,"failover_window_seconds":5}') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 200 || rec.Body.String() != "backup" || backup.Load() != 1 {
		t.Fatalf("status=%d body=%q backup=%d", rec.Code, rec.Body.String(), backup.Load())
	}
}

func TestInferenceRetryWaitDoesNotStartExpiredAttempt(t *testing.T) {
	var bad, backup atomic.Int32
	a, key, done := passthroughFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			bad.Add(1)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(503)
		},
		func(w http.ResponseWriter, r *http.Request) { backup.Add(1); _, _ = w.Write([]byte("backup")) },
	)
	defer done()
	if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('inference_settings','{"channel_window_seconds":1,"failover_window_seconds":5}') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 200 || bad.Load() != 1 || backup.Load() != 1 {
		t.Fatalf("status=%d bad=%d backup=%d", rec.Code, bad.Load(), backup.Load())
	}
}

func TestInferenceWindowStopsAtCommitNotStreamDuration(t *testing.T) {
	ctx, guard := newInferenceAttemptWindow(context.Background(), time.Now().Add(50*time.Millisecond))
	out := &inferenceWindowWriter{ResponseWriter: httptest.NewRecorder(), guard: guard}
	out.WriteHeader(200)
	select {
	case <-ctx.Done():
		t.Fatal("committed response canceled by routing window")
	case <-time.After(100 * time.Millisecond):
	}
	if expired, committed := guard.close(); expired || !committed {
		t.Fatal("committed guard marked expired or not committed")
	}
	ctx, guard = newInferenceAttemptWindow(context.Background(), time.Now().Add(20*time.Millisecond))
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("uncommitted attempt did not expire")
	}
	if expired, committed := guard.close(); !expired || committed {
		t.Fatal("expired guard not marked or falsely committed")
	}
	rec := httptest.NewRecorder()
	out = &inferenceWindowWriter{ResponseWriter: rec, guard: guard}
	out.WriteHeader(201)
	if n, err := out.Write([]byte("late")); n != 0 || err != context.DeadlineExceeded {
		t.Fatalf("late write n=%d err=%v", n, err)
	}
	out.Flush()
	if rec.Body.Len() != 0 || rec.Flushed || rec.Code != 200 {
		t.Fatalf("expired attempt committed late output: %+v", rec)
	}
}

func TestInferenceExpiredAttemptHeadersAreRestored(t *testing.T) {
	header := http.Header{"Vary": {"Origin", "Accept"}}
	header.Set("X-FusionGate-Request-ID", "gateway")
	saved := header.Clone()
	header.Set("Content-Encoding", "gzip")
	header.Set("Content-Length", "999")
	header.Set("X-Upstream", "expired")
	restoreInferenceHeaders(header, saved)
	if header.Get("Content-Encoding") != "" || header.Get("Content-Length") != "" || header.Get("X-Upstream") != "" || header.Get("X-FusionGate-Request-ID") != "gateway" || len(header.Values("Vary")) != 2 {
		t.Fatalf("restored headers=%v", header)
	}
	header["Vary"][0] = "changed"
	if saved.Values("Vary")[0] != "Origin" {
		t.Fatal("snapshot shares header values")
	}
}

func TestInferenceActiveStreamOutlivesRoutingWindows(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte(":heartbeat\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(220 * time.Millisecond)
		}
	})
	defer done()
	if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('inference_settings','{"channel_window_seconds":1,"failover_window_seconds":1}') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
	if rec.Code != 200 || rec.Body.String() != strings.Repeat(":heartbeat\n\n", 6) {
		t.Fatalf("stream truncated status=%d body=%q", rec.Code, rec.Body.String())
	}
}
