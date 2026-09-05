package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A route whose upstream model is switched off on every key used to make the
// whole manual health check fail with "no enabled key/model combinations". The
// operator saw one red line and no hint which of their routes was the problem.
// Such a route is now reported as a skipped item with the reason attached.
func TestManualHealthCheckSkipsRouteNoKeyCanServe(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "ghost-route", "openai_compatible", "https://example.test", "legacy", 1, 1, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	routeID := insertTestRoute(t, a, providerID, "gpt-ghost", "gpt-ghost", "chat,stream", 1)
	keyID := insertProviderKeyForTest(t, a, providerID, "key-one", "Key One", "", "inherit", nil, 1, 0)
	if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at,enabled) VALUES(?,?,?,?,?,0)`, keyID, "gpt-ghost", "Ghost", "chat,stream", now()); err != nil {
		t.Fatal(err)
	}

	job, err := a.healthCheckJobs.StartModels(context.Background(), []int64{providerID}, nil, nil, "all")
	if err != nil {
		t.Fatalf("job should start and report the route as skipped, got error: %v", err)
	}
	if job.Total != 1 || job.Skipped != 1 || job.Completed != 1 {
		t.Fatalf("job=%+v", job)
	}
	if job.Results[0].Status != "skipped" || !strings.Contains(job.Results[0].Error, "switched off") || job.Results[0].RouteID != routeID {
		t.Fatalf("result=%+v", job.Results[0])
	}
	job = waitForHealthCheckJob(t, a, job.ID)
	if job.Status != "completed" || job.Healthy != 0 || job.Failed != 0 || job.Skipped != 1 {
		t.Fatalf("finished job=%+v", job)
	}
	// A skipped route was never probed, so its stored health must be untouched.
	var status string
	if err := a.db.QueryRow(`SELECT health_check_status FROM model_routes WHERE id=?`, routeID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("route health status=%q, want pending (not probed)", status)
	}
}

func TestManualHealthCheckProbesLiveRoutesAndSkipsDeadOnes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": healthProbeAnswer(r)}}},
		})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "mixed-routes", "openai_compatible", upstream.URL, "legacy", 1, 1, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	liveRoute := insertTestRoute(t, a, providerID, "live-model", "live-model", "chat,stream", 1)
	deadRoute := insertTestRoute(t, a, providerID, "dead-model", "dead-model", "chat,stream", 1)
	keyID := insertProviderKeyForTest(t, a, providerID, "key-one", "Key One", "", "inherit", nil, 1, 0)
	for model, enabled := range map[string]int{"live-model": 1, "dead-model": 0} {
		if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at,enabled) VALUES(?,?,?,?,?,?)`, keyID, model, model, "chat,stream", now(), enabled); err != nil {
			t.Fatal(err)
		}
	}

	job, err := a.healthCheckJobs.StartModels(context.Background(), []int64{providerID}, nil, nil, "all")
	if err != nil {
		t.Fatal(err)
	}
	if job.Total != 2 {
		t.Fatalf("total=%d want 2 (one probe + one skipped)", job.Total)
	}
	job = waitForHealthCheckJob(t, a, job.ID)
	if job.Status != "completed" || job.Healthy != 1 || job.Skipped != 1 || job.Failed != 0 {
		t.Fatalf("job=%+v", job)
	}
	byRoute := map[int64]healthCheckItemResult{}
	for _, r := range job.Results {
		byRoute[r.RouteID] = r
	}
	if byRoute[liveRoute].Status != "healthy" || byRoute[liveRoute].ProviderKeyID != keyID {
		t.Fatalf("live=%+v", byRoute[liveRoute])
	}
	if byRoute[deadRoute].Status != "skipped" || byRoute[deadRoute].ProviderKeyID != 0 {
		t.Fatalf("dead=%+v", byRoute[deadRoute])
	}
}

