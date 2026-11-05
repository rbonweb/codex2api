package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestApplyCodexSessionHeadersNativeShape(t *testing.T) {
	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	// The three real-client session headers (codex-rs/codex-api/src/endpoint/responses.rs).
	for name, want := range map[string]string{
		"Session-Id":          "cache-key",
		"Thread-Id":           "cache-key",
		"X-Client-Request-Id": "cache-key",
	} {
		if got := outbound.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// The underscore spelling and Conversation_id are both outside the real shape. Sending both the new and old sets is more suspicious than sending only the old.
	for _, name := range []string{"Session_id", "Conversation_id"} {
		if got := outbound.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexSessionHeadersClearsLegacyLeftovers(t *testing.T) {
	// An earlier rewrite step in the upstream path may already have written an old header (e.g. an account custom header or historical pass-through); the native tier
	// must clear them, otherwise both sets of session headers would go out together.
	outbound := http.Header{}
	outbound.Set("Session_id", "stale")
	outbound.Set("Conversation_id", "stale")

	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	for _, name := range []string{"Session_id", "Conversation_id"} {
		if got := outbound.Get(name); got != "" {
			t.Fatalf("%s = %q, want cleared", name, got)
		}
	}
}

func TestApplyCodexSessionHeadersPreservesForwardedClientRequestID(t *testing.T) {
	// x-client-request-id, already settled by the whitelist pass-through plus fingerprint convergence, must not be overwritten,
	// otherwise the already-aligned convergence result would be overwritten.
	outbound := http.Header{}
	outbound.Set("X-Client-Request-Id", "converged-thread")

	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	if got := outbound.Get("X-Client-Request-Id"); got != "converged-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want the existing value to be kept", got)
	}
}

func TestApplyCodexSessionHeadersPreservesRawThreadWithoutSessionConvergence(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("Thread-Id", "client-child-thread")

	tests := []struct {
		name    string
		account *auth.Account
	}{
		{name: "off", account: fingerprintAccount(t, auth.CodexFingerprintModeOff)},
		{name: "device", account: fingerprintAccount(t, auth.CodexFingerprintModeDevice)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outbound := http.Header{}
			ApplyCodexSessionHeaders(outbound, tt.account, "gateway-cache-key", downstream, false)
			if got := outbound.Get("Session-Id"); got != "gateway-cache-key" {
				t.Fatalf("Session-Id = %q, want gateway cache key", got)
			}
			if got := outbound.Get("Thread-Id"); got != "client-child-thread" {
				t.Fatalf("Thread-Id = %q, want raw child thread", got)
			}
			if got := outbound.Get("X-Client-Request-Id"); got != "client-child-thread" {
				t.Fatalf("X-Client-Request-Id = %q, want raw child thread", got)
			}
		})
	}
}

func TestApplyCodexSessionHeadersUsesMetadataThreadFallback(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("X-Codex-Turn-Metadata", `{"session_id":"metadata-session","thread_id":"metadata-child"}`)

	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Thread-Id"); got != "metadata-child" {
		t.Fatalf("Thread-Id = %q, want metadata fallback", got)
	}

	downstream.Set("Thread-Id", "header-child")
	outbound = http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Thread-Id"); got != "header-child" {
		t.Fatalf("Thread-Id = %q, want explicit header precedence", got)
	}
}

