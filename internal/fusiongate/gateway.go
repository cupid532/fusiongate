package fusiongate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type authKey struct {
	ID           int64
	Name         string
	Prefix       string
	Hash         string
	AllowAll     bool
	AllowModels  string
	DenyModels   string
	AllowImages  bool
	RPMLimit     int
	Revoked      bool
	ExpiresAt    *time.Time
	BudgetMicros int64
	SpentMicros  int64
}

type resolvedRoute struct {
	Route          Route
	Provider       Provider
	ProviderKeyID  int64
	AttemptID      int64
	Credential     string
	AuthCredential *ProviderCredential
}

func (a *App) api(fn func(http.ResponseWriter, *http.Request, authKey)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setGatewayCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		k, ok := a.authenticateKey(r)
		if !ok {
			fail(w, http.StatusUnauthorized, "invalid_api_key", "missing, expired, revoked, or invalid API key")
			return
		}
		if !a.allowRate(k) {
			fail(w, http.StatusTooManyRequests, "rate_limit_exceeded", "API key rate limit exceeded")
			return
		}
		if k.BudgetMicros > 0 {
			if !a.acquireBudgetKey(k.ID) {
				fail(w, http.StatusTooManyRequests, "budget_request_inflight", "another request is already using this key's budget")
				return
			}
			defer a.releaseBudgetKey(k.ID)
			refreshed, valid := a.authenticateKey(r)
			if !valid || refreshed.SpentMicros >= refreshed.BudgetMicros {
				fail(w, http.StatusPaymentRequired, "budget_exceeded", "API key budget exhausted")
				return
			}
			k = refreshed
		}
		fn(w, r, k)
	}
}

func (a *App) acquireBudgetKey(id int64) bool {
	a.budgetMu.Lock()
	defer a.budgetMu.Unlock()
	if a.budgetInflight[id] {
		return false
	}
	a.budgetInflight[id] = true
	return true
}

func (a *App) releaseBudgetKey(id int64) {
	a.budgetMu.Lock()
	delete(a.budgetInflight, id)
	a.budgetMu.Unlock()
}

