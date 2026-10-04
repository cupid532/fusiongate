package fusiongate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type manualModelEntry struct {
	Model        string `json:"model"`
	DisplayName  string `json:"display_name"`
	Capabilities string `json:"capabilities"`
}
type manualModelsInput struct {
	Entries       []manualModelEntry `json:"entries"`
	KeyIDs        []int64            `json:"key_ids"`
	OriginalModel string             `json:"original_model"`
	SyncRoutes    bool               `json:"sync_routes"`
	CreateRoutes  bool               `json:"create_routes"`
	PublicName    string             `json:"public_name"`
	Enabled       *bool              `json:"enabled"`
}
type manualModelsResult struct {
	Keys          int `json:"keys"`
	Models        int `json:"models"`
	RoutesCreated int `json:"routes_created"`
	RoutesUpdated int `json:"routes_updated"`
}
type manualModelError struct {
	status        int
	code, message string
}

func (e *manualModelError) Error() string { return e.message }
func manualModelConflict(message string) error {
	return &manualModelError{http.StatusConflict, "model_conflict", message}
}
func manualModelInvalid(message string) error {
	return &manualModelError{http.StatusBadRequest, "invalid_model", message}
}

// Model identifiers are matched case-insensitively for legacy compatibility,
// but the configured spelling is retained for forwarding to the upstream.
func validManualModelName(name string) bool {
	if name == "" || len(name) > 512 {
		return false
	}
	for _, c := range name {
		if unicode.IsSpace(c) || unicode.IsControl(c) || c == ',' {
			return false
		}
	}
	return true
}
func (a *App) providerManualModels(w http.ResponseWriter, r *http.Request, providerID int64) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	var in manualModelsInput
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "invalid_request", err.Error())
		return
	}
	out, err := a.saveManualModels(r.Context(), providerID, in)
	if err != nil {
		var clientErr *manualModelError
		if errors.As(err, &clientErr) {
			fail(w, clientErr.status, clientErr.code, clientErr.message)
		} else {
			fail(w, 500, "database_error", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (a *App) saveManualModels(ctx context.Context, providerID int64, in manualModelsInput) (manualModelsResult, error) {
	out := manualModelsResult{}
	if len(in.KeyIDs) == 0 || len(in.KeyIDs) > providerKeySoftLimit || len(in.Entries) == 0 || len(in.Entries) > 500 {
		return out, manualModelInvalid("key_ids and entries are required (maximum 500 each)")
	}
	in.OriginalModel = strings.TrimSpace(in.OriginalModel)
	in.PublicName = strings.ToLower(strings.TrimSpace(in.PublicName))
	if (in.OriginalModel != "" || in.PublicName != "") && len(in.Entries) != 1 {
		return out, manualModelInvalid("original_model/public_name require one model entry")
	}
	if in.OriginalModel != "" && !validManualModelName(in.OriginalModel) {
		return out, manualModelInvalid("invalid original_model")
	}
	if in.PublicName != "" && !validManualModelName(in.PublicName) {
		return out, manualModelInvalid("invalid public_name")
	}
	seen := map[string]bool{}
	for i := range in.Entries {
		e := &in.Entries[i]
		e.Model = strings.TrimSpace(e.Model)
		e.DisplayName = strings.TrimSpace(e.DisplayName)
		e.Capabilities = strings.TrimSpace(e.Capabilities)
		id := normalizeProviderKeyModel(e.Model)
		if !validManualModelName(e.Model) || seen[id] {
			return out, manualModelInvalid("empty, invalid or duplicate model entry")
		}
		seen[id] = true
		if len(e.DisplayName) > 512 {
			return out, manualModelInvalid("display_name too long")
		}
		if e.Capabilities == "" {
			e.Capabilities = "chat,stream"
		}
		for _, c := range strings.Split(e.Capabilities, ",") {
			switch strings.TrimSpace(c) {
			case "chat", "stream", "tools", "vision", "image", "images", "embedding", "embeddings", "responses", "messages", "audio", "reasoning":
			default:
				return out, manualModelInvalid("unsupported capability: " + c)
			}
		}
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var authKind string
	if err = tx.QueryRowContext(ctx, `SELECT auth_kind FROM providers WHERE id=?`, providerID).Scan(&authKind); errors.Is(err, sql.ErrNoRows) {
		return out, &manualModelError{404, "not_found", "provider not found"}
	} else if err != nil {
		return out, err
	}
	if authKind != "api_key" {
		return out, manualModelInvalid("manual Key models require an API-key provider")
	}
	keySet := map[int64]bool{}
	for _, keyID := range in.KeyIDs {
		if keyID < 1 || keySet[keyID] {
			return out, manualModelInvalid("invalid or duplicate key_id")
		}
		keySet[keyID] = true
		var fixed string
		if err = tx.QueryRowContext(ctx, `SELECT model FROM provider_api_keys WHERE id=? AND provider_id=?`, keyID, providerID).Scan(&fixed); errors.Is(err, sql.ErrNoRows) {
			return out, &manualModelError{404, "key_not_found", "Key does not belong to this provider"}
		} else if err != nil {
			return out, err
		}
		if fixed != "" {
			for _, e := range in.Entries {
				if normalizeProviderKeyModel(e.Model) != normalizeProviderKeyModel(fixed) && normalizeProviderKeyModel(in.OriginalModel) != normalizeProviderKeyModel(fixed) {
					return out, manualModelConflict("Key " + strconv.FormatInt(keyID, 10) + " is restricted to a fixed model")
				}
			}
		}
	}
	oldID := normalizeProviderKeyModel(in.OriginalModel)
	if oldID != "" && in.SyncRoutes && in.OriginalModel != in.Entries[0].Model {
		rows, e := tx.QueryContext(ctx, `SELECT k.id,k.model_policy,k.model_allowlist,EXISTS(SELECT 1 FROM provider_api_key_models m WHERE m.provider_key_id=k.id AND lower(m.model)=?) OR lower(k.model)=? FROM provider_api_keys k WHERE k.provider_id=?`, oldID, oldID, providerID)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id int64
			var policy, allowlist string
			var holds int
			if e = rows.Scan(&id, &policy, &allowlist, &holds); e != nil {
				rows.Close()
				return out, e
			}
			if policy == "allowlist" {
				for _, model := range strings.Split(normalizeProviderKeyAllowlist(allowlist), ",") {
					if model == oldID {
						holds = 1
					}
				}
			}
			if holds != 0 && !keySet[id] {
				rows.Close()
				return out, manualModelConflict("rename with sync_routes must include every Key holding the old model")
			}
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return out, e
		}
		rows.Close()
	}
	for _, keyID := range in.KeyIDs {
		var policy, allowlist, fixed string
		if err = tx.QueryRowContext(ctx, `SELECT model_policy,model_allowlist,model FROM provider_api_keys WHERE id=?`, keyID).Scan(&policy, &allowlist, &fixed); err != nil {
			return out, err
		}
		allowed := map[string]bool{}
		for _, m := range strings.Split(normalizeProviderKeyAllowlist(allowlist), ",") {
			if m != "" {
				allowed[m] = true
			}
		}
		for _, e := range in.Entries {
			targetID := normalizeProviderKeyModel(e.Model)
			lookup := targetID
			if oldID != "" {
				lookup = oldID
			}
			var currentName, source string
			er := tx.QueryRowContext(ctx, `SELECT model,model_source FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)=?`, keyID, lookup).Scan(&currentName, &source)
			exists := er == nil
			if er != nil && !errors.Is(er, sql.ErrNoRows) {
				return out, er
			}
			if oldID != "" && !exists {
				return out, &manualModelError{404, "model_not_found", fmt.Sprintf("original model not found on Key %d", keyID)}
			}
			if oldID != "" && oldID != targetID {
				var n int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM provider_api_key_models WHERE provider_key_id=? AND lower(model)=?`, keyID, targetID).Scan(&n); err != nil {
					return out, err
				}
				if n > 0 {
					return out, manualModelConflict("target model already exists on a selected Key")
				}
			}
			if !exists {
				source = "manual"
			} else if source == "discovered" {
				source = "both"
			} else if source == "" {
				source = "both"
			}
			display := e.DisplayName
			if display == "" {
				display = e.Model
			}
			if exists {
				_, err = tx.ExecContext(ctx, `UPDATE provider_api_key_models SET model=?,display_name=?,capabilities=?,enabled=?,model_source=?,manual_display_name=?,manual_capabilities=? WHERE provider_key_id=? AND model=?`, e.Model, display, e.Capabilities, boolInt(enabled), source, display, e.Capabilities, keyID, currentName)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,enabled,discovered_at,model_source,manual_display_name,manual_capabilities) VALUES(?,?,?,?,?,?,?,?,?)`, keyID, e.Model, display, e.Capabilities, boolInt(enabled), now(), source, display, e.Capabilities)
			}
			if err != nil {
				return out, err
			}
			if oldID != "" && oldID != targetID {
				delete(allowed, oldID)
				if _, err = tx.ExecContext(ctx, `DELETE FROM provider_api_key_model_health WHERE provider_key_id=? AND lower(model)=?`, keyID, oldID); err != nil {
					return out, err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,?,?) ON CONFLICT(provider_key_id,model) DO NOTHING`, keyID, oldID, now()); err != nil {
					return out, err
				}
				if normalizeProviderKeyModel(fixed) == oldID {
					fixed = e.Model
				}
			}
			if enabled {
				allowed[targetID] = true
			} else {
				delete(allowed, targetID)
			}
			if enabled {
				_, err = tx.ExecContext(ctx, `DELETE FROM provider_api_key_model_exclusions WHERE provider_key_id=? AND lower(model)=?`, keyID, targetID)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO provider_api_key_model_exclusions(provider_key_id,model,created_at) VALUES(?,?,?) ON CONFLICT(provider_key_id,model) DO NOTHING`, keyID, targetID, now())
			}
			if err != nil {
				return out, err
			}
		}
		values := []string{}
		for m := range allowed {
			values = append(values, m)
		}
		// Stable order is not semantically significant, but deterministic backups are.
		sort.Strings(values)
		if _, err = tx.ExecContext(ctx, `UPDATE provider_api_keys SET model_allowlist=?,model=?,updated_at=? WHERE id=?`, strings.Join(values, ","), fixed, now(), keyID); err != nil {
			return out, err
		}
	}
	if oldID != "" && in.SyncRoutes {
		e := in.Entries[0]
		var duplicate int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM model_routes a JOIN model_routes b ON a.provider_id=b.provider_id AND a.public_name=b.public_name AND a.id<>b.id WHERE a.provider_id=? AND lower(a.upstream_model)=? AND lower(b.upstream_model)=?`, providerID, oldID, normalizeProviderKeyModel(e.Model)).Scan(&duplicate); err != nil {
			return out, err
		}
		if duplicate > 0 {
			return out, manualModelConflict("rename would duplicate an existing route")
		}
		res, er := tx.ExecContext(ctx, `UPDATE model_routes SET upstream_model=?,capabilities=?,updated_at=? WHERE provider_id=? AND lower(upstream_model)=?`, e.Model, e.Capabilities, now(), providerID, oldID)
		if er != nil {
			return out, er
		}
		n, _ := res.RowsAffected()
		out.RoutesUpdated = int(n)
	}
	if in.CreateRoutes {
		for _, e := range in.Entries {
			public := in.PublicName
			if public == "" {
				public = strings.ToLower(e.Model)
			}
			var alias int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM model_aliases WHERE lower(alias)=?`, public).Scan(&alias); err != nil {
				return out, err
			}
			if alias > 0 {
				return out, manualModelConflict("request model name conflicts with a call alias")
			}
			res, er := tx.ExecContext(ctx, `INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,created_at,updated_at) SELECT ?,?,?,?, ?,0,(SELECT COALESCE(max(sort_order),-1)+1 FROM model_routes WHERE public_name=?),?,? WHERE NOT EXISTS(SELECT 1 FROM model_routes WHERE provider_id=? AND lower(public_name)=? AND lower(upstream_model)=?)`, public, providerID, e.Model, e.Capabilities, boolInt(enabled), public, now(), now(), providerID, public, normalizeProviderKeyModel(e.Model))
			if er != nil {
				return out, er
			}
			n, _ := res.RowsAffected()
			out.RoutesCreated += int(n)
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	a.resetProviderKeyRoundRobin(providerID)
	if out.RoutesCreated > 0 {
		a.triggerPricingSync()
	}
	out.Keys = len(in.KeyIDs)
	out.Models = len(in.Entries)
	return out, nil
}
