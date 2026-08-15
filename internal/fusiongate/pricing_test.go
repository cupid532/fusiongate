package fusiongate

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPricingSyncIntervalDefaultsAndValidates(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "default", want: time.Hour},
		{name: "explicit", value: "2h", want: 2 * time.Hour},
		{name: "minimum", value: "5m", want: 5 * time.Minute},
		{name: "below minimum", value: "4m", want: time.Hour},
		{name: "disabled", value: "off", want: 0},
		{name: "invalid", value: "not-a-duration", want: time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FUSIONGATE_PRICING_SYNC_INTERVAL", tc.value)
			if got := pricingSyncInterval(); got != tc.want {
				t.Fatalf("pricingSyncInterval() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestParseXAIPricingSupportsLongContextRates(t *testing.T) {
	catalog, err := parseXAIPricing([]byte(`
| Model | Context | Input | Cached | Output |
| grok-4.5 (< 200k prompt tokens) | 500k | $2.00 | $0.30 | $6.00 |
| grok-4.5 (≥ 200k prompt tokens) | 500k | $4.00 | $0.60 | $12.00 |
`))
	if err != nil {
		t.Fatal(err)
	}
	price := catalog["grok-4.5"]
	if price.InputMicros != 2_000_000 || price.CachedMicros != 300_000 || price.OutputMicros != 6_000_000 {
		t.Fatalf("unexpected standard price: %+v", price)
	}
	if price.LongContextThreshold != 200_000 || price.LongInputMicros != 4_000_000 || price.LongCachedMicros != 600_000 || price.LongOutputMicros != 12_000_000 {
		t.Fatalf("unexpected long-context price: %+v", price)
	}
}

func TestParseOpenAIPricingUsesLastColumnAsOutput(t *testing.T) {
	catalog, err := parseOpenAIPricing([]byte(`
<div data-value="standard">
["gpt-5.6-sol", 5, 0.5, 6.25, 30],
["gpt-4o-mini", 0.15, 0.075, 0.6]
</div><div data-value="batch"></div>
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog["gpt-5.6-sol"]; got.InputMicros != 5_000_000 || got.CachedMicros != 500_000 || got.OutputMicros != 30_000_000 {
		t.Fatalf("unexpected extended OpenAI row: %+v", got)
	}
	if got := catalog["gpt-4o-mini"]; got.OutputMicros != 600_000 {
		t.Fatalf("unexpected OpenAI row: %+v", got)
	}
}

func TestParseOpenAIPricingSupportsCurrentMarkdownTable(t *testing.T) {
	catalog, err := parseOpenAIPricing([]byte(`
### Standard pricing data
| Model | Short context input | Short context cached input | Short context cache writes | Short context output | Long context input | Long context cached input | Long context cache writes | Long context output |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| gpt-5.6-sol | $5.00 | $0.50 | $6.25 | $30.00 | $10.00 | $1.00 | $12.50 | $45.00 |
### Batch pricing data
| gpt-5.6-sol | $2.50 | $0.25 | $3.125 | $15.00 | $5.00 | $0.50 | $6.25 | $22.50 |
`))
	if err != nil {
		t.Fatal(err)
	}
	got := catalog["gpt-5.6-sol"]
	if got.InputMicros != 5_000_000 || got.CachedMicros != 500_000 || got.OutputMicros != 30_000_000 {
		t.Fatalf("unexpected standard Markdown price: %+v", got)
	}
	if got.LongContextThreshold != 272_000 || got.LongInputMicros != 10_000_000 || got.LongCachedMicros != 1_000_000 || got.LongOutputMicros != 45_000_000 {
		t.Fatalf("unexpected long-context Markdown price: %+v", got)
	}
}

func TestParseGeminiPricingReadsStandardTier(t *testing.T) {
	catalog, err := parseGeminiPricing([]byte(`
<div class="models-section"><h2><code>gemini-3.5-flash</code></h2>
<h3 data-text="Standard">Standard</h3><table>
<tr><td>Input price</td><td>$1.50</td></tr>
<tr><td>Output price (including thinking tokens)</td><td>$9.00</td></tr>
<tr><td>Context caching price</td><td>$0.15</td></tr>
</table></div>
`))
	if err != nil {
		t.Fatal(err)
	}
	got := catalog["gemini-3.5-flash"]
	if got.InputMicros != 1_500_000 || got.CachedMicros != 150_000 || got.OutputMicros != 9_000_000 {
		t.Fatalf("unexpected Gemini price: %+v", got)
	}
}

func TestParseAnthropicPricingReadsOfficialModelTable(t *testing.T) {
	body := []byte(`
| Model | Base Input Tokens | 5m Cache Writes | 1h Cache Writes | Cache Hits & Refreshes | Output Tokens |
| Claude Sonnet 5<br/>Introductory pricing through August 31, 2026 | $2 / MTok | $2.50 / MTok | $4 / MTok | $0.20 / MTok | $10 / MTok |
| Claude Sonnet 5<br/>Pricing starting September 1, 2026 | $3 / MTok | $3.75 / MTok | $6 / MTok | $0.30 / MTok | $15 / MTok |
| Claude Opus 4.8 | $5 / MTok | $6.25 / MTok | $10 / MTok | $0.50 / MTok | $25 / MTok |
| Claude Haiku 3.5 | $0.80 / MTok | $1 / MTok | $1.60 / MTok | $0.08 / MTok | $4 / MTok |
`)
	catalog, err := parseAnthropicPricingAt(body, time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog["claude-sonnet-5"]; got.InputMicros != 2_000_000 || got.CachedMicros != 200_000 || got.OutputMicros != 10_000_000 {
		t.Fatalf("unexpected introductory Claude price: %+v", got)
	}
	if got := catalog["claude-opus-4-8"]; got.InputMicros != 5_000_000 || got.CachedMicros != 500_000 || got.OutputMicros != 25_000_000 {
		t.Fatalf("unexpected Claude Opus price: %+v", got)
	}
	if got := catalog["claude-3-5-haiku"]; got.InputMicros != 800_000 || got.OutputMicros != 4_000_000 {
		t.Fatalf("unexpected legacy Claude model key or price: %+v", got)
	}
	future, err := parseAnthropicPricingAt(body, time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got := future["claude-sonnet-5"]; got.InputMicros != 3_000_000 || got.OutputMicros != 15_000_000 {
		t.Fatalf("unexpected post-introductory Claude price: %+v", got)
	}
}

func TestLookupOfficialPriceAcceptsLatestAndDatedVersions(t *testing.T) {
	catalog := map[string]officialModelPrice{"gpt-5": {Model: "gpt-5", InputMicros: 1}}
	for _, model := range []string{"gpt-5", "gpt-5-latest", "gpt-5-2026-07-01", "models/gpt-5"} {
		if _, ok := lookupOfficialPrice(catalog, model); !ok {
			t.Fatalf("expected price match for %q", model)
		}
	}
}

func TestLookupOfficialPriceAcceptsDatedClaudeVersions(t *testing.T) {
	catalog := map[string]officialModelPrice{"claude-sonnet-4-5": {Model: "claude-sonnet-4-5", InputMicros: 1}}
	for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929", "models/claude-sonnet-4-5-latest"} {
		if _, ok := lookupOfficialPrice(catalog, model); !ok {
			t.Fatalf("expected Claude price match for %q", model)
		}
	}
}

func TestOfficialPricingCatalogUsesModelIdentityAcrossCompatibleChannels(t *testing.T) {
	for _, tc := range []struct {
		providerType, model, want string
	}{
		{"openai", "claude-sonnet-5", "claude"},
		{"openai_compatible", "anthropic/claude-opus-4-8", "claude"},
		{"openai", "grok-4.5", "grok"},
		{"openrouter", "google/gemini-3.5-flash", "gemini"},
		{"openai", "gpt-5.6-sol", "openai"},
	} {
		if got := officialPricingCatalogName(tc.providerType, tc.model); got != tc.want {
			t.Fatalf("provider=%q model=%q catalog=%q want=%q", tc.providerType, tc.model, got, tc.want)
		}
	}
}

func TestEstimatedCostHandlesCachedAndLongContextTokens(t *testing.T) {
	z := resolvedRoute{Route: Route{
		InputPriceMicros:      2_000_000,
		CachedPriceMicros:     300_000,
		OutputPriceMicros:     6_000_000,
		LongContextThreshold:  200_000,
		LongInputPriceMicros:  4_000_000,
		LongCachedPriceMicros: 600_000,
		LongOutputPriceMicros: 12_000_000,
	}}
	standard := Usage{Input: 100, Cached: 40, Output: 10}
	cost(z, &standard)
	if standard.CostMicros != 192 || standard.CostType != "estimated" {
		t.Fatalf("standard cost=%d type=%s, want 192 estimated", standard.CostMicros, standard.CostType)
	}
	long := Usage{Input: 200_000, Cached: 50_000, Output: 10_000}
	cost(z, &long)
	if long.CostMicros != 750_000 {
		t.Fatalf("long-context cost=%d, want 750000", long.CostMicros)
	}
}

func TestDeletePublicModelRemovesAllMappings(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	for _, name := range []string{"one", "two"} {
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, name, "openai", "https://api.example", []byte{1}, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "shared", id, "model-"+name, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	a.modelByName(recorder, httptest.NewRequest(http.MethodDelete, "/api/admin/models/shared", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var routes, exclusions int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE public_name='shared'`).Scan(&routes)
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM model_route_exclusions WHERE public_name IN ('model-one','model-two') AND public_name=upstream_model`).Scan(&exclusions)
	if routes != 0 || exclusions != 2 {
		t.Fatalf("routes=%d exclusions=%d, expected complete deletion with two exclusions", routes, exclusions)
	}
}

func TestDeleteRouteCreatesModelExclusion(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "single", "openai", "https://api.example", []byte{1}, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := res.LastInsertId()
	res, err = a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "removed", providerID, "removed-upstream", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	routeID, _ := res.LastInsertId()
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/routes/1", nil)
	rec := httptest.NewRecorder()
	a.routeByID(rec, req, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var publicName, upstreamModel string
	if err := a.db.QueryRow(`SELECT public_name,upstream_model FROM model_route_exclusions WHERE provider_id=?`, providerID).Scan(&publicName, &upstreamModel); err != nil {
		t.Fatal(err)
	}
	if routeID != 1 || publicName != "removed-upstream" || upstreamModel != "removed-upstream" {
		t.Fatalf("routeID=%d exclusion=%q/%q", routeID, publicName, upstreamModel)
	}
}

func TestBatchDeletePublicModels(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "batch", "openai", "https://api.example", []byte{1}, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := res.LastInsertId()
	for _, model := range []string{"alpha", "beta", "keep"} {
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, model, providerID, model, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/models", strings.NewReader(`{"models":["BETA","alpha","alpha"]}`))
	rec := httptest.NewRecorder()
	a.adminModels(rec, req, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var remaining, exclusions int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_route_exclusions`).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || exclusions != 2 {
		t.Fatalf("remaining=%d exclusions=%d, want 1 and 2", remaining, exclusions)
	}
}

func TestModelPricingPatchUpdatesEveryChannelOnce(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	for i, providerType := range []string{"anthropic", "claude_oauth"} {
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "claude-channel-"+string(rune('a'+i)), providerType, "https://api.example", []byte{1}, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		providerID, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,input_price_micros,output_price_micros,pricing_source,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, "claude-sonnet-4-5", providerID, "claude-sonnet-4-5-20250929", 1, 2, claudePricingURL, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"input_price_micros":3000000,"cached_price_micros":300000,"output_price_micros":15000000,"long_context_threshold":200000,"long_input_price_micros":6000000,"long_cached_price_micros":600000,"long_output_price_micros":22500000}`
	rec := httptest.NewRecorder()
	a.modelByName(rec, httptest.NewRequest(http.MethodPatch, "/api/admin/models/claude-sonnet-4-5/pricing", strings.NewReader(body)), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := a.db.Query(`SELECT input_price_micros,cached_price_micros,output_price_micros,long_context_threshold,long_output_price_micros,pricing_source FROM model_routes WHERE public_name='claude-sonnet-4-5'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var input, cached, output, threshold, longOutput int64
		var source string
		if err := rows.Scan(&input, &cached, &output, &threshold, &longOutput, &source); err != nil {
			t.Fatal(err)
		}
		if input != 3_000_000 || cached != 300_000 || output != 15_000_000 || threshold != 200_000 || longOutput != 22_500_000 || source != manualPricingSource {
			t.Fatalf("unexpected unified price: %d %d %d %d %d %q", input, cached, output, threshold, longOutput, source)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("updated %d routes, want 2", count)
	}
}

func TestOfficialPricingDoesNotOverwriteManualModelPrice(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	for i, source := range []string{manualPricingSource, ""} {
		providerType := []string{"anthropic", "openai"}[i]
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "claude-"+string(rune('a'+i)), providerType, "https://api.example", []byte{1}, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		providerID, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,input_price_micros,output_price_micros,pricing_source,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, "claude-sonnet-4-5", providerID, "claude-sonnet-4-5-20250929", 9, 19, source, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	catalogs := map[string]map[string]officialModelPrice{"claude": {"claude-sonnet-4-5": {Model: "claude-sonnet-4-5", InputMicros: 3_000_000, CachedMicros: 300_000, OutputMicros: 15_000_000, Source: claudePricingURL}}}
	updated, applyErrors, err := a.applyOfficialPricing(t.Context(), catalogs, "claude-sonnet-4-5", false)
	if err != nil || len(applyErrors) != 0 {
		t.Fatalf("apply err=%v errors=%v", err, applyErrors)
	}
	if updated != 1 {
		t.Fatalf("updated=%d, want 1", updated)
	}
	var manualInput, automaticInput int64
	if err := a.db.QueryRow(`SELECT input_price_micros FROM model_routes WHERE pricing_source=?`, manualPricingSource).Scan(&manualInput); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT input_price_micros FROM model_routes WHERE pricing_source=?`, claudePricingURL).Scan(&automaticInput); err != nil {
		t.Fatal(err)
	}
	if manualInput != 9 || automaticInput != 3_000_000 {
		t.Fatalf("manual=%d automatic=%d", manualInput, automaticInput)
	}
}

type pricingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f pricingRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func insertManualPricingRoute(t *testing.T, a *App, model string) int64 {
	t.Helper()
	providerID := insertTestProvider(t, a, "manual-pricing", "openai_compatible", "https://api.example", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	routeID := insertTestRoute(t, a, providerID, model, model, "chat", 0)
	if _, err := a.db.Exec(`UPDATE model_routes SET input_price_micros=111,cached_price_micros=22,output_price_micros=333,pricing_source=?,pricing_updated_at='manual-stamp' WHERE id=?`, manualPricingSource, routeID); err != nil {
		t.Fatal(err)
	}
	return routeID
}

func assertManualPricingUnchanged(t *testing.T, a *App, routeID int64) {
	t.Helper()
	var input, cached, output int64
	var source, updatedAt string
	if err := a.db.QueryRow(`SELECT input_price_micros,cached_price_micros,output_price_micros,pricing_source,pricing_updated_at FROM model_routes WHERE id=?`, routeID).Scan(&input, &cached, &output, &source, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if input != 111 || cached != 22 || output != 333 || source != manualPricingSource || updatedAt != "manual-stamp" {
		t.Fatalf("manual pricing changed to input=%d cached=%d output=%d source=%q updated_at=%q", input, cached, output, source, updatedAt)
	}
}

func TestRestoreOfficialPricingPreservesManualValuesWhenSourcesFail(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routeID := insertManualPricingRoute(t, a, "manual-model")
	a.pricingClient = &http.Client{Transport: pricingRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}

	rec := httptest.NewRecorder()
	a.restoreModelOfficialPricing(rec, httptest.NewRequest(http.MethodPost, "/api/admin/models/manual-model/pricing/official", nil), "manual-model")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "pricing_sync_failed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertManualPricingUnchanged(t, a, routeID)
}

func TestRestoreOfficialPricingPreservesManualValuesWithoutMatch(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routeID := insertManualPricingRoute(t, a, "manual-model")
	a.pricingClient = &http.Client{Transport: pricingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusBadGateway
		body := "unavailable"
		if r.URL.String() == openRouterPricingURL {
			status = http.StatusOK
			body = `{"data":[{"id":"vendor/other-model","canonical_slug":"vendor/other-model","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0"}}]}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}

	rec := httptest.NewRecorder()
	a.restoreModelOfficialPricing(rec, httptest.NewRequest(http.MethodPost, "/api/admin/models/manual-model/pricing/official", nil), "manual-model")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "pricing_sync_failed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertManualPricingUnchanged(t, a, routeID)
}

func TestRestoreOfficialPricingReplacesManualValuesAfterMatch(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	routeID := insertManualPricingRoute(t, a, "manual-model")
	a.pricingClient = &http.Client{Transport: pricingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusBadGateway
		body := "unavailable"
		if r.URL.String() == openRouterPricingURL {
			status = http.StatusOK
			body = `{"data":[{"id":"vendor/manual-model","canonical_slug":"vendor/manual-model","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.00000025"}}]}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}

	rec := httptest.NewRecorder()
	a.restoreModelOfficialPricing(rec, httptest.NewRequest(http.MethodPost, "/api/admin/models/manual-model/pricing/official", nil), "manual-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var input, cached, output int64
	var source string
	if err := a.db.QueryRow(`SELECT input_price_micros,cached_price_micros,output_price_micros,pricing_source FROM model_routes WHERE id=?`, routeID).Scan(&input, &cached, &output, &source); err != nil {
		t.Fatal(err)
	}
	if input != 1_000_000 || cached != 250_000 || output != 2_000_000 || source != openRouterPricingURL {
		t.Fatalf("official pricing input=%d cached=%d output=%d source=%q", input, cached, output, source)
	}
}

func TestModelPricingPatchRejectsInvalidPriceAndMissingModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/admin/models/missing/pricing", `{"input_price_micros":1}`, http.StatusNotFound},
		{"/api/admin/models/missing/pricing", `{"input_price_micros":-1}`, http.StatusBadRequest},
		{"/api/admin/models/missing/pricing", `{"input_price_micros":1000000000001}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		a.modelByName(rec, httptest.NewRequest(http.MethodPatch, tc.path, strings.NewReader(tc.body)), adminCtx{})
		if rec.Code != tc.status {
			t.Fatalf("body=%s status=%d want=%d response=%s", tc.body, rec.Code, tc.status, rec.Body.String())
		}
	}
}

func TestAPIKeyBudgetAndExpiryAdmission(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := "fg_budget_test_key"
	hash := sha256.Sum256([]byte(raw))
	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_images,rpm_limit,budget_micros,created_at) VALUES(?,?,?,?,?,?,?,?)`, "budget", "fg_budget", hex.EncodeToString(hash[:]), 1, 1, 120, 1_000_000, now())
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := res.LastInsertId()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	if _, ok := a.authenticateKey(req); !ok {
		t.Fatal("key should authenticate before its budget is spent")
	}
	// Spend is settled onto the key's running total, which is what admission reads.
	a.endLedger(a.startLedger(authKey{ID: keyID}, resolvedRoute{Route: Route{ID: 1, PublicName: "m", UpstreamModel: "m"}, Provider: Provider{ID: 1}}, "test", false, "127.0.0.1", "req_spent", "", 1, ""), 1, keyID, "openai", "m",
		true, 200, "", time.Now(), Usage{CostMicros: 1_000_000, CostType: "estimated", Reported: true})
	a.flushLedgerWrites()
	if _, ok := a.authenticateKey(req); !ok {
		t.Fatal("budget-exhausted valid key must authenticate so Router can return budget_exceeded")
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), `"code":"budget_exceeded"`) {
		t.Fatalf("exhausted key status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := a.db.Exec(`UPDATE api_keys SET budget_micros=0,expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), keyID); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.authenticateKey(req); ok {
		t.Fatal("expired key authenticated")
	}
}

