import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { providersApi, refreshProviderViews, isUncertainError } from "@/lib/provider-management"
import { EgressSelect } from "@/components/EgressSelect"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { QueryError } from "@/components/ui/query-error"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

export function ProviderEgressDialog({ open, onOpenChange, providerIds, onApplied }: {
  open: boolean
  onOpenChange: (value: boolean) => void
  providerIds: number[]
  onApplied?: (ids: number[]) => void
}) {
  const client = useQueryClient()
  const [value, setValue] = useState("")
  const [message, setMessage] = useState("")
  const [verifyMessage, setVerifyMessage] = useState("")
  const nodes = useQuery({ queryKey: ["ippool"], queryFn: ({ signal }) => providersApi.nodes(signal), enabled: open })
  const providers = useQuery({ queryKey: ["providers"], queryFn: ({ signal }) => providersApi.list(signal), enabled: open })
  const ids = [...new Set(providerIds)]
  const selected = providers.data?.filter((provider) => ids.includes(provider.id)) ?? []
  const nodeId = value === "direct" ? 0 : Number(value.slice(5))
  const valid = value === "direct" || value.startsWith("node:") && nodes.data?.some((node) => node.id === nodeId && node.enabled)
  const save = useMutation({
    mutationFn: () => providersApi.batch(ids, "egress", nodeId),
    onSuccess: async (result) => {
      setMessage(`已更新 ${result.affected} 个渠道的默认出口；Key 独立出口保持不变。`)
      await refreshProviderViews(client)
      onApplied?.(ids)
    },
  })
  const resetSave = save.reset
  useEffect(() => { if (open) { setValue(""); setMessage(""); setVerifyMessage(""); resetSave() } }, [open, resetSave])
  const uncertain = isUncertainError(save.error)
  async function verify() {
    try {
      const latest = await providersApi.list()
      const matched = ids.filter((id) => latest.some((provider) => provider.id === id && (provider.ip_pool_node_id ?? 0) === nodeId))
      setVerifyMessage(`当前读取结果：${matched.length} / ${ids.length} 个渠道已使用目标默认出口。`)
      await refreshProviderViews(client)
      if (matched.length === ids.length) { setMessage("所选渠道当前配置已与目标一致，未重复提交。"); save.reset(); onApplied?.(ids) }
    } catch (error) { setVerifyMessage(error instanceof Error ? error.message : "核对失败，请稍后重试") }
  }
  return <Dialog open={open} onOpenChange={(next) => { if (!save.isPending) onOpenChange(next) }}><DialogContent className="max-w-xl">
    <DialogHeader><DialogTitle>批量指定出口</DialogTitle><DialogDescription>修改所选 {ids.length} 个渠道的默认出口。继承渠道的 Key 随之使用新出口；独立节点或强制直连的 Key 不受影响。</DialogDescription></DialogHeader>
    {nodes.isError ? <QueryError title="无法读取出口节点" error={nodes.error} onRetry={() => void nodes.refetch()} retrying={nodes.isFetching} /> : <div className="space-y-2"><Label htmlFor="batch-egress">目标出口</Label><EgressSelect id="batch-egress" value={value} onChange={setValue} nodes={nodes.data ?? []} disabled={nodes.isPending || save.isPending || !!message || !!uncertain} />{nodes.isPending && <p role="status" className="text-xs text-muted-foreground">读取出口节点中…</p>}{!nodes.isPending && !nodes.data?.some((node) => node.enabled) && <div className="text-xs text-muted-foreground">没有已启用的出口节点，仍可明确选择本机直连。<Button variant="link" size="sm" onClick={() => { onOpenChange(false); location.hash = "ippool" }}>前往 IP 池</Button></div>}</div>}
    <div className="rounded-lg border bg-muted/20 p-3 text-xs text-muted-foreground"><p className="font-medium text-foreground">确认影响范围</p><p className="mt-1">{selected.map((provider) => provider.name).join("、") || `${ids.length} 个所选渠道`}</p><p className="mt-2">只修改渠道默认出口，不覆盖 Key 独立设置。配置保存不会发起检活，也不保证节点连接或上游访问正常。</p>{ids.length > 200 && <p className="mt-2 text-destructive">单次最多 200 个渠道，请缩小选择范围。</p>}</div>
    {message && <p role="status" className="text-sm text-emerald-600">{message}</p>}
    {!message && save.error && <p role="alert" className="text-sm text-destructive">{uncertain ? "请求结果不确定，请先读取当前配置核对，不要重复应用。" : save.error instanceof Error ? save.error.message : "出口设置失败"}</p>}
    {verifyMessage && <p role="status" className="text-xs text-muted-foreground">{verifyMessage}</p>}
    <DialogFooter><Button variant="outline" disabled={save.isPending} onClick={() => onOpenChange(false)}>{message ? "完成" : "取消"}</Button>{uncertain && !message ? <Button onClick={() => void verify()}>读取配置核对</Button> : !message && <Button onClick={() => save.mutate()} disabled={!valid || !ids.length || ids.length > 200 || nodes.isPending || nodes.isError || save.isPending}>{save.isPending ? "应用中…" : "确认应用默认出口"}</Button>}</DialogFooter>
  </DialogContent></Dialog>
}
