package fusiongate

import (
	"net/http"
	"strings"
)

// How a bridge treats a field it cannot express.
//
// The whole policy answers one question per field: does dropping it change what
// the client gets, or only how the answer is shaped?
//
//   - bridgeKeep: the converter expresses it, so the upstream sees it.
//   - bridgeDrop: accepted, then dropped. A sampling hint, a cache lifetime, an
//     attribution tag, a verbosity request -- the answer still means the same
//     thing without it.
//   - bridgeRefuse: it changes what the client gets. A structured-output
//     contract, a second choice, state the bridge cannot carry, a hosted tool
//     that reaches the network. Never quietly degraded into something else.
//
// Declaring every field exactly once, next to the protocols it arrives from, is
// what keeps what used to be three hand-synchronised lists -- the per-client
// accept list, the per-target accept list, and the list of names a rejection may
// print -- from drifting apart. A field a new client starts sending is one line
// here plus, if the converter must carry it, one line in the converter.
type bridgeDisposition int

const (
	bridgeKeep bridgeDisposition = iota
	bridgeDrop
	bridgeRefuse
)

func (d bridgeDisposition) String() string {
	switch d {
	case bridgeKeep:
		return "keep"
	case bridgeDrop:
		return "drop"
	default:
		return "refuse"
	}
}

// bridgeAny marks a rule that applies to every protocol of its stage.
const bridgeAny = "*"

type bridgeFieldRule struct {
	path        string
	disposition bridgeDisposition
	protocols   []string
	reason      string // printed by a refusal
}

func keep(path string, protocols ...string) bridgeFieldRule {
	return bridgeFieldRule{path: path, disposition: bridgeKeep, protocols: protocols}
}

func drop(path string, protocols ...string) bridgeFieldRule {
	return bridgeFieldRule{path: path, disposition: bridgeDrop, protocols: protocols}
}

func refuse(path, reason string, protocols ...string) bridgeFieldRule {
	return bridgeFieldRule{path: path, disposition: bridgeRefuse, protocols: protocols, reason: reason}
}

// bridgeClientFieldPolicy is the policy for the request as the client wrote it.
//
// The set a protocol may send is not the same for all of them: `input` belongs
// to Responses, `system` to Messages, `contents` to Gemini. Reading one table
// top to bottom is how an operator answers "may this client send that" without
// diffing four string literals.
var bridgeClientFieldPolicy = []bridgeFieldRule{
	keep("model"), keep("stream"), keep("tools"),

	// Sampling and length hints. Each one only shapes the answer; where a target
	// cannot take it, the converter drops it instead of refusing the client.
	keep("temperature", wireChat, wireResponses, wireMessages),
	keep("top_p", wireChat, wireResponses, wireMessages),
	keep("max_tokens", wireChat, wireMessages),
	keep("max_output_tokens", wireResponses),
	keep("max_completion_tokens", wireChat),
	keep("tool_choice", wireChat, wireResponses, wireMessages),
	keep("parallel_tool_calls", wireChat, wireResponses),
	keep("stop", wireChat),
	keep("stop_sequences", wireMessages),
	drop("reasoning_effort", wireChat),
	drop("thinking", wireMessages), // an extended budget only the native API honours
	drop("top_k", wireMessages),

	// Prompt-cache hints. Chat has no parameter for how long an upstream retains
	// a cache and the answer is unaffected, so all of them are dropped. The alias
	// the OpenCode SDK emits is normalised into the canonical field first.
	keep("prompt_cache_key", wireChat, wireResponses),
	drop("promptCacheKey", wireChat),
	drop("prompt_cache_options", wireChat, wireResponses),
	drop("prompt_cache_retention", wireChat, wireResponses),

	// Diagnostics, attribution and admission hints: dropped, never refused.
	drop("stream_options", wireChat),
	drop("client_metadata", wireChat, wireResponses),
	drop("frequency_penalty", wireChat),
	drop("presence_penalty", wireChat),
	drop("seed", wireChat),
	drop("user", wireChat),
	drop("logprobs", wireChat),
	drop("top_logprobs", wireChat),
	drop("metadata", wireChat, wireMessages),
	drop("service_tier", wireChat),

	// Deliberately conditional rather than plain: a single choice is not a
	// contract (several are), and store=false is expressible while store=true is
	// state a bridge cannot carry. Their own checks below enforce the value.
	keep("n", wireChat),
	keep("store", wireChat, wireResponses),

	// The fields whose whole content only shapes the answer, inspected one level
	// down, which is why they carry a dotted path.
	keep("reasoning", wireResponses),
	drop("reasoning.summary", wireResponses),
	drop("reasoning.generate_summary", wireResponses),
	drop("include", wireResponses),
	keep("text", wireResponses),
	drop("text.verbosity", wireResponses),

	// The conversational body, in the shape each protocol calls it.
	keep("messages", wireChat, wireMessages),
	keep("system", wireMessages),
	keep("input", wireResponses),
	keep("instructions", wireResponses),
	keep("contents", wireGemini),
	keep("systemInstruction", wireGemini),
	keep("generationConfig", wireGemini),
	drop("toolConfig", wireGemini),

	// Explicit refusals. A field absent from every accept rule would be refused
	// anyway; naming the important ones states why, so the reason travels with
	// the decision instead of living in a comment somewhere else.
	refuse("response_format", "it is a structured-output contract a bridge cannot honour"),
	refuse("previous_response_id", "it is conversation state a bridge cannot carry"),
	refuse("conversation", "it is conversation state a bridge cannot carry"),
	refuse("context_management", "it is conversation state a bridge cannot carry"),
	refuse("truncation", "it changes the answer, not just its shape"),
	refuse("background", "it changes the answer, not just its shape"),
	refuse("modalities", "it changes the answer, not just its shape"),
	refuse("audio", "it changes the answer, not just its shape"),
}

