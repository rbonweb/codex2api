package auth

import (
	"testing"
	"time"
)

// TestTransportFailureNeedsStreakBeforeTierDrop verifies that an isolated transport-layer disconnect no longer cuts an account's
// concurrency, while consecutive failures still lower the tier (issue #491).
func TestTransportFailureNeedsStreakBeforeTierDrop(t *testing.T) {
	s := &Store{}
	acc := &Account{HealthTier: HealthTierHealthy}

	// Every disconnect before the threshold must not drop a healthy account below Healthy.
	for i := 1; i < transportFailureTierDropStreak; i++ {
		s.ReportRequestFailure(acc, transportFailureKind, 10*time.Millisecond)
		if acc.HealthTier != HealthTierHealthy {
			t.Fatalf("isolated transport failure #%d dropped tier to %s", i, acc.HealthTier)
		}
	}
	// The consecutive-failure threshold is reached: the exemption no longer applies, the account drops a tier, and scheduling moves traffic away from it.
	s.ReportRequestFailure(acc, transportFailureKind, 10*time.Millisecond)
	if acc.HealthTier != HealthTierWarm {
		t.Fatalf("streak of %d transport failures should drop to warm, got %s", transportFailureTierDropStreak, acc.HealthTier)
	}
	if !acc.LastFailureAt.After(acc.LastSuccessAt) || acc.LastFailureKind != transportFailureKind {
		t.Fatalf("failure attribution not recorded: kind=%q", acc.LastFailureKind)
	}

	// A success resets the streak and the exemption applies again (this only asserts the exemption criterion itself: the account's current
	// success rate was already pulled down by the earlier consecutive failures, and the final tier is decided by the score, so it is not suitable to assert the tier again).
	s.ReportRequestSuccess(acc, 10*time.Millisecond)
	if acc.FailureStreak != 0 {
		t.Fatalf("success must reset failure streak, got %d", acc.FailureStreak)
	}
	s.ReportRequestFailure(acc, transportFailureKind, 10*time.Millisecond)
	if !acc.isolatedTransportFailureLocked() {
		t.Fatal("post-success single transport failure must be treated as isolated again")
	}
}

// TestNonTransportFailureStillDropsImmediately verifies that the exemption applies only to the transport layer:
// server/timeout drops a tier on the first failure, and unauthorized still bans.
func TestNonTransportFailureStillDropsImmediately(t *testing.T) {
	s := &Store{}
	for _, kind := range []string{"server", "timeout"} {
		acc := &Account{HealthTier: HealthTierHealthy}
		s.ReportRequestFailure(acc, kind, 10*time.Millisecond)
		if acc.HealthTier != HealthTierWarm {
			t.Fatalf("%s failure must drop tier on first hit, got %s", kind, acc.HealthTier)
		}
		if acc.isolatedTransportFailureLocked() {
			t.Fatalf("%s failure must not qualify for the transport exemption", kind)
		}
	}

	banned := &Account{HealthTier: HealthTierHealthy}
	s.ReportRequestFailure(banned, "unauthorized", 10*time.Millisecond)
	if banned.HealthTier != HealthTierBanned {
		t.Fatalf("unauthorized must still ban, got %s", banned.HealthTier)
	}
}
