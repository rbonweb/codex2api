package proxy

import (
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
)

// Keep the Codex session identity headers faithful to real clients.
//
// Real clients send three session headers on every HTTP /responses request (codex-rs/codex-api/src/endpoint/
// responses.rs stream_request + requests/headers.rs build_session_headers):
//
//	x-client-request-id: {thread_id}
//	session-id:          {session_id}
//	thread-id:           {thread_id}
//
// The WS handshake has the same shape (codex-rs/core/src/client.rs build_websocket_headers). The gateway previously sent only
// one Session_id: HTTP/2 lowercases the header name to session_id, while the real header is session-id, and these are **two
// different headers**; thread-id and x-client-request-id were missing entirely. In other words, every request that goes through this gateway
// has a session header set that matches no real Codex client, and this difference is present on every request.
//
// Escape hatch: CODEX_SESSION_HEADER_MODE=legacy restores the old Session_id form. The default is native,
// because the old form is a defect rather than a style choice — but if it ever turns out that the upstream actually depends on the underscore spelling,
// a single environment variable can roll back the whole thing without changing code or redeploying.
const (
	codexSessionHeaderModeNative = "native"
	codexSessionHeaderModeLegacy = "legacy"

	codexSessionIDHeader       = "Session-Id"
	codexLegacySessionIDHeader = "Session_id"
	codexConversationIDHeader  = "Conversation_id"
)

// codexSessionHeaderModeFromEnv reads the session header form tier. Only an explicit legacy value falls back,
// every other value (including empty and misspelled ones) is treated as native.
func codexSessionHeaderModeFromEnv() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_MODE")), codexSessionHeaderModeLegacy) {
		return codexSessionHeaderModeLegacy
	}
	return codexSessionHeaderModeNative
}

// codexSessionHeaderAlignsConverged reports whether the outbound session-id header uses the converged session
// identity, aligning it with the session_id in turn metadata / client_metadata.
//
// Off by default, because this would overturn an existing explicit constraint in this repository: the outbound session key belongs only to
// resolveUpstreamSessionID, and convergence does not intervene (see the top of codex_fingerprint.go and
// auth/codex_fingerprint_mode.go). That constraint protects prompt cache isolation: the request body's
// prompt_cache_key is indeed still decided independently by resolveUpstreamSessionID and is not affected by this switch,
// but whether the upstream **also** uses the session-id header in cache grouping cannot be seen from the client source. If it does,
// then once this is enabled every downstream user under the same account would share one cache prefix, which is a privacy and correctness problem,
// not a shape problem.
//
// Benefit of enabling: the real client's session-id header is identical to metadata.session_id
// (core/src/client.rs fills both places with the same session_id). Convergence only changes the metadata side,
// which would make these two places disagree with each other, creating a comparable inconsistency — but this comparison requires the upstream to actively link
// the header and the body, which costs far more than a glance at the UUID version bit. So the default stays on the safe side,
// and the deployer explicitly turns it on after confirming the upstream's caching behavior.
func codexSessionHeaderAlignsConverged() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// ConvergedCodexSessionIdentity returns the converged (session, thread) identity for this request;
// with convergence off (off tier) or when nothing can be derived, it returns an empty string, and the caller falls back to the gateway's own session key.
func ConvergedCodexSessionIdentity(account *auth.Account, downstreamHeaders http.Header) (sessionID, threadID string) {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return "", ""
	}
	return ids.sessionID, ids.threadID
}

