export type ToastType = 'success' | 'error' | 'warning' | 'info'
export type ISODateString = string
export type UpstreamChannel = 'codex'

export interface ToastState {
  msg: string
  type: ToastType
}

export type AccountStatus = 'active' | 'ready' | 'cooldown' | 'error' | 'refreshing' | 'paused' | 'quota_paused' | string
export type CodexClientMetadataMode = 'auto' | 'always' | 'off'
/** Device fingerprint convergence level for official outbound Codex requests, default off (no convergence). */
export type CodexFingerprintMode = 'off' | 'device' | 'session' | 'single_machine_multi_window' | 'full'
export type ModelCooldownMode = 'off' | 'fixed' | 'adaptive'

export type ResponseCacheWritePolicy = 'always' | 'on_demand'

export interface StatsChannelCounts {
  total: number
  available: number
  rate_limited: number
  error: number
  today_requests: number
}

export interface StatsResponse {
  total: number
  available: number
  rate_limited: number
  error: number
  today_requests: number
  // Account and today's request counts, split by upstream channel (codex)
  channels?: Record<string, StatsChannelCounts>
}

export interface AccountUsageWindow {
  requests: number
  tokens: number
  account_billed?: number
  user_billed?: number
  model_counts?: Record<string, number>
  model_success_counts?: Record<string, number>
  model_avg_first_token_ms?: Record<string, number>
}

export type SubscriptionBusinessStatus = 'active' | 'expiring_today' | 'expired' | 'grace_period' | 'unknown'
export type SubscriptionSyncState = 'confirmed' | 'pending' | 'failed' | 'unknown' | 'unsupported'
export type SubscriptionAutoRenew = 'enabled' | 'disabled' | 'unsupported' | 'unknown'

/** Subscription status object: business status (expiry check) and sync status (data freshness) are expressed separately. */
export interface SubscriptionStatus {
  business_status: SubscriptionBusinessStatus
  plan?: string
  expires_at?: ISODateString
  days_remaining: number
  days_overdue: number
  last_known_status?: SubscriptionBusinessStatus
  auto_renew: SubscriptionAutoRenew
  grace_until?: ISODateString
  last_checked_at?: ISODateString
  source?: 'jwt' | 'plan_header' | 'provider_api' | string
  sync_state: SubscriptionSyncState
  renewal_detected_at?: ISODateString
  error?: string
  timezone: string
}

export type SubscriptionRefreshOutcome = 'updated' | 'unchanged' | 'no_subscription' | 'unsupported' | 'failed'

export interface SubscriptionRefreshResponse {
  outcome: SubscriptionRefreshOutcome
  error?: string
  subscription?: SubscriptionStatus
  subscription_expires_at?: ISODateString
}

export interface AccountRow {
  codex_last_refresh_at?: string
  codex_refresh_error?: string
  upstream_request_id_header?: string | null
  detail_loaded?: boolean
  id: number
  name: string
  email: string
  email_domain?: string
  chatgpt_account_id?: string
  token_workspace_id?: string
  workspace_id_override?: string
  effective_workspace_id?: string
  plan_type: string
  subscription_expires_at?: string
  /** Subscription status object calculated by the server in the business timezone; omitted for plans that do not track a subscription (api, or free without an expiry time). */
  subscription?: SubscriptionStatus
  status: AccountStatus
  error_message?: string
  at_only?: boolean
  access_token_type?: string
  account_type?: string
  agent_identity?: boolean
  project_id?: string
  avatar_url?: string
  verified_email?: boolean
  base_url?: string
  models?: string[]
  model_mapping?: string
  codex_client_metadata_mode?: CodexClientMetadataMode
  codex_fingerprint_mode?: CodexFingerprintMode
  timezone?: string
  /** Account page link; when empty, falls back to opening base_url (api-base). */
  account_href?: string
  custom_headers?: Record<string, string> | null
  health_tier?: string
  scheduler_score?: number
  dispatch_score?: number
  score_bias_override?: number | null
  score_bias_effective?: number
  base_concurrency_override?: number | null
  base_concurrency_effective?: number
  skip_warm_tier?: boolean
  keep_concurrency_on_degrade?: boolean
  dynamic_concurrency_limit?: number
  allowed_api_key_ids?: number[]
  tags?: string[]
  // General note; accounts submitted through self-service carry a note such as "Self-service submission contact: ...".
  note?: string
  group_ids?: number[]
  scheduler_breakdown?: {
    unauthorized_penalty: number
    rate_limit_penalty: number
    timeout_penalty: number
    server_penalty: number
    failure_penalty: number
    success_bonus: number
    usage_penalty_7d: number
    usage_urgency_bonus_5h?: number
    usage_urgency_bonus_7d?: number
    latency_penalty: number
    success_rate_penalty?: number
  }
  last_unauthorized_at?: ISODateString
  last_rate_limited_at?: ISODateString
  last_timeout_at?: ISODateString
  last_server_error_at?: ISODateString
  proxy_url: string
  created_at: ISODateString
  updated_at: ISODateString
  codex_usage_updated_at?: ISODateString
  active_requests?: number
  occupied_requests?: number
  session_slot_buffer_enabled?: boolean
  total_requests?: number
  last_used_at?: ISODateString
  success_requests?: number
  error_requests?: number
  retry_error_requests?: number
  rate_limit_attempts?: number
  error_status_counts?: Record<string, number>
  success_model_counts?: Record<string, number>
  usage_percent_7d?: number | null
  usage_percent_5h?: number | null
  usage_percent_spark?: number | null
  rate_limit_reset_credits?: number | null
  daybreak_supported?: boolean
  daybreak_models?: Record<string, string[]>
  daybreak_checked_at?: number
  applicable_reset_credits?: number | null
  credits_valid?: boolean
  credits_balance?: string | null
  credits_has_credits?: boolean | null
  credits_unlimited?: boolean | null
  credits_overage_limit_reached?: boolean | null
  credits_spend_control_reached?: boolean | null
  credits_rate_limit_reached_type?: string | null
  auto_pause_5h_threshold?: number | null
  auto_pause_7d_threshold?: number | null
  auto_pause_5h_disabled?: boolean
  auto_pause_7d_disabled?: boolean
  ignore_usage_limit_status_override?: boolean | null
  ignore_usage_limit_status_effective?: boolean
  dispatch_count_limit?: number | null
  scheduler_priority?: number | null
  dispatch_count_used?: number
  dispatch_count_reset_at?: ISODateString
  dispatch_count_limited?: boolean
  usage_5h_detail?: AccountUsageWindow
  usage_7d_detail?: AccountUsageWindow
  // Today's gateway-side aggregate (from 00:00 in the server timezone), filled in by page-stats.
  usage_today_detail?: AccountUsageWindow
  reset_5h_at?: ISODateString
  reset_7d_at?: ISODateString
  reset_spark_at?: ISODateString
  // Real type of the long window (7d slot): "monthly" (free/team monthly window), "weekly", or unknown.
  // The long window of free/team plans is actually about 30 days, so the label should show 30d instead of 7d (issue #324).
  usage_window_7d_kind?: 'monthly' | 'weekly' | ''
  usage_window_7d_seconds?: number
  billed_5h?: number
  billed_7d?: number
  // Cumulative cost (USD) on the official billing basis. From the full-window account_daily_usage snapshot,
  // This is a separate ledger from billed_7d (gateway cost computed from local logs); the list shows them side by side.
  official_usd?: number
  // Kept for compatibility with the old page-stats field; the value is the same as official_usd and no longer means "7 days only".
  official_usd_7d?: number
  // The official snapshot has synced successfully before, but the upstream window has no data (official stats lag behind).
  // When this flag is set, page-stats is not fetched again, and the pill shows a static "No data" instead of a spinner.
  official_usage_synced?: boolean
  cooldown_until?: ISODateString
  cooldown_reason?: string
  model_cooldowns?: Array<{
    model: string
    reason: string
    reset_at: ISODateString
    remaining_seconds: number
  }>
  model_cooldown_mode_override?: ModelCooldownMode | null
  model_cooldown_seconds_override?: number | null
  model_cooldown_backoff_override?: boolean | null
  model_cooldown_mode_effective?: ModelCooldownMode
  model_cooldown_seconds_effective?: number
  model_cooldown_backoff_effective?: boolean
  enabled?: boolean
  locked?: boolean
  credit_enabled?: boolean
  credit_skip_usage_window?: boolean
  // using_credits sits alongside status: the usage window is full but credits keep it going, so status is still active
  // (it is truly schedulable). This flag is only used to show a "Using credits" badge next to the status badge.
  using_credits?: boolean
  // Image quota info
  image_quota_remaining?: number
  image_quota_total?: number
  today_used_count?: number
  image_quota_reset_at?: ISODateString
}

export type AccountsResponse = ApiListResponse<'accounts', AccountRow>

export interface AccountListSummary {
  total: number
  normal: number
  active: number
  overload_paused: number
  rate_limited: number
  rate_limited_5h: number
  rate_limited_7d: number
  abnormal: number
  banned: number
  error: number
  unsampled: number
  disabled: number
  locked: number
  healthy: number
  warm: number
  risky: number
  oauth: number
  api_key: number
  subscription_unlocked: number
  unauthorized_24h: number
  rate_limited_1h: number
  timeout_15m: number
  self_service_pending?: number
}

export interface AccountEmailDomainFacet {
  domain: string
  total: number
  banned: number
}

export interface AccountsPageResponse extends AccountsResponse {
  page: number
  page_size: number
  total: number
  summary: AccountListSummary
  facets: {
    tags: string[]
    email_domains: AccountEmailDomainFacet[]
  }
  snapshot_at: ISODateString
  stats_state: 'ready' | 'stale' | 'warming'
  disabled_sorts?: string[]
}

export interface AccountPageStatsItem {
  usage_5h_detail?: AccountUsageWindow
  usage_7d_detail?: AccountUsageWindow
  usage_today_detail?: AccountUsageWindow
  billed_5h?: number
  billed_7d?: number
  official_usd?: number
  official_usd_7d?: number
  official_usage_synced?: boolean
}

export interface AccountPageStatsResponse {
  stats: Record<string, AccountPageStatsItem>
}

export interface AccountLiveStateResponse {
  accounts: Record<string, {
    active_requests: number
    occupied_requests: number
    // Concurrency limit the scheduler is actually enforcing and the configured value; keep the list value when an older backend does not return it.
    dynamic_concurrency_limit?: number
    base_concurrency_effective?: number
    // Runtime usage projection: keep the list value when an older backend lacks the key; an explicit null means the window is no longer valid.
    status?: AccountStatus
    using_credits?: boolean
    plan_type?: string
    credits_valid?: boolean
    credits_balance?: string | null
    credits_has_credits?: boolean | null
    credits_unlimited?: boolean | null
    credits_overage_limit_reached?: boolean | null
    credits_spend_control_reached?: boolean | null
    credits_rate_limit_reached_type?: string | null
    usage_percent_5h?: number | null
    reset_5h_at?: ISODateString | null
    codex_5h_usage_updated_at?: ISODateString | null
    usage_percent_7d?: number | null
    reset_7d_at?: ISODateString | null
    codex_usage_updated_at?: ISODateString | null
    // Most recently applied WS quota frame; older backends do not return it, and it is only used to observe frame ingestion.
    codex_usage_frame_at?: ISODateString
    codex_usage_frame_kind?: string
  }>
  session_slot_buffer_enabled: boolean
}

export type SubscriptionFilter =
  | 'all'
  | 'active'
  | 'expiring_9d'
  | 'expiring_3d'
  | 'expiring_today'
  | 'expired'
  | 'grace_period'
  | 'pending'
  | 'failed'
  | 'unknown'

export const SUBSCRIPTION_FILTER_OPTIONS: SubscriptionFilter[] = [
  'all',
  'expiring_9d',
  'expiring_3d',
  'expiring_today',
  'expired',
  'grace_period',
  'active',
  'pending',
  'failed',
  'unknown',
]

