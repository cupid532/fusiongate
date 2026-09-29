package fusiongate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxPassthroughBody = 32 << 20
const maxPassthroughError = 2 << 20

func passthroughModel(body []byte, contentType string) (string, bool, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err == nil && mediaType == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false, err
			}
			if part.FormName() == "model" {
				raw, err := io.ReadAll(io.LimitReader(part, 4097))
				if err != nil {
					return "", false, err
				}
				if len(raw) > 4096 {
					return "", false, errors.New("model field too large")
				}
				return string(raw), false, nil
			}
		}
		return "", false, nil
	}
	var metadata struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return "", false, err
	}
	return metadata.Model, metadata.Stream, nil
}

func passthroughProtocol(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "openai_chat"
	case "/v1/responses":
		return "openai_responses"
	case "/v1/responses/compact":
		return "openai_responses_compact"
	case "/v1/messages":
		return "anthropic_messages"
	case "/v1/messages/count_tokens":
		return "anthropic_count_tokens"
	case "/v1/images/generations":
		return "openai_images"
	case "/v1/audio/speech":
		return "openai_audio_speech"
	case "/v1/audio/transcriptions":
		return "openai_audio_transcriptions"
	case "/v1/embeddings":
		return "openai_embeddings"
	}
	return path
}

type prefixedPassthroughBody struct {
	io.Reader
	io.Closer
}

// The timer is reset by bytes, not JSON/SSE semantics. It owns no read goroutine.
type passthroughTimedBody struct {
	io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	first  func()
	once   sync.Once
	cancel context.CancelFunc
}

func (b *passthroughTimedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.first != nil {
			b.once.Do(b.first)
		}
		if b.idle > 0 {
			b.timer.Reset(b.idle)
		}
	}
	if err != nil {
		b.timer.Stop()
	}
	return n, err
}
func (b *passthroughTimedBody) Close() error { b.timer.Stop(); b.cancel(); return b.ReadCloser.Close() }

func passthroughResponse(w http.ResponseWriter, resp *http.Response) (error, string) {
	err, reason, _ := passthroughResponseUsage(w, resp, "")
	return err, reason
}

// passthroughUsageFormat names the usage payload shape a channel answers with
// on the given wire path, so the passive tap can pick the right parser.
func passthroughUsageFormat(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic"
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"):
		return "gemini"
	case strings.HasPrefix(path, "/v1/chat/completions"), strings.HasPrefix(path, "/v1/responses"), strings.HasPrefix(path, "/responses"):
		return "openai"
	}
	return ""
}

// usageTap passively observes the bytes a passthrough forwards and extracts the
// token usage from them. It never changes, delays or blocks the bytes the client
// receives: it is written to after the downstream write and cannot fail.
type usageTap struct {
	format   string
	sse      bool
	encoded  bool
	observer *sseUsageObserver
	buf      bytes.Buffer
	overflow bool
}

const maxUsageTapBody = 16 << 20

func newUsageTap(format string, header http.Header) *usageTap {
	if format == "" {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	tap := &usageTap{format: format, sse: mediaType == "text/event-stream", encoded: encoding != "" && encoding != "identity"}
	if tap.encoded && encoding != "gzip" {
		// Only gzip can be decoded without extra dependencies; other encodings
		// leave the usage unknown rather than guessing.
		return nil
	}
	if tap.sse && !tap.encoded {
		tap.observer = &sseUsageObserver{usageFormat: format}
	}
	return tap
}

func (t *usageTap) Write(p []byte) {
	if t.observer != nil {
		t.observer.Write(p)
		return
	}
	if t.overflow {
		return
	}
	if t.buf.Len()+len(p) > maxUsageTapBody {
		t.overflow = true
		t.buf.Reset()
		return
	}
	t.buf.Write(p)
}

func (t *usageTap) finish() Usage {
	if t.observer != nil {
		return t.observer.finish()
	}
	if t.overflow {
		return Usage{CostType: "unknown"}
	}
	body := t.buf.Bytes()
	if t.encoded {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return Usage{CostType: "unknown"}
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, maxUsageTapBody))
		if err != nil && len(decoded) == 0 {
			return Usage{CostType: "unknown"}
		}
		body = decoded
	}
	if t.sse {
		observer := &sseUsageObserver{usageFormat: t.format}
		observer.Write(body)
		return observer.finish()
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		// Some channels answer a non-stream request with SSE anyway.
		observer := &sseUsageObserver{usageFormat: t.format}
		observer.Write(body)
		return observer.finish()
	}
	usage := Usage{CostType: "unknown"}
	// Gemini may answer with a JSON array of chunks.
	values := []any{decoded}
	if list, ok := decoded.([]any); ok {
		values = list
	}
	for _, value := range values {
		payload, _ := value.(map[string]any)
		if payload != nil {
			mergeUsage(&usage, parseUsagePayload(t.format, payload))
		}
	}
	return usage
}

func parseUsagePayload(format string, payload map[string]any) Usage {
	switch format {
	case "anthropic":
		return parseAnthropicUsage(payload)
	case "gemini":
		return parseGeminiUsage(payload)
	}
	return parseOpenAIUsage(payload)
}

// passthroughResponseUsage forwards resp byte for byte and, when format names a
// known protocol, reports the usage the upstream included in its answer.
func passthroughResponseUsage(w http.ResponseWriter, resp *http.Response, format string) (error, string, Usage) {
	defer resp.Body.Close()
	var tap *usageTap
	if resp.StatusCode < 400 {
		tap = newUsageTap(format, resp.Header)
	}
	usage := func() Usage {
		if tap == nil {
			return Usage{CostType: "unknown"}
		}
		return tap.finish()
	}
	copyUpstreamResponseHeaders(w.Header(), resp.Header)
	skip := connectionHeaders(resp.Header)
	allowedTrailer := func(key string) bool {
		key = http.CanonicalHeaderKey(key)
		return !hopByHopHeaders[key] && !skip[key] && !gatewayOwnedResponseHeaders[key] && key != "Set-Cookie" && key != "Content-Length"
	}
	if len(resp.Trailer) > 0 {
		w.Header().Del("Content-Length")
	}
	for key := range resp.Trailer {
		if allowedTrailer(key) {
			w.Header().Add("Trailer", key)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return e, "downstream_write_error", usage()
			}
			if tap != nil {
				tap.Write(buf[:n])
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if tap != nil && tap.observer != nil {
			if tap.observer.failed {
				return errors.New("upstream stream failed"), "upstream_stream_error", usage()
			}
			if tap.observer.completed && err == nil {
				break
			}
			if err == io.EOF && !tap.observer.completed && (format == "openai" || format == "anthropic") {
				return io.ErrUnexpectedEOF, "upstream_incomplete_stream", usage()
			}
		}
		if err != nil {
			if err != io.EOF {
				// Clients commonly close the stream as soon as the protocol's
				// terminal event arrives, before the upstream HTTP body ends.
				// Only a fully written terminal event can settle cancellation;
				// partial output and downstream write failures remain errors.
				if !errors.Is(err, context.Canceled) || tap == nil || tap.observer == nil || !tap.observer.completed {
					return err, "upstream_read_error", usage()
				}
			}
			break
		}
	}
	for key, values := range resp.Trailer {
		if allowedTrailer(key) {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
	}
	return nil, "", usage()
}
