package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

// ==================== utls RoundTripper (Chrome fingerprint + HTTP/2) ====================
//
// 设计要点：
//   - 使用 HelloChrome_Auto 模拟 Chrome 浏览器的 TLS 指纹
//   - 支持 HTTP/2 协议（与 OpenAI API 兼容）
//   - 连接池 + pending 管理：防止同一 host 重复创建连接
//   - 代理支持：HTTP(S) 和 SOCKS5

// Connection lifecycle parameters (issue #446).
//
// This transport manages its own HTTP/2 connection pool and does not go through the standard library *http.Transport, so the standard library's
// IdleConnTimeout has no effect at all; it must be set explicitly on the http2.Transport, otherwise
// NewClientConn will not install an idle timer (it is skipped directly when idleConnTimeout()==0),
// and the connection's readLoop goroutine will hold the socket and buffers forever, neither closed nor garbage-collectable.
const (
	// codexUTLSIdleConnTimeout is the idle lifetime cap for self-managed HTTP/2 connections,
	// aligned with the standard transport's IdleConnTimeout. When it expires, h2's own idle timer
	// calls closeIfIdle to reclaim, the final fallback for all reclaim paths.
	codexUTLSIdleConnTimeout = 90 * time.Second

	// utlsCloseIdleGrace is the CloseIdleConnections protection window: from the moment a connection is taken from the pool
	// until RoundTrip actually starts, there is a lock-free window in which the h2 side has no stream at all and looks
	// "idle". The last hand-off time covers this window, so a connection just taken for use is not closed.
	utlsCloseIdleGrace = 5 * time.Second
)

// utlsShutdownTimeout returns the upper limit for waiting on in-flight streams to finish during a graceful shutdown.
//
// It can be changed in the system settings (utls_shutdown_timeout_minutes, default 30 minutes): a streaming answer
// in one turn can exceed 10 minutes (issue #287), so the default leaves ample margin; only after the timeout is it forcibly closed,
// so that a stream that hangs abnormally cannot keep the connection alive forever.
func utlsShutdownTimeout() time.Duration {
	return CurrentRuntimeSettings().UTLSShutdownTimeout()
}

// utlsConn wraps one self-managed HTTP/2 connection and records the last time it was "handed to a request".
// h2's State() only reflects the current stream count and cannot tell "just taken, no request sent yet" apart from
// "fully idle", so a timestamp is recorded in addition to decide idleness.
type utlsConn struct {
	conn     *http2.ClientConn
	lastUsed atomic.Int64 // UnixNano
}

func (c *utlsConn) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

// busy reports whether the connection has in-flight requests (including reserved and queued streams).
// A streaming response always counts toward StreamsActive until its body is closed, so a long answer is never mistaken for idle.
func (c *utlsConn) busy() bool {
	st := c.conn.State()
	return st.StreamsActive > 0 || st.StreamsReserved > 0 || st.StreamsPending > 0
}

// reclaimable reports whether the connection can be closed safely: it has no in-flight requests and the hand-off protection window has passed.
func (c *utlsConn) reclaimable(now time.Time, grace time.Duration) bool {
	if c.busy() {
		return false
	}
	return now.Sub(time.Unix(0, c.lastUsed.Load())) >= grace
}

// utlsRoundTripper implements the http.RoundTripper interface
// It uses utls to simulate the Chrome browser's TLS fingerprint to get past TLS fingerprint detection
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*utlsConn  // HTTP/2 connection pool, indexed by host
	pending     map[string]*sync.Cond // prevents duplicate connection creation
	dialer      xproxy.Dialer         // underlying dialer (supports proxies)
}

// utlsSessionCache shares a TLS session cache across all uTLS connections, so reconnects use TLS resumption.
// It must be shared at the instance level (not a new one each time), otherwise the cache never hits.
var utlsSessionCache = utls.NewLRUClientSessionCache(256)

