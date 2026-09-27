import type { Provider } from "@/lib/types"

export function ProviderPassthroughLabel({ provider }: { provider: Provider }) {
  if (provider.passthrough_supported === false) {
    return <span className="text-xs text-amber-700 dark:text-amber-400" title={provider.passthrough_reason || "该专用渠道不支持纯透传"}>纯透传不支持此渠道</span>
  }
  if (provider.passthrough_supported === true) return <span className="text-xs text-muted-foreground">原样透传</span>
  return <span className="text-xs text-muted-foreground">透传支持状态未知</span>
}
