import { useEffect, useMemo, useRef, useState } from "react"
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, RefreshCw, Search, HeartPulse, DatabaseBackup, Archive, FolderTree, GripVertical, ExternalLink, Server, CheckCircle2, Pause, Network } from "lucide-react"
import { api } from "@/lib/api"
import { providersApi, providerStatus, refreshProviderViews, isUncertainError, type ProviderPatch } from "@/lib/provider-management"
import { remainingBarTone } from "@/lib/codex-windows"
import { reorderProviderIDs } from "@/lib/provider-order"
import { providerSiteURL } from "@/lib/provider-site"
import type { Provider, RoutingSettings } from "@/lib/types"
import { ROUTING_STRATEGY_HELP } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import { notify } from "@/lib/notify"
import { usePageParams } from "@/lib/navigation"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import { ProviderDialog, type ProviderSection } from "@/components/ProviderDialog"
import { ProviderPassthroughLabel } from "@/components/ProviderPassthroughLabel"
import { ProviderEgressDialog } from "@/components/ProviderEgressDialog"
import { ProviderBatchDialog, type ProviderBatchEdit } from "@/components/ProviderBatchDialog"
import { HealthCheckDialog } from "@/components/HealthCheckDialog"
import { ExportImportDialog } from "@/components/ExportImportDialog"
import { GroupManager } from "@/components/GroupManager"
import { InlinePriorityEditor } from "@/components/InlinePriorityEditor"
import { useConfirm } from "@/components/ui/confirm"
import { QueryError } from "@/components/ui/query-error"
import { StatCard } from "@/components/ui/stat-card"

type Filter = "all" | "enabled" | "disabled" | "archived" | "unhealthy"
const typeLabels: Record<string, string> = { openai: "OpenAI", grok: "Grok", openrouter: "OpenRouter", openai_compatible: "OpenAI 兼容", anthropic: "Anthropic 官方", anthropic_compatible: "Anthropic 兼容", claude_oauth: "Claude OAuth", codex_oauth: "Codex OAuth", grok_oauth: "Grok OAuth", gemini: "Gemini", opencode: "OpenCode" }
const selectClass = "h-8 max-w-44 rounded-md border border-input bg-background px-2 text-xs"
const emptyProviders: Provider[] = []

