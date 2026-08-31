package fusiongate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	errNoEligibleRoute = errors.New("no eligible route")
	errRouteResolution = errors.New("model route resolution failed")
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
	AllowAudio   bool
	RPMLimit     int
	Revoked      bool
	ExpiresAt    *time.Time
	BudgetMicros int64
	SpentMicros  int64
	QualityRoute *qualityDetectorRouteSession
}

type resolvedRoute struct {
	Route          Route
	Provider       Provider
	CanonicalModel string
	ProviderKeyID  int64
	AttemptID      int64
	Credential     string
	AuthCredential *ProviderCredential
}

func (a *App) api(fn func(http.ResponseWriter, *http.Request, authKey)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setGatewayCORS(w, r, a.cfg.CORSOrigins)
		if anthropicEndpoint(r) {
			w.Header().Set("request-id", requestID())
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		k, ok := a.authenticateKey(r)
		if !ok {
			failRequest(w, r, http.StatusUnauthorized, "invalid_api_key", "missing, expired, revoked, or invalid API key")
			return
		}
		if !a.allowRate(k) {
			failRequest(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "API key rate limit exceeded")
			return
		}
		// A budget is an accounting limit, never a concurrency limit. Requests are
		// admitted whenever the budget still has headroom, no matter how many are
		// already in flight for this key. Cost is only known once the upstream
		// reports usage, so requests already running when the budget is reached
		// still settle and may take the total slightly past the limit.
		if k.BudgetMicros > 0 {
			spent, err := a.keySpentMicros(k.ID)
			if err != nil {
				failRequest(w, r, http.StatusInternalServerError, "database_error", "cannot evaluate this key's budget")
				return
			}
			if spent >= k.BudgetMicros {
				failRequest(w, r, http.StatusPaymentRequired, "budget_exceeded", "API key budget exhausted")
				return
			}
		}
		if !a.tryAcquireRequestSlot() {
			a.metrics.overloaded.Add(1)
			w.Header().Set("Retry-After", "1")
			failRequest(w, r, http.StatusServiceUnavailable, "gateway_overloaded", "gateway concurrency limit reached")
			return
		}
		defer a.releaseRequestSlot()
		a.metrics.requests.Add(1)
		a.metrics.active.Add(1)
		defer a.metrics.active.Add(-1)
		fn(w, r, k)
	}
}

// keySpentMicros reads the running total maintained on the key. Summing the ledger
// instead would scan every row that key ever produced, on every request, and would
// also refund budget once retention pruned rows older than a year.
func (a *App) keySpentMicros(id int64) (int64, error) {
	var spent sql.NullInt64
	if err := a.reader().QueryRow(`SELECT spent_micros FROM api_keys WHERE id=?`, id).Scan(&spent); err != nil {
		return 0, err
	}
	return spent.Int64, nil
}

func setGatewayCORS(w http.ResponseWriter, r *http.Request, allowlist string) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || !corsOriginAllowed(origin, allowlist) {
		return
	}
	h := w.Header()
	if strings.TrimSpace(allowlist) == "" || strings.TrimSpace(allowlist) == "*" {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	requestedHeaders := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
	if requestedHeaders == "" {
		requestedHeaders = "Authorization, Content-Type, X-API-Key"
	}
	h.Set("Access-Control-Allow-Headers", requestedHeaders)
	h.Set("Access-Control-Expose-Headers", "Content-Type, Retry-After, Request-Id, X-FusionGate-Request-ID, X-FusionGate-Upstream-Protocol")
	h.Set("Access-Control-Max-Age", "86400")
	h.Add("Vary", "Origin")
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Access-Control-Request-Private-Network")), "true") {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}
}

func corsOriginAllowed(origin, allowlist string) bool {
	allowlist = strings.TrimSpace(allowlist)
	if allowlist == "" || allowlist == "*" {
		return true
	}
	for _, candidate := range strings.Split(allowlist, ",") {
		if strings.EqualFold(strings.TrimSpace(candidate), origin) {
			return true
		}
	}
	return false
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
	if key, ok := a.authenticateQualityDetectorRoute(r, raw); ok {
		return key, true
	}
	sum := sha256.Sum256([]byte(raw))
	var x authKey
	var allowAll, allowImages, allowAudio, revoked int
	var expiresAt, createdAt string
	err := a.reader().QueryRow(`SELECT id,name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,allow_audio,rpm_limit,revoked,COALESCE(expires_at,''),created_at,budget_micros FROM api_keys WHERE key_hash=?`, hex.EncodeToString(sum[:])).Scan(&x.ID, &x.Name, &x.Prefix, &x.Hash, &allowAll, &x.AllowModels, &x.DenyModels, &allowImages, &allowAudio, &x.RPMLimit, &revoked, &expiresAt, &createdAt, &x.BudgetMicros)
	if err != nil {
		return authKey{}, false
	}
	x.AllowAll = strBool(allowAll)
	x.AllowImages = strBool(allowImages)
	x.AllowAudio = strBool(allowAudio)
	x.Revoked = strBool(revoked)
	x.ExpiresAt = parseTime(expiresAt)
	if x.Revoked || (x.ExpiresAt != nil && time.Now().After(*x.ExpiresAt)) {
		return authKey{}, false
	}
	// Only budgeted keys need the ledger aggregate, which is the most expensive part
	// of authentication on a single-connection SQLite database.
	if x.BudgetMicros > 0 {
		spent, spentErr := a.keySpentMicros(x.ID)
		if spentErr != nil {
			return authKey{}, false
		}
		x.SpentMicros = spent
	}
	a.markAPIKeyUsed(x.ID)
	return x, true
}

func restrictQualityDetectorRoutes(key authKey, routes []resolvedRoute) []resolvedRoute {
	if key.QualityRoute == nil {
		return routes
	}
	target := key.QualityRoute.Target
	filtered := routes[:0]
	for _, route := range routes {
		if route.Route.ID != target.RouteID || route.Provider.ID != target.ProviderID || route.ProviderKeyID != target.ProviderKeyID {
			continue
		}
		filtered = append(filtered, route)
	}
	return filtered
}

func (a *App) markAPIKeyUsed(id int64) {
	current := time.Now()
	a.lastUsedMu.Lock()
	last := a.lastUsedAt[id]
	if !last.IsZero() && current.Sub(last) < 30*time.Second {
		a.lastUsedMu.Unlock()
		return
	}
	a.lastUsedAt[id] = current
	a.lastUsedMu.Unlock()
	_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, current.UTC().Format(time.RFC3339Nano), id)
}

