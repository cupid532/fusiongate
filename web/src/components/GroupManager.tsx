import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "@/lib/api"
import type { ProviderGroup } from "@/lib/types"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"

export function GroupManager({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState("")

  const { data: groups = [], isLoading } = useQuery({
    queryKey: ["provider-groups"],
    queryFn: () => api<ProviderGroup[]>("/api/admin/provider-groups"),
    enabled: open,
  })

  const create = useMutation({
    mutationFn: async (n: string) => api("/api/admin/provider-groups", { method: "POST", body: JSON.stringify({ name: n }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["provider-groups"] })
      setName("")
    },
  })

  const remove = useMutation({
    mutationFn: async (id: number) => api(`/api/admin/provider-groups/${id}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["provider-groups"] }),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>渠道分组</DialogTitle>
          <DialogDescription>创建分组，然后在渠道编辑里把渠道分配到分组。</DialogDescription>
        </DialogHeader>

        <div className="flex items-end gap-2">
          <div className="flex flex-1 flex-col gap-1.5">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="分组名称" />
          </div>
          <Button onClick={() => create.mutate(name)} disabled={!name.trim() || create.isPending}>
            新建
          </Button>
        </div>

        <div className="space-y-1.5">
          {isLoading ? (
            <div className="py-4 text-center text-sm text-muted-foreground">加载中…</div>
          ) : groups.length === 0 ? (
            <div className="py-4 text-center text-sm text-muted-foreground">还没有分组</div>
          ) : (
            groups.map((g) => (
              <div key={g.id} className="flex items-center gap-3 rounded-lg border px-3 py-2">
                <div className="flex-1">
                  <div className="text-sm font-medium">{g.name}</div>
                </div>
                <Badge variant="neutral">{g.member_count} 个渠道</Badge>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    if (confirm(`删除分组「${g.name}」？成员渠道会变为未分组。`)) remove.mutate(g.id)
                  }}
                >
                  删除
                </Button>
              </div>
            ))
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
