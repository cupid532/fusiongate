import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Activity, Plus, Settings2, Trash2 } from "lucide-react"
import { providerKeysApi } from "@/lib/api"
import { providersApi, refreshProviderViews } from "@/lib/provider-management"
import type { ProviderKey } from "@/lib/types"
import { EgressSelect } from "@/components/EgressSelect"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import { QueryError } from "@/components/ui/query-error"
import { useConfirmDelete } from "@/components/ui/confirm"

function statusBadge(status?: string) {
  if (status === "healthy") return <Badge variant="success">正常</Badge>
  if (status === "failed" || status === "unhealthy") return <Badge variant="danger">异常</Badge>
  if (status === "pending" || status === "running") return <Badge variant="warning">检测中</Badge>
  return <Badge variant="neutral">未测试</Badge>
}

type KeyDraft = { name: string; egress: string; cost: string; enabled: boolean; health: boolean }
function keyDraft(key: ProviderKey): KeyDraft {
  return { name: key.name || "", egress: key.egress_mode === "node" ? "node:" + key.ip_pool_node_id : key.egress_mode || "inherit", cost: String(key.cost_multiplier || 1), enabled: key.enabled, health: key.health_check_enabled }
}
function keyPatch(key: ProviderKey, draft: KeyDraft): Record<string, unknown> {
  const patch: Record<string, unknown> = {}
  const baseline = keyDraft(key)
  if (draft.name.trim() !== baseline.name) patch.name = draft.name.trim()
  if (draft.egress !== baseline.egress) {
    const [mode, node] = draft.egress.split(":")
    patch.egress_mode = mode
    patch.ip_pool_node_id = mode === "node" ? Number(node) : null
  }
  if (Number(draft.cost) !== Number(baseline.cost)) patch.cost_multiplier = Number(draft.cost)
  if (draft.enabled !== baseline.enabled) patch.enabled = draft.enabled
  if (draft.health !== baseline.health) patch.health_check_enabled = draft.health
  return patch
}

type PanelProps = {
  open: boolean
  providerId: number
  providerName?: string
  onManageModels?: () => void
  onClose?: () => void
  onDirtyChange?: (dirty: boolean) => void
  onBusyChange?: (busy: boolean) => void
}

