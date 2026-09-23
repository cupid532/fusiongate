package fusiongate

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLedgerMinuteFiltersSnapshotsTotalsPaginationAndCSV(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	provider := insertTestProvider(t, a, "ledger-channel", "openai_compatible", "https://example.test", "upstream-secret", 1, 1, "normalized", "any", 0, 3, 30)
	key := authKey{ID: 91, Name: "desktop snapshot", Prefix: "fg_safe"}
	if _, err := a.db.Exec(`INSERT INTO api_keys(id,name,key_prefix,key_hash,created_at) VALUES(91,'renamed','fg_safe','private-hash',?)`, now()); err != nil {
		t.Fatal(err)
	}
	route := resolvedRoute{Provider: Provider{ID: provider, Name: "ledger-channel"}, Route: Route{ID: 1, PublicName: "focus", UpstreamModel: "upstream"}}
	for _, row := range []struct {
		stamp, model string
		success      bool
	}{
		{"2026-09-23T06:39:59.999999999Z", "focus", true},
		{"2026-09-23T06:40:00Z", "focus", true},
		{"2026-09-23T06:40:00.1Z", "focus", true},
		{"2026-09-23T06:40:59.999999999Z", "focus", true},
		{"2026-09-23T06:41:00Z", "focus", true},
		{"2026-09-23T06:41:00.000000001Z", "focus", true},
		{"2026-09-23T06:40:30Z", "other", true},
		{"2026-09-23T06:40:30Z", "focus", false},
	} {
		route.Route.PublicName = row.model
		id := a.startLedger(key, route, "openai_chat", false, "127.0.0.1", row.stamp+row.model, "", 1, "")
		a.endLedger(id, provider, key.ID, "openai", route.Route.UpstreamModel, row.success, 200, "", time.Now(), Usage{Input: 2, Output: 3, Reported: true})
		a.flushLedgerWrites()
		if _, err := a.db.Exec(`UPDATE request_ledger SET created_at=?,completed_at=? WHERE request_id=?`, row.stamp, row.stamp, id); err != nil {
			t.Fatal(err)
		}
	}
	params := url.Values{"from": {"2026-09-23T14:40:00+08:00"}, "until": {"2026-09-23T14:41:00+08:00"}, "api_key_id": {"91"}, "provider_id": {intString(provider)}, "model": {"focus"}, "status": {"success"}, "q": {"desktop snapshot"}, "limit": {"2"}}
	type item struct {
		ID     int64  `json:"id"`
		Name   string `json:"api_key_name"`
		Prefix string `json:"api_key_prefix"`
	}
	type result struct {
		Items  []item `json:"items"`
		Total  int    `json:"total"`
		Totals struct {
			Requests int `json:"requests"`
			Tokens   int `json:"total_tokens"`
		} `json:"totals"`
	}
	read := func() result {
		t.Helper()
		r := httptest.NewRecorder()
		a.requests(r, httptest.NewRequest("GET", "/api/admin/requests?"+params.Encode(), nil), adminCtx{})
		if r.Code != 200 {
			t.Fatalf("list status=%d body=%s", r.Code, r.Body.String())
		}
		var out result
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := read()
	if first.Total != 3 || first.Totals.Requests != 3 || first.Totals.Tokens != 15 || len(first.Items) != 2 {
		t.Fatalf("first=%+v", first)
	}
	for _, r := range first.Items {
		if r.Name != key.Name || r.Prefix != key.Prefix {
			t.Fatalf("snapshot=%+v", r)
		}
	}
	params.Set("before", intString(first.Items[1].ID))
	second := read()
	if second.Total != 3 || second.Totals.Tokens != 15 || len(second.Items) != 1 {
		t.Fatalf("second=%+v", second)
	}
	params.Del("before")
	rec := httptest.NewRecorder()
	a.ledgerExport(rec, httptest.NewRequest("GET", "/api/admin/ledger/export?"+params.Encode(), nil), adminCtx{})
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(records) != 4 {
		t.Fatalf("CSV status=%d rows=%d", rec.Code, len(records))
	}
	if strings.Contains(rec.Body.String(), "private-hash") || strings.Contains(rec.Body.String(), "upstream-secret") {
		t.Fatal("CSV exposed credentials")
	}
	// Model filtering is independent of key selection.
	params.Del("api_key_id")
	params.Del("limit")
	if got := read(); got.Total != 3 {
		t.Fatalf("model-only total=%d", got.Total)
	}
	for _, endpoint := range []string{"/api/admin/requests", "/api/admin/ledger/export"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", endpoint+"?until=not-a-time", nil)
		if strings.Contains(endpoint, "export") {
			a.ledgerExport(rec, req, adminCtx{})
		} else {
			a.requests(rec, req, adminCtx{})
		}
		if rec.Code != 400 {
			t.Fatalf("invalid until status=%d", rec.Code)
		}
	}
}

func TestAuthCatalogSyncRefreshesExistingAndSelectedBeyondFirst200(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	catalog := `{"models":[{"slug":"future-2099"}]}`
	a.client = &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) { return authJSONResponse(200, catalog), nil })}
	id, _, err := a.saveOAuthProvider(context.Background(), "existing-catalog", 1, ProviderCredential{Kind: "oauth", Platform: "codex", Source: "json", AccountID: "first", AccessToken: "safe-fixture"}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, id, "old-model", "old-model", "chat,stream", 1)
	sync := func(body string) {
		t.Helper()
		r := httptest.NewRecorder()
		a.authModelSync(r, httptest.NewRequest("POST", "/api/admin/auth/models/sync", strings.NewReader(body)), adminCtx{})
		if r.Code != 200 {
			t.Fatalf("sync=%d %s", r.Code, r.Body.String())
		}
	}
	sync(`{}`)
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=? AND upstream_model='future-2099'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("new catalog model missing: n=%d err=%v", n, err)
	}
	if _, err := a.db.Exec(`WITH RECURSIVE ids(n) AS (VALUES(2) UNION ALL SELECT n+1 FROM ids WHERE n<202)
 INSERT INTO providers(id,name,type,base_url,credential,auth_kind,auth_source,created_at,updated_at)
 SELECT n,'account-'||n,p.type,p.base_url,p.credential,'oauth','json',p.created_at,p.updated_at FROM ids JOIN providers p ON p.id=?`, id); err != nil {
		t.Fatal(err)
	}
	catalog = `{"models":[{"model_id":"future-2100"}]}`
	sync(`{"provider_ids":[202]}`)
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=202 AND upstream_model='future-2100'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("selected account beyond 200 skipped: n=%d err=%v", n, err)
	}
}

