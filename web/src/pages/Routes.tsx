import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Coins, Plus, RefreshCw, Search, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { PricingStatus, PricingSyncResult, Route } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { ModelAliasManager } from "@/components/ModelAliasManager"
import { PricingDialog } from "@/components/PricingDialog"
import { RouteDialog } from "@/components/RouteDialog"

const price = (micros: number) => `$${(micros / 1_000_000).toFixed(micros % 1_000_000 === 0 ? 0 : 3)}`
const pricingSource = (source?: string) => !source ? "未定价" : source === "manual" ? "手工" : source.includes("openrouter.ai") ? "OpenRouter" : "官网"

export function Routes() {
  const qc = useQueryClient()
  const [q, setQ] = useState("")
  const [pricingOpen, setPricingOpen] = useState(false)
  const [pricingModel, setPricingModel] = useState("")
  const [routeOpen, setRouteOpen] = useState(false)
  const { data: routes = [], isLoading } = useQuery({ queryKey: ["routes"], queryFn: () => api<Route[]>("/api/admin/routes") })
  const { data: pricing } = useQuery({ queryKey: ["pricing"], queryFn: () => api<PricingStatus>("/api/admin/pricing") })
  const syncPricing = useMutation({ mutationFn: () => api<PricingSyncResult>("/api/admin/pricing", { method: "POST" }), onSuccess: () => { qc.invalidateQueries({ queryKey: ["pricing"] }); qc.invalidateQueries({ queryKey: ["routes"] }) } })
  const toggle = useMutation({ mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) => api(`/api/admin/routes/${id}`, { method: "PATCH", body: JSON.stringify({ enabled }) }), onSuccess: () => qc.invalidateQueries({ queryKey: ["routes"] }) })
  const remove = useMutation({ mutationFn: (id: number) => api(`/api/admin/routes/${id}`, { method: "DELETE" }), onSuccess: () => { qc.invalidateQueries({ queryKey: ["routes"] }); qc.invalidateQueries({ queryKey: ["model-aliases"] }) } })
  const groups = useMemo(() => {
    const map = new Map<string, Route[]>()
    for (const route of routes) map.set(route.public_name, [...(map.get(route.public_name) ?? []), route])
    const keyword = q.trim().toLowerCase()
    return [...map.entries()].filter(([name, list]) => !keyword || name.toLowerCase().includes(keyword) || list.some((route) => route.upstream_model.toLowerCase().includes(keyword) || route.provider_name?.toLowerCase().includes(keyword)))
  }, [routes, q])
  const selectedRoutes = routes.filter((route) => route.public_name === pricingModel)
  const status = pricing?.status ?? {}

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
        <div><h1 className="text-2xl font-bold tracking-tight">模型路由</h1><p className="mt-1 text-sm text-muted-foreground">统一模型别名、价格、轮询与健康感知故障转移。</p></div>
        <div className="flex flex-wrap items-center gap-2"><div className="relative"><Search className="absolute left-2.5 top-1/2 -translate-y-1/2 text-muted-foreground" /><Input value={q} onChange={(event) => setQ(event.target.value)} placeholder="搜索模型、渠道" className="h-9 w-56 pl-8 text-xs" /></div><Button onClick={() => setRouteOpen(true)}><Plus />添加路由</Button></div>
      </div>

      <Card className="mb-4 overflow-hidden">
        <CardContent className="flex flex-col gap-3 p-4 lg:flex-row lg:items-center lg:justify-between">
          <div><div className="flex flex-wrap items-center gap-2"><div className="text-sm font-semibold">模型价格同步</div><Badge variant={status.pricing_last_error ? "warning" : "success"}>{pricing?.interval === "0s" ? "自动同步已停用" : status.pricing_last_sync ? "自动同步已启用" : "等待首次同步"}</Badge><span className="text-xs text-muted-foreground">周期 {pricing?.interval ?? "—"}</span></div><div className="mt-1 text-xs text-muted-foreground">优先官网，OpenRouter 兜底 · 上次 {status.pricing_last_sync ? new Date(status.pricing_last_sync).toLocaleString() : "尚未同步"} · {status.pricing_last_sources ?? 0} 个来源 / {status.pricing_last_models ?? 0} 个价格 / 更新 {status.pricing_last_updated_routes ?? 0} 条路由</div>{status.pricing_last_error && <div className="mt-1 max-w-4xl truncate text-xs text-amber-700 dark:text-amber-400">{status.pricing_last_error}</div>}{syncPricing.data?.errors?.length ? <div className="mt-1 text-xs text-amber-700 dark:text-amber-400">{syncPricing.data.errors.join("；")}</div> : null}</div>
          <Button variant="outline" onClick={() => syncPricing.mutate()} disabled={syncPricing.isPending}><RefreshCw className={syncPricing.isPending ? "animate-spin" : ""} />立即同步</Button>
        </CardContent>
      </Card>

      <ModelAliasManager routes={routes} />

      {isLoading ? <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div> : groups.length === 0 ? <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">没有符合条件的路由</div> : (
        <div className="space-y-4">{groups.map(([name, list]) => {
          const route = list[0]
          return <Card key={name}><CardContent className="p-0"><div className="border-b px-4 py-3"><div className="flex flex-wrap items-center justify-between gap-2"><div className="flex flex-wrap items-center gap-2"><span className="font-mono text-sm font-semibold">{name}</span><Badge variant="neutral">{list.length} 个上游</Badge>{route.input_price_micros || route.output_price_micros ? <Badge variant={route.pricing_source === "manual" ? "warning" : "success"}>输入 {price(route.input_price_micros)} · 输出 {price(route.output_price_micros)} · {pricingSource(route.pricing_source)}</Badge> : <Badge variant="neutral">未定价</Badge>}</div><Button variant="ghost" size="sm" onClick={() => { setPricingModel(name); setPricingOpen(true) }}><Coins />定价</Button></div></div><div className="divide-y">{list.map((item) => <div key={item.id} className="grid gap-3 px-4 py-3 hover:bg-muted/30 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-center"><div className="min-w-0"><div className="text-sm font-medium">{item.provider_name || "—"}</div><div className="truncate font-mono text-xs text-muted-foreground">{item.upstream_model}</div><div className="mt-1 flex flex-wrap gap-1"><Badge variant="outline">{item.provider_type}</Badge><Badge variant="outline">优先级 {item.priority}</Badge><Badge variant="outline">{item.capabilities}</Badge></div></div><div className="flex items-center gap-2 text-xs text-muted-foreground">{item.health_check_status === "healthy" ? <Badge variant="success">健康</Badge> : item.health_check_status === "unhealthy" ? <Badge variant="danger">不健康</Badge> : <Badge variant="neutral">待检测</Badge>}<span>{item.provider_latency_ms} ms</span></div><div className="flex items-center justify-end gap-1"><Button variant={item.enabled ? "outline" : "ghost"} size="sm" onClick={() => toggle.mutate({ id: item.id, enabled: !item.enabled })}>{item.enabled ? "已启用" : "已停用"}</Button><Button variant="ghost" size="icon" onClick={() => { if (confirm(`删除路由「${name} / ${item.upstream_model}」？`)) remove.mutate(item.id) }} aria-label="删除路由"><Trash2 className="text-destructive" /></Button></div></div>)}</div></CardContent></Card>
        })}</div>
      )}
      <RouteDialog open={routeOpen} onOpenChange={setRouteOpen} />
      <PricingDialog open={pricingOpen} onOpenChange={setPricingOpen} model={pricingModel} routes={selectedRoutes} />
    </motion.div>
  )
}