export function Providers() {
  const client = useQueryClient()
  const confirm = useConfirm()
  const params = usePageParams()
  const [filter, setFilter] = useState<Filter>("all")
  const [search, setSearch] = useState("")
  const [groupFilter, setGroupFilter] = useState("")
  const [egressFilter, setEgressFilter] = useState("")
  const [order, setOrder] = useState<"position" | "priority">("position")
  const [sorting, setSorting] = useState(false)
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Provider | null>(null)
  const [initialSection, setInitialSection] = useState<ProviderSection>("connection")
  const [healthIds, setHealthIds] = useState<number[]>([])
  const [egressIds, setEgressIds] = useState<number[]>([])
  const [batchEdit, setBatchEdit] = useState<{ action: ProviderBatchEdit; providers: Provider[] } | null>(null)
  const [backupOpen, setBackupOpen] = useState(false)
  const [groupOpen, setGroupOpen] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [draggingId, setDraggingId] = useState<number | null>(null)
  const [dragOverId, setDragOverId] = useState<number | null>(null)
  const [batchCheck, setBatchCheck] = useState("")
  const selectAll = useRef<HTMLInputElement>(null)
  const openedLink = useRef("")
  const { data: routing } = useQuery({ queryKey: ["routing"], queryFn: ({ signal }) => api<RoutingSettings>("/api/admin/routing", { signal }), staleTime: 30_000 })
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: ({ signal }) => providersApi.list(signal) })
  const providers = providersQuery.data ?? emptyProviders
  const nodesQuery = useQuery({ queryKey: ["ippool"], queryFn: ({ signal }) => providersApi.nodes(signal), staleTime: 30_000 })
  const groupsQuery = useQuery({ queryKey: ["provider-groups"], queryFn: ({ signal }) => providersApi.groups(signal), staleTime: 30_000 })
  const nodes = nodesQuery.data ?? []
  const groups = groupsQuery.data ?? []
  const visibleProviders = useMemo(() => providers.filter((provider) => provider.auth_kind !== "oauth"), [providers])
  const balanceTargets = useMemo(() => visibleProviders.filter((provider) => (provider.manual_balance_micros ?? 0) > 0), [visibleProviders])
  const balances = useQueries({ queries: balanceTargets.map((provider) => ({ queryKey: ["balance", provider.id], queryFn: ({ signal }: { signal: AbortSignal }) => api<{ manual?: { remaining_micros: number; used_percent: number } }>("/api/admin/providers/" + provider.id + "/balance", { signal }), staleTime: 60_000 })) })
  const balanceMap = new Map(balanceTargets.flatMap((provider, index) => balances[index]?.data?.manual ? [[provider.id, balances[index].data!.manual!] as const] : []))
  const update = useMutation({ mutationFn: ({ id, patch }: { id: number; patch: ProviderPatch }) => providersApi.patch(id, patch), onSuccess: () => refreshProviderViews(client) })
  const reorder = useMutation({ mutationFn: ({ sourceId, targetId }: { sourceId: number; targetId: number }) => providersApi.reorder(reorderProviderIDs(providers.map((provider) => provider.id), sourceId, targetId)), onSuccess: () => refreshProviderViews(client) })
  const batch = useMutation({
    mutationFn: ({ ids, action }: { ids: number[]; action: "enable" | "disable" | "delete" }) => providersApi.batch(ids, action),
    onSuccess: async (result, variables) => { await refreshProviderViews(client); clearApplied(variables.ids); notify({ tone: "success", title: "批量操作完成", description: "已处理 " + result.affected + " 个渠道。" }) },
  })
  const busy = batch.isPending || update.isPending || reorder.isPending
  const batchUncertain = isUncertainError(batch.error)
  const filtered = useMemo(() => {
    let list = visibleProviders.filter((provider) => filter === "archived" ? provider.archived : !provider.archived)
    if (filter === "enabled") list = list.filter((provider) => provider.enabled)
    if (filter === "disabled") list = list.filter((provider) => !provider.enabled)
    if (filter === "unhealthy") list = list.filter((provider) => ["danger", "warning"].includes(providerStatus(provider).variant))
    if (groupFilter) list = list.filter((provider) => (provider.group_id ?? 0) === Number(groupFilter))
    if (egressFilter) list = list.filter((provider) => (provider.ip_pool_node_id ?? 0) === Number(egressFilter))
    const keyword = search.trim().toLowerCase()
    if (keyword) list = list.filter((provider) => [provider.name, provider.base_url, provider.website_url || "", provider.notes, provider.ip_pool_node_name || ""].some((value) => value.toLowerCase().includes(keyword)))
    return order === "priority" ? [...list].sort((first, second) => second.priority - first.priority || first.sort_order - second.sort_order || first.id - second.id) : list
  }, [visibleProviders, filter, search, groupFilter, egressFilter, order])
  const selectedProviders = visibleProviders.filter((provider) => selected.has(provider.id))
  const hiddenSelection = selectedProviders.filter((provider) => !filtered.some((visible) => visible.id === provider.id)).length
  const selectedCount = selectedProviders.length
  const healthEligible = selectedProviders.filter((provider) => provider.enabled && provider.health_check_enabled && !provider.archived).map((provider) => provider.id)
  const counts = {
    all: visibleProviders.filter((provider) => !provider.archived).length,
    enabled: visibleProviders.filter((provider) => !provider.archived && provider.enabled).length,
    disabled: visibleProviders.filter((provider) => !provider.archived && !provider.enabled).length,
    archived: visibleProviders.filter((provider) => provider.archived).length,
    unhealthy: visibleProviders.filter((provider) => !provider.archived && ["danger", "warning"].includes(providerStatus(provider).variant)).length,
  }
  useEffect(() => { if (selectAll.current) selectAll.current.indeterminate = filtered.some((provider) => selected.has(provider.id)) && !filtered.every((provider) => selected.has(provider.id)) }, [filtered, selected])
  useEffect(() => {
    if (!providersQuery.data) return
    const allowed = new Set(visibleProviders.map((provider) => provider.id))
    setSelected((previous) => [...previous].every((id) => allowed.has(id)) ? previous : new Set([...previous].filter((id) => allowed.has(id))))
  }, [providersQuery.data, visibleProviders])
  useEffect(() => {
    const id = Number(params.get("provider"))
    const signature = params.toString()
    const provider = providers.find((item) => item.id === id)
    if (!provider || openedLink.current === signature) return
    openedLink.current = signature
    if (provider.auth_kind === "oauth") { location.hash = "authfiles"; return }
    const requested = params.get("section")
    setInitialSection(requested === "keys" || requested === "models" || requested === "routing" || requested === "health" ? requested : "connection")
    setEditing(provider)
    setDialogOpen(true)
  }, [params, providers])
  function openProvider(provider: Provider | null, section: ProviderSection = "connection") { setEditing(provider); setInitialSection(section); setDialogOpen(true) }
  function clearApplied(ids: number[]) { setSelected((previous) => new Set([...previous].filter((id) => !ids.includes(id)))) }
  function toggleSelected(id: number, checked: boolean) { setSelected((previous) => { const next = new Set(previous); if (checked) next.add(id); else next.delete(id); return next }) }
  async function deleteSelected() {
    const ids = selectedProviders.map((provider) => provider.id)
    if (await confirm({ title: "删除选中的 " + ids.length + " 个渠道？", description: selectedProviders.map((provider) => provider.name).slice(0, 5).join("、") + "。渠道、Key 和模型路由将被删除，此操作不可恢复。", destructive: true, confirmLabel: "确认删除" })) batch.mutate({ ids, action: "delete" })
  }
  async function verifyBatch() {
    if (!batch.variables) return
    try {
      const latest = await providersApi.list()
      const { ids, action } = batch.variables
      const remaining = ids.filter((id) => {
        const provider = latest.find((item) => item.id === id)
        return action === "delete" ? !!provider : !provider || provider.enabled !== (action === "enable")
      })
      client.setQueryData(["providers"], latest)
      clearApplied(ids.filter((id) => !remaining.includes(id)))
      setBatchCheck(`已核对当前配置：${ids.length - remaining.length} / ${ids.length} 项与目标一致，未重复提交。${remaining.length ? "未达目标项的选择已保留。" : ""}`)
      batch.reset()
    } catch (error) { setBatchCheck(error instanceof Error ? error.message : "配置读取失败，请稍后重试") }
  }
  return <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
    <div className="mb-5 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between"><div><h1 className="text-2xl font-bold tracking-tight">上游渠道</h1><p className="mt-1 text-sm text-muted-foreground">管理 API 渠道、Key、模型与默认出口。OAuth 渠道在认证文件页管理。</p></div><div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => setGroupOpen(true)}><FolderTree className="h-4 w-4" />分组管理</Button><Button variant="outline" onClick={() => setBackupOpen(true)}><DatabaseBackup className="h-4 w-4" />备份</Button><Button onClick={() => openProvider(null)}><Plus className="h-4 w-4" />添加渠道</Button></div></div>
    <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4"><StatCard label="活动渠道" value={counts.all} icon={<Server className="h-4 w-4" />} tone="text-foreground" sub={"含归档总计 " + visibleProviders.length} /><StatCard label="已启用" value={counts.enabled} icon={<CheckCircle2 className="h-4 w-4" />} tone="text-emerald-600" sub="配置启用，不等同检活通过" /><StatCard label="已停用" value={counts.disabled} icon={<Pause className="h-4 w-4" />} tone="text-amber-500" sub="不参与调度" /><StatCard label="已归档" value={counts.archived} icon={<Archive className="h-4 w-4" />} tone="text-muted-foreground" sub="保留配置与历史" /></div>
    <details className="mb-4 rounded-lg border px-3 py-2 text-xs text-muted-foreground"><summary className="cursor-pointer font-medium text-foreground">排序规则与出口说明</summary><p className="mt-2">{routing ? ROUTING_STRATEGY_HELP[routing.strategy] : "优先级从高到低，同优先级按全局位置排序。"}</p><p className="mt-1">拖动只调整同优先级的全局位置，不能跨越更高优先级。OAuth 渠道也可能参与调度；某次请求的实际尝试顺序以请求账本为准。</p><p className="mt-1">列表显示渠道默认出口，Key 的独立节点与强制直连不会被它覆盖。</p></details>
    <Card><CardContent className="p-0">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b p-3"><div className="flex flex-wrap gap-1.5">{([["all", "活动", counts.all], ["enabled", "已启用", counts.enabled], ["disabled", "已停用", counts.disabled], ["unhealthy", "需关注", counts.unhealthy], ["archived", "归档", counts.archived]] as [Filter, string, number][]).map(([value, label, count]) => <button key={value} type="button" disabled={busy} onClick={() => setFilter(value)} className={cn("rounded-lg border px-2.5 py-1.5 text-xs", filter === value ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground")}>{label} {count}</button>)}</div><div className="flex gap-2"><Button size="sm" variant={sorting ? "default" : "outline"} disabled={busy} onClick={() => { setSorting(!sorting); setOrder("position") }}><GripVertical className="h-4 w-4" />{sorting ? "完成排序" : "调整顺序"}</Button><Button size="sm" variant="ghost" disabled={busy || providersQuery.isFetching} onClick={() => void refreshProviderViews(client)} aria-label="刷新渠道"><RefreshCw className={cn("h-4 w-4", providersQuery.isFetching && "animate-spin")} /></Button></div></div>
      <div className="flex flex-wrap items-center gap-2 border-b p-3"><div className="relative"><Search className="absolute left-2.5 top-2 h-3.5 w-3.5 text-muted-foreground" /><Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索渠道、地址、备注" aria-label="搜索渠道" className="h-8 w-52 pl-8 text-xs" /></div><select aria-label="筛选分组" value={groupFilter} onChange={(event) => setGroupFilter(event.target.value)} disabled={groupsQuery.isPending || groupsQuery.isError} className={selectClass}><option value="">全部分组</option><option value="0">未分组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select><select aria-label="筛选默认出口" value={egressFilter} onChange={(event) => setEgressFilter(event.target.value)} disabled={nodesQuery.isPending || nodesQuery.isError} className={selectClass}><option value="">全部默认出口</option><option value="0">本机直连</option>{nodes.map((node) => <option key={node.id} value={node.id}>{node.name}{node.enabled ? "" : "（停用）"}</option>)}</select><select aria-label="列表展示顺序" value={order} onChange={(event) => setOrder(event.target.value as "position" | "priority")} disabled={sorting} className={selectClass}><option value="position">配置位置</option><option value="priority">调度优先级顺序</option></select>{(search || groupFilter || egressFilter) && <Button size="sm" variant="ghost" onClick={() => { setSearch(""); setGroupFilter(""); setEgressFilter("") }}>清空筛选</Button>}</div>
      {(nodesQuery.isError || groupsQuery.isError) && <QueryError title="部分筛选数据读取失败" error={nodesQuery.error || groupsQuery.error} onRetry={() => { void nodesQuery.refetch(); void groupsQuery.refetch() }} className="m-3 p-3" />}
      {selectedCount > 0 && <div className="sticky top-0 z-20 flex flex-wrap items-center gap-2 border-b bg-background/95 p-3 shadow-sm backdrop-blur" aria-label="渠道批量操作"><span className="text-xs font-medium">已选 {selectedCount}{hiddenSelection > 0 ? " · " + hiddenSelection + " 项不在当前筛选结果" : ""}</span><Button size="sm" variant="outline" disabled={busy || batchUncertain || selectedCount > 200} onClick={() => setEgressIds(selectedProviders.map((provider) => provider.id))}><Network className="h-4 w-4" />指定出口</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => setBatchEdit({ action: "priority", providers: selectedProviders })}>设置优先级</Button><Button size="sm" variant="outline" disabled={busy} onClick={() => setBatchEdit({ action: "group", providers: selectedProviders })}>设置分组</Button><Button size="sm" variant="outline" disabled={busy || batchUncertain || selectedCount > 200} onClick={() => batch.mutate({ ids: selectedProviders.map((provider) => provider.id), action: "enable" })}>启用</Button><Button size="sm" variant="outline" disabled={busy || batchUncertain || selectedCount > 200} onClick={() => batch.mutate({ ids: selectedProviders.map((provider) => provider.id), action: "disable" })}>停用</Button><Button size="sm" variant="outline" disabled={busy || !healthEligible.length} title={"将跳过 " + (selectedCount - healthEligible.length) + " 项停用、归档或关闭检活的渠道"} onClick={() => setHealthIds(healthEligible)}><HeartPulse className="h-4 w-4" />检活（{healthEligible.length}）</Button><select aria-label="更多批量操作" className={selectClass} value="" disabled={busy || batchUncertain} onChange={(event) => { const action = event.target.value; if (action === "delete") void deleteSelected(); else if (action === "archive" || action === "unarchive") setBatchEdit({ action, providers: selectedProviders }) }}><option value="">更多操作…</option><option value="archive">归档</option><option value="unarchive">取消归档</option><option value="delete" disabled={selectedCount > 200}>删除</option></select><Button size="sm" variant="ghost" disabled={busy} onClick={() => setSelected(new Set())}>清空选择</Button>{selectedCount > 200 && <span className="text-xs text-amber-600">原生批量操作单次最多 200 项</span>}</div>}
      {batch.error && <p role="alert" className="border-b p-3 text-xs text-destructive">{batch.error instanceof Error ? batch.error.message : "批量操作失败"}。选择已保留；连接中断时请先刷新核对配置，不要重复提交。</p>}
      {batchUncertain && <div className="p-3"><Button size="sm" variant="outline" onClick={() => void verifyBatch()}>读取配置核对</Button></div>}{batchCheck && <p role="status" className="p-3 text-xs text-muted-foreground">{batchCheck}</p>}
      {providersQuery.isPending ? <div role="status" className="p-8 text-center text-sm text-muted-foreground">读取渠道中…</div> : providersQuery.isError ? <QueryError title="无法读取渠道" error={providersQuery.error} onRetry={() => void providersQuery.refetch()} className="m-4" /> : !filtered.length ? <div className="p-8 text-center text-sm text-muted-foreground">没有符合条件的渠道</div> : <div className="overflow-x-auto"><table className="w-full text-sm"><thead><tr className="border-b text-left text-xs text-muted-foreground"><th className="w-12 px-3 py-3"><input ref={selectAll} type="checkbox" aria-label="选择当前筛选全部渠道" disabled={busy} checked={filtered.length > 0 && filtered.every((provider) => selected.has(provider.id))} onChange={(event) => { const checked = event.target.checked; setSelected((previous) => { const next = new Set(previous); for (const provider of filtered) { if (checked) next.add(provider.id); else next.delete(provider.id) } return next }) }} /></th><th className="px-3 py-3 font-medium">渠道 / 分组</th><th className="px-3 py-3 font-medium">优先级</th><th className="px-3 py-3 font-medium">默认出口</th><th className="px-3 py-3 font-medium">状态</th><th className="px-3 py-3 font-medium">公共模型</th><th className="px-3 py-3 text-right font-medium">操作</th></tr></thead><tbody>{filtered.map((provider) => {
        const state = providerStatus(provider)
        const balance = balanceMap.get(provider.id)
        const node = nodes.find((item) => item.id === provider.ip_pool_node_id)
        const site = providerSiteURL(provider.base_url, provider.website_url)
        return <tr key={provider.id} draggable={sorting && !busy} onDragStart={() => setDraggingId(provider.id)} onDragEnd={() => { setDraggingId(null); setDragOverId(null) }} onDragOver={(event) => { if (sorting && draggingId && providers.find((item) => item.id === draggingId)?.priority === provider.priority) { event.preventDefault(); setDragOverId(provider.id) } }} onDrop={(event) => { event.preventDefault(); if (sorting && !busy && draggingId && draggingId !== provider.id && providers.find((item) => item.id === draggingId)?.priority === provider.priority) reorder.mutate({ sourceId: draggingId, targetId: provider.id }); setDraggingId(null); setDragOverId(null) }} className={cn("border-b last:border-0 even:bg-muted/20 hover:bg-muted/40", draggingId === provider.id && "opacity-40", dragOverId === provider.id && "bg-primary/10")}><td className="px-3 py-3"><div className="flex items-center gap-1"><input type="checkbox" aria-label={"选择 " + provider.name} checked={selected.has(provider.id)} disabled={busy} onChange={(event) => toggleSelected(provider.id, event.target.checked)} />{sorting && <GripVertical className="h-4 w-4 cursor-grab text-muted-foreground" />}</div></td><td className="min-w-44 px-3 py-3"><div className="flex items-center gap-2"><button className="text-left font-medium hover:text-primary hover:underline" onClick={() => openProvider(provider)}>{provider.name}</button>{site && <a href={site} target="_blank" rel="noreferrer noopener" title="打开商家网站" aria-label={"打开 " + provider.name + " 商家网站"}><ExternalLink className="h-3.5 w-3.5 text-muted-foreground" /></a>}</div><p className="max-w-56 truncate text-xs text-muted-foreground" title={provider.base_url}>{provider.base_url}</p><div className="mt-1 flex flex-wrap items-center gap-1 text-[11px] text-muted-foreground"><span>{typeLabels[provider.type] ?? provider.type}</span><span>· {groups.find((group) => group.id === provider.group_id)?.name || (provider.group_id ? "分组 #" + provider.group_id : "未分组")}</span></div><ProviderPassthroughLabel provider={provider} />{balance && <div className="mt-2 max-w-44"><p className="mb-1 text-[10px] text-muted-foreground">剩余余额 {formatCost(balance.remaining_micros)}</p><div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className={cn("h-full", remainingBarTone(100 - balance.used_percent))} style={{ width: Math.min(100, Math.max(0, 100 - balance.used_percent)) + "%" }} /></div></div>}</td><td className="px-3 py-3"><InlinePriorityEditor value={provider.priority} disabled={busy} onSave={async (priority) => { await update.mutateAsync({ id: provider.id, patch: { priority } }) }} /></td><td className="px-3 py-3"><button onClick={() => openProvider(provider, "routing")} className="max-w-40 text-left text-xs hover:text-primary hover:underline">{provider.ip_pool_node_name || (provider.ip_pool_node_id ? "节点 #" + provider.ip_pool_node_id : "本机直连")}</button>{node && !node.enabled && <p className="mt-1 text-[10px] text-destructive">节点已停用</p>}<p className="mt-1 text-[10px] text-muted-foreground">Key 可独立覆盖</p></td><td className="px-3 py-3"><Badge variant={state.variant}>{state.label}</Badge><p className="mt-1 text-[10px] text-muted-foreground">{provider.archived ? "不参与调度" : provider.enabled ? "配置已启用" : "配置已停用"}</p>{provider.consecutive_failures > 0 && <p className="text-[10px] text-muted-foreground">{provider.consecutive_failures}/{provider.failure_threshold} 失败</p>}</td><td className="px-3 py-3"><button onClick={() => openProvider(provider, "models")} className="whitespace-nowrap text-xs hover:text-primary hover:underline">{provider.model_count} 个</button></td><td className="px-3 py-3"><div className="flex items-center justify-end gap-1"><Button size="sm" variant="outline" onClick={() => openProvider(provider)} aria-label={"管理 " + provider.name}>管理</Button><select aria-label={provider.name + " 快捷操作"} value="" className="h-8 w-16 rounded-md border bg-background text-xs" onChange={(event) => { const action = event.target.value; if (action === "requests") location.hash = "requests?provider=" + provider.id; else if (action === "keys" || action === "routing" || action === "models") openProvider(provider, action) }}><option value="">更多</option><option value="keys">Key</option><option value="routing">出口</option><option value="models">模型</option><option value="requests">账本</option></select><Switch checked={provider.enabled} disabled={busy || provider.archived} onCheckedChange={(enabled) => update.mutate({ id: provider.id, patch: { enabled } })} aria-label={provider.name + " 开关"} /></div></td></tr>
      })}</tbody></table></div>}
    </CardContent></Card>
    <p className="mt-3 text-xs text-muted-foreground">活动 {counts.all} · 归档 {counts.archived} · 共 {visibleProviders.length} 个 API 渠道。勾选列表可批量管理。</p>
    <ProviderDialog open={dialogOpen} onOpenChange={setDialogOpen} provider={editing} initialSection={initialSection} />
    <ProviderEgressDialog open={egressIds.length > 0} onOpenChange={(open) => { if (!open) setEgressIds([]) }} providerIds={egressIds} onApplied={clearApplied} />
    {batchEdit && <ProviderBatchDialog open providers={batchEdit.providers} action={batchEdit.action} onOpenChange={(open) => { if (!open) setBatchEdit(null) }} onApplied={clearApplied} />}
    {healthIds.length > 0 && <HealthCheckDialog open onOpenChange={(open) => { if (!open) setHealthIds([]) }} providerIds={healthIds} title={"批量检活 · " + healthIds.length + " 个渠道"} />}
    <ExportImportDialog open={backupOpen} onOpenChange={setBackupOpen} /><GroupManager open={groupOpen} onOpenChange={setGroupOpen} />
  </motion.div>
}
