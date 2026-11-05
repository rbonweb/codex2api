// Package proxy: usage budgets per API Key × account group/account scope (issue #439).
//
// Unlike the key-level limits in apikey_limits.go, these limits only count "usage this key sends to this scope",
// so the result is not a simple allow/deny but **scheduling candidate filtering**:
//
//   - on_exhausted=skip (default): an exhausted scope is removed from this request's candidate pool, and the request falls through to the key's
//     other allowed groups/accounts; when no candidates remain it returns 429 and states the scope budget is exhausted (not 503).
//   - on_exhausted=reject:     once this scope is exhausted, requests from this key always get 429 and do not switch accounts.
//
// Data source: window aggregates of usage_logs split by account (one query per window every 60s, regardless of how many scopes are configured)
// + in-process incremental event corrections. The latter suppresses the overspend caused by the 60s cache lag; without it under high concurrency,
// requests keep flowing for a full cache cycle after the budget is exhausted.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

const (
	apiKeyScopeUsageCacheNamespace = "api-key-scope-usage"
	apiKeyScopeUsageCacheTTL       = 60 * time.Second
	// Local increments only need to cover the cache lag window; a 5-minute margin is enough, no need to retain them for the limit window (up to 30d).
	apiKeyScopeDeltaRetention = 5 * time.Minute
	// Cap on incremental events kept per key. Beyond it the oldest half is dropped: at extreme rates the incremental correction will run low,
	// but the fallback DB aggregate stays accurate and no violation is missed.
	apiKeyScopeDeltaMaxEvents = 4096
)

// apiKeyScopeUsageSnapshot is the cached per-account window usage snapshot. SnapshotAt is used to combine with local increments
// by adding only events that occur after the snapshot time, to avoid double counting.
type apiKeyScopeUsageSnapshot struct {
	SnapshotAt time.Time                            `json:"snapshot_at"`
	Accounts   map[int64]database.APIKeyWindowUsage `json:"accounts"`
}

// ==================== local incremental events ====================

type apiKeyScopeUsageEvent struct {
	at        time.Time
	accountID int64
	tokens    int64
	cost      float64
}

// apiKeyScopeUsageTracker records usage events split by (API Key, account) from the last few minutes.
// Only tracks keys that actually have scope limits configured (marked by buildScopeBudgetGate); other keys have zero overhead.
type apiKeyScopeUsageTracker struct {
	mu          sync.Mutex
	events      map[int64][]apiKeyScopeUsageEvent
	tracked     map[int64]time.Time
	shared      map[int64]*sharedScopeBucketCache
	counters    map[int64]*scopeCounterCache
	sharedLoads singleflight.Group
}

// scopeCounterCache caches the result of one cumulative counter read (see apiKeyScopeCounterCacheTTL).
type scopeCounterCache struct {
	counters  map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter
	expiresAt time.Time
}

// sharedScopeBucketCache caches the result of one cross-instance minute-bucket read (see apiKeyScopeSharedReadTTL).
type sharedScopeBucketCache struct {
	buckets   map[int64]map[int64]database.APIKeyWindowUsage
	readAt    time.Time
	expiresAt time.Time
}

func newAPIKeyScopeUsageTracker() *apiKeyScopeUsageTracker {
	return &apiKeyScopeUsageTracker{
		events:   make(map[int64][]apiKeyScopeUsageEvent),
		tracked:  make(map[int64]time.Time),
		shared:   make(map[int64]*sharedScopeBucketCache),
		counters: make(map[int64]*scopeCounterCache),
	}
}

// isTracked reports whether a key is registered as having scope limits configured. Unregistered keys record no increments,
// and do not write to cross-instance buckets.
func (t *apiKeyScopeUsageTracker) isTracked(apiKeyID int64) bool {
	if t == nil || apiKeyID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.tracked[apiKeyID]
	return ok
}

func (t *apiKeyScopeUsageTracker) cachedSharedBuckets(apiKeyID int64) (map[int64]map[int64]database.APIKeyWindowUsage, time.Time, bool) {
	if t == nil {
		return nil, time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.shared[apiKeyID]
	if entry == nil || time.Now().After(entry.expiresAt) {
		return nil, time.Time{}, false
	}
	return entry.buckets, entry.readAt, true
}

func (t *apiKeyScopeUsageTracker) storeSharedBuckets(apiKeyID int64, buckets map[int64]map[int64]database.APIKeyWindowUsage, readAt time.Time) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shared[apiKeyID] = &sharedScopeBucketCache{
		buckets:   buckets,
		readAt:    readAt,
		expiresAt: readAt.Add(apiKeyScopeSharedReadTTL),
	}
}

func (t *apiKeyScopeUsageTracker) cachedCounters(apiKeyID int64) (map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.counters[apiKeyID]
	if entry == nil || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.counters, true
}

