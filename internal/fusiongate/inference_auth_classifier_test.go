package fusiongate

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInferenceStructuredAuthClassifier(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
	}{
		{"nested code", `{"error":{"code":"auth_unavailable","message":"relay cannot serve"}}`, "upstream_auth_error"},
		{"nested type", `{"error":{"type":"authentication_error","message":"bad credential"}}`, "upstream_auth_error"},
		{"nested exact message", `{"error":{"message":"no auth available"}}`, "upstream_auth_error"},
		{"nested detailed message", `{"error":{"message":"auth_unavailable: no auth available"}}`, "upstream_auth_error"},
		{"top level code", `{"code":"token_expired","message":"expired"}`, "upstream_auth_error"},
		{"top level type", `{"type":"authentication_error","message":"expired"}`, "upstream_auth_error"},
		{"top level message", `{"message":"No auth available."}`, "upstream_auth_error"},
		{"OAuth error string", `{"error":"invalid_grant","error_description":"grant revoked"}`, "upstream_auth_error"},
		{"OAuth detailed description", `{"error_description":"invalid_grant: revoked"}`, "upstream_auth_error"},
		{"API key generic type", `{"error":{"type":"invalid_request_error","code":"invalid_api_key"}}`, "upstream_auth_error"},
		{"top level error code", `{"error_code":"expired_token"}`, "upstream_auth_error"},
		{"case and whitespace", `{"error":{"code":" TOKEN_EXPIRED "}}`, "upstream_auth_error"},
		{"invalid request", `{"error":{"code":"invalid_request","message":"unknown model"}}`, ""},
		{"invalid request mentioning auth", `{"error":{"code":"invalid_request","message":"no auth available"}}`, ""},
		{"invalid request conflicting type", `{"error":{"code":"invalid_request","type":"authentication_error"}}`, ""},
		{"arbitrary prose", `{"error":{"message":"the input included token_expired and no auth available"}}`, ""},
		{"parameter prefix", `{"error":{"message":"invalid parameter: token_expired"}}`, ""},
		{"request echo", `{"request":{"code":"auth_unavailable"},"error":{"message":"bad input"}}`, ""},
		{"unrelated metadata", `{"error":{"metadata":{"code":"invalid_grant"}}}`, ""},
		{"plain text", `auth_unavailable: no auth available`, ""},
		{"JSON string", `"auth_unavailable"`, ""},
		{"array", `[{"error":"invalid_grant"}]`, ""},
		{"malformed", `{"error":{"code":"auth_unavailable"}`, ""},
		{"numeric code", `{"error":{"code":401}}`, ""},
		{"generic unauthorized", `{"error":{"code":"permission_denied","message":"access forbidden"}}`, ""},
		{"policy denial", `{"error":{"code":"gateway_protocol_policy_denied"}}`, "upstream_protocol_denied"},
		{"top policy denial", `{"code":"gateway_protocol_policy_denied","error":"invalid_grant"}`, "upstream_protocol_denied"},
		{"policy overrides auth", `{"error":{"type":"auth_unavailable","code":"gateway_protocol_policy_denied","message":"no auth available"}}`, "upstream_protocol_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferenceStructuredErrorReason([]byte(tc.body)); got != tc.reason {
				t.Fatalf("reason=%q want=%q body=%s", got, tc.reason, tc.body)
			}
		})
	}
}

func classifierResponse(status int, body []byte, encoding string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{
		"Content-Encoding": []string{encoding}, "Retry-After": []string{"7"},
	}}
}

func classifierGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestInferenceAuthHTTPResultPreservesWire(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 500} {
		for _, encoding := range []string{"", "gzip"} {
			t.Run(fmt.Sprintf("%d/%s", status, encoding), func(t *testing.T) {
				body := []byte(" {\n\"error\": {\"code\":\"auth_unavailable\", \"message\":\"no auth available\"}}\n")
				if encoding == "gzip" {
					body = classifierGzip(t, body)
				}
				resp := classifierResponse(status, body, encoding)
				result := inferenceHTTPResponseResult(resp)
				if result.Status != status || result.Response != resp || result.Retryable != (status >= 400) || result.RetryAfter != 7*time.Second {
					t.Fatalf("result=%+v", result)
				}
				if status >= 400 && (result.Reason != "upstream_auth_error" || !inferenceCredentialFailure(result) || !isProviderFailure(result) || providerStatus(result) != "auth_expired" || classifyInferenceResult(result, false) != decisionRetryChannel) {
					t.Fatalf("auth was not retried and attributed to credential: %+v", result)
				}
				if status < 400 && result.Reason != "" {
					t.Fatalf("success body was classified as auth: %+v", result)
				}
				wire, err := io.ReadAll(resp.Body)
				if err != nil || !bytes.Equal(wire, body) || resp.Header.Get("Content-Encoding") != encoding {
					t.Fatalf("peek changed response bytes or encoding: error=%v headers=%v", err, resp.Header)
				}
			})
		}
	}
}

