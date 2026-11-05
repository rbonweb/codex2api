package wsrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== WebSocket executor constants ====================

const (
	// Beta header that enables the WebSocket Responses API
	responsesWebsocketBetaHeader = "responses_websockets=2026-02-06"

	// Codex WebSocket endpoint
	CodexWsEndpoint = "/responses"
)

func shouldSendWebsocketUserAgent() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_SEND_USER_AGENT"))) {
	case "0", "false", "no", "n", "off":
		return false
	default:
		return true
	}
}

// statelessOneShotEnabled reports whether WS connection reuse is disabled for requests without an explicit session (each request gets its own connection,
// destroyed after use). This is the hard-isolation escape hatch that stops any connection-level state from leaking across requests or users, at the cost of
// a handshake per request (under high RPM this can trigger upstream handshake rate limiting with 503).
func statelessOneShotEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_STATELESS_ONESHOT"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// resolveHandshakeSessionID decides the value of the WS handshake headers Session_id/Conversation_id.
// These headers are frozen per connection: they are sent once when the connection is opened and never updated when it is reused. So for a stateless connection that is reused across several requests
// (or even several end users sharing the same API key), the connection must never carry any
// "per-request identity" (such as a random prompt_cache_key per request). If the upstream binds conversation state at the connection level,
// Conversation_id, the first request's conversation identity leaks to the later requests that reuse this connection,
// all users, causing cross-user context pollution (same class as issue #268/#308, "user 2 context bleeding into user 1").
//
//   - Explicit session (not stateless): the connection is dedicated to the session, and the header = the session ID (original behavior).
//   - stateless + default isolation (poolRouteKey non-empty): returns an empty string, so this header group is not sent,
//     the upstream has no connection-level session identity to bind; per-request identity is carried entirely by the per-request unique
//     prompt_cache_key in the frame body.
//   - stateless + per-api-key mode (poolRouteKey empty): keeps the deterministic
//     cache key (this mode explicitly chooses to share the upstream cache per key, and the cache only pays off when the header and the frame body match).
func resolveHandshakeSessionID(sessionID, poolRouteKey string, wsBody []byte) string {
	if !proxy.IsStatelessWebsocketSessionID(sessionID) {
		return sessionID
	}
	if strings.TrimSpace(poolRouteKey) != "" {
		return ""
	}
	if cacheKey := strings.TrimSpace(gjson.GetBytes(wsBody, "prompt_cache_key").String()); cacheKey != "" {
		return cacheKey
	}
	return sessionID
}

// ==================== WebSocket executor ====================

// Executor is the WebSocket executor
type Executor struct {
	manager *Manager
	mu      sync.RWMutex
}

// NewExecutor creates a WebSocket executor
func NewExecutor() *Executor {
	return &Executor{
		manager: GetManager(),
	}
}

// NewExecutorWithManager creates an executor with the given manager
func NewExecutorWithManager(manager *Manager) *Executor {
	return &Executor{
		manager: manager,
	}
}

