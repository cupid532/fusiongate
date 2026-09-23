package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRequestLedgerAccessKeyAttributionFiltersAndExclusiveUntil(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,created_at) VALUES(?,?,?,?)`, "current access", "fg_current", "hash-request-access", now())
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := res.LastInsertId()
	for _, row := range []struct {
		id, created, name, prefix string
	}{
		{"snapshot", "2024-02-29T12:00:00Z", "historic access", "fg_history"},
		{"fallback", "2024-02-29T12:30:00Z", "", ""},
		{"boundary", "2024-03-01T00:00:00Z", "historic access", "fg_history"},
	} {
		if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,completed_at,api_key_id,api_key_name,api_key_prefix,public_model,upstream_model,protocol,success) VALUES(?,?,?,?,?,?,?,?,?,1)`, row.id, row.created, row.created, keyID, row.name, row.prefix, "model", "model", "test"); err != nil {
			t.Fatal(err)
		}
	}

	request := func(query string) struct {
		Items []struct {
			RequestID    string `json:"request_id"`
			APIKeyID     int64  `json:"api_key_id"`
			APIKeyName   string `json:"api_key_name"`
			APIKeyPrefix string `json:"api_key_prefix"`
		} `json:"items"`
	} {
		t.Helper()
		recorder := httptest.NewRecorder()
		a.requests(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/requests?"+query, nil), adminCtx{})
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
		var payload struct {
			Items []struct {
				RequestID    string `json:"request_id"`
				APIKeyID     int64  `json:"api_key_id"`
				APIKeyName   string `json:"api_key_name"`
				APIKeyPrefix string `json:"api_key_prefix"`
			} `json:"items"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	until := url.QueryEscape("2024-03-01T00:00:00Z")
	payload := request("api_key_id=" + intString(keyID) + "&from=" + url.QueryEscape("2024-02-29T00:00:00Z") + "&until=" + until)
	if len(payload.Items) != 2 {
		t.Fatalf("exclusive until returned %+v", payload.Items)
	}
	byID := map[string]struct{ name, prefix string }{}
	for _, item := range payload.Items {
		if item.APIKeyID != keyID {
			t.Fatalf("api key id=%d want %d", item.APIKeyID, keyID)
		}
		byID[item.RequestID] = struct{ name, prefix string }{item.APIKeyName, item.APIKeyPrefix}
	}
	if got := byID["snapshot"]; got.name != "historic access" || got.prefix != "fg_history" {
		t.Fatalf("snapshot attribution=%+v", got)
	}
	if got := byID["fallback"]; got.name != "current access" || got.prefix != "fg_current" {
		t.Fatalf("fallback attribution=%+v", got)
	}
	if got := request("q=historic%20access"); len(got.Items) != 2 {
		t.Fatalf("snapshot name search=%+v", got.Items)
	}
	if got := request("q=fg_current"); len(got.Items) != 1 || got.Items[0].RequestID != "fallback" {
		t.Fatalf("fallback prefix search=%+v", got.Items)
	}

	invalid := httptest.NewRecorder()
	a.requests(invalid, httptest.NewRequest(http.MethodGet, "/api/admin/requests?api_key_id=0", nil), adminCtx{})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid api_key_id status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestLedgerExportIncludesSafeAccessKeyAttributionAndExclusiveUntil(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	res, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,created_at) VALUES(?,?,?,?)`, "export current", "fg_export", "hash-export-access", now())
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := res.LastInsertId()
	for _, row := range []struct{ id, created string }{{"included", "2025-01-01T00:00:00Z"}, {"excluded", "2025-01-02T00:00:00Z"}} {
		if _, err := a.db.Exec(`INSERT INTO request_ledger(request_id,created_at,completed_at,api_key_id,api_key_name,api_key_prefix,public_model,upstream_model,protocol,success) VALUES(?,?,?,?,?,?,?,?,?,1)`, row.id, row.created, row.created, keyID, "export snapshot", "fg_safe", "model", "model", "test"); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	path := "/api/admin/ledger/export?api_key_id=" + intString(keyID) + "&until=" + url.QueryEscape("2025-01-02T00:00:00Z")
	a.ledgerExport(recorder, httptest.NewRequest(http.MethodGet, path, nil), adminCtx{})
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "访问秘钥名称") || !strings.Contains(body, "访问秘钥前缀") || !strings.Contains(body, "export snapshot") || !strings.Contains(body, "fg_safe") || !strings.Contains(body, "included") {
		t.Fatalf("export status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, "excluded") || strings.Contains(body, "hash-export-access") {
		t.Fatalf("export leaked excluded or credential material: %s", body)
	}
}
