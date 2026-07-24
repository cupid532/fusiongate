package fusiongate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxCodexImageResponse = 128 << 20

type codexImageResult struct {
	Base64        string
	RevisedPrompt string
	Usage         Usage
	UpstreamError string
}

func codexImageRequest(raw []byte, upstreamModel string) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	prompt, _ := source["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if value, exists := source["n"]; exists {
		n := num(value)
		if n != 1 {
			return nil, errors.New("Codex OAuth image generation supports exactly one image per request (n=1)")
		}
	}
	if responseFormat, _ := source["response_format"].(string); responseFormat != "" && responseFormat != "b64_json" {
		return nil, errors.New("Codex OAuth image generation supports response_format=b64_json only")
	}

	tool := map[string]any{"type": "image_generation"}
	for _, key := range []string{"size", "output_format", "output_compression", "background", "moderation", "partial_images"} {
		if value, exists := source[key]; exists {
			tool[key] = value
		}
	}
	if background, _ := tool["background"].(string); background == "transparent" {
		return nil, errors.New("Codex OAuth image generation does not support a transparent background")
	}

	// gpt-image-2 is a built-in tool backend, not the outer Responses model.
	// Synthetic discovery routes point at gpt-5.5; keep a safe fallback for
	// manually-created routes whose upstream model was set to an image alias.
	if upstreamModel == "" || strings.Contains(strings.ToLower(upstreamModel), "image") {
		upstreamModel = "gpt-5.5"
	}
	return json.Marshal(map[string]any{
		"model":  upstreamModel,
		"input":  []any{map[string]any{"role": "user", "content": prompt}},
		"tools":  []any{tool},
		"stream": true,
		"store":  false,
	})
}

func parseCodexImageSSE(raw []byte) (codexImageResult, error) {
	var result codexImageResult
	normalized := bytes.ReplaceAll(raw, []byte("\r"), nil)
	for _, event := range bytes.Split(normalized, []byte("\n\n")) {
		var data []string
		for _, line := range strings.Split(strings.TrimSpace(string(event)), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(payload), &decoded) != nil {
			continue
		}
		findCodexImagePayload(decoded, &result)
		if decoded["type"] == "response.completed" {
			if response, ok := decoded["response"].(map[string]any); ok {
				result.Usage = parseOpenAIUsage(response)
			}
		}
	}
	if result.Base64 == "" {
		if result.UpstreamError != "" {
			return result, fmt.Errorf("image tool failed: %s", result.UpstreamError)
		}
		return result, errors.New("Codex response completed without an image result")
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(result.Base64))
	if _, err := io.Copy(io.Discard, decoder); err != nil {
		return codexImageResult{}, errors.New("Codex returned an invalid base64 image result")
	}
	return result, nil
}

func findCodexImagePayload(value any, result *codexImageResult) {
	switch item := value.(type) {
	case map[string]any:
		if itemType, _ := item["type"].(string); itemType == "image_generation_call" {
			if encoded, _ := item["result"].(string); encoded != "" {
				result.Base64 = encoded
			}
			if revised, _ := item["revised_prompt"].(string); revised != "" {
				result.RevisedPrompt = revised
			}
		}
		if errValue, ok := item["error"].(map[string]any); ok && result.UpstreamError == "" {
			if message, _ := errValue["message"].(string); message != "" {
				result.UpstreamError = message
			} else if code, _ := errValue["code"].(string); code != "" {
				result.UpstreamError = code
			}
		}
		for _, child := range item {
			findCodexImagePayload(child, result)
		}
	case []any:
		for _, child := range item {
			findCodexImagePayload(child, result)
		}
	}
}

func (a *App) codexImageProxy(w http.ResponseWriter, incoming *http.Request, raw []byte, z resolvedRoute, requestID string, onFirstByte func()) attemptResult {
	if err := a.ensureFreshProviderCredential(incoming.Context(), &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}
	}
	body, err := codexImageRequest(raw, z.Route.UpstreamModel)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_image_request", err.Error())
		return attemptResult{Status: http.StatusBadRequest, Handled: true, Reason: "invalid_image_request", Err: err}
	}
	upstreamURL, err := joinURLQuery(z.Provider.BaseURL, "/responses", "")
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	ctx, cancel := providerContext(incoming.Context(), z.Provider)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "route_configuration_error", Err: err}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		return attemptResult{Status: http.StatusBadGateway, Reason: retryReason(0, err), Err: err}
	}
	defer resp.Body.Close()
	resp.Body = observeFirstByte(resp.Body, onFirstByte)
	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-FusionGate-Request-ID", requestID)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: retryReason(resp.StatusCode, nil), Err: copyErr}
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexImageResponse+1))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Reason: "upstream_stream_interrupted", Err: err}
	}
	if len(responseBody) > maxCodexImageResponse {
		fail(w, http.StatusBadGateway, "image_response_too_large", "Codex image response exceeded the gateway limit")
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "image_response_too_large"}
	}
	image, err := parseCodexImageSSE(responseBody)
	if err != nil {
		fail(w, http.StatusBadGateway, "image_generation_failed", err.Error())
		return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "image_generation_failed", Err: err}
	}
	cost(z, &image.Usage)
	item := map[string]any{"b64_json": image.Base64}
	if image.RevisedPrompt != "" {
		item["revised_prompt"] = image.RevisedPrompt
	}
	w.Header().Set("X-FusionGate-Request-ID", requestID)
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []any{item}})
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: image.Usage}
}
