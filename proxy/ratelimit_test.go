package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestRateMeterRollingMinute(t *testing.T) {
	var meter requestRateMeter
	base := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		meter.mark(base)
	}
	for i := 0; i < 2; i++ {
		meter.mark(base.Add(55 * time.Second))
	}
	if got := meter.rpm(base.Add(55 * time.Second)); got != 5 {
		t.Fatalf("rpm in rolling minute = %d, want 5", got)
	}
	if got := meter.rpm(base.Add(61 * time.Second)); got != 2 {
		t.Fatalf("rpm after oldest bucket expired = %d, want 2", got)
	}
}

func TestRequestRateMeterConcurrentMarks(t *testing.T) {
	var meter requestRateMeter
	now := time.Unix(1_800_000_000, 0)
	const workers = 32
	const perWorker = 1000
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				meter.mark(now)
			}
		}()
	}
	wg.Wait()
	if got := meter.rpm(now); got != workers*perWorker {
		t.Fatalf("concurrent rpm = %d, want %d", got, workers*perWorker)
	}
}

func TestRateLimiterTracksTrafficWhenLimitDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := NewRateLimiter(0)
	router := gin.New()
	router.Use(limiter.Middleware())
	activeSeen := int64(0)
	router.GET("/v1/test", func(c *gin.Context) {
		activeSeen = limiter.GetActiveRequests()
		c.Status(http.StatusNoContent)
	})
	router.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/test", nil))
	if got := limiter.GetCurrentRPM(); got != 1 {
		t.Fatalf("tracked RPM = %d, want 1 with global limit disabled", got)
	}
	if activeSeen != 1 || limiter.GetActiveRequests() != 0 {
		t.Fatalf("active requests seen/final = %d/%d, want 1/0", activeSeen, limiter.GetActiveRequests())
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if got := limiter.GetCurrentRPM(); got != 1 {
		t.Fatalf("health request changed tracked RPM to %d", got)
	}
}

// ============ Token Bucket Tests ============

func TestTokenBucket_BasicAllow(t *testing.T) {
	tb := newTokenBucket(60) // 60 RPM = 1 RPS

	// should be allowed initially
	if !tb.allow() {
		t.Error("Expected first allow to succeed")
	}
}

func TestTokenBucket_RateLimiting(t *testing.T) {
	tb := newTokenBucket(60) // 60 RPM = 1 RPS

	// quickly consume all tokens
	allowed := 0
	for i := 0; i < 70; i++ {
		if tb.allow() {
			allowed++
		}
	}

	// should only allow about 60 (the initial bucket size)
	if allowed < 50 || allowed > 65 {
		t.Errorf("Expected ~60 allowed, got %d", allowed)
	}
}

func TestTokenBucket_Refill(t *testing.T) {
	tb := newTokenBucket(60) // 60 RPM = 1 RPS

	// consume some tokens
	for i := 0; i < 10; i++ {
		tb.allow()
	}

	// wait for a refill
	time.Sleep(2 * time.Second)

	// should be able to get new tokens
	if !tb.allow() {
		t.Error("Expected token refill after wait")
	}
}

func TestTokenBucket_UpdateRPM(t *testing.T) {
	tb := newTokenBucket(60)

	// update to a higher RPM
	tb.updateRPM(120)

	if tb.maxTokens != 120 {
		t.Errorf("Expected maxTokens=120, got %f", tb.maxTokens)
	}

	// update to 0 (disabled)
	tb.updateRPM(0)
	if tb.maxTokens != 0 {
		t.Errorf("Expected maxTokens=0, got %f", tb.maxTokens)
	}
}

func TestTokenBucket_AllowN(t *testing.T) {
	tb := newTokenBucket(60)

	// request 5 tokens
	if !tb.allowN(5) {
		t.Error("Expected allowN(5) to succeed")
	}

	// request more than the remaining tokens
	if tb.allowN(100) {
		t.Error("Expected allowN(100) to fail after consuming 5")
	}
}

func TestTokenBucket_Concurrent(t *testing.T) {
	tb := newTokenBucket(1000)

	var wg sync.WaitGroup
	allowed := int64(0)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if tb.allow() {
					// use atomic operations
					atomic.AddInt64(&allowed, 1)
				}
			}
		}()
	}

	wg.Wait()
	// mainly verifies there are no race conditions
}

// ============ Cooldown Manager Tests ============

