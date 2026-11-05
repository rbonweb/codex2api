package admin

import (
	"sync"
	"time"
)

const (
	adminAuthFailureWindow = 10 * time.Minute
	adminAuthBlockDuration = 10 * time.Minute
	adminAuthMaxFailures   = 5
	adminAuthMaxEntries    = 4096
)

// adminAuthThrottle records admin auth failure counts per IP.
// It only counts failures; once the threshold is reached, it blocks all authentication attempts from that IP before the key comparison.
type adminAuthThrottle struct {
	mu      sync.Mutex
	entries map[string]*adminAuthThrottleEntry
}

type adminAuthThrottleEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

func newAdminAuthThrottle() *adminAuthThrottle {
	return &adminAuthThrottle{entries: make(map[string]*adminAuthThrottleEntry)}
}

// retryAfter returns the remaining block time for this IP; returns 0 when not blocked.
func (t *adminAuthThrottle) retryAfter(ip string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[ip]
	if entry == nil {
		t.evictLocked(now)
		return t.capacityRetryAfterLocked(now)
	}
	if !now.Before(entry.blockedUntil) {
		return 0
	}
	return entry.blockedUntil.Sub(now)
}

// recordFailure records one failure; when the threshold is reached it starts blocking and returns the block duration.
func (t *adminAuthThrottle) recordFailure(ip string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[ip]
	if entry == nil {
		t.evictLocked(now)
		if retry := t.capacityRetryAfterLocked(now); retry > 0 {
			return retry
		}
		entry = &adminAuthThrottleEntry{windowStart: now}
		t.entries[ip] = entry
	}
	if now.Before(entry.blockedUntil) {
		return entry.blockedUntil.Sub(now)
	}
	if !now.Before(entry.windowStart.Add(adminAuthFailureWindow)) {
		entry.failures = 0
		entry.windowStart = now
	}
	entry.failures++
	if entry.failures < adminAuthMaxFailures {
		return 0
	}
	entry.failures = 0
	entry.windowStart = now
	entry.blockedUntil = now.Add(adminAuthBlockDuration)
	return adminAuthBlockDuration
}

func (t *adminAuthThrottle) reset(ip string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.entries, ip)
	t.mu.Unlock()
}

// evictLocked only removes expired entries; live failures and blocks survive saturation.
func (t *adminAuthThrottle) evictLocked(now time.Time) {
	if len(t.entries) < adminAuthMaxEntries {
		return
	}
	for ip, entry := range t.entries {
		if !now.Before(entry.blockedUntil) && !now.Before(entry.windowStart.Add(adminAuthFailureWindow)) {
			delete(t.entries, ip)
		}
	}
}

// Untracked sources wait for the first slot to expire before key comparison.
func (t *adminAuthThrottle) capacityRetryAfterLocked(now time.Time) time.Duration {
	if len(t.entries) < adminAuthMaxEntries {
		return 0
	}
	var earliest time.Time
	for _, entry := range t.entries {
		expires := entry.windowStart.Add(adminAuthFailureWindow)
		if entry.blockedUntil.After(expires) {
			expires = entry.blockedUntil
		}
		if earliest.IsZero() || expires.Before(earliest) {
			earliest = expires
		}
	}
	return earliest.Sub(now)
}
