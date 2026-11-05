package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ============ core data structures ============

// RateLimitLevel is the rate limit level
type RateLimitLevel int

const (
	LevelGlobal  RateLimitLevel = 0 // global rate limit
	LevelAccount RateLimitLevel = 1 // account-level rate limit
	LevelModel   RateLimitLevel = 2 // model-level rate limit
)

// RateLimitStatus is the rate limit status
type RateLimitStatus int

const (
	StatusAllow RateLimitStatus = 0 // allowed
	StatusDeny  RateLimitStatus = 1 // deny the request
)

// LimitSnapshot is a rate limit snapshot (used for persistence)
type LimitSnapshot struct {
	Key        string    `json:"key"`         // rate limit key
	Level      string    `json:"level"`       // level: global/account/model
	CurrentRPM int64     `json:"current_rpm"` // current RPM
	LimitRPM   int64     `json:"limit_rpm"`   // RPM limit
	Blocked    bool      `json:"blocked"`     // whether it is blocked
	BlockedAt  time.Time `json:"blocked_at"`  // time of blocking
	ResetAt    time.Time `json:"reset_at"`    // reset time
}

// LimitMetrics is the rate limit metrics
type LimitMetrics struct {
	TotalRequests   int64     `json:"total_requests"`   // total requests
	AllowedRequests int64     `json:"allowed_requests"` // allowed requests
	BlockedRequests int64     `json:"blocked_requests"` // blocked requests
	CurrentRPM      int64     `json:"current_rpm"`      // current RPM
	LimitRPM        int64     `json:"limit_rpm"`        // RPM limit
	CooldownLevel   int       `json:"cooldown_level"`   // current cooldown level
	NextResetAt     time.Time `json:"next_reset_at"`    // next reset time
	LastUpdatedAt   time.Time `json:"last_updated_at"`  // last update time
}

// ============ token bucket rate limiter ============

// tokenBucket is a token bucket rate limiter
type tokenBucket struct {
	mu         sync.RWMutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens refilled per second
	lastRefill time.Time
}

func newTokenBucket(rpm int) *tokenBucket {
	rps := float64(rpm) / 60.0
	return &tokenBucket{
		tokens:     float64(rpm),
		maxTokens:  float64(rpm),
		refillRate: rps,
		lastRefill: time.Now(),
	}
}

// allow tries to take one token
func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = now

	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// allowN tries to take N tokens
func (tb *tokenBucket) allowN(n int) bool {
	if n <= 0 {
		return true
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.refillRate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = now

	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		return true
	}
	return false
}

// getRate returns the current rate
func (tb *tokenBucket) getRate() float64 {
	tb.mu.RLock()
	defer tb.mu.RUnlock()
	return tb.refillRate * 60 // convert to RPM
}

// getTokens returns the current token count
func (tb *tokenBucket) getTokens() float64 {
	tb.mu.RLock()
	defer tb.mu.RUnlock()

	elapsed := time.Since(tb.lastRefill).Seconds()
	tokens := tb.tokens + elapsed*tb.refillRate
	if tokens > tb.maxTokens {
		tokens = tb.maxTokens
	}
	return tokens
}

// updateRPM dynamically updates the RPM
func (tb *tokenBucket) updateRPM(rpm int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	rps := float64(rpm) / 60.0
	// first save the old value to compute the ratio
	oldMaxTokens := tb.maxTokens
	tb.maxTokens = float64(rpm)
	tb.refillRate = rps
	// keep the current token ratio
	if oldMaxTokens > 0 {
		tb.tokens = (tb.tokens / oldMaxTokens) * float64(rpm)
	} else {
		tb.tokens = float64(rpm)
	}
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}
	tb.lastRefill = time.Now()
}

// ============ smart cooldown policy ============

// cooldownManager is the smart cooldown manager
type cooldownManager struct {
	mu          sync.RWMutex
	level       int           // current cooldown level
	blockedAt   time.Time     // time the blocking started
	resetAt     time.Time     // expected reset time
	lastBackoff time.Duration // last backoff time
}

// cooldownDurations is the cooldown time config (exponential backoff)
var cooldownDurations = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	64 * time.Second,
	128 * time.Second,
	256 * time.Second,
	512 * time.Second,
	1024 * time.Second, // ~17min
	1800 * time.Second, // 30min (max)
}

