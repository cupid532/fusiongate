import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { AuthOAuthDialog } from "./AuthOAuthDialog"
import { notifySuccess } from "@/lib/notify"

vi.mock("@/lib/notify", () => ({ notifySuccess: vi.fn(), reportUnauthorized: vi.fn() }))

const device = {
  session_id: "grok-session", flow: "device", user_code: "ABCD-EFGH", expires_in: 900, poll_interval: 5,
  verification_url: "https://accounts.x.ai/device", verification_url_complete: "https://accounts.x.ai/device?user_code=ABCD-EFGH",
}
const startPath = "/api/admin/auth/oauth/start"
const completePath = "/api/admin/auth/oauth/complete"
function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } })
}

function mount(fetcher: (path: string, init: RequestInit) => Response | Promise<Response>, defaultPlatform = "grok") {
  const fetchMock = vi.fn(async (path: string, init: RequestInit) => fetcher(path, init))
  vi.stubGlobal("fetch", fetchMock)
  const client = new QueryClient()
  const close = vi.fn()
  const view = render(<QueryClientProvider client={client}><AuthOAuthDialog defaultPlatform={defaultPlatform} onClose={close} /></QueryClientProvider>)
  return { fetchMock, close, client, unmount: view.unmount }
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.useRealTimers(); vi.clearAllMocks() })

describe("Grok device authorization", () => {
  it("shows the real verification link and code, then only reports success after save", async () => {
    vi.useFakeTimers()
    let polls = 0
    const { fetchMock, close, client } = mount((path) => {
      if (path === startPath) return json(device)
      if (++polls === 1) return json({ pending: true, retry_after: 8 }, 202)
      return json({ id: 19, model_sync: { status: "ok" } }, 201)
    })
    const invalidate = vi.spyOn(client, "invalidateQueries")
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "获取 xAI 设备码" })) })
    expect(JSON.parse(String(fetchMock.mock.calls[0][1].body))).toEqual({ platform: "grok" })
    expect(screen.getByRole("link", { name: "打开 xAI 授权页面" }).getAttribute("href")).toBe(device.verification_url_complete)
    expect(screen.getByText("ABCD-EFGH")).toBeTruthy()
    expect(screen.queryByLabelText(/回调地址/)).toBeNull()
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "我已授权，检查状态" })) })
    expect(fetchMock.mock.calls[1][0]).toBe(completePath)
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toEqual({ session_id: "grok-session", name: "Grok (xAI) 账号", priority: 1 })
    expect(screen.getByRole("status").textContent).toContain("正在等待 xAI")
    expect(close).not.toHaveBeenCalled()
    expect(notifySuccess).not.toHaveBeenCalled()
    expect(invalidate).not.toHaveBeenCalled()
    await act(async () => { await vi.advanceTimersByTimeAsync(7999) })
    expect(polls).toBe(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(1) })
    expect(polls).toBe(2)
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["providers"] })
    expect(notifySuccess).toHaveBeenCalledWith("授权凭据已保存", undefined)
    expect(close).toHaveBeenCalledOnce()
  })

  it("stops polls when the dialog is cancelled", async () => {
    vi.useFakeTimers()
    const { fetchMock, close, unmount } = mount((path) => path === startPath ? json(device) : json({ pending: true }, 202))
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "获取 xAI 设备码" })) })
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "我已授权，检查状态" })) })
    fireEvent.click(screen.getByRole("button", { name: "取消" }))
    expect(close).toHaveBeenCalledOnce()
    expect(fetchMock.mock.calls[1][1].signal?.aborted).toBe(true)
    unmount()
    await act(async () => { await vi.advanceTimersByTimeAsync(20_000) })
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(notifySuccess).not.toHaveBeenCalled()
  })

  it("allows starting over on denial and falls back to the trusted verification URL", async () => {
    const { close } = mount((path) => path === startPath
      ? json({ ...device, verification_url_complete: "https://accounts.x.ai.example.com/device" })
      : json({ error: { code: "oauth_access_denied", message: "denied" } }, 400))
    fireEvent.click(screen.getByRole("button", { name: "获取 xAI 设备码" }))
    expect((await screen.findByRole("link", { name: "打开 xAI 授权页面" })).getAttribute("href")).toBe(device.verification_url)
    fireEvent.click(screen.getByRole("button", { name: "我已授权，检查状态" }))
    expect((await screen.findByRole("alert")).textContent).toContain("拒绝授权")
    expect(screen.getByRole("button", { name: "获取 xAI 设备码" })).toBeTruthy()
    expect(close).not.toHaveBeenCalled()
  })

  it("does not report success when the response lacks a saved credential", async () => {
    const { close } = mount((path) => path === startPath ? json(device) : json({}))
    fireEvent.click(screen.getByRole("button", { name: "获取 xAI 设备码" }))
    fireEvent.click(await screen.findByRole("button", { name: "我已授权，检查状态" }))
    expect((await screen.findByRole("alert")).textContent).toContain("未确认凭据已保存")
    expect(close).not.toHaveBeenCalled()
    expect(notifySuccess).not.toHaveBeenCalled()
  })
})

describe("browser callback authorization", () => {
  it.each(["codex", "claude"])("retains the callback flow for %s", async (platform) => {
    const callback = "http://localhost:1455/auth/callback?code=test-code&state=session"
    const { fetchMock, close } = mount((path) => path === startPath
      ? json({ session_id: "session", expires_in: 900, auth_url: "https://example.com/authorize" })
      : json({ id: 11 }, 201), platform)
    fireEvent.click(screen.getByRole("button", { name: "生成授权地址" }))
    fireEvent.change(await screen.findByLabelText(/回调地址（授权后/), { target: { value: callback } })
    fireEvent.click(screen.getByRole("button", { name: "完成授权" }))
    await waitFor(() => expect(close).toHaveBeenCalledOnce())
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).toMatchObject({ session_id: "session", callback })
  })
})
