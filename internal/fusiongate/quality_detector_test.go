package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type qualityDetectorCapture struct {
	mu        sync.Mutex
	startBody map[string]any
	requests  []string
}

func newQualityDetectorFixture(t *testing.T) (*httptest.Server, *qualityDetectorCapture) {
	t.Helper()
	capture := &qualityDetectorCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.mu.Lock()
		capture.requests = append(capture.requests, r.Method+" "+r.URL.Path)
		capture.mu.Unlock()
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-CSRF-Token") != "" {
			t.Errorf("sensitive inbound headers reached sidecar: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/bootstrap":
			writeJSON(w, http.StatusOK, map[string]any{
				"session_token": "sidecar-session",
				"single_presets": map[string]any{
					"low":    map[string]any{"preset": "low", "official": true},
					"medium": map[string]any{"preset": "medium", "official": true},
					"high":   map[string]any{"preset": "high", "official": true},
				},
			})
		case "/api/detector/estimate":
			if r.Header.Get("X-GPT56-Session") != "sidecar-session" {
				t.Errorf("estimate token=%q", r.Header.Get("X-GPT56-Session"))
			}
			writeJSON(w, http.StatusOK, map[string]any{"total_requests": 14})
		case "/api/detector/start":
			if r.Header.Get("X-GPT56-Session") != "sidecar-session" {
				t.Errorf("start token=%q", r.Header.Get("X-GPT56-Session"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			capture.mu.Lock()
			capture.startBody = body
			capture.mu.Unlock()
			if body["api_key"] == "fg_error_secret_value" {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed with fg_error_secret_value"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "running", "session_id": "run-1"})
		case "/api/detector/status":
			writeJSON(w, http.StatusOK, map[string]any{"status": "running", "session_id": "run-1"})
		case "/api/detector/report":
			writeJSON(w, http.StatusOK, map[string]any{"overall_verdict": "通过"})
		case "/api/detector/stop":
			if r.Header.Get("X-GPT56-Session") != "sidecar-session" {
				t.Errorf("stop token=%q", r.Header.Get("X-GPT56-Session"))
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "stopping"})
		default:
			http.NotFound(w, r)
		}
	}))
	return server, capture
}

func TestQualityDetectorUsesFrozenPresetAndInternalTarget(t *testing.T) {
	sidecar, capture := newQualityDetectorFixture(t)
	defer sidecar.Close()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = sidecar.URL
	cfg.QualityDetectorBaseURL = "http://127.0.0.1:8787/v1"
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	metadata := httptest.NewRecorder()
	a.qualityDetector(metadata, httptest.NewRequest(http.MethodGet, "/api/admin/quality-detector", nil), adminCtx{})
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"version":"4.0.1"`) || !strings.Contains(metadata.Body.String(), `"total_requests":14`) {
		t.Fatalf("metadata status=%d body=%s", metadata.Code, metadata.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(`{"preset":"low","model":"gpt-5.6-sol","api_key":"fg_test_secret"}`))
	request.Header.Set("Cookie", "fg_admin=must-not-leak")
	request.Header.Set("Authorization", "Bearer must-not-leak")
	request.Header.Set("X-CSRF-Token", "must-not-leak")
	start := httptest.NewRecorder()
	a.qualityDetector(start, request, adminCtx{})
	if start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	capture.mu.Lock()
	body := capture.startBody
	capture.mu.Unlock()
	if body["base_url"] != "http://127.0.0.1:8787/v1" || body["model"] != "gpt-5.6-sol" || body["api_key"] != "fg_test_secret" || body["retention_enabled"] != false {
		t.Fatalf("unexpected start body=%#v", body)
	}
	config, _ := body["config"].(map[string]any)
	if config["preset"] != "low" || config["official"] != true {
		t.Fatalf("start did not use frozen sidecar preset: %#v", config)
	}
}

func TestQualityDetectorRejectsUnsupportedInputsAndRequiresAdmin(t *testing.T) {
	sidecar, _ := newQualityDetectorFixture(t)
	defer sidecar.Close()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = sidecar.URL
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, body := range []string{
		`{"preset":"custom","model":"gpt-5.6-sol","api_key":"fg_key"}`,
		`{"preset":"low","model":"other-model","api_key":"fg_key"}`,
		`{"preset":"low","model":"gpt-5.6-sol","api_key":""}`,
	} {
		recorder := httptest.NewRecorder()
		a.qualityDetector(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(body)), adminCtx{})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}

	unauthorized := httptest.NewRecorder()
	a.Router().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/admin/quality-detector", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
}

func TestQualityDetectorConfigurationRequiresLoopback(t *testing.T) {
	for _, rawURL := range []string{"https://127.0.0.1:18789", "http://detector:18789", "http://example.com", "http://user:pass@127.0.0.1:18789"} {
		if _, err := newQualityDetectorClient(rawURL); err == nil {
			t.Fatalf("unsafe detector URL was accepted: %s", rawURL)
		}
	}
	for _, rawURL := range []string{"http://127.0.0.1:18789", "http://localhost:18789", "http://[::1]:18789"} {
		if _, err := newQualityDetectorClient(rawURL); err != nil {
			t.Fatalf("loopback detector URL %s was rejected: %v", rawURL, err)
		}
	}
}

func TestQualityDetectorRedactsSidecarErrors(t *testing.T) {
	sidecar, _ := newQualityDetectorFixture(t)
	defer sidecar.Close()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = sidecar.URL
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recorder := httptest.NewRecorder()
	a.qualityDetector(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(`{"preset":"low","model":"gpt-5.6-sol","api_key":"fg_error_secret_value"}`)), adminCtx{})
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "fg_error_secret_value") || !strings.Contains(recorder.Body.String(), "redacted-key") {
		t.Fatalf("redaction status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
