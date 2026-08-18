package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type qualityJobFixture struct {
	mu               sync.Mutex
	starts           int
	reports          int
	status           string
	statusAfterStart string
	failFirstStart   bool
}

func newQualityJobFixture(t *testing.T, statusAfterStart string) (*httptest.Server, *qualityJobFixture) {
	t.Helper()
	f := &qualityJobFixture{status: "idle", statusAfterStart: statusAfterStart}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case "/api/detector/start":
			f.mu.Lock()
			f.starts++
			fail := f.failFirstStart && f.starts == 1
			if !fail {
				f.status = f.statusAfterStart
			}
			f.mu.Unlock()
			if fail {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream failed with sk-secret-abcdef123456"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "running", "session_id": "run"})
		case "/api/detector/status":
			f.mu.Lock()
			writeJSON(w, http.StatusOK, map[string]any{"status": f.status, "session_id": "run"})
			f.mu.Unlock()
		case "/api/detector/report":
			f.mu.Lock()
			f.reports++
			f.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"auth_values_persisted": false, "overall_verdict": "通过", "session_id": "run"})
		case "/api/detector/stop":
			writeJSON(w, http.StatusOK, map[string]any{"status": "stopped"})
		default:
			http.NotFound(w, r)
		}
	}))
	return server, f
}

