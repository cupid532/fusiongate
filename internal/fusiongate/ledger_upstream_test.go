package fusiongate

import "testing"

func TestLedgerUpstreamName(t *testing.T) {
	for _, tc := range []struct{ address, name, want string }{
		{"https://api.openai.com/v1", "custom", "OpenAI"},
		{"https://api.anthropic.com", "custom", "Anthropic"},
		{"https://example.openai.azure.com", "custom", "Azure OpenAI"},
		{"https://generativelanguage.googleapis.com", "custom", "Google Gemini"},
		{"https://api.x.ai", "custom", "xAI"},
		{"https://openrouter.ai/api", "custom", "OpenRouter"},
		{"https://api.deepseek.com", "custom", "DeepSeek"},
		{"https://api.openai.com.evil.example", "中转渠道", "中转渠道"},
		{"https://reseller.example", "某某中转商", "某某中转商"},
		{"https://reseller.example", "https://reseller.example", "未知"},
		{"https://reseller.example", "", "未知"},
		{"", "renamed provider", "未知"},
	} {
		if got := ledgerUpstreamName(tc.address, tc.name); got != tc.want {
			t.Errorf("source = %q, want %q", got, tc.want)
		}
	}
}

func TestLedgerUpstreamAddress(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://user:password@example.com:8443/v1?api_key=secret#token", "https://example.com:8443/v1"},
		{"http://localhost:9000/v1", "http://localhost:9000/v1"},
		{"", ""}, {"/relative", ""}, {"https://%invalid", ""},
	} {
		if got := ledgerUpstreamAddress(tc.raw); got != tc.want {
			t.Errorf("sanitized address = %q, want %q", got, tc.want)
		}
	}
}

func TestLedgerSnapshotsUpstreamPerAttempt(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "model", UpstreamModel: "original"}, Provider: Provider{ID: 1, BaseURL: "https://first.example/v1?key=secret"}}
	first := a.startLedger(authKey{ID: 1}, route, "openai_chat", false, "127.0.0.1", "snapshot", "", 1, "")
	route.Provider.BaseURL = "https://second.example/v1"
	route.Route.UpstreamModel = "fallback"
	second := a.startLedger(authKey{ID: 1}, route, "openai_chat", false, "127.0.0.1", "snapshot", "", 2, "503")
	a.flushLedgerWrites()
	for _, tc := range []struct{ id, address, model string }{{first, "https://first.example/v1", "original"}, {second, "https://second.example/v1", "fallback"}} {
		var address, model string
		if err := a.db.QueryRow(`SELECT upstream_address,upstream_model FROM request_ledger WHERE request_id=?`, tc.id).Scan(&address, &model); err != nil {
			t.Fatal(err)
		}
		if address != tc.address || model != tc.model {
			t.Fatalf("snapshot = %q %q", address, model)
		}
	}
}
