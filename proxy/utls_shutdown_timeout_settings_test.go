package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

// issue #446: the wait limit for graceful uTLS connection shutdown changed from a hard-coded constant to a system setting.
// The tests below pin the default values, the clamp bounds, and the DB→runtime propagation path.

func TestUTLSShutdownTimeoutDefaultIs30Min(t *testing.T) {
	if got := DefaultRuntimeSettings().UTLSShutdownTimeout(); got != 30*time.Minute {
		t.Fatalf("default uTLS graceful shutdown limit = %s, want 30m", got)
	}
	// When unset (zero value), it must fall back to the default instead of becoming 0 — 0 would make the context time out immediately,
	// cutting off in-flight streamed responses immediately.
	var zero RuntimeSettings
	if got := zero.UTLSShutdownTimeout(); got != 30*time.Minute {
		t.Fatalf("zero-value uTLS graceful shutdown limit = %s, want 30m (0 would immediately cut off in-flight requests)", got)
	}
}

func TestNormalizeUTLSShutdownTimeoutMinutesClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 30},     // unset, falls back to the default
		{-5, 30},    // invalid negative value, falls back to the default
		{1, 1},      // lower bound
		{30, 30},    // default value as is
		{240, 240},  // upper bound
		{1000, 240}, // out of range, clamped to the upper bound
	}
	for _, c := range cases {
		if got := database.NormalizeUTLSShutdownTimeoutMinutes(c.in); got != c.want {
			t.Errorf("NormalizeUTLSShutdownTimeoutMinutes(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// Runtime normalization must clamp out-of-range values, so the admin side cannot bypass validation and write extreme values.
func TestNormalizeRuntimeSettingsClampsUTLSShutdownTimeout(t *testing.T) {
	s := DefaultRuntimeSettings()
	s.UTLSShutdownTimeoutMin = 99999
	if got := NormalizeRuntimeSettings(s).UTLSShutdownTimeoutMin; got != 240 {
		t.Fatalf("out-of-range value normalized = %d, want 240", got)
	}
	s.UTLSShutdownTimeoutMin = 0
	if got := NormalizeRuntimeSettings(s).UTLSShutdownTimeoutMin; got != 30 {
		t.Fatalf("zero value normalized = %d, want 30", got)
	}
}

// The DB setting must really be delivered to the runtime and read by the transport.
func TestApplyRuntimeSettingsFromSystemUTLSShutdownTimeout(t *testing.T) {
	original := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(original) })

	next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		UTLSShutdownTimeoutMinutes: 5,
	})
	if next.UTLSShutdownTimeoutMin != 5 {
		t.Fatalf("after delivery UTLSShutdownTimeoutMin = %d, want 5", next.UTLSShutdownTimeoutMin)
	}
	// The transport-side value getter must follow setting changes (it used to be a compile-time constant).
	if got := utlsShutdownTimeout(); got != 5*time.Minute {
		t.Fatalf("upper limit read by transport = %s, want 5m", got)
	}

	// A snapshot from an old database or old instance that does not set this field must fall back to the default, not become 0.
	def := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{})
	if def.UTLSShutdownTimeoutMin != 30 {
		t.Fatalf("snapshot missing field fell back to = %d, want 30", def.UTLSShutdownTimeoutMin)
	}
	if got := utlsShutdownTimeout(); got != 30*time.Minute {
		t.Fatalf("transport read after fallback = %s, want 30m", got)
	}
}
