import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { IPPool } from "./IPPool"
import { ConfirmProvider } from "@/components/ui/confirm"

vi.mock("@/lib/notify", () => ({ notify: vi.fn(), notifySuccess: vi.fn(), reportUnauthorized: vi.fn() }))

const nodes = [
  { id: 1, name: "US-ORD", protocol: "vless", server: "a.example", enabled: true, status: "healthy", last_latency_ms: 10, last_checked_at: "", exit_ip: "", provider_count: 1, last_error: "" },
  { id: 2, name: "JP-TYO", protocol: "ss", server: "b.example", enabled: false, status: "ready", last_latency_ms: 0, last_checked_at: "", exit_ip: "", provider_count: 0, last_error: "" },
]

function json(value: unknown) {
  return new Response(JSON.stringify(value), { headers: { "content-type": "application/json" } })
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe("IP pool batch operations", () => {
  it("blocks a second batch while the first one is still running", async () => {
    // 停用 was the one batch button without the busy guard: clicking it while a
    // batch 启用 was in flight started a second sequential run over the same
    // nodes, whose interleaved PATCHes fought over the final state.
    let release: (() => void) | undefined
    const blocked = new Promise<void>((resolve) => { release = resolve })
    const patches: string[] = []
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if ((init?.method ?? "GET") === "PATCH") {
        patches.push(url)
        await blocked
        return json({})
      }
      return json(nodes)
    }))
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ConfirmProvider>
          <IPPool />
        </ConfirmProvider>
      </QueryClientProvider>,
    )

    await screen.findByText("US-ORD")
    fireEvent.click(screen.getByRole("button", { name: "多选" }))
    fireEvent.click(screen.getByRole("checkbox", { name: "选择 US-ORD" }))
    fireEvent.click(screen.getByRole("checkbox", { name: "选择 JP-TYO" }))

    fireEvent.click(screen.getByRole("button", { name: "启用" }))

    await waitFor(() => expect(patches).toHaveLength(1))
    for (const label of ["启用", "停用", "删除"]) {
      expect((screen.getByRole("button", { name: label }) as HTMLButtonElement).disabled).toBe(true)
    }

    release?.()
    // Both nodes succeeded, so the selection clears and the batch bar closes.
    await waitFor(() => expect(screen.queryByRole("button", { name: "停用" })).toBeNull())
  })
})
