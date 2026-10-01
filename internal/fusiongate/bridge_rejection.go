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

func rejectBridgeFeature(feature, reason string) error {
	const known = "model stream messages input instructions system contents tools tool_choice parallel_tool_calls " +
		"temperature top_p max_tokens max_completion_tokens max_output_tokens reasoning_effort stream_options " +
		"promptCacheKey prompt_cache_key store include text.verbosity context_management previous_response_id " +
		"conversation truncation background audio modalities metadata user stop seed frequency_penalty presence_penalty " +
		"response_format service_tier verbosity logprobs top_logprobs reasoning reasoning.effort reasoning.summary " +
		"reasoning.generate_summary reasoning.budget_tokens tools.function.strict tools.web_search tools.web_search_preview " +
		"tools.file_search tools.code_interpreter tools.computer tools.computer_use_preview tools.image_generation tools.mcp " +
		"tools.bash tools.text_editor tools.functionDeclarations tools.function_declarations compaction compaction_summary " +
		"item_reference local_shell_call local_shell_call_output web_search_call image_generation_call redacted_thinking " +
		"thinking input_file file input_audio document video"
	if feature != "opaque context" && feature != "custom tool grammar" && !containsString(strings.Fields(known), feature) {
		feature = "unrecognized_field"
	}
	return &bridgeCapabilityError{feature: feature, reason: reason}
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
