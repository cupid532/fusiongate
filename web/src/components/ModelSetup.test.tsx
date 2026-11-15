import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { useModelSetup, ModelSetupFields, useSetupRequest } from "./ModelSetup"
import { ProviderDialog } from "./ProviderDialog"
import { ConfirmProvider } from "@/components/ui/confirm"

function response(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } }) }
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers() })
const connection = { type: "openai_compatible", baseURL: "https://example.com/v1", credential: "draft" }

describe("draft model discovery", () => {
  it("discards stale requests after Key changes and on cancellation", async () => {
    vi.useFakeTimers()
    const pending: Array<(value: Response) => void> = []
    const fetcher = vi.fn(() => new Promise<Response>((resolve) => pending.push(resolve)))
    vi.stubGlobal("fetch", fetcher)
    const view = renderHook(({ credential, open }) => useModelSetup(open, { ...connection, credential }), { initialProps: { credential: "first", open: true } })
    await act(async () => { vi.advanceTimersByTime(701) })
    view.rerender({ credential: "second", open: true })
    await act(async () => { vi.advanceTimersByTime(701) })
    await act(async () => { pending[1](response({ inventory: [{ model: "new" }] })) })
    expect(view.result.current.inventory.map((m) => m.model)).toEqual(["new"])
    await act(async () => { pending[0](response({ inventory: [{ model: "old" }] })) })
    expect(view.result.current.inventory.map((m) => m.model)).toEqual(["new"])
    expect(view.result.current.selected).toEqual([])
    view.rerender({ credential: "third", open: true })
    expect(view.result.current.inventory).toEqual([])
    view.rerender({ credential: "third", open: false })
    await act(async () => { vi.advanceTimersByTime(2000) })
    expect(fetcher).toHaveBeenCalledTimes(2)
  })
  it("allows manual selection after discovery failure and requires explicit configuration-only consent", async () => {
    vi.useFakeTimers()
    vi.stubGlobal("fetch", vi.fn(async () => response({ error: { message: "No models API" } }, 404)))
    const view = renderHook(() => useModelSetup(true, connection))
    await act(async () => { await vi.advanceTimersByTimeAsync(701) })
    expect(view.result.current.error).toContain("No models API")
    expect(view.result.current.ready).toBe(false)
    act(() => view.result.current.setManual("Vendor/Code"))
    expect(view.result.current.inventory[0]).toMatchObject({ model: "Vendor/Code", source: "manual" })
    act(() => view.result.current.setSelected(["Vendor/Code"]))
    expect(view.result.current.ready).toBe(true)
    act(() => view.result.current.setSkip(true))
    expect(view.result.current.setup).toEqual({ inventory: [], selected: [] })
  })
  it("selects only search matches, preserving selections outside the search", async () => {
    vi.useFakeTimers()
    vi.stubGlobal("fetch", vi.fn(async () => response({ inventory: [{ model: "alpha" }, { model: "beta" }, { model: "gamma" }] })))
    function Fields() { const state = useModelSetup(true, connection);return <><ModelSetupFields state={state} /><output>{state.selected.join(",")}</output></> }
    render(<Fields />)
    await act(async () => { await vi.advanceTimersByTimeAsync(701) })
    fireEvent.click(screen.getByLabelText("beta"))
    fireEvent.change(screen.getByLabelText("搜索待接入模型"), { target: { value: "alpha" } })
    fireEvent.click(screen.getByRole("button", { name: "选择当前结果（1）" }))
    expect(screen.getByRole("status").textContent).toBe("beta,alpha")
  })
  it("reuses idempotency IDs only for identical submissions", () => {
    const view = renderHook(() => useSetupRequest())
    const first = view.result.current({ name: "one" })
    expect(view.result.current({ name: "one" }).request_id).toBe(first.request_id)
    expect(view.result.current({ name: "two" }).request_id).not.toBe(first.request_id)
  })
})

describe("one-save channel onboarding", () => {
  function show(onClose = vi.fn()) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
    render(<QueryClientProvider client={client}><ConfirmProvider><ProviderDialog open provider={null} onOpenChange={onClose} /></ConfirmProvider></QueryClientProvider>)
    return onClose
  }
  it("retrieves before creation, preserves spelling, and submits chosen models once", async () => {
    const writes: Array<{ path: string; body: Record<string, unknown> }> = []
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
      const path = String(input)
      if (options?.body) writes.push({ path, body: JSON.parse(String(options.body)) })
      if (path.endsWith("/preview-models")) return response({ inventory: [{ model: "Vendor/Code", source: "discovered" }, { model: "Other" }] })
      return response(path === "/api/admin/providers" && options?.method === "POST" ? { id: 17 } : [])
    }))
    const close = show()
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "New" } })
    fireEvent.change(screen.getByLabelText("API 地址"), { target: { value: "https://example.com/v1" } })
    fireEvent.change(screen.getByLabelText("首张 API Key"), { target: { value: "secret" } })
    fireEvent.click(await screen.findByLabelText("Vendor/Code", {}, { timeout: 2000 }))
    expect(writes.filter((w) => w.path === "/api/admin/providers")).toHaveLength(0)
    fireEvent.click(screen.getByRole("button", { name: "保存渠道（已选 1 个模型）" }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    const saved = writes.filter((w) => w.path === "/api/admin/providers")
    expect(saved).toHaveLength(1)
    expect(saved[0].body).toMatchObject({ credential: "secret", auto_discover: false, setup: { selected: ["Vendor/Code"] } })
  })
  it("confirmation cancellation never creates a channel", async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, _options?: RequestInit) => response([]));vi.stubGlobal("fetch", fetcher)
    const close = show()
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Unsaved" } })
    fireEvent.click(screen.getByRole("button", { name: "取消" }))
    fireEvent.click(await screen.findByRole("button", { name: "放弃" }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(fetcher.mock.calls.every((args) => !(args[1] as RequestInit | undefined)?.body)).toBe(true)
  })
})
