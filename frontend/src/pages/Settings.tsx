import type { ChangeEvent, FocusEvent, ReactNode } from 'react'
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState, setAdminKey } from '../api'
import { formatBeijingTime } from '../utils/time'
import DisplayTimezoneSelect from '../components/DisplayTimezoneSelect'
import PageHeader from '../components/PageHeader'
import CodexClientVersionsPanel from '../components/CodexClientVersionsPanel'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { CodexClientVersionSyncResult, CodexClientVersionTarget, CodexUserAgentCatalog, CodexUserAgentPreview, HealthResponse, ModelInfo, SiteBranding, SystemSettings } from '../types'
import { countPayloadRules, PAYLOAD_RULE_GROUPS } from './PayloadRules'
import { getErrorMessage } from '../utils/error'
import { buildWritableSettingsPayload } from '../lib/settingsPayload'
import { resolveByteLimitCommit } from '../lib/byteLimitCommit'
import {
  mergeConfirmedSettingsValues,
  mergeConfirmedSettingsWithVersions,
  resolveAutoSaveRollbackValues,
} from '../lib/settingsAutoSave'
import {
  buildContinuousRetryCatchAllPatch,
  buildContinuousRetryEnabledPatch,
  createContinuousRetrySaveQueue,
  parseContinuousRetryErrorCodes,
  parseContinuousRetryMaxDurationSeconds,
  parseContinuousRetryStatusCodes,
} from '../lib/continuousRetrySettings'
import {
  MIB,
  buildResponseCacheBudgetPatch,
  bytesToMiB,
  mergeResponseCacheGeneration,
  mibToBytes,
  validateResponseCacheBudget,
  type ResponseCacheBudgetMiB,
  type ResponseCacheBudgetValidationError,
} from '../lib/responseCacheMetrics'
import {
  DEFAULT_OUTPUT_GUARD_SETTINGS,
  KIB,
  OUTPUT_GUARD_KEYS,
  bytesToKiB,
  kibToBytes,
  omitOutputGuardFields,
  preserveConfirmedGuardFields,
  preserveOutputGuardFields,
  validateOutputGuardHierarchy,
  validateOutputGuardRanges,
} from '../lib/outputGuardSettings'
import { DEFAULT_SITE_LOGO, isBrandingVideo, sanitizeBrandingImage, sanitizeBrandingLogo, useBranding } from '../branding'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { cn } from '@/lib/utils'

import { Switch } from '@/components/ui/switch'
import {
  Sheet,
  SheetBody,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import {
  Activity,
  ArrowRight,
  ArrowUpRight,
  Braces,
  Brain,
  Check,
  ChevronDown,
  ChevronRight,
  CircleAlert,
  CircleHelp,
  Cloud,
  Database,
  ExternalLink,
  Eye,
  Fingerprint,
  Gauge,
  Globe,
  Image as ImageIcon,
  Layers,
  Link2,
  Loader2,
  Palette,
  Plus,
  RefreshCw,
  RotateCcw,
  Save,
  Search,
  Server,
  Shield,
  ShieldAlert,
  SlidersHorizontal,
  Terminal,
  Trash2,
  Timer,
  Upload,
  Users,
  Shuffle,
  Wifi,
  Wrench,
  X,
} from 'lucide-react'
import { Link, useLocation, useNavigate, useSearchParams } from 'react-router-dom'
import ChannelLogo from '../components/ChannelLogo'

type ModelPanelKey = 'registry' | 'codex' | 'reasoning'
const MODEL_PANEL_FIELDS = {
  codex: 'codex_model_mapping',
  reasoning: 'reasoning_effort_models',
} as const satisfies Partial<Record<ModelPanelKey, keyof SystemSettings>>
const MODEL_REGISTRY_PREVIEW_LIMIT = 6
const MODEL_RULE_PREVIEW_LIMIT = 3

type ModelMappingEntry = [string, string]
const EMPTY_MODEL_MAPPING_ENTRIES: ModelMappingEntry[] = []
type ReasoningEffortModelEntry = {
  model: string
  effort: string
}
type AutoSaveStatus = 'idle' | 'saving' | 'saved' | 'error'
type CodexUserAgentConfig = {
  raw_user_agent?: string
  client_name?: string
  client_version?: string
  os_name?: string
  os_version?: string
  arch?: string
  terminal?: string
  client_kind?: string
  app_name?: string
  app_version?: string
  mode?: string
  pool_mix?: Record<string, number>
}
type CodexUAKind = 'codex-tui' | 'codex-desktop' | 'codex-vscode' | 'codex-exec' | 'custom'
const CODEX_UA_KINDS: CodexUAKind[] = ['codex-tui', 'codex-desktop', 'codex-vscode', 'codex-exec', 'custom']
const CODEX_UA_POOL_KINDS: CodexUAKind[] = ['codex-desktop', 'codex-vscode', 'codex-tui', 'codex-exec']
const CODEX_CLIENT_SYNC_SOURCES = [
  { key: 'cli', label: 'Codex CLI' },
  { key: 'desktop_mac', label: 'Desktop macOS' },
  { key: 'desktop_windows', label: 'Desktop Windows' },
  { key: 'vscode', label: 'VS Code' },
] as const
type CodexClientSyncSource = (typeof CODEX_CLIENT_SYNC_SOURCES)[number]['key']

// The settings endpoint returns only the last successful pairing; targets whose sync failed this time overlay their status and error, so the panel can show the failure reason.
function overlayCodexClientSyncFailures(targets: CodexClientVersionTarget[], result: Record<CodexClientSyncSource, CodexClientVersionSyncResult>): CodexClientVersionTarget[] {
  const failures = new Map<string, CodexClientVersionTarget>()
  for (const source of CODEX_CLIENT_SYNC_SOURCES) {
    for (const target of result[source.key].targets ?? []) {
      if (target.error) failures.set(`${target.client_kind}/${target.target_platform}`, target)
    }
  }
  return targets.map((target) => {
    const failure = failures.get(`${target.client_kind}/${target.target_platform}`)
    return failure ? { ...target, status: failure.status, error: failure.error } : target
  })
}
const CODEX_UA_FALLBACK_POOL_MIX: Record<string, number> = { 'codex-desktop': 50, 'codex-vscode': 30, 'codex-tui': 20 }
const CODEX_UA_STRING_KEYS = ['raw_user_agent', 'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'terminal', 'client_kind', 'app_name', 'app_version', 'mode'] as const
// Same rules as the backend inferCodexClientKind: old configs with no shape specified are inferred from the client name.
const inferCodexUAKind = (clientName?: string): CodexUAKind => {
  const name = (clientName ?? '').trim().toLowerCase()
  if (!name || name === 'codex-tui') return 'codex-tui'
  if (name === 'codex desktop') return 'codex-desktop'
  if (name === 'codex_vscode') return 'codex-vscode'
  if (name === 'codex_exec') return 'codex-exec'
  return 'custom'
}

const EMPTY_REASONING_EFFORT_MODEL_ENTRIES: ReasoningEffortModelEntry[] = []
const REASONING_EFFORT_OPTIONS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra', 'max'].map((effort) => ({
  label: effort,
  value: effort,
}))
const AUTO_SAVE_STATUS_RESET_MS = 1800
const AUTO_SAVE_TOAST_MS = 2000
const DEFAULT_RESPONSE_CACHE_TOTAL_BYTES = 64 * MIB
const DEFAULT_RESPONSE_CACHE_ENTRY_BYTES = 8 * MIB
const DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES = 64 * MIB
const DEFAULT_MODELS_LIST_READ_MAX_BYTES = 8 * MIB
const RESPONSE_CACHE_BUDGET_KEYS = [
  'response_cache_local_max_bytes',
  'response_cache_local_max_entry_bytes',
  'response_cache_reconstruct_max_bytes',
  'response_cache_write_policy',
] as const satisfies ReadonlyArray<keyof SystemSettings>
const DEFAULT_CODEX_UA_CONFIG: Required<CodexUserAgentConfig> = {
  raw_user_agent: '',
  client_name: 'codex-tui',
  client_version: '0.160.0',
  os_name: 'Mac OS',
  os_version: '15.5.0',
  arch: 'arm64',
  terminal: 'xterm-256color',
  client_kind: '',
  app_name: '',
  app_version: '',
  mode: '',
  pool_mix: {},
}

type SettingsTabKey = 'codex' | 'appearance' | 'general'
const SETTINGS_TAB_KEYS: readonly SettingsTabKey[] = ['codex', 'appearance', 'general']
const DEFAULT_SETTINGS_TAB: SettingsTabKey = 'codex'
const HIDDEN_IMAGE_DRIVER_MODELS = new Set(['gpt-5.3-codex-spark', 'codex-auto-review', 'gpt-reserve'])
const isSettingsTabKey = (value: string | null): value is SettingsTabKey =>
  value !== null && (SETTINGS_TAB_KEYS as readonly string[]).includes(value)
// Mapping from the old single-page anchors to tabs, so external deep links keep working.
const LEGACY_SECTION_TABS: Record<string, SettingsTabKey> = {
  'settings-overview': 'general',
  'settings-traffic': 'general',
  'settings-runtime': 'general',
  'settings-storage': 'general',
  'settings-security': 'general',
  'settings-reference': 'general',
  'settings-models': 'codex',
  'settings-codex-quota': 'codex',
  'settings-codex-transport': 'codex',
  'settings-codex-client': 'codex',
  'settings-codex-images': 'codex',
  'settings-appearance': 'appearance',
}
// Section directory inside each tab: a tab with more than one section renders a side directory and highlights by scroll position.
// The icon matches the icon of the corresponding SettingsSection, so directory items and section titles line up with each other.
const SETTINGS_TAB_SECTION_INDEX: Record<SettingsTabKey, ReadonlyArray<{ id: string; labelKey: string; icon: ReactNode }>> = {
  codex: [
    { id: 'settings-codex-quota', labelKey: 'settings.nav.codexQuota', icon: <Gauge /> },
    { id: 'settings-codex-transport', labelKey: 'settings.nav.codexTransport', icon: <Wifi /> },
    { id: 'settings-codex-client', labelKey: 'settings.nav.codexClient', icon: <Terminal /> },
    { id: 'settings-codex-images', labelKey: 'settings.nav.codexImages', icon: <ImageIcon /> },
    { id: 'settings-models', labelKey: 'settings.nav.models', icon: <Layers /> },
  ],
  appearance: [{ id: 'settings-appearance', labelKey: 'settings.nav.appearance', icon: <Palette /> }],
  general: [
    { id: 'settings-overview', labelKey: 'settings.nav.overview', icon: <Activity /> },
    { id: 'settings-traffic', labelKey: 'settings.nav.traffic', icon: <Gauge /> },
    { id: 'settings-runtime', labelKey: 'settings.nav.runtime', icon: <Wrench /> },
    { id: 'settings-storage', labelKey: 'settings.nav.storage', icon: <ImageIcon /> },
    { id: 'settings-security', labelKey: 'settings.nav.security', icon: <Shield /> },
    { id: 'settings-reference', labelKey: 'settings.nav.reference', icon: <Link2 /> },
  ],
}
// Scroll-highlight threshold line for sections: when a section's top crosses this height in the viewport, it counts as the current section (it has to clear the sticky tab bar).
const SETTINGS_SECTION_SPY_OFFSET_PX = 140
// Keys skipped by the dirty check for manually saved fields: the generation number is read-only on the server, and custom prompt rules are saved separately on the rules page.
const SETTINGS_DIRTY_IGNORED_KEYS: ReadonlySet<string> = new Set(['response_cache_config_generation', 'output_guard_config_generation', 'prompt_filter_custom_patterns', 'codex_images_default_main_model', 'codex_egress'])

// Byte-limit fields shown in KiB/MiB and auto-saved on blur: the displayed value is a rounded integer,
// the blur commit must compare against the persisted raw byte value, otherwise unedited fields would also be rewritten by rounding.
type ByteLimitField =
  | 'output_guard_max_tool_argument_bytes'
  | 'output_guard_max_channel_bytes'
  | 'output_guard_max_request_bytes'
  | 'models_list_read_max_bytes'

const parseModelMappingEntries = (value: string, fallbackEntries: ModelMappingEntry[] = []): ModelMappingEntry[] => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return fallbackEntries
    }

    const entries = Object.entries(parsed).map(([key, model]) => [
      key,
      typeof model === 'string' ? model : String(model ?? ''),
    ]) as ModelMappingEntry[]

    // If the database value is empty, fill in the default value provided by the caller
    return entries.length > 0 ? entries : fallbackEntries
  } catch {
    return fallbackEntries
  }
}

const serializeModelMappingEntries = (entries: ModelMappingEntry[]) => {
  const obj: Record<string, string> = {}
  for (const [key, model] of entries) {
    const trimmedKey = key.trim()
    const trimmedModel = model.trim()
    if (trimmedKey && trimmedModel) obj[trimmedKey] = trimmedModel
  }
  return JSON.stringify(obj)
}

const normalizeReasoningEffortValue = (effort: string) => {
  const value = effort.trim().toLowerCase()
  // max is supported only by gpt-5.6+ upstreams; the backend clamps it per model, and the frontend keeps the original value so the user can still configure it
  return ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra', 'max'].includes(value) ? value : 'xhigh'
}

const normalizeBillingTierPolicyValue = (value?: string | null): 'actual' | 'requested' =>
  value === 'requested' ? 'requested' : 'actual'

const getSettingsPatchValues = (settings: SystemSettings, keys: Array<keyof SystemSettings>): Partial<SystemSettings> => {
  const patch: Record<string, unknown> = {}
  for (const key of keys) {
    patch[key] = settings[key]
  }
  return patch as Partial<SystemSettings>
}

// Loose equality for the dirty check: null and undefined are treated as the same, and arrays and objects are compared by JSON structure.
const settingsValueEquals = (a: unknown, b: unknown) => {
  if (a === b) return true
  if (a == null && b == null) return true
  if (a == null || b == null) return false
  if (typeof a === 'object' || typeof b === 'object') return JSON.stringify(a) === JSON.stringify(b)
  return false
}
const normalizeResponseCacheSettings = (settings: SystemSettings): SystemSettings => ({
  ...settings,
  response_cache_local_max_bytes: Number.isFinite(settings.response_cache_local_max_bytes)
    ? settings.response_cache_local_max_bytes
    : DEFAULT_RESPONSE_CACHE_TOTAL_BYTES,
  response_cache_local_max_entry_bytes: Number.isFinite(settings.response_cache_local_max_entry_bytes)
    ? settings.response_cache_local_max_entry_bytes
    : DEFAULT_RESPONSE_CACHE_ENTRY_BYTES,
  response_cache_reconstruct_max_bytes: Number.isFinite(settings.response_cache_reconstruct_max_bytes)
    ? settings.response_cache_reconstruct_max_bytes
    : DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES,
  response_cache_write_policy: settings.response_cache_write_policy === 'on_demand' ? 'on_demand' : 'always',
  response_cache_config_generation: Number.isFinite(settings.response_cache_config_generation)
    ? settings.response_cache_config_generation
    : 0,
})

const responseCacheBudgetFromSettings = (settings: SystemSettings): ResponseCacheBudgetMiB => ({
  totalMiB: bytesToMiB(settings.response_cache_local_max_bytes),
  entryMiB: bytesToMiB(settings.response_cache_local_max_entry_bytes),
  reconstructMiB: bytesToMiB(settings.response_cache_reconstruct_max_bytes),
})

const isResponseCacheBudgetKey = (key: keyof SystemSettings) =>
  RESPONSE_CACHE_BUDGET_KEYS.some((candidate) => candidate === key)

const responseCacheBudgetFieldPatch = (
  field: keyof ResponseCacheBudgetMiB,
  value: number,
): Partial<SystemSettings> => {
  switch (field) {
    case 'totalMiB':
      return { response_cache_local_max_bytes: mibToBytes(value) }
    case 'entryMiB':
      return { response_cache_local_max_entry_bytes: mibToBytes(value) }
    case 'reconstructMiB':
      return { response_cache_reconstruct_max_bytes: mibToBytes(value) }
  }
}

const parseReasoningEffortModelEntries = (value: string): ReasoningEffortModelEntry[] => {
  try {
    const parsed = JSON.parse(value || '[]')
    if (!Array.isArray(parsed)) return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
    return parsed
      .map((entry) => ({
        model: typeof entry?.model === 'string' ? entry.model : '',
        effort: normalizeReasoningEffortValue(typeof entry?.effort === 'string' ? entry.effort : 'xhigh'),
      }))
      .filter((entry) => entry.model.trim())
  } catch {
    return EMPTY_REASONING_EFFORT_MODEL_ENTRIES
  }
}

const serializeReasoningEffortModelEntries = (entries: ReasoningEffortModelEntry[]) => {
  const seen = new Set<string>()
  const normalized: ReasoningEffortModelEntry[] = []
  for (const entry of entries) {
    const model = entry.model.trim()
    const effort = normalizeReasoningEffortValue(entry.effort)
    if (!model) continue
    const key = `${model.toLowerCase()}(${effort})`
    if (seen.has(key)) continue
    seen.add(key)
    normalized.push({ model, effort })
  }
  return JSON.stringify(normalized)
}

const reasoningEffortAlias = (entry: ReasoningEffortModelEntry) => {
  const model = entry.model.trim()
  const effort = normalizeReasoningEffortValue(entry.effort)
  return model ? `${model}(${effort})` : ''
}

const parseCodexUserAgentConfig = (value?: string): CodexUserAgentConfig => {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    const poolMix: Record<string, number> = {}
    if (parsed.pool_mix && typeof parsed.pool_mix === 'object' && !Array.isArray(parsed.pool_mix)) {
      for (const [kind, weight] of Object.entries(parsed.pool_mix as Record<string, unknown>)) {
        if (typeof weight === 'number' && Number.isFinite(weight)) poolMix[kind] = weight
      }
    }
    return {
      raw_user_agent: typeof parsed.raw_user_agent === 'string' ? parsed.raw_user_agent : '',
      client_name: typeof parsed.client_name === 'string' ? parsed.client_name : '',
      client_version: typeof parsed.client_version === 'string' ? parsed.client_version : '',
      os_name: typeof parsed.os_name === 'string' ? parsed.os_name : '',
      os_version: typeof parsed.os_version === 'string' ? parsed.os_version : '',
      arch: typeof parsed.arch === 'string' ? parsed.arch : '',
      terminal: typeof parsed.terminal === 'string' ? parsed.terminal : '',
      client_kind: typeof parsed.client_kind === 'string' ? parsed.client_kind : '',
      app_name: typeof parsed.app_name === 'string' ? parsed.app_name : '',
      app_version: typeof parsed.app_version === 'string' ? parsed.app_version : '',
      mode: typeof parsed.mode === 'string' ? parsed.mode : '',
      pool_mix: poolMix,
    }
  } catch {
    return {}
  }
}

const serializeCodexUserAgentConfig = (config: CodexUserAgentConfig) => {
  const normalized: CodexUserAgentConfig = {}
  for (const key of CODEX_UA_STRING_KEYS) {
    const value = (config[key] ?? '').trim()
    if (value) normalized[key] = key === 'client_version' ? normalizeVersionText(value) : value
  }
  const poolMix: Record<string, number> = {}
  for (const [kind, weight] of Object.entries(config.pool_mix ?? {})) {
    if (Number.isFinite(weight) && weight >= 0) poolMix[kind] = Math.floor(weight)
  }
  if (Object.keys(poolMix).length > 0) normalized.pool_mix = poolMix
  return JSON.stringify(normalized)
}

const normalizeVersionText = (version?: string) => (version ?? '').trim().replace(/^v/i, '')

// Model mapping editor component
function ModelMappingEditor({
  value,
  onChange,
  fallbackEntries = EMPTY_MODEL_MAPPING_ENTRIES,
  sourceOptions,
  targetOptions,
  sourceLabel,
  targetLabel,
  sourcePlaceholder,
  targetPlaceholder,
}: {
  value: string
  onChange: (v: string) => void
  fallbackEntries?: ModelMappingEntry[]
  sourceOptions?: Array<{ label: string; value: string }>
  targetOptions?: Array<{ label: string; value: string }>
  sourceLabel: string
  targetLabel: string
  sourcePlaceholder: string
  targetPlaceholder: string
}) {
  const { t } = useTranslation()
  const [mappings, setMappings] = useState<ModelMappingEntry[]>(() => parseModelMappingEntries(value, fallbackEntries))
  const lastEmittedValueRef = useRef<string | null>(null)
  const sourceListId = useId()
  const targetListId = useId()
  const sourceSuggestions = useMemo(() => {
    if (!sourceOptions) return []
    const byValue = new Map(sourceOptions.map((option) => [option.value, option]))
    for (const [source] of mappings) {
      const value = source.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, sourceOptions])
  const targetSuggestions = useMemo(() => {
    if (!targetOptions) return []
    const byValue = new Map(targetOptions.map((option) => [option.value, option]))
    for (const [, target] of mappings) {
      const value = target.trim()
      if (value && !byValue.has(value)) {
        byValue.set(value, { label: value, value })
      }
    }
    return [...byValue.values()]
  }, [mappings, targetOptions])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setMappings(parseModelMappingEntries(value, fallbackEntries))
  }, [fallbackEntries, value])

  const updateMappings = (entries: ModelMappingEntry[]) => {
    setMappings(entries)
    const serialized = serializeModelMappingEntries(entries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, field: 0 | 1, val: string) => {
    const next = [...mappings]
    next[index] = [...next[index]] as ModelMappingEntry
    next[index][field] = val
    updateMappings(next)
  }

  const handleRemove = (index: number) => {
    const next = mappings.filter((_, i) => i !== index)
    updateMappings(next)
  }

  const listRef = useRef<HTMLDivElement>(null)
  const focusLastRowRef = useRef(false)
  const handleAdd = () => {
    const defaultSource = sourceOptions && targetOptions
      ? sourceOptions[1]?.value ?? sourceOptions[0]?.value ?? ''
      : sourceOptions?.[0]?.value ?? ''
    focusLastRowRef.current = true
    updateMappings([...mappings, [defaultSource, targetOptions?.[0]?.value ?? '']])
  }

  useEffect(() => {
    if (!focusLastRowRef.current) return
    focusLastRowRef.current = false
    const input = listRef.current?.querySelector<HTMLInputElement>('[data-mapping-row]:last-child input')
    input?.focus()
    input?.select()
    input?.scrollIntoView({ block: 'nearest' })
  }, [mappings.length])

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      {mappings.length > 0 ? (
        <div className="hidden shrink-0 grid-cols-[minmax(0,1fr)_1rem_minmax(0,1fr)_2rem] gap-1.5 px-1 text-xs font-semibold text-muted-foreground sm:grid">
          <span>{sourceLabel}</span>
          <span />
          <span>{targetLabel}</span>
          <span />
        </div>
      ) : (
        <div className="rounded-lg border border-dashed border-border/70 py-8 text-center text-xs text-muted-foreground">
          {t('settings2.mappingEmpty')}
        </div>
      )}
      <div ref={listRef} className="space-y-2 sm:space-y-1.5">
        {mappings.map(([k, v], i) => (
          <div
            key={i}
            data-mapping-row
            className="grid grid-cols-1 gap-2 rounded-xl border border-border bg-background/70 p-3 sm:grid-cols-[minmax(0,1fr)_1rem_minmax(0,1fr)_2rem] sm:items-center sm:gap-1.5 sm:rounded-none sm:border-0 sm:bg-transparent sm:p-0"
          >
            <div className="min-w-0 space-y-1 sm:space-y-0">
              <span className="text-[11px] font-semibold text-muted-foreground sm:hidden">
                {sourceLabel}
              </span>
              <Input
                className="h-8 px-2 font-mono text-xs"
                list={sourceOptions ? sourceListId : undefined}
                placeholder={sourcePlaceholder}
                value={k}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 0, e.target.value)}
              />
            </div>
            <ArrowRight className="hidden size-3.5 justify-self-center text-muted-foreground/60 sm:block" aria-hidden="true" />
            <div className="min-w-0 space-y-1 sm:space-y-0">
              <span className="text-[11px] font-semibold text-muted-foreground sm:hidden">
                {targetLabel}
              </span>
              <Input
                className="h-8 px-2 font-mono text-xs"
                list={targetOptions ? targetListId : undefined}
                placeholder={targetPlaceholder}
                value={v}
                onChange={(e: ChangeEvent<HTMLInputElement>) => handleChange(i, 1, e.target.value)}
              />
            </div>
            <button
              type="button"
              onClick={() => handleRemove(i)}
              aria-label={t('common.delete')}
              className="flex size-8 items-center justify-center justify-self-end rounded-md text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10 sm:justify-self-auto"
            >
              <Trash2 className="size-3.5" />
            </button>
          </div>
        ))}
      </div>
      {sourceOptions ? (
        <datalist id={sourceListId}>
          {sourceSuggestions.map((option) => (
            <option key={option.value} value={option.value} label={option.label} />
          ))}
        </datalist>
      ) : null}
      {targetOptions ? (
        <datalist id={targetListId}>
          {targetSuggestions.map((option) => (
            <option key={option.value} value={option.value} label={option.label} />
          ))}
        </datalist>
      ) : null}
      <Button type="button" variant="outline" size="sm" className="w-full border-dashed text-muted-foreground hover:text-foreground" onClick={handleAdd}>
        <Plus className="size-3.5" />
        {t('settings2.addMapping')}
      </Button>
    </div>
  )
}

