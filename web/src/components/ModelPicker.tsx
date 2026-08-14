import { useEffect, useState } from "react"
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
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerId: number
  providerName: string
}) {
  const qc = useQueryClient()
  const [models, setModels] = useState<DiscoveredModel[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")

  useEffect(() => {
    if (open && providerId) {
      setLoading(true)
      setError("")
      api<DiscoverResult>(`/api/admin/providers/${providerId}/discover-models`, { method: "POST" })
        .then((r) => {
          setModels(r.models ?? [])
          setSelected(new Set((r.models ?? []).filter((m) => !m.unavailable).map((m) => m.id)))
        })
        .catch((e) => setError(e instanceof Error ? e.message : "识别失败"))
        .finally(() => setLoading(false))
    }
  }, [open, providerId])

  const importModels = useMutation({
    mutationFn: async () => api(`/api/admin/providers/${providerId}/import-models`, { method: "POST", body: JSON.stringify({ models: [...selected] }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      onOpenChange(false)
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>识别模型 · {providerName}</DialogTitle>
          <DialogDescription>选择要启用的模型，导入后将建立统一模型路由。</DialogDescription>
        </DialogHeader>

        {loading ? (
          <div className="py-8 text-center text-sm text-muted-foreground">正在识别模型…</div>
        ) : error ? (
          <div className="py-8 text-center text-sm text-destructive">{error}</div>
        ) : (
          <>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground">
                已选择 <span className="font-semibold text-primary">{selected.size}</span> / {models.length} 个模型
              </span>
              <div className="flex gap-2">
                <Button variant="outline" size="sm" onClick={() => setSelected(new Set(models.filter((m) => !m.unavailable).map((m) => m.id)))}>
                  全选
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setSelected(new Set())}>
                  清空
                </Button>
              </div>
            </div>
            <div className="max-h-[50vh] space-y-1 overflow-y-auto rounded-lg border p-2">
              {models.map((m) => (
                <label
                  key={m.id}
                  className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2 hover:bg-muted/40"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(m.id)}
                    disabled={m.unavailable}
                    onChange={(e) => {
                      const next = new Set(selected)
                      if (e.target.checked) next.add(m.id)
                      else next.delete(m.id)
                      setSelected(next)
                    }}
                  />
                  <span className="flex-1">
                    <span className="block font-mono text-sm">{m.id}</span>
                    {m.display_name && <span className="block text-xs text-muted-foreground">{m.display_name}</span>}
                  </span>
                  {m.existing && <Badge variant="neutral">已存在</Badge>}
                  {m.unavailable && <Badge variant="danger">不可用</Badge>}
                </label>
              ))}
            </div>
          </>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={() => importModels.mutate()} disabled={loading || selected.size === 0 || importModels.isPending}>
            {importModels.isPending ? "导入中…" : "导入所选模型"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
