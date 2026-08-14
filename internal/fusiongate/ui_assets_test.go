package fusiongate

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console is a Vite/React build embedded as an FS tree. The shell is served at
// "/" with the version injected, and the hashed assets live under /ui/assets/* so an
// upgrade can never leave a browser mixing a new shell with a cached old bundle.
func TestConsoleShellIsServedWithVersion(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	a.ui(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	shell := rec.Body.String()
	if !strings.Contains(shell, "fusiongate-version") {
		t.Fatal("shell is missing the version meta tag")
	}
	if strings.Contains(shell, "{{FUSIONGATE_VERSION}}") {
		t.Fatal("shell contains an unresolved version placeholder")
	}
	if !strings.Contains(shell, "/ui/assets/") {
		t.Fatal("shell must reference the hashed build assets under /ui/assets/")
	}
}

func TestConsoleAssetsAreServedImmutably(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Discover the real hashed asset names from the embedded FS.
	var cssName, jsName string
	_ = fs.WalkDir(uiSub, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".css") {
			cssName = path
		}
		if strings.HasSuffix(path, ".js") {
			jsName = path
		}
		return nil
	})
	if cssName == "" || jsName == "" {
		t.Fatal("embedded build is missing hashed assets")
	}

	for _, tc := range []struct{ name, contentType string }{
		{cssName, "text/css; charset=utf-8"},
		{jsName, "text/javascript; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/"+tc.name, nil)
			rec := httptest.NewRecorder()
			a.uiAsset(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
				t.Fatalf("hashed asset should be cacheable, got Cache-Control %q", got)
			}
		})
	}
}

func TestConsoleAssetPathTraversalIsRejected(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, path := range []string{"/ui/../app.go", "/ui/assets/../../app.go", "/ui/", "/ui/not-real.js"} {
		rec := httptest.NewRecorder()
		a.uiAsset(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", path, rec.Code)
		}
	}
}
