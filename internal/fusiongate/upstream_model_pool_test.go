package fusiongate

import (
	"context"
	"testing"
)

func TestResolvePoolsRoutesByFinalUpstreamModel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := insertTestProvider(t, a, "luna-route", "openai_compatible", "http://luna.test", "first", 0, 100, "normalized", "any", 0, 5, 30)
	second := insertTestProvider(t, a, "native-route", "openai_compatible", "http://native.test", "second", 0, 100, "normalized", "any", 0, 5, 30)
	other := insertTestProvider(t, a, "other-route", "openai_compatible", "http://other.test", "third", 0, 100, "normalized", "any", 0, 5, 30)
	insertPoolRoute(t, a, first, "gpt-5.6-luna", "deepseek-v4-flash")
	insertPoolRoute(t, a, second, "deepseek-v4-flash", "deepseek-v4-flash")
	insertPoolRoute(t, a, other, "gpt-5.6-luna", "another-upstream")
	routes, err := a.resolve(context.Background(), "gpt-5.6-luna", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("routes=%d, want 3: %#v", len(routes), routes)
	}
	seen := map[int64]string{}
	for _, route := range routes {
		if route.Route.PublicName != "gpt-5.6-luna" {
			t.Fatalf("public name=%q", route.Route.PublicName)
		}
		seen[route.Provider.ID] = route.Route.UpstreamModel
	}
	if seen[first] != "deepseek-v4-flash" || seen[second] != "deepseek-v4-flash" || seen[other] != "another-upstream" {
		t.Fatalf("pool members=%v", seen)
	}
}

func TestResolveDoesNotDoubleCountProviderInUpstreamPool(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "dual-name", "openai_compatible", "http://dual.test", "secret", 0, 100, "normalized", "any", 0, 5, 30)
	insertPoolRoute(t, a, providerID, "gpt-5.6-luna", "deepseek-v4-flash")
	insertPoolRoute(t, a, providerID, "deepseek-v4-flash", "deepseek-v4-flash")
	routes, err := a.resolve(context.Background(), "gpt-5.6-luna", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("provider received %d pool seats, want 1", len(routes))
	}
	if routes[0].Route.PublicName != "gpt-5.6-luna" || routes[0].Route.UpstreamModel != "deepseek-v4-flash" {
		t.Fatalf("route=%+v", routes[0].Route)
	}
}

func TestResolveDoesNotMixUnrelatedUpstreamPools(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	wanted := insertTestProvider(t, a, "wanted", "openai_compatible", "http://wanted.test", "first", 0, 100, "normalized", "any", 0, 5, 30)
	unrelated := insertTestProvider(t, a, "unrelated", "openai_compatible", "http://unrelated.test", "second", 0, 100, "normalized", "any", 0, 5, 30)
	insertPoolRoute(t, a, wanted, "gpt-5.6-luna", "deepseek-v4-flash")
	insertPoolRoute(t, a, unrelated, "another-public-name", "another-upstream")
	routes, err := a.resolve(context.Background(), "gpt-5.6-luna", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Provider.ID != wanted {
		t.Fatalf("unrelated route entered pool: %#v", routes)
	}
}

func insertPoolRoute(t *testing.T, a *App, providerID int64, publicName, upstreamModel string) {
	t.Helper()
	if _, err := a.db.Exec(`INSERT INTO model_routes(public_name,provider_id,upstream_model,capabilities,enabled,priority,sort_order,created_at,updated_at) VALUES(?,?,?,'chat,stream',1,0,0,?,?)`, publicName, providerID, upstreamModel, now(), now()); err != nil {
		t.Fatal(err)
	}
}
