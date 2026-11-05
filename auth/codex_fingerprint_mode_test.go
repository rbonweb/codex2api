package auth

import "testing"

func TestNormalizeCodexFingerprintMode(t *testing.T) {
	cases := map[string]string{
		"":                              CodexFingerprintModeOff,
		"off":                           CodexFingerprintModeOff,
		"unknown":                       CodexFingerprintModeOff,
		"DEVICE":                        CodexFingerprintModeDevice,
		" session ":                     CodexFingerprintModeSession,
		"Full":                          CodexFingerprintModeFull,
		" Single_Machine_Multi_Window ": CodexFingerprintModeSingleMachineMultiWindow,
	}
	for input, want := range cases {
		if got := NormalizeCodexFingerprintMode(input); got != want {
			t.Errorf("NormalizeCodexFingerprintMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsValidCodexFingerprintMode(t *testing.T) {
	for _, value := range []string{CodexFingerprintModeOff, CodexFingerprintModeDevice, CodexFingerprintModeSession, CodexFingerprintModeFull, CodexFingerprintModeSingleMachineMultiWindow, " FULL "} {
		if !IsValidCodexFingerprintMode(value) {
			t.Errorf("IsValidCodexFingerprintMode(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "converge", "device-only"} {
		if IsValidCodexFingerprintMode(value) {
			t.Errorf("IsValidCodexFingerprintMode(%q) = true, want false", value)
		}
	}
}

func TestEffectiveCodexFingerprintMode(t *testing.T) {
	if got := (*Account)(nil).EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("nil account mode = %q, want %q", got, CodexFingerprintModeOff)
	}

	// Existing accounts without configuration must stay off; the upgrade does not change outbound behavior.
	if got := (&Account{DBID: 1}).EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("unconfigured account mode = %q, want %q", got, CodexFingerprintModeOff)
	}

	codex := &Account{DBID: 1, CodexFingerprintMode: CodexFingerprintModeSession}
	if got := codex.EffectiveCodexFingerprintMode(); got != CodexFingerprintModeSession {
		t.Errorf("codex account mode = %q, want %q", got, CodexFingerprintModeSession)
	}
}
