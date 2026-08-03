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
