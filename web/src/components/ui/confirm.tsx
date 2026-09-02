import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react"
import { AlertTriangle } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"

export type ConfirmOptions = {
  title: string
  description?: ReactNode
  /** Label for the confirming button. Defaults to 「确定」. */
  confirmLabel?: string
  cancelLabel?: string
  /** Renders the confirm button in the destructive style. */
  destructive?: boolean
  /**
   * When set, the confirm button stays disabled until the user types this exact
   * string. Reserve it for the genuinely irreversible operations — clearing the
   * request ledger, for instance, which drops tens of thousands of rows.
   */
  requireText?: string
}

type Pending = ConfirmOptions & { resolve: (ok: boolean) => void }

const ConfirmContext = createContext<((options: ConfirmOptions) => Promise<boolean>) | null>(null)

/**
 * Replaces the browser's native `confirm()`.
 *
 * The native dialog was wrong here for three reasons: it ignores the app's
 * theme entirely (always light, even in the dark console), it blocks the whole
 * JS thread while open, and on mobile it prefixes every message with
 * "api.codelee.de 说：". This renders the same decision in the console's own
 * design language and returns a promise so call sites stay one-liners:
 *
 *   if (await confirm({ title: "删除渠道？", destructive: true })) remove.mutate(id)
 */
// oxlint-disable-next-line react/only-export-components
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<Pending | null>(null)
  const [typed, setTyped] = useState("")
  // Held so an unmount mid-dialog still settles the awaiting caller.
  const pendingRef = useRef<Pending | null>(null)

  const confirm = useCallback((options: ConfirmOptions) => {
    return new Promise<boolean>((resolve) => {
      const entry = { ...options, resolve }
      pendingRef.current = entry
      setTyped("")
      setPending(entry)
    })
  }, [])

  const settle = useCallback((ok: boolean) => {
    const entry = pendingRef.current
    pendingRef.current = null
    setPending(null)
    setTyped("")
    entry?.resolve(ok)
  }, [])

  const gateSatisfied = !pending?.requireText || typed.trim() === pending.requireText

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <Dialog
        open={pending != null}
        // Covers Escape, the overlay click and the close button — all of which
        // mean "no". Without settling here the awaiting promise would hang.
        onOpenChange={(open) => { if (!open) settle(false) }}
      >
        <DialogContent className="max-w-md">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              {pending?.destructive && <AlertTriangle className="h-4 w-4 shrink-0 text-destructive" />}
              {pending?.title}
            </DialogTitle>
            {pending?.description && <DialogDescription>{pending.description}</DialogDescription>}
          </DialogHeader>

          {pending?.requireText && (
            <div className="flex flex-col gap-2">
              <label htmlFor="confirm-gate" className="text-xs text-muted-foreground">
                请输入 <span className="font-mono font-semibold text-foreground">{pending.requireText}</span> 以确认
              </label>
              <Input
                id="confirm-gate"
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                placeholder={pending.requireText}
                autoComplete="off"
                autoFocus
              />
            </div>
          )}

          <DialogFooter>
            <Button variant="outline" onClick={() => settle(false)}>
              {pending?.cancelLabel ?? "取消"}
            </Button>
            <Button
              variant={pending?.destructive ? "destructive" : "default"}
              disabled={!gateSatisfied}
              onClick={() => settle(true)}
              autoFocus={!pending?.requireText}
            >
              {pending?.confirmLabel ?? "确定"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </ConfirmContext.Provider>
  )
}

// oxlint-disable-next-line react/only-export-components
export function useConfirm() {
  const ctx = useContext(ConfirmContext)
  if (!ctx) throw new Error("useConfirm must be used within ConfirmProvider")
  return ctx
}

/** Convenience wrapper for the common "delete this named thing" prompt. */
// oxlint-disable-next-line react/only-export-components
export function useConfirmDelete() {
  const confirm = useConfirm()
  return useMemo(
    () => (what: string, description?: ReactNode) =>
      confirm({ title: `删除${what}？`, description, destructive: true, confirmLabel: "删除" }),
    [confirm]
  )
}
