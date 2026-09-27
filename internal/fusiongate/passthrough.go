package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func providerPassthroughSupport(kind string) (bool, string) {
	switch kind {
	case "openai", "grok", "openrouter", "openai_compatible", "opencode", "anthropic", "anthropic_compatible":
		return true, ""
	default:
		return false, "纯透传不支持此渠道：需要专用协议适配，账号与历史数据保留"
	}
}

const maxPassthroughBody = 32 << 20
const maxPassthroughError = 2 << 20

func passthroughModel(body []byte, contentType string) (string, bool, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err == nil && mediaType == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false, err
			}
			if part.FormName() == "model" {
				raw, err := io.ReadAll(io.LimitReader(part, 4097))
				if err != nil {
					return "", false, err
				}
				if len(raw) > 4096 {
					return "", false, errors.New("model field too large")
				}
				return string(raw), false, nil
			}
		}
		return "", false, nil
	}
	var metadata struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", false, err
	}
	return metadata.Model, metadata.Stream, nil
}

func passthroughProtocol(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "openai_chat"
	case "/v1/responses":
		return "openai_responses"
	case "/v1/responses/compact":
		return "openai_responses_compact"
	case "/v1/messages":
		return "anthropic_messages"
	case "/v1/messages/count_tokens":
		return "anthropic_count_tokens"
	case "/v1/images/generations":
		return "openai_images"
	case "/v1/audio/speech":
		return "openai_audio_speech"
	case "/v1/audio/transcriptions":
		return "openai_audio_transcriptions"
	case "/v1/embeddings":
		return "openai_embeddings"
	}
	return path
}

