package proxy

import (
	"net"
	"net/http"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// h2TestServer is a plaintext HTTP/2 (h2c) test server, used to build a real
// *http2.ClientConn. Connection lifecycle assertions must hit a real connection,
// because a mock cannot reproduce key behavior such as the readLoop, the idle timer or stream counting.
type h2TestServer struct {
	listener net.Listener
	server   *http.Server
}

func (s *h2TestServer) Close() {
	_ = s.server.Close()
	_ = s.listener.Close()
}

// newIdleHTTP2ClientConn starts a local h2c server and returns a connection that has completed the handshake
// and is currently idle (no streams at all).
//
// The caller is responsible for closing the connection (or hands it to the code under test to close and then asserts its state).
func newIdleHTTP2ClientConn(t *testing.T) (*h2TestServer, *http2.ClientConn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h2s := &http2.Server{}
	handler := h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), h2s)
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()

	server := &h2TestServer{listener: listener, server: srv}

	rawConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatalf("dial: %v", err)
	}

	// Same settings as createConnection, so the test covers the production path's parameters.
	tr := &http2.Transport{
		ReadIdleTimeout: codexHTTP2ReadIdleTimeout,
		PingTimeout:     codexHTTP2PingTimeout,
		IdleConnTimeout: codexUTLSIdleConnTimeout,
		AllowHTTP:       true,
	}
	clientConn, err := tr.NewClientConn(rawConn)
	if err != nil {
		_ = rawConn.Close()
		server.Close()
		t.Fatalf("NewClientConn: %v", err)
	}

	return server, clientConn
}
