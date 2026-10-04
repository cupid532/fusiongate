import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api, providerModelsApi } from "@/lib/api"
import { refreshProviderViews } from "@/lib/provider-management"
import type { ProviderKey, ProviderKeyModel, Route } from "@/lib/types"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { Switch } from "@/components/ui/switch"

import { parseManualModels } from "@/lib/manual-models"
import { modelDisplayName } from "@/lib/model-display-name"

export function ManualModelDialog({ open, onOpenChange, providerId, keys, selectedKeyId, editing, initialModels = "", onBusyChange }: {
  open: boolean; onOpenChange: (open: boolean) => void; providerId: number; keys: ProviderKey[]; selectedKeyId: number | null
  editing?: ProviderKeyModel | null; initialModels?: string; onBusyChange?: (busy: boolean) => void
}) {
  const qc = useQueryClient()
  const [names, setNames] = useState("")
  const [displayName, setDisplayName] = useState("")
  const [capabilities, setCapabilities] = useState("chat,stream")
  const [keyIds, setKeyIds] = useState<number[]>([])
  const [enabled, setEnabled] = useState(true)
  const [createRoutes, setCreateRoutes] = useState(true)
  const [publicName, setPublicName] = useState("")
  const [syncRoutes, setSyncRoutes] = useState(false)
  const [error, setError] = useState("")
  const { data: routes = [], isError: routesFailed } = useQuery({ queryKey: ["routes"], queryFn: () => api<Route[]>("/api/admin/routes"), enabled: open })
  useEffect(() => {
    if (!open) return
    setNames(editing?.model ?? initialModels)
    setDisplayName(editing?.display_name ?? "")
    setCapabilities(editing?.capabilities ?? "chat,stream")
    setKeyIds(selectedKeyId ? [selectedKeyId] : [])
    setEnabled(editing?.enabled ?? true)
    setCreateRoutes(!editing)
    setPublicName("")
    setSyncRoutes(false)
    setError("")
  }, [open, editing, initialModels, selectedKeyId])
  const models = parseManualModels(names)
  const renamed = !!editing && models[0] !== editing.model
  const affected = routes.filter((route) => route.provider_id === providerId && route.upstream_model.toLowerCase() === editing?.model.toLowerCase())
  const oldModel = editing?.model.toLowerCase()
  const otherKeys = keys.filter((key) => !keyIds.includes(key.id) && !!oldModel && (key.models.some((model) => model.model.toLowerCase() === oldModel) || key.model?.toLowerCase() === oldModel || key.model_allowlist?.split(",").some((model) => model.trim().toLowerCase() === oldModel)))
  const save = useMutation({
    mutationFn: () => providerModelsApi.manual(providerId, {
      entries: models.map((model) => ({ model, display_name: models.length === 1 ? displayName.trim() : undefined, capabilities: capabilities.trim() })),
      key_ids: keyIds, original_model: editing?.model, enabled,
      sync_routes: renamed && syncRoutes, create_routes: createRoutes,
      public_name: models.length === 1 && publicName.trim() ? publicName.trim() : undefined,
    }),
    onSuccess: async () => { await refreshProviderViews(qc); onOpenChange(false) },
    onError: (reason) => setError(reason instanceof Error ? reason.message : "保存失败，请刷新核对配置后重试"),
  })
  useEffect(() => { onBusyChange?.(save.isPending) }, [save.isPending, onBusyChange])
  const valid = models.length > 0 && models.length <= 200 && keyIds.length > 0 && (!editing || models.length === 1) && !!capabilities.trim() && !(renamed && syncRoutes && (otherKeys.length > 0 || routesFailed))
  return <Dialog open={open} onOpenChange={(value) => { if (!save.isPending) onOpenChange(value) }}>
    <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
      <DialogHeader><DialogTitle>{editing ? "编辑上游模型" : initialModels ? "复制模型到其他 Key" : "手动添加模型"}</DialogTitle><DialogDescription>无需上游模型列表接口。只修改所选 Key 的模型权限，保存不会发起付费生成请求。</DialogDescription></DialogHeader>
      <div className="space-y-4">
        <div className="space-y-1.5"><Label htmlFor="manual-model-names">{editing ? "上游模型名" : "上游模型名（每行一个，最多 200 个）"}</Label><Textarea id="manual-model-names" value={names} onChange={(event) => setNames(event.target.value)} rows={editing ? 1 : 4} placeholder="vendor/model-v2" className="font-mono text-xs" /></div>
        {models.length === 1 && <div className="space-y-1.5"><Label htmlFor="manual-model-display">显示名称（选填，不改变转发名称）</Label><Input id="manual-model-display" value={displayName} onChange={(event) => setDisplayName(event.target.value)} placeholder={`留空默认去掉渠道前缀：${modelDisplayName(models[0])}`} /></div>}
        <div className="space-y-1.5"><Label htmlFor="manual-model-capabilities">能力</Label><Input id="manual-model-capabilities" value={capabilities} onChange={(event) => setCapabilities(event.target.value)} placeholder="chat,stream,tools" /></div>
        <fieldset className="rounded-md border p-3"><legend className="px-1 text-sm font-medium">适用 Key（{keyIds.length}）</legend><div className="mb-2 flex gap-2"><Button variant="outline" size="sm" onClick={() => setKeyIds(keys.map((key) => key.id))}>当前渠道全部 Key</Button><Button variant="ghost" size="sm" onClick={() => setKeyIds(selectedKeyId ? [selectedKeyId] : [])}>仅当前 Key</Button></div><div className="grid gap-2 sm:grid-cols-2">{keys.map((key) => <label key={key.id} className="flex items-center gap-2 text-sm"><input type="checkbox" checked={keyIds.includes(key.id)} onChange={(event) => setKeyIds((previous) => event.target.checked ? [...previous, key.id] : previous.filter((id) => id !== key.id))} />{key.name || key.key_hint}{!key.enabled && <span className="text-xs text-muted-foreground">（Key 已停用）</span>}</label>)}</div></fieldset>
        <label className="flex items-center gap-2 text-sm"><Switch checked={enabled} onCheckedChange={setEnabled} />启用所选 Key 的模型权限（不改变 Key 自身启停）</label>
        {!editing && <><label className="flex items-center gap-2 text-sm"><Switch checked={createRoutes} onCheckedChange={setCreateRoutes} />同时创建请求模型路由</label>{createRoutes && models.length === 1 && <div className="space-y-1.5"><Label htmlFor="manual-public-name">请求模型名（留空使用上游模型名）</Label><Input id="manual-public-name" value={publicName} onChange={(event) => setPublicName(event.target.value)} placeholder={models[0]} /></div>}{createRoutes && models.length > 1 && <p className="text-xs text-muted-foreground">每个上游模型创建同名请求入口，已有映射保留。</p>}</>}
        {renamed && <div className="space-y-2 rounded-md border p-3"><p className="text-sm">改名影响：所选 {keyIds.length} 张 Key；本渠道关联 {affected.length} 条路由。</p><p className="break-words text-xs text-muted-foreground">{affected.map((route) => `${route.public_name} → ${route.upstream_model}`).join("；") || "暂无关联路由"}</p><label className="flex items-center gap-2 text-sm"><Switch checked={syncRoutes} onCheckedChange={setSyncRoutes} disabled={otherKeys.length > 0 || routesFailed} />同步更新关联路由的上游模型名</label>{otherKeys.length > 0 && <p className="text-xs text-amber-600">另有 {otherKeys.length} 张未选 Key 保留旧模型。选中这些 Key 后才允许同步共享路由。</p>}{!syncRoutes && <p className="text-xs text-amber-600">路由暂时保留旧模型名；若不再有 Key 支持旧模型，该成员将不可调度。</p>}</div>}
        {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
        <p className="text-xs text-muted-foreground">手动配置优先保留，后续识别不会覆盖显示名称、能力或启停状态。新模型显示“未验证”，可另行检活。</p>
      </div>
      <DialogFooter><Button variant="outline" disabled={save.isPending} onClick={() => onOpenChange(false)}>取消</Button><Button disabled={!valid || save.isPending} onClick={() => save.mutate()}>{save.isPending ? "保存中…" : "保存模型"}</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
