import type { RequestLedgerRow } from "@/lib/types"

export function attemptsForGatewayRequest(rows: RequestLedgerRow[], gatewayRequestId: string): RequestLedgerRow[] {
  if (!gatewayRequestId) return []
  return rows
    .filter((row) => row.gateway_request_id === gatewayRequestId)
    .sort((a, b) => a.attempt - b.attempt || a.id - b.id)
}

export function attemptLabel(row: RequestLedgerRow): string {
  const provider = row.provider_name || "渠道未知"
  const key = row.provider_key_name ? ` / ${row.provider_key_name}` : ""
  return `第 ${row.attempt} 次：${provider}${key}`
}
