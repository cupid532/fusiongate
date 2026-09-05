import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertTriangle, CheckCircle2, Loader2, MinusCircle, XCircle } from "lucide-react"
import { ApiError, healthChecksApi } from "@/lib/api"
import type { HealthCheckJob, HealthCheckRoutePreview } from "@/lib/types"
import { describeHealthReason, describeHealthStartError, describeHealthStatus } from "@/lib/health-check-messages"
import { cn } from "@/lib/utils"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"

/**
 * Manual model health check.
 *
 * Two modes share one dialog:
 *  - one provider: show every enabled route, which keys can probe it and — for
 *    the ones nothing can — why; let the operator pick routes/keys or run all.
 *  - several providers (batch from 认证文件 / 上游渠道 multi-select): start at
 *    once and show the live results. Before this the batch path only raised a
 *    toast pointing at a page that never listed these jobs, so the results
 *    were effectively invisible.
 *
 * The server runs one manual job at a time. Rather than surfacing the 409 as a
 * red "failed", the dialog detects the running job, shows its progress, and
 * offers to watch or cancel it.
 */

type Props = {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerIds: number[]
  title: string
  /** Batch mode: start a full check the moment the dialog opens. */
  autoStart?: boolean
}

const ACTIVE_STATES = new Set(["queued", "running", "cancelling"])

function isActive(job: HealthCheckJob | null | undefined): boolean {
  return !!job && ACTIVE_STATES.has(job.status)
}

function keyLabel(name?: string, hint?: string) {
  return name || hint || "默认"
}

