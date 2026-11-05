import { useEffect, useState } from 'react'

/**
 * useMediaQuery subscribes to the match state of a CSS media query.
 * It is used for mutually exclusive views such as "desktop table / mobile cards", rendering only one copy per viewport,
 * instead of rendering both and hiding one with CSS (on large lists, that doubles the rendering cost).
 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() =>
    typeof window !== 'undefined' ? window.matchMedia(query).matches : false,
  )

  useEffect(() => {
    const mql = window.matchMedia(query)
    setMatches(mql.matches)
    const onChange = (event: MediaQueryListEvent) => setMatches(event.matches)
    mql.addEventListener('change', onChange)
    return () => mql.removeEventListener('change', onChange)
  }, [query])

  return matches
}

/** Tailwind lg breakpoint (1024px), kept consistent with data-table-shell's hidden lg:block. */
export function useIsDesktop(): boolean {
  return useMediaQuery('(min-width: 1024px)')
}
