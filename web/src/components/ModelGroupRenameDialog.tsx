import { useEffect, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

export function ModelGroupRenameDialog({ model, onClose }: { model: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState(model)
  const [keepAlias, setKeepAlias] = useState(true)
  useEffect(() => { setName(model); setKeepAlias(true) }, [model])
  const rename = useMutation({ mutationFn: () => api("/api/admin/model-groups/rename", { method: "POST", body: JSON.stringify({ old_name: model, new_name: name.trim(), keep_old_alias: keepAlias }) }), onSuccess: () => { for (const key of ["routes", "model-aliases", "keys", "pricing", "providers"]) qc.invalidateQueries({ queryKey: [key] }); onClose() } })
  return <Dialog open={!!model} onOpenChange={(open) => { if (!open && !rename.isPending) onClose() }}><DialogContent>
    <DialogHeader><DialogTitle>重命名请求模型</DialogTitle><DialogDescription>整个模型组及其调用别名一起更新。上游模型名、Key 模型权限和历史账本保持不变。</DialogDescription></DialogHeader>
    <div className="space-y-4"><p className="break-all text-sm">当前请求模型：<span className="font-mono">{model}</span></p><div className="space-y-1.5"><Label htmlFor="new-group-name">新的请求模型名</Label><Input id="new-group-name" value={name} onChange={(event) => setName(event.target.value)} disabled={rename.isPending} className="font-mono" /></div><label className="flex items-center justify-between gap-3 text-sm"><span>保留旧名称作为调用别名（推荐）</span><Switch checked={keepAlias} onCheckedChange={setKeepAlias} disabled={rename.isPending} aria-label="保留旧名称作为调用别名" /></label><p className="text-xs text-muted-foreground">下游访问密钥中针对旧组的允许/拒绝规则将定向兼容新名称，不扩大其他模型权限。若通配符或别名权限无法安全迁移，将拒绝整个操作，请调整规则后重试。</p>{!keepAlias && <p className="text-xs text-amber-600">旧名称将无法再调用，旧客户端需改用新名称。</p>}{rename.error && <p role="alert" className="text-sm text-destructive">{rename.error.message}</p>}</div>
    <DialogFooter><Button variant="outline" disabled={rename.isPending} onClick={onClose}>取消</Button><Button disabled={!name.trim() || name.trim().toLowerCase() === model.toLowerCase() || rename.isPending} onClick={() => rename.mutate()}>{rename.isPending ? "更新中…" : "确认重命名整个模型组"}</Button></DialogFooter>
  </DialogContent></Dialog>
}
