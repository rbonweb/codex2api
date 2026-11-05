package auth

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var fastSchedulerTierOrder = []AccountHealthTier{
	HealthTierHealthy,
	HealthTierWarm,
	HealthTierRisky,
}

type fastSchedulerEntry struct {
	acc           *Account
	dbID          int64
	dispatchScore float64
	proven        bool
	priority      int64     // account scheduling priority (issue #358): selection is globally descending, and within the same priority it is scheduled by health bucket
	reset5hAt     time.Time // immutable sort key until Update or a reset boundary
}

type fastSchedulerPosition struct {
	tier  AccountHealthTier
	index int
}

// FastScheduler is a POC scheduler that uses only local memory.
// It does not recompute the full score on the request hot path; instead it reuses the cached
// HealthTier / DispatchScore / DynamicConcurrencyLimit.
//
// Scheduling policy: scheduling priority comes first globally; within the same priority, accounts are bucketed by health tier,
// and within each bucket they are sorted by dispatch score and then served round-robin.
// verified accounts only act as a tie-breaker on equal scores, so that historical request volume does not override the priority of accounts whose quota resets soon.
// When picking an account, only the occupancy of a fixed-size candidate window is compared; quota mode still compares usage first.
// Filter and admission callbacks may access Redis/DB, so they must run outside the scheduling lock.
//
// fill_first mode (issue #501): within the same priority, accounts are sorted by 7d usage in descending order and always taken from the head of the list
// so traffic concentrates on the account with the least remaining quota; once that account is rate limited or exhausted, it naturally falls to the next one,
// and it is re-queued by its latest usage after it recovers. Accounts without usage data fall back to a fixed order (dbID ascending).
type FastScheduler struct {
	mu            sync.RWMutex
	baseLimit     int64
	schedulerMode string
	buckets       map[AccountHealthTier][]fastSchedulerEntry
	positions     map[int64]fastSchedulerPosition
	// unsorted records which buckets have had their order shuffled by the write side. The write side (add/remove/status change) only does
	// an O(1) change and marks the bucket here; the actual re-sort is deferred and merged into one per bucket at the next account pick.
	// With tens of thousands of accounts, "re-sorting the whole bucket and rebuilding the full position index on every account state change" would hold the scheduler
	// exclusive lock (bulk import is the easiest scenario to trigger this).
	unsorted   map[AccountHealthTier]bool
	cursors    [3]atomic.Uint64
	groupCheck func(apiKeyID int64, account *Account) bool
	acquire    func(account *Account, concurrencyLimit int64) bool
	generation uint64 // detects candidate invalidation while callbacks run without mu
	metrics    *schedulerRuntimeMetrics
	// retainUnavailable keeps dormant entries in sparse routing schedulers. The
	// live Account snapshot still gates acquisition, but recovery no longer
	// requires rebuilding every API-key sub-pool.
	retainUnavailable bool
	// resorts counts whole-bucket re-sorts; it is used by regression tests to lock in the property that "bulk writes no longer re-sort one entry at a time".
	resorts             atomic.Uint64
	reset5hOrderExpires map[AccountHealthTier]time.Time
}

// ResortCount returns the cumulative number of whole-bucket re-sorts.
func (s *FastScheduler) ResortCount() uint64 {
	if s == nil {
		return 0
	}
	return s.resorts.Load()
}

func NewFastScheduler(baseLimit int64, schedulerMode string) *FastScheduler {
	if baseLimit <= 0 {
		baseLimit = 1
	}
	if schedulerMode == "" {
		schedulerMode = "round_robin"
	}
	return &FastScheduler{
		baseLimit:     baseLimit,
		schedulerMode: schedulerMode,
		buckets: map[AccountHealthTier][]fastSchedulerEntry{
			HealthTierHealthy: nil,
			HealthTierWarm:    nil,
			HealthTierRisky:   nil,
		},
		positions: map[int64]fastSchedulerPosition{},
		unsorted:  map[AccountHealthTier]bool{},
	}
}

func (s *FastScheduler) SetGroupCheck(check func(apiKeyID int64, account *Account) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.groupCheck = check
	s.generation++
	s.mu.Unlock()
}

func (s *FastScheduler) SetAcquireFunc(acquire func(account *Account, concurrencyLimit int64) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.acquire = acquire
	s.generation++
	s.mu.Unlock()
}

