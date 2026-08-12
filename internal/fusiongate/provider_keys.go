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

const providerKeySoftLimit = 500

const (
	providerKeyEgressInherit = "inherit"
	providerKeyEgressDirect  = "direct"
	providerKeyEgressNode    = "node"
)

type ProviderAPIKey struct {
	ID                int64              `json:"id"`
	ProviderID        int64              `json:"provider_id"`
	Name              string             `json:"name"`
	KeyHint           string             `json:"key_hint"`
	Model             string             `json:"model,omitempty"`
	EffectiveModel    string             `json:"effective_model,omitempty"`
	ModelInherited    bool               `json:"model_inherited"`
	EgressMode        string             `json:"egress_mode"`
	IPPoolNodeID      *int64             `json:"ip_pool_node_id,omitempty"`
	IPPoolNodeName    string             `json:"ip_pool_node_name,omitempty"`
	EffectiveEgress   string             `json:"effective_egress"`
	EffectiveNodeID   *int64             `json:"effective_node_id,omitempty"`
	EgressInherited   bool               `json:"egress_inherited"`
	Enabled           bool               `json:"enabled"`
	SortOrder         int                `json:"sort_order"`
	Status            string             `json:"status"`
	LastError         string             `json:"last_error,omitempty"`
	LastTestedAt      string             `json:"last_tested_at,omitempty"`
	LastTestLatencyMS int64              `json:"last_test_latency_ms"`
	DiscoveredModels  int                `json:"discovered_models"`
	LastDiscoveredAt  string             `json:"last_discovered_at,omitempty"`
	Models            []ProviderKeyModel `json:"models"`
	CreatedAt         string             `json:"created_at"`
	UpdatedAt         string             `json:"updated_at"`
}

type ProviderKeyModel struct {
	Model        string `json:"model"`
	DisplayName  string `json:"display_name"`
	Capabilities string `json:"capabilities"`
	Enabled      bool   `json:"enabled"`
}

type selectedProviderKey struct {
	ID           int64
	Credential   string
	Name         string
	Hint         string
	Model        string
	IPPoolNodeID *int64
}

func validProviderKeyEgressMode(mode string) bool {
	return mode == providerKeyEgressInherit || mode == providerKeyEgressDirect || mode == providerKeyEgressNode
}

func normalizeProviderKeyModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
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
			_, err = tx.ExecContext(ctx, `INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,'','inherit',1,0,'untested',?,?) ON CONFLICT(provider_id,fingerprint) DO NOTHING`, item.id, item.credential, a.providerKeyFingerprint(raw), providerKeyHint(raw), "默认 Key", now(), now())
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
	return nil
}

func (a *App) selectProviderKey(ctx context.Context, providerID int64, upstreamModel string, providerNodeID *int64, legacyCredential []byte, initialized bool) (selectedProviderKey, error) {
	keys, err := a.selectProviderKeys(ctx, providerID, upstreamModel, providerNodeID, legacyCredential, initialized)
	if err != nil {
		return selectedProviderKey{}, err
	}
	return keys[0], nil
}

