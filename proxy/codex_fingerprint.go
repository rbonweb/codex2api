package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/codex2api/auth"
)

// Codex device fingerprint convergence: rewrite the client identifiers carried in outbound requests into account-level constant values,
// so that the "device count / session count" seen by the upstream converges, instead of growing with the number of downstream users sharing the account.
// For the semantics of each tier, see auth/codex_fingerprint_mode.go.
//
// single_machine_multi_window preserves real conversation/subagent topology and
// aligns session/thread headers. See codex_fingerprint_modes.go. No fixed window
// pool or fabricated subagents are used; existing turn IDs/timestamps survive.
// The following historical constraints describe the original four modes.
// Convergence is strictly limited to two carriers:
//   - The JSON in the X-Codex-Turn-Metadata request header (this project passes it through verbatim per the allowlist, see
//     codexAllowedForwardHeaders; this is where the client's real identifiers leak)
//   - The client_metadata in the request body (the official Codex path also passes it through verbatim)
//
// Explicitly not rewritten:
//   - The outbound Session_id header: decided independently by resolveUpstreamSessionID (in default isolation mode, per request
//     random), convergence does not touch it, otherwise prompt cache isolation semantics would change. Note that it and the metadata's
//     session_id are two separate identities; it is expected that their values differ once convergence is enabled.
//   - turn_id / turn_started_at_unix_ms: per-turn random values from the client; they do not identify a device or session;
//     recomputing them would make the header, the body, and turn_id and its timestamp inconsistent with each other, which is itself a fingerprint.
//   - Fields the client did not send: only existing keys are rewritten, never added, so the shape of metadata does not change.
//     This rule applies equally to request headers: outbound only overwrites identifier headers the downstream actually sent, and never fabricates them.

const (
	codexTurnMetadataHeader    = "X-Codex-Turn-Metadata"
	codexInstallationIDHeader  = "X-Codex-Installation-Id"
	codexWindowIDHeader        = "X-Codex-Window-Id"
	codexClientRequestIDHeader = "X-Client-Request-Id"
	codexThreadIDHeader        = "Thread-Id"
	codexParentThreadIDHeader  = "X-Codex-Parent-Thread-Id"
)

// codexFingerprintIDs is the set of convergence target values for one request. All fields are derived deterministically from "account + downstream request headers",
// so the request header rewrite point and the request body rewrite point each derive them and necessarily get the same set of values,
// guaranteeing consistency without passing anything across functions.
type codexFingerprintIDs struct {
	mode           string
	accountID      int64
	installationID string
	sessionID      string
	threadID       string
	windowID       string
}

// deriveStableCodexUUID derives a UUIDv4-format string deterministically from a seed: the same seed always returns
// the same value, and it does not change across process restarts, so nothing needs to be persisted.
//
// Used only for installation_id: packet captures show the real client's installation identifier is v4, while session/thread/
// window are v7 (see deriveStableCodexUUIDv7). NewSHA1 is not reused here (unlike other deterministic IDs in this repo),
// because that would produce a v5 UUID whose version bits differ from the real v4, which is itself recognizable.
func deriveStableCodexUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

