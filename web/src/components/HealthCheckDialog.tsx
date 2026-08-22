import { useEffect, useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { ProviderKey, Route } from "@/lib/types"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"

type HealthItem = {
  provider_name: string
  public_name?: string
  model?: string
  provider_key_hint?: string
  status: string
  latency_ms: number
  model_count: number
  error?: string
}

type HealthJob = {
  id: string
  status: string
  total: number
  completed: number
  healthy: number
  failed: number
  skipped: number
  results: HealthItem[]
}

export function HealthCheckDialog({
  open,
  onOpenChange,
  providerId,
  providerName,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerId: number
  providerName: string
}) {
  const [job, setJob] = useState<HealthJob | null>(null)
  const [error, setError] = useState("")
  const [selectedModels, setSelectedModels] = useState<Set<number>>(new Set())
  const [selectedKeys, setSelectedKeys] = useState<Set<number>>(new Set())
  const [starting, setStarting] = useState(false)

  const { data: routes = [] } = useQuery({
    queryKey: ["routes"],
    queryFn: () => api<Route[]>("/api/admin/routes"),
    enabled: open,
  })
  const { data: keys = [] } = useQuery({
    queryKey: ["provider-keys", providerId],
    queryFn: () => api<ProviderKey[]>(`/api/admin/providers/${providerId}/keys`),
    enabled: open,
  })

  const models = useMemo(
    () =>
      routes
        .filter((r) => r.provider_id === providerId && r.enabled && r.provider_enabled && (r.capabilities ?? "").includes("chat"))
        .map((r) => ({ id: r.id, name: r.public_name, upstream: r.upstream_model })),
    [routes, providerId]
  )

  useEffect(() => {
    if (open) {
      setJob(null)
      setError("")
      setSelectedModels(new Set())
      setSelectedKeys(new Set())
    }
  }, [open])

  useEffect(() => {
    const id = job?.id
    if (id == null) return
    let timer: ReturnType<typeof setTimeout>
    const poll = async () => {
      try {
        const cur = await api<HealthJob>(`/api/admin/health-checks/${id}`)
        setJob(cur)
        if (cur.status === "running" || cur.status === "queued") {
          timer = setTimeout(poll, 1500)
        }
      } catch {
        /* stop polling */
      }
    }
    timer = setTimeout(poll, 1500)
    return () => clearTimeout(timer)
  }, [job?.id])

  async function start(scope: "all" | "selected") {
    setStarting(true)
    setError("")
    setJob(null)
    try {
      const body: Record<string, unknown> = { provider_ids: [providerId], model_scope: scope }
      if (scope === "selected") {
        body.route_ids = [...selectedModels]
        if (selectedKeys.size > 0) body.provider_key_ids = [...selectedKeys]
      }
      const j = await api<HealthJob>("/api/admin/health-checks", { method: "POST", body: JSON.stringify(body) })
      setJob(j)
    } catch (e) {
      setError(e instanceof Error ? e.message : "检活失败")
    } finally {
      setStarting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[88vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>模型检活 · {providerName}</DialogTitle>
          <DialogDescription>勾选指定模型检活，或一键检活全部；多 Key 时可指定 Key 对应模型。</DialogDescription>
        </DialogHeader>

        {!job && (
          <>
            {keys.length > 0 && (
              <div>
                <div className="mb-2 text-xs font-medium text-muted-foreground">Key（可选，留空测活所有 Key）</div>
                <div className="flex flex-wrap gap-1.5">
                  {keys.map((k) => (
                    <button
                      key={k.id}
                      onClick={() => {
                        const next = new Set(selectedKeys)
                        if (next.has(k.id)) next.delete(k.id)
                        else next.add(k.id)
                        setSelectedKeys(next)
                      }}
                      className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                        selectedKeys.has(k.id) ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {k.name || k.key_hint}
                    </button>
                  ))}
                </div>
              </div>
            )}

            <div>
              <div className="mb-2 flex items-center justify-between text-xs text-muted-foreground">
                <span>
                  已选 <span className="font-semibold text-primary">{selectedModels.size}</span> / {models.length} 个模型
                </span>
                <div className="flex gap-2">
                  <button className="text-primary hover:underline" onClick={() => setSelectedModels(new Set(models.map((m) => m.id)))}>
                    全选
                  </button>
                  <button className="text-muted-foreground hover:underline" onClick={() => setSelectedModels(new Set())}>
                    清空
                  </button>
                </div>
              </div>
              <div className="max-h-[40vh] space-y-1 overflow-y-auto rounded-lg border p-2">
                {models.map((m) => (
                  <label key={m.id} className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2 hover:bg-muted/40">
                    <input
                      type="checkbox"
                      checked={selectedModels.has(m.id)}
                      onChange={(e) => {
                        const next = new Set(selectedModels)
                        if (e.target.checked) next.add(m.id)
                        else next.delete(m.id)
                        setSelectedModels(next)
                      }}
                    />
                    <span className="font-mono text-sm">{m.name}</span>
                    <span className="text-xs text-muted-foreground">上游 {m.upstream}</span>
                  </label>
                ))}
              </div>
            </div>

            {error && <div className="text-sm text-destructive">{error}</div>}

            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                取消
              </Button>
              <Button variant="outline" onClick={() => start("selected")} disabled={selectedModels.size === 0 || starting}>
                测活选中
              </Button>
              <Button onClick={() => start("all")} disabled={starting}>
                {starting ? "启动中…" : "一键测活全部"}
              </Button>
            </div>
          </>
        )}

        {job && (
          <>
            <div className="flex items-center gap-3">
              {job.status === "running" || job.status === "queued" ? (
                <Badge variant="warning">
                  检测中 {job.completed}/{job.total}
                </Badge>
              ) : (
                <Badge variant="success">完成</Badge>
              )}
              <span className="text-xs text-muted-foreground">
                健康 {job.healthy} · 失败 {job.failed} · 跳过 {job.skipped}
              </span>
            </div>
            <div className="max-h-[50vh] space-y-1 overflow-y-auto rounded-lg border p-2">
              {job.results.map((r, i) => (
                <div key={i} className="flex items-center gap-3 rounded-lg px-3 py-2 hover:bg-muted/40">
                  <span className="flex-1">
                    <span className="block text-sm font-medium">{r.public_name || r.model || r.provider_name}</span>
                    {r.provider_key_hint && <span className="block text-xs text-muted-foreground">Key：{r.provider_key_hint}</span>}
                    {r.error && <span className="block text-xs text-destructive">{r.error}</span>}
                  </span>
                  <span className="text-xs text-muted-foreground">{r.latency_ms} ms · {r.model_count} 模型</span>
                  {r.status === "healthy" ? (
                    <Badge variant="success">健康</Badge>
                  ) : r.status === "unhealthy" ? (
                    <Badge variant="danger">不健康</Badge>
                  ) : (
                    <Badge variant="neutral">{r.status}</Badge>
                  )}
                </div>
              ))}
            </div>
            <div className="flex justify-end gap-2">
              <Button
                variant="outline"
                onClick={() => {
                  setJob(null)
                  setError("")
                }}
              >
                返回
              </Button>
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                关闭
              </Button>
            </div>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
