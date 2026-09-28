package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// inference is the single public inference entry point. It replaces the V3.12
// passthrough handler.
//
// The request is sent to a channel exactly as the client wrote it. What may
// differ per channel is the credential, the account headers, the wire path the
// upstream really exposes, and — only when an operator configured it explicitly
// — the protocol conversion. Nothing about the payload is rewritten by default.
//
// Failover follows one order: provider priority descending, then the configured
// position, then the mapping position. A channel gets a small attempt budget
// shared by all of its Keys; only after that budget is spent does the request
// move to the next channel. The channel a task advanced to is remembered, so the
// next turn of the same task resumes there instead of walking back up the order.
func (a *App) inference(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		failRequest(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/images/") && !key.AllowImages {
		failRequest(w, r, http.StatusForbidden, "images_not_allowed", "images not allowed")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/audio/") && !key.AllowAudio {
		failRequest(w, r, http.StatusForbidden, "audio_not_allowed", "audio not allowed")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPassthroughBody+1))
	if err != nil {
		failBodyError(w, r, err)
		return
	}
	if len(raw) > maxPassthroughBody {
		failBodyError(w, r, errRequestBodyTooLarge)
		return
	}
	model, stream, err := passthroughModel(raw, r.Header.Get("Content-Type"))
	if err != nil || model == "" {
		failRequest(w, r, http.StatusBadRequest, "invalid_request", "model is required and must be readable")
		return
	}
	if !allowed(key, model) {
		failRequest(w, r, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}

	path := r.URL.Path
	protocol := passthroughProtocol(path)
	settings, err := a.inferenceSettingsFor(r.Context())
	if err != nil {
		a.log.Error("inference settings", "error", err)
		settings = defaultInferenceSettings()
	}
	run := inferenceRun{
		app:       a,
		key:       key,
		gatewayID: requestID(),
		model:     model,
		path:      path,
		protocol:  protocol,
		stream:    stream,
		raw:       raw,
		clientIP:  requestClientIP(r),
		settings:  settings,
		reasoning: requestReasoningEffortFromRaw(raw),
		startedAt: time.Now(),
	}

	// Strict task stickiness needs a client-declared task identifier. Without one
	// request-level failover still works; only the cross-turn binding is absent.
	taskHash, taskSource, _ := inferenceTaskIdentity(r, key, a.keyMaterial)
	scope := inferenceTaskScopeFor(model, protocol)
	session, hasSession := routingSession{}, false
	if taskHash != "" {
		session, hasSession = a.loadRoutingSession(r.Context(), taskHash, scope)
	}

	routes, resolveErr := a.resolveRoutes(r.Context(), model, "inference", true)
	// Two different questions, two different answers: which channels can serve
	// this endpoint at all (a configuration or model question, reported as 404),
	// and which of those tolerate this client's real identity (reported as 403).
	supported := filterInferenceRoutes(routes, path)
	eligible := filterClientRoutes(supported, r)
	plan := buildInferencePlan(eligible, path, session, hasSession)
	run.supported = len(supported)
	run.plan = plan
	run.taskHash = taskHash
	run.taskScope = scope
	run.taskSource = taskSource
	run.session = session
	run.hasSession = hasSession

	defer func() {
		a.metrics.completed.Add(1)
		if run.success {
			a.metrics.successes.Add(1)
		} else {
			a.metrics.failures.Add(1)
		}
	}()

	if resolveErr != nil || len(plan.Channels) == 0 {
		run.count, run.exclusions = a.inferenceDiagnostics(r.Context(), model, path, routes, r)
		run.failBeforeUpstream(w, r, resolveErr)
		return
	}
	run.count, run.exclusions = a.inferenceDiagnostics(r.Context(), model, path, routes, r)
	run.execute(w, r)
}

// inferenceRun carries the state of one public inference request.
type inferenceRun struct {
	app        *App
	key        authKey
	gatewayID  string
	model      string
	path       string
	protocol   string
	stream     bool
	raw        []byte
	clientIP   string
	reasoning  string
	settings   inferenceSettings
	startedAt  time.Time
	plan       inferencePlan
	taskHash   string
	taskScope  string
	taskSource string
	count      int
	exclusions string
	session    routingSession
	hasSession bool
	supported  int

	attempts int
	notes    []string
	success  bool
	// committed records that a response has already been written downstream.
	// Once true, nothing may write again: a later channel cannot repair bytes the
	// client already received, and a synthetic gateway error appended to a real
	// upstream answer would corrupt it.
	committed  bool
	retryPause time.Duration
	lastID     string
	last       *http.Response
	lastErr    string
	lastCode   int
}

