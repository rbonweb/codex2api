package wsrelay

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPrepareWebsocketHeadersUsesConfiguredDefaultsAndBetaFeatures(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	exec := NewExecutor()
	cfg := &proxy.DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
		BetaFeatures:           "multi_agent",
	}
	ginHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", cfg, ginHeaders, nil, "")

	if got := headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != responsesWebsocketBetaHeader {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	if got := headers.Get("X-Codex-Beta-Features"); got != "multi_agent" {
		t.Fatalf("X-Codex-Beta-Features = %q", got)
	}
	if got := headers.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := headers.Get("Originator"); got != proxy.Originator {
		t.Fatalf("Originator = %q", got)
	}
	if got := headers.Get("Chatgpt-Account-Id"); got != "42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	// The handshake session headers now use the real client shape: build_websocket_headers sends session-id /
	// thread-id / x-client-request-id, not the underscore spelling, and there is no Conversation_id.
	if got := headers.Get("Session-Id"); got != "session-123" {
		t.Fatalf("Session-Id = %q", got)
	}
	if got := headers.Get("Thread-Id"); got != "session-123" {
		t.Fatalf("Thread-Id = %q, want same value as session", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty", got)
	}
}

// TestPrepareWebsocketHeadersUsesProjectAttestation verifies that the WS attestation is generated only by this project.
func TestPrepareWebsocketHeadersUsesProjectAttestation(t *testing.T) {
	exec := NewExecutor()
	acc := &auth.Account{DBID: 42, AccountID: "42", CustomHeaders: map[string]string{
		"X-Oai-Attestation": "v1.account-override",
	}}

	withToken := exec.prepareWebsocketHeaders(context.Background(), "token-123", acc, "42", "session-123", "api-key-1", nil, http.Header{
		"User-Agent":        {"codex-tui/0.164.0 (Windows 11; x86_64) unknown"},
		"X-Oai-Attestation": []string{"v1.real-devicecheck-token"},
	}, nil, "")
	if got := withToken.Get("X-Oai-Attestation"); !strings.HasPrefix(got, `{"v":1,"s":0,"t":"v1.`) {
		t.Fatalf("X-Oai-Attestation = %q, want project-generated envelope", got)
	}

	without := exec.prepareWebsocketHeaders(context.Background(), "token-123", acc, "42", "session-123", "api-key-1", nil, http.Header{
		"User-Agent":        {"codex-tui/0.164.0 (Linux Ubuntu 24.04; x86_64) unknown"},
		"X-Oai-Attestation": {"v1.real-devicecheck-token"},
	}, nil, "")
	if got := without.Get("X-Oai-Attestation"); got != "" {
		t.Fatalf("X-Oai-Attestation = %q, want empty for Linux", got)
	}
}

func TestPrepareWebsocketHeadersAppliesAccountCustomHeadersLast(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{
		DBID:      42,
		AccountID: "42",
		CustomHeaders: map[string]string{
			"Authorization":      "Bearer websocket-override",
			"Chatgpt-Account-Id": "acct-override",
			"X-Custom-Header":    "custom-value",
		},
	}

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, nil, "")

	if got := headers.Get("Authorization"); got != "Bearer websocket-override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Chatgpt-Account-Id"); got != "acct-override" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := headers.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
}

func TestPrepareWebsocketHeadersSendsRoutingHintFromBody(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{DBID: 42, AccountID: "42"}
	wsBody := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, wsBody, "")
	if got := headers.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-codex;tier=priority" {
		t.Fatalf("X-Codex-Routing-Hint = %q, want priority hint from final WS body", got)
	}

	// Not sent when there is no body.
	headers = exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, []byte(`{"model":"gpt-5.6-sol","service_tier":"ultrafast"}`), "")
	if got := headers.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-sol;tier=ultrafast" {
		t.Fatalf("Ultrafast routing hint = %q", got)
	}

	// Not sent when there is no body.
	headers = exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, nil, "")
	if got := headers.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("X-Codex-Routing-Hint = %q, want empty without body", got)
	}
}

