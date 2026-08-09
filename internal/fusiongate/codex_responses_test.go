package fusiongate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func codexTestRoute(upstreamURL string) resolvedRoute {
	credential := ProviderCredential{Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth", AccessToken: "codex-access", AccountID: "acct-id"}
	return resolvedRoute{
		Route:      Route{PublicName: "gpt-public", UpstreamModel: "gpt-upstream"},
		Provider:   Provider{Type: "codex_oauth", BaseURL: upstreamURL, RequestTimeoutMS: 5000},
		Credential: credential.AccessToken, AuthCredential: &credential,
	}
}

func codexCompletedSSE(id string) string {
	return codexCompletedSSEText(id, "OK")
}

func codexCompletedSSEText(id, text string) string {
	return fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n"+
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg-1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q,\"annotations\":[]}]}}\n\n"+
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"+
		"data: [DONE]\n\n", id, text, id)
}

func TestNormalizedCodexResponsesBody(t *testing.T) {
	encoded, err := normalizedCodexResponsesBody([]byte(`{"model":"public","input":"hello","stream":false,"store":true,"max_output_tokens":12,"stream_options":{"include_usage":true}}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream" || body["stream"] != true || body["store"] != false {
		t.Fatalf("normalized body=%s", encoded)
	}
	if _, exists := body["stream_options"]; exists {
		t.Fatalf("stream_options was not removed: %s", encoded)
	}
	if _, exists := body["max_output_tokens"]; exists {
		t.Fatalf("unsupported max_output_tokens was not removed: %s", encoded)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input=%#v", body["input"])
	}
	message, _ := input[0].(map[string]any)
	content, _ := message["content"].([]any)
	part, _ := content[0].(map[string]any)
	if message["role"] != "user" || part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("normalized input=%#v", input)
	}

	encoded, err = normalizedCodexResponsesBody([]byte(`{"model":"public","input":{"role":"user","content":[]}}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if input, ok := body["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("object input was not wrapped: %s", encoded)
	}
}

func TestCodexChatRequestAndResponseConversion(t *testing.T) {
	encoded, err := codexResponsesBodyFromChat([]byte(`{"model":"public","messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Hello"},{"role":"assistant","content":"Checking","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},{"role":"tool","tool_call_id":"call-1","content":"found"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}],"reasoning_effort":"low","stream":false}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if request["model"] != "upstream" || request["stream"] != true || request["store"] != false {
		t.Fatalf("request=%#v", request)
	}
	if reasoning := asMap(request["reasoning"]); reasoning["effort"] != "low" {
		t.Fatalf("reasoning=%#v", request["reasoning"])
	}
	input := anySlice(request["input"])
	if len(input) != 5 || asMap(input[4])["type"] != "function_call_output" {
		t.Fatalf("input=%#v", input)
	}
	tools := anySlice(request["tools"])
	if len(tools) != 1 || asMap(tools[0])["name"] != "lookup" {
		t.Fatalf("tools=%#v", tools)
	}

	completed := []byte(`{"id":"resp-chat","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done"}]},{"type":"function_call","call_id":"call-2","name":"next","arguments":"{}"}],"usage":{"input_tokens":11,"output_tokens":4}}`)
	chat, contentType, err := codexChatResponse(completed, false, "public")
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" {
		t.Fatalf("content type=%q", contentType)
	}
	var response map[string]any
	if err := json.Unmarshal(chat, &response); err != nil {
		t.Fatal(err)
	}
	choice := asMap(anySlice(response["choices"])[0])
	message := asMap(choice["message"])
	if message["content"] != "Done" || choice["finish_reason"] != "tool_calls" || len(anySlice(message["tool_calls"])) != 1 {
		t.Fatalf("response=%#v", response)
	}
}

func TestCodexChatRequestAcceptsResponsesReasoningShape(t *testing.T) {
	encoded, err := codexResponsesBodyFromChat([]byte(`{"model":"public","messages":[{"role":"user","content":"Hello"}],"reasoning":{"effort":"xhigh"}}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if reasoning := asMap(request["reasoning"]); reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning=%#v", request["reasoning"])
	}
}

