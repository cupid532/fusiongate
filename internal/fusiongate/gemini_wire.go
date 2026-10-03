package fusiongate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Gemini generateContent is bridged through the chat intermediate form like the
// other protocols. The conversion is lenient: fields without a chat equivalent
// are dropped rather than rejected, so a client that sends newer fields keeps
// working.

func geminiRequestToChat(raw []byte) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	messages := make([]any, 0)
	if system := geminiPartsText(asMap(source["systemInstruction"])["parts"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	// Gemini pairs a function response with its call by name. Chat pairs them by
	// id, so ids are minted in order and handed back out per name.
	pending := map[string][]string{}
	nextID := 0
	for _, value := range anySlice(source["contents"]) {
		content := asMap(value)
		role := asString(content["role"])
		parts := anySlice(content["parts"])
		if role == "model" {
			var text strings.Builder
			calls := make([]any, 0)
			for _, rawPart := range parts {
				part := asMap(rawPart)
				if thought, _ := part["thought"].(bool); thought {
					continue
				}
				text.WriteString(asStringValue(part["text"]))
				if call := asMap(part["functionCall"]); call != nil {
					name := asString(call["name"])
					id := asString(call["id"])
					if id == "" {
						id = fmt.Sprintf("call_gemini_%d", nextID)
						nextID++
					}
					pending[name] = append(pending[name], id)
					arguments, _ := json.Marshal(call["args"])
					if call["args"] == nil {
						arguments = []byte("{}")
					}
					calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}})
				}
			}
			message := map[string]any{"role": "assistant", "content": text.String()}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			messages = append(messages, message)
			continue
		}
		userParts := make([]any, 0)
		for _, rawPart := range parts {
			part := asMap(rawPart)
			if response := asMap(part["functionResponse"]); response != nil {
				name := asString(response["name"])
				id := asString(response["id"])
				if id == "" {
					if queue := pending[name]; len(queue) > 0 {
						id, pending[name] = queue[0], queue[1:]
					} else {
						id = fmt.Sprintf("call_gemini_%d", nextID)
						nextID++
					}
				}
				output, _ := json.Marshal(response["response"])
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": string(output)})
				continue
			}
			if text := asStringValue(part["text"]); text != "" {
				userParts = append(userParts, map[string]any{"type": "text", "text": text})
			}
			if inline := asMap(firstPresent(part, "inlineData", "inline_data")); inline != nil {
				mimeType := firstNonEmpty(asString(inline["mimeType"]), asString(inline["mime_type"]), "application/octet-stream")
				userParts = append(userParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mimeType + ";base64," + asString(inline["data"])}})
			}
			if file := asMap(firstPresent(part, "fileData", "file_data")); file != nil {
				if uri := firstNonEmpty(asString(file["fileUri"]), asString(file["file_uri"])); uri != "" {
					userParts = append(userParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}})
				}
			}
		}
		if len(userParts) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": userParts})
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("request contains no contents")
	}
	chat := map[string]any{"messages": messages}
	config := asMap(source["generationConfig"])
	for from, to := range map[string]string{"temperature": "temperature", "topP": "top_p", "maxOutputTokens": "max_tokens", "stopSequences": "stop", "presencePenalty": "presence_penalty", "frequencyPenalty": "frequency_penalty", "seed": "seed"} {
		if value, ok := config[from]; ok {
			chat[to] = value
		}
	}
	if asString(config["responseMimeType"]) == "application/json" {
		if schema := firstPresent(config, "responseJsonSchema", "responseSchema"); schema != nil {
			chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": geminiSchema(schema)}}
		} else {
			chat["response_format"] = map[string]any{"type": "json_object"}
		}
	}
	tools := make([]any, 0)
	for _, value := range anySlice(source["tools"]) {
		for _, rawDeclaration := range anySlice(firstPresent(asMap(value), "functionDeclarations", "function_declarations")) {
			declaration := asMap(rawDeclaration)
			parameters := firstPresent(declaration, "parametersJsonSchema", "parameters")
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			function := map[string]any{"name": declaration["name"], "parameters": geminiSchema(parameters)}
			if description := asString(declaration["description"]); description != "" {
				function["description"] = description
			}
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
	}
	if len(tools) > 0 {
		chat["tools"] = tools
		calling := asMap(asMap(source["toolConfig"])["functionCallingConfig"])
		switch strings.ToUpper(asString(calling["mode"])) {
		case "ANY":
			if names := anySlice(calling["allowedFunctionNames"]); len(names) == 1 {
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": names[0]}}
			} else {
				chat["tool_choice"] = "required"
			}
		case "NONE":
			chat["tool_choice"] = "none"
		}
	}
	return json.Marshal(chat)
}

func geminiPartsText(parts any) string {
	texts := make([]string, 0)
	for _, value := range anySlice(parts) {
		if text := asStringValue(asMap(value)["text"]); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// geminiSchema converts Gemini's OpenAPI subset (upper-case type names) into
// the JSON Schema chat upstreams expect.
func geminiSchema(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "type" {
				if name, ok := item.(string); ok {
					out[key] = strings.ToLower(name)
					continue
				}
			}
			if key == "nullable" || key == "propertyOrdering" {
				continue
			}
			out[key] = geminiSchema(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = geminiSchema(item)
		}
		return out
	}
	return value
}

func geminiFinishReason(reason string) string {
	switch reason {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	}
	return "STOP"
}

func geminiUsageFromChat(usage map[string]any) map[string]any {
	input := num(usage["prompt_tokens"])
	output := num(usage["completion_tokens"])
	out := map[string]any{"promptTokenCount": input, "candidatesTokenCount": output, "totalTokenCount": input + output}
	if cached := num(asMap(usage["prompt_tokens_details"])["cached_tokens"]); cached > 0 {
		out["cachedContentTokenCount"] = cached
	}
	if reasoning := num(asMap(usage["completion_tokens_details"])["reasoning_tokens"]); reasoning > 0 {
		out["thoughtsTokenCount"] = reasoning
	}
	return out
}

func geminiCallPart(name string, arguments string) map[string]any {
	var args any = map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if json.Unmarshal([]byte(arguments), &args) != nil {
			args = map[string]any{"input": arguments}
		}
	}
	return map[string]any{"functionCall": map[string]any{"name": name, "args": args}}
}

