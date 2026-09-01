import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Search, Trash2, Download, HardDrive, Save } from "lucide-react"
import { api, getCsrfToken } from "@/lib/api"
import type { Provider, RequestLedgerPayload, LedgerStatus } from "@/lib/types"
import { cn, formatCost, formatTokens } from "@/lib/utils"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { SegmentedTabs } from "@/components/ui/segmented-tabs"

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

export function Requests() {
  const [status, setStatus] = useState<StatusFilter>("all")
  const [q, setQ] = useState("")
  const [providerId, setProviderId] = useState("")
  const [range, setRange] = useState("")
  const [view, setView] = useState<"list" | "group">("list")

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

  async function exportLedger() {
    // Export honors the time-range and status filters (the same ones the list uses).
    const q = new URLSearchParams()
    if (status !== "all") q.set("status", status)
    if (range) {
      const now = new Date()
      const ms: Record<string, number> = { "1h": 3600_000, "24h": 86400_000, "7d": 604800_000 }
      if (ms[range]) q.set("from", new Date(now.getTime() - ms[range]).toISOString())
    }
    const qs = q.toString()
    try {
      const res = await fetch(`/api/admin/ledger/export${qs ? `?${qs}` : ""}`, { headers: { "X-CSRF-Token": getCsrfToken() } })
      const blob = await res.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = `fusiongate-requests-${new Date().toISOString().slice(0, 10)}.json`
      a.click()
      URL.revokeObjectURL(url)
    } catch {
      /* ignore */
    }
  }

  const params = useMemo(() => {
    const p = new URLSearchParams({ limit: "100" })
    if (status !== "all") p.set("status", status)
    if (q.trim()) p.set("q", q.trim())
    if (providerId) p.set("provider_id", providerId)
    if (range) {
      const now = new Date()
      const ms: Record<string, number> = { "1h": 3600_000, "24h": 86400_000, "7d": 604800_000 }
      if (ms[range]) p.set("from", new Date(now.getTime() - ms[range]).toISOString())
    }
    return p.toString()
  }, [status, q, providerId, range])

  const requestsQuery = useQuery({
    queryKey: ["requests", status, q, providerId, range],
    queryFn: () => api<RequestLedgerPayload>(`/api/admin/requests?${params}`),
    refetchInterval: 5000,
  })
  const rows = useMemo(() => requestsQuery.data?.items ?? [], [requestsQuery.data?.items])
  const isLoading = requestsQuery.isLoading
  const isFetching = requestsQuery.isFetching
  const refetch = requestsQuery.refetch

  const groupByModel = useMemo(() => {
    const map = new Map<string, { model: string; count: number; success: number; failed: number; tokens: number; cost_micros: number; avg_latency: number }>()
    for (const r of rows) {
      const g = map.get(r.model) ?? { model: r.model, count: 0, success: 0, failed: 0, tokens: 0, cost_micros: 0, avg_latency: 0 }
      g.count++
      if (r.success) g.success++
      else if (!r.running) g.failed++
      g.tokens += r.total_tokens
      g.cost_micros += r.cost_micros
      if (r.latency_ms) g.avg_latency += r.latency_ms
      map.set(r.model, g)
    }
    const list = [...map.values()]
      .map((g) => ({ ...g, avg_latency: g.count ? Math.round(g.avg_latency / g.count) : 0 }))
      .sort((a, b) => b.tokens - a.tokens)
    return { list, maxTokens: Math.max(1, ...list.map((g) => g.tokens)) }
  }, [rows])

  const summary = useMemo(() => {
    let cost = 0, tokens = 0, failed = 0, ok = 0
    for (const r of rows) {
      cost += r.cost_micros
      tokens += r.total_tokens
      if (r.success) ok++
      else if (!r.running) failed++
    }
    return { count: rows.length, ok, failed, tokens, cost }
  }, [rows])

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

      <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
        {[
          { label: "当前范围请求", value: String(summary.count), tone: "text-foreground" },
          { label: "成功", value: String(summary.ok), tone: "text-emerald-600" },
          { label: "失败", value: String(summary.failed), tone: summary.failed ? "text-destructive" : "text-muted-foreground" },
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
              <Button variant="outline" size="sm" onClick={() => exportLedger()} disabled={ledger?.rows === 0}>
                <Download className="h-3.5 w-3.5" />
                导出
              </Button>
              <Button
                variant="destructive"
                size="sm"
                disabled={clearLedger.isPending || ledger?.rows === 0}
                onClick={() => {
                  if (window.confirm("确定清空整个请求账本？此操作不可恢复。")) clearLedger.mutate()
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
        <SegmentedTabs<"list" | "group">
          value={view}
          onChange={setView}
          tabs={[
            { value: "list", label: "明细" },
            { value: "group", label: "按模型分组", count: groupByModel.list.length },
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
                      <span>费用 {formatCost(g.cost_micros)}</span>
                    </div>
                  </div>
                ))}
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
                    <th className="px-4 py-3 font-medium">首字节</th>
                    <th className="px-4 py-3 font-medium">Token</th>
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
                      <td className="px-4 py-3 text-xs text-muted-foreground">{formatCost(r.cost_micros)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </motion.div>
  )
}
