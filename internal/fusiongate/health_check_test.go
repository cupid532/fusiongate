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
	if result.Status != "healthy" || result.Error != "" || result.Mode != healthCheckModeGeneration || result.Model != "gpt-route-model" || result.FirstByteMS < 0 {
		t.Fatalf("result=%+v", result)
	}
	if path != "/responses" || accept != "text/event-stream" || account != "health-account" || model != "gpt-route-model" {
		t.Fatalf("path=%q accept=%q account=%q model=%q", path, accept, account, model)
	}
	if _, ok := input.([]any); !ok || store != false || stream != true {
		t.Fatalf("input=%#v store=%#v stream=%#v", input, store, stream)
	}
}

func TestConnectivityHealthProbeUsesModelListWithoutGeneration(t *testing.T) {
	var modelCalls, generationCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelCalls++
			if got := r.URL.Query().Get("client_version"); got != defaultCodexCLIVersion {
				t.Errorf("client_version=%q, want %q", got, defaultCodexCLIVersion)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer connectivity-access" {
				t.Errorf("authorization=%q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{"models": []any{map[string]any{"slug": "GPT-5.4", "display_name": "GPT-5.4"}}})
		case "/responses":
			generationCalls++
			http.Error(w, "generation must not run", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "connectivity-access", AccountID: "connectivity-account",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "codex-connectivity", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET base_url=? WHERE id=?`, upstream.URL, providerID); err != nil {
		t.Fatal(err)
	}

	h := NewHealthChecker(a, time.Minute, 1)
	result := h.probeProviderMode(context.Background(), providerID, healthCheckModeConnectivity)
	if result.Status != "reachable" || result.Error != "" || result.Mode != healthCheckModeConnectivity || result.ModelCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	if modelCalls != 1 || generationCalls != 0 {
		t.Fatalf("model calls=%d generation calls=%d", modelCalls, generationCalls)
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
func TestGenerationHealthProbeDoesNotTrustModelList(t *testing.T) {
	var modelCalls, generationCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelCalls++
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "probe-model"}}})
		case "/v1/chat/completions":
			generationCalls++
			http.Error(w, "generation unavailable", http.StatusGatewayTimeout)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "probe-provider", "openai_compatible", upstream.URL, "probe-secret", 1, 1, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "probe-model", "probe-model", "chat,stream", 1)
	h := NewHealthChecker(a, time.Minute, 1)

	connectivity := h.probeProviderMode(context.Background(), providerID, healthCheckModeConnectivity)
	if connectivity.Status != "reachable" {
		t.Fatalf("connectivity result=%+v", connectivity)
	}
	generation := h.probeProvider(context.Background(), providerID)
	if generation.Status == "healthy" || generation.Error == "" {
		t.Fatalf("generation result=%+v", generation)
	}
	if modelCalls != 1 || generationCalls != 1 {
		t.Fatalf("model calls=%d generation calls=%d", modelCalls, generationCalls)
	}
}

func TestConnectivityHealthProbeDoesNotRecoverCircuit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "probe-model"}}})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "circuit-connectivity", "openai_compatible", upstream.URL, "probe-secret", 1, 1, "normalized", "any", 0, 3, 30)
	openUntil := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE providers SET status='circuit_open',consecutive_failures=3,circuit_open_until=? WHERE id=?`, openUntil, providerID); err != nil {
		t.Fatal(err)
	}

	h := NewHealthChecker(a, time.Minute, 1)
	h.minDelay = 0
	h.maxDelay = 0
	h.checkProvider(context.Background(), providerID)

	var status, circuitOpenUntil string
	if err := a.db.QueryRow(`SELECT status,COALESCE(circuit_open_until,'') FROM providers WHERE id=?`, providerID).Scan(&status, &circuitOpenUntil); err != nil {
		t.Fatal(err)
	}
	if status != "circuit_open" || circuitOpenUntil != openUntil {
		t.Fatalf("connectivity probe changed business circuit: status=%q circuit_open_until=%q", status, circuitOpenUntil)
	}
}
