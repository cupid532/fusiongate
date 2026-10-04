package fusiongate

import (
	"context"
	"strings"
)

// modelDisplayName is presentation only; never use it for routing or forwarding.
func modelDisplayName(model, display string) string {
	if strings.TrimSpace(display) != "" {
		return display
	}
	model = strings.TrimSpace(model)
	if index := strings.LastIndex(model, "/"); index >= 0 && index < len(model)-1 {
		return model[index+1:]
	}
	return model
}

type providerModelDisplayNames map[int64]map[string]string

// Load once per provider, after closing route/key rows, rather than issuing one
// metadata lookup for every key/model probe. Inventory names are matched exactly
// as routing does, but the stored display spelling is preserved.
func (a *App) providerModelDisplayNames(ctx context.Context, providerID int64) (providerModelDisplayNames, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT m.provider_key_id,m.model,m.display_name,m.manual_display_name FROM provider_api_key_models m JOIN provider_api_keys k ON k.id=m.provider_key_id WHERE k.provider_id=? ORDER BY k.sort_order,k.id,m.model`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := providerModelDisplayNames{}
	for rows.Next() {
		var keyID int64
		var model, display, manual string
		if err := rows.Scan(&keyID, &model, &display, &manual); err != nil {
			return nil, err
		}
		if strings.TrimSpace(manual) != "" {
			display = manual
		}
		if names[keyID] == nil {
			names[keyID] = map[string]string{}
		}
		names[keyID][normalizeProviderKeyModel(model)] = modelDisplayName(model, display)
	}
	return names, rows.Err()
}

func (names providerModelDisplayNames) forKey(keyID int64, model string) string {
	return modelDisplayName(model, names[keyID][normalizeProviderKeyModel(model)])
}

// Unprobeable routes still show their configured label. Use a deterministic
// inventory entry when no eligible Key has been selected for the route.
func (names providerModelDisplayNames) forModel(model string) string {
	var firstID int64
	display := ""
	for keyID, models := range names {
		if value, ok := models[normalizeProviderKeyModel(model)]; ok && (firstID == 0 || keyID < firstID) {
			firstID, display = keyID, value
		}
	}
	return modelDisplayName(model, display)
}
