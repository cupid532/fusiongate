import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Coins, Plus, RefreshCw, Search, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { ModelAlias, PricingStatus, PricingSyncResult, Route, RoutingStrategy } from "@/lib/types"
import { ROUTING_STRATEGY_HELP, ROUTING_STRATEGY_LABELS } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { InlinePriorityEditor } from "@/components/InlinePriorityEditor"
import { ModelAliasManager } from "@/components/ModelAliasManager"
import { PricingDialog } from "@/components/PricingDialog"
import { RouteDialog } from "@/components/RouteDialog"

const price = (micros: number) => `$${(micros / 1_000_000).toFixed(micros % 1_000_000 === 0 ? 0 : 3)}`
const pricingSource = (source?: string) => !source ? "未定价" : source === "manual" ? "手工" : source.includes("openrouter.ai") ? "OpenRouter" : "官网"

const strategyLabels = ROUTING_STRATEGY_LABELS
const strategyHelp = ROUTING_STRATEGY_HELP

type StatusVariant = "success" | "warning" | "danger" | "neutral"

function routeState(route: Route): { label: string; detail: string; variant: StatusVariant; eligible: boolean } {
  if (!route.enabled) return { label: "不参与", detail: "路由已停用", variant: "neutral", eligible: false }
  if (route.provider_archived) return { label: "不参与", detail: "渠道已归档", variant: "neutral", eligible: false }
  if (!route.provider_enabled) return { label: "不参与", detail: "渠道已停用", variant: "neutral", eligible: false }
  const circuitUntil = route.provider_circuit_open_until ? Date.parse(route.provider_circuit_open_until) : 0
  if (circuitUntil > Date.now()) return { label: "临时跳过", detail: `熔断至 ${new Date(circuitUntil).toLocaleTimeString()}`, variant: "warning", eligible: false }
  if (route.provider_status === "auth_expired") return { label: "待恢复", detail: "上游认证异常，将由半开探针重试", variant: "danger", eligible: true }
  if (route.provider_status === "rate_limited") return { label: "待恢复", detail: "上游限流，将在冷却后重试", variant: "warning", eligible: true }
  if (route.health_check_status === "unhealthy") return { label: "可调度", detail: "最近检活失败，真实请求仍受熔断保护", variant: "warning", eligible: true }
  return { label: "参与调度", detail: route.provider_inflight > 0 ? `当前并发 ${route.provider_inflight}` : "当前可用", variant: "success", eligible: true }
}

function sortRoutes(routes: Route[], strategy: RoutingStrategy) {
  return [...routes].sort((a, b) => {
    if (strategy === "priority_failover" && a.provider_priority !== b.provider_priority) return b.provider_priority - a.provider_priority
    if (a.provider_sort_order !== b.provider_sort_order) return a.provider_sort_order - b.provider_sort_order
    if (strategy === "priority_failover" && a.priority !== b.priority) return b.priority - a.priority
    if (a.sort_order !== b.sort_order) return a.sort_order - b.sort_order
    return a.id - b.id
  })
}

