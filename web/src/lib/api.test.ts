import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, apiDownload, setCsrfToken } from "./api"

vi.mock("@/lib/notify", () => ({ reportUnauthorized: vi.fn() }))

function respond(body: string, contentType: string, init: ResponseInit = {}) {
  return new Response(body, { ...init, headers: { "content-type": contentType } })
}

afterEach(() => {
  vi.unstubAllGlobals()
  setCsrfToken("")
})

describe("apiDownload", () => {
  const json = { what: "渠道备份", mime: ["application/json"], json: true as const }

  it("hands back the payload and the server's file name", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"providers":[]}', {
      headers: {
        "content-type": "application/json; charset=utf-8",
        "content-disposition": 'attachment; filename="fusiongate-providers-20261003.json"',
      },
    })))
    const { blob, filename } = await apiDownload("/api/admin/providers/export", { method: "POST" }, json)
    expect(filename).toBe("fusiongate-providers-20261003.json")
    await expect(blob.text()).resolves.toBe('{"providers":[]}')
  })

  it("refuses a 200 login page instead of saving it as a backup", async () => {
    // Exactly what a reverse proxy or an expired session produces: an HTML page
    // with a success status. The old helper wrote it to disk as the backup.
    vi.stubGlobal("fetch", vi.fn(async () => respond("<html>please sign in</html>", "text/html")))
    await expect(apiDownload("/api/admin/providers/export", { method: "POST" }, json)).rejects.toMatchObject({
      name: "ApiError",
      status: 502,
      code: "unexpected_content_type",
    })
  })

  it("refuses an HTML body that claims to be JSON", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => respond("<html>please sign in</html>", "application/json")))
    await expect(apiDownload("/api/admin/providers/export", { method: "POST" }, json)).rejects.toMatchObject({
      code: "invalid_response",
    })
  })

  it("keeps accepting a CSV ledger export", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => respond("时间,模型\n2026-01-01,gpt\n", "text/csv; charset=utf-8")))
    const { blob } = await apiDownload("/api/admin/ledger/export", {}, { what: "请求账本 CSV", mime: ["text/csv"] })
    await expect(blob.text()).resolves.toContain("gpt")
  })

  it("still surfaces a real HTTP failure as its own status", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response('{"error":{"code":"database_error","message":"nope"}}', {
      status: 500,
      headers: { "content-type": "application/json" },
    })))
    const error = await apiDownload("/api/admin/providers/export", { method: "POST" }, json).catch((e) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(500)
    expect(error.code).toBe("database_error")
  })
})
