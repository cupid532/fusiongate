package fusiongate

import (
	"encoding/json"
	"strings"
)

// droppableBridgeInclude reports whether one `include` entry only asks for extra
// output. A bridged answer carries no reasoning at all, so every reasoning
// artifact a client can ask for is uniformly unavailable rather than silently
// wrong, while anything outside that family stays a hard rejection.
func droppableBridgeInclude(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "reasoning.")
}

// Native requests never enter here. A bridge must not pretend unsupported
// state, hosted tools or signed/opaque context can be represented as chat text.
func validateBridgeCapabilities(client string, raw []byte) error {
	return validateBridgeCapabilitiesFor(client, raw, bridgeOptions{})
}

// validateBridgeCapabilitiesFor applies the same policy with what the caller has
// explicitly accepted losing.
func validateBridgeCapabilitiesFor(client string, raw []byte, opts bridgeOptions) error {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	reject := func(field string) error {
		reason, refused := bridgeFieldRefusal(bridgeClientFieldPolicy, client, field)
		if !refused {
			// A field with a conditional rule -- a single choice, store=false --
			// is refused here by its value rather than by its disposition.
			reason = "requires a native channel"
		}
		return rejectBridgeFeature(field, reason)
	}
	// Every field's disposition is declared once, in bridge_field_policy.go. The
	// fields a protocol may send beyond its own shape are the ones that only shape
	// the answer -- sampling penalties, seeds, diagnostics, attribution, cache
	// hints -- so a converter that drops them still returns a usable answer. A
	// structured-output contract, a second choice and the state fields are refused
	// instead, because dropping those changes what the client gets. A field with
	// no rule is refused too, so nothing a new client invents disappears silently.
	//
	// A single choice is not a contract, which is why n is declared but its value
	// is checked here.
	if n, ok := body["n"]; ok && n != nil && n != float64(1) {
		return reject("n")
	}
	for _, field := range bridgeFieldNames(body) {
		if body[field] == nil {
			continue
		}
		if reason, refused := bridgeFieldRefusal(bridgeClientFieldPolicy, client, field); refused {
			// A loss the caller declared it can survive is taken, not refused.
			if opts.acceptsLostField(field) {
				continue
			}
			return rejectBridgeFeature(field, reason)
		}
	}
	if body["store"] == true {
		return reject("store")
	}
	if reasoning := asMap(body["reasoning"]); reasoning != nil {
		for _, field := range bridgeFieldNames(reasoning) {
			if field != "effort" && !droppableBridgeField("reasoning."+field) {
				return reject("reasoning." + field)
			}
		}
	}
	if client == wireResponses {
		for _, value := range anySlice(body["include"]) {
			if !droppableBridgeInclude(asString(value)) {
				return reject("include")
			}
		}
		if text := asMap(body["text"]); text["verbosity"] != nil && !droppableBridgeField("text.verbosity") {
			return reject("text.verbosity")
		}
	}
	for _, value := range anySlice(body["tools"]) {
		tool := asMap(value)
		kind := asString(tool["type"])
		switch client {
		case wireMessages:
			if kind != "" && kind != "custom" {
				return reject("tools." + kind)
			}
		case wireGemini:
			for _, key := range bridgeFieldNames(tool) {
				if key != "functionDeclarations" && key != "function_declarations" {
					return reject("tools." + key)
				}
			}
		default:
			switch kind {
			case "function":
			case "custom":
				// A custom tool becomes a function taking one string argument. A
				// grammar cannot be enforced by a Chat upstream, so its definition is
				// carried into the description and the model is asked to follow it;
				// any other format would be dropped without a trace.
				switch asString(asMap(tool["format"])["type"]) {
				case "", "text", "grammar":
				default:
					return reject("custom tool grammar")
				}
			case "namespace", "tool_search":
				// A grouped or deferred declaration belongs to the client that made
				// it: the bridge cannot know the name the client registered a member
				// tool under, and inventing one could route a call to the wrong tool.
				// Dropping the declaration is always safe, because the model only
				// calls a tool that was declared, so the request still works with
				// fewer tools instead of being refused.
			case "web_search":
				// A search the client already excluded from external access has no
				// capability left to lose; one that can reach the web does, and a
				// bridge cannot provide it.
				if !searchDeclinedExternalAccess(tool) {
					return reject("tools." + kind)
				}
			default:
				return reject("tools." + kind)
			}
		}
	}
	var inspect func(any) error
	inspect = func(value any) error {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				if err := inspect(item); err != nil {
					return err
				}
			}
		case map[string]any:
			kind := asString(v["type"])
			switch kind {
			case "compaction", "compaction_summary", "item_reference", "local_shell_call", "local_shell_call_output", "web_search_call", "image_generation_call", "redacted_thinking", "reasoning", "thinking", "input_file", "file", "input_audio", "audio", "document", "video":
				return reject(kind)
			}
			// cache_control is a cache breakpoint marker, not state: dropping it only
			// changes how the upstream caches, so an Anthropic-style client keeps
			// working through a bridge. Signature-bearing blocks stay refused.
			if v["encrypted_content"] != nil || v["signature"] != nil || v["thoughtSignature"] != nil || v["thought"] == true {
				return reject("opaque context")
			}
			for _, field := range bridgeFieldNames(v) {
				if err := inspect(v[field]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []string{"input", "messages", "contents", "system", "systemInstruction"} {
		if err := inspect(body[field]); err != nil {
			return err
		}
	}
	return nil
}

// Validate both legs: the intermediate Chat shape alone does not establish
// that the destination can represent all of its fields.
//
// The answer depends only on the two protocols and the request, never on which
// channel will serve it: a conversion that loses data is refused for every
// channel, and one that does not is allowed for every channel.
func validateBridgeRoute(client, target string, raw []byte, path string) error {
	return validateBridgeRouteFor(client, target, raw, path, bridgeOptions{})
}

// validateBridgeRouteFor validates both legs, with what the caller has explicitly
// accepted losing applied to both of them.
func validateBridgeRouteFor(client, target string, raw []byte, path string, opts bridgeOptions) error {
	chat, _, _, err := bridgeClientToChatFor(client, raw, path, opts)
	if err != nil {
		return err
	}
	if target == wireChat {
		return nil
	}
	// A Responses or Messages target has no equivalent for the OpenAI-side hints,
	// and dropping them still produces a usable answer: Anthropic offers explicit
	// cache breakpoints instead of a cache key, allows parallel tool use by
	// default, and takes an explicit thinking budget instead of a reasoning
	// effort. What must not be dropped -- a structured-output contract, a second
	// choice -- is refused through the same policy that checks the client request,
	// so the two legs of a bridge cannot disagree about a field.
	for _, field := range bridgeFieldNames(chat) {
		if chat[field] == nil {
			continue
		}
		if _, refused := bridgeFieldRefusal(bridgeChatFieldPolicy, target, field); refused {
			if opts.acceptsLostField(field) {
				continue
			}
			return rejectBridgeFeature(field, "cannot be preserved by "+target)
		}
	}
	// Sampling and length hints are deliberately dropped for the ChatGPT Codex
	// backend, which rejects them. Rejecting the whole route here instead would
	// make a healthy auth-file channel unreachable from the Chat endpoint for
	// every client that sends ordinary parameters, even though the converter
	// below already handles their absence. The native Responses path takes the
	// same decision for the same field (see normalizedCodexResponsesBody), so a
	// parameter-rich request degrades to a working answer rather than an error.
	if target == wireMessages {
		for _, value := range anySlice(chat["tools"]) {
			if asMap(asMap(value)["function"])["strict"] != nil {
				return rejectBridgeFeature("tools.function.strict", "requires a compatible channel")
			}
		}
	}
	return nil
}

// capabilityWireProtocol maps the protocol name used in a `protocol:<name>`
// capability onto the wire protocol it names. OpenCode's model table spells the
// Anthropic (Messages) protocol "anthropic" while FusionGate's own records and
// the discovery demotion use "messages", so both spellings have to be read.
func capabilityWireProtocol(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case wireChat:
		return wireChat
	case wireResponses:
		return wireResponses
	case wireMessages, "anthropic", "claude":
		return wireMessages
	case wireGemini:
		return wireGemini
	default:
		return ""
	}
}

// routeServesProtocol reports whether a route's own capability evidence names a
// protocol.
//
// It is an ordering signal, never a filter: a declaration tells the bridge which
// target the operator wants tried first, and only a learned fact -- real evidence,
// with a lifetime -- excludes a target. Treating an absent name as "this route
// serves nothing else" would let a declaration silently take away a working
// bridge target, which is the opposite of what a declaration means.
//
// Only an explicit `protocol:<name>` entry counts as evidence. The capability list
// of an ordinary route describes the model (chat, stream, tools, reasoning) and
// says nothing about protocols.
func routeServesProtocol(z resolvedRoute, protocol string) bool {
	listed := false
	for capabilities := z.Route.Capabilities; capabilities != ""; {
		var capability string
		capability, capabilities = nextListItem(capabilities)
		name, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(capability)), "protocol:")
		if !ok {
			continue
		}
		listed = true
		if capabilityWireProtocol(name) == protocol {
			return true
		}
	}
	return !listed
}
