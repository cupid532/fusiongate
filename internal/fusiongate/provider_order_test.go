package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReorderProvidersPersistsCompleteGlobalOrder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := insertTestProvider(t, a, "first-order", "openai_compatible", "http://first.test", "one", 1, 100, "normalized", "any", 0, 3, 30)
	second := insertTestProvider(t, a, "second-order", "openai_compatible", "http://second.test", "two", 1, 100, "normalized", "any", 0, 3, 30)
	third := insertTestProvider(t, a, "third-order", "openai_compatible", "http://third.test", "three", 1, 100, "normalized", "any", 0, 3, 30)

	body := `{"provider_ids":[` + intString(third) + `,` + intString(first) + `,` + intString(second) + `]}`
	recorder := httptest.NewRecorder()
	a.reorderProviders(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/reorder", strings.NewReader(body)), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	list := httptest.NewRecorder()
	a.providers(list, httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil), adminCtx{})
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var providers []Provider
	if err := json.Unmarshal(list.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	want := []int64{third, first, second}
	if len(providers) != len(want) {
		t.Fatalf("providers=%d want=%d", len(providers), len(want))
	}
	for index, provider := range providers {
		if provider.ID != want[index] || provider.SortOrder != index {
			t.Fatalf("providers=%#v, want IDs %v with sequential positions", providers, want)
		}
	}
}

func TestReorderProvidersRejectsPartialOrder(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first := insertTestProvider(t, a, "partial-first", "openai_compatible", "http://first.test", "one", 1, 100, "normalized", "any", 0, 3, 30)
	_ = insertTestProvider(t, a, "partial-second", "openai_compatible", "http://second.test", "two", 1, 100, "normalized", "any", 0, 3, 30)

	recorder := httptest.NewRecorder()
	body := `{"provider_ids":[` + intString(first) + `]}`
	a.reorderProviders(recorder, httptest.NewRequest(http.MethodPatch, "/api/admin/providers/reorder", strings.NewReader(body)), adminCtx{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
