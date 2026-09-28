package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// V3.13 offers exactly one routing strategy. The endpoint still exists so the
// console can read and tune reliability parameters, but a request to store any
// other algorithm must be refused instead of quietly accepted and ignored.
func TestRoutingSettingsAcceptOnlyPriorityFailover(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("A")) }, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("B")) })
	defer done()
	// The fixture gives the first provider the higher priority. Equalise the
	// priorities so this test exercises the documented tie-break instead:
	// provider priority DESC, then sort_order ASC, then id ASC.
	if _, err := a.db.Exec(`UPDATE providers SET priority=0,sort_order=CASE id WHEN 1 THEN 2 ELSE 1 END`); err != nil {
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

	// Every historical algorithm is rejected with a clear error rather than
	// being stored as a value the router no longer reads.
	for _, strategy := range []string{"ordered_round_robin", "smart_round_robin", "adaptive"} {
		patch := adminRequest("PATCH", `{"strategy":"`+strategy+`"}`)
		if patch.Code != http.StatusBadRequest || !strings.Contains(patch.Body.String(), "only priority_failover is supported") {
			t.Fatalf("patch %s => %d %s", strategy, patch.Code, patch.Body.String())
		}
	}

	// Accepting the supported strategy keeps the ordering the gateway actually
	// uses: provider priority DESC, then sort_order ASC, then id ASC.
	patch := adminRequest("PATCH", `{"strategy":"priority_failover"}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch %d %s", patch.Code, patch.Body.String())
	}
	read := adminRequest("GET", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), string(StrategyPriorityFailover)) {
		t.Fatalf("strategy readback %d %s", read.Code, read.Body.String())
	}

	// Both providers share priority 0 and the same public model, so the sort
	// order decides: provider 2 (sort_order 1) must serve every request.
	got := []string{}
	for range 4 {
		rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
		if rec.Code != 200 {
			t.Fatalf("inference %d %s", rec.Code, rec.Body.String())
		}
		got = append(got, rec.Body.String())
	}
	if want := []string{"B", "B", "B", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered channels got=%v want=%v", got, want)
	}

	a.flushLedgerWrites()
	var saved string
	if err := a.db.QueryRow(`SELECT routing_strategy FROM request_ledger ORDER BY id DESC LIMIT 1`).Scan(&saved); err != nil || saved != string(StrategyPriorityFailover) {
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
	if reopened.globalRoutingStrategy() != StrategyPriorityFailover {
		t.Fatalf("strategy not persisted across restart: %s", reopened.globalRoutingStrategy())
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
