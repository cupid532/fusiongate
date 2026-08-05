package fusiongate

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type providerRuntime struct {
	Current             float64
	Inflight            int
	ConsecutiveFailures int
	CircuitOpenUntil    time.Time
	HalfOpenProbe       bool
	EWMALatencyMS       float64
	EWMAFirstByteMS     float64
}

type attemptResult struct {
	Status     int
	Usage      Usage
	Handled    bool
	Retryable  bool
	Reason     string
	Err        error
	RetryAfter time.Duration
}

type routeAvailability struct {
	RetryAfter time.Duration
	Reason     string
}

func validPassthroughMode(v string) bool {
	return v == "normalized" || v == "transparent"
}

func validClientPolicy(v string) bool {
	return v == "any" || v == "codex" || v == "claude_code"
}

// filterClientRoutes never fabricates a client identity. It only routes requests whose
// real incoming User-Agent already matches a provider's declared client policy.
func filterClientRoutes(routes []resolvedRoute, r *http.Request) []resolvedRoute {
	ua := strings.ToLower(r.UserAgent())
	out := make([]resolvedRoute, 0, len(routes))
	for _, z := range routes {
		switch z.Provider.ClientPolicy {
		case "", "any":
			out = append(out, z)
		case "codex":
			if strings.Contains(ua, "codex") {
				out = append(out, z)
			}
		case "claude_code":
			if strings.Contains(ua, "claude-code") || strings.Contains(ua, "claude code") {
				out = append(out, z)
			}
		}
	}
	return out
}

func (a *App) stateForLocked(p Provider) *providerRuntime {
	state := a.providerStates[p.ID]
	if state != nil {
		return state
	}
	state = &providerRuntime{
		ConsecutiveFailures: p.ConsecutiveFailures,
		EWMALatencyMS:       float64(p.LastLatencyMS),
		EWMAFirstByteMS:     float64(p.LastFirstByteMS),
	}
	if t := parseTime(p.CircuitOpenUntil); t != nil {
		state.CircuitOpenUntil = *t
	}
	a.providerStates[p.ID] = state
	return state
}

type RoutingStrategy string

const (
	StrategyPriorityFailover  RoutingStrategy = "priority_failover"
	StrategyOrderedRoundRobin RoutingStrategy = "ordered_round_robin"
	StrategySmartRoundRobin   RoutingStrategy = "smart_round_robin"
	StrategyAdaptive          RoutingStrategy = "adaptive"
)

func validRoutingStrategy(v string) bool {
	switch RoutingStrategy(v) {
	case StrategyPriorityFailover, StrategyOrderedRoundRobin, StrategySmartRoundRobin, StrategyAdaptive:
		return true
	}
	return false
}

func routeStrategy(routes []resolvedRoute) RoutingStrategy {
	if len(routes) > 0 && validRoutingStrategy(routes[0].Route.Strategy) {
		return RoutingStrategy(routes[0].Route.Strategy)
	}
	return StrategyPriorityFailover
}

// prepareRoutes builds a deterministic request-local failover plan. Priority mode
// sorts channels by provider priority from high to low, then by configured position.
// Ordered mode always starts from the first configured channel. Smart round robin advances the starting channel
// for every new request while retaining request-local seamless failover.
func (a *App) prepareRoutes(routes []resolvedRoute, strategy RoutingStrategy) []resolvedRoute {
	planned := append([]resolvedRoute(nil), routes...)
	sort.SliceStable(planned, func(i, j int) bool {
		if strategy == StrategyPriorityFailover && planned[i].Provider.Priority != planned[j].Provider.Priority {
			return planned[i].Provider.Priority > planned[j].Provider.Priority
		}
		if planned[i].Provider.SortOrder != planned[j].Provider.SortOrder {
			return planned[i].Provider.SortOrder < planned[j].Provider.SortOrder
		}
		if planned[i].Route.SortOrder != planned[j].Route.SortOrder {
			return planned[i].Route.SortOrder < planned[j].Route.SortOrder
		}
		return planned[i].Route.ID < planned[j].Route.ID
	})
	if strategy != StrategySmartRoundRobin || len(planned) < 2 {
		return planned
	}
	model := planned[0].Route.PublicName
	a.routeMu.Lock()
	start := a.roundRobinCursor[model] % len(planned)
	a.roundRobinCursor[model] = (start + 1) % len(planned)
	a.routeMu.Unlock()
	return append(append([]resolvedRoute(nil), planned[start:]...), planned[:start]...)
}

