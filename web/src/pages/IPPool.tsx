import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, Plug, Settings2, Server, Wifi, WifiOff, Link2, Search, ListChecks } from "lucide-react"
import { api } from "@/lib/api"
import type { IPPoolNode } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "@/components/ui/dialog"
import { QueryError } from "@/components/ui/query-error"
import { StatCard } from "@/components/ui/stat-card"
import { useConfirm, useConfirmDelete } from "@/components/ui/confirm"
import { notify, notifySuccess } from "@/lib/notify"

function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "—"
  const diff = Date.now() - new Date(iso).getTime()
  if (diff < 60_000) return "刚刚"
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`
  return `${Math.floor(diff / 86_400_000)}d ago`
}

export function IPPool() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const confirmDelete = useConfirmDelete()
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [link, setLink] = useState("")
  const [editing, setEditing] = useState<IPPoolNode | null>(null)
  const [editName, setEditName] = useState("")
  const [editLink, setEditLink] = useState("")
  const [editEnabled, setEditEnabled] = useState(true)
  const [q, setQ] = useState("")
  const [multiSelect, setMultiSelect] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())

  const { data: nodes = [], isLoading, isError, error, refetch } = useQuery({
    queryKey: ["ippool"],
    queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"),
  })

  const filtered = useMemo(() => {
    if (!q.trim()) return nodes
    const term = q.trim().toLowerCase()
    return nodes.filter((n) => n.name.toLowerCase().includes(term) || (n.server && n.server.toLowerCase().includes(term)) || (n.exit_ip && n.exit_ip.toLowerCase().includes(term)))
  }, [nodes, q])

  const nodeCounts = useMemo(() => ({
    total: nodes.length,
    online: nodes.filter((n) => n.enabled).length,
    offline: nodes.filter((n) => !n.enabled).length,
    providers: nodes.reduce((sum, n) => sum + n.provider_count, 0),
  }), [nodes])

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

  const update = useMutation({
    mutationFn: async ({ id, patch }: { id: number; patch: Record<string, unknown> }) =>
      api(`/api/admin/ip-pool/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ippool"] })
      setEditing(null)
      notifySuccess("节点已更新")
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

  const batchToggle = useMutation({
    mutationFn: async ({ ids, enabled }: { ids: number[]; enabled: boolean }) => {
      for (const id of ids) await api(`/api/admin/ip-pool/${id}`, { method: "PATCH", body: JSON.stringify({ enabled }) })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ippool"] })
      setSelected(new Set())
      notifySuccess(selected.size + " 个节点已更新")
    },
  })

  const batchDelete = useMutation({
    mutationFn: async (ids: number[]) => {
      for (const id of ids) await api(`/api/admin/ip-pool/${id}`, { method: "DELETE" })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["ippool"] })
      setSelected(new Set())
    },
  })

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
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
              <Button onClick={() => create.mutate()} disabled={!name.trim() || !link.trim() || create.isPending}>添加</Button>
              <Button variant="ghost" onClick={() => setCreating(false)}>取消</Button>
            </div>
          </CardContent>
        </Card>
      )}

      <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="总节点" value={nodeCounts.total} icon={<Server className="h-4 w-4" />} tone="text-foreground" sub="出站代理节点" />
        <StatCard label="在线" value={nodeCounts.online} icon={<Wifi className="h-4 w-4" />} tone="text-emerald-600" sub="已启用" />
        <StatCard label="离线" value={nodeCounts.offline} icon={<WifiOff className="h-4 w-4" />} tone="text-muted-foreground" sub="已停用" />
        <StatCard label="关联渠道" value={nodeCounts.providers} icon={<Link2 className="h-4 w-4" />} tone="text-primary" sub="使用代理的渠道数" />
      </div>

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2">
            {multiSelect && selected.size > 0 && (
              <div className="flex items-center gap-1.5">
                <span className="text-xs text-muted-foreground">已选 {selected.size}</span>
                <Button size="sm" variant="outline" onClick={() => batchToggle.mutate({ ids: [...selected], enabled: true })}>
                  启用
                </Button>
                <Button size="sm" variant="outline" onClick={() => batchToggle.mutate({ ids: [...selected], enabled: false })}>
                  停用
                </Button>
                <Button
                  size="sm"
                  variant="destructive"
                  onClick={async () => {
                    const names = [...selected].map((id) => nodes.find((n) => n.id === id)?.name).filter(Boolean).slice(0, 5).join("、")
                    if (await confirm({ title: `删除选中的 ${selected.size} 个节点？`, description: `${names}${selected.size > 5 ? " 等" : ""}。此操作不可恢复。`, destructive: true, confirmLabel: `删除 ${selected.size} 个` })) {
                      batchDelete.mutate([...selected])
                    }
                  }}
                >
                  删除
                </Button>
              </div>
            )}
            <Button size="sm" variant={multiSelect ? "default" : "outline"} onClick={() => { setMultiSelect((v) => !v); setSelected(new Set()) }}>
              <ListChecks className="h-4 w-4" />
              {multiSelect ? "退出多选" : "多选"}
            </Button>
            <div className="relative ml-auto">
              <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索节点" className="h-8 w-52 pl-8 text-xs" />
            </div>
          </div>
          {isLoading ? (
            <div className="space-y-2 p-4">{Array.from({ length: 3 }).map((_, i) => <div key={i} className="h-12 animate-pulse rounded-lg bg-muted/40" />)}</div>
          ) : isError ? (
            <QueryError title="无法加载节点列表" error={error} onRetry={() => void refetch()} />
          ) : filtered.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">{nodes.length === 0 ? "还没有节点" : "没有匹配的节点"}</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    {multiSelect && (
                      <th className="w-10 px-3 py-3">
                        <input type="checkbox" aria-label="选择全部节点" checked={filtered.length > 0 && filtered.every((n) => selected.has(n.id))} onChange={(e) => { if (e.target.checked) setSelected(new Set(filtered.map((n) => n.id))); else setSelected(new Set()) }} />
                      </th>
                    )}
                    <th className="px-4 py-3 font-medium">名称</th>
                    <th className="px-4 py-3 font-medium">协议</th>
                    <th className="px-4 py-3 font-medium">服务器</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">延迟</th>
                    <th className="px-4 py-3 font-medium">最后检测</th>
                    <th className="px-4 py-3 font-medium">出口 IP</th>
                    <th className="px-4 py-3 font-medium">渠道数</th>
                    <th className="px-4 py-3 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map((n) => (
                    <tr key={n.id} className="border-b border-border/50 last:border-0 even:bg-muted/30 hover:bg-muted/50 transition-colors duration-150">
                      {multiSelect && (
                        <td className="px-3 py-3">
                          <input type="checkbox" aria-label={`选择 ${n.name}`} checked={selected.has(n.id)} onChange={(e) => { const next = new Set(selected); if (e.target.checked) next.add(n.id); else next.delete(n.id); setSelected(next) }} />
                        </td>
                      )}
                      <td className="px-4 py-3 font-medium">{n.name}</td>
                      <td className="px-4 py-3"><Badge variant="neutral">{n.protocol}</Badge></td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{n.server}</td>
                      <td className="px-4 py-3">
                        {n.status === "healthy" ? (
                          <Badge variant="success">正常</Badge>
                        ) : n.status === "config_error" ? (
                          <Badge variant="danger">配置错误</Badge>
                        ) : n.last_error ? (
                          <Badge variant="danger" title={n.last_error}>错误</Badge>
                        ) : (
                          <Badge variant="neutral">{n.status || "待检测"}</Badge>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs tabular-nums text-muted-foreground">
                        {n.last_latency_ms > 0 ? `${n.last_latency_ms}ms` : "—"}
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{timeAgo(n.last_checked_at)}</td>
                      <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{n.exit_ip || "—"}</td>
                      <td className="px-4 py-3 text-xs">{n.provider_count}</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          <Switch checked={n.enabled} onCheckedChange={(v) => toggle.mutate({ id: n.id, enabled: v })} aria-label={`${n.name} 开关`} />
                          <Button variant="ghost" size="icon" onClick={() => { setEditing(n); setEditName(n.name); setEditLink(""); setEditEnabled(n.enabled) }} aria-label={`编辑 ${n.name}`}>
                            <Settings2 className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() =>
                              test.mutate(n.id, {
                                onSuccess: (r) => notify({ tone: r.status === "ok" ? "success" : "error", title: `${n.name}：${r.status === "ok" ? "连通" : r.status}`, description: `出口 IP ${r.exit_ip} · 延迟 ${r.latency_ms} ms`, duration: 12_000 }),
                              })
                            }
                          >
                            <Plug className="h-4 w-4" />
                            测试
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={async () => {
                              if (await confirmDelete(`节点「${n.name}」`)) remove.mutate(n.id)
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

      <Dialog open={!!editing} onOpenChange={(o) => { if (!o) setEditing(null) }}>
        <DialogContent>
          <DialogHeader><DialogTitle>编辑节点</DialogTitle></DialogHeader>
          <div className="space-y-4">
            <div className="flex flex-col gap-1.5">
              <Label>名称</Label>
              <Input value={editName} onChange={(e) => setEditName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>分享链接（留空不修改）</Label>
              <Input value={editLink} onChange={(e) => setEditLink(e.target.value)} placeholder="vless://… 或 ss://…" className="font-mono text-xs" />
            </div>
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={editEnabled} onCheckedChange={setEditEnabled} />
              启用节点
            </label>
          </div>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setEditing(null)}>取消</Button>
            <Button
              onClick={() => {
                if (!editing) return
                const patch: Record<string, unknown> = { name: editName, enabled: editEnabled }
                if (editLink.trim()) patch.share_link = editLink
                update.mutate({ id: editing.id, patch })
              }}
              disabled={!editName.trim() || update.isPending}
            >
              {update.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.div>
  )
}
