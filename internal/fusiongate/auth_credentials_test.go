package fusiongate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func unsignedJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
}

func TestParseCredentialImportsCompatibility(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	expires := time.Now().UTC().Add(time.Hour).Unix()
	codexJWT := unsignedJWT(map[string]any{
		"email":                       "USER@EXAMPLE.COM",
		"exp":                         expires,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-codex"},
	})
	content := fmt.Sprintf(`
{"type":"codex","access_token":%q,"refresh_token":"clip-refresh","id_token":%q,"last_refresh":"2026-07-22T10:00:00Z","unknown":"preserved-by-source"}
{"accounts":[
  {"name":"Claude imported","platform":"anthropic","type":"oauth","credentials":{"accessToken":"claude-access","refreshToken":"claude-refresh","email":"claude@example.com","expiresAt":%d}},
  {"platform":"openai","credentials":{"access_token":"sub-access","refresh_token":"sub-refresh","chatgpt_account_id":"acct-sub"}}
]}
`, codexJWT, codexJWT, (expires+3600)*1000)

	items, err := a.parseCredentialImports(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items=%d, want 3: %#v", len(items), items)
	}
	if got := items[0].Credential; got.Platform != "codex" || got.Source != "cliproxy" || got.Email != "user@example.com" || got.AccountID != "acct-codex" || got.RefreshToken != "clip-refresh" {
		t.Fatalf("CLIProxy Codex normalization = %#v", got)
	}
	if got := items[1].Credential; got.Platform != "claude" || got.Source != "sub2api" || got.Email != "claude@example.com" || got.RefreshToken != "claude-refresh" {
		t.Fatalf("sub2api Claude normalization = %#v", got)
	}
	if got := items[2].Credential; got.Platform != "codex" || got.Source != "sub2api" || got.AccountID != "acct-sub" {
		t.Fatalf("sub2api Codex normalization = %#v", got)
	}
	if items[1].Credential.ExpiresAt == "" || items[0].Credential.ExpiresAt == "" {
		t.Fatalf("expiry was not normalized: %#v %#v", items[0].Credential, items[1].Credential)
	}
}

