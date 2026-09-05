/**
 * Human-readable Chinese for the manual health-check API.
 *
 * The backend speaks English in its status codes and reason strings (they are
 * stable identifiers as much as messages). The console used to print them
 * verbatim, so a failed check read as one line of English jargon. Everything
 * the operator can act on is translated here; anything unknown falls through
 * unchanged rather than being hidden.
 *
 * Keep the reason strings in sync with the constants in
 * internal/fusiongate/health_check_jobs.go.
 */

const REASONS: Record<string, string> = {
  "no enabled API key has health checks turned on": "没有启用了检活的 Key",
  "none of the selected API keys are enabled for health checks": "所选 Key 均未启用检活",
  "this model is switched off on every API key; enable it in model management": "该模型在所有 Key 上都处于关闭状态，请在「模型管理」中开启",
  "no API key lists this model; discover models for a key or add it in model management": "没有任何 Key 的模型清单包含该模型，请先识别模型或在「模型管理」中添加",
  "API key is disabled": "Key 已停用",
  "health checks are off for this API key": "该 Key 已关闭检活",
  "model is switched off on this API key": "该 Key 上此模型已关闭",
  "this API key does not list the model": "该 Key 的模型清单不含此模型",
  "only chat models support generation health checks": "仅对话模型支持生成式检活",
  "health check cancelled": "已取消",
  "not started because the health check was cancelled": "任务取消，未开始",
  "request timeout": "请求超时",
  "model returned an unexpected answer": "模型返回了错误答案",
  "authentication failed": "认证失败",
  "failed to load provider": "渠道加载失败",
  "health checks disabled for this provider": "该渠道已关闭检活",
  "health checks disabled for this API key": "该 Key 已关闭检活",
  "model route no longer exists": "模型路由已不存在",
  "selected API key does not support this model": "所选 Key 不支持该模型",
  "could not read generation response": "无法读取响应内容",
  "generation response was not valid JSON": "响应不是合法 JSON",
  "generation response contained no assistant text": "响应中没有模型文本",
}

/** Errors returned when *starting* a job, keyed by the API's error code. */
const START_ERRORS: Record<string, string> = {
  health_check_running: "已有一项检活正在进行，请等待其完成或先取消",
  health_check_unavailable: "检活服务暂不可用",
}

const START_MESSAGES: Array<[RegExp, string]> = [
  [/disabled providers cannot be health checked/i, "渠道已停用，无法检活"],
  [/health checks are disabled for one or more providers/i, "渠道已关闭检活，请先在渠道设置中开启"],
  [/one or more providers do not support health checks/i, "该类型渠道不支持检活"],
  [/one or more providers no longer exist/i, "渠道已不存在"],
  [/the selected providers have no enabled models/i, "渠道没有启用的模型，请先在「模型管理」中添加"],
  [/one or more selected models are disabled, missing/i, "所选模型已停用或不属于该渠道"],
  [/one or more selected keys are disabled, missing, or do not support/i, "所选 Key 已停用或不支持所选模型"],
  [/select between 1 and \d+ providers/i, "请选择至少一个渠道"],
  [/select between 1 and \d+ models/i, "请至少勾选一个模型"],
  [/health check expands to more than (\d+)/i, "本次检活展开的组合数超过上限，请缩小范围"],
]

export function describeHealthReason(reason: string | undefined): string {
  const text = (reason ?? "").trim()
  if (!text) return ""
  return REASONS[text] ?? text
}

export function describeHealthStartError(error: unknown): string {
  const code = (error as { code?: string } | null)?.code
  if (code && START_ERRORS[code]) return START_ERRORS[code]
  const message = error instanceof Error ? error.message : String(error ?? "")
  for (const [pattern, text] of START_MESSAGES) {
    if (pattern.test(message)) return text
  }
  return message || "检活启动失败"
}

const STATUS: Record<string, { label: string; tone: "success" | "danger" | "warning" | "neutral" }> = {
  healthy: { label: "健康", tone: "success" },
  unhealthy: { label: "不健康", tone: "danger" },
  timeout: { label: "超时", tone: "danger" },
  network_error: { label: "网络错误", tone: "danger" },
  auth_expired: { label: "认证失效", tone: "danger" },
  auth_error: { label: "认证失败", tone: "danger" },
  rate_limited: { label: "被限流", tone: "warning" },
  server_error: { label: "上游错误", tone: "danger" },
  invalid_response: { label: "响应异常", tone: "danger" },
  content_mismatch: { label: "答案错误", tone: "danger" },
  config_error: { label: "配置错误", tone: "danger" },
  unsupported: { label: "不支持", tone: "neutral" },
  disabled: { label: "已关闭", tone: "neutral" },
  skipped: { label: "已跳过", tone: "neutral" },
  cancelled: { label: "已取消", tone: "neutral" },
  queued: { label: "排队中", tone: "warning" },
  running: { label: "检测中", tone: "warning" },
}

export function describeHealthStatus(status: string): { label: string; tone: "success" | "danger" | "warning" | "neutral" } {
  return STATUS[status] ?? { label: status || "未知", tone: "neutral" }
}
