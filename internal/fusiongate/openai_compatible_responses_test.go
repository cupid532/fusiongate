package fusiongate

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopyUpstreamRequestHeadersDropsCodexInternalResponsesLiteHeader(t *testing.T) {
	src := http.Header{
		"X-OpenAI-Internal-Codex-Responses-Lite": []string{"1"},
		"X-Client-Trace":                         []string{"trace"},
	}
	dst := make(http.Header)

	copyUpstreamRequestHeaders(dst, src)

	if got := dst.Get("X-OpenAI-Internal-Codex-Responses-Lite"); got != "" {
		t.Fatalf("internal Codex header was forwarded: %q", got)
	}
	if got := dst.Get("X-Client-Trace"); got != "trace" {
		t.Fatalf("ordinary client header was not forwarded: %q", got)
	}
}

func TestNormalizedCompatibleChatBodyConvertsDeveloperRole(t *testing.T) {
	encoded, err := normalizedCompatibleChatBody([]byte(`{"model":"m","messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	messages := anySlice(body["messages"])
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role was not converted: %s", encoded)
	}
}

func TestNormalizedCompatibleChatBodyOmitsDeprecatedClaude5Sampling(t *testing.T) {
	for _, model := range []string{"claude-fable-5", "claude-haiku-5", "claude-opus-5", "claude-sonnet-5", "claude-sonnet-5-20260801"} {
		encoded, err := normalizedCompatibleChatBody([]byte(`{"model":"` + model + `","temperature":0,"top_p":0.9,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("temperature was retained for %s: %s", model, encoded)
		}
		if _, exists := body["top_p"]; exists {
			t.Fatalf("top_p was retained for %s: %s", model, encoded)
		}
	}
}

func TestNormalizedCompatibleChatBodyPreservesSamplingForOlderClaudeModels(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "claude-3-5-sonnet", "gpt-5.6-sol"} {
		encoded, err := normalizedCompatibleChatBody([]byte(`{"model":"` + model + `","temperature":0,"top_p":0.9,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["temperature"]; !exists {
			t.Fatalf("temperature was removed for %s: %s", model, encoded)
		}
		if _, exists := body["top_p"]; !exists {
			t.Fatalf("top_p was removed for %s: %s", model, encoded)
		}
	}
}

func TestCompatibleResponsesBodyFromRequest(t *testing.T) {
	raw := []byte(`{"model":"public","instructions":"Be concise","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call_output","call_id":"call-1","output":"found"}],"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"max_output_tokens":64,"reasoning":{"effort":"low"},"stream":true}`)
	encoded, stream, err := compatibleResponsesBodyFromRequest(raw, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("downstream stream flag was lost")
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream" || body["stream"] != false || asInt64(body["max_completion_tokens"]) != 64 || body["reasoning_effort"] != "low" {
		t.Fatalf("unexpected converted body: %s", encoded)
	}
	messages := anySlice(body["messages"])
	if len(messages) != 3 || asMap(messages[0])["role"] != "system" || asMap(messages[2])["role"] != "tool" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	tools := anySlice(body["tools"])
	if len(tools) != 1 || asMap(asMap(tools[0])["function"])["name"] != "lookup" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	if asMap(asMap(body["tool_choice"])["function"])["name"] != "lookup" {
		t.Fatalf("unexpected tool choice: %#v", body["tool_choice"])
	}
}

func TestCompatibleResponsesFromChatJSONAndSSE(t *testing.T) {
	chat := []byte(`{"id":"chatcmpl-1","created":123,"choices":[{"message":{"role":"assistant","content":"OK","reasoning_content":"checked","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`)
	encoded, contentType, err := compatibleResponsesFromChat(chat, "public", false)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" {
		t.Fatalf("content type=%q", contentType)
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["model"] != "public" || len(anySlice(response["output"])) != 3 {
		t.Fatalf("unexpected response: %s", encoded)
	}
	usage := asMap(response["usage"])
	if asInt64(usage["input_tokens"]) != 7 || asInt64(usage["output_tokens"]) != 3 {
		t.Fatalf("unexpected usage: %#v", usage)
	}

	streamBody, streamType, err := compatibleResponsesFromChat(chat, "public", true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(streamBody)
	if streamType != "text/event-stream" || !strings.Contains(text, "event: response.output_text.delta") || !strings.Contains(text, "event: response.completed") || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("unexpected SSE: %s", text)
	}
}

func TestCompatibleResponsesProxyDecodesGzipAndBridgesChat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("automatic gzip negotiation missing: %q", r.Header.Get("Accept-Encoding"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "upstream" || body["stream"] != false || asMap(anySlice(body["messages"])[0])["role"] != "system" {
			t.Errorf("unexpected upstream body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, `{"id":"chatcmpl-gzip","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		_ = zw.Close()
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := resolvedRoute{
		Route:      Route{ID: 1, PublicName: "public", UpstreamModel: "upstream"},
		Provider:   Provider{ID: 1, Type: "openai_compatible", BaseURL: upstream.URL, RequestTimeoutMS: 5000},
		Credential: "secret",
	}
	raw := []byte(`{"model":"public","instructions":"rules","input":"hello","stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	result := a.compatibleResponsesProxy(rec, req, raw, z, "request-id", false, true, nil)
	if result.Status != http.StatusOK || !result.Handled || result.Err != nil {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("stale content encoding leaked: %q", rec.Header().Get("Content-Encoding"))
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || textContent(asMap(anySlice(response["output"])[0])["content"]) != "OK" {
		t.Fatalf("unexpected downstream response: %s", rec.Body.String())
	}
}

func cacheControlTTLs(node any) []string {
	var blocks []map[string]any
	collectCacheControls(node, &blocks)
	ttls := make([]string, 0, len(blocks))
	for _, control := range blocks {
		ttls = append(ttls, asString(control["ttl"]))
	}
	return ttls
}

func TestNormalizedCompatibleChatBodyDowngradesLateOneHourCacheTTL(t *testing.T) {
	// Claude Code sends a multi block system prompt where a one hour cache
	// marker trails a five minute one, which Anthropic upstreams reject with
	// "a ttl='1h' cache_control block must not come after a ttl='5m' block".
	encoded, err := normalizedCompatibleChatBody([]byte(`{
		"model": "claude-opus-5",
		"messages": [
			{"role": "system", "content": [
				{"type": "text", "text": "identity", "cache_control": {"type": "ephemeral"}},
				{"type": "text", "text": "tools", "cache_control": {"type": "ephemeral", "ttl": "5m"}},
				{"type": "text", "text": "project", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "hello", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
			]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	messages := anySlice(body["messages"])
	system := asMap(messages[0])
	if got := cacheControlTTLs(system["content"]); strings.Join(got, ",") != ",5m,5m" {
		t.Fatalf("system cache ttls were not normalized: %v (%s)", got, encoded)
	}
	if got := cacheControlTTLs(asMap(messages[1])["content"]); strings.Join(got, ",") != "5m" {
		t.Fatalf("trailing message cache ttl was not normalized: %v (%s)", got, encoded)
	}
	if asMap(anySlice(system["content"])[2])["text"] != "project" {
		t.Fatalf("block content was altered: %s", encoded)
	}
}

func TestNormalizedCompatibleChatBodyKeepsCompliantCacheTTLOrder(t *testing.T) {
	encoded, err := normalizedCompatibleChatBody([]byte(`{
		"model": "claude-opus-5",
		"tools": [{"type": "function", "function": {"name": "read"}, "cache_control": {"type": "ephemeral", "ttl": "1h"}}],
		"messages": [
			{"role": "system", "content": [{"type": "text", "text": "rules", "cache_control": {"type": "ephemeral", "ttl": "1h"}}]},
			{"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral", "ttl": "5m"}}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if got := cacheControlTTLs(body["tools"]); strings.Join(got, ",") != "1h" {
		t.Fatalf("leading tool cache ttl was downgraded: %v", got)
	}
	messages := anySlice(body["messages"])
	if got := cacheControlTTLs(asMap(messages[0])["content"]); strings.Join(got, ",") != "1h" {
		t.Fatalf("leading system cache ttl was downgraded: %v", got)
	}
	if got := cacheControlTTLs(asMap(messages[1])["content"]); strings.Join(got, ",") != "5m" {
		t.Fatalf("trailing short cache ttl was rewritten: %v", got)
	}
}

func TestNormalizedCompatibleChatBodyOrdersSystemMessagesFirst(t *testing.T) {
	// Anthropic bridges lift system messages into the system array, so a one
	// hour system marker must survive a five minute marker that appears
	// earlier in the OpenAI message list.
	encoded, err := normalizedCompatibleChatBody([]byte(`{
		"model": "claude-opus-5",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral", "ttl": "5m"}}]},
			{"role": "system", "content": [{"type": "text", "text": "rules", "cache_control": {"type": "ephemeral", "ttl": "1h"}}]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	messages := anySlice(body["messages"])
	if got := cacheControlTTLs(asMap(messages[1])["content"]); strings.Join(got, ",") != "1h" {
		t.Fatalf("system cache ttl was downgraded despite being processed first: %v", got)
	}
}

func TestNormalizeAnthropicCacheControlTTLWalksToolsSystemThenMessages(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(`{
		"model": "claude-opus-5",
		"tools": [{"name": "read", "cache_control": {"type": "ephemeral", "ttl": "5m"}}],
		"system": [{"type": "text", "text": "rules", "cache_control": {"type": "ephemeral", "ttl": "1h"}}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral", "ttl": "1h"}}]}]
	}`), &body); err != nil {
		t.Fatal(err)
	}
	if !normalizeAnthropicCacheControlTTL(body) {
		t.Fatal("normalization reported no change for a violating request")
	}
	if got := cacheControlTTLs(body["tools"]); strings.Join(got, ",") != "5m" {
		t.Fatalf("tool cache ttl was rewritten: %v", got)
	}
	if got := cacheControlTTLs(body["system"]); strings.Join(got, ",") != "5m" {
		t.Fatalf("system cache ttl was not downgraded: %v", got)
	}
	if got := cacheControlTTLs(body["messages"]); strings.Join(got, ",") != "5m" {
		t.Fatalf("message cache ttl was not downgraded: %v", got)
	}
}

func TestNormalizeAnthropicCacheControlTTLLeavesCompliantRequestsAlone(t *testing.T) {
	for _, raw := range []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`,
	} {
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		if normalizeAnthropicCacheControlTTL(body) {
			t.Fatalf("compliant request was rewritten: %s", raw)
		}
	}
}