func (a *App) routeSelectableLocked(z resolvedRoute, state *providerRuntime, nowTime time.Time, availability *routeAvailability) bool {
	if z.ProviderKeyID > 0 {
		if openUntil := a.providerKeyCooldowns[z.ProviderKeyID]; openUntil.After(nowTime) {
			wait := time.Until(openUntil)
			if availability.RetryAfter == 0 || wait < availability.RetryAfter {
				availability.RetryAfter = wait
			}
			availability.Reason = "provider_key_cooldown"
			return false
		}
		delete(a.providerKeyCooldowns, z.ProviderKeyID)
	}
	if state.CircuitOpenUntil.After(nowTime) {
		wait := time.Until(state.CircuitOpenUntil)
		if availability.RetryAfter == 0 || wait < availability.RetryAfter {
			availability.RetryAfter = wait
		}
		availability.Reason = "circuit_open"
		return false
	}
	if !state.CircuitOpenUntil.IsZero() && state.HalfOpenProbe {
		availability.Reason = "half_open_probe_inflight"
		return false
	}
	if z.Provider.MaxConcurrency > 0 && state.Inflight >= z.Provider.MaxConcurrency {
		availability.Reason = "provider_saturated"
		return false
	}
	return true
}

func reserveRouteLocked(z resolvedRoute, state *providerRuntime) resolvedRoute {
	state.Inflight++
	if !state.CircuitOpenUntil.IsZero() {
		state.HalfOpenProbe = true
	}
	return z
}

// acquireRoute selects one route from a request-local plan while excluding routes
// already attempted by this request and providers that are saturated or circuit-open.
func (a *App) acquireRoute(routes []resolvedRoute, tried map[int64]bool, strategy RoutingStrategy) (resolvedRoute, routeAvailability, bool) {
	nowTime := time.Now()
	a.routeMu.Lock()
	defer a.routeMu.Unlock()

	availability := routeAvailability{Reason: "no_eligible_route"}
	if strategy != StrategyAdaptive {
		for _, z := range routes {
			attemptID := z.AttemptID
			if attemptID == 0 {
				attemptID = z.Route.ID
			}
			if tried[attemptID] {
				continue
			}
			state := a.stateForLocked(z.Provider)
			if !a.routeSelectableLocked(z, state, nowTime, &availability) {
				continue
			}
			return reserveRouteLocked(z, state), routeAvailability{}, true
		}
		return resolvedRoute{}, availability, false
	}

	var selected resolvedRoute
	var selectedState *providerRuntime
	best := -math.MaxFloat64
	total := 0.0
	for _, z := range routes {
		attemptID := z.AttemptID
		if attemptID == 0 {
			attemptID = z.Route.ID
		}
		if tried[attemptID] {
			continue
		}
		state := a.stateForLocked(z.Provider)
		if !a.routeSelectableLocked(z, state, nowTime, &availability) {
			continue
		}
		weight := float64(z.Provider.Weight)
		if weight <= 0 {
			weight = 1
		}
		// Interactive clients care much more about time-to-first-byte than the
		// total duration of a response, which is also affected by output length.
		// Fall back to total latency until a successful streaming observation is
		// available for this provider.
		observedLatency := state.EWMAFirstByteMS
		if observedLatency <= 0 {
			observedLatency = state.EWMALatencyMS
		}
		latencyFactor := 1.0
		if observedLatency > 0 {
			latencyFactor = math.Max(0.06, 1500.0/(1500.0+observedLatency))
		}
		failureFactor := math.Pow(0.55, float64(state.ConsecutiveFailures))
		loadFactor := 1.0 / float64(state.Inflight+1)
		effective := weight * latencyFactor * failureFactor * loadFactor
		state.Current += effective
		total += effective
		if state.Current > best || (state.Current == best && (selectedState == nil || z.Provider.SortOrder < selected.Provider.SortOrder || (z.Provider.SortOrder == selected.Provider.SortOrder && z.Route.SortOrder < selected.Route.SortOrder))) {
			best = state.Current
			selected = z
			selectedState = state
		}
	}
	if selectedState == nil {
		return resolvedRoute{}, availability, false
	}
	selectedState.Current -= total
	return reserveRouteLocked(selected, selectedState), routeAvailability{}, true
}

