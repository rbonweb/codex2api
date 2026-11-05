package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/codex2api/security"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http2"
)

// Connection health probe parameters for the Codex/OpenAI HTTP/2 upstream.
//
// The standard library net/http negotiates HTTP/2 via ALPN, but http2.Transport by default
// has ReadIdleTimeout=0 (no keepalive PING), so it cannot detect connections silently cut off by a proxy or NAT
// "dead connections" (both ends think they are alive). A request that lands on one hangs until the OS TCP
// retransmission timeout (minutes) finally fails it, which shows up as a very long TTFT. Enable active PING: once the connection is idle for
// ReadIdleTimeout, send a PING. If there is no reply within PingTimeout, the connection is judged dead and closed,
// removed at the source, so the request can be rebuilt on another connection instead of hanging.
//
// Applies only to direct HTTP/2 connections (the standard transport and the uTLS transport); the WebSocket
// WebSocket path already has full Ping/Pong keepalive and liveness checks before reuse, so it does not go through here.
// The defaults suit direct connections. Through a high-latency proxy or weak-network egress, 15s is too aggressive: one PING that is not answered in time
// will tear down the whole connection and every stream multiplexed on it (showing as "http2: client connection
// lost"). So CODEX_HTTP2_READ_IDLE_TIMEOUT / CODEX_HTTP2_PING_TIMEOUT may be set to override them
// Values use Go duration syntax, such as "45s"; 0 disables active PING and falls back to the standard library default.
const (
	defaultCodexHTTP2ReadIdleTimeout = 15 * time.Second
	defaultCodexHTTP2PingTimeout     = 15 * time.Second
)

var (
	codexHTTP2ReadIdleTimeout = durationFromEnv("CODEX_HTTP2_READ_IDLE_TIMEOUT", defaultCodexHTTP2ReadIdleTimeout)
	codexHTTP2PingTimeout     = durationFromEnv("CODEX_HTTP2_PING_TIMEOUT", defaultCodexHTTP2PingTimeout)
)

// durationFromEnv reads an environment variable in Go duration format; a missing or invalid value falls back to the default,
// while an explicit "0" is valid input (it disables the corresponding mechanism).
func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("[Config] %s=%q is invalid, keeping default %s", key, raw, fallback)
		return fallback
	}
	return d
}

// enableCodexHTTP2KeepAlive explicitly configures HTTP/2 on a standard *http.Transport and
// enables connection health probes (ReadIdleTimeout/PingTimeout), returning the underlying *http2.Transport
// for test assertions. If configuration fails (for example, when this transport already has h2 registered), the h1 fallback is unaffected, and it only records
// a log line and returns nil, in which case the standard library default (no active PING) is used.
func enableCodexHTTP2KeepAlive(transport *http.Transport) *http2.Transport {
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		log.Printf("[CodexTransport] enabling HTTP/2 keepalive failed, keeping default (no PING): err=%v", err)
		return nil
	}
	if h2 != nil {
		h2.ReadIdleTimeout = codexHTTP2ReadIdleTimeout
		h2.PingTimeout = codexHTTP2PingTimeout
	}
	return h2
}

// ==================== HTTP connection pool (isolated per account + TTL eviction) ====================
//
// Design points:
//   - Isolated per account: prevents the same TCP connection from being reused by different tokens (which the server can detect)
//   - TTL eviction: only active accounts hold connections; inactive ones are cleaned up automatically, so tens of thousands of accounts will not exhaust memory
//   - Minimal idle connections: each account keeps only 1 idle connection, closed automatically after 30s idle

// poolEntry wraps http.Client and tracks the last-use time for TTL eviction
type poolEntry struct {
	client    *http.Client
	lastUsed  atomic.Int64 // UnixNano timestamp
	createdAt int64        // UnixNano, used for max-age rotation
	// rotatable marks whether this entry can be rotated by age. Only the standard transport sets it: rotation works by
	// "removing from the pool + closing idle connections"; in-flight requests keep running on the old connection until they finish naturally;
	// the uTLS transport manages its own connection pool, and the release path sends GOAWAY for in-flight connections and, after a timeout,
	// forcibly closes them. Periodic rotation by age could truncate long answers, so it does not take part in rotation.
	rotatable bool
}

func (e *poolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

func (e *poolEntry) expiredByAge(now int64, maxAge time.Duration) bool {
	if e == nil || !e.rotatable || maxAge <= 0 || e.createdAt == 0 {
		return false
	}
	return now-e.createdAt > int64(maxAge)
}

var clientPool sync.Map // map[string]*poolEntry, key = accountID|proxyURL|transportMode

// clientPoolTTL: a Client unused for longer than this is evicted
const clientPoolTTL = 5 * time.Minute

// clientPoolMaxAge is the maximum age of a pooled Client: a busy account's connection would never
// sit idle long enough to trigger TTL eviction, and one HTTP/2 connection can carry thousands of requests (the issue #491 report saw
// stream ID 539, meaning the same connection had been reused about 270 times). Upstream edges have
// their own recycling policy for long-lived connections, and in-flight streams get RST when a recycle window is hit. Rotating proactively on schedule staggers this:
// rotation only moves the entry out of the pool and closes its idle connections; new requests use a new connection, and in-flight requests are unaffected.
// The WS path already has a similar mechanism (proactive rotation every 50 minutes, issue #346). Set to 0 to disable this mechanism.
var clientPoolMaxAge = durationFromEnv("CODEX_HTTP_CLIENT_MAX_AGE", 30*time.Minute)

// clientPoolCleanupInterval: interval at which the cleanup goroutine runs
const clientPoolCleanupInterval = 60 * time.Second

func init() {
	// Background cleanup: scan every 60 seconds and evict expired Clients
	go func() {
		ticker := time.NewTicker(clientPoolCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			evictExpiredClients()
		}
	}()
}

func evictExpiredClients() {
	now := time.Now()
	cutoff := now.Add(-clientPoolTTL).UnixNano()
	nowNanos := now.UnixNano()
	clientPool.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		idle := entry.lastUsed.Load() < cutoff
		aged := entry.expiredByAge(nowNanos, clientPoolMaxAge)
		if !idle && !aged {
			return true
		}
		// LoadAndDelete guarantees that only one cleaner actually releases this entry; a concurrent
		// new entry swapped in by getPooledClient is never deleted by mistake.
		if actual, ok := clientPool.LoadAndDelete(key); ok {
			released := actual.(*poolEntry)
			releaseEvictedClient(released.client)
			if aged && !idle {
				log.Printf("[CodexTransport] connection age expired, rotating: key=%s age=%s", key, time.Duration(nowNanos-released.createdAt).Truncate(time.Second))
			}
		}
		return true
	})
}

