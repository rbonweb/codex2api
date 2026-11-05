package proxy

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

const (
	ClientCompatModePreserve = "preserve"
	ClientCompatModeAuto     = "auto"
	ClientCompatModeForce    = "force"

	StreamFlushPolicyImmediate = "immediate"
	StreamFlushPolicyCoalesce  = "coalesce"

	// FirstTokenModeStrict is retired: first-token stats are now always recorded on the loose basis,
	// the constant is kept only for compatibility normalization of old configuration and old stored values.
	FirstTokenModeStrict = "strict"
	FirstTokenModeLoose  = "loose"

	BillingTierPolicyActual    = "actual"
	BillingTierPolicyRequested = "requested"

	// RequestIsolationMode values:
	//   isolated   — requests without an explicit session isolate the upstream identity per request (the default);
	//   per-api-key — requests without an explicit session share the upstream identity by downstream API Key (restores the old v2 behavior,
	//                  keeping implicit prompt cache hits).
	// Override the default with the environment variable CODEX_REQUEST_ISOLATION_MODE.
	RequestIsolationModeIsolated  = "isolated"
	RequestIsolationModePerAPIKey = "per-api-key"

	defaultClientCompatMode      = ClientCompatModePreserve
	defaultCodexMinCLIVersion    = "0.160.0"
	defaultStreamFlushPolicy     = StreamFlushPolicyImmediate
	defaultStreamFlushIntervalMS = 20
	minStreamFlushIntervalMS     = 1
	maxStreamFlushIntervalMS     = 1000
	defaultFirstTokenMode        = FirstTokenModeLoose
	defaultFirstTokenTimeoutSec  = 0
	maxFirstTokenTimeoutSec      = 600
	defaultBillingTierPolicy     = BillingTierPolicyActual
	// defaultCodexRequestCompression matches the real Codex CLI: for HTTP /responses request bodies to the ChatGPT backend it compresses with zstd by default,
	// so the gateway also compresses by default.
	defaultCodexRequestCompression = true
	defaultCodexWSHideErrors       = true
	defaultCodexWSSilentRetry      = true
	defaultCodexWSSilentRetries    = 2
	defaultCodexWSSizeRouter       = true
	maxCodexWSSilentRetries        = 10
	defaultCodexWSBusyMaxWaitSec   = 30
	defaultCodexWSBusyPatienceSec  = 2
	maxCodexWSBusyWaitSec          = 300
	defaultCodexWSStatelessSlots   = 8
	maxCodexWSStatelessSlots       = 32

	defaultCodexContinueMaxRounds = 8
	minCodexContinueMaxRounds     = 1
	maxCodexContinueMaxRounds     = 32
)