func TestCodexChatReasoningEffortPrefersExplicitChatField(t *testing.T) {
	encoded, err := codexResponsesBodyFromChat([]byte(`{"model":"public","messages":[{"role":"user","content":"Hello"}],"reasoning_effort":"low","reasoning":{"effort":"high"}}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if reasoning := asMap(request["reasoning"]); reasoning["effort"] != "low" {
		t.Fatalf("reasoning=%#v", request["reasoning"])
	}
}

func TestCodexChatRequestPreservesImageInput(t *testing.T) {
	encoded, err := codexResponsesBodyFromChat([]byte(`{"model":"public","messages":[{"role":"user","content":[{"type":"text","text":"What is shown?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8=","detail":"high"}}]}]}`), "upstream")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	input := anySlice(request["input"])
	content := anySlice(asMap(input[0])["content"])
	if len(content) != 2 {
		t.Fatalf("content=%#v", content)
	}
	image := asMap(content[1])
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,aGVsbG8=" || image["detail"] != "high" {
		t.Fatalf("image=%#v", image)
	}
}

func TestCodexChatRequestRejectsMalformedImageInput(t *testing.T) {
	_, err := codexResponsesBodyFromChat([]byte(`{"model":"public","messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`), "upstream")
	if err == nil || !strings.Contains(err.Error(), "image_url.url") {
		t.Fatalf("err=%v", err)
	}
}

func TestCodexChatCompletionsUsesResponsesBridge(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSEText("resp-chat", "Hello from Plus")))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	credential := ProviderCredential{Version: 1, Kind: "oauth", Platform: "codex", Source: "fusiongate_oauth", AccessToken: "access", AccountID: "account"}
	payload, _ := json.Marshal(credential)
	sealed, _ := a.encrypt(string(payload))
	stamp := now()
	result, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,auth_kind,auth_source,enabled,priority,weight,status,passthrough_mode,client_policy,request_timeout_ms,failure_threshold,cooldown_seconds,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "plus", "codex_oauth", upstream.URL, sealed, "oauth", "fusiongate_oauth", 1, 1, 100, "unknown", "normalized", "any", 5000, 3, 30, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := result.LastInsertId()
	insertTestRoute(t, a, providerID, "gpt-plus", "gpt-upstream", "chat,stream", 1)
	key := insertTestKey(t, a, false)

	recorder := gatewayRequest(t, a, "/v1/chat/completions", key, `{"model":"gpt-plus","messages":[{"role":"user","content":"Hello"}],"stream":false}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if received["model"] != "gpt-upstream" || received["stream"] != true || received["store"] != false {
		t.Fatalf("upstream request=%#v", received)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response JSON: %v body=%s", err, recorder.Body.String())
	}
	choice := asMap(anySlice(response["choices"])[0])
	message := asMap(choice["message"])
	if message["content"] != "Hello from Plus" || response["model"] != "gpt-plus" {
		t.Fatalf("response=%#v", response)
	}
	a.flushLedgerWrites()
	var protocol string
	var success int
	if err := a.db.QueryRow(`SELECT protocol,success FROM request_ledger ORDER BY id DESC LIMIT 1`).Scan(&protocol, &success); err != nil {
		t.Fatal(err)
	}
	if protocol != "openai_chat" || success != 1 {
		t.Fatalf("ledger protocol=%q success=%d", protocol, success)
	}
}

func TestCompletedCodexResponseReassemblesMultipleOutputItems(t *testing.T) {
	stream := `event: response.output_item.done
` +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"reasoning-1","type":"reasoning","summary":[{"type":"summary_text","text":"brief"}]}}

` +
		`event: response.output_item.done
` +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"message-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}

` +
		`event: response.completed
` +
		`data: {"type":"response.completed","response":{"id":"resp-multiple","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":4}}}

`

	encoded, usage, err := completedResponseFromSSE([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("output=%#v", response["output"])
	}
	reasoning, _ := output[0].(map[string]any)
	message, _ := output[1].(map[string]any)
	if reasoning["type"] != "reasoning" || message["type"] != "message" {
		t.Fatalf("output=%#v", output)
	}
	if !usage.Reported || usage.Input != 9 || usage.Output != 4 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestCompletedCodexResponsePreservesExistingOutput(t *testing.T) {
	stream := `event: response.output_item.done
` +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"event-item","type":"message","role":"assistant","content":[]}}

` +
		`event: response.completed
` +
		`data: {"type":"response.completed","response":{"id":"resp-existing","status":"completed","output":[{"id":"completed-item","type":"message","role":"assistant","content":[]}]}}

`

	encoded, _, err := completedResponseFromSSE([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]any)
	item, _ := output[0].(map[string]any)
	if len(output) != 1 || item["id"] != "completed-item" {
		t.Fatalf("completed output was overwritten: %#v", output)
	}
}

