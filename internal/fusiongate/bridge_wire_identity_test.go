package fusiongate

import (
	"strings"
	"testing"
	"time"
)

// wireIdentityRoute is a resolved route whose credential is an OAuth token,
// which is the shape whose learning used to be thrown away on every refresh.
func wireIdentityRoute() resolvedRoute {
	return resolvedRoute{
		Route:      Route{ID: 1, UpstreamModel: "upstream-model", Capabilities: "chat,stream"},
		Provider:   Provider{ID: 7, Type: "openai_compatible", BaseURL: "https://relay.example", ProtocolPolicy: protocolAuto},
		Credential: "access-token-a",
		AuthCredential: &ProviderCredential{
			Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
			AccessToken: "access-token-a", AccountID: "acct-1", Email: "user@example.com",
		},
	}
}

func resetProtocolMemory() {
	protocolMemory = &wireMemory{entries: map[string]time.Time{}}
}

// A refreshed access token must not erase what the channel taught us. Keying the
// memory on the token itself meant the entry written before a refresh could not
// be looked up after it, so a chat-only channel paid for the same doomed native
// attempt again on the first request following every refresh.
func TestWireMemorySurvivesTokenRefresh(t *testing.T) {
	resetProtocolMemory()
	z := wireIdentityRoute()
	protocolMemory.remember(z, wireResponses)

	refreshed := z
	refreshed.Credential = "access-token-b"
	refreshed.AuthCredential = &ProviderCredential{
		Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth",
		AccessToken: "access-token-b", RefreshToken: "same-refresh", AccountID: "acct-1", Email: "user@example.com",
	}
	if !protocolMemory.unsupported(refreshed, wireResponses) {
		t.Fatal("a token refresh must not invalidate what the channel already proved")
	}
}

// The same refresh, observed at the planning layer: the second request must go
// straight to the bridge instead of probing the native endpoint again.
func TestPlanStaysBridgedAfterTokenRefresh(t *testing.T) {
	resetProtocolMemory()
	z := wireIdentityRoute()
	protocolMemory.remember(z, wireResponses)

	refreshed := z
	refreshed.Credential = "access-token-b"
	if adapter := planInferenceAdapter(refreshed, "/v1/responses"); adapter != bridgeAdapter(wireResponses, wireChat) {
		t.Fatalf("adapter after refresh = %q, want %q", adapter, bridgeAdapter(wireResponses, wireChat))
	}
}

// Accounts of one channel keep separate learning: a second subscription can
// carry different entitlements, so it must not inherit the first one's verdict.
func TestWireMemorySeparatesAccounts(t *testing.T) {
	resetProtocolMemory()
	z := wireIdentityRoute()
	protocolMemory.remember(z, wireResponses)

	other := z
	other.AuthCredential = &ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", AccountID: "acct-2", AccessToken: "access-token-c"}
	if protocolMemory.unsupported(other, wireResponses) {
		t.Fatal("a second account must not inherit another account's learning")
	}
}

// A Key-backed route is identified by its Key row, which survives both token
// refreshes and credential re-imports.
func TestWireMemorySeparatesProviderKeys(t *testing.T) {
	resetProtocolMemory()
	z := wireIdentityRoute()
	z.ProviderKeyID = 11
	protocolMemory.remember(z, wireResponses)

	rotated := z
	rotated.Credential = "access-token-b"
	if !protocolMemory.unsupported(rotated, wireResponses) {
		t.Fatal("a rotated token must not invalidate a Key's learning")
	}
	sibling := z
	sibling.ProviderKeyID = 12
	sibling.Credential = "access-token-a"
	if protocolMemory.unsupported(sibling, wireResponses) {
		t.Fatal("a different Key of the same provider must keep its own learning")
	}
}

func TestWireIdentityCarriesNoCredential(t *testing.T) {
	z := wireIdentityRoute()
	z.Credential = "sk-live-secret-token"
	z.AuthCredential = &ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", AccessToken: "sk-live-secret-token", RefreshToken: "sk-live-secret-refresh", IDToken: "sk-live-secret-id"}
	for name, value := range map[string]string{
		"identity":   wireCredentialIdentity(z),
		"memory key": wireMemoryKey(z, wireResponses),
		"chat key":   wireMemoryKey(z, wireChat),
	} {
		if strings.Contains(value, "secret") {
			t.Fatalf("%s leaked a credential: %q", name, value)
		}
	}
}

// A route that names its protocol constrains which protocol a bridge may convert
// into: converting into one the model does not serve costs a whole upstream call
// to learn what discovery already recorded.
func TestBridgeTargetHonoursRouteProtocolEvidence(t *testing.T) {
	resetProtocolMemory()
	z := resolvedRoute{
		Route:    Route{ID: 1, UpstreamModel: "upstream-model", Capabilities: "protocol:responses,stream"},
		Provider: Provider{ID: 1, Type: "openai_compatible", ProtocolPolicy: protocolAuto},
	}
	natives := typeWireProtocols("openai_compatible")
	if got := bridgeTarget(z, wireChat, natives); got != bridgeAdapter(wireChat, wireResponses) {
		t.Fatalf("chat client bridge target = %q, want responses", got)
	}
	// The route rules Messages out but does not rule Chat in: a Responses client
	// has no target the route claims to serve, so the honest answer is to fail
	// over rather than convert into a protocol the model may not speak.
	if got := bridgeTarget(z, wireResponses, natives); got != "" {
		t.Fatalf("responses client bridge target = %q, want no bridge", got)
	}
	only := z
	only.Route.Capabilities = "protocol:responses"
	if got := bridgeTarget(only, wireResponses, natives); got != "" {
		t.Fatalf("a route that serves only responses must not bridge away from it: %q", got)
	}
}

// OpenCode's model table spells the Messages protocol "anthropic". A route
// annotated that way must still be reachable from a Responses client.
func TestBridgeTargetReadsAnthropicProtocolEvidence(t *testing.T) {
	resetProtocolMemory()
	z := resolvedRoute{
		Route:    Route{ID: 1, UpstreamModel: "claude-x", Capabilities: "protocol:anthropic"},
		Provider: Provider{ID: 1, Type: "openai_compatible", ProtocolPolicy: protocolAuto},
	}
	natives := []string{wireChat, wireResponses, wireMessages}
	if got := bridgeTarget(z, wireResponses, natives); got != bridgeAdapter(wireResponses, wireMessages) {
		t.Fatalf("bridge target = %q, want messages", got)
	}
}

// A capability list that names no protocol is not evidence: the channel type
// stays the only guide, which is how every route behaved before capabilities
// carried protocols. Reading it as "serves nothing" would deny every bridge.
func TestRouteWithoutProtocolEvidenceKeepsTypePreference(t *testing.T) {
	resetProtocolMemory()
	z := resolvedRoute{
		Route:    Route{ID: 1, UpstreamModel: "upstream-model", Capabilities: "chat,stream,tools"},
		Provider: Provider{ID: 1, Type: "openai_compatible", ProtocolPolicy: protocolAuto},
	}
	for _, protocol := range []string{wireChat, wireResponses, wireMessages} {
		if !routeServesProtocol(z, protocol) {
			t.Fatalf("%s must stay allowed when no protocol is declared", protocol)
		}
	}
	if got := bridgeTarget(z, wireMessages, typeWireProtocols("openai_compatible")); got != bridgeAdapter(wireMessages, wireChat) {
		t.Fatalf("bridge target = %q, want the type's first preference", got)
	}
}
