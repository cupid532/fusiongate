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
			if strings.Contains(ua, "claude-code") || strings.Contains(ua, "claude code") || strings.Contains(ua, "claude-cli") {
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

func schedulingModel(routes []resolvedRoute) string {
	if len(routes) == 0 {
		return ""
	}
	if model := strings.TrimSpace(routes[0].CanonicalModel); model != "" {
		return model
	}
	return routes[0].Route.PublicName
}

// prepareRoutes builds a deterministic request-local failover plan. Priority mode
// sorts by provider priority descending, provider position, route priority
// descending, route position, and route ID. Provider priority and position remain
// authoritative; route priority is the next mapping-level precedence.
// Ordered mode always starts from the first configured channel. Smart round robin
// advances the starting channel for every new request while retaining request-local
// seamless failover.
func (a *App) prepareRoutes(routes []resolvedRoute, strategy RoutingStrategy) []resolvedRoute {
	planned := append([]resolvedRoute(nil), routes...)
	sort.SliceStable(planned, func(i, j int) bool {
		if strategy == StrategyPriorityFailover && planned[i].Provider.Priority != planned[j].Provider.Priority {
			return planned[i].Provider.Priority > planned[j].Provider.Priority
		}
		if planned[i].Provider.SortOrder != planned[j].Provider.SortOrder {
			return planned[i].Provider.SortOrder < planned[j].Provider.SortOrder
		}
		if strategy == StrategyPriorityFailover && planned[i].Route.Priority != planned[j].Route.Priority {
			return planned[i].Route.Priority > planned[j].Route.Priority
		}
		if planned[i].Route.SortOrder != planned[j].Route.SortOrder {
			return planned[i].Route.SortOrder < planned[j].Route.SortOrder
		}
		return planned[i].Route.ID < planned[j].Route.ID
	})
	if strategy != StrategySmartRoundRobin || len(planned) < 2 {
		return planned
	}
	// Rotate channels, not resolved key routes. A provider with several API keys must
	// not receive proportionally more first attempts merely because it expands to
	// several candidates. Keep every provider's key routes together so request-local
	// failover can still try the remaining keys before moving to the next channel.
	groups := make([][]resolvedRoute, 0, len(planned))
	groupIndex := make(map[int64]int, len(planned))
	for _, route := range planned {
		index, ok := groupIndex[route.Provider.ID]
		if !ok {
			index = len(groups)
			groupIndex[route.Provider.ID] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], route)
	}
	if len(groups) < 2 {
		return planned
	}
	model := schedulingModel(planned)
	a.routeMu.Lock()
	start := a.roundRobinCursor[model] % len(groups)
	a.roundRobinCursor[model] = (start + 1) % len(groups)
	a.routeMu.Unlock()
	rotated := make([]resolvedRoute, 0, len(planned))
	for offset := range groups {
		rotated = append(rotated, groups[(start+offset)%len(groups)]...)
	}
	return rotated
}