// releaseEvictedClient fully releases a Client that has been evicted from the pool and will not be taken again.
//
// For a normal transport, CloseIdleConnections is enough (the rest is reclaimed by
// IdleConnTimeout after in-flight requests end). But the uTLS transport manages its own pool, so here it must also drop
// the in-flight connections (in-flight streams close gracefully): otherwise, once the entry is deleted from the map, nobody
// holds that transport any more, and the connections under it leak until the process ends (issue #446).
func releaseEvictedClient(client *http.Client) {
	if client == nil {
		return
	}
	if rt, ok := client.Transport.(*utlsRoundTripper); ok {
		rt.CloseAllConnections()
		return
	}
	client.CloseIdleConnections()
}

const (
	codexTransportModeStandard   = "standard"
	codexTransportModeUTLSChrome = "utls_chrome"
)

func codexTransportModeFromEnv() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TRANSPORT_MODE"))) {
	case "", "standard", "go", "default":
		return codexTransportModeStandard
	case "utls", "utls_chrome", "chrome":
		return codexTransportModeUTLSChrome
	default:
		return codexTransportModeStandard
	}
}

func clientPoolKey(account *auth.Account, proxyURL, transportMode string) string {
	return fmt.Sprintf("%d|%s|%s", account.ID(), strings.TrimSpace(proxyURL), transportMode)
}

func shouldRecyclePooledClient(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection is shutting down") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe")
}

func recyclePooledClient(account *auth.Account, proxyURL string) {
	recyclePooledClientForRoute(account, proxyURL, false)
}

func recyclePooledClientForRoute(account *auth.Account, proxyURL string, accountBound bool) {
	key := clientPoolKey(account, proxyURL, codexTransportModeFromEnv())
	if accountBound {
		key = "account-bound|" + key
	}
	if v, ok := clientPool.LoadAndDelete(key); ok {
		releaseEvictedClient(v.(*poolEntry).client)
	}
}

func recyclePooledClientForAccount(account *auth.Account) {
	if account == nil {
		return
	}

	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	recyclePooledClient(account, proxyURL)
}

// codexTLSSessionCache shares a TLS session cache across all standard transports,
// so reconnects (after pool TTL eviction or a 30s idle close) use TLS resumption(1-RTT), lowering the reconnect handshake cost.
var codexTLSSessionCache = tls.NewLRUClientSessionCache(256)

func newCodexStandardTransport(proxyURL string) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 4
	transport.IdleConnTimeout = 90 * time.Second
	// Guards against the silent-stall case where the upstream does not reply with headers after the connection is established. Response headers (including the SSE
	// 200 header) normally arrive well before the first token, so 5 minutes is very generous.
	transport.ResponseHeaderTimeout = 5 * time.Minute
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ClientSessionCache = codexTLSSessionCache
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
		log.Printf("[CodexTransport] proxy configuration failed, falling back to direct connection: proxy=%s err_type=%T", security.MaskURLCredentials(proxyURL), err)
		transport.Proxy = nil
		transport.DialContext = baseDialer.DialContext
	}
	// Enable HTTP/2 keepalive PING only after the proxy/DialContext is settled, to remove dead connections
	// silently cut off by middleboxes, so requests do not hang until the TCP retransmission timeout.
	enableCodexHTTP2KeepAlive(transport)
	return transport
}

func newCodexTransport(proxyURL string) http.RoundTripper {
	switch codexTransportModeFromEnv() {
	case codexTransportModeUTLSChrome:
		return NewUTLSTransport(proxyURL)
	default:
		return newCodexStandardTransport(proxyURL)
	}
}

func codexFingerprintDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_FINGERPRINT_DEBUG"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func shortHashForLog(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:6])
}

func logCodexFingerprintDebug(kind string, account *auth.Account, proxyURL string, headers http.Header) {
	if !codexFingerprintDebugEnabled() {
		return
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	originator := strings.TrimSpace(headers.Get("Originator"))
	log.Printf("[CodexFingerprint] kind=%s account_id=%d transport_mode=%s proxy_enabled=%t official_client=%t ua_hash=%s originator=%s session_hash=%s stainless_present=%t",
		kind,
		accountID,
		codexTransportModeFromEnv(),
		strings.TrimSpace(proxyURL) != "",
		IsCodexOfficialClientByHeaders(userAgent, originator),
		shortHashForLog(userAgent),
		originator,
		shortHashForLog(headers.Get("Session_id")),
		headers.Get("X-Stainless-Package-Version") != "" ||
			headers.Get("X-Stainless-Runtime-Version") != "" ||
			headers.Get("X-Stainless-Os") != "" ||
			headers.Get("X-Stainless-Arch") != "",
	)
}

// getPooledClient gets or creates an HTTP Client from the connection pool (isolated per account, with TTL auto-eviction)
func getPooledClient(account *auth.Account, proxyURL string) *http.Client {
	return getPooledClientForRoute(account, proxyURL, false)
}

func getPooledClientForRoute(account *auth.Account, proxyURL string, accountBound bool) *http.Client {
	transportMode := codexTransportModeFromEnv()
	key := clientPoolKey(account, proxyURL, transportMode)
	if accountBound {
		key = "account-bound|" + key
	}
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	var transport http.RoundTripper
	if accountBound {
		transport = newAccountBoundTransport(proxyURL)
	} else {
		transport = newCodexTransport(proxyURL)
	}

	entry := &poolEntry{
		createdAt: time.Now().UnixNano(),
		rotatable: transportMode == codexTransportModeStandard,
		client: &http.Client{
			Transport: transport,
			// No overall timeout: http.Client.Timeout covers the full lifetime, including reading the response body, which
			// would cut off a streamed answer over the limit mid-transfer (issue #287,
			// a single turn of a complex task can exceed 10 minutes). The lifetime is controlled by the request context
			// (cancelled when the downstream disconnects); silent-stall cases are covered by the dial timeout and ResponseHeaderTimeout
			// + stream-level disconnect detection as a fallback, consistent with the uTLS path (NewUTLSHttpClient).
			Timeout: 0,
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}

// Codex upstream constants
const (
	CodexBaseURL                     = "https://chatgpt.com/backend-api/codex"
	Originator                       = "codex-tui"
	codexResponsesLiteHeader         = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteWSMetadataPath = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
)

const (
	codexBetaFeaturesHeader = "X-Codex-Beta-Features"
	// defaultCodexBetaFeatures is the session-level feature negotiation value that the real Codex installs by default.
	defaultCodexBetaFeatures = "remote_compaction_v2"
)

var codexAllowedForwardHeaders = []string{
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"X-Codex-Beta-Features",
	codexResponsesLiteHeader,
}

func codexResponsesLiteRequested(requestBody []byte, headers http.Header) bool {
	if headers != nil && strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, codexResponsesLiteWSMetadataPath).String()), "true")
}

