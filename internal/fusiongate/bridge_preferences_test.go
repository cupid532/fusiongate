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
const codexPreferencesRequest = `{"model":"public-model","instructions":"be brief","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"abc","reasoning":{"effort":"high","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

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
	for _, field := range []string{"reasoning", "include", "text", "store", "input", "instructions"} {
		if _, ok := body[field]; ok {
			t.Fatalf("droppable field %q leaked into the chat body: %v", field, body)
		}
	}
	messages := anySlice(body["messages"])
	if len(messages) != 2 || asString(asMap(messages[0])["content"]) != "be brief" {
		t.Fatalf("instructions must become a system message: %v", messages)
	}
}
