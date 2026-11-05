// Display label for the long window (7d slot). plus/pro are weekly windows (7d), while free/team plans are actually monthly windows (about 30 days),
// A hard-coded 7d label is misleading (issue #324). Prefer the window type the backend detects, then derive it from the real period in seconds,
// fall back to '7d' when both are unknown (matching the backend slot naming).
export function formatLongUsageWindowLabel(account: {
  usage_window_7d_kind?: string
  usage_window_7d_seconds?: number
}): string {
  if (account.usage_window_7d_kind === 'monthly') return '30d'
  const seconds = account.usage_window_7d_seconds
  if (typeof seconds === 'number' && Number.isFinite(seconds) && seconds > 0) {
    const days = Math.round(seconds / 86_400)
    if (days >= 1) return `${days}d`
    const hours = Math.round(seconds / 3_600)
    if (hours >= 1) return `${hours}h`
  }
  return '7d'
}

export function formatUsageNumber(
  value?: number | null,
  showFullNumbers = false,
  locale?: Intl.LocalesArgument,
): string {
  if (value === undefined || value === null) return '0'

  const numericValue = Number(value)
  if (!Number.isFinite(numericValue)) return '0'

  const roundedValue = Math.round(numericValue)
  if (showFullNumbers) return roundedValue.toLocaleString(locale)

  const absValue = Math.abs(numericValue)
  const units = [
    { value: 1_000_000_000_000, suffix: 'T' },
    { value: 1_000_000_000, suffix: 'B' },
    { value: 1_000_000, suffix: 'M' },
    { value: 1_000, suffix: 'K' },
  ]
  const unit = units.find((item) => absValue >= item.value)
  if (!unit) return roundedValue.toLocaleString(locale)

  const scaled = numericValue / unit.value
  const fractionDigits = Math.abs(scaled) >= 100 ? 0 : Math.abs(scaled) >= 10 ? 1 : 2
  const compact = scaled
    .toFixed(fractionDigits)
    .replace(/\.0+$/, '')
    .replace(/(\.\d*?)0+$/, '$1')

  return `${compact}${unit.suffix}`
}

export function needsUsageReload(account: {
  status?: string
  usage_percent_5h?: number | null
  usage_percent_7d?: number | null
}): boolean {
  if (account.status !== 'active' && account.status !== 'ready') return false

  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined
  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
  return !has5h && !has7d
}

type AccountStatusSource = {
  status?: string | null
  usage_percent_5h?: number | null
  usage_percent_7d?: number | null
}

export function isUnsampledQuotaAccount(account: AccountStatusSource): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized' || status === 'error') {
    return false
  }
  // Team-type workspaces such as k12 may return only the 5h window: data in either window counts as sampled.
  const has7d =
    typeof account.usage_percent_7d === 'number' &&
    Number.isFinite(account.usage_percent_7d)
  const has5h =
    typeof account.usage_percent_5h === 'number' &&
    Number.isFinite(account.usage_percent_5h)
  return !has7d && !has5h
}

export function getAccountStatusBadgeStatus(account: AccountStatusSource): string {
  const status = account.status || 'unknown'
  if (status === 'overload_paused') return 'active'
  const key = status.toLowerCase()
  if ((key === 'active' || key === 'ready') && isUnsampledQuotaAccount(account)) {
    return 'unsampled'
  }
  return status
}

// Official billing usually produces data the day after an account makes requests. Pulling from upstream before a full day has passed only spins without results.
export const officialCostMinAccountAgeMs = 24 * 60 * 60 * 1000

export function isOfficialCostHiddenAccount(account: {
  status?: string | null
}): boolean {
  const status = (account.status || '').toLowerCase()
  return status === 'unauthorized' || status === 'error'
}

export function isOfficialCostTooNew(
  account: { created_at?: string | null },
  now = Date.now(),
): boolean {
  if (!account.created_at) return false
  const created = Date.parse(account.created_at)
  if (!Number.isFinite(created)) return false
  return now - created < officialCostMinAccountAgeMs
}

