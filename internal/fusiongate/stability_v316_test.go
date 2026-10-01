package fusiongate

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInferencePoolReuseAndCompression(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprint("http2=", h2), func(t *testing.T) {
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if h2 && r.ProtoMajor != 2 {
					t.Errorf("protocol=%s", r.Proto)
				}
				if r.Header.Get("Accept-Encoding") == "gzip" {
					w.Header().Set("Content-Encoding", "gzip")
					g := gzip.NewWriter(w)
					_, _ = g.Write([]byte("ok"))
					_ = g.Close()
				} else {
					_, _ = io.WriteString(w, "ok")
				}
			}))
			server.EnableHTTP2 = h2
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
			defer server.Close()
			base := server.Client().Transport.(*http.Transport)
			base.ForceAttemptHTTP2 = true
			a := &App{client: server.Client()}
			defer a.closeInferenceTransports()
			z := resolvedRoute{Provider: Provider{BaseURL: server.URL, Type: "openai", RequestTimeoutMS: 1000}, Credential: "test"}
			for _, bridged := range []bool{false, true} {
				before := connections.Load()
				for i := 0; i < 101; i++ {
					r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
					result, cancel := a.inferenceSend(r, []byte(`{}`), z, "/v1/chat/completions", false, nil, bridged)
					if result.Err != nil {
						t.Fatal(result.Err)
					}
					body, err := io.ReadAll(result.Response.Body)
					_ = result.Response.Body.Close()
					cancel()
					if err != nil || string(body) != "ok" {
						t.Fatalf("body=%q err=%v", body, err)
					}
				}
				if got := connections.Load() - before; got > 5 {
					t.Fatalf("101 requests made %d connections", got)
				}
			}
			first := a.nativeTransport(base)
			other := base.Clone()
			defer other.CloseIdleConnections()
			if first == a.nativeTransport(other) {
				t.Fatal("egress pools shared")
			}
			a.retireTransport(base)
			if first == a.nativeTransport(base) {
				t.Fatal("pool not invalidated")
			}
		})
	}
}

func TestStreamTimeoutPrecedence(t *testing.T) {
	a := &App{}
	p := Provider{RequestTimeoutMS: 120000}
	start, idle := a.streamTimeouts(p)
	if start != 120*time.Second || idle != 300*time.Second {
		t.Fatal(start, idle)
	}
	a.cfg.StreamStartTimeout = 40 * time.Second
	a.cfg.StreamIdleTimeout = 50 * time.Second
	start, idle = a.streamTimeouts(p)
	if start != 40*time.Second || idle != 50*time.Second {
		t.Fatal(start, idle)
	}
	p.StreamStartTimeoutMS = 70000
	p.StreamIdleTimeoutMS = 80000
	start, idle = a.streamTimeouts(p)
	if start != 70*time.Second || idle != 80*time.Second {
		t.Fatal(start, idle)
	}
}

func TestStrictProtocolCapabilityLearning(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":"unknown parameter temperature"}`, false}, {422, `unsupported field`, false},
		{404, `model not found`, false}, {404, `not found`, false}, {500, `unsupported protocol`, false},
		{401, `unknown endpoint`, false}, {429, `unknown endpoint`, false}, {404, `Invalid URL (POST /v1/responses)`, true},
		{400, `{"error":{"code":"gateway_provider_protocol_unavailable","message":"No provider supports the protocol for this model"}}`, true},
		{400, `endpoint not supported`, true}, {405, ``, true}, {501, ``, true},
	} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		if got := protocolUnsupportedSignal(resp); got != tc.want {
			t.Errorf("%d %s got=%v", tc.status, tc.body, got)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != tc.body {
			t.Fatal("body consumed")
		}
	}
	m := &wireMemory{entries: map[string]time.Time{}}
	z := resolvedRoute{Provider: Provider{ID: 1, BaseURL: "https://example.com"}, Credential: "one",
		AuthCredential: &ProviderCredential{Kind: "oauth", Platform: "codex", AccountID: "acct-1"}}
	m.remember(z, wireChat)
	if !m.unsupported(z, wireChat) {
		t.Fatal("not learned")
	}
	// A renewed token of the same account keeps the verdict. The learning is
	// about the channel and the account, and discarding it on every refresh made
	// the channel re-probe an endpoint it had already disproved -- on the first
	// request after each refresh, for the subscription channels that refresh on
	// a schedule.
	z.Credential = "two"
	z.AuthCredential.AccessToken = "two"
	if !m.unsupported(z, wireChat) {
		t.Fatal("a renewed token lost the learning")
	}
	// A different account must not inherit it: a second subscription can carry
	// different entitlements.
	z.AuthCredential = &ProviderCredential{Kind: "oauth", Platform: "codex", AccountID: "acct-2"}
	if m.unsupported(z, wireChat) {
		t.Fatal("stale account cache")
	}
	z.AuthCredential = &ProviderCredential{Kind: "oauth", Platform: "codex", AccountID: "acct-1"}
	z.Provider.BaseURL += "/new"
	if m.unsupported(z, wireChat) {
		t.Fatal("stale address cache")
	}
}

