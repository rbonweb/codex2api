package auth

import "strings"

// Codex device fingerprint convergence mode. When several people share the same Codex OAuth account, each downstream user's
// Codex client carries its own installation_id / session_id / thread_id, which the upstream uses to
// determine how many devices and how many sessions sit behind this one account. Convergence mode rewrites these identifiers to account-level
// constant values, so the device/session count the upstream sees converges toward the shape of a single user.
//
// Legacy modes only rewrite identity metadata. Session identity convergence also aligns
// session/thread headers while leaving gateway prompt-cache partitioning intact.
// No mode is enabled implicitly for existing accounts.
const (
	// CodexFingerprintModeOff does no convergence; client identifiers pass through unchanged (default).
	CodexFingerprintModeOff = "off"
	// CodexFingerprintModeSingleMachineMultiWindow shares a device, not a conversation.
	// Real windows and child threads retain distinct deterministic identities.
	CodexFingerprintModeSingleMachineMultiWindow = "single_machine_multi_window"
	// CodexFingerprintModeDevice only converges installation_id to an account-level constant.
	// The upstream sees 1 device + each downstream user's own sessions.
	CodexFingerprintModeDevice = "device"
	// CodexFingerprintModeSession converges installation_id + session_id, and per client
	// original session identifier deterministically derives thread_id: each real Codex session gets its own thread.
	// The upstream sees 1 device + 1 session + N threads, close to a single person opening multiple windows or spawning sub-agents.
	CodexFingerprintModeSession = "session"
	// CodexFingerprintModeFull makes thread_id equal session_id, so the upstream sees
	// 1 device + 1 session + 1 thread. The most aggressive mode, and the concurrency shape least like a real client.
	CodexFingerprintModeFull = "full"
)

// CodexFingerprintModeCredentialKey is the storage key for this mode in the account credentials.
const CodexFingerprintModeCredentialKey = "codex_fingerprint_mode"

// NormalizeCodexFingerprintMode normalizes the mode value; empty and invalid values both fall back to off,
// so existing accounts keep exactly the same outbound behavior after an upgrade; convergence is enabled only by explicit configuration.
func NormalizeCodexFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexFingerprintModeSingleMachineMultiWindow:
		return CodexFingerprintModeSingleMachineMultiWindow
	case CodexFingerprintModeDevice:
		return CodexFingerprintModeDevice
	case CodexFingerprintModeSession:
		return CodexFingerprintModeSession
	case CodexFingerprintModeFull:
		return CodexFingerprintModeFull
	default:
		return CodexFingerprintModeOff
	}
}

// IsValidCodexFingerprintMode reports whether the mode is known.
func IsValidCodexFingerprintMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexFingerprintModeOff, CodexFingerprintModeSingleMachineMultiWindow, CodexFingerprintModeDevice, CodexFingerprintModeSession, CodexFingerprintModeFull:
		return true
	default:
		return false
	}
}

// EffectiveCodexFingerprintMode returns the convergence mode in effect for the account.
func (a *Account) EffectiveCodexFingerprintMode() string {
	if a == nil {
		return CodexFingerprintModeOff
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeCodexFingerprintMode(a.CodexFingerprintMode)
}
