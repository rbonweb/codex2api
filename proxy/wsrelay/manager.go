package wsrelay

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gorilla/websocket"
)

// ==================== Connection pool manager ====================

// ConnectionState is the connection state.
type ConnectionState int32

const (
	StateDisconnected ConnectionState = 0
	StateConnecting   ConnectionState = 1
	StateConnected    ConnectionState = 2
	StateClosing      ConnectionState = 3
)

// WsConnection wraps a WebSocket connection.
type WsConnection struct {
	// WebSocket connection
	conn *websocket.Conn

	// The User-Agent actually sent upstream during the handshake. When the connection is reused, each request keeps this value,
	// it must not be re-derived from the current config, otherwise after a settings change it would record a UA that was never sent.
	upstreamUserAgent      string
	upstreamUserAgentKnown bool
	upstreamClientIdentity string
	// The Version header sent during the handshake, used to tell whether an upstream "model not supported" error comes from the client version gate.
	upstreamVersion string

	// The account that created or reuses this connection. Request concurrency is managed independently by the scheduling layer,
	// response_id reuse narrows idle connections without a binding down to the shared connection slot budget.
	account *auth.Account

	// Session
	session *Session

	// Connection URL
	URL string

	// Connection pool key
	PoolKey string

	// Connection state
	state atomic.Int32

	capacitySlot atomic.Pointer[connectionCapacitySlot]
	closeCause   atomic.Pointer[connectionCloseCause]

	// Last business use time; heartbeats and idle metadata do not renew it.
	lastUsed atomic.Int64

	// Most recent inbound activity time (data frame / peer Ping / Pong reply, UnixNano). Only for probe
	// lets probe skip a round trip: recent inbound activity proves TCP is alive in both directions. 0 means no inbound activity yet, so probe does the full
	// round trip. Deliberately not merged into lastUsed, so a peer Ping does not incidentally extend idle eviction.
	lastInbound atomic.Int64

	// Creation time (UnixNano), used for connection age checks (the upstream caps connection lifetime at 60 minutes).
	// Not changed after construction; 0 means unknown (test literals) and is treated as not yet over age.
	createdAt int64

	// Write operation lock
	writeMu sync.Mutex

	// Permanent reader, business frame lease and probe state. The state is initialized on demand, to stay compatible with tests
	// that build a WsConnection as a literal with no underlying socket.
	readStateOnce       sync.Once
	readPumpOnce        sync.Once
	readFailureOnce     sync.Once
	controlHandlersOnce sync.Once
	readState           *wsReadState

	probeGateOnce sync.Once
	probeGate     chan struct{}
	probeStateMu  sync.Mutex
	probePayload  string
	probeResult   chan struct{}

	// The underlying socket and the disconnect callback are closed/called only once.
	closeOnce          sync.Once
	closeErr           error
	disconnectNotified atomic.Bool

	// HTTP handshake response
	httpResp *http.Response

	// Connection close callback
	onDisconnected func(accountID int64)

	// Permanent reader failure callback. Manager uses a pointer-level CompareAndDelete to remove exactly
	// the current connection, so it does not wrongly delete a connection under the same PoolKey that was already rebuilt.
	onReadFailure func(wc *WsConnection)

	// Protected by the owning Manager's respConnMu. Once removed from the pool,
	// late completion callbacks must never retain this connection again.
	responseBindingsDisabled bool
}

func effectiveProxyURL(account *auth.Account, proxyOverride string) string {
	proxyURL := ""
	if account != nil {
		account.Mu().RLock()
		proxyURL = account.ProxyURL
		account.Mu().RUnlock()
	}
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = proxyOverride
	}
	return strings.TrimSpace(proxyURL)
}

// configureWebsocketDialerProxy applies the configured proxy to a Gorilla
// WebSocket dialer. Gorilla accepts "socks5" but not the curl-style
// "socks5h" alias. Its SOCKS5 implementation already sends domain names to
// the proxy, so normalizing the alias preserves remote DNS resolution.
func configureWebsocketDialerProxy(dialer *websocket.Dialer, rawProxyURL string) error {
	parsed, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL failed: %w", err)
	}

	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme == "socks5h" {
		parsed.Scheme = "socks5"
	}
	dialer.Proxy = http.ProxyURL(parsed)
	return nil
}

// NewWsConnection creates a WebSocket connection.
func NewWsConnection(conn *websocket.Conn, session *Session, wsURL string) *WsConnection {
	wc := &WsConnection{
		conn:      conn,
		session:   session,
		URL:       wsURL,
		createdAt: time.Now().UnixNano(),
	}
	wc.lastUsed.Store(time.Now().UnixNano())
	wc.state.Store(int32(StateConnected))
	return wc
}

// Touch synchronously updates the last business use time of the connection and session.
func (wc *WsConnection) Touch() {
	wc.lastUsed.Store(time.Now().UnixNano())
	if wc.session != nil {
		slot := wc.capacitySlot.Load()
		wc.session.touchActivity(slot == nil || connectionCapacityKind(slot.kind.Load()) == capacityChat)
	}
}

// touchInbound records one inbound activity (data frame / peer Ping / Pong reply to our Ping).
func (wc *WsConnection) touchInbound() {
	wc.lastInbound.Store(time.Now().UnixNano())
}

// recentInboundWithin reports whether there was inbound activity within the last window.
func (wc *WsConnection) recentInboundWithin(window time.Duration) bool {
	ts := wc.lastInbound.Load()
	if ts == 0 {
		return false
	}
	return time.Since(time.Unix(0, ts)) <= window
}

