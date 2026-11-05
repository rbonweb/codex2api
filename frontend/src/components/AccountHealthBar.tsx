import AccountStatusBar from './AccountStatusBar'
import { useTranslation } from 'react-i18next'
import type { AccountHealthBucket } from '../types'

// "Health status" bar: renders the account's recent request success and failure buckets as a row of color blocks, plus the success rate.
// Ported from ProviderStatusBar in CLIProxyAPI, reimplemented with Tailwind.
// Data comes from the backend /api/accounts/health-bars (20 cells × 10 minutes ≈ the last 3.3 hours).

interface StatusBlockDetail {
  success: number
  failure: number
  rate: number // -1 means there were no requests in this period (idle)
  startTime: number
  endTime: number
}

interface StatusBarData {
  blockDetails: StatusBlockDetail[]
  successRate: number
  totalSuccess: number
  totalFailure: number
}

// RGB linear interpolation from 0 → red (#ef4444) → 0.5 → gold (#facc15) → 1 → green (#22c55e).
const COLOR_STOPS = [
  { r: 239, g: 68, b: 68 },
  { r: 250, g: 204, b: 21 },
  { r: 34, g: 197, b: 94 },
] as const

function rateToColor(rate: number): string {
  const t = Math.max(0, Math.min(1, rate))
  const segment = t < 0.5 ? 0 : 1
  const localT = segment === 0 ? t * 2 : (t - 0.5) * 2
  const from = COLOR_STOPS[segment]
  const to = COLOR_STOPS[segment + 1]
  const r = Math.round(from.r + (to.r - from.r) * localT)
  const g = Math.round(from.g + (to.g - from.g) * localT)
  const b = Math.round(from.b + (to.b - from.b) * localT)
  return `rgb(${r}, ${g}, ${b})`
}

function formatClock(timestamp: number): string {
  const date = new Date(timestamp)
  const h = date.getHours().toString().padStart(2, '0')
  const m = date.getMinutes().toString().padStart(2, '0')
  return `${h}:${m}`
}

// statusBarDataFromBuckets converts the backend's success and failure buckets (oldest to newest) into the structure needed for rendering.
// When there are too few buckets, empty buckets are padded at the head (aligned with endTime=now).
function statusBarDataFromBuckets(
  buckets: AccountHealthBucket[],
  blockCount: number,
  blockMinutes: number,
): StatusBarData {
  const blockDurationMs = blockMinutes * 60 * 1000
  const padCount = Math.max(0, blockCount - buckets.length)
  const stats = [
    ...Array.from({ length: padCount }, () => ({ success: 0, failed: 0 })),
    ...buckets.slice(-blockCount),
  ]

  const now = Date.now()
  const windowStart = now - blockCount * blockDurationMs

  const blockDetails: StatusBlockDetail[] = []
  let totalSuccess = 0
  let totalFailure = 0

  stats.forEach((bucket, index) => {
    const success = bucket.success
    const failure = bucket.failed
    const total = success + failure
    totalSuccess += success
    totalFailure += failure

    const start = windowStart + index * blockDurationMs
    blockDetails.push({
      success,
      failure,
      rate: total > 0 ? success / total : -1,
      startTime: start,
      endTime: start + blockDurationMs,
    })
  })

  const total = totalSuccess + totalFailure
  return {
    blockDetails,
    successRate: total > 0 ? (totalSuccess / total) * 100 : 100,
    totalSuccess,
    totalFailure,
  }
}

interface Props {
  buckets: AccountHealthBucket[] | undefined
  blockCount?: number
  blockMinutes?: number
}

export default function AccountHealthBar({
  buckets,
  blockCount = 20,
  blockMinutes = 10,
}: Props) {
  const { t } = useTranslation()
  const data = statusBarDataFromBuckets(buckets ?? [], blockCount, blockMinutes)
  const hasData = data.totalSuccess + data.totalFailure > 0
  const rateClass = !hasData
    ? 'bg-muted text-muted-foreground'
    : data.successRate >= 90
      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
      : data.successRate >= 50
        ? 'bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-400'
        : 'bg-red-100 text-red-600 dark:bg-red-950 dark:text-red-400'

  const renderTooltip = (detail: StatusBlockDetail) => {
    const count = detail.success + detail.failure
    const timeRange = `${formatClock(detail.startTime)} – ${formatClock(detail.endTime)}`

    return (
      <div
        className="contents"
      >
        <span className="mb-0.5 block text-muted-foreground">{timeRange}</span>
        {count > 0 ? (
          <span className="flex items-center gap-2">
            <span className="text-emerald-600">
              {t('accounts.healthBarSuccessShort')} {detail.success}
            </span>
            <span className="text-red-500">
              {t('accounts.healthBarFailureShort')} {detail.failure}
            </span>
            <span className="text-muted-foreground">({(detail.rate * 100).toFixed(1)}%)</span>
          </span>
        ) : (
          <span className="text-muted-foreground">{t('accounts.healthBarNoRequests')}</span>
        )}
      </div>
    )
  }

  return <AccountStatusBar
    blocks={data.blockDetails.map(detail => ({
      color: detail.rate === -1 ? undefined : rateToColor(detail.rate),
      tooltip: renderTooltip(detail),
    }))}
    rate={hasData ? data.successRate : null}
    rateClass={rateClass}
  />
}
