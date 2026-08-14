import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, Search } from "lucide-react"
import { api } from "@/lib/api"
import type { Route } from "@/lib/types"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"

export function Routes() {
  const qc = useQueryClient()
  const [q, setQ] = useState("")

  const { data: routes = [], isLoading } = useQuery({
    queryKey: ["routes"],
    queryFn: () => api<Route[]>("/api/admin/routes"),
  })

  const toggle = useMutation({
    mutationFn: async ({ id, enabled }: { id: number; enabled: boolean }) =>
      api(`/api/admin/routes/${id}`, { method: "PATCH", body: JSON.stringify({ enabled }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["routes"] }),
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/routes/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["routes"] }),
  })

  // 按 public_name 分组
  const groups = useMemo(() => {
    const map = new Map<string, Route[]>()
    for (const r of routes) {
      const list = map.get(r.public_name) ?? []
      list.push(r)
      map.set(r.public_name, list)
    }
    let entries = [...map.entries()]
    if (q.trim()) {
      const kw = q.trim().toLowerCase()
      entries = entries.filter(([name, list]) => name.toLowerCase().includes(kw) || list.some((r) => r.upstream_model.toLowerCase().includes(kw)))
    }
    return entries
  }, [routes, q])

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">模型路由</h1>
          <p className="mt-1 text-sm text-muted-foreground">把不同渠道的真实模型合并为统一故障转移组。</p>
        </div>
        <div className="flex items-center gap-2">
          <div className="relative">
            <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索模型" className="h-8 w-52 pl-8 text-xs" />
          </div>
          <Button>
            <Plus className="h-4 w-4" />
            添加路由
          </Button>
        </div>
      </div>

      {isLoading ? (
        <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
      ) : groups.length === 0 ? (
        <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">没有符合条件的路由</div>
      ) : (
        <div className="space-y-4">
          {groups.map(([name, list]) => (
            <Card key={name}>
              <CardContent className="p-0">
                <div className="border-b px-4 py-3">
                  <div className="flex items-center gap-2">
                    <span className="font-mono text-sm font-semibold">{name}</span>
                    <Badge variant="neutral">{list.length} 个上游</Badge>
                  </div>
                </div>
                <div className="divide-y">
                  {list.map((r) => (
                    <div key={r.id} className="flex items-center justify-between gap-4 px-4 py-2.5 hover:bg-muted/30">
                      <div className="min-w-0">
                        <div className="text-sm font-medium">{r.provider_name || "—"}</div>
                        <div className="truncate font-mono text-xs text-muted-foreground">{r.upstream_model}</div>
                      </div>
                      <div className="flex items-center gap-2 text-xs text-muted-foreground">
                        {r.health_check_status === "healthy" ? (
                          <Badge variant="success">健康</Badge>
                        ) : r.health_check_status === "unhealthy" ? (
                          <Badge variant="danger">不健康</Badge>
                        ) : (
                          <Badge variant="neutral">待检测</Badge>
                        )}
                        <span className="hidden sm:inline">{r.provider_latency_ms} ms</span>
                      </div>
                      <div className="flex items-center gap-1">
                        <Button
                          variant={r.enabled ? "outline" : "ghost"}
                          size="sm"
                          onClick={() => toggle.mutate({ id: r.id, enabled: !r.enabled })}
                        >
                          {r.enabled ? "已启用" : "已停用"}
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          onClick={() => {
                            if (confirm(`删除路由「${name} / ${r.upstream_model}」？`)) remove.mutate(r.id)
                          }}
                          aria-label="删除路由"
                        >
                          <Trash2 className="h-4 w-4 text-destructive" />
                        </Button>
                      </div>
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </motion.div>
  )
}
