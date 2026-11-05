package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
)

func fingerprintAccount(t *testing.T, mode string) *auth.Account {
	t.Helper()
	return &auth.Account{DBID: 42, CodexFingerprintMode: mode}
}

// codexClientHeaders simulates the downstream request headers of the real Codex CLI: the x-codex- prefixed headers (engine traits)
// and the hyphenated session-id.
func codexClientHeaders(turnMetadata, sessionID string) http.Header {
	headers := http.Header{}
	if turnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", turnMetadata)
	}
	if sessionID != "" {
		headers.Set("Session-Id", sessionID)
	}
	return headers
}

// codexUUIDUnixMilli decodes the millisecond timestamp from the first 48 bits of a v7 UUID, used to assert that the timestamp bits were written correctly.
func codexUUIDUnixMilli(t *testing.T, id string) int64 {
	t.Helper()
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", id, err)
	}
	return int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 | int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
}

// deriveStableCodexUUID now serves only installation_id: the real client's installation identifier is v4.
func TestDeriveStableCodexUUIDIsDeterministicAndV4(t *testing.T) {
	first := deriveStableCodexUUID("seed-a")
	if second := deriveStableCodexUUID("seed-a"); first != second {
		t.Fatalf("deriveStableCodexUUID(%q) = %q then %q, want identical values", "seed-a", first, second)
	}
	if other := deriveStableCodexUUID("seed-b"); other == first {
		t.Fatalf("different seeds produced the same value %q", first)
	}

	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", first, err)
	}
	if parsed.Version() != 4 {
		t.Fatalf("UUID version = %d, want 4 (installation id must match the client's v4 identifier)", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("UUID variant = %v, want RFC4122", parsed.Variant())
	}
}

// TestDeriveStableCodexUUIDv7 locks down the derivation of session/thread/window: the real client's three identifiers
// are UUIDv7, so converged values must also be v7 and their timestamp bits must be written precisely (otherwise sorting by UUID would expose them, see #536).
func TestDeriveStableCodexUUIDv7(t *testing.T) {
	const ts int64 = 1786949015072 // millisecond timestamp at packet-capture magnitude
	first := deriveStableCodexUUIDv7("seed-a", ts)
	if second := deriveStableCodexUUIDv7("seed-a", ts); first != second {
		t.Fatalf("deriveStableCodexUUIDv7(%q) = %q then %q, want identical values", "seed-a", first, second)
	}
	if other := deriveStableCodexUUIDv7("seed-b", ts); other == first {
		t.Fatalf("different seeds produced the same value %q", first)
	}

	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", first, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("UUID version = %d, want 7 (must match the client's v7 session/thread/window)", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("UUID variant = %v, want RFC4122", parsed.Variant())
	}
	if got := codexUUIDUnixMilli(t, first); got != ts {
		t.Fatalf("timestamp bits = %d, want %d written into the leading 48 bits", got, ts)
	}
}

// TestConvergedIdentityUUIDVersionsAndTimestamp runs the full derivation and asserts that session/thread/window
// are v7 with timestamps anchored within the spread window of the account join time, while installation remains v4.
func TestConvergedIdentityUUIDVersionsAndTimestamp(t *testing.T) {
	const addedAtMilli int64 = 1780000000000
	account := &auth.Account{DBID: 42, CodexFingerprintMode: auth.CodexFingerprintModeSession}
	account.AddedAt.Store(addedAtMilli * int64(time.Millisecond))

	ids := resolveCodexFingerprintIDs(account, codexClientHeaders("", "client-session"))
	if ids == nil {
		t.Fatal("session mode returned nil")
	}

	for name, id := range map[string]string{
		"session": ids.sessionID,
		"thread":  ids.threadID,
		"window":  strings.TrimSuffix(ids.windowID, ":0"),
	} {
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("%s id %q not a valid uuid: %v", name, id, err)
		}
		if parsed.Version() != 7 {
			t.Fatalf("%s id version = %d, want 7", name, parsed.Version())
		}
		ms := codexUUIDUnixMilli(t, id)
		if ms < addedAtMilli || ms >= addedAtMilli+codexIdentitySpreadMilli {
			t.Fatalf("%s id timestamp = %d, want within [%d, %d) anchored to account added time",
				name, ms, addedAtMilli, addedAtMilli+codexIdentitySpreadMilli)
		}
	}

	// installation really is v4 and is not affected by the v7 change.
	installation, err := uuid.Parse(ids.installationID)
	if err != nil {
		t.Fatalf("installation id %q not a valid uuid: %v", ids.installationID, err)
	}
	if installation.Version() != 4 {
		t.Fatalf("installation id version = %d, want 4", installation.Version())
	}

	// session and thread seeds differ, so their timestamp bits should spread apart rather than all colliding on the same millisecond.
	if codexUUIDUnixMilli(t, ids.sessionID) == codexUUIDUnixMilli(t, ids.threadID) {
		t.Fatal("session and thread share the same v7 timestamp, want the per-seed spread to separate them")
	}
}