function ReasoningEffortModelsEditor({
  value,
  onChange,
  baseModelOptions,
}: {
  value: string
  onChange: (v: string) => void
  baseModelOptions: Array<{ label: string; value: string }>
}) {
  const { t } = useTranslation()
  const [entries, setEntries] = useState<ReasoningEffortModelEntry[]>(() => parseReasoningEffortModelEntries(value))
  const lastEmittedValueRef = useRef<string | null>(null)
  const modelOptions = useMemo(() => {
    const byValue = new Map(baseModelOptions.map((option) => [option.value, option]))
    for (const entry of entries) {
      const model = entry.model.trim()
      if (model && !byValue.has(model)) {
        byValue.set(model, { label: model, value: model })
      }
    }
    return [...byValue.values()]
  }, [baseModelOptions, entries])

  useEffect(() => {
    if (value === lastEmittedValueRef.current) return
    setEntries(parseReasoningEffortModelEntries(value))
  }, [value])

  const updateEntries = (nextEntries: ReasoningEffortModelEntry[]) => {
    setEntries(nextEntries)
    const serialized = serializeReasoningEffortModelEntries(nextEntries)
    lastEmittedValueRef.current = serialized
    onChange(serialized)
  }

  const handleChange = (index: number, patch: Partial<ReasoningEffortModelEntry>) => {
    const next = entries.map((entry, i) => (i === index ? { ...entry, ...patch } : entry))
    updateEntries(next)
  }

  const handleRemove = (index: number) => {
    updateEntries(entries.filter((_, i) => i !== index))
  }

  const handleAdd = () => {
    updateEntries([...entries, { model: baseModelOptions[0]?.value ?? 'gpt-5.5', effort: 'xhigh' }])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3">
      {entries.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border/70 py-8 text-center text-xs text-muted-foreground">
          {t('settings2.reasoningEmpty')}
        </div>
      ) : null}
      {/* Mobile: stacked cards */}
      <div className="space-y-2 sm:hidden">
        {entries.map((entry, i) => (
          <div
            key={i}
            className="rounded-xl border border-border bg-background/70 p-3 space-y-2"
          >
            <div className="flex items-center justify-between gap-2">
              <span className="text-[11px] font-semibold text-muted-foreground">
                {t('settings2.baseModel')}
              </span>
              <button
                type="button"
                onClick={() => handleRemove(i)}
                aria-label={t('common.delete')}
                className="flex size-8 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
              >
                <Trash2 className="size-3.5" />
              </button>
            </div>
            <Select
              compact
              value={entry.model.trim()}
              options={modelOptions}
              placeholder={t('settings2.selectBaseModel')}
              disabled={modelOptions.length === 0}
              onValueChange={(model) => handleChange(i, { model })}
            />
            <div className="grid grid-cols-2 gap-2">
              <div className="min-w-0 space-y-1">
                <span className="text-[11px] font-semibold text-muted-foreground">
                  {t('settings2.reasoningEffort')}
                </span>
                <Select
                  compact
                  value={normalizeReasoningEffortValue(entry.effort)}
                  options={REASONING_EFFORT_OPTIONS}
                  onValueChange={(effort) => handleChange(i, { effort })}
                />
              </div>
              <div className="min-w-0 space-y-1">
                <span className="text-[11px] font-semibold text-muted-foreground">
                  {t('settings2.generatedModel')}
                </span>
                <Badge variant="secondary" className="max-w-full px-2 py-1.5 font-mono text-[11px]">
                  <span className="truncate">{reasoningEffortAlias(entry) || '-'}</span>
                </Badge>
              </div>
            </div>
          </div>
        ))}
      </div>

      {/* Desktop: compact grid */}
      <div className={cn('hidden min-h-0 flex-1 flex-col gap-2', entries.length > 0 && 'sm:flex')}>
        <div className="grid shrink-0 grid-cols-[minmax(0,1.2fr)_minmax(0,0.8fr)_minmax(0,1fr)_2rem] gap-2 px-1 text-xs font-semibold text-muted-foreground">
          <span>{t('settings2.baseModel')}</span>
          <span>{t('settings2.reasoningEffort')}</span>
          <span>{t('settings2.generatedModel')}</span>
          <span />
        </div>
        <div className="space-y-1.5">
          {entries.map((entry, i) => (
            <div
              key={i}
              className="grid grid-cols-[minmax(0,1.2fr)_minmax(0,0.8fr)_minmax(0,1fr)_2rem] items-center gap-2"
            >
              <Select
                compact
                value={entry.model.trim()}
                options={modelOptions}
                placeholder={t('settings2.selectBaseModel')}
                disabled={modelOptions.length === 0}
                onValueChange={(model) => handleChange(i, { model })}
              />
              <Select
                compact
                value={normalizeReasoningEffortValue(entry.effort)}
                options={REASONING_EFFORT_OPTIONS}
                onValueChange={(effort) => handleChange(i, { effort })}
              />
              <div className="flex min-w-0">
                <Badge variant="secondary" className="max-w-full px-2 py-1 font-mono text-[11px]">
                  <span className="truncate">{reasoningEffortAlias(entry) || '-'}</span>
                </Badge>
              </div>
              <button
                type="button"
                onClick={() => handleRemove(i)}
                aria-label={t('common.delete')}
                className="flex size-8 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-red-50 hover:text-red-500 dark:hover:bg-red-500/10"
              >
                <Trash2 className="size-3.5" />
              </button>
            </div>
          ))}
        </div>
      </div>
      <Button type="button" variant="outline" size="sm" className="w-full border-dashed text-muted-foreground hover:text-foreground" onClick={handleAdd}>
        <Plus className="size-3.5" />
        {t('settings2.addReasoningModel')}
      </Button>
    </div>
  )
}

/** Shared form grids — explicit columns so col-span / alignment stay predictable. */
const SETTINGS_FIELD_GRID = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2'
const SETTINGS_FIELD_GRID_3 = 'grid grid-cols-1 gap-x-4 gap-y-4 sm:grid-cols-2 xl:grid-cols-3'
const SETTINGS_SWITCH_GRID = 'grid grid-cols-1 gap-3 sm:grid-cols-2'
// When a card has only one switch, use a full row; in a two-column grid it would be squeezed to half width and the label would wrap.
const SETTINGS_SWITCH_ROW = 'grid grid-cols-1 gap-3'
// A group of related switch-only settings is merged into one card, with rows laid out one per line via SettingField layout="row", and the description shown directly.
const SETTINGS_ROW_LIST = 'divide-y divide-border/60'
// Card-level two-column grid: cards have different heights and must be top-aligned, otherwise short cards get stretched and leave large gaps.
const SETTINGS_CARD_GRID_2 = 'grid gap-4 lg:grid-cols-2 lg:items-stretch'

function SettingsCard({
  title,
  description,
  children,
  className,
  contentClassName,
  footer,
  icon,
  badge,
  tone = 'default',
}: {
  title: string
  description?: string
  children: ReactNode
  className?: string
  contentClassName?: string
  footer?: ReactNode
  icon?: ReactNode
  badge?: ReactNode
  tone?: 'default' | 'danger'
}) {
  return (
    <Card
      className={cn(
        'gap-0 py-0 border-border/60 bg-card shadow-2xs',
        tone === 'danger' && 'border-destructive/30 bg-destructive/[0.02]',
        className,
      )}
    >
      <CardContent className={cn('p-4.5 sm:p-5.5', contentClassName)}>
        <div className="mb-4.5 flex shrink-0 items-start gap-3">
          {icon ? (
            <div
              className={cn(
                'flex size-8 shrink-0 items-center justify-center rounded-lg ring-1 ring-inset',
                tone === 'danger'
                  ? 'bg-destructive/10 text-destructive ring-destructive/20'
                  : 'bg-muted/70 text-muted-foreground ring-border/60',
              )}
              aria-hidden="true"
            >
              <span className="[&_svg]:size-4">{icon}</span>
            </div>
          ) : null}
          <div className="min-w-0 flex-1 pt-0.5">
            <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
              <h3 className="text-sm font-semibold leading-snug tracking-tight text-foreground sm:text-[15px]">
                {title}
              </h3>
              {badge}
            </div>
            {description ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground/90">{description}</p>
            ) : null}
          </div>
        </div>
        {children}
        {footer ? <div className="mt-4.5 border-t border-border/60 pt-4 sm:mt-5">{footer}</div> : null}
      </CardContent>
    </Card>
  )
}

function SettingsCollapsibleNote({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  return (
    <details
      role="note"
      className="group overflow-hidden rounded-lg border border-primary/20 bg-primary/5"
    >
      <summary className="flex cursor-pointer list-none items-center gap-2.5 px-3 py-2.5 marker:content-none transition-colors hover:bg-primary/[0.06] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden">
        <CircleHelp className="size-4 shrink-0 text-primary" aria-hidden="true" />
        <span className="min-w-0 flex-1 text-xs font-semibold text-foreground">
          {title}
        </span>
        <ChevronDown
          className="size-3.5 shrink-0 text-muted-foreground transition-transform group-open:rotate-180"
          aria-hidden="true"
        />
      </summary>
      <div className="border-t border-primary/10 px-3 pb-2.5 pt-2">
        <p className="text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
          {children}
        </p>
      </div>
    </details>
  )
}

function SettingHelp({ text }: { text: string }) {
  return (
    <TooltipProvider delayDuration={200}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="inline-flex size-4 shrink-0 items-center justify-center rounded-full text-muted-foreground/80 transition-colors hover:bg-muted hover:text-foreground"
            aria-label={text}
          >
            <CircleHelp className="size-3.5" />
          </button>
        </TooltipTrigger>
        <TooltipContent
          side="top"
          sideOffset={6}
          className="max-w-[280px] bg-popover px-3 py-2 text-left text-xs leading-relaxed text-popover-foreground shadow-md"
        >
          {text}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}

function SettingField({
  label,
  description,
  help,
  warning,
  children,
  className,
  layout = 'stack',
  suffix,
  stretch = false,
}: {
  label: string
  description?: string
  // In row layout the description is shown directly and only help goes into the question-mark tooltip; in other layouts help and description are merged into the tooltip.
  help?: string
  warning?: string
  // In the stretch:stack layout, the control fills the remaining height (the textarea in equal-height cards).
  stretch?: boolean
  children: ReactNode
  className?: string
  layout?: 'stack' | 'switch' | 'row'
  suffix?: string
}) {
  const control = suffix ? (
    <div className="relative min-w-0">
      <div className="[&_[data-slot=input]]:pr-11 [&_[data-slot=select-trigger]]:pr-11 [&_input]:pr-11">
        {children}
      </div>
      <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium tabular-nums text-muted-foreground">
        {suffix}
      </span>
    </div>
  ) : (
    children
  )

  if (layout === 'row') {
    return (
      <div className={cn('flex min-w-0 items-start justify-between gap-4 py-4 first:pt-0 last:pb-0', className)}>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <label className="text-[13px] font-semibold leading-snug text-foreground sm:text-sm">{label}</label>
            {help ? <SettingHelp text={help} /> : null}
          </div>
          {description ? (
            <p className="max-w-3xl text-xs leading-relaxed text-muted-foreground">{description}</p>
          ) : null}
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">{warning}</p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center pt-0.5">{control}</div>
      </div>
    )
  }

  const tooltip = [description, help].filter(Boolean).join(' ')

  if (layout === 'switch') {
    return (
      <div
        className={cn(
          'flex min-h-[52px] min-w-0 items-center justify-between gap-3 rounded-xl border border-border/70 bg-card p-3.5 shadow-2xs transition-colors hover:border-border/90',
          className,
        )}
      >
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="flex items-center gap-1.5">
            <label className="block text-[13px] font-semibold leading-snug text-foreground sm:text-sm">
              {label}
            </label>
            {tooltip ? <SettingHelp text={tooltip} /> : null}
          </div>
          {warning ? (
            <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
              {warning}
            </p>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center self-center">{control}</div>
      </div>
    )
  }

  return (
    <div className={cn('flex min-w-0 flex-col gap-1.5', stretch && 'flex-1', className)}>
      <div className="flex min-h-5 items-center gap-1.5">
        <label className="block text-[13px] font-semibold leading-none text-foreground sm:text-sm">
          {label}
        </label>
        {tooltip ? <SettingHelp text={tooltip} /> : null}
      </div>
      <div className={cn('min-w-0', stretch && 'flex flex-1 flex-col [&>*]:flex-1')}>{control}</div>
      {warning ? (
        <p className="text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
          {warning}
        </p>
      ) : null}
    </div>
  )
}

function SettingsFieldGroup({
  title,
  hint,
  children,
}: {
  title: string
  hint?: string
  children: ReactNode
}) {
  return (
    <div className="grid grid-cols-1 gap-3 p-3.5 sm:p-4 md:grid-cols-[7.5rem_minmax(0,1fr)] md:gap-5">
      <div className="min-w-0 md:pt-0.5">
        <div className="text-[13px] font-semibold leading-snug text-foreground">{title}</div>
        {hint ? <code className="mt-1 block truncate font-mono text-[11px] text-muted-foreground">{hint}</code> : null}
      </div>
      <div className="grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">{children}</div>
    </div>
  )
}

function SettingsSkeleton() {
  return (
    <div className="space-y-6" aria-busy="true" aria-live="polite">
      <div className="space-y-2">
        <div className="h-8 w-40 animate-pulse rounded-lg bg-muted" />
        <div className="h-4 w-72 max-w-full animate-pulse rounded-md bg-muted/70" />
      </div>
      <div className="h-11 w-full animate-pulse rounded-full bg-muted" />
      <div className="grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="h-[72px] animate-pulse rounded-lg border border-border bg-muted/40" />
        ))}
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        {[0, 1].map((i) => (
          <Card key={i} className="gap-0 py-0">
            <CardContent className="space-y-3 p-5">
              <div className="h-4 w-28 animate-pulse rounded bg-muted" />
              <div className="grid grid-cols-2 gap-3">
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/70" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/60" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/50" />
                <div className="h-9 w-full animate-pulse rounded-md bg-muted/40" />
              </div>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  )
}

function UnsavedDot({ label }: { label: string }) {
  return (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-semibold leading-none text-amber-600 dark:text-amber-400">
      <span className="size-1.5 rounded-full bg-amber-500" aria-hidden="true" />
      {label}
    </span>
  )
}

function ModelSourceBadge({ source }: { source: string }) {
  const { t } = useTranslation()
  const label =
    source === 'official_codex_docs'
      ? t('settings.modelSourceOfficial')
      : source === 'upstream_manifest'
        ? t('settings.modelSourceUpstream')
        : source === 'reasoning_effort'
          ? t('settings.modelSourceReasoning')
          : source === 'manual'
            ? t('settings.modelSourceManual')
            : source === 'daybreak'
              ? 'Daybreak'
              : t('settings.modelSourceBuiltin')
  return (
    <Badge
      variant={source === 'official_codex_docs' ? 'default' : source === 'builtin' ? 'outline' : 'secondary'}
      className="text-[10px]"
    >
      {label}
    </Badge>
  )
}

function ModelRegistryOverview({
  items,
  lastSyncedLabel,
  sourceUrl,
  syncing,
  onSync,
  onOpen,
}: {
  items: ModelInfo[]
  lastSyncedLabel: string
  sourceUrl: string
  syncing: boolean
  onSync: () => void
  onOpen: () => void
}) {
  const { t } = useTranslation()
  const enabled = items.filter((model) => model.enabled)
  const imageCount = enabled.filter((model) => model.category === 'image').length
  const preview = enabled.slice(0, MODEL_REGISTRY_PREVIEW_LIMIT)
  const stats = [
    { label: t('settings.modelsEnabled'), value: `${enabled.length}/${items.length}` },
    { label: t('settings.modelCategoryText'), value: String(enabled.length - imageCount) },
    { label: t('settings.modelImage'), value: String(imageCount) },
    { label: t('settings.modelsLastSynced'), value: lastSyncedLabel },
  ]

  return (
    <SettingsCard
      title={t('settings.modelRegistry')}
      description={t('settings.modelRegistryDesc')}
      icon={<Layers className="size-4" />}
      badge={
        <a
          href={sourceUrl}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-primary"
        >
          {t('settings.nav.openSource')}
          <ExternalLink className="size-3" />
        </a>
      }
    >
      <div className="space-y-3.5">
        <div className="grid grid-cols-2 overflow-hidden rounded-lg border border-border/60 bg-muted/20 sm:grid-cols-[repeat(3,minmax(0,1fr))_minmax(0,1.9fr)]">
          {stats.map((stat, index) => (
            <div
              key={stat.label}
              className={cn(
                'min-w-0 px-3.5 py-2.5',
                index % 2 === 1 && 'border-l border-border/60',
                index >= 2 && 'max-sm:border-t max-sm:border-border/60',
                index === 2 && 'sm:border-l',
              )}
            >
              <div className="text-[11px] font-medium text-muted-foreground">{stat.label}</div>
              <div className="mt-0.5 truncate text-sm font-semibold tabular-nums text-foreground" title={stat.value}>{stat.value}</div>
            </div>
          ))}
        </div>
        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <button
            type="button"
            onClick={onOpen}
            className="group flex min-w-0 flex-1 flex-wrap items-center gap-1.5 rounded-lg text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            {preview.map((model) => (
              <span
                key={model.id}
                className="rounded-md border border-border/70 bg-background px-2 py-0.5 font-mono text-[11px] text-foreground/90 transition-colors group-hover:border-primary/30"
              >
                {model.id}
              </span>
            ))}
            {enabled.length > preview.length ? (
              <span className="rounded-md bg-muted px-2 py-0.5 text-[11px] font-semibold tabular-nums text-muted-foreground">
                +{enabled.length - preview.length}
              </span>
            ) : null}
          </button>
          <div className="flex shrink-0 items-center gap-2">
            <Button size="sm" variant="ghost" onClick={onOpen}>
              {t('settings.modelRegistryViewAll', { count: items.length })}
              <ChevronRight className="size-3.5" />
            </Button>
            <Button size="sm" variant="outline" onClick={onSync} disabled={syncing}>
              <RefreshCw className={cn('size-3.5', syncing && 'animate-spin')} />
              {syncing ? t('settings.modelsSyncing') : t('settings.syncUpstreamModels')}
            </Button>
          </div>
        </div>
      </div>
    </SettingsCard>
  )
}

function ModelRuleCard({
  icon,
  title,
  description,
  count,
  preview,
  dirty = false,
  external = false,
  onOpen,
}: {
  icon: ReactNode
  title: string
  description: string
  count: number
  preview?: ReactNode
  dirty?: boolean
  external?: boolean
  onOpen: () => void
}) {
  const { t } = useTranslation()
  const configured = count > 0
  return (
    <button
      type="button"
      onClick={onOpen}
      className="group flex h-full w-full flex-col rounded-xl border border-border/70 bg-card p-4 text-left shadow-2xs transition-all hover:-translate-y-px hover:border-primary/40 hover:shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      <div className="flex items-start gap-3">
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-muted/70 text-muted-foreground ring-1 ring-inset ring-border/60 transition-colors group-hover:bg-primary/10 group-hover:text-primary group-hover:ring-primary/20 [&_svg]:size-4">
          {icon}
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate text-sm font-semibold leading-snug text-foreground">{title}</span>
            {dirty ? <UnsavedDot label={t('settings.modelRuleUnsaved')} /> : null}
          </div>
          <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-muted-foreground" title={description}>{description}</p>
        </div>
        {external ? (
          <ArrowUpRight className="mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform group-hover:-translate-y-0.5 group-hover:translate-x-0.5 group-hover:text-primary" />
        ) : (
          <ChevronRight className="mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5 group-hover:text-primary" />
        )}
      </div>
      <div className="mt-3.5 flex flex-1 flex-col justify-end gap-3 border-t border-border/50 pt-3">
        {configured ? (
          preview ? <div className="min-h-[3.25rem]">{preview}</div> : null
        ) : (
          <div className="flex min-h-[3.25rem] items-center justify-center rounded-lg border border-dashed border-border/70 px-3 text-center text-[11px] text-muted-foreground transition-colors group-hover:border-primary/30 group-hover:text-foreground/80">
            {t('settings.modelRuleAddFirst')}
          </div>
        )}
        <div className="flex items-center justify-between gap-2 text-xs">
          {configured ? (
            <Badge variant="secondary" className="font-semibold tabular-nums">
              {t('settings.nav.mappingCount', { count })}
            </Badge>
          ) : (
            <span className="text-muted-foreground">{t('settings.modelRuleEmpty')}</span>
          )}
          <span className="font-medium text-muted-foreground transition-colors group-hover:text-primary">
            {external ? t('settings.modelRuleOpenPage') : t('settings.nav.manage')}
          </span>
        </div>
      </div>
    </button>
  )
}

function ModelMappingPreview({ entries }: { entries: ModelMappingEntry[] }) {
  return (
    <ul className="space-y-1">
      {entries.slice(0, MODEL_RULE_PREVIEW_LIMIT).map(([from, to], index) => (
        <li key={`${from}-${index}`} className="flex min-w-0 items-center gap-1.5 font-mono text-[11px] leading-4">
          <span className="min-w-0 truncate text-muted-foreground">{from}</span>
          <ArrowRight className="size-3 shrink-0 text-muted-foreground/60" />
          <span className="min-w-0 truncate font-medium text-foreground">{to}</span>
        </li>
      ))}
    </ul>
  )
}

function ModelChipPreview({ labels }: { labels: string[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {labels.map((label) => (
        <span key={label} className="rounded-md border border-border/70 bg-muted/30 px-1.5 py-0.5 font-mono text-[11px] text-foreground/90">
          {label}
        </span>
      ))}
    </div>
  )
}

function ModelRegistryPanel({
  items,
  lastSyncedLabel,
  syncing,
  onSync,
}: {
  items: ModelInfo[]
  lastSyncedLabel: string
  syncing: boolean
  onSync: () => void
}) {
  const { t } = useTranslation()
  const [query, setQuery] = useState('')
  const [category, setCategory] = useState<'all' | 'text' | 'image'>('all')
  const imageCount = items.filter((model) => model.category === 'image').length
  const needle = query.trim().toLowerCase()
  const filtered = items.filter((model) => {
    if (category === 'image' && model.category !== 'image') return false
    if (category === 'text' && model.category === 'image') return false
    return !needle || model.id.toLowerCase().includes(needle)
  })

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border/60 bg-muted/20 px-3 py-2 text-xs text-muted-foreground">
        <span className="tabular-nums">
          {t('settings.modelsEnabled')}{' '}
          <span className="font-semibold text-foreground">{items.filter((model) => model.enabled).length}/{items.length}</span>
          <span className="mx-2 text-border">·</span>
          {t('settings.modelsLastSynced')} <span className="font-semibold text-foreground">{lastSyncedLabel}</span>
        </span>
        <Button size="sm" variant="outline" onClick={onSync} disabled={syncing}>
          <RefreshCw className={cn('size-3.5', syncing && 'animate-spin')} />
          {syncing ? t('settings.modelsSyncing') : t('settings.syncUpstreamModels')}
        </Button>
      </div>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e: ChangeEvent<HTMLInputElement>) => setQuery(e.target.value)}
            placeholder={t('settings.modelRegistrySearch')}
            className="h-8 pl-8 font-mono text-xs"
          />
        </div>
        <SegmentedPillGroup
          className="sm:w-auto"
          value={category}
          onChange={setCategory}
          options={[
            { value: 'all', label: `${t('settings.modelCategoryAll')} ${items.length}` },
            { value: 'text', label: `${t('settings.modelCategoryText')} ${items.length - imageCount}` },
            { value: 'image', label: `${t('settings.modelImage')} ${imageCount}` },
          ]}
        />
      </div>
      {filtered.length > 0 ? (
        <div className="grid gap-1.5 sm:grid-cols-2">
          {filtered.map((model) => (
            <div
              key={model.id}
              className={cn(
                'flex min-w-0 items-center justify-between gap-2 rounded-lg border border-border/60 bg-background px-3 py-2',
                !model.enabled && 'opacity-60',
              )}
            >
              <span className="min-w-0 truncate font-mono text-xs font-semibold text-foreground" title={model.id}>
                {model.id}
              </span>
              <div className="flex shrink-0 items-center gap-1">
                {!model.enabled ? <Badge variant="outline" className="text-[10px]">{t('settings.modelDisabled')}</Badge> : null}
                {model.pro_only ? <Badge variant="outline" className="text-[10px]">{t('settings.modelProOnly')}</Badge> : null}
                <ModelSourceBadge source={model.source} />
              </div>
            </div>
          ))}
        </div>
      ) : (
        <div className="rounded-lg border border-dashed border-border/70 py-10 text-center text-xs text-muted-foreground">
          {t('settings.modelRegistryEmpty')}
        </div>
      )}
    </div>
  )
}

function StatusTile({
  label,
  children,
  icon,
}: {
  label: string
  children: ReactNode
  icon?: ReactNode
}) {
  return (
    <div
      data-slot="status-tile"
      className="flex min-h-[80px] flex-col justify-between gap-2.5 rounded-xl border border-border/70 bg-gradient-to-br from-card via-card to-muted/20 p-3.5 shadow-2xs transition-all hover:border-border"
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground/90">
          {label}
        </span>
        {icon ? <span className="text-muted-foreground/70 [&_svg]:size-4">{icon}</span> : null}
      </div>
      <div className="flex min-h-6 items-center text-sm font-bold tabular-nums text-foreground">
        {children}
      </div>
    </div>
  )
}