// noteSkip keeps the most informative reason a channel was passed over.
func (run *inferenceRun) noteSkip(reason string) {
	if reason == "" {
		return
	}
	for _, existing := range run.notes {
		if existing == reason {
			return
		}
	}
	run.notes = append(run.notes, reason)
}

// exclusionJSON merges the configuration exclusions with this request's runtime
// skips so the console can explain both in one field.
func (run *inferenceRun) exclusionJSON() string {
	if len(run.notes) == 0 {
		return run.exclusions
	}
	combined := decodeExclusions(run.exclusions)
	combined = append(combined, run.notes...)
	raw, err := json.Marshal(combined)
	if err != nil {
		return run.exclusions
	}
	return string(raw)
}

// failBeforeUpstream reports a request that never reached a channel.
func (run *inferenceRun) failBeforeUpstream(w http.ResponseWriter, r *http.Request, resolveErr error) {
	status, reason := http.StatusNotFound, "no_eligible_model_route"
	hint := "no eligible inference channel"
	switch {
	case errors.Is(resolveErr, errRouteResolution):
		status, reason, hint = http.StatusServiceUnavailable, "route_resolution_failed", "channel credentials could not be resolved"
	case resolveErr == nil && run.plan.Exhausted:
		status, reason, hint = http.StatusServiceUnavailable, "candidates_exhausted", "every channel this task knew about is gone"
	case resolveErr == nil && run.supported > 0:
		// Channels exist that this endpoint can serve, but none of them tolerates
		// this client's real identity. Reporting "not found" would hide the cause.
		status, reason, hint = http.StatusForbidden, "provider_client_policy_mismatch", "no channel accepts this request's real User-Agent"
	}
	z := resolvedRoute{Route: Route{PublicName: run.model, UpstreamModel: run.model}}
	started := time.Now()
	id := run.app.startLedger(run.key, z, run.protocol, run.stream, run.clientIP, run.gatewayID, run.reasoning, 0, "")
	run.annotate(id, reason)
	run.app.endLedger(id, 0, run.key.ID, "", run.model, false, status, reason, started, Usage{CostType: "unknown"})
	failRequest(w, r, status, reason, hint)
}

