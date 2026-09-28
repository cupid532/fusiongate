package fusiongate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPassthroughEveryPublicInferenceEndpoint(t *testing.T) {
	paths := []string{"/v1/chat/completions", "/v1/responses", "/v1/responses/compact", "/v1/messages", "/v1/messages/count_tokens", "/v1/images/generations", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/embeddings"}
	for _, kind := range []string{"openai", "grok", "openrouter", "openai_compatible", "opencode", "anthropic", "anthropic_compatible"} {
		t.Run(kind, func(t *testing.T) {
			body := `{ "model":"native", "stream":false, "vendor":{"unknown":true}, "tools":[{"type":"future"}] }`
			var receivedPath string
			a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
				receivedPath = r.URL.Path
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != body {
					t.Errorf("request bytes changed: %q %v", raw, err)
				}
				if r.Header.Get("Anthropic-Version") != "client-version" {
					t.Error("protocol header changed")
				}
				if isAnthropicProvider(kind) {
					if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Authorization") != "" {
						t.Error("incorrect Anthropic credential replacement")
					}
				} else if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("incorrect bearer replacement")
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write([]byte("native\x00response"))
			})
			defer done()
			if _, err := a.db.Exec(`UPDATE providers SET type=?,protocol_policy='auto',protocol_preference='responses',passthrough_mode='normalized'`, kind); err != nil {
				t.Fatal(err)
			}
			if _, err := a.db.Exec(`UPDATE api_keys SET allow_images=1,allow_audio=1`); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				t.Run(path, func(t *testing.T) {
					req := httptest.NewRequest("POST", path, strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+key)
					req.Header.Set("Anthropic-Version", "client-version")
					rec := httptest.NewRecorder()
					a.Router().ServeHTTP(rec, req)
					if rec.Code != 200 || rec.Body.String() != "native\x00response" || receivedPath != path {
						t.Fatalf("status=%d path=%q body=%q", rec.Code, receivedPath, rec.Body.String())
					}
				})
			}
		})
	}
}

// V3.13 restored the credential-bearing channel types: they speak the same wire
// format as the public endpoints their own API exposes, so the request body is
// still forwarded untouched and only the credential and account headers differ.
// These channels are never handed an endpoint their upstream does not have: a
// client protocol the channel lacks is bridged instead (see bridge_test.go).
func TestInferenceIdentityChannelsServeTheirNativeEndpoints(t *testing.T) {
	cases := []struct {
		kind  string
		path  string
		code  int
		calls int32
	}{
		{"codex_oauth", "/v1/responses", 200, 1},
		{"codex_oauth", "/v1/responses/compact", 200, 1},
		{"grok_oauth", "/v1/responses", 200, 1},
		{"grok_oauth", "/v1/chat/completions", 200, 1},
		{"claude_oauth", "/v1/messages", 200, 1},
		{"claude_oauth", "/v1/messages/count_tokens", 200, 1},
	}
	for _, tc := range cases {
		t.Run(tc.kind+" "+tc.path, func(t *testing.T) {
			body := `{ "model":"native", "stream":false, "vendor":{"unknown":true} }`
			var calls atomic.Int32
			var received string
			a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				received = string(raw)
				w.Write([]byte("native\x00response"))
			})
			defer done()
			if _, err := a.db.Exec(`UPDATE providers SET type=?`, tc.kind); err != nil {
				t.Fatal(err)
			}
			rec := gatewayRequest(t, a, tc.path, key, body, "")
			if rec.Code != tc.code || calls.Load() != tc.calls {
				t.Fatalf("status=%d calls=%d want status=%d calls=%d", rec.Code, calls.Load(), tc.code, tc.calls)
			}
			if tc.calls == 0 {
				return
			}
			// Body passthrough: an identity channel changes the credential, never
			// the payload, so unknown fields and formatting survive verbatim.
			if received != body || rec.Body.String() != "native\x00response" {
				t.Fatalf("identity channel rewrote the exchange: sent=%q got=%q", received, rec.Body.String())
			}
		})
	}
}

// Types with no verified implementation stay stored and enabled exactly as the
// operator configured them; they are simply not selected for a public inference
// request, and the ledger records why.
func TestPassthroughUnsupportedAdaptersStayStoredButExcluded(t *testing.T) {
	for _, kind := range []string{"gemini", "gemini_oauth", "antigravity", "qwen_oauth", "iflow_oauth"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
			defer done()
			if _, err := a.db.Exec(`UPDATE providers SET type=?`, kind); err != nil {
				t.Fatal(err)
			}
			rec := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
			if rec.Code != 404 || calls.Load() != 0 {
				t.Fatalf("unsupported routed: status=%d calls=%d", rec.Code, calls.Load())
			}
			models := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			a.Router().ServeHTTP(models, req)
			var list struct {
				Data []any `json:"data"`
			}
			if err := json.Unmarshal(models.Body.Bytes(), &list); err != nil || models.Code != 200 || len(list.Data) != 0 {
				t.Fatalf("models=%s err=%v", models.Body.String(), err)
			}
			var stored, enabled int
			if err := a.db.QueryRow(`SELECT COUNT(*),SUM(enabled) FROM providers`).Scan(&stored, &enabled); err != nil || stored != 1 || enabled != 1 {
				t.Fatalf("account changed stored=%d enabled=%d err=%v", stored, enabled, err)
			}
			a.flushLedgerWrites()
			var reason string
			if err := a.db.QueryRow(`SELECT candidate_exclusions FROM request_ledger LIMIT 1`).Scan(&reason); err != nil || !strings.Contains(reason, "specialized_adapter_unsupported") {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
		})
	}
}
