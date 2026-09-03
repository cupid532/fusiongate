package fusiongate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errNoProviderKeySupportsModel = errors.New("no enabled API key supports this model")

const providerKeySoftLimit = 500

const (
	providerKeySelectionConfigured     = "configured"
	providerKeySelectionLowMultiplier  = "low_multiplier"
	providerKeySelectionHighMultiplier = "high_multiplier"
	providerKeySelectionRoundRobin     = "round_robin"
)

const (
	providerKeyEgressInherit = "inherit"
	providerKeyEgressDirect  = "direct"
	providerKeyEgressNode    = "node"
)

type ProviderAPIKey struct {
	ID                 int64              `json:"id"`
	ProviderID         int64              `json:"provider_id"`
	Name               string             `json:"name"`
	KeyHint            string             `json:"key_hint"`
	Model              string             `json:"model,omitempty"`
	EffectiveModel     string             `json:"effective_model,omitempty"`
	ModelInherited     bool               `json:"model_inherited"`
	EgressMode         string             `json:"egress_mode"`
	IPPoolNodeID       *int64             `json:"ip_pool_node_id,omitempty"`
	IPPoolNodeName     string             `json:"ip_pool_node_name,omitempty"`
	EffectiveEgress    string             `json:"effective_egress"`
	EffectiveNodeID    *int64             `json:"effective_node_id,omitempty"`
	EgressInherited    bool               `json:"egress_inherited"`
	Enabled            bool               `json:"enabled"`
	HealthCheckEnabled bool               `json:"health_check_enabled"`
	ModelPolicy        string             `json:"model_policy"`
	ModelAllowlist     string             `json:"model_allowlist,omitempty"`
	CostMultiplier     float64            `json:"cost_multiplier"`
	SortOrder          int                `json:"sort_order"`
	Status             string             `json:"status"`
	LastError          string             `json:"last_error,omitempty"`
	LastTestedAt       string             `json:"last_tested_at,omitempty"`
	LastTestLatencyMS  int64              `json:"last_test_latency_ms"`
	DiscoveredModels   int                `json:"discovered_models"`
	EnabledModels      int                `json:"enabled_models"`
	RoutableModels     int                `json:"routable_models"`
	LastDiscoveredAt   string             `json:"last_discovered_at,omitempty"`
	Models             []ProviderKeyModel `json:"models"`
	CreatedAt          string             `json:"created_at"`
	UpdatedAt          string             `json:"updated_at"`
}

type ProviderKeyModel struct {
	Model         string   `json:"model"`
	DisplayName   string   `json:"display_name"`
	Capabilities  string   `json:"capabilities"`
	Enabled       bool     `json:"enabled"`
	HealthStatus  string   `json:"health_status,omitempty"`
	HealthError   string   `json:"health_error,omitempty"`
	LatencyMS     int64    `json:"latency_ms"`
	FirstByteMS   int64    `json:"first_byte_ms"`
	LastCheckedAt string   `json:"last_checked_at,omitempty"`
	PublicNames   []string `json:"public_names,omitempty"`
	RouteStatus   string   `json:"route_status,omitempty"`
}

type selectedProviderKey struct {
	ID             int64
	Credential     string
	Name           string
	Hint           string
	Model          string
	IPPoolNodeID   *int64
	CooldownUntil  time.Time
	CostMultiplier float64
}

func validProviderKeyEgressMode(mode string) bool {
	return mode == providerKeyEgressInherit || mode == providerKeyEgressDirect || mode == providerKeyEgressNode
}

func normalizeKeySelectionMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if validKeySelectionMode(mode) {
		return mode
	}
	return providerKeySelectionConfigured
}

func validKeySelectionMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case providerKeySelectionConfigured, providerKeySelectionLowMultiplier, providerKeySelectionHighMultiplier, providerKeySelectionRoundRobin:
		return true
	default:
		return false
	}
}

func (a *App) resetProviderKeyRoundRobin(providerID int64) {
	prefix := strconv.FormatInt(providerID, 10) + "\x00"
	a.routeMu.Lock()
	for key := range a.providerKeyRoundRobin {
		if strings.HasPrefix(key, prefix) {
			delete(a.providerKeyRoundRobin, key)
		}
	}
	a.routeMu.Unlock()
}

func normalizeProviderKeyModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func validProviderKeyModelPolicy(policy string) bool {
	return policy == "fallback" || policy == "allowlist"
}