func TestResolveCodexFingerprintIDsPerMode(t *testing.T) {
	headers := codexClientHeaders("", "client-session-1")

	if ids := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeOff), headers); ids != nil {
		t.Fatalf("off mode returned %#v, want nil", ids)
	}
	if ids := resolveCodexFingerprintIDs(nil, headers); ids != nil {
		t.Fatalf("nil account returned %#v, want nil", ids)
	}

	device := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeDevice), headers)
	if device == nil || device.installationID == "" {
		t.Fatalf("device mode ids = %#v, want a converged installation id", device)
	}
	if device.sessionID != "" || device.threadID != "" || device.windowID != "" {
		t.Fatalf("device mode converged session/thread/window (%#v), want installation id only", device)
	}

	session := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), headers)
	if session == nil {
		t.Fatal("session mode returned nil")
	}
	if session.installationID != device.installationID {
		t.Fatalf("installation id = %q, want %q (must not depend on mode)", session.installationID, device.installationID)
	}
	if session.sessionID == "" || session.threadID == "" {
		t.Fatalf("session mode ids = %#v, want session and thread ids", session)
	}
	if session.threadID == session.sessionID {
		t.Fatal("session mode thread id equals session id, want a thread derived from the client session")
	}
	if want := session.threadID + ":0"; session.windowID != want {
		t.Fatalf("window id = %q, want %q", session.windowID, want)
	}

	// Different client sessions derive different threads; the same client session always derives the same thread.
	other := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), codexClientHeaders("", "client-session-2"))
	if other.threadID == session.threadID {
		t.Fatal("distinct client sessions derived the same thread id")
	}
	if other.sessionID != session.sessionID {
		t.Fatalf("session id = %q, want %q (account level constant)", other.sessionID, session.sessionID)
	}
	repeat := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), headers)
	if repeat.threadID != session.threadID {
		t.Fatalf("thread id = %q, want %q (same client session must be stable)", repeat.threadID, session.threadID)
	}

	full := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeFull), headers)
	if full.threadID != full.sessionID {
		t.Fatalf("full mode thread id = %q, want it to equal session id %q", full.threadID, full.sessionID)
	}

	// When the client sends no session header, the session tier falls back to a single thread rather than drifting per request.
	noSession := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), http.Header{})
	if noSession.threadID != noSession.sessionID {
		t.Fatalf("thread id = %q, want fallback to session id %q", noSession.threadID, noSession.sessionID)
	}
}

func TestResolveCodexFingerprintIDsSessionModeKeepsDistinctClientThreads(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	parent := resolveCodexFingerprintIDs(account, codexClientHeaders(
		`{"session_id":"client-session","thread_id":"client-session"}`, "client-session"))
	spawn := resolveCodexFingerprintIDs(account, func() http.Header {
		headers := codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-thread"}`, "client-session")
		headers.Set("Thread-Id", "client-thread")
		return headers
	}())
	if parent == nil || spawn == nil {
		t.Fatal("session mode returned nil")
	}
	if spawn.sessionID != parent.sessionID {
		t.Fatalf("spawn session id = %q, want parent session %q", spawn.sessionID, parent.sessionID)
	}
	if spawn.threadID == parent.threadID {
		t.Fatal("spawn thread collapsed onto the parent session thread")
	}
	if spawn.threadID == spawn.sessionID {
		t.Fatal("spawn thread id equals session id, want a thread derived from the client thread")
	}

	repeat := resolveCodexFingerprintIDs(account, func() http.Header {
		headers := codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-thread"}`, "client-session")
		headers.Set("Thread-Id", "client-thread")
		return headers
	}())
	if repeat.threadID != spawn.threadID {
		t.Fatalf("thread id = %q, want %q (same client thread must be stable)", repeat.threadID, spawn.threadID)
	}
}

