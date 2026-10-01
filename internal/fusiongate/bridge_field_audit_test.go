package fusiongate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func auditRoute() resolvedRoute {
	return resolvedRoute{
		Route:    Route{ID: 1, UpstreamModel: "upstream-model"},
		Provider: Provider{ID: 5, Name: "chat-only relay", Type: "openai_compatible"},
	}
}

// A refusal is recorded against the channel that could not serve the field, so
// the console can answer "what is blocking this channel" without a capture proxy
// on a live client.
func TestAuditRecordsRefusedFieldPerChannel(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := auditRoute()
	rejection := validateBridgeCapabilities(wireChat, []byte(`{"model":"m","messages":[],"response_format":{"type":"json_object"}}`))
	if rejection == nil {
		t.Fatal("the fixture must be refused")
	}
	a.auditBridgeRefusal(z, wireChat, rejection)
	a.auditBridgeRefusal(z, wireChat, rejection)
	a.auditBridgeRefusal(z, wireResponses, validateBridgeCapabilities(wireResponses, []byte(`{"model":"m","input":"hi","n":3}`)))

	events := a.bridgeFieldEvents()
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Hits != 2 || events[0].Field != "response_format" || events[0].Disposition != bridgeDispositionRefused {
		t.Fatalf("most frequent event = %+v", events[0])
	}
	if events[0].ProviderID != 5 || events[0].ProviderName != "chat-only relay" || events[0].UpstreamModel != "upstream-model" {
		t.Fatalf("event is not attributed to the channel: %+v", events[0])
	}
	if events[0].Reason == "" || events[0].FirstSeenAt == "" || events[0].LastSeenAt == "" {
		t.Fatalf("event lacks its explanation or timestamps: %+v", events[0])
	}
	if events[1].Field != "n" || events[1].Protocol != wireResponses {
		t.Fatalf("second event = %+v", events[1])
	}
}

// What a bridge drops is recorded too, but only for fields the policy actually
// drops: an ordinary field is not an observation.
func TestAuditRecordsDroppedFieldsOnly(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.auditBridgeDrops(auditRoute(), wireChat, map[string]any{
		"model":                 "m",
		"messages":              []any{},
		"prompt_cache_options":  map[string]any{"ttl": "30m"},
		"service_tier":          "auto",
		"prompt_cache_key":      "abc", // kept, not dropped
		"some_future_parameter": true,  // never reaches here; it would be refused
	}, bridgeOptions{})
	events := a.bridgeFieldEvents()
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	for _, event := range events {
		if event.Disposition != bridgeDispositionDropped {
			t.Fatalf("event = %+v", event)
		}
	}
	if events[0].Field != "prompt_cache_options" && events[1].Field != "prompt_cache_options" {
		t.Fatalf("the dropped cache hint was not recorded: %+v", events)
	}
}

// The aggregate is bounded, so a client inventing a field name per request
// cannot grow it without limit.
func TestAuditAggregateIsBounded(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	z := auditRoute()
	for i := range bridgeFieldAuditLimit + 50 {
		a.recordBridgeField(z, wireChat, "field_"+strconv.Itoa(i), bridgeDispositionRefused, "fixture")
	}
	if events := a.bridgeFieldEvents(); len(events) > bridgeFieldAuditLimit {
		t.Fatalf("aggregate grew to %d entries", len(events))
	}
}

// The console reads one authenticated JSON document.
func TestCapabilityAuditEndpoint(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.auditBridgeDrops(auditRoute(), wireChat, map[string]any{"prompt_cache_retention": "24h"}, bridgeOptions{})

	rec := httptest.NewRecorder()
	a.capabilityAudit(rec, httptest.NewRequest(http.MethodGet, "/api/admin/capabilities", nil), adminCtx{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Events []bridgeFieldEvent `json:"events"`
		Limit  int                `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Events) != 1 || payload.Events[0].Field != "prompt_cache_retention" || payload.Limit != bridgeFieldAuditLimit {
		t.Fatalf("payload = %+v", payload)
	}

	rec = httptest.NewRecorder()
	a.capabilityAudit(rec, httptest.NewRequest(http.MethodPost, "/api/admin/capabilities", nil), adminCtx{})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
