import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { Settings } from "./Settings"

const fetchMock = vi.fn()
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  render(<QueryClientProvider client={client}><Settings /></QueryClientProvider>)
}
afterEach(() => { cleanup(); history.replaceState(null, "", "#settings"); vi.resetAllMocks(); vi.unstubAllGlobals() })

const settings = {
  strategy: "priority_failover",
  channel_attempts: 3,
  max_attempts: 9,
  channel_window_seconds: 30,
  failover_window_seconds: 120,
  session_idle_days: 7,
  session_max_days: 30,
  session_capacity: 10000,
}

function json(value: unknown) {
  return new Response(JSON.stringify(value), { headers: { "content-type": "application/json" } })
}

describe("field compatibility settings", () => {
  it("loads the audit only after its settings tab is opened", async () => {
    history.replaceState(null, "", "#settings")
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async (input: RequestInfo | URL) =>
      json(String(input) === "/api/admin/capabilities" ? { events: [], limit: 512 } : settings)))
    mount()
    await screen.findByLabelText("单渠道重试次数")
    expect(fetchMock.mock.calls.some(([url]) => url === "/api/admin/capabilities")).toBe(false)
    fireEvent.click(screen.getByRole("button", { name: "字段兼容" }))
    await screen.findByText("还没有字段决策记录")
    expect(fetchMock.mock.calls.some(([url]) => url === "/api/admin/capabilities")).toBe(true)
    expect(screen.queryByLabelText("单渠道重试次数")).toBeNull()
  })

  it.each(["#capabilities", "#settings?tab=capabilities"])("opens %s while settings is already mounted", async (hash) => {
    history.replaceState(null, "", "#settings")
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async (input: RequestInfo | URL) =>
      json(String(input) === "/api/admin/capabilities" ? { events: [], limit: 512 } : settings)))
    mount()
    await screen.findByLabelText("单渠道重试次数")
    history.replaceState(null, "", hash)
    fireEvent(window, new HashChangeEvent("hashchange"))
    await screen.findByText("还没有字段决策记录")
    expect(screen.queryByLabelText("单渠道重试次数")).toBeNull()
  })

  it("opens the audit from a settings deep link", async () => {
    history.replaceState(null, "", "#settings?tab=capabilities")
    vi.stubGlobal("fetch", fetchMock.mockResolvedValue(json({ events: [], limit: 512 })))
    mount()
    await screen.findByText("还没有字段决策记录")
    expect(fetchMock.mock.calls.some(([url]) => url === "/api/admin/routing")).toBe(false)
  })
})

describe("routing settings", () => {
  it("does not show fabricated parameters when routing cannot load", async () => {
    vi.stubGlobal("fetch", fetchMock.mockRejectedValue(new Error("offline")))
    mount()
    const alert = await screen.findByRole("alert")
    expect(alert.textContent).toContain("路由参数读取失败")
    // Seven inputs with invented values would look exactly like real defaults.
    expect(screen.queryByLabelText("单渠道重试次数")).toBeNull()
    expect(screen.queryByLabelText("会话容量")).toBeNull()
    expect(screen.queryByRole("button", { name: "保存路由参数" })).toBeNull()
    // The single strategy is a property of the build, so stating it is not a
    // fabricated default; offering a choice of strategies would be.
    expect(screen.queryByRole("combobox")).toBeNull()
    expect(screen.queryByRole("button", { name: /自适应加权/ })).toBeNull()
  })

  it("names the one strategy and offers no selector", async () => {
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async () => json(settings)))
    mount()
    const line = await screen.findByText((_, node) =>
      node?.tagName === "P" && (node.textContent ?? "").includes("这是唯一的策略，无法切换。"))
    expect(line.textContent).toContain("优先级固定（按渠道优先级从高到低）")
    expect(screen.queryByRole("combobox")).toBeNull()
    expect(screen.queryByRole("button", { name: /自适应加权/ })).toBeNull()
  })

  it("seeds the retry parameters from the server and PATCHes the whole set", async () => {
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PATCH") return json({ ...settings, session_capacity: 500 })
      return json(settings)
    }))
    mount()

    const attempts = await screen.findByLabelText<HTMLInputElement>("单渠道重试次数")
    expect(attempts.value).toBe("3")
    expect(screen.getByLabelText<HTMLInputElement>("最大总尝试次数").value).toBe("9")
    expect(screen.getByLabelText<HTMLInputElement>("会话容量").value).toBe("10000")

    fireEvent.change(screen.getByLabelText("会话容量"), { target: { value: "500" } })
    fireEvent.click(screen.getByRole("button", { name: "保存路由参数" }))

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith("/api/admin/routing", expect.objectContaining({ method: "PATCH" })))
    const patch = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === "PATCH")!
    // The endpoint validates the parameters as a set, so every key travels
    // together even though only one field changed.
    expect(JSON.parse(String((patch[1] as RequestInit).body))).toEqual({ ...settings, session_capacity: 500 })
  })

  it("surfaces the server message when a parameter is rejected", async () => {
    vi.stubGlobal("fetch", fetchMock.mockImplementation(async (_input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PATCH") {
        return new Response(JSON.stringify({ error: { message: "session_capacity out of range", code: "invalid_settings" } }), { status: 400, headers: { "content-type": "application/json" } })
      }
      return json(settings)
    }))
    mount()
    await screen.findByLabelText("单渠道重试次数")
    fireEvent.change(screen.getByLabelText("会话容量"), { target: { value: "0" } })
    fireEvent.click(screen.getByRole("button", { name: "保存路由参数" }))
    const alert = await screen.findByRole("alert")
    expect(alert.textContent).toContain("session_capacity out of range")
  })
})