export function HealthCheckDialog({ open, onOpenChange, providerIds, title, autoStart = false }: Props) {
  const qc = useQueryClient()
  const single = providerIds.length === 1 ? providerIds[0] : null

  const [job, setJob] = useState<HealthCheckJob | null>(null)
  const [error, setError] = useState("")
  const [starting, setStarting] = useState(false)
  const [selectedRoutes, setSelectedRoutes] = useState<Set<number>>(new Set())
  const [selectedKeys, setSelectedKeys] = useState<Set<number>>(new Set())
  const [problemsOnly, setProblemsOnly] = useState(false)
  // A job that is running but is not ours — the reason a start would be refused.
  const [blocker, setBlocker] = useState<HealthCheckJob | null>(null)
  const autoStarted = useRef(false)

  const preview = useQuery({
    queryKey: ["health-check-preview", single],
    queryFn: () => healthChecksApi.preview(single as number),
    enabled: open && single != null,
    staleTime: 0,
  })

  const invalidate = useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["providers"] })
    void qc.invalidateQueries({ queryKey: ["routes"] })
    for (const id of providerIds) void qc.invalidateQueries({ queryKey: ["provider-keys", id] })
  }, [qc, providerIds])

  useEffect(() => {
    if (!open) return
    setJob(null)
    setError("")
    setBlocker(null)
    setSelectedRoutes(new Set())
    setSelectedKeys(new Set())
    setProblemsOnly(false)
    autoStarted.current = false
  }, [open])

  // While we have no job of our own, keep an eye on the single manual slot so
  // the start buttons can say "busy" instead of failing on click.
  useEffect(() => {
    if (!open || job) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const tick = async () => {
      try {
        const res = await healthChecksApi.active()
        if (cancelled) return
        setBlocker(res.active && res.job ? res.job : null)
      } catch {
        /* keep the last known state */
      }
      if (!cancelled) timer = setTimeout(tick, 2000)
    }
    void tick()
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [open, job])

  // Poll our job until it settles, then refresh the pages that show health.
  useEffect(() => {
    const id = job?.id
    if (!id || !isActive(job)) return
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const poll = async () => {
      try {
        const cur = await healthChecksApi.get(id)
        if (cancelled) return
        setJob(cur)
        if (isActive(cur)) timer = setTimeout(poll, 1500)
        else invalidate()
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : "读取检活进度失败")
      }
    }
    timer = setTimeout(poll, 1200)
    return () => {
      cancelled = true
      if (timer) clearTimeout(timer)
    }
  }, [job?.id, job?.status, invalidate]) // eslint-disable-line react-hooks/exhaustive-deps

  const previewData = preview.data
  const routes: HealthCheckRoutePreview[] = useMemo(() => previewData?.routes ?? [], [previewData])
  const keys = useMemo(() => {
    const map = new Map<number, { id: number; label: string; enabled: boolean; healthEnabled: boolean; reason?: string }>()
    for (const route of routes) {
      for (const k of route.keys) {
        if (!map.has(k.key_id)) map.set(k.key_id, { id: k.key_id, label: keyLabel(k.name, k.hint), enabled: k.enabled, healthEnabled: k.health_check_enabled, reason: !k.enabled || !k.health_check_enabled ? k.reason : undefined })
      }
    }
    return [...map.values()]
  }, [routes])

  // A route is probeable under the current key filter when at least one
  // selected key (or any key, with no filter) can serve it.
  const routeProbeable = useCallback(
    (route: HealthCheckRoutePreview) => {
      if (!route.supported) return false
      if (selectedKeys.size === 0) return true
      return route.keys.some((k) => k.supported && selectedKeys.has(k.key_id))
    },
    [selectedKeys]
  )
  const probeableRoutes = useMemo(() => routes.filter(routeProbeable), [routes, routeProbeable])
  const unprobeableRoutes = useMemo(() => routes.filter((r) => !r.supported), [routes])

  const providerBlocked = preview.data ? (!preview.data.enabled ? "渠道已停用，无法检活" : !preview.data.health_check_enabled ? "该渠道已关闭检活，请先在渠道设置中开启" : "") : ""

  const start = useCallback(
    async (scope: "all" | "selected") => {
      setStarting(true)
      setError("")
      try {
        const body: Parameters<typeof healthChecksApi.start>[0] = { provider_ids: providerIds, model_scope: scope }
        if (scope === "selected") {
          const chosen = routes.filter((r) => selectedRoutes.has(r.route_id) && routeProbeable(r))
          body.route_ids = chosen.map((r) => r.route_id)
          if (selectedKeys.size > 0) {
            // Only send keys that actually serve a chosen route; the server
            // rejects a key selection that matches nothing.
            const used = new Set<number>()
            for (const r of chosen) for (const k of r.keys) if (k.supported && selectedKeys.has(k.key_id)) used.add(k.key_id)
            body.provider_key_ids = [...used]
          }
        }
        const created = await healthChecksApi.start(body)
        setBlocker(null)
        setJob(created)
      } catch (e) {
        if (e instanceof ApiError && e.code === "health_check_running") {
          try {
            const res = await healthChecksApi.active()
            setBlocker(res.active && res.job ? res.job : null)
          } catch {
            /* fall through to the message */
          }
        }
        setError(describeHealthStartError(e))
      } finally {
        setStarting(false)
      }
    },
    [providerIds, routes, selectedRoutes, selectedKeys, routeProbeable]
  )

  useEffect(() => {
    if (!open || !autoStart || autoStarted.current || job) return
    autoStarted.current = true
    void start("all")
  }, [open, autoStart, job, start])

  const cancel = async (target: HealthCheckJob) => {
    try {
      const next = await healthChecksApi.cancel(target.id)
      if (job?.id === target.id) setJob(next)
      else setBlocker(isActive(next) ? next : null)
    } catch (e) {
      setError(e instanceof Error ? e.message : "取消检活失败")
    }
  }

  const toggleRoute = (id: number, checked: boolean) => {
    setSelectedRoutes((prev) => {
      const next = new Set(prev)
      if (checked) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const results = useMemo(() => {
    const list = job?.results ?? []
    return problemsOnly ? list.filter((r) => r.status !== "healthy" && r.status !== "queued" && r.status !== "running") : list
  }, [job?.results, problemsOnly])

  const busy = starting || (blocker != null && !job)

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[88vh] max-w-3xl flex-col overflow-hidden">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {single != null ? "向每个模型发送一次极短的生成请求，验证 Key、路由与上游都真正可用。" : `对 ${providerIds.length} 个认证的全部已启用模型逐个探测。`}
          </DialogDescription>
        </DialogHeader>

        {!job && blocker && (
          <div className="flex flex-wrap items-center gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs">
            <Loader2 className="h-3.5 w-3.5 animate-spin text-amber-600" />
            <span className="min-w-0 flex-1">
              另一项检活正在进行（{blocker.completed}/{blocker.total}
              {blocker.results?.[0]?.provider_name ? `，${blocker.results[0].provider_name}${blocker.results.length > 1 ? " 等" : ""}` : ""}
              ），同一时间只能运行一项。
            </span>
            <Button size="sm" variant="outline" onClick={() => setJob(blocker)}>查看进度</Button>
            {blocker.can_cancel && <Button size="sm" variant="destructive" onClick={() => void cancel(blocker)}>取消它</Button>}
          </div>
        )}

        {!job && single != null && (
          <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-hidden">
            {preview.isLoading ? (
              <div className="py-8 text-center text-sm text-muted-foreground">正在读取模型与 Key…</div>
            ) : preview.isError ? (
              <div className="py-8 text-center text-sm text-destructive">{preview.error instanceof Error ? preview.error.message : "读取失败"}</div>
            ) : (
              <>
                {providerBlocked && (
                  <div className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
                    <XCircle className="h-3.5 w-3.5 shrink-0" />
                    {providerBlocked}
                  </div>
                )}

                {keys.length > 0 && (
                  <div>
                    <div className="mb-1.5 text-xs font-medium text-muted-foreground">Key 过滤（不选＝测所有可用 Key）</div>
                    <div className="flex flex-wrap gap-1.5">
                      {keys.map((k) => {
                        const selectable = k.enabled && k.healthEnabled
                        const on = selectedKeys.has(k.id)
                        return (
                          <button
                            key={k.id}
                            type="button"
                            disabled={!selectable}
                            title={!selectable ? describeHealthReason(k.reason) : undefined}
                            onClick={() => setSelectedKeys((prev) => { const next = new Set(prev); if (next.has(k.id)) next.delete(k.id); else next.add(k.id); return next })}
                            className={cn(
                              "rounded-lg border px-2.5 py-1 text-xs transition-colors",
                              on ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground",
                              !selectable && "cursor-not-allowed line-through opacity-50 hover:text-muted-foreground"
                            )}
                          >
                            {k.label}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                )}

                <div className="flex min-h-0 flex-1 flex-col">
                  <div className="mb-1.5 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span>
                      已选 <span className="font-semibold text-primary">{[...selectedRoutes].filter((id) => probeableRoutes.some((r) => r.route_id === id)).length}</span> / 可检活 {probeableRoutes.length} 个模型
                      {unprobeableRoutes.length > 0 && <span className="ml-1 text-amber-600">· {unprobeableRoutes.length} 个无法检活</span>}
                    </span>
                    <div className="flex gap-3">
                      <button type="button" className="text-primary hover:underline" onClick={() => setSelectedRoutes(new Set(probeableRoutes.map((r) => r.route_id)))}>全选可检活</button>
                      <button type="button" className="hover:underline" onClick={() => setSelectedRoutes(new Set())}>清空</button>
                    </div>
                  </div>
                  <div className="min-h-0 flex-1 space-y-1 overflow-y-auto rounded-lg border p-2">
                    {routes.length === 0 && (
                      <div className="py-6 text-center text-sm text-muted-foreground">该渠道还没有启用的模型，请先在「模型管理」中添加。</div>
                    )}
                    {routes.map((r) => {
                      const ok = routeProbeable(r)
                      const supportingKeys = r.keys.filter((k) => k.supported)
                      return (
                        <label
                          key={r.route_id}
                          className={cn("flex items-start gap-3 rounded-lg px-3 py-2", ok ? "cursor-pointer hover:bg-muted/40" : "opacity-70")}
                          title={!r.supported ? describeHealthReason(r.reason) : undefined}
                        >
                          <input type="checkbox" className="mt-1" disabled={!ok} checked={ok && selectedRoutes.has(r.route_id)} onChange={(e) => toggleRoute(r.route_id, e.target.checked)} />
                          <span className="min-w-0 flex-1">
                            <span className="flex flex-wrap items-baseline gap-x-2">
                              <span className="font-mono text-sm">{r.public_name}</span>
                              {r.upstream_model.toLowerCase() !== r.public_name.toLowerCase() && <span className="text-xs text-muted-foreground">上游 {r.upstream_model}</span>}
                            </span>
                            {!r.supported ? (
                              <span className="mt-0.5 flex items-start gap-1 text-xs text-amber-600">
                                <AlertTriangle className="mt-0.5 h-3 w-3 shrink-0" />
                                {describeHealthReason(r.reason)}
                              </span>
                            ) : r.keys.length > 0 ? (
                              <span className="mt-0.5 block text-[11px] text-muted-foreground">
                                {supportingKeys.map((k) => keyLabel(k.name, k.hint)).join(" · ")}
                                {!ok && <span className="ml-1 text-amber-600">（当前 Key 过滤下不可用）</span>}
                              </span>
                            ) : null}
                          </span>
                        </label>
                      )
                    })}
                  </div>
                </div>
              </>
            )}

            {error && <div className="text-sm text-destructive">{error}</div>}

            <div className="flex flex-wrap justify-end gap-2">
              <Button variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
              <Button
                variant="outline"
                onClick={() => void start("selected")}
                disabled={busy || !!providerBlocked || ![...selectedRoutes].some((id) => probeableRoutes.some((r) => r.route_id === id))}
              >
                测活选中
              </Button>
              <Button onClick={() => void start("all")} disabled={busy || !!providerBlocked || preview.isLoading || probeableRoutes.length === 0} title={probeableRoutes.length === 0 && !preview.isLoading ? "没有可检活的模型" : undefined}>
                {starting ? "启动中…" : blocker ? "等待中…" : "一键测活全部"}
              </Button>
            </div>
          </div>
        )}

        {!job && single == null && (
          <div className="py-6 text-center text-sm text-muted-foreground">
            {error ? <span className="text-destructive">{error}</span> : blocker ? "等待上一项检活结束…" : "正在启动检活…"}
            {error && (
              <div className="mt-4 flex justify-center gap-2">
                <Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button>
                <Button onClick={() => void start("all")} disabled={busy}>重试</Button>
              </div>
            )}
          </div>
        )}

        {job && (
          <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-hidden">
            <div className="flex flex-wrap items-center gap-3">
              {isActive(job) ? (
                <Badge variant="warning">
                  <Loader2 className="h-3 w-3 animate-spin" />
                  {job.status === "cancelling" ? "取消中" : "检测中"} {job.completed}/{job.total}
                </Badge>
              ) : job.status === "cancelled" ? (
                <Badge variant="neutral">已取消</Badge>
              ) : job.failed > 0 ? (
                <Badge variant="danger">{job.failed} 个失败</Badge>
              ) : job.healthy === 0 && job.skipped > 0 ? (
                <Badge variant="warning">全部跳过</Badge>
              ) : (
                <Badge variant="success">全部健康</Badge>
              )}
              <span className="flex items-center gap-3 text-xs text-muted-foreground">
                <span className="flex items-center gap-1"><CheckCircle2 className="h-3.5 w-3.5 text-emerald-600" />{job.healthy}</span>
                <span className="flex items-center gap-1"><XCircle className="h-3.5 w-3.5 text-destructive" />{job.failed}</span>
                <span className="flex items-center gap-1"><MinusCircle className="h-3.5 w-3.5" />{job.skipped}</span>
              </span>
              {(job.failed > 0 || job.skipped > 0) && (
                <label className="ml-auto flex items-center gap-1.5 text-xs text-muted-foreground">
                  <input type="checkbox" checked={problemsOnly} onChange={(e) => setProblemsOnly(e.target.checked)} />
                  仅看问题
                </label>
              )}
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-muted">
              <div className={cn("h-full rounded-full transition-all", job.failed > 0 ? "bg-destructive" : "bg-primary")} style={{ width: `${job.total ? (job.completed / job.total) * 100 : 0}%` }} />
            </div>

            <div className="min-h-0 flex-1 space-y-1 overflow-y-auto rounded-lg border p-2">
              {results.length === 0 && <div className="py-6 text-center text-sm text-muted-foreground">{problemsOnly ? "没有问题项" : "没有结果"}</div>}
              {results.map((r, i) => {
                const st = describeHealthStatus(r.status)
                return (
                  <div key={`${r.route_id}-${r.provider_key_id}-${i}`} className="flex items-start gap-3 rounded-lg px-3 py-2 hover:bg-muted/40">
                    <span className="min-w-0 flex-1">
                      <span className="flex flex-wrap items-baseline gap-x-2">
                        <span className="font-mono text-sm font-medium">{r.public_name || r.model || r.provider_name}</span>
                        {single == null && <span className="text-xs text-muted-foreground">{r.provider_name}</span>}
                        {r.model && r.public_name && r.model.toLowerCase() !== r.public_name.toLowerCase() && <span className="text-xs text-muted-foreground">上游 {r.model}</span>}
                      </span>
                      <span className="block text-[11px] text-muted-foreground">
                        Key {keyLabel(r.provider_key_name, r.provider_key_hint)}
                        {r.status !== "skipped" && r.status !== "queued" && r.latency_ms > 0 && ` · ${r.latency_ms} ms${r.first_byte_ms ? ` · 首字节 ${r.first_byte_ms} ms` : ""}`}
                      </span>
                      {r.error && <span className={cn("block break-words text-xs", r.status === "skipped" || r.status === "cancelled" ? "text-amber-600" : "text-destructive")}>{describeHealthReason(r.error)}</span>}
                    </span>
                    <Badge variant={st.tone}>
                      {(r.status === "running" || r.status === "queued") && <Loader2 className="h-3 w-3 animate-spin" />}
                      {st.label}
                    </Badge>
                  </div>
                )
              })}
            </div>

            {error && <div className="text-sm text-destructive">{error}</div>}

            <div className="flex flex-wrap justify-end gap-2">
              {isActive(job) && job.status !== "cancelling" && job.can_cancel && (
                <Button variant="destructive" onClick={() => void cancel(job)}>取消检活</Button>
              )}
              {single != null && !isActive(job) && (
                <Button variant="outline" onClick={() => { setJob(null); setError(""); void preview.refetch() }}>再测一次</Button>
              )}
              <Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
