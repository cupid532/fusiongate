import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, Plug } from "lucide-react"
import { api } from "@/lib/api"
import type { IPPoolNode } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"

export function IPPool() {
  const qc = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [link, setLink] = useState("")

  const { data: nodes = [], isLoading } = useQuery({
    queryKey: ["ippool"],
    queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"),
  })

  const create = useMutation({
    mutationFn: async () =>
      api("/api/admin/ip-pool", { method: "POST", body: JSON.stringify({ name, share_link: link }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ippool"] })
      setName("")
      setLink("")
      setCreating(false)
    },
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/ip-pool/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ippool"] }),
  })

  const toggle = useMutation({
    mutationFn: async ({ id, enabled }: { id: number; enabled: boolean }) =>
      api(`/api/admin/ip-pool/${id}`, { method: "PATCH", body: JSON.stringify({ enabled }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ippool"] }),
  })

  const test = useMutation({
    mutationFn: async (id: number) => api<{ status: string; exit_ip: string; latency_ms: number }>(`/api/admin/ip-pool/${id}/test`, { method: "POST" }),
  })

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">IP 池</h1>
          <p className="mt-1 text-sm text-muted-foreground">管理渠道的出站代理节点。</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4" />
          添加节点
        </Button>
      </div>

      {creating && (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle className="text-base">新建节点</CardTitle>
            <CardDescription>粘贴 sing-box 分享链接。</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-col gap-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：US-ORD" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>分享链接</Label>
              <Input value={link} onChange={(e) => setLink(e.target.value)} placeholder="vless://… 或 ss://…" className="font-mono text-xs" />
            </div>
            <div className="flex gap-2">
              <Button onClick={() => create.mutate()} disabled={!name.trim() || !link.trim() || create.isPending}>
                添加
              </Button>
              <Button variant="ghost" onClick={() => setCreating(false)}>
                取消
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent className="p-0">
          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : nodes.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">还没有节点</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">名称</th>
                    <th className="px-4 py-3 font-medium">协议</th>
                    <th className="px-4 py-3 font-medium">服务器</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">出口 IP</th>
                    <th className="px-4 py-3 font-medium">渠道数</th>
                    <th className="px-4 py-3 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {nodes.map((n) => (
                    <tr key={n.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3 font-medium">{n.name}</td>
                      <td className="px-4 py-3">
                        <Badge variant="neutral">{n.protocol}</Badge>
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{n.server}</td>
                      <td className="px-4 py-3">
                        {n.status === "healthy" ? <Badge variant="success">正常</Badge> : n.status === "config_error" ? <Badge variant="danger">配置错误</Badge> : <Badge variant="neutral">{n.status || "待检测"}</Badge>}
                      </td>
                      <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{n.exit_ip || "—"}</td>
                      <td className="px-4 py-3 text-xs">{n.provider_count}</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          <Switch checked={n.enabled} onCheckedChange={(v) => toggle.mutate({ id: n.id, enabled: v })} aria-label={`${n.name} 开关`} />
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() =>
                              test.mutate(n.id, {
                                onSuccess: (r) => alert(`状态：${r.status}\n出口 IP：${r.exit_ip}\n延迟：${r.latency_ms} ms`),
                              })
                            }
                          >
                            <Plug className="h-4 w-4" />
                            测试
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              if (confirm(`删除节点「${n.name}」？`)) remove.mutate(n.id)
                            }}
                            aria-label={`删除 ${n.name}`}
                          >
                            <Trash2 className="h-4 w-4 text-destructive" />
                          </Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </motion.div>
  )
}
