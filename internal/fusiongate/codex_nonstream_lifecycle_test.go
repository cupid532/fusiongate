package fusiongate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func codexNonstreamGzip(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	var wire bytes.Buffer
	writer := gzip.NewWriter(&wire)
	if _, err := io.Copy(writer, reader); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

func TestCodexNonstreamCollectionSuccess(t *testing.T) {
	for _, encoding := range []string{"", "gzip"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			wire := []byte(codexCompletedSSE("bounded-response"))
			if encoding == "gzip" {
				wire = codexNonstreamGzip(t, bytes.NewReader(wire))
			}
			a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Errorf("Accept-Encoding=%q, want original client negotiation", r.Header.Get("Accept-Encoding"))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				_, _ = w.Write(wire)
			})
			defer done()
			a.cfg.MaxFailoverAttempts = 1
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"hi","stream":false}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			a.Router().ServeHTTP(rec, req)
			var response map[string]any
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if response["model"] != "public-model" || response["status"] != "completed" || response["id"] != "bounded-response" {
				t.Fatalf("response=%v", response)
			}
			if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("JSON response headers=%v", rec.Header())
			}
			input, cached, output, costType, costMicros := ledgerUsage(t, a)
			if input != 7 || cached != 0 || output != 3 || costType != "estimated" || costMicros != 13 {
				t.Fatalf("usage input=%d cached=%d output=%d cost_type=%s cost=%d, want 7/0/3/estimated/13", input, cached, output, costType, costMicros)
			}
		})
	}
}

