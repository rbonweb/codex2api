package auth

import (
	"context"
	"log"
	"strings"
	"time"
)

const premium5hFallbackWindow = 5 * time.Hour
const premium5hCooldownReason = "rate_limited_5h"

// ResponsesRateLimitedCooldownReason marks authoritative upstream rejection.
// It must remain distinct from WHAM-derived cooldowns so an active turn can
// bypass only the latter when IgnoreUsageLimitStatus is enabled.
const ResponsesRateLimitedCooldownReason = "responses_rate_limited"

// NormalizePlanType canonicalizes a plan string for behavior-level comparisons.
// OpenAI reports the $100 Pro tier as "prolite" and the top Pro tier as
// "promax"; functionally both are Pro plans with a different usage cap, so we
// fold them into "pro" so that downstream plan gating (premium 5h rate-limit,
// Spark routing, scheduler bias, 429 cooldown window) treats them identically.
// The raw value is kept in Account.PlanType so the UI can still render
// "prolite"/"promax" for operator visibility. Enterprise and Edu SKUs
// (ent26, enterprise_cbp_*, edu_plus, edu_pro) fold into their family the
// same way.
func NormalizePlanType(plan string) string {
	normalized := strings.ToLower(strings.TrimSpace(plan))
	switch normalized {
	case "prolite", "pro_lite", "pro-lite", "promax", "pro_max", "pro-max":
		return "pro"
	case "ent26", "enterprise_cbp_usage_based", "enterprise_cbp_automation":
		return "enterprise"
	case "edu_plus", "edu_pro":
		return "edu"
	default:
		return normalized
	}
}

func normalizePlanType(plan string) string {
	return NormalizePlanType(plan)
}

// isPremium5hPlan reports whether the plan carries a rolling 5h usage window
// (premium 5h rate-limit semantics). Free-tier plans only have the 7d window.
// Education (k12/edu) and other paid workspace plans do have the 5h window —
// excluding them made 429'd k12 accounts show as available and let the wham
// usage probe clear their cooldown while still limited (issue #306/#309).
// The plan gate errs broad on purpose: MarkPremium5hRateLimited and
// premium5hRateLimitedLocked additionally require an actually observed 5h
// window at 100%, so a plan without a real 5h window can never get stuck.
func isPremium5hPlan(plan string) bool {
	normalized := normalizePlanType(plan)
	switch normalized {
	case "plus", "pro", "team", "k12", "edu", "education", "go":
		return true
	default:
		return IsPlusOrHigherPlan(plan)
	}
}

// IsPlusOrHigherPlan reports whether a plan should be treated as paid for
// image-generation routing. Keep this broader than premium 5h rate-limit
// semantics so variants such as teamplus/enterprise can be preferred too.
func IsPlusOrHigherPlan(plan string) bool {
	normalized := normalizePlanType(plan)
	if normalized == "" || normalized == "free" {
		return false
	}
	switch normalized {
	case "plus", "pro", "team", "teamplus", "enterprise", "business", "edu", "education", "k12", "go":
		return true
	default:
		return strings.Contains(normalized, "plus") ||
			strings.HasPrefix(normalized, "pro") ||
			strings.HasPrefix(normalized, "team") ||
			strings.Contains(normalized, "enterprise") ||
			strings.Contains(normalized, "business")
	}
}

// IsPremium5hPlan reports whether the current account falls within the premium 5h rate-limit semantics.
func (a *Account) IsPremium5hPlan() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return isPremium5hPlan(a.PlanType)
}

func (a *Account) premium5hRateLimitedLocked(now time.Time) bool {
	if a.skipsUsageWindowLimitsLocked() {
		return false
	}
	return a.rawPremium5hRateLimitedLocked(now)
}

// rawPremium5hRateLimitedLocked is the premium 5h rate-limit check that ignores any skip switch.
// "Use credits to replace rate limiting" needs to know whether the window itself is full, so it cannot use the result above, which the switch has already flattened.
func (a *Account) rawPremium5hRateLimitedLocked(now time.Time) bool {
	if !isPremium5hPlan(a.PlanType) {
		return false
	}
	if !a.UsagePercent5hValid || a.UsagePercent5h < 100 {
		return false
	}
	if a.Reset5hAt.IsZero() {
		return false
	}
	return a.Reset5hAt.After(now)
}

