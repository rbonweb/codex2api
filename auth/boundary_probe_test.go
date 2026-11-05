package auth

import (
	"testing"
	"time"
)

// TestNextProbeBoundaryPicksEarliest verifies that nextProbeBoundary picks the nearest future moment among several candidate boundaries,
// and correctly ignores cases that should not be probed, such as "already expired", "snapshot refreshed after reset", and "unauthorized cooldown".
func TestNextProbeBoundaryPicksEarliest(t *testing.T) {
	now := time.Now()

	t.Run("picks the nearest future boundary (5h cooldown before 7d reset)", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			Status:              StatusCooldown,
			CooldownReason:      "rate_limited",
			CooldownUtil:        now.Add(30 * time.Second),
			UsagePercent7dValid: true,
			Reset7dAt:           now.Add(6 * 24 * time.Hour),
			UsageUpdatedAt:      now.Add(-time.Hour),
		}
		got, ok := a.nextProbeBoundary(now)
		if !ok || !got.Equal(a.CooldownUtil) {
			t.Fatalf("got %v,%v, want %v,true (should pick the nearer cooldown end time)", got, ok, a.CooldownUtil)
		}
	})

	t.Run("5h window reset", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(90 * time.Second),
			UsageUpdatedAt5h:    now.Add(-time.Hour), // snapshot taken before the reset
		}
		got, ok := a.nextProbeBoundary(now)
		if !ok || !got.Equal(a.Reset5hAt) {
			t.Fatalf("got %v,%v, want %v,true", got, ok, a.Reset5hAt)
		}
	})

	t.Run("snapshot refreshed after the reset → this boundary is no longer needed", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(90 * time.Second),
			UsageUpdatedAt5h:    now.Add(2 * time.Minute), // snapshot is later than the reset
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v, want zero,false (the snapshot already covers this window, no probe needed)", got, ok)
		}
	})

	t.Run("unauthorized cooldown does not arm (the probe always gets 401)", func(t *testing.T) {
		a := &Account{
			AccessToken:    "tok",
			Status:         StatusCooldown,
			CooldownReason: "unauthorized",
			CooldownUtil:   now.Add(time.Minute),
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v, want zero,false", got, ok)
		}
	})

	t.Run("boundary already passed → ignored", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(-time.Minute),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v, want zero,false (an expired boundary is left to the regular probe)", got, ok)
		}
	})

	t.Run("no credentials / error state → does not arm", func(t *testing.T) {
		a := &Account{
			Status:              StatusCooldown,
			CooldownReason:      "rate_limited",
			CooldownUtil:        now.Add(time.Minute),
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(time.Minute),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		}
		// AccessToken is empty
		if _, ok := a.nextProbeBoundary(now); ok {
			t.Fatal("no AccessToken should return false")
		}
		a.AccessToken = "tok"
		a.Status = StatusError
		if _, ok := a.nextProbeBoundary(now); ok {
			t.Fatal("StatusError should return false")
		}
	})
}

// TestWakeBoundaryProbeOnlyNudgesForEarlier verifies the throttle of WakeBoundaryProbe that only disturbs for an earlier boundary:
// an earlier boundary triggers a wake-up, while a later or already-passed boundary does not, to avoid frequent re-sorting.
func TestWakeBoundaryProbeOnlyNudgesForEarlier(t *testing.T) {
	now := time.Now()
	s := &Store{boundaryProbeWakeCh: make(chan struct{}, 1)}

	drain := func() bool {
		select {
		case <-s.boundaryProbeWakeCh:
			return true
		default:
			return false
		}
	}

	// Not armed (armedBoundaryAt=0): any future boundary should wake it.
	s.WakeBoundaryProbe(now.Add(time.Minute))
	if !drain() {
		t.Fatal("a future boundary should trigger a wake-up when not armed")
	}

	// Simulate being armed to +30s.
	s.armedBoundaryAt.Store(now.Add(30 * time.Second).UnixNano())

	// Later boundary (+2min) → does not disturb.
	s.WakeBoundaryProbe(now.Add(2 * time.Minute))
	if drain() {
		t.Fatal("a later boundary should not trigger a wake-up")
	}

	// Earlier boundary (+5s) → disturbs.
	s.WakeBoundaryProbe(now.Add(5 * time.Second))
	if !drain() {
		t.Fatal("an earlier boundary should trigger a wake-up")
	}

	// Expired boundary → does not disturb.
	s.WakeBoundaryProbe(now.Add(-time.Second))
	if drain() {
		t.Fatal("an expired boundary should not trigger a wake-up")
	}

	// Zero value → force a wake-up (unknown time, requires a full re-sort).
	s.WakeBoundaryProbe(time.Time{})
	if !drain() {
		t.Fatal("a zero value should force a wake-up")
	}
}

// TestArmNextBoundaryProbeArmsTimer verifies the timer behavior of armNextBoundaryProbe with and without a pending boundary,
// and that it updates armedBoundaryAt in sync.
func TestArmNextBoundaryProbeArmsTimer(t *testing.T) {
	now := time.Now()
	s := &Store{boundaryProbeWakeCh: make(chan struct{}, 1)}
	s.accounts = []*Account{
		{ // nearest boundary: the 5h reset at +40ms
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(40 * time.Millisecond),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		},
		{ // farther: +10min cooldown
			AccessToken:    "tok",
			Status:         StatusCooldown,
			CooldownReason: "rate_limited",
			CooldownUtil:   now.Add(10 * time.Minute),
		},
	}
	s.publishAccountSnapshot(s.accounts)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}

	s.armNextBoundaryProbe(timer)

	if s.armedBoundaryAt.Load() != s.accounts[0].Reset5hAt.UnixNano() {
		t.Fatalf("armedBoundaryAt=%d, want %d (nearest boundary)", s.armedBoundaryAt.Load(), s.accounts[0].Reset5hAt.UnixNano())
	}

	// The timer should fire after the nearest boundary (+ probeBoundaryLag); allow enough margin here.
	select {
	case <-timer.C:
	case <-time.After(probeBoundaryLag + time.Second):
		t.Fatal("the timer should fire once the nearest boundary is reached")
	}

	// No pending boundary → stop the timer and reset armedBoundaryAt to zero.
	s.accounts = []*Account{{AccessToken: "tok"}}
	s.publishAccountSnapshot(s.accounts)
	timer2 := time.NewTimer(time.Hour)
	s.armNextBoundaryProbe(timer2)
	if s.armedBoundaryAt.Load() != 0 {
		t.Fatalf("armedBoundaryAt=%d, want 0 (no pending boundary)", s.armedBoundaryAt.Load())
	}
	select {
	case <-timer2.C:
		t.Fatal("the timer should not fire when there is no boundary")
	case <-time.After(100 * time.Millisecond):
	}
}
