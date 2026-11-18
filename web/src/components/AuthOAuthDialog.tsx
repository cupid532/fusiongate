import { useEffect, useRef, useState } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import { notifySuccess } from "@/lib/notify"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

const platformLabels = { codex: "Codex (ChatGPT)", claude: "Claude", grok: "Grok (xAI)" }
type Platform = keyof typeof platformLabels

type StartResponse = {
  session_id?: string
  flow?: string
  auth_url?: string
  verification_url?: string
  verification_url_complete?: string
  user_code?: string
  expires_in?: number
  poll_interval?: number
}

type LoginSession = { id: string; url: string; expiresAt: number } & (
  { flow: "callback" } | { flow: "device"; code: string; interval: number }
)

type CompleteResponse = { pending: true; retry_after?: number } | { id: number; model_sync?: { status: string } }

function secureURL(value: unknown, xaiOnly = false): string | undefined {
  if (typeof value !== "string") return
  try {
    const url = new URL(value)
    if (url.protocol !== "https:" || url.username || url.password) return
    if (xaiOnly && url.hostname !== "x.ai" && !url.hostname.endsWith(".x.ai")) return
    return url.href
  } catch {
    return
  }
}

function loginSession(platform: Platform, response: StartResponse | null): LoginSession {
  if (response && typeof response.session_id === "string" && response.session_id.trim()
    && typeof response.expires_in === "number" && Number.isFinite(response.expires_in) && response.expires_in > 0) {
    const base = { id: response.session_id, expiresAt: Date.now() + response.expires_in * 1000 }
    if (platform === "grok" && response.flow === "device") {
      const url = secureURL(response.verification_url_complete, true) ?? secureURL(response.verification_url, true)
      if (url && typeof response.user_code === "string" && response.user_code.trim()
        && typeof response.poll_interval === "number" && Number.isFinite(response.poll_interval) && response.poll_interval > 0) {
        return { ...base, url, flow: "device", code: response.user_code, interval: response.poll_interval }
      }
    } else if (platform !== "grok" && response.flow !== "device") {
      const url = secureURL(response.auth_url)
      if (url) return { ...base, url, flow: "callback" }
    }
  }
  throw new Error("网关未返回完整的授权信息，请重新生成。")
}

function waitForPoll(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    signal.throwIfAborted()
    const timer = window.setTimeout(() => { signal.removeEventListener("abort", stop); resolve() }, ms)
    function stop() { window.clearTimeout(timer); reject(signal.reason) }
    signal.addEventListener("abort", stop, { once: true })
  })
}

const errorMessages: Record<string, string> = {
  oauth_start_failed: "暂时无法从 xAI 获取设备码，请重试。",
  oauth_exchange_failed: "未能换取授权凭据，请重新生成授权后再试。",
  oauth_session_expired: "授权会话已过期，请重新生成授权。",
  oauth_access_denied: "你已在 xAI 拒绝授权；如需授权，请重新生成。",
  credential_exists: "此账号已在认证文件中，本次未创建新凭据。",
}