func (s *FastScheduler) SetRetainUnavailable(enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retainUnavailable = enabled
	s.generation++
	s.mu.Unlock()
}

func validFastSchedulerTier(tier AccountHealthTier) bool {
	return tier == HealthTierHealthy || tier == HealthTierWarm || tier == HealthTierRisky
}

// normalizeRetainedTier assigns dormant/banned accounts to a stable bucket in
// routing sub-pools. Once their live tier becomes schedulable, scanRangeLocked
// moves them to the correct bucket before acquisition.
func (s *FastScheduler) normalizeRetainedTier(tier, fallback AccountHealthTier) (AccountHealthTier, bool) {
	if validFastSchedulerTier(tier) {
		return tier, true
	}
	if !s.retainUnavailable {
		return "", false
	}
	if validFastSchedulerTier(fallback) {
		return fallback, true
	}
	return HealthTierRisky, true
}

func (s *FastScheduler) SetSchedulerMode(mode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == "" {
		mode = "round_robin"
	}
	s.schedulerMode = mode

	// Re-sort all tier buckets according to the new mode.
	for _, tier := range fastSchedulerTierOrder {
		s.sortBucketLocked(tier)
	}
}

// entryLessLocked is the in-bucket sort predicate shared by all scheduling modes. The caller must hold s.mu.
//
// Sort keys for each mode:
//   - reset_5h_first: valid Codex 5h reset time ascending; unknown windows fall back to round-robin
//   - fill_first: 7d usage descending, so traffic concentrates on the account with the least remaining quota (issue #501)
//   - remaining_quota: 7d usage ascending, preferring accounts with more remaining quota
//   - round_robin healthy bucket: 7d usage ascending, spreading load across all available accounts (issue #150)
//   - others: dispatch score descending
//
// Scheduling priority (issue #358) always comes first globally; verified accounts only act as a same-score tie-breaker,
// and dbID is the final fallback that guarantees a total order (so the sort result does not depend on whether the sort is stable).
func (s *FastScheduler) entryLessLocked(tier AccountHealthTier, a, b *fastSchedulerEntry) bool {
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	if s.schedulerMode == SchedulerModeReset5hFirst {
		if order := compareReset5hDeadlines(a.reset5hAt, b.reset5hAt); order != 0 {
			return order < 0
		}
	}
	switch {
	case s.schedulerMode == "fill_first":
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA > usageB
		}
	case s.schedulerMode == "remaining_quota":
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA < usageB
		}
	case (s.schedulerMode == "round_robin" || s.schedulerMode == SchedulerModeReset5hFirst) && tier == HealthTierHealthy:
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA < usageB
		}
		if a.dispatchScore != b.dispatchScore {
			return a.dispatchScore > b.dispatchScore
		}
	default:
		if a.dispatchScore != b.dispatchScore {
			return a.dispatchScore > b.dispatchScore
		}
	}
	if a.proven != b.proven {
		return a.proven
	}
	return a.dbID < b.dbID
}

// sortBucketLocked re-sorts a single bucket, rebuilds its position index, and clears the pending re-sort mark.
func (s *FastScheduler) sortBucketLocked(tier AccountHealthTier) {
	s.generation++
	entries := s.buckets[tier]
	delete(s.unsorted, tier)
	if s.schedulerMode == SchedulerModeReset5hFirst {
		s.refreshReset5hOrderLocked(tier, time.Now())
	}
	if len(entries) == 0 {
		return
	}
	if len(entries) > 1 {
		s.resorts.Add(1)
		sort.SliceStable(entries, func(i, j int) bool {
			return s.entryLessLocked(tier, &entries[i], &entries[j])
		})
		s.buckets[tier] = entries
	}
	s.rebuildPositionsLocked(tier)
}

// ensureSortedLocked re-sorts each shuffled bucket once before an account is picked. The write side only marks buckets and does not sort,
// so a batch of consecutive changes (for example a bulk import or concurrent Release calls) is merged into a single sort here.
func (s *FastScheduler) ensureSortedLocked() {
	if s.schedulerMode == SchedulerModeReset5hFirst {
		now := time.Now()
		for tier, expiry := range s.reset5hOrderExpires {
			if !expiry.IsZero() && !expiry.After(now) {
				s.unsorted[tier] = true
			}
		}
	}
	if len(s.unsorted) == 0 {
		return
	}
	for _, tier := range fastSchedulerTierOrder {
		if s.unsorted[tier] {
			s.sortBucketLocked(tier)
		}
	}
}

