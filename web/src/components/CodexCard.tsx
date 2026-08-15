import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Ticket, Zap, Clock } from "lucide-react"
import { api } from "@/lib/api"
import type { CodexAccountQuota, Provider } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { InlinePriorityEditor } from "@/components/InlinePriorityEditor"

function formatDate(iso?: string) {
  if (!iso) return "—"
  return new Date(iso).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

function formatDuration(seconds: number): string {
  if (seconds <= 0) return "即将重置"
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分钟`
  return `${m} 分钟`
}

export function CodexCard({ provider }: { provider: Provider }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [notice, setNotice] = useState("")

  const updatePriority = useMutation({
    mutationFn: async (priority: number) =>
      api(`/api/admin/providers/${provider.id}`, { method: "PATCH", body: JSON.stringify({ priority }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers"] }),
  })

  const { data: quota, isLoading, refetch, isFetching } = useQuery({
    queryKey: ["codex-quota", provider.id],
    queryFn: () => api<CodexAccountQuota>(`/api/admin/auth/quota/${provider.id}`),
  })

  // 重置倒计时（基于 primary 窗口的 reset_after_seconds）
  const [remaining, setRemaining] = useState(0)
  useEffect(() => {
    const resetAfter = quota?.primary?.reset_after_seconds
    if (!resetAfter) {
      setRemaining(0)
      return
    }
    setRemaining(resetAfter)
    const timer = setInterval(() => setRemaining((s) => Math.max(0, s - 1)), 1000)
    return () => clearInterval(timer)
  }, [quota?.primary?.reset_after_seconds])

  const toggle = useMutation({
    mutationFn: async (enabled: boolean) =>
      api(`/api/admin/providers/${provider.id}`, { method: "PATCH", body: JSON.stringify({ enabled }) }),
    onSuccess: (_res, enabled) => {
      setNotice(enabled ? "认证已开启，将参与后续调用" : "认证已关闭，不参与模型调用，配额仍可查看")
      setTimeout(() => setNotice(""), 3000)
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      qc.invalidateQueries({ queryKey: ["codex-quota", provider.id] })
    },
    onError: (e) => {
      setNotice(e instanceof Error ? e.message : "认证状态更新失败")
      setTimeout(() => setNotice(""), 3000)
    },
  })

  const redeem = useMutation({
    mutationFn: async (creditId?: string) =>
      api<{ redeemed?: unknown; quota?: CodexAccountQuota | null; warning?: string }>(
        `/api/admin/auth/quota/${provider.id}/reset`,
        { method: "POST", body: JSON.stringify(creditId ? { credit_id: creditId } : {}) }
      ),
    onSuccess: (res) => {
      if (res.warning) {
        setNotice("已兑换，但刷新配额失败")
      } else {
        setNotice("重置卡已使用，额度已重置")
      }
      setTimeout(() => setNotice(""), 3000)
      qc.invalidateQueries({ queryKey: ["codex-quota", provider.id] })
    },
    onError: (e) => {
      setNotice(e instanceof Error ? e.message : "兑换失败")
      setTimeout(() => setNotice(""), 3000)
    },
  })

  function handleRedeem(creditId?: string) {
    const total = quota?.reset_cards ?? 0
    if (total <= 0) {
      setNotice("当前没有可用的重置卡")
      setTimeout(() => setNotice(""), 3000)
      return
    }
    if (!confirm(`当前有 ${total} 张重置卡，确定使用 1 张重置卡？`)) return
    redeem.mutate(creditId)
  }

  const quotaRemaining = quota?.remaining_quota ?? 0
  const quotaUsed = quota?.used_quota ?? 0
  const windowTotal = quota?.primary?.limit_window_seconds ?? remaining
  const resetRatio = windowTotal > 0 ? remaining / windowTotal : 0

  return (
    <Card className="overflow-hidden">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center justify-between text-base">
          <span className="flex min-w-0 items-center gap-2"><span className="truncate">{provider.name}</span><InlinePriorityEditor value={provider.priority} disabled={updatePriority.isPending} onSave={async (priority) => { await updatePriority.mutateAsync(priority) }} /></span>
          <Badge variant={!provider.enabled ? "neutral" : quota?.allowed === false ? "danger" : quota?.limit_reached ? "warning" : "success"}>
            {!provider.enabled ? "已关闭" : quota?.allowed === false ? "不可用" : quota?.limit_reached ? "已达限" : "可用"}
          </Badge>
        </CardTitle>
        {quota?.plan_type && <p className="text-xs text-muted-foreground">{quota.subscription_plan || quota.plan_type}</p>}
      </CardHeader>

      <CardContent className="space-y-3">
        {!provider.enabled && (
          <div className="rounded-md border border-dashed px-3 py-2 text-xs text-muted-foreground">
            已关闭模型调用，配额与重置时间仍会更新。
          </div>
        )}

        {isLoading ? (
          <div className="py-4 text-center text-sm text-muted-foreground">加载配额中…</div>
        ) : quota ? (
          <>
            {/* 剩余额度进度条 */}
            <div>
              <div className="mb-1 flex items-end justify-between">
                <span className="text-xs text-muted-foreground">剩余额度</span>
                <span className="text-2xl font-bold tracking-tight">{quotaRemaining.toFixed(1)}%</span>
              </div>
              <div className="h-2 overflow-hidden rounded-full bg-muted">
                <motion.div
                  className={cn("h-2 rounded-full", quotaRemaining < 10 ? "bg-destructive" : quotaRemaining < 30 ? "bg-amber-500" : "bg-primary")}
                  initial={{ width: 0 }}
                  animate={{ width: `${quotaRemaining}%` }}
                  transition={{ duration: 0.6, ease: [0.16, 1, 0.3, 1] }}
                />
              </div>
              <div className="mt-1 text-[11px] text-muted-foreground">已用 {quotaUsed.toFixed(1)}% · 总额度 100%</div>
            </div>

            {/* 重置倒计时能量条 */}
            {quota.primary?.reset_after_seconds != null && quota.primary.reset_after_seconds > 0 && (
              <div>
                <div className="mb-1 flex items-end justify-between">
                  <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    <Clock className="h-3.5 w-3.5" />
                    距下次重置
                  </span>
                  <span className="text-sm font-semibold">{formatDuration(remaining)}</span>
                </div>
                <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <motion.div
                    className="h-1.5 rounded-full bg-blue-500"
                    animate={{ width: `${Math.max(0, Math.min(100, resetRatio * 100))}%` }}
                    transition={{ duration: 1, ease: "linear" }}
                  />
                </div>
              </div>
            )}

            {/* 使用窗口 */}
            {(quota.primary || quota.secondary) && (
              <div className="grid grid-cols-2 gap-2">
                {quota.primary && (
                  <div className="rounded-lg border p-2">
                    <div className="text-[10px] text-muted-foreground">主窗口</div>
                    <div className="text-sm font-semibold">{quota.primary.used_percent.toFixed(1)}% 已用</div>
                    {quota.primary.reset_at && <div className="text-[10px] text-muted-foreground">重置 {formatDate(quota.primary.reset_at)}</div>}
                  </div>
                )}
                {quota.secondary && (
                  <div className="rounded-lg border p-2">
                    <div className="text-[10px] text-muted-foreground">次窗口</div>
                    <div className="text-sm font-semibold">{quota.secondary.used_percent.toFixed(1)}% 已用</div>
                    {quota.secondary.reset_at && <div className="text-[10px] text-muted-foreground">重置 {formatDate(quota.secondary.reset_at)}</div>}
                  </div>
                )}
              </div>
            )}

            {/* 下次重置 */}
            {quota.next_reset_date && (
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <RefreshCw className="h-3.5 w-3.5" />
                下次重置：{formatDate(quota.next_reset_date)}
              </div>
            )}

            {/* 积分余额 */}
            {quota.credits_balance != null && (
              <div className="flex items-center gap-2 text-xs">
                <Zap className="h-3.5 w-3.5 text-amber-500" />
                <span className="text-muted-foreground">积分余额</span>
                <span className="font-semibold">
                  {quota.credits_unlimited ? "无限" : formatCost(quota.credits_balance * 1_000_000)}
                </span>
              </div>
            )}

            {/* 重置卡 */}
            <div className="rounded-lg border p-2">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-2 text-xs">
                  <Ticket className="h-3.5 w-3.5 text-primary" />
                  <span className="font-medium">重置卡</span>
                  <Badge variant={quota.reset_cards > 0 ? "success" : "neutral"}>{quota.reset_cards} 张</Badge>
                </div>
                <Button size="sm" variant="outline" disabled={redeem.isPending} onClick={() => handleRedeem()}>
                  {redeem.isPending ? "使用中…" : "使用重置卡"}
                </Button>
              </div>

              {quota.reset_card_details && quota.reset_card_details.length > 0 && (
                <>
                  <button className="mt-2 text-[11px] text-muted-foreground hover:underline" onClick={() => setExpanded((v) => !v)}>
                    {expanded ? "收起" : `展开 ${quota.reset_card_details.length} 张卡详情`}
                  </button>
                  {expanded && (
                    <div className="mt-2 space-y-1.5">
                      {quota.reset_card_details.map((c, i) => (
                        <div key={c.id || i} className="flex items-center justify-between rounded-md bg-muted/40 px-2 py-1.5 text-[11px]">
                          <span>
                            <span className="font-medium">{c.reset_type || "重置卡"}</span>
                            {c.status && <span className="ml-2 text-muted-foreground">{c.status}</span>}
                          </span>
                          <div className="flex items-center gap-2">
                            {c.expires_at && <span className="text-muted-foreground">到期 {formatDate(c.expires_at)}</span>}
                            {c.status !== "redeemed" && c.status !== "expired" && (
                              <Button size="sm" variant="ghost" disabled={redeem.isPending} onClick={() => handleRedeem(c.id)}>
                                兑换
                              </Button>
                            )}
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </>
              )}
            </div>
          </>
        ) : (
          <div className="py-4 text-center text-sm text-muted-foreground">无法获取配额</div>
        )}

        {/* 正反馈提示 */}
        {notice && (
          <div className="rounded-md bg-primary/10 px-3 py-2 text-xs text-primary">{notice}</div>
        )}

        <div className="flex items-center justify-between border-t pt-3">
          <label className="flex items-center gap-2 text-sm font-medium">
            <Switch
              checked={provider.enabled}
              disabled={toggle.isPending}
              onCheckedChange={(enabled) => toggle.mutate(enabled)}
              aria-label={`${provider.name} 参与调用开关`}
            />
            {provider.enabled ? "参与调用" : "已关闭"}
          </label>
          <Button variant="ghost" size="sm" disabled={toggle.isPending} onClick={() => void refetch()}>
            <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
            刷新
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
