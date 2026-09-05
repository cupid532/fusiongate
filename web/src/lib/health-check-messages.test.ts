import { describe, expect, it } from "vitest"
import { ApiError } from "./api"
import { describeHealthReason, describeHealthStartError, describeHealthStatus } from "./health-check-messages"

describe("describeHealthReason", () => {
  it("translates the backend's skip reasons", () => {
    expect(describeHealthReason("this model is switched off on every API key; enable it in model management")).toContain("模型管理")
    expect(describeHealthReason("API key is disabled")).toBe("Key 已停用")
  })

  it("passes unknown text through instead of hiding it", () => {
    expect(describeHealthReason("upstream said no")).toBe("upstream said no")
    expect(describeHealthReason(undefined)).toBe("")
  })
})

describe("describeHealthStartError", () => {
  it("recognises the running-job conflict by its code", () => {
    expect(describeHealthStartError(new ApiError(409, "health_check_running", "another health check is already running; wait for it to finish or cancel it"))).toContain("正在进行")
  })

  it("translates selection errors by message", () => {
    expect(describeHealthStartError(new Error("disabled providers cannot be health checked"))).toBe("渠道已停用，无法检活")
    expect(describeHealthStartError(new Error("the selected providers have no enabled models"))).toContain("模型管理")
  })

  it("falls back to the raw message", () => {
    expect(describeHealthStartError(new Error("boom"))).toBe("boom")
    expect(describeHealthStartError(null)).toBe("检活启动失败")
  })
})

describe("describeHealthStatus", () => {
  it("maps every terminal status to a label and tone", () => {
    expect(describeHealthStatus("healthy")).toEqual({ label: "健康", tone: "success" })
    expect(describeHealthStatus("skipped").tone).toBe("neutral")
    expect(describeHealthStatus("content_mismatch").tone).toBe("danger")
    expect(describeHealthStatus("weird")).toEqual({ label: "weird", tone: "neutral" })
  })
})
