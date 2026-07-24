package fusiongate

import (
	"net/http"
	"os"
	"strings"
)

// The Grok OAuth gateway validates the client metadata independently of the
// OAuth bearer token. FusionGate is an API gateway, so it must provide the
// same compatibility metadata when it calls cli-chat-proxy.grok.com.
const defaultGrokCLIVersion = "0.2.111"

func grokCLIVersion() string {
	if value := strings.TrimSpace(os.Getenv("FUSIONGATE_GROK_CLI_VERSION")); value != "" {
		return value
	}
	return defaultGrokCLIVersion
}

func setGrokClientHeaders(header http.Header) {
	version := grokCLIVersion()
	header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	header.Set("x-grok-client-version", version)
	header.Set("User-Agent", "xai-grok-workspace/"+version)
}
