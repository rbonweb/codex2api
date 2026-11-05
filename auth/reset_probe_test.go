package auth

import (
	"testing"
	"time"
)

// TestNeedsUsageProbeFiresAtResetBoundary verifies that once the 5h/7d window reset time arrives,
// if the usage snapshot still holds data collected before the reset, a probe fires right away (this catches accounts that "look usable once the countdown reaches zero"
// but are in fact already banned); once the probe refreshes the snapshot, it does not fire again.
func TestNeedsUsageProbeFiresAtResetBoundary(t *testing.T) {
	now := time.Now()
	maxAge := 10 * time.Minute

	// base builds an account where everything except the window under test is normal and fresh, isolating the other probe branches.
	base := func() *Account {
		return &Account{
			AccessToken:          "at-x",
			Status:               StatusReady,
			PlanType:             "", // not premium, to avoid the premium5h branch
			resetCreditsProbedAt: now,
			UsagePercent7d:       50,
			UsagePercent7dValid:  true,
			UsageUpdatedAt:       now,
			Reset7dAt:            now.Add(time.Hour),
			UsagePercent5h:       10,
			UsagePercent5hValid:  true,
			UsageUpdatedAt5h:     now,
			Reset5hAt:            now.Add(time.Hour),
		}
	}

	t.Run("5h reset reached and snapshot expired→probe", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now.Add(-6 * time.Minute) // snapshot is older than the reset time
		if !a.NeedsUsageProbe(maxAge) {
			t.Fatal("expected a probe to fire right when the 5h reset is reached")
		}
	})

	t.Run("5h reset already refreshed→no repeat probe", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 0
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now // already refreshed after the reset
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("snapshot was already refreshed after the reset, should not fire again")
		}
	})

	t.Run("5h not yet reset and snapshot fresh→does not fire", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(time.Hour)
		a.UsageUpdatedAt5h = now
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("5h not yet reset and snapshot fresh, should not fire")
		}
	})

	t.Run("7d reset reached and snapshot expired→probe", func(t *testing.T) {
		a := base()
		a.Reset7dAt = now.Add(-time.Minute)
		a.UsageUpdatedAt = now.Add(-2 * time.Minute) // older than the 7d reset, but still < maxAge
		if !a.NeedsUsageProbe(maxAge) {
			t.Fatal("expected a probe to fire right when the 7d reset is reached")
		}
	})

	t.Run("does not fire when status is error", func(t *testing.T) {
		a := base()
		a.Status = StatusError
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now.Add(-6 * time.Minute)
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("error accounts should not trigger a probe")
		}
	})
}
