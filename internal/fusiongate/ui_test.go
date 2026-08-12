package fusiongate

import (
	"regexp"
	"strings"
	"testing"
)

// adminSource is the whole console as one string. The shell, stylesheet and script are
// separate embedded assets, so assertions that do not care which file a snippet lives
// in search all three.
func adminSource() string {
	return string(adminHTML) + string(adminCSS) + string(adminJS)
}

func TestAdminUIRendersVersion(t *testing.T) {
	html := adminSource()
	if !strings.Contains(html, "FusionGate "+Version) {
		t.Fatalf("admin UI does not contain version %q", Version)
	}
	if strings.Contains(html, "{{FUSIONGATE_VERSION}}") {
		t.Fatal("admin UI contains unresolved version placeholder")
	}
}

func TestMobileNavigationIsDismissibleAndAccessible(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		`id="sidebarBackdrop"`,
		`aria-controls="sidebar"`,
		`aria-expanded="false"`,
		`function setMobileMenu(open)`,
		`document.body.classList.toggle('mobile-nav-open',shouldOpen)`,
		`$('#sidebarBackdrop').onclick=()=>closeMobileMenu(true)`,
		`if($('#sidebar').classList.contains('open'))`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("mobile navigation is missing %q", required)
		}
	}
}

func TestMobileControlsKeepTouchSizedTargets(t *testing.T) {
	css := string(adminCSS)
	for _, required := range []string{
		`.provider-balance button{display:inline-flex;align-items:center;justify-content:center;min-width:44px;min-height:44px`,
		`.provider-order-controls{grid-template-columns:repeat(2,44px);gap:4px;opacity:1}`,
		`.provider-move{width:44px;height:44px}`,
		`.route-position{left:14px;top:12px;transform:none;grid-template-columns:repeat(2,44px);gap:8px}`,
		`.route-move{width:44px;height:44px}`,
	} {
		if !strings.Contains(css, required) {
			t.Fatalf("mobile controls are missing touch-sized rule %q", required)
		}
	}
}

func TestUsageQuickRangesUseRollingHours(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		`<option value="1">近 1 小时</option>`,
		`<option value="24">近 1 天</option>`,
		`<option value="168">近 7 天</option>`,
		`<option value="720" selected>近 30 天</option>`,
		`const hours=Math.max(1,Number(value)||720)`,
		`from=new Date(to.getTime()-hours*3600000)`,
		`days:String(range==='custom'?30:usageRangeDays(range))`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("usage quick ranges are missing %q", required)
		}
	}
	for _, obsolete := range []string{
		`<option value="90">近 90 天</option>`,
		`<option value="365">近一年</option>`,
		`from.setHours(0,0,0,0)`,
	} {
		if strings.Contains(html, obsolete) {
			t.Fatalf("usage quick ranges still contain %q", obsolete)
		}
	}
}

func TestAuthModelAutoSyncSkipsExpiredExternalCredentials(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		`function externalAuthSource(source)`,
		`function authCredentialExpired(x)`,
		`externalAuthSource(x.auth_source)||!x.has_refresh_token`,
		`authPlatform(x)&&!authCredentialExpired(x)&&Number(x.model_count||0)===0`,
		`external?['等待凭据更新','orange']`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("OAuth model auto-sync guard is missing %q", required)
		}
	}
}

func TestModelSelectionToolbarReceivesVisibleModels(t *testing.T) {
	html := adminSource()
	if !strings.Contains(html, "updateModelSelectionToolbar(visible.map(([name])=>name))") {
		t.Fatal("renderRoutes must pass the visible public model names to the selection toolbar")
	}
	if !strings.Contains(html, "function updateModelSelectionToolbar(visibleNames=[]){visibleNames=Array.isArray(visibleNames)?visibleNames:[];") {
		t.Fatal("selection toolbar must tolerate a missing or invalid visible model list")
	}
}

