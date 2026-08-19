import { Fragment, useMemo } from "react"
import { cn } from "@/lib/utils"

/**
 * Lightweight heatmap: given a matrix of values (rows x cols) with labels,
 * render a color-scaled grid. Used for usage-by-hour / usage-by-day heatmaps.
 */
export function Heatmap({
  matrix,
  rowLabels,
  colLabels,
  formatCell,
  formatTooltip,
  className,
  cellClassName,
}: {
  matrix: number[][] // rows(x) x cols(y)
  rowLabels: string[]
  colLabels: string[]
  formatCell?: (v: number) => string
  formatTooltip?: (row: string, col: string, v: number) => string
  className?: string
  cellClassName?: string
}) {
  const max = useMemo(() => Math.max(1, ...matrix.flat()), [matrix])

  function alpha(v: number) {
    if (v <= 0) return 0
    const t = Math.log1p(v) / Math.log1p(max)
    return 0.12 + t * 0.88
  }

  return (
    <div className={cn("w-full overflow-x-auto", className)}>
      <div className="min-w-max">
        <div className="grid gap-1" style={{ gridTemplateColumns: `auto repeat(${colLabels.length}, minmax(30px, 1fr))` }}>
          <div />
          {colLabels.map((c, i) => (
            <div key={i} className="pb-1 text-center text-[10px] text-muted-foreground">{c}</div>
          ))}
          {matrix.map((row, ri) => (
            <Fragment key={ri}>
              <div className="pr-2 text-right text-[10px] text-muted-foreground">{rowLabels[ri]}</div>
              {row.map((v, ci) => (
                <div
                  key={ci}
                  className={cn("grid aspect-square min-h-[22px] place-items-center rounded-[4px] text-[9px] tabular-nums text-white/90", cellClassName)}
                  style={{ backgroundColor: v > 0 ? `rgba(56, 189, 248, ${alpha(v)})` : "transparent", outline: v > 0 ? "none" : "1px solid var(--border, #e2e8f0)" }}
                  title={formatTooltip ? formatTooltip(rowLabels[ri], colLabels[ci], v) : `${rowLabels[ri]} / ${colLabels[ci]}: ${formatCell ? formatCell(v) : v}`}
                >
                  {v > 0 && (formatCell ? formatCell(v) : v)}
                </div>
              ))}
            </Fragment>
          ))}
        </div>
      </div>
    </div>
  )
}