func (s *FastScheduler) SchedulerMode() string {
	if s == nil {
		return "round_robin"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schedulerMode
}

// BuildFastScheduler builds a standalone scheduler from a snapshot of the current Store.
// This method does not affect the existing production traffic path; it is only used for POC, benchmark, and gradual rollout verification.
func (s *Store) BuildFastScheduler() *FastScheduler {
	if s == nil {
		return NewFastScheduler(1, "round_robin")
	}
	scheduler := NewFastScheduler(s.maxConcurrency.Load(), s.GetSchedulerMode())
	s.configureFastScheduler(scheduler)

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	scheduler.Rebuild(accounts)
	return scheduler
}

func (s *FastScheduler) Rebuild(accounts []*Account) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.buckets = map[AccountHealthTier][]fastSchedulerEntry{
		HealthTierHealthy: nil,
		HealthTierWarm:    nil,
		HealthTierRisky:   nil,
	}
	s.positions = make(map[int64]fastSchedulerPosition, len(accounts))
	s.unsorted = map[AccountHealthTier]bool{}
	s.reset5hOrderExpires = map[AccountHealthTier]time.Time{}

	// Bulk insert: put all entries into the buckets first, without sorting one by one
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.DBID == 0 {
			continue
		}
		tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
		if !s.retainUnavailable && !acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available) {
			continue
		}
		tier, keep := s.normalizeRetainedTier(tier, "")
		if !keep {
			continue
		}
		s.buckets[tier] = append(s.buckets[tier], fastSchedulerEntry{
			acc:           acc,
			dbID:          acc.DBID,
			dispatchScore: dispatchScore,
			proven:        proven,
			priority:      acc.schedulerPriority(),
			reset5hAt:     acc.reset5hSchedulingDeadline(now),
		})
	}

	// Sort each bucket only once, rebuild the position index, and compute the verified-account boundary
	for _, tier := range fastSchedulerTierOrder {
		s.sortBucketLocked(tier)
	}
}

func (s *FastScheduler) Update(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.updateLocked(acc, time.Now())
}

// UpdateMany applies one import batch while holding the scheduler lock once.
// A hot Acquire path must not observe the bucket as unsorted between individual
// additions, otherwise it can sort the whole pool once per imported account.
func (s *FastScheduler) UpdateMany(accounts []*Account) {
	if s == nil || len(accounts) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, acc := range accounts {
		s.updateLocked(acc, now)
	}
}

// updateLocked prefers to update an entry already in its bucket in place: only when the new sort key really breaks its order relative to its neighbours
// does the bucket get a pending re-sort mark. This way one Release / usage refresh / cooldown mark
// costs O(1), instead of "re-sorting the whole bucket + rebuilding the full position index".
func (s *FastScheduler) updateLocked(acc *Account, now time.Time) {
	if acc == nil || acc.DBID == 0 {
		return
	}

	pos, exists := s.positions[acc.DBID]
	tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
	schedulable := s.retainUnavailable || acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available)
	tier, validTier := s.normalizeRetainedTier(tier, pos.tier)
	schedulable = schedulable && validTier
	if !schedulable {
		if exists {
			s.removeLocked(acc.DBID)
		}
		return
	}

	entries := s.buckets[pos.tier]
	inPlace := exists && pos.tier == tier &&
		pos.index >= 0 && pos.index < len(entries) && entries[pos.index].dbID == acc.DBID
	if !inPlace {
		// New account, a changed health tier, or a position index that is out of step with the bucket: take the slow path of "remove, then re-attach".
		if exists {
			s.removeLocked(acc.DBID)
		}
		s.appendLocked(acc, tier, dispatchScore, proven)
		return
	}

	previousPriority := entries[pos.index].priority
	previousReset5h := entries[pos.index].reset5hAt
	entries[pos.index] = fastSchedulerEntry{
		acc:           acc,
		dbID:          acc.DBID,
		dispatchScore: dispatchScore,
		proven:        proven,
		priority:      acc.schedulerPriority(),
		reset5hAt:     acc.reset5hSchedulingDeadline(now),
	}
	s.trackReset5hDeadlineLocked(tier, entries[pos.index].reset5hAt)
	if previousPriority != entries[pos.index].priority ||
		(s.schedulerMode == SchedulerModeReset5hFirst && !previousReset5h.Equal(entries[pos.index].reset5hAt)) {
		s.generation++
	}
	if s.entryOutOfOrderLocked(tier, entries, pos.index) {
		s.generation++
		s.unsorted[tier] = true
	}
}

