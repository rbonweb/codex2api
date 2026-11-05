package proxy

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
)

// Codex overload circuit breaker: when upstream returns server_is_overloaded (service_unavailable_error,
// "Our servers are currently overloaded"), once a single account's share of that error within the sliding window
// reaches the threshold, scheduling is paused automatically for a while, so the scheduler does not keep sending traffic at an account that is already overloaded.
//
// The counting hook is attached to the common usage-log sink (logUsage): every attempt passes through there, whether it succeeds or fails,
// so it naturally gets the account ID / channel / status code / error summary. It counts only the Codex channel; once tripped, it goes through the existing account cooldown
// mechanism (reason=overload_paused): the scheduler skips the account, the account page shows the reason and a recovery countdown, and it recovers
// automatically when due, all reusing the existing path. Counts are kept in instance memory, so each instance of a multi-instance deployment trips independently.

// overloadPauseReason is the cooldown reason used by the overload circuit breaker; it feeds the rate-limit status view (accounts_paged).
const overloadPauseReason = "overload_paused"

// overloadMinSamples is the minimum number of requests in the window before a trip is judged: with too few samples the ratio is meaningless,
// so that a single error does not hurt a low-traffic account.
const overloadMinSamples = 5

// overloadErrorCode identifies overload by the error code, not by matching human-readable text.
const overloadErrorCode = "server_is_overloaded"

type overloadBucket struct {
	total      int
	overloaded int
}

type overloadBreaker struct {
	mu sync.Mutex
	// windows[accountID][unixMinute] -> count bucket; record also drops the old buckets outside the window.
	windows map[int64]map[int64]*overloadBucket
}

var globalOverloadBreaker = &overloadBreaker{windows: make(map[int64]map[int64]*overloadBucket)}

// record logs one request result and returns the account's (total, overloaded) counts within the window.
func (b *overloadBreaker) record(accountID int64, overloaded bool, windowMinutes int, now time.Time) (int, int) {
	minute := now.Unix() / 60
	cutoff := minute - int64(windowMinutes) + 1

	b.mu.Lock()
	defer b.mu.Unlock()
	buckets := b.windows[accountID]
	if buckets == nil {
		buckets = make(map[int64]*overloadBucket)
		b.windows[accountID] = buckets
	}
	for m := range buckets {
		if m < cutoff {
			delete(buckets, m)
		}
	}
	bucket := buckets[minute]
	if bucket == nil {
		bucket = &overloadBucket{}
		buckets[minute] = bucket
	}
	bucket.total++
	if overloaded {
		bucket.overloaded++
	}
	total, over := 0, 0
	for _, item := range buckets {
		total += item.total
		over += item.overloaded
	}
	return total, over
}

// reset clears the account's counting window (called after a pause is triggered; counting starts from zero on recovery, so it does not trip again immediately).
func (b *overloadBreaker) reset(accountID int64) {
	b.mu.Lock()
	delete(b.windows, accountID)
	b.mu.Unlock()
}

// shouldTripOverload reports whether the window counts meet the circuit-breaker condition.
func shouldTripOverload(total, overloaded, thresholdPercent int) bool {
	if overloaded == 0 || total < overloadMinSamples {
		return false
	}
	return overloaded*100 >= total*thresholdPercent
}

// isOverloadedUsageError reports whether this usage log entry is an upstream overload error.
func isOverloadedUsageError(input *database.UsageLogInput) bool {
	return input != nil && input.StatusCode >= 400 &&
		strings.Contains(input.ErrorMessage, overloadErrorCode)
}

// noteOverloadOutcome records the overload count before the usage log is persisted, and pauses scheduling for that account when the threshold is reached.
// Requires input.Channel to be resolved (logUsage calls it after the channel is fixed).
func (h *Handler) noteOverloadOutcome(input *database.UsageLogInput) {
	settings := CurrentRuntimeSettings()
	if !settings.CodexOverloadPauseEnabled {
		return
	}
	if input == nil || input.AccountID <= 0 || input.Channel != database.UpstreamChannelCodex {
		return
	}
	overloaded := isOverloadedUsageError(input)
	total, over := globalOverloadBreaker.record(input.AccountID, overloaded, settings.CodexOverloadWindowMinutes, time.Now())
	// A threshold can only be newly crossed when this request is an overload error, which also avoids useless checks for successful requests.
	if !overloaded || !shouldTripOverload(total, over, settings.CodexOverloadThresholdPercent) {
		return
	}
	if h == nil || h.store == nil {
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || account.HasActiveCooldown() {
		return
	}
	globalOverloadBreaker.reset(input.AccountID)
	pause := time.Duration(settings.CodexOverloadPauseMinutes) * time.Minute
	message := fmt.Sprintf("overload circuit breaker: %d-minute window has %d/%d %s, scheduling paused for %d minutes",
		settings.CodexOverloadWindowMinutes, over, total, overloadErrorCode, settings.CodexOverloadPauseMinutes)
	h.store.MarkCooldownWithErrorExactDuration(account, pause, overloadPauseReason, message)
	log.Printf("[account %d] %s", input.AccountID, message)
}