// allowRate applies the key's per-minute limit as a sliding window. A fixed window
// would let a caller send the full limit at the end of one window and again at the
// start of the next, delivering twice the configured rate across the boundary.
func (a *App) allowRate(k authKey) bool {
	if k.RPMLimit <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := time.Now()
	window := a.rate[k.Hash]
	if window == nil {
		a.rate[k.Hash] = &rateWindow{At: current, Count: 1}
		return true
	}
	elapsed := current.Sub(window.At)
	switch {
	case elapsed >= 2*time.Minute:
		window.At, window.Count, window.Prev = current, 0, 0
		elapsed = 0
	case elapsed >= time.Minute:
		window.At = window.At.Add(time.Minute)
		window.Prev, window.Count = window.Count, 0
		elapsed = current.Sub(window.At)
	}
	// Weight the previous window by the part of it that still falls inside the
	// trailing minute.
	overlap := 1 - elapsed.Seconds()/60
	if overlap < 0 {
		overlap = 0
	}
	if float64(window.Prev)*overlap+float64(window.Count)+1 > float64(k.RPMLimit) {
		return false
	}
	window.Count++
	return true
}

func allowed(k authKey, model string) bool {
	if matches(k.DenyModels, model) {
		return false
	}
	return k.AllowAll || matches(k.AllowModels, model)
}

// nextListItem splits a comma-separated list in place. Permission checks run on every
// request, so they should not allocate a slice just to look at each entry.
func nextListItem(list string) (item, rest string) {
	if index := strings.IndexByte(list, ','); index >= 0 {
		return strings.TrimSpace(list[:index]), list[index+1:]
	}
	return strings.TrimSpace(list), ""
}

func matches(patterns, model string) bool {
	for patterns != "" {
		var p string
		p, patterns = nextListItem(patterns)
		if p == "" {
			continue
		}
		if p == model {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, p[:len(p)-1]) {
			return true
		}
	}
	return false
}

const (
	protocolAuto      = "auto"
	protocolFixed     = "fixed"
	protocolResponses = "responses"
	protocolMessages  = "messages"
	protocolChat      = "chat"
)

func validProtocolPolicy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case protocolAuto, protocolFixed:
		return true
	default:
		return false
	}
}

func normalizeProtocolPreference(value string) (string, bool) {
	seen := map[string]bool{}
	ordered := make([]string, 0, 3)
	for value != "" {
		var item string
		item, value = nextListItem(value)
		item = strings.ToLower(strings.TrimSpace(item))
		switch item {
		case protocolResponses, protocolMessages, protocolChat:
			if !seen[item] {
				seen[item] = true
				ordered = append(ordered, item)
			}
		case "":
		default:
			return "", false
		}
	}
	return strings.Join(ordered, ","), true
}

func routeProtocolEnabled(z resolvedRoute, protocol string) bool {
	policy := strings.ToLower(strings.TrimSpace(z.Provider.ProtocolPolicy))
	preference, valid := normalizeProtocolPreference(z.Provider.ProtocolPreference)
	if !valid {
		return false
	}
	if policy == protocolFixed {
		first, _ := nextListItem(preference)
		return strings.EqualFold(first, protocol)
	}
	// Auto mode is capability-driven. Preference orders protocols already
	// known to work; it never invents support that discovery did not prove.
	return matchesCapability(z.Route.Capabilities, "protocol:"+protocol)
}

