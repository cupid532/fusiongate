import { useState } from "react"
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import {
  Settings2,
  Route,
  DollarSign,
  Info,
  Shield,
  RefreshCw,
  CheckCircle2,
  AlertCircle,
  ExternalLink,
  Copy,
  Check,
  Eye,
  EyeOff,
  Lock,
} from "lucide-react"
import { api } from "@/lib/api"
import type { RoutingSettings } from "@/lib/types"
import { ROUTING_STRATEGY_HELP, ROUTING_STRATEGY_LABELS } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { cn } from "@/lib/utils"
import { notifySuccess, notifyError } from "@/lib/notify"

type SettingsTab = "routing" | "pricing" | "security" | "info"

const tabs: { value: SettingsTab; label: string; icon: typeof Route }[] = [
  { value: "routing", label: "路由策略", icon: Route },
  { value: "pricing", label: "模型定价", icon: DollarSign },
  { value: "security", label: "安全", icon: Shield },
  { value: "info", label: "网关信息", icon: Info },
]

interface PricingStatus {
  interval: string
  sources: string[]
  status: {
    pricing_last_error: string
    pricing_last_models: string
    pricing_last_sources: string
    pricing_last_sync: string
    pricing_last_updated_routes: string
  }
}

interface Metrics {
  uptime_seconds?: number
  goroutines?: number
  memory_alloc_mb?: number
  [key: string]: unknown
}

// The reliability parameters exposed by PATCH /api/admin/routing. They tune the
// single strategy's retries and session retention; there is no second algorithm
// to select between.
const routingFields = [
  { key: "channel_attempts", label: "单渠道重试次数", hint: "包含第一次，1–5；同一渠道的全部 Key 共享该额度" },
  { key: "max_attempts", label: "最大总尝试次数", hint: "1–100；达到后停止故障转移" },
  { key: "channel_window_seconds", label: "单渠道时间窗口（秒）", hint: "1–3600；同一渠道重试的时限" },
  { key: "failover_window_seconds", label: "故障转移总窗口（秒）", hint: "1–7200；整次请求的转移时限" },
  { key: "session_idle_days", label: "会话空闲保留（天）", hint: "1–365；任务静默超过该天数后不再保持渠道粘性" },
  { key: "session_max_days", label: "会话最长保留（天）", hint: "不小于空闲天数，最长 365" },
  { key: "session_capacity", label: "会话容量", hint: "1–1000000；保留的任务绑定数量上限" },
] as const satisfies ReadonlyArray<{ key: keyof Omit<RoutingSettings, "strategy">; label: string; hint: string }>

