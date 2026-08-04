package fusiongate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var healthProbeSumPattern = regexp.MustCompile(`What is (\d+) \+ (\d+)`)

func healthProbeAnswer(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	match := healthProbeSumPattern.FindSubmatch(body)
	if len(match) != 3 {
		return "invalid probe"
	}
	left, _ := strconv.Atoi(string(match[1]))
	right, _ := strconv.Atoi(string(match[2]))
	return strconv.Itoa(left + right)
}

func TestManualHealthCheckJobCompletesSelectedProviders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSEText("manual-health", healthProbeAnswer(r))))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	ids := make([]int64, 0, 2)
	for i, accountID := range []string{"health-account-a", "health-account-b"} {
		credential := ProviderCredential{
			Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
			AccessToken: "health-access-" + accountID, AccountID: accountID,
			ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		}
		providerID, _, err := a.saveOAuthProvider(context.Background(), "health-job-"+accountID, i+1, credential, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`UPDATE providers SET base_url=? WHERE id=?`, upstream.URL, providerID); err != nil {
			t.Fatal(err)
		}
		insertTestRoute(t, a, providerID, "health-public-"+accountID, "gpt-health", "chat,stream", 1)
		ids = append(ids, providerID)
	}

	job, err := a.healthCheckJobs.StartModels(context.Background(), []int64{ids[0], ids[1], ids[0]}, nil, nil, "all")
	if err != nil {
		t.Fatal(err)
	}
	if job.Total != 2 {
		t.Fatalf("job total=%d, want 2 unique providers", job.Total)
	}
	job = waitForHealthCheckJob(t, a, job.ID)
	if job.Status != "completed" || job.Completed != 2 || job.Healthy != 2 || job.Failed != 0 || job.Skipped != 0 {
		t.Fatalf("job=%+v", job)
	}
	for _, result := range job.Results {
		if result.Status != "healthy" || result.LatencyMS < 0 || result.StartedAt == "" || result.FinishedAt == "" {
			t.Fatalf("result=%+v", result)
		}
		var status string
		if err := a.db.QueryRow(`SELECT health_check_status FROM model_routes WHERE id=?`, result.RouteID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "healthy" {
			t.Fatalf("route %d status=%q", result.RouteID, status)
		}
	}
}

func TestManualHealthCheckExpandsSelectedKeysPerModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "multi-key-health", "openai_compatible", "https://example.test", "legacy", 1, 1, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	routeID := insertTestRoute(t, a, providerID, "shared-model", "upstream-model", "chat,stream", 1)
	first := insertProviderKeyForTest(t, a, providerID, "key-one", "Key One", "", "inherit", nil, 1, 0)
	second := insertProviderKeyForTest(t, a, providerID, "key-two", "Key Two", "", "inherit", nil, 1, 1)
	for _, keyID := range []int64{first, second} {
		if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,display_name,capabilities,discovered_at,enabled) VALUES(?,?,?,?,?,1)`, keyID, "upstream-model", "Shared", "chat,stream", now()); err != nil {
			t.Fatal(err)
		}
	}
	providers, err := a.healthCheckJobs.loadTargets(context.Background(), []int64{providerID})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := a.healthCheckJobs.loadModelTargets(context.Background(), providers, []int64{routeID}, []int64{first, second}, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].ProviderKeyID != first || targets[1].ProviderKeyID != second {
		t.Fatalf("targets=%+v", targets)
	}
	for i, target := range targets {
		p, err := a.loadDiscoveryProvider(context.Background(), providerID)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.applyProviderKeyByID(context.Background(), &p, target.ProviderKeyID, target.UpstreamModel); err != nil {
			t.Fatal(err)
		}
		if want := []string{"key-one", "key-two"}[i]; p.Credential != want {
			t.Fatalf("credential=%q want %q", p.Credential, want)
		}
	}
}

func TestManualHealthCheckRejectsOverlappingJob(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSE("manual-health")))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "health-overlap", AccountID: "health-overlap",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "health-overlap", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE providers SET base_url=? WHERE id=?`, upstream.URL, providerID); err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, providerID, "health-overlap", "gpt-health", "chat,stream", 1)

	job, err := a.healthCheckJobs.StartModels(context.Background(), []int64{providerID}, nil, nil, "all")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.healthCheckJobs.StartModels(context.Background(), []int64{providerID}, nil, nil, "all"); !errors.Is(err, errHealthCheckAlreadyRunning) {
		t.Fatalf("second start error=%v", err)
	}
	if _, err := a.healthCheckJobs.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	job = waitForHealthCheckJob(t, a, job.ID)
	if job.Status != "cancelled" || job.Completed != job.Total {
		t.Fatalf("cancelled job=%+v", job)
	}
}

func TestHealthChecksAdminEndpointStartsJob(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "endpoint-health", AccountID: "endpoint-health",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "endpoint-health", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, providerID, "endpoint-health", "gpt-health", "chat,stream", 1)

	body := `{"provider_ids":[` + jsonNumber(providerID) + `]}`
	recorder := httptest.NewRecorder()
	a.healthChecks(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/health-checks", strings.NewReader(body)), adminCtx{})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var job healthCheckJob
	if err := json.Unmarshal(recorder.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.Total != 1 || job.Mode != healthCheckModeGeneration {
		t.Fatalf("job=%+v", job)
	}
	_, _ = a.healthCheckJobs.Cancel(job.ID)
	_ = waitForHealthCheckJob(t, a, job.ID)
}

func TestHealthChecksAdminEndpointAcceptsGenerationMode(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "generation-health", AccountID: "generation-health",
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	providerID, _, err := a.saveOAuthProvider(context.Background(), "generation-health", 1, credential, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	insertTestRoute(t, a, providerID, "generation-health", "gpt-health", "chat,stream", 1)

	body := `{"provider_ids":[` + jsonNumber(providerID) + `],"model_scope":"all"}`
	recorder := httptest.NewRecorder()
	a.healthChecks(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/health-checks", strings.NewReader(body)), adminCtx{})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var job healthCheckJob
	if err := json.Unmarshal(recorder.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Mode != healthCheckModeGeneration {
		t.Fatalf("job=%+v", job)
	}
	_, _ = a.healthCheckJobs.Cancel(job.ID)
	_ = waitForHealthCheckJob(t, a, job.ID)
}

func TestHealthChecksAdminEndpointRejectsInvalidMode(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	recorder := httptest.NewRecorder()
	a.healthChecks(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/health-checks", strings.NewReader(`{"provider_ids":[1],"mode":"destructive"}`)), adminCtx{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func waitForHealthCheckJob(t *testing.T, a *App, jobID string) healthCheckJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := a.healthCheckJobs.Get(jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "completed" || job.Status == "cancelled" {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("health check job did not finish")
	return healthCheckJob{}
}

func jsonNumber(v int64) string {
	data, _ := json.Marshal(v)
	return string(data)
}
