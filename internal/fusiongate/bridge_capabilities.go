package fusiongate

import (
	"encoding/json"
	"fmt"
)

// Native requests never enter here. A bridge must not pretend unsupported
// state, hosted tools or signed/opaque context can be represented as chat text.
func validateBridgeCapabilities(client string, raw []byte) error {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	reject := func(field string) error {
		return fmt.Errorf("capability_not_supported: %s requires a native channel", field)
	}
	for _, field := range []string{"context_management", "previous_response_id", "conversation", "truncation", "background", "audio", "modalities"} {
		if value, ok := body[field]; ok && value != nil && value != false && value != "" {
			return reject(field)
		}
	}
	if client == wireResponses {
		if len(anySlice(body["include"])) > 0 {
			return reject("include")
		}
		if text := asMap(body["text"]); text["verbosity"] != nil {
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
			for key := range tool {
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
			case "compaction", "compaction_summary", "item_reference", "local_shell_call", "local_shell_call_output", "web_search_call", "image_generation_call", "redacted_thinking":
				return reject(kind)
			}
			if v["encrypted_content"] != nil || v["signature"] != nil {
				return reject("opaque context")
			}
			for _, item := range v {
				if err := inspect(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, field := range []string{"input", "messages", "contents"} {
		if err := inspect(body[field]); err != nil {
			return err
		}
	}
	return nil
}
