package fusiongate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Setup is explicit: an empty selection grants no model permissions. Older
// clients omitting setup retain their existing create/discovery behaviour.
type providerModelSetup struct {
	Inventory []setupModelEntry `json:"inventory"`
	Selected  []string          `json:"selected"`
}

type setupModelEntry struct {
	Model        string `json:"model"`
	DisplayName  string `json:"display_name"`
	Capabilities string `json:"capabilities"`
	Source       string `json:"source"`
}

func validateModelSetup(setup *providerModelSetup) error {
	if setup == nil {
		return nil
	}
	if len(setup.Inventory) > 5000 || len(setup.Selected) > 5000 {
		return errors.New("最多配置 5000 个模型")
	}
	seen := map[string]bool{}
	for i := range setup.Inventory {
		m := &setup.Inventory[i]
		m.Model = strings.TrimSpace(m.Model)
		id := normalizeProviderKeyModel(m.Model)
		if !validManualModelName(m.Model) || seen[id] || len(m.DisplayName) > 512 || len(m.Capabilities) > 2048 || m.Capabilities == "unsupported" {
			return errors.New("模型清单包含无效或重复名称")
		}
		seen[id] = true
	}
	selected := map[string]bool{}
	for _, m := range setup.Selected {
		id := normalizeProviderKeyModel(m)
		if !seen[id] || selected[id] {
			return errors.New("请选择当前清单中的模型，且不要重复")
		}
		selected[id] = true
	}
	return nil
}

func (a *App) applyModelSetupTx(ctx context.Context, tx *sql.Tx, providerID, keyID int64, setup *providerModelSetup) (int, error) {
	if setup == nil {
		return 0, nil
	}
	selected := map[string]bool{}
	for _, m := range setup.Selected {
		selected[normalizeProviderKeyModel(m)] = true
	}
	for _, m := range setup.Inventory {
		source, manualName, manualCaps := "discovered", "", ""
		if m.Source != "discovered" {
			source, manualName, manualCaps = "manual", m.DisplayName, m.Capabilities
		}
		caps := strings.TrimSpace(m.Capabilities)
		if caps == "" {
			caps = "chat,stream"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at,model_source,manual_display_name,manual_capabilities) VALUES(?,?,?,?,?,?,?,?,?)`, keyID, m.Model, m.DisplayName, caps, boolInt(selected[normalizeProviderKeyModel(m.Model)]), now(), source, manualName, manualCaps); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist=? WHERE id=?`, normalizeProviderKeyAllowlist(strings.Join(setup.Selected, ",")), keyID); err != nil {
		return 0, err
	}
	if len(setup.Selected) == 0 {
		return 0, nil
	}
	return a.syncProviderModelRoutesScoped(ctx, tx, providerID, setup.Selected, true)
}

// Only a keyed digest is persisted, never the plaintext request or credentials.
func (a *App) onboardingDigest(in any) string { raw, _ := json.Marshal(in); return a.sign(string(raw)) }
func (a *App) replayOnboarding(ctx context.Context, tx *sql.Tx, w http.ResponseWriter, requestID, digest string, keyResource bool) bool {
	if requestID == "" {
		return false
	}
	var old string
	var providerID, keyID sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT digest,provider_id,key_id FROM provider_setup_requests WHERE request_id=?`, requestID).Scan(&old, &providerID, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		fail(w, 500, "database_error", "无法核对上次保存结果")
		return true
	}
	if old != digest || !providerID.Valid || !keyID.Valid {
		fail(w, 409, "setup_request_conflict", "此保存请求已使用或资源已删除，请重新打开表单核对")
		return true
	}
	id := providerID.Int64
	if keyResource {
		id = keyID.Int64
	}
	writeJSON(w, 200, map[string]any{"id": id, "provider_id": providerID.Int64, "key_id": keyID.Int64, "replayed": true})
	return true
}
func rememberOnboarding(ctx context.Context, tx *sql.Tx, requestID, digest string, providerID, keyID int64) error {
	if requestID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_setup_requests(request_id,digest,provider_id,key_id,created_at) VALUES(?,?,?,?,?)`, requestID, digest, providerID, keyID, now())
	return err
}

// Preview accepts unsaved connection inputs, uses the same upstream validation
// and egress transport as discovery, and never writes channels or permissions.
func (a *App) previewProviderModels(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		Type         string `json:"type"`
		BaseURL      string `json:"baseURL"`
		Credential   string `json:"credential"`
		IPPoolNodeID *int64 `json:"ip_pool_node_id"`
		ProviderID   int64  `json:"provider_id"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "invalid_request", "连接信息无效")
		return
	}
	in.Credential = strings.TrimSpace(in.Credential)
	if in.ProviderID > 0 {
		var authKind string
		if err := a.db.QueryRowContext(r.Context(), `SELECT type,base_url,ip_pool_node_id,auth_kind FROM providers WHERE id=?`, in.ProviderID).Scan(&in.Type, &in.BaseURL, &in.IPPoolNodeID, &authKind); err != nil || authKind != "api_key" {
			fail(w, 400, "invalid_provider", "请选择 API Key 渠道")
			return
		}
	}
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if !validProviderType(in.Type) || in.Credential == "" {
		fail(w, 400, "invalid_request", "请填写类型、API 地址与 Key")
		return
	}
	if err := validateUpstream(in.BaseURL, a.cfg); err != nil {
		fail(w, 400, "unsafe_upstream", err.Error())
		return
	}
	if in.IPPoolNodeID != nil && *in.IPPoolNodeID > 0 {
		if err := a.validateIPPoolNode(*in.IPPoolNodeID); err != nil {
			fail(w, 400, "invalid_ip_pool_node", err.Error())
			return
		}
	} else {
		in.IPPoolNodeID = nil
	}
	models, _, err := a.fetchDiscoveryCandidate(r.Context(), discoveryProvider{Type: in.Type, BaseURL: in.BaseURL, Credential: in.Credential, IPPoolNodeID: in.IPPoolNodeID, RequestTimeoutMS: 20000})
	if err != nil {
		fail(w, discoveryErrorStatus(err), "model_discovery_failed", safeDiscoveryError(err, in.Credential))
		return
	}
	inventory := make([]setupModelEntry, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		name := strings.TrimSpace(firstNonEmpty(m.UpstreamID, m.ID))
		id := normalizeProviderKeyModel(name)
		if !validManualModelName(name) || m.Capabilities == "unsupported" || seen[id] {
			continue
		}
		seen[id] = true
		inventory = append(inventory, setupModelEntry{Model: name, DisplayName: m.DisplayName, Capabilities: m.Capabilities, Source: "discovered"})
	}
	writeJSON(w, 200, map[string]any{"inventory": inventory, "message": "仅获取模型列表，未验证推理能力"})
}