func isNeutralResult(result attemptResult) bool {
	switch result.Reason {
	case "route_configuration_error", "protocol_not_supported", "invalid_request", "downstream_write_error", "downstream_canceled", "upstream_route_not_found":
		return true
	}
	return false
}

func isProviderFailure(result attemptResult) bool {
	if isNeutralResult(result) {
		return false
	}
	if result.Err != nil {
		return true
	}
	switch result.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return result.Status >= 500
}

func providerStatus(result attemptResult) string {
	if !isProviderFailure(result) {
		return "healthy"
	}
	switch result.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth_expired"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if result.Err != nil || result.Status >= 500 || result.Status == http.StatusRequestTimeout {
		return "degraded"
	}
	return "healthy"
}

func (a *App) completeRoute(z resolvedRoute, result attemptResult, latency time.Duration, firstByte ...time.Duration) {
	a.routeMu.Lock()
	state := a.stateForLocked(z.Provider)
	if state.Inflight > 0 {
		state.Inflight--
	}
	state.HalfOpenProbe = false
	keyFailure := z.ProviderKeyID > 0 && (result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden || result.Status == http.StatusTooManyRequests)
	if keyFailure {
		cooldown := 5 * time.Minute
		if result.RetryAfter > cooldown {
			cooldown = result.RetryAfter
		}
		a.providerKeyCooldowns[z.ProviderKeyID] = time.Now().Add(cooldown)
		a.routeMu.Unlock()
		_, err := a.db.Exec(`UPDATE provider_api_keys SET status=?,last_error=?,updated_at=? WHERE id=?`, providerStatus(result), result.Reason, now(), z.ProviderKeyID)
		if err != nil {
			a.log.Error("provider key health update", "provider_key_id", z.ProviderKeyID, "error", err)
		}
		return
	}
	if z.ProviderKeyID > 0 && !isProviderFailure(result) {
		delete(a.providerKeyCooldowns, z.ProviderKeyID)
	}
	if isNeutralResult(result) {
		a.routeMu.Unlock()
		return
	}

	status := providerStatus(result)
	providerFailure := isProviderFailure(result)
	if !providerFailure {
		latencyMS := float64(latency.Milliseconds())
		if latencyMS < 1 {
			latencyMS = 1
		}
		if state.EWMALatencyMS == 0 {
			state.EWMALatencyMS = latencyMS
		} else {
			state.EWMALatencyMS = state.EWMALatencyMS*0.8 + latencyMS*0.2
		}
		if len(firstByte) > 0 && firstByte[0] > 0 {
			firstByteMS := float64(firstByte[0].Milliseconds())
			if firstByteMS < 1 {
				firstByteMS = 1
			}
			if state.EWMAFirstByteMS == 0 {
				state.EWMAFirstByteMS = firstByteMS
			} else {
				state.EWMAFirstByteMS = state.EWMAFirstByteMS*0.75 + firstByteMS*0.25
			}
		}
	}
	lastError := ""
	lastSuccessAt := ""
	lastFailureAt := ""
	openUntil := ""
	if providerFailure {
		state.ConsecutiveFailures++
		lastFailureAt = now()
		lastError = result.Reason
		if lastError == "" && result.Err != nil {
			lastError = result.Err.Error()
		}
		threshold := z.Provider.FailureThreshold
		if threshold <= 0 {
			threshold = 3
		}
		immediate := result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden ||
			result.Status == http.StatusTooManyRequests
		if immediate || state.ConsecutiveFailures >= threshold {
			cooldown := time.Duration(z.Provider.CooldownSeconds) * time.Second
			if cooldown <= 0 {
				cooldown = 30 * time.Second
			}
			// Exponential cooldown avoids repeatedly hammering a persistently failing upstream.
			if excess := state.ConsecutiveFailures - threshold; excess > 0 {
				if excess > 4 {
					excess = 4
				}
				cooldown *= time.Duration(1 << excess)
			}
			if cooldown > 10*time.Minute {
				cooldown = 10 * time.Minute
			}
			if immediate && cooldown < 5*time.Minute {
				cooldown = 5 * time.Minute
			}
			if result.RetryAfter > cooldown {
				cooldown = result.RetryAfter
			}
			state.CircuitOpenUntil = time.Now().Add(cooldown)
			openUntil = state.CircuitOpenUntil.UTC().Format(time.RFC3339Nano)
			switch result.Status {
			case http.StatusTooManyRequests:
				// Keep the reason visible in the console while CircuitOpenUntil
				// prevents this account from being selected during cooldown.
				status = "rate_limited"
			case http.StatusUnauthorized, http.StatusForbidden:
				status = "auth_expired"
			default:
				status = "circuit_open"
			}
		}
	} else {
		state.ConsecutiveFailures = 0
		state.CircuitOpenUntil = time.Time{}
		lastSuccessAt = now()
	}
	failures := state.ConsecutiveFailures
	ewma := int64(state.EWMALatencyMS)
	ewmaFirstByte := int64(state.EWMAFirstByteMS)
	if openUntil == "" && !state.CircuitOpenUntil.IsZero() {
		openUntil = state.CircuitOpenUntil.UTC().Format(time.RFC3339Nano)
	}
	a.routeMu.Unlock()

	_, err := a.db.Exec(`UPDATE providers SET status=?,consecutive_failures=?,circuit_open_until=?,last_error=?,last_latency_ms=?,last_first_byte_ms=?,last_success_at=CASE WHEN ?='' THEN last_success_at ELSE ? END,last_failure_at=CASE WHEN ?='' THEN last_failure_at ELSE ? END,updated_at=? WHERE id=?`, status, failures, nullableTime(openUntil), lastError, ewma, ewmaFirstByte, lastSuccessAt, lastSuccessAt, lastFailureAt, lastFailureAt, now(), z.Provider.ID)
	if err != nil {
		a.log.Error("provider health update", "provider_id", z.Provider.ID, "error", err)
	}
}