func TestCodexNonstreamCollectionMissingUsageStaysUnknown(t *testing.T) {
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"missing-usage\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/responses", `{"model":"public-model","input":"hi","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()
	var reported int
	var costType string
	var costMicros int64
	if err := a.db.QueryRow(`SELECT usage_reported,cost_type,cost_micros FROM request_ledger WHERE success=1`).Scan(&reported, &costType, &costMicros); err != nil {
		t.Fatal(err)
	}
	if reported != 0 || costType != "unknown" || costMicros != 0 {
		t.Fatalf("missing usage reported=%d cost_type=%s cost=%d, want 0/unknown/0", reported, costType, costMicros)
	}
}

type codexNonstreamBody struct {
	io.Reader
	read   int64
	closed bool
}

func (b *codexNonstreamBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += int64(n)
	return n, err
}
func (b *codexNonstreamBody) Close() error { b.closed = true; return nil }

type codexNonstreamFillReader struct{}

func (codexNonstreamFillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type codexNonstreamErrorReader struct{ err error }

func (r codexNonstreamErrorReader) Read([]byte) (int, error) { return 0, r.err }

type codexNonstreamOutput struct {
	*httptest.ResponseRecorder
	committed bool
}

func (w *codexNonstreamOutput) WriteHeader(status int) {
	w.committed = true
	w.ResponseRecorder.WriteHeader(status)
}
func (w *codexNonstreamOutput) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseRecorder.Write(p)
}

func TestCodexNonstreamCollectionFailuresReleaseUpstream(t *testing.T) {
	readFailure := errors.New("test upstream read failure")
	for _, name := range []string{"plain_overflow", "gzip_decoded_overflow", "read_error", "invalid_gzip", "truncated_gzip", "unsupported_encoding", "incomplete_sse"} {
		t.Run(name, func(t *testing.T) {
			encoding, reason := "", "upstream_read_error"
			var reader io.Reader
			switch name {
			case "plain_overflow":
				reader = io.LimitReader(codexNonstreamFillReader{}, maxBufferedUpstreamBody+1024)
				reason = "upstream_response_too_large"
			case "gzip_decoded_overflow":
				wire := codexNonstreamGzip(t, io.LimitReader(codexNonstreamFillReader{}, maxBufferedUpstreamBody+1024))
				if len(wire) >= maxBufferedUpstreamBody {
					t.Fatal("compressed test input should be small")
				}
				reader, encoding = bytes.NewReader(wire), "gzip"
				reason = "upstream_response_too_large"
			case "read_error":
				reader = io.MultiReader(strings.NewReader(codexCompletedSSE("must-not-commit")), codexNonstreamErrorReader{readFailure})
			case "invalid_gzip":
				reader, encoding = strings.NewReader("not gzip"), "gzip"
			case "truncated_gzip":
				wire := codexNonstreamGzip(t, strings.NewReader(codexCompletedSSE("must-not-commit")))
				reader, encoding = bytes.NewReader(wire[:len(wire)-4]), "gzip"
			case "unsupported_encoding":
				reader, encoding = strings.NewReader("not brotli"), "br"
			case "incomplete_sse":
				reader = strings.NewReader("data: {\"type\":\"response.created\"}\n\n")
				reason = "upstream_invalid_response"
			}
			body := &codexNonstreamBody{Reader: reader}
			var upstreamCtx context.Context
			a := &App{client: &http.Client{Transport: authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				upstreamCtx = req.Context()
				header := http.Header{"Content-Type": {"text/event-stream"}}
				if encoding != "" {
					header.Set("Content-Encoding", encoding)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body}, nil
			})}}
			run := inferenceRun{app: a, path: "/v1/responses", raw: []byte(`{"model":"gpt-public","input":"hi","stream":false}`)}
			out := &codexNonstreamOutput{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest(http.MethodPost, run.path, nil)
			result, cancel := run.attempt(out, req, codexTestRoute("https://example.test"), "", nil)
			defer cancel()
			if result.Status != http.StatusBadGateway || !result.Retryable || result.Handled || result.Response != nil || result.Err == nil || result.Reason != reason {
				t.Fatalf("result=%+v, want unhandled retryable 502/%s", result, reason)
			}
			if out.committed || out.Body.Len() != 0 || len(out.Header()) != 0 {
				t.Fatalf("failed collection committed response: headers=%v bytes=%d", out.Header(), out.Body.Len())
			}
			if !body.closed || upstreamCtx == nil || upstreamCtx.Err() != context.Canceled {
				t.Fatalf("upstream leaked: closed=%v context=%v", body.closed, upstreamCtx)
			}
			if name == "plain_overflow" && body.read != maxBufferedUpstreamBody+1 {
				t.Fatalf("read=%d, want decoded limit plus one", body.read)
			}
			if name == "read_error" && !errors.Is(result.Err, readFailure) {
				t.Fatalf("read error changed: %v", result.Err)
			}
			if name == "truncated_gzip" && !errors.Is(result.Err, io.ErrUnexpectedEOF) {
				t.Fatalf("gzip truncation error=%v", result.Err)
			}
		})
	}
}

func TestCodexNonstreamCollectionCancellation(t *testing.T) {
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	readStarted := make(chan struct{})
	reader, writer := io.Pipe()
	defer writer.Close()
	var body *codexNonstreamBody
	a := &App{client: &http.Client{Transport: authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body = &codexNonstreamBody{Reader: reader}
		go func() {
			close(readStarted)
			<-req.Context().Done()
			_ = writer.CloseWithError(req.Context().Err())
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}}
	run := inferenceRun{app: a, path: "/v1/responses", raw: []byte(`{"model":"gpt-public","input":"hi","stream":false}`)}
	out := &codexNonstreamOutput{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, run.path, nil).WithContext(ctx)
	done := make(chan attemptResult, 1)
	go func() {
		result, cancel := run.attempt(out, req, codexTestRoute("https://example.test"), "", nil)
		cancel()
		done <- result
	}()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("upstream collection did not start")
	}
	cancelRequest()
	select {
	case result := <-done:
		if !errors.Is(result.Err, context.Canceled) || result.Handled || result.Response != nil || out.committed || !body.closed {
			t.Fatalf("canceled collection result=%+v committed=%v closed=%v", result, out.committed, body.closed)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled collection did not release upstream")
	}
}

func TestCodexNonstreamCollectionPreservesNativeWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		stream bool
		body   string
	}{
		{"gzip_error", http.StatusUnprocessableEntity, false, `{"error":{"code":"invalid_request","message":"invalid input"}}`},
		{"gzip_redirect", http.StatusTemporaryRedirect, false, "redirect response"},
		{"gzip_stream", http.StatusOK, true, codexCompletedSSE("native-stream")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := codexNonstreamGzip(t, strings.NewReader(tc.body))
			a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.status == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "/must-not-follow")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(wire)
			})
			defer done()
			a.cfg.MaxFailoverAttempts = 1
			raw := `{"model":"public-model","input":"hi","stream":false}`
			if tc.stream {
				raw = `{"model":"public-model","input":"hi","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			a.Router().ServeHTTP(rec, req)
			if rec.Code != tc.status || rec.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(rec.Body.Bytes(), wire) {
				t.Fatalf("native wire changed: status=%d encoding=%q bytes=%d want=%d", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len(), len(wire))
			}
		})
	}
}
