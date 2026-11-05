import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'
import { AutoRefreshButton } from './AutoRefreshButton'

export interface PageHeaderAutoRefresh {
  // Auto-refresh callback; it should refresh silently (no loading state flash), and the next round is only scheduled after the returned Promise completes.
  onAutoRefresh: () => Promise<unknown>
  // Optional interval in seconds; 0 means off.
  intervals: readonly number[]
  loadSeconds?: () => number
  saveSeconds?: (seconds: number) => void
}

interface PageHeaderProps {
  title: string
  description?: string
  onRefresh?: () => void
  // When provided, the refresh button becomes a split button, and the dropdown lets the user pick an auto-refresh interval.
  autoRefresh?: PageHeaderAutoRefresh
  refreshLabel?: string
  actions?: ReactNode
  actionMeta?: ReactNode
  // titleAdornment is rendered to the right of the title text (e.g. a mode switch dropdown).
  titleAdornment?: ReactNode
  // When hideTitle is set, the title text is visually hidden (kept in the accessibility tree), and titleAdornment fills its place.
  hideTitle?: boolean
  // When actionsBelow is set, the action area is rendered on its own row below the title row,
  // to avoid a wide toolbar competing with titleAdornment (such as the channel switcher) for the same row width, which would cause truncation.
  actionsBelow?: boolean
  className?: string
}

export default function PageHeader({
  title,
  description,
  onRefresh,
  autoRefresh,
  refreshLabel,
  actions,
  actionMeta,
  titleAdornment,
  hideTitle,
  actionsBelow,
  className,
}: PageHeaderProps) {
  const { t } = useTranslation()
  const hasActions = Boolean(onRefresh) || Boolean(actions) || Boolean(actionMeta)
  const resolvedRefreshLabel = refreshLabel ?? t('common.refresh')

  return (
    <div
      data-slot="page-header"
      className={cn(
        'mb-4 flex flex-col gap-3 sm:mb-6',
        !actionsBelow && 'sm:flex-row sm:items-end sm:justify-between sm:gap-5',
        className,
      )}
    >
      <div className={cn('min-w-0', !actionsBelow && 'max-w-[760px]')}>
        <div className="flex flex-wrap items-center gap-2 sm:gap-3">
          <h2
            className={cn(
              'text-[22px] font-semibold leading-tight tracking-tight text-foreground sm:text-[28px]',
              hideTitle && 'sr-only',
            )}
          >
            {title}
          </h2>
          {titleAdornment}
        </div>
        {description ? (
          <p className="mt-1 max-w-[640px] text-[13px] leading-relaxed text-muted-foreground sm:mt-2 sm:text-sm">
            {description}
          </p>
        ) : null}
      </div>
      {hasActions ? (
        <div className="flex min-w-0 flex-col gap-2 sm:items-end">
          {actionMeta ? (
            <div className="text-left text-xs text-muted-foreground sm:text-right">
              {actionMeta}
            </div>
          ) : null}
          <div className="flex min-w-0 flex-wrap items-center gap-1.5 sm:justify-end sm:gap-2">
            {actions}
            {onRefresh && autoRefresh ? (
              <AutoRefreshButton
                onRefresh={onRefresh}
                onAutoRefresh={autoRefresh.onAutoRefresh}
                intervals={autoRefresh.intervals}
                loadSeconds={autoRefresh.loadSeconds}
                saveSeconds={autoRefresh.saveSeconds}
                label={resolvedRefreshLabel}
              />
            ) : onRefresh ? (
              <Button
                variant="outline"
                size="sm"
                onClick={onRefresh}
                className="shrink-0"
              >
                <RefreshCw className="size-3.5" />
                <span className="max-[380px]:hidden">{resolvedRefreshLabel}</span>
              </Button>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  )
}
