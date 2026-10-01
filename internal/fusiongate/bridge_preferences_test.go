package fusiongate

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The shape Codex CLI sends. It asks for a reasoning summary, encrypted
// reasoning to replay and low verbosity on every request. None of the three can
// be expressed in the Chat form, and none of them changes what the answer means,
// so a chat-only channel must still be able to serve the request.
const codexPreferencesRequest = `{"model":"public-model","instructions":"be brief","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"abc","prompt_cache_options":{"ttl":"30m"},"reasoning":{"effort":"high","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

// A newer Codex build asks how long to retain the prompt cache, using the field
// that replaced prompt_cache_retention. Chat has no equivalent hint, so both
// spellings are dropped rather than refusing the request.
func TestBridgeAcceptsDroppableCacheHints(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"hi","prompt_cache_options":{"ttl":"30m"}}`,
		`{"model":"m","input":"hi","prompt_cache_retention":"24h"}`,
	} {
		if err := validateBridgeCapabilities(wireResponses, []byte(body)); err != nil {
			t.Fatalf("cache hint must be droppable: %s: %v", body, err)
		}
	}
}

// The rejection names the field a bridge cannot express, so the next operator can
// see what a client started sending instead of a redacted placeholder.
func TestRejectionNamesUnrecognizedField(t *testing.T) {
	err := validateBridgeCapabilities(wireResponses, []byte(`{"model":"m","input":"hi","brand_new_option":1}`))
	if err == nil {
		t.Fatal("an unknown field must be rejected")
	}
	if !strings.Contains(err.Error(), "unrecognized_field:brand_new_option") {
		t.Fatalf("rejection does not name the field: %v", err)
	}
	err = validateBridgeCapabilities(wireResponses, []byte(`{"model":"m","input":"hi","not a field":1}`))
	if err == nil || !strings.Contains(err.Error(), "unrecognized_field requires") {
		t.Fatalf("a non-identifier key must fall back to the placeholder: %v", err)
	}
}

func TestBridgeAcceptsDroppableCodexPreferences(t *testing.T) {
	if err := validateBridgeCapabilities(wireResponses, []byte(codexPreferencesRequest)); err != nil {
		t.Fatalf("a first-turn Codex request must be bridgeable: %v", err)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"store keeps state", `{"model":"m","input":"hi","store":true}`},
		{"replayed reasoning item", `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","encrypted_content":"x","summary":[]}]}`},
		{"hosted tool", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`},
		{"previous response id", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`},
		{"unknown reasoning field", `{"model":"m","input":"hi","reasoning":{"effort":"high","secret":"x"}}`},
		{"non-reasoning include", `{"model":"m","input":"hi","include":["message.output_text.logprobs"]}`},
		{"unknown field", `{"model":"m","input":"hi","guess":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBridgeCapabilities(wireResponses, []byte(tc.body)); err == nil {
				t.Fatalf("must stay rejected: %s", tc.body)
			}
		})
	}
}

// A relay that only serves Chat Completions, pinned to that interface method the
// way an operator configures it, has to answer a Codex request whose only
// unrepresentable parts are the droppable preferences.
func TestCodexPreferencesStillBridgeToChatOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	chat := chatOnlyUpstream(t, &bodies, &mu, false)
	a, key, done := bridgeFixture(t, "openai_compatible", chat)
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='chat' WHERE name='bridge'`); err != nil {
		t.Fatal(err)
	}

	rec := bridgeRequest(t, a, key, "/v1/responses", codexPreferencesRequest)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("bridged chat calls = %d", len(bodies))
	}
	body := bodies[0]
	if body["model"] != "upstream-model" || body["stream"] != true || body["reasoning_effort"] != "high" {
		t.Fatalf("bridged body lost effort or model: %v", body)
	}
	for _, field := range []string{"reasoning", "include", "text", "store", "input", "instructions", "prompt_cache_options"} {
		if _, ok := body[field]; ok {
			t.Fatalf("droppable field %q leaked into the chat body: %v", field, body)
		}
	}
	messages := anySlice(body["messages"])
	if len(messages) != 2 || asString(asMap(messages[0])["content"]) != "be brief" {
		t.Fatalf("instructions must become a system message: %v", messages)
	}
}

// A current Codex build also declares grouped, deferred and hosted tool shapes
// that a Chat upstream cannot express, plus client attribution metadata. The
// declarations are dropped rather than refusing the request: the model only calls
// a tool that was declared, so nothing can be mis-routed and the request still
// works with fewer tools.
const codexGroupedToolsRequest = `{"model":"public-model","instructions":"be brief","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"abc","reasoning":{"effort":"high"},"text":{"verbosity":"low"},"client_metadata":{"session_id":"s","x-codex-window-id":"s:0"},"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},{"type":"namespace","name":"collaboration","description":"sub-agents","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{}}}]},{"type":"namespace","name":"mcp__cua_repl","tools":[{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}]},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}},{"type":"web_search","external_web_access":false,"search_content_types":["text"]}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func TestBridgeAcceptsGroupedAndDeferredToolDeclarations(t *testing.T) {
	if err := validateBridgeCapabilities(wireResponses, []byte(codexGroupedToolsRequest)); err != nil {
		t.Fatalf("grouped and deferred declarations must be droppable: %v", err)
	}
	for _, tc := range []struct{ name, body string }{
		{"search without the opt-out", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`},
		{"search with external access on", `{"model":"m","input":"hi","tools":[{"type":"web_search","external_web_access":true}]}`},
		{"hosted tool with no rule", `{"model":"m","input":"hi","tools":[{"type":"computer_use_preview"}]}`},
		{"custom format the converter drops", `{"model":"m","input":"hi","tools":[{"type":"custom","name":"apply_patch","format":{"type":"regex","pattern":"x"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBridgeCapabilities(wireResponses, []byte(tc.body)); err == nil {
				t.Fatalf("must stay rejected: %s", tc.body)
			}
		})
	}
}

func TestGroupedToolDeclarationsStillBridgeToChatOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	chat := chatOnlyUpstream(t, &bodies, &mu, false)
	a, key, done := bridgeFixture(t, "openai_compatible", chat)
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='chat' WHERE name='bridge'`); err != nil {
		t.Fatal(err)
	}

	rec := bridgeRequest(t, a, key, "/v1/responses", codexGroupedToolsRequest)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("bridged chat calls = %d", len(bodies))
	}
	names := []string{}
	for _, value := range anySlice(bodies[0]["tools"]) {
		names = append(names, asString(asMap(asMap(value)["function"])["name"]))
	}
	if strings.Join(names, ",") != "exec_command,apply_patch" {
		t.Fatalf("only representable tools may reach the upstream: %v", names)
	}
	if _, ok := bodies[0]["client_metadata"]; ok {
		t.Fatalf("client metadata leaked into the chat body: %v", bodies[0]["client_metadata"])
	}
}
