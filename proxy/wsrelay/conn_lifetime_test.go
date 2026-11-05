package wsrelay

import (
	"io"
	"testing"
	"time"
)

// overAge moves the connection's creation time back to before MaxConnLifetime (test helper).
func overAge(wc *WsConnection) {
	wc.createdAt = time.Now().Add(-MaxConnLifetime - time.Minute).UnixNano()
}

// TestIsOverAge verifies the connection age check: it returns true when the age limit is reached; createdAt of 0 (a literal construction)
// is treated as unknown age and does not count as reaching the limit.
func TestIsOverAge(t *testing.T) {
	wc := &WsConnection{}
	if wc.IsOverAge() {
		t.Fatal("zero createdAt must not be treated as over-age")
	}
	wc.createdAt = time.Now().UnixNano()
	if wc.IsOverAge() {
		t.Fatal("fresh connection must not be over-age")
	}
	overAge(wc)
	if !wc.IsOverAge() {
		t.Fatal("connection older than MaxConnLifetime must be over-age")
	}
}

// TestCanReuseConnectionRejectsOverAge verifies that a connection past its age limit cannot be reused, even when it is idle and connected:
// upstream counts a 60-minute lifetime from connection establishment; after the limit is hit, response.create always fails, but the Ping liveness probe still succeeds,
// so the reuse check must look at the age (issue #346).
func TestCanReuseConnectionRejectsOverAge(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	if !canReuseConnection(wc) {
		t.Fatal("setup failed: fresh idle connection should be reusable")
	}
	overAge(wc)
	if canReuseConnection(wc) {
		t.Fatal("over-age connection must not be reusable")
	}
}

// TestEvictExpiredRotatesIdleOverAgeConnection verifies that background cleanup actively rotates idle connections that have reached the age limit
// connections; a connection that has reached the limit but still has an in-flight request is kept and rotated after the request ends, without cutting off the in-flight stream.
func TestEvictExpiredRotatesIdleOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	idle := addConnectedConn(t, m, 1, "s1")
	overAge(idle)

	busy := addConnectedConn(t, m, 2, "s2")
	overAge(busy)
	pr := busy.session.AddPendingRequest("s2")
	t.Cleanup(func() { busy.session.RemovePendingRequest(pr.RequestID) })

	fresh := addConnectedConn(t, m, 3, "s3")

	m.evictExpired()

	if _, ok := m.connections.Load(idle.PoolKey); ok {
		t.Fatal("idle over-age connection must be evicted for rotation")
	}
	if idle.IsConnected() {
		t.Fatal("evicted over-age connection must be closed")
	}
	if _, ok := m.connections.Load(busy.PoolKey); !ok {
		t.Fatal("over-age connection with in-flight request must be kept until it drains")
	}
	if _, ok := m.connections.Load(fresh.PoolKey); !ok {
		t.Fatal("fresh connection must survive eviction")
	}
}

// TestPingIdleConnectionsSkipsOverAgeConnection verifies that connections past the age limit are no longer kept alive,
// and are rotated by the cleaner, avoiding hitting the upstream 60-minute lifetime limit.
func TestPingIdleConnectionsSkipsOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	var pingedKeys []string
	m.keepalivePingFunc = func(wc *WsConnection) error {
		pingedKeys = append(pingedKeys, wc.PoolKey)
		return nil
	}

	aged := addConnectedConn(t, m, 1, "s1")
	overAge(aged)
	fresh := addConnectedConn(t, m, 2, "s2")

	pinged, failed := m.PingIdleConnections()
	if pinged != 1 || failed != 0 {
		t.Fatalf("pinged=%d failed=%d, want pinged=1 failed=0", pinged, failed)
	}
	if len(pingedKeys) != 1 || pingedKeys[0] != fresh.PoolKey {
		t.Fatalf("pingedKeys=%v, want only fresh connection %q", pingedKeys, fresh.PoolKey)
	}
}

// TestLookupResponseConnRejectsOverAgeConnection verifies that continuation affinity does not route requests back
// to a connection past its age limit (which would necessarily receive websocket_connection_limit_reached).
func TestLookupResponseConnRejectsOverAgeConnection(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	m.BindResponseConn("resp_1", wc, "s1", 1, "key-1")

	if got, _ := m.lookupResponseConn("resp_1", 1, "key-1"); got != wc {
		t.Fatal("setup failed: fresh binding should resolve to the connection")
	}
	overAge(wc)
	if got, _ := m.lookupResponseConn("resp_1", 1, "key-1"); got != nil {
		t.Fatal("binding to an over-age connection must not resolve")
	}
}

// TestConnLimitErrorFrameMarksConnBroken verifies that an upstream websocket_connection_limit_reached
// error frame (connection-level limit) marks the connection as broken; a normal error frame (request-level) does not mark it, and the connection can still be returned for reuse.
func TestConnLimitErrorFrameMarksConnBroken(t *testing.T) {
	limitFrame := []byte(`{"type":"error","error":{"code":"websocket_connection_limit_reached","type":"invalid_request_error","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`)
	plainFrame := []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`)

	r := &WsResponse{}
	if err := r.handleMessage(limitFrame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r.mu.Lock()
	broken := r.connBroken
	r.mu.Unlock()
	if !broken {
		t.Fatal("connection-limit error frame must mark connection broken (issue #346)")
	}

	r2 := &WsResponse{}
	if err := r2.handleMessage(plainFrame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r2.mu.Lock()
	broken2 := r2.connBroken
	r2.mu.Unlock()
	if broken2 {
		t.Fatal("request-level error frame must not mark connection broken")
	}
}

// TestConnLimitErrorDiscardsConnectionOnClose verifies end to end the issue #346 containment path:
// after a connection-level limit error frame is received, Close() destroys the connection and removes it from the pool, instead of returning it for reuse
// and continuing to poison subsequent requests.
func TestConnLimitErrorDiscardsConnectionOnClose(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Stop)

	wc := addConnectedConn(t, m, 1, "s1")
	pr := wc.session.AddPendingRequest("s1")

	r := &WsResponse{conn: wc, pendingReq: pr, sessionID: "s1", manager: m}
	frame := []byte(`{"type":"error","error":{"code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes)."}}`)
	if err := r.handleMessage(frame, func([]byte) bool { return true }); err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}
	r.markStreamCompleted()

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := m.connections.Load(wc.PoolKey); ok {
		t.Fatal("connection that hit the upstream lifetime limit must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("connection that hit the upstream lifetime limit must be closed")
	}
}
