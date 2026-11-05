import type { ReactNode } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { PieChart, Pie, Cell, ResponsiveContainer, Tooltip } from 'recharts'
import {
  Activity,
  AlertTriangle,
  Banknote,
  BarChart3,
  Clock3,
  Coins,
  Gauge,
  KeyRound,
  Link2,
  Package,
  Receipt,
  RefreshCw,
  RotateCcw,
  Search,
  Zap,
} from 'lucide-react'
import Modal from './Modal'
import { api } from '../api'
import type { AccountKeyStat, AccountModelStat, AccountRow, AccountUsageDayStat, AccountUsageDetail, ResetCreditItem, WhamDailyUsageBreakdownEntry, WhamDailyUsageCycle, WhamDailyUsageItem, WhamDailyUsageResponse, WhamDailyUsageSplit } from '../types'
import { formatUsageNumber, officialUsdFromDailyItems, supportsOfficialUsage, isWorkspaceCreditHardStop } from '../lib/usageFormat'
import { useShowFullUsageNumbers } from '../hooks/useShowFullUsageNumbers'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'

const COLORS = [
  '#0f766e',
  '#2563eb',
  '#d97706',
  '#7c3aed',
  '#dc2626',
  '#059669',
  '#0891b2',
  '#ea580c',
  '#4f46e5',
  '#db2777',
]

// Data source of the official stats, shown next to the range selector (matches the backend proxy.WhamDailyUsageURL).
const WHAM_DAILY_USAGE_ENDPOINT =
  'https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts'

type UsagePage = 'overview' | 'detail' | 'quality' | 'official'
type UsageRangeKey = '7' | '30' | '90' | 'all'
type ModelMetricKey = 'requests' | 'tokens' | 'cost'
type QualityTone = 'neutral' | 'success' | 'warning' | 'danger'

const USAGE_RANGE_OPTIONS: Array<{ key: UsageRangeKey; days: number; labelKey: string }> = [
  { key: '7', days: 7, labelKey: 'accounts.usageRange7d' },
  { key: '30', days: 30, labelKey: 'accounts.usageRange30d' },
  { key: '90', days: 90, labelKey: 'accounts.usageRange90d' },
  { key: 'all', days: 0, labelKey: 'accounts.usageRangeAll' },
]

const MODEL_METRIC_OPTIONS: Array<{ key: ModelMetricKey; labelKey: string }> = [
  { key: 'requests', labelKey: 'accounts.usageModelMetricRequests' },
  { key: 'tokens', labelKey: 'accounts.usageModelMetricTokens' },
  { key: 'cost', labelKey: 'accounts.usageModelMetricCost' },
]

export interface OfficialUsageRefreshPatch {
  accountId: number
  officialUsd: number | null
}

interface Props {
  account: AccountRow
  onClose: () => void
  onCreditsReset?: () => void
  // Codex-only credit voucher / credit settings block; pass false for non-Codex accounts such as Grok to hide it.
  showCreditSettings?: boolean
  // Open straight on the given tab (clicking the "official billing" cost in the list should land on the official stats,
  // instead of making the user open the overview and switch again).
  initialPage?: UsagePage
  // Callback after the official stats sync succeeds: the list page immediately updates the badge from this 7d quota,
  // and refetches page-stats to align the snapshot, without waiting for the next page turn.
  onOfficialUsageRefreshed?: (patch: OfficialUsageRefreshPatch) => void
  // Force switch for the official stats tab: pass false for channels without a ChatGPT official billing path, such as Claude, to hide it;
  // when omitted, it is decided automatically by supportsOfficialUsage(account).
  officialUsage?: boolean
}

export default function AccountUsageModal({ account, onClose, onCreditsReset, showCreditSettings = true, initialPage, onOfficialUsageRefreshed, officialUsage }: Props) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const [data, setData] = useState<AccountUsageDetail | null>(null)
  const [dataRange, setDataRange] = useState<UsageRangeKey | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [page, setPage] = useState<UsagePage>(initialPage ?? 'overview')
  const [range, setRange] = useState<UsageRangeKey>('30')
  const requestSeq = useRef(0)

  // Official billing stats can only be queried for ChatGPT OAuth accounts (the wham endpoint belongs to the ChatGPT backend).
  // codex_at, Responses API relay and Grok have no such path, so this tab is not shown.
  const showOfficialUsage = officialUsage ?? supportsOfficialUsage(account)

  const [creditEnabled, setCreditEnabled] = useState(account.credit_enabled ?? false)
  const [creditSkipWindow, setCreditSkipWindow] = useState(account.credit_skip_usage_window ?? false)
  const [savingCredit, setSavingCredit] = useState(false)
  const [creditError, setCreditError] = useState<string | null>(null)

  const load = useCallback(async () => {
    const seq = requestSeq.current + 1
    requestSeq.current = seq
    setLoading(true)
    setError(null)
    try {
      const result = await api.getAccountUsage(account.id, usageRangeToDays(range))
      if (requestSeq.current !== seq) return
      setData(result)
      setDataRange(range)
    } catch (err) {
      if (requestSeq.current !== seq) return
      setError(getErrorMessage(err))
    } finally {
      if (requestSeq.current === seq) {
        setLoading(false)
      }
    }
  }, [account.id, range])

  useEffect(() => { void load() }, [load])

  const accountLabel = account.email || account.name || `#${account.id}`
  const title = t('accounts.usageDetailTitle') + ' — ' + accountLabel

  const handleViewLogs = () => {
    const params = new URLSearchParams({ account_id: String(account.id) })
    if (range === '7' || range === '30') {
      params.set('range', `${range}d`)
    } else if (range === '90' || range === 'all') {
      params.set('range', 'custom')
      params.set('days', '90')
    }
    onClose()
    navigate(`/usage?${params.toString()}`)
  }

  // One switch writes both columns: the backend gate is CreditEnabled && CreditSkipUsageWindow, left unchanged,
  // the UI just no longer exposes the intermediate switch that has no behavior of its own.
  const handleCreditToggle = async (value: boolean) => {
    setCreditError(null)
    setSavingCredit(true)
    // Optimistic update: the switch slides over immediately without waiting for the round trip (the backend step may also release the cooldown, which can take a while).
    // On failure, roll back to the original value and show the error.
    const prevEnabled = creditEnabled
    const prevSkipWindow = creditSkipWindow
    setCreditEnabled(value)
    setCreditSkipWindow(value)
    try {
      await api.updateAccountCredit(account.id, {
        credit_enabled: value,
        credit_skip_usage_window: value,
      })
      // The backend may have released the usage window cooldown when the credit gate opened, so let the outer layer refresh the status and badge.
      onCreditsReset?.()
    } catch (err) {
      setCreditEnabled(prevEnabled)
      setCreditSkipWindow(prevSkipWindow)
      setCreditError(getErrorMessage(err))
    } finally {
      setSavingCredit(false)
    }
  }

  // When entering from the list via "official billing", the official stats do not depend on the local usage_logs.
  // Show the official page first and hit upstream immediately, so the badge aligns with this sync time,
  // instead of getting stuck behind the gateway usage endpoint.
  const officialReady = page === 'official' && showOfficialUsage

  return (
    <Modal
      show
      title={title}
      onClose={onClose}
      contentClassName="sm:max-w-[960px]"
      bodyClassName="px-5 py-5 sm:px-6"
    >
      {loading && !data && !officialReady ? (
        <div className="flex items-center justify-center py-12 text-sm text-muted-foreground">
          {t('common.loading')}
        </div>
      ) : error && !data && !officialReady ? (
        <div className="py-8 text-center text-sm text-red-500">{error}</div>
      ) : !data && !officialReady ? (
        <div className="py-12 text-center text-sm text-muted-foreground">
          {t('accounts.noUsageData')}
        </div>
      ) : (
        <UsageStatsContent
          account={account}
          accountLabel={accountLabel}
          data={data ?? emptyUsageDetail()}
          // Relay accounts have no official stats tab; on a deep link, fall back to the overview instead of stopping on a blank page.
          page={page === 'official' && !showOfficialUsage ? 'overview' : page}
          range={range}
          dataRange={dataRange || range}
          refreshing={loading}
          refreshError={error}
          onPageChange={setPage}
          onRangeChange={setRange}
          onViewLogs={handleViewLogs}
          showOfficialUsage={showOfficialUsage}
          onOfficialUsageRefreshed={onOfficialUsageRefreshed}
        />
      )}

      {showCreditSettings && (
        <>
          <CreditSettings
            account={account}
            creditEnabled={creditEnabled}
            creditSkipWindow={creditSkipWindow}
            savingCredit={savingCredit}
            creditError={creditError}
            onToggle={handleCreditToggle}
          />

          <ResetCreditsSection account={account} onResetDone={onCreditsReset} />
        </>
      )}
    </Modal>
  )
}