func (a *App) selectProviderKeys(ctx context.Context, providerID int64, upstreamModel string, providerNodeID *int64, legacyCredential []byte, initialized bool) ([]selectedProviderKey, error) {
	if !initialized {
		raw, err := a.decrypt(legacyCredential)
		if err != nil {
			return nil, err
		}
		return []selectedProviderKey{{Credential: raw, Hint: providerKeyHint(raw), IPPoolNodeID: providerNodeID}}, nil
	}
	rows, err := a.db.QueryContext(ctx, `
SELECT k.id,k.credential,k.name,k.key_hint,k.model,k.egress_mode,k.ip_pool_node_id
FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id
WHERE k.provider_id=? AND k.enabled=1
  AND (
    (k.model<>'' AND lower(k.model)=lower(?))
    OR (k.model='' AND EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=k.id AND km.enabled=1 AND lower(km.model)=lower(?)))
    OR (k.model='' AND NOT EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=k.id) AND (p.default_model='' OR lower(p.default_model)=lower(?)))
  )
ORDER BY k.sort_order,k.id`, providerID, upstreamModel, upstreamModel, upstreamModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var providerNode sql.NullInt64
	if providerNodeID != nil {
		providerNode = sql.NullInt64{Int64: *providerNodeID, Valid: true}
	}
	selected := make([]selectedProviderKey, 0)
	for rows.Next() {
		var key selectedProviderKey
		var encrypted []byte
		var keyNodeID sql.NullInt64
		var egressMode string
		if err := rows.Scan(&key.ID, &encrypted, &key.Name, &key.Hint, &key.Model, &egressMode, &keyNodeID); err != nil {
			return nil, err
		}
		raw, err := a.decrypt(encrypted)
		if err != nil {
			return nil, err
		}
		key.Credential = raw
		key.IPPoolNodeID, _ = effectiveProviderKeyNode(egressMode, keyNodeID, providerNode)
		selected = append(selected, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, errors.New("no enabled API key supports this model")
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
	err := a.db.QueryRowContext(ctx, `SELECT k.credential,k.egress_mode,k.ip_pool_node_id,p.ip_pool_node_id FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.id=? AND k.provider_id=? AND k.enabled=1 AND (k.model<>'' AND lower(k.model)=lower(?) OR k.model='' AND EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=k.id AND km.enabled=1 AND lower(km.model)=lower(?)))`, keyID, p.ID, upstreamModel, upstreamModel).Scan(&encrypted, &mode, &keyNode, &providerNode)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("selected API key does not support this model")
	}
	if err != nil {
		return err
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
		rows, err := a.db.Query(`SELECT k.id,k.provider_id,k.name,k.key_hint,k.model,k.egress_mode,k.ip_pool_node_id,COALESCE(n.name,''),k.enabled,k.sort_order,k.status,k.last_error,COALESCE(k.last_tested_at,''),k.last_test_latency_ms,(SELECT COUNT(*) FROM provider_api_key_models km WHERE km.provider_key_id=k.id),COALESCE((SELECT MAX(discovered_at) FROM provider_api_key_models km WHERE km.provider_key_id=k.id),''),k.created_at,k.updated_at FROM provider_api_keys k LEFT JOIN ip_pool_nodes n ON n.id=k.ip_pool_node_id WHERE k.provider_id=? ORDER BY k.sort_order,k.id`, providerID)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []ProviderAPIKey{}
		for rows.Next() {
			var key ProviderAPIKey
			var enabled int
			var keyNodeID sql.NullInt64
			if err := rows.Scan(&key.ID, &key.ProviderID, &key.Name, &key.KeyHint, &key.Model, &key.EgressMode, &keyNodeID, &key.IPPoolNodeName, &enabled, &key.SortOrder, &key.Status, &key.LastError, &key.LastTestedAt, &key.LastTestLatencyMS, &key.DiscoveredModels, &key.LastDiscoveredAt, &key.CreatedAt, &key.UpdatedAt); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			key.Enabled = strBool(enabled)
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
			modelRows, modelErr := a.db.Query(`SELECT model,display_name,capabilities,enabled FROM provider_api_key_models WHERE provider_key_id=? ORDER BY model`, out[i].ID)
			if modelErr != nil {
				fail(w, http.StatusInternalServerError, "database_error", modelErr.Error())
				return
			}
			out[i].Models = []ProviderKeyModel{}
			for modelRows.Next() {
				var model ProviderKeyModel
				var modelEnabled int
				if modelRows.Scan(&model.Model, &model.DisplayName, &model.Capabilities, &modelEnabled) == nil {
					model.Enabled = strBool(modelEnabled)
					out[i].Models = append(out[i].Models, model)
				}
			}
			_ = modelRows.Close()
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			APIKey       string `json:"api_key"`
			Name         string `json:"name"`
			Model        string `json:"model"`
			EgressMode   string `json:"egress_mode"`
			IPPoolNodeID *int64 `json:"ip_pool_node_id"`
			Enabled      *bool  `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		in.APIKey = strings.TrimSpace(in.APIKey)
		in.Name = strings.TrimSpace(in.Name)
		in.Model = normalizeProviderKeyModel(in.Model)
		in.EgressMode = strings.ToLower(strings.TrimSpace(in.EgressMode))
		if in.EgressMode == "" {
			in.EgressMode = providerKeyEgressInherit
		}
		if in.APIKey == "" || !validProviderKeyEgressMode(in.EgressMode) {
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
		res, err := tx.Exec(`INSERT INTO provider_api_keys(provider_id,credential,fingerprint,key_hint,name,model,egress_mode,ip_pool_node_id,enabled,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?, 'untested',?,?)`, providerID, encrypted, a.providerKeyFingerprint(in.APIKey), providerKeyHint(in.APIKey), in.Name, in.Model, in.EgressMode, nodeArg, boolInt(enabled), nextOrder, now(), now())
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
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
			APIKey       *string   `json:"api_key"`
			Name         *string   `json:"name"`
			Model        *string   `json:"model"`
			EgressMode   *string   `json:"egress_mode"`
			IPPoolNodeID *int64    `json:"ip_pool_node_id"`
			Enabled      *bool     `json:"enabled"`
			SortOrder    *int      `json:"sort_order"`
			Models       *[]string `json:"models"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
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
		res, err := a.db.Exec(`UPDATE provider_api_keys SET credential=COALESCE(?,credential),fingerprint=COALESCE(?,fingerprint),key_hint=COALESCE(?,key_hint),name=COALESCE(?,name),model=COALESCE(?,model),egress_mode=COALESCE(?,egress_mode),ip_pool_node_id=CASE WHEN ? THEN ? ELSE ip_pool_node_id END,enabled=COALESCE(?,enabled),sort_order=COALESCE(?,sort_order),status=CASE WHEN ? THEN 'untested' ELSE status END,last_error=CASE WHEN ? THEN '' ELSE last_error END,updated_at=? WHERE id=? AND provider_id=?`, encryptedArg, fingerprintArg, hintArg, nameArg, modelArg, egressArg, in.EgressMode != nil || in.IPPoolNodeID != nil, nodeArg, maybeBool(in.Enabled), in.SortOrder, encryptedArg != nil || modelArg != nil || egressArg != nil || in.IPPoolNodeID != nil, encryptedArg != nil || modelArg != nil || egressArg != nil || in.IPPoolNodeID != nil, now(), keyID, providerID)
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
		if encryptedArg != nil {
			_, _ = a.db.Exec(`DELETE FROM provider_api_key_models WHERE provider_key_id=?`, keyID)
		}
		if in.Models != nil {
			selected := map[string]bool{}
			for _, model := range *in.Models {
				if normalized := normalizeProviderKeyModel(model); normalized != "" {
					selected[normalized] = true
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
