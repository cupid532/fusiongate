package fusiongate

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func normalizeModelAlias(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (a *App) canonicalModel(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", errors.New("model is required")
	}
	var direct int
	if err := a.reader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, requested).Scan(&direct); err != nil {
		return "", err
	}
	if direct != 0 {
		return requested, nil
	}
	var target string
	err := a.reader().QueryRowContext(ctx, `SELECT target_model FROM model_aliases WHERE alias=? AND enabled=1`, requested).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return requested, nil
	}
	if err != nil {
		return "", err
	}
	return target, nil
}

func modelAllowed(key authKey, requested, canonical string) bool {
	if matches(key.DenyModels, requested) || (canonical != requested && matches(key.DenyModels, canonical)) {
		return false
	}
	return key.AllowAll || matches(key.AllowModels, requested) || (canonical != requested && matches(key.AllowModels, canonical))
}

func exposeRequestedModel(routes []resolvedRoute, requested string) []resolvedRoute {
	out := routes[:0]
	for _, route := range routes {
		if route.Provider.PassthroughMode == "transparent" && requested != route.Route.UpstreamModel {
			continue
		}
		route.Route.PublicName = requested
		out = append(out, route)
	}
	return out
}

func (a *App) validateAliasTarget(ctx context.Context, alias, target string) error {
	if alias == target {
		return errors.New("alias and target model must differ")
	}
	var exists int
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, target).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return errors.New("target model does not exist")
	}
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.public_name=? AND p.passthrough_mode<>'transparent')`, target).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return errors.New("target model has no route that can safely rewrite an alias")
	}
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_routes WHERE public_name=?)`, alias).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return errors.New("alias conflicts with an existing public model")
	}
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_aliases WHERE alias=? AND alias<>?)`, target, alias).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return errors.New("target model must be a public model, not another alias")
	}
	return nil
}

func (a *App) modelAliases(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.reader().Query(`SELECT alias,target_model,enabled,created_at,updated_at FROM model_aliases ORDER BY alias`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		out := []ModelAlias{}
		for rows.Next() {
			var item ModelAlias
			var enabled int
			if err := rows.Scan(&item.Alias, &item.TargetModel, &enabled, &item.CreatedAt, &item.UpdatedAt); err != nil {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
				return
			}
			item.Enabled = strBool(enabled)
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Alias       string `json:"alias"`
			TargetModel string `json:"target_model"`
			Enabled     *bool  `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		alias := normalizeModelAlias(in.Alias)
		target := normalizeModelAlias(in.TargetModel)
		if alias == "" || target == "" || len(alias) > 255 || len(target) > 255 {
			fail(w, http.StatusBadRequest, "invalid_request", "alias and target_model are required and must not exceed 255 characters")
			return
		}
		if err := a.validateAliasTarget(r.Context(), alias, target); err != nil {
			fail(w, http.StatusBadRequest, "invalid_model_alias", err.Error())
			return
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		stamp := now()
		if _, err := a.db.ExecContext(r.Context(), `INSERT INTO model_aliases(alias,target_model,enabled,created_at,updated_at) VALUES(?,?,?,?,?)`, alias, target, boolInt(enabled), stamp, stamp); err != nil {
			fail(w, http.StatusConflict, "model_alias_conflict", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, ModelAlias{Alias: alias, TargetModel: target, Enabled: enabled, CreatedAt: stamp, UpdatedAt: stamp})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) modelAliasByName(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	encoded := strings.TrimPrefix(r.URL.Path, "/api/admin/model-aliases/")
	alias, err := url.PathUnescape(encoded)
	if err != nil || normalizeModelAlias(alias) == "" {
		fail(w, http.StatusNotFound, "not_found", "model alias not found")
		return
	}
	alias = normalizeModelAlias(alias)
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			TargetModel *string `json:"target_model"`
			Enabled     *bool   `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if in.TargetModel == nil && in.Enabled == nil {
			fail(w, http.StatusBadRequest, "invalid_request", "target_model or enabled is required")
			return
		}
		var currentTarget string
		if err := a.db.QueryRowContext(r.Context(), `SELECT target_model FROM model_aliases WHERE alias=?`, alias).Scan(&currentTarget); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fail(w, http.StatusNotFound, "not_found", "model alias not found")
			} else {
				fail(w, http.StatusInternalServerError, "database_error", err.Error())
			}
			return
		}
		target := currentTarget
		if in.TargetModel != nil {
			target = normalizeModelAlias(*in.TargetModel)
			if target == "" || len(target) > 255 {
				fail(w, http.StatusBadRequest, "invalid_request", "target_model is required and must not exceed 255 characters")
				return
			}
			if err := a.validateAliasTarget(r.Context(), alias, target); err != nil {
				fail(w, http.StatusBadRequest, "invalid_model_alias", err.Error())
				return
			}
		}
		res, err := a.db.ExecContext(r.Context(), `UPDATE model_aliases SET target_model=?,enabled=COALESCE(?,enabled),updated_at=? WHERE alias=?`, target, maybeBool(in.Enabled), now(), alias)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		updated, _ := res.RowsAffected()
		if updated == 0 {
			fail(w, http.StatusNotFound, "not_found", "model alias not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "alias": alias, "target_model": target})
	case http.MethodDelete:
		res, err := a.db.ExecContext(r.Context(), `DELETE FROM model_aliases WHERE alias=?`, alias)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		deleted, _ := res.RowsAffected()
		if deleted == 0 {
			fail(w, http.StatusNotFound, "not_found", "model alias not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH or DELETE required")
	}
}
