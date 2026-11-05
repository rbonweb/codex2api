package wsrelay

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ==================== Heartbeat configuration constants ====================

const (
	// Heartbeat interval: send a Ping every 30 seconds
	HeartbeatPingInterval = 30 * time.Second

	// Active-stream liveness recheck interval: when business-frame silence reaches 120 seconds, do not disconnect directly; first check
	// recent inbound activity, then send a Ping with a unique payload as needed and wait for the matching Pong.
	ReadLivenessCheckInterval = 120 * time.Second

	// Active-stream recent inbound window: covers two heartbeat periods, allowing an occasional Pong delay or loss.
	// Data frames, peer Pings, and Pong acknowledgements of our Pings all refresh the inbound time.
	ActiveReadRecentInboundWindow = 2 * HeartbeatPingInterval

	// Active-stream probe timeout: triggers only when there are no business frames and no inbound activity recently either.
	// Normal requests and long reasoning with heartbeat acknowledgements do not produce this extra round trip.
	ActiveReadProbeTimeout = 5 * time.Second

	// Active-stream business-frame silence limit: the liveness recheck proves the transport is alive, but cannot prove the upstream is still processing
	// this request; if the silence between two business frames in a single response turn exceeds this value, the request is closed as timed out, to prevent
	// a worker that hangs while heartbeats still get through from pinning the request and lease on the connection forever.
	// env CODEX_WS_MAX_TURN_SILENCE is adjustable ("0" disables it).
	ActiveReadMaxTurnSilence = 15 * time.Minute

	// Write timeout: 30 seconds
	WriteTimeout = 30 * time.Second

	// Business idle timeout: disconnect after 30 minutes without processing a chat request or response; heartbeats do not extend it.
	IdleTimeout = 30 * time.Minute

	// Maximum connection lifetime: the upstream chatgpt backend enforces a 60-minute lifetime limit on each Responses WS connection; past the limit,
	// response.create on that connection always returns
	// websocket_connection_limit_reached (and Ping probes still succeed, so probing cannot detect it).
	// Rotate and destroy proactively at 50 minutes while idle, to keep active sessions from hitting the limit (issue #346).
	MaxConnLifetime = 50 * time.Minute

	// Handshake timeout: 30 seconds
	HandshakeTimeout = 30 * time.Second

	// Poll backoff parameters for waiting until a connection reusing the same session becomes idle.
	AcquireInitialBackoff = 10 * time.Millisecond  // initial backoff
	AcquireMaxBackoff     = 200 * time.Millisecond // backoff cap
	AcquireMaxWait        = 30 * time.Second       // maximum cumulative wait; returns an error on timeout
)

// ==================== Pending request management ====================

// PendingRequest is a request waiting for a response
type PendingRequest struct {
	// Request ID
	RequestID string

	// Session ID
	SessionID string

	// Creation time
	CreatedAt time.Time

	// Response channel
	ResponseChan chan *Message

	// Streaming data channel
	StreamChan chan *Message

	// Context (used for cancellation)
	Ctx context.Context

	// Cancel function
	Cancel context.CancelFunc

	// Closed flag, prevents double close
	closed  bool
	closeMu sync.Mutex
}

// NewPendingRequest creates a new waiting request
func NewPendingRequest(sessionID string) *PendingRequest {
	// Ctx is only used for the cancellation broadcast on Close; the application-level cap on request duration is enforced by the
	// business-frame silence limit (ActiveReadMaxTurnSilence) on the read path. Historically the 2min WithTimeout here had no
	// consumer, yet it made the comment "Pending has a 2-minute backstop" circulate elsewhere, so it was changed to WithCancel.
	ctx, cancel := context.WithCancel(context.Background())
	return &PendingRequest{
		RequestID:    uuid.New().String(),
		SessionID:    sessionID,
		CreatedAt:    time.Now(),
		ResponseChan: make(chan *Message, 1),
		StreamChan:   make(chan *Message, 64), // streaming data buffer
		Ctx:          ctx,
		Cancel:       cancel,
	}
}

// Close closes the request and releases resources (idempotent)
func (pr *PendingRequest) Close() {
	pr.closeMu.Lock()
	defer pr.closeMu.Unlock()

	if pr.closed {
		return
	}
	pr.closed = true

	pr.Cancel()
	close(pr.ResponseChan)
	close(pr.StreamChan)
}

// ==================== Session management ====================

// Session is a WebSocket session
type Session struct {
	// Session ID
	ID string

	// Account ID
	AccountID int64

	// Creation time
	CreatedAt time.Time

	// Last active time
	LastActiveAt time.Time

	// Has once carried user chat; this stays set even after the continuation binding expires, so it must not revert to an empty pre-warm slot.
	userContext bool

	// Connection state
	Connected bool

	// Read-write lock protecting internal state
	mu sync.RWMutex

	// Pending request map (requestID -> *PendingRequest)
	pending sync.Map

	// Heartbeat timer
	heartbeatTimer *time.Timer

	// Connection close callback
	onClose func()

	// Reference to the session manager
	manager *Manager
}

