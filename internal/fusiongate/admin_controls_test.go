package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeleteKeyRemovesItAndPreservesLedgerWithoutKeyReference(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	create := httptest.NewRecorder()
	a.keys(create, httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(`{"name":"delete-me","allow_all":true}`)), adminCtx{})
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var key struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,api_key_id,public_model,upstream_model,protocol) VALUES(?,?,?,?,?,?)`, "key-delete-ledger", now(), key.ID, "model", "model", "test"); err != nil {
		t.Fatal(err)
	}

	deleted := httptest.NewRecorder()
	a.keyByID(deleted, httptest.NewRequest(http.MethodDelete, "/api/admin/keys/"+intString(key.ID), nil), adminCtx{})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	var keyCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id=?`, key.ID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 0 {
		t.Fatalf("deleted key remains in database: %d", keyCount)
	}
	var ledgerKeyID any
	if err := a.db.QueryRow(`SELECT api_key_id FROM request_ledger WHERE request_id='key-delete-ledger'`).Scan(&ledgerKeyID); err != nil {
		t.Fatal(err)
	}
	if ledgerKeyID != nil {
		t.Fatalf("ledger retained deleted key reference: %#v", ledgerKeyID)
	}

	list := httptest.NewRecorder()
	a.keys(list, httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil), adminCtx{})
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "delete-me") {
		t.Fatalf("deleted key appeared in list: status=%d body=%s", list.Code, list.Body.String())
	}
	reveal := httptest.NewRecorder()
	a.keyByID(reveal, httptest.NewRequest(http.MethodPost, "/api/admin/keys/"+intString(key.ID)+"/reveal", nil), adminCtx{})
	if reveal.Code != http.StatusNotFound {
		t.Fatalf("reveal deleted key status=%d body=%s", reveal.Code, reveal.Body.String())
	}
}

func TestGlobalRoutingStrategyAndTokenAccounting(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	get := httptest.NewRecorder()
	a.routing(get, httptest.NewRequest(http.MethodGet, "/api/admin/routing", nil), adminCtx{})
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), string(StrategyPriorityFailover)) {
		t.Fatalf("default routing=%d %s", get.Code, get.Body.String())
	}
	set := httptest.NewRecorder()
	a.routing(set, httptest.NewRequest(http.MethodPatch, "/api/admin/routing", strings.NewReader(`{"strategy":"ordered_round_robin"}`)), adminCtx{})
	if set.Code != http.StatusOK || a.globalRoutingStrategy() != StrategyOrderedRoundRobin {
		t.Fatalf("set routing=%d %s strategy=%s", set.Code, set.Body.String(), a.globalRoutingStrategy())
	}
	invalid := httptest.NewRecorder()
	a.routing(invalid, httptest.NewRequest(http.MethodPatch, "/api/admin/routing", strings.NewReader(`{"strategy":"not-a-strategy"}`)), adminCtx{})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid routing status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,completed_at,public_model,upstream_model,protocol,input_tokens,output_tokens,cached_tokens,reasoning_tokens) VALUES(?,?,?,?,?,?,?,?,?,?)`, "token-one", now(), now(), "model", "model", "test", 120, 30, 10, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,completed_at,public_model,upstream_model,protocol,input_tokens,output_tokens,cached_tokens,reasoning_tokens) VALUES(?,?,?,?,?,?,?,?,?,?)`, "token-two", now(), now(), "model", "model", "test", 80, 20, 3, 2); err != nil {
		t.Fatal(err)
	}

	dashboard := httptest.NewRecorder()
	a.dashboard(dashboard, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil), adminCtx{})
	if dashboard.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", dashboard.Code, dashboard.Body.String())
	}
	var summary struct {
		Input     int64 `json:"input_tokens"`
		Output    int64 `json:"output_tokens"`
		Cached    int64 `json:"cached_tokens"`
		Reasoning int64 `json:"reasoning_tokens"`
		Total     int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal(dashboard.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Input != 200 || summary.Output != 50 || summary.Cached != 13 || summary.Reasoning != 9 || summary.Total != 250 {
		t.Fatalf("dashboard token summary=%+v", summary)
	}

	requests := httptest.NewRecorder()
	a.requests(requests, httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil), adminCtx{})
	if requests.Code != http.StatusOK || !strings.Contains(requests.Body.String(), `"cached_tokens":3`) || !strings.Contains(requests.Body.String(), `"reasoning_tokens":2`) || !strings.Contains(requests.Body.String(), `"total_tokens":100`) {
		t.Fatalf("request token response=%d %s", requests.Code, requests.Body.String())
	}
}

func TestSSEUsageObserverReadsUsageWithoutChangingStream(t *testing.T) {
	observer := &sseUsageObserver{usage: Usage{CostType: "unknown"}}
	payload := []byte("data: {\"choices\":[]}\r\n\r\ndata: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\r\n\r\ndata: [DONE]\r\n\r\n")
	for _, chunk := range [][]byte{payload[:19], payload[19:73], payload[73:]} {
		if n, err := observer.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("observer write n=%d err=%v", n, err)
		}
	}
	usage := observer.finish()
	if !usage.Reported || usage.Input != 12 || usage.Output != 5 || usage.Cached != 3 || usage.Reasoning != 2 {
		t.Fatalf("stream usage=%+v", usage)
	}
}