// IsExpired only applies business idle reclamation to connections that were used for chat; blank slots are kept alive normally.
func (wc *WsConnection) IsExpired() bool {
	if !weakNetworkModeEnabled() && (wc.session == nil || !wc.session.hasUserContext()) {
		return false
	}
	lastUsed := time.Unix(0, wc.lastUsed.Load())
	return time.Since(lastUsed) >= connectionIdleTimeout()
}

// IsOverAge checks whether the connection exceeds the maximum lifetime of the current mode. An over-age connection can no longer take new requests:
// default mode avoids the upstream 60-minute hard limit early; weak-network mode uses a shorter window and rotates actively.
func (wc *WsConnection) IsOverAge() bool {
	if wc.createdAt == 0 {
		return false
	}
	return time.Since(time.Unix(0, wc.createdAt)) > connectionMaxLifetime()
}

// IsConnected checks whether it is connected.
func (wc *WsConnection) IsConnected() bool {
	return wc.state.Load() == int32(StateConnected)
}

// Close closes the connection safely.
func (wc *WsConnection) Close() error {
	if wc == nil {
		return nil
	}
	wc.closeOnce.Do(func() {
		wc.recordDirectClose()
		wc.state.Store(int32(StateClosing))
		if wc.conn != nil {
			wc.closeErr = wc.conn.Close()
		}
		wc.state.Store(int32(StateDisconnected))
		wc.capacitySlot.Load().release()
		wc.logClose()
	})
	if wc.onDisconnected != nil && wc.session != nil && wc.disconnectNotified.CompareAndSwap(false, true) {
		wc.onDisconnected(wc.session.AccountID)
	}
	return wc.closeErr
}

// SetState sets the connection state.
func (wc *WsConnection) SetState(state ConnectionState) {
	wc.state.Store(int32(state))
}

// WriteMessage writes a message safely.
func (wc *WsConnection) WriteMessage(messageType int, data []byte) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() || wc.conn == nil {
		return fmt.Errorf("websocket connection is not connected")
	}
	leaseID, tracksLease, err := wc.beginReadLeaseWrite(messageType)
	if err != nil {
		return err
	}

	wc.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	defer wc.conn.SetWriteDeadline(time.Time{})

	writeErr := wc.conn.WriteMessage(messageType, data)
	if writeErr == nil && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) {
		wc.Touch()
	}
	if tracksLease {
		return wc.completeReadLeaseWrite(leaseID, writeErr)
	}
	return writeErr
}

// HTTPResponse returns the HTTP handshake response.
func (wc *WsConnection) HTTPResponse() *http.Response {
	return wc.httpResp
}

// ==================== Connection pool manager ====================

// Manager is the WebSocket connection pool manager.
type Manager struct {
	// Connection pool (accountID -> *WsConnection)
	connections sync.Map

	// Session pool (accountID -> *Session)
	sessions sync.Map

	// Dialer config
	dialer *websocket.Dialer

	// Cleanup timer
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
	stopOnce      sync.Once

	// Connection callbacks
	onConnected    func(accountID int64, session *Session)
	onDisconnected func(accountID int64)

	// Read-write lock guarding the callback settings
	mu sync.RWMutex

	// Serializes at the pool key level, so the same logical session does not compete for the same connection during acquire
	keyLocks refCountedLockRegistry[string]
	// Serializes connection acquisition per account, so the per-account limit is still strictly enforced when connections are created across sessions/pool keys
	// Per-account connection limit, so many short sessions do not each leave an idle connection behind.
	accountLocks    refCountedLockRegistry[int64]
	capacityMu      sync.Mutex
	connectionSlots map[int64]map[*connectionCapacitySlot]*WsConnection

	// Per-account wakeup for waiters: busy/capacity waiters are woken on the same account when a connection is released, destroyed or created, or an in-flight request ends,
	// and re-pick a connection right away instead of waiting out the whole backoff interval (at most AcquireMaxBackoff).
	// Each notify closes and replaces the current channel; an entry exists only while waiters exist.
	waitNotifyMu sync.Mutex
	waitNotify   map[int64]chan struct{}

	// response_id -> connection binding (continuation affinity). When the upstream chatgpt backend has no server-side storage,
	// the context for previous_response_id only lives on the WS connection that produced that response; a request with a continuation ID
	// must go back to the original connection; landing on another slot would get "previous response not found".
	// Modeled on sub2api openai_ws_state_store's BindResponseConn/GetResponseConn.
	respConnMu       sync.Mutex
	respConnBindings map[string]responseConnBinding

	// Optional probe function (for test replacement); when nil, the default probeConnection is used
	probeFunc func(wc *WsConnection) bool

	// Optional keepalive ping function (for test replacement); when nil, the default SendHeartbeat is used
	keepalivePingFunc func(wc *WsConnection) error

	// Test hook: fires after a connection is written to the pool and before the first pending/read lease is set up.
	afterConnectionStored func(wc *WsConnection)
}

// responseConnBinding records which connection produced a given response_id.
// The conn pointer doubles as an identity check: when a connection under the same poolKey is rebuilt, the old binding becomes invalid automatically.
// apiKey is the downstream API key that produced the response (plaintext, kept in memory only); lookup requires a match,
// to stop another key from using someone else's response_id to target and squeeze onto their connection (the same principle as the response cache's
// owner isolation).
type responseConnBinding struct {
	conn       *WsConnection
	sessionKey string
	accountID  int64
	apiKey     string
	model      string
	expiresAt  time.Time
}

