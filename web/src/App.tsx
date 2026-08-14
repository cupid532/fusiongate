import { lazy, Suspense, useCallback, useEffect, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import { useAuth } from "./providers/auth"
import { Sidebar, type Page } from "./components/layout/Sidebar"
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
  return <div className="py-16 text-center text-sm text-muted-foreground">加载中…</div>
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

export default function App() {
  const { loading, authenticated } = useAuth()
  const [page, setPage] = useState<Page>(() => (location.hash.slice(1) as Page) || "dashboard")

  useEffect(() => {
    const onHash = () => setPage((location.hash.slice(1) as Page) || "dashboard")
    window.addEventListener("hashchange", onHash)
    return () => window.removeEventListener("hashchange", onHash)
  }, [])

  const navigate = useCallback((p: Page) => {
    location.hash = p
    setPage(p)
  }, [])

  if (loading) {
    return <div className="grid min-h-screen place-items-center text-sm text-muted-foreground">加载中…</div>
  }
  if (!authenticated) return <Login />

  return (
    <div className="min-h-screen">
      <Sidebar page={page} onNavigate={navigate} />
      <div className="ml-[248px]">
        <Topbar page={page} />
        <main className="mx-auto max-w-[1680px] px-8 py-7 pb-16">
          <AnimatePresence mode="wait">
            <motion.div
              key={page}
              initial={{ opacity: 0, y: 8 }}
              animate={{ opacity: 1, y: 0 }}
              exit={{ opacity: 0, y: -8 }}
              transition={{ duration: 0.2 }}
            >
              <Suspense fallback={<PageFallback />}>{pageContent(page)}</Suspense>
            </motion.div>
          </AnimatePresence>
        </main>
      </div>
    </div>
  )
}
