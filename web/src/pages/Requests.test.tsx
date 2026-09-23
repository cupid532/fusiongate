import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { apiDownload } from "@/lib/api"
import { exactTimeRange } from "@/lib/exact-time-range"
import { notifySuccess } from "@/lib/notify"
import type { RequestLedgerPayload, RequestLedgerRow } from "@/lib/types"
import { Requests } from "./Requests"

vi.mock("@formkit/auto-animate/react", () => ({ useAutoAnimate: () => [undefined] }))
vi.mock("@/components/ui/confirm", () => ({ useConfirm: () => vi.fn() }))
vi.mock("@/lib/notify", () => ({ notifySuccess: vi.fn(), reportUnauthorized: vi.fn() }))
vi.mock("@/lib/api", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/api")>(),
  apiDownload: vi.fn(async () => new Blob(["csv"])),
  saveBlob: vi.fn(),
}))

function row(id: number): RequestLedgerRow {
  return {
    id, model: `model-${id}`, request_id: `request-${id}`, gateway_request_id: `gateway-${id}`,
    attempt: 1, retry_reason: "", api_key_id: 7, api_key_name: "archive key", api_key_prefix: "fg-test",
    provider_name: "provider", provider_key_id: 1, provider_key_name: "", provider_key_hint: "",
    client_ip: "127.0.0.1", created_at: "2024-02-29T10:00:00Z", completed_at: "2024-02-29T10:00:01Z",
    running: false, first_byte_ms: 10, upstream_model: "", protocol: "openai", stream: false,
    success: true, status_code: 200, error_type: "", latency_ms: 100, input_tokens: 10,
    output_tokens: 5, cached_tokens: 0, reasoning_tokens: 0, total_tokens: 15,
    cost_micros: 1, cost_type: "", usage_reported: true, reasoning_effort: "",
  }
}

function page(ids: number[], total = 6): RequestLedgerPayload {
  return {
    items: ids.map(row), count: ids.length, total, limit: 100, truncated: ids.length < total,
    totals: { requests: total, success: total, failed: 0, running: 0, input_tokens: 10, output_tokens: 5, cached_tokens: 0, total_tokens: 15, cost_micros: 1 },
    server_now: "2024-02-29T10:01:00Z",
  }
}

function deferredPage() {
  let resolve!: (value: RequestLedgerPayload) => void
  const promise = new Promise<RequestLedgerPayload>((done) => { resolve = done })
  return { promise, resolve }
}

const list = vi.fn<(params: URLSearchParams) => RequestLedgerPayload | Promise<RequestLedgerPayload>>()
const clients: QueryClient[] = []

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } } })
  clients.push(client)
  render(<QueryClientProvider client={client}><Requests /></QueryClientProvider>)
  return client
}

function lastParams() {
  return list.mock.calls.at(-1)![0]
}

function filters(params: URLSearchParams) {
  const copy = new URLSearchParams(params)
  copy.delete("limit")
  copy.delete("before")
  return Object.fromEntries(copy)
}

beforeEach(() => {
  vi.clearAllMocks()
  list.mockReset().mockReturnValue(page([5, 4]))
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://localhost")
    const data = url.pathname === "/api/admin/requests" ? await list(url.searchParams)
      : url.pathname === "/api/admin/providers" ? [{ id: 1, name: "provider" }]
      : url.pathname === "/api/admin/keys" ? [{ id: 7, name: "archive key", prefix: "fg-test" }]
      : { rows: 6, max_mb: 100, used_mb: 1, est_bytes: 1024, capped: false }
    return new Response(JSON.stringify(data), { headers: { "Content-Type": "application/json" } })
  }))
})

afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe("Requests filters", () => {
  it("blocks invalid exact times, hides cached rows and discards a pending page", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] })
    const client = mount()
    await screen.findByText("model-5")
    fireEvent.change(screen.getByLabelText("精确时间粒度"), { target: { value: "year" } })
    await screen.findByText("model-5")
    const pending = deferredPage()
    list.mockImplementation((params) => params.has("before") ? pending.promise : page([5, 4]))
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    expect(lastParams().get("before")).toBe("4")

    fireEvent.change(screen.getByLabelText("精确时间"), { target: { value: "99" } })
    expect(screen.getByRole("alert").textContent).toContain("精确时间无效")
    expect(screen.getByLabelText("精确时间").getAttribute("aria-invalid")).toBe("true")
    expect(screen.queryByRole("table")).toBeNull()
    expect(screen.queryByText("model-5")).toBeNull()
    expect(screen.queryByRole("button", { name: /加载更多/ })).toBeNull()
    const refresh = screen.getByRole<HTMLButtonElement>("button", { name: "刷新" })
    const exportButton = screen.getByRole<HTMLButtonElement>("button", { name: "导出" })
    expect(refresh.disabled).toBe(true)
    expect(exportButton.disabled).toBe(true)
    const callCount = list.mock.calls.length
    fireEvent.click(refresh)
    fireEvent.click(exportButton)
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["requests"] })
      pending.resolve(page([3, 2]))
      await vi.advanceTimersByTimeAsync(6000)
    })
    expect(list).toHaveBeenCalledTimes(callCount)
    expect(apiDownload).not.toHaveBeenCalled()
    expect(notifySuccess).not.toHaveBeenCalled()
    expect(screen.queryByText("model-3")).toBeNull()
    expect(screen.queryByRole("table")).toBeNull()

    fireEvent.change(screen.getByLabelText("精确时间"), { target: { value: "2024" } })
    await screen.findByRole("table")
    expect(lastParams().get("from")).toBe(exactTimeRange("year", "2024")!.from)
  })

  it("sends arbitrary historical models and every filter to both list and export", async () => {
    list.mockImplementation((params) => page(params.has("model") ? [20] : [5, 4]))
    const client = mount()
    await screen.findByText("model-5")
    fireEvent.change(screen.getByLabelText("按模型筛选请求"), { target: { value: "retired/model-v1" } })
    fireEvent.change(screen.getByLabelText("按渠道筛选请求"), { target: { value: "1" } })
    fireEvent.change(screen.getByLabelText("按访问秘钥筛选请求"), { target: { value: "7" } })
    fireEvent.click(screen.getByRole("button", { name: "失败" }))
    fireEvent.change(screen.getByLabelText("搜索请求账本"), { target: { value: " archive " } })
    fireEvent.change(screen.getByLabelText("精确时间粒度"), { target: { value: "minute" } })
    fireEvent.change(screen.getByLabelText("精确时间"), { target: { value: "2024-02-29T10:15" } })
    const expected = {
      model: "retired/model-v1", provider_id: "1", api_key_id: "7", status: "failed", q: "archive",
      ...exactTimeRange("minute", "2024-02-29T10:15"),
    }
    await waitFor(() => expect(filters(lastParams())).toEqual(expected))
    // Render server results without a second, client-only model filter.
    await screen.findByText("model-20")
    expect(client.getQueryCache().findAll({ queryKey: ["requests"] }).some((query) => JSON.stringify(query.queryKey).includes("model=retired%2Fmodel-v1"))).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "导出" }))
    await waitFor(() => expect(apiDownload).toHaveBeenCalledTimes(1))
    expect(Object.fromEntries(new URL(vi.mocked(apiDownload).mock.calls[0][0], "http://localhost").searchParams)).toEqual(expected)

    fireEvent.change(screen.getByLabelText("按模型筛选请求"), { target: { value: "another-retired-model" } })
    await waitFor(() => expect(lastParams().get("model")).toBe("another-retired-model"))
  })

  it("keeps quick-range bounds identical after time passes, filters change and pages load", async () => {
    const now = vi.spyOn(Date, "now").mockReturnValue(Date.parse("2024-02-29T12:00:00Z"))
    list.mockImplementation((params) => page(params.has("before") ? [3, 2] : [5, 4]))
    mount()
    await screen.findByText("model-5")
    fireEvent.change(screen.getByLabelText("按时间范围筛选请求"), { target: { value: "1h" } })
    await screen.findByText("model-5")
    const from = "2024-02-29T11:00:00.000Z"
    expect(lastParams().get("from")).toBe(from)
    now.mockReturnValue(Date.parse("2024-02-29T12:05:00Z"))
    fireEvent.change(screen.getByLabelText("按模型筛选请求"), { target: { value: "history" } })
    await screen.findByText("model-5")
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    await screen.findByText("model-3")
    expect(filters(lastParams())).toEqual({ model: "history", from })
    fireEvent.click(screen.getByRole("button", { name: "导出" }))
    await waitFor(() => expect(apiDownload).toHaveBeenCalledTimes(1))
    expect(Object.fromEntries(new URL(vi.mocked(apiDownload).mock.calls[0][0], "http://localhost").searchParams)).toEqual({ model: "history", from })
    expect(screen.getByText(/浏览器本地时区/).textContent).toContain(`from=${from}；until=不限`)
    fireEvent.click(screen.getByRole("button", { name: "刷新" }))
    await waitFor(() => expect(lastParams().has("before")).toBe(false))
    expect(filters(lastParams())).toEqual({ model: "history", from })
  })

  it("normalizes the hour control to :00 and shows its actual bounds and local zone", async () => {
    mount()
    await screen.findByText("model-5")
    fireEvent.change(screen.getByLabelText("精确时间粒度"), { target: { value: "hour" } })
    fireEvent.change(screen.getByLabelText("精确时间"), { target: { value: "2024-02-29T12:37" } })
    expect(screen.getByLabelText<HTMLInputElement>("精确时间").value).toBe("2024-02-29T12:00")
    const range = exactTimeRange("hour", "2024-02-29T12:00")!
    await waitFor(() => expect(filters(lastParams())).toEqual(range))
    const hint = screen.getByText(/浏览器本地时区/).textContent
    expect(hint).toContain(Intl.DateTimeFormat().resolvedOptions().timeZone)
    expect(hint).toContain(`from=${range.from}；until=${range.until}`)
  })
})