export function ProviderKeysPanel({ open, providerId, onManageModels, onClose, onDirtyChange, onBusyChange }: PanelProps) {
  const client = useQueryClient()
  const confirmDelete = useConfirmDelete()
  const [newKey, setNewKey] = useState("")
  const [newName, setNewName] = useState("")
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")
  const [drafts, setDrafts] = useState<Record<number, KeyDraft>>({})
  useEffect(() => { setNewKey(""); setNewName(""); setError(""); setNotice(""); setDrafts({}) }, [providerId])
  const nodes = useQuery({ queryKey: ["ippool"], queryFn: ({ signal }) => providersApi.nodes(signal), enabled: open })
  const keys = useQuery({ queryKey: ["provider-keys", providerId], queryFn: ({ signal }) => providerKeysApi.list(providerId, signal), enabled: open })
  const reportError = (reason: unknown) => setError(reason instanceof Error ? reason.message : "Key 操作失败")
  const add = useMutation({
    mutationFn: () => providerKeysApi.create(providerId, { api_key: newKey.trim(), name: newName.trim() || undefined, health_check_enabled: true }),
    onSuccess: async () => { setNewKey(""); setNewName(""); setNotice("Key 已添加，默认继承渠道出口。"); await refreshProviderViews(client) }, onError: reportError,
  })
  const patch = useMutation({
    mutationFn: ({ key, draft }: { key: ProviderKey; draft: KeyDraft }) => providerKeysApi.patch(providerId, key.id, keyPatch(key, draft)),
    onSuccess: async (_response, { key }) => { setError(""); await refreshProviderViews(client); setDrafts((previous) => { const next = { ...previous }; delete next[key.id]; return next }); setNotice("Key 配置已保存，不影响其他 Key 或渠道参数。") }, onError: reportError,
  })
  const remove = useMutation({ mutationFn: (keyId: number) => providerKeysApi.remove(providerId, keyId), onSuccess: async () => { await refreshProviderViews(client); setNotice("Key 已删除。") }, onError: reportError })
  const test = useMutation({ mutationFn: (keyId: number) => providerKeysApi.test(providerId, keyId), onSuccess: async () => { await refreshProviderViews(client); setNotice("测试请求已完成，请查看该 Key 的状态与错误详情。") }, onError: reportError })
  const busy = add.isPending || patch.isPending || remove.isPending || test.isPending
  const dirty = !!newKey.trim() || !!newName.trim() || (keys.data ?? []).some((key) => drafts[key.id] && Object.keys(keyPatch(key, drafts[key.id])).length > 0)
  useEffect(() => { onDirtyChange?.(dirty) }, [dirty, onDirtyChange])
  useEffect(() => { onBusyChange?.(busy) }, [busy, onBusyChange])
  function change(key: ProviderKey, value: Partial<KeyDraft>) {
    setError("")
    setNotice("")
    setDrafts((previous) => ({ ...previous, [key.id]: { ...(previous[key.id] ?? keyDraft(key)), ...value } }))
  }
  return <div className="space-y-4">
    <p className="text-xs text-muted-foreground">这里管理上游 API Key，区别于客户端使用的下游访问密钥。配置需点击每行“保存 Key”；测试与删除立即执行。</p>
    {error && <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</div>}
    {notice && <p role="status" className="text-xs text-emerald-600">{notice}</p>}
    <fieldset disabled={busy} className="grid gap-2 rounded-md border p-3 sm:grid-cols-[minmax(0,1fr)_9rem_auto] sm:items-end">
      <div className="space-y-1.5"><Label htmlFor="new-api-key">新 API Key</Label><Input id="new-api-key" type="password" autoComplete="new-password" value={newKey} onChange={(event) => setNewKey(event.target.value)} placeholder="sk-…" className="font-mono text-xs" /></div>
      <div className="space-y-1.5"><Label htmlFor="new-key-name">Key 名称</Label><Input id="new-key-name" value={newName} onChange={(event) => setNewName(event.target.value)} placeholder="例如：备用 Key" /></div>
      <Button disabled={!newKey.trim() || busy} onClick={() => add.mutate()}><Plus className="h-4 w-4" />{add.isPending ? "添加中…" : "添加 Key"}</Button>
    </fieldset>
    {onManageModels && <Button variant="outline" onClick={onManageModels}><Settings2 className="h-4 w-4" />管理此渠道的模型</Button>}
    {nodes.isError && <QueryError title="无法读取出口节点" error={nodes.error} onRetry={() => void nodes.refetch()} className="p-3" />}
    {keys.isPending ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">读取 Key 中…</p> : keys.isError ? <QueryError title="无法读取 Key" error={keys.error} onRetry={() => void keys.refetch()} /> : !keys.data.length ? <p className="py-6 text-center text-sm text-muted-foreground">尚未添加 Key。</p> : <div className="space-y-3">{keys.data.map((key) => {
      const draft = drafts[key.id] ?? keyDraft(key)
      const changed = Object.keys(keyPatch(key, draft)).length > 0
      const validCost = draft.cost.trim() !== "" && Number.isFinite(Number(draft.cost)) && Number(draft.cost) > 0 && Number(draft.cost) <= 1000
      const changedNode = draft.egress !== keyDraft(key).egress && draft.egress.startsWith("node:")
      const validNode = !changedNode || nodes.data?.some((node) => node.id === Number(draft.egress.slice(5)) && node.enabled)
      const nodeName = nodes.data?.find((node) => node.id === key.effective_node_id)?.name ?? (key.effective_node_id ? "节点 #" + key.effective_node_id : "本机直连")
      return <section key={key.id} className="rounded-lg border p-3">
        <div className="flex flex-wrap items-start justify-between gap-2"><div className="min-w-0"><div className="text-sm font-medium">{key.name || "未命名 Key"} {statusBadge(key.status)}</div><p className="mt-1 font-mono text-xs text-muted-foreground">{key.key_hint}</p><p className="mt-1 text-xs text-muted-foreground">当前有效出口：{key.egress_mode === "inherit" ? "继承渠道 → " : key.egress_mode === "direct" ? "强制直连 → " : "独立节点 → "}{key.effective_egress === "direct" ? "本机直连" : nodeName}</p>{key.last_error && <p className="mt-1 break-words text-xs text-destructive">{key.last_error}</p>}</div><div className="flex gap-1"><Button variant="outline" size="sm" onClick={() => test.mutate(key.id)} disabled={busy || changed} title={changed ? "请先保存或取消此 Key 的草稿" : "使用已保存配置测试，可能产生上游费用"}><Activity className="h-4 w-4" />{test.isPending && test.variables === key.id ? "测试中…" : "测试"}</Button><Button variant="ghost" size="icon" aria-label={"删除 " + (key.name || key.key_hint)} disabled={busy} onClick={async () => { if (await confirmDelete("Key " + (key.name || key.key_hint))) remove.mutate(key.id) }}><Trash2 className="h-4 w-4 text-destructive" /></Button></div></div>
        <fieldset disabled={busy} className="mt-3 grid gap-3 border-t pt-3 sm:grid-cols-2">
          <div className="space-y-1"><Label htmlFor={"key-name-" + key.id}>Key 名称</Label><Input id={"key-name-" + key.id} value={draft.name} onChange={(event) => change(key, { name: event.target.value })} /></div>
          <div className="space-y-1"><Label htmlFor={"key-egress-" + key.id}>出口</Label><EgressSelect id={"key-egress-" + key.id} value={draft.egress} onChange={(egress) => change(key, { egress })} nodes={nodes.data ?? []} inherited disabled={nodes.isPending || nodes.isError || busy} /></div>
          <div className="space-y-1"><Label htmlFor={"key-cost-" + key.id}>成本倍率</Label><Input id={"key-cost-" + key.id} type="number" min="0.01" max="1000" step="0.01" value={draft.cost} onChange={(event) => change(key, { cost: event.target.value })} aria-invalid={!validCost || undefined} />{!validCost && <p className="text-xs text-destructive">倍率必须大于 0 且不超过 1000。</p>}</div>
          <div className="flex flex-wrap items-center gap-4"><label className="flex items-center gap-2 text-xs"><Switch checked={draft.enabled} onCheckedChange={(enabled) => change(key, { enabled })} aria-label={(key.name || key.key_hint) + " 启用"} />启用 Key</label><label className="flex items-center gap-2 text-xs"><Switch checked={draft.health} onCheckedChange={(health) => change(key, { health })} aria-label={(key.name || key.key_hint) + " 检活"} />允许检活</label></div>
        </fieldset>
        <div className="mt-3 flex items-center justify-between gap-2"><span className="text-xs text-muted-foreground">{changed ? "此 Key 有未保存修改" : "此 Key 无未保存修改"}</span><div className="flex gap-2"><Button variant="outline" size="sm" disabled={!changed || busy} onClick={() => setDrafts((previous) => { const next = { ...previous }; delete next[key.id]; return next })}>取消修改</Button><Button size="sm" disabled={!changed || !validCost || !validNode || busy || nodes.isError} onClick={() => patch.mutate({ key, draft })}>{patch.isPending && patch.variables?.key.id === key.id ? "保存中…" : "保存 Key"}</Button></div></div>
      </section>
    })}</div>}
    {onClose && <div className="flex justify-end"><Button variant="outline" disabled={busy} onClick={onClose}>关闭</Button></div>}
  </div>
}

export function ProviderKeysDialog({ open, onOpenChange, providerId, providerName, onManageModels }: {
  open: boolean
  onOpenChange: (value: boolean) => void
  providerId: number
  providerName: string
  onManageModels?: () => void
}) {
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent className="max-h-[90vh] max-w-3xl overflow-y-auto"><DialogHeader><DialogTitle>Key 管理 · {providerName}</DialogTitle><DialogDescription>每行配置独立保存；模型配置请进入模型管理。</DialogDescription></DialogHeader><ProviderKeysPanel open={open} providerId={providerId} onManageModels={onManageModels} onClose={() => onOpenChange(false)} /></DialogContent></Dialog>
}
