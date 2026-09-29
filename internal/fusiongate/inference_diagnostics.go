package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type diagnosticsKey struct{}
type attemptDiagnostics struct {
	mu                    sync.Mutex
	start                 time.Time
	gotConn, wroteRequest time.Time
	ConnectionWaitMS      *int64 `json:"connection_wait_ms,omitempty"`
	UpstreamWaitMS        *int64 `json:"upstream_wait_ms,omitempty"`
	FirstByteMS           *int64 `json:"first_byte_ms,omitempty"`
	FirstOutputMS         *int64 `json:"first_output_ms,omitempty"`
	DownstreamWriteMS     int64  `json:"downstream_write_ms"`
	RetryWaitMS           int64  `json:"retry_wait_ms"`
	ConversionPrepareMS   *int64 `json:"conversion_prepare_ms,omitempty"`
	ConnectionReused      bool   `json:"connection_reused"`
	Termination           string `json:"termination_reason"`
	writeDuration         time.Duration
	timeout               string
}

func diagnosticFor(ctx context.Context) *attemptDiagnostics {
	d, _ := ctx.Value(diagnosticsKey{}).(*attemptDiagnostics)
	return d
}
func (d *attemptDiagnostics) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { d.mu.Lock(); d.gotConn = time.Now(); d.mu.Unlock() },
		GotConn: func(info httptrace.GotConnInfo) {
			d.mu.Lock()
			defer d.mu.Unlock()
			ms := time.Since(d.gotConn).Milliseconds()
			d.ConnectionWaitMS = &ms
			d.ConnectionReused = info.Reused
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { d.mu.Lock(); d.wroteRequest = time.Now(); d.mu.Unlock() },
		GotFirstResponseByte: func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if !d.wroteRequest.IsZero() {
				ms := time.Since(d.wroteRequest).Milliseconds()
				d.UpstreamWaitMS = &ms
			}
		},
	}
}
func (d *attemptDiagnostics) firstByte() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FirstByteMS == nil {
		v := time.Since(d.start).Milliseconds()
		d.FirstByteMS = &v
	}
}
func (d *attemptDiagnostics) firstOutput() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.FirstOutputMS == nil {
		v := time.Since(d.start).Milliseconds()
		d.FirstOutputMS = &v
	}
}
func (d *attemptDiagnostics) conversion(elapsed time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := elapsed.Milliseconds()
	d.ConversionPrepareMS = &v
}
func (d *attemptDiagnostics) timedOut(kind string) { d.mu.Lock(); d.timeout = kind; d.mu.Unlock() }
func (d *attemptDiagnostics) encoded(reason string, success bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Termination = reason
	if success {
		d.Termination = "completed"
	} else if d.timeout != "" && reason != "downstream_canceled" && reason != "downstream_write_error" {
		d.Termination = d.timeout
	}
	if d.Termination == "" {
		d.Termination = "upstream_http_error"
	}
	d.DownstreamWriteMS = d.writeDuration.Milliseconds()
	raw, _ := json.Marshal(d)
	return string(raw)
}

// Measures only time spent writing/flushing, not the upstream generation gap.
// The observer retains bounded SSE framing state, never logs content.
type diagnosticWriter struct {
	http.ResponseWriter
	diagnostics *attemptDiagnostics
	observer    sseUsageObserver
	status      int
	stream      bool
	encoded     bool
}

func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticWriter) WriteHeader(code int) {
	w.status = code
	w.encoded = w.Header().Get("Content-Encoding") != ""
	w.ResponseWriter.WriteHeader(code)
}
func (w *diagnosticWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := w.ResponseWriter.Write(p)
	w.diagnostics.mu.Lock()
	w.diagnostics.writeDuration += time.Since(start)
	w.diagnostics.mu.Unlock()
	if n > 0 && w.status < 400 && !w.encoded {
		if w.stream {
			w.observer.Write(p[:n])
		} else {
			w.diagnostics.firstOutput()
		}
	}
	return n, err
}
func (w *diagnosticWriter) Flush() {
	start := time.Now()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	w.diagnostics.mu.Lock()
	w.diagnostics.writeDuration += time.Since(start)
	w.diagnostics.mu.Unlock()
}
func semanticStreamEvent(data map[string]any) bool {
	if hasAnthropicBridgeSemanticOutput(data) {
		return true
	}
	for _, value := range anySlice(data["choices"]) {
		delta := asMap(asMap(value)["delta"])
		if asString(delta["reasoning_content"]) != "" || asString(delta["refusal"]) != "" {
			return true
		}
	}
	switch asString(data["type"]) {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return asString(data["delta"]) != ""
	case "content_block_delta":
		delta := asMap(data["delta"])
		return asString(delta["text"]) != "" || asString(delta["thinking"]) != "" || asString(delta["partial_json"]) != ""
	}
	return len(anySlice(data["candidates"])) > 0
}
