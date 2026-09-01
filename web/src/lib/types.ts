// —— 后端 /api/admin/* 的类型定义 ——

export interface DashboardData {
  providers: number
  models: number
  keys: number
  requests: number
  today_requests: number
  failures_24h: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  reasoning_tokens: number
  total_tokens: number
  cost_micros: number
}

export interface Provider {
  id: number
  name: string
  type: string
  base_url: string
  credential_hint: string
  auth_kind: string
  auth_source: string
  auth_email?: string
  auth_account_id?: string
  auth_expires_at?: string
  auth_status: string
  has_refresh_token: boolean
  status: string
  notes: string
  enabled: boolean
  archived: boolean
  priority: number
  sort_order: number
  weight: number
  passthrough_mode: string
  client_policy: string
  health_check_enabled: boolean
  max_concurrency: number
  request_timeout_ms: number
  failure_threshold: number
  cooldown_seconds: number
  consecutive_failures: number
  circuit_open_until?: string
  last_error?: string
  last_latency_ms: number
  last_first_byte_ms: number
  last_success_at?: string
  last_failure_at?: string
  inflight: number
  model_count: number
  group_id?: number
  group_sort_order: number
  last_health_check_at?: string
  health_check_status: string
  health_check_error?: string
  health_check_latency_ms: number
  health_check_mode: string
  health_check_first_byte_ms: number
  health_check_model?: string
  health_check_model_count: number
  health_score: number
  manual_balance_micros?: number
  balance_baseline_at?: string
  balance_multiplier_openai: number
  balance_multiplier_claude: number
  balance_multiplier_grok: number
  balance_multiplier_gemini: number
  balance_multiplier_other: number
  ip_pool_node_id?: number
  ip_pool_node_name?: string
  ip_pool_node_protocol?: string
  default_model?: string
  api_key_count: number
  enabled_api_key_count: number
}

export interface Route {
  id: number
  provider_id: number
  public_name: string
  upstream_model: string
  capabilities: string
  enabled: boolean
  priority: number
  input_price_micros: number
  cached_price_micros: number
  output_price_micros: number
  long_context_threshold: number
  long_input_price_micros: number
  long_cached_price_micros: number
  long_output_price_micros: number
  pricing_source?: string
  pricing_updated_at?: string
  provider_name?: string
  provider_type?: string
  provider_enabled: boolean
  provider_archived: boolean
  provider_priority: number
  provider_sort_order: number
  provider_circuit_open_until?: string
  sort_order: number
  provider_status?: string
  provider_latency_ms: number
  provider_first_byte_ms: number
  provider_failures: number
  provider_inflight: number
  health_score: number
  last_health_check_at?: string
  health_check_status: string
  health_check_error?: string
  health_check_latency_ms: number
  health_check_first_byte_ms: number
}

