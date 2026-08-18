import { motion } from "motion/react"
import { useEffect, useState } from "react"
import {
  LayoutDashboard,
  FileKey,
  Server,
  Network,
  Route,
  KeyRound,
  BarChart3,
  ScrollText,
  ShieldCheck,
  Octagon,
  X,
} from "lucide-react"
import { cn } from "@/lib/utils"

export type Page = "dashboard" | "authfiles" | "providers" | "ippool" | "routes" | "keys" | "usage" | "requests" | "quality"

const navItems: { page: Page; label: string; icon: typeof LayoutDashboard }[] = [
  { page: "dashboard", label: "概览", icon: LayoutDashboard },
  { page: "authfiles", label: "认证文件", icon: FileKey },
  { page: "providers", label: "上游渠道", icon: Server },
  { page: "ippool", label: "IP 池", icon: Network },
  { page: "routes", label: "模型路由", icon: Route },
  { page: "keys", label: "访问密钥", icon: KeyRound },
  { page: "usage", label: "用量与费用", icon: BarChart3 },
  { page: "requests", label: "请求账本", icon: ScrollText },
  { page: "quality", label: "质量检测", icon: ShieldCheck },
]

export function Sidebar({
  page,
  onNavigate,
  open,
  onClose,
}: {
  page: Page
  onNavigate: (p: Page) => void
  open: boolean
  onClose: () => void
}) {
  const [version, setVersion] = useState("")

  useEffect(() => {
    setVersion(document.querySelector('meta[name="fusiongate-version"]')?.getAttribute("content") ?? "")
  }, [])

  return (
    <>
      {open && (
        <button
          type="button"
          aria-label="关闭导航"
          onClick={onClose}
          className="fixed inset-0 z-30 bg-black/40 backdrop-blur-sm md:hidden"
        />
      )}
      <aside
        className={cn(
          "fixed inset-y-0 left-0 z-40 flex w-[248px] flex-col border-r bg-sidebar px-4 py-5 text-sidebar-foreground transition-transform duration-200 md:translate-x-0",
          open ? "translate-x-0" : "-translate-x-full"
        )}
      >
        <div className="flex h-12 items-center gap-3 border-b px-2 pb-4 mb-4">
          <div className="grid h-9 w-9 place-items-center rounded-xl bg-gradient-to-br from-[#66ab71] to-[#458554] text-white shadow-md">
            <Octagon className="h-5 w-5" />
          </div>
          <div className="leading-tight">
            <div className="text-[15px] font-semibold tracking-tight">FusionGate</div>
            <div className="text-[10px] uppercase tracking-wider text-sidebar-foreground/50">Control Plane</div>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="关闭导航"
            className="ml-auto grid h-8 w-8 place-items-center rounded-md text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-foreground md:hidden"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="px-3 pb-2 text-[9px] font-bold uppercase tracking-[0.14em] text-sidebar-foreground/50">
          Workspace
        </div>

        <nav className="flex flex-col gap-1">
          {navItems.map((item) => {
            const active = item.page === page
            return (
              <button
                key={item.page}
                onClick={() => onNavigate(item.page)}
                className={cn(
                  "relative flex items-center gap-3 rounded-lg px-3 py-2.5 text-sm font-medium transition-colors",
                  active
                    ? "text-sidebar-primary-foreground"
                    : "text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-foreground"
                )}
              >
                {active && (
                  <motion.span
                    layoutId="nav-active"
                    className="absolute inset-0 rounded-lg bg-primary"
                    transition={{ type: "spring", stiffness: 400, damping: 32 }}
                  />
                )}
                <item.icon className="relative z-10 h-[18px] w-[18px]" />
                <span className="relative z-10">{item.label}</span>
              </button>
            )
          })}
        </nav>

        <div className="mt-auto rounded-lg border bg-sidebar-accent p-3">
          <div className="flex items-center gap-2 text-xs">
            <span className="relative flex h-2 w-2">
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary opacity-60" />
              <span className="relative inline-flex h-2 w-2 rounded-full bg-primary" />
            </span>
            <span className="font-medium">网关运行正常</span>
          </div>
          <div className="mt-1 pl-4 text-[10px] text-muted-foreground">FusionGate {version}</div>
        </div>
      </aside>
    </>
  )
}
