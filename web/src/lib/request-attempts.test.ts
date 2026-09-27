import { describe, expect, it } from "vitest"
import type { RequestLedgerRow } from "@/lib/types"
import { attemptLabel, attemptsForGatewayRequest } from "./request-attempts"

function row(id: number, gateway_request_id: string, attempt: number): RequestLedgerRow {
  return {
    id, request_id: `request-${id}`, gateway_request_id, attempt, retry_reason: "", api_key_id: 0, api_key_name: "", api_key_prefix: "",
    provider_name: `provider-${id}`, provider_key_id: 0, provider_key_name: `key-${id}`, provider_key_hint: "", client_ip: "",
    created_at: "", completed_at: "", running: false, first_byte_ms: null, model: "", upstream_model: "", protocol: "", stream: false,
    success: false, status_code: 0, error_type: "", latency_ms: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0,
    reasoning_tokens: 0, total_tokens: 0, cost_micros: 0, cost_type: "", usage_reported: false, reasoning_effort: "",
  }
}

describe("request attempt sequence", () => {
  it("groups by gateway request and sorts the rows available on this page", () => {
    const pageRows = [row(4, "other", 1), row(3, "gateway", 3), row(1, "gateway", 1), row(2, "gateway", 2)]
    expect(attemptsForGatewayRequest(pageRows, "gateway").map((item) => item.attempt)).toEqual([1, 2, 3])
    expect(attemptLabel(row(2, "gateway", 2))).toBe("第 2 次：provider-2 / key-2")
  })

  it("returns only the visible attempt when earlier rows are absent and handles missing IDs", () => {
    expect(attemptsForGatewayRequest([row(3, "gateway", 3)], "gateway").map((item) => item.attempt)).toEqual([3])
    expect(attemptsForGatewayRequest([row(1, "", 1)], "")).toEqual([])
  })
})
