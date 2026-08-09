package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFailoverAttemptLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxFailoverAttempts = 2
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	routes := []resolvedRoute{
		schedulerRoute(1, 1, "model", 1, 0),
		schedulerRoute(2, 2, "model", 1, 1),
		schedulerRoute(3, 3, "model", 1, 2),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	calls := 0
	a.runRoutes(rec, req, authKey{}, routes, "test", "", false, func(resolvedRoute, string, func()) attemptResult {
		calls++
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_server_error"}
	})
	if calls != 2 {
		t.Fatalf("attempts=%d, want 2", calls)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusBadGateway)
	}
	if rec.Header().Get("X-FusionGate-Attempts") != "2" {
		t.Fatalf("attempt header=%q", rec.Header().Get("X-FusionGate-Attempts"))
	}
	if !strings.Contains(rec.Body.String(), "upstream_attempt_limit") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRequestAdmissionReleasesSlot(t *testing.T) {
	a := &App{requestSlots: make(chan struct{}, 1)}
	if !a.tryAcquireRequestSlot() {
		t.Fatal("first request was rejected")
	}
	if a.tryAcquireRequestSlot() {
		t.Fatal("second request bypassed the limit")
	}
	a.releaseRequestSlot()
	if !a.tryAcquireRequestSlot() {
		t.Fatal("slot was not released")
	}
}

func TestProviderHealthFactorPenalizesUnhealthyChecks(t *testing.T) {
	healthy := providerHealthFactor(Provider{Status: "healthy", HealthCheckStatus: "healthy"})
	degraded := providerHealthFactor(Provider{Status: "degraded", HealthCheckStatus: "config_error"})
	if healthy <= degraded || degraded <= 0 {
		t.Fatalf("healthy=%v degraded=%v", healthy, degraded)
	}
}
