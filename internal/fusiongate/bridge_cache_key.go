package fusiongate

// normalizeChatCacheKey accepts the alias emitted by OpenCode's compatible
// SDK. It runs only for bridged Chat requests, leaving native bytes untouched.
// Null means absent; an explicit empty string remains an explicit empty string.
func normalizeChatCacheKey(body map[string]any) error {
	canonical := body["prompt_cache_key"]
	alias := body["promptCacheKey"]
	for _, field := range []string{"prompt_cache_key", "promptCacheKey"} {
		if value := body[field]; value != nil {
			if _, ok := value.(string); !ok {
				return rejectBridgeFeature(field, "must be a string")
			}
		}
	}
	if alias != nil && canonical != nil && alias != canonical {
		return rejectBridgeFeature("promptCacheKey", "conflicts with prompt_cache_key")
	}
	if alias != nil && canonical == nil {
		body["prompt_cache_key"] = alias
	}
	delete(body, "promptCacheKey")
	return nil
}
