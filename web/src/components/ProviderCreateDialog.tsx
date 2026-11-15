import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { providerForm, providerPatch, providersApi, refreshProviderViews, validateProviderForm } from "@/lib/provider-management"
import { ModelSetupFields, useModelSetup, useSetupRequest } from "@/components/ModelSetup"
import { EgressSelect } from "@/components/EgressSelect"
import { ProtocolMethodSelect } from "@/components/ProtocolMethodSelect"
import { supportsProtocolMethod } from "@/lib/protocol-methods"
import { PROVIDER_KEY_SELECTION_MODE_LABELS, type ProviderKeySelectionMode } from "@/lib/types"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useConfirm } from "@/components/ui/confirm"

export function ProviderCreateDialog({ onOpenChange, onCreated }: { onOpenChange: (open: boolean) => void; onCreated?: (provider: { id: number; name: string }) => void }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const [form, setForm] = useState(() => providerForm(null))
  const [credential, setCredential] = useState("")
  const [keyName, setKeyName] = useState("")
  const request = useSetupRequest()
  const nodes = useQuery({ queryKey: ["ippool"], queryFn: ({ signal }) => providersApi.nodes(signal) })
  const groups = useQuery({ queryKey: ["provider-groups"], queryFn: ({ signal }) => providersApi.groups(signal) })
  const models = useModelSetup(true, { type: form.type, baseURL: form.baseURL.trim(), credential: credential.trim(), ip_pool_node_id: form.ip_pool_node_id })
  const validation = validateProviderForm(form)
  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) => setForm((previous) => ({ ...previous, [key]: value }))
  const save = useMutation({
    mutationFn: () => api<{ id: number }>("/api/admin/providers", { method: "POST", body: JSON.stringify(request({ ...providerPatch(form, null), credential: credential.trim(), key_name: keyName.trim() || "主 Key", auto_discover: false, setup: models.setup })) }),
    onSuccess: async (created) => { await refreshProviderViews(qc); onCreated?.({ id: created.id, name: form.name.trim() }); onOpenChange(false) },
  })
  async function close() {
    if (save.isPending) return
    if ((JSON.stringify(form) !== JSON.stringify(providerForm(null)) || credential || keyName || models.manual) && !await confirm({ title: "放弃新建渠道？", description: "尚未保存，不会创建渠道或 Key。", confirmLabel: "放弃" })) return
    onOpenChange(false)
  }
  return <Dialog open onOpenChange={(value) => { if (!value) void close() }}>
    <DialogContent className="flex max-h-[94dvh] max-w-3xl flex-col overflow-hidden">
      <DialogHeader><DialogTitle>添加 API 渠道</DialogTitle><DialogDescription>填写连接 → 自动获取模型 → 勾选 → 一次保存。取消不会留下半成品。</DialogDescription></DialogHeader>
      <fieldset disabled={save.isPending} className="min-h-0 space-y-4 overflow-y-auto px-1">
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1"><Label htmlFor="create-name">名称</Label><Input id="create-name" value={form.name} onChange={(event) => set("name", event.target.value)} placeholder="你的渠道名称" /></div>
          <div className="space-y-1"><Label htmlFor="create-type">类型</Label><select id="create-type" className="h-9 w-full rounded-md border bg-background px-2 text-sm" value={form.type} onChange={(event) => { const type = event.target.value; setForm((value) => ({ ...value, type, protocol_method: supportsProtocolMethod(type, value.protocol_method) ? value.protocol_method : "auto" })) }}>{[["openai_compatible", "OpenAI 兼容"], ["openai", "OpenAI"], ["anthropic", "Anthropic 官方"], ["anthropic_compatible", "Anthropic 兼容"], ["gemini", "Gemini"], ["openrouter", "OpenRouter"], ["grok", "Grok"], ["opencode", "OpenCode"]].map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></div>
          <div className="space-y-1 sm:col-span-2"><Label htmlFor="create-url">API 地址</Label><Input id="create-url" value={form.baseURL} onChange={(event) => set("baseURL", event.target.value)} placeholder="https://api.example.com/v1" className="font-mono text-xs" /><p className="text-xs text-muted-foreground">填写服务根地址，可带 /v1，不填 /chat/completions 等具体接口路径。</p></div>
          <div className="space-y-1"><Label htmlFor="create-key">首张 API Key</Label><Input id="create-key" type="password" autoComplete="off" data-1p-ignore="true" data-lpignore="true" value={credential} onChange={(event) => setCredential(event.target.value)} className="font-mono text-xs" /></div>
          <div className="space-y-1"><Label htmlFor="create-key-name">Key 名称（选填）</Label><Input id="create-key-name" value={keyName} onChange={(event) => setKeyName(event.target.value)} placeholder="主 Key" /></div>
          <div className="space-y-1 sm:col-span-2"><Label htmlFor="create-egress">网络出口</Label><EgressSelect id="create-egress" value={form.ip_pool_node_id ? `node:${form.ip_pool_node_id}` : "direct"} onChange={(value) => set("ip_pool_node_id", value === "direct" ? 0 : Number(value.slice(5)))} nodes={nodes.data ?? []} disabled={nodes.isPending || nodes.isError} />{nodes.isError && <p role="alert" className="text-xs text-destructive">出口读取失败；保留本机直连，或重试后选择节点。<button type="button" className="underline" onClick={() => void nodes.refetch()}>重试</button></p>}</div>
        </div>
        <ModelSetupFields state={models} disabled={save.isPending} />
        <details className="rounded-lg border p-3"><summary className="cursor-pointer text-sm">更多设置（选填）</summary><div className="mt-3 grid gap-3 sm:grid-cols-2">
          <div className="space-y-1"><Label htmlFor="create-priority">优先级（数字越大越优先）</Label><Input id="create-priority" type="number" min={0} value={form.priority} onChange={(event) => set("priority", Number(event.target.value))} /></div>
          <div className="space-y-1"><Label htmlFor="create-group">分组</Label><select id="create-group" className="h-9 w-full rounded-md border bg-background px-2 text-sm" value={form.group_id} disabled={!groups.isSuccess} onChange={(event) => set("group_id", Number(event.target.value))}><option value={0}>未分组</option>{groups.data?.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select></div>
          <div className="space-y-1"><Label htmlFor="create-method">接口方式</Label><ProtocolMethodSelect id="create-method" type={form.type} value={form.protocol_method} onChange={(value) => set("protocol_method", value)} /></div>
          <div className="space-y-1"><Label htmlFor="create-timeout">请求超时（ms）</Label><Input id="create-timeout" type="number" min={1000} value={form.request_timeout_ms} onChange={(event) => set("request_timeout_ms", Number(event.target.value))} /></div>
          <div className="space-y-1"><Label htmlFor="create-concurrency">最大并发（0 = 不限）</Label><Input id="create-concurrency" type="number" min={0} value={form.max_concurrency} onChange={(event) => set("max_concurrency", Number(event.target.value))} /></div>
          <div className="space-y-1"><Label htmlFor="create-key-mode">Key 优选策略</Label><select id="create-key-mode" className="h-9 w-full rounded-md border bg-background px-2 text-sm" value={form.key_selection_mode} onChange={(event) => set("key_selection_mode", event.target.value as ProviderKeySelectionMode)}>{Object.entries(PROVIDER_KEY_SELECTION_MODE_LABELS).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></div>
          <div className="space-y-1"><Label htmlFor="create-stream-start">流式首包超时（ms，0 = 继承）</Label><Input id="create-stream-start" type="number" min={0} value={form.stream_start_timeout_ms} onChange={(event) => set("stream_start_timeout_ms", Number(event.target.value))} /></div>
          <div className="space-y-1"><Label htmlFor="create-stream-idle">流中空闲超时（ms，0 = 继承）</Label><Input id="create-stream-idle" type="number" min={0} value={form.stream_idle_timeout_ms} onChange={(event) => set("stream_idle_timeout_ms", Number(event.target.value))} /></div>
          <label className="flex items-center gap-2 text-xs sm:col-span-2"><input type="checkbox" checked={form.health_check_enabled} onChange={(event) => set("health_check_enabled", event.target.checked)} />允许渠道检活（保存不会立即发起检活）</label>
          <div className="space-y-1 sm:col-span-2"><Label htmlFor="create-website">商家地址（选填）</Label><Input id="create-website" value={form.websiteURL} onChange={(event) => set("websiteURL", event.target.value)} /></div>
          <div className="space-y-1 sm:col-span-2"><Label htmlFor="create-notes">备注</Label><Input id="create-notes" value={form.notes} onChange={(event) => set("notes", event.target.value)} /></div>
        </div></details>
      </fieldset>
      {form.name.trim() && form.baseURL.trim() && Object.keys(validation).length > 0 && <p role="alert" className="text-xs text-destructive">{Object.values(validation).join("；")}</p>}
      {save.error && <p role="alert" className="break-words text-sm text-destructive">{save.error.message}。相同内容可安全重试；如需修改内容，请先刷新渠道列表核对上次结果。</p>}
      <DialogFooter><Button variant="outline" disabled={save.isPending} onClick={() => void close()}>取消</Button><Button disabled={save.isPending || !models.ready || Object.keys(validation).length > 0} onClick={() => save.mutate()}>{save.isPending ? "保存中…" : models.skip ? "仅保存配置" : `保存渠道（已选 ${models.selected.length} 个模型）`}</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
