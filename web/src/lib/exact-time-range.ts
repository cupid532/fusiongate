export type TimeGranularity = "year" | "month" | "day" | "hour" | "minute"

export interface ExactTimeRange {
  from: string
  until: string
}

function validDate(date: Date): boolean {
  return !Number.isNaN(date.getTime())
}

/** Converts a browser-local calendar bucket to RFC3339 bounds with an exclusive end. */
export function exactTimeRange(granularity: TimeGranularity, value: string): ExactTimeRange | null {
  let from: Date
  let until: Date

  if (granularity === "year") {
    if (!/^\d{4}$/.test(value)) return null
    const year = Number(value)
    if (year < 1000 || year > 9998) return null
    from = new Date(year, 0, 1)
    until = new Date(year + 1, 0, 1)
  } else if (granularity === "month") {
    const match = /^(\d{4})-(\d{2})$/.exec(value)
    if (!match) return null
    const year = Number(match[1])
    const month = Number(match[2])
    if (year < 1000 || year > 9998 || month < 1 || month > 12) return null
    from = new Date(year, month - 1, 1)
    until = new Date(year, month, 1)
  } else {
    const match = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2})(?::(\d{2}))?)?$/.exec(value)
    if (!match) return null
    const year = Number(match[1])
    const month = Number(match[2])
    const day = Number(match[3])
    const rawHour = match[4]
    const rawMinute = match[5]
    if (granularity !== "day" && rawHour == null) return null
    if (granularity === "minute" && rawMinute == null) return null
    const hour = granularity === "day" ? 0 : Number(rawHour)
    const minute = granularity === "minute" ? Number(rawMinute) : 0
    if (year < 1000 || year > 9998 || month < 1 || month > 12 || day < 1 || day > 31 || hour < 0 || hour > 23 || minute < 0 || minute > 59) return null
    from = new Date(year, month - 1, day, hour, minute)
    if (!validDate(from) || from.getFullYear() !== year || from.getMonth() !== month - 1 || from.getDate() !== day || from.getHours() !== hour || from.getMinutes() !== minute) return null
    until = new Date(from)
    if (granularity === "day") until.setDate(until.getDate() + 1)
    else if (granularity === "hour") until = new Date(from.getTime() + 60 * 60 * 1000)
    else until = new Date(from.getTime() + 60 * 1000)
  }

  if (!validDate(from) || !validDate(until)) return null
  return { from: from.toISOString(), until: until.toISOString() }
}
