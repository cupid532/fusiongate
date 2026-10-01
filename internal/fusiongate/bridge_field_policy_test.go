package fusiongate

import (
	"encoding/json"
	"strings"
	"testing"
)

// policyFieldValue is a value that passes every structural check a field might
// also be subject to, so a table entry is judged by its own disposition alone.
func policyFieldValue(path string) any {
	switch path {
	case "store", "parallel_tool_calls", "stream", "logprobs":
		return false
	case "n":
		return 1
	case "top_logprobs", "seed", "temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "frequency_penalty", "presence_penalty":
		return 1
	case "tools", "messages", "input", "include", "system", "stop", "stop_sequences", "contents":
		return []any{}
	case "reasoning":
		return map[string]any{"effort": "high"}
	case "text":
		return map[string]any{"verbosity": "low"}
	}
	return map[string]any{"present": true}
}

func policyBody(path string) []byte {
	body := map[string]any{"model": "m", path: policyFieldValue(path)}
	if path == "model" {
		body = map[string]any{"model": "m"}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return encoded
}

// Every rule must be reachable and unambiguous: a field declared twice for one
// protocol would be decided by whichever entry happens to come first.
func TestBridgePolicyHasNoAmbiguousRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  []bridgeFieldRule
		probes  []string
		allowed bool
	}{
		{"client", bridgeClientFieldPolicy, clientProtocols, false},
		{"chat body", bridgeChatFieldPolicy, []string{wireResponses, wireMessages}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := append(tc.probes, bridgeAny, "unknown-protocol")
			seen := map[string]string{}
			for _, rule := range tc.policy {
				for _, protocol := range probes {
					if !rule.covers(protocol) {
						continue
					}
					key := protocol + "\x00" + rule.path
					if previous, ok := seen[key]; ok {
						t.Fatalf("%s/%s declared twice: %s and %s", protocol, rule.path, previous, rule.disposition)
					}
					seen[key] = "rule"
				}
			}
		})
	}
}

// A field that only shapes the answer must be accepted for every protocol its
// rule names; a field that changes what the client gets must be refused by name.
func TestBridgePolicyMatchesItsDispositions(t *testing.T) {
	for _, rule := range bridgeClientFieldPolicy {
		protocols := rule.protocols
		if len(protocols) == 0 {
			protocols = clientProtocols
		}
		for _, protocol := range protocols {
			err := validateBridgeCapabilities(protocol, policyBody(rule.path))
			switch rule.disposition {
			case bridgeKeep, bridgeDrop:
				if err != nil {
					t.Errorf("%s field %s must be accepted: %v", protocol, rule.path, err)
				}
			case bridgeRefuse:
				if err == nil {
					t.Errorf("%s field %s must stay refused", protocol, rule.path)
				} else if !strings.Contains(err.Error(), rule.path) {
					t.Errorf("%s field %s refusal does not name it: %v", protocol, rule.path, err)
				}
			}
		}
	}
}

// A field nobody declared is refused, and reported as an unrecognized name
// rather than silently dropped through a converter.
func TestUndeclaredFieldIsRefusedByName(t *testing.T) {
	for _, protocol := range clientProtocols {
		err := validateBridgeCapabilities(protocol, policyBody("brand_new_option"))
		if err == nil {
			t.Fatalf("%s: an undeclared field must be refused", protocol)
		}
		if !strings.Contains(err.Error(), "unrecognized_field:brand_new_option") {
			t.Fatalf("%s: refusal does not name the field: %v", protocol, err)
		}
	}
}

// The name vocabulary is derived from the policy, so a declared field can never
// be printable in one place and redacted in another. That drift is what hid
// which field a client had started sending.
func TestEveryDeclaredFieldIsPrintable(t *testing.T) {
	known := knownBridgeFieldNames()
	for _, policy := range [][]bridgeFieldRule{bridgeClientFieldPolicy, bridgeChatFieldPolicy} {
		for _, rule := range policy {
			if !known[rule.path] {
				t.Fatalf("declared field %q cannot be printed in a rejection", rule.path)
			}
		}
	}
	for _, rule := range bridgeClientFieldPolicy {
		if rule.disposition == bridgeRefuse && rule.reason == "" {
			t.Fatalf("refusal of %q has no reason to print", rule.path)
		}
	}
}

// The intermediate Chat body follows the same criterion, so a target cannot
// accept something the client leg refused.
func TestChatBodyPolicyAcceptsWhatTheTargetsExpress(t *testing.T) {
	for _, target := range []string{wireResponses, wireMessages} {
		for _, rule := range bridgeChatFieldPolicy {
			_, refused := bridgeFieldRefusal(bridgeChatFieldPolicy, target, rule.path)
			if refused != (rule.disposition == bridgeRefuse) {
				t.Fatalf("%s target treats %q inconsistently", target, rule.path)
			}
		}
	}
}
