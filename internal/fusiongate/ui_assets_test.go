package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console is served as three embedded assets instead of one 285 KB file. The shell
// has to reference the other two with the running version so an upgrade cannot leave a
// browser mixing a new shell with a cached old script.
func TestConsoleShellReferencesVersionedAssets(t *testing.T) {
	shell := string(adminHTML)
	for _, required := range []string{
		`<link rel="stylesheet" href="/ui/app.css?v=` + Version + `">`,
		`<script src="/ui/app.js?v=` + Version + `"></script>`,
	} {
		if !strings.Contains(shell, required) {
			t.Fatalf("console shell is missing %q", required)
		}
	}
	if strings.Contains(shell, "<style>") {
		t.Fatal("the stylesheet is inline again instead of being served as an asset")
	}
	// The theme has to be applied before first paint, so that one script stays inline.
	if !strings.Contains(shell, "localStorage.getItem('fusiongate-theme')") {
		t.Fatal("the pre-paint theme script must stay inline to avoid a flash of the wrong theme")
	}
	if strings.Contains(string(adminCSS), "{{FUSIONGATE_VERSION}}") || strings.Contains(string(adminJS), "{{FUSIONGATE_VERSION}}") {
		t.Fatal("an asset contains an unresolved version placeholder")
	}
}

func TestConsoleAssetsAreServedWithCorrectTypeAndCaching(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	cases := []struct {
		path, query, contentType string
		body                     []byte
	}{
		{"/ui/app.css", "v=" + Version, "text/css; charset=utf-8", adminCSS},
		{"/ui/app.js", "v=" + Version, "text/javascript; charset=utf-8", adminJS},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path+"?"+tc.query, nil)
			rec := httptest.NewRecorder()
			a.uiAsset(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
				t.Fatalf("a version-matched asset should be cacheable, got Cache-Control %q", got)
			}
			if rec.Body.Len() != len(tc.body) {
				t.Fatalf("served %d bytes, embedded asset is %d", rec.Body.Len(), len(tc.body))
			}
		})
	}

	// A stale or missing version must not be cached, otherwise a browser could pin an
	// old asset across an upgrade.
	req := httptest.NewRequest(http.MethodGet, "/ui/app.js?v=V0.1", nil)
	rec := httptest.NewRecorder()
	a.uiAsset(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("mismatched version Cache-Control = %q, want no-cache", got)
	}

	// Only the two known assets exist; the path is not a file server.
	for _, path := range []string{"/ui/../app.go", "/ui/index.html", "/ui/", "/ui/app.css.map"} {
		rec := httptest.NewRecorder()
		a.uiAsset(rec, httptest.NewRequest(http.MethodGet, "/ui/"+strings.TrimPrefix(path, "/ui/"), nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", path, rec.Code)
		}
	}
}

func TestConsoleRoutesServeShellAndAssets(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	server := httptest.NewServer(a.Router())
	defer server.Close()

	for _, tc := range []struct{ path, contains string }{
		{"/", "/ui/app.js?v=" + Version},
		{"/ui/app.css?v=" + Version, "THEME TOKENS"},
		{"/ui/app.js?v=" + Version, "function renderIPPool()"},
	} {
		resp, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 0)
		buf := make([]byte, 32<<10)
		for {
			n, readErr := resp.Body.Read(buf)
			body = append(body, buf[:n]...)
			if readErr != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s -> %d", tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), tc.contains) {
			t.Fatalf("GET %s did not contain %q", tc.path, tc.contains)
		}
	}
}
