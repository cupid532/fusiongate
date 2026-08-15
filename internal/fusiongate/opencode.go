package fusiongate

import "strings"

const (
	opencodeProtocolChat      = "chat"
	opencodeProtocolResponses = "responses"
	opencodeProtocolAnthropic = "anthropic"
	opencodeProtocolGemini    = "gemini"
)

func opencodeModelProtocol(model, capabilities string) string {
	for capabilities != "" {
		var capability string
		capability, capabilities = nextListItem(capabilities)
		if protocol := strings.TrimPrefix(strings.ToLower(capability), "protocol:"); protocol != capability {
			switch protocol {
			case opencodeProtocolChat, opencodeProtocolResponses, opencodeProtocolAnthropic, opencodeProtocolGemini:
				return protocol
			}
		}
	}
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "grok-"), strings.HasPrefix(model, "muse-"):
		return opencodeProtocolResponses
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "qwen"):
		return opencodeProtocolAnthropic
	case strings.HasPrefix(model, "gemini-"):
		return opencodeProtocolGemini
	default:
		return opencodeProtocolChat
	}
}

func opencodeRouteProtocol(route resolvedRoute) string {
	return opencodeModelProtocol(route.Route.UpstreamModel, route.Route.Capabilities)
}

func withOpenCodeProtocol(capabilities, model string) string {
	protocol := "protocol:" + opencodeModelProtocol(model, "")
	if matchesCapability(capabilities, protocol) {
		return capabilities
	}
	if strings.TrimSpace(capabilities) == "" {
		return protocol
	}
	return capabilities + "," + protocol
}
