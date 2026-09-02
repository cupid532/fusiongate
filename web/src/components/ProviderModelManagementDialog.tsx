import { useEffect, useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Check, RefreshCw, Save, Search, Trash2 } from "lucide-react"
import { providerModelsApi } from "@/lib/api"
import type { ProviderKey, ProviderKeyModel } from "@/lib/types"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"

function statusBadge(status?: string) {
  if (status === "healthy") return <Badge variant="success">正常</Badge>
  if (status === "failed" || status === "unhealthy") return <Badge variant="danger">异常</Badge>
  if (status === "pending" || status === "running") return <Badge variant="warning">检测中</Badge>
  return <Badge variant="neutral">{status || "未测试"}</Badge>
}

function keyLabel(key: ProviderKey) {
  return key.name || key.key_hint || `Key #${key.id}`
}

type Draft = { policy: "fallback" | "allowlist"; models: Set<string>; removed: Set<string> }

function draftFor(key: ProviderKey): Draft {
  return {
    policy: key.model_policy === "allowlist" ? "allowlist" : "fallback",
    models: new Set((key.models ?? []).filter((model) => model.enabled).map((model) => model.model)),
    removed: new Set(),
  }
}

function sameDraft(a: Draft, b: Draft) {
  return a.policy === b.policy && [...a.models].sort().join("\n") === [...b.models].sort().join("\n") && [...a.removed].sort().join("\n") === [...b.removed].sort().join("\n")
}

