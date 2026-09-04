/**
 * The page a channel's name should open.
 *
 * `base_url` is an API root — `https://api.example.com/v1` — and opening that
 * verbatim lands on a 404 or a bare JSON error. The site people actually want
 * is its origin, so strip the path and keep scheme + host.
 *
 * Returns "" for anything that is not an http(s) URL, so callers can fall back
 * to plain text rather than render a link that goes nowhere.
 */
export function providerSiteURL(baseURL: string): string {
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