// entryOutOfOrderLocked only compares the left and right neighbours: when the bucket is already sorted, this is enough to tell whether the entry is still in the
// right position. When the bucket is already marked for re-sorting, the result does not matter, since the whole bucket is about to be re-sorted anyway.
func (s *FastScheduler) entryOutOfOrderLocked(tier AccountHealthTier, entries []fastSchedulerEntry, idx int) bool {
	if idx > 0 && s.entryLessLocked(tier, &entries[idx], &entries[idx-1]) {
		return true
	}
	if idx+1 < len(entries) && s.entryLessLocked(tier, &entries[idx+1], &entries[idx]) {
		return true
	}
	return false
}

// appendLocked attaches the entry to the end of the bucket and marks the bucket for re-sorting, O(1).
func (s *FastScheduler) appendLocked(acc *Account, tier AccountHealthTier, dispatchScore float64, proven bool) {
	s.generation++
	entries := append(s.buckets[tier], fastSchedulerEntry{
		acc:           acc,
		dbID:          acc.DBID,
		dispatchScore: dispatchScore,
		proven:        proven,
		priority:      acc.schedulerPriority(),
		reset5hAt:     acc.reset5hSchedulingDeadline(time.Now()),
	})
	s.trackReset5hDeadlineLocked(tier, entries[len(entries)-1].reset5hAt)
	s.buckets[tier] = entries
	s.positions[acc.DBID] = fastSchedulerPosition{tier: tier, index: len(entries) - 1}
	if len(entries) > 1 && s.entryLessLocked(tier, &entries[len(entries)-1], &entries[len(entries)-2]) {
		s.unsorted[tier] = true
	}
}

func (s *FastScheduler) Remove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(dbID)
}

func (s *FastScheduler) SetBaseLimit(baseLimit int64) {
	if s == nil {
		return
	}
	if baseLimit <= 0 {
		baseLimit = 1
	}
	s.mu.Lock()
	s.baseLimit = baseLimit
	s.generation++
	s.mu.Unlock()
}

func (s *FastScheduler) Acquire() *Account {
	return s.AcquireExcluding(0, nil)
}

// AcquireExcluding gets the next available account, excluding the given set of account IDs
func (s *FastScheduler) AcquireExcluding(apiKeyID int64, exclude map[int64]bool) *Account {
	return s.AcquireExcludingWithFilter(apiKeyID, exclude, nil)
}

// AcquireExcludingWithFilter gets the next available account and applies the request-level account filter.
func (s *FastScheduler) AcquireExcludingWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	return s.AcquireExcludingWithDispatch(apiKeyID, exclude, filter, DispatchPolicyStandard)
}

// AcquireExcludingWithDispatch picks an account by usage policy. spark requests ignore account-level 5h/7d.
func (s *FastScheduler) AcquireExcludingWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	return s.acquireExcludingWithDispatch(apiKeyID, exclude, filter, policy, nil, false)
}

// AcquireForAffinityWithDispatch chooses a deterministic start offset inside
// the highest-priority healthy segment. Unlike the legacy HRW path it does not
// allocate and sort an O(N) candidate slice for every new session; normally it
// inspects one entry and remains O(1) with respect to account-pool size.
func (s *FastScheduler) AcquireForAffinityWithDispatch(affinityHash uint64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	return s.acquireExcludingWithDispatch(apiKeyID, exclude, filter, policy, &affinityHash, false)
}

// HasAvailableWithDispatch is a read-only shadow check. It deliberately
// compares candidate presence rather than an exact account ID because the
// indexed and legacy round-robin cursors are independent and both choices can
// be policy-correct.
func (s *FastScheduler) HasAvailableWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) bool {
	return s.acquireExcludingWithDispatch(apiKeyID, exclude, filter, policy, nil, true) != nil
}

// fastSchedulerCandidateWindow bounds load comparisons and stack storage. A
// rejected window advances through the same priority/tier before falling back.
const fastSchedulerCandidateWindow = 8
const fastSchedulerMaxConcurrentRestarts = 16

type fastSelectionStats struct {
	scanned, filterChecks, acquireFailures, restarts int
	lockWait                                         time.Duration
}