export interface AccountsPageParams {
  channel?: UpstreamChannel
  page: number
  pageSize: number
  search?: string
  status?: string
  plan?: string
  authKind?: string
  tag?: string
  emailDomain?: string
  groupInclude?: number[]
  groupExclude?: number[]
  ungrouped?: boolean
  healthTier?: 'healthy' | 'warm' | 'risky' | 'banned' | 'attention'
  proxyUrl?: string
  proxyFilter?: 'all' | 'unbound' | 'this' | 'other'
  /** Subscription status filter (Codex channel); values are listed in SUBSCRIPTION_FILTER_OPTIONS. */
  subscription?: SubscriptionFilter
  sort?: 'requests' | 'today' | 'usage' | 'created_at' | 'updated_at' | 'scheduler_priority' | 'group' | 'risk' | 'dispatch_score' | 'latency_penalty' | 'unauthorized' | 'id'
  order?: 'asc' | 'desc'
}

export interface AccountQuotaAnalysisBucket {
  min: number
  max: number
  count: number
}

export interface AccountQuotaAnalysis {
  total: number
  sampled: number
  unsampled: number
  high_usage: number
  exhausted: number
  average_used: number | null
  buckets: AccountQuotaAnalysisBucket[]
}

export interface AccountAnalysisTimeBucket {
  start_at: number
  end_at: number
  count: number
  cooldown_count?: number
}

export interface AccountRecoveryAnalysis {
  total: number
  recoverable: number
  unknown: number
  next_at: number | null
  buckets: AccountAnalysisTimeBucket[]
}

export interface AccountResetAnalysis {
  total: number
  known: number
  unknown: number
  next_at: number | null
  buckets: AccountAnalysisTimeBucket[]
}

export interface AccountPressureForecastAnalysis {
  sampled: number
  threshold: number
  predicted_at: number | null
  predicted_count: number
  unknown: number
  rpm: number
  effective_rpm_limit: number
  rpm_pressure: number | null
  active_pressure: number
  rate_limit_pressure: number
  dispatchable_accounts: number
  avg_concurrency: number
  high_pressure_at: number | null
  supply_shortage_at: number | null
  risk_level: 'low' | 'medium' | 'high'
  confidence: number
}

export interface AccountAnalysisResponse {
  channel: UpstreamChannel
  quota: Record<'5h' | '7d', AccountQuotaAnalysis>
  recovery: Record<'5h' | '7d', AccountRecoveryAnalysis>
  reset: AccountResetAnalysis
  forecasts: Record<'5h' | '7d', AccountPressureForecastAnalysis>
  snapshot_at: ISODateString
  stats_state: 'ready' | 'stale' | 'warming'
}

export interface AccountOperationSelector {
  channel: UpstreamChannel
  search?: string
  status?: string
  plan?: string
  auth_kind?: string
  tag?: string
  email_domain?: string
  group_include?: number[]
  group_exclude?: number[]
  ungrouped?: boolean
  refreshable_only?: boolean
  subscription_unlocked?: boolean
  subscription?: SubscriptionFilter
}

// Validity details for a single "active reset count" coupon (issue #322).
export interface ResetCreditItem {
  id: string
  granted_at?: ISODateString
  expires_at: ISODateString
}

export interface ResetCreditsDetailResponse {
  available_count: number
  credits: ResetCreditItem[]
}

// AccountHealthBucket holds the request success and failure counts for a single time window of the health status bar.
export interface AccountHealthBucket {
  success: number
  failed: number
}

// AccountHealthBarsResponse is the response of GET /api/accounts/health-bars.
// buckets maps each account ID (string) to block_count time buckets, ordered from oldest to newest.
export interface AccountHealthBarsResponse {
  buckets: Record<string, AccountHealthBucket[]>
  block_count: number
  block_minutes: number
}

export interface InviteItem {
  referral_id?: string
  email?: string
  invite_url?: string
}