// prepareCodexResponsesLiteTransport keeps the request-scoped Responses Lite
// signal intact when codex2api changes the upstream transport. HTTP carries the
// signal in a header; WebSocket carries it on each response.create frame so a
// pooled connection can safely serve both Lite and non-Lite requests.
func prepareCodexResponsesLiteTransport(requestBody []byte, headers http.Header, useWebsocket, enabled bool) ([]byte, http.Header) {
	forwardHeaders := headers
	headersCloned := false
	if headers != nil && headers.Get(codexResponsesLiteHeader) != "" {
		forwardHeaders = headers.Clone()
		headersCloned = true
		forwardHeaders.Del(codexResponsesLiteHeader)
	}
	if useWebsocket {
		if enabled {
			updated, err := sjson.SetBytes(requestBody, codexResponsesLiteWSMetadataPath, "true")
			if err == nil {
				requestBody = updated
			}
		} else {
			// When the signal is stripped by the model gate (or the downstream marked it as not true), clear the leftover marker in the body,
			// so a model that does not support lite does not carry the marker to the WS upstream and trigger a 400.
			requestBody = jsonbody.DeletePath(requestBody, codexResponsesLiteWSMetadataPath)
		}
		return requestBody, forwardHeaders
	}

	requestBody = jsonbody.DeletePath(requestBody, codexResponsesLiteWSMetadataPath)
	if enabled {
		if !headersCloned {
			forwardHeaders = headers.Clone()
		}
		if forwardHeaders == nil {
			forwardHeaders = make(http.Header)
		}
		forwardHeaders.Set(codexResponsesLiteHeader, "true")
	}
	return requestBody, forwardHeaders
}

// normalizeCodexResponsesLiteBody sanitizes the request body against the upstream's mandatory constraints for Responses Lite requests:
// reasoning.context must be all_turns and parallel_tool_calls must be false, otherwise the upstream
// returns 400 unsupported_value (checked by both WS and HTTP). The HTTP upstream also requires that tools contain only function/
// custom/client-executed tool search. The hosted image_generation tool injected by the gateway
// (and its bridge instructions) would get the whole request rejected, so it must be stripped before sending; the WS upstream accepts the tool,
// and the image-generation bridge works there, so it is not stripped, to avoid a feature regression.
func normalizeCodexResponsesLiteBody(requestBody []byte, stripHostedTools bool) []byte {
	requestBody, _ = sjson.SetBytes(requestBody, "parallel_tool_calls", false)
	requestBody, _ = sjson.SetBytes(requestBody, "reasoning.context", "all_turns")
	if !stripHostedTools {
		return requestBody
	}

	if tools := gjson.GetBytes(requestBody, "tools"); tools.IsArray() {
		kept := make([]json.RawMessage, 0, len(tools.Array()))
		removed := false
		for _, tool := range tools.Array() {
			if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
				removed = true
				continue
			}
			kept = append(kept, json.RawMessage(tool.Raw))
		}
		if removed {
			if len(kept) == 0 {
				requestBody, _ = sjson.DeleteBytes(requestBody, "tools")
			} else if raw, err := json.Marshal(kept); err == nil {
				requestBody, _ = sjson.SetRawBytes(requestBody, "tools", raw)
			}
			if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, "tool_choice.type").String()), "image_generation") {
				requestBody, _ = sjson.DeleteBytes(requestBody, "tool_choice")
			}
		}
	}
	if instructions := gjson.GetBytes(requestBody, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", removeCodexImageGenerationBridgeText(instructions))
	}
	return requestBody
}

// WebsocketExecuteFunc is the WebSocket execution function (registered in main.go by the wsrelay package, to avoid an import cycle)
// poolRouteKey: local connection-pool routing key (local only, never sent upstream). When non-empty, wsrelay uses it as the 8-slot pool's
// baseKey, which decouples the upstream session identity (unique per request) from connection reuse (stable per API key); when empty it falls back to
// headerSessionID as the baseKey (the original behavior for explicit sessions and per-api-key mode).
var WebsocketExecuteFunc func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error)

// EnsureCodexAgentIdentityTaskFunc is registered by startup wiring (store.EnsureCodexAgentIdentityTask),
// which ensures the task_id is ready before an Agent Identity request is built; forceRefresh=true is used for a 401 task-invalid re-registration.
// When nil it is skipped (for example, embedded calls or an initialization-order issue).
var EnsureCodexAgentIdentityTaskFunc func(ctx context.Context, account *auth.Account, forceRefresh bool) error

// IsolateCodexSessionID deterministically maps the downstream session seed to an upstream session identity isolated per API key.
//
// The output takes the UUIDv7 shape: the real Codex client's session_id is v7, whereas the original output here was a
// 16-character bare hex string, not even a UUID. It appeared both in the outbound session header and in the
// prompt_cache_key body field, a shape difference more conspicuous than any metadata field.
//
// Changing the format would replace every existing deployment's deterministic cache key, so the first request after an upgrade
// would necessarily miss the upstream prompt cache once and then re-aggregate under the new key. That is a one-time cost, in exchange for the correct shape of every request.
func IsolateCodexSessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || apiKeyID <= 0 {
		return raw
	}
	return DeriveStableSessionUUIDv7(fmt.Sprintf("api-key:%d:%s", apiKeyID, raw))
}

// resolveUpstreamSessionID decides the session/cache identity key passed to the upstream.
//   - Explicit session (the user supplied Session_id/Conversation_id/Idempotency-Key/prompt_cache_key):
//     keeps the deterministic isolation behavior of IsolateCodexSessionID, hitting the cache and sticking to the session.
//   - No explicit session + default isolation: HTTP returns a per-request unique UUID (isolating the upstream prompt_cache_key/
//     Session_id); WS returns "" (handed to the stateless path of ExecuteRequest, with the connection-pool key kept stable separately).
//   - No explicit session + per-api-key: WS returns ""; HTTP uses IsolateCodexSessionID (restores the old per-key sharing).
//
// Note: the account affinity key is derived from requestSessionIdentity.affinityID; this function only receives
// upstreamSeed, so the local affinity header does not affect the upstream identity.
func resolveUpstreamSessionID(apiKeyID int64, upstreamSeed, explicitSessionID string, useWebsocket bool) string {
	if useWebsocket && explicitSessionID == "" {
		return ""
	}
	if explicitSessionID == "" && CurrentRuntimeSettings().IsolateRequestsByDefault() {
		// v7 rather than v4: this is the default path, and the session keys of the vast majority of outbound requests are produced here.
		return NewUpstreamSessionUUID()
	}
	return IsolateCodexSessionID(apiKeyID, upstreamSeed)
}

