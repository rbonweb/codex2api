package proxy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestStreamPhaseDiagnosticsDistinguishesBackpressure verifies that the stream-break criterion can tell apart
// "downstream backpressure stalls upstream reads" and "upstream resets on its own" (issue #491).
func TestStreamPhaseDiagnosticsDistinguishesBackpressure(t *testing.T) {
	// With no upstream frames, no criterion is produced: not even the response headers were read, which is unrelated to backpressure.
	if got := newStreamPhaseDiagnostics().summary(); got != "" {
		t.Fatalf("no upstream frame should yield empty summary, got %q", got)
	}

	diag := newStreamPhaseDiagnostics()
	diag.markUpstreamFrame()
	diag.markUpstreamFrame()
	diag.addWriteBlocked(time.Now().Add(-1200 * time.Millisecond))
	summary := diag.summary()
	for _, want := range []string{"upstream frames 2", "downstream write blocked 1", "since the last upstream frame"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}

	// A nil diagnostics object is safe throughout (zero-overhead path when not attached).
	var nilDiag *streamPhaseDiagnostics
	nilDiag.markUpstreamFrame()
	nilDiag.addWriteBlocked(time.Now())
	if got := nilDiag.summary(); got != "" {
		t.Fatalf("nil diagnostics summary = %q, want empty", got)
	}
	// A zero start value is not counted (diagnosticsClock returns the zero value when no diagnostics object is attached).
	zeroStart := newStreamPhaseDiagnostics()
	zeroStart.markUpstreamFrame()
	zeroStart.addWriteBlocked(time.Time{})
	if !strings.Contains(zeroStart.summary(), "downstream write blocked 0ms") {
		t.Fatalf("zero start must not accumulate: %s", zeroStart.summary())
	}
}

// TestAnnotateStreamBreakDiagnosticsOnlyForBreaks verifies that the criterion is appended only to stream-break failures,
// while client-initiated disconnects and normal completion are not affected.
func TestAnnotateStreamBreakDiagnosticsOnlyForBreaks(t *testing.T) {
	diag := newStreamPhaseDiagnostics()
	diag.markUpstreamFrame()

	broke := classifyStreamOutcome(nil, errors.New("stream error: stream ID 19; INTERNAL_ERROR; received from peer"), nil, false)
	annotated := annotateStreamBreakDiagnostics(broke, diag)
	if !strings.Contains(annotated.failureMessage, "failed to read upstream stream") || !strings.Contains(annotated.failureMessage, "upstream frames 1") {
		t.Fatalf("stream break message not annotated: %q", annotated.failureMessage)
	}
	if annotated.failureKind != "transport" || annotated.logStatusCode != logStatusUpstreamStreamBreak {
		t.Fatalf("annotation must not alter classification: kind=%q status=%d", annotated.failureKind, annotated.logStatusCode)
	}

	clientGone := classifyStreamOutcome(errors.New("context canceled"), nil, nil, false)
	if got := annotateStreamBreakDiagnostics(clientGone, diag); got.failureMessage != clientGone.failureMessage {
		t.Fatalf("client-close outcome must stay untouched: %q", got.failureMessage)
	}

	ok := classifyStreamOutcome(nil, nil, nil, true)
	if got := annotateStreamBreakDiagnostics(ok, diag); got.failureMessage != "" {
		t.Fatalf("successful stream must stay untouched: %q", got.failureMessage)
	}
}

// TestPoolEntryAgeRotation verifies that age-based rotation applies only to the standard transport, and that 0 means disabled.
func TestPoolEntryAgeRotation(t *testing.T) {
	now := time.Now().UnixNano()
	fresh := &poolEntry{createdAt: now - int64(5*time.Minute), rotatable: true}
	if fresh.expiredByAge(now, 30*time.Minute) {
		t.Fatal("young entry must not rotate")
	}
	old := &poolEntry{createdAt: now - int64(31*time.Minute), rotatable: true}
	if !old.expiredByAge(now, 30*time.Minute) {
		t.Fatal("entry past max age must rotate")
	}
	if old.expiredByAge(now, 0) {
		t.Fatal("max age 0 must disable rotation")
	}
	utls := &poolEntry{createdAt: now - int64(31*time.Minute), rotatable: false}
	if utls.expiredByAge(now, 30*time.Minute) {
		t.Fatal("uTLS entry must never rotate by age")
	}
	var nilEntry *poolEntry
	if nilEntry.expiredByAge(now, 30*time.Minute) {
		t.Fatal("nil entry must be safe")
	}
}

func TestDurationFromEnv(t *testing.T) {
	if got := durationFromEnv("CODEX_TEST_UNSET_DURATION", 15*time.Second); got != 15*time.Second {
		t.Fatalf("unset = %s, want 15s", got)
	}
	t.Setenv("CODEX_TEST_DURATION", "45s")
	if got := durationFromEnv("CODEX_TEST_DURATION", 15*time.Second); got != 45*time.Second {
		t.Fatalf("override = %s, want 45s", got)
	}
	t.Setenv("CODEX_TEST_DURATION", "0")
	if got := durationFromEnv("CODEX_TEST_DURATION", 15*time.Second); got != 0 {
		t.Fatalf("explicit zero = %s, want 0", got)
	}
	t.Setenv("CODEX_TEST_DURATION", "nonsense")
	if got := durationFromEnv("CODEX_TEST_DURATION", 15*time.Second); got != 15*time.Second {
		t.Fatalf("invalid must fall back, got %s", got)
	}
}
