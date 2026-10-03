package fusiongate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func inferencePreflightRequest(t *testing.T, a *App, key, session, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Opencode-Session", session)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

func TestInferencePreflightRejectionRecordsSafeZeroAttempt(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	})
	defer done()

	var firstProvider int64
	var baseURL string
	if err := a.db.QueryRow(`SELECT id,base_url FROM providers WHERE name='bridge'`).Scan(&firstProvider, &baseURL); err != nil {
		t.Fatal(err)
	}
	secondProvider := insertTestProvider(t, a, "second-preflight", "codex_oauth", baseURL, "preflight-upstream-credential", 0, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, secondProvider, "public-model", "upstream-model", "chat,stream", 0)

	const session = "preflight-private-session"
	const prompt = "preflight-private-prompt"
	const arguments = "preflight-private-tool-arguments"
	body := `{"model":"public-model","stream":true,"store":true,"messages":[{"role":"user","content":"` + prompt + `"},{"role":"assistant","tool_calls":[{"id":"private-call-id","type":"function","function":{"name":"lookup","arguments":"` + arguments + `"}}]}],"tools":[{"type":"function","function":{"name":"lookup","description":"preflight-private-description","parameters":{"type":"object"}}}]}`
	rec := inferencePreflightRequest(t, a, key, session, body)
	if rec.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status=%d upstream_calls=%d", rec.Code, calls.Load())
	}
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "capability_not_supported" || !strings.Contains(response.Error.Message, "store") {
		t.Fatalf("response does not identify the rejected field: %+v", response)
	}
	gatewayID := rec.Header().Get("X-FusionGate-Request-ID")
	if gatewayID == "" {
		t.Fatal("local rejection has no correlation header")
	}
	a.flushLedgerWrites()

	var rowCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("ledger rows=%d, want one rejected request", rowCount)
	}
	var requestID, storedGatewayID, completedAt, errorType, stopReason, exclusions, diagnostics, costType string
	var attempt, providerID, routeID, success, status, costMicros, usageReported, tokens, firstByte int64
	if err := a.db.QueryRow(`SELECT request_id,gateway_request_id,COALESCE(completed_at,''),attempt,COALESCE(provider_id,0),COALESCE(route_id,0),success,status_code,error_type,stop_reason,candidate_exclusions,diagnostics_json,cost_type,cost_micros,usage_reported,input_tokens+output_tokens,COALESCE(first_byte_ms,0) FROM request_ledger`).Scan(&requestID, &storedGatewayID, &completedAt, &attempt, &providerID, &routeID, &success, &status, &errorType, &stopReason, &exclusions, &diagnostics, &costType, &costMicros, &usageReported, &tokens, &firstByte); err != nil {
		t.Fatal(err)
	}
	if requestID != gatewayID+"_a0" || storedGatewayID != gatewayID || completedAt == "" || attempt != 0 || providerID != 0 || routeID != 0 || success != 0 || status != http.StatusBadRequest {
		t.Fatalf("incorrect rejection ledger identity or outcome: request=%q gateway=%q completed=%q attempt=%d provider=%d route=%d success=%d status=%d", requestID, storedGatewayID, completedAt, attempt, providerID, routeID, success, status)
	}
	if errorType != "capability_not_supported" || stopReason != errorType {
		t.Fatalf("error=%q stop=%q", errorType, stopReason)
	}
	if costType != "unknown" || costMicros != 0 || usageReported != 0 || tokens != 0 || firstByte != 0 {
		t.Fatalf("undispatched request has upstream usage or timing: cost_type=%q cost=%d reported=%d tokens=%d first_byte=%d", costType, costMicros, usageReported, tokens, firstByte)
	}
	var reasons []string
	if err := json.Unmarshal([]byte(exclusions), &reasons); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{firstProvider, secondProvider} {
		found := false
		for _, reason := range reasons {
			if strings.HasPrefix(reason, fmt.Sprintf("provider=%d:", id)) && strings.Contains(reason, "store") {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing provider %d rejection detail: %s", id, exclusions)
		}
	}
	for _, secret := range []string{key, session, prompt, arguments, "private-call-id", "preflight-private-description", "preflight-upstream-credential"} {
		if strings.Contains(rec.Body.String()+exclusions+diagnostics, secret) {
			t.Fatal("private request or credential value leaked into rejection diagnostics")
		}
	}
	for _, table := range []string{"inference_attempts", "routing_sessions"} {
		var count int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("preflight rejection wrote %d rows to %s", count, table)
		}
	}
	if a.metrics.attempts.Load() != 0 || a.metrics.failovers.Load() != 0 || a.metrics.completed.Load() != 1 || a.metrics.failures.Load() != 1 {
		t.Fatal("preflight rejection changed upstream counters or counted the request twice")
	}

	valid := inferencePreflightRequest(t, a, key, session, `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if valid.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("next valid request did not reach the first channel: status=%d calls=%d", valid.Code, calls.Load())
	}
	a.flushLedgerWrites()
	var served int64
	if err := a.db.QueryRow(`SELECT provider_id FROM request_ledger WHERE success=1 ORDER BY id DESC LIMIT 1`).Scan(&served); err != nil {
		t.Fatal(err)
	}
	if served != firstProvider {
		t.Fatalf("next request served by provider %d, want original priority provider %d", served, firstProvider)
	}
}

func TestInferencePreflightFallbackKeepsRealAttemptOnly(t *testing.T) {
	var codexCalls, relayCalls atomic.Int32
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		codexCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer done()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer relay.Close()
	relayID := insertTestProvider(t, a, "preflight-relay", "openai_compatible", relay.URL, "relay-secret", 0, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, relayID, "public-model", "public-model", "chat,stream", 0)

	rec := inferencePreflightRequest(t, a, key, "fallback-task", `{"model":"public-model","store":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || codexCalls.Load() != 0 || relayCalls.Load() != 1 {
		t.Fatalf("fallback status=%d codex=%d relay=%d", rec.Code, codexCalls.Load(), relayCalls.Load())
	}
	a.flushLedgerWrites()
	var rows, zeroAttempts, sessions int
	var exclusions string
	if err := a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN attempt=0 THEN 1 ELSE 0 END),0) FROM request_ledger`).Scan(&rows, &zeroAttempts); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT candidate_exclusions FROM request_ledger WHERE success=1`).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM routing_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || zeroAttempts != 0 || sessions != 0 || !strings.Contains(exclusions, "store") {
		t.Fatalf("fallback ledger or stickiness changed: rows=%d zero=%d sessions=%d exclusions=%s", rows, zeroAttempts, sessions, exclusions)
	}
	valid := inferencePreflightRequest(t, a, key, "fallback-task", `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if valid.Code != http.StatusOK || codexCalls.Load() != 1 || relayCalls.Load() != 1 {
		t.Fatalf("next request remained pinned below rejected channel: status=%d codex=%d relay=%d", valid.Code, codexCalls.Load(), relayCalls.Load())
	}
}

func TestInferencePreflightAfterAttemptDoesNotAddLedgerRow(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"unknown endpoint"}}`)
	})
	defer done()
	rec := inferencePreflightRequest(t, a, key, "after-attempt", `{"model":"public-model","store":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("status=%d upstream_calls=%d", rec.Code, calls.Load())
	}
	a.flushLedgerWrites()
	var rows, attempt, status int
	var stop, exclusions string
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT attempt,status_code,stop_reason,candidate_exclusions FROM request_ledger`).Scan(&attempt, &status, &stop, &exclusions); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || attempt != 1 || status != http.StatusNotFound || stop != "capability_not_supported" || !strings.Contains(exclusions, "store") {
		t.Fatalf("actual upstream row duplicated or overwritten: rows=%d attempt=%d status=%d stop=%q exclusions=%s", rows, attempt, status, stop, exclusions)
	}
}