// Metadata contains identifiers and exclusion categories only, never credentials or payloads.
func (a *App) passthroughDiagnostics(ctx context.Context, model string, routes []resolvedRoute, incoming *http.Request) (int, string) {
	providers := map[int64]bool{}
	exclusions := []string{}
	present := map[int64]bool{}
	a.routeMu.Lock()
	for _, z := range routes {
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
	rows, err := a.reader().QueryContext(ctx, `SELECT p.id,p.type,p.enabled,p.archived,r.enabled,r.public_name,r.upstream_model FROM model_routes r JOIN providers p ON p.id=r.provider_id WHERE LOWER(r.public_name)=LOWER(?)`, model)
	if err != nil {
		exclusions = append(exclusions, "configuration_diagnostics_unavailable")
	} else {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var kind, public, upstream string
			var enabled, archived, routeEnabled int
			if rows.Scan(&id, &kind, &enabled, &archived, &routeEnabled, &public, &upstream) != nil {
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
			case public != model || upstream != model:
				reason = "model_rewrite_required"
			default:
				if supported, _ := providerPassthroughSupport(kind); !supported {
					reason = "specialized_adapter_unsupported"
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

func (a *App) passthroughInference(w http.ResponseWriter, r *http.Request, key authKey) {
	if r.Method != http.MethodPost {
		failRequest(w, r, 405, "method_not_allowed", "POST required")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/images/") && !key.AllowImages {
		failRequest(w, r, 403, "images_not_allowed", "images not allowed")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/audio/") && !key.AllowAudio {
		failRequest(w, r, 403, "audio_not_allowed", "audio not allowed")
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
		failRequest(w, r, 400, "invalid_request", "model is required and must be readable")
		return
	}
	if !allowed(key, model) {
		failRequest(w, r, 403, "model_not_allowed", "model not allowed")
		return
	}
	strategy := a.globalRoutingStrategy()
	routes, resolveErr := a.resolve(r.Context(), model, "passthrough")
	count, exclusions := a.passthroughDiagnostics(r.Context(), model, routes, r)
	routes = filterClientRoutes(routes, r)
	gatewayID := requestID()
	protocol := passthroughProtocol(r.URL.Path)
	finalSuccess := false
	defer func() {
		a.metrics.completed.Add(1)
		if finalSuccess {
			a.metrics.successes.Add(1)
		} else {
			a.metrics.failures.Add(1)
		}
	}()
	annotate := func(id, stop string) {
		a.queueLedgerWrite(`UPDATE request_ledger SET routing_strategy=?,candidate_count=?,candidate_exclusions=?,stop_reason=? WHERE request_id=?`, string(strategy), count, exclusions, stop, id)
	}
	if resolveErr != nil || len(routes) == 0 {
		status, reason := 404, "no_eligible_model_route"
		if errors.Is(resolveErr, errRouteResolution) {
			status, reason = 503, "route_resolution_failed"
		} else if resolveErr == nil {
			status, reason = 403, "provider_client_policy_mismatch"
		}
		z := resolvedRoute{Route: Route{PublicName: model, UpstreamModel: model}}
		started := time.Now()
		id := a.startLedger(key, z, protocol, stream, requestClientIP(r), gatewayID, "", 0, "")
		annotate(id, reason)
		a.endLedger(id, 0, key.ID, "", model, false, status, reason, started, Usage{CostType: "unknown"})
		failRequest(w, r, status, reason, "no eligible native passthrough route")
		return
	}
	routes = interleaveProviderKeys(a.prepareRoutes(routes, strategy))
	for i := range routes {
		routes[i].AttemptID = int64(i + 1)
	}
	tried := map[int64]bool{}
	providerVisits := map[int64]int{}
	var last *http.Response
	var lastID string
	lastStatus := 503
	lastReason := ""
	defer func() {
		if last != nil {
			last.Body.Close()
		}
	}()
	finish := func(stop string) {
		if lastID != "" {
			annotate(lastID, stop)
		}
		if last != nil {
			_, _ = passthroughResponse(w, last)
			return
		}
		status := lastStatus
		if status < 500 {
			status = 503
		}
		failRequest(w, r, status, "upstream_unavailable", stop)
	}
	for attempt := 1; ; attempt++ {
		if r.Context().Err() != nil {
			if lastID != "" {
				annotate(lastID, "downstream_canceled")
			}
			return
		}
		if a.cfg.MaxFailoverAttempts > 0 && attempt > a.cfg.MaxFailoverAttempts {
			finish("attempt_limit")
			return
		}
		// Adaptive weighting applies only within the least-visited provider round.
		eligible := routes
		if strategy == StrategyAdaptive {
			minimum := int(^uint(0) >> 1)
			for _, z := range routes {
				if !tried[z.AttemptID] && providerVisits[z.Provider.ID] < minimum {
					minimum = providerVisits[z.Provider.ID]
				}
			}
			eligible = nil
			for _, z := range routes {
				if providerVisits[z.Provider.ID] == minimum {
					eligible = append(eligible, z)
				}
			}
		}
		z, availability, ok := a.acquireRoute(eligible, tried, strategy)
		if !ok && len(eligible) != len(routes) {
			z, availability, ok = a.acquireRoute(routes, tried, strategy)
		}
		if !ok {
			if lastID == "" {
				started := time.Now()
				empty := resolvedRoute{Route: Route{PublicName: model, UpstreamModel: model}}
				lastID = a.startLedger(key, empty, protocol, stream, requestClientIP(r), gatewayID, "", 0, "")
				a.endLedger(lastID, 0, key.ID, "", model, false, 503, availability.Reason, started, Usage{CostType: "unknown"})
			}
			stop := availability.Reason
			if stop == "no_eligible_route" && len(tried) > 0 {
				stop = "candidates_exhausted"
			}
			if last == nil && availability.RetryAfter > 0 {
				w.Header().Set("Retry-After", fmt.Sprint(max(1, int(availability.RetryAfter.Seconds()))))
			}
			finish(stop)
			return
		}
		tried[z.AttemptID] = true
		providerVisits[z.Provider.ID]++
		a.metrics.attempts.Add(1)
		if attempt > 1 {
			a.metrics.failovers.Add(1)
		}
		started := time.Now()
		id := a.startLedger(key, z, protocol, stream, requestClientIP(r), gatewayID, "", attempt, lastReason)
		lastID = id
		annotate(id, "")
		var firstByte time.Duration
		result, cancel := a.passthroughAttempt(r, raw, z, stream, func() {
			firstByte = time.Since(started)
			a.recordFirstByte(id, started)
			a.metrics.firstByteCount.Add(1)
			a.metrics.firstByteMillis.Add(max(1, firstByte.Milliseconds()))
		})
		stop := "failover"
		terminal := false
		if result.Response != nil {
			resp := result.Response
			if result.Retryable {
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPassthroughError+1))
				if readErr == nil && len(body) <= maxPassthroughError {
					resp.Body.Close()
					cancel()
					resp.Body = io.NopCloser(bytes.NewReader(body))
					if last != nil {
						last.Body.Close()
					}
					last = resp
				} else {
					// Resource boundary: forward the original bytes, including any already read.
					resp.Body = &prefixedPassthroughBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), Closer: resp.Body}
					result.Err, result.Reason = passthroughResponse(w, resp)
					if readErr != nil && result.Err == nil {
						result.Err = readErr
						result.Reason = "upstream_read_error"
					}
					stop = "error_body_limit"
					if readErr != nil {
						stop = "error_body_interrupted"
					}
					terminal = true
				}
			} else {
				result.Err, result.Reason = passthroughResponse(w, resp)
				stop = "http_terminal"
				if result.Err != nil {
					stop = "response_interrupted"
				}
				terminal = true
			}
		} else if !result.Retryable {
			terminal = true
			stop = "transport_terminal"
		}
		cancel()
		if r.Context().Err() != nil {
			result.Err = r.Context().Err()
			result.Reason = "downstream_canceled"
			stop = "downstream_canceled"
			terminal = true
		}
		lastStatus = result.Status
		if lastStatus == 0 {
			lastStatus = 502
		}
		lastReason = result.Reason
		if lastReason == "" && lastStatus >= 400 {
			lastReason = retryReason(lastStatus, result.Err)
		}
		result.Reason = lastReason
		a.completeRoute(z, result, time.Since(started), firstByte)
		success := result.Err == nil && lastStatus < 400
		a.endLedger(id, z.Provider.ID, key.ID, z.Provider.Type, z.Route.UpstreamModel, success, lastStatus, lastReason, started, Usage{CostType: "unknown"})
		annotate(id, stop)
		if terminal {
			finalSuccess = success
			if result.Response == nil && r.Context().Err() == nil {
				finish(stop)
			}
			return
		}
	}
}

type prefixedPassthroughBody struct {
	io.Reader
	io.Closer
}

func passthroughReplayable(r *http.Request) bool {
	return r.Method == http.MethodPost && passthroughProtocol(r.URL.Path) != r.URL.Path
}

// The timer is reset by bytes, not JSON/SSE semantics. It owns no read goroutine.
type passthroughTimedBody struct {
	io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	first  func()
	once   sync.Once
	cancel context.CancelFunc
}

func (b *passthroughTimedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.once.Do(b.first)
		b.timer.Reset(b.idle)
	}
	if err != nil {
		b.timer.Stop()
	}
	return n, err
}
func (b *passthroughTimedBody) Close() error { b.timer.Stop(); b.cancel(); return b.ReadCloser.Close() }