// NewSession creates a new session
func NewSession(accountID int64, manager *Manager) *Session {
	return &Session{
		ID:           uuid.New().String(),
		AccountID:    accountID,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		Connected:    false,
		manager:      manager,
	}
}

// SetConnected sets the connection state
func (s *Session) SetConnected(connected bool) {
	s.mu.Lock()
	s.Connected = connected
	if connected {
		s.LastActiveAt = time.Now()
	}
	s.mu.Unlock()
}

// IsConnected checks whether connected
func (s *Session) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Connected
}

// AddPendingRequest adds a waiting request
func (s *Session) AddPendingRequest(sessionID string) *PendingRequest {
	pr := NewPendingRequest(sessionID)
	s.pending.Store(pr.RequestID, pr)
	return pr
}

// GetPendingRequest gets a waiting request
func (s *Session) GetPendingRequest(requestID string) (*PendingRequest, bool) {
	if v, ok := s.pending.Load(requestID); ok {
		return v.(*PendingRequest), true
	}
	return nil, false
}

// RemovePendingRequest removes a waiting request
func (s *Session) RemovePendingRequest(requestID string) {
	if v, ok := s.pending.LoadAndDelete(requestID); ok {
		pr := v.(*PendingRequest)
		pr.Close()
		// The in-flight request ends: wake same-session acquires waiting on this connection, rather than waiting for the backoff to expire.
		if s.manager != nil {
			s.manager.notifyAccountWaiters(s.AccountID)
		}
	}
}

// PendingCount returns the number of in-flight requests bound to this session.
func (s *Session) PendingCount() int {
	if s == nil {
		return 0
	}
	count := 0
	s.pending.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// DeliverResponse delivers the response to the waiting request
func (s *Session) DeliverResponse(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.ResponseChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// Channel is full or closed
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// DeliverStreamChunk delivers a streaming data chunk
func (s *Session) DeliverStreamChunk(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.StreamChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// Channel is full: drop the current streaming chunk and return false
			log.Printf("DeliverStreamChunk stream channel full: account=%d session=%s requestID=%s capacity=%d length=%d; dropping current chunk", s.AccountID, s.ID, msg.RequestID, cap(pr.StreamChan), len(pr.StreamChan))
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// StartHeartbeat starts the heartbeat (re-entry protected)
func (s *Session) StartHeartbeat(sendPing func() error) {
	s.mu.Lock()
	// Re-entry protection: if a timer already exists, return directly
	if s.heartbeatTimer != nil {
		s.mu.Unlock()
		return
	}
	s.heartbeatTimer = time.AfterFunc(HeartbeatPingInterval, func() {
		s.heartbeatTick(sendPing)
	})
	s.mu.Unlock()
}

// heartbeatTick is the single-fire logic of the heartbeat timer.
func (s *Session) heartbeatTick(sendPing func() error) {
	s.mu.RLock()
	connected := s.Connected
	s.mu.RUnlock()

	if !connected {
		return
	}

	// In weak-network mode, heartbeats do not keep idle connections alive indefinitely; when there is no in-flight request, the heartbeat stops,
	// so the short idle window rotates the connection out naturally. In-flight requests keep the original heartbeat protection.
	if weakNetworkModeEnabled() && s.PendingCount() == 0 {
		s.StopHeartbeat()
		return
	}

	// Send Ping
	if err := sendPing(); err != nil {
		// A failed Ping write only means the write path failed; the read path may still be delivering normally. With in-flight requests
		// it must not Close (that would truncate all pending requests of this session in the same second, issue #436); leave the decision
		// to the read path: a pump read error or a failed ReadMessage liveness recheck takes its own failure handling. The connection itself was already removed
		// from the pool by SendHeartbeat, so no new request will come in; the heartbeat chain stops here.
		if s.PendingCount() > 0 {
			s.StopHeartbeat()
			return
		}
		s.Close()
		return
	}

	// Check whether the timer still exists (it may have been cleared by StopHeartbeat)
	s.mu.Lock()
	timer := s.heartbeatTimer
	s.mu.Unlock()

	// Safely reset the timer
	if timer != nil {
		timer.Reset(HeartbeatPingInterval)
	}
}

// StopHeartbeat stops the heartbeat
func (s *Session) StopHeartbeat() {
	s.mu.Lock()
	if s.heartbeatTimer != nil {
		s.heartbeatTimer.Stop()
		s.heartbeatTimer = nil
	}
	s.mu.Unlock()
}

// Close closes the session
func (s *Session) Close() {
	s.StopHeartbeat()
	s.SetConnected(false)

	// Close all waiting requests
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		pr.Close()
		s.pending.Delete(key)
		return true
	})

	// Invoke the close callback
	if s.onClose != nil {
		s.onClose()
	}
}

// SetOnClose sets the close callback
func (s *Session) SetOnClose(fn func()) {
	s.mu.Lock()
	s.onClose = fn
	s.mu.Unlock()
}

// ClearPendingRequests clears all waiting requests
func (s *Session) ClearPendingRequests() {
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		// Send an error response
		errMsg := NewErrorMessage(pr.RequestID, 503, "session closed")
		select {
		case pr.ResponseChan <- errMsg:
		default:
		}
		pr.Close()
		s.pending.Delete(key)
		return true
	})
}
