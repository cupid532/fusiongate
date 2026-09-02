import { useCallback, useEffect, useRef, useState } from "react"
import { AnimatePresence, motion } from "motion/react"
import { CheckCircle2, AlertTriangle, Info, X } from "lucide-react"
import { cn } from "@/lib/utils"
import { subscribeToasts, type Toast, type ToastTone } from "@/lib/notify"

const toneStyles: Record<ToastTone, { ring: string; icon: typeof Info; iconClass: string }> = {
  success: { ring: "border-primary/40", icon: CheckCircle2, iconClass: "text-primary" },
  error: { ring: "border-destructive/50", icon: AlertTriangle, iconClass: "text-destructive" },
  info: { ring: "border-border", icon: Info, iconClass: "text-muted-foreground" },
}

/**
 * Renders the app's toast stack.
 *
 * Mounted once, near the root. Everything that needs to say something to the
 * user pushes through `lib/notify`, including the global query/mutation error
 * handlers — which is what gives every one of the app's mutations feedback on
 * failure without each call site wiring up its own onError.
 */
export function Toaster() {
  const [toasts, setToasts] = useState<Toast[]>([])
  const timers = useRef(new Map<number, number>())

  const dismiss = useCallback((id: number) => {
    setToasts((list) => list.filter((t) => t.id !== id))
    const timer = timers.current.get(id)
    if (timer != null) {
      window.clearTimeout(timer)
      timers.current.delete(id)
    }
  }, [])

  useEffect(() => {
    // Captured so the cleanup closes over this exact Map rather than reading
    // `.current` after the component has gone.
    const pending = timers.current
    const unsubscribe = subscribeToasts((toast) => {
      setToasts((list) => {
        // Identical back-to-back messages (e.g. a fan-out where every request
        // fails the same way) collapse into one entry rather than a wall.
        const duplicate = list.find((t) => t.title === toast.title && t.description === toast.description)
        if (duplicate) return list
        // Keep the stack bounded so a burst can never cover the whole viewport.
        return [...list, toast].slice(-4)
      })
      const timer = window.setTimeout(() => dismiss(toast.id), toast.duration)
      pending.set(toast.id, timer)
    })
    return () => {
      unsubscribe()
      for (const timer of pending.values()) window.clearTimeout(timer)
      pending.clear()
    }
  }, [dismiss])

  return (
    <div
      // aria-live so screen readers announce toasts; pointer-events-none on the
      // container keeps the rest of the page clickable behind the stack.
      aria-live="polite"
      aria-atomic="false"
      className="pointer-events-none fixed inset-x-0 bottom-0 z-[100] flex flex-col items-center gap-2 p-4 sm:inset-x-auto sm:right-0 sm:items-end"
    >
      <AnimatePresence initial={false}>
        {toasts.map((toast) => {
          const { ring, icon: Icon, iconClass } = toneStyles[toast.tone]
          return (
            <motion.div
              key={toast.id}
              layout
              initial={{ opacity: 0, y: 12, scale: 0.97 }}
              animate={{ opacity: 1, y: 0, scale: 1 }}
              exit={{ opacity: 0, y: 8, scale: 0.97 }}
              transition={{ duration: 0.18 }}
              role={toast.tone === "error" ? "alert" : "status"}
              className={cn(
                "pointer-events-auto flex w-full max-w-[26rem] items-start gap-3 rounded-xl border bg-popover px-4 py-3 text-popover-foreground shadow-lg",
                ring
              )}
            >
              <Icon className={cn("mt-0.5 h-4 w-4 shrink-0", iconClass)} />
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium">{toast.title}</div>
                {toast.description && (
                  <div className="mt-0.5 break-words text-xs text-muted-foreground">{toast.description}</div>
                )}
              </div>
              <button
                type="button"
                onClick={() => dismiss(toast.id)}
                aria-label="关闭提示"
                className="-mr-1 -mt-1 rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                <X className="h-3.5 w-3.5" />
              </button>
            </motion.div>
          )
        })}
      </AnimatePresence>
    </div>
  )
}
