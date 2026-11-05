package proxy

import (
	"net/http"
	"testing"
)

// TestEnableCodexHTTP2KeepAlive verifies that after enabling HTTP/2 keepalive on the standard transport,
// the underlying http2.Transport's ReadIdleTimeout/PingTimeout are set to the expected values,
// so that dead connections silently cut off are actively probed with PING, and requests do not hang until the TCP retransmission timeout.
func TestEnableCodexHTTP2KeepAlive(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	h2 := enableCodexHTTP2KeepAlive(transport)
	if h2 == nil {
		t.Fatal("enableCodexHTTP2KeepAlive returned nil http2.Transport")
	}
	if h2.ReadIdleTimeout != codexHTTP2ReadIdleTimeout {
		t.Fatalf("ReadIdleTimeout = %s, want %s", h2.ReadIdleTimeout, codexHTTP2ReadIdleTimeout)
	}
	if h2.PingTimeout != codexHTTP2PingTimeout {
		t.Fatalf("PingTimeout = %s, want %s", h2.PingTimeout, codexHTTP2PingTimeout)
	}
}

// TestNewCodexStandardTransportIsHTTP2 verifies that the transport built by newCodexStandardTransport
// is still a *http.Transport that can negotiate HTTP/2 (regression guard: after the keepalive config is wired in,
// the transport type and h2 capability are not broken).
func TestNewCodexStandardTransportIsHTTP2(t *testing.T) {
	rt := newCodexStandardTransport("")
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("expected ForceAttemptHTTP2 = true on standard codex transport")
	}
}