func TestResolveCodexFingerprintIDsIsolatesAccounts(t *testing.T) {
	headers := codexClientHeaders("", "client-session-1")
	first := resolveCodexFingerprintIDs(&auth.Account{DBID: 1, CodexFingerprintMode: auth.CodexFingerprintModeSession}, headers)
	second := resolveCodexFingerprintIDs(&auth.Account{DBID: 2, CodexFingerprintMode: auth.CodexFingerprintModeSession}, headers)
	if first.installationID == second.installationID {
		t.Fatal("distinct accounts share an installation id")
	}
	if first.sessionID == second.sessionID {
		t.Fatal("distinct accounts share a session id")
	}
	if first.threadID == second.threadID {
		t.Fatal("distinct accounts share a thread id")
	}
}

// TestResolveCodexFingerprintIDsHonorsPinnedInstallationHeader verifies that the installation id the operator pins in the account's custom headers
// takes priority over the derived value: custom headers override outbound headers anyway, so taking the same value keeps the
// request headers and metadata consistent.
func TestResolveCodexFingerprintIDsHonorsPinnedInstallationHeader(t *testing.T) {
	account := &auth.Account{
		DBID:                 42,
		CodexFingerprintMode: auth.CodexFingerprintModeDevice,
		CustomHeaders:        map[string]string{"x-codex-installation-id": "pinned-device-id"},
	}
	ids := resolveCodexFingerprintIDs(account, http.Header{})
	if ids.installationID != "pinned-device-id" {
		t.Fatalf("installation id = %q, want the pinned custom header value", ids.installationID)
	}
}

func TestApplyCodexFingerprintHeadersRewritesOnlyPresentTurnMetadataKeys(t *testing.T) {
	// Non-identity fields such as sandbox / thread_source must be kept verbatim, and a missing window_id must not be filled in.
	const raw = `{"thread_id":"client-thread","installation_id":"client-install","sandbox":"danger-full-access","session_id":"client-session","turn_id":"client-turn","turn_started_at_unix_ms":1730000000000}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(raw, "client-session")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	got := outbound.Get("X-Codex-Turn-Metadata")
	ids := resolveCodexFingerprintIDs(account, downstream)
	if value := gjson.Get(got, "installation_id").String(); value != ids.installationID {
		t.Fatalf("installation_id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.Get(got, "session_id").String(); value != ids.sessionID {
		t.Fatalf("session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.Get(got, "thread_id").String(); value != ids.threadID {
		t.Fatalf("thread_id = %q, want %q", value, ids.threadID)
	}
	if gjson.Get(got, "window_id").Exists() {
		t.Fatalf("window_id was added to %q, want absent keys left absent", got)
	}
	if value := gjson.Get(got, "sandbox").String(); value != "danger-full-access" {
		t.Fatalf("sandbox = %q, want it preserved", value)
	}
	// turn_id and its timestamp are per-turn random values and do not identify a device; rewriting them would only introduce a header/body inconsistency.
	if value := gjson.Get(got, "turn_id").String(); value != "client-turn" {
		t.Fatalf("turn_id = %q, want the client value preserved", value)
	}
	if value := gjson.Get(got, "turn_started_at_unix_ms").Int(); value != 1730000000000 {
		t.Fatalf("turn_started_at_unix_ms = %d, want the client value preserved", value)
	}
	// Key order must be kept as is: reordering is itself a recognizable trait.
	if wantPrefix := `{"thread_id":"`; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("rewritten metadata = %q, want original key order starting with %q", got, wantPrefix)
	}
}

func TestApplyCodexFingerprintHeadersDeviceModeLeavesSessionFieldsAlone(t *testing.T) {
	const raw = `{"installation_id":"client-install","session_id":"client-session","thread_id":"client-thread"}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeDevice), codexClientHeaders(raw, "client-session"))

	got := outbound.Get("X-Codex-Turn-Metadata")
	if value := gjson.Get(got, "installation_id").String(); value == "client-install" {
		t.Fatal("installation_id was not converged in device mode")
	}
	if value := gjson.Get(got, "session_id").String(); value != "client-session" {
		t.Fatalf("session_id = %q, want the client value untouched in device mode", value)
	}
	if value := gjson.Get(got, "thread_id").String(); value != "client-thread" {
		t.Fatalf("thread_id = %q, want the client value untouched in device mode", value)
	}
	if outbound.Get("X-Codex-Window-Id") != "" {
		t.Fatal("device mode set a window id header")
	}
}

// TestApplyCodexFingerprintHeadersNeverTouchesSessionIDHeader locks the boundary with the existing upstream session isolation logic:
// Session_id is decided by resolveUpstreamSessionID, and convergence must not intervene,
// otherwise the prompt cache isolation semantics would change.
func TestApplyCodexFingerprintHeadersNeverTouchesSessionIDHeader(t *testing.T) {
	outbound := http.Header{}
	outbound.Set("Session_id", "upstream-isolated-id")
	outbound.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session"}`)

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeFull), codexClientHeaders(`{"session_id":"client-session"}`, "client-session"))

	if got := outbound.Get("Session_id"); got != "upstream-isolated-id" {
		t.Fatalf("Session_id = %q, want it left to resolveUpstreamSessionID", got)
	}
	if got := outbound.Get("Thread-Id"); got != "" {
		t.Fatalf("Thread-Id = %q, want unset (not part of this project's outbound header set)", got)
	}
}

