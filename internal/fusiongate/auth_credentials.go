package fusiongate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexOAuthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	claudeOAuthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthRedirectURI = "http://localhost:54545/callback"
	xaiOAuthClientID       = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiOAuthScope          = "openid profile email offline_access grok-cli:access api:access"
	authSessionTTL         = 15 * time.Minute
	authImportMaxBytes     = 8 << 20
	authExportMaxItems     = 200
)

var (
	codexOAuthAuthorizeURL  = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL      = "https://auth.openai.com/oauth/token"
	claudeOAuthAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeOAuthTokenURL     = "https://api.anthropic.com/v1/oauth/token"
	xaiOIDCDiscoveryURL     = "https://auth.x.ai/.well-known/openid-configuration"
)

type ProviderCredential struct {
	Version      int            `json:"version"`
	Kind         string         `json:"kind"`
	Platform     string         `json:"platform"`
	Source       string         `json:"source"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	IDToken      string         `json:"id_token,omitempty"`
	AccountID    string         `json:"account_id,omitempty"`
	Email        string         `json:"email,omitempty"`
	ExpiresAt    string         `json:"expires_at,omitempty"`
	LastRefresh  string         `json:"last_refresh,omitempty"`
	Scope        string         `json:"scope,omitempty"`
	Extra        map[string]any `json:"extra,omitempty"`
}

type oauthSession struct {
	Platform      string
	State         string
	Verifier      string
	Created       time.Time
	DeviceCode    string
	TokenEndpoint string
	PollInterval  time.Duration
	LastPoll      time.Time
	ExpiresAt     time.Time
}

type credentialImport struct {
	ID          int
	Name        string
	Credential  ProviderCredential
	Fingerprint string
	DuplicateID int64
	Status      string
}

type credentialImportSession struct {
	Created time.Time
	Items   []credentialImport
}

type authModelSyncTarget struct {
	ID   int64
	Name string
}

type authModelSyncItem struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Discovered int    `json:"discovered"`
	Added      int    `json:"added"`
	Existing   int    `json:"existing"`
	Skipped    int    `json:"skipped"`
	Error      string `json:"error,omitempty"`
}

type authModelSyncSummary struct {
	Providers int                 `json:"providers"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	Models    int                 `json:"models"`
	Added     int                 `json:"added"`
	Existing  int                 `json:"existing"`
	Items     []authModelSyncItem `json:"items"`
}

type credentialImportPreview struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	Source          string `json:"source"`
	Email           string `json:"email,omitempty"`
	AccountID       string `json:"account_id,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	Status          string `json:"status"`
	Duplicate       bool   `json:"duplicate"`
	DuplicateID     int64  `json:"duplicate_provider_id,omitempty"`
}

type credentialExportBundle struct {
	Version     int                     `json:"version"`
	Format      string                  `json:"format"`
	ExportedAt  string                  `json:"exported_at"`
	Credentials []credentialExportEntry `json:"credentials"`
}

type credentialExportEntry struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	Platform         string `json:"platform"`
	AuthKind         string `json:"auth_kind"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	AccountID        string `json:"account_id,omitempty"`
	ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`
	Subject          string `json:"sub,omitempty"`
	Email            string `json:"email,omitempty"`
	Expired          string `json:"expired,omitempty"`
	LastRefresh      string `json:"last_refresh,omitempty"`
	Scope            string `json:"scope,omitempty"`
	TokenEndpoint    string `json:"token_endpoint,omitempty"`
	BaseURL          string `json:"base_url,omitempty"`
	Priority         int    `json:"priority"`
	Enabled          bool   `json:"enabled"`
	Source           string `json:"source"`
}

type codexUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	RemainingPercent   float64 `json:"remaining_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds,omitempty"`
	ResetAt            string  `json:"reset_at,omitempty"`
}

type codexResetCard struct {
	ID        string `json:"id,omitempty"`
	Status    string `json:"status,omitempty"`
	ResetType string `json:"reset_type,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type codexAccountQuota struct {
	PlanType         string            `json:"plan_type,omitempty"`
	SubscriptionPlan string            `json:"subscription_plan,omitempty"`
	Allowed          bool              `json:"allowed"`
	LimitReached     bool              `json:"limit_reached"`
	Primary          *codexUsageWindow `json:"primary,omitempty"`
	Secondary        *codexUsageWindow `json:"secondary,omitempty"`
	ResetCards       int               `json:"reset_cards"`
	ResetCardDetails []codexResetCard  `json:"reset_card_details,omitempty"`
	CreditsBalance   *float64          `json:"credits_balance,omitempty"`
	CreditsUnlimited bool              `json:"credits_unlimited,omitempty"`
	// Legacy fields kept for older UI snippets.
	TotalQuota     float64 `json:"total_quota"`
	UsedQuota      float64 `json:"used_quota"`
	RemainingQuota float64 `json:"remaining_quota"`
	NextResetDate  string  `json:"next_reset_date,omitempty"`
}

func normalizeOAuthPlatform(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex", "openai", "chatgpt", "codex_oauth", "openai_oauth":
		return "codex"
	case "claude", "anthropic", "claude_oauth", "claude-code", "claude_code":
		return "claude"
	case "grok", "xai", "x.ai", "grok_oauth", "xai_oauth":
		return "grok"
	default:
		return ""
	}
}

func oauthProviderType(platform string) string {
	switch platform {
	case "codex":
		return "codex_oauth"
	case "grok":
		return "grok_oauth"
	default:
		return "claude_oauth"
	}
}

func oauthProviderBaseURL(platform string) string {
	switch platform {
	case "codex":
		return "https://chatgpt.com/backend-api/codex"
	case "grok":
		// FusionGate appends /v1 endpoints, so keep the base URL free of /v1.
		return "https://cli-chat-proxy.grok.com"
	default:
		return "https://api.anthropic.com"
	}
}

func pkceVerifier() string { return base64.RawURLEncoding.EncodeToString(randomBytes(32)) }
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *App) pruneAuthMemoryLocked(t time.Time) {
	for key, session := range a.oauthSessions {
		if t.Sub(session.Created) > authSessionTTL {
			delete(a.oauthSessions, key)
		}
	}
	for key, session := range a.authImports {
		if t.Sub(session.Created) > authSessionTTL {
			delete(a.authImports, key)
		}
	}
}

