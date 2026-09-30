import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, api } from "@/lib/api"
import { applyProviderPatches, providerForm, providerPatch, validateProviderForm, type BatchItemResult } from "@/lib/provider-management"
import type { Provider } from "@/lib/types"

afterEach(() => vi.unstubAllGlobals())

describe("channel patch contracts", () => {
  const baseline = providerForm({ name: "Channel", type: "openai_compatible", base_url: "https://upstream.example/v1", ip_pool_node_id: 8, group_id: 3 } as Provider)
  it("does not submit untouched connection or egress fields", () => {
    expect(providerPatch({ ...baseline, notes: "Updated" }, baseline)).toEqual({ notes: "Updated" })
  })
  it("uses zero, not null, to clear channel egress", () => {
    expect(providerPatch({ ...baseline, ip_pool_node_id: 0 }, baseline)).toEqual({ ip_pool_node_id: 0 })
  })
  it("clears a group explicitly and serializes new protocols", () => {
    expect(providerPatch({ ...baseline, group_id: 0, protocol_method: "messages" }, baseline)).toEqual({ clear_group: true, protocol_policy: "fixed", protocol_preference: "messages" })
  })
  it("validates numeric limits and disallows credential-bearing or non-HTTP URLs", () => {
    expect(validateProviderForm({ ...baseline, request_timeout_ms: 999, priority: -1, baseURL: "ftp://upstream.example", websiteURL: "https://name:password@upstream.example" })).toMatchObject({ request_timeout_ms: expect.any(String), priority: expect.any(String), baseURL: expect.any(String), websiteURL: expect.any(String) })
    expect(validateProviderForm(baseline)).toEqual({})
  })
})

describe("sequential front-end batch results", () => {
  const providers = [{ id: 1, name: "First" }, { id: 2, name: "Second" }, { id: 3, name: "Third" }]
  it("retains successes and continues after a definite item error", async () => {
    const request = vi.fn(async (path: string) => new Response(JSON.stringify(path.endsWith("/2") ? { error: { code: "invalid_request", message: "Rejected" } } : {}), { status: path.endsWith("/2") ? 400 : 200, headers: { "content-type": "application/json" } }))
    vi.stubGlobal("fetch", request)
    const progress: BatchItemResult[][] = []
    const results = await applyProviderPatches(providers, { priority: 7 }, (items) => progress.push(items))
    expect(results.map((item) => item.status)).toEqual(["success", "error", "success"])
    expect(request).toHaveBeenCalledTimes(3)
    expect(progress.at(-1)).toEqual(results)
  })
  it("stops after an uncertain network failure and does not replay the write", async () => {
    const request = vi.fn().mockRejectedValue(new TypeError("Disconnected"))
    vi.stubGlobal("fetch", request)
    const results = await applyProviderPatches(providers, { archived: true })
    expect(results.map((item) => item.status)).toEqual(["unknown", "skipped", "skipped"])
    expect(request).toHaveBeenCalledTimes(1)
  })
  it("treats a malformed success response as uncertain rather than a definite failure", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not-json", { status: 200, headers: { "content-type": "application/json" } })))
    expect((await applyProviderPatches(providers, { priority: 4 })).map((item) => item.status)).toEqual(["unknown", "skipped", "skipped"])
  })
})

describe("admin response validation", () => {
  it("rejects a successful HTML response rather than treating it as empty data", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("<html>proxy</html>", { status: 200, headers: { "content-type": "text/html" } })))
    await expect(api("/api/admin/providers")).rejects.toMatchObject({ code: "invalid_response" })
  })
  it("keeps a JSON authorization status", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: { code: "unauthorized", message: "Please login" } }), { status: 401, headers: { "content-type": "application/json" } })))
    await expect(api("/api/admin/providers")).rejects.toBeInstanceOf(ApiError)
    await expect(api("/api/admin/providers")).rejects.toMatchObject({ status: 401 })
  })
})
