import { describe, expect, it } from "vitest"
import { render, screen } from "@testing-library/react"
import type { Provider } from "@/lib/types"
import { ProviderPassthroughLabel } from "@/components/ProviderPassthroughLabel"

function provider(fields: Partial<Provider>): Provider { return { id: 1, name: "channel", type: "openai_compatible", ...fields } as Provider }

describe("provider passthrough status", () => {
  it("shows supported channels as passthrough-first with their protocol method", () => {
    render(<ProviderPassthroughLabel provider={provider({ passthrough_supported: true, protocol_policy: "fixed", protocol_preference: "chat" })} />)
    expect(screen.getByText("透传优先 · OpenAI Chat Completions")).toBeTruthy()
  })
  it("shows unsupported specialized channels and server reason", () => {
    render(<ProviderPassthroughLabel provider={provider({ passthrough_supported: false, passthrough_reason: "OAuth 专用适配" })} />)
    expect(screen.getByText("不参与推理路由").getAttribute("title")).toBe("OAuth 专用适配")
  })
  it("keeps missing metadata unknown", () => {
    render(<ProviderPassthroughLabel provider={provider({})} />)
    expect(screen.getByText("透传支持状态未知")).toBeTruthy()
  })
})
