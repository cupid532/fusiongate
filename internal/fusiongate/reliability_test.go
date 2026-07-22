package fusiongate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	first := abruptServer(t, "data: partial\n\n", 100)
	defer first.Close()
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		_, _ = w.Write([]byte("data: backup\n\n"))
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
		_, _ = w.Write([]byte("data: backup\n\n"))
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

func TestImageTransportFailureIsNotReplayed(t *testing.T) {
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
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
	}))
	defer backup.Close()
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p1 := insertTestProvider(t, a, "image-primary", "openai_compatible", broken.URL, "one", 2, 1, "normalized", "any", 0, 3, 30)
	p2 := insertTestProvider(t, a, "image-backup", "openai_compatible", backup.URL, "two", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, p1, "image-model", "upstream", "image", 1)
	insertTestRoute(t, a, p2, "image-model", "upstream", "image", 1)
	key := insertTestKey(t, a, true)
	rec := gatewayRequest(t, a, "/v1/images/generations", key, `{"model":"image-model","prompt":"cat"}`, "test/1")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("image request replayed to backup %d times", backupCalls.Load())
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
		_, _ = io.WriteString(w, "data: {\"source\":\"backup\"}\n\n")
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
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{}})
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

func TestProviderAutoDisablesAfterFiveConsecutiveFailures(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	providerID := insertTestProvider(t, a, "auto-close", "openai_compatible", "http://provider.test", "secret", 1, 1, "normalized", "any", 0, 10, 30)
	insertTestRoute(t, a, providerID, "model", "upstream", "chat", 1)
	z := resolvedRoute{
		Route:    Route{ID: 1, ProviderID: providerID, PublicName: "model", UpstreamModel: "upstream"},
		Provider: Provider{ID: providerID, FailureThreshold: 10, CooldownSeconds: 30},
	}
	failure := attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_server_error"}
	for range autoDisableAfterConsecutiveFailures {
		a.completeRoute(z, failure, time.Millisecond)
	}

	var enabled, failures int
	var status, lastError string
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || failures != autoDisableAfterConsecutiveFailures || status != "disabled" || lastError != "upstream_server_error" {
		t.Fatalf("auto-closed provider enabled=%d failures=%d status=%q last_error=%q", enabled, failures, status, lastError)
	}
	if _, err := a.resolve(context.Background(), "model", "chat"); err == nil {
		t.Fatal("automatically closed provider remained routable")
	}

	reenable := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"enabled":true}`))
	rec := httptest.NewRecorder()
	a.providerByID(rec, reenable, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := a.db.QueryRow(`SELECT enabled,consecutive_failures,status,last_error FROM providers WHERE id=?`, providerID).Scan(&enabled, &failures, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || failures != 0 || status != "unknown" || lastError != "" {
		t.Fatalf("re-enabled provider enabled=%d failures=%d status=%q last_error=%q", enabled, failures, status, lastError)
	}
	if _, err := a.resolve(context.Background(), "model", "chat"); err != nil {
		t.Fatalf("re-enabled provider did not return to routing: %v", err)
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
	var attempts int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
}
