import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Network } from "lucide-react"
import { api } from "@/lib/api"
import type { IPPoolNode } from "@/lib/types"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

export function AuthEgressDialog({ open, onOpenChange, providerIds }: { open: boolean; onOpenChange: (value: boolean) => void; providerIds: number[] }) {
  const qc = useQueryClient()
  const [nodeId, setNodeId] = useState(0)
  const [message, setMessage] = useState("")
  const { data: nodes = [], isLoading } = useQuery({ queryKey: ["ippool"], queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"), enabled: open })
  useEffect(() => { if (open) { setNodeId(0); setMessage("") } }, [open])
  const save = useMutation({
    mutationFn: async () => api<{ affected: number }>("/api/admin/providers/batch", { method: "POST", body: JSON.stringify({ provider_ids: providerIds, action: "egress", ip_pool_node_id: nodeId }) }),
    onSuccess: (result) => { setMessage(`已为 ${result.affected} 个认证指定出口`); qc.invalidateQueries({ queryKey: ["providers"] }); setTimeout(() => onOpenChange(false), 500) },
  })
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader><DialogTitle>批量指定出口</DialogTitle><DialogDescription>所选 {providerIds.length} 个认证将使用同一个网络出口。</DialogDescription></DialogHeader>
        <div className="space-y-2">
          <EgressOption selected={nodeId === 0} onClick={() => setNodeId(0)} title="直连" detail="不使用 IP 池代理" />
          {isLoading ? <div className="py-5 text-center text-sm text-muted-foreground">加载出口节点中…</div> : nodes.filter((node) => node.enabled).map((node) => <EgressOption key={node.id} selected={nodeId === node.id} onClick={() => setNodeId(node.id)} title={node.name} detail={node.protocol + " · " + (node.exit_ip || node.server)} status={node.status} />)}
        </div>
        {message && <div className="rounded-md bg-primary/10 px-3 py-2 text-xs text-primary">{message}</div>}
        {save.error && <div className="rounded-md bg-destructive/10 px-3 py-2 text-xs text-destructive">{save.error instanceof Error ? save.error.message : "出口设置失败"}</div>}
        <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>取消</Button><Button onClick={() => save.mutate()} disabled={!providerIds.length || save.isPending}>{save.isPending ? "保存中…" : "应用出口"}</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function EgressOption({ selected, onClick, title, detail, status }: { selected: boolean; onClick: () => void; title: string; detail: string; status?: string }) {
  const usable = !status || status === "healthy" || status === "ready"
  return <button type="button" onClick={onClick} className={"flex w-full items-center gap-3 rounded-md border px-3 py-3 text-left transition-colors " + (selected ? "border-primary bg-primary/10" : "hover:bg-muted/40")}>
    <Network className="h-4 w-4 text-muted-foreground" />
    <span className="min-w-0 flex-1"><span className="block truncate text-sm font-medium">{title}</span><span className="block truncate text-xs text-muted-foreground">{detail}</span></span>
    {status && <Badge variant={usable ? "success" : "neutral"}>{usable ? "可用" : status}</Badge>}
    {selected && <Badge variant="default">已选择</Badge>}
  </button>
}