package fusiongate

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestAnthropicMessagesRequestToOpenAISupportsDynamicSystemMessages(t *testing.T) {
	body := map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "Static instructions."}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Hello"}}},
			map[string]any{"role": "system", "content": "Dynamic session context."},
		},
		"tools": []any{
			map[string]any{"name": "AskUserQuestion", "input_schema": map[string]any{"type": "object"}},
		},
	}
	encoded, err := anthropicMessagesRequestToOpenAI(body, "upstream-model", true, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	messages := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%#v", messages)
	}
	first := messages[0].(map[string]any)
	dynamic := messages[2].(map[string]any)
	if first["role"] != "system" || first["content"] != "Static instructions." {
		t.Fatalf("static system=%#v", first)
	}
	if dynamic["role"] != "system" || dynamic["content"] != "Dynamic session context." {
		t.Fatalf("dynamic system=%#v", dynamic)
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
	result := streamOpenAIAsAnthropic(rec, strings.NewReader(upstream), resolvedRoute{Route: Route{PublicName: "claude-test"}}, "request2", time.Second, time.Second)
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

func TestStreamOpenAIAsAnthropicEmptyStreamDoesNotCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	result := streamOpenAIAsAnthropic(rec, strings.NewReader(""), resolvedRoute{Route: Route{PublicName: "claude-test"}}, "empty", time.Second, time.Second)
	if result.Status != http.StatusBadGateway || !result.Retryable || result.Handled || result.Reason != "upstream_empty_stream" {
		t.Fatalf("result=%+v", result)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("stream committed downstream response: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestStreamOpenAIAsAnthropicAcceptsCRLF(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n"
	rec := httptest.NewRecorder()
	result := streamOpenAIAsAnthropic(rec, strings.NewReader(upstream), resolvedRoute{Route: Route{PublicName: "claude-test"}}, "crlf", time.Second, time.Second)
	if result.Status != http.StatusOK || !result.Handled || result.Err != nil {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"hello"`) || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("converted stream=%s", rec.Body.String())
	}
}

func TestStreamOpenAIAsAnthropicOutputStartTimeoutDoesNotCommit(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	rec := httptest.NewRecorder()
	result := streamOpenAIAsAnthropic(rec, reader, resolvedRoute{Route: Route{PublicName: "claude-test"}}, "timeout", 20*time.Millisecond, time.Second)
	if result.Status != http.StatusGatewayTimeout || !result.Retryable || result.Handled || result.Reason != "upstream_output_timeout" {
		t.Fatalf("result=%+v", result)
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("stream committed downstream response: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	_ = reader.Close()
}

func TestStreamOpenAIAsAnthropicIdleTimeoutAndInterruption(t *testing.T) {
	valid := `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"
	tests := []struct {
		name   string
		error  error
		reason string
		status int
	}{
		{name: "idle timeout", error: context.DeadlineExceeded, reason: "upstream_stalled", status: http.StatusGatewayTimeout},
		{name: "interruption", error: errors.New("upstream disconnected"), reason: "upstream_stream_interrupted", status: http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			go func() {
				_, _ = io.WriteString(writer, valid)
				if tc.error == context.DeadlineExceeded {
					return
				}
				_ = writer.CloseWithError(tc.error)
			}()
			rec := httptest.NewRecorder()
			result := streamOpenAIAsAnthropic(rec, reader, resolvedRoute{Route: Route{PublicName: "claude-test"}}, "idle", time.Second, 20*time.Millisecond)
			if result.Status != tc.status || !result.Handled || result.Reason != tc.reason {
				t.Fatalf("result=%+v body=%s", result, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"text":"hello"`) || strings.Contains(body, "event: message_stop") {
				t.Fatalf("incomplete converted stream=%s", body)
			}
			_ = reader.Close()
		})
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
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("automatic gzip negotiation missing: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		gz := gzip.NewWriter(w)
		if err := json.NewEncoder(gz).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 1},
		}); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
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

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/1")
	// Reproduce Caddy/desktop compression negotiation. The bridge must remove
	// this explicit header so net/http can transparently decode upstream gzip.
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
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
	a.flushLedgerWrites()
	var protocol string
	var success, inputTokens, outputTokens int
	if err := a.db.QueryRow(`SELECT protocol,success,input_tokens,output_tokens FROM request_ledger`).Scan(&protocol, &success, &inputTokens, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if protocol != "anthropic_messages" || success != 1 || inputTokens != 3 || outputTokens != 1 {
		t.Fatalf("ledger protocol=%s success=%d usage=%d/%d", protocol, success, inputTokens, outputTokens)
	}
}

func TestMessagesNonStreamUsesOpenAIUpstreamSSE(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept=%q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-anthropic\",\"model\":\"provider-claude\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-anthropic\",\"model\":\"provider-claude\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "streamed-openai-claude", "openai_compatible", upstream.URL, "upstream-secret", 1, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, providerID, "claude-test", "provider-claude", "chat,stream", 1)
	key := insertTestKey(t, a, false)
	rec := gatewayRequest(t, a, "/v1/messages", key, `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`, "claude-cli/1")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if received["stream"] != true {
		t.Fatalf("upstream request=%#v", received)
	}
	var completed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	content := anySlice(completed["content"])
	tool := asMap(content[1])
	if asMap(content[0])["text"] != "pong" || completed["stop_reason"] != "tool_use" || tool["name"] != "lookup" || asInt64(asMap(tool["input"])["q"]) != 1 {
		t.Fatalf("completed=%#v", completed)
	}
	if asInt64(asMap(completed["usage"])["input_tokens"]) != 8 || asInt64(asMap(completed["usage"])["output_tokens"]) != 4 {
		t.Fatalf("usage=%#v", completed["usage"])
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
		if body["stream"] != true || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("stream=%#v accept=%q", body["stream"], r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"native-claude\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"native\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
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
