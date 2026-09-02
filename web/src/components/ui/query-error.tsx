import { AlertTriangle, RefreshCw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * Shown in place of a table or list when its query failed.
 *
 * Every data page in this console defaults its query result to an empty array
 * (`const { data: providers = [] } = useQuery(...)`). That is a reasonable
 * default, but it meant a failed request fell through to the page's ordinary
 * "nothing here" copy — "没有符合条件的渠道", "当前筛选范围还没有请求" — so a
 * gateway that was unreachable looked exactly like a gateway with no data. The
 * distinction matters a lot when the thing you are looking at is a list of
 * credentials, so failure now says so and offers a retry.
 */
export function QueryError({
  title = "数据加载失败",
  error,
  onRetry,
  retrying,
  className,
}: {
  title?: string
  error?: unknown
  onRetry?: () => void
  retrying?: boolean
  className?: string
}) {
  const message = error instanceof Error && error.message ? error.message : "无法从网关获取数据"
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center rounded-xl border border-dashed border-destructive/40 bg-destructive/5 p-10 text-center",
        className
      )}
    >
      <div className="grid h-11 w-11 place-items-center rounded-full bg-destructive/10 text-destructive">
        <AlertTriangle className="h-5 w-5" aria-hidden="true" />
      </div>
      <div className="mt-4 text-sm font-semibold">{title}</div>
      <div className="mt-1 max-w-md break-words text-xs text-muted-foreground">{message}</div>
      {onRetry && (
        <Button variant="outline" size="sm" className="mt-4" onClick={onRetry} disabled={retrying}>
          <RefreshCw className={cn("h-3.5 w-3.5", retrying && "animate-spin")} />
          重试
        </Button>
      )}
    </div>
  )
}