func isTrustedXAIEndpoint(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

type xaiOIDCConfiguration struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

func (a *App) xaiOIDCConfiguration(ctx context.Context) (xaiOIDCConfiguration, error) {
	return a.xaiOIDCConfigurationViaNode(ctx, nil)
}

func (a *App) xaiOIDCConfigurationViaNode(ctx context.Context, nodeID *int64) (xaiOIDCConfiguration, error) {
	if !isTrustedXAIEndpoint(xaiOIDCDiscoveryURL) {
		return xaiOIDCConfiguration{}, errors.New("xAI discovery configuration is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiOIDCDiscoveryURL, nil)
	if err != nil {
		return xaiOIDCConfiguration{}, errors.New("xAI discovery configuration is invalid")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.doProviderRequest(req, nodeID)
	if err != nil {
		return xaiOIDCConfiguration{}, errors.New("xAI authorization service is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return xaiOIDCConfiguration{}, errors.New("xAI authorization service is unavailable")
	}
	var config xaiOIDCConfiguration
	if json.Unmarshal(body, &config) != nil || !isTrustedXAIEndpoint(config.DeviceAuthorizationEndpoint) || !isTrustedXAIEndpoint(config.TokenEndpoint) {
		return xaiOIDCConfiguration{}, errors.New("xAI authorization configuration is invalid")
	}
	return config, nil
}

func (a *App) startXAIDeviceAuthorization(ctx context.Context) (oauthSession, map[string]any, error) {
	config, err := a.xaiOIDCConfiguration(ctx)
	if err != nil {
		return oauthSession{}, nil, err
	}
	form := url.Values{"client_id": {xaiOAuthClientID}, "scope": {xaiOAuthScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthSession{}, nil, errors.New("xAI authorization setup failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return oauthSession{}, nil, errors.New("xAI authorization service is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oauthSession{}, nil, errors.New("xAI device authorization could not be started")
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if json.Unmarshal(body, &device) != nil || strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" || !isTrustedXAIEndpoint(device.VerificationURI) {
		return oauthSession{}, nil, errors.New("xAI device authorization response is invalid")
	}
	if device.ExpiresIn <= 0 || time.Duration(device.ExpiresIn)*time.Second > authSessionTTL {
		device.ExpiresIn = int(authSessionTTL.Seconds())
	}
	if device.Interval < 2 {
		device.Interval = 5
	}
	sessionID := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	session := oauthSession{Platform: "grok", State: sessionID, Created: time.Now(), DeviceCode: device.DeviceCode, TokenEndpoint: config.TokenEndpoint, PollInterval: time.Duration(device.Interval) * time.Second, ExpiresAt: time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)}
	result := map[string]any{
		"session_id": sessionID, "platform": "grok", "flow": "device", "verification_url": device.VerificationURI,
		"user_code": device.UserCode, "expires_in": device.ExpiresIn, "poll_interval": device.Interval,
		"instruction": "请在新窗口完成 xAI · Grok 授权，再回到此处确认。认证码不会保存到页面或日志。",
	}
	if isTrustedXAIEndpoint(device.VerificationURIComplete) {
		result["verification_url_complete"] = device.VerificationURIComplete
	}
	return session, result, nil
}

func (a *App) oauthStart(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		Platform string `json:"platform"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	platform := normalizeOAuthPlatform(in.Platform)
	if platform == "" {
		fail(w, http.StatusBadRequest, "unsupported_platform", "only Codex, Claude, and Grok authorization are supported")
		return
	}
	if platform == "grok" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		session, response, err := a.startXAIDeviceAuthorization(ctx)
		if err != nil {
			fail(w, http.StatusBadGateway, "oauth_start_failed", "xAI device authorization could not be started")
			return
		}
		a.authMu.Lock()
		a.pruneAuthMemoryLocked(time.Now())
		a.oauthSessions[session.State] = session
		a.authMu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	verifier := pkceVerifier()
	params := url.Values{
		"client_id": {codexOAuthClientID}, "response_type": {"code"}, "redirect_uri": {codexOAuthRedirectURI},
		"scope": {"openid profile email offline_access api.connectors.read api.connectors.invoke"}, "state": {state}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"codex_cli_rs"},
	}
	authURL := codexOAuthAuthorizeURL + "?" + params.Encode()
	if platform == "claude" {
		params = url.Values{
			"code": {"true"}, "client_id": {claudeOAuthClientID}, "response_type": {"code"}, "redirect_uri": {claudeOAuthRedirectURI},
			"scope": {"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"},
			"state": {state}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"},
		}
		authURL = claudeOAuthAuthorizeURL + "?" + params.Encode()
	}
	a.authMu.Lock()
	a.pruneAuthMemoryLocked(time.Now())
	a.oauthSessions[state] = oauthSession{Platform: platform, State: state, Verifier: verifier, Created: time.Now()}
	a.authMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": state, "platform": platform, "auth_url": authURL, "expires_in": int(authSessionTTL.Seconds()),
		"instruction": "授权后请复制浏览器地址栏中的完整 localhost 回调地址并粘贴回来",
	})
}

func parseOAuthCallback(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("callback URL or authorization code is required")
	}
	if strings.Contains(raw, "://") {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			return "", "", errors.New("invalid callback URL")
		}
		code = strings.TrimSpace(u.Query().Get("code"))
		state = strings.TrimSpace(u.Query().Get("state"))
		if oauthError := strings.TrimSpace(u.Query().Get("error")); oauthError != "" {
			description := strings.TrimSpace(u.Query().Get("error_description"))
			if description != "" {
				return "", state, fmt.Errorf("authorization was denied: %s", sanitizeOAuthDetail(description))
			}
			return "", state, fmt.Errorf("authorization was denied: %s", sanitizeOAuthDetail(oauthError))
		}
		if state == "" {
			state = strings.TrimSpace(u.Fragment)
		}
		if parts := strings.SplitN(code, "#", 2); len(parts) == 2 {
			code = strings.TrimSpace(parts[0])
			if state == "" {
				state = strings.TrimSpace(parts[1])
			}
		}
	} else {
		parts := strings.SplitN(raw, "#", 2)
		code = strings.TrimSpace(parts[0])
		if len(parts) == 2 {
			state = strings.TrimSpace(parts[1])
		}
	}
	if code == "" {
		return "", "", errors.New("authorization code is missing")
	}
	return code, state, nil
}

func sanitizeOAuthDetail(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		value = value[:240] + "…"
	}
	return value
}

func publicOAuthExchangeError(err error) string {
	if err == nil {
		return "unknown authentication error"
	}
	detail := sanitizeOAuthDetail(err.Error())
	lower := strings.ToLower(detail)
	for _, sensitive := range []string{"access_token", "refresh_token", "id_token", "authorization: bearer", "client_secret"} {
		if strings.Contains(lower, sensitive) {
			return "authentication service rejected the authorization code"
		}
	}
	return detail
}

func (a *App) oauthComplete(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
		Callback  string `json:"callback"`
		Name      string `json:"name"`
		Priority  *int   `json:"priority"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	priority := 1
	if in.Priority != nil {
		priority = *in.Priority
	}
	if priority < 0 {
		fail(w, http.StatusBadRequest, "invalid_priority", "priority must be zero or greater")
		return
	}
	sessionID := strings.TrimSpace(in.SessionID)
	a.authMu.Lock()
	a.pruneAuthMemoryLocked(time.Now())
	session, ok := a.oauthSessions[sessionID]
	a.authMu.Unlock()
	if !ok || time.Since(session.Created) > authSessionTTL || (!session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt)) {
		fail(w, http.StatusBadRequest, "oauth_session_expired", "authorization session expired; start again")
		return
	}
	if session.Platform == "grok" {
		nowTime := time.Now()
		a.authMu.Lock()
		// Check and update under one lock so parallel requests cannot over-poll the device endpoint.
		session, ok = a.oauthSessions[sessionID]
		retryAfter := 0
		if ok && !session.LastPoll.IsZero() && nowTime.Sub(session.LastPoll) < session.PollInterval {
			wait := session.PollInterval - nowTime.Sub(session.LastPoll)
			retryAfter = max(1, int(wait.Seconds())+1)
		} else if ok {
			session.LastPoll = nowTime
			a.oauthSessions[sessionID] = session
		}
		a.authMu.Unlock()
		if !ok {
			fail(w, http.StatusBadRequest, "oauth_session_expired", "authorization session expired; start again")
			return
		}
		if retryAfter > 0 {
			writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "retry_after": retryAfter})
			return
		}
		credential, pending, retryAfter, err := a.pollXAIDeviceAuthorization(r.Context(), session)
		if err != nil {
			fail(w, http.StatusBadGateway, "oauth_exchange_failed", "xAI authorization could not be completed; start a new login and try again")
			return
		}
		if pending {
			writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "retry_after": retryAfter})
			return
		}
		id, createdName, err := a.saveOAuthProvider(r.Context(), strings.TrimSpace(in.Name), priority, credential, 0, false)
		if err != nil {
			if errors.Is(err, errDuplicateCredential) {
				fail(w, http.StatusConflict, "credential_exists", "this authorized account already exists")
			} else {
				a.log.Error("OAuth credential save failed", "error", err)
				fail(w, http.StatusInternalServerError, "credential_save_failed", "credential could not be saved")
			}
			return
		}
		a.authMu.Lock()
		delete(a.oauthSessions, sessionID)
		a.authMu.Unlock()
		modelSync := a.syncOAuthModelTargets(r.Context(), []authModelSyncTarget{{ID: id, Name: createdName}})
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": createdName, "platform": credential.Platform, "message": "authorization stored encrypted", "model_sync": modelSync.Items[0]})
		return
	}
	code, callbackState, err := parseOAuthCallback(in.Callback)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_callback", err.Error())
		return
	}
	if callbackState != "" && callbackState != session.State {
		fail(w, http.StatusBadRequest, "oauth_state_mismatch", "authorization state does not match")
		return
	}
	// Consume browser callback sessions before exchanging a code; authorization codes are single-use.
	a.authMu.Lock()
	delete(a.oauthSessions, sessionID)
	a.authMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	credential, err := a.exchangeOAuthCode(ctx, session, code)
	if err != nil {
		detail := publicOAuthExchangeError(err)
		a.log.Warn("OAuth code exchange failed", "platform", session.Platform, "error", detail)
		fail(w, http.StatusBadGateway, "oauth_exchange_failed", "authorization exchange failed: "+detail+"; start a new login and try again")
		return
	}
	id, createdName, err := a.saveOAuthProvider(r.Context(), strings.TrimSpace(in.Name), priority, credential, 0, false)
	if err != nil {
		if errors.Is(err, errDuplicateCredential) {
			fail(w, http.StatusConflict, "credential_exists", "this authorized account already exists")
		} else {
			a.log.Error("OAuth credential save failed", "error", err)
			fail(w, http.StatusInternalServerError, "credential_save_failed", "credential could not be saved")
		}
		return
	}
	modelSync := a.syncOAuthModelTargets(r.Context(), []authModelSyncTarget{{ID: id, Name: createdName}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": createdName, "platform": credential.Platform, "message": "authorization stored encrypted", "model_sync": modelSync.Items[0]})
}

func (a *App) pollXAIDeviceAuthorization(ctx context.Context, session oauthSession) (ProviderCredential, bool, int, error) {
	if !isTrustedXAIEndpoint(session.TokenEndpoint) || strings.TrimSpace(session.DeviceCode) == "" {
		return ProviderCredential{}, false, 0, errors.New("xAI authorization session is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {xaiOAuthClientID}, "device_code": {session.DeviceCode}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return ProviderCredential{}, false, 0, errors.New("xAI authorization request is invalid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return ProviderCredential{}, false, 0, errors.New("xAI authorization service is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProviderCredential{}, false, 0, errors.New("cannot read xAI authorization response")
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		switch failure.Error {
		case "authorization_pending":
			return ProviderCredential{}, true, max(2, int(session.PollInterval.Seconds())), nil
		case "slow_down":
			return ProviderCredential{}, true, max(5, int(session.PollInterval.Seconds())+5), nil
		case "expired_token", "access_denied":
			return ProviderCredential{}, false, 0, errors.New("xAI device authorization was not completed")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderCredential{}, false, 0, fmt.Errorf("xAI authorization service returned status %d", resp.StatusCode)
	}
	credential, err := credentialFromOAuthTokenBody(body, "grok", "fusiongate_oauth")
	if err != nil {
		return ProviderCredential{}, false, 0, err
	}
	credential.Extra = map[string]any{"token_endpoint": session.TokenEndpoint}
	return credential, false, 0, nil
}

func (a *App) exchangeOAuthCode(ctx context.Context, session oauthSession, code string) (ProviderCredential, error) {
	if session.Platform == "codex" {
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {codexOAuthClientID}, "code": {code}, "redirect_uri": {codexOAuthRedirectURI}, "code_verifier": {session.Verifier}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		return a.readOAuthTokenResponse(req, "codex", "fusiongate_oauth")
	}
	payload, _ := json.Marshal(map[string]any{"code": code, "state": session.State, "grant_type": "authorization_code", "client_id": claudeOAuthClientID, "redirect_uri": claudeOAuthRedirectURI, "code_verifier": session.Verifier})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return a.readOAuthTokenResponse(req, "claude", "fusiongate_oauth")
}

func oauthTokenErrorMessage(status int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		parts := make([]string, 0, 3)
		if v := strings.TrimSpace(asString(payload["error"])); v != "" {
			parts = append(parts, v)
		}
		if v := strings.TrimSpace(asString(payload["error_description"])); v != "" {
			parts = append(parts, v)
		}
		if v := strings.TrimSpace(asString(payload["message"])); v != "" && (len(parts) == 0 || !strings.Contains(strings.Join(parts, " "), v)) {
			parts = append(parts, v)
		}
		if len(parts) > 0 {
			msg = strings.Join(parts, ": ")
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return msg
}

func (a *App) readOAuthTokenResponse(req *http.Request, platform, source string) (ProviderCredential, error) {
	return a.readOAuthTokenResponseViaNode(req, platform, source, nil)
}

func (a *App) readOAuthTokenResponseViaNode(req *http.Request, platform, source string, nodeID *int64) (ProviderCredential, error) {
	resp, err := a.doProviderRequest(req, nodeID)
	if err != nil {
		return ProviderCredential{}, errors.New("authentication service is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProviderCredential{}, errors.New("cannot read authentication response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderCredential{}, fmt.Errorf("authentication service returned status %d: %s", resp.StatusCode, oauthTokenErrorMessage(resp.StatusCode, body))
	}
	return credentialFromOAuthTokenBody(body, platform, source)
}

func credentialFromOAuthTokenBody(body []byte, platform, source string) (ProviderCredential, error) {
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Account      struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
	}
	if json.Unmarshal(body, &token) != nil || strings.TrimSpace(token.AccessToken) == "" {
		return ProviderCredential{}, errors.New("authentication response is invalid")
	}
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: platform, Source: source, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, Scope: token.Scope, AccountID: token.Account.UUID, Email: token.Account.EmailAddress, LastRefresh: now()}
	if token.ExpiresIn > 0 {
		credential.ExpiresAt = time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	enrichCredentialFromJWT(&credential)
	return credential, nil
}

func (a *App) authImportPreview(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	// The credential JSON is itself wrapped in a JSON request string, so quotes
	// and control characters may be escaped on the wire. Enforce the real limit
	// again after decoding while allowing bounded encoding overhead here.
	r.Body = http.MaxBytesReader(w, r.Body, authImportMaxBytes*3+64<<10)
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid or oversized credential import request")
		return
	}
	if len(in.Content) > authImportMaxBytes {
		fail(w, http.StatusRequestEntityTooLarge, "credential_file_too_large", "credential JSON must not exceed 8 MiB")
		return
	}
	items, err := a.parseCredentialImports(in.Content)
	if err != nil {
		fail(w, http.StatusBadRequest, "credential_parse_failed", err.Error())
		return
	}
	sessionID := base64.RawURLEncoding.EncodeToString(randomBytes(24))
	a.authMu.Lock()
	a.pruneAuthMemoryLocked(time.Now())
	a.authImports[sessionID] = credentialImportSession{Created: time.Now(), Items: items}
	a.authMu.Unlock()
	preview := make([]credentialImportPreview, 0, len(items))
	for _, item := range items {
		c := item.Credential
		preview = append(preview, credentialImportPreview{ID: item.ID, Name: maskCredentialPreviewName(item.Name, c), Platform: c.Platform, Source: c.Source, Email: maskEmail(c.Email), AccountID: maskIdentity(c.AccountID), ExpiresAt: c.ExpiresAt, HasRefreshToken: c.RefreshToken != "", Status: item.Status, Duplicate: item.DuplicateID > 0, DuplicateID: item.DuplicateID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sessionID, "expires_in": int(authSessionTTL.Seconds()), "items": preview})
}

func oauthExportType(platform string) string {
	if platform == "grok" {
		return "xai"
	}
	return platform
}

func oauthExportBaseURL(platform string) string {
	base := oauthProviderBaseURL(platform)
	if platform == "grok" {
		return strings.TrimRight(base, "/") + "/v1"
	}
	return base
}

func exportedCredentialEntry(name string, priority int, enabled bool, source string, c ProviderCredential) credentialExportEntry {
	platform := normalizeOAuthPlatform(c.Platform)
	entry := credentialExportEntry{
		Name: name, Type: oauthExportType(platform), Platform: platform, AuthKind: "oauth",
		AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, IDToken: c.IDToken, AccountID: c.AccountID,
		Email: c.Email, Expired: c.ExpiresAt, LastRefresh: c.LastRefresh, Scope: c.Scope,
		BaseURL: oauthExportBaseURL(platform), Priority: priority, Enabled: enabled, Source: firstNonEmpty(c.Source, source, "fusiongate"),
	}
	if platform == "codex" {
		entry.ChatGPTAccountID = c.AccountID
	} else {
		entry.Subject = c.AccountID
	}
	if platform == "grok" && c.Extra != nil {
		if endpoint, ok := c.Extra["token_endpoint"].(string); ok && isTrustedXAIEndpoint(endpoint) {
			entry.TokenEndpoint = endpoint
		}
	}
	return entry
}

func (a *App) authExport(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		ProviderIDs []int64 `json:"provider_ids"`
		Acknowledge bool    `json:"acknowledge_sensitive_export"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !in.Acknowledge {
		fail(w, http.StatusBadRequest, "export_confirmation_required", "sensitive credential export must be explicitly confirmed")
		return
	}
	ids := make([]int64, 0, len(in.ProviderIDs))
	seen := make(map[int64]bool, len(in.ProviderIDs))
	for _, id := range in.ProviderIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > authExportMaxItems {
		fail(w, http.StatusBadRequest, "invalid_export_selection", "select between 1 and 200 authentication files")
		return
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,name,credential,enabled,priority,auth_source FROM providers WHERE auth_kind='oauth' AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be loaded")
		return
	}
	defer rows.Close()
	type storedCredential struct {
		Name       string
		Credential []byte
		Enabled    int
		Priority   int
		Source     string
	}
	stored := make(map[int64]storedCredential, len(ids))
	for rows.Next() {
		var id int64
		var item storedCredential
		if err := rows.Scan(&id, &item.Name, &item.Credential, &item.Enabled, &item.Priority, &item.Source); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be read")
			return
		}
		stored[id] = item
	}
	if err := rows.Err(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be read")
		return
	}
	if len(stored) != len(ids) {
		fail(w, http.StatusBadRequest, "invalid_export_selection", "only existing OAuth authentication files can be exported")
		return
	}
	bundle := credentialExportBundle{Version: 1, Format: "fusiongate_auth_export", ExportedAt: now(), Credentials: make([]credentialExportEntry, 0, len(ids))}
	for _, id := range ids {
		item := stored[id]
		plain, err := a.decrypt(item.Credential)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_decrypt_failed", "authentication file could not be decrypted")
			return
		}
		var credential ProviderCredential
		if err := json.Unmarshal([]byte(plain), &credential); err != nil || normalizeOAuthPlatform(credential.Platform) == "" || credential.AccessToken == "" {
			fail(w, http.StatusInternalServerError, "credential_invalid", "stored authentication file is invalid")
			return
		}
		bundle.Credentials = append(bundle.Credentials, exportedCredentialEntry(item.Name, item.Priority, item.Enabled != 0, item.Source, credential))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fusiongate-auth-export-`+time.Now().UTC().Format("20060102-150405")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := json.NewEncoder(w).Encode(bundle); err != nil {
		a.log.Error("credential export response failed", "error", err)
	}
}

func (a *App) authImportCommit(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		SessionID      string `json:"session_id"`
		Selected       []int  `json:"selected"`
		Priority       *int   `json:"priority"`
		UpdateExisting bool   `json:"update_existing"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	priority := 1
	if in.Priority != nil {
		priority = *in.Priority
	}
	if priority < 0 || len(in.Selected) == 0 {
		fail(w, http.StatusBadRequest, "invalid_request", "select at least one account and use a non-negative priority")
		return
	}
	selected := map[int]bool{}
	for _, id := range in.Selected {
		selected[id] = true
	}
	sessionID := strings.TrimSpace(in.SessionID)
	a.authMu.Lock()
	a.pruneAuthMemoryLocked(time.Now())
	session, ok := a.authImports[sessionID]
	if ok {
		validIDs := make(map[int]bool, len(session.Items))
		for _, item := range session.Items {
			validIDs[item.ID] = true
		}
		for id := range selected {
			if !validIDs[id] {
				ok = false
				break
			}
		}
	}
	if ok {
		delete(a.authImports, sessionID)
	}
	a.authMu.Unlock()
	if !ok {
		fail(w, http.StatusBadRequest, "invalid_import_selection", "import preview expired or selection is invalid; parse the JSON again")
		return
	}
	if time.Since(session.Created) > authSessionTTL {
		fail(w, http.StatusBadRequest, "import_session_expired", "import preview expired; parse the JSON again")
		return
	}
	created, updated, skipped := 0, 0, 0
	providers := []map[string]any{}
	syncTargets := []authModelSyncTarget{}
	for _, item := range session.Items {
		if !selected[item.ID] {
			continue
		}
		id, name, err := a.saveOAuthProvider(r.Context(), item.Name, priority, item.Credential, item.DuplicateID, in.UpdateExisting)
		if errors.Is(err, errDuplicateCredential) {
			skipped++
			continue
		}
		if err != nil {
			a.log.Error("credential import save failed", "error", err)
			fail(w, http.StatusInternalServerError, "credential_save_failed", "credential could not be saved")
			return
		}
		if item.DuplicateID > 0 {
			updated++
		} else {
			created++
		}
		providers = append(providers, map[string]any{"id": id, "name": name, "platform": item.Credential.Platform})
		if item.DuplicateID == 0 || a.oauthProviderNeedsModels(r.Context(), id) {
			syncTargets = append(syncTargets, authModelSyncTarget{ID: id, Name: name})
		}
	}
	modelSync := a.syncOAuthModelTargets(r.Context(), syncTargets)
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "updated": updated, "skipped": skipped, "providers": providers, "model_sync": modelSync})
}

func (a *App) oauthProviderNeedsModels(ctx context.Context, providerID int64) bool {
	var count int
	return a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, providerID).Scan(&count) == nil && count == 0
}

func (a *App) syncOAuthModelTargets(ctx context.Context, targets []authModelSyncTarget) authModelSyncSummary {
	summary := authModelSyncSummary{Providers: len(targets), Items: make([]authModelSyncItem, len(targets))}
	if len(targets) == 0 {
		return summary
	}
	type job struct {
		index  int
		target authModelSyncTarget
	}
	jobs := make(chan job)
	workers := min(4, len(targets))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for work := range jobs {
				discovery, imported, err := a.discoverAndImportAllModels(ctx, work.target.ID)
				item := authModelSyncItem{ID: work.target.ID, Name: work.target.Name}
				if err != nil {
					item.Status = "error"
					item.Error = "模型自动识别失败，可稍后重试"
					a.log.Warn("OAuth model auto-discovery failed", "provider_id", work.target.ID, "error", err)
				} else {
					item.Status = "ok"
					item.Discovered = discovery.Discovered
					item.Added = imported.Added
					item.Existing = imported.Existing
					item.Skipped = discovery.Skipped
				}
				summary.Items[work.index] = item
			}
		}()
	}
	for index, target := range targets {
		jobs <- job{index: index, target: target}
	}
	close(jobs)
	wg.Wait()
	for _, item := range summary.Items {
		if item.Status == "ok" {
			summary.Succeeded++
			summary.Models += item.Discovered
			summary.Added += item.Added
			summary.Existing += item.Existing
		} else {
			summary.Failed++
		}
	}
	return summary
}

func (a *App) authModelSync(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		ProviderIDs []int64 `json:"provider_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	requested := make(map[int64]bool, len(in.ProviderIDs))
	for _, id := range in.ProviderIDs {
		if id > 0 {
			requested[id] = true
		}
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id,p.name FROM providers p
		WHERE p.auth_kind='oauth'
		  AND NOT EXISTS (SELECT 1 FROM model_routes r WHERE r.provider_id=p.id)
		  AND NOT (
			lower(COALESCE(p.auth_source,'')) IN ('cliproxy','cli-proxy','cli_proxy','cpa','sub2api')
			AND p.auth_expires_at IS NOT NULL
			AND p.auth_expires_at!=''
			AND p.auth_expires_at<=?
		  )
		ORDER BY p.id LIMIT 200`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be loaded")
		return
	}
	targets := []authModelSyncTarget{}
	for rows.Next() {
		var target authModelSyncTarget
		if err := rows.Scan(&target.ID, &target.Name); err != nil {
			_ = rows.Close()
			fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be read")
			return
		}
		if len(requested) == 0 || requested[target.ID] {
			targets = append(targets, target)
		}
	}
	if err := rows.Close(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "authentication files could not be read")
		return
	}
	writeJSON(w, http.StatusOK, a.syncOAuthModelTargets(r.Context(), targets))
}

var (
	errDuplicateCredential   = errors.New("credential already exists")
	errUnsupportedCredential = errors.New("unsupported credential entry")
)

func (a *App) saveOAuthProvider(ctx context.Context, requestedName string, priority int, c ProviderCredential, duplicateID int64, updateExisting bool) (int64, string, error) {
	if c.AccessToken == "" || normalizeOAuthPlatform(c.Platform) == "" {
		return 0, "", errors.New("invalid OAuth credential")
	}
	c.Platform = normalizeOAuthPlatform(c.Platform)
	c.Version, c.Kind = 1, "oauth"
	enrichCredentialFromJWT(&c)
	fingerprint := credentialFingerprint(c)
	if duplicateID == 0 {
		_ = a.db.QueryRowContext(ctx, `SELECT id FROM providers WHERE auth_fingerprint=?`, fingerprint).Scan(&duplicateID)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return 0, "", err
	}
	encrypted, err := a.encrypt(string(payload))
	if err != nil {
		return 0, "", err
	}
	status := credentialStatus(c)
	sharedRisk := sharedGrokImport(c)
	sharedNote := ""
	if sharedRisk {
		sharedNote = "shared_import_risk: 导入的 Grok refresh token 可能仍被其他运行实例使用，已默认停用且禁止 FusionGate 续期；请用设备授权获取独立凭证"
	}
	if duplicateID > 0 {
		if !updateExisting {
			return duplicateID, requestedName, errDuplicateCredential
		}
		var currentName string
		err = a.db.QueryRowContext(ctx, `SELECT name FROM providers WHERE id=?`, duplicateID).Scan(&currentName)
		if err != nil {
			return 0, "", err
		}
		// Shared Grok imports stay disabled so proactive refresh never revokes the source side.
		enabledVal := map[bool]int{true: 1, false: 0}[status != "expired" && !sharedRisk]
		_, err = a.db.ExecContext(ctx, `UPDATE providers SET type=?,base_url=?,credential=?,auth_kind='oauth',auth_source=?,auth_account_id=?,auth_email=?,auth_expires_at=?,auth_last_refresh_at=?,auth_status=?,auth_fingerprint=?,auth_has_refresh=?,status=?,enabled=?,notes=CASE WHEN ?!='' THEN ? ELSE notes END,last_error='',health_check_status='',health_check_error='',updated_at=? WHERE id=?`, oauthProviderType(c.Platform), oauthProviderBaseURL(c.Platform), encrypted, c.Source, c.AccountID, strings.ToLower(c.Email), nullableString(c.ExpiresAt), nullableString(c.LastRefresh), status, fingerprint, boolInt(c.RefreshToken != ""), map[bool]string{true: "unknown", false: "auth_expired"}[status != "expired"], enabledVal, sharedNote, sharedNote, now(), duplicateID)
		return duplicateID, currentName, err
	}
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name = suggestedCredentialName(c, 1)
	}
	name = a.uniqueProviderName(ctx, name)
	enabled := status != "expired" && !sharedRisk
	res, err := a.db.ExecContext(ctx, `INSERT INTO providers(name,type,base_url,credential,enabled,archived,priority,sort_order,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,auth_kind,auth_source,auth_account_id,auth_email,auth_expires_at,auth_last_refresh_at,auth_status,auth_fingerprint,auth_has_refresh,created_at,updated_at) SELECT ?,?,?,?,?,0,?,COALESCE(MAX(sort_order),-1)+1,100,'unknown',?,'normalized','any',0,120000,3,30,'oauth',?,?,?,?,?,?,?,?,?,? FROM providers`, name, oauthProviderType(c.Platform), oauthProviderBaseURL(c.Platform), encrypted, boolInt(enabled), priority, sharedNote, c.Source, c.AccountID, strings.ToLower(c.Email), nullableString(c.ExpiresAt), nullableString(c.LastRefresh), status, fingerprint, boolInt(c.RefreshToken != ""), now(), now())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	return id, name, nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func (a *App) uniqueProviderName(ctx context.Context, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "授权渠道"
	}
	for i := 1; ; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d)", base, i)
		}
		var exists int
		if err := a.db.QueryRowContext(ctx, `SELECT 1 FROM providers WHERE name=?`, candidate).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return candidate
		}
	}
}

func (a *App) parseCredentialImports(content string) ([]credentialImport, error) {
	values, err := decodeCredentialJSON(content)
	if err != nil {
		return nil, err
	}
	items := make([]credentialImport, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("each imported credential must be a JSON object")
		}
		credential, name, err := normalizeImportedCredential(obj)
		if errors.Is(err, errUnsupportedCredential) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(items)+1, err)
		}
		fingerprint := credentialFingerprint(credential)
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		var duplicateID int64
		_ = a.db.QueryRow(`SELECT id FROM providers WHERE auth_fingerprint=?`, fingerprint).Scan(&duplicateID)
		status := credentialStatus(credential)
		items = append(items, credentialImport{ID: len(items) + 1, Name: firstNonEmpty(name, suggestedCredentialName(credential, len(items)+1)), Credential: credential, Fingerprint: fingerprint, DuplicateID: duplicateID, Status: status})
	}
	if len(items) == 0 {
		return nil, errors.New("no supported Codex, Claude, or Grok credentials found")
	}
	return items, nil
}

func decodeCredentialJSON(content string) ([]any, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("credential JSON is empty")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	var decoded []any
	for {
		var value any
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("invalid JSON credential content")
		}
		decoded = append(decoded, value)
	}
	var out []any
	var flatten func(any)
	flatten = func(value any) {
		if arr, ok := value.([]any); ok {
			for _, item := range arr {
				flatten(item)
			}
			return
		}
		if obj, ok := value.(map[string]any); ok {
			for _, key := range []string{"accounts", "items"} {
				if collection, exists := obj[key].([]any); exists {
					flatten(collection)
					return
				}
			}
			if nested, exists := obj["data"]; exists {
				switch data := nested.(type) {
				case []any:
					flatten(data)
					return
				case map[string]any:
					if _, hasAccounts := data["accounts"]; hasAccounts {
						flatten(data)
						return
					}
					if _, hasItems := data["items"]; hasItems {
						flatten(data)
						return
					}
				}
			}
		}
		out = append(out, value)
	}
	for _, value := range decoded {
		flatten(value)
	}
	return out, nil
}

func normalizeImportedCredential(raw map[string]any) (ProviderCredential, string, error) {
	nestedMaps := make([]map[string]any, 0, 5)
	for _, key := range []string{"credentials", "tokens", "token_data", "tokenData", "credential"} {
		if nested, ok := raw[key].(map[string]any); ok {
			nestedMaps = append(nestedMaps, nested)
		}
	}
	allMaps := append([]map[string]any{raw}, nestedMaps...)
	tokenMaps := append(append([]map[string]any{}, nestedMaps...), raw)
	platform := normalizeOAuthPlatform(firstStringMaps(allMaps, []string{"platform"}, []string{"provider"}))
	explicitType := strings.ToLower(firstStringMaps(allMaps, []string{"type"}, []string{"auth_type"}, []string{"authType"}, []string{"auth_mode"}, []string{"authMode"}))
	if platform == "" {
		platform = normalizeOAuthPlatform(explicitType)
	}
	_, sub2apiShape := raw["credentials"]
	if sub2apiShape && explicitType != "" && explicitType != "oauth" && explicitType != "codex" && explicitType != "claude" && explicitType != "grok" && explicitType != "xai" && explicitType != "openai_oauth" && explicitType != "claude_oauth" && explicitType != "grok_oauth" && explicitType != "xai_oauth" {
		return ProviderCredential{}, "", errUnsupportedCredential
	}
	access := firstStringMaps(tokenMaps, []string{"access_token"}, []string{"accessToken"})
	if access == "" && explicitType != "apikey" && explicitType != "api_key" {
		access = firstStringMaps(tokenMaps, []string{"token"})
	}
	refresh := firstStringMaps(tokenMaps, []string{"refresh_token"}, []string{"refreshToken"})
	idToken := firstStringMaps(tokenMaps, []string{"id_token"}, []string{"idToken"})
	if platform == "" {
		if firstStringMaps(allMaps, []string{"chatgpt_account_id"}, []string{"chatgptAccountId"}, []string{"account_id"}, []string{"accountId"}, []string{"account", "id"}) != "" {
			platform = "codex"
		}
	}
	if platform == "" && strings.Contains(strings.ToLower(firstStringMaps(allMaps, []string{"base_url"}, []string{"baseURL"})), "grok.com") {
		platform = "grok"
	}
	if platform == "" && idToken != "" {
		platform = "codex"
	}
	if platform == "" {
		return ProviderCredential{}, "", errUnsupportedCredential
	}
	if access == "" {
		return ProviderCredential{}, "", errors.New("supported OAuth credential is missing an access token")
	}
	source := "json"
	if sub2apiShape {
		source = "sub2api"
	}
	if explicitType == "codex" || explicitType == "claude" || explicitType == "grok" || explicitType == "xai" || raw["last_refresh"] != nil || raw["expired"] != nil {
		source = "cliproxy"
	}
	c := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: platform, Source: source, AccessToken: access, RefreshToken: refresh, IDToken: idToken,
		AccountID: firstStringMaps(allMaps,
			[]string{"chatgpt_account_id"}, []string{"chatgptAccountId"}, []string{"account_id"}, []string{"accountId"}, []string{"sub"}, []string{"subject"},
			[]string{"account", "id"}, []string{"account", "account_id"}, []string{"account", "chatgpt_account_id"}),
		Email: firstStringMaps(allMaps, []string{"email"}, []string{"email_address"}, []string{"user", "email"}, []string{"account", "email"}),
		ExpiresAt: firstTimeMaps(tokenMaps,
			[]string{"expired"}, []string{"expires_at"}, []string{"expiresAt"}, []string{"expiry"}),
		LastRefresh: firstTimeMaps(tokenMaps,
			[]string{"last_refresh"}, []string{"lastRefresh"}, []string{"last_refresh_at"}),
		Scope: firstStringMaps(tokenMaps, []string{"scope"}),
	}
	extra := map[string]any{}
	if endpoint := firstStringMaps(tokenMaps, []string{"token_endpoint"}, []string{"tokenEndpoint"}); endpoint != "" && isTrustedXAIEndpoint(endpoint) {
		extra["token_endpoint"] = endpoint
	}
	if headers, ok := raw["headers"].(map[string]any); ok && len(headers) > 0 {
		// Preserve imported Grok client headers so upstream metadata stays consistent.
		copied := map[string]any{}
		for k, v := range headers {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				copied[k] = strings.TrimSpace(s)
			}
		}
		if len(copied) > 0 {
			extra["headers"] = copied
		}
	}
	if expiresIn := firstStringMaps(tokenMaps, []string{"expires_in"}, []string{"expiresIn"}); expiresIn != "" {
		extra["expires_in"] = expiresIn
	}
	if len(extra) > 0 {
		c.Extra = extra
	}
	enrichCredentialFromJWT(&c)
	return c, firstStringMaps([]map[string]any{raw}, []string{"name"}, []string{"user", "name"}), nil
}

func firstStringMaps(maps []map[string]any, paths ...[]string) string {
	for _, obj := range maps {
		for _, path := range paths {
			if value, ok := mapPath(obj, path); ok {
				switch v := value.(type) {
				case string:
					if strings.TrimSpace(v) != "" {
						return strings.TrimSpace(v)
					}
				case json.Number:
					return v.String()
				}
			}
		}
	}
	return ""
}

func firstTimeMaps(maps []map[string]any, paths ...[]string) string {
	for _, obj := range maps {
		for _, path := range paths {
			if value, ok := mapPath(obj, path); ok {
				if stamp := normalizeTimeValue(value); stamp != "" {
					return stamp
				}
			}
		}
	}
	return ""
}

func mapPath(obj map[string]any, path []string) (any, bool) {
	var current any = obj
	for _, part := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = mapped[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func normalizeTimeValue(value any) string {
	var raw string
	switch v := value.(type) {
	case string:
		raw = strings.TrimSpace(v)
	case json.Number:
		raw = v.String()
	case float64:
		raw = strconv.FormatInt(int64(v), 10)
	}
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 10_000_000_000 {
			n /= 1000
		}
		if n > 0 {
			return time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func enrichCredentialFromJWT(c *ProviderCredential) {
	for _, token := range []string{c.IDToken, c.AccessToken} {
		claims := jwtClaims(token)
		if claims == nil {
			continue
		}
		if c.Email == "" {
			c.Email = firstStringMaps([]map[string]any{claims}, []string{"email"})
		}
		if c.AccountID == "" {
			c.AccountID = firstStringMaps([]map[string]any{claims}, []string{"chatgpt_account_id"}, []string{"account_id"}, []string{"sub"}, []string{"subject"}, []string{"https://api.openai.com/auth", "chatgpt_account_id"}, []string{"https://api.openai.com/auth", "account_id"})
		}
		if c.ExpiresAt == "" {
			if exp, ok := claims["exp"]; ok {
				c.ExpiresAt = normalizeTimeValue(exp)
			}
		}
	}
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
}

func credentialFingerprint(c ProviderCredential) string {
	identity := strings.TrimSpace(c.AccountID)
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(c.Email))
	}
	if identity == "" {
		token := c.RefreshToken
		if token == "" {
			token = c.AccessToken
		}
		sum := sha256.Sum256([]byte(token))
		identity = hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(c.Platform + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

func credentialStatus(c ProviderCredential) string {
	if c.ExpiresAt == "" {
		return "ready"
	}
	expires := parseTime(c.ExpiresAt)
	if expires == nil || expires.After(time.Now()) {
		return "ready"
	}
	if c.RefreshToken != "" {
		return "expired_refreshable"
	}
	return "expired"
}

func suggestedCredentialName(c ProviderCredential, index int) string {
	label := "Codex"
	switch c.Platform {
	case "claude":
		label = "Claude"
	case "grok":
		label = "Grok"
	}
	identity := maskEmail(c.Email)
	if identity == "" {
		identity = maskIdentity(c.AccountID)
	}
	if identity == "" {
		identity = fmt.Sprintf("授权 %d", index)
	}
	return label + " · " + identity
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func maskCredentialPreviewName(name string, c ProviderCredential) string {
	masked := strings.TrimSpace(name)
	for raw, replacement := range map[string]string{c.Email: maskEmail(c.Email), c.AccountID: maskIdentity(c.AccountID)} {
		if strings.TrimSpace(raw) != "" {
			masked = strings.ReplaceAll(masked, raw, replacement)
		}
	}
	return masked
}

func maskEmail(value string) string {
	value = strings.TrimSpace(value)
	parts := strings.SplitN(value, "@", 2)
	if len(parts) != 2 {
		return maskIdentity(value)
	}
	local := parts[0]
	if len(local) > 2 {
		local = local[:2] + "•••"
	} else if local != "" {
		local = local[:1] + "•••"
	}
	return local + "@" + parts[1]
}

func maskIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 2 {
		return "••"
	}
	if len(value) <= 8 {
		return value[:1] + "••••" + value[len(value)-1:]
	}
	return value[:4] + "••••" + value[len(value)-4:]
}

func decodeStoredCredential(kind string, plaintext string) (ProviderCredential, string, error) {
	if kind == "" || kind == "api_key" {
		return ProviderCredential{}, plaintext, nil
	}
	var credential ProviderCredential
	if err := json.Unmarshal([]byte(plaintext), &credential); err != nil {
		return credential, "", errors.New("invalid encrypted OAuth credential")
	}
	if credential.AccessToken == "" {
		return credential, "", errors.New("OAuth access token is missing")
	}
	return credential, credential.AccessToken, nil
}

func (a *App) ensureFreshProviderCredential(ctx context.Context, z *resolvedRoute) error {
	return a.refreshProviderCredential(ctx, z, false)
}

// refreshProviderCredential refreshes an OAuth credential when it is near expiry,
// or unconditionally when force is true after an upstream authentication rejection.
func (a *App) refreshProviderCredential(ctx context.Context, z *resolvedRoute, force bool) error {
	if z == nil || z.AuthCredential == nil {
		return nil
	}
	credential := *z.AuthCredential
	expires := parseTime(credential.ExpiresAt)
	if !force && (expires == nil || expires.After(time.Now().Add(oauthRefreshLeadTime()))) {
		return nil
	}
	if credential.RefreshToken == "" {
		return errors.New("OAuth credential expired and has no refresh token")
	}
	if externalOAuthOwner(credential) {
		// The source process owns refresh-token rotation for externally imported
		// credentials. FusionGate may keep using a still-valid access token,
		// but must never hit the token endpoint or it can revoke the source side.
		// Native FusionGate OAuth (source=fusiongate_oauth) is intentionally
		// NOT covered here and continues to refresh itself.
		if !force {
			if expires != nil && expires.After(time.Now()) {
				return nil
			}
		}
		owner := strings.TrimSpace(credential.Source)
		if owner == "" {
			owner = "external"
		}
		detail := fmt.Sprintf("externally managed OAuth credential (%s): access token expired; update the imported credential at its source (FusionGate will not rotate imported refresh tokens)", owner)
		_, _ = a.db.ExecContext(context.Background(), `UPDATE providers SET auth_status='expired',status='auth_expired',last_error=?,updated_at=? WHERE id=?`, detail, now(), z.Provider.ID)
		return errors.New(detail)
	}
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	var encrypted []byte
	var kind string
	if err := a.db.QueryRowContext(ctx, `SELECT credential,auth_kind FROM providers WHERE id=?`, z.Provider.ID).Scan(&encrypted, &kind); err != nil {
		return err
	}
	plaintext, err := a.decrypt(encrypted)
	if err != nil {
		return err
	}
	current, token, err := decodeStoredCredential(kind, plaintext)
	if err != nil {
		return err
	}
	if !force {
		if currentExpires := parseTime(current.ExpiresAt); currentExpires == nil || currentExpires.After(time.Now().Add(oauthRefreshLeadTime())) {
			z.AuthCredential, z.Credential = &current, token
			return nil
		}
	}
	refreshed, err := a.refreshOAuthCredentialViaNode(ctx, current, z.Provider.IPPoolNodeID)
	if err != nil {
		detail := strings.TrimSpace(err.Error())
		if detail == "" {
			detail = "OAuth refresh failed"
		}
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		_, _ = a.db.ExecContext(context.Background(), `UPDATE providers SET auth_status='refresh_failed',status='auth_expired',last_error=?,updated_at=? WHERE id=?`, detail, now(), z.Provider.ID)
		a.log.Warn("oauth refresh failed", "provider_id", z.Provider.ID, "platform", current.Platform, "error", detail)
		return fmt.Errorf("OAuth token refresh failed: %s", detail)
	}
	payload, _ := json.Marshal(refreshed)
	sealed, err := a.encrypt(string(payload))
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE providers SET credential=?,auth_account_id=?,auth_email=?,auth_expires_at=?,auth_last_refresh_at=?,auth_status='ready',auth_has_refresh=?,status='unknown',last_error='',updated_at=? WHERE id=?`, sealed, refreshed.AccountID, refreshed.Email, nullableString(refreshed.ExpiresAt), nullableString(refreshed.LastRefresh), boolInt(refreshed.RefreshToken != ""), now(), z.Provider.ID)
	if err != nil {
		return err
	}
	z.AuthCredential, z.Credential = &refreshed, refreshed.AccessToken
	return nil
}

func (a *App) refreshOAuthCredential(ctx context.Context, current ProviderCredential) (ProviderCredential, error) {
	return a.refreshOAuthCredentialViaNode(ctx, current, nil)
}

func (a *App) refreshOAuthCredentialViaNode(ctx context.Context, current ProviderCredential, nodeID *int64) (ProviderCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var req *http.Request
	switch current.Platform {
	case "codex":
		form := url.Values{"client_id": {codexOAuthClientID}, "grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}, "scope": {"openid profile email"}}
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case "grok":
		tokenEndpoint, _ := current.Extra["token_endpoint"].(string)
		if !isTrustedXAIEndpoint(tokenEndpoint) {
			config, err := a.xaiOIDCConfigurationViaNode(ctx, nodeID)
			if err != nil {
				return current, err
			}
			tokenEndpoint = config.TokenEndpoint
		}
		form := url.Values{"client_id": {xaiOAuthClientID}, "grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}}
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if current.Extra == nil {
			current.Extra = map[string]any{}
		}
		current.Extra["token_endpoint"] = tokenEndpoint
	default:
		payload, _ := json.Marshal(map[string]any{"client_id": claudeOAuthClientID, "grant_type": "refresh_token", "refresh_token": current.RefreshToken})
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	fresh, err := a.readOAuthTokenResponseViaNode(req, current.Platform, current.Source, nodeID)
	if err != nil {
		return current, err
	}
	// xAI (and some other providers) rotate refresh tokens on every successful refresh.
	// Keeping a stale refresh token here permanently breaks auto-renewal.
	if strings.TrimSpace(fresh.RefreshToken) == "" {
		fresh.RefreshToken = current.RefreshToken
	}
	if fresh.IDToken == "" {
		fresh.IDToken = current.IDToken
	}
	if fresh.AccountID == "" {
		fresh.AccountID = current.AccountID
	}
	if fresh.Email == "" {
		fresh.Email = current.Email
	}
	// Preserve non-token metadata (token endpoint / client headers) across rotation.
	if current.Extra != nil {
		merged := make(map[string]any, len(current.Extra)+2)
		for k, v := range current.Extra {
			merged[k] = v
		}
		if fresh.Extra != nil {
			for k, v := range fresh.Extra {
				merged[k] = v
			}
		}
		fresh.Extra = merged
	}
	return fresh, nil
}

// externalOAuthOwner reports credentials that another runtime is responsible for
// refreshing. FusionGate must not rotate those
// refresh tokens. Credentials created via FusionGate device/browser OAuth
// (source=fusiongate_oauth) keep using FusionGate's own refresh loop.
func externalOAuthOwner(c ProviderCredential) bool {
	src := strings.ToLower(strings.TrimSpace(c.Source))
	switch src {
	case "cliproxy", "cli-proxy", "cli_proxy", "cpa", "sub2api":
		return true
	default:
		return false
	}
}

// sharedGrokImport is kept as a narrow alias for Grok-specific import UX/notes.
func sharedGrokImport(c ProviderCredential) bool {
	return normalizeOAuthPlatform(c.Platform) == "grok" && externalOAuthOwner(c)
}

func oauthRefreshLeadTime() time.Duration {
	// Refresh a bit early so traffic never hits an already-expired access token.
	return 15 * time.Minute
}

func (a *App) runOAuthRefreshLoop(ctx context.Context) {
	// Stagger startup so health checks and migrations settle first.
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	a.proactiveRefreshOAuthCredentials(ctx)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.proactiveRefreshOAuthCredentials(ctx)
		}
	}
}

func (a *App) proactiveRefreshOAuthCredentials(parent context.Context) {
	if parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()

	// Prefer soon-to-expire *FusionGate-owned* credentials that still have a
	// refresh token. Externally managed imports are excluded.
	// Retry refresh_failed only occasionally, and skip permanent invalid_grant/
	// revoked failures so we do not hammer upstream with dead refresh tokens.
	lead := time.Now().UTC().Add(oauthRefreshLeadTime()).Format(time.RFC3339)
	retryFailedBefore := time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339)
	// Only refresh credentials FusionGate itself owns. Externally managed imports
	// must be updated by their source and must not be rotated here.
	rows, err := a.db.QueryContext(ctx, `
		SELECT id FROM providers
		WHERE auth_kind='oauth' AND enabled=1 AND auth_has_refresh=1
		  AND lower(COALESCE(auth_source,'')) NOT IN ('cliproxy','cli-proxy','cli_proxy','cpa','sub2api')
		  AND (
			(
			  auth_status!='refresh_failed'
			  AND (auth_expires_at IS NULL OR auth_expires_at = '' OR auth_expires_at <= ?)
			)
			OR (
			  auth_status='refresh_failed'
			  AND updated_at <= ?
			  AND lower(COALESCE(last_error,'')) NOT LIKE '%revoked%'
			  AND lower(COALESCE(last_error,'')) NOT LIKE '%invalid_grant%'
			  AND lower(COALESCE(last_error,'')) NOT LIKE '%invalid refresh%'
			)
		  )
		ORDER BY
			CASE WHEN auth_status='refresh_failed' THEN 1 ELSE 0 END ASC,
			COALESCE(auth_expires_at, '1970-01-01') ASC,
			id ASC
		LIMIT 40
	`, lead, retryFailedBefore)
	if err != nil {
		a.log.Warn("oauth proactive refresh query failed", "error", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}

	ok, failed := 0, 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := a.refreshProviderByID(ctx, id, false); err != nil {
			failed++
			// Keep going; individual failures are recorded on the provider row.
			continue
		}
		ok++
	}
	if ok > 0 || failed > 0 {
		a.log.Info("oauth proactive refresh pass", "refreshed", ok, "failed", failed, "selected", len(ids))
	}
}

func (a *App) refreshProviderByID(ctx context.Context, providerID int64, force bool) error {
	p, err := a.loadDiscoveryProvider(ctx, providerID)
	if err != nil {
		return err
	}
	if p.AuthCredential == nil {
		return errors.New("provider has no oauth credential")
	}
	z := &resolvedRoute{
		Provider: Provider{
			ID:           p.ID,
			Name:         p.Name,
			Type:         p.Type,
			IPPoolNodeID: p.IPPoolNodeID,
		},
		AuthCredential: p.AuthCredential,
		Credential:     p.Credential,
	}
	return a.refreshProviderCredential(ctx, z, force)
}

func formatUnixTimestamp(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

func parseCodexUsageWindow(raw map[string]any) *codexUsageWindow {
	if raw == nil {
		return nil
	}
	used := asFloat64(raw["used_percent"])
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	win := &codexUsageWindow{
		UsedPercent:        used,
		RemainingPercent:   100 - used,
		LimitWindowSeconds: asInt64(raw["limit_window_seconds"]),
		ResetAfterSeconds:  asInt64(raw["reset_after_seconds"]),
	}
	switch v := raw["reset_at"].(type) {
	case float64:
		win.ResetAt = formatUnixTimestamp(int64(v))
	case json.Number:
		n, _ := v.Int64()
		win.ResetAt = formatUnixTimestamp(n)
	case string:
		if strings.TrimSpace(v) != "" {
			win.ResetAt = strings.TrimSpace(v)
		}
	}
	if win.ResetAt == "" && win.ResetAfterSeconds > 0 {
		win.ResetAt = time.Now().UTC().Add(time.Duration(win.ResetAfterSeconds) * time.Second).Format(time.RFC3339)
	}
	return win
}

func asFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f
	default:
		return 0
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	default:
		return 0
	}
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return strings.TrimSpace(s)
	case json.Number:
		return s.String()
	default:
		return ""
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func setCodexQuotaRequestHeaders(req *http.Request, accessToken, accountID string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("OAI-Product-Sku", "CODEX")
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	setCodexClientHeaders(req.Header)
	// Reset-credit inventory/consume endpoints are exercised by Codex Desktop clients.
	req.Header.Set("originator", "Codex Desktop")
	req.Header.Set("User-Agent", "Codex Desktop")
}

func (a *App) doCodexQuotaRequestViaNode(ctx context.Context, method, endpoint, accessToken, accountID string, body any, nodeID *int64) (map[string]any, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	setCodexQuotaRequestHeaders(req, accessToken, accountID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.doProviderRequest(req, nodeID)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 240 {
			msg = msg[:240] + "…"
		}
		if msg == "" {
			msg = resp.Status
		}
		return nil, resp.StatusCode, fmt.Errorf("%s returned HTTP %d: %s", endpoint, resp.StatusCode, msg)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, resp.StatusCode, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return payload, resp.StatusCode, nil
}

func (a *App) doCodexQuotaGET(ctx context.Context, endpoint, accessToken, accountID string) (map[string]any, error) {
	return a.doCodexQuotaGETViaNode(ctx, endpoint, accessToken, accountID, nil)
}

func (a *App) doCodexQuotaGETViaNode(ctx context.Context, endpoint, accessToken, accountID string, nodeID *int64) (map[string]any, error) {
	payload, _, err := a.doCodexQuotaRequestViaNode(ctx, http.MethodGet, endpoint, accessToken, accountID, nil, nodeID)
	return payload, err
}

func parseCodexResetCards(payload map[string]any) (int, []codexResetCard) {
	if payload == nil {
		return 0, nil
	}
	var cards []codexResetCard
	addCard := func(item map[string]any) {
		if item == nil {
			return
		}
		card := codexResetCard{
			ID:        firstNonEmpty(asString(item["id"]), asString(item["credit_id"])),
			Status:    firstNonEmpty(asString(item["status"]), "available"),
			ResetType: firstNonEmpty(asString(item["reset_type"]), asString(item["resetType"]), "codex_rate_limits"),
			GrantedAt: firstNonEmpty(asString(item["granted_at"]), asString(item["grantedAt"])),
			ExpiresAt: firstNonEmpty(asString(item["expires_at"]), asString(item["expiresAt"])),
		}
		if card.ID == "" && card.Status == "" && card.ExpiresAt == "" {
			return
		}
		cards = append(cards, card)
	}

	for _, key := range []string{"credits", "items", "rate_limit_reset_credits", "data"} {
		switch typed := payload[key].(type) {
		case []any:
			for _, entry := range typed {
				addCard(asMap(entry))
			}
		case map[string]any:
			if nested := asSlice(typed["credits"]); len(nested) > 0 {
				for _, entry := range nested {
					addCard(asMap(entry))
				}
			} else if nested := asSlice(typed["items"]); len(nested) > 0 {
				for _, entry := range nested {
					addCard(asMap(entry))
				}
			}
		}
	}

	count := int(asInt64(payload["available_count"]))
	if count == 0 {
		count = int(asInt64(payload["availableCount"]))
	}
	if count == 0 && len(cards) > 0 {
		available := 0
		for _, card := range cards {
			status := strings.ToLower(card.Status)
			if status == "" || status == "available" {
				available++
			}
		}
		count = available
	}
	return count, cards
}

func (a *App) fetchCodexAccountQuotaViaNode(ctx context.Context, accessToken, accountID string, nodeID *int64) (*codexAccountQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	usageURL := "https://chatgpt.com/backend-api/wham/usage"
	usage, err := a.doCodexQuotaGETViaNode(ctx, usageURL, accessToken, accountID, nodeID)
	if err != nil {
		return nil, err
	}

	rateLimit := asMap(usage["rate_limit"])
	primary := parseCodexUsageWindow(asMap(rateLimit["primary_window"]))
	secondary := parseCodexUsageWindow(asMap(rateLimit["secondary_window"]))

	quota := &codexAccountQuota{
		PlanType:         firstNonEmpty(asString(usage["plan_type"]), asString(usage["planType"])),
		SubscriptionPlan: firstNonEmpty(asString(usage["plan_type"]), asString(usage["planType"])),
		Allowed:          true,
		Primary:          primary,
		Secondary:        secondary,
	}
	if rateLimit != nil {
		if allowed, ok := rateLimit["allowed"].(bool); ok {
			quota.Allowed = allowed
		}
		if reached, ok := rateLimit["limit_reached"].(bool); ok {
			quota.LimitReached = reached
		} else if reached, ok := rateLimit["limitReached"].(bool); ok {
			quota.LimitReached = reached
		}
	}

	// Prefer dedicated reset-credit inventory; fall back to summary embedded in /wham/usage.
	resetPayload, resetErr := a.doCodexQuotaGETViaNode(ctx, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", accessToken, accountID, nodeID)
	if resetErr == nil {
		quota.ResetCards, quota.ResetCardDetails = parseCodexResetCards(resetPayload)
	} else {
		a.log.Warn("codex reset-credit inventory fetch failed", "error", resetErr)
		if embedded := asMap(usage["rate_limit_reset_credits"]); embedded != nil {
			quota.ResetCards, quota.ResetCardDetails = parseCodexResetCards(embedded)
		} else if embedded := asMap(usage["rateLimitResetCredits"]); embedded != nil {
			quota.ResetCards, quota.ResetCardDetails = parseCodexResetCards(embedded)
		}
	}

	if credits := asMap(usage["credits"]); credits != nil {
		if balance, ok := credits["balance"]; ok {
			value := asFloat64(balance)
			quota.CreditsBalance = &value
		}
		if unlimited, ok := credits["unlimited"].(bool); ok {
			quota.CreditsUnlimited = unlimited
		} else if hasCredits, ok := credits["hasCredits"].(bool); ok && !hasCredits {
			zero := 0.0
			quota.CreditsBalance = &zero
		}
	}

	// Legacy percent-style fields for the existing UI helpers.
	if primary != nil {
		quota.TotalQuota = 100
		quota.UsedQuota = primary.UsedPercent
		quota.RemainingQuota = primary.RemainingPercent
		quota.NextResetDate = primary.ResetAt
	} else if secondary != nil {
		quota.TotalQuota = 100
		quota.UsedQuota = secondary.UsedPercent
		quota.RemainingQuota = secondary.RemainingPercent
		quota.NextResetDate = secondary.ResetAt
	}

	return quota, nil
}

func (a *App) loadCodexOAuthCredential(ctx context.Context, id int64) (ProviderCredential, *int64, error) {
	var providerType, authKind string
	var encrypted []byte
	var nodeID sql.NullInt64
	err := a.db.QueryRowContext(ctx, `SELECT type, auth_kind, credential, ip_pool_node_id FROM providers WHERE id=?`, id).Scan(&providerType, &authKind, &encrypted, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderCredential{}, nil, errProviderNotFound
	}
	if err != nil {
		return ProviderCredential{}, nil, err
	}
	if authKind != "oauth" || providerType != "codex_oauth" {
		return ProviderCredential{}, nil, errUnsupportedQuotaProvider
	}
	plaintext, err := a.decrypt(encrypted)
	if err != nil {
		return ProviderCredential{}, nil, err
	}
	var credential ProviderCredential
	if err := json.Unmarshal([]byte(plaintext), &credential); err != nil {
		return ProviderCredential{}, nil, err
	}
	var selected *int64
	if nodeID.Valid {
		value := nodeID.Int64
		selected = &value
	}
	return credential, selected, nil
}

func (a *App) persistOAuthCredential(ctx context.Context, id int64, updated ProviderCredential) {
	payload, marshalErr := json.Marshal(updated)
	if marshalErr != nil {
		return
	}
	sealed, sealErr := a.encrypt(string(payload))
	if sealErr != nil {
		return
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE providers SET credential=?,auth_account_id=?,auth_email=?,auth_expires_at=?,auth_last_refresh_at=?,auth_status='ready',auth_has_refresh=?,updated_at=? WHERE id=?`, sealed, updated.AccountID, updated.Email, nullableString(updated.ExpiresAt), nullableString(updated.LastRefresh), boolInt(updated.RefreshToken != ""), now(), id)
}

func (a *App) ensureCodexAccessToken(ctx context.Context, id int64, credential ProviderCredential, nodeID *int64) (ProviderCredential, error) {
	if strings.TrimSpace(credential.AccessToken) != "" {
		return credential, nil
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return credential, errors.New("access token not found")
	}
	refreshed, err := a.refreshOAuthCredentialViaNode(ctx, credential, nodeID)
	if err != nil || strings.TrimSpace(refreshed.AccessToken) == "" {
		if err != nil {
			return credential, err
		}
		return credential, errors.New("access token not found")
	}
	a.persistOAuthCredential(ctx, id, refreshed)
	return refreshed, nil
}

func (a *App) withCodexCredential(ctx context.Context, id int64, fn func(ProviderCredential, *int64) (any, error)) (any, error) {
	credential, nodeID, err := a.loadCodexOAuthCredential(ctx, id)
	if err != nil {
		return nil, err
	}
	credential, err = a.ensureCodexAccessToken(ctx, id, credential, nodeID)
	if err != nil {
		return nil, err
	}
	result, err := fn(credential, nodeID)
	if err != nil && strings.TrimSpace(credential.RefreshToken) != "" {
		refreshed, refreshErr := a.refreshOAuthCredentialViaNode(ctx, credential, nodeID)
		if refreshErr == nil && strings.TrimSpace(refreshed.AccessToken) != "" {
			a.persistOAuthCredential(ctx, id, refreshed)
			return fn(refreshed, nodeID)
		}
	}
	return result, err
}

var (
	errProviderNotFound         = errors.New("provider not found")
	errUnsupportedQuotaProvider = errors.New("only Codex OAuth providers support quota operations")
)

func (a *App) redeemCodexResetCard(ctx context.Context, accessToken, accountID, creditID string, nodeID *int64) (map[string]any, error) {
	if strings.TrimSpace(creditID) == "" {
		// Auto-pick the soonest-expiring available card.
		inventory, err := a.doCodexQuotaGETViaNode(ctx, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", accessToken, accountID, nodeID)
		if err != nil {
			return nil, err
		}
		_, cards := parseCodexResetCards(inventory)
		var chosen *codexResetCard
		for i := range cards {
			card := cards[i]
			if strings.ToLower(card.Status) != "" && strings.ToLower(card.Status) != "available" {
				continue
			}
			if card.ID == "" {
				continue
			}
			if chosen == nil {
				chosen = &card
				continue
			}
			if card.ExpiresAt != "" && (chosen.ExpiresAt == "" || card.ExpiresAt < chosen.ExpiresAt) {
				tmp := card
				chosen = &tmp
			}
		}
		if chosen == nil {
			return nil, errors.New("no available reset card")
		}
		creditID = chosen.ID
	}

	redeemID := fmt.Sprintf("%s-%d", strings.ReplaceAll(creditID, "RateLimitResetCredit_", "fg"), time.Now().UnixNano())
	if len(redeemID) > 80 {
		redeemID = fmt.Sprintf("fg-%d", time.Now().UnixNano())
	}
	payload := map[string]any{
		"credit_id":         creditID,
		"redeem_request_id": redeemID,
	}
	result, _, err := a.doCodexQuotaRequestViaNode(ctx, http.MethodPost, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", accessToken, accountID, payload, nodeID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = map[string]any{}
	}
	result["credit_id"] = creditID
	return result, nil
}

func (a *App) authQuota(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/auth/quota/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		fail(w, http.StatusBadRequest, "invalid_provider_id", "valid provider ID required")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusBadRequest, "invalid_provider_id", "valid provider ID required")
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		result, err := a.withCodexCredential(r.Context(), id, func(credential ProviderCredential, nodeID *int64) (any, error) {
			return a.fetchCodexAccountQuotaViaNode(r.Context(), credential.AccessToken, credential.AccountID, nodeID)
		})
		if err != nil {
			a.writeQuotaError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case (action == "reset" || action == "redeem") && r.Method == http.MethodPost:
		var in struct {
			CreditID string `json:"credit_id"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
		}
		result, err := a.withCodexCredential(r.Context(), id, func(credential ProviderCredential, nodeID *int64) (any, error) {
			redeemed, redeemErr := a.redeemCodexResetCard(r.Context(), credential.AccessToken, credential.AccountID, strings.TrimSpace(in.CreditID), nodeID)
			if redeemErr != nil {
				return nil, redeemErr
			}
			quota, quotaErr := a.fetchCodexAccountQuotaViaNode(r.Context(), credential.AccessToken, credential.AccountID, nodeID)
			if quotaErr != nil {
				return map[string]any{"redeemed": redeemed, "quota": nil, "warning": quotaErr.Error()}, nil
			}
			return map[string]any{"ok": true, "redeemed": redeemed, "quota": quota}, nil
		})
		if err != nil {
			a.writeQuotaError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET /api/admin/auth/quota/{id} or POST /api/admin/auth/quota/{id}/reset required")
	}
}

func (a *App) writeQuotaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errProviderNotFound):
		fail(w, http.StatusNotFound, "provider_not_found", "provider not found")
	case errors.Is(err, errUnsupportedQuotaProvider):
		fail(w, http.StatusBadRequest, "unsupported_provider", err.Error())
	case err != nil && strings.Contains(err.Error(), "access token not found"):
		fail(w, http.StatusBadRequest, "missing_token", "access token not found")
	case err != nil && strings.Contains(err.Error(), "no available reset card"):
		fail(w, http.StatusBadRequest, "no_reset_card", "当前没有可用的重置卡")
	default:
		a.log.Warn("quota operation failed", "error", err)
		fail(w, http.StatusBadGateway, "quota_operation_failed", err.Error())
	}
}
