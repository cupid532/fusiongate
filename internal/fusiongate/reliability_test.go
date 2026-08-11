package fusiongate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func insertTestProvider(t *testing.T, a *App, name, kind, baseURL, secret string, priority, weight int, mode, policy string, maxConcurrency, failureThreshold, cooldownSeconds int) int64 {
	t.Helper()
	encrypted, err := a.encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	created := now()
	res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, name, kind, baseURL, encrypted, 1, priority, weight, "unknown", "", mode, policy, maxConcurrency, 5000, failureThreshold, cooldownSeconds, created, created)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertTestRoute(t *testing.T, a *App, providerID int64, publicModel, upstreamModel, capabilities string, priority int) int64 {
	t.Helper()
	created := now()
	res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,input_price_micros,output_price_micros,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, publicModel, providerID, upstreamModel, capabilities, 1, priority, 0, 0, created, created)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertTestKey(t *testing.T, a *App, allowImages bool) string {
	t.Helper()
	key := "fg_" + hex.EncodeToString(randomBytes(18))
	sum := sha256.Sum256([]byte(key))
	_, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "test", key[:11], hex.EncodeToString(sum[:]), 1, "", "", boolInt(allowImages), 10000, now())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func gatewayRequest(t *testing.T, a *App, path, key, body, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

func TestFailoverRecordsAttempts(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		http.Error(w, "temporary", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "backup"}}}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1}})
	}))
	defer second.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "primary", "openai_compatible", first.URL, "one", 2, 100, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "backup", "openai_compatible", second.URL, "two", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "smart", "upstream", "chat,stream", 1)
	insertTestRoute(t, a, p2, "smart", "upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)

	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"smart","messages":[{"role":"user","content":"ping"}]}`, "test-client/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d", firstCalls.Load(), secondCalls.Load())
	}
	a.flushLedgerWrites()
	rows, err := a.db.Query(`SELECT attempt,retry_reason,success FROM request_ledger ORDER BY attempt`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var attempt, success int
		var reason string
		if err := rows.Scan(&attempt, &reason, &success); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s:%d", attempt, reason, success))
	}
	if strings.Join(got, ",") != "1::0,2:upstream_server_error:1" {
		t.Fatalf("ledger attempts = %v", got)
	}
}

func TestSmoothWeightedRoundRobin(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "weight-one", "openai_compatible", "http://example.test", "one", 1, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "weight-three", "openai_compatible", "http://example.test", "two", 1, 3, "normalized", "any", 0, 3, 30)
	routes := []resolvedRoute{
		{Route: Route{ID: 1, ProviderID: p1, Priority: 1}, Provider: Provider{ID: p1, Priority: 1, Weight: 1}},
		{Route: Route{ID: 2, ProviderID: p2, Priority: 1}, Provider: Provider{ID: p2, Priority: 1, Weight: 3}},
	}
	counts := map[int64]int{}
	for i := 0; i < 400; i++ {
		z, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyAdaptive)
		if !ok {
			t.Fatal("no route")
		}
		counts[z.Provider.ID]++
		a.routeMu.Lock()
		a.providerStates[z.Provider.ID].Inflight--
		a.routeMu.Unlock()
	}
	if counts[p1] != 100 || counts[p2] != 300 {
		t.Fatalf("weighted counts = %v", counts)
	}
}

func TestCircuitBreakerHalfOpenRecovery(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "circuit", "openai_compatible", "http://example.test", "x", 1, 1, "normalized", "any", 0, 2, 1)
	z := resolvedRoute{Route: Route{ID: 10, ProviderID: providerID, Priority: 1}, Provider: Provider{ID: providerID, Priority: 1, Weight: 1, FailureThreshold: 2, CooldownSeconds: 1}}
	for i := 0; i < 2; i++ {
		picked, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover)
		if !ok {
			t.Fatal("expected route before threshold")
		}
		a.completeRoute(picked, attemptResult{Status: 500, Retryable: true, Reason: "upstream_server_error"}, time.Millisecond)
	}
	if _, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover); ok {
		t.Fatal("open circuit was selected")
	}
	a.routeMu.Lock()
	a.providerStates[providerID].CircuitOpenUntil = time.Now().Add(-time.Millisecond)
	a.routeMu.Unlock()
	probe, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover)
	if !ok {
		t.Fatal("half-open probe not allowed")
	}
	if _, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover); ok {
		t.Fatal("second half-open probe was allowed")
	}
	a.completeRoute(probe, attemptResult{Status: 200, Handled: true}, time.Millisecond)
	if _, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover); !ok {
		t.Fatal("circuit did not recover after successful probe")
	}
}

func TestConcurrencyLimitFallsBack(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "limited", "openai_compatible", "http://example.test", "one", 1, 1, "normalized", "any", 1, 3, 30)
	p2 := insertTestProvider(t, a, "spare", "openai_compatible", "http://example.test", "two", 2, 1, "normalized", "any", 1, 3, 30)
	routes := []resolvedRoute{
		{Route: Route{ID: 1, ProviderID: p1, Priority: 1}, Provider: Provider{ID: p1, Priority: 1, Weight: 1, MaxConcurrency: 1}},
		{Route: Route{ID: 2, ProviderID: p2, Priority: 1}, Provider: Provider{ID: p2, Priority: 2, Weight: 1, MaxConcurrency: 1}},
	}
	first, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyPriorityFailover)
	if !ok || first.Provider.ID != p1 {
		t.Fatalf("first route = %#v", first)
	}
	second, _, ok := a.acquireRoute(routes, map[int64]bool{}, StrategyPriorityFailover)
	if !ok || second.Provider.ID != p2 {
		t.Fatalf("fallback route = %#v", second)
	}
}

func TestTransparentBodyAndHeadersArePreserved(t *testing.T) {
	raw := []byte(`{ "model" : "same-model", "unknown" : {"b":2,"a":1}, "stream": false }`)
	var upstreamBody []byte
	var upstreamUA, upstreamCustom, upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamUA = r.Header.Get("User-Agent")
		upstreamCustom = r.Header.Get("X-Client-Feature")
		upstreamAuth = r.Header.Get("Authorization")
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "transparent", "openai_compatible", upstream.URL, "upstream-secret", 1, 1, "transparent", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "same-model", "same-model", "chat", 1)
	key := insertTestKey(t, a, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "real-client/9.1")
	req.Header.Set("X-Client-Feature", "alpha")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if string(upstreamBody) != string(raw) {
		t.Fatalf("body changed\nwant: %q\n got: %q", raw, upstreamBody)
	}
	if upstreamUA != "real-client/9.1" || upstreamCustom != "alpha" || upstreamAuth != "Bearer upstream-secret" {
		t.Fatalf("headers ua=%q custom=%q auth=%q", upstreamUA, upstreamCustom, upstreamAuth)
	}
}

func TestClientPolicyUsesRealUserAgent(t *testing.T) {
	routes := []resolvedRoute{{Provider: Provider{ClientPolicy: "codex"}}, {Provider: Provider{ClientPolicy: "claude_code"}}}
	for _, tc := range []struct {
		ua   string
		want int
	}{{"codex-cli/1.0", 1}, {"claude-code/2.0", 1}, {"browser/1.0", 0}} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("User-Agent", tc.ua)
		if got := len(filterClientRoutes(routes, req)); got != tc.want {
			t.Fatalf("UA %q got %d routes, want %d", tc.ua, got, tc.want)
		}
	}
}

func abruptServer(t *testing.T, body string, contentLength int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijacking unavailable")
		}
		conn, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(buffer, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", contentLength, body)
		_ = buffer.Flush()
		_ = conn.Close()
	}))
}

func TestStreamingDoesNotFailOverAfterResponseStarts(t *testing.T) {
	first := abruptServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", 100)
	defer first.Close()
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"backup\"}}]}\n\n"))
	}))
	defer backup.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "stream-primary", "openai_compatible", first.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "stream-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "stream-model", "upstream", "chat,stream", 1)
	insertTestRoute(t, a, p2, "stream-model", "upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"stream-model","stream":true,"messages":[]}`, "test/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("backup called %d times after stream started", backupCalls.Load())
	}
}

