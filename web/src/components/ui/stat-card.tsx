import { cn } from "@/lib/utils"

export function StatCard({
  label,
  value,
  tone = "text-foreground",
  sub,
  icon,
  className,
}: {
  label: string
  value: React.ReactNode
  tone?: string
  sub?: React.ReactNode
  icon?: React.ReactNode
  className?: string
}) {
  return (
    <div className={cn("rounded-xl border bg-card p-4", className)}>
      <div className="flex items-center justify-between">
        <div className="text-xs text-muted-foreground">{label}</div>
        {icon && <div className={cn("grid h-7 w-7 place-items-center rounded-lg", tone.split("text-")[1] ? `bg-${tone.replace("text-", "")}/10` : "bg-muted")}>{icon}</div>}
      </div>
      <div className={cn("mt-2 text-2xl font-bold tracking-tight tabular-nums", tone)}>{value}</div>
      {sub && <div className="mt-1 text-xs text-muted-foreground">{sub}</div>}
    </div>
  )
}
