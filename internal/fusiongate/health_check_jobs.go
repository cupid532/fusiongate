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
	// SkipReason is set when the route is kept in the job only to be reported
	// as skipped: no enabled API key can serve this upstream model. Empty for
	// targets that will actually be probed.
	SkipReason string
}

// Reasons a route or key cannot be probed. They are user-facing (the console
// translates the exact strings), so change them together with
// web/src/lib/health-check-messages.ts.
const (
	reasonNoHealthKeys     = "no enabled API key has health checks turned on"
	reasonNoSelectedKeys   = "none of the selected API keys are enabled for health checks"
	reasonModelDisabled    = "this model is switched off on every API key; enable it in model management"
	reasonModelUnlisted    = "no API key lists this model; discover models for a key or add it in model management"
	reasonKeyDisabled      = "API key is disabled"
	reasonKeyHealthOff     = "health checks are off for this API key"
	reasonKeyModelOff      = "model is switched off on this API key"
	reasonKeyModelUnlisted = "this API key does not list the model"
	reasonNotChat          = "only chat models support generation health checks"
)

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
		if target.SkipReason != "" {
			// Reported, never probed: the job would otherwise have failed as a
			// whole with one opaque error the moment any route lacked a key.
			job.Results[i].Status = "skipped"
			job.Results[i].Error = target.SkipReason
			job.Results[i].FinishedAt = job.CreatedAt
			job.Completed++
			job.Skipped++
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
		keyTargets, reason, err := m.routeKeyTargets(ctx, target, allowed)
		if err != nil {
			return nil, err
		}
		if len(keyTargets) == 0 {
			skipped := target
			skipped.SkipReason = reason
			expanded = append(expanded, skipped)
			continue
		}
		for _, item := range keyTargets {
			matched[item.ProviderKeyID] = true
		}
		expanded = append(expanded, keyTargets...)
	}
	if len(allowed) > 0 && len(matched) != len(allowed) {
		return nil, errors.New("one or more selected keys are disabled, missing, or do not support the selected models")
	}
	if len(expanded) > manualHealthCheckMaxItems {
		return nil, fmt.Errorf("health check expands to more than %d key/model combinations", manualHealthCheckMaxItems)
	}
	return expanded, nil
}