// ExecuteRequest sends a request to the Codex upstream
// sessionID is optional, used for prompt cache session binding
// useWebsocket is optional: when not passed, the global forced-WS setting applies; when true/false is passed, the caller controls it explicitly.
// headers are the downstream request headers, used for device-fingerprint learning
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (upstreamResponse *http.Response, upstreamErr error) {
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, sessionID, headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()

	// Payload rule rewriting: applied uniformly before the WS/HTTP split, so both upstream paths share the rewritten result.
	// Image-generation requests are skipped: their instructions and tools are built by the gateway itself, and rewriting would break the bridge protocol.
	detectorProbe := isCodexDetectorRequest(ctx)
	if !responsesBodyRequestsImageGeneration(requestBody) && !detectorProbe {
		RecordObservedInstructions(requestBody, headers)
		requestBody = ApplyPayloadRulesToBody(requestBody, gjson.GetBytes(requestBody, "model").String(), headers, PayloadRuleIdentityFromContext(ctx))
		// Rule rewriting happens after each handler's service_tier sanitization, so tiers injected by rules (flex/auto etc.)
		// that the upstream does not accept would be sent as-is and trigger a 400; this adds one more sanitization pass as a safety net. The usage log's
		// requested-tier attribution goes through EffectiveRequestedServiceTier (the value taken before sanitization), so it is unaffected.
		requestBody = sanitizeServiceTierForUpstream(requestBody)
	}
	var daybreakErr error
	requestBody, daybreakErr = guardDaybreakUpstream(ctx, account, requestBody)
	if daybreakErr != nil {
		return nil, daybreakErr
	}
	// Fingerprint convergence rewrites the request body uniformly before the WS/HTTP split, so both upstream paths share the result; on the header side the
	// convergence (ApplyCodexFingerprintHeaders) derives from the same "account + downstream headers", so the values match.
	headers = PrepareCodexFingerprintHeaders(account, headers, requestBody)
	requestBody = ApplyCodexFingerprintToBody(requestBody, account, headers)
	// Account-bound timezone: rewrites the timezone/date in environment_context, handled uniformly before the split just like fingerprint convergence.
	requestBody = ApplyCodexTimezoneToBody(requestBody, account, time.Now())
	// Codex multi-agent v2 compatibility layer: it rewrites the collaboration tool schema only when the caller explicitly installs state, the deployment has the switch turned on,
	// and the downstream really is a Codex client. Requests with conflicts, invalid
	// JSON, or structures over the limit keep the native path; the task body is never guessed.
	if state := codexMultiAgentV2StateFromContext(ctx); state != nil &&
		codexMultiAgentV2CompatEnabled() && isCodexMultiAgentV2Client(headers.Get("User-Agent")) {
		model := gjson.GetBytes(requestBody, "model").String()
		if updated, optimized, err := prepareCodexMultiAgentV2Request(requestBody); err != nil {
			log.Printf("[multi-agent-v2] compatibility layer skipped request rewrite (account=%d model=%s): %v", account.ID(), model, err)
		} else if optimized {
			requestBody = updated
			state.markOptimized(account.ID(), model)
			log.Printf("[multi-agent-v2] parent request switched to plaintext collaboration tools (account=%d model=%s)", account.ID(), model)
		}
	}
	// lite signal convergence: the signature is collected after payload rule rewriting (rules can inject or delete the WS marker; collecting
	// before rewriting would make injections ineffective and deletions get restored), and the model is already finalized by entry mapping and rules: a model known not to support
	// lite that is sent with the signal always gets a 400 upstream, so the signal is stripped before sending.
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)
	wantWebsocket := CurrentRuntimeSettings().CodexForceWebsocket
	if len(useWebsocket) > 0 {
		wantWebsocket = useWebsocket[0]
	}
	// Agent Identity accounts are forced onto HTTP: their auth is a per-request dynamically signed AgentAssertion header,
	// and the one-handshake auth model of long-lived WS connections does not fit, so v1 uses HTTP uniformly.
	if account.IsCodexAgentIdentity() {
		wantWebsocket = false
	}
	var telemetryAttempt *codexTelemetryAttempt
	if !detectorProbe {
		telemetryAttempt = beginCodexTelemetry(codexTelemetryRequest{
			account: account, body: requestBody, sessionID: sessionID, proxyOverride: proxyOverride,
			apiKey: apiKey, deviceCfg: deviceCfg, headers: headers,
		})
	}
	defer func() { telemetryAttempt.observeResult(upstreamResponse, upstreamErr) }()
	poolRouteKey := ""
	if wantWebsocket {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			// The stateless connection ID is used only for WS connection-pool isolation, so concurrent requests with the same API Key
			// do not queue up on a single connection.
			sessionID = statelessWebsocketSessionID()
			if strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String()) == "" {
				det := deterministicPromptCacheKey(apiKey, account)
				if CurrentRuntimeSettings().IsolateRequestsByDefault() {
					// Default isolation: the per-request unique prompt_cache_key is written into the response.create frame body, achieving upstream
					// identity isolation (no cross-contamination); the connection-pool baseKey is passed separately as a stable deterministic key, keeping 8-slot reuse and
					// resistance to handshake rate limiting (503). Note: upstream session isolation relies on the frame-body prompt_cache_key, not on handshake headers
					// Session_id/Conversation_id (the latter is per-connection, not per-request, on a reused connection).
					requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", NewUpstreamSessionUUID())
					poolRouteKey = det
					if poolRouteKey == "" {
						// det is empty only when there is neither an API key nor an account ID (unreachable on the production path); a fixed sentinel is used as a fallback,
						// to avoid degrading baseKey to a per-request unique key, which would trigger a handshake storm.
						poolRouteKey = "ws-pool-default"
					}
				} else if det != "" {
					// per-api-key: keeps the same-source deterministic prompt cache key as the HTTP path (serving as both the upstream identity and the
					// baseKey); otherwise the upstream prompt cache would miss on every request (a regression introduced in v2.2.7).
					requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", det)
				}
			}
		}
	}
	if wantWebsocket && WebsocketExecuteFunc != nil {
		requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, true, responsesLite)
		if responsesLite {
			requestBody = normalizeCodexResponsesLiteBody(requestBody, false)
		}
		requestBody = normalizeCodexStructuredOutputForTransport(requestBody, true, responsesLite)
		// Final safety net before sending: no intermediate rewrite may place a normal input item
		// after a compaction_trigger, otherwise the upstream returns invalid_request_error directly.
		requestBody = normalizeCompactionTriggerFinal(requestBody, false)
		traceProxy := proxyOverride
		if traceProxy == "" {
			account.Mu().RLock()
			traceProxy = account.ProxyURL
			account.Mu().RUnlock()
		}
		recordTrace := beginUpstreamTrace(ctx, account, traceProxy, true)
		resp, err := WebsocketExecuteFunc(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
		recordTrace(resp)
		return resp, err
	}
	if wantWebsocket && WebsocketExecuteFunc == nil {
		// The request or config requires WebSocket, but the WS executor is not registered (for example, embedded calls or an initialization-order issue).
		// Silently falling back to HTTP would make "thinking WS is on but it is actually HTTP" hard to debug, so warn explicitly here.
		log.Printf("[WS] warning: expected the WebSocket upstream, but WebsocketExecuteFunc is not registered; fell back to HTTP (account %d)", account.ID())
	}
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)
	if responsesLite {
		requestBody = normalizeCodexResponsesLiteBody(requestBody, true)
	}
	requestBody = normalizeCodexStructuredOutputForTransport(requestBody, false, responsesLite)
	requestBody = normalizeCompactionTriggerFinal(requestBody, false)

	account.Mu().RLock()
	accessToken := account.AccessToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	// Proxy pool priority: proxyOverride (from NextProxy) > account.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	proxyURL = requestProxyURL(ctx, proxyURL)

	isAgentIdentity := account.IsCodexAgentIdentity()
	// Agent Identity has no access_token; auth relies on AgentAssertion, so the task is ensured registered before the request.
	if isAgentIdentity {
		if EnsureCodexAgentIdentityTaskFunc != nil {
			if err := EnsureCodexAgentIdentityTaskFunc(ctx, account, false); err != nil {
				return nil, ErrUpstream(0, "agent identity task registration failed", err)
			}
		}
	} else if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	// ==================== Codex request body optimization ====================
	// Referenced from CLIProxyAPI/codex_executor.go and sub2api's implementation

	// 1. Ensure the instructions field exists (required by the Codex backend)
	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}

	// 2. Remove extra fields that could make the upstream error
	requestBody = jsonbody.DeletePath(requestBody, "previous_response_id")
	// Note: the HTTP /responses upstream does not accept prompt_cache_retention (it returns 400), so it must be deleted;
	// the cache benefit of this field is only injected on the WS path (see prepareWebsocketBody in wsrelay).
	requestBody = jsonbody.DeletePath(requestBody, "prompt_cache_retention")
	requestBody = jsonbody.DeletePath(requestBody, "safety_identifier")
	requestBody = jsonbody.DeletePath(requestBody, "disable_response_storage")
	// The top-level type is the Responses WS event envelope field (response.create). The native WS ingress
	// 1009 downgrade, forced-HTTP image generation, and forced-HTTP Agent Identity all reuse the enveloped body, and
	// the HTTP /responses upstream does not accept it (400 Unsupported parameter: type). This is the
	// final outbound safety net; sjson deletes only the top-level path, so nested type fields such as in input[] are unaffected (issue #548).
	requestBody = jsonbody.DeletePath(requestBody, "type")
	requestBody = jsonbody.DeletePath(requestBody, "stream_id")

	// 3. Inject prompt_cache_key (if the request body does not have one and sessionID is not empty)
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		requestBody = jsonbody.SetString(requestBody, "prompt_cache_key", cacheKey)
	}

	endpoint := CodexBaseURL + "/responses"

	// The outbound bytes are finalized before the client is selected: send() replays on an Agent Identity 401 re-registration,
	// and the two replays must send the same bytes. Rewriting points that need to read fields, such as routing hints, keep using the plaintext
	// requestBody: they parse JSON, and receiving a compressed frame would silently mismatch.
	var outboundBody []byte
	var contentEncoding string
	if pipelineFromContext(ctx) != nil {
		outboundBody, contentEncoding = compressPipelineRequestBody(requestBody)
	} else {
		outboundBody, contentEncoding = CompressCodexRequestBody(requestBody)
	}

	// The egress chain is decided uniformly by ResolveCodexEgress (Resin > proxy > direct, see egress.go).
	egress := ResolveCodexRequestEgress(ctx, account, endpoint, proxyURL, false)
	endpoint = egress.URL
	client := egress.Client()

	var pipelineFile *os.File
	var routingHeaders http.Header
	requestModel := strings.Clone(gjson.GetBytes(requestBody, "model").String())
	if pipeline := pipelineFromContext(ctx); pipeline != nil {
		var err error
		pipelineFile, err = pipeline.spool(outboundBody)
		if err != nil {
			return nil, ErrInternalError("cannot spool upstream image request", err)
		}
		defer removePipelineFile(pipelineFile)
		routingHeaders = make(http.Header)
		ApplyCodexRoutingHint(routingHeaders, account, requestBody)
		// Queue-generated image requests contain no encrypted input items. Clear
		// transformed and compressed payloads before waiting on the HTTP transport.
		requestBody = nil
		outboundBody = nil
	}
	send := func() (*http.Response, error) {
		var reader io.Reader = bytes.NewReader(outboundBody)
		var input *os.File
		if pipelineFile != nil {
			var err error
			input, err = os.Open(pipelineFile.Name())
			if err != nil {
				return nil, err
			}
			defer input.Close()
			reader = input
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
		if err != nil {
			return nil, ErrInternalError("failed to create request", err)
		}

		// ==================== Request headers (disguised as the Codex CLI) ====================
		if err := applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers); err != nil {
			return nil, err
		}
		// Content-Encoding is set after the common header assembly: real clients also add this header only when encoding finishes
		// (codex-rs/http-client/src/request.rs prepare_encoded_json), and an account's custom headers
		// must not be able to declare an encoding that does not match the actual bytes.
		if contentEncoding != "" {
			req.Header.Set("Content-Encoding", contentEncoding)
		}
		// The routing hint is synthesized by the gateway from the final outbound body, so it must be set after the account's custom headers.
		if pipelineFile != nil {
			stat, err := input.Stat()
			if err != nil {
				return nil, err
			}
			req.ContentLength = stat.Size()
			req.GetBody = func() (io.ReadCloser, error) { return os.Open(pipelineFile.Name()) }
			deleteHeaderCaseInsensitive(req.Header, codexRoutingHintHeader)
			for key, values := range routingHeaders {
				req.Header[key] = append([]string(nil), values...)
			}
		} else {
			ApplyCodexRoutingHint(req.Header, account, requestBody)
		}

		egress.ApplyHeaders(req.Header)
		logCodexFingerprintDebug("http", account, egress.DialProxyURL, req.Header)

		if err := ConsumeAPIKeyModelRequestQuota(ctx, requestModel); err != nil {
			return nil, err
		}
		if pipeline := pipelineFromContext(ctx); pipeline != nil {
			pipeline.Release()
			log.Printf("[image-pipeline] job=%d stage=waiting_upstream request_bytes=%d", pipeline.jobID, req.ContentLength)
		}
		resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
		if err != nil {
			if shouldRecyclePooledClient(err) {
				recycleRequestClient(ctx, account, proxyURL)
			}
			return nil, ErrUpstream(0, "upstream request failed", err)
		}
		return resp, nil
	}

	resp, err := send()
	if err != nil {
		return nil, err
	}

	// Agent Identity: when the task is invalid (401 invalid_task_id etc.), re-register and retry once.
	if isAgentIdentity && resp != nil && resp.StatusCode == http.StatusUnauthorized && EnsureCodexAgentIdentityTaskFunc != nil {
		peeked, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if auth.IsAgentIdentityTaskInvalidResponse(resp.StatusCode, peeked) {
			if regErr := EnsureCodexAgentIdentityTaskFunc(ctx, account, true); regErr == nil {
				return send()
			}
		}
		// Not a task invalidation, or re-registration failed: restore the body that was already read and return it as-is to the upper-layer error handling.
		resp.Body = io.NopCloser(bytes.NewReader(peeked))
	}

	return resp, nil
}

func isCodexAccessRestrictedResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden || resp.Body == nil {
		return false
	}
	// Read only the prefix needed for identification, then splice the prefix and the rest of the stream back together, to avoid
	// reading the whole error body into memory (the upstream may be controlled by a third-party relay).
	const peekLimit = 64 << 10
	peek, err := io.ReadAll(io.LimitReader(resp.Body, peekLimit))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	if err != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(peek, "error.code").String()), "codex_access_restricted") {
		return true
	}
	// Some relays (sub2api's codex_cli_only, zzzcoding and other third-party gateways) use
	// {"error":{"message":"This account only allows Codex official clients",
	// "type":"forbidden_error"}} to express the same official-client restriction.
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(peek, "error.type").String()), "forbidden_error") {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(gjson.GetBytes(peek, "error.message").String()))
	return strings.Contains(msg, "official clients") ||
		strings.Contains(msg, "codex client") ||
		strings.Contains(msg, "codex 客户端")
}

// ExecuteCompactRequest sends a /responses/compact request to the Codex upstream (the non-streaming compaction endpoint)
func ExecuteCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (upstreamResponse *http.Response, upstreamErr error) {
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, sessionID, headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)

	account.Mu().RLock()
	accessToken := account.AccessToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	// Same request body optimization as ExecuteRequest
	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}
	requestBody = jsonbody.DeletePath(requestBody, "previous_response_id")
	// The compact endpoint also goes over HTTP and does not accept prompt_cache_retention, so it must be deleted.
	requestBody = jsonbody.DeletePath(requestBody, "prompt_cache_retention")
	requestBody = jsonbody.DeletePath(requestBody, "safety_identifier")
	requestBody = jsonbody.DeletePath(requestBody, "disable_response_storage")
	// The top-level type is a WS event envelope field; the compact HTTP endpoint does not accept it either, so it is deleted as a fallback (issue #548).
	requestBody = jsonbody.DeletePath(requestBody, "type")
	requestBody = jsonbody.DeletePath(requestBody, "stream_id")
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)
	// Fingerprint convergence: like ExecuteRequest, rewrite client_metadata after the body is finalized and before building the outbound request.
	// If this step is missed, the compact path converges only the headers while the body still carries the client's real identifiers, so
	// the upstream sees a contradiction that a real client never produces: the header says device A, the body says device B.
	// Must use the headers returned after prepareCodexResponsesLiteTransport (it may return a clone),
	// taking the same downstream headers as applyCodexRequestHeaders below, so both derivations match.
	headers = PrepareCodexFingerprintHeaders(account, headers, requestBody)
	requestBody = ApplyCodexFingerprintToBody(requestBody, account, headers)
	requestBody = ApplyCodexTimezoneToBody(requestBody, account, time.Now())
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		requestBody = jsonbody.SetString(requestBody, "prompt_cache_key", cacheKey)
	}

	// compact endpoint
	endpoint := CodexBaseURL + "/responses/compact"

	// The egress chain is decided uniformly by ResolveCodexEgress (Resin > proxy > direct, see egress.go).
	egress := ResolveCodexEgress(account, endpoint, proxyURL)
	endpoint = egress.URL
	client := egress.Client()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, ErrInternalError("failed to create request", err)
	}

	if err := applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers); err != nil {
		return nil, err
	}
	// The routing hint is synthesized by the gateway from the final outbound body, so it must be set after the account's custom headers.
	ApplyCodexRoutingHint(req.Header, account, requestBody)

	egress.ApplyHeaders(req.Header)
	logCodexFingerprintDebug("compact", account, egress.DialProxyURL, req.Header)

	if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(requestBody, "model").String()); err != nil {
		return nil, err
	}
	resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "upstream request failed", err)
	}

	return resp, nil
}

