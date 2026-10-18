import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, expect, it } from "vitest"
import { ThemeProvider, useTheme } from "./theme"

function Probe() {
  const { theme, toggle } = useTheme()
  return <button onClick={toggle}>{theme}</button>
}

afterEach(() => {
  cleanup()
  localStorage.clear()
  document.documentElement.classList.remove("dark")
})

it("defaults the orbital console to dark without persisting an implicit preference", () => {
  render(<ThemeProvider><Probe /></ThemeProvider>)
  expect(screen.getByRole("button").textContent).toBe("dark")
  expect(document.documentElement.classList.contains("dark")).toBe(true)
  expect(localStorage.getItem("fusiongate-theme")).toBeNull()
})

it("preserves an explicit light choice and persists toggles", () => {
  localStorage.setItem("fusiongate-theme", "light")
  render(<ThemeProvider><Probe /></ThemeProvider>)
  expect(screen.getByRole("button").textContent).toBe("light")
  fireEvent.click(screen.getByRole("button"))
  expect(localStorage.getItem("fusiongate-theme")).toBe("dark")
  expect(document.documentElement.classList.contains("dark")).toBe(true)
})

it("uses the pre-paint theme class as the first-render source of truth", () => {
  document.documentElement.classList.add("dark")
  render(<ThemeProvider><Probe /></ThemeProvider>)
  expect(screen.getByRole("button").textContent).toBe("dark")
})