export function supportsOfficialUsage(account: {
  access_token_type?: string | null
  chatgpt_account_id?: string | null
  effective_workspace_id?: string | null
}): boolean {
  const isAT = (account.access_token_type || '').trim().toLowerCase() === 'codex_at'
  if (!isAT) return true
  // A codex_at account that has a workspace ID (filled in by whoami or set by a custom header override) supports official usage stats.
  // Matches the backend isCodexATAccount: check the effective workspace first (including the header override), then the one in the credentials.
  const workspaceID = (account.effective_workspace_id || account.chatgpt_account_id || '').trim()
  return workspaceID !== ''
}

export function officialUsdValue(account: {
  official_usd?: number | null
  official_usd_7d?: number | null
}): number | null {
  if (typeof account.official_usd === 'number') return account.official_usd
  if (typeof account.official_usd_7d === 'number') return account.official_usd_7d
  return null
}

// The official billing cost for Codex OAuth accounts comes from a local snapshot. When the list opens, the snapshot is often still empty,
// so page-stats must be re-fetched until the backfill finishes; codex_at has no such path, so do not spin on it.
// official_usage_synced means the backend has synced successfully but the upstream has no data (official stats are
// delayed), so re-fetching will not produce a result; leave it to the background hourly probe.
export function needsOfficialCostReload(account: {
  status?: string | null
  created_at?: string | null
  access_token_type?: string | null
  official_usd?: number | null
  official_usd_7d?: number | null
  official_usage_synced?: boolean
}): boolean {
  if (!supportsOfficialUsage(account)) return false
  if (isOfficialCostHiddenAccount(account) || isOfficialCostTooNew(account)) return false
  if (account.official_usage_synced) return false
  return officialUsdValue(account) === null
}

// The list's "official billing" lines up with the same sync as the official stats page: all the daily snapshots just refreshed are summed.
// When windowDays > 0, the window ends at the upstream's latest day, so a browser time zone cannot be off by one day.
export function officialUsdFromDailyItems(
  items: Array<{ day?: string | null; usd?: number | null }>,
  windowDays = 0,
): number | null {
  if (!Array.isArray(items) || items.length === 0) return null
  let cutoffDay = ''
  if (windowDays > 0) {
    const days = items
      .map((item) => (item.day || '').trim())
      .filter((day) => /^\d{4}-\d{2}-\d{2}$/.test(day))
      .sort()
    const latest = days[days.length - 1]
    if (!latest) return null
    const latestDate = new Date(`${latest}T00:00:00Z`)
    if (Number.isNaN(latestDate.getTime())) return null
    const cutoff = new Date(latestDate)
    cutoff.setUTCDate(cutoff.getUTCDate() - (windowDays - 1))
    cutoffDay = cutoff.toISOString().slice(0, 10)
  }
  let usd = 0
  let any = false
  for (const item of items) {
    const day = (item.day || '').trim()
    if (cutoffDay && day < cutoffDay) continue
    const value = Number(item.usd)
    if (!Number.isFinite(value)) continue
    usd += value
    any = true
  }
  return any ? usd : null
}

const WORKSPACE_CREDIT_HARD_STOPS = new Set([
  'workspace_owner_credits_depleted',
  'workspace_member_credits_depleted',
  'workspace_owner_usage_limit_reached',
  'workspace_member_usage_limit_reached',
])

export function isWorkspaceCreditHardStop(account: {
  credits_spend_control_reached?: boolean | null
  credits_rate_limit_reached_type?: string | null
}): boolean {
  if (account.credits_spend_control_reached === true) {
    return true
  }
  const kind = (account.credits_rate_limit_reached_type ?? '').trim().toLowerCase()
  return WORKSPACE_CREDIT_HARD_STOPS.has(kind)
}