// TestApplyCodexFingerprintHeadersSkipsNonCodexClients verifies that x-codex-* headers are not fabricated for downstream requests
// that lack Codex engine traits: a half-set of traits is more suspicious than sending none.
func TestApplyCodexFingerprintHeadersSkipsNonCodexClients(t *testing.T) {
	outbound := http.Header{}
	plainSDKHeaders := http.Header{"User-Agent": []string{"OpenAI/Python 1.0"}}

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeSession), plainSDKHeaders)

	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset for a non-Codex downstream", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != "" {
		t.Fatalf("X-Codex-Window-Id = %q, want unset for a non-Codex downstream", got)
	}
}

// TestApplyCodexFingerprintHeadersNeverAddsAbsentIdentityHeaders locks in "overwrite only, never add" for request headers as well.
// Measurements show the real Codex CLI only sends x-codex-window-id and never sends
// x-codex-installation-id (that value exists only inside the turn metadata JSON); adding it out of thin air would give the request
// a header the real client would never have.
func TestApplyCodexFingerprintHeadersNeverAddsAbsentIdentityHeaders(t *testing.T) {
	outbound := http.Header{}
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(`{"installation_id":"client-install"}`, "client-session")
	outbound.Set("X-Codex-Turn-Metadata", downstream.Get("X-Codex-Turn-Metadata"))

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset (downstream never sent it)", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != "" {
		t.Fatalf("X-Codex-Window-Id = %q, want unset (downstream never sent it)", got)
	}
	// installation_id inside metadata should still converge: the convergence surface is in the JSON anyway.
	if got := gjson.Get(outbound.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got == "client-install" {
		t.Fatal("metadata installation_id was left at the client value")
	}
}

// TestApplyCodexFingerprintHeadersOverridesPresentIdentityHeaders verifies that when the downstream did send a header,
// the outbound value is overridden by the converged value (the real CLI does send the window id; this is its main path).
func TestApplyCodexFingerprintHeadersOverridesPresentIdentityHeaders(t *testing.T) {
	outbound := http.Header{}
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(`{"installation_id":"client-install"}`, "client-session")
	downstream.Set("X-Codex-Window-Id", "client-window:0")
	downstream.Set("X-Codex-Installation-Id", "client-install")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	ids := resolveCodexFingerprintIDs(account, downstream)
	if got := outbound.Get("X-Codex-Installation-Id"); got != ids.installationID {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, ids.installationID)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != ids.windowID {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, ids.windowID)
	}
}

