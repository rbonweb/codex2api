package auth

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

// Business status is decided by calendar day in the business timezone: the same day before expiry = expiring today, the expiry instant = overdue 1 day,
// each business day crossed after that adds 1; a daylight saving switch day is computed from date components, so 23- or 25-hour days do not affect it.
func TestComputeSubscriptionBusinessStatus(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	ny := mustLoc(t, "America/New_York")
	at := func(loc *time.Location, y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, loc)
	}

	cases := []struct {
		name        string
		loc         *time.Location
		expiresAt   time.Time
		graceUntil  time.Time
		now         time.Time
		wantStatus  string
		wantRemain  int
		wantOverdue int
	}{
		{"no expiry time", sh, time.Time{}, time.Time{}, at(sh, 2026, 9, 12, 10, 0), SubscriptionStatusUnknown, 0, 0},
		{"expires later the same day=expiring today", sh, at(sh, 2026, 9, 12, 23, 59), time.Time{}, at(sh, 2026, 9, 12, 8, 0), SubscriptionStatusExpiringToday, 0, 0},
		{"expires early the next day=1 day remaining", sh, at(sh, 2026, 9, 13, 0, 30), time.Time{}, at(sh, 2026, 9, 12, 23, 0), SubscriptionStatusActive, 1, 0},
		{"UTC time within the business day but UTC date already changed=still today", sh, at(sh, 2026, 9, 12, 23, 59), time.Time{}, at(sh, 2026, 9, 12, 10, 0).In(time.UTC), SubscriptionStatusExpiringToday, 0, 0},
		{"expires at this instant=overdue 1 day", sh, at(sh, 2026, 9, 12, 12, 0), time.Time{}, at(sh, 2026, 9, 12, 12, 0), SubscriptionStatusExpired, 0, 1},
		{"still the expiry day, later=overdue 1 day", sh, at(sh, 2026, 9, 12, 12, 0), time.Time{}, at(sh, 2026, 9, 12, 20, 0), SubscriptionStatusExpired, 0, 1},
		{"crossed one business day=overdue 1 day", sh, at(sh, 2026, 9, 12, 23, 59), time.Time{}, at(sh, 2026, 9, 13, 0, 1), SubscriptionStatusExpired, 0, 1},
		{"crossed two business days=overdue 2 days", sh, at(sh, 2026, 9, 12, 12, 0), time.Time{}, at(sh, 2026, 9, 14, 0, 1), SubscriptionStatusExpired, 0, 2},
		{"within grace period", sh, at(sh, 2026, 9, 12, 12, 0), at(sh, 2026, 9, 20, 0, 0), at(sh, 2026, 9, 14, 0, 1), SubscriptionStatusGracePeriod, 0, 2},
		{"after grace period ends=expired", sh, at(sh, 2026, 9, 12, 12, 0), at(sh, 2026, 9, 13, 0, 0), at(sh, 2026, 9, 14, 0, 1), SubscriptionStatusExpired, 0, 2},
		// 2026-11-01 New York daylight saving time ends (that day has 25 hours): 10-31 12:00 → 11-02 12:00 is actually 49 hours, still 2 days.
		{"DST end crossed a day boundary=2 days remaining", ny, at(ny, 2026, 11, 2, 12, 0), time.Time{}, at(ny, 2026, 10, 31, 12, 0), SubscriptionStatusActive, 2, 0},
		{"overdue days after DST end", ny, at(ny, 2026, 10, 31, 12, 0), time.Time{}, at(ny, 2026, 11, 2, 11, 0), SubscriptionStatusExpired, 0, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, remain, overdue := ComputeSubscriptionBusinessStatus(tc.expiresAt, tc.graceUntil, tc.now, tc.loc)
			if status != tc.wantStatus || remain != tc.wantRemain || overdue != tc.wantOverdue {
				t.Fatalf("got (%s, %d, %d), want (%s, %d, %d)", status, remain, overdue, tc.wantStatus, tc.wantRemain, tc.wantOverdue)
			}
		})
	}
}