func TestApplyCodexSessionHeadersFingerprintThreadMatrix(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("Thread-Id", "client-child-thread")

	sessionAccount := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	_, sessionThread := ConvergedCodexSessionIdentity(sessionAccount, downstream)
	if sessionThread == "" {
		t.Fatal("session mode did not derive a thread identity")
	}
	sessionOutbound := http.Header{}
	ApplyCodexSessionHeaders(sessionOutbound, sessionAccount, "gateway-cache-key", downstream, false)
	if got := sessionOutbound.Get("Thread-Id"); got != sessionThread {
		t.Fatalf("session Thread-Id = %q, want converged %q", got, sessionThread)
	}

	fullAccount := fingerprintAccount(t, auth.CodexFingerprintModeFull)
	fullSession, fullThread := ConvergedCodexSessionIdentity(fullAccount, downstream)
	if fullSession == "" || fullThread != fullSession {
		t.Fatalf("full mode identity = (%q, %q), want collapsed non-empty values", fullSession, fullThread)
	}
	fullOutbound := http.Header{}
	ApplyCodexSessionHeaders(fullOutbound, fullAccount, "gateway-cache-key", downstream, false)
	if got := fullOutbound.Get("Thread-Id"); got != fullThread {
		t.Fatalf("full Thread-Id = %q, want collapsed %q", got, fullThread)
	}
}

func TestApplyCodexSessionHeadersSkipsEmptySessionID(t *testing.T) {
	// An empty session key is a legitimate state (with stateless WS plus default isolation, resolveHandshakeSessionID
	// deliberately returns an empty string, so the upstream has no connection-level session identity to bind to). In that case no header should be sent at all.
	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "   ", nil, false)

	if len(outbound) != 0 {
		t.Fatalf("empty session key still wrote headers: %v", outbound)
	}
}

func TestApplyCodexSessionHeadersLegacyModeRestoresOldShape(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "legacy")

	httpHeaders := http.Header{}
	ApplyCodexSessionHeaders(httpHeaders, nil, "cache-key", nil, false)
	if got := httpHeaders.Get("Session_id"); got != "cache-key" {
		t.Fatalf("legacy HTTP Session_id = %q, want %q", got, "cache-key")
	}
	if got := httpHeaders.Get("Conversation_id"); got != "" {
		t.Fatalf("legacy HTTP Conversation_id = %q, want empty (the old HTTP behavior is to delete it)", got)
	}
	if got := httpHeaders.Get("Session-Id"); got != "" {
		t.Fatalf("legacy mode must not write the hyphenated header, got %q", got)
	}

	// The old WS handshake behavior also sent Conversation_id, so the legacy contract is to restore each path individually.
	wsHeaders := http.Header{}
	ApplyCodexSessionHeaders(wsHeaders, nil, "cache-key", nil, true)
	if got := wsHeaders.Get("Conversation_id"); got != "cache-key" {
		t.Fatalf("legacy WS Conversation_id = %q, want %q", got, "cache-key")
	}
}

func TestApplyCodexSessionHeadersConvergedAlignmentIsOptIn(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("Session-Id", "01a00e75-8856-7542-89bf-35812620690f")
	downstream.Set("Thread-Id", "01a00e75-8856-7542-89bf-35812620690f")

	convergedSessionID, convergedThreadID := ConvergedCodexSessionIdentity(account, downstream)
	if convergedSessionID == "" || convergedThreadID == "" {
		t.Fatal("session mode did not derive a converged identity")
	}

	// Default: the outbound session key still belongs to resolveUpstreamSessionID, and convergence does not intervene. This is this repository's
	// existing constraint, which protects prompt cache isolation semantics.
	off := http.Header{}
	ApplyCodexSessionHeaders(off, account, "gateway-cache-key", downstream, false)
	if got := off.Get("Session-Id"); got != "gateway-cache-key" {
		t.Fatalf("default mode Session-Id = %q, want the gateway session key not overwritten by convergence", got)
	}
	// thread-id is not subject to that constraint: this header did not exist before, and it must match the converged
	// x-client-request-id with the same value.
	if got := off.Get("Thread-Id"); got != convergedThreadID {
		t.Fatalf("Thread-Id = %q, want converged %q", got, convergedThreadID)
	}

	// After explicitly enabling it, session-id is aligned with metadata.session_id.
	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "1")
	on := http.Header{}
	ApplyCodexSessionHeaders(on, account, "gateway-cache-key", downstream, false)
	if got := on.Get("Session-Id"); got != convergedSessionID {
		t.Fatalf("after alignment was enabled, Session-Id = %q, want converged %q", got, convergedSessionID)
	}
}