const circuitRecoveryInterval = 30 * time.Second

func (a *App) runCircuitRecoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(circuitRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.probeOpenCircuits(ctx)
		}
	}
}

func (a *App) probeOpenCircuits(parent context.Context) {
	rows, err := a.db.QueryContext(parent, `SELECT id FROM providers WHERE enabled=1 AND circuit_open_until IS NOT NULL ORDER BY id LIMIT 100`)
	if err != nil {
		a.log.Error("circuit recovery query", "error", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		if parent.Err() != nil || !a.beginHealthProbe(id) {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		result := a.healthChecker.probeProviderMode(ctx, id, healthCheckModeConnectivity)
		cancel()
		a.endHealthProbe(id)
		a.healthChecker.updateHealthStatus(id, result)
		if result.Status != "healthy" {
			continue
		}
		a.resetProviderRuntime(id)
		if _, err := a.db.ExecContext(parent, `UPDATE providers SET status='healthy',consecutive_failures=0,circuit_open_until=NULL,last_error='',last_success_at=?,updated_at=? WHERE id=? AND enabled=1`, now(), now(), id); err != nil {
			a.log.Error("circuit recovery update", "provider_id", id, "error", err)
		}
	}
}

func nullableTime(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (a *App) providerInflight(id int64) int {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	if state := a.providerStates[id]; state != nil {
		return state.Inflight
	}
	return 0
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

func retryReason(status int, err error) string {
	if err != nil {
		if err == context.DeadlineExceeded || strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return "upstream_timeout"
		}
		return "upstream_transport_error"
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "upstream_auth_error"
	case http.StatusNotFound:
		return "upstream_route_not_found"
	case http.StatusRequestTimeout:
		return "upstream_timeout"
	case http.StatusTooManyRequests:
		return "upstream_rate_limited"
	}
	if status >= 500 {
		return "upstream_server_error"
	}
	return "upstream_error"
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}

func routeUnavailableMessage(av routeAvailability) string {
	if av.Reason == "provider_saturated" {
		return "all eligible providers are at their concurrency limit"
	}
	if av.Reason == "circuit_open" || av.Reason == "half_open_probe_inflight" {
		return "all eligible provider circuits are open"
	}
	return fmt.Sprintf("no available provider route (%s)", av.Reason)
}

func (a *App) resetProviderRuntime(id int64) {
	a.routeMu.Lock()
	delete(a.providerStates, id)
	a.routeMu.Unlock()
}
