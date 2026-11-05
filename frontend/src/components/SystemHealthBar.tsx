import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { getBucketConfig, type TimeRangeKey } from '../lib/timeRange'
import type { ChartAggregation } from '../types'

// SystemHealthBar is the system-level "health status" bar on the dashboard. It takes the server-aggregated
// timeline (request count / 4xx / 5xx per bucket) for the selected time span and renders it as a row of success-rate color blocks plus the overall success rate.
// It reuses the chartData that the Dashboard has already loaded, so no extra request is needed; the time span follows the selector at the top.

interface Block {
  success: number
  failure: number
  rate: number // -1 means no requests in this period (idle)
  startTime: number
  endTime: number
}

const COLOR_STOPS = [
  { r: 239, g: 68, b: 68 }, // #ef4444 red
  { r: 250, g: 204, b: 21 }, // #facc15 gold
  { r: 34, g: 197, b: 94 }, // #22c55e green
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

function formatDate(timestamp: number): string {
  const date = new Date(timestamp)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

function formatClock(timestamp: number, withDate: boolean): string {
  const date = new Date(timestamp)
  const pad = (n: number) => String(n).padStart(2, '0')
  const hm = `${pad(date.getHours())}:${pad(date.getMinutes())}`
  return withDate ? `${formatDate(timestamp)} ${hm}` : hm
}

function formatSuccessRate(rate: number): string {
  const rounded = rate.toFixed(1)
  return `${rounded.endsWith('.0') ? rounded.slice(0, -2) : rounded}%`
}

interface Props {
  chartData: ChartAggregation | null
  timeRange: TimeRangeKey
  loading?: boolean
}

export default function SystemHealthBar({ chartData, timeRange, loading = false }: Props) {
  const { t } = useTranslation()
  const [activeTooltip, setActiveTooltip] = useState<number | null>(null)
  const blocksRef = useRef<HTMLDivElement>(null)

  // The backend chart aggregation groups by bucket and only returns buckets that have requests; empty buckets are not in the result. Here, based on the selected
  // span, we rebuild a fixed-size grid of cells (e.g. 7d = 28 cells) and place each timeline point into the matching cell by time,
  // cells with no data show as gray idle; otherwise 7 days would only draw a few cells and the empty gaps would not be visible.
  const { bucketMinutes, bucketCount } = getBucketConfig(timeRange)
  const bucketDurationMs = bucketMinutes * 60 * 1000
  const isDayBucket = bucketMinutes >= 1440
  const withDate = timeRange === '24h' || timeRange === '7d' || timeRange === '30d'

  // Align the grid to local clock boundaries (day buckets start at 00:00 of that day, hour buckets at the top of the hour), instead of anchoring to
  // the arbitrary current moment, otherwise long-span bucket starts would show odd values such as 03:16.
  const tzOffsetMs = new Date().getTimezoneOffset() * 60 * 1000
  const alignToBucket = (ms: number) =>
    Math.floor((ms - tzOffsetMs) / bucketDurationMs) * bucketDurationMs + tzOffsetMs
  const now = Date.now()
  const lastBucketStart = alignToBucket(now)
  const windowStart = lastBucketStart - (bucketCount - 1) * bucketDurationMs

  const blocks: Block[] = Array.from({ length: bucketCount }, (_, idx) => {
    const start = windowStart + idx * bucketDurationMs
    return { success: 0, failure: 0, rate: -1, startTime: start, endTime: start + bucketDurationMs }
  })

  for (const point of chartData?.timeline ?? []) {
    const ts = new Date(point.bucket).getTime()
    if (Number.isNaN(ts)) continue
    let idx = Math.floor((alignToBucket(ts) - windowStart) / bucketDurationMs)
    if (idx < 0) idx = 0
    if (idx >= bucketCount) idx = bucketCount - 1
    const failure = point.errors_4xx + point.errors_5xx
    const success = Math.max(0, point.requests - failure)
    blocks[idx].success += success
    blocks[idx].failure += failure
  }

  for (const block of blocks) {
    const total = block.success + block.failure
    block.rate = total > 0 ? block.success / total : -1
  }

  const totalSuccess = blocks.reduce((sum, b) => sum + b.success, 0)
  const totalFailure = blocks.reduce((sum, b) => sum + b.failure, 0)
  const grandTotal = totalSuccess + totalFailure
  const hasData = grandTotal > 0
  const successRate = hasData ? (totalSuccess / grandTotal) * 100 : 100

  const rateClass = !hasData
    ? 'bg-muted text-muted-foreground'
    : successRate >= 90
      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950 dark:text-emerald-400'
      : successRate >= 50
        ? 'bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-400'
        : 'bg-red-100 text-red-600 dark:bg-red-950 dark:text-red-400'

  useEffect(() => {
    if (activeTooltip === null) return
    const handler = (e: PointerEvent) => {
      if (blocksRef.current && !blocksRef.current.contains(e.target as Node)) {
        setActiveTooltip(null)
      }
    }
    document.addEventListener('pointerdown', handler)
    return () => document.removeEventListener('pointerdown', handler)
  }, [activeTooltip])

  const handlePointerEnter = useCallback((e: React.PointerEvent, idx: number) => {
    if (e.pointerType === 'mouse') setActiveTooltip(idx)
  }, [])
  const handlePointerLeave = useCallback((e: React.PointerEvent) => {
    if (e.pointerType === 'mouse') setActiveTooltip(null)
  }, [])
  const handlePointerDown = useCallback((e: React.PointerEvent, idx: number) => {
    if (e.pointerType === 'touch') {
      e.preventDefault()
      setActiveTooltip((prev) => (prev === idx ? null : idx))
    }
  }, [])

  const total = blocks.length

  const renderTooltip = (block: Block, idx: number) => {
    const count = block.success + block.failure
    const posClass =
      idx <= 2
        ? 'left-0 translate-x-0'
        : idx >= total - 3
          ? 'right-0 left-auto translate-x-0'
          : 'left-1/2 -translate-x-1/2'
    // Day granularity (30d) shows only the date; otherwise show the time range as "[date] start – end".
    const timeRangeLabel = isDayBucket
      ? formatDate(block.startTime)
      : `${formatClock(block.startTime, withDate)} – ${formatClock(block.endTime, false)}`
    return (
      <div
        className={`pointer-events-none absolute bottom-[calc(100%+8px)] z-30 whitespace-nowrap rounded-md border bg-popover px-2.5 py-1.5 text-[11px] leading-snug text-popover-foreground shadow-md ${posClass}`}
      >
        <span className="mb-0.5 block text-muted-foreground">{timeRangeLabel}</span>
        {count > 0 ? (
          <span className="flex items-center gap-2">
            <span className="text-emerald-600">
              {t('dashboard.healthBarSuccessShort')} {block.success}
            </span>
            <span className="text-red-500">
              {t('dashboard.healthBarFailureShort')} {block.failure}
            </span>
            <span className="text-muted-foreground">({(block.rate * 100).toFixed(1)}%)</span>
          </span>
        ) : (
          <span className="text-muted-foreground">{t('dashboard.healthBarNoRequests')}</span>
        )}
      </div>
    )
  }

  return (
    <Card className="py-0 border-border/70 bg-card shadow-2xs">
      <CardContent className="p-4 sm:p-5">
        <div className="mb-3.5 flex items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <Activity className="size-4 text-primary" />
            <h3 className="text-sm font-semibold text-foreground">
              {t('dashboard.systemHealthTitle')}
            </h3>
          </div>
          <span
            className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-bold tabular-nums shadow-2xs ${rateClass}`}
          >
            {hasData ? formatSuccessRate(successRate) : '--'}
          </span>
        </div>

        {loading && !hasData ? (
          <div className="flex gap-1">
            {Array.from({ length: bucketCount }).map((_, i) => (
              <div
                key={i}
                className="h-3 flex-1 animate-pulse rounded-full bg-muted/60"
                style={{ animationDelay: `${i * 40}ms` }}
              />
            ))}
          </div>
        ) : (
          <div className="relative flex gap-1 items-center" ref={blocksRef}>
            {blocks.map((block, idx) => {
              const isIdle = block.rate === -1
              const isActive = activeTooltip === idx
              return (
                <div
                  key={idx}
                  className="group relative min-w-[3px] flex-1 cursor-pointer py-1"
                  onPointerEnter={(e) => handlePointerEnter(e, idx)}
                  onPointerLeave={handlePointerLeave}
                  onPointerDown={(e) => handlePointerDown(e, idx)}
                >
                  <div
                    className={`h-3 w-full rounded-full transition-all duration-150 group-hover:scale-y-125 ${
                      isIdle ? 'bg-muted/70' : ''
                    } ${isActive ? 'scale-y-125 opacity-90 ring-2 ring-primary/40' : ''}`}
                    style={isIdle ? undefined : { backgroundColor: rateToColor(block.rate) }}
                  />
                  {isActive && renderTooltip(block, idx)}
                </div>
              )
            })}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