func TestCredentialImportPreviewIsMaskedAndCommitIsEncrypted(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	content := `{"type":"codex","access_token":"access-super-secret","refresh_token":"refresh-super-secret","account_id":"account-123456789","email":"person@example.com"}`
	payload, _ := json.Marshal(map[string]any{"content": content})
	previewRec := httptest.NewRecorder()
	a.authImportPreview(previewRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/import/preview", strings.NewReader(string(payload))), adminCtx{})
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	for _, secret := range []string{"access-super-secret", "refresh-super-secret", "person@example.com", "account-123456789"} {
		if strings.Contains(previewRec.Body.String(), secret) {
			t.Fatalf("preview leaked %q: %s", secret, previewRec.Body.String())
		}
	}
	var preview struct {
		SessionID string                    `json:"session_id"`
		Items     []credentialImportPreview `json:"items"`
	}
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Email != "pe•••@example.com" || preview.Items[0].AccountID == "account-123456789" {
		t.Fatalf("unexpected preview: %#v", preview)
	}

	emptyPayload, _ := json.Marshal(map[string]any{"session_id": preview.SessionID, "selected": []int{}})
	emptyRec := httptest.NewRecorder()
	a.authImportCommit(emptyRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/import/commit", strings.NewReader(string(emptyPayload))), adminCtx{})
	if emptyRec.Code != http.StatusBadRequest {
		t.Fatalf("empty selection status=%d body=%s", emptyRec.Code, emptyRec.Body.String())
	}

	// A rejected commit must not consume the preview session.
	commitPayload, _ := json.Marshal(map[string]any{"session_id": preview.SessionID, "selected": []int{1}, "priority": 1})
	commitRec := httptest.NewRecorder()
	a.authImportCommit(commitRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/import/commit", strings.NewReader(string(commitPayload))), adminCtx{})
	if commitRec.Code != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", commitRec.Code, commitRec.Body.String())
	}
	var encrypted []byte
	var authKind, source string
	if err := a.db.QueryRow(`SELECT credential,auth_kind,auth_source FROM providers`).Scan(&encrypted, &authKind, &source); err != nil {
		t.Fatal(err)
	}
	if authKind != "oauth" || source != "cliproxy" {
		t.Fatalf("auth metadata kind=%q source=%q", authKind, source)
	}
	if strings.Contains(string(encrypted), "super-secret") {
		t.Fatal("database credential contains plaintext token")
	}
	plaintext, err := a.decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plaintext, "access-super-secret") || !strings.Contains(plaintext, "refresh-super-secret") {
		t.Fatalf("encrypted credential does not round trip: %s", plaintext)
	}
}

func TestDuplicateCredentialCanBeSkippedOrUpdated(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	original := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "cliproxy", AccessToken: "old-access", RefreshToken: "stable-refresh", AccountID: "same-account", Email: "same@example.com"}
	id, _, err := a.saveOAuthProvider(context.Background(), "existing", 1, original, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Source = "sub2api"
	updated.AccessToken = "new-access"
	if _, _, err := a.saveOAuthProvider(context.Background(), "ignored", 9, updated, id, false); err != errDuplicateCredential {
		t.Fatalf("duplicate skip error=%v", err)
	}
	if _, _, err := a.saveOAuthProvider(context.Background(), "ignored", 9, updated, id, true); err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	var source, name string
	if err := a.db.QueryRow(`SELECT name,credential,auth_source FROM providers WHERE id=?`, id).Scan(&name, &encrypted, &source); err != nil {
		t.Fatal(err)
	}
	plaintext, _ := a.decrypt(encrypted)
	if name != "existing" || source != "sub2api" || !strings.Contains(plaintext, "new-access") || strings.Contains(plaintext, "old-access") {
		t.Fatalf("updated provider name=%q source=%q credential=%s", name, source, plaintext)
	}
}

func TestCredentialExportRequiresAcknowledgementAndRoundTripsEncryptedOAuth(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "cliproxy", AccessToken: "export-access-secret", RefreshToken: "export-refresh-secret", IDToken: "export-id-secret", AccountID: "export-account", Email: "export@example.com", ExpiresAt: "2026-07-24T00:00:00Z"}
	id, _, err := a.saveOAuthProvider(context.Background(), "export account", 7, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}

	withoutAck, _ := json.Marshal(map[string]any{"provider_ids": []int64{id}})
	rec := httptest.NewRecorder()
	a.authExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/export", strings.NewReader(string(withoutAck))), adminCtx{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "export_confirmation_required") {
		t.Fatalf("without acknowledgement status=%d body=%s", rec.Code, rec.Body.String())
	}

	body, _ := json.Marshal(map[string]any{"provider_ids": []int64{id}, "acknowledge_sensitive_export": true})
	rec = httptest.NewRecorder()
	a.authExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/export", strings.NewReader(string(body))), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	for key, want := range map[string]string{"Cache-Control": "no-store", "Pragma": "no-cache", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := rec.Header().Get(key); got != want {
			t.Fatalf("header %s=%q, want %q", key, got, want)
		}
	}
	if rec.Header().Get("Content-Disposition") == "" {
		t.Fatal("missing download filename")
	}
	var bundle credentialExportBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Format != "fusiongate_auth_export" || len(bundle.Credentials) != 1 {
		t.Fatalf("unexpected bundle: %#v", bundle)
	}
	entry := bundle.Credentials[0]
	if entry.Type != "codex" || entry.Platform != "codex" || entry.Priority != 7 || !entry.Enabled || entry.AccessToken != "export-access-secret" || entry.RefreshToken != "export-refresh-secret" || entry.ChatGPTAccountID != "export-account" {
		t.Fatalf("unexpected export entry: %#v", entry)
	}
	var encrypted []byte
	if err := a.db.QueryRow(`SELECT credential FROM providers WHERE id=?`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), "export-access-secret") || strings.Contains(string(encrypted), "export-refresh-secret") {
		t.Fatal("database credential contains plaintext export token")
	}
}

