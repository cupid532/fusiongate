package fusiongate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

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

func TestLookupOfficialPriceAcceptsLatestAndDatedVersions(t *testing.T) {
	catalog := map[string]officialModelPrice{"gpt-5": {Model: "gpt-5", InputMicros: 1}}
	for _, model := range []string{"gpt-5", "gpt-5-latest", "gpt-5-2026-07-01", "models/gpt-5"} {
		if _, ok := lookupOfficialPrice(catalog, model); !ok {
			t.Fatalf("expected price match for %q", model)
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
	for i, name := range []string{"one", "two"} {
		res, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,created_at,updated_at) VALUES(?,?,?,?,?,?)`, name, "openai", "https://api.example", []byte{1}, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,created_at,updated_at) VALUES(?,?,?,?,?)`, "shared", id, "model-"+name, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			_, _ = a.db.Exec(`INSERT INTO route_policies(public_name,strategy,updated_at) VALUES(?,?,?)`, "shared", StrategyPriorityFailover, stamp)
		}
	}
	recorder := httptest.NewRecorder()
	a.modelByName(recorder, httptest.NewRequest(http.MethodDelete, "/api/admin/models/shared", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var routes, policies int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE public_name='shared'`).Scan(&routes)
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM route_policies WHERE public_name='shared'`).Scan(&policies)
	if routes != 0 || policies != 0 {
		t.Fatalf("routes=%d policies=%d, expected complete deletion", routes, policies)
	}
}

func TestAPIKeyBudgetAndExpiryStopAuthentication(t *testing.T) {
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
	stamp := now()
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,completed_at,api_key_id,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?)`, "spent", stamp, stamp, keyID, "m", "m", "test", 1_000_000, "estimated"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.authenticateKey(req); ok {
		t.Fatal("budget-exhausted key authenticated")
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
	if result.Sources != 3 || result.Models < 3 {
		t.Fatalf("unexpected live pricing result: %+v", result)
	}
}
