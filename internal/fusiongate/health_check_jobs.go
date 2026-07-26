package fusiongate

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	manualHealthCheckMaxItems    = 100
	manualHealthCheckConcurrency = 3
	manualConnectivityTimeout    = 12 * time.Second
	manualGenerationTimeout      = 35 * time.Second
	manualHealthCheckRetention   = 30 * time.Minute
)

var (
	errHealthCheckAlreadyRunning = errors.New("a health check is already running")
	errHealthCheckJobNotFound    = errors.New("health check job not found")
	errHealthProbeAlreadyRunning = errors.New("provider health check already in progress")
)

type healthCheckJobManager struct {
	app    *App
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	wg       sync.WaitGroup
	jobs     map[string]*healthCheckJob
	activeID string
}

type healthCheckTarget struct {
	ProviderID   int64
	ProviderName string
}

type healthCheckJob struct {
	ID         string                  `json:"id"`
	Mode       string                  `json:"mode"`
	Status     string                  `json:"status"`
	Total      int                     `json:"total"`
	Completed  int                     `json:"completed"`
	Healthy    int                     `json:"healthy"`
	Failed     int                     `json:"failed"`
	Skipped    int                     `json:"skipped"`
	CreatedAt  string                  `json:"created_at"`
	StartedAt  string                  `json:"started_at,omitempty"`
	FinishedAt string                  `json:"finished_at,omitempty"`
	CanCancel  bool                    `json:"can_cancel"`
	Results    []healthCheckItemResult `json:"results"`
	cancel     context.CancelFunc
	finishedAt time.Time
}

type healthCheckItemResult struct {
	ProviderID   int64  `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Status       string `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	FirstByteMS  int64  `json:"first_byte_ms"`
	Mode         string `json:"mode"`
	Model        string `json:"model,omitempty"`
	ModelCount   int    `json:"model_count"`
	Error        string `json:"error,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
}

func newHealthCheckJobManager(app *App) *healthCheckJobManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &healthCheckJobManager{
		app:    app,
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(map[string]*healthCheckJob),
	}
}

func (m *healthCheckJobManager) Close() {
	if m == nil {
		return
	}
	m.cancel()
	m.mu.Lock()
	for _, job := range m.jobs {
		if job.cancel != nil {
			job.cancel()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *healthCheckJobManager) Start(ctx context.Context, providerIDs []int64) (healthCheckJob, error) {
	return m.StartMode(ctx, providerIDs, healthCheckModeGeneration)
}

func (m *healthCheckJobManager) StartMode(ctx context.Context, providerIDs []int64, mode string) (healthCheckJob, error) {
	if !validHealthCheckMode(mode) {
		return healthCheckJob{}, errors.New("health check mode must be connectivity or generation")
	}
	targets, err := m.loadTargets(ctx, providerIDs)
	if err != nil {
		return healthCheckJob{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(time.Now())
	if active := m.jobs[m.activeID]; active != nil && (active.Status == "queued" || active.Status == "running") {
		return cloneHealthCheckJob(active), errHealthCheckAlreadyRunning
	}

	jobID := base64.RawURLEncoding.EncodeToString(randomBytes(18))
	jobCtx, cancel := context.WithCancel(m.ctx)
	job := &healthCheckJob{
		ID:        jobID,
		Mode:      mode,
		Status:    "queued",
		Total:     len(targets),
		CreatedAt: now(),
		CanCancel: true,
		Results:   make([]healthCheckItemResult, len(targets)),
		cancel:    cancel,
	}
	for i, target := range targets {
		job.Results[i] = healthCheckItemResult{
			ProviderID:   target.ProviderID,
			ProviderName: target.ProviderName,
			Mode:         mode,
			Status:       "queued",
		}
	}
	m.jobs[jobID] = job
	m.activeID = jobID
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(jobCtx, jobID)
	}()
	return cloneHealthCheckJob(job), nil
}

func (m *healthCheckJobManager) Get(jobID string) (healthCheckJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[jobID]
	if job == nil {
		return healthCheckJob{}, errHealthCheckJobNotFound
	}
	return cloneHealthCheckJob(job), nil
}

func (m *healthCheckJobManager) Active() (healthCheckJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[m.activeID]
	if job == nil || (job.Status != "queued" && job.Status != "running") {
		return healthCheckJob{}, errHealthCheckJobNotFound
	}
	return cloneHealthCheckJob(job), nil
}

func (m *healthCheckJobManager) Cancel(jobID string) (healthCheckJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[jobID]
	if job == nil {
		return healthCheckJob{}, errHealthCheckJobNotFound
	}
	if job.Status == "queued" || job.Status == "running" {
		job.Status = "cancelling"
		job.CanCancel = false
		if job.cancel != nil {
			job.cancel()
		}
	}
	return cloneHealthCheckJob(job), nil
}

func (m *healthCheckJobManager) loadTargets(ctx context.Context, providerIDs []int64) ([]healthCheckTarget, error) {
	if len(providerIDs) == 0 || len(providerIDs) > manualHealthCheckMaxItems {
		return nil, fmt.Errorf("select between 1 and %d authentication files", manualHealthCheckMaxItems)
	}
	ids := make([]int64, 0, len(providerIDs))
	seen := make(map[int64]struct{}, len(providerIDs))
	for _, id := range providerIDs {
		if id < 1 {
			return nil, errors.New("provider_ids must contain positive integers")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("select at least one authentication file")
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := m.app.db.QueryContext(ctx, `SELECT id,name,auth_kind FROM providers WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[int64]healthCheckTarget, len(ids))
	for rows.Next() {
		var target healthCheckTarget
		var authKind string
		if err := rows.Scan(&target.ProviderID, &target.ProviderName, &authKind); err != nil {
			return nil, err
		}
		if authKind != "oauth" {
			return nil, errors.New("health checks only support OAuth authentication files")
		}
		found[target.ProviderID] = target
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) != len(ids) {
		return nil, errors.New("one or more authentication files no longer exist")
	}
	targets := make([]healthCheckTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, found[id])
	}
	return targets, nil
}

