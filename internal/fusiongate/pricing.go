package fusiongate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	openAIPricingURL    = "https://developers.openai.com/api/docs/pricing.md"
	xAIPricingURL       = "https://docs.x.ai/developers/pricing"
	geminiPricingURL    = "https://ai.google.dev/gemini-api/docs/pricing?hl=en"
	claudePricingURL    = "https://platform.claude.com/docs/en/about-claude/pricing.md"
	manualPricingSource = "manual"
)

type officialModelPrice struct {
	Model                string `json:"model"`
	InputMicros          int64  `json:"input_price_micros"`
	CachedMicros         int64  `json:"cached_price_micros"`
	OutputMicros         int64  `json:"output_price_micros"`
	LongContextThreshold int64  `json:"long_context_threshold"`
	LongInputMicros      int64  `json:"long_input_price_micros"`
	LongCachedMicros     int64  `json:"long_cached_price_micros"`
	LongOutputMicros     int64  `json:"long_output_price_micros"`
	Source               string `json:"source"`
}

type pricingSyncResult struct {
	Sources       int      `json:"sources"`
	Models        int      `json:"models"`
	UpdatedRoutes int64    `json:"updated_routes"`
	SyncedAt      string   `json:"synced_at"`
	Errors        []string `json:"errors,omitempty"`
}

func pricingSyncInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("FUSIONGATE_PRICING_SYNC_INTERVAL"))
	if value == "" {
		return 24 * time.Hour
	}
	if value == "0" || strings.EqualFold(value, "off") || strings.EqualFold(value, "false") {
		return 0
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Hour {
		return 24 * time.Hour
	}
	return interval
}

func (a *App) runPricingSyncLoop(ctx context.Context) {
	interval := pricingSyncInterval()
	if interval <= 0 {
		return
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if _, err := a.syncOfficialPricing(ctx); err != nil {
				a.log.Warn("official pricing sync failed", "error", err)
			}
			timer.Reset(interval)
		}
	}
}