function UsageStatsContent({
  account,
  accountLabel,
  data,
  page,
  range,
  dataRange,
  refreshing,
  refreshError,
  onPageChange,
  onRangeChange,
  onViewLogs,
  showOfficialUsage,
  onOfficialUsageRefreshed,
}: {
  account: AccountRow
  accountLabel: string
  data: AccountUsageDetail
  page: UsagePage
  range: UsageRangeKey
  dataRange: UsageRangeKey
  refreshing: boolean
  refreshError: string | null
  onPageChange: (page: UsagePage) => void
  onRangeChange: (range: UsageRangeKey) => void
  onViewLogs: () => void
  showOfficialUsage: boolean
  onOfficialUsageRefreshed?: (patch: OfficialUsageRefreshPatch) => void
  // Force switch for the official stats tab: pass false for channels without a ChatGPT official billing path, such as Claude, to hide it;
  // when omitted, it is decided automatically by supportsOfficialUsage(account).
  officialUsage?: boolean
}) {
  const { t } = useTranslation()
  const activeDays = Math.max(0, data.active_days || 0)
  const periodDays = data.period_days ?? usageRangeToDays(dataRange)
  const displayDays = Math.max(1, periodDays || usageRangeToDays(dataRange))
  const rangeDescription = dataRange === 'all'
    ? t('accounts.usageAllTimeStats')
    : t('accounts.usageLastDays', { days: displayDays })
  const totalCostLabel = dataRange === 'all'
    ? t('accounts.usageTotalCostAll')
    : t('accounts.usageTotalCostRange', { days: displayDays })
  const today = data.today || emptyDayStat()
  const highestCostDay = data.highest_cost_day || emptyDayStat()
  const highestRequestDay = data.highest_request_day || emptyDayStat()
  const topModel = data.models[0]

  return (
    <div className="space-y-5">
      <div className="rounded-2xl border bg-card shadow-sm">
        <div className="flex flex-col gap-4 border-b px-4 py-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex min-w-0 items-center gap-3">
            <div className="flex size-11 shrink-0 items-center justify-center rounded-xl bg-foreground text-background">
              <BarChart3 className="size-5" />
            </div>
            <div className="min-w-0">
              <div className="truncate text-base font-semibold text-foreground">
                {accountLabel}
              </div>
              <div className="text-sm text-muted-foreground">
                {rangeDescription}
                {refreshing && (
                  <span className="ml-2 text-xs">{t('common.loading')}</span>
                )}
              </div>
              {refreshError && (
                <div className="mt-1 text-xs text-red-500">{refreshError}</div>
              )}
            </div>
          </div>

          <div className="flex flex-wrap items-center justify-end gap-2">
            <span className="inline-flex h-8 items-center rounded-full border bg-muted/40 px-3 text-xs font-semibold text-muted-foreground">
              {account.status || t('accounts.unknown')}
            </span>
            <button
              type="button"
              onClick={onViewLogs}
              className="inline-flex h-8 items-center gap-1.5 rounded-lg border bg-background px-3 text-xs font-semibold text-muted-foreground transition-colors hover:text-foreground"
            >
              <Search className="size-3.5" />
              {t('accounts.usageViewLogs')}
            </button>
            <div className={`grid h-9 ${showOfficialUsage ? 'grid-cols-4' : 'grid-cols-3'} rounded-lg border bg-muted/40 p-1`}>
              <PageButton
                active={page === 'overview'}
                icon={<Gauge className="size-3.5" />}
                label={t('accounts.usageOverviewTab')}
                onClick={() => onPageChange('overview')}
              />
              <PageButton
                active={page === 'detail'}
                icon={<Package className="size-3.5" />}
                label={t('accounts.usageDetailTab')}
                onClick={() => onPageChange('detail')}
              />
              <PageButton
                active={page === 'quality'}
                icon={<Activity className="size-3.5" />}
                label={t('accounts.usageQualityTab')}
                onClick={() => onPageChange('quality')}
              />
              {showOfficialUsage && (
                <PageButton
                  active={page === 'official'}
                  icon={<Receipt className="size-3.5" />}
                  label={t('accounts.usageOfficialTab')}
                  onClick={() => onPageChange('official')}
                />
              )}
            </div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <span className="text-xs font-semibold uppercase text-muted-foreground">
            {t('accounts.usageRange')}
          </span>
          <div className="flex rounded-lg border bg-muted/40 p-1">
            {USAGE_RANGE_OPTIONS.map((option) => (
              <RangeButton
                key={option.key}
                active={range === option.key}
                label={t(option.labelKey)}
                onClick={() => onRangeChange(option.key)}
              />
            ))}
          </div>
          {/* The official stats and the other tabs have different data sources (upstream billing vs local usage_logs),
              so label the source endpoint; otherwise there is no way to investigate when the two figures disagree. */}
          {page === 'official' && (
            <span
              className="ml-auto inline-flex min-w-0 max-w-full items-center gap-1.5 rounded-lg border border-primary/20 bg-primary/5 px-2.5 py-1 text-[11px] text-muted-foreground"
              title={WHAM_DAILY_USAGE_ENDPOINT}
            >
              <Link2 className="size-3.5 shrink-0 text-primary" aria-hidden />
              <span className="shrink-0">{t('accounts.usageOfficialSource')}</span>
              <span className="min-w-0 truncate font-mono text-foreground">
                {WHAM_DAILY_USAGE_ENDPOINT}
              </span>
            </span>
          )}
        </div>

        {page === 'overview' ? (
          <OverviewPage
            data={data}
            today={today}
            highestCostDay={highestCostDay}
            highestRequestDay={highestRequestDay}
            activeDays={activeDays}
            periodDays={periodDays}
            totalCostLabel={totalCostLabel}
            topModel={topModel}
          />
        ) : page === 'detail' ? (
          <DetailPage
            data={data}
            activeDays={activeDays}
            periodDays={periodDays}
          />
        ) : page === 'official' ? (
          <OfficialUsagePage
            accountId={account.id}
            range={range}
            // Hit upstream once on opening the official stats: the "official billing" badge in the list aligns with this sync time.
            autoRefresh
            onRefreshed={onOfficialUsageRefreshed}
          />
        ) : (
          <QualityPage data={data} />
        )}
      </div>
    </div>
  )
}

function OverviewPage({
  data,
  today,
  highestCostDay,
  highestRequestDay,
  activeDays,
  periodDays,
  totalCostLabel,
  topModel,
}: {
  data: AccountUsageDetail
  today: AccountUsageDayStat
  highestCostDay: AccountUsageDayStat
  highestRequestDay: AccountUsageDayStat
  activeDays: number
  periodDays: number
  totalCostLabel: string
  topModel?: { model: string; requests: number; tokens: number }
}) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const activeDaysText = formatActiveDaysText(activeDays, periodDays, t('accounts.usageDaysUnit'))
  return (
    <div className="p-4 sm:p-5">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1.2fr)_minmax(300px,0.8fr)]">
        <section className="rounded-2xl border bg-gradient-to-br from-background via-background to-muted/50 p-5">
          <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <div className="text-sm font-medium text-muted-foreground">
                {totalCostLabel}
              </div>
              <div className="mt-2 text-5xl font-semibold tracking-normal text-foreground">
                ${formatCost(data.total_account_billed)}
              </div>
            </div>
            <div className="grid grid-cols-2 gap-2 text-sm">
              <MiniPill label={t('accounts.accountBilledLabel')} value={`$${formatCost(data.total_account_billed)}`} />
              <MiniPill label={t('accounts.userBilledLabel')} value={`$${formatCost(data.total_user_billed)}`} />
            </div>
          </div>

          <div className="mt-6 grid gap-3 sm:grid-cols-3">
            <CompactMetric icon={<Zap className="size-4" />} label={t('accounts.totalRequests')} value={formatCompactNumber(data.total_requests, fullNumbers)} />
            <CompactMetric icon={<Package className="size-4" />} label={t('accounts.totalTokens')} value={formatTokens(data.total_tokens, fullNumbers)} />
            <CompactMetric icon={<Clock3 className="size-4" />} label={t('accounts.usageAvgResponse')} value={formatDuration(data.avg_duration_ms)} />
          </div>

          <UsageTrend history={data.history || []} />
        </section>

        <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-1">
          <SignalCard
            icon={<Activity className="size-4" />}
            title={t('accounts.usageTodayOverview')}
            rows={[
              [t('accounts.usageRequests'), formatNumber(today.requests)],
              [t('accounts.usageTokens'), formatTokens(today.tokens, fullNumbers)],
              [t('accounts.usageTodayCost'), `$${formatCost(today.account_billed)}`],
            ]}
          />
          <SignalCard
            icon={<Gauge className="size-4" />}
            title={t('accounts.usageDailyBaseline')}
            rows={[
              [t('accounts.usageAvgDailyCost'), `$${formatCost(data.avg_daily_account_billed)}`],
              [t('accounts.usageAvgDailyRequests'), formatCompactNumber(Math.round(data.avg_daily_requests), fullNumbers)],
              [t('accounts.usageActiveDays'), activeDaysText],
            ]}
          />
        </section>
      </div>
      <div className="mt-4 grid gap-3 md:grid-cols-3">
        <HighlightStrip
          label={t('accounts.usageHighestCostDay')}
          value={highestCostDay.label || '-'}
          detail={`$${formatCost(highestCostDay.account_billed)} · ${formatNumber(highestCostDay.requests)} ${t('accounts.usageReqUnit')}`}
        />
        <HighlightStrip
          label={t('accounts.usageHighestRequestDay')}
          value={highestRequestDay.label || '-'}
          detail={`${formatCompactNumber(highestRequestDay.requests, fullNumbers)} ${t('accounts.usageReqUnit')} · $${formatCost(highestRequestDay.account_billed)}`}
        />
        <HighlightStrip
          label={t('accounts.usageTopModel')}
          value={topModel?.model || '-'}
          detail={topModel ? `${formatNumber(topModel.requests)} ${t('accounts.usageReqUnit')} · ${formatTokens(topModel.tokens, fullNumbers)} ${t('accounts.usageTokUnit')}` : '-'}
        />
      </div>
    </div>
  )
}