// execute walks the plan channel by channel.
//
// A channel is the unit of failover: it owns one shared attempt budget for all
// of its credentials, and the request only moves on once that budget is spent,
// the channel is demonstrably unusable for this request, or every one of its
// credentials has been isolated.
func (run *inferenceRun) execute(w http.ResponseWriter, r *http.Request) {
	a := run.app
	defer func() {
		if run.last != nil {
			run.last.Body.Close()
			run.last = nil
		}
	}()
	maxAttempts := run.settings.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultInferenceSettings().MaxAttempts
	}
	channelAttempts := run.settings.ChannelAttempts
	if channelAttempts <= 0 {
		channelAttempts = defaultInferenceSettings().ChannelAttempts
	}
	channelWindow := time.Duration(run.settings.ChannelWindowSeconds) * time.Second
	if channelWindow <= 0 {
		channelWindow = time.Duration(defaultInferenceSettings().ChannelWindowSeconds) * time.Second
	}
	deadline := run.startedAt.Add(time.Duration(run.settings.FailoverWindowSeconds) * time.Second)

	for index := run.plan.Start; index < len(run.plan.Channels); index++ {
		if r.Context().Err() != nil {
			run.stop(r, "downstream_canceled")
			return
		}
		channel := run.plan.Channels[index]
		keys := preferSessionKey(channel.Routes, run.session.ProviderKeyID)
		isolated := map[int64]bool{}
		// A Retry-After belongs to the channel that sent it: the next channel
		// must not inherit a pause it never asked for.
		run.retryPause = 0
		channelDeadline := time.Now().Add(channelWindow)
		budget := channelAttempts
		if len(keys) > budget {
			// Never grant a channel fewer attempts than it has usable Keys: an
			// isolated bad Key must be survivable inside the channel.
			budget = len(keys)
		}
		for attempt := 1; attempt <= budget; attempt++ {
			if r.Context().Err() != nil {
				run.stop(r, "downstream_canceled")
				return
			}
			if run.attempts >= maxAttempts {
				run.finish(w, r, "attempt_limit")
				return
			}
			if time.Now().After(deadline) {
				run.finish(w, r, "failover_window_exceeded")
				return
			}
			if attempt > 1 && !run.pause(r, attempt, channelDeadline) {
				break
			}
			z, availability, ok := a.acquireInferenceRoute(keys, isolated, attempt)
			if !ok {
				run.noteSkip(availability.Reason)
				break
			}
			run.attempts++
			a.metrics.attempts.Add(1)
			if run.attempts > 1 {
				a.metrics.failovers.Add(1)
			}
			started := time.Now()
			id := a.startLedger(run.key, z, run.protocol, run.stream, run.clientIP, run.gatewayID, run.reasoning, run.attempts, run.lastErr)
			run.lastID = id
			adapter := inferenceAdapterID(z, run.path)
			run.recordAttempt(id, z, adapter, run.attempts, index, attempt)

			var firstByte time.Duration
			result, cancel := run.attempt(w, r, z, adapter, func() {
				if firstByte == 0 {
					firstByte = time.Since(started)
				}
				a.recordFirstByte(id, started)
				a.metrics.firstByteCount.Add(1)
				a.metrics.firstByteMillis.Add(max(1, time.Since(started).Milliseconds()))
			})

			stop := "failover"
			terminal := false
			decision := decisionRetryChannel
			switch {
			case result.Handled:
				// Bytes are already committed downstream. Nothing may be retried,
				// and nothing may be written again afterwards.
				run.committed = true
				stop, terminal = "http_terminal", true
				if result.Err != nil {
					stop = "response_interrupted"
				}
			case result.Response != nil:
				if result.Retryable {
					if !run.retain(result) {
						// The error body exceeded the bounded buffer: forward the
						// original bytes rather than truncating the upstream answer.
						run.committed = true
						result.Err, result.Reason = passthroughResponse(w, result.Response)
						run.last = nil
						stop = "error_body_limit"
						if result.Err != nil {
							stop = "error_body_interrupted"
						}
						terminal = true
					} else {
						decision = classifyInferenceResult(result, false)
						stop = run.stopReasonFor(decision, result)
					}
				} else {
					// A deliberate upstream answer for this request. It is
					// forwarded verbatim, which ends the request.
					run.committed = true
					result.Err, result.Reason = passthroughResponse(w, result.Response)
					run.last = nil
					stop = "http_terminal"
					if result.Err != nil {
						stop = "response_interrupted"
					}
					terminal = true
				}
			default:
				decision = classifyInferenceResult(result, false)
				stop = run.stopReasonFor(decision, result)
			}
			cancel()
			if r.Context().Err() != nil {
				result.Err = r.Context().Err()
				result.Reason = "downstream_canceled"
				stop, terminal = "downstream_canceled", true
			}
			status := result.Status
			if status == 0 {
				status = http.StatusBadGateway
			}
			// The pause this attempt asked for belongs to this channel only.
			run.retryPause = result.RetryAfter
			run.lastCode = status
			reason := result.Reason
			if reason == "" && status >= 400 {
				reason = retryReason(status, result.Err)
			}
			result.Reason = reason
			run.lastErr = reason
			a.completeRoute(z, result, time.Since(started), firstByte)

			success := status < 400 && result.Err == nil
			// A pure passthrough never parses the upstream body, so its token
			// usage is genuinely unknown. Recording that explicitly keeps the
			// cost type honest instead of writing an empty string that reads as
			// "not accounted for yet".
			usage := result.Usage
			if usage.CostType == "" {
				usage.CostType = "unknown"
			}
			a.endLedger(id, z.Provider.ID, run.key.ID, z.Provider.Type, z.Route.UpstreamModel, success, status, reason, started, usage)
			run.annotate(id, stop)
			if success {
				run.success = true
				run.noteSuccess(r, z, index)
				run.finish(w, r, stop)
				return
			}
			if isProviderFailure(result) || result.Status == 0 {
				scope, kind := faultScopeFor(z, run.protocol, run.model, status, reason)
				a.recordRecoveryFault(r.Context(), scope, kind, reason, z, run.model, run.protocol)
				run.recordFault(id, scope)
			} else if z.Provider.ID > 0 {
				a.noteRecoverySuccess(r.Context(), providerFaultScope(z.Provider.ID))
			}
			if terminal {
				run.finish(w, r, stop)
				return
			}
			if decision == decisionReturnRaw && run.last != nil {
				// A client-side error the upstream answered deliberately. Hand
				// the real answer back instead of a synthesised gateway error.
				run.finish(w, r, "upstream_client_error")
				return
			}
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				// Isolate this credential for the rest of the request so the next
				// attempt inside the same channel uses a different Key.
				isolated[z.ProviderKeyID] = true
			}
			if decision == decisionAdvanceChannel {
				break
			}
			if time.Now().After(channelDeadline) {
				break
			}
		}
		// This channel is spent. Persist the forward move before touching the
		// next one so a concurrent turn of the same task cannot walk back.
		if next := index + 1; next < len(run.plan.Channels) {
			run.advance(r, next)
		}
	}
	stop := "candidates_exhausted"
	if run.attempts >= maxAttempts {
		stop = "attempt_limit"
	} else if time.Now().After(deadline) {
		stop = "failover_window_exceeded"
	}
	run.finish(w, r, stop)
}

