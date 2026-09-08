import { lazy, Suspense, useCallback, useEffect, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import { useAuth } from "./providers/auth"
import { Sidebar, isPage, type Page } from "./components/layout/Sidebar"
import { Topbar } from "./components/layout/Topbar"
import { Login } from "./pages/Login"

const Dashboard = lazy(() => import("./pages/Dashboard").then((m) => ({ default: m.Dashboard })))
const Providers = lazy(() => import("./pages/Providers").then((m) => ({ default: m.Providers })))
const Keys = lazy(() => import("./pages/Keys").then((m) => ({ default: m.Keys })))
const Requests = lazy(() => import("./pages/Requests").then((m) => ({ default: m.Requests })))
const IPPool = lazy(() => import("./pages/IPPool").then((m) => ({ default: m.IPPool })))
const Routes = lazy(() => import("./pages/Routes").then((m) => ({ default: m.Routes })))
const Usage = lazy(() => import("./pages/Usage").then((m) => ({ default: m.Usage })))
const Quality = lazy(() => import("./pages/Quality").then((m) => ({ default: m.Quality })))
const AuthFiles = lazy(() => import("./pages/AuthFiles").then((m) => ({ default: m.AuthFiles })))

function PageFallback() {
  return <PageSkeleton />
}

function PageSkeleton() {
  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="h-7 w-48 animate-pulse rounded-md bg-muted/60" />
        <div className="h-4 w-72 animate-pulse rounded-md bg-muted/40" />
      </div>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        {Array.from({ length: 6 }).map((_, i) => (
          <div key={i} className="rounded-xl border bg-card p-4">
            <div className="flex items-center justify-between">
              <div className="h-3 w-14 animate-pulse rounded bg-muted/60" />
              <div className="h-8 w-8 animate-pulse rounded-lg bg-muted/40" />
            </div>
            <div className="mt-3 h-7 w-16 animate-pulse rounded bg-muted/60" />
          </div>
        ))}
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {Array.from({ length: 2 }).map((_, i) => (
          <div key={i} className="rounded-xl border bg-card p-5">
            <div className="h-4 w-32 animate-pulse rounded bg-muted/60" />
            <div className="mt-2 h-3 w-48 animate-pulse rounded bg-muted/40" />
            <div className="mt-5 space-y-3">
              {Array.from({ length: 3 }).map((_, j) => (
                <div key={j} className="h-12 animate-pulse rounded-lg bg-muted/40" />
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

// Resolves the location hash to a real page. Anything unrecognised — a stale
// bookmark, a hand-edited URL, a link to a page that has since been renamed —
// falls back to the dashboard. Previously the raw hash was cast straight to
// Page, so `#settings` produced an unhandled switch case and rendered an
// entirely blank content area under a topbar that still said 概览.
function pageFromHash(): Page {
  const raw = decodeURIComponent(location.hash.replace(/^#/, ""))
  return isPage(raw) ? raw : "dashboard"
}

function pageContent(page: Page) {
  switch (page) {
    case "dashboard":
      return <Dashboard />
    case "authfiles":
      return <AuthFiles />
    case "providers":
      return <Providers />
    case "ippool":
      return <IPPool />
    case "routes":
      return <Routes />
    case "keys":
      return <Keys />
    case "usage":
      return <Usage />
    case "requests":
      return <Requests />
    case "quality":
      return <Quality />
  }
}

function NavProgress({ page }: { page: string }) {
  const [visible, setVisible] = useState(false)
  useEffect(() => {
    setVisible(true)
    const t = setTimeout(() => setVisible(false), 500)
    return () => clearTimeout(t)
  }, [page])
  if (!visible) return null
  return (
    <motion.div
      className="fixed left-0 right-0 top-0 z-[60] h-[2px] origin-left bg-primary"
      initial={{ scaleX: 0, opacity: 1 }}
      animate={{ scaleX: 1, opacity: 0 }}
      transition={{ scaleX: { duration: 0.45, ease: [0.16, 1, 0.3, 1] }, opacity: { duration: 0.2, delay: 0.35 } }}
    />
  )
}

export default function App() {
  const { loading, authenticated } = useAuth()
  const [page, setPage] = useState<Page>(pageFromHash)
  const [sidebarOpen, setSidebarOpen] = useState(false)

  useEffect(() => {
    const onHash = () => setPage(pageFromHash())
    window.addEventListener("hashchange", onHash)
    return () => window.removeEventListener("hashchange", onHash)
  }, [])

  // Normalise a bad hash in the address bar so a reload doesn't land on it
  // again and the URL matches what is actually on screen.
  useEffect(() => {
    const raw = decodeURIComponent(location.hash.replace(/^#/, ""))
    if (raw && !isPage(raw)) location.replace(`#${page}`)
  }, [page])

  const navigate = useCallback((p: Page) => {
    location.hash = p
    setPage(p)
    setSidebarOpen(false)
  }, [])

  // Escape closes the mobile drawer. It is a modal overlay, so this is the
  // behaviour anyone would expect; previously the only way out was to hit the
  // small × or the scrim.
  useEffect(() => {
    if (!sidebarOpen) return
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setSidebarOpen(false)
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [sidebarOpen])

  if (loading) {
    return (
      <div className="grid min-h-screen place-items-center">
        <div className="h-5 w-5 animate-spin rounded-full border-2 border-primary border-t-transparent" />
      </div>
    )
  }
  if (!authenticated) return <Login />

  return (
    <div className="min-h-screen">
      <NavProgress page={page} />
      <Sidebar page={page} onNavigate={navigate} open={sidebarOpen} onClose={() => setSidebarOpen(false)} />
      <div className="md:ml-[248px]">
        <Topbar page={page} onMenu={() => setSidebarOpen(true)} />
        <main className="mx-auto max-w-[1680px] px-4 py-6 pb-16 sm:px-6 md:px-8 md:py-7">
          <AnimatePresence mode="wait">
            <motion.div
              key={page}
              initial={{ opacity: 0, y: 10 }}
              animate={{ opacity: 1, y: 0, transition: { duration: 0.35, ease: [0.16, 1, 0.3, 1] } }}
              exit={{ opacity: 0, y: -6, transition: { duration: 0.18, ease: [0.4, 0, 1, 1] } }}
            >
              <Suspense fallback={<PageFallback />}>{pageContent(page)}</Suspense>
            </motion.div>
          </AnimatePresence>
        </main>
      </div>
    </div>
  )
}