describe("Requests pagination", () => {
  it("deduplicates IDs and drops old pages/responses when a refreshed first page shifts", async () => {
    let first = page([8, 7], 10)
    const oldPending = deferredPage()
    const newPending = deferredPage()
    list.mockImplementation((params) => {
      if (params.get("before") === "7") return page([7, 6, 6, 5], 10)
      if (params.get("before") === "5") return oldPending.promise
      if (params.get("before") === "9") return newPending.promise
      return first
    })
    const client = mount()
    await screen.findByText("model-8")
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    await screen.findByText("model-5")
    expect(screen.getAllByText("model-6")).toHaveLength(1)
    expect(document.querySelectorAll("tbody > tr")).toHaveLength(4)

    // Refreshes with unchanged IDs keep the cursor chain intact.
    first = { ...first, server_now: "2024-02-29T10:02:00Z" }
    await act(async () => { await client.refetchQueries({ queryKey: ["requests"] }) })
    expect(screen.getByText("model-5")).toBeDefined()
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    expect(lastParams().get("before")).toBe("5")

    // Same path as polling: refresh only the query, without clicking manual refresh.
    first = page([10, 9], 10)
    await act(async () => { await client.refetchQueries({ queryKey: ["requests"] }) })
    await screen.findByText("model-10")
    expect(screen.queryByText("model-5")).toBeNull()
    expect(screen.queryByText("model-8")).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    expect(lastParams().get("before")).toBe("9")
    await act(async () => { oldPending.resolve(page([4, 3], 10)) })
    expect(screen.queryByText("model-4")).toBeNull()
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "加载中…" }).disabled).toBe(true)
    await act(async () => { newPending.resolve(page([9, 8, 8, 7], 10)) })
    await screen.findByText("model-8")
    expect(screen.getAllByText("model-8")).toHaveLength(1)
    expect(document.querySelectorAll("tbody > tr")).toHaveLength(4)
  })

  it("ignores an old filter's pending page even if the new first page has identical IDs", async () => {
    const oldPending = deferredPage()
    list.mockImplementation((params) => params.has("before") ? oldPending.promise : page([5, 4]))
    mount()
    await screen.findByText("model-5")
    fireEvent.click(screen.getByRole("button", { name: /加载更多/ }))
    fireEvent.change(screen.getByLabelText("按模型筛选请求"), { target: { value: "history" } })
    await screen.findByText("model-5")
    expect(lastParams().get("model")).toBe("history")
    await act(async () => { oldPending.resolve(page([3, 2])) })
    expect(screen.queryByText("model-3")).toBeNull()
    expect(document.querySelectorAll("tbody > tr")).toHaveLength(2)
    expect(screen.getByRole<HTMLButtonElement>("button", { name: /加载更多/ }).disabled).toBe(false)
  })
})
