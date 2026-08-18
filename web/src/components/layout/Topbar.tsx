import { Menu, Moon, Sun, LogOut } from "lucide-react"
import { Button } from "@/components/ui/button"
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

export function Topbar({ page, onMenu }: { page: string; onMenu: () => void }) {
  const { theme, toggle } = useTheme()
  const { logout } = useAuth()

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
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-2">
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
