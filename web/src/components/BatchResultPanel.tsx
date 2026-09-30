import type { BatchItemResult } from "@/lib/provider-management"
import { Badge } from "@/components/ui/badge"

export function BatchResultPanel({ results }: { results: BatchItemResult[] }) {
  if (!results.length) return null
  const successful = results.filter((item) => item.status === "success").length
  const failed = results.filter((item) => item.status === "error").length
  const unknown = results.filter((item) => item.status === "unknown").length
  const skipped = results.filter((item) => item.status === "skipped").length
  return <div role="status" className="space-y-2 rounded-lg border p-3 text-sm">
    <p>成功 {successful} · 失败 {failed} · 待核对 {unknown} · 未执行 {skipped}</p>
    <div className="max-h-48 space-y-2 overflow-y-auto">{results.map((item) => <div key={item.id} className="flex items-start justify-between gap-3 text-xs"><span className="min-w-0 break-words">{item.name}{item.message && <span className="block text-muted-foreground">{item.message}</span>}</span><Badge variant={item.status === "success" ? "success" : item.status === "error" ? "danger" : "warning"}>{item.status === "success" ? "已完成" : item.status === "error" ? "失败" : item.status === "unknown" ? "待核对" : "未执行"}</Badge></div>)}</div>
  </div>
}
