package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HealthChecker 后台定期检查 OAuth provider 的真实可用性
type HealthChecker struct {
	app           *App
	interval      time.Duration
	maxConcurrent int
	timeout       time.Duration
	minDelay      time.Duration
	maxDelay      time.Duration
	enabled       bool
	mu            sync.Mutex
	running       bool
	cancel        context.CancelFunc
}

type healthCheckResult struct {
	Status      string
	Mode        string
	LatencyMS   int64
	FirstByteMS int64
	Model       string
	ModelCount  int
	Error       string
}

type generationProbe struct {
	Prompt string
	Answer string
}

const (
	healthCheckModeConnectivity = "connectivity"
	healthCheckModeGeneration   = "generation"
)

func validHealthCheckMode(mode string) bool {
	return mode == healthCheckModeConnectivity || mode == healthCheckModeGeneration
}

func NewHealthChecker(app *App, interval time.Duration, maxConcurrent int) *HealthChecker {
	timeout := 10 * time.Second
	if interval > 0 && interval < timeout {
		timeout = interval / 2
	}
	return &HealthChecker{
		app:           app,
		interval:      interval,
		maxConcurrent: maxConcurrent,
		timeout:       timeout,
		minDelay:      1 * time.Second,
		maxDelay:      5 * time.Second,
		enabled:       interval > 0,
	}
}

func (h *HealthChecker) Start(ctx context.Context) {
	if !h.enabled || h.interval <= 0 {
		return
	}
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	h.running = true
	ctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.mu.Unlock()

	go h.run(ctx)
}

func (h *HealthChecker) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
	h.running = false
}

func (h *HealthChecker) run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	// 启动时立即检查一次（延迟30秒避免启动拥堵）
	time.AfterFunc(30*time.Second, func() {
		h.checkBatch(ctx)
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkBatch(ctx)
		}
	}
}

func (h *HealthChecker) checkBatch(parent context.Context) {
	if parent.Err() != nil {
		return
	}

	// 查询需要检查的 OAuth providers（按上次检查时间排序，优先检查旧的）
	rows, err := h.app.db.Query(`
		SELECT id FROM providers 
		WHERE auth_kind='oauth' AND enabled=1
		ORDER BY COALESCE(last_health_check_at, '1970-01-01') ASC
		LIMIT 100
	`)
	if err != nil {
		h.app.log.Error("health check query failed", "error", err)
		return
	}

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()

	if len(ids) == 0 {
		return
	}

	// 使用信号量限制并发
	sem := make(chan struct{}, h.maxConcurrent)
	var wg sync.WaitGroup

	for _, id := range ids {
		if parent.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(providerID int64) {
			defer func() {
				<-sem
				wg.Done()
			}()
			h.checkProvider(parent, providerID)
		}(id)
	}

	wg.Wait()
}

