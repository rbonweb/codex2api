package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	lineageContextWindowID = "01a00e75-1111-7542-89bf-35812620690f"
	lineageParentThreadID  = "01a00e75-2222-7542-89bf-35812620690f"
	lineageForkedThreadID  = "01a00e75-3333-7542-89bf-35812620690f"
	lineageClientThreadID  = "01a00e75-8856-7542-89bf-35812620690f"
)

func lineageTurnMetadata() string {
	return `{"installation_id":"341596ee-ab98-43f8-82e2-08ecdfb56db4","session_id":"` + lineageClientThreadID +
		`","thread_id":"` + lineageClientThreadID + `","window_id":"` + lineageClientThreadID +
		`:0","context_window_id":"` + lineageContextWindowID +
		`","parent_thread_id":"` + lineageParentThreadID +
		`","forked_from_thread_id":"` + lineageForkedThreadID +
		`","parent_turn_id":"turn-parent-1","root_turn_id":"turn-root-1",` +
		`"turn_id":"turn-9","turn_started_at_unix_ms":1735689600000,"request_kind":"turn"}`
}

func TestConvergeLineageMetadataReplacesDownstreamIdentities(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	if ids == nil {
		t.Fatal("session tier did not derive a converged identity")
	}

	rewritten, changed := rewriteCodexTurnMetadataJSON(lineageTurnMetadata(), ids)
	if !changed {
		t.Fatal("turn metadata was not rewritten")
	}

	// Session-level and thread-lineage identifiers must be replaced: they stay stable across requests, so keeping them would let the same downstream user's
	// requests be chained together, which would make the convergence of the fields above pointless.
	for path, downstream := range map[string]string{
		"context_window_id":     lineageContextWindowID,
		"parent_thread_id":      lineageParentThreadID,
		"forked_from_thread_id": lineageForkedThreadID,
	} {
		got := gjson.Get(rewritten, path).String()
		if got == downstream {
			t.Fatalf("%s is still the downstream real value %q", path, downstream)
		}
		if got == "" {
			t.Fatalf("%s was deleted; it should be replaced, not removed (keep the shape)", path)
		}
		assertUUIDv7(t, path, got)
	}

	// Turn-level identifiers stay as they are: this matches the repo's existing judgment on turn_id, which changes every turn and does not identify
	// the device or session, and recomputing them would break their consistency with the turn_id / timestamp of the same turn.
	for path, want := range map[string]string{
		"parent_turn_id": "turn-parent-1",
		"root_turn_id":   "turn-root-1",
		"turn_id":        "turn-9",
	} {
		if got := gjson.Get(rewritten, path).String(); got != want {
			t.Fatalf("%s = %q, want %q kept as-is", path, got, want)
		}
	}
	if got := gjson.Get(rewritten, "turn_started_at_unix_ms").Int(); got != 1735689600000 {
		t.Fatalf("turn_started_at_unix_ms = %d, want it kept as-is", got)
	}
}

func TestConvergeLineageMetadataIsDeterministicAndAccountScoped(t *testing.T) {
	first := convergeCodexLineageValue(1, "context_window_id", lineageContextWindowID)
	if second := convergeCodexLineageValue(1, "context_window_id", lineageContextWindowID); second != first {
		t.Fatalf("same input derived different values twice: %q vs %q", first, second)
	}
	// A different account must get a different value, otherwise the upstream could link two accounts to the same downstream user.
	if other := convergeCodexLineageValue(2, "context_window_id", lineageContextWindowID); other == first {
		t.Fatalf("value %q collides across accounts, so the same downstream user could be linked across two accounts", first)
	}
	// The same raw UUID appearing in different fields must not derive the same value either.
	if sameValue := convergeCodexLineageValue(1, "parent_thread_id", lineageContextWindowID); sameValue == first {
		t.Fatalf("value %q collides across fields", first)
	}
}

