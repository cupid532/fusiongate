import { afterEach, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ProviderModelsPanel } from "@/components/ProviderModelManagementDialog"

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
it("renders explicit display names or upstream basenames while preserving inventory identities", async () => {
  const models = [
    { model: "deepseek/deepseek-v4.1-flash", display_name: "", enabled: true, capabilities: "chat,stream" },
    { model: "other/deepseek-v4.1-flash", display_name: "", enabled: true, capabilities: "chat,stream" },
    { model: "vendor/model", display_name: "我的 / Flash", enabled: true, capabilities: "chat,stream" },
  ]
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify([{ id: 1, name: "Primary", key_hint: "sk-***", model_policy: "allowlist", models, enabled: true }]), { status: 200, headers: { "content-type": "application/json" } })))
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProviderModelsPanel open providerId={12} /></QueryClientProvider>)
  await screen.findByText("我的 / Flash")
  expect(screen.getAllByText("deepseek-v4.1-flash")).toHaveLength(2)
  for (const model of models) {
    const toggle = screen.getByRole("switch", { name: `Primary 模型 ${model.model}` })
    const row = toggle.parentElement!
    expect(within(row).getByText(model.display_name || "deepseek-v4.1-flash")).toBeTruthy()
    expect(within(row).getByText(model.model)).toBeTruthy()
    expect(toggle.getAttribute("aria-checked")).toBe("true")
  }
})
it("selects only filtered results, preserves other selections and supports staged removal undo", async () => {
  const calls: string[] = []
  const models = [
    { model: "Alpha", enabled: false, capabilities: "chat,stream" },
    { model: "Beta", enabled: true, capabilities: "chat,stream" },
    { model: "Gamma", enabled: false, capabilities: "chat,stream" },
  ]
  vi.stubGlobal("fetch", vi.fn(async (path: string, options?: RequestInit) => {
    if (options?.method && options.method !== "GET") calls.push(path)
    return new Response(JSON.stringify([{ id: 1, name: "Primary", model_policy: "allowlist", models, enabled: true }]), { status: 200, headers: { "content-type": "application/json" } })
  }))
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProviderModelsPanel open providerId={12} /></QueryClientProvider>)
  await screen.findByRole("switch", { name: "Primary 模型 Alpha" })
  fireEvent.change(screen.getByPlaceholderText("搜索模型"), { target: { value: "Alpha" } })
  fireEvent.click(screen.getByRole("button", { name: "选择当前结果（1）" }))
  fireEvent.change(screen.getByPlaceholderText("搜索模型"), { target: { value: "" } })
  expect(screen.getByRole("switch", { name: "Primary 模型 Beta" }).getAttribute("aria-checked")).toBe("true")
  expect(screen.getByRole("switch", { name: "Primary 模型 Gamma" }).getAttribute("aria-checked")).toBe("false")
  fireEvent.click(screen.getByRole("button", { name: "删除模型 Beta" }))
  expect(screen.queryByRole("switch", { name: "Primary 模型 Beta" })).toBeNull()
  expect(screen.getByText(/待从此 Key 清单移除 1 个模型/)).toBeTruthy()
  fireEvent.click(screen.getByRole("button", { name: "撤销移除" }))
  expect(screen.getByRole("switch", { name: "Primary 模型 Beta" }).getAttribute("aria-checked")).toBe("true")
  expect(calls).toHaveLength(0)
})
it("offers manual models for an empty inventory without invoking discovery", async () => {
  const calls: string[] = []
  vi.stubGlobal("fetch", vi.fn(async (path: string) => {
    calls.push(path)
    const result = path.includes("/keys") ? [{ id: 1, name: "Primary", key_hint: "sk-***", model_policy: "allowlist", models: [], enabled: true }] : []
    return new Response(JSON.stringify(result), { status: 200, headers: { "content-type": "application/json" } })
  }))
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProviderModelsPanel open providerId={12} /></QueryClientProvider>)
  const button = await screen.findByRole("button", { name: "手动添加 / 批量添加模型" })
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false))
  fireEvent.click(button)
  expect(screen.getByRole("heading", { name: "手动添加模型" })).toBeTruthy()
  expect(screen.getByText("尚无模型，可手动添加，也可识别该 Key 的模型。")).toBeTruthy()
  expect(calls.some((path) => path.includes("discover"))).toBe(false)
})
