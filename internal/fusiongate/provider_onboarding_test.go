package fusiongate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func setupPost(t *testing.T, a *App, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	if path == "/api/admin/providers" {
		a.providers(w, r, adminCtx{})
	} else if path == "/api/admin/providers/preview-models" {
		a.previewProviderModels(w, r, adminCtx{})
	} else {
		a.providerByID(w, r, adminCtx{})
	}
	return w
}
func newSetupApp(t *testing.T) *App {
	t.Helper()
	a, e := New(testConfig(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func setupCount(t *testing.T, a *App, table string, want int) {
	t.Helper()
	var got int
	if e := a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); e != nil || got != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, got, want, e)
	}
}

const setupBody = `{"name":"setup","type":"openai_compatible","baseURL":"https://upstream.example/v1","credential":"sk-draft-secret","request_id":"new-setup","setup":{"inventory":[{"model":"Vendor/Code-V2","capabilities":"chat,stream,tools","source":"discovered"},{"model":"NotSelected","source":"manual"}],"selected":["Vendor/Code-V2"]}}`

func TestProviderSetupAtomicAndIdempotent(t *testing.T) {
	a := newSetupApp(t)
	w := setupPost(t, a, "/api/admin/providers", setupBody)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var first map[string]any
	json.Unmarshal(w.Body.Bytes(), &first)
	for range 2 {
		w = setupPost(t, a, "/api/admin/providers", setupBody)
		if w.Code != 200 {
			t.Fatalf("retry %d %s", w.Code, w.Body.String())
		}
		var again map[string]any
		json.Unmarshal(w.Body.Bytes(), &again)
		if again["id"] != first["id"] || again["key_id"] != first["key_id"] {
			t.Fatal("retry changed identity")
		}
	}
	for table, want := range map[string]int{"providers": 1, "provider_api_keys": 1, "provider_api_key_models": 2, "model_routes": 1, "provider_setup_requests": 1} {
		setupCount(t, a, table, want)
	}
	var model, policy, allowlist, source string
	var enabled int
	if e := a.db.QueryRow(`SELECT upstream_model FROM model_routes`).Scan(&model); e != nil || model != "Vendor/Code-V2" {
		t.Fatalf("wire spelling %q %v", model, e)
	}
	a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys`).Scan(&policy, &allowlist)
	a.db.QueryRow(`SELECT enabled,model_source FROM provider_api_key_models WHERE model='NotSelected'`).Scan(&enabled, &source)
	if policy != "allowlist" || allowlist != "vendor/code-v2" || enabled != 0 || source != "manual" {
		t.Fatal("permission or source incorrect")
	}
	var digest string
	a.db.QueryRow(`SELECT digest FROM provider_setup_requests`).Scan(&digest)
	if strings.Contains(digest, "sk-draft") {
		t.Fatal("secret stored")
	}
	w = setupPost(t, a, "/api/admin/providers", strings.Replace(setupBody, `"setup"`, `"changed"`, 1))
	if w.Code != 409 {
		t.Fatalf("changed retry %d", w.Code)
	}
}
func TestProviderSetupRollbackOnRouteFailure(t *testing.T) {
	a := newSetupApp(t)
	if _, e := a.db.Exec(`CREATE TRIGGER setup_fail BEFORE INSERT ON model_routes BEGIN SELECT RAISE(ABORT,'injected failure'); END`); e != nil {
		t.Fatal(e)
	}
	w := setupPost(t, a, "/api/admin/providers", setupBody)
	if w.Code != 500 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"providers", "provider_api_keys", "provider_api_key_models", "model_routes", "provider_setup_requests"} {
		setupCount(t, a, table, 0)
	}
	a.db.Exec(`DROP TRIGGER setup_fail`)
	if w = setupPost(t, a, "/api/admin/providers", setupBody); w.Code != 201 {
		t.Fatalf("retry %d %s", w.Code, w.Body.String())
	}
}
func TestProviderSetupConcurrentRetry(t *testing.T) {
	a := newSetupApp(t)
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for range 6 {
		wg.Go(func() { codes <- setupPost(t, a, "/api/admin/providers", setupBody).Code })
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == 201 {
			created++
		} else if code != 200 {
			t.Fatalf("retry status %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("created %d", created)
	}
	setupCount(t, a, "providers", 1)
}
func TestProviderSetupEmptyAndInvalidSelections(t *testing.T) {
	for _, test := range []struct {
		name, setup string
		status      int
	}{
		{"empty", `{"inventory":[],"selected":[]}`, 201},
		{"unknown", `{"inventory":[{"model":"Known"}],"selected":["Unknown"]}`, 400},
		{"duplicate", `{"inventory":[{"model":"Known"},{"model":"KNOWN"}],"selected":[]}`, 400},
		{"invalid", `{"inventory":[{"model":"bad name"}],"selected":[]}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newSetupApp(t)
			body := fmt.Sprintf(`{"name":"setup","type":"openai_compatible","baseURL":"https://upstream.example","credential":"secret","request_id":"request","setup":%s}`, test.setup)
			w := setupPost(t, a, "/api/admin/providers", body)
			if w.Code != test.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			setupCount(t, a, "model_routes", 0)
			if test.status == 201 {
				var p, l string
				a.db.QueryRow(`SELECT model_policy,model_allowlist FROM provider_api_keys`).Scan(&p, &l)
				if providerKeySupportsModel(p, l, "", "", "any", nil, nil) {
					t.Fatal("empty setup grants permission")
				}
			} else {
				setupCount(t, a, "providers", 0)
			}
		})
	}
}
func TestProviderSetupPreviewDoesNotPersist(t *testing.T) {
	a := newSetupApp(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer preview-secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]string{{"id": "Vendor/Code-V2"}}})
	}))
	defer upstream.Close()
	body := fmt.Sprintf(`{"type":"openai_compatible","baseURL":%q,"credential":"preview-secret"}`, upstream.URL+"/v1")
	w := setupPost(t, a, "/api/admin/providers/preview-models", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Vendor/Code-V2") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"providers", "provider_api_keys", "provider_api_key_models", "model_routes", "provider_setup_requests"} {
		setupCount(t, a, table, 0)
	}
	a.cfg.AllowPrivateUpstreams = false
	if w = setupPost(t, a, "/api/admin/providers/preview-models", body); w.Code != 400 {
		t.Fatalf("unsafe upstream %d", w.Code)
	}
}
func TestAdditionalKeySetupPreservesExistingRoutes(t *testing.T) {
	a := newSetupApp(t)
	w := setupPost(t, a, "/api/admin/providers", setupBody)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	a.db.Exec(`UPDATE model_routes SET public_name='custom-alias',capabilities='chat',priority=9,enabled=0`)
	insertTestRoute(t, a, created.ID, "manual-route", "NotSelected", "chat,stream", 1)
	body := `{"api_key":"backup-secret","request_id":"backup-setup","setup":{"inventory":[{"model":"Vendor/Code-V2","capabilities":"chat,stream,tools"},{"model":"NotSelected"},{"model":"NewModel"}],"selected":["Vendor/Code-V2","NewModel"]}}`
	path := fmt.Sprintf("/api/admin/providers/%d/keys", created.ID)
	w = setupPost(t, a, path, body)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var first map[string]any
	json.Unmarshal(w.Body.Bytes(), &first)
	w = setupPost(t, a, path, body)
	var retry map[string]any
	json.Unmarshal(w.Body.Bytes(), &retry)
	if w.Code != 200 || retry["id"] != first["id"] {
		t.Fatalf("retry %d %s", w.Code, w.Body.String())
	}
	setupCount(t, a, "provider_api_keys", 2)
	setupCount(t, a, "model_routes", 3)
	var caps string
	var priority, enabled int
	a.db.QueryRow(`SELECT capabilities,priority,enabled FROM model_routes WHERE public_name='custom-alias'`).Scan(&caps, &priority, &enabled)
	if caps != "chat" || priority != 9 || enabled != 0 {
		t.Fatal("existing route modified")
	}
}
