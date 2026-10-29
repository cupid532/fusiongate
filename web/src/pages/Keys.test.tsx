import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Keys } from "./Keys"
import { ConfirmProvider } from "@/components/ui/confirm"

vi.mock("@formkit/auto-animate/react", () => ({ useAutoAnimate: () => [undefined] }))

function json(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "content-type": "application/json" } })
}

function mount(fetcher: (path: string, options?: RequestInit) => Promise<Response> = vi.fn(async () => json([]))) {
  vi.stubGlobal("fetch", fetcher)
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <ConfirmProvider>
        <Keys />
      </ConfirmProvider>
    </QueryClientProvider>,
  )
  return client
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe("API key expiry", () => {
  it("refuses to create a key whose expiry contradicts the form", async () => {
    // "永不过期" off with no date used to submit a body without `expires_at`,
    // leaving the real lifetime to whatever the gateway defaults to.
    mount()
    fireEvent.click(await screen.findByRole("button", { name: "创建密钥" }))

    const name = document.querySelector<HTMLInputElement>('input[placeholder="例如：my-client"]') ?? screen.getAllByRole("textbox")[0] as HTMLInputElement
    fireEvent.change(name, { target: { value: "client-a" } })

    const never = screen.getByRole("checkbox", { name: "永不过期" }) as HTMLInputElement
    const submit = screen.getByRole("button", { name: "创建" }) as HTMLButtonElement
    expect(never.checked).toBe(true)
    expect(submit.disabled).toBe(false)

    fireEvent.click(never)

    expect(screen.getByText(/请选择过期时间，或勾选“永不过期”/)).toBeTruthy()
    expect(submit.disabled).toBe(true)

    const when = document.querySelector<HTMLInputElement>('input[type="datetime-local"]')!
    fireEvent.change(when, { target: { value: "2027-01-01T00:00" } })

    expect(screen.queryByText(/请选择过期时间/)).toBeNull()
    expect(submit.disabled).toBe(false)
  })
})

const keyList = Array.from({ length: 40 }, (_, i) => ({
  id: i + 1, name: `client-${i + 1}`, prefix: `fg-${i + 1}`, revoked: false,
  allow_all: true, budget_micros: 0, spent_micros: 0, rpm_limit: 120,
}))

describe("API key reveal dialog", () => {
  it("shows the selected key in a portal, copies it, clears it on close and restores focus without scrolling", async () => {
    const writeText = vi.fn(async () => {})
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } })
    const focus = vi.spyOn(HTMLElement.prototype, "focus")
    const fetcher = vi.fn(async (path: string) => path.endsWith("/reveal")
      ? json({ key: path.includes("/40/") ? "fg-test-forty" : "fg-test-one" }) : json(keyList))
    const client = mount(fetcher)
    const trigger = await screen.findByRole("button", { name: "查看 client-40" })
    fireEvent.click(trigger)

    const dialog = await screen.findByRole("dialog", { name: "完整访问密钥" })
    expect(within(dialog).getByText(/密钥名称：client-40/)).toBeTruthy()
    expect(within(dialog).getByText("fg-test-forty")).toBeTruthy()
    expect(dialog.closest("table")).toBeNull()
    expect(dialog.closest(".fixed")).toBeTruthy()
    fireEvent.click(within(dialog).getByRole("button", { name: "复制密钥" }))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("fg-test-forty"))
    fireEvent.click(within(dialog).getByRole("button", { name: "完成" }))
    await waitFor(() => expect(screen.queryByText("fg-test-forty")).toBeNull())
    await waitFor(() => expect(document.activeElement).toBe(trigger))
    expect(focus).toHaveBeenCalledWith({ preventScroll: true })
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0))

    fireEvent.click(screen.getByRole("button", { name: "查看 client-1" }))
    const next = await screen.findByRole("dialog")
    expect(within(next).getByText("fg-test-one")).toBeTruthy()
    expect(screen.queryByText("fg-test-forty")).toBeNull()
    fireEvent.click(within(next).getByRole("button", { name: "关闭" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
  })

  it("prevents duplicate reveal requests while loading and lets the operator retry a failure", async () => {
    let finish!: (response: Response) => void
    const fetcher = vi.fn(async (path: string) => path.endsWith("/reveal")
      ? new Promise<Response>((resolve) => { finish = resolve }) : json(keyList))
    mount(fetcher)
    const trigger = await screen.findByRole("button", { name: "查看 client-40" }) as HTMLButtonElement
    fireEvent.click(trigger)
    await waitFor(() => expect(trigger.disabled).toBe(true))
    fireEvent.click(trigger)
    expect(fetcher.mock.calls.filter(([path]) => path.endsWith("/reveal"))).toHaveLength(1)
    finish(json({ error: { message: "暂时无法查看" } }, 500))
    await waitFor(() => expect(trigger.disabled).toBe(false))
    expect(screen.queryByRole("dialog")).toBeNull()
    fireEvent.click(trigger)
    await waitFor(() => expect(fetcher.mock.calls.filter(([path]) => path.endsWith("/reveal"))).toHaveLength(2))
    finish(json({ key: "fg-retry-test" }))
    expect(await screen.findByText("fg-retry-test")).toBeTruthy()
  })

  it("opens the same dialog for a newly created key and discards its mutation data on close", async () => {
    const fetcher = vi.fn(async (path: string, options?: RequestInit) =>
      path === "/api/admin/keys" && options?.method === "POST"
        ? json({ id: 41, key: "fg-created-test" }) : json(keyList))
    const client = mount(fetcher)
    fireEvent.click(screen.getByRole("button", { name: "创建密钥" }))
    fireEvent.change(screen.getByPlaceholderText("例如：Hermes local"), { target: { value: "new-client" } })
    fireEvent.click(screen.getByRole("button", { name: "创建" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByText(/密钥名称：new-client/)).toBeTruthy()
    expect(within(dialog).getByText("fg-created-test")).toBeTruthy()
    fireEvent.click(within(dialog).getByRole("button", { name: "完成" }))
    await waitFor(() => expect(screen.queryByText("fg-created-test")).toBeNull())
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0))
  })
})
