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
    priority: 1, max_concurrency: 0, request_timeout_ms: 120000,
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

describe("ProviderDialog passthrough", () => {
  it("clears channel egress with zero and leaves other connection settings untouched", async () => {
    setup()
    show(provider({ ip_pool_node_id: 8 }))
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    const egress = screen.getByRole("combobox", { name: "渠道默认出口" })
    await waitFor(() => expect((egress as HTMLSelectElement).disabled).toBe(false))
    fireEvent.change(egress, { target: { value: "direct" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.find((request) => request.url === "/api/admin/providers/7")?.body).toEqual({ ip_pool_node_id: 0 }))
  })

  it("retains unsaved model drafts across section changes and discards them when reopened", async () => {
    setup()
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const body = String(input).endsWith("/keys") ? [{ id: 3, name: "主 Key", key_hint: "***", enabled: true, model_policy: "fallback", egress_mode: "inherit", models: [{ model: "model-a", enabled: false }] }] : []
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } })
    }))
    const channel = provider()
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const content = (open: boolean) => <QueryClientProvider client={client}><ConfirmProvider><ProviderDialog open={open} provider={channel} onOpenChange={() => {}} /></ConfirmProvider></QueryClientProvider>
    const view = render(content(true))
    fireEvent.click(screen.getByRole("button", { name: /模型管理/ }))
    fireEvent.click(await screen.findByRole("switch", { name: "主 Key 模型 model-a" }))
    fireEvent.click(screen.getByRole("button", { name: /连接信息/ }))
    fireEvent.click(screen.getByRole("button", { name: /模型管理/ }))
    expect(screen.getByRole("switch", { name: "主 Key 模型 model-a" }).getAttribute("aria-checked")).toBe("true")
    view.rerender(content(false))
    view.rerender(content(true))
    fireEvent.click(screen.getByRole("button", { name: /模型管理/ }))
    await waitFor(() => expect(screen.getByRole("switch", { name: "主 Key 模型 model-a" }).getAttribute("aria-checked")).toBe("false"))
  })

  it("keeps the protocol choice unless it is changed", async () => {
    setup()
    show(provider({ passthrough_supported: true }))
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    expect(screen.getByRole("combobox", { name: "接口方式" })).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: /连接信息/ }))
    fireEvent.change(screen.getByRole("textbox", { name: "备注" }), { target: { value: "更新备注" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(true))
    const saved = requests.find((r) => r.url === "/api/admin/providers/7")!.body
    expect(saved).not.toHaveProperty("protocol_policy")
    expect(saved).not.toHaveProperty("passthrough_mode")
  })

  it("writes a fixed protocol when one is chosen", async () => {
    setup()
    show(provider({ passthrough_supported: true }))
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    fireEvent.change(screen.getByRole("combobox", { name: "接口方式" }), { target: { value: "chat" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(true))
    const saved = requests.find((r) => r.url === "/api/admin/providers/7")!.body
    expect(saved).toMatchObject({ protocol_policy: "fixed", protocol_preference: "chat" })
  })

  it("shows a server-provided unsupported channel reason", async () => {
    setup()
    show(provider({ passthrough_supported: false, passthrough_reason: "OAuth 专用协议" }))
    fireEvent.click(screen.getByRole("button", { name: /转发与调度/ }))
    expect(screen.getByText("OAuth 专用协议")).toBeTruthy()
  })

  it("saves channel health-check enablement with channel parameters", async () => {
    setup()
    show(provider({ health_check_enabled: false }))
    fireEvent.click(screen.getByRole("button", { name: /健康检查/ }))
    fireEvent.click(screen.getByRole("switch", { name: "允许渠道检活" }))
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7")?.body).toMatchObject({ health_check_enabled: true }))
  })

  it("preserves Key egress while explicitly saving cost independently of channel settings", async () => {
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
    await waitFor(() => expect((egress as HTMLSelectElement).disabled).toBe(false))
    expect(requests).toHaveLength(0)
    const cost = screen.getByRole("spinbutton", { name: "成本倍率" })
    fireEvent.change(cost, { target: { value: "1.25" } })
    fireEvent.blur(cost)
    expect(requests.some((r) => r.body.cost_multiplier === 1.25)).toBe(false)
    fireEvent.click(screen.getByRole("button", { name: "保存 Key" }))
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7/keys/3" && r.body.cost_multiplier === 1.25)).toBe(true))
    expect(requests.find((r) => r.url === "/api/admin/providers/7/keys/3")?.body).not.toHaveProperty("egress_mode")
    expect(requests.find((r) => r.url === "/api/admin/providers/7/keys/3")?.body).not.toHaveProperty("ip_pool_node_id")
    expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(false)
  })

  it.each([
    ["direct", { egress_mode: "direct", ip_pool_node_id: 0 }],
    ["node:8", { egress_mode: "node", ip_pool_node_id: 8 }],
  ])("explicitly saves Key egress %s without modifying model policy", async (value, expected) => {
    setup()
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
      const url = String(input)
      if (options?.body) requests.push({ url, body: JSON.parse(String(options.body)) })
      const body = url.endsWith("/keys") ? [{ id: 3, name: "主 Key", key_hint: "***", enabled: true, health_check_enabled: true, egress_mode: "inherit", cost_multiplier: 1, models: [] }] : url.endsWith("/ip-pool") ? [{ id: 8, name: "出口节点", protocol: "socks5", enabled: true }] : []
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } })
    }))
    show(provider())
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    const egress = await screen.findByRole("combobox", { name: "出口" })
    await waitFor(() => expect((egress as HTMLSelectElement).disabled).toBe(false))
    fireEvent.change(egress, { target: { value } })
    expect(requests).toHaveLength(0)
    fireEvent.click(screen.getByRole("button", { name: "保存 Key" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7/keys/3")?.body).toEqual(expected))
  })

  it("saves and clears the merchant URL with channel parameters", async () => {
    setup()
    show(provider({ website_url: "https://shop.example.com/old" }))
    fireEvent.change(screen.getByRole("textbox", { name: "商家地址（选填）" }), { target: { value: "https://shop.example.com/topup" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7")?.body).toMatchObject({ website_url: "https://shop.example.com/topup" }))
  })

  it("creates with the first Key and continues in model configuration", async () => {
    setup()
    show(null)
    fireEvent.change(screen.getByRole("textbox", { name: "名称" }), { target: { value: "新渠道" } })
    fireEvent.change(screen.getByRole("textbox", { name: "API 地址" }), { target: { value: "https://new.example" } })
    fireEvent.change(screen.getByRole("textbox", { name: "商家地址（选填）" }), { target: { value: "https://merchant.example/credit" } })
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    fireEvent.change(screen.getByLabelText("首张 API Key"), { target: { value: "sk-first" } })
    fireEvent.click(screen.getByRole("button", { name: "创建渠道" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers")?.body).toMatchObject({ credential: "sk-first", name: "新渠道", website_url: "https://merchant.example/credit" }))
    await waitFor(() => expect(screen.getByRole("heading", { name: "管理渠道 · 新渠道" })).toBeTruthy())
    expect(screen.getByRole("button", { name: "手动添加 / 批量添加模型" })).toBeTruthy()
    expect(screen.getByRole("button", { name: "模型管理" }).getAttribute("aria-current")).toBe("page")
  })

  it("keeps the first channel API Key masked and out of password managers", () => {
    setup()
    show(null)
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    const initial = screen.getByLabelText("首张 API Key") as HTMLInputElement
    // Masked for shoulder-surfing, but an upstream credential rather than a
    // website password, so no password manager should offer to save or fill it.
    expect(initial.type).toBe("password")
    expect(initial.autocomplete).toBe("off")
    expect(initial.getAttribute("data-1p-ignore")).toBe("true")
    expect(initial.getAttribute("data-lpignore")).toBe("true")
  })

  it("excludes an existing channel's new Key field from password managers", async () => {
    setup()
    show(provider())
    fireEvent.click(screen.getByRole("button", { name: /API Keys/ }))
    const newKey = (await screen.findByLabelText("新 API Key")) as HTMLInputElement
    expect(newKey.type).toBe("password")
    expect(newKey.autocomplete).toBe("off")
    expect(newKey.getAttribute("data-1p-ignore")).toBe("true")
    expect(newKey.getAttribute("data-lpignore")).toBe("true")
  })
})