function QualityPage({ data }: { data: AccountUsageDetail }) {
  return (
    <div className="p-4 sm:p-5">
      <QualitySignals data={data} />
    </div>
  )
}

// OfficialUsagePage shows usage on OpenAI's billing basis, which is a separate data set from the local usage_logs
// aggregation of the other tabs: the credits and tokens here are official billing figures and can be split by client entry,
// showing how much of an account's consumption came from this gateway and how much from the official client; per-model cost comes from
// the share allocation of the model x speed breakdown endpoint (including the fast/priority tier).
function OfficialUsagePage({
  accountId,
  range,
  autoRefresh = false,
  onRefreshed,
}: {
  accountId: number
  range: UsageRangeKey
  // On entering this view, hit upstream first, then let the list badge catch up with this sync; changing the date range only reads the local snapshot.
  autoRefresh?: boolean
  onRefreshed?: (patch: OfficialUsageRefreshPatch) => void
}) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const [data, setData] = useState<WhamDailyUsageResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const requestSeq = useRef(0)
  const autoRefreshedAccountRef = useRef<number | null>(null)
  // 'all' makes no sense here (the local snapshot keeps at most one year), so take the maximum number of days.
  const days = range === 'all' ? 365 : usageRangeToDays(range)

  const load = useCallback(async (refresh: boolean) => {
    const seq = requestSeq.current + 1
    requestSeq.current = seq
    if (refresh) setRefreshing(true)
    else setLoading(true)
    setError(null)
    try {
      const result = await api.getWhamDailyUsage(accountId, days, refresh)
      if (requestSeq.current !== seq) return
      setData(result)
      // Notify the list only when the refresh really succeeded (with refresh_error the snapshot did not change, so refetching is pointless).
      if (refresh && !result.refresh_error) {
        onRefreshed?.({
          accountId,
          officialUsd: officialUsdFromDailyItems(result.items ?? []),
        })
      }
    } catch (err) {
      if (requestSeq.current !== seq) return
      setError(getErrorMessage(err))
    } finally {
      if (requestSeq.current === seq) {
        setLoading(false)
        setRefreshing(false)
      }
    }
  }, [accountId, days, onRefreshed])

  useEffect(() => {
    const shouldRefresh = autoRefresh && autoRefreshedAccountRef.current !== accountId
    if (shouldRefresh) autoRefreshedAccountRef.current = accountId
    void load(shouldRefresh)
  }, [load, accountId, autoRefresh])

  const items = data?.items ?? []
  const maxCredits = useMemo(
    () => items.reduce((max, item) => Math.max(max, item.credits), 0),
    [items],
  )
  // The conversion rate is sent by the backend and not hardcoded in the frontend, so only one place changes when the official ratio changes.
  const creditsPerUSD = data?.credits_per_usd || 25
  // The client breakdown is accumulated over the whole window: a single day is too noisy to show the entry share.
  const clientTotals = useMemo(
    () => aggregateSplits(items, 'clients', creditsPerUSD),
    [items, creditsPerUSD],
  )
  // Per-model cost comes from the share allocation of daily-token-usage-breakdown (counts gives no credits
  // at the model level). When not one day in the window has a breakdown synced, fall back to the old per-turn display.
  const modelSplit = useMemo(
    () => aggregateModelBreakdown(items, creditsPerUSD),
    [items, creditsPerUSD],
  )

  if (loading) {
    return <div className="py-12 text-center text-sm text-muted-foreground">{t('common.loading')}</div>
  }

  return (
    <div className="space-y-4 p-4 sm:p-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold text-foreground">{t('accounts.usageOfficialTitle')}</span>
          <span className="rounded-md bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">
            {t('accounts.usageOfficialRetentionHint', { days: data?.retention_days ?? 7 })}
          </span>
        </div>
        <div className="flex items-center gap-2">
          {data?.last_synced_at && (
            <span className="text-[11px] text-muted-foreground">
              {t('accounts.usageOfficialLastSynced', { time: formatBeijingTime(data.last_synced_at) })}
            </span>
          )}
          <button
            type="button"
            disabled={refreshing}
            onClick={() => void load(true)}
            className="inline-flex h-8 items-center gap-1.5 rounded-lg border bg-background px-3 text-xs font-semibold text-muted-foreground transition-colors hover:text-foreground disabled:opacity-60"
          >
            <RefreshCw className={`size-3.5 ${refreshing ? 'animate-spin' : ''}`} />
            {t('accounts.usageOfficialRefresh')}
          </button>
        </div>
      </div>

      {error && <div className="rounded-lg bg-red-500/10 px-3 py-2 text-xs text-red-500">{error}</div>}
      {data?.refresh_error && (
        <div className="rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-600 dark:text-amber-400">
          {t('accounts.usageOfficialRefreshFailed', { reason: data.refresh_error })}
        </div>
      )}
      {data?.breakdown_refresh_error && (
        <div className="rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-600 dark:text-amber-400">
          {t('accounts.usageOfficialBreakdownRefreshFailed', { reason: data.breakdown_refresh_error })}
        </div>
      )}

      {items.length === 0 ? (
        <div className="py-10 text-center text-sm text-muted-foreground">
          {t('accounts.usageOfficialEmpty')}
        </div>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-3">
            <CompactMetric
              icon={<Banknote className="size-4" />}
              label={t('accounts.usageOfficialCost')}
              value={formatUSD(data?.totals.usd ?? 0)}
              // The dollar amount is converted from credits; the raw credits are shown as well, to ease reconciliation with the official console.
              detail={t('accounts.usageOfficialCredits', {
                credits: formatCredits(data?.totals.credits ?? 0),
                rate: data?.credits_per_usd ?? 25,
              })}
            />
            <CompactMetric
              icon={<Package className="size-4" />}
              label={t('accounts.usageOfficialTokens')}
              value={formatTokens(data?.totals.total_tokens ?? 0, fullNumbers)}
            />
            <CompactMetric
              icon={<Zap className="size-4" />}
              label={t('accounts.usageOfficialTurns')}
              value={formatNumber(data?.totals.turns ?? 0)}
            />
          </div>

          {data?.cycle && <OfficialCycleCards cycle={data.cycle} />}

          <section className="rounded-2xl border bg-background p-4">
            <div className="mb-3 text-xs font-semibold uppercase text-muted-foreground">
              {t('accounts.usageOfficialDailyTrend')}
            </div>
            <div className="space-y-1.5">
              {items.map((item) => (
                <OfficialUsageDayRow key={item.day} item={item} maxCredits={maxCredits} />
              ))}
            </div>
          </section>

          <div className="grid gap-3 lg:grid-cols-2">
            <OfficialSplitTable
              title={t('accounts.usageOfficialByClient')}
              rows={clientTotals}
              emptyLabel={t('accounts.usageOfficialEmpty')}
            />
            <OfficialSplitTable
              title={t('accounts.usageOfficialByModel')}
              rows={modelSplit.rows}
              emptyLabel={t('accounts.usageOfficialEmpty')}
              // With a breakdown, show the allocated cost; free accounts always have 0 credits and only a share, so show the average share instead;
              // when no day in the window has a breakdown (old snapshot), fall back to turns only.
              costMode={!modelSplit.hasBreakdown ? 'none' : modelSplit.hasCost ? 'usd' : 'share'}
              footnote={
                modelSplit.hasBreakdown && modelSplit.breakdownDays < modelSplit.totalDays
                  ? t('accounts.usageOfficialBreakdownCoverage', { covered: modelSplit.breakdownDays, total: modelSplit.totalDays })
                  : undefined
              }
            />
          </div>
        </>
      )}
    </div>
  )
}

function OfficialUsageDayRow({ item, maxCredits }: { item: WhamDailyUsageItem; maxCredits: number }) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const width = maxCredits > 0 ? Math.max(2, (item.credits / maxCredits) * 100) : 0
  return (
    <div className="flex items-center gap-3 text-xs">
      <span className="w-20 shrink-0 font-mono text-muted-foreground">{item.day.slice(5)}</span>
      <div className="h-4 flex-1 overflow-hidden rounded bg-muted/50">
        <div className="h-full rounded bg-primary/70" style={{ width: `${width}%` }} />
      </div>
      <span
        className="w-20 shrink-0 text-right font-semibold tabular-nums text-foreground"
        title={t('accounts.usageOfficialCreditsRaw', { credits: formatCredits(item.credits) })}
      >
        {formatUSD(item.usd)}
      </span>
      {/* The row for the current day (UTC) also carries tokens, but they keep changing through the day: show them when present, and add a marker while unsettled. */}
      <span
        className="w-20 shrink-0 text-right tabular-nums text-muted-foreground"
        title={item.settled ? undefined : t('accounts.usageOfficialUnsettled')}
      >
        {item.total_tokens > 0 ? formatTokens(item.total_tokens, fullNumbers) : t('accounts.usageOfficialUnsettled')}
        {item.total_tokens > 0 && !item.settled && <span className="ml-0.5 text-[10px] opacity-70">*</span>}
      </span>
    </div>
  )
}

