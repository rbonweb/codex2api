package auth

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestStoreSkipsCachedAccountCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := (&Store{
		accounts:   []*Account{primary, fallback},
		tokenCache: tokenCache,
	}).withMaxConcurrency(1)
	store.setCachedAccountCooldown(primary.DBID, "rate_limited", time.Now().Add(time.Hour))

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Next() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if primary.IsAvailable() {
		t.Fatal("primary account should have been synchronized into cooldown")
	}
}

func TestFastSchedulerSkipsCachedAccountCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := (&Store{
		accounts:   []*Account{primary, fallback},
		tokenCache: tokenCache,
	}).withMaxConcurrency(1)
	store.SetFastSchedulerEnabled(true)
	store.setCachedAccountCooldown(primary.DBID, "rate_limited", time.Now().Add(time.Hour))

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Next() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if primary.IsAvailable() {
		t.Fatal("primary account should have been synchronized into cooldown")
	}
}

func TestCachedPremium5hCooldownHydratesRiskyTier(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	account := newFastSchedulerTestAccount(9, HealthTierHealthy, 100, 1)
	store := (&Store{accounts: []*Account{account}, tokenCache: tokenCache}).withMaxConcurrency(1)
	store.setCachedAccountCooldown(account.DBID, "rate_limited_5h", time.Now().Add(time.Hour))

	if !store.accountHasCachedCooldown(account) {
		t.Fatal("accountHasCachedCooldown() = false, want true")
	}
	if got := account.GetSchedulerDebugSnapshot(1).HealthTier; got != string(HealthTierRisky) {
		t.Fatalf("hydrated health tier = %q, want %q", got, HealthTierRisky)
	}
}

func TestStoreSkipsCachedModelCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := (&Store{
		accounts:   []*Account{primary, fallback},
		tokenCache: tokenCache,
	}).withMaxConcurrency(1)
	store.setCachedModelCooldown(primary.DBID, ModelCooldown{
		Model:     "gpt-5.4",
		Reason:    "model_capacity",
		ResetAt:   time.Now().Add(time.Hour),
		UpdatedAt: time.Now(),
	})

	got := store.NextExcludingWithFilter(0, nil, store.WithModelCooldownFilter("GPT-5.4", nil))
	if got == nil {
		t.Fatal("NextExcludingWithFilter() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("NextExcludingWithFilter() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if !primary.IsModelRateLimited("gpt-5.4") {
		t.Fatal("primary model cooldown should have been synchronized from runtime cache")
	}
}

func TestCooldownCacheWritesAndDeletes(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	store := (&Store{
		accounts:   []*Account{acc},
		tokenCache: tokenCache,
	}).withMaxConcurrency(1)

	store.MarkCooldown(acc, 5*time.Minute, "rate_limited")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(acc.DBID)); err != nil || !ok {
		t.Fatalf("account cooldown runtime cache ok=%v err=%v, want ok", ok, err)
	}
	store.ClearCooldown(acc)
	if _, ok, err := tokenCache.GetRuntime(context.Background(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(acc.DBID)); err != nil || ok {
		t.Fatalf("account cooldown runtime cache after clear ok=%v err=%v, want miss", ok, err)
	}

	store.MarkModelCooldown(acc, "gpt-5.4", 5*time.Minute, "model_capacity")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, "gpt-5.4")); err != nil || !ok {
		t.Fatalf("model cooldown runtime cache ok=%v err=%v, want ok", ok, err)
	}
	store.ClearModelCooldown(acc, "gpt-5.4")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, "gpt-5.4")); err != nil || ok {
		t.Fatalf("model cooldown runtime cache after clear ok=%v err=%v, want miss", ok, err)
	}
}

// ageModelCooldown moves a stored cooldown record back in time, simulating "the cooldown expired long ago, but the account failed not long ago".
func ageModelCooldown(acc *Account, model string, resetAgo, updatedAgo time.Duration) {
	key := normalizeModelCooldownKey(model)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	now := time.Now()
	cd := acc.ModelCooldowns[key]
	cd.ResetAt = now.Add(-resetAgo)
	cd.UpdatedAt = now.Add(-updatedAgo)
	acc.ModelCooldowns[key] = cd
}

