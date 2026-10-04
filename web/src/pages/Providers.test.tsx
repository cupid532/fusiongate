import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { reorderProviderIDs } from "@/lib/provider-order"
import { Providers } from "@/pages/Providers"
import { ConfirmProvider } from "@/components/ui/confirm"
import type { Provider } from "@/lib/types"

afterEach(() => { cleanup(); vi.unstubAllGlobals(); location.hash = "" })

function showProviders() {
  const providers = [
    { id: 1, name: "First", base_url: "https://first.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 1, sort_order: 0, model_count: 2, failure_threshold: 5, health_check_enabled: true, health_check_status: "pending" },
    { id: 2, name: "Second", base_url: "https://second.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 3, sort_order: 1, model_count: 3, failure_threshold: 5, health_check_enabled: true, health_check_status: "healthy" },
  ] as Provider[]
  vi.stubGlobal("fetch", vi.fn(async (path: string) => new Response(JSON.stringify(path === "/api/admin/providers" ? providers : path === "/api/admin/routing" ? { strategy: "priority_failover" } : []), { status: 200, headers: { "content-type": "application/json" } })))
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ConfirmProvider><Providers /></ConfirmProvider></QueryClientProvider>)
}

describe("channel management workflow", () => {
  it("makes selection and batch egress directly available", async () => {
    showProviders()
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择 First" }))
    expect(screen.getByRole("button", { name: "指定出口" })).toBeTruthy()
    expect(screen.getByRole("button", { name: "设置优先级" })).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "指定出口" }))
    expect(screen.getByRole("heading", { name: "批量指定出口" })).toBeTruthy()
  })
  it("keeps hidden selections and uses indeterminate select-all", async () => {
    showProviders()
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择 First" }))
    expect((screen.getByRole("checkbox", { name: "选择当前筛选全部渠道" }) as HTMLInputElement).indeterminate).toBe(true)
    fireEvent.change(screen.getByRole("textbox", { name: "搜索渠道" }), { target: { value: "Second" } })
    expect(screen.getByText("已选 1 · 1 项不在当前筛选结果")).toBeTruthy()
    fireEvent.click(screen.getByRole("checkbox", { name: "选择当前筛选全部渠道" }))
    expect(screen.getByText("已选 2 · 1 项不在当前筛选结果")).toBeTruthy()
  })
  it("sorts a priority view without changing persisted positions", async () => {
    showProviders()
    await screen.findByRole("checkbox", { name: "选择 First" })
    fireEvent.change(screen.getByRole("combobox", { name: "列表展示顺序" }), { target: { value: "priority" } })
    await waitFor(() => expect(screen.getAllByRole("row")[1].textContent).toContain("Second"))
    expect(screen.getByText("待检活")).toBeTruthy()
  })
  it("keeps narrow channel tables readable inside a keyboard-accessible scroll region", async () => {
    showProviders()
    await screen.findByRole("checkbox", { name: "选择 First" })
    expect(screen.getByRole("region", { name: "渠道列表" }).tabIndex).toBe(0)
    const table = screen.getByRole("table")
    expect(table.classList.contains("min-w-[48rem]")).toBe(true)
    expect(table.classList.contains("whitespace-nowrap")).toBe(true)
  })

  it("starts a batch health check for several selected channels", async () => {
    // Batch mode has no manual start button, so the dialog must auto-start.
    // Without that it sat on 「正在启动检活…」 and never issued the request.
    const providers = [
      { id: 1, name: "First", base_url: "https://first.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 1, sort_order: 0, model_count: 2, failure_threshold: 5, health_check_enabled: true, health_check_status: "pending" },
      { id: 2, name: "Second", base_url: "https://second.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 3, sort_order: 1, model_count: 3, failure_threshold: 5, health_check_enabled: true, health_check_status: "healthy" },
    ] as Provider[]
    const started = {
      id: "job-1", mode: "generation", status: "completed", total: 2, completed: 2,
      healthy: 2, failed: 0, skipped: 0, created_at: "2026-10-03T00:00:00Z", can_cancel: false, results: [],
    }
    const calls: Array<{ url: string; method: string; body?: unknown }> = []
    const json = (value: unknown, status = 200) =>
      new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } })
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = (init?.method ?? "GET").toUpperCase()
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
      if (url === "/api/admin/providers") return json(providers)
      if (url === "/api/admin/health-checks" && method === "POST") return json(started, 202)
      return json([])
    }))
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ConfirmProvider><Providers /></ConfirmProvider></QueryClientProvider>)

    fireEvent.click(await screen.findByRole("checkbox", { name: "选择 First" }))
    fireEvent.click(screen.getByRole("checkbox", { name: "选择 Second" }))
    fireEvent.click(screen.getByRole("button", { name: "检活（2）" }))

    await waitFor(() => {
      const start = calls.find((call) => call.url === "/api/admin/health-checks" && call.method === "POST")
      expect(start?.body).toEqual({ provider_ids: [1, 2], model_scope: "all" })
    })
  })
})

