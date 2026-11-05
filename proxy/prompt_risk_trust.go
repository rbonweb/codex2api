package proxy

import (
	"context"
	"hash/crc32"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const (
	promptRiskTrustBypassAuditInterval = 10 * time.Minute
	promptRiskTrustReviewLeaseDuration = 30 * time.Second
)

var promptRiskTrustBypassAudit sync.Map  // subject key -> time.Time
var promptRiskTrustReviewLeases sync.Map // subject key -> time.Time lease expiry

func (h *Handler) promptRiskTrustPolicyForRequest(c *gin.Context) (database.PromptRiskTrustPolicy, string, bool) {
	if h == nil || h.store == nil || c == nil {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	raw, ok := c.Get(newAPIIdentityContextKey)
	if !ok {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	identity, ok := raw.(verifiedNewAPIIdentityContext)
	if !ok {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	subjectKey := database.PromptRiskNewAPIUserSubjectKey(identity.Platform, identity.Identity.UserID)
	policy, ok := h.store.GetPromptRiskTrustPolicy(subjectKey, time.Now().UTC())
	return policy, subjectKey, ok
}

func promptRiskTrustCanBypassReview(decision promptfilter.Decision, verdict promptfilter.Verdict, reviewText string) bool {
	if reviewText == "" || decision.Action != promptfilter.ActionAllow || verdict.Action != promptfilter.ActionAllow {
		return false
	}
	if len(decision.Errors) > 0 || verdict.ReviewError != "" || decision.Terminal || verdict.StrictHit || verdict.TerminalStrictHit || verdict.TerminalCategoryHit || verdict.SensitiveIntent {
		return false
	}
	threshold := verdict.Threshold
	if threshold <= 0 {
		threshold = promptfilter.DefaultThreshold
	}
	if decision.AuditScore >= threshold || decision.AuditRawScore >= threshold*2 {
		return false
	}
	return promptRiskTrustHasOnlyLowImpactAuditSignals(decision.Signals, verdict.Matched)
}

func promptRiskTrustHasOnlyLowImpactAuditSignals(signals []promptfilter.Signal, matches []promptfilter.Match) bool {
	for _, match := range matches {
		if !match.SignalOnly || match.Strict {
			return false
		}
	}
	for _, signal := range signals {
		if signal.TerminalCandidate || signal.StrikeEligible || signal.SuggestedAction == promptfilter.ActionWarn || signal.SuggestedAction == promptfilter.ActionBlock || len(signal.Matches) == 0 {
			return false
		}
		for _, match := range signal.Matches {
			if !match.SignalOnly || match.Strict {
				return false
			}
		}
	}
	return true
}

func promptRiskTrustShouldSuspend(decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	return decision.Action != promptfilter.ActionAllow || verdict.Action != promptfilter.ActionAllow ||
		decision.AuditScore > 0 || decision.AuditRawScore > 0 || len(decision.Signals) > 0 ||
		len(verdict.Matched) > 0
}

func promptRiskTrustReviewShouldSuspend(verdict promptfilter.Verdict) bool {
	return verdict.ReviewError == "" && (verdict.ReviewFlagged || verdict.Action == promptfilter.ActionBlock)
}

func promptRiskTrustReviewRequired(c *gin.Context, cfg promptfilter.Config, policy database.PromptRiskTrustPolicy, subjectKey string) bool {
	adaptive := cfg.Advanced.AdaptiveReview
	if policy.ID <= 0 || subjectKey == "" {
		return true
	}
	if !adaptive.Enabled {
		return policy.Source == database.PromptRiskTrustSourceAutomatic
	}
	now := time.Now().UTC()
	forceInterval := time.Duration(adaptive.ForceReviewIntervalMinutes) * time.Minute
	forceDue := policy.LastModelReviewAt == nil || forceInterval <= 0 || now.Sub(policy.LastModelReviewAt.UTC()) >= forceInterval
	if forceDue {
		return promptRiskTrustAcquireReviewLease(subjectKey, now)
	}
	if adaptive.SamplePercent <= 0 {
		return false
	}
	correlationID := ensurePromptPolicyRequestCorrelationID(c)
	bucket := crc32.ChecksumIEEE([]byte(subjectKey+"\x00"+correlationID)) % 100
	return int(bucket) < adaptive.SamplePercent && promptRiskTrustAcquireReviewLease(subjectKey, now)
}

func promptRiskTrustAcquireReviewLease(subjectKey string, now time.Time) bool {
	if subjectKey == "" {
		return true
	}
	sweepPromptRiskTrustReviewLeases(now)
	next := now.Add(promptRiskTrustReviewLeaseDuration)
	for {
		current, loaded := promptRiskTrustReviewLeases.LoadOrStore(subjectKey, next)
		if !loaded {
			return true
		}
		expiresAt, ok := current.(time.Time)
		if ok && now.Before(expiresAt) {
			return false
		}
		if promptRiskTrustReviewLeases.CompareAndSwap(subjectKey, current, next) {
			return true
		}
	}
}

// promptRiskTrustReleaseReviewLease releases the lease at once when the model review of a request holding it fails,
// so the same subject cannot skip forced review for the rest of the lease period without any review actually
// completing. The caller cannot tell whether it holds the lock; the cost of wrongly releasing someone else's in-flight lease is only one extra
// review, which fails on the safe side.
func promptRiskTrustReleaseReviewLease(subjectKey string) {
	if subjectKey != "" {
		promptRiskTrustReviewLeases.Delete(subjectKey)
	}
}

var promptRiskTrustLeaseSweepAtNanos atomic.Int64

const promptRiskTrustLeaseSweepInterval = 10 * time.Minute

// sweepPromptRiskTrustReviewLeases periodically sweeps lease entries that expired long ago; expired entries
// are only overwritten when the same subject makes another request, so a subject that goes quiet would hold memory forever.
func sweepPromptRiskTrustReviewLeases(now time.Time) {
	last := promptRiskTrustLeaseSweepAtNanos.Load()
	if now.UnixNano()-last < int64(promptRiskTrustLeaseSweepInterval) ||
		!promptRiskTrustLeaseSweepAtNanos.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	promptRiskTrustReviewLeases.Range(func(key, value any) bool {
		if expiresAt, ok := value.(time.Time); !ok || now.Sub(expiresAt) > promptRiskTrustReviewLeaseDuration {
			promptRiskTrustReviewLeases.Delete(key)
		}
		return true
	})
}

func promptRiskTrustBypassedSignalNames(decision promptfilter.Decision, verdict promptfilter.Verdict) []string {
	if len(decision.Signals) == 0 && len(verdict.Matched) == 0 {
		return nil
	}
	names := make([]string, 0, len(decision.Signals)+len(verdict.Matched))
	for _, signal := range decision.Signals {
		names = append(names, signal.Detector)
	}
	for _, match := range verdict.Matched {
		names = append(names, match.Name)
	}
	return names
}

func (h *Handler) recordPromptRiskTrustBypass(c *gin.Context, policy database.PromptRiskTrustPolicy, subjectKey string, bypassedSignals []string) {
	if h == nil || h.db == nil || policy.ID <= 0 || subjectKey == "" {
		return
	}
	now := time.Now().UTC()
	if raw, ok := promptRiskTrustBypassAudit.Load(subjectKey); ok {
		if last, valid := raw.(time.Time); valid && now.Sub(last) < promptRiskTrustBypassAuditInterval {
			return
		}
	}
	promptRiskTrustBypassAudit.Store(subjectKey, now)
	if len(bypassedSignals) > 0 {
		// Still bypassing on a signal-only hit is the new loosened form; keep the names of the bypassed signals in
		// the log, so operators can trace whether the loosening is being abused. It shares the same throttle window with DB auditing.
		log.Printf("adaptive prompt trust bypass with low-impact signals subject=%s signals=%v", subjectKey, bypassedSignals)
	}
	requestIDHash := promptfilter.StableEvidenceFingerprint("adaptive-trust-request", ensurePromptPolicyRequestCorrelationID(c))
	h.db.RunBackgroundTask(func(ctx context.Context) {
		_ = h.db.RecordPromptRiskTrustBypass(ctx, policy.ID, policy.SubjectType, subjectKey, requestIDHash)
	})
}

func (h *Handler) recordPromptRiskTrustModelReview(c *gin.Context, policy database.PromptRiskTrustPolicy, subjectKey string) {
	if h == nil || h.store == nil || policy.ID <= 0 || subjectKey == "" {
		return
	}
	now := time.Now().UTC()
	h.store.RecordPromptRiskTrustModelReview(subjectKey, now)
	if h.db == nil {
		return
	}
	requestIDHash := promptfilter.StableEvidenceFingerprint("adaptive-trust-model-review", ensurePromptPolicyRequestCorrelationID(c))
	h.db.RunBackgroundTask(func(ctx context.Context) {
		if err := h.db.RecordPromptRiskTrustModelReview(ctx, policy.ID, policy.SubjectType, subjectKey, requestIDHash); err != nil {
			log.Printf("record adaptive prompt trust model review failed policy=%d subject=%s: %v", policy.ID, subjectKey, err)
		}
	})
}

func (h *Handler) suspendPromptRiskTrustPolicy(policy database.PromptRiskTrustPolicy, subjectKey, reason string) {
	if h == nil || h.store == nil || subjectKey == "" {
		return
	}
	h.store.RemovePromptRiskTrustPolicy(subjectKey)
	if h.db == nil || policy.ID <= 0 {
		return
	}
	score := policy.LastRiskScore
	if score < policy.RiskThreshold {
		score = policy.RiskThreshold
	}
	h.db.RunBackgroundTask(func(ctx context.Context) {
		if _, err := h.db.SuspendPromptRiskTrustPolicy(ctx, policy.SubjectType, subjectKey, reason, score, database.PromptRiskLevelElevated); err != nil {
			log.Printf("suspend adaptive prompt trust failed policy=%d subject=%s: %v", policy.ID, subjectKey, err)
		}
	})
}
