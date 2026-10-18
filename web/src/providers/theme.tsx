import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react"

type Theme = "light" | "dark"
type ThemeState = { theme: Theme; toggle: () => void }

const STORAGE_KEY = "fusiongate-theme"

const ThemeContext = createContext<ThemeState | null>(null)

function storedTheme(): Theme | null {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    return stored === "light" || stored === "dark" ? stored : null
  } catch {
    // localStorage throws in some privacy modes.
    return null
  }
}

function getInitialTheme(): Theme {
  if (typeof window === "undefined") return "dark"
  // The inline script in index.html has already resolved this and put the class
  // on <html> before first paint. Read it back rather than recomputing, so the
  // two can never disagree.
  if (document.documentElement.classList.contains("dark")) return "dark"
  return storedTheme() ?? "dark"
}

// The provider and hook intentionally share this module; the warning is about
// hot-reload boundaries only, not runtime correctness.
// oxlint-disable-next-line react/only-export-components
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<Theme>(getInitialTheme)
  // The orbital console defaults to dark. Preserve explicit user choices.
  const [explicit, setExplicit] = useState<boolean>(() => storedTheme() != null)

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
  }, [theme])

  useEffect(() => {
    if (!explicit) return
    try {
      localStorage.setItem(STORAGE_KEY, theme)
    } catch {
      // Non-persistent session; the theme still applies for this page.
    }
  }, [theme, explicit])

  const toggle = useCallback(() => {
    setExplicit(true)
    setTheme((t) => (t === "light" ? "dark" : "light"))
  }, [])

  const value = useMemo(() => ({ theme, toggle }), [theme, toggle])

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}

// oxlint-disable-next-line react/only-export-components
export function useTheme() {
  const ctx = useContext(ThemeContext)
  if (!ctx) throw new Error("useTheme must be used within ThemeProvider")
  return ctx
}
