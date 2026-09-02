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
  const data = isJson ? await res.json() : null
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
 * Fetch a file download and hand back the blob.
 *
 * The naive version of this checked neither `res.ok` nor the content type, so a
 * 401 or 500 was cheerfully written to the user's disk as a `.json` file full of
 * error text. Anything that isn't a real success now throws instead.
 */
export async function apiDownload(path: string, options: RequestInit = {}): Promise<Blob> {
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
  return res.blob()
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
  list: (providerId: number) => api<import("@/lib/types").ProviderKey[]>(`/api/admin/providers/${providerId}/keys`),
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
