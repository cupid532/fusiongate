package fusiongate

// isAnthropicProvider reports provider types that use the Anthropic Messages API.
func isAnthropicProvider(providerType string) bool {
	return providerType == "anthropic" || providerType == "anthropic_compatible"
}
