package fusiongate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestClineManualModelRenameKeepsPublicNameAndForwardsUpstreamName(t *testing.T) {
	const publicModel = "deepseek/deepseek-v4.1-flash"
	const upstreamModel = "cline-pass/deepseek-v4.1-flash"
	const providerSecret = "fixture-cline-key"
	const responseBody = `{"id":"cline-local","choices":[{"message":{"role":"assistant","content":"本地回答"}}],"unknown_response_field":{"kept":true}}`

	// Capture the real HTTP request without relying on a live Cline service.
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+providerSecret {
			t.Errorf("upstream authorization = %q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			http.Error(w, "cannot read request", http.StatusBadRequest)
			return
		}
		select {
		case received <- raw:
		default:
			t.Error("unexpected extra upstream request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "cline", "openai_compatible", upstream.URL, "unused-legacy-key", 1, 100, "normalized", "any", 0, 3, 30)
	providerKeyID := insertProviderKeyForTest(t, a, providerID, providerSecret, "cline-single-key", publicModel, providerKeyEgressDirect, nil, 1, 0)
	if _, err := a.db.Exec(`UPDATE providers SET multi_key_initialized=1 WHERE id=?`, providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE provider_api_keys SET model_policy='allowlist',model_allowlist=? WHERE id=?`, publicModel, providerKeyID); err != nil {
		t.Fatal(err)
	}
	// Seed the old inventory and route locally; no discovery or pricing fetch
	// is needed to exercise the same manual rename used by the console.
	if _, err := a.saveManualModels(context.Background(), providerID, manualModelsInput{
		KeyIDs:  []int64{providerKeyID},
		Entries: []manualModelEntry{{Model: publicModel, Capabilities: "chat,stream,tools"}},
	}); err != nil {
		t.Fatal(err)
	}
	routeID := insertTestRoute(t, a, providerID, publicModel, publicModel, "chat,stream,tools", 1)

	out, err := a.saveManualModels(context.Background(), providerID, manualModelsInput{
		KeyIDs:        []int64{providerKeyID},
		OriginalModel: publicModel,
		Entries:       []manualModelEntry{{Model: upstreamModel, Capabilities: "chat,stream,tools"}},
		SyncRoutes:    true,
	})
	if err != nil {
		t.Fatalf("rename Cline model: %v", err)
	}
	if out.Keys != 1 || out.Models != 1 || out.RoutesUpdated != 1 || out.RoutesCreated != 0 {
		t.Fatalf("rename result = %+v", out)
	}
	var public, upstreamName string
	if err := a.db.QueryRow(`SELECT public_name,upstream_model FROM model_routes WHERE id=?`, routeID).Scan(&public, &upstreamName); err != nil {
		t.Fatal(err)
	}
	if public != publicModel || upstreamName != upstreamModel {
		t.Fatalf("route public=%q upstream=%q; want public=%q upstream=%q", public, upstreamName, publicModel, upstreamModel)
	}
	var inventoryModel, fixedModel, allowlist string
	if err := a.db.QueryRow(`SELECT m.model,k.model,k.model_allowlist FROM provider_api_key_models m JOIN provider_api_keys k ON k.id=m.provider_key_id WHERE k.id=?`, providerKeyID).Scan(&inventoryModel, &fixedModel, &allowlist); err != nil {
		t.Fatal(err)
	}
	if inventoryModel != upstreamModel || fixedModel != upstreamModel || allowlist != upstreamModel {
		t.Fatalf("renamed single Key inventory=%q fixed=%q allowlist=%q", inventoryModel, fixedModel, allowlist)
	}

	clientKey := insertTestKey(t, a, false)
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer "+clientKey)
	models := httptest.NewRecorder()
	a.Router().ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", models.Code, models.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != publicModel {
		t.Fatalf("models must list the unchanged public name, not the renamed upstream: %s", models.Body.String())
	}

	const requestBody = `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"保留原文\nsecond line","unknown_message_field":{"keep":[1,true,"原样"]}}],"stream":false,"unknown_client_field":{"large_integer":9007199254740993,"nested":[{"opaque":"keep me"}]}}`
	rec := gatewayRequest(t, a, "/v1/chat/completions", clientKey, requestBody, "cline-regression-test")
	if rec.Code != http.StatusOK || rec.Body.String() != responseBody {
		t.Fatalf("chat status=%d body=%s; want unchanged upstream response", rec.Code, rec.Body.String())
	}
	var forwarded []byte
	select {
	case forwarded = <-received:
	default:
		t.Fatal("public model request never reached the local Cline upstream")
	}
	decode := func(raw []byte) map[string]any {
		t.Helper()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var body map[string]any
		if err := decoder.Decode(&body); err != nil {
			t.Fatalf("decode request %s: %v", raw, err)
		}
		return body
	}
	got := decode(forwarded)
	if got["model"] != upstreamModel {
		t.Fatalf("forwarded model=%v; want %q", got["model"], upstreamModel)
	}
	want := decode([]byte(requestBody))
	want["model"] = upstreamModel
	// UseNumber also catches precision loss in unknown numeric fields. Only
	// the top-level model may change; message content and extensions survive.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forwarding changed content or unknown fields:\ngot:  %s\nwant: %s (with model=%q)", forwarded, requestBody, upstreamModel)
	}
}
