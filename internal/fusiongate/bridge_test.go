package fusiongate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chatOnlyUpstream serves only /v1/chat/completions, streaming one text answer
// or one tool call, and 404s everything else the way chat-only relays do.
func chatOnlyUpstream(t *testing.T, bodies *[]map[string]any, mu *sync.Mutex, tool bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid URL (POST `+r.URL.Path+`)"}}`)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("upstream received invalid JSON: %s", raw)
		}
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if tool {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"apply_patch","arguments":""}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"*** Begin Patch\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":100}}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func bridgeFixture(t *testing.T, kind string, handler http.HandlerFunc) (*App, string, func()) {
	t.Helper()
	protocolMemory = &wireMemory{entries: map[string]time.Time{}}
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	key := insertTestKey(t, a, true)
	server := httptest.NewServer(handler)
	id := insertTestProvider(t, a, "bridge", kind, server.URL, "secret", 1, 1, "normalized", "any", 0, 5, 30)
	routeID := insertTestRoute(t, a, id, "public-model", "upstream-model", "chat,stream", 0)
	if _, err := a.db.Exec(`UPDATE model_routes SET input_price_micros=1000000,output_price_micros=2000000 WHERE id=?`, routeID); err != nil {
		t.Fatal(err)
	}
	return a, key, func() { server.Close(); a.Close() }
}

func bridgeRequest(t *testing.T, a *App, key, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

func ledgerUsage(t *testing.T, a *App) (int64, int64, int64, string, int64) {
	t.Helper()
	a.flushLedgerWrites()
	var input, cached, output, cost int64
	var costType string
	if err := a.db.QueryRow(`SELECT input_tokens,cached_tokens,output_tokens,cost_type,cost_micros FROM request_ledger WHERE success=1 ORDER BY id DESC LIMIT 1`).Scan(&input, &cached, &output, &costType, &cost); err != nil {
		t.Fatal(err)
	}
	return input, cached, output, costType, cost
}

const codexRequest = `{"model":"public-model","instructions":"be brief","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"abc","reasoning":{"effort":"high","summary":"auto"},"text":{"verbosity":"low"},
"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},{"type":"web_search"}],
"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
{"type":"reasoning","summary":[],"encrypted_content":"x"},
{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
{"type":"function_call_output","call_id":"c1","output":"ok"},
{"type":"custom_tool_call_output","call_id":"c2","output":"done"}]}`

func TestCodexResponsesBridgesToChatOnlyChannelAndLearns(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var responsesCalls atomic.Int32
	chat := chatOnlyUpstream(t, &bodies, &mu, true)
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			responsesCalls.Add(1)
		}
		chat(w, r)
	})
	defer done()

	for turn := 0; turn < 2; turn++ {
		rec := bridgeRequest(t, a, key, "/v1/responses", codexRequest)
		out := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(out, "event: response.completed") || !strings.Contains(out, `"type":"custom_tool_call"`) || !strings.Contains(out, `"input":"*** Begin Patch"`) {
			t.Fatalf("turn %d status=%d body=%s", turn, rec.Code, out)
		}
		if !strings.Contains(out, `"input_tokens":120`) || strings.Contains(out, "[DONE]") {
			t.Fatalf("turn %d missing usage or leaked chat terminator: %s", turn, out)
		}
	}
	if got := responsesCalls.Load(); got != 1 {
		t.Fatalf("native /v1/responses should be tried once then remembered, got %d calls", got)
	}
	mu.Lock()
	body := bodies[0]
	mu.Unlock()
	if body["model"] != "upstream-model" || body["stream"] != true {
		t.Fatalf("bridged body model/stream wrong: %v", body)
	}
	for _, field := range []string{"store", "include", "prompt_cache_key", "text", "reasoning", "input", "instructions"} {
		if _, ok := body[field]; ok {
			t.Fatalf("responses-only field %q leaked into chat body: %v", field, body)
		}
	}
	messages := anySlice(body["messages"])
	roles := []string{}
	for _, value := range messages {
		roles = append(roles, asString(asMap(value)["role"]))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,tool" {
		t.Fatalf("parallel calls must merge into one assistant turn, roles=%v", roles)
	}
	if calls := anySlice(asMap(messages[2])["tool_calls"]); len(calls) != 2 {
		t.Fatalf("assistant turn should carry both calls: %v", messages[2])
	}
	names := []string{}
	for _, tool := range anySlice(body["tools"]) {
		names = append(names, asString(asMap(asMap(tool)["function"])["name"]))
	}
	if strings.Join(names, ",") != "shell,apply_patch" {
		t.Fatalf("tools=%v", names)
	}
	input, cached, output, costType, cost := ledgerUsage(t, a)
	if input != 120 || cached != 100 || output != 30 || costType == "unknown" || cost <= 0 {
		t.Fatalf("usage not recorded: input=%d cached=%d output=%d type=%s cost=%d", input, cached, output, costType, cost)
	}
}