func TestCooldownManager_Basic(t *testing.T) {
	cm := newCooldownManager()

	if cm.isInCooldown() {
		t.Error("Expected no cooldown initially")
	}

	// enter cooldown
	duration := cm.enterCooldown()
	if duration <= 0 {
		t.Error("Expected positive cooldown duration")
	}

	if !cm.isInCooldown() {
		t.Error("Expected to be in cooldown after entering")
	}

	// verify the level increases
	level, _, _ := cm.getState()
	if level != 0 {
		t.Errorf("Expected level=0, got %d", level)
	}
}

func TestCooldownManager_ExponentialBackoff(t *testing.T) {
	cm := newCooldownManager()

	expectedDurations := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
	}

	for i, expected := range expectedDurations {
		cm.reset()
		// simulate entering cooldown several times to reach level i
		// each enterCooldown first calls computeCooldown, which increases the level
		// so to reach level i we need to call it i times
		for j := 0; j < i; j++ {
			cm.enterCooldown()
		}

		duration, level := cm.computeCooldown()
		if duration != expected {
			t.Errorf("Iteration %d: Expected duration=%v, got %v", i, expected, duration)
		}
		if level != i {
			t.Errorf("Iteration %d: Expected level=%d, got %d", i, i, level)
		}
	}
}

func TestCooldownManager_MaxLevel(t *testing.T) {
	cm := newCooldownManager()

	// try to exceed the maximum level
	for i := 0; i < len(cooldownDurations)+10; i++ {
		cm.enterCooldown()
	}

	level, _, _ := cm.getState()
	maxLevel := len(cooldownDurations) - 1
	if level != maxLevel {
		t.Errorf("Expected max level=%d, got %d", maxLevel, level)
	}
}

func TestCooldownManager_Reset(t *testing.T) {
	cm := newCooldownManager()

	// enter cooldown
	cm.enterCooldown()
	if !cm.isInCooldown() {
		t.Fatal("Expected to be in cooldown")
	}

	// reset
	cm.reset()
	if cm.isInCooldown() {
		t.Error("Expected no cooldown after reset")
	}

	level, _, _ := cm.getState()
	if level != -1 {
		t.Errorf("Expected level=-1 after reset, got %d", level)
	}
}

func TestCooldownManager_Remaining(t *testing.T) {
	cm := newCooldownManager()

	// enter a short cooldown
	cm.enterCooldown()
	remaining := cm.getRemainingCooldown()
	if remaining <= 0 {
		t.Error("Expected positive remaining cooldown")
	}

	// should be 0 after reset
	cm.reset()
	remaining = cm.getRemainingCooldown()
	if remaining != 0 {
		t.Errorf("Expected 0 remaining after reset, got %v", remaining)
	}
}

// ============ Level Limiter Tests ============

func TestLevelLimiter_Basic(t *testing.T) {
	ll := newLevelLimiter("test", LevelGlobal, 60)

	// should allow the request
	if !ll.allow() {
		t.Error("Expected allow to succeed")
	}

	// check the metrics
	metrics := ll.getMetrics()
	if metrics.TotalRequests != 1 {
		t.Errorf("Expected TotalRequests=1, got %d", metrics.TotalRequests)
	}
	if metrics.AllowedRequests != 1 {
		t.Errorf("Expected AllowedRequests=1, got %d", metrics.AllowedRequests)
	}
}

func TestLevelLimiter_Cooldown(t *testing.T) {
	ll := newLevelLimiter("test", LevelGlobal, 1) // 1 RPM, easy to trigger the rate limit

	// the first request should pass
	if !ll.allow() {
		t.Error("Expected first allow to succeed")
	}

	// the second request should trigger cooldown
	if ll.allow() {
		t.Error("Expected second allow to fail due to rate limit")
	}

	// check whether it is cooling down
	if !ll.cooldown.isInCooldown() {
		t.Error("Expected to be in cooldown")
	}

	// should keep rejecting during cooldown
	if ll.allow() {
		t.Error("Expected allow to fail during cooldown")
	}
}

func TestLevelLimiter_UpdateRPM(t *testing.T) {
	ll := newLevelLimiter("test", LevelGlobal, 60)

	// update RPM
	ll.updateRPM(120)

	metrics := ll.getMetrics()
	if metrics.LimitRPM != 120 {
		t.Errorf("Expected LimitRPM=120, got %d", metrics.LimitRPM)
	}

	// disable
	ll.updateRPM(0)
	if !ll.allow() {
		t.Error("Expected allow to succeed when disabled")
	}
}

func TestLevelLimiter_ResetCooldown(t *testing.T) {
	ll := newLevelLimiter("test", LevelGlobal, 1)

	// trigger cooldown
	ll.allow()
	ll.allow() // trigger the rate limit

	if !ll.cooldown.isInCooldown() {
		t.Fatal("Expected to be in cooldown")
	}

	// reset cooldown
	ll.resetCooldown()
	if ll.cooldown.isInCooldown() {
		t.Error("Expected cooldown to be reset")
	}
}