export function Settings() {
  const [tab, setTab] = useState<SettingsTab>("routing")

  return (
    <div>
      <div className="mb-6">
        <h1 className="text-2xl font-bold tracking-tight">系统设置</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          管理路由策略、模型定价、安全与网关运行状态。
        </p>
      </div>

      <div className="mb-6 flex items-center gap-1 rounded-lg bg-muted/60 p-1">
        {tabs.map((t) => (
          <button
            key={t.value}
            onClick={() => setTab(t.value)}
            className={cn(
              "inline-flex items-center gap-2 rounded-md px-4 py-2 text-sm font-medium transition-colors",
              tab === t.value
                ? "bg-card text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            <t.icon className="h-4 w-4" />
            {t.label}
          </button>
        ))}
      </div>

      <motion.div
        key={tab}
        initial={{ opacity: 0, y: 8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.25 }}
      >
        {tab === "routing" && <RoutingTab />}
        {tab === "pricing" && <PricingTab />}
        {tab === "security" && <SecurityTab />}
        {tab === "info" && <InfoTab />}
      </motion.div>
    </div>
  )
}

function RoutingTab() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: ["routing"],
    queryFn: () => api<RoutingSettings>("/api/admin/routing"),
  })

  // The form is seeded from the server response and only ever PATCHes the whole
  // object, because the endpoint validates the parameters as a set (for example
  // session_max_days must not be below session_idle_days).
  const [form, setForm] = useState<RoutingSettings | null>(null)
  const active = form ?? data ?? null

  const mutation = useMutation({
    mutationFn: (next: RoutingSettings) =>
      api<RoutingSettings>("/api/admin/routing", { method: "PATCH", body: JSON.stringify(next) }),
    onSuccess: async (result) => {
      qc.setQueryData(["routing"], result)
      await qc.invalidateQueries({ queryKey: ["routing"] })
      notifySuccess("路由参数已更新")
    },
    onError: (err: Error) => notifyError("路由参数保存失败", err.message),
  })

  return (
    <div className="space-y-6">
      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-1 flex items-center gap-2">
          <Settings2 className="h-5 w-5 text-primary" />
          <h2 className="text-lg font-semibold">渠道选择顺序</h2>
        </div>
        <p className="mb-4 text-sm text-muted-foreground">
          当前策略：{ROUTING_STRATEGY_LABELS.priority_failover}。这是唯一的策略，无法切换。
        </p>
        <p className="text-xs text-muted-foreground">{ROUTING_STRATEGY_HELP.priority_failover}</p>
      </div>

      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-1 flex items-center gap-2">
          <RefreshCw className="h-5 w-5 text-primary" />
          <h2 className="text-lg font-semibold">重试与故障转移参数</h2>
        </div>
        <p className="mb-6 text-sm text-muted-foreground">
          这些参数只影响重试次数、时间窗口和任务粘性保留，不改变渠道选择顺序。
        </p>

        {isLoading ? (
          <div role="status" className="py-8 text-center text-sm text-muted-foreground">正在读取路由参数…</div>
        ) : isError ? (
          <div role="alert" className="space-y-3 text-sm text-destructive">
            <p>路由参数读取失败：{error instanceof Error ? error.message : "未知错误"}</p>
            <Button variant="outline" onClick={() => void refetch()}>重试</Button>
          </div>
        ) : active ? (
          <form
            className="space-y-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (form) mutation.mutate(form)
            }}
          >
            <div className="grid gap-4 sm:grid-cols-2">
              {routingFields.map((field) => (
                <div key={field.key} className="space-y-1.5">
                  <Label htmlFor={`routing-${field.key}`}>{field.label}</Label>
                  <Input
                    id={`routing-${field.key}`}
                    name={field.key}
                    type="number"
                    inputMode="numeric"
                    value={active[field.key]}
                    onChange={(event) => {
                      const parsed = Number.parseInt(event.target.value, 10)
                      setForm({
                        ...active,
                        [field.key]: Number.isNaN(parsed) ? 0 : parsed,
                      })
                    }}
                  />
                  <p className="text-xs text-muted-foreground">{field.hint}</p>
                </div>
              ))}
            </div>
            <div className="flex items-center gap-3">
              <Button type="submit" disabled={!form || mutation.isPending}>保存路由参数</Button>
              {form && <Button type="button" variant="outline" onClick={() => setForm(null)} disabled={mutation.isPending}>放弃修改</Button>}
            </div>
          </form>
        ) : (
          <div role="alert" className="text-sm text-destructive">路由参数不可用。</div>
        )}

        {mutation.isError && <div role="alert" className="mt-4 text-sm text-destructive">路由参数保存失败：{mutation.error instanceof Error ? mutation.error.message : "未知错误"}</div>}
      </div>
    </div>
  )
}

