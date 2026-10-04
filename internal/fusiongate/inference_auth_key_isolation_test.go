package fusiongate

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestInferenceStructuredAuthIsolatesOnlyBrokenAPIKey(t *testing.T) {
	for _, status := range []int{400, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var badCalls, healthyCalls, backupCalls atomic.Int32
			a, key, done := passthroughFixture(t,
				func(w http.ResponseWriter, r *http.Request) {
					switch r.Header.Get("Authorization") {
					case "Bearer broken-key":
						badCalls.Add(1)
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
					case "Bearer healthy-key":
						healthyCalls.Add(1)
						_, _ = w.Write([]byte(`{"answer":"same channel healthy key"}`))
					default:
						t.Errorf("unexpected credential %q", r.Header.Get("Authorization"))
						w.WriteHeader(500)
					}
				},
				func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					_, _ = w.Write([]byte(`{"answer":"backup"}`))
				},
			)
			defer done()
			var providerID int64
			if err := a.db.QueryRow(`SELECT id FROM providers WHERE name='passthrough-0'`).Scan(&providerID); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
				t.Fatal(err)
			}
			badKeyID := insertProviderKeyForTest(t, a, providerID, "broken-key", "broken", "", "inherit", nil, 1, 0)
			healthyKeyID := insertProviderKeyForTest(t, a, providerID, "healthy-key", "healthy", "", "inherit", nil, 1, 1)
			for turn := 1; turn <= 2; turn++ {
				rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"native"}`, "")
				if rec.Code != 200 || rec.Body.String() != `{"answer":"same channel healthy key"}` {
					t.Fatalf("turn=%d status=%d body=%s", turn, rec.Code, rec.Body.String())
				}
			}
			if badCalls.Load() != 1 || healthyCalls.Load() != 2 || backupCalls.Load() != 0 {
				t.Fatalf("bad=%d healthy=%d backup=%d", badCalls.Load(), healthyCalls.Load(), backupCalls.Load())
			}
			a.flushLedgerWrites()
			var badStatus, healthyStatus, providerStatus string
			if err := a.db.QueryRow(`SELECT status FROM provider_api_keys WHERE id=?`, badKeyID).Scan(&badStatus); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT status FROM provider_api_keys WHERE id=?`, healthyKeyID).Scan(&healthyStatus); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT status FROM providers WHERE id=?`, providerID).Scan(&providerStatus); err != nil {
				t.Fatal(err)
			}
			if badStatus != "auth_expired" || healthyStatus != "healthy" || providerStatus != "healthy" {
				t.Fatalf("bad=%s healthy=%s provider=%s", badStatus, healthyStatus, providerStatus)
			}
			var scope string
			if err := a.db.QueryRow(`SELECT scope FROM recovery_faults WHERE provider_key_id=?`, badKeyID).Scan(&scope); err != nil {
				t.Fatal(err)
			}
			if scope != keyFaultScope(badKeyID) {
				t.Fatalf("scope=%s", scope)
			}
		})
	}
}
