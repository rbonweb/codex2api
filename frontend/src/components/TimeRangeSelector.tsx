import { useTranslation } from 'react-i18next'
import { TIME_RANGE_OPTIONS, type TimeRangeKey } from '../lib/timeRange'

interface Props {
  timeRange: TimeRangeKey
  onTimeRangeChange: (range: TimeRangeKey) => void
  // When showLiveBadge is true and the current range is 1h (live), a pulsing "Live" badge is shown in front.
  showLiveBadge?: boolean
}

// TimeRangeSelector is the time span selector shared by the dashboard (1h/6h/24h/7d/30d).
// Extracted from the inline control in the "Usage trend" section and moved to the top right of the dashboard, so the whole page can share it.
export default function TimeRangeSelector({
  timeRange,
  onTimeRangeChange,
  showLiveBadge = true,
}: Props) {
  const { t } = useTranslation()
  const isLive = timeRange === '1h'

  return (
    <div className="flex max-w-full items-center gap-1.5 overflow-x-auto [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      {showLiveBadge && isLive && (
        <div className="mr-0.5 inline-flex shrink-0 items-center gap-1.5 rounded-full border border-emerald-500/20 bg-emerald-500/10 px-2.5 py-1 text-[11px] font-medium text-emerald-600 dark:text-emerald-300 sm:px-3 sm:text-xs">
          <span className="size-1.5 rounded-full bg-current animate-pulse" />
          <span>{t('dashboard.liveBadge')}</span>
        </div>
      )}
      <div className="inline-flex shrink-0 rounded-lg border border-border bg-muted/50 p-0.5">
        {TIME_RANGE_OPTIONS.map((key) => (
          <button
            key={key}
            type="button"
            onClick={() => onTimeRangeChange(key)}
            className={`rounded-md px-2 py-1 text-[11px] font-medium transition-all duration-200 sm:px-3 sm:py-1.5 sm:text-xs ${
              timeRange === key
                ? 'border border-border bg-background text-foreground shadow-sm'
                : 'text-muted-foreground hover:text-foreground'
            }`}
          >
            {t(`dashboard.timeRange${key.toUpperCase()}`)}
          </button>
        ))}
      </div>
    </div>
  )
}
