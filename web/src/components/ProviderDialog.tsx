import { useEffect, useMemo, useState } from "react"
import { Archive, Trash2 } from "lucide-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import {
  PROVIDER_KEY_SELECTION_MODE_HELP, PROVIDER_KEY_SELECTION_MODE_LABELS,
  type IPPoolNode, type Provider, type ProviderGroup, type ProviderKeySelectionMode,
} from "@/lib/types"
import { protocolMethod, protocolMethodLabel, protocolMethodPatch, supportsProtocolMethod } from "@/lib/protocol-methods"
import { ProtocolMethodSelect } from "@/components/ProtocolMethodSelect"
import { ProviderManagementCard } from "@/components/ProviderManagementCard"
import { ProviderKeysPanel } from "@/components/ProviderKeysDialog"
import { ProviderModelsPanel } from "@/components/ProviderModelManagementDialog"
import { ProviderBalancePanel } from "@/components/BalanceDialog"
import { ProviderHealthPanel } from "@/components/HealthCheckDialog"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import { useConfirm, useConfirmDelete } from "@/components/ui/confirm"

const providerTypes = [
  { value: "openai_compatible", label: "OpenAI 兼容" },
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic 官方" },
  { value: "anthropic_compatible", label: "Anthropic 兼容" },
  { value: "gemini", label: "Gemini" },
  { value: "openrouter", label: "OpenRouter" },
  { value: "grok", label: "Grok" },
  { value: "opencode", label: "OpenCode" },
]

function providerForm(provider: Provider | null) {
  return {
    name: provider?.name ?? "",
    type: provider?.type ?? "openai_compatible",
    baseURL: provider?.base_url ?? "",
    priority: provider?.priority ?? 1,
    max_concurrency: provider?.max_concurrency ?? 0,
    request_timeout_ms: provider?.request_timeout_ms ?? 120000,
    passthrough_mode: provider?.passthrough_mode ?? "normalized",
    notes: provider?.notes ?? "",
    ip_pool_node_id: provider?.ip_pool_node_id ?? 0,
    group_id: provider?.group_id ?? 0,
    key_selection_mode: (provider?.key_selection_mode ?? "configured") as ProviderKeySelectionMode,
    health_check_enabled: provider?.health_check_enabled ?? true,
    protocol_method: protocolMethod(provider?.protocol_policy, provider?.protocol_preference),
  }
}

type Section = "connection" | "routing" | "keys" | "models" | "balance" | "health" | "maintenance"

