package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiNativeChatExtractsCandidatePartText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-upstream:generateContent" || r.URL.Query().Get("key") != "gemini-secret" {
			t.Errorf("request URL=%s", r.URL.String())
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
				map[string]any{"text": "Hello "},
				map[string]any{"text": "from Gemini"},
			}}}},
			"usageMetadata": map[string]any{"promptTokenCount": 2, "candidatesTokenCount": 3},
		})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "gemini-native", "gemini", upstream.URL, "gemini-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "gemini-public", "gemini-public", "chat", 1)

	recorder := gatewayRequest(t, a, "/v1/chat/completions", insertTestKey(t, a, false), `{"model":"gemini-public","messages":[{"role":"user","content":"ping"}]}`, "test/1")
	if recorder.Code != http.StatusNotFound || recorder.Body.Len() == 0 {
		t.Fatalf("specialized Gemini adapter should be excluded: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
