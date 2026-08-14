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
