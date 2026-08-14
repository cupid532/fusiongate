import { useEffect, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import { formatCost } from "@/lib/utils"
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

export function BalanceDialog({
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
  const [usd, setUsd] = useState("")
  const [multipliers, setMultipliers] = useState<Record<string, string>>({})

  const { data: balance } = useQuery({
    queryKey: ["balance", providerId],
    queryFn: () => api<BalanceResponse>(`/api/admin/providers/${providerId}/balance`),
    enabled: open,
  })

  useEffect(() => {
    if (balance) {
      setUsd(balance.manual?.configured_micros != null ? (balance.manual.configured_micros / 1_000_000).toString() : "")
      const m: Record<string, string> = {}
      for (const k of Object.keys(multiplierLabels)) {
        m[k] = String(balance.manual?.multipliers?.[k] ?? 1)
      }
      setMultipliers(m)
    }
  }, [balance])

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
      qc.invalidateQueries({ queryKey: ["balance", providerId] })
      qc.invalidateQueries({ queryKey: ["providers"] })
      onOpenChange(false)
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>余额设置 · {providerName}</DialogTitle>
          <DialogDescription>设置手动余额与计价倍率，用于成本追踪。</DialogDescription>
        </DialogHeader>

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
              <Label>计价倍率</Label>
              <div className="mt-2 grid grid-cols-2 gap-3">
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
          <div className="py-6 text-center text-sm text-muted-foreground">加载中…</div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