func codexVersionFromProfile(profile deviceProfile, fallback string) string {
	return codexVersionFromUserAgent(profile.UserAgent, fallback)
}

func codexVersionFromUserAgent(userAgent, fallback string) string {
	if _, rawVersion, ok := parseCodexClientVersionDetails(userAgent); ok {
		return rawVersion
	}
	return strings.TrimSpace(fallback)
}

func shouldGenerateCodexClientHeaders(settings RuntimeSettings, userAgent, originator string) bool {
	switch settings.ClientCompatMode {
	case ClientCompatModeForce:
		return true
	case ClientCompatModeAuto:
		_, version, ok := parseCodexClientVersionDetails(userAgent)
		if !ok {
			return false
		}
		floor := firstNonEmptyString(settings.CodexMinCLIVersion, defaultCodexMinCLIVersion)
		cmp, valid := compareCodexClientVersions(version, floor)
		return valid && cmp < 0 && IsCodexStrictOfficialClientByHeaders(userAgent, originator)
	default:
		return false
	}
}

func applyCodexAllowedForwardHeaders(req *http.Request, downstreamHeaders http.Header) {
	if req == nil || downstreamHeaders == nil {
		return
	}
	for _, name := range codexAllowedForwardHeaders {
		if value := strings.TrimSpace(downstreamHeaders.Get(name)); value != "" {
			req.Header.Set(name, value)
		}
	}
}

func applyAccountCustomHeaders(req *http.Request, account *auth.Account) {
	if req == nil || account == nil {
		return
	}
	for name, value := range account.GetCustomHeaders() {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		req.Header.Set(name, value)
	}
}

