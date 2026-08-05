package fusiongate

import (
	"strings"
	"testing"
)

func TestAdminUIRendersVersion(t *testing.T) {
	html := string(adminHTML)
	if !strings.Contains(html, "FusionGate "+Version) {
		t.Fatalf("admin UI does not contain version %q", Version)
	}
	if strings.Contains(html, "{{FUSIONGATE_VERSION}}") {
		t.Fatal("admin UI contains unresolved version placeholder")
	}
}

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

func TestLightThemeUsesHighContrastCoolPalette(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`/* Crisp visual system: strong hierarchy, compact controls, explicit status. */`,
		`html[data-theme="light"]{--bg:#f3f6fa;--sidebar:#0d1726;--surface:#fff;--surface-2:#f6f8fb;--surface-3:#e9eef5`,
		`--text:#172033;--muted:#526176;--muted-2:#758399;--accent:#087f70;--accent-strong:#0a927f`,
		`html[data-theme="light"] .sidebar{background:linear-gradient(180deg,#0d1726,#101c2e)`,
		`notice(next==='light'?'已切换到高对比日间主题':'已切换到深色主题')`,
		`<strong>运行正常</strong><small>LOCAL · SQLITE</small>`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("high contrast light theme is missing %q", required)
		}
	}
	if strings.Contains(html, `已切换到奶白主题`) {
		t.Fatal("the low-contrast cream theme label is still present")
	}
}

func TestRequestLedgerHasServerSideDetailedTimeFilters(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`id="requestFrom" type="datetime-local" step="1"`,
		`id="requestTo" type="datetime-local" step="1"`,
		`id="requestStatus"`,
		`id="requestProvider"`,
		`function requestParams()`,
		`new Date(from).toISOString()`,
		`/api/admin/requests?'+requestParams()`,
		`function formatRequestTime(value)`,
		`开始 / 完成时间`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("request ledger detailed filters are missing %q", required)
		}
	}
}

func TestCompletedHealthCheckPanelStaysHidden(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`terminal=['completed','cancelled'].includes(job?.status)`,
		`$(id)!==panel||terminal`,
		`if(!panel||!job||terminal||!results.some`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("completed health check panel hiding is missing %q", required)
		}
	}
}

func TestRoutesPageSupportsPersistentChannelReordering(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`draggable="true"`,
		"function routeDrop(event,targetId)",
		"function moveRoute(id,direction)",
		"/api/admin/routes/reorder",
		"route_ids:ids",
		"sort_order",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("routes page reordering is missing %q", required)
		}
	}
}
