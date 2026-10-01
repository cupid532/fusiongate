package fusiongate

import (
	"errors"
	"sort"
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
//
// The vocabulary of known names is derived from the field policy itself, so a
// field can no longer be declared for acceptance yet be missing from the list of
// printable names -- which is what used to turn a real, named field into a
// redacted placeholder and hide what a client had just started sending.
func rejectBridgeFeature(feature, reason string) error {
	if !knownBridgeFieldNames()[feature] {
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
