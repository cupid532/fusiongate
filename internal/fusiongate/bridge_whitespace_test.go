package fusiongate

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBridgeWhitespacePayloads(t *testing.T) {
	fragments := []string{" hello", " ", "world ", "\n", "\t", "  "}
	want := strings.Join(fragments, "")
	for _, protocol := range []string{wireResponses, wireMessages} {
		t.Run(protocol, func(t *testing.T) {
			var source, out bytes.Buffer
			emit := func(value map[string]any) {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				source.WriteString("data: " + string(encoded) + "\n\n")
			}
			if protocol == wireResponses {
				emit(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "item", "call_id": "call", "name": "exec"}})
				for _, fragment := range fragments {
					for _, kind := range []string{"response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta"} {
						emit(map[string]any{"type": kind, "item_id": "item", "delta": fragment})
					}
				}
				emit(map[string]any{"type": "response.completed", "response": map[string]any{}})
			} else {
				emit(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call", "name": "exec"}})
				for _, fragment := range fragments {
					for kind, key := range map[string]string{"text_delta": "text", "thinking_delta": "thinking", "input_json_delta": "partial_json"} {
						emit(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": kind, key: fragment}})
					}
				}
				emit(map[string]any{"type": "message_stop"})
			}
			e := chatChunkEmitter{w: &out, id: "id", model: "model"}
			var err error
			if protocol == wireResponses {
				err = e.fromResponsesSSE(&source)
			} else {
				err = e.fromAnthropicSSE(&source)
			}
			if err != nil {
				t.Fatal(err)
			}
			source.Reset()
			source.Write(out.Bytes())
			source.WriteString("data: [DONE]\n\n")
			var text, reason, args strings.Builder
			err = forEachChatChunk(&source, func(chunk map[string]any) error {
				choices := anySlice(chunk["choices"])
				if len(choices) == 0 {
					return nil
				}
				delta := asMap(asMap(choices[0])["delta"])
				text.WriteString(asStringValue(delta["content"]))
				reason.WriteString(asStringValue(delta["reasoning_content"]))
				for _, call := range anySlice(delta["tool_calls"]) {
					args.WriteString(asStringValue(asMap(asMap(call)["function"])["arguments"]))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for name, got := range map[string]string{"text": text.String(), "reason": reason.String(), "arguments": args.String()} {
				if got != want {
					t.Fatalf("%s: got %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestClientWritersWhitespacePayloads(t *testing.T) {
	fragments := []string{" ", "\t", "\n", " hello "}
	want := strings.Join(fragments, "")
	if got := firstNonEmptyStringValue(" ", "fallback"); got != " " {
		t.Fatalf("fallback trimmed %q", got)
	}
	var source bytes.Buffer
	e := chatChunkEmitter{w: &source, id: "id", model: "model"}
	for _, fragment := range fragments {
		if err := e.chunk(map[string]any{"content": fragment, "reasoning_content": fragment}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	source.WriteString("data: [DONE]\n\n")
	rec := httptest.NewRecorder()
	result := writeGeminiStream(rec, strings.NewReader(source.String()), "model", "rid", false)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var chunks []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &chunks); err != nil {
		t.Fatal(err)
	}
	var text, reason strings.Builder
	for _, chunk := range chunks {
		candidate := asMap(anySlice(chunk["candidates"])[0])
		for _, raw := range anySlice(asMap(candidate["content"])["parts"]) {
			part := asMap(raw)
			if thought, _ := part["thought"].(bool); thought {
				reason.WriteString(asStringValue(part["text"]))
			} else {
				text.WriteString(asStringValue(part["text"]))
			}
		}
	}
	if text.String() != want || reason.String() != want {
		t.Fatalf("Gemini text=%q reason=%q want=%q", text.String(), reason.String(), want)
	}
}

func TestResponsesWriterPreservesWhitespaceAndCommand(t *testing.T) {
	var source bytes.Buffer
	e := chatChunkEmitter{w: &source, id: "id", model: "model"}
	fragments := []string{" ", "\t", "\n", " hello "}
	for _, fragment := range fragments {
		if err := e.chunk(map[string]any{"content": fragment, "reasoning_content": fragment}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"{\"command\":\"ls", " ", "-la\"}"}
	for _, fragment := range args {
		if err := e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call", "function": map[string]any{"name": "exec", "arguments": fragment}}}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	source.WriteString("data: [DONE]\n\n")
	rec := httptest.NewRecorder()
	result := writeResponsesStream(rec, &source, "model", "rid", nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var text, reason, arguments strings.Builder
	err := forEachSSEEvent(strings.NewReader(rec.Body.String()), func(event string, data map[string]any) (bool, error) {
		switch event {
		case "response.output_text.delta":
			text.WriteString(asStringValue(data["delta"]))
		case "response.reasoning_summary_text.delta":
			reason.WriteString(asStringValue(data["delta"]))
		case "response.function_call_arguments.delta":
			arguments.WriteString(asStringValue(data["delta"]))
		case "response.completed":
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(fragments, "")
	if text.String() != want || reason.String() != want || arguments.String() != strings.Join(args, "") {
		t.Fatalf("text=%q reason=%q arguments=%q", text.String(), reason.String(), arguments.String())
	}
}
