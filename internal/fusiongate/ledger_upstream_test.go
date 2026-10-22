package fusiongate

import "testing"

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
