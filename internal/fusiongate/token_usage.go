package fusiongate

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	requestLedgerRetentionYears = 1
	requestLedgerCleanupEvery   = 24 * time.Hour
)

type tokenUsageMetrics struct {
	Requests           int64   `json:"requests"`
	Attempts           int64   `json:"attempts"`
	SuccessfulRequests int64   `json:"successful_requests"`
	ReportedRequests   int64   `json:"reported_requests"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CachedTokens       int64   `json:"cached_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	UsageCoverage      float64 `json:"usage_coverage"`
}

type tokenUsageSeriesPoint struct {
	Date string `json:"date"`
	tokenUsageMetrics
}

type tokenUsageRank struct {
	ID            int64  `json:"id,omitempty"`
	Name          string `json:"name"`
	Prefix        string `json:"prefix,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	tokenUsageMetrics
}

type tokenUsageDetail struct {
	Date          string `json:"date"`
	APIKeyID      int64  `json:"api_key_id,omitempty"`
	APIKeyName    string `json:"api_key_name"`
	APIKeyPrefix  string `json:"api_key_prefix,omitempty"`
	ProviderID    int64  `json:"provider_id,omitempty"`
	ProviderName  string `json:"provider_name"`
	PublicModel   string `json:"public_model"`
	UpstreamModel string `json:"upstream_model"`
	tokenUsageMetrics
}

type tokenUsageResponse struct {
	Period struct {
		Days          int    `json:"days"`
		From          string `json:"from"`
		To            string `json:"to"`
		RetentionDays int    `json:"retention_days"`
		Timezone      string `json:"timezone"`
	} `json:"period"`
	Filters struct {
		APIKeyID   int64  `json:"api_key_id,omitempty"`
		ProviderID int64  `json:"provider_id,omitempty"`
		Model      string `json:"model,omitempty"`
	} `json:"filters"`
	Totals      tokenUsageMetrics       `json:"totals"`
	Series      []tokenUsageSeriesPoint `json:"series"`
	ByKeys      []tokenUsageRank        `json:"by_keys"`
	ByProviders []tokenUsageRank        `json:"by_providers"`
	ByModels    []tokenUsageRank        `json:"by_models"`
	Details     []tokenUsageDetail      `json:"details"`
	Page        int                     `json:"page"`
	PageSize    int                     `json:"page_size"`
	HasMore     bool                    `json:"has_more"`
}

func (a *App) pruneRequestLedger(ctx context.Context, force bool) error {
	a.ledgerCleanupMu.Lock()
	defer a.ledgerCleanupMu.Unlock()

	current := time.Now().UTC()
	if !force && !a.lastLedgerCleanup.IsZero() && current.Sub(a.lastLedgerCleanup) < requestLedgerCleanupEvery {
		return nil
	}
	cutoff := current.AddDate(-requestLedgerRetentionYears, 0, 0).Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(ctx, `DELETE FROM request_ledger WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	a.lastLedgerCleanup = current
	return nil
}

func tokenRequestIdentity(alias string) string {
	return "CASE WHEN " + alias + ".gateway_request_id='' THEN " + alias + ".request_id ELSE " + alias + ".gateway_request_id END"
}

func tokenMetricsSQL(alias string) string {
	identity := tokenRequestIdentity(alias)
	return `COUNT(DISTINCT ` + identity + `),
		COUNT(*),
		COUNT(DISTINCT CASE WHEN ` + alias + `.success=1 THEN ` + identity + ` END),
		COUNT(DISTINCT CASE WHEN ` + alias + `.usage_reported=1 AND ` + alias + `.success=1 THEN ` + identity + ` END),
		COALESCE(SUM(` + alias + `.input_tokens),0),
		COALESCE(SUM(` + alias + `.output_tokens),0),
		COALESCE(SUM(` + alias + `.cached_tokens),0),
		COALESCE(SUM(` + alias + `.reasoning_tokens),0),
		COALESCE(SUM(` + alias + `.input_tokens+` + alias + `.output_tokens),0)`
}

func scanTokenMetrics(scanner interface{ Scan(...any) error }, metrics *tokenUsageMetrics, prefix ...any) error {
	targets := append(prefix,
		&metrics.Requests,
		&metrics.Attempts,
		&metrics.SuccessfulRequests,
		&metrics.ReportedRequests,
		&metrics.InputTokens,
		&metrics.OutputTokens,
		&metrics.CachedTokens,
		&metrics.ReasoningTokens,
		&metrics.TotalTokens,
	)
	if err := scanner.Scan(targets...); err != nil {
		return err
	}
	metrics.setCoverage()
	return nil
}

func (m *tokenUsageMetrics) setCoverage() {
	if m.SuccessfulRequests <= 0 {
		m.UsageCoverage = 0
		return
	}
	m.UsageCoverage = float64(m.ReportedRequests) * 100 / float64(m.SuccessfulRequests)
}

