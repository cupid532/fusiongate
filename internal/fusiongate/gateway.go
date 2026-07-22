package fusiongate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type authKey struct {
	ID          int64
	Hash        string
	AllowAll    bool
	AllowModels string
	DenyModels  string
	AllowImages bool
	RPMLimit    int
	Revoked     bool
	ExpiresAt   *time.Time
}

type resolvedRoute struct {
	Route      Route
	Provider   Provider
	Credential string
}

func (a *App) api(fn func(http.ResponseWriter, *http.Request, authKey)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k, ok := a.authenticateKey(r)
		if !ok {
			fail(w, http.StatusUnauthorized, "invalid_api_key", "missing, expired, revoked, or invalid API key")
			return
		}
		if !a.allowRate(k) {
			fail(w, http.StatusTooManyRequests, "rate_limit_exceeded", "API key rate limit exceeded")
			return
		}
		fn(w, r, k)
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
	err := a.db.QueryRow(`SELECT id,name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,revoked,COALESCE(expires_at,''),created_at FROM api_keys WHERE key_hash=?`, hex.EncodeToString(sum[:])).Scan(&x.ID, new(string), new(string), &x.Hash, &allowAll, &x.AllowModels, &x.DenyModels, &allowImages, &x.RPMLimit, &revoked, &expiresAt, &createdAt)
	if err != nil {
		return authKey{}, false
	}
	x.AllowAll = strBool(allowAll)
	x.AllowImages = strBool(allowImages)
	x.Revoked = strBool(revoked)
	x.ExpiresAt = parseTime(expiresAt)
	if x.Revoked || (x.ExpiresAt != nil && time.Now().After(*x.ExpiresAt)) {
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
SELECT r.id,r.provider_id,r.public_name,r.upstream_model,r.capabilities,r.enabled,r.priority,r.sort_order,r.input_price_micros,r.output_price_micros,
       p.id,p.name,p.type,p.base_url,p.credential,p.enabled,p.priority,p.weight,p.status,p.notes,
       p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,
       p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,
       COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,'')
FROM model_routes r JOIN providers p ON p.id=r.provider_id
WHERE r.public_name=? AND r.enabled=1 AND p.enabled=1
ORDER BY p.priority DESC,p.id,r.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []resolvedRoute{}
	for rows.Next() {
		var z resolvedRoute
		var routeEnabled, providerEnabled int
		var credential []byte
		if err := rows.Scan(
			&z.Route.ID, &z.Route.ProviderID, &z.Route.PublicName, &z.Route.UpstreamModel, &z.Route.Capabilities, &routeEnabled,
			&z.Route.Priority, &z.Route.SortOrder, &z.Route.InputPriceMicros, &z.Route.OutputPriceMicros,
			&z.Provider.ID, &z.Provider.Name, &z.Provider.Type, &z.Provider.BaseURL, &credential, &providerEnabled,
			&z.Provider.Priority, &z.Provider.Weight, &z.Provider.Status, &z.Provider.Notes,
			&z.Provider.PassthroughMode, &z.Provider.ClientPolicy, &z.Provider.MaxConcurrency, &z.Provider.RequestTimeoutMS,
			&z.Provider.FailureThreshold, &z.Provider.CooldownSeconds, &z.Provider.ConsecutiveFailures,
			&z.Provider.CircuitOpenUntil, &z.Provider.LastError, &z.Provider.LastLatencyMS,
			&z.Provider.LastSuccessAt, &z.Provider.LastFailureAt,
		); err != nil {
			return nil, err
		}
		z.Route.Enabled = strBool(routeEnabled)
		z.Provider.Enabled = strBool(providerEnabled)
		if !matchesCapability(z.Route.Capabilities, requiredCapability) {
			continue
		}
		z.Credential, err = a.decrypt(credential)
		if err != nil {
			return nil, fmt.Errorf("cannot decrypt provider %s credential: %w", z.Provider.Name, err)
		}
		out = append(out, z)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no eligible route for model %q", model)
	}
	return out, nil
}

func requestID() string { return "req_" + hex.EncodeToString(randomBytes(12)) }