func TestModelPickerUsesExistingModelsAsEditableSelection(t *testing.T) {
	html := adminSource()
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
	html := adminSource()
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
	html := adminSource()
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
	html := adminSource()
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
	html := adminSource()
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

// The edit path must never send key_name: the PATCH endpoint rejects unknown
// fields, so a stray key_name made every “渠道设置” save fail with a cryptic
// JSON error and made channel addresses appear uneditable.
func TestProviderEditPayloadOmitsKeyName(t *testing.T) {
	js := string(adminJS)
	for _, required := range []string{
		`if(editing){delete payload.key_name;if(!String(o.credential||'').trim())delete payload.credential}`,
	} {
		if !strings.Contains(js, required) {
			t.Fatalf("provider edit payload must omit key_name, missing %q", required)
		}
	}
}

func TestProviderEditorUsesSingleAccessibleTabbedSurface(t *testing.T) {
	html := string(adminHTML)
	for _, required := range []string{
		`class="provider-form-tabs" role="tablist" aria-label="渠道设置"`,
		`id="providerTabConnectionButton" type="button" role="tab"`,
		`aria-controls="providerTabConnection" aria-selected="true"`,
		`id="providerTabKeysButton" type="button" role="tab"`,
		`aria-controls="providerTabKeys" aria-selected="false" aria-disabled="true"`,
		`id="providerTabRuntimeButton" type="button" role="tab"`,
		`role="tabpanel" aria-labelledby="providerTabConnectionButton" data-tab="connection"`,
		`role="tabpanel" aria-labelledby="providerTabKeysButton" data-tab="keys"`,
		`role="tabpanel" aria-labelledby="providerTabRuntimeButton" data-tab="runtime"`,
		`id="providerKeyAdd" class="btn btn-primary" type="button" onclick="addProviderKeyCard()" disabled`,
		`id="providerFormActions" class="form-actions span-12"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("unified provider editor is missing %q", required)
		}
	}
	if strings.Contains(html, `id="providerKeysModal"`) {
		t.Fatal("the obsolete standalone provider key modal is still present")
	}
	if got := strings.Count(html, `<symbol id="i-activity"`); got != 1 {
		t.Fatalf("i-activity symbol count = %d, want 1", got)
	}
	seen := map[string]bool{}
	for _, match := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if seen[match[1]] {
			t.Fatalf("admin shell contains duplicate id %q", match[1])
		}
		seen[match[1]] = true
	}
}

func TestProviderEditorGuardsStateAndAsyncLoads(t *testing.T) {
	js := string(adminJS)
	for _, required := range []string{
		`balance.dataset.original='';delete balance.dataset.clear`,
		`balanceInput.dataset.original=balanceValue;delete balanceInput.dataset.clear`,
		`event.target.name==='manualBalanceUSD'&&String(event.target.value).trim()!==''`,
		`delete event.target.dataset.clear`,
		`providerPanelGeneration++`,
		`generation!==providerPanelGeneration||Number(providerKeyManager.providerId)!==providerId`,
		`generation===providerPanelGeneration&&Number(providerEditId)===Number(id)`,
		`generation!==providerBalanceLoadGeneration||Number(providerBalanceEditId)!==Number(id)`,
		`setProviderKeysLoading(provider.api_key_count)`,
		`providerKeyManager={providerId:Number(id),items:[],revealed:new Map()}`,
		`if(!providerKeyManager.providerId)return notice('请先保存渠道，再添加 Key',true)`,
		`$('#keyTabCount').textContent=String(items.length)`,
		`$('#providerFormActions').classList.toggle('hidden',keys)`,
		`$('#providerSubmit').disabled=keys`,
		`if(!force&&(providerFormDirty||providerKeyDirty)&&!confirm(`,
		`current==='keys'&&name!=='keys'&&providerKeyDirty`,
		`current!=='keys'&&name==='keys'&&providerFormDirty`,
		`const panel=field.closest('.provider-tab-panel')`,
		`switchProviderTab(panel.dataset.tab,true)`,
		`field.reportValidity()`,
	} {
		if !strings.Contains(js, required) {
			t.Fatalf("provider editor lifecycle guard is missing %q", required)
		}
	}
}

func TestProviderEditorKeepsFinalMobileAndRenderingOverrides(t *testing.T) {
	css := string(adminCSS)
	js := string(adminJS)
	if strings.Contains(css, `.provider-keys-modal`) {
		t.Fatal("standalone provider key modal CSS is still present")
	}
	mobile := strings.LastIndex(css, "@media(max-width:780px){\n  .provider-form-modal")
	desktopRefresh := strings.LastIndex(css, `.provider-form-modal .form-actions{bottom:-22px`)
	if mobile < 0 || mobile <= desktopRefresh {
		t.Fatalf("final provider mobile override is not last/effective: mobile=%d desktop=%d", mobile, desktopRefresh)
	}
	if got := strings.Count(js, `$$('#providers button[onclick^="openProviderEdit("]').forEach`); got != 1 {
		t.Fatalf("provider edit button decoration loop count = %d, want 1", got)
	}
	for _, forbidden := range []string{
		`</div></div></td></tr>`,
		`$('#ipPoolSubmit').innerHTML=icon('i-check')+'保存渠道'`,
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("provider UI still contains obsolete rendering fragment %q", forbidden)
		}
	}
	if !strings.Contains(js, `$('#ipPoolSubmit').innerHTML=icon('i-check')+'保存节点'`) {
		t.Fatal("IP node editor save label is not 保存节点")
	}
}

func TestLightThemeUsesHighContrastCoolPalette(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		`/* Crisp visual system: strong hierarchy, compact controls, explicit status. */`,
		`html[data-theme="light"]{--bg:#eef3fb;--sidebar:#0d1a30;--surface:#fff;--surface-2:#f4f8fe;--surface-3:#e8effa`,
		`--text:#16233a;--muted:#556783;--muted-2:#8193ae;--accent:#2563eb;--accent-strong:#1d4ed8`,
		// The light sidebar stays dark, but through a token rather than a
		// per-component override.
		`--sidebar-bg:linear-gradient(180deg,#0e1c34,#0b1526)`,
		`background:var(--sidebar-bg)`,
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
	// The abandoned warm palette must not leak back into the light theme.
	for _, forbidden := range []string{"#fffdf8", "#f4f0e8", "#f2e5da", "#84452f", "#eae4da"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("warm cream palette value %q is back in the stylesheet", forbidden)
		}
	}
}

// Restyling the console at scale must stay a matter of editing the token blocks.
// Every themed colour therefore has to resolve through a var(--token); the only
// per-component theme rules allowed are the ones that toggle behaviour rather than
// colour.
func TestThemeIsDrivenBySingleTokenLayer(t *testing.T) {
	html := adminSource()
	if !strings.Contains(html, "THEME TOKENS - the single place to restyle the whole console.") {
		t.Fatal("the theme token layer is no longer documented")
	}
	style := string(adminCSS)
	allowed := map[string]bool{
		`html[data-theme="light"] body`: true,
		`html[data-theme="dark"] body`:  true,
		`html[data-theme="dark"] .theme-icon-sun,html[data-theme="light"] .theme-icon-moon`: true,
	}
	rules := regexp.MustCompile(`html\[data-theme="(?:light|dark)"\][^{]*\{[^}]*\}`).FindAllString(style, -1)
	for _, rule := range rules {
		selector := strings.TrimSpace(rule[:strings.Index(rule, "{")])
		if strings.Contains(rule, "--bg:") || allowed[selector] {
			continue
		}
		t.Fatalf("per-component theme override reintroduced: %q — express it as a token instead", selector)
	}
	// Guard the scopes that must not derive their foreground from the flipping
	// global --text/--muted tokens.
	for _, token := range []string{"--sidebar-fg", "--nav-fg", "--nav-active-bg", "--sidebar-status-bg", "--topbar-bg", "--modal-bg", "--th-bg", "--toast-bg"} {
		if strings.Count(style, token+":") < 2 {
			t.Fatalf("token %s is not declared for both themes", token)
		}
	}
}

func TestRequestLedgerHasServerSideDetailedTimeFilters(t *testing.T) {
	html := adminSource()
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

func TestQualityDetectorAppearsBelowRequestLedgerAndUsesNativeControls(t *testing.T) {
	html := adminSource()
	requestsNav := strings.Index(html, `data-page="requests"`)
	qualityNav := strings.Index(html, `data-page="quality"`)
	if requestsNav < 0 || qualityNav <= requestsNav {
		t.Fatalf("quality detector navigation is not below request ledger: requests=%d quality=%d", requestsNav, qualityNav)
	}
	for _, required := range []string{
		`id="page-quality"`,
		`质量检测`,
		`检测失败不等于中转一定主动掺水`,
		`真实上游密钥始终留在 FusionGate 内`,
		`id="qualityProvider"`,
		`id="qualityCredential"`,
		`target_id:target.id`,
		`<option value="low">低 · 快速排查</option>`,
		`<option value="medium">中 · 日常完整</option>`,
		`<option value="high">高 · 全面留证</option>`,
		`function startQualityDetection()`,
		`function stopQualityDetection()`,
		`/api/admin/quality-detector/start`,
		`/api/admin/quality-detector/status`,
		`/api/admin/quality-detector/report`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("quality detector UI is missing %q", required)
		}
	}
	for _, forbidden := range []string{`<iframe`, `/quality-detector/assets/`, `X-GPT56-Session`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("third-party detector frontend leaked into the console: %q", forbidden)
		}
	}
}

func TestCompletedHealthCheckPanelStaysHidden(t *testing.T) {
	html := adminSource()
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
	html := adminSource()
	for _, required := range []string{
		`draggable="true"`,
		"function routeDrop(event,targetId)",
		"function moveRoute(id,direction)",
		"/api/admin/routes/reorder",
		"route_ids:ids",
		"sort_order",
		"saveRouteOrder=async function(name,ids)",
		"const previous=new Map(routeOrder(name).map(route=>[Number(route.id),route.sort_order]))",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("routes page reordering is missing %q", required)
		}
	}
}

func TestProvidersPageSupportsPersistentGlobalReordering(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		"function providerDragStart(event,id)",
		"function providerDrop(event,targetId)",
		"function moveProvider(id,direction)",
		"/api/admin/providers/reorder",
		"provider_ids:ids",
		"decorateProviderOrder()",
		"cache.providers=providerOrder()",
		"saveProviderOrder=async function(ordinaryIDs)",
		"previous=new Map(cache.providers.map(provider=>[Number(provider.id),provider.sort_order]))",
		"清除搜索和状态筛选",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("provider reordering is missing %q", required)
		}
	}
	if strings.Contains(html, "cache.providers.sort((a,b)=>(b.priority-a.priority)") {
		t.Fatal("priority editing still overwrites the persisted provider order")
	}
	for _, forbidden := range []string{`provider-link-ext`, `>↗</span>`, `>•••</button>`, `>↑</button>`, `>↓</button>`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("raw provider or route reorder decoration remains: %q", forbidden)
		}
	}
	for _, required := range []string{`icon('i-grip')`, `icon('i-caret-up')`, `icon('i-caret-down')`, `aria-modal="true"`, `aria-labelledby="providerPanelTitle"`, `event.target===providerPanel`, `event.key!=='Escape'`} {
		if !strings.Contains(html, required) {
			t.Fatalf("provider UI polish is missing %q", required)
		}
	}
}

func TestProvidersPageSupportsArchiveFilter(t *testing.T) {
	html := adminSource()
	for _, required := range []string{
		"['all','enabled','disabled','circuit','archived']",
		"['all','全部渠道'],['enabled','参与调度'],['disabled','已停用'],['circuit','熔断冷却'],['archived','归档']",
		"余额耗尽但是优秀的站点",
		"toggleProviderArchive",
		"provider.archived&&providerStatusFilter==='all'",
		"provider.archived?'已归档':'未归档'",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("provider archive behavior is missing %q", required)
		}
	}
}
