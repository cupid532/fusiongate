package fusiongate

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed ui
var uiFiles embed.FS

var uiSub, _ = fs.Sub(uiFiles, "ui")

// ui serves the single-page shell (index.html) and resolves the version
// placeholder so the displayed version always comes from fusiongate.Version.
func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(uiSub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data = bytes.ReplaceAll(data, []byte("{{FUSIONGATE_VERSION}}"), []byte(Version))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// uiAsset serves the hashed build assets (js/css/svg/…). The Vite build emits
// immutable content-hashed filenames under /ui/assets, so they can be cached
// hard and busted automatically on upgrade.
func (a *App) uiAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(uiSub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	contentType := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".css"):
		contentType = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		contentType = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		contentType = "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		contentType = "image/png"
	case strings.HasSuffix(name, ".ico"):
		contentType = "image/x-icon"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