// bridgeChatFieldPolicy is the policy for the intermediate Chat body, applied
// before it is converted into the target protocol. One list covers every target:
// a target that cannot express one of these fields drops it during its own
// conversion, and a Chat target returns before this check runs.
var bridgeChatFieldPolicy = []bridgeFieldRule{
	keep("model"), keep("messages"), keep("stream"), keep("tools"), keep("tool_choice"),
	keep("temperature"), keep("top_p"), keep("max_tokens"), keep("max_completion_tokens"), keep("stop"),
	// Carried by the Responses converter, which is the target that expresses
	// them; the Messages converter leaves them behind, and a disposition states
	// the best case rather than the worst.
	keep("prompt_cache_key"), keep("parallel_tool_calls"),
	keep("store", wireResponses),
	drop("store", wireMessages),
	drop("reasoning_effort"), drop("prompt_cache_options"), drop("prompt_cache_retention"),
	drop("client_metadata"), drop("frequency_penalty"), drop("presence_penalty"), drop("seed"),
	drop("user"), drop("logprobs"), drop("top_logprobs"), drop("metadata"), drop("service_tier"),
	drop("stream_options"), drop("n"),
}

// structuralBridgeFieldNames are the names a structural check prints directly,
// rather than names that arrive as fields of the request body: tool kinds and
// content-block kinds read out of the request. They are listed so a rejection can
// name what it saw instead of falling back to the placeholder; the character-set
// check in redactedFeatureName still guards anything else.
const structuralBridgeFieldNames = "tools.function.strict tools.web_search tools.web_search_preview " +
	"tools.file_search tools.code_interpreter tools.computer tools.computer_use_preview tools.image_generation tools.mcp " +
	"tools.bash tools.text_editor tools.functionDeclarations tools.function_declarations " +
	"item_reference local_shell_call local_shell_call_output web_search_call image_generation_call " +
	"redacted_thinking compaction compaction_summary input_file file input_audio document video"

// structuralBridgeRuleNames name a rule rather than a field. They contain a space,
// so they cannot live in the whitespace-separated list above.
var structuralBridgeRuleNames = []string{"opaque context", "custom tool grammar"}

// legacyBridgeFieldNames were printable before the policy became one table and
// are kept printable so a rejection's wording does not drift for the clients that
// still send them.
const legacyBridgeFieldNames = "verbosity reasoning.effort reasoning.budget_tokens"

// clientFieldRule finds the rule governing one field of one client protocol.
func clientFieldRule(protocol, path string) (bridgeFieldRule, bool) {
	return bridgeFieldIn(bridgeClientFieldPolicy, protocol, path)
}

