import { useEffect, useMemo, useRef, useState } from "react"
import { api } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"

export type SetupModel = { model: string; display_name?: string; capabilities?: string; source?: "manual" | "discovered" }
export type ModelSetup = { inventory: SetupModel[]; selected: string[] }
type Connection = { type?: string; baseURL?: string; credential: string; ip_pool_node_id?: number; provider_id?: number }

export function useModelSetup(open: boolean, connection: Connection) {
  const signature = JSON.stringify(connection)
  const ready = !!connection.credential.trim() && (!!connection.provider_id || !!connection.type && /^https?:\/\//.test(connection.baseURL ?? ""))
  const [result, setResult] = useState<{ signature: string; inventory: SetupModel[] } | null>(null)
  const [selected, setSelected] = useState<string[]>([])
  const [manual, setManual] = useState("")
  const [skip, setSkip] = useState(false)
  const [error, setError] = useState("")
  const [loading, setLoading] = useState(false)
  const [retry, setRetry] = useState(0)
  const generation = useRef(0)
  const abortDiscovery = useRef<() => void>(() => {})
  useEffect(() => {
    const current = ++generation.current
    setResult(null); setSelected([]); setManual(""); setSkip(false); setError(""); setLoading(false)
    if (!open || !ready) return
    const controller = new AbortController()
    setLoading(true)
    const timer = setTimeout(() => {
      api<{ inventory: SetupModel[] }>("/api/admin/providers/preview-models", { method: "POST", signal: controller.signal, body: signature })
        .then((value) => { if (generation.current === current && !controller.signal.aborted) setResult({ signature, inventory: value.inventory ?? [] }) })
        .catch((reason) => { if (generation.current === current && !controller.signal.aborted) setError(reason instanceof Error ? reason.message : "获取模型失败") })
        .finally(() => { if (generation.current === current && !controller.signal.aborted) setLoading(false) })
    }, 700)
    abortDiscovery.current = () => { clearTimeout(timer); controller.abort(); setLoading(false) }
    return () => { clearTimeout(timer); controller.abort() }
  }, [open, signature, ready, retry])
  const inventory = useMemo(() => {
    const items = result?.signature === signature ? [...result.inventory] : []
    const seen = new Set(items.map((m) => m.model.toLowerCase()))
    for (const name of manual.split(/\r?\n/).map((m) => m.trim()).filter(Boolean)) {
      if (!seen.has(name.toLowerCase())) { items.push({ model: name, capabilities: "chat,stream", source: "manual" }); seen.add(name.toLowerCase()) }
    }
    return items
  }, [result, signature, manual])
  const manualNames = manual.split(/\r?\n/).map((m) => m.trim()).filter(Boolean)
  const invalid = manualNames.some((m) => new TextEncoder().encode(m).length > 512 || /[\s,]/.test(m) || [...m].some((c) => c.charCodeAt(0) < 32 || c.charCodeAt(0) === 127)) || inventory.length > 5000
  const validSelected = selected.filter((id) => inventory.some((m) => m.model === id))
  return { inventory, selected: validSelected, setSelected, manual, setManual, skip, setSkip, loading, error, invalid,
    ready: ready && !loading && !invalid && (skip || validSelected.length > 0),
    cancelDiscovery: () => abortDiscovery.current(),
    setup: { inventory: skip ? [] : inventory, selected: skip ? [] : validSelected } as ModelSetup,
    refresh: () => setRetry((value) => value + 1),
  }
}

export function ModelSetupFields({ state, disabled = false }: { state: ReturnType<typeof useModelSetup>; disabled?: boolean }) {
  const [search, setSearch] = useState("")
  const [selectedOnly, setSelectedOnly] = useState(false)
  const keyword = search.trim().toLowerCase()
  const visible = state.inventory.filter((m) => (!selectedOnly || state.selected.includes(m.model)) && `${m.model} ${m.display_name ?? ""}`.toLowerCase().includes(keyword))
  function toggle(model: string, checked: boolean) { state.setSelected((values) => checked ? [...new Set([...values, model])] : values.filter((m) => m !== model)) }
  return <fieldset disabled={disabled} className="min-w-0 space-y-3 rounded-lg border p-3">
    <legend className="px-1 text-sm font-medium">选择接入模型</legend>
    <p className="text-xs text-muted-foreground">连接信息填写完整后自动获取列表，不发起生成请求。列表存在不等于推理验证通过；勾选后保存会同时建立 Key 权限和同名请求路由。</p>
    {state.loading && <div className="flex flex-wrap items-center gap-2"><p role="status" className="text-sm">正在获取上游模型…</p><Button size="sm" variant="ghost" onClick={state.cancelDiscovery}>停止获取，改为手动配置</Button></div>}
    {state.error && <p role="alert" className="break-words text-sm text-destructive">{state.error}。可以重试或手动输入模型。</p>}
    <div className="flex flex-wrap items-center gap-2"><Button size="sm" variant="outline" disabled={disabled || state.loading} onClick={state.refresh}>重新获取</Button><span className="text-xs text-muted-foreground">已选 {state.selected.length} / {state.inventory.length}</span></div>
    {state.inventory.length > 0 && <>
      <Input aria-label="搜索待接入模型" placeholder="搜索模型" value={search} onChange={(event) => setSearch(event.target.value)} />
      <div className="flex flex-wrap items-center gap-2"><Button size="sm" variant="outline" onClick={() => state.setSelected((values) => [...new Set([...values, ...visible.map((m) => m.model)])])}>选择当前结果（{visible.length}）</Button><Button size="sm" variant="ghost" onClick={() => state.setSelected([])}>清空选择</Button><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={selectedOnly} onChange={(event) => setSelectedOnly(event.target.checked)} />仅看已选</label></div>
      <div className="max-h-64 overflow-y-auto rounded-md border divide-y">{visible.map((m) => <label key={m.model} className="flex items-start gap-2 p-2 text-sm"><input className="mt-1" type="checkbox" checked={state.selected.includes(m.model)} disabled={disabled || state.skip} onChange={(event) => toggle(m.model, event.target.checked)} /><span className="min-w-0 break-all font-mono">{m.model}{m.display_name && <span className="block text-xs text-muted-foreground">{m.display_name}</span>}</span></label>)}{visible.length === 0 && <p className="p-3 text-sm text-muted-foreground">当前筛选没有模型。</p>}</div>
    </>}
    {!state.loading && !state.error && state.inventory.length === 0 && <p className="text-xs text-muted-foreground">等待连接信息，或上游未返回模型。也可在下方手动添加。</p>}
    <label className="block space-y-1 text-xs"><span>手动添加模型（每行一个，添加后请勾选）</span><Textarea aria-label="手动添加待接入模型" rows={2} value={state.manual} onChange={(event) => state.setManual(event.target.value)} placeholder="vendor/model-v2" className="font-mono text-xs" /></label>
    {state.invalid && <p role="alert" className="text-xs text-destructive">模型名称不能含空格、逗号或控制字符，长度不超过 512，总数最多 5000。</p>}
    <label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={state.skip} onChange={(event) => state.setSkip(event.target.checked)} />仅保存配置，暂不接入模型</label>
    {state.skip && <p className="text-xs text-amber-600">保存后标记为待配置模型，不会授予任何模型权限。</p>}
  </fieldset>
}

// Reuse the ID for identical retries, but never for changed form inputs.
export function useSetupRequest() {
  const previous = useRef({ body: "", id: "" })
  return (body: Record<string, unknown>) => {
    const serialized = JSON.stringify(body)
    if (previous.current.body !== serialized) previous.current = { body: serialized, id: crypto.randomUUID() }
    return { ...body, request_id: previous.current.id }
  }
}
