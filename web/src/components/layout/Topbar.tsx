import { useEffect, useState } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { Menu, Moon, Sun, LogOut, RefreshCw, ExternalLink } from "lucide-react"
import { Button, buttonVariants } from "@/components/ui/button"
import { useTheme } from "@/providers/theme"
import { useAuth } from "@/providers/auth"

const titles: Record<string, string> = {
  dashboard: "概览",
  authfiles: "认证文件",
  providers: "上游渠道",
  ippool: "IP 池",
  routes: "模型路由",
  keys: "访问密钥",
  usage: "用量与费用",
  requests: "请求账本",
  quality: "质量检测",
}

const GITHUB_URL = "https://github.com/cupid532/fusiongate"

export function Topbar({ page, onMenu }: { page: string; onMenu: () => void }) {
  const { theme, toggle } = useTheme()
  const { logout } = useAuth()
  const queryClient = useQueryClient()
  const [refreshing, setRefreshing] = useState(false)
  const [version, setVersion] = useState("")

  useEffect(() => {
    setVersion(document.querySelector('meta[name="fusiongate-version"]')?.getAttribute("content") ?? "")
  }, [])

  async function refreshAll() {
    setRefreshing(true)
    try {
      // invalidateQueries already resolves once the active refetches settle,
      // so the extra fixed 800ms timer the old version added on top just made
      // the spinner outlast the work it was reporting.
      await queryClient.invalidateQueries()
    } finally {
      setRefreshing(false)
    }
  }

  return (
    <header className="sticky top-0 z-10 flex h-16 items-center justify-between gap-2 border-b bg-background/80 px-4 backdrop-blur-md sm:px-6 md:px-8">
      <div className="flex min-w-0 items-center gap-2">
        <Button variant="ghost" size="icon" onClick={onMenu} aria-label="打开导航" className="md:hidden">
          <Menu className="h-5 w-5" />
        </Button>
        <div className="truncate text-sm font-semibold text-muted-foreground">
          <span className="text-muted-foreground/60">FusionGate</span>
          <span className="mx-2">/</span>
          <span className="text-foreground">{titles[page] ?? "概览"}</span>
          {version && <span className="ml-2 rounded border px-1.5 py-0.5 align-middle text-[10px] font-normal text-muted-foreground">{version}</span>}
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1.5">
        <Button variant="ghost" size="icon" onClick={() => void refreshAll()} aria-label="刷新全部" title="刷新全部数据">
          <RefreshCw className={`h-4 w-4 ${refreshing ? "animate-spin" : ""}`} />
        </Button>
        {/* An anchor rather than window.open: it gets middle-click and
            "open in new window" for free, and rel=noopener stops the opened
            page from holding a reference back to this one via window.opener. */}
        <a
          href={GITHUB_URL}
          target="_blank"
          rel="noreferrer noopener"
          aria-label="打开 GitHub 仓库"
          title="打开 GitHub 仓库"
          className={buttonVariants({ variant: "ghost", size: "icon" })}
        >
          <ExternalLink className="h-4 w-4" />
        </a>
        <Button variant="ghost" size="icon" onClick={toggle} aria-label="切换明暗主题">
          {theme === "light" ? <Moon className="h-4 w-4" /> : <Sun className="h-4 w-4" />}
        </Button>
        <Button variant="ghost" size="icon" onClick={() => void logout()} aria-label="退出登录">
          <LogOut className="h-4 w-4" />
        </Button>
      </div>
    </header>
  )
}
