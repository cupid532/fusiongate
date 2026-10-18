import { useState } from "react"
import { ArrowUpRight, Octagon, ShieldCheck, Database, EyeOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { GatewayArt } from "@/components/GatewayArt"
import { useAuth } from "@/providers/auth"

const features = [
  { icon: ShieldCheck, label: "凭据本地加密" },
  { icon: EyeOff, label: "请求内容零存储" },
  { icon: Database, label: "SQLite 无依赖" },
]

export function Login() {
  const { login } = useAuth()
  const [password, setPassword] = useState("")
  const [error, setError] = useState("")
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!password || submitting) return
    setSubmitting(true)
    setError("")
    try {
      await login(password)
    } catch (err) {
      setError(err instanceof Error ? err.message : "登录失败")
      setSubmitting(false)
    }
  }

  return (
    <main className="login-scene">
      <section className="login-story" aria-label="FusionGate 模型网关">
        <a className="brand-lockup" href="#dashboard" aria-label="FusionGate 首页">
          <span className="brand-mark"><Octagon size={23} /></span>
          <span>FusionGate<span className="brand-caption">THE AI CONTROL PLANE</span></span>
        </a>
        <div className="login-poster">
          <div className="eyebrow">PRIVATE INFRASTRUCTURE / UNIFIED INTELLIGENCE</div>
          <h1>ONE GATE.<br /><span className="type-outline">EVERY AI.</span></h1>
          <GatewayArt className="login-sculpture" />
          <div className="login-story-copy">
            <span className="eyebrow">分散的智能，统一的入口。</span>
            <p>连接不同模型，让每一次调用<br />都有清晰的路由、权限与记录。</p>
          </div>
          <span className="orbit-caption">MANY MODELS.<br />ONE GRAVITY CENTER. ↗</span>
        </div>
        <div className="login-story-footer">
          <span>SELF-HOSTED. BY DESIGN.</span>
          <span>OpenAI compatible ↗</span>
        </div>
      </section>

      <section className="login-access" aria-label="管理员登录">
        <div className="login-access-top"><span className="eyebrow">CONSOLE ACCESS</span><ShieldCheck size={18} /></div>
        <form id="fusiongate-admin-login" autoComplete="on" onSubmit={handleSubmit} className="login-form">
          <div className="eyebrow mb-5">01 / ADMINISTRATOR</div>
          <h2>欢迎回来<span className="text-primary">。</span></h2>
          <p className="mt-3 mb-10 text-sm leading-relaxed text-muted-foreground">你的模型宇宙，尽在掌控。<br />使用管理员密码进入私有控制台。</p>
          <div className="flex flex-col gap-3">
            <Label htmlFor="password">管理员密码</Label>
            <Input id="password" name="password" type="password" autoComplete="current-password" placeholder="输入管理员密码" value={password} onChange={(e) => setPassword(e.target.value)} className="h-12" autoFocus required aria-describedby={error ? "login-error" : undefined} aria-invalid={!!error} />
            {error && <div id="login-error" role="alert" className="text-sm text-destructive">{error}</div>}
            <Button type="submit" disabled={submitting || !password} className="mt-3 h-12 justify-between px-5">
              {submitting ? "登录中…" : "进入控制台"}<ArrowUpRight className="h-4 w-4" />
            </Button>
          </div>
          <div className="login-assurances">
            {features.map((f) => <span key={f.label}><f.icon size={13} />{f.label}</span>)}
          </div>
        </form>
        <div className="login-access-bottom"><span>FusionGate / 私有部署</span><span>YOUR MODELS. YOUR RULES.</span></div>
      </section>
    </main>
  )
}