// IsPremium5hRateLimited reports whether the account is currently in the premium 5h rate-limited state.
func (a *Account) IsPremium5hRateLimited() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.premium5hRateLimitedLocked(time.Now())
}

// GetUsageSnapshot5h returns the current 5h usage snapshot.
func (a *Account) GetUsageSnapshot5h() (pct float64, resetAt time.Time, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.UsagePercent5hValid {
		return 0, time.Time{}, false
	}
	return a.UsagePercent5h, a.Reset5hAt, true
}

// PersistUsageSnapshot5hOnly persists a usage snapshot that contains only 5h data.
func (s *Store) PersistUsageSnapshot5hOnly(acc *Account) {
	s.PublishUsageSnapshot5hOnly(acc, time.Now(), true)
}

// PublishUsageSnapshot5hOnly updates the in-memory 5h snapshot and scheduler.
// persist=false skips the DB write for the throttled WS frame path.
func (s *Store) PublishUsageSnapshot5hOnly(acc *Account, observedAt time.Time, persist bool) bool {
	if acc == nil || s == nil {
		return false
	}

	pct5h, reset5hAt, ok := acc.GetUsageSnapshot5h()
	if !ok {
		return false
	}

	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	acc.mu.Lock()
	acc.UsageUpdatedAt5h = observedAt
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)

	if !persist || s.db == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.withUsageSnapshotWrite(acc, func() error {
		return s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, pct5h, reset5hAt, observedAt)
	})
	if err != nil {
		log.Printf("[account %d] failed to persist 5h usage snapshot: %v", acc.DBID, err)
		return false
	}
	return true
}

// Persist5hWindowActivated writes "which Reset5hAt the window-open request has already been sent for" into credentials, so it is not sent again after a restart.
func (s *Store) Persist5hWindowActivated(acc *Account) {
	if acc == nil || s == nil || s.db == nil {
		return
	}
	resetAt := acc.GetActivated5hResetAt()
	if resetAt.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{
		"codex_5h_window_activated_reset_at": resetAt.UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("[account %d] failed to persist 5h window-open marker: %v", acc.DBID, err)
	}
}

// ClearAbsentUsageSnapshot5h clears the local 5h snapshot when the upstream authoritative probe does not return a 5h window.
// It clears both memory and credentials (to avoid "absent in memory, present in the DB" being re-hydrated after a restart).
// If it was in the premium 5h rate-limited state before clearing, the cooldown it drove is cleared as well.
// Returns true if the account locally held a valid 5h snapshot (or was in the premium 5h rate-limited state) before clearing.
func (s *Store) ClearAbsentUsageSnapshot5h(acc *Account) bool {
	if acc == nil {
		return false
	}
	observedAt := time.Now()
	cleared := false
	acc.ApplyUsageObservation(observedAt, func() {
		cleared = s.ClearAbsentUsageSnapshot5hAt(acc, observedAt)
	})
	return cleared
}

