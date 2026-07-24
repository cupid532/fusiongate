package fusiongate

import (
	"net/http"
	"os"
	"strings"
)

// The ChatGPT Codex backend validates CLI compatibility metadata separately
// from the OAuth bearer token. Keep the version configurable so deployments
// can follow an upstream minimum-version change without rebuilding FusionGate.
const defaultCodexCLIVersion = "0.129.0"

func codexCLIVersion() string {
	if value := strings.TrimSpace(os.Getenv("FUSIONGATE_CODEX_CLI_VERSION")); value != "" {
		return value
	}
	return defaultCodexCLIVersion
}

func setCodexClientHeaders(header http.Header) {
	version := codexCLIVersion()
	header.Set("originator", "codex_cli_rs")
	header.Set("User-Agent", "codex_cli_rs/"+version)
}