func TestStreamingFailsOverBeforeFirstByte(t *testing.T) {
	first := abruptServer(t, "", 100)
	defer first.Close()
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"backup\"}}]}\n\n"))
	}))
	defer backup.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "empty-primary", "openai_compatible", first.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "empty-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "stream-model", "upstream", "chat,stream", 1)
	insertTestRoute(t, a, p2, "stream-model", "upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"stream-model","stream":true,"messages":[]}`, "test/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("backup calls = %d", backupCalls.Load())
	}
}

func TestCrossHostRedirectBlockedBeforeCredentialLeak(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirected.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(redirected.URL, "http://"))
	redirectTarget := "http://localhost:" + port + "/secret"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget, http.StatusFound)
	}))
	defer origin.Close()
	client := newUpstreamHTTPClient(Config{AllowInsecureUpstreams: true, AllowPrivateUpstreams: true})
	req, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	req.Header.Set("Authorization", "Bearer must-not-leak")
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "cross-host") {
		t.Fatalf("redirect error = %v", err)
	}
	if redirectedCalls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", redirectedCalls.Load())
	}
}

func TestClientErrorDoesNotFailOver(t *testing.T) {
	var backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad input"})
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer backup.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "client-error", "openai_compatible", primary.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "unused-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","messages":[]}`, "test/1")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad input") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("backup called for a client error: %d", backupCalls.Load())
	}
}

func TestRetryAfterPropagatesAfterAllProvidersRateLimit(t *testing.T) {
	limited := func(seconds string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", seconds)
			http.Error(w, "limited", http.StatusTooManyRequests)
		}))
	}
	first := limited("7")
	defer first.Close()
	second := limited("17")
	defer second.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "limited-one", "openai_compatible", first.URL, "one", 1, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "limited-two", "openai_compatible", second.URL, "two", 2, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","messages":[]}`, "test/1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "17" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
}