const (
	// responseConnBindingTTL is how long a continuation binding lives. Upstream idle connections are themselves cleaned up after IdleTimeout
	// (30min), so a binding living longer would be pointless; align it with that.
	responseConnBindingTTL = IdleTimeout
	// responseConnBindingMaxEntries is the binding table limit, to prevent memory bloat.
	responseConnBindingMaxEntries = 4096
)

// wsWriteBufferPool shares write buffers across all upstream WS connections, reducing memory use under high concurrency.
var wsWriteBufferPool = &sync.Pool{}

// NewManager creates the connection pool manager.
func NewManager() *Manager {
	m := &Manager{
		dialer: &websocket.Dialer{
			HandshakeTimeout:  HandshakeTimeout,
			EnableCompression: true,
			// Upstream Codex WS frames can reach 48-91KB; the default 4KB buffer would need multiple syscall rounds per frame;
			// raising it to 64KB reduces read/write loop iterations, and write buffers are reused from the shared pool.
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			WriteBufferPool: wsWriteBufferPool,
			NetDialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		stopCleanup: make(chan struct{}),
	}

	// Start background cleanup
	m.cleanupTicker = time.NewTicker(30 * time.Second)
	go m.cleanupLoop()

	return m
}

// cleanupLoop periodically cleans up expired connections.
func (m *Manager) cleanupLoop() {
	for {
		select {
		case <-m.cleanupTicker.C:
			m.evictExpired()
		case <-m.stopCleanup:
			m.cleanupTicker.Stop()
			return
		}
	}
}

// evictExpired cleans up expired connections and sessions (including over-age idle connections, rotated proactively to avoid hitting the upstream lifetime limit).
// Connections/sessions with in-flight requests are always skipped: IsExpired only looks at lastUsed/LastActiveAt, and during long upstream reasoning
// or a lost pong would misjudge an active object as idle; calling Close directly would cut off all in-flight streams in the same second
// (issue #436); clean up in the next round after in-flight work finishes (the read path's business-frame silence cap ActiveReadMaxTurnSilence is the backstop).
func (m *Manager) evictExpired() {
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		if wc.session != nil && wc.session.PendingCount() > 0 {
			return true
		}
		if wc.IsExpired() || !wc.IsConnected() || isRotatableOverAge(wc) {
			m.discardConnectionFor(wc, connectionExpiryReason(wc))
			if wc.session != nil {
				m.notifyAccountWaiters(wc.session.AccountID)
			}
		}
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		if s.PendingCount() > 0 {
			return true
		}
		if s.IsExpired() || !s.IsConnected() {
			m.sessions.CompareAndDelete(key, s)
			s.Close()
		}
		return true
	})
	m.evictResponseConnBindings(time.Now())
	m.trimAllIdleChatConnections()
}

// Stop stops the manager.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCleanup)
		m.closeAll()
	})
}

// closeAll closes all connections.
func (m *Manager) closeAll() {
	m.closeCapacityConnections()
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		m.discardConnectionFor(wc, closeManagerShutdown)
		return true
	})
	m.respConnMu.Lock()
	m.respConnBindings = nil
	m.respConnMu.Unlock()

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		m.sessions.Delete(key)
		s.Close()
		return true
	})
}

// SetOnConnected sets the connection callback.
func (m *Manager) SetOnConnected(fn func(accountID int64, session *Session)) {
	m.mu.Lock()
	m.onConnected = fn
	m.mu.Unlock()
}

// SetOnDisconnected sets the disconnect callback.
func (m *Manager) SetOnDisconnected(fn func(accountID int64)) {
	m.mu.Lock()
	m.onDisconnected = fn
	m.mu.Unlock()
}

// getOnDisconnected returns the disconnect callback.
func (m *Manager) getOnDisconnected() func(accountID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onDisconnected
}

// getOnConnected returns the connect callback.
func (m *Manager) getOnConnected() func(accountID int64, session *Session) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onConnected
}

type referencedMutex struct {
	mu   sync.Mutex
	refs int
}

// A reference covers the caller's entire use of the mutex, including waiting
// for it and temporarily unlocking it. Deleting locks when a connection closes
// would let a new caller race an existing waiter using a different mutex.
type refCountedLockRegistry[K comparable] struct {
	mu    sync.Mutex
	locks map[K]*referencedMutex
}

func (r *refCountedLockRegistry[K]) retain(key K) (*sync.Mutex, func()) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[K]*referencedMutex)
	}
	entry := r.locks[key]
	if entry == nil {
		entry = &referencedMutex{}
		r.locks[key] = entry
	}
	entry.refs++
	r.mu.Unlock()
	return &entry.mu, func() {
		r.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
}

func (m *Manager) keyLock(key string) (*sync.Mutex, func()) {
	return m.keyLocks.retain(key)
}

func (m *Manager) accountLock(accountID int64) (*sync.Mutex, func()) {
	return m.accountLocks.retain(accountID)
}

func (m *Manager) accountWaitSignal(accountID int64) <-chan struct{} {
	m.waitNotifyMu.Lock()
	defer m.waitNotifyMu.Unlock()
	if m.waitNotify == nil {
		m.waitNotify = make(map[int64]chan struct{})
	}
	ch, ok := m.waitNotify[accountID]
	if !ok {
		ch = make(chan struct{})
		m.waitNotify[accountID] = ch
	}
	return ch
}