// ExecuteRequestViaWebsocket sends a request over WebSocket
func (e *Executor) ExecuteRequestViaWebsocket(
	ctx context.Context,
	account *auth.Account,
	requestBody []byte,
	sessionID string,
	proxyOverride string,
	apiKey string,
	deviceCfg *proxy.DeviceProfileConfig,
	ginHeaders http.Header,
	poolRouteKey string,
) (*WsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	accountIDStr := account.AccountID
	account.Mu().RUnlock()

	if accessToken == "" {
		return nil, fmt.Errorf("no available access_token")
	}

	// Prepare the request body
	wsBody := e.prepareWebsocketBody(requestBody, sessionID)

	headerSessionID := resolveHandshakeSessionID(sessionID, poolRouteKey, wsBody)

	// Build the WebSocket URL
	httpURL := proxy.CodexBaseURL + CodexWsEndpoint
	wsURL, err := buildWebsocketURL(httpURL)
	if err != nil {
		return nil, fmt.Errorf("failed to build WebSocket URL: %w", err)
	}

	// The egress path is decided uniformly by ResolveCodexWebsocketEgress (Resin > proxy > direct):
	// In Resin mode the WS address is rewritten to the reverse-proxy path, and the dialer (createConnection) likewise skips the proxy based on it.
	egress := proxy.ResolveCodexRequestEgress(ctx, account, wsURL, effectiveProxyURL(account, proxyOverride), true)
	wsURL = egress.URL

	// Prepare the request headers
	// The cross-account echo guard strips turn state known to come from other accounts at the end of handshake header assembly.
	affinityKey := proxy.CodexTurnStateAffinityKeyFromContext(ctx)
	headers, err := e.prepareWebsocketHeadersChecked(ctx, accessToken, account, accountIDStr, headerSessionID, apiKey, deviceCfg, ginHeaders, wsBody, affinityKey)
	if err != nil {
		return nil, err
	}
	// Handshake headers are not resent on a reused connection, so every turn must sync the final state into response.create.
	if state := headers.Get("X-Codex-Turn-State"); state != "" {
		wsBody = jsonbody.SetString(wsBody, "client_metadata.x-codex-turn-state", state)
	} else {
		wsBody = jsonbody.DeletePath(wsBody, "client_metadata.x-codex-turn-state")
	}
	// Record the attempted handshake UA immediately so failed handshakes are
	// still auditable. A reused connection replaces this below with the UA that
	// was actually sent when that connection was established.
	proxy.RecordUpstreamUserAgent(ctx, headers.Get("User-Agent"))

	egress.ApplyHeaders(headers)

	// Get or create a connection. Requests without an explicit session (stateless connection ID) take the deterministic cache key
	// and reuse connections within its slot pool, so that sustained high RPM does not trigger upstream rate limits from per-request handshakes.
	//
	// The connection pool baseKey must be stable per API key and must never equal the upstream identity key that is unique per request, otherwise
	// in default isolation mode it changes on every request → the slot pool is invalidated → per-request handshakes trigger 503.
	// When poolRouteKey (from the upstream deterministic key) is non-empty, it takes priority as the baseKey, so connection reuse is per API key
	// and reliably hits the same group of 8 slots.
	//
	// Isolation note: in default isolation mode, per-request upstream identity isolation is guaranteed by the per-request unique
	// prompt_cache_key written into each response.create frame body (see the injection point in proxy/executor.go). The handshake headers
	// Session_id/Conversation_id are frozen per connection and must never carry the identity of any single request
	// (see resolveHandshakeSessionID).
	//
	// When CODEX_WS_STATELESS_ONESHOT=1, slot reuse is disabled: each request without a session gets its own connection,
	// destroyed after use (this fully prevents any connection-level state from leaking across requests, at the cost of a handshake per request).
	// Continuation-chain affinity: when the upstream has no server-side storage, the context of previous_response_id only lives on the
	// WS connection that produced that response. A request carrying a continuation-chain ID first tries to fetch the original connection (used only if exclusive acquisition succeeds),
	// otherwise falling back to a random slot would trigger upstream "previous response not found".
	// Background side requests on the same thread (request_kind=memory, guardian subagents) form their own lane,
	// and do not queue behind the user's in-flight turn with the same key; when Desktop uses HTTP, the metadata is only in the request body.
	// The original envelope owns local lane isolation. Its stream_id is not a
	// Codex backend field and has already been removed from the outbound body.
	poolSessionID := proxy.ResolveCodexWebsocketTransportSessionKeyWithBody(sessionID, ginHeaders, requestBody)
	ctx = withConnectionCapacityKind(ctx, ginHeaders, wsBody)
	var wc *WsConnection
	var pr *PendingRequest
	var err2 error
	acquireStart := time.Now()
	if prevRespID := strings.TrimSpace(gjson.GetBytes(wsBody, "previous_response_id").String()); prevRespID != "" {
		wc, pr, poolSessionID, err2 = e.acquireContinuation(ctx, websocketContinuation{
			responseID: prevRespID, accountID: account.ID(), apiKey: apiKey, identity: websocketClientIdentity(headers),
			model: gjson.GetBytes(wsBody, "model").String(), url: wsURL, proxyURL: requestProxyKey(ctx, account, proxyOverride),
		})
		if err2 != nil {
			return nil, err2
		}
	}
	baseKey := strings.TrimSpace(poolRouteKey)
	if baseKey == "" && headerSessionID != sessionID {
		baseKey = headerSessionID
	}
	// Handshake headers (including X-Codex-Turn-State from template Apply) freeze at
	// dial time. Isolate reusable slots by the same model string used for template
	// lookup so a later model cannot reuse a connection whose turn-state was frozen
	// for a different template identity. prompt_cache_key / session header isolation
	// comments above are unchanged.
	baseKey = reusablePoolBaseKeyWithModel(baseKey, gjson.GetBytes(wsBody, "model").String())
	baseKey = websocketClientPoolKey(baseKey, headers)
	if wc == nil {
		poolSessionID = websocketClientPoolKey(poolSessionID, headers)
		// A pooled handshake must belong to the same mapped conversation/thread.
		poolSessionID = proxy.ScopeCodexFingerprintTransportKey(poolSessionID, account, ginHeaders)
		baseKey = proxy.ScopeCodexFingerprintTransportKey(baseKey, account, ginHeaders)
		if proxy.IsStatelessWebsocketSessionID(sessionID) && baseKey != "" && !statelessOneShotEnabled() {
			wc, pr, poolSessionID, err2 = e.manager.AcquireReusableConnection(ctx, account, wsURL, baseKey, sessionID, statelessConnectionSlots(), headers, proxyOverride)
		} else {
			wc, pr, err2 = e.manager.AcquireConnection(ctx, account, wsURL, poolSessionID, headers, proxyOverride)
		}
	}
	// Connection acquisition time (busy queueing + liveness probe + handshake) is counted in this attempt's ws_acquire_ms (issue #413)
	proxy.AddWsAcquireDuration(ctx, time.Since(acquireStart))
	if err2 != nil {
		return nil, err2
	}
	// AcquireConnection may fall back to <lane>#ovf-N under the busy overflow policy;
	// both the response_id continuation binding and the redial after a failed send must use the actual slot, and must not keep using
	// the base lane recorded before the call. The normal, stateless slot and preferred paths all
	// make the same idempotent correction here.
	poolSessionID = actualWebsocketPoolSessionID(wc, poolSessionID)
	if wc.upstreamUserAgentKnown {
		proxy.RecordUpstreamUserAgent(ctx, wc.upstreamUserAgent)
	}

	// Send the request; on failure, retry up to 2 times (rebuilding the connection).
	// Use DiscardConnection to clean up precisely by connection pointer: the PoolKey of a connection fetched through continuation affinity
	// may differ from the proxy combination of the current request, so recomputing the key from the parameters would miss the removal.
	if err := proxy.ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(wsBody, "model").String()); err != nil {
		if !wc.cancelUnsentReadLease(pr.RequestID) {
			e.manager.DiscardConnection(wc)
		}
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.releaseUnusedChatCapacity(wc)
		return nil, err
	}
	sendErr := e.sendRequest(wc, wsBody, pr.RequestID)
	if sendErr != nil && gjson.GetBytes(wsBody, "previous_response_id").String() != "" {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.DiscardConnection(wc)
		return nil, &proxy.ResponsesContinuationLostError{Reason: "original_connection_send_failed"}
	}
	for retries := 0; shouldRetryWebsocketSendError(sendErr) && retries < 2; retries++ {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.DiscardConnection(wc)

		if err := waitWebsocketSendRetry(ctx, retries); err != nil {
			return nil, err
		}

		reacquireStart := time.Now()
		wc, pr, err2 = e.manager.AcquireConnection(ctx, account, wsURL, poolSessionID, headers, proxyOverride)
		proxy.AddWsAcquireDuration(ctx, time.Since(reacquireStart))
		if err2 != nil {
			return nil, err2
		}
		if wc.upstreamUserAgentKnown {
			proxy.RecordUpstreamUserAgent(ctx, wc.upstreamUserAgent)
		}
		sendErr = e.sendRequest(wc, wsBody, pr.RequestID)
	}
	if sendErr != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.DiscardConnection(wc)
		return nil, fmt.Errorf("failed to send WebSocket request: %w", sendErr)
	}

	// Start the heartbeat
	e.manager.StartHeartbeat(wc)

	return &WsResponse{
		conn:              wc,
		pendingReq:        pr,
		sessionID:         poolSessionID,
		manager:           e.manager,
		apiKey:            apiKey,
		model:             gjson.GetBytes(wsBody, "model").String(),
		readErrChan:       make(chan error, 1),
		upstreamUserAgent: wc.upstreamUserAgent,
		upstreamVersion:   wc.upstreamVersion,
	}, nil
}

