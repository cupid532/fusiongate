import { useMemo, useState } from "react"
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import { Plus, Trash2, RefreshCw, Search, Settings2, ScanSearch, HeartPulse, Wallet, KeySquare, DatabaseBackup, Archive, FolderTree } from "lucide-react"
import { api } from "@/lib/api"
import type { Provider, RoutingStrategy } from "@/lib/types"
import { cn } from "@/lib/utils"
import { Card, CardContent } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { Switch } from "@/components/ui/switch"
import { ProviderDialog } from "@/components/ProviderDialog"
import { ModelPicker } from "@/components/ModelPicker"
import { HealthCheckDialog } from "@/components/HealthCheckDialog"
import { BalanceDialog } from "@/components/BalanceDialog"
import { ProviderKeysDialog } from "@/components/ProviderKeysDialog"
import { ExportImportDialog } from "@/components/ExportImportDialog"
import { GroupManager } from "@/components/GroupManager"
import { InlinePriorityEditor } from "@/components/InlinePriorityEditor"

type Filter = "all" | "enabled" | "disabled" | "archived"

const typeLabels: Record<string, string> = {
  openai: "OpenAI",
  grok: "Grok",
  openrouter: "OpenRouter",
  openai_compatible: "OpenAI 兼容",
  anthropic: "Anthropic",
  claude_oauth: "Claude OAuth",
  codex_oauth: "Codex OAuth",
  grok_oauth: "Grok OAuth",
  gemini: "Gemini",
  opencode: "OpenCode",
}

function statusBadge(p: Provider) {
  if (p.archived) return <Badge variant="neutral">归档</Badge>
  if (!p.enabled) return <Badge variant="neutral">已停用</Badge>
  if (p.status === "circuit_open") return <Badge variant="warning">熔断冷却</Badge>
  if (p.health_check_status === "unhealthy") return <Badge variant="danger">不健康</Badge>
  if (p.health_check_status === "healthy") return <Badge variant="success">健康</Badge>
  if (p.consecutive_failures > 0) return <Badge variant="warning">不稳定</Badge>
  return <Badge variant="success">运行中</Badge>
}

