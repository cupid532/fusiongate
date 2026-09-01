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
import { Badge } from "@/components/ui/badge"

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
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  provider: Provider | null
}) {
  const qc = useQueryClient()
  const [newKeys, setNewKeys] = useState([{ name: "默认 Key", api_key: "" }])
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
      setNewKeys([{ name: provider ? "" : "默认 Key", api_key: "" }])
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
      const pendingKeys = newKeys.map((key) => ({ name: key.name.trim(), api_key: key.api_key.trim() })).filter((key) => key.api_key)
      if (provider) {
        if (!form.group_id) body.clear_group = true
        await api(`/api/admin/providers/${provider.id}`, { method: "PATCH", body: JSON.stringify(body) })
        for (const key of pendingKeys) {
          await api(`/api/admin/providers/${provider.id}/keys`, { method: "POST", body: JSON.stringify(key) })
        }
        return
      }
      const [primary, ...additional] = pendingKeys
      const created = await api<{ id: number }>("/api/admin/providers", { method: "POST", body: JSON.stringify({ ...body, credential: primary.api_key, key_name: primary.name || "默认 Key", auto_discover: false }) })
      for (const key of additional) {
        await api(`/api/admin/providers/${created.id}/keys`, { method: "POST", body: JSON.stringify(key) })
      }
      return created
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["provider-keys"] })
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
              <div><Label>上游 API Keys</Label><div className="mt-1 text-xs text-muted-foreground">同一渠道可一次添加多张 Key；保存后分别识别模型。</div></div>
              <Button type="button" size="sm" variant="outline" onClick={() => setNewKeys((keys) => [...keys, { name: "", api_key: "" }])}><Plus />添加一行</Button>
            </div>
            {provider && existingKeys.length > 0 && <div className="flex flex-wrap gap-1.5">{existingKeys.map((key) => <Badge key={key.id} variant={key.enabled ? "success" : "neutral"}>{key.name || "API Key"} · {key.key_hint}</Badge>)}</div>}
            <div className="space-y-2">
              {newKeys.map((key, index) => <div key={index} className="grid gap-2 sm:grid-cols-[9rem_minmax(0,1fr)_auto]">
                <Input value={key.name} onChange={(e) => setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, name: e.target.value } : item))} placeholder={`Key ${index + 1} 名称`} />
                <Input value={key.api_key} onChange={(e) => setNewKeys((keys) => keys.map((item, i) => i === index ? { ...item, api_key: e.target.value } : item))} placeholder={provider ? "新增 Key（留空不添加）" : "sk-…"} className="font-mono text-xs" />
                <Button type="button" variant="ghost" size="icon" disabled={newKeys.length === 1} onClick={() => setNewKeys((keys) => keys.filter((_, i) => i !== index))} aria-label={`删除 Key 输入行 ${index + 1}`}><Trash2 /></Button>
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
