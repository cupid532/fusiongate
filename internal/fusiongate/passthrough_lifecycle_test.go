package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPassthroughLastHTTPErrorSurvivesTransportFailure(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Error", "one")
		w.Header().Add("X-Error", "two")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(503)
		w.Write([]byte("raw last upstream error"))
	}, func(w http.ResponseWriter, r *http.Request) { h := w.(http.Hijacker); c, _, _ := h.Hijack(); c.Close() })
	defer done()
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 503 || rec.Body.String() != "raw last upstream error" || rec.Header().Get("Retry-After") != "7" || len(rec.Header().Values("X-Error")) != 2 {
		t.Fatalf("response %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	a.flushLedgerWrites()
	var attempts int
	// Each channel gets its own attempt budget (three attempts by default
	// including the first), so the 503 channel is retried three times and the
	// unreachable one three times before the retained 503 is forwarded.
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE attempt>0`).Scan(&attempts); err != nil || attempts != 6 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestPassthroughMultiKeyVisitsOtherChannelFirst(t *testing.T) {
	// V3.13 has a single strategy; the loop remains so the test still documents
	// that the channel-major key walk is strategy-independent.
	for _, strategy := range []RoutingStrategy{StrategyPriorityFailover} {
		t.Run(string(strategy), func(t *testing.T) {
			var mu sync.Mutex
			calls := []string{}
			handler := func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.Header.Get("Authorization"))
				mu.Unlock()
				w.WriteHeader(401)
				w.Write([]byte("native error"))
			}
			a, key, done := passthroughFixture(t, handler, handler)
			defer done()
			if _, err := a.db.Exec(`UPDATE settings SET value=? WHERE key='routing_strategy'`, strategy); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1`); err != nil {
				t.Fatal(err)
			}
			insertProviderKeyForTest(t, a, 1, "a1", "a1", "", providerKeyEgressInherit, nil, 1, 0)
			insertProviderKeyForTest(t, a, 1, "a2", "a2", "", providerKeyEgressInherit, nil, 1, 1)
			insertProviderKeyForTest(t, a, 2, "b1", "b1", "", providerKeyEgressInherit, nil, 1, 0)
			rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
			mu.Lock()
			got := append([]string(nil), calls...)
			mu.Unlock()
			// A channel keeps its credentials together: both of channel A's Keys
			// are isolated before the request advances to channel B, which then
			// answers with its own 401.
			if rec.Code != 401 || !reflect.DeepEqual(got, []string{"Bearer a1", "Bearer a2", "Bearer b1"}) {
				t.Fatalf("status=%d calls=%v", rec.Code, got)
			}
			a.flushLedgerWrites()
			var count int
			if err := a.db.QueryRow(`SELECT candidate_count FROM request_ledger LIMIT 1`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("candidate_count=%d err=%v", count, err)
			}
		})
	}
}