func actualWebsocketPoolSessionID(wc *WsConnection, fallback string) string {
	if wc == nil || wc.session == nil {
		return fallback
	}
	if actual := strings.TrimSpace(wc.session.ID); actual != "" {
		return actual
	}
	return fallback
}

func shouldRetryWebsocketSendError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	return !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig
}

// prepareWebsocketBody prepares the WebSocket request body
func (e *Executor) prepareWebsocketBody(body []byte, sessionID string) []byte {
	if len(body) == 0 {
		return nil
	}

	// Clone and modify the request body
	wsBody := bytes.Clone(body)

	// 1. Ensure the instructions field exists
	if !gjson.GetBytes(wsBody, "instructions").Exists() {
		wsBody, _ = sjson.SetBytes(wsBody, "instructions", "")
	}

	// 2. Remove extra fields (the upstream does not accept prompt_cache_retention and returns 400 Unsupported parameter, so it must be deleted)
	wsBody = jsonbody.DeletePath(wsBody, "prompt_cache_retention")
	wsBody = jsonbody.DeletePath(wsBody, "safety_identifier")
	wsBody = jsonbody.DeletePath(wsBody, "disable_response_storage")
	// Downstream named lanes are multiplexed locally onto independent leases.
	// Their routing envelope must not reach the single-response Codex socket.
	wsBody = jsonbody.DeletePath(wsBody, "stream_id")

	// 3. Inject prompt_cache_key
	// The stateless sessionID is only a one-off random ID for connection pool isolation; injecting it would make the upstream
	// prompt cache miss on every request; in that case keep the deterministic cache key already in the request body
	// (injected by proxy.ExecuteRequest or supplied by the client).
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(wsBody, "prompt_cache_key").String())
	if sessionID != "" && !proxy.IsStatelessWebsocketSessionID(sessionID) {
		wsBody = jsonbody.SetString(wsBody, "prompt_cache_key", sessionID)
	} else if existingCacheKey != "" {
		wsBody = jsonbody.SetString(wsBody, "prompt_cache_key", existingCacheKey)
	}

	// 4. Set the request type and stream
	wsBody = jsonbody.SetString(wsBody, "type", "response.create")
	wsBody = jsonbody.SetBool(wsBody, "stream", true)

	return wsBody
}

