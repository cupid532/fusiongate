package fusiongate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type completionTestBody struct {
	chunks []string
	err    error
}

func (b *completionTestBody) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, b.err
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if b.chunks[0] == "" {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}
func (*completionTestBody) Close() error { return nil }

func TestPassthroughTerminalEventCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, format, body string
		success            bool
	}{
		{"chat done", "openai", "data: [DONE]\n\n", true},
		{"responses complete", "openai", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n", true},
		{"anthropic stop", "anthropic", "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n", true},
		{"partial output", "openai", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", false},
		{"partial delimiter", "openai", "data: [DONE]\n", false},
		{"partial json", "openai", "data: {\"type\":\"response.completed\"\n\n", false},
		{"missing response", "openai", "data: {\"type\":\"response.completed\"}\n\n", false},
		{"incomplete", "openai", "data: {\"type\":\"response.incomplete\"}\n\ndata: [DONE]\n\n", false},
		{"failed", "openai", "data: {\"type\":\"response.failed\"}\n\ndata: [DONE]\n\n", false},
		{"error after done", "openai", "data: [DONE]\n\ndata: {\"error\":{\"message\":\"failed\"}}\n\n", false},
		{"marker inside text", "openai", "data: {\"text\":\"data: [DONE]\\n\\n\"}\n\n", false},
		{"unknown protocol", "gemini", "data: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One byte per read also splits CRLF and the terminal delimiter.
			chunks := strings.Split(tc.body, "")
			rec := httptest.NewRecorder()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &completionTestBody{chunks: chunks, err: context.Canceled}}
			err, reason, usage := passthroughResponseUsage(rec, resp, tc.format)
			if (err == nil) != tc.success {
				t.Fatalf("err=%v reason=%s", err, reason)
			}
			if rec.Body.String() != tc.body {
				t.Fatal("passthrough bytes changed")
			}
			if tc.name == "responses complete" && (!usage.Reported || usage.Input != 3 || usage.Output != 5) {
				t.Fatalf("usage=%+v", usage)
			}
		})
	}
	for _, readErr := range []error{context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &completionTestBody{chunks: []string{"data: [DONE]\n\n"}, err: readErr}}
		err, _, _ := passthroughResponseUsage(httptest.NewRecorder(), resp, "openai")
		if !errors.Is(err, readErr) {
			t.Fatalf("masked read failure: %v", err)
		}
	}
}

type cancelAfterWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	fail   bool
}

func (w *cancelAfterWrite) Write(p []byte) (int, error) {
	if w.fail {
		w.cancel()
		return 0, io.ErrClosedPipe
	}
	n, err := w.ResponseRecorder.Write(p)
	w.cancel()
	return n, err
}

func TestInferenceCompletedResponseSurvivesClientCancel(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		stream, hold, fail      bool
		success                 int
	}{
		{"JSON completed", "application/json", `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`, false, false, false, 1},
		{"SSE terminal before HTTP EOF", "text/event-stream", "data: [DONE]\n\n", true, true, false, 1},
		{"SSE canceled mid generation", "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", true, true, false, 0},
		{"terminal write failed", "text/event-stream", "data: [DONE]\n\n", true, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var backup atomic.Int32
			a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", tc.contentType)
				io.WriteString(w, tc.body)
				if tc.hold {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}, func(w http.ResponseWriter, r *http.Request) { backup.Add(1) })
			defer done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw := `{"model":"native","stream":false}`
			if tc.stream {
				raw = `{"model":"native","stream":true}`
			}
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(raw)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+key)
			rec := &cancelAfterWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, fail: tc.fail}
			a.Router().ServeHTTP(rec, req)
			a.flushLedgerWrites()
			var success int
			var reason string
			if err := a.db.QueryRow(`SELECT success,error_type FROM request_ledger WHERE attempt>0 ORDER BY id LIMIT 1`).Scan(&success, &reason); err != nil {
				t.Fatal(err)
			}
			if success != tc.success {
				t.Fatalf("success=%d reason=%s", success, reason)
			}
			if success == 1 && reason != "" {
				t.Fatalf("completed response logged as %s", reason)
			}
			if success == 0 && reason != "downstream_canceled" {
				t.Fatalf("cancellation not recorded: %s", reason)
			}
			if backup.Load() != 0 {
				t.Fatal("retried after client canceled")
			}
		})
	}
}
