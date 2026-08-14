import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, RefreshCw, Search } from "lucide-react"
import { api } from "@/lib/api"
import type { Provider } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"

type Filter = "all" | "enabled" | "disabled" | "archived"

const typeLabels: Record<string, string> = {
  openai: "OpenAI",
  grok: "Grok",
  openrouter: "OpenRouter",
  openai_compatible: "OpenAI 兼容",
  anthropic: "Anthropic",
  claude_oauth: "Claude OAuth",
  codex_oauth: "Codex OAuth",
  grok_oauth: "Grok OAuth",
  gemini: "Gemini",
  opencode: "OpenCode",
}

function statusBadge(p: Provider) {
  if (p.archived) return <Badge variant="neutral">归档</Badge>
  if (!p.enabled) return <Badge variant="neutral">已停用</Badge>
  if (p.status === "circuit_open") return <Badge variant="warning">熔断冷却</Badge>
  if (p.health_check_status === "unhealthy") return <Badge variant="danger">不健康</Badge>
  if (p.health_check_status === "healthy") return <Badge variant="success">健康</Badge>
  if (p.consecutive_failures > 0) return <Badge variant="warning">不稳定</Badge>
  return <Badge variant="success">运行中</Badge>
}

export function Providers() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState<Filter>("all")
  const [q, setQ] = useState("")

  const { data: providers = [], isLoading, refetch, isFetching } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })

  const update = useMutation({
    mutationFn: async ({ id, patch }: { id: number; patch: Record<string, unknown> }) =>
      api(`/api/admin/providers/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers"] }),
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/providers/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers"] }),
  })

  const filtered = useMemo(() => {
    let list = providers
    if (filter === "enabled") list = list.filter((p) => p.enabled && !p.archived)
    else if (filter === "disabled") list = list.filter((p) => !p.enabled && !p.archived)
    else if (filter === "archived") list = list.filter((p) => p.archived)
    else list = list.filter((p) => !p.archived)
    if (q.trim()) {
      const kw = q.trim().toLowerCase()
      list = list.filter((p) => p.name.toLowerCase().includes(kw) || p.base_url.toLowerCase().includes(kw))
    }
    return list
  }, [providers, filter, q])

  const counts = useMemo(
    () => ({
      all: providers.filter((p) => !p.archived).length,
      enabled: providers.filter((p) => p.enabled && !p.archived).length,
      disabled: providers.filter((p) => !p.enabled && !p.archived).length,
      archived: providers.filter((p) => p.archived).length,
    }),
    [providers]
  )

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">上游渠道</h1>
          <p className="mt-1 text-sm text-muted-foreground">管理 API 渠道、优先级与全局故障转移。</p>
        </div>
        <Button onClick={() => void refetch()}>
          <Plus className="h-4 w-4" />
          添加渠道
        </Button>
      </div>

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b p-3">
            <div className="flex flex-wrap gap-1.5">
              {(
                [
                  ["all", "全部", counts.all],
                  ["enabled", "参与调度", counts.enabled],
                  ["disabled", "已停用", counts.disabled],
                  ["archived", "归档", counts.archived],
                ] as [Filter, string, number][]
              ).map(([f, label, n]) => (
                <button
                  key={f}
                  onClick={() => setFilter(f)}
                  className={cn(
                    "flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs font-medium transition-colors",
                    filter === f
                      ? "border-primary bg-primary/10 text-primary"
                      : "text-muted-foreground hover:text-foreground"
                  )}
                >
                  {label}
                  <span className="rounded-full bg-muted px-1.5 text-[10px]">{n}</span>
                </button>
              ))}
            </div>
            <div className="flex items-center gap-2">
              <div className="relative">
                <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索渠道" className="h-8 w-52 pl-8 text-xs" />
              </div>
              <Button variant="ghost" size="icon" onClick={() => void refetch()} aria-label="刷新">
                <RefreshCw className={cn("h-4 w-4", isFetching && "animate-spin")} />
              </Button>
            </div>
          </div>

          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : filtered.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">没有符合条件的渠道</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">渠道</th>
                    <th className="px-4 py-3 font-medium">类型</th>
                    <th className="px-4 py-3 font-medium">优先级</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">模型</th>
                    <th className="px-4 py-3 text-right font-medium">开关 / 操作</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map((p) => (
                    <tr key={p.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3">
                        <div className="font-medium">{p.name}</div>
                        <div className="truncate text-xs text-muted-foreground">{p.base_url}</div>
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{typeLabels[p.type] ?? p.type}</td>
                      <td className="px-4 py-3 text-xs">{p.priority}</td>
                      <td className="px-4 py-3">{statusBadge(p)}</td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{p.model_count} 个</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-2">
                          <Switch
                            checked={p.enabled}
                            onCheckedChange={(v) => update.mutate({ id: p.id, patch: { enabled: v } })}
                            aria-label={`${p.name} 开关`}
                          />
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              if (confirm(`删除渠道「${p.name}」？`)) remove.mutate(p.id)
                            }}
                            aria-label={`删除 ${p.name}`}
                          >
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
                      </td>
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