func geminiResponseFromChat(chat []byte, model string) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(chat, &source); err != nil {
		return nil, err
	}
	choices := anySlice(source["choices"])
	if len(choices) == 0 {
		return nil, errors.New("upstream response contained no choices")
	}
	choice := asMap(choices[0])
	message := asMap(choice["message"])
	parts := make([]any, 0)
	if reasoning := firstNonEmptyStringValue(asStringValue(message["reasoning_content"]), asStringValue(message["reasoning"])); reasoning != "" {
		parts = append(parts, map[string]any{"text": reasoning, "thought": true})
	}
	if text := textContent(message["content"]); text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, value := range anySlice(message["tool_calls"]) {
		function := asMap(asMap(value)["function"])
		parts = append(parts, geminiCallPart(asString(function["name"]), asStringValue(function["arguments"])))
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}
	response := map[string]any{
		"candidates":   []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": geminiFinishReason(asString(choice["finish_reason"]))}},
		"modelVersion": model,
		"responseId":   "resp_" + requestID(),
	}
	if usage := asMap(source["usage"]); usage != nil {
		response["usageMetadata"] = geminiUsageFromChat(usage)
	}
	return json.Marshal(response)
}

// writeGeminiStream renders chat chunks as streamGenerateContent output: SSE
// when the client asked for alt=sse, otherwise a JSON array streamed element by
// element.
func writeGeminiStream(w http.ResponseWriter, r io.Reader, model, rid string, sse bool) attemptResult {
	contentType := "application/json"
	if sse {
		contentType = "text/event-stream"
	}
	out := &lazyStream{w: w, contentType: contentType, rid: rid}
	responseID := "resp_" + strings.TrimPrefix(rid, "req_")
	written := 0
	emit := func(parts []any, finish string, usage map[string]any) error {
		candidate := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}
		if finish != "" {
			candidate["finishReason"] = finish
		}
		payload := map[string]any{"candidates": []any{candidate}, "modelVersion": model, "responseId": responseID}
		if usage != nil {
			payload["usageMetadata"] = usage
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		switch {
		case sse:
			_, err = fmt.Fprintf(out, "data: %s\r\n\r\n", encoded)
		case written == 0:
			_, err = fmt.Fprintf(out, "[%s", encoded)
		default:
			_, err = fmt.Fprintf(out, ",\r\n%s", encoded)
		}
		written++
		return err
	}
	type call struct {
		name      string
		arguments strings.Builder
	}
	calls := map[int]*call{}
	order := []int{}
	finish := ""
	var usage map[string]any
	err := forEachChatChunk(r, func(chunk map[string]any) error {
		if value := asMap(chunk["usage"]); value != nil {
			usage = geminiUsageFromChat(value)
		}
		choices := anySlice(chunk["choices"])
		if len(choices) == 0 {
			return nil
		}
		choice := asMap(choices[0])
		if reason := asString(choice["finish_reason"]); reason != "" {
			finish = reason
		}
		delta := asMap(choice["delta"])
		for _, value := range anySlice(delta["tool_calls"]) {
			entry := asMap(value)
			index := int(num(entry["index"]))
			item := calls[index]
			if item == nil {
				item = &call{}
				calls[index] = item
				order = append(order, index)
			}
			function := asMap(entry["function"])
			if name := asString(function["name"]); name != "" {
				item.name = name
			}
			item.arguments.WriteString(asStringValue(function["arguments"]))
		}
		parts := make([]any, 0, 2)
		if reasoning := firstNonEmptyStringValue(asStringValue(delta["reasoning_content"]), asStringValue(delta["reasoning"])); reasoning != "" {
			parts = append(parts, map[string]any{"text": reasoning, "thought": true})
		}
		if text := asStringValue(delta["content"]); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		if len(parts) == 0 {
			return nil
		}
		return emit(parts, "", nil)
	})
	if err == nil {
		if written == 0 && finish == "" && len(calls) == 0 {
			return out.result(errors.New("upstream stream ended before model output"))
		}
		parts := make([]any, 0, len(order))
		for _, index := range order {
			parts = append(parts, geminiCallPart(calls[index].name, calls[index].arguments.String()))
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"text": ""})
		}
		err = emit(parts, geminiFinishReason(finish), usage)
		if err == nil && !sse {
			_, err = io.WriteString(out, "]")
		}
	}
	return out.result(err)
}

// geminiCountTokens answers countTokens locally; no bridged channel exposes an
// equivalent and the estimate is what clients use it for.
func geminiCountTokens(raw []byte) []byte {
	total := int64(0)
	var source map[string]any
	if json.Unmarshal(raw, &source) == nil {
		if request := asMap(source["generateContentRequest"]); request != nil {
			source = request
		}
		if encoded, err := geminiRequestToChat(mustJSON(source)); err == nil {
			total = int64(len(encoded)+3) / 4
		}
	}
	encoded, _ := json.Marshal(map[string]any{"totalTokens": total})
	return encoded
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