func TestLevelLimiter_Snapshot(t *testing.T) {
	ll := newLevelLimiter("test-key", LevelAccount, 60)

	// trigger some requests
	ll.allow()
	ll.allow()

	snapshot := ll.getSnapshot()
	if snapshot.Key != "test-key" {
		t.Errorf("Expected Key='test-key', got %s", snapshot.Key)
	}
	if snapshot.Level != "account" {
		t.Errorf("Expected Level='account', got %s", snapshot.Level)
	}
	if snapshot.LimitRPM != 60 {
		t.Errorf("Expected LimitRPM=60, got %d", snapshot.LimitRPM)
	}
}

func TestLevelLimiter_Metrics(t *testing.T) {
	ll := newLevelLimiter("test", LevelGlobal, 60)

	// trigger the rate limit
	for i := 0; i < 100; i++ {
		ll.allow()
	}

	metrics := ll.getMetrics()
	if metrics.TotalRequests <= 0 {
		t.Error("Expected positive TotalRequests")
	}
	if metrics.BlockedRequests <= 0 {
		t.Error("Expected positive BlockedRequests after triggering limit")
	}

	// verify the update time
	if metrics.LastUpdatedAt.IsZero() {
		t.Error("Expected LastUpdatedAt to be set")
	}
}

// ============ Enhanced Rate Limiter Tests ============

func TestEnhancedRateLimiter_Basic(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 100, 50, 30)
	defer erl.Stop()

	// global should allow
	if !erl.Allow() {
		t.Error("Expected global allow to succeed")
	}
}

func TestEnhancedRateLimiter_AllowWithContext(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 1000, 100, 50)
	defer erl.Stop()

	// request with context
	if !erl.AllowWithContext("account1", "gpt-5.4") {
		t.Error("Expected allow with context to succeed")
	}
}

func TestEnhancedRateLimiter_MultiLevel(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 100, 10, 5)
	defer erl.Stop()

	// quickly trigger the account-level rate limit
	account := "test-account"
	allowed := 0
	for i := 0; i < 20; i++ {
		if erl.AllowWithContext(account, "") {
			allowed++
		}
	}

	// some should be rate limited
	if allowed >= 20 {
		t.Error("Expected some requests to be rate limited")
	}

	// check account-level metrics
	metrics := erl.GetAccountMetrics(account)
	if metrics.TotalRequests == 0 {
		t.Error("Expected non-zero TotalRequests for account")
	}
}

func TestEnhancedRateLimiter_UpdateRPM(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 60, 30, 15)
	defer erl.Stop()

	// update global RPM
	erl.UpdateGlobalRPM(120)
	if erl.globalRPM != 120 {
		t.Errorf("Expected globalRPM=120, got %d", erl.globalRPM)
	}

	// update account RPM
	erl.UpdateAccountRPM(60)
	if erl.accountRPM != 60 {
		t.Errorf("Expected accountRPM=60, got %d", erl.accountRPM)
	}

	// update model RPM
	erl.UpdateModelRPM(30)
	if erl.modelRPM != 30 {
		t.Errorf("Expected modelRPM=30, got %d", erl.modelRPM)
	}
}

func TestEnhancedRateLimiter_UpdateAllRPM(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 60, 30, 15)
	defer erl.Stop()

	// update all at once
	erl.UpdateAllRPM(120, 60, 30)

	if erl.globalRPM != 120 {
		t.Errorf("Expected globalRPM=120, got %d", erl.globalRPM)
	}
	if erl.accountRPM != 60 {
		t.Errorf("Expected accountRPM=60, got %d", erl.accountRPM)
	}
	if erl.modelRPM != 30 {
		t.Errorf("Expected modelRPM=30, got %d", erl.modelRPM)
	}
}

func TestEnhancedRateLimiter_Metrics(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 100, 50, 25)
	defer erl.Stop()

	// trigger some requests
	for i := 0; i < 10; i++ {
		erl.AllowWithContext("acc1", "model1")
	}

	// get global metrics
	globalMetrics := erl.GetGlobalMetrics()
	if globalMetrics.TotalRequests == 0 {
		t.Error("Expected non-zero global TotalRequests")
	}

	// get account metrics
	accMetrics := erl.GetAccountMetrics("acc1")
	if accMetrics.TotalRequests == 0 {
		t.Error("Expected non-zero account TotalRequests")
	}

	// get model metrics
	modelMetrics := erl.GetModelMetrics("model1")
	if modelMetrics.TotalRequests == 0 {
		t.Error("Expected non-zero model TotalRequests")
	}

	// get all metrics
	allMetrics := erl.GetAllMetrics()
	if _, ok := allMetrics["global"]; !ok {
		t.Error("Expected 'global' in all metrics")
	}
	if _, ok := allMetrics["accounts"]; !ok {
		t.Error("Expected 'accounts' in all metrics")
	}
	if _, ok := allMetrics["models"]; !ok {
		t.Error("Expected 'models' in all metrics")
	}
}