function SegmentedPillGroup<T extends string>({
  value,
  onChange,
  options,
  disabled = false,
  className,
}: {
  value: T
  onChange: (value: T) => void
  options: Array<{ label: string; value: T; icon?: ReactNode }>
  disabled?: boolean
  className?: string
}) {
  const activeIndex = options.findIndex((opt) => opt.value === value)
  const count = options.length

  return (
    <div
      className={cn(
        'relative flex items-center rounded-xl border border-border/70 bg-muted/35 p-1 shadow-2xs select-none',
        className,
      )}
    >
      {/* Smooth sliding background indicator */}
      {activeIndex >= 0 && count > 0 ? (
        <div
          className="absolute inset-y-1 rounded-lg bg-background shadow-xs ring-1 ring-border/60 transition-all duration-200 ease-[cubic-bezier(0.16,1,0.3,1)]"
          style={{
            width: `calc((100% - 0.5rem) / ${count})`,
            left: `calc(0.25rem + ${activeIndex} * ((100% - 0.5rem) / ${count}))`,
          }}
        />
      ) : null}

      {/* Option buttons */}
      <div className="relative z-10 flex w-full items-center">
        {options.map((opt) => {
          const active = opt.value === value
          return (
            <button
              key={opt.value}
              type="button"
              disabled={disabled}
              onClick={() => onChange(opt.value)}
              className={cn(
                'flex flex-1 items-center justify-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-semibold transition-colors duration-200 active:scale-98',
                active
                  ? 'text-foreground font-bold'
                  : 'text-muted-foreground hover:text-foreground',
                disabled && 'opacity-50 cursor-not-allowed',
              )}
            >
              {opt.icon ? <span className="[&_svg]:size-3.5">{opt.icon}</span> : null}
              <span className="whitespace-nowrap">{opt.label}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

function SettingsSection({
  id,
  title,
  description,
  icon,
  children,
}: {
  id: string
  title: string
  description?: string
  icon?: ReactNode
  children: ReactNode
}) {
  return (
    <section id={id} data-settings-section={id} className="scroll-mt-32 space-y-4">
      <div className="space-y-1 px-0.5">
        <div className="flex items-center gap-2.5">
          {icon ? (
            <span className="shrink-0 text-muted-foreground [&_svg]:size-4" aria-hidden="true">
              {icon}
            </span>
          ) : null}
          <h2 className="text-[15px] font-semibold tracking-tight text-foreground sm:text-base">{title}</h2>
          <div className="ml-1 h-px flex-1 bg-border/60" />
        </div>
        {description ? (
          <p className="max-w-3xl text-xs leading-relaxed text-muted-foreground">{description}</p>
        ) : null}
      </div>
      <div className="space-y-4">{children}</div>
    </section>
  )
}

type SettingsSectionIndexItem = { id: string; label: string; icon: ReactNode }

// Work out the current section from the scroll position: the last section whose top has crossed the threshold line is the current section.
// After a directory click, the chosen section is locked first; position-based detection resumes only when the user scrolls manually (wheel/touch/keyboard) —
// When several sections at the end of the page fit on one screen, scroll position cannot tell which one the user clicked.
// The scroll-to-bottom fallback gives the highlight to the last section only when it really occupies the lower half of the viewport; otherwise it would steal the highlight from the second-to-last section.
function useActiveSettingsSection(sectionIds: readonly string[]) {
  const [activeId, setActiveId] = useState<string | null>(sectionIds[0] ?? null)
  const pinnedRef = useRef<string | null>(null)
  const pinSection = useCallback((id: string) => {
    pinnedRef.current = id
    setActiveId(id)
  }, [])
  useEffect(() => {
    pinnedRef.current = null
    setActiveId(sectionIds[0] ?? null)
    if (sectionIds.length < 2) return
    let frame = 0
    const update = () => {
      frame = 0
      if (pinnedRef.current) return
      let current = sectionIds[0]
      for (const id of sectionIds) {
        const el = document.getElementById(id)
        if (el && el.getBoundingClientRect().top <= SETTINGS_SECTION_SPY_OFFSET_PX) current = id
      }
      const doc = document.documentElement
      const atBottom = window.innerHeight + window.scrollY >= doc.scrollHeight - 2
      if (atBottom) {
        const lastId = sectionIds[sectionIds.length - 1]
        const last = document.getElementById(lastId)
        if (last && last.getBoundingClientRect().top <= window.innerHeight / 2) current = lastId
      }
      setActiveId(current)
    }
    const schedule = () => {
      if (!frame) frame = window.requestAnimationFrame(update)
    }
    const unpin = () => {
      if (!pinnedRef.current) return
      pinnedRef.current = null
      schedule()
    }
    update()
    window.addEventListener('scroll', schedule, { passive: true })
    window.addEventListener('resize', schedule)
    window.addEventListener('wheel', unpin, { passive: true })
    window.addEventListener('touchstart', unpin, { passive: true })
    window.addEventListener('keydown', unpin)
    return () => {
      if (frame) window.cancelAnimationFrame(frame)
      window.removeEventListener('scroll', schedule)
      window.removeEventListener('resize', schedule)
      window.removeEventListener('wheel', unpin)
      window.removeEventListener('touchstart', unpin)
      window.removeEventListener('keydown', unpin)
    }
  }, [sectionIds])
  return { activeId, pinSection }
}

// Section directory inside a tab: a centered frosted-glass pill bar below the tab bar, sticks to the top together with the tab bar, and highlights the current section by scroll position.
function SettingsSectionIndex({
  items,
  activeId,
  label,
  onSelect,
}: {
  items: readonly SettingsSectionIndexItem[]
  activeId: string | null
  label: string
  onSelect: (id: string) => void
}) {
  return (
    <nav aria-label={label} className="flex justify-center">
      <div
        className={cn(
          'flex max-w-full items-center gap-1 overflow-x-auto rounded-full border border-white/50 bg-card/55 p-1 backdrop-blur-xl backdrop-saturate-150 dark:border-white/10 dark:bg-card/45',
          'shadow-[0_8px_30px_rgb(0_0_0/0.06)] ring-1 ring-black/[0.04] dark:ring-white/[0.05]',
          '[-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        )}
      >
        {items.map((item) => {
          const active = item.id === activeId
          return (
            <button
              key={item.id}
              type="button"
              onClick={() => onSelect(item.id)}
              aria-current={active ? 'location' : undefined}
              className={cn(
                'inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full px-3 py-1.5 text-xs font-semibold transition-colors duration-200 [&_svg]:size-3.5',
                active
                  ? 'bg-primary/12 text-primary shadow-2xs ring-1 ring-primary/15'
                  : 'text-muted-foreground hover:bg-background/70 hover:text-foreground',
              )}
            >
              <span className={cn('shrink-0', active ? 'opacity-100' : 'opacity-75')} aria-hidden="true">
                {item.icon}
              </span>
              {item.label}
            </button>
          )
        })}
      </div>
    </nav>
  )
}

// Header save status: auto-save in progress > manual fields unsaved > auto-save failed > saved.
function SaveStatusPill({
  autoSaveStatus,
  dirtyCount,
}: {
  autoSaveStatus: AutoSaveStatus
  dirtyCount: number
}) {
  const { t } = useTranslation()
  let tone = 'text-muted-foreground'
  let icon: ReactNode = <Check className="size-3.5" />
  let text = t('settings.saveStatusSaved')
  let title: string | undefined
  if (autoSaveStatus === 'saving') {
    icon = <Loader2 className="size-3.5 animate-spin" />
    text = t('settings.autoSaving')
  } else if (dirtyCount > 0) {
    tone = 'text-amber-700 dark:text-amber-300'
    icon = <span className="size-1.5 rounded-full bg-amber-500" aria-hidden="true" />
    text = t('settings.saveStatusUnsaved', { n: dirtyCount })
    title = t('settings.saveStatusUnsavedHint')
  } else if (autoSaveStatus === 'error') {
    tone = 'text-destructive'
    icon = <CircleAlert className="size-3.5" />
    text = t('settings.autoSaveFailed')
  } else if (autoSaveStatus === 'saved') {
    tone = 'text-emerald-700 dark:text-emerald-300'
    text = t('settings.autoSaved')
  }
  return (
    <span
      data-slot="save-status"
      title={title}
      aria-live="polite"
      className={cn('inline-flex h-8 items-center gap-1.5 whitespace-nowrap px-1 text-xs font-medium tabular-nums', tone)}
    >
      {icon}
      {text}
    </span>
  )
}

const SITE_LOGO_MAX_BYTES = 600 * 1024
const SITE_LOGO_CANVAS_SIZE = 80
const BACKGROUND_IMAGE_UPLOAD_MAX_BYTES = 20 * 1024 * 1024
const BACKGROUND_VIDEO_UPLOAD_MAX_BYTES = 40 * 1024 * 1024

function formatBytesKB(bytes: number) {
  return Math.max(1, Math.round(bytes / 1024))
}

function getSiteLogoMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  return ''
}

function getBackgroundImageMimeType(file: File) {
  const type = file.type.toLowerCase()
  const name = file.name.toLowerCase()
  if (type === 'image/png' || name.endsWith('.png')) return 'image/png'
  if (type === 'image/jpeg' || name.endsWith('.jpg') || name.endsWith('.jpeg')) return 'image/jpeg'
  if (type === 'image/webp' || name.endsWith('.webp')) return 'image/webp'
  if (type === 'image/svg+xml' || name.endsWith('.svg')) return 'image/svg+xml'
  if (type === 'video/mp4' || name.endsWith('.mp4')) return 'video/mp4'
  return ''
}

function dataURLByteLength(dataURL: string) {
  const commaIndex = dataURL.indexOf(',')
  if (commaIndex === -1) return new Blob([dataURL]).size
  const meta = dataURL.slice(0, commaIndex)
  const data = dataURL.slice(commaIndex + 1)
  if (meta.endsWith(';base64')) {
    const padding = data.endsWith('==') ? 2 : data.endsWith('=') ? 1 : 0
    return Math.floor((data.length * 3) / 4) - padding
  }
  return new Blob([decodeURIComponent(data)]).size
}

function readFileAsDataURL(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : '')
    reader.onerror = reject
    reader.readAsDataURL(file)
  })
}

function textToBase64(value: string) {
  const bytes = new TextEncoder().encode(value)
  let binary = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.slice(i, i + 0x8000))
  }
  return btoa(binary)
}

function minifySVG(value: string) {
  // Strip comments in a loop until a fixed point: a single pass can leave residue, because adjacent fragments can rejoin to form "<!--"
  let out = value
  for (let prev = ''; prev !== out; ) {
    prev = out
    out = out.replace(/<!--[\s\S]*?-->/g, '').replace(/<!--/g, '')
  }
  return out
    .replace(/>\s+</g, '><')
    .replace(/\s{2,}/g, ' ')
    .trim()
}

function loadImage(src: string) {
  return new Promise<HTMLImageElement>((resolve, reject) => {
    const image = new Image()
    image.onload = () => resolve(image)
    image.onerror = reject
    image.src = src
  })
}

function canvasToBlob(canvas: HTMLCanvasElement, type: string, quality?: number) {
  return new Promise<Blob>((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (blob) resolve(blob)
      else reject(new Error('canvas-to-blob-failed'))
    }, type, quality)
  })
}

async function blobToDataURL(blob: Blob) {
  return readFileAsDataURL(new File([blob], 'site-logo', { type: blob.type }))
}

async function compressImageSourceToDataURL(src: string, mimeType: string) {
  const image = await loadImage(src)
  const canvas = document.createElement('canvas')
  canvas.width = SITE_LOGO_CANVAS_SIZE
  canvas.height = SITE_LOGO_CANVAS_SIZE
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('canvas-context-unavailable')

  const outputType = mimeType === 'image/jpeg' ? 'image/jpeg' : 'image/png'
  if (outputType === 'image/jpeg') {
    ctx.fillStyle = '#ffffff'
    ctx.fillRect(0, 0, canvas.width, canvas.height)
  } else {
    ctx.clearRect(0, 0, canvas.width, canvas.height)
  }

  const sourceWidth = image.naturalWidth || image.width || SITE_LOGO_CANVAS_SIZE
  const sourceHeight = image.naturalHeight || image.height || SITE_LOGO_CANVAS_SIZE
  const scale = Math.min(canvas.width / sourceWidth, canvas.height / sourceHeight)
  const drawWidth = Math.max(1, Math.round(sourceWidth * scale))
  const drawHeight = Math.max(1, Math.round(sourceHeight * scale))
  const dx = Math.round((canvas.width - drawWidth) / 2)
  const dy = Math.round((canvas.height - drawHeight) / 2)
  ctx.drawImage(image, dx, dy, drawWidth, drawHeight)

  if (outputType === 'image/png') {
    const blob = await canvasToBlob(canvas, outputType)
    return blobToDataURL(blob)
  }

  const qualities = [0.86, 0.72, 0.6, 0.48, 0.36]
  let bestDataURL = ''
  for (const quality of qualities) {
    const blob = await canvasToBlob(canvas, outputType, quality)
    const dataURL = await blobToDataURL(blob)
    bestDataURL = dataURL
    if (dataURLByteLength(dataURL) <= SITE_LOGO_MAX_BYTES) return dataURL
  }
  return bestDataURL
}

async function compressSiteLogoFile(file: File, mimeType: string) {
  if (mimeType === 'image/svg+xml') {
    const minified = minifySVG(await file.text())
    const svgDataURL = `data:image/svg+xml;base64,${textToBase64(minified)}`
    if (dataURLByteLength(svgDataURL) <= SITE_LOGO_MAX_BYTES) return svgDataURL
    return compressImageSourceToDataURL(svgDataURL, mimeType)
  }

  const objectURL = URL.createObjectURL(file)
  try {
    return await compressImageSourceToDataURL(objectURL, mimeType)
  } finally {
    URL.revokeObjectURL(objectURL)
  }
}

