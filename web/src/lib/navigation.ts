import { useEffect, useMemo, useState } from "react"

export function usePageParams() {
  const [hash, setHash] = useState(() => location.hash)
  useEffect(() => {
    const update = () => setHash(location.hash)
    window.addEventListener("hashchange", update)
    return () => window.removeEventListener("hashchange", update)
  }, [])
  return useMemo(() => new URLSearchParams(hash.split("?").slice(1).join("?")), [hash])
}
