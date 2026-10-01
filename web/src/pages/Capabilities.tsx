import { useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Ban, ArrowDownToLine, RefreshCw } from "lucide-react"
import { api } from "@/lib/api"
import type { BridgeFieldAuditResponse, BridgeFieldEvent } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { EmptyState } from "@/components/ui/empty-state"
import { QueryError } from "@/components/ui/query-error"
import { SegmentedTabs } from "@/components/ui/segmented-tabs"
import { cn } from "@/lib/utils"

type Filter = "all" | "refused" | "dropped"

const protocolLabels: Record<string, string> = {
  chat: "Chat Completions",
  responses: "Responses",
  messages: "Messages",
  gemini: "Gemini",
}

function protocolLabel(protocol: string): string {
  return protocolLabels[protocol] ?? protocol
}

function timeAgo(iso: string): string {
  if (!iso) return "—"
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 60_000) return "刚刚"
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  return `${Math.floor(diff / 86_400_000)} 天前`
}

/**
 * What the bridge policy is doing to each channel's fields.
 *
 * A client that starts sending a field no bridge can express is answered with a
 * 400 that names the field, and that used to be the whole diagnosis: answering
 * "which channel is being held back, and by which field" meant attaching a
 * capture proxy to a live client. This page reads the aggregate the gateway
 * keeps while it runs, so a new client field is a row here instead of a
 * debugging session.
 */
export function Capabilities() {
  const [filter, setFilter] = useState<Filter>("all")
  const { data, error, isFetching, refetch } = useQuery({
    queryKey: ["capabilities"],
    queryFn: () => api<BridgeFieldAuditResponse>("/api/admin/capabilities"),
    refetchInterval: 15_000,
  })

  const all = useMemo(() => data?.events ?? [], [data])
  const refused = useMemo(() => all.filter((event) => event.disposition === "refused"), [all])
  const dropped = useMemo(() => all.filter((event) => event.disposition === "dropped"), [all])
  const events = filter === "refused" ? refused : filter === "dropped" ? dropped : all
  const channels = useMemo(() => new Set(all.map((event) => event.provider_id)).size, [all])
  const refusedHits = useMemo(() => refused.reduce((sum, event) => sum + event.hits, 0), [refused])
  const droppedHits = useMemo(() => dropped.reduce((sum, event) => sum + event.hits, 0), [dropped])

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">字段兼容</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            桥接在每条渠道上丢弃或拒绝过哪些请求字段。被拒绝的字段会让该渠道无法服务这类客户端协议。
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching}>
          <RefreshCw className={cn("mr-1.5 h-3.5 w-3.5", isFetching && "animate-spin")} />
          刷新
        </Button>
      </div>

      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <SummaryTile label="受影响渠道" value={String(channels)} />
        <SummaryTile label="拒绝次数" value={String(refusedHits)} tone="destructive" />
        <SummaryTile label="丢弃次数" value={String(droppedHits)} />
        <SummaryTile label="条目" value={String(all.length)} />
      </div>

      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0">
          <div>
            <CardTitle className="text-base">字段决策</CardTitle>
            <CardDescription>
              {`进程启动以来累计，最多保留 ${data?.limit ?? 0} 条；重启后重新统计。`}
            </CardDescription>
          </div>
          <SegmentedTabs
            value={filter}
            onChange={setFilter}
            tabs={[
              { value: "all", label: "全部", count: all.length },
              { value: "refused", label: "拒绝", count: refused.length },
              { value: "dropped", label: "丢弃", count: dropped.length },
            ]}
          />
        </CardHeader>
        <CardContent>
          {error ? (
            <QueryError error={error} onRetry={() => refetch()} retrying={isFetching} />
          ) : events.length === 0 ? (
            <EmptyState
              title={all.length === 0 ? "还没有字段决策记录" : "当前筛选没有记录"}
              description={
                all.length === 0
                  ? "只有当请求经过协议桥接、并且有字段被丢弃或拒绝时，这里才会出现记录。"
                  : "换一个筛选条件试试。"
              }
            />
          ) : (
            <div className="space-y-2">
              {events.map((event, index) => (
                <FieldEventRow
                  key={`${event.provider_id}-${event.upstream_model}-${event.protocol}-${event.field}-${event.disposition}`}
                  event={event}
                  index={index}
                />
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}


function SummaryTile({ label, value, tone }: { label: string; value: string; tone?: "destructive" }) {
  return (
    <div className="rounded-xl border bg-card p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={cn("mt-1 text-lg font-semibold tabular-nums", tone === "destructive" && "text-destructive")}>{value}</div>
    </div>
  )
}

function FieldEventRow({ event, index }: { event: BridgeFieldEvent; index: number }) {
  const refused = event.disposition === "refused"
  return (
    <motion.div
      initial={{ opacity: 0, y: 4 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.2, delay: Math.min(index * 0.01, 0.2) }}
      className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border px-3 py-2.5"
    >
      <span
        className={cn(
          "grid h-7 w-7 shrink-0 place-items-center rounded-md",
          refused ? "bg-destructive/10 text-destructive" : "bg-muted text-muted-foreground",
        )}
        aria-hidden="true"
      >
        {refused ? <Ban className="h-3.5 w-3.5" /> : <ArrowDownToLine className="h-3.5 w-3.5" />}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <code className="rounded bg-muted px-1.5 py-0.5 text-xs font-medium">{event.field}</code>
          <Badge variant={refused ? "danger" : "neutral"} className="text-[10px]">
            {refused ? "拒绝" : "丢弃"}
          </Badge>
          <span className="text-xs text-muted-foreground">{protocolLabel(event.protocol)} 客户端</span>
        </div>
        <div className="mt-1 truncate text-xs text-muted-foreground">
          {event.provider_name}
          <span className="mx-1.5">·</span>
          {event.upstream_model || "—"}
          {event.reason ? (
            <>
              <span className="mx-1.5">·</span>
              {event.reason}
            </>
          ) : null}
        </div>
      </div>
      <div className="text-right">
        <div className="text-sm font-semibold tabular-nums">{event.hits}</div>
        <div className="text-[11px] text-muted-foreground">{timeAgo(event.last_seen_at)}</div>
      </div>
    </motion.div>
  )
}
