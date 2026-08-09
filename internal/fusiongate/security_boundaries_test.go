package fusiongate

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSONRejectsTrailingAndOversizedBodies(t *testing.T) {
	var value struct {
		Name string `json:"name"`
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"one"} {"name":"two"}`))
	if err := readJSON(request, &value); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+strings.Repeat("x", 10<<20)+`"}`))
	if err := readJSON(request, &value); !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("oversized body error=%v", err)
	}
}

func TestAdminSessionCanBeRevoked(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	loginRequest := httptest.NewRequest(http.MethodPost, "/api/admin/login", nil)
	loginRecorder := httptest.NewRecorder()
	a.setAdminCookies(loginRecorder, loginRequest)
	cookies := loginRecorder.Result().Cookies()
	authRequest := httptest.NewRequest(http.MethodGet, "/api/admin/session", nil)
	for _, cookie := range cookies {
		authRequest.AddCookie(cookie)
	}
	if _, ok := a.adminAuth(authRequest); !ok {
		t.Fatal("new session was not accepted")
	}
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/admin/logout", nil)
	for _, cookie := range cookies {
		logoutRequest.AddCookie(cookie)
	}
	logoutRecorder := httptest.NewRecorder()
	a.logout(logoutRecorder, logoutRequest)
	if _, ok := a.adminAuth(authRequest); ok {
		t.Fatal("revoked session remained valid")
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for attempt := 0; attempt < 5; attempt++ {
		if !a.allowAdminLogin("203.0.113.7") {
			t.Fatalf("attempt %d unexpectedly rejected", attempt+1)
		}
	}
	if a.allowAdminLogin("203.0.113.7") {
		t.Fatal("sixth client attempt was accepted")
	}
	if !a.allowAdminLogin("203.0.113.8") {
		t.Fatal("separate client was incorrectly rejected")
	}
}

func TestReadinessDropsBeforeShutdown(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recorder := httptest.NewRecorder()
	a.readyHealth(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status=%d", recorder.Code)
	}
	a.BeginShutdown()
	recorder = httptest.NewRecorder()
	a.readyHealth(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown readiness status=%d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	a.live(recorder, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("liveness status=%d", recorder.Code)
	}
}

func TestSecurityHeadersConstrainConsoleResources(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recorder := httptest.NewRecorder()
	a.Router().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "connect-src 'self'", "form-action 'self'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP %q is missing %q", csp, directive)
		}
	}
	if recorder.Header().Get("X-Frame-Options") != "DENY" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers=%v", recorder.Header())
	}
}

func TestLoginClientIDIgnoresForgedForwardedPrefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/login", nil)
	r.RemoteAddr = "127.0.0.1:44321"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.8, 127.0.0.1")
	if got := loginClientID(r); got != "203.0.113.8" {
		t.Fatalf("login client ID=%q, want proxy-observed client", got)
	}
}

func TestBrokenProviderCredentialDoesNotLeakDetailsToAPI(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "private-provider-name", "codex_oauth", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "public-model", "internal-model", "chat,stream", 0)
	if _, err := a.db.Exec(`UPDATE providers SET credential=X'00' WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	key := insertTestKey(t, a, false)
	recorder := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"public-model","messages":[{"role":"user","content":"hello"}]}`, "")
	body := recorder.Body.String()
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
	for _, secret := range []string{"private-provider-name", "internal-model", "decrypt", "credential", "invalid encrypted"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(secret)) {
			t.Fatalf("API leaked operator detail %q in %s", secret, body)
		}
	}
}