// reusablePoolBaseKeyWithModel isolates reusable WS slots by the model string used
// for turn-state template lookup. Handshake headers freeze at dial time; without
// this, a later model can reuse a connection whose X-Codex-Turn-State was frozen
// for a different template identity.
func reusablePoolBaseKeyWithModel(baseKey, model string) string {
	baseKey = strings.TrimSpace(baseKey)
	model = strings.TrimSpace(model)
	if baseKey == "" || model == "" {
		return baseKey
	}
	return baseKey + "|m:" + model
}

// prepareWebsocketHeadersChecked prepares the WebSocket request headers; it returns an error if client identity resolution fails, and does not assemble incomplete handshake headers.
// affinityKey is used by the turn-state cross-account echo guard; when it is empty the guard is a no-op.
func (e *Executor) prepareWebsocketHeadersChecked(ctx context.Context, accessToken string, account *auth.Account, accountID, sessionID, apiKey string, deviceCfg *proxy.DeviceProfileConfig, ginHeaders http.Header, wsBody []byte, affinityKey string) (http.Header, error) {
	headers := http.Header{}

	// Auth headers
	headers.Set("Authorization", "Bearer "+accessToken)

	// Beta header that enables the WebSocket Responses API
	headers.Set("OpenAI-Beta", responsesWebsocketBetaHeader)

	if account == nil {
		account = &auth.Account{AccountID: accountID}
	}
	identity, err := proxy.ResolveCodexOutboundClientIdentity(proxy.CodexClientIdentityInput{Account: account, APIKey: apiKey, DeviceConfig: deviceCfg, Headers: ginHeaders})
	if err != nil {
		return nil, err
	}
	usedGeneratedHeaders := identity.Generated
	if shouldSendWebsocketUserAgent() {
		headers.Set("User-Agent", identity.UserAgent)
		if identity.Version != "" {
			headers.Set("Version", identity.Version)
		}
	} else {
		// An explicit empty value suppresses the Go default UA; the version check still runs before assembly.
		headers["User-Agent"] = []string{""}
	}
	if betaFeatures := strings.TrimSpace(ginHeaders.Get("X-Codex-Beta-Features")); betaFeatures != "" {
		headers.Set("X-Codex-Beta-Features", betaFeatures)
	} else if deviceCfg != nil && strings.TrimSpace(deviceCfg.BetaFeatures) != "" {
		headers.Set("X-Codex-Beta-Features", strings.TrimSpace(deviceCfg.BetaFeatures))
	} else {
		// Session-level default: real Codex sends this header on every handshake, and the default value is remote_compaction_v2.
		headers.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	}

	// Originator: same rule as the HTTP path. When a UA is generated, it follows the generated client prefix,
	// and when an official client is passed through, the downstream value is kept.
	if usedGeneratedHeaders {
		headers.Set("Originator", proxy.CodexOriginatorForGeneratedUserAgent(identity.UserAgent))
	} else if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" && proxy.IsCodexOfficialClientByHeaders("", originator) {
		headers.Set("Originator", originator)
	} else {
		headers.Set("Originator", proxy.Originator)
	}
	// attestation is always generated by this project and is never passed through from downstream.
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Responsesapi-Include-Timing-Metrics"} {
		if value := strings.TrimSpace(ginHeaders.Get(name)); value != "" {
			headers.Set(name, value)
		}
	}
	if headers.Get("X-Codex-Turn-State") == "" {
		if state := strings.TrimSpace(gjson.GetBytes(wsBody, "client_metadata.x-codex-turn-state").String()); state != "" {
			headers.Set("X-Codex-Turn-State", state)
		}
	}
	proxy.GuardCodexTurnStateEcho(affinityKey, account, headers)
	// Fingerprint convergence: it overrides the client's original values after pass-through, and keeps operator overrides ahead of the account's custom headers.
	// Handshake headers are frozen per connection, and a reused connection keeps the values from when it was opened; the converged values are constant per account, which is consistent with
	// this semantic. The off level is a no-op.
	proxy.ApplyCodexFingerprintHeaders(headers, account, ginHeaders)

	// Account ID
	if accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	// Session identity headers use the same assembly as the HTTP path: the real client's WS handshake headers
	// (codex-rs/core/src/client.rs build_websocket_headers) have the same shape as HTTP /responses,
	// all being session-id / thread-id / x-client-request-id, and none of them sends Conversation_id.
	// Under the legacy level, this function restores the old Session_id behavior and clears Conversation_id.
	proxy.ApplyCodexSessionHeaders(headers, account, sessionID, ginHeaders, true)
	for name, value := range account.GetCustomHeaders() {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		headers.Set(name, value)
	}
	if err := proxy.ApplyCodexAttestation(ctx, proxy.CodexAttestationInput{Headers: headers, Account: account}); err != nil {
		return nil, err
	}

	// The routing hint is synthesized by the gateway from the final WS frame body and set after the account's custom headers.
	// Handshake headers are frozen per connection: a reused connection keeps the hint from when it was opened, and its meaning is a soft affinity at dial time.
	proxy.ApplyCodexRoutingHint(headers, account, wsBody)

	return headers, nil
}

