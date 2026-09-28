import { describe, expect, it } from "vitest"
import {
  protocolMethod,
  protocolMethodLabel,
  protocolMethodPatch,
  supportsProtocolMethod,
} from "./protocol-methods"

describe("provider upstream interface method", () => {
  it("keeps the default adaptive and serializes each fixed upstream interface", () => {
    expect(protocolMethod(undefined, undefined)).toBe("auto")
    expect(protocolMethodPatch("auto")).toEqual({ protocol_policy: "auto", protocol_preference: "" })
    for (const method of ["chat", "responses", "messages"] as const) {
      expect(protocolMethodPatch(method)).toEqual({ protocol_policy: "fixed", protocol_preference: method })
      expect(protocolMethod("fixed", method)).toBe(method)
    }
  })

  it("displays legacy multi-protocol preferences without hiding their actual value", () => {
    expect(protocolMethodLabel("fixed", "responses,messages")).toContain("Responses")
    expect(protocolMethodLabel("fixed", "responses,messages")).toContain("Messages")
  })

  it("limits dedicated provider types to their own adaptive interface", () => {
    for (const type of ["codex_oauth", "claude_oauth", "grok_oauth", "grok_console", "grok_web", "gemini"]) {
      expect(supportsProtocolMethod(type, "auto")).toBe(true)
      expect(supportsProtocolMethod(type, "chat")).toBe(false)
      expect(supportsProtocolMethod(type, "responses")).toBe(false)
      expect(supportsProtocolMethod(type, "messages")).toBe(false)
    }
    expect(supportsProtocolMethod("anthropic_compatible", "messages")).toBe(true)
    expect(supportsProtocolMethod("openai_compatible", "responses")).toBe(true)
    expect(supportsProtocolMethod("opencode", "messages")).toBe(true)
  })
})
