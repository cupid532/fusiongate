package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelListsFollowEnabledKeyPermissions(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	p := insertTestProvider(t, a, "AIAPI", "openai_compatible", "https://upstream.invalid", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	key := insertProviderKeyForTest(t, a, p, "test-key", "GPT", "", providerKeyEgressInherit, nil, 1, 0)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, p); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-a", "gpt-b", "gpt-c"} {
		insertTestRoute(t, a, p, model, model, "chat", 0)
		if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,enabled,discovered_at) VALUES(?,?,1,?)`, key, model, now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []string{"claude-old-a", "claude-old-b"} {
		insertTestRoute(t, a, p, model, model, "chat,image,reasoning:high", 0)
	}
	check := func(want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		models := httptest.NewRecorder()
		a.models(models, r, authKey{AllowAll: true})
		var listed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(models.Body.Bytes(), &listed); err != nil || models.Code != 200 || len(listed.Data) != want {
			t.Fatalf("models want=%d status=%d body=%s err=%v", want, models.Code, models.Body.String(), err)
		}
		providers := httptest.NewRecorder()
		a.providers(providers, r, adminCtx{})
		var channels []Provider
		if err := json.Unmarshal(providers.Body.Bytes(), &channels); err != nil || providers.Code != 200 || len(channels) != 1 || channels[0].ModelCount != want {
			t.Fatalf("providers want=%d body=%s err=%v", want, providers.Body.String(), err)
		}
		routes := httptest.NewRecorder()
		a.routes(routes, r, adminCtx{})
		var mappings []Route
		if err := json.Unmarshal(routes.Body.Bytes(), &mappings); err != nil || routes.Code != 200 || len(mappings) != want {
			t.Fatalf("routes want=%d body=%s err=%v", want, routes.Body.String(), err)
		}
		dashboard := httptest.NewRecorder()
		a.dashboard(dashboard, r, adminCtx{})
		var stats struct {
			Models int `json:"models"`
		}
		if err := json.Unmarshal(dashboard.Body.Bytes(), &stats); err != nil || dashboard.Code != 200 || stats.Models != want {
			t.Fatalf("dashboard want=%d body=%s err=%v", want, dashboard.Body.String(), err)
		}
	}
	check(3)
	// A disabled card cannot keep a model visible; cooldowns do not withdraw
	// configured access, and re-enabling permissions restores retained mappings.
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET enabled=0 WHERE id=?`, key); err != nil {
		t.Fatal(err)
	}
	check(0)
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET enabled=1,cooldown_until='2999-01-01T00:00:00Z' WHERE id=?`, key); err != nil {
		t.Fatal(err)
	}
	check(3)
	if _, err := a.db.Exec(`INSERT INTO provider_api_key_models(provider_key_id,model,enabled,discovered_at) VALUES(?,'claude-old-a',1,?)`, key, now()); err != nil {
		t.Fatal(err)
	}
	check(4)
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM model_routes WHERE provider_id=?`, p).Scan(&count); err != nil || count != 5 {
		t.Fatalf("route definitions changed count=%d err=%v", count, err)
	}
}

func TestModelVisibilityUsesRoutingPermissionRules(t *testing.T) {
	cases := []struct {
		name  string
		keys  []*modelVisibilityKey
		model string
		want  bool
	}{
		{"no enabled keys", nil, "a", false},
		{"fallback inventory", []*modelVisibilityKey{{policy: "fallback", inventory: map[string]bool{"a": true, "b": false}}}, "b", false},
		{"fallback unknown", []*modelVisibilityKey{{policy: "fallback", inventory: map[string]bool{"a": true}}}, "b", false},
		{"default without inventory", []*modelVisibilityKey{{policy: "fallback", defaultModel: "a"}}, "a", true},
		{"unrestricted legacy fallback", []*modelVisibilityKey{{policy: "fallback"}}, "a", true},
		{"empty allowlist", []*modelVisibilityKey{{policy: "allowlist"}}, "a", false},
		{"allowlist", []*modelVisibilityKey{{policy: "allowlist", allowlist: "A,b"}}, "a", true},
		{"fixed", []*modelVisibilityKey{{policy: "fallback", fixed: "a"}}, "b", false},
		{"exclusion", []*modelVisibilityKey{{policy: "allowlist", allowlist: "a", exclusions: map[string]bool{"a": true}}}, "a", false},
		{"another key supports", []*modelVisibilityKey{{policy: "allowlist"}, {policy: "allowlist", allowlist: "a"}}, "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (modelVisibility{1: tc.keys}).supports(1, tc.model); got != tc.want {
				t.Fatalf("supports=%v want=%v", got, tc.want)
			}
		})
	}
	if !(modelVisibility{}).supports(1, "oauth-model") {
		t.Fatal("legacy/OAuth visibility changed")
	}
}