// notifyAccountWaiters wakes all acquire calls waiting on this account: connection released/destroyed/created, in-flight request finished,
// or a dial placeholder returned. With no waiters it is a single map lookup.
func (m *Manager) notifyAccountWaiters(accountID int64) {
	if m == nil {
		return
	}
	m.waitNotifyMu.Lock()
	ch, ok := m.waitNotify[accountID]
	if ok {
		delete(m.waitNotify, accountID)
	}
	m.waitNotifyMu.Unlock()
	if ok {
		close(ch)
	}
}

// waitForAccountChange waits for an account pool change, backoff expiry or context cancellation, and returns the actual wait duration.
func waitForAccountChange(ctx context.Context, wake <-chan struct{}, backoff time.Duration) (time.Duration, error) {
	start := time.Now()
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return time.Since(start), ctx.Err()
	case <-wake:
	case <-timer.C:
	}
	return time.Since(start), nil
}

// AcquireConnection gets or creates a connection.
// Reuses only within the same logical session and when the connection is idle, so different sessions do not share one handshaken connection.
func (m *Manager) AcquireConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	kind := requestConnectionCapacityKind(ctx)
	key := m.poolKey(account.ID(), wsURL, sessionKey, requestProxyKey(ctx, account, proxyOverride))
	lock, releaseKeyLock := m.keyLock(key)
	defer releaseKeyLock()
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	wait := AcquireInitialBackoff
	var waited time.Duration
	var createLeaseFailures int
	var busyOverflowAttempted bool

	for {
		// Take the wakeup signal before checking the pool state: any release/destroy after the check will necessarily close this channel.
		wake := m.accountWaitSignal(account.ID())
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				// Send a ping probe to confirm the connection is really alive
				if m.probe(wc) {
					// The network probe does not hold the account lock. Other pool keys on the same account can probe in parallel;
					// during the probe the connection may be trimmed by account capacity, so it must be re-checked after taking the lock.
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if !exists || current != wc || !canReuseConnection(wc) {
						accountLock.Unlock()
						lock.Unlock()
						continue
					}
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey, kind)
					if leaseErr == nil {
						wc.account = account
						wc.Touch()
						m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
						accountLock.Unlock()
						lock.Unlock()
						return wc, pr, nil
					}
					m.discardAcquisitionFailure(wc, leaseErr)
					accountLock.Unlock()
					lock.Unlock()
					if isConnectionCapacityError(leaseErr) {
						return nil, nil, leaseErr
					}
					continue
				}
				// Probe failed; clean up the dead connection
				m.discardConnectionFor(wc, closeProbeFailed)
				lock.Unlock()
				continue
			}
			if wc.session != nil && wc.session.PendingCount() > 0 {
				lock.Unlock()
				// The connection is held by the previous request of the same session: poll with exponential backoff until it is idle,
				// return an error once the cumulative wait exceeds the limit, to avoid unbounded blocking and fixed-interval spinning that competes for the lock.
				// Over-age connections also end up here, waiting for in-flight requests to finish; after that, the next loop round rotates and rebuilds them.
				//
				// After a short wait (patience), it can overflow to a sibling slot on the same account (issue #413, off by default):
				// when the previous request streams output for a long time, concurrent requests in the same session no longer wait the full limit.
				// Tries only once; on failure (capacity full / dial failed) it falls back to waiting, and the worst case matches the disabled behavior.
				if !busyOverflowAttempted && busyOverflowEnabled() && waited >= busyOverflowPatience() && !isBusyOverflowSessionKey(sessionKey) {
					busyOverflowAttempted = true
					if owc, opr, ok := m.tryAcquireBusyOverflow(ctx, account, wsURL, sessionKey, headers, proxyOverride); ok {
						log.Printf("[WS] busy session overflowed to a sibling connection on the same account (account=%d, waited=%s)", account.ID(), waited.Round(time.Millisecond))
						return owc, opr, nil
					}
				}
				if maxWait := busyAcquireMaxWait(); waited >= maxWait {
					return nil, nil, fmt.Errorf("acquire websocket connection timed out after %s waiting for busy session", maxWait)
				}
				// Wakes immediately when an in-flight request ends (RemovePendingRequest) or the connection is destroyed; backoff is only the fallback.
				slept, waitErr := waitForAccountChange(ctx, wake, wait)
				if waitErr != nil {
					return nil, nil, waitErr
				}
				waited += slept
				if wait < AcquireMaxBackoff {
					wait *= 2
					if wait > AcquireMaxBackoff {
						wait = AcquireMaxBackoff
					}
				}
				continue
			}
			m.DiscardConnection(wc)
		}
		accountLock.Lock()
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: connectionLimit(kind), protectedKey: key, kind: kind})
		if capacity == nil {
			accountLock.Unlock()
			lock.Unlock()
			return nil, nil, m.connectionCapacityError(account.ID(), kind)
		}
		// The real connection is registered; the account lock is released during dialing so other sessions can keep acquiring connections.
		// Dialing a blank connection registers against the budget; chat reasoning does not use the idle reserve quota.
		accountLock.Unlock()

		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: sessionKey, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			return nil, nil, err
		}

		// Store the new connection and immediately reserve a pending request, to avoid a race from accounting only after return
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			createLeaseFailures++
			if createLeaseFailures >= maxCreateLeaseAttempts {
				return nil, nil, fmt.Errorf("reserve new websocket connection after %d attempts: %w", createLeaseFailures, leaseErr)
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}
			continue
		}
		lock.Unlock()

		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}

		return wc, pr, nil
	}
}