func TestApplyCodexFingerprintOffModeIsNoop(t *testing.T) {
	const raw = `{"installation_id":"client-install","session_id":"client-session"}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	account := fingerprintAccount(t, auth.CodexFingerprintModeOff)
	downstream := codexClientHeaders(raw, "client-session")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)
	if got := outbound.Get("X-Codex-Turn-Metadata"); got != raw {
		t.Fatalf("turn metadata = %q, want %q unchanged", got, raw)
	}
	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	body := []byte(`{"client_metadata":{"x-codex-installation-id":"client-install"}}`)
	if got := ApplyCodexFingerprintToBody(body, account, downstream); string(got) != string(body) {
		t.Fatalf("body = %s, want %s unchanged", got, body)
	}
}

func TestApplyCodexFingerprintToBodyRewritesClientMetadata(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders("", "client-session")
	ids := resolveCodexFingerprintIDs(account, downstream)
	body := []byte(`{"model":"gpt-5.6-codex","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0","x-codex-turn-metadata":"{\"installation_id\":\"client-install\",\"session_id\":\"client-session\",\"sandbox\":\"read-only\"}"}}`)

	got := ApplyCodexFingerprintToBody(body, account, downstream)

	if value := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); value != ids.installationID {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.GetBytes(got, "client_metadata.session_id").String(); value != ids.sessionID {
		t.Fatalf("client_metadata.session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.GetBytes(got, "client_metadata.thread_id").String(); value != ids.threadID {
		t.Fatalf("client_metadata.thread_id = %q, want %q", value, ids.threadID)
	}
	if value := gjson.GetBytes(got, "client_metadata.x-codex-window-id").String(); value != ids.windowID {
		t.Fatalf("client_metadata.x-codex-window-id = %q, want %q", value, ids.windowID)
	}
	if value := gjson.GetBytes(got, "model").String(); value != "gpt-5.6-codex" {
		t.Fatalf("model = %q, want it preserved", value)
	}

	embedded := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String()
	if value := gjson.Get(embedded, "installation_id").String(); value != ids.installationID {
		t.Fatalf("embedded installation_id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.Get(embedded, "session_id").String(); value != ids.sessionID {
		t.Fatalf("embedded session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.Get(embedded, "sandbox").String(); value != "read-only" {
		t.Fatalf("embedded sandbox = %q, want it preserved", value)
	}
}

func TestApplyCodexFingerprintToBodyLeavesBodiesWithoutClientMetadata(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeFull)
	downstream := codexClientHeaders("", "client-session")

	body := []byte(`{"model":"gpt-5.6-codex","input":[]}`)
	if got := ApplyCodexFingerprintToBody(body, account, downstream); string(got) != string(body) {
		t.Fatalf("body = %s, want %s unchanged", got, body)
	}

	// client_metadata that exists but has no identity fields must not gain fields.
	partial := []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	if got := ApplyCodexFingerprintToBody(partial, account, downstream); string(got) != string(partial) {
		t.Fatalf("body = %s, want %s unchanged", got, partial)
	}
}

// TestCodexFingerprintHeaderAndBodyAgree is the core invariant of this rewrite: the request headers and request body are
// derived at two independent rewrite points, and must land on exactly the same set of identifiers.
func TestCodexFingerprintHeaderAndBodyAgree(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	const raw = `{"installation_id":"client-install","session_id":"client-session","thread_id":"client-thread","window_id":"client-window:0"}`
	downstream := codexClientHeaders(raw, "client-session")
	// Identifier headers are overwritten only when the downstream sent them; this fills them in here so the header/metadata consistency can be asserted.
	downstream.Set("X-Codex-Installation-Id", "client-install")
	downstream.Set("X-Codex-Window-Id", "client-window:0")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	body := ApplyCodexFingerprintToBody(
		[]byte(`{"client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0"}}`),
		account, downstream,
	)

	headerMetadata := outbound.Get("X-Codex-Turn-Metadata")
	pairs := []struct {
		name       string
		headerPath string
		bodyPath   string
	}{
		{"installation id", "installation_id", "client_metadata.x-codex-installation-id"},
		{"session id", "session_id", "client_metadata.session_id"},
		{"thread id", "thread_id", "client_metadata.thread_id"},
		{"window id", "window_id", "client_metadata.x-codex-window-id"},
	}
	for _, pair := range pairs {
		fromHeader := gjson.Get(headerMetadata, pair.headerPath).String()
		fromBody := gjson.GetBytes(body, pair.bodyPath).String()
		if fromHeader == "" {
			t.Fatalf("%s missing from rewritten header metadata %q", pair.name, headerMetadata)
		}
		if fromHeader != fromBody {
			t.Fatalf("%s: header = %q, body = %q, want identical values", pair.name, fromHeader, fromBody)
		}
	}
	if got := outbound.Get("X-Codex-Installation-Id"); got != gjson.Get(headerMetadata, "installation_id").String() {
		t.Fatalf("X-Codex-Installation-Id = %q, want it to match the metadata installation id", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != gjson.Get(headerMetadata, "window_id").String() {
		t.Fatalf("X-Codex-Window-Id = %q, want it to match the metadata window id", got)
	}
}

// TestApplyCodexFingerprintHeadersIgnoresInvalidTurnMetadata verifies that illegal JSON sent by the client
// passes through verbatim and a failed rewrite does not break the request.
func TestApplyCodexFingerprintHeadersIgnoresInvalidTurnMetadata(t *testing.T) {
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", "not-json")

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeSession), codexClientHeaders("not-json", "client-session"))

	if got := outbound.Get("X-Codex-Turn-Metadata"); got != "not-json" {
		t.Fatalf("turn metadata = %q, want it passed through unchanged", got)
	}
}

// The compact path previously converged only the request headers and sent the request body client_metadata verbatim, so the upstream would see
// the contradiction of "header says device A, body says device B", which a real client would never have (it also made the convergence meaningless).
func TestExecuteCompactRequestConvergesBodyAndHeaders(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	account.AccessToken = "token-compact"
	// Only a request carrying turn metadata counts as a real Codex client (see the EvaluateEngineFingerprint gate in
	// ApplyCodexFingerprintHeaders); identifier headers are still overwritten only when the downstream actually sent them.
	downstream := codexClientHeaders(`{"installation_id":"client-install","session_id":"client-session"}`, "client-session")
	downstream.Set("X-Codex-Installation-Id", "client-install")
	ids := resolveCodexFingerprintIDs(account, downstream)
	if ids == nil {
		t.Fatal("expected convergence ids for session mode")
	}

	var capturedBody []byte
	var capturedHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody = readUpstreamRequestBody(r)
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// Route the outbound request through the Resin reverse proxy to a local server, so the bytes actually sent can be asserted.
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))

	body := []byte(`{"model":"gpt-5.6-codex","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0"}}`)
	resp, err := ExecuteCompactRequest(context.Background(), account, body, "", "", "api-key-1", nil, downstream)
	if err != nil {
		t.Fatalf("ExecuteCompactRequest: %v", err)
	}
	_ = resp.Body.Close()

	for _, tc := range []struct{ path, want string }{
		{"client_metadata.x-codex-installation-id", ids.installationID},
		{"client_metadata.session_id", ids.sessionID},
		{"client_metadata.thread_id", ids.threadID},
		{"client_metadata.x-codex-window-id", ids.windowID},
	} {
		if got := gjson.GetBytes(capturedBody, tc.path).String(); got != tc.want {
			t.Fatalf("outbound %s = %q, want converged %q", tc.path, got, tc.want)
		}
	}
	// Headers and body must come from the same derivation, otherwise the contradiction is itself a trait.
	if got := capturedHeader.Get(codexInstallationIDHeader); got != ids.installationID {
		t.Fatalf("outbound %s = %q, want %q", codexInstallationIDHeader, got, ids.installationID)
	}
	if got := gjson.GetBytes(capturedBody, "client_metadata.x-codex-installation-id").String(); got != capturedHeader.Get(codexInstallationIDHeader) {
		t.Fatalf("header/body installation id disagree: body=%q header=%q", got, capturedHeader.Get(codexInstallationIDHeader))
	}
}

// TestConvergedClientRequestIDFollowsClientIdentity covers the main complaint of issue #536:
// measurements show the real client sets X-Client-Request-Id to the same value as its own thread_id, so convergence must follow it,
// otherwise the original thread identifier would bypass the metadata rewrite and go straight to the upstream.
func TestConvergedClientRequestIDFollowsClientIdentity(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)

	t.Run("matches thread id", func(t *testing.T) {
		const raw = `{"session_id":"client-session","thread_id":"client-thread"}`
		downstream := codexClientHeaders(raw, "client-session")
		downstream.Set("Thread-Id", "client-thread")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-thread") // result of the allowlist's verbatim passthrough
		outbound.Set("X-Codex-Turn-Metadata", raw)

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		ids := resolveCodexFingerprintIDs(account, downstream)
		if got := outbound.Get("X-Client-Request-Id"); got != ids.threadID {
			t.Fatalf("X-Client-Request-Id = %q, want converged thread id %q", got, ids.threadID)
		}
		parent := resolveCodexFingerprintIDs(account, codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-session"}`, "client-session"))
		if got := outbound.Get("X-Client-Request-Id"); got == parent.threadID {
			t.Fatal("X-Client-Request-Id collapsed onto the parent session thread")
		}
	})

	t.Run("matches session id", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-session")

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		ids := resolveCodexFingerprintIDs(account, downstream)
		if got := outbound.Get("X-Client-Request-Id"); got != ids.sessionID {
			t.Fatalf("X-Client-Request-Id = %q, want converged session id %q", got, ids.sessionID)
		}
	})

	// When it matches none of the self-reported identities, it is confirmed to be a random per-request value, and rewriting it would only create an inconsistency.
	t.Run("leaves unrelated values alone", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "per-request-random-value")

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		if got := outbound.Get("X-Client-Request-Id"); got != "per-request-random-value" {
			t.Fatalf("X-Client-Request-Id = %q, want the client value preserved", got)
		}
	})

	// The device tier does not converge session-level identifiers, so this header should not be touched either.
	t.Run("device mode is a noop", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-session")

		ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeDevice), downstream)

		if got := outbound.Get("X-Client-Request-Id"); got != "client-session" {
			t.Fatalf("X-Client-Request-Id = %q, want untouched in device mode", got)
		}
	})
}