// sendRequest sends a WebSocket request
func (e *Executor) sendRequest(wc *WsConnection, body []byte, requestID string) error {
	if !wc.IsConnected() {
		return fmt.Errorf("websocket connection is not connected")
	}
	if err := wc.ensureReadLeaseForSend(requestID); err != nil {
		return err
	}
	return wc.WriteMessage(websocket.TextMessage, body)
}

// ==================== WebSocket response handling ====================

// WsResponse is the WebSocket response wrapper
type WsResponse struct {
	model       string
	conn        *WsConnection
	pendingReq  *PendingRequest
	sessionID   string
	manager     *Manager
	readErrChan chan error
	closed      bool
	// apiKey is the downstream API key that issued this request, used to verify ownership of the response_id → connection binding.
	apiKey string
	// The client identity sent during the handshake by the connection that carries this request.
	upstreamUserAgent string
	upstreamVersion   string
	// connBroken marks that the read stream was terminated by an upstream WS error (abnormal close) or a downstream write failure;
	// Close() uses it to destroy the broken connection instead of returning it to the pool for reuse. Protected by mu.
	connBroken bool
	// streamCompleted marks that the read stream has been consumed to an explicit termination boundary (response.completed /
	// response.failed / upstream error frame). Close() returns the connection for reuse only when this flag is true and
	// connBroken is not set; in all other cases (downstream disconnect, ctx cancelled, upstream close,
	// a read stream left unread after a handshake failure, etc.) the upstream may still push leftover frames on that connection, and returning it for reuse would mix the previous
	// request's response into the next user's stream (issue #308), so it must be destroyed. Protected by mu.
	streamCompleted       bool
	mu                    sync.Mutex
	interruptResponseID   string
	interruptTerminalID   string
	interruptTerminalSeen bool
	interruptTimer        *time.Timer
}

// ReadStream reads the SSE stream
func (r *WsResponse) ReadStream(callback func(data []byte) bool) error {
	if r.conn == nil {
		return fmt.Errorf("websocket connection is not available")
	}
	if r.pendingReq != nil {
		if err := r.conn.ensureReadLeaseForResponse(r.pendingReq.RequestID); err != nil {
			return fmt.Errorf("websocket connection is not available: %w", err)
		}
	}

	for {
		msgType, payload, err := r.conn.ReadMessage()
		if err != nil {
			// ReadStream only returns successfully after consuming an explicit
			// response terminal frame. Any socket close here, including 1000/1001,
			// is premature and must preserve the real close error for the consumer.
			r.markConnBroken()
			return fmt.Errorf("websocket read error: %w", err)
		}

		// Only handle text messages
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				return fmt.Errorf("unexpected binary message from websocket")
			}
			continue
		}

		// Clean up the message
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}

		// Parse and handle the message
		payload = normalizeCompletionEvent(payload)
		handled, interruptErr := r.handleInterruptMessage(payload, callback)
		if !handled {
			interruptErr = r.handleMessage(payload, callback)
		}
		if err := interruptErr; err != nil {
			if err == io.EOF {
				// The termination boundary (completed / failed / error frame) has been reached. If a downstream write failure already marked
				// connBroken midway, Close() still destroys the connection.
				r.markStreamCompleted()
				return nil
			}
			return err
		}
	}
}

