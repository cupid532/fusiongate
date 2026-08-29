import { useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Coins, Boxes, KeyRound, Server, Activity, BarChart3, Flame, RefreshCw } from "lucide-react"
import { api } from "@/lib/api"
import type { APIKey, Provider, TokenUsageHeatmapCell, TokenUsageResponse } from "@/lib/types"
import { cn, formatCost, formatTokens } from "@/lib/utils"
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { SegmentedTabs } from "@/components/ui/segmented-tabs"
import { StatCard } from "@/components/ui/stat-card"
import { EmptyState } from "@/components/ui/empty-state"
import { Heatmap } from "@/components/ui/heatmap"

const ranges = [
  { d: 1, label: "1 天" },
  { d: 7, label: "7 天" },
  { d: 30, label: "30 天" },
  { d: 90, label: "90 天" },
  { d: 365, label: "一年" },
]

type Tab = "overview" | "trends" | "models" | "keys" | "providers" | "heatmap"

type HeatmapMetric = "total_tokens" | "input_tokens" | "output_tokens" | "cached_tokens" | "cost_micros"

const inputStyles = "h-9 rounded-md border border-input bg-transparent px-2 text-sm"

export function Usage() {
  const [days, setDays] = useState(30)
  const [apiKeyId, setApiKeyId] = useState("")
  const [providerId, setProviderId] = useState("")
  const [model, setModel] = useState("")
  const [tab, setTab] = useState<Tab>("overview")
  const [heatmapMetric, setHeatmapMetric] = useState<HeatmapMetric>("total_tokens")

  const { data: providers = [] } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })
  const { data: keys = [] } = useQuery({
    queryKey: ["keys"],
    queryFn: () => api<APIKey[]>("/api/admin/keys"),
  })

  const params = useMemo(() => {
    const p = new URLSearchParams({ days: String(days) })
    if (apiKeyId) p.set("api_key_id", apiKeyId)
    if (providerId) p.set("provider_id", providerId)
    if (model.trim()) p.set("model", model.trim())
    if (tab === "heatmap") p.set("heatmap", "1")
    return p.toString()
  }, [days, apiKeyId, providerId, model, tab])

  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ["usage", days, apiKeyId, providerId, model, tab === "heatmap"],
    queryFn: () => api<TokenUsageResponse>(`/api/admin/token-usage?${params}`),
  })

  const maxTokens = useMemo(() => {
    if (!data) return 0
    return Math.max(1, ...data.series.map((s) => s.total_tokens))
  }, [data])

  // Heatmap grid: rows(top models) x cols(dates). Each cell keeps the full
  // ledger aggregate so the tooltip can show the input/cached/output split
  // with the real cost, and the metric switcher can re-color the grid.
  const heatmap = useMemo(() => {
    const cells = data?.heatmap ?? []
    if (!cells.length) return null
    const modelOrder: string[] = []
    const byModel = new Map<string, Map<string, TokenUsageHeatmapCell>>()
    // maintain top-model order (backend already returns top first)
    for (const c of cells) {
      if (!byModel.has(c.model)) {
        byModel.set(c.model, new Map())
        modelOrder.push(c.model)
      }
      byModel.get(c.model)!.set(c.date, c)
    }
    const dates = [...new Set(cells.map((c) => c.date))].sort()
    const colLabels = dates.map((d) => d.slice(8)) // MM-DD -> DD
    const grid = modelOrder.map((model) => dates.map((date) => byModel.get(model)!.get(date) ?? null))
    return { grid, rowLabels: modelOrder, colLabels, dates }
  }, [data?.heatmap])

  const heatmapMatrix = useMemo(() => {
    if (!heatmap) return []
    return heatmap.grid.map((row) => row.map((c) => (c ? c[heatmapMetric] : 0)))
  }, [heatmap, heatmapMetric])

  const stats = [
    { label: "估算费用", value: formatCost(data?.totals.cost_micros ?? 0), tone: "text-amber-600", icon: <Coins className="h-4 w-4" /> },
    { label: "总 Token", value: formatTokens(data?.totals.total_tokens ?? 0), tone: "text-primary", icon: <Boxes className="h-4 w-4" /> },
    { label: "输入 Token", value: formatTokens(data?.totals.input_tokens ?? 0), tone: "text-blue-600", icon: <BarChart3 className="h-4 w-4" /> },
    { label: "输出 Token", value: formatTokens(data?.totals.output_tokens ?? 0), tone: "text-orange-600", icon: <Activity className="h-4 w-4" /> },
    { label: "请求数", value: String(data?.totals.requests ?? 0), tone: "text-foreground", icon: <Server className="h-4 w-4" /> },
    { label: "采集率", value: `${(data?.totals.usage_coverage ?? 0).toFixed(1)}%`, tone: "text-primary", icon: <Flame className="h-4 w-4" /> },
  ]

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-5 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">用量与费用</h1>
          <p className="mt-1 text-sm text-muted-foreground">按时间、模型、密钥与渠道多维分析 Token 用量与估算费用。</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <select aria-label="按渠道筛选用量" value={providerId} onChange={(e) => setProviderId(e.target.value)} className={inputStyles}>
            <option value="">全部渠道</option>
            {providers.map((p) => (<option key={p.id} value={p.id}>{p.name}</option>))}
          </select>
          <select aria-label="按访问密钥筛选用量" value={apiKeyId} onChange={(e) => setApiKeyId(e.target.value)} className={inputStyles}>
            <option value="">全部 Key</option>
            {keys.map((k) => (<option key={k.id} value={k.id}>{k.name}</option>))}
          </select>
          <input aria-label="按模型筛选用量" value={model} onChange={(e) => setModel(e.target.value)} placeholder="模型名（可选）" className={cn(inputStyles, "px-3")} />
          <div className="flex gap-1.5">
            {ranges.map((r) => (
              <button key={r.d} onClick={() => setDays(r.d)} className={cn("rounded-lg border px-3 py-1.5 text-xs font-medium transition-colors", days === r.d ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground")}>
                {r.label}
              </button>
            ))}
          </div>
          <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching} aria-label="刷新">
            <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
          </Button>
        </div>
      </div>

      <SegmentedTabs<Tab>
        className="mb-5"
        value={tab}
        onChange={setTab}
        tabs={[
          { value: "overview", label: "总览" },
          { value: "trends", label: "趋势" },
          { value: "models", label: "模型", count: data?.by_models.length },
          { value: "keys", label: "密钥", count: data?.by_keys.length },
          { value: "providers", label: "渠道", count: data?.by_providers.length },
          { value: "heatmap", label: "热力图" },
        ]}
      />

      {isLoading || !data ? (
        <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
      ) : (
        <div className="space-y-5">
          {/* ===== 总览 ===== */}
          {tab === "overview" && (
            <>
              <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
                {stats.map((s) => <StatCard key={s.label} {...s} />)}
              </div>
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">每日 Token 趋势</CardTitle>
                  <CardDescription>按日聚合的输入 / 缓存 / 输出 Token 堆叠。</CardDescription>
                </CardHeader>
                <CardContent>
                  <div className="flex h-48 items-end gap-1">
                    {data.series.map((s, i) => {
                      const total = Math.max(1, s.input_tokens + s.output_tokens)
                      const h = Math.max(2, (total / maxTokens) * 100)
                      const uncachedInput = Math.max(0, s.input_tokens - s.cached_tokens)
                      return (
                        <div key={i} className="flex h-full flex-1 flex-col justify-end gap-px" title={`${s.date}\n输入 ${formatTokens(s.input_tokens)} · 缓存 ${formatTokens(s.cached_tokens)} · 输出 ${formatTokens(s.output_tokens)}`}>
                          <motion.div className="w-full bg-orange-400" initial={{ height: 0 }} animate={{ height: `${(s.output_tokens / total) * h}%` }} transition={{ duration: 0.5, delay: i * 0.01, ease: [0.16, 1, 0.3, 1] }} />
                          <motion.div className="w-full bg-violet-400" initial={{ height: 0 }} animate={{ height: `${(s.cached_tokens / total) * h}%` }} transition={{ duration: 0.5, delay: i * 0.01, ease: [0.16, 1, 0.3, 1] }} />
                          <motion.div className="w-full bg-blue-400" initial={{ height: 0 }} animate={{ height: `${(uncachedInput / total) * h}%` }} transition={{ duration: 0.5, delay: i * 0.01, ease: [0.16, 1, 0.3, 1] }} />
                        </div>
                      )
                    })}
                  </div>
                  <div className="mt-2 flex justify-between text-[10px] text-muted-foreground">
                    <span>{data.series[0]?.date?.slice(0, 10)}</span>
                    {data.period && <span className="text-muted-foreground">采集 {data.period.timezone}</span>}
                    <span>{data.series[data.series.length - 1]?.date?.slice(0, 10)}</span>
                  </div>
                </CardContent>
              </Card>
            </>
          )}

          {/* ===== 趋势 ===== */}
          {tab === "trends" && (
            <Card>
              <CardHeader><CardTitle className="text-base">趋势分析</CardTitle><CardDescription>输入 / 输出 / 缓存 Token 逐日变化。</CardDescription></CardHeader>
              <CardContent className="space-y-6">
                {[
                  { key: "input_tokens" as const, label: "输入", color: "from-blue-500 to-blue-400" },
                  { key: "output_tokens" as const, label: "输出", color: "from-orange-500 to-orange-400" },
                  { key: "cached_tokens" as const, label: "缓存", color: "from-violet-500 to-violet-400" },
                ].map((m) => {
                  const max = Math.max(1, ...data.series.map((s) => s[m.key]))
                  return (
                    <div key={m.key}>
                      <div className="mb-2 flex items-center justify-between text-sm"><span className="font-medium">{m.label}</span><span className="text-xs text-muted-foreground">{formatTokens(data.totals[m.key])}</span></div>
                      <div className="flex h-20 items-end gap-1">
                        {data.series.map((s, i) => (<motion.div key={i} className={cn("flex-1 rounded-t bg-gradient-to-t", m.color)} initial={{ height: 0 }} animate={{ height: `${Math.max(2, (s[m.key] / max) * 100)}%` }} transition={{ duration: 0.4, delay: i * 0.01 }} title={`${s.date}: ${formatTokens(s[m.key])}`} />))}
                      </div>
                    </div>
                  )
                })}
              </CardContent>
            </Card>
          )}

          {/* ===== 模型 / 密钥 / 渠道 ===== */}
          {tab !== "overview" && tab !== "trends" && tab !== "heatmap" && (
            <RankPanel
              tab={tab}
              data={data}
              onSelectModel={(m) => { setModel(m); setTab("overview") }}
            />
          )}

          {/* ===== 热力图 ===== */}
          {tab === "heatmap" && (
            <Card>
              <CardHeader>
                <CardTitle className="text-base">用量热力图</CardTitle>
                <CardDescription>Top 模型 × 日期，可按总量 / 输入 / 缓存 / 输出 / 费用着色；悬停查看明细。</CardDescription>
              </CardHeader>
              <CardContent>
                {!heatmap ? (
                  <EmptyState title="暂无热力图数据" description="时间范围内没有足够的用量记录来绘制热力图。" />
                ) : (
                  <>
                    <SegmentedTabs<HeatmapMetric>
                      className="mb-3"
                      value={heatmapMetric}
                      onChange={setHeatmapMetric}
                      tabs={[
                        { value: "total_tokens", label: "总量" },
                        { value: "input_tokens", label: "输入" },
                        { value: "cached_tokens", label: "缓存" },
                        { value: "output_tokens", label: "输出" },
                        { value: "cost_micros", label: "费用" },
                      ]}
                    />
                    <Heatmap
                      matrix={heatmapMatrix}
                      rowLabels={heatmap.rowLabels}
                      colLabels={heatmap.colLabels}
                      formatTooltip={(row, col, _v) => {
                        const cell = heatmap.grid[heatmap.rowLabels.indexOf(row)]?.[heatmap.colLabels.indexOf(col)]
                        if (!cell) return `${row} · ${col}\n无数据`
                        const up = cell.upstream_model
                        return `${row}${up && up !== row ? ` (${up})` : ""} · ${col}\n${cell.requests} 请求\n输入 ${formatTokens(cell.input_tokens)} · 缓存 ${formatTokens(cell.cached_tokens)} · 输出 ${formatTokens(cell.output_tokens)}\n费用 ${formatCost(cell.cost_micros)}`
                      }}
                      formatCell={(v) => (heatmapMetric === "cost_micros" ? formatCost(v) : v >= 1_000_000 ? `${(v / 1_000_000).toFixed(1)}M` : v >= 1000 ? `${(v / 1000).toFixed(0)}K` : String(v))}
                    />
                  </>
                )}
              </CardContent>
            </Card>
          )}
        </div>
      )}
    </motion.div>
  )
}

