/**
 * The page a channel's name should open.
 *
 * A configured merchant URL opens as-is, including its path and query. When
 * omitted, base_url is an API root, so link to its origin instead of /v1.
 *
 * Returns "" for anything that is not an http(s) URL, so callers can fall back
 * to plain text rather than render a link that goes nowhere.
 */
export function providerSiteURL(baseURL: string, websiteURL?: string): string {
  const merchant = (websiteURL ?? "").trim()
  if (merchant) {
    try {
      const url = new URL(merchant)
      if ((url.protocol === "http:" || url.protocol === "https:") && url.hostname && !url.username && !url.password) return url.href
    } catch {
      // Old or imported invalid addresses fall back to the API host.
    }
  }
  const raw = (baseURL || "").trim()
  if (!raw) return ""
  try {
    const url = new URL(raw)
    if (url.protocol !== "http:" && url.protocol !== "https:") return ""
    return url.origin
  } catch {
    return ""
  }
}