export default function Settings() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { applyBranding } = useBranding()
  const schedulerModeOptions = [
    { label: t('settings.schedulerModeRoundRobin'), value: 'round_robin' },
    { label: t('settings.schedulerModeRemainingQuota'), value: 'remaining_quota' },
    { label: t('settings.schedulerModeFillFirst'), value: 'fill_first' },
    { label: t('settings.schedulerModeReset5hFirst'), value: 'reset_5h_first' },
  ]
  const schedulerEngineOptions = [
    { label: t('settings.schedulerEngineLegacy'), value: 'legacy' },
    { label: t('settings.schedulerEngineShadow'), value: 'shadow' },
    { label: t('settings.schedulerEngineIndexed'), value: 'indexed' },
  ]
  const schedulerEngineExplanations = [
    {
      label: t('settings.schedulerEngineLegacy'),
      value: 'legacy',
      description: t('settings.schedulerEngineLegacyDesc'),
    },
    {
      label: t('settings.schedulerEngineShadow'),
      value: 'shadow',
      description: t('settings.schedulerEngineShadowDesc'),
    },
    {
      label: t('settings.schedulerEngineIndexed'),
      value: 'indexed',
      description: t('settings.schedulerEngineIndexedDesc'),
    },
  ]
  const transportRetryPolicyOptions = [
    { label: t('settings.transportRetryPolicyRotate'), value: 'rotate' },
    { label: t('settings.transportRetryPolicySticky'), value: 'sticky' },
  ]
  const continuousRetryCategoryOptions = [
    { label: t('settings.continuousRetryCategoryTransport'), value: 'transport' },
    { label: t('settings.continuousRetryCategory429'), value: 'http_429' },
    { label: t('settings.continuousRetryCategory4xx'), value: 'http_4xx' },
    { label: t('settings.continuousRetryCategory5xx'), value: 'http_5xx' },
    { label: t('settings.continuousRetryCategoryStream'), value: 'stream_error' },
    { label: t('settings.continuousRetryCategoryResponseFailed'), value: 'response_failed' },
    { label: t('settings.continuousRetryCategoryContext'), value: 'context_error' },
  ]
  const codexFingerprintDefaultModeOptions = [
    { label: t('accounts.codexFingerprintModeOff'), value: 'off' },
    { label: t('accounts.codexFingerprintModeDevice'), value: 'device' },
    { label: t('accounts.codexFingerprintModeSession'), value: 'session' },
    { label: t('accounts.codexFingerprintModeSessionIdentity'), value: 'single_machine_multi_window' },
    { label: t('accounts.codexFingerprintModeFull'), value: 'full' },
  ]
  const modelCooldownModeOptions = [
    { label: t('settings.modelCooldownModeOff'), value: 'off' },
    { label: t('settings.modelCooldownModeFixed'), value: 'fixed' },
    { label: t('settings.modelCooldownModeAdaptive'), value: 'adaptive' },
  ]
  const responseCacheWritePolicyOptions = [
    { label: t('settings.responseCache.writePolicyAlways'), value: 'always' },
    { label: t('settings.responseCache.writePolicyOnDemand'), value: 'on_demand' },
  ]
  const affinityModeOptions = [
    { label: t('settings.affinityModeBounded'), value: 'bounded' },
    { label: t('settings.affinityModeOff'), value: 'off' },
    { label: t('settings.affinityModeStrict'), value: 'strict' },
  ]
  const clientCompatOptions = [
    { label: t('settings.clientCompatPreserve'), value: 'preserve' },
    { label: t('settings.clientCompatAuto'), value: 'auto' },
    { label: t('settings.clientCompatForce'), value: 'force' },
  ]
  const usageLogModeOptions = [
    { label: t('settings.usageLogFull'), value: 'full' },
    { label: t('settings.usageLogErrors'), value: 'errors' },
    { label: t('settings.usageLogOff'), value: 'off' },
  ]
  const billingTierPolicyOptions = [
    { label: t('settings.billingTierPolicyActual'), value: 'actual' },
    { label: t('settings.billingTierPolicyRequested'), value: 'requested' },
  ]
  const streamFlushPolicyOptions = [
    { label: t('settings.streamFlushImmediate'), value: 'immediate' },
    { label: t('settings.streamFlushCoalesce'), value: 'coalesce' },
  ]
  const imageStorageBackendOptions = [
    { label: t('settings.imageStorageLocal'), value: 'local' },
    { label: t('settings.imageStorageS3'), value: 's3' },
  ]
  const normalizeLazySettingsForm = useCallback((settings: SystemSettings): SystemSettings => {
    const cacheNormalized = normalizeResponseCacheSettings(settings)
    const normalized = {
      ...cacheNormalized,
      codex_images_main_model: cacheNormalized.codex_images_main_model ?? '',
      codex_telemetry_enabled: cacheNormalized.codex_telemetry_enabled ?? false,
      codex_telemetry_timing_debug: cacheNormalized.codex_telemetry_timing_debug ?? false,
      billing_tier_policy: normalizeBillingTierPolicyValue(cacheNormalized.billing_tier_policy),
      first_token_mode: 'loose',
      models_list_read_max_bytes:
        Number.isFinite(cacheNormalized.models_list_read_max_bytes) && cacheNormalized.models_list_read_max_bytes >= MIB
          ? cacheNormalized.models_list_read_max_bytes
          : DEFAULT_MODELS_LIST_READ_MAX_BYTES,
    }
    if (!normalized.lazy_mode) {
      return normalized
    }
    return {
      ...normalized,
      auto_clean_full_usage: false,
    }
  }, [])
  const [settingsForm, setSettingsForm] = useState<SystemSettings>({
    site_name: 'CodexProxy',
    site_logo: '',
    background_image: '',
    background_opacity: 18,
    background_blur: 0,
    background_glass_opacity: 58,
    background_glass_blur: 5,
    max_concurrency: 2,
    global_rpm: 0,
    test_model: '',
    test_content: 'hi',
    test_concurrency: 50,
	    background_refresh_interval_minutes: 2,
	    usage_probe_max_age_minutes: 10,
	    usage_probe_concurrency: 16,
	    usage_probe_responses_fallback_enabled: true,
	    recovery_probe_interval_minutes: 30,
    lazy_mode: false,
    codex_oauth_keepalive_enabled: false,
    pg_max_conns: 50,
    redis_pool_size: 30,
    auto_clean_unauthorized: false,
    auto_clean_rate_limited: false,
    auto_clean_error: false,
    auto_clean_expired: false,
    admin_secret: '',
    admin_auth_source: 'disabled',
    auto_clean_full_usage: false,
    proxy_pool_enabled: false,
    fast_scheduler_enabled: false,
    scheduler_engine: 'legacy',
    auto_reset_credits_on_exhaustion_enabled: false,
    auto_reset_credits_enabled: false,
    auto_reset_credits_before_expiry_min: 60,
    auto_activate_5h_window_enabled: false,
    codex_force_websocket: false,
    codex_telemetry_enabled: false,
    codex_telemetry_timing_debug: false,
    codex_request_compression: true,
    codex_ws_weak_network_mode: false,
    codex_ws_keepalive_enabled: false,
    codex_ws_keepalive_interval_sec: 60,
    codex_ws_hide_upstream_errors: true,
    codex_ws_silent_retry_enabled: true,
    codex_ws_silent_max_retries: 2,
    codex_ws_size_router_enabled: true,
    codex_ws_busy_acquire_max_wait_sec: 30,
    codex_ws_busy_overflow_enabled: false,
    codex_ws_busy_patience_sec: 2,
    codex_ws_stateless_slots: 8,
    codex_ws_downstream_keepalive_slots: 8,
    github_token_configured: false,
    github_proxy_url: '',
    codex_overload_pause_enabled: false,
    codex_overload_threshold_percent: 20,
    codex_overload_pause_minutes: 30,
    codex_overload_window_minutes: 5,
    codex_continue_thinking_enabled: false,
    overflow_auto_compact_enabled: false,
    compact_via_responses_enabled: false,
    codex_preflight_sse_passthrough_enabled: false,
    codex_continue_max_rounds: 8,
    utls_shutdown_timeout_minutes: 30,
    scheduler_mode: 'round_robin',
    affinity_mode: 'bounded',
    session_affinity_spread: false,
    session_slot_buffer_enabled: false,
    session_slot_buffer_seconds: 10,
    max_retries: 2,
    max_rate_limit_retries: 1,
    retry_interval_ms: 0,
    transport_retry_policy: 'rotate',
    continuous_retry_enabled: false,
    continuous_retry_catch_all: false,
    continuous_retry_categories: ['transport', 'http_429', 'http_5xx', 'stream_error'],
    continuous_retry_status_codes: [],
    continuous_retry_error_codes: [],
    continuous_retry_max_duration_seconds: 600,
    codex_fingerprint_default_mode: 'off',
    allow_remote_migration: false,
    database_driver: 'postgres',
    database_label: 'PostgreSQL',
    cache_driver: 'redis',
    cache_label: 'Redis',
    response_cache_local_max_bytes: DEFAULT_RESPONSE_CACHE_TOTAL_BYTES,
    response_cache_local_max_entry_bytes: DEFAULT_RESPONSE_CACHE_ENTRY_BYTES,
    response_cache_reconstruct_max_bytes: DEFAULT_RESPONSE_CACHE_RECONSTRUCT_BYTES,
    response_cache_write_policy: 'always',
    response_cache_config_generation: 0,
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 262144,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2097152,
    output_guard_max_request_bytes: 4194304,
    output_guard_config_generation: 0,
    // The settings type still carries the relay cooldown fields until the final cleanup slice removes them.
    relay_model_cooldown_mode: 'off',
    relay_model_cooldown_seconds: 2,
    relay_model_cooldown_backoff_enabled: false,
    oauth_model_cooldown_mode: 'adaptive',
    oauth_model_cooldown_seconds: 300,
    oauth_model_cooldown_backoff_enabled: true,
    oauth_unsupported_model_cooldown_seconds: 1800,
    model_mapping: '{}',
    codex_model_mapping: '{}',
    payload_rules: '{}',
    reasoning_effort_models: '[]',
    resin_url: '',
    resin_platform_name: '',
    prompt_filter_enabled: false,
    prompt_filter_mode: 'monitor',
    prompt_filter_threshold: 50,
    prompt_filter_strict_threshold: 90,
    prompt_filter_strict_terminal_enabled: false,
    prompt_filter_advanced_config: '{}',
    prompt_filter_log_matches: true,
    prompt_filter_max_text_length: 81920,
    prompt_filter_sensitive_words: '',
    prompt_filter_custom_patterns: '[]',
    prompt_filter_disabled_patterns: '[]',
    prompt_filter_review_enabled: false,
    prompt_filter_review_api_key: '',
    prompt_filter_review_api_key_configured: false,
    prompt_filter_review_base_url: 'https://api.openai.com',
    prompt_filter_review_model: 'omni-moderation-latest',
    prompt_filter_review_timeout_seconds: 10,
    prompt_filter_review_fail_closed: true,
    client_compat_mode: 'preserve',
    codex_min_cli_version: '0.160.0',
    codex_images_main_model: '',
    codex_cli_version_sync_enabled: true,
    codex_cli_version_sync_interval_hours: 12,
    codex_user_agent_config: '{}',
    usage_log_mode: 'full',
    usage_log_batch_size: 200,
    usage_log_flush_interval_seconds: 5,
    stream_flush_policy: 'immediate',
    stream_flush_interval_ms: 20,
    first_token_mode: 'loose',
    first_token_timeout_seconds: 0,
    first_token_excludes_ws_acquire: false,
    billing_tier_policy: 'actual',
    models_list_read_max_bytes: DEFAULT_MODELS_LIST_READ_MAX_BYTES,
    show_full_usage_numbers: false,
    show_upstream_model_mismatch: true,
    public_key_usage_page_enabled: true,
    public_image_studio_page_enabled: true,
    public_account_portal_page_enabled: false,
    image_storage_backend: 'local',
    image_s3_endpoint: '',
    image_s3_region: '',
    image_s3_bucket: '',
    image_s3_access_key: '',
    image_s3_secret_key: '',
    image_s3_prefix: '',
    image_s3_force_path_style: false,
    auto_pause_5h_threshold: 0,
    auto_pause_7d_threshold: 0,
    auto_pause_5h_guard_band_percent: 5,
    auto_pause_5h_guard_concurrency: 1,
    smart_pacing_enabled: false,
    smart_pacing_min_concurrency: 1,
    smart_pacing_windows: '5h,7d',
    ignore_usage_limit_status: false,
  })
  const continuousRetryStatusCodesText = (settingsForm.continuous_retry_status_codes ?? []).join(',')
  const continuousRetryErrorCodesText = (settingsForm.continuous_retry_error_codes ?? []).join(',')
  const continuousRetryFineControlsDisabled = !settingsForm.continuous_retry_enabled || settingsForm.continuous_retry_catch_all
  const [continuousRetryStatusCodesDraft, setContinuousRetryStatusCodesDraft] = useState(continuousRetryStatusCodesText)
  const [continuousRetryErrorCodesDraft, setContinuousRetryErrorCodesDraft] = useState(continuousRetryErrorCodesText)
  const lazyModeActive = settingsForm.lazy_mode
  const responseCacheBudget = responseCacheBudgetFromSettings(settingsForm)
  const [savingSettings, setSavingSettings] = useState(false)
  const [autoSaveStatus, setAutoSaveStatus] = useState<AutoSaveStatus>('idle')
  // Snapshot of the settings confirmed by the server, used to count how many manually saved fields are still unsaved. The auto-save path merges per key locally,
  // and must not overwrite the whole thing, otherwise one toggle auto-save would also mark other unsaved text edits as saved.
  const [persistedSettings, setPersistedSettings] = useState<SystemSettings | null>(null)
  const markPersisted = useCallback((patch: Partial<SystemSettings>) => {
    setPersistedSettings((current) => (current ? { ...current, ...patch } : current))
  }, [])
  const [responseCacheValidationError, setResponseCacheValidationError] = useState<ResponseCacheBudgetValidationError | null>(null)
  const responseCacheValidationMessage = responseCacheValidationError
    ? t(`settings.responseCache.validation.${responseCacheValidationError}`)
    : ''
  const outputGuardValidationMessage = useMemo(
    () => validateOutputGuardRanges({
      output_guard_max_tool_argument_bytes: settingsForm.output_guard_max_tool_argument_bytes,
      output_guard_max_tool_argument_deltas: settingsForm.output_guard_max_tool_argument_deltas,
      output_guard_max_channel_bytes: settingsForm.output_guard_max_channel_bytes,
      output_guard_max_request_bytes: settingsForm.output_guard_max_request_bytes,
    }) ?? validateOutputGuardHierarchy({
      output_guard_max_tool_argument_bytes: settingsForm.output_guard_max_tool_argument_bytes,
      output_guard_max_channel_bytes: settingsForm.output_guard_max_channel_bytes,
      output_guard_max_request_bytes: settingsForm.output_guard_max_request_bytes,
    }),
    [
      settingsForm.output_guard_max_tool_argument_bytes,
      settingsForm.output_guard_max_tool_argument_deltas,
      settingsForm.output_guard_max_channel_bytes,
      settingsForm.output_guard_max_request_bytes,
    ],
  )
  const [testingImageStorage, setTestingImageStorage] = useState(false)
  const [loadedAdminSecret, setLoadedAdminSecret] = useState('')
  const [modelList, setModelList] = useState<string[]>([])
  const [modelItems, setModelItems] = useState<ModelInfo[]>([])
  const [modelsLastSyncedAt, setModelsLastSyncedAt] = useState<string | undefined>()
  const [modelsSourceURL, setModelsSourceURL] = useState('')
  const [syncingModels, setSyncingModels] = useState(false)
  const [syncingCliVersion, setSyncingCliVersion] = useState(false)
  // GitHub token is write-only and never echoed back: its draft state is independent of settingsForm and is cleared after submit (issue #522)
  const [githubTokenDraft, setGithubTokenDraft] = useState('')
  const [syncedCliVersion, setSyncedCliVersion] = useState('')
  // The version actually used for the outbound UA (the larger of the built-in and synced versions). The "set to synced version" button uses it as the reference, so when the synced value is expired or empty, the threshold is never lowered.
  const [effectiveCliVersion, setEffectiveCliVersion] = useState('')
  const [syncedAppBuilds, setSyncedAppBuilds] = useState({ desktop_mac: '', desktop_windows: '', vscode: '' })
  const [clientSyncErrors, setClientSyncErrors] = useState<Partial<Record<CodexClientSyncSource, string>>>({})
  const [codexClientVersions, setCodexClientVersions] = useState<CodexClientVersionTarget[]>([])
  const logoFileInputRef = useRef<HTMLInputElement>(null)
  const backgroundFileInputRef = useRef<HTMLInputElement>(null)
  const persistedBrandingRef = useRef<Partial<SiteBranding> | null>(null)
  const settingsFormRef = useRef(settingsForm)
  // Snapshot of the settings last confirmed by the server (updated after a successful first load or a successful auto/manual save).
  // Typed fields are already synced into settingsFormRef before the blur commit, so if auto-save fails and rollback uses that value,
  // the draft that the server never accepted stays on screen; rollback must use this snapshot.
  const confirmedSettingsRef = useRef<SystemSettings | null>(null)
  const confirmedFieldVersionsRef = useRef<Record<string, number>>({})
  const autoSavePendingCountRef = useRef(0)
  const autoSaveFieldVersionsRef = useRef<Record<string, number>>({})
  const autoSaveStatusTimerRef = useRef<number | null>(null)
  const continuousRetrySaveQueueRef = useRef(createContinuousRetrySaveQueue())
  const { toast, showToast } = useToast()
  // The invite guide toggle uses its own endpoint (system_settings.invite_guide_config) and is not part of the main settings
  // form: that UPSERT's placeholder is already at $119, and every added column would shift them all.
  // null means not loaded yet; the toggle does not render at that point, to avoid briefly flashing a wrong default state.
  const [inviteGuideEnabled, setInviteGuideEnabled] = useState<boolean | null>(null)

  useEffect(() => {
    let cancelled = false
    void api.getInviteGuideSettings()
      .then((res) => { if (!cancelled) setInviteGuideEnabled(res.enabled) })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [])

  // Optimistic update + rollback on failure: the toggle is independent local state, so a failed write must return to the real value,
  // otherwise the UI shows it on while the backend is actually off.
  const saveInviteGuideEnabled = async (next: boolean) => {
    const previous = inviteGuideEnabled
    setInviteGuideEnabled(next)
    try {
      await api.updateInviteGuideSettings(next)
      showToast(t('settings.autoSaved'), 'success', AUTO_SAVE_TOAST_MS)
    } catch (error) {
      setInviteGuideEnabled(previous)
      showToast(getErrorMessage(error), 'error')
    }
  }

  useEffect(() => {
    settingsFormRef.current = settingsForm
  }, [settingsForm])

  useEffect(() => {
    setContinuousRetryStatusCodesDraft(continuousRetryStatusCodesText)
  }, [continuousRetryStatusCodesText])

  useEffect(() => {
    setContinuousRetryErrorCodesDraft(continuousRetryErrorCodesText)
  }, [continuousRetryErrorCodesText])

  useEffect(() => {
    return () => {
      if (autoSaveStatusTimerRef.current) {
        window.clearTimeout(autoSaveStatusTimerRef.current)
      }
    }
  }, [])

  const commitSettingsForm = useCallback(
    (next: SystemSettings) => {
      const normalized = normalizeLazySettingsForm(next)
      settingsFormRef.current = normalized
      setSettingsForm(normalized)
      return normalized
    },
    [normalizeLazySettingsForm],
  )

  const scheduleAutoSaveStatusReset = useCallback(() => {
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
    }
    autoSaveStatusTimerRef.current = window.setTimeout(() => {
      setAutoSaveStatus((status) => (status === 'saved' ? 'idle' : status))
      autoSaveStatusTimerRef.current = null
    }, AUTO_SAVE_STATUS_RESET_MS)
  }, [])

  const finishAutoSaveRequest = useCallback((status: Exclude<AutoSaveStatus, 'idle' | 'saving'>) => {
    autoSavePendingCountRef.current = Math.max(0, autoSavePendingCountRef.current - 1)
    if (autoSavePendingCountRef.current > 0) {
      setAutoSaveStatus('saving')
      return
    }
    setAutoSaveStatus(status)
    if (status === 'saved') {
      scheduleAutoSaveStatusReset()
    }
  }, [scheduleAutoSaveStatusReset])

  const autoSaveSettingsPatch = useCallback(async (patch: Partial<SystemSettings>) => {
    const patchKeys = Object.keys(patch) as Array<keyof SystemSettings>
    if (patchKeys.length === 0) return

    const previous = settingsFormRef.current
    const optimistic = commitSettingsForm({
      ...previous,
      ...patch,
    })
    markPersisted(getSettingsPatchValues(optimistic, patchKeys))
    const requestedVersions: Record<string, number> = {}

    for (const key of patchKeys) {
      const fieldKey = String(key)
      const nextVersion = (autoSaveFieldVersionsRef.current[fieldKey] ?? 0) + 1
      autoSaveFieldVersionsRef.current[fieldKey] = nextVersion
      requestedVersions[fieldKey] = nextVersion
    }

    autoSavePendingCountRef.current += 1
    if (autoSaveStatusTimerRef.current) {
      window.clearTimeout(autoSaveStatusTimerRef.current)
      autoSaveStatusTimerRef.current = null
    }
    setAutoSaveStatus('saving')

    try {
      const updated = await api.updateSettings(getSettingsPatchValues(optimistic, patchKeys))
      // The fields submitted in this request are confirmed by the server; record them in the snapshot so that the "last server-confirmed
      // settings" can serve as a reliable rollback target for later failures.
      // Check the version for each field: if a response for a newer version of the same field has already arrived, an older response must not overwrite the new baseline.
      const mergedConfirmedState = mergeConfirmedSettingsWithVersions(
        confirmedSettingsRef.current,
        confirmedFieldVersionsRef.current,
        getSettingsPatchValues(updated, patchKeys),
        requestedVersions,
      )
      const confirmedWithGeneration = mergedConfirmedState.confirmed && typeof updated.output_guard_config_generation === 'number'
        ? {
            ...mergedConfirmedState.confirmed,
            output_guard_config_generation: Math.max(
              mergedConfirmedState.confirmed.output_guard_config_generation || 0,
              updated.output_guard_config_generation,
            ),
          }
        : mergedConfirmedState.confirmed
      confirmedSettingsRef.current = confirmedWithGeneration
      confirmedFieldVersionsRef.current = mergedConfirmedState.confirmedVersions
      const mergeKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      const currentSettings = settingsFormRef.current
      const responseCacheRequest = patchKeys.some(isResponseCacheBudgetKey)
      const mergedResponseCacheGeneration = responseCacheRequest
        ? mergeResponseCacheGeneration(
            currentSettings.response_cache_config_generation,
            updated.response_cache_config_generation,
          )
        : currentSettings.response_cache_config_generation
      const outputGuardRequest = patchKeys.some(
        (key) => OUTPUT_GUARD_KEYS.includes(key as (typeof OUTPUT_GUARD_KEYS)[number]),
      )
      const mergedOutputGuardGeneration = outputGuardRequest && typeof updated.output_guard_config_generation === 'number'
        ? Math.max(currentSettings.output_guard_config_generation || 0, updated.output_guard_config_generation)
        : currentSettings.output_guard_config_generation
      if (
        mergeKeys.length > 0
        || mergedResponseCacheGeneration !== currentSettings.response_cache_config_generation
        || mergedOutputGuardGeneration !== currentSettings.output_guard_config_generation
      ) {
        commitSettingsForm({
          ...currentSettings,
          ...getSettingsPatchValues(updated, mergeKeys),
          ...(responseCacheRequest
            ? { response_cache_config_generation: mergedResponseCacheGeneration }
            : {}),
          ...(outputGuardRequest
            ? { output_guard_config_generation: mergedOutputGuardGeneration }
            : {}),
        })
        markPersisted(getSettingsPatchValues(updated, mergeKeys))
      }
      const autoSaveSuccessMessage = updated.expired_cleaned && updated.expired_cleaned > 0
        ? `${t('settings.autoSaved')} · ${t('settings.expiredCleanedResult', { count: updated.expired_cleaned })}`
        : t('settings.autoSaved')
      showToast(autoSaveSuccessMessage, 'success', AUTO_SAVE_TOAST_MS)
      finishAutoSaveRequest('saved')
    } catch (error) {
      const rollbackKeys = patchKeys.filter((key) => {
        const fieldKey = String(key)
        return autoSaveFieldVersionsRef.current[fieldKey] === requestedVersions[fieldKey]
      })
      if (rollbackKeys.length > 0) {
        // Roll back to the last server-confirmed value, not the form value from before the submit: typed fields in blur
        // have already written the draft into settingsFormRef before the submit, and that value was never accepted by the server.
        const rollbackValues = resolveAutoSaveRollbackValues(
          confirmedSettingsRef.current,
          previous,
          rollbackKeys,
        )
        commitSettingsForm({
          ...settingsFormRef.current,
          ...rollbackValues,
        })
        markPersisted(rollbackValues)
      }
      const message = getErrorMessage(error)
      showToast(`${t('settings.autoSaveFailed')}: ${message}`, 'error')
      finishAutoSaveRequest('error')
    }
  }, [commitSettingsForm, finishAutoSaveRequest, showToast, t])

  // Blur commit for a byte-limit input: when the displayed unit value has not changed, no write happens, and the persisted
  // raw byte value is kept, so a non-round value such as 131073 is not rounded to 131072. The baseline comes from the persisted snapshot
  // rather than the current form value, because while typing the form has already become the converted new byte value.
  const commitByteLimitField = useCallback((
    field: ByteLimitField,
    submittedUnits: number,
    bytesPerUnit: number,
  ) => {
    const decision = resolveByteLimitCommit(
      persistedSettings?.[field],
      submittedUnits,
      bytesPerUnit,
    )
    if (!decision.submit) {
      if (settingsFormRef.current[field] !== decision.bytes) {
        commitSettingsForm({ ...settingsFormRef.current, [field]: decision.bytes })
      }
      return
    }
    void autoSaveSettingsPatch({ [field]: decision.bytes })
  }, [autoSaveSettingsPatch, commitSettingsForm, persistedSettings])

  const autoSaveContinuousRetryPatch = useCallback((patch: Partial<SystemSettings>) => {
    return continuousRetrySaveQueueRef.current(() => autoSaveSettingsPatch(patch))
  }, [autoSaveSettingsPatch])

  const autoSaveBooleanField = useCallback((field: keyof SystemSettings, value: boolean, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value,
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  const autoSaveStringField = useCallback((field: keyof SystemSettings, value: string, extraPatch: Partial<SystemSettings> = {}) => {
    void autoSaveSettingsPatch({
      ...extraPatch,
      [field]: value,
    } as Partial<SystemSettings>)
  }, [autoSaveSettingsPatch])

  const updateResponseCacheBudget = (
    field: keyof ResponseCacheBudgetMiB,
    value: number,
  ) => {
    const next = {
      ...responseCacheBudgetFromSettings(settingsFormRef.current),
      [field]: value,
    }
    setResponseCacheValidationError(validateResponseCacheBudget(next))
    commitSettingsForm({
      ...settingsFormRef.current,
      ...responseCacheBudgetFieldPatch(field, value),
    })
  }

  const commitResponseCacheBudget = (
    field: keyof ResponseCacheBudgetMiB,
    value: number,
  ) => {
    const next = {
      ...responseCacheBudgetFromSettings(settingsFormRef.current),
      [field]: value,
    }
    const validationError = validateResponseCacheBudget(next)
    setResponseCacheValidationError(validationError)
    if (validationError) {
      showToast(t(`settings.responseCache.validation.${validationError}`), 'error')
      return
    }
    void autoSaveSettingsPatch(buildResponseCacheBudgetPatch(next))
  }

  const loadSettingsData = useCallback(async () => {
    const [health, settings, modelsResp] = await Promise.all([api.getHealth(), api.getSettings(), api.getModels()])
    setPersistedSettings(commitSettingsForm(settings))
    // commitSettingsForm has already synced the normalized load result into settingsFormRef.
    confirmedSettingsRef.current = settingsFormRef.current
    const branding = {
      site_name: settings.site_name,
      site_logo: settings.site_logo,
      background_image: settings.background_image,
      background_opacity: settings.background_opacity,
      background_blur: settings.background_blur,
      background_glass_opacity: settings.background_glass_opacity,
      background_glass_blur: settings.background_glass_blur,
    }
    persistedBrandingRef.current = branding
    applyBranding(branding)
    setLoadedAdminSecret(settings.admin_secret ?? '')
    setSyncedCliVersion(settings.codex_synced_cli_version ?? '')
    setEffectiveCliVersion(settings.codex_effective_cli_version ?? '')
    setSyncedAppBuilds({
      desktop_mac: settings.codex_synced_desktop_mac_build ?? '',
      desktop_windows: settings.codex_synced_desktop_windows_build ?? '',
      vscode: settings.codex_synced_vscode_build ?? '',
    })
    setCodexClientVersions(settings.codex_client_versions ?? [])
    setModelList(modelsResp.models ?? [])
    setModelItems(modelsResp.items ?? [])
    setModelsLastSyncedAt(modelsResp.last_synced_at)
    setModelsSourceURL(modelsResp.source_url ?? '')
    return {
      health,
    }
  }, [applyBranding, commitSettingsForm])

  const { data, loading, error, reload } = useDataLoader<{
    health: HealthResponse | null
  }>({
    initialData: {
      health: null,
    },
    load: loadSettingsData,
  })

  const handleSaveSettings = async () => {
    const normalized = normalizeLazySettingsForm(settingsForm)
    const validationError = validateResponseCacheBudget(responseCacheBudgetFromSettings(normalized))
    setResponseCacheValidationError(validationError)
    if (validationError) {
      showToast(t(`settings.responseCache.validation.${validationError}`), 'error')
      return
    }
    setSavingSettings(true)
    try {
      const adminSecretChanged = settingsForm.admin_auth_source !== 'env' && settingsForm.admin_secret !== loadedAdminSecret
	      const payload: Partial<SystemSettings> = omitOutputGuardFields(buildWritableSettingsPayload(normalized))
      // Custom prompt rules are saved separately on the rules page, so an ordinary settings submit does not overwrite concurrent publish results.
      delete payload.prompt_filter_custom_patterns
      // Runaway-protection fields go through auto-save only: a full-form submit would send the stale snapshot read when the page was opened manually
      // together with the new generation, and could overwrite a guard that another admin just changed or even just turned off.
      const updated = await api.updateSettings(payload)
      // After a successful manual save the form is updated from the server response, but the current local guard fields must be kept,
      // so that new guard values that took effect via autosave during or before the save are not reverted by the old guard fields in the manual response.
      const safeMerged = preserveOutputGuardFields(updated, settingsFormRef.current)
      setPersistedSettings(commitSettingsForm(safeMerged))
      // A manual save also refreshes the "last server-confirmed" snapshot, so a later auto-save failure will not
      // roll back to the old values from before this manual save. Here only the non-guard fields are copied from the form into the confirmed snapshot;
      // if the generation in the response is greater than the confirmed snapshot's, the server's guard fields and generation are adopted as well,
      // otherwise the guard fields already in the confirmed snapshot are kept, so unconfirmed guard values from an in-flight autosave are not carried into the snapshot.
      confirmedSettingsRef.current = preserveConfirmedGuardFields(settingsFormRef.current, confirmedSettingsRef.current, updated)
      const branding = {
        site_name: updated.site_name,
        site_logo: updated.site_logo,
        background_image: updated.background_image,
        background_opacity: updated.background_opacity,
        background_blur: updated.background_blur,
        background_glass_opacity: updated.background_glass_opacity,
        background_glass_blur: updated.background_glass_blur,
      }
      persistedBrandingRef.current = branding
      applyBranding(branding)
      setLoadedAdminSecret(updated.admin_secret ?? '')
      if (updated.admin_auth_source !== 'env') {
        setAdminKey(updated.admin_secret ?? '')
      }
      if (adminSecretChanged) {
        resetAdminAuthState()
        return
      }
      if (updated.expired_cleaned && updated.expired_cleaned > 0) {
        showToast(t('settings.expiredCleanedResult', { count: updated.expired_cleaned }))
      } else {
        showToast(t('settings.saveSuccess'))
      }
    } catch (error) {
      showToast(`${t('settings.saveFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSavingSettings(false)
    }
  }

  useEffect(() => {
    if (!persistedBrandingRef.current) return
    applyBranding({
      site_name: settingsForm.site_name,
      site_logo: settingsForm.site_logo,
      background_image: settingsForm.background_image,
      background_opacity: settingsForm.background_opacity,
      background_blur: settingsForm.background_blur,
      background_glass_opacity: settingsForm.background_glass_opacity,
      background_glass_blur: settingsForm.background_glass_blur,
    })
  }, [
    applyBranding,
    settingsForm.site_name,
    settingsForm.site_logo,
    settingsForm.background_image,
    settingsForm.background_opacity,
    settingsForm.background_blur,
    settingsForm.background_glass_opacity,
    settingsForm.background_glass_blur,
  ])

  useEffect(() => {
    return () => {
      if (persistedBrandingRef.current) {
        applyBranding(persistedBrandingRef.current)
      }
    }
  }, [applyBranding])

  const handleSiteLogoUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getSiteLogoMimeType(file)
    if (!mimeType) {
      showToast(t('settings.siteLogoInvalidType'), 'error')
      return
    }

    try {
      const result = file.size <= SITE_LOGO_MAX_BYTES
        ? await readFileAsDataURL(file)
        : await compressSiteLogoFile(file, mimeType)
      if (dataURLByteLength(result) > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoTooLarge'), 'error')
        return
      }
      setSettingsForm((f) => ({ ...f, site_logo: result }))
      if (file.size > SITE_LOGO_MAX_BYTES) {
        showToast(t('settings.siteLogoCompressed', { size: formatBytesKB(dataURLByteLength(result)) }))
      }
    } catch {
      showToast(t('settings.siteLogoCompressionFailed'), 'error')
    }
  }

  const handleBackgroundImageUpload = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    const mimeType = getBackgroundImageMimeType(file)
    if (!mimeType) {
      showToast(t('settings.backgroundImageInvalidType'), 'error')
      return
    }
    const maxBytes = mimeType === 'video/mp4' ? BACKGROUND_VIDEO_UPLOAD_MAX_BYTES : BACKGROUND_IMAGE_UPLOAD_MAX_BYTES
    if (file.size > maxBytes) {
      showToast(t(mimeType === 'video/mp4' ? 'settings.backgroundVideoTooLarge' : 'settings.backgroundImageTooLarge'), 'error')
      return
    }

    try {
      const uploaded = await api.uploadBackground(file)
      setSettingsForm((f) => ({
        ...f,
        background_image: uploaded.url,
        background_opacity: f.background_opacity || 18,
      }))
      showToast(t('settings.backgroundImageUploaded'))
    } catch (err) {
      showToast(getErrorMessage(err) || t('settings.backgroundImageUploadFailed'), 'error')
    }
  }

  const handleTestImageStorage = async () => {
    setTestingImageStorage(true)
    try {
      const result = await api.testImageStorageConnection({
        endpoint: settingsForm.image_s3_endpoint,
        region: settingsForm.image_s3_region,
        bucket: settingsForm.image_s3_bucket,
        access_key: settingsForm.image_s3_access_key,
        secret_key: settingsForm.image_s3_secret_key,
        prefix: settingsForm.image_s3_prefix,
        force_path_style: settingsForm.image_s3_force_path_style,
      })
      showToast(t('settings.imageS3TestSuccess', { bucket: result.bucket }))
    } catch (error) {
      showToast(`${t('settings.imageS3TestFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setTestingImageStorage(false)
    }
  }

  const handleSyncCliVersion = async () => {
    setSyncingCliVersion(true)
    setClientSyncErrors({})
    try {
      const result = await api.syncCodexClientVersions()
      setSyncedCliVersion(result.cli.synced_version || '')
      setEffectiveCliVersion(result.cli.effective_version)
      setSyncedAppBuilds({
        desktop_mac: result.desktop_mac.effective_version,
        desktop_windows: result.desktop_windows.effective_version,
        vscode: result.vscode.effective_version,
      })
      const errors: Partial<Record<CodexClientSyncSource, string>> = {}
      for (const source of CODEX_CLIENT_SYNC_SOURCES) {
        const error = result[source.key].error
        if (error) errors[source.key] = error
      }
      setClientSyncErrors(errors)
      try {
        const settings = await api.getSettings()
        setCodexClientVersions(overlayCodexClientSyncFailures(settings.codex_client_versions ?? [], result))
      } catch {
        // The sync itself succeeded; the panel keeps the old data and refreshes it the next time settings load.
      }
      const failed = Object.keys(errors).length > 0
      showToast(failed ? t('settings.clientVersionSyncPartial') : t('settings.clientVersionSyncSuccess'), failed ? 'error' : 'success')
    } catch (error) {
      showToast(`${t('settings.cliVersionSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingCliVersion(false)
    }
  }

  const handleSyncModels = async () => {
    setSyncingModels(true)
    try {
      const result = await api.syncModels()
      setModelList(result.models ?? [])
      setModelItems(result.items ?? [])
      setModelsLastSyncedAt(result.last_synced_at)
      setModelsSourceURL(result.source_url ?? '')
      showToast(t('settings.modelsSyncSuccess', {
        added: result.added,
        updated: result.updated,
        skipped: result.skipped?.length ?? 0,
        removed: result.removed?.length ?? 0,
      }))
    } catch (error) {
      showToast(`${t('settings.modelsSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingModels(false)
    }
  }

  const { health } = data
  const isExternalDatabase = settingsForm.database_driver === 'postgres'
  const isExternalCache = settingsForm.cache_driver === 'redis'
  const showConnectionPool = isExternalDatabase || isExternalCache
  // Whether Resin is in effect is determined by the backend summary (refreshed with the response after saving), not guessed from unsaved drafts in the form.
  const codexEgress = persistedSettings?.codex_egress
  const resinActive = Boolean(codexEgress?.resin_enabled)
  const canConfigureRemoteMigration = settingsForm.admin_auth_source === 'env' || settingsForm.admin_secret.trim() !== ''
  const saveButtonLabel = savingSettings ? t('common.saving') : t('settings.saveSettings')
  const siteLogoPreview = sanitizeBrandingLogo(settingsForm.site_logo) || DEFAULT_SITE_LOGO
  const backgroundImagePreview = sanitizeBrandingImage(settingsForm.background_image)
  const backgroundIsVideo = isBrandingVideo(backgroundImagePreview)
  const visibleModelItems = useMemo(() => {
    if (modelItems.length > 0) {
      return modelItems
    }
    return modelList.map((id) => ({
      id,
      enabled: true,
      category: id.includes('image') ? 'image' : 'codex',
      source: 'builtin',
      pro_only: id === 'gpt-5.3-codex-spark',
      api_key_auth_available: !['gpt-5.5', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6-luna'].includes(id),
    }))
  }, [modelItems, modelList])
  const codexModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const textModelOptions = visibleModelItems
    .filter((model) =>
      model.enabled &&
      model.category !== 'image' &&
      !model.id.includes('image') &&
      !model.id.includes('(') &&
      !model.id.includes(')')
    )
    .map((model) => ({ label: model.id, value: model.id }))
  const imagesDefaultMainModel = settingsForm.codex_images_default_main_model || 'gpt-5.6-luna'
  const imagesMainModelOptions = [
    { value: '', label: t('settings.codexImagesDefault', { model: imagesDefaultMainModel }) },
    ...textModelOptions,
  ]
  if (settingsForm.codex_images_main_model && !imagesMainModelOptions.some((option) => option.value === settingsForm.codex_images_main_model)) {
    imagesMainModelOptions.push({ value: settingsForm.codex_images_main_model, label: settingsForm.codex_images_main_model })
  }
  const modelsLastSyncedLabel = modelsLastSyncedAt ? formatBeijingTime(modelsLastSyncedAt) : t('settings.modelsNeverSynced')
  const modelsSourceLabel = modelsSourceURL || 'https://developers.openai.com/codex/models'
  const codexMappingEntries = useMemo(
    () => parseModelMappingEntries(settingsForm.codex_model_mapping),
    [settingsForm.codex_model_mapping],
  )
  const reasoningEffortAliases = useMemo(
    () => parseReasoningEffortModelEntries(settingsForm.reasoning_effort_models).map(reasoningEffortAlias).filter(Boolean),
    [settingsForm.reasoning_effort_models],
  )
  const payloadRuleCount = useMemo(
    () => countPayloadRules(settingsForm.payload_rules),
    [settingsForm.payload_rules],
  )
  const payloadRuleGroupLabels = useMemo(() => {
    try {
      const parsed = JSON.parse(settingsForm.payload_rules || '{}') as Record<string, unknown>
      return PAYLOAD_RULE_GROUPS.flatMap((group) => {
        const rules = parsed[group]
        return Array.isArray(rules) && rules.length > 0 ? [`${group} ×${rules.length}`] : []
      })
    } catch {
      return []
    }
  }, [settingsForm.payload_rules])
  const showInitialSkeleton = loading && !health
  const codexUserAgentConfig = useMemo(
    () => parseCodexUserAgentConfig(settingsForm.codex_user_agent_config),
    [settingsForm.codex_user_agent_config],
  )
  const updateCodexUserAgentConfig = useCallback((patch: Partial<CodexUserAgentConfig>) => {
    setSettingsForm((form) => {
      const current = parseCodexUserAgentConfig(form.codex_user_agent_config)
      return {
        ...form,
        codex_user_agent_config: serializeCodexUserAgentConfig({ ...current, ...patch }),
      }
    })
  }, [])
  const saveCodexUserAgentConfig = useCallback(() => {
    void autoSaveSettingsPatch({ codex_user_agent_config: settingsForm.codex_user_agent_config })
  }, [autoSaveSettingsPatch, settingsForm.codex_user_agent_config])
  // Dropdown and segmented controls have no blur, so changes are written to the database right away; read the latest form from the ref to avoid stale values in the closure.
  const patchAndSaveCodexUserAgentConfig = useCallback((patch: Partial<CodexUserAgentConfig>) => {
    const current = parseCodexUserAgentConfig(settingsFormRef.current.codex_user_agent_config)
    void autoSaveSettingsPatch({ codex_user_agent_config: serializeCodexUserAgentConfig({ ...current, ...patch }) })
  }, [autoSaveSettingsPatch])
  // The shape catalog and the outbound identity preview are both computed by the backend; the frontend does not duplicate the UA assembly or version pairing rules.
  const [codexUACatalog, setCodexUACatalog] = useState<CodexUserAgentCatalog | null>(null)
  const [codexUAPreview, setCodexUAPreview] = useState<CodexUserAgentPreview | null>(null)
  const [codexUAPreviewError, setCodexUAPreviewError] = useState('')
  useEffect(() => {
    let cancelled = false
    api.getCodexUserAgentCatalog()
      .then((catalog) => { if (!cancelled) setCodexUACatalog(catalog) })
      .catch(() => { /* Failing to load the directory only affects preset candidates; the form can still be filled in freely */ })
    return () => { cancelled = true }
  }, [])
  useEffect(() => {
    let cancelled = false
    const timer = window.setTimeout(() => {
      api.previewCodexUserAgent({
        config: settingsForm.codex_user_agent_config,
        client_compat_mode: settingsForm.client_compat_mode,
        codex_min_cli_version: settingsForm.codex_min_cli_version,
      })
        .then((preview) => {
          if (cancelled) return
          setCodexUAPreview(preview)
          setCodexUAPreviewError('')
        })
        .catch((err: unknown) => {
          if (cancelled) return
          setCodexUAPreview(null)
          setCodexUAPreviewError(err instanceof Error ? err.message : String(err))
        })
    }, 250)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [settingsForm.codex_user_agent_config, settingsForm.client_compat_mode, settingsForm.codex_min_cli_version, codexClientVersions])
  const codexUAMode: 'single' | 'pool' = codexUserAgentConfig.mode === 'pool' ? 'pool' : 'single'
  const codexUAKind: CodexUAKind = CODEX_UA_KINDS.includes(codexUserAgentConfig.client_kind as CodexUAKind)
    ? (codexUserAgentConfig.client_kind as CodexUAKind)
    : inferCodexUAKind(codexUserAgentConfig.client_name)
  const codexUAKindSpec = codexUACatalog?.kinds.find((kind) => kind.kind === codexUAKind) ?? null
  const codexUAAppFollowsCLI = codexUAKindSpec ? codexUAKindSpec.app_follows_cli : codexUAKind === 'codex-tui' || codexUAKind === 'codex-exec'
  const codexUARawActive = Boolean(codexUserAgentConfig.raw_user_agent?.trim())
  const codexUADefaultPoolMix = codexUACatalog?.default_pool_mix ?? CODEX_UA_FALLBACK_POOL_MIX
  const codexUAKindLabel = useCallback((kind: CodexUAKind) => {
    switch (kind) {
      case 'codex-desktop': return t('settings.codexUAKindDesktop')
      case 'codex-vscode': return t('settings.codexUAKindVscode')
      case 'codex-exec': return t('settings.codexUAKindExec')
      case 'custom': return t('settings.codexUAKindCustom')
      default: return t('settings.codexUAKindTui')
    }
  }, [t])
  const codexUAKindOptions = useMemo(() => CODEX_UA_KINDS.map((kind) => ({ label: codexUAKindLabel(kind), value: kind })), [codexUAKindLabel])
  const codexUAModeOptions = useMemo(() => [
    { label: t('settings.codexUAModeSingle'), value: 'single' as const },
    { label: t('settings.codexUAModePool'), value: 'pool' as const },
  ], [t])
  const selectCodexUAKind = useCallback((kind: CodexUAKind) => {
    patchAndSaveCodexUserAgentConfig({
      client_kind: kind,
      client_name: '',
      client_version: '',
      app_name: '',
      app_version: '',
      os_name: '',
      os_version: '',
      arch: '',
      terminal: '',
    })
  }, [patchAndSaveCodexUserAgentConfig])
  const codexUAPlatformKey = (osName: string, osVersion: string, arch: string) => `${osName}|${osVersion}|${arch}`
  const codexUAPlatformOptions = useMemo(() => {
    const options = (codexUAKindSpec?.platforms ?? []).map((platform) => ({
      label: `${platform.os_name} ${platform.os_version} · ${platform.arch}`,
      value: codexUAPlatformKey(platform.os_name, platform.os_version, platform.arch),
    }))
    const referenceOptions = (codexUAKindSpec?.reference_platforms ?? []).map((platform) => {
      const name = `${platform.os_name} ${platform.os_version} · ${platform.arch}`
      return {
        label: `${name} · ${t('settings.codexUAReference')}`,
        value: codexUAPlatformKey(platform.os_name, platform.os_version, platform.arch),
        triggerLabel: name,
        content: (
          <span className="flex items-start gap-2">
            <span className="min-w-0 flex-1 break-all">{name}</span>
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium leading-none text-muted-foreground">
              {t('settings.codexUAReference')}
            </span>
          </span>
        ),
      }
    })
    return [...referenceOptions, ...options, { label: t('settings.codexUACustomOption'), value: 'custom' }]
  }, [codexUAKindSpec, t])
  const codexUAEffectivePlatform = {
    os_name: (codexUserAgentConfig.os_name ?? '').trim() || codexUAKindSpec?.default_platform.os_name || DEFAULT_CODEX_UA_CONFIG.os_name,
    os_version: (codexUserAgentConfig.os_version ?? '').trim() || codexUAKindSpec?.default_platform.os_version || DEFAULT_CODEX_UA_CONFIG.os_version,
    arch: (codexUserAgentConfig.arch ?? '').trim() || codexUAKindSpec?.default_platform.arch || DEFAULT_CODEX_UA_CONFIG.arch,
  }
  const codexUAPlatformPresetValue = (() => {
    const key = codexUAPlatformKey(codexUAEffectivePlatform.os_name, codexUAEffectivePlatform.os_version, codexUAEffectivePlatform.arch)
    return codexUAPlatformOptions.some((option) => option.value === key) ? key : 'custom'
  })()
  const applyCodexUAPlatformPreset = useCallback((value: string) => {
    if (value === 'custom') return
    const [os_name, os_version, arch] = value.split('|')
    patchAndSaveCodexUserAgentConfig({ os_name, os_version, arch })
  }, [patchAndSaveCodexUserAgentConfig])
  const codexUATerminalOptions = useMemo(() => [
    ...(codexUAKindSpec?.terminals ?? []).map((terminal) => ({ label: terminal.value, value: terminal.value })),
    ...(codexUAKindSpec?.reference_terminals ?? []).map((terminal) => ({
      label: `${terminal} · ${t('settings.codexUAReference')}`,
      value: terminal,
      triggerLabel: terminal,
      content: (
        <span className="flex items-start gap-2">
          <span className="min-w-0 flex-1 break-all">{terminal}</span>
          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium leading-none text-muted-foreground">
            {t('settings.codexUAReference')}
          </span>
        </span>
      ),
    })),
    { label: t('settings.codexUACustomOption'), value: 'custom' },
  ], [codexUAKindSpec, t])
  const codexUAEffectiveTerminal = (codexUserAgentConfig.terminal ?? '').trim() || codexUAKindSpec?.default_terminal || DEFAULT_CODEX_UA_CONFIG.terminal
  const codexUATerminalPresetValue = codexUATerminalOptions.some((option) => option.value === codexUAEffectiveTerminal && option.value !== 'custom') ? codexUAEffectiveTerminal : 'custom'
  const codexUAAppNameOptions = useMemo(() => [
    ...(codexUAKindSpec?.app_names ?? []).map((name) => ({ label: name.value, value: name.value })),
    { label: t('settings.codexUACustomOption'), value: 'custom' },
  ], [codexUAKindSpec, t])
  const codexUAEffectiveAppName = (codexUserAgentConfig.app_name ?? '').trim() || codexUAKindSpec?.default_app_name || (codexUserAgentConfig.client_name ?? '').trim() || DEFAULT_CODEX_UA_CONFIG.client_name
  const codexUAAppNamePresetValue = codexUAAppNameOptions.some((option) => option.value === codexUAEffectiveAppName && option.value !== 'custom') ? codexUAEffectiveAppName : 'custom'
  const codexUAShowAppNamePreset = codexUAKind !== 'custom' && !codexUAAppFollowsCLI && (codexUAKindSpec?.app_names?.length ?? 0) > 1
  // The default version comes from the backend preview, using the same pairing selector as outbound requests.
  const codexUAClientVersionPlaceholder = codexUAPreviewError
    ? t('settings.codexClientVersions.unavailable')
    : codexUAPreview?.persona?.version || t('settings.codexUAPreviewLoading')
  const codexUAAppVersionPlaceholder = codexUAPreviewError
    ? t('settings.codexClientVersions.unavailable')
    : codexUAPreview?.persona?.app_version || t('settings.codexUAAutoPaired')
  const codexUAPoolMixValue = (kind: CodexUAKind) => {
    const weight = codexUserAgentConfig.pool_mix?.[kind]
    return weight === undefined ? '' : String(weight)
  }
  const updateCodexUAPoolMix = useCallback((kind: CodexUAKind, text: string) => {
    const current = parseCodexUserAgentConfig(settingsFormRef.current.codex_user_agent_config)
    const parsed = Number(text)
    const weight = text.trim() === '' || !Number.isFinite(parsed) ? 0 : Math.max(0, Math.floor(parsed))
    // On first edit, fill in the other shapes from the default ratio, so that filling in one item does not zero out all the other shapes.
    updateCodexUserAgentConfig({ pool_mix: { ...codexUADefaultPoolMix, ...(current.pool_mix ?? {}), [kind]: weight } })
  }, [codexUADefaultPoolMix, updateCodexUserAgentConfig])
  const dirtyKeys = useMemo(() => {
    if (!persistedSettings) return [] as string[]
    const current = normalizeLazySettingsForm(settingsForm) as unknown as Record<string, unknown>
    const base = persistedSettings as unknown as Record<string, unknown>
    const keys = new Set([...Object.keys(current), ...Object.keys(base)])
    const changed: string[] = []
    for (const key of keys) {
      if (SETTINGS_DIRTY_IGNORED_KEYS.has(key)) continue
      if (!settingsValueEquals(current[key], base[key])) changed.push(key)
    }
    return changed
  }, [normalizeLazySettingsForm, persistedSettings, settingsForm])
  const dirtyCount = dirtyKeys.length
  const discardChanges = useCallback(() => {
    if (!persistedSettings) return
    commitSettingsForm(persistedSettings)
    setResponseCacheValidationError(null)
  }, [commitSettingsForm, persistedSettings])
  const dirtyKeySet = useMemo(() => new Set(dirtyKeys), [dirtyKeys])
  const discardSettingsField = useCallback((field: keyof SystemSettings) => {
    if (!persistedSettings) return
    setSettingsForm((form) => ({ ...form, [field]: persistedSettings[field] }))
  }, [persistedSettings])
  // The save button uses the primary color only when there are unsaved changes; it stays clickable even with no changes, so users can still force a save if the dirty check misses something.
  const renderSaveButton = (className?: string) => (
    <Button
      className={className}
      variant={dirtyCount > 0 ? 'default' : 'outline'}
      onClick={() => void handleSaveSettings()}
      disabled={savingSettings || autoSaveStatus === 'saving'}
    >
      <Save className="size-4" />
      {saveButtonLabel}
    </Button>
  )

  const settingsTabs = useMemo(
    () =>
      [
        { id: 'codex', label: t('settings.nav.codex'), icon: <ChannelLogo channel="codex" size={16} /> },
        { id: 'appearance', label: t('settings.nav.appearance'), icon: <Palette className="size-4" /> },
        { id: 'general', label: t('settings.nav.general'), icon: <SlidersHorizontal className="size-4" /> },
      ] as const satisfies ReadonlyArray<{ id: SettingsTabKey; label: string; icon: ReactNode }>,
    [t],
  )
  const [searchParams, setSearchParams] = useSearchParams()
  const location = useLocation()
  const tabParam = searchParams.get('tab')
  const activeTab: SettingsTabKey = isSettingsTabKey(tabParam) ? tabParam : DEFAULT_SETTINGS_TAB
  const [modelPanel, setModelPanel] = useState<ModelPanelKey | null>(null)
  const modelPanelMeta = (() => {
    switch (modelPanel) {
      case 'registry':
        return { title: t('settings.modelRegistry'), description: t('settings.modelRegistryDesc'), icon: <Layers />, count: visibleModelItems.length }
      case 'codex':
        return { title: t('settings2.codexModelMapping'), description: t('settings2.codexModelMappingDesc'), icon: <Shuffle />, count: codexMappingEntries.length }
      case 'reasoning':
        return { title: t('settings2.reasoningEffortModels'), description: t('settings2.reasoningEffortModelsDesc'), icon: <Brain />, count: reasoningEffortAliases.length }
      default:
        return null
    }
  })()
  const modelPanelField = modelPanel && modelPanel !== 'registry' ? MODEL_PANEL_FIELDS[modelPanel] : null
  const modelPanelDirty = modelPanelField ? dirtyKeySet.has(modelPanelField) : false
  const modelPanelOtherDirtyCount = dirtyCount - (modelPanelDirty ? 1 : 0)
  const settingsNavRef = useRef<HTMLElement | null>(null)

  // The section id to scroll to after switching tabs: the panel content only mounts on the next render, so the scroll action goes into an effect.
  const pendingSectionRef = useRef<string | null>(null)
  const selectTab = useCallback(
    (tab: SettingsTabKey, sectionId?: string) => {
      pendingSectionRef.current = sectionId ?? null
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.set('tab', tab)
          return next
        },
        { replace: true },
      )
      if (!sectionId) window.scrollTo({ top: 0 })
    },
    [setSearchParams],
  )
  useEffect(() => {
    const sectionId = pendingSectionRef.current
    if (!sectionId) return
    pendingSectionRef.current = null
    document.getElementById(sectionId)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [activeTab])

  // Compatibility for old #settings-xxx anchor deep links: on first entry, map the anchor to the matching tab.
  useEffect(() => {
    if (tabParam) return
    const legacy = LEGACY_SECTION_TABS[location.hash.replace(/^#/, '')]
    if (legacy) selectTab(legacy)
  }, [location.hash, selectTab, tabParam])

  // When the current tab changes, scroll its pill into view in the top bar (horizontal scrolling navigation on narrow screens).
  useEffect(() => {
    const nav = settingsNavRef.current
    if (!nav) return
    const btn = nav.querySelector<HTMLElement>(`[data-tab-id="${activeTab}"]`)
    btn?.scrollIntoView({ behavior: 'smooth', inline: 'center', block: 'nearest' })
  }, [activeTab])

  const sectionIndexItems = useMemo(
    () => SETTINGS_TAB_SECTION_INDEX[activeTab].map((item) => ({ id: item.id, label: t(item.labelKey), icon: item.icon })),
    [activeTab, t],
  )
  const sectionIds = useMemo(() => SETTINGS_TAB_SECTION_INDEX[activeTab].map((item) => item.id), [activeTab])
  const { activeId: activeSectionId, pinSection } = useActiveSettingsSection(sectionIds)
  const hasSectionIndex = sectionIndexItems.length > 1
  const jumpToSection = useCallback((sectionId: string) => {
    pinSection(sectionId)
    document.getElementById(sectionId)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }, [pinSection])

  if (showInitialSkeleton) {
    return <SettingsSkeleton />
  }

  return (
    <StateShell
      variant="page"
      loading={false}
      error={error && !health ? error : null}
      onRetry={() => void reload()}
      loadingTitle={t('settings.loadingTitle')}
      loadingDescription={t('settings.loadingDesc')}
      errorTitle={t('settings.errorTitle')}
    >
      <>
        <PageHeader
          title={t('settings.title')}
          description={t('settings.description')}
          actions={
            <>
              <SaveStatusPill autoSaveStatus={autoSaveStatus} dirtyCount={dirtyCount} />
              {renderSaveButton('shrink-0')}
            </>
          }
        />

        {/* The tab bar and section directory flow with the page and stick to the top when scrolling; they no longer use fixed positioning to float over content */}
        <div className="sticky top-2 z-30 mb-5 space-y-2.5 lg:top-3">
          <nav
            ref={settingsNavRef}
            role="tablist"
            aria-label={t('settings.navLabel')}
            className={cn(
              'flex min-w-0 items-center gap-1 overflow-x-auto rounded-full border border-border/70 bg-card/95 p-1 shadow-sm backdrop-blur-xl',
              '[-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
            )}
          >
            {settingsTabs.map((tab) => {
              const active = activeTab === tab.id
              return (
                <button
                  key={tab.id}
                  type="button"
                  role="tab"
                  data-tab-id={tab.id}
                  aria-selected={active}
                  aria-current={active ? 'true' : undefined}
                  onClick={() => selectTab(tab.id)}
                  className={cn(
                    'inline-flex shrink-0 items-center justify-center gap-2 rounded-full px-3.5 py-1.5 text-[13px] font-semibold tracking-tight transition-colors duration-200 sm:flex-1 sm:basis-0 sm:px-4 sm:py-1.5 sm:text-xs',
                    active
                      ? 'bg-primary text-primary-foreground shadow-2xs'
                      : 'text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                  )}
                >
                  <span
                    className={cn(
                      'shrink-0 [&_svg]:size-4 sm:[&_svg]:size-[1.05rem]',
                      active ? 'opacity-100' : 'opacity-75',
                    )}
                  >
                    {tab.icon}
                  </span>
                  <span className="whitespace-nowrap">{tab.label}</span>
                </button>
              )
            })}
          </nav>
          {hasSectionIndex ? (
            <SettingsSectionIndex
              items={sectionIndexItems}
              activeId={activeSectionId}
              label={t('settings.sectionIndex')}
              onSelect={jumpToSection}
            />
          ) : null}
        </div>

        <div key={activeTab} className="pb-4">
          <div className="min-w-0 space-y-7">
          {activeTab === 'codex' ? (
            <>
              <SettingsSection id="settings-codex-quota" title={t('settings.nav.codexQuota')} description={t('settings.nav.codexQuotaDesc')} icon={<Gauge className="size-4" />}>
              <div className={SETTINGS_CARD_GRID_2}>
                <SettingsCard title={t('settings.probeScheduling')} icon={<RefreshCw className="size-4" />}>
                  <div className="space-y-4">
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.backgroundRefreshInterval')} description={t('settings.backgroundRefreshIntervalDesc')} suffix={t('settings.unit.min')}>
                        <DraftNumberInput
                          min={1}
                          max={1440}
                          value={settingsForm.background_refresh_interval_minutes}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, background_refresh_interval_minutes: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.usageProbeMaxAge')} description={t('settings.usageProbeMaxAgeDesc')} suffix={t('settings.unit.min')}>
                        <DraftNumberInput
                          min={1}
                          max={10080}
                          value={settingsForm.usage_probe_max_age_minutes}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_probe_max_age_minutes: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.usageProbeConcurrency')} description={t('settings.usageProbeConcurrencyDesc')} suffix={t('settings.unit.concurrency')}>
                        <DraftNumberInput
                          min={1}
                          max={128}
                          value={settingsForm.usage_probe_concurrency}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_probe_concurrency: value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.recoveryProbeInterval')} description={t('settings.recoveryProbeIntervalDesc')}>
                        {lazyModeActive ? (
                          <Input value="∞" disabled />
                        ) : (
                          <DraftNumberInput
                            min={1}
                            max={10080}
                            value={settingsForm.recovery_probe_interval_minutes}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, recovery_probe_interval_minutes: value }))}
                          />
                        )}
                      </SettingField>
                    </div>
                    <div className={SETTINGS_SWITCH_GRID}>
                      <SettingField label={t('settings.usageProbeResponsesFallback')} description={t('settings.usageProbeResponsesFallbackDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.usage_probe_responses_fallback_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('usage_probe_responses_fallback_enabled', checked)}
                        />
                      </SettingField>
                      <SettingField label={t('settings.lazyMode')} description={t('settings.lazyModeDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.lazy_mode}
                          onCheckedChange={(enabled) => {
                            void autoSaveSettingsPatch({
                              lazy_mode: enabled,
                              auto_clean_full_usage: enabled ? false : settingsFormRef.current.auto_clean_full_usage,
                            })
                          }}
                        />
                      </SettingField>
                      <SettingField label={t('settings.codexOAuthKeepalive')} description={t('settings.codexOAuthKeepaliveDesc')} layout="switch">
                        <Switch
                          aria-label={t('settings.codexOAuthKeepalive')}
                          checked={settingsForm.codex_oauth_keepalive_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('codex_oauth_keepalive_enabled', checked)}
                        />
                      </SettingField>
                      {inviteGuideEnabled !== null && (
                        <SettingField
                          label={t('settings.inviteGuide')}
                          description={t('settings.inviteGuideDesc')}
                          layout="switch"
                        >
                          <Switch
                            aria-label={t('settings.inviteGuide')}
                            checked={inviteGuideEnabled}
                            onCheckedChange={(checked) => void saveInviteGuideEnabled(checked)}
                          />
                        </SettingField>
                      )}
                    </div>
                  </div>
                </SettingsCard>
              <SettingsCard
                title={t('settings.connectivityTest')}
                description={t('settings.connectivityTestDesc')}
                icon={<Wifi className="size-4" />}
                className="h-full"
                contentClassName="flex h-full flex-col"
              >
                <div className="flex flex-1 flex-col gap-4">
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.testModelLabel')} description={t('settings.testModelHint')}>
                      <Select
                        value={settingsForm.test_model}
                        onValueChange={(value) => autoSaveStringField('test_model', value)}
                        options={textModelOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.testConcurrency')} description={t('settings.testConcurrencyRange')}>
                      <DraftNumberInput
                        min={1}
                        max={200}
                        value={settingsForm.test_concurrency}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, test_concurrency: value }))}
                      />
                    </SettingField>
                  </div>
                  <SettingField label={t('settings.testContent')} description={t('settings.testContentDesc')} stretch>
                    <textarea
                      rows={3}
                      value={settingsForm.test_content}
                      placeholder={t('settings.testContentPlaceholder')}
                      onChange={(e: ChangeEvent<HTMLTextAreaElement>) => setSettingsForm(f => ({ ...f, test_content: e.target.value }))}
                      onBlur={(e) => autoSaveStringField('test_content', e.currentTarget.value)}
                      className={cn(
                        'flex min-h-[88px] w-full resize-y rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground shadow-xs transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50',
                      )}
                    />
                  </SettingField>
                </div>
              </SettingsCard>
              </div>

              <SettingsCard
                title={t('settings.autoResetCreditsTitle')}
                description={t('settings.autoResetCreditsDesc')}
                icon={<RefreshCw className="size-4" />}
              >
                <div className={cn(SETTINGS_SWITCH_GRID, 'items-stretch')}>
                  <SettingField
                    label={t('settings.autoResetCreditsOnExhaustionEnabled')}
                    description={t('settings.autoResetCreditsOnExhaustionDesc')}
                    layout="switch"
                    className="h-full"
                  >
                    <Switch
                      checked={settingsForm.auto_reset_credits_on_exhaustion_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_reset_credits_on_exhaustion_enabled', checked)}
                    />
                  </SettingField>
                  <SettingField
                    label={t('settings.autoResetCreditsEnabled')}
                    description={t('settings.autoResetCreditsEnabledDesc')}
                    layout="switch"
                    className="h-full"
                  >
                    <Switch
                      checked={settingsForm.auto_reset_credits_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField(
                        'auto_reset_credits_enabled',
                        checked,
                        checked
                          ? { auto_reset_credits_before_expiry_min: settingsFormRef.current.auto_reset_credits_before_expiry_min }
                          : {},
                      )}
                    />
                  </SettingField>
                  <div className="flex min-h-[48px] min-w-0 items-center justify-between gap-3 rounded-lg border border-border/60 bg-muted/20 px-3 py-2.5">
                    <div className="min-w-0 flex-1 space-y-0.5">
                      <div className="flex items-center gap-1.5">
                        <label className="block text-[13px] font-medium leading-snug text-foreground sm:text-sm">
                          {t('settings.autoResetCreditsBeforeExpiry')}
                        </label>
                        <SettingHelp text={t('settings.autoResetCreditsBeforeExpiryDesc')} />
                      </div>
                    </div>
                    <div className="relative w-[7.5rem] shrink-0 sm:w-[8.5rem]">
                      <DraftNumberInput
                        min={10}
                        max={10080}
                        step={10}
                        className="pr-11"
                        value={settingsForm.auto_reset_credits_before_expiry_min}
                        onValueChange={(value) => {
                          commitSettingsForm({
                            ...settingsFormRef.current,
                            auto_reset_credits_before_expiry_min: value,
                          })
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            auto_reset_credits_before_expiry_min: value,
                          })
                        }}
                      />
                      <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium tabular-nums text-muted-foreground">
                        {t('settings.unit.min')}
                      </span>
                    </div>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard
                title={t('settings.autoActivate5hTitle')}
                description={t('settings.autoActivate5hDesc')}
                icon={<Timer className="size-4" />}
              >
                <SettingField
                  label={t('settings.autoActivate5hEnabled')}
                  description={t('settings.autoActivate5hEnabledDesc')}
                  layout="switch"
                >
                  <Switch
                    checked={Boolean(settingsForm.auto_activate_5h_window_enabled)}
                    onCheckedChange={(checked) => autoSaveBooleanField('auto_activate_5h_window_enabled', checked)}
                  />
                </SettingField>
              </SettingsCard>

              <SettingsCard title={t('settings.globalAutoPauseTitle')} description={t('settings.globalAutoPauseDesc')} icon={<Activity className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.globalAutoPause5h')} description={t('settings.globalAutoPauseHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.globalAutoPausePlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_5h_threshold * 100}
                        formatValue={(value) => value > 0 ? value.toFixed(1).replace(/\.0$/, '') : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_threshold: value / 100 }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_threshold: value / 100 })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.globalAutoPause7d')} description={t('settings.globalAutoPauseHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.globalAutoPausePlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_7d_threshold * 100}
                        formatValue={(value) => value > 0 ? value.toFixed(1).replace(/\.0$/, '') : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_7d_threshold: value / 100 }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_7d_threshold: value / 100 })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.autoPause5hGuardBand')} description={t('settings.autoPause5hGuardBandHint')}>
                      <DraftNumberInput
                        min={0}
                        max={100}
                        step={0.1}
                        inputMode="decimal"
                        placeholder={t('settings.autoPause5hGuardBandPlaceholder')}
                        integer={false}
                        emptyValue={0}
                        value={settingsForm.auto_pause_5h_guard_band_percent}
                        formatValue={(value) => value > 0 ? String(value) : ''}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_guard_band_percent: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_guard_band_percent: value })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.autoPause5hGuardConcurrency')} description={t('settings.autoPause5hGuardConcurrencyHint')}>
                      <DraftNumberInput
                        min={0}
                        max={1000}
                        step={1}
                        inputMode="numeric"
                        value={settingsForm.auto_pause_5h_guard_concurrency ?? 1}
                        emptyValue={0}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, auto_pause_5h_guard_concurrency: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ auto_pause_5h_guard_concurrency: value })
                        }}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingWindows')} description={t('settings.smartPacingWindowsHint')}>
                      <Select
                        value={settingsForm.smart_pacing_windows || '5h,7d'}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, smart_pacing_windows: value }))
                          void autoSaveSettingsPatch({ smart_pacing_windows: value })
                        }}
                        options={[
                          { value: '5h,7d', label: t('settings.smartPacingWindowsBoth') },
                          { value: '5h', label: t('settings.smartPacingWindows5h') },
                          { value: '7d', label: t('settings.smartPacingWindows7d') },
                        ]}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingMinConcurrency')} description={t('settings.smartPacingMinConcurrencyHint')}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        step={1}
                        inputMode="numeric"
                        value={settingsForm.smart_pacing_min_concurrency ?? 1}
                        onValueChange={(value) => {
                          setSettingsForm(f => ({ ...f, smart_pacing_min_concurrency: value }))
                        }}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ smart_pacing_min_concurrency: value })
                        }}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_GRID}>
                    <SettingField
                      label={t('settings.ignoreUsageLimitStatus')}
                      description={t('settings.ignoreUsageLimitStatusHint')}
                      layout="switch"
                    >
                      <Switch
                        checked={settingsForm.ignore_usage_limit_status}
                        onCheckedChange={(checked) => autoSaveBooleanField('ignore_usage_limit_status', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.smartPacingEnabled')} description={t('settings.smartPacingEnabledHint')} layout="switch">
                      <Switch
                        checked={settingsForm.smart_pacing_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('smart_pacing_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              {/* The scheduling policy is shared across channels and belongs in general settings; this is only a pointer, so users do not miss it on the Codex page. */}
              <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-dashed border-border bg-muted/20 px-4 py-3">
                <div className="flex min-w-0 items-center gap-3">
                  <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-muted/70 text-muted-foreground ring-1 ring-border/60">
                    <Layers className="size-4" />
                  </div>
                  <div className="min-w-0">
                    <div className="text-sm font-semibold text-foreground">{t('settings.codexSchedulingHintTitle')}</div>
                    <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{t('settings.codexSchedulingHintDesc')}</p>
                  </div>
                </div>
                <Button size="sm" variant="outline" onClick={() => selectTab('general', 'settings-traffic')}>
                  <ChevronRight className="size-3.5" />
                  {t('settings.codexSchedulingHintAction')}
                </Button>
              </div>
              </SettingsSection>

              <SettingsSection id="settings-codex-transport" title={t('settings.nav.codexTransport')} description={t('settings.nav.codexTransportDesc')} icon={<Wifi className="size-4" />}>
              <SettingsCard title={t('settings.codexWebsocket')} description={t('settings.codexWebsocketDesc')} icon={<Wifi className="size-4" />}>
                <div className="space-y-4">
                  <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
                    <SettingField label={t('settings.codexForceWebsocket')} description={t('settings.codexForceWebsocketDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_force_websocket}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_force_websocket', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexRequestCompression')} description={t('settings.codexRequestCompressionDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_request_compression}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_request_compression', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSWeakNetworkMode')} description={t('settings.codexWSWeakNetworkModeDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_weak_network_mode}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_weak_network_mode', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSKeepaliveEnabled')} description={t('settings.codexWSKeepaliveEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_keepalive_enabled}
                        disabled={settingsForm.codex_ws_weak_network_mode}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_keepalive_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSHideUpstreamErrors')} description={t('settings.codexWSHideUpstreamErrorsDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_hide_upstream_errors}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_hide_upstream_errors', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSSilentRetryEnabled')} description={t('settings.codexWSSilentRetryEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_silent_retry_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_silent_retry_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSSizeRouterEnabled')} description={t('settings.codexWSSizeRouterEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_size_router_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_size_router_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexWSBusyOverflowEnabled')} description={t('settings.codexWSBusyOverflowEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_ws_busy_overflow_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_ws_busy_overflow_enabled', checked)}
                      />
                    </SettingField>
                  </div>

                  <div className={cn(SETTINGS_FIELD_GRID, 'border-t border-border/80 pt-4')}>
                    <SettingField
                      label={t('settings.codexWSKeepaliveInterval')}
                      description={t('settings.codexWSKeepaliveIntervalDesc')}
                      suffix={t('settings.unit.sec')}
                      className={cn((!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode) && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={10}
                        max={600}
                        disabled={!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode}
                        value={settingsForm.codex_ws_keepalive_interval_sec}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_keepalive_interval_sec: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_keepalive_enabled || settingsForm.codex_ws_weak_network_mode) return
                          void autoSaveSettingsPatch({
                            codex_ws_keepalive_interval_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSSilentMaxRetries')}
                      description={t('settings.codexWSSilentMaxRetriesDesc')}
                      suffix={t('settings.unit.times')}
                      className={cn(!settingsForm.codex_ws_silent_retry_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={10}
                        disabled={!settingsForm.codex_ws_silent_retry_enabled}
                        value={settingsForm.codex_ws_silent_max_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_silent_max_retries: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_silent_retry_enabled) return
                          void autoSaveSettingsPatch({
                            codex_ws_silent_max_retries: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSBusyAcquireMaxWait')}
                      description={t('settings.codexWSBusyAcquireMaxWaitDesc')}
                      suffix={t('settings.unit.sec')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={300}
                        value={settingsForm.codex_ws_busy_acquire_max_wait_sec}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_busy_acquire_max_wait_sec: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            codex_ws_busy_acquire_max_wait_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSBusyPatience')}
                      description={t('settings.codexWSBusyPatienceDesc')}
                      suffix={t('settings.unit.sec')}
                      className={cn(!settingsForm.codex_ws_busy_overflow_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={300}
                        disabled={!settingsForm.codex_ws_busy_overflow_enabled}
                        value={settingsForm.codex_ws_busy_patience_sec}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_busy_patience_sec: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_ws_busy_overflow_enabled) return
                          void autoSaveSettingsPatch({
                            codex_ws_busy_patience_sec: value,
                          })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSDownstreamKeepaliveSlots')}
                      description={t('settings.codexWSDownstreamKeepaliveSlotsDesc')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={32}
                        value={settingsForm.codex_ws_downstream_keepalive_slots}
                        emptyValue={8}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_downstream_keepalive_slots: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_ws_downstream_keepalive_slots: value })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexWSStatelessSlots')}
                      description={t('settings.codexWSStatelessSlotsDesc')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={32}
                        value={settingsForm.codex_ws_stateless_slots}
                        emptyValue={8}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_ws_stateless_slots: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({
                            codex_ws_stateless_slots: value,
                          })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexContinueThinking')} description={t('settings.codexContinueThinkingDesc')} icon={<Brain className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.codexContinueThinking')} description={t('settings.codexContinueThinkingDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_continue_thinking_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_continue_thinking_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField
                      label={t('settings.codexContinueMaxRounds')}
                      description={t('settings.codexContinueMaxRoundsDesc')}
                      className={cn(!settingsForm.codex_continue_thinking_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={32}
                        disabled={!settingsForm.codex_continue_thinking_enabled}
                        value={settingsForm.codex_continue_max_rounds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_continue_max_rounds: value }))}
                        onValueCommit={(value) => {
                          if (!settingsForm.codex_continue_thinking_enabled) return
                          void autoSaveSettingsPatch({
                            codex_continue_max_rounds: value,
                          })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              {/* The three compatibility items that each have only one switch are merged into one card, one row each, with descriptions shown directly; split into three narrow cards, the switches would be squeezed to half width and wrap. */}
              <SettingsCard title={t('settings.codexCompatToggles')} description={t('settings.codexCompatTogglesDesc')} icon={<Layers className="size-4" />}>
                <div className={SETTINGS_ROW_LIST}>
                  <SettingField
                    label={t('settings.overflowAutoCompact')}
                    description={t('settings.overflowAutoCompactDesc')}
                    help={t('settings.overflowAutoCompactEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.overflowAutoCompactEnabled')}
                      checked={settingsForm.overflow_auto_compact_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('overflow_auto_compact_enabled', checked)}
                    />
                  </SettingField>
                  <SettingField
                    label={t('settings.compactViaResponses')}
                    description={t('settings.compactViaResponsesDesc')}
                    help={t('settings.compactViaResponsesEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.compactViaResponsesEnabled')}
                      checked={settingsForm.compact_via_responses_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('compact_via_responses_enabled', checked)}
                    />
                  </SettingField>
                  <SettingField
                    label={t('settings.codexPreflightSSEPassthrough')}
                    description={t('settings.codexPreflightSSEPassthroughDesc')}
                    help={t('settings.codexPreflightSSEPassthroughEnabledDesc')}
                    layout="row"
                  >
                    <Switch
                      aria-label={t('settings.codexPreflightSSEPassthroughEnabled')}
                      checked={settingsForm.codex_preflight_sse_passthrough_enabled}
                      onCheckedChange={(checked) => autoSaveBooleanField('codex_preflight_sse_passthrough_enabled', checked)}
                    />
                  </SettingField>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexOverloadPause')} description={t('settings.codexOverloadPauseDesc')} icon={<ShieldAlert className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.codexOverloadPauseEnabled')} description={t('settings.codexOverloadPauseEnabledDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.codex_overload_pause_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_overload_pause_enabled', checked)}
                      />
                    </SettingField>
                  </div>
                  <div className={cn(SETTINGS_FIELD_GRID_3, !settingsForm.codex_overload_pause_enabled && 'opacity-60')}>
                    <SettingField
                      label={t('settings.codexOverloadThreshold')}
                      description={t('settings.codexOverloadThresholdDesc')}
                      suffix="%"
                    >
                      <DraftNumberInput
                        min={1}
                        max={100}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_threshold_percent}
                        emptyValue={20}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_threshold_percent: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_threshold_percent: value })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexOverloadPauseMinutes')}
                      description={t('settings.codexOverloadPauseMinutesDesc')}
                      suffix={t('settings.unit.min')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={1440}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_pause_minutes}
                        emptyValue={30}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_pause_minutes: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_pause_minutes: value })
                        }}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexOverloadWindow')}
                      description={t('settings.codexOverloadWindowDesc')}
                      suffix={t('settings.unit.min')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={120}
                        disabled={!settingsForm.codex_overload_pause_enabled}
                        value={settingsForm.codex_overload_window_minutes}
                        emptyValue={5}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, codex_overload_window_minutes: value }))}
                        onValueCommit={(value) => {
                          void autoSaveSettingsPatch({ codex_overload_window_minutes: value })
                        }}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard
                title={t('settings.responseCache.title')}
                description={t('settings.responseCache.description')}
                icon={<Database className="size-4" />}
                badge={
                  <Badge variant="outline" className="text-[11px] tabular-nums">
                    {settingsForm.response_cache_config_generation > 0
                      ? t('settings.responseCache.generation', { value: settingsForm.response_cache_config_generation })
                      : t('settings.responseCache.generationPending')}
                  </Badge>
                }
              >
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField
                      label={t('settings.responseCache.total')}
                      description={t('settings.responseCache.totalDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.totalMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('totalMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('totalMiB', value)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.responseCache.entry')}
                      description={t('settings.responseCache.entryDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.entryMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('entryMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('entryMiB', value)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.responseCache.reconstruct')}
                      description={t('settings.responseCache.reconstructDesc')}
                      suffix="MiB"
                    >
                      <DraftNumberInput
                        step={1}
                        integer={true}
                        value={responseCacheBudget.reconstructMiB}
                        aria-invalid={Boolean(responseCacheValidationError)}
                        onValueChange={(value) => updateResponseCacheBudget('reconstructMiB', value)}
                        onValueCommit={(value) => commitResponseCacheBudget('reconstructMiB', value)}
                      />
                    </SettingField>
                  </div>
                  {responseCacheValidationMessage ? (
                    <p role="alert" className="text-xs font-medium text-destructive">
                      {responseCacheValidationMessage}
                    </p>
                  ) : null}

                  <SettingField
                    label={t('settings.responseCache.writePolicy')}
                    description={t('settings.responseCache.writePolicyDesc')}
                  >
                    <SegmentedPillGroup
                      value={settingsForm.response_cache_write_policy}
                      onChange={(value) => autoSaveStringField('response_cache_write_policy', value)}
                      options={responseCacheWritePolicyOptions}
                    />
                  </SettingField>

                  {/* Visual budget allocation ratio bar (Memory Allocation Bar) */}
                  <div className="rounded-xl border border-border/70 bg-muted/20 p-3.5 space-y-2.5">
                    <div className="flex items-center justify-between text-xs font-semibold">
                      <span className="text-foreground">Memory cache allocation preview (Memory Allocation)</span>
                      <span className="font-mono text-primary font-bold">{responseCacheBudget.totalMiB} MiB</span>
                    </div>
                    <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-muted shadow-inner">
                      <div
                        className="bg-primary transition-all duration-300"
                        style={{ width: `${Math.min(100, Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100))}%` }}
                      />
                      <div
                        className="bg-amber-500 transition-all duration-300"
                        style={{ width: `${Math.min(100 - Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100), Math.round((responseCacheBudget.reconstructMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100))}%` }}
                      />
                      <div
                        className="bg-emerald-500/40 flex-1 transition-all duration-300"
                      />
                    </div>
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[11px] text-muted-foreground">
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-primary" />
                        <span>{t('settings.responseCache.entry')}: <strong className="text-foreground font-mono">{responseCacheBudget.entryMiB} MiB</strong> ({Math.round((responseCacheBudget.entryMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100)}%)</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-amber-500" />
                        <span>{t('settings.responseCache.reconstruct')}: <strong className="text-foreground font-mono">{responseCacheBudget.reconstructMiB} MiB</strong> ({Math.round((responseCacheBudget.reconstructMiB / Math.max(1, responseCacheBudget.totalMiB)) * 100)}%)</span>
                      </div>
                      <div className="flex items-center gap-1.5">
                        <span className="size-2 rounded-full bg-emerald-500/60" />
                        <span>Resident hot pool: <strong className="text-foreground font-mono">{Math.max(0, responseCacheBudget.totalMiB - responseCacheBudget.entryMiB - responseCacheBudget.reconstructMiB)} MiB</strong></span>
                      </div>
                    </div>
                  </div>

                  <div className="rounded-lg border border-primary/15 bg-primary/5 px-3.5 py-3 text-xs leading-relaxed text-muted-foreground">
                    <p>{t('settings.responseCache.l1Note')}</p>
                    <p className="mt-1.5">{t('settings.responseCache.memoryNote')}</p>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-codex-client" title={t('settings.nav.codexClient')} description={t('settings.nav.codexClientDesc')} icon={<Terminal className="size-4" />}>
              <SettingsCard title={t('settings.codexClientVersionCardTitle')} description={t('settings.codexClientVersionCardDesc')} icon={<RefreshCw className="size-4" />}>
                <div className="space-y-4">
                  <div className="overflow-hidden rounded-xl border border-border/60">
                    <div className="grid grid-cols-2 gap-px bg-border/60 lg:grid-cols-4">
                      {CODEX_CLIENT_SYNC_SOURCES.map((source) => {
                        const version = source.key === 'cli' ? effectiveCliVersion : syncedAppBuilds[source.key]
                        const error = clientSyncErrors[source.key]
                        return (
                          <div key={source.key} className="min-w-0 bg-card px-3.5 py-3">
                            <div className="flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
                              <span
                                aria-hidden="true"
                                className={cn('size-1.5 shrink-0 rounded-full', error ? 'bg-destructive' : version ? 'bg-emerald-500' : 'bg-muted-foreground/40')}
                              />
                              <span className="truncate">{source.label}</span>
                            </div>
                            <div
                              className={cn(
                                'mt-1 truncate font-mono text-sm font-semibold tabular-nums',
                                version ? 'text-foreground' : 'text-muted-foreground',
                                syncingCliVersion && 'motion-safe:animate-pulse',
                              )}
                            >
                              {version || t('settings.clientVersionNotSynced')}
                            </div>
                            {error ? (
                              <p className="mt-1 line-clamp-2 break-all text-[11px] leading-snug text-destructive" title={error}>
                                {t('settings.clientVersionSyncKept')} · {error}
                              </p>
                            ) : null}
                          </div>
                        )
                      })}
                    </div>
                    <div className="flex flex-col gap-3 border-t border-border/60 bg-muted/20 px-3.5 py-2.5 sm:flex-row sm:items-center sm:justify-between">
                      <div className="flex flex-wrap items-center gap-x-6 gap-y-2.5">
                        <div className="flex items-center gap-2">
                          <label className="flex cursor-pointer items-center gap-2">
                            <Switch
                              checked={settingsForm.codex_cli_version_sync_enabled}
                              onCheckedChange={(checked) => autoSaveBooleanField('codex_cli_version_sync_enabled', checked)}
                            />
                            <span className="text-[13px] font-medium text-foreground">{t('settings.codexCliVersionAutoSync')}</span>
                          </label>
                          <SettingHelp text={t('settings.codexCliVersionAutoSyncDesc')} />
                        </div>
                        <div className={cn('flex items-center gap-2 transition-opacity', !settingsForm.codex_cli_version_sync_enabled && 'opacity-50')}>
                          <span className="text-[13px] text-muted-foreground">{t('settings.codexCliVersionSyncInterval')}</span>
                          <SettingHelp text={t('settings.codexCliVersionSyncIntervalDesc')} />
                          <div className="relative w-[6.5rem] shrink-0">
                            <DraftNumberInput
                              min={1}
                              max={720}
                              aria-label={t('settings.codexCliVersionSyncInterval')}
                              className="h-8 pr-10 tabular-nums"
                              disabled={!settingsForm.codex_cli_version_sync_enabled}
                              value={settingsForm.codex_cli_version_sync_interval_hours}
                              onValueChange={(value) =>
                                setSettingsForm((f) => ({
                                  ...f,
                                  codex_cli_version_sync_interval_hours: value,
                                }))
                              }
                              onValueCommit={(value) => {
                                if (!settingsForm.codex_cli_version_sync_enabled) return
                                void autoSaveSettingsPatch({
                                  codex_cli_version_sync_interval_hours: value,
                                })
                              }}
                            />
                            <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium text-muted-foreground">
                              {t('settings.unit.hour')}
                            </span>
                          </div>
                        </div>
                      </div>
                      <Button size="sm" variant="outline" className="shrink-0 self-start sm:self-auto" onClick={() => void handleSyncCliVersion()} disabled={syncingCliVersion}>
                        <RefreshCw className={cn('size-3.5', syncingCliVersion && 'animate-spin')} />
                        {syncingCliVersion ? t('settings.cliVersionSyncing') : t('settings.cliVersionSyncNow')}
                      </Button>
                    </div>
                  </div>
                  <div className="rounded-lg border border-border/60 p-3">
                    <CodexClientVersionsPanel targets={codexClientVersions} />
                  </div>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.clientCompatMode')} description={t('settings.clientCompatModeDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.client_compat_mode}
                        onChange={(value) => autoSaveStringField('client_compat_mode', value)}
                        options={clientCompatOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.codexMinCliVersion')} description={t('settings.codexMinCliVersionDesc')}>
                      <div className="flex items-center gap-2">
                        <Input
                          className="min-w-0 flex-1 font-mono tabular-nums"
                          value={settingsForm.codex_min_cli_version}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, codex_min_cli_version: e.target.value }))}
                        />
                        {/* One click aligns the threshold to the CLI version currently synced; it only changes the form value and is saved with "Save settings" */}
                        <Button
                          size="sm"
                          variant="outline"
                          className="shrink-0"
                          disabled={!effectiveCliVersion || settingsForm.codex_min_cli_version.trim() === effectiveCliVersion}
                          title={effectiveCliVersion ? t('settings.codexMinCliVersionUseSyncedDesc', { version: effectiveCliVersion }) : t('settings.codexMinCliVersionNoSynced')}
                          onClick={() => setSettingsForm(f => ({ ...f, codex_min_cli_version: effectiveCliVersion }))}
                        >
                          {t('settings.codexMinCliVersionUseSynced')}
                        </Button>
                      </div>
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexConnectionCardTitle')} description={t('settings.codexConnectionCardDesc')} icon={<Fingerprint className="size-4" />}>
                <div className={SETTINGS_FIELD_GRID}>
                  <SettingField label={t('settings.codexFingerprintDefaultMode')} description={t('settings.codexFingerprintDefaultModeDesc')}>
                    <Select
                      value={settingsForm.codex_fingerprint_default_mode || 'off'}
                      onValueChange={(value) => autoSaveStringField('codex_fingerprint_default_mode', value)}
                      options={codexFingerprintDefaultModeOptions}
                    />
                  </SettingField>
                  <SettingField label={t('settings.utlsShutdownTimeout')} description={t('settings.utlsShutdownTimeoutDesc')} suffix={t('settings.unit.min')}>
                    <DraftNumberInput
                      min={1}
                      max={240}
                      className="tabular-nums"
                      value={settingsForm.utls_shutdown_timeout_minutes}
                      onValueChange={(value) => setSettingsForm(f => ({ ...f, utls_shutdown_timeout_minutes: value }))}
                      onValueCommit={(value) => {
                        void autoSaveSettingsPatch({
                          utls_shutdown_timeout_minutes: value,
                        })
                      }}
                    />
                  </SettingField>
                </div>
                <div className="mt-4 border-t border-border/60 pt-4">
                  <div className={SETTINGS_ROW_LIST}>
                    <SettingField
                      label={t('settings.codexTelemetry')}
                      description={t('settings.codexTelemetryDesc')}
                      layout="row"
                    >
                      <Switch
                        aria-label={t('settings.codexTelemetry')}
                        checked={settingsForm.codex_telemetry_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_telemetry_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.codexTelemetryTiming')}
                      description={t('settings.codexTelemetryTimingDesc')}
                      layout="row"
                    >
                      <Switch
                        aria-label={t('settings.codexTelemetryTiming')}
                        checked={settingsForm.codex_telemetry_timing_debug}
                        onCheckedChange={(checked) => autoSaveBooleanField('codex_telemetry_timing_debug', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.codexClientTitle')} description={t('settings.codexClientDesc')} icon={<Terminal className="size-4" />}>
                <div className="space-y-4">
                  <div className="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,15rem)_minmax(0,1fr)]">
                    <SettingField label={t('settings.codexUAMode')} description={t('settings.codexUAModeDesc')}>
                      <SegmentedPillGroup
                        value={codexUAMode}
                        onChange={(value) => patchAndSaveCodexUserAgentConfig({ mode: value === 'pool' ? 'pool' : '' })}
                        options={codexUAModeOptions}
                      />
                    </SettingField>
                    {codexUAMode === 'pool' ? null : (
                      <SettingField label={t('settings.codexUAKind')} description={t('settings.codexUAKindDesc')}>
                        <div className="overflow-x-auto">
                          <SegmentedPillGroup
                            className="min-w-max"
                            value={codexUAKind}
                            onChange={selectCodexUAKind}
                            options={codexUAKindOptions}
                          />
                        </div>
                      </SettingField>
                    )}
                  </div>
                  {codexUAMode === 'pool' ? (
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
                      {CODEX_UA_POOL_KINDS.map((kind) => (
                        <SettingField key={kind} label={`${t('settings.codexUAPoolMix')} · ${codexUAKindLabel(kind)}`} description={t('settings.codexUAPoolMixDesc')}>
                          <Input
                            type="number"
                            min={0}
                            step={1}
                            inputMode="numeric"
                            className="tabular-nums"
                            value={codexUAPoolMixValue(kind)}
                            placeholder={String(codexUADefaultPoolMix[kind] ?? 0)}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUAPoolMix(kind, e.target.value)}
                            onBlur={saveCodexUserAgentConfig}
                          />
                        </SettingField>
                      ))}
                    </div>
                  ) : null}
                  <div className="min-w-0 rounded-xl border border-primary/15 bg-primary/[0.03] px-3.5 py-3">
                    <div className="mb-2 flex items-center gap-1.5 text-[13px] font-semibold text-foreground">
                      <Eye className="size-3.5 text-primary" aria-hidden="true" />
                      {codexUAMode === 'pool' ? t('settings.codexUAPoolPreview') : t('settings.codexUAPreview')}
                    </div>
                    {codexUAPreviewError ? (
                      <div className="break-all text-[11px] leading-5 text-destructive">{codexUAPreviewError}</div>
                    ) : !codexUAPreview ? (
                      <div className="text-[11px] leading-5 text-muted-foreground">{t('settings.codexUAPreviewLoading')}</div>
                    ) : codexUAPreview.persona ? (
                      <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 font-mono text-[11px] leading-5 text-muted-foreground sm:text-xs">
                        <dt className="text-foreground/70">User-Agent</dt>
                        <dd className="break-all text-foreground">{codexUAPreview.persona.user_agent}</dd>
                        <dt className="text-foreground/70">Originator</dt>
                        <dd className="break-all">{codexUAPreview.persona.originator}</dd>
                        <dt className="text-foreground/70">Version</dt>
                        <dd className="break-all">{codexUAPreview.persona.version}</dd>
                        <dt className="text-foreground/70">{t('settings.codexClientVersions.source')}</dt>
                        <dd>{t(`settings.codexClientVersions.sources.${codexUAPreview.persona.source ?? 'builtin_observed'}`, { defaultValue: codexUAPreview.persona.source ?? 'builtin_observed' })}</dd>
                      </dl>
                    ) : (
                      <ul className="space-y-1 font-mono text-[11px] leading-5 text-muted-foreground sm:text-xs">
                        {(codexUAPreview.samples ?? []).map((sample) => (
                          <li key={`${sample.label}-${sample.account_id ?? 0}`} className="break-all">
                            <span className="text-foreground/70">{sample.label}{sample.account_id ? ` · ${sample.account_id}` : ''}</span>
                            {' '}<span className="text-foreground">{sample.user_agent}</span>
                            <span className="text-foreground/50">{' · '}{sample.originator}</span>
                          </li>
                        ))}
                      </ul>
                    )}
                    {codexUAPreview?.warnings?.length ? (
                      <div className="mt-2 flex items-start gap-1.5 text-[11px] leading-5 text-amber-600 dark:text-amber-400">
                        <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                        <span>{t('settings.codexUAWarnUnseen', { fields: codexUAPreview.warnings.map((field) => t(`settings.codexUAWarn_${field}`)).join(' / ') })}</span>
                      </div>
                    ) : null}
                  </div>
                  {codexUAMode === 'pool' ? null : (
                    <>
                      <SettingField label={t('settings.codexUserAgentRaw')} description={t('settings.codexUserAgentRawDesc')}>
                        <Input
                          className="font-mono text-xs"
                          value={codexUserAgentConfig.raw_user_agent ?? ''}
                          placeholder="codex-tui/0.160.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.160.0)"
                          onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ raw_user_agent: e.target.value })}
                          onBlur={saveCodexUserAgentConfig}
                        />
                      </SettingField>
                      {codexUARawActive ? (
                        <p className="-mt-1.5 text-[11px] leading-relaxed text-amber-600 dark:text-amber-400 sm:text-xs">
                          {t('settings.codexUARawActiveHint')}
                        </p>
                      ) : null}
                      <div className={cn('divide-y divide-border/60 rounded-xl border border-border/60 transition-opacity', codexUARawActive && 'opacity-60')}>
                        <SettingsFieldGroup title={t('settings.codexUAGroupClient')} hint="name/version">
                          <SettingField label={t('settings.codexUAClientName')} description={t('settings.codexUAClientNameDesc')}>
                            <Input
                              value={codexUserAgentConfig.client_name ?? ''}
                              placeholder={codexUAKindSpec?.client_name ?? DEFAULT_CODEX_UA_CONFIG.client_name}
                              disabled={codexUAKind !== 'custom'}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_name: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                          <SettingField label={t('settings.codexUAClientVersion')} description={t('settings.codexUAClientVersionDesc')}>
                            <Input
                              value={codexUserAgentConfig.client_version ?? ''}
                              placeholder={codexUAClientVersionPlaceholder}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ client_version: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                        </SettingsFieldGroup>
                        <SettingsFieldGroup title={t('settings.codexUAGroupPlatform')} hint="(os version; arch)">
                          <SettingField label={t('settings.codexUAPlatformPreset')} description={t('settings.codexUAPlatformPresetDesc')}>
                            <Select
                              value={codexUAPlatformPresetValue}
                              onValueChange={applyCodexUAPlatformPreset}
                              options={codexUAPlatformOptions}
                            />
                          </SettingField>
                          <SettingField label={t('settings.codexUAOSName')} description={t('settings.codexUAOSNameDesc')}>
                            <Input
                              value={codexUserAgentConfig.os_name ?? ''}
                              placeholder={codexUAKindSpec?.default_platform.os_name ?? DEFAULT_CODEX_UA_CONFIG.os_name}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_name: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                          <SettingField label={t('settings.codexUAOSVersion')} description={t('settings.codexUAOSVersionDesc')}>
                            <Input
                              value={codexUserAgentConfig.os_version ?? ''}
                              placeholder={codexUAKindSpec?.default_platform.os_version ?? DEFAULT_CODEX_UA_CONFIG.os_version}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ os_version: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                          <SettingField label={t('settings.codexUAArch')} description={t('settings.codexUAArchDesc')}>
                            <Input
                              value={codexUserAgentConfig.arch ?? ''}
                              placeholder={codexUAKindSpec?.default_platform.arch ?? DEFAULT_CODEX_UA_CONFIG.arch}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ arch: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                        </SettingsFieldGroup>
                        <SettingsFieldGroup title={t('settings.codexUAGroupTerminal')} hint="TERM_PROGRAM">
                          <SettingField label={t('settings.codexUATerminalPreset')} description={t('settings.codexUATerminalPresetDesc')}>
                            <Select
                              value={codexUATerminalPresetValue}
                              onValueChange={(value) => { if (value !== 'custom') patchAndSaveCodexUserAgentConfig({ terminal: value }) }}
                              options={codexUATerminalOptions}
                            />
                          </SettingField>
                          <SettingField label={t('settings.codexUATerminal')} description={t('settings.codexUATerminalDesc')}>
                            <Input
                              value={codexUserAgentConfig.terminal ?? ''}
                              placeholder={codexUAKindSpec?.default_terminal ?? DEFAULT_CODEX_UA_CONFIG.terminal}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ terminal: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                        </SettingsFieldGroup>
                        <SettingsFieldGroup title={t('settings.codexUAGroupApp')} hint="(app; version)">
                          <SettingField label={t('settings.codexUAAppName')} description={t('settings.codexUAAppNameDesc')}>
                            <div className="space-y-2">
                              {codexUAShowAppNamePreset ? (
                                <Select
                                  value={codexUAAppNamePresetValue}
                                  onValueChange={(value) => { if (value !== 'custom') patchAndSaveCodexUserAgentConfig({ app_name: value }) }}
                                  options={codexUAAppNameOptions}
                                />
                              ) : null}
                              <Input
                                value={codexUserAgentConfig.app_name ?? ''}
                                placeholder={codexUAAppFollowsCLI ? t('settings.codexUAFollowsClient') : codexUAEffectiveAppName}
                                disabled={codexUAAppFollowsCLI || (codexUAKind === 'codex-desktop')}
                                onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ app_name: e.target.value })}
                                onBlur={saveCodexUserAgentConfig}
                              />
                            </div>
                          </SettingField>
                          <SettingField label={t('settings.codexUAAppVersion')} description={t('settings.codexUAAppVersionDesc')}>
                            <Input
                              value={codexUserAgentConfig.app_version ?? ''}
                              placeholder={codexUAAppVersionPlaceholder}
                              disabled={codexUAAppFollowsCLI}
                              onChange={(e: ChangeEvent<HTMLInputElement>) => updateCodexUserAgentConfig({ app_version: e.target.value })}
                              onBlur={saveCodexUserAgentConfig}
                            />
                          </SettingField>
                        </SettingsFieldGroup>
                      </div>
                    </>
                  )}
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-codex-images" title={t('settings.nav.codexImages')} description={t('settings.nav.codexImagesDesc')} icon={<ImageIcon className="size-4" />}>
                <SettingsCard title={t('settings.codexImagesDriver')} description={t('settings.codexImagesDriverDesc')} icon={<ImageIcon className="size-4" />}>
                  <div className="space-y-4">
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.codexImagesMainModel')} description={t('settings.codexImagesMainModelDesc')}>
                        <Select
                          value={settingsForm.codex_images_main_model}
                          options={imagesMainModelOptions.filter(({ value }) => !HIDDEN_IMAGE_DRIVER_MODELS.has(value.toLowerCase()))}
                          onValueChange={(value) => autoSaveStringField('codex_images_main_model', value)}
                        />
                      </SettingField>
                    </div>
                    <div className="rounded-lg border border-border/60 bg-muted/20 px-3.5 py-3 text-xs leading-relaxed text-muted-foreground">
                      <p>{t('settings.codexImagesScope')}</p>
                      <p className="mt-1.5">{t('settings.codexImagesFallback')}</p>
                    </div>
                  </div>
                </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-models" title={t('settings.nav.models')} description={t('settings.nav.modelsDesc')} icon={<Layers className="size-4" />}>
                <ModelRegistryOverview
                  items={visibleModelItems}
                  lastSyncedLabel={modelsLastSyncedLabel}
                  sourceUrl={modelsSourceLabel}
                  syncing={syncingModels}
                  onSync={() => void handleSyncModels()}
                  onOpen={() => setModelPanel('registry')}
                />
                <div className="space-y-2.5">
                  <div className="px-0.5 text-[11px] font-semibold uppercase tracking-wider text-muted-foreground/90">
                    {t('settings.modelRulesTitle')}
                  </div>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <ModelRuleCard
                      icon={<Shuffle />}
                      title={t('settings2.codexModelMapping')}
                      description={t('settings2.codexModelMappingDesc')}
                      count={codexMappingEntries.length}
                      preview={<ModelMappingPreview entries={codexMappingEntries} />}
                      dirty={dirtyKeySet.has(MODEL_PANEL_FIELDS.codex)}
                      onOpen={() => setModelPanel('codex')}
                    />
                    <ModelRuleCard
                      icon={<Brain />}
                      title={t('settings2.reasoningEffortModels')}
                      description={t('settings2.reasoningEffortModelsDesc')}
                      count={reasoningEffortAliases.length}
                      preview={<ModelChipPreview labels={reasoningEffortAliases.slice(0, MODEL_REGISTRY_PREVIEW_LIMIT)} />}
                      dirty={dirtyKeySet.has(MODEL_PANEL_FIELDS.reasoning)}
                      onOpen={() => setModelPanel('reasoning')}
                    />
                    <ModelRuleCard
                      icon={<Braces />}
                      title={t('settings2.payloadRules')}
                      description={t('settings2.payloadRulesDesc')}
                      count={payloadRuleCount}
                      preview={<ModelChipPreview labels={payloadRuleGroupLabels} />}
                      external
                      onOpen={() => navigate('/payload-rules')}
                    />
                  </div>
                </div>

                <Sheet open={modelPanel !== null} onOpenChange={(open) => { if (!open) setModelPanel(null) }}>
                  <SheetContent
                    side="right"
                    className="sm:w-[min(calc(100%-2rem),720px)] sm:max-w-[min(calc(100%-2rem),720px)]"
                    onOpenAutoFocus={(event) => {
                      event.preventDefault()
                      ;(event.currentTarget as HTMLElement | null)?.focus()
                    }}
                  >
                    {modelPanelMeta ? (
                      <SheetHeader>
                        <div className="flex items-start gap-3">
                          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-inset ring-primary/20 [&_svg]:size-4">
                            {modelPanelMeta.icon}
                          </div>
                          <div className="min-w-0 flex-1 space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                              <SheetTitle>{modelPanelMeta.title}</SheetTitle>
                              <Badge variant="secondary" className="tabular-nums">{modelPanelMeta.count}</Badge>
                            </div>
                            <SheetDescription className="text-xs leading-relaxed">{modelPanelMeta.description}</SheetDescription>
                          </div>
                        </div>
                      </SheetHeader>
                    ) : null}
                    <SheetBody className="space-y-4">
                      {modelPanel === 'registry' ? (
                        <ModelRegistryPanel
                          items={visibleModelItems}
                          lastSyncedLabel={modelsLastSyncedLabel}
                          syncing={syncingModels}
                          onSync={() => void handleSyncModels()}
                        />
                      ) : null}
                      {modelPanel === 'codex' ? (
                        <ModelMappingEditor
                          value={settingsForm.codex_model_mapping}
                          onChange={(v) => setSettingsForm((f) => ({ ...f, codex_model_mapping: v }))}
                          sourceOptions={codexModelOptions}
                          targetOptions={codexModelOptions}
                          sourceLabel={t('settings2.requestedModel')}
                          targetLabel={t('settings2.targetModel')}
                          sourcePlaceholder="gpt-5.2"
                          targetPlaceholder="gpt-5.5"
                        />
                      ) : null}
                      {modelPanel === 'reasoning' ? (
                        <ReasoningEffortModelsEditor
                          value={settingsForm.reasoning_effort_models}
                          onChange={(v) => setSettingsForm((f) => ({ ...f, reasoning_effort_models: v }))}
                          baseModelOptions={textModelOptions}
                        />
                      ) : null}
                    </SheetBody>
                    {modelPanelField ? (
                      <SheetFooter className="gap-3 sm:flex-row sm:items-center sm:justify-between">
                        <div className="flex min-w-0 items-start gap-2 text-xs">
                          <span
                            className={cn('mt-1 size-2 shrink-0 rounded-full', modelPanelDirty ? 'bg-amber-500' : 'bg-emerald-500')}
                            aria-hidden="true"
                          />
                          <div className="min-w-0">
                            <div className="font-medium text-foreground">
                              {modelPanelDirty ? t('settings.modelPanelUnsaved') : t('settings.saveStatusSaved')}
                            </div>
                            {modelPanelOtherDirtyCount > 0 ? (
                              <p className="mt-0.5 text-[11px] text-muted-foreground">
                                {t('settings.modelPanelOtherDirty', { n: modelPanelOtherDirtyCount })}
                              </p>
                            ) : null}
                          </div>
                        </div>
                        <div className="flex shrink-0 items-center gap-2 max-sm:w-full">
                          {modelPanelDirty ? (
                            <Button
                              variant="ghost"
                              size="sm"
                              className="max-sm:flex-1"
                              onClick={() => discardSettingsField(modelPanelField)}
                              disabled={savingSettings}
                            >
                              <RotateCcw className="size-3.5" />
                              {t('settings.discardChanges')}
                            </Button>
                          ) : null}
                          <Button
                            size="sm"
                            className="max-sm:flex-1"
                            onClick={() => void handleSaveSettings()}
                            disabled={dirtyCount === 0 || savingSettings || autoSaveStatus === 'saving'}
                          >
                            <Save className="size-3.5" />
                            {saveButtonLabel}
                          </Button>
                        </div>
                      </SheetFooter>
                    ) : null}
                  </SheetContent>
                </Sheet>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'appearance' ? (
            <>
              <SettingsSection id="settings-appearance" title={t('settings.nav.appearance')} description={t('settings.nav.appearanceDesc')} icon={<Palette className="size-4" />}>
                <SettingsCard title={t('settings.display')} icon={<Palette className="size-4" />}>
                  <div className="space-y-4">
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.siteName')} description={t('settings.siteNameDesc')}>
                        <Input
                          value={settingsForm.site_name}
                          maxLength={80}
                          placeholder="CodexProxy"
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_name: e.target.value }))}
                        />
                      </SettingField>
                      <SettingField label={t('settings.timezone')} description={t('settings.timezoneDesc')}>
                        <DisplayTimezoneSelect />
                      </SettingField>
                    </div>
                    <SettingField label={t('settings.siteLogo')} description={t('settings.siteLogoDesc')}>
                      <div className="flex items-center gap-3">
                        <div className="flex size-11 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-border bg-background shadow-sm">
                          <img src={siteLogoPreview} alt={settingsForm.site_name || 'CodexProxy'} className="size-full object-cover" />
                        </div>
                        <div className="min-w-0 flex-1 space-y-2">
                          <Input
                            value={settingsForm.site_logo}
                            placeholder="/favicon.png or https://..."
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, site_logo: e.target.value }))}
                          />
                          <div className="flex flex-wrap gap-2">
                            <Button type="button" variant="outline" size="sm" onClick={() => logoFileInputRef.current?.click()}>
                              <Upload className="size-3.5" />
                              {t('settings.siteLogoUpload')}
                            </Button>
                            <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, site_logo: '' }))}>
                              <X className="size-3.5" />
                              {t('settings.siteLogoReset')}
                            </Button>
                          </div>
                          <input
                            ref={logoFileInputRef}
                            type="file"
                            accept="image/png,image/jpeg,image/svg+xml,.png,.jpg,.jpeg,.svg"
                            className="hidden"
                            onChange={handleSiteLogoUpload}
                          />
                        </div>
                      </div>
                    </SettingField>
                    <div className={SETTINGS_SWITCH_ROW}>
                      <SettingField label={t('settings.showFullUsageNumbers')} description={t('settings.showFullUsageNumbersDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.show_full_usage_numbers}
                          onCheckedChange={(checked) => autoSaveBooleanField('show_full_usage_numbers', checked)}
                        />
                      </SettingField>
                      <SettingField label={t('settings.showUpstreamModelMismatch')} description={t('settings.showUpstreamModelMismatchDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.show_upstream_model_mismatch}
                          onCheckedChange={(checked) => autoSaveBooleanField('show_upstream_model_mismatch', checked)}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>

              <SettingsCard title={t('settings.backgroundImage')} description={t('settings.backgroundImageDesc')} icon={<ImageIcon className="size-4" />}>
                <div className="grid gap-5 xl:grid-cols-[minmax(0,1.4fr)_minmax(280px,0.6fr)] xl:items-start">
                  <div className="relative aspect-[16/7] min-h-[180px] overflow-hidden rounded-xl border border-border/80 bg-muted/40 shadow-inner max-sm:aspect-[4/3] sm:min-h-[220px]">
                    {backgroundImagePreview && backgroundIsVideo ? (
                      <video
                        src={backgroundImagePreview}
                        className="size-full object-cover"
                        style={{
                          opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                          filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                          transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                        }}
                        autoPlay
                        muted
                        loop
                        playsInline
                      />
                    ) : backgroundImagePreview ? (
                      <img
                        src={backgroundImagePreview}
                        alt=""
                        className="size-full object-cover"
                        style={{
                          opacity: Math.min(100, Math.max(0, settingsForm.background_opacity)) / 100,
                          filter: settingsForm.background_blur > 0 ? `blur(${settingsForm.background_blur}px)` : undefined,
                          transform: settingsForm.background_blur > 0 ? 'scale(1.04)' : undefined,
                        }}
                      />
                    ) : (
                      <div className="flex size-full items-center justify-center text-xs font-medium text-muted-foreground bg-gradient-to-br from-primary/10 via-muted/30 to-background">
                        {t('settings.backgroundImageEmpty')}
                      </div>
                    )}
                    {/* Frosted-glass live preview card (Glassmorphism Live Preview Overlay) */}
                    <div
                      className="absolute inset-x-4 bottom-4 flex items-center justify-between rounded-xl border border-white/20 bg-background/60 p-3 shadow-lg backdrop-blur-md dark:border-white/10 dark:bg-black/50"
                      style={{
                        backgroundColor: `rgba(var(--bg-card-rgb, 255, 255, 255), ${Math.min(1, Math.max(0, settingsForm.background_glass_opacity / 100))})`,
                        backdropFilter: `blur(${settingsForm.background_glass_blur}px)`,
                        WebkitBackdropFilter: `blur(${settingsForm.background_glass_blur}px)`,
                      }}
                    >
                      <div className="flex items-center gap-2.5">
                        {siteLogoPreview ? (
                          <img src={siteLogoPreview} alt="" className="size-6 rounded object-contain shadow-2xs" />
                        ) : (
                          <div className="size-6 rounded bg-primary/20 flex items-center justify-center text-[10px] font-bold text-primary">
                            CP
                          </div>
                        )}
                        <div className="space-y-0.5">
                          <div className="text-xs font-bold leading-none text-foreground">{settingsForm.site_name || 'CodexProxy'}</div>
                          <div className="text-[10px] text-muted-foreground">Frosted glass preview Glass Effect</div>
                        </div>
                      </div>
                      <Badge variant="outline" className="text-[10px] border-primary/30 bg-primary/10 text-primary font-mono">
                        {settingsForm.background_glass_opacity}% glass
                      </Badge>
                    </div>
                  </div>
                  <div className="flex min-w-0 flex-col gap-4">
                    <div className="min-w-0 space-y-2.5">
                      <Input
                        value={settingsForm.background_image}
                        placeholder="/wallpaper.jpg or https://..."
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, background_image: e.target.value }))}
                      />
                      <div className="flex flex-wrap gap-2">
                        <Button type="button" variant="outline" size="sm" onClick={() => backgroundFileInputRef.current?.click()}>
                          <Upload className="size-3.5" />
                          {t('settings.backgroundImageUpload')}
                        </Button>
                        <Button type="button" variant="ghost" size="sm" onClick={() => setSettingsForm(f => ({ ...f, background_image: '' }))}>
                          <X className="size-3.5" />
                          {t('settings.backgroundImageReset')}
                        </Button>
                      </div>
                      <input
                        ref={backgroundFileInputRef}
                        type="file"
                        accept="image/png,image/jpeg,image/webp,image/svg+xml,video/mp4,.png,.jpg,.jpeg,.webp,.svg,.mp4"
                        className="hidden"
                        onChange={handleBackgroundImageUpload}
                      />
                    </div>
                    <div className="grid gap-3.5 rounded-lg border border-border/60 bg-muted/15 p-3.5">
                      {([
                        {
                          label: t('settings.backgroundOpacity'),
                          value: settingsForm.background_opacity,
                          unit: '%',
                          min: 0,
                          max: 100,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_opacity: v })),
                        },
                        {
                          label: t('settings.backgroundBlur'),
                          value: settingsForm.background_blur,
                          unit: 'px',
                          min: 0,
                          max: 24,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_blur: v })),
                        },
                        {
                          label: t('settings.backgroundGlassOpacity'),
                          value: settingsForm.background_glass_opacity,
                          unit: '%',
                          min: 0,
                          max: 100,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_glass_opacity: v })),
                        },
                        {
                          label: t('settings.backgroundGlassBlur'),
                          value: settingsForm.background_glass_blur,
                          unit: 'px',
                          min: 0,
                          max: 20,
                          onChange: (v: number) => setSettingsForm(f => ({ ...f, background_glass_blur: v })),
                        },
                      ] as const).map((slider) => (
                        <div key={slider.label} className="space-y-1.5">
                          <div className="flex items-center justify-between gap-3 text-xs">
                            <span className="font-medium text-muted-foreground">{slider.label}</span>
                            <span className="min-w-[3rem] text-right font-semibold tabular-nums text-foreground">
                              {slider.value}{slider.unit}
                            </span>
                          </div>
                          <input
                            type="range"
                            min={slider.min}
                            max={slider.max}
                            value={slider.value}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => slider.onChange(parseInt(e.target.value) || 0)}
                            className="h-1.5 w-full cursor-pointer accent-primary"
                          />
                        </div>
                      ))}
                    </div>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>
            </>
          ) : null}

          {activeTab === 'general' ? (
            <>
              <SettingsSection id="settings-overview" title={t('settings.nav.overview')} description={t('settings.nav.overviewDesc')} icon={<Activity className="size-4" />}>
              <SettingsCard
                title={t('settings.systemStatus')}
                icon={<Activity className="size-4" />}
                badge={
                  <Badge variant="secondary" className="text-[11px]">
                    {t('settings.nav.live')}
                  </Badge>
                }
              >
                <div className="grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4">
                  <StatusTile label={t('settings.service')} icon={<Activity className="size-4" />}>
                    <Badge variant={health?.status === 'ok' ? 'default' : 'destructive'} className="gap-1.5">
                      <span className={`size-1.5 rounded-full ${health?.status === 'ok' ? 'bg-emerald-500' : 'bg-red-400'}`} />
                      {health?.status === 'ok' ? t('common.running') : t('common.error')}
                    </Badge>
                  </StatusTile>
                  <StatusTile label={t('settings.accountsLabel')} icon={<Users className="size-4" />}>
                    {health?.available ?? 0} / {health?.total ?? 0}
                  </StatusTile>
                  <StatusTile label={settingsForm.database_label} icon={<Database className="size-4" />}>
                    <Badge variant="default" className="gap-1.5">
                      <span className="size-1.5 rounded-full bg-emerald-500" />
                      {isExternalDatabase ? t('common.connected') : t('common.running')}
                    </Badge>
                  </StatusTile>
                  <StatusTile label={settingsForm.cache_label} icon={<Layers className="size-4" />}>
                    <Badge variant="default" className="gap-1.5">
                      <span className="size-1.5 rounded-full bg-emerald-500" />
                      {isExternalCache ? t('common.connected') : t('common.running')}
                    </Badge>
                  </StatusTile>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-traffic" title={t('settings.nav.traffic')} description={t('settings.nav.trafficDesc')} icon={<Gauge className="size-4" />}>
                <SettingsCard title={t('settings.trafficProtection')} icon={<Gauge className="size-4" />}>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.maxConcurrency')} description={t('settings.maxConcurrencyRange')} suffix={t('settings.unit.concurrency')}>
                      <DraftNumberInput
                        min={1}
                        value={settingsForm.max_concurrency}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_concurrency: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.globalRpm')} description={t('settings.globalRpmRange')} suffix={t('settings.unit.rpm')}>
                      <DraftNumberInput
                        min={0}
                        value={settingsForm.global_rpm}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, global_rpm: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.maxRetries')} description={t('settings.maxRetriesRange')} suffix={t('settings.unit.times')}>
                      <DraftNumberInput
                        min={0}
                        max={10}
                        value={settingsForm.max_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_retries: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.maxRateLimitRetries')} description={t('settings.maxRateLimitRetriesRange')} suffix={t('settings.unit.times')}>
                      <DraftNumberInput
                        min={0}
                        max={10}
                        value={settingsForm.max_rate_limit_retries}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, max_rate_limit_retries: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.retryIntervalMs')} description={t('settings.retryIntervalMsDesc')} suffix="ms">
                      <DraftNumberInput
                        min={0}
                        max={30000}
                        step={100}
                        value={settingsForm.retry_interval_ms}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, retry_interval_ms: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.transportRetryPolicy')} description={t('settings.transportRetryPolicyDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.transport_retry_policy || 'rotate'}
                        onChange={(value) => autoSaveStringField('transport_retry_policy', value)}
                        options={transportRetryPolicyOptions}
                      />
                    </SettingField>
                  </div>
                </SettingsCard>

                  <SettingsCard
                    title={t('settings.continuousRetryTitle')}
                    description={t('settings.continuousRetryDesc')}
                    icon={<RefreshCw className="size-4" />}
                  >
                    <div className="space-y-4">
                      <SettingField
                        label={t('settings.continuousRetryEnabled')}
                        description={t('settings.continuousRetryEnabledDesc')}
                        layout="switch"
                      >
                        <Switch
                          aria-label={t('settings.continuousRetryEnabled')}
                          checked={settingsForm.continuous_retry_enabled}
                          onCheckedChange={(checked) => void autoSaveContinuousRetryPatch(buildContinuousRetryEnabledPatch(checked))}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.continuousRetryCatchAll')}
                        description={t('settings.continuousRetryCatchAllDesc')}
                        warning={t('settings.continuousRetryCatchAllWarning')}
                        layout="switch"
                        className={cn(
                          'rounded-lg',
                          settingsForm.continuous_retry_catch_all && 'border-amber-500/50 bg-amber-500/10 hover:border-amber-500/60',
                        )}
                      >
                        <Switch
                          aria-label={t('settings.continuousRetryCatchAll')}
                          checked={settingsForm.continuous_retry_catch_all}
                          onCheckedChange={(checked) => void autoSaveContinuousRetryPatch(buildContinuousRetryCatchAllPatch(checked))}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.continuousRetryMaxDuration')}
                        description={t('settings.continuousRetryMaxDurationDesc')}
                      >
                        <Input
                          aria-label={t('settings.continuousRetryMaxDuration')}
                          type="number"
                          min={1}
                          max={900}
                          step={1}
                          value={settingsForm.continuous_retry_max_duration_seconds}
                          disabled={!settingsForm.continuous_retry_enabled}
                          onChange={(event) => {
                            const value = Number(event.target.value)
                            setSettingsForm((current) => ({
                              ...current,
                              continuous_retry_max_duration_seconds: Number.isFinite(value) ? value : 600,
                            }))
                          }}
                          onBlur={(event) => {
                            const value = parseContinuousRetryMaxDurationSeconds(event.target.value)
                            setSettingsForm((current) => ({ ...current, continuous_retry_max_duration_seconds: value }))
                            void autoSaveContinuousRetryPatch({ continuous_retry_max_duration_seconds: value })
                          }}
                        />
                      </SettingField>
                      <div className={cn('grid gap-3 sm:grid-cols-2 lg:grid-cols-4', continuousRetryFineControlsDisabled && 'opacity-60')}>
                        {continuousRetryCategoryOptions.map((option) => (
                          <SettingField
                            key={option.value}
                            label={option.label}
                            layout="switch"
                            className="rounded-lg border border-border/60 px-3 py-2"
                          >
                            <Switch
                              aria-label={option.label}
                              checked={(settingsForm.continuous_retry_categories ?? []).includes(option.value)}
                              disabled={continuousRetryFineControlsDisabled}
                              onCheckedChange={(checked) => {
                                const current = settingsFormRef.current.continuous_retry_categories ?? []
                                const next = checked
                                  ? Array.from(new Set([...current, option.value]))
                                  : current.filter((value) => value !== option.value)
                                void autoSaveContinuousRetryPatch({ continuous_retry_categories: next })
                              }}
                            />
                          </SettingField>
                        ))}
                      </div>
                      <div className="grid gap-4 lg:grid-cols-2">
                        <SettingField
                          label={t('settings.continuousRetryStatusCodes')}
                          description={t('settings.continuousRetryStatusCodesDesc')}
                        >
                          <Input
                            aria-label={t('settings.continuousRetryStatusCodes')}
                            value={continuousRetryStatusCodesDraft}
                            disabled={continuousRetryFineControlsDisabled}
                            placeholder="403,404,429,500,501,502,503,504"
                            onChange={(event) => setContinuousRetryStatusCodesDraft(event.target.value)}
                            onBlur={(event) => {
                              const values = parseContinuousRetryStatusCodes(event.target.value)
                              setContinuousRetryStatusCodesDraft(values.join(','))
                              void autoSaveContinuousRetryPatch({ continuous_retry_status_codes: values })
                            }}
                          />
                        </SettingField>
                        <SettingField
                          label={t('settings.continuousRetryErrorCodes')}
                          description={t('settings.continuousRetryErrorCodesDesc')}
                        >
                          <Input
                            aria-label={t('settings.continuousRetryErrorCodes')}
                            value={continuousRetryErrorCodesDraft}
                            disabled={continuousRetryFineControlsDisabled}
                            placeholder="rate_limited,context_length_exceeded"
                            onChange={(event) => setContinuousRetryErrorCodesDraft(event.target.value)}
                            onBlur={(event) => {
                              const values = parseContinuousRetryErrorCodes(event.target.value)
                              setContinuousRetryErrorCodesDraft(values.join(','))
                              void autoSaveContinuousRetryPatch({ continuous_retry_error_codes: values })
                            }}
                          />
                        </SettingField>
                      </div>
                      <p className="text-xs leading-relaxed text-amber-600 dark:text-amber-400">
                        {t('settings.continuousRetryWarning')}
                      </p>
                    </div>
                  </SettingsCard>

              <SettingsCard
                title={t('settings.modelCooldownTitle')}
                description={t('settings.modelCooldownDesc')}
                icon={<Timer className="size-4" />}
              >
                <div className="grid gap-4">
                  <div className="space-y-3 rounded-xl border border-amber-500/20 bg-amber-500/5 p-3.5">
                    <div>
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold">{t('settings.oauthModelCooldownTitle')}</h3>
                      </div>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.oauthModelCooldownDesc')}
                      </p>
                    </div>
                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField label={t('settings.modelCooldownMode')} description={t('settings.modelCooldownModeDesc')}>
                        <SegmentedPillGroup
                          value={settingsForm.oauth_model_cooldown_mode}
                          onChange={(value) => autoSaveStringField('oauth_model_cooldown_mode', value)}
                          options={modelCooldownModeOptions}
                        />
                      </SettingField>
                      <SettingField
                        label={t('settings.modelCooldownSeconds')}
                        description={t('settings.modelCooldownSecondsDesc')}
                        suffix={t('settings.unit.sec')}
                        className={cn(settingsForm.oauth_model_cooldown_mode === 'off' && 'opacity-60')}
                      >
                        <DraftNumberInput
                          min={1}
                          max={1800}
                          disabled={settingsForm.oauth_model_cooldown_mode === 'off'}
                          value={settingsForm.oauth_model_cooldown_seconds}
                          onValueChange={(value) => setSettingsForm(f => ({ ...f, oauth_model_cooldown_seconds: value }))}
                          onValueCommit={(value) => void autoSaveSettingsPatch({ oauth_model_cooldown_seconds: value })}
                        />
                      </SettingField>
                    </div>
                    <SettingField
                      label={t('settings.modelCooldownBackoff')}
                      description={t('settings.modelCooldownBackoffDesc')}
                      layout="switch"
                      className={cn(settingsForm.oauth_model_cooldown_mode !== 'adaptive' && 'opacity-60')}
                    >
                      <Switch
                        checked={settingsForm.oauth_model_cooldown_backoff_enabled}
                        disabled={settingsForm.oauth_model_cooldown_mode !== 'adaptive'}
                        onCheckedChange={(checked) => autoSaveBooleanField('oauth_model_cooldown_backoff_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.unsupportedModelCooldownSeconds')}
                      description={t('settings.unsupportedModelCooldownSecondsDesc')}
                      suffix={t('settings.unit.sec')}
                    >
                      <DraftNumberInput
                        min={0}
                        max={1800}
                        value={settingsForm.oauth_unsupported_model_cooldown_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, oauth_unsupported_model_cooldown_seconds: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ oauth_unsupported_model_cooldown_seconds: value })}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.schedulingStrategy')} icon={<Layers className="size-4" />}>
                <div className="grid auto-rows-min items-start gap-4 lg:grid-cols-2">
                  <div className="h-fit space-y-3 rounded-xl border border-border/60 bg-muted/10 p-3.5">
                    <div>
                      <h3 className="text-sm font-semibold">{t('settings.schedulingAccountGroup')}</h3>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.schedulingAccountGroupDesc')}
                      </p>
                    </div>
                    <SettingsCollapsibleNote title={t('settings.schedulerEngineCompatibilityTitle')}>
                      {t('settings.schedulerEngineCompatibilityNote')}
                    </SettingsCollapsibleNote>
                    <SettingField label={t('settings.schedulerEngine')} description={t('settings.schedulerEngineDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.scheduler_engine}
                        onChange={(value) => autoSaveStringField('scheduler_engine', value)}
                        options={schedulerEngineOptions}
                      />
                    </SettingField>
                    <div className="grid gap-2" role="list" aria-label={t('settings.schedulerEngine')}>
                      {schedulerEngineExplanations.map((option) => {
                        const active = option.value === settingsForm.scheduler_engine
                        return (
                          <div
                            key={option.value}
                            role="listitem"
                            aria-current={active ? 'true' : undefined}
                            className={cn(
                              'rounded-lg border px-3 py-2.5 transition-colors',
                              active
                                ? 'border-primary/35 bg-primary/5'
                                : 'border-border/50 bg-background/45',
                            )}
                          >
                            <div className="flex items-start gap-2.5">
                              <span
                                className={cn(
                                  'mt-1.5 size-1.5 shrink-0 rounded-full',
                                  active ? 'bg-primary' : 'bg-muted-foreground/40',
                                )}
                                aria-hidden="true"
                              />
                              <div className="min-w-0">
                                <div className={cn('text-xs font-semibold', active ? 'text-primary' : 'text-foreground')}>
                                  {option.label}
                                </div>
                                <p className="mt-0.5 text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
                                  {option.description}
                                </p>
                              </div>
                            </div>
                          </div>
                        )
                      })}
                    </div>
                    <SettingField
                      label={t('settings.schedulerMode')}
                      description={settingsForm.scheduler_mode === 'reset_5h_first' ? t('settings.schedulerModeReset5hFirstDesc') : t('settings.schedulerModeDesc')}
                      warning={settingsForm.scheduler_engine !== 'legacy' || settingsForm.scheduler_mode === 'reset_5h_first' ? undefined : t('settings.schedulerModeRequiresFast')}
                      className={cn(settingsForm.scheduler_engine === 'legacy' && settingsForm.scheduler_mode !== 'reset_5h_first' && 'opacity-60')}
                    >
                      <SegmentedPillGroup
                        value={settingsForm.scheduler_mode}
                        onChange={(value) => autoSaveStringField('scheduler_mode', value)}
                        options={schedulerModeOptions}
                      />
                    </SettingField>
                  </div>

                  <div className="h-fit space-y-3 rounded-xl border border-border/60 bg-muted/10 p-3.5">
                    <div>
                      <h3 className="text-sm font-semibold">{t('settings.schedulingAffinityGroup')}</h3>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                        {t('settings.schedulingAffinityGroupDesc')}
                      </p>
                    </div>
                    <SettingField label={t('settings.affinityMode')} description={t('settings.affinityModeDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.affinity_mode || 'bounded'}
                        onChange={(value) => autoSaveStringField('affinity_mode', value)}
                        options={affinityModeOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.sessionAffinitySpread')} description={t('settings.sessionAffinitySpreadDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.session_affinity_spread}
                        onCheckedChange={(checked) => autoSaveBooleanField('session_affinity_spread', checked)}
                      />
                    </SettingField>
                    <SettingField label={t('settings.sessionSlotBuffer')} description={t('settings.sessionSlotBufferDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.session_slot_buffer_enabled}
                        onCheckedChange={(checked) => autoSaveBooleanField('session_slot_buffer_enabled', checked)}
                      />
                    </SettingField>
                    <SettingField
                      label={t('settings.sessionSlotBufferSeconds')}
                      description={t('settings.sessionSlotBufferSecondsDesc')}
                      className={cn(!settingsForm.session_slot_buffer_enabled && 'opacity-60')}
                    >
                      <DraftNumberInput
                        min={1}
                        max={60}
                        disabled={!settingsForm.session_slot_buffer_enabled}
                        value={settingsForm.session_slot_buffer_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, session_slot_buffer_seconds: value }))}
                        onValueCommit={(value) => void autoSaveSettingsPatch({ session_slot_buffer_seconds: value })}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-runtime" title={t('settings.nav.runtime')} description={t('settings.nav.runtimeDesc')} icon={<Wrench className="size-4" />}>
              <SettingsCard title={t('settings.runtimeOptimization')} description={t('settings.runtimeOptimizationDesc')} icon={<Wrench className="size-4" />}>
                <div className="space-y-4">
                  <div className={SETTINGS_FIELD_GRID_3}>
                    <SettingField label={t('settings.usageLogMode')} description={t('settings.usageLogModeDesc')}>
                      <Select
                        value={settingsForm.usage_log_mode}
                        onValueChange={(value) => autoSaveStringField('usage_log_mode', value)}
                        options={usageLogModeOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.usageLogBatchSize')} description={t('settings.usageLogBatchSizeDesc')}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        value={settingsForm.usage_log_batch_size}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_log_batch_size: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.usageLogFlushInterval')} description={t('settings.usageLogFlushIntervalDesc')}>
                      <DraftNumberInput
                        min={1}
                        max={300}
                        value={settingsForm.usage_log_flush_interval_seconds}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, usage_log_flush_interval_seconds: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.billingTierPolicy')} description={t('settings.billingTierPolicyDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.billing_tier_policy}
                        onChange={(value) => autoSaveStringField('billing_tier_policy', value)}
                        options={billingTierPolicyOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.modelsListReadMaxBytes')} description={t('settings.modelsListReadMaxBytesDesc')}>
                      <div className="relative">
                        <DraftNumberInput
                          min={1}
                          max={256}
                          className="pr-12 tabular-nums"
                          value={bytesToMiB(settingsForm.models_list_read_max_bytes)}
                          onValueChange={(value) =>
                            setSettingsForm((form) => ({
                              ...form,
                              models_list_read_max_bytes: mibToBytes(value),
                            }))
                          }
                          onValueCommit={(value) =>
                            commitByteLimitField('models_list_read_max_bytes', value, MIB)
                          }
                        />
                        <span className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-[11px] font-medium text-muted-foreground">
                          MiB
                        </span>
                      </div>
                    </SettingField>
                    <SettingField label={t('settings.streamFlushPolicy')} description={t('settings.streamFlushPolicyDesc')}>
                      <SegmentedPillGroup
                        value={settingsForm.stream_flush_policy}
                        onChange={(value) => autoSaveStringField('stream_flush_policy', value)}
                        options={streamFlushPolicyOptions}
                      />
                    </SettingField>
                    <SettingField label={t('settings.streamFlushInterval')} description={t('settings.streamFlushIntervalDesc')}>
                      <DraftNumberInput
                        min={1}
                        max={1000}
                        value={settingsForm.stream_flush_interval_ms}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, stream_flush_interval_ms: value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.firstTokenTimeout')} description={t('settings.firstTokenTimeoutDesc')}>
                      <DraftNumberInput
                        min={0}
                        max={600}
                        value={settingsForm.first_token_timeout_seconds}
                        emptyValue={0}
                        onValueChange={(value) => setSettingsForm(f => ({ ...f, first_token_timeout_seconds: value }))}
                      />
                    </SettingField>
                  </div>
                  <div className={SETTINGS_SWITCH_ROW}>
                    <SettingField label={t('settings.firstTokenExcludesWsAcquire')} description={t('settings.firstTokenExcludesWsAcquireDesc')} layout="switch">
                      <Switch
                        checked={settingsForm.first_token_excludes_ws_acquire}
                        onCheckedChange={(checked) => autoSaveBooleanField('first_token_excludes_ws_acquire', checked)}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>

              <SettingsCard title={t('settings.githubAccess')} description={t('settings.githubAccessDesc')} icon={<Globe className="size-4" />}>
                <div className={SETTINGS_FIELD_GRID}>
                  <SettingField label={t('settings.githubToken')} description={t('settings.githubTokenDesc')}>
                    <div className="flex items-center gap-2">
                      <Input
                        type="password"
                        autoComplete="off"
                        placeholder={settingsForm.github_token_configured ? t('settings.githubTokenConfiguredPlaceholder') : t('settings.githubTokenPlaceholder')}
                        value={githubTokenDraft}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setGithubTokenDraft(e.target.value)}
                        onBlur={() => {
                          const value = githubTokenDraft.trim()
                          if (!value) return
                          void autoSaveSettingsPatch({ github_token: value, github_token_configured: true })
                          setGithubTokenDraft('')
                        }}
                      />
                      {settingsForm.github_token_configured && (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            void autoSaveSettingsPatch({ github_token: '', github_token_configured: false })
                          }}
                        >
                          {t('settings.githubTokenClear')}
                        </Button>
                      )}
                    </div>
                  </SettingField>
                  <SettingField label={t('settings.githubProxy')} description={t('settings.githubProxyDesc')}>
                    <Input
                      value={settingsForm.github_proxy_url}
                      placeholder="http://host:port / socks5://host:port"
                      onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, github_proxy_url: e.target.value }))}
                      onBlur={(e: FocusEvent<HTMLInputElement>) => autoSaveStringField('github_proxy_url', e.target.value.trim())}
                    />
                  </SettingField>
                </div>
              </SettingsCard>

              <SettingsCard
                title={showConnectionPool ? t('settings.connectionPool') : t('settings.resinTitle')}
                description={showConnectionPool ? t('settings.nav.poolRestartHint') : t('settings.resinDesc')}
                icon={<Database className="size-4" />}
                badge={
                  showConnectionPool ? (
                    <Badge variant="outline" className="text-[11px]">
                      {t('settings.nav.restartRequired')}
                    </Badge>
                  ) : (
                    <Badge variant={resinActive ? 'default' : 'outline'} className="text-[11px]">
                      {resinActive ? t('settings.resinActiveBadge') : t('settings.resinInactiveBadge')}
                    </Badge>
                  )
                }
              >
                <div className="space-y-4">
                  {showConnectionPool ? (
                    <div className={SETTINGS_FIELD_GRID}>
                      {isExternalDatabase ? (
                        <SettingField label={t('settings.pgMaxConns')} description={t('settings.pgMaxConnsRange')}>
                          <DraftNumberInput
                            min={5}
                            max={5000}
                            value={settingsForm.pg_max_conns}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, pg_max_conns: value }))}
                          />
                        </SettingField>
                      ) : null}
                      {isExternalCache ? (
                        <SettingField label={t('settings.redisPoolSize')} description={t('settings.redisPoolSizeRange')}>
                          <DraftNumberInput
                            min={5}
                            max={5000}
                            value={settingsForm.redis_pool_size}
                            onValueChange={(value) => setSettingsForm(f => ({ ...f, redis_pool_size: value }))}
                          />
                        </SettingField>
                      ) : null}
                    </div>
                  ) : null}
                  {showConnectionPool ? (
                    <div className="border-t border-border/80 pt-4">
                      <div className="flex flex-wrap items-center gap-2">
                        <h4 className="text-[13px] font-semibold text-foreground sm:text-sm">{t('settings.resinTitle')}</h4>
                        <Badge variant={resinActive ? 'default' : 'outline'} className="text-[11px]">
                          {resinActive ? t('settings.resinActiveBadge') : t('settings.resinInactiveBadge')}
                        </Badge>
                      </div>
                      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('settings.resinDesc')}</p>
                    </div>
                  ) : null}
                  {/* When three egress options coexist, it must be stated clearly which one is in effect (issue #679): Resin is a full-layer override, not an overlay. */}
                  <div
                    className={cn(
                      'rounded-lg border px-3 py-2 text-xs leading-relaxed',
                      resinActive
                        ? 'border-amber-500/30 bg-amber-500/5 text-amber-800 dark:text-amber-200'
                        : 'border-border/60 bg-muted/20 text-muted-foreground',
                    )}
                  >
                    {resinActive
                      ? t('settings.resinActiveNote', { endpoint: codexEgress?.resin_endpoint || '', platform: codexEgress?.resin_platform_name || '' })
                      : t('settings.resinInactiveNote')}
                  </div>
                  <div className={SETTINGS_FIELD_GRID}>
                    <SettingField label={t('settings.resinUrl')} description={t('settings.resinUrlDesc')}>
                      <Input
                        placeholder="http://127.0.0.1:2260/your-token"
                        value={settingsForm.resin_url}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_url: e.target.value }))}
                      />
                    </SettingField>
                    <SettingField label={t('settings.resinPlatformName')} description={t('settings.resinPlatformNameDesc')}>
                      <Input
                        placeholder="codex2api"
                        value={settingsForm.resin_platform_name}
                        onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, resin_platform_name: e.target.value }))}
                      />
                    </SettingField>
                  </div>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-storage" title={t('settings.nav.storage')} description={t('settings.nav.storageDesc')} icon={<ImageIcon className="size-4" />}>
              <SettingsCard title={t('settings.imageStorage')} description={t('settings.imageStorageDesc')} icon={<ImageIcon className="size-4" />}>
                <div className="space-y-4">
                  <SettingField label={t('settings.imageStorageBackend')} description={t('settings.imageStorageBackendDesc')}>
                    <SegmentedPillGroup
                      value={settingsForm.image_storage_backend}
                      onChange={(value) => setSettingsForm((f) => ({ ...f, image_storage_backend: value }))}
                      options={imageStorageBackendOptions}
                    />
                  </SettingField>

                  {settingsForm.image_storage_backend === 's3' ? (
                    <div className="rounded-xl border border-primary/20 bg-primary/5 p-4 space-y-4">
                      <div className="flex items-center gap-2 font-semibold text-foreground text-sm border-b border-primary/10 pb-2.5">
                        <Cloud className="size-4 text-primary" />
                        <span>Object storage credentials (S3 Compatible Storage)</span>
                      </div>
                      <div className={SETTINGS_FIELD_GRID_3}>
                        <SettingField label={t('settings.imageS3Endpoint')} description={t('settings.imageS3EndpointDesc')}>
                          <Input
                            value={settingsForm.image_s3_endpoint}
                            placeholder="https://..."
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_endpoint: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Region')} description={t('settings.imageS3RegionDesc')}>
                          <Input
                            value={settingsForm.image_s3_region}
                            placeholder="auto"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_region: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Bucket')} description={t('settings.imageS3BucketDesc')}>
                          <Input
                            value={settingsForm.image_s3_bucket}
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_bucket: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3AccessKey')} description={t('settings.imageS3AccessKeyDesc')}>
                          <Input
                            value={settingsForm.image_s3_access_key}
                            autoComplete="off"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_access_key: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3SecretKey')} description={t('settings.imageS3SecretKeyDesc')}>
                          <Input
                            type="password"
                            value={settingsForm.image_s3_secret_key}
                            autoComplete="new-password"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_secret_key: e.target.value }))}
                          />
                        </SettingField>
                        <SettingField label={t('settings.imageS3Prefix')} description={t('settings.imageS3PrefixDesc')}>
                          <Input
                            value={settingsForm.image_s3_prefix}
                            placeholder="codex/images"
                            onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => ({ ...f, image_s3_prefix: e.target.value }))}
                          />
                        </SettingField>
                      </div>
                      <SettingField label={t('settings.imageS3ForcePathStyle')} description={t('settings.imageS3ForcePathStyleDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.image_s3_force_path_style}
                          onCheckedChange={(checked) => autoSaveBooleanField('image_s3_force_path_style', checked)}
                        />
                      </SettingField>
                    </div>
                  ) : null}
                </div>
                {settingsForm.image_storage_backend === 's3' ? (
                  <div className="mt-4">
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => void handleTestImageStorage()}
                      disabled={testingImageStorage || !settingsForm.image_s3_bucket || !settingsForm.image_s3_access_key || !settingsForm.image_s3_secret_key}
                    >
                      {testingImageStorage ? t('settings.imageS3Testing') : t('settings.imageS3Test')}
                    </Button>
                  </div>
                ) : null}
              </SettingsCard>

              <SettingsCard title={t('settings.autoCleanup')} icon={<Trash2 className="size-4" />} tone="danger">
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
                  <SettingField label={t('settings.autoCleanUnauthorized')} description={t('settings.autoCleanUnauthorizedDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_unauthorized}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_unauthorized', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanRateLimited')} description={t('settings.autoCleanRateLimitedDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_rate_limited}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_rate_limited', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanFullUsage')} description={t('settings.autoCleanFullUsageDesc')} layout="switch">
                    <Switch
                      checked={lazyModeActive ? false : settingsForm.auto_clean_full_usage}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_full_usage', checked)}
                      disabled={lazyModeActive}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanError')} description={t('settings.autoCleanErrorDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_error}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_error', checked)}
                    />
                  </SettingField>
                  <SettingField label={t('settings.autoCleanExpired')} description={t('settings.autoCleanExpiredDesc')} layout="switch">
                    <Switch
                      checked={settingsForm.auto_clean_expired}
                      onCheckedChange={(checked) => autoSaveBooleanField('auto_clean_expired', checked)}
                    />
                  </SettingField>
                </div>
              </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-security" title={t('settings.nav.security')} description={t('settings.nav.securityDesc')} icon={<Shield className="size-4" />}>
                <SettingsCard
                  title={t('settings.security')}
                  icon={<Shield className="size-4" />}
                  tone="danger"
                  badge={
                    <Badge variant="outline" className="border-destructive/30 text-[11px] text-destructive">
                      {t('settings.nav.sensitive')}
                    </Badge>
                  }
                >
                  <div className="space-y-4">
                    <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-destructive/20 bg-destructive/5 p-3.5 text-xs">
                      <div className="flex items-center gap-2 font-medium text-foreground">
                        <ShieldAlert className="size-4 text-destructive" />
                        <span>System protection and audit (Security Guard)</span>
                      </div>
                      <div className="flex items-center gap-2">
                        <Badge variant={settingsForm.prompt_filter_enabled ? 'default' : 'outline'} className="text-[11px] gap-1">
                          <span className={`size-1.5 rounded-full ${settingsForm.prompt_filter_enabled ? 'bg-emerald-400' : 'bg-amber-400'}`} />
                          Prompt risk control: {settingsForm.prompt_filter_enabled ? 'Enabled' : 'Disabled'}
                        </Badge>
                        <Badge variant="secondary" className="text-[11px] uppercase font-mono">
                          Mode: {settingsForm.prompt_filter_mode}
                        </Badge>
                      </div>
                    </div>

                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField
                        label={t('settings.adminSecret')}
                        description={t('settings.adminSecretDesc')}
                        warning={settingsForm.admin_auth_source === 'env' ? t('settings.adminSecretEnvOverride') : undefined}
                      >
                        <Input
                          type="text"
                          placeholder={t('settings.adminSecretPlaceholder')}
                          value={settingsForm.admin_secret}
                          disabled={settingsForm.admin_auth_source === 'env'}
                          onChange={(e: ChangeEvent<HTMLInputElement>) => setSettingsForm(f => {
                            const nextSecret = e.target.value
                            return {
                              ...f,
                              admin_secret: nextSecret,
                              allow_remote_migration: nextSecret.trim() === '' ? false : f.allow_remote_migration,
                            }
                          })}
                        />
                      </SettingField>
                      <SettingField label={t('settings.promptFilterMode')} description={t('settings.promptFilterModeDesc')}>
                        <Select
                          value={settingsForm.prompt_filter_mode}
                          onValueChange={(value) => autoSaveStringField('prompt_filter_mode', value)}
                          options={[
                            { label: t('promptFilter.modeMonitor'), value: 'monitor' },
                            { label: t('promptFilter.modeWarn'), value: 'warn' },
                            { label: t('promptFilter.modeBlock'), value: 'block' },
                          ]}
                        />
                      </SettingField>
                    </div>
                    <div className={SETTINGS_SWITCH_GRID}>
                      <SettingField
                        label={t('settings.allowRemoteMigration')}
                        description={t('settings.allowRemoteMigrationDesc')}
                        warning={
                          !canConfigureRemoteMigration
                            ? t('settings.allowRemoteMigrationRequiresSecret')
                            : undefined
                        }
                        layout="switch"
                      >
                        <Switch
                          checked={settingsForm.allow_remote_migration}
                          disabled={!canConfigureRemoteMigration}
                          onCheckedChange={(checked) => autoSaveBooleanField('allow_remote_migration', checked)}
                        />
                      </SettingField>
                      <SettingField label={t('settings.promptFilterEnabled')} description={t('settings.promptFilterEnabledDesc')} layout="switch">
                        <Switch
                          checked={settingsForm.prompt_filter_enabled}
                          onCheckedChange={(checked) => autoSaveBooleanField('prompt_filter_enabled', checked)}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>

                <SettingsCard
                  title="Model output runaway protection"
                  description="Globally and in real time monitors for tool arguments that keep repeating, floods of tiny deltas, and channel- or request-level byte budget overruns"
                  icon={<ShieldAlert className="size-4" />}
                  tone={settingsForm.output_guard_enabled ? 'default' : 'danger'}
                  badge={
                    <Badge variant={settingsForm.output_guard_enabled ? 'outline' : 'destructive'} className="text-[11px] gap-1">
                      <span className={`size-1.5 rounded-full ${settingsForm.output_guard_enabled ? 'bg-emerald-400' : 'bg-red-500'}`} />
                      {settingsForm.output_guard_enabled ? 'Global protection enabled' : 'Turned off (high risk)'}
                    </Badge>
                  }
                  footer={
                    <div className="flex justify-end">
                      <Button
                        variant="outline"
                        size="sm"
                        className="text-xs h-7 gap-1"
                        onClick={() => {
                          const patch = { ...DEFAULT_OUTPUT_GUARD_SETTINGS }
                          setSettingsForm((prev) => ({ ...prev, ...patch }))
                          void autoSaveSettingsPatch(patch)
                        }}
                      >
                        <RotateCcw className="size-3" />
                        Restore default protection
                      </Button>
                    </div>
                  }
                >
                  <div className="space-y-4">
                    {!settingsForm.output_guard_enabled && (
                      <div className="rounded-xl border border-destructive/30 bg-destructive/10 p-3.5 text-xs text-destructive flex items-start gap-2">
                        <ShieldAlert className="size-4 shrink-0 mt-0.5" />
                        <div>
                          <p className="font-semibold">High-risk operation warning: runaway protection is turned off globally</p>
                          <p className="opacity-90">With protection off, if the model gets stuck in a generation loop, emits repeated code, or produces tiny arguments deltas, it will break straight through the proxy and cause an O(n²) full re-parse avalanche downstream. Use this only for temporary diagnosis, and turn it back on as soon as possible!</p>
                        </div>
                      </div>
                    )}

                    <div className="rounded-xl border bg-muted/20 p-3.5 text-xs text-muted-foreground space-y-1.5">
                      <p className="font-medium text-foreground">Before turning it on, note the following</p>
                      <ul className="list-disc pl-4 space-y-1">
                        <li><span className="font-medium text-foreground">CPU overhead</span>: once enabled, every model output stream is scanned for repetition and has its budget accounted for, which adds CPU usage (measured at about 1%–2% for a single 1 MiB stream; the net increase at 40 concurrent streams falls within measurement noise). For high-throughput deployments, keep watching CPU and throughput after enabling.</li>
                        <li><span className="font-medium text-foreground">Hit behavior</span>: a hit immediately ends this upstream generation and returns 502 (error code <code className="font-mono">model_output_guard_triggered</code>); content already sent is not rolled back; a Guard hit <b>does not retry automatically</b> (the runaway is deterministic, so a retry would only repeat the cost).</li>
                        <li><span className="font-medium text-foreground">Difference from stream breaks</span>: an upstream stream break (<code className="font-mono">upstream_stream_break</code>) is a transient failure, its message carries a <code className="font-mono">status 502</code> marker, and clients retry it automatically; a Guard hit is different.</li>
                        <li><span className="font-medium text-foreground">Scope</span>: applies globally (all accounts and models), off by default; turning off the Guard does not turn off the framing hard limits (8 MiB per line/event, 48 MB for WS).</li>
                        <li><span className="font-medium text-foreground">False positives</span>: if normal, very long output is blocked, raise the &quot;Tool argument delta limit&quot; (≤65536) or the &quot;Per-channel byte limit&quot;; changes take effect within about 5 seconds after saving, with no restart needed.</li>
                        <li><span className="font-medium text-foreground">Data safety</span>: the block reason is recorded only on the server; downstream receives only a stable public error code and a generic message (no model text, tool paths, prompts or keys).</li>
                      </ul>
                    </div>

                    {outputGuardValidationMessage ? (
                      <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-xs text-amber-700 dark:text-amber-400">
                        {outputGuardValidationMessage}
                      </div>
                    ) : null}

                    <div className="flex items-center justify-between p-3 rounded-lg border bg-muted/20">
                      <div>
                        <div className="text-sm font-medium">Enable runaway output protection</div>
                        <div className="text-xs text-muted-foreground">Detects, globally and in real time, persistent short-period repetition, floods of tiny arguments deltas, and byte budget overruns, and cuts them off early</div>
                      </div>
                      <Switch
                        checked={settingsForm.output_guard_enabled}
                        onCheckedChange={(checked) => {
                          setSettingsForm((f) => ({ ...f, output_guard_enabled: checked }))
                          void autoSaveSettingsPatch({ output_guard_enabled: checked })
                        }}
                      />
                    </div>

                    <div className={SETTINGS_FIELD_GRID}>
                      <SettingField
                        label="Tool argument byte limit (KiB)"
                        description="Hard cap on the full serialized byte size of one tool call's arguments (default 256 KiB, range 16 KiB..4096 KiB)"
                      >
                        <DraftNumberInput
                          min={16}
                          max={4096}
                          value={bytesToKiB(settingsForm.output_guard_max_tool_argument_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_tool_argument_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            commitByteLimitField('output_guard_max_tool_argument_bytes', val, KIB)
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="Tool non-empty delta count limit"
                        description="Limits how many tiny arguments deltas can trigger high-frequency downstream re-parse events (default 4096, range 256..65536)"
                      >
                        <DraftNumberInput
                          min={256}
                          max={65536}
                          value={settingsForm.output_guard_max_tool_argument_deltas}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_tool_argument_deltas: val }))
                          }}
                          onValueCommit={(val) => {
                            void autoSaveSettingsPatch({ output_guard_max_tool_argument_deltas: val })
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="Per-channel byte limit (KiB)"
                        description="Cumulative byte limit for one text/thinking generation channel (default 2048 KiB, range 128 KiB..16384 KiB)"
                      >
                        <DraftNumberInput
                          min={128}
                          max={16384}
                          value={bytesToKiB(settingsForm.output_guard_max_channel_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_channel_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            commitByteLimitField('output_guard_max_channel_bytes', val, KIB)
                          }}
                        />
                      </SettingField>

                      <SettingField
                        label="Per-request semantic budget (KiB)"
                        description={`Hard limit on the total semantic bytes consumed by all channels within one request (default ${bytesToMiB(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_request_bytes)} MiB, range 256 KiB..65536 KiB)`}
                      >
                        <DraftNumberInput
                          min={256}
                          max={65536}
                          value={bytesToKiB(settingsForm.output_guard_max_request_bytes)}
                          onValueChange={(val) => {
                            setSettingsForm((f) => ({ ...f, output_guard_max_request_bytes: kibToBytes(val) }))
                          }}
                          onValueCommit={(val) => {
                            commitByteLimitField('output_guard_max_request_bytes', val, KIB)
                          }}
                        />
                      </SettingField>
                    </div>
                  </div>
                </SettingsCard>
              </SettingsSection>

              <SettingsSection id="settings-reference" title={t('settings.nav.reference')} description={t('settings.nav.referenceDesc')} icon={<Link2 className="size-4" />}>
                {/* The read-only reference table stays expanded: if it were collapsed, users could not find the endpoint list. */}
                <SettingsCard title={t('settings.apiEndpoints')} description={t('settings.nav.endpointsHint')} icon={<Link2 className="size-4" />}>
                    <div className="space-y-3">
                      <div className="flex flex-wrap items-center justify-between gap-2">
                        <p className="text-xs text-muted-foreground">
                          {t('settings.nav.endpointsReadonly')}
                        </p>
                        <Link
                          to="/docs#model-api"
                          className="inline-flex items-center gap-1 text-xs font-semibold text-primary hover:underline"
                        >
                          <ExternalLink className="size-3.5" />
                          {t('settings.nav.openDocs')}
                        </Link>
                      </div>
                      <div className="grid gap-2 sm:hidden">
                        {([
                          { method: 'POST', path: '/v1/chat/completions', desc: t('settings.openaiCompat'), tone: 'default' as const },
                          { method: 'POST', path: '/v1/responses', desc: t('settings.responsesApi'), tone: 'outline' as const },
                          { method: 'POST', path: '/v1/images/generations', desc: t('settings.imageGenerationApi'), tone: 'outline' as const },
                          { method: 'POST', path: '/v1/images/edits', desc: t('settings.imageEditApi'), tone: 'outline' as const },
                          { method: 'GET', path: '/v1/models', desc: t('settings.modelList'), tone: 'secondary' as const },
                        ]).map((item) => (
                          <div
                            key={item.path}
                            className="rounded-xl border border-border bg-background/70 px-3 py-2.5"
                          >
                            <div className="flex items-center gap-2">
                              <Badge variant={item.tone} className="shrink-0 text-[11px]">
                                {item.method}
                              </Badge>
                              <code className="min-w-0 flex-1 truncate font-mono text-[12px] font-semibold text-foreground">
                                {item.path}
                              </code>
                            </div>
                            <p className="mt-1.5 text-[12px] leading-relaxed text-muted-foreground">
                              {item.desc}
                            </p>
                          </div>
                        ))}
                      </div>
                      <div className="data-table-shell hidden sm:block">
                        <Table>
                          <TableHeader>
                            <TableRow>
                              <TableHead className="text-[12px] font-semibold">{t('settings.method')}</TableHead>
                              <TableHead className="text-[12px] font-semibold">{t('settings.path')}</TableHead>
                              <TableHead className="text-[12px] font-semibold">{t('settings.endpointDesc')}</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            <TableRow>
                              <TableCell><Badge variant="default" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/chat/completions</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.openaiCompat')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/responses</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.responsesApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/images/generations</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageGenerationApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="outline" className="text-[12px]">POST</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/images/edits</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.imageEditApi')}</TableCell>
                            </TableRow>
                            <TableRow>
                              <TableCell><Badge variant="secondary" className="text-[12px]">GET</Badge></TableCell>
                              <TableCell className="font-mono text-[13px]">/v1/models</TableCell>
                              <TableCell className="text-[13px] text-muted-foreground">{t('settings.modelList')}</TableCell>
                            </TableRow>
                          </TableBody>
                        </Table>
                      </div>
                    </div>
                </SettingsCard>
              </SettingsSection>
            </>
          ) : null}
          </div>
        </div>

        {/* Bottom action bar that appears only when manually saved fields have changed; toggles and dropdowns auto-save, so they do not need it. */}
        {dirtyCount > 0 ? (
          <div
            role="status"
            className="sticky bottom-3 z-30 mt-2 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-amber-500/30 bg-card/95 px-4 py-2.5 shadow-lg backdrop-blur-md max-lg:bottom-[calc(5.5rem+env(safe-area-inset-bottom,0px))]"
          >
            <div className="flex min-w-0 items-center gap-2.5">
              <span className="size-2 shrink-0 rounded-full bg-amber-500" aria-hidden="true" />
              <div className="min-w-0">
                <div className="text-sm font-semibold text-foreground">{t('settings.saveStatusUnsaved', { n: dirtyCount })}</div>
                <p className="text-[11px] leading-relaxed text-muted-foreground sm:text-xs">{t('settings.saveStatusUnsavedHint')}</p>
              </div>
            </div>
            <div className="flex shrink-0 items-center gap-2 max-sm:w-full">
              <Button variant="ghost" size="sm" onClick={discardChanges} disabled={savingSettings} className="max-sm:flex-1">
                <RotateCcw className="size-3.5" />
                {t('settings.discardChanges')}
              </Button>
              {renderSaveButton('max-sm:flex-1')}
            </div>
          </div>
        ) : null}
      </>
    </StateShell>
  )
}
