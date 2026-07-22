package fusiongate

import (
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

func (a *App) sign(v string) string {
	h := hmac.New(sha256.New, []byte(a.cfg.MasterKey))
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (a *App) setAdminCookies(w http.ResponseWriter, r *http.Request) string {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	exp := time.Now().Add(12 * time.Hour)
	payload := strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: "fg_admin", Value: payload + "." + a.sign(payload), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure, Expires: exp})
	csrf := hex.EncodeToString(randomBytes(24))
	http.SetCookie(w, &http.Cookie{Name: "fg_csrf", Value: csrf, Path: "/", HttpOnly: false, SameSite: http.SameSiteStrictMode, Secure: secure, Expires: exp})
	return csrf
}
func (a *App) adminAuth(r *http.Request) (adminCtx, bool) {
	c, e := r.Cookie("fg_admin")
	if e != nil {
		return adminCtx{}, false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 2 || !hmac.Equal([]byte(a.sign(p[0])), []byte(p[1])) {
		return adminCtx{}, false
	}
	u, e := strconv.ParseInt(p[0], 10, 64)
	if e != nil || time.Now().After(time.Unix(u, 0)) {
		return adminCtx{}, false
	}
	csrf, _ := r.Cookie("fg_csrf")
	return adminCtx{CSRF: func() string {
		if csrf == nil {
			return ""
		}
		return csrf.Value
	}()}, true
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
	var h string
	if err := a.db.QueryRow(`SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&h); err != nil || !checkPassword(in.Password, h) {
		time.Sleep(400 * time.Millisecond)
		fail(w, 401, "invalid_credentials", "invalid credentials")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "csrf_token": a.setAdminCookies(w, r)})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed", "POST required")
		return
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
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	if err := a.db.PingContext(r.Context()); err != nil {
		fail(w, 503, "database_unavailable", "database unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "service": "fusiongate", "time": now()})
}

func validProviderType(t string) bool {
	switch t {
	case "openai", "openrouter", "openai_compatible", "anthropic", "gemini", "codex_oauth", "claude_oauth", "gemini_cli":
		return true
	}
	return false
}
func (a *App) providers(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`SELECT p.id,p.name,p.type,p.base_url,p.enabled,p.priority,p.weight,p.status,p.notes,p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,''),(SELECT COUNT(*) FROM model_routes r WHERE r.provider_id=p.id) FROM providers p ORDER BY p.priority DESC,p.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []Provider{}
		for rows.Next() {
			var p Provider
			var enabled int
			if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &enabled, &p.Priority, &p.Weight, &p.Status, &p.Notes, &p.PassthroughMode, &p.ClientPolicy, &p.MaxConcurrency, &p.RequestTimeoutMS, &p.FailureThreshold, &p.CooldownSeconds, &p.ConsecutiveFailures, &p.CircuitOpenUntil, &p.LastError, &p.LastLatencyMS, &p.LastSuccessAt, &p.LastFailureAt, &p.ModelCount); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			p.Enabled = strBool(enabled)
			p.CredentialHint = "configured"
			p.Inflight = a.providerInflight(p.ID)
			out = append(out, p)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Name             string `json:"name"`
			Type             string `json:"type"`
			BaseURL          string `json:"baseURL"`
			Credential       string `json:"credential"`
			Notes            string `json:"notes"`
			Enabled          *bool  `json:"enabled"`
			Priority         *int   `json:"priority"`
			Weight           int    `json:"weight"`
			PassthroughMode  string `json:"passthrough_mode"`
			ClientPolicy     string `json:"client_policy"`
			MaxConcurrency   int    `json:"max_concurrency"`
			RequestTimeoutMS int    `json:"request_timeout_ms"`
			FailureThreshold int    `json:"failure_threshold"`
			CooldownSeconds  int    `json:"cooldown_seconds"`
			AutoDiscover     *bool  `json:"auto_discover"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		in.Type = strings.TrimSpace(in.Type)
		in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
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
		if priority < 0 || in.Weight < 1 || in.MaxConcurrency < 0 || in.RequestTimeoutMS < 1000 || in.FailureThreshold < 1 || in.CooldownSeconds < 1 || !validPassthroughMode(in.PassthroughMode) || !validClientPolicy(in.ClientPolicy) {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid priority, weight, forwarding mode, client policy, concurrency, timeout, failure threshold, or cooldown")
			return
		}
		encrypted, err := a.encrypt(in.Credential)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_error", err.Error())
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.Name, in.Type, in.BaseURL, encrypted, boolInt(enabled), priority, in.Weight, "unknown", in.Notes, in.PassthroughMode, in.ClientPolicy, in.MaxConcurrency, in.RequestTimeoutMS, in.FailureThreshold, in.CooldownSeconds, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "provider_conflict", err.Error())
			return
		}
		id, _ := res.LastInsertId()
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

func (a *App) providerByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/providers/"), "/")
	parts := strings.Split(suffix, "/")
	idText := parts[0]
	if !isID(idText) {
		fail(w, http.StatusNotFound, "not_found", "provider not found")
		return
	}
	id, _ := strconv.ParseInt(idText, 10, 64)
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
	case http.MethodPatch:
		var in struct {
			Enabled          *bool   `json:"enabled"`
			Priority         *int    `json:"priority"`
			Weight           *int    `json:"weight"`
			Notes            *string `json:"notes"`
			PassthroughMode  *string `json:"passthrough_mode"`
			ClientPolicy     *string `json:"client_policy"`
			MaxConcurrency   *int    `json:"max_concurrency"`
			RequestTimeoutMS *int    `json:"request_timeout_ms"`
			FailureThreshold *int    `json:"failure_threshold"`
			CooldownSeconds  *int    `json:"cooldown_seconds"`
			ResetHealth      bool    `json:"reset_health"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if (in.Priority != nil && *in.Priority < 0) || (in.Weight != nil && *in.Weight < 1) || (in.MaxConcurrency != nil && *in.MaxConcurrency < 0) || (in.RequestTimeoutMS != nil && *in.RequestTimeoutMS < 1000) || (in.FailureThreshold != nil && *in.FailureThreshold < 1) || (in.CooldownSeconds != nil && *in.CooldownSeconds < 1) || (in.PassthroughMode != nil && !validPassthroughMode(*in.PassthroughMode)) || (in.ClientPolicy != nil && !validClientPolicy(*in.ClientPolicy)) {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid provider scheduling or forwarding configuration")
			return
		}
		// Re-enabling a channel starts a fresh health window. This matters most
		// for channels automatically closed after five consecutive failures.
		resetOnEnable := false
		if in.Enabled != nil && *in.Enabled {
			var enabled int
			if err := a.db.QueryRow(`SELECT enabled FROM providers WHERE id=?`, id).Scan(&enabled); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					fail(w, http.StatusNotFound, "not_found", "provider not found")
					return
				}
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			resetOnEnable = !strBool(enabled)
		}
		res, err := a.db.Exec(`UPDATE providers SET enabled=COALESCE(?,enabled),priority=COALESCE(?,priority),weight=COALESCE(?,weight),notes=COALESCE(?,notes),passthrough_mode=COALESCE(?,passthrough_mode),client_policy=COALESCE(?,client_policy),max_concurrency=COALESCE(?,max_concurrency),request_timeout_ms=COALESCE(?,request_timeout_ms),failure_threshold=COALESCE(?,failure_threshold),cooldown_seconds=COALESCE(?,cooldown_seconds),updated_at=? WHERE id=?`, maybeBool(in.Enabled), in.Priority, in.Weight, in.Notes, in.PassthroughMode, in.ClientPolicy, in.MaxConcurrency, in.RequestTimeoutMS, in.FailureThreshold, in.CooldownSeconds, now(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		if in.ResetHealth || resetOnEnable {
			_, err = a.db.Exec(`UPDATE providers SET status='unknown',consecutive_failures=0,circuit_open_until=NULL,last_error='',last_failure_at=NULL,updated_at=? WHERE id=?`, now(), id)
			if err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			a.resetProviderRuntime(id)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}

func maybeBool(v *bool) any {
	if v == nil {
		return nil
	}
	return boolInt(*v)
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
       r.input_price_micros,r.output_price_micros,p.name,p.type,
       p.enabled,p.status,p.last_latency_ms,p.consecutive_failures
FROM model_routes r
JOIN providers p ON p.id=r.provider_id
ORDER BY r.public_name,p.priority DESC,p.id,r.id`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []Route{}
		for rows.Next() {
			var x Route
			var en, providerEnabled int
			if err := rows.Scan(&x.ID, &x.ProviderID, &x.PublicName, &x.UpstreamModel, &x.Capabilities, &en, &x.Priority, &x.SortOrder, &x.InputPriceMicros, &x.OutputPriceMicros, &x.ProviderName, &x.ProviderType, &providerEnabled, &x.ProviderStatus, &x.ProviderLatencyMS, &x.ProviderFailures); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			x.Enabled = strBool(en)
			x.ProviderEnabled = strBool(providerEnabled)
			x.ProviderInflight = a.providerInflight(x.ProviderID)
			if x.ProviderEnabled {
				x.HealthScore = routeHealthScore(x.ProviderStatus, x.ProviderLatencyMS, x.ProviderFailures, x.ProviderInflight)
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
		if _, err := tx.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO NOTHING`, in.PublicName, StrategyPriorityFailover, now()); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
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
			Enabled  *bool `json:"enabled"`
			Priority *int  `json:"priority"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.Enabled == nil && in.Priority == nil {
			fail(w, http.StatusBadRequest, "invalid_request", "enabled or priority is required")
			return
		}
		if in.Priority != nil && *in.Priority < 0 {
			fail(w, http.StatusBadRequest, "invalid_priority", "priority must be zero or greater")
			return
		}
		res, err := a.db.Exec(`UPDATE model_routes SET enabled=COALESCE(?,enabled),priority=COALESCE(?,priority),updated_at=? WHERE id=?`, maybeBool(in.Enabled), in.Priority, now(), id)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		var publicName string
		if err := a.db.QueryRow(`SELECT public_name FROM model_routes WHERE id=?`, id).Scan(&publicName); err != nil {
			if err == sql.ErrNoRows {
				fail(w, http.StatusNotFound, "not_found", "route not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		if _, err := a.db.Exec(`DELETE FROM model_routes WHERE id=?`, id); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		_, _ = a.db.Exec(`DELETE FROM route_policies WHERE public_name=? AND NOT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, publicName, publicName)
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
	delete(a.roundRobinCursor, in.PublicName)
	a.routeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) routePolicies(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT or PATCH required")
		return
	}
	var in struct {
		PublicName string `json:"public_name"`
		Strategy   string `json:"strategy"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	in.PublicName = strings.ToLower(strings.TrimSpace(in.PublicName))
	if in.PublicName == "" || !validRoutingStrategy(in.Strategy) {
		fail(w, http.StatusBadRequest, "invalid_strategy", "strategy must be priority_failover, ordered_round_robin, or adaptive")
		return
	}
	var exists int
	if err := a.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, in.PublicName).Scan(&exists); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if exists == 0 {
		fail(w, http.StatusNotFound, "not_found", "public model not found")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?) ON CONFLICT(public_name) DO UPDATE SET strategy=excluded.strategy,updated_at=excluded.updated_at`, in.PublicName, in.Strategy, now()); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if in.Strategy == string(StrategyOrderedRoundRobin) {
		a.routeMu.Lock()
		delete(a.roundRobinCursor, in.PublicName)
		a.routeMu.Unlock()
	}
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
		rows, err := a.db.Query(`SELECT id,name,key_prefix,allow_all,allow_models,deny_models,allow_images,rpm_limit,revoked,COALESCE(expires_at,''),created_at,encrypted_key IS NOT NULL FROM api_keys ORDER BY id DESC`)
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
			if err := rows.Scan(&x.ID, &x.Name, &x.Prefix, &aa, &x.AllowModels, &x.DenyModels, &ai, &x.RPMLimit, &rv, &ex, &cr, &canReveal); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			x.AllowAll = strBool(aa)
			x.AllowImages = strBool(ai)
			x.Revoked = strBool(rv)
			x.CanReveal = strBool(canReveal)
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
			Name        string `json:"name"`
			AllowModels string `json:"allow_models"`
			DenyModels  string `json:"deny_models"`
			AllowAll    bool   `json:"allow_all"`
			AllowImages bool   `json:"allow_images"`
			RPMLimit    int    `json:"rpm_limit"`
			ExpiresAt   string `json:"expires_at"`
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
		res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,expires_at,created_at,encrypted_key) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, in.Name, prefix, hex.EncodeToString(sum[:]), boolInt(in.AllowAll), in.AllowModels, in.DenyModels, boolInt(in.AllowImages), in.RPMLimit, exp, now(), encrypted)
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
	if _, err := tx.Exec(`UPDATE request_ledger SET api_key_id=NULL WHERE api_key_id=?`, parts[0]); err != nil {
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

func (a *App) dashboard(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var p, m, k, total, today, failures int
	var input, output, cached, reasoning int64
	a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE enabled=1`).Scan(&p)
	a.db.QueryRow(`SELECT COUNT(DISTINCT r.public_name) FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.enabled=1 AND p.enabled=1`).Scan(&m)
	a.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked=0`).Scan(&k)
	a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger`).Scan(&total)
	a.db.QueryRow(`SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cached_tokens),0),COALESCE(SUM(reasoning_tokens),0) FROM request_ledger WHERE completed_at IS NOT NULL`).Scan(&input, &output, &cached, &reasoning)
	a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=?`, time.Now().UTC().Truncate(24*time.Hour).Format(time.RFC3339)).Scan(&today)
	a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=? AND completed_at IS NOT NULL AND success=0`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)).Scan(&failures)
	writeJSON(w, 200, map[string]any{"providers": p, "models": m, "keys": k, "requests": total, "today_requests": today, "failures_24h": failures, "input_tokens": input, "output_tokens": output, "cached_tokens": cached, "reasoning_tokens": reasoning, "total_tokens": input + output})
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
			fail(w, http.StatusBadRequest, "invalid_strategy", "strategy must be priority_failover, ordered_round_robin, or adaptive")
			return
		}
		if _, err := a.db.Exec(`INSERT INTO settings(key,value) VALUES('routing_strategy',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, in.Strategy); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if in.Strategy == string(StrategyOrderedRoundRobin) {
			a.routeMu.Lock()
			a.roundRobinCursor = map[string]int{}
			a.routeMu.Unlock()
		}
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
	rows, err := a.db.Query(`SELECT l.id,l.request_id,l.gateway_request_id,l.attempt,l.retry_reason,l.created_at,COALESCE(l.completed_at,''),l.first_byte_ms,l.public_model,l.upstream_model,l.protocol,l.stream,l.success,l.status_code,l.error_type,l.latency_ms,l.input_tokens,l.output_tokens,l.cached_tokens,l.reasoning_tokens,l.cost_micros,l.cost_type,COALESCE(p.name,'') FROM request_ledger l LEFT JOIN providers p ON p.id=l.provider_id ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, attempt, stream, success, status, latency int
		var rid, gatewayID, retryReason, created, completed, pm, um, proto, et, ct, providerName string
		var firstByte sql.NullInt64
		var input, output, cached, reasoning, cost int64
		if err := rows.Scan(&id, &rid, &gatewayID, &attempt, &retryReason, &created, &completed, &firstByte, &pm, &um, &proto, &stream, &success, &status, &et, &latency, &input, &output, &cached, &reasoning, &cost, &ct, &providerName); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		var firstByteMS any
		if firstByte.Valid {
			firstByteMS = firstByte.Int64
		}
		out = append(out, map[string]any{"id": id, "request_id": rid, "gateway_request_id": gatewayID, "attempt": attempt, "retry_reason": retryReason, "provider_name": providerName, "created_at": created, "completed_at": completed, "running": completed == "", "first_byte_ms": firstByteMS, "model": pm, "upstream_model": um, "protocol": proto, "stream": strBool(stream), "success": strBool(success), "status_code": status, "error_type": et, "latency_ms": latency, "input_tokens": input, "output_tokens": output, "cached_tokens": cached, "reasoning_tokens": reasoning, "total_tokens": input + output, "cost_micros": cost, "cost_type": ct})
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