func newQualityJobApp(t *testing.T, server *httptest.Server) *App {
	t.Helper()
	cfg := testConfig(t)
	cfg.QualityDetectorURL = server.URL
	cfg.QualityDetectorBaseURL = "http://127.0.0.1:8787/v1"
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func waitQualityJob(t *testing.T, m *qualityDetectorJobManager, id string) qualityDetectorJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := m.Get(id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		switch job.Status {
		case "completed", "cancelled", "interrupted":
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal state", id)
	return qualityDetectorJob{}
}

func TestQualityDetectorBatchRunsTargetsSequentially(t *testing.T) {
	server, f := newQualityJobFixture(t, "complete")
	defer server.Close()
	a := newQualityJobApp(t, server)

	first := insertQualityDetectorTarget(t, a, "Sol 渠道 A", "sk-first")
	second := insertQualityDetectorTarget(t, a, "Sol 渠道 B", "sk-second")

	job, err := a.qualityDetectorJobs.Create(context.Background(), "low", []string{first.ID, second.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Total != 2 {
		t.Fatalf("total=%d want 2", job.Total)
	}

	done := waitQualityJob(t, a.qualityDetectorJobs, job.ID)
	if done.Status != "completed" {
		t.Fatalf("status=%s want completed", done.Status)
	}
	f.mu.Lock()
	starts, reports := f.starts, f.reports
	f.mu.Unlock()
	if starts != 2 || reports != 2 {
		t.Fatalf("starts=%d reports=%d want 2/2", starts, reports)
	}
	if done.Completed != 2 || done.Succeeded != 2 || done.Failed != 0 {
		t.Fatalf("counters=%+v", done)
	}
	for _, item := range done.Items {
		if item.Status != "completed" || item.Verdict != "通过" {
			t.Fatalf("item=%+v", item)
		}
	}
}

func TestQualityDetectorBatchFailureContinuesQueue(t *testing.T) {
	server, f := newQualityJobFixture(t, "complete")
	f.failFirstStart = true
	defer server.Close()
	a := newQualityJobApp(t, server)

	first := insertQualityDetectorTarget(t, a, "Failing 渠道", "sk-fail")
	second := insertQualityDetectorTarget(t, a, "Healthy 渠道", "sk-ok")

	job, err := a.qualityDetectorJobs.Create(context.Background(), "low", []string{first.ID, second.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	done := waitQualityJob(t, a.qualityDetectorJobs, job.ID)
	if done.Status != "completed" {
		t.Fatalf("status=%s want completed", done.Status)
	}
	if done.Failed != 1 || done.Succeeded != 1 {
		t.Fatalf("counters=%+v", done)
	}
	if done.Items[0].Status != "failed" || done.Items[1].Status != "completed" {
		t.Fatalf("items=%+v", done.Items)
	}
	if strings.Contains(done.Items[0].Error, "sk-secret-abcdef123456") {
		t.Fatalf("sidecar error was not redacted: %q", done.Items[0].Error)
	}
}

func TestQualityDetectorBatchCancel(t *testing.T) {
	server, f := newQualityJobFixture(t, "running")
	defer server.Close()
	a := newQualityJobApp(t, server)

	first := insertQualityDetectorTarget(t, a, "Slow 渠道", "sk-slow")
	job, err := a.qualityDetectorJobs.Create(context.Background(), "low", []string{first.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		started := f.starts >= 1
		f.mu.Unlock()
		if started {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelled, err := a.qualityDetectorJobs.Cancel(job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != "cancelling" && cancelled.Status != "cancelled" {
		t.Fatalf("cancel status=%s", cancelled.Status)
	}

	done := waitQualityJob(t, a.qualityDetectorJobs, job.ID)
	if done.Status != "cancelled" {
		t.Fatalf("status=%s want cancelled", done.Status)
	}
	if done.Items[0].Status != "cancelled" {
		t.Fatalf("item=%+v", done.Items[0])
	}
}

func TestQualityDetectorBatchDedupAndValidation(t *testing.T) {
	server, _ := newQualityJobFixture(t, "complete")
	defer server.Close()
	a := newQualityJobApp(t, server)

	target := insertQualityDetectorTarget(t, a, "Dedup 渠道", "sk-dup")

	if _, err := a.qualityDetectorJobs.Create(context.Background(), "low", nil); err == nil {
		t.Fatal("empty selection should fail")
	}
	if _, err := a.qualityDetectorJobs.Create(context.Background(), "custom", []string{target.ID}); err == nil {
		t.Fatal("unsupported preset should fail")
	}
	job, err := a.qualityDetectorJobs.Create(context.Background(), "low", []string{target.ID, target.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Total != 1 {
		t.Fatalf("dedup total=%d want 1", job.Total)
	}
	_ = waitQualityJob(t, a.qualityDetectorJobs, job.ID)
}

func TestQualityDetectorReportSanitization(t *testing.T) {
	// A secret inside a sensitive-named field is whole-field redacted.
	valid := []byte(`{"auth_values_persisted":false,"overall_verdict":"通过","candidate_configuration_without_key":"sk-abcdef123456"}`)
	report, verdict, ok := sanitizeQualityDetectorReport(valid)
	if !ok || verdict != "通过" {
		t.Fatalf("valid report: ok=%v verdict=%q", ok, verdict)
	}
	if strings.Contains(report, "sk-abcdef123456") {
		t.Fatalf("secret not redacted: %s", report)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(report), &value); err != nil {
		t.Fatalf("redacted report is not valid JSON: %v", err)
	}
	if value["candidate_configuration_without_key"] != "<redacted>" {
		t.Fatalf("sensitive field not redacted: %#v", value["candidate_configuration_without_key"])
	}

	// A secret inside a non-sensitive field is caught by the value regex.
	plain := []byte(`{"auth_values_persisted":false,"overall_verdict":"通过","limitations":["seed sk-abcdef123456"]}`)
	report2, _, ok2 := sanitizeQualityDetectorReport(plain)
	if !ok2 {
		t.Fatal("plain report rejected")
	}
	if strings.Contains(report2, "sk-abcdef123456") {
		t.Fatalf("value secret not redacted: %s", report2)
	}
	if !strings.Contains(report2, "<redacted-key>") {
		t.Fatalf("expected redaction marker: %s", report2)
	}

	persisted := []byte(`{"auth_values_persisted":true,"overall_verdict":"通过"}`)
	if _, _, ok := sanitizeQualityDetectorReport(persisted); ok {
		t.Fatal("report with persisted auth values must be rejected")
	}

	if _, _, ok := sanitizeQualityDetectorReport([]byte(`{not json`)); ok {
		t.Fatal("malformed report must be rejected")
	}
}

func TestQualityDetectorTablesCreated(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, table := range []string{"quality_detector_jobs", "quality_detector_job_items"} {
		var name string
		if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil || name != table {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestQualityDetectorJobHistoryPrune(t *testing.T) {
	server, _ := newQualityJobFixture(t, "complete")
	defer server.Close()
	a := newQualityJobApp(t, server)

	target := insertQualityDetectorTarget(t, a, "Prune 渠道", "sk-prune")
	job, err := a.qualityDetectorJobs.Create(context.Background(), "low", []string{target.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = waitQualityJob(t, a.qualityDetectorJobs, job.ID)

	if _, err := a.db.Exec(`UPDATE quality_detector_jobs SET created_at=? WHERE id=?`, time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	a.qualityDetectorJobs.prune(context.Background())
	if _, err := a.qualityDetectorJobs.Get(job.ID); err == nil {
		t.Fatal("expired job was not pruned")
	}
}