// stopReasonFor names the decision in operator terms.
func (run *inferenceRun) stopReasonFor(decision inferenceDecision, result attemptResult) string {
	switch decision {
	case decisionReturnRaw:
		return "upstream_client_error"
	case decisionAdvanceChannel:
		return "endpoint_not_supported"
	case decisionTerminal:
		return "terminal"
	}
	if result.RetryAfter > 0 {
		return "rate_limited_retry"
	}
	return "failover"
}

// retain keeps the last retryable upstream error so the caller receives the
// upstream's own status, headers and body if no channel succeeds.
//
// The buffer is bounded. An error body larger than the bound is not truncated:
// retain reports false and the caller forwards the original bytes.
func (run *inferenceRun) retain(result attemptResult) bool {
	resp := result.Response
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPassthroughError+1))
	if readErr == nil && len(body) <= maxPassthroughError {
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if run.last != nil {
			run.last.Body.Close()
		}
		run.last = resp
		return true
	}
	resp.Body = &prefixedPassthroughBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), Closer: resp.Body}
	return false
}

// pause waits before the next attempt inside one channel. A Retry-After from the
// channel wins, but it is capped by the channel window: a long upstream pause is
// a reason to try a different channel, not to hold the client.
func (run *inferenceRun) pause(r *http.Request, attempt int, channelDeadline time.Time) bool {
	delay := inferenceRetryDelay(attempt)
	if wait := run.retryAfter(); wait > delay {
		delay = wait
	}
	if delay <= 0 {
		return true
	}
	remaining := time.Until(channelDeadline)
	if remaining <= 0 {
		return false
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return false
	case <-timer.C:
		return true
	}
}

// retryAfter reads the pause the last retryable answer asked for, capped at the
// same-channel wait limit.
func (run *inferenceRun) retryAfter() time.Duration {
	wait := run.retryPause
	if wait > maxInferenceRetryAfter {
		wait = maxInferenceRetryAfter
	}
	return wait
}

const maxInferenceRetryAfter = 30 * time.Second

// attempt performs one upstream call for one channel.
func (run *inferenceRun) attempt(w http.ResponseWriter, r *http.Request, z resolvedRoute, adapter string, onFirstByte func()) (attemptResult, context.CancelFunc) {
	a := run.app
	switch adapter {
	case "responses_to_chat":
		return a.compatibleResponsesProxy(w, r, run.raw, z, run.gatewayID, run.stream, true, onFirstByte), func() {}
	case "messages_to_chat":
		var body map[string]any
		if err := json.Unmarshal(run.raw, &body); err != nil {
			return attemptResult{Status: http.StatusBadRequest, Reason: "invalid_request", Err: err}, func() {}
		}
		return a.anthropicMessagesOpenAI(w, r, body, z, run.gatewayID, run.stream, onFirstByte), func() {}
	}
	return a.inferenceAttempt(r, run.raw, z, inferenceUpstreamPath(z, run.path), run.stream, onFirstByte)
}

