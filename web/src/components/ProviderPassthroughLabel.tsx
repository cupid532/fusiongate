import type { Provider } from "@/lib/types"
import { protocolMethodLabel } from "@/lib/protocol-methods"

export function ProviderPassthroughLabel({ provider }: { provider: Provider }) {
  if (provider.passthrough_supported === false) {
    return <span className="text-xs text-amber-700 dark:text-amber-400" title={provider.passthrough_reason || "该专用渠道不参与推理路由"}>不参与推理路由</span>
  }
  if (provider.passthrough_supported === true) {
    return <span className="text-xs text-muted-foreground" title="客户端协议渠道支持时原样透传，否则自动转换协议">透传优先 · {protocolMethodLabel(provider.protocol_policy, provider.protocol_preference)}</span>
  }
  return <span className="text-xs text-muted-foreground">透传支持状态未知</span>
}
