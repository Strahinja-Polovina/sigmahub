import * as React from "react"

const MOBILE_BREAKPOINT = 768

// useSyncExternalStore reads matchMedia without a setState-in-effect: the
// server snapshot is false, the client subscribes to breakpoint changes.
export function useIsMobile() {
  return React.useSyncExternalStore(
    (onChange) => {
      const mql = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`)
      mql.addEventListener("change", onChange)
      return () => mql.removeEventListener("change", onChange)
    },
    () => window.innerWidth < MOBILE_BREAKPOINT,
    () => false,
  )
}
