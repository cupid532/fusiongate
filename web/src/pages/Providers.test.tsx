import { describe, expect, it } from "vitest"
import { reorderProviderIDs } from "@/lib/provider-order"

describe("reorderProviderIDs", () => {
  it("keeps hidden providers in the complete reorder payload", () => {
    // IDs 2 and 4 represent rows omitted by search/status filters.
    expect(reorderProviderIDs([1, 2, 3, 4, 5], 1, 5)).toEqual([2, 3, 4, 1, 5])
  })

  it("moves a visible row upward without dropping hidden IDs", () => {
    expect(reorderProviderIDs([1, 2, 3, 4, 5], 5, 2)).toEqual([1, 5, 2, 3, 4])
  })

  it("does not alter the list for unknown or identical IDs", () => {
    expect(reorderProviderIDs([1, 2, 3], 9, 2)).toEqual([1, 2, 3])
    expect(reorderProviderIDs([1, 2, 3], 2, 2)).toEqual([1, 2, 3])
  })
})