func TestCredentialExportGrokShapeAndRejectsNonOAuth(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "grok", Source: "cliproxy", AccessToken: "grok-export-access", RefreshToken: "grok-export-refresh", AccountID: "grok-subject", Extra: map[string]any{"token_endpoint": "https://evil.example/token"}}
	id, _, err := a.saveOAuthProvider(context.Background(), "grok export", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	regularCredential, err := a.encrypt("ordinary-api-key")
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "ordinary", "openai", "https://api.example", regularCredential, 1, 1, 100, "unknown", "", now(), now())
	if err != nil {
		t.Fatal(err)
	}
	regularID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"provider_ids": []int64{id, regularID}, "acknowledge_sensitive_export": true})
	rec := httptest.NewRecorder()
	a.authExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/export", strings.NewReader(string(body))), adminCtx{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only existing OAuth") {
		t.Fatalf("mixed selection status=%d body=%s", rec.Code, rec.Body.String())
	}

	body, _ = json.Marshal(map[string]any{"provider_ids": []int64{id}, "acknowledge_sensitive_export": true})
	rec = httptest.NewRecorder()
	a.authExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/export", strings.NewReader(string(body))), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("grok export status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bundle credentialExportBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Credentials) != 1 || bundle.Credentials[0].Type != "xai" || bundle.Credentials[0].BaseURL != "https://cli-chat-proxy.grok.com/v1" || bundle.Credentials[0].TokenEndpoint != "" {
		t.Fatalf("unexpected Grok export: %#v", bundle.Credentials)
	}
}

func TestCredentialExportSelectionLimit(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ids := make([]int64, authExportMaxItems+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	body, _ := json.Marshal(map[string]any{"provider_ids": ids, "acknowledge_sensitive_export": true})
	rec := httptest.NewRecorder()
	a.authExport(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/export", strings.NewReader(string(body))), adminCtx{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 1 and 200") {
		t.Fatalf("limit status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCredentialImportRejectsContentOverBatchLimit(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	body, err := json.Marshal(map[string]any{"content": strings.Repeat("x", authImportMaxBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.authImportPreview(rec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/import/preview", strings.NewReader(string(body))), adminCtx{})
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "credential_file_too_large") {
		t.Fatalf("oversized import status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOAuthStartCompletePKCEStateAndReplay(t *testing.T) {
	oldTokenURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = oldTokenURL }()
	var tokenCalls atomic.Int32
	var received url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		received = url.Values{}
		for key, values := range r.Form {
			received[key] = append([]string(nil), values...)
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "oauth-access", "refresh_token": "oauth-refresh", "expires_in": 3600})
	}))
	defer tokenServer.Close()
	codexOAuthTokenURL = tokenServer.URL

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	startRec := httptest.NewRecorder()
	a.oauthStart(startRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/start", strings.NewReader(`{"platform":"codex"}`)), adminCtx{})
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startRec.Code, startRec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		AuthURL   string `json:"auth_url"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if authURL.Query().Get("state") != start.SessionID || authURL.Query().Get("code_challenge_method") != "S256" || authURL.Query().Get("code_challenge") == "" {
		t.Fatalf("invalid authorization URL: %s", start.AuthURL)
	}

	badPayload, _ := json.Marshal(map[string]any{"session_id": start.SessionID, "callback": codexOAuthRedirectURI + "?code=bad&state=wrong"})
	badRec := httptest.NewRecorder()
	a.oauthComplete(badRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/complete", strings.NewReader(string(badPayload))), adminCtx{})
	if badRec.Code != http.StatusBadRequest || tokenCalls.Load() != 0 {
		t.Fatalf("state mismatch status=%d calls=%d body=%s", badRec.Code, tokenCalls.Load(), badRec.Body.String())
	}

	// Start again because mismatched state intentionally consumes the one-time session.
	startRec = httptest.NewRecorder()
	a.oauthStart(startRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/start", strings.NewReader(`{"platform":"codex"}`)), adminCtx{})
	_ = json.Unmarshal(startRec.Body.Bytes(), &start)
	completePayload, _ := json.Marshal(map[string]any{"session_id": start.SessionID, "callback": codexOAuthRedirectURI + "?code=good&state=" + start.SessionID, "priority": 1})
	completeRec := httptest.NewRecorder()
	a.oauthComplete(completeRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/complete", strings.NewReader(string(completePayload))), adminCtx{})
	if completeRec.Code != http.StatusCreated {
		t.Fatalf("complete status=%d body=%s", completeRec.Code, completeRec.Body.String())
	}
	if tokenCalls.Load() != 1 || received.Get("code") != "good" || received.Get("code_verifier") == "" {
		t.Fatalf("token exchange calls=%d form=%v", tokenCalls.Load(), received)
	}
	replayRec := httptest.NewRecorder()
	a.oauthComplete(replayRec, httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/complete", strings.NewReader(string(completePayload))), adminCtx{})
	if replayRec.Code != http.StatusBadRequest || tokenCalls.Load() != 1 {
		t.Fatalf("replay status=%d calls=%d body=%s", replayRec.Code, tokenCalls.Load(), replayRec.Body.String())
	}
}

func TestParseClaudeCallbackCodeHashState(t *testing.T) {
	code, state, err := parseOAuthCallback("http://localhost:54545/callback?code=abc%23state-123")
	if err != nil || code != "abc" || state != "state-123" {
		t.Fatalf("code=%q state=%q err=%v", code, state, err)
	}
	code, state, err = parseOAuthCallback("abc#state-456")
	if err != nil || code != "abc" || state != "state-456" {
		t.Fatalf("plain code=%q state=%q err=%v", code, state, err)
	}
}

func TestOAuthErrorsDoNotLeakTokenResponse(t *testing.T) {
	oldTokenURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = oldTokenURL }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"access_token":"leak-me","refresh_token":"never-show"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	startRec := httptest.NewRecorder()
	a.oauthStart(startRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"platform":"codex"}`)), adminCtx{})
	var start struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(startRec.Body.Bytes(), &start)
	payload, _ := json.Marshal(map[string]any{"session_id": start.SessionID, "callback": "code#" + start.SessionID})
	rec := httptest.NewRecorder()
	a.oauthComplete(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(payload))), adminCtx{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "leak-me") || strings.Contains(rec.Body.String(), "never-show") {
		t.Fatalf("OAuth error leaked token response: %s", rec.Body.String())
	}
}