func (t *apiKeyScopeUsageTracker) storeCounters(apiKeyID int64, counters map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counters[apiKeyID] = &scopeCounterCache{
		counters:  counters,
		expiresAt: time.Now().Add(apiKeyScopeCounterCacheTTL),
	}
}

// markTracked declares that a key needs increment tracking. Called each time a request builds its gate, and also cleans up keys that have been inactive for a long time.
func (t *apiKeyScopeUsageTracker) markTracked(apiKeyID int64) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tracked[apiKeyID] = now
	t.sweepLocked(now)
}

// record appends one usage event. Keys that are not tracked are ignored.
func (t *apiKeyScopeUsageTracker) record(apiKeyID, accountID int64, tokens int64, cost float64) {
	if t == nil || apiKeyID <= 0 || accountID <= 0 {
		return
	}
	if tokens <= 0 && cost <= 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.tracked[apiKeyID]; !ok {
		return
	}
	events := append(t.pruneLocked(t.events[apiKeyID], now), apiKeyScopeUsageEvent{
		at:        now,
		accountID: accountID,
		tokens:    tokens,
		cost:      cost,
	})
	if len(events) > apiKeyScopeDeltaMaxEvents {
		events = append([]apiKeyScopeUsageEvent(nil), events[len(events)/2:]...)
	}
	t.events[apiKeyID] = events
}

// deltaSince returns the incremental usage (per account) of a key after since. Returns nil when since is the zero value.
func (t *apiKeyScopeUsageTracker) deltaSince(apiKeyID int64, since time.Time) map[int64]database.APIKeyWindowUsage {
	if since.IsZero() {
		return nil
	}
	return t.deltaSelect(apiKeyID, func(at time.Time) bool { return at.After(since) })
}

// deltaWithSharedBuckets, when cross-instance shared increments are enabled, picks local events to avoid double counting with the shared buckets:
//   - events in the snapshot's minute that come after the snapshot time: the shared bucket skipped that minute, so local must fill it in;
//   - events after the shared bucket read time: not yet in (or not read from) the shared bucket, also to be filled in.
//
// All other events are already covered by the shared buckets.
func (t *apiKeyScopeUsageTracker) deltaWithSharedBuckets(apiKeyID int64, snapshotAt, sharedReadAt time.Time) map[int64]database.APIKeyWindowUsage {
	if snapshotAt.IsZero() {
		return nil
	}
	snapshotMinute := snapshotAt.Unix() / 60
	return t.deltaSelect(apiKeyID, func(at time.Time) bool {
		if at.After(snapshotAt) && at.Unix()/60 <= snapshotMinute {
			return true
		}
		return !sharedReadAt.IsZero() && at.After(sharedReadAt)
	})
}

func (t *apiKeyScopeUsageTracker) deltaSelect(apiKeyID int64, keep func(time.Time) bool) map[int64]database.APIKeyWindowUsage {
	if t == nil || apiKeyID <= 0 || keep == nil {
		return nil
	}
	now := time.Now()
	t.mu.Lock()
	events := t.pruneLocked(t.events[apiKeyID], now)
	t.events[apiKeyID] = events
	snapshot := make([]apiKeyScopeUsageEvent, 0, len(events))
	for _, event := range events {
		if keep(event.at) {
			snapshot = append(snapshot, event)
		}
	}
	t.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}
	out := make(map[int64]database.APIKeyWindowUsage, len(snapshot))
	for _, event := range snapshot {
		usage := out[event.accountID]
		usage.Requests++
		usage.Tokens += event.tokens
		usage.UserBilled += event.cost
		out[event.accountID] = usage
	}
	return out
}

func (t *apiKeyScopeUsageTracker) pruneLocked(events []apiKeyScopeUsageEvent, now time.Time) []apiKeyScopeUsageEvent {
	cutoff := now.Add(-apiKeyScopeDeltaRetention)
	idx := 0
	for idx < len(events) && !events[idx].at.After(cutoff) {
		idx++
	}
	if idx == 0 {
		return events
	}
	if idx >= len(events) {
		return nil
	}
	return append([]apiKeyScopeUsageEvent(nil), events[idx:]...)
}

func (t *apiKeyScopeUsageTracker) sweepLocked(now time.Time) {
	cutoff := now.Add(-2 * apiKeyScopeDeltaRetention)
	for apiKeyID, seen := range t.tracked {
		if seen.After(cutoff) {
			continue
		}
		delete(t.tracked, apiKeyID)
		delete(t.events, apiKeyID)
		delete(t.shared, apiKeyID)
		delete(t.counters, apiKeyID)
	}
}

func (h *Handler) apiKeyScopeUsageTracker() *apiKeyScopeUsageTracker {
	if h == nil {
		return nil
	}
	h.scopeUsageMu.Lock()
	defer h.scopeUsageMu.Unlock()
	if h.scopeUsage == nil {
		h.scopeUsage = newAPIKeyScopeUsageTracker()
	}
	return h.scopeUsage
}

