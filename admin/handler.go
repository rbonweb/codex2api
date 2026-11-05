package admin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/internal/openaiidentity"
	"github.com/codex2api/internal/version"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// Handler for the admin panel API
type Handler struct {
	intelligenceDeviceConfig *proxy.DeviceProfileConfig
	intelligenceOnce         sync.Once
	intelligenceWG           sync.WaitGroup
	imageQueue               *imageJobQueue
	qualityTestContext       context.Context
	qualityTestWG            sync.WaitGroup
	store                    *auth.Store
	modelRefreshFuncs        map[string]channelModelRefreshFunc // nil = per-channel default implementation; used for test injection
	proxyRiskJobsMu          sync.RWMutex
	proxyRiskJobs            map[string]*proxyRiskScoringJob
	cache                    cache.TokenCache
	authCacheProxy           *proxy.Handler
	db                       *database.DB
	cacheCfgStore            responseCacheSettingsStore
	rateLimiter              *proxy.RateLimiter
	systemUpdate             *systemUpdater
	systemUpdateOnce         sync.Once
	refreshAccount           func(context.Context, int64) error
	probeUsage               func(context.Context, *auth.Account) error

	codexUsageRefreshRunning atomic.Bool

	activate5hWindow             func(context.Context, *auth.Account) error
	executeUsageProbe            usageProbeRequestFunc
	syncAccountPlanOnReset       func(context.Context, *auth.Account) error
	queryResetUsage              func(context.Context, *auth.Account, string) (*proxy.WhamUsage, *http.Response, error)
	queryResetCredits            func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error)
	consumeResetCredit           func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error)
	queryWhamDailyUsage          func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error)
	queryWhamDailyTokenBreakdown func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error)
	sendCodexInvite              func(context.Context, *auth.Account, string, string, string, []string) (*proxy.CodexInviteResult, error)
	// When the list page-stats finds that the current page lacks official settlement snapshots, it backfills per account immediately;
	// last/in-flight prevents page turns or frontend retries from overloading the upstream with one account, and failedAt gives accounts
	// that keep failing a longer cooldown; syncedOnce records "synced successfully, but upstream returned no data"
	// (official statistics lag), so page-stats returns an explicit empty state instead of endlessly triggering backfills.
	whamDailyBackfillMu       sync.Mutex
	whamDailyBackfillLast     map[int64]time.Time
	whamDailyBackfillInFlight map[int64]struct{}
	whamDailyBackfillFailedAt map[int64]time.Time
	// patWhoAmIFailedAt records the time of the most recent failure of PAT account whoami workspace completion (used for backoff).
	patWhoAmIFailedAt     map[int64]time.Time
	whamDailySyncedOnce   map[int64]struct{}
	whamDailyDeepSynced   map[int64]whamDailyDeepState
	recordAccountEvent    func(int64, string, string)
	proxyProbe            func(context.Context, string, string) proxyProbeResult
	reloadProxyPoolFn     func() error
	proxyBatchEventSender func(*gin.Context, proxyBatchTestEvent) bool
	proxyBatchTestMu      sync.Mutex
	cpuSampler            *cpuSampler
	memReader             memStatsReader
	startedAt             time.Time
	pgMaxConns            int
	redisPoolSize         int
	databaseDriver        string
	databaseLabel         string
	cacheDriver           string
	cacheLabel            string
	adminSecretEnv        string
	adminAuthThrottle     *adminAuthThrottle
	imageProxy            *proxy.Handler

	// Usage-sampling queue triggered by import. A fixed number of workers consume tasks, avoiding "one goroutine per account"
	// piling up thousands of blocked goroutines during large file imports.
	importProbeQueueMu sync.Mutex
	importProbeQueue   []func(context.Context)
	importProbeWorkers int
	importProbeActive  atomic.Int32
	importLoadMu       sync.Mutex
	importLoadTier     importLoadTier
	importLoadReady    bool
	importLoadDBWait   int64
	importLoadChanged  time.Time
	importLoadBusyTill time.Time
	importLoadNow      func() time.Time
	importLoadSnapshot func() importRuntimeLoadSnapshot

	// In-memory cache for chart aggregation (10s TTL)
	chartCacheMu   sync.RWMutex
	chartCacheData map[string]*chartCacheEntry

	// Account request statistics cache, keyed by channel (each channel refreshes independently; the old full-scan path
	// uses the "all" key). The paged path uses stale-while-revalidate, with the TTL in requestCountCacheTTL.
	reqCountMu         sync.RWMutex
	reqCountCache      map[string]*requestCountCacheEntry
	reqCountRefreshMu  sync.Mutex
	reqCountRefreshing map[string]bool
	// Resumable intermediate results of batched aggregation for large pools; see requestCountStaging.
	reqCountStagingMu sync.Mutex
	reqCountStaging   map[string]*requestCountStaging

	// Lightweight snapshot used for account pagination in the admin panel. Snapshots are cached per channel and store only the filters, sorting and
	// the information needed to locate the current page; the full account response is still built only for the current page.
	accountListCacheMu sync.RWMutex
	accountListCache   map[string]*accountListSnapshot
	accountListBuildMu sync.Mutex
	// accountCachesGen is incremented on account changes; the rebuild goroutine checks the generation before installing a snapshot,
	// so an in-flight rebuild that started reading the database before a change cannot write stale data back into the cache.
	accountCachesGen atomic.Uint64

	// Analytics charts use fixed-size aggregated results to avoid passing the full pool to the browser. Cached separately from the account
	// snapshot; it is only built when the analytics section is expanded or the Dashboard runway is shown.
	accountAnalysisCacheMu        sync.RWMutex
	accountAnalysisCache          map[string]*accountAnalysisCacheEntry
	accountAnalysisBuildMu        sync.Mutex
	accountAnalysisTrafficMu      sync.RWMutex
	accountAnalysisTraffic        map[string]*accountAnalysisTrafficCacheEntry
	accountAnalysisTrafficBuildMu sync.Mutex

	// Workspace-level mutex for the "proactive reset count" consume operation (workspace -> *sync.Mutex),
	// serializes concurrent resets on the same upstream workspace, avoiding duplicate consumption and races on the reset count.
	resetCreditLocks               sync.Map
	resetCreditLastSuccess         sync.Map
	resetCreditSuccessfulIDs       sync.Map
	autoResetCreditsWake           chan struct{}
	codexTurnStateRenewalStartOnce sync.Once
	codexTurnStateRenewalWG        sync.WaitGroup
	autoResetCreditsStartOnce      sync.Once
	autoResetCreditsWG             sync.WaitGroup
	autoActivate5hWake             chan struct{}
	autoActivate5hStartOnce        sync.Once
	autoActivate5hWG               sync.WaitGroup
	scheduledTestStartOnce         sync.Once
	scheduledTestWG                sync.WaitGroup
	scheduledTestGateOnce          sync.Once
	scheduledTestGate              chan struct{}
	resetCreditPostMu              sync.Mutex
	resetCreditPostWG              sync.WaitGroup
	resetCreditPostCtx             context.Context
	resetCreditPostCancel          context.CancelFunc
	resetCreditPostClosed          bool
	settingsUpdateMu               sync.Mutex

	// Mutex for merging duplicate accounts: serializes mergeRefreshedDuplicateIntoExisting,
	// prevents concurrent imports of several accounts with the same identity from merging into each other and soft-deleting both (losing accounts).
	mergeDuplicateMu sync.Mutex

	// Agent Identity import mutex: serializes the database duplicate check and insert for runtime_id,
	// prevents concurrent requests from both creating an account after "checking that it does not exist".
	agentIdentityImportMu sync.Mutex
}

type responseCacheSettingsStore interface {
	GetResponseCacheSettings(context.Context) (database.ResponseCacheSettings, error)
	UpdateResponseCacheSettings(
		context.Context,
		database.ResponseCacheSettingsUpdate,
	) (database.ResponseCacheSettings, error)
}

func validateResponseCacheSettingsUpdateRanges(update database.ResponseCacheSettingsUpdate) error {
	switch {
	case update.LocalMaxBytes != nil &&
		(*update.LocalMaxBytes < database.MinResponseCacheLocalMaxBytes ||
			*update.LocalMaxBytes > database.MaxResponseCacheLocalMaxBytes):
		return fmt.Errorf(
			"%w: response_cache_local_max_bytes must be between %d and %d",
			database.ErrInvalidResponseCacheSettings,
			database.MinResponseCacheLocalMaxBytes,
			database.MaxResponseCacheLocalMaxBytes,
		)
	case update.LocalMaxEntryBytes != nil &&
		(*update.LocalMaxEntryBytes < database.MinResponseCacheLocalMaxEntryBytes ||
			*update.LocalMaxEntryBytes > database.MaxResponseCacheLocalMaxEntryBytes):
		return fmt.Errorf(
			"%w: response_cache_local_max_entry_bytes must be between %d and %d",
			database.ErrInvalidResponseCacheSettings,
			database.MinResponseCacheLocalMaxEntryBytes,
			database.MaxResponseCacheLocalMaxEntryBytes,
		)
	case update.ReconstructMaxBytes != nil &&
		(*update.ReconstructMaxBytes < database.MinResponseCacheReconstructMaxBytes ||
			*update.ReconstructMaxBytes > database.MaxResponseCacheReconstructMaxBytes):
		return fmt.Errorf(
			"%w: response_cache_reconstruct_max_bytes must be between %d and %d",
			database.ErrInvalidResponseCacheSettings,
			database.MinResponseCacheReconstructMaxBytes,
			database.MaxResponseCacheReconstructMaxBytes,
		)
	case update.WritePolicy != nil && !database.ValidResponseCacheWritePolicy(*update.WritePolicy):
		return fmt.Errorf(
			"%w: response_cache_write_policy must be %q or %q",
			database.ErrInvalidResponseCacheSettings,
			database.ResponseCacheWritePolicyAlways,
			database.ResponseCacheWritePolicyOnDemand,
		)
	default:
		return nil
	}
}

func (h *Handler) cacheSettingsStore() responseCacheSettingsStore {
	if h == nil {
		return nil
	}
	if h.cacheCfgStore != nil {
		return h.cacheCfgStore
	}
	if h.db == nil {
		return nil
	}
	return h.db
}

type chartCacheEntry struct {
	data      *database.ChartAggregation
	expiresAt time.Time
}

const (
	adminUsageStatsCacheNamespace = "admin:usage-stats"
	adminChartCacheNamespace      = "admin:chart-data"
	// v2: the response structure adds a reconciliation field; bumping the namespace version invalidates old entries that Redis holds
	// written before deployment, so zero-value reconciliation is not shown during the rolling window.
	adminAPIKeyAccountsNamespace   = "admin:api-key-accounts:v2"
	adminAPIKeyStatsNamespace      = "admin:api-key-stats"
	adminAccountWindowsNamespace   = "admin:account-usage-windows"
	adminAPIKeyCacheNamespace      = "api-key"
	adminAPIKeyCountNamespace      = "api-key-count"
	adminUsageStatsCacheTTL        = 5 * time.Second
	adminUsageRangeCacheTTL        = 35 * time.Second
	adminChartCacheTTL             = 10 * time.Second
	adminAccountWindowsCacheTTL    = 30 * time.Second
	importFileSizeLimitBytes       = 200 * 1024 * 1024
	importFileSizeLimitLabel       = "200MB"
	accountRefreshBatchConcurrency = 4
)

func (h *Handler) getRuntimeJSON(ctx context.Context, namespace, key string, dest interface{}) bool {
	if h == nil || h.cache == nil || dest == nil {
		return false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, namespace, key)
	if err != nil {
		log.Printf("failed to read runtime cache: namespace=%s err=%v", namespace, err)
		return false
	}
	if !ok || len(raw) == 0 {
		return false
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		log.Printf("failed to parse runtime cache: namespace=%s err=%v", namespace, err)
		return false
	}
	return true
}

func (h *Handler) setRuntimeJSON(ctx context.Context, namespace, key string, value interface{}, ttl time.Duration) {
	if h == nil || h.cache == nil || value == nil {
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		log.Printf("failed to encode runtime cache: namespace=%s err=%v", namespace, err)
		return
	}
	if err := h.cache.SetRuntime(ctx, namespace, key, payload, ttl); err != nil {
		log.Printf("failed to write runtime cache: namespace=%s err=%v", namespace, err)
	}
}

func validateImportFileSize(fh *multipart.FileHeader) error {
	if fh.Size > importFileSizeLimitBytes {
		return fmt.Errorf("file %s size exceeds %s", fh.Filename, importFileSizeLimitLabel)
	}
	return nil
}

func (h *Handler) usageProbeFunc() func(context.Context, *auth.Account) error {
	if h != nil && h.probeUsage != nil {
		return h.probeUsage
	}
	if h != nil {
		return h.ProbeUsageSnapshot
	}
	return nil
}

func (h *Handler) probeImportedAccountUsage(ctx context.Context, accountID int64, source string) {
	if h == nil || h.store == nil {
		return
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		return
	}
	// Agent Identity has no AccessToken but can run /responses probes with its signature, so this gate must not stop it.
	if account.GetAccessToken() == "" && !account.IsCodexAgentIdentity() {
		return
	}
	defer h.refreshImportedDaybreak(ctx, accountID)
	probeFn := h.usageProbeFunc()
	if probeFn == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := probeFn(probeCtx, account); err != nil {
		log.Printf("import account %d usage sampling failed (%s): %v", accountID, source, err)
		return
	}
	// Agent Identity has no OAuth identity to merge (no RT/AT), so it must not enter
	// the Codex email+workspace duplicate check chain.
	if !shouldMergeImportedIdentity(account) {
		return
	}
	// The OAuth identity (email + valid workspace) of an AT / codex_at account cannot be decoded from the
	// JWT at insert time; the wham probe above fills it in and persists it. Now that the identity is known, check right here whether it matches
	// an existing account: if it is a duplicate, merge the credentials into the old account and soft-delete this one, matching the RT path
	// refreshImportedAccountAndProbe, closing the gap where AT imports/adds cannot be de-duplicated after the fact.
	// Merging is keyed by email + valid workspace identity; a Chatgpt-Account-Id override represents a separate route.
	// Database lifecycle ctx and the serial merge lock (prevents concurrent imports from soft-deleting each other).
	h.mergeRefreshedDuplicateIntoExistingContext(ctx, accountID, source)
}

func shouldMergeImportedIdentity(account *auth.Account) bool {
	return account != nil && !account.IsCodexAgentIdentity()
}

func (h *Handler) startDBBackgroundTask(task func(context.Context)) bool {
	if h == nil || task == nil {
		return false
	}
	if h.db != nil {
		return h.db.RunBackgroundTask(task)
	}
	go task(context.Background())
	return true
}

// startDBBackgroundTaskWithParent ties a task to both a caller-owned service
// context and the database lifecycle. Cancellation of either context stops the
// task, while the database tracker guarantees shutdown waits for its exit.
func (h *Handler) startDBBackgroundTaskWithParent(parent context.Context, task func(context.Context)) bool {
	if task == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	return h.startDBBackgroundTask(func(lifecycle context.Context) {
		ctx, cancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(parent, cancel)
		defer func() {
			stopParent()
			cancel()
		}()
		task(ctx)
	})
}

type importLoadTier uint8

const (
	importLoadLow importLoadTier = iota
	importLoadMedium
	importLoadHigh
)

const (
	maxImportDBConcurrency    = 12
	maxImportProbeConcurrency = 8
	importPermitPollInterval  = 25 * time.Millisecond
	importTierRecoveryDelay   = 5 * time.Second
	importDBWaitBackoff       = 10 * time.Second
)

// maxAdminPage limits the page number to avoid (page-1)*page_size overflowing into a negative OFFSET.
const maxAdminPage = 100000

type importRuntimeLoadSnapshot struct {
	RPM         int64
	Active      int64
	DBInUse     int
	DBMaxOpen   int
	DBWaitCount int64
}

type importConcurrencyLimits struct {
	db    int
	probe int
}

func (h *Handler) currentImportRuntimeLoad() importRuntimeLoadSnapshot {
	if h == nil {
		return importRuntimeLoadSnapshot{}
	}
	if h.importLoadSnapshot != nil {
		return h.importLoadSnapshot()
	}
	var snapshot importRuntimeLoadSnapshot
	if h.rateLimiter != nil {
		snapshot.RPM = h.rateLimiter.GetCurrentRPM()
		snapshot.Active = h.rateLimiter.GetActiveRequests()
	}
	if h.db != nil {
		stats := h.db.Stats()
		snapshot.DBInUse = stats.InUse
		snapshot.DBMaxOpen = stats.MaxOpenConnections
		snapshot.DBWaitCount = stats.WaitCount
	}
	return snapshot
}

func importDBUsagePercent(snapshot importRuntimeLoadSnapshot) int {
	if snapshot.DBMaxOpen <= 0 || snapshot.DBInUse <= 0 {
		return 0
	}
	return snapshot.DBInUse * 100 / snapshot.DBMaxOpen
}

// nextImportLoadTier uses different up/down thresholds to form hysteresis: when load rises, it tightens immediately,
// but falling back requires crossing a lower threshold, avoiding repeatedly adding and removing workers near the critical RPM.
func nextImportLoadTier(current importLoadTier, initialized bool, snapshot importRuntimeLoadSnapshot, dbWaitIncreased bool) importLoadTier {
	dbUsage := importDBUsagePercent(snapshot)
	enterHigh := dbWaitIncreased || snapshot.RPM >= 600 || snapshot.Active >= 64 || dbUsage >= 70
	enterMedium := snapshot.RPM >= 180 || snapshot.Active >= 16 || dbUsage >= 40
	if !initialized {
		if enterHigh {
			return importLoadHigh
		}
		if enterMedium {
			return importLoadMedium
		}
		return importLoadLow
	}

	switch current {
	case importLoadHigh:
		if dbWaitIncreased || snapshot.RPM >= 400 || snapshot.Active >= 32 || dbUsage >= 50 {
			return importLoadHigh
		}
		// Step down at most one tier at a time, so recovery stays smooth.
		return importLoadMedium
	case importLoadMedium:
		if enterHigh {
			return importLoadHigh
		}
		if snapshot.RPM < 120 && snapshot.Active < 8 && dbUsage < 25 {
			return importLoadLow
		}
		return importLoadMedium
	default:
		if enterHigh {
			return importLoadHigh
		}
		if enterMedium {
			return importLoadMedium
		}
		return importLoadLow
	}
}

func importLimitsForTier(tier importLoadTier, snapshot importRuntimeLoadSnapshot) importConcurrencyLimits {
	limits := importConcurrencyLimits{db: maxImportDBConcurrency, probe: maxImportProbeConcurrency}
	switch tier {
	case importLoadHigh:
		limits.db, limits.probe = 4, 4
	case importLoadMedium:
		limits.db, limits.probe = 8, 6
	}
	// Import uses at most about a quarter of the connection pool; a small SQLite pool is not monopolized by imports either.
	if snapshot.DBMaxOpen > 0 {
		poolShare := snapshot.DBMaxOpen / 4
		if poolShare < 1 {
			poolShare = 1
		}
		if limits.db > poolShare {
			limits.db = poolShare
		}
	}
	return limits
}

func (h *Handler) adaptiveImportLimits() importConcurrencyLimits {
	if h == nil {
		return importConcurrencyLimits{db: 4, probe: 4}
	}
	snapshot := h.currentImportRuntimeLoad()
	now := time.Now()
	if h.importLoadNow != nil {
		now = h.importLoadNow()
	}
	h.importLoadMu.Lock()
	defer h.importLoadMu.Unlock()

	waitIncreased := h.importLoadReady && snapshot.DBWaitCount > h.importLoadDBWait
	if waitIncreased {
		h.importLoadBusyTill = now.Add(importDBWaitBackoff)
	}
	if now.Before(h.importLoadBusyTill) {
		waitIncreased = true
	}
	nextTier := nextImportLoadTier(h.importLoadTier, h.importLoadReady, snapshot, waitIncreased)
	if h.importLoadReady && nextTier < h.importLoadTier && now.Sub(h.importLoadChanged) < importTierRecoveryDelay {
		nextTier = h.importLoadTier
	}
	if !h.importLoadReady || nextTier != h.importLoadTier {
		h.importLoadChanged = now
	}
	h.importLoadTier = nextTier
	h.importLoadReady = true
	h.importLoadDBWait = snapshot.DBWaitCount
	return importLimitsForTier(h.importLoadTier, snapshot)
}

func (h *Handler) importProbeWorkerCapacity() int {
	capacity := maxImportProbeConcurrency
	if h != nil && h.store != nil {
		if configured := h.store.GetUsageProbeConcurrency(); configured > 0 && configured < capacity {
			capacity = configured
		}
	}
	return capacity
}

func (h *Handler) importProbeConcurrency() int {
	limit := h.adaptiveImportLimits().probe
	if capacity := h.importProbeWorkerCapacity(); limit > capacity {
		limit = capacity
	}
	return limit
}

func acquireAdaptivePermit(ctx context.Context, active *atomic.Int32, limit func() int) bool {
	ticker := time.NewTicker(importPermitPollInterval)
	defer ticker.Stop()
	for {
		maxActive := limit()
		if maxActive < 1 {
			maxActive = 1
		}
		current := active.Load()
		if current < int32(maxActive) && active.CompareAndSwap(current, current+1) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// runImportProbeTask puts import warm-ups into an in-process queue and starts workers according to the current import concurrency limit.
// Queued tasks hold only one function reference and do not each create a goroutine; workers exit once the queue is drained.
func (h *Handler) runImportProbeTask(fn func(context.Context)) {
	if h == nil || fn == nil {
		return
	}
	h.importProbeQueueMu.Lock()
	h.importProbeQueue = append(h.importProbeQueue, fn)
	if h.importProbeWorkers >= h.importProbeWorkerCapacity() {
		h.importProbeQueueMu.Unlock()
		return
	}
	h.importProbeWorkers++
	h.importProbeQueueMu.Unlock()

	if h.startDBBackgroundTask(h.runImportProbeWorker) {
		return
	}
	h.importProbeQueueMu.Lock()
	h.importProbeWorkers--
	h.importProbeQueue = nil
	h.importProbeQueueMu.Unlock()
}

func (h *Handler) runImportProbeWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			h.importProbeQueueMu.Lock()
			h.importProbeQueue = nil
			h.importProbeWorkers--
			h.importProbeQueueMu.Unlock()
			return
		}

		h.importProbeQueueMu.Lock()
		if len(h.importProbeQueue) == 0 {
			h.importProbeWorkers--
			h.importProbeQueueMu.Unlock()
			return
		}
		fn := h.importProbeQueue[0]
		h.importProbeQueue[0] = nil
		h.importProbeQueue = h.importProbeQueue[1:]
		h.importProbeQueueMu.Unlock()

		if !acquireAdaptivePermit(ctx, &h.importProbeActive, h.importProbeConcurrency) {
			continue
		}
		fn(ctx)
		h.importProbeActive.Add(-1)
	}
}

type adaptiveImportDBLimiter struct {
	handler *Handler
	active  atomic.Int32
}

func (l *adaptiveImportDBLimiter) acquire(ctx context.Context) bool {
	if l == nil || l.handler == nil {
		return false
	}
	return acquireAdaptivePermit(ctx, &l.active, func() int {
		return l.handler.adaptiveImportLimits().db
	})
}

func (l *adaptiveImportDBLimiter) release() {
	if l != nil {
		l.active.Add(-1)
	}
}

func (h *Handler) triggerImportedAccountUsageProbe(accountID int64, source string) {
	h.runImportProbeTask(func(ctx context.Context) {
		h.probeImportedAccountUsage(ctx, accountID, source)
	})
}

// scheduleImportedAccountWarmup handles the token exchange (AT) / usage probe after import. Scripted bulk account adds and file imports
// must go through the same concurrency gate: a bare RT-to-AT exchange also hits upstream, and without limits it would saturate the gateway and auth endpoints together.
func (h *Handler) scheduleImportedAccountWarmup(acc *auth.Account, id int64, source string) {
	if h == nil || acc == nil || id <= 0 {
		return
	}
	if acc.GetAccessToken() != "" {
		h.triggerImportedAccountUsageProbe(id, source)
		return
	}
	if h.store != nil {
		h.runImportProbeTask(func(ctx context.Context) {
			h.refreshImportedAccountAndProbe(ctx, id, source+"_refresh")
		})
	}
}

// commitImportedRuntimeAccounts writes to the in-memory pool in one batch, then queues warm-ups as needed. Calling AddAccount one at a time
// would repeatedly contend for the pool's write lock; warm-up must happen after entering the pool, since refreshAccountByID looks up the runtime account by DBID.
func (h *Handler) commitImportedRuntimeAccounts(accounts []*auth.Account, source string, skipRefresh bool) {
	if h == nil || h.store == nil || len(accounts) == 0 {
		return
	}
	h.store.AddAccounts(accounts)
	if skipRefresh {
		for _, account := range accounts {
			if account != nil && account.GetAccessToken() != "" {
				id := account.ID()
				h.runImportProbeTask(func(ctx context.Context) { h.refreshImportedDaybreak(ctx, id) })
			}
		}
		return
	}
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		h.scheduleImportedAccountWarmup(acc, acc.DBID, source)
	}
}

func (h *Handler) applyImportedAccountUsageState(account *auth.Account, source string) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if h.store.MarkUsage7dRateLimited(account) {
		log.Printf("import account %d marked rate limited after 7d usage was exhausted (%s)", account.DBID, source)
	}
}

// importRefreshTransientRetryLimit is the number of extra retries for transient import token-exchange failures (timeouts / proxy jitter / upstream
// 5xx / refresh-lock contention). A transient failure must not immediately set a sticky error: error has no
// automatic retry, so semantically it is worse than a fully dead RT (which self-heals after cooldown); when a bulk import hits proxy jitter,
// a batch of good accounts would be permanently marked dead.
const importRefreshTransientRetryLimit = 3

// importRefreshTransientRetryDelay is the interval between two import refresh retries. A var, so tests can shorten it.
var importRefreshTransientRetryDelay = 2 * time.Minute

func (h *Handler) refreshImportedAccountAndProbe(ctx context.Context, accountID int64, source string) {
	h.refreshImportedAccountWithRetry(ctx, accountID, source, 0)
}

func (h *Handler) refreshImportedAccountWithRetry(ctx context.Context, accountID int64, source string, attempt int) {
	if ctx == nil {
		ctx = context.Background()
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	// The import path runs one unified probe after identity merging; the refresh itself does not probe first, so that
	// a successful bare RT exchange is not followed by two consecutive hits to the wham/subscription upstream.
	err := h.refreshAccountByIDWithProbe(refreshCtx, accountID, false)
	cancel()
	if err != nil {
		log.Printf("import account %d refresh failed (attempt %d): %v", accountID, attempt+1, err)
		if h.scheduleImportedRefreshRetry(accountID, source, attempt, err) {
			return
		}
		h.markImportedRefreshFailure(accountID, err)
		return
	}
	log.Printf("import account %d refreshed successfully", accountID)
	// For a bare RT import, the identity is only known after the first refresh: check for an identity duplicate right then,
	// and if it matches an existing account, merge the credentials and remove this account (keeping the old account's usage statistics).
	if h.mergeRefreshedDuplicateIntoExistingContext(ctx, accountID, source) {
		return
	}
	h.probeImportedAccountUsage(ctx, accountID, source)
}

// scheduleImportedRefreshRetry schedules one delayed retry for transient refresh failures: when due, it goes back through the
// import concurrency gate without sleeping inside the gate to hold a slot. During the retry window the account stays "refreshing"; this is the real state
// and it is bounded (at most limit+1 attempts). Permanent failures or exhausted retries return false, leaving the caller to set the status.
func (h *Handler) scheduleImportedRefreshRetry(accountID int64, source string, attempt int, err error) bool {
	if h == nil || h.store == nil || err == nil {
		return false
	}
	if auth.IsPermanentRefreshFailure(err) || attempt >= importRefreshTransientRetryLimit {
		return false
	}
	acc := h.store.FindByID(accountID)
	if acc == nil || acc.GetAccessToken() != "" {
		return false
	}
	time.AfterFunc(importRefreshTransientRetryDelay, func() {
		h.runImportProbeTask(func(ctx context.Context) {
			h.refreshImportedAccountWithRetry(ctx, accountID, source, attempt+1)
		})
	})
	return true
}

// markImportedRefreshFailure sets the status when the import token exchange fails (transient failures have already exhausted retries),
// so a bare RT does not stay stuck in "refreshing". A revoked session or invalid RT marks unauthorized (the duration is set by the
// adaptive 6/24h policy of unauthorized; the input is only a fallback); other failures mark error.
func (h *Handler) markImportedRefreshFailure(accountID int64, err error) {
	if h == nil || h.store == nil || err == nil {
		return
	}
	acc := h.store.FindByID(accountID)
	if acc == nil || acc.GetAccessToken() != "" {
		return
	}
	// store's refresh path already applied the unauthorized cooldown to non-retryable errors; marking again would
	// double FailureStreak, make the adaptive cooldown jump straight to the 24h tier, and persist twice.
	switch acc.RuntimeStatus() {
	case "unauthorized", "error":
		return
	}
	msg := err.Error()
	if auth.IsPermanentRefreshFailure(err) {
		h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", msg)
		return
	}
	h.store.MarkError(acc, msg)
}

// mergeRefreshedDuplicateIntoExisting checks whether the newly imported account, just refreshed, is the same
// OAuth identity as an existing account. If so, it merges the new credentials (refresh_token has the highest priority and can renew automatically)
// into the existing account; the codex_* usage snapshot keys are not in the update set, so the old account's usage statistics and the
// request history linked by account ID are all kept; then it soft-deletes the newly inserted account. Returns true if merged.
func (h *Handler) mergeRefreshedDuplicateIntoExisting(newID int64, source string) bool {
	return h.mergeRefreshedDuplicateIntoExistingContext(context.Background(), newID, source)
}

func (h *Handler) mergeRefreshedDuplicateIntoExistingContext(parent context.Context, newID int64, source string) bool {
	if h == nil || h.db == nil || h.store == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	// Serialize merging: when several accounts with the same identity are imported concurrently and two merge flows interleave,
	// they could each pick the other as the "existing account", causing both to be soft-deleted (losing accounts).
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	newRow, err := h.db.GetAccountByID(ctx, newID)
	if err != nil || newRow == nil {
		return false
	}
	email := strings.TrimSpace(newRow.GetCredential("email"))
	effectiveWorkspaceID := openaiidentity.EffectiveWorkspaceID(
		newRow.GetCredential("workspace_id"),
		newRow.GetCredentialStringMap("custom_headers"),
	)
	if email == "" || effectiveWorkspaceID == "" {
		return false
	}
	oldID, err := h.db.FindActiveAccountByOAuthRouteIdentity(ctx, email, effectiveWorkspaceID, newID)
	if err != nil || oldID <= 0 {
		return false
	}
	oldRow, err := h.db.GetAccountByID(ctx, oldID)
	if err != nil || oldRow == nil {
		return false
	}

	updates := make(map[string]interface{})
	for _, key := range []string{"refresh_token", "session_token", "access_token", "access_token_type", "id_token", "expires_at", "email", "account_id", "workspace_id", "user_id", "plan_type", "subscription_expires_at"} {
		if v := strings.TrimSpace(newRow.GetCredential(key)); v != "" {
			updates[key] = v
		}
	}
	oldHeaders := oldRow.GetCredentialStringMap("custom_headers")
	oldOverride := openaiidentity.WorkspaceOverrideFromHeaders(oldHeaders)
	newOverride := openaiidentity.WorkspaceOverrideFromHeaders(newRow.GetCredentialStringMap("custom_headers"))
	newTokenWorkspaceID := openaiidentity.NormalizeWorkspaceID(newRow.GetCredential("workspace_id"))
	if oldOverride == "" && newOverride != "" && newTokenWorkspaceID != effectiveWorkspaceID {
		// The duplicate is the same effective route only because the new row
		// carries an explicit override. Preserve that route on the survivor
		// before copying token-native identity fields from the new credentials.
		updates["custom_headers"] = customHeadersWithWorkspaceOverride(oldHeaders, newOverride)
	}
	if len(updates) == 0 {
		return false
	}
	proxyURL := strings.TrimSpace(newRow.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(oldRow.ProxyURL)
	}
	if err := h.db.UpdateOAuthAccountCredentials(ctx, oldID, updates, proxyURL); err != nil {
		log.Printf("failed to merge credentials of imported account %d into existing account %d: %v", newID, oldID, err)
		return false
	}
	// The new credentials were just verified as usable during refresh/probe, so the old account's earlier error / 401 unauthorized state
	// is out of date: if it is not cleared, the re-authorized RT is merged in, the new account is soft-deleted, and the user sees
	// the old account still showing "unauthorized" until the adaptive cooldown expires (issue #618). This matches the import path where the JWT yields the
	// identity and the flow goes through upsertOAuthIdentityAccount; rate-limit cooldowns are not affected.
	if h.clearReimportedAccountErrorState(ctx, oldRow, "merge credentials") {
		log.Printf("cleared the error/401 state of existing account %d while merging credentials of imported account %d", newID, oldID)
	}
	// Soft-delete the new account first, then reload the old one: reloadTokenAccount asynchronously triggers a
	// probe-then-merge of the old account again; if the new account is still active at that moment, the reverse duplicate check would merge the old account into the new one,
	// and both would be soft-deleted. Soft-deleting first ensures no later duplicate check can see the new account.
	if err := h.db.SoftDeleteAccount(ctx, newID); err != nil {
		log.Printf("failed to soft-delete duplicate imported account %d: %v", newID, err)
	}
	h.store.RemoveAccount(newID)
	if err := h.reloadTokenAccount(ctx, oldID, source); err != nil {
		log.Printf("failed to reload account %d after merge: %v", oldID, err)
	}
	if err := h.db.InsertAccountEvent(ctx, newID, "deleted", fmt.Sprintf("merged_into_%d", oldID)); err != nil {
		log.Printf("failed to record delete event for merged account %d: %v", newID, err)
	}
	if err := h.db.InsertAccountEvent(ctx, oldID, "updated", "rt_upgrade_merge"); err != nil {
		log.Printf("failed to record update event for merged account %d: %v", oldID, err)
	}
	log.Printf("imported account %d has the same OAuth identity as existing account %d; credentials merged (RT upgrade) and usage statistics kept (source=%s)", newID, oldID, source)
	return true
}

func (h *Handler) deleteRuntimeCache(ctx context.Context, namespace, key string) {
	if h == nil || h.cache == nil {
		return
	}
	if err := h.cache.DeleteRuntime(ctx, namespace, key); err != nil {
		log.Printf("failed to delete runtime cache: namespace=%s err=%v", namespace, err)
	}
}

func (h *Handler) invalidateAPIKeyRuntimeCaches(ctx context.Context, apiKey string) {
	if h.authCacheProxy != nil {
		h.authCacheProxy.InvalidateAPIKeyAuthCache(ctx)
	}
	h.deleteRuntimeCache(ctx, adminAPIKeyCountNamespace, "all")
	if strings.TrimSpace(apiKey) != "" {
		h.deleteRuntimeCache(ctx, adminAPIKeyCacheNamespace, apiKey)
	}
}

func (h *Handler) getUsageStatsCached(ctx context.Context, rangeStart, rangeEnd time.Time, channel string) (*database.UsageStats, error) {
	return h.getUsageStatsFilteredCached(ctx, rangeStart, rangeEnd, channel, database.UsageLogFilter{})
}

// getUsageStatsFilteredCached returns full statistics with dimension filters (account/key/model/endpoint/search, etc.).
// The dimension fingerprint is folded into the cache key: the same filter combination is reused within a 30-second bucket, and different combinations never mix.
func (h *Handler) getUsageStatsFilteredCached(ctx context.Context, rangeStart, rangeEnd time.Time, channel string, dim database.UsageLogFilter) (*database.UsageStats, error) {
	cacheKey := ""
	cacheTTL := adminUsageStatsCacheTTL
	dimKey := usageStatsDimensionCacheKey(dim)
	if rangeStart.IsZero() && rangeEnd.IsZero() && channel == "" && dimKey == "" {
		cacheKey = "global"
	} else if !rangeStart.IsZero() && !rangeEnd.IsZero() {
		// When the dashboard refreshes every 15 seconds, start/end shift along with it. Full results are reused within 30-second buckets,
		// which keeps the cumulative, range, model and breakdown semantics intact and avoids rescanning million-row logs within the same minute.
		cacheKey = fmt.Sprintf("range:%d:%d:%s%s", rangeStart.Unix()/30, rangeEnd.Unix()/30, channel, dimKey)
		cacheTTL = adminUsageRangeCacheTTL
	}
	if cacheKey != "" {
		var cached database.UsageStats
		if h.getRuntimeJSON(ctx, adminUsageStatsCacheNamespace, cacheKey, &cached) {
			return &cached, nil
		}
	}
	stats, err := h.db.GetUsageStatsFiltered(ctx, rangeStart, rangeEnd, channel, dim, true)
	if err != nil {
		return nil, err
	}
	if cacheKey != "" {
		h.setRuntimeJSON(ctx, adminUsageStatsCacheNamespace, cacheKey, stats, cacheTTL)
	}
	return stats, nil
}

// usageStatsDimensionCacheKey compresses dimension filters into a fixed-length fingerprint (with a leading colon), and returns an empty string when there are no filters.
// Search terms can be long and contain arbitrary characters; concatenating them directly into the cache key is neither safe nor efficient, so they are hashed.
func usageStatsDimensionCacheKey(dim database.UsageLogFilter) string {
	raw := dim.DimensionKey()
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return ":dim:" + hex.EncodeToString(sum[:8])
}

func (h *Handler) getUsageStatsSummaryCached(ctx context.Context, rangeStart, rangeEnd time.Time, channel string) (*database.UsageStats, error) {
	return h.getUsageStatsSummaryFilteredCached(ctx, rangeStart, rangeEnd, channel, database.UsageLogFilter{})
}

func (h *Handler) getUsageStatsSummaryFilteredCached(ctx context.Context, rangeStart, rangeEnd time.Time, channel string, dim database.UsageLogFilter) (*database.UsageStats, error) {
	cacheKey := "summary:global"
	cacheTTL := adminUsageStatsCacheTTL
	dimKey := usageStatsDimensionCacheKey(dim)
	if !rangeStart.IsZero() && !rangeEnd.IsZero() {
		cacheKey = fmt.Sprintf("summary:range:%d:%d:%s%s", rangeStart.Unix()/30, rangeEnd.Unix()/30, channel, dimKey)
		cacheTTL = adminUsageRangeCacheTTL
	} else {
		if channel != "" {
			cacheKey += ":" + channel
		}
		cacheKey += dimKey
	}
	var cached database.UsageStats
	if h.getRuntimeJSON(ctx, adminUsageStatsCacheNamespace, cacheKey, &cached) {
		return &cached, nil
	}
	stats, err := h.db.GetUsageStatsFiltered(ctx, rangeStart, rangeEnd, channel, dim, false)
	if err != nil {
		return nil, err
	}
	h.setRuntimeJSON(ctx, adminUsageStatsCacheNamespace, cacheKey, stats, cacheTTL)
	return stats, nil
}

// parseUsageChannel parses the account/usage channel filter parameters in the query.
func parseUsageChannel(c *gin.Context) string {
	switch strings.ToLower(strings.TrimSpace(c.Query("channel"))) {
	case database.UpstreamChannelCodex:
		return database.UpstreamChannelCodex
	}
	return ""
}

// SetAPIKeyAuthCacheHandler connects management changes to proxy cache invalidation.
func (h *Handler) SetAPIKeyAuthCacheHandler(handler *proxy.Handler) { h.authCacheProxy = handler }

// NewHandler creates the admin panel handler
func NewHandler(store *auth.Store, db *database.DB, tc cache.TokenCache, rl *proxy.RateLimiter, adminSecretEnv string) *Handler {
	handler := &Handler{
		store:                store,
		proxyRiskJobs:        make(map[string]*proxyRiskScoringJob),
		cache:                tc,
		db:                   db,
		cacheCfgStore:        db,
		rateLimiter:          rl,
		cpuSampler:           newCPUSampler(),
		startedAt:            time.Now(),
		databaseDriver:       db.Driver(),
		databaseLabel:        db.Label(),
		cacheDriver:          tc.Driver(),
		cacheLabel:           tc.Label(),
		adminSecretEnv:       adminSecretEnv,
		adminAuthThrottle:    newAdminAuthThrottle(),
		imageProxy:           proxy.NewHandler(store, db, nil, nil),
		chartCacheData:       make(map[string]*chartCacheEntry),
		accountListCache:     make(map[string]*accountListSnapshot),
		accountAnalysisCache: make(map[string]*accountAnalysisCacheEntry),
	}
	if handler.imageProxy != nil {
		handler.imageProxy.SetRuntimeCache(tc)
	}
	store.SetUsageProbeCompletionFunc(handler.invalidateAccountSnapshotCaches)
	handler.refreshAccount = handler.refreshSingleAccount
	handler.probeUsage = handler.ProbeUsageSnapshot
	handler.syncAccountPlanOnReset = handler.syncSingleAccountPlanOnReset
	handler.queryResetCredits = proxy.QueryWhamResetCredits
	handler.consumeResetCredit = proxy.ConsumeResetCreditParsed
	handler.queryWhamDailyUsage = proxy.QueryWhamDailyUsage
	handler.queryWhamDailyTokenBreakdown = proxy.QueryWhamDailyTokenBreakdown
	handler.sendCodexInvite = proxy.SendCodexInvite
	handler.whamDailyBackfillLast = make(map[int64]time.Time)
	handler.whamDailyBackfillInFlight = make(map[int64]struct{})
	handler.whamDailyBackfillFailedAt = make(map[int64]time.Time)
	handler.whamDailySyncedOnce = make(map[int64]struct{})
	handler.autoResetCreditsWake = make(chan struct{}, 1)
	handler.autoActivate5hWake = make(chan struct{}, 1)
	if db != nil {
		handler.recordAccountEvent = db.InsertAccountEventAsync
		if workers, _ := ImageJobWorkerCount(); workers == 0 {
			if err := db.MarkInterruptedImageJobs(context.Background()); err != nil {
				log.Printf("failed to mark interrupted image generation job: %v", err)
			}
		}
	}
	return handler
}

// SetPoolSizes sets the connection pool size tracking values (called by main.go at startup)
func (h *Handler) SetPoolSizes(pgMaxConns, redisPoolSize int) {
	h.pgMaxConns = pgMaxConns
	h.redisPoolSize = redisPoolSize
}

// RegisterRoutes registers the admin API routes
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/p/img/:id", h.GetSignedImageAssetFile)
	r.GET("/p/backgrounds/:filename", h.GetBackgroundAssetFile)
	r.HEAD("/p/backgrounds/:filename", h.GetBackgroundAssetFile)
	r.GET("/api/branding", h.GetBranding)
	keyUsage := r.Group("/api/key-usage")
	keyUsage.GET("/summary", h.GetPublicAPIKeyUsageSummary)
	keyUsage.GET("/me", h.GetPublicAPIKeyUsageSummary)
	keyUsage.GET("/concurrency", h.GetPublicAPIKeyConcurrency)

	// Public self-service account add portal (no admin auth; gated by a switch and IP rate limiting; see self_service.go)
	accountPortal := r.Group("/api/account-portal")
	accountPortal.Use(h.accountPortalMiddleware())
	accountPortal.POST("/generate-auth-url", h.GenerateAccountPortalAuthURL)
	accountPortal.POST("/submit-code", h.SubmitAccountPortalCode)

	r.GET("/api/image-studio/quota", h.GetPortalImageQuota)
	imageStudioPortal := r.Group("/api/image-studio")
	imageStudioPortal.Use(h.imageStudioPortalAuthMiddleware())
	imageStudioPortal.POST("/jobs", h.CreatePortalImageJob)
	imageStudioPortal.POST("/edit-jobs", h.CreatePortalImageEditJob)
	imageStudioPortal.GET("/jobs", h.ListPortalImageJobs)
	imageStudioPortal.GET("/jobs/:id", h.GetPortalImageJob)
	imageStudioPortal.DELETE("/jobs/:id", h.DeletePortalImageJob)
	imageStudioPortal.GET("/assets", h.ListPortalImageAssets)
	imageStudioPortal.GET("/assets/:id/file", h.GetPortalImageAssetFile)
	imageStudioPortal.DELETE("/assets/:id", h.DeletePortalImageAsset)

	// First-time setup endpoints (no auth required; only available when ADMIN_SECRET is not configured on the system)
	// These two endpoints must be registered outside adminAuthMiddleware, otherwise fail-closed would block them.
	r.GET("/api/admin/bootstrap-status", h.GetBootstrapStatus)
	r.POST("/api/admin/bootstrap", h.PostBootstrap)
	// Static, credential-free shell. Generated HTML is delivered by the parent via
	// postMessage and remains in an opaque-origin sandbox, never stored by the server.
	r.GET("/api/quality-test/preview", serveQualityTestPreview)

	api := r.Group("/api/admin")
	api.Use(h.adminAuthMiddleware())
	api.Use(func(c *gin.Context) {
		c.Next()
		if shouldInvalidateAccountSnapshotCaches(c.Request.Method, c.Request.URL.Path, c.Writer.Status()) {
			h.invalidateAccountSnapshotCaches()
		}
	})
	api.GET("/stats", h.GetStats)
	api.GET("/accounts", h.ListAccounts)
	api.GET("/accounts/analysis", h.GetAccountAnalysis)
	api.GET("/accounts/page-stats", h.GetAccountPageStats)
	api.GET("/accounts/live", h.GetAccountLiveState)
	api.GET("/accounts/:id", h.GetAccount)
	api.POST("/accounts", h.AddAccount)
	api.POST("/accounts/at", h.AddATAccount)
	api.POST("/accounts/codex/agent-identity", h.ImportCodexAgentIdentity)
	api.POST("/accounts/codex/agent-identity/import", h.BatchImportCodexAgentIdentity)
	api.POST("/accounts/:id/oauth/exchange-code", h.UpdateOAuthAccountCode)
	api.POST("/accounts/import", h.ImportAccounts)
	api.POST("/accounts/sub2api/preview", h.PreviewSub2APIAccounts)
	api.POST("/accounts/sub2api/import", h.ImportFromSub2API)
	api.PATCH("/accounts/:id/models", h.UpdateAccountModels)
	api.POST("/accounts/:id/models/sync-upstream", h.SyncAccountUpstreamModels)
	api.POST("/accounts/:id/models/probe", h.ProbeAccountModels)
	api.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
	api.DELETE("/accounts/:id", h.DeleteAccount)
	api.GET("/accounts/health-bars", h.GetAccountHealthBars)
	api.GET("/accounts/recycle-bin", h.ListRecycleBinAccounts)
	api.GET("/accounts/recycle-bin/export", h.ExportRecycleBinAccounts)
	api.DELETE("/accounts/recycle-bin", h.EmptyRecycleBin)
	api.POST("/accounts/recycle-bin/batch-test", h.RecycleBinBatchTest)
	api.POST("/accounts/:id/restore", h.RestoreAccount)
	api.DELETE("/accounts/:id/purge", h.PurgeAccount)
	api.POST("/accounts/:id/refresh", h.RefreshAccount)
	api.POST("/accounts/:id/enable", h.ToggleAccountEnabled)
	api.PATCH("/accounts/:id/note", h.UpdateAccountNote)
	api.POST("/accounts/:id/lock", h.ToggleAccountLock)
	api.POST("/accounts/:id/reset-status", h.ResetAccountStatus)
	api.PATCH("/accounts/:id/model-cooldown-policy", h.UpdateAccountModelCooldownPolicy)
	api.DELETE("/accounts/:id/model-cooldowns", h.ClearAllAccountModelCooldowns)
	api.DELETE("/accounts/:id/model-cooldowns/:model", h.ClearAccountModelCooldown)
	api.POST("/accounts/:id/reset-credits", h.ResetCredits)
	api.GET("/accounts/:id/reset-credits", h.GetResetCredits)
	api.GET("/accounts/:id/wham-daily-usage", h.GetAccountWhamDailyUsage)
	api.POST("/accounts/:id/invite", h.SendInvite)
	api.GET("/accounts/:id/invite/eligibility", h.GetInviteEligibility)
	api.GET("/accounts/:id/invite/tracking", h.GetInviteTracking)
	api.POST("/accounts/invite/recipients/check", h.CheckInviteRecipients)
	api.GET("/accounts/invite/plan", h.GetInviteGuidePlan)
	api.POST("/accounts/invite/plan/probe", h.ProbeInviteGuidePlan)
	api.GET("/accounts/:id/test", h.TestConnection)
	api.GET("/accounts/:id/scheduled-tests", h.GetScheduledConnectionTests)
	api.PUT("/accounts/:id/scheduled-tests", h.UpdateScheduledConnectionTests)
	api.GET("/accounts/:id/model-detector", h.DetectCodexModel)
	api.GET("/modeltrace/bank", h.GetModelTraceBank)
	api.POST("/modeltrace/bank/update", h.UpdateModelTraceBank)
	api.DELETE("/modeltrace/bank", h.ResetModelTraceBank)
	api.GET("/accounts/:id/quality-test/options", h.QualityTestOptions)
	api.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	h.registerIntelligenceCheckRoutes(api)
	api.GET("/quality-tests", h.ListQualityTests)
	api.GET("/quality-tests/:id", h.GetQualityTest)
	api.POST("/quality-tests/:id/cancel", h.CancelQualityTest)
	api.GET("/quality-test-prompts", h.ListQualityTestPrompts)
	api.POST("/quality-test-prompts", h.CreateQualityTestPrompt)
	api.PATCH("/quality-test-prompts/:id", h.UpdateQualityTestPrompt)
	api.DELETE("/quality-test-prompts/:id", h.DeleteQualityTestPrompt)
	api.GET("/accounts/:id/usage", h.GetAccountUsage)
	api.POST("/accounts/:id/usage/refresh", h.RefreshAccountUsage)
	api.GET("/accounts/:id/subscription", h.GetAccountSubscription)
	api.POST("/accounts/:id/subscription/refresh", h.RefreshAccountSubscription)
	api.GET("/accounts/:id/auth-json", h.GetAccountAuthJSON)
	api.PATCH("/accounts/:id/credit", h.UpdateAccountCredit)
	api.POST("/accounts/batch-test", h.BatchTest)
	api.POST("/accounts/batch-refresh", h.BatchRefreshAccounts)
	api.POST("/accounts/batch-refresh-usage", h.BatchRefreshCodexUsage)
	api.POST("/accounts/batch-delete", h.BatchDeleteAccounts)
	api.POST("/accounts/batch-update", h.BatchUpdateAccounts)
	api.POST("/accounts/batch-reset-status", h.BatchResetStatus)
	api.POST("/accounts/clean-banned", h.CleanBanned)
	api.POST("/accounts/clean-rate-limited", h.CleanRateLimited)
	api.POST("/accounts/clean-error", h.CleanError)
	api.GET("/accounts/export", h.ExportAccounts)
	api.POST("/accounts/migrate", h.MigrateAccounts)
	api.GET("/accounts/event-trend", h.GetAccountEventTrend)
	api.POST("/accounts/usage/probe", h.ForceUsageProbe)
	api.GET("/usage/stats", h.GetUsageStats)
	api.GET("/usage/api-keys", h.GetAPIKeyTokenStats)
	api.GET("/usage/api-keys/:id/accounts", h.GetAPIKeyAccountStats)
	api.GET("/usage/logs", h.GetUsageLogs)
	api.GET("/usage/logs/error-summary", h.GetUsageLogsErrorSummary)
	api.GET("/usage/chart-data", h.GetChartData)
	api.DELETE("/usage/logs", h.ClearUsageLogs)
	api.GET("/setup-hints", h.GetSetupHints)
	api.GET("/keys", h.ListAPIKeys)
	api.GET("/keys-concurrency", h.GetAPIKeyConcurrency)
	api.POST("/keys", h.CreateAPIKey)
	api.POST("/keys/reset-all-quotas", h.ResetAllAPIKeyQuotas)
	api.PATCH("/keys/:id", h.UpdateAPIKey)
	api.POST("/keys/:id/reset-quota", h.ResetAPIKeyQuota)
	api.GET("/keys/:id/scope-usage", h.GetAPIKeyScopeUsage)
	api.GET("/keys/:id/model-request-usage", h.GetAPIKeyModelRequestUsage)
	api.GET("/keys-scope-summary", h.GetAPIKeysScopeSummary)
	api.POST("/keys/:id/scope-quota/reset", h.ResetAPIKeyScopeQuota)
	api.DELETE("/keys/:id", h.DeleteAPIKey)
	api.GET("/account-groups", h.ListAccountGroups)
	api.POST("/account-groups", h.CreateAccountGroup)
	api.PATCH("/account-groups/:id", h.UpdateAccountGroup)
	api.DELETE("/account-groups/:id", h.DeleteAccountGroup)
	api.GET("/health", h.GetHealth)
	api.GET("/runtime-status", h.GetRuntimeStatus)
	api.GET("/system/update", h.GetSystemUpdate)
	api.POST("/system/update", h.PerformSystemUpdate)
	api.GET("/ops/overview", h.GetOpsOverview)
	api.GET("/ops/runtime-status", h.GetRuntimeStatus)
	api.GET("/ops/errors", h.GetOpsErrorLogs)
	api.GET("/ops/errors/export", h.ExportOpsErrorLogs)
	api.GET("/ops/errors/summary", h.GetOpsErrorSummary)
	api.GET("/settings", h.GetSettings)
	api.PUT("/settings", h.UpdateSettings)
	api.GET("/settings/codex-user-agent/catalog", h.GetCodexUserAgentCatalog)
	api.POST("/settings/codex-user-agent/preview", h.PreviewCodexUserAgent)
	api.GET("/settings/observed-instructions", h.GetObservedInstructions)
	api.GET("/settings/invite-guide", h.GetInviteGuideSettings)
	api.PUT("/settings/invite-guide", h.UpdateInviteGuideSettings)
	api.GET("/settings/visible-channels", h.GetVisibleChannelsSettings)
	api.PUT("/settings/visible-channels", h.UpdateVisibleChannelsSettings)
	api.POST("/settings/background-upload", h.UploadBackgroundAsset)
	api.POST("/settings/image-storage/test", h.TestImageStorageConnection)
	api.GET("/prompt-filter/logs", h.ListPromptFilterLogs)
	api.GET("/prompt-filter/logs/match", h.MatchPromptFilterLog)
	api.DELETE("/prompt-filter/logs", h.ClearPromptFilterLogs)
	api.GET("/prompt-filter/retention", h.GetPromptLogRetention)
	api.PUT("/prompt-filter/retention", h.UpdatePromptLogRetention)
	api.POST("/prompt-filter/retention/run", h.RunPromptLogRetentionNow)
	api.GET("/prompt-policy/incidents", h.ListPromptPolicyIncidents)
	api.DELETE("/prompt-policy/incidents", h.ClearPromptPolicyIncidents)
	api.DELETE("/prompt-policy/incidents/:incident_id", h.DeletePromptPolicyIncident)
	api.GET("/prompt-policy/incidents/health", h.GetPromptPolicyAuditHealth)
	api.GET("/prompt-policy/incidents/:incident_id", h.GetPromptPolicyIncident)
	api.GET("/prompt-policy/risk-profiles", h.ListPromptRiskProfiles)
	api.GET("/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)
	api.PUT("/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.UpsertPromptRiskTrustPolicy)
	api.DELETE("/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.RevokePromptRiskTrustPolicy)
	api.POST("/prompt-policy/conversation-locks/:lock_key/unlock", h.UnlockPromptConversation)
	api.POST("/prompt-filter/test", h.TestPromptFilter)
	api.GET("/prompt-filter/review/keys", h.ListPromptReviewAPIKeys)
	api.DELETE("/prompt-filter/review/keys/:key_id", h.DeletePromptReviewAPIKey)
	api.GET("/prompt-filter/review/profiles", h.ListPromptReviewProfiles)
	api.POST("/prompt-filter/review/profiles", h.SavePromptReviewProfile)
	api.POST("/prompt-filter/review/profiles/:profile_id/activate", h.ActivatePromptReviewProfile)
	api.DELETE("/prompt-filter/review/profiles/:profile_id", h.DeletePromptReviewProfile)
	api.POST("/prompt-filter/review/test", h.TestPromptReviewConnection)
	api.POST("/prompt-filter/review/models", h.ListPromptReviewModels)
	api.POST("/prompt-filter/rules/test", h.TestPromptFilterRulePattern)
	api.GET("/prompt-filter/rules", h.GetPromptFilterRules)
	api.GET("/prompt-filter/newapi-bindings", h.ListPromptFilterNewAPIBindings)
	api.POST("/prompt-filter/newapi-bindings", h.CreatePromptFilterNewAPIBinding)
	api.GET("/prompt-filter/newapi-bindings/:api_key_id", h.GetPromptFilterNewAPIBinding)
	api.PATCH("/prompt-filter/newapi-bindings/:api_key_id", h.UpdatePromptFilterNewAPIBinding)
	api.POST("/prompt-filter/newapi-bindings/:api_key_id/secret/generate", h.GeneratePromptFilterNewAPIBindingSecret)
	api.PUT("/prompt-filter/newapi-bindings/:api_key_id/secret", h.ReplacePromptFilterNewAPIBindingSecret)
	api.DELETE("/prompt-filter/newapi-bindings/:api_key_id", h.DeletePromptFilterNewAPIBinding)
	api.POST("/prompt-filter/intelligence/run", h.RunPromptIntelligence)
	api.GET("/prompt-filter/intelligence/history", h.ListPromptIntelligenceHistory)
	api.GET("/prompt-filter/intelligence/candidates", h.ListPromptIntelligenceCandidates)
	api.GET("/prompt-filter/intelligence/ai-providers", h.GetPromptIntelligenceAIProviders)
	api.GET("/prompt-filter/intelligence/candidates/:id/evidence", h.GetPromptIntelligenceCandidateEvidence)
	api.POST("/prompt-filter/intelligence/candidates/:id/analyze", h.AnalyzePromptIntelligenceCandidate)
	api.POST("/prompt-filter/intelligence/candidates/:id/identity-updates/:evidence_id/apply", h.ApplyPromptIntelligenceIdentityUpdate)
	api.POST("/prompt-filter/intelligence/candidates/:id/identity-updates/:evidence_id/rollback", h.RollbackPromptIntelligenceIdentityUpdate)
	api.POST("/prompt-filter/intelligence/candidates/:id/draft", h.CreatePromptIntelligenceCandidateDraft)
	api.POST("/prompt-filter/intelligence/candidates/:id/draft/suggest", h.SuggestPromptIntelligenceCandidateDraft)
	api.POST("/prompt-filter/intelligence/candidates/:id/publish", h.PublishPromptIntelligenceCandidate)
	api.POST("/prompt-filter/intelligence/candidates/:id/dismiss", h.DismissPromptIntelligenceCandidate)
	api.GET("/models", h.ListModels)
	api.POST("/models/sync", h.SyncModels)
	api.POST("/models/refresh-all", h.RefreshAllModels)
	api.POST("/codex-cli-version/sync", h.SyncCodexCLIVersion)
	api.POST("/codex-client-versions/sync", h.SyncCodexClientVersions)
	api.GET("/model-pricing", h.ListModelPricing)
	api.PUT("/model-pricing", h.UpdateModelPricing)
	api.POST("/model-pricing/sync", h.SyncModelPricing)
	api.PUT("/model-pricing/official-sync/config", h.UpdateOfficialPricingSyncConfig)
	api.POST("/model-pricing/official-sync", h.SyncOfficialPricingNow)
	api.GET("/image-prompts", h.ListImagePromptTemplates)
	api.POST("/image-prompts", h.CreateImagePromptTemplate)
	api.PATCH("/image-prompts/:id", h.UpdateImagePromptTemplate)
	api.DELETE("/image-prompts/:id", h.DeleteImagePromptTemplate)
	api.POST("/images/jobs", h.CreateImageGenerationJob)
	api.POST("/images/edit-jobs", h.CreateImageEditJob)
	api.GET("/images/jobs", h.ListImageGenerationJobs)
	api.GET("/images/jobs/:id", h.GetImageGenerationJob)
	api.DELETE("/images/jobs/:id", h.DeleteImageGenerationJob)
	api.GET("/images/assets", h.ListImageAssets)
	api.GET("/images/assets/:id/file", h.GetImageAssetFile)
	api.DELETE("/images/assets/:id", h.DeleteImageAsset)
	api.GET("/proxies", h.ListProxies)
	api.POST("/proxies", h.AddProxies)
	api.DELETE("/proxies/:id", h.DeleteProxy)
	api.PATCH("/proxies/:id", h.UpdateProxy)
	api.POST("/proxies/batch-delete", h.BatchDeleteProxies)
	api.POST("/proxies/clean-error", h.CleanErrorProxies)
	api.POST("/proxies/test", h.TestProxy)
	api.POST("/proxies/test-all", h.TestAllProxies)
	api.POST("/proxies/auto-balance", h.AutoBalanceProxies)
	api.GET("/proxy-risk-scoring/profiles", h.ListProxyRiskScoringProfiles)
	api.POST("/proxy-risk-scoring/profiles", h.CreateProxyRiskScoringProfile)
	api.PATCH("/proxy-risk-scoring/profiles/:profile_id", h.UpdateProxyRiskScoringProfile)
	api.DELETE("/proxy-risk-scoring/profiles/:profile_id", h.DeleteProxyRiskScoringProfile)
	api.POST("/proxy-risk-scoring/profiles/:profile_id/test", h.TestProxyRiskScoringProfile)
	api.POST("/proxies/risk-score", h.StartProxyRiskScoringJob)
	api.GET("/proxies/risk-score/jobs/:job_id", h.GetProxyRiskScoringJob)
	api.POST("/proxies/risk-score/jobs/:job_id/cancel", h.CancelProxyRiskScoringJob)
	api.GET("/proxies/:id/risk-score", h.GetProxyRiskScore)
	api.GET("/proxies/:id/risk-score/history", h.ListProxyRiskScoreHistory)

	// OAuth authorization flow
	api.POST("/oauth/generate-auth-url", h.GenerateOAuthURL)
	api.POST("/oauth/exchange-code", h.ExchangeOAuthCode)
	api.GET("/oauth/poll-callback", h.PollOAuthCallback)

	// OAuth callback endpoint (no admin auth required; called by OpenAI redirects)
	r.GET("/auth/callback", h.OAuthCallback)
}

// adminAuthMiddleware is the admin interface auth middleware (enhanced version, adds security audit logging)
//
// Security policy (fail-closed):
//   - Requests are refused (503) whenever ADMIN_SECRET is not configured, to keep /api/admin/* from being exposed unprotected.
//   - Users should set the initial key on the frontend "first-time setup" page (the unauthenticated /api/admin/bootstrap endpoint)
//     or set ADMIN_SECRET explicitly in .env and restart.
func (h *Handler) adminAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		adminSecret, source := h.resolveAdminSecret(c.Request.Context())
		if adminSecret == "" {
			// fail-closed: refuse and tell the user to configure ADMIN_SECRET
			security.SecurityAuditLog("ADMIN_BLOCKED_NO_SECRET", fmt.Sprintf("path=%s ip=%s", c.Request.URL.Path, c.ClientIP()))
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Admin interface not initialized: ADMIN_SECRET is not configured. Open /admin/ in a browser to complete first-time setup, or set ADMIN_SECRET in .env and restart.",
				"code":  "bootstrap_required",
			})
			c.Abort()
			return
		}

		adminKey := c.GetHeader("X-Admin-Key")
		if adminKey == "" {
			// Compatible with the Authorization: Bearer form
			authHeader := c.GetHeader("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				adminKey = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		clientIP := c.ClientIP()
		// Check the block before comparing any candidate. Accepting a correct
		// candidate during a block would leave an unlimited password oracle.
		if retry := h.adminAuthThrottle.retryAfter(clientIP, time.Now()); retry > 0 {
			c.Header("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many failed authentication attempts, please try again later",
			})
			return
		}

		// Sanitize input
		adminKey = security.SanitizeInput(adminKey)

		// Use constant-time comparison to prevent timing attacks
		if !security.SecureCompare(adminKey, adminSecret) {
			// Record security audit log
			security.SecurityAuditLog("ADMIN_AUTH_FAILED", fmt.Sprintf("path=%s ip=%s source=%s", c.Request.URL.Path, security.SanitizeLog(clientIP), source))
			if blocked := h.adminAuthThrottle.recordFailure(clientIP, time.Now()); blocked > 0 {
				c.Header("Retry-After", strconv.Itoa(int((blocked+time.Second-1)/time.Second)))
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error": "Too many failed authentication attempts, please try again later",
				})
				c.Abort()
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Admin key is invalid or missing",
			})
			c.Abort()
			return
		}
		h.adminAuthThrottle.reset(clientIP)

		// Admin responses contain secrets such as raw_key, auth.json and settings, so intermediate caches must not store them.
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")

		// Authentication succeeded; record audit log
		if security.IsSensitiveEndpoint(c.Request.URL.Path) {
			security.SecurityAuditLog("ADMIN_ACCESS", fmt.Sprintf("path=%s ip=%s method=%s", c.Request.URL.Path, c.ClientIP(), c.Request.Method))
		}

		c.Next()
	}
}

func (h *Handler) resolveAdminSecret(ctx context.Context) (string, string) {
	if h.adminSecretEnv != "" {
		return h.adminSecretEnv, "env"
	}

	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(readCtx)
	if err != nil || settings == nil || settings.AdminSecret == "" {
		return "", "disabled"
	}
	return settings.AdminSecret, "database"
}

func (h *Handler) hasConfiguredAdminSecret(ctx context.Context) bool {
	adminSecret, _ := h.resolveAdminSecret(ctx)
	return strings.TrimSpace(adminSecret) != ""
}

// ==================== Stats ====================

// GetStats returns dashboard statistics
func (h *Handler) GetStats(c *gin.Context) {
	// Large installations may need more than five seconds to aggregate the
	// dashboard inputs. Keep the request bounded without failing normal loads.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	accounts, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	accountCounts, channelCounts := summarizeDashboardAccounts(accounts, h.store.Accounts())

	todayByChannel, _ := h.db.CountTodayRequestsByChannel(ctx)
	todayReqs := int64(0)
	for _, count := range todayByChannel {
		todayReqs += count
	}

	channels := make(map[string]statsChannelCounts, len(channelCounts))
	for ch, counts := range channelCounts {
		channels[ch] = statsChannelCounts{
			Total:         counts.total,
			Available:     counts.normal,
			RateLimited:   counts.rateLimited,
			Error:         counts.abnormal,
			TodayRequests: todayByChannel[ch],
		}
	}

	c.JSON(http.StatusOK, statsResponse{
		Total:         accountCounts.total,
		Available:     accountCounts.normal,
		RateLimited:   accountCounts.rateLimited,
		Error:         accountCounts.abnormal,
		TodayRequests: todayReqs,
		Channels:      channels,
	})
}

type dashboardAccountCounts struct {
	total       int
	normal      int
	rateLimited int
	abnormal    int
	disabled    int
}

// summarizeDashboardAccounts summarizes account health counts and splits them by independent account channel.
// Channel determination prefers runtime accounts; rows not in the pool fall back to the upstream_type credentials.
func summarizeDashboardAccounts(rows []*database.AccountRow, runtimeAccounts []*auth.Account) (dashboardAccountCounts, map[string]dashboardAccountCounts) {
	runtimeByID := make(map[int64]*auth.Account, len(runtimeAccounts))
	for _, acc := range runtimeAccounts {
		if acc != nil {
			runtimeByID[acc.DBID] = acc
		}
	}

	var counts dashboardAccountCounts
	channelCounts := map[string]dashboardAccountCounts{
		database.UpstreamChannelCodex: {},
	}
	counts.total = len(rows)
	for _, row := range rows {
		if row == nil {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(row.Status))
		cooldownReason := strings.ToLower(strings.TrimSpace(row.CooldownReason))
		channel := database.UpstreamChannelCodex
		usingCredits := false
		acc := runtimeByID[row.ID]
		if acc != nil {
			status = strings.ToLower(strings.TrimSpace(acc.RuntimeStatus()))
			cooldownReason = ""
			// Credit covering a rate limit: status still reports rate limited (the window is objectively full), but the account keeps participating in scheduling, so it counts as available.
			usingCredits = acc.UsingCredits()
		}
		perChannel := channelCounts[channel]
		perChannel.total++

		if !row.Enabled {
			counts.disabled++
			perChannel.disabled++
		}
		switch {
		case isDashboardAbnormalAccount(status):
			counts.abnormal++
			perChannel.abnormal++
		case !usingCredits && isDashboardRateLimitedAccount(status, cooldownReason):
			counts.rateLimited++
			perChannel.rateLimited++
		case isDashboardUnsampledAccount(row, acc):
			// Unsampled accounts are not counted as available: the dashboard "available" stays aligned with the "normal/scheduling" states on the account page.
		default:
			counts.normal++
			perChannel.normal++
		}
		channelCounts[channel] = perChannel
	}
	return counts, channelCounts
}

func isDashboardAbnormalAccount(status string) bool {
	return status == "unauthorized" || status == "error"
}

func isDashboardUnsampledAccount(row *database.AccountRow, acc *auth.Account) bool {
	if acc != nil {
		snapshot := acc.GetAccountListRuntimeSnapshot()
		status := strings.ToLower(strings.TrimSpace(snapshot.Status))
		if status == "unauthorized" || status == "error" {
			return false
		}
		return !snapshot.UsagePercent5hValid && !snapshot.UsagePercent7dValid
	}
	if row == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(row.Status))
	if status == "unauthorized" || status == "error" {
		return false
	}
	return true
}

func isDashboardRateLimitedAccount(status string, cooldownReason string) bool {
	switch status {
	case "rate_limited", auth.ResponsesRateLimitedCooldownReason, "usage_exhausted", "usage_limited", "quota_paused", "rate_limited_5h", "rate_limited_7d":
		return true
	}
	switch cooldownReason {
	case "rate_limited", auth.ResponsesRateLimitedCooldownReason, "rate_limited_5h", "rate_limited_7d", "usage_limited":
		return true
	}
	return false
}

// ==================== Accounts ====================

type accountResponse struct {
	CodexLastRefreshAt      string `json:"codex_last_refresh_at,omitempty"`
	CodexRefreshError       string `json:"codex_refresh_error,omitempty"`
	UpstreamRequestIDHeader string `json:"upstream_request_id_header"`
	DetailLoaded            bool   `json:"detail_loaded,omitempty"`
	ID                      int64  `json:"id"`
	Name                    string `json:"name"`
	Email                   string `json:"email"`
	EmailDomain             string `json:"email_domain,omitempty"`
	ChatGPTAccountID        string `json:"chatgpt_account_id,omitempty"`
	TokenWorkspaceID        string `json:"token_workspace_id,omitempty"`
	WorkspaceIDOverride     string `json:"workspace_id_override,omitempty"`
	EffectiveWorkspaceID    string `json:"effective_workspace_id,omitempty"`
	PlanType                string `json:"plan_type"`
	SubscriptionExpiresAt   string `json:"subscription_expires_at,omitempty"`
	// Subscription is the subscription status object computed server-side (business status + sync status); empty for accounts that do not track a
	// plan (api / free without an expiry time).
	Subscription          *auth.SubscriptionStatusView `json:"subscription,omitempty"`
	Status                string                       `json:"status"`
	ErrorMessage          string                       `json:"error_message,omitempty"`
	ATOnly                bool                         `json:"at_only"`
	CreditEnabled         bool                         `json:"credit_enabled"`
	CreditSkipUsageWindow bool                         `json:"credit_skip_usage_window"`
	// UsingCredits is a separate signal alongside Status: the usage window is full but credits are covering it,
	// status is still active (schedulable), and the frontend shows a "using credits" badge next to the status badge.
	UsingCredits                  bool                       `json:"using_credits,omitempty"`
	SkipWarmTier                  bool                       `json:"skip_warm_tier"`
	AccountType                   string                     `json:"account_type,omitempty"`
	AccessTokenType               string                     `json:"access_token_type,omitempty"`
	AgentIdentity                 bool                       `json:"agent_identity,omitempty"`
	AvatarURL                     string                     `json:"avatar_url,omitempty"`
	VerifiedEmail                 bool                       `json:"verified_email,omitempty"`
	ProjectID                     string                     `json:"project_id,omitempty"`
	BaseURL                       string                     `json:"base_url,omitempty"`
	Models                        []string                   `json:"models,omitempty"`
	ModelMapping                  string                     `json:"model_mapping,omitempty"`
	CodexPassthroughMode          string                     `json:"codex_passthrough_mode,omitempty"`
	ResponsesUpstreamTransport    string                     `json:"responses_upstream_transport,omitempty"`
	CodexFingerprintMode          string                     `json:"codex_fingerprint_mode,omitempty"`
	Timezone                      string                     `json:"timezone,omitempty"`
	AccountHref                   string                     `json:"account_href,omitempty"`
	CustomHeaders                 map[string]string          `json:"custom_headers,omitempty"`
	HealthTier                    string                     `json:"health_tier"`
	SchedulerScore                float64                    `json:"scheduler_score"`
	DispatchScore                 float64                    `json:"dispatch_score"`
	ScoreBiasOverride             *int64                     `json:"score_bias_override"`
	ScoreBiasEffective            int64                      `json:"score_bias_effective"`
	BaseConcurrencyOverride       *int64                     `json:"base_concurrency_override"`
	BaseConcurrencyEffective      int64                      `json:"base_concurrency_effective"`
	ConcurrencyCap                int64                      `json:"dynamic_concurrency_limit"`
	ProxyURL                      string                     `json:"proxy_url"`
	CreatedAt                     string                     `json:"created_at"`
	UpdatedAt                     string                     `json:"updated_at"`
	CodexUsageUpdatedAt           string                     `json:"codex_usage_updated_at,omitempty"`
	Codex5HUsageUpdatedAt         string                     `json:"codex_5h_usage_updated_at,omitempty"`
	ActiveRequests                int64                      `json:"active_requests"`
	OccupiedRequests              int64                      `json:"occupied_requests"`
	SessionSlotBufferEnabled      bool                       `json:"session_slot_buffer_enabled"`
	TotalRequests                 int64                      `json:"total_requests"`
	LastUsedAt                    string                     `json:"last_used_at"`
	SuccessRequests               int64                      `json:"success_requests"`
	ErrorRequests                 int64                      `json:"error_requests"`
	RetryErrorRequests            int64                      `json:"retry_error_requests"`
	RateLimitAttempts             int64                      `json:"rate_limit_attempts"`
	ErrorStatusCounts             map[string]int64           `json:"error_status_counts,omitempty"`
	SuccessModelCounts            map[string]int64           `json:"success_model_counts,omitempty"`
	UsagePercent7d                *float64                   `json:"usage_percent_7d"`
	UsagePercent5h                *float64                   `json:"usage_percent_5h"`
	UsagePercentSpark             *float64                   `json:"usage_percent_spark"`
	RateLimitResetCredits         *int                       `json:"rate_limit_reset_credits"`
	DaybreakSupported             bool                       `json:"daybreak_supported"`
	DaybreakModels                map[string][]string        `json:"daybreak_models,omitempty"`
	DaybreakCheckedAt             int64                      `json:"daybreak_checked_at,omitempty"`
	ApplicableResetCredits        *int                       `json:"applicable_reset_credits"`
	CreditsValid                  bool                       `json:"credits_valid"`
	CreditsBalance                *string                    `json:"credits_balance"`
	CreditsHasCredits             *bool                      `json:"credits_has_credits"`
	CreditsUnlimited              *bool                      `json:"credits_unlimited"`
	CreditsOverageLimitReached    *bool                      `json:"credits_overage_limit_reached"`
	CreditsSpendControlReached    *bool                      `json:"credits_spend_control_reached,omitempty"`
	CreditsRateLimitReachedType   string                     `json:"credits_rate_limit_reached_type,omitempty"`
	AutoPause5hThreshold          *float64                   `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold          *float64                   `json:"auto_pause_7d_threshold"`
	AutoPause5hDisabled           bool                       `json:"auto_pause_5h_disabled"`
	AutoPause7dDisabled           bool                       `json:"auto_pause_7d_disabled"`
	KeepConcurrencyOnDegrade      bool                       `json:"keep_concurrency_on_degrade"`
	UsageLimitOverride            *bool                      `json:"ignore_usage_limit_status_override"`
	UsageLimitEffective           bool                       `json:"ignore_usage_limit_status_effective"`
	DispatchCountLimit            *int64                     `json:"dispatch_count_limit"`
	DispatchCountUsed             int64                      `json:"dispatch_count_used,omitempty"`
	DispatchCountResetAt          string                     `json:"dispatch_count_reset_at,omitempty"`
	DispatchCountLimited          bool                       `json:"dispatch_count_limited,omitempty"`
	SchedulerPriority             *int64                     `json:"scheduler_priority"`
	Usage5hDetail                 *accountUsageWindow        `json:"usage_5h_detail,omitempty"`
	Usage7dDetail                 *accountUsageWindow        `json:"usage_7d_detail,omitempty"`
	Reset5hAt                     string                     `json:"reset_5h_at,omitempty"`
	Reset7dAt                     string                     `json:"reset_7d_at,omitempty"`
	ResetSparkAt                  string                     `json:"reset_spark_at,omitempty"`
	Window7dKind                  string                     `json:"usage_window_7d_kind,omitempty"`    // "monthly" (team monthly window) / "weekly" / ""; tells the frontend to label it "30 days" rather than "7 days"
	Window7dSeconds               *int64                     `json:"usage_window_7d_seconds,omitempty"` // real period of the long window, in seconds
	Billed5h                      *float64                   `json:"billed_5h"`
	Billed7d                      *float64                   `json:"billed_7d"`
	ScoreBreakdown                schedulerBreakdownResponse `json:"scheduler_breakdown"`
	LastUnauthorizedAt            string                     `json:"last_unauthorized_at,omitempty"`
	LastRateLimitedAt             string                     `json:"last_rate_limited_at,omitempty"`
	LastTimeoutAt                 string                     `json:"last_timeout_at,omitempty"`
	LastServerErrorAt             string                     `json:"last_server_error_at,omitempty"`
	CooldownReason                string                     `json:"cooldown_reason,omitempty"`
	CooldownUntil                 string                     `json:"cooldown_until,omitempty"`
	ModelCooldowns                []modelCooldownResponse    `json:"model_cooldowns,omitempty"`
	ModelCooldownModeOverride     *string                    `json:"model_cooldown_mode_override"`
	ModelCooldownSecondsOverride  *int                       `json:"model_cooldown_seconds_override"`
	ModelCooldownBackoffOverride  *bool                      `json:"model_cooldown_backoff_override"`
	ModelCooldownModeEffective    string                     `json:"model_cooldown_mode_effective"`
	ModelCooldownSecondsEffective int                        `json:"model_cooldown_seconds_effective"`
	ModelCooldownBackoffEffective bool                       `json:"model_cooldown_backoff_effective"`
	Enabled                       bool                       `json:"enabled"`
	Locked                        bool                       `json:"locked"`
	AllowedAPIKeyIDs              []int64                    `json:"allowed_api_key_ids"`
	Tags                          []string                   `json:"tags"`
	GroupIDs                      []int64                    `json:"group_ids"`
	Note                          string                     `json:"note"`
	// Image quota information
	ImageQuotaRemaining *int   `json:"image_quota_remaining,omitempty"`
	ImageQuotaTotal     *int   `json:"image_quota_total,omitempty"`
	TodayUsedCount      *int   `json:"today_used_count,omitempty"`
	ImageQuotaResetAt   string `json:"image_quota_reset_at,omitempty"`
}

type modelCooldownResponse struct {
	Model     string `json:"model"`
	Reason    string `json:"reason"`
	ResetAt   string `json:"reset_at"`
	Remaining int64  `json:"remaining_seconds"`
}

type accountUsageWindow struct {
	Requests             int64              `json:"requests"`
	Tokens               int64              `json:"tokens"`
	AccountBilled        float64            `json:"account_billed"`
	UserBilled           float64            `json:"user_billed"`
	ModelCounts          map[string]int64   `json:"model_counts,omitempty"`
	ModelSuccessCounts   map[string]int64   `json:"model_success_counts,omitempty"`
	ModelAvgFirstTokenMs map[string]float64 `json:"model_avg_first_token_ms,omitempty"`
}

func accountEmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || strings.ContainsAny(email, " \t\r\n") {
		return ""
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.Trim(strings.TrimSpace(email[at+1:]), ".")
	if domain == "" || strings.ContainsAny(domain, " /\\:") || !strings.Contains(domain, ".") {
		return ""
	}
	return domain
}

func accountAccessTokenType(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	if tokenType := strings.TrimSpace(row.GetCredential("access_token_type")); tokenType != "" {
		return tokenType
	}
	return accessTokenTypeForToken(row.GetCredential("access_token"))
}

type schedulerBreakdownResponse struct {
	UnauthorizedPenalty float64 `json:"unauthorized_penalty"`
	RateLimitPenalty    float64 `json:"rate_limit_penalty"`
	TimeoutPenalty      float64 `json:"timeout_penalty"`
	ServerPenalty       float64 `json:"server_penalty"`
	FailurePenalty      float64 `json:"failure_penalty"`
	SuccessBonus        float64 `json:"success_bonus"`
	UsagePenalty7d      float64 `json:"usage_penalty_7d"`
	UsageUrgencyBonus5h float64 `json:"usage_urgency_bonus_5h"`
	UsageUrgencyBonus7d float64 `json:"usage_urgency_bonus_7d"`
	ExpiryUrgencyBonus  float64 `json:"expiry_urgency_bonus"`
	LatencyPenalty      float64 `json:"latency_penalty"`
	SuccessRatePenalty  float64 `json:"success_rate_penalty"`
}

// ListAccounts returns the account list
func (h *Handler) ListAccounts(c *gin.Context) {
	view := strings.ToLower(strings.TrimSpace(c.Query("view")))
	timeout := 5 * time.Second
	if view == "page" {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	// ?view=lite: lightweight view that returns only identity/binding fields and skips usage enrichment and probe triggers.
	// Used by scenarios such as the proxy binding dialog that only need "which account it is and which proxy it is bound to"; large pools no longer transfer
	// the full scheduling metrics (the proxy page freeze issue).
	if view == "lite" {
		h.listAccountsLite(c, ctx)
		return
	}

	// The paged list is a read path and must never fan out probes across a
	// 40k-account pool. Existing background schedulers and explicit refresh
	// actions own probing; the legacy full response preserves its old behavior.
	if view != "page" {
		h.store.TriggerUsageProbeAsync()
		h.store.TriggerRecoveryProbeAsync()
	}

	// Optional ?channel=codex — server-side filter for the account set.
	channel := parseUsageChannel(c)
	var pageSelection *accountPageSelection
	var rows []*database.AccountRow
	var err error
	if view == "page" {
		pageSelection, err = h.getAccountPageSelection(ctx, c, channel)
		if err == nil {
			rows = pageSelection.Rows
		}
	} else {
		rows, err = h.db.ListActiveByChannel(ctx, channel)
	}
	if err != nil {
		var queryErr *accountPageQueryError
		if view == "page" && errors.As(err, &queryErr) {
			writeError(c, http.StatusBadRequest, err.Error())
		} else {
			writeInternalError(c, err)
		}
		return
	}

	// Merge in-memory scheduling metrics
	accountMap := make(map[int64]*auth.Account)
	if view == "page" {
		for _, row := range rows {
			if acc := h.store.FindByID(row.ID); acc != nil {
				accountMap[row.ID] = acc
			}
		}
	} else {
		for _, acc := range h.store.Accounts() {
			accountMap[acc.DBID] = acc
		}
	}

	var reqCounts map[int64]*database.AccountRequestCount
	var usage5h, usage7d map[int64]*database.AccountTimeRangeUsage
	if view == "page" {
		pageIDs := make([]int64, 0, len(rows))
		for _, row := range rows {
			pageIDs = append(pageIDs, row.ID)
		}
		reqCounts, err = h.db.GetAccountRequestCountsByIDs(ctx, pageIDs)
		if err != nil {
			log.Printf("failed to get request stats for the current page's accounts: %v", err)
			reqCounts = make(map[int64]*database.AccountRequestCount)
		}
		usage5h = make(map[int64]*database.AccountTimeRangeUsage)
		usage7d = make(map[int64]*database.AccountTimeRangeUsage)
	} else {
		// The legacy full-list endpoint keeps its original behavior and caching semantics.
		reqCounts = h.getCachedRequestCounts()
		usage5h, usage7d = h.getAccountUsageWindows(ctx)
	}

	accounts := make([]accountResponse, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, h.buildAccountResponse(
			row,
			accountMap[row.ID],
			reqCounts[row.ID],
			usage5h[row.ID],
			usage7d[row.ID],
			view != "page",
		))
	}

	if view != "page" {
		billing5hWindows := make(map[int64]time.Time)
		billing7dWindows := make(map[int64]time.Time)
		for i := range accounts {
			acc, ok := accountMap[accounts[i].ID]
			if !ok {
				continue
			}
			if t := acc.GetReset5hAt(); !t.IsZero() {
				billing5hWindows[accounts[i].ID] = t.Add(-5 * time.Hour)
			}
			if t := acc.GetReset7dAt(); !t.IsZero() {
				// Long-window start = reset - real period. Free/team are monthly windows (about 30 days),
				// so hard-coding a 7-day subtraction would put the start in the future and the cost would always be 0 (issue #324).
				windowDur := 7 * 24 * time.Hour
				if sec := acc.GetWindow7dSeconds(); sec > 0 {
					windowDur = time.Duration(sec) * time.Second
				}
				billing7dWindows[accounts[i].ID] = t.Add(-windowDur)
			}
		}

		billed5h, billingErr := h.db.GetAccountsBilledSince(ctx, billing5hWindows)
		if billingErr != nil {
			log.Printf("batch fetch of account 5h cost failed: %v", billingErr)
			billed5h = nil
		}
		billed7d, billingErr := h.db.GetAccountsBilledSince(ctx, billing7dWindows)
		if billingErr != nil {
			log.Printf("batch fetch of account 7d cost failed: %v", billingErr)
			billed7d = nil
		}
		for i := range accounts {
			if billed, ok := billed5h[accounts[i].ID]; ok {
				accounts[i].Billed5h = &billed
			}
			if billed, ok := billed7d[accounts[i].ID]; ok {
				accounts[i].Billed7d = &billed
			}
		}
	}

	if pageSelection != nil {
		c.JSON(http.StatusOK, accountsPageResponse{
			Accounts: accounts,
			Page:     pageSelection.Page, PageSize: pageSelection.PageSize, Total: pageSelection.Total,
			Summary: pageSelection.Summary, Facets: pageSelection.Facets,
			SnapshotAt: pageSelection.SnapshotAt.Format(time.RFC3339), StatsState: pageSelection.StatsState,
			DisabledSorts: pageSelection.DisabledSorts,
		})
		return
	}
	c.JSON(http.StatusOK, accountsResponse{Accounts: accounts})
}

// GetAccount returns the fully enriched representation for one account. Large
// account pages call this endpoint only when a row is opened or after a
// mutation, keeping expensive detail fields off the critical list path.
// GET /api/admin/accounts/:id
func (h *Handler) GetAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeInternalError(c, err)
		return
	}

	requestCounts, err := h.db.GetAccountRequestCountsByIDs(ctx, []int64{id})
	if err != nil {
		log.Printf("failed to get request stats for account %d: %v", id, err)
		requestCounts = make(map[int64]*database.AccountRequestCount)
	}
	now := time.Now()
	usage5h, usage7d, err := h.db.GetAccountUsageWindowsByIDs(ctx, []int64{id}, now.Add(-5*time.Hour), now.AddDate(0, 0, -7))
	if err != nil {
		log.Printf("failed to get usage stats for account %d: %v", id, err)
		usage5h = make(map[int64]*database.AccountTimeRangeUsage)
		usage7d = make(map[int64]*database.AccountTimeRangeUsage)
	}

	runtimeAccount := h.store.FindByID(id)
	resp := h.buildAccountResponse(row, runtimeAccount, requestCounts[id], usage5h[id], usage7d[id], true)
	if runtimeAccount != nil {
		if resetAt := runtimeAccount.GetReset5hAt(); !resetAt.IsZero() {
			if billed, billedErr := h.db.GetAccountBilledSince(ctx, id, resetAt.Add(-5*time.Hour)); billedErr == nil {
				resp.Billed5h = &billed
			} else {
				log.Printf("failed to get 5h cost for account %d: %v", id, billedErr)
			}
		}
		if resetAt := runtimeAccount.GetReset7dAt(); !resetAt.IsZero() {
			windowDuration := 7 * 24 * time.Hour
			if seconds := runtimeAccount.GetWindow7dSeconds(); seconds > 0 {
				windowDuration = time.Duration(seconds) * time.Second
			}
			if billed, billedErr := h.db.GetAccountBilledSince(ctx, id, resetAt.Add(-windowDuration)); billedErr == nil {
				resp.Billed7d = &billed
			} else {
				log.Printf("failed to get long-window cost for account %d: %v", id, billedErr)
			}
		}
	}
	c.JSON(http.StatusOK, resp)
}

// accountLiteResponse is the ?view=lite account entry: identity and binding fields, with no scheduling or usage metrics.
// Field names match the full accountResponse, so the frontend can consume it directly as a subset of AccountRow.
type accountLiteResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	PlanType      string `json:"plan_type"`
	Status        string `json:"status"`
	Enabled       bool   `json:"enabled"`
	ProxyURL      string `json:"proxy_url"`
	ATOnly        bool   `json:"at_only"`
	AgentIdentity bool   `json:"agent_identity"`
}

func (h *Handler) listAccountsLite(c *gin.Context, ctx context.Context) {
	channel := parseUsageChannel(c)
	rows, err := h.db.ListActiveByChannel(ctx, channel)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// Runtime state overrides the DB state (consistent with the full view); all other enrichment is skipped.
	runtimeStatus := make(map[int64]string)
	for _, acc := range h.store.Accounts() {
		runtimeStatus[acc.DBID] = acc.RuntimeStatus()
	}

	accounts := make([]accountLiteResponse, 0, len(rows))
	for _, row := range rows {
		email := row.GetCredential("email")
		planType := row.GetCredential("plan_type")
		status := row.Status
		if rt, ok := runtimeStatus[row.ID]; ok && rt != "" {
			status = rt
		}
		accounts = append(accounts, accountLiteResponse{
			ID:            row.ID,
			Name:          row.Name,
			Email:         email,
			PlanType:      planType,
			Status:        status,
			Enabled:       row.Enabled,
			ProxyURL:      row.ProxyURL,
			ATOnly:        row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
			AgentIdentity: isAgentIdentityCredentialRow(row),
		})
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

type updateAccountSchedulerReq struct {
	UpstreamRequestIDHeader json.RawMessage `json:"upstream_request_id_header"`
	ScoreBiasOverride       json.RawMessage `json:"score_bias_override"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	SkipWarmTier            json.RawMessage `json:"skip_warm_tier"`
	KeepConcurrency         json.RawMessage `json:"keep_concurrency_on_degrade"`
	AllowedAPIKeyIDs        json.RawMessage `json:"allowed_api_key_ids"`
	Tags                    json.RawMessage `json:"tags"`
	GroupIDs                json.RawMessage `json:"group_ids"`
	AutoPause5hThreshold    json.RawMessage `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    json.RawMessage `json:"auto_pause_7d_threshold"`
	AutoPause5hDisabled     json.RawMessage `json:"auto_pause_5h_disabled"`
	AutoPause7dDisabled     json.RawMessage `json:"auto_pause_7d_disabled"`
	UsageLimitOverride      json.RawMessage `json:"ignore_usage_limit_status_override"`
	DispatchCountLimit      json.RawMessage `json:"dispatch_count_limit"`
	SchedulerPriority       json.RawMessage `json:"scheduler_priority"`
	ProxyURL                json.RawMessage `json:"proxy_url"`
	CustomHeaders           json.RawMessage `json:"custom_headers"`
	CodexFingerprintMode    json.RawMessage `json:"codex_fingerprint_mode"`
	Timezone                json.RawMessage `json:"timezone"`
	AccountHref             json.RawMessage `json:"account_href"`
}

type accountSchedulerUpdate struct {
	ScoreBiasOverride       database.OptionalNullInt64
	BaseConcurrencyOverride database.OptionalNullInt64
	SkipWarmTier            database.OptionalBool
	KeepConcurrency         database.OptionalBool
	AllowedAPIKeyIDs        database.OptionalInt64Slice
	Tags                    optionalStringSlice
	GroupIDs                database.OptionalInt64Slice
	AutoPause5hThreshold    optionalFloat64
	AutoPause7dThreshold    optionalFloat64
	AutoPause5hDisabled     database.OptionalBool
	AutoPause7dDisabled     database.OptionalBool
	UsageLimitOverride      optionalNullableBool
	DispatchCountLimit      database.OptionalNullInt64
	SchedulerPriority       database.OptionalNullInt64
	ProxyURL                database.OptionalString
	CustomHeaders           optionalCustomHeaders
	CodexFingerprintMode    database.OptionalString
	Timezone                database.OptionalString
	AccountHref             database.OptionalString
	CredentialUpdates       map[string]interface{}
}

func parseAccountSchedulerUpdate(req updateAccountSchedulerReq) (accountSchedulerUpdate, error) {
	scoreBiasOverride, err := parseOptionalIntegerField(req.ScoreBiasOverride, "score_bias_override", -200, 200)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	// Base concurrency override: minimum 1, no upper limit (consistent with the global max_concurrency)
	baseConcurrencyOverride, err := parseOptionalIntegerField(req.BaseConcurrencyOverride, "base_concurrency_override", 1, math.MaxInt64)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	skipWarmTier, err := parseOptionalBoolField(req.SkipWarmTier, "skip_warm_tier")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	keepConcurrency, err := parseOptionalBoolField(req.KeepConcurrency, auth.KeepConcurrencyOnDegradeCredentialKey)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	allowedAPIKeyIDs, err := parseOptionalIntegerSliceField(req.AllowedAPIKeyIDs, "allowed_api_key_ids")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	tags, err := parseOptionalStringSliceField(req.Tags, "tags")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	groupIDs, err := parseOptionalIntegerSliceField(req.GroupIDs, "group_ids")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause5hThreshold, err := parseOptionalRatioField(req.AutoPause5hThreshold, "auto_pause_5h_threshold")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause7dThreshold, err := parseOptionalRatioField(req.AutoPause7dThreshold, "auto_pause_7d_threshold")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause5hDisabled, err := parseOptionalBoolField(req.AutoPause5hDisabled, "auto_pause_5h_disabled")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	autoPause7dDisabled, err := parseOptionalBoolField(req.AutoPause7dDisabled, "auto_pause_7d_disabled")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	ignoreUsageLimitStatusOverride, err := parseOptionalNullableBoolField(req.UsageLimitOverride, "ignore_usage_limit_status_override")
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	dispatchCountLimit, err := parseOptionalIntegerField(req.DispatchCountLimit, "dispatch_count_limit", 0, 1000000)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	schedulerPriority, err := parseOptionalIntegerField(req.SchedulerPriority, "scheduler_priority", -100, 100)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}

	proxyURL, err := parseOptionalStringField(req.ProxyURL, "proxy_url", security.ValidateProxyURL)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	customHeaders, err := parseOptionalCustomHeadersField(req.CustomHeaders)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	// null is treated as resetting to the default tier, off.
	codexFingerprintMode, err := parseOptionalStringField(req.CodexFingerprintMode, "codex_fingerprint_mode", validateCodexFingerprintMode)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	timezoneField, err := parseOptionalStringField(req.Timezone, "timezone", validateAccountTimezone)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	accountHref, err := parseOptionalStringField(req.AccountHref, "account_href", validateAccountHref)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	if codexFingerprintMode.Set {
		codexFingerprintMode.Value = auth.NormalizeCodexFingerprintMode(codexFingerprintMode.Value)
	}
	requestIDHeader, err := parseOptionalStringField(req.UpstreamRequestIDHeader, "upstream_request_id_header", auth.ValidateUpstreamRequestIDHeader)
	if err != nil {
		return accountSchedulerUpdate{}, err
	}
	credentialUpdates := make(map[string]interface{})
	if requestIDHeader.Set {
		credentialUpdates[auth.UpstreamRequestIDHeaderCredentialKey] = strings.TrimSpace(requestIDHeader.Value)
	}
	if customHeaders.Set {
		credentialUpdates["custom_headers"] = cloneCustomHeaders(customHeaders.Values)
	}
	if codexFingerprintMode.Set {
		credentialUpdates[auth.CodexFingerprintModeCredentialKey] = codexFingerprintMode.Value
	}
	if timezoneField.Set {
		credentialUpdates[auth.AccountTimezoneCredentialKey] = strings.TrimSpace(timezoneField.Value)
	}
	if accountHref.Set {
		normalized, err := auth.NormalizeAccountHref(accountHref.Value)
		if err != nil {
			return accountSchedulerUpdate{}, err
		}
		credentialUpdates[auth.AccountHrefCredentialKey] = normalized
	}
	if autoPause5hThreshold.Set {
		credentialUpdates["auto_pause_5h_threshold"] = autoPause5hThreshold.Value
	}
	if autoPause7dThreshold.Set {
		credentialUpdates["auto_pause_7d_threshold"] = autoPause7dThreshold.Value
	}
	if autoPause5hDisabled.Set {
		credentialUpdates["auto_pause_5h_disabled"] = autoPause5hDisabled.Value
	}
	if autoPause7dDisabled.Set {
		credentialUpdates["auto_pause_7d_disabled"] = autoPause7dDisabled.Value
	}
	if keepConcurrency.Set {
		credentialUpdates[auth.KeepConcurrencyOnDegradeCredentialKey] = keepConcurrency.Value
	}
	if ignoreUsageLimitStatusOverride.Set {
		if ignoreUsageLimitStatusOverride.Value == nil {
			credentialUpdates["ignore_usage_limit_status_override"] = nil
		} else {
			credentialUpdates["ignore_usage_limit_status_override"] = *ignoreUsageLimitStatusOverride.Value
		}
	}
	if dispatchCountLimit.Set {
		if dispatchCountLimit.Value.Valid {
			credentialUpdates["dispatch_count_limit"] = dispatchCountLimit.Value.Int64
		} else {
			credentialUpdates["dispatch_count_limit"] = int64(0)
		}
	}
	if schedulerPriority.Set {
		if schedulerPriority.Value.Valid {
			credentialUpdates["scheduler_priority"] = schedulerPriority.Value.Int64
		} else {
			credentialUpdates["scheduler_priority"] = int64(0)
		}
	}
	if len(credentialUpdates) == 0 {
		credentialUpdates = nil
	}

	return accountSchedulerUpdate{
		ScoreBiasOverride:       scoreBiasOverride,
		BaseConcurrencyOverride: baseConcurrencyOverride,
		SkipWarmTier:            skipWarmTier,
		KeepConcurrency:         keepConcurrency,
		AllowedAPIKeyIDs:        allowedAPIKeyIDs,
		Tags:                    tags,
		GroupIDs:                groupIDs,
		AutoPause5hThreshold:    autoPause5hThreshold,
		AutoPause7dThreshold:    autoPause7dThreshold,
		AutoPause5hDisabled:     autoPause5hDisabled,
		AutoPause7dDisabled:     autoPause7dDisabled,
		UsageLimitOverride:      ignoreUsageLimitStatusOverride,
		DispatchCountLimit:      dispatchCountLimit,
		SchedulerPriority:       schedulerPriority,
		ProxyURL:                proxyURL,
		CustomHeaders:           customHeaders,
		CodexFingerprintMode:    codexFingerprintMode,
		Timezone:                timezoneField,
		AccountHref:             accountHref,
		CredentialUpdates:       credentialUpdates,
	}, nil
}

// validateAccountTimezone allows an empty string (= clear); a non-empty value must be a loadable IANA timezone.
func validateAccountTimezone(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	if strings.EqualFold(v, "Local") {
		return fmt.Errorf("timezone must identify a fixed IANA location, not Local")
	}
	if _, err := time.LoadLocation(v); err != nil {
		return fmt.Errorf("timezone must be a valid IANA timezone, e.g. Asia/Shanghai")
	}
	return nil
}

// validateAccountHref allows an empty string (= clear the redirect address, falling back to api-base); anything else must be an
// absolute http/https address; normalization is done by auth.NormalizeAccountHref before the database write.
func validateAccountHref(value string) error {
	_, err := auth.NormalizeAccountHref(value)
	return err
}

// validateCodexFingerprintMode allows an empty string (equivalent to the default tier, off); anything else must be a known tier.
func validateCodexFingerprintMode(value string) error {
	if value == "" || auth.IsValidCodexFingerprintMode(value) {
		return nil
	}
	return errors.New("must be off, device, session, single_machine_multi_window, or full")
}

func (u accountSchedulerUpdate) hasChanges() bool {
	return u.ScoreBiasOverride.Set ||
		u.BaseConcurrencyOverride.Set ||
		u.SkipWarmTier.Set ||
		u.KeepConcurrency.Set ||
		u.AllowedAPIKeyIDs.Set ||
		u.Tags.Set ||
		u.GroupIDs.Set ||
		u.AutoPause5hThreshold.Set ||
		u.AutoPause7dThreshold.Set ||
		u.AutoPause5hDisabled.Set ||
		u.AutoPause7dDisabled.Set ||
		u.UsageLimitOverride.Set ||
		u.DispatchCountLimit.Set ||
		u.SchedulerPriority.Set ||
		u.ProxyURL.Set ||
		u.CustomHeaders.Set ||
		u.CodexFingerprintMode.Set ||
		u.Timezone.Set ||
		u.AccountHref.Set
}

func optionalBoolFromPtr(value *bool) database.OptionalBool {
	if value == nil {
		return database.OptionalBool{}
	}
	return database.OptionalBool{Set: true, Value: *value}
}

// UpdateAccountScheduler updates the account scheduling configuration.
// UpdateAccountCredit updates the account credit settings
func (h *Handler) UpdateAccountCredit(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req struct {
		CreditEnabled         *bool `json:"credit_enabled"`
		CreditSkipUsageWindow *bool `json:"credit_skip_usage_window"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}

	// Pass *bool: nil = do not modify this field
	if err := h.store.UpdateAccountCredit(id, req.CreditEnabled, req.CreditSkipUsageWindow); err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to update credit settings: "+err.Error())
		return
	}

	acc = h.store.FindByID(id)
	if acc != nil {
		// When the switch is turned on, the account may already be penalized by a usage window. Without actively releasing it, one would have to wait
		// until the window resets, and "found a rate limit, then turned the switch on" is the most common way it is used.
		released := h.store.ReleaseUsageWindowCooldownForCredits(acc)
		c.JSON(http.StatusOK, gin.H{
			"message":                  "Credit settings updated",
			"credit_enabled":           acc.CreditEnabled,
			"credit_skip_usage_window": acc.CreditSkipUsageWindow,
			"using_credits":            acc.UsingCredits(),
			"cooldown_released":        released,
		})
	} else {
		c.JSON(http.StatusOK, gin.H{"message": "Credit settings updated"})
	}
}

func (h *Handler) UpdateAccountScheduler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req updateAccountSchedulerReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	update, err := parseAccountSchedulerUpdate(req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if update.AllowedAPIKeyIDs.Set {
		missingAPIKeyIDs, err := h.findMissingAPIKeyIDs(ctx, update.AllowedAPIKeyIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "Failed to verify API Key: "+err.Error())
			return
		}
		if len(missingAPIKeyIDs) > 0 {
			values := make([]string, 0, len(missingAPIKeyIDs))
			for _, value := range missingAPIKeyIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_api_key_ids contains nonexistent API Key ID: "+strings.Join(values, ", "))
			return
		}
	}
	if update.GroupIDs.Set {
		missingGroupIDs, err := h.db.VerifyAccountGroupIDs(ctx, update.GroupIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "Failed to verify account groups: "+err.Error())
			return
		}
		if len(missingGroupIDs) > 0 {
			values := make([]string, 0, len(missingGroupIDs))
			for _, value := range missingGroupIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "group_ids contains nonexistent group ID: "+strings.Join(values, ", "))
			return
		}
		if len(update.GroupIDs.Values) > 0 {
			row, err := h.db.GetAccountByID(ctx, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(c, http.StatusNotFound, "Account not found")
					return
				}
				writeError(c, http.StatusInternalServerError, "Failed to query account: "+err.Error())
				return
			}
			if err := h.validateGroupChannelForRows(ctx, []*database.AccountRow{row}, update.GroupIDs.Values); err != nil {
				writeError(c, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	if update.Timezone.Set {
		if update.CredentialUpdates == nil {
			update.CredentialUpdates = make(map[string]interface{})
		}
		_, err := h.db.GetAccountByID(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusNotFound, "Account not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "Failed to query account: "+err.Error())
			return
		}
	}

	if update.CustomHeaders.Set {
		h.mergeDuplicateMu.Lock()
		defer h.mergeDuplicateMu.Unlock()

		row, err := h.db.GetAccountByID(ctx, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusNotFound, "Account not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "Failed to query account: "+err.Error())
			return
		}
		seed := tokenCredentialSeedFromAccountRow(row)
		previousOverride := openaiidentity.WorkspaceOverrideFromHeaders(seed.customHeaders)
		seed.customHeaders = cloneCustomHeaders(update.CustomHeaders.Values)
		seed = normalizeTokenCredentialSeed(seed)
		nextOverride := openaiidentity.WorkspaceOverrideFromHeaders(seed.customHeaders)

		if previousOverride != nextOverride {
			if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
				duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, id)
				if err != nil {
					writeError(c, http.StatusInternalServerError, "Failed to verify workspace route: "+err.Error())
					return
				}
				if duplicateID > 0 {
					writeError(c, http.StatusConflict, fmt.Sprintf("The target workspace for this login identity already exists (id=%d)", duplicateID))
					return
				}
			}
			duplicateID, err := h.findCredentialWorkspaceRouteDuplicate(ctx, seed, id)
			if err != nil {
				writeError(c, http.StatusInternalServerError, "Failed to verify credential workspace route: "+err.Error())
				return
			}
			if duplicateID > 0 {
				writeError(c, http.StatusConflict, fmt.Sprintf("The target workspace for the same credential already exists (id=%d)", duplicateID))
				return
			}
		}
	}

	if err := h.db.UpdateAccountSchedulerMetadata(ctx, id, update.ScoreBiasOverride, update.BaseConcurrencyOverride, update.SkipWarmTier, update.AllowedAPIKeyIDs, database.OptionalStringSlice{Set: update.Tags.Set, Values: update.Tags.Values}, update.GroupIDs, update.ProxyURL, update.CredentialUpdates); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to update account scheduling configuration: "+err.Error())
		return
	}
	h.applyAccountSchedulerRuntimeUpdate(id, update)

	writeMessage(c, http.StatusOK, "Account scheduling settings updated")
}

func (h *Handler) applyAccountSchedulerRuntimeUpdate(id int64, update accountSchedulerUpdate) {
	if h.store == nil {
		return
	}
	if update.ScoreBiasOverride.Set || update.BaseConcurrencyOverride.Set || update.SkipWarmTier.Set {
		h.store.ApplyAccountSchedulerOverridePatch(
			id,
			update.ScoreBiasOverride.Set,
			nullableInt64Pointer(update.ScoreBiasOverride.Value),
			update.BaseConcurrencyOverride.Set,
			nullableInt64Pointer(update.BaseConcurrencyOverride.Value),
			optionalBoolPtr(update.SkipWarmTier),
		)
	}
	if update.KeepConcurrency.Set {
		h.store.ApplyAccountKeepConcurrencyOnDegrade(id, update.KeepConcurrency.Value)
	}
	if update.AllowedAPIKeyIDs.Set {
		h.store.ApplyAccountAllowedAPIKeys(id, update.AllowedAPIKeyIDs.Values)
	}
	if update.AutoPause5hThreshold.Set || update.AutoPause7dThreshold.Set || update.AutoPause5hDisabled.Set || update.AutoPause7dDisabled.Set {
		h.store.ApplyAccountQuotaAutoPauseConfig(
			id,
			optionalFloat64Ptr(update.AutoPause5hThreshold),
			optionalFloat64Ptr(update.AutoPause7dThreshold),
			optionalBoolPtr(update.AutoPause5hDisabled),
			optionalBoolPtr(update.AutoPause7dDisabled),
		)
	}
	if update.UsageLimitOverride.Set {
		h.store.ApplyAccountIgnoreUsageLimitStatus(id, update.UsageLimitOverride.Value)
	}
	if update.DispatchCountLimit.Set {
		h.store.ApplyAccountDispatchCountLimit(id, nullableInt64Pointer(update.DispatchCountLimit.Value))
	}
	if update.SchedulerPriority.Set {
		h.store.ApplyAccountSchedulerPriority(id, nullableInt64Pointer(update.SchedulerPriority.Value))
	}
	if update.Tags.Set {
		h.store.ApplyAccountTags(id, update.Tags.Values)
	}
	if update.GroupIDs.Set {
		h.store.ApplyAccountGroups(id, update.GroupIDs.Values)
	}
	if update.ProxyURL.Set {
		h.store.ApplyAccountProxyURL(id, update.ProxyURL.Value)
	}
	if value, ok := update.CredentialUpdates[auth.UpstreamRequestIDHeaderCredentialKey].(string); ok {
		h.store.ApplyAccountUpstreamRequestIDHeader(id, value)
	}
	if update.CustomHeaders.Set {
		h.store.ApplyAccountCustomHeaders(id, update.CustomHeaders.Values)
	}
	if update.CodexFingerprintMode.Set {
		h.store.ApplyAccountCodexFingerprintMode(id, update.CodexFingerprintMode.Value)
	}
	if update.Timezone.Set {
		h.store.ApplyAccountTimezone(id, update.Timezone.Value)
	}
}

type optionalCustomHeaders struct {
	Set    bool
	Values map[string]string
}

type optionalNullableBool struct {
	Set   bool
	Value *bool
}

func parseOptionalCustomHeadersField(raw json.RawMessage) (optionalCustomHeaders, error) {
	if len(raw) == 0 {
		return optionalCustomHeaders{}, nil
	}
	if string(raw) == "null" {
		return optionalCustomHeaders{Set: true}, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return optionalCustomHeaders{}, fmt.Errorf("custom_headers must be an object or null")
	}
	normalized, err := normalizeCustomHeaders(headers)
	if err != nil {
		return optionalCustomHeaders{}, err
	}
	return optionalCustomHeaders{Set: true, Values: normalized}, nil
}

func normalizeCustomHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	if len(headers) > 64 {
		return nil, fmt.Errorf("custom_headers supports at most 64 headers")
	}
	out := make(map[string]string, len(headers))
	for rawName, rawValue := range headers {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if len(name) > 128 || !isValidHeaderName(name) {
			return nil, fmt.Errorf("custom_headers contains an invalid header name: %s", name)
		}
		value := strings.TrimSpace(rawValue)
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("custom_headers.%s must not contain line breaks", name)
		}
		if len(value) > 8192 {
			return nil, fmt.Errorf("custom_headers.%s must not exceed 8192 characters", name)
		}
		canonicalName := http.CanonicalHeaderKey(name)
		if previous, exists := out[canonicalName]; exists && previous != value {
			return nil, fmt.Errorf("custom_headers contains headers that differ only in case with conflicting values: %s", canonicalName)
		}
		out[canonicalName] = value
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func isValidHeaderName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return name != ""
}

func cloneCustomHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[name] = value
	}
	return out
}

func customHeadersWithWorkspaceOverride(headers map[string]string, workspaceID string) map[string]string {
	out := make(map[string]string, len(headers)+1)
	for name, value := range headers {
		if strings.EqualFold(strings.TrimSpace(name), openaiidentity.ChatGPTAccountIDHeader) {
			continue
		}
		out[name] = value
	}
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID != "" {
		out[openaiidentity.ChatGPTAccountIDHeader] = workspaceID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func credentialWorkspaceRouteKey(kind, token string, customHeaders map[string]string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return kind + "\x00" + token + "\x00" + openaiidentity.WorkspaceOverrideFromHeaders(customHeaders)
}

func tokenCredentialSeedWorkspaceRouteKeys(seed tokenCredentialSeed) []string {
	seed = normalizeTokenCredentialSeed(seed)
	keys := make([]string, 0, 3)
	for _, credential := range []struct {
		kind  string
		token string
	}{
		{kind: "rt", token: seed.refreshToken},
		{kind: "st", token: seed.sessionToken},
		{kind: "at", token: seed.accessToken},
	} {
		if key := credentialWorkspaceRouteKey(credential.kind, credential.token, seed.customHeaders); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func accountCredentialWorkspaceRouteKeys(row *database.AccountRow) []string {
	if row == nil {
		return nil
	}
	return tokenCredentialSeedWorkspaceRouteKeys(tokenCredentialSeedFromAccountRow(row))
}

// existingCredentialWorkspaceRouteOwners returns a map from "credential workspace route key -> the
// active account ID holding that credential". On a dedup hit, the caller looks up the account state by ID to decide whether to
// simply skip it or revive an old account in an error state. In theory a route has only one active account; if there are several
// (imported with allow_duplicate), keeping the first one encountered is enough.
func (h *Handler) existingCredentialWorkspaceRouteOwners(ctx context.Context) (map[string]int64, error) {
	if h == nil || h.db == nil {
		return nil, fmt.Errorf("database is not configured")
	}
	rows, err := h.db.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]int64, len(rows))
	for _, row := range rows {
		for _, key := range accountCredentialWorkspaceRouteKeys(row) {
			if _, exists := owners[key]; !exists {
				owners[key] = row.ID
			}
		}
	}
	return owners, nil
}

// reviveDuplicateRouteOwner, after a dedup hit on an existing account by credential route, tries to revive it (only when
// the account is in error / unauthorized state). Returns true if revived; the caller should count it as
// "updated" rather than "duplicate".
func (h *Handler) reviveDuplicateRouteOwner(ctx context.Context, ownerID int64, source string) bool {
	if h == nil || h.db == nil || ownerID <= 0 {
		return false
	}
	row, err := h.db.GetAccountByID(ctx, ownerID)
	if err != nil || row == nil {
		return false
	}
	return h.reviveReimportedAccount(ctx, row, source)
}

func (h *Handler) findCredentialWorkspaceRouteDuplicate(ctx context.Context, seed tokenCredentialSeed, excludeID int64) (int64, error) {
	candidateKeys := tokenCredentialSeedWorkspaceRouteKeys(seed)
	if len(candidateKeys) == 0 {
		return 0, nil
	}
	candidates := make(map[string]struct{}, len(candidateKeys))
	for _, key := range candidateKeys {
		candidates[key] = struct{}{}
	}
	rows, err := h.db.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if row.ID == excludeID {
			continue
		}
		for _, key := range accountCredentialWorkspaceRouteKeys(row) {
			if _, ok := candidates[key]; ok {
				return row.ID, nil
			}
		}
	}
	return 0, nil
}

type optionalStringSlice struct {
	Set    bool
	Values []string
}

type optionalFloat64 struct {
	Set   bool
	Value float64
}

func accountQuotaAutoPauseThreshold(row *database.AccountRow, key string) *float64 {
	value, ok := row.GetCredentialFloat64(key)
	if !ok || value <= 0 {
		return nil
	}
	if value > 1 {
		value = 1
	}
	return &value
}

func accountDispatchCountLimit(row *database.AccountRow) *int64 {
	value, ok := row.GetCredentialInt64("dispatch_count_limit")
	if !ok || value <= 0 {
		return nil
	}
	if value > 1000000 {
		value = 1000000
	}
	return &value
}

func accountSchedulerPriority(row *database.AccountRow) *int64 {
	value, ok := row.GetCredentialInt64("scheduler_priority")
	if !ok || value == 0 {
		return nil
	}
	if value > 100 {
		value = 100
	}
	if value < -100 {
		value = -100
	}
	return &value
}

func parseOptionalStringSliceField(raw json.RawMessage, field string) (optionalStringSlice, error) {
	if len(raw) == 0 {
		return optionalStringSlice{}, nil
	}
	if string(raw) == "null" {
		return optionalStringSlice{Set: true, Values: []string{}}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return optionalStringSlice{}, fmt.Errorf("%s must be a string array or null", field)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		clean := strings.TrimSpace(value)
		if clean == "" {
			continue
		}
		if utf8.RuneCountInString(clean) > 40 {
			return optionalStringSlice{}, fmt.Errorf("each tag in %s must not exceed 40 characters", field)
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	if len(out) > 32 {
		return optionalStringSlice{}, fmt.Errorf("%s allows at most 32 tags", field)
	}
	return optionalStringSlice{Set: true, Values: out}, nil
}

func parseOptionalStringField(raw json.RawMessage, field string, validator func(string) error) (database.OptionalString, error) {
	if len(raw) == 0 {
		return database.OptionalString{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalString{Set: true, Value: ""}, nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return database.OptionalString{}, fmt.Errorf("%s must be a string or null", field)
	}
	value = strings.TrimSpace(value)
	if validator != nil {
		if err := validator(value); err != nil {
			return database.OptionalString{}, fmt.Errorf("%s is invalid: %w", field, err)
		}
	}
	return database.OptionalString{Set: true, Value: value}, nil
}

func parseOptionalIntegerField(raw json.RawMessage, field string, minValue, maxValue int64) (database.OptionalNullInt64, error) {
	if len(raw) == 0 {
		return database.OptionalNullInt64{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalNullInt64{Set: true}, nil
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return database.OptionalNullInt64{}, fmt.Errorf("%s must be an integer or null", field)
	}
	value, err := number.Int64()
	if err != nil {
		return database.OptionalNullInt64{}, fmt.Errorf("%s must be an integer or null", field)
	}
	if value < minValue || value > maxValue {
		if maxValue == math.MaxInt64 {
			return database.OptionalNullInt64{}, fmt.Errorf("%s is out of range, must be >= %d", field, minValue)
		}
		return database.OptionalNullInt64{}, fmt.Errorf("%s is out of range, must be between %d..%d", field, minValue, maxValue)
	}
	return database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: value, Valid: true}}, nil
}

func parseOptionalRatioField(raw json.RawMessage, field string) (optionalFloat64, error) {
	if len(raw) == 0 {
		return optionalFloat64{}, nil
	}
	if string(raw) == "null" {
		return optionalFloat64{Set: true, Value: 0}, nil
	}

	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return optionalFloat64{}, fmt.Errorf("%s must be a decimal between 0..1 or null", field)
	}
	value, err := number.Float64()
	if err != nil {
		return optionalFloat64{}, fmt.Errorf("%s must be a decimal between 0..1 or null", field)
	}
	if value < 0 || value > 1 {
		return optionalFloat64{}, fmt.Errorf("%s is out of range, must be between 0..1", field)
	}
	return optionalFloat64{Set: true, Value: value}, nil
}

func parseOptionalBoolField(raw json.RawMessage, field string) (database.OptionalBool, error) {
	if len(raw) == 0 {
		return database.OptionalBool{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalBool{Set: true, Value: false}, nil
	}

	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return database.OptionalBool{}, fmt.Errorf("%s must be a boolean or null", field)
	}
	return database.OptionalBool{Set: true, Value: value}, nil
}

func parseOptionalNullableBoolField(raw json.RawMessage, field string) (optionalNullableBool, error) {
	if len(raw) == 0 {
		return optionalNullableBool{}, nil
	}
	if string(raw) == "null" {
		return optionalNullableBool{Set: true}, nil
	}

	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return optionalNullableBool{}, fmt.Errorf("%s must be a boolean or null", field)
	}
	return optionalNullableBool{Set: true, Value: &value}, nil
}

func parseOptionalIntegerSliceField(raw json.RawMessage, field string) (database.OptionalInt64Slice, error) {
	if len(raw) == 0 {
		return database.OptionalInt64Slice{}, nil
	}
	if string(raw) == "null" {
		return database.OptionalInt64Slice{Set: true, Values: []int64{}}, nil
	}

	var values []json.Number
	if err := json.Unmarshal(raw, &values); err != nil {
		return database.OptionalInt64Slice{}, fmt.Errorf("%s must be an integer array or null", field)
	}
	if len(values) == 0 {
		return database.OptionalInt64Slice{Set: true, Values: []int64{}}, nil
	}

	unique := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, number := range values {
		value, err := number.Int64()
		if err != nil {
			return database.OptionalInt64Slice{}, fmt.Errorf("%s must be an integer array or null", field)
		}
		if value <= 0 {
			return database.OptionalInt64Slice{}, fmt.Errorf("values in %s must be positive integers", field)
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
	return database.OptionalInt64Slice{Set: true, Values: result}, nil
}

func (h *Handler) findMissingAPIKeyIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[int64]struct{}, len(keys))
	for _, key := range keys {
		if key == nil {
			continue
		}
		existing[key.ID] = struct{}{}
	}

	missing := make([]int64, 0)
	for _, id := range ids {
		if _, ok := existing[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	return missing, nil
}

func nullableInt64Pointer(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

func optionalFloat64Ptr(value optionalFloat64) *float64 {
	if !value.Set {
		return nil
	}
	v := value.Value
	return &v
}

func optionalBoolPtr(value database.OptionalBool) *bool {
	if !value.Set {
		return nil
	}
	v := value.Value
	return &v
}

func effectiveScoreBias(planType string, override sql.NullInt64) int64 {
	if override.Valid {
		return override.Int64
	}
	// Keep consistent with auth.defaultScoreBiasForPlan; k12 is the education-edition team plan (issue #282)
	switch auth.NormalizePlanType(planType) {
	case "pro", "plus", "team", "k12":
		return 50
	default:
		return 0
	}
}

func effectiveBaseConcurrency(override sql.NullInt64, defaultValue int64) int64 {
	if override.Valid {
		return override.Int64
	}
	return defaultValue
}

func dispatchScoreFallback(schedulerScore float64, scoreBiasEffective int64, healthTier string, status string) float64 {
	if schedulerScore == 0 {
		return 0
	}
	if !allowScoreBias(healthTier, status) {
		return schedulerScore
	}
	return schedulerScore + float64(scoreBiasEffective)
}

func allowScoreBias(healthTier string, status string) bool {
	if status != "" && status != "active" {
		return false
	}
	switch strings.ToLower(healthTier) {
	case "healthy", "warm":
		return true
	default:
		return false
	}
}

// Prefer the runtime/debug fields newly added by the parallel auth-layer implementation; the field names follow this convention:
// DispatchScore / ScoreBiasEffective / BaseConcurrencyEffective.
// If those fields are not yet integrated on the main branch, fall back to compatibility values the admin layer can derive, to avoid blocking front-end/back-end integration.
func reflectFloat64Field(value interface{}, field string) (float64, bool) {
	v := reflect.Indirect(reflect.ValueOf(value))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Float32, reflect.Float64:
		return f.Convert(reflect.TypeOf(float64(0))).Float(), true
	default:
		return 0, false
	}
}

func reflectInt64Field(value interface{}, field string) (int64, bool) {
	v := reflect.Indirect(reflect.ValueOf(value))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return f.Int(), true
	default:
		return 0, false
	}
}

// getCachedRequestCounts preserves the legacy full-list behavior: a blocking
// global aggregation cached under its own key, so it never contends with the
// per-channel paged caches.
func (h *Handler) getCachedRequestCounts() map[int64]*database.AccountRequestCount {
	const cacheKey = "all"
	h.reqCountMu.RLock()
	entry := h.reqCountCache[cacheKey]
	if entry != nil && time.Now().Before(entry.expiresAt) {
		h.reqCountMu.RUnlock()
		return entry.counts
	}
	h.reqCountMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	counts, err := h.db.GetAccountRequestCounts(ctx)
	if err != nil {
		log.Printf("failed to get account request stats: %v", err)
		return make(map[int64]*database.AccountRequestCount)
	}
	h.storeRequestCountCache(cacheKey, counts, nil, time.Time{})
	return counts
}

func (h *Handler) getAccountUsageWindows(ctx context.Context) (map[int64]*database.AccountTimeRangeUsage, map[int64]*database.AccountTimeRangeUsage) {
	type cachedUsageWindows struct {
		Usage5h map[int64]*database.AccountTimeRangeUsage `json:"usage_5h"`
		Usage7d map[int64]*database.AccountTimeRangeUsage `json:"usage_7d"`
	}
	var cached cachedUsageWindows
	if h.getRuntimeJSON(ctx, adminAccountWindowsNamespace, "global", &cached) && cached.Usage5h != nil && cached.Usage7d != nil {
		return cached.Usage5h, cached.Usage7d
	}
	now := time.Now()
	usage5h, usage7d, err := h.db.GetAccountUsageWindows(ctx, now.Add(-5*time.Hour), now.AddDate(0, 0, -7))
	if err != nil {
		log.Printf("failed to get account 5h/7d usage stats: %v", err)
		usage5h = make(map[int64]*database.AccountTimeRangeUsage)
		usage7d = make(map[int64]*database.AccountTimeRangeUsage)
		return usage5h, usage7d
	}
	h.setRuntimeJSON(ctx, adminAccountWindowsNamespace, "global", cachedUsageWindows{Usage5h: usage5h, Usage7d: usage7d}, adminAccountWindowsCacheTTL)
	return usage5h, usage7d
}

type addAccountReq struct {
	Name           string            `json:"name"`
	RefreshToken   string            `json:"refresh_token"`
	SessionToken   string            `json:"session_token"`
	ProxyURL       string            `json:"proxy_url"`
	CustomHeaders  map[string]string `json:"custom_headers"`
	AllowDuplicate bool              `json:"allow_duplicate"`
	// SkipRefresh only stores the account: it does not exchange the AT and does not run a usage probe. Turn it on for large scripted imports,
	// to avoid every batch of 50 bare RTs immediately launching unbounded upstream refreshes that choke the gateway.
	SkipRefresh bool `json:"skip_refresh"`
	// GroupIDs binds the new accounts to the given groups at add time; duplicates that are skipped are not affected.
	GroupIDs json.RawMessage `json:"group_ids"`
}

func splitAccountCredentialLines(raw string, sanitize bool) []string {
	lines := strings.Split(raw, "\n")
	tokens := make([]string, 0, len(lines))
	for _, line := range lines {
		token := strings.TrimSpace(line)
		if sanitize {
			token = strings.TrimSpace(security.SanitizeInput(token))
		}
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// accountCredentialDedup tracks dedup of raw RT/ST text (used by the single-account and batch RT/ST add paths).
// Identity-based (OAuth) dedup is handled separately in file import and the AT path; this only covers RT/ST whose identity cannot be decoded at add time.
type accountCredentialDedup struct {
	// existingOwners maps route key -> the ID of the active account in the database that holds the credential.
	existingOwners map[string]int64
	seenRoutes     map[string]bool
}

func (h *Handler) newAccountCredentialDedup(ctx context.Context) *accountCredentialDedup {
	d := &accountCredentialDedup{
		seenRoutes: make(map[string]bool),
	}
	existingOwners, err := h.existingCredentialWorkspaceRouteOwners(ctx)
	if err != nil {
		log.Printf("failed to query existing credential workspace routes: %v", err)
		existingOwners = make(map[string]int64)
	}
	d.existingOwners = existingOwners
	return d
}

func (d *accountCredentialDedup) routeKeys(seed tokenCredentialSeed) []string {
	keys := make([]string, 0, 2)
	if key := credentialWorkspaceRouteKey("rt", seed.refreshToken, seed.customHeaders); key != "" {
		keys = append(keys, key)
	}
	if key := credentialWorkspaceRouteKey("st", seed.sessionToken, seed.customHeaders); key != "" {
		keys = append(keys, key)
	}
	return keys
}

// checkAndMarkOwner returns whether this seed duplicates an existing record or one in this batch (and should be skipped); when it is not a
// duplicate, it records the credential. On a hit against an existing account in the database, it also returns that account's ID (a duplicate within this batch returns
// 0), so the caller can decide whether to revive an old account in an error state.
func (d *accountCredentialDedup) checkAndMarkOwner(seed tokenCredentialSeed) (bool, int64) {
	keys := d.routeKeys(seed)
	for _, key := range keys {
		if ownerID, ok := d.existingOwners[key]; ok && ownerID > 0 {
			return true, ownerID
		}
		if d.seenRoutes[key] {
			return true, 0
		}
	}
	for _, key := range keys {
		d.seenRoutes[key] = true
	}
	return false, 0
}

// AddAccount adds new accounts (batch supported: refresh_token/session_token split by line)
func (h *Handler) AddAccount(c *gin.Context) {
	var req addAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	// Validate and sanitize input
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if strings.TrimSpace(req.RefreshToken) == "" && strings.TrimSpace(req.SessionToken) == "" {
		writeError(c, http.StatusBadRequest, "refresh_token or session_token is required")
		return
	}

	// Check for XSS and SQL injection
	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "Name contains invalid characters")
		return
	}

	// Validate name length
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "Name must not exceed 100 characters")
		return
	}

	// Validate proxy URL
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid proxy URL")
		return
	}
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.CustomHeaders = customHeaders

	// Split by line to support batch add. When refresh_token and session_token are both filled in,
	// session_token may be one line applied to every RT, or one line per RT line.
	refreshTokens := splitAccountCredentialLines(req.RefreshToken, true)
	sessionTokens := splitAccountCredentialLines(req.SessionToken, true)
	total := len(refreshTokens)
	if total == 0 {
		total = len(sessionTokens)
	}
	if len(refreshTokens) > 0 && len(sessionTokens) > 1 && len(sessionTokens) != len(refreshTokens) {
		writeError(c, http.StatusBadRequest, "session_token line count must be 1 or match the refresh_token line count")
		return
	}

	var seeds []tokenCredentialSeed
	for i := 0; i < total; i++ {
		seed := tokenCredentialSeed{allowDuplicate: req.AllowDuplicate, customHeaders: customHeaders}
		if len(refreshTokens) > 0 {
			seed.refreshToken = refreshTokens[i]
		}
		if len(sessionTokens) == 1 {
			seed.sessionToken = sessionTokens[0]
		} else if len(sessionTokens) > 1 {
			seed.sessionToken = sessionTokens[i]
		}
		seeds = append(seeds, seed)
	}

	if len(seeds) == 0 {
		writeError(c, http.StatusBadRequest, "No valid Refresh Token or Session Token found")
		return
	}

	// Limit the batch add count
	if len(seeds) > 100 {
		writeError(c, http.StatusBadRequest, "Up to 100 accounts can be added at once")
		return
	}

	// Validate groups before inserting accounts: a wrong group ID must not leave half of the accounts already stored.
	groupCtx, groupCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	groupIDs, err := h.resolveImportGroupIDsJSON(groupCtx, req.GroupIDs)
	groupCancel()
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamAddAccounts(c, req, seeds, groupIDs)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0
	duplicateCount := 0
	revivedCount := 0
	createdIDs := &importedAccountIDs{}
	pending := make([]*auth.Account, 0, len(seeds))

	var dedup *accountCredentialDedup
	if !req.AllowDuplicate || openaiidentity.WorkspaceOverrideFromHeaders(customHeaders) != "" {
		dedup = h.newAccountCredentialDedup(ctx)
	}

	for i, seed := range seeds {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(seeds) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		if dedup != nil {
			if duplicate, ownerID := dedup.checkAndMarkOwner(seed); duplicate {
				// The same credential is added again while the old account is in error / 401 state: treat it as a revive request.
				if h.reviveDuplicateRouteOwner(ctx, ownerID, "manual_add") {
					revivedCount++
					continue
				}
				duplicateCount++
				log.Printf("account %d already exists (duplicate RT/ST), skipped", i+1)
				continue
			}
		}

		id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), req.ProxyURL)
		if err != nil {
			log.Printf("batch add account %d failed: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		createdIDs.add(id)
		pending = append(pending, h.newCodexAccountFromSeed(id, req.ProxyURL, seed))
	}
	h.db.BatchInsertAccountEventsAsync(createdIDs.snapshot(), "added", "manual")
	h.commitImportedRuntimeAccounts(pending, "manual_add", req.SkipRefresh)

	// Record security audit log
	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, revivedCount, duplicateCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("Added %d accounts", successCount)
	if revivedCount > 0 {
		msg += fmt.Sprintf(", %d existing accounts restored", revivedCount)
	}
	if duplicateCount > 0 {
		msg += fmt.Sprintf(", %d duplicates skipped", duplicateCount)
	}
	if failCount > 0 {
		msg += fmt.Sprintf(", %d failed", failCount)
	}
	boundGroups := len(groupIDs) > 0
	if err := h.bindImportedAccountGroups(ctx, createdIDs.snapshot(), groupIDs); err != nil {
		// The accounts are stored, but the group binding did not go through; this must be reported, or the user will think it was bound.
		boundGroups = false
		msg += ", but group binding failed: " + err.Error()
	}

	newAccountIDs := createdIDs.snapshot()
	h.scheduleInviteGuideProbes(ctx, newAccountIDs)

	c.JSON(http.StatusOK, gin.H{
		"message":      msg,
		"success":      successCount,
		"updated":      revivedCount,
		"duplicate":    duplicateCount,
		"failed":       failCount,
		"bound_groups": boundGroups,
		"group_ids":    groupIDs,
		"created_ids":  newAccountIDs,
	})
}

func (h *Handler) streamAddAccounts(c *gin.Context, req addAccountReq, seeds []tokenCredentialSeed, groupIDs []int64) {
	setupSSE(c)

	total := len(seeds)
	successCount := 0
	failCount := 0
	duplicateCount := 0
	revivedCount := 0
	sendImportEvent(c, importEvent{
		Type: "progress", Current: 0, Total: total,
		Success: 0, Updated: 0, Duplicate: 0, Failed: 0,
	})
	progress := func(current int) {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: current, Total: total,
			Success: successCount, Updated: revivedCount, Duplicate: duplicateCount, Failed: failCount,
		})
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	var dedup *accountCredentialDedup
	if !req.AllowDuplicate || openaiidentity.WorkspaceOverrideFromHeaders(req.CustomHeaders) != "" {
		dedup = h.newAccountCredentialDedup(ctx)
	}
	createdIDs := &importedAccountIDs{}
	pending := make([]*auth.Account, 0, len(seeds))

	for i, seed := range seeds {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", i+1)
		} else if len(seeds) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		if dedup != nil {
			if duplicate, ownerID := dedup.checkAndMarkOwner(seed); duplicate {
				// The same credential is added again while the old account is in error / 401 state: treat it as a revive request.
				if h.reviveDuplicateRouteOwner(ctx, ownerID, "manual_add") {
					revivedCount++
				} else {
					duplicateCount++
				}
				progress(i + 1)
				continue
			}
		}

		id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), req.ProxyURL)
		if err != nil {
			log.Printf("batch add account %d failed: %v", i+1, err)
			failCount++
			progress(i + 1)
			continue
		}

		successCount++
		createdIDs.add(id)
		pending = append(pending, h.newCodexAccountFromSeed(id, req.ProxyURL, seed))

		progress(i + 1)
	}
	h.db.BatchInsertAccountEventsAsync(createdIDs.snapshot(), "added", "manual")
	h.commitImportedRuntimeAccounts(pending, "manual_add", req.SkipRefresh)

	security.SecurityAuditLog("ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, revivedCount, duplicateCount, failCount, c.ClientIP()))
	// Binding must finish before the complete event: the frontend refreshes the list as soon as it receives complete.
	if err := h.bindImportedAccountGroups(ctx, createdIDs.snapshot(), groupIDs); err != nil {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: total, Total: total,
			Success: successCount, Updated: revivedCount, Duplicate: duplicateCount, Failed: failCount,
			Warning: "Account added, but group binding failed: " + err.Error(),
		})
	}
	newAccountIDs := createdIDs.snapshot()
	h.scheduleInviteGuideProbes(ctx, newAccountIDs)

	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: successCount, Updated: revivedCount, Duplicate: duplicateCount, Failed: failCount,
		CreatedIDs: newAccountIDs,
	})
}

// addATAccountReq is the request for adding an account in AT mode
type addATAccountReq struct {
	Name           string            `json:"name"`
	AccessToken    string            `json:"access_token"`
	ProxyURL       string            `json:"proxy_url"`
	CustomHeaders  map[string]string `json:"custom_headers"`
	AllowDuplicate bool              `json:"allow_duplicate"`
	// GroupIDs binds the new accounts to the given groups at add time; duplicates that are skipped and accounts updated because they matched an existing identity are not affected.
	GroupIDs json.RawMessage `json:"group_ids"`
}

// AddATAccount adds AT-only accounts (batch supported: access_token split by line)
func (h *Handler) AddATAccount(c *gin.Context) {
	var req addATAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	if req.AccessToken == "" {
		writeError(c, http.StatusBadRequest, "access_token is required")
		return
	}

	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "Name contains invalid characters")
		return
	}

	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "Name must not exceed 100 characters")
		return
	}

	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "invalid proxy URL")
		return
	}
	customHeaders, err := normalizeCustomHeaders(req.CustomHeaders)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.CustomHeaders = customHeaders

	// Split by line, supporting batch addition
	lines := strings.Split(req.AccessToken, "\n")
	var tokens []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t != "" {
			tokens = append(tokens, t)
		}
	}

	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "no valid Access Token found")
		return
	}

	if len(tokens) > 100 {
		writeError(c, http.StatusBadRequest, "at most 100 accounts can be added at once")
		return
	}

	groupCtx, groupCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	groupIDs, err := h.resolveImportGroupIDsJSON(groupCtx, req.GroupIDs)
	groupCancel()
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamAddATAccounts(c, req, tokens, groupIDs)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	successCount := 0
	failCount := 0
	updatedCount := 0
	duplicateCount := 0
	createdIDs := &importedAccountIDs{}
	pending := make([]*auth.Account, 0, len(tokens))

	// AT dedup: non-identity AT-only entries (email + valid workspace cannot be decoded from the JWT, e.g. codex_at)
	// Deduplicate by the raw access_token; identity-type ATs are handled by upsertOAuthIdentityAccount by OAuth identity
	// (email + valid workspace) for deduplication/update. An explicit Chatgpt-Account-Id can split the same AT
	// into multiple independent workspace routes; the same route still updates the existing account.
	existingATRoutes := make(map[string]int64)
	seenATRoutes := make(map[string]bool)
	workspaceOverrideKnown := openaiidentity.WorkspaceOverrideFromHeaders(customHeaders) != ""
	if !req.AllowDuplicate || workspaceOverrideKnown {
		if got, err := h.existingCredentialWorkspaceRouteOwners(ctx); err != nil {
			log.Printf("failed to query existing credential workspace route: %v", err)
		} else {
			existingATRoutes = got
		}
	}

	whoami := &patWhoAmIHydrator{proxyURL: req.ProxyURL}
	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if len(tokens) > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
			accessToken:    at,
			allowDuplicate: req.AllowDuplicate,
			customHeaders:  customHeaders,
		})
		// codex_at does not go through OAuth identity dedup: the email+workspace filled in by whoami would collide with the same-workspace
		// OAuth account; merging the PAT into it would overwrite that account's access_token with the PAT. PAT is always deduplicated by its token routing key.
		if seed.accessTokenType != accessTokenTypeCodexAT && seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
			id, updated, newAcc, err := h.upsertOAuthIdentityAccountDeferred(ctx, name, req.ProxyURL, seed, "manual_at", overwriteAccountProxy)
			if err != nil {
				log.Printf("failed to add AT account %d: %v", i+1, err)
				failCount++
				continue
			}
			if updated {
				// An existing account only has its credentials updated; it is not counted as "added".
				updatedCount++
				log.Printf("AT account %d matched an existing identity and updated its credentials (id=%d)", i+1, id)
			} else {
				successCount++
				createdIDs.add(id)
				pending = append(pending, newAcc)
				log.Printf("AT account %d joined the account pool (id=%d)", i+1, id)
			}
			continue
		}

		if !req.AllowDuplicate || workspaceOverrideKnown {
			routeKey := credentialWorkspaceRouteKey("at", at, seed.customHeaders)
			if ownerID, exists := existingATRoutes[routeKey]; exists || seenATRoutes[routeKey] {
				// The same AT was added again while the old account is in error / 401 state: treat it as a request to revive it.
				if exists && h.reviveDuplicateRouteOwner(ctx, ownerID, "manual_at") {
					updatedCount++
					continue
				}
				duplicateCount++
				log.Printf("AT account %d already exists (access_token duplicates the target workspace), skipped", i+1)
				continue
			}
			seenATRoutes[routeKey] = true
		}

		// Fill in identities after dedup: re-importing a known batch should not pay N network round trips again.
		seed = whoami.hydrate(ctx, seed)
		id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), req.ProxyURL)
		if err != nil {
			log.Printf("failed to add AT account %d: %v", i+1, err)
			failCount++
			continue
		}

		successCount++
		createdIDs.add(id)

		// Hot-load into the in-memory pool (AT-only, no RT). codex_at does not go through JWT decoding,
		// identity info is filled in later by the wham usage query.
		newAcc := h.newCodexAccountFromSeed(id, req.ProxyURL, seed)
		pending = append(pending, newAcc)
		log.Printf("AT account %d joined the account pool (id=%d, email=%s)", i+1, id, newAcc.Email)
	}
	h.db.BatchInsertAccountEventsAsync(createdIDs.snapshot(), "added", "manual_at")
	h.commitImportedRuntimeAccounts(pending, "manual_at", false)

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, updatedCount, duplicateCount, failCount, c.ClientIP()))

	msg := fmt.Sprintf("successfully added %d AT accounts", successCount)
	if updatedCount > 0 {
		msg += fmt.Sprintf(", %d existing accounts updated", updatedCount)
	}
	if duplicateCount > 0 {
		msg += fmt.Sprintf(", %d duplicates skipped", duplicateCount)
	}
	if failCount > 0 {
		msg += fmt.Sprintf(", %d failed", failCount)
	}
	boundGroups := len(groupIDs) > 0
	if err := h.bindImportedAccountGroups(ctx, createdIDs.snapshot(), groupIDs); err != nil {
		boundGroups = false
		msg += ", but group binding failed: " + err.Error()
	}

	c.JSON(http.StatusOK, gin.H{
		"message":      msg,
		"success":      successCount,
		"updated":      updatedCount,
		"duplicate":    duplicateCount,
		"failed":       failCount,
		"bound_groups": boundGroups,
		"group_ids":    groupIDs,
	})
}

// streamAddATAccounts streams AT batch-add progress over SSE (aligned with streamAddAccounts).
func (h *Handler) streamAddATAccounts(c *gin.Context, req addATAccountReq, tokens []string, groupIDs []int64) {
	setupSSE(c)

	total := len(tokens)
	successCount := 0
	failCount := 0
	updatedCount := 0
	duplicateCount := 0
	sendImportEvent(c, importEvent{
		Type: "progress", Current: 0, Total: total,
		Success: 0, Updated: 0, Duplicate: 0, Failed: 0,
	})

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	existingATRoutes := make(map[string]int64)
	seenATRoutes := make(map[string]bool)
	workspaceOverrideKnown := openaiidentity.WorkspaceOverrideFromHeaders(req.CustomHeaders) != ""
	if !req.AllowDuplicate || workspaceOverrideKnown {
		if got, err := h.existingCredentialWorkspaceRouteOwners(ctx); err != nil {
			log.Printf("failed to query existing credential workspace route: %v", err)
		} else {
			existingATRoutes = got
		}
	}

	progress := func(current int) {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: current, Total: total,
			Success: successCount, Updated: updatedCount, Duplicate: duplicateCount, Failed: failCount,
		})
	}
	createdIDs := &importedAccountIDs{}
	pending := make([]*auth.Account, 0, len(tokens))

	whoami := &patWhoAmIHydrator{proxyURL: req.ProxyURL}
	for i, at := range tokens {
		name := req.Name
		if name == "" {
			name = fmt.Sprintf("at-account-%d", i+1)
		} else if total > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i+1)
		}

		seed := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: at, allowDuplicate: req.AllowDuplicate, customHeaders: req.CustomHeaders})
		// Same as AddATAccount: codex_at does not go through OAuth identity dedup, see the comment above.
		if seed.accessTokenType != accessTokenTypeCodexAT && seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
			id, updated, newAcc, err := h.upsertOAuthIdentityAccountDeferred(ctx, name, req.ProxyURL, seed, "manual_at", overwriteAccountProxy)
			if err != nil {
				log.Printf("failed to add AT account %d: %v", i+1, err)
				failCount++
			} else if updated {
				// An existing account only has its credentials updated and is not counted as "added" (added should be 0 on a repeat add).
				updatedCount++
				log.Printf("AT account %d matched an existing identity and updated its credentials (id=%d)", i+1, id)
			} else {
				successCount++
				createdIDs.add(id)
				pending = append(pending, newAcc)
				log.Printf("AT account %d joined the account pool (id=%d)", i+1, id)
			}
			progress(i + 1)
			continue
		}

		if !req.AllowDuplicate || workspaceOverrideKnown {
			routeKey := credentialWorkspaceRouteKey("at", at, seed.customHeaders)
			if ownerID, exists := existingATRoutes[routeKey]; exists || seenATRoutes[routeKey] {
				// The same AT was added again while the old account is in error / 401 state: treat it as a request to revive it.
				if exists && h.reviveDuplicateRouteOwner(ctx, ownerID, "manual_at") {
					updatedCount++
				} else {
					duplicateCount++
				}
				progress(i + 1)
				continue
			}
			seenATRoutes[routeKey] = true
		}

		seed = whoami.hydrate(ctx, seed)
		id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), req.ProxyURL)
		if err != nil {
			log.Printf("failed to add AT account %d: %v", i+1, err)
			failCount++
			progress(i + 1)
			continue
		}

		successCount++
		createdIDs.add(id)
		newAcc := h.newCodexAccountFromSeed(id, req.ProxyURL, seed)
		pending = append(pending, newAcc)
		progress(i + 1)
	}
	h.db.BatchInsertAccountEventsAsync(createdIDs.snapshot(), "added", "manual_at")
	h.commitImportedRuntimeAccounts(pending, "manual_at", false)

	security.SecurityAuditLog("AT_ACCOUNTS_ADDED", fmt.Sprintf("success=%d updated=%d duplicate=%d failed=%d ip=%s", successCount, updatedCount, duplicateCount, failCount, c.ClientIP()))
	// Binding must finish before the complete event: the frontend refreshes the list as soon as it receives complete.
	if err := h.bindImportedAccountGroups(ctx, createdIDs.snapshot(), groupIDs); err != nil {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: total, Total: total,
			Success: successCount, Updated: updatedCount, Duplicate: duplicateCount, Failed: failCount,
			Warning: "accounts added, but group binding failed: " + err.Error(),
		})
	}
	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: successCount, Updated: updatedCount, Duplicate: duplicateCount, Failed: failCount,
	})
}

type updateAccountModelsRequest struct {
	Models []string `json:"models"`
}

// UpdateAccountModels sets the supported-model allowlist of an OAuth account.
// An empty array clears the allowlist and allows all models;
// when non-empty, the scheduler only routes requests for allowlisted models to this account.
func (h *Handler) UpdateAccountModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	var req updateAccountModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request format")
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	if len(models) > 200 {
		writeError(c, http.StatusBadRequest, "the number of models must not exceed 200")
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("invalid model name: %s", model))
			return
		}
	}

	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "account is not in the runtime pool")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{"models": models}); err != nil {
		writeInternalError(c, err)
		return
	}
	h.store.ApplyAccountModels(id, models)
	h.db.InsertAccountEventAsync(id, "updated", "account_models")
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// SyncAccountUpstreamModels fetches the upstream model list live using the account's own credentials,
// and returns the model slug list actually available to this account. The account allowlist itself is read-only and not persisted; it is saved by the admin after confirmation;
// however, models in the list that the registry does not yet know are learned into the registry along the way (add-only, never modified or deleted, same as the learning done when the client refreshes
// the picker menu; same implementation). Otherwise a model such as Trusted Access for Cyber, which only some accounts have,
// is visible to the probe, but after it is saved to the allowlist /v1/models does not list it and calls fail directly with model-not-found (issue #624).
func (h *Handler) SyncAccountUpstreamModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "account is not in the runtime pool")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	daybreakSnapshot := account.BeginDaybreakObservation()
	manifest, err := proxy.FetchCodexModelsManifest(ctx, account, h.store.ResolveProxyForAccount(account), "", "", nil)
	if err != nil {
		writeError(c, http.StatusBadGateway, fmt.Sprintf("failed to fetch upstream model list: %s", err.Error()))
		return
	}
	proxy.RecordResponsesLiteSupportFromManifest(manifest.Body)
	if err := manifest.SaveCapabilities(ctx, h.db); err != nil {
		writeError(c, http.StatusBadGateway, "failed to save Codex capability snapshot: "+err.Error())
		return
	}
	if err := (proxy.DaybreakObservation{Account: account, Snapshot: daybreakSnapshot, Body: manifest.Body}).Save(ctx, h.db); err != nil {
		writeError(c, http.StatusBadGateway, "failed to save Daybreak capability: "+err.Error())
		return
	}
	models := auth.NormalizeAccountModels(proxy.ExtractManifestModelSlugs(manifest.Body))
	if len(models) == 0 {
		writeError(c, http.StatusBadGateway, "upstream model list returned no available models")
		return
	}
	// A learning failure is only logged and does not affect the probe result being returned.
	if added, learnErr := proxy.LearnModelsFromManifest(ctx, h.db, manifest.Body, time.Now().UTC()); learnErr != nil {
		log.Printf("[account %d] model list learning failed (probe result not affected): %v", id, learnErr)
	} else if len(added) > 0 {
		log.Printf("[account %d] learned %d new models from the upstream model list into the registry: %s", id, len(added), strings.Join(added, ", "))
	}
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// importToken is the unified token carrier used during import
type importToken struct {
	refreshToken          string
	sessionToken          string
	accessToken           string // AT-only compatibility path
	name                  string
	email                 string
	idToken               string
	accountID             string
	chatgptAccountID      string // ChatGPT account unique identifier from export formats such as sub2api, used for precise dedup
	planType              string
	expiresAt             string
	codex7DUsedPercent    string
	codex7DResetAt        string
	codex5HUsedPercent    string
	codex5HResetAt        string
	codex5HUsageUpdatedAt string
	codexUsageUpdatedAt   string
	// Agent Identity (auth_mode=agentIdentity): no RT/ST/AT; dynamically signed with a private key.
	agentRuntimeID  string
	agentPrivateKey string
	agentTaskID     string
	chatgptUserID   string
	agentFedRAMP    bool
	// proxyURL is the proxy carried in the import file; it only takes effect when import_proxy is enabled.
	// It must never enter importTokenSeed or the dedup fingerprint: the same account with a different proxy is still the same account,
	// including it in the fingerprint would import it twice as two separate records.
	proxyURL     string
	proxyLabel   string
	proxyEnabled *bool
}

// importSettings collects the form switches of one import request. These switches have to pass through three layers: the entry point → 4 format
// parse functions → importAccountsCommon. Continuing to stack them as positional parameters would only get longer.
type importSettings struct {
	// defaultProxyURL is the proxy entered in the form: the fallback when the file carries no proxy or importProxies is not enabled.
	defaultProxyURL string
	// When importProxies is enabled, the proxies carried in the file are used and registered into the proxy pool.
	importProxies  bool
	allowDuplicate bool
	customHeaders  map[string]string
}

// proxyForToken returns the proxy that finally applies to this entry: the in-file proxy takes priority, falling back to the form value when missing.
func (s importSettings) proxyForToken(t importToken) string {
	if s.importProxies {
		if fromFile := strings.TrimSpace(t.proxyURL); fromFile != "" {
			return fromFile
		}
	}
	return strings.TrimSpace(s.defaultProxyURL)
}

// proxyOverwritePolicyForToken decides how the proxy binding is handled when an upsert matches an existing account.
// A proxy from the file is passive data and does not overwrite the binding already on the target side (it may already have been carefully allocated there);
// a proxy entered in the form is the operator's explicit intent to rebind, so the existing overwrite semantics are kept.
func (s importSettings) proxyOverwritePolicyForToken(t importToken) proxyOverwritePolicy {
	if s.importProxies && strings.TrimSpace(t.proxyURL) != "" {
		return preserveAccountProxy
	}
	return overwriteAccountProxy
}

func (t importToken) isAgentIdentity() bool {
	return strings.TrimSpace(t.agentRuntimeID) != "" && strings.TrimSpace(t.agentPrivateKey) != ""
}

// jsonAgentIdentityNode is the agent_identity sub-object in CLIProxyAPI/Sub2Api exports.
type jsonAgentIdentityNode struct {
	AgentRuntimeID  string `json:"agent_runtime_id"`
	AgentPrivateKey string `json:"agent_private_key"`
	TaskID          string `json:"task_id"`
	AccountID       string `json:"account_id"`
	ChatGPTUserID   string `json:"chatgpt_user_id"`
	Email           string `json:"email"`
	PlanType        string `json:"plan_type"`
	FedRAMP         bool   `json:"chatgpt_account_is_fedramp"`
}

// agentIdentityNodeFromFlatCredentials synthesizes an agent_identity node from the Agent Identity fields flattened into credentials
// (agent_identity node): the sub2api / codex2api account exports put these fields directly flat on
// the credentials object (auth_mode=agentIdentity + agent_runtime_id…), without a wrapping
// agent_identity sub-object. If the sub-object already exists, this function need not be called.
func agentIdentityNodeFromFlatCredentials(authMode, runtimeID, privateKey, taskID, accountID, userID, email, planType string, fedramp bool) *jsonAgentIdentityNode {
	runtimeID = strings.TrimSpace(runtimeID)
	// Only treat it as the flat Agent Identity form when auth_mode declares it or runtime_id is present.
	if !strings.EqualFold(strings.TrimSpace(authMode), auth.CodexAuthModeAgentIdentity) && runtimeID == "" {
		return nil
	}
	if runtimeID == "" || strings.TrimSpace(privateKey) == "" {
		return nil
	}
	return &jsonAgentIdentityNode{
		AgentRuntimeID:  runtimeID,
		AgentPrivateKey: strings.TrimSpace(privateKey),
		TaskID:          strings.TrimSpace(taskID),
		AccountID:       strings.TrimSpace(accountID),
		ChatGPTUserID:   strings.TrimSpace(userID),
		Email:           strings.TrimSpace(email),
		PlanType:        strings.TrimSpace(planType),
		FedRAMP:         fedramp,
	}
}

// agentIdentityImportTokenFromNode converts an agent_identity sub-object into an importToken (returns ok=false when there are no valid fields).
func agentIdentityImportTokenFromNode(node *jsonAgentIdentityNode, fallbackName string) (importToken, bool) {
	if node == nil {
		return importToken{}, false
	}
	runtimeID := strings.TrimSpace(node.AgentRuntimeID)
	privateKey := strings.TrimSpace(node.AgentPrivateKey)
	if runtimeID == "" || privateKey == "" {
		return importToken{}, false
	}
	email := strings.TrimSpace(node.Email)
	name := firstNonEmpty(fallbackName, email)
	return importToken{
		name:            name,
		email:           email,
		accountID:       strings.TrimSpace(node.AccountID),
		planType:        strings.TrimSpace(node.PlanType),
		agentRuntimeID:  runtimeID,
		agentPrivateKey: privateKey,
		agentTaskID:     strings.TrimSpace(node.TaskID),
		chatgptUserID:   strings.TrimSpace(node.ChatGPTUserID),
		agentFedRAMP:    node.FedRAMP,
	}, true
}

// jsonAccountEntry is a CLIProxyAPI credential JSON entry
type jsonAccountEntry struct {
	AuthMode              string                 `json:"auth_mode"`
	AgentIdentity         *jsonAgentIdentityNode `json:"agent_identity"`
	AgentRuntimeID        string                 `json:"agent_runtime_id"`
	AgentPrivateKey       string                 `json:"agent_private_key"`
	AgentTaskID           string                 `json:"task_id"`
	ChatGPTUserID         string                 `json:"chatgpt_user_id"`
	AgentFedRAMP          bool                   `json:"chatgpt_account_is_fedramp"`
	RefreshToken          string                 `json:"refresh_token"`
	SessionToken          string                 `json:"session_token"`
	SessionTokenCamel     string                 `json:"sessionToken"`
	AccessToken           string                 `json:"access_token"`
	AccessTokenCamel      string                 `json:"accessToken"`
	IDToken               string                 `json:"id_token"`
	IDTokenCamel          string                 `json:"idToken"`
	AccountID             string                 `json:"account_id"`
	ChatGPTAccountID      string                 `json:"chatgpt_account_id"`
	Email                 string                 `json:"email"`
	Name                  string                 `json:"name"`
	PlanType              string                 `json:"plan_type"`
	PlanTypeCamel         string                 `json:"planType"`
	User                  jsonAccountUser        `json:"user"`
	Account               jsonAccountAccount     `json:"account"`
	Expired               importJSONScalarString `json:"expired"`
	ExpiresAt             importJSONScalarString `json:"expires_at"`
	Expires               importJSONScalarString `json:"expires"`
	Codex7DUsedPercent    importJSONScalarString `json:"codex_7d_used_percent"`
	Codex7DResetAt        string                 `json:"codex_7d_reset_at"`
	Codex5HUsedPercent    importJSONScalarString `json:"codex_5h_used_percent"`
	Codex5HResetAt        string                 `json:"codex_5h_reset_at"`
	Codex5HUsageUpdatedAt string                 `json:"codex_5h_usage_updated_at"`
	CodexUsageUpdatedAt   string                 `json:"codex_usage_updated_at"`
	ProxyURL              string                 `json:"proxy_url"`
	ProxyLabel            string                 `json:"proxy_label"`
	ProxyEnabled          *bool                  `json:"proxy_enabled"`
}

type jsonAccountUser struct {
	Email string `json:"email"`
	ID    string `json:"id"`
	Name  string `json:"name"`
}

type jsonAccountAccount struct {
	PlanType      string `json:"plan_type"`
	PlanTypeCamel string `json:"planType"`
	ID            string `json:"id"`
}

type sub2apiImportPayload struct {
	Accounts []sub2apiAccountEntry `json:"accounts"`
}

type sub2apiAccountEntry struct {
	Name        string                    `json:"name"`
	Credentials sub2apiAccountCredentials `json:"credentials"`
	// A proxy is an account attribute, not a credential. Some export implementations write it on the entry root and others put it into
	// credentials; both places are accepted, with the root taking precedence.
	ProxyURL     string `json:"proxy_url"`
	ProxyLabel   string `json:"proxy_label"`
	ProxyEnabled *bool  `json:"proxy_enabled"`
}

// proxyFields returns the final proxy triple for this entry: the entry root takes priority, falling back to credentials.
// The URL decides which group is used, to avoid pairing a label written on the root with a URL taken from credentials.
func (a sub2apiAccountEntry) proxyFields() (string, string, *bool) {
	if url := strings.TrimSpace(a.ProxyURL); url != "" {
		return url, strings.TrimSpace(a.ProxyLabel), a.ProxyEnabled
	}
	return strings.TrimSpace(a.Credentials.ProxyURL), strings.TrimSpace(a.Credentials.ProxyLabel), a.Credentials.ProxyEnabled
}

type sub2apiAccountCredentials struct {
	AuthMode              string                 `json:"auth_mode"`
	AgentIdentity         *jsonAgentIdentityNode `json:"agent_identity"`
	AgentRuntimeID        string                 `json:"agent_runtime_id"`
	AgentPrivateKey       string                 `json:"agent_private_key"`
	AgentTaskID           string                 `json:"task_id"`
	ChatGPTUserID         string                 `json:"chatgpt_user_id"`
	AgentFedRAMP          bool                   `json:"chatgpt_account_is_fedramp"`
	RefreshToken          string                 `json:"refresh_token"`
	SessionToken          string                 `json:"session_token"`
	SessionTokenCamel     string                 `json:"sessionToken"`
	AccessToken           string                 `json:"access_token"`
	AccessTokenCamel      string                 `json:"accessToken"`
	IDToken               string                 `json:"id_token"`
	IDTokenCamel          string                 `json:"idToken"`
	AccountID             string                 `json:"account_id"`
	ChatGPTAccountID      string                 `json:"chatgpt_account_id"`
	Email                 string                 `json:"email"`
	PlanType              string                 `json:"plan_type"`
	PlanTypeCamel         string                 `json:"planType"`
	User                  jsonAccountUser        `json:"user"`
	Account               jsonAccountAccount     `json:"account"`
	ExpiresAt             importJSONScalarString `json:"expires_at"`
	Expired               importJSONScalarString `json:"expired"`
	Expires               importJSONScalarString `json:"expires"`
	Codex7DUsedPercent    importJSONScalarString `json:"codex_7d_used_percent"`
	Codex7DResetAt        string                 `json:"codex_7d_reset_at"`
	Codex5HUsedPercent    importJSONScalarString `json:"codex_5h_used_percent"`
	Codex5HResetAt        string                 `json:"codex_5h_reset_at"`
	Codex5HUsageUpdatedAt string                 `json:"codex_5h_usage_updated_at"`
	CodexUsageUpdatedAt   string                 `json:"codex_usage_updated_at"`
	ProxyURL              string                 `json:"proxy_url"`
	ProxyLabel            string                 `json:"proxy_label"`
	ProxyEnabled          *bool                  `json:"proxy_enabled"`
}

type importJSONScalarString string

func (v *importJSONScalarString) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var raw interface{}
	if err := decoder.Decode(&raw); err != nil {
		return err
	}

	switch value := raw.(type) {
	case string:
		*v = importJSONScalarString(strings.TrimSpace(value))
	case json.Number:
		*v = importJSONScalarString(value.String())
	case bool:
		*v = importJSONScalarString(strconv.FormatBool(value))
	default:
		*v = ""
	}

	return nil
}

func (v importJSONScalarString) String() string {
	return strings.TrimSpace(string(v))
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func trimUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, utf8BOM)
}

// parseImportJSONTokens accepts both the existing flat JSON and the top-level Sub2Api object.
func parseImportJSONTokens(data []byte) ([]importToken, error) {
	data = trimUTF8BOM(data)
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid import json")
	}

	if tokens := parseFlatJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	if tokens := parseSub2APIJSONImportTokens(data); len(tokens) > 0 {
		return tokens, nil
	}

	return nil, nil
}

// parseUploadedImportJSONFile opens the uploaded JSON file and parses it as a stream, without reading the whole file into
// memory. Large multipart files (>32MB) are stored in a temp file by ParseMultipartForm, and fh.Open()
// returns a reader that is fed straight into the streaming parser.
func parseUploadedImportJSONFile(fh *multipart.FileHeader) ([]importToken, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open file %s", fh.Filename)
	}
	defer f.Close()
	return parseImportJSONTokensStream(f)
}

// parseImportJSONTokensStream parses the import JSON as a stream, to avoid loading the whole file (up to 200MB)
// into memory and unmarshalling it at once. Array elements are decoded one by one, so peak memory drops to the size of a single account.
// It covers three shapes: a top-level array [ {...} ], a sub2api top-level object { "accounts": [ {...} ] },
// and a single flat object { ... }; when the shape cannot be determined while streaming, it falls back to the full parseImportJSONTokens.
func parseImportJSONTokensStream(r io.Reader) ([]importToken, error) {
	// json.Decoder does not strip the UTF-8 BOM, and some export files carry a BOM (aligned with the full-path
	// trimUTF8BOM handling): peek the first 3 bytes and discard them if they are a BOM.
	br := bufio.NewReader(r)
	if prefix, err := br.Peek(3); err == nil && prefix[0] == 0xef && prefix[1] == 0xbb && prefix[2] == 0xbf {
		_, _ = br.Discard(3)
	}
	dec := json.NewDecoder(br)
	first, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid import json")
	}
	delim, ok := first.(json.Delim)
	if !ok {
		return nil, fmt.Errorf("invalid import json")
	}

	switch delim {
	case '[':
		// Top-level array: decode jsonAccountEntry one by one.
		var tokens []importToken
		for dec.More() {
			var entry jsonAccountEntry
			if err := dec.Decode(&entry); err != nil {
				return nil, fmt.Errorf("invalid import json")
			}
			tokens = append(tokens, jsonAccountEntriesToTokens([]jsonAccountEntry{entry})...)
		}
		return tokens, nil

	case '{':
		// Top-level object: either a sub2api {accounts:[...]} or a single flat account object.
		// Walk the top-level fields; the accounts array is stream-decoded element by element, and the other fields (usually small) are collected
		// as RawMessage; if accounts is not seen after the walk, rebuild and parse them as a single flat object.
		var tokens []importToken
		sawAccounts := false
		other := map[string]json.RawMessage{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("invalid import json")
			}
			key, _ := keyTok.(string)
			if key == "accounts" {
				sawAccounts = true
				arrTok, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("invalid import json")
				}
				if d, ok := arrTok.(json.Delim); !ok || d != '[' {
					return nil, fmt.Errorf("invalid import json")
				}
				for dec.More() {
					var account sub2apiAccountEntry
					if err := dec.Decode(&account); err != nil {
						return nil, fmt.Errorf("invalid import json")
					}
					tokens = append(tokens, sub2apiAccountEntryToTokens(account)...)
				}
				if _, err := dec.Token(); err != nil { // consume ']'
					return nil, fmt.Errorf("invalid import json")
				}
				continue
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("invalid import json")
			}
			other[key] = raw
		}
		if sawAccounts {
			return tokens, nil
		}
		// No accounts field: this is a single flat account object; rebuild it from the collected fields and then
		// parse it as the flat form (a single object is small, so there is no memory pressure).
		objBytes, err := json.Marshal(other)
		if err != nil {
			return nil, fmt.Errorf("invalid import json")
		}
		return parseFlatJSONImportTokens(objBytes), nil
	}

	return nil, fmt.Errorf("invalid import json")
}

func parseFlatJSONImportTokens(data []byte) []importToken {
	var entries []jsonAccountEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return jsonAccountEntriesToTokens(entries)
	}

	var single jsonAccountEntry
	if err := json.Unmarshal(data, &single); err == nil {
		return jsonAccountEntriesToTokens([]jsonAccountEntry{single})
	}

	return nil
}

func jsonAccountEntriesToTokens(entries []jsonAccountEntry) []importToken {
	tokens := make([]importToken, 0, len(entries))
	for _, entry := range entries {
		rt := strings.TrimSpace(entry.RefreshToken)
		st := firstNonEmpty(entry.SessionToken, entry.SessionTokenCamel)
		at := firstNonEmpty(entry.AccessToken, entry.AccessTokenCamel)
		idTok := firstNonEmpty(entry.IDToken, entry.IDTokenCamel)
		email := firstNonEmpty(entry.Email, entry.User.Email)
		name := firstNonEmpty(entry.Name, entry.User.Name, email)
		planType := firstNonEmpty(entry.PlanType, entry.PlanTypeCamel, entry.Account.PlanType, entry.Account.PlanTypeCamel)
		accID := firstNonEmpty(entry.AccountID, entry.User.ID, entry.Account.ID)
		expiresAt := firstNonEmpty(entry.ExpiresAt.String(), entry.Expired.String(), entry.Expires.String())

		// Agent Identity entry: no RT/ST/AT, recognized separately. When the sub-object is missing, fall back to
		// the Agent Identity fields flattened on the entry root (the sub2api / codex2api export form).
		agentNode := entry.AgentIdentity
		if agentNode == nil {
			agentNode = agentIdentityNodeFromFlatCredentials(entry.AuthMode, entry.AgentRuntimeID, entry.AgentPrivateKey, entry.AgentTaskID, accID, entry.ChatGPTUserID, email, planType, entry.AgentFedRAMP)
		}
		if tok, ok := agentIdentityImportTokenFromNode(agentNode, name); ok {
			tok.proxyURL = strings.TrimSpace(entry.ProxyURL)
			tok.proxyLabel = strings.TrimSpace(entry.ProxyLabel)
			tok.proxyEnabled = entry.ProxyEnabled
			tokens = append(tokens, tok)
			continue
		}

		if rt != "" || st != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken:          rt,
				sessionToken:          st,
				accessToken:           at,
				name:                  name,
				email:                 email,
				idToken:               idTok,
				accountID:             strings.TrimSpace(entry.AccountID),
				chatgptAccountID:      firstNonEmpty(entry.ChatGPTAccountID, accID),
				planType:              planType,
				expiresAt:             expiresAt,
				codex7DUsedPercent:    strings.TrimSpace(entry.Codex7DUsedPercent.String()),
				codex7DResetAt:        strings.TrimSpace(entry.Codex7DResetAt),
				codex5HUsedPercent:    strings.TrimSpace(entry.Codex5HUsedPercent.String()),
				codex5HResetAt:        strings.TrimSpace(entry.Codex5HResetAt),
				codex5HUsageUpdatedAt: strings.TrimSpace(entry.Codex5HUsageUpdatedAt),
				codexUsageUpdatedAt:   strings.TrimSpace(entry.CodexUsageUpdatedAt),
				proxyURL:              strings.TrimSpace(entry.ProxyURL),
				proxyLabel:            strings.TrimSpace(entry.ProxyLabel),
				proxyEnabled:          entry.ProxyEnabled,
			})
		}
	}
	return tokens
}

func parseSub2APIJSONImportTokens(data []byte) []importToken {
	var payload sub2apiImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}

	tokens := make([]importToken, 0, len(payload.Accounts))
	for _, account := range payload.Accounts {
		tokens = append(tokens, sub2apiAccountEntryToTokens(account)...)
	}
	return tokens
}

// sub2apiAccountEntryToTokens converts a single sub2api account entry into importTokens (0 or 1).
// Extracted from parseSub2APIJSONImportTokens so the streaming parser can reuse it per element, avoiding two copies of the logic drifting apart.
func sub2apiAccountEntryToTokens(account sub2apiAccountEntry) []importToken {
	var tokens []importToken
	{
		c := account.Credentials
		rt := strings.TrimSpace(c.RefreshToken)
		st := firstNonEmpty(c.SessionToken, c.SessionTokenCamel)
		at := firstNonEmpty(c.AccessToken, c.AccessTokenCamel)
		idTok := firstNonEmpty(c.IDToken, c.IDTokenCamel)
		name := firstNonEmpty(account.Name, c.User.Name)
		email := firstNonEmpty(c.Email, c.User.Email)

		if name == "" {
			name = email
		}
		planType := firstNonEmpty(c.PlanType, c.PlanTypeCamel, c.Account.PlanType, c.Account.PlanTypeCamel)
		accID := firstNonEmpty(c.AccountID, c.User.ID, c.Account.ID)
		expiresAt := firstNonEmpty(c.ExpiresAt.String(), c.Expired.String(), c.Expires.String())
		proxyURL, proxyLabel, proxyEnabled := account.proxyFields()

		// Agent Identity entry: no RT/ST/AT, recognized separately. When the sub-object is missing, fall back to
		// the Agent Identity fields flattened into credentials (the sub2api export form).
		agentNode := c.AgentIdentity
		if agentNode == nil {
			agentNode = agentIdentityNodeFromFlatCredentials(c.AuthMode, c.AgentRuntimeID, c.AgentPrivateKey, c.AgentTaskID, accID, c.ChatGPTUserID, email, planType, c.AgentFedRAMP)
		}
		if tok, ok := agentIdentityImportTokenFromNode(agentNode, name); ok {
			tok.proxyURL = proxyURL
			tok.proxyLabel = proxyLabel
			tok.proxyEnabled = proxyEnabled
			return append(tokens, tok)
		}

		if rt != "" || st != "" || at != "" {
			tokens = append(tokens, importToken{
				refreshToken:          rt,
				sessionToken:          st,
				accessToken:           at,
				name:                  name,
				email:                 email,
				idToken:               idTok,
				accountID:             strings.TrimSpace(c.AccountID),
				chatgptAccountID:      firstNonEmpty(c.ChatGPTAccountID, accID),
				planType:              planType,
				expiresAt:             expiresAt,
				codex7DUsedPercent:    strings.TrimSpace(c.Codex7DUsedPercent.String()),
				codex7DResetAt:        strings.TrimSpace(c.Codex7DResetAt),
				codex5HUsedPercent:    strings.TrimSpace(c.Codex5HUsedPercent.String()),
				codex5HResetAt:        strings.TrimSpace(c.Codex5HResetAt),
				codex5HUsageUpdatedAt: strings.TrimSpace(c.Codex5HUsageUpdatedAt),
				codexUsageUpdatedAt:   strings.TrimSpace(c.CodexUsageUpdatedAt),
				proxyURL:              proxyURL,
				proxyLabel:            proxyLabel,
				proxyEnabled:          proxyEnabled,
			})
		}
	}

	return tokens
}

func importTokenCredentialIdentity(t importToken) string {
	switch {
	case t.refreshToken != "":
		return "rt:" + t.refreshToken
	case t.sessionToken != "":
		return "st:" + t.sessionToken
	case t.accessToken != "":
		return "at:" + t.accessToken
	default:
		return ""
	}
}

func importCredentialFingerprint(refreshToken, sessionToken, accessToken string) string {
	return strings.TrimSpace(refreshToken) + "\x00" + strings.TrimSpace(sessionToken) + "\x00" + strings.TrimSpace(accessToken)
}

func importTokenCredentialFingerprint(t importToken, conflicts map[string]bool) string {
	seed := importTokenSeed(t, conflicts)
	return importCredentialFingerprint(seed.refreshToken, seed.sessionToken, seed.accessToken)
}

func importTokenCredentialWorkspaceRouteKeys(t importToken, conflicts map[string]bool, customHeaders map[string]string) []string {
	seed := importTokenSeed(t, conflicts)
	seed.customHeaders = cloneCustomHeaders(customHeaders)
	return tokenCredentialSeedWorkspaceRouteKeys(seed)
}

func importAccountCredentialFingerprint(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	return importCredentialFingerprint(
		row.GetCredential("refresh_token"),
		row.GetCredential("session_token"),
		row.GetCredential("access_token"),
	)
}

func conflictingImportChatGPTIDs(tokens []importToken) map[string]bool {
	identitiesByID := make(map[string]map[string]struct{})
	for _, t := range tokens {
		id := strings.TrimSpace(t.chatgptAccountID)
		if id == "" {
			continue
		}
		identity := importTokenCredentialIdentity(t)
		if identity == "" {
			continue
		}
		identities := identitiesByID[id]
		if identities == nil {
			identities = make(map[string]struct{}, 1)
			identitiesByID[id] = identities
		}
		identities[identity] = struct{}{}
	}

	conflicts := make(map[string]bool)
	for id, identities := range identitiesByID {
		if len(identities) > 1 {
			conflicts[id] = true
		}
	}
	return conflicts
}

func reliableImportChatGPTID(t importToken, conflicts map[string]bool) string {
	id := strings.TrimSpace(t.chatgptAccountID)
	if id == "" || conflicts[id] {
		return ""
	}
	return id
}

func importStoredAccountID(t importToken, conflicts map[string]bool) string {
	if strings.TrimSpace(t.accountID) != "" {
		return strings.TrimSpace(t.accountID)
	}
	return reliableImportChatGPTID(t, conflicts)
}

func importTokenSeed(t importToken, conflicts map[string]bool) tokenCredentialSeed {
	return normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken:          t.refreshToken,
		sessionToken:          t.sessionToken,
		accessToken:           t.accessToken,
		idToken:               t.idToken,
		accountID:             importStoredAccountID(t, conflicts),
		email:                 t.email,
		planType:              t.planType,
		expiresAtRaw:          t.expiresAt,
		codex7DUsedPercent:    t.codex7DUsedPercent,
		codex7DResetAt:        t.codex7DResetAt,
		codex5HUsedPercent:    t.codex5HUsedPercent,
		codex5HResetAt:        t.codex5HResetAt,
		codex5HUsageUpdatedAt: t.codex5HUsageUpdatedAt,
		codexUsageUpdatedAt:   t.codexUsageUpdatedAt,
	})
}

func importTokenOAuthIdentityKey(t importToken, conflicts map[string]bool) string {
	seed := importTokenSeed(t, conflicts)
	email := strings.ToLower(strings.TrimSpace(seed.email))
	workspaceID := strings.TrimSpace(seed.workspaceID)
	if email == "" || workspaceID == "" {
		return ""
	}
	return email + "\x00" + workspaceID
}

// ImportAccounts batch-imports accounts (TXT / JSON supported)
func (h *Handler) ImportAccounts(c *gin.Context) {
	format := c.DefaultPostForm("format", "txt")
	customHeaders, err := parseCustomHeadersForm(c.PostForm("custom_headers"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	settings := importSettings{
		defaultProxyURL: c.PostForm("proxy_url"),
		// TXT-style formats have one token per line and physically cannot carry a proxy. Here the switch is ignored rather than
		// raising an error, so the frontend does not have to clear that state whenever the format changes.
		importProxies:  parseBoolForm(c.PostForm("import_proxy")) && (format == "json" || format == "json_at"),
		allowDuplicate: parseBoolForm(c.PostForm("allow_duplicate")),
		customHeaders:  customHeaders,
	}
	// Group validation runs before the file is parsed: if a group ID is wrong, no account should be imported.
	groupCtx, groupCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	groupIDs, err := h.resolveImportGroupIDsForm(groupCtx, c.PostForm(importGroupIDsField))
	groupCancel()
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Set(importGroupIDsContextKey, groupIDs)

	switch format {
	case "json":
		h.importAccountsJSON(c, settings)
	case "json_at":
		h.importAccountsJSONPreferAT(c, settings)
	case "at_txt":
		h.importAccountsATTXT(c, settings)
	default:
		h.importAccountsTXT(c, settings)
	}
}

// parseBoolForm parses a boolean switch in the form (1/true/yes/on count as true).
func parseBoolForm(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func parseCustomHeadersForm(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(trimmed), &headers); err != nil {
		return nil, fmt.Errorf("custom_headers must be a JSON object")
	}
	return normalizeCustomHeaders(headers)
}

type uploadedImportFile struct {
	name string
	data []byte
}

func readUploadedImportFiles(c *gin.Context) ([]uploadedImportFile, error) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, fmt.Errorf("failed to parse form")
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		return nil, fmt.Errorf("please upload a file (field name: file)")
	}

	result := make([]uploadedImportFile, 0, len(files))
	for _, fh := range files {
		if err := validateImportFileSize(fh); err != nil {
			return nil, err
		}

		f, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open file %s", fh.Filename)
		}
		data, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("failed to read file %s", fh.Filename)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("failed to close file %s", fh.Filename)
		}

		result = append(result, uploadedImportFile{name: fh.Filename, data: data})
	}
	return result, nil
}

func importTokensFromTextFiles(files []uploadedImportFile, makeToken func(string) importToken) []importToken {
	seen := make(map[string]bool)
	var tokens []importToken
	for _, file := range files {
		lines := strings.Split(string(trimUTF8BOM(file.data)), "\n")
		for _, line := range lines {
			t := strings.TrimSpace(line)
			if t != "" && !seen[t] {
				seen[t] = true
				tokens = append(tokens, makeToken(t))
			}
		}
	}
	return tokens
}

// importAccountsTXT imports from a TXT file (one RT per line)
func (h *Handler) importAccountsTXT(c *gin.Context, settings importSettings) {
	files, err := readUploadedImportFiles(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	tokens := importTokensFromTextFiles(files, func(token string) importToken {
		return importToken{refreshToken: token}
	})
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "no valid Refresh Token found in file")
		return
	}

	h.importAccountsCommon(c, tokens, settings)
}

// importAccountsJSON imports from JSON files (compatible with the CLIProxyAPI credential format)
func (h *Handler) importAccountsJSON(c *gin.Context, settings importSettings) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse form")
		return
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(c, http.StatusBadRequest, "please upload at least one JSON file")
		return
	}

	var allTokens []importToken

	for _, fh := range files {
		if err := validateImportFileSize(fh); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}

		tokens, err := parseUploadedImportJSONFile(fh)
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("file %s is not valid JSON", fh.Filename))
			return
		}

		allTokens = append(allTokens, tokens...)
	}

	if len(allTokens) == 0 {
		writeError(c, http.StatusBadRequest, "no valid refresh_token or access_token found in JSON file")
		return
	}

	h.importAccountsCommon(c, allTokens, settings)
}

// importAccountsJSONPreferAT imports from JSON files but only trusts access_token,
// for cases where refresh_token / session_token in some export tools are placeholder or duplicate values.
func (h *Handler) importAccountsJSONPreferAT(c *gin.Context, settings importSettings) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse form")
		return
	}

	files := c.Request.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(c, http.StatusBadRequest, "please upload at least one JSON file")
		return
	}

	var allTokens []importToken

	for _, fh := range files {
		if err := validateImportFileSize(fh); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}

		tokens, err := parseUploadedImportJSONFile(fh)
		if err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("file %s is not valid JSON", fh.Filename))
			return
		}

		for _, t := range tokens {
			if strings.TrimSpace(t.accessToken) == "" {
				continue
			}
			t.refreshToken = ""
			t.sessionToken = ""
			allTokens = append(allTokens, t)
		}
	}

	if len(allTokens) == 0 {
		writeError(c, http.StatusBadRequest, "no valid access_token found in JSON file")
		return
	}

	h.importAccountsCommon(c, allTokens, settings)
}

// importEvent is an SSE import progress event
type importEvent struct {
	Type      string `json:"type"` // progress | complete
	Current   int    `json:"current"`
	Total     int    `json:"total"`
	Success   int    `json:"success"`
	Updated   int    `json:"updated"`
	Duplicate int    `json:"duplicate"`
	Failed    int    `json:"failed"`
	// Warning is for cases like "the account was stored, but a follow-up step failed": they must be reported but should not be treated as a failure,
	// for example, the import succeeded but group binding failed. An empty value is omitted when serialized, so older frontends are not affected.
	Warning string `json:"warning,omitempty"`
	// CreatedIDs is only sent with the complete event, so the frontend can fetch the invite-earnings evaluation for the accounts imported this time.
	// Omitted when empty, so older frontends are not affected.
	CreatedIDs []int64 `json:"created_ids,omitempty"`
	// Proxy registration result, non-zero only when "proxies in the import file" is enabled. Also omitempty.
	ProxiesImported int `json:"proxies_imported,omitempty"`
	ProxiesSkipped  int `json:"proxies_skipped,omitempty"`
}

// sendImportEvent pushes one import progress event; it returns false when the downstream connection can no longer be written to.
func sendImportEvent(c *gin.Context, e importEvent) bool {
	return sendSSEJSON(c, e)
}

// importProgressInterval is the push interval for import progress events.
const importProgressInterval = 200 * time.Millisecond

// runImportProgressPusher pushes import progress at a fixed interval and returns a channel that is closed once the push goroutine exits.
// It stops in three cases: the import finishes (done is closed), the downstream disconnects (reqCtx is cancelled), or a write
// fails. The last two must stop: the import itself keeps running to the end, but the connection is already dead, and continuing to write would log a broken pipe every
// interval until the import ends.
//
// The caller must wait for the returned channel to close after close(done) before writing the final event: gin's
// ResponseWriter does not support concurrent writes; writing from both sides would interleave events, the frontend would fail to parse complete,
// and the progress bar would stay stuck at the last percentage.
func runImportProgressPusher(reqCtx context.Context, done <-chan struct{}, interval time.Duration, snapshot func() importEvent, send func(importEvent) bool) <-chan struct{} {
	stopped := make(chan struct{})
	var clientGone <-chan struct{}
	if reqCtx != nil {
		clientGone = reqCtx.Done()
	}
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !send(snapshot()) {
					return
				}
			case <-clientGone:
				return
			case <-done:
				return
			}
		}
	}()
	return stopped
}

func setupSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

func sendSSEJSON(c *gin.Context, event any) bool {
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("failed to marshal SSE event: %v", err)
		return false
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		log.Printf("failed to write SSE event: %v", err)
		return false
	}
	c.Writer.Flush()
	return true
}

// importAccountsCommon is the shared dedup, concurrent insert and SSE progress push logic (supports mixed RT and AT-only imports)
func (h *Handler) importAccountsCommon(c *gin.Context, tokens []importToken, settings importSettings) {
	importCustomHeaders := settings.customHeaders
	allowDuplicate := settings.allowDuplicate

	// Proxy registration must run before any account is stored, including the Agent Identity branch below.
	// registerImportedProxies normalizes the proxy URLs on tokens in place; if it fails, the whole import
	// is aborted: continuing to write would only produce a batch of accounts bound to proxies that are not in the pool, and therefore not schedulable.
	// SSE has not started here (setupSSE runs after dedup), so a normal HTTP error can be returned.
	var proxyOutcome importProxyOutcome
	if settings.importProxies {
		proxyCtx, proxyCancel := context.WithTimeout(context.Background(), 15*time.Second)
		outcome, err := h.registerImportedProxies(proxyCtx, tokens)
		proxyCancel()
		if err != nil {
			writeError(c, http.StatusInternalServerError, "failed to import proxies, no accounts were written: "+err.Error())
			return
		}
		proxyOutcome = outcome
	}

	// Agent Identity entries are handled separately (no RT/ST/AT, deduplicated by runtime_id, dynamically signed),
	// split out of the regular token stream; their counts are merged into the total response at the end.
	var agentTokens, regularTokens []importToken
	for _, t := range tokens {
		if t.isAgentIdentity() {
			agentTokens = append(agentTokens, t)
		} else {
			regularTokens = append(regularTokens, t)
		}
	}
	agentSuccess, agentDuplicate, agentFailed := 0, 0, 0
	var agentCreatedIDs []int64
	if len(agentTokens) > 0 {
		agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Second)
		agentSuccess, agentDuplicate, agentFailed, agentCreatedIDs = h.importAgentIdentityTokens(agentCtx, agentTokens, settings)
		agentCancel()
		log.Printf("import: %d Agent Identity entries (added %d, skipped %d, failed %d)", len(agentTokens), agentSuccess, agentDuplicate, agentFailed)
	}
	tokens = regularTokens
	// Dedup within the file:
	// 1) When email + workspace_id can be parsed from the JWT, use them as the OAuth identity key;
	//    the same identity with the same RT/ST/AT is collapsed; the same identity with different RT/ST/AT is skipped as a whole group, to avoid arbitrarily picking one to overwrite.
	// 2) Without an OAuth identity, fall back to deduplicating by RT / ST / AT in that order (compatible with older export formats).
	// 3) If the same file contains "one RT mapped to several different chatgpt_account_id values",
	//    they are all kept as independent accounts; refresh_token has no UNIQUE constraint at the database level, so this is safe.
	conflictingChatGPTIDs := conflictingImportChatGPTIDs(tokens)
	type oauthIdentityImportState struct {
		count        int
		fingerprints map[string]struct{}
	}
	oauthIdentityStates := make(map[string]*oauthIdentityImportState)
	for _, t := range tokens {
		oauthIdentity := importTokenOAuthIdentityKey(t, conflictingChatGPTIDs)
		if oauthIdentity == "" {
			continue
		}
		state := oauthIdentityStates[oauthIdentity]
		if state == nil {
			state = &oauthIdentityImportState{fingerprints: make(map[string]struct{}, 1)}
			oauthIdentityStates[oauthIdentity] = state
		}
		state.count++
		state.fingerprints[importTokenCredentialFingerprint(t, conflictingChatGPTIDs)] = struct{}{}
	}

	seenOAuthIdentity := make(map[string]bool)
	seenRT := make(map[string]bool)
	seenST := make(map[string]bool)
	seenAT := make(map[string]bool)
	var unique []importToken
	ambiguousOAuthIdentityCount := 0
	for _, t := range tokens {
		oauthIdentity := importTokenOAuthIdentityKey(t, conflictingChatGPTIDs)
		if oauthIdentity != "" {
			state := oauthIdentityStates[oauthIdentity]
			if state != nil && len(state.fingerprints) > 1 {
				if !seenOAuthIdentity[oauthIdentity] {
					ambiguousOAuthIdentityCount += state.count
					seenOAuthIdentity[oauthIdentity] = true
				}
				continue
			}
			if seenOAuthIdentity[oauthIdentity] {
				continue
			}
			seenOAuthIdentity[oauthIdentity] = true
			if t.refreshToken != "" {
				seenRT[t.refreshToken] = true
			}
			if t.sessionToken != "" {
				seenST[t.sessionToken] = true
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
			unique = append(unique, t)
			continue
		}
		if t.refreshToken != "" {
			if !seenRT[t.refreshToken] {
				seenRT[t.refreshToken] = true
				unique = append(unique, t)
			}
			if t.sessionToken != "" {
				seenST[t.sessionToken] = true
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
		} else if t.sessionToken != "" {
			if !seenST[t.sessionToken] {
				seenST[t.sessionToken] = true
				unique = append(unique, t)
			}
			if t.accessToken != "" {
				seenAT[t.accessToken] = true
			}
		} else if t.accessToken != "" {
			if !seenAT[t.accessToken] {
				seenAT[t.accessToken] = true
				unique = append(unique, t)
			}
		}
	}

	// Database dedup (separate short timeout)
	dedupeCtx, dedupeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dedupeCancel()

	fileDuplicateCount := len(tokens) - len(unique) - ambiguousOAuthIdentityCount
	if fileDuplicateCount < 0 {
		fileDuplicateCount = 0
	}
	log.Printf("import parse: %d entries in file, %d after dedup (%d duplicates within file, %d skipped due to OAuth identity conflicts)", len(tokens), len(unique), fileDuplicateCount, ambiguousOAuthIdentityCount)

	var newTokens []importToken
	duplicateCount := ambiguousOAuthIdentityCount

	// When it matches an existing account whose credentials are identical word for word: an account in normal state is counted as a duplicate and skipped; one stuck in
	// error / 401 unauthorized state goes into the revival queue, is processed later together with new accounts, and is counted as
	// "updated": re-importing the same credentials is a request to bring this account back (issue #618).
	// The queue only reads state during dedup and does not write to the database; the revival action runs later behind the concurrency gate.
	var reviveRows []*database.AccountRow
	queuedReviveIDs := make(map[int64]bool)
	queueDuplicate := func(row *database.AccountRow) {
		if row != nil && accountErrorStateNeedsReset(row) && !queuedReviveIDs[row.ID] {
			queuedReviveIDs[row.ID] = true
			reviveRows = append(reviveRows, row)
			return
		}
		duplicateCount++
	}

	workspaceOverrideKnown := openaiidentity.WorkspaceOverrideFromHeaders(importCustomHeaders) != ""
	if allowDuplicate && !workspaceOverrideKnown {
		knownCount := 0
		for _, t := range tokens {
			if importTokenOAuthIdentityKey(t, conflictingChatGPTIDs) == "" {
				newTokens = append(newTokens, t)
			} else {
				knownCount++
			}
		}
		knownUniqueCount := 0
		for _, t := range unique {
			if importTokenOAuthIdentityKey(t, conflictingChatGPTIDs) != "" {
				knownUniqueCount++
				seed := importTokenSeed(t, conflictingChatGPTIDs)
				seed.customHeaders = cloneCustomHeaders(importCustomHeaders)
				if duplicateID, err := h.findOAuthIdentityDuplicate(dedupeCtx, seed, 0); err != nil {
					log.Printf("failed to query existing OAuth identity: %v", err)
				} else if duplicateID > 0 {
					row, err := h.db.GetAccountByID(dedupeCtx, duplicateID)
					if err != nil {
						log.Printf("failed to query existing OAuth account %d: %v", duplicateID, err)
					} else if importAccountCredentialFingerprint(row) == importTokenCredentialFingerprint(t, conflictingChatGPTIDs) {
						queueDuplicate(row)
						continue
					}
				}
				newTokens = append(newTokens, t)
			}
		}
		duplicateCount += knownCount - knownUniqueCount - ambiguousOAuthIdentityCount
	} else {
		// Routing key → owner account ID; keys newly claimed in this batch are recorded as 0 (no owner, used only for in-batch dedup).
		existingCredentialRoutes, err := h.existingCredentialWorkspaceRouteOwners(dedupeCtx)
		if err != nil {
			log.Printf("failed to query existing credential workspace route: %v", err)
			existingCredentialRoutes = make(map[string]int64)
		}

		for _, t := range unique {
			seed := importTokenSeed(t, conflictingChatGPTIDs)
			seed.customHeaders = cloneCustomHeaders(importCustomHeaders)
			if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
				if duplicateID, err := h.findOAuthIdentityDuplicate(dedupeCtx, seed, 0); err != nil {
					log.Printf("failed to query existing OAuth identity: %v", err)
				} else if duplicateID > 0 {
					row, err := h.db.GetAccountByID(dedupeCtx, duplicateID)
					if err != nil {
						log.Printf("failed to query existing OAuth account %d: %v", duplicateID, err)
					} else if importAccountCredentialFingerprint(row) == importTokenCredentialFingerprint(t, conflictingChatGPTIDs) {
						queueDuplicate(row)
						continue
					}
				}
				newTokens = append(newTokens, t)
				continue
			}

			routeKeys := importTokenCredentialWorkspaceRouteKeys(t, conflictingChatGPTIDs, importCustomHeaders)
			isDuplicate := false
			var ownerID int64
			for _, key := range routeKeys {
				if id, exists := existingCredentialRoutes[key]; exists {
					isDuplicate = true
					ownerID = id
					break
				}
			}
			if isDuplicate {
				var ownerRow *database.AccountRow
				if ownerID > 0 {
					if row, err := h.db.GetAccountByID(dedupeCtx, ownerID); err != nil {
						log.Printf("failed to query existing credential account %d: %v", ownerID, err)
					} else {
						ownerRow = row
					}
				}
				queueDuplicate(ownerRow)
				continue
			}
			newTokens = append(newTokens, t)
			for _, key := range routeKeys {
				existingCredentialRoutes[key] = 0
			}
		}
	}

	total := len(unique) + ambiguousOAuthIdentityCount + len(agentTokens)
	if allowDuplicate && !workspaceOverrideKnown {
		total = len(tokens) + len(agentTokens)
	}
	duplicateCount += agentDuplicate

	log.Printf("import dedup: total %d, already in database %d, pending revival %d, pending import %d", total, duplicateCount, len(reviveRows), len(newTokens))

	if len(newTokens) == 0 && len(reviveRows) == 0 {
		// No regular tokens to import (possibly a pure Agent Identity file); reflects the agent counts.
		if err := h.bindImportedAccountGroups(c.Request.Context(), agentCreatedIDs, importGroupIDsFromContext(c)); err != nil {
			log.Printf("import: Agent Identity account group binding failed: %v", err)
		}
		response := gin.H{
			"message":   fmt.Sprintf("import complete: %d added, %d skipped, %d failed", agentSuccess, duplicateCount, agentFailed),
			"success":   agentSuccess,
			"duplicate": duplicateCount,
			"failed":    agentFailed,
			"total":     total,
		}
		if settings.importProxies {
			response["proxies_imported"] = proxyOutcome.inserted
			response["proxies_skipped"] = proxyOutcome.skipped
			if warning := proxyOutcome.warning(); warning != "" {
				response["warning"] = warning
			}
		}
		c.JSON(http.StatusOK, response)
		return
	}

	// Switch to an SSE streaming response
	setupSSE(c)

	var successCount int64
	var updatedCount int64
	var failCount int64
	var current int64
	// Accounts actually created this time are bound to groups in one go at the end (groups of matched existing accounts are left unchanged).
	createdIDs := &importedAccountIDs{}
	createdATIDs := &importedAccountIDs{}
	createdRTIDs := &importedAccountIDs{}
	type pendingRuntimeAccount struct {
		account *auth.Account
		source  string
	}
	var pendingMu sync.Mutex
	pendingRuntime := make([]pendingRuntimeAccount, 0, len(newTokens))
	addPendingRuntime := func(account *auth.Account, source string) {
		if account == nil {
			return
		}
		pendingMu.Lock()
		pendingRuntime = append(pendingRuntime, pendingRuntimeAccount{account: account, source: source})
		pendingMu.Unlock()
	}
	// Database write concurrency is adjusted dynamically based on proxy traffic over the last minute and connection pool pressure. The producer acquires a permit
	// before starting a goroutine, so waiting goroutines never pile up no matter how the target concurrency changes.
	dbLimiter := &adaptiveImportDBLimiter{handler: h}
	var wg sync.WaitGroup

	// Progress push goroutine: sends on a timer, to avoid an IO bottleneck from writing on every entry.
	done := make(chan struct{})
	progressStopped := runImportProgressPusher(
		c.Request.Context(), done, importProgressInterval,
		func() importEvent {
			return importEvent{
				Type:      "progress",
				Current:   int(atomic.LoadInt64(&current)) + duplicateCount,
				Total:     total,
				Success:   int(atomic.LoadInt64(&successCount)),
				Updated:   int(atomic.LoadInt64(&updatedCount)),
				Failed:    int(atomic.LoadInt64(&failCount)),
				Duplicate: duplicateCount,
			}
		},
		func(e importEvent) bool { return sendImportEvent(c, e) },
	)

	// Revival queue: duplicate entries whose credentials are identical word for word but whose old account is stuck in error / 401 state. They go through the same
	// write concurrency gate; a successful revival counts as "updated", and if another flow has already cleared the state, it falls back to "duplicate".
	// duplicateCount is read concurrently by the progress push goroutine, so it can only be changed through an atomic counter here.
	var revivedAsDuplicate int64
	for _, row := range reviveRows {
		if !dbLimiter.acquire(context.Background()) {
			break
		}
		wg.Add(1)
		go func(row *database.AccountRow) {
			defer wg.Done()
			defer dbLimiter.release()
			reviveCtx, reviveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer reviveCancel()
			if h.reviveReimportedAccount(reviveCtx, row, "import") {
				atomic.AddInt64(&updatedCount, 1)
			} else {
				atomic.AddInt64(&revivedAsDuplicate, 1)
			}
			atomic.AddInt64(&current, 1)
		}(row)
	}

	for i, t := range newTokens {
		if !dbLimiter.acquire(context.Background()) {
			break
		}
		wg.Add(1)
		go func(idx int, tok importToken) {
			defer wg.Done()
			defer dbLimiter.release()

			name := tok.name
			// The in-file proxy takes priority and the form proxy is the fallback; registerImportedProxies has already cleared invalid values
			// so the URL obtained here is always a validated one.
			proxyURL := settings.proxyForToken(tok)

			seed := importTokenSeed(tok, conflictingChatGPTIDs)
			seed.allowDuplicate = allowDuplicate
			seed.customHeaders = cloneCustomHeaders(importCustomHeaders)
			importSource := "import"
			if tok.accessToken != "" && tok.refreshToken == "" {
				importSource = "import_at"
			}
			if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
				if name == "" {
					if importSource == "import_at" {
						name = fmt.Sprintf("at-import-%d", idx+1)
					} else {
						name = fmt.Sprintf("import-%d", idx+1)
					}
				}

				upsertCtx, upsertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, updated, newAcc, err := h.upsertOAuthIdentityAccountDeferred(upsertCtx, name, proxyURL, seed, importSource, settings.proxyOverwritePolicyForToken(tok))
				upsertCancel()
				if err != nil {
					log.Printf("failed to update or write imported account %d/%d: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				if updated {
					// An existing account only has its credentials updated; it is not counted as "added", and its groups stay as they are.
					atomic.AddInt64(&updatedCount, 1)
					if h.store != nil {
						if acc := h.store.FindByID(id); acc != nil {
							h.applyImportedAccountUsageState(acc, importSource)
							if acc.GetAccessToken() == "" && !h.store.GetLazyMode() {
								h.runImportProbeTask(func(ctx context.Context) {
									h.refreshImportedAccountAndProbe(ctx, id, importSource+"_refresh")
								})
							}
						}
					}
				} else {
					atomic.AddInt64(&successCount, 1)
					createdIDs.add(id)
					if importSource == "import_at" {
						createdATIDs.add(id)
					} else {
						createdRTIDs.add(id)
					}
					addPendingRuntime(newAcc, importSource)
				}
				atomic.AddInt64(&current, 1)
				return
			}

			if tok.accessToken != "" && tok.refreshToken == "" {
				// AT-only import path
				if name == "" {
					name = fmt.Sprintf("at-import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, err := h.db.InsertAccountWithCredentials(insertCtx, name, h.newCodexAccountCredentials(seed), proxyURL)
				insertCancel()

				if err != nil {
					log.Printf("failed to import AT account %d/%d: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				atomic.AddInt64(&successCount, 1)
				createdIDs.add(id)
				createdATIDs.add(id)
				atomic.AddInt64(&current, 1)

				newAcc := h.newCodexAccountFromSeed(id, proxyURL, seed)
				addPendingRuntime(newAcc, "import_at")
			} else {
				// RT import path; if the import file also carries an AT, that AT is used first and refreshed by background scheduling before it expires.
				if name == "" {
					name = fmt.Sprintf("import-%d", idx+1)
				}

				insertCtx, insertCancel := context.WithTimeout(context.Background(), 5*time.Second)
				id, err := h.db.InsertAccountWithCredentials(insertCtx, name, h.newCodexAccountCredentials(seed), proxyURL)
				insertCancel()

				if err != nil {
					log.Printf("failed to import account %d/%d: %v", idx+1, len(newTokens), err)
					atomic.AddInt64(&failCount, 1)
					atomic.AddInt64(&current, 1)
					return
				}

				atomic.AddInt64(&successCount, 1)
				createdIDs.add(id)
				createdRTIDs.add(id)
				atomic.AddInt64(&current, 1)

				newAcc := h.newCodexAccountFromSeed(id, proxyURL, seed)
				addPendingRuntime(newAcc, "import")
			}
		}(i, t)
	}

	wg.Wait()
	h.db.BatchInsertAccountEventsAsync(createdATIDs.snapshot(), "added", "import_at")
	h.db.BatchInsertAccountEventsAsync(createdRTIDs.snapshot(), "added", "import")
	if len(pendingRuntime) > 0 && h.store != nil {
		accounts := make([]*auth.Account, 0, len(pendingRuntime))
		for _, pending := range pendingRuntime {
			accounts = append(accounts, pending.account)
		}
		// One Store lock plus one FastScheduler lock commit the whole batch, to avoid high-RPM Acquire
		// repeatedly triggering a full-bucket sort between two accounts.
		h.store.AddAccounts(accounts)
		for _, pending := range pendingRuntime {
			h.applyImportedAccountUsageState(pending.account, pending.source)
			h.scheduleImportedAccountWarmup(pending.account, pending.account.DBID, pending.source)
		}
	}
	close(done)
	// Wait for the push goroutine to actually exit before writing the final event: gin's ResponseWriter does not support concurrent writes,
	// if we only close(done) without waiting, the final event may interleave with the last progress frame,
	// the frontend would fail to parse complete, and the progress bar would stay stuck at the last percentage.
	<-progressStopped
	// The push goroutine has exited, so it is safe to merge in the duplicate count from failed revivals.
	duplicateCount += int(atomic.LoadInt64(&revivedAsDuplicate))

	// Send the complete event (Agent Identity counts merged in)
	suc := int(atomic.LoadInt64(&successCount)) + agentSuccess
	upd := int(atomic.LoadInt64(&updatedCount))
	fai := int(atomic.LoadInt64(&failCount)) + agentFailed
	// Group binding must finish before complete: the frontend refreshes the list as soon as it receives complete,
	// binding later would make it look as if nothing took effect. Agent Identity entries are bound together, so that one import does not
	// end up with only half of its accounts in the group.
	newAccountIDs := append(createdIDs.snapshot(), agentCreatedIDs...)
	if err := h.bindImportedAccountGroups(c.Request.Context(), newAccountIDs, importGroupIDsFromContext(c)); err != nil {
		sendImportEvent(c, importEvent{
			Type: "progress", Current: total, Total: total,
			Success: suc, Updated: upd, Duplicate: duplicateCount, Failed: fai,
			Warning: "accounts imported, but group binding failed: " + err.Error(),
		})
	}
	// The invite eligibility probe is queued before complete but does not wait for its result: the probe runs on the background worker behind the import gate,
	// and the frontend polls the plan endpoint itself after receiving created_ids. Blocking here would stretch one import's
	// response out to tens of seconds.
	h.scheduleInviteGuideProbes(c.Request.Context(), newAccountIDs)

	sendImportEvent(c, importEvent{
		Type: "complete", Current: total, Total: total,
		Success: suc, Updated: upd, Duplicate: duplicateCount, Failed: fai,
		CreatedIDs:      newAccountIDs,
		ProxiesImported: proxyOutcome.inserted,
		ProxiesSkipped:  proxyOutcome.skipped,
		Warning:         proxyOutcome.warning(),
	})

	log.Printf("import complete: success=%d, updated=%d, duplicate=%d, failed=%d, total=%d", suc, upd, duplicateCount, fai, total)
}

// importAccountsATTXT imports AT-only accounts from a TXT file (one Access Token per line)
func (h *Handler) importAccountsATTXT(c *gin.Context, settings importSettings) {
	files, err := readUploadedImportFiles(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	tokens := importTokensFromTextFiles(files, func(token string) importToken {
		return importToken{accessToken: token}
	})
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "no valid Access Token found in file")
		return
	}

	h.importAccountsCommon(c, tokens, settings)
}

// GetAccountUsage queries the usage statistics of a single account
func (h *Handler) GetAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	days := 30
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 3650 {
			writeError(c, http.StatusBadRequest, "invalid days parameter, an integer from 0 to 3650 is required")
			return
		}
		days = parsed
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	detail, err := h.db.GetAccountUsageStats(ctx, id, days)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// RefreshAccountUsage synchronously refreshes the usage snapshot of a single account (preferring the zero-cost wham endpoint),
// and after it finishes returns the account's latest 5h/7d usage fields, so the frontend usage column can update its progress bars immediately.
// POST /api/admin/accounts/:id/usage/refresh
func (h *Handler) RefreshAccountUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}

	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "account is not in the runtime pool")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	if err := h.ProbeUsageSnapshot(ctx, account); err != nil {
		writeError(c, http.StatusBadGateway, fmt.Sprintf("failed to refresh usage: %s", err.Error()))
		return
	}

	resp := gin.H{"refreshed": true}
	if pct, ok := account.GetUsagePercent5h(); ok {
		resp["usage_percent_5h"] = pct
	}
	if pct, ok := account.GetUsagePercent7d(); ok {
		resp["usage_percent_7d"] = pct
	}
	if pct, ok := account.GetUsagePercentSpark(); ok {
		resp["usage_percent_spark"] = pct
	}
	if t := account.GetReset5hAt(); !t.IsZero() {
		resp["reset_5h_at"] = t.Format(time.RFC3339)
	}
	if t := account.GetReset7dAt(); !t.IsZero() {
		resp["reset_7d_at"] = t.Format(time.RFC3339)
	}
	if t := account.GetResetSparkAt(); !t.IsZero() {
		resp["reset_spark_at"] = t.Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
}

type batchAccountIDsRequest struct {
	IDs      *[]int64                  `json:"ids"`
	Selector *accountOperationSelector `json:"selector,omitempty"`
}

type batchUpdateAccountsReq struct {
	updateAccountSchedulerReq
	IDs      *[]int64                  `json:"ids"`
	Selector *accountOperationSelector `json:"selector,omitempty"`
	Enabled  *bool                     `json:"enabled"`
	Locked   *bool                     `json:"locked"`
}

func (h *Handler) accountOperationIdentity(id int64) (string, string) {
	h.accountListCacheMu.RLock()
	for _, channel := range []string{database.UpstreamChannelCodex} {
		snapshot := h.accountListCache[channel]
		if snapshot == nil {
			continue
		}
		index := sort.Search(len(snapshot.Items), func(index int) bool {
			return snapshot.Items[index].ID >= id
		})
		if index < len(snapshot.Items) && snapshot.Items[index].ID == id {
			item := snapshot.Items[index]
			name := strings.TrimSpace(item.Row.Name)
			email := strings.TrimSpace(item.Email)
			h.accountListCacheMu.RUnlock()
			return name, email
		}
	}
	h.accountListCacheMu.RUnlock()
	if h.store == nil {
		return "", ""
	}
	return runtimeAccountOperationIdentity(h.store.FindByID(id))
}

// DeleteAccount deletes an account
func (h *Handler) DeleteAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// Soft delete: account data and event records are kept, but the account is removed from the runtime pool and the active list.
	if err := h.deleteAccountByID(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	h.pruneAccountsFromSnapshotCaches([]int64{id})

	writeMessage(c, http.StatusOK, "Account deleted")
}

func (h *Handler) deleteAccountByID(ctx context.Context, id int64) error {
	if err := h.db.SoftDeleteAccount(ctx, id); err != nil {
		return err
	}
	h.store.RemoveAccount(id)
	h.db.InsertAccountEventAsync(id, "deleted", "manual")
	return nil
}

type recycleBinAccountResponse struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Email           string   `json:"email"`
	PlanType        string   `json:"plan_type"`
	ATOnly          bool     `json:"at_only"`
	AccessTokenType string   `json:"access_token_type,omitempty"`
	Models          []string `json:"models,omitempty"`
	CreatedAt       string   `json:"created_at"`
	DeletedAt       string   `json:"deleted_at,omitempty"`
	LastTestStatus  string   `json:"last_test_status,omitempty"`
	LastTestAt      string   `json:"last_test_at,omitempty"`
}

// ListRecycleBinAccounts gets the list of accounts in the recycle bin
// GET /api/admin/accounts/recycle-bin
func (h *Handler) ListRecycleBinAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	rows, err := h.db.ListDeleted(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	accounts := make([]recycleBinAccountResponse, 0, len(rows))
	for _, row := range rows {
		resp := recycleBinAccountResponse{
			ID:              row.ID,
			Name:            row.Name,
			Email:           row.GetCredential("email"),
			PlanType:        row.GetCredential("plan_type"),
			ATOnly:          row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
			AccessTokenType: accountAccessTokenType(row),
			Models:          row.GetCredentialStringSlice("models"),
			CreatedAt:       row.CreatedAt.Format(time.RFC3339),
			LastTestStatus:  row.GetCredential("recycle_last_test_status"),
			LastTestAt:      row.GetCredential("recycle_last_test_at"),
		}
		if row.DeletedAt.Valid {
			resp.DeletedAt = row.DeletedAt.Time.Format(time.RFC3339)
		} else if !row.UpdatedAt.IsZero() {
			// Old data may have no deleted_at; soft delete refreshes updated_at, which is used as the fallback.
			resp.DeletedAt = row.UpdatedAt.Format(time.RFC3339)
		}
		accounts = append(accounts, resp)
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

// RestoreAccount restores an account from the recycle bin to the account pool
// POST /api/admin/accounts/:id/restore
func (h *Handler) RestoreAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.restoreAccountByID(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "account does not exist in the recycle bin")
			return
		}
		if errors.Is(err, errDuplicateOAuthIdentity) || errors.Is(err, errDuplicateCredentialWorkspaceRoute) {
			writeError(c, http.StatusConflict, "restore failed: "+err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "restore failed: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "account restored")
}

// restoreAccountByID restores a recycle bin account to active and re-adds it to the runtime pool.
func (h *Handler) restoreAccountByID(ctx context.Context, id int64) error {
	row, err := h.db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil {
		return err
	}
	seed := tokenCredentialSeedFromAccountRow(row)
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()

	if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
		if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, id); err != nil {
			return err
		} else if duplicateID > 0 {
			return fmt.Errorf("%w: an identical OAuth account already exists (id=%d); delete the normal account or clear the recycle bin account first", errDuplicateOAuthIdentity, duplicateID)
		}
		if row.GetCredential("workspace_id") != seed.workspaceID {
			if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{"workspace_id": seed.workspaceID}); err != nil {
				return err
			}
		}
	}
	if duplicateID, err := h.findCredentialWorkspaceRouteDuplicate(ctx, seed, id); err != nil {
		return err
	} else if duplicateID > 0 {
		return fmt.Errorf("%w: an account with the same credentials and target workspace already exists (id=%d); delete the normal account or clear the recycle bin account first", errDuplicateCredentialWorkspaceRoute, duplicateID)
	}

	if err := h.db.RestoreAccount(ctx, id); err != nil {
		return err
	}
	if h.store != nil {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			log.Printf("failed to load runtime after restoring account %d: %v", id, err)
			return fmt.Errorf("failed to load runtime after restoring account: %w", err)
		}
	}
	h.db.InsertAccountEventAsync(id, "restored", "recycle_bin")
	return nil
}

func tokenCredentialSeedFromAccountRow(row *database.AccountRow) tokenCredentialSeed {
	if row == nil {
		return tokenCredentialSeed{}
	}
	return normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken:          row.GetCredential("refresh_token"),
		sessionToken:          row.GetCredential("session_token"),
		accessToken:           row.GetCredential("access_token"),
		accessTokenType:       row.GetCredential("access_token_type"),
		idToken:               row.GetCredential("id_token"),
		accountID:             firstNonEmpty(row.GetCredential("account_id"), row.GetCredential("chatgpt_account_id")),
		workspaceID:           row.GetCredential("workspace_id"),
		customHeaders:         row.GetCredentialStringMap("custom_headers"),
		email:                 row.GetCredential("email"),
		planType:              row.GetCredential("plan_type"),
		expiresAtRaw:          row.GetCredential("expires_at"),
		codex7DUsedPercent:    row.GetCredential("codex_7d_used_percent"),
		codex7DResetAt:        row.GetCredential("codex_7d_reset_at"),
		codex5HUsedPercent:    row.GetCredential("codex_5h_used_percent"),
		codex5HResetAt:        row.GetCredential("codex_5h_reset_at"),
		codex5HUsageUpdatedAt: row.GetCredential("codex_5h_usage_updated_at"),
		codexUsageUpdatedAt:   row.GetCredential("codex_usage_updated_at"),
	})
}

// PurgeAccount permanently deletes an account from the recycle bin (physical delete)
// DELETE /api/admin/accounts/:id/purge
func (h *Handler) PurgeAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.db.PurgeAccount(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "account does not exist in the recycle bin")
			return
		}
		writeError(c, http.StatusInternalServerError, "permanent delete failed: "+err.Error())
		return
	}
	h.store.RemoveAccount(id)
	security.SecurityAuditLog("ACCOUNT_PURGED", fmt.Sprintf("account_id=%d ip=%s", id, c.ClientIP()))
	writeMessage(c, http.StatusOK, "account permanently deleted")
}

// emptyRecycleBinConfirmToken is the confirmation token for emptying the recycle bin; the caller must include it in the request body
// explicitly, to prevent an accidental call or a one-click script from irreversibly and physically deleting accounts.
const emptyRecycleBinConfirmToken = "EMPTY-RECYCLE-BIN"

// EmptyRecycleBin empties the recycle bin
// DELETE /api/admin/accounts/recycle-bin
// The request body must include {"confirm":"EMPTY-RECYCLE-BIN"}, otherwise the request is rejected.
func (h *Handler) EmptyRecycleBin(c *gin.Context) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request format")
			return
		}
	}
	if strings.TrimSpace(req.Confirm) != emptyRecycleBinConfirmToken {
		writeError(c, http.StatusBadRequest, `empty recycle bin requires confirmation: the request body must include confirm="EMPTY-RECYCLE-BIN"`)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	purged, err := h.db.PurgeDeletedAccounts(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "failed to empty recycle bin: "+err.Error())
		return
	}
	security.SecurityAuditLog("RECYCLE_BIN_EMPTIED", fmt.Sprintf("purged=%d ip=%s", purged, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{"message": "recycle bin emptied", "purged": purged})
}

// BatchDeleteAccounts batch-deletes accounts; when stream=true, real-time progress is returned over SSE.
// POST /api/admin/accounts/batch-delete
func (h *Handler) BatchDeleteAccounts(c *gin.Context) {
	var req batchAccountIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request format")
		return
	}
	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids and selector cannot be provided together")
		return
	}
	var ids []int64
	if req.IDs != nil {
		ids = uniqueAccountIDs(*req.IDs)
	}
	if req.Selector != nil {
		selectorCtx, selectorCancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		resolvedIDs, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		selectorCancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		ids = resolvedIDs
	}
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "please provide the list of account IDs to delete")
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchDeleteAccounts(c, ids)
		return
	}

	success, fail := h.runBatchDeleteAccounts(c.Request.Context(), ids, nil)
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("deleted %d accounts, %d failed", success, fail),
		"deleted": success,
		"success": success,
		"failed":  fail,
	})
}

func uniqueAccountIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func (h *Handler) streamBatchDeleteAccounts(c *gin.Context, ids []int64) {
	setupSSE(c)
	total := len(ids)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_delete", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_delete"})
		return
	}

	success, fail := h.runBatchDeleteAccounts(c.Request.Context(), ids, func(event batchOperationEvent) {
		sendSSEJSON(c, event)
	})
	sendSSEJSON(c, batchOperationEvent{
		Type:    "complete",
		Action:  "batch_delete",
		Current: total,
		Total:   total,
		Success: success,
		Failed:  fail,
		Deleted: success,
	})
}

func (h *Handler) runBatchDeleteAccounts(ctx context.Context, ids []int64, onProgress func(batchOperationEvent)) (int64, int64) {
	total := len(ids)
	var success int64
	var fail int64
	deleted := make([]int64, 0, len(ids))

	for i, id := range ids {
		if ctx.Err() != nil {
			fail += int64(total - i)
			break
		}

		accountName, accountEmail := h.accountOperationIdentity(id)
		err := h.deleteAccountByID(ctx, id)
		event := batchOperationEvent{
			Type:         "progress",
			Action:       "batch_delete",
			Current:      i + 1,
			Total:        total,
			AccountID:    id,
			AccountName:  accountName,
			AccountEmail: accountEmail,
		}
		if err != nil {
			fail++
			event.Error = err.Error()
			if errors.Is(err, sql.ErrNoRows) {
				event.Error = "Account not found"
			}
		} else {
			success++
			deleted = append(deleted, id)
			event.Deleted = success
			event.Message = "Account deleted"
		}
		event.Success = success
		event.Failed = fail
		if onProgress != nil {
			onProgress(event)
		}
	}

	h.pruneAccountsFromSnapshotCaches(deleted)
	return success, fail
}

// BatchUpdateAccounts batch-updates account enablement, locking and scheduling metadata.
// POST /api/admin/accounts/batch-update
func (h *Handler) BatchUpdateAccounts(c *gin.Context) {
	var req batchUpdateAccountsReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request format")
		return
	}

	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids and selector cannot be provided together")
		return
	}
	var ids []int64
	if req.IDs != nil {
		ids = uniqueAccountIDs(*req.IDs)
	}
	if req.Selector != nil {
		selectorCtx, selectorCancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		resolvedIDs, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		selectorCancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		ids = resolvedIDs
	}
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "please provide the list of account IDs to update")
		return
	}

	schedulerUpdate, err := parseAccountSchedulerUpdate(req.updateAccountSchedulerReq)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	enabled := optionalBoolFromPtr(req.Enabled)
	locked := optionalBoolFromPtr(req.Locked)
	if !enabled.Set && !locked.Set && !schedulerUpdate.hasChanges() {
		writeError(c, http.StatusBadRequest, "please provide the fields to update")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if schedulerUpdate.AllowedAPIKeyIDs.Set {
		missingAPIKeyIDs, err := h.findMissingAPIKeyIDs(ctx, schedulerUpdate.AllowedAPIKeyIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "failed to validate API key: "+err.Error())
			return
		}
		if len(missingAPIKeyIDs) > 0 {
			values := make([]string, 0, len(missingAPIKeyIDs))
			for _, value := range missingAPIKeyIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_api_key_ids contains API Key IDs that do not exist: "+strings.Join(values, ", "))
			return
		}
	}
	if schedulerUpdate.GroupIDs.Set {
		missingGroupIDs, err := h.db.VerifyAccountGroupIDs(ctx, schedulerUpdate.GroupIDs.Values)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "failed to validate account groups: "+err.Error())
			return
		}
		if len(missingGroupIDs) > 0 {
			values := make([]string, 0, len(missingGroupIDs))
			for _, value := range missingGroupIDs {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "group_ids contains group IDs that do not exist: "+strings.Join(values, ", "))
			return
		}
		if len(schedulerUpdate.GroupIDs.Values) > 0 {
			rows, err := h.db.ListActive(ctx)
			if err != nil {
				writeError(c, http.StatusInternalServerError, "failed to query accounts: "+err.Error())
				return
			}
			idSet := make(map[int64]bool, len(ids))
			for _, id := range ids {
				idSet[id] = true
			}
			targetRows := make([]*database.AccountRow, 0, len(ids))
			for _, row := range rows {
				if idSet[row.ID] {
					targetRows = append(targetRows, row)
				}
			}
			if err := h.validateGroupChannelForRows(ctx, targetRows, schedulerUpdate.GroupIDs.Values); err != nil {
				writeError(c, http.StatusBadRequest, err.Error())
				return
			}
		}
	}

	updatedIDs, err := h.db.BatchUpdateAccountMetadata(ctx, ids, database.BatchAccountMetadataUpdate{
		Enabled:                 enabled,
		Locked:                  locked,
		ScoreBiasOverride:       schedulerUpdate.ScoreBiasOverride,
		BaseConcurrencyOverride: schedulerUpdate.BaseConcurrencyOverride,
		SkipWarmTier:            schedulerUpdate.SkipWarmTier,
		AllowedAPIKeyIDs:        schedulerUpdate.AllowedAPIKeyIDs,
		Tags:                    database.OptionalStringSlice{Set: schedulerUpdate.Tags.Set, Values: schedulerUpdate.Tags.Values},
		GroupIDs:                schedulerUpdate.GroupIDs,
		ProxyURL:                schedulerUpdate.ProxyURL,
		CredentialUpdates:       schedulerUpdate.CredentialUpdates,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "batch update of accounts failed: "+err.Error())
		return
	}

	if h.store != nil {
		for _, id := range updatedIDs {
			if enabled.Set {
				if !h.store.ApplyAccountEnabled(id, enabled.Value) && enabled.Value {
					if err := h.store.LoadAccountByID(ctx, id); err != nil {
						log.Printf("failed to load account %d into the scheduling pool after batch enabling: %v", id, err)
					}
				}
			}
			if locked.Set {
				if acc := h.store.FindByID(id); acc != nil {
					if locked.Value {
						atomic.StoreInt32(&acc.Locked, 1)
					} else {
						atomic.StoreInt32(&acc.Locked, 0)
					}
				}
			}
			h.applyAccountSchedulerRuntimeUpdate(id, schedulerUpdate)
		}
	}

	success := int64(len(updatedIDs))
	failed := int64(len(ids)) - success
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("updated %d accounts, %d failed", success, failed),
		"success": success,
		"failed":  failed,
	})
}

// RefreshAccount manually refreshes an account's AT
func (h *Handler) RefreshAccount(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}

	if err := h.refreshAccountByID(c.Request.Context(), id); err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			writeError(c, http.StatusNotFound, err.Error())
			return
		}
		writeError(c, http.StatusInternalServerError, "refresh failed: "+err.Error())
		return
	}

	writeMessage(c, http.StatusOK, "Account refreshed successfully")
}

// BatchRefreshAccounts refreshes account ATs in batch; when stream=true, it returns real-time progress over SSE.
// POST /api/admin/accounts/batch-refresh
func (h *Handler) BatchRefreshAccounts(c *gin.Context) {
	var req batchAccountIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids and selector cannot be provided at the same time")
		return
	}
	var ids []int64
	if req.IDs != nil {
		ids = uniqueAccountIDs(*req.IDs)
	}
	if req.Selector != nil {
		selectorCtx, selectorCancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		resolvedIDs, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		selectorCancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		ids = resolvedIDs
	}
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "Provide the list of account IDs to refresh")
		return
	}

	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchRefreshAccounts(c, ids)
		return
	}

	success, fail := h.runBatchRefreshAccounts(c.Request.Context(), ids, nil)
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Refreshed %d accounts, %d failed", success, fail),
		"success": success,
		"failed":  fail,
	})
}

func (h *Handler) refreshAccountByID(ctx context.Context, id int64) error {
	return h.refreshAccountByIDWithProbe(ctx, id, true)
}

func (h *Handler) refreshAccountByIDWithProbe(ctx context.Context, id int64, probeAfterRefresh bool) error {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	refreshFn := h.refreshAccount
	if refreshFn == nil {
		refreshFn = h.refreshSingleAccount
	}
	if err := refreshFn(refreshCtx, id); err != nil {
		return err
	}

	// After a successful refresh, also run a zero-cost wham usage probe to sync the subscription expiry time and usage from the server's authoritative data.
	// After renewal, the chatgpt_subscription_active_until claim in the access/id token is not necessarily updated right away (it lags).
	// Relying on token refresh alone would leave the "validity period" at the old value for a long time; wham/usage returns the server's current subscription expiry time.
	// (issue #300)
	if probeAfterRefresh {
		probe := h.usageProbeFunc()
		if probe == nil || h.store == nil {
			return nil
		}
		if acc := h.store.FindByID(id); acc != nil {
			probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
			if err := probe(probeCtx, acc); err != nil {
				log.Printf("[account %d] usage/subscription expiry probe after refresh failed (ignored): %v", id, err)
			}
			probeCancel()
		}
	}
	return nil
}

func (h *Handler) streamBatchRefreshAccounts(c *gin.Context, ids []int64) {
	setupSSE(c)
	total := len(ids)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_refresh", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_refresh"})
		return
	}

	events := make(chan batchOperationEvent, len(ids)+1)
	ctx := c.Request.Context()
	go func() {
		success, fail := h.runBatchRefreshAccounts(ctx, ids, func(event batchOperationEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		select {
		case events <- batchOperationEvent{
			Type:    "complete",
			Action:  "batch_refresh",
			Current: total,
			Total:   total,
			Success: success,
			Failed:  fail,
		}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for event := range events {
		sendSSEJSON(c, event)
	}
}

func (h *Handler) runBatchRefreshAccounts(ctx context.Context, ids []int64, onProgress func(batchOperationEvent)) (int64, int64) {
	total := len(ids)
	var (
		success   int64
		fail      int64
		completed int64
		wg        sync.WaitGroup
		sem       = make(chan struct{}, accountRefreshBatchConcurrency)
	)

	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			accountName, accountEmail := h.accountOperationIdentity(id)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&fail, 1)
				emitBatchRefreshProgress(onProgress, id, accountName, accountEmail, total, &completed, &success, &fail, "Refresh canceled", true)
				return
			}
			defer func() { <-sem }()

			if err := h.refreshAccountByID(ctx, id); err != nil {
				atomic.AddInt64(&fail, 1)
				emitBatchRefreshProgress(onProgress, id, accountName, accountEmail, total, &completed, &success, &fail, err.Error(), true)
				return
			}

			atomic.AddInt64(&success, 1)
			emitBatchRefreshProgress(onProgress, id, accountName, accountEmail, total, &completed, &success, &fail, "Account refreshed successfully", false)
		}()
	}

	wg.Wait()
	return atomic.LoadInt64(&success), atomic.LoadInt64(&fail)
}

func emitBatchRefreshProgress(
	onProgress func(batchOperationEvent),
	accountID int64,
	accountName string,
	accountEmail string,
	total int,
	completedCount *int64,
	successCount *int64,
	failedCount *int64,
	message string,
	failed bool,
) {
	if onProgress == nil {
		return
	}
	current := int(atomic.AddInt64(completedCount, 1))
	event := batchOperationEvent{
		Type:         "progress",
		Action:       "batch_refresh",
		Status:       "success",
		HTTPStatus:   http.StatusOK,
		Current:      current,
		Total:        total,
		Success:      atomic.LoadInt64(successCount),
		Failed:       atomic.LoadInt64(failedCount),
		AccountID:    accountID,
		AccountName:  accountName,
		AccountEmail: accountEmail,
		Message:      message,
	}
	if failed {
		event.Status = "failed"
		event.HTTPStatus = batchOperationHTTPStatus(event.Status, message)
		event.Error = message
	}
	onProgress(event)
}

// ToggleAccountEnabled toggles whether an account takes part in scheduling selection
func (h *Handler) ToggleAccountEnabled(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.SetAccountEnabled(ctx, id, *req.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to update enabled status: "+err.Error())
		return
	}

	// When enabling an account that has not yet entered the runtime pool (such as a pending-review account submitted via the self-service portal), ApplyAccountEnabled
	// returns false because it cannot find the runtime object; in that case, load the account into the scheduling pool on demand so that "approve" takes effect immediately (issue #393).
	if !h.store.ApplyAccountEnabled(id, *req.Enabled) && *req.Enabled {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			log.Printf("failed to load account %d into scheduling pool after enabling: %v", id, err)
		}
	}

	if *req.Enabled {
		writeMessage(c, http.StatusOK, "Account enabled")
	} else {
		writeMessage(c, http.StatusOK, "Account disabled")
	}
}

// UpdateAccountNote updates the account note (a general identifier field).
func (h *Handler) UpdateAccountNote(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	note := security.SanitizeInput(strings.TrimSpace(req.Note))
	if utf8.RuneCountInString(note) > 500 {
		writeError(c, http.StatusBadRequest, "Note must not exceed 500 characters")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.db.UpdateAccountNote(ctx, id, note); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to update note: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "Note updated")
}

// ToggleAccountLock toggles the lock state of an account
func (h *Handler) ToggleAccountLock(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req struct {
		Locked bool `json:"locked"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	if err := h.db.SetAccountLocked(ctx, id, req.Locked); err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to update lock state: "+err.Error())
		return
	}

	// Sync the update to the in-memory state
	if acc := h.store.FindByID(id); acc != nil {
		if req.Locked {
			atomic.StoreInt32(&acc.Locked, 1)
		} else {
			atomic.StoreInt32(&acc.Locked, 0)
		}
	}

	if req.Locked {
		writeMessage(c, http.StatusOK, "Account locked")
	} else {
		writeMessage(c, http.StatusOK, "Account unlocked")
	}
}

func accountIsOverloadPaused(acc *auth.Account) bool {
	if acc == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(acc.GetCooldownReason()), "overload_paused") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(acc.RuntimeStatus()), "overload_paused")
}

// resetAccountRuntimeStatus clears cooldowns and model cooldowns. A pause for overload only restores scheduling and must not clear the usage snapshot,
// otherwise free accounts (which only have a 30d window, no 5h) would first show "-" in the quota bar of the list and only recover after the next probe.
func (h *Handler) resetAccountRuntimeStatus(ctx context.Context, acc *auth.Account) {
	keepUsage := accountIsOverloadPaused(acc)
	h.store.ClearCooldown(acc)
	h.store.ClearAllModelCooldowns(acc)
	if keepUsage {
		return
	}
	acc.ClearUsageCache()
	h.syncAccountPlanAfterReset(ctx, acc)
}

// ResetAccountStatus resets a single account's status to normal
func (h *Handler) ResetAccountStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "Account is not in the runtime pool")
		return
	}

	h.resetAccountRuntimeStatus(c.Request.Context(), acc)
	writeMessage(c, http.StatusOK, "Account status reset")
}

// BatchResetStatus resets account statuses to normal in batch
func (h *Handler) BatchResetStatus(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "Provide the list of account IDs to reset")
		return
	}

	success := 0
	fail := 0
	for _, id := range req.IDs {
		acc := h.store.FindByID(id)
		if acc == nil {
			fail++
			continue
		}
		h.resetAccountRuntimeStatus(c.Request.Context(), acc)
		success++
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Reset status of %d accounts", success),
		"success": success,
		"failed":  fail,
	})
}

func (h *Handler) UpdateAccountModelCooldownPolicy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}
	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}

	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	allowed := map[string]bool{
		"mode": true, "seconds": true, "backoff_enabled": true,
	}
	for key := range raw {
		if !allowed[key] {
			writeError(c, http.StatusBadRequest, "Unknown field: "+key)
			return
		}
	}
	if len(raw) == 0 {
		writeError(c, http.StatusBadRequest, "Provide at least one policy field")
		return
	}

	currentMode, currentSeconds, currentBackoff := acc.GetModelCooldownPolicyOverride()
	mode, seconds, backoff := currentMode, currentSeconds, currentBackoff
	updates := make(map[string]interface{})
	if value, ok := raw["mode"]; ok {
		if string(value) == "null" {
			mode = nil
			updates["model_cooldown_mode_override"] = nil
		} else {
			var parsed string
			if json.Unmarshal(value, &parsed) != nil || !database.IsValidModelCooldownMode(parsed) {
				writeError(c, http.StatusBadRequest, "mode must be off, fixed, adaptive, or null")
				return
			}
			parsed = database.NormalizeModelCooldownMode(parsed, database.ModelCooldownModeAdaptive)
			mode = &parsed
			updates["model_cooldown_mode_override"] = parsed
		}
	}
	if value, ok := raw["seconds"]; ok {
		if string(value) == "null" {
			seconds = nil
			updates["model_cooldown_seconds_override"] = nil
		} else {
			var parsed int
			if json.Unmarshal(value, &parsed) != nil || parsed < 1 || parsed > database.MaxModelCooldownSeconds {
				writeError(c, http.StatusBadRequest, fmt.Sprintf("seconds must be between 1 and %d, or null", database.MaxModelCooldownSeconds))
				return
			}
			seconds = &parsed
			updates["model_cooldown_seconds_override"] = parsed
		}
	}
	if value, ok := raw["backoff_enabled"]; ok {
		if string(value) == "null" {
			backoff = nil
			updates["model_cooldown_backoff_override"] = nil
		} else {
			var parsed bool
			if json.Unmarshal(value, &parsed) != nil {
				writeError(c, http.StatusBadRequest, "backoff_enabled must be a boolean or null")
				return
			}
			backoff = &parsed
			updates["model_cooldown_backoff_override"] = parsed
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, id, updates); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to save model cooldown policy: "+err.Error())
		return
	}
	h.store.ApplyAccountModelCooldownPolicyOverride(id, mode, seconds, backoff)
	effective := h.store.ResolveModelCooldownPolicy(acc)
	c.JSON(http.StatusOK, gin.H{
		"message":                   "Model cooldown policy updated",
		"mode_override":             mode,
		"seconds_override":          seconds,
		"backoff_enabled_override":  backoff,
		"mode_effective":            effective.Mode,
		"seconds_effective":         effective.Seconds,
		"backoff_enabled_effective": effective.BackoffEnabled,
	})
}

func (h *Handler) ClearAccountModelCooldown(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}
	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}
	model := strings.TrimSpace(c.Param("model"))
	if model == "" {
		writeError(c, http.StatusBadRequest, "Model must not be empty")
		return
	}
	h.store.ClearModelCooldown(acc, model)
	writeMessage(c, http.StatusOK, "Model cooldown cleared")
}

func (h *Handler) ClearAllAccountModelCooldowns(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}
	acc := h.store.FindByID(id)
	if acc == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "All model cooldowns for the account cleared",
		"cleared": h.store.ClearAllModelCooldowns(acc),
	})
}

func (h *Handler) syncAccountPlanAfterReset(_ context.Context, acc *auth.Account) {
	if h == nil || h.syncAccountPlanOnReset == nil || acc == nil {
		return
	}
	h.startDBBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		if err := h.syncAccountPlanOnReset(ctx, acc); err != nil {
			log.Printf("[account %d] failed to sync Codex plan type after reset: %v", acc.DBID, err)
		}
	})
}

func (h *Handler) syncSingleAccountPlanOnReset(ctx context.Context, acc *auth.Account) error {
	if h == nil || h.store == nil || acc == nil || acc.GetAccessToken() == "" {
		return nil
	}
	model, err := h.connectionTestModelForAccount(ctx, acc, "")
	if err != nil {
		return err
	}
	resp, err := proxy.ExecuteRequest(ctx, acc, buildConnectionTestPayload(h.store, model), "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	proxy.SyncCodexUsageState(h.store, acc, resp)
	return nil
}

func (h *Handler) refreshSingleAccount(ctx context.Context, id int64) error {
	if h == nil || h.store == nil {
		return fmt.Errorf("account pool is not initialized")
	}
	return h.store.RefreshSingle(ctx, id)
}

// ==================== Health ====================

// GetHealth system health check (extended version)
func (h *Handler) GetHealth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, healthResponse{
		Status:       "ok",
		Available:    h.store.AvailableCount(),
		Total:        h.store.AccountCount(),
		BuildVersion: version.Current(),
	})
}

// ==================== Usage ====================

// GetUsageStats returns usage statistics.
// Supports optional query parameters start/end (RFC3339); when omitted, falls back to the "today" behavior.
func (h *Handler) GetUsageStats(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	rangeStart, rangeEnd, err := parseUsageStatsRange(c.Query("start"), c.Query("end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	// The range cards follow the account/key/model/endpoint/search filters of the usage page (the same parameter set as /usage/logs);
	// status-type parameters (status/error_only, etc.) are meaningless for statistics, so they are ignored after parsing; cumulative fields are always global.
	dim, ok := parseUsageLogsFilter(c, rangeStart, rangeEnd)
	if !ok {
		return
	}

	var stats *database.UsageStats
	if strings.EqualFold(strings.TrimSpace(c.Query("detail")), "summary") {
		stats, err = h.getUsageStatsSummaryFilteredCached(ctx, rangeStart, rangeEnd, parseUsageChannel(c), dim)
	} else {
		stats, err = h.getUsageStatsFilteredCached(ctx, rangeStart, rangeEnd, parseUsageChannel(c), dim)
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, stats)
}

// parseUsageStatsRange parses the optional start/end query of /usage/stats.
// If either is empty, it is treated as a zero value and the caller decides the fallback behavior ("today" by default); if both are provided, both must be valid.
func parseUsageStatsRange(startStr, endStr string) (time.Time, time.Time, error) {
	startStr = strings.TrimSpace(startStr)
	endStr = strings.TrimSpace(endStr)
	var start, end time.Time
	if startStr != "" {
		t, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start parameter has an invalid format, RFC3339 is required")
		}
		start = t
	}
	if endStr != "" {
		t, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end parameter has an invalid format, RFC3339 is required")
		}
		end = t
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end must be later than start")
	}
	return start, end, nil
}

// GetAPIKeyTokenStats returns a token usage list aggregated by API key (issue #162).
// Supports optional query parameters start/end (RFC3339); defaults to "today" when omitted.
// No pagination or row limit: the frontend handles sorting, search, and pagination.
func (h *Handler) GetAPIKeyTokenStats(c *gin.Context) {
	rangeStart, rangeEnd, err := parseUsageStatsRange(c.Query("start"), c.Query("end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if !rangeStart.IsZero() && !rangeEnd.IsZero() && rangeEnd.Sub(rangeStart) > 366*24*time.Hour {
		writeError(c, http.StatusBadRequest, "Time range must not exceed 366 days")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	cacheKey := fmt.Sprintf("%d:%d", rangeStart.Unix()/30, rangeEnd.Unix()/30)
	type cachedResponse struct {
		Items []database.APIKeyTokenStat `json:"items"`
	}
	var response cachedResponse
	if h.getRuntimeJSON(ctx, adminAPIKeyStatsNamespace, cacheKey, &response) {
		c.JSON(http.StatusOK, response)
		return
	}

	items, err := h.db.ListAPIKeyTokenStats(ctx, rangeStart, rangeEnd)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if items == nil {
		items = []database.APIKeyTokenStat{}
	}
	response.Items = items
	h.setRuntimeJSON(ctx, adminAPIKeyStatsNamespace, cacheKey, response, adminUsageRangeCacheTTL)
	c.JSON(http.StatusOK, response)
}

// GetAPIKeyAccountStats returns the usage of a single API key split by upstream account (the transposed view of "breakdown by key" in account details).
// Supports optional query parameters start/end (RFC3339); defaults to "today" when omitted.
// GET /api/admin/usage/api-keys/:id/accounts
func (h *Handler) GetAPIKeyAccountStats(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid API key ID")
		return
	}

	rangeStart, rangeEnd, err := parseUsageStatsRange(c.Query("start"), c.Query("end"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	if !rangeStart.IsZero() && !rangeEnd.IsZero() && rangeEnd.Sub(rangeStart) > 366*24*time.Hour {
		writeError(c, http.StatusBadRequest, "Time range must not exceed 366 days")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	cacheKey := fmt.Sprintf("%d:%d:%d", id, rangeStart.Unix()/30, rangeEnd.Unix()/30)
	type cachedResponse struct {
		Items           []database.APIKeyAccountStat     `json:"items"`
		Groups          []apiKeyAccountGroupUsage        `json:"groups"`
		Summary         apiKeyAccountUsageSummary        `json:"summary"`
		Reconciliation  apiKeyAccountUsageReconciliation `json:"reconciliation"`
		MembershipBasis string                           `json:"membership_basis"`
	}
	var response cachedResponse
	if h.getRuntimeJSON(ctx, adminAPIKeyAccountsNamespace, cacheKey, &response) {
		c.JSON(http.StatusOK, response)
		return
	}

	items, err := h.db.ListAPIKeyAccountStats(ctx, id, rangeStart, rangeEnd)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if items == nil {
		items = []database.APIKeyAccountStat{}
	}
	response.Items = items
	response.Groups, response.Summary, response.Reconciliation = aggregateAPIKeyAccountGroups(items)
	response.MembershipBasis = "current_and_deleted_last_membership"
	h.setRuntimeJSON(ctx, adminAPIKeyAccountsNamespace, cacheKey, response, adminUsageRangeCacheTTL)
	c.JSON(http.StatusOK, response)
}

type apiKeyAccountUsageSummary struct {
	Accounts      int     `json:"accounts"`
	Requests      int64   `json:"requests"`
	TotalTokens   int64   `json:"total_tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
}

type apiKeyAccountGroupUsage struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Color         string  `json:"color"`
	Accounts      int     `json:"accounts"`
	Requests      int64   `json:"requests"`
	TotalTokens   int64   `json:"total_tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
}

type apiKeyAccountUsageReconciliation struct {
	GroupedTotal          apiKeyAccountUsageSummary `json:"grouped_total"`
	Ungrouped             apiKeyAccountUsageSummary `json:"ungrouped"`
	Duplicate             apiKeyAccountUsageSummary `json:"duplicate"`
	UniqueGroupedAccounts int                       `json:"unique_grouped_accounts"`
	MultiGroupAccounts    int                       `json:"multi_group_accounts"`
}

// aggregateAPIKeyAccountGroups uses current memberships for active accounts and
// the retained last membership for recycle-bin accounts. If an account belongs
// to multiple groups, its usage is intentionally included in each group; the
// overall summary remains de-duplicated.
func aggregateAPIKeyAccountGroups(items []database.APIKeyAccountStat) ([]apiKeyAccountGroupUsage, apiKeyAccountUsageSummary, apiKeyAccountUsageReconciliation) {
	groupMap := make(map[int64]*apiKeyAccountGroupUsage)
	summary := apiKeyAccountUsageSummary{Accounts: len(items)}
	reconciliation := apiKeyAccountUsageReconciliation{}
	for _, item := range items {
		summary.Requests += item.Requests
		summary.TotalTokens += item.TotalTokens
		summary.AccountBilled += item.AccountBilled
		summary.UserBilled += item.UserBilled
		groupCount := len(item.Groups)
		if groupCount == 0 {
			reconciliation.Ungrouped.Accounts++
			reconciliation.Ungrouped.Requests += item.Requests
			reconciliation.Ungrouped.TotalTokens += item.TotalTokens
			reconciliation.Ungrouped.AccountBilled += item.AccountBilled
			reconciliation.Ungrouped.UserBilled += item.UserBilled
		} else {
			reconciliation.UniqueGroupedAccounts++
		}
		if groupCount > 1 {
			reconciliation.MultiGroupAccounts++
			extraAssignments := int64(groupCount - 1)
			reconciliation.Duplicate.Accounts += groupCount - 1
			reconciliation.Duplicate.Requests += item.Requests * extraAssignments
			reconciliation.Duplicate.TotalTokens += item.TotalTokens * extraAssignments
			reconciliation.Duplicate.AccountBilled += item.AccountBilled * float64(extraAssignments)
			reconciliation.Duplicate.UserBilled += item.UserBilled * float64(extraAssignments)
		}
		for _, group := range item.Groups {
			total := groupMap[group.ID]
			if total == nil {
				total = &apiKeyAccountGroupUsage{ID: group.ID, Name: group.Name, Color: group.Color}
				groupMap[group.ID] = total
			}
			total.Accounts++
			total.Requests += item.Requests
			total.TotalTokens += item.TotalTokens
			total.AccountBilled += item.AccountBilled
			total.UserBilled += item.UserBilled
		}
	}
	groups := make([]apiKeyAccountGroupUsage, 0, len(groupMap))
	for _, group := range groupMap {
		groups = append(groups, *group)
		reconciliation.GroupedTotal.Accounts += group.Accounts
		reconciliation.GroupedTotal.Requests += group.Requests
		reconciliation.GroupedTotal.TotalTokens += group.TotalTokens
		reconciliation.GroupedTotal.AccountBilled += group.AccountBilled
		reconciliation.GroupedTotal.UserBilled += group.UserBilled
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].UserBilled == groups[j].UserBilled {
			return groups[i].TotalTokens > groups[j].TotalTokens
		}
		return groups[i].UserBilled > groups[j].UserBilled
	})
	return groups, summary, reconciliation
}

// GetChartData returns aggregated chart data (server-side bucketing + in-memory cache)
func (h *Handler) GetChartData(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	bucketStr := c.DefaultQuery("bucket_minutes", "5")

	startTime, e1 := time.Parse(time.RFC3339, startStr)
	endTime, e2 := time.Parse(time.RFC3339, endStr)
	if e1 != nil || e2 != nil {
		writeError(c, http.StatusBadRequest, "start/end parameter has an invalid format, RFC3339 is required")
		return
	}
	bucketMinutes, _ := strconv.Atoi(bucketStr)
	if bucketMinutes < 1 {
		bucketMinutes = 5
	}

	channel := parseUsageChannel(c)

	// Canonicalize moving ranges so periodic refreshes reuse the same result.
	// The bucket width itself is the natural cache window for chart data.
	cacheWindow := int64(bucketMinutes * 60)
	cacheKey := fmt.Sprintf("%d|%d|%d|%s", startTime.Unix()/cacheWindow, endTime.Unix()/cacheWindow, bucketMinutes, channel)
	h.chartCacheMu.RLock()
	if entry, ok := h.chartCacheData[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		h.chartCacheMu.RUnlock()
		c.JSON(http.StatusOK, entry.data)
		return
	}
	h.chartCacheMu.RUnlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var cached database.ChartAggregation
	if h.getRuntimeJSON(ctx, adminChartCacheNamespace, cacheKey, &cached) {
		result := &cached
		h.chartCacheMu.Lock()
		h.chartCacheData[cacheKey] = &chartCacheEntry{
			data:      result,
			expiresAt: time.Now().Add(adminChartCacheTTL),
		}
		h.chartCacheMu.Unlock()
		c.JSON(http.StatusOK, result)
		return
	}

	result, err := h.db.GetChartAggregation(ctx, startTime, endTime, bucketMinutes, channel)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.setRuntimeJSON(ctx, adminChartCacheNamespace, cacheKey, result, adminChartCacheTTL)

	// Write to cache
	h.chartCacheMu.Lock()
	h.chartCacheData[cacheKey] = &chartCacheEntry{
		data:      result,
		expiresAt: time.Now().Add(adminChartCacheTTL),
	}
	// Clean up expired entries (deferred cleanup to avoid memory leaks)
	for k, v := range h.chartCacheData {
		if time.Now().After(v.expiresAt) {
			delete(h.chartCacheData, k)
		}
	}
	h.chartCacheMu.Unlock()

	c.JSON(http.StatusOK, result)
}

func parseOpsErrorPositiveInt64(c *gin.Context, name string) (*int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("%s parameter is invalid, a positive integer is required", name))
		return nil, false
	}
	return &parsed, true
}

func parseOpsErrorLogFilter(c *gin.Context, withPaging bool) (database.UsageLogFilter, bool) {
	endTime := time.Now()
	startTime := endTime.Add(-1 * time.Hour)
	startStr := strings.TrimSpace(c.Query("start"))
	endStr := strings.TrimSpace(c.Query("end"))
	if startStr != "" || endStr != "" {
		if startStr == "" || endStr == "" {
			writeError(c, http.StatusBadRequest, "start/end parameters must be provided together")
			return database.UsageLogFilter{}, false
		}
		parsedStart, e1 := time.Parse(time.RFC3339, startStr)
		parsedEnd, e2 := time.Parse(time.RFC3339, endStr)
		if e1 != nil || e2 != nil {
			writeError(c, http.StatusBadRequest, "start/end parameter has an invalid format, RFC3339 is required")
			return database.UsageLogFilter{}, false
		}
		startTime = parsedStart
		endTime = parsedEnd
	}

	apiKeyID, ok := parseOpsErrorPositiveInt64(c, "api_key_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	accountID, ok := parseOpsErrorPositiveInt64(c, "account_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}

	filter := database.UsageLogFilter{
		Start:           startTime,
		End:             endTime,
		Page:            1,
		PageSize:        20,
		Email:           strings.TrimSpace(c.Query("email")),
		Model:           strings.TrimSpace(c.Query("model")),
		Endpoint:        strings.TrimSpace(c.Query("endpoint")),
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		ErrorOnly:       true,
		IncludeCanceled: true,
		ErrorKind:       strings.TrimSpace(c.Query("error_kind")),
		Query:           strings.TrimSpace(c.Query("q")),
		Channel:         parseUsageChannel(c),
	}

	status := strings.TrimSpace(c.Query("status"))
	if status == "" {
		status = strings.TrimSpace(c.Query("status_code"))
	}
	switch strings.ToLower(status) {
	case "", "all":
	case "4xx", "5xx":
		filter.StatusFamily = strings.ToLower(status)
	default:
		statusCode, err := strconv.Atoi(status)
		if err != nil || statusCode < 100 || statusCode > 599 {
			writeError(c, http.StatusBadRequest, "status/status_code parameter is invalid")
			return database.UsageLogFilter{}, false
		}
		filter.StatusCode = statusCode
	}

	if fastStr := c.Query("fast"); fastStr != "" {
		v := fastStr == "true"
		filter.FastOnly = &v
	}
	if streamStr := c.Query("stream"); streamStr != "" {
		v := streamStr == "true"
		filter.StreamOnly = &v
	}

	if withPaging {
		if pageStr := c.Query("page"); pageStr != "" {
			if page, err := strconv.Atoi(pageStr); err == nil && page > 0 {
				filter.Page = page
			}
		}
		if ps := c.Query("page_size"); ps != "" {
			if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 500 {
				filter.PageSize = n
			}
		}
	}

	return filter, true
}

type opsErrorExportFile struct {
	Version           int                   `json:"version"`
	GeneratedAt       time.Time             `json:"generated_at"`
	Range             opsErrorExportRange   `json:"range"`
	Filters           opsErrorExportFilters `json:"filters"`
	Options           opsErrorExportOptions `json:"options"`
	TotalMatched      int                   `json:"total_matched"`
	ExcludedCount     int                   `json:"excluded_count"`
	ExportedCount     int                   `json:"exported_count"`
	DuplicatesRemoved int                   `json:"duplicates_removed"`
	Errors            []opsErrorExportEntry `json:"errors"`
}

type opsErrorExportRange struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type opsErrorExportFilters struct {
	Email        string `json:"email,omitempty"`
	Model        string `json:"model,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	APIKeyID     *int64 `json:"api_key_id,omitempty"`
	AccountID    *int64 `json:"account_id,omitempty"`
	FastOnly     *bool  `json:"fast_only,omitempty"`
	StreamOnly   *bool  `json:"stream_only,omitempty"`
	StatusCode   int    `json:"status_code,omitempty"`
	StatusFamily string `json:"status_family,omitempty"`
	ErrorKind    string `json:"error_kind,omitempty"`
	Query        string `json:"query,omitempty"`
}

type opsErrorExportOptions struct {
	Dedupe              bool  `json:"dedupe"`
	ExcludedStatusCodes []int `json:"excluded_status_codes,omitempty"`
}

type opsErrorExportEntry struct {
	Signature          string    `json:"signature"`
	Occurrences        int       `json:"occurrences"`
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	SampleIDs          []int64   `json:"sample_ids"`
	AffectedAccountIDs []int64   `json:"affected_account_ids,omitempty"`
	AffectedAPIKeyIDs  []int64   `json:"affected_api_key_ids,omitempty"`
	ID                 int64     `json:"id"`
	CreatedAt          time.Time `json:"created_at"`
	StatusCode         int       `json:"status_code"`
	ErrorKind          string    `json:"error_kind"`
	ErrorMessage       string    `json:"error_message"`
	AccountID          int64     `json:"account_id"`
	AccountName        string    `json:"account_name"`
	AccountEmail       string    `json:"account_email"`
	APIKeyID           int64     `json:"api_key_id"`
	APIKeyName         string    `json:"api_key_name"`
	APIKeyMasked       string    `json:"api_key_masked"`
	Endpoint           string    `json:"endpoint"`
	UpstreamEndpoint   string    `json:"upstream_endpoint"`
	Model              string    `json:"model"`
	EffectiveModel     string    `json:"effective_model"`
	Stream             bool      `json:"stream"`
	DurationMs         int       `json:"duration_ms"`
	FirstTokenMs       int       `json:"first_token_ms"`
	IsRetryAttempt     bool      `json:"is_retry_attempt"`
	AttemptIndex       int       `json:"attempt_index"`
}

// GetOpsErrorLogs gets the ops error logs
func (h *Handler) GetOpsErrorLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, true)
	if !ok {
		return
	}
	result, err := h.db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ExportOpsErrorLogs exports the ops error logs as JSON.
func (h *Handler) ExportOpsErrorLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, false)
	if !ok {
		return
	}
	dedupe := parseBoolQueryDefault(c, "dedupe", true)
	excludedStatusCodes, excludedStatusSet, ok := parseExcludedStatusCodes(c.Query("exclude_status"))
	if !ok {
		writeError(c, http.StatusBadRequest, "exclude_status parameter is invalid")
		return
	}

	logs, err := h.db.ListUsageLogsByFilter(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	exportFile := buildOpsErrorExportFile(logs, filter, dedupe, excludedStatusCodes, excludedStatusSet)
	body, err := json.MarshalIndent(exportFile, "", "  ")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	body = append(body, '\n')

	filename := fmt.Sprintf("ops-errors-%s.json", time.Now().Format("20060102-150405"))
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func parseBoolQueryDefault(c *gin.Context, name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(c.Query(name)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseExcludedStatusCodes(raw string) ([]int, map[int]bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, map[int]bool{}, true
	}
	seen := map[int]bool{}
	var statuses []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, err := strconv.Atoi(part)
		if err != nil || code < 100 || code > 599 {
			return nil, nil, false
		}
		if !seen[code] {
			seen[code] = true
			statuses = append(statuses, code)
		}
	}
	sort.Ints(statuses)
	return statuses, seen, true
}

func buildOpsErrorExportFile(logs []*database.UsageLog, filter database.UsageLogFilter, dedupe bool, excludedStatusCodes []int, excludedStatusSet map[int]bool) opsErrorExportFile {
	exportFile := opsErrorExportFile{
		Version:      1,
		GeneratedAt:  time.Now(),
		Range:        opsErrorExportRange{Start: filter.Start, End: filter.End},
		Filters:      opsErrorExportFiltersFromUsageFilter(filter),
		Options:      opsErrorExportOptions{Dedupe: dedupe, ExcludedStatusCodes: excludedStatusCodes},
		TotalMatched: len(logs),
		Errors:       []opsErrorExportEntry{},
	}

	filteredLogs := make([]*database.UsageLog, 0, len(logs))
	for _, logRow := range logs {
		if logRow == nil {
			continue
		}
		if excludedStatusSet[logRow.StatusCode] {
			exportFile.ExcludedCount++
			continue
		}
		filteredLogs = append(filteredLogs, logRow)
	}

	if !dedupe {
		for _, logRow := range filteredLogs {
			entry := newOpsErrorExportEntry(logRow)
			exportFile.Errors = append(exportFile.Errors, entry)
		}
		exportFile.ExportedCount = len(exportFile.Errors)
		return exportFile
	}

	entryBySignature := make(map[string]int)
	for _, logRow := range filteredLogs {
		entry := newOpsErrorExportEntry(logRow)
		if idx, exists := entryBySignature[entry.Signature]; exists {
			exportFile.Errors[idx].merge(logRow)
			continue
		}
		entryBySignature[entry.Signature] = len(exportFile.Errors)
		exportFile.Errors = append(exportFile.Errors, entry)
	}
	sort.SliceStable(exportFile.Errors, func(i, j int) bool {
		if exportFile.Errors[i].Occurrences != exportFile.Errors[j].Occurrences {
			return exportFile.Errors[i].Occurrences > exportFile.Errors[j].Occurrences
		}
		return exportFile.Errors[i].LastSeen.After(exportFile.Errors[j].LastSeen)
	})
	exportFile.ExportedCount = len(exportFile.Errors)
	exportFile.DuplicatesRemoved = len(filteredLogs) - len(exportFile.Errors)
	return exportFile
}

func opsErrorExportFiltersFromUsageFilter(filter database.UsageLogFilter) opsErrorExportFilters {
	return opsErrorExportFilters{
		Email:        filter.Email,
		Model:        filter.Model,
		Endpoint:     filter.Endpoint,
		APIKeyID:     filter.APIKeyID,
		AccountID:    filter.AccountID,
		FastOnly:     filter.FastOnly,
		StreamOnly:   filter.StreamOnly,
		StatusCode:   filter.StatusCode,
		StatusFamily: filter.StatusFamily,
		ErrorKind:    filter.ErrorKind,
		Query:        filter.Query,
	}
}

func newOpsErrorExportEntry(logRow *database.UsageLog) opsErrorExportEntry {
	entry := opsErrorExportEntry{
		Signature:          opsErrorSignature(logRow),
		Occurrences:        1,
		FirstSeen:          logRow.CreatedAt,
		LastSeen:           logRow.CreatedAt,
		SampleIDs:          []int64{logRow.ID},
		AffectedAccountIDs: appendUniqueInt64(nil, logRow.AccountID, 50),
		AffectedAPIKeyIDs:  appendUniqueInt64(nil, logRow.APIKeyID, 50),
		ID:                 logRow.ID,
		CreatedAt:          logRow.CreatedAt,
		StatusCode:         logRow.StatusCode,
		ErrorKind:          logRow.UpstreamErrorKind,
		ErrorMessage:       logRow.ErrorMessage,
		AccountID:          logRow.AccountID,
		AccountName:        logRow.AccountName,
		AccountEmail:       logRow.AccountEmail,
		APIKeyID:           logRow.APIKeyID,
		APIKeyName:         logRow.APIKeyName,
		APIKeyMasked:       logRow.APIKeyMasked,
		Endpoint:           firstNonEmpty(logRow.InboundEndpoint, logRow.Endpoint),
		UpstreamEndpoint:   logRow.UpstreamEndpoint,
		Model:              logRow.Model,
		EffectiveModel:     logRow.EffectiveModel,
		Stream:             logRow.Stream,
		DurationMs:         logRow.DurationMs,
		FirstTokenMs:       logRow.FirstTokenMs,
		IsRetryAttempt:     logRow.IsRetryAttempt,
		AttemptIndex:       logRow.AttemptIndex,
	}
	return entry
}

func (entry *opsErrorExportEntry) merge(logRow *database.UsageLog) {
	entry.Occurrences++
	if logRow.CreatedAt.Before(entry.FirstSeen) {
		entry.FirstSeen = logRow.CreatedAt
	}
	if logRow.CreatedAt.After(entry.LastSeen) {
		entry.LastSeen = logRow.CreatedAt
	}
	entry.SampleIDs = appendUniqueInt64(entry.SampleIDs, logRow.ID, 20)
	entry.AffectedAccountIDs = appendUniqueInt64(entry.AffectedAccountIDs, logRow.AccountID, 50)
	entry.AffectedAPIKeyIDs = appendUniqueInt64(entry.AffectedAPIKeyIDs, logRow.APIKeyID, 50)
}

func opsErrorSignature(logRow *database.UsageLog) string {
	parts := []string{
		strconv.Itoa(logRow.StatusCode),
		strings.TrimSpace(logRow.UpstreamErrorKind),
		strings.Join(strings.Fields(logRow.ErrorMessage), " "),
		firstNonEmpty(logRow.InboundEndpoint, logRow.Endpoint),
		strings.TrimSpace(logRow.UpstreamEndpoint),
		strings.TrimSpace(logRow.Model),
		strings.TrimSpace(logRow.EffectiveModel),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:12])
}

func appendUniqueInt64(values []int64, value int64, limit int) []int64 {
	if value <= 0 || (limit > 0 && len(values) >= limit) {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func parseUsageLogBoolFilter(c *gin.Context, name string) (*bool, bool) {
	raw, exists := c.GetQuery(name)
	if !exists {
		return nil, true
	}
	switch strings.TrimSpace(raw) {
	case "true":
		value := true
		return &value, true
	case "false":
		value := false
		return &value, true
	default:
		writeError(c, http.StatusBadRequest, name+" parameter is invalid, true or false is required")
		return nil, false
	}
}

func parseUsageLogStatusFilter(c *gin.Context, filter *database.UsageLogFilter) bool {
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(c.Query("status_code")))
	}
	switch status {
	case "", "all":
		return true
	case "2xx", "4xx", "5xx":
		filter.StatusFamily = status
		if status == "4xx" {
			filter.IncludeCanceled = true
		}
		return true
	default:
		statusCode, err := strconv.Atoi(status)
		if err != nil || statusCode < 100 || statusCode > 599 {
			writeError(c, http.StatusBadRequest, "status/status_code parameter is invalid")
			return false
		}
		filter.StatusCode = statusCode
		if statusCode == 499 {
			filter.IncludeCanceled = true
		}
		return true
	}
}

func parseUsageLogsFilter(c *gin.Context, startTime, endTime time.Time) (database.UsageLogFilter, bool) {
	apiKeyID, ok := parseOpsErrorPositiveInt64(c, "api_key_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	accountID, ok := parseOpsErrorPositiveInt64(c, "account_id")
	if !ok {
		return database.UsageLogFilter{}, false
	}

	filter := database.UsageLogFilter{
		RequestID:         strings.TrimSpace(c.Query("request_id")),
		UpstreamRequestID: strings.TrimSpace(c.Query("upstream_request_id")),
		Start:             startTime,
		End:               endTime,
		Page:              1,
		PageSize:          20,
		Email:             strings.TrimSpace(c.Query("email")),
		Model:             strings.TrimSpace(c.Query("model")),
		Endpoint:          strings.TrimSpace(c.Query("endpoint")),
		APIKeyID:          apiKeyID,
		AccountID:         accountID,
		ErrorKind:         strings.TrimSpace(c.Query("error_kind")),
		Query:             strings.TrimSpace(c.Query("q")),
		Channel:           parseUsageChannel(c),
	}

	if pageStr := c.Query("page"); pageStr != "" {
		if page, err := strconv.Atoi(pageStr); err == nil && page > 0 {
			filter.Page = page
		}
	}
	if pageSizeStr := c.Query("page_size"); pageSizeStr != "" {
		if pageSize, err := strconv.Atoi(pageSizeStr); err == nil && pageSize > 0 && pageSize <= 500 {
			filter.PageSize = pageSize
		}
	}

	if fastStr := c.Query("fast"); fastStr != "" {
		value := fastStr == "true"
		filter.FastOnly = &value
	}
	if streamStr := c.Query("stream"); streamStr != "" {
		value := streamStr == "true"
		filter.StreamOnly = &value
	}

	filter.CompactOnly, ok = parseUsageLogBoolFilter(c, "compact")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	filter.CompactionHistoryOnly, ok = parseUsageLogBoolFilter(c, "has_compaction_history")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	filter.RetryOnly, ok = parseUsageLogBoolFilter(c, "retry")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	filter.ViaWebsocketOnly, ok = parseUsageLogBoolFilter(c, "via_websocket")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	filter.UltraOnly, ok = parseUsageLogBoolFilter(c, "ultra")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	filter.UpstreamModelMismatchOnly, ok = parseUsageLogBoolFilter(c, "upstream_model_mismatch")
	if !ok {
		return database.UsageLogFilter{}, false
	}

	errorOnly, ok := parseUsageLogBoolFilter(c, "error_only")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	if errorOnly != nil {
		filter.ErrorOnly = *errorOnly
	}
	includeCanceled, ok := parseUsageLogBoolFilter(c, "include_canceled")
	if !ok {
		return database.UsageLogFilter{}, false
	}
	if includeCanceled != nil {
		filter.IncludeCanceled = *includeCanceled
	}
	if filter.ErrorOnly {
		filter.IncludeCanceled = true
	}
	if !parseUsageLogStatusFilter(c, &filter) {
		return database.UsageLogFilter{}, false
	}

	return filter, true
}

// GetOpsErrorSummary gets the summary of the ops error logs
func (h *Handler) GetOpsErrorSummary(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	filter, ok := parseOpsErrorLogFilter(c, false)
	if !ok {
		return
	}
	result, err := h.db.GetUsageErrorSummary(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetUsageLogsErrorSummary returns the error summary linked to the request log filters.
func (h *Handler) GetUsageLogsErrorSummary(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	startStr := strings.TrimSpace(c.Query("start"))
	endStr := strings.TrimSpace(c.Query("end"))
	if startStr == "" || endStr == "" {
		writeError(c, http.StatusBadRequest, "start/end parameters must be provided together")
		return
	}
	startTime, startErr := time.Parse(time.RFC3339, startStr)
	endTime, endErr := time.Parse(time.RFC3339, endStr)
	if startErr != nil || endErr != nil {
		writeError(c, http.StatusBadRequest, "start/end parameter has an invalid format, RFC3339 is required")
		return
	}

	filter, ok := parseUsageLogsFilter(c, startTime, endTime)
	if !ok {
		return
	}
	result, err := h.db.GetUsageErrorSummary(ctx, filter)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetUsageLogs gets the usage logs
func (h *Handler) GetUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	startStr := c.Query("start")
	endStr := c.Query("end")

	if startStr != "" && endStr != "" {
		startTime, e1 := time.Parse(time.RFC3339, startStr)
		endTime, e2 := time.Parse(time.RFC3339, endStr)
		if e1 != nil || e2 != nil {
			writeError(c, http.StatusBadRequest, "start/end parameter has an invalid format, RFC3339 is required")
			return
		}

		// With a page parameter → server-side pagination (usage page table)
		if c.Query("page") != "" {
			filter, ok := parseUsageLogsFilter(c, startTime, endTime)
			if !ok {
				return
			}

			result, err := h.db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				writeInternalError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
			return
		}

		// Without a page parameter → return everything (dashboard chart aggregation)
		logs, err := h.db.ListUsageLogsByTimeRange(ctx, startTime, endTime)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if logs == nil {
			logs = []*database.UsageLog{}
		}
		c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
		return
	}

	// Fallback: limit mode
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	logs, err := h.db.ListRecentUsageLogs(ctx, limit)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if logs == nil {
		logs = []*database.UsageLog{}
	}
	c.JSON(http.StatusOK, usageLogsResponse{Logs: logs})
}

// ClearUsageLogs clears all usage logs
func (h *Handler) ClearUsageLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if err := h.db.ClearUsageLogs(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	h.deleteRuntimeCache(ctx, adminUsageStatsCacheNamespace, "global")
	h.chartCacheMu.Lock()
	h.chartCacheData = make(map[string]*chartCacheEntry)
	h.chartCacheMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"message": "Logs cleared"})
}

// ==================== API Keys ====================

// ListAPIKeys gets all API keys (masked version).
// view=lite returns only the key rows themselves, skipping the window cost and last-used time: both need to aggregate usage_logs,
// while the filter dropdowns, banners, and similar places do not need them, and on a slow SQLite disk they would scan everything again on every page open.
func (h *Handler) ListAPIKeys(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if strings.EqualFold(strings.TrimSpace(c.Query("view")), "lite") {
		maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
		for _, k := range keys {
			maskedKeys = append(maskedKeys, NewMaskedAPIKeyRow(k))
		}
		c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
		return
	}

	// Check whether any key has a window cost limit configured
	var need5h, need7d, need30d, needDaily bool
	for _, k := range keys {
		if k.Limits.CostLimit5h > 0 {
			need5h = true
		}
		if k.Limits.CostLimit7d > 0 {
			need7d = true
		}
		if k.Limits.CostLimit30d > 0 {
			need30d = true
		}
		if k.Limits.CostLimitDaily > 0 {
			needDaily = true
		}
	}

	// Query window usage in batch on demand
	var cost5h, cost7d, cost30d, costToday map[int64]float64
	if need5h {
		cost5h, _ = h.db.GetAllAPIKeysWindowCost(ctx, 5*time.Hour)
	}
	if need7d {
		cost7d, _ = h.db.GetAllAPIKeysWindowCost(ctx, 7*24*time.Hour)
	}
	if need30d {
		cost30d, _ = h.db.GetAllAPIKeysWindowCost(ctx, 30*24*time.Hour)
	}
	if needDaily {
		costToday, _ = h.db.GetAllAPIKeysCostSince(ctx, database.StartOfDay(time.Now()))
	}

	// Last-used time: one aggregation; a failure does not block the list
	lastUsedByID, _ := h.db.ListAPIKeyLastUsedAt(ctx)

	// Convert to the masked response
	maskedKeys := make([]*MaskedAPIKeyRow, 0, len(keys))
	for _, k := range keys {
		mk := NewMaskedAPIKeyRow(k)
		if k.Limits.CostLimit5h > 0 || k.Limits.CostLimit7d > 0 || k.Limits.CostLimit30d > 0 || k.Limits.CostLimitDaily > 0 {
			detail := &APIKeyWindowUsageDetail{}
			if cost5h != nil {
				detail.Cost5h = cost5h[k.ID]
			}
			if cost7d != nil {
				detail.Cost7d = cost7d[k.ID]
			}
			if cost30d != nil {
				detail.Cost30d = cost30d[k.ID]
			}
			if costToday != nil {
				detail.CostToday = costToday[k.ID]
			}
			mk.WindowUsage = detail
		}
		if lastUsedByID != nil {
			if lastUsed, ok := lastUsedByID[k.ID]; ok && !lastUsed.IsZero() {
				formatted := lastUsed.Format(time.RFC3339)
				mk.LastUsedAt = &formatted
			}
		}
		maskedKeys = append(maskedKeys, mk)
	}

	c.JSON(http.StatusOK, apiKeysResponse{Keys: maskedKeys})
}

type createKeyReq struct {
	Name            string              `json:"name"`
	Key             string              `json:"key"`
	QuotaLimit      *float64            `json:"quota_limit"`
	Quota           *float64            `json:"quota"`
	ExpiresAt       string              `json:"expires_at"`
	ExpiresInDays   *int                `json:"expires_in_days"`
	AllowedGroupIDs json.RawMessage     `json:"allowed_group_ids"`
	Limits          *createAPIKeyLimits `json:"limits"`
}

// generateKey generates a random API key
func generateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// CreateAPIKey creates a new API key (enhanced version, with input validation)
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Name = ""
	}

	// Input validation and sanitization
	req.Name = security.SanitizeInput(req.Name)
	if req.Name == "" {
		req.Name = "default"
	}

	// Validate name length
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "Name must not exceed 100 characters")
		return
	}

	// Check for XSS
	if security.ContainsXSS(req.Name) {
		writeError(c, http.StatusBadRequest, "Name contains illegal characters")
		return
	}

	quotaLimit := 0.0
	if req.Quota != nil {
		quotaLimit = *req.Quota
	}
	if req.QuotaLimit != nil {
		quotaLimit = *req.QuotaLimit
	}
	if quotaLimit < 0 {
		writeError(c, http.StatusBadRequest, "Quota limit cannot be less than 0")
		return
	}
	if quotaLimit > 1000000000 {
		writeError(c, http.StatusBadRequest, "Quota limit is too large")
		return
	}

	expiresAt, err := parseAPIKeyExpiresAt(req.ExpiresAt, req.ExpiresInDays)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	allowedGroupIDs, err := parseOptionalIntegerSliceField(req.AllowedGroupIDs, "allowed_group_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	key := req.Key
	if key == "" {
		var err error
		key, err = generateKey()
		if err != nil {
			writeInternalError(c, err)
			return
		}
	} else {
		// Validate the format of the key supplied by the user
		key = security.SanitizeInput(key)
		if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
			writeError(c, http.StatusBadRequest, "API key format is invalid")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if allowedGroupIDs.Set {
		missing, err := h.db.VerifyAccountGroupIDs(ctx, allowedGroupIDs.Values)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if len(missing) > 0 {
			values := make([]string, 0, len(missing))
			for _, value := range missing {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_group_ids contains group IDs that do not exist: "+strings.Join(values, ", "))
			return
		}
	}

	limits := sanitizeAPIKeyLimits(req.Limits.withDefaults())
	if req.Limits != nil {
		limits.ModelRequestLimits, err = normalizeAdminAPIKeyModelRequestLimits(req.Limits.ModelRequestLimits, nil)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.validateAPIKeyGroupIDs(ctx, limits.NoAffinityGroupIDs, "limits.no_affinity_group_ids"); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.validateAPIKeyScopeLimits(ctx, limits.ScopeLimits); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	id, err := h.db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{
		Name:            req.Name,
		Key:             key,
		QuotaLimit:      quotaLimit,
		ExpiresAt:       expiresAt,
		AllowedGroupIDs: allowedGroupIDs.Values,
		Limits:          limits,
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Creation failed: "+err.Error())
		return
	}
	if allowedGroupIDs.Set {
		values := dedupeInt64(allowedGroupIDs.Values)
		if h.store != nil {
			h.store.SetAPIKeyAllowedGroups(id, values)
		}
	}
	if h.store != nil {
		h.store.SetAPIKeyNoAffinityGroups(id, limits.NoAffinityGroupIDs)
		h.store.SetAPIKeyAllowedPlans(id, limits.PlanAllow)
	}
	// A newly configured cumulative quota must start metering immediately, without waiting for the 60s cache on the persistence side to expire.
	h.db.InvalidateScopeQuotaKeyCache()
	h.invalidateAPIKeyRuntimeCaches(ctx, key)

	// Record the security audit log
	security.SecurityAuditLog("API_KEY_CREATED", fmt.Sprintf("id=%d name=%s ip=%s", id, security.SanitizeLog(req.Name), c.ClientIP()))

	var expiresAtResponse *string
	if expiresAt.Valid {
		formatted := expiresAt.Time.Format(time.RFC3339)
		expiresAtResponse = &formatted
	}
	c.JSON(http.StatusOK, createAPIKeyResponse{
		ID:              id,
		Key:             key,
		Name:            req.Name,
		QuotaLimit:      quotaLimit,
		QuotaUsed:       0,
		ExpiresAt:       expiresAtResponse,
		AllowedGroupIDs: dedupeInt64(allowedGroupIDs.Values),
	})
}

type updateAPIKeyReq struct {
	Name            *string                `json:"name"`
	QuotaLimit      json.RawMessage        `json:"quota_limit"`
	Quota           json.RawMessage        `json:"quota"`
	ResetQuota      *bool                  `json:"reset_quota"`
	ExpiresAt       json.RawMessage        `json:"expires_at"`
	ExpiresInDays   *int                   `json:"expires_in_days"`
	AllowedGroupIDs json.RawMessage        `json:"allowed_group_ids"`
	Limits          *database.APIKeyLimits `json:"limits"`
	Enabled         *bool                  `json:"enabled"`
}

func (h *Handler) UpdateAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid ID")
		return
	}
	var req updateAPIKeyReq
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	allowedGroupIDs, err := parseOptionalIntegerSliceField(req.AllowedGroupIDs, "allowed_group_ids")
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	quotaLimit, quotaLimitSet, err := parseOptionalAPIKeyQuota(req.QuotaLimit, req.Quota)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	expiresAt, expiresAtSet, err := parseOptionalAPIKeyExpiration(req.ExpiresAt, req.ExpiresInDays)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "API key not found")
			return
		}
		writeInternalError(c, err)
		return
	}
	if req.Name != nil {
		name := security.SanitizeInput(*req.Name)
		if strings.TrimSpace(name) == "" {
			writeError(c, http.StatusBadRequest, "Name must not be empty")
			return
		}
		if utf8.RuneCountInString(name) > 100 {
			writeError(c, http.StatusBadRequest, "Name must not exceed 100 characters")
			return
		}
		if security.ContainsXSS(name) {
			writeError(c, http.StatusBadRequest, "Name contains illegal characters")
			return
		}
		req.Name = &name
	}
	if quotaLimitSet {
		if quotaLimit > 1000000000 {
			writeError(c, http.StatusBadRequest, "Quota limit must not exceed 1000000000")
			return
		}
	}
	var allowedGroupValues []int64
	if allowedGroupIDs.Set {
		missing, err := h.db.VerifyAccountGroupIDs(ctx, allowedGroupIDs.Values)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if len(missing) > 0 {
			values := make([]string, 0, len(missing))
			for _, value := range missing {
				values = append(values, strconv.FormatInt(value, 10))
			}
			writeError(c, http.StatusBadRequest, "allowed_group_ids contains group IDs that do not exist: "+strings.Join(values, ", "))
			return
		}
		allowedGroupValues = dedupeInt64(allowedGroupIDs.Values)
	}
	update := database.APIKeyUpdate{
		QuotaLimit:         quotaLimit,
		QuotaLimitSet:      quotaLimitSet,
		ResetQuota:         req.ResetQuota != nil && *req.ResetQuota,
		ExpiresAt:          expiresAt,
		ExpiresAtSet:       expiresAtSet,
		AllowedGroupIDs:    allowedGroupValues,
		AllowedGroupIDsSet: allowedGroupIDs.Set,
	}
	if req.Name != nil {
		update.Name = *req.Name
		update.NameSet = true
	}
	if req.Enabled != nil {
		update.Enabled = *req.Enabled
		update.EnabledSet = true
	}
	if req.Limits != nil {
		update.Limits = sanitizeAPIKeyLimits(*req.Limits)
		update.Limits.ModelRequestLimits, err = normalizeAdminAPIKeyModelRequestLimits(req.Limits.ModelRequestLimits, row.Limits.ModelRequestLimits)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.validateAPIKeyGroupIDs(ctx, update.Limits.NoAffinityGroupIDs, "limits.no_affinity_group_ids"); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.validateAPIKeyScopeLimits(ctx, update.Limits.ScopeLimits); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		update.LimitsSet = true
	}
	if err := h.db.UpdateAPIKey(ctx, id, update); err != nil {
		writeInternalError(c, err)
		return
	}
	if allowedGroupIDs.Set && h.store != nil {
		h.store.SetAPIKeyAllowedGroups(id, allowedGroupValues)
	}
	if update.LimitsSet && h.store != nil {
		h.store.SetAPIKeyNoAffinityGroups(id, update.Limits.NoAffinityGroupIDs)
		h.store.SetAPIKeyAllowedPlans(id, update.Limits.PlanAllow)
	}
	if update.LimitsSet {
		h.db.InvalidateScopeQuotaKeyCache()
	}
	h.invalidateAPIKeyRuntimeCaches(ctx, row.Key)
	savedLimits := row.Limits
	if update.LimitsSet {
		savedLimits = update.Limits
	}
	c.JSON(http.StatusOK, gin.H{"message": "API key updated", "limits": savedLimits})
}

// sanitizeAPIKeyLimits normalizes the limits from the request body: negative values are set to 0, blank model names are filtered out, and strings are lowercased.
// When both ModelAllow and ModelDeny are configured, the allowlist takes precedence (already in effect at enforce time); the blocklist is not forcibly cleared here.
func sanitizeAPIKeyLimits(in database.APIKeyLimits) database.APIKeyLimits {
	clean := func(items []string) []string {
		if len(items) == 0 {
			return nil
		}
		seen := make(map[string]struct{}, len(items))
		out := make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			lower := strings.ToLower(item)
			if _, ok := seen[lower]; ok {
				continue
			}
			seen[lower] = struct{}{}
			out = append(out, item)
		}
		return out
	}
	out := database.APIKeyLimits{
		ModelAllow:             clean(in.ModelAllow),
		ModelDeny:              clean(in.ModelDeny),
		PlanAllow:              cleanPlanAllow(in.PlanAllow),
		NoAffinityGroupIDs:     dedupeInt64(in.NoAffinityGroupIDs),
		RPM:                    maxInt(in.RPM, 0),
		RPD:                    maxInt(in.RPD, 0),
		MaxConcurrency:         maxInt(in.MaxConcurrency, 0),
		CostLimit5h:            maxFloat(in.CostLimit5h, 0),
		CostLimit7d:            maxFloat(in.CostLimit7d, 0),
		CostLimit30d:           maxFloat(in.CostLimit30d, 0),
		CostLimitDaily:         maxFloat(in.CostLimitDaily, 0),
		TokenLimit5h:           maxInt64(in.TokenLimit5h, 0),
		TokenLimit7d:           maxInt64(in.TokenLimit7d, 0),
		TokenLimit30d:          maxInt64(in.TokenLimit30d, 0),
		TokenLimitDaily:        maxInt64(in.TokenLimitDaily, 0),
		DisableImageGeneration: in.DisableImageGeneration,
		ImageGenerationPolicy:  sanitizeImageGenerationPolicy(in),
		AutoCompactOnOverflow:  in.AutoCompactOnOverflow,
		AllowLive:              in.AllowLive,
		ExternalScheduler:      in.ExternalScheduler,
		UpstreamChannel:        in.ResolveUpstreamChannel(),
		ScopeLimits:            database.NormalizeAPIKeyScopeLimits(in.ScopeLimits),
		ModelRequestLimits:     in.ModelRequestLimits,
	}
	// After normalization, the legacy bool and the new policy stay consistent, avoiding configuration drift between the two.
	out.DisableImageGeneration = out.ImageGenerationPolicy == database.ImageGenerationPolicyBlock
	if out.ImageGenerationPolicy == database.ImageGenerationPolicyAllow {
		out.ImageGenerationPolicy = ""
	}
	return out
}

func (h *Handler) validateAPIKeyGroupIDs(ctx context.Context, groupIDs []int64, field string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	missing, err := h.db.VerifyAccountGroupIDs(ctx, groupIDs)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s contains group IDs that do not exist: %s", field, joinInt64s(missing))
	}
	return nil
}

// validateAPIKeyScopeLimits validates that the scopes pointed to by group- and account-level limits really exist (issue #439).
// Groups are looked up in the DB; accounts are looked up in the runtime account pool (accounts in the recycle bin are treated as non-existent). Pointing at a wrong ID would make the limit
// never trigger, so we return 400 here directly instead of silently discarding it.
func (h *Handler) validateAPIKeyScopeLimits(ctx context.Context, scopes []database.APIKeyScopeLimit) error {
	if len(scopes) == 0 {
		return nil
	}
	groupIDs := make([]int64, 0, len(scopes))
	accountIDs := make([]int64, 0, len(scopes))
	for _, scope := range scopes {
		if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
			accountIDs = append(accountIDs, scope.ScopeID)
			continue
		}
		groupIDs = append(groupIDs, scope.ScopeID)
	}
	if len(groupIDs) > 0 {
		missing, err := h.db.VerifyAccountGroupIDs(ctx, groupIDs)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("limits.scope_limits contains group IDs that do not exist: %s", joinInt64s(missing))
		}
	}
	if len(accountIDs) > 0 && h.store != nil {
		missing := make([]int64, 0)
		for _, id := range accountIDs {
			if h.store.FindByID(id) == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("limits.scope_limits contains account IDs that do not exist: %s", joinInt64s(missing))
		}
	}
	return nil
}

func joinInt64s(values []int64) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatInt(value, 10))
	}
	return strings.Join(parts, ", ")
}

// sanitizeImageGenerationPolicy normalizes the values of the image tool policy (allow/strip/block), and stays compatible with the old
// DisableImageGeneration bool: an explicit policy takes precedence; if it is unset, bool=true is treated as block.
func sanitizeImageGenerationPolicy(in database.APIKeyLimits) string {
	switch strings.ToLower(strings.TrimSpace(in.ImageGenerationPolicy)) {
	case database.ImageGenerationPolicyStrip:
		return database.ImageGenerationPolicyStrip
	case database.ImageGenerationPolicyBlock:
		return database.ImageGenerationPolicyBlock
	case database.ImageGenerationPolicyAllow:
		return database.ImageGenerationPolicyAllow
	}
	if in.DisableImageGeneration {
		return database.ImageGenerationPolicyBlock
	}
	return database.ImageGenerationPolicyAllow
}

// knownAPIKeyPlanFilters is the set of values allowed by the account plan allowlist. It stays consistent with the
// options of the frontend PlanMultiSelect and with the plan filter on the Accounts page (exact match on the raw plan_type; pro and prolite/promax
// are independent of each other). Unknown values are dropped in cleanPlanAllow, so that a typo does not get written into the filter condition and
// leave the key unable to ever select any account.
var knownAPIKeyPlanFilters = map[string]struct{}{
	"free": {}, "plus": {}, "pro": {}, "prolite": {}, "promax": {}, "team": {}, "k12": {}, "go": {},
	"enterprise": {}, "business": {},
}

// cleanPlanAllow normalizes the account plan allowlist: lowercase, strip whitespace, drop unknown values, and deduplicate.
func cleanPlanAllow(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		plan := strings.ToLower(strings.TrimSpace(item))
		if plan == "" {
			continue
		}
		if _, ok := knownAPIKeyPlanFilters[plan]; !ok {
			continue
		}
		if _, ok := seen[plan]; ok {
			continue
		}
		seen[plan] = struct{}{}
		out = append(out, plan)
	}
	return out
}

func maxInt(v, lo int) int {
	if v < lo {
		return lo
	}
	return v
}

func maxInt64(v, lo int64) int64 {
	if v < lo {
		return lo
	}
	return v
}

func maxFloat(v, lo float64) float64 {
	if v < lo {
		return lo
	}
	return v
}

func parseOptionalAPIKeyQuota(quotaLimitRaw, quotaRaw json.RawMessage) (float64, bool, error) {
	raw := quotaLimitRaw
	if len(raw) == 0 {
		raw = quotaRaw
	}
	if len(raw) == 0 {
		return 0, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, true, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, fmt.Errorf("quota limit must be a number")
	}
	if value < 0 {
		return 0, true, fmt.Errorf("Quota limit cannot be less than 0")
	}
	return value, true, nil
}

func parseOptionalAPIKeyExpiration(raw json.RawMessage, expiresInDays *int) (sql.NullTime, bool, error) {
	if expiresInDays != nil {
		expiresAt, err := parseAPIKeyExpiresAt("", expiresInDays)
		return expiresAt, true, err
	}
	if len(raw) == 0 {
		return sql.NullTime{}, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return sql.NullTime{}, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return sql.NullTime{}, true, fmt.Errorf("expiry time has an invalid format")
	}
	expiresAt, err := parseAPIKeyExpiresAt(value, nil)
	return expiresAt, true, err
}

func parseAPIKeyExpiresAt(raw string, expiresInDays *int) (sql.NullTime, error) {
	if expiresInDays != nil {
		if *expiresInDays < 0 {
			return sql.NullTime{}, fmt.Errorf("expiry days must not be less than 0")
		}
		if *expiresInDays > 0 {
			if *expiresInDays > 3650 {
				return sql.NullTime{}, fmt.Errorf("expiry days must not exceed 3650")
			}
			return sql.NullTime{Time: time.Now().AddDate(0, 0, *expiresInDays), Valid: true}, nil
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sql.NullTime{}, nil
	}
	layouts := []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}
	var parsed time.Time
	var err error
	for _, layout := range layouts {
		if layout == time.RFC3339 {
			parsed, err = time.Parse(layout, raw)
		} else {
			parsed, err = time.ParseInLocation(layout, raw, time.Local)
		}
		if err == nil {
			if layout == "2006-01-02" {
				parsed = parsed.Add(24*time.Hour - time.Nanosecond)
			}
			if !parsed.After(time.Now()) {
				return sql.NullTime{}, fmt.Errorf("expiry time must be later than the current time")
			}
			return sql.NullTime{Time: parsed, Valid: true}, nil
		}
	}
	return sql.NullTime{}, fmt.Errorf("expiry time has an invalid format")
}

// DeleteAPIKey deletes an API key
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	keyToInvalidate := ""
	if row, err := h.db.GetAPIKeyByID(ctx, id); err == nil && row != nil {
		keyToInvalidate = row.Key
	}
	if err := h.db.DeleteAPIKey(ctx, id); err != nil {
		writeError(c, http.StatusInternalServerError, "Deletion failed: "+err.Error())
		return
	}
	if h.store != nil {
		h.store.SetAPIKeyAllowedGroups(id, nil)
		h.store.SetAPIKeyNoAffinityGroups(id, nil)
		h.store.SetAPIKeyAllowedPlans(id, nil)
		h.store.RemovePromptFilterNewAPIBinding(id)
	}
	h.invalidateAPIKeyRuntimeCaches(ctx, keyToInvalidate)
	writeMessage(c, http.StatusOK, "Deleted")
}

// ==================== Settings ====================

type settingsResponse struct {
	SiteName                            string `json:"site_name"`
	SiteLogo                            string `json:"site_logo"`
	BackgroundImage                     string `json:"background_image"`
	BackgroundOpacity                   int    `json:"background_opacity"`
	BackgroundBlur                      int    `json:"background_blur"`
	BackgroundGlassOpacity              int    `json:"background_glass_opacity"`
	BackgroundGlassBlur                 int    `json:"background_glass_blur"`
	MaxConcurrency                      int    `json:"max_concurrency"`
	GlobalRPM                           int    `json:"global_rpm"`
	TestModel                           string `json:"test_model"`
	CodexImagesMainModel                string `json:"codex_images_main_model"`
	CodexImagesDefaultMainModel         string `json:"codex_images_default_main_model"`
	TestContent                         string `json:"test_content"`
	TestConcurrency                     int    `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes    int    `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes             int    `json:"usage_probe_max_age_minutes"`
	UsageProbeConcurrency               int    `json:"usage_probe_concurrency"`
	UsageProbeResponsesFallbackEnabled  bool   `json:"usage_probe_responses_fallback_enabled"`
	RecoveryProbeIntervalMinutes        int    `json:"recovery_probe_interval_minutes"`
	LazyMode                            bool   `json:"lazy_mode"`
	CodexOAuthKeepaliveEnabled          bool   `json:"codex_oauth_keepalive_enabled"`
	ProxyURL                            string `json:"proxy_url"`
	PgMaxConns                          int    `json:"pg_max_conns"`
	RedisPoolSize                       int    `json:"redis_pool_size"`
	AutoCleanUnauthorized               bool   `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited                bool   `json:"auto_clean_rate_limited"`
	AdminSecret                         string `json:"admin_secret"`
	AdminAuthSource                     string `json:"admin_auth_source"`
	AutoCleanFullUsage                  bool   `json:"auto_clean_full_usage"`
	AutoCleanError                      bool   `json:"auto_clean_error"`
	AutoCleanExpired                    bool   `json:"auto_clean_expired"`
	AutoResetCreditsEnabled             bool   `json:"auto_reset_credits_enabled"`
	AutoResetCreditsOnExhaustionEnabled bool   `json:"auto_reset_credits_on_exhaustion_enabled"`
	AutoResetCreditsBeforeExpiryMin     int    `json:"auto_reset_credits_before_expiry_min"`
	AutoActivate5hWindowEnabled         bool   `json:"auto_activate_5h_window_enabled"`
	ProxyPoolEnabled                    bool   `json:"proxy_pool_enabled"`
	FastSchedulerEnabled                bool   `json:"fast_scheduler_enabled"`
	SchedulerEngine                     string `json:"scheduler_engine"`
	CodexForceWebsocket                 bool   `json:"codex_force_websocket"`
	CodexRequestCompression             bool   `json:"codex_request_compression"`
	CodexWSWeakNetworkMode              bool   `json:"codex_ws_weak_network_mode"`
	CodexWSKeepaliveEnabled             bool   `json:"codex_ws_keepalive_enabled"`
	CodexWSKeepaliveIntervalSec         int    `json:"codex_ws_keepalive_interval_sec"`
	CodexWSHideUpstreamErrors           bool   `json:"codex_ws_hide_upstream_errors"`
	CodexWSSilentRetryEnabled           bool   `json:"codex_ws_silent_retry_enabled"`
	CodexWSSilentMaxRetries             int    `json:"codex_ws_silent_max_retries"`
	CodexWSSizeRouterEnabled            bool   `json:"codex_ws_size_router_enabled"`
	CodexWSBusyAcquireMaxWaitSec        int    `json:"codex_ws_busy_acquire_max_wait_sec"`
	CodexWSBusyOverflowEnabled          bool   `json:"codex_ws_busy_overflow_enabled"`
	CodexWSBusyPatienceSec              int    `json:"codex_ws_busy_patience_sec"`
	CodexWSStatelessSlots               int    `json:"codex_ws_stateless_slots"`
	CodexWSDownstreamKeepaliveSlots     int    `json:"codex_ws_downstream_keepalive_slots"`
	GithubTokenConfigured               bool   `json:"github_token_configured"`
	GithubProxyURL                      string `json:"github_proxy_url"`
	CodexOverloadPauseEnabled           bool   `json:"codex_overload_pause_enabled"`
	CodexOverloadThresholdPercent       int    `json:"codex_overload_threshold_percent"`
	CodexOverloadPauseMinutes           int    `json:"codex_overload_pause_minutes"`
	CodexOverloadWindowMinutes          int    `json:"codex_overload_window_minutes"`
	OverflowAutoCompactEnabled          bool   `json:"overflow_auto_compact_enabled"`
	CompactViaResponsesEnabled          bool   `json:"compact_via_responses_enabled"`
	CodexPreflightSSEPassthroughEnabled bool   `json:"codex_preflight_sse_passthrough_enabled"`
	FirstTokenExcludesWsAcquire         bool   `json:"first_token_excludes_ws_acquire"`
	CodexContinueThinkingEnabled        bool   `json:"codex_continue_thinking_enabled"`
	CodexContinueMaxRounds              int    `json:"codex_continue_max_rounds"`
	UTLSShutdownTimeoutMinutes          int    `json:"utls_shutdown_timeout_minutes"`
	CodexCLIVersionSyncEnabled          bool   `json:"codex_cli_version_sync_enabled"`
	CodexCLIVersionSyncIntervalHours    int    `json:"codex_cli_version_sync_interval_hours"`
	CodexSyncedCLIVersion               string `json:"codex_synced_cli_version"`
	CodexSyncedDesktopMacBuild          string `json:"codex_synced_desktop_mac_build"`
	CodexSyncedDesktopWindowsBuild      string `json:"codex_synced_desktop_windows_build"`
	CodexSyncedVSCodeBuild              string `json:"codex_synced_vscode_build"`

	CodexClientVersions []proxy.CodexClientVersionTarget `json:"codex_client_versions"`

	// CodexEffectiveCLIVersion is the version actually used for the outbound UA right now (the larger of the built-in constant and the synced value),
	// for the settings page's "set as synced version" button: the synced value may be stale or empty, so the built-in value is the floor.
	CodexEffectiveCLIVersion          string   `json:"codex_effective_cli_version"`
	SchedulerMode                     string   `json:"scheduler_mode"`
	AffinityMode                      string   `json:"affinity_mode"`
	SessionAffinitySpread             bool     `json:"session_affinity_spread"`
	SessionSlotBufferEnabled          bool     `json:"session_slot_buffer_enabled"`
	SessionSlotBufferSeconds          int      `json:"session_slot_buffer_seconds"`
	MaxRetries                        int      `json:"max_retries"`
	MaxRateLimitRetries               int      `json:"max_rate_limit_retries"`
	RetryIntervalMS                   int      `json:"retry_interval_ms"`
	TransportRetryPolicy              string   `json:"transport_retry_policy"`
	ContinuousRetryEnabled            bool     `json:"continuous_retry_enabled"`
	ContinuousRetryCatchAll           bool     `json:"continuous_retry_catch_all"`
	ContinuousRetryCategories         []string `json:"continuous_retry_categories"`
	ContinuousRetryStatusCodes        []int    `json:"continuous_retry_status_codes"`
	ContinuousRetryErrorCodes         []string `json:"continuous_retry_error_codes"`
	ContinuousRetryMaxDurationSeconds int      `json:"continuous_retry_max_duration_seconds"`
	CodexFingerprintDefaultMode       string   `json:"codex_fingerprint_default_mode"`
	AllowRemoteMigration              bool     `json:"allow_remote_migration"`
	DatabaseDriver                    string   `json:"database_driver"`
	DatabaseLabel                     string   `json:"database_label"`
	CacheDriver                       string   `json:"cache_driver"`
	CacheLabel                        string   `json:"cache_label"`
	ExpiredCleaned                    int      `json:"expired_cleaned,omitempty"`
	ModelMapping                      string   `json:"model_mapping"`
	CodexModelMapping                 string   `json:"codex_model_mapping"`
	PayloadRules                      string   `json:"payload_rules"`
	ReasoningEffortModels             string   `json:"reasoning_effort_models"`
	ResinURL                          string   `json:"resin_url"`
	ResinPlatformName                 string   `json:"resin_platform_name"`
	// CodexEgress is the backend's authoritative summary of who currently bears the Codex channel's outbound traffic: when Resin is enabled, the proxy pool and
	// proxy_url have no effect on Codex; the UI labels this accordingly, so that three configurations do not coexist with no visible sign of which one is in effect (issue #679).
	CodexEgress                        proxy.CodexEgressSummary         `json:"codex_egress"`
	PromptFilterEnabled                bool                             `json:"prompt_filter_enabled"`
	PromptFilterMode                   string                           `json:"prompt_filter_mode"`
	PromptFilterThreshold              int                              `json:"prompt_filter_threshold"`
	PromptFilterStrictThreshold        int                              `json:"prompt_filter_strict_threshold"`
	PromptFilterStrictTerminalEnabled  bool                             `json:"prompt_filter_strict_terminal_enabled"`
	PromptFilterAdvancedConfig         string                           `json:"prompt_filter_advanced_config"`
	PromptFilterLogMatches             bool                             `json:"prompt_filter_log_matches"`
	PromptFilterMaxTextLength          int                              `json:"prompt_filter_max_text_length"`
	PromptFilterSensitiveWords         string                           `json:"prompt_filter_sensitive_words"`
	PromptFilterCustomPatterns         string                           `json:"prompt_filter_custom_patterns"`
	PromptFilterPatternQuarantines     []promptfilter.PatternQuarantine `json:"prompt_filter_pattern_quarantines,omitempty"`
	PromptFilterDisabledPatterns       string                           `json:"prompt_filter_disabled_patterns"`
	PromptFilterReviewEnabled          bool                             `json:"prompt_filter_review_enabled"`
	PromptFilterReviewAPIKeyConfigured bool                             `json:"prompt_filter_review_api_key_configured"`
	PromptFilterReviewAPIKeyCount      int                              `json:"prompt_filter_review_api_key_count"`
	PromptFilterReviewBaseURL          string                           `json:"prompt_filter_review_base_url"`
	PromptFilterReviewModel            string                           `json:"prompt_filter_review_model"`
	PromptFilterReviewTimeoutSeconds   int                              `json:"prompt_filter_review_timeout_seconds"`
	PromptFilterReviewFailClosed       bool                             `json:"prompt_filter_review_fail_closed"`
	ClientCompatMode                   string                           `json:"client_compat_mode"`
	CodexMinCLIVersion                 string                           `json:"codex_min_cli_version"`
	CodexUserAgentConfig               string                           `json:"codex_user_agent_config"`
	CodexTelemetryEnabled              bool                             `json:"codex_telemetry_enabled"`
	CodexTelemetryTimingDebug          bool                             `json:"codex_telemetry_timing_debug"`
	UsageLogMode                       string                           `json:"usage_log_mode"`
	UsageLogBatchSize                  int                              `json:"usage_log_batch_size"`
	UsageLogFlushIntervalSeconds       int                              `json:"usage_log_flush_interval_seconds"`
	StreamFlushPolicy                  string                           `json:"stream_flush_policy"`
	StreamFlushIntervalMS              int                              `json:"stream_flush_interval_ms"`
	FirstTokenMode                     string                           `json:"first_token_mode"`
	FirstTokenTimeoutSeconds           int                              `json:"first_token_timeout_seconds"`
	BillingTierPolicy                  string                           `json:"billing_tier_policy"`
	ModelsListReadMaxBytes             int64                            `json:"models_list_read_max_bytes"`
	ShowFullUsageNumbers               bool                             `json:"show_full_usage_numbers"`
	ShowUpstreamModelMismatch          bool                             `json:"show_upstream_model_mismatch"`
	PublicKeyUsagePageEnabled          bool                             `json:"public_key_usage_page_enabled"`
	PublicImageStudioPageEnabled       bool                             `json:"public_image_studio_page_enabled"`
	PublicAccountPortalPageEnabled     bool                             `json:"public_account_portal_page_enabled"`
	ImageStorageBackend                string                           `json:"image_storage_backend"`
	ImageS3Endpoint                    string                           `json:"image_s3_endpoint"`
	ImageS3Region                      string                           `json:"image_s3_region"`
	ImageS3Bucket                      string                           `json:"image_s3_bucket"`
	ImageS3AccessKey                   string                           `json:"image_s3_access_key"`
	ImageS3SecretKey                   string                           `json:"image_s3_secret_key"`
	ImageS3Prefix                      string                           `json:"image_s3_prefix"`
	ImageS3ForcePathStyle              bool                             `json:"image_s3_force_path_style"`
	AutoPause5hThreshold               float64                          `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold               float64                          `json:"auto_pause_7d_threshold"`
	AutoPause5hGuardBandPercent        float64                          `json:"auto_pause_5h_guard_band_percent"`
	AutoPause5hGuardConcurrency        int                              `json:"auto_pause_5h_guard_concurrency"`
	SmartPacingEnabled                 bool                             `json:"smart_pacing_enabled"`
	SmartPacingMinConcurrency          int                              `json:"smart_pacing_min_concurrency"`
	SmartPacingWindows                 string                           `json:"smart_pacing_windows"`
	IgnoreUsageLimitStatus             bool                             `json:"ignore_usage_limit_status"`
	ResponseCacheLocalMaxBytes         int64                            `json:"response_cache_local_max_bytes"`
	ResponseCacheLocalMaxEntryBytes    int64                            `json:"response_cache_local_max_entry_bytes"`
	ResponseCacheReconstructMaxBytes   int64                            `json:"response_cache_reconstruct_max_bytes"`
	ResponseCacheWritePolicy           string                           `json:"response_cache_write_policy"`
	ResponseCacheConfigGeneration      int64                            `json:"response_cache_config_generation"`
	RelayModelCooldownMode             string                           `json:"relay_model_cooldown_mode"`
	RelayModelCooldownSeconds          int                              `json:"relay_model_cooldown_seconds"`
	RelayModelCooldownBackoffEnabled   bool                             `json:"relay_model_cooldown_backoff_enabled"`
	OAuthModelCooldownMode             string                           `json:"oauth_model_cooldown_mode"`
	OAuthModelCooldownSeconds          int                              `json:"oauth_model_cooldown_seconds"`
	OAuthModelCooldownBackoffEnabled   bool                             `json:"oauth_model_cooldown_backoff_enabled"`
	OAuthUnsupportedCooldownSeconds    int                              `json:"oauth_unsupported_model_cooldown_seconds"`
	OutputGuardEnabled                 bool                             `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes    int64                            `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas   int                              `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes         int64                            `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes         int64                            `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration        int64                            `json:"output_guard_config_generation"`
}

type rawJSON = json.RawMessage

type updateSettingsReq struct {
	SiteName                            *string   `json:"site_name"`
	SiteLogo                            *string   `json:"site_logo"`
	BackgroundImage                     *string   `json:"background_image"`
	BackgroundOpacity                   *int      `json:"background_opacity"`
	BackgroundBlur                      *int      `json:"background_blur"`
	BackgroundGlassOpacity              *int      `json:"background_glass_opacity"`
	BackgroundGlassBlur                 *int      `json:"background_glass_blur"`
	MaxConcurrency                      *int      `json:"max_concurrency"`
	GlobalRPM                           *int      `json:"global_rpm"`
	TestModel                           *string   `json:"test_model"`
	CodexImagesMainModel                *string   `json:"codex_images_main_model"`
	TestContent                         *string   `json:"test_content"`
	TestConcurrency                     *int      `json:"test_concurrency"`
	BackgroundRefreshIntervalMinutes    *int      `json:"background_refresh_interval_minutes"`
	UsageProbeMaxAgeMinutes             *int      `json:"usage_probe_max_age_minutes"`
	UsageProbeConcurrency               *int      `json:"usage_probe_concurrency"`
	UsageProbeResponsesFallbackEnabled  *bool     `json:"usage_probe_responses_fallback_enabled"`
	RecoveryProbeIntervalMinutes        *int      `json:"recovery_probe_interval_minutes"`
	LazyMode                            *bool     `json:"lazy_mode"`
	CodexOAuthKeepaliveEnabled          *bool     `json:"codex_oauth_keepalive_enabled"`
	ProxyURL                            *string   `json:"proxy_url"`
	PgMaxConns                          *int      `json:"pg_max_conns"`
	RedisPoolSize                       *int      `json:"redis_pool_size"`
	AutoCleanUnauthorized               *bool     `json:"auto_clean_unauthorized"`
	AutoCleanRateLimited                *bool     `json:"auto_clean_rate_limited"`
	AdminSecret                         *string   `json:"admin_secret"`
	AutoCleanFullUsage                  *bool     `json:"auto_clean_full_usage"`
	AutoCleanError                      *bool     `json:"auto_clean_error"`
	AutoCleanExpired                    *bool     `json:"auto_clean_expired"`
	AutoResetCreditsEnabled             *bool     `json:"auto_reset_credits_enabled"`
	AutoResetCreditsOnExhaustionEnabled *bool     `json:"auto_reset_credits_on_exhaustion_enabled"`
	AutoResetCreditsBeforeExpiryMin     *int      `json:"auto_reset_credits_before_expiry_min"`
	AutoActivate5hWindowEnabled         *bool     `json:"auto_activate_5h_window_enabled"`
	ProxyPoolEnabled                    *bool     `json:"proxy_pool_enabled"`
	FastSchedulerEnabled                *bool     `json:"fast_scheduler_enabled"`
	SchedulerEngine                     *string   `json:"scheduler_engine"`
	CodexForceWebsocket                 *bool     `json:"codex_force_websocket"`
	CodexRequestCompression             *bool     `json:"codex_request_compression"`
	CodexWSWeakNetworkMode              *bool     `json:"codex_ws_weak_network_mode"`
	CodexWSKeepaliveEnabled             *bool     `json:"codex_ws_keepalive_enabled"`
	CodexWSKeepaliveIntervalSec         *int      `json:"codex_ws_keepalive_interval_sec"`
	CodexWSHideUpstreamErrors           *bool     `json:"codex_ws_hide_upstream_errors"`
	CodexWSSilentRetryEnabled           *bool     `json:"codex_ws_silent_retry_enabled"`
	CodexWSSilentMaxRetries             *int      `json:"codex_ws_silent_max_retries"`
	CodexWSSizeRouterEnabled            *bool     `json:"codex_ws_size_router_enabled"`
	CodexWSBusyAcquireMaxWaitSec        *int      `json:"codex_ws_busy_acquire_max_wait_sec"`
	CodexWSBusyOverflowEnabled          *bool     `json:"codex_ws_busy_overflow_enabled"`
	CodexWSBusyPatienceSec              *int      `json:"codex_ws_busy_patience_sec"`
	CodexWSStatelessSlots               *int      `json:"codex_ws_stateless_slots"`
	CodexWSDownstreamKeepaliveSlots     *int      `json:"codex_ws_downstream_keepalive_slots"`
	GithubToken                         *string   `json:"github_token"`
	GithubProxyURL                      *string   `json:"github_proxy_url"`
	CodexOverloadPauseEnabled           *bool     `json:"codex_overload_pause_enabled"`
	CodexOverloadThresholdPercent       *int      `json:"codex_overload_threshold_percent"`
	CodexOverloadPauseMinutes           *int      `json:"codex_overload_pause_minutes"`
	CodexOverloadWindowMinutes          *int      `json:"codex_overload_window_minutes"`
	OverflowAutoCompactEnabled          *bool     `json:"overflow_auto_compact_enabled"`
	CompactViaResponsesEnabled          *bool     `json:"compact_via_responses_enabled"`
	CodexPreflightSSEPassthroughEnabled *bool     `json:"codex_preflight_sse_passthrough_enabled"`
	FirstTokenExcludesWsAcquire         *bool     `json:"first_token_excludes_ws_acquire"`
	CodexContinueThinkingEnabled        *bool     `json:"codex_continue_thinking_enabled"`
	CodexContinueMaxRounds              *int      `json:"codex_continue_max_rounds"`
	UTLSShutdownTimeoutMinutes          *int      `json:"utls_shutdown_timeout_minutes"`
	CodexCLIVersionSyncEnabled          *bool     `json:"codex_cli_version_sync_enabled"`
	CodexCLIVersionSyncIntervalHours    *int      `json:"codex_cli_version_sync_interval_hours"`
	SchedulerMode                       *string   `json:"scheduler_mode"`
	AffinityMode                        *string   `json:"affinity_mode"`
	SessionAffinitySpread               *bool     `json:"session_affinity_spread"`
	SessionSlotBufferEnabled            *bool     `json:"session_slot_buffer_enabled"`
	SessionSlotBufferSeconds            *int      `json:"session_slot_buffer_seconds"`
	MaxRetries                          *int      `json:"max_retries"`
	MaxRateLimitRetries                 *int      `json:"max_rate_limit_retries"`
	RetryIntervalMS                     *int      `json:"retry_interval_ms"`
	TransportRetryPolicy                *string   `json:"transport_retry_policy"`
	ContinuousRetryEnabled              *bool     `json:"continuous_retry_enabled"`
	ContinuousRetryCatchAll             *bool     `json:"continuous_retry_catch_all"`
	ContinuousRetryCategories           *[]string `json:"continuous_retry_categories"`
	ContinuousRetryStatusCodes          *[]int    `json:"continuous_retry_status_codes"`
	ContinuousRetryErrorCodes           *[]string `json:"continuous_retry_error_codes"`
	ContinuousRetryMaxDurationSeconds   *int      `json:"continuous_retry_max_duration_seconds"`
	CodexFingerprintDefaultMode         *string   `json:"codex_fingerprint_default_mode"`
	AllowRemoteMigration                *bool     `json:"allow_remote_migration"`
	ModelMapping                        *string   `json:"model_mapping"`
	CodexModelMapping                   *string   `json:"codex_model_mapping"`
	PayloadRules                        *string   `json:"payload_rules"`
	ReasoningEffortModels               *string   `json:"reasoning_effort_models"`
	ResinURL                            *string   `json:"resin_url"`
	ResinPlatformName                   *string   `json:"resin_platform_name"`
	PromptFilterEnabled                 *bool     `json:"prompt_filter_enabled"`
	PromptFilterMode                    *string   `json:"prompt_filter_mode"`
	PromptFilterThreshold               *int      `json:"prompt_filter_threshold"`
	PromptFilterStrictThreshold         *int      `json:"prompt_filter_strict_threshold"`
	PromptFilterStrictTerminalEnabled   *bool     `json:"prompt_filter_strict_terminal_enabled"`
	PromptFilterAdvancedConfig          *string   `json:"prompt_filter_advanced_config"`
	PromptFilterLogMatches              *bool     `json:"prompt_filter_log_matches"`
	PromptFilterMaxTextLength           *int      `json:"prompt_filter_max_text_length"`
	PromptFilterSensitiveWords          *string   `json:"prompt_filter_sensitive_words"`
	PromptFilterCustomPatterns          *string   `json:"prompt_filter_custom_patterns"`
	PromptFilterCustomPatternsExpected  *string   `json:"prompt_filter_custom_patterns_expected"`
	PromptFilterDisabledPatterns        *string   `json:"prompt_filter_disabled_patterns"`
	PromptFilterReviewEnabled           *bool     `json:"prompt_filter_review_enabled"`
	PromptFilterReviewAPIKey            *string   `json:"prompt_filter_review_api_key"`
	PromptFilterReviewBaseURL           *string   `json:"prompt_filter_review_base_url"`
	PromptFilterReviewModel             *string   `json:"prompt_filter_review_model"`
	PromptFilterReviewTimeoutSeconds    *int      `json:"prompt_filter_review_timeout_seconds"`
	PromptFilterReviewFailClosed        *bool     `json:"prompt_filter_review_fail_closed"`
	ClientCompatMode                    *string   `json:"client_compat_mode"`
	CodexMinCLIVersion                  *string   `json:"codex_min_cli_version"`
	CodexUserAgentConfig                *string   `json:"codex_user_agent_config"`
	CodexTelemetryEnabled               *bool     `json:"codex_telemetry_enabled"`
	CodexTelemetryTimingDebug           *bool     `json:"codex_telemetry_timing_debug"`
	UsageLogMode                        *string   `json:"usage_log_mode"`
	UsageLogBatchSize                   *int      `json:"usage_log_batch_size"`
	UsageLogFlushIntervalSeconds        *int      `json:"usage_log_flush_interval_seconds"`
	StreamFlushPolicy                   *string   `json:"stream_flush_policy"`
	StreamFlushIntervalMS               *int      `json:"stream_flush_interval_ms"`
	FirstTokenMode                      *string   `json:"first_token_mode"`
	FirstTokenTimeoutSeconds            *int      `json:"first_token_timeout_seconds"`
	BillingTierPolicy                   *string   `json:"billing_tier_policy"`
	ModelsListReadMaxBytes              *int64    `json:"models_list_read_max_bytes"`
	ShowFullUsageNumbers                *bool     `json:"show_full_usage_numbers"`
	ShowUpstreamModelMismatch           *bool     `json:"show_upstream_model_mismatch"`
	PublicKeyUsagePageEnabled           *bool     `json:"public_key_usage_page_enabled"`
	PublicImageStudioPageEnabled        *bool     `json:"public_image_studio_page_enabled"`
	PublicAccountPortalPageEnabled      *bool     `json:"public_account_portal_page_enabled"`
	ImageStorageBackend                 *string   `json:"image_storage_backend"`
	ImageS3Endpoint                     *string   `json:"image_s3_endpoint"`
	ImageS3Region                       *string   `json:"image_s3_region"`
	ImageS3Bucket                       *string   `json:"image_s3_bucket"`
	ImageS3AccessKey                    *string   `json:"image_s3_access_key"`
	ImageS3SecretKey                    *string   `json:"image_s3_secret_key"`
	ImageS3Prefix                       *string   `json:"image_s3_prefix"`
	ImageS3ForcePathStyle               *bool     `json:"image_s3_force_path_style"`
	AutoPause5hThreshold                *float64  `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold                *float64  `json:"auto_pause_7d_threshold"`
	AutoPause5hGuardBandPercent         *float64  `json:"auto_pause_5h_guard_band_percent"`
	AutoPause5hGuardConcurrency         *int      `json:"auto_pause_5h_guard_concurrency"`
	SmartPacingEnabled                  *bool     `json:"smart_pacing_enabled"`
	SmartPacingMinConcurrency           *int      `json:"smart_pacing_min_concurrency"`
	SmartPacingWindows                  *string   `json:"smart_pacing_windows"`
	IgnoreUsageLimitStatus              *bool     `json:"ignore_usage_limit_status"`
	ResponseCacheLocalMaxBytes          *int64    `json:"response_cache_local_max_bytes"`
	ResponseCacheLocalMaxEntryBytes     *int64    `json:"response_cache_local_max_entry_bytes"`
	ResponseCacheReconstructMaxBytes    *int64    `json:"response_cache_reconstruct_max_bytes"`
	ResponseCacheWritePolicy            *string   `json:"response_cache_write_policy"`
	ResponseCacheConfigGeneration       rawJSON   `json:"response_cache_config_generation"`
	RelayModelCooldownMode              *string   `json:"relay_model_cooldown_mode"`
	RelayModelCooldownSeconds           *int      `json:"relay_model_cooldown_seconds"`
	RelayModelCooldownBackoffEnabled    *bool     `json:"relay_model_cooldown_backoff_enabled"`
	OAuthModelCooldownMode              *string   `json:"oauth_model_cooldown_mode"`
	OAuthModelCooldownSeconds           *int      `json:"oauth_model_cooldown_seconds"`
	OAuthModelCooldownBackoffEnabled    *bool     `json:"oauth_model_cooldown_backoff_enabled"`
	OAuthUnsupportedCooldownSeconds     *int      `json:"oauth_unsupported_model_cooldown_seconds"`
	OutputGuardEnabled                  *bool     `json:"output_guard_enabled"`
	OutputGuardMaxToolArgumentBytes     *int64    `json:"output_guard_max_tool_argument_bytes"`
	OutputGuardMaxToolArgumentDeltas    *int      `json:"output_guard_max_tool_argument_deltas"`
	OutputGuardMaxChannelBytes          *int64    `json:"output_guard_max_channel_bytes"`
	OutputGuardMaxRequestBytes          *int64    `json:"output_guard_max_request_bytes"`
	OutputGuardConfigGeneration         rawJSON   `json:"output_guard_config_generation"`
}

func updateSettingsHasFieldsOtherThanCustomPatterns(req updateSettingsReq) bool {
	value := reflect.ValueOf(req)
	typeOf := value.Type()
	for index := 0; index < value.NumField(); index++ {
		name := typeOf.Field(index).Name
		if name == "PromptFilterCustomPatterns" || name == "PromptFilterCustomPatternsExpected" {
			continue
		}
		if !value.Field(index).IsZero() {
			return true
		}
	}
	return false
}

type brandingResponse struct {
	SiteName               string `json:"site_name"`
	SiteLogo               string `json:"site_logo"`
	BackgroundImage        string `json:"background_image"`
	BackgroundOpacity      int    `json:"background_opacity"`
	BackgroundBlur         int    `json:"background_blur"`
	BackgroundGlassOpacity int    `json:"background_glass_opacity"`
	BackgroundGlassBlur    int    `json:"background_glass_blur"`
}

const maxSiteLogoBytes = 600 * 1024
const maxBackgroundImageBytes = 2 * 1024 * 1024
const maxBackgroundVideoBytes = 40 * 1024 * 1024
const maxBackgroundImageAssetUploadBytes = 20 * 1024 * 1024
const maxBackgroundVideoAssetUploadBytes = 40 * 1024 * 1024
const maxBackgroundAssetUploadBytes = maxBackgroundVideoAssetUploadBytes
const maxSiteLogoURLChars = 4096
const maxBackgroundImageURLChars = 20000
const defaultBackgroundOpacity = 18
const maxBackgroundBlur = 24
const defaultBackgroundGlassOpacity = 58
const defaultBackgroundGlassBlur = 5
const maxBackgroundGlassBlur = 20
const defaultBackgroundAssetDir = "/data/backgrounds"
const backgroundAssetURLPrefix = "/p/backgrounds/"

type brandingBackgroundConfig struct {
	Image        string `json:"image"`
	Opacity      int    `json:"opacity"`
	Blur         int    `json:"blur"`
	GlassOpacity int    `json:"glass_opacity"`
	GlassBlur    int    `json:"glass_blur"`
}

type backgroundAssetUploadResponse struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Bytes    int    `json:"bytes"`
}

func normalizeSiteLogo(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:image/") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("site icon data URL format is invalid")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("site icon base64 data is invalid")
		}
		if len(decoded) > maxSiteLogoBytes {
			return "", fmt.Errorf("site icon must not exceed 600KB")
		}
		return value, nil
	case strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"):
		if len(value) > maxSiteLogoURLChars {
			return "", fmt.Errorf("site icon URL is too long")
		}
		return value, nil
	case strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//"):
		if len(value) > maxSiteLogoURLChars {
			return "", fmt.Errorf("site icon path is too long")
		}
		return value, nil
	default:
		return "", fmt.Errorf("site icon only supports http(s) URLs, on-site paths, or data:image base64")
	}
}

func normalizeBackgroundImage(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:image/") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("background image data URL format is invalid")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("background image base64 data is invalid")
		}
		if len(decoded) > maxBackgroundImageBytes {
			return "", fmt.Errorf("background image must not exceed 2MB")
		}
		return value, nil
	case strings.HasPrefix(lower, "data:video/mp4") && strings.Contains(lower, ";base64,"):
		commaIndex := strings.Index(value, ",")
		if commaIndex < 0 {
			return "", fmt.Errorf("animated wallpaper data URL format is invalid")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[commaIndex+1:]))
		if err != nil {
			return "", fmt.Errorf("animated wallpaper base64 data is invalid")
		}
		if len(decoded) > maxBackgroundVideoBytes {
			return "", fmt.Errorf("animated wallpaper must not exceed 40MB")
		}
		return value, nil
	case strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://"):
		if len(value) > maxBackgroundImageURLChars {
			return "", fmt.Errorf("background image URL is too long")
		}
		return value, nil
	case strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//"):
		if len(value) > maxBackgroundImageURLChars {
			return "", fmt.Errorf("background image path is too long")
		}
		return value, nil
	default:
		return "", fmt.Errorf("background only supports http(s) URLs, on-site paths, data:image base64, or data:video/mp4 base64")
	}
}

func backgroundAssetDir() string {
	if dir := strings.TrimSpace(os.Getenv("BACKGROUND_ASSET_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("IMAGE_ASSET_DIR")); dir != "" {
		clean := filepath.Clean(dir)
		parent := filepath.Dir(clean)
		if parent != "." && parent != string(os.PathSeparator) {
			return filepath.Join(parent, "backgrounds")
		}
		return filepath.Join(clean, "backgrounds")
	}
	if dbPath := strings.TrimSpace(os.Getenv("DATABASE_PATH")); dbPath != "" {
		return filepath.Join(filepath.Dir(dbPath), "backgrounds")
	}
	return defaultBackgroundAssetDir
}

func backgroundAssetPath(filename string) (string, bool) {
	name := filepath.Base(strings.TrimSpace(filename))
	if name == "" || name == "." || name != strings.TrimSpace(filename) {
		return "", false
	}
	dir, err := filepath.Abs(backgroundAssetDir())
	if err != nil {
		return "", false
	}
	full, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func backgroundAssetURL(filename string) string {
	return backgroundAssetURLPrefix + filename
}

func validateConnectionTestContent(content string) (string, error) {
	normalized := auth.NormalizeTestContent(content)
	if len([]rune(normalized)) > auth.MaxTestContentRunes {
		return "", fmt.Errorf("test_content must not exceed %d characters", auth.MaxTestContentRunes)
	}
	return normalized, nil
}

func randomBackgroundAssetFilename(ext string) string {
	ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	if ext == "" {
		ext = "bin"
	}
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d.%s", time.Now().UnixNano(), ext)
	}
	return fmt.Sprintf("%d-%s.%s", time.Now().UnixNano(), hex.EncodeToString(b), ext)
}

func declaredBackgroundMediaType(filename, contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/svg+xml", "video/mp4":
		if contentType == "image/jpg" {
			return "image/jpeg"
		}
		return contentType
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".mp4":
		return "video/mp4"
	default:
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); byExt != "" {
			return strings.ToLower(strings.TrimSpace(strings.Split(byExt, ";")[0]))
		}
		return ""
	}
}

func looksLikeSVG(data []byte) bool {
	sample := strings.ToLower(string(data))
	return strings.Contains(sample, "<svg") && !strings.Contains(sample, "<script")
}

func looksLikeWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

func looksLikeMP4(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

func normalizeBackgroundUploadMedia(filename, contentType string, data []byte) (string, string, error) {
	if len(data) == 0 {
		return "", "", fmt.Errorf("background file is empty")
	}
	declared := declaredBackgroundMediaType(filename, contentType)
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	switch detected {
	case "image/png":
		return "image/png", "png", nil
	case "image/jpeg":
		return "image/jpeg", "jpg", nil
	case "image/webp":
		return "image/webp", "webp", nil
	}
	switch declared {
	case "image/webp":
		if looksLikeWebP(data) {
			return "image/webp", "webp", nil
		}
	case "image/svg+xml":
		if looksLikeSVG(data) {
			return "image/svg+xml", "svg", nil
		}
	case "video/mp4":
		if looksLikeMP4(data) {
			return "video/mp4", "mp4", nil
		}
	}
	return "", "", fmt.Errorf("background only supports PNG, JPG, WebP, SVG, or MP4")
}

func backgroundUploadLimitBytes(mimeType string) int {
	if mimeType == "video/mp4" {
		return maxBackgroundVideoAssetUploadBytes
	}
	return maxBackgroundImageAssetUploadBytes
}

func backgroundUploadTooLargeMessage(mimeType string) string {
	if mimeType == "video/mp4" {
		return "MP4 animated wallpaper must not exceed 40MB"
	}
	return "background image must not exceed 20MB"
}

func (h *Handler) UploadBackgroundAsset(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "Select a background file")
		return
	}
	if fh.Size <= 0 {
		writeError(c, http.StatusBadRequest, "Background file is empty")
		return
	}
	if fh.Size > maxBackgroundAssetUploadBytes {
		writeError(c, http.StatusBadRequest, "Background file must not exceed 40MB")
		return
	}
	file, err := fh.Open()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBackgroundAssetUploadBytes+1))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(data) > maxBackgroundAssetUploadBytes {
		writeError(c, http.StatusBadRequest, "Background file must not exceed 40MB")
		return
	}
	mimeType, ext, err := normalizeBackgroundUploadMedia(fh.Filename, fh.Header.Get("Content-Type"), data)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(data) > backgroundUploadLimitBytes(mimeType) {
		writeError(c, http.StatusBadRequest, backgroundUploadTooLargeMessage(mimeType))
		return
	}

	if err := os.MkdirAll(backgroundAssetDir(), 0o755); err != nil {
		writeInternalError(c, fmt.Errorf("failed to create background directory: %w", err))
		return
	}
	filename := randomBackgroundAssetFilename(ext)
	fullPath, ok := backgroundAssetPath(filename)
	if !ok {
		writeInternalError(c, fmt.Errorf("background file path is invalid"))
		return
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		writeInternalError(c, fmt.Errorf("failed to save background file: %w", err))
		return
	}

	c.JSON(http.StatusOK, backgroundAssetUploadResponse{
		URL:      backgroundAssetURL(filename),
		Filename: filename,
		MimeType: mimeType,
		Bytes:    len(data),
	})
}

func (h *Handler) GetBackgroundAssetFile(c *gin.Context) {
	fullPath, ok := backgroundAssetPath(c.Param("filename"))
	if !ok {
		writeError(c, http.StatusNotFound, "Background file does not exist")
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		writeError(c, http.StatusNotFound, "Background file does not exist")
		return
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.File(fullPath)
}

func normalizeBackgroundOpacity(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeBackgroundBlur(value int) int {
	if value < 0 {
		return 0
	}
	if value > maxBackgroundBlur {
		return maxBackgroundBlur
	}
	return value
}

func normalizeBackgroundGlassOpacity(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeBackgroundGlassBlur(value int) int {
	if value < 0 {
		return 0
	}
	if value > maxBackgroundGlassBlur {
		return maxBackgroundGlassBlur
	}
	return value
}

func normalizeBackgroundConfig(cfg brandingBackgroundConfig) brandingBackgroundConfig {
	image, err := normalizeBackgroundImage(cfg.Image)
	if err != nil {
		image = ""
	}
	opacity := normalizeBackgroundOpacity(cfg.Opacity)
	if opacity == 0 && strings.TrimSpace(image) != "" && cfg.Opacity == 0 {
		opacity = 0
	}
	return brandingBackgroundConfig{
		Image:        image,
		Opacity:      opacity,
		Blur:         normalizeBackgroundBlur(cfg.Blur),
		GlassOpacity: normalizeBackgroundGlassOpacity(cfg.GlassOpacity),
		GlassBlur:    normalizeBackgroundGlassBlur(cfg.GlassBlur),
	}
}

func defaultBackgroundConfig() brandingBackgroundConfig {
	return brandingBackgroundConfig{
		Opacity:      defaultBackgroundOpacity,
		GlassOpacity: defaultBackgroundGlassOpacity,
		GlassBlur:    defaultBackgroundGlassBlur,
	}
}

func decodeBackgroundConfig(raw string) brandingBackgroundConfig {
	cfg := defaultBackgroundConfig()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return defaultBackgroundConfig()
	}
	return normalizeBackgroundConfig(cfg)
}

func encodeBackgroundConfig(cfg brandingBackgroundConfig) string {
	cfg = normalizeBackgroundConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func brandingFromSettings(settings *database.SystemSettings) brandingResponse {
	resp := brandingResponse{SiteName: database.DefaultSiteName}
	bg := defaultBackgroundConfig()
	if settings == nil {
		resp.BackgroundOpacity = bg.Opacity
		resp.BackgroundGlassOpacity = bg.GlassOpacity
		resp.BackgroundGlassBlur = bg.GlassBlur
		return resp
	}
	resp.SiteName = database.NormalizeSiteName(settings.SiteName)
	resp.SiteLogo = strings.TrimSpace(settings.SiteLogo)
	bg = decodeBackgroundConfig(settings.BackgroundConfig)
	resp.BackgroundImage = bg.Image
	resp.BackgroundOpacity = bg.Opacity
	resp.BackgroundBlur = bg.Blur
	resp.BackgroundGlassOpacity = bg.GlassOpacity
	resp.BackgroundGlassBlur = bg.GlassBlur
	return resp
}

// GetBranding gets the public site branding configuration (no admin key required).
func (h *Handler) GetBranding(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		log.Printf("failed to read site branding configuration: %v", err)
		c.JSON(http.StatusOK, brandingFromSettings(nil))
		return
	}
	c.JSON(http.StatusOK, brandingFromSettings(settings))
}

// GetSettings gets the current system settings
// GetObservedInstructions returns the most recently observed client pass-through instructions samples,
// so that admins can see the original system prompt the client actually sent when configuring payload rewrite rules.
func (h *Handler) GetObservedInstructions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"samples": proxy.ObservedInstructions()})
}

func (h *Handler) GetSettings(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	cacheSettingsStore := h.cacheSettingsStore()
	if cacheSettingsStore == nil {
		writeError(c, http.StatusInternalServerError, "Response cache settings storage is unavailable")
		return
	}
	responseCacheSettings, err := cacheSettingsStore.GetResponseCacheSettings(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to read response cache settings: "+err.Error())
		return
	}
	outputGuardSettings, err := h.db.GetOutputGuardSettings(ctx)
	if err != nil {
		// When reading the guard settings fails, do not fall back to defaults: the proxy process may still be using the last successfully
		// applied configuration (which may have been enabled), and returning defaults would make the UI display something that does not match the actual running state.
		log.Printf("failed to read output guard settings: %v", err)
		writeError(c, http.StatusInternalServerError, "Failed to read guard settings: "+err.Error())
		return
	}
	outputGuardEnabled := false
	if outputGuardSettings.Enabled != nil {
		outputGuardEnabled = *outputGuardSettings.Enabled
	}
	dbSettings, _ := h.db.GetSystemSettings(ctx)
	_, adminAuthSource := h.resolveAdminSecret(c.Request.Context())
	adminSecret := ""
	var resinURL, resinPlatformName string
	branding := brandingFromSettings(dbSettings)
	showFullUsageNumbers := false
	showUpstreamModelMismatch := true
	publicKeyUsagePageEnabled := true
	publicImageStudioPageEnabled := true
	publicAccountPortalPageEnabled := false
	if dbSettings != nil && adminAuthSource != "env" {
		adminSecret = dbSettings.AdminSecret
	}
	if dbSettings != nil {
		resinURL = dbSettings.ResinURL
		resinPlatformName = dbSettings.ResinPlatformName
		showFullUsageNumbers = dbSettings.ShowFullUsageNumbers
		showUpstreamModelMismatch = dbSettings.ShowUpstreamModelMismatch
		publicKeyUsagePageEnabled = dbSettings.PublicKeyUsagePageEnabled
		publicImageStudioPageEnabled = dbSettings.PublicImageStudioPageEnabled
		publicAccountPortalPageEnabled = dbSettings.PublicAccountPortalPageEnabled
	}
	promptFilterCfg := h.store.GetPromptFilterConfig()
	promptFilterAdvancedRaw := h.store.GetPromptFilterAdvancedConfig()
	if dbSettings != nil {
		if document, err := promptfilter.ParseAdvancedConfigDocument(dbSettings.PromptFilterAdvancedConfig); err == nil {
			promptFilterAdvancedRaw = document.Raw
		}
	}
	runtimeCfg := proxy.CurrentRuntimeSettings()
	autoResetCreditsEnabled := runtimeCfg.AutoResetCreditsEnabled
	autoResetCreditsOnExhaustionEnabled := runtimeCfg.AutoResetCreditsOnExhaustionEnabled
	autoResetCreditsBeforeExpiryMin := runtimeCfg.AutoResetCreditsBeforeExpiryMin
	autoActivate5hWindowEnabled := runtimeCfg.AutoActivate5hWindowEnabled
	// Wait-time cap for graceful uTLS shutdown (issue #446): same as automatic consumption; the database is the authoritative source under multi-instance deployment.
	utlsShutdownTimeoutMinutes := runtimeCfg.UTLSShutdownTimeoutMin
	if dbSettings != nil {
		autoResetCreditsEnabled = dbSettings.AutoResetCreditsEnabled
		autoResetCreditsOnExhaustionEnabled = dbSettings.AutoResetCreditsOnExhaustionEnabled
		autoResetCreditsBeforeExpiryMin = dbSettings.AutoResetCreditsBeforeExpiryMin
		autoActivate5hWindowEnabled = dbSettings.AutoActivate5hWindowEnabled
		utlsShutdownTimeoutMinutes = database.NormalizeUTLSShutdownTimeoutMinutes(dbSettings.UTLSShutdownTimeoutMinutes)
	}
	imgCfg := imagestore.CurrentConfig()
	imgPrefix := strings.TrimSuffix(imgCfg.Prefix, "/")
	bgCfg := defaultBackgroundConfig()
	if dbSettings != nil {
		bgCfg = decodeBackgroundConfig(dbSettings.BackgroundConfig)
	}
	modelCooldownSettings := h.store.GetModelCooldownSettings()
	continuousRetryPolicy := h.store.GetContinuousRetryPolicy()
	c.JSON(http.StatusOK, settingsResponse{
		SiteName:                            branding.SiteName,
		SiteLogo:                            branding.SiteLogo,
		BackgroundImage:                     bgCfg.Image,
		BackgroundOpacity:                   bgCfg.Opacity,
		BackgroundBlur:                      bgCfg.Blur,
		BackgroundGlassOpacity:              bgCfg.GlassOpacity,
		BackgroundGlassBlur:                 bgCfg.GlassBlur,
		MaxConcurrency:                      h.store.GetMaxConcurrency(),
		GlobalRPM:                           h.rateLimiter.GetRPM(),
		TestModel:                           h.store.GetTestModel(),
		CodexImagesMainModel:                runtimeCfg.CodexImagesMainModel,
		CodexImagesDefaultMainModel:         proxy.ImagesDefaultMainModel(),
		TestContent:                         h.store.GetTestContent(),
		TestConcurrency:                     h.store.GetTestConcurrency(),
		ResponseCacheLocalMaxBytes:          responseCacheSettings.LocalMaxBytes,
		ResponseCacheLocalMaxEntryBytes:     responseCacheSettings.LocalMaxEntryBytes,
		ResponseCacheReconstructMaxBytes:    responseCacheSettings.ReconstructMaxBytes,
		ResponseCacheWritePolicy:            responseCacheSettings.WritePolicy,
		ResponseCacheConfigGeneration:       responseCacheSettings.Generation,
		RelayModelCooldownMode:              modelCooldownSettings.RelayMode,
		RelayModelCooldownSeconds:           modelCooldownSettings.RelaySeconds,
		RelayModelCooldownBackoffEnabled:    modelCooldownSettings.RelayBackoffEnabled,
		OAuthModelCooldownMode:              modelCooldownSettings.OAuthMode,
		OAuthModelCooldownSeconds:           modelCooldownSettings.OAuthSeconds,
		OAuthModelCooldownBackoffEnabled:    modelCooldownSettings.OAuthBackoffEnabled,
		OAuthUnsupportedCooldownSeconds:     modelCooldownSettings.UnsupportedCooldownSeconds(),
		BackgroundRefreshIntervalMinutes:    h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:             h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:               h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled:  h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:        h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                            h.store.GetLazyMode(),
		CodexOAuthKeepaliveEnabled:          h.store.GetCodexOAuthKeepalive(),
		ProxyURL:                            h.store.GetProxyURL(),
		PgMaxConns:                          h.pgMaxConns,
		RedisPoolSize:                       h.redisPoolSize,
		AutoCleanUnauthorized:               h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:                h.store.GetAutoCleanRateLimited(),
		AdminSecret:                         adminSecret,
		AdminAuthSource:                     adminAuthSource,
		AutoCleanFullUsage:                  h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                      h.store.GetAutoCleanError(),
		AutoCleanExpired:                    h.store.GetAutoCleanExpired(),
		AutoResetCreditsEnabled:             autoResetCreditsEnabled,
		AutoResetCreditsOnExhaustionEnabled: autoResetCreditsOnExhaustionEnabled,
		AutoResetCreditsBeforeExpiryMin:     autoResetCreditsBeforeExpiryMin,
		AutoActivate5hWindowEnabled:         autoActivate5hWindowEnabled,
		ProxyPoolEnabled:                    h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:                h.store.FastSchedulerEnabled(),
		SchedulerEngine:                     h.store.SchedulerEngine(),
		CodexForceWebsocket:                 h.store.CodexForceWebsocket(),
		CodexRequestCompression:             h.store.CodexRequestCompression(),
		CodexWSWeakNetworkMode:              runtimeCfg.CodexWSWeakNetworkMode,
		CodexWSKeepaliveEnabled:             h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:         h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:           h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:           h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:             h.store.CodexWSSilentMaxRetries(),
		CodexWSSizeRouterEnabled:            h.store.CodexWSSizeRouterEnabled(),
		CodexWSBusyAcquireMaxWaitSec:        h.store.CodexWSBusyAcquireMaxWaitSec(),
		CodexWSBusyOverflowEnabled:          h.store.CodexWSBusyOverflowEnabled(),
		CodexWSBusyPatienceSec:              h.store.CodexWSBusyPatienceSec(),
		CodexWSStatelessSlots:               h.store.CodexWSStatelessSlots(),
		CodexWSDownstreamKeepaliveSlots:     h.store.CodexWSDownstreamKeepaliveSlots(),
		GithubTokenConfigured:               h.store.GithubToken() != "",
		GithubProxyURL:                      h.store.GithubProxyURL(),
		CodexOverloadPauseEnabled:           runtimeCfg.CodexOverloadPauseEnabled,
		CodexOverloadThresholdPercent:       runtimeCfg.CodexOverloadThresholdPercent,
		CodexOverloadPauseMinutes:           runtimeCfg.CodexOverloadPauseMinutes,
		CodexOverloadWindowMinutes:          runtimeCfg.CodexOverloadWindowMinutes,
		OverflowAutoCompactEnabled:          h.store.OverflowAutoCompactEnabled(),
		CompactViaResponsesEnabled:          h.store.CompactViaResponsesEnabled(),
		CodexPreflightSSEPassthroughEnabled: h.store.CodexPreflightSSEPassthroughEnabled(),
		FirstTokenExcludesWsAcquire:         h.store.FirstTokenExcludesWsAcquire(),
		CodexContinueThinkingEnabled:        h.store.CodexContinueThinkingEnabled(),
		CodexContinueMaxRounds:              h.store.CodexContinueMaxRounds(),
		UTLSShutdownTimeoutMinutes:          utlsShutdownTimeoutMinutes,
		CodexCLIVersionSyncEnabled:          h.store.CodexCLIVersionSyncEnabled(),
		CodexCLIVersionSyncIntervalHours:    h.store.CodexCLIVersionSyncIntervalHours(),
		CodexSyncedCLIVersion:               proxy.CurrentRuntimeSettings().CodexSyncedCLIVersion,
		CodexSyncedDesktopMacBuild:          proxy.CurrentRuntimeSettings().CodexSyncedDesktopMacBuild,
		CodexSyncedDesktopWindowsBuild:      proxy.CurrentRuntimeSettings().CodexSyncedDesktopWindowsBuild,
		CodexSyncedVSCodeBuild:              proxy.CurrentRuntimeSettings().CodexSyncedVSCodeBuild,
		CodexClientVersions:                 proxy.CodexClientVersionCacheView(),
		CodexEffectiveCLIVersion:            proxy.LatestCodexCLIVersionForHeaders(),
		SchedulerMode:                       h.store.GetSchedulerMode(),
		AffinityMode:                        h.store.GetAffinityMode(),
		SessionAffinitySpread:               h.store.GetSessionAffinitySpread(),
		SessionSlotBufferEnabled:            h.store.SessionSlotBufferEnabled(),
		SessionSlotBufferSeconds:            int(h.store.GetSessionSlotBuffer() / time.Second),
		MaxRetries:                          h.store.GetMaxRetries(),
		MaxRateLimitRetries:                 h.store.GetMaxRateLimitRetries(),
		RetryIntervalMS:                     h.store.GetRetryIntervalMS(),
		TransportRetryPolicy:                h.store.GetTransportRetryPolicy(),
		ContinuousRetryEnabled:              continuousRetryPolicy.Enabled,
		ContinuousRetryCatchAll:             continuousRetryPolicy.CatchAll,
		ContinuousRetryCategories:           continuousRetryPolicy.Categories,
		ContinuousRetryStatusCodes:          continuousRetryPolicy.StatusCodes,
		ContinuousRetryErrorCodes:           continuousRetryPolicy.ErrorCodes,
		ContinuousRetryMaxDurationSeconds:   continuousRetryPolicy.MaxDurationSeconds,
		CodexFingerprintDefaultMode:         h.store.GetCodexFingerprintDefaultMode(),
		AllowRemoteMigration:                h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                      h.databaseDriver,
		DatabaseLabel:                       h.databaseLabel,
		CacheDriver:                         h.cacheDriver,
		CacheLabel:                          h.cacheLabel,
		ModelMapping:                        h.store.GetModelMapping(),
		CodexModelMapping:                   h.store.GetCodexModelMapping(),
		PayloadRules:                        h.store.GetPayloadRules(),
		ReasoningEffortModels:               h.store.GetReasoningEffortModels(),
		ResinURL:                            resinURL,
		ResinPlatformName:                   resinPlatformName,
		CodexEgress:                         proxy.CurrentCodexEgressSummary(),
		PromptFilterEnabled:                 promptFilterCfg.Enabled,
		PromptFilterMode:                    promptFilterCfg.Mode,
		PromptFilterThreshold:               promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:         promptFilterCfg.StrictThreshold,
		PromptFilterStrictTerminalEnabled:   promptFilterCfg.StrictTerminalEnabled,
		PromptFilterAdvancedConfig:          promptFilterAdvancedRaw,
		PromptFilterLogMatches:              promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:           promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:          promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:          promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PromptFilterDisabledPatterns:        promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:           promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKeyConfigured:  promptFilterCfg.Review.APIKey != "",
		PromptFilterReviewAPIKeyCount:       len(promptFilterCfg.Review.APIKeyList()),
		PromptFilterReviewBaseURL:           promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:             promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:    promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:        promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                    runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                  runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:                runtimeCfg.CodexUserAgentConfig,
		CodexTelemetryEnabled:               runtimeCfg.CodexTelemetryEnabled,
		CodexTelemetryTimingDebug:           runtimeCfg.CodexTelemetryTimingDebug,
		UsageLogMode:                        h.db.GetUsageLogMode(),
		UsageLogBatchSize:                   h.db.GetUsageLogBatchSize(),
		UsageLogFlushIntervalSeconds:        h.db.GetUsageLogFlushIntervalSeconds(),
		StreamFlushPolicy:                   runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:               runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                      runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:            runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                   runtimeCfg.BillingTierPolicy,
		ModelsListReadMaxBytes:              runtimeCfg.ModelsListReadMaxBytes,
		ShowFullUsageNumbers:                showFullUsageNumbers,
		ShowUpstreamModelMismatch:           showUpstreamModelMismatch,
		PublicKeyUsagePageEnabled:           publicKeyUsagePageEnabled,
		PublicImageStudioPageEnabled:        publicImageStudioPageEnabled,
		PublicAccountPortalPageEnabled:      publicAccountPortalPageEnabled,
		ImageStorageBackend:                 imgCfg.Backend,
		ImageS3Endpoint:                     imgCfg.Endpoint,
		ImageS3Region:                       imgCfg.Region,
		ImageS3Bucket:                       imgCfg.Bucket,
		ImageS3AccessKey:                    imgCfg.AccessKey,
		ImageS3SecretKey:                    imgCfg.SecretKey,
		ImageS3Prefix:                       imgPrefix,
		ImageS3ForcePathStyle:               imgCfg.ForcePathStyle,
		AutoPause5hThreshold:                h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:                h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:         h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:         h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                  h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:           h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                  h.store.GetSmartPacingWindows(),
		IgnoreUsageLimitStatus:              h.store.IgnoreUsageLimitStatus(),
		OutputGuardEnabled:                  outputGuardEnabled,
		OutputGuardMaxToolArgumentBytes:     outputGuardSettings.MaxToolArgumentBytes,
		OutputGuardMaxToolArgumentDeltas:    outputGuardSettings.MaxToolArgumentDeltas,
		OutputGuardMaxChannelBytes:          outputGuardSettings.MaxChannelBytes,
		OutputGuardMaxRequestBytes:          outputGuardSettings.MaxRequestBytes,
		OutputGuardConfigGeneration:         outputGuardSettings.ConfigGeneration,
	})
}

func promptFilterCustomPatternSnapshotsEquivalent(leftRaw, rightRaw string) bool {
	var leftUnknown, rightUnknown []map[string]any
	if json.Unmarshal([]byte(leftRaw), &leftUnknown) != nil || json.Unmarshal([]byte(rightRaw), &rightUnknown) != nil || len(leftUnknown) != len(rightUnknown) {
		return false
	}
	knownFields := []string{
		"name", "pattern", "weight", "category", "strict", "signal_only", "enabled",
		"all_patterns", "any_patterns", "exclude_patterns", "min_matches",
	}
	for index := range leftUnknown {
		for _, field := range knownFields {
			delete(leftUnknown[index], field)
			delete(rightUnknown[index], field)
		}
		if !reflect.DeepEqual(leftUnknown[index], rightUnknown[index]) {
			return false
		}
	}
	left, leftErr := promptfilter.ParseCustomPatterns(leftRaw)
	right, rightErr := promptfilter.ParseCustomPatterns(rightRaw)
	if leftErr != nil || rightErr != nil || len(left) != len(right) {
		return false
	}
	// Settings responses expose the effective runtime snapshot. Unsafe legacy
	// rules are quarantined there with enabled=false, while the persisted JSON
	// deliberately remains unchanged until an administrator saves the rule set.
	// Compare both sides after applying that same quarantine transformation so
	// deleting or editing a quarantined rule does not fail forever with 409.
	left, _ = promptfilter.SanitizeCustomPatterns(left)
	right, _ = promptfilter.SanitizeCustomPatterns(right)
	// Omitted enabled and explicit true are the same active runtime rule.
	for index := range left {
		if left[index].Enabled != nil && *left[index].Enabled {
			left[index].Enabled = nil
		}
		if right[index].Enabled != nil && *right[index].Enabled {
			right[index].Enabled = nil
		}
	}
	return promptfilter.MarshalCustomPatterns(left) == promptfilter.MarshalCustomPatterns(right)
}

func (h *Handler) updatePromptFilterCustomPatterns(c *gin.Context, patterns []promptfilter.PatternConfig, expectedRaw string) {
	ctx := c.Request.Context()
	persisted, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to read existing Prompt custom rules: "+err.Error())
		return
	}
	persistedRaw := "[]"
	if persisted != nil {
		persistedRaw = strings.TrimSpace(persisted.PromptFilterCustomPatterns)
		if persistedRaw == "" {
			persistedRaw = "[]"
		}
	}
	if _, err := promptfilter.ParseCustomPatterns(persistedRaw); err != nil {
		writeError(c, http.StatusInternalServerError, "Prompt custom rules in the database are invalid; repair the persisted configuration first")
		return
	}
	expectedForCAS := strings.TrimSpace(expectedRaw)
	if expectedForCAS == "" {
		expectedForCAS = "[]"
	}
	// The settings response exposes canonical runtime JSON, while an older
	// database may still contain equivalent pretty-printed JSON. Compare
	// against the exact persisted bytes only when both decode to the same
	// ordered snapshot; a real semantic difference must remain a conflict.
	if promptFilterCustomPatternSnapshotsEquivalent(persistedRaw, expectedForCAS) {
		expectedForCAS = persistedRaw
	}
	replacement := promptfilter.MarshalCustomPatterns(patterns)
	swapped, err := h.db.CompareAndSwapPromptFilterCustomPatterns(ctx, expectedForCAS, replacement)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to save Prompt custom rules: "+err.Error())
		return
	}
	if !swapped {
		// Refresh this replica before returning 409 so the frontend's immediate
		// reload sees the authoritative snapshot without waiting for the periodic
		// multi-replica synchronizer.
		if latest, readErr := h.db.GetSystemSettings(ctx); readErr == nil && latest != nil {
			if latestPatterns, parseErr := promptfilter.ParseCustomPatterns(latest.PromptFilterCustomPatterns); parseErr == nil {
				latestCfg := h.store.GetPromptFilterConfig()
				latestCfg.CustomPatterns = latestPatterns
				h.store.SetPromptFilterConfig(latestCfg)
			} else {
				log.Printf("failed to parse the database snapshot after a Prompt custom rules conflict: %v", parseErr)
			}
		} else if readErr != nil {
			log.Printf("failed to refresh the database snapshot after a Prompt custom rules conflict: %v", readErr)
		}
		writeError(c, http.StatusConflict, "Prompt custom rules were updated by another page or instance; refresh and try again")
		return
	}
	latestCfg := h.store.GetPromptFilterConfig()
	latestCfg.CustomPatterns = patterns
	h.store.SetPromptFilterConfig(latestCfg)
	log.Printf("settings updated: prompt_filter custom_patterns=%d", len(patterns))
	h.GetSettings(c)
}

// UpdateSettings updates system settings (takes effect immediately)
func (h *Handler) UpdateSettings(c *gin.Context) {
	var req updateSettingsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.PromptFilterCustomPatternsExpected != nil && req.PromptFilterCustomPatterns == nil {
		writeError(c, http.StatusBadRequest, "The Prompt custom rules version snapshot cannot be submitted on its own")
		return
	}
	if req.PromptFilterCustomPatterns != nil && updateSettingsHasFieldsOtherThanCustomPatterns(req) {
		writeError(c, http.StatusBadRequest, "Prompt custom rules must be saved on their own; refresh and retry from the rules page")
		return
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	if req.ResponseCacheConfigGeneration != nil {
		writeError(c, http.StatusBadRequest, "response_cache_config_generation is a read-only field")
		return
	}
	if req.OutputGuardConfigGeneration != nil {
		writeError(c, http.StatusBadRequest, "output_guard_config_generation is a read-only field")
		return
	}
	if req.CodexImagesMainModel != nil {
		normalized, err := proxy.NormalizeImagesMainModel(*req.CodexImagesMainModel)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		req.CodexImagesMainModel = &normalized
	}
	modelCooldownUpdateRequested := req.RelayModelCooldownMode != nil ||
		req.RelayModelCooldownSeconds != nil ||
		req.RelayModelCooldownBackoffEnabled != nil ||
		req.OAuthModelCooldownMode != nil ||
		req.OAuthModelCooldownSeconds != nil ||
		req.OAuthModelCooldownBackoffEnabled != nil ||
		req.OAuthUnsupportedCooldownSeconds != nil
	if req.RelayModelCooldownMode != nil && !database.IsValidModelCooldownMode(*req.RelayModelCooldownMode) {
		writeError(c, http.StatusBadRequest, "relay_model_cooldown_mode must be off, fixed, or adaptive")
		return
	}
	if req.OAuthModelCooldownMode != nil && !database.IsValidModelCooldownMode(*req.OAuthModelCooldownMode) {
		writeError(c, http.StatusBadRequest, "oauth_model_cooldown_mode must be off, fixed, or adaptive")
		return
	}
	for field, value := range map[string]*int{
		"relay_model_cooldown_seconds": req.RelayModelCooldownSeconds,
		"oauth_model_cooldown_seconds": req.OAuthModelCooldownSeconds,
	} {
		if value != nil && (*value < 1 || *value > database.MaxModelCooldownSeconds) {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("%s must be between 1 and %d", field, database.MaxModelCooldownSeconds))
			return
		}
	}
	if value := req.OAuthUnsupportedCooldownSeconds; value != nil && (*value < 0 || *value > database.MaxModelCooldownSeconds) {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("oauth_unsupported_model_cooldown_seconds must be between 0 and %d (0 means off)", database.MaxModelCooldownSeconds))
		return
	}
	var submittedPromptFilterCustomPatterns []promptfilter.PatternConfig
	var promptFilterPatternQuarantines []promptfilter.PatternQuarantine
	var expectedPromptFilterCustomPatterns string
	if req.PromptFilterCustomPatterns != nil {
		patterns, err := promptfilter.ParseCustomPatterns(*req.PromptFilterCustomPatterns)
		if err != nil {
			writeError(c, http.StatusBadRequest, "Prompt check custom rules JSON is invalid: "+err.Error())
			return
		}
		if err := promptfilter.ValidateCustomPatterns(patterns); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt check custom rules did not pass security validation: "+err.Error())
			return
		}
		submittedPromptFilterCustomPatterns = patterns
		if req.PromptFilterCustomPatternsExpected == nil {
			writeError(c, http.StatusConflict, "Prompt custom rules are missing a version snapshot; refresh the page and try again")
			return
		}
		expectedPromptFilterCustomPatterns = strings.TrimSpace(*req.PromptFilterCustomPatternsExpected)
		if expectedPromptFilterCustomPatterns == "" {
			expectedPromptFilterCustomPatterns = "[]"
		}
		if _, err := promptfilter.ParseCustomPatterns(expectedPromptFilterCustomPatterns); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt custom rules version snapshot is invalid: "+err.Error())
			return
		}
		h.updatePromptFilterCustomPatterns(c, submittedPromptFilterCustomPatterns, expectedPromptFilterCustomPatterns)
		return
	}
	if req.AutoPause5hThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_5h_threshold", *req.AutoPause5hThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.AutoPause7dThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_7d_threshold", *req.AutoPause7dThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	if req.AutoPause5hGuardBandPercent != nil {
		if *req.AutoPause5hGuardBandPercent < 0 || *req.AutoPause5hGuardBandPercent > 100 {
			writeError(c, http.StatusBadRequest, "auto_pause_5h_guard_band_percent must be between 0 and 100")
			return
		}
	}
	if req.AutoPause5hGuardConcurrency != nil {
		if *req.AutoPause5hGuardConcurrency < 0 || *req.AutoPause5hGuardConcurrency > 1000 {
			writeError(c, http.StatusBadRequest, "auto_pause_5h_guard_concurrency must be between 0 and 1000")
			return
		}
	}
	if req.SmartPacingMinConcurrency != nil {
		if *req.SmartPacingMinConcurrency < 1 || *req.SmartPacingMinConcurrency > 1000 {
			writeError(c, http.StatusBadRequest, "smart_pacing_min_concurrency must be between 1 and 1000")
			return
		}
	}
	if req.SmartPacingWindows != nil {
		switch strings.ToLower(strings.TrimSpace(*req.SmartPacingWindows)) {
		case "5h,7d", "7d,5h", "5h", "7d", "":
		default:
			writeError(c, http.StatusBadRequest, "smart_pacing_windows only supports 5h,7d / 5h / 7d")
			return
		}
	}
	if req.AutoResetCreditsBeforeExpiryMin != nil {
		if *req.AutoResetCreditsBeforeExpiryMin < 10 || *req.AutoResetCreditsBeforeExpiryMin > 10080 {
			writeError(c, http.StatusBadRequest, "auto_reset_credits_before_expiry_min must be between 10 and 10080 minutes")
			return
		}
	}

	responseCacheUpdate := database.ResponseCacheSettingsUpdate{
		LocalMaxBytes:       req.ResponseCacheLocalMaxBytes,
		LocalMaxEntryBytes:  req.ResponseCacheLocalMaxEntryBytes,
		ReconstructMaxBytes: req.ResponseCacheReconstructMaxBytes,
		WritePolicy:         req.ResponseCacheWritePolicy,
	}
	responseCacheUpdateRequested := responseCacheUpdate.LocalMaxBytes != nil ||
		responseCacheUpdate.LocalMaxEntryBytes != nil ||
		responseCacheUpdate.ReconstructMaxBytes != nil ||
		responseCacheUpdate.WritePolicy != nil
	if err := validateResponseCacheSettingsUpdateRanges(responseCacheUpdate); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	cacheSettingsStore := h.cacheSettingsStore()
	if cacheSettingsStore == nil {
		writeError(c, http.StatusInternalServerError, "Response cache settings storage is unavailable")
		return
	}
	responseCacheSettings, err := cacheSettingsStore.GetResponseCacheSettings(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to read response cache settings: "+err.Error())
		return
	}
	if responseCacheUpdate.LocalMaxBytes != nil {
		responseCacheSettings.LocalMaxBytes = *responseCacheUpdate.LocalMaxBytes
	}
	if responseCacheUpdate.LocalMaxEntryBytes != nil {
		responseCacheSettings.LocalMaxEntryBytes = *responseCacheUpdate.LocalMaxEntryBytes
	}
	if responseCacheUpdate.ReconstructMaxBytes != nil {
		responseCacheSettings.ReconstructMaxBytes = *responseCacheUpdate.ReconstructMaxBytes
	}
	if err := database.ValidateResponseCacheSettings(responseCacheSettings); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	outputGuardUpdate := database.OutputGuardSettingsUpdate{
		Enabled:               req.OutputGuardEnabled,
		MaxToolArgumentBytes:  req.OutputGuardMaxToolArgumentBytes,
		MaxToolArgumentDeltas: req.OutputGuardMaxToolArgumentDeltas,
		MaxChannelBytes:       req.OutputGuardMaxChannelBytes,
		MaxRequestBytes:       req.OutputGuardMaxRequestBytes,
	}
	outputGuardUpdateRequested := outputGuardUpdate.Enabled != nil ||
		outputGuardUpdate.MaxToolArgumentBytes != nil ||
		outputGuardUpdate.MaxToolArgumentDeltas != nil ||
		outputGuardUpdate.MaxChannelBytes != nil ||
		outputGuardUpdate.MaxRequestBytes != nil

	// Reading, merging, validating, and incrementing the generation of the guard settings are all handled by
	// UpdateSystemSettingsWithOutputGuard within the same write transaction. There is no longer any pre-read, pre-merge, or pre-validation outside the transaction:
	// under concurrent saves, old values read outside the transaction would wrongly reject a valid patch with
	// 400, while a real rejection inside the transaction would be reported as 500.
	var outputGuardSettings database.OutputGuardSettings
	if !outputGuardUpdateRequested {
		latestGuard, err := h.db.GetOutputGuardSettings(c.Request.Context())
		if err != nil {
			log.Printf("failed to read output guard settings: %v", err)
			writeError(c, http.StatusInternalServerError, "Failed to read guard settings: "+err.Error())
			return
		}
		outputGuardSettings = latestGuard
	}

	currentAdminSecret := ""
	siteName := database.DefaultSiteName
	siteLogo := ""
	bgCfg := defaultBackgroundConfig()
	showFullUsageNumbers := false
	showUpstreamModelMismatch := true
	publicKeyUsagePageEnabled := true
	publicImageStudioPageEnabled := true
	publicAccountPortalPageEnabled := false
	modelPricingOverrides := "{}"
	modelPricingSyncURL := ""
	persistedAutoResetCreditsEnabled := false
	persistedAutoResetCreditsOnExhaustionEnabled := false
	persistedAutoResetCreditsBeforeExpiryMin := 60
	persistedAutoActivate5hWindowEnabled := false
	codexImagesMainModel := ""
	persistedUTLSShutdownTimeoutMinutes := database.NormalizeUTLSShutdownTimeoutMinutes(0)
	modelsListReadMaxBytes := database.DefaultModelsListReadMaxBytes
	sessionSlotBufferEnabled := h.store.SessionSlotBufferEnabled()
	sessionSlotBufferSeconds := database.NormalizeSessionSlotBufferSeconds(int(h.store.GetSessionSlotBuffer() / time.Second))
	existingSettings, settingsErr := h.db.GetSystemSettings(c.Request.Context())
	if settingsErr != nil {
		writeError(c, http.StatusInternalServerError, "Failed to read existing settings: "+settingsErr.Error())
		return
	}
	if existingSettings != nil {
		currentAdminSecret = existingSettings.AdminSecret
		siteName = database.NormalizeSiteName(existingSettings.SiteName)
		siteLogo = strings.TrimSpace(existingSettings.SiteLogo)
		bgCfg = decodeBackgroundConfig(existingSettings.BackgroundConfig)
		showFullUsageNumbers = existingSettings.ShowFullUsageNumbers
		showUpstreamModelMismatch = existingSettings.ShowUpstreamModelMismatch
		publicKeyUsagePageEnabled = existingSettings.PublicKeyUsagePageEnabled
		publicImageStudioPageEnabled = existingSettings.PublicImageStudioPageEnabled
		publicAccountPortalPageEnabled = existingSettings.PublicAccountPortalPageEnabled
		modelPricingOverrides = existingSettings.ModelPricingOverrides
		modelPricingSyncURL = existingSettings.ModelPricingSyncURL
		persistedAutoResetCreditsEnabled = existingSettings.AutoResetCreditsEnabled
		persistedAutoResetCreditsOnExhaustionEnabled = existingSettings.AutoResetCreditsOnExhaustionEnabled
		persistedAutoResetCreditsBeforeExpiryMin = existingSettings.AutoResetCreditsBeforeExpiryMin
		persistedAutoActivate5hWindowEnabled = existingSettings.AutoActivate5hWindowEnabled
		codexImagesMainModel = existingSettings.CodexImagesMainModel
		persistedUTLSShutdownTimeoutMinutes = database.NormalizeUTLSShutdownTimeoutMinutes(existingSettings.UTLSShutdownTimeoutMinutes)
		modelsListReadMaxBytes = database.NormalizeModelsListReadMaxBytes(existingSettings.ModelsListReadMaxBytes)
		sessionSlotBufferEnabled = existingSettings.SessionSlotBufferEnabled
		sessionSlotBufferSeconds = database.NormalizeSessionSlotBufferSeconds(existingSettings.SessionSlotBufferSeconds)
	}
	if req.SessionSlotBufferEnabled != nil {
		sessionSlotBufferEnabled = *req.SessionSlotBufferEnabled
	}
	if req.CodexImagesMainModel != nil {
		codexImagesMainModel = *req.CodexImagesMainModel
	}
	if req.SessionSlotBufferSeconds != nil {
		sessionSlotBufferSeconds = database.NormalizeSessionSlotBufferSeconds(*req.SessionSlotBufferSeconds)
	}
	modelsListReadLimitChanged := false
	if req.ModelsListReadMaxBytes != nil {
		if err := database.ValidateModelsListReadMaxBytes(*req.ModelsListReadMaxBytes); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		modelsListReadLimitChanged = *req.ModelsListReadMaxBytes != modelsListReadMaxBytes
	}
	if req.AdminSecret != nil {
		if h.adminSecretEnv == "" {
			currentAdminSecret = *req.AdminSecret
			log.Printf("settings updated: admin_secret (length=%d)", len(currentAdminSecret))
		} else {
			log.Printf("ADMIN_SECRET environment variable detected; ignoring the admin_secret submitted from the frontend")
		}
	}
	if req.SiteName != nil {
		siteName = database.NormalizeSiteName(*req.SiteName)
		log.Printf("settings updated: site_name = %s", siteName)
	}
	if req.SiteLogo != nil {
		normalized, err := normalizeSiteLogo(*req.SiteLogo)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		siteLogo = normalized
		log.Printf("settings updated: site_logo (length=%d)", len(siteLogo))
	}
	if req.BackgroundImage != nil {
		normalized, err := normalizeBackgroundImage(*req.BackgroundImage)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		bgCfg.Image = normalized
		log.Printf("settings updated: background_image (length=%d)", len(bgCfg.Image))
	}
	if req.BackgroundOpacity != nil {
		bgCfg.Opacity = normalizeBackgroundOpacity(*req.BackgroundOpacity)
		log.Printf("settings updated: background_opacity = %d", bgCfg.Opacity)
	}
	if req.BackgroundBlur != nil {
		bgCfg.Blur = normalizeBackgroundBlur(*req.BackgroundBlur)
		log.Printf("settings updated: background_blur = %d", bgCfg.Blur)
	}
	if req.BackgroundGlassOpacity != nil {
		bgCfg.GlassOpacity = normalizeBackgroundGlassOpacity(*req.BackgroundGlassOpacity)
		log.Printf("settings updated: background_glass_opacity = %d", bgCfg.GlassOpacity)
	}
	if req.BackgroundGlassBlur != nil {
		bgCfg.GlassBlur = normalizeBackgroundGlassBlur(*req.BackgroundGlassBlur)
		log.Printf("settings updated: background_glass_blur = %d", bgCfg.GlassBlur)
	}
	hasAdminSecret := strings.TrimSpace(currentAdminSecret) != "" || strings.TrimSpace(h.adminSecretEnv) != ""
	runtimeCfg := proxy.CurrentRuntimeSettings()
	previousMaxConcurrency := h.store.GetMaxConcurrency()
	previousAutoResetCreditsEnabled := runtimeCfg.AutoResetCreditsEnabled
	previousAutoResetCreditsOnExhaustionEnabled := runtimeCfg.AutoResetCreditsOnExhaustionEnabled
	previousAutoResetCreditsBeforeExpiryMin := runtimeCfg.AutoResetCreditsBeforeExpiryMin
	previousAutoActivate5hWindowEnabled := runtimeCfg.AutoActivate5hWindowEnabled
	// The database is the authoritative source under multi-instance deployment; the persisted values are the baseline for this partial update,
	// so that an old instance saving unrelated fields does not roll the automatic consumption config back to its own stale snapshot.
	runtimeCfg.AutoResetCreditsEnabled = persistedAutoResetCreditsEnabled
	runtimeCfg.AutoResetCreditsOnExhaustionEnabled = persistedAutoResetCreditsOnExhaustionEnabled
	runtimeCfg.AutoResetCreditsBeforeExpiryMin = persistedAutoResetCreditsBeforeExpiryMin
	runtimeCfg.AutoActivate5hWindowEnabled = persistedAutoActivate5hWindowEnabled
	runtimeCfg.UTLSShutdownTimeoutMin = persistedUTLSShutdownTimeoutMinutes
	runtimeCfg.ModelsListReadMaxBytes = modelsListReadMaxBytes
	continuousRetryPolicy := h.store.GetContinuousRetryPolicy()
	continuousRetryUpdate := database.ContinuousRetryPolicyUpdate{
		Enabled:            req.ContinuousRetryEnabled,
		CatchAll:           req.ContinuousRetryCatchAll,
		Categories:         req.ContinuousRetryCategories,
		StatusCodes:        req.ContinuousRetryStatusCodes,
		ErrorCodes:         req.ContinuousRetryErrorCodes,
		MaxDurationSeconds: req.ContinuousRetryMaxDurationSeconds,
	}
	continuousRetryChanged := req.ContinuousRetryEnabled != nil || req.ContinuousRetryCatchAll != nil || req.ContinuousRetryCategories != nil || req.ContinuousRetryStatusCodes != nil || req.ContinuousRetryErrorCodes != nil || req.ContinuousRetryMaxDurationSeconds != nil
	utlsShutdownTimeoutMinutes := persistedUTLSShutdownTimeoutMinutes
	autoResetCreditsChanged := (req.AutoResetCreditsOnExhaustionEnabled != nil && *req.AutoResetCreditsOnExhaustionEnabled != persistedAutoResetCreditsOnExhaustionEnabled) ||
		(req.AutoResetCreditsEnabled != nil && *req.AutoResetCreditsEnabled != persistedAutoResetCreditsEnabled) ||
		(req.AutoResetCreditsBeforeExpiryMin != nil && *req.AutoResetCreditsBeforeExpiryMin != persistedAutoResetCreditsBeforeExpiryMin)
	autoActivate5hChanged := req.AutoActivate5hWindowEnabled != nil && *req.AutoActivate5hWindowEnabled != persistedAutoActivate5hWindowEnabled
	usageLogMode := h.db.GetUsageLogMode()
	usageLogBatchSize := h.db.GetUsageLogBatchSize()
	usageLogFlushIntervalSeconds := h.db.GetUsageLogFlushIntervalSeconds()

	maxConcurrencyToApply := previousMaxConcurrency
	maxConcurrencyChanged := false
	if req.MaxConcurrency != nil {
		v := *req.MaxConcurrency
		if v < 1 {
			v = 1
		}
		// No upper limit: operators decide this themselves based on the machine and the upstream capacity
		maxConcurrencyToApply = v
		maxConcurrencyChanged = true
	}

	previousGlobalRPM := h.rateLimiter.GetRPM()
	globalRPMToApply := previousGlobalRPM
	globalRPMChanged := false
	if req.GlobalRPM != nil {
		v := *req.GlobalRPM
		if v < 0 {
			v = 0
		}
		globalRPMToApply = v
		globalRPMChanged = true
	}

	if req.TestModel != nil && *req.TestModel != "" {
		h.store.SetTestModel(*req.TestModel)
		log.Printf("settings updated: test_model = %s", *req.TestModel)
	}

	if req.TestContent != nil {
		testContent, err := validateConnectionTestContent(*req.TestContent)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		h.store.SetTestContent(testContent)
		log.Printf("settings updated: test_content (length=%d)", len([]rune(testContent)))
	}

	if req.TestConcurrency != nil {
		v := *req.TestConcurrency
		if v < 1 {
			v = 1
		}
		if v > 200 {
			v = 200
		}
		h.store.SetTestConcurrency(v)
		log.Printf("settings updated: test_concurrency = %d", v)
	}

	if req.BackgroundRefreshIntervalMinutes != nil {
		v := *req.BackgroundRefreshIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 1440 {
			v = 1440
		}
		h.store.SetBackgroundRefreshInterval(time.Duration(v) * time.Minute)
		log.Printf("settings updated: background_refresh_interval_minutes = %d", v)
	}

	if req.UsageProbeMaxAgeMinutes != nil {
		v := *req.UsageProbeMaxAgeMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetUsageProbeMaxAge(time.Duration(v) * time.Minute)
		log.Printf("settings updated: usage_probe_max_age_minutes = %d", v)
	}

	if req.UsageProbeConcurrency != nil {
		v := *req.UsageProbeConcurrency
		if v < 1 {
			v = 1
		}
		if v > 128 {
			v = 128
		}
		h.store.SetUsageProbeConcurrency(v)
		log.Printf("settings updated: usage_probe_concurrency = %d", v)
	}

	if req.UsageProbeResponsesFallbackEnabled != nil {
		h.store.SetUsageProbeResponsesFallbackEnabled(*req.UsageProbeResponsesFallbackEnabled)
		log.Printf("settings updated: usage_probe_responses_fallback_enabled = %t", *req.UsageProbeResponsesFallbackEnabled)
	}

	if req.RecoveryProbeIntervalMinutes != nil {
		v := *req.RecoveryProbeIntervalMinutes
		if v < 1 {
			v = 1
		}
		if v > 10080 {
			v = 10080
		}
		h.store.SetRecoveryProbeInterval(time.Duration(v) * time.Minute)
		log.Printf("settings updated: recovery_probe_interval_minutes = %d", v)
	}

	if req.CodexOAuthKeepaliveEnabled != nil {
		h.store.SetCodexOAuthKeepalive(*req.CodexOAuthKeepaliveEnabled)
	}
	if req.LazyMode != nil {
		h.store.SetLazyMode(*req.LazyMode)
		log.Printf("settings updated: lazy_mode = %t", *req.LazyMode)
	}

	if req.ProxyURL != nil {
		h.store.SetProxyURL(*req.ProxyURL)
		log.Printf("settings updated: proxy_url = %s", security.MaskURLCredentials(*req.ProxyURL))
	}

	if req.PgMaxConns != nil {
		v := *req.PgMaxConns
		if v < 5 {
			v = 5
		}
		if v > 5000 {
			v = 5000
		}
		h.db.SetMaxOpenConns(v)
		h.pgMaxConns = v
		log.Printf("settings updated: pg_max_conns = %d", v)
	}

	if req.RedisPoolSize != nil {
		v := *req.RedisPoolSize
		if v < 5 {
			v = 5
		}
		if v > 5000 {
			v = 5000
		}
		h.cache.SetPoolSize(v)
		h.redisPoolSize = v
		log.Printf("settings updated: redis_pool_size = %d", v)
	}

	if req.AutoCleanUnauthorized != nil {
		h.store.SetAutoCleanUnauthorized(*req.AutoCleanUnauthorized)
		log.Printf("settings updated: auto_clean_unauthorized = %t", *req.AutoCleanUnauthorized)
	}

	if req.AutoCleanRateLimited != nil {
		h.store.SetAutoCleanRateLimited(*req.AutoCleanRateLimited)
		log.Printf("settings updated: auto_clean_rate_limited = %t", *req.AutoCleanRateLimited)
	}

	if req.AutoCleanFullUsage != nil {
		h.store.SetAutoCleanFullUsage(*req.AutoCleanFullUsage)
		log.Printf("settings updated: auto_clean_full_usage = %t", *req.AutoCleanFullUsage)
	}

	if req.AutoCleanError != nil {
		h.store.SetAutoCleanError(*req.AutoCleanError)
		log.Printf("settings updated: auto_clean_error = %t", *req.AutoCleanError)
	}

	var expiredCleaned int
	if req.AutoCleanExpired != nil {
		h.store.SetAutoCleanExpired(*req.AutoCleanExpired)
		log.Printf("settings updated: auto_clean_expired = %t", *req.AutoCleanExpired)
		// When turned on, run one cleanup immediately
		if *req.AutoCleanExpired {
			expiredCleaned = h.store.CleanExpiredNow()
		}
	}

	if req.ProxyPoolEnabled != nil {
		if *req.ProxyPoolEnabled {
			if err := h.store.ReloadProxyPool(); err != nil {
				writeError(c, http.StatusInternalServerError, "Proxy pool refresh failed: "+err.Error())
				return
			}
		}
		h.store.SetProxyPoolEnabled(*req.ProxyPoolEnabled)
		log.Printf("settings updated: proxy_pool_enabled = %t", *req.ProxyPoolEnabled)
	}

	if req.SchedulerEngine != nil {
		engine := strings.ToLower(strings.TrimSpace(*req.SchedulerEngine))
		if engine != "legacy" && engine != "shadow" && engine != "indexed" {
			writeError(c, http.StatusBadRequest, "scheduler_engine must be legacy, shadow, or indexed")
			return
		}
		h.store.SetSchedulerEngine(engine)
		log.Printf("settings updated: scheduler_engine = %s", engine)
	} else if req.FastSchedulerEnabled != nil {
		h.store.SetFastSchedulerEnabled(*req.FastSchedulerEnabled)
		log.Printf("settings updated: fast_scheduler_enabled = %t", *req.FastSchedulerEnabled)
	}

	if req.CodexForceWebsocket != nil {
		h.store.SetCodexForceWebsocket(*req.CodexForceWebsocket)
		runtimeCfg.CodexForceWebsocket = *req.CodexForceWebsocket
		log.Printf("settings updated: codex_force_websocket = %t", *req.CodexForceWebsocket)
	}
	if req.CodexRequestCompression != nil {
		h.store.SetCodexRequestCompression(*req.CodexRequestCompression)
		runtimeCfg.CodexRequestCompression = *req.CodexRequestCompression
		log.Printf("settings updated: codex_request_compression = %t", *req.CodexRequestCompression)
	}

	if req.CodexWSWeakNetworkMode != nil {
		runtimeCfg.CodexWSWeakNetworkMode = *req.CodexWSWeakNetworkMode
		log.Printf("settings updated: codex_ws_weak_network_mode = %t", *req.CodexWSWeakNetworkMode)
	}

	if req.CodexWSKeepaliveEnabled != nil {
		h.store.SetCodexWSKeepaliveEnabled(*req.CodexWSKeepaliveEnabled)
		log.Printf("settings updated: codex_ws_keepalive_enabled = %t", *req.CodexWSKeepaliveEnabled)
	}

	if req.CodexWSKeepaliveIntervalSec != nil {
		h.store.SetCodexWSKeepaliveIntervalSec(*req.CodexWSKeepaliveIntervalSec)
		log.Printf("settings updated: codex_ws_keepalive_interval_sec = %d", *req.CodexWSKeepaliveIntervalSec)
	}

	if req.CodexWSHideUpstreamErrors != nil {
		h.store.SetCodexWSHideUpstreamErrors(*req.CodexWSHideUpstreamErrors)
		runtimeCfg.CodexWSHideErrors = *req.CodexWSHideUpstreamErrors
		log.Printf("settings updated: codex_ws_hide_upstream_errors = %t", *req.CodexWSHideUpstreamErrors)
	}

	if req.CodexWSSilentRetryEnabled != nil {
		h.store.SetCodexWSSilentRetryEnabled(*req.CodexWSSilentRetryEnabled)
		runtimeCfg.CodexWSSilentRetry = *req.CodexWSSilentRetryEnabled
		log.Printf("settings updated: codex_ws_silent_retry_enabled = %t", *req.CodexWSSilentRetryEnabled)
	}

	if req.CodexWSSizeRouterEnabled != nil {
		h.store.SetCodexWSSizeRouterEnabled(*req.CodexWSSizeRouterEnabled)
		runtimeCfg.CodexWSSizeRouter = *req.CodexWSSizeRouterEnabled
		log.Printf("settings updated: codex_ws_size_router_enabled = %t", *req.CodexWSSizeRouterEnabled)
	}

	if req.CodexWSSilentMaxRetries != nil {
		v := *req.CodexWSSilentMaxRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		h.store.SetCodexWSSilentMaxRetries(v)
		runtimeCfg.CodexWSSilentRetries = v
		log.Printf("settings updated: codex_ws_silent_max_retries = %d", v)
	}

	if req.CodexWSBusyAcquireMaxWaitSec != nil {
		v := database.NormalizeCodexWSBusyAcquireMaxWaitSec(*req.CodexWSBusyAcquireMaxWaitSec)
		h.store.SetCodexWSBusyAcquireMaxWaitSec(v)
		runtimeCfg.CodexWSBusyMaxWaitSec = v
		log.Printf("settings updated: codex_ws_busy_acquire_max_wait_sec = %d", v)
	}

	if req.CodexWSBusyOverflowEnabled != nil {
		h.store.SetCodexWSBusyOverflowEnabled(*req.CodexWSBusyOverflowEnabled)
		runtimeCfg.CodexWSBusyOverflow = *req.CodexWSBusyOverflowEnabled
		log.Printf("settings updated: codex_ws_busy_overflow_enabled = %t", *req.CodexWSBusyOverflowEnabled)
	}

	if req.CodexWSBusyPatienceSec != nil {
		v := database.NormalizeCodexWSBusyPatienceSec(*req.CodexWSBusyPatienceSec)
		h.store.SetCodexWSBusyPatienceSec(v)
		runtimeCfg.CodexWSBusyPatienceSec = v
		log.Printf("settings updated: codex_ws_busy_patience_sec = %d", v)
	}

	if req.CodexWSStatelessSlots != nil {
		v := database.NormalizeCodexWSStatelessSlots(*req.CodexWSStatelessSlots)
		h.store.SetCodexWSStatelessSlots(v)
		runtimeCfg.CodexWSStatelessSlots = v
		log.Printf("settings updated: codex_ws_stateless_slots = %d", v)
	}

	if req.CodexWSDownstreamKeepaliveSlots != nil {
		v := database.NormalizeCodexWSDownstreamKeepaliveSlots(*req.CodexWSDownstreamKeepaliveSlots)
		h.store.SetCodexWSDownstreamKeepaliveSlots(v)
		runtimeCfg.CodexWSDownstreamKeepaliveSlots = v
		log.Printf("settings updated: codex_ws_downstream_keepalive_slots = %d", v)
	}

	// GitHub access settings (issue #522). The token is never echoed or logged; an empty string clears it.
	if req.GithubToken != nil {
		v := strings.TrimSpace(*req.GithubToken)
		h.store.SetGithubToken(v)
		runtimeCfg.GithubToken = v
		log.Printf("settings updated: github_token configured=%t", v != "")
	}
	if req.GithubProxyURL != nil {
		v := strings.TrimSpace(*req.GithubProxyURL)
		h.store.SetGithubProxyURL(v)
		runtimeCfg.GithubProxyURL = v
		log.Printf("settings updated: github_proxy_url = %s", security.MaskURLCredentials(v))
	}

	// Codex overload circuit breaker (the config is stored only in RuntimeSettings and takes effect on hot update)
	if req.CodexOverloadPauseEnabled != nil {
		runtimeCfg.CodexOverloadPauseEnabled = *req.CodexOverloadPauseEnabled
		log.Printf("settings updated: codex_overload_pause_enabled = %t", *req.CodexOverloadPauseEnabled)
	}
	if req.CodexOverloadThresholdPercent != nil {
		v := database.NormalizeCodexOverloadThresholdPercent(*req.CodexOverloadThresholdPercent)
		runtimeCfg.CodexOverloadThresholdPercent = v
		log.Printf("settings updated: codex_overload_threshold_percent = %d", v)
	}
	if req.CodexOverloadPauseMinutes != nil {
		v := database.NormalizeCodexOverloadPauseMinutes(*req.CodexOverloadPauseMinutes)
		runtimeCfg.CodexOverloadPauseMinutes = v
		log.Printf("settings updated: codex_overload_pause_minutes = %d", v)
	}
	if req.CodexOverloadWindowMinutes != nil {
		v := database.NormalizeCodexOverloadWindowMinutes(*req.CodexOverloadWindowMinutes)
		runtimeCfg.CodexOverloadWindowMinutes = v
		log.Printf("settings updated: codex_overload_window_minutes = %d", v)
	}

	if req.OverflowAutoCompactEnabled != nil {
		h.store.SetOverflowAutoCompactEnabled(*req.OverflowAutoCompactEnabled)
		runtimeCfg.OverflowAutoCompact = *req.OverflowAutoCompactEnabled
		log.Printf("settings updated: overflow_auto_compact_enabled = %t", *req.OverflowAutoCompactEnabled)
	}

	if req.CompactViaResponsesEnabled != nil {
		h.store.SetCompactViaResponsesEnabled(*req.CompactViaResponsesEnabled)
		runtimeCfg.CompactViaResponses = *req.CompactViaResponsesEnabled
		log.Printf("settings updated: compact_via_responses_enabled = %t", *req.CompactViaResponsesEnabled)
	}

	if req.CodexPreflightSSEPassthroughEnabled != nil {
		h.store.SetCodexPreflightSSEPassthroughEnabled(*req.CodexPreflightSSEPassthroughEnabled)
		runtimeCfg.CodexPreflightSSEPassthrough = *req.CodexPreflightSSEPassthroughEnabled
		log.Printf("settings updated: codex_preflight_sse_passthrough_enabled = %t", *req.CodexPreflightSSEPassthroughEnabled)
	}

	if req.FirstTokenExcludesWsAcquire != nil {
		h.store.SetFirstTokenExcludesWsAcquire(*req.FirstTokenExcludesWsAcquire)
		runtimeCfg.FirstTokenExcludesWsAcquire = *req.FirstTokenExcludesWsAcquire
		log.Printf("settings updated: first_token_excludes_ws_acquire = %t", *req.FirstTokenExcludesWsAcquire)
	}

	if req.CodexContinueThinkingEnabled != nil {
		h.store.SetCodexContinueThinkingEnabled(*req.CodexContinueThinkingEnabled)
		runtimeCfg.CodexContinueThinking = *req.CodexContinueThinkingEnabled
		log.Printf("settings updated: codex_continue_thinking_enabled = %t", *req.CodexContinueThinkingEnabled)
	}

	if req.CodexContinueMaxRounds != nil {
		v := database.NormalizeCodexContinueMaxRounds(*req.CodexContinueMaxRounds)
		h.store.SetCodexContinueMaxRounds(v)
		runtimeCfg.CodexContinueMaxRounds = v
		log.Printf("settings updated: codex_continue_max_rounds = %d", v)
	}

	if req.UTLSShutdownTimeoutMinutes != nil {
		v := database.NormalizeUTLSShutdownTimeoutMinutes(*req.UTLSShutdownTimeoutMinutes)
		runtimeCfg.UTLSShutdownTimeoutMin = v
		utlsShutdownTimeoutMinutes = v
		log.Printf("settings updated: utls_shutdown_timeout_minutes = %d", v)
	}

	if req.CodexCLIVersionSyncEnabled != nil {
		h.store.SetCodexCLIVersionSyncEnabled(*req.CodexCLIVersionSyncEnabled)
		runtimeCfg.CodexCLIVersionSyncEnabled = *req.CodexCLIVersionSyncEnabled
		log.Printf("settings updated: codex_cli_version_sync_enabled = %t", *req.CodexCLIVersionSyncEnabled)
	}

	if req.CodexCLIVersionSyncIntervalHours != nil {
		v := database.NormalizeCodexCLIVersionSyncIntervalHours(*req.CodexCLIVersionSyncIntervalHours)
		h.store.SetCodexCLIVersionSyncIntervalHours(v)
		runtimeCfg.CodexCLIVersionSyncIntervalHours = v
		log.Printf("settings updated: codex_cli_version_sync_interval_hours = %d", v)
	}

	if req.SchedulerMode != nil {
		h.store.SetSchedulerMode(*req.SchedulerMode)
		log.Printf("settings updated: scheduler_mode = %s", *req.SchedulerMode)
	}

	if req.AffinityMode != nil {
		h.store.SetAffinityMode(*req.AffinityMode)
		log.Printf("settings updated: affinity_mode = %s", *req.AffinityMode)
	}
	if req.SessionAffinitySpread != nil {
		h.store.SetSessionAffinitySpread(*req.SessionAffinitySpread)
		log.Printf("settings updated: session_affinity_spread = %t", *req.SessionAffinitySpread)
	}
	previousMaxRetries := h.store.GetMaxRetries()
	maxRetriesToApply := previousMaxRetries
	maxRetriesChanged := false
	if req.MaxRetries != nil {
		v := *req.MaxRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		maxRetriesToApply = v
		maxRetriesChanged = true
	}

	previousMaxRateLimitRetries := h.store.GetMaxRateLimitRetries()
	maxRateLimitRetriesToApply := previousMaxRateLimitRetries
	maxRateLimitRetriesChanged := false
	if req.MaxRateLimitRetries != nil {
		v := *req.MaxRateLimitRetries
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		maxRateLimitRetriesToApply = v
		maxRateLimitRetriesChanged = true
	}

	if req.RetryIntervalMS != nil {
		v := *req.RetryIntervalMS
		if v < 0 {
			v = 0
		}
		if v > 30000 {
			v = 30000
		}
		h.store.SetRetryIntervalMS(v)
		log.Printf("settings updated: retry_interval_ms = %d", v)
	}

	if req.TransportRetryPolicy != nil {
		v := database.NormalizeTransportRetryPolicy(*req.TransportRetryPolicy)
		h.store.SetTransportRetryPolicy(v)
		log.Printf("settings updated: transport_retry_policy = %s", v)
	}

	if req.CodexFingerprintDefaultMode != nil {
		if err := validateCodexFingerprintMode(*req.CodexFingerprintDefaultMode); err != nil {
			writeError(c, http.StatusBadRequest, "codex_fingerprint_default_mode "+err.Error())
			return
		}
		v := auth.NormalizeCodexFingerprintMode(*req.CodexFingerprintDefaultMode)
		h.store.SetCodexFingerprintDefaultMode(v)
		log.Printf("settings updated: codex_fingerprint_default_mode = %s", v)
	}

	if req.AllowRemoteMigration != nil {
		if *req.AllowRemoteMigration && !hasAdminSecret {
			writeError(c, http.StatusBadRequest, "Set an admin key before enabling remote migration")
			return
		}
		h.store.SetAllowRemoteMigration(*req.AllowRemoteMigration)
		log.Printf("settings updated: allow_remote_migration = %t", *req.AllowRemoteMigration)
	} else if !hasAdminSecret {
		h.store.SetAllowRemoteMigration(false)
	}

	if req.ModelMapping != nil {
		h.store.SetModelMapping(*req.ModelMapping)
		log.Printf("settings updated: model_mapping")
	}
	if req.CodexModelMapping != nil {
		h.store.SetCodexModelMapping(*req.CodexModelMapping)
		log.Printf("settings updated: codex_model_mapping")
	}
	if req.PayloadRules != nil {
		normalized, err := proxy.NormalizePayloadRulesJSON(*req.PayloadRules)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := proxy.SetPayloadRulesJSON(normalized); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		h.store.SetPayloadRules(normalized)
		log.Printf("settings updated: payload_rules")
	}
	if req.ReasoningEffortModels != nil {
		normalized, err := proxy.NormalizeReasoningEffortModelsJSON(*req.ReasoningEffortModels, proxy.SupportedModelIDs(c.Request.Context(), h.db))
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		h.store.SetReasoningEffortModels(normalized)
		log.Printf("settings updated: reasoning_effort_models")
	}

	if req.ClientCompatMode != nil {
		runtimeCfg.ClientCompatMode = proxy.NormalizeClientCompatMode(*req.ClientCompatMode)
		log.Printf("settings updated: client_compat_mode = %s", runtimeCfg.ClientCompatMode)
	}
	if req.CodexMinCLIVersion != nil {
		runtimeCfg.CodexMinCLIVersion = strings.TrimSpace(*req.CodexMinCLIVersion)
		log.Printf("settings updated: codex_min_cli_version = %s", runtimeCfg.CodexMinCLIVersion)
	}
	if req.CodexUserAgentConfig != nil {
		normalized, err := proxy.NormalizeCodexUserAgentConfigJSON(*req.CodexUserAgentConfig)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		runtimeCfg.CodexUserAgentConfig = normalized
		log.Printf("settings updated: codex_user_agent_config")
	}
	if req.CodexTelemetryEnabled != nil {
		runtimeCfg.CodexTelemetryEnabled = *req.CodexTelemetryEnabled
		log.Printf("settings updated: codex_telemetry_enabled = %t", runtimeCfg.CodexTelemetryEnabled)
	}
	if req.CodexTelemetryTimingDebug != nil {
		runtimeCfg.CodexTelemetryTimingDebug = *req.CodexTelemetryTimingDebug
		log.Printf("settings updated: codex_telemetry_timing_debug = %t", runtimeCfg.CodexTelemetryTimingDebug)
	}
	if req.StreamFlushPolicy != nil {
		runtimeCfg.StreamFlushPolicy = proxy.NormalizeStreamFlushPolicy(*req.StreamFlushPolicy)
		log.Printf("settings updated: stream_flush_policy = %s", runtimeCfg.StreamFlushPolicy)
	}
	if req.StreamFlushIntervalMS != nil {
		runtimeCfg.StreamFlushIntervalMS = *req.StreamFlushIntervalMS
		log.Printf("settings updated: stream_flush_interval_ms = %d", runtimeCfg.StreamFlushIntervalMS)
	}
	if req.FirstTokenMode != nil {
		runtimeCfg.FirstTokenMode = proxy.NormalizeFirstTokenMode(*req.FirstTokenMode)
		log.Printf("settings updated: first_token_mode = %s", runtimeCfg.FirstTokenMode)
	}
	if req.FirstTokenTimeoutSeconds != nil {
		runtimeCfg.FirstTokenTimeoutSec = *req.FirstTokenTimeoutSeconds
		log.Printf("settings updated: first_token_timeout_seconds = %d", runtimeCfg.FirstTokenTimeoutSec)
	}
	if req.BillingTierPolicy != nil {
		runtimeCfg.BillingTierPolicy = proxy.NormalizeBillingTierPolicy(*req.BillingTierPolicy)
		log.Printf("settings updated: billing_tier_policy = %s", runtimeCfg.BillingTierPolicy)
	}
	if req.ShowFullUsageNumbers != nil {
		showFullUsageNumbers = *req.ShowFullUsageNumbers
		log.Printf("settings updated: show_full_usage_numbers = %t", showFullUsageNumbers)
	}
	if req.ShowUpstreamModelMismatch != nil {
		showUpstreamModelMismatch = *req.ShowUpstreamModelMismatch
		log.Printf("settings updated: show_upstream_model_mismatch = %t", showUpstreamModelMismatch)
	}
	if req.PublicKeyUsagePageEnabled != nil {
		publicKeyUsagePageEnabled = *req.PublicKeyUsagePageEnabled
		log.Printf("settings updated: public_key_usage_page_enabled = %t", publicKeyUsagePageEnabled)
	}
	if req.PublicImageStudioPageEnabled != nil {
		publicImageStudioPageEnabled = *req.PublicImageStudioPageEnabled
		log.Printf("settings updated: public_image_studio_page_enabled = %t", publicImageStudioPageEnabled)
	}
	if req.PublicAccountPortalPageEnabled != nil {
		publicAccountPortalPageEnabled = *req.PublicAccountPortalPageEnabled
		log.Printf("settings updated: public_account_portal_page_enabled = %t", publicAccountPortalPageEnabled)
	}
	if req.AutoPause5hThreshold != nil || req.AutoPause7dThreshold != nil {
		t5h := h.store.GetGlobalAutoPause5hThreshold()
		t7d := h.store.GetGlobalAutoPause7dThreshold()
		if req.AutoPause5hThreshold != nil {
			t5h = *req.AutoPause5hThreshold
		}
		if req.AutoPause7dThreshold != nil {
			t7d = *req.AutoPause7dThreshold
		}
		h.store.SetGlobalAutoPauseThresholds(t5h, t7d)
		log.Printf("settings updated: auto_pause thresholds 5h=%.4f 7d=%.4f", t5h, t7d)
	}
	if req.AutoPause5hGuardBandPercent != nil {
		h.store.SetAutoPause5hGuardBandPercent(*req.AutoPause5hGuardBandPercent)
		log.Printf("settings updated: auto_pause_5h_guard_band_percent = %.2f", *req.AutoPause5hGuardBandPercent)
	}
	if req.AutoPause5hGuardConcurrency != nil {
		h.store.SetAutoPause5hGuardConcurrency(*req.AutoPause5hGuardConcurrency)
		log.Printf("settings updated: auto_pause_5h_guard_concurrency = %d", *req.AutoPause5hGuardConcurrency)
	}
	if req.SmartPacingEnabled != nil {
		h.store.SetSmartPacingEnabled(*req.SmartPacingEnabled)
		log.Printf("settings updated: smart_pacing_enabled = %t", *req.SmartPacingEnabled)
	}
	if req.SmartPacingMinConcurrency != nil {
		h.store.SetSmartPacingMinConcurrency(*req.SmartPacingMinConcurrency)
		log.Printf("settings updated: smart_pacing_min_concurrency = %d", *req.SmartPacingMinConcurrency)
	}
	if req.SmartPacingWindows != nil {
		h.store.SetSmartPacingWindows(*req.SmartPacingWindows)
		log.Printf("settings updated: smart_pacing_windows = %s", h.store.GetSmartPacingWindows())
	}
	if req.IgnoreUsageLimitStatus != nil {
		h.store.SetIgnoreUsageLimitStatus(*req.IgnoreUsageLimitStatus)
		log.Printf("settings updated: ignore_usage_limit_status = %t", *req.IgnoreUsageLimitStatus)
	}
	if req.AutoResetCreditsOnExhaustionEnabled != nil {
		runtimeCfg.AutoResetCreditsOnExhaustionEnabled = *req.AutoResetCreditsOnExhaustionEnabled
	}
	if req.AutoResetCreditsEnabled != nil {
		runtimeCfg.AutoResetCreditsEnabled = *req.AutoResetCreditsEnabled
		log.Printf("settings updated: auto_reset_credits_enabled = %t", *req.AutoResetCreditsEnabled)
	}
	if req.AutoResetCreditsBeforeExpiryMin != nil {
		runtimeCfg.AutoResetCreditsBeforeExpiryMin = *req.AutoResetCreditsBeforeExpiryMin
		log.Printf("settings updated: auto_reset_credits_before_expiry_min = %d", *req.AutoResetCreditsBeforeExpiryMin)
	}
	if req.AutoActivate5hWindowEnabled != nil {
		runtimeCfg.AutoActivate5hWindowEnabled = *req.AutoActivate5hWindowEnabled
		log.Printf("settings updated: auto_activate_5h_window_enabled = %t", *req.AutoActivate5hWindowEnabled)
	}
	// Auto-consume and auto-open-window actions are irreversible or cost quota. The pending values are normalized first, but in the database
	// before the save is confirmed, the runtime keeps using the old config, so a failed persist does not let background tasks start running.
	runtimeCfg = proxy.NormalizeRuntimeSettings(runtimeCfg)
	effectiveRuntimeCfg := runtimeCfg
	if autoResetCreditsChanged {
		effectiveRuntimeCfg.AutoResetCreditsEnabled = previousAutoResetCreditsEnabled
		effectiveRuntimeCfg.AutoResetCreditsOnExhaustionEnabled = previousAutoResetCreditsOnExhaustionEnabled
		effectiveRuntimeCfg.AutoResetCreditsBeforeExpiryMin = previousAutoResetCreditsBeforeExpiryMin
	}
	if autoActivate5hChanged {
		effectiveRuntimeCfg.AutoActivate5hWindowEnabled = previousAutoActivate5hWindowEnabled
	}
	effectiveRuntimeCfg = proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
		// CodexSyncedCLIVersion is maintained independently by the background sync task; when an admin saves other settings
		// it must keep the latest value read inside the critical section, so the sync result is not rolled back.
		effectiveRuntimeCfg.CodexSyncedCLIVersion = current.CodexSyncedCLIVersion
		effectiveRuntimeCfg.CodexSyncedDesktopMacBuild = current.CodexSyncedDesktopMacBuild
		effectiveRuntimeCfg.CodexSyncedDesktopWindowsBuild = current.CodexSyncedDesktopWindowsBuild
		effectiveRuntimeCfg.CodexSyncedVSCodeBuild = current.CodexSyncedVSCodeBuild
		return effectiveRuntimeCfg
	})

	usageLogChanged := false
	if req.UsageLogMode != nil {
		usageLogMode = database.NormalizeUsageLogMode(*req.UsageLogMode)
		usageLogChanged = true
		log.Printf("settings updated: usage_log_mode = %s", usageLogMode)
	}
	if req.UsageLogBatchSize != nil {
		usageLogBatchSize = database.NormalizeUsageLogBatchSize(*req.UsageLogBatchSize)
		usageLogChanged = true
		log.Printf("settings updated: usage_log_batch_size = %d", usageLogBatchSize)
	}
	if req.UsageLogFlushIntervalSeconds != nil {
		usageLogFlushIntervalSeconds = database.NormalizeUsageLogFlushIntervalSeconds(*req.UsageLogFlushIntervalSeconds)
		usageLogChanged = true
		log.Printf("settings updated: usage_log_flush_interval_seconds = %d", usageLogFlushIntervalSeconds)
	}
	if usageLogChanged {
		h.db.SetUsageLogConfig(usageLogMode, usageLogBatchSize, usageLogFlushIntervalSeconds)
		usageLogMode = h.db.GetUsageLogMode()
		usageLogBatchSize = h.db.GetUsageLogBatchSize()
		usageLogFlushIntervalSeconds = h.db.GetUsageLogFlushIntervalSeconds()
	}

	promptFilterCfg := h.store.GetPromptFilterConfig()
	promptFilterAdvancedRaw := h.store.GetPromptFilterAdvancedConfig()
	// The database is authoritative for the persisted JSON in multi-instance
	// deployments. Invalid persisted JSON must not replace the Store's last
	// valid raw/effective pair.
	if existingSettings != nil {
		if document, err := promptfilter.ParseAdvancedConfigDocument(existingSettings.PromptFilterAdvancedConfig); err == nil {
			promptFilterAdvancedRaw = document.Raw
			promptFilterCfg.Advanced = document.Effective
		}
	}
	promptFilterChanged := false
	if req.PromptFilterEnabled != nil {
		promptFilterCfg.Enabled = *req.PromptFilterEnabled
		promptFilterChanged = true
	}
	if req.PromptFilterMode != nil {
		promptFilterCfg.Mode = *req.PromptFilterMode
		promptFilterChanged = true
	}
	if req.PromptFilterThreshold != nil {
		promptFilterCfg.Threshold = *req.PromptFilterThreshold
		promptFilterChanged = true
	}
	if req.PromptFilterStrictThreshold != nil {
		promptFilterCfg.StrictThreshold = *req.PromptFilterStrictThreshold
		promptFilterChanged = true
	}
	if req.PromptFilterStrictTerminalEnabled != nil {
		promptFilterCfg.StrictTerminalEnabled = *req.PromptFilterStrictTerminalEnabled
		promptFilterChanged = true
	}
	if req.PromptFilterAdvancedConfig != nil {
		document, err := promptfilter.MergeAdvancedConfigDocument(promptFilterAdvancedRaw, *req.PromptFilterAdvancedConfig)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "prompt_filter_advanced_config JSON is invalid: " + err.Error()})
			return
		}
		promptFilterAdvancedRaw = document.Raw
		promptFilterCfg.Advanced = document.Effective
		promptFilterChanged = true
	}
	if req.PromptFilterLogMatches != nil {
		promptFilterCfg.LogMatches = *req.PromptFilterLogMatches
		promptFilterChanged = true
	}
	if req.PromptFilterMaxTextLength != nil {
		promptFilterCfg.MaxTextLength = *req.PromptFilterMaxTextLength
		promptFilterChanged = true
	}
	if req.PromptFilterSensitiveWords != nil {
		promptFilterCfg.SensitiveWords = *req.PromptFilterSensitiveWords
		promptFilterChanged = true
	}
	if req.PromptFilterCustomPatterns != nil {
		promptFilterCfg.CustomPatterns = submittedPromptFilterCustomPatterns
		promptFilterChanged = true
	}
	if req.PromptFilterDisabledPatterns != nil {
		disabled, err := promptfilter.ParseDisabledPatterns(*req.PromptFilterDisabledPatterns)
		if err != nil {
			writeError(c, http.StatusBadRequest, "Prompt check disabled-rules JSON is invalid: "+err.Error())
			return
		}
		promptFilterCfg.DisabledPatterns = disabled
		promptFilterChanged = true
	}
	if req.PromptFilterReviewEnabled != nil {
		promptFilterCfg.Review.Enabled = *req.PromptFilterReviewEnabled
		promptFilterChanged = true
	}
	if req.PromptFilterReviewAPIKey != nil {
		if key := strings.TrimSpace(*req.PromptFilterReviewAPIKey); key != "" {
			promptFilterCfg.Review.APIKey = key
			promptFilterChanged = true
		}
	}
	if req.PromptFilterReviewBaseURL != nil {
		promptFilterCfg.Review.BaseURL = strings.TrimSpace(*req.PromptFilterReviewBaseURL)
		promptFilterChanged = true
	}
	if req.PromptFilterReviewModel != nil {
		promptFilterCfg.Review.Model = strings.TrimSpace(*req.PromptFilterReviewModel)
		promptFilterChanged = true
	}
	if req.PromptFilterReviewTimeoutSeconds != nil {
		promptFilterCfg.Review.TimeoutSeconds = *req.PromptFilterReviewTimeoutSeconds
		promptFilterChanged = true
	}
	if req.PromptFilterReviewFailClosed != nil {
		promptFilterCfg.Review.FailClosed = *req.PromptFilterReviewFailClosed
		promptFilterChanged = true
	}
	if promptFilterChanged {
		promptFilterCfg.Review.Adapter = promptFilterCfg.Advanced.ReviewAdapter
		promptFilterCfg = promptfilter.NormalizeConfig(promptFilterCfg)
		if promptFilterCfg.Review.Enabled && strings.TrimSpace(promptFilterCfg.Review.APIKey) == "" {
			writeError(c, http.StatusBadRequest, "Prompt check review API key is required when secondary review is enabled")
			return
		}
		if err := promptfilter.ValidateReviewConfig(promptFilterCfg.Review); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt check review config is invalid: "+err.Error())
			return
		}
		if _, err := promptfilter.NewEngine(promptFilterCfg); err != nil {
			writeError(c, http.StatusBadRequest, "Prompt check rules are invalid: "+err.Error())
			return
		}
	}

	// Resin sticky proxy pool configuration
	resinURL := ""
	resinPlatformName := ""
	if existingSettings != nil {
		resinURL = existingSettings.ResinURL
		resinPlatformName = existingSettings.ResinPlatformName
	}
	if req.ResinURL != nil {
		resinURL = *req.ResinURL
		log.Printf("settings updated: resin_url")
	}
	if req.ResinPlatformName != nil {
		resinPlatformName = *req.ResinPlatformName
		log.Printf("settings updated: resin_platform_name")
	}
	if req.ResinURL != nil || req.ResinPlatformName != nil {
		proxy.SetResinConfig(&proxy.ResinConfig{
			BaseURL:      resinURL,
			PlatformName: resinPlatformName,
		})
		if strings.TrimSpace(resinURL) != "" && strings.TrimSpace(resinPlatformName) != "" {
			auth.ResinRequestDecorator = func(targetURL, accountID string) string {
				return proxy.BuildReverseProxyURL(targetURL)
			}
		} else {
			auth.ResinRequestDecorator = nil
		}
	}

	// Image storage backend configuration
	imgCfg := imagestore.CurrentConfig()
	imgChanged := false
	if req.ImageStorageBackend != nil {
		imgCfg.Backend = *req.ImageStorageBackend
		imgChanged = true
	}
	if req.ImageS3Endpoint != nil {
		imgCfg.Endpoint = *req.ImageS3Endpoint
		imgChanged = true
	}
	if req.ImageS3Region != nil {
		imgCfg.Region = *req.ImageS3Region
		imgChanged = true
	}
	if req.ImageS3Bucket != nil {
		imgCfg.Bucket = *req.ImageS3Bucket
		imgChanged = true
	}
	if req.ImageS3AccessKey != nil {
		imgCfg.AccessKey = *req.ImageS3AccessKey
		imgChanged = true
	}
	if req.ImageS3SecretKey != nil {
		imgCfg.SecretKey = *req.ImageS3SecretKey
		imgChanged = true
	}
	if req.ImageS3Prefix != nil {
		imgCfg.Prefix = *req.ImageS3Prefix
		imgChanged = true
	}
	if req.ImageS3ForcePathStyle != nil {
		imgCfg.ForcePathStyle = *req.ImageS3ForcePathStyle
		imgChanged = true
	}
	imgCfg.LocalDir = imageAssetDir()
	if imgChanged {
		if err := imagestore.Configure(imgCfg); err != nil {
			writeError(c, http.StatusBadRequest, "image storage config is invalid: "+err.Error())
			return
		}
		// Configure already calls Normalize internally; re-read the result for persistence.
		imgCfg = imagestore.CurrentConfig()
		log.Printf("settings updated: image_storage_backend = %s", imgCfg.Backend)
	}
	imgConfigJSON, encodeErr := imagestore.EncodeConfigJSON(imgCfg)
	if encodeErr != nil {
		log.Printf("image storage config serialization failed: %v", encodeErr)
		imgConfigJSON = "{}"
	}

	// Persist to the database
	systemSettings := &database.SystemSettings{
		SiteName:                            siteName,
		SiteLogo:                            siteLogo,
		MaxConcurrency:                      maxConcurrencyToApply,
		GlobalRPM:                           globalRPMToApply,
		TestModel:                           h.store.GetTestModel(),
		CodexImagesMainModel:                codexImagesMainModel,
		TestContent:                         h.store.GetTestContent(),
		TestConcurrency:                     h.store.GetTestConcurrency(),
		BackgroundRefreshIntervalMinutes:    h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:             h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:               h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled:  h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:        h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                            h.store.GetLazyMode(),
		CodexOAuthKeepaliveEnabled:          h.store.GetCodexOAuthKeepalive(),
		ProxyURL:                            h.store.GetProxyURL(),
		PgMaxConns:                          h.pgMaxConns,
		RedisPoolSize:                       h.redisPoolSize,
		AutoCleanUnauthorized:               h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:                h.store.GetAutoCleanRateLimited(),
		AdminSecret:                         currentAdminSecret,
		AutoCleanFullUsage:                  h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                      h.store.GetAutoCleanError(),
		AutoCleanExpired:                    h.store.GetAutoCleanExpired(),
		AutoResetCreditsEnabled:             runtimeCfg.AutoResetCreditsEnabled,
		AutoResetCreditsOnExhaustionEnabled: runtimeCfg.AutoResetCreditsOnExhaustionEnabled,
		AutoResetCreditsBeforeExpiryMin:     runtimeCfg.AutoResetCreditsBeforeExpiryMin,
		AutoActivate5hWindowEnabled:         runtimeCfg.AutoActivate5hWindowEnabled,
		ProxyPoolEnabled:                    h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:                h.store.FastSchedulerEnabled(),
		SchedulerEngine:                     h.store.SchedulerEngine(),
		CodexForceWebsocket:                 h.store.CodexForceWebsocket(),
		CodexRequestCompression:             h.store.CodexRequestCompression(),
		CodexWSWeakNetworkMode:              runtimeCfg.CodexWSWeakNetworkMode,
		CodexWSKeepaliveEnabled:             h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:         h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:           h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:           h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:             h.store.CodexWSSilentMaxRetries(),
		CodexWSSizeRouterEnabled:            h.store.CodexWSSizeRouterEnabled(),
		CodexWSBusyAcquireMaxWaitSec:        h.store.CodexWSBusyAcquireMaxWaitSec(),
		CodexWSBusyOverflowEnabled:          h.store.CodexWSBusyOverflowEnabled(),
		CodexWSBusyPatienceSec:              h.store.CodexWSBusyPatienceSec(),
		CodexWSStatelessSlots:               h.store.CodexWSStatelessSlots(),
		CodexWSDownstreamKeepaliveSlots:     h.store.CodexWSDownstreamKeepaliveSlots(),
		GithubToken:                         h.store.GithubToken(),
		GithubProxyURL:                      h.store.GithubProxyURL(),
		CodexOverloadPauseEnabled:           runtimeCfg.CodexOverloadPauseEnabled,
		CodexOverloadThresholdPercent:       runtimeCfg.CodexOverloadThresholdPercent,
		CodexOverloadPauseMinutes:           runtimeCfg.CodexOverloadPauseMinutes,
		CodexOverloadWindowMinutes:          runtimeCfg.CodexOverloadWindowMinutes,
		OverflowAutoCompactEnabled:          h.store.OverflowAutoCompactEnabled(),
		CompactViaResponsesEnabled:          h.store.CompactViaResponsesEnabled(),
		CodexPreflightSSEPassthroughEnabled: h.store.CodexPreflightSSEPassthroughEnabled(),
		FirstTokenExcludesWsAcquire:         h.store.FirstTokenExcludesWsAcquire(),
		CodexContinueThinkingEnabled:        h.store.CodexContinueThinkingEnabled(),
		CodexContinueMaxRounds:              h.store.CodexContinueMaxRounds(),
		UTLSShutdownTimeoutMinutes:          utlsShutdownTimeoutMinutes,
		CodexCLIVersionSyncEnabled:          h.store.CodexCLIVersionSyncEnabled(),
		CodexCLIVersionSyncIntervalHours:    h.store.CodexCLIVersionSyncIntervalHours(),
		CodexSyncedCLIVersion:               proxy.CurrentRuntimeSettings().CodexSyncedCLIVersion,
		CodexSyncedDesktopMacBuild:          proxy.CurrentRuntimeSettings().CodexSyncedDesktopMacBuild,
		CodexSyncedDesktopWindowsBuild:      proxy.CurrentRuntimeSettings().CodexSyncedDesktopWindowsBuild,
		CodexSyncedVSCodeBuild:              proxy.CurrentRuntimeSettings().CodexSyncedVSCodeBuild,
		SchedulerMode:                       h.store.GetSchedulerMode(),
		AffinityMode:                        h.store.GetAffinityMode(),
		SessionAffinitySpread:               h.store.GetSessionAffinitySpread(),
		SessionSlotBufferEnabled:            sessionSlotBufferEnabled,
		SessionSlotBufferSeconds:            sessionSlotBufferSeconds,
		MaxRetries:                          maxRetriesToApply,
		MaxRateLimitRetries:                 maxRateLimitRetriesToApply,
		RetryIntervalMS:                     h.store.GetRetryIntervalMS(),
		TransportRetryPolicy:                h.store.GetTransportRetryPolicy(),
		ContinuousRetryPolicy:               database.EncodeContinuousRetryPolicy(h.store.GetContinuousRetryPolicy()),
		CodexFingerprintDefaultMode:         h.store.GetCodexFingerprintDefaultMode(),
		AllowRemoteMigration:                h.store.GetAllowRemoteMigration() && hasAdminSecret,
		ModelMapping:                        h.store.GetModelMapping(),
		CodexModelMapping:                   h.store.GetCodexModelMapping(),
		PayloadRules:                        h.store.GetPayloadRules(),
		ReasoningEffortModels:               h.store.GetReasoningEffortModels(),
		ResinURL:                            resinURL,
		ResinPlatformName:                   resinPlatformName,
		PromptFilterEnabled:                 promptFilterCfg.Enabled,
		PromptFilterMode:                    promptFilterCfg.Mode,
		PromptFilterThreshold:               promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:         promptFilterCfg.StrictThreshold,
		PromptFilterStrictTerminalEnabled:   promptFilterCfg.StrictTerminalEnabled,
		PromptFilterAdvancedConfig:          promptFilterAdvancedRaw,
		PromptFilterLogMatches:              promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:           promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:          promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:          promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PreservePromptFilterCustomPatterns:  req.PromptFilterCustomPatterns == nil,
		PromptFilterDisabledPatterns:        promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:           promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKey:            promptFilterCfg.Review.APIKey,
		PreservePromptFilterReviewAPIKey:    req.PromptFilterReviewAPIKey == nil || strings.TrimSpace(*req.PromptFilterReviewAPIKey) == "",
		PromptFilterReviewBaseURL:           promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:             promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:    promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:        promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                    runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                  runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:                runtimeCfg.CodexUserAgentConfig,
		CodexTelemetryEnabled:               runtimeCfg.CodexTelemetryEnabled,
		CodexTelemetryTimingDebug:           runtimeCfg.CodexTelemetryTimingDebug,
		UsageLogMode:                        usageLogMode,
		UsageLogBatchSize:                   usageLogBatchSize,
		UsageLogFlushIntervalSeconds:        usageLogFlushIntervalSeconds,
		StreamFlushPolicy:                   runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:               runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                      runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:            runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                   runtimeCfg.BillingTierPolicy,
		ShowFullUsageNumbers:                showFullUsageNumbers,
		ShowUpstreamModelMismatch:           showUpstreamModelMismatch,
		PublicKeyUsagePageEnabled:           publicKeyUsagePageEnabled,
		PublicImageStudioPageEnabled:        publicImageStudioPageEnabled,
		PublicAccountPortalPageEnabled:      publicAccountPortalPageEnabled,
		ImageStorageConfig:                  imgConfigJSON,
		BackgroundConfig:                    encodeBackgroundConfig(bgCfg),
		AutoPause5hThreshold:                h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:                h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:         h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:         h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                  h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:           h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                  h.store.GetSmartPacingWindows(),
		IgnoreUsageLimitStatus:              h.store.IgnoreUsageLimitStatus(),
		ModelPricingOverrides:               modelPricingOverrides,
		ModelPricingSyncURL:                 modelPricingSyncURL,
	}

	// The guard columns and the other system_settings columns are committed together in the same row and transaction: if any write fails,
	// no saved guard config is left behind. Hot apply must run only after the transaction commits successfully.
	var committedOutputGuard database.OutputGuardSettings
	if outputGuardUpdateRequested {
		committedOutputGuard, err = h.db.UpdateSystemSettingsWithOutputGuard(c.Request.Context(), systemSettings, outputGuardUpdate)
	} else {
		err = h.db.UpdateSystemSettings(c.Request.Context(), systemSettings)
	}
	if err != nil {
		log.Printf("cannot persist settings: %v", err)
		if outputGuardUpdateRequested {
			// A validation failure inside the transaction is a caller input problem and maps to 400; everything else (database/IO, etc.) is 500.
			if errors.Is(err, database.ErrInvalidOutputGuardSettings) {
				writeError(c, http.StatusBadRequest, err.Error())
				return
			}
			writeError(c, http.StatusInternalServerError, "Failed to save guard settings, settings not applied: "+err.Error())
			return
		}
		if req.CodexImagesMainModel != nil {
			writeError(c, http.StatusInternalServerError, "Failed to save image generation settings, text-driven model not applied")
			return
		}
		if req.SessionSlotBufferEnabled != nil || req.SessionSlotBufferSeconds != nil {
			writeError(c, http.StatusInternalServerError, "Failed to save session concurrency slot buffer settings, settings not applied")
			return
		}
		if modelCooldownUpdateRequested {
			writeError(c, http.StatusInternalServerError, "Cannot persist system settings before saving model cooldown settings")
			return
		}
		if responseCacheUpdateRequested {
			writeError(c, http.StatusInternalServerError, "Cannot persist system settings before saving response cache settings")
			return
		}
		if modelsListReadLimitChanged {
			writeError(c, http.StatusInternalServerError, "Cannot persist system settings before saving the model list read limit")
			return
		}
		if promptFilterChanged {
			writeError(c, http.StatusInternalServerError, "Failed to save Prompt check settings, settings not applied")
			return
		}
		if autoResetCreditsChanged {
			runtimeCfg = effectiveRuntimeCfg
			writeError(c, http.StatusInternalServerError, "Failed to save auto-consume settings, settings not applied")
			return
		}
		if autoActivate5hChanged {
			runtimeCfg = effectiveRuntimeCfg
			writeError(c, http.StatusInternalServerError, "Failed to save 5h window auto-activation settings, settings not applied")
			return
		}
		if continuousRetryChanged {
			writeError(c, http.StatusInternalServerError, "Failed to save continuous retry policy, settings not applied")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to save system settings, settings not applied: "+err.Error())
		return
	} else {
		// Right after the settings transaction commits, hot-apply the base runtime limits and the global rate limit,
		// so that if later independent steps such as UpdateContinuousRetryPolicy fail,
		// the concurrency/retry/RPM config already committed to the database also stays consistent in this instance's memory.
		if maxConcurrencyChanged {
			h.store.SetMaxConcurrency(maxConcurrencyToApply)
			log.Printf("settings updated: max_concurrency = %d", maxConcurrencyToApply)
		}
		if maxRetriesChanged {
			h.store.SetMaxRetries(maxRetriesToApply)
			log.Printf("settings updated: max_retries = %d", maxRetriesToApply)
		}
		if maxRateLimitRetriesChanged {
			h.store.SetMaxRateLimitRetries(maxRateLimitRetriesToApply)
			log.Printf("settings updated: max_rate_limit_retries = %d", maxRateLimitRetriesToApply)
		}
		if globalRPMChanged {
			h.rateLimiter.UpdateRPM(globalRPMToApply)
			log.Printf("settings updated: global_rpm = %d", globalRPMToApply)
		}
		if outputGuardUpdateRequested {
			outputGuardSettings = committedOutputGuard
			if proxy.ApplyOutputGuardSettings(committedOutputGuard) {
				log.Printf("settings updated: output_guard generation=%d", committedOutputGuard.ConfigGeneration)
			} else {
				log.Printf("output guard config committed, but runtime apply was rejected (a newer generation already exists or the config is invalid): generation=%d", committedOutputGuard.ConfigGeneration)
			}
		}
		if req.SessionSlotBufferSeconds != nil {
			h.store.SetSessionSlotBuffer(time.Duration(sessionSlotBufferSeconds) * time.Second)
			log.Printf("settings updated: session_slot_buffer_seconds = %d", sessionSlotBufferSeconds)
		}
		runtimeCfg.CodexImagesMainModel = codexImagesMainModel
		proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
			current.CodexImagesMainModel = codexImagesMainModel
			return current
		})
		if req.SessionSlotBufferEnabled != nil {
			h.store.SetSessionSlotBufferEnabled(sessionSlotBufferEnabled)
			log.Printf("settings updated: session_slot_buffer_enabled = %t", sessionSlotBufferEnabled)
		}
		if continuousRetryChanged {
			committed, updateErr := h.db.UpdateContinuousRetryPolicy(c.Request.Context(), continuousRetryUpdate)
			if updateErr != nil {
				writeError(c, http.StatusInternalServerError, "Failed to save continuous retry policy")
				return
			}
			continuousRetryPolicy = committed
			h.store.SetContinuousRetryPolicy(continuousRetryPolicy)
			proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
				current.ContinuousRetryPolicy = continuousRetryPolicy
				return current
			})
			log.Printf("settings updated: continuous_retry enabled=%t catch_all=%t categories=%d status_codes=%d error_codes=%d", continuousRetryPolicy.Enabled, continuousRetryPolicy.CatchAll, len(continuousRetryPolicy.Categories), len(continuousRetryPolicy.StatusCodes), len(continuousRetryPolicy.ErrorCodes))
		}
		if promptFilterChanged {
			if req.PromptFilterCustomPatterns == nil {
				// The database preserved this field atomically because the request did
				// not edit rules. Reload the committed value so a different replica's
				// just-published rule is not temporarily replaced in this Store by the
				// older snapshot used to edit unrelated Prompt settings.
				if persisted, readErr := h.db.GetSystemSettings(c.Request.Context()); readErr == nil && persisted != nil {
					if patterns, parseErr := promptfilter.ParseCustomPatterns(persisted.PromptFilterCustomPatterns); parseErr == nil {
						promptFilterCfg.CustomPatterns = patterns
					} else {
						log.Printf("cannot parse the Prompt custom rules in the database, keeping the current runtime rules: %v", parseErr)
						promptFilterCfg.CustomPatterns = h.store.GetPromptFilterConfig().CustomPatterns
					}
				} else {
					log.Printf("cannot re-read the Prompt custom rules, keeping the current runtime rules: %v", readErr)
					promptFilterCfg.CustomPatterns = h.store.GetPromptFilterConfig().CustomPatterns
				}
			}
			if err := h.store.SetPromptFilterConfigWithAdvancedRaw(promptFilterCfg, promptFilterAdvancedRaw); err != nil {
				// The document was validated before persistence, so reaching this
				// branch indicates an internal invariant violation. Keep the last
				// valid runtime state rather than publishing a partial update.
				log.Printf("cannot publish the Prompt check runtime config: %v", err)
				writeError(c, http.StatusInternalServerError, "Prompt check settings were saved, but the runtime config update failed")
				return
			}
			log.Printf("settings updated: prompt_filter enabled=%t mode=%s threshold=%d", promptFilterCfg.Enabled, promptFilterCfg.Mode, promptFilterCfg.Threshold)
		}
		if autoResetCreditsChanged {
			runtimeCfg = proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
				runtimeCfg.CodexSyncedCLIVersion = current.CodexSyncedCLIVersion
				runtimeCfg.CodexSyncedDesktopMacBuild = current.CodexSyncedDesktopMacBuild
				runtimeCfg.CodexSyncedDesktopWindowsBuild = current.CodexSyncedDesktopWindowsBuild
				runtimeCfg.CodexSyncedVSCodeBuild = current.CodexSyncedVSCodeBuild
				return runtimeCfg
			})
			h.triggerAutoResetCreditsScan()
		}
		if autoActivate5hChanged {
			runtimeCfg = proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
				runtimeCfg.CodexSyncedCLIVersion = current.CodexSyncedCLIVersion
				runtimeCfg.CodexSyncedDesktopMacBuild = current.CodexSyncedDesktopMacBuild
				runtimeCfg.CodexSyncedDesktopWindowsBuild = current.CodexSyncedDesktopWindowsBuild
				runtimeCfg.CodexSyncedVSCodeBuild = current.CodexSyncedVSCodeBuild
				return runtimeCfg
			})
			h.triggerAutoActivate5hScan()
		}
		if modelsListReadLimitChanged {
			if updateErr := h.db.UpdateModelsListReadMaxBytes(c.Request.Context(), *req.ModelsListReadMaxBytes); updateErr != nil {
				writeError(c, http.StatusInternalServerError, "Failed to save the model list read limit: "+updateErr.Error())
				return
			}
			modelsListReadMaxBytes = *req.ModelsListReadMaxBytes
			runtimeCfg = proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
				current.ModelsListReadMaxBytes = modelsListReadMaxBytes
				return current
			})
			log.Printf("settings updated: models_list_read_max_bytes = %d", modelsListReadMaxBytes)
		}
	}

	if modelCooldownUpdateRequested {
		committed, updateErr := h.db.UpdateModelCooldownSettings(c.Request.Context(), database.ModelCooldownSettingsUpdate{
			RelayMode:           req.RelayModelCooldownMode,
			RelaySeconds:        req.RelayModelCooldownSeconds,
			RelayBackoffEnabled: req.RelayModelCooldownBackoffEnabled,
			OAuthMode:           req.OAuthModelCooldownMode,
			OAuthSeconds:        req.OAuthModelCooldownSeconds,
			OAuthBackoffEnabled: req.OAuthModelCooldownBackoffEnabled,

			OAuthUnsupportedSeconds: req.OAuthUnsupportedCooldownSeconds,
		})
		if updateErr != nil {
			writeError(c, http.StatusInternalServerError, updateErr.Error())
			return
		}
		previousUnsupported := h.store.UnsupportedModelCooldown()
		h.store.SetModelCooldownSettings(committed)
		// Existing cooldowns longer than the new setting would otherwise keep
		// blocking the model after it was disabled or shortened.
		if next := h.store.UnsupportedModelCooldown(); next < previousUnsupported {
			if cleared := h.store.ClearModelCooldownsByReason(auth.ModelCooldownReasonUnsupported, next); cleared > 0 {
				log.Printf("settings updated: oauth_unsupported_model_cooldown_seconds = %d, cleared %d model unsupported-cooldown entries that exceed the new duration", committed.UnsupportedCooldownSeconds(), cleared)
			}
		}
	}

	if responseCacheUpdateRequested {
		committed, updateErr := cacheSettingsStore.UpdateResponseCacheSettings(
			c.Request.Context(),
			responseCacheUpdate,
		)
		if updateErr != nil {
			if errors.Is(updateErr, database.ErrInvalidResponseCacheSettings) {
				writeError(c, http.StatusBadRequest, updateErr.Error())
			} else {
				writeError(c, http.StatusInternalServerError, "Failed to save response cache settings: "+updateErr.Error())
			}
			return
		}
		responseCacheSettings = committed
		proxy.ApplyResponseCacheSettings(committed)
	} else {
		latest, readErr := cacheSettingsStore.GetResponseCacheSettings(c.Request.Context())
		if readErr != nil {
			writeError(c, http.StatusInternalServerError, "Failed to read response cache settings: "+readErr.Error())
			return
		}
		responseCacheSettings = latest
	}

	if h.store.GetAutoCleanUnauthorized() || h.store.GetAutoCleanRateLimited() || h.store.GetAutoCleanError() {
		h.store.TriggerAutoCleanupAsync()
	}

	adminSecretForDisplay := currentAdminSecret
	adminAuthSource := func() string {
		_, source := h.resolveAdminSecret(c.Request.Context())
		return source
	}()
	if adminAuthSource == "env" {
		adminSecretForDisplay = ""
	}
	modelCooldownSettings := h.store.GetModelCooldownSettings()

	c.JSON(http.StatusOK, settingsResponse{
		SiteName:                            siteName,
		SiteLogo:                            siteLogo,
		BackgroundImage:                     bgCfg.Image,
		BackgroundOpacity:                   bgCfg.Opacity,
		BackgroundBlur:                      bgCfg.Blur,
		BackgroundGlassOpacity:              bgCfg.GlassOpacity,
		BackgroundGlassBlur:                 bgCfg.GlassBlur,
		MaxConcurrency:                      h.store.GetMaxConcurrency(),
		GlobalRPM:                           h.rateLimiter.GetRPM(),
		TestModel:                           h.store.GetTestModel(),
		CodexImagesMainModel:                runtimeCfg.CodexImagesMainModel,
		CodexImagesDefaultMainModel:         proxy.ImagesDefaultMainModel(),
		TestContent:                         h.store.GetTestContent(),
		TestConcurrency:                     h.store.GetTestConcurrency(),
		ResponseCacheLocalMaxBytes:          responseCacheSettings.LocalMaxBytes,
		ResponseCacheLocalMaxEntryBytes:     responseCacheSettings.LocalMaxEntryBytes,
		ResponseCacheReconstructMaxBytes:    responseCacheSettings.ReconstructMaxBytes,
		ResponseCacheWritePolicy:            responseCacheSettings.WritePolicy,
		ResponseCacheConfigGeneration:       responseCacheSettings.Generation,
		RelayModelCooldownMode:              modelCooldownSettings.RelayMode,
		RelayModelCooldownSeconds:           modelCooldownSettings.RelaySeconds,
		RelayModelCooldownBackoffEnabled:    modelCooldownSettings.RelayBackoffEnabled,
		OAuthModelCooldownMode:              modelCooldownSettings.OAuthMode,
		OAuthModelCooldownSeconds:           modelCooldownSettings.OAuthSeconds,
		OAuthModelCooldownBackoffEnabled:    modelCooldownSettings.OAuthBackoffEnabled,
		OAuthUnsupportedCooldownSeconds:     modelCooldownSettings.UnsupportedCooldownSeconds(),
		BackgroundRefreshIntervalMinutes:    h.store.GetBackgroundRefreshIntervalMinutes(),
		UsageProbeMaxAgeMinutes:             h.store.GetUsageProbeMaxAgeMinutes(),
		UsageProbeConcurrency:               h.store.GetUsageProbeConcurrency(),
		UsageProbeResponsesFallbackEnabled:  h.store.UsageProbeResponsesFallbackEnabled(),
		RecoveryProbeIntervalMinutes:        h.store.GetRecoveryProbeIntervalMinutes(),
		LazyMode:                            h.store.GetLazyMode(),
		CodexOAuthKeepaliveEnabled:          h.store.GetCodexOAuthKeepalive(),
		ProxyURL:                            h.store.GetProxyURL(),
		PgMaxConns:                          h.pgMaxConns,
		RedisPoolSize:                       h.redisPoolSize,
		AutoCleanUnauthorized:               h.store.GetAutoCleanUnauthorized(),
		AutoCleanRateLimited:                h.store.GetAutoCleanRateLimited(),
		AdminSecret:                         adminSecretForDisplay,
		AdminAuthSource:                     adminAuthSource,
		AutoCleanFullUsage:                  h.store.GetAutoCleanFullUsage(),
		AutoCleanError:                      h.store.GetAutoCleanError(),
		AutoCleanExpired:                    h.store.GetAutoCleanExpired(),
		AutoResetCreditsEnabled:             runtimeCfg.AutoResetCreditsEnabled,
		AutoResetCreditsOnExhaustionEnabled: runtimeCfg.AutoResetCreditsOnExhaustionEnabled,
		AutoResetCreditsBeforeExpiryMin:     runtimeCfg.AutoResetCreditsBeforeExpiryMin,
		AutoActivate5hWindowEnabled:         runtimeCfg.AutoActivate5hWindowEnabled,
		ProxyPoolEnabled:                    h.store.GetProxyPoolEnabled(),
		FastSchedulerEnabled:                h.store.FastSchedulerEnabled(),
		SchedulerEngine:                     h.store.SchedulerEngine(),
		CodexForceWebsocket:                 h.store.CodexForceWebsocket(),
		CodexRequestCompression:             h.store.CodexRequestCompression(),
		CodexWSWeakNetworkMode:              runtimeCfg.CodexWSWeakNetworkMode,
		CodexWSKeepaliveEnabled:             h.store.CodexWSKeepaliveEnabled(),
		CodexWSKeepaliveIntervalSec:         h.store.CodexWSKeepaliveIntervalSec(),
		CodexWSHideUpstreamErrors:           h.store.CodexWSHideUpstreamErrors(),
		CodexWSSilentRetryEnabled:           h.store.CodexWSSilentRetryEnabled(),
		CodexWSSilentMaxRetries:             h.store.CodexWSSilentMaxRetries(),
		CodexWSSizeRouterEnabled:            h.store.CodexWSSizeRouterEnabled(),
		CodexWSBusyAcquireMaxWaitSec:        h.store.CodexWSBusyAcquireMaxWaitSec(),
		CodexWSBusyOverflowEnabled:          h.store.CodexWSBusyOverflowEnabled(),
		CodexWSBusyPatienceSec:              h.store.CodexWSBusyPatienceSec(),
		CodexWSStatelessSlots:               h.store.CodexWSStatelessSlots(),
		CodexWSDownstreamKeepaliveSlots:     h.store.CodexWSDownstreamKeepaliveSlots(),
		GithubTokenConfigured:               h.store.GithubToken() != "",
		GithubProxyURL:                      h.store.GithubProxyURL(),
		CodexOverloadPauseEnabled:           runtimeCfg.CodexOverloadPauseEnabled,
		CodexOverloadThresholdPercent:       runtimeCfg.CodexOverloadThresholdPercent,
		CodexOverloadPauseMinutes:           runtimeCfg.CodexOverloadPauseMinutes,
		CodexOverloadWindowMinutes:          runtimeCfg.CodexOverloadWindowMinutes,
		OverflowAutoCompactEnabled:          h.store.OverflowAutoCompactEnabled(),
		CompactViaResponsesEnabled:          h.store.CompactViaResponsesEnabled(),
		CodexPreflightSSEPassthroughEnabled: h.store.CodexPreflightSSEPassthroughEnabled(),
		FirstTokenExcludesWsAcquire:         h.store.FirstTokenExcludesWsAcquire(),
		CodexContinueThinkingEnabled:        h.store.CodexContinueThinkingEnabled(),
		CodexContinueMaxRounds:              h.store.CodexContinueMaxRounds(),
		UTLSShutdownTimeoutMinutes:          utlsShutdownTimeoutMinutes,
		CodexCLIVersionSyncEnabled:          h.store.CodexCLIVersionSyncEnabled(),
		CodexCLIVersionSyncIntervalHours:    h.store.CodexCLIVersionSyncIntervalHours(),
		CodexSyncedCLIVersion:               proxy.CurrentRuntimeSettings().CodexSyncedCLIVersion,
		CodexSyncedDesktopMacBuild:          proxy.CurrentRuntimeSettings().CodexSyncedDesktopMacBuild,
		CodexSyncedDesktopWindowsBuild:      proxy.CurrentRuntimeSettings().CodexSyncedDesktopWindowsBuild,
		CodexSyncedVSCodeBuild:              proxy.CurrentRuntimeSettings().CodexSyncedVSCodeBuild,
		CodexClientVersions:                 proxy.CodexClientVersionCacheView(),
		CodexEffectiveCLIVersion:            proxy.LatestCodexCLIVersionForHeaders(),
		SchedulerMode:                       h.store.GetSchedulerMode(),
		AffinityMode:                        h.store.GetAffinityMode(),
		SessionAffinitySpread:               h.store.GetSessionAffinitySpread(),
		SessionSlotBufferEnabled:            h.store.SessionSlotBufferEnabled(),
		SessionSlotBufferSeconds:            int(h.store.GetSessionSlotBuffer() / time.Second),
		MaxRetries:                          h.store.GetMaxRetries(),
		MaxRateLimitRetries:                 h.store.GetMaxRateLimitRetries(),
		RetryIntervalMS:                     h.store.GetRetryIntervalMS(),
		TransportRetryPolicy:                h.store.GetTransportRetryPolicy(),
		ContinuousRetryEnabled:              continuousRetryPolicy.Enabled,
		ContinuousRetryCatchAll:             continuousRetryPolicy.CatchAll,
		ContinuousRetryCategories:           continuousRetryPolicy.Categories,
		ContinuousRetryStatusCodes:          continuousRetryPolicy.StatusCodes,
		ContinuousRetryErrorCodes:           continuousRetryPolicy.ErrorCodes,
		ContinuousRetryMaxDurationSeconds:   continuousRetryPolicy.MaxDurationSeconds,
		CodexFingerprintDefaultMode:         h.store.GetCodexFingerprintDefaultMode(),
		AllowRemoteMigration:                h.store.GetAllowRemoteMigration() && adminAuthSource != "disabled",
		DatabaseDriver:                      h.databaseDriver,
		DatabaseLabel:                       h.databaseLabel,
		CacheDriver:                         h.cacheDriver,
		CacheLabel:                          h.cacheLabel,
		ExpiredCleaned:                      expiredCleaned,
		ModelMapping:                        h.store.GetModelMapping(),
		CodexModelMapping:                   h.store.GetCodexModelMapping(),
		PayloadRules:                        h.store.GetPayloadRules(),
		ReasoningEffortModels:               h.store.GetReasoningEffortModels(),
		ResinURL:                            resinURL,
		ResinPlatformName:                   resinPlatformName,
		CodexEgress:                         proxy.CurrentCodexEgressSummary(),
		PromptFilterEnabled:                 promptFilterCfg.Enabled,
		PromptFilterMode:                    promptFilterCfg.Mode,
		PromptFilterThreshold:               promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:         promptFilterCfg.StrictThreshold,
		PromptFilterStrictTerminalEnabled:   promptFilterCfg.StrictTerminalEnabled,
		PromptFilterAdvancedConfig:          promptFilterAdvancedRaw,
		PromptFilterLogMatches:              promptFilterCfg.LogMatches,
		PromptFilterMaxTextLength:           promptFilterCfg.MaxTextLength,
		PromptFilterSensitiveWords:          promptFilterCfg.SensitiveWords,
		PromptFilterCustomPatterns:          promptfilter.MarshalCustomPatterns(promptFilterCfg.CustomPatterns),
		PromptFilterPatternQuarantines:      promptFilterPatternQuarantines,
		PromptFilterDisabledPatterns:        promptfilter.MarshalDisabledPatterns(promptFilterCfg.DisabledPatterns),
		PromptFilterReviewEnabled:           promptFilterCfg.Review.Enabled,
		PromptFilterReviewAPIKeyConfigured:  promptFilterCfg.Review.APIKey != "",
		PromptFilterReviewAPIKeyCount:       len(promptFilterCfg.Review.APIKeyList()),
		PromptFilterReviewBaseURL:           promptFilterCfg.Review.BaseURL,
		PromptFilterReviewModel:             promptFilterCfg.Review.Model,
		PromptFilterReviewTimeoutSeconds:    promptFilterCfg.Review.TimeoutSeconds,
		PromptFilterReviewFailClosed:        promptFilterCfg.Review.FailClosed,
		ClientCompatMode:                    runtimeCfg.ClientCompatMode,
		CodexMinCLIVersion:                  runtimeCfg.CodexMinCLIVersion,
		CodexUserAgentConfig:                runtimeCfg.CodexUserAgentConfig,
		CodexTelemetryEnabled:               runtimeCfg.CodexTelemetryEnabled,
		CodexTelemetryTimingDebug:           runtimeCfg.CodexTelemetryTimingDebug,
		UsageLogMode:                        usageLogMode,
		UsageLogBatchSize:                   usageLogBatchSize,
		UsageLogFlushIntervalSeconds:        usageLogFlushIntervalSeconds,
		StreamFlushPolicy:                   runtimeCfg.StreamFlushPolicy,
		StreamFlushIntervalMS:               runtimeCfg.StreamFlushIntervalMS,
		FirstTokenMode:                      runtimeCfg.FirstTokenMode,
		FirstTokenTimeoutSeconds:            runtimeCfg.FirstTokenTimeoutSec,
		BillingTierPolicy:                   runtimeCfg.BillingTierPolicy,
		ModelsListReadMaxBytes:              runtimeCfg.ModelsListReadMaxBytes,
		ShowFullUsageNumbers:                showFullUsageNumbers,
		ShowUpstreamModelMismatch:           showUpstreamModelMismatch,
		PublicKeyUsagePageEnabled:           publicKeyUsagePageEnabled,
		PublicImageStudioPageEnabled:        publicImageStudioPageEnabled,
		PublicAccountPortalPageEnabled:      publicAccountPortalPageEnabled,
		ImageStorageBackend:                 imgCfg.Backend,
		ImageS3Endpoint:                     imgCfg.Endpoint,
		ImageS3Region:                       imgCfg.Region,
		ImageS3Bucket:                       imgCfg.Bucket,
		ImageS3AccessKey:                    imgCfg.AccessKey,
		ImageS3SecretKey:                    imgCfg.SecretKey,
		ImageS3Prefix:                       strings.TrimSuffix(imgCfg.Prefix, "/"),
		ImageS3ForcePathStyle:               imgCfg.ForcePathStyle,
		AutoPause5hThreshold:                h.store.GetGlobalAutoPause5hThreshold(),
		AutoPause7dThreshold:                h.store.GetGlobalAutoPause7dThreshold(),
		AutoPause5hGuardBandPercent:         h.store.GetAutoPause5hGuardBandPercent(),
		AutoPause5hGuardConcurrency:         h.store.GetAutoPause5hGuardConcurrency(),
		SmartPacingEnabled:                  h.store.GetSmartPacingEnabled(),
		SmartPacingMinConcurrency:           h.store.GetSmartPacingMinConcurrency(),
		SmartPacingWindows:                  h.store.GetSmartPacingWindows(),
		IgnoreUsageLimitStatus:              h.store.IgnoreUsageLimitStatus(),
		OutputGuardEnabled:                  outputGuardSettings.Enabled == nil || *outputGuardSettings.Enabled,
		OutputGuardMaxToolArgumentBytes:     outputGuardSettings.MaxToolArgumentBytes,
		OutputGuardMaxToolArgumentDeltas:    outputGuardSettings.MaxToolArgumentDeltas,
		OutputGuardMaxChannelBytes:          outputGuardSettings.MaxChannelBytes,
		OutputGuardMaxRequestBytes:          outputGuardSettings.MaxRequestBytes,
		OutputGuardConfigGeneration:         outputGuardSettings.ConfigGeneration,
	})
}

type testImageStorageReq struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	AccessKey      string `json:"access_key"`
	SecretKey      string `json:"secret_key"`
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
}

// TestImageStorageConnection temporarily builds an S3Backend from the submitted fields and calls HeadBucket to verify reachability.
// It does not change any persisted state, so the user can click "test connection" before saving.
func (h *Handler) TestImageStorageConnection(c *gin.Context) {
	var req testImageStorageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	cfg := imagestore.Config{
		Backend:        imagestore.BackendS3,
		Endpoint:       req.Endpoint,
		Region:         req.Region,
		Bucket:         req.Bucket,
		AccessKey:      req.AccessKey,
		SecretKey:      req.SecretKey,
		Prefix:         req.Prefix,
		ForcePathStyle: req.ForcePathStyle,
	}.Normalize()
	if err := cfg.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	backend, err := imagestore.NewS3Backend(cfg)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := backend.HeadBucket(ctx); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "bucket": cfg.Bucket})
}

// ==================== export & migration ====================

type cpaExportEntry struct {
	Type                  string `json:"type"`
	Email                 string `json:"email"`
	PlanType              string `json:"plan_type,omitempty"`
	Codex7DUsedPercent    string `json:"codex_7d_used_percent,omitempty"`
	Codex7DResetAt        string `json:"codex_7d_reset_at,omitempty"`
	Codex5HUsedPercent    string `json:"codex_5h_used_percent,omitempty"`
	Codex5HResetAt        string `json:"codex_5h_reset_at,omitempty"`
	Codex5HUsageUpdatedAt string `json:"codex_5h_usage_updated_at,omitempty"`
	CodexUsageUpdatedAt   string `json:"codex_usage_updated_at,omitempty"`
	Expired               string `json:"expired"`
	IDToken               string `json:"id_token"`
	AccountID             string `json:"account_id"`
	AccessToken           string `json:"access_token"`
	LastRefresh           string `json:"last_refresh"`
	RefreshToken          string `json:"refresh_token"`
	// The three proxy fields are written only when include_proxy=1: a proxy URL often carries a plaintext username and password.
	// ProxyEnabled uses a pointer to tell apart "the file does not have this field" (an old file, treated as enabled) and
	// "the source explicitly disabled it"; a bool's zero value would be swallowed together with omitempty.
	ProxyURL     string `json:"proxy_url,omitempty"`
	ProxyLabel   string `json:"proxy_label,omitempty"`
	ProxyEnabled *bool  `json:"proxy_enabled,omitempty"`
}

// exportProxyResolver decides whether an exported entry carries the account-bound proxy. The zero value means it does not carry it—
// exporting a proxy is the same as exporting its credentials, so the caller must enable it explicitly.
type exportProxyResolver struct {
	include bool
	byURL   map[string]*database.ProxyRow
}

// newExportProxyResolver reads the proxy table once when include is true, to fill in the
// label / enabled. A custom proxy bound to an account (not in the proxy table) carries only the URL.
func (h *Handler) newExportProxyResolver(ctx context.Context, include bool) exportProxyResolver {
	if !include {
		return exportProxyResolver{}
	}
	resolver := exportProxyResolver{include: true, byURL: make(map[string]*database.ProxyRow)}
	proxies, err := h.db.ListProxies(ctx)
	if err != nil {
		// A failed read of the proxy table should not fail the whole export: the URL is on the account row, and label/enabled are only annotations.
		log.Printf("export accounts: failed to read the proxy table, only proxy URLs are exported this time: %v", err)
		return resolver
	}
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		resolver.byURL[strings.TrimSpace(proxy.URL)] = proxy
	}
	return resolver
}

// resolve returns the proxy URL / label / enabled state to write into the export entry.
func (r exportProxyResolver) resolve(rawURL string) (string, string, *bool) {
	if !r.include {
		return "", "", nil
	}
	proxyURL := strings.TrimSpace(rawURL)
	if proxyURL == "" {
		return "", "", nil
	}
	row := r.byURL[proxyURL]
	if row == nil {
		return proxyURL, "", nil
	}
	enabled := row.Enabled
	return proxyURL, row.Label, &enabled
}

// exportIncludeProxy parses the include_proxy query parameter; when it is not passed explicitly, the channel default is used.
// It uses Query().Has to check presence instead of comparing with an empty string: include_proxy=0 must be able to override
// a channel that enables it by default.
func exportIncludeProxy(c *gin.Context, defaultValue bool) bool {
	if !c.Request.URL.Query().Has("include_proxy") {
		return defaultValue
	}
	return parseBoolForm(c.Query("include_proxy"))
}

type accountAuthJSONTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type accountAuthJSON struct {
	AuthMode     string                `json:"auth_mode"`
	OpenAIAPIKey *string               `json:"OPENAI_API_KEY"`
	Tokens       accountAuthJSONTokens `json:"tokens"`
	LastRefresh  string                `json:"last_refresh"`
}

// GetAccountAuthJSON generates an auth.json for a single account that can be used with the Codex CLI.
func (h *Handler) GetAccountAuthJSON(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to query account: "+err.Error())
		return
	}

	refreshToken := row.GetCredential("refresh_token")
	accessToken := row.GetCredential("access_token")
	idToken := row.GetCredential("id_token")
	accountID := row.GetCredential("account_id")
	if refreshToken == "" {
		writeError(c, http.StatusBadRequest, "This account has no refresh_token, so auth.json cannot be generated")
		return
	}
	if accessToken == "" || idToken == "" {
		writeError(c, http.StatusBadRequest, "Account is missing access_token or id_token; refresh the account before generating auth.json")
		return
	}
	if accountID == "" {
		if info := auth.ParseIDToken(idToken); info != nil {
			accountID = info.ChatGPTAccountID
		}
	}
	if accountID == "" {
		if info := auth.ParseAccessToken(accessToken); info != nil {
			accountID = info.ChatGPTAccountID
		}
	}
	if accountID == "" {
		writeError(c, http.StatusBadRequest, "Account is missing account_id; refresh the account before generating auth.json")
		return
	}

	c.Header("Content-Disposition", `attachment; filename="auth.json"`)
	c.JSON(http.StatusOK, accountAuthJSON{
		AuthMode:     "chatgpt",
		OpenAIAPIKey: nil,
		Tokens: accountAuthJSONTokens{
			IDToken:      idToken,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			AccountID:    accountID,
		},
		LastRefresh: row.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
}

// accountRowToCPAExportEntry converts a database account row into a CPA export entry; it returns false when there are no credentials.
// proxies decides whether the account-bound proxy is exported along with it (zero value = do not export).
func accountRowToCPAExportEntry(row *database.AccountRow, proxies exportProxyResolver) (cpaExportEntry, bool) {
	if row == nil {
		return cpaExportEntry{}, false
	}
	rt := row.GetCredential("refresh_token")
	at := row.GetCredential("access_token")
	// AT-only accounts (no refresh_token, relying only on access_token, often used to avoid
	// add-phone Plus accounts) also need to be exportable and migratable. Skip only when both credentials are missing.
	if rt == "" && at == "" {
		return cpaExportEntry{}, false
	}
	// account_id is stored in the credentials as chatgpt_account_id (new field) or account_id (legacy field)
	accountID := row.GetCredential("chatgpt_account_id")
	if accountID == "" {
		accountID = row.GetCredential("account_id")
	}
	proxyURL, proxyLabel, proxyEnabled := proxies.resolve(row.ProxyURL)
	return cpaExportEntry{
		Type:                  "codex",
		Email:                 row.GetCredential("email"),
		PlanType:              row.GetCredential("plan_type"),
		Codex7DUsedPercent:    row.GetCredential("codex_7d_used_percent"),
		Codex7DResetAt:        row.GetCredential("codex_7d_reset_at"),
		Codex5HUsedPercent:    row.GetCredential("codex_5h_used_percent"),
		Codex5HResetAt:        row.GetCredential("codex_5h_reset_at"),
		Codex5HUsageUpdatedAt: row.GetCredential("codex_5h_usage_updated_at"),
		CodexUsageUpdatedAt:   row.GetCredential("codex_usage_updated_at"),
		Expired:               row.GetCredential("expires_at"),
		IDToken:               row.GetCredential("id_token"),
		AccountID:             accountID,
		AccessToken:           at,
		LastRefresh:           row.UpdatedAt.Format(time.RFC3339),
		RefreshToken:          rt,
		ProxyURL:              proxyURL,
		ProxyLabel:            proxyLabel,
		ProxyEnabled:          proxyEnabled,
	}, true
}

func parseExportIDSet(idsParam string) map[int64]bool {
	idsParam = strings.TrimSpace(idsParam)
	if idsParam == "" {
		return nil
	}
	idSet := make(map[int64]bool)
	for _, s := range strings.Split(idsParam, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			idSet[id] = true
		}
	}
	if len(idSet) == 0 {
		return nil
	}
	return idSet
}

// ExportAccounts exports accounts (CPA JSON format)
func (h *Handler) ExportAccounts(c *gin.Context) {
	filter := c.DefaultQuery("filter", "healthy")
	idsParam := c.Query("ids")
	remote := c.Query("remote")
	// channel=codex limits the export to one channel: the Codex accounts page passes codex when exporting. Without it, everything is exported,
	// Remote migration (remote=true) relies on the full-export semantics.
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))

	// Remote calls must check allow_remote_migration
	if remote == "true" {
		if !h.hasConfiguredAdminSecret(c.Request.Context()) {
			writeError(c, http.StatusForbidden, "Set an admin key before enabling remote migration")
			return
		}
		if !h.store.GetAllowRemoteMigration() {
			writeError(c, http.StatusForbidden, "Remote migration is not enabled; turn it on in system settings")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.db.ListActiveByChannel(ctx, channel)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to query accounts: "+err.Error())
		return
	}

	// Remote migration also does not carry proxies by default: the target machine may not be able to reach the source machine's proxy network, and silently inheriting would make
	// the whole batch of accounts bind to an unreachable egress. When needed, the caller passes include_proxy=1 explicitly.
	proxies := h.newExportProxyResolver(ctx, exportIncludeProxy(c, false))
	idSet := parseExportIDSet(idsParam)

	// Build the runtime status map (used for health filtering)
	runtimeMap := make(map[int64]*auth.Account)
	if filter == "healthy" {
		for _, acc := range h.store.Accounts() {
			runtimeMap[acc.DBID] = acc
		}
	}

	entries := make([]any, 0, len(rows))
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			acc, ok := runtimeMap[row.ID]
			if !ok || acc.RuntimeStatus() != "active" {
				continue
			}
		}
		entry, ok := accountRowToExportEntry(row, proxies)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}

	writeSecretResponseHeaders(c)
	c.JSON(http.StatusOK, entries)
}

// ExportRecycleBinAccounts exports recycle bin accounts (CPA JSON format).
// GET /api/admin/accounts/recycle-bin/export?ids=1,2,3
// ids is optional: if omitted, all recycle bin accounts are exported; if given, only the specified IDs are exported (they must be in the recycle bin).
func (h *Handler) ExportRecycleBinAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	rows, err := h.db.ListDeleted(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to query recycle bin: "+err.Error())
		return
	}

	proxies := h.newExportProxyResolver(ctx, exportIncludeProxy(c, false))
	idSet := parseExportIDSet(c.Query("ids"))
	entries := make([]any, 0, len(rows))
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		entry, ok := accountRowToExportEntry(row, proxies)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}
	writeSecretResponseHeaders(c)
	c.JSON(http.StatusOK, entries)
}

type migrateReq struct {
	URL      string `json:"url"`
	AdminKey string `json:"admin_key"`
}

// MigrateAccounts migrates healthy accounts from a remote codex2api instance (SSE streaming progress)
func (h *Handler) MigrateAccounts(c *gin.Context) {
	if !h.hasConfiguredAdminSecret(c.Request.Context()) {
		writeError(c, http.StatusForbidden, "Set an admin key before using remote migration")
		return
	}

	var req migrateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.URL == "" || req.AdminKey == "" {
		writeError(c, http.StatusBadRequest, "url and admin_key are required fields")
		return
	}
	parsedURL, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeError(c, http.StatusBadRequest, "url must be a full http/https address")
		return
	}

	remoteURL := strings.TrimRight(parsedURL.String(), "/")
	exportURL := remoteURL + "/api/admin/accounts/export?filter=healthy&remote=true"

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer fetchCancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, exportURL, nil)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Failed to build request: "+err.Error())
		return
	}
	httpReq.Header.Set("X-Admin-Key", req.AdminKey)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(httpReq)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Failed to connect to remote instance: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeError(c, http.StatusBadGateway, fmt.Sprintf("Remote instance returned an error (%d): %s", resp.StatusCode, string(body)))
		return
	}

	var remoteAccounts []cpaExportEntry
	if err := json.NewDecoder(resp.Body).Decode(&remoteAccounts); err != nil {
		writeError(c, http.StatusBadGateway, "Failed to parse remote data: "+err.Error())
		return
	}

	if len(remoteAccounts) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "The remote instance has no healthy accounts to migrate", "total": 0, "imported": 0, "duplicate": 0, "failed": 0})
		return
	}

	// Convert to the importToken format and reuse importAccountsCommon (which natively supports mixed AT-only imports)
	var tokens []importToken
	for _, entry := range remoteAccounts {
		rt := strings.TrimSpace(entry.RefreshToken)
		at := strings.TrimSpace(entry.AccessToken)
		// At least one kind of credential is required; if both are empty, the account has no usable credentials at all.
		if rt == "" && at == "" {
			continue
		}
		name := entry.Email
		if name == "" {
			name = "migrate"
		}
		tokens = append(tokens, importToken{
			refreshToken:          rt,
			accessToken:           at,
			name:                  name,
			email:                 strings.TrimSpace(entry.Email),
			idToken:               strings.TrimSpace(entry.IDToken),
			accountID:             strings.TrimSpace(entry.AccountID),
			planType:              strings.TrimSpace(entry.PlanType),
			expiresAt:             strings.TrimSpace(entry.Expired),
			codex7DUsedPercent:    strings.TrimSpace(entry.Codex7DUsedPercent),
			codex7DResetAt:        strings.TrimSpace(entry.Codex7DResetAt),
			codex5HUsedPercent:    strings.TrimSpace(entry.Codex5HUsedPercent),
			codex5HResetAt:        strings.TrimSpace(entry.Codex5HResetAt),
			codex5HUsageUpdatedAt: strings.TrimSpace(entry.Codex5HUsageUpdatedAt),
			codexUsageUpdatedAt:   strings.TrimSpace(entry.CodexUsageUpdatedAt),
		})
	}

	log.Printf("remote migration: pulled from %s, %d accounts, starting import", remoteURL, len(tokens))
	h.importAccountsCommon(c, tokens, importSettings{})
}

// ==================== Models ====================

// ListModels returns the supported model list (used by the frontend settings page)
func (h *Handler) ListModels(c *gin.Context) {
	catalog, _ := proxy.ListModelCatalog(c.Request.Context(), h.db)
	h.addDaybreakCatalogModels(&catalog)
	c.JSON(http.StatusOK, catalog)
}

// SyncModels syncs the model registry from the official Codex models page.
func (h *Handler) SyncModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	result, err := proxy.SyncOfficialCodexModels(ctx, h.db, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// SyncCodexCLIVersion fetches the latest stable version from openai/codex releases,
// raises the simulated version of the outbound UA / manifest (never downgrades), called by the "sync now" button on the settings page.
func (h *Handler) SyncCodexCLIVersion(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	result, err := proxy.SyncCodexCLIVersion(ctx, h.db, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// SyncCodexClientVersions syncs the CLI, Desktop, and VSCode, and returns the independent result for each source.
func (h *Handler) SyncCodexClientVersions(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Minute)
	defer cancel()
	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	result, err := proxy.SyncCodexClientVersions(ctx, h.db, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// ==================== Codex User-Agent shape catalog and preview ====================

// GetCodexUserAgentCatalog returns the Codex client shape catalog (shape, platform, terminal, trailing marker name,
// CLI version→build number pairing and default account pool ratios), for the settings page to pick a combination from.
func (h *Handler) GetCodexUserAgentCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, proxy.CodexUserAgentCatalog())
}

type codexUserAgentPreviewRequest struct {
	Config             string `json:"config"`
	ClientCompatMode   string `json:"client_compat_mode"`
	CodexMinCLIVersion string `json:"codex_min_cli_version"`
}

// PreviewCodexUserAgent computes the real outbound identity (User-Agent /
// Originator / Version), using the same rules as the execution path; in account pool mode, it samples the first few Codex accounts one by one.
// The compatibility mode and minimum CLI version can be passed in the request (form values); by default the currently effective settings are used.
func (h *Handler) PreviewCodexUserAgent(c *gin.Context) {
	var req codexUserAgentPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	settings := proxy.CurrentRuntimeSettings()
	compatMode := strings.TrimSpace(req.ClientCompatMode)
	if compatMode == "" {
		compatMode = settings.ClientCompatMode
	}
	minVersion := strings.TrimSpace(req.CodexMinCLIVersion)
	if minVersion == "" {
		minVersion = settings.CodexMinCLIVersion
	}
	versionFloor := ""
	if compatMode == proxy.ClientCompatModeAuto {
		versionFloor = minVersion
	}
	var sampleIDs []int64
	if h.store != nil {
		for _, acc := range h.store.Accounts() {
			if acc == nil {
				continue
			}
			sampleIDs = append(sampleIDs, acc.ID())
			if len(sampleIDs) >= 6 {
				break
			}
		}
	}
	preview, err := proxy.PreviewCodexUserAgentConfig(req.Config, versionFloor, sampleIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, preview)
}

// ==================== account trend ====================

// GetAccountEventTrend returns the aggregated account add/remove trend data
func (h *Handler) GetAccountEventTrend(c *gin.Context) {
	startStr := c.Query("start")
	endStr := c.Query("end")
	if startStr == "" || endStr == "" {
		writeError(c, http.StatusBadRequest, "start and end parameters are required")
		return
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "start time format is invalid (must be RFC3339)")
		return
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "end time format is invalid (must be RFC3339)")
		return
	}

	bucketMinutes := 60
	if bStr := c.Query("bucket_minutes"); bStr != "" {
		if b, err := strconv.Atoi(bStr); err == nil && b > 0 {
			bucketMinutes = b
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	trend, err := h.db.GetAccountEventTrend(ctx, start, end, bucketMinutes)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"trend": trend})
}

// ==================== cleanup ====================

// CleanBanned cleans up banned (unauthorized) accounts
func (h *Handler) CleanBanned(c *gin.Context) {
	h.cleanByStatus(c, "unauthorized")
}

// CleanRateLimited cleans up all rate-limited accounts in one step (including premium 5h, free 7d, usage_exhausted)
func (h *Handler) CleanRateLimited(c *gin.Context) {
	h.cleanAccountTargets(c, h.store.CollectRateLimitedManualTargets(), "manual_clean")
}

// CleanError cleans up error accounts (error status)
func (h *Handler) CleanError(c *gin.Context) {
	h.cleanByStatus(c, "error")
}

// cleanByStatus cleans up accounts by runtime status
func (h *Handler) cleanByStatus(c *gin.Context, targetStatus string) {
	h.cleanAccountTargets(c, h.store.CollectCleanTargets(targetStatus, nil), "auto_clean")
}

func (h *Handler) cleanAccountTargets(c *gin.Context, targets []*auth.Account, eventReason string) {
	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamCleanAccounts(c, targets, eventReason)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	success, _ := h.runCleanAccounts(ctx, targets, eventReason, nil)
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Cleaned up %d accounts", success), "cleaned": success})
}

func (h *Handler) streamCleanAccounts(c *gin.Context, targets []*auth.Account, eventReason string) {
	setupSSE(c)
	total := len(targets)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "clean", Total: total})
	success, fail := h.runCleanAccounts(c.Request.Context(), targets, eventReason, func(event batchOperationEvent) {
		sendSSEJSON(c, event)
	})
	sendSSEJSON(c, batchOperationEvent{
		Type:    "complete",
		Action:  "clean",
		Current: total,
		Total:   total,
		Success: success,
		Failed:  fail,
		Deleted: success,
		Message: fmt.Sprintf("Cleaned up %d accounts", success),
	})
}

func (h *Handler) runCleanAccounts(ctx context.Context, targets []*auth.Account, eventReason string, onProgress func(batchOperationEvent)) (int64, int64) {
	total := len(targets)
	var success int64
	var fail int64
	deleted := make([]int64, 0, len(targets))

	for i, acc := range targets {
		if ctx.Err() != nil {
			fail += int64(total - i)
			break
		}
		if acc == nil {
			fail++
			if onProgress != nil {
				onProgress(batchOperationEvent{
					Type:    "progress",
					Action:  "clean",
					Current: i + 1,
					Total:   total,
					Success: success,
					Failed:  fail,
					Error:   "Account not found",
				})
			}
			continue
		}

		name, email := runtimeAccountOperationIdentity(acc)
		err := h.store.SoftDeleteForClean(ctx, acc, eventReason)
		event := batchOperationEvent{
			Type:         "progress",
			Action:       "clean",
			Current:      i + 1,
			Total:        total,
			AccountID:    acc.DBID,
			AccountName:  name,
			AccountEmail: email,
		}
		if err != nil {
			fail++
			event.Error = err.Error()
		} else {
			success++
			deleted = append(deleted, acc.DBID)
			event.Deleted = success
		}
		event.Success = success
		event.Failed = fail
		if onProgress != nil {
			onProgress(event)
		}
	}

	h.pruneAccountsFromSnapshotCaches(deleted)
	return success, fail
}

// ==================== Proxies ====================

func normalizeManagedProxyURL(raw string) (string, error) {
	normalized := strings.TrimSpace(raw)
	if err := security.ValidateProxyURL(normalized); err != nil {
		return "", err
	}
	if _, err := security.ParseProxyURL(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// ListProxies returns the proxy list
func (h *Handler) ListProxies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	proxies, err := h.db.ListProxies(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to get proxy list")
		return
	}
	if proxies == nil {
		proxies = []*database.ProxyRow{}
	}
	// Bound counts are aggregated on the server; a failure does not block the list (the frontend shows 0 as "unbound").
	if boundCounts, err := h.db.CountAccountsByProxyURL(ctx); err == nil {
		for _, p := range proxies {
			p.BoundCount = boundCounts[strings.TrimSpace(p.URL)]
		}
	}
	c.JSON(http.StatusOK, gin.H{"proxies": proxies})
}

// AddProxies adds proxies (batch supported)
func (h *Handler) AddProxies(c *gin.Context) {
	var req struct {
		URLs  []string `json:"urls"`
		URL   string   `json:"url"`
		Label string   `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	// Merge single and batch input
	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		writeError(c, http.StatusBadRequest, "Provide at least one proxy URL")
		return
	}

	// Filter out empty lines
	cleaned := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.TrimSpace(u) != "" {
			normalizedURL, err := normalizeManagedProxyURL(u)
			if err != nil {
				writeError(c, http.StatusBadRequest, "Invalid proxy URL: "+err.Error())
				return
			}
			cleaned = append(cleaned, normalizedURL)
		}
	}
	if len(cleaned) == 0 {
		writeError(c, http.StatusBadRequest, "Provide at least one valid proxy URL")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	inserted, err := h.db.InsertProxies(ctx, cleaned, req.Label)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to add proxies: "+err.Error())
		return
	}

	if err := h.store.ReloadProxyPool(); err != nil {
		log.Printf("proxies added, but proxy pool refresh failed: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  fmt.Sprintf("Added %d proxies", inserted),
		"inserted": inserted,
		"total":    len(cleaned),
	})
}

// DeleteProxy deletes a single proxy and immediately unbinds the accounts that still reference its URL.
func (h *Handler) DeleteProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	result, err := h.db.RetireProxiesByIDs(ctx, []int64{id})
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to delete proxy")
		return
	}
	if result.Deleted == 0 {
		writeError(c, http.StatusNotFound, "Proxy not found")
		return
	}

	if err := h.applyRetiredProxiesToRuntime(result.DeletedProxyURLs, result.UnboundAccountIDs); err != nil {
		log.Printf("proxy deleted, but proxy pool refresh failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Proxy deleted, but proxy pool refresh failed",
			"deleted": result.Deleted,
			"unbound": result.Unbound,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Proxy deleted",
		"deleted": result.Deleted,
		"unbound": result.Unbound,
	})
}

// UpdateProxy updates a proxy (enable/disable/change label/change URL)
func (h *Handler) UpdateProxy(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid proxy ID")
		return
	}

	var req struct {
		URL     *string `json:"url"`
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.URL != nil {
		normalizedURL, err := normalizeManagedProxyURL(*req.URL)
		if err != nil {
			writeError(c, http.StatusBadRequest, "Invalid proxy URL: "+err.Error())
			return
		}
		req.URL = &normalizedURL
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	existing, err := h.db.GetProxy(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Proxy not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to get proxy info")
		return
	}
	oldURL := strings.TrimSpace(existing.URL)

	if err := h.db.UpdateProxy(ctx, id, req.URL, req.Label, req.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Proxy not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to update proxy")
		return
	}

	newURL := oldURL
	if req.URL != nil {
		newURL = strings.TrimSpace(*req.URL)
	}
	if newURL != "" && newURL != oldURL {
		reboundIDs, rebindErr := h.db.RebindAccountProxyURLs(ctx, oldURL, newURL)
		if rebindErr != nil {
			writeError(c, http.StatusInternalServerError, "Failed to update proxy binding")
			return
		}
		if h.store != nil {
			for _, accountID := range reboundIDs {
				h.store.ApplyAccountProxyURL(accountID, newURL)
			}
		}
		h.removeProxyURLsFromRuntime([]string{oldURL})
	}
	if req.Enabled != nil && !*req.Enabled {
		h.removeProxyURLsFromRuntime([]string{newURL})
	}

	if err := h.reloadProxyPool(); err != nil {
		log.Printf("proxy updated, but proxy pool refresh failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Proxy updated, but proxy pool refresh failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Proxy updated"})
}

// BatchDeleteProxies deletes proxies in batch
func (h *Handler) BatchDeleteProxies(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "Provide the list of proxy IDs to delete")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	result, err := h.db.RetireProxiesByIDs(ctx, req.IDs)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Batch delete failed")
		return
	}

	if err := h.applyRetiredProxiesToRuntime(result.DeletedProxyURLs, result.UnboundAccountIDs); err != nil {
		log.Printf("proxies batch-deleted, but proxy pool refresh failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Proxy deleted, but proxy pool refresh failed",
			"deleted": result.Deleted,
			"unbound": result.Unbound,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Deleted %d proxies", result.Deleted),
		"deleted": result.Deleted,
		"unbound": result.Unbound,
	})
}

// CleanErrorProxies cleans up proxies that failed testing in one step and unbinds the accounts that reference them.
func (h *Handler) CleanErrorProxies(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	result, err := h.db.CleanErrorProxies(ctx)
	if err != nil {
		log.Printf("failed to clean up error proxies: %v", err)
		writeError(c, http.StatusInternalServerError, "Failed to clean up error proxies")
		return
	}

	if h.store != nil {
		if err := h.applyRetiredProxiesToRuntime(result.DeletedProxyURLs, result.UnboundAccountIDs); err != nil {
			log.Printf("error proxies cleaned up, but proxy pool refresh failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "Error proxies cleaned up, but proxy pool refresh failed",
				"cleaned": result.Deleted,
				"unbound": result.Unbound,
			})
			return
		}
	} else if err := h.reloadProxyPool(); err != nil {
		log.Printf("error proxies cleaned up, but proxy pool refresh failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Error proxies cleaned up, but proxy pool refresh failed",
			"cleaned": result.Deleted,
			"unbound": result.Unbound,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Cleaned up %d error proxies and unbound %d accounts", result.Deleted, result.Unbound),
		"cleaned": result.Deleted,
		"unbound": result.Unbound,
	})
}

func (h *Handler) persistProxyTestResult(ctx context.Context, id int64, expectedURL, status, ip, location string, latencyMs int) error {
	if id <= 0 {
		return nil
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.db.UpdateProxyTestResult(saveCtx, id, expectedURL, status, ip, location, latencyMs); err != nil {
		return err
	}
	if status == database.ProxyTestStatusError {
		h.removeProxyURLsFromRuntime([]string{expectedURL})
	}
	if err := h.reloadProxyPool(); err != nil {
		return fmt.Errorf("proxy test status saved, but proxy pool refresh failed: %w", err)
	}
	return nil
}

func respondProxyTestSaveError(c *gin.Context, err error, probeMessage string) {
	if errors.Is(err, database.ErrProxyTestTargetChanged) {
		c.JSON(http.StatusConflict, gin.H{"error": "Proxy was modified during the test, please test again"})
		return
	}
	if strings.TrimSpace(probeMessage) == "" {
		probeMessage = "Proxy test completed"
	}
	log.Printf("failed to sync proxy test result: probe_error=%q err=%v", probeMessage, err)
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": fmt.Sprintf("%s; failed to save test result or refresh proxy pool: %v", probeMessage, err),
	})
}

// TestProxy tests proxy connectivity and the egress IP location
func (h *Handler) TestProxy(c *gin.Context) {
	var req struct {
		URL  string `json:"url"`
		ID   int64  `json:"id"`
		Lang string `json:"lang"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Provide a proxy URL")
		return
	}
	proxyURL := strings.TrimSpace(req.URL)
	if proxyURL == "" {
		writeError(c, http.StatusBadRequest, "Provide a proxy URL")
		return
	}
	expectedURL := proxyURL
	if req.ID > 0 {
		row, err := h.db.GetProxy(c.Request.Context(), req.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusNotFound, "Proxy not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "Failed to get proxy info")
			return
		}
		storedURL := row.URL
		if strings.TrimSpace(storedURL) != proxyURL {
			c.JSON(http.StatusConflict, gin.H{"error": "Proxy was modified; refresh and test again"})
			return
		}
		expectedURL = storedURL
		proxyURL = strings.TrimSpace(storedURL)
	}

	result := h.runProxyProbe(c.Request.Context(), proxyURL, req.Lang)
	if result.Conclusive {
		status := database.ProxyTestStatusError
		if result.Success {
			status = database.ProxyTestStatusSuccess
		}
		if err := h.persistProxyTestResult(
			c.Request.Context(),
			req.ID,
			expectedURL,
			status,
			result.IP,
			result.Location,
			result.LatencyMs,
		); err != nil {
			respondProxyTestSaveError(c, err, result.Error)
			return
		}
	}
	c.JSON(http.StatusOK, result)
}
