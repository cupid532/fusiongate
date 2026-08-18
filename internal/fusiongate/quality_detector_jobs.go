package fusiongate

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	qualityDetectorMaxTargets            = 100
	qualityDetectorJobRetention          = 24 * time.Hour
	qualityDetectorRetentionCleanupEvery = 1 * time.Hour
	qualityDetectorMaxReportBytes        = 262144 // 256 KiB
	qualityDetectorHistoryLimit          = 50
)

var (
	errQualityDetectorJobNotFound  = errors.New("quality detector job not found")
	errQualityDetectorJobNotActive = errors.New("quality detector job is not active")
	errQualityDetectorBatchRunning = errors.New("a quality detection batch is already running")
)

var qualityDetectorSensitiveKey = regexp.MustCompile(`(?i)(key|token|secret|password|credential|authorization|bearer|cookie)`)

// qualityDetectorJob is a persisted batch of targeted detections.
type qualityDetectorJob struct {
	ID         string                   `json:"id"`
	Status     string                   `json:"status"`
	Preset     string                   `json:"preset"`
	Total      int                      `json:"total"`
	Completed  int                      `json:"completed"`
	Succeeded  int                      `json:"succeeded"`
	Failed     int                      `json:"failed"`
	Skipped    int                      `json:"skipped"`
	Cancelled  int                      `json:"cancelled"`
	CreatedAt  string                   `json:"created_at"`
	StartedAt  string                   `json:"started_at,omitempty"`
	FinishedAt string                   `json:"finished_at,omitempty"`
	Items      []qualityDetectorJobItem `json:"items,omitempty"`
}

// qualityDetectorJobItem is one targeted detection inside a batch. It only ever
// stores a redacted snapshot; the upstream key and route token are never written.
type qualityDetectorJobItem struct {
	ID              int64  `json:"id"`
	Position        int    `json:"position"`
	TargetID        string `json:"target_id"`
	Model           string `json:"model"`
	ProviderID      int64  `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderType    string `json:"provider_type"`
	ProviderKeyID   int64  `json:"provider_key_id"`
	ProviderKeyName string `json:"provider_key_name"`
	ProviderKeyHint string `json:"provider_key_hint"`
	UpstreamModel   string `json:"upstream_model"`
	Status          string `json:"status"`
	Verdict         string `json:"verdict"`
	Error           string `json:"error"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	Report          string `json:"report,omitempty"`
}

// qualityDetectorJobManager owns batch lifecycle and serial execution. The
// detector sidecar can only run one detection at a time, so a single batch runs
// its targets sequentially and at most one batch is active at any moment.
type qualityDetectorJobManager struct {
	app *App

	mu       sync.Mutex
	activeID string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newQualityDetectorJobManager(app *App) *qualityDetectorJobManager {
	return &qualityDetectorJobManager{app: app}
}

func (m *qualityDetectorJobManager) Close() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *qualityDetectorJobManager) HasActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeID != ""
}

func (m *qualityDetectorJobManager) ActiveID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeID
}

func (m *qualityDetectorJobManager) release(jobID string) {
	m.mu.Lock()
	if m.activeID == jobID {
		m.activeID = ""
		m.cancel = nil
	}
	m.mu.Unlock()
}

// convergeInterrupted marks jobs left behind by a previous process as interrupted
// so they never resume sending requests after a restart.
func (m *qualityDetectorJobManager) convergeInterrupted(ctx context.Context) {
	_, _ = m.app.db.ExecContext(ctx, `UPDATE quality_detector_jobs SET status='interrupted', finished_at=? WHERE status IN ('queued','running','cancelling')`, now())
	_, _ = m.app.db.ExecContext(ctx, `UPDATE quality_detector_job_items SET status='interrupted', finished_at=? WHERE status IN ('queued','running')`, now())
}

func (m *qualityDetectorJobManager) prune(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-qualityDetectorJobRetention).Format(time.RFC3339Nano)
	m.mu.Lock()
	active := m.activeID
	m.mu.Unlock()
	if active != "" {
		_, _ = m.app.db.ExecContext(ctx, `DELETE FROM quality_detector_jobs WHERE created_at < ? AND id != ?`, cutoff, active)
	} else {
		_, _ = m.app.db.ExecContext(ctx, `DELETE FROM quality_detector_jobs WHERE created_at < ?`, cutoff)
	}
}