func TestPrepareWebsocketHeadersSendsUserAgentByDefault(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "")
	exec := NewExecutor()
	ginHeaders := http.Header{
		"X-Codex-Turn-State":                    []string{"turn-state"},
		"X-Codex-Turn-Metadata":                 []string{"turn-metadata"},
		"X-Client-Request-Id":                   []string{"req-123"},
		"X-Responsesapi-Include-Timing-Metrics": []string{"true"},
	}

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, ginHeaders, nil, "")

	if got := headers.Get("User-Agent"); got != proxy.MinimalCodexCLIUserAgentForHeaders() {
		t.Fatalf("User-Agent = %q, want %q", got, proxy.MinimalCodexCLIUserAgentForHeaders())
	}
	if got := headers.Get("Version"); got != proxy.LatestCodexCLIVersionForHeaders() {
		t.Fatalf("Version = %q, want %q", got, proxy.LatestCodexCLIVersionForHeaders())
	}
	if got := headers.Get("OpenAI-Beta"); got != responsesWebsocketBetaHeader {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Responsesapi-Include-Timing-Metrics"} {
		if got := headers.Get(name); got != ginHeaders.Get(name) {
			t.Fatalf("%s = %q, want %q", name, got, ginHeaders.Get(name))
		}
	}
	if got := headers.Get("Session-Id"); got != "session-123" {
		t.Fatalf("Session-Id = %q", got)
	}
	if got := headers.Get("Thread-Id"); got != "session-123" {
		t.Fatalf("Thread-Id = %q, want same value as session", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty (the real handshake headers do not have this header)", got)
	}
}

func TestPrepareWebsocketHeadersCanOptOutOfUserAgent(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "false")
	exec := NewExecutor()

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, http.Header{}, nil, "")

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %q, want empty", got)
	}
	if values, ok := headers["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
		t.Fatalf("User-Agent header entry = %#v, want explicit empty value to suppress Go default", values)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
}

func TestPrepareWebsocketHeadersHonorsForcedGeneratedUserAgent(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	prev := proxy.CurrentRuntimeSettings()
	proxy.ApplyRuntimeSettings(proxy.RuntimeSettings{ClientCompatMode: proxy.ClientCompatModeForce})
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(prev) })
	exec := NewExecutor()
	account := &auth.Account{DBID: 43, AccountID: "42"}
	ginHeaders := http.Header{
		"User-Agent": []string{"codex_vscode/1.2.3"},
		"Originator": []string{"codex_vscode"},
		"Version":    []string{"1.2.3"},
	}

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "session-123", "api-key-1", nil, ginHeaders, nil, "")

	got := headers.Get("User-Agent")
	if got == ginHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved client UA %q in forced mode", got)
	}
	if got != proxy.ProfileForAccount(account.DBID).UserAgent {
		t.Fatalf("User-Agent = %q, want real account profile %q", got, proxy.ProfileForAccount(account.DBID).UserAgent)
	}
	if !strings.HasPrefix(got, "codex-tui/") || !strings.Contains(got, " (") {
		t.Fatalf("User-Agent = %q, want generated full codex-tui profile", got)
	}
	if version := headers.Get("Version"); version != proxy.LatestCodexCLIVersionForHeaders() {
		t.Fatalf("Version = %q, want %q", version, proxy.LatestCodexCLIVersionForHeaders())
	}
	if originator := headers.Get("Originator"); originator != proxy.Originator {
		t.Fatalf("Originator = %q, want %q", originator, proxy.Originator)
	}
}

func TestPrepareWebsocketBodyPreservesPreviousResponseID(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","previous_response_id":"resp_123","input":[{"role":"user","content":"continue"}]}`), "session-123")

	if prev := gjson.GetBytes(got, "previous_response_id").String(); prev != "resp_123" {
		t.Fatalf("previous_response_id = %q, want resp_123; body=%s", prev, got)
	}
	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "session-123" {
		t.Fatalf("prompt_cache_key = %q, want session-123; body=%s", cacheKey, got)
	}
	if typ := gjson.GetBytes(got, "type").String(); typ != "response.create" {
		t.Fatalf("type = %q, want response.create; body=%s", typ, got)
	}
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Fatalf("stream should be true; body=%s", got)
	}
}