func TestImageTransportFailureFailsOverBeforeClientResponse(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker := w.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer broken.Close()
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"b64_json": "YmFja3Vw"}}})
	}))
	defer backup.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.db.Exec(`UPDATE settings SET value=? WHERE key='routing_strategy'`, StrategyPriorityFailover); err != nil {
		t.Fatal(err)
	}
	p1 := insertTestProvider(t, a, "image-primary", "openai_compatible", broken.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "image-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "image-model", "upstream", "image", 1)
	insertTestRoute(t, a, p2, "image-model", "upstream", "image", 1)
	key := insertTestKey(t, a, true)
	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"image-model","prompt":"cat"}`, "test/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("backup calls=%d, want 1", backupCalls.Load())
	}
	if !strings.Contains(rec.Body.String(), "YmFja3Vw") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestEmptyStreamFailsOverBeforeHeadersAreCommitted(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup\"}}]}\n\n")
	}))
	defer backup.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "empty-stream", "openai_compatible", primary.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "stream-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat,stream", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","stream":true,"messages":[]}`, "test/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backup") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
	a.flushLedgerWrites()
	var retryReason string
	if err := a.db.QueryRow(`SELECT retry_reason FROM request_ledger WHERE attempt=2`).Scan(&retryReason); err != nil {
		t.Fatal(err)
	}
	if retryReason != "upstream_empty_stream" {
		t.Fatalf("retry reason = %q", retryReason)
	}
}

func TestRetryAfterImmediatelyOpensProviderCircuit(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "backup"}}}})
	}))
	defer backup.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "rate-limited", "openai_compatible", primary.URL, "one", 2, 1, "normalized", "any", 0, 5, 30)
	p2 := insertTestProvider(t, a, "rate-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat", 1)
	key := insertTestKey(t, a, false)

	for range 2 {
		rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","messages":[]}`, "test/1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 2 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
	a.routeMu.Lock()
	openUntil := a.stateForLocked(Provider{ID: p1}).CircuitOpenUntil
	a.routeMu.Unlock()
	if time.Until(openUntil) < 55*time.Second {
		t.Fatalf("circuit open until %s", openUntil)
	}
}