// OfficialCycleCards shows the official cost used in the current reset cycle, the live used percentage and the quota estimate.
// Estimate = used cost / used percentage: neither upstream endpoint gives the absolute weekly quota (the breakdown endpoint's
// percent is normalized to the interval peak), and this is the only defensible derivation. The percentage has integer precision only, so a
// +/-0.5% interval is given, and it is marked unreliable below 10%.
function OfficialCycleCards({ cycle }: { cycle: WhamDailyUsageCycle }) {
  const { t } = useTranslation()
  const reasonText = (() => {
    switch (cycle.reason) {
      case 'no_window':
        return t('accounts.usageOfficialCycleReasonNoWindow')
      case 'window_stale':
        return t('accounts.usageOfficialCycleReasonWindowStale')
      case 'no_percent':
        return t('accounts.usageOfficialCycleReasonNoPercent')
      case 'no_credits':
        return t('accounts.usageOfficialCycleReasonNoCredits')
      case 'percent_too_low':
        return t('accounts.usageOfficialCycleReasonPercentTooLow')
      default:
        return ''
    }
  })()
  const percent = cycle.used_percent
  const percentText = percent == null ? '—' : `${Number.isInteger(percent) ? percent : percent.toFixed(1)}%`
  const estimate = cycle.estimate
  const estimateDetail = estimate
    ? [
        t('accounts.usageOfficialCycleEstimateRange', { low: formatUSD(estimate.usd_low), high: formatUSD(estimate.usd_high) }),
        estimate.reliable ? '' : t('accounts.usageOfficialCycleUnreliable'),
      ]
        .filter(Boolean)
        .join(' · ')
    : reasonText
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      <CompactMetric
        icon={<Coins className="size-4" />}
        label={t('accounts.usageOfficialCycleUsed')}
        value={formatUSD(cycle.used_usd)}
        detail={
          cycle.start_at
            ? t('accounts.usageOfficialCycleUsedDetail', { days: cycle.days, start: formatBeijingTime(cycle.start_at) })
            : reasonText || undefined
        }
      />
      <CompactMetric
        icon={<Gauge className="size-4" />}
        label={t('accounts.usageOfficialCyclePercent')}
        value={percentText}
        detail={
          cycle.reset_at
            ? t('accounts.usageOfficialCyclePercentDetail', {
                reset: formatBeijingTime(cycle.reset_at),
                time: cycle.used_percent_updated_at ? formatBeijingTime(cycle.used_percent_updated_at) : '—',
              })
            : reasonText || undefined
        }
      />
      <div title={t('accounts.usageOfficialCycleEstimateHint')}>
        <CompactMetric
          icon={<BarChart3 className="size-4" />}
          label={t('accounts.usageOfficialCycleEstimate')}
          value={estimate ? formatUSD(estimate.usd) : t('accounts.usageOfficialCycleUnavailable')}
          detail={estimateDetail || undefined}
        />
      </div>
    </div>
  )
}

// costMode decides the rightmost column: usd shows the allocated cost; share shows the average daily share over the window (free accounts only have
// a share and no credits); none shows nothing (the model level of old snapshots has no cost).
type SplitCostMode = 'usd' | 'share' | 'none'

function OfficialSplitTable({
  title,
  rows,
  emptyLabel,
  costMode = 'usd',
  footnote,
}: {
  title: string
  rows: AggregatedSplit[]
  emptyLabel: string
  costMode?: SplitCostMode
  // Note below the table, for example when the breakdown covers only part of the days in the window.
  footnote?: string
}) {
  const { t } = useTranslation()
  return (
    <section className="rounded-2xl border bg-background p-4">
      <div className="mb-3 text-xs font-semibold uppercase text-muted-foreground">{title}</div>
      {rows.length === 0 ? (
        <div className="py-4 text-center text-xs text-muted-foreground">{emptyLabel}</div>
      ) : (
        <div className="space-y-1.5">
          {rows.map((row) => (
            <div key={row.label} className="flex items-center justify-between gap-3 text-xs">
              <span className="min-w-0 flex-1 truncate text-foreground" title={row.label}>
                {row.label}
              </span>
              <span className="shrink-0 tabular-nums text-muted-foreground">
                {formatNumber(row.turns)} {t('accounts.usageOfficialTurnUnit')}
              </span>
              {costMode === 'usd' && (
                <span
                  className="w-20 shrink-0 text-right font-semibold tabular-nums text-foreground"
                  title={t('accounts.usageOfficialCreditsRaw', { credits: formatCredits(row.credits) })}
                >
                  {formatUSD(row.usd)}
                </span>
              )}
              {costMode === 'share' && (
                <span
                  className="w-20 shrink-0 text-right font-semibold tabular-nums text-foreground"
                  title={t('accounts.usageOfficialShareHint')}
                >
                  {formatShare(row.share)}
                </span>
              )}
            </div>
          ))}
        </div>
      )}
      {footnote && <div className="mt-3 text-[11px] text-muted-foreground">{footnote}</div>}
    </section>
  )
}

interface AggregatedSplit {
  label: string
  credits: number
  usd: number
  turns: number
  tokens: number
  // Average daily share over the window (0~1), filled only by the model breakdown; shares cannot be added across days, so this is a per-day average.
  share: number
}

interface ModelBreakdownSplit {
  rows: AggregatedSplit[]
  // At least one day in the window synced the breakdown; otherwise fall back to the per-turn view of counts.models.
  hasBreakdown: boolean
  // The breakdown has any non-zero cost; free accounts are all 0, in which case only the share can be viewed.
  hasCost: boolean
  // Days with a breakdown / days in the window: old snapshots only have a breakdown for the latest few days, and cost and turns count only those days,
  // so when coverage is incomplete the table needs a note below it, otherwise it would be read as the model cost of the whole window.
  breakdownDays: number
  totalDays: number
}

// aggregateModelBreakdown accumulates the daily model x speed share-allocated cost over the whole window.
//
// Rows are split by (model, speed); fast (priority tier) gets its own row and is marked on the label. Turns come from
// counts.models (model dimension only, no speed dimension): if a model has a standard row, they are recorded on the standard row,
// and if it only has a fast row, on the fast row, never counted twice. A model present in counts but not in the breakdown
// (for example a catalog entry with a 0 share) gets a zero-cost row, so that no turns are lost.
function aggregateModelBreakdown(items: WhamDailyUsageItem[], creditsPerUSD: number): ModelBreakdownSplit {
  const withBreakdown = items.filter((item) => item.breakdown_available && Array.isArray(item.breakdown))
  if (withBreakdown.length === 0) {
    return {
      rows: aggregateSplits(items, 'models', creditsPerUSD),
      hasBreakdown: false,
      hasCost: false,
      breakdownDays: 0,
      totalDays: items.length,
    }
  }

  type Row = AggregatedSplit & { model: string; speed: string; shareSum: number }
  const rows = new Map<string, Row>()
  const keyOf = (model: string, speed: string) => `${model} ${speed}`
  for (const item of withBreakdown) {
    for (const entry of item.breakdown as WhamDailyUsageBreakdownEntry[]) {
      const model = (entry.model ?? '').trim() || '-'
      const speed = (entry.speed ?? '').trim().toLowerCase() || 'standard'
      const key = keyOf(model, speed)
      const row =
        rows.get(key) ??
        {
          label: speed === 'standard' ? model : `${model} · ${speed}`,
          model,
          speed,
          credits: 0,
          usd: 0,
          turns: 0,
          tokens: 0,
          share: 0,
          shareSum: 0,
        }
      row.credits += entry.credits ?? 0
      row.usd += (entry.credits ?? 0) / creditsPerUSD
      row.shareSum += entry.share ?? 0
      rows.set(key, row)
    }
  }

  // Turns are summed by model, then attached to that model's standard row (if none, to its only other speed row).
  // Only the days with a breakdown are counted: cost and turns must cover the same window, otherwise 7 days of cost paired with 30 days of turns
  // would make the per-turn cost misleading.
  const turnsByModel = new Map<string, number>()
  for (const item of withBreakdown) {
    for (const split of item.models ?? []) {
      const model = (split.model ?? '').trim() || '-'
      turnsByModel.set(model, (turnsByModel.get(model) ?? 0) + (split.turns ?? 0))
    }
  }
  for (const [model, turns] of turnsByModel) {
    const standard = rows.get(keyOf(model, 'standard'))
    if (standard) {
      standard.turns += turns
      continue
    }
    const sibling = [...rows.values()].find((row) => row.model === model)
    if (sibling) {
      sibling.turns += turns
      continue
    }
    rows.set(keyOf(model, 'standard'), {
      label: model,
      model,
      speed: 'standard',
      credits: 0,
      usd: 0,
      turns,
      tokens: 0,
      share: 0,
      shareSum: 0,
    })
  }

  const days = withBreakdown.length
  const out: AggregatedSplit[] = [...rows.values()].map(({ model: _model, speed: _speed, shareSum, ...row }) => ({
    ...row,
    share: days > 0 ? shareSum / days : 0,
  }))
  const hasCost = out.some((row) => row.usd > 0)
  out.sort((a, b) =>
    hasCost
      ? (b.usd - a.usd) || (b.turns - a.turns)
      : (b.share - a.share) || (b.turns - a.turns),
  )
  return { rows: out, hasBreakdown: true, hasCost, breakdownDays: days, totalDays: items.length }
}

