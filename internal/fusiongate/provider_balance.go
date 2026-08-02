package fusiongate

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
)

const providerBalanceCacheTTL = 30 * time.Second

type providerSpend struct {
	CostMicros     int64   `json:"cost_micros"`
	Requests       int64   `json:"requests"`
	PricedRequests int64   `json:"priced_requests"`
	CostCoverage   float64 `json:"cost_coverage"`
}

type ProviderUpstreamBalance struct {
	Status    string             `json:"status"`
	Source    string             `json:"source"`
	CheckedAt string             `json:"checked_at"`
	Message   string             `json:"message,omitempty"`
	Quota     *codexAccountQuota `json:"quota,omitempty"`
}

type providerBalanceResponse struct {
	ProviderID int64 `json:"provider_id"`
	ProviderUpstreamBalance
	EstimatedSpend providerSpend               `json:"estimated_spend"`
	Manual         *providerManualBalance      `json:"manual,omitempty"`
	ModelGroups    []providerBalanceModelGroup `json:"model_groups"`
}

type providerManualBalance struct {
	ConfiguredMicros    int64              `json:"configured_micros"`
	AdjustedSpendMicros int64              `json:"adjusted_spend_micros"`
	RemainingMicros     int64              `json:"remaining_micros"`
	UsedPercent         float64            `json:"used_percent"`
	Requests            int64              `json:"requests"`
	PricedRequests      int64              `json:"priced_requests"`
	CostCoverage        float64            `json:"cost_coverage"`
	BaselineAt          string             `json:"baseline_at"`
	Multipliers         map[string]float64 `json:"multipliers"`
	SpendByCategory     map[string]int64   `json:"spend_by_category"`
}

type providerBalanceModelGroup struct {
	Category   string   `json:"category"`
	Label      string   `json:"label"`
	Multiplier float64  `json:"multiplier"`
	Models     []string `json:"models"`
}

func balanceCategory(providerType, model string) string {
	category := officialPricingCatalogName(providerType, model)
	normalized := strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndex(normalized, "/"); slash >= 0 {
		normalized = normalized[slash+1:]
	}
	if category == "" && (strings.HasPrefix(normalized, "gpt-") || strings.HasPrefix(normalized, "o1") || strings.HasPrefix(normalized, "o3") || strings.HasPrefix(normalized, "o4") || strings.HasPrefix(normalized, "codex-")) {
		category = "openai"
	}
	if category == "" {
		return "other"
	}
	return category
}

func balanceMultipliers(openai, claude, grok, gemini, other float64) map[string]float64 {
	return map[string]float64{"openai": openai, "claude": claude, "grok": grok, "gemini": gemini, "other": other}
}

func balanceCategoryLabel(category string) string {
	switch category {
	case "openai":
		return "OpenAI"
	case "claude":
		return "Claude"
	case "grok":
		return "Grok"
	case "gemini":
		return "Gemini"
	default:
		return "其他模型"
	}
}

