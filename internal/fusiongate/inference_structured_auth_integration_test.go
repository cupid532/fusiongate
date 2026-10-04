package fusiongate

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInferenceStructuredAuthReachesHealthyBackup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"auth unavailable 400", 400, `{"error":{"code":"auth_unavailable","message":"no auth available"}}`},
		{"no auth available 500", 500, `{"error":{"message":"no auth available"}}`},
		{"invalid grant 400", 400, `{"error":"invalid_grant","error_description":"refresh revoked"}`},
		{"token expired 500", 500, `{"code":"token_expired"}`},
		{"auth beats protocol learning", 400, `{"error":{"code":"auth_unavailable","message":"unsupported protocol"}}`},
		{"401", 401, `{"error":{"message":"bad credential"}}`},
		{"403", 403, `{"error":{"message":"bad credential"}}`},
	} {
		for _, bridged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bridge=%v", tc.name, bridged), func(t *testing.T) {
				var failedCalls, backupCalls atomic.Int32
				a, key, done := passthroughFixture(t,
					func(w http.ResponseWriter, r *http.Request) {
						failedCalls.Add(1)
						if bridged && r.URL.Path != "/v1/messages" {
							t.Errorf("bridge upstream path=%s", r.URL.Path)
						}
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
					},
					func(w http.ResponseWriter, r *http.Request) {
						backupCalls.Add(1)
						_, _ = w.Write([]byte(`{"answer":"backup"}`))
					},
				)
				defer done()
				if bridged {
					if _, err := a.db.Exec(`UPDATE providers SET type='anthropic_compatible',protocol_policy='fixed',protocol_preference='messages' WHERE name='passthrough-0'`); err != nil {
						t.Fatal(err)
					}
				}
				rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"native","messages":[{"role":"user","content":"hi"}]}`, "")
				if rec.Code != 200 || rec.Body.String() != `{"answer":"backup"}` || failedCalls.Load() != 1 || backupCalls.Load() != 1 {
					t.Fatalf("status=%d body=%s failed=%d backup=%d", rec.Code, rec.Body.String(), failedCalls.Load(), backupCalls.Load())
				}
				a.flushLedgerWrites()
				var reason string
				if err := a.db.QueryRow(`SELECT error_type FROM request_ledger WHERE success=0 ORDER BY id LIMIT 1`).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != "upstream_auth_error" {
					t.Fatalf("reason=%s", reason)
				}
			})
		}
	}
}

func TestInferenceStructuredAuthLastFailurePreservesCompressedBody(t *testing.T) {
	for _, status := range []int{400, 500} {
		for _, encoding := range []string{"", "gzip"} {
			t.Run(fmt.Sprintf("%d/%s", status, encoding), func(t *testing.T) {
				body := []byte(" {\n\"error\":{\"code\":\"auth_unavailable\",\"message\":\"no auth available\"}}\n")
				if encoding == "gzip" {
					body = classifierGzip(t, body)
				}
				var calls atomic.Int32
				a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Encoding", encoding)
					w.Header().Add("X-Upstream-Detail", "first")
					w.Header().Add("X-Upstream-Detail", "second")
					w.WriteHeader(status)
					_, _ = w.Write(body)
				})
				defer done()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"native"}`))
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept-Encoding", "gzip")
				rec := httptest.NewRecorder()
				a.Router().ServeHTTP(rec, req)
				if rec.Code != status || !bytes.Equal(rec.Body.Bytes(), body) || rec.Header().Get("Content-Encoding") != encoding || len(rec.Header().Values("X-Upstream-Detail")) != 2 || calls.Load() != 1 {
					t.Fatalf("status=%d headers=%v body=%q calls=%d", rec.Code, rec.Header(), rec.Body.Bytes(), calls.Load())
				}
			})
		}
	}
}

func TestInferenceStructuredAuthDoesNotRetryInvalidRequestOrPolicyDenial(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":{"code":"invalid_request","message":"no auth available"}}`},
		{400, `{"error":{"message":"unknown model"}}`},
		{400, `{"error":{"message":"input contains invalid_grant"}}`},
		{403, `{"error":{"code":"gateway_protocol_policy_denied","type":"authentication_error"}}`},
		{500, `{"code":"gateway_protocol_policy_denied","message":"no auth available"}`},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			var failedCalls, backupCalls atomic.Int32
			a, key, done := passthroughFixture(t,
				func(w http.ResponseWriter, r *http.Request) {
					failedCalls.Add(1)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				},
				func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					_, _ = w.Write([]byte(`{"answer":"backup"}`))
				},
			)
			defer done()
			rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"native"}`, "")
			if rec.Code != tc.status || rec.Body.String() != tc.body || failedCalls.Load() != 1 || backupCalls.Load() != 0 {
				t.Fatalf("status=%d body=%s failed=%d backup=%d", rec.Code, rec.Body.String(), failedCalls.Load(), backupCalls.Load())
			}
			a.routeMu.Lock()
			for id, state := range a.providerStates {
				if state.ConsecutiveFailures != 0 || !state.CircuitOpenUntil.IsZero() {
					t.Errorf("provider %d was penalized: %+v", id, state)
				}
			}
			a.routeMu.Unlock()
		})
	}
}
