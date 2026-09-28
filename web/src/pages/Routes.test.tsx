import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { Routes } from "./Routes"

vi.mock("@/components/ui/confirm", () => ({ useConfirm: () => vi.fn(async () => false), useConfirmDelete: () => vi.fn(async () => false) }))
vi.unmock("@/components/ModelAliasManager")
vi.mock("@/components/PricingDialog", () => ({ PricingDialog: () => null }))
vi.mock("@/components/RouteDialog", () => ({ RouteDialog: () => null }))

const routeRows = [0, 1, 2, undefined].map((eligible, i) => ({
  id: i + 1, provider_id: i + 1, public_name: `model-${i}`, upstream_model: `upstream-${i}`, capabilities: "chat", enabled: true, priority: 0,
  input_price_micros: 0, cached_price_micros: 0, output_price_micros: 0, long_context_threshold: 0, long_input_price_micros: 0, long_cached_price_micros: 0, long_output_price_micros: 0,
  provider_enabled: true, provider_archived: false, provider_priority: 0, provider_sort_order: i, sort_order: i, provider_latency_ms: 0, provider_first_byte_ms: 0, provider_failures: 0, provider_inflight: 0, health_score: 0, health_check_status: "", health_check_latency_ms: 0, health_check_first_byte_ms: 0,
  ...(eligible === undefined ? {} : { eligible_provider_count: eligible }),
}))

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } } })
  render(<QueryClientProvider client={client}><Routes /></QueryClientProvider>)
}

function response(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } })
}

afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals() })

describe("Routes passthrough routing status", () => {
  it("shows total candidates and derived backup counts without fabricating missing values", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://localhost").pathname
      if (path === "/api/admin/routes") return response(routeRows)
      if (path === "/api/admin/routing") return response({ strategy: "priority_failover" })
      if (path === "/api/admin/model-aliases") return response([{ alias: "legacy-model", target_model: "model-2", enabled: true, created_at: "", updated_at: "" }])
      if (path === "/api/admin/pricing") return response({ status: {}, interval: "0s", sources: [] })
      return response({})
    }))
    mount()
    expect(await screen.findByText("0 个候选 · 0 个备用")).toBeTruthy()
    expect(screen.getByText("1 个候选 · 0 个备用")).toBeTruthy()
    expect(screen.getByText("2 个候选 · 1 个备用")).toBeTruthy()
    expect(screen.getByText("legacy-model")).toBeTruthy()
    expect(screen.getByText("不改写模型")).toBeTruthy()
    expect(screen.getByText("可用候选数未知")).toBeTruthy()
    expect(screen.getByText("不改写模型")).toBeTruthy()
    expect(screen.getByText("可用候选数未知")).toBeTruthy()
    expect(screen.getByText("单候选：无备用渠道，无法故障转移")).toBeTruthy()
    // The ordering is fixed, so the page states it instead of offering a choice.
    // Other selects on this page (fault-transfer groups) are unrelated, so scope
    // the assertion to the ordering card rather than the whole document.
    expect(screen.getByText("候选顺序")).toBeTruthy()
    expect(screen.getByText("优先级固定（按渠道优先级从高到低）")).toBeTruthy()
    expect(screen.queryByRole("combobox", { name: "全局起始渠道选择策略" })).toBeNull()
  })

  it("states the ordering is unknown on query error, offers retry, and recovers", async () => {
    let queryFails = true
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://localhost").pathname
      if (path === "/api/admin/routes") return response([])
      if (path === "/api/admin/model-aliases") return response([])
      if (path === "/api/admin/pricing") return response({ status: {}, interval: "0s", sources: [] })
      if (path === "/api/admin/routing" && queryFails) return response({ error: { message: "offline", code: "error" } }, 500)
      return response({ strategy: "priority_failover" })
    }))
    mount()
    expect(await screen.findByText("路由设置读取失败")).toBeTruthy()
    // Nothing may claim the order is known while the settings are unreadable.
    expect(screen.queryByText("优先级固定（按渠道优先级从高到低）")).toBeNull()

    queryFails = false
    fireEvent.click(screen.getByRole("button", { name: "重试" }))
    expect(await screen.findByText("优先级固定（按渠道优先级从高到低）")).toBeTruthy()
    expect(screen.queryByText("路由设置读取失败")).toBeNull()
  })
})
