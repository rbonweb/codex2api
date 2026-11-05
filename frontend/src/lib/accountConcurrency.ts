// Display rules for the account concurrency badge.
//
// Scheduling admission checks "occupied slots >= dynamic_concurrency_limit": dynamic is the configured concurrency
// (account override > group > global default), further lowered at runtime by health, quota protection and so on; it is the effective limit.
// So the denominator uses the effective limit; the numerator uses occupied slots when the session buffer slot is enabled, and real in-flight requests otherwise.
export type AccountConcurrencyInput = {
  active_requests?: number
  occupied_requests?: number
  session_slot_buffer_enabled?: boolean
  dynamic_concurrency_limit?: number
  base_concurrency_effective?: number
}

export type AccountConcurrencyDisplay = {
  active: number
  occupied: number
  buffered: number
  // When the session buffer slot is enabled, show the occupied-slot basis.
  showOccupied: boolean
  // Numerator: used slots, on the same basis as the admission check.
  used: number
  // Effective concurrency limit; null when unknown or <= 0, in which case only the numerator is shown.
  limit: number | null
  // Configured concurrency; null when unknown or <= 0.
  base: number | null
  // The effective limit is below the configured value (temporarily lowered at runtime).
  degraded: boolean
}

function positiveInteger(value: number | undefined): number | null {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return null
  }
  return Math.floor(value)
}

// resolveAccountConcurrencyDisplay returns the badge data; it returns null when there are no in-flight or occupied slots,
// keeping the earlier behavior where idle accounts do not show a concurrency badge.
export function resolveAccountConcurrencyDisplay(
  account: AccountConcurrencyInput,
): AccountConcurrencyDisplay | null {
  const active = Math.max(0, account.active_requests ?? 0)
  const occupied = Math.max(active, account.occupied_requests ?? active)
  if (occupied === 0) return null

  const showOccupied = account.session_slot_buffer_enabled === true
  const limit = positiveInteger(account.dynamic_concurrency_limit)
  const base = positiveInteger(account.base_concurrency_effective)
  return {
    active,
    occupied,
    buffered: occupied - active,
    showOccupied,
    used: showOccupied ? occupied : active,
    limit,
    base,
    degraded: limit !== null && base !== null && limit < base,
  }
}

export function formatAccountConcurrencyText(
  display: AccountConcurrencyDisplay,
): string {
  return display.limit === null
    ? String(display.used)
    : `${display.used} / ${display.limit}`
}