// Plan tracking rules: paid plans are always tracked; free is tracked only when an expiry time is kept (a real expiry that dropped to free); api is not tracked.
func TestBuildSubscriptionStatusView(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)

	if v := BuildSubscriptionStatusView("api", past, SubscriptionMeta{}, now, time.UTC); v != nil {
		t.Fatalf("api plan should not be tracked, got %+v", v)
	}
	if v := BuildSubscriptionStatusView("free", time.Time{}, SubscriptionMeta{}, now, time.UTC); v != nil {
		t.Fatalf("free plan without expiry should not be tracked, got %+v", v)
	}
	v := BuildSubscriptionStatusView("free", past, SubscriptionMeta{}, now, time.UTC)
	if v == nil || v.BusinessStatus != SubscriptionStatusExpired || v.DaysOverdue != 2 {
		t.Fatalf("free plan with past expiry should be expired 2d, got %+v", v)
	}
	if v.Source != SubscriptionSourceJWT || v.SyncState != SubscriptionSyncUnknown || v.AutoRenew != SubscriptionAutoRenewUnknown {
		t.Fatalf("legacy defaults: source=%s sync=%s autoRenew=%s", v.Source, v.SyncState, v.AutoRenew)
	}

	meta := SubscriptionMeta{
		CheckedAt: now.Add(-time.Hour), SyncState: SubscriptionSyncConfirmed, Source: SubscriptionSourceProviderAPI,
		WillRenew: "true", RenewalDetectedAt: now.Add(-2 * time.Hour),
	}
	v = BuildSubscriptionStatusView("plus", now.Add(10*24*time.Hour), meta, now, time.UTC)
	if v == nil || v.BusinessStatus != SubscriptionStatusActive || v.DaysRemaining != 10 {
		t.Fatalf("plus active 10d, got %+v", v)
	}
	if v.AutoRenew != SubscriptionAutoRenewEnabled || v.LastCheckedAt == "" || v.RenewalDetectedAt == "" || v.Timezone != "UTC" {
		t.Fatalf("meta not carried: %+v", v)
	}
	// Awaiting confirmation (stale value already cleared): no expiry time, but sync status is pending.
	v = BuildSubscriptionStatusView("plus", time.Time{}, SubscriptionMeta{SyncState: SubscriptionSyncPending, Source: SubscriptionSourcePlanHeader, LastKnownStatus: SubscriptionStatusExpired}, now, time.UTC)
	if v == nil || v.BusinessStatus != SubscriptionStatusUnknown || v.SyncState != SubscriptionSyncPending || v.LastKnownStatus != SubscriptionStatusExpired {
		t.Fatalf("pending view: %+v", v)
	}
	// Provider has no subscription record: auto-renewal is classified as unsupported.
	v = BuildSubscriptionStatusView("k12", now.Add(24*time.Hour), SubscriptionMeta{SyncState: SubscriptionSyncUnsupported}, now, time.UTC)
	if v == nil || v.AutoRenew != SubscriptionAutoRenewUnsupported {
		t.Fatalf("unsupported view: %+v", v)
	}
}

// Metadata credentials round trip: all eight keys are written, and empty values are written as empty strings.
func TestSubscriptionMetaCredentialsRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	meta := SubscriptionMeta{CheckedAt: now, SyncState: SubscriptionSyncFailed, Source: SubscriptionSourceJWT, Error: "boom", LastKnownStatus: SubscriptionStatusActive, WillRenew: "false"}
	updates := meta.CredentialUpdates()
	if len(updates) != 8 {
		t.Fatalf("updates has %d keys, want 8", len(updates))
	}
	if updates[SubscriptionGraceUntilCredentialKey] != "" || updates[SubscriptionRenewalDetectedAtCredentialKey] != "" {
		t.Fatalf("zero times must serialize to empty: %+v", updates)
	}
	got := SubscriptionMetaFromCredentials(func(k string) string {
		v, _ := updates[k].(string)
		return v
	})
	if got != meta {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, meta)
	}
	// Unix seconds can also be read back (compatible with manual or third-party writes).
	if ts := parseSubscriptionTime("1789000000"); ts.IsZero() {
		t.Fatal("unix seconds should parse")
	}
}