export interface InviteResult {
  ok: boolean
  status_code: number
  request_id?: string
  program_id: string
  entrypoint: string
  emails: string[]
  invites?: InviteItem[]
  // upstream_message is the reason from the upstream detail (for example "This person has already received a referral invite"),
  // failed_emails are the rejected recipients. Rejection at the recipient level is unrelated to account eligibility, so do not report it as "account not eligible".
  upstream_message?: string
  failed_emails?: string[]
  // challenged being true means the request was stopped by a Cloudflare challenge, not a business decision from upstream.
  // In this case status_code (usually 403) must not be read as "not eligible"; show a retry hint instead.
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

export interface InviteResponse {
  ok: boolean
  result: InviteResult
  // recorded_emails are the emails whose invite records were written successfully in this request; they may be missing from failed responses.
  recorded_emails?: string[]
}

// InviteRecipientRecord is one invited recipient. The backend enforces uniqueness on trim + lower(email)
// as a unique constraint; the frontend keeps the original email for display only, and the other fields help identify the source and time.
export interface InviteRecipientRecord {
  email: string
  state: string
  sender_account_id?: number
  invited_at?: ISODateString
}

export interface InviteRecipientsCheckResponse {
  recipients: InviteRecipientRecord[]
}

// InviteGrant is one reward entry (one each for the inviter and the invitee).
export interface InviteGrant {
  recipient?: string
  grant_type?: string
  amount?: number
  reward_id?: string
}

// InviteTimeFrameRule is one quota rule. capacity_type distinguishes two independent limits:
// send is the number of sends and reward is the number of times a reward can be earned (the latter is usually much smaller than the former).
export interface InviteTimeFrameRule {
  invites_sent: number
  invites_total: number
  time_frame?: string
  type?: string
  capacity_type?: string
}

// InviteEligibility is the result of GET /api/accounts/:id/invite/eligibility.
// A missing remaining_* field means upstream did not provide it, which differs from an explicit 0, so do not treat it as quota exhausted.
export interface InviteEligibility {
  ok: boolean
  status_code: number
  request_id?: string
  should_show: boolean
  ineligible_reason?: string
  ineligible_reason_code?: string
  program_id?: string
  entrypoint?: string
  offer_id?: string
  grants?: InviteGrant[]
  remaining_send_capacity?: number
  remaining_reward_capacity?: number
  title?: string
  description?: string
  rules?: string[]
  time_frame_rules?: InviteTimeFrameRule[]
  requires_explicit_confirmation?: boolean
  upstream_message?: string
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

// InviteCacheMeta says whether this result was fetched live or served from cache, and when it was fetched.
// source=upstream means upstream was just queried; runtime and snapshot come from the runtime cache and the database snapshot respectively.
export interface InviteCacheMeta {
  source: 'upstream' | 'runtime' | 'snapshot'
  observed_at?: string
  expires_at?: string
}

export interface InviteEligibilityResponse {
  ok: boolean
  result: InviteEligibility
  cache?: InviteCacheMeta
}

// InviteTrackingItem is one sent invite record.
export interface InviteTrackingItem {
  referral_id?: string
  email?: string
  status?: string
  can_resend?: boolean
  invite_url?: string
  resend_available_at?: string
  grants?: InviteGrant[]
  created_at?: string
  expires_at?: string
}

export interface InviteTracking {
  ok: boolean
  status_code: number
  request_id?: string
  items: InviteTrackingItem[]
  cursor?: string
  upstream_message?: string
  challenged?: boolean
  upstream?: unknown
  upstream_raw?: string
}

export interface InviteTrackingResponse {
  ok: boolean
  result: InviteTracking
  cache?: InviteCacheMeta
}

// InviteGuideAccountPlan is the invite earnings estimate for a single account in the import guide.
// state meanings: pending = no probe result yet, eligible = eligible and reward attempts remain,
// exhausted = eligible but this month's reward attempts are used up (sending still earns no credits), ineligible = upstream judged not eligible.
export type InviteGuideState = 'pending' | 'eligible' | 'exhausted' | 'ineligible'

export interface InviteGuideAccountPlan {
  id: number
  email?: string
  plan_type?: string
  state: InviteGuideState
  // A missing remaining_* field means upstream did not provide it, which differs from an explicit 0.
  remaining_send_capacity?: number
  remaining_reward_capacity?: number
  // grant_amount is the amount the inviter gets per invite, excluding the invitee's share.
  grant_amount?: number
  // This month's send usage, from time_frame_rules in the eligibility API. It is not the same
  // window: this is the "month", and the invites_* window is the 90 days of invite records.
  monthly_sent?: number
  monthly_send_total?: number
  // Actual invite records from the last 90 days. A missing field means "no tracking data", which differs from "actually 0" —
  // The import probe only fetches eligibility and not records, so most accounts never have this data to begin with.
  invites_sent?: number
  invites_accepted?: number
  invites_pending?: number
  potential_credits: number
  offer_id?: string
  title?: string
  ineligible_reason?: string
  suggested_invites: number
  observed_at?: string
}

export interface InviteGuidePlan {
  enabled: boolean
  total: number
  probed: number
  pending: number
  unprobed: number
  eligible: number
  probe_cap: number
  total_reward_slots: number
  total_potential_credits: number
  email_budget: number
  accounts: InviteGuideAccountPlan[]
}

export interface RecycleBinAccountRow {
  id: number
  name: string
  email: string
  plan_type: string
  at_only?: boolean
  access_token_type?: string
  base_url?: string
  models?: string[]
  created_at: ISODateString
  deleted_at?: ISODateString
  last_test_status?: string
  last_test_at?: ISODateString
}

export type RecycleBinAccountsResponse = ApiListResponse<'accounts', RecycleBinAccountRow>

export interface AddAccountRequest {
  name?: string
  refresh_token?: string
  session_token?: string
  proxy_url: string
  allow_duplicate?: boolean
  custom_headers?: Record<string, string> | null
  /** Account group bound directly when adding or importing; when an existing account is matched, its group is not changed. */
  group_ids?: number[]
}

export interface AddATAccountRequest {
  name?: string
  access_token: string
  proxy_url: string
  allow_duplicate?: boolean
  custom_headers?: Record<string, string> | null
  /** Account group bound directly when adding or importing; when an existing account is matched, its group is not changed. */
  group_ids?: number[]
}

// Imports a Codex Agent Identity auth.json (auth_mode=agentIdentity, dynamic signing, AT/RT are not stored).
export interface ImportAgentIdentityRequest {
  name?: string
  auth_json: string
  proxy_url?: string
}

export interface ImportAgentIdentityResponse {
  message: string
  id: number
  email?: string
}

// Batch import of Agent Identity auth.json files (each item is the raw JSON content of one file).
export interface AgentIdentityBatchImportRequest {
  files: string[]
  proxy_url?: string
}

export interface AgentIdentityImportItem {
  email?: string
  id?: number
  ok: boolean
  error?: string
}

export interface AgentIdentityBatchImportResponse {
  total: number
  imported: number
  failed: number
  items: AgentIdentityImportItem[]
}

export interface UpdateAccountSchedulerRequest {
  upstream_request_id_header?: string | null
  score_bias_override?: number | null
  base_concurrency_override?: number | null
  skip_warm_tier?: boolean
  keep_concurrency_on_degrade?: boolean
  allowed_api_key_ids?: number[] | null
  proxy_url?: string | null
  tags?: string[] | null
  group_ids?: number[] | null
  auto_pause_5h_threshold?: number | null
  auto_pause_7d_threshold?: number | null
  auto_pause_5h_disabled?: boolean
  auto_pause_7d_disabled?: boolean
  ignore_usage_limit_status_override?: boolean | null
  dispatch_count_limit?: number | null
  scheduler_priority?: number | null
  custom_headers?: Record<string, string> | null
  codex_fingerprint_mode?: CodexFingerprintMode | null
  timezone?: string | null
  account_href?: string | null
}

export interface BatchUpdateAccountsRequest extends UpdateAccountSchedulerRequest {
  ids?: number[]
  selector?: AccountOperationSelector
  enabled?: boolean
  locked?: boolean
}

export interface AccountGroup {
  id: number
  name: string
  description: string
  color: string
  sort_order: number
  member_count: number
  base_concurrency_override: number | null
  auto_pause_5h_threshold: number
  auto_pause_7d_threshold: number
  proxy_urls: string[]
  channel: UpstreamChannel
  created_at: ISODateString
  updated_at: ISODateString
}

export interface AccountGroupsResponse {
  groups: AccountGroup[]
}

export interface CreateAccountGroupRequest {
  name: string
  description?: string
  color?: string
  sort_order?: number
  base_concurrency_override?: number | null
  auto_pause_5h_threshold?: number
  auto_pause_7d_threshold?: number
  proxy_urls?: string[]
  channel?: UpstreamChannel
}

export interface UpdateAccountGroupRequest {
  name?: string
  description?: string
  color?: string
  sort_order?: number
  base_concurrency_override?: number | null
  auto_pause_5h_threshold?: number
  auto_pause_7d_threshold?: number
  proxy_urls?: string[]
  channel?: UpstreamChannel
}

export interface AccountModelStat {
  model: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  account_billed: number
  user_billed: number
}

// Official settled usage (the snapshot stored from wham daily-workspace-usage-counts).
// This is a different basis from the local usage_logs aggregate: this one is the authoritative billing data on the OpenAI side.
export interface WhamDailyUsageSplit {
  client_id?: string
  model?: string
  users: number
  threads: number
  turns: number
  credits: number
  uncached_text_input_tokens?: number
  cached_text_input_tokens?: number
  text_output_tokens?: number
  text_total_tokens?: number
}

// Share of a single (model, speed) pair on a given day (from wham daily-token-usage-breakdown, converted per day after it is stored).
// share is the proportion within that day (0 to 1), only meaningful for that day and not additive across days;
// credits/usd are the official daily costs the backend has already split by share, so use these two for cross-day totals.
// Free plans always have 0 credits, but share is still valid. speed of fast means the priority tier.
export interface WhamDailyUsageBreakdownEntry {
  model: string
  speed: 'standard' | 'fast' | string
  share: number
  credits: number
  usd: number
}

// Daily share by product entry point (cli / desktop_app / vscode / exec / web …), same meaning as above.
export interface WhamDailyUsageSurfaceEntry {
  surface: string
  share: number
  credits: number
  usd: number
}

export interface WhamDailyUsageItem {
  day: string
  credits: number
  usd: number
  users: number
  threads: number
  turns: number
  uncached_input_tokens: number
  cached_input_tokens: number
  output_tokens: number
  total_tokens: number
  // settled=false means this day is still being settled (rows for the current UTC day are always unsettled): token and credits may
  // already have values, but they keep changing all day and only stabilize after the next day's backfill.
  settled: boolean
  clients: WhamDailyUsageSplit[]
  models: WhamDailyUsageSplit[]
  // Whether the model × speed breakdown has been synced for this day; older snapshots do not have these three fields.
  breakdown_available?: boolean
  breakdown?: WhamDailyUsageBreakdownEntry[]
  surfaces?: WhamDailyUsageSurfaceEntry[]
}

// Official cost and quota estimate for the current reset period. Estimate = official cost used this period ÷ live used percentage;
// the percentage is an integer, the range is estimated at ±0.5%, and it is unreliable below 10%.
export interface WhamDailyUsageCycle {
  // Both the window info and the live percentage were obtained; when false, check reason.
  available: boolean
  reason?: 'no_window' | 'window_stale' | 'no_percent' | 'no_credits' | 'percent_too_low' | string
  start_at?: string
  reset_at?: string
  window_seconds?: number
  window_kind?: 'weekly' | 'monthly' | ''
  used_percent?: number
  used_percent_updated_at?: string
  used_credits: number
  used_usd: number
  // Number of days in this period that have official settlement data.
  days: number
  estimate?: {
    usd: number
    usd_low: number
    usd_high: number
    reliable: boolean
  }
}

export interface WhamDailyUsageResponse {
  days: number
  items: WhamDailyUsageItem[]
  totals: {
    credits: number
    usd: number
    total_tokens: number
    turns: number
  }
  credits_per_usd: number
  // Days upstream can backfill (the deep backfill window of the first sync); older history exists only in the local snapshot.
  retention_days: number
  last_synced_at?: string
  // Reason the counts endpoint refresh failed (the stored snapshot is shown in this case).
  refresh_error?: string
  // Only the per-model breakdown endpoint failed to refresh: counts refreshed successfully, but the per-model costs may lag by one round.
  breakdown_refresh_error?: string
  // Official cost used and quota estimate for the current reset period (7d or monthly window); only Codex OAuth accounts that have window info have this.
  cycle?: WhamDailyUsageCycle
}

export interface AccountUsageDayStat {
  date: string
  label: string
  requests: number
  tokens: number
  account_billed: number
  user_billed: number
}

export interface AccountKeyStat {
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  requests: number
  tokens: number
  account_billed: number
  user_billed: number
}

export interface AccountUsageDetail {
  period_days: number
  active_days: number
  total_requests: number
  total_tokens: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  cache_hit_rate: number
  total_account_billed: number
  total_user_billed: number
  avg_daily_account_billed: number
  avg_daily_user_billed: number
  avg_daily_requests: number
  avg_daily_tokens: number
  avg_duration_ms: number
  avg_first_token_ms: number
  p95_duration_ms: number
  error_requests: number
  error_rate: number
  retry_requests: number
  first_token_samples: number
  stream_requests: number
  stream_rate: number
  compact_requests: number
  compact_rate: number
  today: AccountUsageDayStat
  highest_cost_day?: AccountUsageDayStat
  highest_request_day?: AccountUsageDayStat
  history: AccountUsageDayStat[]
  models: AccountModelStat[]
  by_api_key: AccountKeyStat[]
}

export interface MessageResponse {
  message: string
  warning?: string
}

export interface SystemUpdateInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  supported: boolean
  unsupported_reason?: string
  runtime_os: string
  runtime_arch: string
  mode: string
  release_url?: string
  asset_name?: string
  published_at?: string
  warning?: string
}

export interface SystemUpdateResult extends MessageResponse {
  current_version: string
  latest_version: string
  need_restart: boolean
  restarting: boolean
  mode: string
  backup_path?: string
}

export interface CreateAccountResponse extends MessageResponse {
  id: number
}

export interface AdminErrorResponse {
  error: string
}

export interface HealthResponse {
  status: 'ok' | string
  build_version?: string
  available: number
  total: number
}

export interface SiteBranding {
  site_name: string
  site_logo: string
  background_image: string
  background_opacity: number
  background_blur: number
  background_glass_opacity: number
  background_glass_blur: number
}

export interface BackgroundUploadResponse {
  url: string
  filename: string
  mime_type: string
  bytes: number
}

export interface AccountEventTrendPoint {
  bucket: string
  added: number
  deleted: number
}

export interface OpsOverviewResponse {
  updated_at: ISODateString
  uptime_seconds: number
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  cpu: {
    percent: number
    cores: number
  }
  memory: {
    percent: number
    used_bytes: number
    total_bytes: number
    process_bytes: number
    container_used_bytes?: number
    container_limit_bytes?: number
    container_percent?: number
    container_source?: 'cgroup' | 'process'
    heap_alloc_bytes?: number
    heap_inuse_bytes?: number
    heap_released_bytes?: number
    num_gc?: number
  }
  response_cache?: {
    effective_config: {
      generation: number
      local_max_bytes: number
      local_max_entry_bytes: number
      reconstruct_max_bytes: number
      write_policy?: ResponseCacheWritePolicy
    }
    applied_config: {
      generation: number
      local_max_bytes: number
      local_max_entry_bytes: number
      reconstruct_max_bytes: number
      write_policy?: ResponseCacheWritePolicy
    }
    entries: number
    max_entries: number
    current_bytes: number
    max_bytes: number
    high_water_bytes: number
    largest_entry_bytes: number
    shared_payload_bytes?: number
    local_hits: number
    local_misses: number
    remote_hits: number
    remote_misses: number
    expirations: number
    count_evictions: number
    byte_evictions: number
    oversize_bypasses: number
    oversize_rejections: number
    known_unavailable_errors: number
    skipped_writes?: number
    chain_owners?: number
    last_config_sync_at: ISODateString | ''
    last_config_sync_error: string
  }
  runtime: {
    goroutines: number
    available_accounts: number
    total_accounts: number
  }
  requests: {
    active: number
    total: number
  }
  scheduler?: {
    engine: 'legacy' | 'shadow' | 'indexed' | string
    selection_total: number
    selection_fast_hit: number
    selection_slow_hit: number
    selection_miss: number
    selection_duration_ns: number
    slow_scanned_accounts: number
    wait_started: number
    wait_wakeups: number
    wait_timeouts: number
    wait_canceled: number
    waiters: number
    availability_signals: number
    snapshot_generation: number
    snapshot_account_count: number
    last_snapshot_at: ISODateString | ''
    outbox_watermark: number
    outbox_high_watermark: number
    outbox_backlog: number
    outbox_events: number
    outbox_batches: number
    outbox_errors: number
    outbox_lag_ms: number
    outbox_last_applied_at: ISODateString | ''
    routing_cache_hits: number
    routing_cache_misses: number
    routing_cache_builds: number
    routing_cache_fallbacks: number
    routing_cache_invalidations: number
    routing_cache_evictions: number
    routing_cache_entries: number
    routing_cache_accounts: number
    shadow_checks: number
    shadow_agreements: number
    shadow_mismatches: number
  }
  postgres: {
    healthy: boolean
    open: number
    in_use: number
    idle: number
    max_open: number
    wait_count: number
    usage_percent: number
  }
  redis: {
    healthy: boolean
    total_conns: number
    idle_conns: number
    stale_conns: number
    pool_size: number
    usage_percent: number
  }
  traffic: {
    qps: number
    qps_peak: number
    tps: number
    tps_peak: number
    rpm: number
    tpm: number
    error_rate: number
    today_requests: number
    today_tokens: number
    rpm_limit: number
    avg_duration_ms: number
  }
}

export type RuntimeHealthStatus = 'ok' | 'degraded' | 'error' | string

export interface RuntimeCheck {
  component: string
  status: RuntimeHealthStatus
  code: string
  message: string
}

export interface RuntimeStatusResponse {
  updated_at: ISODateString
  status: RuntimeHealthStatus
  service: {
    status: RuntimeHealthStatus
    service_url: string
    admin_url: string
    api_base_url: string
    uptime_seconds: number
    goroutines: number
    go_version: string
    os: string
    arch: string
    pid: number
  }
  database: {
    status: RuntimeHealthStatus
    driver: string
    label: string
    location: string
    healthy: boolean
    error?: string
    open: number
    in_use: number
    idle: number
    max_open: number
    wait_count: number
    usage_percent: number
  }
  cache: {
    status: RuntimeHealthStatus
    driver: string
    label: string
    healthy: boolean
    error?: string
    total_conns: number
    idle_conns: number
    stale_conns: number
    pool_size: number
    usage_percent: number
  }
  usage_log: {
    status: RuntimeHealthStatus
    mode: string
    enabled: boolean
    batch_size: number
    flush_interval_seconds: number
    buffer_length: number
    buffer_capacity: number
    buffer_limit?: number
    dropped_total?: number
  }
  probes: {
    status: RuntimeHealthStatus
    lazy_mode: boolean
    background_refresh_interval_minutes: number
	    usage_probe_max_age_minutes: number
	    usage_probe_concurrency: number
	    usage_probe_responses_fallback_enabled: boolean
	    recovery_probe_interval_minutes: number
    usage_probe_running: boolean
    recovery_probe_running: boolean
    auto_cleanup_running: boolean
  }
  accounts: {
    status: RuntimeHealthStatus
    total: number
    available: number
    active_requests: number
    total_requests: number
    status_counts: Record<string, number>
  }
  image_storage: {
    status: RuntimeHealthStatus
    backend: string
    local_dir?: string
    bucket?: string
    prefix?: string
    healthy: boolean
    error?: string
  }
  admin_auth: {
    status: RuntimeHealthStatus
    source: string
    configured: boolean
  }
  checks: RuntimeCheck[]
}

export interface PromptFilterPatternQuarantine {
  index: number
  name: string
  code: string
  message: string
}


/** Who currently handles outbound traffic for the Codex channel: resin means a full-layer override, proxy_chain means resolution in the order account > group > proxy pool > global > direct. */
export interface CodexEgressSummary {
  mode: 'resin' | 'proxy_chain' | string
  resin_enabled: boolean
  /** Masked Resin address (token excluded), for display only. */
  resin_endpoint?: string
  resin_platform_name?: string
}

export interface SystemSettings {
  site_name: string
  site_logo: string
  background_image: string
  background_opacity: number
  background_blur: number
  background_glass_opacity: number
  background_glass_blur: number
  max_concurrency: number
  global_rpm: number
  test_model: string
  test_content: string
  test_concurrency: number
  background_refresh_interval_minutes: number
	  usage_probe_max_age_minutes: number
	  usage_probe_concurrency: number
	  usage_probe_responses_fallback_enabled: boolean
	  recovery_probe_interval_minutes: number
  lazy_mode: boolean
  codex_oauth_keepalive_enabled: boolean
  proxy_url?: string
  pg_max_conns: number
  redis_pool_size: number
  auto_clean_unauthorized: boolean
  auto_clean_rate_limited: boolean
  admin_secret: string
  admin_secret_configured?: boolean
  admin_auth_source: 'env' | 'database' | 'disabled' | string
  auto_clean_full_usage: boolean
  auto_clean_error: boolean
  auto_clean_expired: boolean
  auto_reset_credits_on_exhaustion_enabled: boolean
  auto_reset_credits_enabled: boolean
  auto_reset_credits_before_expiry_min: number
  auto_activate_5h_window_enabled: boolean
  proxy_pool_enabled: boolean
  fast_scheduler_enabled: boolean
  scheduler_engine: 'legacy' | 'shadow' | 'indexed'
  codex_force_websocket: boolean
  codex_telemetry_enabled: boolean
  codex_telemetry_timing_debug: boolean
  codex_request_compression: boolean
  codex_ws_weak_network_mode: boolean
  codex_ws_keepalive_enabled: boolean
  codex_ws_keepalive_interval_sec: number
  codex_ws_hide_upstream_errors: boolean
  codex_ws_silent_retry_enabled: boolean
  codex_ws_silent_max_retries: number
  codex_ws_size_router_enabled: boolean
  codex_ws_busy_acquire_max_wait_sec: number
  codex_ws_busy_overflow_enabled: boolean
  codex_ws_busy_patience_sec: number
  codex_ws_stateless_slots: number
  codex_ws_downstream_keepalive_slots: number
  // GitHub access (issue #522): the token is write-only, and the response only returns configured
  github_token?: string
  github_token_configured?: boolean
  github_proxy_url: string
  // Codex overload circuit breaker: when the share of server_is_overloaded within the window reaches the threshold, scheduling is paused automatically
  codex_overload_pause_enabled: boolean
  codex_overload_threshold_percent: number
  codex_overload_pause_minutes: number
  codex_overload_window_minutes: number
  codex_continue_thinking_enabled: boolean
  overflow_auto_compact_enabled: boolean
  compact_via_responses_enabled: boolean
  codex_preflight_sse_passthrough_enabled: boolean
  codex_continue_max_rounds: number
  utls_shutdown_timeout_minutes: number
  scheduler_mode: string
  affinity_mode?: string
  session_affinity_spread?: boolean
  session_slot_buffer_enabled: boolean
  session_slot_buffer_seconds: number
  max_retries: number
  max_rate_limit_retries: number
  retry_interval_ms: number
  transport_retry_policy: string
  continuous_retry_enabled: boolean
  continuous_retry_catch_all: boolean
  continuous_retry_categories: string[]
  continuous_retry_status_codes: number[]
  continuous_retry_error_codes: string[]
  continuous_retry_max_duration_seconds: number
  /** Device fingerprint convergence level applied by default to newly imported or created Codex accounts (off/device/session/full). */
  codex_fingerprint_default_mode: string
  allow_remote_migration: boolean
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  response_cache_local_max_bytes: number
  response_cache_local_max_entry_bytes: number
  response_cache_reconstruct_max_bytes: number
  response_cache_write_policy: ResponseCacheWritePolicy
  readonly response_cache_config_generation: number
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
  readonly output_guard_config_generation: number
  relay_model_cooldown_mode: ModelCooldownMode
  relay_model_cooldown_seconds: number
  relay_model_cooldown_backoff_enabled: boolean
  oauth_model_cooldown_mode: ModelCooldownMode
  oauth_model_cooldown_seconds: number
  oauth_model_cooldown_backoff_enabled: boolean
  oauth_unsupported_model_cooldown_seconds: number
  expired_cleaned?: number
  model_mapping: string
  codex_model_mapping: string
  payload_rules: string
  reasoning_effort_models: string
  resin_url: string
  resin_platform_name: string
  /** Authoritative Codex egress summary from the backend (read-only): when Resin is enabled, proxy pool, group, account and global proxies do not take part in outbound traffic for the Codex channel. */
  codex_egress?: CodexEgressSummary
  prompt_filter_enabled: boolean
  prompt_filter_mode: 'monitor' | 'warn' | 'block' | string
  prompt_filter_threshold: number
  prompt_filter_strict_threshold: number
  prompt_filter_strict_terminal_enabled: boolean
  prompt_filter_advanced_config: string
  prompt_filter_log_matches: boolean
  prompt_filter_max_text_length: number
  prompt_filter_sensitive_words: string
  prompt_filter_custom_patterns: string
  prompt_filter_custom_patterns_expected?: string
  prompt_filter_pattern_quarantines?: PromptFilterPatternQuarantine[]
  prompt_filter_disabled_patterns: string
  prompt_filter_review_enabled: boolean
  prompt_filter_review_api_key?: string
  prompt_filter_review_api_key_configured?: boolean
  prompt_filter_review_api_key_count?: number
  prompt_filter_review_base_url: string
  prompt_filter_review_model: string
  prompt_filter_review_timeout_seconds: number
  prompt_filter_review_fail_closed: boolean
  client_compat_mode: 'preserve' | 'auto' | 'force' | string
  codex_min_cli_version: string
  codex_images_main_model: string
  codex_images_default_main_model?: string
  codex_cli_version_sync_enabled: boolean
  codex_cli_version_sync_interval_hours: number
  codex_synced_cli_version?: string
  codex_synced_desktop_mac_build?: string
  codex_synced_desktop_windows_build?: string
  codex_synced_vscode_build?: string
  /** Verified app and built-in CLI pairings; read-only, not saved with settings. */
  codex_client_versions?: CodexClientVersionTarget[]
  codex_effective_cli_version?: string
  codex_user_agent_config: string
  usage_log_mode: 'full' | 'errors' | 'off' | string
  usage_log_batch_size: number
  usage_log_flush_interval_seconds: number
  stream_flush_policy: 'immediate' | 'coalesce' | string
  stream_flush_interval_ms: number
  first_token_mode: 'strict' | 'loose' | string
  first_token_timeout_seconds: number
  first_token_excludes_ws_acquire: boolean
  billing_tier_policy: 'actual' | 'requested' | string
  models_list_read_max_bytes: number
  show_full_usage_numbers: boolean
  show_upstream_model_mismatch: boolean
  public_key_usage_page_enabled: boolean
  public_image_studio_page_enabled: boolean
  public_account_portal_page_enabled: boolean
  image_storage_backend: 'local' | 's3' | string
  image_s3_endpoint: string
  image_s3_region: string
  image_s3_bucket: string
  image_s3_access_key: string
  image_s3_secret_key: string
  image_s3_secret_key_configured?: boolean
  image_s3_prefix: string
  image_s3_force_path_style: boolean
  auto_pause_5h_threshold: number
  auto_pause_7d_threshold: number
  auto_pause_5h_guard_band_percent: number
  auto_pause_5h_guard_concurrency: number
  smart_pacing_enabled: boolean
  smart_pacing_min_concurrency: number
  smart_pacing_windows: string
  ignore_usage_limit_status: boolean
}

export interface SetupHintsResponse {
  service_url?: string
  admin_url?: string
  api_base_url?: string
  database?: {
    driver?: string
    label?: string
    location?: string
  }
  cache?: {
    driver?: string
    label?: string
  }
  data?: {
    image_local_dir?: string
    image_storage_backend?: string
  }
  usage?: {
    log_mode?: string
    batch_size?: number
    flush_interval_seconds?: number
  }
}

export interface PromptFilterMatch {
  name: string
  weight: number
  category?: string
  strict?: boolean
  signal_only?: boolean
}

export interface PromptFilterVerdict {
  enabled: boolean
  mode: string
  action: 'allow' | 'warn' | 'block' | string
  score: number
  raw_score: number
  threshold: number
  strict_hit: boolean
  matched: PromptFilterMatch[]
  text_preview?: string
  reason?: string
  extracted_chars: number
  reviewed?: boolean
  review_flagged?: boolean
  review_error?: string
  review_model?: string
}

export interface PromptFilterLog {
  id: number
  created_at: ISODateString
  source: string
  endpoint: string
  protocol?: string
  provider?: string
  model: string
  action: string
  mode: string
  score: number
  audit_score?: number
  threshold: number
  policy_profile?: string
  reason_code?: string
  primary_origin?: string
  strike_eligible?: boolean
  matched_patterns: string
  match_context?: string
  text_preview: string
  full_text: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  client_ip: string
  error_code: string
  review_model: string
  review_flagged: boolean
	review_error: string
	reviewed: boolean
	review_confidence: number | null
	review_threshold: number | null
	review_reason: string
	review_endpoint: string
	review_request_mode: string
	review_latency_ms: number | null
	request_correlation_id?: string
	newapi_policy_status?: string
	newapi_platform?: string
  newapi_user_id?: string
  newapi_request_id?: string
  newapi_decision_id?: string
  session_hash?: string
  client_ip_hash?: string
}

export interface PromptFilterLogsResponse {
  logs: PromptFilterLog[]
  total: number
  page: number
  page_size: number
}

export type PromptPolicyEvaluationState = 'completed' | 'not_run' | 'unavailable' | 'legacy_unknown'
export type PromptPolicyLocalOutcome = 'no_hit' | 'audit_hit' | 'warn' | 'block'
export type PromptPolicyLocalComparison = 'confirmed_miss' | 'upstream_only' | 'evidence_unavailable' | 'local_detected' | 'not_comparable' | 'legacy_unknown'

export interface PromptPolicyIncident {
	id: number
	incident_id: string
	request_correlation_id?: string
	created_at: ISODateString
	attempt_index: number
	transport: string
	endpoint: string
	protocol: string
	provider: string
	model: string
	status_code: number
	account_id: number
	account_name: string
	account_platform: string
	account_group_ids: number[]
	account_group_names: string[]
	api_key_id: number
	api_key_name: string
	api_key_masked: string
	api_key_allowed_group_ids: number[]
	api_key_allowed_group_names: string[]
	routing_snapshot_state: 'event_snapshot' | 'current_inferred' | 'unavailable'
	platform: string
	newapi_policy_status?: string
	newapi_platform?: string
	newapi_user_id?: string
	newapi_request_id?: string
	session_hash?: string
	client_ip_hash?: string
	source_ref?: string
	upstream_error_code: string
	upstream_error: string
	local_evaluation_state: PromptPolicyEvaluationState
	local_outcome: PromptPolicyLocalOutcome
	local_action: string
	local_score: number | null
	local_raw_score: number | null
	local_audit_score: number | null
	local_audit_raw_score: number | null
	local_threshold: number
	local_mode: string
	local_policy_profile: string
	local_reason_code: string
	local_reason: string
	local_primary_origin: string
	local_strike_eligible: boolean
	local_review_model: string
	local_review_flagged: boolean
	local_review_error: string
	local_matched_patterns: string
	prompt_fingerprint: string
	prompt_preview: string
	prompt_text: string
	prompt_available: boolean
	local_comparison: PromptPolicyLocalComparison
	candidate_id?: number
	candidate_evidence_id?: number
	local_miss: boolean
}

export interface PromptPolicyIncidentsResponse {
	incidents: PromptPolicyIncident[]
	total: number
	page: number
	page_size: number
}

export interface PromptPolicyAuditHealth {
	ok: boolean
	status: 'healthy' | 'degraded' | string
	storage_ready: boolean
	prompt_filter_enabled: boolean
	review_enabled: boolean
	review_fail_closed: boolean
	review_pool: {
		configured: number
		available: number
		cooling_down: number
		probing: number
		next_retry_at?: ISODateString
	}
	conversation_lock_enabled: boolean
	incident_count: number
	latest_incident_id?: string
	latest_incident_at?: ISODateString
	queue: {
		enqueued: number
		completed: number
		dropped_high: number
		dropped_low: number
		failed: number
		pending_high: number
		pending_low: number
		retained_bytes: number
	}
}

export interface PromptPolicyIncidentDetailResponse {
	incident: PromptPolicyIncident
	matches: PromptFilterMatch[]
	risk_subjects?: PromptRiskIncidentSubject[]
	candidate?: {
		id: number
		status: string
		kind: string
		name: string
		category: string
		evidence_count: number
		sample_preview?: string
	}
	evidence?: {
		id: number
		source_kind: string
		source_ref?: string
		prompt_policy_incident_id?: string
		observed_at: ISODateString
	}
}

export type PromptRiskSubjectType = 'newapi_user' | 'session' | 'api_key' | 'client_ip' | 'upstream_account'
export type PromptRiskLevel = 'low' | 'observed' | 'elevated' | 'high' | 'critical'

export interface PromptRiskScoreBreakdown {
  local_signal: number
  upstream_signal: number
  recurrence: number
  identity_confidence: number
}

export type PromptRiskTrustStatus = 'active' | 'suspended' | 'revoked' | 'expired'

export interface PromptRiskTrustPolicy {
  id: number
  subject_type: PromptRiskSubjectType
  subject_key: string
  status: PromptRiskTrustStatus | string
  source: 'manual' | 'automatic' | string
  reason?: string
  risk_threshold: number
  valid_until: ISODateString
  last_evaluated_at?: ISODateString
  last_risk_score: number
  last_risk_level?: PromptRiskLevel | string
  bypass_count: number
  last_bypass_at?: ISODateString
  model_review_count: number
  last_model_review_at?: ISODateString
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptRiskTrustEvent {
  id: number
  policy_id: number
  subject_type: PromptRiskSubjectType
  subject_key: string
  event_type: string
  reason?: string
  risk_score: number
  risk_level?: PromptRiskLevel | string
  request_id_hash?: string
  created_at: ISODateString
}

export interface PromptRiskAdaptiveReviewBasis {
  enabled: boolean
  review_enabled: boolean
  eligible: boolean
  decision: 'disabled' | 'not_person' | 'adaptive_active' | 'suspended' | 'eligible' | 'building_history' | 'unavailable' | string
  clean_review_count: number
  positive_evidence_count: number
  min_clean_reviews: number
  min_observation_hours: number
  observation_hours: number
  sample_percent: number
  force_review_interval_minutes: number
  trust_duration_hours: number
  risk_threshold: number
  first_clean_at?: ISODateString
  last_clean_at?: ISODateString
  next_forced_review_at?: ISODateString
  force_review_due: boolean
}

export interface PromptRiskProfile {
  subject_type: PromptRiskSubjectType
  subject_key: string
  subject_display: string
  platform?: string
  newapi_user_id?: string
  newapi_user_name?: string
  newapi_user_email?: string
  newapi_user_group?: string
  is_person: boolean
  identity_confidence: number
  risk_score: number
  risk_level: PromptRiskLevel
  recommended_actions: string[]
  score_breakdown: PromptRiskScoreBreakdown
  has_activity: boolean
  identity_source?: string
  identity_updated_at?: ISODateString
  latest_at: ISODateString
  event_count: number
  events_10m: number
  events_24h: number
  events_7d: number
  events_30d: number
  upstream_cy_count: number
  confirmed_miss_count: number
  local_block_count: number
  local_warn_count: number
  distinct_fingerprints: number
  repeated_fingerprints: number
  api_key_id?: number
  api_key_name?: string
  api_key_masked?: string
  account_id?: number
  account_name?: string
  trust_policy?: PromptRiskTrustPolicy
  conversation_lock?: PromptConversationLock
}

export interface PromptConversationLock {
  id: number
  lock_key: string
  status: 'active' | 'unlocked'
  identity_kind: 'newapi' | 'codex_session' | 'fingerprint_replay' | string
  platform: string
  newapi_user_id: string
  session_fingerprint: string
  session_hash: string
  incident_id?: string
  decision_id: string
  request_id?: string
  reason_code: string
  endpoint?: string
  model?: string
  trigger_count: number
  unlock_count: number
  locked_at: ISODateString
  unlocked_at?: ISODateString
  unlock_reason?: string
	restriction_scope?: 'conversation' | 'user_cooldown' | 'fingerprint_replay'
  expires_at?: ISODateString
  remaining_seconds?: number
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptRiskEvent {
  id: number
  created_at: ISODateString
  source_type: string
  source_id: string
  incident_id?: string
  prompt_filter_log_id?: number
  request_correlation_id?: string
  subject_type: PromptRiskSubjectType
  subject_key: string
  subject_display: string
  platform?: string
  newapi_user_id?: string
  newapi_user_name?: string
  newapi_user_email?: string
  newapi_user_group?: string
  is_person: boolean
  identity_confidence: number
  event_kind: string
  request_risk_score: number
  evidence_confidence: number
  reason_code?: string
  action?: string
  local_outcome?: string
  local_comparison?: string
  endpoint?: string
  model?: string
  prompt_fingerprint?: string
  prompt_preview?: string
  api_key_id?: number
  api_key_name?: string
  api_key_masked?: string
  account_id?: number
  account_name?: string
}

export interface PromptRiskProfilesResponse {
  profiles: PromptRiskProfile[]
  total: number
  page: number
  page_size: number
  scoring_version: string
  guardrail: string
}

export interface PromptRiskProfileDetailResponse {
  profile: PromptRiskProfile
  events: PromptRiskEvent[]
  trust_events: PromptRiskTrustEvent[]
  adaptive_review_basis: PromptRiskAdaptiveReviewBasis
  event_total: number
  event_page: number
  event_page_size: number
  trust_event_total: number
  trust_event_page: number
  trust_event_page_size: number
  scoring_version: string
  guardrail: string
}

export interface PromptFilterTestResponse {
  verdict: PromptFilterVerdict
  decision?: PromptGuardDecision
  protocol?: string
  provider?: string
  endpoint?: string
  model?: string
}

export interface PromptReviewTestRequest {
  text: string
  api_key?: string
  base_url: string
  model: string
  request_mode: 'moderations' | 'chat_completions' | string
  system_prompt: string
  user_prompt_template: string
  payload_template: string
  confidence_threshold: number
  moderation_thresholds: Record<string, number>
  timeout_seconds: number
  max_concurrent: number
  max_text_length: number
  test_all_keys?: boolean
}

export interface PromptReviewKeyTestResult {
  key_index: number
  key_id?: string
  key_masked?: string
  ok: boolean
  endpoint?: string
  model?: string
  flagged: boolean
  confidence: number
  reason?: string
  highest_category?: string
  decision_category?: string
  decision_score?: number
  decision_threshold?: number
  category_scores?: Record<string, number>
  moderation_thresholds?: Record<string, number>
  latency_ms: number
  error?: string
}

export interface PromptReviewAPIKeyDescriptor {
  id: string
  index: number
  masked: string
}

export interface PromptReviewAPIKeysResponse {
  items: PromptReviewAPIKeyDescriptor[]
  count: number
}

export interface PromptReviewProfile {
  id: string
  name: string
  base_url: string
  model: string
  request_mode: string
  timeout_seconds: number
  key_count: number
  active: boolean
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptReviewProfilesResponse {
  profiles: PromptReviewProfile[]
}

export interface PromptReviewTestResponse {
  ok: boolean
  endpoint: string
  model: string
  flagged: boolean
  confidence: number
  confidence_threshold: number
  reason?: string
  highest_category?: string
  decision_category?: string
  decision_score?: number
  decision_threshold?: number
  category_scores?: Record<string, number>
  moderation_thresholds?: Record<string, number>
  latency_ms: number
  key_count?: number
  results?: PromptReviewKeyTestResult[]
}

export interface PromptFilterRulePatternTestResponse {
  matched: boolean
  error?: string
}

export type PromptGuardMode = 'inherit' | 'off' | 'shadow' | 'warn' | 'enforce'

export type PromptGuardProfile = 'balanced' | 'strict' | 'research'

export type PromptGuardProvider = 'openai' | 'anthropic' | 'xai' | 'unknown'

export interface PromptGuardDecision {
  enabled: boolean
  mode: string
  profile: string
  application_prompt_kind?: string
  action: string
  would_action: string
  score: number
  raw_score: number
  audit_score?: number
  audit_raw_score?: number
  reason_code?: string
  reason?: string
  terminal?: boolean
  strike_eligible?: boolean
  truncated?: boolean
  current_user_truncated?: boolean
  auxiliary_truncated?: boolean
  primary_origin?: string
  primary_detector?: string
  signals?: PromptGuardSignal[]
  errors?: string[]
}

export interface PromptGuardSignal {
  detector: string
  family: string
  category?: string
  correlation_key?: string
  origin: string
  layer_mode: string
  score: number
  raw_score: number
  confidence: number
  suggested_action: string
  terminal_candidate?: boolean
  strike_eligible?: boolean
  reason?: string
  matches?: PromptFilterMatch[]
}

export interface PromptGuardPerformanceConfig {
  async_shadow_auxiliary_enabled: boolean
  exact_segment_cache_enabled: boolean
  exact_segment_cache_entries: number
  exact_segment_cache_ttl_seconds: number
  max_segments: number
  max_current_user_bytes: number
  max_auxiliary_bytes: number
  scan_chunk_bytes: number
  scan_overlap_bytes: number
  shadow_workers: number
  shadow_queue_size: number
  shadow_overflow_mode: 'drop'
}

export type PromptGuardLayer =
  | 'current_user'
  | 'history'
  | 'system'
  | 'developer'
  | 'instructions'
  | 'tool_output'
  | 'tool_arguments'
  | 'attachment_refs'
  | 'session_context'
  | 'attachment_content'

export interface PromptGuardConfig {
  mode: PromptGuardMode
  default_profile: PromptGuardProfile
  allow_trusted_overrides: boolean
  provider_profiles: Partial<Record<PromptGuardProvider, PromptGuardProfile>>
  layers: Record<PromptGuardLayer, { mode: PromptGuardMode }>
  performance: PromptGuardPerformanceConfig
}

export type AdvancedConfigObject = Record<string, unknown>

export interface AdvancedConfigDocument {
  ok: boolean
  value: AdvancedConfigObject | null
  error: 'invalid_json' | 'root_not_object' | null
}

export interface AdvancedConfigPatch {
  path: readonly string[]
  value?: unknown
  remove?: boolean
}

export interface AdvancedConfigPatchResult extends AdvancedConfigDocument {
  serialized: string
}

function isAdvancedConfigObject(value: unknown): value is AdvancedConfigObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * Parse the persisted advanced configuration without normalizing or rebuilding
 * it. Callers can derive a typed view separately while retaining this original
 * tree as the source of truth for compatible field-level updates.
 */
export function parseAdvancedConfigDocument(raw: string): AdvancedConfigDocument {
  try {
    const value = JSON.parse(raw || '{}') as unknown
    if (!isAdvancedConfigObject(value)) {
      return { ok: false, value: null, error: 'root_not_object' }
    }
    return { ok: true, value, error: null }
  } catch {
    return { ok: false, value: null, error: 'invalid_json' }
  }
}

export function readAdvancedConfigPath(
  value: AdvancedConfigObject | null,
  path: readonly string[],
): unknown {
  let current: unknown = value
  for (const key of path) {
    if (!isAdvancedConfigObject(current)) return undefined
    current = current[key]
  }
  return current
}

/**
 * Apply only explicitly edited JSON paths to a freshly parsed document. This
 * preserves unknown top-level and nested fields, including future enum values.
 * Invalid JSON is returned untouched so the UI can block saving instead of
 * silently replacing it with defaults.
 */
export function patchAdvancedConfigDocument(
  raw: string,
  patches: readonly AdvancedConfigPatch[],
): AdvancedConfigPatchResult {
  const parsed = parseAdvancedConfigDocument(raw)
  if (!parsed.ok || !parsed.value) {
    return { ...parsed, serialized: raw }
  }

  const root = parsed.value
  for (const patch of patches) {
    if (patch.path.length === 0) continue
    let current = root
    for (const key of patch.path.slice(0, -1)) {
      const child = current[key]
      if (isAdvancedConfigObject(child)) {
        current = child
      } else {
        const next: AdvancedConfigObject = {}
        current[key] = next
        current = next
      }
    }
    const leaf = patch.path[patch.path.length - 1]
    if (patch.remove) delete current[leaf]
    else current[leaf] = patch.value
  }

  return {
    ok: true,
    value: root,
    error: null,
    serialized: JSON.stringify(root),
  }
}

export interface PromptFilterRule {
  name: string
  pattern: string
  weight: number
  category?: string
  strict?: boolean
  enabled?: boolean
  builtin?: boolean
}

export interface PromptFilterRulesResponse {
  builtin_patterns: PromptFilterRule[]
  custom_patterns: PromptFilterRule[]
  disabled_patterns: string[]
}

export interface PromptIntelligenceRuleDraft {
  name: string
  pattern: string
  weight: number
  category: string
  strict: boolean
  rationale?: string
  source_url?: string
  change_type?: 'new' | 'update' | string
}

export interface PromptIntelligenceCandidate extends PromptIntelligenceRuleDraft {
  id: number
  fingerprint: string
  kind: 'pattern' | 'evidence' | string
  lifecycle_status: 'pending' | 'published' | 'dismissed' | 'superseded' | string
  source?: string
  evidence_count: number
  sample_preview?: string
  protocol?: string
  provider?: string
  model?: string
  api_key_id?: number
  api_key_name?: string
  ai_analyzed?: boolean
  ai_analysis_count?: number
  ai_analyzed_at?: string
  latest_ai_analysis?: PromptIntelligenceAIAnalysisResponse
  created_at?: string
  updated_at?: string
  last_seen_at?: string
}

export interface PromptIntelligenceRunCandidate extends PromptIntelligenceRuleDraft {
  id?: number
  fingerprint?: string
  kind?: 'pattern' | 'evidence' | string
  lifecycle_status?: string
  status?: 'new' | 'update' | string
  source?: string
  evidence_count?: number
  sample_preview?: string
}

export interface PromptIntelligenceCandidatesResponse {
  candidates: PromptIntelligenceCandidate[]
  total: number
}

export interface PromptIntelligenceEvidence {
  id: number
  source_kind: string
  source_ref?: string
  sample_preview?: string
  metadata: Record<string, unknown>
  protocol?: string
  provider?: string
  model?: string
  api_key_id?: number
  api_key_name?: string
  observed_at: string
  incident_id?: string
  risk_subjects?: PromptRiskIncidentSubject[]
}

export interface PromptIntelligenceEvidenceResponse {
  candidate: PromptIntelligenceCandidate
  evidence: PromptIntelligenceEvidence[]
}

export type PromptIntelligenceAIProvider = 'review' | 'account_pool'
export type PromptIdentityUpdateMode = 'suggest' | 'guarded_auto'

export interface PromptIntelligenceAIAnalysisRequest {
  provider: PromptIntelligenceAIProvider
  model?: string
  api_key_id?: number
  identity_update_mode: PromptIdentityUpdateMode
}

export interface PromptIntelligenceGatewayKey {
  id: number
  name: string
  masked: string
  status: 'active' | 'expired' | 'quota_exhausted' | string
}

export interface PromptIntelligenceAIProvidersResponse {
  review: { configured: boolean; model: string; key_count: number }
  gateway_keys: PromptIntelligenceGatewayKey[]
}

export interface PromptIntelligenceAIIdentityPatch {
  clauses: string[]
  rationale?: string
}

export interface PromptIntelligenceAIDecision {
  decision: 'no_change' | 'rule' | 'identity' | 'both'
  confidence: number
  reason: string
  rule?: PromptIntelligenceRuleDraft
  identity_patch?: PromptIntelligenceAIIdentityPatch
}

export interface PromptIdentityUpdateResult {
  mode: 'suggest' | 'guarded_auto' | 'manual' | 'rollback' | string
  suggested: boolean
  eligible: boolean
  applied: boolean
  rolled_back?: boolean
  analysis_evidence_id: number
  revision_evidence_id?: number
  clauses?: string[]
  block_reason?: string
}

export interface PromptIntelligenceAIAnalysisResponse {
  analysis_evidence_id: number
  provider: PromptIntelligenceAIProvider
  model: string
  evidence_basis?: 'prompt' | 'context_only'
  decision: PromptIntelligenceAIDecision
  rule_candidate?: PromptIntelligenceCandidate
  rule_error?: string
  identity_update: PromptIdentityUpdateResult
}

export interface PromptRiskIncidentSubject {
	subject_type: PromptRiskSubjectType
	subject_key: string
	subject_display: string
	platform?: string
	is_person: boolean
	identity_confidence: number
	newapi_user_id?: string
	newapi_user_name?: string
	newapi_user_email?: string
	newapi_user_group?: string
	event_count: number
}

export interface PromptIntelligenceDraftSuggestion {
  provider: PromptIntelligenceAIProvider
  model: string
  evidence_basis: 'prompt' | 'context_only'
  confidence: number
  reason: string
  rule: { name: string; pattern: string; weight: number; category: string; strict: boolean; rationale: string }
  validation_error?: string
  evidence_matched: number
  evidence_total: number
}

export interface ProxyRiskScoreSnapshot {
  id: number
  proxy_id: number
  profile_id: number
  provider: string
  resolved_ip: string
  score: number | null
  risk_level: string
  recommendation: string
  proxy_type?: string
  is_vpn: boolean
  is_tor: boolean
  is_datacenter: boolean
  is_blacklisted: boolean
  blacklist_sources?: string[]
  isp?: string
  country?: string
  latency_ms: number
  status: string
  error?: string
  features_json?: string
  raw_response_json?: string
  checked_at: ISODateString
  expires_at?: ISODateString | null
}

export interface ProxyRiskScoringProfile {
  id: number
  name: string
  provider: string
  engine?: string
  enabled: boolean
  priority: number
  scamalytics_host: string
  scamalytics_user: string
  scamalytics_key_configured?: boolean
  scamalytics_key_masked?: string
  timeout_seconds: number
  concurrency: number
  request_delay_ms: number
  cache_ttl_seconds: number
  max_checks_per_job: number
  daily_check_limit: number
  credit_reserve: number
  allow_force_refresh: boolean
  resolve_hostnames: boolean
  allow_private_targets: boolean
  docs_url: string
  tutorial_url: string
  daily_used_date?: string
  daily_used_count?: number
  credits_remaining?: number | null
  credits_used?: number | null
  credit_reset_at?: ISODateString | null
  last_quota_checked_at?: ISODateString | null
  last_error?: string
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptLogRetention {
  retention_days: number
  running: boolean
  last_run_at?: string
  last_deleted_logs: number
  last_deleted_events: number
  last_deleted_sources: number
  last_duration_ms: number
  last_error?: string
}

export interface ProxyRiskScoringJobItem {
  seq: number
  proxy_id: number
  label: string
  status: 'success' | 'error' | 'skipped' | 'cached' | string
  error?: string
  snapshot?: ProxyRiskScoreSnapshot | null
  checked_at: ISODateString
}

export interface ProxyRiskScoringJob {
  job_id: string
  current?: string
  items: ProxyRiskScoringJobItem[]
  last_seq: number
  profile_id: number
  status: string
  total: number
  done: number
  success: number
  failed: number
  skipped: number
  cache_hits: number
  error?: string
  created_at: ISODateString
  updated_at: ISODateString
}

export interface PromptIntelligenceHistoryResponse {
  runs: PromptIntelligenceRun[]
  total: number
}

export interface PromptIntelligenceRun {
  started_at: string
  finished_at: string
  queries: string[]
  sources: Array<{ provider: string; title: string; url: string; description: string; updated_at: string }>
  candidates: PromptIntelligenceRunCandidate[]
  model_calls: number
  staged?: number
  added?: number
  errors: string[]
}

export interface ModelInfo {
  id: string
  enabled: boolean
  category: string
  source: string
  pro_only: boolean
  api_key_auth_available: boolean
  last_seen_at?: string
  updated_at?: string
}

export interface ModelsResponse {
  models: string[]
  items?: ModelInfo[]
  last_synced_at?: string
  source_url: string
  warning?: string
}

export interface ChannelModelRefreshResult {
  channel: 'codex' | string
  groups?: number
  refreshed: number
  failed: number
  added: string[]
  error?: string
}

export interface RefreshAllModelsResponse {
  type: 'complete'
  message: string
  channels: ChannelModelRefreshResult[]
  added: string[]
  model_count: number
  duration_ms: number
}

export interface ModelSyncResponse {
  added: number
  updated: number
  unchanged: number
  skipped: string[]
  removed?: string[]
  models: string[]
  items: ModelInfo[]
  last_synced_at: string
  source_url: string
}

export interface CPAExportEntry {
  type: string
  email: string
  expired: string
  id_token: string
  account_id: string
  access_token: string
  last_refresh: string
  refresh_token: string
  /**
   * The three proxy fields only appear when "Include proxy configuration" is ticked on export. proxy_enabled uses an optional boolean
   * to distinguish "the file does not contain this field" (old files, treated as enabled) from "explicitly disabled at the source".
   */
  proxy_url?: string
  proxy_label?: string
  proxy_enabled?: boolean
}

export interface UsageStats {
  total_requests: number
  total_tokens: number
  total_prompt_tokens: number
  total_completion_tokens: number
  total_input_tokens?: number
  total_cached_tokens: number
  total_cache_rate?: number
  total_account_billed: number
  total_user_billed: number
  avg_account_billed_per_request: number
  avg_user_billed_per_request: number
  today_requests: number
  today_tokens: number
  today_input_tokens?: number
  today_prompt_tokens?: number
  today_completion_tokens?: number
  today_cached_tokens?: number
  today_cache_rate?: number
  today_account_billed: number
  today_user_billed: number
  rpm: number
  tpm: number
  avg_duration_ms: number
  avg_first_token_ms?: number
  error_rate: number
  feature_stats: UsageFeatureStats
  model_stats: UsageModelStat[]
  endpoint_stats: UsageEndpointStat[]
  api_key_stats: UsageAPIKeyStat[]
}

export interface UsageModelStat {
  model: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  account_billed: number
  user_billed: number
  error_count: number
}

export interface UsageFeatureStats {
  stream_requests: number
  sync_requests: number
  fast_requests: number
  cache_hit_requests: number
  reasoning_requests: number
  image_requests: number
  retry_requests: number
  error_requests: number
}

export interface UsageEndpointStat {
  endpoint: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

export interface UsageAPIKeyStat {
  api_key_id: number
  label: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

// APIKeyTokenStat is an item returned by the /usage/api-keys endpoint, with more detailed fields than UsageAPIKeyStat
// (input/output/cached tokens in separate columns), and the number of items is not limited.
export interface APIKeyTokenStat {
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  label: string
  requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  total_tokens: number
  error_count: number
  user_billed: number
}

// APIKeyAccountGroup is a condensed display item for upstream account groups (used for token usage details).
export interface APIKeyAccountGroup {
  id: number
  name: string
  color: string
}

// APIKeyAccountStat is an item returned by the /usage/api-keys/:id/accounts endpoint:
// Usage of a downstream Key within a time range, split by upstream account (the transpose of the account "breakdown by Key" view).
export interface APIKeyAccountStat {
  account_id: number
  account_name: string
  account_email: string
  account_deleted?: boolean
  groups?: APIKeyAccountGroup[]
  requests: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  total_tokens: number
  error_count: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountGroupUsage {
  id: number
  name: string
  color: string
  accounts: number
  requests: number
  total_tokens: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountUsageSummary {
  accounts: number
  requests: number
  total_tokens: number
  account_billed: number
  user_billed: number
}

export interface APIKeyAccountUsageReconciliation {
  grouped_total: APIKeyAccountUsageSummary
  ungrouped: APIKeyAccountUsageSummary
  duplicate: APIKeyAccountUsageSummary
  unique_grouped_accounts: number
  multi_group_accounts: number
}

export interface APIKeyAccountStatsResponse {
  items: APIKeyAccountStat[]
  groups: APIKeyAccountGroupUsage[]
  summary: APIKeyAccountUsageSummary
  reconciliation?: APIKeyAccountUsageReconciliation
  /** Active accounts use current memberships; deleted accounts use their last retained membership. */
  membership_basis: 'current_and_deleted_last_membership'
}

export type UserBillingMode = 'token' | 'per_image' | 'per_video' | 'per_second'

export interface UsageLog {
  /** Generated video duration (seconds) on Grok video settlement rows. */
  video_seconds?: number
  user_billing_mode?: '' | UserBillingMode
  /** Unit price for unit billing modes (per image / video / second). */
  image_unit_price?: number
  /** Billed units: images, videos or seconds depending on user_billing_mode. */
  billed_image_count?: number
  request_id?: string
  upstream_request_id?: string
  upstream_proxy_id?: number
  upstream_proxy_name?: string
  /** X-Codex-Turn-State value the gateway injected on this attempt ("" = none). */
  injected_turn_state?: string
  /** X-Codex-Turn-State value observed from the upstream response ("" = none). */
  upstream_turn_state?: string
  id: number
  account_id: number
  // Upstream channel (codex), fixed at write time; historical rows are backfilled and may be empty
  channel?: string
  client_ip: string
  client_user_agent: string
  upstream_user_agent: string
  user_agent_overridden: boolean
  turn_state_overridden?: boolean
  turn_state_rewrite_note?: string
  internal_reason: string
  parent_request_id: string
  endpoint: string
  model: string
  effective_model: string
  daybreak_program?: string
  /** Model name the upstream response reports for itself (empty when not reported or for historical rows). */
  upstream_response_model?: string
  /** Three states: undefined/null = upstream did not report, so it cannot be compared; true/false = whether the reported and actually sent models match. */
  upstream_model_mismatch?: boolean | null
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  status_code: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  first_token_ms: number
  ws_acquire_ms?: number
  reasoning_effort: string
  inbound_endpoint: string
  upstream_endpoint: string
  stream: boolean
  compact: boolean
  has_compaction_history: boolean
  ultra?: boolean
  via_websocket?: boolean
  cached_tokens: number
  image_input_tokens?: number
  image_output_tokens?: number
  cached_image_input_tokens?: number
  image_input_cost?: number
  image_cache_read_cost?: number
  image_input_price_per_mtoken?: number
  cached_image_input_price_per_mtoken?: number
  cache_write_5m_tokens: number
  cache_write_1h_tokens: number
  service_tier: string
  requested_service_tier: string
  actual_service_tier: string
  billing_service_tier: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  image_count: number
  image_width: number
  image_height: number
  image_bytes: number
  image_format: string
  image_size: string
  account_name: string
  account_email: string
  created_at: ISODateString
  account_billed: number
  user_billed: number
  input_cost: number
  output_cost: number
  cache_read_cost: number
  cache_write_5m_cost: number
  cache_write_1h_cost: number
  total_cost: number
  input_price_per_mtoken: number
  output_price_per_mtoken: number
  cache_read_price_per_mtoken: number
  cache_write_5m_price_per_mtoken: number
  cache_write_1h_price_per_mtoken: number
  rate_multiplier: number
  long_context?: boolean
  long_context_threshold?: number
  is_retry_attempt: boolean
  attempt_index: number
  upstream_error_kind: string
	error_message: string
	prompt_policy_incident_id?: string
}

export type UsageLogsResponse = ApiListResponse<'logs', UsageLog>

export interface UsageLogsPagedResponse {
  logs: UsageLog[]
  total: number
}

export interface OpsErrorSummary {
  total_errors: number
  status_4xx: number
  status_5xx: number
  unauthorized: number
  rate_limited: number
  canceled: number
  timeouts: number
  retry_attempts: number
  avg_duration_ms: number
}

export interface ChartTimelinePoint {
  bucket: string
  requests: number
  avg_latency: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  errors_4xx: number
  errors_5xx: number
}

export interface ChartModelPoint {
  model: string
  requests: number
}

export interface ChartAggregation {
  timeline: ChartTimelinePoint[]
  models: ChartModelPoint[]
}

export interface ModelPricingOverride {
  user_billing_mode?: UserBillingMode
  image_unit_price?: number
  /** Upstream cost per media unit (USD / image or USD / second) for Grok Imagine models. */
  media_unit_cost?: number
  image_input?: number
  cached_image_input?: number
  source?: string
  input?: number
  cached_input?: number
  cache_write_5m?: number
  cache_write_1h?: number
  output?: number
  input_priority?: number
  cached_input_priority?: number
  output_priority?: number
  input_long?: number
  cached_input_long?: number
  output_long?: number
  input_long_priority?: number
  cached_input_long_priority?: number
  output_long_priority?: number
  long_context_threshold_tokens?: number
}

export interface OfficialPricingSyncConfig {
	enabled: boolean
	interval_minutes: number
	include_openai: boolean
	last_attempt_at?: string
	last_success_at?: string
	last_error?: string
	last_warning?: string
}

export interface OfficialPricingSyncResult {
	fetched: number
	applied: number
	skipped: number
	missing?: string[]
	sources: string[]
	warnings?: string[]
	synced_at: string
	fallbacks?: Record<string, string>
	notices?: string[]
}

/**
 * Usage limit for a single "this Key × an account group / an account" pair (issue #439).
 * 0 or omitted means this dimension is unlimited; when exceeded, by default the accounts in this scope are removed from scheduling candidates.
 */
export interface APIKeyScopeLimit {
  scope_type: 'group' | 'account'
  scope_id: number
  /** skip: remove this scope and keep using other accounts (default); reject: return 429 directly. */
  on_exhausted?: 'skip' | 'reject'
  cost_5h?: number
  cost_1d?: number
  cost_7d?: number
  cost_30d?: number
  token_5h?: number
  token_1d?: number
  token_7d?: number
  token_30d?: number
  requests_1d?: number
  /** Maximum number of in-flight requests for this Key on this scope (soft limit within the process). */
  max_concurrency?: number
  /** Cumulative quota: does not decline over time, and must be reset manually once used up. */
  quota_cost?: number
  quota_tokens?: number
  quota_requests?: number
}

/** Current usage of a scope limit within a window (GET /api/admin/keys/:id/scope-usage). */
export interface APIKeyScopeUsageWindow {
  window: string
  requests: number
  tokens: number
  user_billed: number
  cost_limit?: number
  token_limit?: number
  request_limit?: number
  exhausted: boolean
}

export interface APIKeyScopeCumulativeUsage {
  used_cost: number
  used_tokens: number
  used_requests: number
  quota_cost?: number
  quota_tokens?: number
  quota_requests?: number
  reset_count: number
  last_reset_at?: string
  exhausted: boolean
}

/** Runtime statistics for a scope budget that was judged exhausted (in the gateway process, reset to zero on restart). */
export interface APIKeyScopeSkipStat {
  requests: number
  first_at: string
  last_at: string
  last_message: string
}

export interface APIKeyScopeUsageItem {
  scope_type: 'group' | 'account'
  scope_id: number
  scope_name: string
  scope_exists: boolean
  on_exhausted: 'skip' | 'reject'
  windows: APIKeyScopeUsageWindow[]
  cumulative?: APIKeyScopeCumulativeUsage
  skips?: APIKeyScopeSkipStat
}

/** Scope budget overview for the list page (shows only the tightest window). */
export interface APIKeyScopeSummaryItem {
  scope_type: 'group' | 'account'
  scope_id: number
  scope_name: string
  on_exhausted: 'skip' | 'reject'
  window: string
  metric: string
  ratio: number
  exhausted: boolean
  skip_requests?: number
}

export interface APIKeyModelRequestLimit {
  /** Stable backend-generated identity; omit when adding a rule. */
  id?: string
  model: string
  window: 'week'
  max_requests: number
  timezone: string
  /** ISO weekday: Monday = 1, Sunday = 7. */
  reset_weekday: number
  reset_time: string
}

export interface APIKeyModelRequestUsage {
  rule_id: string
  model: string
  window: 'week'
  limit: number
  used: number
  remaining: number
  window_start: ISODateString
  reset_at: ISODateString
  timezone: string
}

export interface APIKeyLimits {
  model_allow?: string[]
  model_deny?: string[]
  plan_allow?: string[]
  no_affinity_group_ids?: number[]
  rpm?: number
  rpd?: number
  max_concurrency?: number
  cost_limit_5h?: number
  cost_limit_7d?: number
  cost_limit_30d?: number
  /** Calendar-day amount limit (server local timezone), reset at midnight; semantics differ from a sliding window (issue #460). */
  cost_limit_daily?: number
  token_limit_5h?: number
  token_limit_7d?: number
  token_limit_30d?: number
  token_limit_daily?: number
  disable_image_generation?: boolean
  /** Image tool policy: ""/"allow" lets it through, "strip" removes images and continues with the text request, "block" returns 403 on a match. */
  image_generation_policy?: "allow" | "strip" | "block"
  upstream_channel?: UpstreamChannel
  /** Allows this Key to use ChatGPT Live (/v1/live). Newly created auto/Codex channel Keys have it enabled by default. */
  allow_live?: boolean
  /** External scheduling mode (issue #831): requests pick the account with X-Codex2API-Account-ID, and the gateway does not pick, switch or retry accounts. */
  external_scheduler?: boolean
  /** Usage budget at the group / account level (issue #439). */
  scope_limits?: APIKeyScopeLimit[]
  /** Fixed weekly request budgets shared by models matching each rule. */
  model_request_limits?: APIKeyModelRequestLimit[]
}

export interface APIKeyWindowUsage {
  requests?: number
  tokens?: number
  user_billed?: number
  cost_5h: number
  cost_7d: number
  cost_30d: number
  cost_today?: number
}

export interface APIKeyRow {
  id: number
  name: string
  key: string
  raw_key: string
  quota_limit: number
  quota_used: number
  total_used: number
  reset_count: number
  last_reset_at?: ISODateString | null
  expires_at?: ISODateString | null
  status?: 'active' | 'expired' | 'quota_exhausted' | 'disabled'
  enabled?: boolean
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
  window_usage?: APIKeyWindowUsage
  last_used_at?: ISODateString | null
  created_at: ISODateString
}

export type APIKeysResponse = ApiListResponse<'keys', APIKeyRow>

export type PromptFilterScope = 'inherit' | 'local_only' | 'off'

export interface PromptFilterNewAPIBinding {
  api_key_id: number
  platform_code: string
  platform_name: string
  enabled: boolean
  require_signed_identity: boolean
  prompt_filter_scope: PromptFilterScope
  secret_configured: boolean
  secret_masked: string
  previous_secret_active: boolean
  previous_secret_expires_at?: ISODateString | null
  updated_at: ISODateString
  /** Only appears in responses to a successful create or rotate; the list and detail endpoints never return the plain text. */
  secret?: string
}

export interface PromptFilterNewAPIBindingsResponse {
  bindings: PromptFilterNewAPIBinding[]
}

export interface CreatePromptFilterNewAPIBindingRequest {
  api_key_id: number
  platform_code: string
  platform_name: string
  enabled?: boolean
  require_signed_identity?: boolean
  prompt_filter_scope?: PromptFilterScope
}

export type UpdatePromptFilterNewAPIBindingRequest = Partial<
  Omit<CreatePromptFilterNewAPIBindingRequest, 'api_key_id'>
>

export interface CreateAPIKeyRequest {
  name: string
  key?: string
  quota_limit?: number
  quota?: number
  expires_at?: string
  expires_in_days?: number
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
}

export interface UpdateAPIKeyRequest {
  name?: string
  quota_limit?: number | null
  quota?: number | null
  reset_quota?: boolean
  expires_at?: string | null
  expires_in_days?: number
  allowed_group_ids?: number[]
  limits?: APIKeyLimits
  enabled?: boolean
}

export interface ImageStudioQuota {
  image_pricing?: Record<string, { user_billing_mode: 'token' | 'per_image'; image_unit_price?: number }>
  quota_limit: number
  quota_used: number
  quota_remaining: number | null
  expires_at: ISODateString | null
  status: 'active' | 'expired' | 'quota_exhausted'
  refresh_after_seconds: number
}

export interface PublicAPIKeyUsageKey {
  name: string
  key: string
  quota_limit: number
  quota_used: number
  total_used: number
  reset_count: number
  last_reset_at?: ISODateString | null
  expires_at?: ISODateString | null
  limits: APIKeyLimits
  status: 'active' | 'expired' | 'quota_exhausted'
  created_at: ISODateString
}

export interface PublicAPIKeyUsageRange {
  name: 'today' | '7d' | '30d' | 'all' | string
  start?: ISODateString | null
  end: ISODateString
}

export interface PublicAPIKeyWindowUsage {
  requests: number
  tokens: number
  user_billed: number
  /** Time of the earliest usage within the window (omitted when there is no usage). */
  oldest_at?: ISODateString
  /** fixed = fixed calendar-day window (reset to zero at reset_at); sliding = sliding window (starts declining at decay_at). */
  window_kind: 'fixed' | 'sliding'
  reset_at?: ISODateString
  decay_at?: ISODateString
}

export interface PublicAPIKeyUsageWindows {
  today: PublicAPIKeyWindowUsage
  last_5h: PublicAPIKeyWindowUsage
  last_7d: PublicAPIKeyWindowUsage
  last_30d: PublicAPIKeyWindowUsage
}

export interface PublicAPIKeyUsageSummary {
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  error_count: number
  user_billed: number
  avg_duration_ms: number
  avg_first_token_ms: number
  rpm: number
  tpm: number
}

export interface PublicAPIKeyUsageBreakdown {
  name: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  error_count: number
  user_billed: number
}

export interface PublicAPIKeyUsageLog {
  user_billing_mode?: '' | UserBillingMode
  /** Unit price for unit billing modes (per image / video / second). */
  image_unit_price?: number
  /** Billed units: images, videos or seconds depending on user_billing_mode. */
  billed_image_count?: number
  id: number
  channel?: UpstreamChannel | ''
  client_ip: string
  client_user_agent: string
  endpoint: string
  model: string
  effective_model: string
  daybreak_program?: string
  status_code: number
  duration_ms: number
  first_token_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  reasoning_effort: string
  cached_tokens: number
  cache_write_5m_tokens?: number
  cache_write_1h_tokens?: number
  total_tokens: number
  user_billed: number
  input_cost: number
  output_cost: number
  cache_read_cost: number
  total_cost: number
  input_price_per_mtoken: number
  output_price_per_mtoken: number
  cache_read_price_per_mtoken: number
  rate_multiplier: number
  long_context: boolean
  service_tier: string
  stream: boolean
  compact: boolean
  has_compaction_history: boolean
  ultra?: boolean
  via_websocket: boolean
  upstream_error_kind: string
  created_at: ISODateString
}

/** Request-log filters for the public key usage page; they only narrow recent_logs. */
export interface PublicAPIKeyUsageLogFilter {
  model?: string
  endpoint?: string
  status?: '' | 'success' | 'error' | '4xx' | '5xx' | '429'
  stream?: '' | 'stream' | 'sync'
  channel?: '' | UpstreamChannel
}

export interface PublicAPIKeyUsageReport {
  summary: PublicAPIKeyUsageSummary
  windows: PublicAPIKeyUsageWindows
  models: PublicAPIKeyUsageBreakdown[]
  endpoints: PublicAPIKeyUsageBreakdown[]
  /** Requested models / inbound endpoints seen in the range (unaffected by log filters). */
  log_models?: string[]
  log_endpoints?: string[]
  recent_logs: PublicAPIKeyUsageLog[]
  recent_logs_total: number
  recent_logs_page: number
  recent_logs_page_size: number
}

export interface PublicAPIKeyUsageResponse {
  key: PublicAPIKeyUsageKey
  range: PublicAPIKeyUsageRange
  usage: PublicAPIKeyUsageReport
  model_request_usage?: APIKeyModelRequestUsage[]
}

export interface CreateAPIKeyResponse {
  id: number
  key: string
  name: string
  quota_limit: number
  quota_used: number
  expires_at?: ISODateString | null
  allowed_group_ids?: number[]
}

export interface ImagePromptTemplate {
  id: number
  name: string
  prompt: string
  model: string
  size: string
  quality: string
  output_format: string
  background: string
  style: string
  tags: string[]
  favorite: boolean
  usage_count: number
  last_used_at?: ISODateString
  created_at: ISODateString
  updated_at: ISODateString
}

export interface ImageAsset {
  id: number
  job_id: number
  template_id: number
  filename: string
  proxy_url?: string
  thumbnail_url?: string
  mime_type: string
  bytes: number
  width: number
  height: number
  model: string
  requested_size: string
  actual_size: string
  quality: string
  output_format: string
  revised_prompt: string
  created_at: ISODateString
  cache_b64_json?: string
}

export interface ImageGenerationJob {
  id: number
  status: 'queued' | 'running' | 'succeeded' | 'failed' | string
  prompt: string
  params_json: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  error_message: string
  warning?: string
  duration_ms: number
  created_at: ISODateString
  started_at?: ISODateString
  completed_at?: ISODateString
  assets?: ImageAsset[]
}

export interface ImagePromptTemplatesResponse {
  templates: ImagePromptTemplate[]
}

export interface ImageJobResponse {
  job: ImageGenerationJob
}

export interface ImageJobsResponse {
  jobs: ImageGenerationJob[]
  total: number
}

export interface ImageAssetsResponse {
  assets: ImageAsset[]
  total: number
}

export interface ImagePromptTemplatePayload {
  name?: string
  prompt?: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  tags?: string[]
  favorite?: boolean
}

export interface CreateImageJobPayload {
  prompt: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  upscale?: string
  strict_size?: boolean
  upscale_fit?: 'pad' | 'cover'
  api_key_id?: number
  template_id?: number
  input_images?: string[]
}

export type ApiListResponse<K extends string, T> = {
  [P in K]: T[]
}

export interface OAuthURLResponse {
  auth_url: string
  session_id: string
}

// Public account self-service portal: response for generating an authorization link.
export interface AccountPortalAuthURLResponse {
  auth_url: string
  session_id: string
}

// Public account self-service portal: response for submitting an authorization code.
export interface AccountPortalSubmitResponse {
  message: string
}

export interface UpdateOAuthAccountRequest {
  session_id: string
  code: string
  state: string
  proxy_url?: string
}

export interface OAuthExchangeResponse {
  message: string
  id: number
  email: string
  plan_type: string
}

export interface ObservedInstructionsSample {
  model: string
  originator: string
  instructions: string
  length: number
  truncated: boolean
  observed_at: string
}

// Codex User-Agent shape catalog (paired selection on the settings page) and outbound identity preview.
export interface CodexUserAgentCatalogOption {
  value: string
  weight: number
}

export interface CodexUserAgentCatalogPlatform {
  os_name: string
  os_version: string
  arch: string
  weight: number
}

export interface CodexUserAgentCatalogVersionPair {
  cli_version: string
  app_version: string
  weight: number
}

export interface CodexUserAgentCatalogKind {
  kind: string
  client_name: string
  app_follows_cli: boolean
  default_app_name: string
  default_platform: CodexUserAgentCatalogPlatform
  default_terminal: string
  app_names: CodexUserAgentCatalogOption[] | null
  terminals: CodexUserAgentCatalogOption[] | null
  reference_terminals?: string[] | null
  platforms: CodexUserAgentCatalogPlatform[] | null
  reference_platforms?: CodexUserAgentCatalogPlatform[] | null
  version_pairs: CodexUserAgentCatalogVersionPair[] | null
}

export interface CodexUserAgentCatalog {
  kinds: CodexUserAgentCatalogKind[]
  default_pool_mix: Record<string, number>
}

export interface CodexUserAgentPersona {
  label?: string
  account_id?: number
  user_agent: string
  originator: string
  version: string
  app_version?: string
  target_platform?: string
  source?: string
  status?: string
}

export interface CodexClientVersionPair {
  app_version: string
  cli_version: string
  package_version?: string
  source: string
  artifact_url?: string
  artifact_id?: string
  verified_at?: number
}

export interface CodexClientVersionTarget {
  client_kind: string
  target_platform: string
  status: string
  error?: string
  checked_at: number
  pairs: CodexClientVersionPair[]
}

export interface CodexClientVersionSyncResult {
  fetched_version?: string
  synced_version?: string
  effective_version: string
  cli_version?: string
  source?: string
  status: string
  targets?: CodexClientVersionTarget[]
  updated: boolean
  error?: string
}

export interface CodexUserAgentPreview {
  mode: string
  kind?: string
  persona?: CodexUserAgentPersona
  samples?: CodexUserAgentPersona[]
  warnings?: string[]
  normalized: string
}

export interface ObservedInstructionsResponse {
  samples: ObservedInstructionsSample[]
}

export interface ScheduledConnectionTestConfig {
  enabled: boolean
  model: string
  content: string
  weekday_times: string[]
  weekend_times: string[]
}
export interface ScheduledConnectionTestRecord {
  scheduled_at: string
  attempted_at?: string
  account_id: number
  model?: string
  status: 'success' | 'failed' | 'skipped' | 'unknown'
  success: boolean
  error?: string
  next_reset_5h_at?: string
}
export interface ScheduledConnectionTestResponse {
  config: ScheduledConnectionTestConfig
  timezone: string
  records: ScheduledConnectionTestRecord[]
}
