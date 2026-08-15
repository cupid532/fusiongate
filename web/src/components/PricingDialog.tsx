import { useEffect, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"
import { api } from "@/lib/api"
import type { PricingSyncResult, Route } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

type PriceForm = { input: number; cached: number; output: number; threshold: number; longInput: number; longCached: number; longOutput: number }
const emptyForm: PriceForm = { input: 0, cached: 0, output: 0, threshold: 0, longInput: 0, longCached: 0, longOutput: 0 }
const dollars = (micros: number) => micros / 1_000_000
const micros = (dollars: number) => Math.round(dollars * 1_000_000)
const sourceLabel = (source?: string) => !source ? "未同步" : source === "manual" ? "手工定价" : source.includes("openrouter.ai") ? "OpenRouter 兜底" : "官网定价"

function PriceInput({ label, value, onChange }: { label: string; value: number; onChange: (value: number) => void }) {
  return <div className="flex flex-col gap-1.5"><Label>{label}</Label><Input type="number" min={0} step="0.001" value={value} onChange={(event) => onChange(Number(event.target.value))} /></div>
}

export function PricingDialog({ open, onOpenChange, model, routes }: { open: boolean; onOpenChange: (value: boolean) => void; model: string; routes: Route[] }) {
  const qc = useQueryClient()
  const [form, setForm] = useState<PriceForm>(emptyForm)
  const current = routes[0]
  useEffect(() => {
    if (!open || !current) return
    setForm({ input: dollars(current.input_price_micros), cached: dollars(current.cached_price_micros), output: dollars(current.output_price_micros), threshold: current.long_context_threshold, longInput: dollars(current.long_input_price_micros), longCached: dollars(current.long_cached_price_micros), longOutput: dollars(current.long_output_price_micros) })
  }, [open, current])

  const refresh = () => { qc.invalidateQueries({ queryKey: ["routes"] }); qc.invalidateQueries({ queryKey: ["pricing"] }) }
  const save = useMutation({
    mutationFn: () => api(`/api/admin/models/${encodeURIComponent(model)}/pricing`, { method: "PATCH", body: JSON.stringify({ input_price_micros: micros(form.input), cached_price_micros: micros(form.cached), output_price_micros: micros(form.output), long_context_threshold: form.threshold, long_input_price_micros: micros(form.longInput), long_cached_price_micros: micros(form.longCached), long_output_price_micros: micros(form.longOutput) }) }),
    onSuccess: () => { refresh(); onOpenChange(false) },
  })
  const official = useMutation({ mutationFn: () => api<{ updated_routes: number; sync: PricingSyncResult }>(`/api/admin/models/${encodeURIComponent(model)}/pricing/official`, { method: "POST" }), onSuccess: refresh })
  const error = save.error ?? official.error

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>定价 · {model}</DialogTitle>
          <DialogDescription>每百万 Token 的 USD 单价会应用到该公开模型的全部渠道。</DialogDescription>
        </DialogHeader>
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border bg-muted/25 px-3 py-2">
          <div className="min-w-0"><div className="flex items-center gap-2"><Badge variant={current?.pricing_source === "manual" ? "warning" : "success"}>{sourceLabel(current?.pricing_source)}</Badge><span className="text-xs text-muted-foreground">{current?.pricing_updated_at ? new Date(current.pricing_updated_at).toLocaleString() : "尚无更新时间"}</span></div>{current?.pricing_source && current.pricing_source !== "manual" && <div className="mt-1 truncate font-mono text-[11px] text-muted-foreground">{current.pricing_source}</div>}</div>
          <Button variant="outline" size="sm" onClick={() => official.mutate()} disabled={official.isPending}><RefreshCw className={official.isPending ? "animate-spin" : ""} />同步官网价格</Button>
        </div>
        <div>
          <div className="mb-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">标准上下文</div>
          <div className="grid gap-4 sm:grid-cols-3"><PriceInput label="输入（$ / 1M）" value={form.input} onChange={(input) => setForm((value) => ({ ...value, input }))} /><PriceInput label="缓存读取（$ / 1M）" value={form.cached} onChange={(cached) => setForm((value) => ({ ...value, cached }))} /><PriceInput label="输出（$ / 1M）" value={form.output} onChange={(output) => setForm((value) => ({ ...value, output }))} /></div>
        </div>
        <div className="rounded-xl border p-4">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2"><div><div className="text-sm font-semibold">长上下文价格</div><div className="text-xs text-muted-foreground">阈值为 0 时不启用长上下文计价。</div></div><div className="w-44"><Label>触发阈值（Token）</Label><Input type="number" min={0} value={form.threshold} onChange={(event) => setForm((value) => ({ ...value, threshold: Number(event.target.value) }))} /></div></div>
          <div className="grid gap-4 sm:grid-cols-3"><PriceInput label="长输入（$ / 1M）" value={form.longInput} onChange={(longInput) => setForm((value) => ({ ...value, longInput }))} /><PriceInput label="长缓存（$ / 1M）" value={form.longCached} onChange={(longCached) => setForm((value) => ({ ...value, longCached }))} /><PriceInput label="长输出（$ / 1M）" value={form.longOutput} onChange={(longOutput) => setForm((value) => ({ ...value, longOutput }))} /></div>
        </div>
        {official.data && <div className="rounded-lg bg-primary/10 px-3 py-2 text-sm text-primary">已从 {official.data.sync.sources} 个价格源匹配，更新 {official.data.updated_routes} 条路由。</div>}
        {error && <div className="rounded-lg bg-destructive/10 px-3 py-2 text-sm text-destructive">{error.message}</div>}
        <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>取消</Button><Button onClick={() => save.mutate()} disabled={save.isPending}>{save.isPending ? "保存中…" : "保存手工价格"}</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
