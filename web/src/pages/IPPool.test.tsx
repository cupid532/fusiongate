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

describe("IP pool deletion confirmation", () => {
  it.each([
    { mode: "single", title: "删除节点「JP-TYO」？", description: "删除后，绑定此节点的渠道和独立绑定此节点的 API Key 会自动改为直连，可能影响连通性。此操作不可恢复。", confirmLabel: "删除", expectedDeletes: ["/api/admin/ip-pool/2"] },
    { mode: "batch", title: "删除选中的 2 个节点？", description: "US-ORD、JP-TYO。删除后，绑定这些节点的渠道和独立绑定这些节点的 API Key 会自动改为直连，可能影响连通性。此操作不可恢复。", confirmLabel: "删除 2 个", expectedDeletes: ["/api/admin/ip-pool/1", "/api/admin/ip-pool/2"] },
  ])("warns about direct connections and requires confirmation for $mode deletion", async ({ mode, title, description, confirmLabel, expectedDeletes }) => {
    const deletes: string[] = []
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "DELETE") {
        deletes.push(String(input))
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

    await screen.findByText("JP-TYO")
    if (mode === "batch") {
      fireEvent.click(screen.getByRole("button", { name: "多选" }))
      fireEvent.click(screen.getByRole("checkbox", { name: "选择 US-ORD" }))
      fireEvent.click(screen.getByRole("checkbox", { name: "选择 JP-TYO" }))
    }
    const openConfirmation = () => fireEvent.click(screen.getByRole("button", { name: mode === "batch" ? "删除" : "删除 JP-TYO" }))
    openConfirmation()
    await screen.findByRole("dialog", { name: title })
    expect(screen.getByText(description)).toBeTruthy()
    expect(deletes).toEqual([])

    fireEvent.click(screen.getByRole("button", { name: "取消" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    expect(deletes).toEqual([])

    openConfirmation()
    await screen.findByRole("dialog", { name: title })
    fireEvent.click(screen.getByRole("button", { name: confirmLabel }))
    await waitFor(() => expect(deletes).toEqual(expectedDeletes))
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    if (mode === "batch") {
      await waitFor(() => expect(screen.queryByRole("button", { name: "停用" })).toBeNull())
    }
  })
})

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
