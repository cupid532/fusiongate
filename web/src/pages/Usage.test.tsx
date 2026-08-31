import { fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import type { TokenUsageMetrics, TokenUsageResponse } from "@/lib/types"
import { Usage } from "./Usage"

const emptyMetrics: TokenUsageMetrics = {
  requests: 0,
  attempts: 0,
  successful_requests: 0,
  reported_requests: 0,
  input_tokens: 0,
  output_tokens: 0,
  cached_tokens: 0,
  reasoning_tokens: 0,
  total_tokens: 0,
  cost_micros: 0,
  priced_attempts: 0,
  usage_coverage: 0,
  cost_coverage: 0,
}

function usageResponse(): TokenUsageResponse {
  return {
    period: { days: 90, from: "2026-01-01", to: "2026-03-31", retention_days: 365, timezone: "UTC" },
    totals: { ...emptyMetrics, requests: 2, total_tokens: 30 },
    series: [],
    by_keys: [],
    by_providers: [],
    by_models: [],
    heatmap: [
      { model: "shared", upstream_model: "up-a", date: "2026-01-01", requests: 1, input_tokens: 10, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, total_tokens: 10, cost_micros: 100 },
      { model: "shared", upstream_model: "up-b", date: "2026-02-01", requests: 1, input_tokens: 20, output_tokens: 0, cached_tokens: 0, reasoning_tokens: 0, total_tokens: 20, cost_micros: 200 },
    ],
    details: [],
    page: 1,
    page_size: 20,
    has_more: false,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe("Usage heatmap", () => {
  it("uses full dates and the exact duplicate-day column in tooltips", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      const data = path.includes("token-usage") ? usageResponse() : []
      return new Response(JSON.stringify(data), { headers: { "Content-Type": "application/json" } })
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <Usage />
      </QueryClientProvider>,
    )

    fireEvent.click(screen.getByRole("tab", { name: "热力图" }))

    expect(await screen.findByText("01-01")).toBeDefined()
    expect(screen.getByText("02-01")).toBeDefined()

    await waitFor(() => {
      const titles = Array.from(document.querySelectorAll<HTMLElement>("[title]"), (element) => element.title)
      expect(titles).toContain("shared (up-a) · 2026-01-01\n1 请求\n输入 10 · 缓存 0 · 输出 0\n费用 $0.00")
      expect(titles).toContain("shared (up-b) · 2026-02-01\n1 请求\n输入 20 · 缓存 0 · 输出 0\n费用 $0.00")
    })
  })
})