func TestConvergeLineageMetadataSkippedInDeviceMode(t *testing.T) {
	// The device tier only converges installation_id; session-level fields are never touched.
	account := fingerprintAccount(t, auth.CodexFingerprintModeDevice)
	ids := resolveCodexFingerprintIDs(account, nil)
	if ids == nil {
		t.Fatal("device tier did not derive a converged identity")
	}

	rewritten, _ := rewriteCodexTurnMetadataJSON(lineageTurnMetadata(), ids)
	for path, want := range map[string]string{
		"context_window_id":     lineageContextWindowID,
		"parent_thread_id":      lineageParentThreadID,
		"forked_from_thread_id": lineageForkedThreadID,
	} {
		if got := gjson.Get(rewritten, path).String(); got != want {
			t.Fatalf("device tier rewrote %s: %q, want %q", path, got, want)
		}
	}
}

func TestConvergeLineageMetadataLeavesAbsentKeysAlone(t *testing.T) {
	// Never add fields: a key the client never sent appearing out of nowhere already changes the shape of the metadata.
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	minimal := `{"installation_id":"341596ee-ab98-43f8-82e2-08ecdfb56db4","session_id":"` +
		lineageClientThreadID + `","thread_id":"` + lineageClientThreadID + `"}`

	rewritten, _ := rewriteCodexTurnMetadataJSON(minimal, ids)
	for _, path := range codexLineageMetadataPaths {
		if gjson.Get(rewritten, path).Exists() {
			t.Fatalf("added %s out of nowhere: %s", path, rewritten)
		}
	}
}

func TestParentThreadIDHeaderMatchesMetadata(t *testing.T) {
	// In a real client, x-codex-parent-thread-id and metadata.parent_thread_id are the same
	// snapshot projected twice, so reporting different values in the two places is an easily spotted tell.
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	downstream.Set("Session-Id", lineageClientThreadID)
	downstream.Set("Thread-Id", lineageClientThreadID)
	downstream.Set("X-Codex-Parent-Thread-Id", lineageParentThreadID)
	downstream.Set("Originator", "codex-tui")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	headerValue := outbound.Get("X-Codex-Parent-Thread-Id")
	if headerValue == "" {
		t.Fatal("downstream sent the parent-thread-id header, but it was dropped on the way out")
	}
	if headerValue == lineageParentThreadID {
		t.Fatal("parent-thread-id header is still the downstream real value")
	}
	metadataValue := gjson.Get(outbound.Get("X-Codex-Turn-Metadata"), "parent_thread_id").String()
	if headerValue != metadataValue {
		t.Fatalf("header and metadata disagree: header=%q metadata=%q", headerValue, metadataValue)
	}
}

func TestParentThreadIDHeaderNotFabricated(t *testing.T) {
	// If downstream did not send it, do not add it: fabricating half a client fingerprint on a request that never had the header,
	// is more suspicious than not sending it (this matches the file's existing judgment on the installation-id header).
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	downstream.Set("Session-Id", lineageClientThreadID)
	downstream.Set("Originator", "codex-tui")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	if got := outbound.Get("X-Codex-Parent-Thread-Id"); got != "" {
		t.Fatalf("X-Codex-Parent-Thread-Id = %q, want unset", got)
	}
}

func TestConvergeLineageMetadataPreservesKeyOrder(t *testing.T) {
	// Replace in place with sjson rather than deserializing and re-marshaling: a change in key order is itself a recognizable feature.
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	raw := lineageTurnMetadata()

	rewritten, _ := rewriteCodexTurnMetadataJSON(raw, ids)
	keyOrder := func(doc string) []string {
		var keys []string
		gjson.Parse(doc).ForEach(func(key, _ gjson.Result) bool {
			keys = append(keys, key.String())
			return true
		})
		return keys
	}
	before, after := keyOrder(raw), keyOrder(rewritten)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("key order was changed:\nbefore=%v\nafter =%v", before, after)
	}
}