// deriveStableCodexUUIDv7 derives a UUIDv7-format string deterministically from a seed: the first 48 bits are the millisecond
// timestamp (big-endian, per RFC 9562), and the remaining 74 bits come from the seed hash. The same (seed, timestamp) always
// returns the same value, and it does not change across process restarts, so nothing needs to be persisted.
//
// Used for session / thread / window: packet captures show the real client's three identifiers are UUIDv7, with timestamp bits
// that decode to a moment only seconds away from the capture time. Hard-coding v4 everywhere previously had two recognizable traits: the version nibble differed from the real one;
// and more importantly, v4 has no time ordering: if the upstream sorts by UUID (primary key index / logs), a v4 lands because of its
// pseudo-random "timestamp bits" at a position completely unrelated to the real session, jumping to the top or bottom of the sequence (see the #536 comment).
// installation_id really is v4, so it still uses deriveStableCodexUUID.
func deriveStableCodexUUIDv7(seed string, unixMilli int64) string {
	sum := sha256.Sum256([]byte(seed))
	var u uuid.UUID
	copy(u[:], sum[:16])
	ms := uint64(unixMilli)
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

const (
	// codexIdentityFallbackEpochMilli is the timestamp baseline used when the account's join time is unavailable; it takes a fixed
	// recent moment (2025-01-01Z) to ensure the derived v7 timestamps fall in a reasonable range rather than 1970.
	codexIdentityFallbackEpochMilli int64 = 1_735_689_600_000
	// codexIdentitySpreadMilli is the spread window (30 days) of session timestamps relative to the baseline: it keeps the
	// session / thread / window and the v7 timestamps of different threads under the same account distinct from each other, without drifting far from
	// the account's lifetime. Each real client session creates a batch of identifiers with close but distinct timestamps.
	codexIdentitySpreadMilli int64 = 30 * 24 * 60 * 60 * 1000
)

// codexIdentityUnixMilli derives a deterministic millisecond timestamp for the account's converged identity UUID, used as the v7
// timestamp bits. It is anchored on the account's join time (which naturally moves forward with new accounts, avoiding all converged values clustering at one fixed moment),
// plus a bounded offset derived from the seed, so that session / thread / window and the timestamps of different threads are all distinct
// yet still cluster near the account's lifetime. It relies only on the account join time and the seed throughout, and does not introduce time.Now, so
// it keeps the property "same seed, constant value, no persistence needed".
func codexIdentityUnixMilli(account *auth.Account, seed string) int64 {
	base := account.AddedAt.Load() / int64(time.Millisecond)
	if base <= 0 {
		base = codexIdentityFallbackEpochMilli
	}
	sum := sha256.Sum256([]byte("codex2api:codex-identity-ts:v1:" + seed))
	offset := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(codexIdentitySpreadMilli))
	return base + offset
}

// DeriveStableSessionUUIDv7 derives a UUIDv7-format session identity deterministically from a seed, for call sites without an account
// context (the derivation of upstream Session_id / prompt_cache_key only has access to the API Key
// or API Key ID). The timestamp bits are a fixed baseline plus a bounded offset derived from the seed, so the same seed always returns
// the same value, unchanged across process restarts, and different seeds have distinct timestamps.
//
// It exists for format fidelity: the real Codex client's session_id is UUIDv7. Both of this gateway's earlier derivations were not
// that: IsolateCodexSessionID produced 16-digit bare hex (not even a UUID),
// and deterministicPromptCacheKey produced v5 with uuid.NewSHA1 (the version nibble does not match the real one).
// The latter is exactly the pitfall already named in the deriveStableCodexUUID comment; back then only the fingerprint module's side was fixed.
func DeriveStableSessionUUIDv7(seed string) string {
	return deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
}

// NewUpstreamSessionUUID generates a UUIDv7 that differs on every call, for the upstream session identity used by "per-request isolation"
// (the prompt_cache_key and session-id header in default isolation mode).
//
// It must be v7, not the v4 from uuid.NewString(): the real Codex client's session_id is always v7,
// and default isolation is the path this gateway takes most often; most outbound requests' session keys come from here,
// so a wrong version nibble would mean every request carries a marker that differs from the real client. v4 also has no time ordering,
// so if the upstream sorts by UUID (primary key index / logs), it lands because of its pseudo-random "timestamp bits" at a position
// completely unrelated to the real session (the same problem recorded in the deriveStableCodexUUIDv7 comment).
//
// NewV7 only errors when the system entropy source is unavailable; falling back to v4 in that case is still better than failing the request.
func NewUpstreamSessionUUID() string {
	if generated, err := uuid.NewV7(); err == nil {
		return generated.String()
	}
	return uuid.NewString()
}