func TestConcurrentOAuthRefreshOnlyCallsEndpointOnce(t *testing.T) {
	oldTokenURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = oldTokenURL }()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "fresh-access", "refresh_token": "rotated-refresh", "expires_in": 3600})
	}))
	defer server.Close()
	codexOAuthTokenURL = server.URL

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	expired := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "cliproxy", AccessToken: "stale-access", RefreshToken: "old-refresh", AccountID: "acct", ExpiresAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}
	id, _, err := a.saveOAuthProvider(context.Background(), "refresh-test", 1, expired, 0, false)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyCredential := expired
			z := resolvedRoute{Provider: Provider{ID: id}, Credential: expired.AccessToken, AuthCredential: &copyCredential}
			if err := a.ensureFreshProviderCredential(context.Background(), &z); err != nil {
				errs <- err
				return
			}
			if z.Credential != "fresh-access" {
				errs <- fmt.Errorf("credential=%q", z.Credential)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d, want 1", calls.Load())
	}
	var encrypted []byte
	if err := a.db.QueryRow(`SELECT credential FROM providers WHERE id=?`, id).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plaintext, _ := a.decrypt(encrypted)
	if !strings.Contains(plaintext, "fresh-access") || !strings.Contains(plaintext, "rotated-refresh") || strings.Contains(plaintext, "stale-access") {
		t.Fatalf("stored refresh result=%s", plaintext)
	}
}

