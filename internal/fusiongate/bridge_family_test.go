package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// anthropicSSE is the event sequence an Anthropic-compatible upstream streams.
func anthropicSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude\",\"content\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n")
	_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
	_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
	_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

// anyCacheControl reports whether a converted body still carries an Anthropic
// cache breakpoint anywhere, which a Chat upstream must never receive.
func anyCacheControl(node any) bool {
	switch value := node.(type) {
	case []any:
		for _, item := range value {
			if anyCacheControl(item) {
				return true
			}
		}
	case map[string]any:
		if _, ok := value["cache_control"]; ok {
			return true
		}
		for _, item := range value {
			if anyCacheControl(item) {
				return true
			}
		}
	}
	return false
}

// A Codex client must reach an Anthropic-compatible channel, which only serves
// Messages. This is the crossing that no other interface combination exercised.
func TestCodexRequestBridgesToAnthropicOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := bridgeFixture(t, "anthropic_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream received invalid JSON: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		anthropicSSE(w)
	})
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='messages' WHERE name='bridge'`); err != nil {
		t.Fatal(err)
	}

	rec := bridgeRequest(t, a, key, "/v1/responses", codexGroupedToolsRequest)
	out := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(out, "event: response.completed") || !strings.Contains(out, `"text":"hello"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("upstream calls = %d", len(bodies))
	}
	body := bodies[0]
	if body["model"] != "upstream-model" || body["stream"] != true {
		t.Fatalf("anthropic body lost model or stream: %v", body)
	}
	if num(body["max_tokens"]) <= 0 {
		t.Fatalf("Anthropic requires max_tokens: %v", body)
	}
	if asString(body["system"]) != "be brief" {
		t.Fatalf("instructions must become the system prompt: %v", body["system"])
	}
	tools := anySlice(body["tools"])
	if len(tools) == 0 || asMap(tools[0])["input_schema"] == nil {
		t.Fatalf("tools must be converted to input_schema form: %v", tools)
	}
	for _, field := range []string{"prompt_cache_key", "parallel_tool_calls", "include", "text", "client_metadata", "reasoning", "stream_options"} {
		if _, ok := body[field]; ok {
			t.Fatalf("openai-only field %q leaked into the anthropic body: %v", field, body)
		}
	}
}

// A Claude-style client must reach a channel that only serves Chat Completions,
// including the cache markers, thinking budget and user id it sends on every turn.
func TestClaudeStyleRequestBridgesToChatOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := bridgeFixture(t, "openai_compatible", chatOnlyUpstream(t, &bodies, &mu, false))
	defer done()

	rec := bridgeRequest(t, a, key, "/v1/messages", `{"model":"public-model","max_tokens":4096,"stream":true,"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"u1"},"thinking":{"type":"enabled","budget_tokens":2048},"top_k":40,"tools":[{"name":"shell","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	out := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("status = %d, body = %s", rec.Code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("upstream calls = %d", len(bodies))
	}
	body := bodies[0]
	messages := anySlice(body["messages"])
	if len(messages) == 0 || asString(asMap(messages[0])["role"]) != "system" {
		t.Fatalf("system prompt lost: %v", messages)
	}
	if anyCacheControl(body) {
		t.Fatalf("cache markers leaked into the chat body: %v", body)
	}
	for _, field := range []string{"metadata", "thinking", "top_k"} {
		if _, ok := body[field]; ok {
			t.Fatalf("anthropic-only field %q leaked into the chat body: %v", field, body)
		}
	}
	tools := anySlice(body["tools"])
	if len(tools) != 1 || asMap(asMap(tools[0])["function"])["parameters"] == nil {
		t.Fatalf("tool was not converted to a chat function: %v", tools)
	}
}
