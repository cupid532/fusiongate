import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react"
import { useQueryClient } from "@tanstack/react-query"
import { api, ApiError, setCsrfToken } from "@/lib/api"
import { notify, setUnauthorizedHandler } from "@/lib/notify"

type Session = {
  authenticated: boolean
  csrf_token: string
}

type AuthState = {
  loading: boolean
  authenticated: boolean
  login: (password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthState | null>(null)

// The provider and hook intentionally share this module; the warning is about
// hot-reload boundaries only, not runtime correctness.
// oxlint-disable-next-line react/only-export-components
export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const [loading, setLoading] = useState(true)
  const [authenticated, setAuthenticated] = useState(false)

  /** Drop the credentials this console holds for the session. */
  const endSession = useCallback(() => {
    setCsrfToken("")
    setAuthenticated(false)
  }, [])

  // Everything the console fetched lives in one QueryClient, including answers
  // that must not outlive the session — a revealed API key, decrypted
  // credentials, usage tables. Clearing on the way out means the next person to
  // see this screen starts from an empty cache rather than the last one's data.
  useEffect(() => {
    if (authenticated) return
    queryClient.clear()
  }, [authenticated, queryClient])

  useEffect(() => {
    api<Session>("/api/admin/session")
      .then((s) => {
        setCsrfToken(s.csrf_token)
        setAuthenticated(s.authenticated)
      })
      .catch(() => setAuthenticated(false))
      .finally(() => setLoading(false))
  }, [])

  // Sessions live in the gateway's memory with a 12h expiry, so they vanish on
  // every restart. A 401 or an explicit csrf_failed asks for reauthentication;
  // an ordinary action-level 403 (such as a wrong current password) does not.
  // Stale CSRF can also follow a login in another tab, without cookie revocation.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setCsrfToken("")
      setAuthenticated((wasAuthenticated) => {
        if (wasAuthenticated) {
          notify({
            tone: "error",
            title: "登录状态已失效",
            description: "会话过期或安全校验失效，请重新登录。",
          })
        }
        return false
      })
    })
    return () => setUnauthorizedHandler(null)
  }, [])

  const login = useCallback(async (password: string) => {
    const res = await api<{ ok: boolean; csrf_token: string }>("/api/admin/login", {
      method: "POST",
      body: JSON.stringify({ password }),
    })
    setCsrfToken(res.csrf_token)
    setAuthenticated(true)
  }, [])

  const logout = useCallback(async () => {
    try {
      await api("/api/admin/logout", { method: "POST" })
      endSession()
    } catch (error) {
      // Only a 401 proves the session is already gone. A stale CSRF token
      // rejects logout before the gateway revokes the still-valid cookie.
      if (error instanceof ApiError && error.status === 401) {
        endSession()
        return
      }
      // Only the gateway can revoke the session cookie. A network failure or
      // CSRF rejection must not look like a confirmed logout. The global CSRF
      // handler may already have requested reauthentication; explain that the
      // cookie can still be valid even if the login screen is now visible.
      notify({
        tone: "error",
        title: "退出未完成",
        description: "网关没有确认退出，当前会话可能仍然有效。请刷新页面后重试退出；如需登录，请重新登录后再退出。",
      })
    }
  }, [endSession])

  const value = useMemo(
    () => ({ loading, authenticated, login, logout }),
    [loading, authenticated, login, logout]
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

// oxlint-disable-next-line react/only-export-components
export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error("useAuth must be used within AuthProvider")
  return ctx
}