// newCooldownManager creates a cooldown manager
func newCooldownManager() *cooldownManager {
	return &cooldownManager{
		level: -1, // no cooldown initially
	}
}

// computeCooldown computes the next cooldown time and level (based on CPA's backoff mechanism)
// Return values: (cooldown duration, new level)
func (cm *cooldownManager) computeCooldown() (time.Duration, int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	newLevel := cm.level + 1
	if newLevel >= len(cooldownDurations) {
		newLevel = len(cooldownDurations) - 1
	}

	duration := cooldownDurations[newLevel]
	return duration, newLevel
}

// enterCooldown enters the cooldown state
func (cm *cooldownManager) enterCooldown() time.Duration {
	duration, level := cm.computeCooldown()

	cm.mu.Lock()
	cm.level = level
	cm.blockedAt = time.Now()
	cm.resetAt = time.Now().Add(duration)
	cm.lastBackoff = duration
	cm.mu.Unlock()

	return duration
}

// reset resets the cooldown state
func (cm *cooldownManager) reset() {
	cm.mu.Lock()
	cm.level = -1
	cm.blockedAt = time.Time{}
	cm.resetAt = time.Time{}
	cm.lastBackoff = 0
	cm.mu.Unlock()
}

// isInCooldown reports whether it is cooling down
func (cm *cooldownManager) isInCooldown() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.level < 0 {
		return false
	}
	return time.Now().Before(cm.resetAt)
}