// handleMessage handles a single WebSocket message
func (r *WsResponse) handleMessage(payload []byte, callback func(data []byte) bool) error {
	// Upstream error frame: pass it through to the downstream (converted into an SSE error event), instead of turning it into a Go error and silently closing the pipe.
	// Otherwise the downstream only sees an underlying read error → it appears as an empty response, with no way to know the actual error.
	if errEvent, isErr := r.buildErrorEvent(payload); isErr {
		// Connection-level lifetime limit error: it applies to the connection, not to a single request, so later
		// response.create requests on this connection always fail, while the Ping liveness probe still succeeds; returning it to the pool would keep poisoning
		// subsequent requests (including those routed back by continuation affinity), so it must be marked for destruction (issue #346).
		if isConnLimitErrorFrame(payload) {
			r.markConnBroken()
		}
		// Write the error content to the downstream as SSE data, so the client sees the full error JSON.
		if !callback(errEvent) {
			r.markConnBroken()
		}
		// An error is terminal: end the stream (equivalent to response.failed).
		return io.EOF
	}

	// Normalize the completion event type
	payload = normalizeCompletionEvent(payload)

	// For a terminal state, record the binding and completion status first, then hand it to the downstream.
	eventType := gjson.GetBytes(payload, "type").String()
	terminal := isReadLeaseTerminal(payload)
	if eventType == "response.completed" || eventType == "response.incomplete" {
		r.bindCompletedResponse(gjson.GetBytes(payload, "response.id").String())
	}
	if terminal {
		// The downstream can cancel the request once it reads the terminal state; mark early so the cancel callback does not mistakenly destroy a completed connection.
		r.markStreamCompleted()
	}
	// Call the callback
	if !callback(payload) {
		// Downstream write failed (broken pipe / client disconnected): the response stream was truncated at a non-terminal boundary,
		// the upstream will still keep pushing the rest of this response's frames on this connection. The connection must be destroyed,
		// since returning it to the pool for reuse would send the leftover frames to the next request (issue #308).
		if !terminal {
			r.markConnBroken()
		}
		return io.EOF
	}

	// Check whether this is a terminal event
	if terminal {
		return io.EOF
	}

	return nil
}

// buildErrorEvent checks whether the payload is an upstream error frame; if it is, it returns a downstream-recognizable
// response.failed SSE event (keeping the original error content); the second return value marks whether it is an error frame.
func (r *WsResponse) buildErrorEvent(payload []byte) ([]byte, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if gjson.GetBytes(payload, "type").String() != "error" {
		return nil, false
	}

	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}

	errMsg := gjson.GetBytes(payload, "error.message").String()
	if errMsg == "" {
		errMsg = gjson.GetBytes(payload, "message").String()
	}
	if errMsg == "" && status > 0 {
		errMsg = http.StatusText(status)
	}
	if errMsg == "" {
		errMsg = "upstream websocket error"
	}

	// Build the response.failed event: the downstream ReadSSEStream already recognizes this type as a terminal failure,
	// aligned with the error semantics of the HTTP path; it also keeps the original upstream error object so clients can troubleshoot.
	// The upstream error object may be pretty-printed JSON with line breaks, so it must be compacted into a single line,
	// otherwise after SSE data: line encoding the downstream can only read the first line (the error message gets truncated).
	errObj := compactJSONOneLine(gjson.GetBytes(payload, "error").Raw)
	if errObj == "" {
		errObj = fmt.Sprintf(`{"message":%q,"code":%d}`, errMsg, status)
	}
	createdAt := time.Now().Unix()
	event := fmt.Sprintf(`{"type":"response.failed","response":{"created_at":%d,"status":"failed","error":%s}}`, createdAt, errObj)
	if status > 0 {
		event = fmt.Sprintf(`{"type":"response.failed","response":{"created_at":%d,"status":"failed","status_code":%d,"error":%s}}`, createdAt, status, errObj)
	}
	return []byte(event), true
}

// isConnLimitErrorFrame checks whether an upstream error frame is a connection-level lifetime limit error
// (websocket_connection_limit_reached): this error applies per connection rather than per request,
// so reusing this connection will keep failing.
func isConnLimitErrorFrame(payload []byte) bool {
	code := gjson.GetBytes(payload, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(payload, "code").String()
	}
	return code == "websocket_connection_limit_reached"
}