export function Providers() {
  const qc = useQueryClient()
  const [filter, setFilter] = useState<Filter>("all")
  const [q, setQ] = useState("")
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Provider | null>(null)
  const [modelPickerOpen, setModelPickerOpen] = useState(false)
  const [modelPickerProvider, setModelPickerProvider] = useState<Provider | null>(null)
  const [healthCheckOpen, setHealthCheckOpen] = useState(false)
  const [healthCheckProvider, setHealthCheckProvider] = useState<Provider | null>(null)
  const [balanceOpen, setBalanceOpen] = useState(false)
  const [balanceProvider, setBalanceProvider] = useState<Provider | null>(null)
  const [keysOpen, setKeysOpen] = useState(false)
  const [keysProvider, setKeysProvider] = useState<Provider | null>(null)
  const [backupOpen, setBackupOpen] = useState(false)
  const [groupOpen, setGroupOpen] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())

  const { data: routing } = useQuery({
    queryKey: ["routing"],
    queryFn: () => api<{ strategy: RoutingStrategy }>("/api/admin/routing"),
  })

  const setStrategy = useMutation({
    mutationFn: async (strategy: RoutingStrategy) =>
      api("/api/admin/routing", { method: "PATCH", body: JSON.stringify({ strategy }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["routing"] }),
  })

  const { data: providers = [], isLoading, refetch, isFetching } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
    select: (list) => list.filter((p) => p.auth_kind !== "oauth"),
  })

  // 批量获取余额（仅对有手动余额的渠道显示进度条）
  const balances = useQueries({
    queries: providers.map((p) => ({
      queryKey: ["balance", p.id],
      queryFn: () =>
        api<{ manual?: { configured_micros: number; remaining_micros: number; used_percent: number } }>(
          `/api/admin/providers/${p.id}/balance`
        ),
      staleTime: 60_000,
    })),
  })
  const balanceMap = useMemo(() => {
    const m = new Map<number, { remaining_micros: number; used_percent: number }>()
    providers.forEach((p, i) => {
      const b = balances[i]?.data?.manual
      if (b) m.set(p.id, { remaining_micros: b.remaining_micros, used_percent: b.used_percent })
    })
    return m
  }, [providers, balances])

  const update = useMutation({
    mutationFn: async ({ id, patch }: { id: number; patch: Record<string, unknown> }) =>
      api(`/api/admin/providers/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers"] }),
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/providers/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["providers"] }),
  })

  const batch = useMutation({
    mutationFn: async ({ ids, action }: { ids: number[]; action: "enable" | "disable" | "delete" }) =>
      api("/api/admin/providers/batch", { method: "POST", body: JSON.stringify({ provider_ids: ids, action }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      setSelected(new Set())
    },
  })

  const filtered = useMemo(() => {
    let list = providers
    if (filter === "enabled") list = list.filter((p) => p.enabled && !p.archived)
    else if (filter === "disabled") list = list.filter((p) => !p.enabled && !p.archived)
    else if (filter === "archived") list = list.filter((p) => p.archived)
    else list = list.filter((p) => !p.archived)
    if (q.trim()) {
      const kw = q.trim().toLowerCase()
      list = list.filter((p) => p.name.toLowerCase().includes(kw) || p.base_url.toLowerCase().includes(kw))
    }
    return list
  }, [providers, filter, q])

  const counts = useMemo(
    () => ({
      all: providers.filter((p) => !p.archived).length,
      enabled: providers.filter((p) => p.enabled && !p.archived).length,
      disabled: providers.filter((p) => !p.enabled && !p.archived).length,
      archived: providers.filter((p) => p.archived).length,
    }),
    [providers]
  )

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6 flex items-end justify-between gap-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">上游渠道</h1>
          <p className="mt-1 text-sm text-muted-foreground">管理 API 渠道、优先级与全局故障转移。</p>
        </div>
        <div className="flex items-end gap-3">
          <div className="flex flex-col gap-1.5">
            <span className="text-xs font-medium text-muted-foreground">故障转移模式</span>
            <select
              value={routing?.strategy ?? "priority_failover"}
              onChange={(e) => setStrategy.mutate(e.target.value as RoutingStrategy)}
              className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
            >
              <option value="priority_failover">按优先级故障转移</option>
              <option value="ordered_round_robin">固定轮询</option>
              <option value="smart_round_robin">智能轮询</option>
              <option value="adaptive">自适应</option>
            </select>
          </div>
          <Button variant="outline" onClick={() => setGroupOpen(true)}>
            <FolderTree className="h-4 w-4" />
            分组
          </Button>
          <Button variant="outline" onClick={() => setBackupOpen(true)}>
            <DatabaseBackup className="h-4 w-4" />
            备份
          </Button>
          <Button onClick={() => { setEditing(null); setDialogOpen(true) }}>
            <Plus className="h-4 w-4" />
            添加渠道
          </Button>
        </div>
      </div>

      <Card>
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b p-3">
            <div className="flex flex-wrap gap-1.5">
              {(
                [
                  ["all", "全部", counts.all],
                  ["enabled", "参与调度", counts.enabled],
                  ["disabled", "已停用", counts.disabled],
                  ["archived", "归档", counts.archived],
                ] as [Filter, string, number][]
              ).map(([f, label, n]) => (
                <button
                  key={f}
                  onClick={() => setFilter(f)}
                  className={cn(
                    "flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs font-medium transition-colors",
                    filter === f
                      ? "border-primary bg-primary/10 text-primary"
                      : "text-muted-foreground hover:text-foreground"
                  )}
                >
                  {label}
                  <span className="rounded-full bg-muted px-1.5 text-[10px]">{n}</span>
                </button>
              ))}
            </div>
            <div className="flex items-center gap-2">
              {selected.size > 0 && (
                <div className="flex items-center gap-1.5">
                  <span className="text-xs text-muted-foreground">已选 {selected.size}</span>
                  <Button size="sm" variant="outline" onClick={() => batch.mutate({ ids: [...selected], action: "enable" })}>
                    启用
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => batch.mutate({ ids: [...selected], action: "disable" })}>
                    停用
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    onClick={() => {
                      if (confirm(`删除选中的 ${selected.size} 个渠道？`)) batch.mutate({ ids: [...selected], action: "delete" })
                    }}
                  >
                    删除
                  </Button>
                </div>
              )}
              <div className="relative">
                <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索渠道" className="h-8 w-52 pl-8 text-xs" />
              </div>
              <Button variant="ghost" size="icon" onClick={() => void refetch()} aria-label="刷新">
                <RefreshCw className={cn("h-4 w-4", isFetching && "animate-spin")} />
              </Button>
            </div>
          </div>

          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : filtered.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">没有符合条件的渠道</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="w-10 px-3 py-3">
                      <input
                        type="checkbox"
                        checked={filtered.length > 0 && filtered.every((p) => selected.has(p.id))}
                        onChange={(e) => {
                          if (e.target.checked) setSelected(new Set(filtered.map((p) => p.id)))
                          else setSelected(new Set())
                        }}
                      />
                    </th>
                    <th className="px-4 py-3 font-medium">渠道</th>
                    <th className="px-4 py-3 font-medium">类型</th>
                    <th className="w-24 px-4 py-3 font-medium">优先级</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">模型</th>
                    <th className="px-4 py-3 text-right font-medium">开关 / 操作</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map((p) => (
                    <tr key={p.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-3 py-3">
                        <input
                          type="checkbox"
                          checked={selected.has(p.id)}
                          onChange={(e) => {
                            const next = new Set(selected)
                            if (e.target.checked) next.add(p.id)
                            else next.delete(p.id)
                            setSelected(next)
                          }}
                        />
                      </td>
                      <td className="px-4 py-3">
                        <a href={p.base_url} target="_blank" rel="noreferrer" className="font-medium hover:text-primary hover:underline">
                          {p.name}
                        </a>
                        <div className="truncate text-xs text-muted-foreground">{p.base_url}</div>
                        {balanceMap.has(p.id) && (
                          <div className="mt-2 max-w-[220px]">
                            <div className="mb-1 flex items-center justify-between text-[10px] text-muted-foreground">
                              <span>余额已用</span>
                              <span className="font-medium text-foreground">
                                {balanceMap.get(p.id)!.used_percent.toFixed(1)}%
                              </span>
                            </div>
                            <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                              <div
                                className={cn(
                                  "h-1.5 rounded-full",
                                  balanceMap.get(p.id)!.used_percent > 90 ? "bg-destructive" : balanceMap.get(p.id)!.used_percent > 70 ? "bg-amber-500" : "bg-primary"
                                )}
                                style={{ width: `${Math.min(100, balanceMap.get(p.id)!.used_percent)}%` }}
                              />
                            </div>
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{typeLabels[p.type] ?? p.type}</td>
                      <td className="px-4 py-3"><InlinePriorityEditor value={p.priority} disabled={update.isPending} onSave={async (priority) => { await update.mutateAsync({ id: p.id, patch: { priority } }) }} /></td>
                      <td className="px-4 py-3"><div className="flex items-center gap-2">{statusBadge(p)}{p.consecutive_failures > 0 && <span className="text-[10px] tabular-nums text-muted-foreground">{p.consecutive_failures}/5 失败</span>}</div></td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{p.model_count} 个</td>
                      <td className="px-4 py-3">
                        <div className="flex items-center justify-end gap-1">
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              setEditing(p)
                              setDialogOpen(true)
                            }}
                            aria-label={`编辑 ${p.name}`}
                          >
                            <Settings2 className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              setModelPickerProvider(p)
                              setModelPickerOpen(true)
                            }}
                            aria-label={`识别 ${p.name} 模型`}
                          >
                            <ScanSearch className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              setHealthCheckProvider(p)
                              setHealthCheckOpen(true)
                            }}
                            aria-label={`检活 ${p.name}`}
                          >
                            <HeartPulse className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              setBalanceProvider(p)
                              setBalanceOpen(true)
                            }}
                            aria-label={`余额设置 ${p.name}`}
                          >
                            <Wallet className="h-4 w-4" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              setKeysProvider(p)
                              setKeysOpen(true)
                            }}
                            aria-label={`Key 管理 ${p.name}`}
                          >
                            <KeySquare className="h-4 w-4" />
                          </Button>
                          <Switch
                            checked={p.enabled}
                            onCheckedChange={(v) => update.mutate({ id: p.id, patch: { enabled: v } })}
                            aria-label={`${p.name} 开关`}
                          />
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() =>
                              update.mutate({ id: p.id, patch: { archived: !p.archived } })
                            }
                            aria-label={p.archived ? `取消归档 ${p.name}` : `归档 ${p.name}`}
                            title={p.archived ? "取消归档" : "归档"}
                          >
                            <Archive className={cn("h-4 w-4", p.archived && "text-amber-500")} />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            onClick={() => {
                              if (confirm(`删除渠道「${p.name}」？`)) remove.mutate(p.id)
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
          )}
        </CardContent>
      </Card>

      <ProviderDialog open={dialogOpen} onOpenChange={setDialogOpen} provider={editing} />
      {modelPickerProvider && (
        <ModelPicker
          open={modelPickerOpen}
          onOpenChange={setModelPickerOpen}
          providerId={modelPickerProvider.id}
          providerName={modelPickerProvider.name}
        />
      )}
      {healthCheckProvider && (
        <HealthCheckDialog
          open={healthCheckOpen}
          onOpenChange={setHealthCheckOpen}
          providerId={healthCheckProvider.id}
          providerName={healthCheckProvider.name}
        />
      )}
      {balanceProvider && (
        <BalanceDialog
          open={balanceOpen}
          onOpenChange={setBalanceOpen}
          providerId={balanceProvider.id}
          providerName={balanceProvider.name}
        />
      )}
      {keysProvider && (
        <ProviderKeysDialog
          open={keysOpen}
          onOpenChange={setKeysOpen}
          providerId={keysProvider.id}
          providerName={keysProvider.name}
        />
      )}
      <ExportImportDialog open={backupOpen} onOpenChange={setBackupOpen} />
      <GroupManager open={groupOpen} onOpenChange={setGroupOpen} />
    </motion.div>
  )
}
