import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { IPPoolNode, Provider, ProviderGroup } from "@/lib/types"
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
import { Textarea } from "@/components/ui/textarea"

const providerTypes = [
  { value: "openai_compatible", label: "OpenAI 兼容" },
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
  { value: "gemini", label: "Gemini" },
  { value: "openrouter", label: "OpenRouter" },
  { value: "grok", label: "Grok" },
  { value: "opencode", label: "OpenCode" },
]

const passthroughModes = [
  { value: "normalized", label: "标准化（转换协议）" },
  { value: "transparent", label: "透传（原样转发）" },
]

export function ProviderDialog({
  open,
  onOpenChange,
  provider,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  provider: Provider | null
}) {
  const qc = useQueryClient()
  const [form, setForm] = useState({
    name: "",
    type: "openai_compatible",
    baseURL: "",
    credential: "",
    priority: 1,
    max_concurrency: 0,
    request_timeout_ms: 120000,
    passthrough_mode: "normalized",
    notes: "",
    ip_pool_node_id: 0,
    group_id: 0,
  })

  const { data: nodes = [] } = useQuery({
    queryKey: ["ippool"],
    queryFn: () => api<IPPoolNode[]>("/api/admin/ip-pool"),
  })
  const { data: groups = [] } = useQuery({
    queryKey: ["provider-groups"],
    queryFn: () => api<ProviderGroup[]>("/api/admin/provider-groups"),
  })

  useEffect(() => {
    if (open) {
      setForm({
        name: provider?.name ?? "",
        type: provider?.type ?? "openai_compatible",
        baseURL: provider?.base_url ?? "",
        credential: "",
        priority: provider?.priority ?? 1,
        max_concurrency: provider?.max_concurrency ?? 0,
        request_timeout_ms: provider?.request_timeout_ms ?? 120000,
        passthrough_mode: provider?.passthrough_mode ?? "normalized",
        notes: provider?.notes ?? "",
        ip_pool_node_id: provider?.ip_pool_node_id ?? 0,
        group_id: provider?.group_id ?? 0,
      })
    }
  }, [open, provider])

  const save = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        name: form.name,
        type: form.type,
        baseURL: form.baseURL,
        priority: form.priority,
        max_concurrency: form.max_concurrency,
        request_timeout_ms: form.request_timeout_ms,
        passthrough_mode: form.passthrough_mode,
        notes: form.notes,
        ip_pool_node_id: form.ip_pool_node_id || null,
        group_id: form.group_id || null,
      }
      if (form.credential) body.credential = form.credential
      if (provider) {
        return api(`/api/admin/providers/${provider.id}`, { method: "PATCH", body: JSON.stringify(body) })
      }
      return api("/api/admin/providers", { method: "POST", body: JSON.stringify(body) })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      onOpenChange(false)
    },
  })

  const set = (k: keyof typeof form, v: string | number) => setForm((f) => ({ ...f, [k]: v }))

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{provider ? "编辑渠道" : "添加 API 渠道"}</DialogTitle>
          <DialogDescription>配置上游连接信息与运行参数。</DialogDescription>
        </DialogHeader>

        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col gap-1.5">
            <Label>名称</Label>
            <Input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="例如：粥API" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>类型</Label>
            <select
              value={form.type}
              onChange={(e) => set("type", e.target.value)}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              {providerTypes.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
          </div>
          <div className="col-span-2 flex flex-col gap-1.5">
            <Label>API 地址</Label>
            <Input value={form.baseURL} onChange={(e) => set("baseURL", e.target.value)} placeholder="https://api.example.com" className="font-mono text-xs" />
          </div>
          <div className="col-span-2 flex flex-col gap-1.5">
            <Label>API Key{provider ? "（留空保持不变）" : ""}</Label>
            <Input value={form.credential} onChange={(e) => set("credential", e.target.value)} placeholder="sk-…" className="font-mono text-xs" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>优先级</Label>
            <Input type="number" value={form.priority} onChange={(e) => set("priority", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>转发模式</Label>
            <select
              value={form.passthrough_mode}
              onChange={(e) => set("passthrough_mode", e.target.value)}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              {passthroughModes.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>IP 出口</Label>
            <select
              value={form.ip_pool_node_id}
              onChange={(e) => set("ip_pool_node_id", Number(e.target.value))}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              <option value={0}>本机直连</option>
              {nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}（{n.protocol}）
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>最大并发（0 = 不限）</Label>
            <Input type="number" value={form.max_concurrency} onChange={(e) => set("max_concurrency", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>请求超时（ms）</Label>
            <Input type="number" value={form.request_timeout_ms} onChange={(e) => set("request_timeout_ms", Number(e.target.value))} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>分组</Label>
            <select
              value={form.group_id}
              onChange={(e) => set("group_id", Number(e.target.value))}
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
            >
              <option value={0}>未分组</option>
              {groups.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>
          </div>
          <div className="col-span-2 flex flex-col gap-1.5">
            <Label>备注</Label>
            <Textarea value={form.notes} onChange={(e) => set("notes", e.target.value)} rows={2} />
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={() => save.mutate()} disabled={!form.name.trim() || !form.baseURL.trim() || save.isPending}>
            {save.isPending ? "保存中…" : "保存渠道"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