// routeKeyTargets expands one enabled route into per-key probe targets.
//
// OAuth providers have no key cards, so the route itself is the single target.
// For API-key providers it returns one target per enabled, health-check-enabled
// key that can serve the upstream model (optionally narrowed to `allowed`).
// When no key qualifies it returns no targets and a reason that names the
// actual cause — the model switched off on every key, the model missing from
// every key's inventory, or no key having health checks on — so the caller can
// report the route as skipped instead of failing the whole job.
func (m *healthCheckJobManager) routeKeyTargets(ctx context.Context, target healthCheckTarget, allowed map[int64]bool) ([]healthCheckTarget, string, error) {
	var authKind string
	if err := m.app.db.QueryRowContext(ctx, `SELECT auth_kind FROM providers WHERE id=?`, target.ProviderID).Scan(&authKind); err != nil {
		return nil, "", err
	}
	if authKind != "api_key" {
		return []healthCheckTarget{target}, "", nil
	}
	rows, err := m.app.db.QueryContext(ctx, `SELECT k.id,k.name,k.key_hint,k.model,k.model_policy,k.model_allowlist,p.default_model FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.provider_id=? AND k.enabled=1 AND k.health_check_enabled=1 ORDER BY k.sort_order,k.id`, target.ProviderID)
	if err != nil {
		return nil, "", err
	}
	type keyCandidate struct {
		id                                                 int64
		name, hint, model, policy, allowlist, defaultModel string
	}
	candidates := make([]keyCandidate, 0)
	for rows.Next() {
		var c keyCandidate
		if err := rows.Scan(&c.id, &c.name, &c.hint, &c.model, &c.policy, &c.allowlist, &c.defaultModel); err != nil {
			rows.Close()
			return nil, "", err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return nil, reasonNoHealthKeys, nil
	}
	out := make([]healthCheckTarget, 0, len(candidates))
	considered := 0
	disabledSomewhere := false
	for _, c := range candidates {
		if len(allowed) > 0 && !allowed[c.id] {
			continue
		}
		considered++
		inventory, exclusions, err := m.app.providerKeyModelSets(ctx, c.id)
		if err != nil {
			return nil, "", err
		}
		if !providerKeySupportsModel(c.policy, c.allowlist, c.model, c.defaultModel, target.UpstreamModel, inventory, exclusions) {
			if enabled, listed := inventory[normalizeProviderKeyModel(target.UpstreamModel)]; listed && !enabled {
				disabledSomewhere = true
			}
			continue
		}
		item := target
		item.ProviderKeyID, item.ProviderKeyName, item.ProviderKeyHint = c.id, c.name, c.hint
		out = append(out, item)
	}
	if len(out) > 0 {
		return out, "", nil
	}
	switch {
	case considered == 0:
		return nil, reasonNoSelectedKeys, nil
	case disabledSomewhere:
		return nil, reasonModelDisabled, nil
	default:
		return nil, reasonModelUnlisted, nil
	}
}

type healthCheckKeyPreview struct {
	KeyID              int64  `json:"key_id"`
	Name               string `json:"name"`
	Hint               string `json:"hint"`
	Enabled            bool   `json:"enabled"`
	HealthCheckEnabled bool   `json:"health_check_enabled"`
	Supported          bool   `json:"supported"`
	Reason             string `json:"reason,omitempty"`
}

type healthCheckRoutePreview struct {
	RouteID       int64                   `json:"route_id"`
	PublicName    string                  `json:"public_name"`
	UpstreamModel string                  `json:"upstream_model"`
	Capabilities  string                  `json:"capabilities"`
	Supported     bool                    `json:"supported"`
	Reason        string                  `json:"reason,omitempty"`
	Keys          []healthCheckKeyPreview `json:"keys"`
}

// healthCheckPreview is what the console shows before starting a manual check:
// every enabled route of the provider, which keys can probe it, and — for the
// ones nothing can probe — why. It mirrors loadModelTargets exactly, so what the
// dialog offers is what the job will do.
type healthCheckPreview struct {
	ProviderID         int64                     `json:"provider_id"`
	ProviderName       string                    `json:"provider_name"`
	AuthKind           string                    `json:"auth_kind"`
	Enabled            bool                      `json:"enabled"`
	HealthCheckEnabled bool                      `json:"health_check_enabled"`
	Probeable          int                       `json:"probeable"`
	Routes             []healthCheckRoutePreview `json:"routes"`
}

func (m *healthCheckJobManager) Preview(ctx context.Context, providerID int64) (healthCheckPreview, error) {
	out := healthCheckPreview{Routes: []healthCheckRoutePreview{}}
	var providerType string
	var enabled, healthEnabled int
	if err := m.app.db.QueryRowContext(ctx, `SELECT id,name,type,auth_kind,enabled,health_check_enabled FROM providers WHERE id=?`, providerID).Scan(&out.ProviderID, &out.ProviderName, &providerType, &out.AuthKind, &enabled, &healthEnabled); err != nil {
		return out, err
	}
	out.Enabled, out.HealthCheckEnabled = strBool(enabled), strBool(healthEnabled)

	type keyRow struct {
		id                                   int64
		name, hint, model, policy, allowlist string
		defaultModel                         string
		enabled, healthEnabled               bool
		inventory, exclusions                map[string]bool
	}
	keys := []keyRow{}
	if out.AuthKind == "api_key" {
		rows, err := m.app.db.QueryContext(ctx, `SELECT k.id,k.name,k.key_hint,k.model,k.model_policy,k.model_allowlist,k.enabled,k.health_check_enabled,p.default_model FROM provider_api_keys k JOIN providers p ON p.id=k.provider_id WHERE k.provider_id=? ORDER BY k.sort_order,k.id`, providerID)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var k keyRow
			var en, hc int
			if err := rows.Scan(&k.id, &k.name, &k.hint, &k.model, &k.policy, &k.allowlist, &en, &hc, &k.defaultModel); err != nil {
				rows.Close()
				return out, err
			}
			k.enabled, k.healthEnabled = strBool(en), strBool(hc)
			keys = append(keys, k)
		}
		if err := rows.Close(); err != nil {
			return out, err
		}
		for i := range keys {
			inventory, exclusions, err := m.app.providerKeyModelSets(ctx, keys[i].id)
			if err != nil {
				return out, err
			}
			keys[i].inventory, keys[i].exclusions = inventory, exclusions
		}
	}

	rows, err := m.app.db.QueryContext(ctx, `SELECT id,public_name,upstream_model,capabilities FROM model_routes WHERE provider_id=? AND enabled=1 ORDER BY sort_order,id`, providerID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		route := healthCheckRoutePreview{Keys: []healthCheckKeyPreview{}}
		if err := rows.Scan(&route.RouteID, &route.PublicName, &route.UpstreamModel, &route.Capabilities); err != nil {
			return out, err
		}
		if !strings.Contains(route.Capabilities, "chat") {
			route.Reason = reasonNotChat
			out.Routes = append(out.Routes, route)
			continue
		}
		if out.AuthKind != "api_key" {
			route.Supported = true
			out.Probeable++
			out.Routes = append(out.Routes, route)
			continue
		}
		healthKeys := 0
		disabledSomewhere := false
		for _, k := range keys {
			kp := healthCheckKeyPreview{KeyID: k.id, Name: k.name, Hint: k.hint, Enabled: k.enabled, HealthCheckEnabled: k.healthEnabled}
			supports := providerKeySupportsModel(k.policy, k.allowlist, k.model, k.defaultModel, route.UpstreamModel, k.inventory, k.exclusions)
			switch {
			case !k.enabled:
				kp.Reason = reasonKeyDisabled
			case !k.healthEnabled:
				kp.Reason = reasonKeyHealthOff
			case !supports:
				if enabled, listed := k.inventory[normalizeProviderKeyModel(route.UpstreamModel)]; listed && !enabled {
					kp.Reason = reasonKeyModelOff
					disabledSomewhere = true
				} else {
					kp.Reason = reasonKeyModelUnlisted
				}
			default:
				kp.Supported = true
			}
			if k.enabled && k.healthEnabled {
				healthKeys++
			}
			if kp.Supported {
				route.Supported = true
				out.Probeable++
			}
			route.Keys = append(route.Keys, kp)
		}
		if !route.Supported {
			switch {
			case healthKeys == 0:
				route.Reason = reasonNoHealthKeys
			case disabledSomewhere:
				route.Reason = reasonModelDisabled
			default:
				route.Reason = reasonModelUnlisted
			}
		}
		out.Routes = append(out.Routes, route)
	}
	return out, rows.Err()
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
	if item.Status != "queued" {
		m.mu.Unlock()
		return
	}
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
