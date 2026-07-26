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
	return fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n"+
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg-1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\",\"annotations\":[]}]}}\n\n"+
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"+
		"data: [DONE]\n\n", id, id)
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