func applyCodexRequestHeaders(req *http.Request, account *auth.Account, accessToken, cacheKey, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) error {
	if req == nil {
		return nil
	}

	accountID := ""
	if account != nil {
		account.Mu().RLock()
		accountID = account.AccountID
		account.Mu().RUnlock()
	}

	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account, APIKey: apiKey, DeviceConfig: deviceCfg, Headers: downstreamHeaders})
	if err != nil {
		return err
	}
	userAgent, version, usedGeneratedHeaders := identity.UserAgent, identity.Version, identity.Generated
	req.Header.Set("User-Agent", userAgent)

	// Agent Identity accounts use a dynamically signed AgentAssertion header in place of the Bearer (the task is already ensured ready by the caller).
	if account != nil && account.IsCodexAgentIdentity() {
		if assertion, err := account.BuildCodexAgentAssertion(time.Now()); err == nil {
			req.Header.Set("Authorization", assertion)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Do not send Connection: it is a connection-specific header that HTTP/2 explicitly forbids (RFC 9113 §8.2.2),
	// while the official Codex upstream uses h2. Go's http2 transport strips it, but on a proxy chain that cannot strip it,
	// it is an extra header that a real client would never send. Real Codex uses reqwest, which does not send it either.
	if version != "" {
		req.Header.Set("Version", version)
	}
	// Originator must match the client prefix of the outbound UA: when the gateway generates the UA itself, it follows the generated result
	// (when simulating "Codex Desktop", send "Codex Desktop"); when passing through an official client, keep the downstream value.
	if usedGeneratedHeaders {
		req.Header.Set("Originator", CodexOriginatorForGeneratedUserAgent(userAgent))
	} else if originator := strings.TrimSpace(downstreamHeaders.Get("Originator")); originator != "" && IsCodexOfficialClientByHeaders("", originator) {
		req.Header.Set("Originator", originator)
	} else {
		req.Header.Set("Originator", Originator)
	}
	applyCodexAllowedForwardHeaders(req, downstreamHeaders)
	// Session-level beta-features: real Codex sends x-codex-beta-features on every /responses request, the WS handshake and compact.
	// The header defaults to remote_compaction_v2(codex-rs
	// build_model_client_beta_features_header; enabled by default when there are no experimental features). A downstream-declared value is kept as-is.
	// Kept as-is: a non-empty value without v2 means the user explicitly turned it off, so it is not rewritten; when undeclared, the default is added to avoid
	// a pattern where only some requests carry the header, which a real client never produces.
	if strings.TrimSpace(req.Header.Get(codexBetaFeaturesHeader)) == "" {
		value := defaultCodexBetaFeatures
		if deviceCfg != nil && strings.TrimSpace(deviceCfg.BetaFeatures) != "" {
			value = strings.TrimSpace(deviceCfg.BetaFeatures)
		}
		req.Header.Set(codexBetaFeaturesHeader, value)
	}
	// Fingerprint convergence must come after the passthrough whitelist (so it overrides the client's original values), and before the account's custom headers (so an operator's explicit
	// configuration keeps final priority). The off level is a no-op.
	ApplyCodexFingerprintHeaders(req.Header, account, downstreamHeaders)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	// Session identity headers are written in the shape a real client uses (session-id / thread-id / x-client-request-id);
	// when convergence is on, they report the same identity set as the turn metadata. CODEX_SESSION_HEADER_MODE=legacy
	// can revert the whole thing to the old Session_id shape.
	ApplyCodexSessionHeaders(req.Header, account, cacheKey, downstreamHeaders, false)
	applyAccountCustomHeaders(req, account)
	if err := ApplyCodexAttestation(req.Context(), CodexAttestationInput{Headers: req.Header, Account: account}); err != nil {
		return err
	}
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
	return nil
}

const downstreamAffinityHeader = "X-Codex2API-Affinity-Key"

// requestSessionIdentity keeps the local account-routing identity separate
// from the seed used to derive an upstream Session_id/prompt_cache_key. The
// dedicated downstream affinity header may only replace affinityID; it must
// never change upstreamSeed or become an explicit upstream session.
type requestSessionIdentity struct {
	affinityID            string
	upstreamSeed          string
	explicitUpstreamID    string
	hasDownstreamAffinity bool
	hasRequestFingerprint bool
}

// ResolveSessionID extracts or generates a session ID from the downstream request
// Priority:
//  1. Header: X-Codex2API-Affinity-Key (local use only; it is hashed before taking part in binding)
//  2. Header: Session_id
//  3. Header: Conversation_id
//  4. Header: Idempotency-Key
//  5. Header: X-Session-Id / X-Session-Affinity (third-party clients such as opencode)
//  6. Body:   prompt_cache_key
//  7. Body: content-derived seed (model+instructions+system+first user message, see
//     deriveContentSessionSeed; continuation requests that carry previous_response_id are skipped)
//  8. Deterministic UUID based on the Bearer API key
//
// Level 7 lets the multi-turn requests of one conversation converge on the same account affinity key: when a single API key is shared by multiple terminal
// users, stickiness is refined from "the whole key pinned to one account" to "each conversation pinned independently".
// The dedicated affinity header never takes part in the upstream session ID or prompt_cache_key, and it is never forwarded;
// a downstream gateway can use it to pass a stable end-user or conversation identifier, achieving one-user-one-account binding even when the Bearer key is shared.
func ResolveSessionID(headers http.Header, body []byte) string {
	return resolveRequestSessionIdentity(headers, body).affinityID
}

func resolveRequestSessionIdentity(headers http.Header, body []byte) requestSessionIdentity {
	hasEngineFingerprint := EvaluateEngineFingerprint(headers, body, nil)
	explicitID := ResolveExplicitSessionID(headers, body)
	upstreamSeed := explicitID
	if upstreamSeed == "" {
		upstreamSeed = deriveContentSessionSeed(body)
	}
	if upstreamSeed == "" {
		// Generate a deterministic cache key from the downstream user's API key (reference: CLIProxyAPI codex_executor.go:621)
		authHeader := ""
		if headers != nil {
			authHeader = headers.Get("Authorization")
		}
		apiKey := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if apiKey != "" {
			// Must use the same derivation as deterministicPromptCacheKey: the two share the seed string,
			// since different outputs would give the HTTP and WS paths two upstream identities for the same API key.
			upstreamSeed = DeriveStableSessionUUIDv7("codex2api:prompt-cache:" + apiKey)
		}
	}
	if upstreamSeed == "" {
		// Final fallback: the local route and the upstream seed share one random UUID. Use v7: when apiKeyID<=0,
		// IsolateCodexSessionID returns this seed unchanged, so it becomes the outbound session identity directly.
		upstreamSeed = NewUpstreamSessionUUID()
	}

	affinityID := upstreamSeed
	if localAffinityID := resolveDownstreamAffinityID(headers); localAffinityID != "" {
		affinityID = localAffinityID
		return requestSessionIdentity{
			affinityID:            affinityID,
			upstreamSeed:          upstreamSeed,
			explicitUpstreamID:    explicitID,
			hasDownstreamAffinity: true,
			hasRequestFingerprint: true,
		}
	}
	return requestSessionIdentity{
		affinityID:            affinityID,
		upstreamSeed:          upstreamSeed,
		explicitUpstreamID:    explicitID,
		hasRequestFingerprint: hasEngineFingerprint,
	}
}

func resolveDownstreamAffinityID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	raw := strings.TrimSpace(headers.Get(downstreamAffinityHeader))
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codex2api:downstream-affinity:" + raw))
	return "affinity-" + hex.EncodeToString(sum[:16])
}

func ResolveExplicitSessionID(headers http.Header, body []byte) string {
	if headers != nil {
		// Note: Codex CLI sends the hyphenated headers session-id / conversation-id (over HTTP/2 lowercased,
		// normalized by the server to Session-Id / Conversation-Id), which differ from the old underscore form Session_id.
		// Accept both, otherwise the explicit session id cannot be read and affinity falls back to the content seed.
		for _, key := range []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "Idempotency-Key"} {
			if v := strings.TrimSpace(headers.Get(key)); v != "" {
				return v
			}
		}
		// opencode 等第三方 CLI 客户端用 x-session-id / x-session-affinity 标识会话
		// （值为 ses_...，非 UUID，出站上确定性派生为
		// UUIDv7）。优先级低于既有显式头，高于 body prompt_cache_key。
		for _, key := range []string{"X-Session-Id", "X-Session-Affinity"} {
			if v := strings.TrimSpace(headers.Get(key)); v != "" {
				return v
			}
		}
	}
	// When there is no explicit session header, extract it from the body's prompt_cache_key.
	if v := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); v != "" {
		return v
	}

	return ""
}

const statelessWebsocketSessionPrefix = "stateless-"

func statelessWebsocketSessionID() string {
	return statelessWebsocketSessionPrefix + uuid.NewString()
}

