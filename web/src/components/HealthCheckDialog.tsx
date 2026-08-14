import { useEffect, useState } from "react"
import { api } from "@/lib/api"
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
  status: string
  latency_ms: number
  model?: string
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
  can_cancel: boolean
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

  useEffect(() => {
    if (!open) return
    setJob(null)
    setError("")
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>

    async function run() {
      try {
        const j = await api<HealthJob>("/api/admin/health-checks", {
          method: "POST",
          body: JSON.stringify({ provider_ids: [providerId] }),
        })
        if (cancelled) return
        setJob(j)
        // 轮询任务状态直到完成
        const poll = async () => {
          if (cancelled) return
          const cur = await api<HealthJob>(`/api/admin/health-checks/${j.id}`)
          if (cancelled) return
          setJob(cur)
          if (cur.status === "running") {
            timer = setTimeout(poll, 1500)
          }
        }
        timer = setTimeout(poll, 1500)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : "检活失败")
      }
    }
    run()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [open, providerId])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>模型检活 · {providerName}</DialogTitle>
          <DialogDescription>对渠道的模型做健康检查。</DialogDescription>
        </DialogHeader>

        {error ? (
          <div className="py-8 text-center text-sm text-destructive">{error}</div>
        ) : !job ? (
          <div className="py-8 text-center text-sm text-muted-foreground">正在启动检活…</div>
        ) : (
          <>
            <div className="flex items-center gap-3">
              {job.status === "running" ? (
                <Badge variant="warning">检测中 {job.completed}/{job.total}</Badge>
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
          </>
        )}

        <div className="flex justify-end">
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            关闭
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