func TestPassthroughLedgerStaysRunningUntilStreamEnds(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(": heartbeat\n\n"))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Write([]byte("event: future\ndata: opaque\n\n"))
	})
	defer done()
	gatewayDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		gatewayDone <- gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	a.flushLedgerWrites()
	var completed string
	if err := a.db.QueryRow(`SELECT COALESCE(completed_at,'') FROM request_ledger LIMIT 1`).Scan(&completed); err != nil || completed != "" {
		close(release)
		t.Fatalf("closed before stream ended: %q %v", completed, err)
	}
	if a.providerInflight(1) != 1 {
		close(release)
		t.Fatal("inflight released before stream ended")
	}
	close(release)
	select {
	case rec := <-gatewayDone:
		if rec.Body.String() != ": heartbeat\n\nevent: future\ndata: opaque\n\n" {
			t.Fatalf("body=%q", rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not finish")
	}
	a.flushLedgerWrites()
	var success, reported int
	var costType string
	if err := a.db.QueryRow(`SELECT success,usage_reported,cost_type FROM request_ledger LIMIT 1`).Scan(&success, &reported, &costType); err != nil || success != 1 || reported != 0 || costType != "unknown" {
		t.Fatalf("ledger=%d %d %s err=%v", success, reported, costType, err)
	}
	if a.providerInflight(1) != 0 {
		t.Fatal("inflight leaked")
	}
}

func TestPassthroughActiveStreamOutlivesProviderTotalTimeout(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 8; i++ {
			w.Write([]byte(":x\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	defer done()
	a.cfg.StreamStartTimeout = time.Second
	a.cfg.StreamIdleTimeout = time.Second
	if _, err := a.db.Exec(`UPDATE providers SET request_timeout_ms=50`); err != nil {
		t.Fatal(err)
	}
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
	if rec.Code != 200 || rec.Body.String() != strings.Repeat(":x\n\n", 8) {
		t.Fatalf("stream truncated: %d %q", rec.Code, rec.Body.String())
	}
}

func TestPassthroughIdleStreamDoesNotSwitchAfterCommit(t *testing.T) {
	var backup atomic.Int32
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(":x\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); w.Write([]byte("wrong channel")) })
	defer done()
	a.cfg.StreamStartTimeout = time.Second
	a.cfg.StreamIdleTimeout = 40 * time.Millisecond
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
	if rec.Body.String() != ":x\n\n" || backup.Load() != 0 {
		t.Fatalf("spliced response=%q backup=%d", rec.Body.String(), backup.Load())
	}
	a.flushLedgerWrites()
	var success int
	var stop string
	if err := a.db.QueryRow(`SELECT success,stop_reason FROM request_ledger LIMIT 1`).Scan(&success, &stop); err != nil || success != 0 || stop != "response_interrupted" {
		t.Fatalf("success=%d stop=%q err=%v", success, stop, err)
	}
}

func TestPassthroughCancellationStopsNewAttempts(t *testing.T) {
	started := make(chan struct{})
	var backup atomic.Int32
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}, func(w http.ResponseWriter, r *http.Request) { backup.Add(1) })
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"native"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key)
	finished := make(chan struct{})
	go func() { a.Router().ServeHTTP(httptest.NewRecorder(), req); close(finished) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no upstream request")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop request")
	}
	if backup.Load() != 0 {
		t.Fatal("backup called after cancellation")
	}
}

func TestPassthroughMultipartAndBinary(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("model", "native")
	part, _ := form.CreateFormFile("file", "x.bin")
	part.Write([]byte{0, 255, 1, 254})
	form.Close()
	raw := append([]byte(nil), body.Bytes()...)
	output := []byte{0, 1, 255, 254}
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, raw) || r.Header.Get("Content-Type") != form.FormDataContentType() {
			t.Error("multipart changed")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(output)
	})
	defer done()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), output) {
		t.Fatalf("binary changed %d %v", rec.Code, rec.Body.Bytes())
	}
}

func TestPassthroughErrorResourceBoundPreservesBytes(t *testing.T) {
	var backup atomic.Int32
	body := bytes.Repeat([]byte("e"), maxPassthroughError+123)
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); w.Write(body) }, func(w http.ResponseWriter, r *http.Request) { backup.Add(1) })
	defer done()
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 503 || !bytes.Equal(rec.Body.Bytes(), body) || backup.Load() != 0 {
		t.Fatalf("resource fallback status=%d bytes=%d backup=%d", rec.Code, rec.Body.Len(), backup.Load())
	}
}

func TestPassthroughAdminCountsKeysAndCooldowns(t *testing.T) {
	a, _, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {}, func(w http.ResponseWriter, r *http.Request) {})
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET enabled=0 WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.routes(rec, httptest.NewRequest("GET", "/api/admin/routes", nil), adminCtx{})
	var routes []Route
	if err := json.Unmarshal(rec.Body.Bytes(), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].EligibleProviderCount != 1 || !strings.Contains(routes[0].RoutingWarning, "无备用") {
		t.Fatalf("routes=%+v", routes)
	}
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	a.routes(rec, httptest.NewRequest("GET", "/api/admin/routes", nil), adminCtx{})
	if err := json.Unmarshal(rec.Body.Bytes(), &routes); err != nil {
		t.Fatal(err)
	}
	if routes[0].EligibleProviderCount != 0 {
		t.Fatalf("provider without keys counted: %+v", routes)
	}
}
