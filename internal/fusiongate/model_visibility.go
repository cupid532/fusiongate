package fusiongate

import (
	"context"
	"database/sql"
)

// Model visibility follows configured permissions, not transient cooldowns or
// health checks. Keep unsupported route definitions so granting access later
// restores mappings without changing their names, ordering or pricing.
type modelVisibilityKey struct {
	policy, allowlist, fixed, defaultModel string
	inventory, exclusions                  map[string]bool
}

type modelVisibility map[int64][]*modelVisibilityKey

func (a *App) loadModelVisibility(ctx context.Context) (modelVisibility, error) {
	tx, err := a.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	visibility := modelVisibility{}
	keys := map[int64]*modelVisibilityKey{}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,k.id,k.model_policy,k.model_allowlist,k.model,p.default_model FROM providers p LEFT JOIN provider_api_keys k ON k.provider_id=p.id AND k.enabled=1 WHERE p.auth_kind='api_key' AND p.multi_key_initialized=1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var providerID int64
		var keyID *int64
		var policy, allowlist, fixed *string
		var defaultModel string
		if err := rows.Scan(&providerID, &keyID, &policy, &allowlist, &fixed, &defaultModel); err != nil {
			rows.Close()
			return nil, err
		}
		if _, exists := visibility[providerID]; !exists {
			visibility[providerID] = nil
		}
		if keyID != nil {
			key := &modelVisibilityKey{policy: *policy, allowlist: *allowlist, fixed: *fixed, defaultModel: defaultModel, inventory: map[string]bool{}, exclusions: map[string]bool{}}
			keys[*keyID] = key
			visibility[providerID] = append(visibility[providerID], key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT m.provider_key_id,m.model,m.enabled,0 FROM provider_api_key_models m JOIN provider_api_keys k ON k.id=m.provider_key_id WHERE k.enabled=1 UNION ALL SELECT e.provider_key_id,e.model,0,1 FROM provider_api_key_model_exclusions e JOIN provider_api_keys k ON k.id=e.provider_key_id WHERE k.enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var keyID int64
		var model string
		var enabled, excluded int
		if err := rows.Scan(&keyID, &model, &enabled, &excluded); err != nil {
			return nil, err
		}
		if key := keys[keyID]; key != nil {
			model = normalizeProviderKeyModel(model)
			if excluded != 0 {
				key.exclusions[model] = true
			} else {
				key.inventory[model] = strBool(enabled)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return visibility, tx.Commit()
}

func (visibility modelVisibility) supports(providerID int64, upstreamModel string) bool {
	keys, initialized := visibility[providerID]
	if !initialized {
		return true // OAuth and legacy single-credential channels retain their rules.
	}
	for _, key := range keys {
		if providerKeySupportsModel(key.policy, key.allowlist, key.fixed, key.defaultModel, upstreamModel, key.inventory, key.exclusions) {
			return true
		}
	}
	return false
}

func (a *App) visibleModelCounts(ctx context.Context, visibility modelVisibility) (map[int64]int, int, error) {
	rows, err := a.reader().QueryContext(ctx, `SELECT r.provider_id,r.public_name,r.upstream_model,p.enabled,p.archived FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE r.enabled=1`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[int64]int{}
	public := map[string]bool{}
	for rows.Next() {
		var providerID int64
		var name, upstream string
		var enabled, archived int
		if err := rows.Scan(&providerID, &name, &upstream, &enabled, &archived); err != nil {
			return nil, 0, err
		}
		if visibility.supports(providerID, upstream) {
			counts[providerID]++
			if enabled != 0 && archived == 0 {
				public[name] = true
			}
		}
	}
	return counts, len(public), rows.Err()
}
