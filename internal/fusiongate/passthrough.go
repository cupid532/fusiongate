package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
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
		b.once.Do(b.first)
		b.timer.Reset(b.idle)
	}
	if err != nil {
		b.timer.Stop()
	}
	return n, err
}
func (b *passthroughTimedBody) Close() error { b.timer.Stop(); b.cancel(); return b.ReadCloser.Close() }

func passthroughResponse(w http.ResponseWriter, resp *http.Response) (error, string) {
	defer resp.Body.Close()
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
				return e, "downstream_write_error"
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				return err, "upstream_read_error"
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
	return nil, ""
}
