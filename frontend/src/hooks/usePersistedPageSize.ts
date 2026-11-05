import { useCallback, useEffect, useState } from 'react'

const STORAGE_PREFIX = 'codex2api_page_size_'

/**
 * usePersistedPageSize provides page size state persisted by key.
 *
 * - The initial value prefers the value saved in localStorage; if it is not in allowed or is invalid, it falls back to fallback.
 * - Calling setPageSize also writes the value back to localStorage.
 *
 * @param key       Identifier for the page/table (a codex2api_page_size_ prefix is added when writing to localStorage)
 * @param fallback  Default value
 * @param allowed   Valid pageSize set; persisted values not in it are ignored
 */
export function usePersistedPageSize(
  key: string,
  fallback: number,
  allowed: number[],
): [number, (next: number) => void] {
  const storageKey = `${STORAGE_PREFIX}${key}`

  const readInitial = (): number => {
    if (typeof window === 'undefined') return fallback
    try {
      const raw = window.localStorage.getItem(storageKey)
      if (!raw) return fallback
      const parsed = Number.parseInt(raw, 10)
      if (Number.isFinite(parsed) && allowed.includes(parsed)) {
        return parsed
      }
    } catch {
      /* Silently fall back when localStorage is unavailable */
    }
    return fallback
  }

  const [pageSize, setPageSizeState] = useState<number>(readInitial)

  // When allowed changes, fall back to fallback if the current value is no longer valid
  useEffect(() => {
    if (!allowed.includes(pageSize)) {
      setPageSizeState(fallback)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allowed.join(',')])

  const setPageSize = useCallback(
    (next: number) => {
      if (!allowed.includes(next)) return
      setPageSizeState(next)
      try {
        window.localStorage.setItem(storageKey, String(next))
      } catch {
        /* Silent */
      }
    },
    [storageKey, allowed],
  )

  return [pageSize, setPageSize]
}

/** Site-wide shared page size options; callers can trim them as needed. */
export const DEFAULT_PAGE_SIZE_OPTIONS = [10, 20, 50, 100, 200, 500]