func (s *FastScheduler) acquireExcludingWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy, affinityHash *uint64, inspectOnly bool) *Account {
	if s == nil {
		return nil
	}
	var stats fastSelectionStats
	lockStarted := time.Now()
	s.mu.Lock()
	stats.lockWait = time.Since(lockStarted)
	metrics := s.metrics
	defer func() {
		s.mu.Unlock()
		if metrics != nil {
			metrics.fastScannedAccounts.Add(uint64(stats.scanned))
			metrics.fastFilterChecks.Add(uint64(stats.filterChecks))
			metrics.fastAcquireFailures.Add(uint64(stats.acquireFailures))
			metrics.fastLockWaitNS.Add(uint64(stats.lockWait))
		}
	}()

	for {
		if stats.restarts >= fastSchedulerMaxConcurrentRestarts {
			return nil
		}
		s.ensureSortedLocked()
		changed := false
		var segmentStarts [3]int
	priorityLoop:
		for {
			var nextPriority int64
			foundPriority := false
			for tierIdx, tier := range fastSchedulerTierOrder {
				bucket := s.buckets[tier]
				start := segmentStarts[tierIdx]
				if start >= len(bucket) {
					continue
				}
				priority := bucket[start].priority
				if !foundPriority || priority > nextPriority {
					nextPriority, foundPriority = priority, true
				}
			}
			if !foundPriority {
				break
			}
			for tierIdx, tier := range fastSchedulerTierOrder {
				bucket := s.buckets[tier]
				segStart := segmentStarts[tierIdx]
				if segStart >= len(bucket) || bucket[segStart].priority != nextPriority {
					continue
				}
				// Buckets are sorted by priority. Finding the segment boundary
				// must not walk 50k equal-priority entries for every request.
				segEnd := segStart + sort.Search(len(bucket)-segStart, func(i int) bool {
					return bucket[segStart+i].priority < nextPriority
				})
				cursor := &s.cursors[tierIdx]
				var zeroCursor atomic.Uint64
				if inspectOnly || s.schedulerMode == "remaining_quota" || s.schedulerMode == "fill_first" {
					cursor = &zeroCursor
				}
				// Rank known Codex deadlines before the ordinary fallback range.
				// New affinity hashing must not skip the earliest deadline.
				if s.schedulerMode == SchedulerModeReset5hFirst && policy != DispatchPolicySpark {
					knownEnd := segStart + sort.Search(segEnd-segStart, func(i int) bool {
						return bucket[segStart+i].reset5hAt.IsZero()
					})
					acc, stale := s.scanRangeLocked(tier, segStart, knownEnd, &zeroCursor, nil, apiKeyID, exclude, filter, policy, inspectOnly, &stats)
					if acc != nil {
						return acc
					}
					if stale {
						changed = true
						break priorityLoop
					}
					segStart = knownEnd
				}
				acc, stale := s.scanRangeLocked(tier, segStart, segEnd, cursor, affinityHash, apiKeyID, exclude, filter, policy, inspectOnly, &stats)
				if acc != nil {
					return acc
				}
				if stale {
					changed = true
					break priorityLoop
				}
				segmentStarts[tierIdx] = segEnd
			}
		}
		if !changed {
			return nil
		}
	}
}

type fastSchedulerCandidate struct {
	acc       *Account
	occupied  int64
	usage     float64
	reset5hAt time.Time
}