// Clearing a stale value is no longer silent: sync status switches to pending, the source is plan_header, and it records the renewal detection time and
// the business status from before the clearing; an expiry time that has passed inside the grace period is not stale.
func TestClearStaleSubscriptionExpiresAt_RecordsPendingMeta(t *testing.T) {
	store := NewStore(nil, nil, nil)
	past := time.Now().Add(-72 * time.Hour)
	acc := &Account{DBID: 1, AccessToken: "at", Status: StatusReady, PlanType: "plus", SubscriptionExpiresAt: past}
	store.AddAccount(acc)

	var hooked *Account
	prev := OnStaleSubscriptionCleared
	OnStaleSubscriptionCleared = func(_ *Store, a *Account) { hooked = a }
	defer func() { OnStaleSubscriptionCleared = prev }()

	if !store.ClearStaleSubscriptionExpiresAt(acc) {
		t.Fatal("stale value should be cleared")
	}
	meta := acc.SubscriptionMetaSnapshot()
	if meta.SyncState != SubscriptionSyncPending || meta.Source != SubscriptionSourcePlanHeader {
		t.Fatalf("meta = %+v, want pending/plan_header", meta)
	}
	if meta.LastKnownStatus != SubscriptionStatusExpired || meta.RenewalDetectedAt.IsZero() {
		t.Fatalf("meta = %+v, want last_known=expired + renewal_detected_at", meta)
	}
	if hooked != acc {
		t.Fatal("OnStaleSubscriptionCleared hook should fire with the account")
	}
	view := acc.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != SubscriptionStatusUnknown || view.SyncState != SubscriptionSyncPending {
		t.Fatalf("view after clear = %+v", view)
	}

	// Inside the grace period: do not clear.
	grace := &Account{DBID: 2, AccessToken: "at", Status: StatusReady, PlanType: "plus", SubscriptionExpiresAt: past}
	grace.subscriptionMeta.GraceUntil = time.Now().Add(72 * time.Hour)
	store.AddAccount(grace)
	hooked = nil
	if store.ClearStaleSubscriptionExpiresAt(grace) {
		t.Fatal("expiry inside grace period must not be treated as stale")
	}
	if hooked != nil {
		t.Fatal("hook must not fire when nothing was cleared")
	}
	if got := grace.GetSubscriptionExpiresAt(); !got.Equal(past) {
		t.Fatalf("grace account expiry = %v, want %v", got, past)
	}
}

// RT refresh: an earlier expiry time in the token must not overwrite the authoritative value from the subscription provider; a later one is still written.
func TestCodexRefreshedCredentials_JWTDoesNotOverrideAuthoritativeExpiry(t *testing.T) {
	now := time.Now()
	authoritative := now.Add(40 * 24 * time.Hour)
	acc := &Account{DBID: 1, PlanType: "plus", SubscriptionExpiresAt: authoritative}
	acc.subscriptionMeta.Source = SubscriptionSourceProviderAPI
	td := &TokenData{AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: now.Add(time.Hour)}

	older := &AccountInfo{PlanType: "plus", SubscriptionExpiresAt: now.Add(10 * 24 * time.Hour)}
	updates := codexRefreshedCredentials(acc, td, older, "")
	if _, ok := updates["subscription_expires_at"]; ok {
		t.Fatalf("older JWT expiry must not overwrite provider value: %v", updates["subscription_expires_at"])
	}

	newer := &AccountInfo{PlanType: "plus", SubscriptionExpiresAt: now.Add(70 * 24 * time.Hour)}
	updates = codexRefreshedCredentials(acc, td, newer, "")
	if got := updates["subscription_expires_at"]; got != newer.SubscriptionExpiresAt.Format(time.RFC3339) {
		t.Fatalf("newer JWT expiry should be written, got %v", got)
	}
	if got := updates[SubscriptionSourceCredentialKey]; got != SubscriptionSourceJWT {
		t.Fatalf("source should flip to jwt when the token supplies a newer value, got %v", got)
	}

	// Behavior is unchanged when there is no authoritative source: the token value is written directly.
	legacy := &Account{DBID: 2, PlanType: "plus", SubscriptionExpiresAt: authoritative}
	updates = codexRefreshedCredentials(legacy, td, older, "")
	if got := updates["subscription_expires_at"]; got != older.SubscriptionExpiresAt.Format(time.RFC3339) {
		t.Fatalf("legacy account should accept JWT expiry, got %v", got)
	}
}
