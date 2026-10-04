package fusiongate

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// Cooldowns can expire or be reset while a request is still running. They must
// not undo request-local authentication isolation, even with duplicate mappings.
func TestInferenceNeverReusesIsolatedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keyIDs   []int64
		isolated map[int64]bool
		wantKey  int64
		wantOK   bool
	}{
		{"OAuth credential", []int64{0}, map[int64]bool{0: true}, 0, false},
		{"single API key", []int64{10}, map[int64]bool{10: true}, 0, false},
		{"all API keys", []int64{10, 20}, map[int64]bool{10: true, 20: true}, 0, false},
		{"duplicate mappings", []int64{10, 10}, map[int64]bool{10: true}, 0, false},
		{"remaining healthy key", []int64{10, 20}, map[int64]bool{10: true}, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			var routes []resolvedRoute
			for _, id := range tc.keyIDs {
				routes = append(routes, resolvedRoute{Provider: Provider{ID: 1}, ProviderKeyID: id})
			}
			for attempt := 1; attempt <= 3; attempt++ {
				z, _, ok := a.acquireInferenceRoute(routes, tc.isolated, attempt)
				if ok != tc.wantOK || (ok && z.ProviderKeyID != tc.wantKey) {
					t.Fatalf("attempt=%d selected=%d ok=%v; want key=%d ok=%v", attempt, z.ProviderKeyID, ok, tc.wantKey, tc.wantOK)
				}
				if ok {
					a.completeRoute(z, attemptResult{Status: http.StatusOK}, 0)
				}
			}
		})
	}
}

func TestInferenceAuthenticationFailureReachesBackupChannel(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var badCalls, backupCalls atomic.Int32
			a, key, done := passthroughFixture(t,
				func(w http.ResponseWriter, r *http.Request) {
					badCalls.Add(1)
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"message":"authentication failed"}}`))
				},
				func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					_, _ = w.Write([]byte(`{"answer":"backup"}`))
				},
			)
			defer done()
			rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
			if rec.Code != http.StatusOK || rec.Body.String() != `{"answer":"backup"}` || badCalls.Load() != 1 || backupCalls.Load() != 1 {
				t.Fatalf("status=%d body=%s bad=%d backup=%d", rec.Code, rec.Body.String(), badCalls.Load(), backupCalls.Load())
			}
		})
	}
}