// recordAPIKeyScopeUsage records this consumption into local increments before usage is persisted. The billing basis matches
// InsertUsageLog exactly (sharing database.UsageLogUserBilledCost).
// When the runtime cache is shared across instances (Redis), also write a minute bucket so other instances can see this consumption.
func (h *Handler) recordAPIKeyScopeUsage(input *database.UsageLogInput) {
	if h == nil || input == nil || input.APIKeyID <= 0 || input.AccountID <= 0 {
		return
	}
	if input.StatusCode == 499 {
		return
	}
	tokens := int64(input.TotalTokens)
	cost := database.UsageLogUserBilledCost(input)
	tracker := h.apiKeyScopeUsageTracker()
	if !tracker.isTracked(input.APIKeyID) {
		return
	}
	tracker.record(input.APIKeyID, input.AccountID, tokens, cost)
	h.publishSharedScopeDelta(input.APIKeyID, input.AccountID, tokens, cost)
}

// ==================== cross-instance shared increments ====================

const (
	apiKeyScopeSharedDeltaNamespace = "api-key-scope-delta"
	// Each minute bucket carries its own TTL and is cleaned up on natural expiry, so no extra field reclamation logic is needed.
	apiKeyScopeSharedDeltaTTL = 5 * time.Minute
	// Read the current minute plus the previous two minute buckets: that covers the lag of the 60s aggregate cache.
	apiKeyScopeSharedDeltaLookback = 3
	// Local read cache for shared buckets: cuts 3 Redis round trips per request down to 3 every 5 seconds.
	apiKeyScopeSharedReadTTL = 5 * time.Second
	// Local read cache for cumulative quota counters.
	apiKeyScopeCounterCacheTTL = 5 * time.Second
	apiKeyScopeDeltaWriteSlots = 16
)

// sharedScopeDeltaEnabled decides whether cross-instance shared increments are worth using. With a single instance (memory cache),
// local increments are already fully accurate, so there is no need to pay the Redis round trip.
func (h *Handler) sharedScopeDeltaEnabled() bool {
	return h != nil && h.cache != nil && h.cache.SharedAcrossInstances()
}

func apiKeyScopeSharedDeltaKey(apiKeyID int64, minute int64) string {
	return fmt.Sprintf("%d:%d", apiKeyID, minute)
}

// publishSharedScopeDelta adds this consumption to the current minute bucket. The write is async: billing should not make the request wait
// for a Redis round trip; if one write is lost, the count is only briefly low and the DB aggregate still catches it.
func (h *Handler) publishSharedScopeDelta(apiKeyID, accountID, tokens int64, cost float64) {
	if !h.sharedScopeDeltaEnabled() {
		return
	}
	if tokens <= 0 && cost <= 0 {
		return
	}
	deltas := map[string]float64{
		fmt.Sprintf("%d:r", accountID): 1,
		fmt.Sprintf("%d:t", accountID): float64(tokens),
		fmt.Sprintf("%d:c", accountID): cost,
	}
	key := apiKeyScopeSharedDeltaKey(apiKeyID, time.Now().Unix()/60)
	write := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.cache.IncrRuntimeCounters(ctx, apiKeyScopeSharedDeltaNamespace, key, deltas, apiKeyScopeSharedDeltaTTL)
	}
	h.scopeDeltaInit.Do(func() { h.scopeDeltaSlots = make(chan struct{}, apiKeyScopeDeltaWriteSlots) })
	select {
	case h.scopeDeltaSlots <- struct{}{}:
		go func() {
			defer func() { <-h.scopeDeltaSlots }()
			write()
		}()
	default:
		// Backpressure bounds background goroutines. A saturated writer must
		// not silently discard another instance's pending usage correction.
		write()
	}
}

// sharedScopeBuckets reads the most recent minute buckets (contributions from all instances), with a 5s local cache.
// It caches the **raw minute buckets**, not the filtered result: the same key's different windows (5h/1d/...) have different snapshot times,
// and their filter conditions differ too, so a cached filtered result would mix them up.
func (h *Handler) sharedScopeBuckets(ctx context.Context, apiKeyID int64) (map[int64]map[int64]database.APIKeyWindowUsage, time.Time) {
	if !h.sharedScopeDeltaEnabled() {
		return nil, time.Time{}
	}
	tracker := h.apiKeyScopeUsageTracker()
	if buckets, readAt, ok := tracker.cachedSharedBuckets(apiKeyID); ok {
		return buckets, readAt
	}
	result := tracker.sharedLoads.DoChan(strconv.FormatInt(apiKeyID, 10), func() (any, error) {
		if buckets, readAt, ok := tracker.cachedSharedBuckets(apiKeyID); ok {
			return &sharedScopeBucketCache{buckets: buckets, readAt: readAt}, nil
		}
		// A canceled leader must not cancel the shared read for other callers.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		buckets, readAt := h.loadSharedScopeBuckets(readCtx, apiKeyID)
		tracker.storeSharedBuckets(apiKeyID, buckets, readAt)
		return &sharedScopeBucketCache{buckets: buckets, readAt: readAt}, nil
	})
	select {
	case <-ctx.Done():
		return nil, time.Time{}
	case result := <-result:
		entry := result.Val.(*sharedScopeBucketCache)
		return entry.buckets, entry.readAt
	}
}

