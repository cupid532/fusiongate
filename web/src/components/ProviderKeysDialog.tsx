import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"

type ProviderKey = {
  id: number
  name: string
  key_hint: string
  enabled: boolean
  status: string
  model?: string
  discovered_models: number
  last_test_latency_ms: number
}

export function ProviderKeysDialog({
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
  const [newKey, setNewKey] = useState("")
  const [newName, setNewName] = useState("")

  const { data: keys = [], isLoading } = useQuery({
    queryKey: ["provider-keys", providerId],
    queryFn: () => api<ProviderKey[]>(`/api/admin/providers/${providerId}/keys`),
    enabled: open,
  })

  const add = useMutation({
    mutationFn: async () =>
      api(`/api/admin/providers/${providerId}/keys`, {
        method: "POST",
        body: JSON.stringify({ api_key: newKey, name: newName || undefined }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-keys", providerId] })
      qc.invalidateQueries({ queryKey: ["providers"] })
      setNewKey("")
      setNewName("")
    },
  })

  const remove = useMutation({
    mutationFn: async (keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provider-keys", providerId] }),
  })

  const toggle = useMutation({
    mutationFn: async ({ keyId, enabled }: { keyId: number; enabled: boolean }) =>
      api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "PATCH", body: JSON.stringify({ enabled }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provider-keys", providerId] }),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Key 管理 · {providerName}</DialogTitle>
          <DialogDescription>为一个渠道配置多个 API Key 用于轮换。</DialogDescription>
        </DialogHeader>

        <div className="flex items-end gap-2">
          <div className="flex flex-1 flex-col gap-1.5">
            <Label>API Key</Label>
            <Input value={newKey} onChange={(e) => setNewKey(e.target.value)} placeholder="sk-…" className="font-mono text-xs" />
          </div>
          <div className="flex w-32 flex-col gap-1.5">
            <Label>名称</Label>
            <Input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="可选" />
          </div>
          <Button onClick={() => add.mutate()} disabled={!newKey.trim() || add.isPending}>
            添加
          </Button>
        </div>

        {isLoading ? (
          <div className="py-6 text-center text-sm text-muted-foreground">加载中…</div>
        ) : keys.length === 0 ? (
          <div className="py-6 text-center text-sm text-muted-foreground">还没有 Key</div>
        ) : (
          <div className="space-y-1.5">
            {keys.map((k) => (
              <div key={k.id} className="flex items-center gap-3 rounded-lg border px-3 py-2">
                <div className="flex-1">
                  <div className="text-sm font-medium">{k.name || "API Key"}</div>
                  <div className="font-mono text-xs text-muted-foreground">{k.key_hint}</div>
                </div>
                <div className="flex items-center gap-2 text-xs text-muted-foreground">
                  <span>{k.discovered_models} 模型</span>
                  {k.status === "healthy" ? (
                    <Badge variant="success">正常</Badge>
                  ) : k.status === "failed" ? (
                    <Badge variant="danger">异常</Badge>
                  ) : (
                    <Badge variant="neutral">{k.status || "未测试"}</Badge>
                  )}
                </div>
                <Button variant="ghost" size="sm" onClick={() => toggle.mutate({ keyId: k.id, enabled: !k.enabled })}>
                  {k.enabled ? "停用" : "启用"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    if (confirm("删除这个 Key？")) remove.mutate(k.id)
                  }}
                >
                  删除
                </Button>
              </div>
            ))}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            关闭
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
