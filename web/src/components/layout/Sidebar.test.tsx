import { cleanup, render, screen } from "@testing-library/react"
import { afterEach, expect, it, vi } from "vitest"
import { Sidebar, isPage } from "./Sidebar"

vi.mock("@/providers/theme", () => ({ useTheme: () => ({ theme: "light", toggle: vi.fn() }) }))
afterEach(cleanup)

it("keeps field compatibility out of the main navigation", () => {
  render(<Sidebar page="settings" onNavigate={vi.fn()} open={false} onClose={vi.fn()} />)
  expect(screen.queryByRole("button", { name: "字段兼容" })).toBeNull()
  expect(screen.getByRole("button", { name: "系统设置" }).getAttribute("aria-current")).toBe("page")
  expect(isPage("capabilities")).toBe(false)
  expect(isPage("settings")).toBe(true)
})
