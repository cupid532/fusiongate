package fusiongate

import (
	_ "embed"
	"net/http"
)

//go:embed ui/index.html
var adminHTML []byte

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(adminHTML)
}
