package fusiongate

import (
	"net/http"
	"os"
	"strings"
)

// The Grok OAuth gateway validates the client metadata independently of the
// OAuth bearer token. FusionGate is an API gateway, so it must provide the
// same compatibility metadata when it calls cli-chat-proxy.grok.com.
//
// Pin a tested version from https://x.ai/cli/stable. Older client versions
// can receive HTTP 426 even with a valid OAuth token.
const defaultGrokCLIVersion = "1.0.89"
const defaultGrokClientIdentifier = "grok-shell"

func grokCLIVersion() string {
	if value := strings.TrimSpace(os.Getenv("FUSIONGATE_GROK_CLI_VERSION")); value != "" {
		return value
	}
	return defaultGrokCLIVersion
}

func grokClientIdentifier() string {
	if value := strings.TrimSpace(os.Getenv("FUSIONGATE_GROK_CLIENT_IDENTIFIER")); value != "" {
		return value
	}
	return defaultGrokClientIdentifier
}

func setGrokClientHeaders(header http.Header) {
	version := grokCLIVersion()
	header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	header.Set("x-grok-client-version", version)
	header.Set("x-grok-client-identifier", grokClientIdentifier())
	header.Set("x-grok-client-mode", "headless")
	header.Set("User-Agent", "grok-shell/"+version+" (linux; x86_64)")
}