func (h *Handler) loadSharedScopeBuckets(ctx context.Context, apiKeyID int64) (map[int64]map[int64]database.APIKeyWindowUsage, time.Time) {
	nowMinute := time.Now().Unix() / 60
	keys := make([]string, apiKeyScopeSharedDeltaLookback)
	for i := range keys {
		keys[i] = apiKeyScopeSharedDeltaKey(apiKeyID, nowMinute-int64(i))
	}
	var values map[string]map[string]float64
	if reader, ok := h.cache.(cache.RuntimeCounterBatchReader); ok {
		values, _ = reader.GetRuntimeCountersBatch(ctx, apiKeyScopeSharedDeltaNamespace, keys)
	} else {
		values = make(map[string]map[string]float64, len(keys))
		for _, key := range keys {
			if counters, err := h.cache.GetRuntimeCounters(ctx, apiKeyScopeSharedDeltaNamespace, key); err == nil {
				values[key] = counters
			}
		}
	}
	buckets := make(map[int64]map[int64]database.APIKeyWindowUsage, apiKeyScopeSharedDeltaLookback)
	for i := 0; i < apiKeyScopeSharedDeltaLookback; i++ {
		minute := nowMinute - int64(i)
		counters := values[keys[i]]
		if len(counters) == 0 {
			continue
		}
		perAccount := make(map[int64]database.APIKeyWindowUsage)
		for field, value := range counters {
			accountID, metric, ok := parseSharedScopeDeltaField(field)
			if !ok {
				continue
			}
			usage := perAccount[accountID]
			switch metric {
			case "r":
				usage.Requests += int64(value)
			case "t":
				usage.Tokens += int64(value)
			case "c":
				usage.UserBilled += value
			}
			perAccount[accountID] = usage
		}
		buckets[minute] = perAccount
	}
	readAt := time.Now()
	return buckets, readAt
}