export interface ModelAlias {
  alias: string
  target_model: string
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface PricingStatus {
  status: Record<string, string>
  interval: string
  sources: string[]
}

export interface PricingSyncResult {
  sources: number
  models: number
  updated_routes: number
  synced_at: string
  errors?: string[]
}

export interface APIKey {
  id: number
  name: string
  prefix: string
  allow_models: string
  deny_models: string
  allow_all: boolean
  allow_images: boolean
  allow_audio: boolean
  revoked: boolean
  rpm_limit: number
  expires_at?: string
  created_at: string
  can_reveal: boolean
  budget_micros: number
  spent_micros: number
  remaining_micros: number
}

export interface RequestLedgerRow {
  id: number
  request_id: string
  gateway_request_id: string
  attempt: number
  retry_reason: string
  provider_name: string
  provider_key_id: number
  provider_key_name: string
  provider_key_hint: string
  client_ip: string
  created_at: string
  completed_at: string
  running: boolean
  first_byte_ms: number | null
  model: string
  upstream_model: string
  protocol: string
  stream: boolean
  success: boolean
  status_code: number
  error_type: string
  latency_ms: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  reasoning_tokens: number
  total_tokens: number
  cost_micros: number
  cost_type: string
  usage_reported: boolean
  reasoning_effort: string
  stale?: boolean
}

export interface RequestLedgerPayload {
  items: RequestLedgerRow[]
  count: number
  server_now: string
}

export interface TokenUsageMetrics {
  requests: number
  attempts: number
  successful_requests: number
  reported_requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  reasoning_tokens: number
  total_tokens: number
  cost_micros: number
  priced_attempts: number
  usage_coverage: number
  cost_coverage: number
}

export interface TokenUsageHeatmapCell {
  model: string
  upstream_model?: string
  date: string
  requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  reasoning_tokens: number
  total_tokens: number
  cost_micros: number
}

export interface LedgerStatus {
  max_mb: number
  used_mb: number
  est_bytes: number
  rows: number
  capped: boolean
}

export interface TokenUsageResponse {
  period: { days: number; from: string; to: string; retention_days: number; timezone: string }
  totals: TokenUsageMetrics
  series: ({ date: string } & TokenUsageMetrics)[]
  by_keys: ({ id?: number; name: string; prefix?: string } & TokenUsageMetrics)[]
  by_providers: ({ id?: number; name: string } & TokenUsageMetrics)[]
  by_models: ({ name: string; upstream_model?: string } & TokenUsageMetrics)[]
  heatmap?: TokenUsageHeatmapCell[]
  details: any[]
  page: number
  page_size: number
  has_more: boolean
}

export interface IPPoolNode {
  id: number
  name: string
  protocol: string
  server: string
  enabled: boolean
  status: string
  last_error?: string
  last_checked_at?: string
  last_latency_ms: number
  exit_ip?: string
  provider_count: number
  link_configured: boolean
  created_at: string
  updated_at: string
}

export interface ProviderGroup {
  id: number
  name: string
  collapsed: boolean
  sort_order: number
  member_count: number
  healthy_count: number
  created_at: string
  updated_at: string
}

export interface QualityDetectorTarget {
  id: string
  model: string
  route_id: number
  upstream_model: string
  provider_id: number
  provider_name: string
  provider_type: string
  provider_key_id: number
  provider_key_name: string
  provider_key_hint: string
  credential_kind: string
}

export interface QualityDetectorData {
  available: boolean
  version: string
  estimates: Record<string, QualityEstimate>
  targets: QualityDetectorTarget[]
  active_job_id?: string
}

export interface QualityEstimate {
  total_requests: number
  fixed_32k_requests?: number
  approximate_fixed_32k_input_tokens?: number
}

export interface QualityJob {
  id: string
  status: string
  preset: string
  total: number
  completed: number
  succeeded: number
  failed: number
  skipped: number
  cancelled: number
  created_at: string
  started_at?: string
  finished_at?: string
  items?: QualityJobItem[]
}

export interface QualityJobItem {
  id: number
  position: number
  target_id: string
  model: string
  provider_id: number
  provider_name: string
  provider_type: string
  provider_key_id: number
  provider_key_name: string
  provider_key_hint: string
  upstream_model: string
  status: string
  verdict: string
  error: string
  started_at?: string
  finished_at?: string
  report?: string
}

export interface QualityJobListResponse {
  jobs: QualityJob[]
}

export interface CredentialImportPreviewItem {
  id: number
  name: string
  platform: string
  source: string
  email?: string
  account_id?: string
  expires_at?: string
  has_refresh_token: boolean
  status: string
  duplicate: boolean
  duplicate_provider_id?: number
}

export type RoutingStrategy = "priority_failover" | "ordered_round_robin" | "smart_round_robin" | "adaptive"

/**
 * 全局路由（起始渠道选择）策略的展示文案。
 *
 * 注意：四种策略都带有相同的请求内故障转移（首选渠道失败后依次尝试其余渠道、
 * 受熔断与半开探活保护），区别只在于每个新请求的「起始渠道」如何选出：
 * - priority_failover: 始终从优先级最高的渠道开始
 * - ordered_round_robin: 始终从配置顺序的第一个渠道开始（固定起点，不轮换）
 * - smart_round_robin: 在可用渠道间轮换起点，分摊负载
 * - adaptive: 按权重/延迟/失败/并发动态打分选起点（平滑加权轮询）
 */
export const ROUTING_STRATEGY_LABELS: Record<RoutingStrategy, string> = {
  priority_failover: "优先级固定（总从最高优先级开始）",
  ordered_round_robin: "配置顺序固定（总从第一个开始）",
  smart_round_robin: "渠道间轮换（平均分摊）",
  adaptive: "自适应加权（按延迟/失败/并发打分）",
}

export const ROUTING_STRATEGY_HELP: Record<RoutingStrategy, string> = {
  priority_failover: "每个请求都从优先级最高的可用渠道开始；仅当它失败或熔断时才转移。适合有明确主备关系的场景。",
  ordered_round_robin: "每个请求都从配置列表最上方的可用渠道开始；不主动轮换，仅失败时顺延。",
  smart_round_robin: "每个新请求自动换下一个可用渠道作为起点，均匀分摊负载；单次请求内仍会故障转移到后续渠道。",
  adaptive: "按权重、首字节延迟、连续失败和当前并发综合打分，把新请求发给当前得分最高的渠道；恢复冷却的渠道会被优先探测。",
}

export interface ProviderKeyModel {
  model: string
  display_name: string
  capabilities: string
  enabled: boolean
  health_status?: string
  health_error?: string
  latency_ms: number
  first_byte_ms: number
  last_checked_at?: string
}

export interface ProviderKey {
  id: number
  provider_id: number
  name: string
  key_hint: string
  model?: string
  effective_model?: string
  model_inherited: boolean
  egress_mode: "inherit" | "direct" | "node"
  ip_pool_node_id?: number
  ip_pool_node_name?: string
  effective_egress: "direct" | "node"
  effective_node_id?: number
  egress_inherited: boolean
  enabled: boolean
  health_check_enabled: boolean
  cost_multiplier: number
  sort_order: number
  status: string
  last_error?: string
  last_tested_at?: string
  last_test_latency_ms: number
  discovered_models: number
  last_discovered_at?: string
  models: ProviderKeyModel[]
  created_at: string
  updated_at: string
}

export interface CodexUsageWindow {
  used_percent: number
  remaining_percent: number
  limit_window_seconds?: number
  reset_after_seconds?: number
  reset_at?: string
}

export interface CodexResetCard {
  id?: string
  status?: string
  reset_type?: string
  granted_at?: string
  expires_at?: string
}

export interface CodexAccountQuota {
  plan_type?: string
  subscription_plan?: string
  allowed: boolean
  limit_reached: boolean
  primary?: CodexUsageWindow
  secondary?: CodexUsageWindow
  reset_cards: number
  reset_card_details?: CodexResetCard[]
  credits_balance?: number
  credits_unlimited?: boolean
  total_quota: number
  used_quota: number
  remaining_quota: number
  next_reset_date?: string
}
