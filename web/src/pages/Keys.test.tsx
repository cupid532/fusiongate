import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Keys } from "./Keys"
import { ConfirmProvider } from "@/components/ui/confirm"

vi.mock("@formkit/auto-animate/react", () => ({ useAutoAnimate: () => [undefined] }))

function mount() {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify([]), { headers: { "content-type": "application/json" } })))
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ConfirmProvider>
        <Keys />
      </ConfirmProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

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