// IsStatelessWebsocketSessionID reports whether an ID is a one-off connection ID generated by the WS path.
// Such IDs are used only for connection-pool isolation and must not be sent upstream as a prompt cache key.
func IsStatelessWebsocketSessionID(sessionID string) bool {
	return strings.HasPrefix(sessionID, statelessWebsocketSessionPrefix)
}

// deterministicPromptCacheKey generates a deterministic prompt cache key from the same source as the ResolveSessionID fallback logic:
// it derives from the downstream API key first, and from the account when there is no API key.
//
// The output takes the UUIDv7 shape rather than uuid.NewSHA1's v5: the version nibble of v5 is 5, whereas the real client's
// session_id / prompt_cache_key is always v7, and this difference is directly visible to any
// side that parses the UUID version bits. As with IsolateCodexSessionID, the cost of changing the format is a one-time cache miss.
func deterministicPromptCacheKey(apiKey string, account *auth.Account) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		return DeriveStableSessionUUIDv7("codex2api:prompt-cache:" + apiKey)
	}
	if account != nil {
		if id := account.ID(); id > 0 {
			return DeriveStableSessionUUIDv7(fmt.Sprintf("codex2api:prompt-cache:auth:%d", id))
		}
	}
	return ""
}

// ReadSSEStream reads the event stream from an upstream SSE response
// callback returns true to keep reading and false to stop
func ReadSSEStream(body io.Reader, callback func(data []byte) bool) error {
	return ReadSSEStreamWithEvent(body, func(_ string, data []byte) bool {
		return callback(data)
	})
}

// ReadSSEStreamWithEvent preserves the optional SSE event field while keeping
// ReadSSEStream's data-only API compatible for existing callers.
func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error {
	// Use a sync.Pool to reuse buffers and reduce GC pressure
	buf := sseBufferPool.Get().([]byte)
	defer sseBufferPool.Put(buf)

	lineBufPtr := sseLineBufPool.Get().(*[]byte)
	lineBuf := (*lineBufPtr)[:0]
	defer func() {
		// Cap the capacity on return, so an abnormally large buffer does not stay in the pool for long
		if cap(lineBuf) <= 256*1024 {
			*lineBufPtr = lineBuf[:0]
			sseLineBufPool.Put(lineBufPtr)
		}
	}()

	var dataLines [][]byte
	var eventName string
	var currentEventBytes int

	emitEvent := func() (bool, error) {
		event := eventName
		eventName = ""
		currentEventBytes = 0
		if len(dataLines) == 0 {
			return true, nil
		}

		// Most upstream events have a single data: line, which is handed straight to the callback, avoiding
		// a bytes.Join that copies the payload again for every token event. Multi-line SSE events are merged only.
		data := dataLines[0]
		if len(dataLines) > 1 {
			data = bytes.Join(dataLines, []byte("\n"))
		}
		isDone := bytes.Equal(data, []byte("[DONE]"))
		keepReading := !isDone && callback(event, data)
		// Clear the slice reference in the backing array, so the last large event is not kept
		// in the capacity slots of dataLines until the whole stream ends.
		for i := range dataLines {
			dataLines[i] = nil
		}
		dataLines = dataLines[:0]
		return keepReading, nil
	}

	consumeField := func(line []byte) error {
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimPrefix(data, []byte(" "))
			// Single-line and multi-line accumulated events must both be hard-limited on the wire, to keep oversized events
			// from occupying memory for long. The accounting must strictly match the actual bytes that bytes.Join hands to the callback:
			// the first data line counts len(data), and each subsequent one counts 1 + len(data)
			// (including the 1-byte newline separator). Empty data lines (len(data)==0) from the second one onward
			// each count 1 byte for the separator, and accumulation is also bounded by the hard limit at maxSSEEventBytes,
			// which both prevents unbounded growth and guarantees that a legal event of exactly 8MiB is not wrongly rejected.
			lineBytes := len(data)
			if len(dataLines) > 0 {
				lineBytes++
			}
			if currentEventBytes+lineBytes > maxSSEEventBytes {
				return errSSEEventTooLarge
			}
			currentEventBytes += lineBytes
			// Use copy to avoid memory leaks caused by sharing the underlying array.
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)
			dataLines = append(dataLines, dataCopy)
			return nil
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event := bytes.TrimPrefix(line, []byte("event:"))
			event = bytes.TrimPrefix(event, []byte(" "))
			eventName = string(event)
		}
		return nil
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			lineBuf = append(lineBuf, buf[:n]...)

			// Process by line. Scan using offsets, and move the unfinished line to the head of the buffer in one go at the end;
			// do not repeatedly do lineBuf = lineBuf[idx+1:], since each consumed line would shrink the cap,
			// and the next 64KB Read would almost certainly reallocate, which makes the pooled buffer effectively useless.
			consumed := 0
			for {
				idx := bytes.IndexByte(lineBuf[consumed:], '\n')
				if idx < 0 {
					// No newline: only constrain the length of this unfinished line. Each Read appends at most len(buf) more
					// bytes, so memory growth is bounded (at most the line limit plus one
					// Read), and it does not grow without limit on a huge amount of data with no newline.
					if len(lineBuf)-consumed > maxSSELineBytes {
						return errSSELineTooLong
					}
					break
				}

				lineEnd := consumed + idx
				// The line-length check must come after a newline is found and judge complete lines: this read may
				// exactly complete a legal line close to the limit, followed by other lines or events; if the check is done before
				// append, based on the total of "unfinished buffer + this read", that legal line
				// would be misjudged as over-long.
				if lineEnd-consumed > maxSSELineBytes {
					return errSSELineTooLong
				}
				line := bytes.TrimRight(lineBuf[consumed:lineEnd], "\r")
				consumed = lineEnd + 1

				if len(line) == 0 {
					keep, emitErr := emitEvent()
					if emitErr != nil {
						return emitErr
					}
					if !keep {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				// Parse the SSE event/data fields, supporting standard multi-line data aggregation.
				if err := consumeField(line); err != nil {
					return err
				}
			}

			if consumed > 0 {
				remaining := copy(lineBuf, lineBuf[consumed:])
				lineBuf = lineBuf[:remaining]
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					// Defense in depth: the no-newline branch of the scan loop already caps the unfinished line
					// within maxSSELineBytes, so this is mathematically unreachable; it is only
					// a fallback for future changes.
					if len(lineBuf) > maxSSELineBytes {
						return errSSELineTooLong
					}
					line := bytes.TrimRight(lineBuf, "\r")
					if err := consumeField(line); err != nil {
						return err
					}
				}
				if _, emitErr := emitEvent(); emitErr != nil {
					return emitErr
				}
				return nil
			}
			return err
		}
	}
}

// sseBufferPool reuses SSE read buffers (64KB, to fit the large thinking blocks of reasoning models)
var sseBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 64*1024)
	},
}

// sseLineBufPool reuses line buffers, reducing frequent allocation
var sseLineBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}
