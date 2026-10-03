import { useRef, useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { FileUp } from "lucide-react"
import { api, apiDownload, saveBlob } from "@/lib/api"
import { refreshProviderViews } from "@/lib/provider-management"
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
  const [error, setError] = useState("")
  const [result, setResult] = useState<{ providers_created: number; providers_updated: number; warnings?: string[] } | null>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const reader = new FileReader()
    reader.onload = () => {
      if (typeof reader.result === "string") { setContent(reader.result); setError(""); setResult(null) }
    }
    reader.onerror = () => setError("无法读取备份文件")
    reader.readAsText(file)
    // Reset the input so the same file can be selected again if needed.
    e.target.value = ""
  }

  const doExport = async () => {
    setExporting(true)
    setError("")
    try {
      const { blob } = await apiDownload(
        "/api/admin/providers/export",
        { method: "POST" },
        { what: "渠道备份", mime: ["application/json"], json: true },
      )
      saveBlob(blob, "fusiongate-providers.json")
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "导出失败")
    } finally {
      setExporting(false)
    }
  }

  const doImport = useMutation({
    mutationFn: async () => {
      setError("")
      setResult(null)
      const data = JSON.parse(content)
      return api<NonNullable<typeof result>>("/api/admin/providers/import", { method: "POST", body: JSON.stringify(data) })
    },
    onSuccess: async (data) => {
      setResult(data)
      setContent("")
      await refreshProviderViews(qc)
    },
    onError: (reason) => setError(reason instanceof SyntaxError ? "备份 JSON 格式不正确" : reason instanceof Error ? reason.message : "导入失败"),
  })

  return (
    <Dialog open={open} onOpenChange={(value) => { if (!doImport.isPending && !exporting) onOpenChange(value) }}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>渠道备份</DialogTitle>
          <DialogDescription>按渠道名称和 Key 指纹合并配置，保留备份之外的 Key 与路由；不恢复访问密钥、密码或出口节点。</DialogDescription>
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

        {error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
        {result && <div role="status" className="space-y-2 text-sm"><p>导入完成：新增 {result.providers_created} 个渠道，更新 {result.providers_updated} 个渠道。</p>{result.warnings?.map((warning, index) => <p key={index} className="break-words text-amber-700 dark:text-amber-400">{warning}</p>)}</div>}
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
                onChange={(e) => { setContent(e.target.value); setError(""); setResult(null) }}
                placeholder="粘贴导出的渠道备份 JSON"
                className="min-h-[200px] font-mono text-xs"
              />
            </div>
            <DialogFooter>
              <Button variant="outline" disabled={doImport.isPending} onClick={() => onOpenChange(false)}>
                {result ? "关闭" : "取消"}
              </Button>
              <Button onClick={() => doImport.mutate()} disabled={!content.trim() || doImport.isPending}>
                {doImport.isPending ? "导入中…" : "导入"}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <p className="text-sm text-muted-foreground">导出渠道、上游 Key、模型清单、路由和排除规则，含上游密钥，请妥善保管。</p>
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
