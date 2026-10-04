import { afterEach, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ProviderModelsPanel } from "@/components/ProviderModelManagementDialog"

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
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
