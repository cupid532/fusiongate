import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { FileKey, Plus, Trash2 } from "lucide-react"
import { api } from "@/lib/api"
import type { CredentialImportPreviewItem, Provider } from "@/lib/types"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"

const platformLabels: Record<string, string> = {
  codex: "Codex (ChatGPT)",
  claude: "Claude",
  grok: "Grok (xAI)",
}

function statusBadge(p: Provider) {
  if (p.auth_status === "ready") return <Badge variant="success">就绪</Badge>
  if (p.auth_status === "expired") return <Badge variant="danger">已过期</Badge>
  if (p.auth_status === "pending") return <Badge variant="warning">待验证</Badge>
  return <Badge variant="neutral">{p.auth_status || "未知"}</Badge>
}

export function AuthFiles() {
  const qc = useQueryClient()
  const [importOpen, setImportOpen] = useState(false)
  const [oauthOpen, setOauthOpen] = useState(false)

  const { data: providers = [], isLoading } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })

  const oauth = providers.filter((p) => p.auth_kind === "oauth")

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/providers/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
    },
  })

  // 按平台分组
  const groups = useMemo(() => {
    const order = ["codex", "grok", "claude"]
    const map = new Map<string, Provider[]>()
    for (const p of oauth) {
      const key = p.auth_source || p.type
      const list = map.get(key) ?? []
      list.push(p)
      map.set(key, list)
    }
    return order
      .map((k) => ({ platform: k, items: map.get(k) ?? [] }))
      .filter((g) => g.items.length > 0)
      .concat(
        [...map.entries()]
          .filter(([k]) => !order.includes(k))
          .map(([k, items]) => ({ platform: k, items }))
      )
  }, [oauth])

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">认证文件</h1>
          <p className="mt-1 text-sm text-muted-foreground">管理 Codex / Claude / Grok 的 OAuth 凭据。</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => setImportOpen(true)}>
            <FileKey className="h-4 w-4" />
            导入文件
          </Button>
          <Button onClick={() => setOauthOpen(true)}>
            <Plus className="h-4 w-4" />
            添加授权
          </Button>
        </div>
      </div>

      {isLoading ? (
        <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
      ) : oauth.length === 0 ? (
        <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
          还没有认证文件，点击「导入文件」或「添加授权」。
        </div>
      ) : (
        <div className="space-y-4">
          {groups.map((g) => (
            <Card key={g.platform}>
              <CardHeader className="pb-3">
                <CardTitle className="flex items-center gap-2 text-base">
                  {platformLabels[g.platform] ?? g.platform}
                  <Badge variant="neutral">{g.items.length} 个</Badge>
                </CardTitle>
              </CardHeader>
              <CardContent className="p-0">
                <div className="overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b text-left text-xs text-muted-foreground">
                        <th className="px-4 py-2.5 font-medium">名称</th>
                        <th className="px-4 py-2.5 font-medium">账号</th>
                        <th className="px-4 py-2.5 font-medium">状态</th>
                        <th className="px-4 py-2.5 font-medium">模型</th>
                        <th className="px-4 py-2.5 text-right font-medium">操作</th>
                      </tr>
                    </thead>
                    <tbody>
                      {g.items.map((p) => (
                        <tr key={p.id} className="border-b last:border-0 hover:bg-muted/40">
                          <td className="px-4 py-3 font-medium">{p.name}</td>
                          <td className="px-4 py-3 text-xs text-muted-foreground">{p.auth_email || "—"}</td>
                          <td className="px-4 py-3">{statusBadge(p)}</td>
                          <td className="px-4 py-3 text-xs text-muted-foreground">{p.model_count} 个</td>
                          <td className="px-4 py-3">
                            <div className="flex justify-end">
                              <Button
                                variant="ghost"
                                size="icon"
                                onClick={() => {
                                  if (confirm(`删除认证文件「${p.name}」？`)) remove.mutate(p.id)
                                }}
                                aria-label={`删除 ${p.name}`}
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
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <AuthImportDialog open={importOpen} onOpenChange={setImportOpen} />
      <AuthOAuthDialog open={oauthOpen} onOpenChange={setOauthOpen} />
    </motion.div>
  )
}

function AuthImportDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const qc = useQueryClient()
  const [content, setContent] = useState("")
  const [items, setItems] = useState<CredentialImportPreviewItem[]>([])
  const [sessionId, setSessionId] = useState("")
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [step, setStep] = useState<"paste" | "preview">("paste")

  const preview = useMutation({
    mutationFn: async () =>
      api<{ session_id: string; items: CredentialImportPreviewItem[] }>("/api/admin/auth/import/preview", {
        method: "POST",
        body: JSON.stringify({ content }),
      }),
    onSuccess: (r) => {
      setSessionId(r.session_id)
      setItems(r.items)
      setSelected(new Set(r.items.map((i) => i.id)))
      setStep("preview")
    },
  })

  const commit = useMutation({
    mutationFn: async () =>
      api("/api/admin/auth/import/commit", {
        method: "POST",
        body: JSON.stringify({ session_id: sessionId, selected: [...selected], update_existing: true }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      onOpenChange(false)
      setStep("paste")
      setContent("")
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>导入认证文件</DialogTitle>
          <DialogDescription>粘贴 OAuth 凭据 JSON（Codex / Claude / Grok），系统会先预览。</DialogDescription>
        </DialogHeader>

        {step === "paste" ? (
          <Textarea
            value={content}
            onChange={(e) => setContent(e.target.value)}
            placeholder='粘贴凭证 JSON（例如 {"version":1,"kind":"oauth","platform":"codex",...}）'
            className="min-h-[200px] font-mono text-xs"
          />
        ) : (
          <div className="space-y-2">
            <div className="text-sm text-muted-foreground">
              已选择 <span className="font-semibold text-primary">{selected.size}</span> / {items.length} 个凭据
            </div>
            <div className="max-h-[45vh] space-y-1 overflow-y-auto rounded-lg border p-2">
              {items.map((i) => (
                <label key={i.id} className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2 hover:bg-muted/40">
                  <input
                    type="checkbox"
                    checked={selected.has(i.id)}
                    onChange={(e) => {
                      const next = new Set(selected)
                      if (e.target.checked) next.add(i.id)
                      else next.delete(i.id)
                      setSelected(next)
                    }}
                  />
                  <span className="flex-1">
                    <span className="block text-sm font-medium">{i.name}</span>
                    <span className="block text-xs text-muted-foreground">{i.email || i.account_id || i.platform}</span>
                  </span>
                  <Badge variant="neutral">{platformLabels[i.platform] ?? i.platform}</Badge>
                  {i.duplicate && <Badge variant="warning">重复</Badge>}
                </label>
              ))}
            </div>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => (step === "preview" ? setStep("paste") : onOpenChange(false))}>
            {step === "preview" ? "上一步" : "取消"}
          </Button>
          {step === "paste" ? (
            <Button onClick={() => preview.mutate()} disabled={!content.trim() || preview.isPending}>
              {preview.isPending ? "预览中…" : "预览"}
            </Button>
          ) : (
            <Button onClick={() => commit.mutate()} disabled={selected.size === 0 || commit.isPending}>
              {commit.isPending ? "导入中…" : "导入"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function AuthOAuthDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const qc = useQueryClient()
  const [platform, setPlatform] = useState("codex")
  const [authUrl, setAuthUrl] = useState("")
  const [sessionId, setSessionId] = useState("")
  const [callback, setCallback] = useState("")
  const [name, setName] = useState("")

  const start = useMutation({
    mutationFn: async () =>
      api<{ session_id: string; auth_url: string }>("/api/admin/auth/oauth/start", {
        method: "POST",
        body: JSON.stringify({ platform }),
      }),
    onSuccess: (r) => {
      setSessionId(r.session_id)
      setAuthUrl(r.auth_url)
      setName(`${platformLabels[platform]} 账号`)
    },
  })

  const complete = useMutation({
    mutationFn: async () =>
      api("/api/admin/auth/oauth/complete", {
        method: "POST",
        body: JSON.stringify({ session_id: sessionId, callback, name }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      onOpenChange(false)
      setAuthUrl("")
      setSessionId("")
      setCallback("")
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>添加 OAuth 授权</DialogTitle>
          <DialogDescription>选择平台后，在浏览器完成授权，再把回调地址粘贴回来。</DialogDescription>
        </DialogHeader>

        {!authUrl ? (
          <div className="flex flex-col gap-1.5">
            <Label>平台</Label>
            <select value={platform} onChange={(e) => setPlatform(e.target.value)} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">
              <option value="codex">Codex (ChatGPT)</option>
              <option value="claude">Claude</option>
              <option value="grok">Grok (xAI)</option>
            </select>
          </div>
        ) : (
          <div className="space-y-3">
            <div className="rounded-lg bg-muted p-3">
              <div className="text-[10px] uppercase tracking-wider text-muted-foreground">授权地址</div>
              <a href={authUrl} target="_blank" rel="noreferrer" className="mt-1 block break-all font-mono text-xs text-primary underline">
                {authUrl}
              </a>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>名称</Label>
              <Input value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>回调地址（授权后从浏览器地址栏复制）</Label>
              <Input value={callback} onChange={(e) => setCallback(e.target.value)} placeholder="http://localhost:…?code=…" className="font-mono text-xs" />
            </div>
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          {!authUrl ? (
            <Button onClick={() => start.mutate()} disabled={start.isPending}>
              {start.isPending ? "生成中…" : "生成授权地址"}
            </Button>
          ) : (
            <Button onClick={() => complete.mutate()} disabled={!callback.trim() || complete.isPending}>
              {complete.isPending ? "验证中…" : "完成授权"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