type RuntimeSettings struct {
	ClientCompatMode      string
	CodexMinCLIVersion    string
	CodexUserAgentConfig  string
	CodexTelemetryEnabled bool
	// CodexTelemetryTimingDebug enables the temporary timing probe of simulated telemetry (logs only, off by default).
	CodexTelemetryTimingDebug bool
	// CodexImagesMainModel: when empty, falls back to the environment variable or the built-in image-generation text-driving model.
	CodexImagesMainModel  string
	StreamFlushPolicy     string
	StreamFlushIntervalMS int
	FirstTokenMode        string
	FirstTokenTimeoutSec  int
	BillingTierPolicy     string
	// ModelsListReadMaxBytes is the read limit for successful responses from the upstream /v1/models and the Codex model list.
	ModelsListReadMaxBytes int64
	CodexForceWebsocket    bool // force the Codex upstream to use WebSocket (default false)
	// CodexRequestCompression zstd-compresses the HTTP /responses request body (default true,
	// consistent with the real Codex CLI). It is orthogonal to CodexForceWebsocket: the WS path uses
	// permessage-deflate (the dialer has it enabled); this item only applies to the HTTP path, and both can take effect together.
	CodexRequestCompression bool
	// CodexWSWeakNetworkMode takes a conservative approach to reuse for unstable links such as VPN / residential proxies:
	// shorten idle and max lifetime, do a real Ping/Pong on every reuse, and pause idle keepalive (default false).
	CodexWSWeakNetworkMode bool
	CodexWSHideErrors      bool                           // hide raw Codex WS upstream errors (default true)
	CodexWSSilentRetry     bool                           // on a Codex WS upstream error before the first packet, silently retry on a different account (default true)
	CodexWSSilentRetries   int                            // maximum number of silent account-switch retries for Codex WS (default 2)
	ContinuousRetryPolicy  database.ContinuousRetryPolicy // selector for continuous retry on upstream errors (default off)
	CodexWSSizeRouter      bool                           // 1009 self-learning size routing: oversized requests are sent over HTTP first (default true)
	CodexWSBusyMaxWaitSec  int                            // cumulative upper limit in seconds for busy session/capacity waits (default 30, issue #413)
	CodexWSBusyOverflow    bool                           // busy session overflows to a sibling connection on the same account (default false)
	CodexWSBusyPatienceSec int                            // short wait in seconds before overflow is triggered (default 2)
	// CodexWSStatelessSlots limits the per-account blank connection budget and the scan window of the shared pool.
	CodexWSStatelessSlots int
	// CodexWSDownstreamKeepaliveSlots separately limits the idle chat connections kept per account after output finishes.
	CodexWSDownstreamKeepaliveSlots int
	// GithubToken is the Personal Access Token used for api.github.com requests (raises the API rate-limit quota;
	// sent only to api.github.com, never to mirrors or other hosts; empty means not configured, issue #522).
	GithubToken string
	// GithubProxyURL is the outbound proxy dedicated to GitHub domains; empty means fall back to the caller's default proxy (issue #522).
	GithubProxyURL string
	// Codex overload circuit breaker: when, within a single account's sliding window, the share of server_is_overloaded errors reaches the threshold and the sample count
	// is sufficient, scheduling for that account is automatically paused for a period (default off).
	CodexOverloadPauseEnabled     bool
	CodexOverloadThresholdPercent int // trigger ratio (%), default 20
	CodexOverloadPauseMinutes     int // pause duration (minutes), default 30
	CodexOverloadWindowMinutes    int // statistics window (minutes), default 5
	// OverflowAutoCompact: when the context overflows the window, automatically summarize old turns and retry once (experimental, default false, issue #415).
	// The global switch and the per-key limits.auto_compact_overflow are combined with OR.
	OverflowAutoCompact bool
	// CompactViaResponses rewrites the pass-through of /v1/responses/compact for official Codex OAuth accounts
	// into the body-signal form of /responses + compaction_trigger and aggregates it into a single JSON response
	// (the upstream has retired the dedicated compact endpoint; default false). Relay accounts are not affected.
	CompactViaResponses bool
	// CodexPreflightSSEPassthrough passes the preflight metadata events (codex.rate_limits /
	// codex.response.metadata / response.metadata) straight through to the downstream instead of buffering them up to the first content
	// event (legacy compatibility, default false, issue #425). When on, a 200 is committed early before the first content,
	// so a response.failed inside that window can only be sent as an SSE failed frame; real error codes, silent account switching,
	// and overflow-compaction retries are not available.
	CodexPreflightSSEPassthrough bool
	// FirstTokenExcludesWsAcquire decides whether the stored first_token_ms subtracts this attempt's
	// WS acquisition time (default false, keeping the original basis that includes acquisition; raw value = first_token_ms + ws_acquire_ms).
	FirstTokenExcludesWsAcquire bool
	// CodexContinueThinking: when the upstream is detected truncating thinking with the 518n-2 fingerprint, automatically continue thinking and fold it into a single response (default false).
	CodexContinueThinking  bool
	CodexContinueMaxRounds int // maximum continue-thinking rounds for a single request, including the first round (default 8, range 1-32)
	// RequestIsolationMode controls the granularity of upstream identity isolation for requests without an explicit session (isolated|per-api-key, default isolated).
	RequestIsolationMode string
	// CodexSyncedCLIVersion is the latest Codex CLI version synced from openai/codex releases;
	// used to raise the simulated version of the outbound UA / manifest; never lower than the built-in constant; empty means not synced.
	CodexSyncedCLIVersion          string
	CodexSyncedDesktopMacBuild     string
	CodexSyncedDesktopWindowsBuild string
	CodexSyncedVSCodeBuild         string
	// CodexCLIVersionSyncEnabled controls the background scheduled sync of the Codex CLI version (default true).
	CodexCLIVersionSyncEnabled bool
	// CodexCLIVersionSyncIntervalHours is the scheduled sync interval (hours, default 12, range 1-720).
	CodexCLIVersionSyncIntervalHours int
	// AutoResetCreditsEnabled controls the automatic use of Plus/Pro reset credits when they are about to expire (default false).
	AutoResetCreditsEnabled             bool
	AutoResetCreditsOnExhaustionEnabled bool
	// AutoResetCreditsBeforeExpiryMin is how many minutes before expiry the auto-use window opens (default 60).
	AutoResetCreditsBeforeExpiryMin int
	// AutoActivate5hWindowEnabled controls whether, after the 5h window resets, a minimal real /responses is sent once to start the next window (default false, issue #581).
	AutoActivate5hWindowEnabled bool
	// UTLSShutdownTimeoutMin is the upper limit on waiting for in-flight streams on a uTLS connection (CODEX_TRANSPORT_MODE=utls_chrome) to finish after the connection is removed
	// from the connection pool (minutes, default 30, range 1-240).
	// After the timeout it is force-closed, so a stream that hangs abnormally cannot keep the connection forever (issue #446).
	UTLSShutdownTimeoutMin int
}

