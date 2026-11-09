import { afterEach, describe, expect, it, vi } from "vitest"
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { reorderProviderIDs } from "@/lib/provider-order"
import { Providers } from "@/pages/Providers"
import { ConfirmProvider } from "@/components/ui/confirm"
import type { Provider } from "@/lib/types"

afterEach(() => { cleanup(); vi.unstubAllGlobals(); location.hash = "" })

function showProviders(options: { providers?: Provider[]; write?: (path: string, init: RequestInit) => Promise<Response> } = {}) {
  let providers = options.providers ?? [
    { id: 1, name: "First", base_url: "https://first.example/v1", website_url: "https://shop.first.example/account", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 1, sort_order: 0, model_count: 2, failure_threshold: 5, health_check_enabled: true, health_check_status: "pending" },
    { id: 2, name: "Second", base_url: "https://second.example/v1", type: "openai_compatible", auth_kind: "api_key", enabled: true, archived: false, notes: "", priority: 3, sort_order: 1, model_count: 3, failure_threshold: 5, health_check_enabled: true, health_check_status: "healthy" },
  ] as Provider[]
  const fetcher = vi.fn(async (path: string, init?: RequestInit) => {
    const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200, headers: { "content-type": "application/json" } })
    if (init?.method && init.method !== "GET") {
      if (options.write) return options.write(path, init)
      if (path === "/api/admin/providers/batch") {
        const body = JSON.parse(String(init.body)) as { provider_ids: number[]; action: string }
        if (body.action === "delete") providers = providers.filter((provider) => !body.provider_ids.includes(provider.id))
        return json({ affected: body.provider_ids.length })
      }
    }
    return json(path === "/api/admin/providers" ? providers : path === "/api/admin/routing" ? { strategy: "priority_failover" } : [])
  })
  vi.stubGlobal("fetch", fetcher)
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ConfirmProvider><Providers /></ConfirmProvider></QueryClientProvider>)
  return fetcher
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
  it("opens the merchant site from the channel name and keeps management on the button", async () => {
    showProviders()
    const merchant = await screen.findByRole("link", { name: "打开 First 商家网站" })
    expect(merchant.getAttribute("href")).toBe("https://shop.first.example/account")
    expect(merchant.getAttribute("target")).toBe("_blank")
    // A channel without a merchant URL falls back to the API host, not to management.
    expect(screen.getByRole("link", { name: "打开 Second 商家网站" }).getAttribute("href")).toBe("https://second.example")
    fireEvent.click(merchant)
    expect(screen.queryByRole("heading", { name: /管理渠道/ })).toBeNull()
    expect(screen.getByRole("button", { name: "管理 First" })).toBeTruthy()
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

describe("direct channel deletion", () => {
  it("deletes only the named row and preserves other selected channels", async () => {
    const fetcher = showProviders()
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择 First" }))
    fireEvent.click(screen.getByRole("button", { name: "删除 Second" }))
    const dialog = screen.getByRole("dialog", { name: "删除渠道「Second」？" })
    expect(dialog.textContent).toContain("渠道、Key 和模型路由将被删除")
    expect(fetcher.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false)
    fireEvent.click(within(dialog).getByRole("button", { name: "确认删除" }))
    await waitFor(() => expect(screen.queryByRole("checkbox", { name: "选择 Second" })).toBeNull())
    expect((screen.getByRole("checkbox", { name: "选择 First" }) as HTMLInputElement).checked).toBe(true)
    const writes = fetcher.mock.calls.filter(([, init]) => init?.method === "POST")
    expect(writes).toHaveLength(1)
    expect(writes[0][0]).toBe("/api/admin/providers/batch")
    expect(JSON.parse(String(writes[0][1]?.body))).toEqual({ provider_ids: [2], action: "delete" })
  })

  it("lets a channel be deleted without selecting it and cancels without writing", async () => {
    const fetcher = showProviders()
    fireEvent.click(await screen.findByRole("button", { name: "删除 First" }))
    expect(screen.queryByLabelText("渠道批量操作")).toBeNull()
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "取消" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    expect(fetcher.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false)
    expect(screen.getByRole("checkbox", { name: "选择 First" })).toBeTruthy()
  })

  it("exposes batch delete and includes hidden selections in the confirmation and request", async () => {
    const fetcher = showProviders()
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择当前筛选全部渠道" }))
    fireEvent.change(screen.getByRole("textbox", { name: "搜索渠道" }), { target: { value: "Second" } })
    fireEvent.click(screen.getByRole("button", { name: "删除选中的 2 个渠道" }))
    const dialog = screen.getByRole("dialog", { name: "删除选中的 2 个渠道？" })
    expect(dialog.textContent).toContain("First、Second")
    expect(dialog.textContent).toContain("其中 1 项不在当前筛选结果中，也会一并删除")
    fireEvent.click(within(dialog).getByRole("button", { name: "删除 2 个渠道" }))
    await waitFor(() => expect(screen.queryByLabelText("渠道批量操作")).toBeNull())
    expect(screen.queryByRole("checkbox", { name: "选择 Second" })).toBeNull()
    const writes = fetcher.mock.calls.filter(([, init]) => init?.method === "POST")
    expect(writes).toHaveLength(1)
    expect(JSON.parse(String(writes[0][1]?.body))).toEqual({ provider_ids: [1, 2], action: "delete" })
  })

  it("locks row operations while deletion is pending and prevents duplicate submission", async () => {
    let finish!: (response: Response) => void
    const fetcher = showProviders({ write: () => new Promise<Response>((resolve) => { finish = resolve }) })
    fireEvent.click(await screen.findByRole("button", { name: "删除 First" }))
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "确认删除" }))
    await waitFor(() => expect((screen.getByRole("button", { name: "删除 First" }) as HTMLButtonElement).disabled).toBe(true))
    expect((screen.getByRole("button", { name: "管理 First" }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "删除 First" }))
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(1)
    await act(async () => finish(new Response(JSON.stringify({ affected: 1 }), { headers: { "content-type": "application/json" } })))
    await waitFor(() => expect((screen.getByRole("button", { name: "删除 First" }) as HTMLButtonElement).disabled).toBe(false))
  })

  it("preserves the selection after a rejected delete and allows retry", async () => {
    showProviders({ write: async () => new Response(JSON.stringify({ error: { code: "database_error", message: "删除失败，请稍后重试" } }), { status: 500, headers: { "content-type": "application/json" } }) })
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择 First" }))
    fireEvent.click(screen.getByRole("button", { name: "删除选中的 1 个渠道" }))
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "确认删除" }))
    expect((await screen.findByRole("alert")).textContent).toContain("删除失败，请稍后重试")
    expect((screen.getByRole("checkbox", { name: "选择 First" }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole("button", { name: "删除 First" }) as HTMLButtonElement).disabled).toBe(false)
  })

  it("requires a read after a disconnected deletion and never resubmits it during verification", async () => {
    const fetcher = showProviders({ write: async () => { throw new TypeError("Failed to fetch") } })
    fireEvent.click(await screen.findByRole("button", { name: "删除 First" }))
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "确认删除" }))
    expect((await screen.findByRole("alert")).textContent).toContain("操作结果待核对")
    expect((screen.getByRole("button", { name: "删除 First" }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "读取配置核对" }))
    expect((await screen.findByRole("status")).textContent).toContain("0 / 1 项与目标一致")
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(1)
    expect((screen.getByRole("button", { name: "删除 First" }) as HTMLButtonElement).disabled).toBe(false)
  })

  it("reconciles a completed deletion after losing its response without deleting again", async () => {
    const providers = [1, 2].map((id) => ({ id, name: `Channel ${id}`, base_url: "https://example.com/v1", notes: "", enabled: true, archived: false, priority: 1, sort_order: id, model_count: 1, health_check_enabled: true } as Provider))
    const fetcher = showProviders({ providers, write: async () => { providers.splice(0, 1); throw new TypeError("Failed to fetch") } })
    fireEvent.click(await screen.findByRole("checkbox", { name: "选择当前筛选全部渠道" }))
    fireEvent.click(screen.getByRole("button", { name: "删除 Channel 1" }))
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "确认删除" }))
    await screen.findByRole("alert")
    fireEvent.click(screen.getByRole("button", { name: "读取配置核对" }))
    expect((await screen.findByRole("status")).textContent).toContain("1 / 1 项与目标一致")
    expect(screen.queryByRole("checkbox", { name: "选择 Channel 1" })).toBeNull()
    expect((screen.getByRole("checkbox", { name: "选择 Channel 2" }) as HTMLInputElement).checked).toBe(true)
    expect(fetcher.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(1)
  })

  it("does not allow batch deletion above the API limit but still allows deleting one row", async () => {
    showProviders({ providers: Array.from({ length: 201 }, (_, index) => ({ id: index + 1, name: `Channel ${index + 1}`, base_url: "https://example.com/v1", notes: "", enabled: true, archived: false, priority: 1, sort_order: index, model_count: 1, health_check_enabled: true } as Provider)) })
    fireEvent.click(await screen.findByLabelText("选择当前筛选全部渠道"))
    expect((screen.getByLabelText("删除选中的 201 个渠道") as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByLabelText("删除 Channel 1") as HTMLButtonElement).disabled).toBe(false)
  }, 30_000)

  it("offers an immediate way to reset filters when the search has no results", async () => {
    showProviders()
    await screen.findByRole("checkbox", { name: "选择 First" })
    fireEvent.change(screen.getByRole("textbox", { name: "搜索渠道" }), { target: { value: "missing" } })
    fireEvent.click(screen.getByRole("button", { name: "重置筛选" }))
    expect(screen.getByRole("checkbox", { name: "选择 First" })).toBeTruthy()
    expect((screen.getByRole("textbox", { name: "搜索渠道" }) as HTMLInputElement).value).toBe("")
    expect(screen.getByRole("button", { name: "全部 2" }).getAttribute("aria-pressed")).toBe("true")
  })

  it("can reset to the archive when there are no active channels", async () => {
    showProviders({ providers: [{ id: 1, name: "Archived", base_url: "https://example.com/v1", notes: "", enabled: false, archived: true, priority: 1, sort_order: 1, model_count: 1 } as Provider] })
    fireEvent.click(await screen.findByRole("button", { name: "重置筛选" }))
    expect(screen.getByRole("button", { name: "归档 1" }).getAttribute("aria-pressed")).toBe("true")
    expect(screen.getByRole("button", { name: "删除 Archived" })).toBeTruthy()
  })

  it("shows switch failures inline instead of leaving the operation without feedback", async () => {
    showProviders({ write: async () => new Response(JSON.stringify({ error: { message: "保存开关失败" } }), { status: 500, headers: { "content-type": "application/json" } }) })
    fireEvent.click(await screen.findByRole("switch", { name: "First 开关" }))
    expect((await screen.findByRole("alert")).textContent).toContain("保存开关失败")
    expect(screen.getByRole("switch", { name: "First 开关" }).getAttribute("aria-checked")).toBe("true")
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
