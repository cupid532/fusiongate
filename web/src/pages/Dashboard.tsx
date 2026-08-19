import { useEffect, useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { animate, motion, useMotionValue } from "motion/react"
import { Server, Boxes, KeyRound, Activity, AlertTriangle, Coins, Copy, Check, ShieldCheck, HardHat } from "lucide-react"
import { api } from "@/lib/api"
import { formatCost, formatTokens, cn } from "@/lib/utils"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { EmptyState } from "@/components/ui/empty-state"
import type { Provider } from "@/lib/types"

type DashboardData = {
  providers: number
  models: number
  keys: number
  requests: number
  today_requests: number
  failures_24h: number
  total_tokens: number
  cost_micros: number
}

function CountUp({ value, format }: { value: number; format?: (n: number) => string }) {
  const mv = useMotionValue(0)
  const [display, setDisplay] = useState("0")

  useEffect(() => {
    const controls = animate(mv, value, { duration: 0.9, ease: [0.16, 1, 0.3, 1] })
    const unsub = mv.on("change", (v) => {
      setDisplay(format ? format(v) : String(Math.round(v)))
    })
    return () => {
      controls.stop()
      unsub()
    }
  }, [value, mv, format])

  return <span>{display}</span>
}

const statItems: {
  key: keyof DashboardData
  label: string
  icon: typeof Server
  tone: string
  format?: (n: number) => string
}[] = [
  { key: "providers", label: "启用渠道", icon: Server, tone: "text-primary bg-primary/10" },
  { key: "models", label: "公开模型", icon: Boxes, tone: "text-blue-500 bg-blue-500/10" },
  { key: "keys", label: "活跃密钥", icon: KeyRound, tone: "text-violet-500 bg-violet-500/10" },
  { key: "today_requests", label: "今日请求", icon: Activity, tone: "text-cyan-500 bg-cyan-500/10" },
  { key: "failures_24h", label: "24h 失败", icon: AlertTriangle, tone: "text-destructive bg-destructive/10" },
  { key: "cost_micros", label: "近一年费用", icon: Coins, tone: "text-amber-500 bg-amber-500/10", format: (n) => formatCost(n) },
]

export function Dashboard() {
  const { data, isLoading } = useQuery({
    queryKey: ["dashboard"],
    queryFn: () => api<DashboardData>("/api/admin/dashboard"),
    staleTime: 30_000,
  })
  const { data: providers = [] } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
    staleTime: 30_000,
  })

  const health = useMemo(() => {
    const totals = { total: providers.length, healthy: 0, unhealthy: 0, unknown: 0 }
    for (const p of providers) {
      if (!p.enabled) continue
      if (p.health_check_status === "healthy") totals.healthy++
      else if (p.health_check_status === "unhealthy") totals.unhealthy++
      else totals.unknown++
    }
    const healthyPct = totals.total ? Math.round((totals.healthy / totals.total) * 100) : 0
    return { ...totals, healthyPct }
  }, [providers])

  const [copied, setCopied] = useState(false)

  async function copyBaseUrl() {
    const url = `${location.origin}/v1`
    try {
      await navigator.clipboard.writeText(url)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      /* ignore */
    }
  }

  if (isLoading || !data) {
    return <div className="animate-pulse text-sm text-muted-foreground">加载中…</div>
  }

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.35 }}>
      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">网关概览</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            统一观察 Provider、模型路由、访问密钥和请求状态。
          </p>
        </div>
        <Button variant="outline" onClick={copyBaseUrl}>
          {copied ? <Check className="h-4 w-4 text-primary" /> : <Copy className="h-4 w-4" />}
          复制 Base URL
        </Button>
      </div>

      <div className="mb-5 grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        {statItems.map((item, i) => {
          const raw = data[item.key] as number
          const value = item.key === "cost_micros" ? data.cost_micros / 1_000_000 : raw
          return (
            <motion.div
              key={item.key}
              initial={{ opacity: 0, y: 12 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.4, delay: i * 0.05 }}
            >
              <Card className="overflow-hidden">
                <CardContent className="p-4">
                  <div className="flex items-center justify-between text-xs text-muted-foreground">
                    <span>{item.label}</span>
                    <span className={cn("grid h-8 w-8 place-items-center rounded-lg", item.tone)}>
                      <item.icon className="h-4 w-4" />
                    </span>
                  </div>
                  <div className="mt-2.5 text-[26px] font-bold tracking-tight">
                    {item.format ? (
                      <CountUp value={value} format={item.format} />
                    ) : (
                      <CountUp value={value} />
                    )}
                  </div>
                </CardContent>
              </Card>
            </motion.div>
          )
        })}
      </div>

      <div className="mb-4 grid grid-cols-1 gap-4 lg:grid-cols-[0.65fr_1.35fr]">
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <ShieldCheck className="h-4 w-4 text-primary" />
              <CardTitle className="text-base">渠道健康度</CardTitle>
            </div>
            <CardDescription>基于启用渠道的最近健康检测。</CardDescription>
          </CardHeader>
          <CardContent>
            {providers.length === 0 ? (
              <EmptyState title="暂无渠道" description="添加上游 Provider 后即可查看健康概览。" />
            ) : (
              <div className="space-y-4">
                <div className="flex items-end gap-3">
                  <div className="text-4xl font-bold tabular-nums">{health.healthyPct}%</div>
                  <div className="pb-1 text-xs text-muted-foreground">健康率</div>
                </div>
                <div className="flex h-2 w-full overflow-hidden rounded-full bg-muted">
                  <div className="h-full bg-emerald-500" style={{ width: `${health.healthyPct}%` }} />
                  <div className="h-full bg-destructive" style={{ width: `${health.unhealthy ? (health.unhealthy / Math.max(1, health.total)) * 100 : 0}%` }} />
                </div>
                <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                  <span>渠道 <span className="font-semibold text-foreground">{health.total}</span></span>
                  <span>健康 <span className="font-semibold text-emerald-600">{health.healthy}</span></span>
                  <span>不健康 <span className={cn("font-semibold", health.unhealthy ? "text-destructive" : "")}>{health.unhealthy}</span></span>
                  <span>待检测 <span className="font-semibold">{health.unknown}</span></span>
                </div>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">聚合健康状态</CardTitle>
            <CardDescription>近 24 小时请求与失败情况。</CardDescription>
          </CardHeader>
          <CardContent className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            {[
              { label: "24h 失败请求", value: String(data.failures_24h), tone: data.failures_24h ? "text-destructive" : "text-emerald-600" },
              { label: "今日请求", value: String(data.today_requests), tone: "text-foreground" },
              { label: "累计请求", value: formatTokens(data.requests), tone: "text-foreground" },
            ].map((s) => (
              <div key={s.label} className="rounded-xl border bg-card p-3">
                <div className="text-[11px] text-muted-foreground">{s.label}</div>
                <div className={cn("mt-1 text-2xl font-bold tabular-nums", s.tone)}>{s.value}</div>
              </div>
            ))}
            <div className="col-span-2 flex items-center gap-2 rounded-xl border bg-muted/40 p-3 text-xs text-muted-foreground sm:col-span-3">
              <HardHat className="h-4 w-4 text-primary" />
              健康状态由质量检测与健康检查共同驱动；详细情况请在「质量检测」与「上游渠道」中查看。
            </div>
          </CardContent>
        </Card>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[1.35fr_0.65fr]">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">开始使用</CardTitle>
            <CardDescription>完成三步即可从客户端调用统一模型。</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {[
              { n: "01", t: "连接上游 Provider", d: "添加 OpenAI、Anthropic、Gemini 或兼容渠道" },
              { n: "02", t: "建立统一模型路由", d: "把不同渠道的真实模型合并为统一故障转移组" },
              { n: "03", t: "签发下游 API Key", d: "一把 Key 按权限访问多个渠道和模型" },
            ].map((s) => (
              <div key={s.n} className="flex items-center gap-4 rounded-lg border p-3">
                <div className="grid h-8 w-8 shrink-0 place-items-center rounded-lg bg-primary/10 text-sm font-bold text-primary">
                  {s.n}
                </div>
                <div>
                  <div className="text-sm font-medium">{s.t}</div>
                  <div className="text-xs text-muted-foreground">{s.d}</div>
                </div>
              </div>
            ))}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">连接信息</CardTitle>
            <CardDescription>兼容 OpenAI SDK 与常用客户端。</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="rounded-lg bg-muted p-3">
              <div className="text-[10px] uppercase tracking-wider text-muted-foreground">Base URL</div>
              <div className="mt-1 truncate font-mono text-sm">{location.origin}/v1</div>
            </div>
            <div className="text-xs text-muted-foreground">
              近一年累计 <span className="font-semibold text-foreground">{formatTokens(data.total_tokens)}</span> token ·{" "}
              <span className="font-semibold text-foreground">{formatCost(data.cost_micros)}</span> 费用
            </div>
          </CardContent>
        </Card>
      </div>
    </motion.div>
  )
}
