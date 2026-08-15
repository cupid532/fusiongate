import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowRight, Plus, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { ModelAlias, Route } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"

export function ModelAliasManager({ routes }: { routes: Route[] }) {
  const qc = useQueryClient()
  const [alias, setAlias] = useState("")
  const [target, setTarget] = useState("")
  const models = [...new Set(routes.map((route) => route.public_name))].sort()
  const { data: aliases = [] } = useQuery({ queryKey: ["model-aliases"], queryFn: () => api<ModelAlias[]>("/api/admin/model-aliases") })
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["model-aliases"] })
    qc.invalidateQueries({ queryKey: ["routes"] })
  }
  const create = useMutation({ mutationFn: () => api("/api/admin/model-aliases", { method: "POST", body: JSON.stringify({ alias, target_model: target, enabled: true }) }), onSuccess: () => { setAlias(""); setTarget(""); refresh() } })
  const update = useMutation({ mutationFn: ({ name, patch }: { name: string; patch: Record<string, unknown> }) => api(`/api/admin/model-aliases/${encodeURIComponent(name)}`, { method: "PATCH", body: JSON.stringify(patch) }), onSuccess: refresh })
  const remove = useMutation({ mutationFn: (name: string) => api(`/api/admin/model-aliases/${encodeURIComponent(name)}`, { method: "DELETE" }), onSuccess: refresh })

  return (
    <Card className="mb-4 overflow-hidden">
      <CardContent className="p-0">
        <div className="border-b bg-muted/30 px-4 py-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div><div className="text-sm font-semibold">模型别名</div><div className="text-xs text-muted-foreground">例如 `/glm5.2` 路由到 `glm5-2`，继续使用目标模型的轮询和故障转移。</div></div>
            <Badge variant="neutral">{aliases.length} 条</Badge>
          </div>
        </div>
        <div className="grid gap-2 border-b p-3 sm:grid-cols-[1fr_24px_1fr_auto]">
          <Input value={alias} onChange={(event) => setAlias(event.target.value)} placeholder="/glm5.2" className="font-mono" />
          <ArrowRight className="mx-auto hidden self-center text-muted-foreground sm:block" />
          <select value={target} onChange={(event) => setTarget(event.target.value)} className="h-9 rounded-md border border-input bg-transparent px-3 font-mono text-sm">
            <option value="">选择目标模型</option>
            {models.map((model) => <option key={model} value={model}>{model}</option>)}
          </select>
          <Button onClick={() => create.mutate()} disabled={!alias.trim() || !target || create.isPending}><Plus />添加</Button>
          {create.error && <div className="text-xs text-destructive sm:col-span-4">{create.error.message}</div>}
        </div>
        {aliases.length === 0 ? <div className="px-4 py-5 text-center text-sm text-muted-foreground">暂无模型别名</div> : (
          <div className="divide-y">
            {aliases.map((item) => (
              <div key={item.alias} className="grid gap-3 px-4 py-3 sm:grid-cols-[minmax(0,1fr)_24px_minmax(0,1fr)_auto] sm:items-center">
                <div className="truncate font-mono text-sm font-semibold">{item.alias}</div>
                <ArrowRight className="hidden text-muted-foreground sm:block" />
                <select value={item.target_model} onChange={(event) => update.mutate({ name: item.alias, patch: { target_model: event.target.value } })} className="h-8 min-w-0 rounded-md border border-input bg-transparent px-2 font-mono text-xs">
                  {models.map((model) => <option key={model} value={model}>{model}</option>)}
                </select>
                <div className="flex items-center justify-end gap-2">
                  <Switch checked={item.enabled} onCheckedChange={(enabled) => update.mutate({ name: item.alias, patch: { enabled } })} />
                  <Button variant="ghost" size="icon" aria-label="删除别名" onClick={() => { if (confirm(`删除别名「${item.alias}」？`)) remove.mutate(item.alias) }}><Trash2 className="text-destructive" /></Button>
                </div>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
