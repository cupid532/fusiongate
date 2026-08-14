import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, Eye, KeyRound } from "lucide-react"
import { api } from "@/lib/api"
import type { APIKey } from "@/lib/types"
import { formatCost } from "@/lib/utils"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"

export function Keys() {
  const qc = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [revealed, setRevealed] = useState("")

  const { data: keys = [], isLoading } = useQuery({
    queryKey: ["keys"],
    queryFn: () => api<APIKey[]>("/api/admin/keys"),
  })

  const create = useMutation({
    mutationFn: async (n: string) =>
      api<{ id: number; key: string }>("/api/admin/keys", {
        method: "POST",
        body: JSON.stringify({ name: n }),
      }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["keys"] })
      setRevealed(res.key)
      setName("")
      setCreating(false)
    },
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/keys/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["keys"] }),
  })

  const reveal = useMutation({
    mutationFn: async (id: number) => api<{ key: string }>(`/api/admin/keys/${id}/reveal`, { method: "POST" }),
    onSuccess: (res) => setRevealed(res.key),
  })

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">访问密钥</h1>
          <p className="mt-1 text-sm text-muted-foreground">签发下游 API Key，按权限访问渠道和模型。</p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus className="h-4 w-4" />
          创建密钥
        </Button>
      </div>

      {creating && (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle className="text-base">新建密钥</CardTitle>
            <CardDescription>密钥创建后只完整显示一次。</CardDescription>
          </CardHeader>
          <CardContent className="flex items-end gap-3">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="key-name">名称</Label>
              <Input id="key-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：Hermes local" />
            </div>
            <Button onClick={() => create.mutate(name)} disabled={!name.trim() || create.isPending}>
              创建
            </Button>
            <Button variant="ghost" onClick={() => setCreating(false)}>
              取消
            </Button>
          </CardContent>
        </Card>
      )}

      {revealed && (
        <Card className="mb-4 border-primary/40">
          <CardContent className="flex items-center gap-3 p-4">
            <KeyRound className="h-5 w-5 shrink-0 text-primary" />
            <code className="flex-1 break-all font-mono text-sm">{revealed}</code>
            <Button variant="outline" size="sm" onClick={() => setRevealed("")}>
              关闭
            </Button>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent className="p-0">
          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : keys.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">还没有密钥</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">名称</th>
                    <th className="px-4 py-3 font-medium">前缀</th>
                    <th className="px-4 py-3 font-medium">权限</th>
                    <th className="px-4 py-3 font-medium">预算</th>
                    <th className="px-4 py-3 font-medium">RPM</th>
                    <th className="px-4 py-3 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {keys.map((k) => (
                    <tr key={k.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3 font-medium">{k.name}</td>
                      <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{k.prefix}••••</td>
                      <td className="px-4 py-3">
                        {k.revoked ? (
                          <Badge variant="danger">已吊销</Badge>
                        ) : (
                          <Badge variant={k.allow_all ? "success" : "neutral"}>{k.allow_all ? "全部模型" : "指定模型"}</Badge>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs">
                        {k.budget_micros > 0 ? (
                          <span>
                            {formatCost(k.spent_micros)} / {formatCost(k.budget_micros)}
                          </span>
                        ) : (
                          <span className="text-muted-foreground">无限制</span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs">{k.rpm_limit}</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          <Button variant="ghost" size="icon" onClick={() => reveal.mutate(k.id)} aria-label={`查看 ${k.name}`}>
                            <Eye className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              if (confirm(`删除密钥「${k.name}」？`)) remove.mutate(k.id)
                            }}
                            aria-label={`删除 ${k.name}`}
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