// TestScrubCodexWorkspacesRemovesWorkspaceIdentity verifies that workspace identity is removed: local absolute
// paths contain the system username and associated_remote_urls contain repository ownership, so when accounts are shared by several people these would send each
// downstream user's identity verbatim to the upstream.
func TestScrubCodexWorkspacesRemovesWorkspaceIdentity(t *testing.T) {
	// commit hash uses a value of real length: it is a globally unique repository marker, and a public repository can be traced back to its owner from it,
	// it is the same identity leaked as the remote URL.
	const originalCommit = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	const raw = `{"request_kind":"memory","workspaces":{"/Users/alice/code/secret-project":{"associated_remote_urls":{"origin":"https://github.com/alice-corp/secret-project.git"},"latest_git_commit_hash":"` + originalCommit + `","has_changes":false}}}`

	got, changed := scrubCodexWorkspaces(raw, 42)
	if !changed {
		t.Fatal("scrubCodexWorkspaces reported no change, want the workspace scrubbed")
	}
	for _, leaked := range []string{"alice", "secret-project", "github.com", originalCommit} {
		if strings.Contains(got, leaked) {
			t.Fatalf("scrubbed metadata %q still contains %q", got, leaked)
		}
	}

	workspaces := gjson.Get(got, "workspaces")
	if n := len(workspaces.Map()); n != 1 {
		t.Fatalf("workspace entry count = %d, want 1 (shape must be preserved)", n)
	}
	for path, entry := range workspaces.Map() {
		if !strings.HasPrefix(path, "/workspace/") {
			t.Fatalf("workspace key = %q, want a derived placeholder path", path)
		}
		if entry.Get("associated_remote_urls").Exists() {
			t.Fatalf("associated_remote_urls survived in %q", entry.Raw)
		}
		// Shape is preserved: the field is still there, and it is still a hex string of the same length.
		if replaced := entry.Get("latest_git_commit_hash").String(); len(replaced) != len(originalCommit) {
			t.Fatalf("latest_git_commit_hash = %q, want a same-length derived placeholder", replaced)
		}
		if entry.Get("has_changes").Bool() {
			t.Fatal("has_changes = true, want the client value preserved")
		}
	}

	// The same path always maps to the same placeholder, and different paths are distinct from each other.
	repeat, _ := scrubCodexWorkspaces(raw, 42)
	if repeat != got {
		t.Fatalf("scrub is not deterministic: %q vs %q", repeat, got)
	}
	if placeholderWorkspacePath(42, "/Users/alice/a") == placeholderWorkspacePath(42, "/Users/alice/b") {
		t.Fatal("distinct workspace paths collapsed to the same placeholder")
	}

	// Account-level isolation: the same path under different accounts must derive different placeholders, otherwise the upstream could
	// link the same downstream user across accounts.
	if placeholderWorkspacePath(42, "/Users/alice/a") == placeholderWorkspacePath(43, "/Users/alice/a") {
		t.Fatal("same path produced the same placeholder across accounts")
	}
	otherAccount, _ := scrubCodexWorkspaces(raw, 43)
	if otherAccount == got {
		t.Fatal("scrub result is identical across accounts, want account-scoped derivation")
	}

	// The scrub must be idempotent: a retry path may send a rewritten payload in a second time.
	if again, changedAgain := scrubCodexWorkspaces(got, 42); changedAgain || again != got {
		t.Fatalf("second scrub changed the payload again: %q -> %q", got, again)
	}

	// Metadata without workspaces is not affected.
	if _, changed := scrubCodexWorkspaces(`{"session_id":"s"}`, 42); changed {
		t.Fatal("metadata without workspaces was modified")
	}
}

