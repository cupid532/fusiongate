import { reportUnauthorized } from "@/lib/notify"

let csrfToken: string | null = null

export function setCsrfToken(token: string) {
  csrfToken = token
}

export function getCsrfToken(): string {
  return csrfToken ?? ""
}

export class ApiError extends Error {
  status: number
  code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.code = code
  }
}

/** True when the error means "your session is gone", not "this request was bad". */
export function isAuthError(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 401 || error.status === 403)
}

export async function api<T = unknown>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  const method = (options.method || "GET").toUpperCase()
  if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS" && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken)
  }
  if (options.body != null && typeof options.body === "string") {
    headers.set("Content-Type", "application/json")
  }
  const res = await fetch(path, { ...options, method, headers })
  const isJson = res.headers.get("content-type")?.includes("application/json")
  let data: unknown = null
  if (isJson) {
    const text = await res.text()
    try {
      data = text ? JSON.parse(text) : null
    } catch {
      if (res.status === 401 || res.status === 403) reportUnauthorized()
      throw new ApiError(res.ok ? 502 : res.status, "invalid_response", `网关返回了无法解析的数据（HTTP ${res.status}），请刷新核对操作结果`)
    }
  } else if (res.ok && res.status !== 204) {
    throw new ApiError(502, "invalid_response", "网关未返回预期的 JSON 数据，请检查连接并刷新核对操作结果")
  }
  if (!res.ok) {
    const err = (data as { error?: { message?: string; code?: string } })?.error
    const apiError = new ApiError(res.status, err?.code ?? "error", err?.message ?? res.statusText)
    // A 401 means the server forgot our session (restart or 12h expiry); a 403
    // here is a CSRF rejection, which happens for the same reason — the token
    // we hold belongs to a session that no longer exists. Either way the only
    // recovery is to sign in again, so surface it instead of failing silently.
    if (apiError.status === 401 || apiError.status === 403) reportUnauthorized()
    throw apiError
  }
  return data as T
}

/**
 * What a download endpoint is expected to answer with.
 *
 * A reverse proxy or an expired session can turn a download into `200 OK` and an
 * HTML login page, and the old helper wrote that straight to disk under the
 * backup's own filename — a "successful" export that could not be restored. Each
 * caller therefore declares the shape it expects, and a mismatch throws instead
 * of silently producing an unusable file.
 */
export type DownloadExpectation = {
  /** Human label used in the error message, e.g. 「渠道备份」. */
  what: string
  /** Acceptable `Content-Type` fragments, lower-case. Empty means "any". */
  mime: string[]
  /** When true the body must also parse as JSON before it is handed back. */
  json?: boolean
}

/** A validated download: the payload plus the server's suggested file name. */
export type DownloadedFile = {
  blob: Blob
  /** `Content-Disposition` file name, when the server sent a usable one. */
  filename: string | null
}