// sharedScopeDeltaAfterSnapshot sums the shared buckets "strictly later than the snapshot minute".
// It deliberately skips the bucket that spans the snapshot time: the bucket holds usage already included in the snapshot as well as usage after it, so adding the whole bucket would
// double count. The cost is that at most, consumption by other instances in the snapshot minute is under-counted (this instance's part is filled in by local events).
func sharedScopeDeltaAfterSnapshot(buckets map[int64]map[int64]database.APIKeyWindowUsage, snapshotAt time.Time) map[int64]database.APIKeyWindowUsage {
	if len(buckets) == 0 {
		return nil
	}
	snapshotMinute := snapshotAt.Unix() / 60
	out := make(map[int64]database.APIKeyWindowUsage)
	for minute, perAccount := range buckets {
		if minute <= snapshotMinute {
			continue
		}
		for accountID, usage := range perAccount {
			merged := out[accountID]
			merged.Requests += usage.Requests
			merged.Tokens += usage.Tokens
			merged.UserBilled += usage.UserBilled
			out[accountID] = merged
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseSharedScopeDeltaField(field string) (int64, string, bool) {
	idx := strings.LastIndex(field, ":")
	if idx <= 0 || idx == len(field)-1 {
		return 0, "", false
	}
	accountID, err := strconv.ParseInt(field[:idx], 10, 64)
	if err != nil || accountID <= 0 {
		return 0, "", false
	}
	return accountID, field[idx+1:], true
}

// ==================== hit statistics and logs ====================

// scopeSkipKey identifies the hit statistics of one scope budget.
type scopeSkipKey struct {
	apiKeyID  int64
	scopeType string
	scopeID   int64
}

// APIKeyScopeSkipStat is the runtime statistic for a scope budget judged "exhausted".
// It lives only in the process (reset on restart) and answers "is this budget really in effect, and how many requests did it affect" -
// after a budget is exhausted, requests silently fall through to other accounts; without this statistic you can only notice through 429s.
type APIKeyScopeSkipStat struct {
	Requests    int64     `json:"requests"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	LastMessage string    `json:"last_message"`
	Reject      bool      `json:"reject"`
}

type scopeSkipStats struct {
	mu      sync.Mutex
	entries map[scopeSkipKey]*APIKeyScopeSkipStat
	logged  map[scopeSkipKey]time.Time
}

// apiKeyScopeSkipStats is a process-level singleton: /v1 traffic and the admin side may not read the same Handler instance,
// the statistics must be shared to be visible in the admin UI.
var apiKeyScopeSkipStats = &scopeSkipStats{
	entries: make(map[scopeSkipKey]*APIKeyScopeSkipStat),
	logged:  make(map[scopeSkipKey]time.Time),
}

// scopeSkipLogInterval is the minimum interval between log lines for the same scope budget: while the budget is exhausted every request hits it,
// so without throttling the log would flood.
const scopeSkipLogInterval = time.Minute

// record registers one hit (at most once per request, called by evaluateAPIKeyScopeBudgets),
// and logs one line per interval. Returns whether a log line is needed.
func (s *scopeSkipStats) record(key scopeSkipKey, message string, reject bool, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil {
		entry = &APIKeyScopeSkipStat{FirstAt: now}
		s.entries[key] = entry
	}
	entry.Requests++
	entry.LastAt = now
	entry.LastMessage = message
	entry.Reject = reject

	last, ok := s.logged[key]
	if ok && now.Sub(last) < scopeSkipLogInterval {
		return false
	}
	s.logged[key] = now
	return true
}

// snapshot returns a copy of all hit statistics for a key.
func (s *scopeSkipStats) snapshot(apiKeyID int64) map[string]APIKeyScopeSkipStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]APIKeyScopeSkipStat)
	for key, entry := range s.entries {
		if key.apiKeyID != apiKeyID || entry == nil {
			continue
		}
		out[fmt.Sprintf("%s:%d", key.scopeType, key.scopeID)] = *entry
	}
	return out
}

func (s *scopeSkipStats) reset(apiKeyID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if key.apiKeyID == apiKeyID {
			delete(s.entries, key)
			delete(s.logged, key)
		}
	}
}

// APIKeyScopeSkipSnapshot returns the hit statistics of each scope budget of an API Key (keyed by "type:id").
// For the admin UI to show "this budget is in effect and affected N requests".
func APIKeyScopeSkipSnapshot(apiKeyID int64) map[string]APIKeyScopeSkipStat {
	if apiKeyID <= 0 {
		return map[string]APIKeyScopeSkipStat{}
	}
	return apiKeyScopeSkipStats.snapshot(apiKeyID)
}

// ResetAPIKeyScopeSkipStats clears the hit statistics of an API Key (used when re-observing after a config change).
func ResetAPIKeyScopeSkipStats(apiKeyID int64) {
	if apiKeyID > 0 {
		apiKeyScopeSkipStats.reset(apiKeyID)
	}
}

// ==================== request-level gate ====================

// scopeBudgetGate is the per-request scope budget gate: it records which groups/accounts were removed because their budget was exhausted,
// and the note given downstream when they are removed. A nil gate means no scope was removed for this request.
type scopeBudgetGate struct {
	apiKeyID        int64
	blockedGroups   map[int64]struct{}
	blockedAccounts map[int64]struct{}
	message         string
	blocked         atomic.Int64

	// concurrencyScopes are the scopes with a concurrency limit configured. They have nothing to do with budget exhaustion: even when every budget still has balance,
	// the gate must exist as long as a concurrency limit is configured (checking slots while filtering, taking a slot after selection).
	concurrencyScopes []database.APIKeyScopeLimit
	// concurrencyMessage records the note for the most recent candidate removed because its concurrency slots were full.
	concurrencyMu      sync.Mutex
	concurrencyMessage string
	leases             []scopeConcurrencyLease
}

func (g *scopeBudgetGate) setLeases(leases []scopeConcurrencyLease) {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	g.leases = leases
	g.concurrencyMu.Unlock()
}

func (g *scopeBudgetGate) releaseLeases() {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	leases := g.leases
	g.leases = nil
	g.concurrencyMu.Unlock()
	for _, lease := range leases {
		apiKeyScopeConcurrency.release(lease)
	}
}

func (g *scopeBudgetGate) noteConcurrencyBlock(message string) {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	g.concurrencyMessage = message
	g.concurrencyMu.Unlock()
}

// concurrencyFullFor reports whether an account is unavailable because the concurrency slots of some scope are full.
func (g *scopeBudgetGate) concurrencyFullFor(account *auth.Account) bool {
	if g == nil || account == nil || len(g.concurrencyScopes) == 0 {
		return false
	}
	for _, scope := range g.concurrencyScopes {
		if !scopeMatchesAccount(scope, account) {
			continue
		}
		if scopeConcurrencyFull(g.apiKeyID, scope) {
			g.noteConcurrencyBlock(scopeConcurrencyMessage(scopeLabelForMessage(scope), scope.MaxConcurrency))
			return true
		}
	}
	return false
}

func (g *scopeBudgetGate) blocks(account *auth.Account) bool {
	if g == nil || account == nil {
		return false
	}
	if len(g.blockedAccounts) > 0 {
		if _, ok := g.blockedAccounts[account.ID()]; ok {
			return true
		}
	}
	return account.InAnyGroup(g.blockedGroups)
}

// filter wraps the gate as an account filter, layered on the outermost part of the existing filter chain.
//
// It deliberately runs the inner filter before checking the budget: only accounts that "would otherwise be selectable and were rejected only by the budget" count toward blocked,
// so that a "no usable account" result is turned into a 429 for scope budget exhaustion only when it is safe to do so; otherwise a model mismatch and similar
// rejections would also be blamed on the budget.
func (g *scopeBudgetGate) filter(inner auth.AccountFilter) auth.AccountFilter {
	if g == nil {
		return inner
	}
	return func(account *auth.Account) bool {
		if inner != nil && !inner(account) {
			return false
		}
		if g.blocks(account) {
			g.blocked.Add(1)
			return false
		}
		if g.concurrencyFullFor(account) {
			g.blocked.Add(1)
			return false
		}
		return true
	}
}

// exhaustedMessage returns the scope budget exhaustion note when there are "no usable accounts"; if this request had no candidates
// removed by the budget, it returns "" (meaning the account pool itself is empty, so the original 503 should stand).
func (g *scopeBudgetGate) exhaustedMessage() string {
	if g == nil || g.blocked.Load() == 0 {
		return ""
	}
	if g.message != "" {
		return g.message
	}
	g.concurrencyMu.Lock()
	defer g.concurrencyMu.Unlock()
	return g.concurrencyMessage
}

// applyScopeBudgetFilter layers this request's scope budget gate onto the account filter chain.
// Returns the input unchanged when no scope limit is configured or every scope still has balance.
func (h *Handler) applyScopeBudgetFilter(c *gin.Context, filter auth.AccountFilter) auth.AccountFilter {
	filter = daybreakAccountFilter(c, filter)
	gate := scopeBudgetGateFromContext(c)
	if gate == nil {
		return filter
	}
	return gate.filter(filter)
}

// scopeBudgetExhaustedMessage returns the note that this request removed candidates because a scope budget was exhausted,
// so each handler can turn a 503 into an accurately worded 429 in its "no usable account" branch.
func scopeBudgetExhaustedMessage(c *gin.Context) string {
	return scopeBudgetGateFromContext(c).exhaustedMessage()
}

func scopeBudgetGateFromContext(c *gin.Context) *scopeBudgetGate {
	if c == nil {
		return nil
	}
	v, ok := c.Get(contextScopeBudgetGate)
	if !ok || v == nil {
		return nil
	}
	gate, _ := v.(*scopeBudgetGate)
	return gate
}

// evaluateAPIKeyScopeBudgets computes the scope budget status of this key.
// A non-empty rejectMsg means a scope with on_exhausted=reject is exhausted, and the caller should return 429 directly;
// a non-nil gate means skip-type scopes were removed and it must be attached to the account filter chain.
func (h *Handler) evaluateAPIKeyScopeBudgets(ctx context.Context, row *database.APIKeyRow) (*scopeBudgetGate, string) {
	if h == nil || row == nil || row.ID <= 0 {
		return nil, ""
	}
	scopes := database.NormalizeAPIKeyScopeLimits(row.Limits.ScopeLimits)
	if len(scopes) == 0 {
		return nil, ""
	}
	h.apiKeyScopeUsageTracker().markTracked(row.ID)

	// Fetch window snapshots on demand: only windows actually used by some scope are queried.
	usageByWindow := make(map[string]map[int64]database.APIKeyWindowUsage, len(database.APIKeyScopeWindows))
	for _, window := range database.APIKeyScopeWindows {
		needed := false
		for _, scope := range scopes {
			if scope.NeedsWindow(window.Label) {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		usage, err := h.apiKeyScopeWindowUsage(ctx, row.ID, window)
		if err != nil {
			// If usage cannot be read, do not block the request: consistent with the key-level limits, favoring availability.
			continue
		}
		usageByWindow[window.Label] = usage
	}
	// Scopes with only a cumulative quota / concurrency limit (no sliding window at all) must also be handled and must not exit here.
	concurrencyScopes := scopesWithConcurrencyLimit(scopes)
	if len(usageByWindow) == 0 && !anyCumulativeQuota(scopes) && len(concurrencyScopes) == 0 {
		return nil, ""
	}

	groupsByAccount := h.accountGroupLookup(usageByWindow)
	counters := h.apiKeyScopeCounters(ctx, row.ID, scopes)

	var gate *scopeBudgetGate
	for _, scope := range scopes {
		hit, ok := h.checkScopeBudget(scope, usageByWindow, groupsByAccount)
		if !ok {
			// The cumulative quota (which does not decline over time) is judged separately: it reads api_key_scope_counters,
			// a different data source from the window aggregates.
			hit, ok = scope.CheckCumulative(counters[database.APIKeyScopeCounterKey{
				ScopeType: scope.ResolveScopeType(),
				ScopeID:   scope.ScopeID,
			}])
		}
		if !ok {
			continue
		}
		message := hit.Describe(h.describeScope(scope))
		reject := scope.ResolveOnExhausted() == database.APIKeyScopeOnExhaustedReject
		// Each request registers each exhausted scope only once (this is the path that runs once per request),
		// so the statistic is the number of affected requests, not the number of candidate removals.
		if apiKeyScopeSkipStats.record(scopeSkipKey{
			apiKeyID:  row.ID,
			scopeType: scope.ResolveScopeType(),
			scopeID:   scope.ScopeID,
		}, message, reject, time.Now()) {
			action := "skipping its accounts for this request"
			if reject {
				action = "rejecting the request"
			}
			log.Printf("[scope-budget] api_key=%d %s exhausted, %s (%s)", row.ID, h.describeScope(scope), action, message)
		}
		if reject {
			return nil, message
		}
		if gate == nil {
			gate = newScopeBudgetGate(row.ID, concurrencyScopes)
			gate.message = message
		}
		if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
			gate.blockedAccounts[scope.ScopeID] = struct{}{}
		} else {
			gate.blockedGroups[scope.ScopeID] = struct{}{}
		}
	}
	if gate == nil && len(concurrencyScopes) > 0 {
		gate = newScopeBudgetGate(row.ID, concurrencyScopes)
	}
	return gate, ""
}

func newScopeBudgetGate(apiKeyID int64, concurrencyScopes []database.APIKeyScopeLimit) *scopeBudgetGate {
	return &scopeBudgetGate{
		apiKeyID:          apiKeyID,
		blockedGroups:     make(map[int64]struct{}),
		blockedAccounts:   make(map[int64]struct{}),
		concurrencyScopes: concurrencyScopes,
	}
}

func scopesWithConcurrencyLimit(scopes []database.APIKeyScopeLimit) []database.APIKeyScopeLimit {
	out := make([]database.APIKeyScopeLimit, 0, len(scopes))
	for _, scope := range scopes {
		if scope.MaxConcurrency > 0 {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scopeLabelForMessage is a scope label that does not depend on the store (the concurrency hint is generated on the hot filter path,
// so it is inconvenient to resolve the group name again).
func scopeLabelForMessage(scope database.APIKeyScopeLimit) string {
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return fmt.Sprintf("account #%d", scope.ScopeID)
	}
	return fmt.Sprintf("group #%d", scope.ScopeID)
}

func anyCumulativeQuota(scopes []database.APIKeyScopeLimit) bool {
	for _, scope := range scopes {
		if scope.HasCumulativeQuota() {
			return true
		}
	}
	return false
}

// checkScopeBudget checks whether a scope is exhausted, window by window. It returns on the first hit, with shorter windows first.
func (h *Handler) checkScopeBudget(
	scope database.APIKeyScopeLimit,
	usageByWindow map[string]map[int64]database.APIKeyWindowUsage,
	groupsByAccount map[int64][]int64,
) (database.APIKeyScopeExhaustion, bool) {
	isAccountScope := scope.ResolveScopeType() == database.APIKeyScopeTypeAccount
	for _, window := range database.APIKeyScopeWindows {
		if !scope.NeedsWindow(window.Label) {
			continue
		}
		perAccount, ok := usageByWindow[window.Label]
		if !ok {
			continue
		}
		var usage database.APIKeyWindowUsage
		if isAccountScope {
			usage = perAccount[scope.ScopeID]
		} else {
			for accountID, accountUsage := range perAccount {
				if !containsGroupID(groupsByAccount[accountID], scope.ScopeID) {
					continue
				}
				usage.Requests += accountUsage.Requests
				usage.Tokens += accountUsage.Tokens
				usage.UserBilled += accountUsage.UserBilled
			}
		}
		if hit, exhausted := scope.CheckWindow(window.Label, usage); exhausted {
			return hit, true
		}
	}
	return database.APIKeyScopeExhaustion{}, false
}

// accountGroupLookup resolves the current group of accounts that appear in the snapshot. Accounts already removed from the account pool
// cannot be resolved to a group, so their historical usage is ignored at the group level (it remains accurate at the account level).
func (h *Handler) accountGroupLookup(usageByWindow map[string]map[int64]database.APIKeyWindowUsage) map[int64][]int64 {
	if h == nil || h.store == nil {
		return nil
	}
	out := make(map[int64][]int64)
	for _, perAccount := range usageByWindow {
		for accountID := range perAccount {
			if _, ok := out[accountID]; ok {
				continue
			}
			account := h.store.FindByID(accountID)
			if account == nil {
				out[accountID] = nil
				continue
			}
			out[accountID] = account.GroupIDSnapshot()
		}
	}
	return out
}

// describeScope builds a readable label for the scope, used in error text. Falls back to the ID when the group name cannot be resolved.
func (h *Handler) describeScope(scope database.APIKeyScopeLimit) string {
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return fmt.Sprintf("account #%d", scope.ScopeID)
	}
	if h != nil && h.store != nil {
		if names := h.store.ResolveGroupNames([]int64{scope.ScopeID}); len(names) > 0 && names[0] != "" {
			return fmt.Sprintf("group %q", names[0])
		}
	}
	return fmt.Sprintf("group #%d", scope.ScopeID)
}

// apiKeyScopeCounters reads the cumulative quota counters, with a 5s local cache.
//
// It deliberately does not add in-process increments: the cumulative counters, like api_keys.quota_used, are summed in the same transaction as the batch write,
// so the check naturally lags by one flush cycle (seconds), consistent with the existing key-level cumulative quota semantics.
// The 5s cache removes "one DB query per request" while ensuring the limit takes effect quickly after exhaustion.
func (h *Handler) apiKeyScopeCounters(ctx context.Context, apiKeyID int64, scopes []database.APIKeyScopeLimit) map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter {
	needed := false
	for _, scope := range scopes {
		if scope.HasCumulativeQuota() {
			needed = true
			break
		}
	}
	if !needed || h == nil || h.db == nil {
		return nil
	}
	tracker := h.apiKeyScopeUsageTracker()
	if counters, ok := tracker.cachedCounters(apiKeyID); ok {
		return counters
	}
	counters, err := h.db.ListAPIKeyScopeCounters(ctx, apiKeyID)
	if err != nil {
		// If it cannot be read, treat it as no usage: consistent with the window limits, favoring availability.
		return nil
	}
	tracker.storeCounters(apiKeyID, counters)
	return counters
}

// apiKeyScopeWindowUsage returns the usage within a window split by account: cached snapshot (≤60s) + local increment corrections.
func (h *Handler) apiKeyScopeWindowUsage(ctx context.Context, apiKeyID int64, window database.APIKeyScopeWindow) (map[int64]database.APIKeyWindowUsage, error) {
	snapshot, ok := h.readAPIKeyScopeUsageCache(ctx, apiKeyID, window.Label)
	if !ok {
		accounts, err := h.db.GetAPIKeyAccountWindowUsage(ctx, apiKeyID, window.Window)
		if err != nil {
			return nil, err
		}
		snapshot = &apiKeyScopeUsageSnapshot{SnapshotAt: time.Now(), Accounts: accounts}
		h.writeAPIKeyScopeUsageCache(ctx, apiKeyID, window.Label, snapshot)
	}

	merged := make(map[int64]database.APIKeyWindowUsage, len(snapshot.Accounts)+4)
	for accountID, usage := range snapshot.Accounts {
		merged[accountID] = usage
	}
	addDelta := func(delta map[int64]database.APIKeyWindowUsage) {
		for accountID, usage := range delta {
			current := merged[accountID]
			current.Requests += usage.Requests
			current.Tokens += usage.Tokens
			current.UserBilled += usage.UserBilled
			merged[accountID] = current
		}
	}

	tracker := h.apiKeyScopeUsageTracker()
	if h.sharedScopeDeltaEnabled() {
		buckets, sharedReadAt := h.sharedScopeBuckets(ctx, apiKeyID)
		addDelta(sharedScopeDeltaAfterSnapshot(buckets, snapshot.SnapshotAt))
		addDelta(tracker.deltaWithSharedBuckets(apiKeyID, snapshot.SnapshotAt, sharedReadAt))
	} else {
		addDelta(tracker.deltaSince(apiKeyID, snapshot.SnapshotAt))
	}
	return merged, nil
}

func apiKeyScopeUsageCacheKey(apiKeyID int64, label string) string {
	return fmt.Sprintf("%d:%s", apiKeyID, label)
}

func (h *Handler) readAPIKeyScopeUsageCache(ctx context.Context, apiKeyID int64, label string) (*apiKeyScopeUsageSnapshot, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, apiKeyScopeUsageCacheNamespace, apiKeyScopeUsageCacheKey(apiKeyID, label))
	if err != nil || !ok || len(raw) == 0 {
		return nil, false
	}
	var snapshot apiKeyScopeUsageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, false
	}
	if snapshot.SnapshotAt.IsZero() {
		return nil, false
	}
	if snapshot.Accounts == nil {
		snapshot.Accounts = map[int64]database.APIKeyWindowUsage{}
	}
	return &snapshot, true
}

func (h *Handler) writeAPIKeyScopeUsageCache(ctx context.Context, apiKeyID int64, label string, snapshot *apiKeyScopeUsageSnapshot) {
	if h == nil || h.cache == nil || snapshot == nil {
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	_ = h.cache.SetRuntime(ctx, apiKeyScopeUsageCacheNamespace, apiKeyScopeUsageCacheKey(apiKeyID, label), raw, apiKeyScopeUsageCacheTTL)
}

func containsGroupID(groups []int64, target int64) bool {
	for _, id := range groups {
		if id == target {
			return true
		}
	}
	return false
}