func (a *App) passthroughAttempt(incoming *http.Request, raw []byte, z resolvedRoute, stream bool, onFirstByte func()) (attemptResult, context.CancelFunc) {
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
	cancel := func() { timer.Stop(); cancelContext(); cleanup() }
	failed := func(err error, reason string) (attemptResult, context.CancelFunc) {
		status := 502
		retry := passthroughReplayable(incoming)
		if incoming.Context().Err() != nil {
			reason = "downstream_canceled"
			retry = false
		} else if ctx.Err() != nil {
			status = 504
			reason = "upstream_timeout"
		}
		return attemptResult{Status: status, Reason: reason, Err: err, Retryable: retry}, cancel
	}
	base, err := url.Parse(z.Provider.BaseURL)
	if err != nil {
		return failed(err, "route_configuration_error")
	}
	escaped, err := url.ParseRequestURI(joinEndpointPath(base.EscapedPath(), incoming.URL.EscapedPath()))
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
	req, err := http.NewRequestWithContext(ctx, incoming.Method, base.String(), bytes.NewReader(raw))
	if err != nil {
		return failed(err, "route_configuration_error")
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	if _, ok := incoming.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", "")
	}
	req.Header.Del("Proxy-Authorization")
	if z.Provider.Type == "anthropic" || z.Provider.Type == "anthropic_compatible" {
		req.Header.Set("X-Api-Key", z.Credential)
	} else {
		req.Header.Set("Authorization", "Bearer "+z.Credential)
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
	return attemptResult{Status: resp.StatusCode, Response: resp, Retryable: retryableStatus(resp.StatusCode) && passthroughReplayable(incoming), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}, cancel
}

func passthroughResponse(w http.ResponseWriter, resp *http.Response) (error, string) {
	defer resp.Body.Close()
	copyUpstreamResponseHeaders(w.Header(), resp.Header)
	skip := connectionHeaders(resp.Header)
	allowedTrailer := func(key string) bool {
		key = http.CanonicalHeaderKey(key)
		return !hopByHopHeaders[key] && !skip[key] && !gatewayOwnedResponseHeaders[key] && key != "Set-Cookie" && key != "Content-Length"
	}
	if len(resp.Trailer) > 0 {
		w.Header().Del("Content-Length")
	}
	for key := range resp.Trailer {
		if allowedTrailer(key) {
			w.Header().Add("Trailer", key)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return e, "downstream_write_error"
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				return err, "upstream_read_error"
			}
			break
		}
	}
	for key, values := range resp.Trailer {
		if allowedTrailer(key) {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
	}
	return nil, ""
}
