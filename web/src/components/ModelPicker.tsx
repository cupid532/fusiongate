import { useEffect, useMemo, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"

type DiscoveredModel = {
  id: string
  display_name?: string
  capabilities: string
  public_names?: string[]
  existing?: boolean
  excluded?: boolean
  unavailable?: boolean
}

type DiscoverResult = {
  discovered: number
  skipped: number
  models: DiscoveredModel[]
}

export function ModelPicker({
  open,
  onOpenChange,
  providerId,
  providerName,
  providerIds,
  mixedTypes = false,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerId: number
  providerName: string
  providerIds?: number[]
  /**
   * The selection spans several platforms (Codex + Grok, say). One inventory
   * cannot be shared across them, so the picker offers the only sensible bulk
   * action instead: discover and enable every model on each credential.
   */
  mixedTypes?: boolean
}) {
  const qc = useQueryClient()
  const [models, setModels] = useState<DiscoveredModel[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [result, setResult] = useState("")

  const targets = providerIds?.length ? providerIds : [providerId]
  const bulk = mixedTypes && targets.length > 1

  useEffect(() => {
    if (open && providerId && !bulk) {
      setLoading(true)
      setError("")
      setResult("")
      api<DiscoverResult>(`/api/admin/providers/${providerId}/discover-models`, { method: "POST" })
        .then((r) => {
          setModels(r.models ?? [])
          // 已启用（existing）的模型默认打勾
          setSelected(new Set((r.models ?? []).filter((m) => m.existing).map((m) => m.id)))
        })
        .catch((e) => setError(e instanceof Error ? e.message : "识别失败"))
        .finally(() => setLoading(false))
    }
  }, [open, providerId, bulk])

  const syncAll = useMutation({
    mutationFn: () =>
      api<{ providers: number; succeeded: number; failed: number; models: number }>("/api/admin/auth/models/sync", {
        method: "POST",
        body: JSON.stringify({ provider_ids: targets }),
      }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      setResult(`${r.succeeded}/${r.providers} 个认证识别成功，共启用 ${r.models} 个模型${r.failed ? `，${r.failed} 个失败` : ""}`)
    },
    onError: (e) => setError(e instanceof Error ? e.message : "识别失败"),
  })

  const enabledModels = useMemo(() => models.filter((m) => m.existing).sort((a, b) => a.id.localeCompare(b.id)), [models])
  const disabledModels = useMemo(() => models.filter((m) => !m.existing).sort((a, b) => a.id.localeCompare(b.id)), [models])

  const save = useMutation({
    mutationFn: async () =>
      targets.length > 1
        ? api<{ selected: number; added: number; existing: number; removed: number }>("/api/admin/providers/batch", {
            method: "POST",
            body: JSON.stringify({ provider_ids: targets, action: "models", models: [...selected] }),
          })
        : api<{ selected: number; added: number; existing: number; removed: number }>(
            `/api/admin/providers/${providerId}/models`,
            { method: "PUT", body: JSON.stringify({ models: [...selected] }) }
          ),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      setResult(`已保存：启用 ${r.existing + r.added} 个，关闭 ${r.removed} 个`)
      // 重新标记 existing 状态
      setModels((prev) => prev.map((m) => ({ ...m, existing: selected.has(m.id) })))
    },
  })

  function toggle(id: string, checked: boolean) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (checked) next.add(id)
      else next.delete(id)
      return next
    })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{targets.length > 1 ? `批量管理模型 · ${targets.length} 个认证` : `管理模型 · ${providerName}`}</DialogTitle>
          <DialogDescription>
            {bulk
              ? "所选认证来自不同平台，模型清单无法互通；可为每个认证各自识别并启用全部模型。"
              : targets.length > 1
                ? "从首个认证识别模型，并将勾选结果应用到所有选中的同平台认证。「全部启用」再保存即等同于一键识别。"
                : "勾选即启用，取消勾选即关闭。已启用的模型排在上方。"}
          </DialogDescription>
        </DialogHeader>

        {bulk ? (
          <div className="space-y-3">
            {error && <div className="text-sm text-destructive">{error}</div>}
            {result && <div className="rounded-md bg-primary/10 px-3 py-2 text-xs text-primary">{result}</div>}
            {!result && (
              <div className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">
                将对 {targets.length} 个认证分别向上游查询可用模型，并把发现的模型全部加入路由。已有的路由不会被删除。
              </div>
            )}
          </div>
        ) : loading ? (
          <div className="py-8 text-center text-sm text-muted-foreground">正在识别模型…</div>
        ) : error ? (
          <div className="py-8 text-center text-sm text-destructive">{error}</div>
        ) : (
          <>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">
                已启用 <span className="font-semibold text-primary">{selected.size}</span> / {models.length} 个模型
              </span>
              <div className="flex gap-2">
                <Button variant="outline" size="sm" onClick={() => setSelected(new Set(models.filter((m) => !m.unavailable).map((m) => m.id)))}>
                  全部启用
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
                  全部关闭
                </Button>
              </div>
            </div>

            <div className="max-h-[50vh] space-y-4 overflow-y-auto">
              {enabledModels.length > 0 && (
                <div>
                  <div className="mb-1 text-xs font-medium text-muted-foreground">已启用</div>
                  <div className="space-y-1">
                    {enabledModels.map((m) => (
                      <ModelRow key={m.id} m={m} checked={selected.has(m.id)} onToggle={toggle} />
                    ))}
                  </div>
                </div>
              )}
              {disabledModels.length > 0 && (
                <div>
                  <div className="mb-1 text-xs font-medium text-muted-foreground">未启用</div>
                  <div className="space-y-1">
                    {disabledModels.map((m) => (
                      <ModelRow key={m.id} m={m} checked={selected.has(m.id)} onToggle={toggle} />
                    ))}
                  </div>
                </div>
              )}
              {models.length === 0 && <div className="py-6 text-center text-sm text-muted-foreground">没有发现模型</div>}
            </div>

            {result && <div className="rounded-md bg-primary/10 px-3 py-2 text-xs text-primary">{result}</div>}
          </>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            关闭
          </Button>
          {bulk ? (
            <Button onClick={() => syncAll.mutate()} disabled={syncAll.isPending || !!result}>
              {syncAll.isPending ? "识别中…" : `识别并启用全部模型（${targets.length} 个认证）`}
            </Button>
          ) : (
            <Button onClick={() => save.mutate()} disabled={loading || save.isPending}>
              {save.isPending ? "保存中…" : "保存模型设置"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ModelRow({ m, checked, onToggle }: { m: DiscoveredModel; checked: boolean; onToggle: (id: string, v: boolean) => void }) {
  return (
    <label className="flex cursor-pointer items-center gap-3 rounded-lg border px-3 py-2 hover:bg-muted/40">
      <input
        type="checkbox"
        checked={checked}
        disabled={m.unavailable}
        onChange={(e) => onToggle(m.id, e.target.checked)}
      />
      <span className="flex-1">
        <span className="block font-mono text-sm">{m.id}</span>
        {m.display_name && <span className="block text-xs text-muted-foreground">{m.display_name}</span>}
      </span>
      {m.unavailable ? (
        <Badge variant="danger">不可用</Badge>
      ) : m.excluded ? (
        <Badge variant="neutral">已关闭</Badge>
      ) : checked ? (
        <Badge variant="success">启用</Badge>
      ) : (
        <Badge variant="neutral">关闭</Badge>
      )}
    </label>
  )
}
