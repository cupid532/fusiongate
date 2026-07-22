package fusiongate

import (
	"context"
	"fmt"
	"math"
	"net/http"
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
	state = &providerRuntime{ConsecutiveFailures: p.ConsecutiveFailures, EWMALatencyMS: float64(p.LastLatencyMS)}
	if t := parseTime(p.CircuitOpenUntil); t != nil {
		state.CircuitOpenUntil = *t
	}
	a.providerStates[p.ID] = state
	return state
}

// acquireRoute implements priority-tier failover plus health-adjusted smooth weighted
// round robin inside a tier. Saturated and open-circuit providers are excluded.
func (a *App) acquireRoute(routes []resolvedRoute, tried map[int64]bool) (resolvedRoute, routeAvailability, bool) {
	nowTime := time.Now()
	a.routeMu.Lock()
	defer a.routeMu.Unlock()

	tierRoute, tierProvider := math.MaxInt, math.MaxInt
	var retryAfter time.Duration
	reason := "no_eligible_route"
	for _, z := range routes {
		if tried[z.Route.ID] {
			continue
		}
		state := a.stateForLocked(z.Provider)
		if state.CircuitOpenUntil.After(nowTime) {
			wait := time.Until(state.CircuitOpenUntil)
			if retryAfter == 0 || wait < retryAfter {
				retryAfter = wait
			}
			reason = "circuit_open"
			continue
		}
		if !state.CircuitOpenUntil.IsZero() && state.HalfOpenProbe {
			reason = "half_open_probe_inflight"
			continue
		}
		if z.Provider.MaxConcurrency > 0 && state.Inflight >= z.Provider.MaxConcurrency {
			reason = "provider_saturated"
			continue
		}
		if z.Route.Priority < tierRoute || (z.Route.Priority == tierRoute && z.Provider.Priority < tierProvider) {
			tierRoute, tierProvider = z.Route.Priority, z.Provider.Priority
		}
	}
	if tierRoute == math.MaxInt {
		return resolvedRoute{}, routeAvailability{RetryAfter: retryAfter, Reason: reason}, false
	}

	var selected resolvedRoute
	var selectedState *providerRuntime
	best := -math.MaxFloat64
	total := 0.0
	for _, z := range routes {
		if tried[z.Route.ID] || z.Route.Priority != tierRoute || z.Provider.Priority != tierProvider {
			continue
		}
		state := a.stateForLocked(z.Provider)
		if state.CircuitOpenUntil.After(nowTime) || (!state.CircuitOpenUntil.IsZero() && state.HalfOpenProbe) {
			continue
		}
		if z.Provider.MaxConcurrency > 0 && state.Inflight >= z.Provider.MaxConcurrency {
			continue
		}
		weight := float64(z.Provider.Weight)
		if weight <= 0 {
			weight = 1
		}
		// Prefer low-latency and low-inflight providers without starving slower peers.
		latencyFactor := 1.0
		if state.EWMALatencyMS > 0 {
			latencyFactor = math.Max(0.25, 1000.0/(1000.0+state.EWMALatencyMS))
		}
		failureFactor := math.Pow(0.65, float64(state.ConsecutiveFailures))
		effective := weight * latencyFactor * failureFactor / float64(state.Inflight+1)
		state.Current += effective
		total += effective
		if state.Current > best {
			best = state.Current
			selected = z
			selectedState = state
		}
	}
	if selectedState == nil {
		return resolvedRoute{}, routeAvailability{RetryAfter: retryAfter, Reason: reason}, false
	}
	selectedState.Current -= total
	selectedState.Inflight++
	if !selectedState.CircuitOpenUntil.IsZero() {
		selectedState.HalfOpenProbe = true
	}
	return selected, routeAvailability{}, true
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

func (a *App) completeRoute(z resolvedRoute, result attemptResult, latency time.Duration) {
	a.routeMu.Lock()
	state := a.stateForLocked(z.Provider)
	if state.Inflight > 0 {
		state.Inflight--
	}
	state.HalfOpenProbe = false
	latencyMS := float64(latency.Milliseconds())
	if latencyMS < 1 {
		latencyMS = 1
	}
	if state.EWMALatencyMS == 0 {
		state.EWMALatencyMS = latencyMS
	} else {
		state.EWMALatencyMS = state.EWMALatencyMS*0.8 + latencyMS*0.2
	}
	if isNeutralResult(result) {
		a.routeMu.Unlock()
		return
	}

	status := providerStatus(result)
	lastError := ""
	lastSuccessAt := ""
	lastFailureAt := ""
	openUntil := ""
	if isProviderFailure(result) {
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
			(result.Status == http.StatusTooManyRequests && result.RetryAfter > 0)
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
			status = "circuit_open"
		}
	} else {
		state.ConsecutiveFailures = 0
		state.CircuitOpenUntil = time.Time{}
		lastSuccessAt = now()
	}
	failures := state.ConsecutiveFailures
	ewma := int64(state.EWMALatencyMS)
	if openUntil == "" && !state.CircuitOpenUntil.IsZero() {
		openUntil = state.CircuitOpenUntil.UTC().Format(time.RFC3339Nano)
	}
	a.routeMu.Unlock()

	_, err := a.db.Exec(`UPDATE providers SET status=?,consecutive_failures=?,circuit_open_until=?,last_error=?,last_latency_ms=?,last_success_at=CASE WHEN ?='' THEN last_success_at ELSE ? END,last_failure_at=CASE WHEN ?='' THEN last_failure_at ELSE ? END,updated_at=? WHERE id=?`, status, failures, nullableTime(openUntil), lastError, ewma, lastSuccessAt, lastSuccessAt, lastFailureAt, lastFailureAt, now(), z.Provider.ID)
	if err != nil {
		a.log.Error("provider health update", "provider_id", z.Provider.ID, "error", err)
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