// ClearAbsentUsageSnapshot5hAt applies an authoritative absence observation.
// It clears only the cooldown created by the 5h window; newer authentication,
// generic rate-limit, error, and disabled states are deliberately preserved.
// Usage synchronizers call it from Account.ApplyUsageObservation so memory and
// persistence remain ordered as one operation.
func (s *Store) ClearAbsentUsageSnapshot5hAt(acc *Account, observedAt time.Time) bool {
	if acc == nil {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	acc.mu.Lock()
	if observedAt.Before(acc.usageObservedAt) {
		acc.mu.Unlock()
		return false
	}
	acc.usageObservedAt = observedAt
	had5h := acc.UsagePercent5hValid
	cleared5hCooldown := s != nil && acc.Status == StatusCooldown && acc.CooldownReason == premium5hCooldownReason
	if !had5h && !cleared5hCooldown {
		acc.mu.Unlock()
		return false
	}

	acc.UsagePercent5h = 0
	acc.UsagePercent5hValid = false
	acc.Reset5hAt = time.Time{}
	acc.UsageUpdatedAt5h = time.Time{}
	if cleared5hCooldown {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
		acc.LastRateLimitedAt = time.Time{}
		if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierWarm
		}
	}
	if s != nil {
		acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	}
	acc.mu.Unlock()

	if s == nil {
		// Parse-only callers may pass a nil store. They still get the in-memory
		// snapshot update, but cooldown transitions remain store-owned.
		return true
	}
	s.fastSchedulerUpdate(acc)
	if s.db == nil {
		if cleared5hCooldown {
			s.persistAccountCooldownState(acc, true, false)
		}
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.withUsageSnapshotWrite(acc, func() error {
		return s.db.ClearUsageSnapshot5h(ctx, acc.DBID)
	}); err != nil {
		log.Printf("[account %d] failed to clear 5h usage snapshot: %v", acc.DBID, err)
	}
	if cleared5hCooldown {
		s.persistAccountCooldownState(acc, true, false)
	}
	return true
}

// MarkPremium5hRateLimited marks the account as premium 5h rate-limited, and drives recovery by resetAt.
func (s *Store) MarkPremium5hRateLimited(acc *Account, resetAt time.Time) {
	_ = s.MarkPremium5hRateLimitedAt(acc, resetAt, time.Now())
}

// MarkPremium5hRateLimitedAt marks a 5h cooldown only if this observation is
// not older than the latest usage synchronization for the account.
func (s *Store) MarkPremium5hRateLimitedAt(acc *Account, resetAt, observedAt time.Time) bool {
	if acc == nil || s == nil {
		return false
	}
	return acc.ApplyUsageObservation(observedAt, func() {
		// observedAt orders competing upstream observations; stateAt reflects
		// when the state is actually applied after any preceding DB/identity work.
		s.markPremium5hRateLimited(acc, resetAt, time.Now(), premium5hCooldownReason, true)
	})
}

// MarkResponsesPremium5hRateLimited records a 429 returned by /responses.
// Unlike a WHAM snapshot, this is authoritative evidence that even an active
// turn may no longer send another request.
func (s *Store) MarkResponsesPremium5hRateLimited(acc *Account, resetAt time.Time) {
	if acc == nil || s == nil {
		return
	}
	now := time.Now()
	_ = acc.ApplyUsageObservation(now, func() {
		s.markPremium5hRateLimited(acc, resetAt, time.Now(), ResponsesRateLimitedCooldownReason, true)
	})
}

func (s *Store) markPremium5hRateLimited(acc *Account, resetAt, observedAt time.Time, cooldownReason string, persist bool) {
	now := observedAt
	if now.IsZero() {
		now = time.Now()
	}
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(premium5hFallbackWindow)
	}

	acc.mu.Lock()
	snapshotChanged := !acc.UsagePercent5hValid || acc.UsagePercent5h != 100 || !acc.Reset5hAt.Equal(resetAt)
	acc.UsagePercent5h = 100
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = resetAt
	acc.UsageUpdatedAt5h = now
	until, reason, cooldownChanged := acc.usageCooldownTargetLocked(now, resetAt, cooldownReason)
	// A repeated real 429 is new rejection evidence even when its deadline is
	// unchanged. An older in-flight success must not clear that evidence.
	if cooldownReason == ResponsesRateLimitedCooldownReason {
		acc.LastRateLimitedAt = now
	}
	if cooldownChanged {
		acc.LastRateLimitedAt = now
		acc.Status = StatusCooldown
		acc.CooldownUtil = until
		acc.CooldownReason = reason
		acc.usageCooldownPersistPending = true
		if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	if persist {
		s.persistUsageCooldown(acc)
	}

	if !persist || s.db == nil || (!cooldownChanged && !snapshotChanged) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.withUsageSnapshotWrite(acc, func() error {
		return s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, 100, resetAt, now)
	}); err != nil {
		log.Printf("[account %d] failed to persist premium 5h rate-limit snapshot: %v", acc.DBID, err)
	}
}
