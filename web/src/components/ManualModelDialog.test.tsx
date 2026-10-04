import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ManualModelDialog } from "@/components/ManualModelDialog"
import { parseManualModels } from "@/lib/manual-models"
import type { ProviderKey, ProviderKeyModel } from "@/lib/types"

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
const keys = [
  { id: 1, name: "Primary", enabled: true, models: [] },
  { id: 2, name: "Backup", enabled: false, models: [] },
] as unknown as ProviderKey[]
function show(editing?: ProviderKeyModel, inventory = keys) {
  const calls: Array<{ path: string; body: unknown }> = []
  vi.stubGlobal("fetch", vi.fn(async (path: string, options?: RequestInit) => {
    if (options?.body) calls.push({ path, body: JSON.parse(String(options.body)) })
    return new Response(JSON.stringify(path === "/api/admin/routes" ? [] : { ok: true }), { status: 200, headers: { "content-type": "application/json" } })
  }))
  const close = vi.fn()
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ManualModelDialog open onOpenChange={close} providerId={10} keys={inventory} selectedKeyId={1} editing={editing} /></QueryClientProvider>)
  return { calls, close }
}
describe("manual upstream models", () => {
  it("parses newline input without changing upstream capitalization", () => {
    expect(parseManualModels(" Vendor/Model-X\r\n\nVendor/Model-X\nother ")).toEqual(["Vendor/Model-X", "other"])
  })
  it("saves batch models only to explicitly chosen keys without discovery", async () => {
    const { calls, close } = show()
    fireEvent.change(screen.getByLabelText("上游模型名（每行一个，最多 200 个）"), { target: { value: "Vendor/Model-X\nbackup" } })
    fireEvent.click(screen.getByRole("button", { name: "当前渠道全部 Key" }))
    fireEvent.click(screen.getByRole("button", { name: "保存模型" }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(calls).toHaveLength(1)
    expect(calls[0].path).toBe("/api/admin/providers/10/manual-models")
    expect(calls[0].body).toMatchObject({ entries: [{ model: "Vendor/Model-X", capabilities: "chat,stream" }, { model: "backup", capabilities: "chat,stream" }], key_ids: [1, 2], enabled: true, create_routes: true })
  })
  it("hints at the default basename without changing upstream or request names", async () => {
    const { calls, close } = show()
    fireEvent.change(screen.getByLabelText("上游模型名（每行一个，最多 200 个）"), { target: { value: "deepseek/deepseek-v4.1-flash" } })
    const display = screen.getByLabelText("显示名称（选填，不改变转发名称）") as HTMLInputElement
    expect(display.value).toBe("")
    expect(display.placeholder).toBe("留空默认去掉渠道前缀：deepseek-v4.1-flash")
    fireEvent.click(screen.getByRole("button", { name: "保存模型" }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(calls[0].body).toMatchObject({ entries: [{ model: "deepseek/deepseek-v4.1-flash", display_name: "" }] })
    expect(calls[0].body).not.toHaveProperty("public_name")
  })
  it("saves the edited explicit display name independently of wire names", async () => {
    const { calls, close } = show({ model: "deepseek/deepseek-v4.1-flash", display_name: "旧名称", enabled: true, capabilities: "chat,stream" } as ProviderKeyModel)
    fireEvent.change(screen.getByLabelText("显示名称（选填，不改变转发名称）"), { target: { value: "我的 / Flash" } })
    fireEvent.click(screen.getByRole("button", { name: "保存模型" }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(calls[0].body).toMatchObject({ entries: [{ model: "deepseek/deepseek-v4.1-flash", display_name: "我的 / Flash" }], original_model: "deepseek/deepseek-v4.1-flash", sync_routes: false })
    expect(calls[0].body).not.toHaveProperty("public_name")
  })
  it("disables shared route rename until all old-model keys are selected", () => {
    const model = { model: "old", display_name: "Old", enabled: true, capabilities: "chat,stream" } as ProviderKeyModel
    show(model, keys.map((key) => ({ ...key, models: [model] })))
    fireEvent.change(screen.getByLabelText("上游模型名"), { target: { value: "new" } })
    expect((screen.getByRole("switch", { name: "同步更新关联路由的上游模型名" }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "当前渠道全部 Key" }))
    expect((screen.getByRole("switch", { name: "同步更新关联路由的上游模型名" }) as HTMLButtonElement).disabled).toBe(false)
  })
})
