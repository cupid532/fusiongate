package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
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
	path := r.URL.Path
	var model string
	var stream bool
	geminiModel, geminiMethod, isGemini := geminiPathModel(path)
	if isGemini {
		// Gemini names the model and the streaming mode in the path, not the body.
		model, stream = geminiModel, geminiMethod == "streamGenerateContent"
		if !json.Valid(raw) {
			err = errors.New("invalid JSON body")
		}
	} else {
		model, stream, err = passthroughModel(raw, r.Header.Get("Content-Type"))
	}
	if err != nil || model == "" {
		failRequest(w, r, http.StatusBadRequest, "invalid_request", "model is required and must be readable")
		return
	}
	requested := model
	if canonical, err := a.canonicalModel(r.Context(), model); err == nil {
		model = canonical
	}
	if !modelAllowed(key, requested, model) {
		failRequest(w, r, http.StatusForbidden, "model_not_allowed", "model not allowed")
		return
	}
	if isGemini && geminiMethod == "countTokens" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(geminiCountTokens(raw))
		return
	}

	protocol := passthroughProtocol(path)
	if isGemini {
		protocol = "gemini_generate_content"
	}
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
		geminiSSE: isGemini && r.URL.Query().Get("alt") == "sse",
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
	for index := range routes {
		// Answers carry the name the client asked for, alias or not.
		routes[index].Route.PublicName = requested
	}
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
	geminiSSE  bool
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
	retryWait  time.Duration
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
		status, reason, hint = http.StatusServiceUnavailable, "route_resolution_failed", "channel configuration could not be resolved"
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
			compatible := make([]resolvedRoute, 0, len(keys))
			for _, candidate := range keys {
				if client, target, bridge := parseBridgeAdapter(inferenceAdapterID(candidate, run.path)); bridge {
					if err := validateBridgeRoute(client, target, run.raw, run.path, candidate); err != nil {
						run.lastErr = "capability_not_supported"
						run.noteSkip("capability_not_supported")
						continue
					}
				}
				compatible = append(compatible, candidate)
			}
			if len(compatible) == 0 {
				break
			}
			z, availability, ok := a.acquireInferenceRoute(compatible, isolated, attempt)
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
			id := a.startLedger(run.key, z, run.protocol, run.stream, run.clientIP, run.gatewayID, run.reasoning, run.attempts, run.lastErr, r.Context())
			if id == "" {
				a.completeRoute(z, attemptResult{Reason: "downstream_canceled", Err: r.Context().Err()}, 0)
				run.stop(r, "downstream_canceled")
				return
			}
			run.lastID = id
			adapter := inferenceAdapterID(z, run.path)
			run.recordAttempt(id, z, adapter, run.attempts, index, attempt)

			diagnostics := &attemptDiagnostics{start: started, RetryWaitMS: run.retryWait.Milliseconds()}
			run.retryWait = 0
			attemptRequest := r.WithContext(context.WithValue(r.Context(), diagnosticsKey{}, diagnostics))
			output := &diagnosticWriter{ResponseWriter: w, diagnostics: diagnostics, stream: run.stream}
			output.observer.onOutput = diagnostics.firstOutput
			var firstByte time.Duration
			result, cancel := run.attempt(output, attemptRequest, z, adapter, func() {
				diagnostics.firstByte()
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
						result.Err, result.Reason = passthroughResponse(output, result.Response)
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
					previousReason := result.Reason
					result.Err, result.Reason, result.Usage = passthroughResponseUsage(output, result.Response, passthroughUsageFormat(inferenceUpstreamPath(z, run.path)))
					if result.Reason == "" {
						result.Reason = previousReason
					}
					if result.Usage.Reported {
						cost(z, &result.Usage)
					}
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
			// A client may close its connection after receiving the complete
			// response. Do not overwrite an already settled result during cleanup.
			if r.Context().Err() != nil && !(terminal && result.Err == nil) {
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
			finalWrites := []ledgerWrite{}
			a.completeRouteWithWriter(z, result, time.Since(started), func(query string, args ...any) {
				finalWrites = append(finalWrites, ledgerWrite{query: query, args: args})
			}, firstByte)

			success := status < 400 && result.Err == nil
			// Passthrough usage comes from the passive tap and bridged usage from
			// the adapter; both are already priced. A channel that reports no
			// usage stays explicitly unknown rather than reading as free.
			usage := result.Usage
			if usage.CostType == "" {
				usage.CostType = "unknown"
			}
			finalWrites = append(finalWrites, ledgerWrite{query: `UPDATE request_ledger SET diagnostics_json=?,routing_strategy=?,candidate_count=?,candidate_exclusions=?,stop_reason=? WHERE request_id=?`, args: []any{diagnostics.encoded(reason, success), inferenceStrategy, run.count, run.exclusionJSON(), stop, id}})
			a.endLedger(id, z.Provider.ID, run.key.ID, z.Provider.Type, z.Route.UpstreamModel, success, status, reason, started, usage, finalWrites...)
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
	started := time.Now()
	defer func() { run.retryWait += time.Since(started) }()
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
	if client, target, ok := parseBridgeAdapter(adapter); ok {
		result, cancel := run.bridgeAttempt(w, r, z, client, target, onFirstByte)
		if protocolPolicyDenied(result.Response) {
			result.Retryable = false
			result.Reason = "upstream_protocol_denied"
			return result, cancel
		}
		if result.Response != nil && result.Status >= 400 && protocolUnsupportedSignal(result.Response) {
			protocolMemory.remember(z, target)
			result.Retryable = true
			result.Reason = "protocol_fallback"
		}
		return result, cancel
	}
	if run.path == "/v1/messages/count_tokens" && (!containsString(routeWireProtocols(z), wireMessages) || protocolMemory.unsupported(z, wireMessages)) {
		return run.localCountTokens(w), func() {}
	}
	result, cancel := a.inferenceAttempt(r, run.nativeBody(z), z, inferenceUpstreamPath(z, run.path), run.stream, onFirstByte)
	if protocolPolicyDenied(result.Response) {
		result.Retryable = false
		result.Reason = "upstream_protocol_denied"
		return result, cancel
	}
	client := clientWireProtocol(run.path)
	if result.Response == nil || result.Status < 400 {
		if client != "" && result.Response != nil {
			protocolMemory.forget(z, client)
		}
		return result, cancel
	}
	if !protocolUnsupportedSignal(result.Response) {
		return result, cancel
	}
	if run.path == "/v1/messages/count_tokens" {
		result.Response.Body.Close()
		cancel()
		protocolMemory.remember(z, wireMessages)
		return run.localCountTokens(w), func() {}
	}
	// A probe is a real attempt. Learn only explicit endpoint rejection and
	// let the normal bounded loop make (and log) any subsequent bridge call.
	fallback := fallbackInferenceAdapter(z, run.path)
	if _, _, ok := parseBridgeAdapter(fallback); ok {
		protocolMemory.remember(z, client)
		result.Retryable = true
		result.Reason = "protocol_fallback"
	}
	return result, cancel

}

// nativeBody is the client's own body. Only when the route maps the public name
// onto a different upstream name is the top-level model field rewritten; every
// other byte is forwarded as sent.
func (run *inferenceRun) nativeBody(z resolvedRoute) []byte {
	upstream := strings.TrimSpace(z.Route.UpstreamModel)
	if upstream == "" || upstream == run.requestedModel() {
		return run.raw
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(run.raw, &body) != nil {
		return run.raw
	}
	if _, ok := body["model"]; !ok {
		return run.raw
	}
	body["model"], _ = json.Marshal(upstream)
	encoded, err := json.Marshal(body)
	if err != nil {
		return run.raw
	}
	return encoded
}

// requestedModel is the model name exactly as the client's body carries it.
func (run *inferenceRun) requestedModel() string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(run.raw, &body)
	return strings.TrimSpace(body.Model)
}

// noteAdapter records that the attempt switched to a bridge after the native
// call showed the channel does not serve the client's protocol.
func (run *inferenceRun) noteAdapter(z resolvedRoute, adapter string) {
	if run.lastID == "" {
		return
	}
	run.app.queueLedgerWrite(`UPDATE inference_attempts SET adapter_id=?,execution_mode=?,upstream_path=? WHERE request_id=?`,
		adapter, inferenceModeBridge, inferenceAttemptPath(z, run.path, adapter), run.lastID)
}

// localCountTokens answers count_tokens from the gateway's own estimate when the
// channel has no Messages endpoint to ask.
func (run *inferenceRun) localCountTokens(w http.ResponseWriter) attemptResult {
	var body map[string]any
	_ = json.Unmarshal(run.raw, &body)
	encoded, _ := json.Marshal(map[string]any{"input_tokens": estimateAnthropicInputTokens(body)})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-FusionGate-Request-ID", run.gatewayID)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: err}
	}
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: Usage{CostType: "unknown"}}
}

