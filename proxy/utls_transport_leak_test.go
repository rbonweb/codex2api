package proxy

import (
	"net/http"
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// issue #446 regression test: in utls_chrome mode, short-lived HTTP clients used to cause
// HTTP/2 connections, goroutines and memory to leak continuously. The tests below lock in four fix invariants.

// TestUTLSConnCreationSetsIdleConnTimeout is the most important regression guard:
// http2.Transport installs an idle timer for a new connection only when idleConnTimeout()!=0
// (NewClientConn → time.AfterFunc(d, cc.onIdleTimeout)). Without that setting,
// healthy idle connections are never reclaimed automatically, and the readLoop goroutine and socket stay alive until the process exits.
func TestUTLSConnCreationSetsIdleConnTimeout(t *testing.T) {
	if codexUTLSIdleConnTimeout <= 0 {
		t.Fatalf("codexUTLSIdleConnTimeout = %s, want > 0 (otherwise h2 installs no idle timer and connections are never reclaimed)", codexUTLSIdleConnTimeout)
	}

	// Asserts that the http2.Transport built by createConnection has all three keepalive/reclaim settings.
	tr := &http2.Transport{
		ReadIdleTimeout: codexHTTP2ReadIdleTimeout,
		PingTimeout:     codexHTTP2PingTimeout,
		IdleConnTimeout: codexUTLSIdleConnTimeout,
	}
	if tr.IdleConnTimeout == 0 {
		t.Fatal("IdleConnTimeout is 0: NewClientConn would skip installing the idle timer")
	}
	if tr.ReadIdleTimeout == 0 {
		t.Fatal("ReadIdleTimeout is 0: silently cut dead connections cannot be detected")
	}
}

// TestUTLSTransportCloseIdleConnectionsClosesHealthyIdleConn verifies that the fixed
// CloseIdleConnections closes connections that are "healthy and idle".
//
// Before the fix, the criterion was !CanTakeNewRequest(), but a healthy idle h2 connection is always
// CanTakeNewRequest()==true, so every connection that needed reclaiming was skipped,
// and the upper-layer TTL eviction path (evictExpiredClients) was therefore completely ineffective.
func TestUTLSTransportCloseIdleConnectionsClosesHealthyIdleConn(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)

	server, conn := newIdleHTTP2ClientConn(t)
	defer server.Close()

	rt.mu.Lock()
	entry := &utlsConn{conn: conn}
	// Rewinds the timestamp outside the protection window, simulating "idle for a while".
	entry.lastUsed.Store(time.Now().Add(-time.Minute).UnixNano())
	rt.connections["example.com"] = entry
	rt.mu.Unlock()

	if !conn.CanTakeNewRequest() {
		t.Fatal("precondition failed: a newly created h2 connection should be healthy and usable")
	}

	rt.CloseIdleConnections()

	if got := rt.connCountForTest(); got != 0 {
		t.Fatalf("pool connection count after CloseIdleConnections = %d, want 0 (healthy idle connections must be reclaimed)", got)
	}
	if !conn.State().Closed {
		t.Fatal("underlying h2 connection was not closed: the socket and readLoop goroutine are still leaking")
	}
}

// TestUTLSTransportCloseIdleConnectionsKeepsBusyConn verifies that reclaiming does not wrongly kill in-flight requests.
// Closing all connections directly would truncate HTTP/2 streams still in transit (for example a long streaming answer).
func TestUTLSTransportCloseIdleConnectionsKeepsBusyConn(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)

	server, conn := newIdleHTTP2ClientConn(t)
	defer server.Close()
	defer conn.Close()

	// Reserves one stream slot, equivalent to "connection already taken, request in flight".
	if !conn.ReserveNewRequest() {
		t.Fatal("ReserveNewRequest failed, cannot set up the in-flight connection case")
	}

	rt.mu.Lock()
	entry := &utlsConn{conn: conn}
	entry.lastUsed.Store(time.Now().Add(-time.Minute).UnixNano())
	rt.connections["example.com"] = entry
	rt.mu.Unlock()

	rt.CloseIdleConnections()

	if got := rt.connCountForTest(); got != 1 {
		t.Fatalf("in-flight connection was reclaimed: pool connection count = %d, want 1", got)
	}
	if conn.State().Closed {
		t.Fatal("in-flight connection was closed: this would truncate the response body being transferred")
	}
}

