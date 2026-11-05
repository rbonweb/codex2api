package main

import (
	"sync"
	"time"
)

// healthStoreLockStallThreshold is the threshold at which /health judges the account pool to be suspected of a deadlock:
// A single failed TryRLock is just normal lock contention and is not a reason to restart; but if it fails continuously for this long,
// the read lock is never obtained, meaning a write lock is held forever and the instance can no longer serve any request. At that point /health
// returns 503, so the container orchestrator's healthcheck restarts the instance. This restores the self-healing that the old synchronous /health got from its hang timeout
// on deadlock, while not penalizing transient contention under high load.
const healthStoreLockStallThreshold = 30 * time.Second

// healthLockProbe tracks how long /health has continuously failed to get the account pool read lock.
// Any successful probe resets it to zero; it accumulates only when there has been no success since the first failure.
type healthLockProbe struct {
	mu           sync.Mutex
	blockedSince time.Time
}

// observe records one probe result and returns the current duration of consecutive failures (0 on success).
func (p *healthLockProbe) observe(lockAcquired bool, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lockAcquired {
		p.blockedSince = time.Time{}
		return 0
	}
	if p.blockedSince.IsZero() {
		p.blockedSince = now
	}
	return now.Sub(p.blockedSince)
}
