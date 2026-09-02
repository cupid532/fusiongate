import { motion } from "motion/react"
import { useEffect, useMemo, useState } from "react"
import { useQuery } from "@tanstack/react-query"
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
import { api } from "@/lib/api"
import type { Provider } from "@/lib/types"
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

const PAGES = new Set<string>(navItems.map((item) => item.page))

/**
 * Type guard used by the router to reject unknown location hashes.
 * Lives here so the nav list stays the single source of truth for what pages
 * exist. The warning is about hot-reload boundaries only.
 */
// oxlint-disable-next-line react/only-export-components
export function isPage(value: string): value is Page {
  return PAGES.has(value)
}

type GatewayHealth = { tone: "ok" | "warn" | "down"; label: string; detail?: string }

// The footer badge used to be the hard-coded string "网关运行正常" next to a
// pulsing green dot — it claimed the gateway was fine no matter what was
// actually happening. This derives it from the providers list, which the
// console already has cached, so it costs no extra request and tells the truth.
function deriveHealth(providers: Provider[] | undefined, failed: boolean): GatewayHealth {
  if (failed) return { tone: "down", label: "无法连接网关" }
  if (!providers) return { tone: "warn", label: "正在检查状态" }

  const active = providers.filter((p) => p.enabled && !p.archived)
  if (active.length === 0) {
    return { tone: "down", label: "没有可用渠道", detail: "所有渠道均已停用或归档" }
  }

  const broken = active.filter(
    (p) => p.status === "circuit_open" || p.health_check_status === "unhealthy"
  )
  const unstable = active.filter((p) => p.consecutive_failures > 0 && !broken.includes(p))

  if (broken.length > 0) {
    return {
      tone: "warn",
      label: `${broken.length}/${active.length} 个渠道异常`,
      detail: broken.length === active.length ? "全部渠道不可用" : "其余渠道仍在承接流量",
    }
  }
  if (unstable.length > 0) {
    return { tone: "warn", label: `${unstable.length} 个渠道不稳定`, detail: "存在连续失败记录" }
  }
  return { tone: "ok", label: "网关运行正常", detail: `${active.length} 个渠道参与调度` }
}

const toneDot: Record<GatewayHealth["tone"], string> = {
  ok: "bg-primary",
  warn: "bg-amber-500",
  down: "bg-destructive",
}

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

  // Shares the ["providers"] cache with the pages that already load it.
  const { data: providers, isError } = useQuery({
    queryKey: ["providers"],
    queryFn: () => api<Provider[]>("/api/admin/providers"),
    staleTime: 30_000,
  })
  const health = useMemo(() => deriveHealth(providers, isError), [providers, isError])

  return (
    <>
      {open && (
        // A plain div, not a button: a full-viewport <button> was announced to
        // screen readers as an enormous unlabelled control and sat in the tab
        // order. Escape (handled in App) and the × are the accessible paths;
        // this is just the pointer affordance.
        <div
          onClick={onClose}
          aria-hidden="true"
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
                aria-current={active ? "page" : undefined}
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
            <span className="relative flex h-2 w-2 shrink-0">
              {/* The ping animation is for the healthy state only — an animated
                  "everything is fine" pulse on a degraded gateway reads wrong. */}
              {health.tone === "ok" && (
                <span className={cn("absolute inline-flex h-full w-full animate-ping rounded-full opacity-60", toneDot[health.tone])} />
              )}
              <span className={cn("relative inline-flex h-2 w-2 rounded-full", toneDot[health.tone])} />
            </span>
            <span className="min-w-0 truncate font-medium" title={health.detail}>{health.label}</span>
          </div>
          {health.detail && <div className="mt-1 pl-4 text-[10px] text-muted-foreground">{health.detail}</div>}
          <div className="mt-1 pl-4 text-[10px] text-muted-foreground">FusionGate {version}</div>
        </div>
      </aside>
    </>
  )
}
