import { describe, expect, it } from "vitest"
import { bindingWindow, namedWindows, planLabel, windowLabel, windowLabelShort } from "./codex-windows"
import type { CodexAccountQuota, CodexUsageWindow } from "./types"

function win(limitWindowSeconds: number, usedPercent: number): CodexUsageWindow {
  return {
    used_percent: usedPercent,
    remaining_percent: 100 - usedPercent,
    limit_window_seconds: limitWindowSeconds,
  }
}

function quota(partial: Partial<CodexAccountQuota>): CodexAccountQuota {
  return {
    allowed: true,
    limit_reached: false,
    reset_cards: 0,
    total_quota: 100,
    used_quota: 0,
    remaining_quota: 100,
    ...partial,
  }
}

const FIVE_HOURS = 5 * 3600
const ONE_WEEK = 7 * 86400
const THIRTY_DAYS = 30 * 86400

describe("windowLabel", () => {
  it("names the periods Codex actually reports", () => {
    expect(windowLabel(FIVE_HOURS)).toBe("5 小时限制")
    expect(windowLabel(ONE_WEEK)).toBe("每周限制")
    expect(windowLabel(THIRTY_DAYS)).toBe("每月限制")
    expect(windowLabelShort(FIVE_HOURS)).toBe("5 小时")
    expect(windowLabelShort(ONE_WEEK)).toBe("周")
  })

  it("tolerates the upstream reporting a slightly off period", () => {
    // Observed values are not always exact multiples, so matching is fuzzy.
    expect(windowLabel(FIVE_HOURS + 120)).toBe("5 小时限制")
    expect(windowLabel(ONE_WEEK - 3600)).toBe("每周限制")
  })

  it("falls back to a plain duration for an unknown period", () => {
    expect(windowLabel(9 * 3600)).toBe("9 小时限制")
    expect(windowLabel(3 * 86400)).toBe("3 天限制")
    expect(windowLabel(undefined)).toBe("使用窗口")
    expect(windowLabel(0)).toBe("使用窗口")
  })
})

describe("namedWindows", () => {
  it("orders a Plus/Team account 5h before weekly regardless of slot order", () => {
    const result = namedWindows(quota({ primary: win(ONE_WEEK, 10), secondary: win(FIVE_HOURS, 20) }))
    expect(result.map((w) => w.label)).toEqual(["5 小时限制", "每周限制"])
  })

  it("handles a Free account with only a monthly window", () => {
    const result = namedWindows(quota({ primary: win(THIRTY_DAYS, 6) }))
    expect(result).toHaveLength(1)
    expect(result[0].label).toBe("每月限制")
  })

  it("returns nothing when the upstream sent no windows", () => {
    expect(namedWindows(quota({}))).toEqual([])
    expect(namedWindows(undefined)).toEqual([])
  })
})

describe("bindingWindow", () => {
  it("picks the weekly window when it is more exhausted than the 5h window", () => {
    // The exact regression this replaced: the card headlined the primary (5h)
    // window, so a nearly-spent weekly allowance showed a full bar right after
    // the 5h window rolled over.
    const binding = bindingWindow(quota({ primary: win(FIVE_HOURS, 2), secondary: win(ONE_WEEK, 93) }))
    expect(binding?.label).toBe("每周限制")
    expect(binding?.window.used_percent).toBe(93)
  })

  it("picks the 5h window when that is the tighter one", () => {
    const binding = bindingWindow(quota({ primary: win(FIVE_HOURS, 88), secondary: win(ONE_WEEK, 40) }))
    expect(binding?.label).toBe("5 小时限制")
  })

  it("returns the only window when there is one", () => {
    expect(bindingWindow(quota({ primary: win(THIRTY_DAYS, 6) }))?.label).toBe("每月限制")
  })

  it("returns null when there are no windows", () => {
    expect(bindingWindow(quota({}))).toBeNull()
  })
})

describe("planLabel", () => {
  it("capitalises the bare identifiers the API returns", () => {
    expect(planLabel(quota({ plan_type: "free" }))).toBe("Free")
    expect(planLabel(quota({ plan_type: "plus" }))).toBe("Plus")
    expect(planLabel(quota({ plan_type: "team" }))).toBe("Team")
  })

  it("prefers subscription_plan and passes through unknown plans", () => {
    expect(planLabel(quota({ plan_type: "plus", subscription_plan: "team" }))).toBe("Team")
    expect(planLabel(quota({ plan_type: "something_new" }))).toBe("Something_new")
    expect(planLabel(quota({}))).toBe("")
  })
})
