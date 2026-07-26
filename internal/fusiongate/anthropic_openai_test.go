package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicMessagesRequestToOpenAIConvertsToolsAndResults(t *testing.T) {
	body := map[string]any{
		"system":      []any{map[string]any{"type": "text", "text": "Be precise."}},
		"max_tokens":  float64(2048),
		"temperature": float64(0.2),
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Inspect it"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "Checking."},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{"path": "/tmp/a"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{map[string]any{"type": "text", "text": "contents"}}},
				map[string]any{"type": "text", "text": "Continue"},
			}},
		},
		"tools":       []any{map[string]any{"name": "read_file", "description": "Read a file", "input_schema": map[string]any{"type": "object"}}},
		"tool_choice": map[string]any{"type": "tool", "name": "read_file"},
	}
	encoded, err := anthropicMessagesRequestToOpenAI(body, "upstream-model", true, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "upstream-model" || got["stream"] != true || num(got["max_tokens"]) != 2048 {
		t.Fatalf("request metadata = %#v", got)
	}
	messages := got["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("messages=%d %#v", len(messages), messages)
	}
	if messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] != "Be precise." {
		t.Fatalf("system=%#v", messages[0])
	}
	assistant := messages[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read_file" || !strings.Contains(function["arguments"].(string), "/tmp/a") {
		t.Fatalf("tool call=%#v", calls[0])
	}
	toolResult := messages[3].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "toolu_1" || toolResult["content"] != "contents" {
		t.Fatalf("tool result=%#v", toolResult)
	}
	if _, ok := got["stream_options"].(map[string]any); !ok {
		t.Fatalf("stream_options=%#v", got["stream_options"])
	}
	tools := got["tools"].([]any)
	toolFunction := tools[0].(map[string]any)["function"].(map[string]any)
	if toolFunction["name"] != "read_file" {
		t.Fatalf("tools=%#v", tools)
	}
}

func TestWriteOpenAIAsAnthropicConvertsToolUseAndUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	result := writeOpenAIAsAnthropic(rec, strings.NewReader(`{
		"choices":[{"message":{"role":"assistant","content":"I will inspect it.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":12,"completion_tokens":5}
	}`), resolvedRoute{Route: Route{PublicName: "claude-test"}}, "request1")
	if !result.Handled || result.Status != http.StatusOK || result.Usage.Input != 12 || result.Usage.Output != 5 {
		t.Fatalf("result=%+v", result)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "message" || got["model"] != "claude-test" || got["stop_reason"] != "tool_use" {
		t.Fatalf("response=%#v", got)
	}
	content := got["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("content=%#v", content)
	}
	input := content[1].(map[string]any)["input"].(map[string]any)
	if input["path"] != "a.go" {
		t.Fatalf("tool input=%#v", input)
	}
}

func TestStreamOpenAIAsAnthropicEmitsClaudeSSE(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	result := streamOpenAIAsAnthropic(rec, strings.NewReader(upstream), resolvedRoute{Route: Route{PublicName: "claude-test"}}, "request2")
	if !result.Handled || result.Status != http.StatusOK || result.Usage.Input != 9 || result.Usage.Output != 4 {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"text":"Hello ","type":"text_delta"`,
		`"text":"world","type":"text_delta"`,
		`"id":"call_1","input":{},"name":"read_file","type":"tool_use"`,
		`"partial_json":"{\"path\":\"a.go\"}","type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in stream:\n%s", want, body)
		}
	}
}

func TestMessagesUsesOpenAICompatibleRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("authorization missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "provider-claude" {
			t.Errorf("model=%#v", body["model"])
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "openai-claude", "openai_compatible", upstream.URL, "upstream-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-test", "provider-claude", "chat,stream", 1)
	key := insertTestKey(t, a, false)

	rec := gatewayRequest(t, a, "/v1/messages", key, `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`, "claude-cli/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "message" || got["model"] != "claude-test" || got["stop_reason"] != "end_turn" {
		t.Fatalf("response=%#v", got)
	}
	content := got["content"].([]any)
	if content[0].(map[string]any)["text"] != "pong" {
		t.Fatalf("content=%#v", content)
	}
	var protocol string
	var success, inputTokens, outputTokens int
	if err := a.db.QueryRow(`SELECT protocol,success,input_tokens,output_tokens FROM request_ledger`).Scan(&protocol, &success, &inputTokens, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if protocol != "anthropic_messages" || success != 1 || inputTokens != 3 || outputTokens != 1 {
		t.Fatalf("ledger protocol=%s success=%d usage=%d/%d", protocol, success, inputTokens, outputTokens)
	}
}

func TestMessagesKeepsNativeAnthropicRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "native-claude" {
			t.Errorf("model=%#v", body["model"])
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "msg_native", "type": "message", "role": "assistant", "model": "native-claude",
			"content": []any{map[string]any{"type": "text", "text": "native"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
		})
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "native", "anthropic", upstream.URL, "anthropic-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-native", "native-claude", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages", key, `{"model":"claude-native","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`, "claude-cli/1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "native") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
