import { useEffect, useState } from "react"
import { Plus, Trash2 } from "lucide-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { IPPoolNode, Provider, ProviderGroup, ProviderKey } from "@/lib/types"
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
import { Textarea } from "@/components/ui/textarea"

const providerTypes = [
  { value: "openai_compatible", label: "OpenAI 兼容" },
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
  { value: "gemini", label: "Gemini" },
  { value: "openrouter", label: "OpenRouter" },
  { value: "grok", label: "Grok" },
  { value: "opencode", label: "OpenCode" },
]

const passthroughModes = [
  { value: "normalized", label: "标准化（转换协议）" },
  { value: "transparent", label: "透传（原样转发）" },
]

export function ProviderDialog({
  open,
  onOpenChange,
  provider,
  onCreated,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  provider: Provider | null
  onCreated?: (provider: { id: number; name: string }) => void
}) {
  const qc = useQueryClient()
  const [newKeys, setNewKeys] = useState<Array<{ name: string; api_key: string; egress_mode: string; ip_pool_node_id: number; cost_multiplier: number }>>([])
  const [form, setForm] = useState({
    name: "",
    type: "openai_compatible",
    baseURL: "",
    credential: "",
    priority: 1,
    max_concurrency: 0,
    request_timeout_ms: 120000,
    passthrough_mode: "normalized",
    notes: "",
    ip_pool_node_id: 0,
    group_id: 0,
  })

  const { data: nodes = [] } = useQuery({
    queryKey: ["ippool"],
    queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"),
  })
  const { data: existingKeys = [] } = useQuery({
    queryKey: ["provider-keys", provider?.id],
    queryFn: () => api<ProviderKey[]>(`/api/admin/providers/${provider!.id}/keys`),
    enabled: open && !!provider,
  })
  const { data: groups = [] } = useQuery({
    queryKey: ["provider-groups"],
    queryFn: () => api<ProviderGroup[]>("/api/admin/provider-groups"),
  })

  useEffect(() => {
    if (open) {
      setNewKeys([])
      setForm({
        name: provider?.name ?? "",
        type: provider?.type ?? "openai_compatible",
        baseURL: provider?.base_url ?? "",
        credential: "",
        priority: provider?.priority ?? 1,
        max_concurrency: provider?.max_concurrency ?? 0,
        request_timeout_ms: provider?.request_timeout_ms ?? 120000,
        passthrough_mode: provider?.passthrough_mode ?? "normalized",
        notes: provider?.notes ?? "",
        ip_pool_node_id: provider?.ip_pool_node_id ?? 0,
        group_id: provider?.group_id ?? 0,
      })
    }
  }, [open, provider])

  const save = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        name: form.name,
        type: form.type,
        baseURL: form.baseURL,
        priority: form.priority,
        max_concurrency: form.max_concurrency,
        request_timeout_ms: form.request_timeout_ms,
        passthrough_mode: form.passthrough_mode,
        notes: form.notes,
        ip_pool_node_id: form.ip_pool_node_id || null,
      }
      if (form.group_id) body.group_id = form.group_id
      const pendingKeys = newKeys.map((key) => ({ name: key.name.trim(), api_key: key.api_key.trim(), egress_mode: key.egress_mode, ip_pool_node_id: key.egress_mode === "node" ? key.ip_pool_node_id : undefined, cost_multiplier: key.cost_multiplier })).filter((key) => key.api_key)
      if (provider) {
        if (!form.group_id) body.clear_group = true
        await api(`/api/admin/providers/${provider.id}`, { method: "PATCH", body: JSON.stringify(body) })
        for (const key of pendingKeys) {
          await api(`/api/admin/providers/${provider.id}/keys`, { method: "POST", body: JSON.stringify(key) })
        }
        return
      }
      const [primary, ...additional] = pendingKeys
      const created = await api<{ id: number }>("/api/admin/providers", { method: "POST", body: JSON.stringify({ ...body, credential: primary.api_key, key_name: primary.name || "Key 1", auto_discover: false }) })
      const createdKeys = await api<ProviderKey[]>(`/api/admin/providers/${created.id}/keys`)
      if (createdKeys[0]) {
        await api(`/api/admin/providers/${created.id}/keys/${createdKeys[0].id}`, { method: "PATCH", body: JSON.stringify({ name: primary.name || "Key 1", egress_mode: primary.egress_mode, ip_pool_node_id: primary.ip_pool_node_id, cost_multiplier: primary.cost_multiplier }) })
      }
      for (const key of additional) {
        await api(`/api/admin/providers/${created.id}/keys`, { method: "POST", body: JSON.stringify(key) })
      }
      return created
    },
    onSuccess: (created) => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["provider-keys"] })
      if (!provider && created) onCreated?.({ id: created.id, name: form.name.trim() })
      onOpenChange(false)
    },
  })

  const saveError = save.error instanceof Error ? save.error.message : ""

  const set = (k: keyof typeof form, v: string | number) => {
    if (save.error) save.reset()
    setForm((f) => ({ ...f, [k]: v }))
  }

  const handleOpenChange = (value: boolean) => {
    if (!value) save.reset()
    onOpenChange(value)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{provider ? "编辑渠道" : "添加 API 渠道"}</DialogTitle>
          <DialogDescription>配置上游连接信息与运行参数。</DialogDescription>
        </DialogHeader>

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label>名称</Label>
            <Input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="例如：粥API" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>类型</Label>
            <select
              value={form.type}
              onChange={(e) => set("type", e.target.value)}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              {providerTypes.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
          </div>
          <div className="col-span-2 flex flex-col gap-1.5">
            <Label>API 地址</Label>
            <Input value={form.baseURL} onChange={(e) => set("baseURL", e.target.value)} placeholder="https://api.example.com" className="font-mono text-xs" />
          </div>
          <div className="col-span-2 space-y-2 rounded-md border p-3">
            <div className="flex items-center justify-between gap-3">
              <div><Label>上游 API Keys</Label><div className="mt-1 text-xs text-muted-foreground">同一渠道可添加多张 Key。成本倍率调整该 Key 请求的账本成本；保存后可逐 Key 识别模型。</div></div>
              <Button type="button" size="sm" variant="outline" onClick={() => setNewKeys((keys) => [...keys, { name: `Key ${existingKeys.length + keys.length + 1}`, api_key: "", egress_mode: "inherit", ip_pool_node_id: 0, cost_multiplier: 1 }])}><Plus />添加 Key</Button>
            </div>
            {provider && existingKeys.length > 0 && <div className="space-y-2">{existingKeys.map((key) => <div key={key.id} className="grid gap-2 rounded-md border p-2 sm:grid-cols-[8rem_minmax(0,1fr)_8rem_auto_auto_auto]">
              <Input defaultValue={key.name || `Key ${key.id}`} onBlur={(e) => api(`/api/admin/providers/${provider.id}/keys/${key.id}`, { method: "PATCH", body: JSON.stringify({ name: e.target.value }) }).then(() => qc.invalidateQueries({ queryKey: ["provider-keys", provider.id] }))} />
              <select defaultValue={key.egress_mode === "node" ? `node:${key.ip_pool_node_id}` : key.egress_mode} onChange={(e) => { const [mode, node] = e.target.value.split(":"); api(`/api/admin/providers/${provider.id}/keys/${key.id}`, { method: "PATCH", body: JSON.stringify({ egress_mode: mode, ip_pool_node_id: mode === "node" ? Number(node) : null }) }).then(() => qc.invalidateQueries({ queryKey: ["provider-keys", provider.id] })) }} className="h-9 rounded-md border border-input bg-transparent px-2 text-sm"><option value="inherit">继承渠道出口</option><option value="direct">直连</option>{nodes.map((node) => <option key={node.id} value={`node:${node.id}`}>节点：{node.name}</option>)}</select>
              <div className="flex min-w-0 items-center gap-1"><span className="shrink-0 text-xs text-muted-foreground">成本倍率</span><Input type="number" min="0.01" max="1000" step="0.01" defaultValue={key.cost_multiplier || 1} onBlur={(e) => api(`/api/admin/providers/${provider.id}/keys/${key.id}`, { method: "PATCH", body: JSON.stringify({ cost_multiplier: Number(e.target.value) }) }).then(() => qc.invalidateQueries({ queryKey: ["provider-keys", provider.id] }))} aria-label={`${key.name} 成本倍率`} /></div>
              <Button type="button" variant="ghost" size="icon" title="删除 Key" onClick={() => { if (confirm(`删除 ${key.name || key.key_hint}？`)) api(`/api/admin/providers/${provider.id}/keys/${key.id}`, { method: "DELETE" }).then(() => qc.invalidateQueries({ queryKey: ["provider-keys", provider.id] })) }} aria-label={`删除 ${key.name || key.key_hint}`}><Trash2 /></Button>
            </div>)}</div>}
            <div className="space-y-2">
              {newKeys.map((key, index) => <div key={index} className="grid gap-2 rounded-md border p-2 sm:grid-cols-[8rem_minmax(0,1fr)_9rem_7rem_auto]">
                <Input value={key.name} onChange={(e) => setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, name: e.target.value } : item))} placeholder={`Key ${index + 1} 名称`} />
                <Input value={key.api_key} onChange={(e) => setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, api_key: e.target.value } : item))} placeholder={provider ? "新增 Key（留空不添加）" : "sk-…"} className="font-mono text-xs" />
                <select value={key.egress_mode === "node" ? `node:${key.ip_pool_node_id}` : key.egress_mode} onChange={(e) => { const [mode, node] = e.target.value.split(":"); setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, egress_mode: mode, ip_pool_node_id: mode === "node" ? Number(node) : 0 } : item)) }} className="h-9 rounded-md border border-input bg-transparent px-2 text-sm"><option value="inherit">继承出口</option><option value="direct">直连</option>{nodes.map((node) => <option key={node.id} value={`node:${node.id}`}>{node.name}</option>)}</select>
                <div className="flex items-center gap-1"><span className="shrink-0 text-xs text-muted-foreground">成本倍率</span><Input type="number" min="0.01" max="1000" step="0.01" value={key.cost_multiplier} onChange={(e) => setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, cost_multiplier: Number(e.target.value) } : item))} placeholder="1.00" /></div>
                <Button type="button" variant="ghost" size="icon" onClick={() => setNewKeys((keys) => keys.filter((_, i) => i !== index))} aria-label={`删除 Key 输入行 ${index + 1}`}><Trash2 /></Button>
              </div>)}
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>优先级（数字越大越优先）</Label>
            <Input type="number" min={0} value={form.priority} onChange={(e) => set("priority", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>转发模式</Label>
            <select
              value={form.passthrough_mode}
              onChange={(e) => set("passthrough_mode", e.target.value)}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              {passthroughModes.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>IP 出口</Label>
            <select
              value={form.ip_pool_node_id}
              onChange={(e) => set("ip_pool_node_id", Number(e.target.value))}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              <option value={0}>本机直连</option>
              {nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}（{n.protocol}）
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>最大并发（0 = 不限）</Label>
            <Input type="number" value={form.max_concurrency} onChange={(e) => set("max_concurrency", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>请求超时（ms）</Label>
            <Input type="number" value={form.request_timeout_ms} onChange={(e) => set("request_timeout_ms", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>分组</Label>
            <select
              value={form.group_id}
              onChange={(e) => set("group_id", Number(e.target.value))}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              <option value={0}>未分组</option>
              {groups.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>
          </div>
          <div className="col-span-2 flex flex-col gap-1.5">
            <Label>备注</Label>
            <Textarea value={form.notes} onChange={(e) => set("notes", e.target.value)} rows={2} />
          </div>
        </div>

        {saveError && (
          <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {saveError}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => handleOpenChange(false)}>
            取消
          </Button>
          <Button
            onClick={() => save.mutate()}
            disabled={!form.name.trim() || !form.baseURL.trim() || (!provider && !newKeys.some((key) => key.api_key.trim())) || save.isPending}
          >
            {save.isPending ? "保存中…" : "保存渠道"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