func (m *qualityDetectorJobManager) Create(ctx context.Context, preset string, targetIDs []string) (qualityDetectorJob, error) {
	if !qualityDetectorPresets[preset] {
		return qualityDetectorJob{}, errors.New("choose a supported preset")
	}
	seen := map[string]bool{}
	ordered := make([]string, 0, len(targetIDs))
	for _, id := range targetIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	if len(ordered) == 0 {
		return qualityDetectorJob{}, errors.New("select at least one target")
	}
	if len(ordered) > qualityDetectorMaxTargets {
		return qualityDetectorJob{}, fmt.Errorf("at most %d targets per batch", qualityDetectorMaxTargets)
	}

	targets, err := m.app.qualityDetectorTargets(ctx)
	if err != nil {
		return qualityDetectorJob{}, err
	}
	byID := map[string]qualityDetectorTarget{}
	for _, t := range targets {
		byID[t.ID] = t
	}
	items := make([]qualityDetectorJobItem, 0, len(ordered))
	for i, id := range ordered {
		t, ok := byID[id]
		if !ok {
			return qualityDetectorJob{}, fmt.Errorf("target %q is unavailable", id)
		}
		items = append(items, qualityDetectorJobItem{
			Position:        i,
			TargetID:        id,
			Model:           t.Model,
			ProviderID:      t.ProviderID,
			ProviderName:    t.ProviderName,
			ProviderType:    t.ProviderType,
			ProviderKeyID:   t.ProviderKeyID,
			ProviderKeyName: t.ProviderKeyName,
			ProviderKeyHint: t.ProviderKeyHint,
			UpstreamModel:   t.UpstreamModel,
			Status:          "queued",
		})
	}

	m.mu.Lock()
	if m.activeID != "" {
		m.mu.Unlock()
		return qualityDetectorJob{}, errQualityDetectorBatchRunning
	}
	jobID := base64.RawURLEncoding.EncodeToString(randomBytes(18))
	jobCtx, cancel := context.WithCancel(context.Background())
	m.activeID = jobID
	m.cancel = cancel
	m.mu.Unlock()

	createdAt := now()
	if err := m.persistNewJob(jobID, preset, items, createdAt); err != nil {
		m.release(jobID)
		cancel()
		return qualityDetectorJob{}, err
	}

	job := qualityDetectorJob{ID: jobID, Status: "queued", Preset: preset, Total: len(items), CreatedAt: createdAt, Items: items}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(jobCtx, jobID)
	}()
	return job, nil
}