// getRemainingCooldown returns the remaining cooldown time
func (cm *cooldownManager) getRemainingCooldown() time.Duration {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.level < 0 {
		return 0
	}
	remaining := time.Until(cm.resetAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// getState returns the cooldown state
func (cm *cooldownManager) getState() (level int, blockedAt time.Time, resetAt time.Time) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.level, cm.blockedAt, cm.resetAt
}

// ============ multi-level rate limiter ============

// LevelLimiter is a single-level rate limiter
type LevelLimiter struct {
	mu         sync.RWMutex
	key        string           // rate limit key
	level      RateLimitLevel   // rate limit level
	bucket     *tokenBucket     // token bucket
	cooldown   *cooldownManager // cooldown manager
	metrics    LimitMetrics     // metrics
	enabled    bool             // whether it is enabled
	lastAccess atomic.Int64     // last access time (used for TTL cleanup)
}

// newLevelLimiter creates a single-level rate limiter
func newLevelLimiter(key string, level RateLimitLevel, rpm int) *LevelLimiter {
	ll := &LevelLimiter{
		key:      key,
		level:    level,
		enabled:  rpm > 0,
		cooldown: newCooldownManager(),
	}
	ll.lastAccess.Store(time.Now().UnixNano())
	if ll.enabled {
		ll.bucket = newTokenBucket(rpm)
		ll.metrics.LimitRPM = int64(rpm)
	}
	return ll
}

// allow tries to take a token
func (ll *LevelLimiter) allow() bool {
	ll.lastAccess.Store(time.Now().UnixNano())
	ll.mu.Lock()
	defer ll.mu.Unlock()

	// increment the total request count at function entry
	ll.metrics.TotalRequests++
	ll.metrics.LastUpdatedAt = time.Now()

	if !ll.enabled {
		return true
	}

	// check whether it is cooling down
	if ll.cooldown.isInCooldown() {
		ll.metrics.BlockedRequests++
		return false
	}

	// try to take a token
	if ll.bucket.allow() {
		ll.metrics.AllowedRequests++
		ll.metrics.CurrentRPM = int64(ll.bucket.getRate())
		return true
	}

	// not enough tokens, enter cooldown
	ll.enterCooldown()
	ll.metrics.BlockedRequests++
	return false
}

// allowN tries to take N tokens
func (ll *LevelLimiter) allowN(n int) bool {
	ll.lastAccess.Store(time.Now().UnixNano())
	ll.mu.Lock()
	defer ll.mu.Unlock()

	// increment the total request count at function entry
	ll.metrics.TotalRequests += int64(n)
	ll.metrics.LastUpdatedAt = time.Now()

	if !ll.enabled {
		return true
	}

	if ll.cooldown.isInCooldown() {
		ll.metrics.BlockedRequests += int64(n)
		return false
	}

	if ll.bucket.allowN(n) {
		ll.metrics.AllowedRequests += int64(n)
		return true
	}

	ll.enterCooldown()
	ll.metrics.BlockedRequests += int64(n)
	return false
}

// enterCooldown enters the cooldown state
func (ll *LevelLimiter) enterCooldown() time.Duration {
	duration := ll.cooldown.enterCooldown()
	ll.metrics.CooldownLevel, _, ll.metrics.NextResetAt = ll.cooldown.getState()
	return duration
}

// resetCooldown resets the cooldown state
func (ll *LevelLimiter) resetCooldown() {
	ll.cooldown.reset()
	ll.metrics.CooldownLevel = -1
	ll.metrics.NextResetAt = time.Time{}
}

// updateRPM dynamically updates the RPM
func (ll *LevelLimiter) updateRPM(rpm int) {
	ll.mu.Lock()
	defer ll.mu.Unlock()

	ll.enabled = rpm > 0
	ll.metrics.LimitRPM = int64(rpm)

	if ll.enabled {
		if ll.bucket == nil {
			ll.bucket = newTokenBucket(rpm)
		} else {
			ll.bucket.updateRPM(rpm)
		}
	} else {
		ll.bucket = nil
	}
}

// getMetrics returns the metrics
func (ll *LevelLimiter) getMetrics() LimitMetrics {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	// build a copy to avoid data races
	metrics := ll.metrics
	if ll.bucket != nil {
		metrics.CurrentRPM = int64(ll.bucket.getRate())
	}
	metrics.CooldownLevel, _, metrics.NextResetAt = ll.cooldown.getState()
	return metrics
}

// getSnapshot returns a snapshot
func (ll *LevelLimiter) getSnapshot() LimitSnapshot {
	ll.mu.RLock()
	defer ll.mu.RUnlock()

	levelStr := "unknown"
	switch ll.level {
	case LevelGlobal:
		levelStr = "global"
	case LevelAccount:
		levelStr = "account"
	case LevelModel:
		levelStr = "model"
	}

	level, blockedAt, resetAt := ll.cooldown.getState()
	return LimitSnapshot{
		Key:        ll.key,
		Level:      levelStr,
		CurrentRPM: ll.metrics.CurrentRPM,
		LimitRPM:   ll.metrics.LimitRPM,
		Blocked:    level >= 0,
		BlockedAt:  blockedAt,
		ResetAt:    resetAt,
	}
}

// ============ enhanced rate limit manager ============

// EnhancedRateLimiter is an enhanced multi-level rate limiter
type EnhancedRateLimiter struct {
	mu sync.RWMutex

	// rate limiters for each level
	globalLimiter   *LevelLimiter            // global rate limit
	accountLimiters map[string]*LevelLimiter // account-level rate limiters (key: accountID)
	modelLimiters   map[string]*LevelLimiter // model-level rate limiters (key: modelName)

	// config
	globalRPM  int // global RPM limit
	accountRPM int // per-account RPM limit
	modelRPM   int // per-model RPM limit
	enabled    bool

	// persistence
	db              *database.DB
	persistInterval time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once // protects stopCh so it is only closed once

	// metrics collection
	metricsEnabled bool
	totalLimited   atomic.Int64 // total number of rate-limited requests
}

// NewEnhancedRateLimiter creates an enhanced rate limiter
func NewEnhancedRateLimiter(db *database.DB, globalRPM, accountRPM, modelRPM int) *EnhancedRateLimiter {
	erl := &EnhancedRateLimiter{
		globalLimiter:   newLevelLimiter("global", LevelGlobal, globalRPM),
		accountLimiters: make(map[string]*LevelLimiter),
		modelLimiters:   make(map[string]*LevelLimiter),
		db:              db,
		globalRPM:       globalRPM,
		accountRPM:      accountRPM,
		modelRPM:        modelRPM,
		enabled:         globalRPM > 0 || accountRPM > 0 || modelRPM > 0,
		persistInterval: 30 * time.Second,
		stopCh:          make(chan struct{}),
		metricsEnabled:  true,
	}

	// start the persistence goroutine
	if db != nil {
		go erl.persistenceLoop()
	}

	// start the limiter cleanup goroutine (needed whether or not there is a db)
	go erl.limiterEvictionLoop()

	return erl
}

// Allow checks whether the request is allowed through (global rate limit)
func (erl *EnhancedRateLimiter) Allow() bool {
	if !erl.enabled {
		return true
	}
	return erl.globalLimiter.allow()
}

// AllowWithContext checks whether the request is allowed through (multi-level rate limit with context)
func (erl *EnhancedRateLimiter) AllowWithContext(accountID, model string) bool {
	if !erl.enabled {
		return true
	}

	erl.mu.RLock()
	defer erl.mu.RUnlock()

	// 1. check the global rate limit
	if !erl.globalLimiter.allow() {
		erl.totalLimited.Add(1)
		return false
	}

	// 2. check the account-level rate limit
	if erl.accountRPM > 0 && accountID != "" {
		accLimiter, exists := erl.accountLimiters[accountID]
		if !exists {
			// need to upgrade the lock to create a new rate limiter
			erl.mu.RUnlock()
			erl.mu.Lock()
			accLimiter, exists = erl.accountLimiters[accountID]
			if !exists {
				accLimiter = newLevelLimiter(accountID, LevelAccount, erl.accountRPM)
				erl.accountLimiters[accountID] = accLimiter
			}
			erl.mu.Unlock()
			erl.mu.RLock()
		}
		if !accLimiter.allow() {
			erl.totalLimited.Add(1)
			return false
		}
	}

	// 3. check the model-level rate limit
	if erl.modelRPM > 0 && model != "" {
		modelLimiter, exists := erl.modelLimiters[model]
		if !exists {
			// need to upgrade the lock to create a new rate limiter
			erl.mu.RUnlock()
			erl.mu.Lock()
			modelLimiter, exists = erl.modelLimiters[model]
			if !exists {
				modelLimiter = newLevelLimiter(model, LevelModel, erl.modelRPM)
				erl.modelLimiters[model] = modelLimiter
			}
			erl.mu.Unlock()
			erl.mu.RLock()
		}
		if !modelLimiter.allow() {
			erl.totalLimited.Add(1)
			return false
		}
	}

	return true
}

// UpdateGlobalRPM dynamically updates the global rate limit
func (erl *EnhancedRateLimiter) UpdateGlobalRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.globalRPM = rpm
	erl.globalLimiter.updateRPM(rpm)
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateAccountRPM dynamically updates the account-level rate limit
func (erl *EnhancedRateLimiter) UpdateAccountRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.accountRPM = rpm
	// update all existing account rate limiters
	for _, limiter := range erl.accountLimiters {
		limiter.updateRPM(rpm)
	}
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateModelRPM dynamically updates the model-level rate limit
func (erl *EnhancedRateLimiter) UpdateModelRPM(rpm int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.modelRPM = rpm
	// update all existing model rate limiters
	for _, limiter := range erl.modelLimiters {
		limiter.updateRPM(rpm)
	}
	erl.enabled = erl.globalRPM > 0 || erl.accountRPM > 0 || erl.modelRPM > 0
}

// UpdateAllRPM dynamically updates all rate limit levels
func (erl *EnhancedRateLimiter) UpdateAllRPM(globalRPM, accountRPM, modelRPM int) {
	erl.mu.Lock()
	defer erl.mu.Unlock()

	erl.globalRPM = globalRPM
	erl.accountRPM = accountRPM
	erl.modelRPM = modelRPM

	erl.globalLimiter.updateRPM(globalRPM)
	for _, limiter := range erl.accountLimiters {
		limiter.updateRPM(accountRPM)
	}
	for _, limiter := range erl.modelLimiters {
		limiter.updateRPM(modelRPM)
	}
	erl.enabled = globalRPM > 0 || accountRPM > 0 || modelRPM > 0
}

// GetGlobalMetrics returns the global rate limit metrics
func (erl *EnhancedRateLimiter) GetGlobalMetrics() LimitMetrics {
	return erl.globalLimiter.getMetrics()
}

// GetAccountMetrics returns the account rate limit metrics
func (erl *EnhancedRateLimiter) GetAccountMetrics(accountID string) LimitMetrics {
	erl.mu.RLock()
	limiter, exists := erl.accountLimiters[accountID]
	erl.mu.RUnlock()

	if !exists {
		return LimitMetrics{}
	}
	return limiter.getMetrics()
}

// GetModelMetrics returns the model rate limit metrics
func (erl *EnhancedRateLimiter) GetModelMetrics(model string) LimitMetrics {
	erl.mu.RLock()
	limiter, exists := erl.modelLimiters[model]
	erl.mu.RUnlock()

	if !exists {
		return LimitMetrics{}
	}
	return limiter.getMetrics()
}

// GetAllMetrics returns all rate limit metrics
func (erl *EnhancedRateLimiter) GetAllMetrics() map[string]interface{} {
	erl.mu.RLock()
	defer erl.mu.RUnlock()

	result := map[string]interface{}{
		"global":        erl.globalLimiter.getMetrics(),
		"total_limited": erl.totalLimited.Load(),
	}

	accountMetrics := make(map[string]LimitMetrics)
	for id, limiter := range erl.accountLimiters {
		accountMetrics[id] = limiter.getMetrics()
	}
	result["accounts"] = accountMetrics

	modelMetrics := make(map[string]LimitMetrics)
	for model, limiter := range erl.modelLimiters {
		modelMetrics[model] = limiter.getMetrics()
	}
	result["models"] = modelMetrics

	return result
}

// GetAllSnapshots returns all rate limit snapshots (used for persistence)
func (erl *EnhancedRateLimiter) GetAllSnapshots() []LimitSnapshot {
	erl.mu.RLock()
	defer erl.mu.RUnlock()

	snapshots := []LimitSnapshot{erl.globalLimiter.getSnapshot()}

	for _, limiter := range erl.accountLimiters {
		snapshots = append(snapshots, limiter.getSnapshot())
	}

	for _, limiter := range erl.modelLimiters {
		snapshots = append(snapshots, limiter.getSnapshot())
	}

	return snapshots
}

// persistenceLoop is the persistence loop
func (erl *EnhancedRateLimiter) persistenceLoop() {
	ticker := time.NewTicker(erl.persistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			erl.persistSnapshots()
		case <-erl.stopCh:
			return
		}
	}
}