// inferenceAttempt sends the client's own bytes to one channel.
//
// It differs from a bare passthrough in exactly three ways, all of which are
// required to keep an auth-file channel usable without touching the payload: the
// OAuth credential is refreshed when it is near expiry, the credential and
// account headers are applied for the channel's real type, and the wire path is
// the one that channel actually exposes.
func (a *App) inferenceAttempt(incoming *http.Request, raw []byte, z resolvedRoute, path string, stream bool, onFirstByte func()) (attemptResult, context.CancelFunc) {
	ctx, cancelContext := context.WithCancel(incoming.Context())
	start := time.Duration(z.Provider.RequestTimeoutMS) * time.Millisecond
	if start <= 0 {
		start = 120 * time.Second
	}
	if stream {
		start = a.cfg.StreamStartTimeout
		if start <= 0 {
			start = defaultFailoverStartTimeout
		}
	}
	timer := time.AfterFunc(start, cancelContext)
	cleanup := func() {}
	cancel := func() {
		timer.Stop()
		cancelContext()
		cleanup()
	}
	failed := func(err error, reason string) (attemptResult, context.CancelFunc) {
		status := http.StatusBadGateway
		retry := true
		if incoming.Context().Err() != nil {
			reason = "downstream_canceled"
			retry = false
		} else if ctx.Err() != nil {
			status = http.StatusGatewayTimeout
			reason = "upstream_timeout"
		}
		return attemptResult{Status: status, Reason: reason, Err: err, Retryable: retry}, cancel
	}
	if err := a.ensureFreshProviderCredential(incoming.Context(), &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}, cancel
	}
	base, err := url.Parse(z.Provider.BaseURL)
	if err != nil {
		return failed(err, "route_configuration_error")
	}
	escaped, err := url.ParseRequestURI(joinEndpointPath(base.EscapedPath(), path))
	if err != nil {
		return failed(err, "route_configuration_error")
	}
	base.Path, base.RawPath = escaped.Path, escaped.RawPath
	if incoming.URL.RawQuery != "" {
		if base.RawQuery != "" {
			base.RawQuery += "&"
		}
		base.RawQuery += incoming.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(raw))
	if err != nil {
		return failed(err, "route_configuration_error")
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	if _, ok := incoming.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", "")
	}
	req.Header.Del("Proxy-Authorization")
	if err := setProviderAuth(req, z); err != nil {
		return failed(err, "route_configuration_error")
	}
	client, err := a.clientForNode(z.Provider.IPPoolNodeID)
	if err != nil {
		return failed(err, "upstream_transport_error")
	}
	clone := *client
	clone.Timeout = 0
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		// The body is forwarded byte for byte, including its own compression
		// headers, so the transport must not negotiate on the client's behalf.
		transport.DisableCompression = true
		clone.Transport = transport
		cleanup = transport.CloseIdleConnections
	}
	resp, err := clone.Do(req)
	if err != nil {
		return failed(err, "upstream_transport_error")
	}
	idle := a.cfg.StreamIdleTimeout
	if idle <= 0 {
		idle = defaultFailoverIdleTimeout
	}
	if !stream {
		idle = start
	}
	resp.Body = &passthroughTimedBody{ReadCloser: resp.Body, timer: timer, idle: idle, first: onFirstByte, cancel: cancelContext}
	return attemptResult{
		Status:     resp.StatusCode,
		Response:   resp,
		Retryable:  retryableStatus(resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}, cancel
}

