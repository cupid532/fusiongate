import { describe, expect, it } from "vitest"
import { render, screen } from "@testing-library/react"
import type { Provider } from "@/lib/types"
import { ProviderPassthroughLabel } from "@/components/ProviderPassthroughLabel"

function provider(fields: Partial<Provider>): Provider { return { id: 1, name: "channel", type: "openai_compatible", ...fields } as Provider }

describe("provider passthrough status", () => {
  it("shows supported channels as raw passthrough", () => {
    render(<ProviderPassthroughLabel provider={provider({ passthrough_supported: true })} />)
    expect(screen.getByText("原样透传")).toBeTruthy()
  })
  it("shows unsupported specialized channels and server reason", () => {
    render(<ProviderPassthroughLabel provider={provider({ passthrough_supported: false, passthrough_reason: "OAuth 专用适配" })} />)
    expect(screen.getByText("纯透传不支持此渠道").getAttribute("title")).toBe("OAuth 专用适配")
  })
  it("keeps missing metadata unknown", () => {
    render(<ProviderPassthroughLabel provider={provider({})} />)
    expect(screen.getByText("透传支持状态未知")).toBeTruthy()
  })
})
