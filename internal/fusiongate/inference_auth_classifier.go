package fusiongate

import (
	"encoding/json"
	"net/http"
	"strings"
)

// inferenceHTTPResponseResult is shared by native and bridged inference sends.
// Only explicit auth evidence in a bounded JSON error envelope overrides the
// HTTP status. The peek retains the original (possibly compressed) wire bytes.
func inferenceHTTPResponseResult(resp *http.Response) attemptResult {
	if resp == nil {
		return attemptResult{}
	}
	result := attemptResult{
		Status: resp.StatusCode, Response: resp,
		Retryable:  retryableStatus(resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
	if resp.StatusCode < 400 || resp.Body == nil {
		return result
	}
	switch inferenceStructuredErrorReason(protocolErrorPayload(resp)) {
	case "upstream_protocol_denied":
		// A protocol policy denial is not a broken credential and must not be
		// evaded by trying a different credential or channel.
		result.Retryable, result.Reason = false, "upstream_protocol_denied"
	case "upstream_auth_error":
		result.Retryable, result.Reason = true, "upstream_auth_error"
	}
	return result
}

// inferenceStructuredErrorReason intentionally does not recurse or search the
// serialized body: request echoes, arrays, prose and unrelated nested metadata
// are not authentication evidence. OAuth's top-level error string and common
// API error objects/top-level error fields are the only supported envelopes.
func inferenceStructuredErrorReason(body []byte) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top == nil {
		return ""
	}

	envelopes := []map[string]json.RawMessage{top}
	var nested map[string]json.RawMessage
	if json.Unmarshal(top["error"], &nested) == nil && nested != nil {
		envelopes = append(envelopes, nested)
	}
	auth, invalidRequest := false, false
	for _, envelope := range envelopes {
		for _, field := range []string{"error", "code", "type", "status", "reason", "error_code"} {
			value := inferenceErrorString(envelope[field])
			if value == "gateway_protocol_policy_denied" {
				return "upstream_protocol_denied"
			}
			if inferenceAuthCode(value) {
				auth = true
			}
			// A concrete malformed-request code wins over incidental auth prose.
			// invalid_request_error as a generic type is not decisive: several
			// APIs wrap invalid_api_key in that type.
			if (field == "error" || field == "code" || field == "error_code") && value == "invalid_request" {
				invalidRequest = true
			}
		}
	}
	if invalidRequest {
		return ""
	}
	if auth {
		return "upstream_auth_error"
	}
	for _, envelope := range envelopes {
		for _, field := range []string{"message", "error_description", "error"} {
			if inferenceAuthMessage(inferenceErrorString(envelope[field])) {
				return "upstream_auth_error"
			}
		}
	}
	return ""
}

func inferenceErrorString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func inferenceAuthCode(value string) bool {
	switch value {
	case "auth_unavailable", "no_auth_available", "invalid_grant", "token_expired", "expired_token",
		"invalid_token", "token_revoked", "invalid_api_key", "invalid_authentication_token",
		"authentication_error", "authentication_failed", "auth_expired", "credentials_expired":
		return true
	}
	return false
}

func inferenceAuthMessage(value string) bool {
	// Allow an exact canonical message or an explicit leading error identifier
	// followed by colon-delimited detail, never a token appearing in arbitrary
	// prose such as "invalid parameter: token_expired" or a request echo.
	prefix, _, _ := strings.Cut(value, ":")
	prefix = strings.TrimSpace(strings.TrimRight(prefix, ".!"))
	if inferenceAuthCode(prefix) {
		return true
	}
	switch prefix {
	case "no auth available", "token expired", "access token expired", "the access token has expired",
		"authentication failed", "invalid api key", "invalid access token", "credentials expired":
		return true
	}
	return false
}

// inferenceCredentialFailure provides one attribution rule for request-local
// isolation, key cooldowns and provider health, even when auth arrived as 400/500.
func inferenceCredentialFailure(result attemptResult) bool {
	if isNeutralResult(result) {
		return false
	}
	return result.Reason == "upstream_auth_error" || result.Reason == "auth_expired" ||
		result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden
}
