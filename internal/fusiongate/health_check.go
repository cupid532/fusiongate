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
	Status    string
	LatencyMS int64
	Error     string
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
	// 随机延迟避免批量探测特征
	if h.maxDelay > h.minDelay {
		jitter := h.minDelay + time.Duration(rand.Int63n(int64(h.maxDelay-h.minDelay)))
		time.Sleep(jitter)
	}

	ctx, cancel := context.WithTimeout(parent, h.timeout)
	defer cancel()

	result := h.probeProvider(ctx, providerID)
	h.updateHealthStatus(providerID, result)
}

func (h *HealthChecker) probeProvider(ctx context.Context, providerID int64) healthCheckResult {
	start := time.Now()

	// 加载 provider 信息
	p, err := h.app.loadDiscoveryProvider(ctx, providerID)
	if err != nil {
		return healthCheckResult{Status: "config_error", Error: "failed to load provider"}
	}

	// OAuth 凭证：探测前先检查是否需要续签
	if p.AuthCredential != nil && p.AuthCredential.RefreshToken != "" {
		expires := parseTime(p.AuthCredential.ExpiresAt)
		if expires == nil || !expires.After(time.Now().Add(2*time.Minute)) {
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

	// 根据 provider 类型选择探测模型
	probeModel := h.selectProbeModel(p.Type)
	if probeModel == "" {
		return healthCheckResult{Status: "unsupported", Error: "provider type does not support health check"}
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
		return healthCheckResult{Status: "config_error", Error: err.Error()}
	}

	// 发起探测请求
	resp, err := h.app.client.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		if ctx.Err() != nil {
			return healthCheckResult{Status: "timeout", LatencyMS: latency, Error: "request timeout"}
		}
		return healthCheckResult{Status: "network_error", LatencyMS: latency, Error: sanitizeError(err.Error())}
	}
	defer resp.Body.Close()

	// 读取响应（限制大小）
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<10))

	// 解析状态
	status, errMsg := h.parseProbeResponse(resp.StatusCode, body)
	return healthCheckResult{Status: status, LatencyMS: latency, Error: errMsg}
}

func (h *HealthChecker) selectProbeModel(providerType string) string {
	switch providerType {
	case "grok_oauth":
		return "grok-2-mini"
	case "codex_oauth":
		return "gpt-4o-mini"
	case "claude_oauth":
		return "claude-3-5-haiku-20241022"
	case "openai", "openai_compatible":
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
	req.Header.Set("Accept", "application/json")

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
	case 400:
		// 400 可能是模型不存在，也算健康（token有效）
		return "healthy", ""
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
		    updated_at=?
		WHERE id=?
	`, now(), result.Status, result.Error, result.LatencyMS, now(), providerID)

	if err != nil {
		h.app.log.Error("failed to update health status", "provider_id", providerID, "error", err)
	}
}

// CheckProviderNow 立即检查单个 provider（同步）
func (a *App) CheckProviderNow(ctx context.Context, providerID int64) (healthCheckResult, error) {
	var authKind string
	var enabled int
	err := a.db.QueryRowContext(ctx, `SELECT auth_kind, enabled FROM providers WHERE id=?`, providerID).Scan(&authKind, &enabled)
	if err != nil {
		return healthCheckResult{}, errors.New("provider not found")
	}
	if authKind != "oauth" {
		return healthCheckResult{}, errors.New("health check only supports OAuth providers")
	}

	checker := NewHealthChecker(a, 15*time.Minute, 1)
	result := checker.probeProvider(ctx, providerID)
	checker.updateHealthStatus(providerID, result)
	return result, nil
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
