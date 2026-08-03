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

func TestIPPoolUIKeepsDirectAsDefaultAndNeverRendersShareLink(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`data-page="ippool"`,
		`id="page-ippool"`,
		`本机直连（默认）`,
		`id="providerIPPoolNode"`,
		`function renderIPPool()`,
		`function openProviderEgress(id)`,
		`api('/api/admin/ip-pool')`,
		`ip_pool_node_id:Number(o.ipPoolNodeID)||0`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("IP pool UI is missing %q", required)
		}
	}
	if strings.Contains(html, "node.share_link") || strings.Contains(html, "x.share_link") {
		t.Fatal("IP pool UI must never expect or render stored share links")
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

func TestProviderStatusFiltersFollowSchedulingStates(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`id="providerStatusFilters"`,
		`aria-label="渠道状态筛选"`,
		`let providerEditId=0;let providerStatusFilter='all';`,
		`function providerCircuitCooling(x)`,
		`if(Number.isFinite(until))return until>Date.now()`,
		`function providerStatusBucket(x)`,
		`if(!x.enabled)return 'disabled'`,
		`return providerCircuitCooling(x)?'circuit':'enabled'`,
		`badge('恢复探测','orange')`,
		`function setProviderStatusFilter(value)`,
		`function resetProviderFilters()`,
		`全部渠道`,
		`参与调度`,
		`已停用`,
		`熔断冷却`,
		`providerStatusBucket(x)===providerStatusFilter`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("provider status filters are missing %q", required)
		}
	}
}

func TestLightThemeUsesWarmCreamPalette(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`/* warm cream light theme and route orchestration */`,
		`html[data-theme="light"]{--bg:#f2efe7;--sidebar:#eeeae1;--surface:#fbf9f4;--surface-2:#f3efe7;--surface-3:#e9e3d8`,
		`--text:#2b2925;--muted:#6f685f;--muted-2:#938a7d;--accent:#a84f32;--accent-strong:#c66745`,
		`html[data-theme="light"] .brand-mark{color:#fffaf4;background:linear-gradient(145deg,#d98262,#b75639)`,
		`notice(next==='light'?'已切换到奶白主题':'已切换到深色主题')`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("warm cream light theme is missing %q", required)
		}
	}
	if strings.Contains(html, `html[data-theme="light"]{--bg:#f4f7fb`) {
		t.Fatal("the old cool white light-theme palette is still present")
	}
}
