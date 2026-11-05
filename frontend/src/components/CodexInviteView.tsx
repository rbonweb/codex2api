import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getTimezone } from '../utils/time'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import {
  AlertTriangle,
  ArrowLeft,
  Check,
  ChevronDown,
  Clock,
  Copy,
  Gift,
  History,
  Loader2,
  Mail,
  RefreshCw,
  Send,
  Sparkles,
  UserCircle2,
  Users,
  X,
} from 'lucide-react'
import PageHeader from './PageHeader'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '../api'
import type {
  AccountRow,
  InviteCacheMeta,
  InviteEligibility,
  InviteGuideAccountPlan,
  InviteRecipientRecord,
  InviteResult,
  InviteTrackingItem,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'
import {
  alreadyInvitedEmails,
  inviteRecipientRecord,
  inviteRecipientCandidates,
  isCodexInviteSenderCandidate,
  mergeInviteRecipientIndex,
  normalizeInviteEmail,
  normalizeInviteEmails,
  type InviteRecipientIndex,
} from '../lib/inviteAccountSelection'

interface Props {
  accounts: AccountRow[]
  onClose: () => void
  // loading distinguishes "accounts not fetched yet" from "really no usable accounts". Deep-linking straight into this page (/accounts/invite)
  // while the account list is empty is a normal loading state; rendering it as empty would falsely report "no accounts available to invite".
  loading?: boolean
}

const MAX_EMAILS = 10
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const SPLIT_RE = /[,;\r\n\t ]+/

interface ParsedEmails {
  valid: string[]
  invalid: string[]
  duplicates: number
}

// Kept consistent with the backend collectInviteEmails: split on separators, de-duplicate (case-insensitive), validate with a regex.
function parseEmails(text: string): ParsedEmails {
  const tokens = text.split(SPLIT_RE).map((s) => s.trim()).filter(Boolean)
  const seen = new Set<string>()
  const valid: string[] = []
  const invalid: string[] = []
  let duplicates = 0
  for (const tk of tokens) {
    if (!EMAIL_RE.test(tk)) {
      invalid.push(tk)
      continue
    }
    const key = tk.toLowerCase()
    if (seen.has(key)) {
      duplicates++
      continue
    }
    seen.add(key)
    valid.push(tk)
  }
  return { valid, invalid, duplicates }
}

function accountDisplayName(account: AccountRow): string {
  return account.email || account.name || `#${account.id}`
}

function accountSearchText(account: AccountRow): string {
  return [
    String(account.id),
    `#${account.id}`,
    account.email,
    account.name,
    account.status,
    account.plan_type,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase()
}

// Status dot colors, kept consistent with the global StatusBadge.
const STATUS_DOT_COLOR: Record<string, string> = {
  active: 'bg-emerald-500',
  ready: 'bg-emerald-500',
  cooldown: 'bg-amber-500',
  rate_limited: 'bg-yellow-500',
  usage_exhausted: 'bg-yellow-500',
  quota_paused: 'bg-yellow-500',
  unauthorized: 'bg-red-500',
  error: 'bg-red-400',
  refreshing: 'bg-blue-500 animate-pulse',
  paused: 'bg-blue-500',
}

function statusDotColor(status?: string | null): string {
  return STATUS_DOT_COLOR[(status || '').toLowerCase()] ?? 'bg-gray-400'
}

// Colors for invite record status. Observed upstream status values are redeemed and pending; everything else uses the fallback style.
const INVITE_STATUS_TONE: Record<string, string> = {
  redeemed: 'bg-emerald-500/10 text-emerald-600',
  accepted: 'bg-emerald-500/10 text-emerald-600',
  pending: 'bg-amber-500/10 text-amber-600',
  sent: 'bg-amber-500/10 text-amber-600',
  expired: 'bg-muted text-muted-foreground',
  revoked: 'bg-red-500/10 text-red-600',
}

function inviteStatusTone(status?: string): string {
  return INVITE_STATUS_TONE[(status || '').toLowerCase()] ?? 'bg-muted text-muted-foreground'
}

// The upstream returns an ISO time string; when parsing fails, show it as is rather than hiding the information.
function formatInviteTime(value?: string): string {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

// Takes the rule for the given dimension from time_frame_rules (send = send count, reward = reward count).
function findCapacityRule(eligibility: InviteEligibility | null, capacityType: string) {
  return eligibility?.time_frame_rules?.find((rule) => rule.capacity_type === capacityType) ?? null
}

// The lock protects the account from automatic cleanup and does not mean the invite credential has expired; keep the hint so users can tell them apart.
function accountAbnormalKey(account: AccountRow): 'locked' | null {
  if (account.locked) return 'locked'
  return null
}

function resolveAccountInput(accounts: AccountRow[], input: string): AccountRow | null {
  const normalized = input.trim().toLowerCase()
  if (!normalized) return null
  return accounts.find((account) => {
    const id = String(account.id)
    const name = account.name?.trim().toLowerCase()
    const normalizedNameWithID = normalized.replace(/\s+#(?=\d+$)/, '#')
    return (
      normalized === id ||
      normalized === `#${id}` ||
      normalized === account.email?.trim().toLowerCase() ||
      normalized === name ||
      (Boolean(name) && normalizedNameWithID === `${name}#${id}`)
    )
  }) ?? null
}

// CodexInviteView is the "Codex invites" view inside the account management page; its entry point matches the recycle bin.
export default function CodexInviteView({ accounts, onClose, loading = false }: Props) {
  const { t } = useTranslation()
  const { showToast } = useToast()

  // When the guide modal jumps here it carries ?account=<email>. Email rather than ID: the picker is a server-side paginated
  // search, and an email as the query brings the target account into the candidate set; with an ID, if the account is not in the first 100 results,
  // the effect below that clears the selection when the account is not in the candidate set would wipe it out.
  const [searchParams] = useSearchParams()
  const presetAccount = (searchParams.get('account') ?? '').trim()

  const [pickerAccounts, setPickerAccounts] = useState<AccountRow[]>(accounts)
  const [pickerLoading, setPickerLoading] = useState(loading)

  // Only keep Codex OAuth accounts that are in a normal state and usable for referrals; the picker is driven by server-side paginated search.
  const codexAccounts = useMemo(
    () => pickerAccounts.filter(isCodexInviteSenderCandidate),
    [pickerAccounts],
  )
  const firstAccount = codexAccounts[0] ?? null

  const [accountId, setAccountId] = useState<number | null>(firstAccount?.id ?? null)
  const [accountQuery, setAccountQuery] = useState(() =>
    presetAccount || (firstAccount ? accountDisplayName(firstAccount) : ''))
  const [accountOpen, setAccountOpen] = useState(false)
  // accountTyping tells apart "the user is typing a search" from "the input only echoes the selected account". Filter by text only while typing,
  // otherwise opening the dropdown should show all accounts (otherwise the selected account's email would filter it down to one entry).
  const [accountTyping, setAccountTyping] = useState(Boolean(presetAccount))
  // Index of the highlighted item for keyboard navigation in the dropdown (points into filteredAccounts). -1 means no item is highlighted.
  const [activeIndex, setActiveIndex] = useState(-1)
  const [emailsText, setEmailsText] = useState('')
  const [inviteRecipientIndex, setInviteRecipientIndex] = useState<InviteRecipientIndex>({})
  const [checkedRecipientSignature, setCheckedRecipientSignature] = useState('')
  const [recipientCheckPending, setRecipientCheckPending] = useState(false)
  const [recipientCheckError, setRecipientCheckError] = useState<string | null>(null)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [proxyUrl, setProxyUrl] = useState('')
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<InviteResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [eligibility, setEligibility] = useState<InviteEligibility | null>(null)
  const [tracking, setTracking] = useState<InviteTrackingItem[] | null>(null)
  const [infoLoading, setInfoLoading] = useState(false)
  const [eligibilityError, setEligibilityError] = useState<string | null>(null)
  const [trackingError, setTrackingError] = useState<string | null>(null)
  // The freshness of the data as reported by the gateway. source=upstream means it was just fetched; anything else comes from cache, so the observed time
  // must be shown, otherwise the remaining count the user sees may be from a dozen or so minutes ago with no hint.
  const [eligibilityMeta, setEligibilityMeta] = useState<InviteCacheMeta | null>(null)
  const [trackingMeta, setTrackingMeta] = useState<InviteCacheMeta | null>(null)
  // The invite credits for each account in the dropdown. Read-only from the gateway cache; never probe upstream on expand. One dropdown can show up to
  // 100 accounts, so auto-probing would fire 100 requests at Cloudflare-protected endpoints just by opening it. Accounts without cache show no badge,
  // and the user triggers it explicitly by clicking "Probe credits".
  const [creditsMap, setCreditsMap] = useState<Record<number, InviteGuideAccountPlan>>({})
  const [creditsProbing, setCreditsProbing] = useState(false)
  const accountPickerRef = useRef<HTMLDivElement>(null)
  // The proxy is held in a ref: it is only an optional parameter for queries, and changing it on every keystroke should not re-fetch from upstream.
  const proxyUrlRef = useRef(proxyUrl)
  // An incrementing sequence number discards stale responses (when switching accounts quickly, late old responses must not overwrite the new account's data).
  const infoSeqRef = useRef(0)

  useEffect(() => {
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      setPickerLoading(true)
      void api.getAccountsPage({
        channel: 'codex',
        page: 1,
        pageSize: 100,
        search: accountTyping ? accountQuery.trim() : undefined,
      }, controller.signal)
        .then((response) => {
          if (!controller.signal.aborted) setPickerAccounts(response.accounts ?? [])
        })
        .catch(() => undefined)
        .finally(() => {
          if (!controller.signal.aborted) setPickerLoading(false)
        })
    }, accountTyping ? 250 : 0)
    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [accountQuery, accountTyping])

  const parsed = useMemo(() => parseEmails(emailsText), [emailsText])
  const inputRecipientEmails = useMemo(() => normalizeInviteEmails(parsed.valid), [parsed.valid])
  const inputRecipientSignature = inputRecipientEmails.join('\n')
  const mergeInviteRecipients = useCallback((recipients: InviteRecipientRecord[]) => {
    setInviteRecipientIndex((current) => mergeInviteRecipientIndex(current, recipients))
  }, [])

  // Manually pasted emails must also go through the persisted-record check; they cannot rely only on the disabled state of the dropdown row.
  // The signature makes any input change immediately drop the "checked" state, preventing a click on send within the debounce window.
  useEffect(() => {
    if (!inputRecipientSignature) {
      setCheckedRecipientSignature('')
      setRecipientCheckPending(false)
      setRecipientCheckError(null)
      return
    }

    const controller = new AbortController()
    setRecipientCheckPending(true)
    setRecipientCheckError(null)
    const timer = window.setTimeout(() => {
      void api.checkInviteRecipients(inputRecipientEmails, controller.signal)
        .then((response) => {
          if (controller.signal.aborted) return
          mergeInviteRecipients(response.recipients ?? [])
          setCheckedRecipientSignature(inputRecipientSignature)
        })
        .catch((err) => {
          if (!controller.signal.aborted) setRecipientCheckError(getErrorMessage(err))
        })
        .finally(() => {
          if (!controller.signal.aborted) setRecipientCheckPending(false)
        })
    }, 250)

    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [inputRecipientEmails, inputRecipientSignature, mergeInviteRecipients])

  const duplicateInviteEmails = useMemo(
    () => alreadyInvitedEmails(parsed.valid, inviteRecipientIndex),
    [parsed.valid, inviteRecipientIndex],
  )
  const recipientsChecked =
    inputRecipientSignature === '' || checkedRecipientSignature === inputRecipientSignature
  // The recipient picker's checked state is derived directly from the input text (single source of truth): manually entered emails
  // show as selected in the dropdown automatically; checking or unchecking adds or removes them from the input, so the two entry points never keep separate copies.
  const selectedRecipientEmails = useMemo(() => {
    const set = new Set<string>()
    for (const token of emailsText.split(SPLIT_RE)) {
      const value = token.trim()
      if (value) set.add(value.toLowerCase())
    }
    return set
  }, [emailsText])
  const toggleRecipientEmail = useCallback((email: string) => {
    const key = email.trim().toLowerCase()
    setEmailsText((prev) => {
      const tokens = prev.split(SPLIT_RE).map((token) => token.trim()).filter(Boolean)
      if (tokens.some((token) => token.toLowerCase() === key)) {
        return tokens.filter((token) => token.toLowerCase() !== key).join('\n')
      }
      return tokens.length > 0 ? `${tokens.join('\n')}\n${email.trim()}` : email.trim()
    })
  }, [])
  const selectedAccount = useMemo(
    () => codexAccounts.find((a) => a.id === accountId) ?? null,
    [codexAccounts, accountId],
  )
  const filteredAccounts = useMemo(() => {
    // Show all when not typing a search; filter by text only while typing.
    if (!accountTyping) return codexAccounts
    const query = accountQuery.trim().toLowerCase()
    if (!query) return codexAccounts
    return codexAccounts.filter((account) => accountSearchText(account).includes(query))
  }, [accountTyping, accountQuery, codexAccounts])
  const overLimit = parsed.valid.length > MAX_EMAILS
  // Quota is only treated as exhausted when upstream explicitly returns 0; when the field is missing (undefined), nothing is blocked.
  const sendCapacity = eligibility?.remaining_send_capacity
  const rewardCapacity = eligibility?.remaining_reward_capacity
  const sendCapacityExhausted = sendCapacity === 0
  const rewardCapacityExhausted = rewardCapacity === 0
  const overCapacity = typeof sendCapacity === 'number' && sendCapacity > 0 && parsed.valid.length > sendCapacity
  // should_show=false is a hard "not eligible" verdict from upstream; block it directly to save a request that is certain to return 4xx.
  const ineligible = eligibility != null && eligibility.ok && !eligibility.should_show
  const canSend =
    !sending &&
    accountQuery.trim() !== '' &&
    parsed.valid.length > 0 &&
    parsed.invalid.length === 0 &&
    !overLimit &&
    recipientsChecked &&
    !recipientCheckError &&
    duplicateInviteEmails.length === 0 &&
    !sendCapacityExhausted &&
    !ineligible
  // A locked account can still be invited, but its protection status hint is kept.
  const selectedAbnormal = useMemo(
    () => (selectedAccount ? accountAbnormalKey(selectedAccount) : null),
    [selectedAccount],
  )

  // Shared selection logic: used by both dropdown clicks and Enter key presses.
  const selectAccount = (account: AccountRow) => {
    setAccountId(account.id)
    setAccountQuery(accountDisplayName(account))
    setAccountOpen(false)
    setAccountTyping(false)
    setActiveIndex(-1)
    setError(null)
  }

  // When the dropdown opens or the filtered results change, reset the highlight to the current selection (no highlight if there is none).
  useEffect(() => {
    if (!accountOpen) {
      setActiveIndex(-1)
      return
    }
    setActiveIndex((prev) => {
      if (prev >= 0 && prev < filteredAccounts.length) return prev
      const selectedIdx = filteredAccounts.findIndex((a) => a.id === accountId)
      return selectedIdx >= 0 ? selectedIdx : filteredAccounts.length > 0 ? 0 : -1
    })
  }, [accountOpen, filteredAccounts, accountId])

  // Dropdown keyboard navigation: arrow keys move the highlight, Enter confirms, Esc closes.
  const handlePickerKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape') {
      if (accountOpen) {
        event.preventDefault()
        setAccountOpen(false)
      }
      return
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      if (!accountOpen) {
        setAccountOpen(true)
        setAccountTyping(false)
        return
      }
      if (filteredAccounts.length === 0) return
      setActiveIndex((prev) => {
        const delta = event.key === 'ArrowDown' ? 1 : -1
        const base = prev < 0 ? (delta === 1 ? -1 : 0) : prev
        return (base + delta + filteredAccounts.length) % filteredAccounts.length
      })
      return
    }
    if (event.key === 'Enter') {
      if (accountOpen && activeIndex >= 0 && activeIndex < filteredAccounts.length) {
        event.preventDefault()
        selectAccount(filteredAccounts[activeIndex])
      }
    }
  }

  // The URL-preset account is resolved only once: wait until server-side search brings it back into the candidate set, then select it by exact match.
  // Use a ref lock so this effect does not snap back to the preset value when the user later changes the selection manually.
  const presetAppliedRef = useRef(false)
  useEffect(() => {
    if (presetAppliedRef.current || !presetAccount || accountId != null) return
    const matched = resolveAccountInput(codexAccounts, presetAccount)
    if (!matched) return
    presetAppliedRef.current = true
    setAccountId(matched.id)
    setAccountQuery(accountDisplayName(matched))
    setAccountTyping(false)
  }, [presetAccount, codexAccounts, accountId])

  useEffect(() => {
    if (accountId == null) return
    if (codexAccounts.some((a) => a.id === accountId)) return
    setAccountId(null)
    setAccountQuery('')
  }, [accountId, codexAccounts])

  useEffect(() => {
    proxyUrlRef.current = proxyUrl
  }, [proxyUrl])

  // Fetch eligibility and sent invites. The two requests are sent serially rather than concurrently: the upstream endpoint sits behind Cloudflare bot management,
  // and the backend reuses the cookie per account, so the __cf_bm the first request receives means the second request gets challenged less often.
  // A failure in one does not affect displaying the other.
  // force=true makes the gateway bypass the eligibility/record cache and go straight to upstream. Manual refresh and re-fetch after sending an invite both need it,
  // otherwise a quota that was just consumed would keep showing the cached value.
  const loadInviteInfo = useCallback(async (id: number, force = false) => {
    const seq = ++infoSeqRef.current
    setInfoLoading(true)
    setEligibilityError(null)
    setTrackingError(null)
    const proxy = proxyUrlRef.current.trim() || undefined

    try {
      const res = await api.getInviteEligibility(id, { proxy_url: proxy, refresh: force })
      if (seq !== infoSeqRef.current) return
      setEligibility(res.result)
      setEligibilityMeta(res.cache ?? null)
    } catch (err) {
      if (seq !== infoSeqRef.current) return
      setEligibility(null)
      setEligibilityMeta(null)
      setEligibilityError(getErrorMessage(err))
    }

    try {
      const res = await api.getInviteTracking(id, { proxy_url: proxy, refresh: force })
      if (seq !== infoSeqRef.current) return
      setTracking(res.result.challenged ? null : res.result.items ?? [])
      setTrackingMeta(res.result.challenged ? null : res.cache ?? null)
      setTrackingError(res.result.challenged ? t('invite.challengedRetry') : null)
    } catch (err) {
      if (seq !== infoSeqRef.current) return
      setTracking(null)
      setTrackingMeta(null)
      setTrackingError(getErrorMessage(err))
    }

    if (seq !== infoSeqRef.current) return
    setInfoLoading(false)
  }, [t])

  // Re-fetch when switching accounts. Clearing must happen before the request starts: the sequence guard only prevents "late old responses
  // overwriting new data", and cannot prevent "while the new account is still in flight, the page still renders the previous account's quota and records" -
  // in that one or two seconds the account name has already changed while the remaining count is still the previous account's, and the user would act on the wrong number.
  useEffect(() => {
    infoSeqRef.current++
    setEligibility(null)
    setTracking(null)
    setEligibilityMeta(null)
    setTrackingMeta(null)
    setEligibilityError(null)
    setTrackingError(null)
    if (accountId == null) {
      setInfoLoading(false)
      return
    }
    void loadInviteInfo(accountId)
  }, [accountId, loadInviteInfo])

  // Merge rather than replace: when paging or changing the search term, keep the values already fetched so badges do not flicker and disappear.
  const loadVisibleCredits = useCallback(async (ids: number[]) => {
    if (ids.length === 0) return
    try {
      const plan = await api.getInviteGuidePlan(ids)
      setCreditsMap((prev) => {
        const next = { ...prev }
        for (const item of plan.accounts) next[item.id] = item
        return next
      })
    } catch {
      /* Credits are supplementary; if they cannot be fetched, do not show them and do not disrupt the main flow */
    }
  }, [])

  useEffect(() => {
    if (!accountOpen || filteredAccounts.length === 0) return
    const ids = filteredAccounts.map((a) => a.id)
    const timer = window.setTimeout(() => void loadVisibleCredits(ids), 300)
    return () => window.clearTimeout(timer)
  }, [accountOpen, filteredAccounts, loadVisibleCredits])

  // Only probe accounts that are currently visible and have no results yet. The backend caps at 50 and queues through the import gate,
  // so after probing here, poll a few times to read back the results, without waiting indefinitely.
  const probeCreditsByIds = async (candidateIds: number[]) => {
    const ids = candidateIds.filter((id) => !creditsMap[id] || creditsMap[id].state === 'pending')
    if (ids.length === 0 || creditsProbing) return
    setCreditsProbing(true)
    try {
      await api.probeInviteGuidePlan(ids)
      for (let i = 0; i < 5; i++) {
        await new Promise((resolve) => window.setTimeout(resolve, 2500))
        await loadVisibleCredits(ids)
      }
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setCreditsProbing(false)
    }
  }
  const probeVisibleCredits = () => probeCreditsByIds(filteredAccounts.map((a) => a.id))

  useEffect(() => {
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && accountPickerRef.current?.contains(target)) return
      setAccountOpen(false)
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [])

  const handleSend = async () => {
    const accountInput = accountQuery.trim()
    if (!accountInput) {
      setError(t('invite.noAccountSelected'))
      return
    }
    const account = selectedAccount ?? resolveAccountInput(codexAccounts, accountInput)
    if (!account) {
      setError(t('invite.accountNotFound'))
      showToast(t('invite.accountNotFound'), 'error')
      return
    }
    if (parsed.valid.length === 0) {
      setError(t('invite.noValidEmails'))
      return
    }
    setAccountId(account.id)
    setAccountQuery(accountDisplayName(account))
    setSending(true)
    setError(null)
    setResult(null)
    try {
      const res = await api.sendInvite(account.id, {
        emails: parsed.valid,
        proxy_url: proxyUrl.trim() || undefined,
      })
      setResult(res.result)
      if (res.ok) {
        const invitedAt = new Date().toISOString()
        mergeInviteRecipients((res.recorded_emails ?? []).map((email) => ({
          email,
          state: 'sent',
          sender_account_id: account.id,
          invited_at: invitedAt,
        })))
        setEmailsText('')
        showToast(t('invite.sendSuccess'), 'success')
      } else {
        showToast(t('invite.sendUpstreamFailed', { code: res.result.status_code }), 'error')
      }
      // Refresh quota and records whether it succeeded or failed: success must decrement the remaining count, and failure may also mean the quota was used up elsewhere.
      // Force a fetch from upstream: the gateway already cleared its cache after sending, but add another fallback here to avoid any layer keeping stale data.
      void loadInviteInfo(account.id, true)
    } catch (err) {
      setError(getErrorMessage(err))
      showToast(t('invite.sendFailed', { error: getErrorMessage(err) }), 'error')
    } finally {
      setSending(false)
    }
  }

  return (
    <div>
      <PageHeader
        title={t('invite.title')}
        description={t('invite.description')}
        actions={
          <div className="flex flex-wrap items-center justify-end gap-1.5">
            <Button variant="outline" onClick={onClose} className="max-sm:w-full">
              <ArrowLeft className="size-3.5" />
              {t('invite.back')}
            </Button>
          </div>
        }
      />

      {/* Wide screens: three horizontal columns: account/eligibility | send form | invite records; narrow screens stack vertically */}
      <div className="mt-4 space-y-5">
        {codexAccounts.length === 0 ? (
          <div className="mx-auto max-w-2xl">
            <EmptyState message={pickerLoading ? t('invite.accountsLoading') : t('invite.noCodexAccounts')} spinning={pickerLoading} />
          </div>
        ) : (
          <div className="grid grid-cols-1 items-stretch gap-5 lg:grid-cols-2 xl:min-h-[calc(100dvh-12rem)] xl:grid-cols-12">
            {/* Left column: account selection + eligibility and quota; aligned to the same height as the middle and right columns */}
            <section className="flex min-h-0 min-w-0 flex-col gap-5 xl:col-span-4">
              <div className="shrink-0 rounded-2xl border bg-card shadow-sm">
                <div className="border-b px-5 py-4">
                  <div className="flex items-center gap-2">
                    <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                      <UserCircle2 className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <h3 className="text-sm font-semibold leading-tight">{t('invite.accountLabel')}</h3>
                      <p className="text-xs text-muted-foreground">{t('invite.accountHint')}</p>
                    </div>
                  </div>
                </div>
                <div className="p-5">
                  <div ref={accountPickerRef} className="relative">
                    <div className="relative">
                      <Input
                        value={accountQuery}
                        onFocus={() => { setAccountOpen(true); setAccountTyping(false) }}
                        onClick={() => { setAccountOpen(true); setAccountTyping(false) }}
                        onKeyDown={handlePickerKeyDown}
                        onChange={(e) => {
                          const next = e.target.value
                          setAccountQuery(next)
                          setAccountOpen(true)
                          setAccountTyping(true)
                          setAccountId(resolveAccountInput(codexAccounts, next)?.id ?? null)
                          if (error === t('invite.accountNotFound')) setError(null)
                        }}
                        placeholder={t('invite.accountPlaceholder')}
                        role="combobox"
                        autoComplete="off"
                        aria-expanded={accountOpen}
                        aria-controls="codex-invite-account-list"
                        aria-activedescendant={
                          accountOpen && activeIndex >= 0 && activeIndex < filteredAccounts.length
                            ? `codex-invite-option-${filteredAccounts[activeIndex].id}`
                            : undefined
                        }
                        className="h-10 pr-9"
                      />
                      <button
                        type="button"
                        onClick={() => { setAccountOpen((open) => !open); setAccountTyping(false) }}
                        className="absolute inset-y-0 right-0 inline-flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                        aria-label={t('invite.accountToggle')}
                      >
                        <ChevronDown className={`size-4 transition-transform ${accountOpen ? 'rotate-180' : ''}`} />
                      </button>
                    </div>
                    {accountOpen && (
                      <div className="absolute z-30 mt-1.5 w-full overflow-hidden rounded-lg border bg-popover text-popover-foreground shadow-lg">
                      <div
                        id="codex-invite-account-list"
                        role="listbox"
                        className="max-h-[min(60dvh,30rem)] overflow-auto p-1"
                      >
                        {filteredAccounts.length > 0 ? (
                          filteredAccounts.map((account, index) => {
                            const active = account.id === accountId
                            const highlighted = index === activeIndex
                            const abnormal = accountAbnormalKey(account)
                            return (
                              <button
                                key={account.id}
                                id={`codex-invite-option-${account.id}`}
                                type="button"
                                role="option"
                                aria-selected={active}
                                ref={highlighted ? (el) => el?.scrollIntoView({ block: 'nearest' }) : undefined}
                                onMouseDown={(event) => event.preventDefault()}
                                onMouseEnter={() => setActiveIndex(index)}
                                onClick={() => selectAccount(account)}
                                className={`flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm transition-colors ${
                                  highlighted ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/70 hover:text-accent-foreground'
                                }`}
                              >
                                <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-muted text-[11px] font-semibold text-muted-foreground">
                                  #{account.id}
                                </span>
                                <span className="min-w-0 flex-1">
                                  <span className="flex items-center gap-1.5">
                                    <span className={`size-1.5 shrink-0 rounded-full ${statusDotColor(account.status)}`} />
                                    <span className="truncate font-medium">{accountDisplayName(account)}</span>
                                    {abnormal && (
                                      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
                                        {t(`invite.state.${abnormal}`)}
                                      </span>
                                    )}
                                  </span>
                                  <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                                    <span className="truncate">
                                      {[account.name && account.name !== account.email ? account.name : '', account.plan_type, account.status]
                                        .filter(Boolean)
                                        .join(' · ') || '-'}
                                    </span>
                                    <InviteStatsInline plan={creditsMap[account.id]} />
                                  </span>
                                </span>
                                <InviteCreditsBadge plan={creditsMap[account.id]} />
                                {active && <Check className="size-4 shrink-0 text-primary" />}
                              </button>
                            )
                          })
                        ) : (
                          <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                            {t('invite.noAccountMatches')}
                          </div>
                        )}
                      </div>
                      {filteredAccounts.length > 0 && (
                        <div className="flex items-center justify-between gap-2 border-t bg-muted/30 px-2.5 py-1.5 text-[11px] text-muted-foreground">
                          <span>{t('invite.creditsHint')}</span>
                          <button
                            type="button"
                            // preventDefault keeps focus in the input; otherwise a single click would close the dropdown.
                            onMouseDown={(event) => event.preventDefault()}
                            onClick={() => void probeVisibleCredits()}
                            disabled={creditsProbing}
                            className="inline-flex shrink-0 items-center gap-1 rounded-md border bg-background px-2 py-0.5 font-medium transition-colors hover:text-foreground disabled:opacity-50"
                          >
                            {creditsProbing && <Loader2 className="size-3 animate-spin" />}
                            {creditsProbing ? t('invite.creditsProbing') : t('invite.creditsProbe')}
                          </button>
                        </div>
                      )}
                      </div>
                    )}
                  </div>
                  {selectedAccount && (
                    <div className="mt-3 flex flex-wrap items-center gap-1.5">
                      {selectedAccount.plan_type && (
                        <InfoPill label={t('invite.planLabel')} value={selectedAccount.plan_type} />
                      )}
                      <InfoPill label={t('invite.statusLabel')} value={selectedAccount.status || '-'} />
                    </div>
                  )}
                  {selectedAbnormal && (
                    <p className="mt-2 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.abnormalHint', { state: t(`invite.state.${selectedAbnormal}`) })}</span>
                    </p>
                  )}
                </div>
              </div>

              {accountId != null ? (
                <EligibilityPanel
                  eligibility={eligibility}
                  loading={infoLoading}
                  error={eligibilityError}
                  meta={eligibilityMeta}
                  onRefresh={() => void loadInviteInfo(accountId, true)}
                  className="min-h-0 flex-1"
                />
              ) : (
                <div className="min-h-0 flex-1 rounded-2xl border border-dashed bg-card/60 shadow-sm" aria-hidden />
              )}
            </section>

            {/* Middle column: email input and send */}
            <section className="flex min-h-0 min-w-0 flex-col xl:col-span-4">
              <div className="flex h-full min-h-[22rem] flex-col rounded-2xl border bg-card shadow-sm">
                <div className="shrink-0 border-b px-5 py-4">
                  <div className="flex items-center gap-2">
                    <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                      <Mail className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <h3 className="text-sm font-semibold leading-tight">{t('invite.emailsLabel')}</h3>
                      <p className="text-xs text-muted-foreground">{t('invite.emailsHint')}</p>
                    </div>
                  </div>
                </div>
                <div className="flex min-h-0 flex-1 flex-col p-5">
                  <div className="mb-2 flex items-start gap-2">
                    <div className="min-w-0 flex-1">
                      <RecipientAccountPicker
                        selectedEmails={selectedRecipientEmails}
                        onToggle={toggleRecipientEmail}
                        excludeEmail={selectedAccount?.email}
                        inviteRecipientIndex={inviteRecipientIndex}
                        onRecipientsChecked={mergeInviteRecipients}
                        creditsMap={creditsMap}
                        onLoadCredits={loadVisibleCredits}
                        onProbeCredits={probeCreditsByIds}
                        probing={creditsProbing}
                      />
                    </div>
                    {emailsText.trim() !== '' && (
                      <button
                        type="button"
                        onClick={() => setEmailsText('')}
                        title={t('invite.clearEmails')}
                        className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
                      >
                        <X className="size-3.5" />
                        {t('invite.clearEmails')}
                      </button>
                    )}
                  </div>
                  <textarea
                    value={emailsText}
                    onChange={(e) => setEmailsText(e.target.value)}
                    rows={6}
                    placeholder={t('invite.emailsPlaceholder')}
                    className="min-h-[8rem] w-full flex-1 resize-none rounded-lg border bg-background px-3 py-2 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
                  />

                  {(parsed.valid.length > 0 || parsed.invalid.length > 0 || parsed.duplicates > 0) && (
                    <div className="mt-3 flex flex-wrap items-center gap-1.5">
                      <CountPill tone="success" text={t('invite.parsedValid', { count: parsed.valid.length })} />
                      {parsed.duplicates > 0 && (
                        <CountPill tone="muted" text={t('invite.parsedDuplicate', { count: parsed.duplicates })} />
                      )}
                      {parsed.invalid.length > 0 && (
                        <CountPill tone="danger" text={t('invite.parsedInvalid', { count: parsed.invalid.length })} />
                      )}
                    </div>
                  )}
                  {parsed.invalid.length > 0 && (
                    <p className="mt-1.5 break-all text-xs text-red-500">
                      {t('invite.invalidList')} {parsed.invalid.join(', ')}
                    </p>
                  )}
                  {recipientCheckPending && parsed.valid.length > 0 && (
                    <p className="mt-1.5 flex items-center gap-1.5 text-xs text-muted-foreground">
                      <Loader2 className="size-3.5 animate-spin" />
                      <span>{t('invite.recipientChecking')}</span>
                    </p>
                  )}
                  {recipientCheckError && parsed.valid.length > 0 && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.recipientCheckFailed', { error: recipientCheckError })}</span>
                    </p>
                  )}
                  {duplicateInviteEmails.length > 0 && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span className="break-all">
                        {t('invite.alreadyInvitedEmails', { emails: duplicateInviteEmails.join(', ') })}
                      </span>
                    </p>
                  )}
                  {overLimit && (
                    <p className="mt-1.5 flex items-center gap-1 text-xs text-amber-600">
                      <AlertTriangle className="size-3.5" />
                      {t('invite.overLimit', { max: MAX_EMAILS })}
                    </p>
                  )}

                  {/* Quota hint: 0 is a hard block; exceeding the remaining count only warns (quota is a monthly cumulative figure and local data may be stale). */}
                  {ineligible && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.blockedIneligible')}</span>
                    </p>
                  )}
                  {sendCapacityExhausted && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.blockedSendCapacity')}</span>
                    </p>
                  )}
                  {!sendCapacityExhausted && overCapacity && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.warnOverCapacity', { remaining: sendCapacity, count: parsed.valid.length })}</span>
                    </p>
                  )}
                  {!sendCapacityExhausted && rewardCapacityExhausted && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.warnRewardExhausted')}</span>
                    </p>
                  )}

                  {showAdvanced && (
                    <div className="mt-4 rounded-xl border bg-muted/30 p-3">
                      <label className="mb-1 block text-xs font-medium text-muted-foreground">
                        {t('invite.proxyLabel')}
                      </label>
                      <input
                        value={proxyUrl}
                        onChange={(e) => setProxyUrl(e.target.value)}
                        placeholder={t('invite.proxyPlaceholder')}
                        className="h-9 w-full rounded-lg border bg-background px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
                      />
                    </div>
                  )}

                  {error && <div className="mt-3 text-sm text-red-500">{error}</div>}

                  {/* Secondary action (advanced options) and the primary CTA share one row: toggle on the left, send on the right, removing the two stacked rows at the bottom */}
                  <div className="mt-auto flex items-center justify-between gap-3 pt-4">
                    <button
                      type="button"
                      onClick={() => setShowAdvanced((v) => !v)}
                      className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
                    >
                      <ChevronDown className={`size-3.5 transition-transform ${showAdvanced ? 'rotate-180' : ''}`} />
                      {t('invite.advanced')}
                    </button>
                    <Button disabled={!canSend} onClick={() => void handleSend()} className="min-w-[8.5rem]">
                      <Send className="size-3.5" />
                      {sending
                        ? t('invite.sending')
                        : parsed.valid.length > 0
                          ? t('invite.sendCount', { count: parsed.valid.length })
                          : t('invite.send')}
                    </Button>
                  </div>
                </div>
              </div>
            </section>

            {/* Right column: invite records; full-width row on medium screens, side by side with the left and right columns at equal height on wide screens */}
            <section className="flex min-h-0 min-w-0 flex-col lg:col-span-2 xl:col-span-4">
              {accountId != null ? (
                <TrackingCard
                  items={tracking}
                  loading={infoLoading}
                  error={trackingError}
                  meta={trackingMeta}
                  onRefresh={() => void loadInviteInfo(accountId, true)}
                  className="h-full min-h-[22rem]"
                />
              ) : (
                <div className="flex h-full min-h-[22rem] flex-col items-center justify-center rounded-2xl border border-dashed bg-card/60 px-6 py-12 text-center shadow-sm">
                  <div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-muted text-muted-foreground">
                    <History className="size-5" />
                  </div>
                  <p className="text-sm font-medium text-foreground">{t('invite.trackingTitle')}</p>
                  <p className="mt-1 text-xs text-muted-foreground">{t('invite.trackingSelectAccount')}</p>
                </div>
              )}
            </section>
          </div>
        )}

        {result && <InviteResultCard result={result} />}
      </div>
    </div>
  )
}