func TestRateLimitWithoutRetryAfterImmediatelyOpensProviderCircuit(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "backup"}}}})
	}))
	defer backup.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "rate-limited-no-retry-after", "openai_compatible", primary.URL, "one", 2, 1, "normalized", "any", 0, 5, 30)
	p2 := insertTestProvider(t, a, "rate-backup-no-retry-after", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat", 1)
	key := insertTestKey(t, a, false)

	for range 2 {
		rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","messages":[]}`, "test/1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 2 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
	a.routeMu.Lock()
	state := a.stateForLocked(Provider{ID: p1})
	openUntil, failures := state.CircuitOpenUntil, state.ConsecutiveFailures
	a.routeMu.Unlock()
	if failures != 1 {
		t.Fatalf("consecutive failures=%d, want 1", failures)
	}
	if time.Until(openUntil) < 4*time.Minute+55*time.Second {
		t.Fatalf("circuit open until %s", openUntil)
	}

	var enabled int
	var status, lastError string
	if err := a.db.QueryRow(`SELECT enabled,status,last_error FROM providers WHERE id=?`, p1).Scan(&enabled, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || status != "rate_limited" || lastError != "upstream_rate_limited" {
		t.Fatalf("provider enabled=%d status=%q last_error=%q", enabled, status, lastError)
	}
}

func TestRepeatedRateLimitsDoNotAutoDisableProvider(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	providerID := insertTestProvider(t, a, "temporary-rate-limit", "openai_compatible", "http://provider.test", "secret", 1, 1, "normalized", "any", 0, 1, 30)
	z := resolvedRoute{
		Route:    Route{ID: 1, ProviderID: providerID, PublicName: "model", UpstreamModel: "upstream"},
		Provider: Provider{ID: providerID, FailureThreshold: 1, CooldownSeconds: 30},
	}
	limited := attemptResult{Status: http.StatusTooManyRequests, Retryable: true, Reason: "upstream_rate_limited"}
	const attempts = 7
	for range attempts {
		a.completeRoute(z, limited, time.Millisecond)
	}

	var enabled, failures int
	var status, lastError string
	var circuitOpenUntil any
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error,circuit_open_until FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError, &circuitOpenUntil); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || failures != attempts || status != "rate_limited" || lastError != "upstream_rate_limited" || circuitOpenUntil == nil {
		t.Fatalf("rate-limited provider enabled=%d failures=%d status=%q last_error=%q circuit_open_until=%v", enabled, failures, status, lastError, circuitOpenUntil)
	}
	a.routeMu.Lock()
	state := a.stateForLocked(z.Provider)
	if state.CircuitOpenUntil.IsZero() {
		a.routeMu.Unlock()
		t.Fatal("rate-limited runtime circuit was not opened")
	}
	a.routeMu.Unlock()

	a.completeRoute(z, attemptResult{Status: http.StatusOK, Handled: true}, time.Millisecond)
	var recoveredCircuit any
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error,circuit_open_until FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError, &recoveredCircuit); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || failures != 0 || status != "healthy" || lastError != "" || recoveredCircuit != nil {
		t.Fatalf("recovered provider enabled=%d failures=%d status=%q last_error=%q circuit_open_until=%v", enabled, failures, status, lastError, recoveredCircuit)
	}
}

