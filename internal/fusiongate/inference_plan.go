package fusiongate

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// V3.13 replaces the four selectable routing strategies with one: priority
// failover. The scheduler in scheduler.go still implements the old strategies
// because the legacy adapter handlers use them, but the public inference path
// only ever orders candidates one way.
const inferenceStrategy = "priority_failover"

// Execution modes. Native and identity pass the request body through unchanged;
// only the credential, the account headers and (when the upstream requires it)
// the path differ. Bridge is the opt-in protocol conversion.
const (
	inferenceModeNative   = "native"
	inferenceModeIdentity = "identity"
	inferenceModeBridge   = "bridge"
)

// inferenceSupport describes what one provider type can do on one public
// inference endpoint.
//
// mode is empty when the type cannot serve it. reason explains the exclusion in
// operator-readable terms.
type inferenceSupport struct {
	mode   string
	reason string
}

// label names the exclusion for operators and for the ledger. Unsupported types
// always carry the same leading token so a stored exclusion can be classified,
// with the human explanation kept after it.
func (s inferenceSupport) label() string {
	if s.mode != "" {
		return s.mode
	}
	if s.reason == "" {
		return "specialized_adapter_unsupported"
	}
	return "specialized_adapter_unsupported: " + s.reason
}

// inferenceProviderSupport classifies a provider type, independent of the
// endpoint.
//
//   - native: the upstream accepts the OpenAI/Anthropic wire format directly, so
//     the request body, compression, streaming events and unknown fields are
//     forwarded untouched.
//   - identity: the upstream speaks the same wire format but needs an account
//     credential, account headers, a refresh, or a different URL shape. The body
//     still passes through unchanged.
//   - empty: no verified implementation for these endpoints. These channels stay
//     stored and enabled exactly as configured, they are simply not selected.
func inferenceProviderSupport(kind string) inferenceSupport {
	switch kind {
	case "openai", "grok", "openrouter", "openai_compatible", "opencode", "anthropic", "anthropic_compatible":
		return inferenceSupport{mode: inferenceModeNative}
	case "codex_oauth", "claude_oauth", "grok_oauth":
		// Credential refresh, account headers and endpoint mapping only.
		return inferenceSupport{mode: inferenceModeIdentity}
	case "gemini", "gemini_oauth", "gemini_cli", "antigravity", "qwen_oauth", "iflow_oauth":
		return inferenceSupport{reason: "待验证/受限：该渠道使用独立的生成接口，尚未在公开推理入口验证，账号与历史数据保留"}
	case "grok_console", "grok_web":
		return inferenceSupport{reason: "待验证/受限：该渠道使用网页/控制台会话协议，尚未在公开推理入口验证，账号与历史数据保留"}
	default:
		return inferenceSupport{reason: fmt.Sprintf("未知渠道类型 %q：需要在专用适配器中实现并验证", kind)}
	}
}

// providerPassthroughSupport keeps the V3.12 admin field working. The label now
// reflects every mode the public endpoints can serve, so an auth-file channel no
// longer reports itself as unsupported.
func providerPassthroughSupport(kind string) (bool, string) {
	support := inferenceProviderSupport(kind)
	if support.mode == "" {
		return false, support.reason
	}
	return true, ""
}

// inferenceRouteEligible is the one eligibility rule for a public inference
// request, shared by candidate selection and by the console diagnostics so the
// two can never disagree.
//
// V3.13 forwards the client's body untouched, so a channel is only eligible when
// the model name the client sent is the name that channel already expects: the
// mapping's upstream_model must equal the requested model. A route whose
// public_name differs is still usable when its upstream name matches, because
// nothing in the payload is rewritten; the routes console labels the entries
// that would need a rewrite instead of silently rewriting them.
func inferenceRouteEligible(z resolvedRoute, model, path string) bool {
	if path != "" && providerInferenceMode(z.Provider.Type, path).mode == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(z.Route.UpstreamModel), strings.TrimSpace(model))
}

// providerInferenceMode reports whether this provider type may serve the given
// public path, and with which execution mode. An empty path means "any public
// inference endpoint", which is what the routes console asks for.
//
// An identity channel only accepts the endpoints its own API actually exposes:
// the Codex backend has no OpenAI-compatible /v1 path and no Chat Completions
// endpoint, so sending it one would be a fabricated capability.
func providerInferenceMode(kind, path string) inferenceSupport {
	support := inferenceProviderSupport(kind)
	if support.mode == "" || path == "" {
		return support
	}
	if support.mode != inferenceModeIdentity {
		return support
	}
	switch kind {
	case "codex_oauth":
		if path == "/v1/responses" || path == "/v1/responses/compact" {
			return support
		}
		return inferenceSupport{reason: "codex_oauth 仅提供 Responses 接口，此入口不在其原生路径内"}
	case "claude_oauth":
		if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
			return support
		}
		return inferenceSupport{reason: "claude_oauth 仅提供 Messages 接口，此入口不在其原生路径内"}
	case "grok_oauth":
		if path == "/v1/responses" || path == "/v1/chat/completions" {
			return support
		}
		return inferenceSupport{reason: "grok_oauth 仅提供 Responses/Chat 接口，此入口不在其原生路径内"}
	}
	return support
}