// UTLSShutdownTimeout returns the upper limit for a graceful uTLS connection shutdown wait.
func (s RuntimeSettings) UTLSShutdownTimeout() time.Duration {
	return time.Duration(database.NormalizeUTLSShutdownTimeoutMinutes(s.UTLSShutdownTimeoutMin)) * time.Minute
}

// IsolateRequestsByDefault returns whether requests without an explicit session are isolated per request by default.
// Only the per-api-key mode returns false (restoring the old behavior of sharing the cache by API Key).
func (s RuntimeSettings) IsolateRequestsByDefault() bool {
	return NormalizeRequestIsolationMode(s.RequestIsolationMode) != RequestIsolationModePerAPIKey
}

var (
	runtimeSettings         atomic.Value // stores RuntimeSettings
	runtimeSettingsUpdateMu sync.Mutex
)

func init() {
	runtimeSettings.Store(DefaultRuntimeSettings())
}

func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		ClientCompatMode:                 defaultClientCompatMode,
		CodexMinCLIVersion:               defaultCodexMinCLIVersion,
		CodexUserAgentConfig:             DefaultCodexUserAgentConfigJSON(),
		CodexTelemetryEnabled:            false,
		CodexTelemetryTimingDebug:        false,
		StreamFlushPolicy:                defaultStreamFlushPolicy,
		StreamFlushIntervalMS:            defaultStreamFlushIntervalMS,
		FirstTokenMode:                   defaultFirstTokenMode,
		FirstTokenTimeoutSec:             defaultFirstTokenTimeoutSec,
		BillingTierPolicy:                defaultBillingTierPolicy,
		ModelsListReadMaxBytes:           database.DefaultModelsListReadMaxBytes,
		CodexRequestCompression:          defaultCodexRequestCompression,
		CodexWSHideErrors:                defaultCodexWSHideErrors,
		CodexWSSilentRetry:               defaultCodexWSSilentRetry,
		CodexWSSilentRetries:             defaultCodexWSSilentRetries,
		ContinuousRetryPolicy:            database.DefaultContinuousRetryPolicy(),
		CodexWSSizeRouter:                defaultCodexWSSizeRouter,
		CodexWSBusyMaxWaitSec:            defaultCodexWSBusyMaxWaitSec,
		CodexWSBusyPatienceSec:           defaultCodexWSBusyPatienceSec,
		CodexWSStatelessSlots:            defaultCodexWSStatelessSlots,
		CodexWSDownstreamKeepaliveSlots:  database.DefaultCodexWSDownstreamKeepaliveSlots,
		CodexOverloadThresholdPercent:    database.NormalizeCodexOverloadThresholdPercent(0),
		CodexOverloadPauseMinutes:        database.NormalizeCodexOverloadPauseMinutes(0),
		CodexOverloadWindowMinutes:       database.NormalizeCodexOverloadWindowMinutes(0),
		CodexContinueMaxRounds:           defaultCodexContinueMaxRounds,
		RequestIsolationMode:             defaultRequestIsolationMode(),
		CodexCLIVersionSyncEnabled:       true,
		CodexCLIVersionSyncIntervalHours: 12,
		AutoResetCreditsBeforeExpiryMin:  60,
		UTLSShutdownTimeoutMin:           database.NormalizeUTLSShutdownTimeoutMinutes(0),
	}
}

