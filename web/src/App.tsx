import { useCallback, useEffect, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import { useAuth } from "./providers/auth"
import { Sidebar, type Page } from "./components/layout/Sidebar"
import { Topbar } from "./components/layout/Topbar"
import { Login } from "./pages/Login"
import { Dashboard } from "./pages/Dashboard"

function Placeholder({ label }: { label: string }) {
  return (
    <div className="flex h-64 items-center justify-center rounded-xl border border-dashed text-sm text-muted-foreground">
      {label} · 开发中
    </div>
  )
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
              {page === "dashboard" ? <Dashboard /> : <Placeholder label={page} />}
            </motion.div>
          </AnimatePresence>
        </main>
      </div>
    </div>
  )
}
