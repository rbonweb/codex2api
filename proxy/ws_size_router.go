package proxy

import (
	"os"
	"strings"
	"sync"
	"time"
)

// websocketSizeRouter learns the trigger size from upstream close 1009 (message too big) events that actually happen,
// so later requests of the same size go straight to the HTTP upstream and skip the "WS is bound to fail, then fall back to HTTP" sequence
// that adds a long first-token tail (the core pain point of issue #404). The upstream frame limit is not public,
// so it makes no static guess and only records the smallest failing size observed in this process.
type websocketSizeRouter struct {
	mu        sync.Mutex
	minTooBig int
	learnedAt time.Time
}

// wsSizeRouterMinSample is the lower bound for learning samples: the real upstream frame limit is far above this, so smaller
// 1009 results are almost certainly misclassified; this also keeps an abnormally small sample from hijacking the route for a long time.
const wsSizeRouterMinSample = 64 * 1024

// wsSizeRouterTTL is how long a learned result stays valid: the upstream frame limit may change with deployments, so after expiry
// it goes back to the default of "try WS first" and is recalibrated by the next real 1009.
const wsSizeRouterTTL = 6 * time.Hour

// wsSizeRouterMarginPercent is the decision margin: between the request body and the actual WS frame there are envelope fields,
// injected fields cause small differences, so a request slightly smaller than the known failing size also very likely gets a 1009.
const wsSizeRouterMarginPercent = 95

var globalWSSizeRouter websocketSizeRouter

// wsSizeRouterDisabled applies when the system setting codex_ws_size_router_enabled is off, or when the environment variable
// escape hatch CODEX_WS_SIZE_ROUTER=off is set, restoring the old behavior of "always try WS first".
func wsSizeRouterDisabled() bool {
	if !CurrentRuntimeSettings().CodexWSSizeRouter {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_WS_SIZE_ROUTER")), "off")
}

// RecordMessageTooBig records the request body size when a close 1009 happens.
func (r *websocketSizeRouter) RecordMessageTooBig(bodySize int) {
	if bodySize < wsSizeRouterMinSample || wsSizeRouterDisabled() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.minTooBig == 0 || bodySize < r.minTooBig {
		r.minTooBig = bodySize
	}
	r.learnedAt = time.Now()
}

// PreferHTTP reports whether a request of this size should skip WebSocket and go straight to the HTTP upstream.
func (r *websocketSizeRouter) PreferHTTP(bodySize int) bool {
	if wsSizeRouterDisabled() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.minTooBig == 0 {
		return false
	}
	if time.Since(r.learnedAt) > wsSizeRouterTTL {
		r.minTooBig = 0
		return false
	}
	return bodySize >= r.minTooBig*wsSizeRouterMarginPercent/100
}