func tokenUsageRange(days int) (time.Time, time.Time) {
	to := time.Now().UTC()
	today := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(days - 1))
	return from, to
}

func tokenUsageFilters(r *http.Request, from, to time.Time) (string, []any, int64, int64, string, error) {
	where := []string{"l.created_at>=?", "l.created_at<=?", "l.completed_at IS NOT NULL"}
	args := []any{from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)}
	var apiKeyID, providerID int64
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("api_key_id")); raw != "" {
		apiKeyID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || apiKeyID <= 0 {
			return "", nil, 0, 0, "", errInvalidTokenUsageFilter
		}
		where = append(where, "l.api_key_id=?")
		args = append(args, apiKeyID)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("provider_id")); raw != "" {
		providerID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || providerID <= 0 {
			return "", nil, 0, 0, "", errInvalidTokenUsageFilter
		}
		where = append(where, "l.provider_id=?")
		args = append(args, providerID)
	}
	model := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("model")))
	if model != "" {
		where = append(where, "(LOWER(l.public_model)=? OR LOWER(l.upstream_model)=?)")
		args = append(args, model, model)
	}
	return strings.Join(where, " AND "), args, apiKeyID, providerID, model, nil
}

var errInvalidTokenUsageFilter = &tokenUsageInputError{"invalid token usage filter"}

type tokenUsageInputError struct{ message string }

func (e *tokenUsageInputError) Error() string { return e.message }

func tokenUsageInt(raw string, fallback, minimum, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, errInvalidTokenUsageFilter
	}
	return value, nil
}

