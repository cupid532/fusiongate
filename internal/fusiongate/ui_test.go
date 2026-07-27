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