func (a *App) pricing(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	switch r.Method {
	case http.MethodGet:
		status := map[string]string{}
		rows, err := a.db.Query(`SELECT key,value FROM settings WHERE key LIKE 'pricing_%'`)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		defer rows.Close()
		for rows.Next() {
			var key, value string
			if rows.Scan(&key, &value) == nil {
				status[key] = value
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "interval": pricingSyncInterval().String(), "sources": []string{openAIPricingURL, xAIPricingURL, geminiPricingURL, claudePricingURL}})
	case http.MethodPost:
		result, err := a.syncOfficialPricing(r.Context())
		if err != nil && result.Sources == 0 {
			fail(w, http.StatusBadGateway, "pricing_sync_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) syncOfficialPricing(ctx context.Context) (pricingSyncResult, error) {
	return a.syncOfficialPricingTarget(ctx, "")
}

func (a *App) syncOfficialPricingTarget(ctx context.Context, publicName string) (pricingSyncResult, error) {
	result := pricingSyncResult{SyncedAt: now()}
	type source struct {
		name  string
		url   string
		parse func([]byte) (map[string]officialModelPrice, error)
	}
	sources := []source{
		{"openai", openAIPricingURL, parseOpenAIPricing},
		{"grok", xAIPricingURL, parseXAIPricing},
		{"gemini", geminiPricingURL, parseGeminiPricing},
		{"claude", claudePricingURL, parseAnthropicPricing},
	}
	catalogs := map[string]map[string]officialModelPrice{}
	client := &http.Client{Timeout: 25 * time.Second}
	for _, source := range sources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, nil)
		if err != nil {
			result.Errors = append(result.Errors, source.name+": "+err.Error())
			continue
		}
		req.Header.Set("Accept", "text/markdown,text/html;q=0.9")
		req.Header.Set("User-Agent", "FusionGate pricing-sync/1.0")
		resp, err := client.Do(req)
		if err != nil {
			result.Errors = append(result.Errors, source.name+": "+err.Error())
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if readErr != nil {
				err = readErr
			} else {
				err = fmt.Errorf("official endpoint returned HTTP %d", resp.StatusCode)
			}
			result.Errors = append(result.Errors, source.name+": "+err.Error())
			continue
		}
		catalog, err := source.parse(body)
		if err != nil {
			result.Errors = append(result.Errors, source.name+": "+err.Error())
			continue
		}
		for model, price := range catalog {
			price.Source = source.url
			catalog[model] = price
		}
		catalogs[source.name] = catalog
		result.Sources++
		result.Models += len(catalog)
	}
	if result.Sources == 0 {
		err := errors.New(strings.Join(result.Errors, "; "))
		a.savePricingSyncStatus(result, err)
		return result, err
	}

	updated, applyErrors, err := a.applyOfficialPricing(ctx, catalogs, publicName)
	if err != nil {
		return result, err
	}
	result.UpdatedRoutes += updated
	result.Errors = append(result.Errors, applyErrors...)
	var syncErr error
	if len(result.Errors) > 0 {
		syncErr = errors.New(strings.Join(result.Errors, "; "))
	}
	a.savePricingSyncStatus(result, syncErr)
	return result, syncErr
}

func (a *App) applyOfficialPricing(ctx context.Context, catalogs map[string]map[string]officialModelPrice, publicName string) (int64, []string, error) {
	query := `SELECT r.id,r.upstream_model,p.type,r.pricing_source FROM model_routes r JOIN providers p ON p.id=r.provider_id`
	args := []any{}
	if strings.TrimSpace(publicName) != "" {
		query += ` WHERE r.public_name=?`
		args = append(args, strings.ToLower(strings.TrimSpace(publicName)))
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, err
	}
	type routeTarget struct {
		id                  int64
		model, providerType string
		pricingSource       string
	}
	targets := []routeTarget{}
	for rows.Next() {
		var target routeTarget
		if rows.Scan(&target.id, &target.model, &target.providerType, &target.pricingSource) == nil {
			targets = append(targets, target)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	stamp := now()
	var updated int64
	applyErrors := []string{}
	for _, target := range targets {
		if target.pricingSource == manualPricingSource {
			continue
		}
		catalogName := ""
		switch target.providerType {
		case "openai":
			catalogName = "openai"
		case "grok", "grok_oauth":
			catalogName = "grok"
		case "gemini", "gemini_cli":
			catalogName = "gemini"
		case "anthropic", "claude_oauth":
			catalogName = "claude"
		}
		price, ok := lookupOfficialPrice(catalogs[catalogName], target.model)
		if !ok {
			continue
		}
		res, updateErr := a.db.ExecContext(ctx, `UPDATE model_routes SET input_price_micros=?,cached_price_micros=?,output_price_micros=?,long_context_threshold=?,long_input_price_micros=?,long_cached_price_micros=?,long_output_price_micros=?,pricing_source=?,pricing_updated_at=?,updated_at=? WHERE id=? AND pricing_source<>?`, price.InputMicros, price.CachedMicros, price.OutputMicros, price.LongContextThreshold, price.LongInputMicros, price.LongCachedMicros, price.LongOutputMicros, price.Source, stamp, stamp, target.id, manualPricingSource)
		if updateErr != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("route %d: %v", target.id, updateErr))
			continue
		}
		changed, _ := res.RowsAffected()
		updated += changed
	}
	return updated, applyErrors, nil
}

func (a *App) savePricingSyncStatus(result pricingSyncResult, syncErr error) {
	lastError := ""
	if syncErr != nil {
		lastError = syncErr.Error()
	}
	values := map[string]string{
		"pricing_last_sync":           result.SyncedAt,
		"pricing_last_error":          lastError,
		"pricing_last_sources":        strconv.Itoa(result.Sources),
		"pricing_last_models":         strconv.Itoa(result.Models),
		"pricing_last_updated_routes": strconv.FormatInt(result.UpdatedRoutes, 10),
	}
	for key, value := range values {
		_, _ = a.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	}
}

func dollarsPerMillionMicros(value string) int64 {
	value = strings.TrimSpace(strings.TrimPrefix(value, "$"))
	amount, err := strconv.ParseFloat(value, 64)
	if err != nil || amount < 0 {
		return 0
	}
	return int64(amount*1_000_000 + 0.5)
}

var xaiPriceRow = regexp.MustCompile(`^\|\s*([^|]+?)\s*\|\s*([^|]+)\|\s*\$([0-9.]+)\s*\|\s*\$([0-9.]+)\s*\|\s*\$([0-9.]+)\s*\|`)

func parseXAIPricing(body []byte) (map[string]officialModelPrice, error) {
	out := map[string]officialModelPrice{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		match := xaiPriceRow.FindStringSubmatch(scanner.Text())
		if len(match) != 6 {
			continue
		}
		label := strings.TrimSpace(match[1])
		long := strings.Contains(label, "≥")
		model := strings.TrimSpace(strings.Split(label, " (")[0])
		if !strings.HasPrefix(model, "grok-") {
			continue
		}
		price := out[model]
		price.Model = model
		if long {
			price.LongContextThreshold = 200_000
			price.LongInputMicros = dollarsPerMillionMicros(match[3])
			price.LongCachedMicros = dollarsPerMillionMicros(match[4])
			price.LongOutputMicros = dollarsPerMillionMicros(match[5])
		} else {
			price.InputMicros = dollarsPerMillionMicros(match[3])
			price.CachedMicros = dollarsPerMillionMicros(match[4])
			price.OutputMicros = dollarsPerMillionMicros(match[5])
		}
		out[model] = price
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("official xAI pricing table was not recognized")
	}
	return out, nil
}

var openAIPriceRow = regexp.MustCompile(`\["([^"]+)",\s*([0-9.]+),\s*(?:([0-9.]+)|null|"-"),\s*(?:([0-9.]+)|null|"-")(?:,\s*([0-9.]+))?\]`)

func parseOpenAIPricing(body []byte) (map[string]officialModelPrice, error) {
	text := string(body)
	start := strings.Index(text, `data-value="standard"`)
	if start >= 0 {
		text = text[start:]
	}
	if end := strings.Index(text, `data-value="batch"`); end > 0 {
		text = text[:end]
	}
	out := map[string]officialModelPrice{}
	for _, match := range openAIPriceRow.FindAllStringSubmatch(text, -1) {
		model := strings.TrimSpace(strings.Split(match[1], " (")[0])
		if model == "" {
			continue
		}
		output := match[4]
		if match[5] != "" {
			output = match[5]
		}
		out[model] = officialModelPrice{Model: model, InputMicros: dollarsPerMillionMicros(match[2]), CachedMicros: dollarsPerMillionMicros(match[3]), OutputMicros: dollarsPerMillionMicros(output)}
	}
	if len(out) == 0 {
		return nil, errors.New("official OpenAI pricing table was not recognized")
	}
	return out, nil
}

var (
	geminiModelID = regexp.MustCompile(`<code[^>]*>(gemini-[^<]+)</code>`)
	standardTable = regexp.MustCompile(`(?s)<h3[^>]*data-text="Standard"[^>]*>.*?<table[^>]*>(.*?)</table>`)
	priceCell     = regexp.MustCompile(`(?s)<tr>\s*<td>(Input price|Output price(?: \(including thinking tokens\))?|Context caching price)</td>.*?<td>\s*\$([0-9.]+)`)
)

func parseGeminiPricing(body []byte) (map[string]officialModelPrice, error) {
	out := map[string]officialModelPrice{}
	for _, rawSection := range strings.Split(string(body), `<div class="models-section">`)[1:] {
		section := []byte(rawSection)
		modelMatch := geminiModelID.FindSubmatch(section)
		if len(modelMatch) != 2 {
			continue
		}
		model := strings.TrimSpace(string(modelMatch[1]))
		table := standardTable.FindSubmatch(section)
		if len(table) != 2 {
			continue
		}
		price := officialModelPrice{Model: model}
		for _, cell := range priceCell.FindAllSubmatch(table[1], -1) {
			value := dollarsPerMillionMicros(string(cell[2]))
			switch string(cell[1]) {
			case "Input price":
				price.InputMicros = value
			case "Context caching price":
				price.CachedMicros = value
			default:
				price.OutputMicros = value
			}
		}
		if price.InputMicros > 0 || price.OutputMicros > 0 {
			out[model] = price
		}
	}
	if len(out) == 0 {
		return nil, errors.New("official Gemini pricing table was not recognized")
	}
	return out, nil
}

var (
	claudeModelLabel = regexp.MustCompile(`(?i)Claude\s+(Opus|Sonnet|Haiku|Fable|Mythos)\s+([0-9]+(?:\.[0-9]+)?)`)
	claudePriceCell  = regexp.MustCompile(`\$([0-9.]+)\s*/\s*MTok`)
)

func parseAnthropicPricing(body []byte) (map[string]officialModelPrice, error) {
	return parseAnthropicPricingAt(body, time.Now().UTC())
}

func parseAnthropicPricingAt(body []byte, current time.Time) (map[string]officialModelPrice, error) {
	out := map[string]officialModelPrice{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") || !strings.Contains(strings.ToLower(line), "claude") {
			continue
		}
		modelMatch := claudeModelLabel.FindStringSubmatch(line)
		prices := claudePriceCell.FindAllStringSubmatch(line, -1)
		if len(modelMatch) != 3 || len(prices) < 5 || !anthropicPriceRowActive(line, current) {
			continue
		}
		family := strings.ToLower(modelMatch[1])
		version := strings.ReplaceAll(modelMatch[2], ".", "-")
		model := "claude-" + family + "-" + version
		if strings.HasPrefix(version, "3") {
			model = "claude-" + version + "-" + family
		}
		if _, exists := out[model]; exists {
			continue
		}
		out[model] = officialModelPrice{
			Model:        model,
			InputMicros:  dollarsPerMillionMicros(prices[0][1]),
			CachedMicros: dollarsPerMillionMicros(prices[3][1]),
			OutputMicros: dollarsPerMillionMicros(prices[4][1]),
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("official Anthropic pricing table was not recognized")
	}
	return out, nil
}

func anthropicPriceRowActive(line string, current time.Time) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "through august 31, 2026") {
		return current.Before(time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC))
	}
	if strings.Contains(lower, "starting september 1, 2026") {
		return !current.Before(time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC))
	}
	return true
}

