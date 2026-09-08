import { useMemo, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { useAutoAnimate } from "@formkit/auto-animate/react"
import { Plus, Trash2, Eye, KeyRound, Settings2, Lock, Search } from "lucide-react"
import { api } from "@/lib/api"
import type { APIKey } from "@/lib/types"
import { cn, formatCost } from "@/lib/utils"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { CopyButton } from "@/components/ui/copy-button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { SegmentedTabs } from "@/components/ui/segmented-tabs"
import { QueryError } from "@/components/ui/query-error"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "@/components/ui/dialog"
import { useConfirm, useConfirmDelete } from "@/components/ui/confirm"
import { notifySuccess } from "@/lib/notify"

type KeyForm = {
  name: string
  allow_models: string
  deny_models: string
  allow_all: boolean
  allow_images: boolean
  allow_audio: boolean
  rpm_limit: number
  budget_usd: string
}

const emptyForm: KeyForm = {
  name: "",
  allow_models: "",
  deny_models: "",
  allow_all: true,
  allow_images: false,
  allow_audio: false,
  rpm_limit: 120,
  budget_usd: "",
}

type Filter = "all" | "active" | "revoked" | "over_budget"

export function Keys() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const confirmDelete = useConfirmDelete()
  const [animateParent] = useAutoAnimate({ duration: 200 })
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<KeyForm>(emptyForm)
  const [revealed, setRevealed] = useState("")
  const [filter, setFilter] = useState<Filter>("all")
  const [q, setQ] = useState("")
  const [editing, setEditing] = useState<APIKey | null>(null)
  const [editForm, setEditForm] = useState<KeyForm>(emptyForm)

  const { data: keys = [], isLoading, isError, error, refetch } = useQuery({
    queryKey: ["keys"],
    queryFn: () => api<APIKey[]>("/api/admin/keys"),
  })

  const filtered = useMemo(() => {
    let list = keys
    if (filter === "active") list = list.filter((k) => !k.revoked)
    else if (filter === "revoked") list = list.filter((k) => k.revoked)
    else if (filter === "over_budget") list = list.filter((k) => k.budget_micros > 0 && k.spent_micros >= k.budget_micros)
    if (q.trim()) {
      const lower = q.toLowerCase()
      list = list.filter((k) => k.name.toLowerCase().includes(lower) || k.prefix.toLowerCase().includes(lower))
    }
    return list
  }, [keys, filter, q])

  const counts = useMemo(() => ({
    all: keys.length,
    active: keys.filter((k) => !k.revoked).length,
    revoked: keys.filter((k) => k.revoked).length,
    over_budget: keys.filter((k) => k.budget_micros > 0 && k.spent_micros >= k.budget_micros).length,
  }), [keys])

  const create = useMutation({
    mutationFn: async (f: KeyForm) => {
      const body: Record<string, unknown> = {
        name: f.name,
        allow_all: f.allow_all,
        allow_images: f.allow_images,
        allow_audio: f.allow_audio,
        rpm_limit: f.rpm_limit,
      }
      if (f.allow_models.trim()) body.allow_models = f.allow_models
      if (f.deny_models.trim()) body.deny_models = f.deny_models
      if (f.budget_usd.trim()) body.budget_micros = Math.round(Number(f.budget_usd) * 1_000_000)
      return api<{ id: number; key: string }>("/api/admin/keys", { method: "POST", body: JSON.stringify(body) })
    },
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["keys"] })
      setRevealed(res.key)
      setForm(emptyForm)
      setCreating(false)
    },
  })

  const update = useMutation({
    mutationFn: async ({ id, patch }: { id: number; patch: Record<string, unknown> }) =>
      api(`/api/admin/keys/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["keys"] })
      setEditing(null)
      notifySuccess("密钥已更新")
    },
  })

  const revoke = useMutation({
    mutationFn: async (id: number) =>
      api(`/api/admin/keys/${id}`, { method: "PATCH", body: JSON.stringify({ revoked: true }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["keys"] })
      notifySuccess("密钥已吊销")
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

  const set = <K extends keyof KeyForm>(k: K, v: KeyForm[K]) => setForm((f) => ({ ...f, [k]: v }))
  const setEdit = <K extends keyof KeyForm>(k: K, v: KeyForm[K]) => setEditForm((f) => ({ ...f, [k]: v }))

  function openEdit(k: APIKey) {
    setEditing(k)
    setEditForm({
      name: k.name,
      allow_models: k.allow_models || "",
      deny_models: k.deny_models || "",
      allow_all: k.allow_all,
      allow_images: k.allow_images,
      allow_audio: k.allow_audio,
      rpm_limit: k.rpm_limit,
      budget_usd: k.budget_micros > 0 ? String(k.budget_micros / 1_000_000) : "",
    })
  }

  function submitEdit() {
    if (!editing) return
    const patch: Record<string, unknown> = {
      name: editForm.name,
      allow_all: editForm.allow_all,
      allow_models: editForm.allow_models,
      deny_models: editForm.deny_models,
      allow_images: editForm.allow_images,
      allow_audio: editForm.allow_audio,
      rpm_limit: editForm.rpm_limit,
    }
    if (editForm.budget_usd.trim()) patch.budget_usd = Number(editForm.budget_usd)
    update.mutate({ id: editing.id, patch })
  }

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">访问密钥</h1>
          <p className="mt-1 text-sm text-muted-foreground">签发下游 API Key，按权限访问渠道和模型。</p>
        </div>
        <Button onClick={() => setCreating((v) => !v)}>
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
          <CardContent className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>名称</Label>
              <Input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="例如：Hermes local" />
            </div>
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>允许的模型（逗号分隔，留空配合下方「全部模型」）</Label>
              <Input value={form.allow_models} onChange={(e) => set("allow_models", e.target.value)} placeholder="gpt-4,claude-3-5" />
            </div>
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>拒绝的模型（逗号分隔）</Label>
              <Input value={form.deny_models} onChange={(e) => set("deny_models", e.target.value)} placeholder="gpt-4-mini" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>RPM 限制</Label>
              <Input type="number" value={form.rpm_limit} onChange={(e) => set("rpm_limit", Number(e.target.value))} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>预算（USD，留空无限制）</Label>
              <Input value={form.budget_usd} onChange={(e) => set("budget_usd", e.target.value)} placeholder="0.00" type="number" step="0.01" />
            </div>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={form.allow_all} onChange={(e) => set("allow_all", e.target.checked)} />
              允许全部模型
            </label>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={form.allow_images} onChange={(e) => set("allow_images", e.target.checked)} />
              允许图片生成
            </label>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={form.allow_audio} onChange={(e) => set("allow_audio", e.target.checked)} />
              允许音频（TTS / 转录）
            </label>
            <div className="flex justify-end gap-2 sm:col-span-2">
              <Button variant="ghost" onClick={() => setCreating(false)}>取消</Button>
              <Button onClick={() => create.mutate(form)} disabled={!form.name.trim() || create.isPending}>
                {create.isPending ? "创建中…" : "创建"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {revealed && (
        <Card className="mb-4 border-primary/40">
          <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center">
            <div className="flex min-w-0 flex-1 items-start gap-3 sm:items-center">
              <KeyRound className="mt-0.5 h-5 w-5 shrink-0 text-primary sm:mt-0" />
              <div className="min-w-0 flex-1">
                <div className="mb-1 text-xs font-medium text-muted-foreground">完整访问密钥</div>
                <code className="block break-all font-mono text-sm">{revealed}</code>
              </div>
            </div>
            <div className="flex shrink-0 justify-end gap-2">
              <CopyButton value={revealed} label="复制密钥" copiedLabel="密钥已复制" variant="outline" size="sm" />
              <Button variant="outline" size="sm" onClick={() => setRevealed("")}>关闭</Button>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-col gap-3 border-b p-4 sm:flex-row sm:items-center sm:justify-between">
            <SegmentedTabs
              tabs={[
                { value: "all" as Filter, label: "全部", count: counts.all },
                { value: "active" as Filter, label: "活跃", count: counts.active },
                { value: "revoked" as Filter, label: "已吊销", count: counts.revoked },
                { value: "over_budget" as Filter, label: "超额", count: counts.over_budget },
              ]}
              value={filter}
              onChange={setFilter}
            />
            <div className="relative">
              <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索名称或前缀…" className="h-8 w-full pl-8 text-xs sm:w-48" />
            </div>
          </div>

          {isLoading ? (
            <div className="space-y-2 p-4">{Array.from({ length: 4 }).map((_, i) => <div key={i} className="h-12 animate-pulse rounded-lg bg-muted/40" />)}</div>
          ) : isError ? (
            <QueryError title="无法加载密钥列表" error={error} onRetry={() => void refetch()} />
          ) : filtered.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">{keys.length === 0 ? "还没有密钥" : "没有匹配的密钥"}</div>
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
                <tbody ref={animateParent}>
                  {filtered.map((k) => (
                    <tr key={k.id} className={cn("border-b last:border-0 hover:bg-muted/40", k.revoked && "opacity-60")}>
                      <td className={cn("px-4 py-3 font-medium", k.revoked && "line-through")}>{k.name}</td>
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
                          <span className={k.spent_micros >= k.budget_micros ? "font-semibold text-destructive" : ""}>
                            {formatCost(k.spent_micros)} / {formatCost(k.budget_micros)}
                          </span>
                        ) : (
                          <span className="text-muted-foreground">无限制</span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs">{k.rpm_limit}</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          <Button variant="ghost" size="icon" onClick={() => openEdit(k)} aria-label={`编辑 ${k.name}`}>
                            <Settings2 className="h-4 w-4" />
                          </Button>
                          <Button variant="ghost" size="icon" onClick={() => reveal.mutate(k.id)} aria-label={`查看 ${k.name}`}>
                            <Eye className="h-4 w-4" />
                          </Button>
                          {!k.revoked && (
                            <Button
                              variant="ghost"
                              size="icon"
                              onClick={async () => {
                                if (await confirm({ title: `吊销密钥「${k.name}」`, description: "吊销后使用该密钥的客户端会立即失去访问权限。密钥保留在列表中但不可恢复。" }))
                                  revoke.mutate(k.id)
                              }}
                              aria-label={`吊销 ${k.name}`}
                            >
                              <Lock className="h-4 w-4 text-amber-600" />
                            </Button>
                          )}
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={async () => {
                              if (await confirmDelete(`访问密钥「${k.name}」`, "删除后无法恢复，使用记录也将清除。")) remove.mutate(k.id)
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

      <Dialog open={!!editing} onOpenChange={(o) => { if (!o) setEditing(null) }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>编辑密钥</DialogTitle>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>名称</Label>
              <Input value={editForm.name} onChange={(e) => setEdit("name", e.target.value)} />
            </div>
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>允许的模型</Label>
              <Input value={editForm.allow_models} onChange={(e) => setEdit("allow_models", e.target.value)} placeholder="逗号分隔" />
            </div>
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <Label>拒绝的模型</Label>
              <Input value={editForm.deny_models} onChange={(e) => setEdit("deny_models", e.target.value)} placeholder="逗号分隔" />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>RPM 限制</Label>
              <Input type="number" value={editForm.rpm_limit} onChange={(e) => setEdit("rpm_limit", Number(e.target.value))} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label>预算（USD）</Label>
              <Input value={editForm.budget_usd} onChange={(e) => setEdit("budget_usd", e.target.value)} placeholder="留空无限制" type="number" step="0.01" />
            </div>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={editForm.allow_all} onChange={(e) => setEdit("allow_all", e.target.checked)} />
              允许全部模型
            </label>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={editForm.allow_images} onChange={(e) => setEdit("allow_images", e.target.checked)} />
              允许图片生成
            </label>
            <label className="flex items-center gap-2 text-sm sm:col-span-2">
              <input type="checkbox" checked={editForm.allow_audio} onChange={(e) => setEdit("allow_audio", e.target.checked)} />
              允许音频
            </label>
          </div>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setEditing(null)}>取消</Button>
            <Button onClick={submitEdit} disabled={!editForm.name.trim() || update.isPending}>
              {update.isPending ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.div>
  )
}
