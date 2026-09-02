import { useState } from "react"
import { motion } from "motion/react"
import { Octagon, ShieldCheck, Database, EyeOff } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
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
    <div className="grid min-h-screen grid-cols-1 lg:grid-cols-[minmax(360px,1fr)_minmax(420px,560px)]">
      <motion.div
        className="hidden flex-col justify-between overflow-hidden px-[7vw] py-14 lg:flex"
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        transition={{ duration: 0.6 }}
      >
        <div className="flex items-center gap-3">
          <div className="grid h-10 w-10 place-items-center rounded-xl bg-gradient-to-br from-[#66ab71] to-[#458554] text-white">
            <Octagon className="h-6 w-6" />
          </div>
          <span className="text-lg font-bold tracking-tight">FusionGate</span>
        </div>

        <div className="my-8 max-w-[650px]">
          <motion.h1
            className="text-[clamp(42px,5.8vw,78px)] font-bold leading-[1.02] tracking-[-4px]"
            initial={{ opacity: 0, y: 16 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.6, delay: 0.1 }}
          >
            One gateway.
            <br />
            <span className="text-primary">Every model.</span>
          </motion.h1>
          <motion.p
            className="mt-6 max-w-[580px] text-[17px] leading-relaxed text-muted-foreground"
            initial={{ opacity: 0, y: 16 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.6, delay: 0.2 }}
          >
            把分散的 AI Provider、模型别名和访问密钥收拢到一个清晰、私有、可审计的控制平面。
          </motion.p>

          <motion.div
            className="mt-9 flex flex-wrap gap-3"
            initial={{ opacity: 0, y: 16 }}
            animate={{ opacity: 1, y: 0 }}
            transition={{ duration: 0.6, delay: 0.3 }}
          >
            {features.map((f) => (
              <div
                key={f.label}
                className="flex items-center gap-2 rounded-full border bg-background/60 px-3.5 py-2 text-xs text-foreground/80"
              >
                <span className="h-1.5 w-1.5 rounded-full bg-primary" />
                {f.label}
              </div>
            ))}
          </motion.div>
        </div>

        <div className="text-xs text-muted-foreground/60">Self-hosted · Private by default · OpenAI compatible</div>
      </motion.div>

      <div className="flex items-center border-t bg-card/60 px-6 py-12 backdrop-blur-xl lg:border-l lg:border-t-0 lg:px-16">
        <motion.form
          onSubmit={handleSubmit}
          className="mx-auto w-full max-w-[390px]"
          initial={{ opacity: 0, x: 24 }}
          animate={{ opacity: 1, x: 0 }}
          transition={{ duration: 0.5, delay: 0.15 }}
        >
          <div className="mb-8 flex items-center gap-3 lg:hidden">
            <div className="grid h-9 w-9 place-items-center rounded-xl bg-gradient-to-br from-[#66ab71] to-[#458554] text-white">
              <Octagon className="h-5 w-5" />
            </div>
            <span className="text-lg font-bold tracking-tight">FusionGate</span>
          </div>
          <div className="mb-4 text-[11px] font-bold uppercase tracking-[0.18em] text-primary">
            Administrator Console
          </div>
          <h2 className="text-3xl font-semibold tracking-tight">欢迎回来</h2>
          <p className="mt-2 mb-8 text-sm text-muted-foreground">登录以管理你的模型网关。</p>

          <div className="flex flex-col gap-6">
            <div className="flex flex-col gap-2">
              <Label htmlFor="password">管理员密码</Label>
              <Input
                id="password"
                // name + autocomplete are what let a password manager offer to
                // save this credential and fill it next time. Without them the
                // field was invisible to 1Password, Keychain and friends.
                name="password"
                type="password"
                autoComplete="current-password"
                placeholder="输入管理员密码"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="h-11"
                autoFocus
              />
            </div>
            {error && <div className="text-sm text-destructive">{error}</div>}
            <Button type="submit" disabled={submitting} className="h-11">
              {submitting ? "登录中…" : "进入控制台"}
            </Button>
          </div>
        </motion.form>
      </div>
    </div>
  )
}
