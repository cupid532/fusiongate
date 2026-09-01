import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Activity, RefreshCw, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { IPPoolNode, ProviderKey, ProviderKeyModel } from "@/lib/types"
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
import { Switch } from "@/components/ui/switch"

function formatTime(value?: string) {
  if (!value) return "尚未"
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
}

function statusBadge(status?: string) {
  if (status === "healthy") return <Badge variant="success">正常</Badge>
  if (status === "failed" || status === "unhealthy") return <Badge variant="danger">异常</Badge>
  if (status === "disabled") return <Badge variant="neutral">已关闭</Badge>
  if (status === "pending" || status === "running") return <Badge variant="warning">检测中</Badge>
  return <Badge variant="neutral">{status || "未测试"}</Badge>
}

function modelStatus(model: ProviderKeyModel) {
  return statusBadge(model.health_status)
}

function effectiveEgress(key: ProviderKey, nodes: IPPoolNode[]) {
  if (key.effective_egress === "node") {
    const nodeName = nodes.find((node) => node.id === key.effective_node_id)?.name || key.ip_pool_node_name
    const node = nodeName || (key.effective_node_id ? `节点 #${key.effective_node_id}` : "节点")
    return `${node}${key.egress_inherited ? "（继承）" : ""}`
  }
  return `直连${key.egress_inherited ? "（继承）" : ""}`
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
  const { data: nodes = [] } = useQuery({
    queryKey: ["ip-pool"],
    queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"),
    enabled: open,
  })

  const refreshKeys = () => qc.invalidateQueries({ queryKey: ["provider-keys", providerId] })

  const add = useMutation({
    mutationFn: async () =>
      api(`/api/admin/providers/${providerId}/keys`, {
        method: "POST",
        body: JSON.stringify({ api_key: newKey, name: newName || undefined, health_check_enabled: true }),
      }),
    onSuccess: () => {
      refreshKeys()
      qc.invalidateQueries({ queryKey: ["providers"] })
      setNewKey("")
      setNewName("")
    },
  })

  const remove = useMutation({
    mutationFn: async (keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "DELETE" }),
    onSuccess: refreshKeys,
  })

  const patch = useMutation({
    mutationFn: async ({ keyId, body }: { keyId: number; body: Partial<Pick<ProviderKey, "enabled" | "health_check_enabled">> }) =>
      api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "PATCH", body: JSON.stringify(body) }),
    onSuccess: refreshKeys,
  })

  const action = useMutation({
    mutationFn: async ({ keyId, name }: { keyId: number; name: "test" | "discover-models" }) =>
      api(`/api/admin/providers/${providerId}/keys/${keyId}/${name}`, { method: "POST" }),
    onSuccess: refreshKeys,
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[88vh] max-w-3xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Key 管理 · {providerName}</DialogTitle>
          <DialogDescription>为一个渠道配置多个 API Key 用于轮换，并分别控制检活。</DialogDescription>
        </DialogHeader>

        <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_8rem_auto] sm:items-end">
          <div className="flex min-w-0 flex-col gap-1.5">
            <Label>API Key</Label>
            <Input value={newKey} onChange={(e) => setNewKey(e.target.value)} placeholder="sk-…" className="font-mono text-xs" />
          </div>
          <div className="flex min-w-0 flex-col gap-1.5">
            <Label>名称</Label>
            <Input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="可选" />
          </div>
          <Button onClick={() => add.mutate()} disabled={!newKey.trim() || add.isPending} className="w-full sm:w-auto">
            {add.isPending ? "添加中…" : "添加"}
          </Button>
        </div>

        {isLoading ? (
          <div className="py-6 text-center text-sm text-muted-foreground">加载中…</div>
        ) : keys.length === 0 ? (
          <div className="py-6 text-center text-sm text-muted-foreground">还没有 Key</div>
        ) : (
          <div className="space-y-2">
            {keys.map((key) => (
              <section key={key.id} className="rounded-lg border">
                <div className="flex flex-col gap-3 p-3 sm:flex-row sm:items-start">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-sm font-medium">{key.name || "API Key"}</span>
                      <span className="font-mono text-xs text-muted-foreground">{key.key_hint}</span>
                      {!key.enabled && <Badge variant="neutral">已停用</Badge>}
                    </div>
                    <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground">
                      <span>出口：{effectiveEgress(key, nodes)}</span>
                      <span>发现：{key.discovered_models} 个模型 · {formatTime(key.last_discovered_at)}</span>
                      <span>测试：{formatTime(key.last_tested_at)}{key.last_tested_at ? ` · ${key.last_test_latency_ms} ms` : ""}</span>
                    </div>
                    {key.last_error && <div className="mt-1 break-words text-xs text-destructive">{key.last_error}</div>}
                  </div>

                  <div className="flex flex-wrap items-center gap-2 sm:justify-end">
                    {statusBadge(key.status)}
                    <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                      启用
                      <Switch
                        checked={key.enabled}
                        disabled={patch.isPending}
                        onCheckedChange={(enabled) => patch.mutate({ keyId: key.id, body: { enabled } })}
                        aria-label={`${key.name || key.key_hint} 启用`}
                      />
                    </label>
                    <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                      检活
                      <Switch
                        checked={key.health_check_enabled}
                        disabled={patch.isPending}
                        onCheckedChange={(health_check_enabled) => patch.mutate({ keyId: key.id, body: { health_check_enabled } })}
                        aria-label={`${key.name || key.key_hint} 检活`}
                      />
                    </label>
                    <Button
                      variant="ghost"
                      size="icon"
                      title="发现模型"
                      aria-label={`发现 ${key.name || key.key_hint} 的模型`}
                      disabled={action.isPending}
                      onClick={() => action.mutate({ keyId: key.id, name: "discover-models" })}
                    >
                      <RefreshCw />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      title="测试 Key"
                      aria-label={`测试 ${key.name || key.key_hint}`}
                      disabled={action.isPending}
                      onClick={() => action.mutate({ keyId: key.id, name: "test" })}
                    >
                      <Activity />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      title="删除 Key"
                      aria-label={`删除 ${key.name || key.key_hint}`}
                      disabled={remove.isPending}
                      onClick={() => {
                        if (confirm("删除这个 Key？")) remove.mutate(key.id)
                      }}
                    >
                      <Trash2 className="text-destructive" />
                    </Button>
                  </div>
                </div>

                <div className="border-t bg-muted/15 px-3 py-2">
                  {(key.models ?? []).length === 0 ? (
                    <div className="text-xs text-muted-foreground">尚无已发现或已检活模型</div>
                  ) : (
                    <div className="space-y-1">
                      {(key.models ?? []).map((model) => (
                        <div
                          key={model.model}
                          className="grid gap-1 rounded-md px-2 py-1.5 text-xs hover:bg-muted/40 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-center sm:gap-3"
                        >
                          <div className="min-w-0">
                            <div className="truncate font-mono text-foreground" title={model.model}>{model.display_name || model.model}</div>
                            {model.display_name && model.display_name !== model.model && (
                              <div className="truncate font-mono text-[11px] text-muted-foreground" title={model.model}>{model.model}</div>
                            )}
                            {model.health_error && <div className="break-words text-[11px] text-destructive">{model.health_error}</div>}
                          </div>
                          <div className="text-muted-foreground">
                            {model.last_checked_at ? `${model.latency_ms} ms · ${formatTime(model.last_checked_at)}` : "尚未检活"}
                          </div>
                          <div>{modelStatus(model)}</div>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </section>
            ))}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
