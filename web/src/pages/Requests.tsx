import { useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Search } from "lucide-react"
import { api } from "@/lib/api"
import type { Provider, RequestLedgerRow } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"

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

export function Requests() {
  const [status, setStatus] = useState<StatusFilter>("all")
  const [q, setQ] = useState("")
  const [providerId, setProviderId] = useState("")
  const [range, setRange] = useState("")

  const { data: providers = [] } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })

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

  const { data: rows = [], isLoading, isFetching, refetch } = useQuery({
    queryKey: ["requests", status, q, providerId, range],
    queryFn: () => api<RequestLedgerRow[]>(`/api/admin/requests?${params}`),
    refetchInterval: 5000,
  })

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
                <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索模型 / IP / 错误" className="h-8 w-full pl-8 text-xs sm:w-56" />
              </div>
              <select
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
                      <td className="px-4 py-3 text-xs">{r.provider_name || "—"}</td>
                      <td className="px-4 py-3">
                        {r.running ? (
                          <Badge variant="warning">进行中</Badge>
                        ) : r.success ? (
                          <Badge variant="success">成功</Badge>
                        ) : (
                          <Badge variant="danger">失败</Badge>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{duration(r.first_byte_ms)}</td>
                      <td className="px-4 py-3 text-xs">{r.total_tokens}</td>
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
