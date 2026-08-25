import { useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Plus, Power, PowerOff, Trash2, X } from "lucide-react"
import { api } from "@/lib/api"
import type { ModelAlias } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

export function ModelAliasManager({ model, aliases, upstreamModels }: { model: string; aliases: ModelAlias[]; upstreamModels: string[] }) {
  const qc = useQueryClient()
  const [adding, setAdding] = useState(false)
  const [alias, setAlias] = useState("")
  const modelAliases = useMemo(() => aliases.filter((item) => item.target_model === model), [aliases, model])
  const slashCandidates = useMemo(() => [...new Set([model, ...upstreamModels].filter((name) => name && !name.startsWith("/")).map((name) => `/${name}`))], [model, upstreamModels])
  const availableSlashAliases = slashCandidates.filter((name) => !aliases.some((item) => item.alias === name))
  const conflictingSlashAliases = slashCandidates.filter((name) => aliases.some((item) => item.alias === name && item.target_model !== model))
  const refresh = () => qc.invalidateQueries({ queryKey: ["model-aliases"] })

  const create = useMutation({
    mutationFn: (name: string) => api("/api/admin/model-aliases", { method: "POST", body: JSON.stringify({ alias: name, target_model: model, enabled: true }) }),
    onSuccess: () => { setAlias(""); setAdding(false); refresh() },
  })
  const update = useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) => api(`/api/admin/model-aliases/${encodeURIComponent(name)}`, { method: "PATCH", body: JSON.stringify({ enabled }) }),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: (name: string) => api(`/api/admin/model-aliases/${encodeURIComponent(name)}`, { method: "DELETE" }),
    onSuccess: refresh,
  })
  const error = create.error ?? update.error ?? remove.error

  return (
    <div className="border-t bg-muted/15 px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">调用名称</span>
            <Badge variant="outline"><span className="font-mono">{model}</span> · 规范</Badge>
            {modelAliases.map((item) => (
              <span key={item.alias} className="inline-flex items-center gap-1 rounded-full border bg-background py-0.5 pl-2.5 pr-1">
                <span className={`font-mono text-xs ${item.enabled ? "text-foreground" : "text-muted-foreground line-through"}`}>{item.alias}</span>
                <button
                  className="rounded-full p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                  onClick={() => update.mutate({ name: item.alias, enabled: !item.enabled })}
                  title={item.enabled ? "停用调用别名" : "启用调用别名"}
                  aria-label={`${item.enabled ? "停用" : "启用"} ${item.alias}`}
                >
                  {item.enabled ? <Power className="h-3 w-3 text-emerald-600" /> : <PowerOff className="h-3 w-3" />}
                </button>
                <button
                  className="rounded-full p-1 text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive"
                  onClick={() => { if (confirm(`删除调用别名「${item.alias}」？`)) remove.mutate(item.alias) }}
                  title="删除调用别名"
                  aria-label={`删除 ${item.alias}`}
                ><Trash2 className="h-3 w-3" /></button>
              </span>
            ))}
          </div>
          <p className="mt-1.5 text-xs text-muted-foreground">这些名称进入同一个轮询、熔断与故障转移组；上游仍使用下方各渠道自己的模型名。</p>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {availableSlashAliases.length > 0 && (
            <select
              value=""
              onChange={(event) => { if (event.target.value) create.mutate(event.target.value) }}
              disabled={create.isPending}
              className="h-8 max-w-72 rounded-md border border-input bg-transparent px-2 font-mono text-xs"
              title="在规范模型名或上游模型名前添加 /，作为新的调用名称"
            >
              <option value="">添加 / 前缀调用名…</option>
              {availableSlashAliases.map((name) => <option key={name} value={name}>{name}</option>)}
            </select>
          )}
          {conflictingSlashAliases.length > 0 && <Badge variant="warning">{conflictingSlashAliases.length} 个 / 调用名已属于其他组</Badge>}
          <Button variant={adding ? "ghost" : "outline"} size="sm" onClick={() => { setAdding((value) => !value); create.reset() }}>
            {adding ? <X className="h-3.5 w-3.5" /> : <Plus className="h-3.5 w-3.5" />}{adding ? "取消" : "自定义调用名"}
          </Button>
        </div>
      </div>
      {adding && (
        <div className="mt-3 flex max-w-xl flex-col gap-2 sm:flex-row">
          <Input
            value={alias}
            onChange={(event) => setAlias(event.target.value)}
            onKeyDown={(event) => { if (event.key === "Enter" && alias.trim()) create.mutate(alias) }}
            placeholder={`例如 /${model}`}
            className="h-8 font-mono text-xs"
            autoFocus
          />
          <Button size="sm" onClick={() => create.mutate(alias)} disabled={!alias.trim() || create.isPending}>{create.isPending ? "添加中…" : "添加"}</Button>
        </div>
      )}
      {error && <div className="mt-2 text-xs text-destructive">{error.message}</div>}
    </div>
  )
}
