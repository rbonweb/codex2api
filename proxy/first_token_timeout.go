package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type firstTokenTimeoutGuard struct {
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	fired   atomic.Bool
	once    sync.Once
}

func newFirstTokenTimeoutGuard(timeout time.Duration, cancel context.CancelFunc) *firstTokenTimeoutGuard {
	if timeout <= 0 || cancel == nil {
		return nil
	}
	guard := &firstTokenTimeoutGuard{
		timeout: timeout,
		cancel:  cancel,
	}
	guard.timer = time.AfterFunc(timeout, func() {
		guard.fired.Store(true)
		cancel()
	})
	return guard
}

func (g *firstTokenTimeoutGuard) Stop() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.timer != nil {
			g.timer.Stop()
		}
	})
}

func (g *firstTokenTimeoutGuard) MarkEvent(eventType string) {
	if g == nil || !isFirstTokenEvent(eventType) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkPayload(data []byte) {
	if g == nil || !isFirstTokenPayload(data) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkFirstToken() {
	if g == nil {
		return
	}
	g.Stop()
}

// MarkProgress clears the first-token watchdog when the first non-lifecycle frame (any frame other than response.created / response.in_progress)
// arrives. Any such frame proves that the upstream has started streaming a real response,
// so a long reasoning request that emits structure/reasoning frames first and delivers its content tokens late should not be cut off by the first-token timeout
// (issue #207). When the upstream sends nothing within the timeout (or only created/in_progress), the watchdog
// still fires, which matches the "first response sign" semantics before v2.2.7.
func (g *firstTokenTimeoutGuard) MarkProgress(eventType string) {
	if g == nil || isPreContentLifecycleEvent(eventType) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) TimedOut() bool {
	return g != nil && g.fired.Load()
}

func firstTokenTimeoutOutcome(timeout time.Duration) streamOutcome {
	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "timeout",
		failureMessage: fmt.Sprintf("upstream first token timeout: no first response event received within %s", timeout.Round(time.Millisecond)),
		penalize:       true,
	}
}

func firstTokenTimeoutError(timeout time.Duration) error {
	return ErrUpstreamTimeout(fmt.Errorf("first token timeout after %s", timeout.Round(time.Millisecond)))
}

// firstTokenTimeoutForRequest returns the first-token timeout to use for this request. Context compaction turns
// (request body contains compaction_trigger) are exempt from the watchdog: compaction must first read the entire history context
// before it emits its first compaction output item, so the first frame can naturally exceed a shorter user-configured threshold (such as 30s),
// and the encrypted_content of this turn is bound to the original account, so a timeout retry on another account will most likely fail again and exhaust the
// attempt budget, which ruins the session completely (issue #381). So compaction turns return 0 (the watchdog is off),
// so their thinking time is unlimited; abnormal hangs are still covered by the client's own timeout.
func firstTokenTimeoutForRequest(base time.Duration, isCompactionTrigger bool) time.Duration {
	if isCompactionTrigger {
		return 0
	}
	return base
}