export function AuthOAuthDialog({ defaultPlatform, onClose }: { defaultPlatform?: string; onClose: () => void }) {
  const qc = useQueryClient()
  const [platform, setPlatform] = useState<Platform>(defaultPlatform === "grok" || defaultPlatform === "claude" ? defaultPlatform : "codex")
  const [session, setSession] = useState<LoginSession | null>(null)
  const [callback, setCallback] = useState("")
  const [name, setName] = useState("")
  const [priority, setPriority] = useState("1")
  const [busy, setBusy] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState("")
  const current = useRef<AbortController | null>(null)
  useEffect(() => () => current.current?.abort(), [])

  function reset() {
    setSession(null)
    setCallback("")
    setError("")
  }

  async function run(task: (signal: AbortSignal) => Promise<void>) {
    if (current.current) return
    const request = new AbortController()
    current.current = request
    setBusy(true)
    setPending(false)
    setError("")
    try {
      await task(request.signal)
    } catch (err) {
      if (request.signal.aborted) return
      if (err instanceof ApiError && ["oauth_session_expired", "oauth_access_denied", "oauth_exchange_failed", "credential_exists"].includes(err.code)) reset()
      setError(err instanceof ApiError ? errorMessages[err.code] ?? err.message : err instanceof Error ? err.message : "授权失败，请重试。")
    } finally {
      current.current = null
      if (!request.signal.aborted) {
        setBusy(false)
        setPending(false)
      }
    }
  }

  function start() {
    return run(async (signal) => {
      const result = await api<StartResponse>("/api/admin/auth/oauth/start", {
        method: "POST", body: JSON.stringify({ platform }), signal,
      })
      if (signal.aborted) return
      setSession(loginSession(platform, result))
      setName((value) => value || `${platformLabels[platform]} 账号`)
    })
  }

  function complete(login: LoginSession) {
    return run(async (signal) => {
      const body = JSON.stringify({ session_id: login.id, name, priority: Number(priority), ...(login.flow === "callback" ? { callback: callback.trim() } : {}) })
      for (;;) {
        if (Date.now() >= login.expiresAt) throw new ApiError(400, "oauth_session_expired", "")
        const result = await api<CompleteResponse | null>("/api/admin/auth/oauth/complete", { method: "POST", body, signal })
        if (signal.aborted) return
        if (result && typeof result === "object" && "pending" in result && result.pending === true && login.flow === "device") {
          setPending(true)
          const retry = typeof result.retry_after === "number" && Number.isFinite(result.retry_after) ? result.retry_after : 0
          await waitForPoll(Math.min(Math.max(login.interval, retry) * 1000, Math.max(0, login.expiresAt - Date.now())), signal)
          continue
        }
        if (!result || typeof result !== "object" || "pending" in result || !("id" in result) || !Number.isInteger(result.id) || result.id <= 0) {
          throw new Error("网关未确认凭据已保存，请先刷新认证文件列表核对。")
        }
        void qc.invalidateQueries({ queryKey: ["providers"] })
        void qc.invalidateQueries({ queryKey: ["routes"] })
        notifySuccess("授权凭据已保存", result.model_sync?.status === "error" ? "模型同步未完成，可稍后从认证文件页面重试。" : undefined)
        onClose()
        return
      }
    })
  }

  function close() {
    current.current?.abort()
    onClose()
  }

  const validPriority = priority.trim() !== "" && Number.isSafeInteger(Number(priority)) && Number(priority) >= 0
  return (
    <Dialog open onOpenChange={(open) => { if (!open) close() }}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>添加 OAuth 授权</DialogTitle>
          <DialogDescription>{platform === "grok" ? "Grok 使用 xAI 官方设备授权，不需要粘贴回调地址。" : "在浏览器完成授权，再把回调地址粘贴回来。"}</DialogDescription>
        </DialogHeader>

        {!session ? (
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="oauth-platform">平台</Label>
            <select id="oauth-platform" value={platform} disabled={busy} onChange={(e) => { setPlatform(e.target.value as Platform); setName(""); setError("") }} className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm">
              {Object.entries(platformLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
            </select>
          </div>
        ) : (
          <div className="space-y-3">
            <div className="space-y-2 rounded-lg bg-muted p-3 text-sm">
              <a href={session.url} target="_blank" rel="noopener noreferrer" className="font-medium text-primary underline">
                {session.flow === "device" ? "打开 xAI 授权页面" : "打开授权页面"}
              </a>
              {session.flow === "device" && (
                <>
                  <p>用你的 xAI 账号登录并确认授权。如果页面要求输入设备码，请使用下方代码。</p>
                  <div>设备码：<code className="select-all font-semibold tracking-wider">{session.code}</code></div>
                </>
              )}
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="oauth-name">名称</Label>
              <Input id="oauth-name" value={name} disabled={busy} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="oauth-priority">优先级（数字越大越优先）</Label>
              <Input id="oauth-priority" type="number" min={0} step={1} value={priority} disabled={busy} onChange={(e) => setPriority(e.target.value)} />
            </div>
            {session.flow === "callback" && (
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="oauth-callback">回调地址（授权后从浏览器地址栏复制）</Label>
                <Input id="oauth-callback" value={callback} disabled={busy} onChange={(e) => setCallback(e.target.value)} placeholder="http://localhost:…?code=…" className="font-mono text-xs" />
              </div>
            )}
          </div>
        )}

        {pending && <p role="status" className="text-sm text-muted-foreground">正在等待 xAI 确认授权。请在 xAI 页面完成授权，FusionGate 会自动再检查。</p>}
        {error && <p role="alert" className="text-sm text-destructive">{error}</p>}

        <DialogFooter>
          <Button variant="outline" onClick={close}>取消</Button>
          {session && <Button variant="outline" disabled={busy} onClick={reset}>返回平台选择</Button>}
          {!session ? (
            <Button onClick={() => void start()} disabled={busy}>{busy ? "生成中…" : platform === "grok" ? "获取 xAI 设备码" : "生成授权地址"}</Button>
          ) : (
            <Button onClick={() => void complete(session)} disabled={busy || !validPriority || (session.flow === "callback" && !callback.trim())}>
              {busy ? pending ? "等待 xAI 授权…" : "验证中…" : session.flow === "device" ? "我已授权，检查状态" : "完成授权"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