function PricingTab() {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ["pricing"],
    queryFn: () => api<PricingStatus>("/api/admin/pricing"),
  })

  const syncMutation = useMutation({
    mutationFn: () => api("/api/admin/pricing", { method: "POST", body: JSON.stringify({ action: "sync" }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["pricing"] })
      notifySuccess("定价同步已触发")
    },
  })

  const lastSync = data?.status?.pricing_last_sync
    ? new Date(data.status.pricing_last_sync).toLocaleString("zh-CN")
    : "—"
  const lastError = data?.status?.pricing_last_error

  return (
    <div className="space-y-6">
      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-1 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <DollarSign className="h-5 w-5 text-primary" />
            <h2 className="text-lg font-semibold">模型定价同步</h2>
          </div>
          <Button
            size="sm"
            variant="outline"
            onClick={() => syncMutation.mutate()}
            disabled={syncMutation.isPending}
          >
            <RefreshCw className={cn("mr-1.5 h-3.5 w-3.5", syncMutation.isPending && "animate-spin")} />
            立即同步
          </Button>
        </div>
        <p className="mb-6 text-sm text-muted-foreground">
          从官方定价页自动同步模型价格，每 {data?.interval ?? "—"} 自动执行。
        </p>

        {isLoading ? (
          <div className="space-y-3">
            {[1, 2, 3].map((i) => (
              <div key={i} className="h-12 animate-pulse rounded-lg bg-muted/40" />
            ))}
          </div>
        ) : (
          <div className="space-y-4">
            <div className="grid gap-4 sm:grid-cols-3">
              <MetricBox label="上次同步" value={lastSync} />
              <MetricBox label="覆盖模型" value={data?.status?.pricing_last_models ?? "—"} />
              <MetricBox label="已更新路由" value={data?.status?.pricing_last_updated_routes ?? "—"} />
            </div>

            {lastError && (
              <div className="flex items-start gap-2 rounded-lg bg-destructive/8 p-3 text-sm text-destructive">
                <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
                {lastError}
              </div>
            )}

            {!lastError && data?.status?.pricing_last_sync && (
              <div className="flex items-center gap-2 text-sm text-primary">
                <CheckCircle2 className="h-4 w-4" />
                定价数据同步正常
              </div>
            )}

            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">定价来源</div>
              <div className="space-y-1.5">
                {data?.sources?.map((src) => (
                  <div key={src} className="flex items-center gap-2 rounded-lg bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                    <ExternalLink className="h-3 w-3 shrink-0" />
                    <span className="truncate">{src}</span>
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

function SecurityTab() {
  const [currentPw, setCurrentPw] = useState("")
  const [newPw, setNewPw] = useState("")
  const [confirmPw, setConfirmPw] = useState("")
  const [showCurrent, setShowCurrent] = useState(false)
  const [showNew, setShowNew] = useState(false)

  const changePw = useMutation({
    mutationFn: () =>
      api("/api/admin/password", {
        method: "POST",
        body: JSON.stringify({ current_password: currentPw, new_password: newPw }),
      }),
    onSuccess: () => {
      notifySuccess("管理员密码已更新", "下次登录请使用新密码。")
      setCurrentPw("")
      setNewPw("")
      setConfirmPw("")
    },
    onError: (err: Error) => {
      notifyError("密码修改失败", err.message)
    },
  })

  const mismatch = confirmPw.length > 0 && newPw !== confirmPw
  const tooShort = newPw.length > 0 && newPw.length < 8
  const canSubmit = currentPw.length > 0 && newPw.length >= 8 && newPw === confirmPw && !changePw.isPending

  return (
    <div className="space-y-6">
      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-1 flex items-center gap-2">
          <Lock className="h-5 w-5 text-primary" />
          <h2 className="text-lg font-semibold">修改管理员密码</h2>
        </div>
        <p className="mb-6 text-sm text-muted-foreground">
          更改后需重新登录。密码至少 8 个字符。
        </p>

        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (canSubmit) changePw.mutate()
          }}
          className="max-w-md space-y-4"
        >
          <div className="space-y-2">
            <Label htmlFor="current-pw">当前密码</Label>
            <div className="relative">
              <Input
                id="current-pw"
                type={showCurrent ? "text" : "password"}
                autoComplete="current-password"
                value={currentPw}
                onChange={(e) => setCurrentPw(e.target.value)}
                className="h-10 pr-10"
              />
              <button
                type="button"
                onClick={() => setShowCurrent(!showCurrent)}
                className="absolute right-2 top-1/2 -translate-y-1/2 rounded-md p-1 text-muted-foreground hover:text-foreground"
              >
                {showCurrent ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </button>
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="new-pw">新密码</Label>
            <div className="relative">
              <Input
                id="new-pw"
                type={showNew ? "text" : "password"}
                autoComplete="new-password"
                value={newPw}
                onChange={(e) => setNewPw(e.target.value)}
                className={cn("h-10 pr-10", tooShort && "ring-2 ring-destructive/50")}
              />
              <button
                type="button"
                onClick={() => setShowNew(!showNew)}
                className="absolute right-2 top-1/2 -translate-y-1/2 rounded-md p-1 text-muted-foreground hover:text-foreground"
              >
                {showNew ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </button>
            </div>
            {tooShort && <p className="text-xs text-destructive">密码至少 8 个字符</p>}
          </div>

          <div className="space-y-2">
            <Label htmlFor="confirm-pw">确认新密码</Label>
            <Input
              id="confirm-pw"
              type="password"
              autoComplete="new-password"
              value={confirmPw}
              onChange={(e) => setConfirmPw(e.target.value)}
              className={cn("h-10", mismatch && "ring-2 ring-destructive/50")}
            />
            {mismatch && <p className="text-xs text-destructive">两次输入的密码不一致</p>}
          </div>

          <Button type="submit" disabled={!canSubmit} className="mt-2">
            {changePw.isPending ? "提交中…" : "修改密码"}
          </Button>
        </form>
      </div>
    </div>
  )
}

function InfoTab() {
  const [copied, setCopied] = useState(false)
  const { data: metrics } = useQuery({
    queryKey: ["metrics"],
    queryFn: () => api<Metrics>("/api/admin/metrics"),
    refetchInterval: 15_000,
  })
  const { data: dashboard } = useQuery({ queryKey: ["dashboard"], queryFn: () => api<import("@/lib/types").DashboardData>("/api/admin/dashboard") })

  const version = document.querySelector('meta[name="fusiongate-version"]')?.getAttribute("content") ?? "—"
  const baseUrl = `${location.origin}/v1`

  function copyUrl() {
    navigator.clipboard.writeText(baseUrl)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const uptime = metrics?.uptime_seconds
  const uptimeStr = uptime
    ? uptime >= 86400
      ? `${Math.floor(uptime / 86400)}d ${Math.floor((uptime % 86400) / 3600)}h`
      : uptime >= 3600
        ? `${Math.floor(uptime / 3600)}h ${Math.floor((uptime % 3600) / 60)}m`
        : `${Math.floor(uptime / 60)}m`
    : "—"

  return (
    <div className="space-y-6">
      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-4 flex items-center gap-2">
          <Info className="h-5 w-5 text-primary" />
          <h2 className="text-lg font-semibold">网关信息</h2>
        </div>

        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <MetricBox label="版本" value={version} />
          <MetricBox label="运行时间" value={uptimeStr} />
          <MetricBox label="Goroutines" value={metrics?.goroutines?.toString() ?? "—"} />
          <MetricBox label="内存占用" value={metrics?.memory_alloc_mb ? `${metrics.memory_alloc_mb.toFixed(1)} MB` : "—"} />
        </div>
      </div>

      <div className="rounded-xl bg-card p-6 shadow-sm">
        <h2 className="mb-2 text-lg font-semibold">连接信息</h2>
        <p className="mb-3 text-sm text-muted-foreground">兼容 OpenAI SDK 与常用客户端。</p>
        <div className="flex items-center gap-2 rounded-lg bg-muted/40 px-4 py-3">
          <span className="text-[10px] font-bold uppercase tracking-wider text-muted-foreground">Base URL</span>
          <code className="flex-1 text-sm font-mono">{baseUrl}</code>
          <button onClick={copyUrl} className="rounded-md p-1.5 hover:bg-muted transition-colors" title="复制">
            {copied ? <Check className="h-4 w-4 text-primary" /> : <Copy className="h-4 w-4 text-muted-foreground" />}
          </button>
        </div>

        {dashboard && (
          <div className="mt-4 grid gap-4 sm:grid-cols-3">
            <MetricBox label="启用渠道" value={dashboard.providers.toString()} />
            <MetricBox label="公开模型" value={dashboard.models.toString()} />
            <MetricBox label="活跃密钥" value={dashboard.keys.toString()} />
          </div>
        )}
      </div>
    </div>
  )
}

function MetricBox({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg bg-muted/30 px-4 py-3">
      <div className="text-[11px] font-medium text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-sm font-semibold tabular-nums">{value}</div>
    </div>
  )
}
