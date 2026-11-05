package auth

import (
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func newIdleAuthHTTP2ClientConn(t *testing.T) (*http.Server, net.Listener, *http2.ClientConn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := &http.Server{
		Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}), &http2.Server{}),
	}
	go func() { _ = server.Serve(listener) }()

	rawConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		_ = server.Close()
		_ = listener.Close()
		t.Fatalf("dial: %v", err)
	}

	transport := &http2.Transport{AllowHTTP: true}
	clientConn, err := transport.NewClientConn(rawConn)
	if err != nil {
		_ = rawConn.Close()
		_ = server.Close()
		_ = listener.Close()
		t.Fatalf("NewClientConn: %v", err)
	}
	return server, listener, clientConn
}

func TestEvictExpiredUTLSAuthClientClosesHealthyIdleConnection(t *testing.T) {
	const poolKey = "test://expired-auth-client"
	defer utlsAuthClientPool.Delete(poolKey)

	server, listener, conn := newIdleAuthHTTP2ClientConn(t)
	defer server.Close()
	defer listener.Close()
	defer conn.Close()

	transport := newUTLSAuthTransport("").(*utlsAuthRoundTripper)
	transport.mu.Lock()
	transport.connections["example.test"] = conn
	transport.mu.Unlock()

	if !conn.CanTakeNewRequest() {
		t.Fatal("precondition not met: a newly created HTTP/2 connection should be healthy and idle")
	}

	entry := &utlsAuthPoolEntry{
		client: &http.Client{Transport: transport},
	}
	entry.lastUsed.Store(time.Now().Add(-utlsAuthClientPoolTTL - time.Minute).UnixNano())
	utlsAuthClientPool.Store(poolKey, entry)

	evictExpiredUTLSAuthClients()

	if _, ok := utlsAuthClientPool.Load(poolKey); ok {
		t.Fatal("expired auth client is still in the connection pool")
	}
	if !conn.State().Closed {
		t.Fatal("healthy idle HTTP/2 connection was not closed: socket, readLoop goroutine and buffers stay resident")
	}
}

func TestUTLSAuthCloseIdleConnectionsKeepsBusyConnection(t *testing.T) {
	server, listener, conn := newIdleAuthHTTP2ClientConn(t)
	defer server.Close()
	defer listener.Close()
	defer conn.Close()

	if !conn.ReserveNewRequest() {
		t.Fatal("ReserveNewRequest failed")
	}

	transport := newUTLSAuthTransport("").(*utlsAuthRoundTripper)
	transport.mu.Lock()
	transport.connections["example.test"] = conn
	transport.mu.Unlock()

	transport.CloseIdleConnections()

	transport.mu.Lock()
	_, exists := transport.connections["example.test"]
	transport.mu.Unlock()
	if !exists {
		t.Fatal("in-flight HTTP/2 connection was removed as if idle")
	}
	if conn.State().Closed {
		t.Fatal("in-flight HTTP/2 connection was closed")
	}
}
