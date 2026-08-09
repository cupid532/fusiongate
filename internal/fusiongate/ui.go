package fusiongate

import (
	"bytes"
	"embed"
	"net/http"
	"strings"
)

//go:embed ui
var uiFiles embed.FS

// loadUIAsset reads one embedded console asset and resolves the version placeholder,
// so the displayed version always comes from fusiongate.Version and is never
// hard-coded into an asset.
func loadUIAsset(name string) []byte {
	data, err := uiFiles.ReadFile("ui/" + name)
	if err != nil {
		panic("embedded UI asset missing: " + name)
	}
	return bytes.ReplaceAll(data, []byte("{{FUSIONGATE_VERSION}}"), []byte(Version))
}

var (
	adminHTML = loadUIAsset("index.html")
	adminCSS  = loadUIAsset("app.css")
	adminJS   = loadUIAsset("app.js")
)

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(adminHTML)
}

// uiAsset serves the console stylesheet and script. The shell requests both with the
// running version in the query string, so a matching request can be cached hard while
// an upgrade changes the URL and busts that cache. Only the two known names are
// served, so the path cannot be pointed at anything else.
func (a *App) uiAsset(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var contentType string
	switch strings.TrimPrefix(r.URL.Path, "/ui/") {
	case "app.css":
		body, contentType = adminCSS, "text/css; charset=utf-8"
	case "app.js":
		body, contentType = adminJS, "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("v") == Version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
