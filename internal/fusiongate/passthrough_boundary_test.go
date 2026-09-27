package fusiongate

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPassthroughStartTimeoutFailsOver(t *testing.T) {
	var backup atomic.Int32
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }, func(w http.ResponseWriter, r *http.Request) { backup.Add(1); w.Write([]byte("backup")) })
	defer done()
	a.cfg.StreamStartTimeout = 80 * time.Millisecond
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native","stream":true}`, "")
	if rec.Code != 200 || rec.Body.String() != "backup" || backup.Load() != 1 {
		t.Fatalf("status=%d body=%q backup=%d", rec.Code, rec.Body.String(), backup.Load())
	}
	a.flushLedgerWrites()
	var reason string
	if err := a.db.QueryRow(`SELECT error_type FROM request_ledger WHERE attempt=1`).Scan(&reason); err != nil || reason != "upstream_timeout" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
}

func TestPassthroughHTTPBoundary(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Private") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("hop or credential header leaked")
		}
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("transport added compression negotiation")
		}
		w.Header().Set("Location", target.URL)
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "private")
		w.Header().Set("Set-Cookie", "secret=value")
		w.WriteHeader(307)
		w.Write([]byte("redirect unchanged"))
	})
	defer done()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"native"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Api-Key", "client-secret")
	req.Header.Set("Proxy-Authorization", "proxy-secret")
	req.Header.Set("Connection", "X-Private")
	req.Header.Set("X-Private", "secret")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 307 || rec.Body.String() != "redirect unchanged" || rec.Header().Get("Location") != target.URL || redirected.Load() != 0 || rec.Header().Get("X-Hop") != "" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("boundary status=%d headers=%v redirect=%d", rec.Code, rec.Header(), redirected.Load())
	}
}

func TestPassthroughResponseTrailers(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Trailer", "X-Checksum")
		w.Write([]byte("opaque"))
		w.Header().Set("X-Checksum", "verified")
	})
	defer done()
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	result := rec.Result()
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil || string(body) != "opaque" || result.Trailer.Get("X-Checksum") != "verified" {
		t.Fatalf("body=%q trailer=%v err=%v", body, result.Trailer, err)
	}
}
