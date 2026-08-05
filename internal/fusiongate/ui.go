package fusiongate

import (
	"bytes"
	_ "embed"
	"net/http"
)

//go:embed ui/index.html
var adminTemplate []byte

var adminHTML = bytes.ReplaceAll(adminTemplate, []byte("{{FUSIONGATE_VERSION}}"), []byte(Version))

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(adminHTML)
}
