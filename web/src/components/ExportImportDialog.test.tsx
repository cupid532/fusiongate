import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { afterEach, describe, expect, it, vi } from "vitest"
import { ExportImportDialog } from "./ExportImportDialog"

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const invalidate = vi.spyOn(client, "invalidateQueries")
  const close = vi.fn()
  render(<QueryClientProvider client={client}><ExportImportDialog open onOpenChange={close} /></QueryClientProvider>)
  return { invalidate, close }
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe("provider backup feedback", () => {
  it("shows malformed JSON without sending an import", async () => {
    const fetch = vi.fn()
    vi.stubGlobal("fetch", fetch)
    show()
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "{invalid" } })
    fireEvent.click(screen.getAllByRole("button", { name: "导入" }).at(-1)!)
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "备份 JSON 格式不正确")
    expect(fetch).not.toHaveBeenCalled()
  })

  it("keeps import warnings visible and refreshes Key and node views", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ providers_created: 1, providers_updated: 0, warnings: ["出口节点不存在，已改为本机直连"] }), { status: 200, headers: { "content-type": "application/json" } })))
    const { invalidate, close } = show()
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "{}" } })
    fireEvent.click(screen.getAllByRole("button", { name: "导入" }).at(-1)!)
    expect(await screen.findByText("出口节点不存在，已改为本机直连")).toBeTruthy()
    expect(close).not.toHaveBeenCalled()
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["provider-keys"] }))
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["ippool"] })
  })

  it("does not download HTTP errors as backups", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: { code: "database_error", message: "备份读取失败" } }), { status: 500, headers: { "content-type": "application/json" } })))
    const createObjectURL = vi.fn()
    vi.stubGlobal("URL", class extends URL { static createObjectURL = createObjectURL })
    show()
    fireEvent.click(screen.getByRole("button", { name: "导出" }))
    fireEvent.click(screen.getByRole("button", { name: "下载备份" }))
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "备份读取失败")
    expect(createObjectURL).not.toHaveBeenCalled()
  })
})
