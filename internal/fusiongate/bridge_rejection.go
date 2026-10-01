package fusiongate

import (
	"errors"
	"sort"
	"strings"
)

// Capability diagnostics carry known field names and fixed explanations, never
// request values. Unknown client-controlled names are redacted before storage.
type bridgeCapabilityError struct {
	feature string
	reason  string
}

func (e *bridgeCapabilityError) Error() string {
	return "capability_not_supported: " + e.feature + " " + e.reason
}

// rejectBridgeFeature names the field a bridge cannot express. A known name uses
// a fixed spelling; a client's own field is kept when it is a bounded plain field
// path, so an operator can see what a new client started sending. Request values
// still never reach the diagnostics.
func rejectBridgeFeature(feature, reason string) error {
	const known = "model stream messages input instructions system contents tools tool_choice parallel_tool_calls " +
		"temperature top_p max_tokens max_completion_tokens max_output_tokens reasoning_effort stream_options " +
		"promptCacheKey prompt_cache_key prompt_cache_options prompt_cache_retention client_metadata store include text.verbosity " +
		"context_management previous_response_id " +
		"conversation truncation background audio modalities metadata user stop seed frequency_penalty presence_penalty " +
		"response_format service_tier verbosity logprobs top_logprobs reasoning reasoning.effort reasoning.summary " +
		"reasoning.generate_summary reasoning.budget_tokens tools.function.strict tools.web_search tools.web_search_preview " +
		"tools.file_search tools.code_interpreter tools.computer tools.computer_use_preview tools.image_generation tools.mcp " +
		"tools.bash tools.text_editor tools.functionDeclarations tools.function_declarations compaction compaction_summary " +
		"item_reference local_shell_call local_shell_call_output web_search_call image_generation_call redacted_thinking " +
		"thinking input_file file input_audio document video"
	if feature != "opaque context" && feature != "custom tool grammar" && !containsString(strings.Fields(known), feature) {
		feature = redactedFeatureName(feature)
	}
	return &bridgeCapabilityError{feature: feature, reason: reason}
}

// searchDeclinedExternalAccess reports whether a hosted search tool was declared
// without access to the open web. An absent or true value means the tool can reach
// out, so the bridge keeps refusing it.
func searchDeclinedExternalAccess(tool map[string]any) bool {
	value, ok := tool["external_web_access"]
	return ok && value == false
}

// redactedFeatureName keeps a plain dotted field path and falls back to the
// stable placeholder for anything else. A field name is not a value, and a
// bounded identifier cannot carry a credential or a prompt fragment, so naming it
// is what lets an operator answer "which field did this client add?" without
// guessing at a redacted report.
func redactedFeatureName(feature string) string {
	const fallback = "unrecognized_field"
	if feature == "" || len(feature) > 64 {
		return fallback
	}
	for _, r := range feature {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			return fallback
		}
	}
	return fallback + ":" + feature
}

func bridgeRejectionReason(err error) string {
	var capability *bridgeCapabilityError
	if errors.As(err, &capability) {
		return capability.Error()
	}
	return "capability_not_supported: invalid bridge request"
}

func bridgeFieldNames(body map[string]any) []string {
	fields := make([]string, 0, len(body))
	for field := range body {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}
