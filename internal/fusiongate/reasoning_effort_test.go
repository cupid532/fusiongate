package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestReasoningEffortParsesBothShapesAndClamps(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"flat chat field", map[string]any{"reasoning_effort": "high"}, "high"},
		{"nested responses field", map[string]any{"reasoning": map[string]any{"effort": "low"}}, "low"},
		{"mixed case is normalised", map[string]any{"reasoning_effort": "  Medium "}, "medium"},
		{"flat wins over nested", map[string]any{"reasoning_effort": "xhigh", "reasoning": map[string]any{"effort": "low"}}, "xhigh"},
		{"unknown value is dropped", map[string]any{"reasoning_effort": "turbo"}, ""},
		{"injection attempt is dropped", map[string]any{"reasoning_effort": "<script>alert(1)</script>"}, ""},
		{"absent", map[string]any{"model": "gpt-5"}, ""},
		{"non-string is ignored", map[string]any{"reasoning_effort": 3}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestReasoningEffort(tc.body); got != tc.want {
				t.Fatalf("requestReasoningEffort = %q, want %q", got, tc.want)
			}
		})
	}
}

// The effort has to reach the ledger row and come back through the requests API so the
// console can show it next to the model.
func TestReasoningEffortIsRecordedOnTheLedger(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	key := authKey{ID: 1}
	route := resolvedRoute{Route: Route{ID: 1, PublicName: "gpt-5", UpstreamModel: "gpt-5"}, Provider: Provider{ID: 1}}
	attemptID := a.startLedger(key, route, "openai_chat", true, "127.0.0.1", "req_effort", "high", 1, "")
	a.endLedger(attemptID, route.Provider.ID, key.ID, "openai", route.Route.UpstreamModel, true, 200, "", time.Now(), Usage{Reported: true})
	a.flushLedgerWrites()

	var effort string
	if err := a.db.QueryRow(`SELECT reasoning_effort FROM request_ledger WHERE request_id=?`, attemptID).Scan(&effort); err != nil {
		t.Fatal(err)
	}
	if effort != "high" {
		t.Fatalf("stored reasoning_effort = %q, want high", effort)
	}

	recorder := httptest.NewRecorder()
	a.requests(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/requests", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("requests API status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["reasoning_effort"] != "high" {
		t.Fatalf("requests API reasoning effort=%v", rows)
	}
}
