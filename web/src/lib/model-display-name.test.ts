import { describe, expect, it } from "vitest"
import { modelDisplayName } from "./model-display-name"

describe("modelDisplayName", () => {
  it("preserves an explicit nonempty display name verbatim, including slashes", () => {
    expect(modelDisplayName("deepseek/deepseek-v4.1-flash", "我的 / Flash")).toBe("我的 / Flash")
    expect(modelDisplayName("vendor/model", "  Custom Name  ")).toBe("  Custom Name  ")
  })

  it("uses the upstream basename only when no display name is set", () => {
    for (const displayName of [undefined, "", "   "]) {
      expect(modelDisplayName("deepseek/deepseek-v4.1-flash", displayName)).toBe("deepseek-v4.1-flash")
      expect(modelDisplayName("channel/vendor/Model-X", displayName)).toBe("Model-X")
      expect(modelDisplayName("Model-X", displayName)).toBe("Model-X")
      expect(modelDisplayName("vendor/", displayName)).toBe("vendor/")
    }
  })
})