// tryAcquireBusyOverflow, after a busy session has waited longer than patience, tries to use a bounded
// overflow slot (<sessionKey>#ovf-N) on the same account to reuse an idle sibling connection or create one (issue #413).
// Single pass, best effort: if that slot is also busy, move to the next one; if account capacity is full or dialing/lease fails, give up, and the caller
// falls back to waiting for the original connection; a failed attempt is never worse than having overflow disabled.
// Sibling connections enter the pool normally and are reclaimed by the existing IdleTimeout/capacity trimming.
func (m *Manager) tryAcquireBusyOverflow(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseSessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, bool) {
	proxyURL := requestProxyKey(ctx, account, proxyOverride)
	kind := requestConnectionCapacityKind(ctx)
	accountLimit := connectionLimit(kind)
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	for i := 1; i <= BusyOverflowSlots; i++ {
		slotSession := fmt.Sprintf("%s%s%d", baseSessionKey, busyOverflowKeyInfix, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.hasLiveResponseBinding(wc) {
					lock.Unlock()
					continue
				}
				if m.probe(wc) {
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if exists && current == wc && canReuseConnection(wc) && !m.hasLiveResponseBinding(wc) {
						pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
						if leaseErr == nil {
							wc.account = account
							wc.Touch()
							m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
							accountLock.Unlock()
							lock.Unlock()
							return wc, pr, true
						}
						m.discardAcquisitionFailure(wc, leaseErr)
					}
					accountLock.Unlock()
					lock.Unlock()
					continue
				}
				m.discardConnectionFor(wc, closeProbeFailed)
			} else if wc.session != nil && wc.session.PendingCount() > 0 {
				// The sibling slot is also busy: move to the next slot
				lock.Unlock()
				continue
			} else {
				// Dead/over-age/expired connection: clear it to free the slot, and create a new one directly below
				m.discardConnectionFor(wc, connectionExpiryReason(wc))
			}
		}
		accountLock.Lock()
		if _, ok := m.connections.Load(key); ok {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: accountLimit, protectedKey: key, kind: kind})
		if capacity == nil {
			// Account connection capacity is full: do not take more connections for overflow; give up and fall back to waiting
			accountLock.Unlock()
			lock.Unlock()
			return nil, nil, false
		}
		accountLock.Unlock()
		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: slotSession, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			log.Printf("[WS] busy overflow failed to create a connection, falling back to waiting for the original connection (account=%d): %v", account.ID(), err)
			return nil, nil, false
		}
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			return nil, nil, false
		}
		lock.Unlock()
		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, true
	}
	return nil, nil, false
}

// StatelessConnectionSlots: for requests without an explicit session, under each (account, cacheKey) dimension,
// the number of persistent connection slots reused. Idle connections within a slot are reused directly, avoiding a new handshake per request —
// under sustained high RPM, per-request handshakes trigger upstream WS handshake rate limiting (bad handshake → 503).
const StatelessConnectionSlots = 8

// maxCreateLeaseAttempts bounds retries when a freshly completed handshake is
// already rejected by its permanent reader before the first request lease can
// be reserved (for example, an immediately queued peer Close frame).
const maxCreateLeaseAttempts = 3

// Give the permanent reader a small, bounded window to surface a Close/error
// already queued with the handshake before returning a newly reserved lease.
const newConnectionReadFailureGrace = 5 * time.Millisecond

// AcquireReusableConnection reuses or creates a connection within a fixed slot, and returns the session key actually used.
// The first pass only reuses existing idle connections; the second pass creates a persistent connection in an empty slot; when all slots are busy it falls back to
// a temporary connection under fallbackKey. Blank warm-ups share the account's blank budget; after chat finishes, connections are kept up to the idle cap.
func (m *Manager) AcquireReusableConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseKey string,
	fallbackKey string,
	slots int,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, string, error) {
	proxyURL := requestProxyKey(ctx, account, proxyOverride)
	kind := requestConnectionCapacityKind(ctx)
	accountLimit := connectionLimit(kind)
	if slots < 1 || slots > statelessConnectionSlots() {
		slots = statelessConnectionSlots()
	}
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	// First pass: reuse idle connections (clean up any that fail the probe or are already disconnected along the way, so the second pass can fill the slot)
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.hasLiveResponseBinding(wc) {
					lock.Unlock()
					continue
				}
				if m.probe(wc) {
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if !exists || current != wc || !canReuseConnection(wc) || m.hasLiveResponseBinding(wc) {
						accountLock.Unlock()
						lock.Unlock()
						continue
					}
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
					if leaseErr == nil {
						wc.account = account
						wc.Touch()
						m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
						accountLock.Unlock()
						lock.Unlock()
						return wc, pr, slotSession, nil
					}
					m.discardAcquisitionFailure(wc, leaseErr)
					accountLock.Unlock()
					lock.Unlock()
					continue
				}
				m.discardConnectionFor(wc, closeProbeFailed)
			} else if wc.session == nil || wc.session.PendingCount() == 0 {
				m.discardConnectionFor(wc, connectionExpiryReason(wc))
			}
		}
		lock.Unlock()
	}
	// Second pass: create persistent connections in empty slots
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if _, ok := m.connections.Load(key); ok {
			lock.Unlock()
			continue
		}
		accountLock.Lock()
		if _, ok := m.connections.Load(key); ok {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: accountLimit, protectedKey: key, kind: kind})
		if capacity == nil {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		accountLock.Unlock()
		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: slotSession, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			return nil, nil, "", err
		}
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, "", ctx.Err()
			}
			continue
		}
		lock.Unlock()
		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, slotSession, nil
	}
	// All slots busy: fall back to a one-off connection
	wc, pr, err := m.AcquireConnection(ctx, account, wsURL, fallbackKey, headers, proxyOverride)
	return wc, pr, fallbackKey, err
}