// normalizeCompletionEvent normalizes the completion event type
func normalizeCompletionEvent(payload []byte) []byte {
	if gjson.GetBytes(payload, "type").String() == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

// compactJSONOneLine compacts JSON that may contain line breaks into a single line; invalid JSON or an empty string returns "".
func compactJSONOneLine(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return ""
	}
	return buf.String()
}

// markConnBroken marks the underlying connection as not reusable because of an upstream WS error or a downstream write failure (idempotent, protected by mu).
func (r *WsResponse) markConnBroken() {
	r.mu.Lock()
	r.connBroken = true
	r.mu.Unlock()
	if r.manager != nil && r.conn != nil {
		r.manager.DiscardConnection(r.conn)
	}
}

// markStreamCompleted marks that the read stream has been consumed to an explicit termination boundary (idempotent, protected by mu).
func (r *WsResponse) markStreamCompleted() {
	r.mu.Lock()
	r.streamCompleted = true
	r.mu.Unlock()
}

// Close closes the response and returns the connection
func (r *WsResponse) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true
	if r.interruptTimer != nil {
		r.interruptTimer.Stop()
	}

	// Remove the waiting request
	if r.conn != nil && r.conn.session != nil && r.pendingReq != nil {
		r.conn.session.RemovePendingRequest(r.pendingReq.RequestID)
	}

	// Decide where the connection goes based on how the read stream ended:
	//   - The termination boundary (completed/failed/error frame) was reached and there was no error: return the connection to the pool for further reuse.
	//   - In any other case, destroy it and remove it from the pool:
	//     * Upstream WS error (close 1006/1009/1011, read error) → reusing a broken connection drops the stream and leaves the fd stuck in CLOSE_WAIT;
	//     * Downstream write failure / ctx cancelled / normal upstream close / stream left unread after a handshake failure → the stream was not consumed to the boundary,
	//       the upstream may still push leftover frames, and reuse would mix sessions (issue #308).
	if r.conn != nil {
		if !r.connBroken && r.streamCompleted && r.interruptResponseID == "" && !r.shouldDiscardOneShotConn() {
			r.manager.ReleaseConnection(r.conn)
		} else {
			reason := closeUnconsumed
			if !r.connBroken && r.streamCompleted {
				reason = closeOneShotComplete
			}
			r.manager.discardConnectionFor(r.conn, reason)
		}
	}

	return nil
}

// shouldDiscardOneShotConn decides whether a one-shot pool-key connection is destroyed directly after the response finishes normally.
// The pool key of this kind of connection is unique per request, so after it is returned to the pool it can never be reused by key, and it only takes up the account's connection quota
// until the idle timeout. The only reason to keep it is response_id continuation affinity (when the upstream has no server-side storage,
// the previous_response_id context only lives on the connection that produced the response), so while a live binding exists
// it is still returned to the pool. One-shot mode only forbids generic slot reuse and must not break a continuation chain that has already been created.
func (r *WsResponse) shouldDiscardOneShotConn() bool {
	if r.conn == nil || r.conn.session == nil || !proxy.IsStatelessWebsocketSessionID(r.conn.session.ID) {
		return false
	}
	return r.manager == nil || !r.manager.hasLiveResponseBinding(r.conn)
}

// HTTPResponse returns the HTTP handshake response
func (r *WsResponse) HTTPResponse() *http.Response {
	if r.conn != nil {
		return r.conn.HTTPResponse()
	}
	return nil
}

// ==================== Helper functions ====================

// buildWebsocketURL builds a WebSocket URL from an HTTP URL
func buildWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}

	return parsed.String(), nil
}

// ==================== Global executor instance ====================

var globalExecutor *Executor
var executorOnce sync.Once

// GetExecutor returns the global executor instance
func GetExecutor() *Executor {
	executorOnce.Do(func() {
		globalExecutor = NewExecutor()
	})
	return globalExecutor
}

// ShutdownExecutor shuts down the global executor and manager
func ShutdownExecutor() {
	ShutdownManager()
}

