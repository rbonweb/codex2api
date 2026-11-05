package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexContinueThinkingSettingsFromRuntime(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	// default: disabled, round count is 8 by default.
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	if enabled, rounds := codexContinueThinkingSettings(); enabled || rounds != 8 {
		t.Fatalf("should be disabled by default with 8 rounds, got enabled=%v rounds=%d", enabled, rounds)
	}

	settings := DefaultRuntimeSettings()
	settings.CodexContinueThinking = true
	settings.CodexContinueMaxRounds = 5
	ApplyRuntimeSettings(settings)
	if enabled, rounds := codexContinueThinkingSettings(); !enabled || rounds != 5 {
		t.Fatalf("after enabling: got enabled=%v rounds=%d, want true/5", enabled, rounds)
	}
}

func TestCodexContinueMaxRoundsClamp(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	cases := []struct {
		in   int
		want int
	}{
		{0, 8},   // non-positive → default
		{-3, 8},  // negative → default
		{1, 1},   // lower bound
		{8, 8},   // regular
		{32, 32}, // upper bound
		{99, 32}, // above upper bound → clamped
	}
	for _, c := range cases {
		s := DefaultRuntimeSettings()
		s.CodexContinueMaxRounds = c.in
		got := NormalizeRuntimeSettings(s).CodexContinueMaxRounds
		if got != c.want {
			t.Errorf("clamp(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestApplyRuntimeSettingsFromSystemContinueThinking(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexContinueThinkingEnabled: true,
		CodexContinueMaxRounds:       12,
		CodexWSSilentMaxRetries:      2,
	})
	if !next.CodexContinueThinking || next.CodexContinueMaxRounds != 12 {
		t.Fatalf("SystemSettings → RuntimeSettings mapping is wrong: %+v", next)
	}
	// nil settings fall back to the defaults (disabled + 8 rounds).
	def := ApplyRuntimeSettingsFromSystem(nil)
	if def.CodexContinueThinking || def.CodexContinueMaxRounds != 8 {
		t.Fatalf("nil settings should fall back to the defaults: %+v", def)
	}
}

func TestNormalizeCodexContinueMaxRoundsDB(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 8}, {-1, 8}, {1, 1}, {8, 8}, {32, 32}, {33, 32},
	}
	for _, c := range cases {
		if got := database.NormalizeCodexContinueMaxRounds(c.in); got != c.want {
			t.Errorf("NormalizeCodexContinueMaxRounds(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