// seededIdentityUnixMilli is the no-account version of codexIdentityUnixMilli: when the account join time is unavailable, a fixed baseline
// replaces it, and the rest of the spread logic is the same.
func seededIdentityUnixMilli(seed string) int64 {
	sum := sha256.Sum256([]byte("codex2api:session-identity-ts:v1:" + seed))
	offset := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(codexIdentitySpreadMilli))
	return codexIdentityFallbackEpochMilli + offset
}

// resolveCodexFingerprintIDs derives the convergence target values from the account configuration; it returns nil for the off tier or when the account is missing.
//
// downstreamHeaders are the original request headers of the downstream client, used to take the client's real session/thread identifiers to derive
// thread_id. Session tier: one client session maps to one thread; when session and thread differ (subagents), it splits one more thread out by the real
// thread, so that previous_response_id can still be found after X-Client-Request-Id collapses.
func resolveCodexFingerprintIDs(account *auth.Account, downstreamHeaders http.Header) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.EffectiveCodexFingerprintMode()
	if mode == auth.CodexFingerprintModeOff {
		return nil
	}
	accountID := account.ID()
	if accountID <= 0 {
		return nil
	}

	ids := &codexFingerprintIDs{
		mode:           mode,
		accountID:      accountID,
		installationID: resolveConvergedInstallationID(account, accountID),
	}
	if ids.installationID == "" {
		return nil
	}
	if mode == auth.CodexFingerprintModeDevice {
		return ids
	}

	if mode == auth.CodexFingerprintModeSingleMachineMultiWindow {
		clientSession, clientThread := extractClientCodexIdentity(downstreamHeaders)
		if clientSession == "" {
			clientSession = clientThread
		}
		if clientThread == "" {
			clientThread = clientSession
		}
		// Missing identity must not collapse every anonymous request into one thread.
		if clientSession == "" {
			return nil
		}
		ids.sessionID = singleMachineIdentity(accountID, clientSession)
		ids.threadID = singleMachineIdentity(accountID, clientThread)
		ids.windowID = ids.threadID + ":0"
		return ids
	}
	sessionSeed := fmt.Sprintf("codex2api:codex-session-id:v1:%d", accountID)
	ids.sessionID = deriveStableCodexUUIDv7(sessionSeed, codexIdentityUnixMilli(account, sessionSeed))
	ids.threadID = ids.sessionID
	if mode == auth.CodexFingerprintModeSession {
		clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
		var threadSeed string
		switch {
		case clientThreadID != "" && clientSessionID != "" && clientThreadID != clientSessionID:
			// Subagents / multiple windows: the real thread and session differ. Still deriving by session would fold
			// multiple threads into one, and once X-Client-Request-Id (measured to always equal thread-id) collapses along with it,
			// the upstream looking up previous_response_id by thread would return 400 (#541).
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v2:%d:%s", accountID, clientThreadID)
		case clientSessionID != "":
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v1:%d:%s", accountID, clientSessionID)
		case clientThreadID != "":
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v1:%d:%s", accountID, clientThreadID)
		}
		if threadSeed != "" {
			ids.threadID = deriveStableCodexUUIDv7(threadSeed, codexIdentityUnixMilli(account, threadSeed))
		}
	}
	ids.windowID = ids.threadID + ":0"
	return ids
}

// resolveConvergedInstallationID returns the account-level constant installation_id.
// An installation id explicitly set in the account's custom headers takes priority: operators use it to pin the account to a known
// real device identifier, and since that value overrides outbound headers anyway (custom headers are applied last), it is adopted here as well so that
// the header and metadata values are guaranteed to match.
func resolveConvergedInstallationID(account *auth.Account, accountID int64) string {
	for name, value := range account.GetCustomHeaders() {
		if strings.EqualFold(strings.TrimSpace(name), codexInstallationIDHeader) {
			if pinned := strings.TrimSpace(value); pinned != "" {
				return pinned
			}
		}
	}
	return deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-install-id:v1:%d", accountID))
}

