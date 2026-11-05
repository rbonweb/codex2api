package auth

import (
	"time"
)

// The sort key for the scheduling modes (the healthy buckets of remaining_quota / fill_first / round_robin) is
// "account used quota percentage", sourced from Codex's 5h/7d usage windows (wham probe, response headers).

// usagePercentForScheduling returns the used quota percentage used for scheduling order.
func (a *Account) usagePercentForScheduling() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usagePercentForSchedulingLocked(time.Now())
}

func (a *Account) usagePercentForSchedulingLocked(now time.Time) float64 {
	if a.UsagePercent7dValid {
		return a.UsagePercent7d
	}
	return 0
}
