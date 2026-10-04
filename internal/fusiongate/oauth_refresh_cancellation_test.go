package fusiongate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOAuthRefreshWaitingHonorsCanceledContext(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.refreshSlots <- struct{}{}
	defer func() { <-a.refreshSlots }()
	credential := ProviderCredential{Platform: "codex", Source: "fusiongate_oauth", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = a.ensureFreshProviderCredential(ctx, &resolvedRoute{AuthCredential: &credential})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(start))
	}
}

func TestOAuthCanceledRefreshDoesNotInvalidateCredential(t *testing.T) {
	oldURL := codexOAuthTokenURL
	defer func() { codexOAuthTokenURL = oldURL }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	codexOAuthTokenURL = server.URL
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", AccountID: "fixture", ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	id, _, err := a.saveOAuthProvider(context.Background(), "cancel-test", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET auth_status='ready',status='unknown',last_error='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	z := resolvedRoute{Provider: Provider{ID: id}, AuthCredential: &credential, Credential: credential.AccessToken}
	if err = a.ensureFreshProviderCredential(ctx, &z); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	var auth, status, lastError string
	if err = a.db.QueryRow(`SELECT auth_status,status,last_error FROM providers WHERE id=?`, id).Scan(&auth, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if auth != "ready" || status != "unknown" || lastError != "" {
		t.Fatalf("auth=%q status=%q error=%q", auth, status, lastError)
	}
}