// addPendingAndBeginReadLease keeps the Session reservation and the pump lease
// atomic from an acquire caller's perspective. On failure it rolls the pending
// request back; the caller discards the unusable connection while holding its
// pool-key acquisition lock.
func (m *Manager) addPendingAndBeginReadLease(wc *WsConnection, sessionKey string, kind connectionCapacityKind) (*PendingRequest, error) {
	if wc == nil || wc.session == nil {
		return nil, fmt.Errorf("begin websocket read lease: connection has no session")
	}
	if err := m.ensureConnectionBudget(wc, kind); err != nil {
		return nil, err
	}
	pr := wc.session.AddPendingRequest(sessionKey)
	if err := wc.BeginReadLease(pr.RequestID); err != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		return nil, fmt.Errorf("reserve websocket connection: %w", err)
	}
	return pr, nil
}

func canReuseConnection(wc *WsConnection) bool {
	if wc == nil {
		return false
	}
	if !wc.IsConnected() || wc.IsExpired() || wc.IsOverAge() {
		return false
	}
	if wc.session == nil {
		return false
	}
	return wc.session.PendingCount() == 0 && wc.readPumpReusable()
}

// isRotatableOverAge reports whether a connection is over age and has no in-flight requests, so it can be safely rotated (destroyed and rebuilt).
// An over-age connection that still has in-flight requests is left alone: the 50-minute threshold leaves a 10-minute margin, so in-flight streams can still finish normally
// and it is rotated after they end, to avoid cutting off in-flight responses.
func isRotatableOverAge(wc *WsConnection) bool {
	if wc == nil || !wc.IsOverAge() {
		return false
	}
	return wc.session == nil || wc.session.PendingCount() == 0
}

// probeConnection sends a ping to check whether the connection is really alive.
func probeConnection(wc *WsConnection) bool {
	return probeConnectionWithTimeout(wc, defaultProbeTimeout)
}

// A connection with inbound activity (data frame / peer Ping / Pong reply) within probeRecencyWindow skips the
// ping-pong round-trip probe. Round-trip probes run serially under keyLock, and each reuse adds one upstream RTT,
// so hot reuse right after a request completes (the most common path) should not pay for that; recent inbound activity has already proven TCP is alive in both directions
// and the lease/queue cleanliness is checked separately by readPumpReusable. The window is set to the heartbeat interval:
// a half-open connection is at worst detected on the next probe after the window expires, or on a send failure retry.
const probeRecencyWindow = HeartbeatPingInterval

// probe calls the probe function (supports test replacement)
func (m *Manager) probe(wc *WsConnection) bool {
	m.mu.RLock()
	fn := m.probeFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(wc)
	}
	if !weakNetworkModeEnabled() && wc != nil && wc.IsConnected() && wc.recentInboundWithin(probeRecencyWindow) && wc.readPumpReusable() {
		return true
	}
	return probeConnection(wc)
}

// wsCompressionSeen records the permessage-deflate negotiation results this process has already reported
// (bit0 = negotiated successfully was seen, bit1 = not negotiated was seen). The dialer always offers compression, but whether negotiation succeeds
// had no visible signal before; the result depends on the upstream deployment and the outbound path (direct / Resin). Results are deduplicated by
// result, each kind reported once, so even on a mixed path the log is not flooded connection by connection.
var wsCompressionSeen atomic.Int32

// logCompressionNegotiation reports the permessage-deflate negotiation result of this handshake.
func logCompressionNegotiation(resp *http.Response, accountID int64) {
	if resp == nil {
		return
	}
	extensions := resp.Header.Get("Sec-Websocket-Extensions")
	bit := int32(2)
	if strings.Contains(strings.ToLower(extensions), "permessage-deflate") {
		bit = 1
	}
	for {
		seen := wsCompressionSeen.Load()
		if seen&bit != 0 {
			return
		}
		if wsCompressionSeen.CompareAndSwap(seen, seen|bit) {
			break
		}
	}
	if bit == 1 {
		log.Printf("[WS] upstream negotiated permessage-deflate, frame compression is active (account=%d, extensions=%q)", accountID, extensions)
	} else {
		log.Printf("[WS] upstream did not negotiate permessage-deflate, frames are sent in plaintext (account=%d)", accountID)
	}
}

// ReleaseConnection releases a connection (returns it to the pool).
func (m *Manager) ReleaseConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	if wc.session != nil {
		m.retainReleasedConnection(wc)
		return
	}
	wc.Touch()
}

// RemoveConnection removes a connection.
func (m *Manager) RemoveConnection(accountID int64, wsURL string, sessionKey string, proxyURL string) {
	key := m.poolKey(accountID, wsURL, sessionKey, proxyURL)
	if v, ok := m.connections.LoadAndDelete(key); ok {
		wc := v.(*WsConnection)
		wc.recordCloseCause(connectionCloseCause{source: "local", reason: closePoolRemoved, bound: m.hasLiveResponseBinding(wc)})
		m.removeResponseConnBindings(wc)
		wc.Close()
		if wc.session != nil {
			m.sessions.CompareAndDelete(key, wc.session)
		}
	}
	m.notifyAccountWaiters(accountID)
}