// scanRangeLocked copies a bounded window while holding mu, then releases mu
// for callbacks. Never read the bucket slice after a generation change: an
// Update, Remove or concurrent selection may have moved its entries.
func (s *FastScheduler) scanRangeLocked(expectedTier AccountHealthTier, rangeStart, rangeEnd int, cursor *atomic.Uint64, affinityHash *uint64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy, inspectOnly bool, stats *fastSelectionStats) (*Account, bool) {
	bucket := s.buckets[expectedTier]
	rangeLen := rangeEnd - rangeStart
	if rangeLen <= 0 {
		return nil, false
	}
	start := int(cursor.Add(1)-1) % rangeLen
	windowSize := fastSchedulerCandidateWindow
	resetFirst := s.schedulerMode == SchedulerModeReset5hFirst && policy != DispatchPolicySpark &&
		!bucket[rangeStart].reset5hAt.IsZero()
	if affinityHash != nil {
		start = int(*affinityHash % uint64(rangeLen))
		windowSize = 1 // retain deterministic affinity order
	}
	quotaMode := s.schedulerMode == "remaining_quota" || s.schedulerMode == "fill_first"
	for offset := 0; offset < rangeLen; {
		var candidates [fastSchedulerCandidateWindow]fastSchedulerCandidate
		count := 0
		now := time.Now()
		for offset < rangeLen && count < windowSize {
			idx := rangeStart + (start+offset)%rangeLen
			offset++
			stats.scanned++
			entry := bucket[idx]
			if entry.acc == nil || exclude != nil && exclude[entry.dbID] || accountDispatchBlocked(entry.acc) {
				continue
			}
			tier, score, limit, proven, available := entry.acc.fastSchedulerSnapshotForPolicy(s.baseLimit, now, policy)
			tier, keepTier := s.normalizeRetainedTier(tier, expectedTier)
			if !keepTier {
				s.removeLocked(entry.dbID)
				return nil, true
			}
			if tier != expectedTier {
				s.removeLocked(entry.dbID)
				if s.retainUnavailable || entry.acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available) {
					s.insertLocked(entry.acc, now)
				}
				return nil, true
			}
			if proven != entry.proven || math.Abs(score-entry.dispatchScore) >= 1 {
				s.refreshEntryLocked(expectedTier, idx, score, proven)
			}
			occupied := accountOccupiedRequests(entry.acc)
			if !available || limit <= 0 || occupied >= limit {
				continue
			}
			candidate := fastSchedulerCandidate{acc: entry.acc, occupied: occupied}
			if resetFirst {
				candidate.reset5hAt = entry.reset5hAt
			}
			if quotaMode {
				candidate.usage = entry.acc.usagePercentForScheduling()
			}
			candidates[count] = candidate
			count++
		}
		if count == 0 {
			continue
		}
		// Stable insertion sort of at most eight entries needs no allocations.
		for i := 1; i < count; i++ {
			for j := i; j > 0; j-- {
				a, b := candidates[j], candidates[j-1]
				less := a.occupied < b.occupied
				if resetFirst && !a.reset5hAt.Equal(b.reset5hAt) {
					less = compareReset5hDeadlines(a.reset5hAt, b.reset5hAt) < 0
				}
				if quotaMode && a.usage != b.usage {
					less = a.usage < b.usage
					if s.schedulerMode == "fill_first" {
						less = a.usage > b.usage
					}
				}
				if !less {
					break
				}
				candidates[j], candidates[j-1] = b, a
			}
		}
		acc, stale := s.acquireCandidatesOutsideLock(candidates[:count], expectedTier, apiKeyID, filter, policy, inspectOnly, stats)
		if acc != nil || stale {
			return acc, stale
		}
	}
	return nil, false
}

// The caller owns mu on entry and on return, including panic unwinding. No
// filter, group check or admission callback may keep other selections and
// state updates waiting on this scheduler's lock.
func (s *FastScheduler) acquireCandidatesOutsideLock(candidates []fastSchedulerCandidate, expectedTier AccountHealthTier, apiKeyID int64, filter AccountFilter, policy DispatchPolicy, inspectOnly bool, stats *fastSelectionStats) (selected *Account, stale bool) {
	generation, groupCheck, acquire := s.generation, s.groupCheck, s.acquire
	s.mu.Unlock()
	defer func() {
		started := time.Now()
		s.mu.Lock()
		stats.lockWait += time.Since(started)
		if selected == nil && !stale && s.generation != generation {
			stale = true
			stats.restarts++
		}
	}()
	for _, candidate := range candidates {
		acc := candidate.acc
		if !acc.AllowsAPIKey(apiKeyID) || groupCheck != nil && !groupCheck(apiKeyID, acc) {
			continue
		}
		if filter != nil {
			stats.filterChecks++
			if !filter(acc) {
				continue
			}
		}
		s.mu.RLock()
		current := s.generation == generation
		baseLimit := s.baseLimit
		s.mu.RUnlock()
		if !current {
			stats.restarts++
			return nil, true
		}
		if !candidate.reset5hAt.IsZero() && !candidate.reset5hAt.After(time.Now()) {
			stats.restarts++
			return nil, true // next locked pass invalidates the expired sort key
		}
		// A slow filter can outlive a cooldown/disable/concurrency change.
		tier, _, limit, _, available := acc.fastSchedulerSnapshotForPolicy(baseLimit, time.Now(), policy)
		if !available || limit <= 0 || accountDispatchBlocked(acc) || accountOccupiedRequests(acc) >= limit {
			continue
		}
		if tier != expectedTier {
			// Account state can change before its writer reaches Update.
			// Restart so the locked scan repairs the bucket and its priority.
			return nil, true
		}
		if inspectOnly {
			return acc, false
		}
		acquired := false
		if acquire != nil {
			acquired = acquire(acc, limit)
		} else {
			acquired = tryAcquireAccount(acc, limit)
		}
		if acquired {
			return acc, false
		}
		stats.acquireFailures++
	}
	s.mu.RLock()
	stale = s.generation != generation
	s.mu.RUnlock()
	if stale {
		stats.restarts++
	}
	return nil, stale
}

