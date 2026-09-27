package fusiongate

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPassthroughAdminDiagnosticsDoNotAdvanceKeyRotation(t *testing.T) {
	a, key, done := passthroughFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(r.Header.Get("Authorization"))) })
	defer done()
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1,key_selection_mode='round_robin' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	insertProviderKeyForTest(t, a, 1, "first", "first", "", providerKeyEgressInherit, nil, 1, 0)
	insertProviderKeyForTest(t, a, 1, "second", "second", "", providerKeyEgressInherit, nil, 1, 1)
	for _, want := range []string{"Bearer first", "Bearer second"} {
		rec := httptest.NewRecorder()
		a.routes(rec, httptest.NewRequest("GET", "/api/admin/routes", nil), adminCtx{})
		if rec.Code != 200 {
			t.Fatalf("diagnostics=%d %s", rec.Code, rec.Body.String())
		}
		actual := gatewayRequest(t, a, "/v1/responses", key, `{"model":"native"}`, "")
		if actual.Code != 200 || actual.Body.String() != want {
			t.Fatalf("got=%d %q want=%q", actual.Code, actual.Body.String(), want)
		}
	}
}
