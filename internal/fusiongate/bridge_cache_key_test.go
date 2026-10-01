package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Match the actual OpenCode request structure that was rejected before an
// upstream call: ordinary function tools and a camelCase promptCacheKey.
func TestOpenCodeCacheKeyReachesCodex(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   any
		stream bool
	}{
		{"opencode_stream", map[string]any{"promptCacheKey": "cache-session"}, "cache-session", true},
		{"opencode_json", map[string]any{"promptCacheKey": "cache-session"}, "cache-session", false},
		{"canonical", map[string]any{"prompt_cache_key": "cache-session"}, "cache-session", true},
		{"matching_aliases", map[string]any{"prompt_cache_key": "cache-session", "promptCacheKey": "cache-session"}, "cache-session", true},
		{"empty_alias", map[string]any{"promptCacheKey": ""}, "", true},
		{"null_alias", map[string]any{"promptCacheKey": nil}, nil, true},
		{"null_canonical", map[string]any{"prompt_cache_key": nil, "promptCacheKey": "cache-session"}, "cache-session", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan map[string]any, 4)
			a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Errorf("path=%s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				received <- body
				// The real Codex backend can omit this header.
				w.Header()["Content-Type"] = nil
				io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"call1\",\"name\":\"test_echo\",\"arguments\":\"\"}}\n\n"+
					"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc1\",\"delta\":\"{\\\"text\\\":\\\"ok\\\"}\"}\n\n"+
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":5}}}\n\n")
			})
			defer done()
			body := map[string]any{
				"model": "public-model", "stream": tc.stream, "max_tokens": 4096,
				"messages":       []any{map[string]any{"role": "system", "content": "Use tools when helpful."}, map[string]any{"role": "user", "content": "say ok"}},
				"stream_options": map[string]any{"include_usage": true}, "tool_choice": "auto",
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "test_echo", "strict": false, "parameters": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}}}},
			}
			for field, value := range tc.fields {
				body[field] = value
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rec := bridgeRequest(t, a, key, "/v1/chat/completions", string(raw))
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var upstream map[string]any
			select {
			case upstream = <-received:
			default:
				t.Fatal("Codex was never called")
			}
			if upstream["prompt_cache_key"] != tc.want {
				t.Fatalf("cache key=%#v want=%#v", upstream["prompt_cache_key"], tc.want)
			}
			if _, ok := upstream["promptCacheKey"]; ok {
				t.Fatal("camelCase cache option leaked upstream")
			}
			if upstream["model"] != "upstream-model" || upstream["store"] != false || upstream["stream"] != true || upstream["instructions"] != "Use tools when helpful." {
				t.Fatalf("request contract changed: %v", upstream)
			}
			tool := asMap(anySlice(upstream["tools"])[0])
			if tool["name"] != "test_echo" || tool["strict"] != false || upstream["tool_choice"] != "auto" {
				t.Fatalf("function tool changed: %v", upstream)
			}
			if !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(rec.Body.String(), `"name":"test_echo"`) {
				t.Fatalf("tool result missing: %s", rec.Body.String())
			}
			if tc.stream && !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("stream did not complete: %s", rec.Body.String())
			}
			if !tc.stream && !json.Valid(rec.Body.Bytes()) {
				t.Fatal("non-stream response is not JSON")
			}
			a.flushLedgerWrites()
			var success, provider int
			var adapter string
			if err := a.db.QueryRow(`SELECT l.success,l.provider_id,a.adapter_id FROM request_ledger l JOIN inference_attempts a ON l.request_id=a.request_id ORDER BY l.id DESC LIMIT 1`).Scan(&success, &provider, &adapter); err != nil {
				t.Fatal(err)
			}
			if success != 1 || provider == 0 || adapter != "bridge:chat_to_responses" {
				t.Fatalf("success=%d provider=%d adapter=%s", success, provider, adapter)
			}
		})
	}
}

func TestBridgeCacheKeyRejectsConflictsAndUnsupportedTargets(t *testing.T) {
	for _, fields := range []string{
		`"promptCacheKey":"secret-one","prompt_cache_key":"secret-two"`,
		`"promptCacheKey":3`, `"prompt_cache_key":false`,
		`"promptCacheKey":{"private":"value"}`, `"prompt_cache_key":["value"]`,
	} {
		raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` + fields + `}`)
		err := validateBridgeRoute(wireChat, wireResponses, raw, "/v1/chat/completions")
		if err == nil {
			t.Fatalf("invalid cache field accepted: %s", fields)
		}
		if strings.Contains(bridgeRejectionReason(err), "secret-") || strings.Contains(bridgeRejectionReason(err), "private") {
			t.Fatal("cache value leaked into diagnostics")
		}
	}
	// A Messages target cannot carry a cache identity, so the key is dropped rather
	// than refusing the request. Blocking a whole channel family over a cost hint
	// kept Codex and OpenAI-style clients off every Claude-compatible channel.
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"promptCacheKey":"session"}`)
	if err := validateBridgeRoute(wireChat, wireMessages, raw, "/v1/chat/completions"); err != nil {
		t.Fatalf("cache key hint must be droppable for a messages target: %v", err)
	}
}

func TestCacheKeyNativeRequestRemainsUntouched(t *testing.T) {
	raw := `{ "model":"public-model", "messages":[{"role":"user","content":"hi"}], "promptCacheKey":"session", "stream":false }`
	received := make(chan string, 1)
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	})
	defer done()
	if _, err := a.db.Exec(`UPDATE model_routes SET upstream_model='public-model'`); err != nil {
		t.Fatal(err)
	}
	rec := bridgeRequest(t, a, key, "/v1/chat/completions", raw)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := <-received; got != raw {
		t.Fatalf("native bytes changed: %s", got)
	}
}