// refreshEntryLocked refreshes the cached sort key of the entry at one index in the bucket in place, and marks the bucket for re-sorting if the order is broken.
func (s *FastScheduler) refreshEntryLocked(tier AccountHealthTier, idx int, dispatchScore float64, proven bool) {
	entries := s.buckets[tier]
	if idx < 0 || idx >= len(entries) || entries[idx].acc == nil {
		return
	}
	previousPriority := entries[idx].priority
	entries[idx].dispatchScore = dispatchScore
	entries[idx].proven = proven
	entries[idx].priority = entries[idx].acc.schedulerPriority()
	if previousPriority != entries[idx].priority {
		s.generation++
	}
	if s.entryOutOfOrderLocked(tier, entries, idx) {
		s.generation++
		s.unsorted[tier] = true
	}
}

func (s *FastScheduler) Release(acc *Account) {
	releaseOccupiedAccountSlot(acc)
}

func (s *FastScheduler) BucketSizes() map[AccountHealthTier]int {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[AccountHealthTier]int{
		HealthTierHealthy: len(s.buckets[HealthTierHealthy]),
		HealthTierWarm:    len(s.buckets[HealthTierWarm]),
		HealthTierRisky:   len(s.buckets[HealthTierRisky]),
	}
}

func (s *FastScheduler) insertLocked(acc *Account, now time.Time) {
	if acc == nil || acc.DBID == 0 {
		return
	}

	tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
	if !s.retainUnavailable && !acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available) {
		return
	}
	tier, keep := s.normalizeRetainedTier(tier, "")
	if !keep {
		return
	}

	s.appendLocked(acc, tier, dispatchScore, proven)
}

// removeLocked removes an entry by filling the hole with the last entry, O(1). Filling the hole shuffles the order inside the bucket,
// so the bucket is marked for re-sorting, and ensureSortedLocked repairs it before the next account pick.
func (s *FastScheduler) removeLocked(dbID int64) {
	pos, ok := s.positions[dbID]
	if !ok {
		return
	}
	s.generation++

	entries := s.buckets[pos.tier]
	if pos.index < 0 || pos.index >= len(entries) || entries[pos.index].dbID != dbID {
		// Position index is out of step with the bucket (should not happen): do a linear scan, accepting slowness over leaving an entry behind.
		pos.index = -1
		for idx := range entries {
			if entries[idx].dbID == dbID {
				pos.index = idx
				break
			}
		}
		if pos.index < 0 {
			delete(s.positions, dbID)
			s.unsorted[pos.tier] = true
			return
		}
	}

	last := len(entries) - 1
	if pos.index != last {
		entries[pos.index] = entries[last]
		s.positions[entries[pos.index].dbID] = fastSchedulerPosition{tier: pos.tier, index: pos.index}
		s.unsorted[pos.tier] = true
	}
	entries[last] = fastSchedulerEntry{}
	s.buckets[pos.tier] = entries[:last]
	delete(s.positions, dbID)
}

func (s *FastScheduler) rebuildPositionsLocked(tier AccountHealthTier) {
	for idx, entry := range s.buckets[tier] {
		s.positions[entry.dbID] = fastSchedulerPosition{
			tier:  tier,
			index: idx,
		}
	}
}

func (a *Account) fastSchedulerKeepInPool(baseLimit int64, now time.Time, tier AccountHealthTier, limit int64, available bool) bool {
	if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
		return false
	}
	if available && limit > 0 {
		return true
	}
	_, _, sparkLimit, _, sparkOK := a.fastSchedulerSnapshotForSpark(baseLimit, now)
	return sparkOK && sparkLimit > 0
}

