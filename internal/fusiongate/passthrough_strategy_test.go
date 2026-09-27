package fusiongate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPassthroughStrategyChangesAffectRealRequests(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("A")) }, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("B")) })
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET sort_order=CASE id WHEN 1 THEN 2 ELSE 1 END`); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	a.Router().ServeHTTP(login, httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(`{"password":"correct horse battery staple"}`)))
	if login.Code != 200 {
		t.Fatalf("login status=%d", login.Code)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	adminRequest := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/routing", strings.NewReader(body))
		for _, c := range login.Result().Cookies() {
			req.AddCookie(c)
		}
		req.Header.Set("X-CSRF-Token", session.CSRF)
		rec := httptest.NewRecorder()
		a.Router().ServeHTTP(rec, req)
		return rec
	}
	cases := []struct {
		strategy RoutingStrategy
		want     []string
	}{{StrategyPriorityFailover, []string{"A", "A", "A", "A"}}, {StrategyOrderedRoundRobin, []string{"B", "B", "B", "B"}}, {StrategySmartRoundRobin, []string{"B", "A", "B", "A"}}}
	for _, tc := range cases {
		patch := adminRequest("PATCH", fmt.Sprintf(`{"strategy":%q}`, tc.strategy))
		if patch.Code != 200 {
			t.Fatalf("patch %d %s", patch.Code, patch.Body.String())
		}
		read := adminRequest("GET", "")
		if read.Code != 200 || !strings.Contains(read.Body.String(), string(tc.strategy)) {
			t.Fatalf("strategy readback %d %s", read.Code, read.Body.String())
		}
		got := []string{}
		for range tc.want {
			rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
			if rec.Code != 200 {
				t.Fatalf("inference %d %s", rec.Code, rec.Body.String())
			}
			got = append(got, rec.Body.String())
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("strategy %s got=%v want=%v", tc.strategy, got, tc.want)
		}
	}
	patch := adminRequest("PATCH", `{"strategy":"adaptive"}`)
	if patch.Code != 200 {
		t.Fatal(patch.Body.String())
	}
	counts := map[string]int{}
	for range 20 {
		rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
		counts[rec.Body.String()]++
	}
	if counts["A"] < 4 || counts["B"] < 4 {
		t.Fatalf("adaptive didn't distribute healthy peers: %v", counts)
	}
	a.flushLedgerWrites()
	var saved string
	if err := a.db.QueryRow(`SELECT routing_strategy FROM request_ledger ORDER BY id DESC LIMIT 1`).Scan(&saved); err != nil || saved != "adaptive" {
		t.Fatalf("snapshot=%s err=%v", saved, err)
	}
	cfg := a.cfg
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.globalRoutingStrategy() != StrategyAdaptive {
		t.Fatal("strategy not persisted across restart")
	}
}

func TestPassthroughAttemptCap(t *testing.T) {
	var first, second atomic.Int32
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		w.Header().Set("X-Origin", "A")
		w.WriteHeader(503)
		w.Write([]byte("first failure"))
	}, func(w http.ResponseWriter, r *http.Request) { second.Add(1); w.Write([]byte("B")) })
	defer done()
	a.cfg.MaxFailoverAttempts = 1
	rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
	if rec.Code != 503 || rec.Body.String() != "first failure" || rec.Header().Get("X-Origin") != "A" || second.Load() != 0 {
		t.Fatalf("cap status=%d body=%q second=%d", rec.Code, rec.Body.String(), second.Load())
	}
	a.flushLedgerWrites()
	var stop string
	if err := a.db.QueryRow(`SELECT stop_reason FROM request_ledger LIMIT 1`).Scan(&stop); err != nil || stop != "attempt_limit" {
		t.Fatalf("stop=%s err=%v", stop, err)
	}
}
