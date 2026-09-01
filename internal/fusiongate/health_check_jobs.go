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
	manualHealthCheckMaxProviders = 100
	manualHealthCheckMaxItems     = 1000
	manualHealthCheckConcurrency  = 3
	manualGenerationTimeout       = 35 * time.Second
	manualHealthCheckRetention    = 30 * time.Minute
)

var (
	errHealthCheckAlreadyRunning = errors.New("a health check is already running")
	errHealthCheckJobNotFound    = errors.New("health check job not found")
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
	ProviderID      int64
	ProviderName    string
	ProviderKeyID   int64
	ProviderKeyName string
	ProviderKeyHint string
	RouteID         int64
	PublicName      string
	UpstreamModel   string
	Capabilities    string
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
	ProviderID      int64  `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderKeyID   int64  `json:"provider_key_id,omitempty"`
	ProviderKeyName string `json:"provider_key_name,omitempty"`
	ProviderKeyHint string `json:"provider_key_hint,omitempty"`
	RouteID         int64  `json:"route_id,omitempty"`
	PublicName      string `json:"public_name,omitempty"`
	Status          string `json:"status"`
	LatencyMS       int64  `json:"latency_ms"`
	FirstByteMS     int64  `json:"first_byte_ms"`
	Mode            string `json:"mode"`
	Model           string `json:"model,omitempty"`
	ModelCount      int    `json:"model_count"`
	Error           string `json:"error,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
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

func (m *healthCheckJobManager) StartModels(ctx context.Context, providerIDs, routeIDs, providerKeyIDs []int64, modelScope string) (healthCheckJob, error) {
	mode := healthCheckModeGeneration
	providerTargets, err := m.loadTargets(ctx, providerIDs)
	if err != nil {
		return healthCheckJob{}, err
	}
	if modelScope == "" {
		modelScope = "all"
	}
	targets, err := m.loadModelTargets(ctx, providerTargets, routeIDs, providerKeyIDs, modelScope)
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
			ProviderID:      target.ProviderID,
			ProviderName:    target.ProviderName,
			ProviderKeyID:   target.ProviderKeyID,
			ProviderKeyName: target.ProviderKeyName,
			ProviderKeyHint: target.ProviderKeyHint,
			RouteID:         target.RouteID,
			PublicName:      target.PublicName,
			Model:           target.UpstreamModel,
			Mode:            mode,
			Status:          "queued",
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
	if len(providerIDs) == 0 || len(providerIDs) > manualHealthCheckMaxProviders {
		return nil, fmt.Errorf("select between 1 and %d providers", manualHealthCheckMaxProviders)
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
		return nil, errors.New("select at least one provider")
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := m.app.db.QueryContext(ctx, `SELECT id,name,type,enabled,health_check_enabled FROM providers WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[int64]healthCheckTarget, len(ids))
	for rows.Next() {
		var target healthCheckTarget
		var providerType string
		var enabled, healthCheckEnabled int
		if err := rows.Scan(&target.ProviderID, &target.ProviderName, &providerType, &enabled, &healthCheckEnabled); err != nil {
			return nil, err
		}
		if !validProviderType(providerType) {
			return nil, errors.New("one or more providers do not support health checks")
		}
		if !strBool(enabled) {
			return nil, errors.New("disabled providers cannot be health checked")
		}
		if !strBool(healthCheckEnabled) {
			return nil, errors.New("health checks are disabled for one or more providers")
		}
		found[target.ProviderID] = target
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) != len(ids) {
		return nil, errors.New("one or more providers no longer exist")
	}
	targets := make([]healthCheckTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, found[id])
	}
	return targets, nil
}

func (m *healthCheckJobManager) loadModelTargets(ctx context.Context, providers []healthCheckTarget, routeIDs, providerKeyIDs []int64, scope string) ([]healthCheckTarget, error) {
	if scope != "all" && scope != "selected" {
		return nil, errors.New("model_scope must be all or selected")
	}
	providerNames := make(map[int64]string, len(providers))
	providerIDs := make([]int64, 0, len(providers))
	for _, provider := range providers {
		providerNames[provider.ProviderID] = provider.ProviderName
		providerIDs = append(providerIDs, provider.ProviderID)
	}
	args := make([]any, 0, len(providerIDs)+len(routeIDs))
	providerPlaceholders := strings.TrimRight(strings.Repeat("?,", len(providerIDs)), ",")
	for _, id := range providerIDs {
		args = append(args, id)
	}
	query := `SELECT id,provider_id,public_name,upstream_model,capabilities FROM model_routes WHERE enabled=1 AND capabilities LIKE '%chat%' AND provider_id IN (` + providerPlaceholders + `)`
	if scope == "selected" {
		if len(routeIDs) == 0 || len(routeIDs) > manualHealthCheckMaxItems {
			return nil, fmt.Errorf("select between 1 and %d models", manualHealthCheckMaxItems)
		}
		seen := make(map[int64]bool, len(routeIDs))
		unique := routeIDs[:0]
		for _, id := range routeIDs {
			if id < 1 {
				return nil, errors.New("route_ids must contain positive integers")
			}
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}
		routeIDs = unique
		query += ` AND id IN (` + strings.TrimRight(strings.Repeat("?,", len(routeIDs)), ",") + `)`
		for _, id := range routeIDs {
			args = append(args, id)
		}
	}
	query += ` ORDER BY provider_id,sort_order,id`
	rows, err := m.app.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]healthCheckTarget, 0)
	for rows.Next() {
		var target healthCheckTarget
		if err := rows.Scan(&target.RouteID, &target.ProviderID, &target.PublicName, &target.UpstreamModel, &target.Capabilities); err != nil {
			return nil, err
		}
		target.ProviderName = providerNames[target.ProviderID]
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if scope == "selected" && len(targets) != len(routeIDs) {
		return nil, errors.New("one or more selected models are disabled, missing, or do not belong to the selected providers")
	}
	if len(targets) == 0 {
		return nil, errors.New("the selected providers have no enabled models")
	}
	if len(targets) > manualHealthCheckMaxItems {
		return nil, fmt.Errorf("health check expands to more than %d models", manualHealthCheckMaxItems)
	}
	allowed := make(map[int64]bool, len(providerKeyIDs))
	matched := make(map[int64]bool, len(providerKeyIDs))
	for _, id := range providerKeyIDs {
		if id < 1 {
			return nil, errors.New("provider_key_ids must contain positive integers")
		}
		allowed[id] = true
	}
	expanded := make([]healthCheckTarget, 0, len(targets))
	for _, target := range targets {
		var authKind string
		if err := m.app.db.QueryRowContext(ctx, `SELECT auth_kind FROM providers WHERE id=?`, target.ProviderID).Scan(&authKind); err != nil {
			return nil, err
		}
		if authKind != "api_key" {
			expanded = append(expanded, target)
			continue
		}
		rows, err := m.app.db.QueryContext(ctx, `SELECT k.id,k.name,k.key_hint FROM provider_api_keys k WHERE k.provider_id=? AND k.enabled=1 AND k.health_check_enabled=1 AND (k.model<>'' AND lower(k.model)=lower(?) OR k.model='' AND EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=k.id AND km.enabled=1 AND lower(km.model)=lower(?)) OR k.model='' AND NOT EXISTS(SELECT 1 FROM provider_api_key_models km WHERE km.provider_key_id=k.id)) ORDER BY k.sort_order,k.id`, target.ProviderID, target.UpstreamModel, target.UpstreamModel)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			item := target
			if err := rows.Scan(&item.ProviderKeyID, &item.ProviderKeyName, &item.ProviderKeyHint); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if len(allowed) == 0 || allowed[item.ProviderKeyID] {
				expanded = append(expanded, item)
				matched[item.ProviderKeyID] = true
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if len(allowed) > 0 && len(matched) != len(allowed) {
		return nil, errors.New("one or more selected keys are disabled, missing, or do not support the selected models")
	}
	targets = expanded
	if len(targets) == 0 {
		return nil, errors.New("the selected providers have no enabled key/model combinations")
	}
	if len(targets) > manualHealthCheckMaxItems {
		return nil, fmt.Errorf("health check expands to more than %d key/model combinations", manualHealthCheckMaxItems)
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
			ProviderKeyID: item.ProviderKeyID, ProviderKeyName: item.ProviderKeyName, ProviderKeyHint: item.ProviderKeyHint,
			RouteID: item.RouteID, PublicName: item.PublicName, Model: item.Model,
			Status: "cancelled", Error: "health check cancelled",
		})
		return
	case <-timer.C:
	}

	probeCtx, cancel := context.WithTimeout(ctx, manualGenerationTimeout)
	var result healthCheckResult
	var err error
	target := healthCheckTarget{ProviderID: item.ProviderID, ProviderName: item.ProviderName, ProviderKeyID: item.ProviderKeyID, ProviderKeyName: item.ProviderKeyName, ProviderKeyHint: item.ProviderKeyHint, RouteID: item.RouteID, PublicName: item.PublicName, UpstreamModel: item.Model}
	if loadErr := m.app.db.QueryRowContext(probeCtx, `SELECT capabilities FROM model_routes WHERE id=? AND provider_id=?`, item.RouteID, item.ProviderID).Scan(&target.Capabilities); loadErr != nil {
		err = errors.New("model route no longer exists")
	} else {
		checker := NewHealthChecker(m.app, 15*time.Minute, 1)
		result = checker.probeRoute(probeCtx, target)
		if item.ProviderKeyID > 0 {
			if persistErr := m.app.persistProviderKeyModelHealth(probeCtx, item.ProviderID, item.ProviderKeyID, item.Model, result); persistErr != nil {
				err = persistErr
			}
		} else {
			checker.updateRouteHealthStatus(item.RouteID, result)
		}
	}
	cancel()
	finished := healthCheckItemResult{
		ProviderID: item.ProviderID, ProviderName: item.ProviderName,
		ProviderKeyID: item.ProviderKeyID, ProviderKeyName: item.ProviderKeyName, ProviderKeyHint: item.ProviderKeyHint,
		RouteID: item.RouteID, PublicName: item.PublicName,
		Status: result.Status, Mode: mode, LatencyMS: result.LatencyMS,
		FirstByteMS: result.FirstByteMS, Model: result.Model,
		ModelCount: result.ModelCount, Error: result.Error,
	}
	if ctx.Err() != nil {
		finished.Status = "cancelled"
		finished.Error = "health check cancelled"
	} else if err != nil {
		finished.Status = "config_error"
		finished.Error = sanitizeError(err.Error())
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