func (a *App) routeSelectableLocked(z resolvedRoute, state *providerRuntime, nowTime time.Time, availability *routeAvailability) bool {
	// An auth_expired provider is NOT benched forever here. A 401/403 already
	// opens the circuit immediately (see completeRoute), so the circuit check
	// below throttles retries while the credential is broken, and the half-open
	// probe rediscovers the provider once its token is refreshed. A permanent
	// exclusion would require an external status rewrite to ever recover.
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

func providerHealthFactor(p Provider) float64 {
	factor := 1.0
	switch strings.ToLower(strings.TrimSpace(p.Status)) {
	case "auth_expired":
		factor = 0.05
	case "rate_limited":
		factor = 0.15
	case "degraded":
		factor = 0.55
	}
	switch strings.ToLower(strings.TrimSpace(p.HealthCheckStatus)) {
	case "config_error", "unreachable", "failed":
		factor *= 0.35
	case "pending":
		factor *= 0.9
	case "reachable":
		// A confirmed-reachable provider must never score below one whose
		// reachability has simply never been probed.
		factor *= 1.0
	default:
		factor *= 0.95
	}
	return factor
}

// adaptiveMinFailureFactor keeps a repeatedly failing provider heavily
// deprioritized without driving its score to zero. Adaptive selection still has to
// be able to retry such a provider once its cooldown elapses, otherwise a
// recovered upstream would be starved indefinitely by its healthy peers.
const adaptiveMinFailureFactor = 0.05

// adaptiveWeight scores one provider for smooth weighted selection. Interactive
// clients care much more about time-to-first-byte than about total response
// duration, which is also inflated by output length, so first-byte EWMA is
// preferred and total latency is only a fallback until a streaming observation
// exists for this provider.
func adaptiveWeight(p Provider, state *providerRuntime) float64 {
	weight := float64(p.Weight)
	if weight <= 0 {
		weight = 1
	}
	observedLatency := state.EWMAFirstByteMS
	if observedLatency <= 0 {
		observedLatency = state.EWMALatencyMS
	}
	latencyFactor := 1.0
	if observedLatency > 0 {
		latencyFactor = math.Max(0.06, 1500.0/(1500.0+observedLatency))
	}
	failureFactor := math.Max(adaptiveMinFailureFactor, math.Pow(0.55, float64(state.ConsecutiveFailures)))
	loadFactor := 1.0 / float64(state.Inflight+1)
	return weight * latencyFactor * failureFactor * loadFactor * providerHealthFactor(p)
}

// smoothWeightsForLocked returns the smooth weighted round-robin accumulator for one
// public model. The accumulator is scoped per model because the algorithm only
// distributes traffic correctly while the candidate set it runs over stays stable;
// a single global accumulator shared by every model lets one model's rotation
// permanently bias another's.
func (a *App) smoothWeightsForLocked(model string) map[int64]float64 {
	weights := a.smoothWeights[model]
	if weights == nil {
		weights = map[int64]float64{}
		a.smoothWeights[model] = weights
	}
	return weights
}

// forgetRouteCursorsLocked drops all rotation state for a public model so a renamed,
// deleted, or reconfigured model starts from a clean distribution. Callers must hold
// routeMu.
func (a *App) forgetRouteCursorsLocked(model string) {
	delete(a.roundRobinCursor, model)
	delete(a.smoothWeights, model)
}

// resetRouteCursorsLocked clears rotation state for every model. Callers must hold
// routeMu.
func (a *App) resetRouteCursorsLocked() {
	a.roundRobinCursor = map[string]int{}
	a.smoothWeights = map[string]map[int64]float64{}
}

func reserveRouteLocked(z resolvedRoute, state *providerRuntime) resolvedRoute {
	state.Inflight++
	if !state.CircuitOpenUntil.IsZero() {
		state.HalfOpenProbe = true
	}
	return z
}

func (a *App) acquireQualityDetectorRoute(route resolvedRoute) (resolvedRoute, routeAvailability, bool) {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	state := a.stateForLocked(route.Provider)
	if route.Provider.MaxConcurrency > 0 && state.Inflight >= route.Provider.MaxConcurrency {
		return resolvedRoute{}, routeAvailability{Reason: "provider_saturated"}, false
	}
	return reserveRouteLocked(route, state), routeAvailability{}, true
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

	// Adaptive selection scores one entry per provider. A provider that exposes
	// several API keys still resolves to several routes, and accumulating its weight
	// once per route would make multi-key providers win the rotation purely because
	// they were counted more often than single-key peers.
	type adaptiveCandidate struct {
		route resolvedRoute
		state *providerRuntime
	}
	candidates := make(map[int64]adaptiveCandidate, len(routes))
	order := make([]int64, 0, len(routes))
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
		// routeSelectableLocked already rejected circuits that are still open, so a
		// non-zero CircuitOpenUntil here means the cooldown has elapsed and this
		// route is the pending half-open probe. Send it immediately instead of
		// waiting for its penalized score to beat a healthy peer, otherwise a
		// recovered upstream is never rediscovered under adaptive routing.
		if !state.CircuitOpenUntil.IsZero() {
			return reserveRouteLocked(z, state), routeAvailability{}, true
		}
		if _, seen := candidates[z.Provider.ID]; seen {
			continue
		}
		candidates[z.Provider.ID] = adaptiveCandidate{route: z, state: state}
		order = append(order, z.Provider.ID)
	}
	if len(order) == 0 {
		return resolvedRoute{}, availability, false
	}

	weights := a.smoothWeightsForLocked(schedulingModel(routes))
	var selected adaptiveCandidate
	selectedID := int64(0)
	best := -math.MaxFloat64
	total := 0.0
	// `order` already follows the request-local plan order, so a strict comparison
	// keeps ties resolving to the highest-ranked provider deterministically.
	for _, id := range order {
		candidate := candidates[id]
		effective := adaptiveWeight(candidate.route.Provider, candidate.state)
		weights[id] += effective
		total += effective
		if weights[id] > best {
			best = weights[id]
			selected = candidate
			selectedID = id
		}
	}
	weights[selectedID] -= total
	// Drop accumulator entries for providers that no longer serve this model so the
	// map cannot grow without bound as routes are reconfigured.
	if len(weights) > len(order) {
		for id := range weights {
			if _, live := candidates[id]; !live {
				delete(weights, id)
			}
		}
	}
	return reserveRouteLocked(selected.route, selected.state), routeAvailability{}, true
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
	keyFailure := z.ProviderKeyID > 0 && isProviderFailure(result)
	if keyFailure {
		cooldown := time.Duration(z.Provider.CooldownSeconds) * time.Second
		if cooldown <= 0 {
			cooldown = 30 * time.Second
		}
		if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden || result.Status == http.StatusTooManyRequests {
			if cooldown < 5*time.Minute {
				cooldown = 5 * time.Minute
			}
		}
		if result.RetryAfter > cooldown {
			cooldown = result.RetryAfter
		}
		if cooldown > 10*time.Minute {
			cooldown = 10 * time.Minute
		}
		openUntil := time.Now().Add(cooldown)
		a.providerKeyCooldowns[z.ProviderKeyID] = openUntil
		a.routeMu.Unlock()
		message := result.Reason
		if message == "" && result.Err != nil {
			message = result.Err.Error()
		}
		_, err := a.db.Exec(`UPDATE provider_api_keys SET status=?,last_error=?,cooldown_until=?,updated_at=? WHERE id=?`, providerStatus(result), sanitizeError(message), openUntil.UTC().Format(time.RFC3339Nano), now(), z.ProviderKeyID)
		if err != nil {
			a.log.Error("provider key health update", "provider_key_id", z.ProviderKeyID, "error", err)
		}
		return
	}
	if z.ProviderKeyID > 0 && !isProviderFailure(result) {
		delete(a.providerKeyCooldowns, z.ProviderKeyID)
		_, _ = a.db.Exec(`UPDATE provider_api_keys SET status='healthy',last_error='',cooldown_until=NULL,updated_at=? WHERE id=?`, now(), z.ProviderKeyID)
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
			threshold = DefaultFailureThreshold
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
			if cooldown > 10*time.Minute {
				cooldown = 10 * time.Minute
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

// resetProviderKeyRuntime makes an edited, re-enabled, successfully tested, or
// deleted key immediately forget any stale attributable-failure cooldown.
func (a *App) resetProviderKeyRuntime(id int64) {
	a.routeMu.Lock()
	delete(a.providerKeyCooldowns, id)
	a.routeMu.Unlock()
	_, _ = a.db.Exec(`UPDATE provider_api_keys SET cooldown_until=NULL WHERE id=?`, id)
}

func (a *App) resetProviderKeysRuntime(ids []int64) {
	a.routeMu.Lock()
	for _, id := range ids {
		delete(a.providerKeyCooldowns, id)
	}
	a.routeMu.Unlock()
	for _, id := range ids {
		_, _ = a.db.Exec(`UPDATE provider_api_keys SET cooldown_until=NULL WHERE id=?`, id)
	}
}

func (a *App) providerKeyIDs(providerID int64) []int64 {
	rows, err := a.reader().Query(`SELECT id FROM provider_api_keys WHERE provider_id=?`, providerID)
	if err != nil {
		a.log.Error("list provider keys for runtime reset", "provider_id", providerID, "error", err)
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