// DiscardConnection closes a bad connection and removes it from the connection pool.
// Used on upstream WS error paths (read error / close 1006/1009/1011 / broken pipe / unexpected EOF):
// it closes the underlying socket to clear lingering CLOSE_WAIT, and removes the connection from connections/sessions,
// so a bad connection returned by ReleaseConnection is not later misjudged as reusable by canReuseConnection.
// It uses CompareAndDelete to delete exactly this connection, so it does not wrongly delete a new connection under the same PoolKey that was already rebuilt.
func (m *Manager) DiscardConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	wc.recordCloseCause(connectionCloseCause{source: "local", reason: closeUnusable, bound: m.hasLiveResponseBinding(wc)})
	if wc.PoolKey != "" {
		m.connections.CompareAndDelete(wc.PoolKey, wc)
		if wc.session != nil {
			m.sessions.CompareAndDelete(wc.PoolKey, wc.session)
		}
	}
	m.removeResponseConnBindings(wc)
	if wc.session != nil {
		wc.session.StopHeartbeat()
		wc.session.SetConnected(false)
	}
	_ = wc.Close()
	if wc.session != nil {
		m.notifyAccountWaiters(wc.session.AccountID)
	}
}

// removeResponseConnBindings releases the strong references to an unusable
// connection. Keep its reader queue intact: an active consumer may still need
// the already received frames and terminal read error after an upstream close.
func (m *Manager) removeResponseConnBindings(wc *WsConnection) {
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	wc.responseBindingsDisabled = true
	for responseID, binding := range m.respConnBindings {
		if binding.conn == wc {
			delete(m.respConnBindings, responseID)
		}
	}
}

// responseConnBindingLiveLocked requires respConnMu. Pool identity prevents a
// late completion for a replaced socket from keeping the old socket alive.
func (m *Manager) responseConnBindingLiveLocked(wc *WsConnection) bool {
	if wc == nil || wc.responseBindingsDisabled || !wc.IsConnected() {
		return false
	}
	current, ok := m.connections.Load(wc.PoolKey)
	return ok && current == wc
}

func (m *Manager) evictResponseConnBindings(now time.Time) {
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	m.evictResponseConnBindingsLocked(now)
}

func (m *Manager) evictResponseConnBindingsLocked(now time.Time) {
	for responseID, binding := range m.respConnBindings {
		if !binding.expiresAt.After(now) || !m.responseConnBindingLiveLocked(binding.conn) {
			delete(m.respConnBindings, responseID)
		}
	}
}

// BindResponseConn records which connection produced a response_id (continuation affinity).
func (m *Manager) BindResponseConn(responseID string, wc *WsConnection, sessionKey string, accountID int64, apiKey string) {
	m.bindResponseConn(responseID, responseConnBinding{conn: wc, sessionKey: sessionKey, accountID: accountID, apiKey: apiKey})
}

func (m *Manager) bindResponseConn(responseID string, binding responseConnBinding) {
	wc := binding.conn
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" || wc == nil {
		return
	}
	now := time.Now()
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	select {
	case <-m.stopCleanup:
		return
	default:
	}
	if !m.responseConnBindingLiveLocked(wc) {
		return
	}
	if wc.session != nil && connectionBudgetKind(wc) == capacityChat {
		wc.session.markUserContext()
	}
	if m.respConnBindings == nil {
		m.respConnBindings = make(map[string]responseConnBinding, 64)
	}
	// store:false only guarantees that the latest response on the connection can be continued, so old bindings must be invalidated.
	for id, binding := range m.respConnBindings {
		if binding.conn == wc {
			delete(m.respConnBindings, id)
		}
	}
	// Bounded guard: first clear one round of expired entries; if still over the limit, reject new additions (old bindings are more likely to be continued than new ones).
	if len(m.respConnBindings) >= responseConnBindingMaxEntries {
		m.evictResponseConnBindingsLocked(now)
	}
	if len(m.respConnBindings) < responseConnBindingMaxEntries {
		binding.expiresAt = now.Add(responseConnBindingTTL)
		m.respConnBindings[responseID] = binding
	}
}

// lookupResponseConn returns the connection bound to a response_id and its sessionKey in the pool.
// The binding has expired, the account/API key does not match, or the connection has been disconnected or rebuilt (the same key in the pool is no longer the same pointer)
// returns nil.
func (m *Manager) lookupResponseConn(responseID string, accountID int64, apiKey string) (*WsConnection, string) {
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" {
		return nil, ""
	}
	now := time.Now()
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	binding, ok := m.respConnBindings[responseID]
	if !ok {
		return nil, ""
	}
	if !binding.expiresAt.After(now) || !m.responseConnBindingLiveLocked(binding.conn) || binding.conn.IsExpired() || binding.conn.IsOverAge() {
		delete(m.respConnBindings, responseID)
		return nil, ""
	}
	if binding.accountID != accountID || binding.apiKey != apiKey {
		return nil, ""
	}
	return binding.conn, binding.sessionKey
}

// AcquirePreferredConnection tries to take exclusive ownership of the original connection bound to a response_id (continuation affinity).
// On success it returns (connection, pendingRequest, sessionKey in the pool); if the binding is invalid or the connection is busy it returns nil,
// this single attempt is kept as a compatibility entry point; production continuation waits with a bound through acquireContinuation.
func (m *Manager) AcquirePreferredConnection(responseID string, accountID int64, apiKey string) (*WsConnection, *PendingRequest, string) {
	return m.acquirePreferredConnection(websocketContinuation{responseID: responseID, accountID: accountID, apiKey: apiKey})
}

