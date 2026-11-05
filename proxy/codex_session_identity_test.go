package proxy

import (
	"testing"

	"github.com/google/uuid"
)

// assertUUIDv7 asserts that the value is UUIDv7: the session_id / thread_id /
// prompt_cache_key of real Codex clients are all v7; the version nibble and variant bits are directly visible to any side that parses UUIDs.
func assertUUIDv7(t *testing.T, label, value string) {
	t.Helper()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("%s = %q is not a valid UUID: %v", label, value, err)
	}
	if got := parsed.Version(); got != 7 {
		t.Fatalf("%s = %q version = %d, want 7", label, value, got)
	}
	if got := parsed.Variant(); got != uuid.RFC4122 {
		t.Fatalf("%s = %q variant = %v, want RFC4122", label, value, got)
	}
}

func TestDeriveStableSessionUUIDv7IsDeterministicAndV7(t *testing.T) {
	const seed = "codex2api:prompt-cache:some-key"
	first := DeriveStableSessionUUIDv7(seed)
	assertUUIDv7(t, "DeriveStableSessionUUIDv7", first)

	if second := DeriveStableSessionUUIDv7(seed); second != first {
		t.Fatalf("same-seed derivations differ across two calls: %q vs %q (they must also stay constant across process restarts, otherwise every restart would swap in a new batch of upstream identities)", first, second)
	}
	if other := DeriveStableSessionUUIDv7(seed + ":other"); other == first {
		t.Fatalf("different seeds derived a colliding value: %q", other)
	}
}

func TestIsolateCodexSessionIDProducesUUIDv7(t *testing.T) {
	got := IsolateCodexSessionID(7, "downstream-seed")
	assertUUIDv7(t, "IsolateCodexSessionID", got)

	if same := IsolateCodexSessionID(7, "downstream-seed"); same != got {
		t.Fatalf("determinism lost: %q vs %q", got, same)
	}
	if other := IsolateCodexSessionID(8, "downstream-seed"); other == got {
		t.Fatalf("different API keys were not isolated, all got %q", got)
	}
}

func TestIsolateCodexSessionIDLeavesRawWithoutAPIKeyScope(t *testing.T) {
	// apiKeyID<=0 returns the input unchanged: this is the existing contract, and when isolation does not apply, no identity should be made up.
	const raw = "explicit-session"
	if got := IsolateCodexSessionID(0, raw); got != raw {
		t.Fatalf("IsolateCodexSessionID(0, %q) = %q, want returned unchanged", raw, got)
	}
}

func TestDeterministicPromptCacheKeyProducesUUIDv7(t *testing.T) {
	got := deterministicPromptCacheKey("shared-key", nil)
	assertUUIDv7(t, "deterministicPromptCacheKey", got)

	// It shares the seed with the API key fallback in resolveRequestSessionIdentity and must be the same value; otherwise
	// the same API key would get two upstream identities on the HTTP and WS paths.
	if want := DeriveStableSessionUUIDv7("codex2api:prompt-cache:shared-key"); got != want {
		t.Fatalf("deterministicPromptCacheKey = %q, want %q", got, want)
	}
}

func TestNewUpstreamSessionUUIDIsV7AndUnique(t *testing.T) {
	// Default isolation is the path this gateway takes most often: most outbound prompt_cache_key / session-id values are
	// produced by NewUpstreamSessionUUID. This path once sent v4 via uuid.NewString(), and it was caught only when the upstream
	// echoed prompt_cache_key showed wrong version bits (the other two deterministic derivations are already covered separately).
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		got := NewUpstreamSessionUUID()
		assertUUIDv7(t, "NewUpstreamSessionUUID", got)
		if _, dup := seen[got]; dup {
			t.Fatalf("per-request isolated session keys repeated: %q", got)
		}
		seen[got] = struct{}{}
	}
}

func TestResolveUpstreamSessionIDIsolatedModeProducesV7(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	isolated := previous
	isolated.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(isolated)

	// No explicit session + default isolation → unique per request, and it must be v7.
	first := resolveUpstreamSessionID(7, "seed", "", false)
	assertUUIDv7(t, "resolveUpstreamSessionID(isolated)", first)
	if second := resolveUpstreamSessionID(7, "seed", "", false); second == first {
		t.Fatalf("in isolated mode two requests got the same session key %q", first)
	}
}

func TestDeterministicPromptCacheKeyEmptyWithoutIdentity(t *testing.T) {
	if got := deterministicPromptCacheKey("", nil); got != "" {
		t.Fatalf("without an API key or account the result = %q, want empty string", got)
	}
}
