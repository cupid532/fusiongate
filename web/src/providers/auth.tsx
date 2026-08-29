import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react"
import { api, setCsrfToken } from "@/lib/api"

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
  const [loading, setLoading] = useState(true)
  const [authenticated, setAuthenticated] = useState(false)

  useEffect(() => {
    api<Session>("/api/admin/session")
      .then((s) => {
        setCsrfToken(s.csrf_token)
        setAuthenticated(s.authenticated)
      })
      .catch(() => setAuthenticated(false))
      .finally(() => setLoading(false))
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
    } finally {
      setAuthenticated(false)
    }
  }, [])

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
