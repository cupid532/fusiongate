import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import type { Route } from "@/lib/types"
import { RouteDialog } from "./RouteDialog"
import { ModelGroupRenameDialog } from "./ModelGroupRenameDialog"

const route = { id: 9, provider_id: 7, public_name: "coding", upstream_model: "Original", capabilities: "chat,stream", priority: 0, enabled: true } as Route
function response(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } }) }
function mount(content: React.ReactNode) { const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } }); return render(<QueryClientProvider client={client}>{content}</QueryClientProvider>) }
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.clearAllMocks() })
function setup() {
  const requests: Array<{ path: string; method: string; body: Record<string, unknown> }> = []
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
    const path = String(input)
    if (options?.body) requests.push({ path, method: options.method || "GET", body: JSON.parse(String(options.body)) })
    if (path === "/api/admin/providers") return response([{ id: 7, name: "渠道 A", type: "openai_compatible", enabled: true, archived: false }])
    if (path === "/api/admin/routes" && !options?.body) return response([route])
    if (path.includes("/keys")) return response([{ id: 1, models: [{ model: "Original", enabled: true }] }])
    if (path === "/api/admin/routes/preview") return response({ supported_key_count: 0, available_key_count: 0, provider_enabled: true, auth_kind: "api_key", warning: "没有 Key 支持此上游模型" })
    return response({ ok: true })
  }))
  return requests
}

describe("route model management", () => {
  it("edits the complete route preserving upstream case and reports unsupported models", async () => {
    const requests = setup(); const close = vi.fn()
    mount(<RouteDialog open route={route} onOpenChange={close} />)
    expect(await screen.findByDisplayValue("Original")).toBeTruthy()
    fireEvent.change(screen.getByLabelText("上游模型名"), { target: { value: "Vendor/Code-V2" } })
    fireEvent.change(screen.getByLabelText("能力"), { target: { value: "chat,stream,tools" } })
    expect(await screen.findByText("0 张 Key 支持 · 0 张当前可调度（不等于检活通过）")).toBeTruthy()
    expect(screen.getByRole("link", { name: "去渠道模型管理添加到指定 Key" })).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "保存路由" }))
    await waitFor(() => expect(requests.find((request) => request.path === "/api/admin/routes/9")).toEqual({ path: "/api/admin/routes/9", method: "PATCH", body: { public_name: "coding", upstream_model: "Vendor/Code-V2", capabilities: "chat,stream,tools", priority: 0, enabled: true } }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
  })
  it("does not allow new model creation to silently join an existing group", async () => {
    setup(); mount(<RouteDialog open newGroup onOpenChange={() => {}} />)
    await screen.findByRole("option", { name: "渠道 A · openai_compatible" })
    fireEvent.change(screen.getByLabelText("请求模型名"), { target: { value: "CODING" } })
    fireEvent.change(screen.getByLabelText("上游模型名"), { target: { value: "Anything" } })
    expect(screen.getByText("请求模型已存在，请改用“添加渠道成员”。")).toBeTruthy()
    expect((screen.getByRole("button", { name: "创建路由" }) as HTMLButtonElement).disabled).toBe(true)
  })
  it("renames the whole group and keeps the old alias by default", async () => {
    const requests = setup(); const close = vi.fn(); mount(<ModelGroupRenameDialog model="coding" onClose={close} />)
    fireEvent.change(screen.getByLabelText("新的请求模型名"), { target: { value: "New-Coding" } })
    fireEvent.click(screen.getByRole("button", { name: "确认重命名整个模型组" }))
    await waitFor(() => expect(requests.find((request) => request.path === "/api/admin/model-groups/rename")?.body).toEqual({ old_name: "coding", new_name: "New-Coding", keep_old_alias: true }))
    await waitFor(() => expect(close).toHaveBeenCalled())
  })
})