// inferenceAttempt sends the client's own bytes to one channel.
//
// It differs from a bare passthrough in exactly three ways, all of which are
// required to keep an auth-file channel usable without touching the payload: the
// OAuth credential is refreshed when it is near expiry, the credential and
// account headers are applied for the channel's real type, and the wire path is
// the one that channel actually exposes.
func (a *App) inferenceAttempt(incoming *http.Request, raw []byte, z resolvedRoute, path string, stream bool, onFirstByte func()) (attemptResult, context.CancelFunc) {
	return a.inferenceSend(incoming, raw, z, path, stream, onFirstByte, false)
}

// inferenceSend performs the upstream call. A bridged body is the gateway's own
// JSON rather than the client's bytes: it carries no client query string or
// content encoding, and the transport may negotiate compression because the
// gateway, not the client, decodes the answer.
func (a *App) inferenceSend(incoming *http.Request, raw []byte, z resolvedRoute, path string, stream bool, onFirstByte func(), bridged bool) (attemptResult, context.CancelFunc) {
	ctx, cancelContext := context.WithCancel(incoming.Context())
	start := time.Duration(z.Provider.RequestTimeoutMS) * time.Millisecond
	if start <= 0 {
		start = 120 * time.Second
	}
	if stream {
		start, _ = a.streamTimeouts(z.Provider)
	}
	diagnostics := diagnosticFor(ctx)
	var streamStarted atomic.Bool
	timer := time.AfterFunc(start, func() {
		if diagnostics != nil {
			kind := "upstream_start_timeout"
			if !stream {
				kind = "upstream_total_timeout"
			} else if streamStarted.Load() {
				kind = "upstream_idle_timeout"
			}
			diagnostics.timedOut(kind)
		}
		cancelContext()
	})
	cancel := func() {
		timer.Stop()
		cancelContext()
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
	if incoming.URL.RawQuery != "" && !bridged {
		if base.RawQuery != "" {
			base.RawQuery += "&"
		}
		base.RawQuery += incoming.URL.RawQuery
	}
	if diagnostics != nil {
		ctx = httptrace.WithClientTrace(ctx, diagnostics.trace())
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
	if bridged {
		for _, key := range []string{"X-Goog-Api-Key", "Content-Encoding", "Accept-Encoding", "Content-Type", "Accept"} {
			req.Header.Del(key)
		}
		for key := range req.Header {
			if strings.HasPrefix(strings.ToLower(key), "anthropic-") && clientWireProtocol(path) != wireMessages {
				req.Header.Del(key)
			}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
	}
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
	if transport, ok := client.Transport.(*http.Transport); ok && !bridged {
		clone.Transport = a.nativeTransport(transport)
	}
	resp, err := clone.Do(req)
	if err != nil {
		return failed(err, "upstream_transport_error")
	}
	_, idle := a.streamTimeouts(z.Provider)
	if !stream {
		idle = 0
	} // Non-streaming requests retain the absolute deadline.
	resp.Body = &passthroughTimedBody{ReadCloser: resp.Body, timer: timer, idle: idle, first: func() {
		streamStarted.Store(true)
		if onFirstByte != nil {
			onFirstByte()
		}
	}, cancel: cancelContext}
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
	if _, _, ok := parseBridgeAdapter(adapter); ok {
		mode = inferenceModeBridge
	} else if mode == inferenceModeBridge {
		mode = inferenceModeNative
	}
	order, _ := json.Marshal(storedOrderFromChannels(run.plan.Channels))
	run.app.queueLedgerWrite(`INSERT INTO inference_attempts(request_id,gateway_request_id,task_hash,task_source,task_scope,provider_id,provider_key_id,channel_attempt,channel_hop,adapter_id,execution_mode,upstream_protocol,upstream_path,candidate_order,stop_reason,fault_scope,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(request_id) DO UPDATE SET adapter_id=excluded.adapter_id,execution_mode=excluded.execution_mode,upstream_path=excluded.upstream_path`,
		id, run.gatewayID, run.taskHash, run.taskSource, run.taskScope, z.Provider.ID, z.ProviderKeyID,
		total, channelIndex, adapter, mode, passthroughProtocol(inferenceAttemptPath(z, run.path, adapter)), inferenceAttemptPath(z, run.path, adapter),
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
	if run.lastID != "" && !run.committed {
		run.annotate(run.lastID, stop)
	}
	if run.committed {
		// Downstream already owns the response. Layering an upstream status line
		// or a synthesised gateway error on top of bytes the client has already
		// received would corrupt the answer, so only the ledger record changes.
		return
	}
	if run.lastErr == "capability_not_supported" {
		failRequest(w, r, http.StatusBadRequest, "capability_not_supported", "no compatible channel can preserve the requested features; use a native protocol channel")
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
