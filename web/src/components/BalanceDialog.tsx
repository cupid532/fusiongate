import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { formatCost } from "@/lib/utils"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

type BalanceResponse = {
  estimated_spend: { cost_micros: number }
  manual?: {
    configured_micros: number
    remaining_micros: number
    used_percent: number
    multipliers: Record<string, number>
  }
}

const multiplierLabels: Record<string, string> = {
  openai: "OpenAI",
  claude: "Claude",
  grok: "Grok",
  gemini: "Gemini",
  other: "其他",
}

type PanelProps = { open: boolean; providerId: number; providerName?: string; onClose?: () => void; onSaved?: () => void }

export function ProviderBalancePanel({ open, providerId, onClose, onSaved }: PanelProps) {
  const qc = useQueryClient()
  const [usd, setUsd] = useState("")
  const [multipliers, setMultipliers] = useState<Record<string, string>>({})

  const { data: balance, isLoading, error } = useQuery({
    queryKey: ["balance", providerId],
    queryFn: () => api<BalanceResponse>(`/api/admin/providers/${providerId}/balance`),
    enabled: open,
  })

  useEffect(() => {
    if (open && balance) {
      setUsd(balance.manual?.configured_micros != null ? (balance.manual.configured_micros / 1_000_000).toString() : "")
      const m: Record<string, string> = {}
      for (const k of Object.keys(multiplierLabels)) {
        m[k] = String(balance.manual?.multipliers?.[k] ?? 1)
      }
      setMultipliers(m)
    }
  }, [open, balance])

  const save = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        manual_balance_usd: usd.trim() ? Number(usd) : undefined,
        clear_manual_balance: !usd.trim(),
      }
      for (const k of Object.keys(multiplierLabels)) {
        body[`balance_multiplier_${k}`] = Number(multipliers[k] ?? 1)
      }
      return api(`/api/admin/providers/${providerId}`, { method: "PATCH", body: JSON.stringify(body) })
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["balance", providerId] })
      void qc.invalidateQueries({ queryKey: ["providers"] })
      onSaved?.()
    },
  })

  return (
    <div className="space-y-4">

        {balance ? (
          <div className="space-y-4">
            <div className="rounded-lg bg-muted p-3 text-sm">
              <span className="text-muted-foreground">估算累计消费：</span>
              <span className="font-semibold">{formatCost(balance.estimated_spend?.cost_micros ?? 0)}</span>
              {balance.manual && (
                <div className="mt-1 text-xs text-muted-foreground">
                  手动余额已用 {balance.manual.used_percent.toFixed(1)}% · 剩余 {formatCost(balance.manual.remaining_micros)}
                </div>
              )}
            </div>

            <div className="flex flex-col gap-1.5">
              <Label>手动余额（USD，留空清除）</Label>
              <Input value={usd} onChange={(e) => setUsd(e.target.value)} placeholder="0.00" type="number" step="0.01" />
            </div>

            <div>
              <Label>余额分类换算倍率</Label>
              <div className="mt-1 text-xs text-muted-foreground">账本成本已包含命中 Key 的成本倍率；这里再按模型类别换算渠道余额扣减。</div>
              <div className="mt-2 grid grid-cols-1 gap-3 sm:grid-cols-2">
                {Object.keys(multiplierLabels).map((k) => (
                  <div key={k} className="flex items-center gap-2">
                    <span className="w-16 text-xs text-muted-foreground">{multiplierLabels[k]}</span>
                    <Input
                      value={multipliers[k]}
                      onChange={(e) => setMultipliers((m) => ({ ...m, [k]: e.target.value }))}
                      type="number"
                      step="0.1"
                      className="h-8 text-xs"
                    />
                  </div>
                ))}
              </div>
            </div>
          </div>
        ) : (
          <div className="py-6 text-center text-sm text-muted-foreground">{isLoading ? "加载中…" : error instanceof Error ? error.message : "读取余额失败"}</div>
        )}

        {save.isError && <div role="alert" className="text-sm text-destructive">{save.error instanceof Error ? save.error.message : "保存余额失败"}</div>}
        <div className="flex justify-end gap-2">
          {onClose && <Button variant="outline" onClick={onClose}>取消</Button>}
          <Button onClick={() => save.mutate()} disabled={save.isPending || !balance}>
            {save.isPending ? "保存中…" : "保存"}
          </Button>
        </div>
    </div>
  )
}

export function BalanceDialog({ open, onOpenChange, providerId, providerName }: {
  open: boolean
  onOpenChange: (v: boolean) => void
  providerId: number
  providerName: string
}) {
  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-w-lg">
      <DialogHeader>
        <DialogTitle>余额设置 · {providerName}</DialogTitle>
        <DialogDescription>设置渠道余额基线与模型分类换算倍率，用于计算余额消耗；不会替代每张 Key 的成本倍率。</DialogDescription>
      </DialogHeader>
      <ProviderBalancePanel open={open} providerId={providerId} onClose={() => onOpenChange(false)} onSaved={() => onOpenChange(false)} />
    </DialogContent>
  </Dialog>
}
