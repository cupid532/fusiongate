import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { ProviderEgressDialog } from "@/components/ProviderEgressDialog"

afterEach(() => { cleanup(); vi.unstubAllGlobals() })
function show() {
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProviderEgressDialog open onOpenChange={() => {}} providerIds={[1, 2]} /></QueryClientProvider>)
}
describe("shared channel egress", () => {
  it("reads back an uncertain write without replaying it", async () => {
    let written = false
    const requests: string[] = []
    vi.stubGlobal("fetch", vi.fn(async (path: string, options?: RequestInit) => {
      if (options?.method === "POST") { requests.push(path); written = true; throw new TypeError("Disconnected after commit") }
      return new Response(JSON.stringify(path.endsWith("/providers") ? [{ id: 1, name: "First", ip_pool_node_id: written ? undefined : 5 }, { id: 2, name: "Second", ip_pool_node_id: written ? undefined : 5 }] : []), { status: 200, headers: { "content-type": "application/json" } })
    }))
    show()
    await waitFor(() => expect((screen.getByRole("combobox", { name: "目标出口" }) as HTMLSelectElement).disabled).toBe(false))
    fireEvent.change(screen.getByRole("combobox", { name: "目标出口" }), { target: { value: "direct" } })
    fireEvent.click(screen.getByRole("button", { name: "确认应用默认出口" }))
    await screen.findByText("请求结果不确定，请先读取当前配置核对，不要重复应用。")
    fireEvent.click(screen.getByRole("button", { name: "读取配置核对" }))
    await screen.findByText("所选渠道当前配置已与目标一致，未重复提交。")
    expect(requests).toEqual(["/api/admin/providers/batch"])
    expect(screen.queryByRole("alert")).toBeNull()
  })

  it("requires an explicit choice and never changes independent keys", async () => {
    const requests: Array<{ path: string; body: unknown }> = []
    vi.stubGlobal("fetch", vi.fn(async (path: string, options?: RequestInit) => {
      if (options?.body) requests.push({ path, body: JSON.parse(String(options.body)) })
      return new Response(JSON.stringify(path.endsWith("/batch") ? { affected: 2 } : path.endsWith("/providers") ? [{ id: 1, name: "First" }, { id: 2, name: "Second" }] : []), { status: 200, headers: { "content-type": "application/json" } })
    }))
    show()
    const button = screen.getByRole("button", { name: "确认应用默认出口" }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    await waitFor(() => expect((screen.getByRole("combobox", { name: "目标出口" }) as HTMLSelectElement).disabled).toBe(false))
    fireEvent.change(screen.getByRole("combobox", { name: "目标出口" }), { target: { value: "direct" } })
    fireEvent.click(button)
    await screen.findByText("已更新 2 个渠道的默认出口；Key 独立出口保持不变。")
    expect(requests).toEqual([{ path: "/api/admin/providers/batch", body: { provider_ids: [1, 2], action: "egress", ip_pool_node_id: 0 } }])
  })
  it("keeps the apply button disabled when node loading fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async (path: string) => new Response(JSON.stringify(path.endsWith("/ip-pool") ? { error: { message: "Node read failed" } } : []), { status: path.endsWith("/ip-pool") ? 500 : 200, headers: { "content-type": "application/json" } })))
    show()
    await screen.findByText("无法读取出口节点")
    expect((screen.getByRole("button", { name: "确认应用默认出口" }) as HTMLButtonElement).disabled).toBe(true)
    expect(screen.queryByRole("combobox", { name: "目标出口" })).toBeNull()
  })
})
