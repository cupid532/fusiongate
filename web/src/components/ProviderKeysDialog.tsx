import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Activity, ChevronDown, ChevronRight, Plus, RefreshCw, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { IPPoolNode, ProviderKey, ProviderKeyModel } from "@/lib/types"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"

function formatTime(value?: string) {
  if (!value) return "尚未"
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

function statusBadge(status?: string) {
  if (status === "healthy") return <Badge variant="success">正常</Badge>
  if (status === "failed" || status === "unhealthy") return <Badge variant="danger">异常</Badge>
  if (status === "disabled") return <Badge variant="neutral">已关闭</Badge>
  if (status === "pending" || status === "running") return <Badge variant="warning">检测中</Badge>
  return <Badge variant="neutral">{status || "未测试"}</Badge>
}

function modelStatus(model: ProviderKeyModel) { return statusBadge(model.health_status) }

function effectiveEgress(key: ProviderKey, nodes: IPPoolNode[]) {
  if (key.effective_egress === "node") {
    const nodeName = nodes.find((node) => node.id === key.effective_node_id)?.name || key.ip_pool_node_name
    return `${nodeName || (key.effective_node_id ? `节点 #${key.effective_node_id}` : "节点")}${key.egress_inherited ? "（继承）" : ""}`
  }
  return `直连${key.egress_inherited ? "（继承）" : ""}`
}

export function ProviderKeysDialog({ open, onOpenChange, providerId, providerName }: {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerId: number
  providerName: string
}) {
  const qc = useQueryClient()
  const [newKey, setNewKey] = useState("")
  const [newName, setNewName] = useState("")
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  const [modelDrafts, setModelDrafts] = useState<Record<number, string>>( {})

  const { data: keys = [], isLoading } = useQuery({
    queryKey: ["provider-keys", providerId],
    queryFn: () => api<ProviderKey[]>(`/api/admin/providers/${providerId}/keys`),
    enabled: open,
  })
  const { data: nodes = [] } = useQuery({ queryKey: ["ip-pool"], queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"), enabled: open })
  const refreshKeys = () => qc.invalidateQueries({ queryKey: ["provider-keys", providerId] })

  const add = useMutation({
    mutationFn: async () => api(`/api/admin/providers/${providerId}/keys`, { method: "POST", body: JSON.stringify({ api_key: newKey, name: newName || undefined, health_check_enabled: true }) }),
    onSuccess: () => { refreshKeys(); qc.invalidateQueries({ queryKey: ["providers"] }); setNewKey(""); setNewName("") },
  })
  const remove = useMutation({ mutationFn: async (keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "DELETE" }), onSuccess: refreshKeys })
  const patch = useMutation({
    mutationFn: async ({ keyId, body }: { keyId: number; body: Record<string, unknown> }) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "PATCH", body: JSON.stringify(body) }),
    onSuccess: refreshKeys,
  })
  const action = useMutation({
    mutationFn: async ({ keyId, name }: { keyId: number; name: "test" | "discover-models" }) => api(`/api/admin/providers/${providerId}/keys/${keyId}/${name}`, { method: "POST" }),
    onSuccess: (_data, variables) => { setExpanded((current) => new Set(current).add(variables.keyId)); refreshKeys() },
  })

  const setModels = (key: ProviderKey, models: string[]) => patch.mutate({ keyId: key.id, body: { models } })
  const toggleModel = (key: ProviderKey, model: string, enabled: boolean) => {
    const selected = new Set((key.models ?? []).filter((item) => item.enabled).map((item) => item.model))
    if (enabled) selected.add(model); else selected.delete(model)
    setModels(key, [...selected])
  }
  const removeModel = (key: ProviderKey, model: string) => {
    if (!confirm(`从 ${key.name || key.key_hint} 删除模型 ${model}？`)) return
    patch.mutate({ keyId: key.id, body: { remove_models: [model] } })
  }

  const addModel = (key: ProviderKey) => {
    const model = (modelDrafts[key.id] || "").trim()
    if (!model) return
    const selected = new Set((key.models ?? []).filter((item) => item.enabled).map((item) => item.model))
    selected.add(model)
    setModels(key, [...selected])
    setModelDrafts((current) => ({ ...current, [key.id]: "" }))
    setExpanded((current) => new Set(current).add(key.id))
  }

  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-h-[90vh] max-w-4xl overflow-y-auto">
      <DialogHeader>
        <DialogTitle>Key 与模型管理 · {providerName}</DialogTitle>
        <DialogDescription>每张 Key 独立识别、选择模型和检活；模型列表默认收起。</DialogDescription>
      </DialogHeader>

      <div className="grid gap-2 rounded-md border p-3 sm:grid-cols-[minmax(0,1fr)_9rem_auto] sm:items-end">
        <div className="flex min-w-0 flex-col gap-1.5"><Label>新 API Key</Label><Input value={newKey} onChange={(e) => setNewKey(e.target.value)} placeholder="sk-…" className="font-mono text-xs" /></div>
        <div className="flex min-w-0 flex-col gap-1.5"><Label>名称</Label><Input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="例如：主 Key" /></div>
        <Button onClick={() => add.mutate()} disabled={!newKey.trim() || add.isPending}><Plus />{add.isPending ? "添加中…" : "添加 Key"}</Button>
      </div>

      {isLoading ? <div className="py-6 text-center text-sm text-muted-foreground">加载中…</div> : keys.length === 0 ? <div className="py-6 text-center text-sm text-muted-foreground">还没有 Key</div> : <div className="space-y-2">
        {keys.map((key) => {
          const models = key.models ?? []
          const isExpanded = expanded.has(key.id)
          const enabledModels = models.filter((model) => model.enabled).length
          return <section key={key.id} className="rounded-md border">
            <div className="flex flex-col gap-3 p-3 sm:flex-row sm:items-start">
              <button type="button" className="flex min-w-0 flex-1 gap-2 text-left" onClick={() => setExpanded((current) => { const next = new Set(current); if (next.has(key.id)) next.delete(key.id); else next.add(key.id); return next })} aria-expanded={isExpanded}>
                {isExpanded ? <ChevronDown className="mt-0.5 h-4 w-4 shrink-0" /> : <ChevronRight className="mt-0.5 h-4 w-4 shrink-0" />}
                <span className="min-w-0">
                  <span className="flex flex-wrap items-center gap-2"><span className="text-sm font-medium">{key.name || "API Key"}</span><span className="font-mono text-xs text-muted-foreground">{key.key_hint}</span>{!key.enabled && <Badge variant="neutral">已停用</Badge>}</span>
                  <span className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground"><span>出口：{effectiveEgress(key, nodes)}</span><span>成本倍率：{key.cost_multiplier || 1}</span><span>模型：{enabledModels}/{models.length}</span><span>识别：{formatTime(key.last_discovered_at)}</span></span>
                  {key.last_error && <span className="mt-1 block break-words text-xs text-destructive">{key.last_error}</span>}
                </span>
              </button>
              <div className="flex flex-wrap items-center gap-2 sm:justify-end">
                {statusBadge(key.status)}
                <label className="flex items-center gap-1.5 text-xs text-muted-foreground">启用<Switch checked={key.enabled} disabled={patch.isPending} onCheckedChange={(enabled) => patch.mutate({ keyId: key.id, body: { enabled } })} aria-label={`${key.name || key.key_hint} 启用`} /></label>
                <label className="flex items-center gap-1.5 text-xs text-muted-foreground">检活<Switch checked={key.health_check_enabled} disabled={patch.isPending} onCheckedChange={(health_check_enabled) => patch.mutate({ keyId: key.id, body: { health_check_enabled } })} aria-label={`${key.name || key.key_hint} 检活`} /></label>
                <Button variant="ghost" size="icon" title="为此 Key 识别模型" aria-label={`识别 ${key.name || key.key_hint} 的模型`} disabled={action.isPending} onClick={() => action.mutate({ keyId: key.id, name: "discover-models" })}><RefreshCw /></Button>
                <Button variant="ghost" size="icon" title="测试此 Key" aria-label={`测试 ${key.name || key.key_hint}`} disabled={action.isPending} onClick={() => action.mutate({ keyId: key.id, name: "test" })}><Activity /></Button>
                <Button variant="ghost" size="icon" title="删除 Key" aria-label={`删除 ${key.name || key.key_hint}`} disabled={remove.isPending} onClick={() => { if (confirm("删除这个 Key？")) remove.mutate(key.id) }}><Trash2 className="text-destructive" /></Button>
              </div>
            </div>

            {isExpanded && <div className="border-t bg-muted/15 px-3 py-3">
              <div className="mb-3 flex flex-col gap-2 sm:flex-row">
                <Input value={modelDrafts[key.id] || ""} onChange={(e) => setModelDrafts((current) => ({ ...current, [key.id]: e.target.value }))} onKeyDown={(e) => { if (e.key === "Enter") addModel(key) }} placeholder="为此 Key 手工添加模型名" className="font-mono text-xs" />
                <Button variant="outline" onClick={() => addModel(key)} disabled={!modelDrafts[key.id]?.trim() || patch.isPending}><Plus />添加模型</Button>
              </div>
              {models.length === 0 ? <div className="text-xs text-muted-foreground">尚无模型。点击上方识别按钮，或手工添加此 Key 支持的模型。</div> : <div className="max-h-72 space-y-1 overflow-y-auto pr-1">
                {models.map((model) => <label key={model.model} className="grid cursor-pointer gap-1 rounded-md px-2 py-1.5 text-xs hover:bg-muted/50 sm:grid-cols-[auto_minmax(0,1fr)_auto_auto_auto] sm:items-center sm:gap-3">
                  <input type="checkbox" checked={model.enabled} disabled={patch.isPending} onChange={(e) => toggleModel(key, model.model, e.target.checked)} aria-label={`${key.name || key.key_hint} 模型 ${model.model}`} />
                  <span className="min-w-0"><span className="block truncate font-mono text-foreground" title={model.model}>{model.display_name || model.model}</span>{model.display_name && model.display_name !== model.model && <span className="block truncate font-mono text-[11px] text-muted-foreground">{model.model}</span>}{model.health_error && <span className="block break-words text-[11px] text-destructive">{model.health_error}</span>}</span>
                  <span className="text-muted-foreground">{model.last_checked_at ? `${model.latency_ms} ms · ${formatTime(model.last_checked_at)}` : "尚未检活"}</span>
                  <span>{modelStatus(model)}</span>
                  <Button type="button" variant="ghost" size="icon" className="h-7 w-7" title="删除此模型" aria-label={`删除模型 ${model.model}`} disabled={patch.isPending} onClick={(e) => { e.preventDefault(); removeModel(key, model.model) }}><Trash2 className="h-3.5 w-3.5 text-destructive" /></Button>
                </label>)}
              </div>}
            </div>}
          </section>
        })}
      </div>}
      <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