export function ProviderModelManagementDialog({ open, onOpenChange, providerId, providerName }: {
  open: boolean
  onOpenChange: (value: boolean) => void
  providerId: number
  providerName: string
}) {
  const qc = useQueryClient()
  const [selectedKeyId, setSelectedKeyId] = useState<number | null>(null)
  const [search, setSearch] = useState("")
  const [error, setError] = useState("")
  const [drafts, setDrafts] = useState<Record<number, Draft>>({})

  const { data: keys = [], isLoading } = useQuery({ queryKey: ["provider-keys", providerId], queryFn: () => providerModelsApi.listKeys(providerId), enabled: open })

  useEffect(() => {
    if (!open) return
    setSearch("")
    setError("")
    setSelectedKeyId(null)
    setDrafts({})
  }, [open, providerId])

  useEffect(() => {
    setDrafts((current) => {
      const next = { ...current }
      for (const key of keys) if (!next[key.id]) next[key.id] = draftFor(key)
      for (const id of Object.keys(next)) if (!keys.some((key) => key.id === Number(id))) delete next[Number(id)]
      return next
    })
    if (selectedKeyId == null && keys[0]) setSelectedKeyId(keys[0].id)
    else if (selectedKeyId != null && !keys.some((key) => key.id === selectedKeyId)) setSelectedKeyId(keys[0]?.id ?? null)
  }, [keys, selectedKeyId])

  const selectedKey = keys.find((key) => key.id === selectedKeyId) ?? null
  const draft = selectedKey ? drafts[selectedKey.id] ?? draftFor(selectedKey) : null
  const models = useMemo(() => {
    const items = selectedKey?.models ?? []
    const keyword = search.trim().toLowerCase()
    return keyword ? items.filter((model) => `${model.model} ${model.display_name}`.toLowerCase().includes(keyword)) : items
  }, [selectedKey, search])
  const dirty = selectedKey && draft ? !sameDraft(draft, draftFor(selectedKey)) : false
  const updateDraft = (key: ProviderKey, fn: (draft: Draft) => Draft) => setDrafts((current) => ({ ...current, [key.id]: fn(current[key.id] ?? draftFor(key)) }))

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["provider-keys", providerId] })
    void qc.invalidateQueries({ queryKey: ["providers"] })
    void qc.invalidateQueries({ queryKey: ["routes"] })
  }
  const save = useMutation({
    mutationFn: () => providerModelsApi.saveManagement(providerId, keys.map((key) => { const value = drafts[key.id] ?? draftFor(key); return { key_id: key.id, model_policy: value.policy, models: [...value.models], exclude_models: [...value.removed] } })),
    onSuccess: () => { setError(""); refresh() },
    onError: (reason) => setError(reason instanceof Error ? reason.message : "保存模型设置失败"),
  })
  const discover = useMutation({ mutationFn: (keyId: number) => providerModelsApi.discover(providerId, keyId), onSuccess: refresh, onError: (reason) => setError(reason instanceof Error ? reason.message : "识别模型失败") })

  const setModels = (key: ProviderKey, selected: string[]) => updateDraft(key, (value) => ({ ...value, models: new Set(selected), removed: new Set([...value.removed].filter((model) => !selected.includes(model))) }))
  const toggleModel = (key: ProviderKey, model: ProviderKeyModel, enabled: boolean) => {
    const selected = new Set((drafts[key.id] ?? draftFor(key)).models)
    if (enabled) selected.add(model.model)
    else selected.delete(model.model)
    setModels(key, [...selected])
  }
  const allModels = selectedKey?.models ?? []
  const enabledCount = draft?.models.size ?? 0

  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="flex max-h-[90vh] max-w-5xl flex-col overflow-hidden">
      <DialogHeader><DialogTitle>模型管理 · {providerName}</DialogTitle><DialogDescription>按 Key 管理真实模型清单；保存后才会参与网关路由。</DialogDescription></DialogHeader>
      {error && <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</div>}
      {isLoading ? <div className="py-8 text-center text-sm text-muted-foreground">加载中…</div> : keys.length === 0 ? <div className="py-8 text-center text-sm text-muted-foreground">还没有 API Key，请先在 Key 管理中添加。</div> : <div className="grid min-h-0 flex-1 gap-4 md:grid-cols-[13rem_minmax(0,1fr)]">
        <aside className="min-h-0 space-y-1 overflow-y-auto rounded-md border p-2"><div className="px-2 pb-2 text-xs font-medium text-muted-foreground">API Keys（{keys.length}）</div>{keys.map((key) => { const value = drafts[key.id] ?? draftFor(key); return <button key={key.id} type="button" onClick={() => setSelectedKeyId(key.id)} className={`flex w-full items-start justify-between gap-2 rounded-md px-2 py-2 text-left text-sm ${selectedKeyId === key.id ? "bg-primary/10 text-primary" : "hover:bg-muted/50"}`}><span className="min-w-0"><span className="block truncate font-medium">{keyLabel(key)}</span><span className="block truncate font-mono text-[11px] text-muted-foreground">{key.key_hint}</span><span className="block text-[11px] text-muted-foreground">{value.policy === "allowlist" ? "仅清单" : "兼容模式"}{value.policy === "allowlist" && value.models.size === 0 ? " · 未承担模型" : ""}</span></span><span className="shrink-0 text-xs text-muted-foreground">{value.models.size}</span></button> })}</aside>
        <section className="min-h-0 overflow-y-auto rounded-md border">{selectedKey && draft && <>
          <div className="flex flex-wrap items-center justify-between gap-3 border-b p-3"><div><div className="text-sm font-medium">{keyLabel(selectedKey)}</div><div className="font-mono text-xs text-muted-foreground">{selectedKey.key_hint} · Key 已启用 {enabledCount}/{allModels.length} · 公共路由 {selectedKey.routable_models ?? allModels.filter((model) => model.route_status === "routed").length}</div>{draft.policy === "fallback" ? <div className="mt-1 text-xs text-amber-600">兼容模式：已发现库存按 Key 开关生效；无库存时才使用渠道默认模型。</div> : draft.models.size === 0 ? <div className="mt-1 text-xs text-destructive">allowlist 为空：此 Key 当前不承担任何模型。</div> : <div className="mt-1 text-xs text-muted-foreground">仅清单中的模型可路由。</div>}</div><div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" onClick={() => discover.mutate(selectedKey.id)} disabled={discover.isPending}><RefreshCw className={discover.isPending ? "animate-spin" : ""} />{discover.isPending ? "识别中…" : "识别模型"}</Button><Button size="sm" variant="outline" onClick={() => setModels(selectedKey, allModels.map((model) => model.model))} disabled={save.isPending || allModels.length === 0}><Check />全选</Button><Button size="sm" variant="outline" onClick={() => setModels(selectedKey, [])} disabled={save.isPending}><Check />全不选</Button><Button size="sm" onClick={() => save.mutate()} disabled={save.isPending || !dirty}><Save />{save.isPending ? "保存中…" : "保存全部 Key"}</Button></div></div>
          <div className="border-b p-3"><div className="relative"><Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" /><Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索模型" className="pl-8 font-mono text-xs" /></div></div>
          {models.length === 0 ? <div className="p-8 text-center text-sm text-muted-foreground">{allModels.length === 0 ? "尚无模型，请先识别该 Key 的模型。" : "没有匹配的模型。"}</div> : <div className="divide-y">{models.map((model) => <div key={model.model} className="flex items-center gap-3 px-3 py-2.5 hover:bg-muted/30"><Switch checked={draft.models.has(model.model)} disabled={save.isPending} onCheckedChange={(enabled) => toggleModel(selectedKey, model, enabled)} aria-label={`${keyLabel(selectedKey)} 模型 ${model.model}`} /><div className="min-w-0 flex-1"><div className="truncate font-mono text-sm" title={model.model}>{model.display_name || model.model}</div>{model.display_name && model.display_name !== model.model && <div className="truncate font-mono text-[11px] text-muted-foreground">{model.model}</div>}{model.health_error && <div className="break-words text-xs text-destructive">{model.health_error}</div>}{model.route_status === "routed" ? <div className="truncate text-xs text-emerald-600">公共路由：{model.public_names?.join(", ")}</div> : <div className="text-xs text-muted-foreground">尚未接入公共路由</div>}</div><div className="hidden shrink-0 text-right text-xs text-muted-foreground sm:block">{model.last_checked_at ? `${model.latency_ms} ms` : "尚未检活"}</div>{statusBadge(model.health_status)}<Button variant="ghost" size="icon" className="h-8 w-8" title="删除此模型" aria-label={`删除模型 ${model.model}`} disabled={save.isPending} onClick={() => updateDraft(selectedKey, (value) => { const models = new Set(value.models); models.delete(model.model); const removed = new Set(value.removed); removed.add(model.model); return { ...value, models, removed } })}><Trash2 className="h-3.5 w-3.5 text-destructive" /></Button></div>)}</div>}
        </>}</section>
      </div>}
      <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
