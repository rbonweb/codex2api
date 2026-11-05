package proxy

import (
	"context"
	"time"
)

// upstreamDrainTimeout controls how long, at most, to keep reading upstream after the client disconnects in order to extract usage.
// Reference value: upstream response.completed usually arrives within 1-3 seconds after the disconnect;
// 5 seconds covers the vast majority of cases without tying up the connection indefinitely when upstream stalls.
const upstreamDrainTimeout = 5 * time.Second

// newDrainableUpstreamContext creates an upstream context decoupled from the client context,
// Purpose: after the client disconnects, it can still read for drainTimeout, so it can get from the upstream SSE
// response.completed event the usage (key to billing streaming requests).
//
// Behavior:
//   - After the client ctx is cancelled, wait drainTimeout before cancelling upstream
//   - If the caller cancels before drainTimeout, stop immediately
//   - When parent is nil, fall back to context.Background
func newDrainableUpstreamContext(clientCtx context.Context, drainTimeout time.Duration) (context.Context, context.CancelFunc) {
	baseCtx := context.Background()
	if clientCtx != nil {
		// Keep request-scoped values such as the User-Agent audit recorder while
		// detaching cancellation and deadlines for the bounded drain window.
		baseCtx = context.WithoutCancel(clientCtx)
	}
	upstreamCtx, cancelUpstream := context.WithCancel(baseCtx)
	if clientCtx == nil || drainTimeout <= 0 {
		return upstreamCtx, cancelUpstream
	}
	go func() {
		select {
		case <-clientCtx.Done():
			// A newer ingress request has already taken ownership of this exact
			// session. Draining the obsolete upstream for five more seconds would
			// retain its account/concurrency lease and defeat the handoff.
			if continuousRetryDeadlineExceeded(clientCtx) || isResponsesWSSessionPreempted(clientCtx) {
				cancelUpstream()
				return
			}
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancelUpstream()
			case <-upstreamCtx.Done():
			}
		case <-upstreamCtx.Done():
		}
	}()
	return upstreamCtx, cancelUpstream
}
