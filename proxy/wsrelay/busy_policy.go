package wsrelay

import (
	"strings"
	"time"

	"github.com/codex2api/proxy"
)

// issue #413: one logical session monopolizes one WS connection, so concurrent requests in the same session queue up in the busy polling loop
// while a previous request is streaming output for a long time, later requests wait up to the full limit before erroring and switching accounts. The following three
// admin panel settings (system settings, applied by hot reload) control graded degradation:
//
//	codex_ws_busy_acquire_max_wait_sec  cumulative upper limit in seconds for busy/account capacity waits,
//	                                    default 30, range 1~300.
//	codex_ws_busy_overflow_enabled      when enabled, if a busy wait exceeds patience and the slot is still not idle,
//	                                    reuse or create a sibling connection on the bounded overflow slots of the same account
//	                                    (default off). The prompt_cache_key in the request body
//	                                    stays unchanged, and upstream cache identity is not affected
//	                                    by connection switching; only previous_response_id
//	                                    continuation chains depend on the original connection, so they take the AcquirePreferredConnection
//	                                    path and do not enter this degradation.
//	codex_ws_busy_patience_sec          short wait in seconds before overflow is triggered, default 2,
//	                                    range 0~max wait. The original connection, if it becomes idle during a short gap between requests,
//	                                    is still reused first, preserving connection locality.

// BusyOverflowSlots is the number of overflow sibling slots allowed per busy session. They are bounded and reused from the pool,
// to avoid triggering upstream handshake rate limits from per-request handshakes under high same-session concurrency.
const BusyOverflowSlots = 2

// busyOverflowKeyInfix marks the session key of an overflow slot; a key carrying this marker does not
// overflow again, which prevents slots from growing recursively.
const busyOverflowKeyInfix = "#ovf-"

// busyAcquireMaxWait returns the cumulative upper limit for busy/capacity waits (RuntimeSettings already clamps it to 1~300s).
func busyAcquireMaxWait() time.Duration {
	return time.Duration(proxy.CurrentRuntimeSettings().CodexWSBusyMaxWaitSec) * time.Second
}

func busyOverflowEnabled() bool {
	return proxy.CurrentRuntimeSettings().CodexWSBusyOverflow
}

// busyOverflowPatience returns the short wait before overflow is triggered; it never exceeds busyAcquireMaxWait.
func busyOverflowPatience() time.Duration {
	settings := proxy.CurrentRuntimeSettings()
	patience := time.Duration(settings.CodexWSBusyPatienceSec) * time.Second
	if maxWait := time.Duration(settings.CodexWSBusyMaxWaitSec) * time.Second; patience > maxWait {
		return maxWait
	}
	return patience
}

func isBusyOverflowSessionKey(sessionKey string) bool {
	return strings.Contains(sessionKey, busyOverflowKeyInfix)
}

// statelessConnectionSlots returns the account's blank connection budget and the general pool scan window (system setting codex_ws_stateless_slots,
// applied by hot reload, range 0~32; 0 means no blank connections are kept and the general pool is not scanned).
func statelessConnectionSlots() int {
	return proxy.CurrentRuntimeSettings().CodexWSStatelessSlots
}