function RankPanel({ tab, data, onSelectModel }: { tab: "models" | "keys" | "providers"; data: TokenUsageResponse; onSelectModel: (m: string) => void }) {
  type RankItem = (typeof data.by_models)[number] | (typeof data.by_keys)[number] | (typeof data.by_providers)[number]
  const config: Record<string, { title: string; desc: string; icon: React.ReactNode; list: RankItem[]; onSelect?: (item: RankItem) => void }> = {
    models: {
      title: "模型分析", desc: "按公开模型聚合的用量与费用。", icon: <Boxes className="h-4 w-4" />,
      list: data.by_models, onSelect: (it) => onSelectModel(it.name),
    },
    keys: { title: "客户端 Key 分析", desc: "按下游 API Key 聚合。", icon: <KeyRound className="h-4 w-4" />, list: data.by_keys },
    providers: { title: "渠道分析", desc: "按上游 Provider 聚合。", icon: <Server className="h-4 w-4" />, list: data.by_providers },
  }
  const c = config[tab]
  const list = c.list
  const maxTokens = Math.max(1, ...list.map((it) => it.total_tokens))

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2"><span className="text-muted-foreground">{c.icon}</span><CardTitle className="text-base">{c.title}</CardTitle></div>
        <CardDescription>{c.desc}</CardDescription>
      </CardHeader>
      <CardContent>
        {list.length === 0 ? (
          <EmptyState title="暂无数据" description="当前时间范围内没有匹配的用量记录。" />
        ) : (
          <div className="space-y-2">
            {list.map((it, i) => {
              const pct = (it.total_tokens / maxTokens) * 100
              const name = it.name || (tab === "keys" ? "已删除 Key" : "已删除主体") || "—"
              return (
                <button
                  key={i}
                  onClick={() => c.onSelect?.(it)}
                  className={cn("block w-full rounded-lg border p-3 text-left transition-colors", c.onSelect && "hover:border-primary/50 hover:bg-muted/30")}
                >
                  <div className="flex items-center justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2 text-sm font-medium">
                        <span className="text-[10px] text-muted-foreground tabular-nums">#{i + 1}</span>
                        <span className="truncate">{name}</span>
                        {it.name && "upstream_model" in it && it.upstream_model && it.upstream_model !== it.name ? <span className="truncate font-mono text-[10px] text-muted-foreground">{it.upstream_model}</span> : null}
                      </div>
                      <div className="mt-1 flex flex-wrap gap-3 text-[11px] text-muted-foreground">
                        <span>输入 {formatTokens(it.input_tokens)}</span>
                        <span>缓存 {formatTokens(it.cached_tokens)}</span>
                        <span>输出 {formatTokens(it.output_tokens)}</span>
                        <span>{it.requests} 请求</span>
                        <span>{formatCost(it.cost_micros)}</span>
                        <span>失败 {it.requests - it.successful_requests}</span>
                      </div>
                    </div>
                    <div className="shrink-0 text-right">
                      <div className="text-sm font-semibold tabular-nums">{pct.toFixed(0)}%</div>
                      <div className="text-[10px] text-muted-foreground">占比</div>
                    </div>
                  </div>
                  <div className="mt-2 h-1.5 rounded-full bg-muted">
                    <div className="h-1.5 rounded-full bg-gradient-to-r from-primary/60 to-primary" style={{ width: `${pct}%` }} />
                  </div>
                </button>
              )
            })}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
