package auth

import (
	"errors"
	"testing"

	"github.com/codex2api/database"
)

func newPermanentFailureTestStore() (*Store, *Account) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{DBID: 1, RefreshToken: "rt-dead", PlanType: "plus"}
	store.AddAccount(acc)
	return store, acc
}

func TestPermanentRefreshFailureEscalatesToTerminalError(t *testing.T) {
	store, acc := newPermanentFailureTestStore()
	failure := errors.New(`refresh failed (status 401): {"error":{"code":"refresh_token_invalidated"}}`)

	for i := 1; i < permanentRefreshFailureTerminalLimit; i++ {
		store.markPermanentRefreshFailure(acc, failure)
		if got := acc.RuntimeStatus(); got != "unauthorized" {
			t.Fatalf("failure #%d RuntimeStatus() = %q, want unauthorized", i, got)
		}
	}
	store.markPermanentRefreshFailure(acc, failure)
	if got := acc.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() after %d failures = %q, want error", permanentRefreshFailureTerminalLimit, got)
	}
}

func TestTerminalRefreshFailureExitsRecoveryProbeRotation(t *testing.T) {
	store, acc := newPermanentFailureTestStore()
	failure := errors.New("invalid_grant")
	for range permanentRefreshFailureTerminalLimit {
		store.markPermanentRefreshFailure(acc, failure)
	}

	// The banned tier in runtimeStatusLocked unconditionally shows unauthorized, and it is the recovery
	// probe admission tier: a terminal account must already be moved out of banned, otherwise it can never reach the error filter.
	acc.mu.RLock()
	tier := acc.HealthTier
	acc.mu.RUnlock()
	if tier == HealthTierBanned {
		t.Fatal("terminal account must leave the banned tier (banned masks error display and feeds the probe rotation)")
	}
	if acc.NeedsRecoveryProbe(0) {
		t.Fatal("terminal dead-RT account must exit the recovery probe rotation")
	}

	// Fallback gate: when request-side 401 and similar paths push the account back into the banned tier, the terminal-failure counter has not been reset
	// so the same dead RT still must not be used for probing.
	acc.mu.Lock()
	acc.HealthTier = HealthTierBanned
	acc.mu.Unlock()
	if acc.NeedsRecoveryProbe(0) {
		t.Fatal("re-banned terminal account must stay out of the probe rotation while the counter stands")
	}

	// A manual cleanup (the re-authorization entry point) resets the terminal-failure counter and restores probe eligibility.
	store.ClearCooldown(acc)
	acc.mu.Lock()
	acc.HealthTier = HealthTierBanned
	acc.mu.Unlock()
	if !acc.NeedsRecoveryProbe(0) {
		t.Fatal("ClearCooldown must reset the terminal counter and restore probe eligibility")
	}
}
