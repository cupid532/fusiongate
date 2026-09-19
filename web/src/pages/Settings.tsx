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
import {
  type RoutingStrategy,
  ROUTING_STRATEGY_LABELS,
  ROUTING_STRATEGY_HELP,
} from "@/lib/types"
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

const strategies: RoutingStrategy[] = ["priority_failover", "ordered_round_robin", "smart_round_robin", "adaptive"]

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
  const { data } = useQuery({
    queryKey: ["routing"],
    queryFn: () => api<{ strategy: RoutingStrategy }>("/api/admin/routing"),
  })

  const mutation = useMutation({
    mutationFn: (strategy: RoutingStrategy) =>
      api("/api/admin/routing", { method: "PATCH", body: JSON.stringify({ strategy }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["routing"] })
      notifySuccess("路由策略已更新")
    },
  })

  const current = data?.strategy ?? "priority_failover"

  return (
    <div className="space-y-6">
      <div className="rounded-xl bg-card p-6 shadow-sm">
        <div className="mb-1 flex items-center gap-2">
          <Settings2 className="h-5 w-5 text-primary" />
          <h2 className="text-lg font-semibold">起始渠道选择</h2>
        </div>
        <p className="mb-6 text-sm text-muted-foreground">
          决定每个新请求从哪个上游渠道开始。所有策略都带请求内故障转移。
        </p>

        <div className="grid gap-3 sm:grid-cols-2">
          {strategies.map((s) => {
            const active = s === current
            return (
              <button
                key={s}
                onClick={() => mutation.mutate(s)}
                disabled={mutation.isPending}
                className={cn(
                  "group relative rounded-xl p-4 text-left transition-all",
                  active
                    ? "bg-primary/8 ring-2 ring-primary"
                    : "bg-muted/40 hover:bg-muted/70"
                )}
              >
                <div className="flex items-start gap-3">
                  <div
                    className={cn(
                      "mt-0.5 h-4 w-4 rounded-full border-2 transition-colors",
                      active ? "border-primary bg-primary" : "border-muted-foreground/40"
                    )}
                  >
                    {active && (
                      <div className="flex h-full items-center justify-center">
                        <div className="h-1.5 w-1.5 rounded-full bg-white" />
                      </div>
                    )}
                  </div>
                  <div className="flex-1">
                    <div className="text-sm font-medium">{ROUTING_STRATEGY_LABELS[s]}</div>
                    <div className="mt-1 text-xs text-muted-foreground leading-relaxed">
                      {ROUTING_STRATEGY_HELP[s]}
                    </div>
                  </div>
                </div>
              </button>
            )
          })}
        </div>
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
