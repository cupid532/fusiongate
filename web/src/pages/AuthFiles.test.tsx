import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { AuthFiles } from "./AuthFiles"
import { ConfirmProvider } from "@/components/ui/confirm"
import type { Provider } from "@/lib/types"

vi.mock("@formkit/auto-animate/react", () => ({ useAutoAnimate: () => [undefined] }))
vi.mock("@/lib/notify", () => ({ notify: vi.fn(), notifySuccess: vi.fn(), notifyError: vi.fn(), reportUnauthorized: vi.fn() }))
// jsdom cannot start a download; keep the real api/apiDownload, stub only the save.
vi.mock("@/lib/api", async (importOriginal) => ({ ...await importOriginal<typeof import("@/lib/api")>(), saveBlob: vi.fn() }))

const claude = {
  id: 4, name: "Claude One", type: "claude_oauth", auth_kind: "oauth",
  base_url: "", website_url: "", notes: "", enabled: true, archived: false,
  priority: 1, sort_order: 0, model_count: 1, health_check_enabled: true,
  health_check_status: "healthy", auth_status: "ready", auth_email: "a@example.com",
} as unknown as Provider

function mount(onExport: (body: unknown) => Response) {
  const calls: Array<{ url: string; method: string; body?: unknown }> = []
  const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "content-type": "application/json" } })
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const method = (init?.method ?? "GET").toUpperCase()
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ url, method, body })
    if (url === "/api/admin/providers") return json([claude])
    if (url === "/api/admin/auth/export" && method === "POST") return onExport(body)
    return json([])
  }))
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ConfirmProvider>
        <AuthFiles />
      </ConfirmProvider>
    </QueryClientProvider>,
  )
  return calls
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe("sensitive credential export", () => {
  it("asks before telling the gateway the operator acknowledged the risk", async () => {
    // The request body itself carries `acknowledge_sensitive_export: true`.
    // Sending it straight from the button means the console answers for the
    // operator, who never saw a warning about plaintext OAuth credentials.
    const calls = mount(() => new Response("{}", { headers: { "content-type": "application/json" } }))
    await screen.findByText("Claude One")

    fireEvent.click(screen.getByRole("button", { name: "多选" }))
    fireEvent.click(screen.getByRole("checkbox", { name: "选择 Claude One" }))
    fireEvent.click(screen.getByRole("button", { name: "批量导出" }))

    // Nothing has left the browser yet.
    expect(calls.some((call) => call.url === "/api/admin/auth/export")).toBe(false)
    expect(await screen.findByText(/包含可直接使用的 OAuth 凭据/)).toBeTruthy()

    fireEvent.click(screen.getByRole("button", { name: "仍要导出" }))

    await waitFor(() => {
      const exported = calls.find((call) => call.url === "/api/admin/auth/export")
      expect(exported?.body).toEqual({ provider_ids: [4], acknowledge_sensitive_export: true })
    })
  })
})