// extractClientCodexSessionID takes the downstream client's original session identifier.
// Read-only on request headers: the request header rewrite point cannot access the request body, so it is limited to the same input to guarantee the two derivations match.
func extractClientCodexSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	// The Codex CLI sends the hyphenated session-id over HTTP/2; the underscore form is the legacy spelling, and both are accepted.
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	// When the client sends no session header, fall back to the session identifier self-reported in the turn metadata.
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return ""
	}
	for _, path := range []string{"session_id", "thread_id"} {
		if value := strings.TrimSpace(gjson.Get(raw, path).String()); value != "" {
			return value
		}
	}
	return ""
}

// ApplyCodexFingerprintHeaders converges the device fingerprint in outbound request headers according to the account configuration.
// Must be called after allowlist passthrough (applyCodexAllowedForwardHeaders) so it can overwrite the client's original values;
// it should also be called before the account's custom headers, so that headers the operator configures explicitly keep final priority.
//
// downstreamHeaders decide whether this request really came from a Codex client: a non-Codex downstream
// (a plain OpenAI SDK, etc.) does not send x-codex-* headers, otherwise it would amount to fabricating a half-set of client traits
// on a request that has no Codex traits to begin with, which is more suspicious than not sending them.
func ApplyCodexFingerprintHeaders(outbound http.Header, account *auth.Account, downstreamHeaders http.Header) {
	if outbound == nil {
		return
	}
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return
	}

	if raw := strings.TrimSpace(outbound.Get(codexTurnMetadataHeader)); raw != "" {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(raw, ids); changed {
			outbound.Set(codexTurnMetadataHeader, rewritten)
		}
	}

	// X-Client-Request-Id is passed through verbatim per the allowlist (codexAllowedForwardHeaders). Measurements show the real
	// client sets it to the same value as its own thread_id, so leaving it alone would send the downstream user's original
	// thread identifier straight to the upstream, out of step with the converged metadata. Rewrite it only when it does reuse the client's
	// self-reported session/thread identifier; when the value is unrelated to identity, keep it as is.
	if converged := convergedClientRequestID(outbound.Get(codexClientRequestIDHeader), ids, downstreamHeaders); converged != "" {
		outbound.Set(codexClientRequestIDHeader, converged)
	}

	// Only handle identifier headers for downstream requests that do carry real Codex engine traits.
	if !EvaluateEngineFingerprint(downstreamHeaders, nil, nil) {
		return
	}
	// Only overwrite the identifier headers the downstream actually sent. Measurements show the real Codex CLI only sends x-codex-window-id,
	// and never sends x-codex-installation-id (installation_id only exists inside the turn metadata JSON):
	// unconditionally adding it would give the request a header the real client would never have, the opposite of the convergence goal.
	overrideExistingHeader(outbound, downstreamHeaders, codexInstallationIDHeader, ids.installationID)
	overrideExistingHeader(outbound, downstreamHeaders, codexWindowIDHeader, ids.windowID)
	// x-codex-parent-thread-id and parent_thread_id in metadata must report the same value:
	// on the real client these two are two projections of the same snapshot (responses_metadata.rs
	// compatibility_headers), and if one converges but the other does not, it becomes an easily compared giveaway.
	// The device tier does no session-level convergence, consistent with the other fields of that tier.
	if ids.mode != auth.CodexFingerprintModeDevice && downstreamHeaders != nil {
		if original := strings.TrimSpace(downstreamHeaders.Get(codexParentThreadIDHeader)); original != "" {
			overrideExistingHeader(outbound, downstreamHeaders, codexParentThreadIDHeader,
				convergeFingerprintLineageValue(ids, "parent_thread_id", original))
		}
	}
}

// overrideExistingHeader overwrites the outbound value with the converged value only when the downstream actually sent that header.
// The check reads the downstream header rather than the outbound header: neither of these identifier headers is in codexAllowedForwardHeaders,
// so even if the downstream sent them, the outbound request does not carry them at this point.
func overrideExistingHeader(outbound, downstreamHeaders http.Header, name, value string) {
	if value == "" || downstreamHeaders == nil {
		return
	}
	if strings.TrimSpace(downstreamHeaders.Get(name)) == "" {
		return
	}
	outbound.Set(name, value)
}