// ApplyCodexSessionHeaders writes the outbound session identity headers.
//
// fallbackSessionID is the gateway's own session/cache key (the output of resolveUpstreamSessionID);
// when empty, the legacy mode skips everything. The session identity convergence mode may use an explicit client identity;
// that mode's WS connection pool is also partitioned by the mapped session/thread, so handshakes are not reused across conversations.
//
// legacyConversationID takes effect only in the legacy tier: the WS handshake historically also sent a Conversation_id,
// while the HTTP path explicitly deletes it. The legacy contract is to restore the old behavior path by path, so this difference is declared by the caller,
// rather than choosing one behavior for both paths here. The native tier sends neither on either path: the real client's WS handshake headers
// (build_websocket_headers) contain no such header at all.
//
// Must be called after fingerprint convergence and before the account's custom headers, and it keeps the same priority convention as
// ApplyCodexFingerprintHeaders.
func ApplyCodexSessionHeaders(outbound http.Header, account *auth.Account, fallbackSessionID string, downstreamHeaders http.Header, legacyConversationID bool) {
	if outbound == nil {
		return
	}
	fallbackSessionID = strings.TrimSpace(fallbackSessionID)
	if fallbackSessionID == "" && account != nil && account.EffectiveCodexFingerprintMode() == auth.CodexFingerprintModeSingleMachineMultiWindow {
		fallbackSessionID, _ = ConvergedCodexSessionIdentity(account, downstreamHeaders)
	}
	if fallbackSessionID == "" {
		return
	}

	if codexSessionHeaderModeFromEnv() == codexSessionHeaderModeLegacy {
		if account != nil && account.EffectiveCodexFingerprintMode() == auth.CodexFingerprintModeSingleMachineMultiWindow {
			if aligned, _ := ConvergedCodexSessionIdentity(account, downstreamHeaders); aligned != "" {
				fallbackSessionID = aligned
			}
		}
		outbound.Set(codexLegacySessionIDHeader, fallbackSessionID)
		if legacyConversationID {
			outbound.Set(codexConversationIDHeader, fallbackSessionID)
		} else {
			outbound.Del(codexConversationIDHeader)
		}
		return
	}

	convergedSessionID, threadID := ConvergedCodexSessionIdentity(account, downstreamHeaders)
	if threadID == "" {
		// off/device tiers do not generate a converged thread_id, but in real Codex
		// Thread-Id is still the session semantics that distinguish the parent task from a child agent. Prefer the downstream
		// self-reported value (explicit header first, turn metadata as fallback), and only when it is truly missing
		// fall back to the single-thread session form. The full tier already gave a converged value in the previous step, and it still
		// follows the configuration and intentionally folds multiple threads into one upstream identity.
		_, threadID = extractClientCodexIdentity(downstreamHeaders)
	}

	// The session-id header still defaults to the gateway's own session key: convergence does not intervene in the upstream session identity, which is an existing constraint,
	// and only an explicit opt-in to alignment or selection of the session identity convergence mode switches to the converged value.
	sessionID := fallbackSessionID
	if convergedSessionID != "" && (codexSessionHeaderAlignsConverged() || account.EffectiveCodexFingerprintMode() == auth.CodexFingerprintModeSingleMachineMultiWindow) {
		sessionID = convergedSessionID
	}

	// The thread-id uses the converged value, which is not affected by the constraint above: this header did not exist before, so there is no existing behavior to break,
	// and it must take the same value as x-client-request-id, which ApplyCodexFingerprintHeaders has already
	// converged, so falling back to the unconverged value here would be self-contradictory.
	if threadID == "" {
		// In a real client's single-thread session, thread_id and session_id are the same value
		// (core/src/client.rs fills both with the same identifier), so this fallback is not a fabricated shape.
		threadID = sessionID
	}

	outbound.Set(codexSessionIDHeader, sessionID)
	outbound.Set(codexThreadIDHeader, threadID)
	// x-client-request-id is measured to be identical to thread_id, and real clients always send it. If the downstream has already
	// carries it (via allowlist pass-through plus convergence rewrite), keep its value to avoid overwriting an already-aligned convergence result.
	if strings.TrimSpace(outbound.Get(codexClientRequestIDHeader)) == "" {
		outbound.Set(codexClientRequestIDHeader, threadID)
	}
	// The underscore spelling and Conversation_id are both outside the real shape; under the native tier, clear them,
	// otherwise both the new and old session headers would be sent at once, which is more suspicious than sending only the old one.
	outbound.Del(codexLegacySessionIDHeader)
	outbound.Del(codexConversationIDHeader)
}
