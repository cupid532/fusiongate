package fusiongate

import (
	"context"
	"strings"
	"testing"
)

func TestCodexAuthImageDiscoveryNoModeRequired(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := insertCodexOAuthTestProvider(t, a, "https://example.invalid")
	models := []discoveredModel{{ID: "chosen-text", UpstreamID: "chosen-text", Capabilities: "chat,stream"}}
	got, err := a.codexAuthImageModels(context.Background(), id, models)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "gpt-image-tool-chosen-text" || got[1].UpstreamID != "chosen-text" || !got[1].RequiresSelection || !strings.Contains(got[1].DisplayName, "未验证") {
		t.Fatalf("unexpected candidates: %+v", got)
	}
	if _, err = a.importDiscoveredModels(context.Background(), id, got, false); err != nil {
		t.Fatal(err)
	}
	var n int
	a.db.QueryRow(`SELECT count(*) FROM model_routes WHERE provider_id=? AND public_name LIKE 'gpt-image-%'`, id).Scan(&n)
	if n != 0 {
		t.Fatal("automatic sync enabled unverified image candidate")
	}
	insertTestRoute(t, a, id, "gpt-image-2", "chosen-text", "image", 1)
	got, err = a.codexAuthImageModels(context.Background(), id, models)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got {
		if m.ID == "gpt-image-2" {
			found = true
			if m.UpstreamID != "chosen-text" {
				t.Fatal("existing mapping replaced")
			}
		}
	}
	if !found {
		t.Fatal("existing alias lost")
	}
}

func TestCodexAuthImageToolMetadata(t *testing.T) {
	models, _, err := parseDiscoveryModels([]byte(`{"models":[{"slug":"a","supported_tools":["image_generation"]},{"slug":"b","supported_tools":[{"type":"image_generation"}]},{"slug":"c"}]}`), "codex_oauth")
	if err != nil || len(models) != 3 || !models[0].ImageToolSupported || !models[1].ImageToolSupported || models[2].ImageToolSupported {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}
