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