func TestOAuthProviderHeadersAndCodexPath(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		var path, auth, account string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, auth, account = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")
			writeJSON(w, http.StatusOK, map[string]any{"id": "resp", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		}))
		defer upstream.Close()
		a, err := New(testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		credential := ProviderCredential{Kind: "oauth", Platform: "codex", AccessToken: "codex-access", AccountID: "acct-id"}
		z := resolvedRoute{Route: Route{PublicName: "gpt-test", UpstreamModel: "gpt-test"}, Provider: Provider{Type: "codex_oauth", BaseURL: upstream.URL, RequestTimeoutMS: 5000}, Credential: credential.AccessToken, AuthCredential: &credential}
		rec := httptest.NewRecorder()
		incoming := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
		result := a.proxyUpstream(rec, incoming, z, proxyOptions{Endpoint: "/v1/responses", RawBody: []byte(`{"model":"gpt-test","input":"hi"}`), UsageFormat: "openai", GatewayID: "g"})
		if result.Status != http.StatusOK || path != "/responses" || auth != "Bearer codex-access" || account != "acct-id" {
			t.Fatalf("result=%+v path=%q auth=%q account=%q body=%s", result, path, auth, account, rec.Body.String())
		}
	})

	t.Run("claude", func(t *testing.T) {
		var auth, beta, version, app string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, beta, version, app = r.Header.Get("Authorization"), r.Header.Get("Anthropic-Beta"), r.Header.Get("Anthropic-Version"), r.Header.Get("X-App")
			writeJSON(w, http.StatusOK, map[string]any{"content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		}))
		defer upstream.Close()
		a, err := New(testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		credential := ProviderCredential{Kind: "oauth", Platform: "claude", AccessToken: "claude-access"}
		z := resolvedRoute{Route: Route{PublicName: "claude-test", UpstreamModel: "claude-test"}, Provider: Provider{Type: "claude_oauth", BaseURL: upstream.URL, RequestTimeoutMS: 5000}, Credential: credential.AccessToken, AuthCredential: &credential}
		rec := httptest.NewRecorder()
		incoming := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[]}`))
		result := a.proxyUpstream(rec, incoming, z, proxyOptions{Endpoint: "/v1/messages", RawBody: []byte(`{"model":"claude-test","messages":[]}`), UsageFormat: "anthropic", GatewayID: "g"})
		if result.Status != http.StatusOK || auth != "Bearer claude-access" || !strings.Contains(beta, "oauth-2025-04-20") || version != "2023-06-01" || app != "cli" {
			t.Fatalf("result=%+v auth=%q beta=%q version=%q app=%q body=%s", result, auth, beta, version, app, rec.Body.String())
		}
	})

	t.Run("anthropic api key regression", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
		if err := setProviderAuth(req, resolvedRoute{Provider: Provider{Type: "anthropic"}, Credential: "api-key"}); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("x-api-key") != "api-key" || req.Header.Get("Authorization") != "" {
			t.Fatalf("headers=%v", req.Header)
		}
	})
}

func TestSub2APIWrappedExportSkipsNonOAuthAccounts(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	content := `{
	  "code": 0,
	  "data": {
	    "accounts": [
	      {
	        "name": "Codex OAuth",
	        "platform": "openai",
	        "type": "oauth",
	        "credentials": {
	          "access_token": "codex-access",
	          "refresh_token": "codex-refresh",
	          "expires_at": "2026-07-23T12:00:00Z",
	          "chatgpt_account_id": "acct-wrapped"
	        },
	        "expires_at": 1999999999
	      },
	      {
	        "name": "OpenAI API Key",
	        "platform": "openai",
	        "type": "apikey",
	        "credentials": {"token": "must-not-import"}
	      },
	      {
	        "name": "Unsupported Gemini",
	        "platform": "gemini",
	        "type": "oauth",
	        "credentials": {"access_token": "must-not-import-either"}
	      }
	    ]
	  }
	}`
	items, err := a.parseCredentialImports(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d, want only the supported OAuth account: %#v", len(items), items)
	}
	got := items[0].Credential
	if got.Platform != "codex" || got.Source != "sub2api" || got.AccessToken != "codex-access" || got.AccountID != "acct-wrapped" {
		t.Fatalf("wrapped sub2api credential=%#v", got)
	}
	if got.ExpiresAt != "2026-07-23T12:00:00Z" {
		t.Fatalf("token expiry=%q; nested credential expiry must win over account expiry", got.ExpiresAt)
	}
}

func TestInvalidImportSelectionDoesNotConsumePreview(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	payload, _ := json.Marshal(map[string]any{"content": `{"type":"claude","access_token":"access","refresh_token":"refresh","email":"owner@example.com"}`})
	previewRec := httptest.NewRecorder()
	a.authImportPreview(previewRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(payload))), adminCtx{})
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}

	invalid, _ := json.Marshal(map[string]any{"session_id": preview.SessionID, "selected": []int{999}})
	invalidRec := httptest.NewRecorder()
	a.authImportCommit(invalidRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(invalid))), adminCtx{})
	if invalidRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid selection status=%d body=%s", invalidRec.Code, invalidRec.Body.String())
	}

	valid, _ := json.Marshal(map[string]any{"session_id": preview.SessionID, "selected": []int{1}})
	validRec := httptest.NewRecorder()
	a.authImportCommit(validRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(valid))), adminCtx{})
	if validRec.Code != http.StatusOK {
		t.Fatalf("valid retry status=%d body=%s", validRec.Code, validRec.Body.String())
	}
}

type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func authJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestCLIProxyXAICredentialImport(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	content := `{"type":"xai","auth_kind":"oauth","access_token":"xai-access-secret","refresh_token":"xai-refresh-secret","id_token":"xai-id-secret","expired":"2026-07-24T12:00:00Z","last_refresh":"2026-07-23T12:00:00Z","email":"GROK@EXAMPLE.COM","sub":"xai-subject-123","token_endpoint":"https://auth.x.ai/oauth/token","base_url":"https://cli-chat-proxy.grok.com/v1"}`
	items, err := a.parseCredentialImports(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%d, want 1", len(items))
	}
	got := items[0].Credential
	if got.Platform != "grok" || got.Source != "cliproxy" || got.AccountID != "xai-subject-123" || got.Email != "grok@example.com" {
		t.Fatalf("xAI normalization=%#v", got)
	}
	if got.Extra["token_endpoint"] != "https://auth.x.ai/oauth/token" {
		t.Fatalf("token endpoint=%#v", got.Extra)
	}

	payload, _ := json.Marshal(map[string]any{"content": content})
	rec := httptest.NewRecorder()
	a.authImportPreview(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(payload))), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"xai-access-secret", "xai-refresh-secret", "xai-id-secret", "xai-subject-123", "GROK@EXAMPLE.COM"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("preview leaked %q: %s", secret, rec.Body.String())
		}
	}
}

func TestXAIDeviceAuthorizationPendingThenCreatesProvider(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	var polls atomic.Int32
	a.client = &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case xaiOIDCDiscoveryURL:
			return authJSONResponse(http.StatusOK, `{"device_authorization_endpoint":"https://auth.x.ai/oauth/device/code","token_endpoint":"https://auth.x.ai/oauth/token"}`), nil
		case "https://auth.x.ai/oauth/device/code":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("client_id") != xaiOAuthClientID || r.Form.Get("scope") != xaiOAuthScope {
				t.Fatalf("device form=%v", r.Form)
			}
			return authJSONResponse(http.StatusOK, `{"device_code":"device-super-secret","user_code":"ABCD-EFGH","verification_uri":"https://auth.x.ai/device","verification_uri_complete":"https://auth.x.ai/device?user_code=ABCD-EFGH","expires_in":900,"interval":2}`), nil
		case "https://auth.x.ai/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("device_code") != "device-super-secret" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Fatalf("poll form=%v", r.Form)
			}
			if polls.Add(1) == 1 {
				return authJSONResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
			}
			jwt := unsignedJWT(map[string]any{"sub": "grok-user-1", "email": "grok@example.com", "exp": time.Now().Add(time.Hour).Unix()})
			return authJSONResponse(http.StatusOK, fmt.Sprintf(`{"access_token":"access-super-secret","refresh_token":"refresh-super-secret","id_token":%q,"expires_in":3600}`, jwt)), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, nil
		}
	})}

	startBody, _ := json.Marshal(map[string]any{"platform": "grok"})
	startRec := httptest.NewRecorder()
	a.oauthStart(startRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(startBody))), adminCtx{})
	if startRec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startRec.Code, startRec.Body.String())
	}
	if strings.Contains(startRec.Body.String(), "device-super-secret") {
		t.Fatalf("start response leaked device code: %s", startRec.Body.String())
	}
	var started struct {
		SessionID string `json:"session_id"`
		Flow      string `json:"flow"`
		UserCode  string `json:"user_code"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Flow != "device" || started.UserCode != "ABCD-EFGH" || started.SessionID == "" {
		t.Fatalf("start=%#v", started)
	}

	completeBody, _ := json.Marshal(map[string]any{"session_id": started.SessionID, "priority": 1})
	pendingRec := httptest.NewRecorder()
	a.oauthComplete(pendingRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(completeBody))), adminCtx{})
	if pendingRec.Code != http.StatusAccepted || !strings.Contains(pendingRec.Body.String(), `"pending":true`) {
		t.Fatalf("pending status=%d body=%s", pendingRec.Code, pendingRec.Body.String())
	}
	if _, ok := a.oauthSessions[started.SessionID]; !ok {
		t.Fatal("pending device session was consumed")
	}

	a.authMu.Lock()
	session := a.oauthSessions[started.SessionID]
	session.LastPoll = time.Time{}
	a.oauthSessions[started.SessionID] = session
	a.authMu.Unlock()
	completeRec := httptest.NewRecorder()
	a.oauthComplete(completeRec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(completeBody))), adminCtx{})
	if completeRec.Code != http.StatusCreated {
		t.Fatalf("complete status=%d body=%s", completeRec.Code, completeRec.Body.String())
	}
	for _, secret := range []string{"access-super-secret", "refresh-super-secret", "device-super-secret"} {
		if strings.Contains(completeRec.Body.String(), secret) {
			t.Fatalf("complete response leaked %q: %s", secret, completeRec.Body.String())
		}
	}
	var providerType, baseURL, authKind string
	if err := a.db.QueryRow(`SELECT type,base_url,auth_kind FROM providers`).Scan(&providerType, &baseURL, &authKind); err != nil {
		t.Fatal(err)
	}
	if providerType != "grok_oauth" || baseURL != "https://cli-chat-proxy.grok.com" || authKind != "oauth" {
		t.Fatalf("provider type=%q base=%q kind=%q", providerType, baseURL, authKind)
	}
}

