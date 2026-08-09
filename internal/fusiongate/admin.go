package fusiongate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type adminCtx struct{ CSRF string }

type adminSession struct {
	CSRF      string
	ExpiresAt time.Time
}

func (a *App) sign(v string) string {
	h := hmac.New(sha256.New, []byte(a.cfg.MasterKey))
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (a *App) setAdminCookies(w http.ResponseWriter, r *http.Request) string {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	exp := time.Now().Add(12 * time.Hour)
	token := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	csrf := hex.EncodeToString(randomBytes(24))
	a.sessionMu.Lock()
	for id, session := range a.adminSessions {
		if time.Now().After(session.ExpiresAt) {
			delete(a.adminSessions, id)
		}
	}
	a.adminSessions[a.sign(token)] = adminSession{CSRF: csrf, ExpiresAt: exp}
	a.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "fg_admin", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure, Expires: exp})
	http.SetCookie(w, &http.Cookie{Name: "fg_csrf", Value: csrf, Path: "/", HttpOnly: false, SameSite: http.SameSiteStrictMode, Secure: secure, Expires: exp})
	return csrf
}
func (a *App) adminAuth(r *http.Request) (adminCtx, bool) {
	cookie, err := r.Cookie("fg_admin")
	if err != nil || cookie.Value == "" {
		return adminCtx{}, false
	}
	id := a.sign(cookie.Value)
	a.sessionMu.Lock()
	session, ok := a.adminSessions[id]
	if ok && time.Now().After(session.ExpiresAt) {
		delete(a.adminSessions, id)
		ok = false
	}
	a.sessionMu.Unlock()
	if !ok {
		return adminCtx{}, false
	}
	return adminCtx{CSRF: session.CSRF}, true
}
func (a *App) admin(fn func(http.ResponseWriter, *http.Request, adminCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, ok := a.adminAuth(r)
		if !ok {
			fail(w, 401, "unauthorized", "administrator sign-in required")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("X-CSRF-Token") == "" || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(ctx.CSRF)) {
				fail(w, 403, "csrf_failed", "missing or invalid CSRF token")
				return
			}
		}
		fn(w, r, ctx)
	}
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "invalid_request", "invalid JSON")
		return
	}
	clientID := loginClientID(r)
	if !a.allowAdminLogin(clientID) {
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many sign-in attempts")
		return
	}
	select {
	case a.loginVerifiers <- struct{}{}:
		defer func() { <-a.loginVerifiers }()
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many sign-in attempts")
		return
	}
	var h string
	if err := a.db.QueryRow(`SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&h); err != nil || !checkPassword(in.Password, h) {
		fail(w, 401, "invalid_credentials", "invalid credentials")
		return
	}
	a.loginMu.Lock()
	delete(a.loginAttempts, clientID)
	a.loginMu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "csrf_token": a.setAdminCookies(w, r)})
}

func loginClientID(r *http.Request) string {
	host := r.RemoteAddr
	if parsed, err := netip.ParseAddrPort(host); err == nil {
		host = parsed.Addr().String()
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.IsLoopback() {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			if parsed, err := netip.ParseAddr(forwarded); err == nil {
				host = parsed.String()
			}
		}
	}
	return host
}

func (a *App) allowAdminLogin(clientID string) bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	current := time.Now()
	for id, window := range a.loginAttempts {
		if current.Sub(window.At) >= time.Minute {
			delete(a.loginAttempts, id)
		}
	}
	for _, key := range []struct {
		id    string
		limit int
	}{{"global", 30}, {"client:" + clientID, 5}} {
		window := a.loginAttempts[key.id]
		if window != nil && window.Count >= key.limit {
			return false
		}
	}
	for _, id := range []string{"global", "client:" + clientID} {
		window := a.loginAttempts[id]
		if window == nil {
			a.loginAttempts[id] = &rateWindow{At: current, Count: 1}
		} else {
			window.Count++
		}
	}
	return true
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	if cookie, err := r.Cookie("fg_admin"); err == nil {
		a.sessionMu.Lock()
		delete(a.adminSessions, a.sign(cookie.Value))
		a.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "fg_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: "fg_csrf", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) session(w http.ResponseWriter, r *http.Request, c adminCtx) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	writeJSON(w, 200, map[string]any{"authenticated": true, "csrf_token": c.CSRF})
}
func (a *App) live(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "fusiongate", "time": now()})
}

func (a *App) readyHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	if !a.ready.Load() {
		fail(w, http.StatusServiceUnavailable, "service_not_ready", "service is not ready")
		return
	}
	// Use the same budget as SQLite busy_timeout. A tighter deadline makes
	// rolling deploys flake when the previous process is still releasing WAL.
	pingCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := a.db.PingContext(pingCtx); err != nil {
		// If the process is marked ready, prefer live-over-ready during brief
		// SQLite handoff instead of forcing Docker into an unhealthy restart loop.
		writeJSON(w, 200, map[string]any{"status": "degraded", "service": "fusiongate", "time": now(), "database": "busy"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "fusiongate", "time": now()})
}

func validProviderType(t string) bool {
	switch t {
	case "openai", "grok", "openrouter", "openai_compatible", "anthropic", "gemini", "codex_oauth", "claude_oauth", "grok_oauth", "gemini_cli":
		return true
	}
	return false
}

func validEditableProviderType(t string) bool {
	switch t {
	case "openai", "grok", "openrouter", "openai_compatible", "anthropic", "gemini":
		return true
	}
	return false
}
func (a *App) providers(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.reader().Query(`SELECT p.id,p.name,p.type,p.base_url,p.auth_kind,p.auth_source,p.auth_account_id,p.auth_email,COALESCE(p.auth_expires_at,''),p.auth_status,p.auth_has_refresh,p.enabled,p.archived,p.priority,p.sort_order,p.weight,p.status,p.notes,p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,p.health_check_enabled,p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,p.last_first_byte_ms,COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,''),(SELECT COUNT(*) FROM model_routes r WHERE r.provider_id=p.id),p.group_id,p.group_sort_order,COALESCE(p.last_health_check_at,''),p.health_check_status,p.health_check_error,p.health_check_latency_ms,p.health_check_mode,p.health_check_first_byte_ms,p.health_check_model,p.health_check_model_count,p.manual_balance_micros,COALESCE(p.balance_baseline_at,''),p.balance_multiplier_openai,p.balance_multiplier_claude,p.balance_multiplier_grok,p.balance_multiplier_gemini,p.balance_multiplier_other,p.ip_pool_node_id,COALESCE(n.name,''),COALESCE(n.protocol,''),p.default_model,(SELECT COUNT(*) FROM provider_api_keys k WHERE k.provider_id=p.id),(SELECT COUNT(*) FROM provider_api_keys k WHERE k.provider_id=p.id AND k.enabled=1) FROM providers p LEFT JOIN ip_pool_nodes n ON n.id=p.ip_pool_node_id ORDER BY p.sort_order,p.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []Provider{}
		for rows.Next() {
			var p Provider
			var enabled, archived, hasRefresh, healthCheckEnabled int
			var groupID, ipPoolNodeID sql.NullInt64
			var manualBalance sql.NullInt64
			if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthKind, &p.AuthSource, &p.AuthAccountID, &p.AuthEmail, &p.AuthExpiresAt, &p.AuthStatus, &hasRefresh, &enabled, &archived, &p.Priority, &p.SortOrder, &p.Weight, &p.Status, &p.Notes, &p.PassthroughMode, &p.ClientPolicy, &p.MaxConcurrency, &p.RequestTimeoutMS, &p.FailureThreshold, &p.CooldownSeconds, &healthCheckEnabled, &p.ConsecutiveFailures, &p.CircuitOpenUntil, &p.LastError, &p.LastLatencyMS, &p.LastFirstByteMS, &p.LastSuccessAt, &p.LastFailureAt, &p.ModelCount, &groupID, &p.GroupSortOrder, &p.LastHealthCheckAt, &p.HealthCheckStatus, &p.HealthCheckError, &p.HealthCheckLatencyMS, &p.HealthCheckMode, &p.HealthCheckFirstByteMS, &p.HealthCheckModel, &p.HealthCheckModelCount, &manualBalance, &p.BalanceBaselineAt, &p.BalanceMultiplierOpenAI, &p.BalanceMultiplierClaude, &p.BalanceMultiplierGrok, &p.BalanceMultiplierGemini, &p.BalanceMultiplierOther, &ipPoolNodeID, &p.IPPoolNodeName, &p.IPPoolNodeProtocol, &p.DefaultModel, &p.APIKeyCount, &p.EnabledAPIKeyCount); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			p.Enabled = strBool(enabled)
			p.Archived = strBool(archived)
			p.HealthCheckEnabled = strBool(healthCheckEnabled)
			p.HasRefreshToken = strBool(hasRefresh)
			p.CredentialHint = "configured"
			if p.AuthKind == "oauth" {
				p.CredentialHint = "oauth"
				p.AuthEmail = maskEmail(p.AuthEmail)
				p.AuthAccountID = maskIdentity(p.AuthAccountID)
			}
			if groupID.Valid {
				v := groupID.Int64
				p.GroupID = &v
			}
			if ipPoolNodeID.Valid {
				v := ipPoolNodeID.Int64
				p.IPPoolNodeID = &v
			}
			p.Inflight = a.providerInflight(p.ID)
			p.HealthScore = calculateHealthScore(p)
			if manualBalance.Valid {
				value := manualBalance.Int64
				p.ManualBalanceMicros = &value
			}
			out = append(out, p)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Name               string `json:"name"`
			Type               string `json:"type"`
			BaseURL            string `json:"baseURL"`
			Credential         string `json:"credential"`
			Notes              string `json:"notes"`
			Enabled            *bool  `json:"enabled"`
			Priority           *int   `json:"priority"`
			Weight             int    `json:"weight"`
			PassthroughMode    string `json:"passthrough_mode"`
			ClientPolicy       string `json:"client_policy"`
			MaxConcurrency     int    `json:"max_concurrency"`
			RequestTimeoutMS   int    `json:"request_timeout_ms"`
			FailureThreshold   int    `json:"failure_threshold"`
			HealthCheckEnabled *bool  `json:"health_check_enabled"`
			CooldownSeconds    int    `json:"cooldown_seconds"`
			AutoDiscover       *bool  `json:"auto_discover"`
			IPPoolNodeID       *int64 `json:"ip_pool_node_id"`
			DefaultModel       string `json:"default_model"`
			KeyName            string `json:"key_name"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		in.Type = strings.TrimSpace(in.Type)
		in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
		in.Credential = strings.TrimSpace(in.Credential)
		in.DefaultModel = normalizeProviderKeyModel(in.DefaultModel)
		in.KeyName = strings.TrimSpace(in.KeyName)
		if in.Name == "" || !validProviderType(in.Type) || in.Credential == "" {
			fail(w, http.StatusBadRequest, "invalid_request", "name, supported type, and credential are required")
			return
		}
		if err := validateUpstream(in.BaseURL, a.cfg); err != nil {
			fail(w, http.StatusBadRequest, "unsafe_upstream", err.Error())
			return
		}
		priority := 1
		if in.Priority != nil {
			priority = *in.Priority
		}
		if in.Weight == 0 {
			in.Weight = 100
		}
		if in.PassthroughMode == "" {
			in.PassthroughMode = "normalized"
		}
		if in.ClientPolicy == "" {
			in.ClientPolicy = "any"
		}
		if in.RequestTimeoutMS == 0 {
			in.RequestTimeoutMS = 120000
		}
		if in.FailureThreshold == 0 {
			in.FailureThreshold = 3
		}
		if in.CooldownSeconds == 0 {
			in.CooldownSeconds = 30
		}
		if in.IPPoolNodeID != nil && *in.IPPoolNodeID > 0 {
			if err := a.validateIPPoolNode(*in.IPPoolNodeID); err != nil {
				fail(w, http.StatusBadRequest, "invalid_ip_pool_node", err.Error())
				return
			}
		}
		if priority < 0 || in.Weight < 1 || in.MaxConcurrency < 0 || in.RequestTimeoutMS < 1000 || in.FailureThreshold < 1 || in.CooldownSeconds < 1 || !validPassthroughMode(in.PassthroughMode) || !validClientPolicy(in.ClientPolicy) {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid priority, weight, forwarding mode, client policy, concurrency, timeout, failure threshold, or cooldown")
			return
		}
		encrypted, err := a.encrypt(in.Credential)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_error", err.Error())
			return
		}
		healthCheckEnabled := true
		if in.HealthCheckEnabled != nil {
			healthCheckEnabled = *in.HealthCheckEnabled
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		var ipPoolNodeID any
		if in.IPPoolNodeID != nil && *in.IPPoolNodeID > 0 {
			ipPoolNodeID = *in.IPPoolNodeID
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		var sortOrder int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(sort_order),-1)+1 FROM providers`).Scan(&sortOrder); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		res, err := tx.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,sort_order,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,ip_pool_node_id,default_model,multi_key_initialized,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`, in.Name, in.Type, in.BaseURL, encrypted, boolInt(enabled), priority, sortOrder, in.Weight, "unknown", in.Notes, in.PassthroughMode, in.ClientPolicy, in.MaxConcurrency, in.RequestTimeoutMS, in.FailureThreshold, in.CooldownSeconds, ipPoolNodeID, in.DefaultModel, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "provider_conflict", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		if _, err := tx.Exec(`UPDATE providers SET health_check_enabled=? WHERE id=?`, boolInt(healthCheckEnabled), id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		keyName := firstNonEmpty(in.KeyName, "默认 Key")
		if _, err := tx.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,'','inherit',1,0,'untested',?,?)`, id, encrypted, a.providerKeyFingerprint(in.Credential), providerKeyHint(in.Credential), keyName, now(), now()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		response := map[string]any{"id": id, "message": "provider created; credential is encrypted at rest"}
		if in.AutoDiscover == nil || *in.AutoDiscover {
			discovery, discoveryErr := a.discoverProviderModels(r.Context(), id)
			if discoveryErr != nil {
				response["model_discovery"] = map[string]any{"status": "failed", "error": discoveryErr.Error()}
			} else {
				response["model_discovery"] = map[string]any{"status": "ok", "discovered": discovery.Discovered, "skipped": discovery.Skipped, "models": discovery.Models}
			}
		}
		writeJSON(w, http.StatusCreated, response)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) reorderProviders(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPatch {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH required")
		return
	}
	var in struct {
		ProviderIDs []int64 `json:"provider_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(in.ProviderIDs) == 0 {
		fail(w, http.StatusBadRequest, "invalid_request", "provider_ids are required")
		return
	}
	seen := make(map[int64]bool, len(in.ProviderIDs))
	for _, id := range in.ProviderIDs {
		if id < 1 || seen[id] {
			fail(w, http.StatusBadRequest, "invalid_order", "provider_ids must be unique positive IDs")
			return
		}
		seen[id] = true
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(r.Context(), `SELECT id FROM providers`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	actual := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		actual[id] = true
	}
	rows.Close()
	if len(actual) != len(seen) {
		fail(w, http.StatusBadRequest, "invalid_order", "provider_ids must contain every provider")
		return
	}
	for id := range seen {
		if !actual[id] {
			fail(w, http.StatusBadRequest, "invalid_order", "provider_ids contains an unknown provider")
			return
		}
	}
	for order, id := range in.ProviderIDs {
		if _, err := tx.ExecContext(r.Context(), `UPDATE providers SET sort_order=?,updated_at=? WHERE id=?`, order, now(), id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	a.routeMu.Lock()
	a.resetRouteCursorsLocked()
	a.routeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

const providerBatchMaxItems = 200

func (a *App) providerBatch(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		ProviderIDs []int64 `json:"provider_ids"`
		Action      string  `json:"action"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action != "enable" && in.Action != "disable" && in.Action != "delete" {
		fail(w, http.StatusBadRequest, "invalid_request", "action must be enable, disable, or delete")
		return
	}
	if len(in.ProviderIDs) == 0 || len(in.ProviderIDs) > providerBatchMaxItems {
		fail(w, http.StatusBadRequest, "invalid_request", "select between 1 and 200 providers")
		return
	}
	ids := make([]int64, 0, len(in.ProviderIDs))
	seen := make(map[int64]struct{}, len(in.ProviderIDs))
	for _, id := range in.ProviderIDs {
		if id < 1 {
			fail(w, http.StatusBadRequest, "invalid_request", "provider_ids must contain positive integers")
			return
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.Query(`SELECT id,auth_kind FROM providers WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	found := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var authKind string
		if err := rows.Scan(&id, &authKind); err != nil {
			rows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		found[id] = authKind
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if err := rows.Close(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if len(found) != len(ids) {
		fail(w, http.StatusBadRequest, "invalid_provider_selection", "one or more authentication files no longer exist")
		return
	}
	for _, id := range ids {
		if found[id] != "oauth" {
			fail(w, http.StatusBadRequest, "invalid_provider_selection", "batch actions only support OAuth authentication files")
			return
		}
	}

	var res sql.Result
	switch in.Action {
	case "enable":
		updateArgs := append([]any{now()}, args...)
		res, err = tx.Exec(`UPDATE providers SET enabled=1,status='unknown',consecutive_failures=0,circuit_open_until=NULL,last_error='',last_failure_at=NULL,updated_at=? WHERE id IN (`+placeholders+`)`, updateArgs...)
	case "disable":
		updateArgs := append([]any{now()}, args...)
		res, err = tx.Exec(`UPDATE providers SET enabled=0,updated_at=? WHERE id IN (`+placeholders+`)`, updateArgs...)
	case "delete":
		res, err = tx.Exec(`DELETE FROM providers WHERE id IN (`+placeholders+`)`, args...)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	affected, err := res.RowsAffected()
	if err != nil || affected != int64(len(ids)) {
		if err == nil {
			err = fmt.Errorf("expected to update %d providers, updated %d", len(ids), affected)
		}
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	for _, id := range ids {
		a.resetProviderRuntime(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": in.Action, "affected": affected})
}

func (a *App) healthChecks(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if a.healthCheckJobs == nil {
		fail(w, http.StatusServiceUnavailable, "health_check_unavailable", "health check service is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		job, err := a.healthCheckJobs.Active()
		if errors.Is(err, errHealthCheckJobNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "health_check_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active": true, "job": job})
	case http.MethodPost:
		var in struct {
			ProviderIDs    []int64 `json:"provider_ids"`
			RouteIDs       []int64 `json:"route_ids"`
			ProviderKeyIDs []int64 `json:"provider_key_ids"`
			ModelScope     string  `json:"model_scope"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		job, err := a.healthCheckJobs.StartModels(r.Context(), in.ProviderIDs, in.RouteIDs, in.ProviderKeyIDs, in.ModelScope)
		if errors.Is(err, errHealthCheckAlreadyRunning) {
			fail(w, http.StatusConflict, "health_check_running", "another health check is already running; wait for it to finish or cancel it")
			return
		}
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_provider_selection", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) healthCheckByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if a.healthCheckJobs == nil {
		fail(w, http.StatusServiceUnavailable, "health_check_unavailable", "health check service is unavailable")
		return
	}
	jobID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/health-checks/"), "/")
	if jobID == "" || len(jobID) > 64 || strings.Contains(jobID, "/") {
		fail(w, http.StatusNotFound, "not_found", "health check job not found")
		return
	}
	var (
		job healthCheckJob
		err error
	)
	switch r.Method {
	case http.MethodGet:
		job, err = a.healthCheckJobs.Get(jobID)
	case http.MethodDelete:
		job, err = a.healthCheckJobs.Cancel(jobID)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or DELETE required")
		return
	}
	if errors.Is(err, errHealthCheckJobNotFound) {
		fail(w, http.StatusNotFound, "not_found", "health check job not found")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "health_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *App) providerByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/providers/"), "/")
	parts := strings.Split(suffix, "/")
	idText := parts[0]
	if !isID(idText) {
		fail(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	id, _ := strconv.ParseInt(idText, 10, 64)
	if len(parts) == 2 && parts[1] == "balance" {
		a.providerBalanceHandler(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "keys" {
		a.providerKeys(w, r, id)
		return
	}
	if len(parts) >= 3 && parts[1] == "keys" {
		keyID, action, ok := parseProviderKeyPath(parts[1:])
		if !ok {
			fail(w, http.StatusNotFound, "not_found", "API key action not found")
			return
		}
		a.providerKeyByID(w, r, id, keyID, action)
		return
	}
	if len(parts) == 2 && parts[1] == "discover-models" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		result, err := a.discoverProviderModels(r.Context(), id)
		if err != nil {
			fail(w, discoveryErrorStatus(err), "model_discovery_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if len(parts) == 2 && parts[1] == "models" {
		if r.Method != http.MethodPut {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT required")
			return
		}
		var in struct {
			Models *[]string `json:"models"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.Models == nil {
			fail(w, http.StatusBadRequest, "invalid_request", "models is required")
			return
		}
		result, err := a.applySelectedModels(r.Context(), id, *in.Models)
		if err != nil {
			fail(w, modelImportErrorStatus(err), "model_selection_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if len(parts) == 2 && parts[1] == "import-models" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var in struct {
			Models []string `json:"models"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		result, err := a.importSelectedModels(r.Context(), id, in.Models)
		if err != nil {
			fail(w, modelImportErrorStatus(err), "model_import_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if len(parts) != 1 {
		fail(w, http.StatusNotFound, "not_found", "provider action not found")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		a.providerDelete(w, id)
	case http.MethodPatch:
		a.providerUpdate(w, r, id)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}

// providerDelete removes one provider and forgets the scheduler state that referenced it.
func (a *App) providerDelete(w http.ResponseWriter, id int64) {
	res, err := a.db.Exec(`DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	a.resetProviderRuntime(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// providerUpdate applies a partial provider edit. Every field is optional, so a key
// rotation or a scheduling tweak never has to resend the whole provider.
func (a *App) providerUpdate(w http.ResponseWriter, r *http.Request, id int64) {
	var in struct {
		Name                    *string  `json:"name"`
		Type                    *string  `json:"type"`
		BaseURL                 *string  `json:"baseURL"`
		Credential              *string  `json:"credential"`
		Enabled                 *bool    `json:"enabled"`
		Archived                *bool    `json:"archived"`
		Priority                *int     `json:"priority"`
		Weight                  *int     `json:"weight"`
		Notes                   *string  `json:"notes"`
		PassthroughMode         *string  `json:"passthrough_mode"`
		ClientPolicy            *string  `json:"client_policy"`
		MaxConcurrency          *int     `json:"max_concurrency"`
		RequestTimeoutMS        *int     `json:"request_timeout_ms"`
		FailureThreshold        *int     `json:"failure_threshold"`
		CooldownSeconds         *int     `json:"cooldown_seconds"`
		ResetHealth             bool     `json:"reset_health"`
		HealthCheckEnabled      *bool    `json:"health_check_enabled"`
		GroupID                 *int64   `json:"group_id"`
		ClearGroup              bool     `json:"clear_group"`
		GroupSortOrder          *int     `json:"group_sort_order"`
		ManualBalanceUSD        *float64 `json:"manual_balance_usd"`
		ClearManualBalance      bool     `json:"clear_manual_balance"`
		BalanceMultiplierOpenAI *float64 `json:"balance_multiplier_openai"`
		BalanceMultiplierClaude *float64 `json:"balance_multiplier_claude"`
		BalanceMultiplierGrok   *float64 `json:"balance_multiplier_grok"`
		BalanceMultiplierGemini *float64 `json:"balance_multiplier_gemini"`
		BalanceMultiplierOther  *float64 `json:"balance_multiplier_other"`
		IPPoolNodeID            *int64   `json:"ip_pool_node_id"`
		DefaultModel            *string  `json:"default_model"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var currentEnabled int
	var authKind, currentName, currentType, currentBaseURL string
	if err := a.db.QueryRow(`SELECT enabled,auth_kind,name,type,base_url FROM providers WHERE id=?`, id).Scan(&currentEnabled, &authKind, &currentName, &currentType, &currentBaseURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	connectionEditRequested := in.Name != nil || in.Type != nil || in.BaseURL != nil || in.Credential != nil
	if connectionEditRequested && authKind != "api_key" {
		fail(w, http.StatusBadRequest, "invalid_request", "OAuth providers must be managed from credential files")
		return
	}
	if in.Name != nil {
		value := strings.TrimSpace(*in.Name)
		if value == "" {
			fail(w, http.StatusBadRequest, "invalid_request", "provider name is required")
			return
		}
		in.Name = &value
	}
	if in.Type != nil {
		value := strings.TrimSpace(*in.Type)
		if !validEditableProviderType(value) {
			fail(w, http.StatusBadRequest, "invalid_request", "unsupported editable provider type")
			return
		}
		in.Type = &value
	}
	if in.BaseURL != nil {
		value := strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
		if err := validateUpstream(value, a.cfg); err != nil {
			fail(w, http.StatusBadRequest, "unsafe_upstream", err.Error())
			return
		}
		in.BaseURL = &value
	}
	var encryptedCredential any
	credentialUpdated := false
	if in.Credential != nil {
		value := strings.TrimSpace(*in.Credential)
		if value == "" {
			in.Credential = nil
		} else {
			encrypted, err := a.encrypt(value)
			if err != nil {
				fail(w, http.StatusInternalServerError, "credential_error", err.Error())
				return
			}
			encryptedCredential = encrypted
			credentialUpdated = true
		}
	}
	if in.DefaultModel != nil {
		value := normalizeProviderKeyModel(*in.DefaultModel)
		in.DefaultModel = &value
	}
	connectionChanged := credentialUpdated || (in.Name != nil && *in.Name != currentName) || (in.Type != nil && *in.Type != currentType) || (in.BaseURL != nil && *in.BaseURL != currentBaseURL) || in.IPPoolNodeID != nil || in.DefaultModel != nil
	discoveryConnectionChanged := credentialUpdated || (in.Type != nil && *in.Type != currentType) || (in.BaseURL != nil && *in.BaseURL != currentBaseURL)
	if in.IPPoolNodeID != nil && *in.IPPoolNodeID > 0 {
		if err := a.validateIPPoolNode(*in.IPPoolNodeID); err != nil {
			fail(w, http.StatusBadRequest, "invalid_ip_pool_node", err.Error())
			return
		}
	}
	if (in.Priority != nil && *in.Priority < 0) || (in.Weight != nil && *in.Weight < 1) || (in.MaxConcurrency != nil && *in.MaxConcurrency < 0) || (in.RequestTimeoutMS != nil && *in.RequestTimeoutMS < 1000) || (in.FailureThreshold != nil && *in.FailureThreshold < 1) || (in.CooldownSeconds != nil && *in.CooldownSeconds < 1) || (in.PassthroughMode != nil && !validPassthroughMode(*in.PassthroughMode)) || (in.ClientPolicy != nil && !validClientPolicy(*in.ClientPolicy)) {
		fail(w, http.StatusBadRequest, "invalid_request", "invalid provider scheduling or forwarding configuration")
		return
	}
	if in.ManualBalanceUSD != nil && (*in.ManualBalanceUSD < 0 || *in.ManualBalanceUSD > 1_000_000) {
		fail(w, http.StatusBadRequest, "invalid_request", "manual balance must be between 0 and 1000000 USD")
		return
	}
	for _, multiplier := range []*float64{in.BalanceMultiplierOpenAI, in.BalanceMultiplierClaude, in.BalanceMultiplierGrok, in.BalanceMultiplierGemini, in.BalanceMultiplierOther} {
		if multiplier != nil && (*multiplier < 0 || *multiplier > 1000) {
			fail(w, http.StatusBadRequest, "invalid_request", "balance multipliers must be between 0 and 1000")
			return
		}
	}
	// Re-enabling a channel or changing its connection details starts a
	// fresh health window so an old circuit state does not hide a new key.
	resetOnEnable := in.Enabled != nil && *in.Enabled && !strBool(currentEnabled)
	var groupIDArg any
	if in.ClearGroup {
		groupIDArg = sql.NullInt64{}
	} else if in.GroupID != nil {
		groupIDArg = *in.GroupID
	}
	groupAssignRequested := in.ClearGroup || in.GroupID != nil
	var ipPoolNodeArg any
	if in.IPPoolNodeID != nil && *in.IPPoolNodeID > 0 {
		ipPoolNodeArg = *in.IPPoolNodeID
	}
	if credentialUpdated {
		var firstKeyID int64
		if err := a.db.QueryRow(`SELECT id FROM provider_api_keys WHERE provider_id=? ORDER BY sort_order,id LIMIT 1`, id).Scan(&firstKeyID); err == nil {
			raw := strings.TrimSpace(*in.Credential)
			if _, err := a.db.Exec(`UPDATE provider_api_keys SET credential=?,fingerprint=?,key_hint=?,status='untested',last_error='',updated_at=? WHERE id=?`, encryptedCredential, a.providerKeyFingerprint(raw), providerKeyHint(raw), now(), firstKeyID); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "unique") {
					fail(w, http.StatusConflict, "duplicate_api_key", "this API key already exists in the provider")
				} else {
					fail(w, http.StatusInternalServerError, "database_error", err.Error())
				}
				return
			}
		}
	}
	res, err := a.db.Exec(`UPDATE providers SET name=COALESCE(?,name),type=COALESCE(?,type),base_url=COALESCE(?,base_url),credential=COALESCE(?,credential),enabled=COALESCE(?,enabled),archived=COALESCE(?,archived),priority=COALESCE(?,priority),weight=COALESCE(?,weight),notes=COALESCE(?,notes),passthrough_mode=COALESCE(?,passthrough_mode),client_policy=COALESCE(?,client_policy),max_concurrency=COALESCE(?,max_concurrency),request_timeout_ms=COALESCE(?,request_timeout_ms),failure_threshold=COALESCE(?,failure_threshold),cooldown_seconds=COALESCE(?,cooldown_seconds),health_check_enabled=COALESCE(?,health_check_enabled),group_id=CASE WHEN ? THEN ? ELSE group_id END,group_sort_order=COALESCE(?,group_sort_order),ip_pool_node_id=CASE WHEN ? THEN ? ELSE ip_pool_node_id END,default_model=COALESCE(?,default_model),updated_at=? WHERE id=?`, in.Name, in.Type, in.BaseURL, encryptedCredential, maybeBool(in.Enabled), maybeBool(in.Archived), in.Priority, in.Weight, in.Notes, in.PassthroughMode, in.ClientPolicy, in.MaxConcurrency, in.RequestTimeoutMS, in.FailureThreshold, in.CooldownSeconds, maybeBool(in.HealthCheckEnabled), groupAssignRequested, groupIDArg, in.GroupSortOrder, in.IPPoolNodeID != nil, ipPoolNodeArg, in.DefaultModel, now(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	if in.HealthCheckEnabled != nil {
		status, message := "pending", ""
		if !*in.HealthCheckEnabled {
			status, message = "disabled", "health checks disabled for this provider"
		}
		if _, err := a.db.Exec(`UPDATE providers SET health_check_status=?,health_check_error=?,updated_at=? WHERE id=?`, status, message, now(), id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
	}
	if discoveryConnectionChanged {
		if _, err := a.db.Exec(`DELETE FROM provider_api_key_models WHERE provider_key_id IN (SELECT id FROM provider_api_keys WHERE provider_id=?)`, id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if _, err := a.db.Exec(`UPDATE provider_api_keys SET status='untested',last_error='',updated_at=? WHERE provider_id=?`, now(), id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
	}
	if in.ManualBalanceUSD != nil || in.ClearManualBalance || in.BalanceMultiplierOpenAI != nil || in.BalanceMultiplierClaude != nil || in.BalanceMultiplierGrok != nil || in.BalanceMultiplierGemini != nil || in.BalanceMultiplierOther != nil {
		var balance any
		var baseline any
		if in.ManualBalanceUSD != nil {
			balance = int64(*in.ManualBalanceUSD*1_000_000 + 0.5)
			baseline = now()
		} else if in.ClearManualBalance {
			balance = nil
			baseline = nil
		}
		_, err = a.db.Exec(`UPDATE providers SET manual_balance_micros=CASE WHEN ? THEN ? ELSE manual_balance_micros END,balance_baseline_at=CASE WHEN ? THEN ? ELSE balance_baseline_at END,balance_multiplier_openai=COALESCE(?,balance_multiplier_openai),balance_multiplier_claude=COALESCE(?,balance_multiplier_claude),balance_multiplier_grok=COALESCE(?,balance_multiplier_grok),balance_multiplier_gemini=COALESCE(?,balance_multiplier_gemini),balance_multiplier_other=COALESCE(?,balance_multiplier_other),updated_at=? WHERE id=?`, in.ManualBalanceUSD != nil || in.ClearManualBalance, balance, in.ManualBalanceUSD != nil || in.ClearManualBalance, baseline, in.BalanceMultiplierOpenAI, in.BalanceMultiplierClaude, in.BalanceMultiplierGrok, in.BalanceMultiplierGemini, in.BalanceMultiplierOther, now(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
	}
	if in.ResetHealth || resetOnEnable || connectionChanged {
		_, err = a.db.Exec(`UPDATE providers SET status='unknown',consecutive_failures=0,circuit_open_until=NULL,last_error='',last_failure_at=NULL,updated_at=? WHERE id=?`, now(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		a.resetProviderRuntime(id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "credential_updated": credentialUpdated})
}

func maybeBool(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
}

// calculateHealthScore computes a 0-100 score for sorting OAuth providers.
// Non-OAuth providers always return 100 (they rely on existing circuit-breaker status).
func calculateHealthScore(p Provider) int {
	if p.AuthKind != "oauth" {
		return 100
	}
	var base int
	switch p.HealthCheckStatus {
	case "healthy":
		base = 100
	case "reachable":
		base = 70
	case "disabled":
		base = 75
	case "pending", "":
		base = 75
	case "rate_limited":
		base = 40
	case "timeout", "network_error", "server_error":
		base = 30
	case "auth_expired":
		base = 0
	default:
		base = 50
	}
	penalty := p.ConsecutiveFailures * 8
	if p.HealthCheckLatencyMS > 3000 {
		penalty += 10
	} else if p.HealthCheckLatencyMS > 1500 {
		penalty += 5
	}
	score := base - penalty
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}
func isID(v string) bool { _, e := strconv.ParseInt(v, 10, 64); return e == nil }

func routeHealthScore(status string, latency int64, failures, inflight int) int {
	score := 100
	switch status {
	case "circuit_open":
		return 0
	case "auth_expired":
		score = 8
	case "rate_limited":
		score = 35
	case "degraded":
		score = 62
	case "unknown", "":
		score = 78
	}
	score -= failures * 12
	if latency > 0 {
		penalty := int(latency / 250)
		if penalty > 24 {
			penalty = 24
		}
		score -= penalty
	}
	score -= inflight * 3
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func (a *App) routes(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`
SELECT r.id,r.provider_id,r.public_name,r.upstream_model,r.capabilities,r.enabled,r.priority,r.sort_order,
	       r.input_price_micros,r.cached_price_micros,r.output_price_micros,
	       r.long_context_threshold,r.long_input_price_micros,r.long_cached_price_micros,r.long_output_price_micros,
	       r.pricing_source,COALESCE(r.pricing_updated_at,''),p.name,p.type,
	       p.enabled,p.status,p.last_latency_ms,p.last_first_byte_ms,p.consecutive_failures,
	       COALESCE(r.last_health_check_at,''),r.health_check_status,r.health_check_error,
	       r.health_check_latency_ms,r.health_check_first_byte_ms
FROM model_routes r
JOIN providers p ON p.id=r.provider_id
ORDER BY r.public_name,r.sort_order,r.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []Route{}
		for rows.Next() {
			var x Route
			var en, providerEnabled int
			if err := rows.Scan(&x.ID, &x.ProviderID, &x.PublicName, &x.UpstreamModel, &x.Capabilities, &en, &x.Priority, &x.SortOrder,
				&x.InputPriceMicros, &x.CachedPriceMicros, &x.OutputPriceMicros,
				&x.LongContextThreshold, &x.LongInputPriceMicros, &x.LongCachedPriceMicros, &x.LongOutputPriceMicros,
				&x.PricingSource, &x.PricingUpdatedAt, &x.ProviderName, &x.ProviderType, &providerEnabled,
				&x.ProviderStatus, &x.ProviderLatencyMS, &x.ProviderFirstByteMS, &x.ProviderFailures,
				&x.LastHealthCheckAt, &x.HealthCheckStatus, &x.HealthCheckError,
				&x.HealthCheckLatencyMS, &x.HealthCheckFirstByteMS); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			x.Enabled = strBool(en)
			x.ProviderEnabled = strBool(providerEnabled)
			x.ProviderInflight = a.providerInflight(x.ProviderID)
			if x.ProviderEnabled {
				observedLatency := x.ProviderFirstByteMS
				if observedLatency <= 0 {
					observedLatency = x.ProviderLatencyMS
				}
				x.HealthScore = routeHealthScore(x.ProviderStatus, observedLatency, x.ProviderFailures, x.ProviderInflight)
			}
			out = append(out, x)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			ProviderID        int64  `json:"provider_id"`
			PublicName        string `json:"public_name"`
			UpstreamModel     string `json:"upstream_model"`
			Capabilities      string `json:"capabilities"`
			Enabled           *bool  `json:"enabled"`
			Priority          *int   `json:"priority"`
			InputPriceMicros  int64  `json:"input_price_micros"`
			OutputPriceMicros int64  `json:"output_price_micros"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.PublicName = strings.ToLower(strings.TrimSpace(in.PublicName))
		in.UpstreamModel = strings.ToLower(strings.TrimSpace(in.UpstreamModel))
		if in.ProviderID < 1 || in.PublicName == "" || in.UpstreamModel == "" {
			fail(w, http.StatusBadRequest, "invalid_request", "provider_id, public_name, and upstream_model are required")
			return
		}
		if in.Capabilities == "" {
			in.Capabilities = "chat,stream"
		}
		priority := 0
		if in.Priority != nil {
			priority = *in.Priority
		}
		if priority < 0 {
			fail(w, http.StatusBadRequest, "invalid_priority", "priority must be zero or greater")
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		var sortOrder int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(sort_order),-1)+1 FROM model_routes WHERE public_name=?`, in.PublicName).Scan(&sortOrder); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		res, err := tx.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,input_price_micros,output_price_micros,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, in.PublicName, in.ProviderID, in.UpstreamModel, in.Capabilities, boolInt(enabled), priority, sortOrder, in.InputPriceMicros, in.OutputPriceMicros, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "route_conflict", err.Error())
			return
		}
		if _, err := tx.Exec(`DELETE FROM model_route_exclusions WHERE provider_id=? AND (LOWER(public_name) IN (?,?) OR LOWER(upstream_model) IN (?,?))`, in.ProviderID, in.PublicName, in.UpstreamModel, in.PublicName, in.UpstreamModel); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if _, err := tx.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO NOTHING`, in.PublicName, StrategyPriorityFailover, now()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		if in.InputPriceMicros == 0 && in.OutputPriceMicros == 0 {
			a.triggerPricingSync()
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) routeByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/routes/")
	if !isID(id) {
		fail(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Enabled    *bool   `json:"enabled"`
			Priority   *int    `json:"priority"`
			PublicName *string `json:"public_name"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.Enabled == nil && in.Priority == nil && in.PublicName == nil {
			fail(w, http.StatusBadRequest, "invalid_request", "enabled, priority, or public_name is required")
			return
		}
		if in.Priority != nil && *in.Priority < 0 {
			fail(w, http.StatusBadRequest, "invalid_priority", "priority must be zero or greater")
			return
		}
		newPublicName := ""
		if in.PublicName != nil {
			newPublicName = strings.ToLower(strings.TrimSpace(*in.PublicName))
			if newPublicName == "" {
				fail(w, http.StatusBadRequest, "invalid_public_name", "public_name is required")
				return
			}
		}

		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		var oldPublicName, upstreamModel string
		var providerID int64
		if err := tx.QueryRowContext(r.Context(), `SELECT public_name,provider_id,upstream_model FROM model_routes WHERE id=?`, id).Scan(&oldPublicName, &providerID, &upstreamModel); err != nil {
			if err == sql.ErrNoRows {
				fail(w, http.StatusNotFound, "not_found", "route not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		oldPublicName = strings.ToLower(strings.TrimSpace(oldPublicName))
		if in.PublicName == nil {
			newPublicName = oldPublicName
		}
		if newPublicName != oldPublicName {
			var duplicate int
			if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM model_routes WHERE id<>? AND public_name=? AND provider_id=? AND upstream_model=?)`, id, newPublicName, providerID, upstreamModel).Scan(&duplicate); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			if duplicate != 0 {
				fail(w, http.StatusConflict, "route_conflict", "this channel and upstream model are already in the target failover group")
				return
			}
		}
		sortOrderExpr := `sort_order`
		args := []any{maybeBool(in.Enabled), in.Priority, newPublicName}
		if newPublicName != oldPublicName {
			sortOrderExpr = `(SELECT COALESCE(MAX(sort_order),-1)+1 FROM model_routes WHERE public_name=?)`
			args = append(args, newPublicName)
		}
		args = append(args, now(), id)
		query := `UPDATE model_routes SET enabled=COALESCE(?,enabled),priority=COALESCE(?,priority),public_name=?,sort_order=` + sortOrderExpr + `,updated_at=? WHERE id=?`
		res, err := tx.ExecContext(r.Context(), query, args...)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO NOTHING`, newPublicName, StrategyPriorityFailover, now()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM route_policies WHERE public_name=? AND NOT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, oldPublicName, oldPublicName); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		upstreamKey := strings.ToLower(strings.TrimSpace(upstreamModel))
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM model_route_exclusions WHERE provider_id=? AND (LOWER(public_name)=? OR LOWER(upstream_model)=?)`, providerID, upstreamKey, upstreamKey); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		var groupRoutes int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM model_routes WHERE public_name=?`, newPublicName).Scan(&groupRoutes); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		a.routeMu.Lock()
		a.forgetRouteCursorsLocked(oldPublicName)
		a.forgetRouteCursorsLocked(newPublicName)
		a.routeMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "public_name": newPublicName, "group_routes": groupRoutes})
	case http.MethodDelete:
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		var publicName, upstreamModel string
		var providerID int64
		if err := tx.QueryRow(`SELECT public_name,provider_id,upstream_model FROM model_routes WHERE id=?`, id).Scan(&publicName, &providerID, &upstreamModel); err != nil {
			if err == sql.ErrNoRows {
				fail(w, http.StatusNotFound, "not_found", "route not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		upstreamKey := strings.ToLower(strings.TrimSpace(upstreamModel))
		if _, err := tx.Exec(`INSERT OR REPLACE INTO model_route_exclusions(provider_id,public_name,upstream_model,created_at) VALUES(?,?,?,?)`, providerID, upstreamKey, upstreamKey, now()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if _, err := tx.Exec(`DELETE FROM model_routes WHERE id=?`, id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		_, _ = tx.Exec(`DELETE FROM route_policies WHERE public_name=? AND NOT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, publicName, publicName)
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		a.routeMu.Lock()
		a.forgetRouteCursorsLocked(strings.ToLower(strings.TrimSpace(publicName)))
		a.routeMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}

func (a *App) reorderRoutes(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPatch {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH required")
		return
	}
	var in struct {
		PublicName string  `json:"public_name"`
		RouteIDs   []int64 `json:"route_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	in.PublicName = strings.ToLower(strings.TrimSpace(in.PublicName))
	if in.PublicName == "" || len(in.RouteIDs) == 0 {
		fail(w, http.StatusBadRequest, "invalid_request", "public_name and route_ids are required")
		return
	}
	seen := make(map[int64]bool, len(in.RouteIDs))
	for _, id := range in.RouteIDs {
		if id < 1 || seen[id] {
			fail(w, http.StatusBadRequest, "invalid_order", "route_ids must be unique positive IDs")
			return
		}
		seen[id] = true
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM model_routes WHERE public_name=?`, in.PublicName)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	actual := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		actual[id] = true
	}
	rows.Close()
	if len(actual) != len(seen) {
		fail(w, http.StatusBadRequest, "invalid_order", "route_ids must contain every route for this public model")
		return
	}
	for id := range seen {
		if !actual[id] {
			fail(w, http.StatusBadRequest, "invalid_order", "route_ids contains a route from another public model")
			return
		}
	}
	for order, id := range in.RouteIDs {
		if _, err := tx.Exec(`UPDATE model_routes SET sort_order=?,updated_at=? WHERE id=?`, order, now(), id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	a.routeMu.Lock()
	a.forgetRouteCursorsLocked(in.PublicName)
	a.routeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func normalizeModelList(value string) string {
	seen := map[string]bool{}
	models := make([]string, 0)
	for _, model := range strings.Split(value, ",") {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	return strings.Join(models, ",")
}

func (a *App) keys(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.reader().Query(`SELECT id,name,key_prefix,allow_all,allow_models,deny_models,allow_images,rpm_limit,revoked,COALESCE(expires_at,''),created_at,encrypted_key IS NOT NULL,budget_micros,spent_micros FROM api_keys ORDER BY id DESC`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []APIKey{}
		for rows.Next() {
			var x APIKey
			var aa, ai, rv, canReveal int
			var ex, cr string
			if err := rows.Scan(&x.ID, &x.Name, &x.Prefix, &aa, &x.AllowModels, &x.DenyModels, &ai, &x.RPMLimit, &rv, &ex, &cr, &canReveal, &x.BudgetMicros, &x.SpentMicros); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			x.AllowAll = strBool(aa)
			x.AllowImages = strBool(ai)
			x.Revoked = strBool(rv)
			x.CanReveal = strBool(canReveal)
			if x.BudgetMicros > 0 {
				x.RemainingMicros = x.BudgetMicros - x.SpentMicros
				if x.RemainingMicros < 0 {
					x.RemainingMicros = 0
				}
			}
			x.ExpiresAt = parseTime(ex)
			createdAt := parseTime(cr)
			if createdAt != nil {
				x.CreatedAt = *createdAt
			}
			out = append(out, x)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Name         string `json:"name"`
			AllowModels  string `json:"allow_models"`
			DenyModels   string `json:"deny_models"`
			AllowAll     bool   `json:"allow_all"`
			AllowImages  bool   `json:"allow_images"`
			RPMLimit     int    `json:"rpm_limit"`
			ExpiresAt    string `json:"expires_at"`
			BudgetMicros int64  `json:"budget_micros"`
		}

		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" {
			fail(w, http.StatusBadRequest, "invalid_request", "name is required")
			return
		}
		if in.RPMLimit <= 0 {
			in.RPMLimit = 120
		}
		in.AllowModels = normalizeModelList(in.AllowModels)
		in.DenyModels = normalizeModelList(in.DenyModels)
		if in.AllowAll {
			in.AllowModels = ""
		}
		raw := "fg_" + hex.EncodeToString(randomBytes(24))
		sum := sha256.Sum256([]byte(raw))
		prefix := raw[:11]
		encrypted, err := a.encrypt(raw)
		if err != nil {
			fail(w, http.StatusInternalServerError, "encryption_error", "could not protect API key")
			return
		}
		var exp any = nil
		if in.ExpiresAt != "" {
			if _, e := time.Parse(time.RFC3339, in.ExpiresAt); e != nil {
				fail(w, http.StatusBadRequest, "invalid_request", "expires_at must be RFC3339")
				return
			}
			exp = in.ExpiresAt
		}
		if in.BudgetMicros < 0 {
			fail(w, http.StatusBadRequest, "invalid_request", "budget_micros must be zero or greater")
			return
		}
		res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,expires_at,budget_micros,created_at,encrypted_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, in.Name, prefix, hex.EncodeToString(sum[:]), boolInt(in.AllowAll), in.AllowModels, in.DenyModels, boolInt(in.AllowImages), in.RPMLimit, exp, in.BudgetMicros, now(), encrypted)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "key": raw, "can_reveal": true, "message": "API key created and encrypted for administrator recovery."})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) keyByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	remainder := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/keys/"), "/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 2 && parts[1] == "reveal" && isID(parts[0]) {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST /api/admin/keys/{id}/reveal required")
			return
		}
		var encrypted []byte
		var revoked int
		err := a.db.QueryRow(`SELECT encrypted_key,revoked FROM api_keys WHERE id=?`, parts[0]).Scan(&encrypted, &revoked)
		if err == sql.ErrNoRows {
			fail(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if strBool(revoked) {
			fail(w, http.StatusGone, "key_revoked", "revoked keys cannot be revealed")
			return
		}
		if len(encrypted) == 0 {
			fail(w, http.StatusConflict, "key_not_recoverable", "this legacy key was stored only as a hash; create a replacement key")
			return
		}
		raw, err := a.decrypt(encrypted)
		if err != nil {
			fail(w, http.StatusInternalServerError, "decryption_error", "could not decrypt API key")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"key": raw})
		return
	}
	if len(parts) != 1 || !isID(parts[0]) || r.Method != http.MethodDelete {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "DELETE /api/admin/keys/{id} or POST /api/admin/keys/{id}/reveal required")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM request_ledger WHERE api_key_id=?`, parts[0]); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	res, err := tx.Exec(`DELETE FROM api_keys WHERE id=?`, parts[0])
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) runtimeMetrics(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	writeJSON(w, http.StatusOK, a.metrics.snapshot())
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var p, m, k, total, today, failures int
	var input, output, cached, reasoning, costMicros int64
	a.reader().QueryRow(`SELECT COUNT(*) FROM providers WHERE enabled=1`).Scan(&p)
	a.reader().QueryRow(`SELECT COUNT(DISTINCT r.public_name) FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.enabled=1 AND p.enabled=1`).Scan(&m)
	a.reader().QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked=0 AND (expires_at IS NULL OR expires_at='' OR expires_at>?) AND (budget_micros=0 OR spent_micros<budget_micros)`, now()).Scan(&k)
	a.reader().QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&total)
	a.reader().QueryRow(`SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cached_tokens),0),COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(cost_micros),0) FROM request_ledger WHERE completed_at IS NOT NULL`).Scan(&input, &output, &cached, &reasoning, &costMicros)
	a.reader().QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=?`, time.Now().UTC().Truncate(24*time.Hour).Format(time.RFC3339)).Scan(&today)
	a.reader().QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=? AND completed_at IS NOT NULL AND success=0`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)).Scan(&failures)
	writeJSON(w, 200, map[string]any{"providers": p, "models": m, "keys": k, "requests": total, "today_requests": today, "failures_24h": failures, "input_tokens": input, "output_tokens": output, "cached_tokens": cached, "reasoning_tokens": reasoning, "total_tokens": input + output, "cost_micros": costMicros})
}

func (a *App) globalRoutingStrategy() RoutingStrategy {
	var value string
	if err := a.db.QueryRow(`SELECT value FROM settings WHERE key='routing_strategy'`).Scan(&value); err == nil && validRoutingStrategy(value) {
		return RoutingStrategy(value)
	}
	return StrategyPriorityFailover
}

func (a *App) routing(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"strategy": string(a.globalRoutingStrategy())})
	case http.MethodPatch:
		var in struct {
			Strategy string `json:"strategy"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if !validRoutingStrategy(in.Strategy) {
			fail(w, http.StatusBadRequest, "invalid_strategy", "strategy must be priority_failover, ordered_round_robin, smart_round_robin, or adaptive")
			return
		}
		if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('routing_strategy',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, in.Strategy); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		// Both rotating strategies keep per-model state, so clear it on any change
		// instead of only when switching to smart round robin.
		a.routeMu.Lock()
		a.resetRouteCursorsLocked()
		a.routeMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"strategy": in.Strategy})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or PATCH required")
	}
}
func (a *App) requests(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if x, e := strconv.Atoi(s); e == nil && x > 0 && x <= 200 {
			limit = x
		}
	}
	where := []string{"1=1"}
	args := []any{}
	for _, filter := range []struct {
		name, operator string
	}{
		{"from", ">="},
		{"to", "<="},
	} {
		value := strings.TrimSpace(r.URL.Query().Get(filter.name))
		if value == "" {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC3339, value)
		if parseErr != nil {
			fail(w, http.StatusBadRequest, "invalid_time_filter", filter.name+" must be an RFC3339 timestamp")
			return
		}
		where = append(where, "l.created_at "+filter.operator+" ?")
		args = append(args, parsed.UTC().Format(time.RFC3339Nano))
	}
	if providerID := strings.TrimSpace(r.URL.Query().Get("provider_id")); providerID != "" {
		id, parseErr := strconv.ParseInt(providerID, 10, 64)
		if parseErr != nil || id < 1 {
			fail(w, http.StatusBadRequest, "invalid_provider_filter", "provider_id must be a positive integer")
			return
		}
		where = append(where, "l.provider_id=?")
		args = append(args, id)
	}
	switch status := strings.TrimSpace(r.URL.Query().Get("status")); status {
	case "", "all":
	case "running":
		where = append(where, "l.completed_at IS NULL")
	case "success":
		where = append(where, "l.completed_at IS NOT NULL AND l.success=1")
	case "failed":
		where = append(where, "l.completed_at IS NOT NULL AND l.success=0")
	default:
		fail(w, http.StatusBadRequest, "invalid_status_filter", "status must be all, running, success, or failed")
		return
	}
	if query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))); query != "" {
		like := "%" + query + "%"
		where = append(where, `(LOWER(l.public_model) LIKE ? OR LOWER(l.upstream_model) LIKE ? OR LOWER(l.protocol) LIKE ? OR LOWER(l.request_id) LIKE ? OR LOWER(l.gateway_request_id) LIKE ? OR LOWER(l.client_ip) LIKE ? OR LOWER(COALESCE(NULLIF(l.provider_name,''),p.name,'')) LIKE ? OR LOWER(l.error_type) LIKE ? OR LOWER(l.retry_reason) LIKE ?)`)
		for range 9 {
			args = append(args, like)
		}
	}
	args = append(args, limit)
	query := `SELECT l.id,l.request_id,l.gateway_request_id,l.attempt,l.retry_reason,l.created_at,COALESCE(l.completed_at,''),l.first_byte_ms,l.public_model,l.upstream_model,l.protocol,l.stream,l.success,l.status_code,l.error_type,l.latency_ms,l.input_tokens,l.output_tokens,l.cached_tokens,l.reasoning_tokens,l.cost_micros,l.cost_type,l.usage_reported,COALESCE(NULLIF(l.provider_name,''),p.name,''),l.client_ip FROM request_ledger l LEFT JOIN providers p ON p.id=l.provider_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY l.id DESC LIMIT ?`
	rows, err := a.reader().Query(query, args...)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, attempt, stream, success, status, latency, usageReported int
		var rid, gatewayID, retryReason, created, completed, pm, um, proto, et, ct, providerName, clientIP string
		var firstByte sql.NullInt64
		var input, output, cached, reasoning, cost int64
		if err := rows.Scan(&id, &rid, &gatewayID, &attempt, &retryReason, &created, &completed, &firstByte, &pm, &um, &proto, &stream, &success, &status, &et, &latency, &input, &output, &cached, &reasoning, &cost, &ct, &usageReported, &providerName, &clientIP); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		var firstByteMS any
		if firstByte.Valid {
			firstByteMS = firstByte.Int64
		}
		out = append(out, map[string]any{"id": id, "request_id": rid, "gateway_request_id": gatewayID, "attempt": attempt, "retry_reason": retryReason, "provider_name": providerName, "client_ip": clientIP, "created_at": created, "completed_at": completed, "running": completed == "", "first_byte_ms": firstByteMS, "model": pm, "upstream_model": um, "protocol": proto, "stream": strBool(stream), "success": strBool(success), "status_code": status, "error_type": et, "latency_ms": latency, "input_tokens": input, "output_tokens": output, "cached_tokens": cached, "reasoning_tokens": reasoning, "total_tokens": input + output, "cost_micros": cost, "cost_type": ct, "usage_reported": strBool(usageReported)})
	}
	writeJSON(w, 200, out)
}

func validateUpstream(raw string, cfg Config) error {
	u, e := urlParse(raw)
	if e != nil {
		return e
	}
	if u.Scheme != "https" && !cfg.AllowInsecureUpstreams {
		return fmt.Errorf("only HTTPS upstream URLs are allowed; set FUSIONGATE_ALLOW_INSECURE_UPSTREAMS=true only for a trusted development network")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("upstream hostname is required")
	}
	if cfg.AllowPrivateUpstreams {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("localhost is blocked by SSRF protection")
	}
	if ip, e := netip.ParseAddr(host); e == nil && isPrivate(ip) {
		return fmt.Errorf("private, loopback, link-local, and unspecified addresses are blocked by SSRF protection")
	}
	return nil
}
func urlParse(v string) (*url.URL, error) {
	u, e := url.Parse(v)
	if e != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid absolute upstream URL")
	}
	if u.User != nil {
		return nil, fmt.Errorf("upstream URL must not contain user credentials")
	}
	return u, nil
}
func isPrivate(ip netip.Addr) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()
}

// providerGroups handles GET and POST for provider groups
func (a *App) providerGroups(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`
			SELECT g.id, g.name, g.collapsed, g.sort_order, g.created_at, g.updated_at,
			       COUNT(p.id),
			       SUM(CASE WHEN p.enabled=1 AND p.health_check_status='healthy' THEN 1 ELSE 0 END)
			FROM provider_groups g
			LEFT JOIN providers p ON p.group_id=g.id
			GROUP BY g.id
			ORDER BY g.sort_order, g.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []ProviderGroup{}
		for rows.Next() {
			var g ProviderGroup
			var collapsed int
			var memberCount, healthyCount sql.NullInt64
			if err := rows.Scan(&g.ID, &g.Name, &collapsed, &g.SortOrder, &g.CreatedAt, &g.UpdatedAt, &memberCount, &healthyCount); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			g.Collapsed = strBool(collapsed)
			g.MemberCount = int(memberCount.Int64)
			g.HealthyCount = int(healthyCount.Int64)
			out = append(out, g)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Name      string `json:"name"`
			SortOrder *int   `json:"sort_order"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" {
			fail(w, http.StatusBadRequest, "invalid_request", "group name is required")
			return
		}
		sortOrder := 0
		if in.SortOrder != nil {
			sortOrder = *in.SortOrder
		}
		res, err := a.db.Exec(`INSERT INTO provider_groups(name, collapsed, sort_order, created_at, updated_at) VALUES(?,0,?,?,?)`,
			in.Name, sortOrder, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "group_conflict", "a group with that name already exists")
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": in.Name})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

// providerGroupByID handles PATCH and DELETE on /api/admin/provider-groups/{id}
func (a *App) providerGroupByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	idText := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/provider-groups/"), "/")
	if !isID(idText) {
		fail(w, http.StatusNotFound, "not_found", "group not found")
		return
	}
	id, _ := strconv.ParseInt(idText, 10, 64)

	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Name      *string `json:"name"`
			Collapsed *bool   `json:"collapsed"`
			SortOrder *int    `json:"sort_order"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.Name != nil {
			v := strings.TrimSpace(*in.Name)
			if v == "" {
				fail(w, http.StatusBadRequest, "invalid_request", "group name cannot be empty")
				return
			}
			in.Name = &v
		}
		res, err := a.db.Exec(`UPDATE provider_groups SET name=COALESCE(?,name), collapsed=COALESCE(?,collapsed), sort_order=COALESCE(?,sort_order), updated_at=? WHERE id=?`,
			in.Name, maybeBool(in.Collapsed), in.SortOrder, now(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		// Un-assign providers in this group before deleting
		_, _ = a.db.Exec(`UPDATE providers SET group_id=NULL WHERE group_id=?`, id)
		res, err := a.db.Exec(`DELETE FROM provider_groups WHERE id=?`, id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}