// defaultRequestIsolationMode parses the default isolation mode from the environment variable; when unset it isolates per request.
// CODEX_REQUEST_ISOLATION_MODE=per-api-key (or per_api_key / shared / cache) switches back to the old
// behavior of sharing the cache per API Key, as an escape valve for deployments that depend on implicit cache hits.
func defaultRequestIsolationMode() string {
	return NormalizeRequestIsolationMode(os.Getenv("CODEX_REQUEST_ISOLATION_MODE"))
}

// NormalizeRequestIsolationMode normalizes the isolation mode; empty or unknown values fall back to isolated.
func NormalizeRequestIsolationMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RequestIsolationModePerAPIKey, "per_api_key", "per-apikey", "shared", "cache":
		return RequestIsolationModePerAPIKey
	default:
		return RequestIsolationModeIsolated
	}
}

func NormalizeClientCompatMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ClientCompatModePreserve:
		return ClientCompatModePreserve
	case ClientCompatModeAuto:
		return ClientCompatModeAuto
	case ClientCompatModeForce:
		return ClientCompatModeForce
	default:
		return ClientCompatModePreserve
	}
}

func NormalizeStreamFlushPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", StreamFlushPolicyImmediate:
		return StreamFlushPolicyImmediate
	case StreamFlushPolicyCoalesce:
		return StreamFlushPolicyCoalesce
	default:
		return StreamFlushPolicyImmediate
	}
}

// NormalizeFirstTokenMode always returns the loose basis: the strict first-token switch has been removed, and old stored or old request
// values that still carry strict or any invalid value take effect as loose.
func NormalizeFirstTokenMode(_ string) string {
	return FirstTokenModeLoose
}

func NormalizeBillingTierPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", BillingTierPolicyActual:
		return BillingTierPolicyActual
	case BillingTierPolicyRequested:
		return BillingTierPolicyRequested
	default:
		return BillingTierPolicyActual
	}
}

func NormalizeRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	defaults := DefaultRuntimeSettings()
	settings.ClientCompatMode = NormalizeClientCompatMode(settings.ClientCompatMode)
	settings.StreamFlushPolicy = NormalizeStreamFlushPolicy(settings.StreamFlushPolicy)
	settings.FirstTokenMode = NormalizeFirstTokenMode(settings.FirstTokenMode)
	settings.BillingTierPolicy = NormalizeBillingTierPolicy(settings.BillingTierPolicy)
	settings.ModelsListReadMaxBytes = database.NormalizeModelsListReadMaxBytes(settings.ModelsListReadMaxBytes)
	settings.RequestIsolationMode = NormalizeRequestIsolationMode(settings.RequestIsolationMode)
	settings.CodexImagesMainModel, _ = NormalizeImagesMainModel(settings.CodexImagesMainModel)
	if strings.TrimSpace(settings.CodexMinCLIVersion) == "" {
		settings.CodexMinCLIVersion = defaults.CodexMinCLIVersion
	} else {
		settings.CodexMinCLIVersion = strings.TrimSpace(settings.CodexMinCLIVersion)
	}
	if normalized, err := NormalizeCodexUserAgentConfigJSON(settings.CodexUserAgentConfig); err == nil {
		settings.CodexUserAgentConfig = normalized
	} else {
		settings.CodexUserAgentConfig = defaults.CodexUserAgentConfig
	}
	if settings.StreamFlushIntervalMS < minStreamFlushIntervalMS {
		settings.StreamFlushIntervalMS = defaults.StreamFlushIntervalMS
	}
	if settings.StreamFlushIntervalMS > maxStreamFlushIntervalMS {
		settings.StreamFlushIntervalMS = maxStreamFlushIntervalMS
	}
	if settings.FirstTokenTimeoutSec < 0 {
		settings.FirstTokenTimeoutSec = defaultFirstTokenTimeoutSec
	}
	if settings.FirstTokenTimeoutSec > maxFirstTokenTimeoutSec {
		settings.FirstTokenTimeoutSec = maxFirstTokenTimeoutSec
	}
	if settings.CodexWSSilentRetries < 0 {
		settings.CodexWSSilentRetries = 0
	}
	if settings.CodexWSSilentRetries > maxCodexWSSilentRetries {
		settings.CodexWSSilentRetries = maxCodexWSSilentRetries
	}
	if settings.CodexWSBusyMaxWaitSec <= 0 {
		settings.CodexWSBusyMaxWaitSec = defaultCodexWSBusyMaxWaitSec
	}
	if settings.CodexWSBusyMaxWaitSec > maxCodexWSBusyWaitSec {
		settings.CodexWSBusyMaxWaitSec = maxCodexWSBusyWaitSec
	}
	if settings.CodexWSBusyPatienceSec < 0 {
		settings.CodexWSBusyPatienceSec = defaultCodexWSBusyPatienceSec
	}
	if settings.CodexWSBusyPatienceSec > maxCodexWSBusyWaitSec {
		settings.CodexWSBusyPatienceSec = maxCodexWSBusyWaitSec
	}
	if settings.CodexWSStatelessSlots < 0 {
		settings.CodexWSStatelessSlots = defaultCodexWSStatelessSlots
	}
	if settings.CodexWSStatelessSlots > maxCodexWSStatelessSlots {
		settings.CodexWSStatelessSlots = maxCodexWSStatelessSlots
	}
	settings.CodexWSDownstreamKeepaliveSlots = database.NormalizeCodexWSDownstreamKeepaliveSlots(settings.CodexWSDownstreamKeepaliveSlots)
	settings.CodexOverloadThresholdPercent = database.NormalizeCodexOverloadThresholdPercent(settings.CodexOverloadThresholdPercent)
	settings.CodexOverloadPauseMinutes = database.NormalizeCodexOverloadPauseMinutes(settings.CodexOverloadPauseMinutes)
	settings.CodexOverloadWindowMinutes = database.NormalizeCodexOverloadWindowMinutes(settings.CodexOverloadWindowMinutes)
	if settings.CodexContinueMaxRounds < minCodexContinueMaxRounds {
		settings.CodexContinueMaxRounds = defaults.CodexContinueMaxRounds
	}
	if settings.CodexContinueMaxRounds > maxCodexContinueMaxRounds {
		settings.CodexContinueMaxRounds = maxCodexContinueMaxRounds
	}
	settings.AutoResetCreditsBeforeExpiryMin = database.NormalizeAutoResetCreditsBeforeExpiryMinutes(settings.AutoResetCreditsBeforeExpiryMin)
	settings.UTLSShutdownTimeoutMin = database.NormalizeUTLSShutdownTimeoutMinutes(settings.UTLSShutdownTimeoutMin)
	settings.ContinuousRetryPolicy = database.NormalizeContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	return codexRuntimeClientVersionProjections(settings)
}

