import { useEffect, useMemo, useState } from "react"
import { Archive, Trash2 } from "lucide-react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { providerForm, providerPatch, providersApi, refreshProviderViews, validateProviderForm, providerStatus } from "@/lib/provider-management"
import {
  PROVIDER_KEY_SELECTION_MODE_HELP, PROVIDER_KEY_SELECTION_MODE_LABELS,
  type Provider, type ProviderKeySelectionMode,
} from "@/lib/types"
import { protocolMethodLabel, supportsProtocolMethod } from "@/lib/protocol-methods"
import { ProtocolMethodSelect } from "@/components/ProtocolMethodSelect"
import { ProviderManagementCard } from "@/components/ProviderManagementCard"
import { ProviderKeysPanel } from "@/components/ProviderKeysDialog"
import { ProviderModelsPanel } from "@/components/ProviderModelManagementDialog"
import { ProviderBalancePanel } from "@/components/BalanceDialog"
import { ProviderHealthPanel } from "@/components/HealthCheckDialog"
import { EgressSelect } from "@/components/EgressSelect"
import { QueryError } from "@/components/ui/query-error"
import { cn } from "@/lib/utils"
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

export type ProviderSection = "connection" | "routing" | "keys" | "models" | "balance" | "health" | "advanced" | "maintenance"
const sections: Array<{ id: ProviderSection; label: string }> = [
  { id: "connection", label: "连接信息" }, { id: "routing", label: "转发与调度" },
  { id: "keys", label: "API Keys" }, { id: "models", label: "模型管理" },
  { id: "balance", label: "余额与成本" }, { id: "health", label: "健康检查" },
  { id: "advanced", label: "高级设置" }, { id: "maintenance", label: "维护" },
]