// aggregateSplits accumulates the daily split arrays by client_id / model over the whole window, in descending cost order.
// counts gives no credits at the model level, so using this function directly for models can only sort by turns; the model cost
// goes through aggregateModelBreakdown.
function aggregateSplits(
  items: WhamDailyUsageItem[],
  field: 'clients' | 'models',
  creditsPerUSD: number,
): AggregatedSplit[] {
  const totals = new Map<string, AggregatedSplit>()
  for (const item of items) {
    const splits: WhamDailyUsageSplit[] = item[field] ?? []
    for (const split of splits) {
      const label = (field === 'clients' ? split.client_id : split.model)?.trim() || '-'
      const current = totals.get(label) ?? { label, credits: 0, usd: 0, turns: 0, tokens: 0, share: 0 }
      current.credits += split.credits ?? 0
      current.usd += (split.credits ?? 0) / creditsPerUSD
      current.turns += split.turns ?? 0
      current.tokens += split.text_total_tokens ?? 0
      totals.set(label, current)
    }
  }
  return [...totals.values()].sort((a, b) => (b.usd - a.usd) || (b.turns - a.turns))
}

// credits is the official raw billing unit, kept to two decimals (upstream gives decimals); integers are not zero-padded.
function formatCredits(value: number): string {
  if (!Number.isFinite(value)) return '0'
  const rounded = Math.round(value * 100) / 100
  return rounded.toLocaleString(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  })
}

