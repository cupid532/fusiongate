package fusiongate

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Two kinds of state are deliberately kept apart.
//
//   - Global fault state, stored here, decides whether a channel, Key or
//     model×protocol scope may accept new requests.
//   - Task forward state, stored in routing_sessions, decides whether one task
//     has already left a channel.
//
// Background recovery and manual health checks change only the first kind. They
// must never move a task cursor back, which is why nothing in this file writes
// to routing_sessions.
const (
	recoveryScopeProvider = "provider"
	recoveryScopeKey      = "key"
	recoveryScopeRoute    = "route"

	recoveryStatusLocked   = "locked"
	recoveryStatusHalfOpen = "half_open"
	recoveryStatusHealthy  = "healthy"

	// Stable successes needed before a scope drops its backoff level entirely.
	recoveryStableSuccesses = 2
	// Default ceiling on paid generation probes per provider per day.
	defaultGenerationProbeDailyCap = 100
)

// recoveryBackoff is the approved 5s/30s/60s/120s/300s ladder. The last entry
// repeats for every further level.
var recoveryBackoff = []time.Duration{5 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

func recoveryDelay(level int) time.Duration {
	if level <= 0 {
		return recoveryBackoff[0]
	}
	if level > len(recoveryBackoff) {
		level = len(recoveryBackoff)
	}
	return recoveryBackoff[level-1]
}

// recoveryFault is one isolated failure scope and its recovery progress.
type recoveryFault struct {
	Scope         string `json:"scope"`
	Kind          string `json:"kind"`
	ProviderID    int64  `json:"provider_id"`
	ProviderKeyID int64  `json:"provider_key_id"`
	RouteID       int64  `json:"route_id"`
	Model         string `json:"model"`
	Protocol      string `json:"protocol"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	Level         int    `json:"level"`
	Successes     int    `json:"successes"`
	Probes        int    `json:"probes"`
	NextProbeAt   string `json:"next_probe_at"`
	LastProbeAt   string `json:"last_probe_at"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func providerFaultScope(providerID int64) string {
	return fmt.Sprintf("provider:%d", providerID)
}

func keyFaultScope(providerKeyID int64) string {
	return fmt.Sprintf("key:%d", providerKeyID)
}

// routeFaultScope is deliberately model- and protocol-specific: a site whose
// Chat path for one model is broken must not have its unrelated Responses
// traffic disabled.
func routeFaultScope(providerID int64, model, protocol string) string {
	return fmt.Sprintf("route:%d:%s:%s", providerID, strings.ToLower(strings.TrimSpace(model)), protocol)
}

// faultScopeFor classifies one failed attempt into exactly one isolation scope.
// A credential rejection belongs to that Key, not to the whole site; a transport
// failure belongs to the site.
func faultScopeFor(z resolvedRoute, protocol, model string, status int, reason string) (scope, kind string) {
	authFailure := status == http.StatusUnauthorized || status == http.StatusForbidden ||
		reason == "upstream_auth_error" || reason == "auth_expired"
	if authFailure && z.ProviderKeyID > 0 {
		return keyFaultScope(z.ProviderKeyID), recoveryScopeKey
	}
	// An endpoint that does not exist is a model×protocol fact.
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented {
		return routeFaultScope(z.Provider.ID, model, protocol), recoveryScopeRoute
	}
	return providerFaultScope(z.Provider.ID), recoveryScopeProvider
}

type recoveryScheduler struct {
	app      *App
	mu       sync.Mutex
	inflight map[string]struct{}
	slots    chan struct{}
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
	workers  sync.WaitGroup
}

func newRecoveryScheduler(a *App) *recoveryScheduler {
	return &recoveryScheduler{
		app:      a,
		inflight: map[string]struct{}{},
		slots:    make(chan struct{}, defaultRecoveryProbeConcurrency),
		done:     make(chan struct{}),
	}
}

const defaultRecoveryProbeConcurrency = 2

func (a *App) migrateRecovery(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS recovery_faults (
  scope TEXT PRIMARY KEY,
  kind TEXT NOT NULL DEFAULT '',
  provider_id INTEGER NOT NULL DEFAULT 0,
  provider_key_id INTEGER NOT NULL DEFAULT 0,
  route_id INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'locked',
  reason TEXT NOT NULL DEFAULT '',
  level INTEGER NOT NULL DEFAULT 1,
  successes INTEGER NOT NULL DEFAULT 0,
  probes INTEGER NOT NULL DEFAULT 0,
  next_probe_at TEXT NOT NULL DEFAULT '',
  last_probe_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS recovery_faults_due ON recovery_faults(next_probe_at);
CREATE INDEX IF NOT EXISTS recovery_faults_provider ON recovery_faults(provider_id);`)
	return err
}

func (s *recoveryScheduler) start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	inner, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	go s.run(inner)
}

func (s *recoveryScheduler) stop() {
	s.mu.Lock()
	cancel := s.cancel
	started := s.started
	s.started = false
	s.mu.Unlock()
	if started && cancel != nil {
		cancel()
		<-s.done
		s.workers.Wait()
	}
}

func (s *recoveryScheduler) run(ctx context.Context) {
	ticker := time.NewTicker(recoveryTickInterval())
	defer ticker.Stop()
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

const defaultRecoveryTick = 5 * time.Second

func recoveryTickInterval() time.Duration { return defaultRecoveryTick }

// tick walks the due fault scopes. Each scope is probed at most once at a time
// (probeOnce), and the whole queue is bounded by a concurrency limit so a large
// outage cannot turn recovery into a traffic storm against the upstreams.
func (s *recoveryScheduler) tick(ctx context.Context) {
	due, err := s.dueFaults(ctx)
	if err != nil || len(due) == 0 {
		return
	}
	for _, fault := range due {
		if ctx.Err() != nil {
			return
		}
		if !s.probeOnce(fault.Scope) {
			continue
		}
		select {
		case s.slots <- struct{}{}:
		case <-ctx.Done():
			s.releaseProbe(fault.Scope)
			return
		}
		s.workers.Add(1)
		go func(f recoveryFault) {
			defer s.workers.Done()
			defer func() { <-s.slots; s.releaseProbe(f.Scope) }()
			s.probe(ctx, f)
		}(fault)
	}
}

func (s *recoveryScheduler) dueFaults(ctx context.Context) ([]recoveryFault, error) {
	rows, err := s.app.reader().QueryContext(ctx, `
SELECT scope,kind,provider_id,provider_key_id,route_id,model,protocol,status,reason,level,successes,probes,
       next_probe_at,last_probe_at,created_at,updated_at
FROM recovery_faults
WHERE status<>? AND (next_probe_at='' OR next_probe_at<=?)
ORDER BY next_probe_at,scope LIMIT 32`, recoveryStatusHealthy, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recoveryFault{}
	for rows.Next() {
		var f recoveryFault
		if rows.Scan(&f.Scope, &f.Kind, &f.ProviderID, &f.ProviderKeyID, &f.RouteID, &f.Model, &f.Protocol,
			&f.Status, &f.Reason, &f.Level, &f.Successes, &f.Probes, &f.NextProbeAt, &f.LastProbeAt,
			&f.CreatedAt, &f.UpdatedAt) != nil {
			continue
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *recoveryScheduler) probeOnce(scope string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inflight[scope]; busy {
		return false
	}
	s.inflight[scope] = struct{}{}
	return true
}

func (s *recoveryScheduler) releaseProbe(scope string) {
	s.mu.Lock()
	delete(s.inflight, scope)
	s.mu.Unlock()
}

// probe runs the cheapest check that can justify restoring a scope.
//
// Only a generation probe can prove the generation path is back. A directory
// listing proves reachability and nothing more, so a connectivity result moves
// the scope to half-open and lets the next real request of a new task settle it.
func (s *recoveryScheduler) probe(ctx context.Context, fault recoveryFault) {
	probeCtx, cancel := context.WithTimeout(ctx, recoveryProbeTimeout())
	defer cancel()
	started := time.Now()
	eventual := s.app.generationProbeAllowed(ctx, fault)
	if eventual {
		result := s.app.healthCheckerProbe(probeCtx, fault.ProviderID)
		s.app.noteRecoveryProbe(probeCtx, fault, result == "healthy" || result == "reachable", true)
		return
	}
	reachable := s.app.probeReachability(probeCtx, fault.ProviderID)
	s.app.log.Debug("recovery probe", "scope", fault.Scope, "reachable", reachable, "elapsed_ms", time.Since(started).Milliseconds())
	s.app.noteRecoveryProbe(probeCtx, fault, reachable, false)
}

const defaultRecoveryProbeTimeout = 20 * time.Second

func recoveryProbeTimeout() time.Duration { return defaultRecoveryProbeTimeout }

// healthCheckerProbe runs the existing generation health check for one provider.
// It is only reached when the operator opted this provider in.
func (a *App) healthCheckerProbe(ctx context.Context, providerID int64) string {
	if a.healthChecker == nil {
		return "config_error"
	}
	if !a.beginHealthProbe(providerID) {
		return "in_progress"
	}
	defer a.endHealthProbe(providerID)
	result := a.healthChecker.probeProvider(ctx, providerID)
	return result.Status
}

// probeReachability performs the free, non-generating check.
func (a *App) probeReachability(ctx context.Context, providerID int64) bool {
	p, err := a.loadDiscoveryProvider(ctx, providerID)
	if err != nil {
		return false
	}
	if !p.HealthCheckEnabled {
		// The operator turned health checks off for this provider. That setting
		// also covers background recovery probing.
		return false
	}
	_, err = a.fetchDiscoveredModels(ctx, p)
	return err == nil
}

// generationProbeAllowed reports whether a paid generation probe may be sent for
// this scope. Automatic generation probing is off unless the provider explicitly
// enables it, and it stops at the daily cap.
func (a *App) generationProbeAllowed(ctx context.Context, fault recoveryFault) bool {
	if fault.ProviderID <= 0 {
		return false
	}
	var enabled, cap int
	var stamp string
	if err := a.reader().QueryRowContext(ctx, `
SELECT auto_recovery_probe,COALESCE(recovery_probe_daily_cap,0),COALESCE(recovery_probe_day,'')
FROM providers WHERE id=?`, fault.ProviderID).Scan(&enabled, &cap, &stamp); err != nil {
		return false
	}
	if enabled == 0 {
		return false
	}
	if cap <= 0 {
		cap = defaultGenerationProbeDailyCap
	}
	today := time.Now().UTC().Format("2006-01-02")
	if stamp != today {
		return true
	}
	var used int
	if err := a.reader().QueryRowContext(ctx, `SELECT COALESCE(SUM(probes),0) FROM recovery_probes WHERE provider_id=? AND day=?`, fault.ProviderID, today).Scan(&used); err != nil {
		return false
	}
	return used < cap
}

// noteRecoveryProbe applies the result of one probe.
//
// A probe that could not prove generation puts the scope in half-open so a real
// request may try it, instead of declaring a recovery the probe did not observe.
func (a *App) noteRecoveryProbe(ctx context.Context, fault recoveryFault, proved bool, generating bool) {
	stamp := now()
	status := recoveryStatusHalfOpen
	probes := fault.Probes
	if generating {
		probes++
		a.recordRecoveryProbe(ctx, fault.ProviderID, generating)
	}
	level := fault.Level
	next := time.Now().Add(recoveryDelay(level)).UTC().Format(time.RFC3339Nano)
	if proved {
		// Keep the level for now: a scope that fails again shortly after a
		// single success must not restart the ladder from five seconds.
		_, _ = a.db.ExecContext(ctx, `UPDATE recovery_faults SET status=?,probes=?,last_probe_at=?,next_probe_at=?,updated_at=? WHERE scope=?`,
			status, probes, stamp, next, stamp, fault.Scope)
		return
	}
	level++
	if level > len(recoveryBackoff) {
		level = len(recoveryBackoff)
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE recovery_faults SET status=?,probes=?,level=?,last_probe_at=?,next_probe_at=?,updated_at=? WHERE scope=?`,
		recoveryStatusLocked, probes, level, stamp, time.Now().Add(recoveryDelay(level)).UTC().Format(time.RFC3339Nano), stamp, fault.Scope)
}

func (a *App) recordRecoveryProbe(ctx context.Context, providerID int64, generating bool) {
	if !generating || providerID <= 0 {
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	_, _ = a.db.ExecContext(ctx, `INSERT INTO recovery_probes(provider_id,day,probes,updated_at) VALUES(?,?,1,?)
ON CONFLICT(provider_id,day) DO UPDATE SET probes=recovery_probes.probes+1,updated_at=excluded.updated_at`, providerID, day, now())
}

// recordRecoveryFault escalates a scope when a real request fails. Concurrent
// requests share one row, so several simultaneous failures do not multiply the
// backoff: the level advances once per distinct failure event.
func (a *App) recordRecoveryFault(ctx context.Context, scope, kind, reason string, z resolvedRoute, model, protocol string) {
	if scope == "" {
		return
	}
	stamp := now()
	_, err := a.db.ExecContext(ctx, `
INSERT INTO recovery_faults(scope,kind,provider_id,provider_key_id,route_id,model,protocol,status,reason,level,successes,probes,next_probe_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,1,0,0,?,?,?)
ON CONFLICT(scope) DO UPDATE SET
  status=?,reason=excluded.reason,
  provider_key_id=excluded.provider_key_id,route_id=excluded.route_id,
  model=excluded.model,protocol=excluded.protocol,
  level=CASE WHEN recovery_faults.status='healthy' THEN 1
             WHEN recovery_faults.level<5 THEN recovery_faults.level+1 ELSE 5 END,
  successes=0,
  next_probe_at=excluded.next_probe_at,
  updated_at=excluded.updated_at`,
		scope, kind, z.Provider.ID, z.ProviderKeyID, z.Route.ID, model, protocol, recoveryStatusLocked, reason,
		time.Now().Add(recoveryDelay(1)).UTC().Format(time.RFC3339Nano), stamp, stamp,
		recoveryStatusLocked, reason, time.Now().Add(recoveryDelay(1)).UTC().Format(time.RFC3339Nano), stamp)
	if err != nil {
		a.log.Error("record recovery fault", "scope", scope, "error", err)
	}
}

// noteRecoverySuccess clears a scope only after sustained success. One good
// request right after a failure keeps the backoff level so a flapping upstream
// does not reset the ladder and get hammered.
func (a *App) noteRecoverySuccess(ctx context.Context, scope string) {
	if scope == "" {
		return
	}
	var exists bool
	if a.reader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recovery_faults WHERE scope=? AND status<>?)`, scope, recoveryStatusHealthy).Scan(&exists) != nil || !exists {
		return
	}
	stamp := now()
	_, _ = a.db.ExecContext(ctx, `
UPDATE recovery_faults SET
  successes=successes+1,
  status=CASE WHEN successes+1>=? THEN ? ELSE ? END,
  level=CASE WHEN successes+1>=? THEN 0 ELSE level END,
  next_probe_at=CASE WHEN successes+1>=? THEN '' ELSE next_probe_at END,
  updated_at=?
WHERE scope=? AND status<>?`,
		recoveryStableSuccesses, recoveryStatusHealthy, recoveryStatusHalfOpen,
		recoveryStableSuccesses, recoveryStableSuccesses, stamp, scope, recoveryStatusHealthy)
}

// manualRecovery lets an operator or a manual health check settle a scope from
// the same state machine. It still cannot restore a scope that the probe did not
// prove: the scope is only moved to half-open.
func (a *App) manualRecovery(ctx context.Context, scope string) error {
	if scope == "" {
		return fmt.Errorf("recovery scope is required")
	}
	result, err := a.db.ExecContext(ctx, `UPDATE recovery_faults SET status=?,successes=0,next_probe_at=?,updated_at=? WHERE scope=?`,
		recoveryStatusHalfOpen, time.Now().UTC().Format(time.RFC3339Nano), now(), scope)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("no recovery record for scope %q", scope)
	}
	return nil
}

// forgetRecoveryScope drops a fault entirely. Callers use it only when the
// underlying configuration changed, so the old evidence no longer applies.
func (a *App) forgetRecoveryScope(ctx context.Context, scope string) {
	if scope == "" {
		return
	}
	_, _ = a.db.ExecContext(ctx, `DELETE FROM recovery_faults WHERE scope=?`, scope)
}

func (a *App) recoveryFaults(ctx context.Context, limit int) ([]recoveryFault, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := a.reader().QueryContext(ctx, `
SELECT scope,kind,provider_id,provider_key_id,route_id,model,protocol,status,reason,level,successes,probes,
       next_probe_at,last_probe_at,created_at,updated_at
FROM recovery_faults WHERE status<>? ORDER BY updated_at DESC LIMIT ?`, recoveryStatusHealthy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recoveryFault{}
	for rows.Next() {
		var f recoveryFault
		if rows.Scan(&f.Scope, &f.Kind, &f.ProviderID, &f.ProviderKeyID, &f.RouteID, &f.Model, &f.Protocol,
			&f.Status, &f.Reason, &f.Level, &f.Successes, &f.Probes, &f.NextProbeAt, &f.LastProbeAt,
			&f.CreatedAt, &f.UpdatedAt) != nil {
			continue
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (a *App) migrateRecoveryProbes(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS recovery_probes (
  provider_id INTEGER NOT NULL,
  day TEXT NOT NULL,
  probes INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider_id,day));`)
	return err
}

// pruneRecoveryProbes bounds the paid-probe counters. The cap is per day, so
// anything older than a week can never be read again.
func (a *App) pruneRecoveryProbes(ctx context.Context) error {
	cutoff := time.Now().AddDate(0, 0, -7).UTC().Format("2006-01-02")
	_, err := a.db.ExecContext(ctx, `DELETE FROM recovery_probes WHERE day < ?`, cutoff)
	return err
}

// recoveryScopeArgs parses the console's scope query parameter.
func recoveryScopeArgs(r *http.Request) (string, error) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		return "", fmt.Errorf("scope is required")
	}
	if !strings.HasPrefix(scope, "provider:") && !strings.HasPrefix(scope, "key:") && !strings.HasPrefix(scope, "route:") {
		return "", fmt.Errorf("scope must start with provider:, key: or route:")
	}
	return scope, nil
}

func recoveryLevelLabel(level int) string {
	if level <= 0 {
		return "0"
	}
	return strconv.Itoa(level) + "/" + strconv.Itoa(len(recoveryBackoff))
}
