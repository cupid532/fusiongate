import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { Settings } from "./Settings"

const fetchMock = vi.fn()
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  render(<QueryClientProvider client={client}><Settings /></QueryClientProvider>)
}
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals() })

describe("routing settings", () => {
  it("does not show a fabricated default when routing cannot load", async () => {
    vi.stubGlobal("fetch", fetchMock.mockRejectedValue(new Error("offline")))
    mount()
    expect(await screen.findByRole("alert")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "优先级优先" })).toBeNull()
  })

  it("shows the strategy returned by the save response", async () => {
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PATCH") return new Response(JSON.stringify({ strategy: "adaptive" }), { headers: { "content-type": "application/json" } })
      return new Response(JSON.stringify({ strategy: "priority_failover" }), { headers: { "content-type": "application/json" } })
    }))
    mount()
    const current = await screen.findByText("优先级固定（总从最高优先级开始）")
    expect(current).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: /自适应加权/ }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith("/api/admin/routing", expect.objectContaining({ method: "PATCH" })))
    await screen.findByText("自适应加权（按延迟/失败/并发打分）")
  })
})
