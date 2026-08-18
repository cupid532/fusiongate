import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { ShieldCheck, Play, Square, ListChecks, ChevronDown, ChevronRight } from "lucide-react"
import { api } from "@/lib/api"
import type { QualityDetectorData, QualityDetectorTarget, QualityJob, QualityJobItem, QualityJobListResponse } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

const presetLabels: Record<string, string> = { low: "低", medium: "中", high: "高" }

function jobStatusBadge(status: string) {
  switch (status) {
    case "running":
      return <Badge variant="warning">进行中</Badge>
    case "queued":
      return <Badge variant="warning">排队中</Badge>
    case "cancelling":
      return <Badge variant="warning">取消中</Badge>
    case "completed":
      return <Badge variant="success">完成</Badge>
    case "cancelled":
      return <Badge variant="neutral">已取消</Badge>
    case "interrupted":
      return <Badge variant="neutral">已中断</Badge>
    default:
      return <Badge variant="neutral">{status}</Badge>
  }
}

function itemStatusBadge(item: QualityJobItem) {
  switch (item.status) {
    case "completed":
      return <Badge variant="success">{item.verdict || "通过"}</Badge>
    case "failed":
      return <Badge variant="danger">失败</Badge>
    case "skipped":
      return <Badge variant="neutral">跳过</Badge>
    case "cancelled":
      return <Badge variant="neutral">已取消</Badge>
    case "interrupted":
      return <Badge variant="neutral">已中断</Badge>
    case "running":
      return <Badge variant="warning">进行中</Badge>
    default:
      return <Badge variant="neutral">排队中</Badge>
  }
}

function fmt(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M"
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k"
  return String(n)
}

function keyComposite(providerId: number, keyId: number): string {
  return `${providerId}:${keyId}`
}

