package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"math/rand"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
	"github.com/codex2api/security/promptfilter"
)

// AccountStatus account status
type AccountStatus int

const (
	StatusReady    AccountStatus = iota // available
	StatusCooldown                      // cooling down (rate limited)
	StatusError                         // unavailable (RT invalid, etc.)
)

// AccountHealthTier is the account health tier (used only for scheduling priority, not exposed directly to external APIs)
type AccountHealthTier string

const (
	HealthTierHealthy AccountHealthTier = "healthy"
	HealthTierWarm    AccountHealthTier = "warm"
	HealthTierRisky   AccountHealthTier = "risky"
	HealthTierBanned  AccountHealthTier = "banned"
)

const (
	DefaultTestContent = "hi"
	// DefaultTestModel is the factory default model for connectivity tests; it must still be online upstream and usable on free/plus/pro
	// (a model that all three tiers can use; gpt-5.4 was retired in 2026-09).
	DefaultTestModel    = "gpt-5.5"
	MaxTestContentRunes = 8192
)

// NormalizeTestContent returns the prompt text used by connection tests.
// Empty content keeps the historical minimal probe behavior.
func NormalizeTestContent(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return DefaultTestContent
	}
	return content
}

// Account is the runtime account state
type Account struct {
	daybreak                  database.DaybreakSnapshot
	codexLiteSupport          map[string]bool
	codexCapabilityGeneration int64
	codexCapabilityObservedAt int64
	UpstreamRequestIDHeader   string
	mu                        sync.RWMutex
	usageSyncMu               sync.Mutex
	// Snapshot writes serialize per account without blocking other accounts or frame ingestion.
	usageSnapshotPersistMu sync.Mutex
	modelCatalogMu         sync.Mutex
	usageObservedAt        time.Time
	DBID                   int64 // database ID
	RefreshToken           string
	SessionToken           string
	AccessToken            string
	ExpiresAt              time.Time
	AccountID              string
	Email                  string
	PlanType               string
	ProxyURL               string
	CustomHeaders          map[string]string
	UpstreamType           string
	Models                 []string
	// CodexFingerprintMode is in codex_fingerprint_mode.go: the device fingerprint convergence level of official Codex outbound requests
	// (off / device / session / full), default off.
	CodexFingerprintMode string
	// Timezone is the IANA timezone bound to the account (credentials.timezone). The official Codex outbound path uses it to
	// rewrite the timezone and date in environment_context of the request body (see proxy/codex_environment_context.go);
	// empty = not bound, the downstream value is passed through.
	Timezone string
	// Codex Agent Identity (auth_mode=agentIdentity): no AT/RT is stored; each upstream request uses
	// agent_private_key (Ed25519, PKCS#8 base64) to sign dynamically. AgentTaskID is obtained from task registration,
	// cached at runtime and persisted (credentials.task_id).
	CodexAuthMode   string
	AgentRuntimeID  string
	AgentPrivateKey string
	AgentTaskID     string
	// CredentialGeneration fences asynchronous observations and refresh results against a credential change. CredentialFamilyID is stable across AT/RT rotation and is safe to use as a cross-instance lease key (it contains no credential).
	CredentialGeneration        int64
	CredentialFamilyID          string
	Status                      AccountStatus
	CooldownUtil                time.Time
	CooldownReason              string // rate_limited / rate_limited_5h / responses_rate_limited / unauthorized / empty
	usageCooldownPersistMu      sync.Mutex
	usageCooldownPersistPending bool
	ErrorMsg                    string

	// Usage progress (passively parsed from Codex response headers)
	UsagePercent7d      float64 // 7d window usage rate 0-100+
	UsagePercent7dValid bool
	Reset7dAt           time.Time // 7d window reset time
	// Window7dSeconds is the real period in seconds of the "long window" (the 7d slot): plus/pro is usually 7d (604800),
	// the team plan is actually monthly (about 2592000). 0 = unknown (treated as the 7d default). The natural rate of smart pacing
	// must be computed from the real period, otherwise the team monthly window is treated as 7 days, the natural rate is too high, and it over-limits.
	Window7dSeconds     int64
	UsagePercent5h      float64 // 5h window usage rate 0-100+
	UsagePercent5hValid bool
	Reset5hAt           time.Time // 5h window reset time
	UsageUpdatedAt      time.Time // 7d usage snapshot refresh time
	UsageUpdatedAt5h    time.Time // 5h usage snapshot refresh time
	// usageFramePersisted* records the time and percentage of the most recent persisted WS per-frame observation.
	// The in-memory snapshot is updated live on every frame; persisting is only for restart recovery, and on large account pools it is throttled by time and change size,
	// so that every upstream response does not turn into a database write.
	usageFramePersistedAt         time.Time
	usageFramePersisted5h         float64
	usageFramePersisted7d         float64
	usageFramePersistPending      bool
	usageFramePlanPending         bool
	usageFrameObservationMu       sync.Mutex
	usageFramePending             []usageFrameObservation
	usageFrameObservationRunning  bool
	usageFrameObservationOverflow bool
	// usageFrameLastAt/Kind records the most recently applied WS quota frame, for the admin side only
	// to verify that frame ingestion works; it does not take part in scheduling.
	usageFrameLastAt   time.Time
	usageFrameLastKind string
	// activated5hResetAt is the Reset5hAt for which a minimal "window open" /responses request has already been sent.
	// Each observed 5h window is activated at most once (issue #581).
	activated5hResetAt time.Time
	// Spark is a usage window on Pro/Prolite accounts, separate from the main 5h/7d windows.
	UsagePercentSpark      float64
	UsagePercentSparkValid bool
	ResetSparkAt           time.Time
	UsageUpdatedAtSpark    time.Time

	// RateLimitResetCredits is the remaining active reset count of the OpenAI official account, taken from
	// the rate_limit_reset_credits.available_count field of the /backend-api/wham/usage response.
	// -1 means not probed yet (unknown); >=0 is the known count.
	RateLimitResetCredits      int
	RateLimitResetCreditsValid bool
	// ApplicableResetCredits is the number of reset credits that are "applicable" right now, from the wham/usage
	// rate_limit_reset_credits.applicable_available_count field. When not rate limited, upstream returns 0
	// (the credits are still valid but do not apply at this moment).
	ApplicableResetCredits      int
	ApplicableResetCreditsValid bool
	// Credits* is the credits balance snapshot returned by wham/usage (zero quota cost).
	// Balance keeps the upstream string as is (unit unknown, no conversion); Valid means it has been probed.
	CreditsBalance              string
	CreditsBalanceKnown         bool
	CreditsHasCredits           bool
	CreditsUnlimited            bool
	CreditsOverageLimitReached  bool
	CreditsSpendControlReached  bool
	CreditsSpendControlValid    bool
	CreditsRateLimitReachedType string
	CreditsValid                bool
	// creditsPersistedKey is the fingerprint of the most recently persisted credits snapshot, used to skip writes when nothing changed.
	// It compares business fields only, without timestamps; otherwise every probe would write to the database.
	// creditsPersistedFlagsKey is the fingerprint of the same snapshot with the balance removed: the normal response-header path only
	// persists when a business flag changes; the balance decreases on every request, so it must not be written each time.
	creditsPersistedKey      string
	creditsPersistedFlagsKey string
	creditsPersistMu         sync.Mutex
	// resetCreditsProbedAt records the time of the most recent successful wham usage probe.
	// The "active reset count" can only be refreshed through the wham probe (normal /responses traffic does not carry this field),
	// so it is used on its own to decide whether the reset count has expired, so that active accounts whose usage snapshot keeps being refreshed by traffic are not left unprobed for long.
	resetCreditsProbedAt time.Time
	// subscriptionMeta is subscription sync metadata (last query time / sync status / source / grace period, etc.),
	// persisted in credentials; CheckedAt doubles as the throttle for the web /subscriptions probe. (issue #360)
	subscriptionMeta SubscriptionMeta
	// subscriptionSyncInFlight marks an asynchronous authoritative subscription sync as in flight, to avoid starting several for the same account at once.
	subscriptionSyncInFlight bool

	usageProbeInFlight          bool
	recoveryProbeInFlight       bool
	lastAuthVerifyAt            time.Time // throttle timestamp for the auth verification probe triggered after an abnormal WS upstream close
	AutoPause5hThreshold        float64   // 0..1, 0 = disabled
	AutoPause7dThreshold        float64   // 0..1, 0 = disabled
	AutoPause5hDisabled         bool
	AutoPause7dDisabled         bool
	effectiveAutoPause5h        float64 // resolved: account > group > global
	effectiveAutoPause7d        float64
	autoPause5hGuardBandPercent float64 // percentage points, 0 = disabled
	autoPause5hGuardConcurrency int     // 0 = disabled; otherwise guard-band concurrency cap
	// Smart pacing (issue #312): spreads usage evenly over the remaining quota / remaining time until the window resets,
	// and scales concurrency down to a sustainable rate when burning too fast. Parameters come from the Store's global settings snapshot.
	smartPacingEnabled        bool
	smartPacingMinConcurrency int
	smartPacingWindows5h      bool
	smartPacingWindows7d      bool
	DispatchCountLimit        int64 // 0 = disabled; per-reset-window dispatch cap
	dispatchCountMu           sync.Mutex
	dispatchWindowUsed        int64
	dispatchWindowResetAt     time.Time
	// SchedulerPriority is the account scheduling priority (issue #358): a higher value is always scheduled first,
	// and only within the same priority do health tier and scheduling score compete. 0 is the default; negative values can push an account down to a fallback channel.
	SchedulerPriority int64

	// scheduling health signals
	HealthTier               AccountHealthTier
	SchedulerScore           float64
	DispatchScore            float64
	ScoreBiasEffective       int64
	BaseConcurrencyEffective int64
	groupBaseConcurrency     int64 // resolved from memberships; 0 means no group override
	DynamicConcurrencyLimit  int64
	LatencyEWMA              float64
	SuccessStreak            int
	FailureStreak            int
	// PermanentRefreshFailures is the number of consecutive unrecoverable refresh failures (isNonRetryable),
	// memory-only. Reset to zero on a successful refresh or a manual clear (ClearCooldown); once it reaches
	// permanentRefreshFailureTerminalLimit, the account moves to the error terminal state and leaves the recovery probe rotation.
	PermanentRefreshFailures int
	// LastFailureKind records the attribution of the most recent failure (the same as kind in ReportRequestFailure),
	// memory-only. It separates "transport-layer jitter" from "the account itself has a problem", see
	// the exemption for isolated disconnects in recomputeSchedulerLocked.
	LastFailureKind     string
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	LastUnauthorizedAt  time.Time
	LastRateLimitedAt   time.Time
	LastTimeoutAt       time.Time
	LastServerErrorAt   time.Time
	LastRecoveryProbeAt time.Time
	// transientRateLimitBackoff is the in-memory progressive cooldown
	// exponent for account-wide Codex throttle (bare 429 / rate_limit*).
	// It is not persisted: a restart simply starts again at 15s.
	transientRateLimitBackoff int
	// transientRateLimitUntil mirrors CooldownUtil while the active cooldown
	// was created by MarkTransientRateLimited. A later quota cooldown moves
	// CooldownUtil away from it, which is how the two are told apart.
	transientRateLimitUntil time.Time
	transientRateLimitTimer *time.Timer // at most one recovery timer per throttled account

	// sliding window success rate (last N requests)
	RecentResults    [20]uint8 // 1=success, 0=failure
	RecentResultsIdx int       // write position in the ring buffer
	RecentResultsCnt int       // number recorded (max 20)

	// High-concurrency scheduling metrics (atomic operations, no lock needed). atomic.Int64 is used instead of a bare int64:
	// On 32-bit platforms (arm/386) only typed atomics guarantee 8-byte alignment; a bare field in the middle of the struct would panic.
	ActiveRequests atomic.Int64 // number of requests currently executing
	// OccupiedRequests includes the current request and the buffer slot kept for the original session after it finishes successfully.
	// Scheduling admission reads it; the admin side still shows the real in-flight count and the occupancy including the buffer separately.
	OccupiedRequests   atomic.Int64
	TotalRequests      atomic.Int64 // total requests so far
	LastUsedAt         atomic.Int64 // last used time (UnixNano)
	Disabled           int32        // atomic flag, 1 = not schedulable immediately (set instantly on 401, no lock needed)
	AddedAt            atomic.Int64 // time added to the account pool (UnixNano), used for expiry cleanup
	Locked             int32        // atomic flag, 1 = locked, automatic cleanup skips this account
	IntelligencePaused int32        // separate, persisted pause of the intelligence check; does not change the manual enabled flag
	DispatchPaused     int32        // atomic flag, 1 = disable scheduling selection; does not affect refresh/probe/cleanup

	// per-account scheduling config (nil = follow the default)
	ScoreBiasOverride       *int64
	BaseConcurrencyOverride *int64
	CreditEnabled           bool // credit account flag
	CreditSkipUsageWindow   bool // skip the usage window penalty and local rate-limit marking
	// IgnoreUsageLimitStatusOverride follows the global setting when nil; the effective value is resolved by Store.
	IgnoreUsageLimitStatusOverride *bool
	ignoreUsageLimitStatus         bool
	SkipWarmTier                   bool // skip the warm tier downgrade
	KeepConcurrencyOnDegrade       bool // warm/risky do not lower concurrency; the tier is still computed (issue #772)
	AllowedAPIKeyIDs               []int64
	allowedAPIKeySet               map[int64]struct{}
	Tags                           []string
	GroupIDs                       []int64
	ModelCooldowns                 map[string]ModelCooldown
	ModelCooldownModeOverride      *string
	ModelCooldownSecondsOverride   *int
	ModelCooldownBackoffOverride   *bool

	SubscriptionExpiresAt time.Time
}

type ModelCooldown struct {
	Model        string
	Reason       string
	ResetAt      time.Time
	UpdatedAt    time.Time
	BackoffLevel int
}

// AccountFilter is used for per-request scheduling constraints, for example limiting account plans by model.
type AccountFilter func(*Account) bool

const (
	defaultBackgroundRefreshInterval = 2 * time.Minute
	defaultUsageProbeMaxAge          = 10 * time.Minute
	defaultUsageProbeConcurrency     = 16
	defaultRecoveryProbeInterval     = 30 * time.Minute
	// probeBoundaryLag is the lag of the "probe on the dot" timer relative to the boundary moment: slightly later than the reset/cooldown
	// end before probing, to ensure `!ResetAt.After(now)` in NeedsUsageProbe already holds, and to leave room for clock skew between upstream and local,
	// so that probing too early does not still get old data from before the reset.
	probeBoundaryLag                 = 2 * time.Second
	premium5hUrgencyWindow           = 4 * time.Hour
	premium5hUrgencyMaxBonus         = 25.0
	premium5hUrgencyMinRemainingPct  = 5.0
	premium5hUrgencyFullRemainingPct = 50.0
	premium7dUrgencyWindow           = 72 * time.Hour
	premium7dUrgencyMaxBonus         = 80.0
	premium7dUrgencyMinRemainingPct  = 5.0
	premium7dUrgencyFullRemainingPct = 70.0
	expiryUrgencyUrgentDays          = 3
	expiryUrgencyWarnDays            = 7
	expiryUrgencyUrgentBonus         = 60.0
	expiryUrgencyWarnBonus           = 25.0
)

// SchedulerBreakdown is the breakdown of the scheduling score
type SchedulerBreakdown struct {
	UnauthorizedPenalty float64
	RateLimitPenalty    float64
	TimeoutPenalty      float64
	ServerPenalty       float64
	FailurePenalty      float64
	SuccessBonus        float64
	ProvenBonus         float64 // bonus for verified accounts (TotalRequests > 10)
	UsagePenalty7d      float64
	UsageUrgencyBonus5h float64
	UsageUrgencyBonus7d float64
	ExpiryUrgencyBonus  float64
	LatencyPenalty      float64
	SuccessRatePenalty  float64 // sliding window success rate penalty
}

// SchedulerDebugSnapshot is the scheduling debug snapshot
type SchedulerDebugSnapshot struct {
	HealthTier               string
	SchedulerScore           float64
	DispatchScore            float64
	ScoreBiasOverride        *int64
	ScoreBiasEffective       int64
	BaseConcurrencyOverride  *int64
	BaseConcurrencyEffective int64
	DynamicConcurrencyLimit  int64
	Breakdown                SchedulerBreakdown
	LastUnauthorizedAt       time.Time
	LastRateLimitedAt        time.Time
	LastTimeoutAt            time.Time
	LastServerErrorAt        time.Time
}

// AccountListRuntimeSnapshot is the inexpensive runtime projection used by
// the admin account list. Unlike SchedulerDebugSnapshot it never recomputes
// scheduler state; the scheduler already keeps these fields current on every
// state transition. Reading them under one lock keeps large-pool list rebuilds
// bounded without weakening the status shown to operators.
type AccountListRuntimeSnapshot struct {
	Status                  string
	UsingCredits            bool
	GroupIDs                []int64
	PlanType                string
	UsagePercent5h          float64
	UsagePercent5hValid     bool
	UsagePercent7d          float64
	UsagePercent7dValid     bool
	UsagePercentSpark       float64
	UsagePercentSparkValid  bool
	ResetSparkAt            time.Time
	HealthTier              string
	DispatchScore           float64
	LatencyPenalty          float64
	LastUnauthorizedAt      time.Time
	LastRateLimitedAt       time.Time
	LastTimeoutAt           time.Time
	ActiveRequests          int64
	OccupiedRequests        int64
	DynamicConcurrencyLimit int64
	Reset5hAt               time.Time
	Reset7dAt               time.Time
	Window7dSeconds         int64
	CooldownReason          string
	CooldownUntil           time.Time
}

// ID returns the database ID
func (a *Account) ID() int64 {
	return a.DBID
}

// Mu returns the read-write lock (so external packages can read fields safely)
func (a *Account) Mu() *sync.RWMutex {
	return &a.mu
}

func (a *Account) hasDispatchCredentialLocked() bool {
	if a == nil {
		return false
	}
	if a.isCodexAgentIdentityLocked() {
		// Agent Identity has no AT: the private key is the credential, and it can be scheduled directly (task_id is registered lazily before the request).
		return true
	}
	return strings.TrimSpace(a.AccessToken) != ""
}

// SupportsCodexModel reports whether a Codex OAuth account can serve the given model.
// Models empty means no allowlist is configured and all models are allowed; when non-empty, it matches exactly and case-insensitively.
func (a *Account) SupportsCodexModel(model string) bool {
	if a == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.Models) == 0 {
		return true
	}
	for _, candidate := range a.Models {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

// CodexModels returns the supported model allowlist configured for the Codex OAuth account (empty means all are allowed).
func (a *Account) CodexModels() []string {
	if a == nil {
		return []string{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneStringSlice(a.Models)
}

func (a *Account) GetProxyURL() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.ProxyURL)
}

func (a *Account) GetAccessToken() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.AccessToken)
}

func (a *Account) GetCustomHeaders() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneStringMap(a.CustomHeaders)
}

// EffectiveAccountID returns the workspace ID actually used for upstream routing: when a custom request header overrides the
// Chatgpt-Account-Id header, the overriding value wins (consistent with proxy/wsrelay forwarding behavior),
// side requests such as quota probes must use it, otherwise they measure a different space from the traffic.
func (a *Account) EffectiveAccountID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if v := openaiidentity.WorkspaceOverrideFromHeaders(a.CustomHeaders); v != "" {
		return v
	}
	return strings.TrimSpace(a.AccountID)
}

// AccountIDOverridden reports whether a custom request header routes traffic to a workspace different from the OAuth identity.
func (a *Account) AccountIDOverridden() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	v := openaiidentity.WorkspaceOverrideFromHeaders(a.CustomHeaders)
	return v != "" && v != strings.TrimSpace(a.AccountID)
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func cloneInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func cloneInt64Slice(values []int64) []int64 {
	if len(values) == 0 {
		return []int64{}
	}
	cloned := make([]int64, len(values))
	copy(cloned, values)
	return cloned
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func normalizeModelList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return []string{}
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result
}

func normalizeAllowedAPIKeyIDs(values []int64) []int64 {
	if len(values) == 0 {
		return []int64{}
	}
	unique := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, exists := unique[value]; exists {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	if len(result) == 0 {
		return []int64{}
	}
	return result
}

func reflectOptionalInt64Field(src any, fieldName string) *int64 {
	if src == nil || fieldName == "" {
		return nil
	}

	v := reflect.ValueOf(src)
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil
	}

	field := v.FieldByName(fieldName)
	if !field.IsValid() {
		return nil
	}

	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return nil
		}
		field = field.Elem()
	}

	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		return &value
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := int64(field.Uint())
		return &value
	case reflect.Float32, reflect.Float64:
		value := int64(field.Float())
		return &value
	case reflect.Struct:
		validField := field.FieldByName("Valid")
		if validField.IsValid() && validField.Kind() == reflect.Bool && !validField.Bool() {
			return nil
		}
		int64Field := field.FieldByName("Int64")
		if int64Field.IsValid() && int64Field.Kind() == reflect.Int64 {
			value := int64Field.Int()
			return &value
		}
	}

	return nil
}

// fastRandN is a lightweight random number (used for scheduling fairness, no cryptographic security needed)
func fastRandN(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}

func concurrencyLimitForTier(baseLimit int64, tier AccountHealthTier) int64 {
	if baseLimit <= 0 {
		baseLimit = 1
	}

	switch tier {
	case HealthTierHealthy:
		return baseLimit
	case HealthTierWarm:
		half := baseLimit / 2
		if half < 1 {
			return 1
		}
		return half
	case HealthTierRisky:
		return 1
	case HealthTierBanned:
		return 0
	default:
		if baseLimit >= 2 {
			return 2
		}
		return 1
	}
}

// KeepConcurrencyOnDegradeCredentialKey is the key of the "keep concurrency on degrade" switch in the credentials JSON (issue #772).
const KeepConcurrencyOnDegradeCredentialKey = "keep_concurrency_on_degrade"

// tierConcurrencyLimitLocked converts the concurrency limit by health tier; when the account enables
// KeepConcurrencyOnDegrade, warm/risky keep the base concurrency, and only banned drops to zero.
func (a *Account) tierConcurrencyLimitLocked(baseLimit int64, tier AccountHealthTier) int64 {
	if a.KeepConcurrencyOnDegrade && (tier == HealthTierWarm || tier == HealthTierRisky) {
		tier = HealthTierHealthy
	}
	return concurrencyLimitForTier(baseLimit, tier)
}

func defaultScoreBiasForPlan(planType string) int64 {
	switch NormalizePlanType(planType) {
	// k12 is the education team workspace, which behaves the same as team (issue #282)
	case "pro", "plus", "team", "k12":
		return 50
	default:
		return 0
	}
}

func tierPriority(tier AccountHealthTier) int {
	switch tier {
	case HealthTierHealthy:
		return 3
	case HealthTierWarm:
		return 2
	case HealthTierRisky:
		return 1
	default:
		return 0
	}
}

func (a *Account) healthTierLocked() AccountHealthTier {
	if a.HealthTier != "" {
		return a.HealthTier
	}
	if a.hasDispatchCredentialLocked() {
		return HealthTierHealthy
	}
	return HealthTierWarm
}

func (a *Account) recordLatencyLocked(latency time.Duration) {
	if latency <= 0 {
		return
	}

	latencyMs := float64(latency.Milliseconds())
	if latencyMs <= 0 {
		return
	}
	if a.LatencyEWMA == 0 {
		a.LatencyEWMA = latencyMs
		return
	}
	a.LatencyEWMA = a.LatencyEWMA*0.8 + latencyMs*0.2
}

// recordResultLocked records one request result in the sliding window (the lock must be held)
func (a *Account) recordResultLocked(success bool) {
	if success {
		a.RecentResults[a.RecentResultsIdx] = 1
	} else {
		a.RecentResults[a.RecentResultsIdx] = 0
	}
	a.RecentResultsIdx = (a.RecentResultsIdx + 1) % len(a.RecentResults)
	if a.RecentResultsCnt < len(a.RecentResults) {
		a.RecentResultsCnt++
	}
}

// recentSuccessRateLocked computes the sliding window success rate (0.0 ~ 1.0)
func (a *Account) recentSuccessRateLocked() float64 {
	if a.RecentResultsCnt == 0 {
		return 1.0 // returns 100% when there is no data
	}
	var sum int
	for i := 0; i < a.RecentResultsCnt; i++ {
		sum += int(a.RecentResults[i])
	}
	return float64(sum) / float64(a.RecentResultsCnt)
}

// linearDecay is a linear decay: returns base × max(0, 1 - elapsed/window)
func linearDecay(base float64, elapsed, window time.Duration) float64 {
	if elapsed >= window || window <= 0 {
		return 0
	}
	return base * (1.0 - float64(elapsed)/float64(window))
}

func (a *Account) schedulerBreakdownLocked(now time.Time) SchedulerBreakdown {
	breakdown := SchedulerBreakdown{}
	premium5hLimited := a.premium5hRateLimitedLocked(now)

	// linear decay penalty: smoother over time, with no sudden jumps
	if !a.LastUnauthorizedAt.IsZero() {
		elapsed := now.Sub(a.LastUnauthorizedAt)
		breakdown.UnauthorizedPenalty = linearDecay(50, elapsed, 24*time.Hour)
	}
	if !a.LastRateLimitedAt.IsZero() {
		elapsed := now.Sub(a.LastRateLimitedAt)
		breakdown.RateLimitPenalty = linearDecay(22, elapsed, time.Hour)
	}
	if !a.LastTimeoutAt.IsZero() {
		elapsed := now.Sub(a.LastTimeoutAt)
		breakdown.TimeoutPenalty = linearDecay(18, elapsed, 15*time.Minute)
	}
	if !a.LastServerErrorAt.IsZero() {
		elapsed := now.Sub(a.LastServerErrorAt)
		breakdown.ServerPenalty = linearDecay(12, elapsed, 15*time.Minute)
	}

	breakdown.FailurePenalty = float64(clampInt(a.FailureStreak*6, 0, 24))
	if !premium5hLimited {
		breakdown.SuccessBonus = float64(clampInt(a.SuccessStreak*2, 0, 12))
	}

	// verified accounts (more than 10 cumulative requests) are scheduled first
	if !premium5hLimited && a.TotalRequests.Load() > 10 {
		breakdown.ProvenBonus = 20
	}

	// sliding window success rate penalty
	if a.RecentResultsCnt >= 5 { // only counted with at least 5 requests
		rate := a.recentSuccessRateLocked()
		switch {
		case rate < 0.5:
			breakdown.SuccessRatePenalty = 15
		case rate < 0.75:
			breakdown.SuccessRatePenalty = 8
		}
	}

	if !a.skipsUsageWindowLimitsLocked() && a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 100:
			breakdown.UsagePenalty7d = 40
		case a.UsagePercent7d >= 95:
			breakdown.UsagePenalty7d = 30
		case a.UsagePercent7d >= 85:
			breakdown.UsagePenalty7d = 18
		case a.UsagePercent7d >= 70:
			breakdown.UsagePenalty7d = 8
		}
	}

	switch {
	case a.LatencyEWMA >= 20000:
		breakdown.LatencyPenalty = 15
	case a.LatencyEWMA >= 10000:
		breakdown.LatencyPenalty = 8
	case a.LatencyEWMA >= 5000:
		breakdown.LatencyPenalty = 4
	}

	return breakdown
}

func (a *Account) premium5hUsageUrgencyBonusLocked(now time.Time) float64 {
	if !isPremium5hPlan(a.PlanType) {
		return 0
	}
	if !a.UsagePercent5hValid || a.Reset5hAt.IsZero() {
		return 0
	}
	if a.UsagePercent5h >= 100 || a.premium5hRateLimitedLocked(now) {
		return 0
	}
	if a.AccessToken == "" || a.Status == StatusError || a.HealthTier == HealthTierBanned {
		return 0
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0 {
		return 0
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return 0
	}
	if a.usageExhaustedLocked() {
		return 0
	}

	timeRemaining := a.Reset5hAt.Sub(now)
	if timeRemaining <= 0 || timeRemaining > premium5hUrgencyWindow {
		return 0
	}

	quotaRemaining := 100 - a.UsagePercent5h
	if quotaRemaining <= premium5hUrgencyMinRemainingPct {
		return 0
	}

	timeFactor := 1 - float64(timeRemaining)/float64(premium5hUrgencyWindow)
	quotaFactor := quotaRemaining / premium5hUrgencyFullRemainingPct
	if quotaFactor > 1 {
		quotaFactor = 1
	}
	if quotaFactor < 0 {
		quotaFactor = 0
	}

	return premium5hUrgencyMaxBonus * timeFactor * quotaFactor
}

func (a *Account) premium7dUsageUrgencyBonusLocked(now time.Time) float64 {
	if !IsPlusOrHigherPlan(a.PlanType) {
		return 0
	}
	if !a.UsagePercent7dValid || a.Reset7dAt.IsZero() {
		return 0
	}
	if a.UsagePercent7d >= 100 {
		return 0
	}
	if a.AccessToken == "" || a.Status == StatusError || a.HealthTier == HealthTierBanned {
		return 0
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0 {
		return 0
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return 0
	}

	timeRemaining := a.Reset7dAt.Sub(now)
	if timeRemaining <= 0 || timeRemaining > premium7dUrgencyWindow {
		return 0
	}

	quotaRemaining := 100 - a.UsagePercent7d
	if quotaRemaining <= premium7dUrgencyMinRemainingPct {
		return 0
	}

	timeFactor := 1 - float64(timeRemaining)/float64(premium7dUrgencyWindow)
	quotaFactor := quotaRemaining / premium7dUrgencyFullRemainingPct
	if quotaFactor > 1 {
		quotaFactor = 1
	}
	if quotaFactor < 0 {
		quotaFactor = 0
	}
	weightedQuotaFactor := 0.6 + 0.4*quotaFactor

	return premium7dUrgencyMaxBonus * timeFactor * weightedQuotaFactor
}

func (a *Account) effectiveBaseConcurrencyLocked(storeBaseLimit int64) int64 {
	if a.BaseConcurrencyOverride != nil && *a.BaseConcurrencyOverride > 0 {
		return *a.BaseConcurrencyOverride
	}
	if a.groupBaseConcurrency > 0 {
		return a.groupBaseConcurrency
	}
	if storeBaseLimit <= 0 {
		return 1
	}
	return storeBaseLimit
}

func (a *Account) dispatchBonusEligibleLocked(now time.Time, tier AccountHealthTier) bool {
	if tier != HealthTierHealthy && tier != HealthTierWarm {
		return false
	}
	if a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.usageExhaustedLocked() {
		return false
	}
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	if !a.hasDispatchCredentialLocked() {
		return false
	}
	return true
}

func (a *Account) effectiveScoreBiasLocked(now time.Time, tier AccountHealthTier) int64 {
	if !a.dispatchBonusEligibleLocked(now, tier) {
		return 0
	}
	if a.ScoreBiasOverride != nil {
		return *a.ScoreBiasOverride
	}
	return defaultScoreBiasForPlan(a.PlanType)
}

// expiryUrgencyBonusLocked adds a bonus to an account whose subscription is close to expiring, so the scheduler uses it first.
// <= 3d urgent (+60) / <= 7d warning (+25) / other (0). Expired/free/api accounts get no bonus.
func (a *Account) expiryUrgencyBonusLocked(now time.Time) float64 {
	if a.SubscriptionExpiresAt.IsZero() {
		return 0
	}
	plan := strings.ToLower(strings.TrimSpace(a.PlanType))
	if plan == "" || plan == "free" || plan == "api" {
		return 0
	}
	remaining := a.SubscriptionExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	days := remaining.Hours() / 24
	switch {
	case days <= expiryUrgencyUrgentDays:
		return expiryUrgencyUrgentBonus
	case days <= expiryUrgencyWarnDays:
		return expiryUrgencyWarnBonus
	}
	return 0
}

func (a *Account) recomputeSchedulerLocked(baseLimit int64) {
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked(now)
	score := 100.0 -
		breakdown.UnauthorizedPenalty -
		breakdown.RateLimitPenalty -
		breakdown.TimeoutPenalty -
		breakdown.ServerPenalty -
		breakdown.FailurePenalty -
		breakdown.UsagePenalty7d -
		breakdown.LatencyPenalty -
		breakdown.SuccessRatePenalty +
		breakdown.SuccessBonus +
		breakdown.ProvenBonus

	tier := HealthTierHealthy
	switch {
	case score < 60:
		tier = HealthTierRisky
	case score < 85:
		tier = HealthTierWarm
	}

	if a.LastFailureAt.After(a.LastSuccessAt) && !a.LastFailureAt.IsZero() && tier == HealthTierHealthy &&
		!a.isolatedTransportFailureLocked() {
		tier = HealthTierWarm
	}
	if !a.LastUnauthorizedAt.IsZero() && now.Sub(a.LastUnauthorizedAt) < 24*time.Hour && tier == HealthTierHealthy {
		tier = HealthTierWarm
	}
	if !a.skipsUsageWindowLimitsLocked() && a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 95:
			tier = HealthTierRisky
		case a.UsagePercent7d >= 85 && tier == HealthTierHealthy:
			tier = HealthTierWarm
		}
	}
	if a.HealthTier == HealthTierBanned {
		tier = HealthTierBanned
	}
	if a.premium5hRateLimitedLocked(now) && tier != HealthTierBanned {
		tier = HealthTierRisky
	}
	if a.Status == StatusCooldown && a.CooldownReason == premium5hCooldownReason && tier != HealthTierBanned {
		tier = HealthTierRisky
	}
	if a.SkipWarmTier && tier == HealthTierWarm {
		tier = HealthTierHealthy
	}

	baseConcurrencyEffective := a.effectiveBaseConcurrencyLocked(baseLimit)
	scoreBiasEffective := a.effectiveScoreBiasLocked(now, tier)
	if a.dispatchBonusEligibleLocked(now, tier) {
		breakdown.UsageUrgencyBonus5h = a.premium5hUsageUrgencyBonusLocked(now)
		breakdown.UsageUrgencyBonus7d = a.premium7dUsageUrgencyBonusLocked(now)
		breakdown.ExpiryUrgencyBonus = a.expiryUrgencyBonusLocked(now)
	}
	dispatchScore := score + float64(scoreBiasEffective) + breakdown.UsageUrgencyBonus5h + breakdown.UsageUrgencyBonus7d + breakdown.ExpiryUrgencyBonus - a.quotaAutoPause5hGuardDispatchPenaltyLocked(now)

	a.HealthTier = tier
	a.SchedulerScore = score
	a.DispatchScore = dispatchScore
	a.ScoreBiasEffective = scoreBiasEffective
	a.BaseConcurrencyEffective = baseConcurrencyEffective
	a.DynamicConcurrencyLimit = a.quotaAutoPause5hGuardConcurrencyLimitLocked(a.tierConcurrencyLimitLocked(baseConcurrencyEffective, tier), now)
	a.DynamicConcurrencyLimit = a.smartPacingConcurrencyLimitLocked(a.DynamicConcurrencyLimit, now)
	if a.premium5hRateLimitedLocked(now) && a.DynamicConcurrencyLimit > 1 {
		a.DynamicConcurrencyLimit = 1
	}
}

func (a *Account) schedulerSnapshot(baseLimit int64) (AccountHealthTier, float64, float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recomputeSchedulerLocked(baseLimit)
	return a.HealthTier, a.SchedulerScore, a.DispatchScore, a.DynamicConcurrencyLimit
}

// IsAvailable checks whether the account is available
func (a *Account) IsAvailable() bool {
	// atomic flag first: set instantly on 401, so concurrent requests can be blocked without waiting for a lock
	if atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0 {
		return false
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.isAvailableLocked(time.Now())
}

func (a *Account) isAvailableLocked(now time.Time) bool {
	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return false
	}
	// IgnoreUsageLimitStatus only protects an already active continuation;
	// fresh sessions remain fenced by the latest local usage observation.
	if a.usageWindowBlocksFreshDispatchLocked(now) {
		return false
	}
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	// recovers automatically once the cooldown period has passed
	if a.Status == StatusCooldown && !now.Before(a.CooldownUtil) {
		return a.hasDispatchCredentialLocked()
	}
	return a.hasDispatchCredentialLocked()
}

func normalizeQuotaAutoPauseThreshold(value float64) float64 {
	switch {
	case value <= 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

const (
	defaultAutoPause5hGuardBandPercent = 5.0
	defaultAutoPause5hGuardConcurrency = 1
	maxAutoPause5hGuardDispatchPenalty = 50.0

	defaultSmartPacingMinConcurrency = 1
	smartPacingWindow5h              = 5 * time.Hour
	smartPacingWindow7d              = 7 * 24 * time.Hour
)

func normalizeSmartPacingMinConcurrency(value int) int {
	if value < 1 {
		return 1
	}
	if value > 1000 {
		return 1000
	}
	return value
}

// parseSmartPacingWindows parses the "5h,7d" form and returns whether to pace the 5h / 7d windows.
// empty or invalid input always falls back to enabling both windows.
func parseSmartPacingWindows(raw string) (bool, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return true, true
	}
	var w5h, w7d bool
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "5h":
			w5h = true
		case "7d":
			w7d = true
		}
	}
	if !w5h && !w7d {
		return true, true
	}
	return w5h, w7d
}

// Long window (7d slot) period detection: the long rate-limit window of free/team plans is actually a monthly window (about 30 days = 2592000s),
// not the weekly window of plus/pro (7 days = 604800s). A 28–31 day tolerance absorbs slight server-side jitter.
const (
	monthlyWindowMinSeconds int64 = 28 * 24 * 60 * 60
	monthlyWindowMaxSeconds int64 = 31 * 24 * 60 * 60
)

func isMonthlyWindowSeconds(sec int64) bool {
	return sec >= monthlyWindowMinSeconds && sec <= monthlyWindowMaxSeconds
}

// IsMonthlyWindowSeconds reports whether the window period is a monthly window (28–31 days, including the exact value 2592000).
// Exported so the proxy layer's wham/header window classification reuses it, keeping a single source of truth for the criterion.
func IsMonthlyWindowSeconds(sec int64) bool {
	return isMonthlyWindowSeconds(sec)
}

// normalizeSmartPacingWindows normalizes to a canonical string (used for persistence and display).
func normalizeSmartPacingWindows(raw string) string {
	w5h, w7d := parseSmartPacingWindows(raw)
	switch {
	case w5h && w7d:
		return "5h,7d"
	case w5h:
		return "5h"
	default:
		return "7d"
	}
}

// smartPacingRatio computes the window's "pacing ratio" = sustainable rate / natural rate.
//
//	sustainable rate = remaining quota % / remaining time
//	natural rate    = 100% / window length (the rate that spreads the whole window's quota evenly across the whole window)
//	ratio      = remaining quota % × window length / (100 × remaining time)
//
// ratio >= 1 means not burning ahead of pace (no rate limit needed); ratio < 1 means burning too fast, so concurrency must be scaled down proportionally.
// ok=false means the usage/reset signal is invalid or the window has rolled over; do not intervene in this case.
func smartPacingRatio(usage float64, valid bool, resetAt time.Time, window time.Duration, now time.Time) (float64, bool) {
	if !valid || resetAt.IsZero() || window <= 0 {
		return 0, false
	}
	remainingTime := resetAt.Sub(now)
	if remainingTime <= 0 {
		return 0, false
	}
	remainingPct := 100 - usage
	if remainingPct <= 0 {
		// Already exhausted; hand it to the rate limit / automatic pause logic, pacing does not override it.
		return 0, false
	}
	sustainable := remainingPct / remainingTime.Seconds()
	natural := 100.0 / window.Seconds()
	if natural <= 0 {
		return 0, false
	}
	return sustainable / natural, true
}

// window7dDurationLocked returns the period length of the long window (7d slot) used for pacing: when the real length is known (team monthly window),
// it uses the real value, otherwise it falls back to the default of 7 days. The caller must hold a.mu.
func (a *Account) window7dDurationLocked() time.Duration {
	if a.Window7dSeconds > 0 {
		return time.Duration(a.Window7dSeconds) * time.Second
	}
	return smartPacingWindow7d
}

func normalizeAutoPause5hGuardBandPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeAutoPause5hGuardConcurrency(value int) int {
	if value < 0 {
		return 0
	}
	if value > 1000 {
		return 1000
	}
	return value
}

func quotaAutoPausedByWindow(usage float64, valid bool, resetAt time.Time, threshold float64, disabled bool, now time.Time) bool {
	if disabled || threshold <= 0 || !valid {
		return false
	}
	if !resetAt.IsZero() && !now.Before(resetAt) {
		return false
	}
	return usage/100 >= threshold
}

func (a *Account) quotaAutoPause5hGuardConcurrencyLimitLocked(limit int64, now time.Time) int64 {
	if limit <= 1 || a.AutoPause5hDisabled || a.effectiveAutoPause5h <= 0 || !a.UsagePercent5hValid || a.autoPause5hGuardBandPercent <= 0 || a.autoPause5hGuardConcurrency <= 0 {
		return limit
	}
	// When credits cover the rate limit, the automatic pause does not apply (see quotaAutoPausedLocked), and the guardrail band yields as well.
	if a.creditSkipsUsageWindowLocked() {
		return limit
	}
	if !a.Reset5hAt.IsZero() && !now.Before(a.Reset5hAt) {
		return limit
	}

	remainingPercent := a.effectiveAutoPause5h*100 - a.UsagePercent5h
	if remainingPercent <= 0 {
		return 0
	}
	if remainingPercent <= a.autoPause5hGuardBandPercent && limit > int64(a.autoPause5hGuardConcurrency) {
		return int64(a.autoPause5hGuardConcurrency)
	}
	return limit
}

// smartPacingConcurrencyLimitLocked is smart pacing (issue #312): when the account, within a usage window,
// is "burning too fast" (the used share exceeds the share that an even spread over time would allow), it scales by the ratio of sustainable rate to natural rate
// the concurrency limit, so the remaining quota is used smoothly up to the moment the window resets, instead of hitting the 5h/7d rate limit early.
// It only intervenes when there is a valid usage signal and a reset time; for the 5h/7d windows it takes the stricter (smaller) ratio.
func (a *Account) smartPacingConcurrencyLimitLocked(limit int64, now time.Time) int64 {
	if !a.smartPacingEnabled || limit <= 1 {
		return limit
	}
	floor := int64(a.smartPacingMinConcurrency)
	if floor < 1 {
		floor = 1
	}
	if floor >= limit {
		return limit
	}

	ratio := 1.0
	if a.smartPacingWindows5h {
		if r, ok := smartPacingRatio(a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt, smartPacingWindow5h, now); ok && r < ratio {
			ratio = r
		}
	}
	if a.smartPacingWindows7d {
		// Compute the natural rate from the long window's real period (a monthly window for team), so a monthly window is not treated as 7 days and cause over-limiting.
		if r, ok := smartPacingRatio(a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt, a.window7dDurationLocked(), now); ok && r < ratio {
			ratio = r
		}
	}
	if ratio >= 1 {
		return limit
	}

	scaled := int64(math.Ceil(float64(limit) * ratio))
	if scaled < floor {
		scaled = floor
	}
	if scaled > limit {
		scaled = limit
	}
	return scaled
}

func (a *Account) quotaAutoPause5hGuardDispatchPenaltyLocked(now time.Time) float64 {
	if a.AutoPause5hDisabled || a.effectiveAutoPause5h <= 0 || !a.UsagePercent5hValid || a.autoPause5hGuardBandPercent <= 0 || a.autoPause5hGuardConcurrency <= 0 {
		return 0
	}
	if a.creditSkipsUsageWindowLocked() {
		return 0
	}
	if !a.Reset5hAt.IsZero() && !now.Before(a.Reset5hAt) {
		return 0
	}

	remainingPercent := a.effectiveAutoPause5h*100 - a.UsagePercent5h
	if remainingPercent <= 0 || remainingPercent > a.autoPause5hGuardBandPercent {
		return 0
	}
	progress := (a.autoPause5hGuardBandPercent - remainingPercent) / a.autoPause5hGuardBandPercent
	return progress * maxAutoPause5hGuardDispatchPenalty
}

func (a *Account) quotaAutoPausedLocked(now time.Time) bool {
	// When the credits-cover-rate-limit switch is on and there are credits right now, the automatic pause yields: the threshold tops out at 100%, and the threshold still applies as usual, so
	// if it kept stopping scheduling, the window would never reach the "once full, deduct credits" step, and the account-level switch would be meaningless (issue #791).
	// After credits reach zero or the workspace hard-stops, creditSkipsUsageWindowLocked turns false and the threshold applies again.
	if a.creditSkipsUsageWindowLocked() {
		return false
	}
	if quotaAutoPausedByWindow(a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt, a.effectiveAutoPause5h, a.AutoPause5hDisabled, now) {
		return true
	}
	return quotaAutoPausedByWindow(a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt, a.effectiveAutoPause7d, a.AutoPause7dDisabled, now)
}

func (a *Account) recomputeEffectiveAutoPause(s *Store) {
	a.effectiveAutoPause5h = resolveEffectiveThreshold(a.AutoPause5hThreshold, a.GroupIDs, s, true)
	a.effectiveAutoPause7d = resolveEffectiveThreshold(a.AutoPause7dThreshold, a.GroupIDs, s, false)
	if s != nil {
		a.autoPause5hGuardBandPercent = s.GetAutoPause5hGuardBandPercent()
		a.autoPause5hGuardConcurrency = s.GetAutoPause5hGuardConcurrency()
		a.smartPacingEnabled = s.GetSmartPacingEnabled()
		a.smartPacingMinConcurrency = s.GetSmartPacingMinConcurrency()
		a.smartPacingWindows5h, a.smartPacingWindows7d = parseSmartPacingWindows(s.GetSmartPacingWindows())
	} else {
		a.autoPause5hGuardBandPercent = defaultAutoPause5hGuardBandPercent
		a.autoPause5hGuardConcurrency = defaultAutoPause5hGuardConcurrency
		a.smartPacingEnabled = false
		a.smartPacingMinConcurrency = defaultSmartPacingMinConcurrency
		a.smartPacingWindows5h = true
		a.smartPacingWindows7d = true
	}
}

func resolveEffectiveThreshold(accountThreshold float64, groupIDs []int64, s *Store, is5h bool) float64 {
	if accountThreshold > 0 {
		return accountThreshold
	}
	if s == nil {
		return 0
	}
	var best float64
	for _, gid := range groupIDs {
		t5h, t7d := s.getGroupAutoPauseThresholds(gid)
		var t float64
		if is5h {
			t = t5h
		} else {
			t = t7d
		}
		if t > 0 && (best == 0 || t < best) {
			best = t
		}
	}
	if best > 0 {
		return best
	}
	if is5h {
		return s.GetGlobalAutoPause5hThreshold()
	}
	return s.GetGlobalAutoPause7dThreshold()
}

func (a *Account) recomputeEffectiveGroupBaseConcurrency(s *Store) {
	a.groupBaseConcurrency = resolveGroupBaseConcurrency(a.GroupIDs, s)
}

func resolveGroupBaseConcurrency(groupIDs []int64, s *Store) int64 {
	if s == nil {
		return 0
	}
	var best int64
	for _, groupID := range groupIDs {
		value, ok := s.getGroupBaseConcurrencyOverride(groupID)
		if ok && value > 0 && (best == 0 || value < best) {
			best = value
		}
	}
	return best
}

// creditsBalanceValue parses the credits balance string returned by wham/usage (like "1000.0000000000").
// If it cannot be parsed, it is treated as 0: an unreadable balance counts as none, and it is not used to let scheduling through.
func creditsBalanceValue(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return v
}

// isWorkspaceCreditHardStop reports whether rate_limit_reached_type is a workspace-level forced stop signal.
// In the official Codex client:
// - workspace_owner_credits_depleted
// - workspace_member_credits_depleted
// - workspace_owner_usage_limit_reached
// - workspace_member_usage_limit_reached
// All of these are authoritative workspace-level hard stops for quota exhaustion or reaching the limit, and credits cannot cover them.
func isWorkspaceCreditHardStop(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "workspace_owner_credits_depleted",
		"workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached",
		"workspace_member_usage_limit_reached":
		return true
	default:
		return false
	}
}

// creditsAvailableLocked reports whether the account currently has credits it can spend (requires holding the mu read lock).
//
// CreditsValid=false means the wham probe has not run yet and the balance is unknown; here it is treated as "no credits":
// it is better to wait out one usage window than to send requests to an account that is bound to get a 429.
//
// Evaluation order:
// 1. not probed, or marked invalid -> false
// 2. workspace hard-stop (spend_control.reached=true or rate_limit_reached_type exhausted/exceeded) -> false
// 3. upstream overage_limit_reached (kept for compatibility with older upstreams) -> false
// 4. unlimited=false and the balance is known to be 0 -> false (the response that just spent the last credit)
// 5. unlimited=true or has_credits=true -> true (the official Codex explicitly allows has_credits=true with the balance hidden)
func (a *Account) creditsAvailableLocked() bool {
	if !a.CreditsValid {
		return false
	}
	if a.CreditsSpendControlValid && a.CreditsSpendControlReached {
		return false
	}
	if isWorkspaceCreditHardStop(a.CreditsRateLimitReachedType) {
		return false
	}
	if a.CreditsOverageLimitReached {
		return false
	}
	if a.CreditsUnlimited {
		return true
	}
	if a.CreditsBalanceKnown && creditsBalanceValue(a.CreditsBalance) <= 0 {
		return false
	}
	return a.CreditsHasCredits
}

// creditSkipsUsageWindowLocked reports whether credits cover the usage window rate limit.
//
// The two switches only "authorize covering with credits"; actually letting requests through also requires that there are credits to spend right now: Codex's behavior is
// that once the plan quota is used up, it switches to consuming credits; when credits reach zero it should go back to a real rate limit, otherwise scheduling would keep sending requests
// to an account that is certain to return 429.
func (a *Account) creditSkipsUsageWindowLocked() bool {
	if !(a.CreditEnabled && a.CreditSkipUsageWindow) {
		return false
	}
	return a.creditsAvailableLocked()
}

// UsingCredits reports whether the account is currently using credits to cover the usage window rate limit.
//
// This is an independent signal alongside RuntimeStatus, not a status value: the account's status at this moment is still active
// (it really is schedulable). This flag only explains "why it is still available although the window is full". The frontend uses it to place
// a "using credits" badge next to the status badge.
func (a *Account) UsingCredits() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usingCreditsLocked(time.Now())
}

// usingCreditsLocked reports whether the account is in the "credits cover rate limit" state: the usage window itself is full
// (without credits it would now be rate_limited / usage_exhausted), but since credits are available it is still being scheduled.
//
// An upstream-driven cooldown (a real 429) and error take precedence: a real rejection means the credits did not cover it,
// at that point the status is already rate limited / error, and it should not claim to be using credits.
func (a *Account) usingCreditsLocked(now time.Time) bool {
	if !(a.CreditEnabled && a.CreditSkipUsageWindow) || !a.creditsAvailableLocked() {
		return false
	}
	if a.Status == StatusError {
		return false
	}
	// An upstream-driven cooldown (a real 429) takes precedence: a real rejection means the credits did not cover it. But a penalty from the local usage window
	// that produces a cooldown does not count, because that is exactly what credits are meant to cover; a blanket rule would make the badge never appear in the most common case
	// (where the account is discovered to be rate limited and only then the switch is turned on).
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && !a.usageWindowCooldownLocked() {
		return false
	}
	// All three usage window rate-limit paths count: free 7d exhausted, premium 5h full, and the 7d full that applies to all plans
	// (the MarkUsage7dRateLimited one). If any one were missed, that path would show as a plain active.
	return a.rawUsageExhaustedLocked() ||
		a.rawPremium5hRateLimitedLocked(now) ||
		a.rawUsageWindow7dExhaustedLocked(now)
}

// rawUsageExhaustedLocked is the Free 7d usage exhaustion check that ignores any skip switches.
func (a *Account) rawUsageExhaustedLocked() bool {
	return a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") && a.UsagePercent7d >= 100
}

// usageWindowCooldownLocked reports whether the current cooldown was produced by a local usage window penalty rather than an upstream 429.
//
// Both share the "rate_limited" reason, so the reason alone cannot tell them apart. The reliable distinguishing point is the penalty duration:
// MarkUsage7dRateLimited sets CooldownUtil directly to Reset7dAt, while the cooldown duration of an upstream 429
// comes from Retry-After / the rate-limit decision and does not land exactly on the 7d reset moment. A 2s tolerance absorbs computation jitter.
//
// A premium 5h penalty (rate_limited_5h) is only set by response header / WHAM observation; a real 429 from /responses
// records a 5h cooldown with ResponsesRateLimitedCooldownReason, so the reason alone identifies a local penalty.
// If it is missed, Plus/Pro hit the 5h limit first and then turn on the credits switch, and they would have to wait for the 5h reset (issue #791).
func (a *Account) usageWindowCooldownLocked() bool {
	if a.Status != StatusCooldown {
		return false
	}
	switch a.CooldownReason {
	case premium5hCooldownReason:
		return true
	case "rate_limited", "rate_limited_7d", "usage_limited", "usage_limit":
	default:
		return false
	}
	if a.Reset7dAt.IsZero() {
		return false
	}
	drift := a.CooldownUtil.Sub(a.Reset7dAt)
	return drift > -2*time.Second && drift < 2*time.Second
}

// UsageWindowCooldown reports whether the current cooldown was produced by a local usage window penalty.
func (a *Account) UsageWindowCooldown() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usageWindowCooldownLocked()
}

// rawUsageWindow7dExhaustedLocked reports whether the 7d window is full enough to be penalized by MarkUsage7dRateLimited
// (matching the condition there: once the reset time has passed, no penalty is applied).
func (a *Account) rawUsageWindow7dExhaustedLocked(now time.Time) bool {
	if !a.UsagePercent7dValid || a.UsagePercent7d < 100 {
		return false
	}
	return a.Reset7dAt.IsZero() || a.Reset7dAt.After(now)
}

// usageWindowBlocksFreshDispatchLocked keeps WHAM-reported usage limits out
// of the fresh-request pool. IgnoreUsageLimitStatus is intentionally not
// applied here: it is a continuation escape hatch, not permission to start a
// new turn on an account whose usage window is already full.
func (a *Account) usageWindowBlocksFreshDispatchLocked(now time.Time) bool {
	if a.creditSkipsUsageWindowLocked() {
		return false
	}
	return a.rawUsageExhaustedLocked() ||
		a.rawPremium5hRateLimitedLocked(now) ||
		a.rawUsageWindow7dExhaustedLocked(now)
}

func (a *Account) usageLimitContinuationEligibleLocked(now time.Time) bool {
	if !a.ignoreUsageLimitStatus {
		return false
	}
	if atomic.LoadInt32(&a.Disabled) != 0 || (atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0) {
		return false
	}
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned ||
		!a.usageWindowBlocksFreshDispatchLocked(now) {
		return false
	}

	// A local WHAM/window observation may be stale relative to an in-flight
	// Responses turn. A real upstream 429 is authoritative evidence that a new
	// request is rejected, so it must not be bypassed.
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		if a.usageWindowCooldownLocked() {
			return true
		}
		return false
	}
	return true
}

// UsageLimitContinuationEligible reports whether a caller that has already
// established an authoritative Codex turn may retain this account despite a
// WHAM-reported usage window limit. Callers must separately prove turn
// continuity; this method deliberately rejects actual upstream cooldowns.
func (a *Account) UsageLimitContinuationEligible() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usageLimitContinuationEligibleLocked(time.Now())
}

// FreshDispatchUsageLimited reports whether this otherwise dispatchable
// account is blocked specifically by a usage-limit observation. It lets API
// handlers return 429 instead of misreporting an exhausted pool as 503.
func (a *Account) FreshDispatchUsageLimited() bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 || (atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0) {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || !a.hasDispatchCredentialLocked() {
		return false
	}
	if a.usageWindowBlocksFreshDispatchLocked(now) {
		return true
	}
	return a.Status == StatusCooldown && now.Before(a.CooldownUtil) && isUsageLimitCooldownReason(a.CooldownReason)
}

// QuotaAutoPaused reports whether this otherwise dispatchable account is
// fenced only by its configured 5h/7d auto-pause threshold. The decision uses
// valid usage windows, thresholds and reset times alone. Manually disabled or
// paused accounts, accounts without credentials, errors, bans and plain
// concurrency saturation are not quota pauses. Unlike a local 100% WHAM
// observation, the threshold fence has no continuation exception: every
// scheduler path rejects the account until its window resets.
func (a *Account) QuotaAutoPaused() bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || !a.hasDispatchCredentialLocked() {
		return false
	}
	return a.quotaAutoPausedLocked(time.Now())
}

func (a *Account) recomputeEffectiveIgnoreUsageLimitStatus(global bool) {
	if a.IgnoreUsageLimitStatusOverride != nil {
		a.ignoreUsageLimitStatus = *a.IgnoreUsageLimitStatusOverride
		return
	}
	a.ignoreUsageLimitStatus = global
}

func (a *Account) skipsUsageWindowLimitsLocked() bool {
	return a.creditSkipsUsageWindowLocked() || a.ignoreUsageLimitStatus
}

// IgnoresUsageLimitStatus reports whether usage-window percentages are
// informational for this account and Responses outcomes decide availability.
func (a *Account) IgnoresUsageLimitStatus() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ignoreUsageLimitStatus
}

// GetIgnoreUsageLimitStatusOverride returns the account override. nil means
// the account follows the global setting.
func (a *Account) GetIgnoreUsageLimitStatusOverride() *bool {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.IgnoreUsageLimitStatusOverride == nil {
		return nil
	}
	value := *a.IgnoreUsageLimitStatusOverride
	return &value
}

// SkipsUsageWindowLimits reports whether the account should skip the local rate limits triggered by the 5h/7d usage windows.
func (a *Account) SkipsUsageWindowLimits() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skipsUsageWindowLimitsLocked()
}

// usageExhaustedLocked reports whether the Free account's 7d usage is exhausted (requires holding the mu read lock)
func (a *Account) usageExhaustedLocked() bool {
	if a.skipsUsageWindowLimitsLocked() {
		return false
	}
	return a.rawUsageExhaustedLocked()
}

// NeedsRefresh checks whether the AT needs refreshing (refresh 5 minutes before expiry)
func (a *Account) NeedsRefresh() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Until(a.ExpiresAt) < 5*time.Minute
}

// SetCooldown sets the cooldown time
func (a *Account) SetCooldown(duration time.Duration) {
	a.SetCooldownUntil(time.Now().Add(duration), "")
}

// SetCooldownWithReason sets the cooldown time (with a reason)
func (a *Account) SetCooldownWithReason(duration time.Duration, reason string) {
	a.SetCooldownUntil(time.Now().Add(duration), reason)
}

// SetCooldownUntil sets the cooldown end time (with a reason)
func (a *Account) SetCooldownUntil(until time.Time, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setCooldownUntilLocked(until, reason)
}

func (a *Account) setCooldownUntilLocked(until time.Time, reason string) {
	a.transientRateLimitUntil = time.Time{}
	if a.transientRateLimitTimer != nil {
		a.transientRateLimitTimer.Stop()
		a.transientRateLimitTimer = nil
	}
	a.Status = StatusCooldown
	a.CooldownUtil = until
	a.CooldownReason = reason
	switch reason {
	case "unauthorized":
		a.HealthTier = HealthTierBanned
	case "rate_limited_5h", ResponsesRateLimitedCooldownReason:
		if a.HealthTier != HealthTierBanned {
			a.HealthTier = HealthTierRisky
		}
	case "rate_limited", "rate_limited_7d", "usage_limited", "usage_limit":
		if a.healthTierLocked() == HealthTierHealthy {
			a.HealthTier = HealthTierWarm
		} else {
			a.HealthTier = HealthTierRisky
		}
	default:
		if a.HealthTier == "" {
			a.HealthTier = HealthTierWarm
		}
	}
}

// GetCooldownReason gets the cooldown reason
func (a *Account) GetCooldownReason() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason
}

func (a *Account) GetCooldownSnapshot() (string, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason, a.CooldownUtil
}

// HasActiveCooldown checks whether the account is still in the cooldown period
func (a *Account) HasActiveCooldown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil)
}

// IsBanned checks whether the account is in strong isolation
func (a *Account) IsBanned() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.healthTierLocked() == HealthTierBanned
}

// ModelCatalogEligible reports only durable account gates used by /v1/models.
// It intentionally ignores active request count, short cooldowns and transient
// rate limits so a client model menu does not flicker under load.
func (a *Account) ModelCatalogEligible() bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned {
		return false
	}
	// Legacy Codex Free windows are an account availability gate.
	if a.usageExhaustedLocked() {
		return false
	}
	return a.hasDispatchCredentialLocked()
}

// RuntimeStatus returns the runtime status string (used by the admin API)
func (a *Account) RuntimeStatus() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.runtimeStatusLocked(time.Now())
}

// runtimeStatusLocked returns the public runtime status while the caller
// holds a.mu for reading or writing.
func (a *Account) runtimeStatusLocked(now time.Time) string {
	if a.healthTierLocked() == HealthTierBanned {
		return "unauthorized"
	}
	// Hard errors such as a deactivated workspace must take precedence over the usage window rate limit. Team/K12 seats often are first at
	// 5h/7d 100%, and only then are marked as errors by deactivated_workspace; if the rate limit badge took precedence,
	// the list would keep showing "rate limited", which looks like it would recover.
	if a.Status == StatusError {
		return "error"
	}
	// When credits cover the rate limit, the display still shows rate limited: the usage window really is full, and claiming active would make people
	// think the quota is not used up. The real difference is expressed by the UsingCredits flag alongside it: the frontend puts a credits badge after the rate limit badge,
	// while the scheduling side (IsAvailable / cooldown) uses the suppressed result, so the account still takes part in scheduling as usual.
	if a.usingCreditsLocked(now) {
		if a.rawUsageExhaustedLocked() {
			return "usage_exhausted"
		}
		return "rate_limited"
	}
	// The background status must match the scheduling semantics of new requests. ignore_usage_limit_status only allows
	// continuation of an active turn that is bound and carries turn-state, and it does not let new turns bypass WHAM 100%;
	// so this uses the same fresh-dispatch check, to avoid an account showing active but being unable to be scheduled.
	if a.usageWindowBlocksFreshDispatchLocked(now) && a.rawUsageExhaustedLocked() {
		return "usage_exhausted"
	}
	if a.usageWindowBlocksFreshDispatchLocked(now) {
		return "rate_limited"
	}
	switch a.Status {
	case StatusError:
		return "error"
	case StatusCooldown:
		if now.Before(a.CooldownUtil) {
			if a.CooldownReason != "" {
				return a.CooldownReason
			}
			return "cooldown"
		}
		if a.hasDispatchCredentialLocked() {
			if a.quotaAutoPausedLocked(now) {
				return "quota_paused"
			}
			return "active" // cooldown expired, recovered
		}
		if a.RefreshToken != "" {
			return "refreshing"
		}
		return "error"
	default:
		if a.hasDispatchCredentialLocked() && a.quotaAutoPausedLocked(now) {
			return "quota_paused"
		}
		if a.hasDispatchCredentialLocked() {
			return "active"
		}
		if a.RefreshToken != "" && a.ErrorMsg == "" {
			return "refreshing"
		}
		return "error"
	}
}

// GetAccountListRuntimeSnapshot returns every runtime field needed by the
// cached admin list under a single read lock. It intentionally consumes the
// scheduler's maintained values rather than invoking recomputeSchedulerLocked
// for every account during a list-cache rebuild.
func (a *Account) GetAccountListRuntimeSnapshot() AccountListRuntimeSnapshot {
	if a == nil {
		return AccountListRuntimeSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	now := time.Now()
	return AccountListRuntimeSnapshot{
		Status:                  a.runtimeStatusLocked(now),
		UsingCredits:            a.usingCreditsLocked(now),
		GroupIDs:                cloneInt64Slice(a.GroupIDs),
		PlanType:                a.PlanType,
		UsagePercent5h:          a.UsagePercent5h,
		UsagePercent5hValid:     a.UsagePercent5hValid,
		UsagePercent7d:          a.UsagePercent7d,
		UsagePercent7dValid:     a.UsagePercent7dValid,
		UsagePercentSpark:       a.UsagePercentSpark,
		UsagePercentSparkValid:  a.UsagePercentSparkValid,
		ResetSparkAt:            a.ResetSparkAt,
		HealthTier:              string(a.HealthTier),
		DispatchScore:           a.DispatchScore,
		LatencyPenalty:          a.schedulerBreakdownLocked(now).LatencyPenalty,
		LastUnauthorizedAt:      a.LastUnauthorizedAt,
		LastRateLimitedAt:       a.LastRateLimitedAt,
		LastTimeoutAt:           a.LastTimeoutAt,
		ActiveRequests:          a.ActiveRequests.Load(),
		OccupiedRequests:        accountOccupiedRequests(a),
		DynamicConcurrencyLimit: a.DynamicConcurrencyLimit,
		Reset5hAt:               a.Reset5hAt,
		Reset7dAt:               a.Reset7dAt,
		Window7dSeconds:         a.Window7dSeconds,
		CooldownReason:          a.CooldownReason,
		CooldownUntil:           a.CooldownUtil,
	}
}

// SetUsagePercent7d updates the 7d usage percentage
func (a *Account) SetUsagePercent7d(pct float64) {
	a.SetUsageSnapshot(pct, time.Now())
}

// SetUsageSnapshot updates the usage snapshot and its time
func (a *Account) SetUsageSnapshot(pct float64, updatedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = pct
	a.UsagePercent7dValid = true
	a.UsageUpdatedAt = updatedAt
}

// GetUsagePercent7d gets the 7d usage percentage
func (a *Account) GetUsagePercent7d() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent7d, a.UsagePercent7dValid
}

// cooldownMatchesReset reports whether an existing cooldown already ends at
// the observed window reset. Repeated observations of the same exhausted
// window (several in-flight turns, or a WHAM probe while the cooldown is
// active) must not issue another DB write or inflate FailureStreak.
func cooldownMatchesReset(cooldownUntil, resetAt time.Time) bool {
	if resetAt.IsZero() {
		return true
	}
	diff := cooldownUntil.Sub(resetAt)
	if diff < 0 {
		diff = -diff
	}
	return diff <= time.Minute
}

// usageCooldownTargetLocked keeps passive quota observations from weakening an
// active cooldown. Preserve every existing reason rather than maintaining an
// incomplete list of authoritative errors. A real Responses 429 also upgrades a
// local usage cooldown so IgnoreUsageLimitStatus cannot bypass that rejection.
// Authentication failures retain the higher priority used by the runtime cache.
// The caller must hold a.mu.
func (a *Account) usageCooldownTargetLocked(now, resetAt time.Time, reason string) (time.Time, string, bool) {
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		if reason == ResponsesRateLimitedCooldownReason && a.usageWindowCooldownLocked() {
			if resetAt.Before(a.CooldownUtil) {
				resetAt = a.CooldownUtil
			}
			return resetAt, reason, true
		}
		if !resetAt.After(a.CooldownUtil) ||
			(a.CooldownReason == reason && cooldownMatchesReset(a.CooldownUtil, resetAt)) {
			return a.CooldownUtil, a.CooldownReason, false
		}
		if reason != ResponsesRateLimitedCooldownReason || a.CooldownReason == "unauthorized" {
			reason = a.CooldownReason
		}
	}
	return resetAt, reason, true
}

// MarkUsage7dRateLimited marks an account as rate-limited when its active 7d
// usage window is exhausted. A future reset time is preferred; missing reset
// metadata falls back to a full 7d cooldown, while stale reset times are ignored.
func (s *Store) MarkUsage7dRateLimited(acc *Account) bool {
	return s.markUsage7dRateLimited(acc, true)
}

func (s *Store) markUsage7dRateLimited(acc *Account, persist bool) bool {
	if s == nil || acc == nil {
		return false
	}
	acc.mu.Lock()
	if acc.healthTierLocked() == HealthTierBanned || acc.skipsUsageWindowLimitsLocked() {
		acc.mu.Unlock()
		return false
	}

	if !acc.UsagePercent7dValid || acc.UsagePercent7d < 100 {
		acc.mu.Unlock()
		return false
	}

	now := time.Now()
	resetAt := acc.Reset7dAt
	if resetAt.IsZero() {
		resetAt = now.Add(7 * 24 * time.Hour)
	} else if !resetAt.After(now) {
		acc.mu.Unlock()
		return false
	}

	activeCooldown := acc.Status == StatusCooldown && now.Before(acc.CooldownUtil)
	// With no reset metadata, an active cooldown already fences this account.
	// Do not slide the fallback window forward on every observation.
	if activeCooldown && acc.Reset7dAt.IsZero() {
		acc.mu.Unlock()
		if persist {
			s.persistUsageCooldown(acc)
		}
		return true
	}
	until, reason, changed := acc.usageCooldownTargetLocked(now, resetAt, "rate_limited")
	if !changed {
		acc.mu.Unlock()
		if persist {
			s.persistUsageCooldown(acc)
		}
		return true
	}

	if activeCooldown {
		acc.CooldownUtil = until
	} else {
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		acc.setCooldownUntilLocked(until, reason)
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.usageCooldownPersistPending = true
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	if persist {
		s.persistUsageCooldown(acc)
	}
	return true
}

// SetUsageSnapshot5h updates the 5h usage snapshot
func (a *Account) SetUsageSnapshot5h(pct float64, resetAt time.Time) {
	a.SetUsageSnapshot5hAt(pct, resetAt, time.Now())
}

// SetUsageSnapshot5hAt updates the 5h usage snapshot and its refresh time
func (a *Account) SetUsageSnapshot5hAt(pct float64, resetAt time.Time, updatedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if updatedAt.After(a.usageObservedAt) {
		a.usageObservedAt = updatedAt
	}
	a.UsagePercent5h = pct
	a.UsagePercent5hValid = true
	a.Reset5hAt = resetAt
	a.UsageUpdatedAt5h = updatedAt
}

// ApplyUsageObservation serializes one authoritative upstream usage observation,
// including its database writes. The timestamp check prevents an older observer
// that waited on the lock from overwriting a newer 5h presence/absence decision.
func (a *Account) ApplyUsageObservation(observedAt time.Time, apply func()) bool {
	if a == nil || apply == nil {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	a.usageSyncMu.Lock()
	defer a.usageSyncMu.Unlock()

	a.mu.Lock()
	if observedAt.Before(a.usageObservedAt) {
		a.mu.Unlock()
		return false
	}
	a.usageObservedAt = observedAt
	a.mu.Unlock()

	apply()
	return true
}

// TryApplyUsageObservation is the non-blocking variant used by connection-level
// WS frame ingress. A false result lets ApplyUsageFrameObservation queue the
// observation instead of blocking the WebSocket read pump.
func (a *Account) TryApplyUsageObservation(observedAt time.Time, apply func()) bool {
	if a == nil || apply == nil {
		return false
	}
	if !a.usageSyncMu.TryLock() {
		return false
	}
	defer a.usageSyncMu.Unlock()
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	a.mu.Lock()
	if observedAt.Before(a.usageObservedAt) {
		a.mu.Unlock()
		return false
	}
	a.usageObservedAt = observedAt
	a.mu.Unlock()

	apply()
	return true
}

// GetUsagePercent5h gets the 5h usage percentage
func (a *Account) GetUsagePercent5h() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent5h, a.UsagePercent5hValid
}

// UsageFramePersistInterval is the default maximum cadence for persisting WS
// frame observations. In-memory usage is updated on every frame; persistence
// exists only so a restart does not begin from a badly stale snapshot.
const UsageFramePersistInterval = 30 * time.Second

// usageFramePersistMinDelta forces a write when the reported percentage has
// moved this much since the last persisted observation, so a burst cannot hide
// behind the interval. Threshold crossings and 100% always force a write.
const usageFramePersistMinDelta = 5.0

// ShouldPersistUsageFrame decides whether a WS usage-frame observation needs a
// database write. The caller always applies the in-memory snapshot first; this
// only throttles persistence for large pools. The throttle state advances only
// via MarkUsageFramePersisted after a successful write, so a failed write is
// retried on the next frame instead of being suppressed for the interval.
func (a *Account) ShouldPersistUsageFrame(now time.Time, pct5h float64, has5h bool, pct7d float64, has7d bool, interval time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	if interval <= 0 {
		interval = UsageFramePersistInterval
	}
	force := false
	if has5h {
		if pct5h >= 100 && a.usageFramePersisted5h < 100 {
			force = true
		}
		if a.effectiveAutoPause5h > 0 {
			threshold := a.effectiveAutoPause5h * 100
			if pct5h >= threshold && a.usageFramePersisted5h < threshold {
				force = true
			}
		}
		if math.Abs(pct5h-a.usageFramePersisted5h) >= usageFramePersistMinDelta {
			force = true
		}
	}
	if has7d {
		if pct7d >= 100 && a.usageFramePersisted7d < 100 {
			force = true
		}
		if a.effectiveAutoPause7d > 0 {
			threshold := a.effectiveAutoPause7d * 100
			if pct7d >= threshold && a.usageFramePersisted7d < threshold {
				force = true
			}
		}
		if math.Abs(pct7d-a.usageFramePersisted7d) >= usageFramePersistMinDelta {
			force = true
		}
	}
	if !force && !a.usageFramePersistedAt.IsZero() && now.Sub(a.usageFramePersistedAt) < interval {
		return false
	}
	return true
}

// MarkUsageFramePersisted records a successful frame snapshot write.
func (a *Account) MarkUsageFramePersisted(now time.Time, pct5h float64, has5h bool, pct7d float64, has7d bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageFramePersistedAt = now
	if has5h {
		a.usageFramePersisted5h = pct5h
	}
	if has7d {
		a.usageFramePersisted7d = pct7d
	}
}

// MarkUsageFrameObserved records the last WS quota frame ingested for the
// account. The admin live projection exposes it so operators can tell
// frame-driven updates apart from WHAM probes; it does not affect scheduling.
func (a *Account) MarkUsageFrameObserved(kind string, at time.Time) {
	if a == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if at.Before(a.usageFrameLastAt) {
		return
	}
	a.usageFrameLastAt = at
	a.usageFrameLastKind = strings.TrimSpace(kind)
}

// AccountLiveUsageSnapshot is a single-lock projection for the admin live poll.
// It intentionally avoids GetSchedulerDebugSnapshot and other recomputing
// getters, which are too expensive for a one-second cadence over a large page.
type AccountLiveUsageSnapshot struct {
	Status              string
	UsingCredits        bool
	PlanType            string
	CreditsValid        bool
	Credits             CreditBalanceSnapshot
	UsagePercent5h      float64
	UsagePercent5hValid bool
	Reset5hAt           time.Time
	UsageUpdatedAt5h    time.Time
	UsagePercent7d      float64
	UsagePercent7dValid bool
	Reset7dAt           time.Time
	UsageUpdatedAt      time.Time
	// UsageFrameLastAt/Kind record the last WS quota frame ingested for this
	// account, as opposed to snapshots that only advance on WHAM probes.
	UsageFrameLastAt   time.Time
	UsageFrameLastKind string
}

// GetAccountLiveUsageSnapshot returns the runtime usage/status projection used
// by the account list's live poll.
func (a *Account) GetAccountLiveUsageSnapshot() AccountLiveUsageSnapshot {
	if a == nil {
		return AccountLiveUsageSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	return AccountLiveUsageSnapshot{
		Status:              a.runtimeStatusLocked(now),
		UsingCredits:        a.usingCreditsLocked(now),
		PlanType:            a.PlanType,
		CreditsValid:        a.CreditsValid,
		Credits:             a.creditSnapshotLocked(),
		UsagePercent5h:      a.UsagePercent5h,
		UsagePercent5hValid: a.UsagePercent5hValid,
		Reset5hAt:           a.Reset5hAt,
		UsageUpdatedAt5h:    a.UsageUpdatedAt5h,
		UsagePercent7d:      a.UsagePercent7d,
		UsagePercent7dValid: a.UsagePercent7dValid,
		Reset7dAt:           a.Reset7dAt,
		UsageUpdatedAt:      a.UsageUpdatedAt,
		UsageFrameLastAt:    a.usageFrameLastAt,
		UsageFrameLastKind:  a.usageFrameLastKind,
	}
}

// Mark5hWindowActivated records which Reset5hAt the window-open request has already been sent for.
func (a *Account) Mark5hWindowActivated(resetAt time.Time) {
	if a == nil || resetAt.IsZero() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activated5hResetAt = resetAt
}

// GetActivated5hResetAt returns the Reset5hAt matching the most recent 5h window-open request.
func (a *Account) GetActivated5hResetAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.activated5hResetAt
}

// ShouldActivate5hWindow reports whether the account needs one real minimal /responses request to start the next 5h window.
// It only trusts the 5h and reset times observed upstream, not hard-coded per plan; at most one per Reset5hAt.
func (a *Account) ShouldActivate5hWindow(now time.Time) bool {
	if a == nil {
		return false
	}
	if atomic.LoadInt32(&a.Disabled) != 0 || (atomic.LoadInt32(&a.DispatchPaused) != 0 || atomic.LoadInt32(&a.IntelligencePaused) != 0) {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" && !a.isCodexAgentIdentityLocked() {
		return false
	}
	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return false
	}
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	if a.rawUsageExhaustedLocked() || a.rawUsageWindow7dExhaustedLocked(now) {
		return false
	}
	if !a.UsagePercent5hValid || a.Reset5hAt.IsZero() || a.Reset5hAt.After(now) {
		return false
	}
	if !a.activated5hResetAt.IsZero() && a.activated5hResetAt.Unix() == a.Reset5hAt.Unix() {
		return false
	}
	return true
}

// SetRateLimitResetCredits records the account's remaining active reset count.
func (a *Account) SetRateLimitResetCredits(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if count < 0 {
		count = 0
	}
	a.RateLimitResetCredits = count
	a.RateLimitResetCreditsValid = true
}

// GetRateLimitResetCredits returns the account's remaining active reset count and whether it has been probed.
func (a *Account) GetRateLimitResetCredits() (int, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RateLimitResetCredits, a.RateLimitResetCreditsValid
}

// SetApplicableResetCredits records the number of "applicable" reset credits right now (0 when not rate limited).
func (a *Account) SetApplicableResetCredits(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if count < 0 {
		count = 0
	}
	a.ApplicableResetCredits = count
	a.ApplicableResetCreditsValid = true
}

// GetApplicableResetCredits returns the number of applicable reset credits right now and whether it has been probed.
func (a *Account) GetApplicableResetCredits() (int, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ApplicableResetCredits, a.ApplicableResetCreditsValid
}

// SetCreditBalanceDetails records the full credits balance and quota status returned by wham/usage.
// wham is the authoritative full snapshot: when spend_control / rate_limit_reached_type is absent, it is treated as not reached,
// and the old values are cleared too, otherwise the hard-stop flag would stick after the workspace is topped up.
func (a *Account) SetCreditBalanceDetails(balance *string, hasCredits, unlimited, overageReached bool,
	spendControlReached *bool, rateLimitReachedType string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setCreditBalanceLocked(balance)
	a.CreditsHasCredits = hasCredits
	a.CreditsUnlimited = unlimited
	a.CreditsOverageLimitReached = overageReached
	a.CreditsSpendControlValid = spendControlReached != nil
	a.CreditsSpendControlReached = spendControlReached != nil && *spendControlReached
	a.CreditsRateLimitReachedType = strings.TrimSpace(rateLimitReachedType)
	a.CreditsValid = true
}

// setCreditBalanceLocked writes the balance; nil or an empty string both mean the upstream hid the balance (Team members),
// and it is recorded as "unknown" rather than "0", otherwise it would be treated as exhausted.
func (a *Account) setCreditBalanceLocked(balance *string) {
	if balance != nil {
		if trimmed := strings.TrimSpace(*balance); trimmed != "" {
			a.CreditsBalance = trimmed
			a.CreditsBalanceKnown = true
			return
		}
	}
	a.CreditsBalance = ""
	a.CreditsBalanceKnown = false
}

// SetCreditBalance records the credits balance snapshot returned by wham/usage (backward-compatible interface).
func (a *Account) SetCreditBalance(balance string, hasCredits, unlimited, overageReached bool) {
	a.SetCreditBalanceDetails(&balance, hasCredits, unlimited, overageReached, nil, "")
}

// SetRateLimitReachedType updates the account's rate_limit_reached_type.
// This is a per-response field: an empty string means this response did not reach the limit, so the old value must be cleared.
func (a *Account) SetRateLimitReachedType(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CreditsRateLimitReachedType = strings.TrimSpace(kind)
}

// GetRateLimitReachedType returns the last workspace hard-stop marker. WS
// frames do not carry the per-response field, so the frame path preserves this
// value until WHAM or an explicit header observation clears it.
func (a *Account) GetRateLimitReachedType() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CreditsRateLimitReachedType
}

// ApplySparseCreditsHeaders applies the sparse credits observation carried in normal Codex response headers.
// When the balance / overage headers are absent, the old values are kept; when rate_limit_reached_type is absent it means "not reached" and must overwrite.
func (a *Account) ApplySparseCreditsHeaders(hasCredits, unlimited bool, balance *string, overageReached *bool, rateLimitReachedType string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CreditsValid = true
	a.CreditsHasCredits = hasCredits
	a.CreditsUnlimited = unlimited
	if balance != nil {
		a.setCreditBalanceLocked(balance)
	}
	if overageReached != nil {
		a.CreditsOverageLimitReached = *overageReached
	}
	a.CreditsRateLimitReachedType = strings.TrimSpace(rateLimitReachedType)
}

// GetCreditBalance returns the credits snapshot and whether it has been probed.
// A nil Balance in the snapshot means the upstream hid the balance, and a nil SpendControlReached means it was not observed.
func (a *Account) GetCreditBalance() (CreditBalanceSnapshot, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.creditSnapshotLocked(), a.CreditsValid
}

// creditSnapshotLocked projects the in-memory fields into a snapshot (requires holding mu); persistence and external display both use it.
func (a *Account) creditSnapshotLocked() CreditBalanceSnapshot {
	snap := CreditBalanceSnapshot{
		HasCredits:           a.CreditsHasCredits,
		Unlimited:            a.CreditsUnlimited,
		OverageLimitReached:  a.CreditsOverageLimitReached,
		RateLimitReachedType: a.CreditsRateLimitReachedType,
	}
	if a.CreditsBalanceKnown {
		balance := a.CreditsBalance
		snap.Balance = &balance
	}
	if a.CreditsSpendControlValid {
		reached := a.CreditsSpendControlReached
		snap.SpendControlReached = &reached
	}
	return snap
}

// CreditBalanceSnapshot is the credits snapshot persisted to credentials["codex_credits"].
// Without it, CreditsValid resets to zero after a restart, the account is treated as having no credits, and credits covering the rate limit stops working,
// and it gets wrongly deleted by "clean up rate-limited accounts", until the next wham probe lands and restores it.
type CreditBalanceSnapshot struct {
	Balance              *string   `json:"balance"`
	HasCredits           bool      `json:"has_credits"`
	Unlimited            bool      `json:"unlimited"`
	OverageLimitReached  bool      `json:"overage_limit_reached"`
	SpendControlReached  *bool     `json:"spend_control_reached,omitempty"`
	RateLimitReachedType string    `json:"rate_limit_reached_type,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// creditBalanceKey is the comparison fingerprint of the snapshot (without timestamps).
func creditBalanceKey(snap CreditBalanceSnapshot) string {
	balStr := "<nil>"
	if snap.Balance != nil {
		balStr = *snap.Balance
	}
	return balStr + "|" + creditFlagsKey(snap)
}

// creditFlagsKey is the fingerprint with the balance removed. The normal response-header path uses only it to decide whether to persist:
// the balance decreases on every request, and writing it each time would add row-lock transactions to the time to first token; the balance itself is refreshed periodically by the wham probe.
func creditFlagsKey(snap CreditBalanceSnapshot) string {
	scStr := "<nil>"
	if snap.SpendControlReached != nil {
		scStr = strconv.FormatBool(*snap.SpendControlReached)
	}
	return fmt.Sprintf("%t|%t|%t|%s|%s", snap.HasCredits, snap.Unlimited, snap.OverageLimitReached, scStr, snap.RateLimitReachedType)
}

// MarshalCreditBalanceSnapshot serializes the credits snapshot for persistence.
func MarshalCreditBalanceSnapshot(snap CreditBalanceSnapshot) (string, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// RestoreCreditBalanceFromJSON backfills the account from the credits snapshot in the database (restored after a restart).
// An empty string, a parse failure, or never probed (all empty with no valid markers) are all treated as no snapshot, and it returns false,
// keeping the "unknown" meaning of CreditsValid=false, and it never invents a usable balance out of thin air.
func (a *Account) RestoreCreditBalanceFromJSON(raw string) bool {
	if a == nil {
		return false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var snap CreditBalanceSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return false
	}
	// In older snapshots the balance is a plain string; a hidden balance is written as "", which means the same as null.
	if snap.Balance != nil && strings.TrimSpace(*snap.Balance) == "" {
		snap.Balance = nil
	}
	if snap.Balance == nil && !snap.HasCredits && !snap.Unlimited && !snap.OverageLimitReached && snap.SpendControlReached == nil && snap.RateLimitReachedType == "" {
		return false
	}
	a.mu.Lock()
	a.setCreditBalanceLocked(snap.Balance)
	a.CreditsHasCredits = snap.HasCredits
	a.CreditsUnlimited = snap.Unlimited
	a.CreditsOverageLimitReached = snap.OverageLimitReached
	a.CreditsSpendControlValid = snap.SpendControlReached != nil
	a.CreditsSpendControlReached = snap.SpendControlReached != nil && *snap.SpendControlReached
	a.CreditsRateLimitReachedType = strings.TrimSpace(snap.RateLimitReachedType)
	a.CreditsValid = true
	// The restored values come from the database, so the fingerprints are aligned too, to avoid the first probe after startup writing the same content again.
	restored := a.creditSnapshotLocked()
	a.creditsPersistedKey = creditBalanceKey(restored)
	a.creditsPersistedFlagsKey = creditFlagsKey(restored)
	a.mu.Unlock()
	return true
}

// PersistCreditBalance writes the wham credits snapshot and persists it (skips the write when the values have not changed).
// When store is nil (unit tests / no database), only memory is updated, and the behavior matches SetCreditBalanceDetails.
func (s *Store) PersistCreditBalance(acc *Account, balance *string, hasCredits, unlimited, overageReached bool,
	spendControlReached *bool, rateLimitReachedType string) {
	if acc == nil {
		return
	}
	acc.SetCreditBalanceDetails(balance, hasCredits, unlimited, overageReached, spendControlReached, rateLimitReachedType)
	s.persistCreditSnapshot(acc, false)
}

// PersistSparseCreditObservation writes the sparse credits observation from response headers; only when the business flag
// changes is it persisted; balance changes are left to the wham probe to refresh.
func (s *Store) PersistSparseCreditObservation(acc *Account, hasCredits, unlimited bool, balance *string, overageReached *bool, rateLimitReachedType string) {
	if acc == nil {
		return
	}
	acc.ApplySparseCreditsHeaders(hasCredits, unlimited, balance, overageReached, rateLimitReachedType)
	s.persistCreditSnapshot(acc, true)
}

// PersistRateLimitReachedType records rate_limit_reached_type from the response headers (including clearing it) and
// persists it when a credits snapshot already exists, to avoid restoring an already-lifted hard-stop from the database after a restart.
func (s *Store) PersistRateLimitReachedType(acc *Account, kind string) {
	if acc == nil {
		return
	}
	acc.SetRateLimitReachedType(kind)
	acc.mu.RLock()
	valid := acc.CreditsValid
	acc.mu.RUnlock()
	if !valid {
		return
	}
	s.persistCreditSnapshot(acc, true)
}

// persistCreditSnapshot persists the account's current credits snapshot, and skips it when the fingerprint has not changed.
// flagsOnly=true compares only the fingerprint with the balance removed (the normal response-header path).
func (s *Store) persistCreditSnapshot(acc *Account, flagsOnly bool) {
	if s == nil || s.db == nil || acc == nil {
		return
	}
	acc.creditsPersistMu.Lock()
	defer acc.creditsPersistMu.Unlock()
	acc.mu.Lock()
	snap := acc.creditSnapshotLocked()
	fullKey := creditBalanceKey(snap)
	flagsKey := creditFlagsKey(snap)
	unchanged := acc.creditsPersistedKey == fullKey
	if flagsOnly {
		unchanged = acc.creditsPersistedFlagsKey == flagsKey
	}
	if unchanged {
		acc.mu.Unlock()
		return
	}
	acc.creditsPersistedKey = fullKey
	acc.creditsPersistedFlagsKey = flagsKey
	acc.mu.Unlock()

	snap.UpdatedAt = time.Now()
	raw, err := MarshalCreditBalanceSnapshot(snap)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err = s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{"codex_credits": raw})
	}
	if err != nil {
		// If persisting fails, clear the fingerprint so the next observation retries, instead of wrongly recording it as "persisted".
		acc.mu.Lock()
		acc.creditsPersistedKey = ""
		acc.creditsPersistedFlagsKey = ""
		acc.mu.Unlock()
		log.Printf("[account %d] failed to persist credits snapshot: %v", acc.DBID, err)
	}
}

// MarkResetCreditsProbed records the time of the most recent successful wham usage probe.
// Callers should call it after the wham probe succeeds (usage obtained), whether or not this response carries the reset_credits field,
// because "the wham fetch succeeded" by itself means the reset count is up to date.
func (a *Account) MarkResetCreditsProbed(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCreditsProbedAt = t
}

// NeedsSubscriptionExpiryProbe reports whether the authoritative subscription expiry time needs to be synced from the web /subscriptions page
// expiry time: needed only for paid plans whose expiry time is unknown or near or past (it can only change near the renewal window);
// throttled when the last attempt was less than minInterval ago. (issue #360)
func (a *Account) NeedsSubscriptionExpiryProbe(now time.Time, minInterval time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	plan := strings.ToLower(strings.TrimSpace(a.PlanType))
	if plan == "" || plan == "free" || plan == "api" {
		return false
	}
	if !a.subscriptionMeta.CheckedAt.IsZero() && now.Sub(a.subscriptionMeta.CheckedAt) < minInterval {
		return false
	}
	if a.SubscriptionExpiresAt.IsZero() {
		return true
	}
	return a.SubscriptionExpiresAt.Sub(now) <= expiryUrgencyWarnDays*24*time.Hour
}

// MarkSubscriptionExpiryProbed records the attempt time of the subscription expiry probe (whether it succeeded or failed), for throttling.
// It only changes memory; it is persisted together with the result when the sync finishes.
func (a *Account) MarkSubscriptionExpiryProbed(t time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.subscriptionMeta.CheckedAt = t
}

// ClearUsageCache clears the in-memory usage cache, so the next request fetches it from upstream again
func (a *Account) ClearUsageCache() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = 0
	a.UsagePercent7dValid = false
	a.Reset7dAt = time.Time{}
	a.UsagePercent5h = 0
	a.UsagePercent5hValid = false
	a.Reset5hAt = time.Time{}
	a.UsageUpdatedAt = time.Time{}
	a.UsageUpdatedAt5h = time.Time{}
	a.UsagePercentSpark = 0
	a.UsagePercentSparkValid = false
	a.ResetSparkAt = time.Time{}
	a.UsageUpdatedAtSpark = time.Time{}
}

// SetReset7dAt sets the 7d window reset time
func (a *Account) SetReset7dAt(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Reset7dAt = t
}

// SetWindow7dSeconds records the real period in seconds of the long window (7d slot). It is written only when a valid length (>0) is obtained,
// so that paths that do not know the length (load/seed) do not overwrite an already-probed real value with 0.
func (a *Account) SetWindow7dSeconds(sec int64) {
	if sec <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Window7dSeconds = sec
}

// GetWindow7dSeconds returns the real period in seconds of the long window (7d slot) (0 = unknown).
func (a *Account) GetWindow7dSeconds() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Window7dSeconds
}

// Window7dKind returns the type label of the long window (7d slot): "monthly" (free/team monthly window) / "weekly" / "" (unknown),
// so the admin side labels the progress bar "30 days" rather than wrongly "7 days" (issue #324). The criterion matches the monthly window tolerance of the wham classification.
func (a *Account) Window7dKind() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.Window7dSeconds <= 0:
		return ""
	case isMonthlyWindowSeconds(a.Window7dSeconds):
		return "monthly"
	default:
		return "weekly"
	}
}

// GetReset5hAt gets the 5h window reset time
func (a *Account) GetReset5hAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset5hAt
}

// GetReset7dAt gets the 7d window reset time
func (a *Account) GetReset7dAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset7dAt
}

// GetUsageUpdatedAt gets the 7d usage snapshot refresh time
func (a *Account) GetUsageUpdatedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsageUpdatedAt
}

// GetUsageUpdatedAt5h gets the 5h usage snapshot refresh time
func (a *Account) GetUsageUpdatedAt5h() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsageUpdatedAt5h
}

// GetPlanType gets the account plan type
func (a *Account) GetPlanType() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PlanType
}

// GetCredentialFamilyID returns the stable, irreversible refresh family key.
func (a *Account) GetCredentialFamilyID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CredentialFamilyID
}

// GetCredentialGeneration returns the current identity generation.
func (a *Account) GetCredentialGeneration() int64 {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CredentialGeneration
}

// GroupIDSnapshot returns a copy of the account's current group IDs. Writes to GroupIDs are guarded by a.mu
// (ApplyAccountGroups / ApplyAccountGroupMemberships). Reads across goroutines must go through this snapshot.
func (a *Account) GroupIDSnapshot() []int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneInt64Slice(a.GroupIDs)
}

// InAnyGroup reports whether the account belongs to any group in the given set. It is used for group matching on the scheduling hot path:
// Unlike GroupIDSnapshot, it does not copy the slice, which avoids one allocation per candidate account.
func (a *Account) InAnyGroup(groups map[int64]struct{}) bool {
	if a == nil || len(groups) == 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, id := range a.GroupIDs {
		if _, ok := groups[id]; ok {
			return true
		}
	}
	return false
}

// applyRefreshedPlanTypeLocked applies a plan parsed from refreshed tokens.
// Caller must hold a.mu.
func (a *Account) applyRefreshedPlanTypeLocked(planType string, now time.Time) (string, bool) {
	plan := strings.ToLower(strings.TrimSpace(planType))
	if plan == "" {
		return "", false
	}
	if plan != "free" &&
		strings.EqualFold(a.PlanType, "free") &&
		a.UsagePercent7dValid &&
		a.Reset7dAt.After(now) {
		return plan, false
	}
	a.PlanType = plan
	return plan, true
}

// GetHealthTier returns the current health tier
func (a *Account) GetHealthTier() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return string(a.HealthTier)
}

// GetSchedulerScore returns the current scheduling score
func (a *Account) GetSchedulerScore() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SchedulerScore
}

// GetDispatchScore returns the current scheduling score used for sorting
func (a *Account) GetDispatchScore() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DispatchScore
}

// GetScoreBiasOverride returns the account-level score override
func (a *Account) GetScoreBiasOverride() (int64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ScoreBiasOverride == nil {
		return 0, false
	}
	return *a.ScoreBiasOverride, true
}

// GetScoreBiasEffective returns the bonus currently in effect
func (a *Account) GetScoreBiasEffective() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ScoreBiasEffective
}

// GetBaseConcurrencyOverride returns the account-level concurrency override
func (a *Account) GetBaseConcurrencyOverride() (int64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.BaseConcurrencyOverride == nil {
		return 0, false
	}
	return *a.BaseConcurrencyOverride, true
}

// GetBaseConcurrencyEffective returns the base concurrency currently in effect
func (a *Account) GetBaseConcurrencyEffective() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.BaseConcurrencyEffective
}

func (a *Account) setAllowedAPIKeyIDsLocked(values []int64) {
	normalized := normalizeAllowedAPIKeyIDs(values)
	a.AllowedAPIKeyIDs = cloneInt64Slice(normalized)
	if len(normalized) == 0 {
		a.allowedAPIKeySet = nil
		return
	}
	a.allowedAPIKeySet = make(map[int64]struct{}, len(normalized))
	for _, value := range normalized {
		a.allowedAPIKeySet[value] = struct{}{}
	}
}

func (a *Account) SetAllowedAPIKeyIDs(values []int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setAllowedAPIKeyIDsLocked(values)
}

func (a *Account) GetAllowedAPIKeyIDs() []int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneInt64Slice(a.AllowedAPIKeyIDs)
}

func (a *Account) AllowsAPIKey(apiKeyID int64) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if len(a.AllowedAPIKeyIDs) == 0 {
		return true
	}
	if apiKeyID <= 0 {
		return false
	}
	_, ok := a.allowedAPIKeySet[apiKeyID]
	return ok
}

// normalizeModelCooldownKey normalizes the model cooldown key: it trims surrounding whitespace and converts it to lower case.
// An empty string means there is no usable model identifier, so callers use it to skip cooldown writes and lookups.
func normalizeModelCooldownKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// normalizeModelCooldownArgs normalizes the cooldown key and reason. It returns
// ok=false when the key is empty or resetAt is the zero value; callers keep the existing "write nothing" behavior.
func normalizeModelCooldownArgs(model, reason string, resetAt time.Time) (string, string, bool) {
	key := normalizeModelCooldownKey(model)
	if key == "" || resetAt.IsZero() {
		return "", "", false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rate_limited"
	}
	return key, reason, true
}

// setModelCooldownUntilLocked writes the cooldown and returns the write result; the caller must hold the a.mu write lock.
func (a *Account) setModelCooldownUntilLocked(key, reason string, resetAt, now time.Time) ModelCooldown {
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = make(map[string]ModelCooldown)
	}
	current := a.ModelCooldowns[key]
	level := current.BackoffLevel
	if current.ResetAt.After(now) {
		level++
	}
	if level < 0 {
		level = 0
	}
	cooldown := ModelCooldown{
		Model:        key,
		Reason:       reason,
		ResetAt:      resetAt,
		UpdatedAt:    now,
		BackoffLevel: level,
	}
	a.ModelCooldowns[key] = cooldown
	return cooldown
}

// SetModelCooldownUntil writes a "cooldown until resetAt" entry for the given model in the account's in-memory state and returns the written
// entry (this method only updates the in-memory map and does not persist anything). When the model key is empty or resetAt is the zero value, it returns
// the zero value and writes nothing. Cooldown keys shared by several public tiers (such as wire and family) should use
// SetModelCooldownUntilAtLeast, so a shorter deadline does not shorten an existing longer cooldown.
func (a *Account) SetModelCooldownUntil(model, reason string, resetAt time.Time) ModelCooldown {
	key, reason, ok := normalizeModelCooldownArgs(model, reason, resetAt)
	if !ok {
		return ModelCooldown{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	return a.setModelCooldownUntilLocked(key, reason, resetAt, time.Now())
}

// SetModelCooldownUntilAtLeast is the same as SetModelCooldownUntil, but guarantees that it does not shorten a cooldown that is still
// valid: if the model's existing ResetAt is still in effect and later than the passed resetAt, the existing cooldown is kept,
// and a second return value of false means nothing was written this time.
//
// The "read existing ResetAt, compare, write" sequence runs inside one a.mu write-lock critical section; otherwise two concurrent
// writes would both read the same old value, and the shorter deadline saved last would overwrite the longer deadline written first.
func (a *Account) SetModelCooldownUntilAtLeast(model, reason string, resetAt time.Time) (ModelCooldown, bool) {
	key, reason, ok := normalizeModelCooldownArgs(model, reason, resetAt)
	if !ok {
		return ModelCooldown{}, false
	}
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, exists := a.ModelCooldowns[key]; exists && existing.ResetAt.After(now) && existing.ResetAt.After(resetAt) {
		return existing, false
	}
	return a.setModelCooldownUntilLocked(key, reason, resetAt, now), true
}

func (a *Account) RestoreModelCooldown(model, reason string, resetAt, updatedAt time.Time) {
	key := normalizeModelCooldownKey(model)
	if key == "" || resetAt.IsZero() || !resetAt.After(time.Now()) {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rate_limited"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = make(map[string]ModelCooldown)
	}
	a.ModelCooldowns[key] = ModelCooldown{
		Model:     key,
		Reason:    reason,
		ResetAt:   resetAt,
		UpdatedAt: updatedAt,
	}
}

func (a *Account) IsModelRateLimited(model string) bool {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cooldown, ok := a.ModelCooldowns[key]
	return ok && cooldown.ResetAt.After(time.Now())
}

// ModelCooldownRemaining returns the remaining time until ResetAt for the given model's cooldown; when the model key is empty,
// there is no cooldown entry, or the cooldown has expired, it returns 0.
func (a *Account) ModelCooldownRemaining(model string) time.Duration {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cooldown, ok := a.ModelCooldowns[key]
	if !ok {
		return 0
	}
	remaining := time.Until(cooldown.ResetAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// ModelCooldownFor returns the model's cooldown entry on the account that is still in effect (including ResetAt and
// Reason); if it does not exist or has expired, the second return value is false.
//
// Compared with ModelCooldownRemaining, it also returns ResetAt, so callers do not have to work
// backward from "now + remaining" to the deadline, so they can get the exact existing
// deadline before rewriting a shared cooldown key (for example, to check whether this shorter cooldown would shorten a longer shared one).
func (a *Account) ModelCooldownFor(model string) (ModelCooldown, bool) {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return ModelCooldown{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cooldown, ok := a.ModelCooldowns[key]
	if !ok || !cooldown.ResetAt.After(time.Now()) {
		return ModelCooldown{}, false
	}
	return cooldown, true
}

func (a *Account) ActiveModelCooldowns() []ModelCooldown {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.ModelCooldowns) == 0 {
		return nil
	}
	now := time.Now()
	result := make([]ModelCooldown, 0, len(a.ModelCooldowns))
	for _, cooldown := range a.ModelCooldowns {
		if cooldown.ResetAt.After(now) {
			result = append(result, cooldown)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Model < result[j].Model
	})
	return result
}

func (a *Account) ClearModelCooldown(model string) bool {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ModelCooldowns) == 0 {
		return false
	}
	if _, ok := a.ModelCooldowns[key]; !ok {
		return false
	}
	delete(a.ModelCooldowns, key)
	return true
}

func (a *Account) ClearAllModelCooldowns() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ModelCooldowns) == 0 {
		return nil
	}
	models := make([]string, 0, len(a.ModelCooldowns))
	for model := range a.ModelCooldowns {
		models = append(models, model)
	}
	clear(a.ModelCooldowns)
	sort.Strings(models)
	return models
}

func (a *Account) SetModelCooldownPolicyOverride(mode *string, seconds *int, backoff *bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ModelCooldownModeOverride = mode
	a.ModelCooldownSecondsOverride = seconds
	a.ModelCooldownBackoffOverride = backoff
}

func (a *Account) GetModelCooldownPolicyOverride() (*string, *int, *bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var mode *string
	var seconds *int
	var backoff *bool
	if a.ModelCooldownModeOverride != nil {
		value := *a.ModelCooldownModeOverride
		mode = &value
	}
	if a.ModelCooldownSecondsOverride != nil {
		value := *a.ModelCooldownSecondsOverride
		seconds = &value
	}
	if a.ModelCooldownBackoffOverride != nil {
		value := *a.ModelCooldownBackoffOverride
		backoff = &value
	}
	return mode, seconds, backoff
}

// GetDynamicConcurrencyLimit returns the current dynamic concurrency limit
func (a *Account) GetDynamicConcurrencyLimit() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DynamicConcurrencyLimit
}

// GetSchedulerDebugSnapshot returns a scheduler debug snapshot
func (a *Account) GetSchedulerDebugSnapshot(baseLimit int64) SchedulerDebugSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.recomputeSchedulerLocked(baseLimit)
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked(now)
	if a.dispatchBonusEligibleLocked(now, a.HealthTier) {
		breakdown.UsageUrgencyBonus5h = a.premium5hUsageUrgencyBonusLocked(now)
		breakdown.UsageUrgencyBonus7d = a.premium7dUsageUrgencyBonusLocked(now)
		breakdown.ExpiryUrgencyBonus = a.expiryUrgencyBonusLocked(now)
	}
	return SchedulerDebugSnapshot{
		HealthTier:               string(a.HealthTier),
		SchedulerScore:           a.SchedulerScore,
		DispatchScore:            a.DispatchScore,
		ScoreBiasOverride:        cloneInt64Ptr(a.ScoreBiasOverride),
		ScoreBiasEffective:       a.ScoreBiasEffective,
		BaseConcurrencyOverride:  cloneInt64Ptr(a.BaseConcurrencyOverride),
		BaseConcurrencyEffective: a.BaseConcurrencyEffective,
		DynamicConcurrencyLimit:  a.DynamicConcurrencyLimit,
		Breakdown:                breakdown,
		LastUnauthorizedAt:       a.LastUnauthorizedAt,
		LastRateLimitedAt:        a.LastRateLimitedAt,
		LastTimeoutAt:            a.LastTimeoutAt,
		LastServerErrorAt:        a.LastServerErrorAt,
	}
}

// NeedsUsageProbe reports whether an active usage probe is needed to refresh usage
func (a *Account) NeedsUsageProbe(maxAge time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()

	if a.usageProbeInFlight || a.AccessToken == "" || a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && a.CooldownReason == "unauthorized" && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		return false // token is invalid and wham also returns 401, so the probe is pointless
	}
	// The "active reset count" can only be refreshed by a wham probe (normal /responses traffic does not carry this field),
	// so the separate resetCreditsProbedAt decides whether it is stale. Otherwise an active account's usage snapshot keeps being refreshed by business traffic,
	// which makes usage look "fresh" all the time, so the wham probe is not triggered for a long time,
	// and the reset count takes a long time to be detected.
	resetCreditsStale := a.resetCreditsProbedAt.IsZero() || now.Sub(a.resetCreditsProbedAt) > maxAge

	if a.premium5hRateLimitedLocked(now) {
		// During a premium 5h rate limit, wham may still refresh the reset count; whether to add a Responses
		// recovery probe is decided by ProbeUsageSnapshot based on the authoritative check and the fallback setting.
		return resetCreditsStale
	}
	if a.Status == StatusCooldown && isUsageLimitCooldownReason(a.CooldownReason) && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		// During a 429 cooldown, wham may still refresh the reset count; in Responses authoritative mode, one
		// extra real recovery probe may be sent in the same round, while other modes stay wham-only.
		return resetCreditsStale
	}
	if resetCreditsStale {
		return true
	}
	if !a.UsagePercent7dValid || a.UsageUpdatedAt.IsZero() || now.Sub(a.UsageUpdatedAt) > maxAge {
		return true
	}
	// Probe once as soon as the 5h / 7d window resets (issue: after the countdown reaches zero the account looks "available" again,
	// but the upstream may already have banned it). Criterion: if the usage snapshot was collected before the reset time, it shows stale data from before the reset
	// => a wham probe is needed to confirm the real status, rather than letting the account through blindly. After a successful probe
	// UsageUpdatedAt* becomes later than the (old) reset time, so the condition no longer holds on its own: each reset boundary is probed only once
	// and is not delayed by maxAge (it responds sooner than the maxAge rate-limit fallback below).
	if a.UsagePercent5hValid && !a.Reset5hAt.IsZero() && !a.Reset5hAt.After(now) && a.UsageUpdatedAt5h.Before(a.Reset5hAt) {
		return true
	}
	if a.UsagePercent7dValid && !a.Reset7dAt.IsZero() && !a.Reset7dAt.After(now) && a.UsageUpdatedAt.Before(a.Reset7dAt) {
		return true
	}
	// The 5h window is optional upstream: it is refreshed by maxAge only while a valid local 5h snapshot is still held.
	// When the upstream has removed 5h (issue #382), a missing snapshot should not be probed forever because of the auto-pause 5h setting.
	if a.effectiveAutoPause5h > 0 && !a.AutoPause5hDisabled &&
		a.UsagePercent5hValid && !a.UsageUpdatedAt5h.IsZero() {
		if a.Reset5hAt.IsZero() || a.Reset5hAt.After(now) {
			return now.Sub(a.UsageUpdatedAt5h) > maxAge
		}
	}
	// The 5h usage window's reset time has passed, but the snapshot still shows the high usage collected before the reset (stale data) →
	// trigger one wham refresh so the 5h progress bar and the premium 5h rate-limit cooldown recover along with the official window reset.
	// (Stale 7d data is already covered by the 7d freshness check above; the 5h check previously applied only while Reset5hAt had not expired,
	// so after a reset it stayed at the old value; this fills that gap.)
	// now.Sub(UsageUpdatedAt5h) > maxAge both fires as soon as possible after the window resets, and when the upstream occasionally does not return that window,
	// it limits the probe frequency to avoid repeated probes.
	if a.UsagePercent5hValid && a.UsagePercent5h > 0 && !a.Reset5hAt.IsZero() &&
		!a.Reset5hAt.After(now) && now.Sub(a.UsageUpdatedAt5h) > maxAge {
		return true
	}
	return false
}

// nextProbeBoundary returns the nearest future moment at which this account should trigger a wham probe:
//   - 5h / 7d window reset: the snapshot still holds data collected before the reset, so the progress bar should refresh as soon as the window rolls over;
//   - rate-limit cooldown ends (not unauthorized, since those probes would return 401 and be pointless): confirm the real usage and status the moment the account is usable again.
//
// It returns only moments strictly later than now. These are exactly the boundaries where the reset/cooldown criteria of NeedsUsageProbe flip to
// true, so the Store only needs to probe once at that moment to hit it, without waiting for the background check cycle.
func (a *Account) nextProbeBoundary(now time.Time) (time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" || a.Status == StatusError {
		return time.Time{}, false
	}
	var next time.Time
	consider := func(t time.Time) {
		if t.IsZero() || !t.After(now) {
			return
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if a.UsagePercent5hValid && a.UsageUpdatedAt5h.Before(a.Reset5hAt) {
		consider(a.Reset5hAt)
	}
	if a.UsagePercent7dValid && a.UsageUpdatedAt.Before(a.Reset7dAt) {
		consider(a.Reset7dAt)
	}
	if a.Status == StatusCooldown && a.CooldownReason != "unauthorized" && !a.isTransientRateLimitCooldownLocked() {
		consider(a.CooldownUtil)
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// InLimitedState reports whether the account is in a usage rate-limit or cooldown state (429 cooldown or premium 5h
// rate limit). In normal mode, only wham is used based on this; in Responses authoritative mode, it identifies accounts that need a real recovery
// probe.
// Note: unauthorized cooldowns are not included here, because NeedsUsageProbe already skips those accounts.
func (a *Account) InLimitedState() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.premium5hRateLimitedLocked(now) {
		return true
	}
	if a.Status == StatusCooldown && isUsageLimitCooldownReason(a.CooldownReason) && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		return true
	}
	return false
}

// TryBeginUsageProbe tries to start one usage probe
func (a *Account) TryBeginUsageProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usageProbeInFlight {
		return false
	}
	a.usageProbeInFlight = true
	return true
}

// FinishUsageProbe ends one usage probe
func (a *Account) FinishUsageProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageProbeInFlight = false
}

// NeedsRecoveryProbe reports whether a low-frequency recovery probe is needed for a banned account
func (a *Account) NeedsRecoveryProbe(minInterval time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.recoveryProbeInFlight || a.healthTierLocked() != HealthTierBanned {
		return false
	}
	if a.RefreshToken == "" {
		return false
	}
	if a.PermanentRefreshFailures >= permanentRefreshFailureTerminalLimit {
		// The RT has been judged dead consecutively and moved to the error terminal state, and the probe's pre-refresh still uses the same dead RT,
		// so it cannot succeed. In the terminal state the health tier has already been moved out of banned; this is a fallback: other paths (such as
		// a 401 on the request side) may push the account back into the banned tier, so if the count is not cleared it must not be probed again.
		// Eligibility is restored automatically after a successful refresh or after ClearCooldown clears the count.
		return false
	}
	if a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil) {
		return false
	}
	if !a.LastRecoveryProbeAt.IsZero() && time.Since(a.LastRecoveryProbeAt) < minInterval {
		return false
	}
	return true
}

// TryBeginRecoveryProbe tries to start one recovery probe
func (a *Account) TryBeginRecoveryProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recoveryProbeInFlight {
		return false
	}
	a.recoveryProbeInFlight = true
	a.LastRecoveryProbeAt = time.Now()
	return true
}

// FinishRecoveryProbe ends one recovery probe
func (a *Account) FinishRecoveryProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recoveryProbeInFlight = false
}

// GetActiveRequests returns the current concurrency
func (a *Account) GetActiveRequests() int64 {
	return a.ActiveRequests.Load()
}

// GetOccupiedRequests returns admission pressure including buffered session
// reservations. It never mutates counters and is safe for frequent UI polls.
func (a *Account) GetOccupiedRequests() int64 {
	return accountOccupiedRequests(a)
}

// GetTotalRequests returns the cumulative request count
func (a *Account) GetTotalRequests() int64 {
	return a.TotalRequests.Load()
}

// GetLastUsedAt returns the last-used time
func (a *Account) GetLastUsedAt() time.Time {
	nano := a.LastUsedAt.Load()
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// Store manages multiple accounts (database + token cache)
type Store struct {
	proxyAuditLabels                   map[string]ProxyAuditLabel
	mu                                 sync.RWMutex
	accountMutationMu                  sync.Mutex // serializes account-set and scheduler mutations without nesting their locks
	accounts                           []*Account
	accountsByID                       map[int64]*Account // DBID -> Account index, kept in sync with accounts for O(1) lookup
	accountSnapshot                    atomic.Pointer[accountListSnapshot]
	globalProxy                        string
	maxConcurrency                     atomic.Int64 // maximum concurrency per account
	testConcurrency                    atomic.Int64 // concurrency for batch tests
	testModel                          atomic.Value // model used by test connection (string)
	testContent                        atomic.Value // input content used by test connection (string)
	db                                 *database.DB
	tokenCache                         cache.TokenCache
	oauthRefreshLocksMu                sync.Mutex
	oauthRefreshLocks                  map[string]*oauthRefreshLocalLock
	workspaceLinkedMu                  sync.Mutex                     // in-process deduplication for the workspace-linked circuit breaker
	workspaceLinkedRecent              map[string]workspaceLinkedMark // workspaceID → deduplication window
	apiKeyGroupsMu                     sync.RWMutex
	apiKeyAllowedGroups                map[int64][]int64
	apiKeyAllowedGroupSets             map[int64]map[int64]struct{}
	apiKeyNoAffinityGroups             map[int64][]int64
	apiKeyNoAffinityGroupSets          map[int64]map[int64]struct{}
	apiKeyAllowedPlans                 map[int64][]string
	apiKeyAllowedPlanSets              map[int64]map[string]struct{}
	apiKeyUpstreamChannels             map[int64]string
	promptFilterNewAPIBindingsMu       sync.RWMutex
	promptFilterNewAPIBindings         map[int64]database.PromptFilterNewAPIBinding
	promptRiskTrustMu                  sync.RWMutex
	promptRiskTrustPolicies            map[string]database.PromptRiskTrustPolicy
	usageProbeMu                       sync.RWMutex
	usageProbe                         func(context.Context, *Account) error
	usageProbeCompletion               func()
	usageProbeBatch                    atomic.Bool
	recoveryProbeBatch                 atomic.Bool
	autoCleanUnauthorized              atomic.Bool
	autoCleanRateLimited               atomic.Bool
	autoCleanFullUsage                 atomic.Bool
	autoCleanError                     atomic.Bool
	autoCleanExpired                   atomic.Bool
	lazyMode                           atomic.Bool
	codexOAuthKeepalive                atomic.Bool
	autoCleanupBatch                   atomic.Bool
	maxRetries                         atomic.Int64 // maximum retries after a failed request (retry with another account)
	maxRateLimitRetries                atomic.Int64 // maximum account-switch retries on 429
	backgroundRefreshInterval          atomic.Int64 // background refresh / probe check interval (ns)
	usageProbeMaxAge                   atomic.Int64 // maximum cache duration of a usage probe snapshot (ns)
	usageProbeConcurrency              atomic.Int64 // usage probe parallelism
	usageProbeResponsesFallbackEnabled atomic.Bool
	recoveryProbeInterval              atomic.Int64 // minimum interval between recovery probes (ns)
	backgroundRefreshWakeCh            chan struct{}
	// Probe on time: the moment a rate-limit cooldown or a 5h / 7d window reset countdown reaches zero, wake one wham probe exactly,
	// so the usage progress bar refreshes immediately when the official window rolls over, instead of waiting for the next check cycle.
	// boundaryProbeWakeCh is written to non-blockingly by wakeBoundaryProbe (safe under any lock),
	// and the background goroutine, on receiving it, scans every account's nearest boundary and re-arms a single timer.
	// armedBoundaryAt records the nearest boundary currently armed (UnixNano; 0 = not armed),
	// so wakeBoundaryProbe can decide whether a new boundary is earlier and worth interrupting for.
	boundaryProbeWakeCh chan struct{}
	armedBoundaryAt     atomic.Int64
	lazyRefreshInFlight sync.Map
	stopCh              chan struct{}
	usageFramePersist   *usageFramePersistQueue
	stopOnce            sync.Once
	backgroundCtx       context.Context
	backgroundCancel    context.CancelFunc
	wg                  sync.WaitGroup

	// Proxy pool
	proxyPoolReloadMu    sync.Mutex
	proxyPoolLoader      func(context.Context) ([]*database.ProxyRow, error)
	proxyInventoryLoader func(context.Context) ([]*database.ProxyRow, error)
	proxyPool            []string // list of proxy URLs that are enabled and have not failed testing
	proxyPoolSet         map[string]struct{}
	managedProxySet      map[string]struct{} // all URLs in the proxies table (including disabled or failed-test ones)
	proxyPoolEnabled     bool                // whether the proxy pool is enabled
	proxyRoundRobin      atomic.Uint64       // round-robin counter

	// Fast scheduler POC (off by default, enabled through an environment variable)
	fastScheduler            atomic.Pointer[FastScheduler]
	fastSchedulerEnabled     atomic.Bool
	routingSchedulersMu      sync.RWMutex
	routingSchedulers        map[int64]*routingSchedulerEntry
	routingSchedulerAccounts int
	routingSchedulerAliases  int
	routingGeneration        atomic.Uint64
	indexedMissFallbackNS    atomic.Int64
	schedulerEngine          atomic.Value // string: legacy / shadow / indexed
	schedulerMetrics         *schedulerRuntimeMetrics
	availability             atomic.Pointer[availabilityHub]
	schedulerOutboxStarted   atomic.Bool
	dispatchReconcileStateMu sync.Mutex
	dispatchReconcileDone    chan struct{}
	dispatchReconciledAt     atomic.Int64

	// Codex upstream WebSocket settings (all off by default, without affecting the existing HTTP path)
	codexForceWebsocket atomic.Bool // force the Codex upstream to use WebSocket (reuses the connection pool)
	// codexRequestCompression zstd-compresses the HTTP /responses request body, on by default (to match real clients).
	// It is independent of the WS settings above: WS uses permessage-deflate, while this setting applies only to the HTTP path.
	codexRequestCompression         atomic.Bool
	codexWSKeepaliveEnabled         atomic.Bool  // enable keepalive for idle upstream WS connections (Ping only)
	codexWSKeepaliveIntervalSec     atomic.Int64 // WS keepalive Ping interval (seconds), default 60
	codexWSHideUpstreamErrors       atomic.Bool  // hide raw upstream WS errors, on by default
	codexWSSilentRetryEnabled       atomic.Bool  // silently retry on another account for upstream WS errors before the first packet, on by default
	codexWSSilentMaxRetries         atomic.Int64 // maximum silent account-switch retries for WS, default 2
	codexWSSizeRouterEnabled        atomic.Bool  // self-learning size routing for 1009, on by default
	codexWSBusyMaxWaitSec           atomic.Int64 // maximum wait for a busy session (seconds), default 30 (issue #413)
	codexWSBusyOverflowEnabled      atomic.Bool  // let a busy session overflow to a sibling connection of the same account, off by default
	codexWSBusyPatienceSec          atomic.Int64 // short wait before overflow is triggered (seconds), default 2
	codexWSStatelessSlots           atomic.Int64 // per-account slot budget for blank upstream WS connections, default 8
	codexWSDownstreamKeepaliveSlots atomic.Int64
	overflowAutoCompactEnabled      atomic.Bool // automatic summary retry when the context overflows the window (experimental, off by default, issue #415)
	compactViaResponsesEnabled      atomic.Bool // rewrite /v1/responses/compact as /responses body-signal compaction, off by default
	firstTokenExcludesWsAcquire     atomic.Bool // when saving first_token_ms, subtract the WS connection acquisition time, off by default

	// Pass preflight metadata SSE events straight through to the downstream client (legacy compatibility, off by default, issue #425)
	codexPreflightSSEPassthroughEnabled atomic.Bool

	// Automatic continue-thinking when Codex thinking is truncated (off by default, does not affect the existing path)
	codexContinueThinkingEnabled atomic.Bool  // when upstream truncated thinking is detected, continue thinking automatically and fold the result into a single response
	codexContinueMaxRounds       atomic.Int64 // maximum continue-thinking rounds per request (including the first round), default 8
	codexCLIVersionSyncEnabled   atomic.Bool  // periodically sync the simulated Codex CLI version in the background, default true
	codexCLIVersionSyncInterval  atomic.Int64 // periodic sync interval (hours), default 12
	ignoreUsageLimitStatus       atomic.Bool  // usage windows are only recorded and are not treated as evidence that the account is unavailable

	// Retry interval and transport-error retry policy (issue #331)
	retryIntervalMS       atomic.Int64 // retry interval in milliseconds; 0 = retry immediately (old behavior)
	transportRetryPolicy  atomic.Value // transport-error retry policy: rotate / sticky
	continuousRetryPolicy atomic.Value // database.ContinuousRetryPolicy (off by default)
	githubToken           atomic.Value // GitHub API token, sent only to api.github.com (issue #522)
	githubProxyURL        atomic.Value // outbound proxy used only for GitHub domains; when empty, falls back to the global or environment proxy (issue #522)

	// Fingerprint convergence tier applied by default to newly imported or created Codex accounts: off / device / session / full
	codexFingerprintDefaultMode atomic.Value

	// Smart refresh scheduler
	refreshScheduler atomic.Pointer[RefreshSchedulerIntegration]

	allowRemoteMigration     atomic.Bool  // whether pulling accounts through remote migration is allowed
	modelMapping             atomic.Value // model mapping JSON string
	codexModelMapping        atomic.Value // Codex model mapping JSON string
	payloadRules             atomic.Value // payload request-body rewrite rules JSON string
	reasoningEffortModels    atomic.Value // JSON array of model aliases that carry a reasoning effort
	schedulerMode            atomic.Value // string: "round_robin" / "remaining_quota" / "fill_first" / "reset_5h_first"
	affinityMode             atomic.Value // string: "bounded" / "off" / "strict"
	affinitySpreadEnabled    atomic.Bool  // new affinity keys pick accounts by spreading them with HRW hashing (issue #484)
	modelCooldownSettings    atomic.Value // database.ModelCooldownSettings
	promptFilterConfig       atomic.Value // promptFilterConfigState
	sessionMu                sync.RWMutex
	sessionBindings          map[string]sessionAffinity
	sessionSlotBufferEnabled atomic.Bool
	sessionSlotBufferNS      atomic.Int64
	sessionSlotSequence      uint64
	sessionSlotReservations  map[int64]map[string][]uint64

	globalAutoPause5hThreshold    float64  // protected by mu
	globalAutoPause7dThreshold    float64  // protected by mu
	autoPause5hGuardBandPercent   float64  // protected by mu, percentage points
	autoPause5hGuardConcurrency   int      // protected by mu, 0 = disabled
	smartPacingEnabled            bool     // protected by mu; issue #312 master switch for smart pacing
	smartPacingMinConcurrency     int      // protected by mu, lower bound of pacing concurrency
	smartPacingWindows            string   // protected by mu, "5h,7d" / "5h" / "7d"
	groupAutoPauseThresholds      sync.Map // int64 -> [2]float64 {5h, 7d}
	groupBaseConcurrencyOverrides sync.Map // int64 -> int64; missing means inherit global
	groupNames                    sync.Map // int64 -> string; group ID to name, used by payload rules to match by group name
	groupProxyURLs                sync.Map // int64 -> []string; group-level proxy list (issue #479), missing = not set
}

// sessionAffinity records which account/proxy a given sessionKey is currently bound to.
//
// lastUsedAt is used for the escape condition of bounded affinity (issue #584):
//   - a binding idle for longer than sessionAffinityIdleEscape is unbound, and the next request goes through full account selection. Active sessions
//     never switch accounts because of this: the upstream prompt cache works per account, and rotating mid-way would bill the whole context at
//     the cache-miss price again; once the idle time exceeds the threshold, the upstream cache has already expired anyway, so rotating then costs nothing.
//   - the upper layer also checks during selection whether the bound account is still healthy, and switches account directly if it is not healthy
//
// boundAt / requestCount are kept only for observation and no longer take part in the escape check. Strict mode does not read the escape
// fields (the behavior falls back to the old implementation); off mode never enters this path at all.
type sessionAffinity struct {
	accountID    int64
	proxyURL     string
	boundAt      time.Time
	requestCount int64
	lastUsedAt   time.Time
	expiresAt    time.Time
}

// SessionAffinityGuard carries the one-request decision made while selecting
// an account for an existing sticky session. A non-zero guard means the bound
// account was otherwise eligible but temporarily had no concurrency capacity,
// so the selected fallback must not replace the durable binding.
//
// The preserved account ID is intentionally private: callers may only pass the
// opaque decision back to Store when binding the selected account.
type SessionAffinityGuard struct {
	preserveAccountID int64
}

// PreservesExisting reports whether this selection is a temporary capacity
// spillover whose durable affinity must remain unchanged.
func (g SessionAffinityGuard) PreservesExisting() bool {
	return g.preserveAccountID != 0
}

const defaultSessionAffinityTTL = time.Hour

const maxSessionSlotBuffer = 60 * time.Second

// maxSessionBindings is the soft limit of the session stickiness table. When it is exceeded, the bind path does one full sweep of expired entries.
const maxSessionBindings = 65536

// Default idle escape threshold for bounded affinity. The upstream prompt cache is cleared after roughly 5-10 minutes idle,
// so 10 minutes ensures that rotation only happens at a boundary where the cache is already dead.
const defaultAffinityIdleEscape = 10 * time.Minute

// Affinity mode constants. The affinity_mode system setting uses the following values.
const (
	AffinityModeBounded = "bounded" // default. Sticky, with escape conditions
	AffinityModeOff     = "off"     // stickiness disabled. Every request selects an account again according to the scheduling policy
	AffinityModeStrict  = "strict"  // legacy behavior. Sticks to the bound account until the TTL expires or the account fails
)

const (
	accountCooldownCacheNamespace = "account-cooldown"
	modelCooldownCacheNamespace   = "model-cooldown"
	runtimeCooldownCacheTimeout   = 300 * time.Millisecond
)

type runtimeCooldownRecord = cache.RuntimeCooldown

func sessionAffinityTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_SESSION_AFFINITY_TTL"))
	if raw == "" {
		return defaultSessionAffinityTTL
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultSessionAffinityTTL
}

// sessionAffinityIdleEscape returns the idle escape threshold for bounded mode; it can be
// overridden with CODEX_SESSION_AFFINITY_IDLE_ESCAPE (a Duration or plain seconds).
func sessionAffinityIdleEscape() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_SESSION_AFFINITY_IDLE_ESCAPE"))
	if raw == "" {
		return defaultAffinityIdleEscape
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultAffinityIdleEscape
}

func cooldownRuntimeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), runtimeCooldownCacheTimeout)
}

func accountCooldownRuntimeKey(accountID int64) string {
	return strconv.FormatInt(accountID, 10)
}

func modelCooldownRuntimeKey(accountID int64, model string) string {
	return fmt.Sprintf("%d:%s", accountID, normalizeModelCooldownKey(model))
}

func normalizeCooldownReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "rate_limited"
	}
	return reason
}

func cooldownTTL(resetAt time.Time) (time.Duration, bool) {
	if resetAt.IsZero() {
		return 0, false
	}
	ttl := time.Until(resetAt)
	if ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

func (s *Store) setCachedAccountCooldown(accountID int64, reason string, resetAt time.Time) {
	// All cooldown settings (429 / premium 5h / usage_limit) go through this funnel; attach the "probe on time" wake-up here:
	// exactly one probe at the moment the cooldown countdown reaches zero, to refresh the usage progress bar. unauthorized is excluded (the probe would always return 401, so it is pointless).
	if normalizeCooldownReason(reason) != "unauthorized" {
		s.WakeBoundaryProbe(resetAt)
	}
	s.cacheAccountCooldownRecord(accountID, runtimeCooldownRecord{
		Reason: normalizeCooldownReason(reason), ResetAt: resetAt, UpdatedAt: time.Now(),
	})
}

// Publication is outside Account.mu and FastScheduler.mu. Native cache drivers
// merge atomically; older/custom TokenCache implementations retain SetRuntime.
func (s *Store) cacheAccountCooldownRecord(accountID int64, record runtimeCooldownRecord) runtimeCooldownRecord {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return record
	}
	ttl, ok := cooldownTTL(record.ResetAt)
	if !ok {
		return record
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if merger, ok := s.tokenCache.(cache.RuntimeCooldownMerger); ok {
		merged, err := merger.MergeRuntimeCooldown(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID), record)
		if err != nil {
			log.Printf("[account %d] failed to merge account cooldown cache: %v", accountID, err)
			return record
		}
		return merged
	}
	payload, err := json.Marshal(record)
	if err != nil {
		log.Printf("[account %d] failed to serialize account cooldown cache: %v", accountID, err)
		return record
	}
	if err := s.tokenCache.SetRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID), payload, ttl); err != nil {
		log.Printf("[account %d] failed to write account cooldown cache: %v", accountID, err)
	}
	return record
}

func (s *Store) getCachedAccountCooldown(accountID int64) (runtimeCooldownRecord, bool) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return runtimeCooldownRecord{}, false
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	payload, ok, err := s.tokenCache.GetRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID))
	if err != nil {
		log.Printf("[account %d] failed to read account cooldown cache: %v", accountID, err)
		return runtimeCooldownRecord{}, false
	}
	if !ok || len(payload) == 0 {
		return runtimeCooldownRecord{}, false
	}
	var record runtimeCooldownRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		log.Printf("[account %d] failed to parse account cooldown cache: %v", accountID, err)
		s.deleteCachedAccountCooldown(accountID)
		return runtimeCooldownRecord{}, false
	}
	if !record.ResetAt.After(time.Now()) {
		s.deleteCachedAccountCooldown(accountID)
		return runtimeCooldownRecord{}, false
	}
	record.Reason = normalizeCooldownReason(record.Reason)
	return record, true
}

func (s *Store) deleteCachedAccountCooldown(accountID int64) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.DeleteRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID)); err != nil {
		log.Printf("[account %d] failed to delete account cooldown cache: %v", accountID, err)
	}
}

// ForgetCachedAccountCooldown clears the account's record in the cross-instance cooldown cache.
//
// The admin side clears error / unauthorized state directly at the database layer (re-import, re-authorization,
// merging credentials) and reloads the runtime account; it must be called at the same time. Every time the scheduler picks an account it reads this cache back
// and reapplies the cooldown to the in-memory account. Clearing only the database and not the cache would keep a just-revived account blocked
// until the cache TTL expires (unauthorized can last up to 24h).
func (s *Store) ForgetCachedAccountCooldown(accountID int64) {
	s.deleteCachedAccountCooldown(accountID)
}

func (s *Store) applyCachedAccountCooldown(acc *Account, record runtimeCooldownRecord) {
	if s == nil || acc == nil || !record.ResetAt.After(time.Now()) {
		return
	}
	reason := normalizeCooldownReason(record.Reason)
	baseLimit := s.maxConcurrency.Load()
	acc.mu.Lock()
	current := runtimeCooldownRecord{Reason: acc.CooldownReason, ResetAt: acc.CooldownUtil}
	if acc.isTransientRateLimitCooldownLocked() {
		current.Kind = cache.CooldownKindTransient
	}
	if record.Kind == cache.CooldownKindTransient && (acc.Status == StatusError || accountDispatchBlocked(acc) || acc.healthTierLocked() == HealthTierBanned) {
		acc.mu.Unlock()
		return
	}
	if acc.Status == StatusCooldown && current.ResetAt.After(time.Now()) &&
		(current.Strength() > record.Strength() || current.Strength() == record.Strength() && current.ResetAt.After(record.ResetAt)) {
		acc.mu.Unlock()
		return
	}
	acc.Status = StatusCooldown
	acc.CooldownUtil = record.ResetAt
	acc.CooldownReason = reason
	acc.transientRateLimitUntil = time.Time{}
	if record.Kind == cache.CooldownKindTransient && reason == ResponsesRateLimitedCooldownReason {
		acc.transientRateLimitUntil = record.ResetAt
		acc.transientRateLimitBackoff = max(acc.transientRateLimitBackoff, record.BackoffLevel)
		acc.armTransientRateLimitRecoveryLocked(s)
	} else if acc.transientRateLimitTimer != nil {
		acc.transientRateLimitTimer.Stop()
		acc.transientRateLimitTimer = nil
	}
	now := time.Now()
	if !record.UpdatedAt.IsZero() {
		now = record.UpdatedAt
	}
	switch reason {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.HealthTier = HealthTierBanned
	case "rate_limited_5h", ResponsesRateLimitedCooldownReason:
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	case "rate_limited", "rate_limited_7d", "usage_limited", "usage_limit":
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(baseLimit)
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

func (s *Store) accountHasCachedCooldown(acc *Account) bool {
	if acc == nil {
		return false
	}
	record, ok := s.getCachedAccountCooldown(acc.DBID)
	if !ok {
		return false
	}
	s.applyCachedAccountCooldown(acc, record)
	return true
}

func (s *Store) setCachedModelCooldown(accountID int64, cooldown ModelCooldown) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	key := normalizeModelCooldownKey(cooldown.Model)
	if key == "" {
		return
	}
	ttl, ok := cooldownTTL(cooldown.ResetAt)
	if !ok {
		return
	}
	payload, err := json.Marshal(runtimeCooldownRecord{
		Model:        key,
		Reason:       normalizeCooldownReason(cooldown.Reason),
		ResetAt:      cooldown.ResetAt,
		UpdatedAt:    cooldown.UpdatedAt,
		BackoffLevel: cooldown.BackoffLevel,
	})
	if err != nil {
		log.Printf("[account %d] failed to serialize model cooldown cache model=%s: %v", accountID, key, err)
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.SetRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key), payload, ttl); err != nil {
		log.Printf("[account %d] failed to write model cooldown cache model=%s: %v", accountID, key, err)
	}
}

func (s *Store) getCachedModelCooldown(accountID int64, model string) (runtimeCooldownRecord, bool) {
	return s.getCachedModelCooldownContext(context.Background(), accountID, model)
}

func (s *Store) getCachedModelCooldownContext(parent context.Context, accountID int64, model string) (runtimeCooldownRecord, bool) {
	if s == nil || s.tokenCache == nil || accountID == 0 || parent.Err() != nil {
		return runtimeCooldownRecord{}, false
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return runtimeCooldownRecord{}, false
	}
	ctx, cancel := context.WithTimeout(parent, runtimeCooldownCacheTimeout)
	defer cancel()
	if s.schedulerMetrics != nil {
		s.schedulerMetrics.modelCooldownCacheReads.Add(1)
	}
	payload, ok, err := s.tokenCache.GetRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key))
	if err != nil {
		if parent.Err() == nil {
			log.Printf("[account %d] failed to read model cooldown cache model=%s: %v", accountID, key, err)
		}
		return runtimeCooldownRecord{}, false
	}
	if !ok || len(payload) == 0 {
		return runtimeCooldownRecord{}, false
	}
	var record runtimeCooldownRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		log.Printf("[account %d] failed to parse model cooldown cache model=%s: %v", accountID, key, err)
		s.deleteCachedModelCooldownContext(parent, accountID, key)
		return runtimeCooldownRecord{}, false
	}
	if !record.ResetAt.After(time.Now()) {
		s.deleteCachedModelCooldownContext(parent, accountID, key)
		return runtimeCooldownRecord{}, false
	}
	record.Model = key
	record.Reason = normalizeCooldownReason(record.Reason)
	return record, true
}

func (s *Store) deleteCachedModelCooldown(accountID int64, model string) {
	s.deleteCachedModelCooldownContext(context.Background(), accountID, model)
}

func (s *Store) deleteCachedModelCooldownContext(parent context.Context, accountID int64, model string) {
	if s == nil || s.tokenCache == nil || accountID == 0 || parent.Err() != nil {
		return
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, runtimeCooldownCacheTimeout)
	defer cancel()
	if err := s.tokenCache.DeleteRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key)); err != nil {
		if parent.Err() == nil {
			log.Printf("[account %d] failed to delete model cooldown cache model=%s: %v", accountID, key, err)
		}
	}
}

func (s *Store) applyCachedModelCooldown(acc *Account, model string, record runtimeCooldownRecord) {
	if acc == nil || !record.ResetAt.After(time.Now()) {
		return
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		key = normalizeModelCooldownKey(record.Model)
	}
	if key == "" {
		return
	}
	acc.mu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]ModelCooldown)
	}
	acc.ModelCooldowns[key] = ModelCooldown{
		Model:        key,
		Reason:       normalizeCooldownReason(record.Reason),
		ResetAt:      record.ResetAt,
		UpdatedAt:    updatedAt,
		BackoffLevel: record.BackoffLevel,
	}
	acc.mu.Unlock()
}

func (s *Store) accountHasCachedModelCooldown(acc *Account, model string) bool {
	return s.accountHasCachedModelCooldownContext(context.Background(), acc, model)
}

func (s *Store) accountHasCachedModelCooldownContext(ctx context.Context, acc *Account, model string) bool {
	if acc == nil {
		return false
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	if acc.IsModelRateLimited(key) {
		return true
	}
	record, ok := s.getCachedModelCooldownContext(ctx, acc.DBID, key)
	if !ok {
		return false
	}
	s.applyCachedModelCooldown(acc, key, record)
	return true
}

// WithModelCooldownFilter wraps a request model filter with Redis-backed model cooldown checks.
func (s *Store) WithModelCooldownFilter(model string, filter AccountFilter) AccountFilter {
	if s == nil || normalizeModelCooldownKey(model) == "" {
		return filter
	}
	return s.WithModelCooldownFilterContext(context.Background(), model, filter)
}

// WithModelCooldownFilterContext stops Redis reads when the downstream request
// is canceled. A canceled read must reject the candidate, not fail open into a
// fresh upstream request after the client has already disconnected.
func (s *Store) WithModelCooldownFilterContext(ctx context.Context, model string, filter AccountFilter) AccountFilter {
	if ctx == nil {
		ctx = context.Background()
	}
	key := normalizeModelCooldownKey(model)
	return func(acc *Account) bool {
		if acc == nil || ctx.Err() != nil {
			return false
		}
		if filter != nil && !filter(acc) {
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if s == nil || key == "" {
			return true
		}
		blocked := s.accountHasCachedModelCooldownContext(ctx, acc, key)
		return ctx.Err() == nil && !blocked
	}
}

func fastSchedulerEnabledFromEnv() bool {
	for _, key := range []string{"FAST_SCHEDULER_ENABLED", "CODEX_FAST_SCHEDULER"} {
		if truthyEnv(os.Getenv(key)) {
			return true
		}
	}
	return false
}

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}

// NewStore creates the account manager
func NewStore(db *database.DB, tc cache.TokenCache, settings *database.SystemSettings) *Store {
	if settings == nil {
		settings = &database.SystemSettings{
			MaxConcurrency:                     2,
			TestConcurrency:                    50,
			TestModel:                          "gpt-5.4",
			TestContent:                        DefaultTestContent,
			BackgroundRefreshIntervalMinutes:   2,
			UsageProbeMaxAgeMinutes:            10,
			UsageProbeConcurrency:              defaultUsageProbeConcurrency,
			UsageProbeResponsesFallbackEnabled: true,
			RecoveryProbeIntervalMinutes:       30,
			LazyMode:                           false,
			ProxyURL:                           "",
			MaxRateLimitRetries:                1,
			SchedulerMode:                      "round_robin",
			CodexRequestCompression:            true,
			CodexWSHideUpstreamErrors:          true,
			CodexWSSilentRetryEnabled:          true,
			CodexWSSilentMaxRetries:            2,
			CodexWSSizeRouterEnabled:           true,
			CodexWSBusyAcquireMaxWaitSec:       30,
			CodexWSBusyPatienceSec:             2,
			CodexWSStatelessSlots:              8,
			CodexWSDownstreamKeepaliveSlots:    database.DefaultCodexWSDownstreamKeepaliveSlots,
			CodexContinueMaxRounds:             8,
			AutoPause5hGuardBandPercent:        defaultAutoPause5hGuardBandPercent,
			AutoPause5hGuardConcurrency:        defaultAutoPause5hGuardConcurrency,
			SmartPacingMinConcurrency:          defaultSmartPacingMinConcurrency,
			SmartPacingWindows:                 "5h,7d",
		}
	}
	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	s := &Store{
		globalProxy:                settings.ProxyURL,
		db:                         db,
		tokenCache:                 tc,
		backgroundRefreshWakeCh:    make(chan struct{}, 1),
		boundaryProbeWakeCh:        make(chan struct{}, 1),
		stopCh:                     make(chan struct{}),
		usageFramePersist:          newUsageFramePersistQueue(),
		backgroundCtx:              backgroundCtx,
		backgroundCancel:           backgroundCancel,
		schedulerMetrics:           newSchedulerRuntimeMetrics(),
		routingSchedulers:          make(map[int64]*routingSchedulerEntry),
		proxyPoolEnabled:           settings.ProxyPoolEnabled,
		sessionBindings:            make(map[string]sessionAffinity),
		sessionSlotReservations:    make(map[int64]map[string][]uint64),
		promptFilterNewAPIBindings: make(map[int64]database.PromptFilterNewAPIBinding),
		oauthRefreshLocks:          make(map[string]*oauthRefreshLocalLock),
	}
	s.maxConcurrency.Store(int64(settings.MaxConcurrency))
	s.usageFramePersist.start(s)
	s.testConcurrency.Store(int64(settings.TestConcurrency))
	s.codexRequestCompression.Store(settings.CodexRequestCompression)
	s.availability.Store(newAvailabilityHub())
	s.publishAccountSnapshot(nil)
	s.sessionSlotBufferEnabled.Store(settings.SessionSlotBufferEnabled)
	s.SetSessionSlotBuffer(time.Duration(database.NormalizeSessionSlotBufferSeconds(settings.SessionSlotBufferSeconds)) * time.Second)
	if db != nil {
		s.proxyPoolLoader = db.ListEnabledProxies
		s.proxyInventoryLoader = db.ListProxies
	}
	s.testModel.Store(settings.TestModel)
	s.testContent.Store(NormalizeTestContent(settings.TestContent))
	s.SetBackgroundRefreshInterval(time.Duration(settings.BackgroundRefreshIntervalMinutes) * time.Minute)
	s.SetUsageProbeMaxAge(time.Duration(settings.UsageProbeMaxAgeMinutes) * time.Minute)
	s.SetUsageProbeConcurrency(settings.UsageProbeConcurrency)
	s.SetUsageProbeResponsesFallbackEnabled(settings.UsageProbeResponsesFallbackEnabled)
	s.SetRecoveryProbeInterval(time.Duration(settings.RecoveryProbeIntervalMinutes) * time.Minute)
	s.autoCleanUnauthorized.Store(settings.AutoCleanUnauthorized)
	s.autoCleanRateLimited.Store(settings.AutoCleanRateLimited)
	s.autoCleanFullUsage.Store(settings.AutoCleanFullUsage)
	s.autoCleanError.Store(settings.AutoCleanError)
	s.autoCleanExpired.Store(settings.AutoCleanExpired)
	s.lazyMode.Store(settings.LazyMode)
	s.codexOAuthKeepalive.Store(settings.CodexOAuthKeepaliveEnabled)
	retries := int64(settings.MaxRetries)
	if retries <= 0 {
		retries = 2 // default: retry 2 times
	}
	s.maxRetries.Store(retries)
	rateLimitRetries := int64(settings.MaxRateLimitRetries)
	if rateLimitRetries < 0 {
		rateLimitRetries = 0
	}
	s.maxRateLimitRetries.Store(rateLimitRetries)
	s.allowRemoteMigration.Store(settings.AllowRemoteMigration)
	s.schedulerMode.Store(settings.SchedulerMode)
	s.SetAffinityMode(settings.AffinityMode)
	s.SetSessionAffinitySpread(settings.SessionAffinitySpread)
	if settings.ModelMapping != "" {
		s.modelMapping.Store(settings.ModelMapping)
	}
	if settings.CodexModelMapping != "" {
		s.codexModelMapping.Store(settings.CodexModelMapping)
	}
	if settings.PayloadRules != "" {
		s.payloadRules.Store(settings.PayloadRules)
	}
	if settings.ReasoningEffortModels != "" {
		s.reasoningEffortModels.Store(settings.ReasoningEffortModels)
	}
	promptFilterCfg, promptFilterAdvancedRaw := promptFilterConfigFromSettings(settings)
	if err := s.SetPromptFilterConfigWithAdvancedRaw(promptFilterCfg, promptFilterAdvancedRaw); err != nil {
		// promptFilterAdvancedRaw is already validated by
		// promptFilterConfigFromSettings. Keep a defensive fallback so a corrupt
		// persisted value can never prevent Store initialization.
		s.SetPromptFilterConfig(promptFilterCfg)
	}
	// The new scheduling engine environment variable takes priority; when it is not set, the old fast_scheduler_enabled is still honored.
	legacyFastEnabled := fastSchedulerEnabledFromEnv() || settings.FastSchedulerEnabled
	engineSetting := strings.TrimSpace(os.Getenv("CODEX_SCHEDULER_ENGINE"))
	if engineSetting == "" {
		engineSetting = settings.SchedulerEngine
	}
	engine := normalizeSchedulerEngine(engineSetting, legacyFastEnabled)
	s.schedulerEngine.Store(engine)
	fastEnabled := engine != "legacy"
	s.fastSchedulerEnabled.Store(fastEnabled)
	if fastEnabled {
		scheduler := NewFastScheduler(int64(settings.MaxConcurrency), s.GetSchedulerMode())
		s.configureFastScheduler(scheduler)
		s.fastScheduler.Store(scheduler)
		log.Printf("scheduling engine enabled: engine=%s", engine)
	}

	// Codex upstream WebSocket settings (off by default, do not affect the existing path)
	s.codexForceWebsocket.Store(settings.CodexForceWebsocket)
	s.codexRequestCompression.Store(settings.CodexRequestCompression)
	s.codexWSKeepaliveEnabled.Store(settings.CodexWSKeepaliveEnabled)
	s.codexWSKeepaliveIntervalSec.Store(normalizeWSKeepaliveInterval(settings.CodexWSKeepaliveIntervalSec))
	s.codexWSHideUpstreamErrors.Store(settings.CodexWSHideUpstreamErrors)
	s.codexWSSilentRetryEnabled.Store(settings.CodexWSSilentRetryEnabled)
	s.codexWSSilentMaxRetries.Store(normalizeWSSilentMaxRetries(settings.CodexWSSilentMaxRetries))
	s.codexWSSizeRouterEnabled.Store(settings.CodexWSSizeRouterEnabled)
	s.codexWSBusyMaxWaitSec.Store(int64(database.NormalizeCodexWSBusyAcquireMaxWaitSec(settings.CodexWSBusyAcquireMaxWaitSec)))
	s.codexWSBusyOverflowEnabled.Store(settings.CodexWSBusyOverflowEnabled)
	s.codexWSBusyPatienceSec.Store(int64(database.NormalizeCodexWSBusyPatienceSec(settings.CodexWSBusyPatienceSec)))
	s.codexWSStatelessSlots.Store(int64(database.NormalizeCodexWSStatelessSlots(settings.CodexWSStatelessSlots)))
	s.SetCodexWSDownstreamKeepaliveSlots(settings.CodexWSDownstreamKeepaliveSlots)
	s.overflowAutoCompactEnabled.Store(settings.OverflowAutoCompactEnabled)
	s.compactViaResponsesEnabled.Store(settings.CompactViaResponsesEnabled)
	s.codexPreflightSSEPassthroughEnabled.Store(settings.CodexPreflightSSEPassthroughEnabled)
	s.firstTokenExcludesWsAcquire.Store(settings.FirstTokenExcludesWsAcquire)
	s.codexContinueThinkingEnabled.Store(settings.CodexContinueThinkingEnabled)
	s.codexContinueMaxRounds.Store(int64(database.NormalizeCodexContinueMaxRounds(settings.CodexContinueMaxRounds)))
	s.codexCLIVersionSyncEnabled.Store(settings.CodexCLIVersionSyncEnabled)
	s.codexCLIVersionSyncInterval.Store(int64(database.NormalizeCodexCLIVersionSyncIntervalHours(settings.CodexCLIVersionSyncIntervalHours)))
	s.ignoreUsageLimitStatus.Store(settings.IgnoreUsageLimitStatus)
	s.retryIntervalMS.Store(int64(normalizeRetryIntervalMS(settings.RetryIntervalMS)))
	s.transportRetryPolicy.Store(database.NormalizeTransportRetryPolicy(settings.TransportRetryPolicy))
	continuousPolicy := database.ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	if strings.TrimSpace(settings.ContinuousRetryPolicy) == "" {
		continuousPolicy = database.DefaultContinuousRetryPolicy()
	}
	s.continuousRetryPolicy.Store(continuousPolicy)
	s.codexFingerprintDefaultMode.Store(NormalizeCodexFingerprintMode(settings.CodexFingerprintDefaultMode))
	s.githubToken.Store(strings.TrimSpace(settings.GithubToken))
	s.githubProxyURL.Store(strings.TrimSpace(settings.GithubProxyURL))
	s.SetModelCooldownSettings(database.ModelCooldownSettings{
		RelayMode:           settings.RelayModelCooldownMode,
		RelaySeconds:        settings.RelayModelCooldownSeconds,
		RelayBackoffEnabled: settings.RelayModelCooldownBackoffEnabled,
		OAuthMode:           settings.OAuthModelCooldownMode,
		OAuthSeconds:        settings.OAuthModelCooldownSeconds,
		OAuthBackoffEnabled: settings.OAuthModelCooldownBackoffEnabled,

		OAuthUnsupportedDisabled: settings.OAuthUnsupportedModelCooldownDisabled,
		OAuthUnsupportedSeconds:  settings.OAuthUnsupportedModelCooldownSeconds,
	})

	s.globalAutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(settings.AutoPause5hThreshold)
	s.globalAutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(settings.AutoPause7dThreshold)
	s.autoPause5hGuardBandPercent = normalizeAutoPause5hGuardBandPercent(settings.AutoPause5hGuardBandPercent)
	s.autoPause5hGuardConcurrency = normalizeAutoPause5hGuardConcurrency(settings.AutoPause5hGuardConcurrency)
	s.smartPacingEnabled = settings.SmartPacingEnabled
	s.smartPacingMinConcurrency = normalizeSmartPacingMinConcurrency(settings.SmartPacingMinConcurrency)
	s.smartPacingWindows = normalizeSmartPacingWindows(settings.SmartPacingWindows)

	// Load the proxy pool (including all managed URLs, so fail-closed can recognize them after they are disabled)
	if s.proxyPoolLoader != nil {
		if err := s.ReloadProxyPool(); err != nil {
			log.Printf("failed to load proxy pool: %v", err)
		}
	}

	return s
}

func (s *Store) getFastScheduler() *FastScheduler {
	if s == nil || !s.fastSchedulerEnabled.Load() {
		return nil
	}
	return s.fastScheduler.Load()
}

func (s *Store) configureFastScheduler(scheduler *FastScheduler) {
	if s == nil || scheduler == nil {
		return
	}
	scheduler.mu.Lock()
	scheduler.metrics = s.schedulerMetrics
	scheduler.mu.Unlock()
	scheduler.SetGroupCheck(s.APIKeyAllowsAccount)
	scheduler.SetAcquireFunc(func(acc *Account, concurrencyLimit int64) bool {
		return s.tryAcquireAccount(acc, concurrencyLimit, false)
	})
}

func (s *Store) rebuildFastScheduler() {
	if s == nil {
		return
	}
	s.invalidateRoutingSchedulers()
	if !s.fastSchedulerEnabled.Load() {
		return
	}
	scheduler := s.BuildFastScheduler()
	s.configureFastScheduler(scheduler)
	s.fastScheduler.Store(scheduler)
}

func (s *Store) recomputeAllAccountSchedulerState() {
	if s == nil {
		return
	}
	baseLimit := s.maxConcurrency.Load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		acc.recomputeSchedulerLocked(baseLimit)
		acc.mu.Unlock()
	}
}

func (s *Store) fastSchedulerUpdate(acc *Account) {
	if s == nil || acc == nil {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler != nil {
		scheduler.Update(acc)
	}
	if s.GetSchedulerMode() == SchedulerModeReset5hFirst {
		s.refreshRoutingReset5hAccount(acc)
	}
	s.notifySchedulerAccountAvailability(acc, true)
}

// RefreshUsageFrameScheduler applies an in-memory usage snapshot to the fast
// scheduler and wakes queued dispatchers without touching the database. The
// connection-level WS frame path updates the snapshot on the read pump, so the
// scheduler is refreshed separately from the persistence-throttled path.
func (s *Store) RefreshUsageFrameScheduler(acc *Account) {
	s.fastSchedulerUpdate(acc)
}

func (s *Store) fastSchedulerRemove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler != nil {
		scheduler.Remove(dbID)
	}
	s.notifySchedulerAvailability()
}

func (s *Store) SetFastSchedulerEnabled(enabled bool) {
	if s == nil {
		return
	}
	if enabled {
		s.schedulerEngine.Store("indexed")
	} else {
		s.schedulerEngine.Store("legacy")
	}
	s.fastSchedulerEnabled.Store(enabled)
	if enabled {
		s.recomputeAllAccountSchedulerState()
		s.rebuildFastScheduler()
		s.notifySchedulerAvailability()
		return
	}
	s.fastScheduler.Store(nil)
	s.invalidateRoutingSchedulers()
	s.notifySchedulerAvailability()
}

func (s *Store) FastSchedulerEnabled() bool {
	if s == nil {
		return false
	}
	return s.fastSchedulerEnabled.Load()
}

// normalizeWSKeepaliveInterval normalizes the WS keepalive interval (seconds); a non-positive value → default 60.
func normalizeWSKeepaliveInterval(sec int) int64 {
	if sec <= 0 {
		return 60
	}
	return int64(sec)
}

// normalizeWSSilentMaxRetries limits the WS silent retry count to 0-10.
func normalizeWSSilentMaxRetries(retries int) int64 {
	if retries < 0 {
		return 0
	}
	if retries > 10 {
		return 10
	}
	return int64(retries)
}

// SetCodexForceWebsocket sets the "force Codex upstream to use WebSocket" switch (hot-updated at runtime).
func (s *Store) SetCodexForceWebsocket(enabled bool) {
	if s == nil {
		return
	}
	s.codexForceWebsocket.Store(enabled)
}

// CodexForceWebsocket returns whether the Codex upstream is forced to use WebSocket.
func (s *Store) CodexForceWebsocket() bool {
	if s == nil {
		return false
	}
	return s.codexForceWebsocket.Load()
}

// SetCodexRequestCompression sets the HTTP request body zstd compression switch (hot-updated at runtime).
func (s *Store) SetCodexRequestCompression(enabled bool) {
	if s == nil {
		return
	}
	s.codexRequestCompression.Store(enabled)
}

// CodexRequestCompression returns whether the HTTP /responses request body is zstd-compressed.
// A nil store falls back to true: this option is on by default, and when the setting cannot be read it should keep the same behavior as real clients.
func (s *Store) CodexRequestCompression() bool {
	if s == nil {
		return true
	}
	return s.codexRequestCompression.Load()
}

// SetCodexWSKeepaliveEnabled sets the upstream WS idle connection keepalive switch (hot-updated at runtime).
func (s *Store) SetCodexWSKeepaliveEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSKeepaliveEnabled.Store(enabled)
}

// CodexWSKeepaliveEnabled returns whether upstream WS connection keepalive is enabled.
func (s *Store) CodexWSKeepaliveEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexWSKeepaliveEnabled.Load()
}

// SetCodexWSKeepaliveIntervalSec sets the WS keepalive Ping interval (seconds).
func (s *Store) SetCodexWSKeepaliveIntervalSec(sec int) {
	if s == nil {
		return
	}
	s.codexWSKeepaliveIntervalSec.Store(normalizeWSKeepaliveInterval(sec))
}

// CodexWSKeepaliveIntervalSec returns the WS keepalive Ping interval (seconds), minimum 60.
func (s *Store) CodexWSKeepaliveIntervalSec() int {
	if s == nil {
		return 60
	}
	v := s.codexWSKeepaliveIntervalSec.Load()
	if v <= 0 {
		return 60
	}
	return int(v)
}

// SetCodexWSHideUpstreamErrors sets whether raw upstream WS errors are hidden from the client.
func (s *Store) SetCodexWSHideUpstreamErrors(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSHideUpstreamErrors.Store(enabled)
}

// CodexWSHideUpstreamErrors returns whether raw upstream WS errors are hidden from the client.
func (s *Store) CodexWSHideUpstreamErrors() bool {
	if s == nil {
		return true
	}
	return s.codexWSHideUpstreamErrors.Load()
}

// SetCodexWSSilentRetryEnabled sets whether upstream WS errors before the first packet are silently retried on another account.
func (s *Store) SetCodexWSSilentRetryEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSSilentRetryEnabled.Store(enabled)
}

// CodexWSSilentRetryEnabled returns whether upstream WS errors before the first packet are silently retried on another account.
func (s *Store) CodexWSSilentRetryEnabled() bool {
	if s == nil {
		return true
	}
	return s.codexWSSilentRetryEnabled.Load()
}

// SetCodexWSSizeRouterEnabled sets whether 1009 self-learning size routing is enabled.
func (s *Store) SetCodexWSSizeRouterEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSSizeRouterEnabled.Store(enabled)
}

// CodexWSSizeRouterEnabled returns whether 1009 self-learning size routing is enabled.
func (s *Store) CodexWSSizeRouterEnabled() bool {
	if s == nil {
		return true
	}
	return s.codexWSSizeRouterEnabled.Load()
}

// SetCodexWSSilentMaxRetries sets the maximum number of silent account-switch retries for WS.
func (s *Store) SetCodexWSSilentMaxRetries(retries int) {
	if s == nil {
		return
	}
	s.codexWSSilentMaxRetries.Store(normalizeWSSilentMaxRetries(retries))
}

// CodexWSSilentMaxRetries returns the maximum number of silent account-switch retries for WS.
func (s *Store) CodexWSSilentMaxRetries() int {
	if s == nil {
		return 2
	}
	return int(s.codexWSSilentMaxRetries.Load())
}

// SetCodexWSBusyAcquireMaxWaitSec sets the maximum wait for a busy session (seconds).
func (s *Store) SetCodexWSBusyAcquireMaxWaitSec(seconds int) {
	if s == nil {
		return
	}
	s.codexWSBusyMaxWaitSec.Store(int64(database.NormalizeCodexWSBusyAcquireMaxWaitSec(seconds)))
}

// CodexWSBusyAcquireMaxWaitSec returns the maximum wait for a busy session (seconds).
func (s *Store) CodexWSBusyAcquireMaxWaitSec() int {
	if s == nil {
		return 30
	}
	return int(s.codexWSBusyMaxWaitSec.Load())
}

// SetCodexWSBusyOverflowEnabled sets whether a busy session may overflow to a sibling connection of the same account.
func (s *Store) SetCodexWSBusyOverflowEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSBusyOverflowEnabled.Store(enabled)
}

// CodexWSBusyOverflowEnabled returns whether a busy session may overflow to a sibling connection of the same account.
func (s *Store) CodexWSBusyOverflowEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexWSBusyOverflowEnabled.Load()
}

// SetOverflowAutoCompactEnabled sets whether automatic summary retry on context overflow is enabled (experimental).
func (s *Store) SetOverflowAutoCompactEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.overflowAutoCompactEnabled.Store(enabled)
}

// OverflowAutoCompactEnabled returns whether automatic summary retry on context overflow is enabled (experimental).
func (s *Store) OverflowAutoCompactEnabled() bool {
	if s == nil {
		return false
	}
	return s.overflowAutoCompactEnabled.Load()
}

// SetCompactViaResponsesEnabled sets whether /v1/responses/compact is rewritten as /responses body-signal compaction.
func (s *Store) SetCompactViaResponsesEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.compactViaResponsesEnabled.Store(enabled)
}

// CompactViaResponsesEnabled returns whether /v1/responses/compact is rewritten as /responses body-signal compaction.
func (s *Store) CompactViaResponsesEnabled() bool {
	if s == nil {
		return false
	}
	return s.compactViaResponsesEnabled.Load()
}

// SetCodexPreflightSSEPassthroughEnabled sets whether preflight metadata SSE events are passed straight through to the downstream client (legacy compatibility).
func (s *Store) SetCodexPreflightSSEPassthroughEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexPreflightSSEPassthroughEnabled.Store(enabled)
}

// CodexPreflightSSEPassthroughEnabled returns whether preflight metadata SSE events are passed straight through to the downstream client (legacy compatibility).
func (s *Store) CodexPreflightSSEPassthroughEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexPreflightSSEPassthroughEnabled.Load()
}

// SetFirstTokenExcludesWsAcquire sets whether the saved first_token_ms subtracts the WS connection acquisition time.
func (s *Store) SetFirstTokenExcludesWsAcquire(enabled bool) {
	if s == nil {
		return
	}
	s.firstTokenExcludesWsAcquire.Store(enabled)
}

// FirstTokenExcludesWsAcquire returns whether the saved first_token_ms subtracts the WS connection acquisition time.
func (s *Store) FirstTokenExcludesWsAcquire() bool {
	if s == nil {
		return false
	}
	return s.firstTokenExcludesWsAcquire.Load()
}

// SetCodexWSBusyPatienceSec sets the short wait before overflow is triggered (seconds).
func (s *Store) SetCodexWSBusyPatienceSec(seconds int) {
	if s == nil {
		return
	}
	s.codexWSBusyPatienceSec.Store(int64(database.NormalizeCodexWSBusyPatienceSec(seconds)))
}

// CodexWSBusyPatienceSec returns the short wait before overflow is triggered (seconds).
func (s *Store) CodexWSBusyPatienceSec() int {
	if s == nil {
		return 2
	}
	return int(s.codexWSBusyPatienceSec.Load())
}

// SetGithubToken sets the GitHub API token (sent only to api.github.com).
func (s *Store) SetGithubToken(token string) {
	if s == nil {
		return
	}
	s.githubToken.Store(strings.TrimSpace(token))
}

// GithubToken returns the configured GitHub API token; empty means not configured.
func (s *Store) GithubToken() string {
	if s == nil {
		return ""
	}
	if v, ok := s.githubToken.Load().(string); ok {
		return v
	}
	return ""
}

// SetGithubProxyURL sets the outbound proxy used only for GitHub domains.
func (s *Store) SetGithubProxyURL(proxyURL string) {
	if s == nil {
		return
	}
	s.githubProxyURL.Store(strings.TrimSpace(proxyURL))
}

// GithubProxyURL returns the outbound proxy used only for GitHub domains; empty means falling back to the global or environment proxy.
func (s *Store) GithubProxyURL() string {
	if s == nil {
		return ""
	}
	if v, ok := s.githubProxyURL.Load().(string); ok {
		return v
	}
	return ""
}

// SetCodexWSStatelessSlots sets the per-account slot budget for blank upstream WS connections.
func (s *Store) SetCodexWSStatelessSlots(slots int) {
	if s == nil {
		return
	}
	s.codexWSStatelessSlots.Store(int64(database.NormalizeCodexWSStatelessSlots(slots)))
}

// CodexWSStatelessSlots returns the per-account slot budget for blank upstream WS connections.
func (s *Store) CodexWSStatelessSlots() int {
	if s == nil {
		return 8
	}
	return int(s.codexWSStatelessSlots.Load())
}

// SetCodexContinueThinkingEnabled sets whether thinking is automatically continued when upstream thinking is truncated.
func (s *Store) SetCodexContinueThinkingEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexContinueThinkingEnabled.Store(enabled)
}

// CodexContinueThinkingEnabled returns whether thinking is automatically continued when upstream thinking is truncated.
func (s *Store) CodexContinueThinkingEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexContinueThinkingEnabled.Load()
}

// SetCodexContinueMaxRounds sets the maximum continue-thinking rounds per request (including the first round).
func (s *Store) SetCodexContinueMaxRounds(rounds int) {
	if s == nil {
		return
	}
	s.codexContinueMaxRounds.Store(int64(database.NormalizeCodexContinueMaxRounds(rounds)))
}

// CodexContinueMaxRounds returns the maximum continue-thinking rounds per request (including the first round).
func (s *Store) CodexContinueMaxRounds() int {
	if s == nil {
		return 8
	}
	return int(s.codexContinueMaxRounds.Load())
}

// SetCodexCLIVersionSyncEnabled sets whether the simulated Codex CLI version is periodically synced in the background.
func (s *Store) SetCodexCLIVersionSyncEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexCLIVersionSyncEnabled.Store(enabled)
}

// CodexCLIVersionSyncEnabled returns whether the simulated Codex CLI version is periodically synced in the background.
func (s *Store) CodexCLIVersionSyncEnabled() bool {
	if s == nil {
		return true
	}
	return s.codexCLIVersionSyncEnabled.Load()
}

// SetCodexCLIVersionSyncIntervalHours sets the periodic sync interval (hours, clamped to 1-720).
func (s *Store) SetCodexCLIVersionSyncIntervalHours(hours int) {
	if s == nil {
		return
	}
	s.codexCLIVersionSyncInterval.Store(int64(database.NormalizeCodexCLIVersionSyncIntervalHours(hours)))
}

// CodexCLIVersionSyncIntervalHours returns the periodic sync interval (hours).
func (s *Store) CodexCLIVersionSyncIntervalHours() int {
	if s == nil {
		return 12
	}
	return int(s.codexCLIVersionSyncInterval.Load())
}

// GetProxyURL returns the global proxy address
func (s *Store) GetProxyURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.globalProxy
}

// SetProxyURL updates the global proxy address
func (s *Store) SetProxyURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalProxy = url
}

// NextProxy returns the next proxy URL in round-robin order
func (s *Store) NextProxy() string {
	s.mu.RLock()
	enabled := s.proxyPoolEnabled
	pool := s.proxyPool
	s.mu.RUnlock()

	if !enabled || len(pool) == 0 {
		return s.GetProxyURL() // fallback to the single global proxy
	}
	idx := s.proxyRoundRobin.Add(1)
	return pool[idx%uint64(len(pool))]
}

// ResolveProxyForAccount returns the effective proxy for account-bound internal calls.
// Priority: account proxy > group proxy > sticky proxy pool > global proxy > direct.
// A pin to a managed proxy that is disabled, test-failed, or deleted does not
// fall through and does not go direct while the proxy pool is enabled (issue #517).
func (s *Store) ResolveProxyForAccount(acc *Account) string {
	proxyURL, _ := s.resolveProxyForAccountSnapshot(acc)
	return proxyURL
}

// resolveProxyForAccountSnapshot returns both the selected proxy and whether
// direct egress is permitted from one proxy-policy snapshot. Holding the store
// read lock across selection prevents an account pin from being rejected under
// one pool configuration and then authorized as direct under another.
func (s *Store) resolveProxyForAccountSnapshot(acc *Account) (string, bool) {
	if s == nil {
		return "", false
	}

	var accountID int64
	var accountProxy string
	var groupIDs []int64
	// When Resin carries Codex outbound traffic, fail-closed for the proxy pool no longer applies to Codex accounts: the egress IP is provided by
	// Resin, so neither an empty pool nor a disabled bound managed proxy can make it connect directly with a dirty IP. The selected proxy is still returned as usual
	// (in Resin mode the executor uses it only for auditing), and only the usable check lets it through. Relay accounts do not go through Resin,
	// so they still follow the original rule (issue #679).
	resinCarriesEgress := false
	if acc != nil {
		acc.mu.RLock()
		accountID = acc.DBID
		accountProxy = strings.TrimSpace(acc.ProxyURL)
		groupIDs = cloneInt64Slice(acc.GroupIDs)
		resinCarriesEgress = ResinEgressEnabled()
		acc.mu.RUnlock()
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	managedProxyUnavailable := func(proxy string) bool {
		if !s.proxyPoolEnabled {
			return false
		}
		if _, managed := s.managedProxySet[proxy]; !managed {
			return false
		}
		_, enabled := s.proxyPoolSet[proxy]
		return !enabled
	}
	if accountProxy != "" {
		if managedProxyUnavailable(accountProxy) {
			return "", resinCarriesEgress
		}
		return accountProxy, true
	}

	for _, groupID := range groupIDs {
		urls := s.getGroupProxyURLs(groupID)
		if len(urls) == 0 {
			continue
		}
		start := stickyProxyIndex(accountID, len(urls))
		for i := 0; i < len(urls); i++ {
			proxy := strings.TrimSpace(urls[(start+i)%len(urls)])
			if proxy == "" || managedProxyUnavailable(proxy) {
				continue
			}
			return proxy, true
		}
	}

	if s.proxyPoolEnabled && len(s.proxyPool) > 0 {
		start := stickyProxyIndex(accountID, len(s.proxyPool))
		for i := 0; i < len(s.proxyPool); i++ {
			if proxy := strings.TrimSpace(s.proxyPool[(start+i)%len(s.proxyPool)]); proxy != "" {
				return proxy, true
			}
		}
	}

	proxyURL := strings.TrimSpace(s.globalProxy)
	return proxyURL, proxyURL != "" || !s.proxyPoolEnabled || resinCarriesEgress
}

// resolveGroupProxyForAccount returns the proxy an account inherits from its group (issue #479): following GroupIDs
// order, taking the first group that has a proxy configured. Within the group, one proxy is picked sticky by account ID, because the free account pool is most at risk when accounts
// and egress IPs drift and affect each other, so stickiness keeps each account on the same proxy. If nothing matches, it returns an empty string.
func (s *Store) resolveGroupProxyForAccount(acc *Account) string {
	if s == nil || acc == nil {
		return ""
	}
	acc.mu.RLock()
	accountID := acc.DBID
	groupIDs := cloneInt64Slice(acc.GroupIDs)
	acc.mu.RUnlock()
	for _, groupID := range groupIDs {
		urls := s.getGroupProxyURLs(groupID)
		if len(urls) == 0 {
			continue
		}
		start := stickyProxyIndex(accountID, len(urls))
		for i := 0; i < len(urls); i++ {
			proxy := strings.TrimSpace(urls[(start+i)%len(urls)])
			if proxy == "" || s.managedProxyUnavailable(proxy) {
				continue
			}
			return proxy
		}
	}
	return ""
}

func (s *Store) resolveFallbackProxyForAccount(accountID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.proxyPoolEnabled && len(s.proxyPool) > 0 {
		start := stickyProxyIndex(accountID, len(s.proxyPool))
		for i := 0; i < len(s.proxyPool); i++ {
			if proxy := strings.TrimSpace(s.proxyPool[(start+i)%len(s.proxyPool)]); proxy != "" {
				return proxy
			}
		}
	}

	return strings.TrimSpace(s.globalProxy)
}

func stickyProxyIndex(accountID int64, poolSize int) int {
	if poolSize <= 1 {
		return 0
	}
	if accountID <= 0 {
		return 0
	}
	return int((accountID - 1) % int64(poolSize))
}

func proxyRowURL(p *database.ProxyRow) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.URL)
}

func collectProxyURLs(rows []*database.ProxyRow) []string {
	urls := make([]string, 0, len(rows))
	for _, p := range rows {
		if url := proxyRowURL(p); url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

// managedProxyUnavailable reports that url is a proxies-table member that is
// not currently in the enabled pool. Custom account/group proxy URLs that were
// never added to the pool are not managed and stay usable.
func (s *Store) managedProxyUnavailable(proxyURL string) bool {
	if s == nil {
		return false
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.proxyPoolEnabled {
		return false
	}
	if _, managed := s.managedProxySet[proxyURL]; !managed {
		return false
	}
	if _, enabled := s.proxyPoolSet[proxyURL]; enabled {
		return false
	}
	return true
}

// ManagedProxyUnavailable is the exported form of managedProxyUnavailable.
func (s *Store) ManagedProxyUnavailable(proxyURL string) bool {
	return s.managedProxyUnavailable(proxyURL)
}

// AccountHasUsableEgress reports whether this account can reach upstream without
// violating proxy-pool fail-closed: when the pool is on, an empty resolved
// proxy would have meant direct/dirty-IP, so the account is skipped instead.
func (s *Store) AccountHasUsableEgress(acc *Account) bool {
	_, usable := s.ResolveUsableProxyForAccount(acc)
	return usable
}

func (s *Store) accountHasUsableEgress(acc *Account) bool {
	_, usable := s.ResolveUsableProxyForAccount(acc)
	return usable
}

// ResolveUsableProxyForAccount returns the exact proxy decision a caller must
// use together with its fail-closed usability result. Proxy selection and the
// direct-egress decision come from one policy snapshot, so a concurrent pool
// reconfiguration cannot turn a rejected managed proxy into an empty direct
// request.
func (s *Store) ResolveUsableProxyForAccount(acc *Account) (string, bool) {
	if s == nil || acc == nil {
		return "", false
	}
	return s.resolveProxyForAccountSnapshot(acc)
}

func (s *Store) withUsableEgressFilter(filter AccountFilter) AccountFilter {
	return func(acc *Account) bool {
		if !s.accountHasUsableEgress(acc) {
			return false
		}
		if filter != nil && !filter(acc) {
			return false
		}
		return true
	}
}

// GetProxyPoolEnabled returns whether the proxy pool is enabled
func (s *Store) GetProxyPoolEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyPoolEnabled
}

// SetProxyPoolEnabled sets the proxy pool switch
func (s *Store) SetProxyPoolEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxyPoolEnabled = enabled
}

// ReloadProxyPool reloads the proxy pool from the database
func (s *Store) ReloadProxyPool() error {
	s.proxyPoolReloadMu.Lock()
	defer s.proxyPoolReloadMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loader := s.proxyPoolLoader
	if loader == nil {
		return errors.New("proxy pool loader is not configured")
	}
	proxies, err := loader(ctx)
	if err != nil {
		return err
	}
	enabledURLs := collectProxyURLs(proxies)
	managedURLs := enabledURLs
	auditRows := proxies
	if inventory := s.proxyInventoryLoader; inventory != nil {
		allProxies, invErr := inventory(ctx)
		if invErr != nil {
			return invErr
		}
		managedURLs = collectProxyURLs(allProxies)
		auditRows = allProxies
	}
	s.mu.Lock()
	s.proxyPool = enabledURLs
	s.proxyPoolSet = buildProxyPoolSet(enabledURLs)
	s.managedProxySet = buildProxyPoolSet(managedURLs)
	s.proxyAuditLabels = make(map[string]ProxyAuditLabel, len(auditRows))
	for _, row := range auditRows {
		if row != nil {
			name := strings.TrimSpace(row.Label)
			if name == "" {
				name = "proxy"
			}
			s.proxyAuditLabels[row.URL] = ProxyAuditLabel{ID: row.ID, Name: name}
		}
	}
	s.mu.Unlock()
	log.Printf("proxy pool reloaded: %d active proxies", len(enabledURLs))
	return nil
}

// UnusableManagedProxies returns the subset of proxyURLs that are known to the
// proxy table but absent from the enabled set — disabled, test-failed, or
// deleted. It mirrors the fail-closed rule in resolveProxyForAccountSnapshot:
// while the pool is enabled, an account pinned to one of these has no usable
// egress and will not be scheduled. Callers use it to warn instead of silently
// importing accounts that cannot serve traffic.
func (s *Store) UnusableManagedProxies(proxyURLs []string) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.proxyPoolEnabled {
		return nil
	}
	var unusable []string
	for _, raw := range proxyURLs {
		proxyURL := strings.TrimSpace(raw)
		if proxyURL == "" {
			continue
		}
		if _, managed := s.managedProxySet[proxyURL]; !managed {
			continue
		}
		if _, enabled := s.proxyPoolSet[proxyURL]; !enabled {
			unusable = append(unusable, proxyURL)
		}
	}
	return unusable
}

// RemoveProxyURLs immediately removes proxies from the in-memory pool. It uses
// the same serialization lock as ReloadProxyPool so an older reload snapshot
// cannot publish the removed URLs afterward.
func (s *Store) RemoveProxyURLs(proxyURLs []string) {
	if s == nil {
		return
	}
	removeSet := buildProxyPoolSet(proxyURLs)
	if len(removeSet) == 0 {
		return
	}

	s.proxyPoolReloadMu.Lock()
	s.mu.Lock()
	filtered := make([]string, 0, len(s.proxyPool))
	for _, proxyURL := range s.proxyPool {
		if _, remove := removeSet[strings.TrimSpace(proxyURL)]; !remove {
			filtered = append(filtered, proxyURL)
		}
	}
	s.proxyPool = filtered
	s.proxyPoolSet = buildProxyPoolSet(filtered)
	s.mu.Unlock()
	s.proxyPoolReloadMu.Unlock()

	s.sessionMu.Lock()
	for key, binding := range s.sessionBindings {
		if _, remove := removeSet[strings.TrimSpace(binding.proxyURL)]; remove {
			delete(s.sessionBindings, key)
		}
	}
	s.sessionMu.Unlock()
}

// GetAutoCleanUnauthorized returns whether 401 accounts are cleaned up automatically
func (s *Store) GetAutoCleanUnauthorized() bool {
	return s.autoCleanUnauthorized.Load()
}

// SetAutoCleanUnauthorized sets whether 401 accounts are cleaned up automatically
func (s *Store) SetAutoCleanUnauthorized(enabled bool) {
	s.autoCleanUnauthorized.Store(enabled)
}

// GetAutoCleanRateLimited returns whether 429 accounts are cleaned up automatically
func (s *Store) GetAutoCleanRateLimited() bool {
	return s.autoCleanRateLimited.Load()
}

// SetAutoCleanRateLimited sets whether 429 accounts are cleaned up automatically
func (s *Store) SetAutoCleanRateLimited(enabled bool) {
	s.autoCleanRateLimited.Store(enabled)
}

// GetAutoCleanFullUsage returns whether accounts with full usage are cleaned up automatically
func (s *Store) GetAutoCleanFullUsage() bool {
	return s.autoCleanFullUsage.Load()
}

// SetAutoCleanFullUsage sets whether accounts with full usage are cleaned up automatically
func (s *Store) SetAutoCleanFullUsage(enabled bool) {
	s.autoCleanFullUsage.Store(enabled)
}

// GetAutoCleanError returns whether error accounts are cleaned up automatically
func (s *Store) GetAutoCleanError() bool {
	return s.autoCleanError.Load()
}

// SetAutoCleanError sets whether error accounts are cleaned up automatically
func (s *Store) SetAutoCleanError(enabled bool) {
	s.autoCleanError.Store(enabled)
}

// GetAutoCleanExpired returns whether expired accounts are cleaned up automatically
func (s *Store) GetAutoCleanExpired() bool {
	return s.autoCleanExpired.Load()
}

// SetAutoCleanExpired sets whether expired accounts are cleaned up automatically
func (s *Store) SetAutoCleanExpired(enabled bool) {
	s.autoCleanExpired.Store(enabled)
}

// GetLazyMode returns whether lazy mode is enabled.
func (s *Store) GetLazyMode() bool {
	return s.lazyMode.Load()
}

// SetLazyMode sets lazy mode. When enabled, accounts are not refreshed or probed proactively; the AT is refreshed only when scheduling picks the account.
func (s *Store) SetLazyMode(enabled bool) {
	s.lazyMode.Store(enabled)
	s.rebuildFastScheduler()
}

// SetBackgroundRefreshInterval sets the background refresh / probe check interval.
func (s *Store) SetBackgroundRefreshInterval(d time.Duration) {
	if d <= 0 {
		d = defaultBackgroundRefreshInterval
	}
	s.backgroundRefreshInterval.Store(int64(d))
	select {
	case s.backgroundRefreshWakeCh <- struct{}{}:
	default:
	}
}

// GetBackgroundRefreshInterval returns the background refresh / probe check interval.
func (s *Store) GetBackgroundRefreshInterval() time.Duration {
	d := time.Duration(s.backgroundRefreshInterval.Load())
	if d <= 0 {
		return defaultBackgroundRefreshInterval
	}
	return d
}

// SetUsageProbeMaxAge sets the maximum cache duration of usage probes.
func (s *Store) SetUsageProbeMaxAge(d time.Duration) {
	if d <= 0 {
		d = defaultUsageProbeMaxAge
	}
	s.usageProbeMaxAge.Store(int64(d))
}

// GetUsageProbeMaxAge returns the maximum cache duration of usage probes.
func (s *Store) GetUsageProbeMaxAge() time.Duration {
	d := time.Duration(s.usageProbeMaxAge.Load())
	if d <= 0 {
		return defaultUsageProbeMaxAge
	}
	return d
}

// SetUsageProbeConcurrency sets the usage probe parallelism.
func (s *Store) SetUsageProbeConcurrency(n int) {
	if n <= 0 {
		n = defaultUsageProbeConcurrency
	}
	if n > 128 {
		n = 128
	}
	s.usageProbeConcurrency.Store(int64(n))
}

// GetUsageProbeConcurrency returns the usage probe parallelism.
func (s *Store) GetUsageProbeConcurrency() int {
	n := int(s.usageProbeConcurrency.Load())
	if n <= 0 {
		return defaultUsageProbeConcurrency
	}
	return n
}

// SetUsageProbeResponsesFallbackEnabled sets whether a real /responses probe may be sent after a wham failure.
func (s *Store) SetUsageProbeResponsesFallbackEnabled(enabled bool) {
	s.usageProbeResponsesFallbackEnabled.Store(enabled)
}

// UsageProbeResponsesFallbackEnabled returns whether a real /responses probe may be sent after a wham failure.
func (s *Store) UsageProbeResponsesFallbackEnabled() bool {
	if s == nil {
		return true
	}
	return s.usageProbeResponsesFallbackEnabled.Load()
}

// UsageProbeRunning reports whether a batch usage probe is currently active.
func (s *Store) UsageProbeRunning() bool {
	if s == nil {
		return false
	}
	return s.usageProbeBatch.Load()
}

// SetRecoveryProbeInterval sets the minimum interval between recovery probes.
func (s *Store) SetRecoveryProbeInterval(d time.Duration) {
	if d <= 0 {
		d = defaultRecoveryProbeInterval
	}
	s.recoveryProbeInterval.Store(int64(d))
}

// GetRecoveryProbeInterval returns the minimum interval between recovery probes.
func (s *Store) GetRecoveryProbeInterval() time.Duration {
	d := time.Duration(s.recoveryProbeInterval.Load())
	if d <= 0 {
		return defaultRecoveryProbeInterval
	}
	return d
}

// RecoveryProbeRunning reports whether a batch recovery probe is currently active.
func (s *Store) RecoveryProbeRunning() bool {
	if s == nil {
		return false
	}
	return s.recoveryProbeBatch.Load()
}

// AutoCleanupRunning reports whether an automatic cleanup pass is currently active.
func (s *Store) AutoCleanupRunning() bool {
	if s == nil {
		return false
	}
	return s.autoCleanupBatch.Load()
}

// CleanExpiredNow runs one expired-account cleanup immediately and returns the number cleaned up
func (s *Store) CleanExpiredNow() int {
	return s.CleanExpiredAccounts(context.Background(), 30*time.Minute)
}

// Init initializes the store: it loads accounts from the database
func (s *Store) Init(ctx context.Context) error {
	// Capture the durable change-log position before the full snapshot. Events
	// committed while loadFromDB is running are replayed after publication.
	outboxWatermark := int64(0)
	if s.db != nil {
		var err error
		outboxWatermark, err = s.db.SchedulerOutboxHighWatermark(ctx)
		if err != nil {
			return fmt.Errorf("failed to read scheduler outbox watermark: %w", err)
		}
	}
	// 1. Load accounts from the database into memory
	if err := s.loadFromDB(ctx); err != nil {
		return err
	}
	if err := s.LoadPromptFilterNewAPIBindings(ctx); err != nil {
		return fmt.Errorf("failed to load NewAPI platform bindings: %w", err)
	}
	s.startSchedulerOutboxConsumer(outboxWatermark)

	if len(s.accounts) == 0 {
		log.Println("⚠ no accounts in the database yet, add them through the admin panel")
		return nil
	}

	s.rebuildFastScheduler()

	// 2. Count the available accounts; refresh for RT accounts is handled by StartBackgroundRefresh
	available := 0
	for _, acc := range s.accounts {
		if acc.IsAvailable() {
			available++
		}
	}
	log.Printf("account initialization complete: %d/%d available", available, len(s.accounts))
	return nil
}

// loadFromDB loads accounts from the database
func (s *Store) loadFromDB(ctx context.Context) error {
	var pauseErr error
	ctx, pauseErr = s.withIntelligencePauses(ctx)
	if pauseErr != nil {
		return pauseErr
	}
	rows, err := s.db.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("failed to load accounts from the database: %w", err)
	}
	modelCooldowns := make(map[int64][]*database.AccountModelCooldownRow)
	if cooldownRows, err := s.db.ListActiveModelCooldowns(ctx); err == nil {
		for _, row := range cooldownRows {
			modelCooldowns[row.AccountID] = append(modelCooldowns[row.AccountID], row)
		}
	} else {
		log.Printf("failed to load model cooldown state: %v", err)
	}
	if err := s.db.ClearExpiredModelCooldowns(ctx); err != nil {
		log.Printf("failed to clean up expired model cooldown state: %v", err)
	}

	for _, row := range rows {
		account := s.buildAccountFromRow(ctx, row, modelCooldowns)
		if account == nil {
			continue
		}
		s.accounts = append(s.accounts, account)
	}

	s.rebuildAccountIndex()
	s.publishAccountSnapshot(s.accounts)
	log.Printf("loaded %d accounts from the database", len(s.accounts))
	if groups, err := s.db.ListAccountGroups(ctx); err == nil {
		for _, g := range groups {
			if g.AutoPause5hThreshold > 0 || g.AutoPause7dThreshold > 0 {
				s.groupAutoPauseThresholds.Store(g.ID, [2]float64{g.AutoPause5hThreshold, g.AutoPause7dThreshold})
			}
			if g.BaseConcurrencyOverride.Valid {
				s.groupBaseConcurrencyOverrides.Store(g.ID, g.BaseConcurrencyOverride.Int64)
			}
			s.SetGroupProxyURLs(g.ID, g.ProxyURLs)
			s.groupNames.Store(g.ID, strings.TrimSpace(g.Name))
		}
	}
	if memberships, err := s.db.ListAccountGroupMemberships(ctx); err == nil {
		s.ApplyAccountGroupMemberships(memberships)
	} else {
		log.Printf("failed to load account groups: %v", err)
	}
	if err := s.LoadAPIKeyAllowedGroups(ctx); err != nil {
		log.Printf("failed to load API Key group restrictions: %v", err)
	}
	return nil
}

// isRemovedProviderUpstreamType reports whether upstream_type names a provider this gateway no longer serves.
// An empty or unknown value is a Codex account and is never treated as removed.
func isRemovedProviderUpstreamType(upstreamType string) bool {
	switch strings.ToLower(strings.TrimSpace(upstreamType)) {
	case "claude", "grok", "xai", "antigravity", "openai_responses":
		return true
	}
	return false
}

// buildAccountFromRow converts a database account row into a runtime account; returns nil when credentials are missing or unusable.
func (s *Store) buildAccountFromRow(ctx context.Context, row *database.AccountRow, modelCooldowns map[int64][]*database.AccountModelCooldownRow) *Account {
	rt := row.GetCredential("refresh_token")
	st := row.GetCredential("session_token")
	at := row.GetCredential("access_token")
	upstreamType := row.GetCredential("upstream_type")
	if isRemovedProviderUpstreamType(upstreamType) {
		log.Printf("[account %d] upstream type %q is no longer supported (only ChatGPT/Codex accounts are supported), skipping", row.ID, strings.ToLower(strings.TrimSpace(upstreamType)))
		return nil
	}
	models := normalizeModelList(row.GetCredentialStringSlice("models"))
	codexFingerprintMode := NormalizeCodexFingerprintMode(row.GetCredential(CodexFingerprintModeCredentialKey))
	accountTimezone := NormalizeAccountTimezone(row.GetCredential(AccountTimezoneCredentialKey))
	// Agent Identity: no AT/RT; it signs dynamically with agent_private_key, so the empty-credential guard below must not reject it.
	isAgentIdentityAccount := strings.EqualFold(strings.TrimSpace(row.GetCredential("auth_mode")), CodexAuthModeAgentIdentity) &&
		strings.TrimSpace(row.GetCredential("agent_runtime_id")) != "" &&
		strings.TrimSpace(row.GetCredential("agent_private_key")) != ""
	if rt == "" && st == "" && at == "" && !isAgentIdentityAccount {
		log.Printf("[account %d] missing refresh_token, session_token and access_token, skipping", row.ID)
		return nil
	}

	account := &Account{
		DBID:                    row.ID,
		CredentialGeneration:    row.CredentialGeneration,
		CredentialFamilyID:      row.CredentialFamilyID,
		RefreshToken:            rt,
		SessionToken:            st,
		ProxyURL:                strings.TrimSpace(row.ProxyURL),
		CustomHeaders:           row.GetCredentialStringMap("custom_headers"),
		UpstreamRequestIDHeader: row.GetCredential(UpstreamRequestIDHeaderCredentialKey),
		HealthTier:              HealthTierWarm,
		UpstreamType:            upstreamType,
		Models:                  models,
		CodexFingerprintMode:    codexFingerprintMode,
		Timezone:                accountTimezone,
	}
	account.AddedAt.Store(row.CreatedAt.UnixNano())
	if account.CredentialGeneration <= 0 {
		account.CredentialGeneration = 1
	}
	account.ScoreBiasOverride = reflectOptionalInt64Field(row, "ScoreBiasOverride")
	account.BaseConcurrencyOverride = reflectOptionalInt64Field(row, "BaseConcurrencyOverride")
	account.setAllowedAPIKeyIDsLocked(row.GetCredentialInt64Slice("allowed_api_key_ids"))
	account.Tags = cloneStringSlice(row.Tags)
	if row.Locked {
		atomic.StoreInt32(&account.Locked, 1)
	}
	if !row.Enabled {
		atomic.StoreInt32(&account.DispatchPaused, 1)
	}
	if s.intelligencePaused(ctx, row.ID) {
		atomic.StoreInt32(&account.IntelligencePaused, 1)
	}
	account.CreditEnabled = row.CreditEnabled
	account.CreditSkipUsageWindow = row.CreditSkipUsageWindow
	account.IgnoreUsageLimitStatusOverride = row.GetCredentialOptionalBool("ignore_usage_limit_status_override")
	if rawMode := strings.TrimSpace(row.GetCredential("model_cooldown_mode_override")); rawMode != "" {
		if database.IsValidModelCooldownMode(rawMode) {
			mode := database.NormalizeModelCooldownMode(rawMode, database.ModelCooldownModeAdaptive)
			account.ModelCooldownModeOverride = &mode
		}
	}
	if seconds, ok := row.GetCredentialInt64("model_cooldown_seconds_override"); ok {
		if seconds >= 1 && seconds <= database.MaxModelCooldownSeconds {
			value := int(seconds)
			account.ModelCooldownSecondsOverride = &value
		}
	}
	account.ModelCooldownBackoffOverride = row.GetCredentialOptionalBool("model_cooldown_backoff_override")
	account.recomputeEffectiveIgnoreUsageLimitStatus(s.IgnoreUsageLimitStatus())
	account.SkipWarmTier = row.SkipWarmTier
	if row.Status == "error" {
		account.Status = StatusError
		account.ErrorMsg = row.ErrorMessage
		account.HealthTier = HealthTierRisky
	}
	// Agent Identity: fills in signing credentials and identity info (no AT/RT, health tier is set directly to healthy)
	if isAgentIdentityAccount {
		account.CodexAuthMode = CodexAuthModeAgentIdentity
		account.AgentRuntimeID = strings.TrimSpace(row.GetCredential("agent_runtime_id"))
		account.AgentPrivateKey = strings.TrimSpace(row.GetCredential("agent_private_key"))
		account.AgentTaskID = strings.TrimSpace(row.GetCredential("task_id"))
		account.AccountID = row.GetCredential("account_id")
		account.Email = row.GetCredential("email")
		account.PlanType = row.GetCredential("plan_type")
		if account.PlanType == "" {
			account.PlanType = "free"
		}
		if account.Status != StatusError {
			account.HealthTier = HealthTierHealthy
		}
	}

	// try to restore the existing AT from credentials
	if at != "" {
		account.AccessToken = at
		account.AccountID = row.GetCredential("account_id")
		account.Email = row.GetCredential("email")
		account.PlanType = row.GetCredential("plan_type")
		if account.Status != StatusError {
			account.HealthTier = HealthTierHealthy
		}
		if expiresAt := row.GetCredential("expires_at"); expiresAt != "" {
			if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil {
				account.ExpiresAt = parsed
			} else {
				log.Printf("[account %d] failed to parse expires_at: %v", row.ID, err)
			}
		}
	}
	if subExp := row.GetCredential("subscription_expires_at"); subExp != "" {
		if parsed, err := time.Parse(time.RFC3339, subExp); err == nil {
			account.SubscriptionExpiresAt = parsed
		}
	}
	account.subscriptionMeta = SubscriptionMetaFromCredentials(row.GetCredential)
	if row.CooldownUntil.Valid {
		if time.Now().Before(row.CooldownUntil.Time) {
			account.SetCooldownUntil(row.CooldownUntil.Time, row.CooldownReason)
		} else if row.CooldownReason != "" {
			if err := s.db.ClearCooldown(ctx, row.ID); err != nil {
				log.Printf("[account %d] failed to clean up expired cooldown state: %v", row.ID, err)
			}
		}
	}
	if usagePct := row.GetCredential("codex_7d_used_percent"); usagePct != "" {
		if parsed, err := strconv.ParseFloat(usagePct, 64); err == nil {
			updatedAt := time.Time{}
			if usageUpdatedAt := row.GetCredential("codex_usage_updated_at"); usageUpdatedAt != "" {
				if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAt); err == nil {
					updatedAt = parsedTime
				} else {
					log.Printf("[account %d] failed to parse codex_usage_updated_at: %v", row.ID, err)
				}
			}
			account.SetUsageSnapshot(parsed, updatedAt)
			// restore the 7d reset time
			if resetAt := row.GetCredential("codex_7d_reset_at"); resetAt != "" {
				if t, err := time.Parse(time.RFC3339, resetAt); err == nil {
					account.SetReset7dAt(t)
				}
			}
		} else {
			log.Printf("[account %d] failed to parse codex_7d_used_percent: %v", row.ID, err)
		}
	}
	// restore the 5h usage snapshot
	if usagePct5h := row.GetCredential("codex_5h_used_percent"); usagePct5h != "" {
		if parsed, err := strconv.ParseFloat(usagePct5h, 64); err == nil {
			resetAt := time.Time{}
			if r := row.GetCredential("codex_5h_reset_at"); r != "" {
				if t, err := time.Parse(time.RFC3339, r); err == nil {
					resetAt = t
				}
			}
			updatedAt := time.Time{}
			if usageUpdatedAt5h := row.GetCredential("codex_5h_usage_updated_at"); usageUpdatedAt5h != "" {
				if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAt5h); err == nil {
					updatedAt = parsedTime
				} else {
					log.Printf("[account %d] failed to parse codex_5h_usage_updated_at: %v", row.ID, err)
				}
			}
			account.SetUsageSnapshot5hAt(parsed, resetAt, updatedAt)
		}
	}
	if activatedResetAt := row.GetCredential("codex_5h_window_activated_reset_at"); activatedResetAt != "" {
		if t, err := time.Parse(time.RFC3339, activatedResetAt); err == nil {
			account.Mark5hWindowActivated(t)
		} else {
			log.Printf("[account %d] failed to parse codex_5h_window_activated_reset_at: %v", row.ID, err)
		}
	}
	if usagePctSpark := row.GetCredential("codex_spark_used_percent"); usagePctSpark != "" {
		if parsed, err := strconv.ParseFloat(usagePctSpark, 64); err == nil {
			resetAt := time.Time{}
			if r := row.GetCredential("codex_spark_reset_at"); r != "" {
				if t, err := time.Parse(time.RFC3339, r); err == nil {
					resetAt = t
				}
			}
			updatedAt := time.Time{}
			if usageUpdatedAtSpark := row.GetCredential("codex_spark_usage_updated_at"); usageUpdatedAtSpark != "" {
				if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAtSpark); err == nil {
					updatedAt = parsedTime
				} else {
					log.Printf("[account %d] failed to parse codex_spark_usage_updated_at: %v", row.ID, err)
				}
			}
			account.SetUsageSnapshotSparkAt(parsed, resetAt, updatedAt)
		}
	}
	// restore the credit balance snapshot: only the wham probe can refresh credits, so without this restore an account would be judged
	// "out of credits". Credits standing in for a rate limit stop working, and "clean up rate-limited accounts" deletes such an account as a real rate limit.
	account.RestoreCreditBalanceFromJSON(row.GetCredential("codex_credits"))
	if threshold, ok := row.GetCredentialFloat64("auto_pause_5h_threshold"); ok {
		account.AutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(threshold)
	}
	if threshold, ok := row.GetCredentialFloat64("auto_pause_7d_threshold"); ok {
		account.AutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(threshold)
	}
	account.AutoPause5hDisabled = row.GetCredentialBool("auto_pause_5h_disabled")
	account.AutoPause7dDisabled = row.GetCredentialBool("auto_pause_7d_disabled")
	account.KeepConcurrencyOnDegrade = row.GetCredentialBool(KeepConcurrencyOnDegradeCredentialKey)
	if limit, ok := row.GetCredentialInt64("dispatch_count_limit"); ok {
		account.SetDispatchCountLimit(limit)
	}
	if priority, ok := row.GetCredentialInt64("scheduler_priority"); ok {
		account.SetSchedulerPriority(priority)
	}
	account.recomputeEffectiveAutoPause(s)
	for _, cooldown := range modelCooldowns[row.ID] {
		account.RestoreModelCooldown(cooldown.Model, cooldown.Reason, cooldown.ResetAt, cooldown.UpdatedAt)
	}
	account.mu.Lock()
	account.recomputeSchedulerLocked(s.maxConcurrency.Load())
	account.mu.Unlock()
	if s.db != nil {
		if snapshot, err := s.db.LoadDaybreakSnapshot(ctx, row.ID); err == nil {
			account.ApplyDaybreakSnapshot(snapshot)
		}
	}
	return account
}

// BuildTransientAccountByID builds a temporary account from the database (including soft-deleted accounts in the recycle bin),
// not added to the runtime pool and not scheduled; used for connectivity tests in the recycle bin.
func (s *Store) BuildTransientAccountByID(ctx context.Context, dbID int64) (*Account, error) {
	row, err := s.db.GetAccountByIDIncludingDeleted(ctx, dbID)
	if err != nil {
		return nil, err
	}
	account := s.buildAccountFromRow(ctx, row, nil)
	if account == nil {
		return nil, fmt.Errorf("account %d is missing usable credentials", dbID)
	}
	return account, nil
}

// LoadAccountByID loads a single account from the database and adds it to the runtime pool (used for recycle bin restore and similar cases).
func (s *Store) LoadAccountByID(ctx context.Context, dbID int64) error {
	if s.FindByID(dbID) != nil {
		return nil
	}
	row, err := s.db.GetAccountByID(ctx, dbID)
	if err != nil {
		return err
	}
	account := s.buildAccountFromRow(ctx, row, nil)
	if account == nil {
		return fmt.Errorf("account %d is missing usable credentials", dbID)
	}
	// Full startup applies group memberships after loading all accounts.
	// Single-account reloads need the same state before entering the runtime pool.
	groupIDs, err := s.db.GetAccountGroupIDs(ctx, dbID)
	if err != nil {
		return fmt.Errorf("failed to load groups for account %d: %w", dbID, err)
	}
	account.mu.Lock()
	account.GroupIDs = cloneInt64Slice(groupIDs)
	account.recomputeEffectiveAutoPause(s)
	account.mu.Unlock()
	s.AddAccount(account)
	return nil
}

const (
	dispatchStateReconcileInterval = time.Second
	// dispatchStateReconcileTimeout bounds the detached background scan. This
	// is the only runtime path that reloads cross-process account changes, so
	// the bound must comfortably exceed a full-pool scan on a slow database —
	// a scan that always times out would starve cross-process pickup forever.
	dispatchStateReconcileTimeout = 30 * time.Second
)

// ReconcileDispatchState repairs the small runtime projection that can become
// stale when another process updates the shared database. It is intentionally
// called only after account selection misses and is throttled to keep normal
// request traffic database-free.
func (s *Store) ReconcileDispatchState(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	done, owner := s.beginDispatchStateReconcile()
	if !owner {
		return false, nil
	}
	defer s.finishDispatchStateReconcile(done)
	return s.reconcileDispatchState(ctx)
}

func (s *Store) reconcileDispatchState(ctx context.Context) (bool, error) {
	now := time.Now()
	if last := s.dispatchReconciledAt.Load(); last > 0 && now.Sub(time.Unix(0, last)) < dispatchStateReconcileInterval {
		return false, nil
	}
	s.dispatchReconciledAt.Store(now.UnixNano())

	var pauseErr error
	ctx, pauseErr = s.withIntelligencePauses(ctx)
	if pauseErr != nil {
		return false, pauseErr
	}
	rows, err := s.db.ListActive(ctx)
	if err != nil {
		return false, fmt.Errorf("reconcile dispatch accounts: %w", err)
	}
	memberships, err := s.db.ListAccountGroupMemberships(ctx)
	if err != nil {
		return false, fmt.Errorf("reconcile account groups: %w", err)
	}
	cooldownRows, cooldownErr := s.db.ListActiveModelCooldowns(ctx)
	if cooldownErr != nil {
		// Proceeding with an empty map would let newly discovered accounts
		// enter dispatch without their persisted model cooldowns.
		return false, fmt.Errorf("reconcile model cooldowns: %w", cooldownErr)
	}
	modelCooldowns := make(map[int64][]*database.AccountModelCooldownRow, len(cooldownRows))
	for _, cooldown := range cooldownRows {
		modelCooldowns[cooldown.AccountID] = append(modelCooldowns[cooldown.AccountID], cooldown)
	}

	changed := false
	activeIDs := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		if row == nil || row.ID == 0 {
			continue
		}
		activeIDs[row.ID] = struct{}{}
		if acc := s.FindByID(row.ID); acc != nil {
			groupIDs := normalizeAllowedGroupIDs(memberships[row.ID])
			allowedAPIKeyIDs := normalizeAllowedAPIKeyIDs(row.GetCredentialInt64Slice("allowed_api_key_ids"))
			acc.mu.Lock()
			acc.UpstreamRequestIDHeader = row.GetCredential(UpstreamRequestIDHeaderCredentialKey)
			accountMetadataChanged := !int64SliceEqual(normalizeAllowedGroupIDs(acc.GroupIDs), groupIDs) ||
				!int64SliceEqual(normalizeAllowedAPIKeyIDs(acc.AllowedAPIKeyIDs), allowedAPIKeyIDs)
			if accountMetadataChanged {
				acc.GroupIDs = cloneInt64Slice(groupIDs)
				acc.setAllowedAPIKeyIDsLocked(allowedAPIKeyIDs)
				acc.recomputeEffectiveAutoPause(s)
				acc.recomputeEffectiveGroupBaseConcurrency(s)
				acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
			}
			acc.mu.Unlock()
			if accountMetadataChanged {
				changed = true
				s.fastSchedulerUpdate(acc)
			}

			paused := int32(0)
			if !row.Enabled {
				paused = 1
			}
			qualityPaused := int32(0)
			if s.intelligencePaused(ctx, row.ID) {
				qualityPaused = 1
			}
			if atomic.SwapInt32(&acc.IntelligencePaused, qualityPaused) != qualityPaused {
				s.fastSchedulerUpdate(acc)
				changed = true
			}
			if atomic.SwapInt32(&acc.DispatchPaused, paused) != paused {
				changed = true
				s.fastSchedulerUpdate(acc)
			}
			continue
		}

		account := s.buildAccountFromRow(ctx, row, modelCooldowns)
		if account == nil {
			continue
		}
		account.mu.Lock()
		account.GroupIDs = cloneInt64Slice(memberships[row.ID])
		account.recomputeEffectiveAutoPause(s)
		account.mu.Unlock()
		s.AddAccount(account)
		changed = true
	}

	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil {
			continue
		}
		if _, ok := activeIDs[acc.DBID]; ok {
			continue
		}
		if atomic.SwapInt32(&acc.DispatchPaused, 1) == 0 {
			changed = true
			s.fastSchedulerUpdate(acc)
		}
	}
	return changed, nil
}

func (s *Store) beginDispatchStateReconcile() (chan struct{}, bool) {
	s.dispatchReconcileStateMu.Lock()
	defer s.dispatchReconcileStateMu.Unlock()
	if s.dispatchReconcileDone != nil {
		return s.dispatchReconcileDone, false
	}
	done := make(chan struct{})
	s.dispatchReconcileDone = done
	return done, true
}

func (s *Store) finishDispatchStateReconcile(done chan struct{}) {
	s.dispatchReconcileStateMu.Lock()
	defer s.dispatchReconcileStateMu.Unlock()
	if s.dispatchReconcileDone != done {
		return
	}
	s.dispatchReconcileDone = nil
	close(done)
}

// TriggerDispatchStateReconcileAsync refreshes the in-memory dispatch
// projection without putting the request path on the database scan. A miss
// can fan out across many requests during an upstream outage, so this is
// deliberately single-flight and bounded by a short timeout.
func (s *Store) TriggerDispatchStateReconcileAsync() <-chan struct{} {
	if s == nil || s.db == nil {
		return nil
	}

	done, owner := s.beginDispatchStateReconcile()
	if !owner {
		return done
	}
	// Inside the throttle window a fresh run would be a guaranteed no-op:
	// return nil so callers skip their grace wait instead of receiving an
	// instantly-closed channel, and no throwaway goroutine is spawned. The
	// check sits after ownership acquisition so callers racing an in-flight
	// run still coalesce onto its completion channel above.
	if last := s.dispatchReconciledAt.Load(); last > 0 && time.Since(time.Unix(0, last)) < dispatchStateReconcileInterval {
		s.finishDispatchStateReconcile(done)
		return nil
	}
	if !s.startDBBackgroundTask(func(parent context.Context) {
		defer s.finishDispatchStateReconcile(done)
		ctx, cancel := context.WithTimeout(parent, dispatchStateReconcileTimeout)
		defer cancel()
		if changed, err := s.reconcileDispatchState(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("async scheduling state rebuild failed: %v", err)
			}
		} else if changed {
			log.Printf("async scheduling state rebuild completed")
		}
	}) {
		s.finishDispatchStateReconcile(done)
	}
	return done
}

// StartBackgroundRefresh starts periodic background refresh
func (s *Store) StartBackgroundRefresh() {
	backgroundCtx := s.backgroundCtx
	if backgroundCtx == nil {
		backgroundCtx = context.Background()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		refreshTimer := time.NewTimer(s.GetBackgroundRefreshInterval())
		autoCleanupTicker := time.NewTicker(30 * time.Second)
		fullUsageCleanupTicker := time.NewTicker(5 * time.Minute)
		expiredCleanupTicker := time.NewTicker(15 * time.Minute)
		// add a timed rebuild of FastScheduler to improve performance
		rebuildSchedulerTicker := time.NewTicker(10 * time.Minute)
		// probe-on-deadline timer: always armed to the nearest rate-limit cooldown or window reset boundary; when the countdown reaches zero, the probe refreshes immediately.
		boundaryProbeTimer := time.NewTimer(time.Hour)
		if !boundaryProbeTimer.Stop() {
			<-boundaryProbeTimer.C
		}
		defer refreshTimer.Stop()
		defer autoCleanupTicker.Stop()
		defer fullUsageCleanupTicker.Stop()
		defer expiredCleanupTicker.Stop()
		defer rebuildSchedulerTicker.Stop()
		defer boundaryProbeTimer.Stop()

		resetRefreshTimer := func() {
			if !refreshTimer.Stop() {
				select {
				case <-refreshTimer.C:
				default:
				}
			}
			refreshTimer.Reset(s.GetBackgroundRefreshInterval())
		}

		// armed once at startup; after that it is re-armed on every inspection, wake-up and deadline so it always tracks the nearest boundary.
		s.armNextBoundaryProbe(boundaryProbeTimer)

		for {
			select {
			case <-refreshTimer.C:
				if s.GetLazyMode() {
					if s.GetCodexOAuthKeepalive() {
						s.parallelRefreshAll(backgroundCtx)
					}
					s.TriggerUsageProbeAsync()
				} else {
					s.parallelRefreshAll(backgroundCtx)
					s.TriggerUsageProbeAsync()
					s.TriggerRecoveryProbeAsync()
				}
				refreshTimer.Reset(s.GetBackgroundRefreshInterval())
				// the background check may have refreshed each account's reset time, so also re-arm the probe-on-deadline timer,
				// as a fallback for boundary changes that were not explicitly signalled with WakeBoundaryProbe between two wake-ups.
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-boundaryProbeTimer.C:
				// an account's rate-limit cooldown or window reset just reached zero: probe immediately to refresh real usage, then arm the next boundary.
				s.TriggerUsageProbeAsync()
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-s.boundaryProbeWakeCh:
				// an earlier new boundary appeared (e.g. a 429 cooldown was just hit); re-arm to that time.
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-s.backgroundRefreshWakeCh:
				resetRefreshTimer()
			case <-autoCleanupTicker.C:
				s.TriggerAutoCleanupAsync()
			case <-fullUsageCleanupTicker.C:
				if s.GetAutoCleanFullUsage() && !s.GetLazyMode() {
					s.startDBBackgroundTask(func(ctx context.Context) {
						s.CleanFullUsageAccounts(ctx)
					})
				}
			case <-expiredCleanupTicker.C:
				// every 15 minutes, clean up accounts that were added more than 30 minutes ago (requires the switch to be enabled)
				if s.GetAutoCleanExpired() {
					s.startDBBackgroundTask(func(ctx context.Context) {
						s.CleanExpiredAccounts(ctx, 30*time.Minute)
					})
				}
			case <-rebuildSchedulerTicker.C:
				// periodically rebuild the scheduler to optimize memory and performance
				if s.FastSchedulerEnabled() {
					s.rebuildFastScheduler()
				}
			case <-s.stopCh:
				return
			case <-backgroundCtx.Done():
				return
			}
		}
	}()
}

// Stop stops background refresh
func (s *Store) Stop() {
	s.stopOnce.Do(func() {
		if hub := s.availability.Load(); hub != nil {
			hub.stop()
		}
		if s.backgroundCancel != nil {
			s.backgroundCancel()
		}
		for _, acc := range s.accountSnapshotAccounts() {
			if acc == nil {
				continue
			}
			acc.mu.Lock()
			if acc.transientRateLimitTimer != nil {
				acc.transientRateLimitTimer.Stop()
				acc.transientRateLimitTimer = nil
			}
			acc.mu.Unlock()
		}
		if s.stopCh != nil {
			close(s.stopCh)
		}
		s.DisableRefreshScheduler()
	})
	s.wg.Wait()
}

func (s *Store) startDBBackgroundTask(task func(context.Context)) bool {
	if s == nil || task == nil {
		return false
	}
	if s.db != nil {
		return s.db.RunBackgroundTask(task)
	}
	go task(context.Background())
	return true
}

// CleanByRuntimeStatus cleans up accounts by runtime status (used by the automatic cleanup flow)
// premium 5h rate-limited accounts are skipped, because they recover naturally within 5h and do not need to be deleted.
// For a manual one-click cleanup, use CleanRateLimitedManual instead; it removes all rate-limited accounts.
func (s *Store) CleanByRuntimeStatus(ctx context.Context, targetStatus string) int {
	return s.cleanByRuntimeStatusMatch(ctx, targetStatus, nil)
}

// CollectCleanTargets collects accounts that can be cleaned up by runtime status, without deleting them.
// The admin streaming cleanup takes this list first and then calls SoftDeleteForClean on each account, so that progress can be reported.
func (s *Store) CollectCleanTargets(targetStatus string, match func(*Account) bool) []*Account {
	accounts := s.accountSnapshotAccounts()
	targets := make([]*Account, 0)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		status := acc.RuntimeStatus()
		if status != targetStatus && !(targetStatus == "rate_limited" && status == ResponsesRateLimitedCooldownReason) {
			continue
		}
		// accounts using credits to stand in for a rate limit show as rate-limited but are still scheduled normally; cleaning them up would wrongly delete good accounts.
		if acc.UsingCredits() {
			continue
		}
		if match != nil && !match(acc) {
			continue
		}
		if targetStatus == "rate_limited" && acc.IsPremium5hRateLimited() {
			continue
		}
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}
		targets = append(targets, acc)
	}
	return targets
}

// CollectRateLimitedManualTargets collects the accounts to delete for a manual one-click cleanup of rate-limited accounts.
func (s *Store) CollectRateLimitedManualTargets() []*Account {
	accounts := s.accountSnapshotAccounts()
	targets := make([]*Account, 0)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		status := acc.RuntimeStatus()
		if status != "rate_limited" && status != ResponsesRateLimitedCooldownReason && status != "rate_limited_5h" && status != "rate_limited_7d" && status != "usage_exhausted" {
			continue
		}
		if acc.UsingCredits() {
			continue
		}
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}
		targets = append(targets, acc)
	}
	return targets
}

// SoftDeleteForClean removes a single account from the account pool and records a cleanup event. When db is empty, only the in-memory state is updated.
func (s *Store) SoftDeleteForClean(ctx context.Context, acc *Account, eventReason string) error {
	if acc == nil {
		return nil
	}
	if s.db != nil {
		if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
			return err
		}
	}
	s.RemoveAccount(acc.DBID)
	if s.db != nil {
		if err := s.db.InsertAccountEvent(ctx, acc.DBID, "deleted", eventReason); err != nil {
			log.Printf("[account %d] failed to record cleanup event: %v", acc.DBID, err)
		}
	}
	return nil
}

// cleanByRuntimeStatusMatch cleans up accounts by runtime status; when match is non-nil, only matching accounts are cleaned up.
func (s *Store) cleanByRuntimeStatusMatch(ctx context.Context, targetStatus string, match func(*Account) bool) int {
	cleaned := 0
	for _, acc := range s.CollectCleanTargets(targetStatus, match) {
		if err := s.SoftDeleteForClean(ctx, acc, "auto_clean"); err != nil {
			log.Printf("[account %d] failed to clean up %s status: %v", acc.DBID, targetStatus, err)
			continue
		}
		cleaned++
	}
	return cleaned
}

// CleanRateLimitedManual cleans up all accounts that count as "rate-limited" (used for the manual one-click cleanup).
// Difference from CleanByRuntimeStatus("rate_limited"):
//   - covers all rate-limit-related RuntimeStatus values, including the authoritative 429 from Responses
//   - does not skip premium 5h rate-limited accounts: a manual trigger means the user clearly intends to delete them
//   - locked accounts are still skipped (consistent with all cleanup flows)
func (s *Store) CleanRateLimitedManual(ctx context.Context) int {
	cleaned := 0
	for _, acc := range s.CollectRateLimitedManualTargets() {
		if err := s.SoftDeleteForClean(ctx, acc, "manual_clean"); err != nil {
			log.Printf("[account %d] manual cleanup of rate-limited accounts failed: %v", acc.DBID, err)
			continue
		}
		cleaned++
	}
	return cleaned
}

// ==================== least-connections scheduling ====================

// Next gets the next available account (healthy first, pick low load, fair scheduling for warm accounts)
func (s *Store) Next() *Account {
	return s.NextExcluding(0, nil)
}

// NextExcluding gets the next available account, excluding the given set of account IDs.
// Used during retries to avoid selecting an account that already failed (e.g. with 401) again.
func (s *Store) NextExcluding(apiKeyID int64, exclude map[int64]bool) *Account {
	return s.NextExcludingWithFilter(apiKeyID, exclude, nil)
}

type accountAcquireFailure uint8

const (
	accountAcquireFailureNone accountAcquireFailure = iota
	accountAcquireFailureCapacity
	accountAcquireFailureDispatchLimit
	accountAcquireFailureUnavailable
)

func (s *Store) tryAcquireAccountWithFailure(acc *Account, limit int64, updateSchedulerOnLimit bool) (bool, accountAcquireFailure) {
	if acc == nil || limit <= 0 {
		return false, accountAcquireFailureDispatchLimit
	}
	if accountDispatchBlocked(acc) {
		return false, accountAcquireFailureUnavailable
	}
	if !reserveOccupiedAccountSlot(acc, limit) {
		if accountDispatchBlocked(acc) {
			return false, accountAcquireFailureUnavailable
		}
		return false, accountAcquireFailureCapacity
	}
	now := time.Now()
	reservation := acc.reserveDispatchCount(now)
	if !reservation.Allowed {
		releaseOccupiedAccountSlot(acc)
		s.markDispatchCountLimitCooldown(acc, reservation.ResetAt, updateSchedulerOnLimit)
		return false, accountAcquireFailureDispatchLimit
	}
	acc.TotalRequests.Add(1)
	acc.LastUsedAt.Store(now.UnixNano())
	if reservation.HitLimit {
		s.markDispatchCountLimitCooldown(acc, reservation.ResetAt, updateSchedulerOnLimit)
	}
	return true, accountAcquireFailureNone
}

func (s *Store) tryAcquireAccount(acc *Account, limit int64, updateSchedulerOnLimit bool) bool {
	acquired, _ := s.tryAcquireAccountWithFailure(acc, limit, updateSchedulerOnLimit)
	return acquired
}

// accountOccupiedRequests is a pure snapshot. All production admission paths
// update OccupiedRequests together with ActiveRequests, so reads must never
// "repair" the counter: a stale ActiveRequests sample written back here can
// resurrect an already released slot. max is only a defensive display/read
// fallback for tests or rolling upgrades that may momentarily expose old state.
func accountOccupiedRequests(acc *Account) int64 {
	if acc == nil {
		return 0
	}
	active := acc.ActiveRequests.Load()
	occupied := acc.OccupiedRequests.Load()
	if active > occupied {
		return active
	}
	return occupied
}

func accountDispatchBlocked(acc *Account) bool {
	return acc == nil || atomic.LoadInt32(&acc.Disabled) != 0 || (atomic.LoadInt32(&acc.DispatchPaused) != 0 || atomic.LoadInt32(&acc.IntelligencePaused) != 0)
}

func reserveOccupiedAccountSlot(acc *Account, limit int64) bool {
	if limit <= 0 || accountDispatchBlocked(acc) {
		return false
	}
	for {
		occupied := acc.OccupiedRequests.Load()
		if occupied >= limit || acc.ActiveRequests.Load() >= limit {
			return false
		}
		if acc.OccupiedRequests.CompareAndSwap(occupied, occupied+1) {
			acc.ActiveRequests.Add(1)
			if accountDispatchBlocked(acc) {
				releaseOccupiedAccountSlot(acc)
				return false
			}
			return true
		}
	}
}

func releaseOccupiedAccountSlot(acc *Account) bool {
	if acc == nil {
		return false
	}
	activeReleased := atomicDecrementIfPositive(&acc.ActiveRequests)
	occupiedReleased := atomicDecrementIfPositive(&acc.OccupiedRequests)
	return activeReleased || occupiedReleased
}

func atomicDecrementIfPositive(counter *atomic.Int64) bool {
	if counter == nil {
		return false
	}
	for {
		current := counter.Load()
		if current <= 0 {
			return false
		}
		if counter.CompareAndSwap(current, current-1) {
			return true
		}
	}
}

func atomicSubtractFloorZero(counter *atomic.Int64, delta int64) {
	if counter == nil || delta <= 0 {
		return
	}
	for {
		current := counter.Load()
		if current <= 0 {
			return
		}
		next := current - delta
		if next < 0 {
			next = 0
		}
		if counter.CompareAndSwap(current, next) {
			return
		}
	}
}

// NextExcludingWithFilter gets the next available account and applies the request-level account filter.
func (s *Store) NextExcludingWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, DispatchPolicyStandard)
}

// indexedMissFallbackInterval rate-limits the full-scan fallback after an index miss: globally, at most one scan is let through every
// 500ms, which covers time-based recovery (cooldown expiry produces no event) without letting a burst of misses
// degrade back to an O(pool size) scan on every request.
const indexedMissFallbackInterval = 500 * time.Millisecond

func (s *Store) tryIndexedMissFallback() bool {
	now := time.Now().UnixNano()
	last := s.indexedMissFallbackNS.Load()
	if now-last < int64(indexedMissFallbackInterval) {
		return false
	}
	return s.indexedMissFallbackNS.CompareAndSwap(last, now)
}

// NextExcludingWithDispatch selects an account by usage policy. spark requests ignore account-level 5h/7d limits.
func (s *Store) NextExcludingWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	started := time.Now()
	filter = s.withUsableEgressFilter(filter)
	lazyMode := s.GetLazyMode()
	shadowChecked := false
	shadowIndexedHit := false
	if !lazyMode && s.SchedulerEngine() == "shadow" && s.shouldSampleSchedulerShadow() {
		if scheduler := s.getFastScheduler(); scheduler != nil {
			shadowChecked = true
			shadowIndexedHit = scheduler.HasAvailableWithDispatch(apiKeyID, exclude, filter, policy)
		}
	}
	if scheduler := s.routingFastScheduler(apiKeyID); scheduler != nil && s.SchedulerEngine() != "shadow" {
		for attempts := 0; attempts < 16; attempts++ {
			acc := scheduler.AcquireExcludingWithDispatch(apiKeyID, exclude, filter, policy)
			if acc == nil {
				break
			}
			if s.accountHasBlockingCachedCooldown(acc, policy) {
				s.Release(acc)
				continue
			}
			s.recordSchedulerSelection(started, true, false, true, 0)
			return acc
		}
		// an occasional full-scan fallback is let through when the index misses: cooldowns and rate limits that recover purely by time expiry
		// produce no events, so the index never re-inserts them on its own; this rate-limited fallback is what picks them up and repairs the index.
		if s.SchedulerEngine() == "indexed" && !lazyMode && !s.tryIndexedMissFallback() {
			s.recordSchedulerSelection(started, true, false, false, 0)
			return nil
		}
	}
	// Lazy mode still needs its metadata/refresh fallback when no ready indexed
	// account exists. In steady state, however, ready accounts now stay on the
	// O(1) indexed path instead of scanning the whole pool for every request.
	if lazyMode {
		acc := s.nextExcludingWithFilterLazy(apiKeyID, exclude, filter, policy)
		s.recordSchedulerSelection(started, false, true, acc != nil, len(s.accountSnapshotAccounts()))
		return acc
	}

	scanned := 0
	for attempts := 0; attempts < 16; attempts++ {
		var best *Account
		bestSchedulerPriority := minSchedulerPriority - 1
		bestPriority := -1
		bestDispatchScore := -math.MaxFloat64
		var bestReset5h time.Time
		var bestLoad int64 = math.MaxInt64
		var bestLimit int64
		maxConcurrency := s.maxConcurrency.Load()

		accounts := s.accountSnapshotAccounts()
		scanned += len(accounts)
		for _, acc := range accounts {
			if exclude != nil && exclude[acc.DBID] {
				continue
			}
			if !acc.dispatchableForPolicy(policy) {
				continue
			}
			if !s.accountAllowedForAPIKey(acc, apiKeyID) {
				continue
			}
			if filter != nil && !filter(acc) {
				continue
			}

			load := accountOccupiedRequests(acc)
			tier, _, dispatchScore, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, policy)
			if limit <= 0 || load >= limit {
				continue
			}

			// account scheduling priority strictly precedes health tier and scheduling score (issue #358)
			schedulerPriority := acc.schedulerPriority()
			priority := tierPriority(tier)
			var reset5h time.Time
			resetOrder := 0
			if s.GetSchedulerMode() == SchedulerModeReset5hFirst && policy != DispatchPolicySpark {
				reset5h = acc.reset5hSchedulingDeadline(time.Now())
				resetOrder = compareReset5hDeadlines(reset5h, bestReset5h)
			}
			if schedulerPriority > bestSchedulerPriority ||
				(schedulerPriority == bestSchedulerPriority && (priority > bestPriority ||
					(priority == bestPriority && (resetOrder < 0 || resetOrder == 0 && (dispatchScore > bestDispatchScore ||
						(dispatchScore == bestDispatchScore && load < bestLoad) ||
						(dispatchScore == bestDispatchScore && load == bestLoad && fastRandN(2) == 0)))))) {
				bestSchedulerPriority = schedulerPriority
				bestPriority = priority
				bestDispatchScore = dispatchScore
				bestReset5h = reset5h
				bestLoad = load
				bestLimit = limit
				best = acc
			}
		}
		if best == nil {
			s.recordSchedulerSelection(started, false, true, false, scanned)
			if shadowChecked {
				s.recordSchedulerShadow(shadowIndexedHit, false)
			}
			return nil
		}
		if s.accountHasBlockingCachedCooldown(best, policy) {
			continue
		}
		if s.tryAcquireAccount(best, bestLimit, true) {
			s.recordSchedulerSelection(started, false, true, true, scanned)
			if shadowChecked {
				s.recordSchedulerShadow(shadowIndexedHit, true)
			}
			if s.SchedulerEngine() == "indexed" {
				// slow-path fallback hit means the index missed an account (time-based recovery only); re-insert it to repair the index.
				s.fastSchedulerUpdate(best)
			}
			return best
		}
	}
	s.recordSchedulerSelection(started, false, true, false, scanned)
	if shadowChecked {
		s.recordSchedulerShadow(shadowIndexedHit, false)
	}
	return nil
}

func (s *Store) accountLazySelectable(acc *Account) bool {
	if acc == nil {
		return false
	}
	if atomic.LoadInt32(&acc.Disabled) != 0 {
		return false
	}
	if atomic.LoadInt32(&acc.DispatchPaused) != 0 || atomic.LoadInt32(&acc.IntelligencePaused) != 0 {
		return false
	}

	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.lazySelectableLocked(time.Now())
}

func (a *Account) lazySelectableLocked(now time.Time) bool {
	// same hard gate as isAvailableLocked: lazy mode is the other leg of the account selection entry point; omitting it would let
	// blocked accounts be selected under lazy deployments as usual (including the narrow 401 recovery exemption path).
	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.usageWindowBlocksFreshDispatchLocked(now) {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return false
	}
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	return strings.TrimSpace(a.AccessToken) != "" ||
		strings.TrimSpace(a.RefreshToken) != "" ||
		strings.TrimSpace(a.SessionToken) != ""
}

func (s *Store) ensureLazyDispatchReady(acc *Account) bool {
	if acc == nil {
		return false
	}
	if s.lazyNeedsDispatchRefresh(acc) {
		s.triggerLazyRefreshAsync(acc)
		return false
	}
	return acc.IsAvailable()
}

func (s *Store) lazyNeedsDispatchRefresh(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	hasRefreshCredential := strings.TrimSpace(acc.RefreshToken) != "" || strings.TrimSpace(acc.SessionToken) != ""
	acc.mu.RUnlock()
	return hasRefreshCredential && acc.NeedsRefresh()
}

func (s *Store) triggerLazyRefreshAsync(acc *Account) {
	if acc == nil || acc.DBID == 0 {
		return
	}
	dbID := acc.DBID
	if _, loaded := s.lazyRefreshInFlight.LoadOrStore(dbID, struct{}{}); loaded {
		return
	}
	if !s.startDBBackgroundTask(func(parent context.Context) {
		defer s.lazyRefreshInFlight.Delete(dbID)
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		if err := s.refreshAccount(ctx, acc); err != nil {
			log.Printf("[account %d] lazy mode warm-up refresh failed: %v", dbID, err)
		}
	}) {
		s.lazyRefreshInFlight.Delete(dbID)
	}
}

func (s *Store) lazyCanRefreshForMetadata(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.AccessToken == "" &&
		(strings.TrimSpace(acc.RefreshToken) != "" || strings.TrimSpace(acc.SessionToken) != "") &&
		acc.Status != StatusError &&
		acc.healthTierLocked() != HealthTierBanned
}

func (s *Store) acquireLazyCandidate(acc *Account, maxConcurrency int64) bool {
	if !s.ensureLazyDispatchReady(acc) {
		return false
	}
	_, _, _, limit := acc.schedulerSnapshot(maxConcurrency)
	if limit <= 0 {
		return false
	}
	return s.tryAcquireAccount(acc, limit, true)
}

func (s *Store) nextExcludingWithFilterLazy(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	for attempts := 0; attempts < 16; attempts++ {
		var best *Account
		var metadataRefreshCandidate *Account
		bestSchedulerPriority := minSchedulerPriority - 1
		bestPriority := -1
		bestDispatchScore := -math.MaxFloat64
		var bestReset5h time.Time
		var bestLoad int64 = math.MaxInt64
		maxConcurrency := s.maxConcurrency.Load()

		for _, acc := range s.accountSnapshotAccounts() {
			if exclude != nil && exclude[acc.DBID] {
				continue
			}
			if !acc.dispatchableForPolicy(policy) {
				continue
			}
			if policy == DispatchPolicyStandard && !s.accountLazySelectable(acc) {
				continue
			}
			if !s.accountAllowedForAPIKey(acc, apiKeyID) {
				continue
			}
			if filter != nil && !filter(acc) {
				if metadataRefreshCandidate == nil && s.lazyCanRefreshForMetadata(acc) {
					metadataRefreshCandidate = acc
				}
				continue
			}
			if s.lazyNeedsDispatchRefresh(acc) {
				s.triggerLazyRefreshAsync(acc)
				continue
			}

			load := accountOccupiedRequests(acc)
			tier, _, dispatchScore, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, policy)
			if limit <= 0 || load >= limit {
				continue
			}

			// account scheduling priority strictly precedes health tier and scheduling score (issue #358)
			schedulerPriority := acc.schedulerPriority()
			priority := tierPriority(tier)
			var reset5h time.Time
			resetOrder := 0
			if s.GetSchedulerMode() == SchedulerModeReset5hFirst && policy != DispatchPolicySpark {
				reset5h = acc.reset5hSchedulingDeadline(time.Now())
				resetOrder = compareReset5hDeadlines(reset5h, bestReset5h)
			}
			if schedulerPriority > bestSchedulerPriority ||
				(schedulerPriority == bestSchedulerPriority && (priority > bestPriority ||
					(priority == bestPriority && (resetOrder < 0 || resetOrder == 0 && (dispatchScore > bestDispatchScore ||
						(dispatchScore == bestDispatchScore && load < bestLoad) ||
						(dispatchScore == bestDispatchScore && load == bestLoad && fastRandN(2) == 0)))))) {
				bestSchedulerPriority = schedulerPriority
				bestPriority = priority
				bestDispatchScore = dispatchScore
				bestReset5h = reset5h
				bestLoad = load
				best = acc
			}
		}
		if best == nil {
			if metadataRefreshCandidate != nil && s.ensureLazyDispatchReady(metadataRefreshCandidate) {
				continue
			}
			return nil
		}
		if s.accountHasBlockingCachedCooldown(best, policy) {
			continue
		}
		if s.acquireLazyCandidate(best, maxConcurrency) {
			return best
		}
	}
	return nil
}

// BindSessionAffinity records the affinity between a session and an account/proxy.
func (s *Store) BindSessionAffinity(key string, account *Account, proxyURL string) {
	s.bindSessionAffinity(key, account, proxyURL)
}

// BindSessionAffinityWithGuard records the selected account unless selection
// identified it as a one-request capacity spillover. In that case the existing
// healthy binding remains authoritative and the fallback stays request-local.
func (s *Store) BindSessionAffinityWithGuard(key string, account *Account, proxyURL string, guard SessionAffinityGuard) {
	if guard.PreservesExisting() {
		return
	}
	s.bindSessionAffinity(key, account, proxyURL)
}

func (s *Store) bindSessionAffinity(key string, account *Account, proxyURL string) {
	if s == nil || account == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if !s.affinityProxyStillValid(account.DBID, proxyURL) {
		return
	}
	ttl := sessionAffinityTTL()
	now := time.Now()
	binding := sessionAffinity{
		accountID:    account.DBID,
		proxyURL:     proxyURL,
		boundAt:      now,
		requestCount: 0,
		lastUsedAt:   now,
		expiresAt:    now.Add(ttl),
	}

	s.sessionMu.Lock()
	if s.sessionBindings == nil {
		s.sessionBindings = make(map[string]sessionAffinity)
	}
	// bounded protection: an expired binding is only removed lazily when the same key is hit again, so bindings left behind after a conversation ends
	// are never queried again and would leak silently. Sticky keys are derived from content seeds (one per conversation).
	// the number of keys grows with the number of conversations; when the limit is exceeded, one full pass clears the expired entries.
	if len(s.sessionBindings) >= maxSessionBindings {
		for k, b := range s.sessionBindings {
			if !b.expiresAt.After(now) {
				delete(s.sessionBindings, k)
			}
		}
	}
	// consecutive Binds on the same account are treated as reuse: boundAt and requestCount are kept to preserve observation continuity;
	// when switching accounts, counting starts from 0 with the new binding. lastUsedAt is always the current time (it is being used).
	if existing, ok := s.sessionBindings[key]; ok && existing.accountID == account.DBID {
		binding.boundAt = existing.boundAt
		binding.requestCount = existing.requestCount
	}
	s.sessionBindings[key] = binding
	s.sessionMu.Unlock()

	if s.tokenCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := s.tokenCache.SetSessionAffinity(ctx, key, cache.SessionAffinityBinding{
			AccountID: binding.accountID,
			ProxyURL:  binding.proxyURL,
		}, ttl); err != nil {
			log.Printf("failed to write cached session stickiness: account=%d err=%v", binding.accountID, err)
		}
	}
}

// SessionAffinityAccountID returns the account ID currently bound to this affinity key (including bindings in the cross-process cache).
// Continuation requests use it to check whether "the bound account has already been excluded by this request"; if so, waiting for it to free up
// is pointless, and the caller can fall back to another account directly, saving a 30s wait.
func (s *Store) SessionAffinityAccountID(key string) (int64, bool) {
	if s == nil {
		return 0, false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, false
	}
	s.sessionMu.RLock()
	binding, ok := s.sessionBindings[key]
	s.sessionMu.RUnlock()
	if ok {
		return binding.accountID, true
	}
	if cached, cachedOK := s.getCachedSessionAffinity(key); cachedOK {
		return cached.accountID, true
	}
	return 0, false
}

// UnbindSessionAffinity removes a session binding when it still points to the failed account.
func (s *Store) UnbindSessionAffinity(key string, accountID int64) {
	if s == nil || accountID == 0 {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.sessionMu.Lock()
	if binding, ok := s.sessionBindings[key]; ok && binding.accountID == accountID {
		delete(s.sessionBindings, key)
	}
	s.sessionMu.Unlock()

	if s.tokenCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := s.tokenCache.DeleteSessionAffinity(ctx, key, accountID); err != nil {
			log.Printf("failed to delete cached session stickiness: account=%d err=%v", accountID, err)
		}
	}
}

// NextForSession prefers reusing the bound account and proxy, falling back to normal selection on failure.
func (s *Store) NextForSession(key string, apiKeyID int64, exclude map[int64]bool) (*Account, string) {
	return s.NextForSessionWithFilter(key, apiKeyID, exclude, nil)
}

// NextForSessionWithFilter prefers reusing the bound account and proxy, and applies the request-level account filter.
//
// affinity_mode sets the stickiness strength:
//   - off:     never reads bindings; always goes through the full selection policy
//   - bounded (default): the binding is valid, but is released by any of the following conditions
//   - the bound account is no longer in the healthy bucket (warm/risky/banned)
//   - the binding has been idle longer than sessionAffinityIdleEscape (default 10min, upstream cache has expired)
//   - strict:  keeps the old behavior entirely; only switches accounts on TTL expiry or an explicit Unbind
//
// When released, the binding is bypassed and the full selection policy runs (NextExcludingWithFilter); the next BindSessionAffinity
// re-establishes the binding.
func (s *Store) NextForSessionWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, false, SelectionOptions{Policy: DispatchPolicyStandard})
	return account, proxyURL
}

// NextForSessionWithDispatch prefers reusing the bound account and selects by usage policy.
func (s *Store) NextForSessionWithDispatch(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, false, SelectionOptions{Policy: policy})
	return account, proxyURL
}

// NextForSessionWithDispatchGuard is the binding-aware variant used by proxy
// request paths. The returned guard must be passed to BindSessionAffinityWithGuard
// after the attempt is selected or committed.
func (s *Store) NextForSessionWithDispatchGuard(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string, SessionAffinityGuard) {
	return s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, false, SelectionOptions{Policy: policy})
}

// NextForContinuationWithFilter preserves an existing account binding for a
// stateful upstream continuation. Turn state and previous_response_id belong
// to the account that created them, so bounded-affinity escape must not rotate
// accounts.
func (s *Store) NextForContinuationWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, true, SelectionOptions{Policy: DispatchPolicyStandard})
	return account, proxyURL
}

// NextForContinuationWithDispatch preserves a bound turn and applies a usage policy.
func (s *Store) NextForContinuationWithDispatch(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, true, SelectionOptions{Policy: policy})
	return account, proxyURL
}

// nextForSessionWithFilter is the unified implementation of session selection. When preserveBinding=true (continuation requests),
// its semantics are tightened to "only trust the bound account":
//   - ignores all escape conditions of affinity_mode=off and bounded (idle / health tier) —
//     a continuation id is only valid on the account that created it; switching accounts always causes previous_response_not_found,
//     so this deliberately overrides the operator's stickiness configuration;
//   - when the bound account is currently unavailable (over concurrency / cooling down / excluded by this request), returns nil instead of falling back
//     to another account. The caller decides whether to wait for it to free up, or strip the continuation id and downgrade to another account.
//
// If the binding itself does not exist, full selection still runs, as for a normal request; TTL expiry only affects normal requests,
// continuation requests with preserveBinding=true still keep the original account.
func (s *Store) nextForSessionWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, opts SelectionOptions) (*Account, string, SessionAffinityGuard) {
	if s == nil {
		return nil, "", SessionAffinityGuard{}
	}
	policy := opts.Policy
	key = strings.TrimSpace(key)
	if key == "" {
		return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, policy), "", SessionAffinityGuard{}
	}

	now := time.Now()
	s.sessionMu.RLock()
	binding, ok := s.sessionBindings[key]
	s.sessionMu.RUnlock()

	mode := s.GetAffinityMode()
	if mode == AffinityModeOff && !preserveBinding {
		return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, policy), "", SessionAffinityGuard{}
	}

	if ok {
		if !s.affinityProxyStillValid(binding.accountID, binding.proxyURL) {
			if preserveBinding {
				return s.takeByIDForContinuation(binding.accountID, apiKeyID, exclude, filter, key, policy), "", SessionAffinityGuard{}
			}
			s.UnbindSessionAffinity(key, binding.accountID)
			ok = false
		}
	}
	if ok {
		expired := !binding.expiresAt.After(now)
		// bounded mode adds an extra escape check: the account is unhealthy, or the binding has been idle until the upstream
		// prompt cache has certainly expired. Active sessions are not rotated (issue #584).
		escape := false
		if mode == AffinityModeBounded && !preserveBinding {
			if !s.affinityAccountStillHealthy(binding.accountID) {
				escape = true
			} else if !binding.lastUsedAt.IsZero() && now.Sub(binding.lastUsedAt) >= sessionAffinityIdleEscape() {
				escape = true
			}
		}

		if expired || escape {
			s.UnbindSessionAffinity(key, binding.accountID)
		} else {
			acc, capacityFull := s.takeByIDModeWithCapacity(binding.accountID, apiKeyID, exclude, filter, preserveBinding, key, policy)
			if acc != nil {
				// stickiness hit: record one reuse and refresh the idle clock
				s.sessionMu.Lock()
				if current, exists := s.sessionBindings[key]; exists && current.accountID == binding.accountID {
					current.requestCount++
					current.lastUsedAt = now
					s.sessionBindings[key] = current
				}
				s.sessionMu.Unlock()
				return acc, binding.proxyURL, SessionAffinityGuard{}
			}
			if preserveBinding {
				return nil, "", SessionAffinityGuard{}
			}
			if capacityFull {
				fallback := s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, opts)
				if fallback == nil {
					return nil, "", SessionAffinityGuard{}
				}
				log.Printf("session stickiness capacity overflow: bound account=%d is at full concurrency, this request borrows account=%d (upstream cache miss expected for this request)", binding.accountID, fallback.DBID)
				return fallback, "", SessionAffinityGuard{preserveAccountID: binding.accountID}
			}
		}
	}
	if binding, ok := s.getCachedSessionAffinity(key); ok {
		if !s.affinityProxyStillValid(binding.accountID, binding.proxyURL) {
			if preserveBinding {
				return s.takeByIDForContinuation(binding.accountID, apiKeyID, exclude, filter, key, policy), "", SessionAffinityGuard{}
			}
			s.UnbindSessionAffinity(key, binding.accountID)
			return s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, opts), "", SessionAffinityGuard{}
		}
		// the cross-process cached binding is also validated for account health using the bounded logic.
		if mode == AffinityModeBounded && !preserveBinding && !s.affinityAccountStillHealthy(binding.accountID) {
			// not reused; fall through to full selection
		} else {
			acc, capacityFull := s.takeByIDModeWithCapacity(binding.accountID, apiKeyID, exclude, filter, preserveBinding, key, policy)
			if acc != nil {
				s.sessionMu.Lock()
				if s.sessionBindings == nil {
					s.sessionBindings = make(map[string]sessionAffinity)
				}
				s.sessionBindings[key] = binding
				s.sessionMu.Unlock()
				return acc, binding.proxyURL, SessionAffinityGuard{}
			}
			if preserveBinding {
				return nil, "", SessionAffinityGuard{}
			}
			if capacityFull {
				fallback := s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, opts)
				if fallback == nil {
					return nil, "", SessionAffinityGuard{}
				}
				log.Printf("session stickiness capacity overflow: bound account=%d is at full concurrency, this request borrows account=%d (upstream cache miss expected for this request)", binding.accountID, fallback.DBID)
				return fallback, "", SessionAffinityGuard{preserveAccountID: binding.accountID}
			}
		}
	}

	return s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, opts), "", SessionAffinityGuard{}
}

// nextAccountForFreshAffinity selects an account for the first binding of a "new affinity key" (issue #484).
//
// With the hashing switch off, selection follows the scheduler's "highest score first" semantics, but that is exactly the root cause of clustering: when new keys arrive,
// the pool is mostly idle, and tiny differences in dispatchScore make every new key independently pick the same top account.
// When enabled, rendezvous (HRW) hashing is used instead: within the candidate tier of highest scheduling priority plus health tier, for
// each account compute hash(affinity key, account ID) and take the maximum; the same key always lands on the same account while the pool is unchanged
// on the same account (idempotent one-to-one binding), different keys spread evenly, and when the first choice is unavailable it deterministically falls to the next in hash order
// one, and adding or removing accounts only moves the affected keys. Across tiers, the operator's scheduling priority
// and health tier are still strictly respected; only within a tier are score differences ignored.
func (s *Store) nextAccountForFreshAffinity(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	return s.nextAccountForFreshAffinityWithDispatch(key, apiKeyID, exclude, filter, SelectionOptions{Policy: DispatchPolicyStandard})
}

// nextAccountForFreshAffinityWithDispatch is the variant of nextAccountForFreshAffinity with a
// DispatchPolicy: depending on the hashing switch, it falls back to HRW / scheduler / round-robin re-scan.
func (s *Store) nextAccountForFreshAffinityWithDispatch(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, opts SelectionOptions) *Account {
	if s == nil {
		return nil
	}
	policy := opts.Policy
	// Deadline priority governs fresh selection; an existing binding is handled
	// before reaching this method and remains intact.
	if !s.GetSessionAffinitySpread() || strings.TrimSpace(key) == "" ||
		(s.GetSchedulerMode() == SchedulerModeReset5hFirst && policy != DispatchPolicySpark) {
		return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, policy)
	}
	filter = s.withUsableEgressFilter(filter)
	if s.SchedulerEngine() == "indexed" {
		if scheduler := s.routingFastScheduler(apiKeyID); scheduler != nil {
			started := time.Now()
			acc := scheduler.AcquireForAffinityWithDispatch(affinityKeyHash(key), apiKeyID, exclude, filter, policy)
			if acc != nil && s.accountHasBlockingCachedCooldown(acc, policy) {
				s.Release(acc)
				acc = nil
			}
			s.recordSchedulerSelection(started, true, false, acc != nil, 0)
			if acc != nil {
				return acc
			}
			// when the affinity starting point is unavailable, degrade to normal selection (including cooldown retry and the slow-path fallback),
			// instead of giving up and letting the request sit in the 30-second wait.
			return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, policy)
		}
	}

	type affinityCandidate struct {
		acc               *Account
		schedulerPriority int64
		tierPriority      int
		limit             int64
		weight            uint64
	}
	maxConcurrency := s.maxConcurrency.Load()

	accounts := s.accountSnapshotAccounts()
	candidates := make([]affinityCandidate, 0, len(accounts))
	for _, acc := range accounts {
		if exclude != nil && exclude[acc.DBID] {
			continue
		}
		if !acc.dispatchableForPolicy(policy) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		if filter != nil && !filter(acc) {
			continue
		}
		load := accountOccupiedRequests(acc)
		tier, _, _, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, policy)
		if limit <= 0 || load >= limit {
			continue
		}
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(key))
		_, _ = hasher.Write([]byte{':'})
		_, _ = hasher.Write([]byte(strconv.FormatInt(acc.DBID, 10)))
		candidates = append(candidates, affinityCandidate{
			acc:               acc,
			schedulerPriority: acc.schedulerPriority(),
			tierPriority:      tierPriority(tier),
			limit:             limit,
			weight:            hasher.Sum64(),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	// keep only the highest (scheduling priority, health tier) layer.
	bestSchedPrio, bestTier := candidates[0].schedulerPriority, candidates[0].tierPriority
	for _, c := range candidates[1:] {
		if c.schedulerPriority > bestSchedPrio ||
			(c.schedulerPriority == bestSchedPrio && c.tierPriority > bestTier) {
			bestSchedPrio, bestTier = c.schedulerPriority, c.tierPriority
		}
	}
	layer := candidates[:0]
	for _, c := range candidates {
		if c.schedulerPriority == bestSchedPrio && c.tierPriority == bestTier {
			layer = append(layer, c)
		}
	}
	sort.Slice(layer, func(i, j int) bool { return layer[i].weight > layer[j].weight })

	for _, c := range layer {
		if s.accountHasBlockingCachedCooldown(c.acc, policy) {
			continue
		}
		if s.tryAcquireAccount(c.acc, c.limit, true) {
			return c.acc
		}
	}
	// when the whole tier cannot take it (concurrency / cooldown), fall back to regular scheduling; better to cluster temporarily than to reject the request.
	return s.NextExcludingWithDispatch(apiKeyID, exclude, filter, policy)
}

// affinityKeyHash is a stable, allocation-free FNV-1a hash. The indexed
// scheduler maps it to a bucket offset; a session binding preserves the chosen
// account after this one-time selection.
func affinityKeyHash(key string) uint64 {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	hash := offset64
	for i := 0; i < len(key); i++ {
		hash ^= uint64(key[i])
		hash *= prime64
	}
	return hash
}

// affinityProxyStillValid verifies that a sticky proxy still matches the
// account's current explicit proxy or the current fallback proxy configuration.
// This check applies to strict affinity too: affinity may keep an account sticky,
// but it must never resurrect a proxy that has been removed or disabled.
func (s *Store) affinityProxyStillValid(accountID int64, proxyURL string) bool {
	if s == nil || accountID == 0 {
		return false
	}
	proxyURL = strings.TrimSpace(proxyURL)

	s.mu.RLock()
	account := s.lookupByIDLocked(accountID)
	poolEnabled := s.proxyPoolEnabled
	poolHasEntries := len(s.proxyPool) > 0
	poolContainsProxy := false
	if poolHasEntries {
		if s.proxyPoolSet != nil {
			_, poolContainsProxy = s.proxyPoolSet[proxyURL]
		} else {
			// Backward-compatible fallback for tests or manually assembled stores.
			for _, candidate := range s.proxyPool {
				if proxyURL == strings.TrimSpace(candidate) {
					poolContainsProxy = true
					break
				}
			}
		}
	}
	globalProxy := strings.TrimSpace(s.globalProxy)
	s.mu.RUnlock()
	if account == nil {
		return false
	}

	if accountProxy := account.GetProxyURL(); accountProxy != "" {
		if s.managedProxyUnavailable(accountProxy) {
			return false
		}
		return proxyURL == accountProxy
	}
	// when the group proxy changes (list edited / group moved / group deleted), sessions stuck to the old proxy are invalidated here and rebound.
	if groupProxy := s.resolveGroupProxyForAccount(account); groupProxy != "" {
		return proxyURL == groupProxy
	}
	if poolEnabled && poolHasEntries {
		return poolContainsProxy
	}
	if poolEnabled {
		return false
	}
	return proxyURL == globalProxy
}

func buildProxyPoolSet(urls []string) map[string]struct{} {
	set := make(map[string]struct{}, len(urls))
	for _, proxyURL := range urls {
		if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
			set[proxyURL] = struct{}{}
		}
	}
	return set
}

// affinityAccountStillHealthy checks whether the account of a sticky binding is still in the healthy bucket.
// If it has dropped to warm/risky/banned or is not schedulable, bounded mode escapes and selects again.
func (s *Store) affinityAccountStillHealthy(accountID int64) bool {
	if s == nil || accountID == 0 {
		return false
	}
	s.mu.RLock()
	target := s.lookupByIDLocked(accountID)
	s.mu.RUnlock()
	if target == nil {
		return false
	}
	if atomic.LoadInt32(&target.Disabled) != 0 || (atomic.LoadInt32(&target.DispatchPaused) != 0 || atomic.LoadInt32(&target.IntelligencePaused) != 0) {
		return false
	}
	target.mu.RLock()
	defer target.mu.RUnlock()
	if target.Status == StatusError || target.Status == StatusCooldown {
		return false
	}
	tier := target.healthTierLocked()
	return tier == HealthTierHealthy
}

func (s *Store) getCachedSessionAffinity(key string) (sessionAffinity, bool) {
	if s == nil || s.tokenCache == nil {
		return sessionAffinity{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	binding, ok, err := s.tokenCache.GetSessionAffinity(ctx, key)
	if err != nil {
		log.Printf("failed to read cached session stickiness: %v", err)
		return sessionAffinity{}, false
	}
	if !ok || binding.AccountID == 0 {
		return sessionAffinity{}, false
	}
	now := time.Now()
	return sessionAffinity{
		accountID:  binding.AccountID,
		proxyURL:   strings.TrimSpace(binding.ProxyURL),
		boundAt:    now,
		lastUsedAt: now,
		expiresAt:  now.Add(sessionAffinityTTL()),
	}, true
}

func (s *Store) takeByIDExcluding(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	return s.takeByIDMode(id, apiKeyID, exclude, filter, false, "", DispatchPolicyStandard)
}

// TakePreferredAccountWithFilter attempts to acquire one specific account
// while applying the same availability, API-key, egress, filter, cooldown, and
// concurrency gates as normal scheduling. It intentionally bypasses session
// affinity policy so callers can preserve opaque upstream-owned state.
func (s *Store) TakePreferredAccountWithFilter(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	return s.TakePreferredAccountWithDispatch(id, apiKeyID, exclude, filter, DispatchPolicyStandard)
}

func (s *Store) TakePreferredAccountWithDispatch(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	return s.takeByIDMode(id, apiKeyID, exclude, filter, false, "", policy)
}

func (s *Store) takeByIDForContinuation(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, sessionKey string, policy DispatchPolicy) *Account {
	return s.takeByIDMode(id, apiKeyID, exclude, filter, true, sessionKey, policy)
}

func (s *Store) takeByIDMode(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, continuation bool, sessionKey string, policy DispatchPolicy) *Account {
	account, _ := s.takeByIDModeWithCapacity(id, apiKeyID, exclude, filter, continuation, sessionKey, policy)
	return account
}

// takeByIDModeWithCapacity distinguishes a pure concurrency miss from every
// other reason a bound account cannot be selected. Only the former is safe to
// treat as a one-request spillover without migrating the durable binding.
func (s *Store) takeByIDModeWithCapacity(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, continuation bool, sessionKey string, policy DispatchPolicy) (*Account, bool) {
	if s == nil || id == 0 {
		return nil, false
	}
	if exclude != nil && exclude[id] {
		return nil, false
	}

	s.mu.RLock()
	target := s.lookupByIDLocked(id)
	s.mu.RUnlock()
	if target == nil {
		return nil, false
	}
	continuationEligible := continuation && target.UsageLimitContinuationEligible()
	sparkEligible := policy == DispatchPolicySpark && target.SparkDispatchEligible()
	if s.GetLazyMode() {
		if !s.accountLazySelectable(target) && !continuationEligible && !sparkEligible {
			return nil, false
		}
	} else if !target.IsAvailable() && !continuationEligible && !sparkEligible {
		return nil, false
	}
	if s.accountHasCachedCooldown(target) {
		continuationEligible = continuation && target.UsageLimitContinuationEligible()
		sparkEligible = policy == DispatchPolicySpark && target.SparkDispatchEligible()
		if !continuationEligible && !sparkEligible {
			return nil, false
		}
	}
	if !s.accountAllowedForAPIKey(target, apiKeyID) {
		return nil, false
	}
	filter = s.withUsableEgressFilter(filter)
	if filter != nil && !filter(target) {
		return nil, false
	}

	maxConcurrency := s.maxConcurrency.Load()
	now := time.Now()
	if s.GetLazyMode() && !continuationEligible {
		if s.tryReclaimSessionSlot(target, sessionKey, true) {
			return target, false
		}
		if !s.ensureLazyDispatchReady(target) {
			return nil, false
		}
		_, _, _, limit := target.schedulerSnapshot(maxConcurrency)
		if limit <= 0 {
			return nil, false
		}
		acquired, failure := s.tryAcquireAccountWithFailure(target, limit, true)
		if !acquired {
			return nil, failure == accountAcquireFailureCapacity
		}
		return target, false
	}

	var limit int64
	var available bool
	if continuationEligible {
		_, _, limit, _, available = target.fastSchedulerSnapshotForContinuation(maxConcurrency, now)
	} else {
		_, _, limit, _, available = target.fastSchedulerSnapshotForPolicy(maxConcurrency, now, policy)
	}
	if !available || limit <= 0 {
		return nil, false
	}
	if s.tryReclaimSessionSlot(target, sessionKey, true) {
		return target, false
	}
	acquired, failure := s.tryAcquireAccountWithFailure(target, limit, true)
	if !acquired {
		return nil, failure == accountAcquireFailureCapacity
	}
	return target, false
}

// WaitForAvailable waits for an available account (request queuing with timeout)
func (s *Store) WaitForAvailable(ctx context.Context, timeout time.Duration, apiKeyID int64) *Account {
	acc, _ := s.WaitForSessionAvailable(ctx, "", timeout, apiKeyID, nil)
	return acc
}

// WaitForSessionAvailable waits for a session-preferred account and proxy pair.
func (s *Store) WaitForSessionAvailable(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool) (*Account, string) {
	return s.WaitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, nil)
}

// HasDispatchCandidate reports whether any locally known account could serve
// the request once a concurrency slot frees up. Saturation is ignored, so a
// false result means a queue wait with this exclude set can only succeed if an
// account is added or recovers; retry loops use it to recognize that their own
// request-local exclusions are what is blocking selection.
func (s *Store) HasDispatchCandidate(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, policy DispatchPolicy) bool {
	if preserveBinding {
		return s.hasContinuationCandidateWithDispatch(key, apiKeyID, exclude, filter, policy)
	}
	return s.hasDispatchCandidateWithDispatch(apiKeyID, exclude, filter, policy)
}

func (s *Store) hasDispatchCandidateWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) bool {
	return s.hasDispatchCandidateWithDispatch(apiKeyID, exclude, filter, DispatchPolicyStandard)
}

func (s *Store) hasDispatchCandidateWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) bool {
	if s == nil {
		return false
	}
	filter = s.withUsableEgressFilter(filter)

	maxConcurrency := s.maxConcurrency.Load()
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil {
			continue
		}
		if exclude != nil && exclude[acc.DBID] {
			continue
		}
		if !acc.dispatchableForPolicy(policy) {
			continue
		}
		if policy == DispatchPolicyStandard && s.GetLazyMode() && !s.accountLazySelectable(acc) {
			continue
		}
		if s.accountHasBlockingCachedCooldown(acc, policy) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		if filter != nil && !filter(acc) {
			continue
		}

		_, _, _, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, policy)
		if limit > 0 {
			return true
		}
	}
	return false
}

// HasUsageLimitedCandidateWithFilter distinguishes an exhausted account pool
// from a genuinely empty or unhealthy pool. Matching account, API-key, model,
// group, and egress constraints are still applied before reporting 429.
func (s *Store) HasUsageLimitedCandidateWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) bool {
	return s.HasUsageLimitedCandidateWithDispatch(apiKeyID, exclude, filter, DispatchPolicyStandard)
}

func (s *Store) HasUsageLimitedCandidateWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) bool {
	return s.UsageLimitedCandidateSummary(apiKeyID, exclude, filter, policy).Found
}

// UsageLimitedCandidateSummary describes why an otherwise matching pool is
// blocked by rate limits. Callers use it to tell a genuinely exhausted usage
// window (downstream should fail over) from a short account-wide throttle
// (downstream should just wait RetryAfter and try the same upstream again).
type UsageLimitedCandidateSummary struct {
	// Found is true when at least one matching account is usage-limited.
	Found bool
	// TransientOnly is true when every usage-limited candidate is blocked only
	// by a MarkTransientRateLimited freeze, not by a quota window.
	TransientOnly bool
	// RetryAfter is the shortest remaining transient freeze. Zero unless
	// TransientOnly.
	RetryAfter time.Duration
}

func (s *Store) UsageLimitedCandidateSummary(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) UsageLimitedCandidateSummary {
	var summary UsageLimitedCandidateSummary
	if s == nil {
		return summary
	}
	filter = s.withUsableEgressFilter(filter)
	now := time.Now()
	quotaLimited := false
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil || (exclude != nil && exclude[acc.DBID]) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		if filter != nil && !filter(acc) {
			continue
		}
		cachedCooldown := s.accountHasCachedCooldown(acc)
		usageLimited := acc.FreshDispatchUsageLimited() || acc.QuotaAutoPaused()
		if policy == DispatchPolicySpark {
			usageLimited = acc.SparkDispatchUsageLimited()
		}
		if cachedCooldown && !usageLimited {
			if policy == DispatchPolicySpark && acc.SparkDispatchEligible() {
				continue
			}
			if policy != DispatchPolicySpark {
				continue
			}
		}
		if !usageLimited {
			continue
		}
		summary.Found = true
		if remaining, ok := acc.transientRateLimitRemainingForPolicy(now, policy); ok {
			if summary.RetryAfter == 0 || remaining < summary.RetryAfter {
				summary.RetryAfter = remaining
			}
			continue
		}
		quotaLimited = true
	}
	if summary.Found && !quotaLimited {
		summary.TransientOnly = true
	} else {
		summary.RetryAfter = 0
	}
	return summary
}

// CapacitySaturatedCandidateSummary reports a pool that still has matching
// accounts, but every one of them is already at its concurrency limit.
// Disabled, cooling, filtered-out and zero-limit accounts are ignored, so a
// genuinely empty pool stays a no-available-account failure.
type CapacitySaturatedCandidateSummary struct {
	Found bool
}

func (s *Store) CapacitySaturatedCandidateSummary(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) CapacitySaturatedCandidateSummary {
	var summary CapacitySaturatedCandidateSummary
	if s == nil {
		return summary
	}
	filter = s.withUsableEgressFilter(filter)
	maxConcurrency := s.maxConcurrency.Load()
	saturated := 0
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil || (exclude != nil && exclude[acc.DBID]) {
			continue
		}
		if !acc.dispatchableForPolicy(policy) {
			continue
		}
		if policy == DispatchPolicyStandard && s.GetLazyMode() && !s.accountLazySelectable(acc) {
			continue
		}
		if s.accountHasBlockingCachedCooldown(acc, policy) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		if filter != nil && !filter(acc) {
			continue
		}
		_, _, _, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, policy)
		if limit <= 0 {
			continue
		}
		// A free slot means selection failed for some other reason.
		if accountOccupiedRequests(acc) < limit {
			return summary
		}
		saturated++
	}
	summary.Found = saturated > 0
	return summary
}

func (s *Store) hasContinuationCandidateWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) bool {
	return s.hasContinuationCandidateWithDispatch(key, apiKeyID, exclude, filter, DispatchPolicyStandard)
}

func (s *Store) hasContinuationCandidateWithDispatch(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) bool {
	accountID, ok := s.SessionAffinityAccountID(key)
	if !ok || accountID == 0 || (exclude != nil && exclude[accountID]) {
		return false
	}

	s.mu.RLock()
	acc := s.lookupByIDLocked(accountID)
	s.mu.RUnlock()
	if acc == nil || !s.accountAllowedForAPIKey(acc, apiKeyID) {
		return false
	}
	filter = s.withUsableEgressFilter(filter)
	if filter != nil && !filter(acc) {
		return false
	}

	continuationEligible := acc.UsageLimitContinuationEligible()
	sparkEligible := policy == DispatchPolicySpark && acc.SparkDispatchEligible()
	if !acc.IsAvailable() && !continuationEligible && !sparkEligible {
		return false
	}
	if s.accountHasCachedCooldown(acc) {
		continuationEligible = acc.UsageLimitContinuationEligible()
		sparkEligible = policy == DispatchPolicySpark && acc.SparkDispatchEligible()
		if !continuationEligible && !sparkEligible {
			return false
		}
	}

	maxConcurrency := s.maxConcurrency.Load()
	if continuationEligible {
		_, _, limit, _, available := acc.fastSchedulerSnapshotForContinuation(maxConcurrency, time.Now())
		return available && limit > 0
	}
	if sparkEligible {
		_, _, limit, _, available := acc.fastSchedulerSnapshotForSpark(maxConcurrency, time.Now())
		return available && limit > 0
	}
	_, _, limit, _, available := acc.fastSchedulerSnapshot(maxConcurrency, time.Now())
	return available && limit > 0
}

// WaitForSessionAvailableWithFilter waits for an account that satisfies the request-level filter.
func (s *Store) WaitForSessionAvailableWithFilter(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _, _ := s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, false, SelectionOptions{Policy: DispatchPolicyStandard})
	return account, proxyURL
}

// WaitForSessionAvailableWithDispatch queues and waits, under the given DispatchPolicy, for a session account that satisfies the filter,
// returning only the account and proxy address; when the capacity-overflow borrowing check must be kept, use
// WaitForSessionAvailableWithDispatchGuard instead.
func (s *Store) WaitForSessionAvailableWithDispatch(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string) {
	account, proxyURL, _, _ := s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, false, SelectionOptions{Policy: policy})
	return account, proxyURL
}

// WaitForSessionAvailableWithDispatchGuard is the binding-aware waiting path.
// It preserves the capacity-spillover decision made by the successful retry.
func (s *Store) WaitForSessionAvailableWithDispatchGuard(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string, SessionAffinityGuard) {
	account, proxyURL, guard, _ := s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, false, SelectionOptions{Policy: policy})
	return account, proxyURL, guard
}

// WaitForContinuationAvailableWithFilter waits for the account already bound
// to a stateful continuation instead of falling through to another account.
func (s *Store) WaitForContinuationAvailableWithFilter(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _, _ := s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, true, SelectionOptions{Policy: DispatchPolicyStandard})
	return account, proxyURL
}

// WaitForContinuationAvailableWithDispatch queues and waits, under the given DispatchPolicy, for the account bound to a continuation affinity key
// to be available (preserveBinding=true): if a binding exists, that account is locked and no other account is substituted; without a binding,
// the underlying Fresh selection semantics are used.
func (s *Store) WaitForContinuationAvailableWithDispatch(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, string) {
	account, proxyURL, _, _ := s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, true, SelectionOptions{Policy: policy})
	return account, proxyURL
}

// WaitForDispatchAvailable exposes queue admission failures to protocol handlers.
// preserveBinding retains the same account-only semantics as continuation waits.
func (s *Store) WaitForDispatchAvailable(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, policy DispatchPolicy, heartbeat ...SchedulerWaitHeartbeat) (*Account, string, SessionAffinityGuard, error) {
	return s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, preserveBinding, SelectionOptions{Policy: policy}, heartbeat...)
}

// waitForSessionAvailableWithFilter is the shared underlying implementation of all session wait entry points: within the timeout per opts,
// it repeatedly tries selection and parks on the availability hub to wait for a wake-up; when preserveBinding is true and a binding exists,
// that account is locked and no other account is substituted (without a binding, Fresh selection is used); when heartbeat is non-nil, heartbeats are written at the interval
// returned by the callback while waiting. When queue admission is rejected (e.g. the queue is full), the error is passed up unchanged so the protocol handler can tell overload from no candidates.
func (s *Store) waitForSessionAvailableWithFilter(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, preserveBinding bool, opts SelectionOptions, heartbeat ...SchedulerWaitHeartbeat) (*Account, string, SessionAffinityGuard, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := opts.Policy
	if timeout <= 0 || ctx.Err() != nil {
		return nil, "", SessionAffinityGuard{}, ctx.Err()
	}
	hasCandidate := func() bool {
		return s.HasDispatchCandidate(key, apiKeyID, exclude, filter, preserveBinding, policy)
	}
	// Indexed/shadow also wait on an empty snapshot: another replica may add
	// an account and notify this process through the durable outbox.
	if s.SchedulerEngine() == "legacy" && !hasCandidate() {
		return nil, "", SessionAffinityGuard{}, nil
	}
	bindingKey := strings.TrimSpace(key)
	boundAccountID := func() int64 {
		if !preserveBinding || bindingKey == "" {
			return 0
		}
		// This is only a wakeup hint. Do not add Redis reads to a blocked
		// continuation: selection still enforces cached owners when no local
		// binding is known, and an unknown hint accepts any notification.
		s.sessionMu.RLock()
		binding, ok := s.sessionBindings[bindingKey]
		s.sessionMu.RUnlock()
		if ok && binding.expiresAt.After(time.Now()) {
			return binding.accountID
		}
		return 0
	}
	hub := s.schedulerAvailabilityHub()
	waiter, err := hub.join(apiKeyID, boundAccountID(), exclude)
	metrics := s.schedulerMetrics
	if err != nil {
		if metrics != nil && errors.Is(err, ErrSchedulerQueueFull) {
			metrics.waitRejected.Add(1)
			if errors.Is(err, ErrSchedulerKeyQueueFull) {
				metrics.waitRejectedPerKey.Add(1)
			}
		}
		return nil, "", SessionAffinityGuard{}, err
	}
	defer hub.finish(waiter, false, true, 0)
	if metrics != nil {
		metrics.waitStarted.Add(1)
		metrics.waiters.Add(1)
		started := time.Now()
		defer func() {
			metrics.waiters.Add(-1)
			metrics.recordWaitDuration(time.Since(started))
		}()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	expires := time.Now().Add(timeout)
	var heartbeatTimer *time.Timer
	var heartbeatC <-chan time.Time
	defer func() {
		if heartbeatTimer != nil {
			heartbeatTimer.Stop()
		}
	}()
	resetHeartbeat := func() error {
		if len(heartbeat) == 0 || heartbeat[0] == nil {
			return nil
		}
		delay, err := heartbeat[0]()
		if err != nil {
			return err
		}
		if delay <= 0 {
			delay = time.Millisecond
		}
		if heartbeatTimer == nil {
			heartbeatTimer = time.NewTimer(delay)
			heartbeatC = heartbeatTimer.C
		} else {
			heartbeatTimer.Reset(delay)
		}
		return nil
	}
	for {
		// Check both before and after admission. Cancellation must never leak
		// an acquired slot, including when ready and Done fire together.
		if ctx.Err() != nil {
			if metrics != nil {
				metrics.waitCanceled.Add(1)
			}
			return nil, "", SessionAffinityGuard{}, ctx.Err()
		}
		if !time.Now().Before(expires) {
			if metrics != nil {
				metrics.waitTimeouts.Add(1)
			}
			return nil, "", SessionAffinityGuard{}, nil
		}
		var acc *Account
		var proxyURL string
		var guard SessionAffinityGuard
		acc, proxyURL, guard = s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, preserveBinding, opts)
		if acc != nil {
			if ctx.Err() != nil || !time.Now().Before(expires) {
				s.Release(acc)
				if metrics != nil {
					if ctx.Err() != nil {
						metrics.waitCanceled.Add(1)
					} else {
						metrics.waitTimeouts.Add(1)
					}
				}
				return nil, "", SessionAffinityGuard{}, ctx.Err()
			}
			hub.finish(waiter, true, true, 0)
			if metrics != nil {
				metrics.waitGranted.Add(1)
			}
			return acc, proxyURL, guard, nil
		}
		if s.SchedulerEngine() == "legacy" && !hasCandidate() {
			return nil, "", SessionAffinityGuard{}, nil
		}
		hub.finish(waiter, false, false, boundAccountID())
		if heartbeatTimer == nil {
			if err := resetHeartbeat(); err != nil {
				return nil, "", SessionAffinityGuard{}, err
			}
		}
	waitLoop:
		for {
			select {
			case <-heartbeatC:
				if err := resetHeartbeat(); err != nil {
					return nil, "", SessionAffinityGuard{}, err
				}
			case <-waiter.ready:
				if metrics != nil {
					metrics.waitWakeups.Add(1)
				}
				break waitLoop
			case <-ctx.Done():
				if metrics != nil {
					metrics.waitCanceled.Add(1)
				}
				return nil, "", SessionAffinityGuard{}, ctx.Err()
			case <-deadline.C:
				if metrics != nil {
					metrics.waitTimeouts.Add(1)
				}
				return nil, "", SessionAffinityGuard{}, nil
			case <-hub.done:
				return nil, "", SessionAffinityGuard{}, context.Canceled
			}
		}
	}
}

// SetSessionSlotBuffer updates the grace period during which a successfully
// completed request keeps its account slot reserved for the same session.
func (s *Store) SetSessionSlotBuffer(duration time.Duration) {
	if s == nil {
		return
	}
	if duration < 0 {
		duration = 0
	}
	if duration > maxSessionSlotBuffer {
		duration = maxSessionSlotBuffer
	}
	s.sessionSlotBufferNS.Store(int64(duration))
}

func (s *Store) GetSessionSlotBuffer() time.Duration {
	if s == nil {
		return 0
	}
	return time.Duration(s.sessionSlotBufferNS.Load())
}

func (s *Store) SessionSlotBufferEnabled() bool {
	return s != nil && s.sessionSlotBufferEnabled.Load()
}

// SetSessionSlotBufferEnabled hot-updates buffering. Disabling releases all
// reservations immediately without touching live requests.
func (s *Store) SetSessionSlotBufferEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.sessionSlotBufferEnabled.Store(enabled)
	if enabled {
		return
	}

	s.sessionMu.Lock()
	releasedByAccount := make(map[int64]int64, len(s.sessionSlotReservations))
	for accountID, bySession := range s.sessionSlotReservations {
		for _, reservations := range bySession {
			releasedByAccount[accountID] += int64(len(reservations))
		}
	}
	s.sessionSlotReservations = make(map[int64]map[string][]uint64)
	s.sessionMu.Unlock()

	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil {
			continue
		}
		if count := releasedByAccount[acc.DBID]; count > 0 {
			atomicSubtractFloorZero(&acc.OccupiedRequests, count)
		}
	}
	if len(releasedByAccount) > 0 {
		s.notifySchedulerAvailability()
	}
}

// ReleaseForSession converts one live slot into a short reservation owned by
// sessionKey. ActiveRequests drops immediately while OccupiedRequests stays
// unchanged until the owner reclaims the slot or the reservation expires.
func (s *Store) ReleaseForSession(acc *Account, sessionKey string) {
	if s == nil || acc == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	buffer := s.GetSessionSlotBuffer()
	if sessionKey == "" || !s.SessionSlotBufferEnabled() || buffer <= 0 || s.GetAffinityMode() == AffinityModeOff {
		s.Release(acc)
		return
	}

	s.sessionMu.Lock()
	if !s.SessionSlotBufferEnabled() {
		s.sessionMu.Unlock()
		s.Release(acc)
		return
	}
	if s.sessionSlotReservations == nil {
		s.sessionSlotReservations = make(map[int64]map[string][]uint64)
	}
	s.sessionSlotSequence++
	reservationID := s.sessionSlotSequence
	bySession := s.sessionSlotReservations[acc.DBID]
	if bySession == nil {
		bySession = make(map[string][]uint64)
		s.sessionSlotReservations[acc.DBID] = bySession
	}
	bySession[sessionKey] = append(bySession[sessionKey], reservationID)
	atomicDecrementIfPositive(&acc.ActiveRequests)
	s.sessionMu.Unlock()

	time.AfterFunc(buffer, func() {
		s.expireSessionSlot(acc, sessionKey, reservationID)
	})
}

// ReleaseForSessionWithGuard avoids reserving capacity on a temporary fallback.
// The durable owner remains a different account, so a fallback reservation
// cannot be reclaimed through the normal bound-account path and would only
// suppress usable capacity until the buffer expires.
func (s *Store) ReleaseForSessionWithGuard(acc *Account, sessionKey string, guard SessionAffinityGuard) {
	if guard.PreservesExisting() {
		s.Release(acc)
		return
	}
	s.ReleaseForSession(acc, sessionKey)
}

func (s *Store) expireSessionSlot(acc *Account, sessionKey string, reservationID uint64) {
	if s == nil || acc == nil {
		return
	}
	released := false
	s.sessionMu.Lock()
	if bySession := s.sessionSlotReservations[acc.DBID]; bySession != nil {
		reservations := bySession[sessionKey]
		for i, id := range reservations {
			if id != reservationID {
				continue
			}
			reservations = append(reservations[:i], reservations[i+1:]...)
			released = true
			break
		}
		if len(reservations) == 0 {
			delete(bySession, sessionKey)
		} else {
			bySession[sessionKey] = reservations
		}
		if len(bySession) == 0 {
			delete(s.sessionSlotReservations, acc.DBID)
		}
	}
	s.sessionMu.Unlock()
	if released {
		atomicDecrementIfPositive(&acc.OccupiedRequests)
		s.notifySchedulerAccountAvailability(acc, false)
	}
}

func (s *Store) tryReclaimSessionSlot(acc *Account, sessionKey string, updateSchedulerOnLimit bool) bool {
	if s == nil || accountDispatchBlocked(acc) || strings.TrimSpace(sessionKey) == "" || !s.SessionSlotBufferEnabled() || s.GetSessionSlotBuffer() <= 0 {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)

	reclaimed := false
	s.sessionMu.Lock()
	if bySession := s.sessionSlotReservations[acc.DBID]; bySession != nil {
		reservations := bySession[sessionKey]
		if len(reservations) > 0 {
			reservations = reservations[1:]
			if len(reservations) == 0 {
				delete(bySession, sessionKey)
			} else {
				bySession[sessionKey] = reservations
			}
			if len(bySession) == 0 {
				delete(s.sessionSlotReservations, acc.DBID)
			}
			acc.ActiveRequests.Add(1)
			reclaimed = true
		}
	}
	s.sessionMu.Unlock()
	if !reclaimed {
		return false
	}
	if accountDispatchBlocked(acc) {
		if releaseOccupiedAccountSlot(acc) {
			s.notifySchedulerAccountAvailability(acc, false)
		}
		return false
	}

	now := time.Now()
	dispatchReservation := acc.reserveDispatchCount(now)
	if !dispatchReservation.Allowed {
		if releaseOccupiedAccountSlot(acc) {
			s.notifySchedulerAccountAvailability(acc, false)
		}
		s.markDispatchCountLimitCooldown(acc, dispatchReservation.ResetAt, updateSchedulerOnLimit)
		return false
	}
	acc.TotalRequests.Add(1)
	acc.LastUsedAt.Store(now.UnixNano())
	if dispatchReservation.HitLimit {
		s.markDispatchCountLimitCooldown(acc, dispatchReservation.ResetAt, updateSchedulerOnLimit)
	}
	return true
}

// Release immediately frees a live slot. Failures, retries and requests
// without a usable affinity key always use this path.
func (s *Store) Release(acc *Account) {
	if acc == nil {
		return
	}
	if releaseOccupiedAccountSlot(acc) {
		s.notifySchedulerAccountAvailability(acc, false)
	}
}

// SetMaxConcurrency dynamically updates the per-account concurrency limit
func (s *Store) SetMaxConcurrency(n int) {
	s.maxConcurrency.Store(int64(n))
	// Update existing scheduler's base limit in-place before full rebuild.
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.SetBaseLimit(int64(n))
	}
	s.recomputeAllAccountSchedulerState()
	s.rebuildFastScheduler()
}

// GetMaxConcurrency gets the current per-account concurrency limit
func (s *Store) GetMaxConcurrency() int {
	return int(s.maxConcurrency.Load())
}

// SetMaxRetries dynamically updates the maximum number of retries
func (s *Store) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	s.maxRetries.Store(int64(n))
}

// GetMaxRetries gets the current maximum number of retries
func (s *Store) GetMaxRetries() int {
	return int(s.maxRetries.Load())
}

func (s *Store) SetMaxRateLimitRetries(n int) {
	if n < 0 {
		n = 0
	}
	s.maxRateLimitRetries.Store(int64(n))
}

func (s *Store) GetMaxRateLimitRetries() int {
	return int(s.maxRateLimitRetries.Load())
}

// normalizeRetryIntervalMS clamps the retry interval to 0-30000ms (0 = retry immediately).
func normalizeRetryIntervalMS(ms int) int {
	if ms < 0 {
		return 0
	}
	if ms > 30000 {
		return 30000
	}
	return ms
}

// SetRetryIntervalMS dynamically updates the retry interval (milliseconds).
func (s *Store) SetRetryIntervalMS(ms int) {
	if s == nil {
		return
	}
	s.retryIntervalMS.Store(int64(normalizeRetryIntervalMS(ms)))
}

// GetRetryIntervalMS gets the current retry interval (milliseconds); 0 = retry immediately.
func (s *Store) GetRetryIntervalMS() int {
	if s == nil {
		return 0
	}
	return int(s.retryIntervalMS.Load())
}

// SetTransportRetryPolicy dynamically updates the transport error retry policy (rotate / sticky).
func (s *Store) SetTransportRetryPolicy(policy string) {
	if s == nil {
		return
	}
	s.transportRetryPolicy.Store(database.NormalizeTransportRetryPolicy(policy))
}

// GetTransportRetryPolicy gets the transport error retry policy; defaults to rotate (switch accounts, the old behavior).
func (s *Store) GetTransportRetryPolicy() string {
	if s == nil {
		return "rotate"
	}
	if v, ok := s.transportRetryPolicy.Load().(string); ok && v != "" {
		return v
	}
	return "rotate"
}

// SetContinuousRetryPolicy hot-updates the continuous retry policy for upstream errors.
func (s *Store) SetContinuousRetryPolicy(policy database.ContinuousRetryPolicy) {
	if s == nil {
		return
	}
	s.continuousRetryPolicy.Store(database.NormalizeContinuousRetryPolicy(policy))
}

// GetContinuousRetryPolicy returns a value snapshot of the current continuous retry policy for upstream errors.
func (s *Store) GetContinuousRetryPolicy() database.ContinuousRetryPolicy {
	if s == nil {
		return database.DefaultContinuousRetryPolicy()
	}
	if value, ok := s.continuousRetryPolicy.Load().(database.ContinuousRetryPolicy); ok {
		return database.NormalizeContinuousRetryPolicy(value)
	}
	return database.DefaultContinuousRetryPolicy()
}

// SetCodexFingerprintDefaultMode dynamically updates the default fingerprint convergence mode for newly imported accounts.
func (s *Store) SetCodexFingerprintDefaultMode(mode string) {
	if s == nil {
		return
	}
	s.codexFingerprintDefaultMode.Store(NormalizeCodexFingerprintMode(mode))
}

// GetCodexFingerprintDefaultMode gets the default fingerprint convergence mode for newly imported accounts; defaults to off.
func (s *Store) GetCodexFingerprintDefaultMode() string {
	if s == nil {
		return CodexFingerprintModeOff
	}
	if v, ok := s.codexFingerprintDefaultMode.Load().(string); ok && v != "" {
		return v
	}
	return CodexFingerprintModeOff
}

// GetAllowRemoteMigration gets whether remote migration is allowed
func (s *Store) GetAllowRemoteMigration() bool {
	return s.allowRemoteMigration.Load()
}

// SetAllowRemoteMigration sets whether remote migration is allowed
func (s *Store) SetAllowRemoteMigration(enabled bool) {
	s.allowRemoteMigration.Store(enabled)
}

// SetTestModel dynamically updates the model used for test connections
func (s *Store) SetTestModel(m string) {
	s.testModel.Store(m)
}

// GetTestModel gets the current model used for test connections
func (s *Store) GetTestModel() string {
	if v, ok := s.testModel.Load().(string); ok && v != "" {
		return v
	}
	return DefaultTestModel
}

// SetTestContent dynamically updates connection test input text.
func (s *Store) SetTestContent(content string) {
	s.testContent.Store(NormalizeTestContent(content))
}

// GetTestContent returns the input text used by connection tests.
func (s *Store) GetTestContent() string {
	if v, ok := s.testContent.Load().(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return DefaultTestContent
}

// SetTestConcurrency dynamically updates the batch test concurrency
func (s *Store) SetTestConcurrency(n int) {
	s.testConcurrency.Store(int64(n))
}

// GetTestConcurrency gets the current batch test concurrency
func (s *Store) GetTestConcurrency() int {
	return int(s.testConcurrency.Load())
}

// GetBackgroundRefreshIntervalMinutes gets the background check interval (minutes).
func (s *Store) GetBackgroundRefreshIntervalMinutes() int {
	return int(s.GetBackgroundRefreshInterval() / time.Minute)
}

// GetUsageProbeMaxAgeMinutes gets the maximum cache age of the usage probe (minutes).
func (s *Store) GetUsageProbeMaxAgeMinutes() int {
	return int(s.GetUsageProbeMaxAge() / time.Minute)
}

// GetRecoveryProbeIntervalMinutes gets the minimum interval of the recovery probe (minutes).
func (s *Store) GetRecoveryProbeIntervalMinutes() int {
	return int(s.GetRecoveryProbeInterval() / time.Minute)
}

// SetModelMapping dynamically updates the model mapping JSON
func (s *Store) SetModelMapping(mapping string) {
	s.modelMapping.Store(mapping)
}

// GetModelMapping gets the current model mapping JSON
func (s *Store) GetModelMapping() string {
	if v, ok := s.modelMapping.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// SetCodexModelMapping dynamically updates the Codex model mapping JSON
func (s *Store) SetCodexModelMapping(mapping string) {
	s.codexModelMapping.Store(mapping)
}

// GetCodexModelMapping gets the current Codex model mapping JSON
func (s *Store) GetCodexModelMapping() string {
	if v, ok := s.codexModelMapping.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// SetPayloadRules dynamically updates the payload request-body rewrite rules JSON
func (s *Store) SetPayloadRules(rules string) {
	s.payloadRules.Store(rules)
}

// GetPayloadRules gets the current payload request-body rewrite rules JSON
func (s *Store) GetPayloadRules() string {
	if v, ok := s.payloadRules.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// SetReasoningEffortModels dynamically updates the JSON array of model aliases with reasoning effort.
func (s *Store) SetReasoningEffortModels(value string) {
	s.reasoningEffortModels.Store(value)
}

// GetReasoningEffortModels gets the current JSON array of model aliases with reasoning effort.
func (s *Store) GetReasoningEffortModels() string {
	if v, ok := s.reasoningEffortModels.Load().(string); ok && v != "" {
		return v
	}
	return "[]"
}

// GetSchedulerMode gets the current scheduling mode
func (s *Store) GetSchedulerMode() string {
	if v, ok := s.schedulerMode.Load().(string); ok && v != "" {
		return v
	}
	return "round_robin"
}

// SetSchedulerMode sets the scheduling mode and propagates it to FastScheduler
func (s *Store) SetSchedulerMode(mode string) {
	switch mode {
	case "round_robin", "remaining_quota", "fill_first", SchedulerModeReset5hFirst:
		// ok
	default:
		mode = "round_robin"
	}
	s.schedulerMode.Store(mode)
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.SetSchedulerMode(mode)
	}
	s.invalidateRoutingSchedulers()
}

// GetAffinityMode gets the current session affinity mode (bounded / off / strict)
func (s *Store) GetAffinityMode() string {
	if v, ok := s.affinityMode.Load().(string); ok && v != "" {
		return v
	}
	return AffinityModeBounded
}

// SetAffinityMode sets the session affinity mode
func (s *Store) SetAffinityMode(mode string) {
	switch mode {
	case AffinityModeBounded, AffinityModeOff, AffinityModeStrict:
		// ok
	default:
		mode = AffinityModeBounded
	}
	s.affinityMode.Store(mode)
}

// GetSessionAffinitySpread reports whether new affinity keys are spread by HRW hashing when selecting accounts (issue #484).
func (s *Store) GetSessionAffinitySpread() bool {
	if s == nil {
		return false
	}
	return s.affinitySpreadEnabled.Load()
}

// SetSessionAffinitySpread hot-updates the hashed binding switch.
func (s *Store) SetSessionAffinitySpread(enabled bool) {
	if s == nil {
		return
	}
	s.affinitySpreadEnabled.Store(enabled)
}

type promptFilterConfigState struct {
	Config      promptfilter.Config
	AdvancedRaw string
}

func promptFilterConfigFromSettings(settings *database.SystemSettings) (promptfilter.Config, string) {
	cfg := promptfilter.DefaultConfig()
	advancedRaw := promptfilter.MarshalAdvancedConfig(cfg.Advanced)
	if settings == nil {
		return cfg, advancedRaw
	}
	cfg.Enabled = settings.PromptFilterEnabled
	cfg.Mode = settings.PromptFilterMode
	cfg.Threshold = settings.PromptFilterThreshold
	cfg.StrictThreshold = settings.PromptFilterStrictThreshold
	cfg.StrictTerminalEnabled = settings.PromptFilterStrictTerminalEnabled
	if document, err := promptfilter.ParseAdvancedConfigDocument(settings.PromptFilterAdvancedConfig); err == nil {
		cfg.Advanced = document.Effective
		advancedRaw = document.Raw
	}
	cfg.LogMatches = settings.PromptFilterLogMatches
	cfg.MaxTextLength = settings.PromptFilterMaxTextLength
	cfg.SensitiveWords = settings.PromptFilterSensitiveWords
	if patterns, err := promptfilter.ParseCustomPatterns(settings.PromptFilterCustomPatterns); err == nil {
		cfg.CustomPatterns = patterns
	}
	if disabled, err := promptfilter.ParseDisabledPatterns(settings.PromptFilterDisabledPatterns); err == nil {
		cfg.DisabledPatterns = disabled
	}
	cfg.Review = promptfilter.ReviewConfig{
		Enabled:        settings.PromptFilterReviewEnabled,
		APIKey:         settings.PromptFilterReviewAPIKey,
		BaseURL:        settings.PromptFilterReviewBaseURL,
		Model:          settings.PromptFilterReviewModel,
		TimeoutSeconds: settings.PromptFilterReviewTimeoutSeconds,
		FailClosed:     settings.PromptFilterReviewFailClosed,
		Adapter:        cfg.Advanced.ReviewAdapter,
	}
	return promptfilter.NormalizeConfig(cfg), advancedRaw
}

func (s *Store) SetPromptFilterConfig(cfg promptfilter.Config) {
	normalized := promptfilter.NormalizeConfig(cfg)
	// Persisted custom rules are administrator-controlled input and may come
	// from an older build that accepted invalid or over-broad regexes. Sanitize
	// only when publishing a Store snapshot so request-time NormalizeConfig does
	// not repeatedly compile/audit every rule.
	var quarantined []promptfilter.PatternQuarantine
	normalized.CustomPatterns, quarantined = promptfilter.SanitizeCustomPatterns(normalized.CustomPatterns)
	logPromptFilterPatternQuarantines(quarantined)
	advancedRaw := promptfilter.MarshalAdvancedConfig(normalized.Advanced)
	if current, ok := s.promptFilterConfig.Load().(promptFilterConfigState); ok {
		if merged, err := promptfilter.MarshalAdvancedConfigDocument(current.AdvancedRaw, normalized.Advanced); err == nil {
			advancedRaw = merged
		}
	}
	s.promptFilterConfig.Store(promptFilterConfigState{Config: normalized, AdvancedRaw: advancedRaw})
}

// SetPromptFilterConfigWithAdvancedRaw atomically publishes the normalized
// runtime configuration together with its forward-compatible persisted JSON.
// The caller must persist successfully before invoking this method.
func (s *Store) SetPromptFilterConfigWithAdvancedRaw(cfg promptfilter.Config, raw string) error {
	normalized := promptfilter.NormalizeConfig(cfg)
	var quarantined []promptfilter.PatternQuarantine
	normalized.CustomPatterns, quarantined = promptfilter.SanitizeCustomPatterns(normalized.CustomPatterns)
	logPromptFilterPatternQuarantines(quarantined)
	advancedRaw, err := promptfilter.MarshalAdvancedConfigDocument(raw, normalized.Advanced)
	if err != nil {
		return err
	}
	s.promptFilterConfig.Store(promptFilterConfigState{Config: normalized, AdvancedRaw: advancedRaw})
	return nil
}

func logPromptFilterPatternQuarantines(items []promptfilter.PatternQuarantine) {
	for _, item := range items {
		log.Printf("prompt filter: custom rule quarantined index=%d name=%q code=%s message=%s", item.Index, item.Name, item.Code, item.Message)
	}
}

func (s *Store) GetPromptFilterConfig() promptfilter.Config {
	if state, ok := s.promptFilterConfig.Load().(promptFilterConfigState); ok {
		return promptfilter.NormalizeConfig(state.Config)
	}
	return promptfilter.DefaultConfig()
}

// GetPromptFilterConfigSnapshot returns the immutable, already-normalized
// runtime snapshot published by SetPromptFilterConfig. Request hot paths may
// read it without rebuilding maps and slices, but callers must never mutate
// nested maps or slices on the returned value. Administrative edit paths must
// continue to use GetPromptFilterConfig, which returns an independent copy.
func (s *Store) GetPromptFilterConfigSnapshot() promptfilter.Config {
	if state, ok := s.promptFilterConfig.Load().(promptFilterConfigState); ok {
		return state.Config
	}
	return promptfilter.DefaultConfig()
}

// GetPromptFilterAdvancedConfig returns the JSON document exposed through the
// settings API. It contains normalized known fields and retains unknown fields
// loaded from or accepted into persistent settings.
func (s *Store) GetPromptFilterAdvancedConfig() string {
	if state, ok := s.promptFilterConfig.Load().(promptFilterConfigState); ok && strings.TrimSpace(state.AdvancedRaw) != "" {
		return state.AdvancedRaw
	}
	return promptfilter.MarshalAdvancedConfig(s.GetPromptFilterConfig().Advanced)
}

// SetIgnoreUsageLimitStatus updates the global default and immediately
// recomputes accounts that inherit it. Existing explicit cooldowns are kept
// until a real Responses success confirms recovery.
func (s *Store) SetIgnoreUsageLimitStatus(enabled bool) {
	if s == nil {
		return
	}
	s.ignoreUsageLimitStatus.Store(enabled)
	for _, acc := range s.accountSnapshotAccounts() {
		acc.mu.Lock()
		acc.recomputeEffectiveIgnoreUsageLimitStatus(enabled)
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
}

func (s *Store) IgnoreUsageLimitStatus() bool {
	return s != nil && s.ignoreUsageLimitStatus.Load()
}

func (s *Store) SetGlobalAutoPauseThresholds(t5h, t7d float64) {
	s.mu.Lock()
	s.globalAutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(t5h)
	s.globalAutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(t7d)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetGlobalAutoPause5hThreshold() float64 {
	s.mu.RLock()
	v := s.globalAutoPause5hThreshold
	s.mu.RUnlock()
	return v
}

func (s *Store) GetGlobalAutoPause7dThreshold() float64 {
	s.mu.RLock()
	v := s.globalAutoPause7dThreshold
	s.mu.RUnlock()
	return v
}

func (s *Store) SetAutoPause5hGuardBandPercent(value float64) {
	s.mu.Lock()
	s.autoPause5hGuardBandPercent = normalizeAutoPause5hGuardBandPercent(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetAutoPause5hGuardBandPercent() float64 {
	s.mu.RLock()
	v := s.autoPause5hGuardBandPercent
	s.mu.RUnlock()
	return v
}

func (s *Store) SetAutoPause5hGuardConcurrency(value int) {
	s.mu.Lock()
	s.autoPause5hGuardConcurrency = normalizeAutoPause5hGuardConcurrency(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetAutoPause5hGuardConcurrency() int {
	s.mu.RLock()
	v := s.autoPause5hGuardConcurrency
	s.mu.RUnlock()
	return v
}

func (s *Store) SetSmartPacingEnabled(value bool) {
	s.mu.Lock()
	s.smartPacingEnabled = value
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingEnabled() bool {
	s.mu.RLock()
	v := s.smartPacingEnabled
	s.mu.RUnlock()
	return v
}

func (s *Store) SetSmartPacingMinConcurrency(value int) {
	s.mu.Lock()
	s.smartPacingMinConcurrency = normalizeSmartPacingMinConcurrency(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingMinConcurrency() int {
	s.mu.RLock()
	v := s.smartPacingMinConcurrency
	s.mu.RUnlock()
	if v < 1 {
		v = defaultSmartPacingMinConcurrency
	}
	return v
}

func (s *Store) SetSmartPacingWindows(value string) {
	s.mu.Lock()
	s.smartPacingWindows = normalizeSmartPacingWindows(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingWindows() string {
	s.mu.RLock()
	v := s.smartPacingWindows
	s.mu.RUnlock()
	if v == "" {
		return "5h,7d"
	}
	return v
}

func (s *Store) SetGroupAutoPauseThresholds(groupID int64, t5h, t7d float64) {
	s.groupAutoPauseThresholds.Store(groupID, [2]float64{
		normalizeQuotaAutoPauseThreshold(t5h),
		normalizeQuotaAutoPauseThreshold(t7d),
	})
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) DeleteGroupAutoPauseThresholds(groupID int64) {
	s.groupAutoPauseThresholds.Delete(groupID)
}

// SetGroupName records/updates the group ID-to-name mapping (called when a group is created or renamed).
func (s *Store) SetGroupName(groupID int64, name string) {
	if s == nil || groupID <= 0 {
		return
	}
	s.groupNames.Store(groupID, strings.TrimSpace(name))
}

// DeleteGroupName removes the group ID-to-name mapping (called when a group is deleted).
func (s *Store) DeleteGroupName(groupID int64) {
	if s == nil {
		return
	}
	s.groupNames.Delete(groupID)
}

// ResolveGroupNames resolves a list of group IDs into a list of group names; missing items (not loaded / deleted) are skipped.
func (s *Store) ResolveGroupNames(groupIDs []int64) []string {
	if s == nil || len(groupIDs) == 0 {
		return nil
	}
	names := make([]string, 0, len(groupIDs))
	for _, id := range groupIDs {
		if v, ok := s.groupNames.Load(id); ok {
			if name, _ := v.(string); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func (s *Store) GetGroupAutoPauseThresholds(groupID int64) (float64, float64) {
	return s.getGroupAutoPauseThresholds(groupID)
}

func (s *Store) getGroupAutoPauseThresholds(groupID int64) (float64, float64) {
	if v, ok := s.groupAutoPauseThresholds.Load(groupID); ok {
		t := v.([2]float64)
		return t[0], t[1]
	}
	return 0, 0
}

// SetGroupBaseConcurrencyOverride updates a group's inherited per-account base
// concurrency. A nil value clears the group override and falls back to other
// memberships or the global setting.
func (s *Store) SetGroupBaseConcurrencyOverride(groupID int64, value *int64) {
	if s == nil || groupID <= 0 {
		return
	}
	if value == nil {
		s.groupBaseConcurrencyOverrides.Delete(groupID)
	} else {
		s.groupBaseConcurrencyOverrides.Store(groupID, *value)
	}
	s.recomputeAllGroupBaseConcurrency()
}

func (s *Store) DeleteGroupBaseConcurrencyOverride(groupID int64) {
	s.SetGroupBaseConcurrencyOverride(groupID, nil)
}

// SetGroupProxyURLs hot-updates the group-level proxy list; an empty list (or all-empty strings) clears the setting for the group.
// Changes take effect immediately: proxy resolution happens per request, and existing sticky sessions are
// invalidated by affinityProxyStillValid and rebound the next time they are reused (issue #479).
func (s *Store) SetGroupProxyURLs(groupID int64, urls []string) {
	if s == nil || groupID <= 0 {
		return
	}
	cleaned := make([]string, 0, len(urls))
	for _, u := range urls {
		if u = strings.TrimSpace(u); u != "" {
			cleaned = append(cleaned, u)
		}
	}
	if len(cleaned) == 0 {
		s.groupProxyURLs.Delete(groupID)
		return
	}
	s.groupProxyURLs.Store(groupID, cleaned)
}

func (s *Store) DeleteGroupProxyURLs(groupID int64) {
	if s == nil || groupID <= 0 {
		return
	}
	s.groupProxyURLs.Delete(groupID)
}

func (s *Store) getGroupProxyURLs(groupID int64) []string {
	if s == nil || groupID <= 0 {
		return nil
	}
	value, ok := s.groupProxyURLs.Load(groupID)
	if !ok {
		return nil
	}
	urls, _ := value.([]string)
	return urls
}

func (s *Store) GetGroupBaseConcurrencyOverride(groupID int64) (int64, bool) {
	return s.getGroupBaseConcurrencyOverride(groupID)
}

func (s *Store) getGroupBaseConcurrencyOverride(groupID int64) (int64, bool) {
	if s == nil || groupID <= 0 {
		return 0, false
	}
	value, ok := s.groupBaseConcurrencyOverrides.Load(groupID)
	if !ok {
		return 0, false
	}
	return value.(int64), true
}

func (s *Store) recomputeAllGroupBaseConcurrency() {
	if s == nil {
		return
	}
	baseLimit := s.maxConcurrency.Load()
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		acc.recomputeEffectiveGroupBaseConcurrency(s)
		acc.recomputeSchedulerLocked(baseLimit)
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
}

func (s *Store) recomputeAllEffectiveAutoPause() {
	for _, acc := range s.accountSnapshotAccounts() {
		acc.mu.Lock()
		acc.recomputeEffectiveAutoPause(s)
		acc.mu.Unlock()
	}
}

// AddAccount hot-loads a new account into the in-memory pool (takes effect immediately after adding in the frontend)
func (s *Store) AddAccount(acc *Account) {
	s.AddAccounts([]*Account{acc})
}

// AddAccounts adds accounts to the in-memory pool in bulk. The global write lock is taken only once, and DBID index entries are inserted incrementally,
// and scheduler buckets are also processed only once at the final merge. When importing thousands of accounts, calling AddAccount one by one
// would make each call pay an O(pool size) index rebuild plus a full bucket re-sort, saturating the global lock that every request needs,
// which shows up as the whole gateway hanging during the import.
func (s *Store) AddAccounts(accounts []*Account) {
	if s == nil || len(accounts) == 0 {
		return
	}

	s.accountMutationMu.Lock()
	defer s.accountMutationMu.Unlock()

	now := time.Now().UnixNano()
	added := make([]*Account, 0, len(accounts))
	ignoreUsageLimit := s.IgnoreUsageLimitStatus()
	maxConcurrency := s.maxConcurrency.Load()
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		// record the time it was added (used for expiry cleanup)
		if acc.AddedAt.Load() == 0 {
			acc.AddedAt.Store(now)
		}
		acc.mu.Lock()
		acc.recomputeEffectiveIgnoreUsageLimitStatus(ignoreUsageLimit)
		acc.recomputeEffectiveGroupBaseConcurrency(s)
		acc.recomputeSchedulerLocked(maxConcurrency)
		acc.mu.Unlock()
		added = append(added, acc)
	}
	if len(added) == 0 {
		return
	}

	s.mu.Lock()
	s.accounts = append(s.accounts, added...)
	if s.accountsByID == nil {
		s.rebuildAccountIndex()
	} else {
		for _, acc := range added {
			s.accountsByID[acc.DBID] = acc
		}
	}
	s.publishAccountSnapshot(s.accounts)
	s.mu.Unlock()

	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.UpdateMany(added)
	}
	s.invalidateRoutingSchedulers()
	s.notifySchedulerAvailability()
}

// RemoveAccount removes an account from the in-memory pool
func (s *Store) RemoveAccount(dbID int64) {
	s.accountMutationMu.Lock()
	defer s.accountMutationMu.Unlock()

	removed := false
	s.mu.Lock()
	for i, acc := range s.accounts {
		if acc.DBID == dbID {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			s.rebuildAccountIndex()
			s.publishAccountSnapshot(s.accounts)
			removed = true
			break
		}
	}
	s.mu.Unlock()
	if !removed {
		return
	}

	s.fastSchedulerRemove(dbID)
	s.invalidateRoutingSchedulers()
	// clean up any leftover tasks in the RefreshScheduler
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		scheduler.CancelTask(dbID)
	}
}

// FindByID finds a runtime account by its database ID
func (s *Store) FindByID(dbID int64) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lookupByIDLocked(dbID)
}

// lookupByIDLocked looks up an account in O(1) via the index; when the index is missing, falls back to a linear scan.
// The caller must hold s.mu (read or write lock).
func (s *Store) lookupByIDLocked(dbID int64) *Account {
	if s.accountsByID != nil {
		return s.accountsByID[dbID]
	}
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			return acc
		}
	}
	return nil
}

// rebuildAccountIndex rebuilds the DBID index from the current s.accounts.
// The caller must hold the s.mu write lock; call it wherever s.accounts is modified to keep them in sync.
func (s *Store) rebuildAccountIndex() {
	idx := make(map[int64]*Account, len(s.accounts))
	for _, acc := range s.accounts {
		if acc != nil {
			idx[acc.DBID] = acc
		}
	}
	s.accountsByID = idx
}

// ApplyAccountSchedulerOverrides updates the runtime account's scheduler overrides and recomputes immediately.
func (s *Store) ApplyAccountSchedulerOverrides(dbID int64, scoreBiasOverride, baseConcurrencyOverride *int64, skipWarmTier *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.ScoreBiasOverride = cloneInt64Ptr(scoreBiasOverride)
	acc.BaseConcurrencyOverride = cloneInt64Ptr(baseConcurrencyOverride)
	if skipWarmTier != nil {
		acc.SkipWarmTier = *skipWarmTier
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountSchedulerOverridePatch(dbID int64, scoreBiasSet bool, scoreBiasOverride *int64, baseConcurrencySet bool, baseConcurrencyOverride *int64, skipWarmTier *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if scoreBiasSet {
		acc.ScoreBiasOverride = cloneInt64Ptr(scoreBiasOverride)
	}
	if baseConcurrencySet {
		acc.BaseConcurrencyOverride = cloneInt64Ptr(baseConcurrencyOverride)
	}
	if skipWarmTier != nil {
		acc.SkipWarmTier = *skipWarmTier
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountAllowedAPIKeys(dbID int64, allowedAPIKeyIDs []int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.setAllowedAPIKeyIDsLocked(allowedAPIKeyIDs)
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	return true
}

// ApplyAccountIgnoreUsageLimitStatus updates a nullable account override.
// override=nil means follow the global setting.
func (s *Store) ApplyAccountIgnoreUsageLimitStatus(dbID int64, override *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if override == nil {
		acc.IgnoreUsageLimitStatusOverride = nil
	} else {
		value := *override
		acc.IgnoreUsageLimitStatusOverride = &value
	}
	acc.recomputeEffectiveIgnoreUsageLimitStatus(s.IgnoreUsageLimitStatus())
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountQuotaAutoPauseConfig(dbID int64, threshold5h, threshold7d *float64, disabled5h, disabled7d *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if threshold5h != nil {
		acc.AutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(*threshold5h)
	}
	if threshold7d != nil {
		acc.AutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(*threshold7d)
	}
	if disabled5h != nil {
		acc.AutoPause5hDisabled = *disabled5h
	}
	if disabled7d != nil {
		acc.AutoPause7dDisabled = *disabled7d
	}
	acc.recomputeEffectiveAutoPause(s)
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

// ApplyAccountKeepConcurrencyOnDegrade toggles the account's "keep concurrency on degrade" setting and recomputes the limit immediately.
func (s *Store) ApplyAccountKeepConcurrencyOnDegrade(dbID int64, enabled bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.KeepConcurrencyOnDegrade = enabled
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountDispatchCountLimit(dbID int64, limit *int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	if limit == nil {
		acc.SetDispatchCountLimit(0)
	} else {
		acc.SetDispatchCountLimit(*limit)
	}
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountTags(dbID int64, tags []string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Tags = cloneStringSlice(tags)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountGroups(dbID int64, groupIDs []int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.GroupIDs = cloneInt64Slice(groupIDs)
	acc.recomputeEffectiveGroupBaseConcurrency(s)
	acc.recomputeEffectiveAutoPause(s)
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.invalidateRoutingSchedulers()
	s.fastSchedulerUpdate(acc)
	return true
}

// UpdateAccountCredit updates the account credit settings
// Passing nil means the field is not modified.
func (s *Store) UpdateAccountCredit(dbID int64, creditEnabled, creditSkipUsageWindow *bool) error {
	acc := s.FindByID(dbID)
	if acc == nil {
		return fmt.Errorf("account %d does not exist", dbID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.db.UpdateAccountCredit(ctx, dbID, creditEnabled, creditSkipUsageWindow); err != nil {
		return err
	}
	acc.mu.Lock()
	if creditEnabled != nil {
		acc.CreditEnabled = *creditEnabled
	}
	if creditSkipUsageWindow != nil {
		acc.CreditSkipUsageWindow = *creditSkipUsageWindow
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return nil
}

func (s *Store) ApplyAccountGroupMemberships(memberships map[int64][]int64) {
	for _, acc := range s.accountSnapshotAccounts() {
		acc.mu.Lock()
		acc.GroupIDs = cloneInt64Slice(memberships[acc.DBID])
		acc.recomputeEffectiveGroupBaseConcurrency(s)
		acc.recomputeEffectiveAutoPause(s)
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
	s.invalidateRoutingSchedulers()
}

func (s *Store) SetAPIKeyAllowedGroups(apiKeyID int64, groupIDs []int64) {
	if apiKeyID <= 0 {
		return
	}
	normalized := normalizeAllowedGroupIDs(groupIDs)
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyAllowedGroups == nil {
		s.apiKeyAllowedGroups = make(map[int64][]int64)
	}
	if s.apiKeyAllowedGroupSets == nil {
		s.apiKeyAllowedGroupSets = make(map[int64]map[int64]struct{})
	}
	if int64SliceEqual(s.apiKeyAllowedGroups[apiKeyID], normalized) {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if len(normalized) == 0 {
		delete(s.apiKeyAllowedGroups, apiKeyID)
		delete(s.apiKeyAllowedGroupSets, apiKeyID)
	} else {
		s.apiKeyAllowedGroups[apiKeyID] = cloneInt64Slice(normalized)
		s.apiKeyAllowedGroupSets[apiKeyID] = int64Set(normalized)
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

// SetAPIKeyNoAffinityGroups sets the groups that can be used when no downstream affinity header is carried.
// These groups are added to the union of the API Key's account authorizations; the proxy layer still selects the exact pool per request based on whether an affinity header is present.
func (s *Store) SetAPIKeyNoAffinityGroups(apiKeyID int64, groupIDs []int64) {
	if apiKeyID <= 0 {
		return
	}
	normalized := normalizeAllowedGroupIDs(groupIDs)
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyNoAffinityGroups == nil {
		s.apiKeyNoAffinityGroups = make(map[int64][]int64)
	}
	if s.apiKeyNoAffinityGroupSets == nil {
		s.apiKeyNoAffinityGroupSets = make(map[int64]map[int64]struct{})
	}
	if int64SliceEqual(s.apiKeyNoAffinityGroups[apiKeyID], normalized) {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if len(normalized) == 0 {
		delete(s.apiKeyNoAffinityGroups, apiKeyID)
		delete(s.apiKeyNoAffinityGroupSets, apiKeyID)
	} else {
		s.apiKeyNoAffinityGroups[apiKeyID] = cloneInt64Slice(normalized)
		s.apiKeyNoAffinityGroupSets[apiKeyID] = int64Set(normalized)
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

// SetAPIKeyAllowedPlans sets an API Key's account plan allowlist. plans are normalized (lower-cased, trimmed, deduplicated)
// and then stored in the in-memory set; an empty value means no plan restriction. The scheduler is rebuilt only when the set actually changes, so the auth hot path
// does not trigger a rebuild on every request.
func (s *Store) SetAPIKeyAllowedPlans(apiKeyID int64, plans []string) {
	if apiKeyID <= 0 {
		return
	}
	normalized := normalizeAllowedPlans(plans)
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyAllowedPlans == nil {
		s.apiKeyAllowedPlans = make(map[int64][]string)
	}
	if s.apiKeyAllowedPlanSets == nil {
		s.apiKeyAllowedPlanSets = make(map[int64]map[string]struct{})
	}
	if stringSliceEqual(s.apiKeyAllowedPlans[apiKeyID], normalized) {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if len(normalized) == 0 {
		delete(s.apiKeyAllowedPlans, apiKeyID)
		delete(s.apiKeyAllowedPlanSets, apiKeyID)
	} else {
		s.apiKeyAllowedPlans[apiKeyID] = append([]string(nil), normalized...)
		s.apiKeyAllowedPlanSets[apiKeyID] = stringSet(normalized)
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

func (s *Store) GetAPIKeyAllowedGroups(apiKeyID int64) []int64 {
	if apiKeyID <= 0 {
		return nil
	}
	s.apiKeyGroupsMu.RLock()
	defer s.apiKeyGroupsMu.RUnlock()
	return cloneInt64Slice(s.apiKeyAllowedGroups[apiKeyID])
}

// SetAPIKeyUpstreamChannel sets an API Key's upstream channel restriction (codex; empty = unrestricted).
// The scheduler is rebuilt only when the value actually changes.
func (s *Store) SetAPIKeyUpstreamChannel(apiKeyID int64, channel string) {
	if apiKeyID <= 0 {
		return
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != database.UpstreamChannelCodex {
		channel = ""
	}
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyUpstreamChannels == nil {
		s.apiKeyUpstreamChannels = make(map[int64]string)
	}
	if s.apiKeyUpstreamChannels[apiKeyID] == channel {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if channel == "" {
		delete(s.apiKeyUpstreamChannels, apiKeyID)
	} else {
		s.apiKeyUpstreamChannels[apiKeyID] = channel
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

// APIKeyUpstreamChannel returns an API Key's upstream channel restriction (empty = unrestricted).
func (s *Store) APIKeyUpstreamChannel(apiKeyID int64) string {
	if s == nil || apiKeyID <= 0 {
		return ""
	}
	s.apiKeyGroupsMu.RLock()
	defer s.apiKeyGroupsMu.RUnlock()
	return s.apiKeyUpstreamChannels[apiKeyID]
}

func (s *Store) LoadAPIKeyAllowedGroups(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	keys, err := s.db.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	s.apiKeyGroupsMu.Lock()
	s.apiKeyAllowedGroups = make(map[int64][]int64, len(keys))
	s.apiKeyAllowedGroupSets = make(map[int64]map[int64]struct{}, len(keys))
	s.apiKeyNoAffinityGroups = make(map[int64][]int64, len(keys))
	s.apiKeyNoAffinityGroupSets = make(map[int64]map[int64]struct{}, len(keys))
	s.apiKeyAllowedPlans = make(map[int64][]string, len(keys))
	s.apiKeyAllowedPlanSets = make(map[int64]map[string]struct{}, len(keys))
	s.apiKeyUpstreamChannels = make(map[int64]string, len(keys))
	for _, key := range keys {
		normalized := normalizeAllowedGroupIDs(key.AllowedGroupIDs)
		if len(normalized) > 0 {
			s.apiKeyAllowedGroups[key.ID] = cloneInt64Slice(normalized)
			s.apiKeyAllowedGroupSets[key.ID] = int64Set(normalized)
		}
		noAffinityGroups := normalizeAllowedGroupIDs(key.Limits.NoAffinityGroupIDs)
		if len(noAffinityGroups) > 0 {
			s.apiKeyNoAffinityGroups[key.ID] = cloneInt64Slice(noAffinityGroups)
			s.apiKeyNoAffinityGroupSets[key.ID] = int64Set(noAffinityGroups)
		}
		plans := normalizeAllowedPlans(key.Limits.PlanAllow)
		if len(plans) > 0 {
			s.apiKeyAllowedPlans[key.ID] = append([]string(nil), plans...)
			s.apiKeyAllowedPlanSets[key.ID] = stringSet(plans)
		}
		if channel := key.Limits.ResolveUpstreamChannel(); channel == database.UpstreamChannelCodex {
			s.apiKeyUpstreamChannels[key.ID] = channel
		}
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
	return nil
}

// APIKeyAllowsAccount reports whether an API Key is allowed to be scheduled onto the account. The group allowlist and the plan allowlist
// must each match when non-empty (AND semantics); an empty one means that dimension is unrestricted.
func (s *Store) APIKeyAllowsAccount(apiKeyID int64, acc *Account) bool {
	if s == nil || apiKeyID <= 0 || acc == nil {
		return true
	}
	s.apiKeyGroupsMu.RLock()
	allowedGroups := s.apiKeyAllowedGroupSets[apiKeyID]
	noAffinityGroups := s.apiKeyNoAffinityGroupSets[apiKeyID]
	allowedPlans := s.apiKeyAllowedPlanSets[apiKeyID]
	s.apiKeyGroupsMu.RUnlock()
	if len(allowedGroups) == 0 && len(allowedPlans) == 0 {
		return true
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if len(allowedPlans) > 0 {
		plan := lowerTrimPlan(acc.PlanType)
		if _, ok := allowedPlans[plan]; !ok {
			return false
		}
	}
	if len(allowedGroups) == 0 {
		return true
	}
	for _, id := range acc.GroupIDs {
		if _, ok := allowedGroups[id]; ok {
			return true
		}
		if _, ok := noAffinityGroups[id]; ok {
			return true
		}
	}
	return false
}

func normalizeAllowedGroupIDs(groupIDs []int64) []int64 {
	out := make([]int64, 0, len(groupIDs))
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func int64Set(values []int64) map[int64]struct{} {
	out := make(map[int64]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func int64SliceEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lowerTrimPlan normalizes a single plan name for matching: lower-cased and trimmed. It deliberately does not fold prolite/promax into pro,
// so that API Key plan filtering stays consistent with the account list (Accounts page), which matches the raw plan_type exactly.
func lowerTrimPlan(plan string) string {
	return strings.ToLower(strings.TrimSpace(plan))
}

// normalizeAllowedPlans normalizes the account plan allowlist: lower-cased, trimmed, deduplicated and sorted, to keep
// the change detection in SetAPIKeyAllowedPlans stable. Matching also goes through lowerTrimPlan on the account side.
func normalizeAllowedPlans(plans []string) []string {
	out := make([]string, 0, len(plans))
	seen := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		normalized := lowerTrimPlan(plan)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func (s *Store) accountAllowedForAPIKey(acc *Account, apiKeyID int64) bool {
	if acc == nil {
		return false
	}
	return acc.AllowsAPIKey(apiKeyID) && s.APIKeyAllowsAccount(apiKeyID, acc)
}

// NormalizeAccountModels normalizes the account's supported model list (deduplicated, trimmed, sorted lexicographically).
func NormalizeAccountModels(values []string) []string {
	return normalizeModelList(values)
}

// ApplyAccountModels updates the runtime account's supported-model allowlist (an empty list = clear the allowlist, allow all models).
func (s *Store) ApplyAccountModels(dbID int64, models []string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Models = normalizeModelList(models)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountProxyURL(dbID int64, proxyURL string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.ProxyURL = strings.TrimSpace(proxyURL)
	acc.mu.Unlock()
	return true
}

// ClearAccountProxyURLIfMatches clears an account proxy only when its current
// runtime value still points to one of the removed URLs.
func (s *Store) ClearAccountProxyURLIfMatches(dbID int64, proxyURLs []string) bool {
	expected := buildProxyPoolSet(proxyURLs)
	if len(expected) == 0 {
		return false
	}
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if _, ok := expected[strings.TrimSpace(acc.ProxyURL)]; !ok {
		return false
	}
	acc.ProxyURL = ""
	return true
}

func (s *Store) ApplyAccountCustomHeaders(dbID int64, headers map[string]string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CustomHeaders = cloneStringMap(headers)
	acc.mu.Unlock()
	return true
}

// ApplyAccountCodexFingerprintMode syncs the fingerprint convergence mode changed in the admin panel to the runtime account,
// so it takes effect without waiting for the next full reload.
func (s *Store) ApplyAccountCodexFingerprintMode(dbID int64, mode string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CodexFingerprintMode = NormalizeCodexFingerprintMode(mode)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountEnabled(dbID int64, enabled bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	atomic.StoreInt32(&acc.IntelligencePaused, 0) // DB manual write revoked detection ownership.
	if enabled {
		atomic.StoreInt32(&acc.DispatchPaused, 0)
	} else {
		atomic.StoreInt32(&acc.DispatchPaused, 1)
	}
	s.fastSchedulerUpdate(acc)
	return true
}

func normalizeAccountErrorMessage(errorMsg string, fallback string) string {
	errorMsg = strings.TrimSpace(errorMsg)
	if errorMsg == "" {
		errorMsg = strings.TrimSpace(fallback)
	}
	if len(errorMsg) > 500 {
		errorMsg = errorMsg[:500]
	}
	return errorMsg
}

// MarkCooldown puts an account into cooldown and persists it to the database
func (s *Store) MarkCooldown(acc *Account, duration time.Duration, reason string) {
	s.markCooldown(acc, duration, reason, "", false)
}

// MarkResponsesRateLimited records an authoritative account-scoped Responses
// rejection and immediately refreshes its WHAM usage snapshot. The first
// transition is probed once; repeated 429s while the cooldown is active do not
// create a probe storm.
func (s *Store) MarkResponsesRateLimited(acc *Account, duration time.Duration) {
	if s == nil || acc == nil {
		return
	}
	now := time.Now()
	acc.mu.RLock()
	alreadyLimited := acc.Status == StatusCooldown &&
		acc.CooldownReason == ResponsesRateLimitedCooldownReason &&
		(acc.CooldownUtil.IsZero() || now.Before(acc.CooldownUtil))
	acc.mu.RUnlock()

	s.MarkCooldown(acc, duration, ResponsesRateLimitedCooldownReason)
	if !alreadyLimited {
		s.TriggerUsageProbeForAccountAsync(acc)
	}
}

// MarkCooldownWithError puts an account into cooldown and also records the details of this upstream error.
func (s *Store) MarkCooldownWithError(acc *Account, duration time.Duration, reason string, errorMsg string) {
	s.markCooldown(acc, duration, reason, errorMsg, false)
}

// MarkCooldownWithErrorExactDuration puts an account into a cooldown of the specified duration and records the upstream error details.
// Unlike MarkCooldownWithError, this method does not apply the adaptive 6/24-hour policy for unauthorized.
func (s *Store) MarkCooldownWithErrorExactDuration(acc *Account, duration time.Duration, reason string, errorMsg string) {
	s.markCooldown(acc, duration, reason, errorMsg, true)
}

func (s *Store) markDispatchCountLimitCooldown(acc *Account, resetAt time.Time, updateScheduler bool) {
	if s == nil || acc == nil {
		return
	}
	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(dispatchCountFallbackWindow)
	}
	s.markCooldownUntil(acc, resetAt, "rate_limited", updateScheduler)
}

func (s *Store) markCooldownUntil(acc *Account, until time.Time, reason string, updateScheduler bool) {
	if acc == nil {
		return
	}
	now := time.Now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(dispatchCountFallbackWindow)
	}
	reason = normalizeCooldownReason(reason)

	acc.mu.Lock()
	acc.Status = StatusCooldown
	acc.CooldownUtil = until
	acc.CooldownReason = reason
	acc.transientRateLimitUntil = time.Time{}
	if acc.transientRateLimitTimer != nil {
		acc.transientRateLimitTimer.Stop()
		acc.transientRateLimitTimer = nil
	}
	switch reason {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.HealthTier = HealthTierBanned
	case "rate_limited_5h", ResponsesRateLimitedCooldownReason:
		acc.LastRateLimitedAt = now
		if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	case "rate_limited", "rate_limited_7d", "usage_limited", "usage_limit":
		acc.LastRateLimitedAt = now
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()

	if updateScheduler {
		s.fastSchedulerUpdate(acc)
	}
	s.persistAccountCooldownState(acc, true, false)
}

// markCooldown picks adaptive or exact duration based on exactDuration and applies the account cooldown.
func (s *Store) markCooldown(acc *Account, duration time.Duration, reason string, errorMsg string, exactDuration bool) {
	if acc == nil {
		return
	}

	errorMsg = normalizeAccountErrorMessage(errorMsg, "")
	now := time.Now()
	acc.mu.Lock()
	switch reason {
	case "unauthorized":
		if !exactDuration {
			if !acc.LastUnauthorizedAt.IsZero() && now.Sub(acc.LastUnauthorizedAt) < 24*time.Hour {
				duration = 24 * time.Hour
			} else {
				duration = 6 * time.Hour
			}
		}
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		acc.HealthTier = HealthTierBanned
	case "rate_limited_5h", ResponsesRateLimitedCooldownReason:
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	case "rate_limited", "rate_limited_7d", "usage_limited", "usage_limit":
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	}
	if errorMsg != "" {
		acc.ErrorMsg = errorMsg
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	until := now.Add(duration)
	acc.setCooldownUntilLocked(until, reason)
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, false)
}

func (s *Store) MarkModelCooldown(acc *Account, model string, duration time.Duration, reason string) ModelCooldown {
	return s.MarkModelCooldownWithBackoff(acc, model, duration, reason, true)
}

// modelCooldownStreakTTL is the validity period of a model cooldown backoff streak, with a value well above the 30-minute cooldown cap.
//
// The backoff tier used to escalate only when a failure happened while the cooldown had not yet expired; in low-traffic deployments, the gap between two failures is usually
// longer than the cooldown itself: the cooldown has already expired, so the tier stayed at the first level forever and the backoff was ineffective. It is now judged by the time of the most recent
// failure: a failure within the TTL keeps escalating, and a success (ClearModelCooldown deletes the entry)
// or exceeding the TTL returns it to the first level.
const modelCooldownStreakTTL = time.Hour

// modelCooldownStreakActive reports whether the model's failure streak is still within its validity period.
func modelCooldownStreakActive(current ModelCooldown, now time.Time) bool {
	if current.ResetAt.After(now) {
		return true
	}
	if current.UpdatedAt.IsZero() {
		return false
	}
	// when the clock is set back, elapsed is negative and is treated as "just failed": better to back off one more tier,
	// than to hit upstream again because the system clock jumped.
	return now.Sub(current.UpdatedAt) < modelCooldownStreakTTL
}

func (s *Store) MarkModelCooldownWithBackoff(acc *Account, model string, duration time.Duration, reason string, backoffEnabled bool) ModelCooldown {
	if acc == nil {
		return ModelCooldown{}
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return ModelCooldown{}
	}
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	if duration > 30*time.Minute {
		duration = 30 * time.Minute
	}

	now := time.Now()
	acc.mu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]ModelCooldown)
	}
	current := acc.ModelCooldowns[key]
	level := current.BackoffLevel
	switch {
	case !backoffEnabled:
		level = 0
	case modelCooldownStreakActive(current, now):
		level++
		duration *= 2
		for i := 0; i < level-1; i++ {
			duration *= 2
		}
		if duration > 30*time.Minute {
			duration = 30 * time.Minute
		}
	default:
		// the streak is broken: return to the first level, so an old tier does not stay on the account indefinitely.
		level = 0
	}
	resetAt := now.Add(duration)
	if reason == "" {
		reason = "rate_limited"
	}
	// an existing longer cooldown that is still in effect (e.g. credits_required's 30 minutes) must not be shortened
	// by a later, shorter generic rate-limit cooldown, otherwise the account would be selected again within seconds and hit the same error again.
	if current.ResetAt.After(now) && current.ResetAt.After(resetAt) {
		resetAt = current.ResetAt
		if current.Reason != "" {
			reason = current.Reason
		}
		level = current.BackoffLevel
	}
	cooldown := ModelCooldown{
		Model:        key,
		Reason:       reason,
		ResetAt:      resetAt,
		UpdatedAt:    now,
		BackoffLevel: level,
	}
	acc.ModelCooldowns[key] = cooldown
	acc.LastRateLimitedAt = now
	acc.LastFailureAt = now
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0
	if acc.healthTierLocked() == HealthTierHealthy {
		acc.HealthTier = HealthTierWarm
	} else if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.setCachedModelCooldown(acc.DBID, cooldown)

	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.SetModelCooldown(ctx, acc.DBID, key, reason, resetAt); err != nil {
			log.Printf("[account %d] failed to persist model cooldown model=%s: %v", acc.DBID, key, err)
		}
	}
	return cooldown
}

// MarkModelCooldownUntil sets the model cooldown to an explicit reset time (no 30-minute cap clamp),
// used when the upstream explicitly states the recovery window (e.g. the rolling 24h window of exhausted free quota).
func (s *Store) MarkModelCooldownUntil(acc *Account, model, reason string, resetAt time.Time) ModelCooldown {
	if acc == nil || resetAt.IsZero() || !resetAt.After(time.Now()) {
		return ModelCooldown{}
	}
	cooldown := acc.SetModelCooldownUntil(model, reason, resetAt)
	if cooldown.Model == "" {
		return cooldown
	}
	s.afterModelCooldownMarked(acc, cooldown)
	return cooldown
}

// MarkModelCooldownUntilAtLeast is the same as MarkModelCooldownUntil, but does not shorten an existing cooldown that is still
// in effect: if the model's current ResetAt is later than the passed resetAt, the existing cooldown is kept and it returns directly,
// without writing memory, refreshing health, or persisting.
//
// Used for cooldown keys shared by several public tiers that share the same upstream backing (wire model) or the same quota family.
// These keys are written by several events on the same account; if the last writer wins, a bare 5s
// RESOURCE_EXHAUSTED could shorten the 290s shared cooldown just written on the same account, and after the short deadline a sibling
// tier could pick a credential that was just rate-limited. The comparison and the write happen under one write lock on the Account side.
func (s *Store) MarkModelCooldownUntilAtLeast(acc *Account, model, reason string, resetAt time.Time) ModelCooldown {
	if acc == nil || resetAt.IsZero() || !resetAt.After(time.Now()) {
		return ModelCooldown{}
	}
	cooldown, wrote := acc.SetModelCooldownUntilAtLeast(model, reason, resetAt)
	if cooldown.Model == "" || !wrote {
		return cooldown
	}
	s.afterModelCooldownMarked(acc, cooldown)
	return cooldown
}

// afterModelCooldownMarked, after a cooldown is written, uniformly refreshes health, the scheduling snapshot, the cross-instance cooldown cache
// and persistence.
func (s *Store) afterModelCooldownMarked(acc *Account, cooldown ModelCooldown) {
	acc.mu.Lock()
	now := time.Now()
	acc.LastRateLimitedAt = now
	acc.LastFailureAt = now
	if acc.healthTierLocked() == HealthTierHealthy {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.setCachedModelCooldown(acc.DBID, cooldown)

	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.SetModelCooldown(ctx, acc.DBID, cooldown.Model, cooldown.Reason, cooldown.ResetAt); err != nil {
			log.Printf("[account %d] failed to persist model cooldown model=%s: %v", acc.DBID, cooldown.Model, err)
		}
	}
}

func (s *Store) ClearModelCooldown(acc *Account, model string) {
	if acc == nil {
		return
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return
	}
	if !acc.ClearModelCooldown(key) {
		return
	}
	s.deleteCachedModelCooldown(acc.DBID, key)
	s.fastSchedulerUpdate(acc)
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearModelCooldown(ctx, acc.DBID, key); err != nil {
		log.Printf("[account %d] failed to clear model cooldown model=%s: %v", acc.DBID, key, err)
	}
}

func (s *Store) ClearAllModelCooldowns(acc *Account) int {
	if acc == nil {
		return 0
	}
	models := acc.ClearAllModelCooldowns()
	for _, model := range models {
		s.deleteCachedModelCooldown(acc.DBID, model)
	}
	s.fastSchedulerUpdate(acc)
	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.ClearAllModelCooldowns(ctx, acc.DBID); err != nil {
			log.Printf("[account %d] failed to clear all model cooldowns: %v", acc.DBID, err)
		}
	}
	return len(models)
}

// ModelCooldownReasonUnsupported marks the plan-gated Codex 400 "model is not
// supported when using Codex with a ChatGPT account".
const ModelCooldownReasonUnsupported = "model_not_supported"

// UnsupportedModelCooldown returns the configured cooldown for the plan-gated
// unsupported-model 400; zero when disabled.
func (s *Store) UnsupportedModelCooldown() time.Duration {
	settings := s.GetModelCooldownSettings()
	return time.Duration(settings.UnsupportedCooldownSeconds()) * time.Second
}

// MarkModelUnsupported cools (account, model) down without touching account
// health: a plan that cannot serve one model says nothing about the others.
func (s *Store) MarkModelUnsupported(acc *Account, model string, duration time.Duration) ModelCooldown {
	if s == nil || acc == nil || duration <= 0 {
		return ModelCooldown{}
	}
	if duration > 30*time.Minute {
		duration = 30 * time.Minute
	}
	resetAt := time.Now().Add(duration)
	if remaining := acc.ModelCooldownRemaining(model); remaining > duration {
		resetAt = time.Now().Add(remaining)
	}
	cooldown := acc.SetModelCooldownUntil(model, ModelCooldownReasonUnsupported, resetAt)
	if cooldown.Model == "" {
		return cooldown
	}
	s.fastSchedulerUpdate(acc)
	s.setCachedModelCooldown(acc.DBID, cooldown)
	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.SetModelCooldown(ctx, acc.DBID, cooldown.Model, cooldown.Reason, cooldown.ResetAt); err != nil {
			log.Printf("[account %d] failed to persist model cooldown model=%s: %v", acc.DBID, cooldown.Model, err)
		}
	}
	return cooldown
}

// ClearModelCooldownsByReason clears active model cooldowns with the given
// reason whose remaining time exceeds keepWithin (0 clears all of them).
func (s *Store) ClearModelCooldownsByReason(reason string, keepWithin time.Duration) int {
	if s == nil {
		return 0
	}
	cleared := 0
	now := time.Now()
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil {
			continue
		}
		for _, cooldown := range acc.ActiveModelCooldowns() {
			if cooldown.Reason != reason || (keepWithin > 0 && cooldown.ResetAt.Sub(now) <= keepWithin) {
				continue
			}
			s.ClearModelCooldown(acc, cooldown.Model)
			cleared++
		}
	}
	return cleared
}

// UnsupportedModelPoolSummary reports whether dispatchable accounts are being
// skipped for model because each recently answered the unsupported-model 400,
// so callers can surface that instead of a bare "no available account".
type UnsupportedModelPoolSummary struct {
	Found   bool
	ResetAt time.Time // earliest recovery among those accounts
}

func (s *Store) UnsupportedModelCandidateSummary(apiKeyID int64, exclude map[int64]bool, model string, policy DispatchPolicy) UnsupportedModelPoolSummary {
	var summary UnsupportedModelPoolSummary
	key := normalizeModelCooldownKey(model)
	if s == nil || key == "" {
		return summary
	}
	now := time.Now()
	for _, acc := range s.accountSnapshotAccounts() {
		if acc == nil || (exclude != nil && exclude[acc.DBID]) {
			continue
		}
		if !acc.dispatchableForPolicy(policy) || !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		acc.mu.RLock()
		cooldown, ok := acc.ModelCooldowns[key]
		acc.mu.RUnlock()
		if !ok || cooldown.Reason != ModelCooldownReasonUnsupported || !cooldown.ResetAt.After(now) {
			continue
		}
		summary.Found = true
		if summary.ResetAt.IsZero() || cooldown.ResetAt.Before(summary.ResetAt) {
			summary.ResetAt = cooldown.ResetAt
		}
	}
	return summary
}

type ModelCooldownPolicy struct {
	Mode           string
	Seconds        int
	BackoffEnabled bool
	Source         string
}

func (s *Store) SetModelCooldownSettings(settings database.ModelCooldownSettings) {
	if s == nil {
		return
	}
	s.modelCooldownSettings.Store(database.NormalizeModelCooldownSettings(settings))
}

func (s *Store) GetModelCooldownSettings() database.ModelCooldownSettings {
	if s == nil {
		return database.DefaultModelCooldownSettings()
	}
	if value := s.modelCooldownSettings.Load(); value != nil {
		if settings, ok := value.(database.ModelCooldownSettings); ok {
			return database.NormalizeModelCooldownSettings(settings)
		}
	}
	return database.DefaultModelCooldownSettings()
}

func (s *Store) ResolveModelCooldownPolicy(acc *Account) ModelCooldownPolicy {
	settings := s.GetModelCooldownSettings()
	policy := ModelCooldownPolicy{
		Mode:           settings.OAuthMode,
		Seconds:        settings.OAuthSeconds,
		BackoffEnabled: settings.OAuthBackoffEnabled,
		Source:         "oauth",
	}
	if acc != nil {
		mode, seconds, backoff := acc.GetModelCooldownPolicyOverride()
		if mode != nil {
			policy.Mode = database.NormalizeModelCooldownMode(*mode, policy.Mode)
			policy.Source = "account"
		}
		if seconds != nil {
			policy.Seconds = database.NormalizeModelCooldownSeconds(*seconds, policy.Seconds)
			policy.Source = "account"
		}
		if backoff != nil {
			policy.BackoffEnabled = *backoff
			policy.Source = "account"
		}
	}
	return policy
}

func (s *Store) ApplyAccountModelCooldownPolicyOverride(dbID int64, mode *string, seconds *int, backoff *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.SetModelCooldownPolicyOverride(mode, seconds, backoff)
	return true
}

// MarkError marks the account as being in an error state and persists it to the database.
// permanentRefreshFailureTerminalLimit is the cap on consecutive unrecoverable refresh failures for the same account.
// Below the cap, the unauthorized adaptive cooldown applies, leaving a self-healing window for cases such as a concurrent rotation swapping in a new RT or the user re-authorizing;
// reaching the cap consecutively means the RT is truly dead: it switches to the error terminal state and exits the recovery-probe rotation,
// otherwise a dead account would be picked up by the recovery probe each cooldown cycle and retried once, never reaching a terminal state.
const permanentRefreshFailureTerminalLimit = 3

// markPermanentRefreshFailure records one unrecoverable refresh failure and sets the corresponding status.
// The count is reset to zero on a successful refresh and on ClearCooldown (manual cleanup / re-authorization).
func (s *Store) markPermanentRefreshFailure(acc *Account, err error) {
	if acc == nil || err == nil {
		return
	}
	acc.mu.Lock()
	acc.PermanentRefreshFailures++
	failures := acc.PermanentRefreshFailures
	if failures >= permanentRefreshFailureTerminalLimit && acc.HealthTier == HealthTierBanned {
		// the first few rounds of unauthorized cooldown have already pushed the health tier down to banned, while the banned tier in
		// runtimeStatusLocked unconditionally shows unauthorized, and is the admission tier of the recovery probe.
		// If the tier is not lowered first, a terminal account would show as unauthorized forever and never reach the error filter.
		acc.HealthTier = HealthTierRisky
	}
	acc.mu.Unlock()
	if failures >= permanentRefreshFailureTerminalLimit {
		s.MarkError(acc, err.Error())
		return
	}
	// the duration argument is overridden by the unauthorized adaptive policy: first offense 6h, a repeat within 24h 24h.
	s.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", err.Error())
}

func (s *Store) MarkError(acc *Account, errorMsg string) {
	if acc == nil {
		return
	}

	errorMsg = normalizeAccountErrorMessage(errorMsg, "account test failed")

	now := time.Now()
	acc.mu.Lock()
	acc.Status = StatusError
	acc.ErrorMsg = errorMsg
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastFailureAt = now
	acc.FailureStreak++
	acc.SuccessStreak = 0
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, false)
}

// ClearCooldown clears the account cooldown state and syncs the database
func (s *Store) ClearCooldown(acc *Account) {
	if acc == nil {
		return
	}

	atomic.StoreInt32(&acc.Disabled, 0) // clears the atomic disabled flag
	acc.mu.Lock()
	wasCooling := acc.Status == StatusCooldown
	wasError := acc.Status == StatusError
	premium5hLimited := acc.premium5hRateLimitedLocked(time.Now())
	if acc.Status == StatusCooldown || acc.Status == StatusError {
		acc.Status = StatusReady
	}
	acc.ErrorMsg = ""
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	// a manual cleanup gives a fresh chance to self-heal: reset the dead-RT judgment, so probe eligibility is restored.
	acc.PermanentRefreshFailures = 0
	acc.transientRateLimitBackoff = 0
	acc.transientRateLimitUntil = time.Time{}
	if acc.transientRateLimitTimer != nil {
		acc.transientRateLimitTimer.Stop()
		acc.transientRateLimitTimer = nil
	}
	if wasCooling && !premium5hLimited {
		acc.HealthTier = HealthTierWarm
	} else if wasError && acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, true)
}

// ReleaseUsageWindowCooldownForCredits releases the cooldown that local usage-window penalties produced, once the credit gate is open.
//
// Why it is needed: the credit switch previously only blocked *entering* a usage-window cooldown (MarkUsage7dRateLimited returned early),
// which had no effect on accounts already in cooldown. The most natural use, though, is "turn the switch on after noticing the account is rate-limited",
// and by then the penalty had already landed; without actively releasing it, you would have to wait until the window resets, and the switch would look completely useless.
//
// Only the local usage penalty is released (usageWindowCooldownLocked: 7d is identified by the penalty duration aligned to Reset7dAt,
// premium 5h is identified by the rate_limited_5h reason),
// upstream 429 cooldowns are left untouched. If identification is wrong and something is released, upstream will reject once more and re-enter cooldown, at the cost of one request.
// Returns whether it was actually released.
func (s *Store) ReleaseUsageWindowCooldownForCredits(acc *Account) bool {
	if s == nil || acc == nil {
		return false
	}
	acc.mu.RLock()
	release := acc.creditSkipsUsageWindowLocked() && acc.usageWindowCooldownLocked()
	acc.mu.RUnlock()
	if !release {
		return false
	}
	s.ClearCooldown(acc)
	log.Printf("[account %d] credits available, released the usage-window rate-limit cooldown", acc.DBID)
	return true
}

// ClearUsageLimitCooldownSince clears only a usage/rate-limit cooldown that
// was already present when observedAt was captured. Authentication failures,
// generic errors, disabled states, and newer cooldowns are left untouched.
func (s *Store) ClearUsageLimitCooldownSince(acc *Account, observedAt time.Time) bool {
	if s == nil || acc == nil {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	acc.mu.Lock()
	if acc.Status != StatusCooldown || !isUsageLimitCooldownReason(acc.CooldownReason) ||
		(!acc.LastRateLimitedAt.IsZero() && acc.LastRateLimitedAt.After(observedAt)) {
		acc.mu.Unlock()
		return false
	}
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.ErrorMsg = ""
	acc.LastRateLimitedAt = time.Time{}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, false)
	return true
}

func isUsageLimitCooldownReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "rate_limited", "rate_limited_5h", ResponsesRateLimitedCooldownReason, "rate_limited_7d", "usage_limited", "usage_limit":
		return true
	default:
		return false
	}
}

// ConfirmResponsesAvailable preserves the original API for callers whose
// success evidence is current at call time.
func (s *Store) ConfirmResponsesAvailable(acc *Account) bool {
	return s.confirmResponsesAvailable(acc, time.Time{}, false)
}

// ConfirmResponsesAvailableSince clears only a usage/rate-limit cooldown when
// a completed Responses request started after the latest rate-limit evidence.
// A stale in-flight success must not undo a newer usage_limit_reached result.
// Authentication and unrelated error states are intentionally untouched.
func (s *Store) ConfirmResponsesAvailableSince(acc *Account, requestStartedAt time.Time) bool {
	return s.confirmResponsesAvailable(acc, requestStartedAt, true)
}

func (s *Store) confirmResponsesAvailable(acc *Account, requestStartedAt time.Time, fenceNewerRateLimit bool) bool {
	if s == nil || acc == nil {
		return false
	}

	acc.mu.Lock()
	if !acc.ignoreUsageLimitStatus ||
		acc.Status != StatusCooldown ||
		!isUsageLimitCooldownReason(acc.CooldownReason) ||
		(fenceNewerRateLimit && !acc.LastRateLimitedAt.IsZero() && !requestStartedAt.After(acc.LastRateLimitedAt)) {
		acc.mu.Unlock()
		return false
	}
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.ErrorMsg = ""
	acc.LastRateLimitedAt = time.Time{}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, false)
	return true
}

// RecordManualTestSuccess clears failure/cooldown state after an explicit admin
// connection test succeeds.
func (s *Store) RecordManualTestSuccess(acc *Account, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	atomic.StoreInt32(&acc.Disabled, 0)
	acc.mu.Lock()
	wasCooling := acc.Status == StatusCooldown
	wasError := acc.Status == StatusError
	wasBanned := acc.HealthTier == HealthTierBanned
	wasUsageLimitCooldown := acc.ignoreUsageLimitStatus && wasCooling && isUsageLimitCooldownReason(acc.CooldownReason)
	premium5hLimited := acc.premium5hRateLimitedLocked(now)
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(true)
	if wasCooling || wasError {
		acc.Status = StatusReady
	}
	acc.ErrorMsg = ""
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastSuccessAt = now
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	if wasUsageLimitCooldown {
		acc.LastRateLimitedAt = time.Time{}
	}
	if premium5hLimited {
		acc.HealthTier = HealthTierRisky
	} else if wasUsageLimitCooldown {
		acc.HealthTier = HealthTierHealthy
	} else if wasBanned || wasCooling || wasError {
		acc.HealthTier = HealthTierWarm
	} else if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.persistAccountCooldownState(acc, true, true)
}

// ReportRequestSuccess records one successful request, used for dynamic scheduling scores.
func (s *Store) ReportRequestSuccess(acc *Account, latency time.Duration) {
	if acc == nil {
		return
	}

	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(true)
	now := time.Now()
	acc.LastSuccessAt = now
	acc.observeTransientRateLimitSuccessLocked(now)
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// transportFailureTierDropStreak is the number of consecutive transport-layer failures required before the tier starts dropping.
// One success resets FailureStreak, so occasional disconnects never add up to this threshold.
const transportFailureTierDropStreak = 3

// isolatedTransportFailureLocked determines whether "the most recent failure was an isolated transport-layer disconnect".
//
// Transport-layer disconnects mostly come from upstream edge resets or link jitter (peer RST_STREAM, connection reset mid-way),
// unrelated to the account's own health: a few such background-noise events a day should not cut a normal account's concurrency in half
// (issue #491). Only when consecutive failures reach the threshold is the account or egress deemed truly faulty, and by then the score has also already
// dropped out of Healthy (each consecutive failure deducts 6 points), so the two criteria naturally agree.
func (a *Account) isolatedTransportFailureLocked() bool {
	return a.LastFailureKind == transportFailureKind && a.FailureStreak < transportFailureTierDropStreak
}

const transportFailureKind = "transport"

// ReportRequestFailure records one failed request for dynamic scheduling scoring
func (s *Store) ReportRequestFailure(acc *Account, kind string, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(false)
	acc.LastFailureAt = now
	acc.LastFailureKind = kind
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0

	switch kind {
	case "unauthorized":
		// The account cooldown path owns LastUnauthorizedAt so it can
		// distinguish a first 401 from a repeated one. HTTP handlers record
		// failure metrics before applying that cooldown; updating the timestamp
		// here would make the current failure look like a prior failure and
		// incorrectly select the 24-hour backoff.
		acc.HealthTier = HealthTierBanned
	case "timeout":
		acc.LastTimeoutAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "server":
		acc.LastServerErrorAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case transportFailureKind:
		// HealthTier is deliberately left untouched here: recomputeSchedulerLocked at the end of this function recalculates
		// from the score and overwrites the tier, so an assignment here has no effect (assignments in the other branches are the same, except that only
		// the Banned state of unauthorized is explicitly kept by the recalculation logic). The tier for transport-layer failures is decided by
		// the consecutive-failure deduction together with the exemption from isolatedTransportFailureLocked.
	case "client":
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		}
	}

	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// PersistUsageSnapshot persists the account's usage snapshot (7d + 5h)
func (s *Store) PersistUsageSnapshot(acc *Account, pct7d float64) {
	s.PublishUsageSnapshot(acc, pct7d, time.Now(), true)
}

// PublishUsageSnapshot updates the in-memory usage snapshot and the scheduler.
// persist=false skips the database write, used by the WS frame path where the
// in-memory value must change immediately but persistence is throttled.
func (s *Store) PublishUsageSnapshot(acc *Account, pct7d float64, observedAt time.Time, persist bool) bool {
	if acc == nil || s == nil {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	acc.SetUsageSnapshot(pct7d, observedAt)
	s.fastSchedulerUpdate(acc)

	if !persist || s.db == nil {
		return false
	}

	// If there is 5h data, store the full snapshot
	if pct5h, ok := acc.GetUsagePercent5h(); ok {
		reset5hAt := acc.GetReset5hAt()
		reset7dAt := acc.GetReset7dAt()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := s.withUsageSnapshotWrite(acc, func() error {
			return s.db.UpdateUsageSnapshotFull(ctx, acc.DBID, pct7d, reset7dAt, pct5h, reset5hAt, observedAt, acc.GetUsageUpdatedAt5h())
		})
		if err != nil {
			log.Printf("[account %d] failed to persist usage snapshot: %v", acc.DBID, err)
			return false
		}
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.withUsageSnapshotWrite(acc, func() error {
		return s.db.UpdateUsageSnapshot(ctx, acc.DBID, pct7d, observedAt)
	})
	if err != nil {
		log.Printf("[account %d] failed to persist usage snapshot: %v", acc.DBID, err)
		return false
	}
	return true
}

// withUsageSnapshotWrite serializes credential snapshot writes with the async
// frame-persist worker.
func (s *Store) withUsageSnapshotWrite(acc *Account, fn func() error) error {
	acc.usageSnapshotPersistMu.Lock()
	defer acc.usageSnapshotPersistMu.Unlock()
	return fn()
}

// UpdateAccountSubscriptionExpiresAt persists the latest subscription expiration observed from upstream.
func (s *Store) UpdateAccountSubscriptionExpiresAt(acc *Account, expiresAt time.Time) bool {
	if s == nil || acc == nil || expiresAt.IsZero() {
		return false
	}

	acc.mu.Lock()
	changed := acc.SubscriptionExpiresAt.IsZero() || !acc.SubscriptionExpiresAt.Equal(expiresAt)
	if changed {
		acc.SubscriptionExpiresAt = expiresAt
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	}
	acc.mu.Unlock()
	if changed {
		s.fastSchedulerUpdate(acc)
	}

	if s.db == nil {
		return changed
	}

	formatted := expiresAt.Format(time.RFC3339)
	if !changed {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		row, err := s.db.GetAccountByID(ctx, acc.DBID)
		if err != nil {
			log.Printf("[account %d] failed to read subscription_expires_at: %v", acc.DBID, err)
			return changed
		}
		if row.GetCredential("subscription_expires_at") == formatted {
			return changed
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{"subscription_expires_at": formatted}); err != nil {
		log.Printf("[account %d] failed to persist subscription_expires_at: %v", acc.DBID, err)
	}
	return changed
}

// StaleSubscriptionExpiry determines the stale combination of "paid plan + expiry time already passed".
// The only source of the subscription expiry time is the JWT's chatgpt_subscription_active_until. After a renewal, this claim
// is not updated by token refresh, and upstream has no endpoint that returns the new expiry time (wham/usage, as tested, does not include the subscription
// field). Once the plan really expires, the authoritative upstream plan_type drops back to free. So an expiry time that has passed under a paid plan
// must be an old value from before the renewal, and should no longer be shown or persisted as "expired". (issue #360)
func StaleSubscriptionExpiry(planType string, expiresAt time.Time, now time.Time) bool {
	if expiresAt.IsZero() || expiresAt.After(now) {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(planType))
	return plan != "" && plan != "free" && plan != "api"
}

// OnStaleSubscriptionCleared is triggered after a stale expiry time is cleared (inferred as renewed), so the upper layer
// can immediately start an authoritative subscription sync to move "pending confirmation" to "confirmed" as soon as possible. Injected by the proxy package.
var OnStaleSubscriptionCleared func(store *Store, acc *Account)

// ClearStaleSubscriptionExpiresAt clears a stale subscription expiry time after an authoritative paid plan_type is observed upstream
// so that an account that has renewed does not keep showing "expired" for a long time. Returns whether a clear happened. (issue #360)
//
// Clearing is no longer a silent wipe: the sync state switches to pending, the source is recorded as plan_header, and it records
// renewal_detected_at and the last_known_status from before the clear; the frontend uses these to show "renewed · pending confirmation"
// instead of a blank; then an authoritative sync is triggered. Within the grace period (where the subscription provider explicitly gave a grace end time),
// an expiry time that has passed is not treated as stale.
func (s *Store) ClearStaleSubscriptionExpiresAt(acc *Account) bool {
	if s == nil || acc == nil {
		return false
	}
	now := time.Now()
	acc.mu.Lock()
	stale := StaleSubscriptionExpiry(acc.PlanType, acc.SubscriptionExpiresAt, now)
	if stale && !acc.subscriptionMeta.GraceUntil.IsZero() && acc.subscriptionMeta.GraceUntil.After(now) {
		stale = false
	}
	var meta SubscriptionMeta
	if stale {
		lastKnown, _, _ := ComputeSubscriptionBusinessStatus(acc.SubscriptionExpiresAt, time.Time{}, now, time.Local)
		acc.SubscriptionExpiresAt = time.Time{}
		acc.subscriptionMeta.SyncState = SubscriptionSyncPending
		acc.subscriptionMeta.Source = SubscriptionSourcePlanHeader
		acc.subscriptionMeta.LastKnownStatus = lastKnown
		acc.subscriptionMeta.RenewalDetectedAt = now
		acc.subscriptionMeta.Error = ""
		acc.subscriptionMeta.GraceUntil = time.Time{}
		meta = acc.subscriptionMeta
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	}
	acc.mu.Unlock()
	if !stale {
		return false
	}
	s.fastSchedulerUpdate(acc)
	log.Printf("[account %d] plan is still paid but the subscription expiry time has passed (should have been renewed), clearing the stale expiry time and waiting for authoritative confirmation", acc.DBID)
	s.persistSubscriptionMeta(acc.DBID, meta, map[string]interface{}{"subscription_expires_at": ""})
	if hook := OnStaleSubscriptionCleared; hook != nil {
		hook(s, acc)
	}
	return true
}

// UpdateAccountPlanType persists the latest Codex plan type observed from upstream headers.
func (s *Store) UpdateAccountPlanType(acc *Account, planType string) bool {
	return s.updateAccountPlanType(acc, planType, true)
}

// ObserveUsageFramePlan updates runtime routing without writing on a WS reader.
func (s *Store) ObserveUsageFramePlan(acc *Account, planType string) bool {
	return s.updateAccountPlanType(acc, planType, false)
}

func (s *Store) updateAccountPlanType(acc *Account, planType string, persist bool) bool {
	if s == nil || acc == nil {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(planType))
	if plan == "" {
		return false
	}

	acc.mu.Lock()
	changed := acc.PlanType != plan
	if changed {
		acc.PlanType = plan
		acc.usageFramePlanPending = true
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	}
	acc.mu.Unlock()
	if changed {
		s.invalidateRoutingSchedulers()
		s.fastSchedulerUpdate(acc)
	}

	if s.db == nil || !persist {
		return changed
	}

	if err := s.withUsageSnapshotWrite(acc, func() error { return s.persistUsageFramePlan(acc) }); err != nil {
		log.Printf("[account %d] failed to persist plan_type: %v", acc.DBID, err)
	}
	return changed
}

// UpdateAccountIdentity persists account identity observed from upstream usage APIs.
func (s *Store) UpdateAccountIdentity(acc *Account, email, accountID string) bool {
	if s == nil || acc == nil {
		return false
	}
	email = strings.TrimSpace(email)
	accountID = strings.TrimSpace(accountID)
	if email == "" && accountID == "" {
		return false
	}

	fields := make(map[string]interface{}, 2)
	acc.mu.Lock()
	changed := false
	if email != "" && acc.Email != email {
		acc.Email = email
		fields["email"] = email
		changed = true
	}
	if accountID != "" && acc.AccountID != accountID {
		acc.AccountID = accountID
		fields["account_id"] = accountID
		changed = true
	}
	acc.mu.Unlock()

	if s.db == nil || len(fields) == 0 {
		return changed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[account %d] failed to persist account identity: %v", acc.DBID, err)
	}
	return changed
}

// ApplyUsageLimitMetadata applies metadata returned by Codex usage_limit_reached errors.
func (s *Store) ApplyUsageLimitMetadata(acc *Account, planType string, resetAt time.Time) {
	if acc == nil {
		return
	}

	plan := strings.ToLower(strings.TrimSpace(planType))
	now := time.Now()
	fields := make(map[string]interface{})

	acc.mu.Lock()
	planChanged := plan != "" && acc.PlanType != plan
	if plan != "" {
		acc.PlanType = plan
		fields["plan_type"] = plan
	}
	if plan == "free" && !resetAt.IsZero() && resetAt.After(now) {
		acc.UsagePercent7d = 100
		acc.UsagePercent7dValid = true
		acc.Reset7dAt = resetAt
		acc.UsageUpdatedAt = now
		fields["codex_7d_used_percent"] = float64(100)
		fields["codex_7d_reset_at"] = resetAt.Format(time.RFC3339)
		fields["codex_usage_updated_at"] = now.Format(time.RFC3339)
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()
	if planChanged {
		s.invalidateRoutingSchedulers()
	}
	s.fastSchedulerUpdate(acc)

	// For the free plan, arm a "probe at the boundary" at the 7d window reset time, so the progress bar refreshes as soon as the reset happens.
	if plan == "free" {
		s.WakeBoundaryProbe(resetAt)
	}

	if s.db == nil || len(fields) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[account %d] failed to persist usage_limit metadata: %v", acc.DBID, err)
	}
}

// SetUsageProbeFunc registers the active probe callback
func (s *Store) SetUsageProbeFunc(fn func(context.Context, *Account) error) {
	s.usageProbeMu.Lock()
	defer s.usageProbeMu.Unlock()
	s.usageProbe = fn
}

// SetUsageProbeCompletionFunc registers a callback invoked after a batch usage
// probe has fully completed. It lets read-model caches refresh only after all
// per-account state and persistence writes are visible.
func (s *Store) SetUsageProbeCompletionFunc(fn func()) {
	s.usageProbeMu.Lock()
	defer s.usageProbeMu.Unlock()
	s.usageProbeCompletion = fn
}

func (s *Store) finishUsageProbeBatch() {
	s.usageProbeBatch.Store(false)
	s.usageProbeMu.RLock()
	onComplete := s.usageProbeCompletion
	s.usageProbeMu.RUnlock()
	if onComplete != nil {
		onComplete()
	}
}

// TriggerUsageProbeForAccountAsync immediately probes one account without
// waiting for the periodic max-age sweep. ProbeUsageSnapshot sees the account
// in a limited state and starts with WHAM as the zero-cost source of truth.
func (s *Store) TriggerUsageProbeForAccountAsync(account *Account) {
	if s == nil || account == nil {
		return
	}
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil || !account.TryBeginUsageProbe() {
		return
	}

	if !s.startDBBackgroundTask(func(parent context.Context) {
		defer account.FinishUsageProbe()
		ctx, cancel := context.WithTimeout(parent, 25*time.Second)
		defer cancel()
		if err := probeFn(ctx, account); err != nil {
			log.Printf("[account %d] failed to refresh WHAM usage right after Responses rate limiting: %v", account.DBID, err)
		}
	}) {
		account.FinishUsageProbe()
	}
}

// wsAuthVerifyMinInterval limits the minimum interval between WS auth verification probes for the same account,
// so probes are not repeated over and over when the WS upstream closes abnormally at high frequency.
const wsAuthVerifyMinInterval = 30 * time.Second

// VerifyAccountAuthAsync runs, after the WS upstream closes abnormally (such as close 1008 policy violation),
// one usage probe for the single account asynchronously (wham first, zero quota cost).
//
// Background: on the HTTP channel, an invalid token returns 401 → goes through applyCooldown to mark an unauthorized cooldown;
// but on the WS channel, upstream kicks the connection with close 1008, which is classified as an ordinary transport failure, so the account is not banned
// and stays in the account pool failing repeatedly. Here one probe turns the "invisible 401" into the same handling as HTTP:
// when the wham probe gets a 401, the /responses fallback probe decides; only when the fallback also hits 401 does MarkCooldownWithError run
// (a 401 from wham alone does not convict, to avoid wrongly banning codex_at accounts where wham always returns 401 but traffic works, issue #328);
// if the 1008 was only triggered by a content policy or network jitter, the probe returns normally and nothing is wrongly banned. Throttled with a minimum interval.
func (s *Store) VerifyAccountAuthAsync(account *Account) {
	if s == nil || account == nil {
		return
	}
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	now := time.Now()
	account.mu.Lock()
	if !account.lastAuthVerifyAt.IsZero() && now.Sub(account.lastAuthVerifyAt) < wsAuthVerifyMinInterval {
		account.mu.Unlock()
		return
	}
	account.lastAuthVerifyAt = now
	account.mu.Unlock()

	if !account.TryBeginUsageProbe() {
		return
	}
	if !s.startDBBackgroundTask(func(parent context.Context) {
		defer account.FinishUsageProbe()
		ctx, cancel := context.WithTimeout(parent, 25*time.Second)
		defer cancel()
		if err := probeFn(ctx, account); err != nil {
			log.Printf("[account %d] auth verification probe failed after WS upstream closed abnormally: %v", account.DBID, err)
		}
	}) {
		account.FinishUsageProbe()
	}
}

// TriggerUsageProbeAsync asynchronously triggers one batch usage probe
func (s *Store) TriggerUsageProbeAsync() {
	if !s.usageProbeBatch.CompareAndSwap(false, true) {
		return
	}

	if !s.startDBBackgroundTask(func(ctx context.Context) {
		defer s.finishUsageProbeBatch()
		s.parallelProbeUsage(ctx)
	}) {
		s.usageProbeBatch.Store(false)
	}
}

// WakeBoundaryProbe tells the "probe at the boundary" scheduler that an account's rate-limit cooldown / window reset boundary has changed,
// so an earlier boundary than the currently armed one may exist and the timer needs rescheduling. at is that boundary's time (IsZero means unknown,
// forces a reschedule). The background goroutine is disturbed only when at is strictly earlier than the currently armed boundary (or nothing is armed), to avoid
// pointless reschedules caused by frequent 429s or traffic refreshes. This method does just one non-blocking channel write, so calling it under any lock is safe.
func (s *Store) WakeBoundaryProbe(at time.Time) {
	if s == nil || s.boundaryProbeWakeCh == nil {
		return
	}
	if !at.IsZero() {
		if !at.After(time.Now()) {
			return // the boundary has already passed; leave it to the regular background check or probe
		}
		armed := s.armedBoundaryAt.Load()
		if armed != 0 && at.UnixNano() >= armed {
			return // a wake-up plan that is earlier or at the same time already exists; when the timer fires it rescans and takes over the later boundaries
		}
	}
	select {
	case s.boundaryProbeWakeCh <- struct{}{}:
	default:
	}
}

// armNextBoundaryProbe scans all accounts, finds the nearest "probe at the boundary" and reschedules the timer to that moment
// (plus a probeBoundaryLag delay). When there is no pending boundary, the timer is stopped. Called only inside the background refresh goroutine
// (that goroutine holds no account locks, so taking RLock per account here cannot deadlock).
func (s *Store) armNextBoundaryProbe(timer *time.Timer) {
	now := time.Now()
	accounts := s.accountSnapshotAccounts()

	var next time.Time
	for _, acc := range accounts {
		if t, ok := acc.nextProbeBoundary(now); ok {
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}

	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if next.IsZero() {
		s.armedBoundaryAt.Store(0)
		return
	}
	s.armedBoundaryAt.Store(next.UnixNano())
	d := time.Until(next) + probeBoundaryLag
	if d < 0 {
		d = probeBoundaryLag
	}
	timer.Reset(d)
}

// TriggerRecoveryProbeAsync asynchronously triggers one recovery probe for banned accounts
func (s *Store) TriggerRecoveryProbeAsync() {
	if s.GetLazyMode() {
		return
	}
	if !s.recoveryProbeBatch.CompareAndSwap(false, true) {
		return
	}

	if !s.startDBBackgroundTask(func(ctx context.Context) {
		defer s.recoveryProbeBatch.Store(false)
		s.parallelRecoveryProbe(ctx)
	}) {
		s.recoveryProbeBatch.Store(false)
	}
}

// TriggerAutoCleanupAsync asynchronously triggers one automatic cleanup check
func (s *Store) TriggerAutoCleanupAsync() {
	if !s.autoCleanupBatch.CompareAndSwap(false, true) {
		return
	}

	if !s.startDBBackgroundTask(func(ctx context.Context) {
		defer s.autoCleanupBatch.Store(false)
		s.runAutoCleanupSweep(ctx)
	}) {
		s.autoCleanupBatch.Store(false)
	}
}

func (s *Store) runAutoCleanupSweep(ctx context.Context) {
	if !s.GetAutoCleanUnauthorized() && !s.GetAutoCleanRateLimited() && !s.GetAutoCleanError() {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cleanedUnauthorized := 0
	cleanedRateLimited := 0
	cleanedError := 0

	if s.GetAutoCleanUnauthorized() {
		cleanedUnauthorized = s.CleanByRuntimeStatus(cleanupCtx, "unauthorized")
	}
	if s.GetAutoCleanRateLimited() {
		cleanedRateLimited = s.CleanByRuntimeStatus(cleanupCtx, "rate_limited")
	}
	if s.GetAutoCleanError() {
		cleanedError = s.CleanByRuntimeStatus(cleanupCtx, "error")
	}

	if cleanedUnauthorized > 0 || cleanedRateLimited > 0 || cleanedError > 0 {
		log.Printf("auto cleanup finished: unauthorized=%d, rate_limited=%d, error=%d", cleanedUnauthorized, cleanedRateLimited, cleanedError)
	}
}

// CleanFullUsageAccounts cleans up accounts whose usage has reached 100% (skipping accounts that are handling requests)
func (s *Store) CleanFullUsageAccounts(ctx context.Context) int {
	accounts := s.accountSnapshotAccounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil {
			continue
		}

		// Skip locked accounts in automatic cleanup
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		// Skip accounts that are handling requests
		if acc.ActiveRequests.Load() > 0 {
			continue
		}

		// When the usage window is only a display reference for this account (usage limits / reset coupons skip the window),
		// the snapshot is not grounds for "account exhausted", so no automatic cleanup is done.
		if acc.SkipsUsageWindowLimits() {
			continue
		}

		// Check whether usage is >= 100%
		pct, valid := acc.GetUsagePercent7d()
		if !valid || pct < 100.0 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[account %d] failed to clean up account with full usage: %v", acc.DBID, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		log.Printf("[account %d] usage %.1f%% is full, automatically cleaned up (email=%s)", acc.DBID, pct, acc.Email)
		if s.db != nil {
			if err := s.db.InsertAccountEvent(ctx, acc.DBID, "deleted", "clean_full_usage"); err != nil {
				log.Printf("[account %d] failed to record full-usage cleanup event: %v", acc.DBID, err)
			}
		}
		cleaned++
	}

	if cleaned > 0 {
		log.Printf("usage cleanup finished: cleaned %d full-usage accounts", cleaned)
	}
	return cleaned
}

// CleanExpiredAccounts cleans up accounts that have been in the pool longer than the given duration (whether or not they have been used)
// Batch optimization: first collect all expired IDs, then finish the database update and in-memory removal in one go
func (s *Store) CleanExpiredAccounts(ctx context.Context, maxAge time.Duration) int {
	accounts := s.accountSnapshotAccounts()
	now := time.Now()
	cutoff := now.Add(-maxAge).UnixNano()

	// 1. Collect the IDs of all accounts that need cleanup
	var expiredIDs []int64
	var skipNoAddedAt, skipNotExpired, skipActive, skipProven int
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		// Skip locked accounts in automatic cleanup
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}
		addedAt := acc.AddedAt.Load()
		if addedAt == 0 {
			skipNoAddedAt++
			continue
		}
		if addedAt > cutoff {
			skipNotExpired++
			continue
		}
		if acc.ActiveRequests.Load() > 0 {
			skipActive++
			continue
		}
		// Keep accounts with more than 10 successful requests; do not expire-clean them
		if acc.TotalRequests.Load() > 10 {
			skipProven++
			continue
		}
		expiredIDs = append(expiredIDs, acc.DBID)
	}

	log.Printf("expired cleanup scan: total=%d, pending cleanup=%d, skipped (no time=%d, not expired=%d, in progress=%d, verified=%d)",
		len(accounts), len(expiredIDs), skipNoAddedAt, skipNotExpired, skipActive, skipProven)

	if len(expiredIDs) == 0 {
		return 0
	}

	log.Printf("expired cleanup: found %d timed-out accounts, starting batch processing", len(expiredIDs))

	// 2. Batch-update the database state
	if s.db != nil {
		if err := s.db.BatchSoftDeleteAccounts(ctx, expiredIDs); err != nil {
			log.Printf("expired cleanup: batch database update failed: %v, falling back to per-item processing", err)
			return s.cleanExpiredFallback(ctx, expiredIDs)
		}
	}

	// 3. Batch-remove from the in-memory pool
	s.RemoveAccounts(expiredIDs)

	// 4. Batch-write event logs; the cleanup itself already runs in a background task, so keep the same lifecycle.
	if s.db != nil {
		if err := s.db.BatchInsertAccountEvents(ctx, expiredIDs, "deleted", "clean_expired"); err != nil {
			log.Printf("expired cleanup: failed to record batch events: %v", err)
		}
	}

	log.Printf("expired cleanup finished: cleaned %d timed-out accounts", len(expiredIDs))
	return len(expiredIDs)
}

// cleanExpiredFallback falls back to per-item processing when the batch operation fails
func (s *Store) cleanExpiredFallback(ctx context.Context, ids []int64) int {
	cleaned := 0
	for _, id := range ids {
		if err := s.db.SoftDeleteAccount(ctx, id); err != nil {
			log.Printf("[account %d] expired cleanup failed: %v", id, err)
			continue
		}
		s.RemoveAccount(id)
		if err := s.db.InsertAccountEvent(ctx, id, "deleted", "clean_expired"); err != nil {
			log.Printf("[account %d] failed to record expired cleanup event: %v", id, err)
		}
		cleaned++
	}
	if cleaned > 0 {
		log.Printf("expired cleanup (fallback): cleaned %d timed-out accounts", cleaned)
	}
	return cleaned
}

// RemoveAccounts removes accounts from the in-memory pool in batch (one lock, one pass, avoiding O(n²))
func (s *Store) RemoveAccounts(dbIDs []int64) {
	if len(dbIDs) == 0 {
		return
	}

	s.accountMutationMu.Lock()
	defer s.accountMutationMu.Unlock()

	removeSet := make(map[int64]struct{}, len(dbIDs))
	for _, id := range dbIDs {
		removeSet[id] = struct{}{}
	}

	removedIDs := make([]int64, 0, len(removeSet))
	s.mu.Lock()
	kept := s.accounts[:0]
	for _, acc := range s.accounts {
		if _, remove := removeSet[acc.DBID]; remove {
			removedIDs = append(removedIDs, acc.DBID)
		} else {
			kept = append(kept, acc)
		}
	}
	s.accounts = kept
	s.rebuildAccountIndex()
	s.publishAccountSnapshot(s.accounts)
	s.mu.Unlock()

	refreshScheduler := s.GetRefreshScheduler()
	for _, dbID := range removedIDs {
		s.fastSchedulerRemove(dbID)
		if refreshScheduler != nil {
			refreshScheduler.CancelTask(dbID)
		}
	}
	s.invalidateRoutingSchedulers()
}

func (s *Store) parallelProbeUsage(ctx context.Context) {
	s.parallelProbeUsageWith(ctx, s.GetUsageProbeMaxAge())
}

// parallelProbeUsageWith runs one batch usage probe with the given maxAge threshold.
// maxAge<=0 means "probe immediately": refresh once as long as the account can run.
func (s *Store) parallelProbeUsageWith(ctx context.Context, maxAge time.Duration) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	accounts := s.accountSnapshotAccounts()

	sem := make(chan struct{}, s.GetUsageProbeConcurrency())
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsUsageProbe(maxAge) {
			continue
		}
		if !acc.TryBeginUsageProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishUsageProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[account %d] usage probe failed: %v", account.DBID, err)
			}
		}(acc)
	}

	wg.Wait()
}

// TriggerUsageProbeForceAsync asynchronously triggers one batch usage probe that "ignores the cache threshold".
// Used for manual refresh from the admin panel.
func (s *Store) TriggerUsageProbeForceAsync() {
	if !s.usageProbeBatch.CompareAndSwap(false, true) {
		return
	}

	if !s.startDBBackgroundTask(func(ctx context.Context) {
		defer s.finishUsageProbeBatch()
		s.parallelProbeUsageWith(ctx, 0)
	}) {
		s.usageProbeBatch.Store(false)
	}
}

func (s *Store) parallelRecoveryProbe(ctx context.Context) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	accounts := s.accountSnapshotAccounts()

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsRecoveryProbe(s.GetRecoveryProbeInterval()) {
			continue
		}
		if !acc.TryBeginRecoveryProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishRecoveryProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if account.NeedsRefresh() {
				if err := s.refreshAccount(probeCtx, account); err != nil {
					log.Printf("[account %d] refresh failed before recovery probe: %v", account.DBID, err)
				}
			}

			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[account %d] recovery probe failed: %v", account.DBID, err)
			} else {
				// Accounts whose usage is exhausted do not have their state reset
				account.mu.RLock()
				exhausted := account.usageExhaustedLocked()
				account.mu.RUnlock()
				if exhausted {
					log.Printf("[account %d] recovery probe succeeded but usage is exhausted, keeping the current state", account.DBID)
				} else {
					// Probe succeeded: upgrade the account from banned to warm, giving it a chance to be scheduled again
					atomic.StoreInt32(&account.Disabled, 0) // clear the atomic disabled flag
					account.mu.Lock()
					if account.HealthTier == HealthTierBanned {
						account.HealthTier = HealthTierWarm
						account.SchedulerScore = 80
						account.FailureStreak = 0
						account.SuccessStreak = 1
						account.LastSuccessAt = time.Now()
						if account.Status == StatusCooldown {
							account.Status = StatusReady
							account.CooldownUtil = time.Time{}
							account.CooldownReason = ""
						}
						account.recomputeSchedulerLocked(s.maxConcurrency.Load())
						log.Printf("[account %d] recovery probe succeeded! upgraded from banned to warm", account.DBID)
					}
					account.mu.Unlock()
					// Clear the database cooldown state
					s.persistAccountCooldownState(account, true, false)
				}
			}
		}(acc)
	}

	wg.Wait()
}

// RefreshSingle refreshes a single account (called by the admin handler)
func (s *Store) RefreshSingle(ctx context.Context, dbID int64) error {
	s.mu.RLock()
	var target *Account
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			target = acc
			break
		}
	}
	s.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("account %d does not exist", dbID)
	}
	return s.refreshAccountForced(ctx, target)
}

// RefreshSingleAsync performs a forced refresh under the database lifecycle.
// It is intended for detached recovery paths such as an upstream 401 handler.
func (s *Store) RefreshSingleAsync(dbID int64) {
	if s == nil || dbID <= 0 {
		return
	}
	s.startDBBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		if err := s.RefreshSingle(ctx, dbID); err != nil {
			log.Printf("[account %d] asynchronous forced refresh failed: %v", dbID, err)
		}
	})
}

// AccountCount returns the number of accounts
func (s *Store) AccountCount() int {
	return len(s.accountSnapshotAccounts())
}

// AvailableCount returns the number of available accounts
func (s *Store) AvailableCount() int {
	count := 0
	lazy := s.GetLazyMode()
	for _, acc := range s.accountSnapshotAccounts() {
		if (lazy && s.accountLazySelectable(acc)) || (!lazy && acc.IsAvailable()) {
			count++
		}
	}
	return count
}

// HealthCountsNonBlocking returns best-effort account counts without waiting on
// hot request-path locks. It is intended for liveness endpoints.
func (s *Store) HealthCountsNonBlocking() (available int, total int, complete bool) {
	if s == nil {
		return 0, 0, true
	}
	if !s.mu.TryRLock() {
		return -1, -1, false
	}
	s.mu.RUnlock()
	accounts := s.accountSnapshotAccounts()

	complete = true
	total = len(accounts)
	now := time.Now()
	lazy := s.GetLazyMode()
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if atomic.LoadInt32(&acc.Disabled) != 0 || (atomic.LoadInt32(&acc.DispatchPaused) != 0 || atomic.LoadInt32(&acc.IntelligencePaused) != 0) {
			continue
		}
		if !acc.mu.TryRLock() {
			complete = false
			continue
		}
		// Mirror AvailableCount: lazy mode accepts refresh/session-token-only
		// accounts that hydrate on demand, otherwise a healthy lazy pool would
		// report zero available.
		if (lazy && acc.lazySelectableLocked(now)) || (!lazy && acc.isAvailableLocked(now)) {
			available++
		}
		acc.mu.RUnlock()
	}
	return available, total, complete
}

// Accounts returns all accounts (used for statistics)
func (s *Store) Accounts() []*Account {
	accounts := s.accountSnapshotAccounts()
	result := make([]*Account, len(accounts))
	copy(result, accounts)
	return result
}

// ==================== parallel refresh ====================

// parallelRefreshAll refreshes all accounts that need refreshing in parallel (worker pool, concurrency 10)
func (s *Store) parallelRefreshAll(ctx context.Context) {
	accounts := s.accountSnapshotAccounts()

	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i, acc := range accounts {
		if !s.shouldBackgroundRefresh(acc, s.GetLazyMode()) {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, account *Account) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.refreshAccount(ctx, account); err != nil {
				log.Printf("[account %d] refresh failed: %v", idx+1, err)
			} else {
				log.Printf("[account %d] refresh succeeded: email=%s", idx+1, account.Email)
			}
		}(i, acc)
	}
	wg.Wait()
}

func (s *Store) refreshAccount(ctx context.Context, acc *Account) error {
	return s.refreshAccountWithOptions(ctx, acc, false)
}

func (s *Store) refreshAccountForced(ctx context.Context, acc *Account) error {
	return s.refreshAccountWithOptions(ctx, acc, true)
}

// refreshAccountWithOptions refreshes a single account's AT (with the cache lock and token cache)
func (s *Store) refreshAccountWithOptions(ctx context.Context, acc *Account, forceRefresh bool) error {
	return s.refreshCodexAccount(ctx, acc, forceRefresh)
}

// propagateSharedOAuthCredentials syncs the new credentials from a successful refresh to the sibling workspace routes that use the same old RT
// Only the auth material and the token-native identity are synced; each route's own
// Chatgpt-Account-Id, proxy, group, usage, cooldown and scheduling settings stay unchanged.
func (s *Store) propagateSharedOAuthCredentials(
	ctx context.Context,
	source *Account,
	previousRefreshToken string,
	td *TokenData,
	credentials map[string]interface{},
	ttl time.Duration,
) {
	if s == nil || source == nil || td == nil || strings.TrimSpace(previousRefreshToken) == "" {
		return
	}
	source.mu.RLock()
	sourceID := source.DBID
	sourceAccountID := source.AccountID
	sourceEmail := source.Email
	sourcePlanType := source.PlanType
	sourceSubscriptionExpiresAt := source.SubscriptionExpiresAt
	sourceSubscriptionMeta := source.subscriptionMeta
	source.mu.RUnlock()

	for _, sibling := range s.accountSnapshotAccounts() {
		if sibling == nil || sibling.DBID == sourceID {
			continue
		}

		sibling.mu.Lock()
		if strings.TrimSpace(sibling.RefreshToken) != strings.TrimSpace(previousRefreshToken) {
			sibling.mu.Unlock()
			continue
		}
		sibling.AccessToken = td.AccessToken
		if td.RefreshToken != "" {
			sibling.RefreshToken = td.RefreshToken
		}
		if sessionToken, ok := credentials["session_token"].(string); ok {
			sibling.SessionToken = sessionToken
		}
		sibling.ExpiresAt = td.ExpiresAt
		sibling.ErrorMsg = ""
		if sourceAccountID != "" {
			sibling.AccountID = sourceAccountID
		}
		if sourceEmail != "" {
			sibling.Email = sourceEmail
		}
		if sourcePlanType != "" {
			sibling.PlanType = sourcePlanType
		}
		sibling.SubscriptionExpiresAt = sourceSubscriptionExpiresAt
		sibling.subscriptionMeta = sourceSubscriptionMeta
		sibling.recomputeSchedulerLocked(s.maxConcurrency.Load())
		sibling.mu.Unlock()
		s.fastSchedulerUpdate(sibling)

		if s.db != nil {
			if err := s.db.UpdateCredentials(ctx, sibling.DBID, credentials); err != nil {
				log.Printf("[account %d] failed to sync shared OAuth credentials: %v", sibling.DBID, err)
			}
		}
		if s.tokenCache != nil && ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, sibling.DBID, td.AccessToken, ttl)
		}
		log.Printf("[account %d] synced shared OAuth credentials refreshed by account %d; workspace routes stay independent", sibling.DBID, sourceID)
	}
	if sourcePlanType != "" {
		s.invalidateRoutingSchedulers()
	}
}
