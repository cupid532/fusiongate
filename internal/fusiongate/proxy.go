package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxBufferedUpstreamBody = 128 << 20

type proxyOptions struct {
	Endpoint           string
	RawBody            []byte
	Stream             bool
	Transparent        bool
	ParseOpenAIUse     bool
	GatewayID          string
	SafeTransportRetry bool
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func connectionHeaders(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return out
}

func copyUpstreamRequestHeaders(dst, src http.Header) {
	connectionSpecific := connectionHeaders(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonical] || connectionSpecific[canonical] {
			continue
		}
		switch strings.ToLower(canonical) {
		case "authorization", "x-api-key", "cookie", "host", "content-length", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via":
			continue
		}
		if strings.HasPrefix(strings.ToLower(canonical), "x-fusiongate-") {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func copyUpstreamResponseHeaders(dst, src http.Header) {
	connectionSpecific := connectionHeaders(src)
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeaders[canonical] || connectionSpecific[canonical] || strings.EqualFold(canonical, "Set-Cookie") {
			continue
		}
		dst.Del(canonical)
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func joinURLQuery(base, endpoint, rawQuery string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	if rawQuery != "" {
		if u.RawQuery == "" {
			u.RawQuery = rawQuery
		} else {
			u.RawQuery += "&" + rawQuery
		}
	}
	return u.String(), nil
}

func setProviderAuth(req *http.Request, z resolvedRoute) error {
	switch z.Provider.Type {
	case "openai", "openrouter", "openai_compatible":
		req.Header.Set("Authorization", "Bearer "+z.Credential)
	case "anthropic":
		req.Header.Set("x-api-key", z.Credential)
	case "gemini":
		query := req.URL.Query()
		query.Set("key", z.Credential)
		req.URL.RawQuery = query.Encode()
	default:
		return fmt.Errorf("provider type %q does not have an API credential adapter", z.Provider.Type)
	}
	return nil
}

func normalizedOpenAIBody(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = upstreamModel
	if stream {
		if streamOptions, ok := body["stream_options"].(map[string]any); ok {
			streamOptions["include_usage"] = true
		} else if _, exists := body["stream_options"]; !exists {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return json.Marshal(body)
}

func providerContext(parent context.Context, p Provider) (context.Context, context.CancelFunc) {
	timeout := p.RequestTimeoutMS
	if timeout <= 0 {
		timeout = 120000
	}
	return context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
}

func downstreamCanceled(r *http.Request) bool {
	return r.Context().Err() != nil
}

func (a *App) proxyUpstream(w http.ResponseWriter, incoming *http.Request, z resolvedRoute, options proxyOptions) attemptResult {
	if options.Transparent && z.Route.PublicName != z.Route.UpstreamModel {
		return attemptResult{Status: http.StatusServiceUnavailable, Retryable: true, Reason: "route_configuration_error", Err: fmt.Errorf("transparent routes require public_name to equal upstream_model")}
	}
	upstreamURL, err := joinURLQuery(z.Provider.BaseURL, options.Endpoint, incoming.URL.RawQuery)
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	ctx, cancel := providerContext(incoming.Context(), z.Provider)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, incoming.Method, upstreamURL, bytes.NewReader(options.RawBody))
	if err != nil {
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "route_configuration_error", Err: err}
	}
	copyUpstreamRequestHeaders(req.Header, incoming.Header)
	if _, present := incoming.Header["User-Agent"]; !present {
		// Suppress net/http's synthetic Go User-Agent when the real client sent none.
		req.Header.Set("User-Agent", "")
	}
	if !options.Transparent {
		req.Header.Set("Content-Type", "application/json")
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "application/json")
		}
		if z.Provider.Type == "anthropic" && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	if err := setProviderAuth(req, z); err != nil {
		return attemptResult{Status: http.StatusNotImplemented, Retryable: true, Reason: "route_configuration_error", Err: err}
	}

	resp, err := a.client.Do(req)
	if err != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: err}
		}
		reason := retryReason(0, err)
		return attemptResult{Status: http.StatusBadGateway, Retryable: options.SafeTransportRetry, Reason: reason, Err: err}
	}
	defer resp.Body.Close()

	if retryableStatus(resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2<<20))
		return attemptResult{Status: resp.StatusCode, Retryable: true, Reason: retryReason(resp.StatusCode, nil), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 400 {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		reason := retryReason(resp.StatusCode, nil)
		if copyErr != nil {
			reason = "downstream_write_error"
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: reason, Err: copyErr}
	}

	if options.Stream {
		first := make([]byte, 32<<10)
		n, readErr := resp.Body.Read(first)
		if n == 0 && readErr != nil && readErr != io.EOF {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
			}
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, readErr), Err: readErr}
		}
		if n == 0 && readErr == io.EOF {
			return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: "upstream_empty_stream", Err: io.ErrUnexpectedEOF}
		}
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		if n > 0 {
			if _, err := w.Write(first[:n]); err != nil {
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_write_error", Err: err}
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return attemptResult{Status: resp.StatusCode, Handled: true, Usage: Usage{CostType: "unknown"}}
		}
		_, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil {
			if downstreamCanceled(incoming) {
				return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "downstream_canceled", Err: copyErr}
			}
			return attemptResult{Status: http.StatusBadGateway, Handled: true, Reason: "upstream_stream_interrupted", Err: copyErr}
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Usage: Usage{CostType: "unknown"}}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBufferedUpstreamBody+1))
	if readErr != nil {
		if downstreamCanceled(incoming) {
			return attemptResult{Status: http.StatusBadGateway, Reason: "downstream_canceled", Err: readErr}
		}
		return attemptResult{Status: http.StatusBadGateway, Retryable: true, Reason: retryReason(0, readErr), Err: readErr}
	}
	if len(body) > maxBufferedUpstreamBody {
		copyUpstreamResponseHeaders(w.Header(), resp.Header)
		w.Header().Del("Content-Length")
		w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
		w.WriteHeader(resp.StatusCode)
		_, writeErr := w.Write(body)
		if writeErr == nil {
			_, writeErr = io.Copy(w, resp.Body)
		}
		return attemptResult{Status: resp.StatusCode, Handled: true, Reason: "large_response_streamed", Err: writeErr, Usage: Usage{CostType: "unknown"}}
	}
	usage := Usage{CostType: "unknown"}
	if options.ParseOpenAIUse && !options.Transparent {
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			usage = parseOpenAIUsage(payload)
			cost(z, &usage)
		}
	}
	copyUpstreamResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-FusionGate-Request-ID", options.GatewayID)
	w.WriteHeader(resp.StatusCode)
	_, writeErr := w.Write(body)
	return attemptResult{Status: resp.StatusCode, Handled: true, Usage: usage, Reason: func() string {
		if writeErr != nil {
			return "downstream_write_error"
		}
		return ""
	}(), Err: writeErr}
}
