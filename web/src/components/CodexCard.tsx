import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { RefreshCw, Ticket, Zap } from "lucide-react"
import { api } from "@/lib/api"
import type { CodexAccountQuota, Provider } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"

function formatDate(iso?: string) {
  if (!iso) return "—"
  return new Date(iso).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

export function CodexCard({ provider }: { provider: Provider }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)

  const { data: quota, isLoading, refetch, isFetching } = useQuery({
    queryKey: ["codex-quota", provider.id],
    queryFn: () => api<CodexAccountQuota>(`/api/admin/auth/quota/${provider.id}`),
  })

  const redeem = useMutation({
    mutationFn: async (creditId?: string) =>
      api<{ redeemed?: unknown; quota?: CodexAccountQuota | null; warning?: string }>(
        `/api/admin/auth/quota/${provider.id}/reset`,
        { method: "POST", body: JSON.stringify(creditId ? { credit_id: creditId } : {}) }
      ),
    onSuccess: (res) => {
      if (res.warning) alert(`已兑换，但刷新配额失败：${res.warning}`)
      qc.invalidateQueries({ queryKey: ["codex-quota", provider.id] })
    },
  })

  const remaining = quota?.remaining_quota ?? 0
  const used = quota?.used_quota ?? 0

  return (
    <Card className="overflow-hidden">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center justify-between text-base">
          <span className="truncate">{provider.name}</span>
          <Badge variant={quota?.allowed === false ? "danger" : quota?.limit_reached ? "warning" : "success"}>
            {quota?.allowed === false ? "不可用" : quota?.limit_reached ? "已达限" : "可用"}
          </Badge>
        </CardTitle>
        {quota?.plan_type && <p className="text-xs text-muted-foreground">{quota.subscription_plan || quota.plan_type}</p>}
      </CardHeader>

      <CardContent className="space-y-3">
        {isLoading ? (
          <div className="py-4 text-center text-sm text-muted-foreground">加载配额中…</div>
        ) : quota ? (
          <>
            {/* 剩余额度进度条 */}
            <div>
              <div className="mb-1 flex items-end justify-between">
                <span className="text-xs text-muted-foreground">剩余额度</span>
                <span className="text-2xl font-bold tracking-tight">{remaining.toFixed(1)}%</span>
              </div>
              <div className="h-2 overflow-hidden rounded-full bg-muted">
                <motion.div
                  className={cn("h-2 rounded-full", remaining < 10 ? "bg-destructive" : remaining < 30 ? "bg-amber-500" : "bg-primary")}
                  initial={{ width: 0 }}
                  animate={{ width: `${remaining}%` }}
                  transition={{ duration: 0.6, ease: [0.16, 1, 0.3, 1] }}
                />
              </div>
              <div className="mt-1 text-[11px] text-muted-foreground">已用 {used.toFixed(1)}% · 总额度 100%</div>
            </div>

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
                {quota.reset_cards > 0 && (
                  <Button size="sm" variant="outline" disabled={redeem.isPending} onClick={() => redeem.mutate(undefined)}>
                    {redeem.isPending ? "兑换中…" : "兑换"}
                  </Button>
                )}
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
                              <Button size="sm" variant="ghost" disabled={redeem.isPending} onClick={() => redeem.mutate(c.id)}>
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

        <div className="flex justify-end">
          <Button variant="ghost" size="sm" onClick={() => void refetch()}>
            <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
            刷新
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