// NewUTLSTransport creates a RoundTripper that uses the Chrome TLS fingerprint
// Supports HTTP(S) and SOCKS5 proxies
func NewUTLSTransport(proxyURL string) http.RoundTripper {
	var dialer xproxy.Dialer = xproxy.Direct

	if proxyURL != "" {
		d, err := buildProxyDialer(proxyURL)
		if err != nil {
			log.Printf("[UTLS] proxy configuration failed, falling back to a direct connection: proxy=%s err_type=%T", security.MaskURLCredentials(proxyURL), err)
			dialer = xproxy.Direct
		} else {
			dialer = d
		}
	}

	return &utlsRoundTripper{
		connections: make(map[string]*utlsConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

// NewUTLSHttpClient creates an HTTP client that uses the Chrome TLS fingerprint
func NewUTLSHttpClient(proxyURL string) *http.Client {
	return &http.Client{
		Transport: NewUTLSTransport(proxyURL),
		Timeout:   0, // no global timeout; the request context controls it
	}
}

// buildProxyDialer creates a dialer from the proxy URL
func buildProxyDialer(proxyURL string) (xproxy.Dialer, error) {
	u, err := security.ParseProxyURL(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse proxy URL: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return buildHTTPProxyDialer(u)
	case "socks5", "socks5h":
		return buildSOCKS5Dialer(u)
	default:
		return nil, fmt.Errorf("unsupported proxy protocol: %s", u.Scheme)
	}
}

// httpConnectDialer is a dialer that establishes a tunnel with the HTTP CONNECT method
type httpConnectDialer struct {
	proxyAddr       string // proxy server address (host:port)
	proxyServerName string // TLS SNI/certificate verification name for an https proxy
	useTLS          bool   // an https:// proxy needs a TLS handshake with the proxy first
	authHeader      string // Proxy-Authorization header (optional)
}

// Dial connects to the target address through an HTTP CONNECT tunnel
func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	// 1. Open a TCP connection to the proxy server
	conn, err := net.DialTimeout("tcp", d.proxyAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to proxy server: %w", err)
	}

	// An https:// proxy must complete a TLS handshake first, otherwise Proxy-Authorization
	// would cross the network in plain text. Certificate verification stays on.
	if d.useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: d.proxyServerName,
			MinVersion: tls.VersionTLS12,
		})
		_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy TLS handshake failed: %w", err)
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}

	// 2. Send the CONNECT request to establish the tunnel
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.authHeader != "" {
		connectReq += fmt.Sprintf("Proxy-Authorization: %s\r\n", d.authHeader)
	}
	connectReq += "\r\n"

	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send CONNECT request: %w", err)
	}

	// 3. Read the proxy response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read proxy response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed (status %d)", resp.StatusCode)
	}

	// bufio.Reader may have buffered bytes past the response, so wrap it to make sure later reads do not lose them
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: br}, nil
	}
	return conn, nil
}

// bufferedConn wraps net.Conn and reads buffered data from bufio.Reader first
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// buildHTTPProxyDialer creates an HTTP CONNECT proxy dialer
func buildHTTPProxyDialer(u *url.URL) (xproxy.Dialer, error) {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}

	d := &httpConnectDialer{
		proxyAddr:       addr,
		proxyServerName: u.Hostname(),
		useTLS:          strings.EqualFold(u.Scheme, "https"),
	}

	// Handle proxy authentication
	if u.User != nil {
		username := u.User.Username()
		password, _ := u.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		d.authHeader = "Basic " + credentials
	}

	return d, nil
}

// buildSOCKS5Dialer creates a SOCKS5 proxy dialer
func buildSOCKS5Dialer(u *url.URL) (xproxy.Dialer, error) {
	var auth *xproxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &xproxy.Auth{
			User:     u.User.Username(),
			Password: password,
		}
	}

	return xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
}

