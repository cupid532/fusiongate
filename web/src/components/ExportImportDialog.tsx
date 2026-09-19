import { useRef, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { FileUp } from "lucide-react"
import { api, getCsrfToken } from "@/lib/api"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"

export function ExportImportDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const qc = useQueryClient()
  const [tab, setTab] = useState<"import" | "export">("import")
  const [content, setContent] = useState("")
  const [exporting, setExporting] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = () => {
      if (typeof reader.result === "string") setContent(reader.result)
    }
    reader.readAsText(file)
    // Reset the input so the same file can be selected again if needed.
    e.target.value = ""
  }

  const doExport = async () => {
    setExporting(true)
    try {
      const res = await fetch("/api/admin/providers/export", { method: "POST", headers: { "X-CSRF-Token": getCsrfToken() } })
      const blob = await res.blob()
      const url = URL.createObjectURL(blob)
      const a = document.createElement("a")
      a.href = url
      a.download = "fusiongate-providers.json"
      a.click()
      URL.revokeObjectURL(url)
    } finally {
      setExporting(false)
    }
  }

  const doImport = useMutation({
    mutationFn: async () => {
      const data = JSON.parse(content)
      return api("/api/admin/providers/import", { method: "POST", body: JSON.stringify(data) })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["providers"] })
      qc.invalidateQueries({ queryKey: ["routes"] })
      onOpenChange(false)
      setContent("")
    },
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>渠道备份</DialogTitle>
          <DialogDescription>导出或导入渠道配置（含 Key 与模型路由）。</DialogDescription>
        </DialogHeader>

        <div className="flex gap-1.5">
          {(
            [
              ["import", "导入"],
              ["export", "导出"],
            ] as const
          ).map(([t, label]) => (
            <button
              key={t}
              onClick={() => setTab(t)}
              className={`rounded-lg border px-3 py-1.5 text-xs font-medium transition-colors ${
                tab === t ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {label}
            </button>
          ))}
        </div>

        {tab === "import" ? (
          <>
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <Button variant="outline" size="sm" onClick={() => fileInputRef.current?.click()}>
                  <FileUp className="h-4 w-4" />
                  选择文件
                </Button>
                <span className="text-xs text-muted-foreground">或直接在下方粘贴 JSON</span>
                <input ref={fileInputRef} type="file" accept=".json,application/json" className="hidden" onChange={handleFileSelect} />
              </div>
              <Textarea
                value={content}
                onChange={(e) => setContent(e.target.value)}
                placeholder="粘贴导出的渠道备份 JSON"
                className="min-h-[200px] font-mono text-xs"
              />
            </div>
            <DialogFooter>
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                取消
              </Button>
              <Button onClick={() => doImport.mutate()} disabled={!content.trim() || doImport.isPending}>
                {doImport.isPending ? "导入中…" : "导入"}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <p className="text-sm text-muted-foreground">导出包含渠道、Key 和模型路由的完整备份（含密钥）。</p>
            <DialogFooter>
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                取消
              </Button>
              <Button onClick={doExport} disabled={exporting}>
                {exporting ? "导出中…" : "下载备份"}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
