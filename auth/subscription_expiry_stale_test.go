package auth

import (
	"testing"
	"time"
)

// TestStaleSubscriptionExpiry verifies that "paid plan + expiry time has passed" is judged stale,
// other combinations (free/API/not yet expired/no expiry time) are unaffected. (issue #360)
func TestStaleSubscriptionExpiry(t *testing.T) {
	now := time.Now()
	past := now.Add(-48 * time.Hour)
	future := now.Add(48 * time.Hour)

	cases := []struct {
		name      string
		plan      string
		expiresAt time.Time
		want      bool
	}{
		{"paid plan+expired", "plus", past, true},
		{"paid plan case and whitespace", "  Plus ", past, true},
		{"team+expired", "team", past, true},
		{"pro+expired", "pro", past, true},
		{"paid plan+not yet expired", "plus", future, false},
		{"free+expired", "free", past, false},
		{"api+expired", "api", past, false},
		{"unknown plan+expired", "", past, false},
		{"paid plan+zero value", "plus", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleSubscriptionExpiry(tc.plan, tc.expiresAt, now); got != tc.want {
				t.Fatalf("StaleSubscriptionExpiry(%q, %v) = %v, want %v", tc.plan, tc.expiresAt, got, tc.want)
			}
		})
	}
}

// TestClearStaleSubscriptionExpiresAt verifies that clearing only applies to stale combinations:
// the expiry time of a paid plan is cleared; a free plan (truly expired) and a paid plan that has not expired stay unchanged.
func TestClearStaleSubscriptionExpiresAt(t *testing.T) {
	past := time.Now().Add(-72 * time.Hour)
	future := time.Now().Add(30 * 24 * time.Hour)

	cases := []struct {
		name        string
		plan        string
		expiresAt   time.Time
		wantCleared bool
	}{
		{"paid plan stale value cleared", "plus", past, true},
		{"free plan truly expired kept", "free", past, false},
		{"paid plan not expired kept", "plus", future, false},
		{"no expiry time left alone", "plus", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(nil, nil, nil)
			acc := &Account{
				DBID:                  1,
				AccessToken:           "at-test",
				Status:                StatusReady,
				PlanType:              tc.plan,
				SubscriptionExpiresAt: tc.expiresAt,
			}
			store.AddAccount(acc)

			cleared := store.ClearStaleSubscriptionExpiresAt(acc)
			if cleared != tc.wantCleared {
				t.Fatalf("ClearStaleSubscriptionExpiresAt() = %v, want %v", cleared, tc.wantCleared)
			}

			acc.mu.RLock()
			got := acc.SubscriptionExpiresAt
			acc.mu.RUnlock()
			if tc.wantCleared && !got.IsZero() {
				t.Fatalf("SubscriptionExpiresAt = %v, want zero after clear", got)
			}
			if !tc.wantCleared && !got.Equal(tc.expiresAt) {
				t.Fatalf("SubscriptionExpiresAt = %v, want unchanged %v", got, tc.expiresAt)
			}
		})
	}
}

// TestNeedsSubscriptionExpiryProbe verifies the trigger conditions and throttling of the subscription expiry probe:
// it is needed only for a paid plan whose expiry time is unknown, near, or past, and only when the minimum interval since the last attempt has passed.
func TestNeedsSubscriptionExpiryProbe(t *testing.T) {
	now := time.Now()
	interval := 6 * time.Hour

	cases := []struct {
		name      string
		plan      string
		expiresAt time.Time
		probedAt  time.Time
		want      bool
	}{
		{"paid+no expiry time", "plus", time.Time{}, time.Time{}, true},
		{"paid+expired", "plus", now.Add(-48 * time.Hour), time.Time{}, true},
		{"paid+expiring soon (3d)", "plus", now.Add(3 * 24 * time.Hour), time.Time{}, true},
		{"paid+far from expiry (30d)", "plus", now.Add(30 * 24 * time.Hour), time.Time{}, false},
		{"free no probe", "free", time.Time{}, time.Time{}, false},
		{"api no probe", "api", time.Time{}, time.Time{}, false},
		{"unknown plan no probe", "", time.Time{}, time.Time{}, false},
		{"throttle:just probed", "plus", time.Time{}, now.Add(-time.Hour), false},
		{"throttle:interval elapsed", "plus", time.Time{}, now.Add(-7 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{PlanType: tc.plan, SubscriptionExpiresAt: tc.expiresAt}
			if !tc.probedAt.IsZero() {
				acc.MarkSubscriptionExpiryProbed(tc.probedAt)
			}
			if got := acc.NeedsSubscriptionExpiryProbe(now, interval); got != tc.want {
				t.Fatalf("NeedsSubscriptionExpiryProbe() = %v, want %v", got, tc.want)
			}
		})
	}
}
