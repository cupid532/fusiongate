import * as React from "react"

import { cn } from "@/lib/utils"

const Switch = React.forwardRef<
  HTMLButtonElement,
  React.ButtonHTMLAttributes<HTMLButtonElement> & { checked: boolean; onCheckedChange: (v: boolean) => void }
>(({ className, checked, onCheckedChange, ...props }, ref) => (
  <button
    type="button"
    role="switch"
    aria-checked={checked}
    ref={ref}
    onClick={() => onCheckedChange(!checked)}
    className={cn(
      "relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
      checked ? "bg-primary" : "bg-muted-foreground/30",
      className
    )}
    {...props}
  >
    <span
      className={cn(
        "pointer-events-none block h-4 w-4 rounded-full bg-white shadow transition-transform",
        checked ? "translate-x-4" : "translate-x-0"
      )}
    />
  </button>
))
Switch.displayName = "Switch"

export { Switch }
