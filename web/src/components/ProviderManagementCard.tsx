import type { ReactNode } from "react"
import { cn } from "@/lib/utils"

export function ProviderManagementCard({
  title, summary, children, expanded, id, className,
}: {
  title: string
  summary: ReactNode
  children: ReactNode
  expanded: boolean
  onToggle: () => void
  id: string
  className?: string
}) {
  return (
    <section id={id} hidden={!expanded} className={cn("min-w-0 rounded-xl border bg-card", className)}>
      <div className="min-w-0 p-4">
        <span className="min-w-0">
          <span className="block text-sm font-semibold">{title}</span>
          <span className="mt-1 block text-xs text-muted-foreground">{summary}</span>
        </span>
      </div>
      <div className="min-w-0 border-t p-4">{children}</div>
    </section>
  )
}
