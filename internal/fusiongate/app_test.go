package fusiongate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{DataDir: t.TempDir(), MasterKey: base64.StdEncoding.EncodeToString(randomBytes(32)), AdminPassword: "correct horse battery staple", AllowInsecureUpstreams: true, AllowPrivateUpstreams: true}
}
func TestCredentialEncryption(t *testing.T) {
	a, e := New(testConfig(t))
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ciphertext, e := a.encrypt("sk-super-secret")
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(ciphertext), "sk-super-secret") {
		t.Fatal("ciphertext contains plaintext credential")
	}
	plain, e := a.decrypt(ciphertext)
	if e != nil || plain != "sk-super-secret" {
		t.Fatalf("round trip = %q, %v", plain, e)
	}
}
func TestPasswordHash(t *testing.T) {
	h := passwordHash("safe-password", []byte("0123456789abcdef"))
	if !checkPassword("safe-password", h) {
		t.Fatal("correct password rejected")
	}
	if checkPassword("wrong-password", h) {
		t.Fatal("wrong password accepted")
	}
}
func TestModelPermissions(t *testing.T) {
	k := authKey{AllowAll: false, AllowModels: "smart,coding-*", DenyModels: "coding-experimental"}
	for _, tc := range []struct {
		model string
		want  bool
	}{{"smart", true}, {"coding-fast", true}, {"coding-experimental", false}, {"other", false}} {
		if got := allowed(k, tc.model); got != tc.want {
			t.Errorf("allowed(%s)=%v, want %v", tc.model, got, tc.want)
		}
	}
}
func TestUpstreamValidation(t *testing.T) {
	cfg := Config{}
	for _, raw := range []string{"http://example.com", "https://127.0.0.1", "https://localhost", "not-a-url"} {
		if validateUpstream(raw, cfg) == nil {
			t.Errorf("unsafe URL accepted: %s", raw)
		}
	}
	if e := validateUpstream("https://api.example.com", cfg); e != nil {
		t.Fatal(e)
	}
}

func TestOpenAICompatibleGatewayFlow(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "provider-model" {
			t.Errorf("forwarded model = %#v", body["model"])
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "upstream-id", "object": "chat.completion", "model": "provider-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3}})
	}))
	defer upstream.Close()
	encrypted, _ := a.encrypt("upstream-secret")
	nowv := now()
	result, err := a.db.Exec(`INSERT INTO providers(name,type,base_url,credential,enabled,priority,weight,status,notes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "test-provider", "openai_compatible", upstream.URL, encrypted, 1, 1, 100, "healthy", "", nowv, nowv)
	if err != nil {
		t.Fatal(err)
	}
	providerID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,input_price_micros,output_price_micros,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, "smart", providerID, "provider-model", "chat,stream", 1, 1, 1_000_000, 2_000_000, nowv, nowv); err != nil {
		t.Fatal(err)
	}
	key := "fg_integration_key"
	sum := sha256.Sum256([]byte(key))
	if _, err := a.db.Exec(`INSERT INTO api_keys(name,key_prefix,key_hash,allow_all,allow_models,deny_models,allow_images,rpm_limit,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "test", key[:11], hex.EncodeToString(sum[:]), 1, "", "", 0, 100, nowv); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"smart","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	choices := out["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "pong" {
		t.Fatalf("response = %#v", out)
	}
	var success, input, output int
	var cost int64
	if err := a.db.QueryRow(`SELECT success,input_tokens,output_tokens,cost_micros FROM request_ledger`).Scan(&success, &input, &output, &cost); err != nil {
		t.Fatal(err)
	}
	if success != 1 || input != 7 || output != 3 || cost != 13 {
		t.Fatalf("ledger success=%d input=%d output=%d cost=%d", success, input, output, cost)
	}
}