export function ProviderDialog({
  open, onOpenChange, provider, onCreated, initialSection = "connection",
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  provider: Provider | null
  onCreated?: (provider: { id: number; name: string }) => void
  initialSection?: ProviderSection
}) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const confirmDelete = useConfirmDelete()
  const [active, setActive] = useState<Provider | null>(provider)
  const [form, setForm] = useState(() => providerForm(provider))
  const [savedForm, setSavedForm] = useState(() => providerForm(provider))
  const [methodChanged, setMethodChanged] = useState(false)
  const [modelsDirty, setModelsDirty] = useState(false)
  const [keysDirty, setKeysDirty] = useState(false)
  const [keysBusy, setKeysBusy] = useState(false)
  const [modelsBusy, setModelsBusy] = useState(false)
  const [draftEpoch, setDraftEpoch] = useState(0)
  const [initialKey, setInitialKey] = useState("")
  const [initialKeyName, setInitialKeyName] = useState("")
  const [section, setSection] = useState<ProviderSection>(initialSection)
  const [notice, setNotice] = useState("")

  useEffect(() => {
    if (!open) return
    setActive(provider)
    const initial = providerForm(provider)
    setForm(initial)
    setSavedForm(initial)
    setMethodChanged(false)
    setModelsDirty(false)
    setKeysDirty(false)
    setKeysBusy(false)
    setModelsBusy(false)
    setDraftEpoch((previous) => previous + 1)
    setInitialKey("")
    setInitialKeyName("")
    setNotice("")
    setSection(initialSection)
  }, [open, provider, initialSection])

  const nodesQuery = useQuery({ queryKey: ["ippool"], queryFn: ({ signal }) => providersApi.nodes(signal), enabled: open })
  const groupsQuery = useQuery({ queryKey: ["provider-groups"], queryFn: ({ signal }) => providersApi.groups(signal), enabled: open })
  const nodes = nodesQuery.data ?? []
  const groups = groupsQuery.data ?? []
  const { data: keys = [], isPending: keysLoading, isError: keysFailed } = useQuery({
    queryKey: ["provider-keys", active?.id],
    queryFn: () => api<{ id: number; enabled: boolean; status?: string }[]>(`/api/admin/providers/${active!.id}/keys`),
    enabled: open && active != null,
  })
  const dirty = useMemo(() => JSON.stringify(form) !== JSON.stringify(savedForm) || (!active && (!!initialKey.trim() || !!initialKeyName.trim())), [form, savedForm, active, initialKey, initialKeyName])
  const set = <K extends keyof typeof form>(key: K, value: (typeof form)[K]) => {
    setNotice("")
    setForm((previous) => ({ ...previous, [key]: value }))
  }
  const toggle = (target: ProviderSection) => setSection(target)

  const save = useMutation({
    mutationFn: async (submitted: typeof form) => {
      const body = providerPatch(submitted, active ? savedForm : null, methodChanged)
      if (active) {
        const result = await providersApi.patch(active.id, body)
        return { id: active.id, submitted, created: false, discovery: result?.protocol_discovery }
      }
      const created = await api<{ id: number }>("/api/admin/providers", {
        method: "POST",
        body: JSON.stringify({ ...body, credential: initialKey.trim(), key_name: initialKeyName.trim() || "Key 1", auto_discover: false }),
      })
      return { id: created.id, submitted, created: true, discovery: undefined }
    },
    onSuccess: async ({ id, submitted, created, discovery }) => {
      await refreshProviderViews(qc)
      const providers = qc.getQueryData<Provider[]>(["providers"]) ?? []
      const latest = providers.find((item) => item.id === id)
      if (latest) setActive(latest)
      else if (created) setActive({ id, name: submitted.name, type: submitted.type, base_url: submitted.baseURL, website_url: submitted.websiteURL, priority: submitted.priority, model_count: 0 } as Provider)
      if (created) {
        onCreated?.({ id, name: submitted.name })
        setSection("models")
        setInitialKey("")
      }
      const baseline = latest ? providerForm(latest) : { ...submitted, name: submitted.name.trim(), baseURL: submitted.baseURL.trim(), websiteURL: submitted.websiteURL.trim() }
      setForm(baseline)
      setSavedForm(baseline)
      setMethodChanged(false)
      setNotice(discovery?.status === "failed" ? `渠道参数已保存，但模型识别失败：${discovery.error || "请前往模型管理重试"}` : "渠道参数已保存；Key 与模型配置分别保存。")
    },
  })

  const maintenance = useMutation({
    mutationFn: async (action: "archive" | "delete") => {
      if (!active) return
      if (action === "delete") await api(`/api/admin/providers/${active.id}`, { method: "DELETE" })
      else await api(`/api/admin/providers/${active.id}`, { method: "PATCH", body: JSON.stringify({ archived: !active.archived }) })
    },
    onSuccess: async (_result, action) => {
      await refreshProviderViews(qc)
      if (action === "delete") onOpenChange(false)
      else {
        setActive((current) => current ? { ...current, archived: !current.archived } : current)
        setNotice("渠道归档状态已更新。")
      }
    },
  })
  const resetSave = save.reset
  const resetMaintenance = maintenance.reset
  useEffect(() => { if (open) { resetSave(); resetMaintenance() } }, [open, resetSave, resetMaintenance])
  async function close() {
    if (save.isPending || maintenance.isPending || keysBusy || modelsBusy) return
    if ((dirty || modelsDirty || keysDirty) && !(await confirm({ title: "放弃未保存的修改？", description: "未保存的渠道、Key 和模型草稿会丢失；已完成的保存、测试和余额操作会保留。", confirmLabel: "放弃修改" }))) return
    onOpenChange(false)
  }
  const error = save.error ?? maintenance.error
  const existing = !!active
  const validation = validateProviderForm(form)
  const canSave = Object.keys(validation).length === 0 && (existing || !!initialKey.trim()) && (form.ip_pool_node_id === savedForm.ip_pool_node_id || !nodesQuery.isPending && !nodesQuery.isError) && (form.group_id === savedForm.group_id || !groupsQuery.isPending && !groupsQuery.isError)
  const keySummary = existing ? keysLoading ? "读取 Key 中…" : keysFailed ? "Key 数据读取失败" : `${keys.length} 张 Key · ${keys.filter((key) => key.enabled).length} 张启用` : "创建渠道时填写首张 Key"

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) void close() }}>
      <DialogContent className="flex h-[94dvh] max-h-[94dvh] w-[calc(100vw-1rem)] max-w-6xl flex-col overflow-hidden p-0 sm:p-0">
        <DialogHeader className="shrink-0 border-b px-5 pb-4 pt-5 sm:px-6">
          <DialogTitle>{active ? `管理渠道 · ${active.name}` : "添加 API 渠道"}</DialogTitle>
          <DialogDescription>渠道参数、Key 与模型分别保存；测试、识别模型和维护操作点击后执行。</DialogDescription>
          {active && <div className="flex flex-wrap gap-2 pt-2 text-xs"><Badge variant={providerStatus(active).variant}>{providerStatus(active).label}</Badge><span className="text-muted-foreground">默认出口：{active.ip_pool_node_name || (active.ip_pool_node_id ? `节点 #${active.ip_pool_node_id}` : "本机直连")} · 优先级 {active.priority}</span></div>}
        </DialogHeader>
        <div className="flex min-h-0 flex-1 flex-col md:flex-row">
          <nav aria-label="渠道配置区域" className="flex shrink-0 gap-1 overflow-x-auto border-b bg-muted/20 p-2 md:w-44 md:flex-col md:overflow-y-auto md:border-b-0 md:border-r md:p-3">
            {sections.filter((item) => active || item.id !== "maintenance").map((item) => <button key={item.id} type="button" aria-current={section === item.id ? "page" : undefined} onClick={() => toggle(item.id)} className={cn("shrink-0 rounded-lg px-3 py-2 text-left text-sm transition-colors", section === item.id ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted hover:text-foreground")}>{item.label}{item.id === "models" && modelsDirty || item.id === "keys" && keysDirty ? " · 未保存" : ""}</button>)}
          </nav>
          <div className="min-h-0 min-w-0 flex-1 overflow-y-auto p-3 sm:p-5">
          <fieldset disabled={save.isPending || maintenance.isPending} className="min-w-0 space-y-3">
            <ProviderManagementCard id="provider-connection" title="连接信息" summary={`${form.name || "新渠道"} · ${form.type}${active ? active.archived ? " · 已归档" : active.enabled ? " · 参与调度" : " · 已停用" : ""}`} expanded={section === "connection"} onToggle={() => toggle("connection")}>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5"><Label htmlFor="provider-name">名称</Label><Input id="provider-name" aria-invalid={dirty && !!validation.name || undefined} value={form.name} onChange={(event) => set("name", event.target.value)} placeholder="例如：粥API" />{dirty && validation.name && <p className="text-xs text-destructive">{validation.name}</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-type">类型</Label><select id="provider-type" value={form.type} onChange={(event) => { const type = event.target.value; setForm((current) => ({ ...current, type, protocol_method: supportsProtocolMethod(type, current.protocol_method) ? current.protocol_method : "auto" })); setMethodChanged(true) }} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">{providerTypes.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}</select></div>
                <div className="space-y-1.5 sm:col-span-2"><Label htmlFor="provider-url">API 地址</Label><Input id="provider-url" aria-invalid={dirty && !!validation.baseURL || undefined} value={form.baseURL} onChange={(event) => set("baseURL", event.target.value)} placeholder={form.type === "anthropic" ? "https://api.anthropic.com" : "https://api.example.com/v1"} className="font-mono text-xs" />{dirty && validation.baseURL && <p className="text-xs text-destructive">{validation.baseURL}</p>}<p className="text-xs text-muted-foreground">填写服务根地址，可带 /v1；不要填写 /messages 等接口路径。</p></div>
                <div className="space-y-1.5 sm:col-span-2"><Label htmlFor="provider-website">商家地址（选填）</Label><Input id="provider-website" aria-invalid={dirty && !!validation.websiteURL || undefined} type="url" value={form.websiteURL} onChange={(event) => set("websiteURL", event.target.value)} placeholder="https://example.com/account" className="font-mono text-xs" />{dirty && validation.websiteURL && <p className="text-xs text-destructive">{validation.websiteURL}</p>}<p className="text-xs text-muted-foreground">列表名称旁的外链图标打开此地址；留空则打开 API 地址所在网站。点击渠道名称进入管理。</p></div>
                <div className="space-y-1.5 sm:col-span-2"><Label htmlFor="provider-notes">备注</Label><Textarea id="provider-notes" value={form.notes} onChange={(event) => set("notes", event.target.value)} rows={2} /></div>
              </div>
            </ProviderManagementCard>

            <ProviderManagementCard id="provider-routing" title="转发与调度" summary={`${protocolMethodLabel(methodChanged ? form.protocol_method === "auto" ? "auto" : "fixed" : active?.protocol_policy, methodChanged ? form.protocol_method === "auto" ? "" : form.protocol_method : active?.protocol_preference)} · 优先级 ${form.priority}`} expanded={section === "routing"} onToggle={() => toggle("routing")}>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5"><Label htmlFor="provider-protocol-method">接口方式</Label><ProtocolMethodSelect id="provider-protocol-method" type={form.type} value={form.protocol_method} onChange={(method) => { set("protocol_method", method); setMethodChanged(true) }} /><p className="text-xs text-muted-foreground">自适应：客户端协议渠道支持时原样透传，不支持时自动转换到渠道可用的接口。固定：只使用选定的上游接口，其他客户端协议自动转换。</p>{active?.passthrough_supported === false && <p className="text-xs text-amber-700 dark:text-amber-400">{active.passthrough_reason || "该专用渠道不参与推理路由。"}</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-priority">优先级（数字越大越优先）</Label><Input id="provider-priority" aria-invalid={dirty && !!validation.priority || undefined} type="number" min={0} value={form.priority} onChange={(event) => set("priority", Number(event.target.value))} />{dirty && validation.priority && <p className="text-xs text-destructive">{validation.priority}</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-key-mode">Key 优选策略</Label><select id="provider-key-mode" value={form.key_selection_mode} onChange={(event) => set("key_selection_mode", event.target.value as ProviderKeySelectionMode)} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">{Object.entries(PROVIDER_KEY_SELECTION_MODE_LABELS).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select><p className="text-xs text-muted-foreground">{PROVIDER_KEY_SELECTION_MODE_HELP[form.key_selection_mode]}</p></div>
                <div className="space-y-1.5"><Label htmlFor="provider-egress">渠道默认出口</Label><EgressSelect id="provider-egress" value={form.ip_pool_node_id ? `node:${form.ip_pool_node_id}` : "direct"} onChange={(value) => set("ip_pool_node_id", value === "direct" ? 0 : Number(value.slice(5)))} nodes={nodes} disabled={nodesQuery.isPending || nodesQuery.isError} /><p className="text-xs text-muted-foreground">只影响继承渠道出口的 Key；独立节点和强制直连不变。</p>{nodesQuery.isError && <QueryError error={nodesQuery.error} onRetry={() => void nodesQuery.refetch()} className="p-3" />}{!nodesQuery.isPending && !nodesQuery.isError && !nodes.some((node) => node.enabled) && <p className="text-xs text-muted-foreground">暂无启用节点，可在 IP 池添加出口。</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-group">分组</Label><select id="provider-group" value={form.group_id} onChange={(event) => set("group_id", Number(event.target.value))} disabled={groupsQuery.isPending || groupsQuery.isError} className="h-9 w-full rounded-md border border-input bg-background px-3 text-sm"><option value={0}>未分组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select>{groupsQuery.isError && <QueryError error={groupsQuery.error} onRetry={() => void groupsQuery.refetch()} className="p-3" />}</div>
              </div>
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-advanced" title="高级设置" summary="并发与超时 · 不修改后端全局默认值" expanded={section === "advanced"} onToggle={() => toggle("advanced")}>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5"><Label htmlFor="provider-concurrency">最大并发（0 = 不限）</Label><Input id="provider-concurrency" aria-invalid={dirty && !!validation.max_concurrency || undefined} type="number" min={0} step={1} value={form.max_concurrency} onChange={(event) => set("max_concurrency", Number(event.target.value))} />{dirty && validation.max_concurrency && <p className="text-xs text-destructive">{validation.max_concurrency}</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-timeout">请求超时（ms）</Label><Input id="provider-timeout" aria-invalid={dirty && !!validation.request_timeout_ms || undefined} type="number" value={form.request_timeout_ms} onChange={(event) => set("request_timeout_ms", Number(event.target.value))} />{dirty && validation.request_timeout_ms && <p className="text-xs text-destructive">{validation.request_timeout_ms}</p>}</div>
                <div className="space-y-1.5"><Label htmlFor="provider-stream-start">流式首包超时（ms）</Label><Input id="provider-stream-start" aria-invalid={dirty && !!validation.stream_start_timeout_ms || undefined} type="number" min={0} value={form.stream_start_timeout_ms} onChange={(event) => set("stream_start_timeout_ms", Number(event.target.value))} />{dirty && validation.stream_start_timeout_ms && <p className="text-xs text-destructive">{validation.stream_start_timeout_ms}</p>}<p className="text-xs text-muted-foreground">0：继承显式环境配置，否则继承请求超时。</p></div>
                <div className="space-y-1.5"><Label htmlFor="provider-stream-idle">流中空闲超时（ms）</Label><Input id="provider-stream-idle" aria-invalid={dirty && !!validation.stream_idle_timeout_ms || undefined} type="number" min={0} value={form.stream_idle_timeout_ms} onChange={(event) => set("stream_idle_timeout_ms", Number(event.target.value))} />{dirty && validation.stream_idle_timeout_ms && <p className="text-xs text-destructive">{validation.stream_idle_timeout_ms}</p>}<p className="text-xs text-muted-foreground">0：继承显式环境配置，否则 300 秒；持续输出不受总时长限制。</p></div>
              </div>
            </ProviderManagementCard>

            <ProviderManagementCard id="provider-keys" title="API Keys" summary={keySummary} expanded={section === "keys"} onToggle={() => toggle("keys")}>
              {active ? <ProviderKeysPanel key={`${active.id}:${draftEpoch}`} open={open && section === "keys"} providerId={active.id} providerName={active.name} onManageModels={() => setSection("models")} onDirtyChange={setKeysDirty} onBusyChange={setKeysBusy} /> : <div className="grid gap-3 sm:grid-cols-2"><div className="space-y-1.5"><Label htmlFor="initial-key">首张 API Key</Label><Input id="initial-key" type="password" autoComplete="off" data-1p-ignore="true" data-lpignore="true" value={initialKey} onChange={(event) => setInitialKey(event.target.value)} placeholder="sk-…" className="font-mono text-xs" /></div><div className="space-y-1.5"><Label htmlFor="initial-key-name">Key 名称</Label><Input id="initial-key-name" value={initialKeyName} onChange={(event) => setInitialKeyName(event.target.value)} placeholder="例如：主 Key" /></div><p className="text-xs text-muted-foreground sm:col-span-2">保存并创建渠道后，可在此管理多张 Key、出口、成本倍率和连接测试。</p></div>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-models" title="模型管理" summary={active ? `${active.model_count} 个已接入模型 · 按 Key 管理` : "先创建渠道后解锁"} expanded={section === "models"} onToggle={() => toggle("models")}>
              {active ? <ProviderModelsPanel key={`${active.id}:${draftEpoch}`} open={open && section === "models"} providerId={active.id} providerName={active.name} onDirtyChange={setModelsDirty} onBusyChange={setModelsBusy} /> : <p className="text-sm text-muted-foreground">请先保存连接信息和首张 Key。</p>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-balance" title="余额与成本" summary={active ? active.manual_balance_micros != null ? "已配置渠道余额 · 单 Key 成本倍率在 Key 卡片中设置" : "未配置手动余额 · 单 Key 倍率在 Key 卡片中设置" : "先创建渠道后解锁"} expanded={section === "balance"} onToggle={() => toggle("balance")}>
              {active ? <ProviderBalancePanel open={open && section === "balance"} providerId={active.id} providerName={active.name} /> : <p className="text-sm text-muted-foreground">请先创建渠道。</p>}
            </ProviderManagementCard>
            <ProviderManagementCard id="provider-health" title="健康检查" summary={active ? `${form.health_check_enabled ? providerStatus(active).label : "检活已关闭"}${active.last_error ? ` · ${active.last_error}` : ""}` : "先创建渠道后解锁"} expanded={section === "health"} onToggle={() => toggle("health")}>
              {active ? <div className="space-y-4"><label className="flex items-center gap-2 text-sm"><Switch checked={form.health_check_enabled} onCheckedChange={(value) => set("health_check_enabled", value)} aria-label="允许渠道检活" />允许渠道检活</label><p className="text-xs text-muted-foreground">更改检活开关后，请保存渠道参数。</p><ProviderHealthPanel open={open && section === "health"} providerId={active.id} title={`模型检活 · ${active.name}`} /></div> : <p className="text-sm text-muted-foreground">请先创建渠道。</p>}
            </ProviderManagementCard>
            {active && <ProviderManagementCard id="provider-maintenance" title="维护" summary={active.archived ? "已归档 · 可恢复" : "归档或删除渠道"} expanded={section === "maintenance"} onToggle={() => toggle("maintenance")}>
              <div className="flex flex-wrap items-center justify-between gap-3"><div><Badge variant={active.archived ? "warning" : "neutral"}>{active.archived ? "已归档" : "未归档"}</Badge><p className="mt-1 text-xs text-muted-foreground">归档保留数据，删除会移除渠道及其模型路由。</p></div><div className="flex flex-wrap gap-2"><Button variant="outline" disabled={maintenance.isPending} onClick={() => maintenance.mutate("archive")}><Archive className="h-4 w-4" />{active.archived ? "取消归档" : "归档"}</Button><Button variant="destructive" disabled={maintenance.isPending} onClick={async () => { if (await confirmDelete(`渠道「${active.name}」`, "该渠道下的 Key 与模型路由也会失效。")) maintenance.mutate("delete") }}><Trash2 className="h-4 w-4" />删除渠道</Button></div></div>
            </ProviderManagementCard>}
          </fieldset>
          </div>
        </div>
        <div className="shrink-0 border-t bg-background px-4 py-3 sm:px-6">
          {error && <div role="alert" className="mb-2 text-sm text-destructive">{error instanceof Error ? error.message : "操作失败"}</div>}
          {notice && <div role="status" className="mb-2 text-sm text-emerald-600">{notice}</div>}
          {dirty && Object.entries(validation).length > 0 && <div role="alert" className="mb-2 text-xs text-destructive">{Object.values(validation).join("；")}</div>}
          <DialogFooter className="items-center">
            <span className="mr-auto text-xs text-muted-foreground">{dirty ? "渠道参数未保存" : "渠道参数无未保存修改"}{keysDirty ? " · Key 草稿未保存" : ""}{modelsDirty ? " · 模型草稿未保存" : ""} · 各资源分别保存</span>
            <Button variant="outline" disabled={save.isPending || maintenance.isPending || keysBusy || modelsBusy} onClick={() => void close()}>关闭</Button>
            <Button onClick={() => save.mutate({ ...form })} disabled={!canSave || !dirty || save.isPending || keysBusy || modelsBusy}>{save.isPending ? "保存中…" : active ? "保存渠道参数" : "创建渠道"}</Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  )
}
