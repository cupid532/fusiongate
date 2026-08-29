import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary/15 text-primary",
        success: "border-transparent bg-emerald-500/15 text-emerald-700 dark:text-emerald-400",
        danger: "border-transparent bg-destructive/15 text-destructive",
        warning: "border-transparent bg-amber-500/15 text-amber-700 dark:text-amber-400",
        neutral: "border-transparent bg-muted text-muted-foreground",
        outline: "text-foreground",
      },
    },
    defaultVariants: { variant: "default" },
  }
)

type BadgeProps = React.HTMLAttributes<HTMLDivElement> & VariantProps<typeof badgeVariants>

function Badge({ className, variant, ...props }: BadgeProps) {
  return <div className={cn(badgeVariants({ variant }), className)} {...props} />
}

export { Badge }