export function ProviderDialog({
  open, onOpenChange, provider, onCreated,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  provider: Provider | null
  onCreated?: (provider: { id: number; name: string }) => void
}) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const confirmDelete = useConfirmDelete()
  const [active, setActive] = useState<Provider | null>(provider)
  const [form, setForm] = useState(() => providerForm(provider))
  const [savedForm, setSavedForm] = useState(() => providerForm(provider))
  const [methodChanged, setMethodChanged] = useState(false)
  const [modelsDirty, setModelsDirty] = useState(false)
  const [initialKey, setInitialKey] = useState("")
  const [initialKeyName, setInitialKeyName] = useState("")
  const [section, setSection] = useState<Section | null>("connection")
  const [notice, setNotice] = useState("")

  useEffect(() => {
    if (!open) return
    setActive(provider)
    const initial = providerForm(provider)
    setForm(initial)
    setSavedForm(initial)
    setMethodChanged(false)
    setModelsDirty(false)
    setInitialKey("")
    setInitialKeyName("")
    setNotice("")
    setSection("connection")
  }, [open, provider])

  const { data: nodes = [] } = useQuery({ queryKey: ["ippool"], queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"), enabled: open })
  const { data: groups = [] } = useQuery({ queryKey: ["provider-groups"], queryFn: () => api<ProviderGroup[]>("/api/admin/provider-groups"), enabled: open })
  const { data: keys = [] } = useQuery({
    queryKey: ["provider-keys", active?.id],
    queryFn: () => api<{ id: number; enabled: boolean; status?: string }[]>(`/api/admin/providers/${active!.id}/keys`),
    enabled: open && active != null,
  })
  const dirty = useMemo(() => JSON.stringify(form) !== JSON.stringify(savedForm) || (!active && !!initialKey.trim()), [form, savedForm, active, initialKey])
  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) => {
    setNotice("")
    setForm((previous) => ({ ...previous, [key]: value }))
  }
  const toggle = (target: Section) => setSection((current) => current === target ? null : target)

  const save = useMutation({
    mutationFn: async (submitted: typeof form) => {
      const body: Record<string, unknown> = {
        name: submitted.name.trim(), type: submitted.type, baseURL: submitted.baseURL.trim(),
        priority: submitted.priority, max_concurrency: submitted.max_concurrency,
        request_timeout_ms: submitted.request_timeout_ms, passthrough_mode: submitted.passthrough_mode,
        notes: submitted.notes, key_selection_mode: submitted.key_selection_mode, health_check_enabled: submitted.health_check_enabled,
        ip_pool_node_id: submitted.ip_pool_node_id || null,
      }
      if (submitted.group_id) body.group_id = submitted.group_id
      else if (active) body.clear_group = true
      if (!active || methodChanged) Object.assign(body, protocolMethodPatch(submitted.protocol_method))
      if (active) {
        await api(`/api/admin/providers/${active.id}`, { method: "PATCH", body: JSON.stringify(body) })
        return { id: active.id, submitted, created: false }
      }
      const created = await api<{ id: number }>("/api/admin/providers", {
        method: "POST",
        body: JSON.stringify({ ...body, credential: initialKey.trim(), key_name: initialKeyName.trim() || "Key 1", auto_discover: false }),
      })
      return { id: created.id, submitted, created: true }
    },
    onSuccess: async ({ id, submitted, created }) => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["providers"] }),
        qc.invalidateQueries({ queryKey: ["provider-keys", id] }),
        qc.invalidateQueries({ queryKey: ["routes"] }),
      ])
      const providers = qc.getQueryData<Provider[]>(["providers"]) ?? []
      const latest = providers.find((item) => item.id === id)
      if (latest) setActive(latest)
      else if (created) setActive({ id, name: submitted.name, type: submitted.type, base_url: submitted.baseURL, priority: submitted.priority, model_count: 0 } as Provider)
      if (created) {
        onCreated?.({ id, name: submitted.name })
        setSection("keys")
        setInitialKey("")
      }
      setSavedForm({ ...submitted })
      setMethodChanged(false)
      setNotice("渠道参数已保存；其他卡片的操作各自生效。")
    },
  })

  const maintenance = useMutation({
    mutationFn: async (action: "archive" | "delete") => {
      if (!active) return
      if (action === "delete") await api(`/api/admin/providers/${active.id}`, { method: "DELETE" })
      else await api(`/api/admin/providers/${active.id}`, { method: "PATCH", body: JSON.stringify({ archived: !active.archived }) })
    },
    onSuccess: async (_result, action) => {
      await Promise.all([qc.invalidateQueries({ queryKey: ["providers"] }), qc.invalidateQueries({ queryKey: ["routes"] })])
      if (action === "delete") onOpenChange(false)
      else {
        setActive((current) => current ? { ...current, archived: !current.archived } : current)
        setNotice("渠道归档状态已更新。")
      }
    },
  })
  async function close() {
    if ((dirty || modelsDirty) && !(await confirm({ title: "放弃未保存的修改？", description: "渠道参数和模型草稿会丢失；已执行的 Key、模型和余额操作会保留。", confirmLabel: "放弃修改" }))) return
    onOpenChange(false)
  }
  const error = save.error ?? maintenance.error
  const existing = !!active
  const canSave = !!form.name.trim() && !!form.baseURL.trim() && (existing || !!initialKey.trim())
  const keySummary = existing ? `${keys.length} 张 Key · ${keys.filter((key) => key.enabled).length} 张启用` : "创建渠道时填写首张 Key"

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) void close() }}>
      <DialogContent className="flex max-h-[94vh] max-w-5xl flex-col overflow-hidden p-0 sm:p-0">
        <DialogHeader className="shrink-0 border-b px-5 pb-4 pt-5 sm:px-6">
          <DialogTitle>{active ? `管理渠道 · ${active.name}` : "添加 API 渠道"}</DialogTitle>
          <DialogDescription>按卡片查看和管理连接、转发、Key、模型、余额与检活；渠道参数单独保存。</DialogDescription>
        </DialogHeader>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4 sm:px-6">
          <div className="grid grid-cols-1 items-start gap-3 md:grid-cols-2">
            <ProviderManagementCard id="provider-connection" title="连接信息" summary={`${form.name || "新渠道"} · ${form.type}${active ? active.archived ? " · 已归档" : active.enabled ? " · 参与调度" : " · 已停用" : ""}`} expanded={section === "connection"} onToggle={() => toggle("connection")}>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5"><Label htmlFor="provider-name">名称</Label><Input id="provider-name" value={form.name} onChange={(event) => set("name", event.target.value)} placeholder="例如：粥API" /></div>
                <div className="space-y-1.5"><Label htmlFor="provider-type">类型</Label><select id="provider-type" value={form.type} onChange={(event) => { const type = event.target.value; setForm((current) => ({ ...current, type, protocol_method: supportsProtocolMethod(type, current.protocol_method) ? current.protocol_method : "auto" })); setMethodChanged(true) }} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">{providerTypes.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}</select></div>
                <div className="space-y-1.5 sm:col-span-2"><Label htmlFor="provider-url">API 地址</Label><Input id="provider-url" value={form.baseURL} onChange={(event) => set("baseURL", event.target.value)} placeholder={form.type === "anthropic" ? "https://api.anthropic.com" : "https://api.example.com/v1"} className="font-mono text-xs" /><p className="text-xs text-muted-foreground">填写服务根地址，可带 /v1；不要填写 /messages 等接口路径。</p></div>
                <div className="space-y-1.5 sm:col-span-2"><Label htmlFor="provider-notes">备注</Label><Textarea id="provider-notes" value={form.notes} onChange={(event) => set("notes", event.target.value)} rows={2} /></div>
              </div>
            </ProviderManagementCard>

            <ProviderManagementCard id="provider-routing" title="转发与调度" summary={`${protocolMethodLabel(methodChanged ? form.protocol_method === "auto" ? "auto" : "fixed" : active?.protocol_policy, methodChanged ? form.protocol_method === "auto" ? "" : form.protocol_method : active?.protocol_preference)} · 优先级 ${form.priority}`} expanded={section === "routing"} onToggle={() => toggle("routing")}>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5"><Label htmlFor="provider-protocol-method">接口方式</Label><ProtocolMethodSelect id="provider-protocol-method" type={form.type} value={form.protocol_method} onChange={(method) => { set("protocol_method", method); setMethodChanged(true) }} /><p className="text-xs text-muted-foreground">固定方式只使用选定的上游文本接口；图片、音频等专用接口不受影响。</p>{active?.protocol_policy === "fixed" && active.protocol_preference?.includes(",") && !methodChanged && <p className="text-xs text-amber-600">旧配置：{protocolMethodLabel(active.protocol_policy, active.protocol_preference)}。选择新方式后才会替换。</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-passthrough">转发模式</Label><select id="provider-passthrough" value={form.passthrough_mode} onChange={(event) => set("passthrough_mode", event.target.value)} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"><option value="normalized">标准化（转换协议）</option><option value="transparent">透传（原样转发）</option></select></div>
                <div className="space-y-1.5"><Label htmlFor="provider-priority">优先级（数字越大越优先）</Label><Input id="provider-priority" type="number" min={0} value={form.priority} onChange={(event) => set("priority", Number(event.target.value))} /></div>
                <div className="space-y-1.5"><Label htmlFor="provider-key-mode">Key 优选策略</Label><select id="provider-key-mode" value={form.key_selection_mode} onChange={(event) => set("key_selection_mode", event.target.value as ProviderKeySelectionMode)} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">{Object.entries(PROVIDER_KEY_SELECTION_MODE_LABELS).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select><p className="text-xs text-muted-foreground">{PROVIDER_KEY_SELECTION_MODE_HELP[form.key_selection_mode]}</p></div>
                <div className="space-y-1.5"><Label htmlFor="provider-egress">IP 出口</Label><select id="provider-egress" value={form.ip_pool_node_id} onChange={(event) => set("ip_pool_node_id", Number(event.target.value))} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"><option value={0}>本机直连</option>{nodes.map((node) => <option key={node.id} value={node.id}>{node.name}（{node.protocol}）</option>)}</select></div>
                <div className="space-y-1.5"><Label htmlFor="provider-group">分组</Label><select id="provider-group" value={form.group_id} onChange={(event) => set("group_id", Number(event.target.value))} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"><option value={0}>未分组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select></div>
                <div className="space-y-1.5"><Label htmlFor="provider-concurrency">最大并发（0 = 不限）</Label><Input id="provider-concurrency" type="number" value={form.max_concurrency} onChange={(event) => set("max_concurrency", Number(event.target.value))} /></div>
                <div className="space-y-1.5"><Label htmlFor="provider-timeout">请求超时（ms）</Label><Input id="provider-timeout" type="number" value={form.request_timeout_ms} onChange={(event) => set("request_timeout_ms", Number(event.target.value))} /></div>
              </div>
            </ProviderManagementCard>

            <ProviderManagementCard id="provider-keys" title="API Keys" summary={keySummary} expanded={section === "keys"} onToggle={() => toggle("keys")}>
              {active ? <ProviderKeysPanel open={open && section === "keys"} providerId={active.id} providerName={active.name} onManageModels={() => setSection("models")} /> : <div className="grid gap-3 sm:grid-cols-2"><div className="space-y-1.5"><Label htmlFor="initial-key">首张 API Key</Label><Input id="initial-key" value={initialKey} onChange={(event) => setInitialKey(event.target.value)} placeholder="sk-…" className="font-mono text-xs" /></div><div className="space-y-1.5"><Label htmlFor="initial-key-name">Key 名称</Label><Input id="initial-key-name" value={initialKeyName} onChange={(event) => setInitialKeyName(event.target.value)} placeholder="例如：主 Key" /></div><p className="text-xs text-muted-foreground sm:col-span-2">保存并创建渠道后，可在此管理多张 Key、出口、成本倍率和连接测试。</p></div>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-models" title="模型管理" summary={active ? `${active.model_count} 个已接入模型 · 按 Key 管理` : "先创建渠道后解锁"} expanded={section === "models"} onToggle={() => toggle("models")}>
              {active ? <ProviderModelsPanel open={open && section === "models"} providerId={active.id} providerName={active.name} onDirtyChange={setModelsDirty} /> : <p className="text-sm text-muted-foreground">请先保存连接信息和首张 Key。</p>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-balance" title="余额与成本" summary={active ? active.manual_balance_micros != null ? "已配置渠道余额 · 单 Key 成本倍率在 Key 卡片中设置" : "未配置手动余额 · 单 Key 倍率在 Key 卡片中设置" : "先创建渠道后解锁"} expanded={section === "balance"} onToggle={() => toggle("balance")}>
              {active ? <ProviderBalancePanel open={open && section === "balance"} providerId={active.id} providerName={active.name} /> : <p className="text-sm text-muted-foreground">请先创建渠道。</p>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-health" title="健康检查" summary={active ? `${form.health_check_enabled ? active.health_check_status || "未检活" : "检活已关闭"}${active.last_error ? ` · ${active.last_error}` : ""}` : "先创建渠道后解锁"} expanded={section === "health"} onToggle={() => toggle("health")}>
              {active ? <div className="space-y-4"><label className="flex items-center gap-2 text-sm"><Switch checked={form.health_check_enabled} onCheckedChange={(value) => set("health_check_enabled", value)} aria-label="允许渠道检活" />允许渠道检活</label><p className="text-xs text-muted-foreground">更改检活开关后，请保存渠道参数。</p><ProviderHealthPanel open={open && section === "health"} providerId={active.id} title={`模型检活 · ${active.name}`} /></div> : <p className="text-sm text-muted-foreground">请先创建渠道。</p>}
            </ProviderManagementCard>
            {active && <ProviderManagementCard id="provider-maintenance" title="维护" summary={active.archived ? "已归档 · 可恢复" : "归档或删除渠道"} expanded={section === "maintenance"} onToggle={() => toggle("maintenance")}>
              <div className="flex flex-wrap items-center justify-between gap-3"><div><Badge variant={active.archived ? "warning" : "neutral"}>{active.archived ? "已归档" : "未归档"}</Badge><p className="mt-1 text-xs text-muted-foreground">归档保留数据，删除会移除渠道及其模型路由。</p></div><div className="flex flex-wrap gap-2"><Button variant="outline" disabled={maintenance.isPending} onClick={() => maintenance.mutate("archive")}><Archive className="h-4 w-4" />{active.archived ? "取消归档" : "归档"}</Button><Button variant="destructive" disabled={maintenance.isPending} onClick={async () => { if (await confirmDelete(`渠道「${active.name}」`, "该渠道下的 Key 与模型路由也会失效。")) maintenance.mutate("delete") }}><Trash2 className="h-4 w-4" />删除渠道</Button></div></div>
            </ProviderManagementCard>}
          </div>
        </div>
        <div className="shrink-0 border-t bg-background px-4 py-3 sm:px-6">
          {error && <div role="alert" className="mb-2 text-sm text-destructive">{error instanceof Error ? error.message : "操作失败"}</div>}
          {notice && <div role="status" className="mb-2 text-sm text-emerald-600">{notice}</div>}
          <DialogFooter className="items-center">
            <span className="mr-auto text-xs text-muted-foreground">{dirty ? "渠道参数有未保存的修改" : "渠道参数已保存"} · 卡片操作各自生效</span>
            <Button variant="outline" onClick={() => void close()}>关闭</Button>
            <Button onClick={() => save.mutate({ ...form })} disabled={!canSave || !dirty || save.isPending}>{save.isPending ? "保存中…" : active ? "保存渠道参数" : "创建渠道"}</Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  )
}
