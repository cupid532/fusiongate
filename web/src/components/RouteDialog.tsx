import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { Provider } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

export function RouteDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (value: boolean) => void }) {
  const qc = useQueryClient()
  const [form, setForm] = useState({ provider_id: 0, public_name: "", upstream_model: "", capabilities: "chat,stream,tools", priority: 0, enabled: true })
  const { data: providers = [] } = useQuery({ queryKey: ["providers"], queryFn: () => api<Provider[]>("/api/admin/providers") })
  const activeProviders = providers.filter((provider) => provider.enabled && !provider.archived)
  const firstProviderID = activeProviders[0]?.id ?? 0

  useEffect(() => {
    if (!open) return
    setForm({ provider_id: firstProviderID, public_name: "", upstream_model: "", capabilities: "chat,stream,tools", priority: 0, enabled: true })
  }, [open, firstProviderID])

  const save = useMutation({
    mutationFn: () => api("/api/admin/routes", { method: "POST", body: JSON.stringify(form) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["routes"] })
      qc.invalidateQueries({ queryKey: ["pricing"] })
      onOpenChange(false)
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>添加模型路由</DialogTitle>
          <DialogDescription>把一个渠道的真实模型加入公开模型故障转移组。</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5 sm:col-span-2">
            <Label>上游渠道</Label>
            <select value={form.provider_id} onChange={(event) => setForm((value) => ({ ...value, provider_id: Number(event.target.value) }))} className="h-9 rounded-md border border-input bg-transparent px-3 text-sm">
              <option value={0}>选择渠道</option>
              {activeProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name} · {provider.type}</option>)}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>公开模型名</Label>
            <Input value={form.public_name} onChange={(event) => setForm((value) => ({ ...value, public_name: event.target.value }))} placeholder="glm5-2" className="font-mono" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>上游模型名</Label>
            <Input value={form.upstream_model} onChange={(event) => setForm((value) => ({ ...value, upstream_model: event.target.value }))} placeholder="glm-5.2" className="font-mono" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>能力</Label>
            <Input value={form.capabilities} onChange={(event) => setForm((value) => ({ ...value, capabilities: event.target.value }))} placeholder="chat,stream,tools" className="font-mono text-xs" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>组内优先级</Label>
            <Input type="number" min={0} value={form.priority} onChange={(event) => setForm((value) => ({ ...value, priority: Number(event.target.value) }))} />
          </div>
          <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
            <div><div className="text-sm font-medium">立即参与调度</div><div className="text-xs text-muted-foreground">关闭后保留路由，但不接收请求。</div></div>
            <Switch checked={form.enabled} onCheckedChange={(enabled) => setForm((value) => ({ ...value, enabled }))} />
          </div>
          {save.error && <div className="rounded-lg bg-destructive/10 px-3 py-2 text-sm text-destructive sm:col-span-2">{save.error.message}</div>}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>取消</Button>
          <Button onClick={() => save.mutate()} disabled={!form.provider_id || !form.public_name.trim() || !form.upstream_model.trim() || save.isPending}>{save.isPending ? "创建中…" : "创建路由"}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
