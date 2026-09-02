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

function systemTheme(): Theme {
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"
}

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
  if (typeof window === "undefined") return "light"
  // The inline script in index.html has already resolved this and put the class
  // on <html> before first paint. Read it back rather than recomputing, so the
  // two can never disagree.
  if (document.documentElement.classList.contains("dark")) return "dark"
  return storedTheme() ?? systemTheme()
}

// The provider and hook intentionally share this module; the warning is about
// hot-reload boundaries only, not runtime correctness.
// oxlint-disable-next-line react/only-export-components
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<Theme>(getInitialTheme)
  // Tracks whether the user has actually picked a theme, as opposed to us
  // merely following the OS. Only an explicit pick gets persisted.
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

  // With no explicit choice recorded, keep following the OS while the tab is
  // open. Previously the very first mount wrote the resolved value to storage,
  // which silently froze the console to whatever the OS happened to be at the
  // time and stopped it ever tracking a later light/dark switch.
  useEffect(() => {
    if (explicit) return
    const media = window.matchMedia("(prefers-color-scheme: dark)")
    const onChange = (event: MediaQueryListEvent) => setTheme(event.matches ? "dark" : "light")
    media.addEventListener("change", onChange)
    return () => media.removeEventListener("change", onChange)
  }, [explicit])

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
