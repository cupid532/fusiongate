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
    return new Response(JSON.stringify([]), { status: 200, headers: { "content-type": "application/json" } })
  }))
}

function show(p: Provider) {
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
    fireEvent.click(screen.getByText("高级设置"))
    const select = screen.getByRole("combobox", { name: "接口方式" }) as HTMLSelectElement
    expect(select.value).toBe("auto")
    expect((screen.getByRole("option", { name: /Anthropic Messages/ }) as HTMLOptionElement).disabled).toBe(true)
    fireEvent.change(select, { target: { value: "responses" } })
    fireEvent.click(screen.getByRole("button", { name: "保存渠道" }))
    await waitFor(() => expect(requests.find((r) => r.url === "/api/admin/providers/7")?.body).toMatchObject({
      protocol_policy: "fixed", protocol_preference: "responses",
    }))
  })

  it("preserves a legacy preference when saving unrelated fields", async () => {
    setup()
    show(provider({ protocol_policy: "fixed", protocol_preference: "responses,chat" }))
    fireEvent.click(screen.getByText("高级设置"))
    expect(screen.getByText(/现有配置/).textContent).toContain("Responses")
    fireEvent.click(screen.getByRole("button", { name: "保存渠道" }))
    await waitFor(() => expect(requests.some((r) => r.url === "/api/admin/providers/7")).toBe(true))
    const saved = requests.find((r) => r.url === "/api/admin/providers/7")!.body
    expect(saved).not.toHaveProperty("protocol_policy")
    expect(saved).not.toHaveProperty("protocol_preference")
  })
})
