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

/* ── Topology node definitions ─────────────────────────────────── */

const leftNodes = [
  { label: "OpenAI", y: 150 },
  { label: "Anthropic", y: 250 },
  { label: "Gemini", y: 350 },
  { label: "Grok", y: 450 },
]

const rightNodes = [
  { label: "Chat", y: 150 },
  { label: "Responses", y: 250 },
  { label: "Images", y: 350 },
  { label: "Audio", y: 450 },
]

const HUB = { x: 400, y: 300 }
const LX = 120
const RX = 680

/* ── Decorative SVG background ─────────────────────────────────── */

function TopologyBackground() {
  return (
    <div className="pointer-events-none absolute inset-0 overflow-hidden" aria-hidden="true">
      <svg
        className="absolute inset-0 h-full w-full text-foreground"
        viewBox="0 0 800 600"
        preserveAspectRatio="xMidYMid slice"
        fill="none"
      >
        <style>{`
          @keyframes fg-dash  { to { stroke-dashoffset: -40; } }
          @keyframes fg-pulse { 0%,100% { opacity:.6 } 50% { opacity:1 } }
          @keyframes fg-dot   { 0%,100% { opacity:.07 } 50% { opacity:.18 } }
        `}</style>

        <defs>
          <filter id="fg-glow" x="-50%" y="-50%" width="200%" height="200%">
            <feGaussianBlur stdDeviation="10" result="b" />
            <feComposite in="SourceGraphic" in2="b" operator="over" />
          </filter>
        </defs>

        {/* Dot grid */}
        {Array.from({ length: 16 }, (_, r) =>
          Array.from({ length: 20 }, (_, c) => (
            <circle
              key={`${r}-${c}`}
              cx={c * 42 + 21}
              cy={r * 40 + 20}
              r="1.2"
              fill="currentColor"
              style={{
                opacity: 0.07,
                animation: `fg-dot ${3.5 + ((r + c) % 5) * 0.6}s ease-in-out ${((r * 20 + c) % 9) * 0.35}s infinite`,
              }}
            />
          )),
        )}

        {/* Left -> Hub connection lines */}
        {leftNodes.map((n, i) => (
          <path
            key={`ll-${i}`}
            d={`M${LX + 48},${n.y} C${LX + 170},${n.y} ${HUB.x - 110},${HUB.y} ${HUB.x - 28},${HUB.y}`}
            stroke="var(--primary)"
            strokeWidth="1.2"
            strokeDasharray="6 6"
            opacity="0.22"
            style={{ animation: `fg-dash ${2.4 + i * 0.35}s linear infinite` }}
          />
        ))}

        {/* Hub -> Right connection lines */}
        {rightNodes.map((n, i) => (
          <path
            key={`rl-${i}`}
            d={`M${HUB.x + 28},${HUB.y} C${HUB.x + 110},${HUB.y} ${RX - 170},${n.y} ${RX - 48},${n.y}`}
            stroke="var(--primary)"
            strokeWidth="1.2"
            strokeDasharray="6 6"
            opacity="0.22"
            style={{ animation: `fg-dash ${2.4 + i * 0.35}s linear infinite` }}
          />
        ))}

        {/* Left nodes (providers) */}
        {leftNodes.map((n, i) => (
          <g key={`ln-${i}`} style={{ animation: `fg-pulse ${3.2 + i * 0.5}s ease-in-out ${i * 0.4}s infinite` }}>
            <circle cx={LX} cy={n.y} r="26" fill="var(--primary)" opacity="0.07" />
            <circle cx={LX} cy={n.y} r="5" fill="var(--primary)" opacity="0.45" />
            <text x={LX} y={n.y + 40} textAnchor="middle" fill="currentColor" fontSize="10.5" fontWeight="500" opacity="0.3">
              {n.label}
            </text>
          </g>
        ))}

        {/* Central hub */}
        <g filter="url(#fg-glow)">
          <circle cx={HUB.x} cy={HUB.y} r="44" fill="var(--primary)" opacity="0.05" />
          <circle cx={HUB.x} cy={HUB.y} r="22" fill="var(--primary)" opacity="0.10" />
          <circle cx={HUB.x} cy={HUB.y} r="8" fill="var(--primary)" opacity="0.55" />
        </g>
        <text x={HUB.x} y={HUB.y + 60} textAnchor="middle" fill="var(--primary)" fontSize="12" fontWeight="600" opacity="0.40">
          FusionGate
        </text>

        {/* Right nodes (outputs) */}
        {rightNodes.map((n, i) => (
          <g key={`rn-${i}`} style={{ animation: `fg-pulse ${3.2 + i * 0.5}s ease-in-out ${i * 0.4 + 0.2}s infinite` }}>
            <circle cx={RX} cy={n.y} r="26" fill="var(--primary)" opacity="0.07" />
            <circle cx={RX} cy={n.y} r="5" fill="var(--primary)" opacity="0.45" />
            <text x={RX} y={n.y + 40} textAnchor="middle" fill="currentColor" fontSize="10.5" fontWeight="500" opacity="0.3">
              {n.label}
            </text>
          </g>
        ))}
      </svg>

      {/* Gradient overlays so text remains readable on top */}
      <div className="absolute inset-0 bg-gradient-to-b from-background/85 via-background/25 to-background/80" />
      <div className="absolute inset-0 bg-gradient-to-r from-background/60 via-transparent to-background/40" />
    </div>
  )
}

/* ── Login page ────────────────────────────────────────────────── */

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
      {/* ── Left panel: branding + topology background ── */}
      <motion.div
        className="relative hidden flex-col justify-between overflow-hidden px-[7vw] py-14 lg:flex"
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        transition={{ duration: 0.6 }}
      >
        <TopologyBackground />

        <div className="relative z-10 flex items-center gap-3">
          <div className="grid h-10 w-10 place-items-center rounded-xl bg-gradient-to-br from-[#66ab71] to-[#458554] text-white">
            <Octagon className="h-6 w-6" />
          </div>
          <span className="text-lg font-bold tracking-tight">FusionGate</span>
        </div>

        <div className="relative z-10 my-8 max-w-[650px]">
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

        <div className="relative z-10 text-xs text-muted-foreground/60">
          Self-hosted · Private by default · OpenAI compatible
        </div>
      </motion.div>

      {/* ── Right panel: login form (untouched) ── */}
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