func (a *App) startLedger(k authKey, z resolvedRoute, protocol string, stream bool, gatewayID string, attempt int, retryReason string) (int64, string) {
	attemptID := gatewayID + "_a" + strconv.Itoa(attempt)
	res, err := a.db.Exec(`INSERT INTO request_ledger(request_id,gateway_request_id,attempt,retry_reason,created_at,api_key_id,provider_id,route_id,public_model,upstream_model,protocol,stream) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, gatewayID, attempt, retryReason, now(), k.ID, z.Provider.ID, z.Route.ID, z.Route.PublicName, z.Route.UpstreamModel, protocol, boolInt(stream))
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
	_, err := a.db.Exec(`UPDATE request_ledger SET completed_at=?,success=?,status_code=?,error_type=?,latency_ms=?,input_tokens=?,output_tokens=?,cached_tokens=?,reasoning_tokens=?,cost_micros=?,cost_type=? WHERE id=?`, now(), boolInt(success), status, errorType, time.Since(start).Milliseconds(), usage.Input, usage.Output, usage.Cached, usage.Reasoning, usage.CostMicros, usage.CostType, id)
	if err != nil {
		a.log.Error("ledger update", "error", err)
	}
}

func cost(z resolvedRoute, usage *Usage) {
	if usage.CostMicros > 0 {
		usage.CostType = "actual"
		return
	}
	if z.Route.InputPriceMicros > 0 || z.Route.OutputPriceMicros > 0 {
		usage.CostMicros = (usage.Input*z.Route.InputPriceMicros + usage.Output*z.Route.OutputPriceMicros) / 1_000_000
		usage.CostType = "estimated"
	} else {
		usage.CostType = "unknown"
	}
}

func getBody(r *http.Request) (map[string]any, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
	if err != nil {
		return nil, nil, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, err
	}
	return parsed, body, nil
}

func parseOpenAIUsage(payload map[string]any) Usage {
	usage := Usage{CostType: "unknown"}
	m, _ := payload["usage"].(map[string]any)
	usage.Input = num(m["prompt_tokens"])
	if usage.Input == 0 {
		usage.Input = num(m["input_tokens"])
	}
	usage.Output = num(m["completion_tokens"])
	if usage.Output == 0 {
		usage.Output = num(m["output_tokens"])
	}
	if details, ok := m["prompt_tokens_details"].(map[string]any); ok {
		usage.Cached = num(details["cached_tokens"])
	}
	if details, ok := m["completion_tokens_details"].(map[string]any); ok {
		usage.Reasoning = num(details["reasoning_tokens"])
	}
	return usage
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
	gatewayID := requestID()
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
		tried[z.Route.ID] = true
		started := time.Now()
		ledgerID, attemptID := a.startLedger(key, z, protocol, stream, gatewayID, attempt, previousReason)
		result := execute(z, attemptID, func() { a.recordFirstByte(ledgerID, started) })
		latency := time.Since(started)
		a.completeRoute(z, result, latency)
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
	if !transparent {
		body, err = normalizedOpenAIBody(raw, z.Route.UpstreamModel, stream)
		if err != nil {
			return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
		}
	}
	return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: endpoint, RawBody: body, Stream: stream, Transparent: transparent, ParseOpenAIUse: true, GatewayID: requestID, SafeTransportRetry: safeTransportRetry, OnFirstByte: onFirstByte})
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
		case "openai", "openrouter", "openai_compatible":
			return a.openAIProxy(w, r, raw, z, rid, "/v1/chat/completions", stream, true, onFirstByte)
		case "anthropic":
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
	copyUpstreamRequestHeaders(req.Header, r.Header)
	req.Header.Set("x-api-key", z.Credential)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := a.client.Do(req)
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
	usage := Usage{}
	if sourceUsage, ok := source["usage"].(map[string]any); ok {
		usage.Input = num(sourceUsage["input_tokens"])
		usage.Output = num(sourceUsage["output_tokens"])
		usage.Cached = num(sourceUsage["cache_read_input_tokens"])
	}
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
	resp, err := a.client.Do(req)
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
	usage := Usage{}
	if metadata, ok := source["usageMetadata"].(map[string]any); ok {
		usage.Input = num(metadata["promptTokenCount"])
		usage.Output = num(metadata["candidatesTokenCount"])
		usage.Cached = num(metadata["cachedContentTokenCount"])
	}
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
		if z.Provider.Type == "openai" || z.Provider.Type == "openrouter" || z.Provider.Type == "openai_compatible" {
			compatible = append(compatible, z)
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no OpenAI-compatible route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, protocol, stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
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
		if z.Provider.Type == "anthropic" {
			compatible = append(compatible, z)
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no Anthropic route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, "anthropic_messages", stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		transparent := z.Provider.PassthroughMode == "transparent"
		encoded := raw
		if !transparent {
			copyBody := make(map[string]any, len(body))
			for key, value := range body {
				copyBody[key] = value
			}
			copyBody["model"] = z.Route.UpstreamModel
			encoded, err = json.Marshal(copyBody)
			if err != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
			}
		}
		return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/messages", RawBody: encoded, Stream: stream, Transparent: transparent, GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte})
	})
}

func (a *App) images(w http.ResponseWriter, r *http.Request, key authKey) {
	if !key.AllowImages {
		fail(w, http.StatusForbidden, "images_not_allowed", "this key is not permitted to generate images")
		return
	}
	// Image generation may have side effects. Transport failures are not replayed because
	// the gateway cannot prove the upstream did not already accept the job.
	a.openAIEndpoint(w, r, key, "openai_images", "/v1/images/generations", "image", false)
}
