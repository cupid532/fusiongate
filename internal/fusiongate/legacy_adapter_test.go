package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Legacy adapter functions remain for compatibility helpers. These tests call
// them directly; App.Router uses passthroughInference for every public endpoint.
func legacyAdapterRouter(a *App) http.Handler {
	mux := http.NewServeMux()
	handlers := map[string]func(http.ResponseWriter, *http.Request, authKey){"/v1/chat/completions": a.chat, "/v1/responses": a.responses, "/v1/responses/compact": a.responsesCompact, "/v1/messages": a.messages, "/v1/messages/count_tokens": a.messageTokenCount, "/v1/images/generations": a.images, "/v1/audio/speech": a.audioSpeech, "/v1/audio/transcriptions": a.audioTranscriptions, "/v1/embeddings": a.embeddings}
	for path, handler := range handlers {
		mux.HandleFunc(path, a.api(handler))
	}
	mux.Handle("/", a.Router())
	return a.security(mux)
}

func legacyAdapterRequest(t *testing.T, a *App, path, key, body, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	handlers := map[string]func(http.ResponseWriter, *http.Request, authKey){
		"/v1/chat/completions": a.chat, "/v1/responses": a.responses, "/v1/responses/compact": a.responsesCompact,
		"/v1/messages": a.messages, "/v1/messages/count_tokens": a.messageTokenCount, "/v1/images/generations": a.images,
		"/v1/audio/speech": a.audioSpeech, "/v1/audio/transcriptions": a.audioTranscriptions, "/v1/embeddings": a.embeddings,
	}
	handler, ok := handlers[path]
	if !ok {
		t.Fatalf("unknown legacy test endpoint %s", path)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	rec := httptest.NewRecorder()
	a.security(a.api(handler)).ServeHTTP(rec, req)
	return rec
}
