package fusiongate

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func patchProviderForTest(t *testing.T, a *App, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(id), strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.providerByID(rec, req, adminCtx{})
	return rec
}

func TestEditAPIKeyProviderConnectionAndCredential(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	id := insertTestProvider(t, a, "old-name", "openai_compatible", "http://old.test", "old-secret", 1, 100, "normalized", "any", 0, 3, 30)
	routeID := insertTestRoute(t, a, id, "stable-model", "upstream-model", "chat,stream", 1)
	if _, err := a.db.Exec(`UPDATE providers SET status='circuit_open',consecutive_failures=7,circuit_open_until=?,last_error='old failure',last_failure_at=? WHERE id=?`, now(), now(), id); err != nil {
		t.Fatal(err)
	}

	rec := patchProviderForTest(t, a, id, `{
		"name":" updated-name ",
		"type":"anthropic",
		"baseURL":"http://updated.test/",
		"credential":" new-secret ",
		"priority":4,
		"weight":75,
		"notes":"rotated key",
		"passthrough_mode":"transparent",
		"client_policy":"claude_code",
		"max_concurrency":8,
		"request_timeout_ms":45000,
		"failure_threshold":5,
		"cooldown_seconds":90
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		OK                bool `json:"ok"`
		CredentialUpdated bool `json:"credential_updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !response.CredentialUpdated {
		t.Fatalf("patch response=%+v", response)
	}

	var name, providerType, baseURL, notes, passthroughMode, clientPolicy, status, lastError string
	var credential []byte
	var priority, weight, maxConcurrency, requestTimeoutMS, failureThreshold, cooldownSeconds, failures int
	var circuitOpenUntil, lastFailureAt sql.NullString
	if err := a.db.QueryRow(`SELECT name,type,base_url,credential,priority,weight,notes,passthrough_mode,client_policy,max_concurrency,request_timeout_ms,failure_threshold,cooldown_seconds,status,consecutive_failures,circuit_open_until,last_error,last_failure_at FROM providers WHERE id=?`, id).Scan(
		&name, &providerType, &baseURL, &credential, &priority, &weight, &notes, &passthroughMode, &clientPolicy, &maxConcurrency, &requestTimeoutMS, &failureThreshold, &cooldownSeconds, &status, &failures, &circuitOpenUntil, &lastError, &lastFailureAt,
	); err != nil {
		t.Fatal(err)
	}
	plain, err := a.decrypt(credential)
	if err != nil {
		t.Fatal(err)
	}
	if name != "updated-name" || providerType != "anthropic" || baseURL != "http://updated.test" || plain != "new-secret" {
		t.Fatalf("connection update name=%q type=%q base=%q credential=%q", name, providerType, baseURL, plain)
	}
	if priority != 4 || weight != 75 || notes != "rotated key" || passthroughMode != "transparent" || clientPolicy != "claude_code" || maxConcurrency != 8 || requestTimeoutMS != 45000 || failureThreshold != 5 || cooldownSeconds != 90 {
		t.Fatalf("scheduling update priority=%d weight=%d notes=%q passthrough=%q policy=%q concurrency=%d timeout=%d threshold=%d cooldown=%d", priority, weight, notes, passthroughMode, clientPolicy, maxConcurrency, requestTimeoutMS, failureThreshold, cooldownSeconds)
	}
	if status != "unknown" || failures != 0 || circuitOpenUntil.Valid || lastError != "" || lastFailureAt.Valid {
		t.Fatalf("health was not reset status=%q failures=%d circuit=%v error=%q last_failure=%v", status, failures, circuitOpenUntil, lastError, lastFailureAt)
	}
	var routeCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE id=? AND provider_id=?`, routeID, id).Scan(&routeCount); err != nil {
		t.Fatal(err)
	}
	if routeCount != 1 {
		t.Fatal("editing the provider removed its existing model route")
	}
}

func TestEditProviderBlankCredentialKeepsExistingSecret(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	id := insertTestProvider(t, a, "keep-key", "openai_compatible", "http://same.test", "existing-secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET status='degraded',consecutive_failures=2,last_error='temporary' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rec := patchProviderForTest(t, a, id, `{"name":"keep-key","type":"openai_compatible","baseURL":"http://same.test","credential":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		CredentialUpdated bool `json:"credential_updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.CredentialUpdated {
		t.Fatal("blank credential was reported as updated")
	}
	var credential []byte
	var status, lastError string
	var failures int
	if err := a.db.QueryRow(`SELECT credential,status,consecutive_failures,last_error FROM providers WHERE id=?`, id).Scan(&credential, &status, &failures, &lastError); err != nil {
		t.Fatal(err)
	}
	plain, err := a.decrypt(credential)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "existing-secret" {
		t.Fatalf("blank credential replaced existing secret with %q", plain)
	}
	if status != "degraded" || failures != 2 || lastError != "temporary" {
		t.Fatalf("unchanged connection unexpectedly reset health status=%q failures=%d error=%q", status, failures, lastError)
	}
}

func TestEditOAuthProviderConnectionIsRejected(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	id := insertTestProvider(t, a, "oauth-provider", "grok_oauth", "https://api.x.ai", "oauth-secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET auth_kind='oauth' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rec := patchProviderForTest(t, a, id, `{"credential":"replacement"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "credential files") {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var credential []byte
	if err := a.db.QueryRow(`SELECT credential FROM providers WHERE id=?`, id).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	plain, err := a.decrypt(credential)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "oauth-secret" {
		t.Fatal("rejected OAuth edit changed the stored credential")
	}
}

func TestEditProviderValidatesConnectionFields(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertTestProvider(t, a, "validated", "openai_compatible", "http://valid.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty name", body: `{"name":"  "}`},
		{name: "unsupported type", body: `{"type":"grok_oauth"}`},
		{name: "invalid URL", body: `{"baseURL":"not-a-url"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchProviderForTest(t, a, id, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