func TestResponsesNativePassthroughIsByteIdenticalWithUsage(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":50,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":7}}}\n\n"
	var received []byte
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	defer done()
	if _, err := a.db.Exec(`UPDATE model_routes SET upstream_model='public-model'`); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"public-model","stream":true,"input":"hi","store":false}`
	rec := bridgeRequest(t, a, key, "/v1/responses", body)
	if rec.Code != 200 || rec.Body.String() != stream || string(received) != body {
		t.Fatalf("status=%d body=%q received=%q", rec.Code, rec.Body.String(), received)
	}
	input, cached, output, _, _ := ledgerUsage(t, a)
	if input != 50 || cached != 10 || output != 7 {
		t.Fatalf("usage input=%d cached=%d output=%d", input, cached, output)
	}
}

func TestAliasRouteRewritesOnlyModelField(t *testing.T) {
	var received map[string]json.RawMessage
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/chat/completions", `{"model":"public-model","messages":[{"role":"user","content":"hi"}],"x_unknown":{"a":1}}`)
	if rec.Code != 200 || string(received["model"]) != `"upstream-model"` || string(received["x_unknown"]) != `{"a":1}` {
		t.Fatalf("status=%d received=%v", rec.Code, received)
	}
	if input, _, output, _, _ := ledgerUsage(t, a); input != 3 || output != 1 {
		t.Fatalf("non-stream usage not tapped: %d/%d", input, output)
	}
}

func TestClaudeCodeMessagesBridgeToChatOnlyChannel(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := bridgeFixture(t, "openai_compatible", chatOnlyUpstream(t, &bodies, &mu, false))
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/messages", `{"model":"public-model","max_tokens":100,"stream":true,"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"}]}`)
	out := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(out, "event: message_start") || !strings.Contains(out, `"text":"hel"`) || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if input, _, output, _, _ := ledgerUsage(t, a); input != 120 || output != 30 {
		t.Fatalf("usage %d/%d", input, output)
	}
}

func TestChatBridgesToCodexOAuthResponses(t *testing.T) {
	var path string
	var received map[string]any
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n")
	})
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET base_url=base_url||'/backend-api/codex'`); err != nil {
		t.Fatal(err)
	}
	rec := bridgeRequest(t, a, key, "/v1/chat/completions", `{"model":"public-model","messages":[{"role":"system","content":"sys"},{"role":"user","content":"ping"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"pong"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if path != "/backend-api/codex/responses" || received["instructions"] != "sys" || received["model"] != "upstream-model" {
		t.Fatalf("path=%s body=%v", path, received)
	}
	if input, _, output, _, _ := ledgerUsage(t, a); input != 9 || output != 2 {
		t.Fatalf("usage %d/%d", input, output)
	}
}

func TestGeminiGenerateContentBridgesToChat(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	a, key, done := bridgeFixture(t, "openai_compatible", chatOnlyUpstream(t, &bodies, &mu, false))
	defer done()
	body := `{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":50}}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/public-model:streamGenerateContent?alt=sse", strings.NewReader(body))
	req.Header.Set("x-goog-api-key", key)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(out, `"text":"hel"`) || !strings.Contains(out, `"finishReason":"STOP"`) || !strings.Contains(out, `"promptTokenCount":120`) {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1beta/models/public-model:generateContent?key="+key, strings.NewReader(body))
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	var response map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &response) != nil || len(anySlice(response["candidates"])) != 1 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies[0]["model"] != "upstream-model" || asString(asMap(anySlice(bodies[0]["messages"])[0])["content"]) != "sys" {
		t.Fatalf("chat body %v", bodies[0])
	}
	// GET /v1/models/{id} must not be swallowed into the Gemini handler.
	req = httptest.NewRequest(http.MethodGet, "/v1/models/public-model", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code == 200 && bytes.Contains(rec.Body.Bytes(), []byte("candidates")) {
		t.Fatalf("model lookup routed to inference: %s", rec.Body.String())
	}
}

func TestCountTokensAnsweredLocallyWithoutMessagesChannel(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	// The first request probes the channel natively once; the refusal is
	// remembered so later requests are answered without an upstream call.
	for turn := 0; turn < 2; turn++ {
		rec := bridgeRequest(t, a, key, "/v1/messages/count_tokens", `{"model":"public-model","messages":[{"role":"user","content":"hello there"}]}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens"`) || calls.Load() != 1 {
			t.Fatalf("turn %d status=%d body=%s calls=%d", turn, rec.Code, rec.Body.String(), calls.Load())
		}
	}
}

func TestNativeClientErrorIsReturnedRaw(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/responses", `{"model":"public-model","input":"hi"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "context length exceeded") || calls.Load() != 1 {
		t.Fatalf("status=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
}
