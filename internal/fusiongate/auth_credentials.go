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
	if !isTrustedXAIEndpoint(xaiOIDCDiscoveryURL) {
		return xaiOIDCConfiguration{}, errors.New("xAI discovery configuration is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xaiOIDCDiscoveryURL, nil)
	if err != nil {
		return xaiOIDCConfiguration{}, errors.New("xAI discovery configuration is invalid")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
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
		"scope": {"openid email profile offline_access"}, "state": {state}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"},
		"prompt": {"login"}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"},
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
		fail(w, http.StatusBadGateway, "oauth_exchange_failed", "authorization exchange failed; start a new login and try again")
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

func (a *App) readOAuthTokenResponse(req *http.Request, platform, source string) (ProviderCredential, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return ProviderCredential{}, errors.New("authentication service is unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProviderCredential{}, errors.New("cannot read authentication response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProviderCredential{}, fmt.Errorf("authentication service returned status %d", resp.StatusCode)
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
	rows, err := a.db.QueryContext(r.Context(), `SELECT p.id,p.name FROM providers p WHERE p.auth_kind='oauth' AND NOT EXISTS (SELECT 1 FROM model_routes r WHERE r.provider_id=p.id) ORDER BY p.id LIMIT 200`)
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
	if duplicateID > 0 {
		if !updateExisting {
			return duplicateID, requestedName, errDuplicateCredential
		}
		var currentName string
		err = a.db.QueryRowContext(ctx, `SELECT name FROM providers WHERE id=?`, duplicateID).Scan(&currentName)
		if err != nil {
			return 0, "", err
		}
		_, err = a.db.ExecContext(ctx, `UPDATE providers SET type=?,base_url=?,credential=?,auth_kind='oauth',auth_source=?,auth_account_id=?,auth_email=?,auth_expires_at=?,auth_last_refresh_at=?,auth_status=?,auth_fingerprint=?,auth_has_refresh=?,updated_at=? WHERE id=?`, oauthProviderType(c.Platform), oauthProviderBaseURL(c.Platform), encrypted, c.Source, c.AccountID, strings.ToLower(c.Email), nullableString(c.ExpiresAt), nullableString(c.LastRefresh), status, fingerprint, boolInt(c.RefreshToken != ""), now(), duplicateID)
		return duplicateID, currentName, err
	}
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name = suggestedCredentialName(c, 1)
	}
	name = a.uniqueProviderName(ctx, name)
	enabled := status != "expired"
	res, err := a.db.ExecContext(ctx, `INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,auth_kind,auth_source,auth_account_id,auth_email,auth_expires_at,auth_last_refresh_at,auth_status,auth_fingerprint,auth_has_refresh,created_at,updated_at) VALUES(?,?,?,?,?,?,100,'unknown','','normalized','any',0,120000,3,30,'oauth',?,?,?,?,?,?,?,?,?,?)`, name, oauthProviderType(c.Platform), oauthProviderBaseURL(c.Platform), encrypted, boolInt(enabled), priority, c.Source, c.AccountID, strings.ToLower(c.Email), nullableString(c.ExpiresAt), nullableString(c.LastRefresh), status, fingerprint, boolInt(c.RefreshToken != ""), now(), now())
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
	if endpoint := firstStringMaps(tokenMaps, []string{"token_endpoint"}, []string{"tokenEndpoint"}); endpoint != "" && isTrustedXAIEndpoint(endpoint) {
		c.Extra = map[string]any{"token_endpoint": endpoint}
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
	if z == nil || z.AuthCredential == nil {
		return nil
	}
	credential := *z.AuthCredential
	expires := parseTime(credential.ExpiresAt)
	if expires == nil || expires.After(time.Now().Add(2*time.Minute)) {
		return nil
	}
	if credential.RefreshToken == "" {
		return errors.New("OAuth credential expired and has no refresh token")
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
	if currentExpires := parseTime(current.ExpiresAt); currentExpires == nil || currentExpires.After(time.Now().Add(2*time.Minute)) {
		z.AuthCredential, z.Credential = &current, token
		return nil
	}
	refreshed, err := a.refreshOAuthCredential(ctx, current)
	if err != nil {
		_, _ = a.db.ExecContext(context.Background(), `UPDATE providers SET auth_status='refresh_failed',status='auth_expired',last_error='OAuth refresh failed',updated_at=? WHERE id=?`, now(), z.Provider.ID)
		return errors.New("OAuth token refresh failed")
	}
	payload, _ := json.Marshal(refreshed)
	sealed, err := a.encrypt(string(payload))
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE providers SET credential=?,auth_account_id=?,auth_email=?,auth_expires_at=?,auth_last_refresh_at=?,auth_status='ready',auth_has_refresh=?,updated_at=? WHERE id=?`, sealed, refreshed.AccountID, refreshed.Email, nullableString(refreshed.ExpiresAt), nullableString(refreshed.LastRefresh), boolInt(refreshed.RefreshToken != ""), now(), z.Provider.ID)
	if err != nil {
		return err
	}
	z.AuthCredential, z.Credential = &refreshed, refreshed.AccessToken
	return nil
}

func (a *App) refreshOAuthCredential(ctx context.Context, current ProviderCredential) (ProviderCredential, error) {
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
			config, err := a.xaiOIDCConfiguration(ctx)
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
	fresh, err := a.readOAuthTokenResponse(req, current.Platform, current.Source)
	if err != nil {
		return current, err
	}
	if fresh.RefreshToken == "" {
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
	fresh.Extra = current.Extra
	return fresh, nil
}
