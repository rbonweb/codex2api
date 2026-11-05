package wsrelay

import (
	"context"
	"log"
	"sync"
	"time"
)

// ==================== Upstream WS idle connection keepalive (prewarm keepalive) ====================
//
// Design principles (off by default, zero extra account consumption):
//   - Only sends Ping control frames to keep alive upstream WS connections that already exist and are idle;
//   - Never opens new connections or sends any response.create business frames, so it consumes no account quota;
//   - Purpose: avoid idle connections being dropped by the upstream or an intermediate network, and skip the TLS+handshake cold start on the next reuse,
//     closer to the official Codex CLI experience of "long-lived connection + low time to first token".
//   - Weak-network mode pauses this keepalive, so connections rotate actively on a shorter idle window.
//
// When the keepalive switch is off, KeepaliveTask does not start a goroutine at all, so the existing system is unaffected.

// PingIdleConnections sends one keepalive ping to every connected and idle WS connection in the pool.
// Idle means the connection's session currently has no pending request (PendingCount() == 0),
// to avoid contending for the write lock with in-flight requests or disturbing their read/write timing.
// Returns (pinged, failed): the successful ping count and the failure count (a failed idle connection is closed by SendHeartbeat
// and removed; a connection that a request takes right after the snapshot is only dropped from the pool, its socket is not closed, see issue #436).
func (m *Manager) PingIdleConnections() (pinged int, failed int) {
	if m == nil || weakNetworkModeEnabled() {
		return 0, 0
	}
	// Function that actually sends the ping; tests can replace it (when keepalivePingFunc is nil, the default SendHeartbeat is used).
	ping := m.keepalivePingFunc
	if ping == nil {
		ping = m.SendHeartbeat
	}
	// Collect a snapshot first, to avoid modifying the map directly inside the Range callback (a failed SendHeartbeat calls Delete).
	var idle []*WsConnection
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil {
			return true
		}
		if !wc.IsConnected() {
			return true
		}
		// Connections past business idle expiry or max age are no longer kept alive; evictExpired closes them and returns their slots.
		if wc.IsExpired() || wc.IsOverAge() {
			return true
		}
		// Only keep idle connections alive: skip a connection with a pending request (it is already active and needs no extra ping).
		if wc.session != nil && wc.session.PendingCount() > 0 {
			return true
		}
		idle = append(idle, wc)
		return true
	})

	for _, wc := range idle {
		if err := ping(wc); err != nil {
			failed++
			continue
		}
		pinged++
	}
	return pinged, failed
}

// KeepaliveTask is the long-running task that periodically pings idle upstream WS connections to keep them alive.
// It is started by main only when the codex_ws_keepalive_enabled system setting is on.
type KeepaliveTask struct {
	manager     *Manager
	intervalSec func() int // Reads the interval (seconds) dynamically; supports hot updates at runtime
	enabled     func() bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewKeepaliveTask creates a keepalive task.
//   - manager: target connection pool (usually GetManager())
//   - enabled: returns whether keepalive is currently enabled (reads the store switch)
//   - intervalSec: returns the current ping interval in seconds (reads the store config)
func NewKeepaliveTask(manager *Manager, enabled func() bool, intervalSec func() int) *KeepaliveTask {
	ctx, cancel := context.WithCancel(context.Background())
	return &KeepaliveTask{
		manager:     manager,
		enabled:     enabled,
		intervalSec: intervalSec,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start starts the long-running goroutine. It is safe to start even when keepalive is currently off:
// the goroutine checks enabled() on each tick; when it is off it just idles and sends no frames.
func (t *KeepaliveTask) Start() {
	if t == nil || t.manager == nil {
		return
	}
	t.wg.Add(1)
	go t.loop()
	log.Printf("[WS-Keepalive] keepalive background task started (whether pings are actually sent depends on the runtime switch)")
}

func (t *KeepaliveTask) loop() {
	defer t.wg.Done()
	// The tick granularity is fixed at a small value; the loop decides whether to actually run based on the configured intervalSec,
	// so hot updates to the interval config need no ticker restart.
	const tickGranularity = 5 * time.Second
	ticker := time.NewTicker(tickGranularity)
	defer ticker.Stop()

	var lastRun time.Time
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			if t.enabled == nil || !t.enabled() {
				continue
			}
			interval := time.Duration(t.intervalSecOrDefault()) * time.Second
			if !lastRun.IsZero() && now.Sub(lastRun) < interval {
				continue
			}
			lastRun = now
			pinged, failed := t.manager.PingIdleConnections()
			if pinged > 0 || failed > 0 {
				log.Printf("[WS-Keepalive] keepalive done: pinged=%d failed=%d", pinged, failed)
			}
		}
	}
}

func (t *KeepaliveTask) intervalSecOrDefault() int {
	if t.intervalSec == nil {
		return 60
	}
	sec := t.intervalSec()
	if sec <= 0 {
		return 60
	}
	return sec
}

// Stop stops the long-running task and waits for the goroutine to exit.
func (t *KeepaliveTask) Stop() {
	if t == nil {
		return
	}
	t.cancel()
	t.wg.Wait()
}
