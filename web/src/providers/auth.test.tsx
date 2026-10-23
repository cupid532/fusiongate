import { useState } from "react"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { AuthProvider, useAuth } from "./auth"
import { api, getCsrfToken, setCsrfToken } from "@/lib/api"
import { notify } from "@/lib/notify"

vi.mock("@/lib/notify", async (importOriginal) => ({
  ...await importOriginal<typeof import("@/lib/notify")>(),
  notify: vi.fn(),
}))

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })
}

function Harness() {
  const { loading, authenticated, logout } = useAuth()
  const [error, setError] = useState("")
  async function changePassword() {
    try {
      await api("/api/admin/password", { method: "POST", body: "{}" })
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "request failed")
    }
  }
  return (
    <div>
      <span data-testid="state">{loading ? "loading" : authenticated ? "in" : "out"}</span>
      <button onClick={() => void logout()}>退出登录</button>
      <button onClick={() => void changePassword()}>修改密码</button>
      {error && <p role="alert">{error}</p>}
    </div>
  )
}

function mount(session: Response | Error, logout: Response | Error, password?: Response) {
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
    if (url === "/api/admin/password" && password) return password
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
  setCsrfToken("")
})

describe("action-level authorization errors", () => {
  it("keeps the session, CSRF token and cached data after a wrong current password", async () => {
    const client = mount(
      json({ authenticated: true, csrf_token: "t" }),
      json({ ok: true }),
      json({ error: { code: "invalid_credentials", message: "current password is incorrect" } }, 403),
    )
    await screen.findByText("in")
    client.setQueryData(["revealed-key"], "cached-test-value")

    fireEvent.click(screen.getByText("修改密码"))

    expect((await screen.findByRole("alert")).textContent).toBe("current password is incorrect")
    expect(screen.getByTestId("state").textContent).toBe("in")
    expect(getCsrfToken()).toBe("t")
    expect(client.getQueryData(["revealed-key"])).toBe("cached-test-value")
    expect(notify).not.toHaveBeenCalledWith(expect.objectContaining({ title: "登录状态已失效" }))
  })
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

  it("warns that logout is unconfirmed when stale CSRF triggers reauthentication", async () => {
    mount(json({ authenticated: true, csrf_token: "stale" }), json({ error: { code: "csrf_failed", message: "missing or invalid CSRF token" } }, 403))
    await screen.findByText("in")

    fireEvent.click(screen.getByText("退出登录"))

    // The shared cookie may still be valid: middleware rejected this request
    // before the logout handler ran, even though the UI now needs a fresh login.
    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("out"))
    expect(getCsrfToken()).toBe("")
    expect(notify).toHaveBeenCalledWith(expect.objectContaining({ title: "登录状态已失效" }))
    expect(notify).toHaveBeenCalledWith(expect.objectContaining({ title: "退出未完成", description: expect.stringContaining("当前会话可能仍然有效") }))
  })

  it("treats an already-expired session as a finished logout", async () => {
    const client = mount(json({ authenticated: true, csrf_token: "t" }), json({ error: { code: "unauthorized" } }, 401))
    await screen.findByText("in")

    fireEvent.click(screen.getByText("退出登录"))

    await waitFor(() => expect(screen.getByTestId("state").textContent).toBe("out"))
    expect(client.getQueryData(["revealed-key"])).toBeUndefined()
    expect(notify).not.toHaveBeenCalledWith(expect.objectContaining({ title: "退出未完成" }))
  })
})