func TestInferenceAuthClassifierBoundedAndConservative(t *testing.T) {
	for _, tc := range []struct {
		name, encoding string
		body           []byte
	}{
		{"oversized wire", "", []byte(`{"error":{"code":"auth_unavailable"},"padding":"` + strings.Repeat("x", 64<<10) + `"}`)},
		{"oversized decoded", "gzip", classifierGzip(t, []byte(`{"error":"invalid_grant","padding":"`+strings.Repeat("x", 64<<10)+`"}`))},
		{"unknown encoding", "br", []byte(`{"error":"invalid_grant"}`)},
		{"invalid gzip", "gzip", []byte(`{"error":"invalid_grant"}`)},
		{"truncated JSON", "", []byte(`{"error":{"code":"auth_unavailable"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := classifierResponse(400, tc.body, tc.encoding)
			result := inferenceHTTPResponseResult(resp)
			if result.Retryable || result.Reason != "" || isProviderFailure(result) || classifyInferenceResult(result, false) != decisionReturnRaw {
				t.Fatalf("inconclusive body incorrectly retried: %+v", result)
			}
			wire, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(wire, tc.body) {
				t.Fatalf("body was altered: %v", err)
			}
		})
	}
}

func TestInferenceAuthClassifierPolicyDenialIsNeutral(t *testing.T) {
	for _, status := range []int{400, 403, 500} {
		resp := classifierResponse(status, []byte(`{"error":{"code":"gateway_protocol_policy_denied","type":"authentication_error"}}`), "")
		result := inferenceHTTPResponseResult(resp)
		if result.Retryable || result.Reason != "upstream_protocol_denied" || inferenceCredentialFailure(result) || isProviderFailure(result) || providerStatus(result) != "healthy" || classifyInferenceResult(result, false) != decisionReturnRaw {
			t.Fatalf("policy denial was treated as credential failure: %+v", result)
		}
	}
}

func TestInferenceStructuredAuthCooldownAttribution(t *testing.T) {
	for _, status := range []int{400, 500} {
		for _, keyID := range []int64{0, 91} {
			t.Run(fmt.Sprintf("%d/key=%d", status, keyID), func(t *testing.T) {
				a, err := New(testConfig(t))
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				z := resolvedRoute{Provider: Provider{ID: 17, CooldownSeconds: 30, FailureThreshold: 5}, ProviderKeyID: keyID}
				result := inferenceHTTPResponseResult(classifierResponse(status, []byte(`{"error":{"code":"auth_unavailable"}}`), ""))
				var persistedStatus string
				a.completeRouteWithWriter(z, result, time.Millisecond, func(query string, args ...any) { persistedStatus, _ = args[0].(string) })
				if persistedStatus != "auth_expired" {
					t.Fatalf("persisted status=%q", persistedStatus)
				}
				a.routeMu.Lock()
				state := a.stateForLocked(z.Provider)
				keyCooldown := a.providerKeyCooldowns[keyID]
				providerCooldown := state.CircuitOpenUntil
				a.routeMu.Unlock()
				if keyID > 0 {
					if time.Until(keyCooldown) < 4*time.Minute || !providerCooldown.IsZero() || state.ConsecutiveFailures != 0 {
						t.Fatalf("key failure affected provider or missed key cooldown: state=%+v keyCooldown=%v", state, keyCooldown)
					}
					scope, kind := faultScopeFor(z, "responses", "native", status, result.Reason)
					if scope != keyFaultScope(keyID) || kind != recoveryScopeKey {
						t.Fatalf("scope=%q kind=%q", scope, kind)
					}
				} else if time.Until(providerCooldown) < 4*time.Minute || state.ConsecutiveFailures != 1 {
					t.Fatalf("OAuth auth failure did not immediately open account circuit: %+v", state)
				}
			})
		}
	}
}
