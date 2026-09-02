import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Activity, Plus, Settings2, Trash2 } from "lucide-react"
import { providerKeysApi } from "@/lib/api"
import type { ProviderKey } from "@/lib/types"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"

function statusBadge(status?: string) {
  if (status === "healthy") return <Badge variant="success">正常</Badge>
  if (status === "failed" || status === "unhealthy") return <Badge variant="danger">异常</Badge>
  if (status === "pending" || status === "running") return <Badge variant="warning">检测中</Badge>
  return <Badge variant="neutral">{status || "未测试"}</Badge>
}

export function ProviderKeysDialog({ open, onOpenChange, providerId, providerName, onManageModels }: {
  open: boolean
  onOpenChange: (value: boolean) => void
  providerId: number
  providerName: string
  onManageModels?: () => void
}) {
  const qc = useQueryClient()
  const [newKey, setNewKey] = useState("")
  const [newName, setNewName] = useState("")
  const [error, setError] = useState("")
  const { data: keys = [], isLoading } = useQuery({
    queryKey: ["provider-keys", providerId],
    queryFn: () => providerKeysApi.list(providerId),
    enabled: open,
  })
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["provider-keys", providerId] })
    void qc.invalidateQueries({ queryKey: ["providers"] })
  }
  const add = useMutation({
    mutationFn: () => providerKeysApi.create(providerId, { api_key: newKey.trim(), name: newName.trim() || undefined, health_check_enabled: true }),
    onSuccess: () => { setNewKey(""); setNewName(""); setError(""); refresh() },
    onError: (reason) => setError(reason instanceof Error ? reason.message : "添加 Key 失败"),
  })
  const patch = useMutation({
    mutationFn: ({ keyId, body }: { keyId: number; body: Record<string, unknown> }) => providerKeysApi.patch(providerId, keyId, body),
    onSuccess: () => { setError(""); refresh() },
    onError: (reason) => setError(reason instanceof Error ? reason.message : "更新 Key 失败"),
  })
  const remove = useMutation({
    mutationFn: (keyId: number) => providerKeysApi.remove(providerId, keyId),
    onSuccess: refresh,
    onError: (reason) => setError(reason instanceof Error ? reason.message : "删除 Key 失败"),
  })
  const test = useMutation({
    mutationFn: (keyId: number) => providerKeysApi.test(providerId, keyId),
    onSuccess: refresh,
    onError: (reason) => { setError(reason instanceof Error ? reason.message : "Key 测试失败"); refresh() },
  })

  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-h-[90vh] max-w-3xl overflow-y-auto">
      <DialogHeader>
        <DialogTitle>Key 管理 · {providerName}</DialogTitle>
        <DialogDescription>管理 Key 的启用状态、检活开关、生命周期和连接测试；模型配置请进入模型管理。</DialogDescription>
      </DialogHeader>
      {error && <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</div>}
      <div className="grid gap-2 rounded-md border p-3 sm:grid-cols-[minmax(0,1fr)_9rem_auto] sm:items-end">
        <div className="flex min-w-0 flex-col gap-1.5"><Label>新 API Key</Label><Input value={newKey} onChange={(event) => setNewKey(event.target.value)} placeholder="sk-…" className="font-mono text-xs" /></div>
        <div className="flex min-w-0 flex-col gap-1.5"><Label>名称</Label><Input value={newName} onChange={(event) => setNewName(event.target.value)} placeholder="例如：主 Key" /></div>
        <Button onClick={() => add.mutate()} disabled={!newKey.trim() || add.isPending}><Plus />{add.isPending ? "添加中…" : "添加 Key"}</Button>
      </div>
      <div className="flex justify-end"><Button variant="outline" onClick={onManageModels} disabled={!onManageModels}><Settings2 />打开模型管理</Button></div>
      {isLoading ? <div className="py-6 text-center text-sm text-muted-foreground">加载中…</div> : keys.length === 0 ? <div className="py-6 text-center text-sm text-muted-foreground">还没有 Key</div> : <div className="space-y-2">{keys.map((key: ProviderKey) => <section key={key.id} className="rounded-md border p-3"><div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"><div className="min-w-0"><div className="flex flex-wrap items-center gap-2"><span className="text-sm font-medium">{key.name || "API Key"}</span><span className="font-mono text-xs text-muted-foreground">{key.key_hint}</span>{statusBadge(key.status)}</div><div className="mt-1 text-xs text-muted-foreground">模型 {key.models?.length ?? 0} 个 · 上次测试 {key.last_tested_at ? `${key.last_test_latency_ms} ms` : "尚未测试"}</div>{key.last_error && <div className="mt-1 break-words text-xs text-destructive">{key.last_error}</div>}</div><div className="flex flex-wrap items-center gap-3"><label className="flex items-center gap-1.5 text-xs text-muted-foreground">启用<Switch checked={key.enabled} disabled={patch.isPending} onCheckedChange={(enabled) => patch.mutate({ keyId: key.id, body: { enabled } })} aria-label={`${key.name || key.key_hint} 启用`} /></label><label className="flex items-center gap-1.5 text-xs text-muted-foreground">检活<Switch checked={key.health_check_enabled} disabled={patch.isPending} onCheckedChange={(health_check_enabled) => patch.mutate({ keyId: key.id, body: { health_check_enabled } })} aria-label={`${key.name || key.key_hint} 检活`} /></label><Button variant="outline" size="sm" onClick={() => test.mutate(key.id)} disabled={test.isPending}><Activity />{test.isPending ? "测试中…" : "测试"}</Button><Button variant="ghost" size="icon" title="删除 Key" aria-label={`删除 ${key.name || key.key_hint}`} disabled={remove.isPending} onClick={() => { if (confirm(`删除 ${key.name || key.key_hint}？`)) remove.mutate(key.id) }}><Trash2 className="text-destructive" /></Button></div></div></section>)}</div>}
      <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button></DialogFooter>
    </DialogContent>
  </Dialog>
}