// getOrCreateConnection gets or creates an HTTP/2 connection
// It uses sync.Cond to prevent duplicate connection creation for the same host
//
// Before returning, it touch()es the connection it hits: this timestamp is the basis for the CloseIdleConnections protection window,
// so a connection in the "taken but no stream sent yet" window is not closed as if it were idle.
func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*utlsConn, error) {
	t.mu.Lock()

	// Check whether a usable connection already exists
	if entry, ok := t.connections[host]; ok && entry.conn.CanTakeNewRequest() {
		entry.touch()
		t.mu.Unlock()
		return entry, nil
	}

	// Check whether another goroutine is creating a connection
	if cond, ok := t.pending[host]; ok {
		// Wait for the other goroutine to finish (retry in a loop to avoid spurious wakeups)
		for {
			cond.Wait()
			// Check again whether a connection is usable
			if entry, ok := t.connections[host]; ok && entry.conn.CanTakeNewRequest() {
				entry.touch()
				t.mu.Unlock()
				return entry, nil
			}
			// If pending has been removed, creation has finished (it may have failed); break out of the loop and create it ourselves
			if _, still := t.pending[host]; !still {
				break
			}
		}
	}

	// Mark this host as creating a connection
	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	// Create the connection outside the lock
	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	// Remove the pending mark and wake one waiter (Signal rather than Broadcast, to avoid a thundering herd)
	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	// The old connection is no longer usable (reaching here means CanTakeNewRequest()==false), but it may still have
	// in-flight streams (for example GOAWAY was received but old requests are unfinished). Wait for them with a graceful shutdown
	// before closing it; do not Close it directly, which would cut off an in-progress streaming answer.
	if oldEntry, ok := t.connections[host]; ok {
		shutdownUTLSConn(oldEntry.conn)
	}

	// Store the new connection
	entry := &utlsConn{conn: h2Conn}
	entry.touch()
	t.connections[host] = entry
	return entry, nil
}

// shutdownUTLSConn gracefully closes a connection asynchronously: it sends GOAWAY first and waits for in-flight streams to finish,
// and forcibly closes it after a timeout (or on error). Both paths guarantee that the underlying socket and readLoop goroutine
// are eventually released and do not leak as they did before the fix (issue #446).
func shutdownUTLSConn(conn *http2.ClientConn) {
	if conn == nil {
		return
	}
	timeout := utlsShutdownTimeout()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := conn.Shutdown(ctx); err != nil {
			// Force-close on timeout or failure: it is better to cut off a hung request than to leak the connection.
			_ = conn.Close()
		}
	}()
}

// createConnection creates a new HTTP/2 connection
// It uses utls's HelloChrome_Auto to simulate the Chrome browser's TLS fingerprint
func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	// 1. Open a TCP connection (through the proxy or directly)
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP connection failed: %w", err)
	}

	// 2. Configure TLS (shared session cache, so the handshake uses resumption to reduce reconnect cost)
	tlsConfig := &utls.Config{
		ServerName:         host,
		ClientSessionCache: utlsSessionCache,
	}

	// 3. Perform the utls handshake (Chrome fingerprint)
	tlsConn := utls.UClient(conn, tlsConfig, utls.HelloChrome_Auto)

	// Set the handshake timeout
	handshakeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}

	// 4. Create the HTTP/2 connection
	// Enable keepalive PING (ReadIdleTimeout/PingTimeout): the uTLS pool manages its own connections, and pooled connections
	// that a proxy/NAT silently cuts off cannot be detected, and requests hang until TCP retransmission times out. With it enabled, idle connections
	// actively send PING; a failed probe errors in the read loop, which makes the upper layer evict the connection from the pool and rebuild it.
	//
	// IdleConnTimeout must be set explicitly: NewClientConn installs the idle timer only when idleConnTimeout()!=0.
	// (closeIfIdle). Without it, connections are never reclaimed automatically; the readLoop goroutine and
	// socket stay resident, which is the root cause of the issue #446 leak of tens of thousands of connections and goroutines.
	tr := &http2.Transport{
		ReadIdleTimeout: codexHTTP2ReadIdleTimeout,
		PingTimeout:     codexHTTP2PingTimeout,
		IdleConnTimeout: codexUTLSIdleConnTimeout,
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("failed to create HTTP/2 connection: %w", err)
	}

	return h2Conn, nil
}