// limiterEvictionLoop cleans up rate limiters unused for a long time (30 minute TTL)
func (erl *EnhancedRateLimiter) limiterEvictionLoop() {
	const (
		evictionTTL      = 30 * time.Minute
		evictionInterval = 5 * time.Minute
	)

	ticker := time.NewTicker(evictionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			erl.evictStaleLimiters(evictionTTL)
		case <-erl.stopCh:
			return
		}
	}
}

// evictStaleLimiters cleans up rate limiters not accessed within the TTL
func (erl *EnhancedRateLimiter) evictStaleLimiters(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl).UnixNano()

	erl.mu.Lock()
	defer erl.mu.Unlock()

	for key, limiter := range erl.accountLimiters {
		if limiter.lastAccess.Load() < cutoff {
			delete(erl.accountLimiters, key)
		}
	}

	for key, limiter := range erl.modelLimiters {
		if limiter.lastAccess.Load() < cutoff {
			delete(erl.modelLimiters, key)
		}
	}
}

// persistSnapshots persists snapshots to the database
func (erl *EnhancedRateLimiter) persistSnapshots() {
	// the current implementation only takes snapshots and does not actually persist them
	// to persist them, implement the actual storage logic here
	if erl.db == nil {
		return
	}

	snapshots := erl.GetAllSnapshots()
	if len(snapshots) == 0 {
		return
	}

	// store in the database (using the system_settings table)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// here we use a simple key-value storage approach
	// a real project could create a dedicated rate limit state table
	_, _ = erl.db.GetSystemSettings(ctx)
	// Note: rate limit state is transient data and usually does not need strict persistence
	// this is only a demo, actual persistence can be decided based on requirements
}