func TestLiveOfficialPricingSources(t *testing.T) {
	if os.Getenv("FUSIONGATE_LIVE_PRICING_TEST") != "1" {
		t.Skip("set FUSIONGATE_LIVE_PRICING_TEST=1 to verify current official pricing pages")
	}
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := a.syncOfficialPricing(t.Context())
	if err != nil {
		t.Fatalf("live pricing sync: %v (result=%+v)", err, result)
	}
	if result.Sources != 5 || result.Models < 5 {
		t.Fatalf("unexpected live pricing result: %+v", result)
	}
}

func TestParseOpenRouterPricingConvertsPerTokenRatesAndAliases(t *testing.T) {
	catalog, err := parseOpenRouterPricing([]byte(`{"data":[{"id":"openai/gpt-test","canonical_slug":"openai/gpt-test-20260801","pricing":{"prompt":"0.000002","completion":"0.000008","input_cache_read":"0.0000002"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"openai/gpt-test", "gpt-test", "openai/gpt-test-20260801", "gpt-test-20260801"} {
		price, ok := catalog[alias]
		if !ok {
			t.Fatalf("missing alias %q in %#v", alias, catalog)
		}
		if price.InputMicros != 2_000_000 || price.CachedMicros != 200_000 || price.OutputMicros != 8_000_000 {
			t.Fatalf("alias %q price=%+v", alias, price)
		}
	}
}

func TestOfficialPricingUpdatesUnifiedModelAcrossCompatibleChannels(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	for i, upstream := range []string{"gpt-unified", "openai/gpt-unified"} {
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "compatible-"+string(rune('a'+i)), "openai_compatible", "https://api.example", []byte{1}, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		providerID, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "gpt-unified", providerID, upstream, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	catalogs := map[string]map[string]officialModelPrice{
		"openai": {"gpt-unified": {Model: "gpt-unified", InputMicros: 2_000_000, CachedMicros: 200_000, OutputMicros: 8_000_000, Source: openAIPricingURL}},
	}
	updated, warnings, err := a.applyOfficialPricing(t.Context(), catalogs, "gpt-unified", false)
	if err != nil || len(warnings) != 0 || updated != 2 {
		t.Fatalf("updated=%d warnings=%v err=%v", updated, warnings, err)
	}
	rows, err := a.db.Query(`SELECT input_price_micros,output_price_micros,pricing_source FROM model_routes WHERE public_name='gpt-unified'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var input, output int64
		var source string
		if err := rows.Scan(&input, &output, &source); err != nil {
			t.Fatal(err)
		}
		if input != 2_000_000 || output != 8_000_000 || source != openAIPricingURL {
			t.Fatalf("price=%d/%d source=%q", input, output, source)
		}
	}
}

func TestOpenRouterPricingFallbackUpdatesCompatibleModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stamp := now()
	res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, "router-fallback", "openai_compatible", "https://api.example", []byte{1}, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := res.LastInsertId()
	_, _ = a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "kimi-k2.5", providerID, "moonshotai/kimi-k2.5", stamp, stamp)
	catalogs := map[string]map[string]officialModelPrice{
		"openrouter": {"kimi-k2.5": {Model: "moonshotai/kimi-k2.5", InputMicros: 500_000, OutputMicros: 2_500_000, Source: openRouterPricingURL}},
	}
	updated, warnings, err := a.applyOfficialPricing(t.Context(), catalogs, "kimi-k2.5", false)
	if err != nil || len(warnings) != 0 || updated != 1 {
		t.Fatalf("updated=%d warnings=%v err=%v", updated, warnings, err)
	}
	var input, output int64
	var source string
	if err := a.db.QueryRow(`SELECT input_price_micros,output_price_micros,pricing_source FROM model_routes WHERE public_name='kimi-k2.5'`).Scan(&input, &output, &source); err != nil {
		t.Fatal(err)
	}
	if input != 500_000 || output != 2_500_000 || source != openRouterPricingURL {
		t.Fatalf("price=%d/%d source=%q", input, output, source)
	}
}

func TestModelImportTriggersDebouncedPricingSync(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "trigger-price", "openai_compatible", "https://api.example", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	result, err := a.importDiscoveredModels(t.Context(), providerID, []discoveredModel{{ID: "trigger-model", UpstreamID: "trigger-model", Capabilities: "chat,stream"}}, false)
	if err != nil || result.Added != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	select {
	case <-a.pricingSyncTrigger:
	case <-time.After(time.Second):
		t.Fatal("pricing sync was not triggered")
	}
}
