import type { Provider } from "@/lib/types"
import {
  PROTOCOL_METHOD_LABELS,
  PROTOCOL_METHODS,
  protocolMethod,
  protocolMethodLabel,
  supportsProtocolMethod,
  type ProtocolMethod,
} from "@/lib/protocol-methods"

export function ProtocolMethodSelect({
  type, value, onChange, id, disabled = false,
}: {
  type: string
  value: ProtocolMethod
  onChange: (method: ProtocolMethod) => void
  id?: string
  disabled?: boolean
}) {
  return (
    <select
      id={id}
      aria-label="接口方式"
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value as ProtocolMethod)}
      className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm disabled:opacity-60"
    >
      {PROTOCOL_METHODS.map((method) => (
        <option key={method} value={method} disabled={!supportsProtocolMethod(type, method)}>
          {PROTOCOL_METHOD_LABELS[method]}{!supportsProtocolMethod(type, method) ? "（该渠道不可用）" : ""}
        </option>
      ))}
    </select>
  )
}

export function ProviderProtocolLabel({ provider }: { provider: Provider }) {
  const dedicated = !PROTOCOL_METHODS.slice(1).some((method) => supportsProtocolMethod(provider.type, method))
  return (
    <span className="text-xs text-muted-foreground" title={dedicated ? "使用该渠道的专用认证和上游接口；其他接口方式不可用" : "渠道访问上游时采用的接口方式"}>
      {protocolMethodLabel(provider.protocol_policy, provider.protocol_preference)}
      {dedicated && protocolMethod(provider.protocol_policy, provider.protocol_preference) === "auto" ? "（专用接口）" : ""}
    </span>
  )
}