// filterInferenceRoutes keeps only the candidates that can serve this exact
// endpoint. A channel whose API has no such path is an exclusion the diagnostics
// already explain, not a silent failure at request time.
func filterInferenceRoutes(routes []resolvedRoute, path string) []resolvedRoute {
	out := make([]resolvedRoute, 0, len(routes))
	for _, z := range routes {
		if providerInferenceMode(z.Provider.Type, path).mode == "" {
			continue
		}
		out = append(out, z)
	}
	return out
}

// inferenceChannel is one provider and the credential candidates it offers for
// this request. A channel keeps its keys together so that the per-channel
// attempt budget is shared between them instead of multiplied.
type inferenceChannel struct {
	ProviderID int64
	Name       string
	Type       string
	Model      string
	Routes     []resolvedRoute
}

func (c inferenceChannel) label() string {
	return fmt.Sprintf("provider=%d(%s)", c.ProviderID, c.Type)
}

// inferencePlan is the ordered failover plan for one request.
//
// Channels always holds the complete ordered candidate list so a task's history
// can be stored as absolute positions. Start is where this request begins: a task
// that already advanced resumes there instead of walking back up the order.
type inferencePlan struct {
	Model     string
	Channels  []inferenceChannel
	Start     int
	Count     int
	Resumed   bool
	Exhausted bool
}

// orderedInferenceRoutes applies the single approved ordering:
//
//	provider.priority DESC → provider.sort_order ASC → provider.id ASC
//
// with the mapping-level keys breaking ties inside one provider only. Route
// priority may separate two duplicate mappings of the same channel; it can never
// move a channel ahead of a higher-priority channel.
//
// The ordering is stable and total, so two runs over the same configuration
// produce the same plan and the console can present exactly the order the
// gateway will use.
func orderedInferenceRoutes(routes []resolvedRoute) []resolvedRoute {
	planned := append([]resolvedRoute(nil), routes...)
	sort.SliceStable(planned, func(i, j int) bool {
		left, right := planned[i], planned[j]
		if left.Provider.Priority != right.Provider.Priority {
			return left.Provider.Priority > right.Provider.Priority
		}
		if left.Provider.SortOrder != right.Provider.SortOrder {
			return left.Provider.SortOrder < right.Provider.SortOrder
		}
		if left.Provider.ID != right.Provider.ID {
			return left.Provider.ID < right.Provider.ID
		}
		if left.Route.Priority != right.Route.Priority {
			return left.Route.Priority > right.Route.Priority
		}
		if left.Route.SortOrder != right.Route.SortOrder {
			return left.Route.SortOrder < right.Route.SortOrder
		}
		return left.Route.ID < right.Route.ID
	})
	return planned
}

// groupInferenceChannels folds the ordered routes into channels, preserving both
// the channel order and, inside a channel, the credential order.
func groupInferenceChannels(routes []resolvedRoute) []inferenceChannel {
	channels := []inferenceChannel{}
	index := map[int64]int{}
	for _, z := range routes {
		position, ok := index[z.Provider.ID]
		if !ok {
			position = len(channels)
			index[z.Provider.ID] = position
			channels = append(channels, inferenceChannel{
				ProviderID: z.Provider.ID,
				Name:       z.Provider.Name,
				Type:       z.Provider.Type,
				Model:      z.Route.UpstreamModel,
			})
		}
		channels[position].Routes = append(channels[position].Routes, z)
	}
	return channels
}

// buildInferencePlan orders the resolved candidates for the single strategy and
// starts the task at the channel it already advanced to.
func buildInferencePlan(routes []resolvedRoute, path string, session routingSession, hasSession bool) inferencePlan {
	ordered := orderedInferenceRoutes(filterInferenceRoutes(routes, path))
	channels := groupInferenceChannels(ordered)
	plan := inferencePlan{Channels: channels, Count: len(channels)}
	if len(channels) > 0 {
		plan.Model = channels[0].Routes[0].Route.PublicName
	}
	if !hasSession || session.ProviderID <= 0 || len(channels) == 0 {
		return plan
	}
	start := resumeStartIndex(session.Order, session.ProviderID, channels)
	if start >= len(channels) {
		// Every channel this task knew about is gone. Do not silently restart at
		// the top of a plan the task has already exhausted; report candidates
		// exhausted instead, which the console shows as 候选耗尽.
		plan.Channels = nil
		plan.Count = 0
		plan.Exhausted = true
		return plan
	}
	if start > 0 {
		plan.Start = start
		plan.Resumed = true
	}
	return plan
}