func normalizeProviderKeyAllowlist(value string) string {
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, model := range strings.Split(value, ",") {
		model = normalizeProviderKeyModel(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return strings.Join(models, ",")
}

// providerKeySupportsModel is shared by routing, explicit key application, and health checks.
func providerKeySupportsModel(policy, allowlist, fixedModel, defaultModel, requested string, inventory, exclusions map[string]bool) bool {
	requested = normalizeProviderKeyModel(requested)
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = "fallback"
	}
	if requested == "" || !validProviderKeyModelPolicy(policy) || (policy == "allowlist" && normalizeProviderKeyAllowlist(allowlist) == "") {
		return false
	}
	if exclusions[requested] {
		return false
	}
	if fixedModel != "" {
		return normalizeProviderKeyModel(fixedModel) == requested
	}
	if policy == "allowlist" {
		for _, model := range strings.Split(normalizeProviderKeyAllowlist(allowlist), ",") {
			if model == requested {
				return true
			}
		}
		return false
	}
	if len(inventory) > 0 {
		return inventory[requested]
	}
	return defaultModel == "" || normalizeProviderKeyModel(defaultModel) == requested
}

func (a *App) providerKeyModelSets(ctx context.Context, keyID int64) (map[string]bool, map[string]bool, error) {
	inventory := make(map[string]bool)
	exclusions := make(map[string]bool)
	rows, err := a.reader().QueryContext(ctx, `SELECT model,enabled FROM provider_api_key_models WHERE provider_key_id=?`, keyID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var model string
		var enabled int
		if err := rows.Scan(&model, &enabled); err != nil {
			rows.Close()
			return nil, nil, err
		}
		inventory[normalizeProviderKeyModel(model)] = strBool(enabled)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err = a.reader().QueryContext(ctx, `SELECT model FROM provider_api_key_model_exclusions WHERE provider_key_id=?`, keyID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			rows.Close()
			return nil, nil, err
		}
		exclusions[normalizeProviderKeyModel(model)] = true
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	return inventory, exclusions, nil
}

func providerKeyHint(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case len(raw) <= 4:
		return strings.Repeat("•", len(raw))
	case len(raw) <= 8:
		return raw[:2] + "..." + raw[len(raw)-2:]
	default:
		return raw[:4] + "..." + raw[len(raw)-4:]
	}
}

func (a *App) providerKeyFingerprint(raw string) string {
	key := sha256.Sum256([]byte("fusiongate/provider-api-key/fingerprint\x00" + a.cfg.MasterKey))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(mac.Sum(nil))
}

func effectiveProviderKeyNode(mode string, keyNode, providerNode sql.NullInt64) (*int64, string) {
	var selected sql.NullInt64
	switch mode {
	case providerKeyEgressDirect:
		return nil, "direct"
	case providerKeyEgressNode:
		selected = keyNode
	default:
		selected = providerNode
	}
	if selected.Valid {
		value := selected.Int64
		return &value, "node"
	}
	return nil, "direct"
}

func (a *App) migrateProviderAPIKeys(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,credential FROM providers WHERE auth_kind='api_key' AND multi_key_initialized=0 ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyProvider struct {
		id         int64
		credential []byte
	}
	var legacy []legacyProvider
	for rows.Next() {
		var item legacyProvider
		if err := rows.Scan(&item.id, &item.credential); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range legacy {
		raw, err := a.decrypt(item.credential)
		if err != nil {
			a.log.Warn("legacy provider API key could not be migrated", "provider_id", item.id, "error", err)
			raw = ""
		}
		raw = strings.TrimSpace(raw)
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if raw != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,'','inherit',1,0,'untested',?,?) ON CONFLICT(provider_id,fingerprint) DO NOTHING`, item.id, item.credential, a.providerKeyFingerprint(raw), providerKeyHint(raw), "Key 1", now(), now())
			if err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE providers SET multi_key_initialized=1,updated_at=? WHERE id=?`, now(), item.id); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE provider_api_keys SET name='Key 1',updated_at=? WHERE name='默认 Key'`, now()); err != nil {
		return err
	}
	return nil
}

type providerKeySelectionKey struct {
	providerID    int64
	upstreamModel string
}

type providerKeySelectionRequest struct {
	providerID          int64
	upstreamModel       string
	providerNodeID      *int64
	legacyCredential    []byte
	multiKeyInitialized bool
}

type providerKeySelectionResult struct {
	keys []selectedProviderKey
	err  error
}

func makeProviderKeySelectionKey(providerID int64, upstreamModel string) providerKeySelectionKey {
	return providerKeySelectionKey{providerID: providerID, upstreamModel: normalizeProviderKeyModel(upstreamModel)}
}

func (a *App) selectProviderKey(ctx context.Context, providerID int64, upstreamModel string, providerNodeID *int64, legacyCredential []byte, initialized bool) (selectedProviderKey, error) {
	keys, err := a.selectProviderKeys(ctx, providerID, upstreamModel, providerNodeID, legacyCredential, initialized)
	if err != nil {
		return selectedProviderKey{}, err
	}
	return keys[0], nil
}

func (a *App) selectProviderKeys(ctx context.Context, providerID int64, upstreamModel string, providerNodeID *int64, legacyCredential []byte, initialized bool) ([]selectedProviderKey, error) {
	request := providerKeySelectionRequest{
		providerID:          providerID,
		upstreamModel:       upstreamModel,
		providerNodeID:      providerNodeID,
		legacyCredential:    legacyCredential,
		multiKeyInitialized: initialized,
	}
	selected, err := a.selectProviderKeysBatch(ctx, []providerKeySelectionRequest{request})
	if err != nil {
		return nil, err
	}
	result := selected[makeProviderKeySelectionKey(providerID, upstreamModel)]
	return result.keys, result.err
}

func (a *App) selectProviderKeysBatch(ctx context.Context, requests []providerKeySelectionRequest) (map[providerKeySelectionKey]providerKeySelectionResult, error) {
	selected := make(map[providerKeySelectionKey]providerKeySelectionResult, len(requests))
	selectionModes := make(map[providerKeySelectionKey]string, len(requests))
	type initializedRequest struct {
		key     providerKeySelectionKey
		request providerKeySelectionRequest
	}
	initialized := make([]initializedRequest, 0, len(requests))
	for _, request := range requests {
		key := makeProviderKeySelectionKey(request.providerID, request.upstreamModel)
		if _, exists := selected[key]; exists {
			continue
		}
		if !request.multiKeyInitialized {
			raw, err := a.decrypt(request.legacyCredential)
			if err != nil {
				selected[key] = providerKeySelectionResult{err: err}
				continue
			}
			selected[key] = providerKeySelectionResult{keys: []selectedProviderKey{{Credential: raw, Hint: providerKeyHint(raw), IPPoolNodeID: request.providerNodeID}}}
			continue
		}
		selected[key] = providerKeySelectionResult{err: errNoProviderKeySupportsModel}
		initialized = append(initialized, initializedRequest{key: key, request: request})
	}
	if len(initialized) == 0 {
		return selected, nil
	}

	var query strings.Builder
	query.WriteString(`WITH candidates(ordinal,provider_id,upstream_model) AS (VALUES `)
	args := make([]any, 0, len(initialized)*3)
	for i, item := range initialized {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?,?,?)")
		args = append(args, i, item.request.providerID, item.request.upstreamModel)
	}
	query.WriteString(`)
SELECT c.ordinal,k.id,k.credential,k.name,k.key_hint,k.model,k.model_policy,k.model_allowlist,k.egress_mode,k.ip_pool_node_id,COALESCE(k.cooldown_until,''),k.cost_multiplier,p.default_model,p.key_selection_mode
FROM candidates c
JOIN providers p ON p.id=c.provider_id
JOIN provider_api_keys k ON k.provider_id=c.provider_id AND k.enabled=1
ORDER BY c.ordinal,k.sort_order,k.id`)

	rows, err := a.reader().QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		ordinal                                                                   int
		key                                                                       selectedProviderKey
		encrypted                                                                 []byte
		keyNodeID                                                                 sql.NullInt64
		egressMode, cooldownUntil, policy, allowlist, defaultModel, selectionMode string
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ordinal, &c.key.ID, &c.encrypted, &c.key.Name, &c.key.Hint, &c.key.Model, &c.policy, &c.allowlist, &c.egressMode, &c.keyNodeID, &c.cooldownUntil, &c.key.CostMultiplier, &c.defaultModel, &c.selectionMode); err != nil {
			rows.Close()
			return nil, err
		}
		if c.ordinal < 0 || c.ordinal >= len(initialized) {
			rows.Close()
			return nil, errors.New("provider key batch returned an invalid candidate")
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, c := range candidates {
		item := initialized[c.ordinal]
		selectionModes[item.key] = normalizeKeySelectionMode(c.selectionMode)
		inventory, exclusions, setErr := a.providerKeyModelSets(ctx, c.key.ID)
		if setErr != nil {
			return nil, setErr
		}
		if !providerKeySupportsModel(c.policy, c.allowlist, c.key.Model, c.defaultModel, item.request.upstreamModel, inventory, exclusions) {
			continue
		}
		raw, err := a.decrypt(c.encrypted)
		if err != nil {
			a.log.Error("provider key credential decrypt", "provider_id", item.request.providerID, "provider_key_id", c.key.ID, "error", sanitizeError(err.Error()))
			result := selected[item.key]
			if len(result.keys) == 0 && errors.Is(result.err, errNoProviderKeySupportsModel) {
				result.err = err
			}
			selected[item.key] = result
			continue
		}
		c.key.Credential = raw
		var providerNode sql.NullInt64
		if item.request.providerNodeID != nil {
			providerNode = sql.NullInt64{Int64: *item.request.providerNodeID, Valid: true}
		}
		c.key.IPPoolNodeID, _ = effectiveProviderKeyNode(c.egressMode, c.keyNodeID, providerNode)
		if parsed := parseTime(c.cooldownUntil); parsed != nil && parsed.After(time.Now()) {
			c.key.CooldownUntil = *parsed
			a.routeMu.Lock()
			a.providerKeyCooldowns[c.key.ID] = *parsed
			a.routeMu.Unlock()
		}
		result := selected[item.key]
		result.err = nil
		result.keys = append(result.keys, c.key)
		selected[item.key] = result
	}
	for key, result := range selected {
		mode := selectionModes[key]
		if len(result.keys) > 1 && (mode == providerKeySelectionLowMultiplier || mode == providerKeySelectionHighMultiplier) {
			sort.SliceStable(result.keys, func(i, j int) bool {
				if result.keys[i].CostMultiplier == result.keys[j].CostMultiplier {
					return false
				}
				if mode == providerKeySelectionLowMultiplier {
					return result.keys[i].CostMultiplier < result.keys[j].CostMultiplier
				}
				return result.keys[i].CostMultiplier > result.keys[j].CostMultiplier
			})
		}
		if len(result.keys) > 1 && mode == providerKeySelectionRoundRobin {
			a.routeMu.Lock()
			keyName := fmt.Sprintf("%d\x00%s", key.providerID, key.upstreamModel)
			start := a.providerKeyRoundRobin[keyName] % len(result.keys)
			a.providerKeyRoundRobin[keyName] = (start + 1) % len(result.keys)
			a.routeMu.Unlock()
			rotated := append([]selectedProviderKey(nil), result.keys[start:]...)
			result.keys = append(rotated, result.keys[:start]...)
		}
		selected[key] = result
	}
	return selected, nil
}

func (a *App) applyProviderKeyForModel(ctx context.Context, p *discoveryProvider, upstreamModel string) error {
	if p == nil {
		return errors.New("provider is required")
	}
	var authKind string
	var legacyCredential []byte
	var providerNodeID sql.NullInt64
	var initialized int
	if err := a.db.QueryRowContext(ctx, `SELECT auth_kind,credential,ip_pool_node_id,multi_key_initialized FROM providers WHERE id=?`, p.ID).Scan(&authKind, &legacyCredential, &providerNodeID, &initialized); err != nil {
		return err
	}
	if authKind != "api_key" {
		return nil
	}
	var nodeID *int64
	if providerNodeID.Valid {
		value := providerNodeID.Int64
		nodeID = &value
	}
	selected, err := a.selectProviderKey(ctx, p.ID, upstreamModel, nodeID, legacyCredential, strBool(initialized))
	if err != nil {
		return err
	}
	p.Credential = selected.Credential
	p.IPPoolNodeID = selected.IPPoolNodeID
	return nil
}

func (a *App) applyProviderKeyByID(ctx context.Context, p *discoveryProvider, keyID int64, upstreamModel string) error {
	if p == nil {
		return errors.New("provider is required")
	}
	var encrypted []byte
	var mode string
	var keyNode, providerNode sql.NullInt64
	var policy, allowlist, fixedModel, defaultModel string
	err := a.db.QueryRowContext(ctx, `SELECT k.credential,k.egress_mode,k.ip_pool_node_id,p.ip_pool_node_id,k.model,k.model_policy,k.model_allowlist,p.default_model FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.id=? AND k.provider_id=? AND k.enabled=1 AND k.health_check_enabled=1`, keyID, p.ID).Scan(&encrypted, &mode, &keyNode, &providerNode, &fixedModel, &policy, &allowlist, &defaultModel)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("selected API key does not support this model")
	}
	if err != nil {
		return err
	}
	inventory, exclusions, err := a.providerKeyModelSets(ctx, keyID)
	if err != nil {
		return err
	}
	if !providerKeySupportsModel(policy, allowlist, fixedModel, defaultModel, upstreamModel, inventory, exclusions) {
		return errors.New("selected API key does not support this model")
	}
	raw, err := a.decrypt(encrypted)
	if err != nil {
		return err
	}
	p.Credential = raw
	p.IPPoolNodeID, _ = effectiveProviderKeyNode(mode, keyNode, providerNode)
	return nil
}

func (a *App) loadDiscoveryProviderKey(ctx context.Context, providerID int64, providerNodeID *int64, legacyCredential []byte, initialized bool) (selectedProviderKey, error) {
	if !initialized {
		return a.selectProviderKey(ctx, providerID, "", providerNodeID, legacyCredential, false)
	}
	var selected selectedProviderKey
	var encrypted []byte
	var keyNodeID sql.NullInt64
	var egressMode string
	err := a.db.QueryRowContext(ctx, `SELECT id,credential,name,key_hint,model,egress_mode,ip_pool_node_id FROM provider_api_keys WHERE provider_id=? AND enabled=1 ORDER BY sort_order,id LIMIT 1`, providerID).Scan(&selected.ID, &encrypted, &selected.Name, &selected.Hint, &selected.Model, &egressMode, &keyNodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return selectedProviderKey{}, errors.New("provider has no enabled API key")
		}
		return selectedProviderKey{}, err
	}
	raw, err := a.decrypt(encrypted)
	if err != nil {
		return selectedProviderKey{}, err
	}
	selected.Credential = raw
	var providerNode sql.NullInt64
	if providerNodeID != nil {
		providerNode = sql.NullInt64{Int64: *providerNodeID, Valid: true}
	}
	selected.IPPoolNodeID, _ = effectiveProviderKeyNode(egressMode, keyNodeID, providerNode)
	return selected, nil
}

func (a *App) providerKeys(w http.ResponseWriter, r *http.Request, providerID int64) {
	var authKind, defaultModel string
	var providerNodeID sql.NullInt64
	if err := a.db.QueryRow(`SELECT auth_kind,default_model,ip_pool_node_id FROM providers WHERE id=?`, providerID).Scan(&authKind, &defaultModel, &providerNodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusNotFound, "not_found", "provider not found")
		} else {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
		}
		return
	}
	if authKind != "api_key" {
		fail(w, http.StatusBadRequest, "unsupported_provider", "OAuth credential files do not use API key cards")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := a.db.Query(`SELECT k.id,k.provider_id,k.name,k.key_hint,k.model,k.egress_mode,k.ip_pool_node_id,COALESCE(n.name,''),k.enabled,k.health_check_enabled,k.model_policy,k.model_allowlist,k.cost_multiplier,k.sort_order,k.status,k.last_error,COALESCE(k.last_tested_at,''),k.last_test_latency_ms,(SELECT COUNT(*) FROM provider_api_key_models km WHERE km.provider_key_id=k.id),COALESCE((SELECT MAX(discovered_at) FROM provider_api_key_models km WHERE km.provider_key_id=k.id),''),k.created_at,k.updated_at FROM provider_api_keys k LEFT JOIN ip_pool_nodes n ON n.id=k.ip_pool_node_id WHERE k.provider_id=? ORDER BY k.sort_order,k.id`, providerID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []ProviderAPIKey{}
		for rows.Next() {
			var key ProviderAPIKey
			var enabled, healthCheckEnabled int
			var policy, allowlist string
			var keyNodeID sql.NullInt64
			if err := rows.Scan(&key.ID, &key.ProviderID, &key.Name, &key.KeyHint, &key.Model, &key.EgressMode, &keyNodeID, &key.IPPoolNodeName, &enabled, &healthCheckEnabled, &policy, &allowlist, &key.CostMultiplier, &key.SortOrder, &key.Status, &key.LastError, &key.LastTestedAt, &key.LastTestLatencyMS, &key.DiscoveredModels, &key.LastDiscoveredAt, &key.CreatedAt, &key.UpdatedAt); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			key.Enabled = strBool(enabled)
			key.HealthCheckEnabled = strBool(healthCheckEnabled)
			key.ModelPolicy, key.ModelAllowlist = policy, allowlist
			if keyNodeID.Valid {
				value := keyNodeID.Int64
				key.IPPoolNodeID = &value
			}
			key.ModelInherited = key.Model == ""
			key.EffectiveModel = key.Model
			if key.EffectiveModel == "" {
				key.EffectiveModel = defaultModel
			}
			key.EgressInherited = key.EgressMode == providerKeyEgressInherit
			effectiveID, effectiveMode := effectiveProviderKeyNode(key.EgressMode, keyNodeID, providerNodeID)
			key.EffectiveNodeID, key.EffectiveEgress = effectiveID, effectiveMode
			out = append(out, key)
		}
		if err := rows.Close(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		for i := range out {
			modelRows, modelErr := a.db.Query(`SELECT m.model,m.display_name,m.capabilities,m.enabled,COALESCE(h.status,''),COALESCE(h.error,''),COALESCE(h.latency_ms,0),COALESCE(h.first_byte_ms,0),COALESCE(h.last_checked_at,'') FROM (SELECT model,display_name,capabilities,enabled FROM provider_api_key_models WHERE provider_key_id=? UNION ALL SELECT h.model,'','chat,stream',1 FROM provider_api_key_model_health h WHERE h.provider_key_id=? AND NOT EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=h.provider_key_id AND km.model=h.model)) m LEFT JOIN provider_api_key_model_health h ON h.provider_key_id=? AND h.model=m.model ORDER BY m.model`, out[i].ID, out[i].ID, out[i].ID)
			if modelErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", modelErr.Error())
				return
			}
			out[i].Models = []ProviderKeyModel{}
			for modelRows.Next() {
				var model ProviderKeyModel
				var modelEnabled int
				if modelRows.Scan(&model.Model, &model.DisplayName, &model.Capabilities, &modelEnabled, &model.HealthStatus, &model.HealthError, &model.LatencyMS, &model.FirstByteMS, &model.LastCheckedAt) == nil {
					model.Enabled = strBool(modelEnabled)
					if model.Enabled {
						out[i].EnabledModels++
					}
					routeRows, routeErr := a.reader().Query(`SELECT public_name FROM model_routes WHERE provider_id=? AND enabled=1 AND (lower(upstream_model)=lower(?) OR lower(public_name)=lower(?)) ORDER BY public_name`, out[i].ProviderID, model.Model, model.Model)
					if routeErr == nil {
						for routeRows.Next() {
							var publicName string
							if routeRows.Scan(&publicName) == nil {
								model.PublicNames = append(model.PublicNames, publicName)
							}
						}
						_ = routeRows.Close()
					}
					if len(model.PublicNames) > 0 {
						model.RouteStatus = "routed"
						if model.Enabled && out[i].Enabled {
							out[i].RoutableModels++
						}
					} else {
						model.RouteStatus = "unrouted"
					}
					out[i].Models = append(out[i].Models, model)
				}
			}
			_ = modelRows.Close()
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			APIKey             string  `json:"api_key"`
			Name               string  `json:"name"`
			Model              string  `json:"model"`
			EgressMode         string  `json:"egress_mode"`
			IPPoolNodeID       *int64  `json:"ip_pool_node_id"`
			Enabled            *bool   `json:"enabled"`
			HealthCheckEnabled *bool   `json:"health_check_enabled"`
			ModelPolicy        string  `json:"model_policy"`
			ModelAllowlist     string  `json:"model_allowlist"`
			CostMultiplier     float64 `json:"cost_multiplier"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.APIKey = strings.TrimSpace(in.APIKey)
		in.Name = strings.TrimSpace(in.Name)
		in.Model = normalizeProviderKeyModel(in.Model)
		in.ModelPolicy = strings.ToLower(strings.TrimSpace(in.ModelPolicy))
		if in.ModelPolicy == "" {
			in.ModelPolicy = "fallback"
		}
		in.ModelAllowlist = normalizeProviderKeyAllowlist(in.ModelAllowlist)
		in.EgressMode = strings.ToLower(strings.TrimSpace(in.EgressMode))
		if in.EgressMode == "" {
			in.EgressMode = providerKeyEgressInherit
		}
		if in.CostMultiplier == 0 {
			in.CostMultiplier = 1
		}
		if in.APIKey == "" || !validProviderKeyModelPolicy(in.ModelPolicy) || !validProviderKeyEgressMode(in.EgressMode) || in.CostMultiplier <= 0 || in.CostMultiplier > 1000 {
			fail(w, http.StatusBadRequest, "invalid_request", "API key and a valid egress mode are required")
			return
		}
		if err := a.validateProviderKeyNode(in.EgressMode, in.IPPoolNodeID); err != nil {
			fail(w, http.StatusBadRequest, "invalid_ip_pool_node", err.Error())
			return
		}
		var count, nextOrder int
		if err := a.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(sort_order),-1)+1 FROM provider_api_keys WHERE provider_id=?`, providerID).Scan(&count, &nextOrder); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if count >= providerKeySoftLimit {
			fail(w, http.StatusConflict, "key_limit_reached", fmt.Sprintf("a provider supports at most %d API key cards", providerKeySoftLimit))
			return
		}
		encrypted, err := a.encrypt(in.APIKey)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_error", err.Error())
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		var nodeArg any
		if in.EgressMode == providerKeyEgressNode && in.IPPoolNodeID != nil {
			nodeArg = *in.IPPoolNodeID
		}
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		healthCheckEnabled := true
		if in.HealthCheckEnabled != nil {
			healthCheckEnabled = *in.HealthCheckEnabled
		}
		res, err := tx.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,ip_pool_node_id,enabled,health_check_enabled,cost_multiplier,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?, 'untested',?,?)`, providerID, encrypted, a.providerKeyFingerprint(in.APIKey), providerKeyHint(in.APIKey), in.Name, in.Model, in.EgressMode, nodeArg, boolInt(enabled), boolInt(healthCheckEnabled), in.CostMultiplier, nextOrder, now(), now())
		if err != nil {
			lowerError := strings.ToLower(err.Error())
			if strings.Contains(lowerError, "provider key limit reached") {
				fail(w, http.StatusConflict, "key_limit_reached", fmt.Sprintf("a provider supports at most %d API key cards", providerKeySoftLimit))
			} else if strings.Contains(lowerError, "unique") {
				fail(w, http.StatusConflict, "duplicate_api_key", "this API key already exists in the provider")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		if count == 0 {
			if _, err := tx.Exec(`UPDATE providers SET credential=?,multi_key_initialized=1,updated_at=? WHERE id=?`, encrypted, now(), providerID); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "key_hint": providerKeyHint(in.APIKey)})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

type providerModelManagementItem struct {
	KeyID          int64    `json:"key_id"`
	ModelPolicy    string   `json:"model_policy"`
	ModelAllowlist string   `json:"model_allowlist"`
	Models         []string `json:"models"`
	ExcludeModels  []string `json:"exclude_models"`
}

type providerModelManagementStatus struct {
	KeyID  int64  `json:"key_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (a *App) saveProviderModelManagement(ctx context.Context, providerID int64, items []providerModelManagementItem) ([]providerModelManagementStatus, error) {
	if len(items) == 0 || len(items) > providerKeySoftLimit {
		return nil, errors.New("keys are required")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	statuses := make([]providerModelManagementStatus, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		st := providerModelManagementStatus{KeyID: item.KeyID, Status: "error"}
		if item.KeyID < 1 || seen[item.KeyID] {
			st.Error = "duplicate or invalid key_id"
			statuses = append(statuses, st)
			return statuses, errors.New(st.Error)
		}
		seen[item.KeyID] = true
		policy := strings.ToLower(strings.TrimSpace(item.ModelPolicy))
		if policy == "" {
			policy = "fallback"
		}
		if !validProviderKeyModelPolicy(policy) {
			st.Error = "invalid model policy"
			statuses = append(statuses, st)
			return statuses, errors.New(st.Error)
		}
		allowlist := normalizeProviderKeyAllowlist(item.ModelAllowlist)
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_api_keys WHERE id=? AND provider_id=?`, item.KeyID, providerID).Scan(&exists); err != nil {
			return statuses, err
		}
		if exists != 1 {
			st.Error = "key not found"
			statuses = append(statuses, st)
			return statuses, errors.New(st.Error)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE provider_api_keys SET model_policy=?,model_allowlist=?,updated_at=? WHERE id=? AND provider_id=?`, policy, allowlist, now(), item.KeyID, providerID); err != nil {
			return statuses, err
		}
		selected := map[string]bool{}
		for _, m := range item.Models {
			m = normalizeProviderKeyModel(m)
			if m != "" {
				selected[m] = true
			}
		}
		for _, m := range item.ExcludeModels {
			m = normalizeProviderKeyModel(m)
			if m == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)=?`, item.KeyID, m); err != nil {
				return statuses, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,?,?) ON CONFLICT(provider_key_id,model) DO UPDATE SET created_at=excluded.created_at`, item.KeyID, m, now()); err != nil {
				return statuses, err
			}
		}
		if item.Models != nil {
			rows, e := tx.QueryContext(ctx, `SELECT model FROM provider_api_key_models WHERE provider_key_id=?`, item.KeyID)
			if e != nil {
				return statuses, e
			}
			var inventory []string
			for rows.Next() {
				var m string
				if e = rows.Scan(&m); e != nil {
					rows.Close()
					return statuses, e
				}
				inventory = append(inventory, m)
			}
			if e = rows.Close(); e != nil {
				return statuses, e
			}
			for _, m := range inventory {
				enabled := selected[normalizeProviderKeyModel(m)]
				if _, e = tx.ExecContext(ctx, `UPDATE provider_api_key_models SET enabled=? WHERE provider_key_id=? AND model=?`, boolInt(enabled), item.KeyID, m); e != nil {
					return statuses, e
				}
				if enabled {
					if _, e = tx.ExecContext(ctx, `DELETE FROM provider_api_key_model_exclusions WHERE provider_key_id=? AND lower(model)=?`, item.KeyID, normalizeProviderKeyModel(m)); e != nil {
						return statuses, e
					}
				}
			}
		}
		var e error
		for m := range selected {
			if _, e = tx.ExecContext(ctx, `INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at) VALUES(?,?,?,'chat,stream',1,?) ON CONFLICT(provider_key_id,model) DO UPDATE SET enabled=1`, item.KeyID, m, m, now()); e != nil {
				return statuses, e
			}
			if _, e = tx.ExecContext(ctx, `DELETE FROM provider_api_key_model_exclusions WHERE provider_key_id=? AND lower(model)=?`, item.KeyID, m); e != nil {
				return statuses, e
			}
		}
		st.Status = "saved"
		statuses = append(statuses, st)
	}
	if err := tx.Commit(); err != nil {
		return statuses, err
	}
	return statuses, nil
}

func (a *App) providerKeyByID(w http.ResponseWriter, r *http.Request, providerID, keyID int64, action string) {
	if action == "reveal" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
			return
		}
		var encrypted []byte
		if err := a.db.QueryRow(`SELECT credential FROM provider_api_keys WHERE id=? AND provider_id=?`, keyID, providerID).Scan(&encrypted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fail(w, http.StatusNotFound, "not_found", "API key card not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		raw, err := a.decrypt(encrypted)
		if err != nil {
			fail(w, http.StatusInternalServerError, "credential_error", "API key could not be decrypted")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]string{"api_key": raw})
		return
	}
	if action == "test" {
		a.testProviderKey(w, r, providerID, keyID)
		return
	}
	if action == "discover-models" {
		a.discoverProviderKeyModels(w, r, providerID, keyID)
		return
	}
	if action != "" {
		fail(w, http.StatusNotFound, "not_found", "unknown API key action")
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			APIKey             *string   `json:"api_key"`
			Name               *string   `json:"name"`
			Model              *string   `json:"model"`
			EgressMode         *string   `json:"egress_mode"`
			IPPoolNodeID       *int64    `json:"ip_pool_node_id"`
			Enabled            *bool     `json:"enabled"`
			HealthCheckEnabled *bool     `json:"health_check_enabled"`
			ModelPolicy        *string   `json:"model_policy"`
			ModelAllowlist     *string   `json:"model_allowlist"`
			CostMultiplier     *float64  `json:"cost_multiplier"`
			SortOrder          *int      `json:"sort_order"`
			Models             *[]string `json:"models"`
			RemoveModels       []string  `json:"remove_models"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.CostMultiplier != nil && (*in.CostMultiplier <= 0 || *in.CostMultiplier > 1000) {
			fail(w, http.StatusBadRequest, "invalid_request", "cost multiplier must be greater than 0 and at most 1000")
			return
		}
		var encryptedArg, fingerprintArg, hintArg any
		if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
			raw := strings.TrimSpace(*in.APIKey)
			encrypted, err := a.encrypt(raw)
			if err != nil {
				fail(w, http.StatusInternalServerError, "credential_error", err.Error())
				return
			}
			encryptedArg, fingerprintArg, hintArg = encrypted, a.providerKeyFingerprint(raw), providerKeyHint(raw)
		}
		var nameArg, modelArg, egressArg any
		if in.Name != nil {
			nameArg = strings.TrimSpace(*in.Name)
		}
		if in.Model != nil {
			modelArg = normalizeProviderKeyModel(*in.Model)
		}
		var policyArg, allowlistArg any
		if in.ModelPolicy != nil {
			policy := strings.ToLower(strings.TrimSpace(*in.ModelPolicy))
			if !validProviderKeyModelPolicy(policy) {
				fail(w, http.StatusBadRequest, "invalid_request", "invalid model policy")
				return
			}
			policyArg = policy
		}
		if in.ModelAllowlist != nil {
			allowlistArg = normalizeProviderKeyAllowlist(*in.ModelAllowlist)
		}
		if in.EgressMode != nil {
			mode := strings.ToLower(strings.TrimSpace(*in.EgressMode))
			if !validProviderKeyEgressMode(mode) {
				fail(w, http.StatusBadRequest, "invalid_request", "invalid egress mode")
				return
			}
			egressArg = mode
		}
		var currentMode string
		var currentNode sql.NullInt64
		if err := a.db.QueryRow(`SELECT egress_mode,ip_pool_node_id FROM provider_api_keys WHERE id=? AND provider_id=?`, keyID, providerID).Scan(&currentMode, &currentNode); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fail(w, http.StatusNotFound, "not_found", "API key card not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		effectiveMode := currentMode
		if in.EgressMode != nil {
			effectiveMode = egressArg.(string)
		}
		var effectiveNode *int64
		if in.IPPoolNodeID != nil {
			effectiveNode = in.IPPoolNodeID
		} else if currentNode.Valid {
			value := currentNode.Int64
			effectiveNode = &value
		}
		if err := a.validateProviderKeyNode(effectiveMode, effectiveNode); err != nil {
			fail(w, http.StatusBadRequest, "invalid_ip_pool_node", err.Error())
			return
		}
		var nodeArg any
		if effectiveMode == providerKeyEgressNode && effectiveNode != nil && *effectiveNode > 0 {
			nodeArg = *effectiveNode
		}
		res, err := a.db.Exec(`UPDATE provider_api_keys SET credential=COALESCE(?,credential),fingerprint=COALESCE(?,fingerprint),key_hint=COALESCE(?,key_hint),name=COALESCE(?,name),model=COALESCE(?,model),model_policy=COALESCE(?,model_policy),model_allowlist=COALESCE(?,model_allowlist),egress_mode=COALESCE(?,egress_mode),ip_pool_node_id=CASE WHEN ? THEN ? ELSE ip_pool_node_id END,enabled=COALESCE(?,enabled),health_check_enabled=COALESCE(?,health_check_enabled),cost_multiplier=COALESCE(?,cost_multiplier),sort_order=COALESCE(?,sort_order),status=CASE WHEN ? THEN 'untested' ELSE status END,last_error=CASE WHEN ? THEN '' ELSE last_error END,updated_at=? WHERE id=? AND provider_id=?`, encryptedArg, fingerprintArg, hintArg, nameArg, modelArg, egressArg, policyArg, allowlistArg, in.EgressMode != nil || in.IPPoolNodeID != nil, nodeArg, maybeBool(in.Enabled), maybeBool(in.HealthCheckEnabled), in.CostMultiplier, in.SortOrder, encryptedArg != nil || modelArg != nil || egressArg != nil || in.IPPoolNodeID != nil, encryptedArg != nil || modelArg != nil || egressArg != nil || in.IPPoolNodeID != nil, now(), keyID, providerID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				fail(w, http.StatusConflict, "duplicate_api_key", "this API key already exists in the provider")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			fail(w, http.StatusNotFound, "not_found", "API key card not found")
			return
		}
		runtimeChanged := encryptedArg != nil || modelArg != nil || egressArg != nil || in.IPPoolNodeID != nil || (in.Enabled != nil && *in.Enabled)
		if runtimeChanged {
			a.resetProviderKeyRuntime(keyID)
		}
		if in.HealthCheckEnabled != nil {
			status, message := "untested", ""
			if !*in.HealthCheckEnabled {
				status, message = "disabled", "health checks disabled for this API key"
			}
			_, _ = a.db.Exec(`UPDATE provider_api_keys SET status=?,last_error=?,updated_at=? WHERE id=? AND provider_id=?`, status, message, now(), keyID, providerID)
		}
		if encryptedArg != nil {
			_, _ = a.db.Exec(`DELETE FROM provider_api_key_models WHERE provider_key_id=?`, keyID)
		}
		for _, model := range in.RemoveModels {
			model = normalizeProviderKeyModel(model)
			if model == "" {
				continue
			}
			if _, err := a.db.Exec(`DELETE FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)=lower(?)`, keyID, model); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			_, _ = a.db.Exec(`DELETE FROM provider_api_key_model_health WHERE provider_key_id=? AND lower(model)=lower(?)`, keyID, model)
			_, _ = a.db.Exec(`INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,?,?) ON CONFLICT(provider_key_id,model) DO UPDATE SET created_at=excluded.created_at`, keyID, model, now())
		}
		if in.Models != nil {
			selected := map[string]bool{}
			for _, model := range *in.Models {
				if normalized := normalizeProviderKeyModel(model); normalized != "" {
					selected[normalized] = true
					_, _ = a.db.Exec(`DELETE FROM provider_api_key_model_exclusions WHERE provider_key_id=? AND lower(model)=lower(?)`, keyID, normalized)
				}
			}
			for model := range selected {
				if _, insertErr := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at) VALUES(?,?,?,'chat,stream',1,?) ON CONFLICT(provider_key_id,model) DO UPDATE SET enabled=1`, keyID, model, model, now()); insertErr != nil {
					fail(w, http.StatusInternalServerError, "database_error", insertErr.Error())
					return
				}
			}
			modelRows, queryErr := a.db.Query(`SELECT model FROM provider_api_key_models WHERE provider_key_id=?`, keyID)
			if queryErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", queryErr.Error())
				return
			}
			var inventory []string
			for modelRows.Next() {
				var model string
				if modelRows.Scan(&model) == nil {
					inventory = append(inventory, model)
				}
			}
			_ = modelRows.Close()
			for _, model := range inventory {
				_, _ = a.db.Exec(`UPDATE provider_api_key_models SET enabled=? WHERE provider_key_id=? AND model=?`, boolInt(selected[strings.ToLower(model)]), keyID, model)
			}
		}
		// Keep the legacy provider credential synchronized with the current first
		// card. New runtimes never select it after initialization, but this makes
		// a binary rollback preserve the same primary credential.
		_, _ = a.db.Exec(`UPDATE providers SET credential=COALESCE((SELECT credential FROM provider_api_keys WHERE provider_id=? ORDER BY sort_order,id LIMIT 1),credential),updated_at=? WHERE id=?`, providerID, now(), providerID)
		writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
	case http.MethodDelete:
		tx, err := a.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer tx.Rollback()
		res, err := tx.Exec(`DELETE FROM provider_api_keys WHERE id=? AND provider_id=?`, keyID, providerID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			fail(w, http.StatusNotFound, "not_found", "API key card not found")
			return
		}
		var nextCredential []byte
		if err := tx.QueryRow(`SELECT credential FROM provider_api_keys WHERE provider_id=? ORDER BY sort_order,id LIMIT 1`, providerID).Scan(&nextCredential); err == nil {
			if _, err := tx.Exec(`UPDATE providers SET credential=?,updated_at=? WHERE id=?`, nextCredential, now(), providerID); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.Exec(`UPDATE providers SET credential=X'',enabled=0,updated_at=? WHERE id=?`, now(), providerID); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
		} else {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		a.resetProviderKeyRuntime(keyID)
		a.resetProviderRuntime(providerID)
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}

func (a *App) persistProviderKeyModelHealth(ctx context.Context, providerID, keyID int64, model string, result healthCheckResult) error {
	model = normalizeProviderKeyModel(model)
	if model == "" {
		return errors.New("health check model is required")
	}
	stamp := now()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT health_check_enabled FROM provider_api_keys WHERE id=? AND provider_id=?`, keyID, providerID).Scan(&enabled); err != nil {
		return err
	}
	if !strBool(enabled) {
		return errors.New("health checks disabled for this API key")
	}
	message := result.Error
	if result.Status == "healthy" {
		message = ""
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_api_key_model_health(provider_key_id,model,status,error,latency_ms,first_byte_ms,last_checked_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider_key_id,model) DO UPDATE SET status=excluded.status,error=excluded.error,latency_ms=excluded.latency_ms,first_byte_ms=excluded.first_byte_ms,last_checked_at=excluded.last_checked_at`, keyID, model, result.Status, message, result.LatencyMS, result.FirstByteMS, stamp); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT status,error,latency_ms,last_checked_at FROM provider_api_key_model_health WHERE provider_key_id=? ORDER BY last_checked_at DESC,model`, keyID)
	if err != nil {
		return err
	}
	aggregateStatus, aggregateError, lastChecked := "healthy", "", ""
	var aggregateLatency int64
	for rows.Next() {
		var status, healthError, checked string
		var latency int64
		if err := rows.Scan(&status, &healthError, &latency, &checked); err != nil {
			rows.Close()
			return err
		}
		if lastChecked == "" {
			lastChecked = checked
		}
		if latency > aggregateLatency {
			aggregateLatency = latency
		}
		if aggregateStatus == "healthy" && status != "healthy" {
			aggregateStatus, aggregateError = status, healthError
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE provider_api_keys SET status=?,last_error=?,last_tested_at=?,last_test_latency_ms=?,updated_at=? WHERE id=? AND provider_id=?`, aggregateStatus, aggregateError, lastChecked, aggregateLatency, stamp, keyID, providerID); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) validateProviderKeyNode(mode string, nodeID *int64) error {
	if mode != providerKeyEgressNode {
		return nil
	}
	if nodeID == nil || *nodeID < 1 {
		return errors.New("a node egress requires an IP pool node")
	}
	return a.validateIPPoolNode(*nodeID)
}

func (a *App) discoverProviderKeyModels(w http.ResponseWriter, r *http.Request, providerID, keyID int64) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var p discoveryProvider
	var providerNodeID, keyNodeID sql.NullInt64
	var encrypted []byte
	var egressMode, keyName, keyHint string
	if err := a.db.QueryRow(`SELECT p.id,p.name,p.type,p.base_url,p.request_timeout_ms,p.ip_pool_node_id,k.credential,k.name,k.key_hint,k.egress_mode,k.ip_pool_node_id FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.id=? AND k.provider_id=?`, keyID, providerID).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.RequestTimeoutMS, &providerNodeID, &encrypted, &keyName, &keyHint, &egressMode, &keyNodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusNotFound, "not_found", "API key card not found")
		} else {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
		}
		return
	}
	raw, err := a.decrypt(encrypted)
	if err != nil {
		fail(w, http.StatusInternalServerError, "credential_error", "API key could not be decrypted")
		return
	}
	p.Credential = raw
	p.IPPoolNodeID, _ = effectiveProviderKeyNode(egressMode, keyNodeID, providerNodeID)
	models, latency, discoveryErr := a.fetchDiscoveryCandidate(r.Context(), p)
	if persistErr := a.persistProviderKeyDiscovery(r.Context(), keyID, models, latency, discoveryErr); persistErr != nil && discoveryErr == nil {
		discoveryErr = persistErr
	}
	result := providerKeyDiscoveryResult{KeyID: keyID, KeyName: keyName, KeyHint: keyHint, LatencyMS: latency}
	if discoveryErr != nil {
		result.Error = sanitizeError(discoveryErr.Error())
		fail(w, discoveryErrorStatus(discoveryErr), "model_discovery_failed", result.Error)
		return
	}
	available := make([]discoveredModel, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		model.ID = normalizedModelID(model.ID)
		if model.ID == "" || model.Capabilities == "unsupported" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		available = append(available, model)
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	result.Discovered = len(available)
	result.LastDiscoveredAt = now()
	a.resetProviderKeyRuntime(keyID)
	writeJSON(w, http.StatusOK, map[string]any{"key": result, "models": available})
}

func (a *App) testProviderKey(w http.ResponseWriter, r *http.Request, providerID, keyID int64) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var providerType, baseURL, defaultModel string
	var providerNodeID, keyNodeID sql.NullInt64
	var encrypted []byte
	var keyModel, egressMode string
	if err := a.db.QueryRow(`SELECT p.type,p.base_url,p.default_model,p.ip_pool_node_id,k.credential,k.model,k.egress_mode,k.ip_pool_node_id FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.id=? AND k.provider_id=?`, keyID, providerID).Scan(&providerType, &baseURL, &defaultModel, &providerNodeID, &encrypted, &keyModel, &egressMode, &keyNodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusNotFound, "not_found", "API key card not found")
		} else {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
		}
		return
	}
	raw, err := a.decrypt(encrypted)
	if err != nil {
		fail(w, http.StatusInternalServerError, "credential_error", "API key could not be decrypted")
		return
	}
	nodeID, _ := effectiveProviderKeyNode(egressMode, keyNodeID, providerNodeID)
	p := discoveryProvider{ID: providerID, Type: providerType, BaseURL: baseURL, Credential: raw, RequestTimeoutMS: 20000, IPPoolNodeID: nodeID}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	start := time.Now()
	models, testErr := a.fetchDiscoveredModels(ctx, p)
	latency := time.Since(start).Milliseconds()
	if persistErr := a.persistProviderKeyDiscovery(r.Context(), keyID, models, latency, testErr); persistErr != nil && testErr == nil {
		testErr = persistErr
	}
	status, message := "healthy", ""
	effectiveModel := firstNonEmpty(keyModel, defaultModel)
	if testErr == nil && effectiveModel != "" {
		found := false
		for _, model := range models {
			if strings.EqualFold(model.UpstreamID, effectiveModel) || strings.EqualFold(model.ID, effectiveModel) {
				found = true
				break
			}
		}
		if !found {
			testErr = fmt.Errorf("configured model %q was not returned by the upstream", effectiveModel)
		}
	}
	if testErr != nil {
		status, message = "failed", sanitizeError(testErr.Error())
	}
	_, _ = a.db.Exec(`UPDATE provider_api_keys SET status=?,last_error=?,last_tested_at=?,last_test_latency_ms=?,updated_at=? WHERE id=? AND provider_id=?`, status, message, now(), latency, now(), keyID, providerID)
	if testErr != nil {
		fail(w, http.StatusBadGateway, "provider_key_test_failed", message)
		return
	}
	a.resetProviderKeyRuntime(keyID)
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "latency_ms": latency, "model_count": len(models), "effective_model": effectiveModel})
}

func parseProviderKeyPath(parts []string) (keyID int64, action string, ok bool) {
	if len(parts) < 2 || parts[0] != "keys" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 1 {
		return 0, "", false
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	return id, action, len(parts) <= 3
}
