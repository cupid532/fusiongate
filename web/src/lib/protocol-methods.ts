export type ProtocolMethod = "auto" | "chat" | "responses" | "messages"

export const PROTOCOL_METHOD_LABELS: Record<ProtocolMethod, string> = {
  auto: "自适应",
  chat: "OpenAI Chat Completions",
  responses: "OpenAI Responses",
  messages: "Anthropic Messages",
}

export const PROTOCOL_METHODS: ProtocolMethod[] = ["auto", "chat", "responses", "messages"]

// These are upstream endpoints, not the public API exposed by FusionGate.
// OAuth and dedicated web/console channels have their own authentication and endpoints.
const FIXED_METHODS: Record<string, ProtocolMethod[]> = {
  openai_compatible: ["chat", "responses"],
  openai: ["chat", "responses"],
  openrouter: ["chat", "responses"],
  grok: ["chat", "responses"],
  anthropic: ["messages", "responses"],
  anthropic_compatible: ["messages", "responses"],
  opencode: ["chat", "responses", "messages"],
}

export function supportsProtocolMethod(type: string, method: ProtocolMethod): boolean {
  return method === "auto" || (FIXED_METHODS[type]?.includes(method) ?? false)
}

export function protocolMethod(policy?: string, preference?: string): ProtocolMethod {
  if (policy !== "fixed") return "auto"
  const first = preference?.split(",")[0]?.trim()
  return first === "chat" || first === "responses" || first === "messages" ? first : "auto"
}

export function protocolMethodLabel(policy?: string, preference?: string): string {
  if (policy === "fixed" && (preference?.includes(",") || !preference)) {
    return `固定：${preference?.split(",").map((p) => PROTOCOL_METHOD_LABELS[p.trim() as ProtocolMethod] ?? p.trim()).join(" → ") || "未知"}`
  }
  return PROTOCOL_METHOD_LABELS[protocolMethod(policy, preference)]
}

export function protocolMethodPatch(method: ProtocolMethod): { protocol_policy: string; protocol_preference: string } {
  return method === "auto"
    ? { protocol_policy: "auto", protocol_preference: "" }
    : { protocol_policy: "fixed", protocol_preference: method }
}