// inferenceRetryDelays are the same-channel pauses before attempt 2 and 3, with
// a bounded ladder for configurations that allow more attempts.
var inferenceRetryDelays = []time.Duration{300 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// inferenceRetryDelay returns the pause before the given in-channel attempt
// number (2 means "before the second attempt").
func inferenceRetryDelay(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	position := attempt - 2
	if position >= len(inferenceRetryDelays) {
		position = len(inferenceRetryDelays) - 1
	}
	return inferenceRetryDelays[position]
}

// inferenceDecision is what the executor does after one attempt.
type inferenceDecision int

const (
	// decisionTerminal: the downstream response is settled. Never fail over.
	decisionTerminal inferenceDecision = iota
	// decisionRetryChannel: retry the same channel within its budget.
	decisionRetryChannel
	// decisionAdvanceChannel: stop using this channel and move to the next one,
	// without repeating an endpoint the upstream already rejected.
	decisionAdvanceChannel
	// decisionReturnRaw: the upstream answer is a client-side error. Return it
	// verbatim; retrying or fusing the channel would only hide the real answer.
	decisionReturnRaw
)

// classifyInferenceResult maps one attempt outcome onto the next action.
//
// It is deliberately explicit rather than a generic "retry on 5xx": the plan
// distinguishes a credential problem (isolate the Key) from a missing endpoint
// (stop repeating that path) from a transient transport failure (retry the
// channel), and it never retries a request the upstream rejected as malformed.
func classifyInferenceResult(result attemptResult, downstreamCanceled bool) inferenceDecision {
	if downstreamCanceled || result.Handled {
		// Bytes are already on the wire; a later channel cannot repair them.
		return decisionTerminal
	}
	if result.Err == nil && result.Status > 0 && result.Status < 400 {
		return decisionTerminal
	}
	if result.Response != nil && !result.Retryable {
		if result.Status >= 400 {
			return decisionReturnRaw
		}
		return decisionTerminal
	}
	switch result.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusLengthRequired, http.StatusUnavailableForLegalReasons:
		return decisionReturnRaw
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		// The endpoint does not exist for this channel. Repeating it is futile.
		return decisionAdvanceChannel
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return decisionRetryChannel
	}
	if result.Status >= 500 || result.Err != nil || result.Status == 0 {
		return decisionRetryChannel
	}
	return decisionTerminal
}

// inferenceAdapterID names the adapter that will serve a route, for the ledger
// and the console. It never contains credentials.
func inferenceAdapterID(z resolvedRoute, path string) string {
	if adapter := bridgeAdapterFor(z, path); adapter != "" {
		return adapter
	}
	support := providerInferenceMode(z.Provider.Type, path)
	if support.mode == "" {
		return ""
	}
	return z.Provider.Type + ":" + support.mode
}

// bridgeAdapterFor reports the tested conversion that a fixed-protocol channel
// may use for this path. Bridging is never implicit: it requires an explicit
// protocol_policy=fixed whose preference this provider type supports.
func bridgeAdapterFor(z resolvedRoute, path string) string {
	if fixedRouteProtocol(z) != protocolChat || !isOpenAIWireProvider(z.Provider.Type) {
		return ""
	}
	switch path {
	case "/v1/responses", "/v1/responses/compact":
		return "responses_to_chat"
	case "/v1/messages", "/v1/messages/count_tokens":
		return "messages_to_chat"
	}
	return ""
}

func isOpenAIWireProvider(kind string) bool {
	switch kind {
	case "openai", "grok", "openrouter", "openai_compatible", "opencode", "codex_oauth", "grok_oauth", "grok_console", "grok_web":
		return true
	}
	return false
}

// inferenceUpstreamPath maps a public path onto the wire path of one channel.
//
// Native channels receive the client's own path. Identity channels receive the
// path their API actually exposes: the ChatGPT Codex backend serves /responses
// under its base URL rather than /v1/responses.
func inferenceUpstreamPath(z resolvedRoute, path string) string {
	if z.Provider.Type == "codex_oauth" && strings.HasPrefix(path, "/v1/responses") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// inferenceTaskScopeFor covers the model and capability a binding was made for.
func inferenceTaskScopeFor(model, capability string) string {
	return inferenceTaskScope(model, capability)
}
