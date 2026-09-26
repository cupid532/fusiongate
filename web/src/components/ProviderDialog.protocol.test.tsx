import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { ConfirmProvider } from "@/components/ui/confirm"
import type { Provider } from "@/lib/types"
import { ProviderDialog } from "./ProviderDialog"

function provider(overrides: Partial<Provider> = {}): Provider {
  return {
    id: 7, name: "测试渠道", type: "openai_compatible", base_url: "https://upstream.example",
    protocol_policy: "auto", protocol_preference: "", key_selection_mode: "configured",
    priority: 1, max_concurrency: 0, request_timeout_ms: 120000, passthrough_mode: "normalized",
    notes: "", ...overrides,
  } as Provider
}

const requests: Array<{ url: string; body: Record<string, unknown> }> = []
function setup() {
  requests.length = 0
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
    const url = String(input)
    if (options?.body) requests.push({ url, body: JSON.parse(String(options.body)) })
    return new Response(JSON.stringify(options?.method === "POST" && url === "/api/admin/providers" ? { id: 17 } : []), { status: 200, headers: { "content-type": "application/json" } })
  }))
}

function show(p: Provider | null) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <ConfirmProvider>
        <ProviderDialog open provider={p} onOpenChange={() => {}} />
      </ConfirmProvider>
    </QueryClientProvider>
  )
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe("ProviderDialog interface method", () => {
  it("saves a fixed Responses method for an OpenAI compatible channel", async () => {
    setup()
    show(provider())
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    const select = screen.getByRole("combobox", { name: "接口方式" }) as HTMLSelectElement
    expect(select.value).toBe("auto")
    expect((screen.getByRole("option", { name: /Anthropic Messages/ }) as HTMLOptionElement).disabled).toBe(true)
    fireEvent.change(select, { target: { value: "responses" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7")?.body).toMatchObject({
      protocol_policy: "fixed", protocol_preference: "responses",
    }))
  })

  it("preserves a legacy preference when saving unrelated fields", async () => {
    setup()
    show(provider({ protocol_policy: "fixed", protocol_preference: "responses,chat" }))
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    expect(screen.getByText(/旧配置/).textContent).toContain("Responses")
    fireEvent.click(screen.getByRole("button", { name: /连接信息/ }))
    fireEvent.change(screen.getByRole("textbox", { name: "备注" }), { target: { value: "更新备注" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(true))
    const saved = requests.find((r) => r.url === "/api/admin/providers/7")!.body
    expect(saved).not.toHaveProperty("protocol_policy")
    expect(saved).not.toHaveProperty("protocol_preference")
  })

  it("saves channel health-check enablement with channel parameters", async () => {
    setup()
    show(provider({ health_check_enabled: false }))
    fireEvent.click(screen.getByRole("button", { name: /健康检查/ }))
    fireEvent.click(screen.getByRole("switch", { name: "允许渠道检活" }))
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7")?.body).toMatchObject({ health_check_enabled: true }))
  })

  it("saves Key egress and cost independently of channel settings", async () => {
    setup()
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
      const url = String(input)
      if (options?.body) requests.push({ url, body: JSON.parse(String(options.body)) })
      const body = url.endsWith("/keys") ? [{ id: 3, name: "主 Key", key_hint: "***", enabled: true, health_check_enabled: true, egress_mode: "inherit", cost_multiplier: 1, models: [] }] : []
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } })
    }))
    show(provider())
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    const egress = await screen.findByRole("combobox", { name: "出口" })
    fireEvent.change(egress, { target: { value: "direct" } })
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7/keys/3")?.body).toMatchObject({ egress_mode: "direct", ip_pool_node_id: null }))
    const cost = screen.getByRole("spinbutton", { name: "成本倍率" })
    fireEvent.change(cost, { target: { value: "1.25" } })
    fireEvent.blur(cost)
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7/keys/3" && r.body.cost_multiplier === 1.25)).toBe(true))
    expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(false)
  })

  it("creates with the first Key and continues in the Key card", async () => {
    setup()
    show(null)
    fireEvent.change(screen.getByRole("textbox", { name: "名称" }), { target: { value: "新渠道" } })
    fireEvent.change(screen.getByRole("textbox", { name: "API 地址" }), { target: { value: "https://new.example" } })
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    fireEvent.change(screen.getByRole("textbox", { name: "首张 API Key" }), { target: { value: "sk-first" } })
    fireEvent.click(screen.getByRole("button", { name: "创建渠道" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers")?.body).toMatchObject({ credential: "sk-first", name: "新渠道" }))
    await waitFor(() => expect(screen.getByRole("heading", { name: "管理渠道 · 新渠道" })).toBeTruthy())
    expect(screen.getByRole("button", { name: "添加 Key" })).toBeTruthy()
  })
})
