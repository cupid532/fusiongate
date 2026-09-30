import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ProviderBatchDialog } from "@/components/ProviderBatchDialog"
import type { Provider } from "@/lib/types"

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
describe("batch edits and retry", () => {
  it("retries only failed items and retains previous successes in the summary", async () => {
    const writes: string[] = []
    let secondAttempt = false
    vi.stubGlobal("fetch", vi.fn(async (path: string, options?: RequestInit) => {
      if (options?.method === "PATCH") {
        writes.push(path)
        if (path.endsWith("/2") && !secondAttempt) {
          secondAttempt = true
          return new Response(JSON.stringify({ error: { message: "Rejected" } }), { status: 400, headers: { "content-type": "application/json" } })
        }
      }
      return new Response(JSON.stringify({}), { status: 200, headers: { "content-type": "application/json" } })
    }))
    render(<QueryClientProvider client={new QueryClient()}><ProviderBatchDialog open action="priority" providers={[{ id: 1, name: "First" }, { id: 2, name: "Second" }] as Provider[]} onOpenChange={() => {}} /></QueryClientProvider>)
    fireEvent.change(screen.getByRole("spinbutton", { name: "优先级（数字越大越优先）" }), { target: { value: "4" } })
    fireEvent.click(screen.getByRole("button", { name: "确认应用" }))
    await screen.findByText("成功 1 · 失败 1 · 待核对 0 · 未执行 0")
    await waitFor(() => expect((screen.getByRole("button", { name: "仅重试失败 / 未执行项" }) as HTMLButtonElement).disabled).toBe(false))
    fireEvent.click(screen.getByRole("button", { name: "仅重试失败 / 未执行项" }))
    await screen.findByText("成功 2 · 失败 0 · 待核对 0 · 未执行 0")
    expect(writes).toEqual(["/api/admin/providers/1", "/api/admin/providers/2", "/api/admin/providers/2"])
  })
})