// chatFieldRule finds the rule governing one field of the intermediate Chat body
// for one target protocol.
func chatFieldRule(target, path string) (bridgeFieldRule, bool) {
	return bridgeFieldIn(bridgeChatFieldPolicy, target, path)
}

func bridgeFieldIn(policy []bridgeFieldRule, protocol, path string) (bridgeFieldRule, bool) {
	for _, rule := range policy {
		if rule.path != path || !rule.covers(protocol) {
			continue
		}
		return rule, true
	}
	return bridgeFieldRule{}, false
}

func (r bridgeFieldRule) covers(protocol string) bool {
	if len(r.protocols) == 0 {
		return true
	}
	return containsString(r.protocols, bridgeAny) || containsString(r.protocols, protocol)
}

// bridgeFieldRefusal reports whether a policy refuses a field, and what to print
// when it does. A field with no rule at all is refused as well: an unrecognized
// field must never silently disappear through a converter.
func bridgeFieldRefusal(policy []bridgeFieldRule, protocol, path string) (string, bool) {
	rule, ok := bridgeFieldIn(policy, protocol, path)
	if ok && rule.disposition != bridgeRefuse {
		return "", false
	}
	reason := "requires a native channel"
	if ok && rule.reason != "" {
		reason = rule.reason
	}
	return reason, true
}

// droppableBridgeField reports whether a nested field only shapes the answer, so
// that seeing it is not a reason to refuse the request. A path with no such rule
// leaves the decision to the caller.
func droppableBridgeField(path string) bool {
	for _, rule := range bridgeClientFieldPolicy {
		if rule.path == path && rule.disposition == bridgeDrop {
			return true
		}
	}
	return false
}

// bridgeRelaxableFields are the refusals a caller may accept losing for its own
// request.
//
// Dropping one of these degrades the answer instead of changing what the request
// means, so it is still refused by default: silently handing prose to a client
// that asked for JSON is worse than a 400 it can act on. But only the caller
// knows whether *its* parser can live with a degraded answer, and a channel-wide
// switch would make that decision for every client of the channel at once, so the
// relaxation is requested per request. The list stays short on purpose: a field
// belongs here only if losing it can be survived by the caller that asked to lose
// it.
var bridgeRelaxableFields = map[string]bool{"response_format": true}

// bridgeLossHeader names the fields a caller accepts losing. Its value is a
// comma-separated list, and a field outside bridgeRelaxableFields is ignored.
const bridgeLossHeader = "X-FusionGate-Accept-Lost-Contract"

// bridgeOptions is what a caller has explicitly accepted losing.
type bridgeOptions struct {
	allowedLosses map[string]bool
}

// bridgeOptionsForRequest reads the caller's own declaration. An unparsable or
// unknown list is simply empty: the safe default is the strict one, and a typo
// must not turn into a silently degraded answer.
func bridgeOptionsForRequest(r *http.Request) bridgeOptions {
	if r == nil {
		return bridgeOptions{}
	}
	values := r.Header.Values(bridgeLossHeader)
	if len(values) == 0 {
		return bridgeOptions{}
	}
	allowed := map[string]bool{}
	for _, value := range values {
		for _, field := range strings.Split(value, ",") {
			field = strings.ToLower(strings.TrimSpace(field))
			if bridgeRelaxableFields[field] {
				allowed[field] = true
			}
		}
	}
	return bridgeOptions{allowedLosses: allowed}
}

// acceptsLostField reports whether the caller accepted losing one field.
func (o bridgeOptions) acceptsLostField(field string) bool {
	return o.allowedLosses[field]
}

// knownBridgeFieldNames is the vocabulary a rejection may print verbatim: every
// declared field, every name a structural check produces, and the names that were
// printable before the policy became one table.
func knownBridgeFieldNames() map[string]bool {
	names := map[string]bool{}
	for _, policy := range [][]bridgeFieldRule{bridgeClientFieldPolicy, bridgeChatFieldPolicy} {
		for _, rule := range policy {
			names[rule.path] = true
		}
	}
	for _, list := range []string{structuralBridgeFieldNames, legacyBridgeFieldNames} {
		for _, name := range strings.Fields(list) {
			names[name] = true
		}
	}
	for _, name := range structuralBridgeRuleNames {
		names[name] = true
	}
	return names
}