// convergedClientRequestID returns the corresponding converged value when X-Client-Request-Id reuses the client's self-reported identity,
// otherwise it returns an empty string, meaning no rewrite. The device tier has no session-level converged value, so it never rewrites.
func convergedClientRequestID(current string, ids *codexFingerprintIDs, downstreamHeaders http.Header) string {
	current = strings.TrimSpace(current)
	if current == "" || ids == nil || ids.threadID == "" {
		return ""
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	switch {
	case clientThreadID != "" && current == clientThreadID:
		return ids.threadID
	case clientSessionID != "" && current == clientSessionID:
		return ids.sessionID
	default:
		return ""
	}
}

// extractClientCodexIdentity takes the downstream's self-reported original session/thread identifiers, used to check whether other headers
// carry the same value repeatedly. It is implemented separately from extractClientCodexSessionID: the latter is the input for deriving converged values,
// and changing it would make the thread_id that existing accounts have already stabilized drift.
func extractClientCodexIdentity(headers http.Header) (sessionID, threadID string) {
	if headers == nil {
		return "", ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			sessionID = value
			break
		}
	}
	threadID = strings.TrimSpace(headers.Get(codexThreadIDHeader))
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return sessionID, threadID
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(gjson.Get(raw, "session_id").String())
	}
	if threadID == "" {
		threadID = strings.TrimSpace(gjson.Get(raw, "thread_id").String())
	}
	return sessionID, threadID
}

// ApplyCodexFingerprintToBody converges the device fingerprint in the request body's client_metadata.
// Returns the request body unchanged when there is no client_metadata.
func ApplyCodexFingerprintToBody(body []byte, account *auth.Account, downstreamHeaders http.Header) []byte {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return body
	}
	if !gjson.GetBytes(body, "client_metadata").IsObject() {
		return body
	}

	body = setExistingJSONString(body, "client_metadata.x-codex-installation-id", ids.installationID)
	if ids.mode == auth.CodexFingerprintModeSingleMachineMultiWindow {
		body = setExistingJSONString(body, "client_metadata.installation_id", ids.installationID)
		body = rewriteSingleMachineBodyLineage(body, ids)
	}
	if ids.mode != auth.CodexFingerprintModeDevice {
		body = setExistingJSONString(body, "client_metadata.session_id", ids.sessionID)
		body = setExistingJSONString(body, "client_metadata.thread_id", ids.threadID)
		body = setExistingJSONString(body, "client_metadata.x-codex-window-id", ids.windowID)
	}

	// client_metadata may embed a turn metadata JSON string, which must be converged as well.
	const embeddedPath = "client_metadata.x-codex-turn-metadata"
	if embedded := gjson.GetBytes(body, embeddedPath); embedded.Type == gjson.String {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(embedded.String(), ids); changed {
			if updated, err := sjson.SetBytes(body, embeddedPath, rewritten); err == nil {
				body = updated
			}
		}
	}
	return body
}