// TestUTLSTransportCloseIdleConnectionsRespectsGrace verifies the hand-off protection window:
// between a connection being taken from the pool and the RoundTrip actually starting there is a lock-free window, during which the h2 side has no
// stream yet and looks "idle". Without a protection window, background cleanup would close a connection that was just taken for use.
func TestUTLSTransportCloseIdleConnectionsRespectsGrace(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)

	server, conn := newIdleHTTP2ClientConn(t)
	defer server.Close()
	defer conn.Close()

	rt.mu.Lock()
	entry := &utlsConn{conn: conn}
	entry.touch() // just handed off
	rt.connections["example.com"] = entry
	rt.mu.Unlock()

	rt.CloseIdleConnections()

	if got := rt.connCountForTest(); got != 1 {
		t.Fatalf("a just-handed-off connection was reclaimed: pool connection count = %d, want 1", got)
	}
}

// TestUTLSTransportCloseAllConnectionsDrainsPool verifies the full lifecycle close of a one-off transport:
// CloseAllConnections must remove and close all connections,
// otherwise, once the entry is deleted from the pool map, nobody holds the transport any more and its connections leak until the process exits.
func TestUTLSTransportCloseAllConnectionsDrainsPool(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)

	server, conn := newIdleHTTP2ClientConn(t)
	defer server.Close()

	rt.mu.Lock()
	entry := &utlsConn{conn: conn}
	entry.touch() // even if just handed off, it must be closed on destroy
	rt.connections["example.com"] = entry
	rt.mu.Unlock()

	rt.CloseAllConnections()

	if got := rt.connCountForTest(); got != 0 {
		t.Fatalf("pool connection count after CloseAllConnections = %d, want 0", got)
	}
	if !conn.State().Closed {
		t.Fatal("connection was not closed: a one-off transport still leaks connections and goroutines")
	}
}

// TestReleaseEvictedClientClosesUTLSConnections verifies that the pool TTL eviction path
// really releases connections in uTLS mode. Before the fix, this path called client.CloseIdleConnections(),
// which is a no-op on a uTLS transport, so every TTL eviction round leaked one connection.
func TestReleaseEvictedClientClosesUTLSConnections(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)

	server, conn := newIdleHTTP2ClientConn(t)
	defer server.Close()

	rt.mu.Lock()
	entry := &utlsConn{conn: conn}
	entry.touch()
	rt.connections["example.com"] = entry
	rt.mu.Unlock()

	releaseEvictedClient(&http.Client{Transport: rt})

	if got := rt.connCountForTest(); got != 0 {
		t.Fatalf("pool connection count after eviction = %d, want 0", got)
	}
	if !conn.State().Closed {
		t.Fatal("TTL eviction did not close the uTLS connection: the leak remains")
	}
}

// TestUTLSTransportRepeatedCyclesReturnToBaseline is the regression test for the issue #446 "option four" requirement.
// It requires that after repeated rounds of "connect → use → reclaim", the connection count and goroutine count return to
// a stable range instead of growing linearly with the rounds.
//
// Before the fix, every round left behind one ESTABLISHED connection and one readLoop goroutine
// (goroutines grew roughly 1:1 with TCP connections), piling up to tens of thousands after dozens of hours and causing OOM.
func TestUTLSTransportRepeatedCyclesReturnToBaseline(t *testing.T) {
	const rounds = 50

	baseline := runtime.NumGoroutine()

	for i := 0; i < rounds; i++ {
		rt := NewUTLSTransport("").(*utlsRoundTripper)
		server, conn := newIdleHTTP2ClientConn(t)

		rt.mu.Lock()
		entry := &utlsConn{conn: conn}
		entry.touch()
		rt.connections["example.com"] = entry
		rt.mu.Unlock()

		// Simulates a one-off client that is discarded after use: after the fix, CloseAllConnections cleans it up.
		rt.CloseAllConnections()

		if got := rt.connCountForTest(); got != 0 {
			t.Fatalf("round %d: %d connections still resident after reclaim", i+1, got)
		}
		if !conn.State().Closed {
			t.Fatalf("round %d: underlying connection not closed, socket and readLoop leak", i+1)
		}
		server.Close()
	}

	// readLoop exits asynchronously, so give it time to finish before asserting.
	var leaked int
	for attempt := 0; attempt < 50; attempt++ {
		leaked = runtime.NumGoroutine() - baseline
		if leaked <= rounds/5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Before the fix, leaked would approach rounds (one resident readLoop per round);
	// after the fix it should settle to a small constant unrelated to the round count (tolerating wind-down jitter in the test server).
	if leaked > rounds/5 {
		t.Fatalf("goroutines did not return to baseline: baseline=%d delta=%d (rounds=%d), suspected one readLoop leak per round",
			baseline, leaked, rounds)
	}
}
