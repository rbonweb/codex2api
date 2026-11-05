package wsrelay

import (
	"time"

	"github.com/codex2api/proxy"
)

const (
	// WeakNetworkIdleTimeout limits the reuse window after a connection goes idle in weak-network mode. A shorter window reduces
	// the chance of hitting an old connection after a VPN, residential proxy, or NAT has quietly reclaimed the socket.
	WeakNetworkIdleTimeout = 15 * time.Second

	// WeakNetworkMaxConnLifetime periodically rotates long connections on weak links, to avoid connections
	// continuing to be reused after the state on the intermediate network has aged.
	WeakNetworkMaxConnLifetime = 3 * time.Minute
)

func weakNetworkModeEnabled() bool {
	return proxy.CurrentRuntimeSettings().CodexWSWeakNetworkMode
}

func connectionIdleTimeout() time.Duration {
	if weakNetworkModeEnabled() {
		return WeakNetworkIdleTimeout
	}
	return IdleTimeout
}

func connectionMaxLifetime() time.Duration {
	if weakNetworkModeEnabled() {
		return WeakNetworkMaxConnLifetime
	}
	return MaxConnLifetime
}