// rewriteCodexTurnMetadataJSON converges the identity fields in the turn metadata JSON.
// It uses sjson to replace values in place rather than unmarshal to a map and re-marshal: the latter would re-sort keys lexicographically and drop the original key order,
// and this shape difference is itself a recognizable trait. Keys that did not appear are not added.
func rewriteCodexTurnMetadataJSON(raw string, ids *codexFingerprintIDs) (string, bool) {
	if ids == nil || !gjson.Valid(raw) {
		return raw, false
	}
	updates := [][2]string{{"installation_id", ids.installationID}}
	if ids.mode != auth.CodexFingerprintModeDevice {
		updates = append(updates,
			[2]string{"session_id", ids.sessionID},
			[2]string{"thread_id", ids.threadID},
			[2]string{"window_id", ids.windowID},
		)
	}

	changed := false
	for _, update := range updates {
		path, value := update[0], update[1]
		if value == "" {
			continue
		}
		existing := gjson.Get(raw, path)
		if !existing.Exists() || existing.String() == value {
			continue
		}
		updated, err := sjson.Set(raw, path, value)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	if ids.mode != auth.CodexFingerprintModeDevice {
		if rewritten, ok := convergeFingerprintLineageMetadata(raw, ids); ok {
			raw = rewritten
			changed = true
		}
	}
	if scrubbed, ok := scrubCodexWorkspaces(raw, ids.accountID); ok {
		raw = scrubbed
		changed = true
	}
	return raw, changed
}

// codexLineageMetadataPaths are the remaining turn metadata fields that stably identify the downstream
// session/thread across requests (for field definitions see codex-rs/core/src/responses_metadata.rs
// CodexTurnMetadataPayload). The earlier batch of convergence only covered the four keys installation/session/thread/window; these are passed through verbatim, which
// is the same leak through different fields:
//
//   - context_window_id: Uuid::now_v7, constant within a session and incremented by auto-compact
//     (core/src/session/mod.rs current_window). It is stable across requests, and on its own one field
//     can string the multiple requests of the same downstream user into a chain, so the earlier convergence items lose their meaning.
//   - parent_thread_id / forked_from_thread_id: the downstream's real ThreadId; same level as thread_id,
//     and the leak surface is the same.
//
// Deliberately excludes parent_turn_id / root_turn_id: they are turn-level identifiers; for the same reason as the judgment on
// turn_id at the top of this file, they change every turn and do not identify a device or session, and recomputing them would lose consistency with the same turn's
// turn_id / turn_started_at_unix_ms, and that inconsistency is itself a trait.
var codexLineageMetadataPaths = []string{
	"context_window_id",
	"parent_thread_id",
	"forked_from_thread_id",
}

// convergeCodexLineageMetadata replaces the lineage fields with converged values derived from the account + original value.
//
// Deriving from the original value (rather than collapsing everything to one constant) preserves the shape: different downstream windows/parent threads still look like
// different values upstream, which is exactly how a real client looks; only the fact that they no longer equal the downstream's real identifiers changes.
// The seed carries accountID, consistent with the convention of the file's other converged values: otherwise the same window of the same downstream user
// under different accounts would derive the same placeholder value, and the upstream could then link two accounts to the same person.
//
// Only rewrites existing, non-empty string keys; nothing is added and the metadata shape does not change.
func convergeCodexLineageMetadata(raw string, accountID int64) (string, bool) {
	changed := false
	for _, path := range codexLineageMetadataPaths {
		existing := gjson.Get(raw, path)
		if existing.Type != gjson.String {
			continue
		}
		original := strings.TrimSpace(existing.String())
		if original == "" {
			continue
		}
		converged := convergeCodexLineageValue(accountID, path, original)
		if converged == original {
			continue
		}
		updated, err := sjson.Set(raw, path, converged)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	return raw, changed
}

// convergeCodexLineageValue derives a converged value for a single lineage identifier. The key takes part in the seed, so the same
// original UUID on different fields gets different results: the upstream thus cannot tell that "someone's parent thread" and
// "someone's context window" are the same object.
func convergeCodexLineageValue(accountID int64, key, original string) string {
	seed := fmt.Sprintf("codex2api:codex-lineage:v1:%s:%d:%s", key, accountID, original)
	return deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
}

// scrubCodexWorkspaces removes the workspace identity from the turn metadata.
//
// workspaces are keyed by local absolute paths (which include the system username), and entries carry the git remote URL and commit
// hash. When multiple people share one account, these fields would send each downstream user's identity verbatim to the upstream: the same
// installation_id has a pile of unrelated repository addresses and username directories hanging under it, which is both a privacy leak and
// makes the convergence of the earlier identifiers meaningless.
//
// The approach is to converge rather than delete: path keys are replaced with placeholders derived from the original values (entry count and mutual distinctness are both kept,
// the metadata shape does not change), the remote URL is removed entirely, and the commit hash is replaced with a derived placeholder value:
// the commit hash is a globally unique repository marker, and a public repository can be traced back to its owner directly from it, so keeping it would be the same
// identity leak as the remote URL field.
//
// Every derived value carries accountID: otherwise the same path of the same downstream user under different accounts would get the same
// placeholder, and the upstream could then link two accounts to the same person. This also matches the seed convention of the file's other converged values
// (see resolveConvergedInstallationID).
func scrubCodexWorkspaces(raw string, accountID int64) (string, bool) {
	workspaces := gjson.Get(raw, "workspaces")
	if !workspaces.IsObject() {
		return raw, false
	}

	rebuilt := "{}"
	changed := false
	failed := false
	workspaces.ForEach(func(key, value gjson.Result) bool {
		entry := value.Raw
		if gjson.Get(entry, "associated_remote_urls").Exists() {
			if updated, err := sjson.Delete(entry, "associated_remote_urls"); err == nil {
				entry = updated
				changed = true
			}
		}
		originalPath := key.String()
		placeholder := originalPath
		// If it is already a placeholder, leave it as is: the retry path may send a rewritten payload in a second time,
		// and a second hash would make the same workspace drift between attempts.
		if !isPlaceholderWorkspacePath(originalPath) {
			placeholder = placeholderWorkspacePath(accountID, originalPath)
			changed = true
		}
		// The placeholder value of the commit hash is derived from the "already-derived placeholder path", not from the original hash:
		// on second processing the path is already a placeholder and the derived result is unchanged, so idempotency holds, while different workspaces
		// still get mutually distinct hashes.
		if hash := gjson.Get(entry, "latest_git_commit_hash"); hash.Type == gjson.String && hash.String() != "" {
			if replacement := placeholderCommitHash(accountID, placeholder); replacement != hash.String() {
				if updated, err := sjson.Set(entry, "latest_git_commit_hash", replacement); err == nil {
					entry = updated
					changed = true
				}
			}
		}
		updated, err := sjson.SetRaw(rebuilt, placeholder, entry)
		if err != nil {
			failed = true
			return false
		}
		rebuilt = updated
		return true
	})
	if failed || !changed {
		return raw, false
	}

	updated, err := sjson.SetRaw(raw, "workspaces", rebuilt)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// placeholderWorkspacePath maps a local path to a stable placeholder: the same path always gets the same placeholder,
// different paths are distinct from each other, and it carries no username or project name. The return value contains no sjson path metacharacters
// (. * ?), so it can be used directly as an sjson path.
func placeholderWorkspacePath(accountID int64, original string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "codex2api:workspace-path:v2:%d:%s", accountID, original))
	return workspacePlaceholderPrefix + fmt.Sprintf("%x", sum[:workspacePlaceholderDigestBytes])
}

// placeholderCommitHash derives a placeholder value the same length as a real commit hash (40 hex digits),
// keeping the field shape unchanged. The seed is the placeholder path rather than the original hash; see the note at the call site.
func placeholderCommitHash(accountID int64, placeholderPath string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "codex2api:workspace-commit:v1:%d:%s", accountID, placeholderPath))
	return fmt.Sprintf("%x", sum[:20])
}

const (
	workspacePlaceholderPrefix = "/workspace/"
	// 32 bits is too narrow for low-entropy local paths: it could collide and merge two workspaces into one,
	// and it also makes reverse lookup from a table of candidate paths easy. 64 bits costs nothing extra.
	workspacePlaceholderDigestBytes = 8
)

// isPlaceholderWorkspacePath checks whether a path is already a placeholder this function produces, so that the scrub stays idempotent.
func isPlaceholderWorkspacePath(path string) bool {
	suffix, ok := strings.CutPrefix(path, workspacePlaceholderPrefix)
	if !ok || len(suffix) != workspacePlaceholderDigestBytes*2 {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// setExistingJSONString replaces the value only when the target path already exists, so it does not add fields that the client did not send to the
// request.
func setExistingJSONString(body []byte, path, value string) []byte {
	if value == "" {
		return body
	}
	existing := gjson.GetBytes(body, path)
	if !existing.Exists() || existing.String() == value {
		return body
	}
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return updated
}
