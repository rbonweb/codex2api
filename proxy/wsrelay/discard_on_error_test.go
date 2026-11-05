package wsrelay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestDiscardConnectionClosesAndRemovesFromPool verifies that DiscardConnection closes the underlying connection
// and removes the connection and its session from the pool.
func TestDiscardConnectionClosesAndRemovesFromPool(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	manager.DiscardConnection(wc)

	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected discarded connection to be removed from pool")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected discarded session to be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("expected discarded connection to be closed (state != connected)")
	}
	if session.IsConnected() {
		t.Fatal("expected discarded session heartbeat to be stopped")
	}
}

// TestDiscardConnectionKeepsReplacedConnection verifies that DiscardConnection uses
// CompareAndDelete for exact deletion: when the same PoolKey in the pool has already been replaced by a new connection, discarding the old connection must not wrongly delete the new one.
func TestDiscardConnectionKeepsReplacedConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")

	oldSession := NewSession(1, manager)
	oldSession.SetConnected(true)
	oldConn := &WsConnection{session: oldSession, PoolKey: key}
	oldConn.SetState(StateConnected)

	// The pool has already replaced it with a new connection under the same PoolKey
	newSession := NewSession(1, manager)
	newSession.SetConnected(true)
	newConn := &WsConnection{session: newSession, PoolKey: key}
	newConn.SetState(StateConnected)
	newConn.Touch()
	manager.connections.Store(key, newConn)
	manager.sessions.Store(key, newSession)

	manager.DiscardConnection(oldConn)

	if v, ok := manager.connections.Load(key); !ok || v.(*WsConnection) != newConn {
		t.Fatal("DiscardConnection must not remove a replaced connection under the same key")
	}
	if v, ok := manager.sessions.Load(key); !ok || v.(*Session) != newSession {
		t.Fatal("DiscardConnection must not remove the replaced session under the same key")
	}
}

func TestStartHeartbeatSkipsRetiredConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	session := NewSession(1, manager)
	session.SetConnected(false)
	wc := &WsConnection{session: session}
	wc.SetState(StateDisconnected)

	manager.StartHeartbeat(wc)
	session.mu.Lock()
	timer := session.heartbeatTimer
	session.mu.Unlock()
	if timer != nil {
		t.Fatal("retired connection created a new heartbeat timer")
	}
}

// TestWsResponseCloseDiscardsBrokenConnection verifies that after the read stream marks connBroken, Close()
// destroys the broken connection and removes it from the pool, instead of returning it for reuse.
func TestWsResponseCloseDiscardsBrokenConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}
	wsResp.markConnBroken()

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("broken connection must be removed from pool")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("broken session must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("broken connection must be closed")
	}
	if session.PendingCount() != 0 {
		t.Fatalf("pending must be cleared, got %d", session.PendingCount())
	}
}

// TestWsResponseCloseReleasesHealthyConnection verifies that a response completed normally (the read reached the terminal boundary and was not marked
// connBroken) keeps the connection in the pool after close, still connected, so later requests can reuse it.
func TestWsResponseCloseReleasesHealthyConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}
	wsResp.markStreamCompleted()

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); !ok {
		t.Fatal("healthy connection should remain in pool for reuse")
	}
	if !wc.IsConnected() {
		t.Fatal("healthy connection should stay connected")
	}
	if session.PendingCount() != 0 {
		t.Fatalf("pending must be cleared, got %d", session.PendingCount())
	}
}

// TestWsResponseCloseDiscardsIncompleteStream verifies that when the read stream was not consumed to the terminal boundary (the downstream disconnected,
// ctx cancelled, or another early exit), Close() destroys the connection instead of returning it to the pool — the upstream may still push leftover frames on that connection
// and reuse would send the previous request's response to the next user (issue #308).
func TestWsResponseCloseDiscardsIncompleteStream(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, "wss://example.test/responses", "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: key}
	wc.SetState(StateConnected)
	wc.Touch()
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	// Neither markStreamCompleted nor markConnBroken was called: simulate the stream being abandoned midway
	wsResp := &WsResponse{conn: wc, pendingReq: pr, sessionID: "session-1", manager: manager}

	if err := wsResp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("incomplete-stream connection must be removed from pool")
	}
	if wc.IsConnected() {
		t.Fatal("incomplete-stream connection must be closed")
	}
}

// TestHandleMessageDownstreamWriteFailureMarksConnBroken reproduces the trigger path of issue #308:
// when the downstream write fails (the callback returns false, such as broken pipe or a client disconnect), handleMessage
// returns io.EOF so that ReadStream ends, but it must mark connBroken so that Close() destroys the connection.
func TestHandleMessageDownstreamWriteFailureMarksConnBroken(t *testing.T) {
	r := &WsResponse{}

	err := r.handleMessage([]byte(`{"type":"response.output_text.delta","delta":"stale"}`), func(data []byte) bool {
		return false // simulate pw.Write failure
	})
	if err != io.EOF {
		t.Fatalf("handleMessage = %v, want io.EOF", err)
	}

	r.mu.Lock()
	broken := r.connBroken
	r.mu.Unlock()
	if !broken {
		t.Fatal("downstream write failure must mark connection broken to prevent reuse (issue #308)")
	}
}

// TestReadStreamDownstreamWriteStopDiscardsConnection reproduces issue #308 end to end:
// the upstream keeps pushing frames, the downstream write fails midway, ReadStream returns normally (nil), but Close() must
// remove the connection from the pool and close the underlying socket; it must never stay in the pool to be reused by the next request.
func TestReadStreamDownstreamWriteStopDiscardsConnection(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		// Simulate multi-frame output of the previous response: the downstream disconnects after the first frame
		for range 3 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"stale"}`)); err != nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	// The downstream write fails on the first frame (simulating broken pipe / client disconnect)
	err = wsResp.ReadStream(func(data []byte) bool { return false })
	if err != nil {
		t.Fatalf("ReadStream: %v (downstream stop is treated as end of stream)", err)
	}

	if cerr := wsResp.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("downstream write stop left websocket connection in pool; it should be discarded (issue #308)")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("downstream write stop left websocket session in pool; it should be discarded (issue #308)")
	}
	if wc.IsConnected() {
		t.Fatal("connection with unconsumed upstream frames must be closed")
	}
}

// TestReadStreamDiscardsConnectionOnAbnormalClose verifies the issue #267 scenario end to end:
// after the upstream sends close 1009 (message too big), ReadStream returns a read error and marks the connection as broken,
// and Close() removes the connection from the pool and closes the underlying socket (avoiding reuse and CLOSE_WAIT buildup).
func TestReadStreamDiscardsConnectionOnAbnormalClose(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		// Simulate an abnormal upstream close: send close 1009 (message too big)
		deadline := time.Now().Add(time.Second)
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"),
			deadline,
		)
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "session-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	pr := session.AddPendingRequest("session-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	err = wsResp.ReadStream(func(data []byte) bool { return true })
	if err == nil {
		t.Fatal("expected read error from abnormal close 1009")
	}
	if !strings.Contains(err.Error(), "websocket read error") {
		t.Fatalf("err = %v, want wrapped websocket read error", err)
	}

	if cerr := wsResp.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected broken connection removed from pool after abnormal close")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected broken session removed from pool after abnormal close")
	}
	if wc.IsConnected() {
		t.Fatal("expected broken connection to be closed")
	}
}