/** Read a `Content-Disposition` attachment name, preferring the RFC 5987 form. */
function filenameFromDisposition(header: string | null): string | null {
  if (!header) return null
  const encoded = /filename\*=UTF-8''([^;]+)/i.exec(header)
  if (encoded) {
    try {
      const decoded = decodeURIComponent(encoded[1].trim())
      if (decoded) return decoded
    } catch {
      // Malformed percent-encoding; fall through to the plain form.
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(header)
  return plain ? plain[1].trim() || null : null
}

/**
 * Fetch a file download and hand back the blob.
 *
 * The naive version of this checked neither `res.ok` nor the content type, so a
 * 401 or 500 was cheerfully written to the user's disk as a `.json` file full of
 * error text. Anything that isn't a real success now throws instead.
 */
export async function apiDownload(path: string, options: RequestInit, expected: DownloadExpectation): Promise<DownloadedFile> {
  const headers = new Headers(options.headers)
  const method = (options.method || "GET").toUpperCase()
  if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS") {
    headers.set("X-CSRF-Token", getCsrfToken())
  }
  if (options.body != null && typeof options.body === "string") {
    headers.set("Content-Type", "application/json")
  }
  const res = await fetch(path, { ...options, method, headers })
  if (!res.ok) {
    let message = res.statusText
    let code = "error"
    try {
      const body = await res.json()
      message = body?.error?.message ?? message
      code = body?.error?.code ?? code
    } catch {
      // Non-JSON error body; keep the status text.
    }
    const apiError = new ApiError(res.status, code, message)
    if (apiError.status === 401 || apiError.status === 403) reportUnauthorized()
    throw apiError
  }
  const contentType = (res.headers.get("content-type") ?? "").toLowerCase()
  if (expected.mime.length > 0 && !expected.mime.some((fragment) => contentType.includes(fragment))) {
    throw new ApiError(
      502,
      "unexpected_content_type",
      `网关没有返回${expected.what}（收到 ${contentType || "未知类型"}）。可能被登录页或反向代理拦截，请刷新后重试。`
    )
  }
  const filename = filenameFromDisposition(res.headers.get("content-disposition"))
  if (expected.json) {
    const text = await res.text()
    try {
      JSON.parse(text)
    } catch {
      throw new ApiError(502, "invalid_response", `网关返回的${expected.what}无法解析，请刷新核对后再试。`)
    }
    return { blob: new Blob([text], { type: contentType || "application/json" }), filename }
  }
  return { blob: await res.blob(), filename }
}

/**
 * Save a blob to the user's disk.
 *
 * The object URL is revoked on a timer rather than immediately after `click()`:
 * revoking synchronously can cancel a download that hasn't started yet. The
 * anchor is also attached to the document, which Firefox requires.
 */
export function saveBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  a.style.display = "none"
  document.body.appendChild(a)
  a.click()
  window.setTimeout(() => {
    a.remove()
    URL.revokeObjectURL(url)
  }, 10_000)
}

export const providerKeysApi = {
  list: (providerId: number, signal?: AbortSignal) => api<import("@/lib/types").ProviderKey[]>(`/api/admin/providers/${providerId}/keys`, { signal }),
  create: (providerId: number, body: Record<string, unknown>) => api(`/api/admin/providers/${providerId}/keys`, { method: "POST", body: JSON.stringify(body) }),
  patch: (providerId: number, keyId: number, body: Record<string, unknown>) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "PATCH", body: JSON.stringify(body) }),
  remove: (providerId: number, keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: "DELETE" }),
  test: (providerId: number, keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}/test`, { method: "POST" }),
  discover: (providerId: number, keyId: number) => api(`/api/admin/providers/${providerId}/keys/${keyId}/discover-models`, { method: "POST" }),
}

export const providerModelsApi = {
  listKeys: providerKeysApi.list,
  patchKey: providerKeysApi.patch,
  discover: providerKeysApi.discover,
  saveManagement: (providerId: number, keys: Array<Record<string, unknown>>) => api<{ keys: Array<{ key_id: number; status: string; error?: string }> }>(`/api/admin/providers/${providerId}/model-management`, { method: "PATCH", body: JSON.stringify({ keys }) }),
}

export const healthChecksApi = {
  /** Routes × keys a manual check of this provider would probe, with reasons for the ones it cannot. */
  preview: (providerId: number) => api<import("@/lib/types").HealthCheckPreview>(`/api/admin/providers/${providerId}/health-check-targets`),
  /** The job currently occupying the single manual-check slot, if any. */
  active: () => api<{ active: boolean; job?: import("@/lib/types").HealthCheckJob }>("/api/admin/health-checks"),
  start: (body: { provider_ids: number[]; model_scope: "all" | "selected"; route_ids?: number[]; provider_key_ids?: number[] }) =>
    api<import("@/lib/types").HealthCheckJob>("/api/admin/health-checks", { method: "POST", body: JSON.stringify(body) }),
  get: (jobId: string) => api<import("@/lib/types").HealthCheckJob>(`/api/admin/health-checks/${jobId}`),
  cancel: (jobId: string) => api<import("@/lib/types").HealthCheckJob>(`/api/admin/health-checks/${jobId}`, { method: "DELETE" }),
}
