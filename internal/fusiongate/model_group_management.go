package fusiongate

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// renameModelGroup changes a whole public model group without widening downstream permissions.
func (a *App) renameModelGroup(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		OldName      string `json:"old_name"`
		NewName      string `json:"new_name"`
		KeepOldAlias bool   `json:"keep_old_alias"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	oldName, newName := normalizeModelAlias(in.OldName), normalizeModelAlias(in.NewName)
	if oldName == "" || newName == "" || oldName == newName {
		fail(w, 400, "invalid_model_name", "distinct old_name and new_name are required")
		return
	}
	// These names become exact access rules; glob syntax would change unrelated model permissions.
	if strings.ContainsAny(oldName+newName, "*,?[]\r\n") {
		fail(w, 400, "invalid_model_name", "model group names cannot contain permission pattern delimiters")
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	defer tx.Rollback()
	var count, conflict int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_routes WHERE LOWER(public_name)=?`, oldName).Scan(&count); err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	if count == 0 {
		fail(w, 404, "not_found", "model group not found")
		return
	}
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM model_routes WHERE LOWER(public_name)=?)+(SELECT COUNT(*) FROM model_aliases WHERE LOWER(alias)=?)`, newName, newName).Scan(&conflict); err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	if conflict > 0 {
		fail(w, 409, "model_name_conflict", "new name conflicts with an existing group or alias")
		return
	}
	rows, err := tx.QueryContext(ctx, `SELECT alias FROM model_aliases WHERE LOWER(target_model)=? AND enabled=1`, oldName)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	aliases := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			fail(w, 500, "database_error", err.Error())
			return
		}
		aliases = append(aliases, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	type rule struct {
		id          int64
		all         int
		allow, deny string
	}
	rules := []rule{}
	rows, err = tx.QueryContext(ctx, `SELECT id,allow_all,allow_models,deny_models FROM api_keys`)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	for rows.Next() {
		var v rule
		if err = rows.Scan(&v.id, &v.all, &v.allow, &v.deny); err != nil {
			rows.Close()
			fail(w, 500, "database_error", err.Error())
			return
		}
		rules = append(rules, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	changed := 0
	for _, v := range rules {
		before := authKey{AllowAll: v.all != 0, AllowModels: v.allow, DenyModels: v.deny}
		after := before
		if matches(before.DenyModels, oldName) {
			after.DenyModels = appendModelPermission(after.DenyModels, newName)
		}
		if modelAllowed(before, oldName, oldName) && !before.AllowAll {
			after.AllowModels = appendModelPermission(after.AllowModels, newName)
		}
		if !modelAllowed(before, oldName, oldName) && modelAllowed(after, newName, newName) {
			after.DenyModels = appendModelPermission(after.DenyModels, newName)
		}
		valid := modelAllowed(before, oldName, oldName) == modelAllowed(after, newName, newName)
		for _, alias := range aliases {
			valid = valid && modelAllowed(before, alias, oldName) == modelAllowed(after, alias, newName)
		}
		if in.KeepOldAlias {
			valid = valid && modelAllowed(before, oldName, oldName) == modelAllowed(after, oldName, newName)
		}
		if !valid {
			fail(w, 409, "model_permission_conflict", "rename conflicts with downstream allow/deny patterns; adjust affected access rules before retrying")
			return
		}
		if after.AllowModels != before.AllowModels || after.DenyModels != before.DenyModels {
			if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET allow_models=?,deny_models=? WHERE id=?`, after.AllowModels, after.DenyModels, v.id); err != nil {
				fail(w, 500, "database_error", err.Error())
				return
			}
			changed++
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_routes SET public_name=?,updated_at=? WHERE LOWER(public_name)=?`, newName, now(), oldName); err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_aliases SET target_model=?,updated_at=? WHERE LOWER(target_model)=?`, newName, now(), oldName); err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	if in.KeepOldAlias {
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES(?,?,1,?,?)`, oldName, newName, now(), now()); err != nil {
			fail(w, 409, "model_name_conflict", err.Error())
			return
		}
	}
	if err = tx.Commit(); err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	a.routeMu.Lock()
	a.forgetRouteCursorsLocked(oldName)
	a.forgetRouteCursorsLocked(newName)
	a.routeMu.Unlock()
	writeJSON(w, 200, map[string]any{"old_name": oldName, "new_name": newName, "routes_updated": count, "keys_updated": changed, "old_alias_kept": in.KeepOldAlias})
}

func appendModelPermission(list, name string) string {
	if matches(list, name) {
		return list
	}
	if strings.TrimSpace(list) == "" {
		return name
	}
	return list + "," + name
}

type routeModelSupport struct {
	SupportedKeyCount int    `json:"supported_key_count"`
	AvailableKeyCount int    `json:"available_key_count"`
	ProviderEnabled   bool   `json:"provider_enabled"`
	AuthKind          string `json:"auth_kind"`
	Warning           string `json:"warning,omitempty"`
}

// Preview uses the actual key-selection policy, never the mere presence of a model in discovery.
func (a *App) routeModelPreview(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	var in struct {
		ProviderID    int64  `json:"provider_id"`
		UpstreamModel string `json:"upstream_model"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	if in.ProviderID < 1 || strings.TrimSpace(in.UpstreamModel) == "" {
		fail(w, 400, "invalid_request", "provider_id and upstream_model are required")
		return
	}
	var p Provider
	var enabled, archived, initialized int
	var authKind string
	err := a.reader().QueryRowContext(r.Context(), `SELECT id,auth_kind,enabled,archived,default_model,multi_key_initialized,COALESCE(circuit_open_until,'') FROM providers WHERE id=?`, in.ProviderID).Scan(&p.ID, &authKind, &enabled, &archived, &p.DefaultModel, &initialized, &p.CircuitOpenUntil)
	if err == sql.ErrNoRows {
		fail(w, 404, "not_found", "provider not found")
		return
	}
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	support := routeModelSupport{ProviderEnabled: enabled != 0 && archived == 0, AuthKind: authKind}
	if authKind != "api_key" {
		support.Warning = "OAuth 模型支持由上游决定，未发起检活"
		writeJSON(w, 200, support)
		return
	}
	rows, err := a.reader().QueryContext(r.Context(), `SELECT id,model_policy,model_allowlist,model,enabled,COALESCE(cooldown_until,'') FROM provider_api_keys WHERE provider_id=?`, p.ID)
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	type keyPolicy struct {
		id                   int64
		policy, allow, fixed string
		enabled              int
		cooldown             string
	}
	keys := []keyPolicy{}
	for rows.Next() {
		var key keyPolicy
		if err = rows.Scan(&key.id, &key.policy, &key.allow, &key.fixed, &key.enabled, &key.cooldown); err != nil {
			rows.Close()
			fail(w, 500, "database_error", err.Error())
			return
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "database_error", err.Error())
		return
	}
	for _, key := range keys {
		inventory, exclusions, e := a.providerKeyModelSets(r.Context(), key.id)
		if e != nil {
			fail(w, 500, "database_error", e.Error())
			return
		}
		if !providerKeySupportsModel(key.policy, key.allow, key.fixed, p.DefaultModel, in.UpstreamModel, inventory, exclusions) {
			continue
		}
		support.SupportedKeyCount++
		if key.enabled != 0 && support.ProviderEnabled && !isFuture(key.cooldown) && !isFuture(p.CircuitOpenUntil) {
			support.AvailableKeyCount++
		}
	}
	if initialized == 0 && len(keys) == 0 {
		support.Warning = "旧渠道凭据尚未迁移，无法确认 Key 支持数"
	} else if support.SupportedKeyCount == 0 {
		support.Warning = "没有 Key 支持此上游模型，请在渠道模型管理中手动添加"
	} else if support.AvailableKeyCount == 0 {
		support.Warning = "支持的 Key 或渠道已停用/冷却，当前不能调度"
	}
	writeJSON(w, 200, support)
}

func isFuture(value string) bool { t := parseTime(value); return t != nil && t.After(time.Now()) }
