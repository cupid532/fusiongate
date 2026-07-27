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