func TestPrepareWebsocketBodyKeepsCacheKeyForStatelessSession(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","prompt_cache_key":"deterministic-key","input":[]}`), "stateless-abc123")

	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "deterministic-key" {
		t.Fatalf("prompt_cache_key = %q, want deterministic-key (stateless sessionID must not overwrite); body=%s", cacheKey, got)
	}
}

func TestPrepareWebsocketBodyStatelessSessionWithoutCacheKey(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","input":[]}`), "stateless-abc123")

	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want empty (stateless sessionID must not be injected); body=%s", cacheKey, got)
	}
}

func TestNormalizeWebsocketHandshakeResponse(t *testing.T) {
	t.Run("switching protocols is successful websocket handshake", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusSwitchingProtocols,
		})
		if failed {
			t.Fatal("failed = true, want false")
		}
		if statusCode != http.StatusOK {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusOK)
		}
	})

	t.Run("http 2xx is normalized for downstream handler", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusNoContent,
		})
		if failed {
			t.Fatal("failed = true, want false")
		}
		if statusCode != http.StatusOK {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusOK)
		}
	})

	t.Run("non success status remains a handshake failure", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusUnauthorized,
		})
		if !failed {
			t.Fatal("failed = false, want true")
		}
		if statusCode != http.StatusUnauthorized {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusUnauthorized)
		}
	})
}

func TestWebsocketResponseToHTTPClosesBodyOnContextCancel(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		time.Sleep(5 * time.Second)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	session := NewSession(1, nil)
	pr := session.AddPendingRequest("session-1")
	wc := NewWsConnection(conn, session, wsURL)
	manager := NewManager()
	defer manager.Stop()
	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	resp := websocketResponseToHTTP(ctx, wsResp, http.StatusOK, http.Header{})
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Body.Read returned nil error after context cancellation")
		}
		if err != context.Canceled && err != io.ErrClosedPipe && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("Body.Read error = %v, want context cancellation or closed pipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Body.Read stayed blocked after context cancellation")
	}
}

func newClosedTestWebsocketConn(t *testing.T) *websocket.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	handshakeDone := make(chan struct{})
	go func() {
		defer close(handshakeDone)
		defer serverConn.Close()
		req, err := http.ReadRequest(bufio.NewReader(serverConn))
		if err != nil {
			return
		}
		acceptHash := sha1.Sum([]byte(req.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(serverConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(acceptHash[:]))
	}()

	wsURL, err := url.Parse("ws://example.test/responses")
	if err != nil {
		t.Fatalf("parse websocket URL: %v", err)
	}
	conn, _, err := websocket.NewClient(clientConn, wsURL, nil, 1024, 1024)
	if err != nil {
		t.Fatalf("create test websocket client: %v", err)
	}
	<-handshakeDone
	return conn
}

func TestExecuteRequestViaWebsocketSendFailureRemovesEffectiveProxyConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	account := &auth.Account{
		DBID:        42,
		AccessToken: "token-123",
		ProxyURL:    "http://account-proxy.test:8080",
	}
	sessionID := "session-1"
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatalf("buildWebsocketURL: %v", err)
	}
	effectiveProxy := effectiveProxyURL(account, "")
	exec := NewExecutorWithManager(manager)
	headers := exec.prepareWebsocketHeaders(context.Background(), account.AccessToken, account, account.AccountID, sessionID, "", nil, http.Header{}, nil, "")
	key := manager.poolKey(account.ID(), wsURL, websocketClientPoolKey(sessionID, headers), effectiveProxy)
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{
		conn:    newClosedTestWebsocketConn(t),
		session: session,
		URL:     wsURL,
		PoolKey: key,
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = exec.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4","input":"hi"}`), sessionID, "", "", nil, http.Header{}, "")
	if err == nil {
		t.Fatal("expected final send failure")
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected failed connection keyed by effective account proxy to be removed")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected failed session keyed by effective account proxy to be removed")
	}
	if conn.IsConnected() {
		t.Fatal("expected failed connection to be closed")
	}
}

