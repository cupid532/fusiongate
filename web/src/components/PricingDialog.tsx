import { useEffect, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

export function PricingDialog({
  open,
  onOpenChange,
  model,
  routes,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  model: string
  routes: { input_price_micros: number; cached_price_micros: number; output_price_micros: number; long_context_threshold: number }[]
}) {
  const qc = useQueryClient()
  const [form, setForm] = useState({ input: 0, cached: 0, output: 0, threshold: 0 })

  useEffect(() => {
    if (open && routes.length > 0) {
      const r = routes[0]
      setForm({
        input: r.input_price_micros / 1_000_000,
        cached: r.cached_price_micros / 1_000_000,
        output: r.output_price_micros / 1_000_000,
        threshold: r.long_context_threshold,
      })
    }
  }, [open, routes])

  const save = useMutation({
    mutationFn: async () =>
      api(`/api/admin/models/${encodeURIComponent(model)}/pricing`, {
        method: "PATCH",
        body: JSON.stringify({
          input_price_micros: Math.round(form.input * 1_000_000),
          cached_price_micros: Math.round(form.cached * 1_000_000),
          output_price_micros: Math.round(form.output * 1_000_000),
          long_context_threshold: form.threshold,
          long_input_price_micros: Math.round(form.input * 1_000_000),
          long_cached_price_micros: Math.round(form.cached * 1_000_000),
          long_output_price_micros: Math.round(form.output * 1_000_000),
        }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["routes"] })
      onOpenChange(false)
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>定价 · {model}</DialogTitle>
          <DialogDescription>每百万 Token 的计价（USD），应用到该模型的所有路由。</DialogDescription>
        </DialogHeader>

        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col gap-1.5">
            <Label>输入（$ / 1M）</Label>
            <Input type="number" step="0.001" value={form.input} onChange={(e) => setForm((f) => ({ ...f, input: Number(e.target.value) }))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>输出（$ / 1M）</Label>
            <Input type="number" step="0.001" value={form.output} onChange={(e) => setForm((f) => ({ ...f, output: Number(e.target.value) }))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>缓存（$ / 1M）</Label>
            <Input type="number" step="0.001" value={form.cached} onChange={(e) => setForm((f) => ({ ...f, cached: Number(e.target.value) }))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>长上下文阈值（token）</Label>
            <Input type="number" value={form.threshold} onChange={(e) => setForm((f) => ({ ...f, threshold: Number(e.target.value) }))} />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