export function Quality() {
  const qc = useQueryClient()

  const { data: meta, isLoading } = useQuery({
    queryKey: ["quality"],
    queryFn: () => api<QualityDetectorData>("/api/admin/quality-detector"),
    refetchInterval: 10000,
  })

  const activeJobId = meta?.active_job_id ?? ""
  const { data: activeJob } = useQuery({
    queryKey: ["quality-job", activeJobId],
    queryFn: () => api<QualityJob>(`/api/admin/quality-detector/jobs/${activeJobId}`),
    enabled: !!activeJobId,
    refetchInterval: 2000,
  })

  const { data: jobsData } = useQuery({
    queryKey: ["quality-jobs"],
    queryFn: () => api<QualityJobListResponse>("/api/admin/quality-detector/jobs"),
    refetchInterval: 5000,
  })

  const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
  const [selectedProviders, setSelectedProviders] = useState<Set<number>>(new Set())
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set())
  const [selectedTargets, setSelectedTargets] = useState<Set<string>>(new Set())
  const [preset, setPreset] = useState<"low" | "medium" | "high">("low")
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [expandedJob, setExpandedJob] = useState<string | null>(null)
  const [expandedItem, setExpandedItem] = useState<number | null>(null)

  const targets = meta?.targets ?? []

  const models = useMemo(() => [...new Set(targets.map((t) => t.model))].sort(), [targets])

  const providers = useMemo(() => {
    const map = new Map<number, { id: number; name: string }>()
    for (const t of targets) {
      if (!map.has(t.provider_id)) map.set(t.provider_id, { id: t.provider_id, name: t.provider_name })
    }
    return [...map.values()].sort((a, b) => a.name.localeCompare(b.name))
  }, [targets])

  const keysByProvider = useMemo(() => {
    const map = new Map<number, { id: number; name: string; hint: string }[]>()
    for (const t of targets) {
      const list = map.get(t.provider_id) ?? []
      if (t.provider_key_id > 0) {
        if (!list.some((k) => k.id === t.provider_key_id)) list.push({ id: t.provider_key_id, name: t.provider_key_name, hint: t.provider_key_hint })
      } else if (!list.some((k) => k.id === 0)) {
        list.push({ id: 0, name: "渠道凭据", hint: "" })
      }
      map.set(t.provider_id, list)
    }
    return map
  }, [targets])

  const filteredTargets = useMemo(() => {
    return targets.filter((t) => {
      if (selectedModels.size > 0 && !selectedModels.has(t.model)) return false
      if (selectedProviders.size > 0 && !selectedProviders.has(t.provider_id)) return false
      if (selectedKeys.size > 0 && !selectedKeys.has(keyComposite(t.provider_id, t.provider_key_id))) return false
      return true
    })
  }, [targets, selectedModels, selectedProviders, selectedKeys])

  const estimate = meta?.estimates?.[preset]
  const perTargetRequests = estimate?.total_requests ?? 0
  const perTargetTokens = estimate?.approximate_fixed_32k_input_tokens ?? 0
  const totalRequests = perTargetRequests * selectedTargets.size
  const totalTokens = perTargetTokens * selectedTargets.size

  const allFilteredSelected = filteredTargets.length > 0 && filteredTargets.every((t) => selectedTargets.has(t.id))

  function toggleModel(model: string) {
    setSelectedModels((prev) => {
      const next = new Set(prev)
      if (next.has(model)) next.delete(model)
      else next.add(model)
      return next
    })
  }
  function toggleProvider(id: number) {
    setSelectedProviders((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  function toggleKey(composite: string) {
    setSelectedKeys((prev) => {
      const next = new Set(prev)
      if (next.has(composite)) next.delete(composite)
      else next.add(composite)
      return next
    })
  }
  function toggleTarget(id: string) {
    setSelectedTargets((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  function toggleAllFiltered() {
    setSelectedTargets((prev) => {
      const next = new Set(prev)
      if (allFilteredSelected) {
        filteredTargets.forEach((t) => next.delete(t.id))
      } else {
        filteredTargets.forEach((t) => next.add(t.id))
      }
      return next
    })
  }
  function clearSelections() {
    setSelectedModels(new Set())
    setSelectedProviders(new Set())
    setSelectedKeys(new Set())
    setSelectedTargets(new Set())
  }
  function quickDetect(target: QualityDetectorTarget) {
    setSelectedTargets(new Set([target.id]))
    setConfirmOpen(true)
  }

  const startMutation = useMutation({
    mutationFn: () =>
      api<QualityJob>("/api/admin/quality-detector/jobs", {
        method: "POST",
        body: JSON.stringify({ preset, target_ids: [...selectedTargets] }),
      }),
    onSuccess: () => {
      setConfirmOpen(false)
      qc.invalidateQueries({ queryKey: ["quality"] })
      qc.invalidateQueries({ queryKey: ["quality-jobs"] })
    },
  })

  const cancelMutation = useMutation({
    mutationFn: (id: string) => api<QualityJob>(`/api/admin/quality-detector/jobs/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["quality"] })
      qc.invalidateQueries({ queryKey: ["quality-jobs"] })
      qc.invalidateQueries({ queryKey: ["quality-job", activeJobId] })
    },
  })

  const jobs = jobsData?.jobs ?? []

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6">
        <h1 className="text-2xl font-bold tracking-tight">质量检测</h1>
        <p className="mt-1 text-sm text-muted-foreground">选择模型、渠道与 Key，对 GPT-5.6 上游做针对性生成质量探测。</p>
      </div>

      <Card className="mb-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheck className="h-5 w-5 text-primary" />
            检测器状态
          </CardTitle>
          <CardDescription>FusionGate 质量检测 sidecar 的运行状态。</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="text-sm text-muted-foreground">加载中…</div>
          ) : meta?.available ? (
            <div className="flex items-center gap-3">
              <Badge variant="success">可用</Badge>
              <span className="text-sm text-muted-foreground">版本 {meta.version}</span>
              <span className="text-sm text-muted-foreground">{targets.length} 个可检测目标</span>
            </div>
          ) : (
            <Badge variant="danger">不可用</Badge>
          )}
        </CardContent>
      </Card>

      {meta?.available && (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle className="text-base">检测目标</CardTitle>
            <CardDescription>模型、渠道、Key 联动筛选；只生成系统中真实存在的有效组合。</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">预设档位（冻结检测强度）</div>
              <div className="flex flex-wrap gap-2">
                {(["low", "medium", "high"] as const).map((p) => {
                  const est = meta.estimates?.[p]
                  return (
                    <button
                      key={p}
                      onClick={() => setPreset(p)}
                      className={`rounded-lg border px-3 py-1.5 text-sm transition-colors ${
                        preset === p ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      {presetLabels[p]} · {est?.total_requests ?? "—"} 次请求
                    </button>
                  )
                })}
              </div>
            </div>

            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">模型</div>
              <div className="flex flex-wrap gap-1.5">
                {models.map((m) => (
                  <button
                    key={m}
                    onClick={() => toggleModel(m)}
                    className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                      selectedModels.has(m) ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {m}
                  </button>
                ))}
              </div>
            </div>

            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">渠道</div>
              <div className="flex flex-wrap gap-1.5">
                {providers.map((p) => (
                  <button
                    key={p.id}
                    onClick={() => toggleProvider(p.id)}
                    className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                      selectedProviders.has(p.id) ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    {p.name}
                  </button>
                ))}
              </div>
            </div>

            <div>
              <div className="mb-2 text-xs font-medium text-muted-foreground">Key / 凭据（按渠道分组，可选精确指定）</div>
              <div className="space-y-2">
                {providers
                  .filter((p) => selectedProviders.size === 0 || selectedProviders.has(p.id))
                  .map((p) => {
                    const keys = keysByProvider.get(p.id) ?? []
                    if (keys.length === 0) return null
                    return (
                      <div key={p.id} className="rounded-lg border p-2">
                        <div className="mb-1 text-xs text-muted-foreground">{p.name}</div>
                        <div className="flex flex-wrap gap-1.5">
                          {keys.map((k) => {
                            const comp = keyComposite(p.id, k.id)
                            return (
                              <button
                                key={comp}
                                onClick={() => toggleKey(comp)}
                                className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                                  selectedKeys.has(comp) ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
                                }`}
                              >
                                {k.name || k.hint || "渠道凭据"}
                              </button>
                            )
                          })}
                        </div>
                      </div>
                    )
                  })}
              </div>
            </div>

            <div>
              <div className="mb-2 flex items-center justify-between text-xs text-muted-foreground">
                <span>
                  已选 <span className="font-semibold text-primary">{selectedTargets.size}</span> / {targets.length} 个目标
                </span>
                <div className="flex gap-2">
                  <button className="text-primary hover:underline" onClick={clearSelections}>
                    清空筛选
                  </button>
                </div>
              </div>
              <div className="overflow-x-auto rounded-lg border">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b text-left text-xs text-muted-foreground">
                      <th className="w-10 px-3 py-2.5">
                        <input
                          type="checkbox"
                          aria-label="选择当前筛选结果"
                          checked={allFilteredSelected}
                          onChange={toggleAllFiltered}
                        />
                      </th>
                      <th className="px-4 py-2.5 font-medium">模型</th>
                      <th className="px-4 py-2.5 font-medium">渠道</th>
                      <th className="px-4 py-2.5 font-medium">Key / 凭据</th>
                      <th className="px-4 py-2.5 font-medium">上游模型</th>
                      <th className="px-4 py-2.5 text-right font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredTargets.map((t) => (
                      <tr key={t.id} className="border-b last:border-0 hover:bg-muted/40">
                        <td className="px-3 py-3">
                          <input
                            type="checkbox"
                            aria-label={`选择 ${t.model} / ${t.provider_name}`}
                            checked={selectedTargets.has(t.id)}
                            onChange={() => toggleTarget(t.id)}
                          />
                        </td>
                        <td className="px-4 py-3 font-medium">{t.model}</td>
                        <td className="px-4 py-3 text-xs">{t.provider_name}</td>
                        <td className="px-4 py-3 text-xs text-muted-foreground">{t.provider_key_name || t.provider_key_hint}</td>
                        <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{t.upstream_model}</td>
                        <td className="px-4 py-3 text-right">
                          <Button variant="ghost" size="sm" onClick={() => quickDetect(t)}>
                            <Play className="h-4 w-4" />
                            检测
                          </Button>
                        </td>
                      </tr>
                    ))}
                    {filteredTargets.length === 0 && (
                      <tr>
                        <td colSpan={6} className="px-4 py-6 text-center text-sm text-muted-foreground">
                          没有匹配的目标，请调整筛选条件。
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
              </div>
            </div>

            <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3">
              <div className="text-xs text-muted-foreground">
                预估 {fmt(totalRequests)} 次请求
                {totalTokens > 0 && <> · 约 {fmt(totalTokens)} fixed-32k 输入 token</>}
              </div>
              <Button onClick={() => setConfirmOpen(true)} disabled={selectedTargets.size === 0 || startMutation.isPending}>
                <Play className="h-4 w-4" />
                开始检测（{selectedTargets.size}）
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {activeJob && (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle className="flex items-center justify-between text-base">
              <span className="flex items-center gap-2">
                <ListChecks className="h-5 w-5 text-primary" />
                当前批次
              </span>
              {jobStatusBadge(activeJob.status)}
            </CardTitle>
            <CardDescription>
              预设 {presetLabels[activeJob.preset] ?? activeJob.preset} · 进度 {activeJob.completed}/{activeJob.total}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              <span>通过 {activeJob.succeeded}</span>
              <span>失败 {activeJob.failed}</span>
              <span>跳过 {activeJob.skipped}</span>
              <span>取消 {activeJob.cancelled}</span>
            </div>
            <div className="h-2 w-full overflow-hidden rounded-full bg-muted">
              <div
                className="h-full bg-primary transition-all"
                style={{ width: `${activeJob.total ? (activeJob.completed / activeJob.total) * 100 : 0}%` }}
              />
            </div>
            {(activeJob.status === "running" || activeJob.status === "queued" || activeJob.status === "cancelling") && (
              <Button variant="destructive" size="sm" onClick={() => cancelMutation.mutate(activeJob.id)} disabled={cancelMutation.isPending}>
                <Square className="h-4 w-4" />
                停止
              </Button>
            )}
            {activeJob.items && activeJob.items.length > 0 && (
              <div className="max-h-[40vh] space-y-1 overflow-y-auto rounded-lg border p-2">
                {activeJob.items.map((item) => (
                  <div key={item.id} className="rounded-lg px-3 py-2 hover:bg-muted/40">
                    <div className="flex items-center gap-3">
                      <span className="flex-1 truncate text-sm font-medium">
                        {item.model} · {item.provider_name}
                      </span>
                      <span className="text-xs text-muted-foreground">{item.provider_key_name || item.provider_key_hint}</span>
                      {itemStatusBadge(item)}
                    </div>
                    {item.error && <div className="mt-1 text-xs text-destructive">{item.error}</div>}
                    {item.report && (
                      <button
                        className="mt-1 flex items-center gap-1 text-xs text-primary hover:underline"
                        onClick={() => setExpandedItem(expandedItem === item.id ? null : item.id)}
                      >
                        {expandedItem === item.id ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                        查看报告
                      </button>
                    )}
                    {expandedItem === item.id && item.report && (
                      <pre className="mt-2 max-h-64 overflow-auto rounded bg-muted p-2 text-xs">{prettyReport(item.report)}</pre>
                    )}
                  </div>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">检测历史（24 小时）</CardTitle>
          <CardDescription>脱敏报告仅保留最近 24 小时，不保存真实 Key 或检测令牌。</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {jobs.length === 0 ? (
            <div className="px-4 py-6 text-center text-sm text-muted-foreground">暂无检测记录。</div>
          ) : (
            <div className="divide-y">
              {jobs.map((job) => (
                <div key={job.id}>
                  <button
                    className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-muted/40"
                    onClick={() => setExpandedJob(expandedJob === job.id ? null : job.id)}
                  >
                    {expandedJob === job.id ? <ChevronDown className="h-4 w-4 shrink-0" /> : <ChevronRight className="h-4 w-4 shrink-0" />}
                    <span className="flex-1 text-sm">
                      {presetLabels[job.preset] ?? job.preset} 档 · {new Date(job.created_at).toLocaleString()}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      通过 {job.succeeded} · 失败 {job.failed} · 跳过 {job.skipped}
                    </span>
                    {jobStatusBadge(job.status)}
                  </button>
                  {expandedJob === job.id && (
                    <HistoryItems jobId={job.id} />
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle>开始质量检测</DialogTitle>
            <DialogDescription>确认将启动的定向检测任务。</DialogDescription>
          </DialogHeader>
          <div className="space-y-2 text-sm">
            <div className="flex justify-between">
              <span className="text-muted-foreground">预设档位</span>
              <span className="font-medium">{presetLabels[preset]}（{perTargetRequests} 次请求/目标）</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">目标数量</span>
              <span className="font-medium">{selectedTargets.size}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">预估总请求</span>
              <span className="font-medium">{fmt(totalRequests)}</span>
            </div>
            {totalTokens > 0 && (
              <div className="flex justify-between">
                <span className="text-muted-foreground">fixed-32k 输入 token</span>
                <span className="font-medium">约 {fmt(totalTokens)}</span>
              </div>
            )}
            {preset === "high" && (
              <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
                高档位单目标约 202 次请求并包含约 324 万 fixed-32k 输入 token，批量运行成本较高，请确认。
              </div>
            )}
            {startMutation.isError && <div className="text-xs text-destructive">{(startMutation.error as Error)?.message ?? "启动失败"}</div>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmOpen(false)}>
              取消
            </Button>
            <Button onClick={() => startMutation.mutate()} disabled={startMutation.isPending}>
              {startMutation.isPending ? "启动中…" : "确认启动"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.div>
  )
}

function prettyReport(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function HistoryItems({ jobId }: { jobId: string }) {
  const { data } = useQuery({
    queryKey: ["quality-job", jobId],
    queryFn: () => api<QualityJob>(`/api/admin/quality-detector/jobs/${jobId}`),
  })
  const [openItem, setOpenItem] = useState<number | null>(null)
  const items = data?.items ?? []
  if (items.length === 0) {
    return <div className="px-4 py-3 text-xs text-muted-foreground">加载中…</div>
  }
  return (
    <div className="space-y-1 bg-muted/30 px-4 py-2">
      {items.map((item) => (
        <div key={item.id} className="rounded-lg border bg-background px-3 py-2">
          <div className="flex items-center gap-3">
            <span className="flex-1 truncate text-sm font-medium">
              {item.model} · {item.provider_name}
            </span>
            <span className="text-xs text-muted-foreground">{item.provider_key_name || item.provider_key_hint}</span>
            {itemStatusBadge(item)}
          </div>
          {item.error && <div className="mt-1 text-xs text-destructive">{item.error}</div>}
          {item.report && (
            <button
              className="mt-1 flex items-center gap-1 text-xs text-primary hover:underline"
              onClick={() => setOpenItem(openItem === item.id ? null : item.id)}
            >
              {openItem === item.id ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
              查看报告
            </button>
          )}
          {openItem === item.id && item.report && (
            <pre className="mt-2 max-h-64 overflow-auto rounded bg-muted p-2 text-xs">{prettyReport(item.report)}</pre>
          )}
        </div>
      ))}
    </div>
  )
}
