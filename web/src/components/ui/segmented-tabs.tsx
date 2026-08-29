import { cn } from "@/lib/utils"

interface SegmentedTab<T extends string> {
  value: T
  label: React.ReactNode
  count?: number
}

export function SegmentedTabs<T extends string>({
  tabs,
  value,
  onChange,
  className,
}: {
  tabs: SegmentedTab<T>[]
  value: T
  onChange: (v: T) => void
  className?: string
}) {
  return (
    <div role="tablist" className={cn("inline-flex flex-wrap items-center gap-1 rounded-lg bg-muted p-1", className)}>
      {tabs.map((tab) => {
        const active = tab.value === value
        return (
          <button
            key={tab.value}
            role="tab"
            aria-selected={active}
            onClick={() => onChange(tab.value)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
              active ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground"
            )}
          >
            {tab.label}
            {typeof tab.count === "number" && (
              <span className={cn("rounded-full px-1.5 text-[10px] tabular-nums", active ? "bg-primary/10 text-primary" : "bg-muted-foreground/10")}>
                {tab.count}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}
