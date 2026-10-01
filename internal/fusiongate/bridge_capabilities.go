package fusiongate

import (
	"encoding/json"
	"strings"
)

// Fields a Chat bridge cannot express but that only shape the answer, never what
// the request means. Dropping them still yields a working answer -- without a
// reasoning summary, without encrypted reasoning to replay, or with the
// upstream's own verbosity -- whereas rejecting them makes every chat-only
// channel unreachable from /v1/responses, because Codex sends all three on every
// request. This is the same degradation the sampling and length hints already
// take in validateBridgeRoute. State stays rejected: store, previous_response_id,
// conversation, hosted tools and replayed reasoning items change what the answer
// has to be, not just how it is shaped.
var bridgeDroppablePreferences = map[string]bool{
	"reasoning.summary":          true,
	"reasoning.generate_summary": true,
	"text.verbosity":             true,
}

func droppableBridgePreference(feature string) bool {
	return bridgeDroppablePreferences[feature]
}

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
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	reject := func(field string) error {
		return rejectBridgeFeature(field, "requires a native channel")
	}
	// Unrecognized fields must not silently disappear through a converter.
	allowed := "model stream messages tools tool_choice parallel_tool_calls temperature top_p max_tokens max_completion_tokens reasoning_effort stream_options promptCacheKey prompt_cache_key"
	switch client {
	case wireResponses:
		// The cache hints are dropped by the converter: Chat has no parameter for
		// how long an upstream retains a prompt cache, and the answer is unaffected.
		allowed = "model stream input instructions tools tool_choice parallel_tool_calls temperature top_p max_output_tokens reasoning text store prompt_cache_key include prompt_cache_options prompt_cache_retention"
	case wireMessages:
		allowed = "model stream messages system tools tool_choice temperature top_p max_tokens stop_sequences"
	case wireGemini:
		allowed = "contents systemInstruction generationConfig tools toolConfig"
	}
	for _, field := range bridgeFieldNames(body) {
		value := body[field]
		if value != nil && !containsString(strings.Fields(allowed), field) {
			return reject(field)
		}
	}
	if body["store"] == true {
		return reject("store")
	}
	if reasoning := asMap(body["reasoning"]); reasoning != nil {
		for _, field := range bridgeFieldNames(reasoning) {
			if field != "effort" && !droppableBridgePreference("reasoning."+field) {
				return reject("reasoning." + field)
			}
		}
	}
	for _, field := range []string{"context_management", "previous_response_id", "conversation", "truncation", "background", "audio", "modalities"} {
		if value, ok := body[field]; ok && value != nil && value != false && value != "" {
			return reject(field)
		}
	}
	if client == wireResponses {
		for _, value := range anySlice(body["include"]) {
			if !droppableBridgeInclude(asString(value)) {
				return reject("include")
			}
		}
		if text := asMap(body["text"]); text["verbosity"] != nil && !droppableBridgePreference("text.verbosity") {
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
			if kind != "function" && kind != "custom" {
				return reject("tools." + kind)
			}
			if kind == "custom" && asString(asMap(tool["format"])["type"]) != "text" && tool["format"] != nil {
				return reject("custom tool grammar")
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
			if v["encrypted_content"] != nil || v["signature"] != nil || v["thoughtSignature"] != nil || v["thought"] == true || v["cache_control"] != nil {
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
	chat, _, _, err := bridgeClientToChat(client, raw, path)
	if err != nil {
		return err
	}
	allowed := "model messages stream stream_options tools tool_choice parallel_tool_calls temperature top_p max_tokens max_completion_tokens reasoning_effort prompt_cache_key"
	switch target {
	case wireChat:
		return nil
	case wireMessages:
		allowed = "model messages stream stream_options tools tool_choice temperature top_p max_tokens max_completion_tokens stop"
	}
	for _, field := range bridgeFieldNames(chat) {
		value := chat[field]
		if value != nil && !containsString(strings.Fields(allowed), field) {
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