func (h *HealthChecker) checkProvider(parent context.Context, providerID int64) {
	if !h.app.beginHealthProbe(providerID) {
		return
	}
	defer h.app.endHealthProbe(providerID)

	// 随机延迟避免批量探测特征；等待可取消，停机时不会拖住任务。
	if h.maxDelay > h.minDelay {
		jitter := h.minDelay + time.Duration(rand.Int63n(int64(h.maxDelay-h.minDelay)))
		timer := time.NewTimer(jitter)
		select {
		case <-parent.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}

	ctx, cancel := context.WithTimeout(parent, h.timeout)
	defer cancel()

	// Background checks deliberately use the read-only model-list endpoint. A
	// real generation probe remains an explicit manual action because it can
	// consume quota and is more likely to trigger upstream rate limits.
	result := h.probeProviderMode(ctx, providerID, healthCheckModeConnectivity)
	h.updateHealthStatus(providerID, result)
}

func (h *HealthChecker) probeProvider(ctx context.Context, providerID int64) healthCheckResult {
	return h.probeProviderMode(ctx, providerID, healthCheckModeGeneration)
}

func (h *HealthChecker) probeProviderMode(ctx context.Context, providerID int64, mode string) healthCheckResult {
	if !validHealthCheckMode(mode) {
		return healthCheckResult{Status: "config_error", Mode: mode, Error: "invalid health check mode"}
	}
	start := time.Now()

	// 加载 provider 信息
	p, err := h.app.loadDiscoveryProvider(ctx, providerID)
	if err != nil {
		return healthCheckResult{Status: "config_error", Mode: mode, Error: "failed to load provider"}
	}

	// OAuth 凭证：探测前先检查是否需要续签
	if p.AuthCredential != nil && p.AuthCredential.RefreshToken != "" {
		expires := parseTime(p.AuthCredential.ExpiresAt)
		if expires == nil || !expires.After(time.Now().Add(oauthRefreshLeadTime())) {
			// 构造最小 resolvedRoute 用于调用 refresh
			z := &resolvedRoute{
				Provider: Provider{
					ID:   p.ID,
					Name: p.Name,
					Type: p.Type,
				},
				AuthCredential: p.AuthCredential,
				Credential:     p.Credential,
			}
			if refreshErr := h.app.refreshProviderCredential(ctx, z, false); refreshErr == nil {
				p.Credential = z.Credential
				p.AuthCredential = z.AuthCredential
			}
			// 刷新失败时仍用旧 token 尝试，health_check_error 会说明情况
		}
	}

	if mode == healthCheckModeConnectivity {
		models, discoveryErr := h.app.fetchDiscoveredModels(ctx, p)
		latency := time.Since(start).Milliseconds()
		if discoveryErr == nil {
			return healthCheckResult{Status: "healthy", Mode: mode, LatencyMS: latency, ModelCount: len(models)}
		}
		if ctx.Err() != nil {
			return healthCheckResult{Status: "timeout", Mode: mode, LatencyMS: latency, Error: "request timeout"}
		}
		var httpErr *discoveryHTTPError
		if errors.As(discoveryErr, &httpErr) {
			status, message := h.parseProbeResponse(httpErr.Status, nil)
			return healthCheckResult{Status: status, Mode: mode, LatencyMS: latency, Error: message}
		}
		return healthCheckResult{Status: "network_error", Mode: mode, LatencyMS: latency, Error: sanitizeError(discoveryErr.Error())}
	}

	// 根据 provider 类型选择探测模型
	probeModel := h.selectProbeModel(ctx, p)
	if probeModel == "" {
		return healthCheckResult{Status: "unsupported", Mode: mode, Error: "provider type does not support health check"}
	}

	// 构造最小请求（1 token）
	reqBody := map[string]interface{}{
		"model": probeModel,
		"messages": []map[string]string{
			{"role": "user", "content": "Hi"},
		},
		"max_tokens": 1,
		"stream":     false,
	}

	// 特殊处理不同 API 格式
	endpoint := h.buildProbeEndpoint(p)
	req, err := h.buildProbeRequest(ctx, p, endpoint, reqBody)
	if err != nil {
		return healthCheckResult{Status: "config_error", Mode: mode, Model: probeModel, Error: err.Error()}
	}

	// 发起探测请求
	resp, err := h.app.client.Do(req)
	firstByte := time.Since(start).Milliseconds()

	if err != nil {
		latency := time.Since(start).Milliseconds()
		if ctx.Err() != nil {
			return healthCheckResult{Status: "timeout", Mode: mode, Model: probeModel, LatencyMS: latency, Error: "request timeout"}
		}
		return healthCheckResult{Status: "network_error", Mode: mode, Model: probeModel, LatencyMS: latency, Error: sanitizeError(err.Error())}
	}
	defer resp.Body.Close()

	// 读取响应（限制大小）
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<10))
	latency := time.Since(start).Milliseconds()

	// 解析状态
	status, errMsg := h.parseProbeResponse(resp.StatusCode, body)
	if p.Type == "codex_oauth" {
		status, errMsg = h.parseCodexProbeResponse(resp.StatusCode, body)
	}
	return healthCheckResult{Status: status, Mode: mode, Model: probeModel, LatencyMS: latency, FirstByteMS: firstByte, Error: errMsg}
}

func newGenerationProbe() generationProbe {
	a := 120 + rand.Intn(780)
	b := 20 + rand.Intn(79)
	return generationProbe{
		Prompt: fmt.Sprintf("I am reconciling a small spreadsheet. What is %d + %d? Reply with only the number.", a, b),
		Answer: strconv.Itoa(a + b),
	}
}

