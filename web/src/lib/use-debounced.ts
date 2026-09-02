import { useEffect, useState } from "react"

/**
 * Returns `value` after it has stopped changing for `delay` ms.
 *
 * Used for search boxes whose value feeds a query key. Typing "gpt-4o" into
 * the request-ledger filter previously issued six separate queries against a
 * table with tens of thousands of rows — one per keystroke — and raced their
 * responses. Debouncing collapses that to one request per pause.
 */
export function useDebounced<T>(value: T, delay = 300): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delay)
    return () => window.clearTimeout(timer)
  }, [value, delay])

  return debounced
}