function formatUSD(value: number): string {
  if (!Number.isFinite(value) || value === 0) return '$0'
  if (Math.abs(value) < 0.01) return '<$0.01'
  return `$${value.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
}

// A share (0~1) is shown as a percentage; fragments below 0.1% are shown as "<0.1%".
function formatShare(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0%'
  const percent = value * 100
  if (percent < 0.1) return '<0.1%'
  return `${percent.toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: percent < 10 ? 1 : 0 })}%`
}

function QualitySignals({ data }: { data: AccountUsageDetail }) {
  const { t } = useTranslation()
  return (
    <section className="rounded-2xl border bg-background p-4">
      <div className="mb-3 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h4 className="text-base font-semibold text-foreground">{t('accounts.usageQualitySignals')}</h4>
          <p className="text-sm text-muted-foreground">{t('accounts.usageQualitySignalsDesc')}</p>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <QualityMetric
          icon={<Activity className="size-4" />}
          label={t('accounts.usageErrorRate')}
          value={formatPercent(data.error_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.error_requests), total: formatNumber(data.total_requests) })}
          tone={data.error_rate >= 10 ? 'danger' : data.error_rate >= 3 ? 'warning' : 'success'}
        />
        <QualityMetric
          icon={<Gauge className="size-4" />}
          label={t('accounts.usageRetryRequests')}
          value={formatNumber(data.retry_requests)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.retry_requests), total: formatNumber(data.total_requests) })}
          tone={data.retry_requests > 0 ? 'warning' : 'success'}
        />
        <QualityMetric
          icon={<Clock3 className="size-4" />}
          label={t('accounts.usageAvgFirstToken')}
          value={formatDurationOrDash(data.avg_first_token_ms)}
          detail={t('accounts.usageSamplesCount', { count: formatNumber(data.first_token_samples) })}
          tone={data.avg_first_token_ms >= 5000 ? 'danger' : data.avg_first_token_ms >= 2000 ? 'warning' : 'neutral'}
        />
        <QualityMetric
          icon={<Gauge className="size-4" />}
          label={t('accounts.usageP95Response')}
          value={formatDurationOrDash(data.p95_duration_ms)}
          detail={t('accounts.usageSamplesCount', { count: formatNumber(data.total_requests) })}
          tone={data.p95_duration_ms >= 30000 ? 'danger' : data.p95_duration_ms >= 10000 ? 'warning' : 'neutral'}
        />
        <QualityMetric
          icon={<Zap className="size-4" />}
          label={t('accounts.usageStreamShare')}
          value={formatPercent(data.stream_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.stream_requests), total: formatNumber(data.total_requests) })}
        />
        <QualityMetric
          icon={<Package className="size-4" />}
          label={t('accounts.usageCompactShare')}
          value={formatPercent(data.compact_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.compact_requests), total: formatNumber(data.total_requests) })}
        />
      </div>
    </section>
  )
}

function DetailPage({
  data,
  activeDays,
  periodDays,
}: {
  data: AccountUsageDetail
  activeDays: number
  periodDays: number
}) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const [modelMetric, setModelMetric] = useState<ModelMetricKey>('requests')
  const activeDaysText = formatActiveDaysText(activeDays, periodDays, t('accounts.usageDaysUnit'))
  const sortedModels = useMemo(() => {
    return [...data.models].sort((a, b) => {
      const valueDiff = modelMetricValue(b, modelMetric) - modelMetricValue(a, modelMetric)
      if (valueDiff !== 0) return valueDiff
      return b.requests - a.requests
    })
  }, [data.models, modelMetric])
  const modelMetricTotal = useMemo(
    () => sortedModels.reduce((sum, item) => sum + modelMetricValue(item, modelMetric), 0),
    [sortedModels, modelMetric],
  )
  const topModel = sortedModels[0]
  const chartModels = useMemo(
    () => sortedModels.map((model) => ({
      ...model,
      metric_value: modelMetricValue(model, modelMetric),
    })),
    [sortedModels, modelMetric],
  )

  return (
    <div className="p-4 sm:p-5">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_340px]">
        <section className="rounded-2xl border bg-background p-4">
          <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <h4 className="text-base font-semibold">{t('accounts.modelDistribution')}</h4>
              <p className="text-sm text-muted-foreground">
                {topModel ? t('accounts.usageTopModelByMetric', { model: topModel.model }) : '-'}
              </p>
            </div>
            <div className="flex flex-wrap items-center justify-end gap-2">
              <span className="rounded-full bg-muted px-3 py-1 text-xs font-semibold text-muted-foreground">
                {formatModelMetricValue(modelMetricTotal, modelMetric, fullNumbers)} {t(modelMetricLabelKey(modelMetric))}
              </span>
              <div className="flex rounded-lg border bg-muted/40 p-1">
                {MODEL_METRIC_OPTIONS.map((option) => (
                  <ModelMetricButton
                    key={option.key}
                    active={modelMetric === option.key}
                    label={t(option.labelKey)}
                    onClick={() => setModelMetric(option.key)}
                  />
                ))}
              </div>
            </div>
          </div>
          <div className="grid gap-4 md:grid-cols-[230px_minmax(0,1fr)]">
            <div className="h-[230px]">
              <ResponsiveContainer width="100%" height="100%">
                <PieChart>
                  <Pie
                    data={chartModels}
                    dataKey="metric_value"
                    nameKey="model"
                    cx="50%"
                    cy="50%"
                    innerRadius={62}
                    outerRadius={92}
                    // Consistent with the Usage / Key Usage portals: no gap between sectors, to avoid white seams in the donut chart
                    paddingAngle={0}
                    strokeWidth={0}
                  >
                    {chartModels.map((_, i) => (
                      <Cell key={i} fill={COLORS[i % COLORS.length]} />
                    ))}
                  </Pie>
                  <Tooltip
                    formatter={(value, name) => [formatModelMetricValue(Number(value || 0), modelMetric, fullNumbers), String(name ?? '')]}
                    contentStyle={{ fontSize: 12, borderRadius: 8, border: '1px solid hsl(var(--border))' }}
                  />
                </PieChart>
              </ResponsiveContainer>
            </div>
            <div className="min-w-0 space-y-2 self-center">
              {sortedModels.map((m, i) => (
                <ModelRow
                  key={m.model}
                  color={COLORS[i % COLORS.length]}
                  model={m}
                  metric={modelMetric}
                  total={modelMetricTotal}
                />
              ))}
            </div>
          </div>
        </section>

        <section className="rounded-2xl border bg-background p-4">
          <div className="mb-4 flex items-center gap-2">
            <span className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
              <Package className="size-4" />
            </span>
            <h4 className="text-base font-semibold">{t('accounts.usageTokenBreakdown')}</h4>
          </div>
          <div className="space-y-4">
            <TokenBar label={t('accounts.inputTokens')} value={data.input_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.outputTokens')} value={data.output_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.reasoningTokens')} value={data.reasoning_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.cachedTokens')} value={data.cached_tokens} total={data.total_tokens} />
          </div>
        </section>
      </div>

      <KeyDistribution data={data} />

      <div className="mt-4 grid gap-3 md:grid-cols-4">
        <DetailKpi label={t('accounts.usageActiveDays')} value={activeDaysText} />
        <DetailKpi label={t('accounts.usageDailyAvgTokens')} value={formatTokens(Math.round(data.avg_daily_tokens), fullNumbers)} />
        <DetailKpi label={t('accounts.usageCacheHitRate')} value={formatPercent(data.cache_hit_rate)} />
        <DetailKpi label={t('accounts.usageAvgResponse')} value={formatDuration(data.avg_duration_ms)} />
      </div>
    </div>
  )
}

function KeyDistribution({ data }: { data: AccountUsageDetail }) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const [metric, setMetric] = useState<ModelMetricKey>('requests')
  const sortedKeys = useMemo(() => {
    return [...(data.by_api_key || [])].sort((a, b) => {
      const diff = keyMetricValue(b, metric) - keyMetricValue(a, metric)
      if (diff !== 0) return diff
      return b.requests - a.requests
    })
  }, [data.by_api_key, metric])
  const metricTotal = useMemo(
    () => sortedKeys.reduce((sum, item) => sum + keyMetricValue(item, metric), 0),
    [sortedKeys, metric],
  )
  const topKey = sortedKeys[0]

  return (
    <section className="mt-4 rounded-2xl border bg-background p-4">
      <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex items-start gap-2.5">
          <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <KeyRound className="size-4" />
          </span>
          <div>
            <h4 className="text-base font-semibold">{t('accounts.usageKeyDistribution')}</h4>
            <p className="text-sm text-muted-foreground">
              {topKey
                ? t('accounts.usageTopKeyByMetric', { key: keyDisplayName(topKey, t) })
                : t('accounts.usageKeyDistributionDesc')}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <span className="rounded-full bg-muted px-3 py-1 text-xs font-semibold tabular-nums text-muted-foreground">
            {formatModelMetricValue(metricTotal, metric, fullNumbers)} {t(modelMetricLabelKey(metric))}
          </span>
          {sortedKeys.length > 0 && (
            <span className="rounded-full bg-muted/70 px-3 py-1 text-xs font-medium text-muted-foreground">
              {t('accounts.usageKeyCount', { count: sortedKeys.length })}
            </span>
          )}
          <div className="flex rounded-lg border bg-muted/40 p-1">
            {MODEL_METRIC_OPTIONS.map((option) => (
              <ModelMetricButton
                key={option.key}
                active={metric === option.key}
                label={t(option.labelKey)}
                onClick={() => setMetric(option.key)}
              />
            ))}
          </div>
        </div>
      </div>
      {sortedKeys.length === 0 ? (
        <div className="flex h-20 flex-col items-center justify-center gap-1.5 rounded-xl border border-dashed bg-muted/20 text-sm text-muted-foreground">
          <KeyRound className="size-4 opacity-60" />
          {t('accounts.usageKeyEmpty')}
        </div>
      ) : (
        <div className="grid gap-2 sm:grid-cols-2">
          {sortedKeys.map((k, i) => (
            <KeyRow
              key={`${k.api_key_id}-${k.api_key_masked}-${i}`}
              color={COLORS[i % COLORS.length]}
              stat={k}
              metric={metric}
              total={metricTotal}
            />
          ))}
        </div>
      )}
    </section>
  )
}

function KeyRow({
  color,
  stat,
  metric,
  total,
}: {
  color: string
  stat: AccountKeyStat
  metric: ModelMetricKey
  total: number
}) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const value = keyMetricValue(stat, metric)
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  const detail = keyMetricDetail(stat, metric, t, fullNumbers)
  return (
    <div className="rounded-xl border border-border/80 bg-muted/15 px-3 py-2.5 transition-colors hover:border-border hover:bg-muted/25">
      <div className="mb-2 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
        <span
          className="size-2.5 shrink-0 rounded-full ring-2 ring-background"
          style={{ background: color }}
        />
        <span className="flex min-w-0 items-baseline gap-1.5">
          <span className="truncate font-medium text-foreground">{keyDisplayName(stat, t)}</span>
          {stat.api_key_masked && (
            <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] tracking-wide text-muted-foreground">
              {stat.api_key_masked}
            </span>
          )}
        </span>
        <span className="tabular-nums text-xs font-semibold text-muted-foreground">
          {formatPercent(percent)}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div
          className="h-full rounded-full transition-[width] duration-300"
          style={{ width: `${percent}%`, background: color }}
        />
      </div>
      <div className="mt-2 flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span className="font-semibold tabular-nums text-foreground">
          {formatModelMetricValue(value, metric, fullNumbers)}
        </span>
        <span className="truncate text-right">{detail}</span>
      </div>
    </div>
  )
}

// CreditSettings exposes only one switch: "Use credits to bypass rate limits".
//
// Historically there were two switch levels here, and the outer "Enable credit" was a front fuse added in commit c72e267
// (it changed the formerly independent CreditSkipUsageWindow so that it takes effect only when both are true). But it produced
// no behavior of its own and only hid the switch that does the real work, making the feature hard to find.
//
// The backend gate `CreditEnabled && CreditSkipUsageWindow` is **left unchanged**: production may hold historical data
// with only one of the columns on, and removing the gate would suddenly make them bypass rate limits. Here one switch
// writes both columns, so behavior is unchanged and the UI just no longer exposes the meaningless middle state.
function CreditSettings({
  account,
  creditEnabled,
  creditSkipWindow,
  savingCredit,
  creditError,
  onToggle,
}: {
  account: AccountRow
  creditEnabled: boolean
  creditSkipWindow: boolean
  savingCredit: boolean
  creditError: string | null
  onToggle: (value: boolean) => Promise<void>
}) {
  const { t } = useTranslation()
  // The states of the credit gate, telling the user whether this switch actually takes effect right now:
  // unlimited / has_credits true -> bypass rate limits; upstream reports overage or restricted workspace -> rate limits restored; not probed -> treated as no credits.
  const rawBalance = (account.credits_balance ?? '').trim()
  const balance = Number.parseFloat(rawBalance)
  const unlimited = account.credits_unlimited === true
  const probed = account.credits_valid === true || account.credits_balance != null || unlimited
  const overageReached = account.credits_overage_limit_reached === true
  const hardStopped = overageReached || isWorkspaceCreditHardStop(account)
  const hasCredits =
    !hardStopped &&
    (unlimited || account.credits_has_credits === true)

  const balanceDisplay = rawBalance !== '' && Number.isFinite(balance) && balance > 0
    ? formatCreditsBalance(balance)
    : null

  const skipHint = !probed
    ? t('accounts.creditSkipWindowHintUnprobed')
    : hardStopped
      ? (overageReached ? t('accounts.creditSkipWindowHintOverage') : t('accounts.creditSkipWindowHintHardStopped'))
      : unlimited
        ? t('accounts.creditSkipWindowHintUnlimited')
        : hasCredits
          ? (balanceDisplay
              ? t('accounts.creditSkipWindowHintActive', { balance: balanceDisplay })
              : t('accounts.creditSkipWindowHintAvailable'))
          : t('accounts.creditSkipWindowHintDrained')

  // It counts as on only when both columns are true. A combination with only one column on in historical data does not take effect in the backend anyway, so showing it as off is accurate.
  const skipActive = creditEnabled && creditSkipWindow

  return (
    <div className="mt-5 rounded-2xl border bg-card p-4">
      <h4 className="mb-3 text-base font-semibold">{t('accounts.creditSettings')}</h4>
      {creditError && (
        <div className="mb-3 text-xs text-red-500">{creditError}</div>
      )}
      <div className="space-y-3">
        <CreditToggle
          label={t('accounts.creditSkipWindow')}
          hint={skipHint}
          checked={skipActive}
          disabled={savingCredit}
          onClick={() => void onToggle(!skipActive)}
        />
        {/* When the switch is on but there are no credits to spend right now, say so clearly: scheduling will not let requests through, and the state is still rate limited. */}
        {skipActive && !hasCredits && (
          <p className="flex items-start gap-1.5 rounded-lg bg-amber-500/10 px-2.5 py-2 text-xs text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>{t('accounts.creditSkipWindowInactive')}</span>
          </p>
        )}
        {/* When on and credits are available, give positive confirmation, to avoid "turned on but unsure whether it helps". */}
        {skipActive && hasCredits && (
          <p className="flex items-start gap-1.5 rounded-lg bg-teal-500/10 px-2.5 py-2 text-xs text-teal-700 dark:text-teal-300">
            <Coins className="mt-0.5 size-3.5 shrink-0" />
            <span>{t('accounts.creditSkipWindowActiveNote')}</span>
          </p>
        )}
      </div>
    </div>
  )
}

// formatCreditsBalance reduces long decimals such as "1000.0000000000" to 2 places, so the switch description is not stretched.
function formatCreditsBalance(balance: number): string {
  if (!Number.isFinite(balance)) return '-'
  return balance.toFixed(2)
}

function ResetCreditsSection({
  account,
  onResetDone,
}: {
  account: AccountRow
  onResetDone?: () => void
}) {
  const { t } = useTranslation()
  const initial =
    typeof account.rate_limit_reset_credits === 'number'
      ? account.rate_limit_reset_credits
      : null
  const [count, setCount] = useState<number | null>(initial)
  const [credits, setCredits] = useState<ResetCreditItem[] | null>(null)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [resetting, setResetting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [done, setDone] = useState(false)

  // On opening the dialog, fetch the reset voucher details (the validity of each one, issue #322).
  // The details endpoint also returns the authoritative number of available vouchers, which calibrates the local count.
  // Only issued for accounts known to have reset-count semantics (count===null means a non-Codex account or not yet probed).
  const loadDetail = useCallback(async () => {
    setDetailError(null)
    try {
      const res = await api.getResetCredits(account.id)
      setCredits(res.credits ?? [])
      if (typeof res.available_count === 'number') {
        setCount(res.available_count)
      }
    } catch (err) {
      // A failure to fetch the details does not affect the existing count display and reset button; it only notes that the details are unavailable.
      setDetailError(getErrorMessage(err))
    }
  }, [account.id])

  useEffect(() => {
    if (initial === null) return
    void loadDetail()
  }, [initial, loadDetail])

  // When the count is unknown (non-Codex account or not yet probed), this block is not shown.
  if (count === null) return null

  const handleReset = async () => {
    setResetting(true)
    setError(null)
    try {
      const res = await api.resetCredits(account.id)
      const next =
        typeof res.rate_limit_reset_credits === 'number'
          ? res.rate_limit_reset_credits
          : Math.max(0, count - 1)
      setCount(next)
      setDone(true)
      setConfirming(false)
      // The backend has already waited for the usage probe, so the refresh now gets the new usage and status.
      onResetDone?.()
      // When the probe times out without landing (usage_refreshed=false), refresh once more, otherwise the progress bar stays on the old value.
      if (res.usage_refreshed === false) {
        window.setTimeout(() => onResetDone?.(), 5000)
      }
      // The reset consumed one voucher; refetch the details to sync the validity list.
      void loadDetail()
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setResetting(false)
    }
  }

  const balanceDisplay = account.credits_has_credits
    ? account.credits_unlimited
      ? t('accounts.creditsBalanceUnlimitedShort')
      : (account.credits_balance ?? '').trim() || null
    : null
  const applicable =
    typeof account.applicable_reset_credits === 'number'
      ? account.applicable_reset_credits
      : null

  return (
    <div className="mt-5 rounded-2xl border bg-card p-4">
      <h4 className="mb-3 text-base font-semibold">{t('accounts.resetCreditsTitle')}</h4>
      {balanceDisplay !== null && (
        <div className="mb-3 flex items-center justify-between gap-4 rounded-xl bg-muted/40 px-3 py-2">
          <span className="text-sm font-medium">{t('accounts.creditsBalanceLabel')}</span>
          <span className="flex items-center gap-2">
            <span className="text-base font-semibold tabular-nums text-foreground">{balanceDisplay}</span>
            {account.credits_overage_limit_reached && (
              <span className="rounded-md bg-red-50 px-1.5 py-0.5 text-[10px] font-medium text-red-600 ring-1 ring-inset ring-red-500/20 dark:bg-red-950 dark:text-red-400 dark:ring-red-400/20">
                {t('accounts.creditsOverageReached')}
              </span>
            )}
          </span>
        </div>
      )}
      {error && <div className="mb-3 text-xs text-red-500">{error}</div>}
      {done && !error && (
        <div className="mb-3 text-xs text-emerald-600">{t('accounts.resetCreditsSuccess')}</div>
      )}
      <div className="flex items-center justify-between gap-4">
        <div>
          <p className="text-sm font-medium">{t('accounts.resetCreditsLabel')}</p>
          <p className="text-xs text-muted-foreground">{t('accounts.resetCreditsHint')}</p>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex flex-col items-end">
            <span className="text-2xl font-semibold tabular-nums text-foreground">{count}</span>
            {count > 0 && applicable === 0 && (
              <span className="text-[10px] text-muted-foreground">
                {t('accounts.resetCreditsNotApplicable')}
              </span>
            )}
          </div>
          {count > 0 &&
            (confirming ? (
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  disabled={resetting}
                  onClick={() => void handleReset()}
                  className="inline-flex h-8 items-center gap-1.5 rounded-lg bg-primary px-3 text-xs font-semibold text-primary-foreground transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-60"
                >
                  {resetting ? t('common.loading') : t('accounts.resetCreditsConfirmButton')}
                </button>
                <button
                  type="button"
                  disabled={resetting}
                  onClick={() => setConfirming(false)}
                  className="inline-flex h-8 items-center rounded-lg border bg-background px-3 text-xs font-semibold text-muted-foreground transition-colors hover:text-foreground disabled:opacity-60"
                >
                  {t('common.cancel')}
                </button>
              </div>
            ) : (
              <button
                type="button"
                onClick={() => {
                  setDone(false)
                  setConfirming(true)
                }}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border bg-background px-3 text-xs font-semibold text-foreground transition-colors hover:bg-muted/60"
              >
                <RotateCcw className="size-3.5" />
                {t('accounts.resetCreditsButton')}
              </button>
            ))}
        </div>
      </div>
      {confirming && (
        <p className="mt-3 text-xs text-amber-600">
          {t('accounts.resetCreditsConfirmMessage')}
        </p>
      )}
      <ResetCreditExpiryList credits={credits} detailError={detailError} />
    </div>
  )
}

// Reset voucher validity detail list: shows each voucher in ascending expiry order, the soonest to expire first,
// and highlights vouchers expiring within 7 days, so those closest to expiry are used first (issue #322).
function ResetCreditExpiryList({
  credits,
  detailError,
}: {
  credits: ResetCreditItem[] | null
  detailError: string | null
}) {
  const { t } = useTranslation()

  const sorted = useMemo(() => {
    if (!credits) return []
    return credits
      .map((credit, index) => {
        const expiresAtMs = new Date(credit.expires_at).getTime()
        return Number.isFinite(expiresAtMs)
          ? { key: credit.id || String(index), expiresAtMs, credit }
          : null
      })
      .filter((item): item is { key: string; expiresAtMs: number; credit: ResetCreditItem } =>
        Boolean(item),
      )
      .sort((a, b) => a.expiresAtMs - b.expiresAtMs || a.key.localeCompare(b.key))
  }, [credits])

  if (detailError) {
    return (
      <p className="mt-3 text-xs text-muted-foreground">
        {t('accounts.resetCreditsDetailUnavailable')}
      </p>
    )
  }
  if (!credits || sorted.length === 0) return null

  const nowMs = Date.now()
  const soonThresholdMs = 7 * 24 * 60 * 60 * 1000

  return (
    <div className="mt-3 border-t pt-3">
      <p className="mb-2 text-xs font-medium text-muted-foreground">
        {t('accounts.resetCreditsExpiryTitle')}
      </p>
      <ul className="space-y-1">
        {sorted.map((item, index) => {
          const expiringSoon = item.expiresAtMs - nowMs <= soonThresholdMs
          return (
            <li
              key={item.key}
              className="flex items-center justify-between gap-3 text-xs"
            >
              <span className="text-muted-foreground">
                {t('accounts.resetCreditsExpiryItem', { index: index + 1 })}
              </span>
              <span
                className={`tabular-nums ${expiringSoon ? 'font-medium text-amber-600' : 'text-foreground'}`}
              >
                {t('accounts.resetCreditsExpiresAt', {
                  time: formatBeijingTime(item.credit.expires_at),
                })}
              </span>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

function PageButton({
  active,
  icon,
  label,
  onClick,
}: {
  active: boolean
  icon: ReactNode
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`inline-flex min-w-20 items-center justify-center gap-1.5 rounded-md px-3 text-sm font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {icon}
      {label}
    </button>
  )
}