func (a *Account) fastSchedulerSnapshotForPolicy(baseLimit int64, now time.Time, policy DispatchPolicy) (AccountHealthTier, float64, int64, bool, bool) {
	if policy == DispatchPolicySpark {
		return a.fastSchedulerSnapshotForSpark(baseLimit, now)
	}
	return a.fastSchedulerSnapshot(baseLimit, now)
}

func (a *Account) fastSchedulerSnapshotForSpark(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	tier := a.healthTierLocked()
	score := a.DispatchScore
	proven := a.TotalRequests.Load() > 10
	if score == 0 && a.SchedulerScore != 0 {
		score = a.SchedulerScore
	}
	if score == 0 && tier != HealthTierBanned && a.hasDispatchCredentialLocked() && a.Status != StatusError {
		rawScore := 100.0
		appliedBias := a.effectiveScoreBiasLocked(now, tier)
		score = rawScore + float64(appliedBias)
	}
	baseConcurrencyEffective := a.BaseConcurrencyEffective
	if baseConcurrencyEffective <= 0 {
		baseConcurrencyEffective = a.effectiveBaseConcurrencyLocked(baseLimit)
	}
	limit := a.tierConcurrencyLimitLocked(baseConcurrencyEffective, tier)
	// sparkDispatchEligibleLocked, like isAvailableLocked, only looks at state under the lock;
	// DispatchPaused is an atomic flag outside the lock, so the standard snapshot adds an explicit gate here; spark must
	// stay aligned with it, otherwise accounts paused by operators or tripped by overload circuit breakers would still be picked by spark requests.
	available := a.sparkDispatchEligibleLocked(now) && !accountDispatchBlocked(a)
	return tier, score, limit, proven, available
}

func (a *Account) fastSchedulerSnapshot(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	return a.fastSchedulerSnapshotWithUsageOverride(baseLimit, now, false)
}

func (a *Account) fastSchedulerSnapshotForContinuation(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	return a.fastSchedulerSnapshotWithUsageOverride(baseLimit, now, true)
}

func (a *Account) fastSchedulerSnapshotWithUsageOverride(baseLimit int64, now time.Time, continuation bool) (AccountHealthTier, float64, int64, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if (isPremium5hPlan(a.PlanType) && a.UsagePercent5hValid) ||
		(IsPlusOrHigherPlan(a.PlanType) && a.UsagePercent7dValid) {
		a.recomputeSchedulerLocked(baseLimit)
	}

	tier := a.healthTierLocked()
	score := a.DispatchScore
	limit := a.DynamicConcurrencyLimit
	proven := a.TotalRequests.Load() > 10

	if score == 0 && a.SchedulerScore != 0 {
		score = a.SchedulerScore
	}
	if score == 0 && tier != HealthTierBanned && a.hasDispatchCredentialLocked() && a.Status != StatusError {
		rawScore := 100.0
		appliedBias := a.effectiveScoreBiasLocked(now, tier)
		score = rawScore + float64(appliedBias)
	}
	if limit <= 0 {
		baseConcurrencyEffective := a.BaseConcurrencyEffective
		if baseConcurrencyEffective <= 0 {
			baseConcurrencyEffective = a.effectiveBaseConcurrencyLocked(baseLimit)
		}
		limit = a.quotaAutoPause5hGuardConcurrencyLimitLocked(a.tierConcurrencyLimitLocked(baseConcurrencyEffective, tier), now)
		limit = a.smartPacingConcurrencyLimitLocked(limit, now)
	}

	continuationUsageOverride := continuation && a.usageLimitContinuationEligibleLocked(now)
	available := a.Status != StatusError && tier != HealthTierBanned && a.hasDispatchCredentialLocked()
	if accountDispatchBlocked(a) {
		available = false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && !continuationUsageOverride {
		available = false
	}
	if a.quotaAutoPausedLocked(now) {
		available = false
	}
	// Fresh dispatch remains fenced by WHAM-reported usage windows even when
	// IgnoreUsageLimitStatus is enabled. Continuations use a separate, narrow
	// account-selection path in Store.
	if a.usageWindowBlocksFreshDispatchLocked(now) &&
		!continuationUsageOverride {
		available = false
	}

	return tier, score, limit, proven, available
}

func tryAcquireAccount(acc *Account, limit int64) bool {
	if acc == nil {
		return false
	}

	if limit <= 0 {
		return false
	}

	if !reserveOccupiedAccountSlot(acc, limit) {
		return false
	}
	acc.TotalRequests.Add(1)
	acc.LastUsedAt.Store(time.Now().UnixNano())
	return true
}
