import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { Button } from "./button"
import { Input } from "./input"

afterEach(cleanup)

function renderInput(props: React.ComponentProps<typeof Input>) {
  return render(<Input aria-label="字段" {...props} />).getByLabelText("字段") as HTMLInputElement
}

describe("Input credential semantics", () => {
  it("keeps ordinary configuration out of password managers", () => {
    const input = renderInput({})
    expect(input.autocomplete).toBe("off")
    expect(input.getAttribute("data-1p-ignore")).toBe("true")
    expect(input.getAttribute("data-lpignore")).toBe("true")
  })

  it.each([
    "current-password",
    "new-password",
    "username",
    "email",
    "one-time-code",
    "webauthn",
  ])("recognises the auth autocomplete token %s", (token) => {
    const input = renderInput({ autoComplete: token })
    expect(input.autocomplete).toBe(token)
    expect(input.hasAttribute("data-1p-ignore")).toBe(false)
    expect(input.hasAttribute("data-lpignore")).toBe(false)
  })

  it("still protects a revealed password field chosen by autocomplete, not type", () => {
    // A password-visibility toggle switches type to text; the field must stay
    // an auth field because autocomplete, not masking, carries the intent.
    const input = renderInput({ type: "text", autoComplete: "new-password" })
    expect(input.autocomplete).toBe("new-password")
    expect(input.hasAttribute("data-1p-ignore")).toBe(false)
  })

  it("honours explicit passwordManager overrides in both directions", () => {
    const configSecret = renderInput({ passwordManager: true })
    expect(configSecret.hasAttribute("data-1p-ignore")).toBe(false)
    expect(configSecret.hasAttribute("data-lpignore")).toBe(false)

    cleanup()
    const optedOut = renderInput({ autoComplete: "current-password", passwordManager: false })
    expect(optedOut.autocomplete).toBe("current-password")
    expect(optedOut.getAttribute("data-1p-ignore")).toBe("true")
    expect(optedOut.getAttribute("data-lpignore")).toBe("true")
  })

  it("keeps the passwordManager helper out of the DOM", () => {
    const input = renderInput({ passwordManager: true })
    expect(input.hasAttribute("passwordmanager")).toBe(false)
    expect(input.hasAttribute("passwordManager")).toBe(false)
  })
})

describe("Button submit semantics", () => {
  it("does not let an action button submit the surrounding form by default", () => {
    const onSubmit = vi.fn((event: React.FormEvent) => event.preventDefault())
    render(
      <form onSubmit={onSubmit}>
        <Button>添加 Key</Button>
        <Button type="submit">保存渠道参数</Button>
      </form>,
    )
    fireEvent.click(screen.getByRole("button", { name: "添加 Key" }))
    expect(onSubmit).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole("button", { name: "保存渠道参数" }))
    expect(onSubmit).toHaveBeenCalledTimes(1)
  })
})
