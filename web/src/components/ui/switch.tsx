import * as React from "react"
import { motion } from "motion/react"

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
      "relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50",
      checked ? "bg-primary" : "bg-muted-foreground/30",
      className
    )}
    {...props}
  >
    <motion.span
      className="pointer-events-none block h-4 w-4 rounded-full bg-white shadow"
      animate={{ x: checked ? 16 : 0 }}
      transition={{ type: "spring", stiffness: 500, damping: 28 }}
    />
  </button>
))
Switch.displayName = "Switch"

export { Switch }