func TestBridgeRejectsLossyFeaturesBeforeDispatch(t *testing.T) {
	if validateBridgeCapabilities(wireResponses, []byte(fidelitySensitiveCodexRequest)) == nil {
		t.Fatal("lossy conversion allowed")
	}
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "codex_oauth", func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/chat/completions", `{"model":"public-model","stream":true,"messages":[],"tools":[{"type":"web_search"}]}`)
	if calls.Load() != 0 || rec.Code != 400 || !strings.Contains(rec.Body.String(), "capability_not_supported") {
		t.Fatalf("calls=%d status=%d body=%s", calls.Load(), rec.Code, rec.Body.String())
	}
}

func TestBridgeRejectsTruncatedAndErrorStreams(t *testing.T) {
	for _, raw := range []string{`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n", "data: [DONE]\n", `data: {"error":{"message":"fail"}}` + "\n\n"} {
		rec := httptest.NewRecorder()
		result := writeChatStream(rec, strings.NewReader(raw), "test")
		if result.Err == nil || strings.Contains(rec.Body.String(), "[DONE]") {
			t.Fatalf("result=%+v body=%s", result, rec.Body.String())
		}
	}
	for _, target := range []string{wireResponses, wireMessages} {
		resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(": heartbeat\n\n"))}
		body := bridgeUpstreamToChat(target, resp, "m")
		_, err := io.ReadAll(body)
		body.Close()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: %v", target, err)
		}
	}
}

func TestBridgeTerminalReleasesOpenUpstream(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			released := make(chan struct{}, 1)
			a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				released <- struct{}{}
			})
			defer done()
			_, err := a.db.Exec(`UPDATE providers SET protocol_policy='fixed',protocol_preference='chat'`)
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				payload := `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
				if path == "/v1/responses" {
					payload = `{"model":"public-model","stream":true,"input":"hello"}`
				}
				bridgeRequest(t, a, key, path, payload)
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("terminal event did not finish")
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("upstream not released")
			}
		})
	}
}

func TestNonStreamDeadlineDoesNotSlide(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(canceled)
		w.WriteHeader(200)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(15 * time.Millisecond):
				w.Write([]byte("x"))
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	a := &App{client: server.Client()}
	defer a.closeInferenceTransports()
	z := resolvedRoute{Provider: Provider{BaseURL: server.URL, Type: "openai", RequestTimeoutMS: 80}}
	result, cancel := a.inferenceSend(httptest.NewRequest("POST", "/", nil), nil, z, "/", false, nil, false)
	defer cancel()
	if result.Response == nil {
		t.Fatal(result.Err)
	}
	_, err := io.Copy(io.Discard, result.Response.Body)
	result.Response.Body.Close()
	if err == nil {
		t.Fatal("deadline slid")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("leaked request")
	}
}

func TestStreamStartBeyondThirtySeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("real 31-second timeout regression")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(31 * time.Second):
			io.WriteString(w, "ok")
		}
	}))
	defer server.Close()
	a := &App{client: server.Client()}
	defer a.closeInferenceTransports()
	z := resolvedRoute{Provider: Provider{BaseURL: server.URL, Type: "openai", RequestTimeoutMS: 35000}}
	r := httptest.NewRequest("POST", "/", nil).WithContext(context.Background())
	result, cancel := a.inferenceSend(r, nil, z, "/", true, nil, false)
	defer cancel()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var b bytes.Buffer
	_, err := io.Copy(&b, result.Response.Body)
	result.Response.Body.Close()
	if err != nil || b.String() != "ok" {
		t.Fatal(err, b.String())
	}
}

func TestProtocolProbeAndBridgeHaveSeparateAttemptRecords(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"unknown endpoint"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/responses", `{"model":"public-model","input":"hi","stream":true}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	a.flushLedgerWrites()
	var records, ledgers int
	if err := a.db.QueryRow(`SELECT count(*) FROM inference_attempts`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT count(*) FROM request_ledger`).Scan(&ledgers); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || records != 2 || ledgers != 2 {
		t.Fatalf("calls=%d records=%d ledgers=%d", calls.Load(), records, ledgers)
	}
	var protocol string
	if err := a.db.QueryRow(`SELECT upstream_protocol FROM inference_attempts WHERE execution_mode='bridge'`).Scan(&protocol); err != nil || protocol != "openai_chat" {
		t.Fatal(protocol, err)
	}
}

func TestBridgeUnsupportedTargetAdvancesWithinCallBudget(t *testing.T) {
	var calls atomic.Int32
	a, key, done := bridgeFixture(t, "openai_compatible", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"gateway_provider_protocol_unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer done()
	rec := bridgeRequest(t, a, key, "/v1/messages", `{"model":"public-model","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "message_stop") || calls.Load() != 3 {
		t.Fatalf("calls=%d status=%d body=%s", calls.Load(), rec.Code, rec.Body.String())
	}
	a.flushLedgerWrites()
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM inference_attempts`).Scan(&n); err != nil || n != 3 {
		t.Fatal(n, err)
	}
}