function RangeButton({
  active,
  label,
  onClick,
}: {
  active: boolean
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`h-7 min-w-12 rounded-md px-2.5 text-xs font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {label}
    </button>
  )
}

function ModelMetricButton({
  active,
  label,
  onClick,
}: {
  active: boolean
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`h-7 min-w-14 rounded-md px-2.5 text-xs font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {label}
    </button>
  )
}

function MiniPill({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-xl border bg-background/80 px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  )
}

function CompactMetric({
  icon,
  label,
  value,
  detail,
}: {
  icon: ReactNode
  label: string
  value: string
  // Sub-row: shows the raw figure before conversion (such as the original credits value under a dollar amount).
  detail?: string
}) {
  return (
    <div className="rounded-xl border bg-background px-3 py-3">
      <div className="mb-2 flex items-center gap-2 text-muted-foreground">
        {icon}
        <span className="text-xs font-medium">{label}</span>
      </div>
      <div className="text-xl font-semibold tabular-nums text-foreground">{value}</div>
      {detail && (
        <div className="mt-0.5 text-[11px] tabular-nums text-muted-foreground">{detail}</div>
      )}
    </div>
  )
}

function SignalCard({
  icon,
  title,
  rows,
}: {
  icon: ReactNode
  title: string
  rows: Array<[string, string]>
}) {
  return (
    <div className="rounded-2xl border bg-background p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          {icon}
        </span>
        <h4 className="font-semibold text-foreground">{title}</h4>
      </div>
      <div className="space-y-2">
        {rows.map(([label, value]) => (
          <div key={label} className="flex items-center justify-between gap-4">
            <span className="text-sm text-muted-foreground">{label}</span>
            <span className="font-semibold tabular-nums text-foreground">{value}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function HighlightStrip({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <div className="rounded-xl border bg-background p-4">
      <div className="text-xs font-semibold uppercase text-muted-foreground">{label}</div>
      <div className="mt-2 truncate text-lg font-semibold text-foreground">{value}</div>
      <div className="mt-1 truncate text-sm text-muted-foreground">{detail}</div>
    </div>
  )
}

function QualityMetric({
  icon,
  label,
  value,
  detail,
  tone = 'neutral',
}: {
  icon: ReactNode
  label: string
  value: string
  detail: string
  tone?: QualityTone
}) {
  const toneClass = qualityToneClass(tone)
  return (
    <div className={`rounded-xl border px-3 py-3 ${toneClass.box}`}>
      <div className="mb-3 flex items-center justify-between gap-3">
        <span className={`flex size-8 items-center justify-center rounded-lg ${toneClass.icon}`}>
          {icon}
        </span>
        <span className="text-xs font-medium text-muted-foreground">{label}</span>
      </div>
      <div className={`text-2xl font-semibold tabular-nums ${toneClass.value}`}>{value}</div>
      <div className="mt-1 text-xs text-muted-foreground">{detail}</div>
    </div>
  )
}

function qualityToneClass(tone: QualityTone): { box: string; icon: string; value: string } {
  switch (tone) {
    case 'success':
      return {
        box: 'bg-emerald-500/5 border-emerald-500/20',
        icon: 'bg-emerald-500/10 text-emerald-600',
        value: 'text-emerald-600',
      }
    case 'warning':
      return {
        box: 'bg-amber-500/5 border-amber-500/20',
        icon: 'bg-amber-500/10 text-amber-600',
        value: 'text-amber-600',
      }
    case 'danger':
      return {
        box: 'bg-red-500/5 border-red-500/20',
        icon: 'bg-red-500/10 text-red-600',
        value: 'text-red-600',
      }
    default:
      return {
        box: 'bg-background',
        icon: 'bg-muted text-muted-foreground',
        value: 'text-foreground',
      }
  }
}

function UsageTrend({ history }: { history: AccountUsageDayStat[] }) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const display = history.slice(-60)
  const maxCost = Math.max(...display.map((day) => day.account_billed), 0)
  const maxRequests = Math.max(...display.map((day) => day.requests), 0)

  return (
    <div className="mt-5 border-t pt-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div>
          <div className="text-sm font-semibold text-foreground">{t('accounts.usageTrend')}</div>
          <div className="text-xs text-muted-foreground">
            {display.length > 0
              ? t('accounts.usageTrendDays', { count: display.length })
              : t('accounts.usageTrendEmpty')}
          </div>
        </div>
        {display.length > 0 && (
          <div className="text-right text-xs text-muted-foreground">
            <div>{t('accounts.usageHighestCostDay')}: ${formatCost(maxCost)}</div>
            <div>{t('accounts.usageHighestRequestDay')}: {formatCompactNumber(maxRequests, fullNumbers)}</div>
          </div>
        )}
      </div>
      {display.length === 0 ? (
        <div className="flex h-20 items-center justify-center rounded-xl border bg-background text-sm text-muted-foreground">
          {t('accounts.usageTrendEmpty')}
        </div>
      ) : (
        <div className="flex h-20 items-end gap-1 rounded-xl border bg-background px-2 py-2">
          {display.map((day) => {
            const height = maxCost > 0 ? Math.max(8, (day.account_billed / maxCost) * 100) : Math.max(8, (day.requests / Math.max(maxRequests, 1)) * 100)
            return (
              <div
                key={day.date}
                title={`${day.label || day.date}: $${formatCost(day.account_billed)} / ${formatNumber(day.requests)} ${t('accounts.usageReqUnit')}`}
                className="min-w-[3px] flex-1 rounded-t bg-foreground/70 hover:bg-foreground"
                style={{ height: `${height}%` }}
              />
            )
          })}
        </div>
      )}
    </div>
  )
}

function ModelRow({
  color,
  model,
  metric,
  total,
}: {
  color: string
  model: AccountModelStat
  metric: ModelMetricKey
  total: number
}) {
  const fullNumbers = useShowFullUsageNumbers()
  const { t } = useTranslation()
  const value = modelMetricValue(model, metric)
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  const detail = modelMetricDetail(model, metric, t, fullNumbers)
  return (
    <div className="rounded-xl border bg-background px-3 py-2.5">
      <div className="mb-2 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
        <span className="size-2.5 rounded-full" style={{ background: color }} />
        <span className="truncate font-medium text-foreground">{model.model}</span>
        <span className="tabular-nums text-muted-foreground">{formatPercent(percent)}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className="h-full rounded-full" style={{ width: `${percent}%`, background: color }} />
      </div>
      <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground">
        <span className="font-semibold text-foreground">{formatModelMetricValue(value, metric, fullNumbers)}</span>
        <span className="truncate text-right">{detail}</span>
      </div>
    </div>
  )
}

function TokenBar({ label, value, total }: { label: string; value: number; total: number }) {
  const fullNumbers = useShowFullUsageNumbers()
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  return (
    <div>
      <div className="mb-1 flex items-center justify-between gap-3 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-semibold tabular-nums text-foreground">{formatTokens(value, fullNumbers)}</span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-muted">
        <div className="h-full rounded-full bg-foreground" style={{ width: `${percent}%` }} />
      </div>
    </div>
  )
}

function DetailKpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-xl border bg-background p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="mt-1 text-xl font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  )
}

function CreditToggle({
  label,
  hint,
  checked,
  disabled,
  onClick,
}: {
  label: string
  hint: string
  checked: boolean
  disabled: boolean
  onClick: () => void
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-sm font-medium">{label}</p>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </div>
      <button
        type="button"
        role="switch"
        aria-label={label}
        aria-checked={checked}
        disabled={disabled}
        onClick={onClick}
        className={`relative inline-flex h-5 w-9 shrink-0 ${disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer'} items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60 focus-visible:ring-offset-2 ${checked ? 'bg-primary' : 'bg-muted'}`}
      >
        <span className={`pointer-events-none block size-4 rounded-full bg-white shadow transition-transform ${checked ? 'translate-x-4' : 'translate-x-0'}`} />
      </button>
    </div>
  )
}

function emptyDayStat(): AccountUsageDayStat {
  return {
    date: '',
    label: '',
    requests: 0,
    tokens: 0,
    account_billed: 0,
    user_billed: 0,
  }
}

function emptyUsageDetail(): AccountUsageDetail {
  return {
    period_days: 30,
    active_days: 0,
    total_requests: 0,
    total_tokens: 0,
    input_tokens: 0,
    output_tokens: 0,
    reasoning_tokens: 0,
    cached_tokens: 0,
    cache_hit_rate: 0,
    total_account_billed: 0,
    total_user_billed: 0,
    avg_daily_account_billed: 0,
    avg_daily_user_billed: 0,
    avg_daily_requests: 0,
    avg_daily_tokens: 0,
    avg_duration_ms: 0,
    avg_first_token_ms: 0,
    p95_duration_ms: 0,
    error_requests: 0,
    error_rate: 0,
    retry_requests: 0,
    first_token_samples: 0,
    stream_requests: 0,
    stream_rate: 0,
    compact_requests: 0,
    compact_rate: 0,
    today: emptyDayStat(),
    history: [],
    models: [],
    by_api_key: [],
  }
}

function usageRangeToDays(range: UsageRangeKey): number {
  return USAGE_RANGE_OPTIONS.find((option) => option.key === range)?.days ?? 30
}

function formatActiveDaysText(activeDays: number, periodDays: number, dayUnit: string): string {
  if (periodDays > 0) return `${activeDays} / ${periodDays}`
  return `${activeDays} ${dayUnit}`
}

function modelMetricValue(model: AccountModelStat, metric: ModelMetricKey): number {
  switch (metric) {
    case 'tokens':
      return Number(model.tokens || 0)
    case 'cost':
      return Number(model.account_billed || 0)
    default:
      return Number(model.requests || 0)
  }
}

function modelMetricLabelKey(metric: ModelMetricKey): string {
  switch (metric) {
    case 'tokens':
      return 'accounts.usageModelMetricTokens'
    case 'cost':
      return 'accounts.usageModelMetricCost'
    default:
      return 'accounts.usageModelMetricRequests'
  }
}

function formatModelMetricValue(value: number, metric: ModelMetricKey, full: boolean): string {
  switch (metric) {
    case 'tokens':
      return formatTokens(value, full)
    case 'cost':
      return `$${formatCost(value)}`
    default:
      return formatNumber(value)
  }
}

function modelMetricDetail(model: AccountModelStat, metric: ModelMetricKey, t: (key: string) => string, full: boolean): string {
  const requests = `${formatNumber(model.requests)} ${t('accounts.usageReqUnit')}`
  const tokens = `${formatTokens(model.tokens, full)} ${t('accounts.usageTokUnit')}`
  const cost = `$${formatCost(model.account_billed)}`
  switch (metric) {
    case 'tokens':
      return `${requests} · ${cost}`
    case 'cost':
      return `${requests} · ${tokens}`
    default:
      return `${tokens} · ${cost}`
  }
}

function keyDisplayName(stat: AccountKeyStat, t: (key: string) => string): string {
  return stat.api_key_name?.trim() || t('accounts.usageKeyUnnamed')
}

function keyMetricValue(stat: AccountKeyStat, metric: ModelMetricKey): number {
  switch (metric) {
    case 'tokens':
      return Number(stat.tokens || 0)
    case 'cost':
      return Number(stat.account_billed || 0)
    default:
      return Number(stat.requests || 0)
  }
}

function keyMetricDetail(stat: AccountKeyStat, metric: ModelMetricKey, t: (key: string) => string, full: boolean): string {
  const requests = `${formatNumber(stat.requests)} ${t('accounts.usageReqUnit')}`
  const tokens = `${formatTokens(stat.tokens, full)} ${t('accounts.usageTokUnit')}`
  const cost = `$${formatCost(stat.account_billed)}`
  switch (metric) {
    case 'tokens':
      return `${requests} · ${cost}`
    case 'cost':
      return `${requests} · ${tokens}`
    default:
      return `${tokens} · ${cost}`
  }
}

function formatNumber(value: number): string {
  return Math.round(Number(value || 0)).toLocaleString()
}

// Number formatting all goes through lib/usageFormat: it honors the "show full usage numbers" setting, and the compact units
// extend to B/T (the local implementation used to stop carrying at M, so billions of tokens showed as 6609M).
function formatCompactNumber(value: number, full: boolean): string {
  return formatUsageNumber(Number(value || 0), full)
}

function formatTokens(value: number, full: boolean): string {
  return formatCompactNumber(value, full)
}

function formatCost(value: number): string {
  const n = Number(value || 0)
  return n >= 1 ? n.toFixed(2) : n.toFixed(4)
}

function formatDuration(value: number): string {
  const n = Number(value || 0)
  if (n <= 0) return '0ms'
  if (n >= 1000) return `${(n / 1000).toFixed(2)}s`
  return `${Math.round(n)}ms`
}

function formatDurationOrDash(value: number): string {
  const n = Number(value || 0)
  return n > 0 ? formatDuration(n) : '-'
}

function formatPercent(value: number): string {
  const n = Number(value || 0)
  return `${n.toFixed(n >= 10 ? 1 : 2)}%`
}
