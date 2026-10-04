import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api, providerKeysApi } from "@/lib/api"
import type { Provider, Route } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

type ModelSupport = { supported_key_count: number; available_key_count: number; provider_enabled: boolean; auth_kind: string; warning?: string }

export function RouteDialog({ open, onOpenChange, route, initialModel = "", newGroup = false }: { open: boolean; onOpenChange: (value: boolean) => void; route?: Route | null; initialModel?: string; newGroup?: boolean }) {
  const qc = useQueryClient()
  const [form, setForm] = useState({ provider_id: 0, public_name: "", upstream_model: "", capabilities: "chat,stream", priority: 0, enabled: true })
  const { data: providers = [] } = useQuery({ queryKey: ["providers"], queryFn: () => api<Provider[]>("/api/admin/providers"), enabled: open })
  const { data: routes = [] } = useQuery({ queryKey: ["routes"], queryFn: () => api<Route[]>("/api/admin/routes"), enabled: open })
  const activeProviders = providers.filter((provider) => provider.enabled && !provider.archived || provider.id === route?.provider_id)
  const modelGroups = [...new Set(routes.map((item) => item.public_name))].sort()
  const firstProviderID = activeProviders[0]?.id ?? 0
  const { data: providerKeys = [] } = useQuery({ queryKey: ["provider-keys", form.provider_id], queryFn: () => providerKeysApi.list(form.provider_id), enabled: open && form.provider_id > 0 })
  const models = useMemo(() => [...new Set(providerKeys.flatMap((key) => (key.models ?? []).map((model) => model.model)))].sort(), [providerKeys])
  const [previewModel, setPreviewModel] = useState("")
  useEffect(() => { const timer = setTimeout(() => setPreviewModel(form.upstream_model.trim()), 250); return () => clearTimeout(timer) }, [form.upstream_model])
  const preview = useQuery({ queryKey: ["route-preview", form.provider_id, previewModel], queryFn: ({ signal }) => api<ModelSupport>("/api/admin/routes/preview", { method: "POST", signal, body: JSON.stringify({ provider_id: form.provider_id, upstream_model: previewModel }) }), enabled: open && form.provider_id > 0 && !!previewModel })
  const currentPreview = previewModel === form.upstream_model.trim() && !!previewModel
  useEffect(() => {
    if (!open) return
    setForm(route ? { provider_id: route.provider_id, public_name: route.public_name, upstream_model: route.upstream_model, capabilities: route.capabilities, priority: route.priority, enabled: route.enabled } : { provider_id: 0, public_name: initialModel, upstream_model: "", capabilities: "chat,stream", priority: 0, enabled: true })
  }, [open, route, initialModel])
  useEffect(() => { if (open && !route && firstProviderID) setForm((value) => value.provider_id ? value : { ...value, provider_id: firstProviderID }) }, [open, route, firstProviderID])
  const save = useMutation({
    mutationFn: () => api(route ? `/api/admin/routes/${route.id}` : "/api/admin/routes", { method: route ? "PATCH" : "POST", body: JSON.stringify(route ? { public_name: form.public_name, upstream_model: form.upstream_model, capabilities: form.capabilities, priority: form.priority, enabled: form.enabled } : form) }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["routes"] }); qc.invalidateQueries({ queryKey: ["model-aliases"] }); qc.invalidateQueries({ queryKey: ["pricing"] }); onOpenChange(false) },
  })
  const duplicateGroup = newGroup && modelGroups.some((name) => name.toLowerCase() === form.public_name.trim().toLowerCase())
  return <Dialog open={open} onOpenChange={(value) => { if (!save.isPending) onOpenChange(value) }}>
    <DialogContent className="max-h-[90vh] max-w-xl overflow-y-auto">
      <DialogHeader><DialogTitle>{route ? "编辑模型路由" : newGroup ? "新建请求模型" : "添加渠道成员"}</DialogTitle><DialogDescription>请求模型名是客户端调用名称；上游模型名是实际发给该渠道的名称。保存不会扩大 Key 权限。</DialogDescription></DialogHeader>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5 sm:col-span-2"><Label htmlFor="route-provider">上游渠道</Label><select id="route-provider" disabled={!!route || save.isPending} value={form.provider_id} onChange={(event) => setForm((value) => ({ ...value, provider_id: Number(event.target.value) }))} className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"><option value={0}>选择渠道</option>{activeProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name} · {provider.type}</option>)}</select></div>
        <div className="flex flex-col gap-1.5"><Label htmlFor="route-public-name">请求模型名</Label><Input id="route-public-name" list="route-model-groups" value={form.public_name} onChange={(event) => setForm((value) => ({ ...value, public_name: event.target.value }))} placeholder="my-coding" className="font-mono" /><datalist id="route-model-groups">{modelGroups.map((model) => <option key={model} value={model} />)}</datalist><span className="text-xs text-muted-foreground">同一名称进入同一故障转移组。重命名整个组请使用“重命名”。</span>{duplicateGroup && <span role="alert" className="text-xs text-destructive">请求模型已存在，请改用“添加渠道成员”。</span>}</div>
        <div className="flex flex-col gap-1.5"><Label htmlFor="route-upstream-model">上游模型名</Label><Input id="route-upstream-model" list="route-upstream-models" value={form.upstream_model} onChange={(event) => setForm((value) => ({ ...value, upstream_model: event.target.value }))} placeholder="vendor/Code-Model" className="font-mono" /><datalist id="route-upstream-models">{models.map((model) => <option key={model} value={model} />)}</datalist><span className="text-xs text-muted-foreground">可选择已有模型或手动输入；保留上游名称大小写。</span></div>
        <div className="flex flex-col gap-1.5"><Label htmlFor="route-capabilities">能力</Label><Input id="route-capabilities" value={form.capabilities} onChange={(event) => setForm((value) => ({ ...value, capabilities: event.target.value }))} placeholder="chat,stream,tools" className="font-mono text-xs" /></div>
        <div className="flex flex-col gap-1.5"><Label htmlFor="route-priority">组内优先级</Label><Input id="route-priority" type="number" min={0} value={form.priority} onChange={(event) => setForm((value) => ({ ...value, priority: Number(event.target.value) }))} /></div>
        <div className="rounded-lg border p-3 text-xs sm:col-span-2"><div className="break-all font-mono">{form.public_name || "请求模型"} → {activeProviders.find((provider) => provider.id === form.provider_id)?.name || "渠道"} → {form.upstream_model || "上游模型"}</div>{!currentPreview || preview.isFetching ? <p className="mt-2 text-muted-foreground">输入模型名后核对支持情况。</p> : preview.isError ? <p role="alert" className="mt-2 text-destructive">无法核对 Key 支持情况：{preview.error.message}</p> : preview.data && <><p className="mt-2">{preview.data.auth_kind === "api_key" ? `${preview.data.supported_key_count} 张 Key 支持 · ${preview.data.available_key_count} 张当前可调度（不等于检活通过）` : "OAuth 模型支持未验证"}</p>{preview.data.warning && <p className="mt-1 text-amber-600">{preview.data.warning}</p>}{preview.data.auth_kind === "api_key" && preview.data.supported_key_count === 0 && <a href={`#providers?provider=${form.provider_id}&section=models`} onClick={() => onOpenChange(false)} className="mt-2 inline-block text-primary underline">去渠道模型管理添加到指定 Key</a>}</>}</div>
        <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2"><div><div className="text-sm font-medium">启用路由</div><div className="text-xs text-muted-foreground">仅支持该模型且可用的 Key 会被调度。</div></div><Switch checked={form.enabled} onCheckedChange={(enabled) => setForm((value) => ({ ...value, enabled }))} /></div>
        {save.error && <div role="alert" className="rounded-lg bg-destructive/10 px-3 py-2 text-sm text-destructive sm:col-span-2">{save.error.message}</div>}
      </div>
      <DialogFooter><Button variant="outline" disabled={save.isPending} onClick={() => onOpenChange(false)}>取消</Button><Button onClick={() => save.mutate()} disabled={!form.provider_id || !form.public_name.trim() || !form.upstream_model.trim() || !form.capabilities.trim() || !Number.isInteger(form.priority) || form.priority < 0 || duplicateGroup || save.isPending}>{save.isPending ? "保存中…" : route ? "保存路由" : "创建路由"}</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