func ApplyRuntimeSettingsFromSystem(settings *database.SystemSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()

	next := DefaultRuntimeSettings()
	if settings != nil {
		next.ClientCompatMode = settings.ClientCompatMode
		next.CodexMinCLIVersion = settings.CodexMinCLIVersion
		next.CodexUserAgentConfig = settings.CodexUserAgentConfig
		next.CodexTelemetryEnabled = settings.CodexTelemetryEnabled
		next.CodexTelemetryTimingDebug = settings.CodexTelemetryTimingDebug
		next.CodexImagesMainModel = settings.CodexImagesMainModel
		next.StreamFlushPolicy = settings.StreamFlushPolicy
		next.StreamFlushIntervalMS = settings.StreamFlushIntervalMS
		next.FirstTokenMode = settings.FirstTokenMode
		next.FirstTokenTimeoutSec = settings.FirstTokenTimeoutSeconds
		next.BillingTierPolicy = settings.BillingTierPolicy
		next.ModelsListReadMaxBytes = settings.ModelsListReadMaxBytes
		next.CodexForceWebsocket = settings.CodexForceWebsocket
		next.CodexRequestCompression = settings.CodexRequestCompression
		next.CodexWSWeakNetworkMode = settings.CodexWSWeakNetworkMode
		next.CodexWSHideErrors = settings.CodexWSHideUpstreamErrors
		next.CodexWSSilentRetry = settings.CodexWSSilentRetryEnabled
		next.CodexWSSilentRetries = settings.CodexWSSilentMaxRetries
		next.ContinuousRetryPolicy = database.ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy)
		if strings.TrimSpace(settings.ContinuousRetryPolicy) == "" {
			next.ContinuousRetryPolicy = database.DefaultContinuousRetryPolicy()
		}
		next.CodexWSSizeRouter = settings.CodexWSSizeRouterEnabled
		next.CodexWSBusyMaxWaitSec = settings.CodexWSBusyAcquireMaxWaitSec
		next.CodexWSBusyOverflow = settings.CodexWSBusyOverflowEnabled
		next.CodexWSBusyPatienceSec = settings.CodexWSBusyPatienceSec
		next.CodexWSStatelessSlots = settings.CodexWSStatelessSlots
		next.CodexWSDownstreamKeepaliveSlots = settings.CodexWSDownstreamKeepaliveSlots
		next.GithubToken = strings.TrimSpace(settings.GithubToken)
		next.GithubProxyURL = strings.TrimSpace(settings.GithubProxyURL)
		next.CodexOverloadPauseEnabled = settings.CodexOverloadPauseEnabled
		next.CodexOverloadThresholdPercent = settings.CodexOverloadThresholdPercent
		next.CodexOverloadPauseMinutes = settings.CodexOverloadPauseMinutes
		next.CodexOverloadWindowMinutes = settings.CodexOverloadWindowMinutes
		next.OverflowAutoCompact = settings.OverflowAutoCompactEnabled
		next.CompactViaResponses = settings.CompactViaResponsesEnabled
		next.CodexPreflightSSEPassthrough = settings.CodexPreflightSSEPassthroughEnabled
		next.FirstTokenExcludesWsAcquire = settings.FirstTokenExcludesWsAcquire
		next.CodexContinueThinking = settings.CodexContinueThinkingEnabled
		next.CodexContinueMaxRounds = settings.CodexContinueMaxRounds
		next.CodexSyncedCLIVersion = settings.CodexSyncedCLIVersion
		next.CodexSyncedDesktopMacBuild = settings.CodexSyncedDesktopMacBuild
		next.CodexSyncedDesktopWindowsBuild = settings.CodexSyncedDesktopWindowsBuild
		next.CodexSyncedVSCodeBuild = settings.CodexSyncedVSCodeBuild
		next.CodexCLIVersionSyncEnabled = settings.CodexCLIVersionSyncEnabled
		next.CodexCLIVersionSyncIntervalHours = settings.CodexCLIVersionSyncIntervalHours
		next.AutoResetCreditsEnabled = settings.AutoResetCreditsEnabled
		next.AutoResetCreditsOnExhaustionEnabled = settings.AutoResetCreditsOnExhaustionEnabled
		next.AutoResetCreditsBeforeExpiryMin = settings.AutoResetCreditsBeforeExpiryMin
		next.AutoActivate5hWindowEnabled = settings.AutoActivate5hWindowEnabled
		next.UTLSShutdownTimeoutMin = settings.UTLSShutdownTimeoutMinutes
		// Payload rewrite rules are not part of RuntimeSettings (they are stored separately after compilation); the startup seed is also done here.
		if err := SetPayloadRulesJSON(settings.PayloadRules); err != nil {
			log.Printf("failed to parse payload_rules config, ignored: %v", err)
		}
	}
	return storeRuntimeSettings(next)
}

func ApplyRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()
	return storeRuntimeSettings(settings)
}

// UpdateRuntimeSettings updates the runtime config inside the same critical section as the full settings write.
// Background tasks should use this function to modify only the fields they own, so that a stale snapshot does not overwrite settings the admin has just saved.
func UpdateRuntimeSettings(update func(RuntimeSettings) RuntimeSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()

	next := currentRuntimeSettings()
	if update != nil {
		next = update(next)
	}
	return storeRuntimeSettings(next)
}

func CurrentRuntimeSettings() RuntimeSettings {
	return currentRuntimeSettings()
}

func currentRuntimeSettings() RuntimeSettings {
	if v, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return NormalizeRuntimeSettings(v)
	}
	return DefaultRuntimeSettings()
}

func storeRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	settings = NormalizeRuntimeSettings(settings)
	runtimeSettings.Store(settings)
	return settings
}

func currentStreamFlushInterval() time.Duration {
	ms := CurrentRuntimeSettings().StreamFlushIntervalMS
	if ms < minStreamFlushIntervalMS {
		ms = defaultStreamFlushIntervalMS
	}
	return time.Duration(ms) * time.Millisecond
}

func currentFirstTokenTimeout() time.Duration {
	seconds := CurrentRuntimeSettings().FirstTokenTimeoutSec
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// codexContinueThinkingSettings returns the continue-thinking fold switch and the maximum rounds (read from one snapshot).
func codexContinueThinkingSettings() (bool, int) {
	s := CurrentRuntimeSettings()
	return s.CodexContinueThinking, s.CodexContinueMaxRounds
}
