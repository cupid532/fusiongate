package fusiongate

import (
	"net/url"
	"strings"
)

// ledgerUpstreamName identifies only known direct endpoints. A reseller's
// ultimate source cannot be established from its model name or API format.
func ledgerUpstreamName(address, providerName string) string {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return "未知"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "api.openai.com", host == "chatgpt.com":
		return "OpenAI"
	case host == "api.anthropic.com":
		return "Anthropic"
	case host == "generativelanguage.googleapis.com":
		return "Google Gemini"
	case strings.HasSuffix(host, ".openai.azure.com"):
		return "Azure OpenAI"
	case host == "api.x.ai":
		return "xAI"
	case host == "openrouter.ai":
		return "OpenRouter"
	case host == "api.deepseek.com":
		return "DeepSeek"
	}
	name := strings.TrimSpace(providerName)
	// Channel names occasionally contain an address. Never render one as a name.
	if name == "" || strings.Contains(name, "://") || strings.ContainsAny(name, "/@") || strings.Contains(name, ".") {
		return "未知"
	}
	return name
}

// ledgerUpstreamAddress records the direct upstream base address, never URL
// credentials, query tokens or fragments. Do not backfill from mutable providers.
func ledgerUpstreamAddress(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