func (m *Manager) acquirePreferredConnection(input websocketContinuation) (*WsConnection, *PendingRequest, string) {
	accountID := input.accountID
	wc, sessionKey := m.lookupContinuationConn(input)
	if wc == nil {
		return nil, nil, ""
	}
	accountLock, releaseAccountLock := m.accountLock(accountID)
	defer releaseAccountLock()
	lock, releaseKeyLock := m.keyLock(wc.PoolKey)
	defer releaseKeyLock()
	lock.Lock()
	defer lock.Unlock()
	// Re-check after taking the pool-key lock: in the meantime it may have been taken by another request or destroyed.
	if v, exists := m.connections.Load(wc.PoolKey); !exists || v != wc {
		return nil, nil, ""
	}
	if !canReuseConnection(wc) {
		if wc.session == nil || wc.session.PendingCount() == 0 {
			m.DiscardConnection(wc)
		}
		return nil, nil, ""
	}
	if !m.probe(wc) {
		m.DiscardConnection(wc)
		return nil, nil, ""
	}
	// The probe may wait on the network, so it must not hold the account lock. Re-check after taking the account lock, to prevent
	// the connection being safely reclaimed by capacity trimming from another pool key during the probe.
	accountLock.Lock()
	defer accountLock.Unlock()
	current, _ := m.lookupContinuationConn(input)
	if current != wc || !canReuseConnection(wc) {
		return nil, nil, ""
	}
	pr, err := m.addPendingAndBeginReadLease(wc, sessionKey, capacityChat)
	if err != nil {
		m.discardAcquisitionFailure(wc, err)
		return nil, nil, ""
	}
	wc.Touch()
	if wc.account != nil {
		m.trimIdleAccountConnections(accountID, connectionLimit(connectionBudgetKind(wc)), wc)
	}
	return wc, pr, sessionKey
}

// poolKey generates the connection pool key.
func (m *Manager) poolKey(accountID int64, wsURL string, sessionKey string, proxyURL string) string {
	return fmt.Sprintf("%d|%s|%s|%s", accountID, wsURL, strings.TrimSpace(sessionKey), strings.TrimSpace(proxyURL))
}

// GetSession gets a session.
func (m *Manager) GetSession(accountID int64, wsURL string, sessionKey string, proxyURL string) (*Session, bool) {
	if v, ok := m.sessions.Load(m.poolKey(accountID, wsURL, sessionKey, proxyURL)); ok {
		return v.(*Session), true
	}
	return nil, false
}

// ConnectionCount returns the number of connections.
func (m *Manager) ConnectionCount() int {
	count := 0
	m.connections.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// SessionCount returns the number of sessions.
func (m *Manager) SessionCount() int {
	count := 0
	m.sessions.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// ReplaceConnection replaces a connection (used for reconnection).
func (m *Manager) ReplaceConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	// Remove the old connection first
	m.RemoveConnection(account.ID(), wsURL, sessionKey, requestProxyKey(ctx, account, proxyOverride))

	// Create the new connection
	return m.AcquireConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
}

// SendHeartbeat sends a heartbeat ping.
func (m *Manager) SendHeartbeat(wc *WsConnection) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() {
		return fmt.Errorf("connection is not connected")
	}

	deadline := time.Now().Add(10 * time.Second)
	err := wc.conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
	if err != nil {
		// A write-path failure does not mean the read path is dead: with in-flight requests, the connection is only removed from the pool to block new reuse; the socket is not closed,
		// and the session is left alone; a forced close would cut off all in-flight streams on the connection in the same second (issue #436). The socket's
		// final close is handled by the read path as a fallback (on a pump read error, onReadFailure → DiscardConnection,
		// or the upstream closes it when the 60-minute connection lifetime expires).
		if wc.session != nil && wc.session.PendingCount() > 0 {
			log.Printf("WebSocket ping failed (account %d), connection removed from the pool, in-flight requests left to the read path to decide: %v", wc.session.AccountID, err)
			m.removeConnectionFromPool(wc)
			return err
		}
		log.Printf("WebSocket ping failed (account %d): %v", wc.session.AccountID, err)
		m.DiscardConnection(wc)
		return err
	}
	return nil
}

// removeConnectionFromPool only removes the connection from the pool (preventing later reuse); it does not close the underlying socket,
// and does not stop in-flight requests. CompareAndDelete deletes exactly by connection pointer, so it does not wrongly delete a new connection under the same PoolKey that was already rebuilt.
func (m *Manager) removeConnectionFromPool(wc *WsConnection) {
	if wc == nil || wc.PoolKey == "" {
		return
	}
	m.connections.CompareAndDelete(wc.PoolKey, wc)
	if wc.session != nil {
		m.sessions.CompareAndDelete(wc.PoolKey, wc.session)
	}
	m.removeResponseConnBindings(wc)
}

// StartHeartbeat starts connection heartbeats.
func (m *Manager) StartHeartbeat(wc *WsConnection) {
	if wc == nil || wc.session == nil || !wc.IsConnected() || !wc.session.IsConnected() {
		return
	}
	wc.session.StartHeartbeat(func() error {
		return m.SendHeartbeat(wc)
	})
}

// Global manager instance
var globalManager *Manager
var managerOnce sync.Once

// GetManager returns the global manager instance.
func GetManager() *Manager {
	managerOnce.Do(func() {
		globalManager = NewManager()
	})
	return globalManager
}

// ShutdownManager shuts down the global manager.
func ShutdownManager() {
	if globalManager != nil {
		globalManager.Stop()
	}
}