// The dialog must offer exactly what the job will do. The preview therefore
// uses the same support rules and reports, per route and per key, why
// something cannot be probed.
func TestHealthCheckTargetsPreviewExplainsUnprobeableRoutes(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "preview", "openai_compatible", "https://example.test", "legacy", 1, 1, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	offRoute := insertTestRoute(t, a, providerID, "model-off", "model-off", "chat,stream", 1)
	unlistedRoute := insertTestRoute(t, a, providerID, "model-unlisted", "model-unlisted", "chat,stream", 1)
	okRoute := insertTestRoute(t, a, providerID, "model-ok", "model-ok", "chat,stream", 1)
	imageRoute := insertTestRoute(t, a, providerID, "model-image", "model-image", "image", 1)
	activeKey := insertProviderKeyForTest(t, a, providerID, "key-active", "Active", "", "inherit", nil, 1, 0)
	disabledKey := insertProviderKeyForTest(t, a, providerID, "key-disabled", "Disabled", "", "inherit", nil, 0, 1)
	for _, row := range []struct {
		key     int64
		model   string
		enabled int
	}{{activeKey, "model-off", 0}, {activeKey, "model-ok", 1}, {disabledKey, "model-off", 1}} {
		if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at,enabled) VALUES(?,?,?,?,?,?)`, row.key, row.model, row.model, "chat,stream", now(), row.enabled); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/providers/"+jsonNumber(providerID)+"/health-check-targets", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var preview healthCheckPreview
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ProviderID != providerID || preview.AuthKind != "api_key" || !preview.Enabled || !preview.HealthCheckEnabled {
		t.Fatalf("preview header=%+v", preview)
	}
	if preview.Probeable != 1 || len(preview.Routes) != 4 {
		t.Fatalf("probeable=%d routes=%d preview=%+v", preview.Probeable, len(preview.Routes), preview)
	}
	byRoute := map[int64]healthCheckRoutePreview{}
	for _, r := range preview.Routes {
		byRoute[r.RouteID] = r
	}
	if r := byRoute[okRoute]; !r.Supported || r.Reason != "" {
		t.Fatalf("ok route=%+v", r)
	}
	if r := byRoute[offRoute]; r.Supported || r.Reason != reasonModelDisabled {
		t.Fatalf("off route=%+v", r)
	}
	if r := byRoute[unlistedRoute]; r.Supported || r.Reason != reasonModelUnlisted {
		t.Fatalf("unlisted route=%+v", r)
	}
	if r := byRoute[imageRoute]; r.Supported || r.Reason != reasonNotChat {
		t.Fatalf("image route=%+v", r)
	}
	// Per-key detail on the switched-off route: the active key says the model
	// is off, the disabled key says the key itself is off.
	keys := map[int64]healthCheckKeyPreview{}
	for _, k := range byRoute[offRoute].Keys {
		keys[k.KeyID] = k
	}
	if k := keys[activeKey]; k.Supported || k.Reason != reasonKeyModelOff {
		t.Fatalf("active key on off route=%+v", k)
	}
	if k := keys[disabledKey]; k.Supported || k.Reason != reasonKeyDisabled || k.Enabled {
		t.Fatalf("disabled key on off route=%+v", k)
	}

	recorder = httptest.NewRecorder()
	a.providerByID(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/providers/999999/health-check-targets", nil), adminCtx{})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown provider status=%d", recorder.Code)
	}
}

func TestHealthCheckTargetsPreviewOAuthRoutesAreProbeable(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth", AccessToken: "preview-oauth", AccountID: "preview-oauth"}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "preview-oauth", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, providerID, "oauth-public", "gpt-health", "chat,stream", 1)
	preview, err := a.healthCheckJobs.Preview(context.Background(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.AuthKind != "oauth" || preview.Probeable != 1 || len(preview.Routes) != 1 || !preview.Routes[0].Supported || len(preview.Routes[0].Keys) != 0 {
		t.Fatalf("preview=%+v", preview)
	}
}
