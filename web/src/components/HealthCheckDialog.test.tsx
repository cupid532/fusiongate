import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import type { HealthCheckJob, HealthCheckPreview } from "@/lib/types"
import { HealthCheckDialog } from "./HealthCheckDialog"

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })
}

function preview(): HealthCheckPreview {
  return {
    provider_id: 7,
    provider_name: "Fast",
    auth_kind: "api_key",
    enabled: true,
    health_check_enabled: true,
    probeable: 1,
    routes: [
      {
        route_id: 1,
        public_name: "gpt-live",
        upstream_model: "gpt-live",
        capabilities: "chat,stream",
        supported: true,
        keys: [{ key_id: 25, name: "Key 1", hint: "sk-…abcd", enabled: true, health_check_enabled: true, supported: true }],
      },
      {
        route_id: 2,
        public_name: "gpt-ghost",
        upstream_model: "gpt-ghost",
        capabilities: "chat,stream",
        supported: false,
        reason: "this model is switched off on every API key; enable it in model management",
        keys: [{ key_id: 25, name: "Key 1", hint: "sk-…abcd", enabled: true, health_check_enabled: true, supported: false, reason: "model is switched off on this API key" }],
      },
    ],
  }
}

function job(partial: Partial<HealthCheckJob> = {}): HealthCheckJob {
  return {
    id: "job-1",
    mode: "generation",
    status: "completed",
    total: 1,
    completed: 1,
    healthy: 1,
    failed: 0,
    skipped: 0,
    created_at: "2026-09-05T00:00:00Z",
    can_cancel: false,
    results: [{ provider_id: 7, provider_name: "Fast", provider_key_id: 25, provider_key_name: "Key 1", route_id: 1, public_name: "gpt-live", model: "gpt-live", status: "healthy", latency_ms: 812, model_count: 0 }],
    ...partial,
  }
}

type Route = (url: string, init?: RequestInit) => Response | undefined

function mockFetch(route: Route) {
  const calls: Array<{ url: string; method: string; body?: unknown }> = []
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      calls.push({ url, method: (init?.method ?? "GET").toUpperCase(), body: init?.body ? JSON.parse(String(init.body)) : undefined })
      return route(url, init) ?? json({ error: { message: `unmocked ${url}` } }, 500)
    })
  )
  return calls
}

function renderDialog(props: Partial<React.ComponentProps<typeof HealthCheckDialog>> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <HealthCheckDialog open onOpenChange={() => {}} providerIds={[7]} title="模型检活 · Fast" {...props} />
    </QueryClientProvider>
  )
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe("HealthCheckDialog", () => {
  it("shows why a route cannot be probed and keeps it unselectable", async () => {
    mockFetch((url) => {
      if (url.endsWith("/health-check-targets")) return json(preview())
      if (url === "/api/admin/health-checks") return json({ active: false })
      return undefined
    })
    renderDialog()

    await screen.findByText("gpt-ghost")
    // The reason arrives in English from the server and must read as Chinese here.
    expect(screen.getByText(/该模型在所有 Key 上都处于关闭状态/)).toBeTruthy()
    expect(screen.getByText(/1 个无法检活/)).toBeTruthy()

    const boxes = screen.getAllByRole("checkbox") as HTMLInputElement[]
    const ghost = boxes.find((b) => (b.closest("label")?.textContent ?? "").includes("gpt-ghost"))
    expect(ghost?.disabled).toBe(true)
    // "全选可检活" must only pick the live route.
    fireEvent.click(screen.getByText("全选可检活"))
    expect(screen.getByText(/已选/).textContent).toContain("1")
  })

  it("only sends probeable routes and the keys that serve them", async () => {
    const calls = mockFetch((url, init) => {
      if (url.endsWith("/health-check-targets")) return json(preview())
      if (url === "/api/admin/health-checks" && (init?.method ?? "GET") === "GET") return json({ active: false })
      if (url === "/api/admin/health-checks" && init?.method === "POST") return json(job({ status: "completed" }), 202)
      return undefined
    })
    renderDialog()
    await screen.findByText("gpt-live")
    fireEvent.click(screen.getByRole("button", { name: "Key 1" }))
    fireEvent.click(screen.getByText("全选可检活"))
    fireEvent.click(screen.getByText("测活选中"))

    await screen.findByText("全部健康")
    const start = calls.find((c) => c.method === "POST")
    expect(start?.body).toEqual({ provider_ids: [7], model_scope: "selected", route_ids: [1], provider_key_ids: [25] })
  })

  it("treats another running job as a blocker, not a failure", async () => {
    const other = job({ id: "other", status: "running", total: 105, completed: 40, healthy: 40, results: [{ provider_id: 9, provider_name: "Grok · a", status: "healthy", latency_ms: 1, model_count: 0 }], can_cancel: true })
    let active = true
    mockFetch((url, init) => {
      if (url.endsWith("/health-check-targets")) return json(preview())
      if (url === "/api/admin/health-checks" && (init?.method ?? "GET") === "GET") return json(active ? { active: true, job: other } : { active: false })
      if (url === "/api/admin/health-checks/other" && init?.method === "DELETE") {
        active = false
        return json({ ...other, status: "cancelled", can_cancel: false })
      }
      return undefined
    })
    renderDialog()

    await screen.findByText(/另一项检活正在进行/)
    expect(screen.getByText(/40\/105/)).toBeTruthy()
    const startAll = screen.getByText("等待中…").closest("button") as HTMLButtonElement
    expect(startAll.disabled).toBe(true)
    // Nothing red: a busy slot is a state, not an error.
    expect(screen.queryByText(/already running/)).toBeNull()

    fireEvent.click(screen.getByText("取消它"))
    await waitFor(() => expect(screen.queryByText(/另一项检活正在进行/)).toBeNull())
    await waitFor(() => expect((screen.getByText("一键测活全部").closest("button") as HTMLButtonElement).disabled).toBe(false))
  })

  it("renders skipped results with the translated reason", async () => {
    const skipped = job({
      total: 2,
      completed: 2,
      healthy: 1,
      skipped: 1,
      results: [
        ...job().results,
        { provider_id: 7, provider_name: "Fast", route_id: 2, public_name: "gpt-ghost", model: "gpt-ghost", status: "skipped", latency_ms: 0, model_count: 0, error: "this model is switched off on every API key; enable it in model management" },
      ],
    })
    mockFetch((url, init) => {
      if (url === "/api/admin/health-checks" && init?.method === "POST") return json(skipped, 202)
      if (url === "/api/admin/health-checks") return json({ active: false })
      return undefined
    })
    renderDialog({ providerIds: [7, 8], title: "批量检活 · 2 个渠道", autoStart: true })

    await screen.findByText("已跳过")
    expect(screen.getByText("健康")).toBeTruthy()
    expect(screen.getByText(/该模型在所有 Key 上都处于关闭状态/)).toBeTruthy()
    fireEvent.click(screen.getByLabelText("仅看问题"))
    expect(screen.queryByText("gpt-live")).toBeNull()
    expect(screen.getByText("gpt-ghost")).toBeTruthy()
  })

  it("explains a start rejection in Chinese", async () => {
    mockFetch((url, init) => {
      if (url === "/api/admin/health-checks" && init?.method === "POST") return json({ error: { code: "invalid_provider_selection", message: "disabled providers cannot be health checked" } }, 400)
      if (url === "/api/admin/health-checks") return json({ active: false })
      return undefined
    })
    renderDialog({ providerIds: [7, 8], title: "批量检活", autoStart: true })
    await screen.findByText("渠道已停用，无法检活")
    expect(screen.getByText("重试")).toBeTruthy()
  })
})