func TestXAIRefreshUsesTrustedStoredTokenEndpoint(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.client = &http.Client{Transport: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://auth.x.ai/oauth/token" {
			t.Fatalf("refresh URL=%s", r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != xaiOAuthClientID || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-secret" {
			t.Fatalf("refresh form=%v", r.Form)
		}
		return authJSONResponse(http.StatusOK, `{"access_token":"new-access","expires_in":3600}`), nil
	})}
	current := ProviderCredential{Version: 1, Kind: "oauth", Platform: "grok", Source: "cliproxy", AccessToken: "old-access", RefreshToken: "refresh-secret", AccountID: "subject", Extra: map[string]any{"token_endpoint": "https://auth.x.ai/oauth/token"}}
	fresh, err := a.refreshOAuthCredential(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AccessToken != "new-access" || fresh.RefreshToken != "refresh-secret" || fresh.AccountID != "subject" || fresh.Extra["token_endpoint"] != "https://auth.x.ai/oauth/token" {
		t.Fatalf("fresh=%#v", fresh)
	}
}

func TestTrustedXAIEndpointRejectsDiscoveryInjection(t *testing.T) {
	for _, raw := range []string{"http://auth.x.ai/token", "https://x.ai.evil.example/token", "https://evil.example/token", "https://user@auth.x.ai/token"} {
		if isTrustedXAIEndpoint(raw) {
			t.Fatalf("trusted malicious endpoint %q", raw)
		}
	}
	for _, raw := range []string{"https://x.ai/token", "https://auth.x.ai/token"} {
		if !isTrustedXAIEndpoint(raw) {
			t.Fatalf("rejected trusted endpoint %q", raw)
		}
	}
}

func TestGrokOAuthProxyUsesBearerAndSingleV1Prefix(t *testing.T) {
	z := resolvedRoute{Provider: Provider{Type: "grok_oauth", BaseURL: oauthProviderBaseURL("grok")}, Credential: "grok-secret"}
	req := httptest.NewRequest(http.MethodPost, "https://gateway.example/v1/responses", nil)
	upstream, err := joinURLQuery(z.Provider.BaseURL, "/v1/responses", "")
	if err != nil {
		t.Fatal(err)
	}
	if upstream != "https://cli-chat-proxy.grok.com/v1/responses" {
		t.Fatalf("upstream=%q", upstream)
	}
	req.URL, _ = url.Parse(upstream)
	if err := setProviderAuth(req, z); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer grok-secret" {
		t.Fatalf("Authorization=%q", got)
	}
}