// In low-traffic deployments, the gap between two failures is often longer than the cooldown itself. The backoff level must not be reset just because "the cooldown has expired"
// otherwise it stays at the first level forever, backoff becomes meaningless, and the account keeps hitting upstream at a fixed base interval.
func TestModelCooldownBackoffSurvivesSparseTraffic(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 12}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	first := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if first.BackoffLevel != 0 {
		t.Fatalf("first BackoffLevel = %d, want 0", first.BackoffLevel)
	}

	// The cooldown expired 5 minutes ago, but the last failure was 10 minutes ago: the streak is still within the TTL, so the level must keep escalating.
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	second := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if second.BackoffLevel != 1 {
		t.Fatalf("second BackoffLevel = %d, want 1 (streak must survive an expired cooldown)", second.BackoffLevel)
	}
	if remaining := time.Until(second.ResetAt); remaining <= base {
		t.Fatalf("second remaining = %v, want longer than the %v base", remaining, base)
	}

	// After another round of the same sparse failures, the level keeps advancing.
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	third := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if third.BackoffLevel != 2 {
		t.Fatalf("third BackoffLevel = %d, want 2", third.BackoffLevel)
	}
	if !third.ResetAt.After(second.ResetAt.Add(-time.Second)) {
		t.Fatalf("third cooldown should be at least as long as the second")
	}
}

// After the streak TTL is exceeded, return to the first level, so an old level does not stay on the account indefinitely.
func TestModelCooldownBackoffResetsAfterStreakTTL(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 13}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	escalated := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if escalated.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want 1 before the TTL lapses", escalated.BackoffLevel)
	}

	// The last failure is already outside the TTL: the streak breaks, and it returns to the first level and base duration.
	ageModelCooldown(acc, model, 2*modelCooldownStreakTTL, 2*modelCooldownStreakTTL)
	revived := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if revived.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 after the streak TTL lapsed", revived.BackoffLevel)
	}
	if remaining := time.Until(revived.ResetAt); remaining > base+time.Minute {
		t.Fatalf("remaining = %v, want back to the %v base", remaining, base)
	}
}

// A success deletes the cooldown record, and the streak resets to zero along with it.
func TestModelCooldownBackoffResetsAfterSuccess(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 14}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	ageModelCooldown(acc, model, 5*time.Minute, time.Minute)
	if lvl := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true).BackoffLevel; lvl != 1 {
		t.Fatalf("BackoffLevel = %d, want 1", lvl)
	}

	store.ClearModelCooldown(acc, model)
	after := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if after.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 after a success cleared the cooldown", after.BackoffLevel)
	}
}

func TestModelCooldownFixedModeDoesNotBackoff(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 11}

	first := store.MarkModelCooldownWithBackoff(acc, "gpt-5.6-sol", 2*time.Second, "rate_limited_model", false)
	second := store.MarkModelCooldownWithBackoff(acc, "gpt-5.6-sol", 2*time.Second, "rate_limited_model", false)

	if first.Model == "" || second.Model == "" {
		t.Fatal("expected cooldown records")
	}
	if second.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0", second.BackoffLevel)
	}
	if remaining := time.Until(second.ResetAt); remaining < time.Second || remaining > 3*time.Second {
		t.Fatalf("remaining = %v, want fixed ~2s", remaining)
	}
	if cleared := store.ClearAllModelCooldowns(acc); cleared != 1 {
		t.Fatalf("ClearAllModelCooldowns() = %d, want 1", cleared)
	}
	if acc.IsModelRateLimited("gpt-5.6-sol") {
		t.Fatal("model cooldown should be cleared")
	}
}

// After the admin panel clears unauthorized at the database layer, it must also clear the cross-instance cooldown cache, otherwise the scheduler
// reads the cache on every pick and the cooldown is reapplied.
func TestForgetCachedAccountCooldownDropsRecord(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	account := newFastSchedulerTestAccount(11, HealthTierHealthy, 100, 1)
	store := (&Store{accounts: []*Account{account}, tokenCache: tokenCache}).withMaxConcurrency(1)
	store.setCachedAccountCooldown(account.DBID, "unauthorized", time.Now().Add(time.Hour))
	if _, ok := store.getCachedAccountCooldown(account.DBID); !ok {
		t.Fatal("precondition: cached cooldown missing")
	}

	store.ForgetCachedAccountCooldown(account.DBID)

	if _, ok := store.getCachedAccountCooldown(account.DBID); ok {
		t.Fatal("cached cooldown should have been dropped")
	}
	if store.accountHasCachedCooldown(account) {
		t.Fatal("accountHasCachedCooldown() = true after forget, want false")
	}
}
