/// <reference types="node" />
// @vitest-environment node

import { execFileSync } from "node:child_process"
import { readFileSync } from "node:fs"
import { ModuleKind, ScriptTarget, transpileModule } from "typescript"
import { describe, expect, it } from "vitest"
import { exactTimeRange } from "./exact-time-range"

function localParts(iso: string) {
  const d = new Date(iso)
  return [d.getFullYear(), d.getMonth() + 1, d.getDate(), d.getHours(), d.getMinutes()]
}

describe("exactTimeRange", () => {
  it("uses exact local year and month boundaries", () => {
    expect(localParts(exactTimeRange("year", "2024")!.from)).toEqual([2024, 1, 1, 0, 0])
    expect(localParts(exactTimeRange("year", "2024")!.until)).toEqual([2025, 1, 1, 0, 0])
    expect(localParts(exactTimeRange("month", "2024-02")!.until)).toEqual([2024, 3, 1, 0, 0])
    expect(localParts(exactTimeRange("month", "2023-02")!.until)).toEqual([2023, 3, 1, 0, 0])
  })

  it("handles leap days and exclusive day boundaries", () => {
    const range = exactTimeRange("day", "2024-02-29")!
    expect(localParts(range.from)).toEqual([2024, 2, 29, 0, 0])
    expect(localParts(range.until)).toEqual([2024, 3, 1, 0, 0])
    expect(exactTimeRange("day", "2023-02-29")).toBeNull()
  })

  it("advances hour and minute buckets exactly", () => {
    expect(localParts(exactTimeRange("hour", "2024-12-31T23")!.until)).toEqual([2025, 1, 1, 0, 0])
    expect(localParts(exactTimeRange("minute", "2024-12-31T23:59")!.until)).toEqual([2025, 1, 1, 0, 0])
  })

  it("rejects ambiguous years and malformed hour or minute inputs", () => {
    expect(exactTimeRange("year", "0099")).toBeNull()
    expect(exactTimeRange("month", "0099-12")).toBeNull()
    expect(localParts(exactTimeRange("hour", "2024-01-01T12:30")!.from)).toEqual([2024, 1, 1, 12, 0])
    expect(exactTimeRange("hour", "2024-01-01T12:00")).not.toBeNull()
    expect(exactTimeRange("minute", "2024-01-01T12")).toBeNull()
  })
})

// Launch with TZ before Node initializes Date; these tests also run on UTC hosts.
function newYorkRanges(cases: Parameters<typeof exactTimeRange>[]) {
  const source = transpileModule(readFileSync(new URL("./exact-time-range.ts", import.meta.url), "utf8"), {
    compilerOptions: { module: ModuleKind.ESNext, target: ScriptTarget.ES2022 },
  }).outputText
  const script = `${source}\nconsole.log(JSON.stringify({
    zone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    ranges: ${JSON.stringify(cases)}.map(([granularity, value]) => exactTimeRange(granularity, value))
  }))`
  const result = JSON.parse(execFileSync(process.execPath, ["--input-type=module", "--eval", script], {
    env: { ...process.env, TZ: "America/New_York" },
    encoding: "utf8",
    timeout: 10_000,
  })) as { zone: string; ranges: ReturnType<typeof exactTimeRange>[] }
  expect(result.zone).toBe("America/New_York")
  return result.ranges
}

describe("exactTimeRange across New York DST transitions", () => {
  it("keeps calendar days at 23/25 hours", () => {
    const [spring, fall] = newYorkRanges([["day", "2024-03-10"], ["day", "2024-11-03"]])
    expect(spring).toEqual({ from: "2024-03-10T05:00:00.000Z", until: "2024-03-11T04:00:00.000Z" })
    expect(fall).toEqual({ from: "2024-11-03T04:00:00.000Z", until: "2024-11-04T05:00:00.000Z" })
  })

  it("uses absolute hour/minute durations and the first repeated local time", () => {
    expect(newYorkRanges([
      ["hour", "2024-03-10T01:00"],
      ["minute", "2024-03-10T01:59"],
      ["hour", "2024-11-03T01:00"],
      ["minute", "2024-11-03T01:59"],
    ])).toEqual([
      { from: "2024-03-10T06:00:00.000Z", until: "2024-03-10T07:00:00.000Z" },
      { from: "2024-03-10T06:59:00.000Z", until: "2024-03-10T07:00:00.000Z" },
      { from: "2024-11-03T05:00:00.000Z", until: "2024-11-03T06:00:00.000Z" },
      { from: "2024-11-03T05:59:00.000Z", until: "2024-11-03T06:00:00.000Z" },
    ])
  })

  it("rejects nonexistent spring-forward hours and minutes", () => {
    expect(newYorkRanges([["hour", "2024-03-10T02:00"], ["minute", "2024-03-10T02:30"]])).toEqual([null, null])
  })
})