func (h *HealthChecker) probeRoute(ctx context.Context, target healthCheckTarget) healthCheckResult {
	start := time.Now()
	p, err := h.app.loadDiscoveryProvider(ctx, target.ProviderID)
	if err != nil {
		return healthCheckResult{Status: "config_error", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, Error: "failed to load provider"}
	}
	if p.AuthCredential != nil && p.AuthCredential.RefreshToken != "" {
		expires := parseTime(p.AuthCredential.ExpiresAt)
		if expires == nil || !expires.After(time.Now().Add(oauthRefreshLeadTime())) {
			z := &resolvedRoute{Provider: Provider{ID: p.ID, Name: p.Name, Type: p.Type}, AuthCredential: p.AuthCredential, Credential: p.Credential}
			if h.app.refreshProviderCredential(ctx, z, false) == nil {
				p.Credential, p.AuthCredential = z.Credential, z.AuthCredential
			}
		}
	}
	if !strings.Contains(target.Capabilities, "chat") {
		return healthCheckResult{Status: "unsupported", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, Error: "only chat models support generation health checks"}
	}
	probe := newGenerationProbe()
	req, err := h.buildRouteProbeRequest(ctx, p, target.UpstreamModel, probe.Prompt)
	if err != nil {
		return healthCheckResult{Status: "config_error", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, Error: sanitizeError(err.Error())}
	}
	resp, err := h.app.client.Do(req)
	firstByte := time.Since(start).Milliseconds()
	if err != nil {
		latency := time.Since(start).Milliseconds()
		if ctx.Err() != nil {
			return healthCheckResult{Status: "timeout", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, Error: "request timeout"}
		}
		return healthCheckResult{Status: "network_error", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, Error: sanitizeError(err.Error())}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	latency := time.Since(start).Milliseconds()
	if readErr != nil {
		return healthCheckResult{Status: "invalid_response", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, FirstByteMS: firstByte, Error: "could not read generation response"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status, message := h.parseProbeResponse(resp.StatusCode, body)
		return healthCheckResult{Status: status, Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, FirstByteMS: firstByte, Error: message}
	}
	content, err := extractProbeContent(p.Type, body)
	if err != nil {
		return healthCheckResult{Status: "invalid_response", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, FirstByteMS: firstByte, Error: sanitizeError(err.Error())}
	}
	if normalizeProbeAnswer(content) != probe.Answer {
		return healthCheckResult{Status: "content_mismatch", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, FirstByteMS: firstByte, Error: "model returned an unexpected answer"}
	}
	return healthCheckResult{Status: "healthy", Mode: healthCheckModeGeneration, Model: target.UpstreamModel, LatencyMS: latency, FirstByteMS: firstByte}
}

func healthProbeURL(base, endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(u.Path, "/")
	for _, version := range []string{"/v1", "/v1beta"} {
		if strings.HasSuffix(basePath, version) && strings.HasPrefix(endpoint, version+"/") {
			endpoint = strings.TrimPrefix(endpoint, version)
			break
		}
	}
	u.Path = basePath + endpoint
	return u.String(), nil
}

func (h *HealthChecker) buildRouteProbeRequest(ctx context.Context, p discoveryProvider, model, prompt string) (*http.Request, error) {
	endpoint := "/v1/chat/completions"
	body := map[string]any{
		"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0, "max_tokens": 32, "stream": false,
	}
	if p.Type == "anthropic" || p.Type == "claude_oauth" {
		endpoint = "/v1/messages"
		body = map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}, "temperature": 0, "max_tokens": 32}
	} else if p.Type == "gemini" {
		endpoint = "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}}, "generationConfig": map[string]any{"temperature": 0, "maxOutputTokens": 32}}
	} else if p.Type == "codex_oauth" {
		endpoint = "/responses"
		body = map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}, "store": false, "stream": true}
	}
	upstreamURL, err := healthProbeURL(p.BaseURL, endpoint)
	if err != nil {
		return nil, err
	}
	if p.Type == "gemini" {
		u, parseErr := url.Parse(upstreamURL)
		if parseErr != nil {
			return nil, parseErr
		}
		q := u.Query()
		q.Set("key", p.Credential)
		u.RawQuery = q.Encode()
		upstreamURL = u.String()
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDiscoveryAuth(req, p)
	req.Header.Set("Accept", "application/json")
	if p.Type == "codex_oauth" {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

func extractProbeContent(providerType string, body []byte) (string, error) {
	if providerType == "codex_oauth" {
		completed, _, err := completedResponseFromSSE(body)
		if err != nil {
			return "", err
		}
		body = completed
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return "", errors.New("generation response was not valid JSON")
	}
	if providerType == "anthropic" || providerType == "claude_oauth" {
		var content string
		for _, item := range anySlice(data["content"]) {
			part, _ := item.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				content += text
			}
		}
		if content == "" {
			return "", errors.New("generation response contained no assistant text")
		}
		return content, nil
	}
	if providerType == "gemini" {
		candidates := anySlice(data["candidates"])
		if len(candidates) > 0 {
			candidate, _ := candidates[0].(map[string]any)
			contentObj, _ := candidate["content"].(map[string]any)
			var content string
			for _, item := range anySlice(contentObj["parts"]) {
				part, _ := item.(map[string]any)
				if text, _ := part["text"].(string); text != "" {
					content += text
				}
			}
			if content != "" {
				return content, nil
			}
		}
		return "", errors.New("generation response contained no assistant text")
	}
	if output := anySlice(data["output"]); len(output) > 0 {
		var content string
		for _, item := range output {
			message, _ := item.(map[string]any)
			for _, value := range anySlice(message["content"]) {
				part, _ := value.(map[string]any)
				if text, _ := part["text"].(string); text != "" {
					content += text
				}
			}
		}
		if content != "" {
			return content, nil
		}
	}
	choices := anySlice(data["choices"])
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if content := textContent(message["content"]); content != "" {
			return content, nil
		}
	}
	return "", errors.New("generation response contained no assistant text")
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func normalizeProbeAnswer(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`*_.,!?:; ")
	return value
}

func (h *HealthChecker) selectProbeModel(ctx context.Context, p discoveryProvider) string {
	var configured string
	if err := h.app.db.QueryRowContext(ctx, `
		SELECT upstream_model FROM model_routes
		WHERE provider_id=? AND enabled=1
		ORDER BY priority DESC, sort_order ASC, id ASC
		LIMIT 1
	`, p.ID).Scan(&configured); err == nil && strings.TrimSpace(configured) != "" {
		return configured
	}
	switch p.Type {
	case "grok_oauth":
		return "grok-2-mini"
	case "codex_oauth":
		return "gpt-5.4"
	case "claude_oauth":
		return "claude-3-5-haiku-20241022"
	case "openai", "grok", "openai_compatible":
		return "gpt-4o-mini"
	case "anthropic":
		return "claude-3-5-haiku-20241022"
	default:
		return ""
	}
}

func (h *HealthChecker) buildProbeEndpoint(p discoveryProvider) string {
	base := strings.TrimRight(p.BaseURL, "/")
	switch p.Type {
	case "anthropic", "claude_oauth":
		return base + "/v1/messages"
	case "grok_oauth":
		return base + "/v1/responses"
	case "codex_oauth":
		return base + "/responses"
	default:
		return base + "/v1/chat/completions"
	}
}

func (h *HealthChecker) buildProbeRequest(ctx context.Context, p discoveryProvider, endpoint string, body map[string]interface{}) (*http.Request, error) {
	// Anthropic 使用不同的请求格式
	if p.Type == "anthropic" || p.Type == "claude_oauth" {
		body = map[string]interface{}{
			"model": body["model"],
			"messages": []map[string]string{
				{"role": "user", "content": "Hi"},
			},
			"max_tokens": 1,
		}
	} else if p.Type == "codex_oauth" {
		body = map[string]interface{}{
			"model": body["model"],
			"input": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "Reply OK",
				}},
			}},
			"store":  false,
			"stream": true,
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	// 设置认证头
	req.Header.Set("Content-Type", "application/json")
	if p.Type == "codex_oauth" {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	switch p.Type {
	case "grok_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		setGrokClientHeaders(req.Header)
	case "codex_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		if p.AuthCredential != nil && p.AuthCredential.AccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", p.AuthCredential.AccountID)
		}
		setCodexClientHeaders(req.Header)
	case "claude_oauth":
		req.Header.Set("Authorization", "Bearer "+p.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
		req.Header.Set("x-app", "cli")
	case "anthropic":
		req.Header.Set("x-api-key", p.Credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+p.Credential)
	}

	return req, nil
}

func (h *HealthChecker) parseCodexProbeResponse(statusCode int, body []byte) (string, string) {
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		if statusCode == http.StatusBadRequest {
			return "config_error", parseErrorMessage(body, "Codex rejected the probe request")
		}
		return h.parseProbeResponse(statusCode, body)
	}
	if _, _, err := completedResponseFromSSE(body); err == nil {
		return "healthy", ""
	}
	var response map[string]any
	if json.Unmarshal(body, &response) == nil {
		if status, _ := response["status"].(string); status == "completed" {
			return "healthy", ""
		}
	}
	return "unknown_error", "Codex probe stream ended without response.completed"
}

func (h *HealthChecker) parseProbeResponse(statusCode int, body []byte) (string, string) {
	switch statusCode {
	case 200, 201:
		return "healthy", ""
	case 401, 403:
		return "auth_expired", parseErrorMessage(body, "authentication failed")
	case 429:
		return "rate_limited", parseErrorMessage(body, "rate limit exceeded")
	case 408, 504:
		return "timeout", parseErrorMessage(body, "upstream timeout")
	case 400, 404, 422:
		return "config_error", parseErrorMessage(body, fmt.Sprintf("upstream rejected the generation request (HTTP %d)", statusCode))
	default:
		if statusCode >= 500 {
			return "server_error", parseErrorMessage(body, fmt.Sprintf("HTTP %d", statusCode))
		}
		return "unknown_error", fmt.Sprintf("HTTP %d", statusCode)
	}
}

func parseErrorMessage(body []byte, fallback string) string {
	if len(body) == 0 {
		return fallback
	}
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err == nil {
		if errObj, ok := data["error"].(map[string]interface{}); ok {
			if msg, ok := errObj["message"].(string); ok && msg != "" {
				return truncate(msg, 200)
			}
		}
		if msg, ok := data["message"].(string); ok && msg != "" {
			return truncate(msg, 200)
		}
		if detail, ok := data["detail"].(string); ok && detail != "" {
			return truncate(detail, 200)
		}
	}
	return fallback
}

func (h *HealthChecker) updateHealthStatus(providerID int64, result healthCheckResult) {
	_, err := h.app.db.Exec(`
		UPDATE providers 
		SET last_health_check_at=?,
		    health_check_status=?,
		    health_check_error=?,
		    health_check_latency_ms=?,
		    health_check_mode=?,
		    health_check_first_byte_ms=?,
		    health_check_model=?,
		    health_check_model_count=?,
		    updated_at=?
		WHERE id=?
	`, now(), result.Status, result.Error, result.LatencyMS, result.Mode, result.FirstByteMS, result.Model, result.ModelCount, now(), providerID)

	if err != nil {
		h.app.log.Error("failed to update health status", "provider_id", providerID, "error", err)
	}
}

func (h *HealthChecker) updateRouteHealthStatus(routeID int64, result healthCheckResult) {
	_, err := h.app.db.Exec(`UPDATE model_routes SET last_health_check_at=?,health_check_status=?,health_check_error=?,health_check_latency_ms=?,health_check_first_byte_ms=?,updated_at=? WHERE id=?`, now(), result.Status, result.Error, result.LatencyMS, result.FirstByteMS, now(), routeID)
	if err != nil {
		h.app.log.Error("failed to update model health status", "route_id", routeID, "error", err)
	}
}

func (a *App) beginHealthProbe(providerID int64) bool {
	a.healthProbeMu.Lock()
	defer a.healthProbeMu.Unlock()
	if a.healthProbes == nil {
		a.healthProbes = make(map[int64]struct{})
	}
	if _, running := a.healthProbes[providerID]; running {
		return false
	}
	a.healthProbes[providerID] = struct{}{}
	return true
}

func (a *App) endHealthProbe(providerID int64) {
	a.healthProbeMu.Lock()
	delete(a.healthProbes, providerID)
	a.healthProbeMu.Unlock()
}

func sanitizeError(err string) string {
	err = strings.ReplaceAll(err, "\n", " ")
	err = strings.ReplaceAll(err, "\r", " ")
	return truncate(err, 200)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