// acquireInferenceRoute selects one credential of a channel and reserves it.
//
// The planner deliberately does not reuse acquireRoute: a channel's own attempts
// must not be blocked by the cooldown this very request just recorded on the
// credential it already failed on, and the per-channel budget (not a global
// tried-set) is what limits repetition. Concurrency limits and the provider
// circuit breaker still apply through routeSelectableLocked.
func (a *App) acquireInferenceRoute(keys []resolvedRoute, isolated map[int64]bool, attempt int) (resolvedRoute, routeAvailability, bool) {
	nowTime := time.Now()
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	availability := routeAvailability{Reason: "no_eligible_route"}
	if len(keys) == 0 {
		return resolvedRoute{}, availability, false
	}
	start := (attempt - 1) % len(keys)
	for offset := 0; offset < len(keys); offset++ {
		z := keys[(start+offset)%len(keys)]
		if isolated[z.ProviderKeyID] && len(isolated) < len(keys) {
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

func decodeExclusions(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// annotate records the routing snapshot on one ledger row.
func (run *inferenceRun) annotate(id, stop string) {
	if id == "" {
		return
	}
	run.app.queueLedgerWrite(`UPDATE request_ledger SET routing_strategy=?,candidate_count=?,candidate_exclusions=?,stop_reason=? WHERE request_id=?`,
		inferenceStrategy, run.count, run.exclusionJSON(), stop, id)
}

// recordAttempt writes the per-attempt routing detail the console shows.
func (run *inferenceRun) recordAttempt(id string, z resolvedRoute, adapter string, total, channelIndex, attempt int) {
	mode := providerInferenceMode(z.Provider.Type, run.path).mode
	order, _ := json.Marshal(storedOrderFromChannels(run.plan.Channels))
	run.app.queueLedgerWrite(`INSERT INTO inference_attempts(request_id,gateway_request_id,task_hash,task_source,task_scope,provider_id,provider_key_id,channel_attempt,channel_hop,adapter_id,execution_mode,upstream_protocol,upstream_path,candidate_order,stop_reason,fault_scope,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(request_id) DO UPDATE SET adapter_id=excluded.adapter_id,execution_mode=excluded.execution_mode,upstream_path=excluded.upstream_path`,
		id, run.gatewayID, run.taskHash, run.taskSource, run.taskScope, z.Provider.ID, z.ProviderKeyID,
		total, channelIndex, adapter, mode, run.protocol, inferenceUpstreamPath(z, run.path),
		string(order), "", "", now())
}

func (run *inferenceRun) recordFault(id, scope string) {
	if scope == "" || id == "" {
		return
	}
	run.app.queueLedgerWrite(`UPDATE inference_attempts SET fault_scope=? WHERE request_id=?`, scope, id)
}

// advance persists the forward move of this task.
//
// The write is monotonic in generation, so a slow request that lost the race
// cannot move the task back to a channel it already left.
func (run *inferenceRun) advance(r *http.Request, next int) {
	if run.taskHash == "" || next <= 0 || next >= len(run.plan.Channels) {
		return
	}
	run.session = routingSession{
		TaskHash:      run.taskHash,
		TaskScope:     run.taskScope,
		TaskSource:    run.taskSource,
		Order:         storedOrderFromChannels(run.plan.Channels),
		Cursor:        next,
		ProviderID:    run.plan.Channels[next].ProviderID,
		ProviderKeyID: run.session.ProviderKeyID,
		PlanID:        sessionPlanID(run.plan.Channels),
		Generation:    run.session.Generation + 1,
	}
	if err := run.app.bindRoutingSession(r.Context(), run.session); err != nil {
		run.app.log.Error("routing session advance", "error", err)
	}
}

// noteSuccess records the channel and credential a task completed on so the next
// turn of the same task starts there.
func (run *inferenceRun) noteSuccess(r *http.Request, z resolvedRoute, index int) {
	if run.taskHash != "" {
		session := routingSession{
			TaskHash:      run.taskHash,
			TaskScope:     run.taskScope,
			TaskSource:    run.taskSource,
			Order:         storedOrderFromChannels(run.plan.Channels),
			Cursor:        index,
			ProviderID:    z.Provider.ID,
			ProviderKeyID: z.ProviderKeyID,
			PlanID:        sessionPlanID(run.plan.Channels),
			Generation:    run.session.Generation + 1,
		}
		run.session = session
		if err := run.app.bindRoutingSession(r.Context(), session); err != nil {
			run.app.log.Error("routing session success", "error", err)
		}
	}
	run.app.noteRecoverySuccess(r.Context(), providerFaultScope(z.Provider.ID))
	run.app.noteRecoverySuccess(r.Context(), keyFaultScope(z.ProviderKeyID))
	run.app.noteRecoverySuccess(r.Context(), routeFaultScope(z.Provider.ID, run.model, run.protocol))
}

// finish completes the response.
func (run *inferenceRun) finish(w http.ResponseWriter, r *http.Request, stop string) {
	if run.lastID != "" {
		run.annotate(run.lastID, stop)
	}
	if run.committed {
		// Downstream already owns the response. Layering an upstream status line
		// or a synthesised gateway error on top of bytes the client has already
		// received would corrupt the answer, so only the ledger record changes.
		return
	}
	if run.last != nil {
		resp := run.last
		run.last = nil
		_, _ = passthroughResponse(w, resp)
		return
	}
	status := run.lastCode
	if status < 500 {
		status = http.StatusServiceUnavailable
	}
	failRequest(w, r, status, "upstream_unavailable", stop)
}

// stop handles a request the caller abandoned.
func (run *inferenceRun) stop(r *http.Request, stop string) {
	if run.lastID != "" {
		run.annotate(run.lastID, stop)
	}
}

// inferenceDiagnostics explains, in operator terms, which configured channels
// could not serve this request. It reports identifiers and categories only —
// never credentials or payloads.
func (a *App) inferenceDiagnostics(ctx context.Context, model, path string, routes []resolvedRoute, incoming *http.Request) (int, string) {
	providers := map[int64]bool{}
	exclusions := []string{}
	present := map[int64]bool{}
	a.routeMu.Lock()
	for _, z := range routes {
		if !inferenceRouteEligible(z, model, path) {
			continue
		}
		present[z.Provider.ID] = true
		availability := routeAvailability{}
		if !a.routeSelectableLocked(z, a.stateForLocked(z.Provider), time.Now(), &availability) {
			exclusions = append(exclusions, fmt.Sprintf("provider=%d key=%d: %s", z.Provider.ID, z.ProviderKeyID, availability.Reason))
			continue
		}
		if incoming != nil && len(filterClientRoutes([]resolvedRoute{z}, incoming)) == 0 {
			exclusions = append(exclusions, fmt.Sprintf("provider=%d: client_policy", z.Provider.ID))
			continue
		}
		providers[z.Provider.ID] = true
	}
	a.routeMu.Unlock()
	rows, err := a.reader().QueryContext(ctx, `SELECT p.id,p.type,p.enabled,p.archived,r.enabled,r.public_name,r.upstream_model FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE LOWER(r.public_name)=LOWER(?) OR LOWER(r.upstream_model)=LOWER(?)`, model, model)
	if err != nil {
		exclusions = append(exclusions, "configuration_diagnostics_unavailable")
	} else {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var kind, publicName, upstreamModel string
			var enabled, archived, routeEnabled int
			if rows.Scan(&id, &kind, &enabled, &archived, &routeEnabled, &publicName, &upstreamModel) != nil {
				continue
			}
			reason := ""
			switch {
			case archived != 0:
				reason = "archived"
			case enabled == 0:
				reason = "provider_disabled"
			case routeEnabled == 0:
				reason = "route_disabled"
			case !strings.EqualFold(strings.TrimSpace(upstreamModel), strings.TrimSpace(model)):
				// The site expects a different model name than the client sent.
				// Serving it would require rewriting the payload, which this
				// release does not do.
				reason = "model_name_mapping_requires_rewrite"
			default:
				if support := providerInferenceMode(kind, path); support.mode == "" {
					reason = support.label()
				} else if !present[id] {
					reason = "no_eligible_key_or_credential"
				}
			}
			if reason != "" {
				exclusions = append(exclusions, fmt.Sprintf("provider=%d: %s", id, reason))
			}
		}
	}
	if len(exclusions) == 0 {
		return len(providers), ""
	}
	raw, _ := json.Marshal(exclusions)
	return len(providers), string(raw)
}

// requestReasoningEffortFromRaw extracts the reasoning effort from an untouched
// body without keeping the parsed value on the hot path.
func requestReasoningEffortFromRaw(raw []byte) string {
	var body map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return requestReasoningEffort(body)
}