// Stop stops the rate limiter
func (erl *EnhancedRateLimiter) Stop() {
	erl.stopOnce.Do(func() {
		close(erl.stopCh)
	})
}

// ComputeCooldown computes the cooldown time (based on CPA's nextQuotaCooldown)
// Exponential backoff: 1s, 2s, 4s, 8s, ... max 30min
func ComputeCooldown(prevLevel int) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = -1
	}
	newLevel := prevLevel + 1
	if newLevel >= len(cooldownDurations) {
		newLevel = len(cooldownDurations) - 1
	}
	return cooldownDurations[newLevel], newLevel
}

// ============ backward-compatible interfaces ============

const (
	requestRateBucketSeconds = int64(5)
	requestRateBucketCount   = 12
)

// requestRateMeter uses 12 atomic 5-second buckets to record the proxy requests of the last minute.
// The request hot path does a single CAS, does not depend on whether the global RPM limit is enabled, and adds no mutex.
type requestRateMeter struct {
	buckets [requestRateBucketCount]atomic.Uint64
}

func (m *requestRateMeter) mark(now time.Time) {
	if m == nil {
		return
	}
	slot := uint32(now.Unix() / requestRateBucketSeconds)
	bucket := &m.buckets[slot%requestRateBucketCount]
	for {
		old := bucket.Load()
		oldSlot := uint32(old >> 32)
		oldCount := uint32(old)
		nextCount := uint32(1)
		if oldSlot == slot {
			if oldCount == ^uint32(0) {
				return
			}
			nextCount = oldCount + 1
		}
		next := uint64(slot)<<32 | uint64(nextCount)
		if bucket.CompareAndSwap(old, next) {
			return
		}
	}
}