describe("channel status grouping", () => {
  function mountMixed() {
    const providers = [
      { id: 1, name: "Live", base_url: "https://live.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 1, sort_order: 0, model_count: 1, failure_threshold: 5, health_check_enabled: true, health_check_status: "healthy" },
      { id: 2, name: "Stopped", base_url: "https://stopped.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: false, archived: false, notes: "", priority: 2, sort_order: 1, model_count: 1, failure_threshold: 5, health_check_enabled: true, health_check_status: "pending" },
      { id: 3, name: "Gone", base_url: "https://gone.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: true, notes: "", priority: 3, sort_order: 2, model_count: 1, failure_threshold: 5, health_check_enabled: true, health_check_status: "pending" },
    ] as Provider[]
    vi.stubGlobal("fetch", vi.fn(async (path: string) => new Response(JSON.stringify(path === "/api/admin/providers" ? providers : path === "/api/admin/routing" ? { strategy: "priority_failover" } : []), { status: 200, headers: { "content-type": "application/json" } })))
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ConfirmProvider><Providers /></ConfirmProvider></QueryClientProvider>)
  }

  it("opens on enabled channels and orders the groups enabled, disabled, all, archived", async () => {
    mountMixed()
    // The default view is the channels that actually serve traffic.
    expect(await screen.findByText("Live")).toBeTruthy()
    expect(screen.queryByText("Stopped")).toBeNull()
    expect(screen.queryByText("Gone")).toBeNull()
    const labels = screen.getAllByRole("button").map((button) => button.textContent ?? "").filter((text) => /^(已启用|已停用|全部|归档) \d+$/.test(text))
    expect(labels).toEqual(["已启用 1", "已停用 1", "全部 2", "归档 1"])
  })

  it("no longer offers the ambiguous attention filter", async () => {
    mountMixed()
    await screen.findByText("Live")
    expect(screen.queryByText(/需关注/)).toBeNull()
  })
})

describe("reorderProviderIDs", () => {
  it("keeps hidden providers in the complete reorder payload", () => {
    // IDs 2 and 4 represent rows omitted by search/status filters.
    expect(reorderProviderIDs([1, 2, 3, 4, 5], 1, 5)).toEqual([2, 3, 4, 1, 5])
  })

  it("moves a visible row upward without dropping hidden IDs", () => {
    expect(reorderProviderIDs([1, 2, 3, 4, 5], 5, 2)).toEqual([1, 5, 2, 3, 4])
  })

  it("does not alter the list for unknown or identical IDs", () => {
    expect(reorderProviderIDs([1, 2, 3], 9, 2)).toEqual([1, 2, 3])
    expect(reorderProviderIDs([1, 2, 3], 2, 2)).toEqual([1, 2, 3])
  })
})
