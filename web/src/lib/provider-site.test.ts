import { describe, expect, it } from "vitest"
import { providerSiteURL } from "./provider-site"

describe("providerSiteURL", () => {
  it("strips the API path so the name opens a page, not an endpoint", () => {
    expect(providerSiteURL("https://api.example.com/v1")).toBe("https://api.example.com")
    expect(providerSiteURL("https://relay.example.com/v1/chat/completions")).toBe("https://relay.example.com")
  })

  it("keeps a non-default port", () => {
    expect(providerSiteURL("http://127.0.0.1:8080/v1")).toBe("http://127.0.0.1:8080")
  })

  it("tolerates surrounding whitespace", () => {
    expect(providerSiteURL("  https://api.example.com/v1  ")).toBe("https://api.example.com")
  })

  it("returns nothing for values that would not open a page", () => {
    expect(providerSiteURL("")).toBe("")
    expect(providerSiteURL("not a url")).toBe("")
    // A javascript: base_url must never become a clickable link.
    expect(providerSiteURL("javascript:alert(1)")).toBe("")
  })
})
