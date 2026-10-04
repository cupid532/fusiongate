import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { Login } from "./Login"

const login = vi.fn()
vi.mock("@/providers/auth", () => ({ useAuth: () => ({ login }) }))
afterEach(() => { cleanup(); vi.resetAllMocks() })

describe("administrator login credential semantics", () => {
  it("keeps the real login recognizable without ignoring password managers", async () => {
    login.mockResolvedValue(undefined)
    render(<Login />)
    const input = screen.getByLabelText("管理员密码") as HTMLInputElement
    expect(input.type).toBe("password")
    expect(input.name).toBe("password")
    expect(input.autocomplete).toBe("current-password")
    expect(input.getAttribute("data-1p-ignore")).not.toBe("true")
    expect(input.getAttribute("data-lpignore")).not.toBe("true")
    const form = input.closest("form")!
    expect(form.id).toBe("fusiongate-admin-login")
    expect(form.getAttribute("autocomplete")).toBe("on")
    expect((screen.getByRole("button", { name: "进入控制台" }) as HTMLButtonElement).type).toBe("submit")
    fireEvent.change(input, { target: { value: "fixture-admin-password" } })
    fireEvent.submit(form)
    await waitFor(() => expect(login).toHaveBeenCalledWith("fixture-admin-password"))
  })
})