// RoundTrip implements the http.RoundTripper interface
func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	// Get the host name (without port) for use as the TLS ServerName
	hostname := req.URL.Hostname()

	entry, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := entry.conn.RoundTrip(req)
	if err != nil {
		// Connection failed: remove it from the cache and close it
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == entry {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		// Close the failed connection to avoid a resource leak. It has already been removed from the pool, but the same connection may still have
		// other concurrent streams (h2 multiplexing), so use a graceful close rather than a direct Close,
		// to avoid wrongly killing neighboring requests; it is still force-closed after the timeout.
		shutdownUTLSConn(entry.conn)
		return nil, err
	}

	// The response headers have arrived, but the body may still be streaming (the stream still counts toward StreamsActive);
	// refresh the timestamp so the idle check is based on the "last use".
	entry.touch()
	return resp, nil
}

// CloseIdleConnections closes all idle connections.
//
// The criterion is "no in-flight streams on the connection", not CanTakeNewRequest(): the latter is always
// true for a healthy idle connection, so the pre-fix !CanTakeNewRequest() condition skipped exactly the idle connections that needed reclaiming
// and left the upper-layer TTL eviction path completely ineffective (issue #446).
func (t *utlsRoundTripper) CloseIdleConnections() {
	t.closeIdleConnections(utlsCloseIdleGrace)
}

func (t *utlsRoundTripper) closeIdleConnections(grace time.Duration) {
	now := time.Now()

	// Remove under the lock first, then close outside it: Close triggers h2 internal locks and callbacks, and closing while holding the lock
	// risks deadlocking against RoundTrip.
	var reclaimed []*http2.ClientConn
	t.mu.Lock()
	for host, entry := range t.connections {
		if entry.reclaimable(now, grace) {
			delete(t.connections, host)
			reclaimed = append(reclaimed, entry.conn)
		}
	}
	t.mu.Unlock()

	for _, conn := range reclaimed {
		_ = conn.Close()
	}
}

// CloseAllConnections removes and closes all connections held by this transport.
//
// It is for the case where "this transport is no longer held" (a one-off Client that has been used up, or pool TTL eviction),
// so no connection is left without anyone to reclaim it. In-flight streams are closed gracefully (waiting for them to finish), without cutting off the
// response body being transferred.
//
// The transport stays usable after the call (later requests dial again): it deliberately does not "permanently poison" itself, otherwise it would conflict with
// pool eviction: a client that was just taken would fail immediately. Connections that a straggler request newly creates after slipping through
// are reclaimed by IdleConnTimeout as a fallback and do not pile up without limit.
func (t *utlsRoundTripper) CloseAllConnections() {
	t.mu.Lock()
	entries := make([]*utlsConn, 0, len(t.connections))
	for host, entry := range t.connections {
		entries = append(entries, entry)
		delete(t.connections, host)
	}
	t.mu.Unlock()

	for _, entry := range entries {
		if entry.busy() {
			shutdownUTLSConn(entry.conn)
			continue
		}
		_ = entry.conn.Close()
	}
}

// connCountForTest returns the current number of connections in the pool; used only for test assertions.
func (t *utlsRoundTripper) connCountForTest() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.connections)
}

// ==================== Wrapper for compatibility with existing code ====================

// uTLSHTTPClientWrapper wraps utlsRoundTripper to stay compatible with the existing http.Client interface
type uTLSHTTPClientWrapper struct {
	transport *utlsRoundTripper
}

// NewUTLSClient creates an HTTP client that uses the Chrome TLS fingerprint
// It returns the wrapped client, which supports CloseIdleConnections
func NewUTLSClient(proxyURL string) *uTLSHTTPClientWrapper {
	rt := NewUTLSTransport(proxyURL).(*utlsRoundTripper)
	return &uTLSHTTPClientWrapper{
		transport: rt,
	}
}

// Do executes an HTTP request
func (c *uTLSHTTPClientWrapper) Do(req *http.Request) (*http.Response, error) {
	return c.transport.RoundTrip(req)
}

// CloseIdleConnections closes idle connections
func (c *uTLSHTTPClientWrapper) CloseIdleConnections() {
	c.transport.CloseIdleConnections()
}
