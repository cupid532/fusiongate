package fusiongate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
		rows, err := a.db.Query(`SELECT p.id,p.name,p.type,p.base_url,p.enabled,p.priority,p.weight,p.status,p.notes,p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,''),(SELECT COUNT(*) FROM model_routes r WHERE r.provider_id=p.id) FROM providers p ORDER BY p.priority,p.id`)
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
			Priority         int    `json:"priority"`
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
		if in.Priority == 0 {
			in.Priority = 100
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
		if in.Weight < 1 || in.MaxConcurrency < 0 || in.RequestTimeoutMS < 1000 || in.FailureThreshold < 1 || in.CooldownSeconds < 1 || !validPassthroughMode(in.PassthroughMode) || !validClientPolicy(in.ClientPolicy) {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid weight, forwarding mode, client policy, concurrency, timeout, failure threshold, or cooldown")
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
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.Name, in.Type, in.BaseURL, encrypted, boolInt(enabled), in.Priority, in.Weight, "unknown", in.Notes, in.PassthroughMode, in.ClientPolicy, in.MaxConcurrency, in.RequestTimeoutMS, in.FailureThreshold, in.CooldownSeconds, now(), now())
		if err != nil {
			fail(w, http.StatusConflict, "provider_conflict", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		response := map[string]any{"id": id, "message": "provider created; credential is encrypted at rest"}
		if in.AutoDiscover == nil || *in.AutoDiscover {
			discovery, discoveryErr := a.discoverAndImportModels(r.Context(), id)
			if discoveryErr != nil {
				response["model_discovery"] = map[string]any{"status": "failed", "error": discoveryErr.Error()}
			} else {
				response["model_discovery"] = map[string]any{"status": "ok", "discovered": discovery.Discovered, "added": discovery.Added, "existing": discovery.Existing, "skipped": discovery.Skipped}
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
		result, err := a.discoverAndImportModels(r.Context(), id)
		if err != nil {
			fail(w, discoveryErrorStatus(err), "model_discovery_failed", err.Error())
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
		if (in.Weight != nil && *in.Weight < 1) || (in.MaxConcurrency != nil && *in.MaxConcurrency < 0) || (in.RequestTimeoutMS != nil && *in.RequestTimeoutMS < 1000) || (in.FailureThreshold != nil && *in.FailureThreshold < 1) || (in.CooldownSeconds != nil && *in.CooldownSeconds < 1) || (in.PassthroughMode != nil && !validPassthroughMode(*in.PassthroughMode)) || (in.ClientPolicy != nil && !validClientPolicy(*in.ClientPolicy)) {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid provider scheduling or forwarding configuration")
			return
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
		if in.ResetHealth {
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

func (a *App) routes(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case "GET":
		rows, err := a.db.Query(`SELECT r.id,r.provider_id,r.public_name,r.upstream_model,r.capabilities,r.enabled,r.priority,r.input_price_micros,r.output_price_micros,p.name,p.type FROM model_routes r JOIN providers p ON p.id=r.provider_id ORDER BY r.public_name,r.priority`)
		if err != nil {
			fail(w, 500, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []Route{}
		for rows.Next() {
			var x Route
			var en int
			if err := rows.Scan(&x.ID, &x.ProviderID, &x.PublicName, &x.UpstreamModel, &x.Capabilities, &en, &x.Priority, &x.InputPriceMicros, &x.OutputPriceMicros, &x.ProviderName, &x.ProviderType); err != nil {
				fail(w, 500, "database_error", err.Error())
				return
			}
			x.Enabled = strBool(en)
			out = append(out, x)
		}
		writeJSON(w, 200, out)
	case "POST":
		var in struct {
			ProviderID        int64  `json:"provider_id"`
			PublicName        string `json:"public_name"`
			UpstreamModel     string `json:"upstream_model"`
			Capabilities      string `json:"capabilities"`
			Enabled           *bool  `json:"enabled"`
			Priority          int    `json:"priority"`
			InputPriceMicros  int64  `json:"input_price_micros"`
			OutputPriceMicros int64  `json:"output_price_micros"`
		}

		if err := readJSON(r, &in); err != nil {
			fail(w, 400, "invalid_request", err.Error())
			return
		}
		in.PublicName = strings.TrimSpace(in.PublicName)
		in.UpstreamModel = strings.TrimSpace(in.UpstreamModel)
		if in.ProviderID < 1 || in.PublicName == "" || in.UpstreamModel == "" {
			fail(w, 400, "invalid_request", "provider_id, public_name, and upstream_model are required")
			return
		}
		if in.Capabilities == "" {
			in.Capabilities = "chat,stream"
		}
		if in.Priority == 0 {
			in.Priority = 100
		}
		en := true
		if in.Enabled != nil {
			en = *in.Enabled
		}
		res, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,input_price_micros,output_price_micros,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, in.PublicName, in.ProviderID, in.UpstreamModel, in.Capabilities, boolInt(en), in.Priority, in.InputPriceMicros, in.OutputPriceMicros, now(), now())
		if err != nil {
			fail(w, 409, "route_conflict", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, 201, map[string]any{"id": id})
	default:
		fail(w, 405, "method_not_allowed", "GET or POST required")
	}
}
func (a *App) routeByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/routes/")
	if r.Method != "DELETE" || !isID(id) {
		fail(w, 405, "method_not_allowed", "DELETE /api/admin/routes/{id} required")
		return
	}
	res, err := a.db.Exec(`DELETE FROM model_routes WHERE id=?`, id)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 404, "not_found", "route not found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) keys(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case "GET":
		rows, err := a.db.Query(`SELECT id,name,key_prefix,allow_all,allow_models,deny_models,allow_images,rpm_limit,revoked,COALESCE(expires_at,''),created_at FROM api_keys ORDER BY id DESC`)
		if err != nil {
			fail(w, 500, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []APIKey{}
		for rows.Next() {
			var x APIKey
			var aa, ai, rv int
			var ex, cr string
			if err := rows.Scan(&x.ID, &x.Name, &x.Prefix, &aa, &x.AllowModels, &x.DenyModels, &ai, &x.RPMLimit, &rv, &ex, &cr); err != nil {
				fail(w, 500, "database_error", err.Error())
				return
			}
			x.AllowAll = strBool(aa)
			x.AllowImages = strBool(ai)
			x.Revoked = strBool(rv)
			x.ExpiresAt = parseTime(ex)
			x.CreatedAt = *parseTime(cr)
			out = append(out, x)
		}
		writeJSON(w, 200, out)
	case "POST":
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
			fail(w, 400, "invalid_request", err.Error())
			return
		}
		if strings.TrimSpace(in.Name) == "" {
			fail(w, 400, "invalid_request", "name is required")
			return
		}
		if in.RPMLimit <= 0 {
			in.RPMLimit = 120
		}
		raw := "fg_" + hex.EncodeToString(randomBytes(24))
		sum := sha256.Sum256([]byte(raw))
		prefix := raw[:11]
		var exp any = nil
		if in.ExpiresAt != "" {
			if _, e := time.Parse(time.RFC3339, in.ExpiresAt); e != nil {
				fail(w, 400, "invalid_request", "expires_at must be RFC3339")
				return
			}
			exp = in.ExpiresAt
		}
		res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, in.Name, prefix, hex.EncodeToString(sum[:]), boolInt(in.AllowAll), in.AllowModels, in.DenyModels, boolInt(in.AllowImages), in.RPMLimit, exp, now())
		if err != nil {
			fail(w, 500, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, 201, map[string]any{"id": id, "key": raw, "message": "Copy this key now. It is stored only as a hash."})
	default:
		fail(w, 405, "method_not_allowed", "GET or POST required")
	}
}
func (a *App) keyByID(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/keys/")
	if r.Method != "DELETE" || !isID(id) {
		fail(w, 405, "method_not_allowed", "DELETE /api/admin/keys/{id} required")
		return
	}
	res, err := a.db.Exec(`UPDATE api_keys SET revoked=1 WHERE id=?`, id)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 404, "not_found", "key not found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) dashboard(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	var p, m, k, total, today, failures int
	var cost int64
	a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE enabled=1`).Scan(&p)
	a.db.QueryRow(`SELECT COUNT(DISTINCT public_name) FROM model_routes WHERE enabled=1`).Scan(&m)
	a.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE revoked=0`).Scan(&k)
	a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(cost_micros),0) FROM request_ledger`).Scan(&total, &cost)
	a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=?`, time.Now().UTC().Truncate(24*time.Hour).Format(time.RFC3339)).Scan(&today)
	a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE created_at>=? AND success=0`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)).Scan(&failures)
	writeJSON(w, 200, map[string]any{"providers": p, "models": m, "keys": k, "requests": total, "today_requests": today, "failures_24h": failures, "cost_micros": cost})
}
func (a *App) requests(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if x, e := strconv.Atoi(s); e == nil && x > 0 && x <= 200 {
			limit = x
		}
	}
	rows, err := a.db.Query(`SELECT l.id,l.request_id,l.gateway_request_id,l.attempt,l.retry_reason,l.created_at,l.public_model,l.upstream_model,l.protocol,l.stream,l.success,l.status_code,l.error_type,l.latency_ms,l.input_tokens,l.output_tokens,l.cost_micros,l.cost_type,COALESCE(p.name,'') FROM request_ledger l LEFT JOIN providers p ON p.id=l.provider_id ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, attempt, stream, success, status, latency int
		var rid, gatewayID, retryReason, created, pm, um, proto, et, ct, providerName string
		var input, output, cost int64
		if err := rows.Scan(&id, &rid, &gatewayID, &attempt, &retryReason, &created, &pm, &um, &proto, &stream, &success, &status, &et, &latency, &input, &output, &cost, &ct, &providerName); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "request_id": rid, "gateway_request_id": gatewayID, "attempt": attempt, "retry_reason": retryReason, "provider_name": providerName, "created_at": created, "model": pm, "upstream_model": um, "protocol": proto, "stream": strBool(stream), "success": strBool(success), "status_code": status, "error_type": et, "latency_ms": latency, "input_tokens": input, "output_tokens": output, "cost_micros": cost, "cost_type": ct})
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