func TestSendRequestWritesResponseCreatePayloadDirectly(t *testing.T) {
	received := make(chan []byte, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read websocket message: %v", err)
			return
		}
		received <- payload
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	exec := NewExecutor()
	wc := NewWsConnection(conn, NewSession(1, nil), wsURL)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":"hi","stream":true}`)
	if err := exec.sendRequest(wc, body, "request-1"); err != nil {
		t.Fatalf("sendRequest: %v", err)
	}

	got := <-received
	if string(got) != string(body) {
		t.Fatalf("sent payload = %s, want %s", got, body)
	}
	if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.create" {
		t.Fatalf("sent type = %q, want response.create; payload=%s", eventType, got)
	}
	if gjson.GetBytes(got, "request_id").Exists() {
		t.Fatalf("payload should not contain internal request_id wrapper: %s", got)
	}
}

// TestResolveHandshakeSessionID verifies the value policy for the handshake headers Session_id/Conversation_id.
// These headers are frozen per connection and never updated when the connection is reused, so a reused stateless connection must never carry any
// single request's identity, otherwise the first request's session identity leaks to all later users who reuse that connection
// (cross-user context pollution, "user 2 context bleeding into user 1").
func TestResolveHandshakeSessionID(t *testing.T) {
	t.Run("explicit session keeps original behavior", func(t *testing.T) {
		got := resolveHandshakeSessionID("session-123", "route-key", []byte(`{"prompt_cache_key":"whatever"}`))
		if got != "session-123" {
			t.Fatalf("headerSessionID = %q, want session-123", got)
		}
	})

	t.Run("stateless isolated mode must not send any session identity", func(t *testing.T) {
		// Default isolation mode: the frame body has a random prompt_cache_key per request, and poolRouteKey is non-empty.
		// If the random key were frozen into the handshake headers, every later user of the reused connection would carry the first request's identity.
		got := resolveHandshakeSessionID("stateless-abc", "route-key", []byte(`{"prompt_cache_key":"per-request-random-uuid"}`))
		if got != "" {
			t.Fatalf("headerSessionID = %q, want empty (no connection-level session identity)", got)
		}
	})

	t.Run("stateless per-api-key mode keeps deterministic cache key", func(t *testing.T) {
		got := resolveHandshakeSessionID("stateless-abc", "", []byte(`{"prompt_cache_key":"deterministic-key"}`))
		if got != "deterministic-key" {
			t.Fatalf("headerSessionID = %q, want deterministic-key", got)
		}
	})

	t.Run("stateless without cache key falls back to stateless id", func(t *testing.T) {
		got := resolveHandshakeSessionID("stateless-abc", "", []byte(`{}`))
		if got != "stateless-abc" {
			t.Fatalf("headerSessionID = %q, want stateless-abc", got)
		}
	})
}

// TestPrepareWebsocketHeadersOmitsSessionHeadersWhenEmpty verifies that when headerSessionID is empty,
// the Session_id/Conversation_id handshake headers are not sent (stateless reused connections in isolation mode).
func TestPrepareWebsocketHeadersOmitsSessionHeadersWhenEmpty(t *testing.T) {
	exec := NewExecutor()

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "", "api-key-1", nil, http.Header{}, nil, "")

	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want unset", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want unset", got)
	}
}

func TestStatelessOneShotEnabled(t *testing.T) {
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "")
	if statelessOneShotEnabled() {
		t.Fatal("default must be false (slot reuse on)")
	}
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "1")
	if !statelessOneShotEnabled() {
		t.Fatal("CODEX_WS_STATELESS_ONESHOT=1 must disable slot reuse")
	}
}

func TestShouldRetryWebsocketSendError(t *testing.T) {
	messageTooBig := fmt.Errorf("send interrupted: %w", &websocket.CloseError{
		Code: websocket.CloseMessageTooBig,
		Text: "message too big",
	})
	if shouldRetryWebsocketSendError(messageTooBig) {
		t.Fatal("close 1009 must return to the handler for HTTP fallback without rebuilding WebSocket connections")
	}

	if !shouldRetryWebsocketSendError(errors.New("temporary write failure")) {
		t.Fatal("ordinary transport write failures should retain the bounded reconnect retry")
	}
	if !shouldRetryWebsocketSendError(fmt.Errorf("send interrupted: %w", &websocket.CloseError{
		Code: websocket.CloseInternalServerErr,
		Text: "temporary upstream failure",
	})) {
		t.Fatal("retryable peer closes other than 1009 should retain the bounded reconnect retry")
	}
	if shouldRetryWebsocketSendError(nil) {
		t.Fatal("nil is not a retryable send error")
	}
}

