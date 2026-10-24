package fusiongate

import (
	"context"
	"strings"
)

// Codex exposes image generation as a Responses tool, not necessarily as a
// separately listed model. Each candidate keeps its own outer model identity;
// discovery must never silently choose or change an operator's mapping.
func (a *App) codexAuthImageModels(ctx context.Context, id int64, models []discoveredModel) ([]discoveredModel, error) {
	existing := map[string]bool{}
	hosts := map[string]discoveredModel{}
	for _, m := range models {
		existing[m.ID] = true
		if strings.Contains(m.Capabilities, "chat") {
			hosts[normalizedModelID(m.UpstreamID)] = m
		}
	}
	rows, err := a.reader().QueryContext(ctx, `SELECT public_name,upstream_model FROM model_routes WHERE provider_id=? AND capabilities LIKE '%image%' AND public_name LIKE 'gpt-image-%'`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var alias, outer string
		if err := rows.Scan(&alias, &outer); err != nil {
			return nil, err
		}
		if _, ok := hosts[normalizedModelID(outer)]; !ok || existing[alias] {
			continue
		}
		models = append(models, discoveredModel{ID: alias, UpstreamID: outer, Capabilities: "image", DisplayName: "图片工具 → " + outer + "（原映射；生图未验证）"})
		existing[alias] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, host := range models {
		if !strings.Contains(host.Capabilities, "chat") || strings.Contains(host.Capabilities, "unsupported") {
			continue
		}
		alias := "gpt-image-tool-" + host.ID
		if existing[alias] {
			continue
		}
		label := "图片工具候选 → " + host.UpstreamID + "（上游未声明支持，生图未验证）"
		if host.ImageToolSupported {
			label = "图片工具 → " + host.UpstreamID + "（上游声明支持，生图未验证）"
		}
		models = append(models, discoveredModel{ID: alias, UpstreamID: host.UpstreamID, Capabilities: "image", RequiresSelection: true, DisplayName: label})
	}
	return models, nil
}
