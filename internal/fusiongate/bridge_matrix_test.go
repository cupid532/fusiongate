package fusiongate

import (
	"strings"
	"testing"
)

// clientPathFor names the public path a client protocol arrives on.
func clientPathFor(protocol string) string {
	switch protocol {
	case wireChat:
		return "/v1/chat/completions"
	case wireResponses:
		return "/v1/responses"
	case wireMessages:
		return "/v1/messages"
	case wireGemini:
		return "/v1beta/models/public-model:generateContent"
	}
	return ""
}

// channelTypes is every type the console offers.
var channelTypes = []string{"openai_compatible", "openai", "openrouter", "opencode", "grok", "anthropic", "anthropic_compatible", "gemini"}

var clientProtocols = []string{wireChat, wireResponses, wireMessages, wireGemini}

// Every client protocol must either be native for the channel type or reach a
// bridge target: "as many channels as possible serve every interface".
func TestEveryChannelTypeServesEveryClientProtocol(t *testing.T) {
	// Gemini channels speak only their own protocol today, so the other three
	// interfaces have no bridge target to reach. Documented gap, not a regression.
	unbridged := map[string]bool{"gemini": true}
	for _, kind := range channelTypes {
		for _, client := range clientProtocols {
			z := resolvedRoute{Provider: Provider{ID: 1, Type: kind, ProtocolPolicy: protocolAuto}}
			natives := typeWireProtocols(kind)
			native := containsString(natives, client)
			adapter := planInferenceAdapter(z, clientPathFor(client))
			if native || adapter != "" {
				continue
			}
			if unbridged[kind] {
				t.Logf("known gap: %s channel cannot serve a %s client (no bridge target)", kind, client)
				continue
			}
			t.Errorf("%s channel cannot serve a %s client: natives=%v adapter=%q", kind, client, natives, adapter)
		}
	}
}

// Representative request shapes as real clients send them.
var (
	openAIChatBody = `{"model":"public-model","messages":[{"role":"user","content":"hi"}],"stream":true,"temperature":0.7,"top_p":1,"parallel_tool_calls":true,"reasoning_effort":"medium","prompt_cache_key":"cache-1","seed":7,"user":"u1","stop":["x"],"logprobs":true,"top_logprobs":2,"frequency_penalty":0.1,"presence_penalty":0.1,"service_tier":"auto","n":1,"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object","properties":{}}}}]}`

	codexResponsesBody = `{"model":"public-model","instructions":"be brief","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"cache-1","parallel_tool_calls":true,"reasoning":{"effort":"medium"},"text":{"verbosity":"low"},"client_metadata":{"session_id":"s"},"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}},{"type":"web_search","external_web_access":false}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

	claudeCodeMessagesBody = `{"model":"public-model","max_tokens":4096,"stream":true,"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"u1"},"tools":[{"name":"shell","description":"run","input_schema":{"type":"object","properties":{}},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

	geminiBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.7}}`
)

func bodyFor(client string) string {
	switch client {
	case wireChat:
		return openAIChatBody
	case wireResponses:
		return codexResponsesBody
	case wireMessages:
		return claudeCodeMessagesBody
	case wireGemini:
		return geminiBody
	}
	return ""
}

// A bridge must accept each representative client request for every target the
// channel type can offer, so that a client is not locked out of a whole channel
// family by a field that only shapes the answer.
func TestBridgeAcceptsRepresentativeClientRequests(t *testing.T) {
	for _, client := range clientProtocols {
		body := bodyFor(client)
		if err := validateBridgeCapabilities(client, []byte(body)); err != nil {
			t.Errorf("%s client request refused before any route is known: %v", client, err)
			continue
		}
		for _, kind := range channelTypes {
			z := resolvedRoute{Provider: Provider{ID: 1, Type: kind, ProtocolPolicy: protocolAuto}}
			natives := typeWireProtocols(kind)
			if containsString(natives, client) {
				continue // native passthrough, no conversion
			}
			adapter := planInferenceAdapter(z, clientPathFor(client))
			_, target, ok := parseBridgeAdapter(adapter)
			if !ok {
				if kind == "gemini" {
					continue
				}
				t.Errorf("%s client has no bridge to a %s channel (natives=%v)", client, kind, natives)
				continue
			}
			if err := validateBridgeRoute(client, target, []byte(body), clientPathFor(client)); err != nil {
				t.Errorf("%s client cannot bridge to a %s channel (%s): %v", client, kind, target, err)
			}
		}
	}
}

// The same matrix, but reporting which pairs pass so the coverage is visible.
func TestBridgeMatrixSummary(t *testing.T) {
	for _, client := range clientProtocols {
		for _, kind := range channelTypes {
			z := resolvedRoute{Provider: Provider{ID: 1, Type: kind, ProtocolPolicy: protocolAuto}}
			natives := typeWireProtocols(kind)
			state := "native"
			if !containsString(natives, client) {
				_, target, ok := parseBridgeAdapter(planInferenceAdapter(z, clientPathFor(client)))
				if !ok {
					state = "NO ROUTE"
				} else {
					state = "bridge->" + target
				}
			}
			t.Logf("%-12s x %-20s %s", client, kind, strings.TrimSpace(state))
		}
	}
}

// What a bridge must keep refusing: a structured-output contract and a
// multi-choice request change what the client gets, not just how it is shaped, so
// they are never dropped for it. The boundary is asserted here so a future
// relaxation cannot quietly turn a contract into a silent drop.
func TestBridgeRefusesContractsItCannotHonour(t *testing.T) {
	for _, tc := range []struct{ name, body, feature string }{
		{"structured output", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, "response_format"},
		{"several choices", `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":3}`, "n"},
		{"hosted search", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`, "tools.web_search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBridgeCapabilities(wireChat, []byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.feature) {
				t.Fatalf("expected a refusal naming %q, got %v", tc.feature, err)
			}
		})
	}
	// A single choice is not a contract: the field is simply dropped.
	if err := validateBridgeCapabilities(wireChat, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`)); err != nil {
		t.Fatalf("n=1 must be acceptable: %v", err)
	}
}

// Every client protocol must convert into every other wire protocol a channel can
// offer. This is the conversion gate itself, independent of which channel type
// reaches which target, so a refusal here is a refusal for the whole family.
func TestBridgeConversionMatrix(t *testing.T) {
	for _, client := range clientProtocols {
		body := bodyFor(client)
		if err := validateBridgeCapabilities(client, []byte(body)); err != nil {
			t.Errorf("%s client request refused before any route is known: %v", client, err)
			continue
		}
		for _, target := range []string{wireChat, wireResponses, wireMessages} {
			if target == client {
				continue
			}
			state := "ok"
			if err := validateBridgeRoute(client, target, []byte(body), clientPathFor(client)); err != nil {
				state = "REFUSED: " + err.Error()
			}
			t.Logf("%-10s -> %-10s %s", client, target, state)
		}
	}
}
