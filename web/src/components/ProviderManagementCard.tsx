import type { ReactNode } from "react"
import { ChevronDown, ChevronRight } from "lucide-react"
import { cn } from "@/lib/utils"

export function ProviderManagementCard({
  title, summary, children, expanded, onToggle, id, className,
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
    <section className={cn("min-w-0 rounded-xl border bg-card", expanded && "md:col-span-2", className)}>
      <button
        type="button"
        className="flex w-full min-w-0 items-center justify-between gap-3 rounded-xl p-4 text-left hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
        aria-expanded={expanded}
        aria-controls={id}
        onClick={onToggle}
      >
        <span className="min-w-0">
          <span className="block text-sm font-semibold">{title}</span>
          <span className="mt-1 block text-xs text-muted-foreground">{summary}</span>
        </span>
        {expanded ? <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" /> : <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />}
      </button>
      <div id={id} className={cn("min-w-0 border-t p-4", !expanded && "hidden")}>{children}</div>
    </section>
  )
}