func TestEnhancedRateLimiter_Snapshots(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 100, 50, 25)
	defer erl.Stop()

	// create some rate limiters
	erl.AllowWithContext("acc1", "model1")
	erl.AllowWithContext("acc2", "model2")

	snapshots := erl.GetAllSnapshots()
	if len(snapshots) == 0 {
		t.Error("Expected non-empty snapshots")
	}

	// should include global
	foundGlobal := false
	for _, s := range snapshots {
		if s.Level == "global" {
			foundGlobal = true
			break
		}
	}
	if !foundGlobal {
		t.Error("Expected global snapshot")
	}
}

func TestEnhancedRateLimiter_Disabled(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 0, 0, 0)
	defer erl.Stop()

	// should always allow when disabled
	if !erl.Allow() {
		t.Error("Expected allow to succeed when disabled")
	}
	if !erl.AllowWithContext("acc", "model") {
		t.Error("Expected allow with context to succeed when disabled")
	}
}

func TestEnhancedRateLimiter_Concurrent(t *testing.T) {
	erl := NewEnhancedRateLimiter(nil, 10000, 1000, 500)
	defer erl.Stop()

	var wg sync.WaitGroup

	// concurrent requests
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			account := fmt.Sprintf("account%d", idx%10)
			model := fmt.Sprintf("model%d", idx%5)
			for j := 0; j < 10; j++ {
				erl.AllowWithContext(account, model)
			}
		}(i)
	}

	wg.Wait()

	// verify metrics
	metrics := erl.GetAllMetrics()
	globalMetrics := metrics["global"].(LimitMetrics)
	if globalMetrics.TotalRequests == 0 {
		t.Error("Expected non-zero global TotalRequests after concurrent access")
	}
}

// ============ Backward Compatibility Tests ============

func TestRateLimiter_BackwardCompatible(t *testing.T) {
	rl := NewRateLimiter(60)

	// basic functionality test
	if rl.GetRPM() != 60 {
		t.Errorf("Expected RPM=60, got %d", rl.GetRPM())
	}

	// update RPM
	rl.UpdateRPM(120)
	if rl.GetRPM() != 120 {
		t.Errorf("Expected RPM=120 after update, got %d", rl.GetRPM())
	}

	// check the enhanced rate limiter
	enhanced := rl.GetEnhancedLimiter()
	if enhanced == nil {
		t.Error("Expected non-nil enhanced limiter")
	}
}

func TestRateLimiter_Middleware(t *testing.T) {
	// here we only test that the middleware exists and does not panic
	// a full test needs the Gin framework
	rl := NewRateLimiter(1000)
	_ = rl.Middleware()
	// if it gets here without a panic, the test passes
}

// ============ ComputeCooldown Tests ============

func TestComputeCooldown(t *testing.T) {
	tests := []struct {
		prevLevel     int
		expectedDur   time.Duration
		expectedLevel int
	}{
		{-1, 1 * time.Second, 0},
		{0, 2 * time.Second, 1},
		{1, 4 * time.Second, 2},
		{5, 64 * time.Second, 6},
		{100, 1800 * time.Second, 11}, // exceeds the maximum level
	}

	for _, tt := range tests {
		dur, level := ComputeCooldown(tt.prevLevel)
		if dur != tt.expectedDur {
			t.Errorf("prevLevel=%d: expected duration=%v, got %v", tt.prevLevel, tt.expectedDur, dur)
		}
		if level != tt.expectedLevel {
			t.Errorf("prevLevel=%d: expected level=%d, got %d", tt.prevLevel, tt.expectedLevel, level)
		}
	}
}

// ============ RateLimitError Tests ============

func TestRateLimitError(t *testing.T) {
	err := &RateLimitError{
		Level:      LevelGlobal,
		Key:        "test",
		RetryAfter: 5 * time.Second,
	}

	errStr := err.Error()
	if errStr == "" {
		t.Error("Expected non-empty error string")
	}

	if err.HTTPStatusCode() != http.StatusTooManyRequests {
		t.Errorf("Expected status %d, got %d", http.StatusTooManyRequests, err.HTTPStatusCode())
	}
}
