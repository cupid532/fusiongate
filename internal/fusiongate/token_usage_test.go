package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func insertUsageFixture(t *testing.T, a *App, requestID, gatewayID, created string, keyID, providerID int64, keyName, keyPrefix, providerName, publicModel, upstreamModel string, input, output, cached, reasoning int64, reported bool) {
	t.Helper()
	_, err := a.db.Exec(`INSERT INTO request_ledger(
		request_id,gateway_request_id,created_at,completed_at,api_key_id,provider_id,
		public_model,upstream_model,protocol,success,status_code,input_tokens,output_tokens,
		cached_tokens,reasoning_tokens,usage_reported,api_key_name,api_key_prefix,provider_name
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		requestID, gatewayID, created, created, keyID, providerID,
		publicModel, upstreamModel, "test", 1, http.StatusOK, input, output,
		cached, reasoning, boolInt(reported), keyName, keyPrefix, providerName,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTokenUsageBreaksDownByDateKeyProviderAndModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	created := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	insertUsageFixture(t, a, "r1", "g1", created, 11, 21, "desktop", "fg_live_a", "primary", "smart", "upstream-smart", 100, 20, 10, 5, true)
	insertUsageFixture(t, a, "r2", "g2", created, 11, 21, "desktop", "fg_live_a", "primary", "smart", "upstream-smart", 50, 10, 4, 2, true)
	insertUsageFixture(t, a, "r3", "g3", created, 12, 22, "automation", "fg_live_b", "backup", "fast", "upstream-fast", 40, 30, 0, 8, true)
	insertUsageFixture(t, a, "r4", "g4", created, 12, 22, "automation", "fg_live_b", "backup", "fast", "upstream-fast", 0, 0, 0, 0, false)

	recorder := httptest.NewRecorder()
	a.tokenUsage(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/token-usage?days=30", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response tokenUsageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Totals.Requests != 4 || response.Totals.Attempts != 4 || response.Totals.ReportedRequests != 3 {
		t.Fatalf("unexpected request totals: %+v", response.Totals)
	}
	if response.Totals.InputTokens != 190 || response.Totals.OutputTokens != 60 || response.Totals.TotalTokens != 250 {
		t.Fatalf("unexpected token totals: %+v", response.Totals)
	}
	if response.Totals.CachedTokens != 14 || response.Totals.ReasoningTokens != 15 || response.Totals.UsageCoverage != 75 {
		t.Fatalf("unexpected detailed totals: %+v", response.Totals)
	}
	if len(response.Series) != 30 || len(response.ByKeys) != 2 || len(response.ByProviders) != 2 || len(response.ByModels) != 2 || len(response.Details) != 2 {
		t.Fatalf("unexpected response dimensions: series=%d keys=%d providers=%d models=%d details=%d", len(response.Series), len(response.ByKeys), len(response.ByProviders), len(response.ByModels), len(response.Details))
	}
	if response.Details[0].APIKeyName != "desktop" || response.Details[0].ProviderName != "primary" || response.Details[0].PublicModel != "smart" || response.Details[0].TotalTokens != 180 {
		t.Fatalf("unexpected leading detail: %+v", response.Details[0])
	}

	filtered := httptest.NewRecorder()
	a.tokenUsage(filtered, httptest.NewRequest(http.MethodGet, "/api/admin/token-usage?days=30&api_key_id=11&provider_id=21&model=SMART", nil), adminCtx{})
	if filtered.Code != http.StatusOK {
		t.Fatalf("filtered status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	var filteredResponse tokenUsageResponse
	if err := json.Unmarshal(filtered.Body.Bytes(), &filteredResponse); err != nil {
		t.Fatal(err)
	}
	if filteredResponse.Totals.Requests != 2 || filteredResponse.Totals.TotalTokens != 180 || len(filteredResponse.Details) != 1 {
		t.Fatalf("unexpected filtered response: totals=%+v details=%+v", filteredResponse.Totals, filteredResponse.Details)
	}
}

func TestRequestLedgerRetentionKeepsOnlyOneYear(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	current := time.Now().UTC()
	oldDate := current.AddDate(-1, 0, -2).Format(time.RFC3339Nano)
	recentDate := current.AddDate(-1, 0, 2).Format(time.RFC3339Nano)
	insertUsageFixture(t, a, "old", "old", oldDate, 1, 1, "key", "fg_old", "provider", "model", "model", 10, 2, 0, 0, true)
	insertUsageFixture(t, a, "recent", "recent", recentDate, 1, 1, "key", "fg_new", "provider", "model", "model", 20, 3, 0, 0, true)

	if err := a.pruneRequestLedger(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	var oldCount, recentCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE request_id='old'`).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE request_id='recent'`).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || recentCount != 1 {
		t.Fatalf("retention result old=%d recent=%d", oldCount, recentCount)
	}
}

func TestLedgerSnapshotsSurviveKeyAndProviderDeletion(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	created := time.Now().UTC().Format(time.RFC3339Nano)
	insertUsageFixture(t, a, "snapshot", "snapshot", created, 91, 92, "deleted-client", "fg_snap", "deleted-provider", "public-model", "upstream-model", 15, 5, 2, 1, true)

	recorder := httptest.NewRecorder()
	a.tokenUsage(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/token-usage?days=7", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response tokenUsageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Details) != 1 || response.Details[0].APIKeyName != "deleted-client" || response.Details[0].APIKeyPrefix != "fg_snap" || response.Details[0].ProviderName != "deleted-provider" {
		t.Fatalf("snapshots were not preserved: %+v", response.Details)
	}
}

func TestUsageReportedDistinguishesMissingFromExplicitZero(t *testing.T) {
	missing := parseOpenAIUsage(map[string]any{"id": "response"})
	if missing.Reported {
		t.Fatalf("missing usage marked as reported: %+v", missing)
	}
	explicitZero := parseOpenAIUsage(map[string]any{"usage": map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}})
	if !explicitZero.Reported || explicitZero.Input != 0 || explicitZero.Output != 0 {
		t.Fatalf("explicit zero usage not preserved: %+v", explicitZero)
	}
}

func TestProviderUsageParsersCoverResponsesAnthropicAndGeminiDetails(t *testing.T) {
	responses := parseOpenAIUsage(map[string]any{"response": map[string]any{"usage": map[string]any{
		"input_tokens":          120,
		"output_tokens":         35,
		"input_tokens_details":  map[string]any{"cached_tokens": 40},
		"output_tokens_details": map[string]any{"reasoning_tokens": 12},
	}}})
	if !responses.Reported || responses.Input != 120 || responses.Output != 35 || responses.Cached != 40 || responses.Reasoning != 12 {
		t.Fatalf("responses usage=%+v", responses)
	}

	anthropic := parseAnthropicUsage(map[string]any{"usage": map[string]any{
		"input_tokens":                30,
		"cache_creation_input_tokens": 11,
		"cache_read_input_tokens":     59,
		"output_tokens":               20,
		"output_tokens_details":       map[string]any{"thinking_tokens": 7},
	}})
	if !anthropic.Reported || anthropic.Input != 100 || anthropic.Output != 20 || anthropic.Cached != 59 || anthropic.Reasoning != 7 {
		t.Fatalf("anthropic usage=%+v", anthropic)
	}

	gemini := parseGeminiUsage(map[string]any{"usageMetadata": map[string]any{
		"promptTokenCount":        80,
		"candidatesTokenCount":    25,
		"cachedContentTokenCount": 30,
		"thoughtsTokenCount":      15,
		"totalTokenCount":         120,
	}})
	if !gemini.Reported || gemini.Input != 80 || gemini.Output != 40 || gemini.Cached != 30 || gemini.Reasoning != 15 {
		t.Fatalf("gemini usage=%+v", gemini)
	}
}

func TestAnthropicSSEUsageObserverMergesStartAndDeltaUsage(t *testing.T) {
	observer := &sseUsageObserver{usage: Usage{CostType: "unknown"}, usageFormat: "anthropic"}
	payload := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_creation_input_tokens\":5,\"cache_read_input_tokens\":10,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":18}}\n\n")
	if _, err := observer.Write(payload); err != nil {
		t.Fatal(err)
	}
	usage := observer.finish()
	if !usage.Reported || usage.Input != 35 || usage.Output != 18 || usage.Cached != 10 {
		t.Fatalf("merged anthropic stream usage=%+v", usage)
	}
}

func TestTokenUsageRejectsInvalidFilters(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, query := range []string{
		"days=31",
		"api_key_id=abc",
		"api_key_id=0",
		"provider_id=-1",
		"page=0",
		"page_size=101",
	} {
		recorder := httptest.NewRecorder()
		a.tokenUsage(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/token-usage?"+query, nil), adminCtx{})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("query %q status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
	}

	year := httptest.NewRecorder()
	a.tokenUsage(year, httptest.NewRequest(http.MethodGet, "/api/admin/token-usage?days=365", nil), adminCtx{})
	if year.Code != http.StatusOK {
		t.Fatalf("365-day status=%d body=%s", year.Code, year.Body.String())
	}
	var response tokenUsageResponse
	if err := json.Unmarshal(year.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Series) != 365 || response.Period.Days != 365 {
		t.Fatalf("365-day series=%d period=%d", len(response.Series), response.Period.Days)
	}
}

func TestNewPrunesExpiredLedgerRowsOnStartup(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC()
	insertUsageFixture(t, a, "startup-old", "startup-old", current.AddDate(-1, 0, -1).Format(time.RFC3339Nano), 1, 1, "key", "fg_old", "provider", "model", "model", 10, 2, 0, 0, true)
	insertUsageFixture(t, a, "startup-recent", "startup-recent", current.AddDate(-1, 0, 1).Format(time.RFC3339Nano), 1, 1, "key", "fg_new", "provider", "model", "model", 20, 3, 0, 0, true)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var oldCount, recentCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE request_id='startup-old'`).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM request_ledger WHERE request_id='startup-recent'`).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || recentCount != 1 {
		t.Fatalf("startup retention result old=%d recent=%d", oldCount, recentCount)
	}
}
