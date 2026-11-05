import { useEffect, useState } from 'react'
import { api } from '../api'

// "Show full usage numbers" is a global setting, and one screen often has a dozen or more components that format numbers.
// This does module-level caching plus shared in-flight requests: the first frame uses the last value directly (no flash of compact units),
// and a calibration fetch runs on mount; even when several components mount at once, only one request is sent.
let cachedValue = false
let inFlight: Promise<boolean> | null = null

function fetchShowFullUsageNumbers(): Promise<boolean> {
  if (inFlight) return inFlight
  inFlight = api
    .getSettings()
    .then((settings) => {
      cachedValue = Boolean(settings.show_full_usage_numbers)
      return cachedValue
    })
    .catch(() => cachedValue)
    .finally(() => {
      inFlight = null
    })
  return inFlight
}

/**
 * Reads the "Show full usage numbers" setting. When on, numbers use full thousands separators; when off, they use the K/M/B/T compact units.
 */
export function useShowFullUsageNumbers(): boolean {
  const [value, setValue] = useState(cachedValue)

  useEffect(() => {
    let cancelled = false
    void fetchShowFullUsageNumbers().then((next) => {
      if (!cancelled) setValue(next)
    })
    return () => {
      cancelled = true
    }
  }, [])

  return value
}