func (m *qualityDetectorJobManager) persistNewJob(jobID, preset string, items []qualityDetectorJobItem, createdAt string) error {
	tx, err := m.app.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO quality_detector_jobs(id,status,preset,total,created_at) VALUES(?,?,?,?,?)`, jobID, "queued", preset, len(items), createdAt); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`INSERT INTO quality_detector_job_items(job_id,position,target_id,model,provider_id,provider_name,provider_type,provider_key_id,provider_key_name,provider_key_hint,upstream_model,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			jobID, it.Position, it.TargetID, it.Model, it.ProviderID, it.ProviderName, it.ProviderType, it.ProviderKeyID, it.ProviderKeyName, it.ProviderKeyHint, it.UpstreamModel, "queued"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *qualityDetectorJobManager) run(ctx context.Context, jobID string) {
	defer m.release(jobID)

	preset := m.loadPreset(jobID)
	_, _ = m.app.db.Exec(`UPDATE quality_detector_jobs SET status='running', started_at=? WHERE id=?`, now(), jobID)

	items := m.loadItemsForRun(jobID)
	cancelled := false
	for _, item := range items {
		if ctx.Err() != nil {
			cancelled = true
			m.finishItem(jobID, item.ID, "cancelled", "", "", "")
			continue
		}
		m.runItem(ctx, jobID, preset, item)
		if ctx.Err() != nil {
			cancelled = true
		}
	}
	if cancelled {
		_, _ = m.app.db.Exec(`UPDATE quality_detector_jobs SET status='cancelled', finished_at=? WHERE id=?`, now(), jobID)
	} else {
		_, _ = m.app.db.Exec(`UPDATE quality_detector_jobs SET status='completed', finished_at=? WHERE id=?`, now(), jobID)
	}
}

func (m *qualityDetectorJobManager) runItem(ctx context.Context, jobID, preset string, item qualityDetectorJobItem) {
	target, err := m.app.qualityDetectorTarget(ctx, item.TargetID)
	if err != nil {
		m.finishItem(jobID, item.ID, "skipped", "", sanitizeError(err.Error()), "")
		return
	}
	_, _ = m.app.db.Exec(`UPDATE quality_detector_job_items SET status='running', started_at=? WHERE id=? AND job_id=?`, now(), item.ID, jobID)
	report, verdict, runErr := m.app.runQualityDetectionOnce(ctx, preset, target)
	if runErr != nil {
		if ctx.Err() != nil {
			m.finishItem(jobID, item.ID, "cancelled", "", "", "")
		} else {
			m.finishItem(jobID, item.ID, "failed", "", sanitizeError(qualityDetectorSecret.ReplaceAllString(runErr.Error(), "<redacted-key>")), "")
		}
		return
	}
	m.finishItem(jobID, item.ID, "completed", verdict, "", report)
}

func (m *qualityDetectorJobManager) finishItem(jobID string, itemID int64, status, verdict, errMsg, report string) {
	_, _ = m.app.db.Exec(`UPDATE quality_detector_job_items SET status=?, verdict=?, error=?, report=?, finished_at=? WHERE id=? AND job_id=?`,
		status, verdict, errMsg, report, now(), itemID, jobID)
}

func (m *qualityDetectorJobManager) loadPreset(jobID string) string {
	var preset string
	if err := m.app.db.QueryRow(`SELECT preset FROM quality_detector_jobs WHERE id=?`, jobID).Scan(&preset); err != nil || preset == "" {
		return "low"
	}
	return preset
}

func (m *qualityDetectorJobManager) loadItemsForRun(jobID string) []qualityDetectorJobItem {
	rows, err := m.app.db.Query(`SELECT id,target_id,model,provider_id,provider_name,provider_type,provider_key_id,provider_key_name,provider_key_hint,upstream_model FROM quality_detector_job_items WHERE job_id=? ORDER BY position`, jobID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	items := make([]qualityDetectorJobItem, 0)
	for rows.Next() {
		var it qualityDetectorJobItem
		if err := rows.Scan(&it.ID, &it.TargetID, &it.Model, &it.ProviderID, &it.ProviderName, &it.ProviderType, &it.ProviderKeyID, &it.ProviderKeyName, &it.ProviderKeyHint, &it.UpstreamModel); err != nil {
			continue
		}
		items = append(items, it)
	}
	return items
}

func (m *qualityDetectorJobManager) Get(jobID string) (qualityDetectorJob, error) {
	var job qualityDetectorJob
	err := m.app.reader().QueryRow(`SELECT id,status,preset,total,created_at,COALESCE(started_at,''),COALESCE(finished_at,'') FROM quality_detector_jobs WHERE id=?`, jobID).
		Scan(&job.ID, &job.Status, &job.Preset, &job.Total, &job.CreatedAt, &job.StartedAt, &job.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return qualityDetectorJob{}, errQualityDetectorJobNotFound
	}
	if err != nil {
		return qualityDetectorJob{}, err
	}
	items, err := m.loadItems(jobID)
	if err != nil {
		return qualityDetectorJob{}, err
	}
	job.Items = items
	m.fillCounters(&job, items)
	return job, nil
}

func (m *qualityDetectorJobManager) loadItems(jobID string) ([]qualityDetectorJobItem, error) {
	rows, err := m.app.reader().Query(`SELECT id,position,target_id,model,provider_id,provider_name,provider_type,provider_key_id,provider_key_name,provider_key_hint,upstream_model,status,verdict,error,COALESCE(started_at,''),COALESCE(finished_at,''),report FROM quality_detector_job_items WHERE job_id=? ORDER BY position`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]qualityDetectorJobItem, 0)
	for rows.Next() {
		var it qualityDetectorJobItem
		if err := rows.Scan(&it.ID, &it.Position, &it.TargetID, &it.Model, &it.ProviderID, &it.ProviderName, &it.ProviderType, &it.ProviderKeyID, &it.ProviderKeyName, &it.ProviderKeyHint, &it.UpstreamModel, &it.Status, &it.Verdict, &it.Error, &it.StartedAt, &it.FinishedAt, &it.Report); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (m *qualityDetectorJobManager) fillCounters(job *qualityDetectorJob, items []qualityDetectorJobItem) {
	job.Completed, job.Succeeded, job.Failed, job.Skipped, job.Cancelled = 0, 0, 0, 0, 0
	for _, it := range items {
		switch it.Status {
		case "completed":
			job.Completed++
			job.Succeeded++
		case "failed":
			job.Completed++
			job.Failed++
		case "skipped":
			job.Completed++
			job.Skipped++
		case "cancelled", "interrupted":
			job.Completed++
			job.Cancelled++
		}
	}
}

func (m *qualityDetectorJobManager) List(limit int) ([]qualityDetectorJob, error) {
	if limit <= 0 || limit > qualityDetectorHistoryLimit {
		limit = qualityDetectorHistoryLimit
	}
	rows, err := m.app.reader().Query(`
		SELECT j.id,j.status,j.preset,j.total,j.created_at,COALESCE(j.started_at,''),COALESCE(j.finished_at,''),
		       COALESCE(SUM(CASE WHEN i.status IN ('completed','failed','skipped','cancelled','interrupted') THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN i.status='completed' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN i.status='failed' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN i.status='skipped' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN i.status IN ('cancelled','interrupted') THEN 1 ELSE 0 END),0)
		FROM quality_detector_jobs j LEFT JOIN quality_detector_job_items i ON i.job_id=j.id
		GROUP BY j.id ORDER BY j.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]qualityDetectorJob, 0)
	for rows.Next() {
		var job qualityDetectorJob
		if err := rows.Scan(&job.ID, &job.Status, &job.Preset, &job.Total, &job.CreatedAt, &job.StartedAt, &job.FinishedAt, &job.Completed, &job.Succeeded, &job.Failed, &job.Skipped, &job.Cancelled); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (m *qualityDetectorJobManager) Cancel(jobID string) (qualityDetectorJob, error) {
	m.mu.Lock()
	if m.activeID != jobID {
		m.mu.Unlock()
		return qualityDetectorJob{}, errQualityDetectorJobNotActive
	}
	cancel := m.cancel
	m.mu.Unlock()

	// Mark cancelling before cancel() so the run goroutine's final state always
	// wins; only touch jobs that are actually still queued or running.
	_, _ = m.app.db.Exec(`UPDATE quality_detector_jobs SET status='cancelling' WHERE id=? AND status IN ('queued','running')`, jobID)

	if cancel != nil {
		cancel()
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	_ = m.app.stopQualityDetectorSidecar(stopCtx)

	return m.Get(jobID)
}

// runQualityDetectorRetentionLoop trims expired jobs on a timer.
func (a *App) runQualityDetectorRetentionLoop(ctx context.Context) {
	ticker := time.NewTicker(qualityDetectorRetentionCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.qualityDetectorJobs != nil {
				a.qualityDetectorJobs.prune(ctx)
			}
		}
	}
}

// sanitizeQualityDetectorReport validates and redacts a sidecar report for safe
// persistence. It refuses reports that persisted auth values, are malformed, or
// exceed the storage cap.
func sanitizeQualityDetectorReport(data []byte) (report, verdict string, ok bool) {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return "", "", false
	}
	if persisted, isBool := value["auth_values_persisted"].(bool); isBool && persisted {
		return "", "", false
	}
	redactQualityJSON(value)
	verdict = qualityDetectorVerdict(value)
	out, err := json.Marshal(value)
	if err != nil {
		return "", "", false
	}
	s := string(out)
	s = qualityDetectorSecret.ReplaceAllString(s, "<redacted-key>")
	if len(s) > qualityDetectorMaxReportBytes {
		return "", "", false
	}
	return s, verdict, true
}

func qualityDetectorVerdict(value map[string]any) string {
	if v, ok := value["overall_verdict"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := value["title_cn"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func redactQualityJSON(v any) {
	switch value := v.(type) {
	case map[string]any:
		for k, child := range value {
			if qualityDetectorSensitiveKey.MatchString(k) {
				value[k] = "<redacted>"
				continue
			}
			redactQualityJSON(child)
		}
	case []any:
		for _, child := range value {
			redactQualityJSON(child)
		}
	}
}

func (a *App) handleQualityDetectorJobs(w http.ResponseWriter, r *http.Request) {
	if a.qualityDetectorJobs == nil {
		fail(w, http.StatusServiceUnavailable, "quality_detector_unavailable", "quality detector jobs are unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := qualityDetectorHistoryLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= qualityDetectorHistoryLimit {
				limit = n
			}
		}
		jobs, err := a.qualityDetectorJobs.List(limit)
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	case http.MethodPost:
		var in struct {
			Preset    string   `json:"preset"`
			TargetIDs []string `json:"target_ids"`
		}
		if err := readJSON(r, &in); err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		job, err := a.qualityDetectorJobs.Create(r.Context(), in.Preset, in.TargetIDs)
		if errors.Is(err, errQualityDetectorBatchRunning) {
			fail(w, http.StatusConflict, "quality_detector_busy", err.Error())
			return
		}
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_quality_detector_target", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST required")
	}
}

func (a *App) handleQualityDetectorJobByID(w http.ResponseWriter, r *http.Request, id string) {
	if a.qualityDetectorJobs == nil {
		fail(w, http.StatusServiceUnavailable, "quality_detector_unavailable", "quality detector jobs are unavailable")
		return
	}
	if id == "" || len(id) > 64 || strings.Contains(id, "/") {
		fail(w, http.StatusNotFound, "not_found", "quality detector job not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		job, err := a.qualityDetectorJobs.Get(id)
		if errors.Is(err, errQualityDetectorJobNotFound) {
			fail(w, http.StatusNotFound, "not_found", "quality detector job not found")
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "database_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		job, err := a.qualityDetectorJobs.Cancel(id)
		if errors.Is(err, errQualityDetectorJobNotActive) {
			fail(w, http.StatusConflict, "quality_detector_not_active", "quality detector job is not active")
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "quality_detector_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or DELETE required")
	}
}
