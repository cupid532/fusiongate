import type { IPPoolNode } from "@/lib/types"

export function EgressSelect({ id, value, onChange, nodes, disabled, inherited, invalid }: {
  id: string
  value: string
  onChange: (value: string) => void
  nodes: IPPoolNode[]
  disabled?: boolean
  inherited?: boolean
  invalid?: boolean
}) {
  const currentNode = value.startsWith("node:") ? Number(value.slice(5)) : null
  return <select id={id} value={value} onChange={(event) => onChange(event.target.value)} disabled={disabled} aria-invalid={invalid || undefined} className="h-9 w-full rounded-md border border-input bg-background px-2 text-sm disabled:opacity-50">
    <option value="" disabled>请选择出口</option>
    {inherited && <option value="inherit">继承渠道默认出口</option>}
    <option value="direct">{inherited ? "强制直连" : "本机直连"}</option>
    {currentNode && !nodes.some((node) => node.id === currentNode) && <option value={value} disabled>节点 #{currentNode}（已删除或暂未加载）</option>}
    {nodes.map((node) => <option key={node.id} value={`node:${node.id}`} disabled={!node.enabled}>{node.name} · {node.protocol} · {node.exit_ip || "出口 IP 未检测"}{!node.enabled ? " · 已停用" : node.status === "healthy" ? " · 检测通过" : node.status === "ready" ? " · 节点就绪" : " · 待核对"}</option>)}
  </select>
}