func TestDownstreamCancellationDoesNotDegradeProvider(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p := insertTestProvider(t, a, "cancel-neutral", "openai_compatible", upstream.URL, "secret", 1, 1, "normalized", "any", 0, 1, 30)
	z := resolvedRoute{
		Route:      Route{ID: 1, PublicName: "model", UpstreamModel: "model"},
		Provider:   Provider{ID: p, Type: "openai_compatible", BaseURL: upstream.URL, RequestTimeoutMS: 5000, FailureThreshold: 1, CooldownSeconds: 30},
		Credential: "secret",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	result := a.proxyUpstream(rec, req, z, proxyOptions{Endpoint: "/v1/chat/completions", RawBody: []byte(`{"model":"model"}`), SafeTransportRetry: true})
	if result.Reason != "downstream_canceled" || result.Retryable {
		t.Fatalf("result=%+v", result)
	}
	a.completeRoute(z, result, time.Millisecond)
	a.routeMu.Lock()
	state := a.stateForLocked(z.Provider)
	failures, openUntil := state.ConsecutiveFailures, state.CircuitOpenUntil
	a.routeMu.Unlock()
	if failures != 0 || !openUntil.IsZero() {
		t.Fatalf("cancellation degraded provider: failures=%d open_until=%s calls=%d", failures, openUntil, upstreamCalls.Load())
	}
}

func TestFailoverStartTimeoutCoversResponseHeaders(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := resolvedRoute{
		Route:      Route{ID: 1, PublicName: "model", UpstreamModel: "model"},
		Provider:   Provider{ID: 1, Type: "openai_compatible", BaseURL: upstream.URL, RequestTimeoutMS: 5000},
		Credential: "secret",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model"}`))
	result := a.proxyUpstream(httptest.NewRecorder(), req, z, proxyOptions{
		Endpoint:           "/v1/chat/completions",
		RawBody:            []byte(`{"model":"model"}`),
		SafeTransportRetry: true,
		OutputStartTimeout: 40 * time.Millisecond,
	})
	if result.Status != http.StatusGatewayTimeout || !result.Retryable || result.Reason != "upstream_timeout" {
		t.Fatalf("result=%+v", result)
	}
}

func TestFailoverStartTimeoutCoversResponseBody(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := resolvedRoute{
		Route:      Route{ID: 1, PublicName: "model", UpstreamModel: "model"},
		Provider:   Provider{ID: 1, Type: "openai_compatible", BaseURL: upstream.URL, RequestTimeoutMS: 5000},
		Credential: "secret",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model"}`))
	result := a.proxyUpstream(httptest.NewRecorder(), req, z, proxyOptions{
		Endpoint:           "/v1/chat/completions",
		RawBody:            []byte(`{"model":"model"}`),
		UsageFormat:        "openai",
		SafeTransportRetry: true,
		OutputStartTimeout: 40 * time.Millisecond,
	})
	if result.Status != http.StatusGatewayTimeout || !result.Retryable || result.Reason != "upstream_output_timeout" {
		t.Fatalf("result=%+v", result)
	}
}

func TestReasoningEventsCountAsModelProgress(t *testing.T) {
	tests := []struct {
		name   string
		format string
		event  string
	}{
		{name: "openai reasoning", format: "openai", event: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"working\"}}]}\n\n"},
		{name: "responses reasoning", format: "openai", event: "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"working\"}\n\n"},
		{name: "anthropic thinking", format: "anthropic", event: "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"working\"}}\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !hasSemanticStreamOutput([]byte(tc.event), tc.format) {
				t.Fatal("reasoning event was not recognized as model progress")
			}
		})
	}
	if hasSemanticStreamOutput([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"), "anthropic") {
		t.Fatal("heartbeat was incorrectly recognized as model progress")
	}
}

func TestStreamingHeartbeatDoesNotHideStalledModel(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"started\"}}]}\n\n")
		w.(http.Flusher).Flush()
		ticker := time.NewTicker(15 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-release:
				return
			case <-ticker.C:
				_, _ = io.WriteString(w, ": heartbeat\n\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer upstream.Close()
	defer close(release)

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := resolvedRoute{
		Route:      Route{ID: 1, PublicName: "model", UpstreamModel: "model"},
		Provider:   Provider{ID: 1, Type: "openai_compatible", BaseURL: upstream.URL, RequestTimeoutMS: 40},
		Credential: "secret",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model","stream":true}`))
	result := a.proxyUpstream(httptest.NewRecorder(), req, z, proxyOptions{
		Endpoint:           "/v1/chat/completions",
		RawBody:            []byte(`{"model":"model","stream":true}`),
		Stream:             true,
		UsageFormat:        "openai",
		SafeTransportRetry: true,
		OutputStartTimeout: 40 * time.Millisecond,
		IdleTimeout:        80 * time.Millisecond,
	})
	if result.Status != http.StatusGatewayTimeout || !result.Handled || result.Reason != "upstream_stalled" {
		t.Fatalf("result=%+v", result)
	}
}

func TestProviderFailuresOpenCircuitWithoutChangingManualToggle(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	providerID := insertTestProvider(t, a, "temporary-circuit", "openai_compatible", "http://provider.test", "secret", 1, 1, "normalized", "any", 0, 5, 30)
	insertTestRoute(t, a, providerID, "model", "upstream", "chat", 1)
	z := resolvedRoute{
		Route:    Route{ID: 1, ProviderID: providerID, PublicName: "model", UpstreamModel: "upstream"},
		Provider: Provider{ID: providerID, FailureThreshold: 5, CooldownSeconds: 30},
	}
	failure := attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_server_error"}
	for range 5 {
		a.completeRoute(z, failure, time.Millisecond)
	}

	var enabled, failures int
	var status, lastError string
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || failures != 5 || status != "circuit_open" || lastError != "upstream_server_error" {
		t.Fatalf("circuit provider enabled=%d failures=%d status=%q last_error=%q", enabled, failures, status, lastError)
	}
	if _, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover); ok {
		t.Fatal("circuit-open provider remained selectable")
	}
	a.completeRoute(z, attemptResult{Status: http.StatusOK, Handled: true}, time.Millisecond)
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || failures != 0 || status != "healthy" || lastError != "" {
		t.Fatalf("recovered provider enabled=%d failures=%d status=%q last_error=%q", enabled, failures, status, lastError)
	}
	if _, _, ok := a.acquireRoute([]resolvedRoute{z}, map[int64]bool{}, StrategyPriorityFailover); !ok {
		t.Fatal("recovered provider did not return to routing")
	}
}

func TestConnectionFailureFailsOverBeforeAnyResponse(t *testing.T) {
	primary := httptest.NewServer(http.NotFoundHandler())
	primaryURL := primary.URL
	primary.Close()

	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "backup after connect failure"}}},
		})
	}))
	defer backup.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "offline-primary", "openai_compatible", primaryURL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "live-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "model", "upstream", "chat", 1)
	insertTestRoute(t, a, p2, "model", "upstream", "chat", 1)
	key := insertTestKey(t, a, false)

	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"model","messages":[]}`, "test/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backup after connect failure") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("backup calls=%d, want 1", backupCalls.Load())
	}
	a.flushLedgerWrites()
	var attempts int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
}

func TestNonStreamingChatUsesUpstreamSSEAndReturnsJSON(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"long \"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "streamed-chat", "openai_compatible", upstream.URL, "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "public", "upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"public","stream":false,"messages":[{"role":"user","content":"hello"}]}`, "test/1")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if received["stream"] != true || asMap(received["stream_options"])["include_usage"] != true {
		t.Fatalf("upstream request=%#v", received)
	}
	var completed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	choice := asMap(anySlice(completed["choices"])[0])
	message := asMap(choice["message"])
	function := asMap(asMap(anySlice(message["tool_calls"])[0])["function"])
	if message["content"] != "long answer" || choice["finish_reason"] != "tool_calls" || function["arguments"] != `{"q":1}` {
		t.Fatalf("completed=%#v", completed)
	}
	if asInt64(asMap(completed["usage"])["prompt_tokens"]) != 11 || asInt64(asMap(completed["usage"])["completion_tokens"]) != 7 {
		t.Fatalf("usage=%#v", completed["usage"])
	}
}