export function Routes() {
  const qc = useQueryClient()
  const [q, setQ] = useState("")
  const [pricingOpen, setPricingOpen] = useState(false)
  const [pricingModel, setPricingModel] = useState("")
  const [routeOpen, setRouteOpen] = useState(false)
  const { data: routes = [], isLoading } = useQuery({ queryKey: ["routes"], queryFn: () => api<Route[]>("/api/admin/routes") })
  const { data: aliases = [] } = useQuery({ queryKey: ["model-aliases"], queryFn: () => api<ModelAlias[]>("/api/admin/model-aliases") })
  const { data: routing } = useQuery({ queryKey: ["routing"], queryFn: () => api<{ strategy: RoutingStrategy }>("/api/admin/routing") })
  const { data: pricing } = useQuery({ queryKey: ["pricing"], queryFn: () => api<PricingStatus>("/api/admin/pricing") })
  const strategy = routing?.strategy ?? "priority_failover"

  const syncPricing = useMutation({ mutationFn: () => api<PricingSyncResult>("/api/admin/pricing", { method: "POST" }), onSuccess: () => { qc.invalidateQueries({ queryKey: ["pricing"] }); qc.invalidateQueries({ queryKey: ["routes"] }) } })
  const setStrategy = useMutation({ mutationFn: (next: RoutingStrategy) => api("/api/admin/routing", { method: "PATCH", body: JSON.stringify({ strategy: next }) }), onSuccess: () => qc.invalidateQueries({ queryKey: ["routing"] }) })
  const updateRoute = useMutation({
    mutationFn: ({ id, patch }: { id: number; patch: Record<string, unknown> }) => api(`/api/admin/routes/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["routes"] }); qc.invalidateQueries({ queryKey: ["model-aliases"] }) },
  })
  const remove = useMutation({ mutationFn: (id: number) => api(`/api/admin/routes/${id}`, { method: "DELETE" }), onSuccess: () => { qc.invalidateQueries({ queryKey: ["routes"] }); qc.invalidateQueries({ queryKey: ["model-aliases"] }) } })

  const groups = useMemo(() => {
    const map = new Map<string, Route[]>()
    for (const route of routes) map.set(route.public_name, [...(map.get(route.public_name) ?? []), route])
    const keyword = q.trim().toLowerCase()
    return [...map.entries()]
      .map(([name, list]) => [name, sortRoutes(list, strategy)] as [string, Route[]])
      .filter(([name, list]) => {
        if (!keyword) return true
        const groupAliases = aliases.filter((item) => item.target_model === name)
        return name.toLowerCase().includes(keyword)
          || groupAliases.some((item) => item.alias.toLowerCase().includes(keyword))
          || list.some((route) => route.upstream_model.toLowerCase().includes(keyword) || route.provider_name?.toLowerCase().includes(keyword))
      })
  }, [routes, aliases, q, strategy])
  const modelNames = useMemo(() => [...new Set(routes.map((route) => route.public_name))].sort(), [routes])
  const selectedRoutes = routes.filter((route) => route.public_name === pricingModel)
  const status = pricing?.status ?? {}

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
        <div><h1 className="text-2xl font-bold tracking-tight">模型路由</h1><p className="mt-1 text-sm text-muted-foreground">按规范模型组管理调用名称、渠道成员与健康感知的请求内故障转移。</p></div>
        <div className="flex flex-wrap items-center gap-2"><div className="relative"><Search className="absolute left-2.5 top-1/2 -translate-y-1/2 text-muted-foreground" /><Input value={q} onChange={(event) => setQ(event.target.value)} placeholder="搜索模型、别名、渠道" className="h-9 w-60 pl-8 text-xs" /></div><Button onClick={() => setRouteOpen(true)}><Plus />添加渠道成员</Button></div>
      </div>

      <Card className="mb-4 overflow-hidden">
        <CardContent className="flex flex-col gap-3 p-4 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <div className="flex flex-wrap items-center gap-2"><div className="text-sm font-semibold">起始渠道选择策略</div><Badge variant="default">{strategyLabels[strategy]}</Badge></div>
            <div className="mt-1 text-xs text-muted-foreground">{strategyHelp[strategy]} 所有策略都带请求内故障转移：起点失败后自动依次尝试其余渠道，并受熔断与半开探活保护；调用别名与规范名称共享同一调度状态。</div>
          </div>
          <select value={strategy} onChange={(event) => setStrategy.mutate(event.target.value as RoutingStrategy)} disabled={setStrategy.isPending} className="h-9 rounded-md border border-input bg-transparent px-3 text-sm">
            {(Object.keys(strategyLabels) as RoutingStrategy[]).map((value) => <option key={value} value={value}>{strategyLabels[value]}</option>)}
          </select>
        </CardContent>
      </Card>

      <Card className="mb-4 overflow-hidden">
        <CardContent className="flex flex-col gap-3 p-4 lg:flex-row lg:items-center lg:justify-between">
          <div><div className="flex flex-wrap items-center gap-2"><div className="text-sm font-semibold">模型价格同步</div><Badge variant={status.pricing_last_error ? "warning" : "success"}>{pricing?.interval === "0s" ? "自动同步已停用" : status.pricing_last_sync ? "自动同步已启用" : "等待首次同步"}</Badge><span className="text-xs text-muted-foreground">周期 {pricing?.interval ?? "—"}</span></div><div className="mt-1 text-xs text-muted-foreground">优先官网，OpenRouter 兜底 · 上次 {status.pricing_last_sync ? new Date(status.pricing_last_sync).toLocaleString() : "尚未同步"} · {status.pricing_last_sources ?? 0} 个来源 / {status.pricing_last_models ?? 0} 个价格 / 更新 {status.pricing_last_updated_routes ?? 0} 条路由</div>{status.pricing_last_error && <div className="mt-1 max-w-4xl truncate text-xs text-amber-700 dark:text-amber-400">{status.pricing_last_error}</div>}{syncPricing.data?.errors?.length ? <div className="mt-1 text-xs text-amber-700 dark:text-amber-400">{syncPricing.data.errors.join("；")}</div> : null}</div>
          <Button variant="outline" onClick={() => syncPricing.mutate()} disabled={syncPricing.isPending}><RefreshCw className={syncPricing.isPending ? "animate-spin" : ""} />立即同步</Button>
        </CardContent>
      </Card>

      {isLoading ? <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div> : groups.length === 0 ? <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">没有符合条件的模型组</div> : (
        <div className="space-y-4">{groups.map(([name, list]) => {
          const route = list[0]
          const groupAliases = aliases.filter((item) => item.target_model === name)
          const states = list.map(routeState)
          const eligible = states.filter((item) => item.eligible).length
          return <Card key={name} className="overflow-hidden"><CardContent className="p-0">
            <div className="border-b px-4 py-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-mono text-sm font-semibold">{name}</span>
                  <Badge variant="neutral">{list.length} 条路由</Badge>
                  <Badge variant={eligible > 1 ? "success" : eligible === 1 ? "warning" : "danger"}>{eligible} 个可调度成员</Badge>
                  {groupAliases.length > 0 && <Badge variant="outline">{groupAliases.length} 个调用别名</Badge>}
                  {eligible < 2 && <span className="text-xs text-amber-700 dark:text-amber-400">当前无法形成轮询冗余</span>}
                  {route.input_price_micros || route.output_price_micros ? <Badge variant={route.pricing_source === "manual" ? "warning" : "success"}>输入 {price(route.input_price_micros)} · 输出 {price(route.output_price_micros)} · {pricingSource(route.pricing_source)}</Badge> : <Badge variant="neutral">未定价</Badge>}
                </div>
                <Button variant="ghost" size="sm" onClick={() => { setPricingModel(name); setPricingOpen(true) }}><Coins />定价</Button>
              </div>
            </div>
            <div className="divide-y">{list.map((item, index) => {
              const state = routeState(item)
              return <div key={item.id} className="grid gap-3 px-4 py-3 hover:bg-muted/30 lg:grid-cols-[36px_minmax(0,1fr)_minmax(180px,auto)_auto] lg:items-center">
                <div className="flex h-7 w-7 items-center justify-center rounded-full border bg-muted/40 text-xs font-semibold tabular-nums text-muted-foreground" title="当前策略下的配置顺序">{index + 1}</div>
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2"><span className="text-sm font-medium">{item.provider_name || "—"}</span><Badge variant="outline">渠道优先级 {item.provider_priority}</Badge></div>
                  <div className="mt-0.5 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground"><span>上游模型</span><code className="truncate rounded bg-muted px-1.5 py-0.5">{item.upstream_model}</code></div>
                  <div className="mt-1.5 flex flex-wrap items-center gap-1.5"><Badge variant="outline">{item.provider_type}</Badge><Badge variant="outline">{item.capabilities}</Badge><span className="text-xs text-muted-foreground">组内优先级</span><InlinePriorityEditor value={item.priority} disabled={updateRoute.isPending} onSave={async (priority) => { await updateRoute.mutateAsync({ id: item.id, patch: { priority } }) }} /></div>
                </div>
                <div>
                  <div className="flex flex-wrap items-center gap-2"><Badge variant={state.variant}>{state.label}</Badge><span className="text-xs text-muted-foreground">{state.detail}</span></div>
                  <div className="mt-1 text-xs text-muted-foreground">首字节 {item.provider_first_byte_ms || "—"} ms · 总延迟 {item.provider_latency_ms || "—"} ms · 连续失败 {item.provider_failures}</div>
                </div>
                <div className="flex flex-wrap items-center justify-end gap-1.5">
                  <select
                    value=""
                    onChange={(event) => {
                      const target = event.target.value
                      if (!target) return
                      if (confirm(`将「${item.provider_name} / ${item.upstream_model}」加入故障转移组「${target}」？\n\n上游模型名保持不变。`)) updateRoute.mutate({ id: item.id, patch: { public_name: target } })
                    }}
                    className="h-8 max-w-40 rounded-md border border-input bg-transparent px-2 text-xs"
                    title="加入其他故障转移组"
                  >
                    <option value="">移入其他组…</option>
                    {modelNames.filter((model) => model !== name).map((model) => <option key={model} value={model}>{model}</option>)}
                  </select>
                  <Button variant={item.enabled ? "outline" : "ghost"} size="sm" onClick={() => updateRoute.mutate({ id: item.id, patch: { enabled: !item.enabled } })}>{item.enabled ? "路由已启用" : "路由已停用"}</Button>
                  <Button variant="ghost" size="icon" onClick={() => { if (confirm(`删除路由「${name} / ${item.upstream_model}」？`)) remove.mutate(item.id) }} aria-label="删除路由"><Trash2 className="text-destructive" /></Button>
                </div>
              </div>
            })}</div>
            <ModelAliasManager model={name} aliases={aliases} upstreamModels={list.map((item) => item.upstream_model)} />
          </CardContent></Card>
        })}</div>
      )}
      <RouteDialog open={routeOpen} onOpenChange={setRouteOpen} />
      <PricingDialog open={pricingOpen} onOpenChange={setPricingOpen} model={pricingModel} routes={selectedRoutes} />
      {(updateRoute.error || setStrategy.error) && <div className="fixed bottom-4 right-4 max-w-md rounded-lg border border-destructive/30 bg-background px-4 py-3 text-sm text-destructive shadow-lg">{(updateRoute.error ?? setStrategy.error)?.message}</div>}
    </motion.div>
  )
}