func TestOAuthRefreshFailureDoesNotExposeUnlabelledTokens(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var logs bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	secret := "eyJhbGciOiJub25lIn0.private-secret-signature"
	a.client = &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return authJSONResponse(400, secret+" invalid_grant"), nil
	})}
	cred := ProviderCredential{Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth", AccountID: "safe-account", AccessToken: "stale-fixture", RefreshToken: "private-refresh-fixture", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	id, _, err := a.saveOAuthProvider(context.Background(), "refresh-safety", 1, cred, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	z := resolvedRoute{Provider: Provider{ID: id}, Credential: cred.AccessToken, AuthCredential: &cred}
	err = a.ensureFreshProviderCredential(context.Background(), &z)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	var detail string
	if e := a.db.QueryRow(`SELECT last_error FROM providers WHERE id=?`, id).Scan(&detail); e != nil {
		t.Fatal(e)
	}
	for _, text := range []string{err.Error(), detail, logs.String()} {
		for _, v := range []string{secret, cred.RefreshToken, cred.AccessToken} {
			if strings.Contains(text, v) {
				t.Fatal("refresh error leaked token")
			}
		}
	}
	if !strings.Contains(detail, "invalid_grant") {
		t.Fatalf("permanent category lost: %s", detail)
	}
	if !strings.Contains(safeOAuthRefreshFailure(context.DeadlineExceeded), "timed out") {
		t.Fatal("timeout classification lost")
	}
	if strings.Contains(safeOAuthRefreshFailure(errors.New(secret)), secret) {
		t.Fatal("generic refresh leaked error text")
	}
}
