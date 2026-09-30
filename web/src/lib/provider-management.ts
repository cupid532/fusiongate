import type { QueryClient } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import { protocolMethod, protocolMethodPatch } from "@/lib/protocol-methods"
import type { IPPoolNode, Provider, ProviderGroup, ProviderKeySelectionMode } from "@/lib/types"

export function providerForm(provider: Provider | null) {
  return {
    name: provider?.name ?? "",
    type: provider?.type ?? "openai_compatible",
    baseURL: provider?.base_url ?? "",
    websiteURL: provider?.website_url ?? "",
    priority: provider?.priority ?? 1,
    max_concurrency: provider?.max_concurrency ?? 0,
    request_timeout_ms: provider?.request_timeout_ms ?? 120000,
    stream_start_timeout_ms: provider?.stream_start_timeout_ms ?? 0,
    stream_idle_timeout_ms: provider?.stream_idle_timeout_ms ?? 0,
    notes: provider?.notes ?? "",
    ip_pool_node_id: provider?.ip_pool_node_id ?? 0,
    group_id: provider?.group_id ?? 0,
    key_selection_mode: (provider?.key_selection_mode ?? "configured") as ProviderKeySelectionMode,
    health_check_enabled: provider?.health_check_enabled ?? true,
    protocol_method: protocolMethod(provider?.protocol_policy, provider?.protocol_preference),
  }
}

export type ProviderForm = ReturnType<typeof providerForm>
export type ProviderPatch = Partial<Omit<ProviderForm, "websiteURL" | "group_id" | "protocol_method">> & {
  website_url?: string
  group_id?: number
  clear_group?: boolean
  archived?: boolean
  enabled?: boolean
  protocol_policy?: string
  protocol_preference?: string
}

export function providerPatch(form: ProviderForm, baseline: ProviderForm | null, protocolChanged = false): ProviderPatch {
  const patch: ProviderPatch = {}
  for (const field of Object.keys(form) as Array<keyof ProviderForm>) {
    const value = typeof form[field] === "string" && ["name", "baseURL", "websiteURL"].includes(field) ? String(form[field]).trim() : form[field]
    if (baseline && value === baseline[field] && !(field === "protocol_method" && protocolChanged)) continue
    if (field === "websiteURL") patch.website_url = String(value)
    else if (field === "group_id") {
      if (form.group_id > 0) patch.group_id = form.group_id
      else if (baseline) patch.clear_group = true
    } else if (field === "protocol_method") Object.assign(patch, protocolMethodPatch(form.protocol_method))
    else Object.assign(patch, { [field]: value })
  }
  return patch
}

export function validateProviderForm(form: ProviderForm): Partial<Record<keyof ProviderForm, string>> {
  const errors: Partial<Record<keyof ProviderForm, string>> = {}
  if (!form.name.trim()) errors.name = "请输入渠道名称"
  for (const field of ["baseURL", "websiteURL"] as const) {
    if (field === "websiteURL" && !form[field].trim()) continue
    try {
      const url = new URL(form[field].trim())
      if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) throw new Error()
    } catch {
      errors[field] = "请输入不含凭据的完整 HTTP / HTTPS 地址"
    }
  }
  for (const field of ["priority", "max_concurrency", "request_timeout_ms", "stream_start_timeout_ms", "stream_idle_timeout_ms"] as const) {
    const minimum = field === "request_timeout_ms" ? 1000 : 0
    if (!Number.isSafeInteger(form[field]) || form[field] < minimum) errors[field] = `请输入不小于 ${minimum} 的整数`
  }
  return errors
}

export type ProviderBatchAction = "enable" | "disable" | "delete" | "egress"
export type BatchItemResult = { id: number; name: string; status: "success" | "error" | "unknown" | "skipped"; message?: string }

export function isUncertainError(error: unknown): boolean {
  return error instanceof TypeError || error instanceof Error && error.name === "AbortError" || error instanceof ApiError && error.code === "invalid_response"
}

export const providersApi = {
  list: (signal?: AbortSignal) => api<Provider[]>("/api/admin/providers", { signal }),
  patch: (id: number, patch: ProviderPatch) => api<{ protocol_discovery?: { status: string; error?: string } }>(`/api/admin/providers/${id}`, { method: "PATCH", body: JSON.stringify(patch) }),
  batch: (ids: number[], action: ProviderBatchAction, nodeId?: number) => api<{ affected: number }>("/api/admin/providers/batch", { method: "POST", body: JSON.stringify({ provider_ids: ids, action, ...(action === "egress" ? { ip_pool_node_id: nodeId } : {}) }) }),
  reorder: (ids: number[]) => api("/api/admin/providers/reorder", { method: "PATCH", body: JSON.stringify({ provider_ids: ids }) }),
  nodes: (signal?: AbortSignal) => api<IPPoolNode[]>("/api/admin/ip-pool", { signal }),
  groups: (signal?: AbortSignal) => api<ProviderGroup[]>("/api/admin/provider-groups", { signal }),
}

export async function refreshProviderViews(client: QueryClient) {
  await Promise.all(["providers", "provider-keys", "provider-groups", "ippool", "routes", "dashboard"].map((key) => client.invalidateQueries({ queryKey: [key] })))
}

export async function applySequential(items: Array<{ id: number; name: string }>, operation: (id: number) => Promise<unknown>, onProgress?: (results: BatchItemResult[]) => void): Promise<BatchItemResult[]> {
  const results: BatchItemResult[] = []
  for (const item of items) {
    try {
      await operation(item.id)
      results.push({ id: item.id, name: item.name, status: "success" })
    } catch (error) {
      const unknown = isUncertainError(error)
      results.push({ id: item.id, name: item.name, status: unknown ? "unknown" : "error", message: unknown ? "请求结果待核对，请刷新配置后再重试" : error instanceof Error ? error.message : "操作失败" })
      if (unknown || (error instanceof Error && "status" in error && [401, 403].includes(Number(error.status)))) {
        results.push(...items.slice(results.length).map((remaining) => ({ id: remaining.id, name: remaining.name, status: "skipped" as const, message: "后续操作已停止" })))
        onProgress?.([...results])
        return results
      }
    }
    onProgress?.([...results])
  }
  return results
}

export function applyProviderPatches(providers: Array<Pick<Provider, "id" | "name">>, patch: ProviderPatch, onProgress?: (results: BatchItemResult[]) => void) {
  return applySequential(providers, (id) => providersApi.patch(id, patch), onProgress)
}

export function providerStatus(provider: Provider) {
  if (provider.archived) return { label: "已归档", variant: "neutral" as const }
  if (!provider.enabled) return { label: "已停用", variant: "neutral" as const }
  if (provider.status === "circuit_open" || (provider.circuit_open_until && new Date(provider.circuit_open_until).getTime() > Date.now())) return { label: "熔断冷却", variant: "warning" as const }
  if (provider.health_check_status === "unhealthy") return { label: "检活失败", variant: "danger" as const }
  if (provider.consecutive_failures > 0) return { label: "不稳定", variant: "warning" as const }
  if (provider.health_check_status === "healthy" || provider.status === "healthy") return { label: "健康", variant: "success" as const }
  if (["pending", "running"].includes(provider.health_check_status)) return { label: "待检活", variant: "neutral" as const }
  return { label: "未检活", variant: "neutral" as const }
}
