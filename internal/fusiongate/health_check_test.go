package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCodexHealthProbeUsesResponsesProtocolAndConfiguredModel(t *testing.T) {
	var path, accept, account, model string
	var input any
	var store, stream any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		accept = r.Header.Get("Accept")
		account = r.Header.Get("ChatGPT-Account-ID")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		model, _ = body["model"].(string)
		input, store, stream = body["input"], body["store"], body["stream"]
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSE("health-response")))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "health-access", AccountID: "health-account",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "codex-health", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET base_url=? WHERE id=?`, upstream.URL, providerID); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, providerID, "gpt-public", "gpt-route-model", "chat,stream", 1)

	h := NewHealthChecker(a, time.Minute, 1)
	result := h.probeProvider(context.Background(), providerID)
	if result.Status != "healthy" || result.Error != "" {
		t.Fatalf("result=%+v", result)
	}
	if path != "/responses" || accept != "text/event-stream" || account != "health-account" || model != "gpt-route-model" {
		t.Fatalf("path=%q accept=%q account=%q model=%q", path, accept, account, model)
	}
	if _, ok := input.([]any); !ok || store != false || stream != true {
		t.Fatalf("input=%#v store=%#v stream=%#v", input, store, stream)
	}
}

func TestCodexHealthProbeDoesNotTreatBadRequestAsHealthy(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	h := NewHealthChecker(a, time.Minute, 1)
	status, message := h.parseCodexProbeResponse(http.StatusBadRequest, []byte(`{"detail":"Store must be set to false"}`))
	if status != "config_error" || message == "" {
		t.Fatalf("status=%q message=%q", status, message)
	}
}
