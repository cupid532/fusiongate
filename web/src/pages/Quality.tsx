import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { ShieldCheck } from "lucide-react"
import { api } from "@/lib/api"
import type { QualityDetectorData } from "@/lib/types"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"

export function Quality() {
  const { data, isLoading } = useQuery({
    queryKey: ["quality"],
    queryFn: () => api<QualityDetectorData>("/api/admin/quality-detector"),
  })

  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
      <div className="mb-6">
        <h1 className="text-2xl font-bold tracking-tight">质量检测</h1>
        <p className="mt-1 text-sm text-muted-foreground">对上游模型做生成质量探测。</p>
      </div>

      <Card className="mb-4">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <ShieldCheck className="h-5 w-5 text-primary" />
            检测器状态
          </CardTitle>
          <CardDescription>FusionGate 质量检测 sidecar 的运行状态。</CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading ? (
            <div className="text-sm text-muted-foreground">加载中…</div>
          ) : data?.available ? (
            <div className="flex items-center gap-3">
              <Badge variant="success">可用</Badge>
              <span className="text-sm text-muted-foreground">版本 {data.version}</span>
              <span className="text-sm text-muted-foreground">{data.targets.length} 个目标</span>
            </div>
          ) : (
            <Badge variant="danger">不可用</Badge>
          )}
        </CardContent>
      </Card>

      {data && data.targets.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">检测目标</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-xs text-muted-foreground">
                    <th className="px-4 py-3 font-medium">模型</th>
                    <th className="px-4 py-3 font-medium">渠道</th>
                    <th className="px-4 py-3 font-medium">上游模型</th>
                    <th className="px-4 py-3 font-medium">凭据</th>
                  </tr>
                </thead>
                <tbody>
                  {data.targets.map((t) => (
                    <tr key={t.id} className="border-b last:border-0 hover:bg-muted/40">
                      <td className="px-4 py-3 font-medium">{t.model}</td>
                      <td className="px-4 py-3 text-xs">{t.provider_name}</td>
                      <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{t.upstream_model}</td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{t.provider_key_hint}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </CardContent>
        </Card>
      )}
    </motion.div>
  )
}