// TestPrepareWebsocketHeadersConvergesForwardedClientRequestID is the WS counterpart of the HTTP-side
// TestApplyCodexRequestHeadersConvergesForwardedClientRequestID test:
// The handshake headers have their own pass-through list and assembly order, so the same set of invariants must be locked down independently,
// otherwise the issue #536 leak would come back only on the WS path.
func TestPrepareWebsocketHeadersConvergesForwardedClientRequestID(t *testing.T) {
	const (
		clientUUID    = "01a00e75-8856-7542-89bf-35812620690f"
		installUUID   = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath = "/Users/kyx/code_project/codex2api"
		remoteURL     = "https://github.com/james-6-23/codex2api.git"
		commitHash    = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	)
	rawMetadata := `{"installation_id":"` + installUUID + `","session_id":"` + clientUUID +
		`","thread_id":"` + clientUUID + `","window_id":"` + clientUUID +
		`:0","request_kind":"turn","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"latest_git_commit_hash":"` + commitHash + `","has_changes":false}}}`

	// Replicate the handshake header set of the real codex-tui (see wss://chatgpt.com/backend-api/codex/responses).
	ginHeaders := http.Header{}
	ginHeaders.Set("X-Codex-Turn-Metadata", rawMetadata)
	ginHeaders.Set("Session-Id", clientUUID)
	ginHeaders.Set("Thread-Id", clientUUID)
	ginHeaders.Set("X-Client-Request-Id", clientUUID)
	ginHeaders.Set("X-Codex-Window-Id", clientUUID+":0")
	ginHeaders.Set("Originator", "codex-tui")

	account := &auth.Account{DBID: 42, AccountID: "42", CodexFingerprintMode: auth.CodexFingerprintModeSession}
	exec := NewExecutor()
	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", account, "42", "upstream-session-id", "api-key-1", nil, ginHeaders, nil, "")

	if got := headers.Get("X-Client-Request-Id"); got == clientUUID {
		t.Fatal("X-Client-Request-Id still carries the downstream thread id after convergence")
	} else if got == "" {
		t.Fatal("X-Client-Request-Id was dropped, want a converged value")
	}
	// The caller decides the handshake session key, and convergence defaults must not intervene (aligning requires explicitly enabling
	// CODEX_SESSION_HEADER_ALIGN_CONVERGED). The header names are switched to the real shape, and the value semantics stay unchanged.
	if got := headers.Get("Session-Id"); got != "upstream-session-id" {
		t.Fatalf("Session-Id = %q, want the caller value untouched", got)
	}
	// thread-id must equal the converged x-client-request-id, otherwise the two headers would disagree.
	if got, want := headers.Get("Thread-Id"), headers.Get("X-Client-Request-Id"); got != want {
		t.Fatalf("Thread-Id = %q, want equal to X-Client-Request-Id %q", got, want)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	// The downstream did not send an installation header, so the outbound request should not gain one out of nowhere.
	if got := headers.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	var dump strings.Builder
	for name, values := range headers {
		for _, value := range values {
			dump.WriteString(name + ": " + value + "\n")
		}
	}
	for _, leaked := range []string{clientUUID, installUUID, workspacePath, remoteURL, "james-6-23", commitHash} {
		if strings.Contains(dump.String(), leaked) {
			t.Fatalf("original identifier %q survived the websocket handshake headers:\n%s", leaked, dump.String())
		}
	}
}

func TestPrepareWebsocketHeadersGeneratedDesktopClientSendsMatchingOriginator(t *testing.T) {
	// issue #653: the WS handshake follows the same rule as the HTTP path, where Originator follows the generated client prefix when a UA is generated.
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	prev := proxy.CurrentRuntimeSettings()
	normalized, err := proxy.NormalizeCodexUserAgentConfigJSON(`{"client_name":"Codex Desktop","client_version":"0.153.3","os_name":"Windows","os_version":"10.0.26100","arch":"x86_64","terminal":"unknown"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	proxy.ApplyRuntimeSettings(proxy.RuntimeSettings{
		ClientCompatMode:     proxy.ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(prev) })

	exec := NewExecutor()
	ginHeaders := http.Header{
		"User-Agent": []string{"codex-tui/0.153.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.153.3)"},
		"Originator": []string{"codex-tui"},
	}

	headers := exec.prepareWebsocketHeaders(context.Background(), "token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, ginHeaders, nil, "")

	wantUA := "Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.930.51102)"
	if got := headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop to match generated User-Agent", got)
	}
	if got := headers.Get("Version"); got != "0.153.3" {
		t.Fatalf("Version = %q, want 0.153.3", got)
	}
}

func TestPrepareWebsocketHeadersGuardsForeignTurnState(t *testing.T) {
	affinityKey := "ws-turn-guard::api-key:1"
	minter := &auth.Account{DBID: 101, AccountID: "101"}
	other := &auth.Account{DBID: 202, AccountID: "202"}
	proxy.NoteCodexTurnStateProvenance(affinityKey, minter)
	t.Cleanup(func() { proxy.ClearCodexTurnStateProvenance(affinityKey) })

	ginHeaders := http.Header{}
	ginHeaders.Set("X-Codex-Turn-State", "blob-from-minter")
	exec := NewExecutor()
	out := exec.prepareWebsocketHeaders(context.Background(), "token", other, "202", "session-1", "api-key", nil, ginHeaders, nil, affinityKey)
	if got := out.Get("X-Codex-Turn-State"); got != "" {
		t.Fatalf("foreign-account turn-state forwarded on WS path: %q", got)
	}

	same := exec.prepareWebsocketHeaders(context.Background(), "token", minter, "101", "session-1", "api-key", nil, ginHeaders, nil, affinityKey)
	if got := same.Get("X-Codex-Turn-State"); got != "blob-from-minter" {
		t.Fatalf("same-account turn-state stripped on WS path: %q", got)
	}
}

func TestSingleMachineFingerprintWebsocketBodyAndHeaders(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "false")
	a := &auth.Account{DBID: 42, AccountID: "42", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow}
	h := http.Header{}
	h.Set("Session-Id", "root")
	h.Set("Thread-Id", "child")
	h.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","installation_id":"device","parent_thread_id":"root"}`)
	body := []byte(`{"prompt_cache_key":"isolated-cache","client_metadata":{"session_id":"root","thread_id":"child"}}`)
	rewritten := proxy.ApplyCodexFingerprintToBody(body, a, h)
	e := NewExecutor()
	out := e.prepareWebsocketHeaders(context.Background(), "token", a, "42", "isolated-cache", "key", nil, h, rewritten, "")
	session, thread := proxy.ConvergedCodexSessionIdentity(a, h)
	if out.Get("Session-Id") != session || out.Get("Thread-Id") != thread {
		t.Fatal("WS identity mismatch", out)
	}
	if gjson.GetBytes(rewritten, "client_metadata.session_id").String() != session || gjson.GetBytes(rewritten, "client_metadata.thread_id").String() != thread {
		t.Fatal("WS body mismatch")
	}
	if gjson.GetBytes(rewritten, "prompt_cache_key").String() != "isolated-cache" {
		t.Fatal("WS cache partition changed")
	}
}

// prepareWebsocketHeaders is a test helper: it ignores identity resolution errors and only asserts the assembled handshake headers.
func (e *Executor) prepareWebsocketHeaders(ctx context.Context, accessToken string, account *auth.Account, accountID, sessionID, apiKey string, deviceCfg *proxy.DeviceProfileConfig, ginHeaders http.Header, wsBody []byte, affinityKey string) http.Header {
	headers, _ := e.prepareWebsocketHeadersChecked(ctx, accessToken, account, accountID, sessionID, apiKey, deviceCfg, ginHeaders, wsBody, affinityKey)
	return headers
}
