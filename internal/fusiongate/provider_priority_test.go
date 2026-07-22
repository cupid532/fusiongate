package fusiongate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderCreationDefaultsPriorityToZero(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", strings.NewReader(`{
		"name":"default-priority",
		"type":"openai_compatible",
		"baseURL":"http://provider.test",
		"credential":"secret",
		"auto_discover":false
	}`))
	rec := httptest.NewRecorder()
	a.providers(rec, req, adminCtx{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var priority int
	if err := a.db.QueryRow(`SELECT priority FROM providers WHERE name='default-priority'`).Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != 0 {
		t.Fatalf("default provider priority=%d, want 0", priority)
	}
}

func TestProviderToggleAndPriorityControlFailover(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	first := insertTestProvider(t, a, "first", "openai_compatible", "http://first.test", "one", 0, 100, "normalized", "any", 0, 3, 30)
	second := insertTestProvider(t, a, "second", "openai_compatible", "http://second.test", "two", 5, 100, "normalized", "any", 0, 3, 30)
	third := insertTestProvider(t, a, "third", "openai_compatible", "http://third.test", "three", 5, 100, "normalized", "any", 0, 3, 30)
	insertTestRoute(t, a, first, "model", "first-model", "chat", 999)
	insertTestRoute(t, a, second, "model", "second-model", "chat", 0)
	insertTestRoute(t, a, third, "model", "third-model", "chat", 0)

	routes, err := a.resolve(context.Background(), "model", "chat")
	if err != nil {
		t.Fatal(err)
	}
	plan := a.prepareRoutes(routes, StrategyPriorityFailover)
	want := []int64{second, third, first}
	for i, providerID := range want {
		if plan[i].Provider.ID != providerID {
			t.Fatalf("provider order=%v, want=%v", []int64{plan[0].Provider.ID, plan[1].Provider.ID, plan[2].Provider.ID}, want)
		}
	}

	disable := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(second), strings.NewReader(`{"enabled":false}`))
	rec := httptest.NewRecorder()
	a.providerByID(rec, disable, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	routes, err = a.resolve(context.Background(), "model", "chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Provider.ID == second {
			t.Fatal("disabled provider remained eligible")
		}
	}

	promote := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(first), strings.NewReader(`{"priority":9}`))
	rec = httptest.NewRecorder()
	a.providerByID(rec, promote, adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("priority patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	routes, err = a.resolve(context.Background(), "model", "chat")
	if err != nil {
		t.Fatal(err)
	}
	plan = a.prepareRoutes(routes, StrategyPriorityFailover)
	if plan[0].Provider.ID != first {
		t.Fatalf("promoted provider=%d, want %d", plan[0].Provider.ID, first)
	}
}

func TestProviderPriorityRejectsNegativeValues(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "provider", "openai_compatible", "http://provider.test", "secret", 0, 100, "normalized", "any", 0, 3, 30)
	req := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"priority":-1}`))
	rec := httptest.NewRecorder()
	a.providerByID(rec, req, adminCtx{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative priority status=%d body=%s", rec.Code, rec.Body.String())
	}
}
