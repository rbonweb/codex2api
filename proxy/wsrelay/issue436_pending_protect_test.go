package wsrelay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHeartbeatTickPingFailureKeepsPendingRequests verifies the issue #436 fix:
// when a heartbeat Ping write fails and the session still has in-flight requests, the session must not be Closed (that would cut off all pending
// requests within the same second), so only the heartbeat chain is stopped and the read path decides.
func TestHeartbeatTickPingFailureKeepsPendingRequests(t *testing.T) {
	s := NewSession(1, nil)
	s.SetConnected(true)
	pr := s.AddPendingRequest("sess-1")

	s.heartbeatTick(func() error { return errors.New("write: broken pipe") })

	if s.PendingCount() != 1 {
		t.Fatalf("pending count = %d, want 1 (ping failure must not kill in-flight requests)", s.PendingCount())
	}
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()
	if closed {
		t.Fatal("pending request was closed by heartbeat ping failure")
	}
	if !s.IsConnected() {
		t.Fatal("session was disconnected by heartbeat ping failure while requests are in flight")
	}
}

// TestHeartbeatTickPingFailureClosesIdleSession verifies that for an idle session (no in-flight requests)
// a heartbeat failure still follows the original Close semantics.
func TestHeartbeatTickPingFailureClosesIdleSession(t *testing.T) {
	s := NewSession(1, nil)
	s.SetConnected(true)

	s.heartbeatTick(func() error { return errors.New("write: broken pipe") })

	if s.IsConnected() {
		t.Fatal("idle session should be closed on heartbeat ping failure")
	}
}

// TestSendHeartbeatBusyConnectionNotClosed verifies the issue #436 fix:
// when a Ping fails on a connection with in-flight requests, SendHeartbeat only removes the connection from the pool (blocking new reuse),
// and does not close the underlying socket: a write-path failure does not mean the read path is dead, and the in-flight stream may still be delivering normally.
func TestSendHeartbeatBusyConnectionNotClosed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	// Close the underlying connection right after dialing: later WriteControl calls will always fail, simulating a write-path failure.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close()

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "sess-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	wc.SetState(StateConnected)
	session.AddPendingRequest("sess-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	if err := manager.SendHeartbeat(wc); err == nil {
		t.Fatal("expected ping failure on closed connection")
	}

	// The connection should be removed from the pool (blocking reuse)
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("busy connection with failed ping should be removed from pool")
	}
	// but in-flight requests must not be killed
	if session.PendingCount() != 1 {
		t.Fatalf("pending count = %d, want 1 (busy connection ping failure must not kill in-flight requests)", session.PendingCount())
	}
	if !session.IsConnected() {
		t.Fatal("session must stay connected for in-flight drain after busy-connection ping failure")
	}
}

// TestSendHeartbeatIdleConnectionDiscarded verifies that a Ping failure on an idle connection (no in-flight requests)
// still goes through DiscardConnection: close the socket and remove it from the pool, the same semantics as before the fix.
func TestSendHeartbeatIdleConnectionDiscarded(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close()

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "sess-idle", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	wc.SetState(StateConnected)
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	if err := manager.SendHeartbeat(wc); err == nil {
		t.Fatal("expected ping failure on closed connection")
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("idle connection with failed ping should be discarded from pool")
	}
	if session.IsConnected() {
		t.Fatal("idle session should be marked disconnected after discard")
	}
}

// TestEvictExpiredSkipsConnectionsWithPendingRequests verifies the issue #436 fix:
// evictExpired always skips connections and sessions with in-flight requests, even when lastUsed/LastActiveAt
// is past IdleTimeout (long thinking and lost pongs would otherwise make active objects look idle).
func TestEvictExpiredSkipsConnectionsWithPendingRequests(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(nil, session, "ws://example")
	wc.PoolKey = "key-busy"
	wc.SetState(StateConnected)
	wc.Touch()
	// Fake an over-age idle timestamp: lastUsed is earlier than IdleTimeout, and LastActiveAt is aged to match
	wc.lastUsed.Store(time.Now().Add(-2 * IdleTimeout).UnixNano())
	session.mu.Lock()
	session.LastActiveAt = time.Now().Add(-2 * IdleTimeout)
	session.mu.Unlock()
	pr := session.AddPendingRequest("sess-busy")

	manager.connections.Store("key-busy", wc)
	manager.sessions.Store("key-busy", session)

	manager.evictExpired()

	if _, ok := manager.connections.Load("key-busy"); !ok {
		t.Fatal("evictExpired removed a connection with in-flight requests")
	}
	if _, ok := manager.sessions.Load("key-busy"); !ok {
		t.Fatal("evictExpired removed a session with in-flight requests")
	}
	if !wc.IsConnected() {
		t.Fatal("evictExpired closed a connection with in-flight requests")
	}

	// After the in-flight request finishes, the same expired state should be cleaned up normally in the next round
	session.RemovePendingRequest(pr.RequestID)
	manager.evictExpired()

	if _, ok := manager.connections.Load("key-busy"); ok {
		t.Fatal("evictExpired should remove the idle-expired connection once pending drained")
	}
	if _, ok := manager.sessions.Load("key-busy"); ok {
		t.Fatal("evictExpired should remove the idle-expired session once pending drained")
	}
}
