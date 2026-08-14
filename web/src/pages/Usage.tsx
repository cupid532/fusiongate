import { useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { api } from "@/lib/api"
import type { APIKey, Provider, TokenUsageResponse } from "@/lib/types"
import { cn, formatCost, formatTokens } from "@/lib/utils"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"

const ranges = [
  { d: 1, label: "1 天" },
  { d: 7, label: "7 天" },
  { d: 30, label: "30 天" },
  { d: 90, label: "90 天" },
  { d: 365, label: "一年" },
]

function Stat({ label, value, tone }: { label: string; value: string; tone: string }) {
  return (
    <Card>
      <CardContent className="p-4">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className={cn("mt-2 text-2xl font-bold tracking-tight", tone)}>{value}</div>
      </CardContent>
    </Card>
  )
}

export function Usage() {
  const [days, setDays] = useState(30)
  const [apiKeyId, setApiKeyId] = useState("")
  const [providerId, setProviderId] = useState("")
  const [model, setModel] = useState("")

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
    return p.toString()
  }, [days, apiKeyId, providerId, model])

  const { data, isLoading } = useQuery({
    queryKey: ["usage", days, apiKeyId, providerId, model],
    queryFn: () => api<TokenUsageResponse>(`/api/admin/token-usage?${params}`),
  })

  const maxTokens = useMemo(() => {
    if (!data) return 0
    return Math.max(1, ...data.series.map((s) => s.total_tokens))
  }, [data])

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">用量与费用</h1>
          <p className="mt-1 text-sm text-muted-foreground">按日聚合的 Token 用量与估算费用。</p>
        </div>
        <div className="flex flex-wrap items-end gap-3">
          <select value={providerId} onChange={(e) => setProviderId(e.target.value)} className="h-9 rounded-md border border-input bg-transparent px-2 text-sm">
            <option value="">全部渠道</option>
            {providers.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
          <select value={apiKeyId} onChange={(e) => setApiKeyId(e.target.value)} className="h-9 rounded-md border border-input bg-transparent px-2 text-sm">
            <option value="">全部 Key</option>
            {keys.map((k) => (
              <option key={k.id} value={k.id}>
                {k.name}
              </option>
            ))}
          </select>
          <input
            value={model}
            onChange={(e) => setModel(e.target.value)}
            placeholder="模型名（可选）"
            className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
          />
          <div className="flex gap-1.5">
            {ranges.map((r) => (
              <button
                key={r.d}
                onClick={() => setDays(r.d)}
                className={cn(
                  "rounded-lg border px-3 py-1.5 text-xs font-medium transition-colors",
                  days === r.d ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                )}
              >
                {r.label}
              </button>
            ))}
          </div>
        </div>
      </div>

      {isLoading || !data ? (
        <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
      ) : (
        <>
          <div className="mb-4 grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
            <Stat label="估算费用" value={formatCost(data.totals.cost_micros)} tone="text-amber-600" />
            <Stat label="总 Token" value={formatTokens(data.totals.total_tokens)} tone="text-primary" />
            <Stat label="输入 Token" value={formatTokens(data.totals.input_tokens)} tone="text-blue-600" />
            <Stat label="输出 Token" value={formatTokens(data.totals.output_tokens)} tone="text-orange-600" />
            <Stat label="请求数" value={formatTokens(data.totals.requests)} tone="text-foreground" />
            <Stat label="采集率" value={(data.totals.usage_coverage ?? 0).toFixed(1) + "%"} tone="text-primary" />
          </div>

          <Card className="mb-4">
            <CardHeader>
              <CardTitle className="text-base">每日 Token 趋势</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="flex h-48 items-end gap-1">
                {data.series.map((s, i) => (
                  <motion.div
                    key={i}
                    className="flex-1 rounded-t bg-gradient-to-t from-primary/50 to-primary"
                    initial={{ height: 0 }}
                    animate={{ height: `${Math.max(2, (s.total_tokens / maxTokens) * 100)}%` }}
                    transition={{ duration: 0.5, delay: i * 0.01, ease: [0.16, 1, 0.3, 1] }}
                    title={`${s.date}: ${formatTokens(s.total_tokens)}`}
                  />
                ))}
              </div>
              <div className="mt-2 flex justify-between text-[10px] text-muted-foreground">
                <span>{data.series[0]?.date?.slice(0, 10)}</span>
                <span>{data.series[data.series.length - 1]?.date?.slice(0, 10)}</span>
              </div>
            </CardContent>
          </Card>

          <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
            {[
              { title: "按密钥", list: data.by_keys.slice(0, 5) },
              { title: "按渠道", list: data.by_providers.slice(0, 5) },
              { title: "按模型", list: data.by_models.slice(0, 5) },
            ].map((rank) => (
              <Card key={rank.title}>
                <CardHeader>
                  <CardTitle className="text-sm">{rank.title}</CardTitle>
                </CardHeader>
                <CardContent className="space-y-2.5">
                  {rank.list.map((item, i) => (
                    <div key={i}>
                      <div className="flex justify-between text-xs">
                        <span className="truncate font-medium">{item.name || "—"}</span>
                        <span className="shrink-0 text-muted-foreground">{formatTokens(item.total_tokens)}</span>
                      </div>
                      <div className="mt-1 h-1.5 rounded-full bg-muted">
                        <div
                          className="h-1.5 rounded-full bg-primary"
                          style={{ width: `${(item.total_tokens / Math.max(1, rank.list[0]?.total_tokens ?? 1)) * 100}%` }}
                        />
                      </div>
                    </div>
                  ))}
                </CardContent>
              </Card>
            ))}
          </div>
        </>
      )}
    </motion.div>
  )
}
