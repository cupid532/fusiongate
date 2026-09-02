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
    throw new ApiError(res.status, err?.code ?? "error", err?.message ?? res.statusText)
  }
  return data as T
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
