import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { FileKey } from "lucide-react"
import { api } from "@/lib/api"
import type { Provider } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"

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
  const { data: providers = [], isLoading } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
  })

  const oauth = providers.filter((p) => p.auth_kind === "oauth")

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6">
        <h1 className="text-2xl font-bold tracking-tight">认证文件</h1>
        <p className="mt-1 text-sm text-muted-foreground">管理 Codex / Claude / Grok 的 OAuth 凭据。</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <FileKey className="h-5 w-5 text-primary" />
            认证账户
          </CardTitle>
          <CardDescription>{oauth.length} 个 OAuth 凭据（含 API 渠道共用的调度策略）。</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {isLoading ? (
            <div className="p-8 text-center text-sm text-muted-foreground">加载中…</div>
          ) : oauth.length === 0 ? (
            <div className="p-8 text-center text-sm text-muted-foreground">还没有认证文件</div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">名称</th>
                    <th className="px-4 py-3 font-medium">平台</th>
                    <th className="px-4 py-3 font-medium">账号</th>
                    <th className="px-4 py-3 font-medium">状态</th>
                    <th className="px-4 py-3 font-medium">模型</th>
                  </tr>
                </thead>
                <tbody>
                  {oauth.map((p) => (
                    <tr key={p.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3 font-medium">{p.name}</td>
                      <td className="px-4 py-3 text-xs">{platformLabels[p.auth_source] ?? p.type}</td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{p.auth_email || "—"}</td>
                      <td className="px-4 py-3">{statusBadge(p)}</td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{p.model_count} 个</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </motion.div>
  )
}
