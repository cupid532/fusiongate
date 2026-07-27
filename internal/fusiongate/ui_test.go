package fusiongate

import (
	"strings"
	"testing"
)

func TestModelSelectionToolbarReceivesVisibleModels(t *testing.T) {
	html := string(adminHTML)
	if !strings.Contains(html, "updateModelSelectionToolbar(visible.map(([name])=>name))") {
		t.Fatal("renderRoutes must pass the visible public model names to the selection toolbar")
	}
	if !strings.Contains(html, "function updateModelSelectionToolbar(visibleNames=[]){visibleNames=Array.isArray(visibleNames)?visibleNames:[];") {
		t.Fatal("selection toolbar must tolerate a missing or invalid visible model list")
	}
}

func TestModelPickerUsesExistingModelsAsEditableSelection(t *testing.T) {
	html := string(adminHTML)
	checks := []string{
		"const active=models.filter(x=>x.existing).map(x=>x.id)",
		"selected:new Set(active),initial:new Set(active)",
		"onclick=\"saveModelSelection()\"",
		"'/api/admin/providers/'+modelPicker.providerId+'/models'",
		"method:'PUT'",
		"保存模型设置",
		"取消勾选即停止使用",
	}
	for _, check := range checks {
		if !strings.Contains(html, check) {
			t.Fatalf("model picker is missing %q", check)
		}
	}
	if strings.Contains(html, "model.existing?'disabled'") || strings.Contains(html, "if(!model||model.existing)return") {
		t.Fatal("existing models must remain editable so unchecking can stop them")
	}
}

func TestRoutesPageUsesProviderDiscoveryAndUnifiedMapping(t *testing.T) {
	html := string(adminHTML)
	for _, obsolete := range []string{"model-add-card", "routeForm", "discoverModelsFromRoutePage", "modelDiscoveryProvider"} {
		if strings.Contains(html, obsolete) {
			t.Fatalf("routes page still contains obsolete model creation UI %q", obsolete)
		}
	}
	for _, required := range []string{
		"function focusUnpricedModels()",
		"onclick=\"focusUnpricedModels()\"",
		"data-public-model=",
		"function openRouteMapping(id)",
		"function saveRouteMapping()",
		"设置统一映射",
		"保存并合并故障转移组",
		"public_name:name",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("routes page is missing %q", required)
		}
	}
}

func TestModelPickerShowsExistingUnifiedAliases(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		"(model.public_names||[])",
		"映射为 ",
		"x.model.public_names||[]",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("model picker alias visibility is missing %q", required)
		}
	}
}
