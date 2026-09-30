import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { applyProviderPatches, providersApi, refreshProviderViews, type BatchItemResult, type ProviderPatch } from "@/lib/provider-management"
import type { Provider } from "@/lib/types"
import { BatchResultPanel } from "@/components/BatchResultPanel"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { QueryError } from "@/components/ui/query-error"

export type ProviderBatchEdit = "priority" | "group" | "archive" | "unarchive"
const titles: Record<ProviderBatchEdit, string> = { priority: "批量设置优先级", group: "批量设置分组", archive: "批量归档", unarchive: "批量取消归档" }

export function ProviderBatchDialog({ open, onOpenChange, providers, action, onApplied }: {
  open: boolean
  onOpenChange: (open: boolean) => void
  providers: Provider[]
  action: ProviderBatchEdit
  onApplied?: (ids: number[]) => void
}) {
  const client = useQueryClient()
  const [value, setValue] = useState("")
  const [results, setResults] = useState<BatchItemResult[]>([])
  const groups = useQuery({ queryKey: ["provider-groups"], queryFn: ({ signal }) => providersApi.groups(signal), enabled: open && action === "group" })
  useEffect(() => { if (open) { setValue(""); setResults([]) } }, [open, action])
  const save = useMutation({
    mutationFn: async (targets: Provider[]) => {
      const patch: ProviderPatch = action === "priority" ? { priority: Number(value) } : action === "group" ? Number(value) > 0 ? { group_id: Number(value) } : { clear_group: true } : { archived: action === "archive" }
      const retained = results.filter((item) => !targets.some((target) => target.id === item.id))
      const updated = await applyProviderPatches(targets, patch, (progress) => setResults([...retained, ...progress]))
      return [...retained, ...updated]
    },
    onSuccess: async (updated) => { await refreshProviderViews(client); onApplied?.(updated.filter((item) => item.status === "success").map((item) => item.id)) },
  })
  const valid = action === "priority" ? value.trim() !== "" && Number.isSafeInteger(Number(value)) && Number(value) >= 0 : action !== "group" || value !== "" && !groups.isPending && !groups.isError
  const failedIds = new Set(results.filter((item) => item.status === "error" || item.status === "skipped").map((item) => item.id))
  const retryable = providers.filter((provider) => failedIds.has(provider.id))
  return <Dialog open={open} onOpenChange={(next) => { if (!save.isPending) onOpenChange(next) }}><DialogContent className="max-w-xl">
    <DialogHeader><DialogTitle>{titles[action]}</DialogTitle><DialogDescription>已选 {providers.length} 个渠道。使用现有单渠道接口逐项保存，可能部分成功；不会自动回滚已完成的配置。</DialogDescription></DialogHeader>
    {action === "priority" && <div className="space-y-2"><Label htmlFor="batch-priority">优先级（数字越大越优先）</Label><Input id="batch-priority" type="number" min={0} step={1} value={value} onChange={(event) => setValue(event.target.value)} disabled={save.isPending || results.length > 0} /></div>}
    {action === "group" && <div className="space-y-2"><Label htmlFor="batch-group">目标分组</Label>{groups.isError ? <QueryError error={groups.error} onRetry={() => void groups.refetch()} /> : <select id="batch-group" value={value} onChange={(event) => setValue(event.target.value)} disabled={groups.isPending || save.isPending || results.length > 0} className="h-9 w-full rounded-md border bg-background px-2 text-sm"><option value="" disabled>{groups.isPending ? "读取分组中…" : "请选择分组"}</option><option value="0">移出分组</option>{groups.data?.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}</select>}</div>}
    {action === "archive" && <p className="text-sm text-muted-foreground">归档保留 Key、模型与历史数据，但渠道不再参与调度。取消归档不会替你启用已停用的渠道。</p>}
    {!results.length && <p className="max-h-24 overflow-y-auto text-xs text-muted-foreground">{providers.map((provider) => provider.name).join("、")}</p>}
    <BatchResultPanel results={results} />
    {save.isPending && <p role="status" className="text-xs text-muted-foreground">已处理 {results.length} / {providers.length}，请勿关闭页面。</p>}
    <DialogFooter><Button variant="outline" disabled={save.isPending} onClick={() => onOpenChange(false)}>关闭</Button>{results.length ? retryable.length > 0 && !results.some((item) => item.status === "unknown") && <Button disabled={save.isPending} onClick={() => save.mutate(retryable)}>仅重试失败 / 未执行项</Button> : <Button disabled={!valid || !providers.length || save.isPending} onClick={() => save.mutate(providers)}>{save.isPending ? "应用中…" : "确认应用"}</Button>}</DialogFooter>
  </DialogContent></Dialog>
}
