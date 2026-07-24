package fusiongate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxCodexImageResponse = 128 << 20
	// maxCodexImageBatch matches the practical OpenAI Images API upper bound.
	// Codex only generates one image per Responses tool call, so FusionGate
	// fans a larger n out into concurrent single-image calls.
	maxCodexImageBatch = 10
)

type codexImageResult struct {
	Base64        string
	RevisedPrompt string
	Usage         Usage
	UpstreamError string
}

type codexImageRequestSpec struct {
	N             int
	UpstreamModel string
	Body          []byte
}

func codexImageCount(source map[string]any) (int, error) {
	if value, exists := source["n"]; exists {
		n := num(value)
		if n < 1 || n > maxCodexImageBatch {
			return 0, fmt.Errorf("Codex OAuth image generation supports n between 1 and %d", maxCodexImageBatch)
		}
		return int(n), nil
	}
	return 1, nil
}

func codexImageRequest(raw []byte, upstreamModel string) (codexImageRequestSpec, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return codexImageRequestSpec{}, err
	}
	prompt, _ := source["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return codexImageRequestSpec{}, errors.New("prompt is required")
	}
	n, err := codexImageCount(source)
	if err != nil {
		return codexImageRequestSpec{}, err
	}
	if responseFormat, _ := source["response_format"].(string); responseFormat != "" && responseFormat != "b64_json" {
		return codexImageRequestSpec{}, errors.New("Codex OAuth image generation supports response_format=b64_json only")
	}

	tool := map[string]any{"type": "image_generation"}
	for _, key := range []string{"size", "output_format", "output_compression", "background", "moderation", "partial_images"} {
		if value, exists := source[key]; exists {
			tool[key] = value
		}
	}
	if background, _ := tool["background"].(string); background == "transparent" {
		return codexImageRequestSpec{}, errors.New("Codex OAuth image generation does not support a transparent background")
	}

	// gpt-image-2 is a built-in tool backend, not the outer Responses model.
	// Synthetic discovery routes point at gpt-5.5; keep a safe fallback for
	// manually-created routes whose upstream model was set to an image alias.
	if upstreamModel == "" || strings.Contains(strings.ToLower(upstreamModel), "image") {
		upstreamModel = "gpt-5.5"
	}
	body, err := json.Marshal(map[string]any{
		"model":  upstreamModel,
		"input":  []any{map[string]any{"role": "user", "content": prompt}},
		"tools":  []any{tool},
		"stream": true,
		"store":  false,
	})
	if err != nil {
		return codexImageRequestSpec{}, err
	}
	return codexImageRequestSpec{N: n, UpstreamModel: upstreamModel, Body: body}, nil
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

func sumUsage(dst *Usage, src Usage) {
	dst.Input += src.Input
	dst.Output += src.Output
	dst.Cached += src.Cached
	dst.Reasoning += src.Reasoning
	dst.CostMicros += src.CostMicros
	if dst.CostType == "" || dst.CostType == "unknown" {
		dst.CostType = src.CostType
	}
	if src.Reported {
		dst.Reported = true
	}
}

func (a *App) generateOneCodexImage(ctx context.Context, z resolvedRoute, body []byte) (codexImageResult, attemptResult) {
	upstreamURL, err := joinURLQuery(z.Provider.BaseURL, "/responses", "")
	if err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := setProviderAuth(req, z); err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "route_configuration_error", Err: err}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, err), Err: err}
	}
	defer resp.Body.Close()
	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return codexImageResult{}, attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		return codexImageResult{}, attemptResult{
			Status:    resp.StatusCode,
			Retryable: false,
			Reason:    retryReason(resp.StatusCode, nil),
			Err:       fmt.Errorf("codex image upstream returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody))),
		}
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexImageResponse+1))
	if err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_stream_interrupted", Err: err}
	}
	if len(responseBody) > maxCodexImageResponse {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "image_response_too_large", Err: errors.New("Codex image response exceeded the gateway limit")}
	}
	image, err := parseCodexImageSSE(responseBody)
	if err != nil {
		return codexImageResult{}, attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "image_generation_failed", Err: err}
	}
	return image, attemptResult{Status: http.StatusOK}
}

func (a *App) codexImageProxy(w http.ResponseWriter, incoming *http.Request, raw []byte, z resolvedRoute, requestID string, onFirstByte func()) attemptResult {
	if err := a.ensureFreshProviderCredential(incoming.Context(), &z); err != nil {
		return attemptResult{Status: http.StatusUnauthorized, Retryable: true, Reason: "auth_expired", Err: err}
	}
	spec, err := codexImageRequest(raw, z.Route.UpstreamModel)
	if err != nil {
		// Request shape errors are client-side and must not be retried on another channel.
		fail(w, http.StatusBadRequest, "invalid_image_request", err.Error())
		return attemptResult{Status: http.StatusBadRequest, Handled: true, Reason: "invalid_image_request", Err: err}
	}

	ctx, cancel := providerContext(incoming.Context(), z.Provider)
	defer cancel()

	results := make([]codexImageResult, spec.N)
	if spec.N == 1 {
		image, result := a.generateOneCodexImage(ctx, z, spec.Body)
		if result.Status != http.StatusOK {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: result.Err}
			}
			// Do not write the response body here: leave Handled=false so runRoutes
			// can seamlessly fail over to the next eligible image channel.
			if !result.Retryable {
				fail(w, result.Status, result.Reason, "upstream image request failed and is not safe to retry")
				return attemptResult{Status: result.Status, Handled: true, Reason: result.Reason, Err: result.Err}
			}
			return result
		}
		if onFirstByte != nil {
			onFirstByte()
		}
		results[0] = image
	} else {
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			firstErr attemptResult
		)
		for i := 0; i < spec.N; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				if ctx.Err() != nil {
					return
				}
				image, result := a.generateOneCodexImage(ctx, z, spec.Body)
				if result.Status != http.StatusOK {
					mu.Lock()
					if firstErr.Status == 0 {
						firstErr = result
						cancel()
					}
					mu.Unlock()
					return
				}
				results[index] = image
			}(i)
		}
		wg.Wait()
		if firstErr.Status != 0 {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: firstErr.Err}
			}
			if !firstErr.Retryable {
				fail(w, firstErr.Status, firstErr.Reason, "upstream image request failed and is not safe to retry")
				return attemptResult{Status: firstErr.Status, Handled: true, Reason: firstErr.Reason, Err: firstErr.Err}
			}
			return firstErr
		}
		if onFirstByte != nil {
			onFirstByte()
		}
	}

	items := make([]any, 0, len(results))
	var usage Usage
	for _, image := range results {
		item := map[string]any{"b64_json": image.Base64}
		if image.RevisedPrompt != "" {
			item["revised_prompt"] = image.RevisedPrompt
		}
		items = append(items, item)
		sumUsage(&usage, image.Usage)
	}
	cost(z, &usage)
	w.Header().Set("X-FusionGate-Request-ID", requestID)
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": items})
	return attemptResult{Status: http.StatusOK, Handled: true, Usage: usage}
}
