import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import App from "./App"
import { ThemeProvider } from "./providers/theme"

vi.mock("./providers/auth", () => ({
  useAuth: () => ({ loading: false, authenticated: true, login: vi.fn(), logout: vi.fn() }),
}))

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  history.replaceState(null, "", "#dashboard")
})

function mount() {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { headers: { "content-type": "application/json" } })))
  // jsdom ships no matchMedia, which the theme provider reads on mount.
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: false, media: query, onchange: null,
    addEventListener: vi.fn(), removeEventListener: vi.fn(),
    addListener: vi.fn(), removeListener: vi.fn(), dispatchEvent: vi.fn(),
  }))
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ThemeProvider>
        <App />
      </ThemeProvider>
    </QueryClientProvider>,
  )
}

describe("console routing", () => {
  it.each(["#%", "#%E0%A4%A", "#no-such-page"])("renders %s instead of dying on it", (hash) => {
    // `decodeURIComponent` throws on a malformed escape, and the throw happened
    // inside the initial useState — the whole console rendered as a blank page.
    history.replaceState(null, "", hash)
    expect(() => mount()).not.toThrow()
    expect(screen.getAllByText("概览").length).toBeGreaterThan(0)
  })

  it("redirects the legacy capabilities bookmark into settings", () => {
    history.replaceState(null, "", "#capabilities")
    mount()
    expect(location.hash).toBe("#settings?tab=capabilities")
  })
})
