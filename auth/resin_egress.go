package auth

import "sync/atomic"

// resinEgressEnabled records whether the Resin reverse proxy carries the Codex channel's egress. The config itself lives in the proxy package
// (auth cannot depend on proxy in reverse), and is written in sync by proxy.SetResinConfig. The scheduler's proxy pool
// fail-closed filter uses this to let Codex accounts through: Resin brings its own egress IP, so "pool empty / bound proxy disabled"
// no longer means the account will connect directly with a dirty IP (issue #679).
var resinEgressEnabled atomic.Bool

// SetResinEgressEnabled is called by the proxy package when Resin is enabled or disabled.
func SetResinEgressEnabled(enabled bool) {
	resinEgressEnabled.Store(enabled)
}

// ResinEgressEnabled reports whether Resin is currently carrying the Codex channel's egress.
func ResinEgressEnabled() bool {
	return resinEgressEnabled.Load()
}
