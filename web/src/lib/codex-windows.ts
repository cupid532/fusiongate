import type { CodexAccountQuota, CodexUsageWindow } from "@/lib/types"

/**
 * Codex rate-limit windows, named.
 *
 * `/backend-api/wham/usage` reports `rate_limit.primary_window` and
 * `.secondary_window`. Those names say nothing about what they actually
 * constrain, and what they constrain depends on the plan:
 *
 *   Free        primary = rolling 30 days,  no secondary
 *   Plus / Team primary = rolling 5 hours,  secondary = rolling 7 days
 *
 * So "主窗口 / 次窗口" was both meaningless and, on Plus and Team, actively
 * misleading. The window length is right there in `limit_window_seconds`, so
 * label from that instead — it stays correct if OpenAI changes the periods
 * again, and it degrades to a plain duration for a period we do not know.
 */

const HOUR = 3600
const DAY = 86400

/** Tolerance when matching a reported window to a known period (±10%). */
function near(seconds: number, target: number): boolean {
  return Math.abs(seconds - target) <= target * 0.1
}

export function windowLabel(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return "使用窗口"
  if (near(seconds, 5 * HOUR)) return "5 小时限制"
  if (near(seconds, 7 * DAY)) return "每周限制"
  if (near(seconds, 30 * DAY)) return "每月限制"
  if (near(seconds, DAY)) return "每日限制"
  if (seconds < DAY) return `${Math.round(seconds / HOUR)} 小时限制`
  return `${Math.round(seconds / DAY)} 天限制`
}

/** Short form for tight spaces (chips, headline suffix). */
export function windowLabelShort(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return "窗口"
  if (near(seconds, 5 * HOUR)) return "5 小时"
  if (near(seconds, 7 * DAY)) return "周"
  if (near(seconds, 30 * DAY)) return "月"
  if (near(seconds, DAY)) return "日"
  if (seconds < DAY) return `${Math.round(seconds / HOUR)} 小时`
  return `${Math.round(seconds / DAY)} 天`
}

export type NamedWindow = {
  /** "primary" | "secondary", kept so callers can key React lists stably. */
  slot: "primary" | "secondary"
  label: string
  shortLabel: string
  window: CodexUsageWindow
}

/**
 * The windows present on this account, shortest period first.
 *
 * Ordering by period rather than by slot name puts "5 小时" before "每周",
 * which is how the Codex CLI presents them and how people reason about them.
 */
export function namedWindows(quota: CodexAccountQuota | undefined): NamedWindow[] {
  if (!quota) return []
  const out: NamedWindow[] = []
  if (quota.primary) out.push(describe("primary", quota.primary))
  if (quota.secondary) out.push(describe("secondary", quota.secondary))
  return out.sort((a, b) => (a.window.limit_window_seconds ?? 0) - (b.window.limit_window_seconds ?? 0))
}

function describe(slot: "primary" | "secondary", window: CodexUsageWindow): NamedWindow {
  return {
    slot,
    label: windowLabel(window.limit_window_seconds),
    shortLabel: windowLabelShort(window.limit_window_seconds),
    window,
  }
}

/**
 * The window that is actually closest to cutting you off.
 *
 * The card used to headline `remaining_quota`, which the backend derives from
 * the *primary* window alone. On Plus and Team that is the 5-hour window, so an
 * account with 90% of its weekly allowance burned still showed a full bar
 * moments after the 5-hour window rolled over — the one number you would look
 * at to decide whether you can keep working was the one that could not tell you.
 */
export function bindingWindow(quota: CodexAccountQuota | undefined): NamedWindow | null {
  const windows = namedWindows(quota)
  if (windows.length === 0) return null
  return windows.reduce((worst, candidate) =>
    remainingPercent(candidate.window) < remainingPercent(worst.window) ? candidate : worst
  )
}

/**
 * How much of the window is left, 0–100.
 *
 * Every bar on the card is a *fuel gauge*: full when the allowance is
 * untouched, draining as it is spent. Reading `remaining_percent` where the
 * upstream sends it (and deriving it otherwise) keeps a single source for that
 * number, so a bar's width, its colour and its label can never disagree.
 */
export function remainingPercent(window: CodexUsageWindow): number {
  const value = window.remaining_percent ?? 100 - window.used_percent
  return Math.min(100, Math.max(0, value))
}

/**
 * Tailwind class for a remaining level, shared by the bars and the numbers.
 *
 * Thresholds are stated in terms of what is *left*, matching the gauge:
 * under 40% left warns, under 20% left is critical.
 */
export function remainingTone(remaining: number): string {
  if (remaining <= REMAINING_CRITICAL) return "text-destructive"
  if (remaining <= REMAINING_WARNING) return "text-amber-500"
  return "text-foreground"
}

export function remainingBarTone(remaining: number): string {
  if (remaining <= REMAINING_CRITICAL) return "bg-destructive"
  if (remaining <= REMAINING_WARNING) return "bg-amber-500"
  return "bg-primary"
}

/** Below this share of the allowance the gauge turns amber. */
export const REMAINING_WARNING = 40
/** Below this share of the allowance the gauge turns red. */
export const REMAINING_CRITICAL = 20

/**
 * Readable plan name. The API returns bare lowercase identifiers ("free",
 * "plus", "team"), which the card was rendering verbatim.
 */
export function planLabel(quota: CodexAccountQuota | undefined): string {
  const raw = (quota?.subscription_plan || quota?.plan_type || "").trim().toLowerCase()
  if (!raw) return ""
  const known: Record<string, string> = {
    free: "Free",
    plus: "Plus",
    pro: "Pro",
    team: "Team",
    business: "Business",
    enterprise: "Enterprise",
    edu: "Edu",
  }
  return known[raw] ?? raw.replace(/\b\w/g, (c) => c.toUpperCase())
}

export function formatResetDuration(seconds: number): string {
  if (seconds <= 0) return "即将重置"
  const d = Math.floor(seconds / DAY)
  const h = Math.floor((seconds % DAY) / HOUR)
  const m = Math.floor((seconds % HOUR) / 60)
  const s = Math.floor(seconds % 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  if (m > 0) return `${m} 分 ${s} 秒`
  return `${s} 秒`
}
