package fusiongate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelDisplayNameUsesExplicitNameOrPrefixFreeDefault(t *testing.T) {
	for _, tc := range []struct{ model, display, want string }{
		{"deepseek/deepseek-4.1-flash", "", "deepseek-4.1-flash"},
		{"cline-pass/deepseek-v4.1-flash", "自定义显示名", "自定义显示名"},
		{"vendor/model", "vendor/custom label", "vendor/custom label"},
		{"model", "", "model"},
		{"outer/vendor/model", "", "model"},
		{"vendor/model", "  ", "model"},
		{"vendor/", "", "vendor/"},
	} {
		if got := modelDisplayName(tc.model, tc.display); got != tc.want {
			t.Errorf("modelDisplayName(%q,%q)=%q want %q", tc.model, tc.display, got, tc.want)
		}
	}
}

func TestHealthCheckDisplayNameFlowsFromInventoryWithoutChangingWireModel(t *testing.T) {
	const model = "cline-pass/deepseek-v4.1-flash"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != model {
			t.Errorf("probe sent display name instead of upstream model: %v", body["model"])
		}
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "OK"}}}})
	}))
	defer upstream.Close()
	a, p, keys := manualModelsFixture(t)
	if _, err := a.db.Exec(`UPDATE providers SET base_url=? WHERE id=?`, upstream.URL, p); err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[:1], Entries: []manualModelEntry{{Model: model, DisplayName: "Flash 显示名称", Capabilities: "chat,stream"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveManualModels(context.Background(), p, manualModelsInput{KeyIDs: keys[1:], Entries: []manualModelEntry{{Model: model, Capabilities: "chat,stream"}}}); err != nil {
		t.Fatal(err)
	}
	routeID := insertTestRoute(t, a, p, "old-public-request", model, "chat,stream", 1)
	preview, err := a.healthCheckJobs.Preview(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Routes) != 1 || preview.Routes[0].DisplayName != "Flash 显示名称" || preview.Routes[0].PublicName != "old-public-request" || preview.Routes[0].UpstreamModel != model {
		t.Fatalf("preview=%+v", preview)
	}
	for _, key := range preview.Routes[0].Keys {
		want := "deepseek-v4.1-flash"
		if key.KeyID == keys[0] {
			want = "Flash 显示名称"
		}
		if key.DisplayName != want {
			t.Fatalf("key preview=%+v want=%q", key, want)
		}
	}
	providers, err := a.healthCheckJobs.loadTargets(context.Background(), []int64{p})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := a.healthCheckJobs.loadModelTargets(context.Background(), providers, []int64{routeID}, keys[1:], "selected")
	if err != nil || len(targets) != 1 || targets[0].DisplayName != "deepseek-v4.1-flash" || targets[0].UpstreamModel != model {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	job, err := a.healthCheckJobs.StartModels(context.Background(), []int64{p}, []int64{routeID}, keys[:1], "selected")
	if err != nil {
		t.Fatal(err)
	}
	if job.Results[0].DisplayName != "Flash 显示名称" {
		t.Fatalf("queued result=%+v", job.Results[0])
	}
	job = waitForHealthCheckJob(t, a, job.ID)
	if len(job.Results) != 1 || job.Results[0].DisplayName != "Flash 显示名称" || job.Results[0].PublicName != "old-public-request" || job.Results[0].Model != model {
		t.Fatalf("finished result=%+v", job.Results)
	}
}