// TestCodexFingerprintLeavesNoOriginalIdentifierOutbound is the consistency regression required by issue #536: it runs the full rewrite chain with a real captured request shape, and asserts that no original identifier remains in the outbound headers.
// Regression: runs the full rewrite chain with a request shape from a real packet capture, and asserts that no original identifier is left in the outbound headers.
// If a future protocol adds an identity field that is missed, this test fails first.
func TestCodexFingerprintLeavesNoOriginalIdentifierOutbound(t *testing.T) {
	const (
		clientSessionUUID = "01a00e75-8856-7542-89bf-35812620690f"
		clientInstallUUID = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath     = "/Users/kyx/code_project/codex2api"
		remoteURL         = "https://github.com/james-6-23/codex2api.git"
		commitHash        = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	)
	rawMetadata := `{"installation_id":"` + clientInstallUUID + `","session_id":"` + clientSessionUUID +
		`","thread_id":"` + clientSessionUUID + `","turn_id":"01a00e76-15fb-7940-91a0-e201c45f502a","window_id":"` +
		clientSessionUUID + `:0","request_kind":"turn","sandbox":"none","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"latest_git_commit_hash":"` + commitHash + `"}},"turn_started_at_unix_ms":1786949015072}`

	// Replicates the set of downstream headers from the real CLI.
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", rawMetadata)
	downstream.Set("Session-Id", clientSessionUUID)
	downstream.Set("Thread-Id", clientSessionUUID)
	downstream.Set("X-Client-Request-Id", clientSessionUUID)
	downstream.Set("X-Codex-Window-Id", clientSessionUUID+":0")

	// Replicates the result of the gateway's allowlist passthrough (see codexAllowedForwardHeaders).
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", rawMetadata)
	outbound.Set("X-Client-Request-Id", clientSessionUUID)

	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ApplyCodexFingerprintHeaders(outbound, account, downstream)
	body := ApplyCodexFingerprintToBody(
		[]byte(`{"client_metadata":{"x-codex-installation-id":"`+clientInstallUUID+`","session_id":"`+clientSessionUUID+
			`","thread_id":"`+clientSessionUUID+`","x-codex-window-id":"`+clientSessionUUID+`:0"}}`),
		account, downstream,
	)

	var outboundDump strings.Builder
	for name, values := range outbound {
		for _, value := range values {
			outboundDump.WriteString(name + ": " + value + "\n")
		}
	}
	outboundDump.Write(body)

	// turn_id is not in this list: it is a per-turn random value and does not identify a device or session, so rewriting it would only introduce an inconsistency.
	for _, leaked := range []string{clientSessionUUID, clientInstallUUID, workspacePath, remoteURL, "james-6-23", commitHash} {
		if strings.Contains(outboundDump.String(), leaked) {
			t.Fatalf("original identifier %q survived into the outbound request:\n%s", leaked, outboundDump.String())
		}
	}
}

