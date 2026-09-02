import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Ticket, Zap, Clock, AlertTriangle } from "lucide-react"
import { api } from "@/lib/api"
import type { CodexAccountQuota, Provider } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import {
  bindingWindow,
  formatResetDuration,
  namedWindows,
  planLabel,
  usageBarTone,
  usageTone,
} from "@/lib/codex-windows"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { InlinePriorityEditor } from "@/components/InlinePriorityEditor"
import { useConfirm } from "@/components/ui/confirm"

function formatDate(iso?: string) {
  if (!iso) return "—"
  return new Date(iso).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

export function CodexCard({ provider }: { provider: Provider }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
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

  const windows = useMemo(() => namedWindows(quota), [quota])
  const binding = useMemo(() => bindingWindow(quota), [quota])

  // One ticking clock drives every window's countdown. The previous version
  // only tracked `primary`, so on Plus and Team the weekly window had no
  // countdown at all — and the weekly window is the one you plan around.
  const [elapsed, setElapsed] = useState(0)
  useEffect(() => {
    setElapsed(0)
    if (windows.every((w) => !w.window.reset_after_seconds)) return
    const timer = setInterval(() => setElapsed((s) => s + 1), 1000)
    return () => clearInterval(timer)
  }, [windows])

  const countdownFor = (seconds?: number) =>
    seconds == null ? null : Math.max(0, seconds - elapsed)

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

  async function handleRedeem(creditId?: string) {
    const total = quota?.reset_cards ?? 0
    if (total <= 0) {
      setNotice("当前没有可用的重置卡")
      setTimeout(() => setNotice(""), 3000)
      return
    }
    if (!(await confirm({ title: "使用 1 张重置卡？", description: `当前共有 ${total} 张，使用后不可撤销。`, confirmLabel: "使用 1 张" }))) return
    redeem.mutate(creditId)
  }

  const plan = planLabel(quota)

  return (
    // h-full + flex column so cards in the same grid row end at the same
    // bottom edge. A Free account has one window row and a Team account has
    // two, so without this the shorter card stopped ~60px above its neighbour
    // and the footers sat at different heights.
    <Card className="flex h-full flex-col overflow-hidden">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center justify-between text-base">
          <span className="flex min-w-0 items-center gap-2"><span className="truncate">{provider.name}</span><InlinePriorityEditor value={provider.priority} disabled={updatePriority.isPending} onSave={async (priority) => { await updatePriority.mutateAsync(priority) }} /></span>
          <Badge variant={!provider.enabled ? "neutral" : quota?.allowed === false ? "danger" : quota?.limit_reached ? "warning" : "success"}>
            {!provider.enabled ? "已关闭" : quota?.allowed === false ? "不可用" : quota?.limit_reached ? "已达限" : "可用"}
          </Badge>
        </CardTitle>
        {plan && (
          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <Badge variant="neutral">{plan}</Badge>
            {/* Which limits apply is a property of the plan, so say so here
                rather than leaving the window rows to be interpreted blind. */}
            <span>
              {windows.length >= 2
                ? `${windows.map((w) => w.shortLabel).join(" + ")} 双限制`
                : windows.length === 1
                  ? windows[0].label
                  : "配额窗口未知"}
            </span>
          </p>
        )}
      </CardHeader>

      <CardContent className="flex flex-1 flex-col space-y-3">
        {!provider.enabled && (
          <div className="rounded-md border border-dashed px-3 py-2 text-xs text-muted-foreground">
            已关闭模型调用，配额与重置时间仍会更新。
          </div>
        )}

        {isLoading ? (
          <div className="py-4 text-center text-sm text-muted-foreground">加载配额中…</div>
        ) : quota ? (
          <>
            {/*
              Headline: the window that is actually closest to cutting you off.
              This used to read `remaining_quota`, which the backend derives
              from the primary window alone — on Plus and Team that is the
              5-hour window, so a nearly-exhausted weekly allowance was
              invisible behind a full bar.
            */}
            {binding ? (
              <div>
                <div className="mb-1 flex items-end justify-between gap-2">
                  <span className="min-w-0 text-xs text-muted-foreground">
                    剩余额度
                    {/* Space before the label: the labels start with a digit
                        for the hourly windows, and "受限于5 小时限制" reads as
                        one run-on token without it. */}
                    <span className="ml-1 text-foreground/70">· 受限于 {binding.label}</span>
                  </span>
                  <span className={cn("shrink-0 text-2xl font-bold tracking-tight tabular-nums", usageTone(binding.window.used_percent))}>
                    {(100 - binding.window.used_percent).toFixed(1)}%
                  </span>
                </div>
                <div className="h-2 overflow-hidden rounded-full bg-muted">
                  <motion.div
                    className={cn("h-2 rounded-full", usageBarTone(binding.window.used_percent))}
                    initial={{ width: 0 }}
                    animate={{ width: `${Math.max(0, 100 - binding.window.used_percent)}%` }}
                    transition={{ duration: 0.6, ease: [0.16, 1, 0.3, 1] }}
                  />
                </div>
                <div className="mt-1 text-[11px] text-muted-foreground">
                  已用 {binding.window.used_percent.toFixed(1)}%
                  {windows.length >= 2 && " · 两个窗口中更紧的那个"}
                </div>
              </div>
            ) : (
              <div className="rounded-md border border-dashed px-3 py-2 text-xs text-muted-foreground">
                上游未返回配额窗口信息。
              </div>
            )}

            {/* Every window the account has, shortest period first, each with
                its own live countdown. */}
            {windows.length > 0 && (
              <div className="space-y-2">
                {windows.map((w) => {
                  const left = countdownFor(w.window.reset_after_seconds)
                  const total = w.window.limit_window_seconds ?? 0
                  const elapsedRatio = total > 0 && left != null ? 1 - left / total : 0
                  return (
                    <div key={w.slot} className="rounded-lg border p-2.5">
                      <div className="flex items-baseline justify-between gap-2">
                        <span className="flex min-w-0 items-center gap-1.5 text-xs font-medium">
                          {w.label}
                          {binding?.slot === w.slot && windows.length >= 2 && (
                            <Badge variant={w.window.used_percent >= 90 ? "danger" : "warning"}>当前瓶颈</Badge>
                          )}
                        </span>
                        <span className={cn("shrink-0 text-sm font-semibold tabular-nums", usageTone(w.window.used_percent))}>
                          {w.window.used_percent.toFixed(1)}% 已用
                        </span>
                      </div>
                      <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
                        <motion.div
                          className={cn("h-1.5 rounded-full", usageBarTone(w.window.used_percent))}
                          initial={{ width: 0 }}
                          animate={{ width: `${Math.min(100, w.window.used_percent)}%` }}
                          transition={{ duration: 0.5, ease: [0.16, 1, 0.3, 1] }}
                        />
                      </div>
                      <div className="mt-1.5 flex flex-wrap items-center justify-between gap-x-3 gap-y-1 text-[10px] text-muted-foreground">
                        {left != null ? (
                          <span className="flex items-center gap-1">
                            <Clock className="h-3 w-3" />
                            {formatResetDuration(left)}后重置
                            {total > 0 && <span className="opacity-60">（窗口已过 {Math.round(elapsedRatio * 100)}%）</span>}
                          </span>
                        ) : (
                          <span />
                        )}
                        {w.window.reset_at && <span>{formatDate(w.window.reset_at)}</span>}
                      </div>
                    </div>
                  )
                })}
              </div>
            )}

            {/* Only Plus/Team-style accounts carry two windows; make the
                consequence of that explicit when one of them is nearly spent. */}
            {binding && binding.window.used_percent >= 90 && (
              <div className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                <span>
                  {binding.label} 已用 {binding.window.used_percent.toFixed(1)}%
                  {countdownFor(binding.window.reset_after_seconds) != null &&
                    `，${formatResetDuration(countdownFor(binding.window.reset_after_seconds)!)}后才会重置`}
                  。达限后该认证会被跳过。
                </span>
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

        {/* mt-auto anchors the footer to the bottom of the (now equal-height)
            card, so the 参与调用 switch and 刷新 line up across cards. */}
        <div className="mt-auto flex items-center justify-between border-t pt-3">
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