func TestCodexResponsesIncompleteSSEIsRetryable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"incomplete\"}}\n\n"))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := []byte(`{"model":"gpt-public","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	result := a.openAIProxy(rec, req, raw, codexTestRoute(upstream.URL), "request-id", "/v1/responses", false, true, nil)
	if result.Status != http.StatusBadGateway || !result.Retryable || result.Handled || result.Reason != "upstream_invalid_stream" || result.Err == nil {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("incomplete response committed downstream bytes: %q", rec.Body.String())
	}
}

func TestOpenAICompatibleResponsesPreserveMaxOutputTokens(t *testing.T) {
	encoded, err := normalizedOpenAIBody([]byte(`{"model":"public","input":"hello","max_output_tokens":12}`), "upstream", false, true)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "upstream" || body["max_output_tokens"] != float64(12) {
		t.Fatalf("ordinary OpenAI-compatible body lost max_output_tokens: %s", encoded)
	}
}

func TestCodexResponsesNonStreamBuffersCompletedEvent(t *testing.T) {
	var received map[string]any
	var accept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if r.URL.Path != "/responses" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSE("resp-buffered")))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := []byte(`{"model":"gpt-public","input":"Reply OK","stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	result := a.openAIProxy(rec, req, raw, codexTestRoute(upstream.URL), "request-id", "/v1/responses", false, true, nil)
	if result.Status != http.StatusOK || !result.Handled || result.Err != nil {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	if accept != "text/event-stream" || received["stream"] != true || received["store"] != false || received["model"] != "gpt-upstream" {
		t.Fatalf("accept=%q request=%#v", accept, received)
	}
	if _, ok := received["input"].([]any); !ok {
		t.Fatalf("input was not normalized: %#v", received["input"])
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type=%q", got)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("downstream is not JSON: %v body=%s", err, rec.Body.String())
	}
	if response["id"] != "resp-buffered" || response["status"] != "completed" {
		t.Fatalf("response=%#v", response)
	}
	output, _ := response["output"].([]any)
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if len(output) != 1 || item["type"] != "message" || part["text"] != "OK" {
		t.Fatalf("reassembled output=%#v", output)
	}
	if !result.Usage.Reported || result.Usage.Input != 7 || result.Usage.Output != 3 {
		t.Fatalf("usage=%+v", result.Usage)
	}
}

func TestCodexResponsesStreamRemainsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true || body["store"] != false {
			t.Errorf("request=%#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexCompletedSSE("resp-stream")))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := []byte(`{"model":"gpt-public","input":[{"role":"user","content":[]}],"stream":true}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	result := a.openAIProxy(rec, req, raw, codexTestRoute(upstream.URL), "request-id", "/v1/responses", true, true, nil)
	if result.Status != http.StatusOK || !result.Handled || result.Err != nil {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: response.completed") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream body=%s", rec.Body.String())
	}
	if !result.Usage.Reported || result.Usage.Input != 7 || result.Usage.Output != 3 {
		t.Fatalf("usage=%+v", result.Usage)
	}
}

func TestCodexResponsesUpstreamErrorIsPreserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"bad input"}`))
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw := []byte(`{"model":"gpt-public","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	result := a.openAIProxy(rec, req, raw, codexTestRoute(upstream.URL), "request-id", "/v1/responses", false, true, nil)
	if result.Status != http.StatusBadRequest || !result.Handled || !strings.Contains(rec.Body.String(), "bad input") {
		t.Fatalf("result=%+v body=%s", result, rec.Body.String())
	}
}
