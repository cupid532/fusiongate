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
	failStart bool
	status    string
}

func newQualityDetectorFixture(t *testing.T) (*httptest.Server, *qualityDetectorCapture) {
	t.Helper()
	capture := &qualityDetectorCapture{status: "idle"}
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
			failStart := capture.failStart
			capture.mu.Unlock()
			if failStart {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed with fg_quality_secret_value"})
				return
			}
			capture.mu.Lock()
			capture.status = "running"
			capture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"status": "running", "session_id": "run-1"})
		case "/api/detector/status":
			capture.mu.Lock()
			status := capture.status
			capture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"status": status, "session_id": "run-1"})
		case "/api/detector/report":
			writeJSON(w, http.StatusOK, map[string]any{"overall_verdict": "通过"})
		case "/api/detector/stop":
			if r.Header.Get("X-GPT56-Session") != "sidecar-session" {
				t.Errorf("stop token=%q", r.Header.Get("X-GPT56-Session"))
			}
			capture.mu.Lock()
			capture.status = "stopping"
			capture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"status": "stopping"})
		default:
			http.NotFound(w, r)
		}
	}))
	return server, capture
}

func insertQualityDetectorTarget(t *testing.T, a *App, providerName, secret string) qualityDetectorTarget {
	t.Helper()
	providerID := insertTestProvider(t, a, providerName, "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	routeID := insertTestRoute(t, a, providerID, "gpt-5.6-sol", "upstream-sol", "chat", 0)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	keyID := insertProviderKeyForTest(t, a, providerID, secret, "检测 Key", "upstream-sol", providerKeyEgressInherit, nil, 1, 0)
	return qualityDetectorTarget{ID: qualityDetectorTargetID(routeID, keyID), Model: "gpt-5.6-sol", RouteID: routeID, ProviderID: providerID, ProviderKeyID: keyID}
}

func TestQualityDetectorUsesFrozenPresetAndTargetedCredential(t *testing.T) {
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
	target := insertQualityDetectorTarget(t, a, "Sol 渠道", "sk-upstream-must-not-leak")

	metadata := httptest.NewRecorder()
	a.qualityDetector(metadata, httptest.NewRequest(http.MethodGet, "/api/admin/quality-detector", nil), adminCtx{})
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"version":"4.0.1"`) || !strings.Contains(metadata.Body.String(), `"total_requests":14`) || !strings.Contains(metadata.Body.String(), `"provider_name":"Sol 渠道"`) || !strings.Contains(metadata.Body.String(), `"provider_key_hint":"sk-u...leak"`) {
		t.Fatalf("metadata status=%d body=%s", metadata.Code, metadata.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(`{"preset":"low","target_id":"`+target.ID+`"}`))
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
	routeToken, _ := body["api_key"].(string)
	if body["base_url"] != "http://127.0.0.1:8787/v1" || body["model"] != "gpt-5.6-sol" || !strings.HasPrefix(routeToken, "fg_quality_") || strings.Contains(routeToken, "upstream") || body["retention_enabled"] != false {
		t.Fatalf("unexpected start body=%#v", body)
	}
	config, _ := body["config"].(map[string]any)
	if config["preset"] != "low" || config["official"] != true {
		t.Fatalf("start did not use frozen sidecar preset: %#v", config)
	}
	loopback := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	loopback.RemoteAddr = "127.0.0.1:41000"
	loopback.Header.Set("Authorization", "Bearer "+routeToken)
	key, ok := a.authenticateKey(loopback)
	if !ok || key.QualityRoute == nil || key.QualityRoute.Target.ProviderKeyID != target.ProviderKeyID {
		t.Fatalf("targeted detector token was not authenticated: key=%#v ok=%v", key, ok)
	}
	public := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	public.RemoteAddr = "203.0.113.10:41000"
	public.Header.Set("Authorization", "Bearer "+routeToken)
	if _, ok := a.authenticateKey(public); ok {
		t.Fatal("quality detector token was accepted outside loopback")
	}
	forwarded := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	forwarded.RemoteAddr = "127.0.0.1:41000"
	forwarded.Header.Set("Authorization", "Bearer "+routeToken)
	forwarded.Header.Set("X-Forwarded-For", "203.0.113.10")
	if _, ok := a.authenticateKey(forwarded); ok {
		t.Fatal("quality detector token was accepted through a reverse proxy")
	}
}

func TestQualityDetectorTokenOnlyAcceptsLoopbackResponsesPost(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	token, _ := a.registerQualityDetectorRoute(qualityDetectorTarget{Model: "gpt-5.6-sol"})

	for _, test := range []struct {
		name       string
		method     string
		target     string
		remoteAddr string
		header     string
	}{
		{name: "wrong method", method: http.MethodGet, target: "/v1/responses", remoteAddr: "127.0.0.1:41000"},
		{name: "wrong path", method: http.MethodPost, target: "/v1/chat/completions", remoteAddr: "127.0.0.1:41000"},
		{name: "path suffix", method: http.MethodPost, target: "/v1/responses/", remoteAddr: "127.0.0.1:41000"},
		{name: "query string", method: http.MethodPost, target: "/v1/responses?debug=1", remoteAddr: "127.0.0.1:41000"},
		{name: "public peer", method: http.MethodPost, target: "/v1/responses", remoteAddr: "203.0.113.10:41000"},
		{name: "forwarded peer", method: http.MethodPost, target: "/v1/responses", remoteAddr: "127.0.0.1:41000", header: "Forwarded"},
		{name: "real IP peer", method: http.MethodPost, target: "/v1/responses", remoteAddr: "127.0.0.1:41000", header: "X-Real-IP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("Authorization", "Bearer "+token)
			if test.header != "" {
				request.Header.Set(test.header, "203.0.113.10")
			}
			if _, ok := a.authenticateKey(request); ok {
				t.Fatal("quality detector token was accepted outside its exact endpoint")
			}
		})
	}

	valid := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	valid.RemoteAddr = "127.0.0.1:41000"
	valid.Header.Set("Authorization", "Bearer "+token)
	if key, ok := a.authenticateKey(valid); !ok || key.QualityRoute == nil {
		t.Fatalf("valid quality detector request was rejected: key=%#v ok=%v", key, ok)
	}
}

func TestQualityDetectorRejectsConcurrentStartAndRevokesTokenOnStop(t *testing.T) {
	sidecar, capture := newQualityDetectorFixture(t)
	defer sidecar.Close()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = sidecar.URL
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	target := insertQualityDetectorTarget(t, a, "Sol 渠道", "sk-sol")
	startBody := `{"preset":"low","target_id":"` + target.ID + `"}`

	first := httptest.NewRecorder()
	a.qualityDetector(first, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(startBody)), adminCtx{})
	if first.Code != http.StatusOK {
		t.Fatalf("first start status=%d body=%s", first.Code, first.Body.String())
	}
	capture.mu.Lock()
	routeToken, _ := capture.startBody["api_key"].(string)
	capture.mu.Unlock()

	second := httptest.NewRecorder()
	a.qualityDetector(second, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(startBody)), adminCtx{})
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "quality_detector_busy") {
		t.Fatalf("second start status=%d body=%s", second.Code, second.Body.String())
	}
	capture.mu.Lock()
	startCalls := 0
	for _, request := range capture.requests {
		if request == "POST /api/detector/start" {
			startCalls++
		}
	}
	capture.mu.Unlock()
	if startCalls != 1 {
		t.Fatalf("sidecar start calls=%d", startCalls)
	}

	stop := httptest.NewRecorder()
	a.qualityDetector(stop, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/stop", strings.NewReader(`{}`)), adminCtx{})
	if stop.Code != http.StatusOK {
		t.Fatalf("stop status=%d body=%s", stop.Code, stop.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.RemoteAddr = "127.0.0.1:41000"
	request.Header.Set("Authorization", "Bearer "+routeToken)
	if _, ok := a.authenticateKey(request); ok {
		t.Fatal("quality detector token remained usable after stop")
	}
}

func TestQualityDetectorRouteIsExactAndDoesNotFailOver(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := insertQualityDetectorTarget(t, a, "First", "sk-first")
	second := insertQualityDetectorTarget(t, a, "Second", "sk-second")
	routes, err := a.resolve(t.Context(), "gpt-5.6-sol", "chat")
	if err != nil {
		t.Fatal(err)
	}
	filtered := restrictQualityDetectorRoutes(authKey{QualityRoute: &qualityDetectorRouteSession{Target: second}}, routes)
	if len(filtered) != 1 || filtered[0].Provider.ID != second.ProviderID || filtered[0].ProviderKeyID != second.ProviderKeyID || filtered[0].Provider.ID == first.ProviderID {
		t.Fatalf("targeted routes=%#v", filtered)
	}
}

func TestQualityDetectorTokenCallsOnlySelectedUpstream(t *testing.T) {
	firstCalls := 0
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "must not be called"})
	}))
	defer firstUpstream.Close()
	secondCalls := 0
	secondAuthorization := ""
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		secondAuthorization = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-selected",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "selected"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer secondUpstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	insertTarget := func(name, baseURL, secret string, priority int) qualityDetectorTarget {
		providerID := insertTestProvider(t, a, name, "openai_compatible", baseURL, "legacy", priority, 100, "normalized", "any", 0, 3, 30)
		routeID := insertTestRoute(t, a, providerID, "gpt-5.6-sol", "upstream-sol", "chat", priority)
		if _, updateErr := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); updateErr != nil {
			t.Fatal(updateErr)
		}
		keyID := insertProviderKeyForTest(t, a, providerID, secret, name+" Key", "upstream-sol", providerKeyEgressInherit, nil, 1, 0)
		return qualityDetectorTarget{ID: qualityDetectorTargetID(routeID, keyID), Model: "gpt-5.6-sol", RouteID: routeID, ProviderID: providerID, ProviderKeyID: keyID}
	}
	_ = insertTarget("First", firstUpstream.URL, "sk-first", 1)
	selected := insertTarget("Selected", secondUpstream.URL, "sk-selected", 2)
	token, _ := a.registerQualityDetectorRoute(selected)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello","stream":false}`))
	request.RemoteAddr = "127.0.0.1:42000"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	a.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if firstCalls != 0 || secondCalls != 1 || secondAuthorization != "Bearer sk-selected" {
		t.Fatalf("first_calls=%d second_calls=%d second_auth=%q", firstCalls, secondCalls, secondAuthorization)
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
	target := insertQualityDetectorTarget(t, a, "Sol 渠道", "sk-sol")

	for _, body := range []string{
		`{"preset":"custom","target_id":"` + target.ID + `"}`,
		`{"preset":"low","target_id":"999:999"}`,
		`{"preset":"low","target_id":""}`,
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
	sidecar, capture := newQualityDetectorFixture(t)
	defer sidecar.Close()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = sidecar.URL
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	target := insertQualityDetectorTarget(t, a, "Sol 渠道", "sk-sol")
	capture.failStart = true
	recorder := httptest.NewRecorder()
	a.qualityDetector(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/quality-detector/start", strings.NewReader(`{"preset":"low","target_id":"`+target.ID+`"}`)), adminCtx{})
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "fg_quality_secret_value") || !strings.Contains(recorder.Body.String(), "redacted-key") {
		t.Fatalf("redaction status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