func matchesCapability(capabilities, required string) bool {
	if required == "" {
		return true
	}
	for capabilities != "" {
		var capability string
		capability, capabilities = nextListItem(capabilities)
		if strings.EqualFold(capability, required) {
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
	rows, err := a.reader().Query(`SELECT r.public_name,MIN(r.created_at),GROUP_CONCAT(r.capabilities,'|'),GROUP_CONCAT(p.type,'|'),GROUP_CONCAT(r.upstream_model,'|') FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.enabled=1 AND p.enabled=1 AND p.archived=0 GROUP BY r.public_name ORDER BY r.public_name`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var name, created, routeCapabilities, providerTypes, upstreamModels string
		if rows.Scan(&name, &created, &routeCapabilities, &providerTypes, &upstreamModels) == nil && allowed(k, name) {
			createdAt := parseTime(created)
			var unix int64
			if createdAt != nil {
				unix = createdAt.Unix()
			}
			metadata := modelMetadata(name, routeCapabilities, providerTypes, upstreamModels)
			metadata["object"] = "model"
			metadata["created"] = unix
			metadata["owned_by"] = "fusiongate"
			data = append(data, metadata)
		}
	}
	aliasRows, err := a.reader().Query(`
SELECT a.alias,a.target_model,a.created_at,GROUP_CONCAT(r.capabilities,'|'),GROUP_CONCAT(p.type,'|'),GROUP_CONCAT(r.upstream_model,'|')
FROM model_aliases a
JOIN model_routes r ON r.public_name=a.target_model AND r.enabled=1
JOIN providers p ON p.id=r.provider_id AND p.enabled=1 AND p.archived=0 AND p.passthrough_mode<>'transparent'
WHERE a.enabled=1
GROUP BY a.alias,a.target_model,a.created_at`)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	defer aliasRows.Close()
	for aliasRows.Next() {
		var alias, target, created, routeCapabilities, providerTypes, upstreamModels string
		if aliasRows.Scan(&alias, &target, &created, &routeCapabilities, &providerTypes, &upstreamModels) != nil || !modelAllowed(k, alias, target) {
			continue
		}
		createdAt := parseTime(created)
		var unix int64
		if createdAt != nil {
			unix = createdAt.Unix()
		}
		metadata := modelMetadata(alias, routeCapabilities, providerTypes, upstreamModels)
		metadata["object"] = "model"
		metadata["created"] = unix
		metadata["owned_by"] = "fusiongate"
		metadata["canonical_model"] = target
		data = append(data, metadata)
	}
	if err := aliasRows.Err(); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	sort.Slice(data, func(i, j int) bool { return asString(data[i]["id"]) < asString(data[j]["id"]) })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

var reasoningEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

func modelMetadata(name, routeCapabilities, providerTypes, upstreamModels string) map[string]any {
	metadata := map[string]any{"id": name}
	capabilities := map[string]bool{}
	reasoningEfforts := map[string]bool{}
	defaultReasoningEffort := ""
	for _, group := range strings.Split(routeCapabilities, "|") {
		for _, capability := range strings.Split(group, ",") {
			if capability = strings.TrimSpace(capability); capability != "" {
				capabilities[capability] = true
				if strings.HasPrefix(capability, "reasoning:") {
					reasoningEfforts[strings.TrimPrefix(capability, "reasoning:")] = true
				}
				if strings.HasPrefix(capability, "reasoning_default:") && defaultReasoningEffort == "" {
					defaultReasoningEffort = strings.TrimPrefix(capability, "reasoning_default:")
				}
			}
		}
	}
	chat := capabilities["chat"]
	imageInput := capabilities["image_input"] || capabilities["vision"]
	reasoning := capabilities["reasoning"]
	for _, providerType := range strings.Split(providerTypes, "|") {
		if providerType == "codex_oauth" {
			reasoning = true
			imageInput = true
			if len(reasoningEfforts) == 0 {
				for _, effort := range []string{"low", "medium", "high", "xhigh"} {
					reasoningEfforts[effort] = true
				}
			}
		}
	}
	for _, model := range append([]string{name}, strings.Split(upstreamModels, "|")...) {
		lower := strings.ToLower(model)
		if strings.Contains(lower, "vision") || strings.Contains(lower, "gpt-4o") || strings.Contains(lower, "gpt-5") || strings.Contains(lower, "claude-3") || strings.Contains(lower, "claude-4") || strings.Contains(lower, "gemini-") || strings.Contains(lower, "grok-4") {
			imageInput = true
		}
		if strings.Contains(lower, "gpt-5") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4") || strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking") || strings.Contains(lower, "grok-4") {
			reasoning = true
		}
	}
	if reasoning && len(reasoningEfforts) == 0 {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			reasoningEfforts[effort] = true
		}
	}
	if chat {
		metadata["input_modalities"] = []string{"text"}
		metadata["output_modalities"] = []string{"text"}
	}
	if imageInput {
		metadata["input_modalities"] = []string{"text", "image"}
	}
	if reasoning || len(reasoningEfforts) > 0 {
		metadata["reasoning"] = true
		ordered := make([]string, 0, len(reasoningEfforts))
		for _, effort := range reasoningEffortOrder {
			if reasoningEfforts[effort] {
				ordered = append(ordered, effort)
				delete(reasoningEfforts, effort)
			}
		}
		customEfforts := make([]string, 0, len(reasoningEfforts))
		for effort := range reasoningEfforts {
			customEfforts = append(customEfforts, effort)
		}
		sort.Strings(customEfforts)
		ordered = append(ordered, customEfforts...)
		if len(ordered) > 0 {
			metadata["supported_reasoning_efforts"] = ordered
		}
		if defaultReasoningEffort != "" {
			metadata["default_reasoning_effort"] = defaultReasoningEffort
		}
	}
	if capabilities["image"] {
		metadata["output_modalities"] = []string{"image"}
	}
	return metadata
}

func (a *App) resolve(ctx context.Context, model, requiredCapability string) ([]resolvedRoute, error) {
	rows, err := a.reader().QueryContext(ctx, `
SELECT r.id,r.provider_id,r.public_name,r.upstream_model,r.capabilities,r.enabled,r.priority,r.sort_order,
       r.input_price_micros,r.cached_price_micros,r.output_price_micros,r.long_context_threshold,
       r.long_input_price_micros,r.long_cached_price_micros,r.long_output_price_micros,
	       p.id,p.name,p.type,p.base_url,p.credential,p.auth_kind,p.enabled,p.priority,p.sort_order,p.weight,p.status,p.notes,
       p.passthrough_mode,p.client_policy,p.max_concurrency,p.request_timeout_ms,p.failure_threshold,p.cooldown_seconds,
       p.consecutive_failures,COALESCE(p.circuit_open_until,''),p.last_error,p.last_latency_ms,p.last_first_byte_ms,
       COALESCE(p.last_success_at,''),COALESCE(p.last_failure_at,''),p.ip_pool_node_id,p.multi_key_initialized,p.default_model,
       p.protocol_policy,p.protocol_preference,COALESCE(p.health_check_status,''),COALESCE(p.health_check_error,'')
FROM model_routes r JOIN providers p ON p.id=r.provider_id
WHERE r.enabled=1 AND p.enabled=1 AND p.archived=0
  AND (LOWER(r.public_name)=LOWER(?) OR LOWER(r.upstream_model) IN (
    SELECT DISTINCT LOWER(seed.upstream_model) FROM model_routes seed
    WHERE seed.enabled=1 AND LOWER(seed.public_name)=LOWER(?)
  ))
ORDER BY CASE WHEN LOWER(r.public_name)=LOWER(?) THEN 0 ELSE 1 END,r.sort_order,r.id`, model, model, model)
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
	pendingIndex := map[string]int{}
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
			&z.Provider.ProtocolPolicy, &z.Provider.ProtocolPreference, &z.Provider.HealthCheckStatus, &z.Provider.HealthCheckError,
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
		// One provider gets one seat per final upstream model. A provider may expose
		// both the requested public name and the upstream model's native public name;
		// counting both would silently double its round-robin weight. The SQL orders
		// direct public-name routes first, so the first entry is authoritative.
		poolKey := strconv.FormatInt(z.Provider.ID, 10) + "\x00" + strings.ToLower(strings.TrimSpace(z.Route.UpstreamModel))
		if _, exists := pendingIndex[poolKey]; exists {
			continue
		}
		pendingIndex[poolKey] = len(pending)
		z.Route.PublicName = model
		z.CanonicalModel = model
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

	selectionRequests := make([]providerKeySelectionRequest, 0, len(pending))
	for _, candidate := range pending {
		if candidate.authKind != "api_key" {
			continue
		}
		z := candidate.resolved
		selectionRequests = append(selectionRequests, providerKeySelectionRequest{
			providerID:          z.Provider.ID,
			upstreamModel:       z.Route.UpstreamModel,
			providerNodeID:      z.Provider.IPPoolNodeID,
			legacyCredential:    candidate.credential,
			multiKeyInitialized: candidate.multiKeyInitialized,
		})
	}
	selectedByCandidate, selectionErr := a.selectProviderKeysBatch(ctx, selectionRequests)
	if selectionErr != nil {
		a.log.Error("provider key selection", "error", sanitizeError(selectionErr.Error()))
		return nil, errRouteResolution
	}
	var firstOperationalErr error

	out := make([]resolvedRoute, 0, len(pending))
	for _, candidate := range pending {
		z := candidate.resolved
		if candidate.authKind == "api_key" {
			selected := selectedByCandidate[makeProviderKeySelectionKey(z.Provider.ID, z.Route.UpstreamModel)]
			if errors.Is(selected.err, errNoProviderKeySupportsModel) {
				continue
			}
			if selected.err != nil {
				if firstOperationalErr == nil {
					firstOperationalErr = selected.err
				}
				if !candidate.multiKeyInitialized {
					a.log.Error("provider credential decrypt", "provider_id", z.Provider.ID, "error", sanitizeError(selected.err.Error()))
				}
				continue
			}
			for _, providerKey := range selected.keys {
				keyRoute := z
				keyRoute.ProviderKeyID = providerKey.ID
				keyRoute.Credential = providerKey.Credential
				keyRoute.Provider.IPPoolNodeID = providerKey.IPPoolNodeID
				out = append(out, keyRoute)
			}
			continue
		}
		plaintext, decryptErr := a.decrypt(candidate.credential)
		if decryptErr != nil {
			if firstOperationalErr == nil {
				firstOperationalErr = decryptErr
			}
			// Credential faults are an operator problem; the API caller only learns
			// that route resolution failed, not which provider or why.
			a.log.Error("provider credential decrypt", "provider_id", z.Provider.ID, "provider", z.Provider.Name, "error", sanitizeError(decryptErr.Error()))
			continue
		}
		authCredential, accessToken, decodeErr := decodeStoredCredential(candidate.authKind, plaintext)
		if decodeErr != nil {
			if firstOperationalErr == nil {
				firstOperationalErr = decodeErr
			}
			a.log.Error("provider credential decode", "provider_id", z.Provider.ID, "provider", z.Provider.Name, "error", sanitizeError(decodeErr.Error()))
			continue
		}
		z.Credential = accessToken
		z.AuthCredential = &authCredential
		out = append(out, z)
	}
	if len(out) == 0 {
		if firstOperationalErr != nil {
			return nil, errRouteResolution
		}
		return nil, fmt.Errorf("%w for model %q", errNoEligibleRoute, model)
	}
	return out, nil
}

func requestID() string { return "req_" + hex.EncodeToString(randomBytes(12)) }

// requestClientIP resolves the caller's address, and is only willing to believe
// X-Forwarded-For when the immediate peer is a local reverse proxy.
//
// Each proxy appends the peer it saw, so the chain reads oldest-first and its tail is
// written by infrastructure we control. Scanning from the right and taking the first
// public address therefore skips both the trusted local hops at the end and any
// addresses a client injected at the front: to poison the result a caller would have
// to make the proxy observe it at a public address other than its own.
func requestClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if parsed, err := netip.ParseAddrPort(host); err == nil {
		host = parsed.Addr().String()
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !(addr.IsLoopback() || addr.IsPrivate()) {
		return host
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	for end := len(forwarded); end > 0; {
		start := strings.LastIndexByte(forwarded[:end], ',') + 1
		candidate := strings.TrimSpace(forwarded[start:end])
		if parsed, parseErr := netip.ParseAddr(candidate); parseErr == nil &&
			!parsed.IsLoopback() && !parsed.IsPrivate() && !parsed.IsLinkLocalUnicast() {
			return parsed.String()
		}
		end = start - 1
	}
	return host
}

// requestReasoningEffort reads the reasoning effort a client asked for, accepting both
// the flat OpenAI Chat form (`reasoning_effort`) and the nested Responses form
// (`reasoning.effort`). It is recorded on the ledger so the console can show the
// intensity next to the model. The value is clamped to a short known vocabulary so a
// hostile client cannot store arbitrary text through it.
func requestReasoningEffort(body map[string]any) string {
	raw := ""
	if v, ok := body["reasoning_effort"].(string); ok {
		raw = v
	} else if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if v, ok := reasoning["effort"].(string); ok {
			raw = v
		}
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return raw
	}
	return ""
}

// startLedger records the beginning of one upstream attempt and returns the
// attempt's stable request_id. The write is queued, so the caller never waits for
// SQLite before dispatching the upstream request.
func (a *App) startLedger(k authKey, z resolvedRoute, protocol string, stream bool, clientIP, gatewayID, reasoningEffort string, attempt int, retryReason string) string {
	attemptID := gatewayID + "_a" + strconv.Itoa(attempt)
	a.queueLedgerWrite(`INSERT INTO request_ledger(request_id,gateway_request_id,attempt,retry_reason,created_at,api_key_id,provider_id,route_id,public_model,upstream_model,protocol,stream,client_ip,api_key_name,api_key_prefix,provider_name,reasoning_effort) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, gatewayID, attempt, retryReason, now(), k.ID, z.Provider.ID, z.Route.ID, z.Route.PublicName, z.Route.UpstreamModel, protocol, boolInt(stream), clientIP, k.Name, k.Prefix, z.Provider.Name, reasoningEffort)
	return attemptID
}

// recordFirstByte is called from inside the response read path, so it must not
// block on the database: a synchronous write here would delay the first byte the
// client sees by however long SQLite takes to accept it.
func (a *App) recordFirstByte(attemptID string, start time.Time) {
	if attemptID == "" {
		return
	}
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	a.queueLedgerWrite(`UPDATE request_ledger SET first_byte_ms=? WHERE request_id=? AND first_byte_ms IS NULL`, elapsed, attemptID)
}

func (a *App) endLedger(attemptID string, providerID, apiKeyID int64, providerType, upstreamModel string, success bool, status int, errorType string, start time.Time, usage Usage) {
	if attemptID == "" {
		return
	}
	if apiKeyID > 0 && usage.CostMicros > 0 {
		a.queueLedgerWrite(`UPDATE api_keys SET spent_micros=spent_micros+? WHERE id=?`, usage.CostMicros, apiKeyID)
	}
	if providerID > 0 {
		a.queueCostCycleWrite(providerID, providerType, upstreamModel, usage)
	}
	a.queueLedgerWrite(`UPDATE request_ledger SET completed_at=?,success=?,status_code=?,error_type=?,latency_ms=?,input_tokens=?,output_tokens=?,cached_tokens=?,reasoning_tokens=?,cost_micros=?,cost_type=?,usage_reported=? WHERE request_id=?`, now(), boolInt(success), status, errorType, time.Since(start).Milliseconds(), usage.Input, usage.Output, usage.Cached, usage.Reasoning, usage.CostMicros, usage.CostType, boolInt(usage.Reported), attemptID)
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
	const limit = 32 << 20
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

func (a *App) runRoutes(w http.ResponseWriter, r *http.Request, key authKey, routes []resolvedRoute, protocol, reasoningEffort string, stream bool, execute routeExecutor) {
	finalStatus := 0
	finalSuccess := false
	defer func() {
		if finalStatus == 0 {
			return
		}
		a.metrics.completed.Add(1)
		if finalSuccess {
			a.metrics.successes.Add(1)
		} else {
			a.metrics.failures.Add(1)
		}
	}()
	if key.QualityRoute == nil {
		routes = filterClientRoutes(routes, r)
	}
	if len(routes) == 0 {
		finalStatus = http.StatusForbidden
		failRequest(w, r, http.StatusForbidden, "provider_client_policy_mismatch", "no provider accepts this request's real User-Agent")
		return
	}
	strategy := a.globalRoutingStrategy()
	if key.QualityRoute != nil {
		strategy = StrategyPriorityFailover
	} else {
		routes = a.prepareRoutes(routes, strategy)
	}
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
		// Only an explicitly configured positive cap ends the loop early;
		// the default (<= 0) tries every candidate in the plan before failing.
		if a.cfg.MaxFailoverAttempts > 0 && attempt > a.cfg.MaxFailoverAttempts {
			status := lastStatus
			if status < http.StatusBadRequest {
				status = http.StatusBadGateway
			}
			w.Header().Set("X-FusionGate-Attempts", strconv.Itoa(attempt-1))
			finalStatus = status
			failRequest(w, r, status, "upstream_attempt_limit", "maximum upstream failover attempts reached")
			return
		}
		var z resolvedRoute
		var availability routeAvailability
		var ok bool
		if key.QualityRoute != nil && len(tried) == 0 {
			z, availability, ok = a.acquireQualityDetectorRoute(routes[0])
		} else {
			z, availability, ok = a.acquireRoute(routes, tried, strategy)
		}
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
			finalStatus = status
			failRequest(w, r, status, "upstream_unavailable", message)
			return
		}
		tried[z.AttemptID] = true
		a.metrics.attempts.Add(1)
		if attempt > 1 {
			a.metrics.failovers.Add(1)
		}
		started := time.Now()
		attemptID := a.startLedger(key, z, protocol, stream, clientIP, gatewayID, reasoningEffort, attempt, previousReason)
		var observedFirstByteMS atomic.Int64
		result := execute(z, attemptID, func() {
			elapsed := time.Since(started).Milliseconds()
			if elapsed < 1 {
				elapsed = 1
			}
			observedFirstByteMS.CompareAndSwap(0, elapsed)
			a.metrics.firstByteCount.Add(1)
			a.metrics.firstByteMillis.Add(elapsed)
			a.recordFirstByte(attemptID, started)
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
		a.endLedger(attemptID, z.Provider.ID, key.ID, z.Provider.Type, z.Route.UpstreamModel, result.Handled && status < 400 && result.Err == nil, status, reason, started, result.Usage)
		if result.Handled {
			finalStatus = status
			finalSuccess = status < 400 && result.Err == nil
			return
		}
		if !result.Retryable {
			finalStatus = status
			failRequest(w, r, status, reason, "upstream request failed and is not safe to retry")
			return
		}
		if result.RetryAfter > retryAfter {
			retryAfter = result.RetryAfter
		}
		previousReason = reason
	}
}

func (a *App) openAIProxy(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, requestID, endpoint string, stream, safeTransportRetry bool, onFirstByte func()) attemptResult {
	return a.openAIProxyWithRetryStatus(w, r, raw, z, requestID, endpoint, stream, safeTransportRetry, onFirstByte, nil)
}

func (a *App) openAIProxyWithRetryStatus(w http.ResponseWriter, r *http.Request, raw []byte, z resolvedRoute, requestID, endpoint string, stream, safeTransportRetry bool, onFirstByte func(), retryStatus func(int) bool) attemptResult {
	transparent := z.Provider.PassthroughMode == "transparent"
	body := raw
	var err error
	upstreamSSE := false
	bufferSSE := false
	var sseTransform func([]byte) ([]byte, string, Usage, error)
	if !transparent {
		if z.Provider.Type == "codex_oauth" && endpoint == "/v1/responses" {
			body, err = normalizedCodexResponsesBody(raw, z.Route.UpstreamModel)
			upstreamSSE = true
			bufferSSE = !stream
			sseTransform = completedResponsesSSEForModel(z.Route.PublicName)
		} else {
			upstreamStream := stream || endpoint == "/v1/chat/completions" || endpoint == "/v1/responses"
			body, err = normalizedOpenAIBody(raw, z.Route.UpstreamModel, upstreamStream, z.Provider.Type != "codex_oauth")
			upstreamSSE = upstreamStream
			bufferSSE = upstreamStream && !stream
			if endpoint == "/v1/responses" {
				sseTransform = completedResponsesSSEForModel(z.Route.PublicName)
			} else if endpoint == "/v1/chat/completions" {
				sseTransform = completedChatCompletionForModel(z.Route.PublicName)
			}
			if err == nil && (z.Provider.Type == "openai_compatible" || z.Provider.Type == "opencode") && endpoint == "/v1/chat/completions" {
				body, err = normalizedCompatibleChatBody(body)
			}
		}
		if err != nil {
			return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
		}
	}
	var streamTransform func([]byte) ([]byte, error)
	if endpoint == "/v1/responses" && stream && !transparent {
		streamTransform = responsesSSEWithPublicModel(z.Route.PublicName)
	} else if endpoint == "/v1/chat/completions" && stream && !transparent {
		streamTransform = chatSSEWithPublicModel(z.Route.PublicName)
	}
	var jsonTransform func([]byte) ([]byte, string, error)
	if endpoint == "/v1/responses" && !stream && !transparent {
		jsonTransform = func(body []byte) ([]byte, string, error) {
			return responsesJSONWithPublicModel(body, z.Route.PublicName)
		}
	} else if endpoint == "/v1/chat/completions" && !stream && !transparent {
		jsonTransform = func(body []byte) ([]byte, string, error) {
			return chatJSONWithPublicModel(body, z.Route.PublicName)
		}
	}
	return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: endpoint, RawBody: body, Stream: stream, Transparent: transparent, UsageFormat: "openai", GatewayID: requestID, SafeTransportRetry: safeTransportRetry, OnFirstByte: onFirstByte, UpstreamSSE: upstreamSSE, BufferSSE: bufferSSE, SSETransform: sseTransform, JSONTransform: jsonTransform, StreamTransform: streamTransform, RetryStatus: retryStatus})
}

func (a *App) chat(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	canonical, err := a.canonicalModel(r.Context(), model)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "could not resolve model alias")
		return
	}
	if !modelAllowed(key, model, canonical) {
		fail(w, http.StatusForbidden, "model_not_allowed", "this API key is not allowed to use this model")
		return
	}
	stream, _ := body["stream"].(bool)
	routes, err := a.resolve(r.Context(), canonical, "chat")
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	routes = restrictQualityDetectorRoutes(key, routes)
	routes = exposeRequestedModel(routes, model)
	if len(routes) == 0 {
		fail(w, http.StatusNotFound, "model_not_found", "the selected quality detector route is unavailable")
		return
	}
	a.runRoutes(w, r, key, routes, "openai_chat", requestReasoningEffort(body), stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		switch z.Provider.Type {
		case "openai", "grok", "openrouter", "openai_compatible", "grok_oauth":
			return a.openAIProxy(w, r, raw, z, rid, "/v1/chat/completions", stream, true, onFirstByte)
		case "opencode":
			switch opencodeRouteProtocol(z) {
			case opencodeProtocolResponses:
				encoded, err := codexResponsesBodyFromChat(raw, z.Route.UpstreamModel)
				if err != nil {
					return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
				}
				return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/responses", RawBody: encoded, Stream: stream, UsageFormat: "openai", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferSSE: true, SSETransform: func(body []byte) ([]byte, string, Usage, error) {
					completed, usage, err := completedResponseFromSSE(body)
					if err != nil {
						return nil, "", usage, err
					}
					transformed, contentType, err := codexChatResponse(completed, stream, z.Route.PublicName)
					return transformed, contentType, usage, err
				}})
			case opencodeProtocolAnthropic:
				if stream || z.Provider.PassthroughMode == "transparent" {
					return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
				}
				return a.chatAnthropic(w, r, body, z, rid, onFirstByte)
			case opencodeProtocolGemini:
				if stream || z.Provider.PassthroughMode == "transparent" {
					return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
				}
				return a.chatGemini(w, r, body, z, rid, onFirstByte)
			default:
				return a.openAIProxy(w, r, raw, z, rid, "/v1/chat/completions", stream, true, onFirstByte)
			}
		case "codex_oauth":
			encoded, err := codexResponsesBodyFromChat(raw, z.Route.UpstreamModel)
			if err != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
			}
			return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/responses", RawBody: encoded, Stream: stream, UsageFormat: "openai", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferSSE: true, SSETransform: func(body []byte) ([]byte, string, Usage, error) {
				completed, usage, err := completedResponseFromSSE(body)
				if err != nil {
					return nil, "", usage, err
				}
				transformed, contentType, err := codexChatResponse(completed, stream, z.Route.PublicName)
				return transformed, contentType, usage, err
			}})
		case "anthropic":
			if routeProtocolEnabled(z, protocolResponses) {
				encoded, err := codexResponsesBodyFromChat(raw, z.Route.UpstreamModel)
				if err != nil {
					return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
				}
				w.Header().Set("X-FusionGate-Upstream-Protocol", "responses")
				return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/responses", RawBody: encoded, Stream: stream, UsageFormat: "openai", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: true, BufferSSE: true, SSETransform: func(body []byte) ([]byte, string, Usage, error) {
					completed, usage, err := completedResponseFromSSE(body)
					if err != nil {
						return nil, "", usage, err
					}
					transformed, contentType, err := codexChatResponse(completed, stream, z.Route.PublicName)
					return transformed, contentType, usage, err
				}})
			}
			if stream || z.Provider.PassthroughMode == "transparent" {
				return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
			}
			return a.chatAnthropic(w, r, body, z, rid, onFirstByte)
		case "claude_oauth":
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

// sendNonStreamingUpstream performs one non-streaming upstream request and applies the
// failover and error-forwarding rules shared by the protocol conversions: transport
// errors and retryable statuses stay retryable, a client error is forwarded verbatim
// and marked handled, and a body that will not decode is retryable. On success it
// returns the decoded upstream payload.
func (a *App) sendNonStreamingUpstream(w http.ResponseWriter, r *http.Request, z resolvedRoute, req *http.Request, onFirstByte func()) (map[string]any, attemptResult, bool) {
	resp, err := a.doProviderRequest(req, z.Provider.IPPoolNodeID)
	if err != nil {
		if downstreamCanceled(r) {
			return nil, attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}, false
		}
		return nil, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, err), Err: err}, false
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, onFirstByte)
	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return nil, attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}, false
	}
	if resp.StatusCode >= 400 {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		reason := retryReason(resp.StatusCode, nil)
		if copyErr != nil {
			reason = "downstream_write_error"
		}
		return nil, attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: copyErr}, false
	}
	var source map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&source); err != nil {
		return nil, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_invalid_response", Err: err}, false
	}
	return source, attemptResult{}, true
}

// writeChatCompletion renders a converted upstream answer as an OpenAI chat completion
// and settles its estimated cost.
func writeChatCompletion(w http.ResponseWriter, z resolvedRoute, rid, content string, finishReason any, usage Usage) attemptResult {
	cost(z, &usage)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl-" + rid, "object": "chat.completion", "created": time.Now().Unix(), "model": z.Route.PublicName,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finishReason}},
		"usage":   map[string]any{"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": usage.Input + usage.Output},
	})
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: usage}
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
	setOpenCodeAnthropicHeaders(req.Header)
	req.Header.Set("content-type", "application/json")
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	source, result, ok := a.sendNonStreamingUpstream(w, r, z, req, onFirstByte)
	if !ok {
		return result
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
	return writeChatCompletion(w, z, rid, content, source["stop_reason"], parseAnthropicUsage(source))
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
	if z.Provider.Type == "opencode" {
		endpoint = "/v1/models/" + url.PathEscape(z.Route.UpstreamModel) + ":generateContent"
	}
	upstreamURL, _ := joinURLQuery(z.Provider.BaseURL, endpoint, "")
	ctx, cancel := providerContext(r.Context(), z.Provider)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
	copyUpstreamRequestHeaders(req.Header, r.Header)
	req.Header.Set("content-type", "application/json")
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	source, result, ok := a.sendNonStreamingUpstream(w, r, z, req, onFirstByte)
	if !ok {
		return result
	}
	content := ""
	if candidates, ok := source["candidates"].([]any); ok && len(candidates) > 0 {
		candidate, _ := candidates[0].(map[string]any)
		candidateContent, _ := candidate["content"].(map[string]any)
		parts, _ := candidateContent["parts"].([]any)
		for _, part := range parts {
			part, _ := part.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				content += text
			}
		}
	}
	return writeChatCompletion(w, z, rid, content, "stop", parseGeminiUsage(source))
}

func (a *App) responses(w http.ResponseWriter, r *http.Request, key authKey) {
	a.openAIEndpoint(w, r, key, "openai_responses", "/v1/responses", "chat", true)
}

func (a *App) responsesCompact(w http.ResponseWriter, r *http.Request, key authKey) {
	a.openAIEndpoint(w, r, key, "openai_responses_compact", "/v1/responses/compact", "chat", true)
}

func (a *App) openAIEndpoint(w http.ResponseWriter, r *http.Request, key authKey, protocol, endpoint, capability string, safeTransportRetry bool) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	canonical, err := a.canonicalModel(r.Context(), model)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "could not resolve model alias")
		return
	}
	if !modelAllowed(key, model, canonical) {
		fail(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), canonical, capability)
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	routes = restrictQualityDetectorRoutes(key, routes)
	routes = exposeRequestedModel(routes, model)
	if len(routes) == 0 {
		fail(w, http.StatusNotFound, "model_not_found", "the selected quality detector route is unavailable")
		return
	}
	compatible := routes[:0]
	for _, z := range routes {
		eligible := z.Provider.Type == "openai" || z.Provider.Type == "grok" || z.Provider.Type == "openrouter" || z.Provider.Type == "openai_compatible" || z.Provider.Type == "codex_oauth" || z.Provider.Type == "grok_oauth"
		if z.Provider.Type == "anthropic" {
			// Some Anthropic-compatible aggregators expose a native OpenAI
			// Responses endpoint alongside Messages. Opt in per route so a
			// regular Anthropic provider is never probed with the wrong API.
			eligible = protocol == "openai_responses" && routeProtocolEnabled(z, protocolResponses)
		}
		if z.Provider.Type == "opencode" {
			eligible = protocol == "openai_responses" && opencodeRouteProtocol(z) == opencodeProtocolResponses
		}
		if eligible {
			compatible = append(compatible, z)
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no OpenAI-compatible route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, protocol, requestReasoningEffort(body), stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		if protocol == "openai_images" && z.Provider.Type == "codex_oauth" {
			return a.codexImageProxy(w, r, raw, z, rid, onFirstByte)
		}
		if protocol == "openai_responses" && z.Provider.Type == "openai_compatible" && z.Provider.PassthroughMode != "transparent" {
			return a.responsesFirstCompatibleProxy(w, r, raw, z, rid, stream, safeTransportRetry, onFirstByte)
		}
		if protocol == "openai_responses" && z.Provider.Type == "anthropic" && routeProtocolEnabled(z, protocolResponses) {
			// Keep the provider typed as Anthropic for /v1/messages while using
			// its explicitly declared native Responses endpoint for this route.
			w.Header().Set("X-FusionGate-Upstream-Protocol", "responses")
			result := a.openAIProxyWithRetryStatus(w, r, raw, z, rid, endpoint, stream, safeTransportRetry, onFirstByte, responsesProtocolFallbackStatus)
			if result.Reason == "upstream_protocol_unsupported" {
				// Discovery is stale: remove only the learned capability. This
				// prevents future requests from repeatedly probing a dead endpoint.
				_, _ = a.db.ExecContext(r.Context(), `UPDATE model_routes SET capabilities=TRIM(REPLACE(','||capabilities||',', ',protocol:responses,', ','), ','),updated_at=? WHERE id=?`, now(), z.Route.ID)
			}
			return result
		}
		if protocol == "openai_responses_compact" {
			if z.Provider.Type == "opencode" {
				return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "protocol_not_supported"}
			}
			encoded := raw
			if z.Provider.PassthroughMode != "transparent" {
				var encodeErr error
				encoded, encodeErr = normalizedOpenAIBody(raw, z.Route.UpstreamModel, false, false)
				if encodeErr != nil {
					return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: encodeErr}
				}
			}
			return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: endpoint, RawBody: encoded, Transparent: z.Provider.PassthroughMode == "transparent", GatewayID: rid, SafeTransportRetry: safeTransportRetry, OnFirstByte: onFirstByte, JSONOutputValid: func(body []byte) bool {
				var payload map[string]any
				return json.Unmarshal(body, &payload) == nil && payload["output"] != nil
			}})
		}
		return a.openAIProxy(w, r, raw, z, rid, endpoint, stream, safeTransportRetry, onFirstByte)
	})
}

func (a *App) messages(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		failRequest(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		failRequest(w, r, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	canonical, err := a.canonicalModel(r.Context(), model)
	if err != nil {
		failRequest(w, r, http.StatusInternalServerError, "database_error", "could not resolve model alias")
		return
	}
	if !modelAllowed(key, model, canonical) {
		failRequest(w, r, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), canonical, "chat")
	if err != nil {
		failRequest(w, r, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	routes = restrictQualityDetectorRoutes(key, routes)
	routes = exposeRequestedModel(routes, model)
	if len(routes) == 0 {
		failRequest(w, r, http.StatusNotFound, "model_not_found", "the selected quality detector route is unavailable")
		return
	}
	compatible := routes[:0]
	for _, z := range routes {
		switch z.Provider.Type {
		case "anthropic", "claude_oauth":
			compatible = append(compatible, z)
		case "opencode":
			if opencodeRouteProtocol(z) == opencodeProtocolAnthropic || opencodeRouteProtocol(z) == opencodeProtocolChat && z.Provider.PassthroughMode != "transparent" {
				compatible = append(compatible, z)
			}
		case "openai", "grok", "openrouter", "openai_compatible", "grok_oauth":
			if z.Provider.PassthroughMode != "transparent" {
				compatible = append(compatible, z)
			}
		}
	}
	if len(compatible) == 0 {
		failRequest(w, r, http.StatusNotImplemented, "protocol_not_supported", "no Anthropic or OpenAI-compatible route is configured")
		return
	}
	stream, _ := body["stream"].(bool)
	a.runRoutes(w, r, key, compatible, "anthropic_messages", requestReasoningEffort(body), stream, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		if z.Provider.Type == "openai" || z.Provider.Type == "grok" || z.Provider.Type == "openrouter" || z.Provider.Type == "openai_compatible" || z.Provider.Type == "grok_oauth" || z.Provider.Type == "opencode" && opencodeRouteProtocol(z) == opencodeProtocolChat {
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
			normalizeAnthropicCacheControlTTL(copyBody)
			var encodeErr error
			encoded, encodeErr = json.Marshal(copyBody)
			if encodeErr != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: encodeErr}
			}
		}
		upstreamStream := stream || !transparent
		if upstreamStream && !transparent {
			var streamed map[string]any
			if err := json.Unmarshal(encoded, &streamed); err != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
			}
			streamed["stream"] = true
			encoded, err = json.Marshal(streamed)
			if err != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}
			}
		}
		var jsonTransform func([]byte) ([]byte, string, error)
		var streamTransform func([]byte) ([]byte, error)
		var sseTransform = completedAnthropicMessageFromSSE
		if !transparent {
			jsonTransform = func(body []byte) ([]byte, string, error) {
				return anthropicJSONWithPublicModel(body, z.Route.PublicName)
			}
			sseTransform = completedAnthropicMessageForModel(z.Route.PublicName)
			if stream {
				streamTransform = anthropicSSEWithPublicModel(z.Route.PublicName)
			}
		}
		return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/messages", RawBody: encoded, Stream: stream, Transparent: transparent, UsageFormat: "anthropic", GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, UpstreamSSE: upstreamStream && !transparent, BufferSSE: upstreamStream && !stream, SSETransform: sseTransform, JSONTransform: jsonTransform, ErrorWriter: writeAnthropicBridgeError, StreamTransform: streamTransform})
	})
}

func (a *App) messageTokenCount(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		failRequest(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	body, raw, err := getBody(r)
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		failRequest(w, r, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	canonical, err := a.canonicalModel(r.Context(), model)
	if err != nil {
		failRequest(w, r, http.StatusInternalServerError, "database_error", "could not resolve model alias")
		return
	}
	if !modelAllowed(key, model, canonical) {
		failRequest(w, r, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), canonical, "chat")
	if err != nil {
		failRequest(w, r, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	routes = exposeRequestedModel(restrictQualityDetectorRoutes(key, routes), model)
	if len(routes) == 0 {
		failRequest(w, r, http.StatusNotFound, "model_not_found", "the selected quality detector route is unavailable")
		return
	}
	if key.QualityRoute == nil {
		routes = filterClientRoutes(routes, r)
		if len(routes) == 0 {
			failRequest(w, r, http.StatusForbidden, "provider_client_policy_mismatch", "no provider accepts this request's real User-Agent")
			return
		}
	}
	native := make([]resolvedRoute, 0, len(routes))
	fallback := make([]resolvedRoute, 0, len(routes))
	for _, z := range routes {
		switch z.Provider.Type {
		case "anthropic", "claude_oauth":
			native = append(native, z)
		case "opencode":
			fallback = append(fallback, z)
		case "openai", "grok", "openrouter", "openai_compatible", "codex_oauth", "grok_oauth":
			fallback = append(fallback, z)
		}
	}
	if len(native) == 0 {
		if len(fallback) == 0 {
			failRequest(w, r, http.StatusNotImplemented, "protocol_not_supported", "no Anthropic-compatible route is configured")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicInputTokens(body)})
		return
	}
	a.runRoutes(w, r, key, native, "anthropic_count_tokens", requestReasoningEffort(body), false, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		transparent := z.Provider.PassthroughMode == "transparent"
		encoded := raw
		if !transparent {
			copyBody := cloneMap(body)
			copyBody["model"] = z.Route.UpstreamModel
			normalizeAnthropicCacheControlTTL(copyBody)
			var encodeErr error
			encoded, encodeErr = json.Marshal(copyBody)
			if encodeErr != nil {
				return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: encodeErr}
			}
		}
		return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/messages/count_tokens", RawBody: encoded, Transparent: transparent, GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte, ErrorWriter: writeAnthropicBridgeError, JSONOutputValid: func(body []byte) bool {
			var payload map[string]any
			return json.Unmarshal(body, &payload) == nil && num(payload["input_tokens"]) > 0
		}})
	})
}

func estimateAnthropicInputTokens(body map[string]any) int64 {
	copyBody := cloneMap(body)
	delete(copyBody, "model")
	encoded, _ := json.Marshal(copyBody)
	var ascii, nonASCII int64
	for _, r := range string(encoded) {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	estimate := (ascii+3)/4 + nonASCII
	if estimate < 1 {
		return 1
	}
	return estimate
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

func (a *App) audioSpeech(w http.ResponseWriter, r *http.Request, key authKey) {
	if !key.AllowAudio {
		fail(w, http.StatusForbidden, "audio_not_allowed", "this key is not permitted to use audio endpoints")
		return
	}
	a.openAIEndpoint(w, r, key, "openai_audio_speech", "/v1/audio/speech", "audio_speech", true)
}

// audioTranscriptions forwards multipart/form-data transcription requests. The
// OpenAI endpoint takes a file part plus a model field, so a custom thin executor
// reads the model part, resolves audio_transcribe routes and forwards the original
// multipart body transparently (Content-Type with its boundary must be preserved;
// getBody/openAIEndpoint only accept JSON bodies).
func (a *App) audioTranscriptions(w http.ResponseWriter, r *http.Request, key authKey) {
	if !key.AllowAudio {
		fail(w, http.StatusForbidden, "audio_not_allowed", "this key is not permitted to use audio endpoints")
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	const limit = 32 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	if len(body) > limit {
		failBodyError(w, r, errRequestBodyTooLarge)
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		fail(w, http.StatusBadRequest, "invalid_request", "multipart/form-data request required")
		return
	}
	model := ""
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, partErr := mr.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			fail(w, http.StatusBadRequest, "invalid_request", "invalid multipart body")
			return
		}
		fieldName := part.FormName()
		if fieldName == "model" {
			raw, readErr := io.ReadAll(io.LimitReader(part, 1<<10))
			_ = part.Close()
			if readErr != nil {
				fail(w, http.StatusBadRequest, "invalid_request", "could not read model field")
				return
			}
			model = strings.TrimSpace(string(raw))
			break
		}
		_ = part.Close()
	}
	if model == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	canonical, err := a.canonicalModel(r.Context(), model)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", "could not resolve model alias")
		return
	}
	if !modelAllowed(key, model, canonical) {
		fail(w, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	routes, err := a.resolve(r.Context(), canonical, "audio_transcribe")
	if err != nil {
		fail(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	routes = restrictQualityDetectorRoutes(key, routes)
	routes = exposeRequestedModel(routes, model)
	if len(routes) == 0 {
		fail(w, http.StatusNotFound, "model_not_found", "the selected quality detector route is unavailable")
		return
	}
	compatible := routes[:0]
	for _, z := range routes {
		switch z.Provider.Type {
		case "openai", "grok", "openrouter", "openai_compatible", "codex_oauth", "grok_oauth":
			compatible = append(compatible, z)
		}
	}
	if len(compatible) == 0 {
		fail(w, http.StatusNotImplemented, "protocol_not_supported", "no OpenAI-compatible route is configured")
		return
	}
	a.runRoutes(w, r, key, compatible, "openai_audio_transcriptions", "", false, func(z resolvedRoute, rid string, onFirstByte func()) attemptResult {
		return a.proxyUpstream(w, r, z, proxyOptions{Endpoint: "/v1/audio/transcriptions", RawBody: body, Transparent: true, GatewayID: rid, SafeTransportRetry: true, OnFirstByte: onFirstByte})
	})
}

func (a *App) embeddings(w http.ResponseWriter, r *http.Request, key authKey) {
	a.openAIEndpoint(w, r, key, "openai_embeddings", "/v1/embeddings", "embedding", true)
}