// EligibilityPanel shows the eligibility verdict from upstream, remaining quota, and the official copy.
// Copy is delivered by upstream according to Oai-Language (upstream i18n is currently incomplete and mixes Chinese and English); it is passed through as is with no second translation.
function EligibilityPanel({
  eligibility,
  loading,
  error,
  meta,
  onRefresh,
  className,
}: {
  eligibility: InviteEligibility | null
  loading: boolean
  error: string | null
  meta: InviteCacheMeta | null
  onRefresh: () => void
  className?: string
}) {
  const { t } = useTranslation()
  const sendRule = findCapacityRule(eligibility, 'send')
  const rewardRule = findCapacityRule(eligibility, 'reward')
  const shell = `flex flex-col rounded-2xl border bg-card shadow-sm ${className ?? ''}`

  if (loading && !eligibility) {
    return (
      <div className={`${shell} items-center justify-center p-5 text-sm text-muted-foreground`}>
        <Loader2 className="size-4 animate-spin" />
        <span className="mt-2">{t('invite.eligibilityLoading')}</span>
      </div>
    )
  }

  if (error) {
    return (
      <div className={`${shell} p-5 text-sm text-muted-foreground`}>
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <span className="min-w-0 flex-1 break-all">{t('invite.eligibilityLoadFailed', { error })}</span>
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>
    )
  }

  if (!eligibility) return null

  // Upstream non-2xx: expose the status code and do not pretend the account is eligible. Cloudflare challenges also return 403,
  // but that means "could not ask", not "not eligible", and must be stated separately, otherwise a normal account would be wrongly reported as lacking permission.
  if (!eligibility.ok) {
    return (
      <div className={`flex flex-col rounded-2xl border border-amber-500/30 bg-amber-500/5 p-5 text-sm text-amber-700 shadow-sm dark:text-amber-300 ${className ?? ''}`}>
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />
          <span className="min-w-0 flex-1">
            {eligibility.challenged
              ? t('invite.challengedRetry')
              : eligibility.upstream_message ||
                t('invite.eligibilityUpstreamFailed', { code: eligibility.status_code })}
          </span>
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>
    )
  }

  return (
    <div className={shell}>
      <div className="flex shrink-0 items-start gap-3 border-b px-5 py-4">
        <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <Gift className="size-4" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-sm font-semibold">{eligibility.title || t('invite.eligibilityTitle')}</span>
            {eligibility.offer_id && (
              <span className="rounded-full bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                {eligibility.offer_id}
              </span>
            )}
          </div>
          {eligibility.description && (
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{eligibility.description}</p>
          )}
        </div>
        {loading ? (
          <Loader2 className="mt-0.5 size-3.5 shrink-0 animate-spin text-muted-foreground" />
        ) : (
          <div className="flex shrink-0 items-center gap-1.5">
            <FreshnessHint meta={meta} />
            <RefreshButton onClick={onRefresh} />
          </div>
        )}
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-3 p-5">
        {!eligibility.should_show && (
          <p className="flex items-start gap-1.5 text-xs text-amber-600">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>
              {eligibility.ineligible_reason ||
                t('invite.ineligibleNoReason', { code: eligibility.ineligible_reason_code || '-' })}
            </span>
          </p>
        )}

        {/* The two kinds of quota are shown separately: how many emails can be sent is not the same as how many rewards can be claimed; only the latter determines whether there is any benefit left. */}
        <div className="flex flex-wrap items-center gap-1.5">
          <CapacityPill
            label={t('invite.remainingSend')}
            remaining={eligibility.remaining_send_capacity}
            used={sendRule?.invites_sent}
            total={sendRule?.invites_total}
          />
          <CapacityPill
            label={t('invite.remainingReward')}
            remaining={eligibility.remaining_reward_capacity}
            used={rewardRule?.invites_sent}
            total={rewardRule?.invites_total}
            highlight
          />
        </div>

        {eligibility.rules && eligibility.rules.length > 0 && (
          <ul className="space-y-1.5 rounded-xl border bg-muted/20 px-3 py-2.5 text-xs text-muted-foreground">
            {eligibility.rules.map((rule, i) => (
              <li key={i} className="flex items-start gap-1.5">
                <span className="mt-1.5 size-1 shrink-0 rounded-full bg-muted-foreground/50" />
                <span className="leading-relaxed">{rule}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

// CapacityPill shows the remaining quota for a single dimension. remaining being undefined means upstream did not provide this field,
// and it displays as "unknown" rather than 0, since the two are handled completely differently.
function CapacityPill({
  label,
  remaining,
  used,
  total,
  highlight,
}: {
  label: string
  remaining?: number
  used?: number
  total?: number
  highlight?: boolean
}) {
  const { t } = useTranslation()
  const unknown = typeof remaining !== 'number'
  const exhausted = remaining === 0
  const cls = unknown
    ? 'bg-muted text-muted-foreground'
    : exhausted
      ? 'bg-red-500/10 text-red-600'
      : highlight
        ? 'bg-primary/10 text-primary'
        : 'bg-emerald-500/10 text-emerald-600'
  const detail =
    typeof used === 'number' && typeof total === 'number' ? ` (${used}/${total})` : ''
  return (
    <span className={`inline-flex items-center gap-1 rounded-full px-2.5 py-1 text-xs font-semibold ${cls}`}>
      <span className="font-normal opacity-70">{label}</span>
      <span>{unknown ? t('invite.capacityUnknown') : `${remaining}${detail}`}</span>
    </span>
  )
}

// TrackingCard shows the invites this account has sent and their redemption status (upstream 90-day window).
function TrackingCard({
  items,
  loading,
  error,
  meta,
  onRefresh,
  className,
}: {
  items: InviteTrackingItem[] | null
  loading: boolean
  error: string | null
  meta: InviteCacheMeta | null
  onRefresh: () => void
  className?: string
}) {
  const { t } = useTranslation()

  return (
    <div className={`flex flex-col rounded-2xl border bg-card shadow-sm ${className ?? ''}`}>
      <div className="flex items-center gap-2 border-b px-5 py-4">
        <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <History className="size-4" />
        </div>
        <div className="min-w-0 flex-1">
          <h4 className="text-sm font-semibold leading-tight">{t('invite.trackingTitle')}</h4>
          <p className="text-xs text-muted-foreground">{t('invite.trackingDescription')}</p>
        </div>
        <div className="flex items-center gap-1.5">
          {loading && <Loader2 className="size-3.5 animate-spin text-muted-foreground" />}
          {!loading && <FreshnessHint meta={meta} />}
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>

      <div className="flex min-h-0 flex-1 flex-col overflow-auto p-5">
        {error ? (
          <p className="flex items-start gap-1.5 break-all text-sm text-amber-600">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>{t('invite.trackingLoadFailed', { error })}</span>
          </p>
        ) : items == null ? (
          <div className="flex flex-1 flex-col items-center justify-center py-10 text-center">
            <Loader2 className="size-5 animate-spin text-muted-foreground" />
            <p className="mt-3 text-sm text-muted-foreground">{t('invite.trackingLoading')}</p>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-1 flex-col items-center justify-center py-10 text-center">
            <div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-muted text-muted-foreground">
              <History className="size-5" />
            </div>
            <p className="text-sm font-medium text-foreground">{t('invite.trackingEmpty')}</p>
            <p className="mt-1 max-w-56 text-xs leading-relaxed text-muted-foreground">
              {t('invite.trackingEmptyHint')}
            </p>
          </div>
        ) : (
          <div className="space-y-2">
            {items.map((item, i) => (
              <div
                key={item.referral_id || item.email || i}
                className="rounded-xl border bg-background px-3 py-2.5"
              >
                <div className="flex items-center gap-2">
                  <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
                    {item.email || '-'}
                  </span>
                  <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold ${inviteStatusTone(item.status)}`}>
                    {item.status || '-'}
                  </span>
                  {item.invite_url && <CopyButton text={item.invite_url} />}
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                  <span>{t('invite.trackingCreatedAt', { time: formatInviteTime(item.created_at) })}</span>
                  {item.expires_at && (
                    <span>{t('invite.trackingExpiresAt', { time: formatInviteTime(item.expires_at) })}</span>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

// FreshnessHint only appears when the data comes from cache. Data just fetched from upstream (source=upstream) shows no hint,
// since that is the default expectation and an extra "just now" tag would only be noise.
function FreshnessHint({ meta }: { meta: InviteCacheMeta | null }) {
  const { t } = useTranslation()
  if (!meta || meta.source === 'upstream' || !meta.observed_at) return null
  const observed = new Date(meta.observed_at)
  if (Number.isNaN(observed.getTime())) return null
  return (
    <span
      title={t('invite.cachedHint')}
      className="inline-flex shrink-0 items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-[10px] font-medium text-muted-foreground"
    >
      <Clock className="size-3" />
      {observed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', timeZone: getTimezone() })}
    </span>
  )
}

// InviteStatsInline appends small invite-related stats to the secondary line of the dropdown: how many credits a single invite earns, how many emails were sent,
// and how many were accepted. Fields without data are not rendered at all; using 0 as a placeholder would be read as "none were ever sent".
//
// "Sent" prefers the tracking records (last 90 days), where it shares a window with "accepted" so the two numbers are comparable;
// it falls back to the eligibility API's send usage for this month only when there is no tracking data, and labels it explicitly as "this month" so readers do not assume
// the two numbers come from the same counting basis.
function InviteStatsInline({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null

  const parts: string[] = []
  if (typeof plan.grant_amount === 'number' && plan.grant_amount > 0) {
    parts.push(t('invite.statPerInvite', { amount: Math.round(plan.grant_amount).toLocaleString() }))
  }
  // An account that is ineligible and has never sent an invite does not show "Sent 0 · Accepted 0": the row already has an "ineligible"
  // badge, and two extra zeros are just noise. Accounts that have sent invites still show the numbers, since that history is useful.
  const hideEmptyStats = plan.state === 'ineligible' && !plan.invites_sent
  if (typeof plan.invites_sent === 'number' && !hideEmptyStats) {
    parts.push(t('invite.statSent', { count: plan.invites_sent }))
    if (typeof plan.invites_accepted === 'number') {
      parts.push(t('invite.statAccepted', { count: plan.invites_accepted }))
    }
  } else if (typeof plan.monthly_sent === 'number' && !hideEmptyStats) {
    parts.push(
      typeof plan.monthly_send_total === 'number'
        ? t('invite.statMonthlySentOf', { sent: plan.monthly_sent, total: plan.monthly_send_total })
        : t('invite.statMonthlySent', { sent: plan.monthly_sent }),
    )
  }
  if (parts.length === 0) return null

  return <span className="shrink-0 whitespace-nowrap text-muted-foreground/80">· {parts.join(' · ')}</span>
}

// InviteCreditsBadge shows how many invite credits this account can still earn (single reward amount × remaining reward count).
// When it has not been probed (pending / no data), nothing is rendered: blank space is more honest than showing "0",
// which would be read as "this account has no credits left".
function InviteCreditsBadge({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null

  if (plan.state === 'ineligible') {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
        {t('invite.creditsIneligible')}
      </span>
    )
  }
  // Can still send but has used up this month's reward count: sending earns no credits, which is a different situation from "ineligible".
  if (plan.state === 'exhausted') {
    return (
      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600">
        {t('invite.creditsExhausted')}
      </span>
    )
  }
  return (
    <span
      title={t('invite.creditsTooltip', {
        amount: Math.round(plan.grant_amount ?? 0).toLocaleString(),
        count: plan.remaining_reward_capacity ?? 0,
      })}
      className="shrink-0 rounded-full bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-semibold tabular-nums text-emerald-600"
    >
      +{Math.round(plan.potential_credits).toLocaleString()}
    </span>
  )
}

// RecipientAccountPicker picks the invitee email from the account list, supporting single selection and continuous multi-select (checking does not close
// the dropdown). Candidates are driven by server-side paginated search, on the same data channel as the left-column account picker. Excludes disabled,
// error, and banned accounts; only shows accounts that have an email, de-duplicated by email (team workspaces can make the same login email appear
// across several account rows), and excludes the account that is initiating the invite, since inviting oneself is always rejected by upstream.
function RecipientAccountPicker({
  selectedEmails,
  onToggle,
  excludeEmail,
  inviteRecipientIndex,
  onRecipientsChecked,
  creditsMap,
  onLoadCredits,
  onProbeCredits,
  probing,
}: {
  selectedEmails: Set<string>
  onToggle: (email: string) => void
  excludeEmail?: string
  inviteRecipientIndex: InviteRecipientIndex
  onRecipientsChecked: (recipients: InviteRecipientRecord[]) => void
  // Shares the same credits data with the initiator dropdown: any account probed on either side is immediately visible on the other.
  creditsMap: Record<number, InviteGuideAccountPlan>
  onLoadCredits: (ids: number[]) => void
  onProbeCredits: (ids: number[]) => void
  probing: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [rows, setRows] = useState<AccountRow[]>([])
  const [loading, setLoading] = useState(false)
  const [checkedCandidateEmails, setCheckedCandidateEmails] = useState<Set<string>>(new Set())
  const [recipientCheckError, setRecipientCheckError] = useState<string | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)

  // Fetch only when expanded; no background requests after collapsing. Search input goes through a 250ms debounce,
  // the first expand fetches immediately; AbortController cancels in-flight requests so a slow response cannot overwrite results for a newer term.
  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      setLoading(true)
      void api.getAccountsPage({
        channel: 'codex',
        page: 1,
        pageSize: 100,
        search: query.trim() || undefined,
      }, controller.signal)
        .then((response) => {
          if (!controller.signal.aborted) setRows(response.accounts ?? [])
        })
        .catch(() => undefined)
        .finally(() => {
          if (!controller.signal.aborted) setLoading(false)
        })
    }, query ? 250 : 0)
    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [open, query])

  useEffect(() => {
    if (!open) return
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && containerRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [open])

  const candidates = useMemo(
    () => inviteRecipientCandidates(rows, excludeEmail),
    [rows, excludeEmail],
  )
  const candidateEmails = useMemo(
    () => normalizeInviteEmails(candidates.map((row) => row.email ?? '')),
    [candidates],
  )

  // Each page of dropdown candidates gets one batch query for invite records. The returned list only contains emails that already exist; while the request is
  // in flight, other emails are only selectable after this round completes, to avoid briefly allowing a duplicate invite before the state comes back.
  useEffect(() => {
    setCheckedCandidateEmails(new Set())
    setRecipientCheckError(null)
    if (!open || candidateEmails.length === 0) return

    const controller = new AbortController()
    void api.checkInviteRecipients(candidateEmails, controller.signal)
      .then((response) => {
        if (controller.signal.aborted) return
        onRecipientsChecked(response.recipients ?? [])
        setCheckedCandidateEmails(new Set(candidateEmails))
      })
      .catch((err) => {
        if (!controller.signal.aborted) setRecipientCheckError(getErrorMessage(err))
      })
    return () => controller.abort()
  }, [open, candidateEmails, onRecipientsChecked])

  const creditCandidates = useMemo(
    () => candidates.filter((row) => {
      const email = row.email ?? ''
      const key = normalizeInviteEmail(email)
      return checkedCandidateEmails.has(key) && !inviteRecipientRecord(inviteRecipientIndex, email)
    }),
    [candidates, checkedCandidateEmails, inviteRecipientIndex],
  )

  // When candidates are visible, read the credits back from the gateway cache (pure cache read, zero upstream), so that info like "250 per invite"
  // shows directly on the row: accounts with low per-invite earnings are better suited as invitees, which is easy to spot at a glance.
  useEffect(() => {
    if (!open || creditCandidates.length === 0) return
    const ids = creditCandidates.map((row) => row.id)
    const timer = window.setTimeout(() => onLoadCredits(ids), 300)
    return () => window.clearTimeout(timer)
  }, [open, creditCandidates, onLoadCredits])

  return (
    <div ref={containerRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        className="inline-flex items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
      >
        <Users className="size-3.5" />
        {t('invite.recipientPicker')}
        <ChevronDown className={`size-3.5 transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div className="absolute left-0 right-0 z-30 mt-1.5 overflow-hidden rounded-lg border bg-popover text-popover-foreground shadow-lg">
          <div className="flex items-center gap-2 border-b p-1.5">
            <Input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault()
                  setOpen(false)
                }
              }}
              placeholder={t('invite.recipientSearch')}
              className="h-8 text-sm"
            />
            {loading && <Loader2 className="mr-1 size-3.5 shrink-0 animate-spin text-muted-foreground" />}
          </div>
          <div role="listbox" aria-multiselectable="true" className="max-h-[min(52dvh,26rem)] overflow-auto p-1">
            {candidates.length > 0 ? (
              candidates.map((row) => {
                const email = row.email?.trim() ?? ''
                const key = normalizeInviteEmail(email)
                const checked = selectedEmails.has(key)
                const invited = inviteRecipientRecord(inviteRecipientIndex, email)
                const statusChecked = checkedCandidateEmails.has(key) || Boolean(invited)
                const disabled = Boolean(invited) || !statusChecked
                return (
                  <button
                    key={row.id}
                    type="button"
                    role="option"
                    aria-selected={checked}
                    aria-disabled={disabled}
                    disabled={disabled}
                    // preventDefault keeps input focus, so continuous checking does not trigger a blur outside the dropdown.
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => onToggle(email)}
                    className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors hover:bg-accent/70 hover:text-accent-foreground disabled:cursor-not-allowed disabled:opacity-60 disabled:hover:bg-transparent"
                  >
                    <span
                      className={`flex size-4 shrink-0 items-center justify-center rounded border transition-colors ${
                        checked ? 'border-primary bg-primary text-primary-foreground' : 'border-input'
                      }`}
                    >
                      {checked && <Check className="size-3" />}
                    </span>
                    <span className="min-w-0 flex-1 truncate">{email}</span>
                    {invited ? (
                      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:text-amber-300">
                        {t('invite.recipientInvited')}
                      </span>
                    ) : (
                      <RecipientCreditsHint plan={creditsMap[row.id]} />
                    )}
                    {row.plan_type && (
                      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                        {row.plan_type}
                      </span>
                    )}
                  </button>
                )
              })
            ) : (
              <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                {loading ? t('invite.recipientLoading') : t('invite.noAccountMatches')}
              </div>
            )}
          </div>
          <div className="flex items-center justify-between gap-2 border-t bg-muted/30 px-2.5 py-1.5 text-[11px] text-muted-foreground">
            <span className={`min-w-0 truncate ${recipientCheckError ? 'text-red-500' : ''}`}>
              {recipientCheckError
                ? t('invite.recipientCheckFailed', { error: recipientCheckError })
                : t('invite.recipientToggleHint')}
            </span>
            <button
              type="button"
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => onProbeCredits(creditCandidates.map((row) => row.id))}
              disabled={probing || creditCandidates.length === 0}
              className="inline-flex shrink-0 items-center gap-1 rounded-md border bg-background px-2 py-0.5 font-medium transition-colors hover:text-foreground disabled:opacity-50"
            >
              {probing && <Loader2 className="size-3 animate-spin" />}
              {probing ? t('invite.creditsProbing') : t('invite.creditsProbe')}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// RecipientCreditsHint marks, on a recipient candidate row, the per-invite earnings of that account when it acts as the initiator.
// This is the core basis for picking invitees: an account that only earns 250 per invite is a poor initiator, so sacrificing it as an invitee is optimal;
// an "ineligible" account cannot send invites itself, so using it as an invitee has no opportunity cost. Left blank if not probed, never fabricating 0.
function RecipientCreditsHint({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null
  if (plan.state === 'ineligible') {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
        {t('invite.creditsIneligible')}
      </span>
    )
  }
  if (plan.state === 'exhausted') {
    return (
      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600">
        {t('invite.creditsExhausted')}
      </span>
    )
  }
  if (typeof plan.grant_amount === 'number' && plan.grant_amount > 0) {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-muted-foreground">
        {t('invite.statPerInvite', { amount: Math.round(plan.grant_amount).toLocaleString() })}
      </span>
    )
  }
  return null
}

function RefreshButton({ onClick }: { onClick: () => void }) {
  const { t } = useTranslation()
  return (
    <button
      type="button"
      onClick={onClick}
      title={t('invite.refresh')}
      aria-label={t('invite.refresh')}
      className="inline-flex size-7 shrink-0 items-center justify-center rounded-lg border bg-background text-muted-foreground transition-colors hover:text-foreground"
    >
      <RefreshCw className="size-3.5" />
    </button>
  )
}

function InviteResultCard({ result }: { result: InviteResult }) {
  const { t } = useTranslation()
  const [showRaw, setShowRaw] = useState(false)
  const rawText =
    result.upstream != null
      ? JSON.stringify(result.upstream, null, 2)
      : result.upstream_raw || ''

  return (
    <div className="rounded-2xl border bg-card shadow-sm">
      <div className="flex items-center gap-2 border-b px-5 py-4">
        <div className={`flex size-8 items-center justify-center rounded-lg ${result.ok ? 'bg-emerald-500/10 text-emerald-600' : 'bg-red-500/10 text-red-600'}`}>
          {result.ok ? <Check className="size-4" /> : <AlertTriangle className="size-4" />}
        </div>
        <div className="min-w-0 flex-1">
          <h4 className="text-sm font-semibold leading-tight">{t('invite.resultTitle')}</h4>
          <p className="text-xs text-muted-foreground">
            {result.ok
              ? t('invite.resultOkDesc', { count: result.emails.length })
              : t('invite.resultFailed', { code: result.status_code })}
          </p>
        </div>
        {result.request_id && (
          <span className="hidden rounded-full bg-muted px-2.5 py-1 font-mono text-[11px] text-muted-foreground sm:inline">
            {result.request_id}
          </span>
        )}
      </div>

      <div className="space-y-3 p-5">
        {/* Blocked by a Cloudflare challenge: the status code is also 403, but no invite was sent and it is unrelated to eligibility; prompt to retry. */}
        {!result.ok && result.challenged && (
          <div className="flex items-start gap-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" />
            <span>{t('invite.challengedRetry')}</span>
          </div>
        )}

        {/* When upstream gives a specific reason, show it directly (e.g. "this person has already received a referral invite"), and list the rejected emails.
            Such failures are recipient-level; the account's eligibility is intact, so the ineligibility hint below must not be applied. */}
        {!result.ok && !result.challenged && result.upstream_message && (
          <div className="rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <div className="flex items-start gap-2">
              <AlertTriangle className="mt-0.5 size-4 shrink-0" />
              <span className="min-w-0 flex-1">{result.upstream_message}</span>
            </div>
            {result.failed_emails && result.failed_emails.length > 0 && (
              <p className="mt-1.5 break-all pl-6 text-xs">
                {t('invite.failedEmails')} {result.failed_emails.join(', ')}
              </p>
            )}
          </div>
        )}

        {/* Fallback hint for ineligibility: only use this guess when it is neither a challenge nor does upstream give a specific reason. */}
        {!result.ok && !result.challenged && !result.upstream_message && result.status_code === 403 && (
          <div className="flex items-start gap-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <Sparkles className="mt-0.5 size-4 shrink-0" />
            <span>{t('invite.eligibilityHint')}</span>
          </div>
        )}

        {/* Invite details */}
        {result.invites && result.invites.length > 0 && (
          <div className="space-y-2">
            {result.invites.map((inv, i) => (
              <div
                key={inv.referral_id || inv.email || i}
                className="flex items-center justify-between gap-3 rounded-xl border bg-background px-3 py-2.5"
              >
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium text-foreground">{inv.email || '-'}</div>
                  {inv.invite_url && (
                    <a
                      href={inv.invite_url}
                      target="_blank"
                      rel="noreferrer"
                      className="block truncate text-xs text-primary hover:underline"
                    >
                      {inv.invite_url}
                    </a>
                  )}
                </div>
                {inv.invite_url && <CopyButton text={inv.invite_url} />}
              </div>
            ))}
          </div>
        )}

        {/* Raw response (collapsed) */}
        {rawText && (
          <div>
            <button
              type="button"
              onClick={() => setShowRaw((v) => !v)}
              className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
            >
              <ChevronDown className={`size-3.5 transition-transform ${showRaw ? 'rotate-180' : ''}`} />
              {t('invite.rawResponse')}
            </button>
            {showRaw && (
              <pre className="mt-2 max-h-64 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs">
                {rawText}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function CopyButton({ text }: { text: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      /* Ignore clipboard permission errors */
    }
  }
  return (
    <button
      type="button"
      onClick={() => void handleCopy()}
      title={copied ? t('invite.copied') : t('invite.copy')}
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg border bg-background text-muted-foreground transition-colors hover:text-foreground"
    >
      {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
    </button>
  )
}

function EmptyState({ message, spinning }: { message: string; spinning?: boolean }) {
  return (
    <div className="flex flex-col items-center justify-center rounded-2xl border border-dashed bg-card py-16 text-center">
      <div className="mb-3 flex size-12 items-center justify-center rounded-2xl bg-muted text-muted-foreground">
        {spinning ? <Loader2 className="size-6 animate-spin" /> : <Mail className="size-6" />}
      </div>
      <p className="text-sm text-muted-foreground">{message}</p>
    </div>
  )
}

function InfoPill({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-muted/60 px-2.5 py-1 text-xs text-muted-foreground">
      <span className="text-muted-foreground/70">{label}</span>
      <span className="font-medium text-foreground">{value}</span>
    </span>
  )
}

function CountPill({ tone, text }: { tone: 'success' | 'danger' | 'muted'; text: string }) {
  const cls =
    tone === 'success'
      ? 'bg-emerald-500/10 text-emerald-600'
      : tone === 'danger'
        ? 'bg-red-500/10 text-red-600'
        : 'bg-muted text-muted-foreground'
  return <span className={`inline-flex items-center rounded-full px-2.5 py-1 text-xs font-semibold ${cls}`}>{text}</span>
}