// TestApplyCodexRequestHeadersConvergesForwardedClientRequestID runs the real outbound header assembly
// order (allowlist passthrough, then fingerprint convergence, then account custom headers), rather than calling the convergence function alone.
// The bug in issue #536 was in exactly this order: X-Client-Request-Id is passed through verbatim by the allowlist first,
// and if convergence does not take over, the original thread identifier goes straight to the upstream.
func TestApplyCodexRequestHeadersConvergesForwardedClientRequestID(t *testing.T) {
	const (
		clientUUID    = "01a00e75-8856-7542-89bf-35812620690f"
		installUUID   = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath = "/Users/kyx/code_project/codex2api"
		remoteURL     = "https://github.com/james-6-23/codex2api.git"
	)
	rawMetadata := `{"installation_id":"` + installUUID + `","session_id":"` + clientUUID +
		`","thread_id":"` + clientUUID + `","window_id":"` + clientUUID +
		`:0","request_kind":"turn","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"has_changes":false}}}`

	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", rawMetadata)
	downstream.Set("Session-Id", clientUUID)
	downstream.Set("Thread-Id", clientUUID)
	downstream.Set("X-Client-Request-Id", clientUUID)
	downstream.Set("X-Codex-Window-Id", clientUUID+":0")
	downstream.Set("Originator", "codex-tui")

	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(req, account, "access-token", "upstream-cache-key", "api-key-1", nil, downstream)

	ids := resolveCodexFingerprintIDs(account, downstream)
	if got := req.Header.Get("X-Client-Request-Id"); got != ids.threadID {
		t.Fatalf("X-Client-Request-Id = %q, want converged thread id %q", got, ids.threadID)
	}
	// The outbound session key still belongs to resolveUpstreamSessionID, and convergence must not intervene by default
	// (aligning requires explicitly enabling CODEX_SESSION_HEADER_ALIGN_CONVERGED). The header name is changed to the real form,
	// but the value semantics are unchanged.
	if got := req.Header.Get("Session-Id"); got != "upstream-cache-key" {
		t.Fatalf("Session-Id = %q, want the cache key untouched", got)
	}
	// thread-id and x-client-request-id must be the same value: the latter has already been rewritten by convergence, so if the former falls back to
	// the unconverged value, they would contradict each other.
	if got := req.Header.Get("Thread-Id"); got != ids.threadID {
		t.Fatalf("Thread-Id = %q, want converged thread id %q", got, ids.threadID)
	}
	// The downstream sent no installation header, so none should be outbound either.
	if got := req.Header.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	var dump strings.Builder
	for name, values := range req.Header {
		for _, value := range values {
			dump.WriteString(name + ": " + value + "\n")
		}
	}
	for _, leaked := range []string{clientUUID, installUUID, workspacePath, remoteURL, "james-6-23"} {
		if strings.Contains(dump.String(), leaked) {
			t.Fatalf("original identifier %q survived the real header pipeline:\n%s", leaked, dump.String())
		}
	}
}