func setGatewayCORS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == "" {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	requestedHeaders := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
	if requestedHeaders == "" {
		requestedHeaders = "Authorization, Content-Type, X-API-Key"
	}
	h.Set("Access-Control-Allow-Headers", requestedHeaders)
	h.Set("Access-Control-Expose-Headers", "Content-Type, Retry-After, X-FusionGate-Request-ID")
	h.Set("Access-Control-Max-Age", "86400")
	h.Add("Vary", "Origin")
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Access-Control-Request-Private-Network")), "true") {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

func (a *App) authenticateKey(r *http.Request) (authKey, bool) {
	raw := bearer(r)
	if raw == "" {
		raw = r.Header.Get("x-api-key")
	}
	if raw == "" {
		return authKey{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	var x authKey
	var allowAll, allowImages, revoked int
	var expiresAt, createdAt string
	err := a.db.QueryRow(`SELECT id,name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,revoked,COALESCE(expires_at,''),created_at,budget_micros,COALESCE((SELECT SUM(cost_micros) FROM request_ledger WHERE api_key_id=api_keys.id AND completed_at IS NOT NULL),0) FROM api_keys WHERE key_hash=?`, hex.EncodeToString(sum[:])).Scan(&x.ID, &x.Name, &x.Prefix, &x.Hash, &allowAll, &x.AllowModels, &x.DenyModels, &allowImages, &x.RPMLimit, &revoked, &expiresAt, &createdAt, &x.BudgetMicros, &x.SpentMicros)
	if err != nil {
		return authKey{}, false
	}
	x.AllowAll = strBool(allowAll)
	x.AllowImages = strBool(allowImages)
	x.Revoked = strBool(revoked)
	x.ExpiresAt = parseTime(expiresAt)
	if x.Revoked || (x.ExpiresAt != nil && time.Now().After(*x.ExpiresAt)) || (x.BudgetMicros > 0 && x.SpentMicros >= x.BudgetMicros) {
		return authKey{}, false
	}
	_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, now(), x.ID)
	return x, true
}

func (a *App) allowRate(k authKey) bool {
	if k.RPMLimit <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	x := a.rate[k.Hash]
	t := time.Now()
	if x == nil || t.Sub(x.At) >= time.Minute {
		a.rate[k.Hash] = &rateWindow{At: t, Count: 1}
		return true
	}
	x.Count++
	return x.Count <= k.RPMLimit
}

func allowed(k authKey, model string) bool {
	if matches(k.DenyModels, model) {
		return false
	}
	return k.AllowAll || matches(k.AllowModels, model)
}

func matches(patterns, model string) bool {
	for _, p := range strings.Split(patterns, ",") {
		p = strings.TrimSpace(p)
		if p == model || (strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*"))) {
			return p != ""
		}
	}
	return false
}

func matchesCapability(capabilities, required string) bool {
	if required == "" {
		return true
	}
	for _, capability := range strings.Split(capabilities, ",") {
		if strings.EqualFold(strings.TrimSpace(capability), required) {
			return true
		}
	}
	return false
}

func (a *App) models(w http.ResponseWriter, r *http.Request, k authKey) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	rows, err := a.db.Query(`SELECT r.public_name,MIN(r.created_at) FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.enabled=1 AND p.enabled=1 GROUP BY r.public_name ORDER BY r.public_name`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var name, created string
		if rows.Scan(&name, &created) == nil && allowed(k, name) {
			createdAt := parseTime(created)
			var unix int64
			if createdAt != nil {
				unix = createdAt.Unix()
			}
			data = append(data, map[string]any{"id": name, "object": "model", "created": unix, "owned_by": "fusiongate"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (a *App) resolve(ctx context.Context, model, requiredCapability string) ([]resolvedRoute, error) {
	rows, err := a.db.QueryContext(ctx, `
SELECT r.id,r.provider_id,r.public_name,r.upstream_model,r.capabilities,r.enabled,r.priority,r.sort_order,
       r.input_price_micros,r.cached_price_micros,r.output_price_micros,r.long_context_threshold,
       r.long_input_price_micros,r.long_cached_price_micros,r.long_output_price_micros,
	       p.id,p.name,p.type,p.base_url,p.credential,p.auth_kind,p.enabled,p.priority,p.sort_order,p.weight,p.status,p.notes,
       p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,
       p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,p.last_first_byte_ms,
       COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,''),p.ip_pool_node_id,p.multi_key_initialized,p.default_model
FROM model_routes r JOIN providers p ON p.id=r.provider_id
WHERE r.public_name=? AND r.enabled=1 AND p.enabled=1 AND p.archived=0
ORDER BY r.sort_order,r.id`, model)
	if err != nil {
		return nil, err
	}
	type pendingRoute struct {
		resolved            resolvedRoute
		credential          []byte
		authKind            string
		multiKeyInitialized bool
	}
	pending := []pendingRoute{}
	for rows.Next() {
		var z resolvedRoute
		var routeEnabled, providerEnabled int
		var credential []byte
		var authKind string
		var ipPoolNodeID sql.NullInt64
		var multiKeyInitialized int
		if err := rows.Scan(
			&z.Route.ID, &z.Route.ProviderID, &z.Route.PublicName, &z.Route.UpstreamModel, &z.Route.Capabilities, &routeEnabled,
			&z.Route.Priority, &z.Route.SortOrder, &z.Route.InputPriceMicros, &z.Route.CachedPriceMicros, &z.Route.OutputPriceMicros,
			&z.Route.LongContextThreshold, &z.Route.LongInputPriceMicros, &z.Route.LongCachedPriceMicros, &z.Route.LongOutputPriceMicros,
			&z.Provider.ID, &z.Provider.Name, &z.Provider.Type, &z.Provider.BaseURL, &credential, &authKind, &providerEnabled,
			&z.Provider.Priority, &z.Provider.SortOrder, &z.Provider.Weight, &z.Provider.Status, &z.Provider.Notes,
			&z.Provider.PassthroughMode, &z.Provider.ClientPolicy, &z.Provider.MaxConcurrency, &z.Provider.RequestTimeoutMS,
			&z.Provider.FailureThreshold, &z.Provider.CooldownSeconds, &z.Provider.ConsecutiveFailures,
			&z.Provider.CircuitOpenUntil, &z.Provider.LastError, &z.Provider.LastLatencyMS, &z.Provider.LastFirstByteMS,
			&z.Provider.LastSuccessAt, &z.Provider.LastFailureAt, &ipPoolNodeID, &multiKeyInitialized, &z.Provider.DefaultModel,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		z.Route.Enabled = strBool(routeEnabled)
		z.Provider.Enabled = strBool(providerEnabled)
		if ipPoolNodeID.Valid {
			value := ipPoolNodeID.Int64
			z.Provider.IPPoolNodeID = &value
		}
		if !matchesCapability(z.Route.Capabilities, requiredCapability) {
			continue
		}
		pending = append(pending, pendingRoute{
			resolved:            z,
			credential:          append([]byte(nil), credential...),
			authKind:            authKind,
			multiKeyInitialized: strBool(multiKeyInitialized),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	out := make([]resolvedRoute, 0, len(pending))
	for _, candidate := range pending {
		z := candidate.resolved
		if candidate.authKind == "api_key" {
			selectedKeys, selectErr := a.selectProviderKeys(ctx, z.Provider.ID, z.Route.UpstreamModel, z.Provider.IPPoolNodeID, candidate.credential, candidate.multiKeyInitialized)
			if selectErr != nil {
				continue
			}
			for _, selected := range selectedKeys {
				keyRoute := z
				keyRoute.ProviderKeyID = selected.ID
				keyRoute.Credential = selected.Credential
				keyRoute.Provider.IPPoolNodeID = selected.IPPoolNodeID
				out = append(out, keyRoute)
			}
			continue
		}
		plaintext, decryptErr := a.decrypt(candidate.credential)
		if decryptErr != nil {
			return nil, fmt.Errorf("cannot decrypt provider %s credential: %w", z.Provider.Name, decryptErr)
		}
		authCredential, accessToken, decodeErr := decodeStoredCredential(candidate.authKind, plaintext)
		if decodeErr != nil {
			return nil, fmt.Errorf("cannot load provider %s credential: %w", z.Provider.Name, decodeErr)
		}
		z.Credential = accessToken
		z.AuthCredential = &authCredential
		out = append(out, z)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no eligible route for model %q", model)
	}
	return out, nil
}

func requestID() string { return "req_" + hex.EncodeToString(randomBytes(12)) }

func requestClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if parsed, err := netip.ParseAddrPort(host); err == nil {
		host = parsed.Addr().String()
	}
	if addr, err := netip.ParseAddr(host); err == nil && (addr.IsLoopback() || addr.IsPrivate()) {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			if parsed, err := netip.ParseAddr(forwarded); err == nil {
				host = parsed.String()
			}
		}
	}
	return host
}

func (a *App) startLedger(k authKey, z resolvedRoute, protocol string, stream bool, clientIP, gatewayID string, attempt int, retryReason string) (int64, string) {
	if err := a.pruneRequestLedger(context.Background(), false); err != nil {
		a.log.Error("request ledger retention cleanup", "error", err)
	}
	attemptID := gatewayID + "_a" + strconv.Itoa(attempt)
	res, err := a.db.Exec(`INSERT INTO request_ledger(request_id,gateway_request_id,attempt,retry_reason,created_at,api_key_id,provider_id,route_id,public_model,upstream_model,protocol,stream,client_ip,api_key_name,api_key_prefix,provider_name) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, gatewayID, attempt, retryReason, now(), k.ID, z.Provider.ID, z.Route.ID, z.Route.PublicName, z.Route.UpstreamModel, protocol, boolInt(stream), clientIP, k.Name, k.Prefix, z.Provider.Name)
	if err != nil {
		a.log.Error("ledger insert", "error", err)
		return 0, attemptID
	}
	id, _ := res.LastInsertId()
	return id, attemptID
}

func (a *App) recordFirstByte(id int64, start time.Time) {
	if id == 0 {
		return
	}
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	if _, err := a.db.Exec(`UPDATE request_ledger SET first_byte_ms=? WHERE id=? AND first_byte_ms IS NULL`, elapsed, id); err != nil {
		a.log.Error("ledger first byte update", "error", err)
	}
}

func (a *App) endLedger(id int64, success bool, status int, errorType string, start time.Time, usage Usage) {
	if id == 0 {
		return
	}
	_, err := a.db.Exec(`UPDATE request_ledger SET completed_at=?,success=?,status_code=?,error_type=?,latency_ms=?,input_tokens=?,output_tokens=?,cached_tokens=?,reasoning_tokens=?,cost_micros=?,cost_type=?,usage_reported=? WHERE id=?`, now(), boolInt(success), status, errorType, time.Since(start).Milliseconds(), usage.Input, usage.Output, usage.Cached, usage.Reasoning, usage.CostMicros, usage.CostType, boolInt(usage.Reported), id)
	if err != nil {
		a.log.Error("ledger update", "error", err)
	}
}

func cost(z resolvedRoute, usage *Usage) {
	if usage.CostMicros > 0 {
		usage.CostType = "actual"
		return
	}
	inputPrice, cachedPrice, outputPrice := z.Route.InputPriceMicros, z.Route.CachedPriceMicros, z.Route.OutputPriceMicros
	if z.Route.LongContextThreshold > 0 && usage.Input >= z.Route.LongContextThreshold && z.Route.LongInputPriceMicros > 0 {
		inputPrice, cachedPrice, outputPrice = z.Route.LongInputPriceMicros, z.Route.LongCachedPriceMicros, z.Route.LongOutputPriceMicros
	}
	if inputPrice > 0 || cachedPrice > 0 || outputPrice > 0 {
		uncached := usage.Input - usage.Cached
		if uncached < 0 {
			uncached = 0
		}
		if cachedPrice <= 0 {
			cachedPrice = inputPrice
		}
		usage.CostMicros = (uncached*inputPrice + usage.Cached*cachedPrice + usage.Output*outputPrice) / 1_000_000
		usage.CostType = "estimated"
	} else {
		usage.CostType = "unknown"
	}
}

func getBody(r *http.Request) (map[string]any, []byte, error) {
	const limit = 20 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > limit {
		return nil, nil, errRequestBodyTooLarge
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, err
	}
	return parsed, body, nil
}

func parseOpenAIUsage(payload map[string]any) Usage {
	usage := Usage{CostType: "unknown"}
	m, ok := payload["usage"].(map[string]any)
	if !ok {
		if response, nested := payload["response"].(map[string]any); nested {
			m, ok = response["usage"].(map[string]any)
		}
	}
	if !ok {
		return usage
	}
	usage.Reported = true
	usage.Input = num(m["prompt_tokens"])
	if _, exists := m["prompt_tokens"]; !exists {
		usage.Input = num(m["input_tokens"])
	}
	usage.Output = num(m["completion_tokens"])
	if _, exists := m["completion_tokens"]; !exists {
		usage.Output = num(m["output_tokens"])
	}
	if details, ok := m["prompt_tokens_details"].(map[string]any); ok {
		usage.Cached = num(details["cached_tokens"])
	}
	if details, ok := m["input_tokens_details"].(map[string]any); ok {
		usage.Cached = num(details["cached_tokens"])
	}
	if details, ok := m["completion_tokens_details"].(map[string]any); ok {
		usage.Reasoning = num(details["reasoning_tokens"])
	}
	if details, ok := m["output_tokens_details"].(map[string]any); ok {
		usage.Reasoning = num(details["reasoning_tokens"])
	}
	return usage
}

func parseAnthropicUsage(payload map[string]any) Usage {
	usage := Usage{CostType: "unknown"}
	m, ok := payload["usage"].(map[string]any)
	if !ok {
		if message, nested := payload["message"].(map[string]any); nested {
			m, ok = message["usage"].(map[string]any)
		}
	}
	if !ok {
		return usage
	}
	usage.Reported = true
	cacheCreation := num(m["cache_creation_input_tokens"])
	cacheRead := num(m["cache_read_input_tokens"])
	usage.Input = num(m["input_tokens"]) + cacheCreation + cacheRead
	usage.Output = num(m["output_tokens"])
	usage.Cached = cacheRead
	if details, ok := m["output_tokens_details"].(map[string]any); ok {
		usage.Reasoning = num(details["thinking_tokens"])
	}
	return usage
}

func parseGeminiUsage(payload map[string]any) Usage {
	usage := Usage{CostType: "unknown"}
	metadata, ok := payload["usageMetadata"].(map[string]any)
	if !ok {
		return usage
	}
	usage.Reported = true
	usage.Input = num(metadata["promptTokenCount"])
	usage.Reasoning = num(metadata["thoughtsTokenCount"])
	usage.Output = num(metadata["candidatesTokenCount"]) + usage.Reasoning
	if total := num(metadata["totalTokenCount"]); total >= usage.Input {
		usage.Output = total - usage.Input
	}
	usage.Cached = num(metadata["cachedContentTokenCount"])
	return usage
}

func mergeUsage(dst *Usage, next Usage) {
	if !next.Reported {
		return
	}
	dst.Reported = true
	if next.Input > dst.Input {
		dst.Input = next.Input
	}
	if next.Output > dst.Output {
		dst.Output = next.Output
	}
	if next.Cached > dst.Cached {
		dst.Cached = next.Cached
	}
	if next.Reasoning > dst.Reasoning {
		dst.Reasoning = next.Reasoning
	}
	if next.CostMicros > dst.CostMicros {
		dst.CostMicros = next.CostMicros
	}
	if next.CostType != "" && next.CostType != "unknown" {
		dst.CostType = next.CostType
	}
}

func num(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func textContent(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var texts []string
		for _, item := range content {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

type routeExecutor func(resolvedRoute, string, func()) attemptResult

func (a *App) runRoutes(w http.ResponseWriter, r *http.Request, key authKey, routes []resolvedRoute, protocol string, stream bool, execute routeExecutor) {
	routes = filterClientRoutes(routes, r)
	if len(routes) == 0 {
		fail(w, http.StatusForbidden, "provider_client_policy_mismatch", "no provider accepts this request's real User-Agent")
		return
	}
	strategy := a.globalRoutingStrategy()
	routes = a.prepareRoutes(routes, strategy)
	for i := range routes {
		routes[i].AttemptID = int64(i + 1)
	}
	gatewayID := requestID()
	clientIP := requestClientIP(r)
	tried := map[int64]bool{}
	previousReason := ""
	lastStatus := http.StatusBadGateway
	var retryAfter time.Duration
	for attempt := 1; ; attempt++ {
		z, availability, ok := a.acquireRoute(routes, tried, strategy)
		if !ok {
			if availability.RetryAfter > retryAfter {
				retryAfter = availability.RetryAfter
			}
			if retryAfter > 0 {
				seconds := int64(retryAfter.Round(time.Second) / time.Second)
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			}
			status := http.StatusServiceUnavailable
			if len(tried) > 0 && lastStatus >= 500 {
				status = http.StatusBadGateway
			}
			message := routeUnavailableMessage(availability)
			if previousReason != "" {
				message = "all eligible providers failed (" + previousReason + ")"
			}
			fail(w, status, "upstream_unavailable", message)
			return
		}
		tried[z.AttemptID] = true
		started := time.Now()
		ledgerID, attemptID := a.startLedger(key, z, protocol, stream, clientIP, gatewayID, attempt, previousReason)
		var observedFirstByteMS atomic.Int64
		result := execute(z, attemptID, func() {
			elapsed := time.Since(started).Milliseconds()
			if elapsed < 1 {
				elapsed = 1
			}
			observedFirstByteMS.CompareAndSwap(0, elapsed)
			a.recordFirstByte(ledgerID, started)
		})
		latency := time.Since(started)
		a.completeRoute(z, result, latency, time.Duration(observedFirstByteMS.Load())*time.Millisecond)
		status := result.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		lastStatus = status
		reason := result.Reason
		if reason == "" && status >= 400 {
			reason = retryReason(status, result.Err)
		}
		a.endLedger(ledgerID, result.Handled && status < 400 && result.Err == nil, status, reason, started, result.Usage)
		if result.Handled {
			return
		}
		if !result.Retryable {
			fail(w, status, reason, "upstream request failed and is not safe to retry")
			return
		}
		if result.RetryAfter > retryAfter {
			retryAfter = result.RetryAfter
		}
		previousReason = reason
	}
}

func (a *App) openAIProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, requestID, endpoint string, stream, safeTransportRetry bool, onFirstByte func()) attemptResult {
	transparent := z.Provider.PassthroughMode == "transparent"
	body := raw
	var err error
	upstreamSSE := false
	bufferResponsesSSE := false
	if !transparent {
		if z.Provider.Type == "codex_oauth" && endpoint == "/v1/responses" {
			body, err = normalizedCodexResponsesBody(raw, z.Route.UpstreamModel)
			upstreamSSE = true
			bufferResponsesSSE = !stream
		} else {
			body, err = normalizedOpenAIBody(raw, z.Route.UpstreamModel, stream, z.Provider.Type != "codex_oauth")
			if err == nil && z.Provider.Type == "openai_compatible" && endpoint == "/v1/chat/completions" {
				body, err = normalizedCompatibleChatBody(body)
			}
		}
		if err != nil {
			return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
		}
	}
	return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: endpoint, RawBody: body, Stream: stream, Transparent: transparent, UsageFormat: "openai", GatewayID: requestID, SafeTransportRetry: safeTransportRetry, OnFirstByte: onFirstByte, UpstreamSSE: upstreamSSE, BufferResponsesSSE: bufferResponsesSSE})
}

func (a *App) chat(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if !allowed(key, model) {
		fail(w, http.StatusForbidden, "model_not_allowed", "this API key is not allowed to use this model")
		return
	}
	stream, _ := body["stream"].(bool)
	routes, err := a.resolve(r.Context(), model, "chat")
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	a.runRoutes(w, r, key, routes, "openai_chat", stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		switch z.Provider.Type {
		case "openai", "grok", "openrouter", "openai_compatible", "grok_oauth":
			return a.openAIProxy(w, r, raw, z, rid, "/v1/chat/completions", stream, true, onFirstByte)
		case "codex_oauth":
			encoded, err := codexResponsesBodyFromChat(raw, z.Route.UpstreamModel)
			if err != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
			}
			return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/responses", RawBody: encoded, Stream: stream, UsageFormat: "openai", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferResponsesSSE: true, ResponsesTransform: func(completed []byte) ([]byte, string, error) {
				return codexChatResponse(completed, stream, z.Route.PublicName)
			}})
		case "anthropic", "claude_oauth":
			if stream || z.Provider.PassthroughMode == "transparent" {
				return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
			}
			return a.chatAnthropic(w, r, body, z, rid, onFirstByte)
		case "gemini":
			if stream || z.Provider.PassthroughMode == "transparent" {
				return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
			}
			return a.chatGemini(w, r, body, z, rid, onFirstByte)
		default:
			return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
		}
	})
}

func (a *App) chatAnthropic(w http.ResponseWriter, r *http.Request, body map[string]any, z resolvedRoute, rid string, onFirstByte func()) attemptResult {
	messages, _ := body["messages"].([]any)
	outMessages := []map[string]any{}
	system := ""
	for _, value := range messages {
		message, _ := value.(map[string]any)
		role, _ := message["role"].(string)
		content := textContent(message["content"])
		if role == "system" {
			system += content + "\n"
			continue
		}
		if role != "assistant" {
			role = "user"
		}
		outMessages = append(outMessages, map[string]any{"role": role, "content": content})
	}
	maxTokens := int64(1024)
	if value := num(body["max_tokens"]); value > 0 {
		maxTokens = value
	}
	input := map[string]any{"model": z.Route.UpstreamModel, "max_tokens": maxTokens, "messages": outMessages}
	if system != "" {
		input["system"] = system
	}
	if temperature, ok := body["temperature"]; ok {
		input["temperature"] = temperature
	}
	encoded, _ := json.Marshal(input)
	upstreamURL, _ := joinURLQuery(z.Provider.BaseURL, "/v1/messages", "")
	ctx, cancel := providerContext(r.Context(), z.Provider)
	defer cancel()
	if err := a.ensureFreshProviderCredential(ctx, &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
	copyUpstreamRequestHeaders(req.Header, r.Header)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	resp, err := a.doProviderRequest(req, z.Provider.IPPoolNodeID)
	if err != nil {
		if downstreamCanceled(r) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, err), Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, onFirstByte)
	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		reason := retryReason(resp.StatusCode, nil)
		if copyErr != nil {
			reason = "downstream_write_error"
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: copyErr}
	}
	var source map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&source); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
	}
	content := ""
	if contents, ok := source["content"].([]any); ok {
		for _, value := range contents {
			if part, ok := value.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					content += text
				}
			}
		}
	}
	usage := parseAnthropicUsage(source)
	cost(z, &usage)
	writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl-" + rid, "object": "chat.completion", "created": time.Now().Unix(), "model": z.Route.PublicName, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": source["stop_reason"]}}, "usage": map[string]any{"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": usage.Input + usage.Output}})
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: usage}
}

func (a *App) chatGemini(w http.ResponseWriter, r *http.Request, body map[string]any, z resolvedRoute, rid string, onFirstByte func()) attemptResult {
	messages, _ := body["messages"].([]any)
	contents := []map[string]any{}
	for _, value := range messages {
		message, _ := value.(map[string]any)
		role, _ := message["role"].(string)
		if role == "assistant" {
			role = "model"
		} else {
			role = "user"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": textContent(message["content"])}}})
	}
	encoded, _ := json.Marshal(map[string]any{"contents": contents})
	endpoint := "/v1beta/models/" + url.PathEscape(z.Route.UpstreamModel) + ":generateContent"
	upstreamURL, _ := joinURLQuery(z.Provider.BaseURL, endpoint, "key="+url.QueryEscape(z.Credential))
	ctx, cancel := providerContext(r.Context(), z.Provider)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
	copyUpstreamRequestHeaders(req.Header, r.Header)
	req.Header.Set("content-type", "application/json")
	resp, err := a.doProviderRequest(req, z.Provider.IPPoolNodeID)
	if err != nil {
		if downstreamCanceled(r) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, err), Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, onFirstByte)
	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		reason := retryReason(resp.StatusCode, nil)
		if copyErr != nil {
			reason = "downstream_write_error"
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: copyErr}
	}
	var source map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&source); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}
	}
	content := ""
	if candidates, ok := source["candidates"].([]any); ok && len(candidates) > 0 {
		candidate, _ := candidates[0].(map[string]any)
		candidateContent, _ := candidate["content"].(map[string]any)
		parts, _ := candidateContent["parts"].([]any)
		for _, part := range parts {
			content += textContent(part)
		}
	}
	usage := parseGeminiUsage(source)
	cost(z, &usage)
	writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl-" + rid, "object": "chat.completion", "created": time.Now().Unix(), "model": z.Route.PublicName, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": usage.Input + usage.Output}})
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: usage}
}

func (a *App) responses(w http.ResponseWriter, r *http.Request, key authKey) {
	a.openAIEndpoint(w, r, key, "openai_responses", "/v1/responses", "chat", true)
}

func (a *App) openAIEndpoint(w http.ResponseWriter, r *http.Request, key authKey, protocol, endpoint, capability string, safeTransportRetry bool) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if !allowed(key, model) {
		fail(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), model, capability)
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	compatible := routes[:0]
	for _, z := range routes {
		if z.Provider.Type == "openai" || z.Provider.Type == "grok" || z.Provider.Type == "openrouter" || z.Provider.Type == "openai_compatible" || z.Provider.Type == "codex_oauth" || z.Provider.Type == "grok_oauth" {
			compatible = append(compatible, z)
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no OpenAI-compatible route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, protocol, stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		if protocol == "openai_images" && z.Provider.Type == "codex_oauth" {
			return a.codexImageProxy(w, r, raw, z, rid, onFirstByte)
		}
		if protocol == "openai_responses" && z.Provider.Type == "openai_compatible" && z.Provider.PassthroughMode != "transparent" {
			return a.compatibleResponsesProxy(w, r, raw, z, rid, stream, safeTransportRetry, onFirstByte)
		}
		return a.openAIProxy(w, r, raw, z, rid, endpoint, stream, safeTransportRetry, onFirstByte)
	})
}

func (a *App) messages(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	model, _ := body["model"].(string)
	if model == "" || !allowed(key, model) {
		fail(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), model, "chat")
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	compatible := routes[:0]
	for _, z := range routes {
		switch z.Provider.Type {
		case "anthropic", "claude_oauth":
			compatible = append(compatible, z)
		case "openai", "grok", "openrouter", "openai_compatible", "grok_oauth":
			if z.Provider.PassthroughMode != "transparent" {
				compatible = append(compatible, z)
			}
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no Anthropic or OpenAI-compatible route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, "anthropic_messages", stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		if z.Provider.Type == "openai" || z.Provider.Type == "grok" || z.Provider.Type == "openrouter" || z.Provider.Type == "openai_compatible" || z.Provider.Type == "grok_oauth" {
			return a.anthropicMessagesOpenAI(w, r, body, z, rid, stream, onFirstByte)
		}
		transparent := z.Provider.PassthroughMode == "transparent"
		encoded := raw
		if !transparent {
			copyBody := make(map[string]any, len(body))
			for key, value := range body {
				copyBody[key] = value
			}
			copyBody["model"] = z.Route.UpstreamModel
			var encodeErr error
			encoded, encodeErr = json.Marshal(copyBody)
			if encodeErr != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: encodeErr}
			}
		}
		return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/messages", RawBody: encoded, Stream: stream, Transparent: transparent, UsageFormat: "anthropic", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte})
	})
}

func (a *App) images(w http.ResponseWriter, r *http.Request, key authKey) {
	if !key.AllowImages {
		fail(w, http.StatusForbidden, "images_not_allowed", "this key is not permitted to generate images")
		return
	}
	// Fail over when the chosen channel never produced a client-visible response
	// (connect/timeout, 401/403/429/5xx before body copy). Once headers/body are
	// written for a terminal non-retryable result, runRoutes stops as usual.
	a.openAIEndpoint(w, r, key, "openai_images", "/v1/images/generations", "image", true)
}
