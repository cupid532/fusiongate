import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { AuthProvider, useAuth } from "./auth"
import { notify } from "@/lib/notify"

vi.mock("@/lib/notify", () => ({
  notify: vi.fn(),
  reportUnauthorized: vi.fn(),
  setUnauthorizedHandler: vi.fn(),
}))

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })
}

function Harness() {
  const { loading, authenticated, logout } = useAuth()
  return (
    <div>
      <span data-testid="state">{loading ? "loading" : authenticated ? "in" : "out"}</span>
      <button onClick={() => void logout()}>退出登录</button>
    </div>
  )
}

function mount(session: Response | Error, logout: Response | Error) {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url === "/api/admin/session") {
      if (session instanceof Error) throw session
      return session
    }
    if (url === "/api/admin/logout" && (init?.method ?? "GET") === "POST") {
      if (logout instanceof Error) throw logout
      return logout
    }
    return json({}, 404)
  }))
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  client.setQueryData(["revealed-key"], "fg-secret")
  render(<QueryClientProvider client={client}><AuthProvider><Harness /></AuthProvider></QueryClientProvider>)
  return client
}

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
  vi.unstubAllGlobals()
})

describe("logout", () => {
  it("does not pretend the session ended when the gateway never confirmed it", async () => {
    // An offline browser or a blocked request never reached the gateway, so the
    // server-side session is still live. Showing the login screen here would
    // look like a logout while a reload walked straight back in.
    mount(json({ authenticated: true, csrf_token: "t" }), new TypeError("Failed to fetch"))
    await screen.findByText("in")

    fireEvent.click(screen.getByText("退出登录"))

    await waitFor(() => expect(notify).toHaveBeenCalledWith(expect.objectContaining({ title: "退出未完成" })))
    expect(screen.getByTestId("state").textContent).toBe("in")
  })

  it("leaves the login screen and drops cached credentials once the gateway confirms", async () => {
    const client = mount(json({ authenticated: true, csrf_token: "t" }), json({ ok: true }))
    await screen.findByText("in")

    fireEvent.click(screen.getByText("退出登录"))

    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("out"))
    // A revealed key used to survive in the query cache behind the login screen.
    expect(client.getQueryData(["revealed-key"])).toBeUndefined()
    expect(notify).not.toHaveBeenCalledWith(expect.objectContaining({ title: "退出未完成" }))
  })

  it("treats an already-expired session as a finished logout", async () => {
    const client = mount(json({ authenticated: true, csrf_token: "t" }), json({ error: { code: "unauthorized" } }, 401))
    await screen.findByText("in")

    fireEvent.click(screen.getByText("退出登录"))

    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("out"))
    expect(client.getQueryData(["revealed-key"])).toBeUndefined()
  })
})