func (a *App) tokenUsage(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	days, err := tokenUsageInt(r.URL.Query().Get("days"), 30, 1, 365)
	if err != nil || (days != 7 && days != 30 && days != 90 && days != 365) {
		fail(w, http.StatusBadRequest, "invalid_filter", "days must be one of 7, 30, 90, or 365")
		return
	}
	page, err := tokenUsageInt(r.URL.Query().Get("page"), 1, 1, 1_000_000)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_filter", "page must be a positive integer")
		return
	}
	pageSize, err := tokenUsageInt(r.URL.Query().Get("page_size"), 50, 1, 100)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_filter", "page_size must be between 1 and 100")
		return
	}
	from, to := tokenUsageRange(days)
	where, args, apiKeyID, providerID, model, err := tokenUsageFilters(r, from, to)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_filter", "invalid Key, provider, or model filter")
		return
	}

	response := tokenUsageResponse{Page: page, PageSize: pageSize}
	response.Period.Days = days
	response.Period.From = from.Format(time.RFC3339Nano)
	response.Period.To = to.Format(time.RFC3339Nano)
	response.Period.RetentionDays = 365
	response.Period.Timezone = "UTC"
	response.Filters.APIKeyID = apiKeyID
	response.Filters.ProviderID = providerID
	response.Filters.Model = model

	if err := scanTokenMetrics(a.db.QueryRow(`SELECT `+tokenMetricsSQL("l")+` FROM request_ledger l WHERE `+where, args...), &response.Totals); err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	response.Series, err = a.tokenUsageSeries(from, days, where, args)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	response.ByKeys, err = a.tokenUsageKeyRanks(where, args)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	response.ByProviders, err = a.tokenUsageProviderRanks(where, args)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	response.ByModels, err = a.tokenUsageModelRanks(where, args)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	response.Details, response.HasMore, err = a.tokenUsageDetails(where, args, page, pageSize)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) tokenUsageSeries(from time.Time, days int, where string, args []any) ([]tokenUsageSeriesPoint, error) {
	rows, err := a.db.Query(`SELECT substr(l.created_at,1,10),`+tokenMetricsSQL("l")+` FROM request_ledger l WHERE `+where+` GROUP BY substr(l.created_at,1,10) ORDER BY substr(l.created_at,1,10)`, args...)
	if err != nil {
		return nil, err
	}
	points := make(map[string]tokenUsageSeriesPoint, days)
	for rows.Next() {
		var point tokenUsageSeriesPoint
		if err := scanTokenMetrics(rows, &point.tokenUsageMetrics, &point.Date); err != nil {
			rows.Close()
			return nil, err
		}
		points[point.Date] = point
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	series := make([]tokenUsageSeriesPoint, 0, days)
	for day := 0; day < days; day++ {
		date := from.AddDate(0, 0, day).Format("2006-01-02")
		point, ok := points[date]
		if !ok {
			point.Date = date
		}
		series = append(series, point)
	}
	return series, nil
}

func (a *App) tokenUsageKeyRanks(where string, args []any) ([]tokenUsageRank, error) {
	keyName := "COALESCE(NULLIF(l.api_key_name,''),k.name,'已删除密钥')"
	keyPrefix := "COALESCE(NULLIF(l.api_key_prefix,''),k.key_prefix,'')"
	query := `SELECT COALESCE(l.api_key_id,0),` + keyName + `,` + keyPrefix + `,` + tokenMetricsSQL("l") + `
		FROM request_ledger l LEFT JOIN api_keys k ON k.id=l.api_key_id WHERE ` + where + `
		GROUP BY COALESCE(l.api_key_id,0),` + keyName + `,` + keyPrefix + `
		ORDER BY COALESCE(SUM(l.input_tokens+l.output_tokens),0) DESC,` + keyName + ` LIMIT 10`
	return a.scanTokenUsageRanks(query, args, true, false)
}

func (a *App) tokenUsageProviderRanks(where string, args []any) ([]tokenUsageRank, error) {
	providerName := "COALESCE(NULLIF(l.provider_name,''),p.name,'已删除渠道')"
	query := `SELECT COALESCE(l.provider_id,0),` + providerName + `,` + tokenMetricsSQL("l") + `
		FROM request_ledger l LEFT JOIN providers p ON p.id=l.provider_id WHERE ` + where + `
		GROUP BY COALESCE(l.provider_id,0),` + providerName + `
		ORDER BY COALESCE(SUM(l.input_tokens+l.output_tokens),0) DESC,` + providerName + ` LIMIT 10`
	return a.scanTokenUsageRanks(query, args, false, false)
}

func (a *App) tokenUsageModelRanks(where string, args []any) ([]tokenUsageRank, error) {
	query := `SELECT l.public_model,l.upstream_model,` + tokenMetricsSQL("l") + `
		FROM request_ledger l WHERE ` + where + `
		GROUP BY l.public_model,l.upstream_model
		ORDER BY COALESCE(SUM(l.input_tokens+l.output_tokens),0) DESC,l.public_model,l.upstream_model LIMIT 10`
	return a.scanTokenUsageRanks(query, args, false, true)
}

func (a *App) scanTokenUsageRanks(query string, args []any, withPrefix, modelRank bool) ([]tokenUsageRank, error) {
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranks := []tokenUsageRank{}
	for rows.Next() {
		var rank tokenUsageRank
		prefix := []any{}
		if modelRank {
			prefix = append(prefix, &rank.Name, &rank.UpstreamModel)
		} else {
			prefix = append(prefix, &rank.ID, &rank.Name)
			if withPrefix {
				prefix = append(prefix, &rank.Prefix)
			}
		}
		if err := scanTokenMetrics(rows, &rank.tokenUsageMetrics, prefix...); err != nil {
			return nil, err
		}
		ranks = append(ranks, rank)
	}
	return ranks, rows.Err()
}

func (a *App) tokenUsageDetails(where string, args []any, page, pageSize int) ([]tokenUsageDetail, bool, error) {
	keyName := "COALESCE(NULLIF(l.api_key_name,''),k.name,'已删除密钥')"
	keyPrefix := "COALESCE(NULLIF(l.api_key_prefix,''),k.key_prefix,'')"
	providerName := "COALESCE(NULLIF(l.provider_name,''),p.name,'已删除渠道')"
	query := `SELECT substr(l.created_at,1,10),COALESCE(l.api_key_id,0),` + keyName + `,` + keyPrefix + `,
		COALESCE(l.provider_id,0),` + providerName + `,l.public_model,l.upstream_model,` + tokenMetricsSQL("l") + `
		FROM request_ledger l
		LEFT JOIN api_keys k ON k.id=l.api_key_id
		LEFT JOIN providers p ON p.id=l.provider_id
		WHERE ` + where + `
		GROUP BY substr(l.created_at,1,10),COALESCE(l.api_key_id,0),` + keyName + `,` + keyPrefix + `,
			COALESCE(l.provider_id,0),` + providerName + `,l.public_model,l.upstream_model
		ORDER BY substr(l.created_at,1,10) DESC,COALESCE(SUM(l.input_tokens+l.output_tokens),0) DESC,` + keyName + `
		LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), pageSize+1, (page-1)*pageSize)
	rows, err := a.db.Query(query, queryArgs...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	details := make([]tokenUsageDetail, 0, pageSize+1)
	for rows.Next() {
		var detail tokenUsageDetail
		if err := scanTokenMetrics(rows, &detail.tokenUsageMetrics,
			&detail.Date, &detail.APIKeyID, &detail.APIKeyName, &detail.APIKeyPrefix,
			&detail.ProviderID, &detail.ProviderName, &detail.PublicModel, &detail.UpstreamModel,
		); err != nil {
			return nil, false, err
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(details) > pageSize
	if hasMore {
		details = details[:pageSize]
	}
	return details, hasMore, nil
}