// ExecuteRequestWebsocket sends a request over WebSocket
// returns a simulated http.Response for compatibility with existing code
func ExecuteRequestWebsocket(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
	exec := GetExecutor()
	wsResp, err := exec.ExecuteRequestViaWebsocket(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
	if err != nil {
		// Upstream 401/402 during the handshake (account-level errors such as an invalid token or a deactivated workspace) are restored as
		// HTTP responses with their real status codes, instead of transport errors: otherwise the usage log would only show
		// 598/transport, and the account would trigger neither the unauthorized cooldown nor
		// the deactivated_workspace error marking, so the bad account would stay in the scheduling pool and be dialed over and over
		// (compare the HTTP path, where 401/402 are visible directly and cool down or are marked immediately).
		if resp, ok := handshakeAccountErrorHTTPResponse(err); ok {
			return resp, nil
		}
		return nil, err
	}

	// Check the HTTP handshake response status. The standard status for a successful WebSocket handshake is 101,
	// but here it must be wrapped into an SSE HTTP 200 response that the existing handler can consume.
	handshakeResp := wsResp.HTTPResponse()
	statusCode, handshakeHeader, handshakeFailed := normalizeWebsocketHandshakeResponse(handshakeResp)
	if handshakeFailed {
		detail := formatFailedHandshakeHTTPBody(statusCode, handshakeResp)
		wsResp.Close()
		return &http.Response{
			StatusCode: statusCode,
			Header:     handshakeHeader.Clone(),
			Body:       io.NopCloser(strings.NewReader(detail)),
		}, nil
	}

	return websocketResponseToHTTP(ctx, wsResp, statusCode, handshakeHeader), nil
}

func websocketResponseToHTTP(ctx context.Context, wsResp *WsResponse, statusCode int, handshakeHeader http.Header) *http.Response {
	if ctx == nil {
		ctx = context.Background()
	}
	unregisterInterrupt := proxy.RegisterResponsesWSInterrupt(ctx, wsResp.interrupt)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       pr,
	}

	// Copy header information from the HTTP handshake response
	if handshakeHeader != nil {
		for key, values := range handshakeHeader {
			for _, v := range values {
				resp.Header.Add(key, v)
			}
		}
	}

	// The connection handshake state is not this turn's upstream response; when reused, the old snapshot must not be re-collected or passed back.
	resp.Header.Del("X-Codex-Turn-State")
	// Like the HTTP response, carry the outbound identity so that the "model not supported" check can tell apart the client version gate.
	if wsResp != nil && (wsResp.upstreamUserAgent != "" || wsResp.upstreamVersion != "") {
		outbound := make(http.Header)
		if wsResp.upstreamUserAgent != "" {
			outbound.Set("User-Agent", wsResp.upstreamUserAgent)
		}
		if wsResp.upstreamVersion != "" {
			outbound.Set("Version", wsResp.upstreamVersion)
		}
		resp.Request = &http.Request{Method: http.MethodGet, Header: outbound}
	}
	// Set the SSE response headers
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Set("Cache-Control", "no-cache")
	resp.Header.Set("Connection", "keep-alive")

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Close the pipe before the WS response: the pipe takes the first error, which guarantees the downstream reads
			// a cancellation, not the read error caused by the connection being destroyed afterwards.
			_ = pw.CloseWithError(ctx.Err())
			_ = wsResp.Close()
		case <-done:
		}
	}()

	// Read the WebSocket stream in the background and write it into the pipe
	go func() {
		defer close(done)
		defer pw.Close()
		defer wsResp.Close()
		defer unregisterInterrupt()

		err := wsResp.ReadStream(func(data []byte) bool {
			// The turn state the upstream sends back is only in the frames (the handshake headers are the old snapshot from when the connection was opened), so it is observed frame by frame and recorded in the trace.
			proxy.ObserveCodexTurnStateFrame(ctx, data)
			// SSE data: payloads are delimited by newlines, so frames containing newlines (such as pretty-printed JSON)
			// must be compacted into a single line first, otherwise the downstream parser can only read the first line.
			if bytes.IndexByte(data, '\n') >= 0 {
				if compacted := compactJSONOneLine(string(data)); compacted != "" {
					data = []byte(compacted)
				} else {
					data = bytes.ReplaceAll(data, []byte("\n"), []byte(" "))
				}
			}
			// Encode the data in SSE format
			if _, err := pw.Write([]byte("data: ")); err != nil {
				return false
			}
			if _, err := pw.Write(data); err != nil {
				return false
			}
			if _, err := pw.Write([]byte("\n\n")); err != nil {
				return false
			}
			return true
		})

		if err != nil && err != io.EOF {
			pw.CloseWithError(err)
		}
	}()

	return resp
}

func normalizeWebsocketHandshakeResponse(handshakeResp *http.Response) (statusCode int, header http.Header, failed bool) {
	if handshakeResp == nil {
		return http.StatusOK, http.Header{}, false
	}

	statusCode = handshakeResp.StatusCode
	header = handshakeResp.Header
	if statusCode == http.StatusSwitchingProtocols || (statusCode >= 200 && statusCode < 300) {
		return http.StatusOK, header, false
	}
	return statusCode, header, true
}
