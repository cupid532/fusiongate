package fusiongate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// OpenCode Go asks every request to carry `x-opencode-session`, one stable ID
// per conversation, and from 2026-09-06 may reject requests without it. It also
// classifies requests by User-Agent, and a blank one lands in "Unknown client".
//
// FusionGate forwards a client's own header untouched. When the client sends
// none, the gateway derives one here so that the upstream still sees a stable
// per-conversation ID, in this order of preference:
//
//  1. a session header the client already uses with another vendor
//     (Codex CLI `session_id` / `conversation_id`, generic `x-session-id`);
//  2. a session ID embedded in the body: OpenAI `prompt_cache_key`,
//     Claude Code's `metadata.user_id` (`…_session_<uuid>`), `metadata.session_id`;
//  3. a fingerprint of the conversation itself — gateway credential + system
//     prompt + first user message — which every later turn of the same
//     conversation reproduces, so the ID stays stable without any state.
//
// Health-check probes and model discovery use a fixed per-provider ID so the
// upstream can recognise them as the gateway's own traffic.

const (
	openCodeSessionHeader = "x-opencode-session"
	openCodeSessionPrefix = "fg-"
)

// applyOpenCodeRequestHeaders is a no-op for every provider type except
// "opencode". `rawBody` is the body actually being sent upstream; for the
// fingerprint it only matters that the same conversation yields the same bytes
// for system prompt and first user message, which every conversion keeps.
func applyOpenCodeRequestHeaders(req *http.Request, z resolvedRoute, incoming *http.Request, rawBody []byte) {
	if req == nil || z.Provider.Type != "opencode" {
		return
	}
	if req.Header.Get(openCodeSessionHeader) == "" {
		if session := openCodeSessionID(incoming, rawBody); session != "" {
			req.Header.Set(openCodeSessionHeader, session)
		}
	}
	ensureFusionGateUserAgent(req.Header)
}

// setOpenCodeProbeHeaders labels the gateway's own health-check and discovery
// requests. Probes have no conversation, so the ID is the provider's.
func setOpenCodeProbeHeaders(header http.Header, providerID int64) {
	if header.Get(openCodeSessionHeader) == "" {
		header.Set(openCodeSessionHeader, "fusiongate-probe-"+strconv.FormatInt(providerID, 10))
	}
	ensureFusionGateUserAgent(header)
}

// ensureFusionGateUserAgent fills a blank User-Agent. The proxy deliberately
// blanks the header when the real client sent none (so net/http's synthetic
// Go-http-client never leaks); OpenCode wants *something* to classify, and
// naming the gateway is the honest answer.
func ensureFusionGateUserAgent(header http.Header) {
	if strings.TrimSpace(header.Get("User-Agent")) == "" {
		header.Set("User-Agent", "FusionGate/"+Version)
	}
}

func openCodeSessionID(incoming *http.Request, rawBody []byte) string {
	var header http.Header
	if incoming != nil {
		header = incoming.Header
	}
	if v := strings.TrimSpace(header.Get(openCodeSessionHeader)); v != "" {
		return v
	}
	for _, name := range []string{"session_id", "x-session-id", "conversation_id", "x-conversation-id"} {
		if v := strings.TrimSpace(header.Get(name)); v != "" {
			return v
		}
	}

	var body map[string]any
	if len(rawBody) > 0 {
		_ = json.Unmarshal(rawBody, &body)
	}
	if body != nil {
		if v, _ := body["prompt_cache_key"].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if metadata, _ := body["metadata"].(map[string]any); metadata != nil {
			if v, _ := metadata["session_id"].(string); strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, _ := metadata["user_id"].(string); v != "" {
				if session := claudeCodeSessionFromUserID(v); session != "" {
					return session
				}
			}
		}
	}

	system, firstUser := conversationFingerprint(body)
	h := sha256.New()
	h.Write([]byte(gatewayCredentialScope(header)))
	h.Write([]byte{0})
	h.Write([]byte(system))
	h.Write([]byte{0})
	h.Write([]byte(firstUser))
	if system == "" && firstUser == "" {
		// Nothing conversational to anchor on (an embeddings call, say): fall
		// back to the exact body so at least identical requests share an ID.
		h.Write([]byte{0})
		h.Write(rawBody)
	}
	return openCodeSessionPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// claudeCodeSessionFromUserID extracts the session UUID from Claude Code's
// `user_<hash>_account_<uuid>_session_<uuid>` metadata.user_id.
func claudeCodeSessionFromUserID(userID string) string {
	const marker = "_session_"
	idx := strings.LastIndex(userID, marker)
	if idx < 0 {
		return ""
	}
	session := strings.TrimSpace(userID[idx+len(marker):])
	if session == "" {
		return ""
	}
	return "claude-" + session
}

// gatewayCredentialScope keys the fingerprint to the caller's FusionGate
// credential, so two tenants opening a conversation with the same words never
// share an upstream session. Only a hash of the credential is used.
func gatewayCredentialScope(header http.Header) string {
	raw := strings.TrimSpace(header.Get("Authorization"))
	if raw == "" {
		raw = strings.TrimSpace(header.Get("x-api-key"))
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// conversationFingerprint pulls the system prompt and the first user message
// out of any of the request shapes the gateway accepts: OpenAI chat
// (`messages` with system/developer/user roles), Anthropic (`system` +
// `messages`), and Responses (`instructions` + `input`).
func conversationFingerprint(body map[string]any) (system, firstUser string) {
	if body == nil {
		return "", ""
	}
	system = fingerprintText(body["system"])
	if system == "" {
		system = fingerprintText(body["instructions"])
	}
	for _, key := range []string{"messages", "input"} {
		items, _ := body[key].([]any)
		for _, item := range items {
			message, _ := item.(map[string]any)
			if message == nil {
				continue
			}
			role, _ := message["role"].(string)
			text := fingerprintText(message["content"])
			switch role {
			case "system", "developer":
				if system == "" {
					system = text
				}
			case "user":
				if firstUser == "" {
					firstUser = text
				}
			}
			if firstUser != "" {
				break
			}
		}
		if firstUser != "" {
			break
		}
	}
	if firstUser == "" {
		if prompt, ok := body["input"].(string); ok {
			firstUser = prompt
		} else if prompt, ok := body["prompt"].(string); ok {
			firstUser = prompt
		}
	}
	return system, firstUser
}

// fingerprintText flattens a content value — a string, or a list of typed
// parts / blocks — to the text it carries. Non-text parts are ignored.
func fingerprintText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			switch p := part.(type) {
			case string:
				sb.WriteString(p)
			case map[string]any:
				if text, _ := p["text"].(string); text != "" {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	}
	return ""
}