func (m *requestRateMeter) rpm(now time.Time) int64 {
	if m == nil {
		return 0
	}
	currentSlot := uint32(now.Unix() / requestRateBucketSeconds)
	var total uint64
	for i := range m.buckets {
		packed := m.buckets[i].Load()
		slot := uint32(packed >> 32)
		if currentSlot-slot < requestRateBucketCount {
			total += uint64(uint32(packed))
		}
	}
	if total > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(total)
}

// RateLimiter is the global rate limiter (backward compatible)
type RateLimiter struct {
	enhanced *EnhancedRateLimiter
	traffic  requestRateMeter
	active   atomic.Int64
}

// NewRateLimiter creates a rate limiter (backward compatible)
func NewRateLimiter(rpm int) *RateLimiter {
	return &RateLimiter{
		enhanced: NewEnhancedRateLimiter(nil, rpm, 0, 0),
	}
}

// UpdateRPM dynamically updates the RPM limit (backward compatible)
func (rl *RateLimiter) UpdateRPM(rpm int) {
	if rl.enhanced != nil {
		rl.enhanced.UpdateGlobalRPM(rpm)
	}
}

// GetRPM returns the current RPM (backward compatible)
func (rl *RateLimiter) GetRPM() int {
	if rl.enhanced != nil {
		return rl.enhanced.globalRPM
	}
	return 0
}

// GetCurrentRPM returns the number of requests that actually entered the proxy routes in the last 60 seconds.
func (rl *RateLimiter) GetCurrentRPM() int64 {
	if rl == nil {
		return 0
	}
	return rl.traffic.rpm(time.Now())
}

// GetActiveRequests returns the number of proxy requests still being processed (including long connections and streaming responses).
func (rl *RateLimiter) GetActiveRequests() int64 {
	if rl == nil {
		return 0
	}
	return rl.active.Load()
}

// Middleware returns the Gin middleware (backward compatible)
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		// only limit proxy requests, not the admin panel or health checks
		if path == "/health" ||
			(len(path) >= 4 && path[:4] == "/api") ||
			(len(path) >= 6 && path[:6] == "/admin") {
			c.Next()
			return
		}

		rl.traffic.mark(time.Now())
		rl.active.Add(1)
		defer rl.active.Add(-1)
		if rl.enhanced != nil && !rl.enhanced.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": "Too many requests, please try again later",
					"type":    "rate_limit_error",
					"code":    "rate_limit_exceeded",
				},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// GetEnhancedLimiter returns the enhanced rate limiter
func (rl *RateLimiter) GetEnhancedLimiter() *EnhancedRateLimiter {
	return rl.enhanced
}

// ============ rate limit errors ============

// RateLimitError is a rate limit error
type RateLimitError struct {
	Level      RateLimitLevel
	Key        string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	levelStr := "unknown"
	switch e.Level {
	case LevelGlobal:
		levelStr = "global"
	case LevelAccount:
		levelStr = "account"
	case LevelModel:
		levelStr = "model"
	}
	return fmt.Sprintf("rate limit exceeded: level=%s, key=%s, retry_after=%v", levelStr, e.Key, e.RetryAfter)
}

// HTTPStatusCode returns the HTTP status code
func (e *RateLimitError) HTTPStatusCode() int {
	return http.StatusTooManyRequests
}