func (a *App) providerBalance(ctx context.Context, id int64, refresh bool) (providerBalanceResponse, error) {
	var providerType, authKind, baselineAt string
	var manualBalance sql.NullInt64
	var openaiMultiplier, claudeMultiplier, grokMultiplier, geminiMultiplier, otherMultiplier float64
	if err := a.db.QueryRowContext(ctx, `SELECT type,auth_kind,manual_balance_micros,COALESCE(balance_baseline_at,''),balance_multiplier_openai,balance_multiplier_claude,balance_multiplier_grok,balance_multiplier_gemini,balance_multiplier_other FROM providers WHERE id=?`, id).Scan(&providerType, &authKind, &manualBalance, &baselineAt, &openaiMultiplier, &claudeMultiplier, &grokMultiplier, &geminiMultiplier, &otherMultiplier); err != nil {
		return providerBalanceResponse{}, err
	}
	multipliers := balanceMultipliers(openaiMultiplier, claudeMultiplier, grokMultiplier, geminiMultiplier, otherMultiplier)

	spend := providerSpend{}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN cost_type<>'unknown' THEN 1 ELSE 0 END),0),COALESCE(SUM(cost_micros),0) FROM request_ledger WHERE provider_id=? AND completed_at IS NOT NULL`, id).Scan(&spend.Requests, &spend.PricedRequests, &spend.CostMicros); err != nil {
		return providerBalanceResponse{}, err
	}
	if spend.Requests > 0 {
		spend.CostCoverage = float64(spend.PricedRequests) * 100 / float64(spend.Requests)
	}

	response := providerBalanceResponse{ProviderID: id, EstimatedSpend: spend, ModelGroups: []providerBalanceModelGroup{}}
	modelsByCategory := map[string][]string{}
	rows, err := a.db.QueryContext(ctx, `SELECT DISTINCT upstream_model FROM model_routes WHERE provider_id=? AND enabled=1 ORDER BY upstream_model`, id)
	if err != nil {
		return providerBalanceResponse{}, err
	}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			rows.Close()
			return providerBalanceResponse{}, err
		}
		category := balanceCategory(providerType, model)
		modelsByCategory[category] = append(modelsByCategory[category], model)
	}
	if err := rows.Close(); err != nil {
		return providerBalanceResponse{}, err
	}
	for _, category := range []string{"openai", "claude", "grok", "gemini", "other"} {
		if models := modelsByCategory[category]; len(models) > 0 {
			response.ModelGroups = append(response.ModelGroups, providerBalanceModelGroup{Category: category, Label: balanceCategoryLabel(category), Multiplier: multipliers[category], Models: models})
		}
	}
	if manualBalance.Valid {
		manual := &providerManualBalance{ConfiguredMicros: manualBalance.Int64, RemainingMicros: manualBalance.Int64, BaselineAt: baselineAt, Multipliers: multipliers, SpendByCategory: map[string]int64{}}
		if queryErr := a.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN cost_type<>'unknown' THEN 1 ELSE 0 END),0) FROM request_ledger WHERE provider_id=? AND completed_at IS NOT NULL AND created_at>=?`, id, baselineAt).Scan(&manual.Requests, &manual.PricedRequests); queryErr != nil {
			return providerBalanceResponse{}, queryErr
		}
		if manual.Requests > 0 {
			manual.CostCoverage = float64(manual.PricedRequests) * 100 / float64(manual.Requests)
		}
		spendRows, queryErr := a.db.QueryContext(ctx, `SELECT upstream_model,cost_micros FROM request_ledger WHERE provider_id=? AND completed_at IS NOT NULL AND created_at>=? AND cost_type<>'unknown'`, id, baselineAt)
		if queryErr != nil {
			return providerBalanceResponse{}, queryErr
		}
		for spendRows.Next() {
			var model string
			var costMicros int64
			if scanErr := spendRows.Scan(&model, &costMicros); scanErr != nil {
				spendRows.Close()
				return providerBalanceResponse{}, scanErr
			}
			category := balanceCategory(providerType, model)
			adjusted := int64(math.Round(float64(costMicros) * multipliers[category]))
			manual.AdjustedSpendMicros += adjusted
			manual.SpendByCategory[category] += adjusted
		}
		if closeErr := spendRows.Close(); closeErr != nil {
			return providerBalanceResponse{}, closeErr
		}
		manual.RemainingMicros = manual.ConfiguredMicros - manual.AdjustedSpendMicros
		if manual.RemainingMicros < 0 {
			manual.RemainingMicros = 0
		}
		if manual.ConfiguredMicros > 0 {
			manual.UsedPercent = math.Min(100, float64(manual.AdjustedSpendMicros)*100/float64(manual.ConfiguredMicros))
		} else {
			manual.UsedPercent = 100
		}
		response.Manual = manual
	}
	if providerType != "codex_oauth" || authKind != "oauth" {
		response.ProviderUpstreamBalance = ProviderUpstreamBalance{
			Status:    "unsupported",
			Source:    "fusiongate_ledger",
			CheckedAt: now(),
			Message:   "upstream balance API is not available for this provider",
		}
		return response, nil
	}

	if !refresh {
		a.balanceMu.Lock()
		cached, ok := a.balanceCache[id]
		a.balanceMu.Unlock()
		if ok {
			checkedAt := parseTime(cached.CheckedAt)
			if checkedAt != nil && time.Since(*checkedAt) < providerBalanceCacheTTL {
				response.ProviderUpstreamBalance = cached
				return response, nil
			}
		}
	}

	upstream := ProviderUpstreamBalance{Status: "error", Source: "openai_codex", CheckedAt: now()}
	result, err := a.withCodexCredential(ctx, id, func(credential ProviderCredential, nodeID *int64) (any, error) {
		return a.fetchCodexAccountQuotaViaNode(ctx, credential.AccessToken, credential.AccountID, nodeID)
	})
	if err != nil {
		upstream.Message = "upstream quota could not be read"
		a.log.Warn("provider balance refresh failed", "provider_id", id, "error", err)
	} else if quota, ok := result.(*codexAccountQuota); ok {
		upstream.Status = "available"
		upstream.Quota = quota
	} else {
		upstream.Message = "upstream quota response was invalid"
	}
	a.balanceMu.Lock()
	a.balanceCache[id] = upstream
	a.balanceMu.Unlock()
	response.ProviderUpstreamBalance = upstream
	return response, nil
}

func (a *App) providerBalanceHandler(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	result, err := a.providerBalance(r.Context(), id, r.URL.Query().Get("refresh") == "1")
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "provider_not_found", "provider not found")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
