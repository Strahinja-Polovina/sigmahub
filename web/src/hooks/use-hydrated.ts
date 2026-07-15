import * as React from "react"

// Returns false during SSR and the first client render, true afterwards —
// without a setState-in-effect. Use to gate rendering that would otherwise
// mismatch between server and client (e.g. theme-dependent icons).
export function useHydrated() {
  return React.useSyncExternalStore(
    () => () => {},
    () => true,
    () => false,
  )
}
