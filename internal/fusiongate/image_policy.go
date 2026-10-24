package fusiongate

import (
	"context"
	"errors"
	"strings"
)

func validateImagePolicy(kind, mode, outer string) error {
	switch mode {
	case "legacy", "disabled":
	case "native":
		if kind == "codex_oauth" {
			return errors.New("Codex native images are not verified; use Responses image tools")
		}
	case "responses":
		if kind != "codex_oauth" {
			return errors.New("Responses image tools currently require a Codex OAuth channel")
		}
		if strings.TrimSpace(outer) == "" || strings.ContainsAny(outer, "\r\n\t ") {
			return errors.New("Responses image tools require an explicit outer model identifier")
		}
	default:
		return errors.New("image_mode must be legacy, disabled, native or responses")
	}
	return nil
}

// imagePolicy checks actual channel-local route enablement at request time.
// No cheapest-model selection, implicit re-enablement or guessed fallback.
func (a *App) imagePolicy(z resolvedRoute) (resolvedRoute, error) {
	var mode, outer string
	if err := a.reader().QueryRow(`SELECT image_mode,image_outer_model FROM providers WHERE id=?`, z.Provider.ID).Scan(&mode, &outer); err != nil {
		return z, err
	}
	if mode == "disabled" {
		return z, errors.New("image generation disabled for this channel")
	}
	if mode == "legacy" && z.Provider.Type == "codex_oauth" {
		outer = z.Route.UpstreamModel
		var disabled, enabled int
		if err := a.reader().QueryRow(`SELECT COALESCE(SUM(CASE WHEN enabled=0 THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN enabled=1 THEN 1 ELSE 0 END),0) FROM model_routes WHERE provider_id=? AND (public_name=? OR upstream_model=?) AND capabilities NOT LIKE '%image%'`, z.Provider.ID, outer, outer).Scan(&disabled, &enabled); err != nil {
			return z, err
		}
		if disabled > 0 && enabled == 0 {
			return z, errors.New("legacy image outer model is disabled on this channel")
		}
	}
	if mode == "responses" || strings.HasPrefix(z.Route.PublicName, "gpt-image-tool-") {
		if mode != "responses" {
			outer = z.Route.UpstreamModel
		}
		var count int
		if err := a.reader().QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=? AND enabled=1 AND (public_name=? OR upstream_model=?) AND capabilities NOT LIKE '%unsupported%' AND capabilities NOT LIKE '%image%'`, z.Provider.ID, outer, outer).Scan(&count); err != nil {
			return z, err
		}
		if count == 0 {
			return z, errors.New("configured outer model is missing or disabled on this channel")
		}
		if z.ProviderKeyID > 0 {
			var policy, allowlist, fixed string
			if err := a.reader().QueryRow(`SELECT model_policy,model_allowlist,model FROM provider_api_keys WHERE id=?`, z.ProviderKeyID).Scan(&policy, &allowlist, &fixed); err != nil {
				return z, err
			}
			inventory, exclusions, err := a.providerKeyModelSets(context.Background(), z.ProviderKeyID)
			if err != nil {
				return z, err
			}
			if !providerKeySupportsModel(policy, allowlist, fixed, "", outer, inventory, exclusions) {
				return z, errors.New("provider key does not allow the configured outer model")
			}
		}
		z.Route.UpstreamModel = outer
	}
	return z, nil
}