func (m *healthCheckJobManager) run(ctx context.Context, jobID string) {
	m.mu.Lock()
	job := m.jobs[jobID]
	if job == nil {
		m.mu.Unlock()
		return
	}
	job.Status = "running"
	job.StartedAt = now()
	m.mu.Unlock()

	work := make(chan int)
	var workers sync.WaitGroup
	workerCount := manualHealthCheckConcurrency
	if workerCount > job.Total {
		workerCount = job.Total
	}
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range work {
				m.runItem(ctx, jobID, index)
			}
		}()
	}

sendLoop:
	for i := 0; i < job.Total; i++ {
		select {
		case <-ctx.Done():
			break sendLoop
		case work <- i:
		}
	}
	close(work)
	workers.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	job = m.jobs[jobID]
	if job == nil {
		return
	}
	cancelled := ctx.Err() != nil
	for i := range job.Results {
		if job.Results[i].Status == "queued" {
			job.Results[i].Status = "cancelled"
			job.Results[i].Error = "not started because the health check was cancelled"
			job.Results[i].FinishedAt = now()
			job.Completed++
			job.Skipped++
		}
	}
	if cancelled {
		job.Status = "cancelled"
	} else {
		job.Status = "completed"
	}
	job.CanCancel = false
	job.FinishedAt = now()
	job.finishedAt = time.Now()
	job.cancel = nil
	if m.activeID == jobID {
		m.activeID = ""
	}
}

func (m *healthCheckJobManager) runItem(ctx context.Context, jobID string, index int) {
	m.mu.Lock()
	job := m.jobs[jobID]
	if job == nil || index < 0 || index >= len(job.Results) {
		m.mu.Unlock()
		return
	}
	item := job.Results[index]
	mode := job.Mode
	job.Results[index].Status = "running"
	job.Results[index].StartedAt = now()
	m.mu.Unlock()

	// A small cancellable jitter avoids sending a recognizable burst to one or
	// more upstreams while keeping a manually requested job responsive.
	delay := 250*time.Millisecond + time.Duration(rand.Int63n(int64(500*time.Millisecond)))
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		m.finishItem(jobID, index, healthCheckItemResult{
			ProviderID: item.ProviderID, ProviderName: item.ProviderName, Mode: mode,
			Status: "cancelled", Error: "health check cancelled",
		})
		return
	case <-timer.C:
	}

	timeout := manualConnectivityTimeout
	if mode == healthCheckModeGeneration {
		timeout = manualGenerationTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	result, err := m.app.CheckProviderNowMode(probeCtx, item.ProviderID, mode)
	cancel()
	finished := healthCheckItemResult{
		ProviderID: item.ProviderID, ProviderName: item.ProviderName,
		Status: result.Status, Mode: mode, LatencyMS: result.LatencyMS,
		FirstByteMS: result.FirstByteMS, Model: result.Model,
		ModelCount: result.ModelCount, Error: result.Error,
	}
	if ctx.Err() != nil {
		finished.Status = "cancelled"
		finished.Error = "health check cancelled"
	} else if err != nil {
		switch {
		case errors.Is(err, errHealthProbeAlreadyRunning):
			finished.Status = "skipped"
			finished.Error = "another health check is already running for this authentication file"
		default:
			finished.Status = "config_error"
			finished.Error = sanitizeError(err.Error())
		}
	}
	m.finishItem(jobID, index, finished)
}

func (m *healthCheckJobManager) finishItem(jobID string, index int, result healthCheckItemResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[jobID]
	if job == nil || index < 0 || index >= len(job.Results) {
		return
	}
	result.StartedAt = job.Results[index].StartedAt
	result.FinishedAt = now()
	job.Results[index] = result
	job.Completed++
	switch result.Status {
	case "healthy":
		job.Healthy++
	case "skipped", "cancelled":
		job.Skipped++
	default:
		job.Failed++
	}
}

func (m *healthCheckJobManager) pruneLocked(at time.Time) {
	for id, job := range m.jobs {
		if job.Status == "queued" || job.Status == "running" || job.Status == "cancelling" {
			continue
		}
		if !job.finishedAt.IsZero() && at.Sub(job.finishedAt) > manualHealthCheckRetention {
			delete(m.jobs, id)
		}
	}
}

func cloneHealthCheckJob(job *healthCheckJob) healthCheckJob {
	if job == nil {
		return healthCheckJob{}
	}
	clone := *job
	clone.cancel = nil
	clone.Results = append([]healthCheckItemResult(nil), job.Results...)
	return clone
}
