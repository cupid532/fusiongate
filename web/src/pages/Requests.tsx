import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Search, Trash2, Download, HardDrive, Save } from "lucide-react"
import { api, apiDownload, saveBlob } from "@/lib/api"
import { notifySuccess } from "@/lib/notify"
import type { Provider, RequestLedgerPayload, LedgerStatus } from "@/lib/types"
import { cn, formatCost, formatTokens } from "@/lib/utils"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { SegmentedTabs } from "@/components/ui/segmented-tabs"
import { useConfirm } from "@/components/ui/confirm"
import { QueryError } from "@/components/ui/query-error"
import { useDebounced } from "@/lib/use-debounced"

type StatusFilter = "all" | "running" | "success" | "failed"

const timeRanges = [
  { v: "", label: "全部" },
  { v: "1h", label: "最近 1 小时" },
  { v: "24h", label: "最近 24 小时" },
  { v: "7d", label: "最近 7 天" },
]

function timeAgo(iso: string) {
  const t = new Date(iso).getTime()
  const diff = Date.now() - t
  if (diff < 60_000) return "刚刚"
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  return new Date(iso).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

function duration(ms: number | null) {
  if (ms == null) return "—"
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

function clockPad(n: number) {
  return String(n).padStart(2, "0")
}

// Reasoning effort is shown with the upstream's own identifiers — minimal,
// low, medium, high, xhigh, max — deliberately untranslated. These are the
// exact strings you pass in an API request and read in provider docs, so a
// Chinese rendering ("极高"?) would be a second vocabulary to map back.
// Only the colour is interpretive: higher effort costs more, so it gets more
// visual weight.
const EFFORT_TONE: Record<string, string> = {
  none: "text-muted-foreground",
  minimal: "text-muted-foreground",
  low: "text-muted-foreground",
  medium: "text-foreground",
  high: "text-amber-500",
  xhigh: "text-destructive",
  max: "text-destructive",
}

function ReasoningEffort({ value }: { value?: string }) {
  const effort = (value ?? "").trim()
  // Most rows carry no effort at all (non-reasoning models, or an upstream
  // that does not report it) — 15k of ~18.6k rows on this deployment. A dash
  // keeps the column quiet rather than filling it with "—" styled as data.
  if (!effort) return <span className="text-muted-foreground/40">—</span>
  return (
    <span
      className={cn("font-mono text-[11px] font-medium", EFFORT_TONE[effort.toLowerCase()] ?? "text-foreground")}
      title={`reasoning effort: ${effort}`}
    >
      {effort}
    </span>
  )
}

// LiveClock renders a self-ticking elapsed timer for a running ledger row.
// Both timestamps are absolute millisecond values; subtracting creation
// time keeps the badge focused on elapsed duration rather than a Unix timestamp.
function LiveClock({ startIso, phase, stale }: { startIso: string; phase: "first" | "stream"; stale: boolean }) {
  const [tick, setTick] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setTick(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [])
  const elapsed = Math.max(0, Math.floor((tick - new Date(startIso).getTime()) / 1000))
  const label = `${clockPad(Math.floor(elapsed / 60))}:${clockPad(elapsed % 60)}`
  if (stale) return <Badge variant="danger">疑似停滞 {label}</Badge>
  if (phase === "first") return <Badge variant="warning">等待首字节 {label}</Badge>
  return (
    <Badge variant="warning">
      输出中 <span className="tabular-nums">{label}</span>
    </Badge>
  )
}

function cacheRate(cached: number, input: number): number {
  return input > 0 ? (cached / input) * 100 : 0
}

function CacheRateBadge({ cached, input }: { cached: number; input: number }) {
  if (input <= 0) return <span className="text-muted-foreground/40">—</span>
  const rate = cacheRate(cached, input)
  return (
    <span className={cn("tabular-nums text-[11px] font-medium", rate >= 50 ? "text-emerald-600" : rate > 0 ? "text-amber-500" : "text-muted-foreground")}>
      {rate.toFixed(1)}%
    </span>
  )
}

const PAGE_LIMIT = 100

export function Requests() {
  const confirm = useConfirm()
  const [status, setStatus] = useState<StatusFilter>("all")
  const [q, setQ] = useState("")
  // The query keys off the debounced value; the input stays fully responsive.
  const debouncedQ = useDebounced(q, 350)
  const [providerId, setProviderId] = useState("")
  const [range, setRange] = useState("")
  const [view, setView] = useState<"list" | "group" | "cache">("list")

  const { data: providers = [] } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })

  const qc = useQueryClient()
  const [capDraft, setCapDraft] = useState("")
  const { data: ledger } = useQuery({
    queryKey: ["ledger"],
    queryFn: () => api<LedgerStatus>("/api/admin/ledger"),
    staleTime: 10_000,
  })
  const updateCap = useMutation({
    mutationFn: (maxMb: number) => api<LedgerStatus>("/api/admin/ledger", { method: "PUT", body: JSON.stringify({ max_mb: maxMb }) }),
    onSuccess: (r) => {
      qc.setQueryData(["ledger"], r)
      qc.invalidateQueries({ queryKey: ["requests"] })
      qc.invalidateQueries({ queryKey: ["ledger"] })
    },
  })
  const clearLedger = useMutation({
    mutationFn: () => api<{ ok: boolean }>("/api/admin/ledger/clear", { method: "POST" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["requests"] })
      qc.invalidateQueries({ queryKey: ["ledger"] })
      qc.invalidateQueries({ queryKey: ["usage"] })
      qc.invalidateQueries({ queryKey: ["dashboard"] })
    },
  })

  let maxMb = ledger?.max_mb ?? 100
  if (typeof capDraft === "string" && capDraft.trim() !== "") {
    const parsed = Number(capDraft)
    if (Number.isFinite(parsed) && parsed > 0) maxMb = parsed
  }

  const exportLedger = useMutation({
    mutationFn: async () => {
      // Export honors the time-range and status filters (the same ones the list uses).
      const params = new URLSearchParams()
      if (status !== "all") params.set("status", status)
      if (range) {
        const now = new Date()
        const ms: Record<string, number> = { "1h": 3600_000, "24h": 86400_000, "7d": 604800_000 }
        if (ms[range]) params.set("from", new Date(now.getTime() - ms[range]).toISOString())
      }
      const qs = params.toString()
      // apiDownload checks res.ok. The previous version did not, and swallowed
      // every failure in an empty catch — so an expired session wrote the 401
      // error body to disk as fusiongate-requests-<date>.json and looked like
      // a successful export.
      const blob = await apiDownload(`/api/admin/ledger/export${qs ? `?${qs}` : ""}`)
      saveBlob(blob, `fusiongate-requests-${new Date().toISOString().slice(0, 10)}.csv`)
    },
    onSuccess: () => notifySuccess("账本已导出"),
  })

  const [pages, setPages] = useState<RequestLedgerPayload[]>([])
  const [loadingMore, setLoadingMore] = useState(false)

  // Reset accumulated pages when any filter changes.
  useEffect(() => { setPages([]) }, [status, debouncedQ, providerId, range])

  const baseParams = useMemo(() => {
    const p = new URLSearchParams({ limit: String(PAGE_LIMIT) })
    if (status !== "all") p.set("status", status)
    if (debouncedQ.trim()) p.set("q", debouncedQ.trim())
    if (providerId) p.set("provider_id", providerId)
    if (range) {
      const now = new Date()
      const ms: Record<string, number> = { "1h": 3600_000, "24h": 86400_000, "7d": 604800_000 }
      if (ms[range]) p.set("from", new Date(now.getTime() - ms[range]).toISOString())
    }
    return p
  }, [status, debouncedQ, providerId, range])

  const requestsQuery = useQuery({
    queryKey: ["requests", status, debouncedQ, providerId, range],
    queryFn: () => api<RequestLedgerPayload>(`/api/admin/requests?${baseParams.toString()}`),
    refetchInterval: 5000,
  })

  const firstPage = requestsQuery.data
  const rows = useMemo(() => {
    const first = firstPage?.items ?? []
    const rest = pages.flatMap((p) => p.items)
    return [...first, ...rest]
  }, [firstPage, pages])
  const isLoading = requestsQuery.isLoading
  const isFetching = requestsQuery.isFetching
  const refetch = () => { setPages([]); return requestsQuery.refetch() }
  const totalRows = firstPage?.total ?? rows.length
  const hasMore = rows.length < totalRows

  async function loadMore() {
    const lastId = rows[rows.length - 1]?.id
    if (!lastId || loadingMore) return
    setLoadingMore(true)
    try {
      const p = new URLSearchParams(baseParams)
      p.set("before", String(lastId))
      const page = await api<RequestLedgerPayload>(`/api/admin/requests?${p.toString()}`)
      setPages((prev) => [...prev, page])
    } finally {
      setLoadingMore(false)
    }
  }

  const groupByModel = useMemo(() => {
    const map = new Map<string, { model: string; count: number; success: number; failed: number; tokens: number; input_tokens: number; cached_tokens: number; cost_micros: number; avg_latency: number }>()
    for (const r of rows) {
      const g = map.get(r.model) ?? { model: r.model, count: 0, success: 0, failed: 0, tokens: 0, input_tokens: 0, cached_tokens: 0, cost_micros: 0, avg_latency: 0 }
      g.count++
      if (r.success) g.success++
      else if (!r.running) g.failed++
      g.tokens += r.total_tokens
      g.input_tokens += r.input_tokens
      g.cached_tokens += r.cached_tokens
      g.cost_micros += r.cost_micros
      if (r.latency_ms) g.avg_latency += r.latency_ms
      map.set(r.model, g)
    }
    const list = [...map.values()]
      .map((g) => ({ ...g, avg_latency: g.count ? Math.round(g.avg_latency / g.count) : 0 }))
      .sort((a, b) => b.tokens - a.tokens)
    return { list, maxTokens: Math.max(1, ...list.map((g) => g.tokens)) }
  }, [rows])

  const groupByChannel = useMemo(() => {
    type ChannelGroup = { channel: string; keys: Map<string, { keyLabel: string; count: number; input_tokens: number; cached_tokens: number; tokens: number; cost_micros: number }> ; count: number; input_tokens: number; cached_tokens: number; tokens: number; cost_micros: number }
    const map = new Map<string, ChannelGroup>()
    for (const r of rows) {
      const ch = r.provider_name || "未知渠道"
      const g = map.get(ch) ?? { channel: ch, keys: new Map(), count: 0, input_tokens: 0, cached_tokens: 0, tokens: 0, cost_micros: 0 }
      g.count++
      g.input_tokens += r.input_tokens
      g.cached_tokens += r.cached_tokens
      g.tokens += r.total_tokens
      g.cost_micros += r.cost_micros
      const keyLabel = [r.provider_key_name, r.provider_key_hint].filter(Boolean).join(" · ") || "默认 Key"
      const k = g.keys.get(keyLabel) ?? { keyLabel, count: 0, input_tokens: 0, cached_tokens: 0, tokens: 0, cost_micros: 0 }
      k.count++
      k.input_tokens += r.input_tokens
      k.cached_tokens += r.cached_tokens
      k.tokens += r.total_tokens
      k.cost_micros += r.cost_micros
      g.keys.set(keyLabel, k)
      map.set(ch, g)
    }
    return [...map.values()].sort((a, b) => b.tokens - a.tokens)
  }, [rows])

  // Comes straight from the server's aggregate over the whole filtered range.
  // Summing the returned page here instead — which is what this did before —
  // reported the newest 100 rows under a "当前范围" label.
  const summary = useMemo(() => {
    const totals = requestsQuery.data?.totals
    if (!totals) return { count: 0, ok: 0, failed: 0, tokens: 0, cost: 0, cacheRate: 0 }
    const inputTokens = totals.input_tokens ?? 0
    const cachedTokens = totals.cached_tokens ?? 0
    return {
      count: totals.requests,
      ok: totals.success,
      failed: totals.failed,
      tokens: totals.total_tokens,
      cost: totals.cost_micros,
      cacheRate: inputTokens > 0 ? (cachedTokens / inputTokens) * 100 : 0,
    }
  }, [requestsQuery.data?.totals])

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">请求账本</h1>
          <p className="mt-1 text-sm text-muted-foreground">观察每一次请求的状态、耗时与 Token 用量。</p>
        </div>
        <Button variant="outline" onClick={() => void refetch()}>
          <RefreshCw className={cn("h-4 w-4", isFetching && "animate-spin")} />
          刷新
        </Button>
      </div>

      <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-5">
        {[
          { label: "当前范围请求", value: summary.count.toLocaleString(), tone: "text-foreground" },
          { label: "成功", value: summary.ok.toLocaleString(), tone: "text-emerald-600" },
          { label: "失败", value: summary.failed.toLocaleString(), tone: summary.failed ? "text-destructive" : "text-muted-foreground" },
          { label: "综合缓存率", value: `${summary.cacheRate.toFixed(1)}%`, tone: summary.cacheRate >= 50 ? "text-emerald-600" : summary.cacheRate > 0 ? "text-amber-500" : "text-muted-foreground" },
          { label: "总 Token · 费用", value: `${formatTokens(summary.tokens)} · ${formatCost(summary.cost)}`, tone: "text-primary" },
        ].map((s) => (
          <div key={s.label} className="rounded-xl border bg-card p-3">
            <div className="text-[11px] text-muted-foreground">{s.label}</div>
            <div className={cn("mt-1 text-lg font-bold tabular-nums", s.tone)}>{s.value}</div>
          </div>
        ))}
      </div>

      <Card className="mb-4">
        <CardContent className="p-4">
          <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <div className="flex min-w-0 flex-1 items-center gap-4">
              <HardDrive className="h-4 w-4 shrink-0 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <div className="flex items-center justify-between gap-2 text-xs">
                  <span className="text-muted-foreground">请求容量</span>
                  <span className="tabular-nums text-muted-foreground">
                    当前 {ledger?.used_mb ?? "—"} MB / 上限 {maxMb} MB
                    {ledger?.rows != null && <span className="ml-2">· {ledger.rows.toLocaleString()} 行</span>}
                  </span>
                </div>
                <div className="mt-1.5 flex h-2 w-full overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn("h-full rounded-full transition-all", ledger?.capped ? "bg-destructive" : "bg-gradient-to-r from-primary/60 to-primary")}
                    style={{ width: `${Math.min(100, ((ledger?.used_mb ?? 0) / Math.max(1, maxMb)) * 100)}%` }}
                  />
                </div>
                {ledger?.capped && <p className="mt-1 text-xs text-destructive">已超出容量上限，后台会自动裁剪最旧记录。</p>}
              </div>
              <div className="flex shrink-0 items-center gap-1.5">
                <Input
                  aria-label="请求账本容量上限（MB）"
                  type="number"
                  min={1}
                  max={10240}
                  value={capDraft}
                  onChange={(e) => setCapDraft(e.target.value)}
                  placeholder={String(ledger?.max_mb ?? 100)}
                  className="h-8 w-20 px-2 text-xs tabular-nums"
                />
                <span className="text-xs text-muted-foreground">MB</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={updateCap.isPending || !(Number(capDraft) > 0 && Number(capDraft) <= 10240)}
                  onClick={() => updateCap.mutate(Number(capDraft))}
                >
                  <Save className="h-3.5 w-3.5" />
                  保存上限
                </Button>
              </div>
            </div>
            <div className="flex shrink-0 items-center gap-2 border-t pt-3 lg:border-l lg:border-t-0 lg:pl-4 lg:pt-0">
              {/* `ledger` is undefined while loading, and `undefined === 0` is
                  false — so these used to be clickable before the row count had
                  even arrived. Gate on the loaded value instead. */}
              <Button
                variant="outline"
                size="sm"
                onClick={() => exportLedger.mutate()}
                disabled={exportLedger.isPending || !ledger || ledger.rows === 0}
              >
                <Download className="h-3.5 w-3.5" />
                {exportLedger.isPending ? "导出中…" : "导出"}
              </Button>
              <Button
                variant="destructive"
                size="sm"
                disabled={clearLedger.isPending || !ledger || ledger.rows === 0}
                onClick={async () => {
                  // Type-to-confirm: this drops every row permanently, and the
                  // ledger routinely holds tens of thousands of them. A single
                  // native OK button was too easy to hit by reflex.
                  const ok = await confirm({
                    title: "清空整个请求账本？",
                    description: `将永久删除 ${(ledger?.rows ?? 0).toLocaleString()} 行请求记录（约 ${ledger?.used_mb ?? 0} MB），用量与费用统计会同时归零。此操作不可恢复，建议先导出备份。`,
                    destructive: true,
                    confirmLabel: "永久清空",
                    requireText: "清空账本",
                  })
                  if (ok) clearLedger.mutate()
                }}
              >
                <Trash2 className="h-3.5 w-3.5" />
                {clearLedger.isPending ? "清空中…" : "一键清除"}
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <div className="mb-3">
        <SegmentedTabs<"list" | "group" | "cache">
          value={view}
          onChange={setView}
          tabs={[
            { value: "list", label: "明细" },
            { value: "group", label: "按模型分组", count: groupByModel.list.length },
            { value: "cache", label: "缓存分析", count: groupByChannel.length },
          ]}
        />
      </div>

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center gap-2 border-b p-3">
            {(
              [
                ["all", "全部"],
                ["running", "进行中"],
                ["success", "成功"],
                ["failed", "失败"],
              ] as [StatusFilter, string][]
            ).map(([f, label]) => (
              <button
                key={f}
                onClick={() => setStatus(f)}
                className={cn(
                  "rounded-lg border px-3 py-1.5 text-xs font-medium transition-colors",
                  status === f ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                )}
              >
                {label}
              </button>
            ))}
            <div className="ml-auto flex w-full flex-wrap items-center gap-2 sm:w-auto sm:flex-nowrap">
              <div className="relative min-w-0 flex-1 sm:flex-none">
                <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input aria-label="搜索请求账本" value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索模型 / IP / 错误" className="h-8 w-full pl-8 text-xs sm:w-56" />
              </div>
              <select
                aria-label="按渠道筛选请求"
                value={providerId}
                onChange={(e) => setProviderId(e.target.value)}
                className="h-8 rounded-md border border-input bg-transparent px-2 text-xs"
              >
                <option value="">全部渠道</option>
                {providers.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
              <select
                aria-label="按时间范围筛选请求"
                value={range}
                onChange={(e) => setRange(e.target.value)}
                className="h-8 rounded-md border border-input bg-transparent px-2 text-xs"
              >
                {timeRanges.map((t) => (
                  <option key={t.v} value={t.v}>
                    {t.label}
                  </option>
                ))}
              </select>
            </div>
          </div>

          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : requestsQuery.isError ? (
            <QueryError
              title="无法加载请求账本"
              error={requestsQuery.error}
              onRetry={() => void refetch()}
              retrying={isFetching}
              className="m-4 border-none bg-transparent"
            />
          ) : rows.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">当前筛选范围还没有请求</div>
          ) : view === "group" ? (
            <div className="p-4">
              <div className="space-y-2">
                {groupByModel.list.map((g, i) => (
                  <div key={g.model} className="rounded-lg border p-3">
                    <div className="flex items-center justify-between gap-3">
                      <div className="flex min-w-0 items-center gap-2 text-sm font-medium">
                        <span className="text-[10px] text-muted-foreground tabular-nums">#{i + 1}</span>
                        <span className="truncate font-mono">{g.model}</span>
                        <span className="shrink-0 text-[10px] text-muted-foreground">{g.count} 请求</span>
                      </div>
                      <div className="shrink-0 text-sm font-semibold tabular-nums">{formatTokens(g.tokens)}</div>
                    </div>
                    <div className="mt-2 h-1.5 rounded-full bg-muted">
                      <div className="h-1.5 rounded-full bg-gradient-to-r from-primary/60 to-primary" style={{ width: `${(g.tokens / groupByModel.maxTokens) * 100}%` }} />
                    </div>
                    <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
                      <span>成功 <span className="font-semibold text-emerald-600">{g.success}</span></span>
                      <span>失败 <span className={cn("font-semibold", g.failed ? "text-destructive" : "")}>{g.failed}</span></span>
                      <span>均延迟 {duration(g.avg_latency)}</span>
                      <span>缓存率 <span className={cn("font-semibold", cacheRate(g.cached_tokens, g.input_tokens) >= 50 ? "text-emerald-600" : cacheRate(g.cached_tokens, g.input_tokens) > 0 ? "text-amber-500" : "")}>{cacheRate(g.cached_tokens, g.input_tokens).toFixed(1)}%</span></span>
                      <span>费用 {formatCost(g.cost_micros)}</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          ) : view === "cache" ? (
            <div className="p-4">
              <div className="space-y-3">
                {groupByChannel.map((ch) => {
                  const chRate = cacheRate(ch.cached_tokens, ch.input_tokens)
                  const keys = [...ch.keys.values()]
                  return (
                    <div key={ch.channel} className="rounded-lg border p-3">
                      <div className="flex items-center justify-between gap-3">
                        <div className="flex min-w-0 items-center gap-2 text-sm font-medium">
                          <span className="truncate">{ch.channel}</span>
                          <span className="shrink-0 text-[10px] text-muted-foreground">{ch.count} 请求</span>
                        </div>
                        <span className={cn("shrink-0 text-base font-bold tabular-nums", chRate >= 50 ? "text-emerald-600" : chRate > 0 ? "text-amber-500" : "text-muted-foreground")}>
                          {chRate.toFixed(1)}%
                        </span>
                      </div>
                      <div className="mt-2 h-1.5 rounded-full bg-muted">
                        <div className={cn("h-1.5 rounded-full", chRate >= 50 ? "bg-emerald-500" : chRate > 0 ? "bg-amber-500" : "bg-muted-foreground/30")} style={{ width: `${Math.max(chRate > 0 ? 2 : 0, chRate)}%` }} />
                      </div>
                      {keys.length > 1 && (
                        <div className="mt-2.5 space-y-1.5 border-t pt-2.5">
                          {keys.sort((a, b) => b.input_tokens - a.input_tokens).map((k) => {
                            const kRate = cacheRate(k.cached_tokens, k.input_tokens)
                            return (
                              <div key={k.keyLabel} className="flex items-center gap-3 text-xs">
                                <span className="min-w-0 flex-1 truncate text-muted-foreground" title={k.keyLabel}>{k.keyLabel}</span>
                                <span className="shrink-0 tabular-nums text-muted-foreground">{k.count} 请求</span>
                                <span className="w-16 shrink-0">
                                  <div className="h-1 rounded-full bg-muted">
                                    <div className={cn("h-1 rounded-full", kRate >= 50 ? "bg-emerald-500" : kRate > 0 ? "bg-amber-500" : "bg-muted-foreground/30")} style={{ width: `${Math.max(kRate > 0 ? 2 : 0, kRate)}%` }} />
                                  </div>
                                </span>
                                <span className={cn("w-14 shrink-0 text-right font-medium tabular-nums", kRate >= 50 ? "text-emerald-600" : kRate > 0 ? "text-amber-500" : "text-muted-foreground")}>
                                  {kRate.toFixed(1)}%
                                </span>
                              </div>
                            )
                          })}
                        </div>
                      )}
                      <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
                        <span>Token {formatTokens(ch.tokens)}</span>
                        <span>缓存 {formatTokens(ch.cached_tokens)}</span>
                        <span>费用 {formatCost(ch.cost_micros)}</span>
                      </div>
                    </div>
                  )
                })}
              </div>
            </div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">时间</th>
                    <th className="px-4 py-3 font-medium">模型</th>
                    <th className="px-4 py-3 font-medium">渠道</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">思考强度</th>
                    <th className="px-4 py-3 font-medium">首字节</th>
                    <th className="px-4 py-3 font-medium">Token</th>
                    <th className="px-4 py-3 font-medium">缓存率</th>
                    <th className="px-4 py-3 font-medium">费用</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={r.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3 text-xs text-muted-foreground">{timeAgo(r.created_at)}</td>
                      <td className="px-4 py-3">
                        <div className="font-medium">{r.model}</div>
                        <div className="text-xs text-muted-foreground">{r.protocol}</div>
                      </td>
                      <td className="px-4 py-3 text-xs">
                        <div>{r.provider_name || "—"}</div>
                        {(r.provider_key_name || r.provider_key_hint) && (
                          <div className="mt-0.5 text-[11px] text-muted-foreground">
                            {[r.provider_key_name, r.provider_key_hint].filter(Boolean).join(" · ")}
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        {r.running && r.first_byte_ms == null ? (
                          <LiveClock startIso={r.created_at} phase="first" stale={!!r.stale} />
                        ) : r.running ? (
                          <LiveClock startIso={r.created_at} phase="stream" stale={!!r.stale} />
                        ) : r.success ? (
                          <Badge variant="success">成功</Badge>
                        ) : (
                          <Badge variant="danger">失败</Badge>
                        )}
                      </td>
                      <td className="px-4 py-3"><ReasoningEffort value={r.reasoning_effort} /></td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{duration(r.first_byte_ms)}</td>
                      <td className="px-4 py-3 text-xs">
                          {(() => {
                            const parts: string[] = []
                            if (r.input_tokens > 0) parts.push(`入 ${formatTokens(r.input_tokens)}`)
                            if (r.cached_tokens > 0) parts.push(`缓 ${formatTokens(r.cached_tokens)}`)
                            if (r.reasoning_tokens > 0) parts.push(`思 ${formatTokens(r.reasoning_tokens)}`)
                            if (r.output_tokens > 0) parts.push(`出 ${formatTokens(r.output_tokens)}`)
                            if (parts.length) return parts.join(" · ")
                            if (r.running) return ""
                            return r.usage_reported ? "0" : "未采集"
                          })()}
                        </td>
                      <td className="px-4 py-3"><CacheRateBadge cached={r.cached_tokens} input={r.input_tokens} /></td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{formatCost(r.cost_micros)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {!isLoading && !requestsQuery.isError && rows.length > 0 && (
            <div className="flex flex-wrap items-center justify-between gap-2 border-t px-4 py-3 text-xs text-muted-foreground">
              <span>
                已加载 <span className="font-medium tabular-nums text-foreground">{rows.length}</span> 条
                {totalRows > rows.length && (
                  <>，当前筛选共 <span className="font-medium tabular-nums text-foreground">{totalRows.toLocaleString()}</span> 条</>
                )}
                {view === "group" && totalRows > rows.length && "（分组统计仅基于已加载的记录，上方汇总为全量）"}
              </span>
              {hasMore && (
                <Button variant="outline" size="sm" disabled={loadingMore} onClick={loadMore}>
                  {loadingMore ? "加载中…" : `加载更多（剩余 ${(totalRows - rows.length).toLocaleString()} 条）`}
                </Button>
              )}
            </div>
          )}
        </CardContent>
      </Card>
    </motion.div>
  )
}
