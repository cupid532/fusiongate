package fusiongate

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderBalanceUnsupportedIncludesEstimatedSpend(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	stamp := now()
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,provider_id,provider_name,created_at,completed_at,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?,?)`, "priced", providerID, "relay", stamp, stamp, "m", "m", "chat", 250000, "estimated"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,provider_id,provider_name,created_at,completed_at,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?,?)`, "unknown", providerID, "relay", stamp, stamp, "m", "m", "chat", 0, "unknown"); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/"+intString(providerID)+"/balance", nil)
	a.providerByID(recorder, req, adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response providerBalanceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "unsupported" || response.Source != "fusiongate_ledger" {
		t.Fatalf("upstream=%#v", response.ProviderUpstreamBalance)
	}
	if response.EstimatedSpend.CostMicros != 250000 || response.EstimatedSpend.Requests != 2 || response.EstimatedSpend.PricedRequests != 1 || response.EstimatedSpend.CostCoverage != 50 {
		t.Fatalf("spend=%#v", response.EstimatedSpend)
	}
}

func TestProviderBalanceCodexUsesCacheAndRefresh(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", AccessToken: "access", AccountID: "account"}
	payload, _ := json.Marshal(credential)
	sealed, err := a.encrypt(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	result, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,auth_kind,auth_source,enabled,priority,weight,status,passthrough_mode,client_policy,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "codex", "codex_oauth", "https://chatgpt.com/backend-api/codex", sealed, "oauth", "fusiongate_oauth", 1, 1, 100, "unknown", "normalized", "any", 5000, 3, 30, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := result.LastInsertId()

	var usageCalls atomic.Int32
	a.client = &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls.Add(1)
			return authJSONResponse(http.StatusOK, `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":18000}}}`), nil
		case "/backend-api/wham/rate-limit-reset-credits":
			return authJSONResponse(http.StatusOK, `{"available_count":0}`), nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
	})}

	request := func(refresh bool) providerBalanceResponse {
		t.Helper()
		path := "/api/admin/providers/" + intString(providerID) + "/balance"
		if refresh {
			path += "?refresh=1"
		}
		recorder := httptest.NewRecorder()
		a.providerByID(recorder, httptest.NewRequest(http.MethodGet, path, nil), adminCtx{})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response providerBalanceResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	first := request(false)
	second := request(false)
	third := request(true)
	if first.Status != "available" || first.Quota == nil || first.Quota.Primary == nil || first.Quota.Primary.RemainingPercent != 75 {
		t.Fatalf("first=%#v", first)
	}
	if second.CheckedAt != first.CheckedAt || third.Status != "available" {
		t.Fatalf("cached=%#v refreshed=%#v", second, third)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usage calls=%d, want 2", usageCalls.Load())
	}
}

func TestProviderBalanceNotFound(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/providers/999/balance", nil), adminCtx{})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProviderManualBalanceUsesBaselineCategoryMultipliersAndGroupsModels(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "mixed relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	for _, model := range []string{"gpt-5.6-sol", "claude-sonnet-5", "grok-4.5", "deepseek-v3"} {
		insertTestRoute(t, a, providerID, model, model, "chat,stream", 1)
	}
	oldStamp := "2025-01-01T00:00:00Z"
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,provider_id,created_at,completed_at,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?)`, "old", providerID, oldStamp, oldStamp, "gpt-5.6-sol", "gpt-5.6-sol", "chat", 900000, "estimated"); err != nil {
		t.Fatal(err)
	}

	patch := `{"manual_balance_usd":5,"balance_multiplier_openai":2,"balance_multiplier_claude":1.5,"balance_multiplier_grok":0.5,"balance_multiplier_gemini":1,"balance_multiplier_other":3}`
	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(patch)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var baseline string
	if err := a.db.QueryRow(`SELECT balance_baseline_at FROM providers WHERE id=?`, providerID).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	for index, item := range []struct {
		model string
		cost  int64
	}{{"gpt-5.6-sol", 500000}, {"claude-sonnet-5", 400000}, {"grok-4.5", 200000}, {"deepseek-v3", 100000}} {
		requestID := "new-" + intString(int64(index))
		if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,provider_id,created_at,completed_at,public_model,upstream_model,protocol,cost_micros,cost_type) VALUES(?,?,?,?,?,?,?,?,?)`, requestID, providerID, baseline, baseline, item.model, item.model, "chat", item.cost, "estimated"); err != nil {
			t.Fatal(err)
		}
	}

	response, err := a.providerBalance(t.Context(), providerID, false)
	if err != nil {
		t.Fatal(err)
	}
	if response.Manual == nil {
		t.Fatal("manual balance missing")
	}
	// 0.5*2 + 0.4*1.5 + 0.2*0.5 + 0.1*3 = $2 adjusted spend.
	if response.Manual.AdjustedSpendMicros != 2_000_000 || response.Manual.RemainingMicros != 3_000_000 || response.Manual.UsedPercent != 40 || response.Manual.Requests != 4 || response.Manual.PricedRequests != 4 || response.Manual.CostCoverage != 100 {
		t.Fatalf("manual=%#v", response.Manual)
	}
	if response.Manual.SpendByCategory["openai"] != 1_000_000 || response.Manual.SpendByCategory["claude"] != 600_000 || response.Manual.SpendByCategory["grok"] != 100_000 || response.Manual.SpendByCategory["other"] != 300_000 {
		t.Fatalf("category spend=%#v", response.Manual.SpendByCategory)
	}
	if len(response.ModelGroups) != 4 || response.ModelGroups[0].Category != "openai" || response.ModelGroups[1].Category != "claude" || response.ModelGroups[2].Category != "grok" || response.ModelGroups[3].Category != "other" {
		t.Fatalf("groups=%#v", response.ModelGroups)
	}
}

func TestProviderManualBalanceCanBeCleared(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "clear relay", "openai_compatible", "https://relay.example/v1", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET manual_balance_micros=1000000,balance_baseline_at=? WHERE id=?`, now(), providerID); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"clear_manual_balance":true}`)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var balance sql.NullInt64
	var baseline sql.NullString
	if err := a.db.QueryRow(`SELECT manual_balance_micros,balance_baseline_at FROM providers WHERE id=?`, providerID).Scan(&balance, &baseline); err != nil {
		t.Fatal(err)
	}
	if balance.Valid || baseline.Valid {
		t.Fatalf("balance=%#v baseline=%#v", balance, baseline)
	}
}
