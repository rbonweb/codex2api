package wsrelay

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// TestPendingRequestCloseIdempotent tests idempotency of PendingRequest.Close
func TestPendingRequestCloseIdempotent(t *testing.T) {
	pr := NewPendingRequest("test-session")

	// The first close should succeed
	pr.Close()

	// Verify the closed flag is set
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()

	if !closed {
		t.Error("expected closed flag to be true after first Close()")
	}

	// The second close must not panic
	pr.Close()

	// The third close must not panic either
	pr.Close()
}

// TestPendingRequestConcurrentClose tests the safety of concurrent Close
func TestPendingRequestConcurrentClose(t *testing.T) {
	pr := NewPendingRequest("test-session")

	done := make(chan bool, 10)

	// Start several goroutines that Close at the same time
	for i := 0; i < 10; i++ {
		go func() {
			pr.Close()
			done <- true
		}()
	}

	// Wait for all goroutines to finish
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent Close")
		}
	}

	// Verify the final state is closed
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()

	if !closed {
		t.Error("expected closed flag to be true after concurrent Close")
	}
}

// TestSessionAddAndRemovePendingRequest tests request management on a session
func TestSessionAddAndRemovePendingRequest(t *testing.T) {
	session := NewSession(12345, nil)

	// Add the request
	pr := session.AddPendingRequest("test-session")
	if pr == nil {
		t.Fatal("expected PendingRequest to be created")
	}

	// Get the request
	foundPr, ok := session.GetPendingRequest(pr.RequestID)
	if !ok {
		t.Fatal("expected to find PendingRequest")
	}
	if foundPr.RequestID != pr.RequestID {
		t.Errorf("expected RequestID %s, got %s", pr.RequestID, foundPr.RequestID)
	}

	// Remove the request
	session.RemovePendingRequest(pr.RequestID)

	// Verify it was removed
	_, ok = session.GetPendingRequest(pr.RequestID)
	if ok {
		t.Error("expected PendingRequest to be removed")
	}
}

// TestSessionExpiration tests session expiry checks
func TestSessionExpiration(t *testing.T) {
	session := NewSession(12345, nil)

	// A newly created session must not be expired
	if session.IsExpired() {
		t.Error("newly created session should not be expired")
	}

	// Manually set LastActiveAt to a long time ago
	session.Touch()
	session.mu.Lock()
	session.LastActiveAt = time.Now().Add(-IdleTimeout - time.Minute)
	session.mu.Unlock()

	// Must expire once beyond the current business idle window.
	if !session.IsExpired() {
		t.Error("session should be expired after IdleTimeout of inactivity")
	}
}

// TestSessionConnectedState tests session connection state
func TestSessionConnectedState(t *testing.T) {
	session := NewSession(12345, nil)

	// Initial state is not connected
	if session.IsConnected() {
		t.Error("newly created session should not be connected")
	}

	// Set to connected
	session.SetConnected(true)
	if !session.IsConnected() {
		t.Error("session should be connected after SetConnected(true)")
	}

	// Set to not connected
	session.SetConnected(false)
	if session.IsConnected() {
		t.Error("session should not be connected after SetConnected(false)")
	}
}

// TestSessionDeliverStreamChunkFullChannelNonBlocking verifies stream backpressure is observable and does not block.
func TestSessionDeliverStreamChunkFullChannelNonBlocking(t *testing.T) {
	session := NewSession(12345, nil)
	session.ID = "test-session"
	pr := session.AddPendingRequest("upstream-session")

	for i := 0; i < cap(pr.StreamChan); i++ {
		pr.StreamChan <- NewStreamChunkMessage(pr.RequestID, []byte("existing"))
	}

	var logBuf bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(originalOutput) })

	start := time.Now()
	delivered := session.DeliverStreamChunk(NewStreamChunkMessage(pr.RequestID, []byte("new")))
	elapsed := time.Since(start)

	if delivered {
		t.Fatal("expected DeliverStreamChunk to return false when StreamChan is full")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("DeliverStreamChunk blocked too long with full StreamChan: %v", elapsed)
	}
	if got := len(pr.StreamChan); got != cap(pr.StreamChan) {
		t.Fatalf("expected StreamChan to remain full, len=%d cap=%d", got, cap(pr.StreamChan))
	}

	logOutput := logBuf.String()
	for _, want := range []string{"stream channel full", "account=12345", "session=test-session", "requestID=" + pr.RequestID, "capacity=64", "length=64"} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("expected log output to contain %q, got %q", want, logOutput)
		}
	}
}