func lookupOfficialPrice(catalog map[string]officialModelPrice, upstream string) (officialModelPrice, bool) {
	if len(catalog) == 0 {
		return officialModelPrice{}, false
	}
	model := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(upstream, "models/")))
	for _, candidate := range []string{model, strings.TrimSuffix(model, "-latest")} {
		if price, ok := catalog[candidate]; ok {
			return price, true
		}
	}
	keys := make([]string, 0, len(catalog))
	for key := range catalog {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, key := range keys {
		if strings.HasPrefix(model, key+"-") {
			return catalog[key], true
		}
	}
	return officialModelPrice{}, false
}

func (a *App) modelByName(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	encoded := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/models/"), "/")
	action := ""
	for _, suffix := range []string{"/pricing/official", "/pricing"} {
		if strings.HasSuffix(encoded, suffix) {
			action = strings.TrimPrefix(suffix, "/")
			encoded = strings.TrimSuffix(encoded, suffix)
			break
		}
	}
	name, err := url.PathUnescape(encoded)
	if err != nil || strings.TrimSpace(name) == "" {
		fail(w, http.StatusBadRequest, "invalid_request", "model name is required")
		return
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if action == "pricing" {
		if r.Method != http.MethodPatch {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "PATCH /api/admin/models/{name}/pricing required")
			return
		}
		a.updateModelPricing(w, r, name)
		return
	}
	if action == "pricing/official" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST /api/admin/models/{name}/pricing/official required")
			return
		}
		a.restoreModelOfficialPricing(w, r, name)
		return
	}
	if r.Method != http.MethodDelete {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "DELETE /api/admin/models/{name} required")
		return
	}
	deletedModels, deletedRoutes, err := a.deletePublicModels(r.Context(), []string{name})
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	if deletedModels == 0 {
		fail(w, http.StatusNotFound, "not_found", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_models": deletedModels, "deleted_routes": deletedRoutes, "model": name})
}

type modelPricingInput struct {
	InputPriceMicros      int64 `json:"input_price_micros"`
	CachedPriceMicros     int64 `json:"cached_price_micros"`
	OutputPriceMicros     int64 `json:"output_price_micros"`
	LongContextThreshold  int64 `json:"long_context_threshold"`
	LongInputPriceMicros  int64 `json:"long_input_price_micros"`
	LongCachedPriceMicros int64 `json:"long_cached_price_micros"`
	LongOutputPriceMicros int64 `json:"long_output_price_micros"`
}

func (in modelPricingInput) validate() error {
	const maxPriceMicros = int64(1_000_000_000_000)
	prices := []int64{in.InputPriceMicros, in.CachedPriceMicros, in.OutputPriceMicros, in.LongInputPriceMicros, in.LongCachedPriceMicros, in.LongOutputPriceMicros}
	for _, price := range prices {
		if price < 0 {
			return errors.New("prices cannot be negative")
		}
		if price > maxPriceMicros {
			return errors.New("price is too large")
		}
	}
	if in.LongContextThreshold < 0 || in.LongContextThreshold > 100_000_000 {
		return errors.New("long context threshold must be between 0 and 100000000")
	}
	return nil
}

func (a *App) updateModelPricing(w http.ResponseWriter, r *http.Request, name string) {
	var in modelPricingInput
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := in.validate(); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	stamp := now()
	res, err := a.db.ExecContext(r.Context(), `UPDATE model_routes SET input_price_micros=?,cached_price_micros=?,output_price_micros=?,long_context_threshold=?,long_input_price_micros=?,long_cached_price_micros=?,long_output_price_micros=?,pricing_source=?,pricing_updated_at=?,updated_at=? WHERE public_name=?`, in.InputPriceMicros, in.CachedPriceMicros, in.OutputPriceMicros, in.LongContextThreshold, in.LongInputPriceMicros, in.LongCachedPriceMicros, in.LongOutputPriceMicros, manualPricingSource, stamp, stamp, name)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	updated, _ := res.RowsAffected()
	if updated == 0 {
		fail(w, http.StatusNotFound, "not_found", "model not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": name, "updated_routes": updated, "pricing_source": manualPricingSource})
}

func (a *App) restoreModelOfficialPricing(w http.ResponseWriter, r *http.Request, name string) {
	res, err := a.db.ExecContext(r.Context(), `UPDATE model_routes SET pricing_source='',pricing_updated_at='',updated_at=? WHERE public_name=?`, now(), name)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	updated, _ := res.RowsAffected()
	if updated == 0 {
		fail(w, http.StatusNotFound, "not_found", "model not found")
		return
	}
	result, syncErr := a.syncOfficialPricingTarget(r.Context(), name)
	var matched int64
	_ = a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM model_routes WHERE public_name=? AND pricing_source<>'' AND pricing_source<>?`, name, manualPricingSource).Scan(&matched)
	if matched == 0 && syncErr != nil {
		fail(w, http.StatusBadGateway, "pricing_sync_failed", syncErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": name, "updated_routes": matched, "sync": result})
}

func (a *App) adminModels(w http.ResponseWriter, r *http.Request, _ adminCtx) {
	if r.Method != http.MethodDelete {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "DELETE required")
		return
	}
	var in struct {
		Models []string `json:"models"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	models := normalizePublicModelNames(in.Models)
	if len(models) == 0 {
		fail(w, http.StatusBadRequest, "invalid_request", "select at least one model")
		return
	}
	if len(models) > 500 {
		fail(w, http.StatusBadRequest, "invalid_request", "too many models; maximum is 500")
		return
	}
	deletedModels, deletedRoutes, err := a.deletePublicModels(r.Context(), models)
	if err != nil {
		fail(w, http.StatusInternalServerError, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_models": deletedModels, "deleted_routes": deletedRoutes})
}

func normalizePublicModelNames(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (a *App) deletePublicModels(ctx context.Context, models []string) (int64, int64, error) {
	models = normalizePublicModelNames(models)
	if len(models) == 0 {
		return 0, 0, nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var deletedModels, deletedRoutes int64
	stamp := now()
	for _, name := range models {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO model_route_exclusions(provider_id,public_name,upstream_model,created_at)
SELECT provider_id,public_name,MIN(upstream_model),? FROM model_routes WHERE public_name=? GROUP BY provider_id,public_name`, stamp, name); err != nil {
			return 0, 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM model_routes WHERE public_name=?`, name)
		if err != nil {
			return 0, 0, err
		}
		deleted, _ := res.RowsAffected()
		if deleted == 0 {
			continue
		}
		deletedModels++
		deletedRoutes += deleted
		if _, err := tx.ExecContext(ctx, `DELETE FROM route_policies WHERE public_name=?`, name); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return deletedModels, deletedRoutes, nil
}
