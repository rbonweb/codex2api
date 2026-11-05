package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// plus5hExhausted builds a plus account whose 5h window is already full: without credits, it is rate_limited.
func plus5hExhausted() *Account {
	return &Account{
		DBID:                1,
		AccessToken:         "at-test",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(2 * time.Hour),
	}
}

// withCredits turns on both credit switches and writes a credit balance snapshot.
func withCredits(acc *Account, balance string, hasCredits, unlimited, overageReached bool) *Account {
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.CreditsValid = true
	acc.CreditsBalance = balance
	acc.CreditsHasCredits = hasCredits
	acc.CreditsUnlimited = unlimited
	acc.CreditsOverageLimitReached = overageReached
	return acc
}

func TestCreditsBalanceValue(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"1000.0000000000", 1000},
		{"981.7471800000", 981.74718},
		{" 12.5 ", 12.5},
		{"0", 0},
		{"0.0000000000", 0},
		{"", 0},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		if got := creditsBalanceValue(tc.raw); got != tc.want {
			t.Errorf("creditsBalanceValue(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// With credits, the usage window rate limit is overridden, the account stays schedulable, and the status shows using_credits.
func TestUsingCreditsSuppressesRateLimitWhenBalancePositive(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)

	if acc.IsPremium5hRateLimited() {
		t.Error("IsPremium5hRateLimited() = true, want false while credits cover the window")
	}
	// The display is still rate limited (the window is objectively full); whether it is schedulable is decided by IsAvailable,
	// "using credits to cover" is expressed by the parallel UsingCredits signal.
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true — the credits badge means it still gets scheduled")
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true while credits cover the window")
	}
}

func stringPtr(s string) *string {
	return &s
}

// After credits run out (has_credits=false), it must go back to a real rate limit, otherwise scheduling keeps sending requests to accounts that are bound to get 429.
func TestUsingCreditsFallsBackToRateLimitedWhenCreditsExhausted(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "0.0000000000", false, false, false)

	if !acc.IsPremium5hRateLimited() {
		t.Error("IsPremium5hRateLimited() = false, want true once credits are gone")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false once credits are gone")
	}
}

// Team member: when has_credits=true but balance is empty (hidden), credits take effect normally and override the rate limit. (issue #662)
func TestUsingCreditsAllowsTeamHiddenBalanceWhenHasCredits(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "", true, false, false)

	if acc.IsPremium5hRateLimited() {
		t.Error("IsPremium5hRateLimited() = true, want false when Team has_credits is true")
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true — Team account with hidden balance should be available")
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true for Team account with has_credits=true")
	}
}

// When the workspace spend limit is reached (spend_control.reached=true), credit use stops immediately. (issue #662)
func TestUsingCreditsBlockedBySpendControlReached(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "", true, false, false)
	reached := true
	acc.SetCreditBalanceDetails(nil, true, false, false, &reached, "")

	if acc.UsingCredits() {
		t.Fatal("UsingCredits() = true, want false when spend_control.reached is true")
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false when spend_control.reached is true")
	}
}

// When rate_limit_reached_type is a workspace-level hard-stop, credit use stops immediately. (issue #662)
func TestUsingCreditsBlockedByWorkspaceHardStop(t *testing.T) {
	for _, hardStopType := range []string{
		"workspace_owner_credits_depleted",
		"workspace_member_credits_depleted",
		"workspace_owner_usage_limit_reached",
		"workspace_member_usage_limit_reached",
	} {
		acc := withCredits(plus5hExhausted(), "1000", true, false, false)
		acc.SetRateLimitReachedType(hardStopType)

		if acc.UsingCredits() {
			t.Fatalf("UsingCredits() = true for %s, want false", hardStopType)
		}
		if acc.IsAvailable() {
			t.Fatalf("IsAvailable() = true for %s, want false", hardStopType)
		}
	}
}

// has_credits=false does not let requests through even if the balance string is non-zero: when upstream says there are none, there are none.
func TestUsingCreditsRequiresHasCreditsFlag(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", false, false, false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when has_credits is false", got)
	}
}

// overage_limit_reached is the upstream's authoritative signal that the overage quota is used up, and it takes priority over the balance number.
func TestUsingCreditsRespectsOverageLimitReached(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, true)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when overage limit is reached", got)
	}
}

// Unlimited-credit accounts always pass.
func TestUsingCreditsHonorsUnlimited(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "0", false, true, false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited for unlimited credits", got)
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true for unlimited credits")
	}
}

// An unknown balance (the wham probe has not run) is treated as no credits: it is better to wait out a window than to send requests to an account that will certainly get 429.
func TestUsingCreditsTreatsUnknownBalanceAsEmpty(t *testing.T) {
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.CreditsValid = false
	acc.CreditsBalance = "1000.0000000000"
	acc.CreditsHasCredits = true

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when the balance was never probed", got)
	}
}

// When neither switch is on, the credit balance does not take part in the decision at all, keeping the original rate limit semantics.
func TestUsingCreditsRequiresBothToggles(t *testing.T) {
	onlyEnabled := plus5hExhausted()
	onlyEnabled.CreditEnabled = true
	onlyEnabled.CreditsValid = true
	onlyEnabled.CreditsBalance = "1000"
	onlyEnabled.CreditsHasCredits = true
	if got := onlyEnabled.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited when skip toggle is off", got)
	}

	neither := plus5hExhausted()
	neither.CreditsValid = true
	neither.CreditsBalance = "1000"
	neither.CreditsHasCredits = true
	if got := neither.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited when both toggles are off", got)
	}
}

// A real 429 from upstream (going through cooldown) means the credits did not cover it, so the real state must be exposed and must not be masked by using_credits.
// A real 429 from /responses is recorded as ResponsesRateLimitedCooldownReason; rate_limited_5h is a local penalty.
func TestUsingCreditsDoesNotMaskUpstreamCooldown(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.SetCooldownUntil(time.Now().Add(30*time.Minute), ResponsesRateLimitedCooldownReason)

	if got := acc.RuntimeStatus(); got != ResponsesRateLimitedCooldownReason {
		t.Fatalf("RuntimeStatus() = %q, want %s — a real upstream 429 must not be hidden", got, ResponsesRateLimitedCooldownReason)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a real 429 means credits did not cover it")
	}
}

// A cooldown produced by a local usage-window penalty must not block "using credits": that is exactly what credits are meant to cover.
// This was a defect found in testing: blocking every cooldown across the board would make the badge never appear in the most common scenario
// (the switch is turned on only after finding the account rate limited).
func TestUsingCreditsIgnoresLocalUsageWindowCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))

	// First, naturally enter the usage-window penalty with no credits.
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}
	if !acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = false, want true for a locally derived usage cooldown")
	}

	// Then turn on the credit switch and top up credits: it should be recognized as "using credits to cover".
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("981.7471800000", true, false, false)

	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true — a local usage cooldown is exactly what credits cover")
	}
}

// Release applies only to local usage penalties: an upstream 429 cooldown (whose duration does not align with Reset7dAt) must be kept.
func TestReleaseUsageWindowCooldownKeepsUpstream429(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("1000", true, false, false)

	// An upstream 429: the cooldown duration comes from the rate-limit decision and will not land on the 7d reset time.
	store.MarkCooldown(acc, 15*time.Minute, "rate_limited")

	if acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = true, want false for an upstream 429 cooldown")
	}
	if store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("released an upstream 429 cooldown, want it preserved")
	}
	if !acc.HasActiveCooldown() {
		t.Fatal("upstream 429 cooldown was cleared, want it intact")
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a real 429 means credits did not cover it")
	}
}

// After the switch is turned on, the account should actively release the existing usage penalty and become schedulable again immediately.
func TestReleaseUsageWindowCooldownReturnsAccountToScheduling(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false while the usage cooldown stands")
	}

	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("1000", true, false, false)

	if !store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("ReleaseUsageWindowCooldownForCredits() = false, want true")
	}
	if acc.HasActiveCooldown() {
		t.Error("cooldown still active after release")
	}
	if !acc.IsAvailable() {
		t.Error("IsAvailable() = false after release, want true — the badge would otherwise be a lie")
	}
}

// Do not release when credits are zero: otherwise requests would be sent back to accounts that are bound to get 429.
func TestReleaseUsageWindowCooldownSkipsWhenNoCredits(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}

	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("0", false, false, false)

	if store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("released the cooldown with zero credits, want it preserved")
	}
	if !acc.HasActiveCooldown() {
		t.Fatal("cooldown cleared with zero credits")
	}
}

// An account that is using credits to cover shows as rate limited, but it is actually working normally: "one-click cleanup of rate-limited accounts" must skip it,
// otherwise one click would delete a good account. This is the direct risk of the design where an account shows rate limited yet still takes part in scheduling.
func TestCleanRateLimitedSkipsAccountsUsingCredits(t *testing.T) {
	store := NewStore(nil, nil, nil)

	covered := withCredits(plus5hExhausted(), "981.7471800000", true, false, false)
	covered.DBID = 1
	drained := withCredits(plus5hExhausted(), "0", false, false, false)
	drained.DBID = 2

	// Preconditions: both show as rate limited; the only difference is credits.
	if got := covered.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("covered RuntimeStatus() = %q, want rate_limited", got)
	}
	if got := drained.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("drained RuntimeStatus() = %q, want rate_limited", got)
	}
	if !covered.UsingCredits() {
		t.Fatal("covered UsingCredits() = false, want true")
	}
	if drained.UsingCredits() {
		t.Fatal("drained UsingCredits() = true, want false")
	}

	store.AddAccounts([]*Account{covered, drained})

	// Without a db, cleanup does not actually delete, but the count reflects which accounts were selected.
	cleaned := store.CleanRateLimitedManual(context.Background())
	if cleaned != 1 {
		t.Fatalf("CleanRateLimitedManual() = %d, want 1 (only the drained account)", cleaned)
	}
}

// The error state likewise takes priority over using_credits.
func TestUsingCreditsDoesNotMaskError(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.Status = StatusError
	acc.ErrorMsg = "boom"

	if got := acc.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
}

// using_credits should not be shown when the window is not full: that would describe a healthy account as burning credits.
func TestUsingCreditsOnlyWhenWindowActuallyExhausted(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.UsagePercent5h = 40

	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active while the window still has room", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a healthy window is not burning credits")
	}
}

// A Free account whose 7d quota is exhausted also goes through the credit gate: credits cover it, and when they reach zero it returns to usage_exhausted.
func TestUsingCreditsCoversFreeUsageExhausted(t *testing.T) {
	base := func() *Account {
		return &Account{
			DBID:                1,
			AccessToken:         "at-test",
			Status:              StatusReady,
			PlanType:            "free",
			UsagePercent7d:      100,
			UsagePercent7dValid: true,
		}
	}

	withBalance := withCredits(base(), "50.0000000000", true, false, false)
	if got := withBalance.RuntimeStatus(); got != "usage_exhausted" {
		t.Errorf("RuntimeStatus() = %q, want usage_exhausted for a free account with credits", got)
	}
	if !withBalance.IsAvailable() {
		t.Error("IsAvailable() = false, want true — credits keep a free account schedulable")
	}
	if !withBalance.UsingCredits() {
		t.Error("UsingCredits() = false, want true for a free account with credits")
	}

	drained := withCredits(base(), "0", true, false, false)
	if got := drained.RuntimeStatus(); got != "usage_exhausted" {
		t.Errorf("RuntimeStatus() = %q, want usage_exhausted once credits are gone", got)
	}
}

// ignore_usage_limit_status is another, independent switch: it only lets through already-bound active turns,
// new turns are still blocked, so the background keeps showing rate limited, but must not show as "using credits".
func TestIgnoreUsageLimitStatusKeepsFreshDispatchRateLimitedWithoutCredits(t *testing.T) {
	acc := plus5hExhausted()
	ignore := true
	acc.IgnoreUsageLimitStatusOverride = &ignore
	acc.recomputeEffectiveIgnoreUsageLimitStatus(false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited while fresh dispatch remains blocked", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — ignore-usage-limit is a separate switch, not credits")
	}
}

// The credit snapshot must survive a restart: if it stayed only in memory, the account would be judged "no credits" after a restart,
// credit coverage would stop working, and "cleanup of rate-limited accounts" would delete it as a real rate limit.
func TestCreditBalanceSnapshotRoundTrip(t *testing.T) {
	raw, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{
		Balance:    stringPtr("981.7471800000"),
		HasCredits: true,
		UpdatedAt:  time.Unix(1754200000, 0),
	})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}

	// Account after restart: both switches come from the database; when the credit snapshot is not restored, it is treated as "no credits".
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	if acc.UsingCredits() {
		t.Fatal("UsingCredits() = true before restore, want false — an unprobed balance is not credits")
	}

	if !acc.RestoreCreditBalanceFromJSON(raw) {
		t.Fatal("RestoreCreditBalanceFromJSON() = false, want true")
	}
	credits, ok := acc.GetCreditBalance()
	if !ok || credits.Balance == nil || *credits.Balance != "981.7471800000" || !credits.HasCredits || credits.Unlimited || credits.OverageLimitReached {
		t.Fatalf("GetCreditBalance() = %+v/%t, want balance 981.7471800000, has_credits, probed", credits, ok)
	}
	if !acc.UsingCredits() {
		t.Error("UsingCredits() = false after restore, want true — the restored balance must keep the override alive")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited — the status still shows rate limited, it is only covered by credits", got)
	}
}

// Compatible with JSON snapshots persisted by older versions (the balance field is a plain string, with no spend_control or rate_limit_reached_type)
func TestRestoreCreditBalanceFromOldJSONFormat(t *testing.T) {
	oldJSON := `{"balance":"1234.5000000000","has_credits":true,"unlimited":false,"overage_limit_reached":false,"updated_at":"2026-08-01T12:00:00Z"}`
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true

	if !acc.RestoreCreditBalanceFromJSON(oldJSON) {
		t.Fatal("RestoreCreditBalanceFromJSON(oldJSON) = false, want true")
	}
	credits, ok := acc.GetCreditBalance()
	if !ok || credits.Balance == nil || *credits.Balance != "1234.5000000000" || !credits.HasCredits || credits.Unlimited || credits.OverageLimitReached {
		t.Fatalf("GetCreditBalance() from old JSON = %+v/%t, want balance 1234.5000000000, has_credits, probed", credits, ok)
	}
	if !acc.UsingCredits() {
		t.Error("UsingCredits() = false after restoring old JSON, want true")
	}
}

// A missing snapshot, a corrupt snapshot, or an all-empty snapshot must never conjure a usable balance.
func TestRestoreCreditBalanceRejectsEmptyOrInvalid(t *testing.T) {
	empty, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}
	for _, raw := range []string{"", "   ", "not-json", empty} {
		acc := plus5hExhausted()
		acc.CreditEnabled = true
		acc.CreditSkipUsageWindow = true
		if acc.RestoreCreditBalanceFromJSON(raw) {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) = true, want false", raw)
		}
		if _, ok := acc.GetCreditBalance(); ok {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) marked the balance as probed", raw)
		}
		if acc.UsingCredits() {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) enabled the credits override", raw)
		}
	}
}

// A snapshot showing credits used up with none available must be restorable: it is the authoritative conclusion that "credits are used up", and after restore the real rate limit should apply,
// rather than fall back to "not probed" — otherwise after a restart requests would again be sent to accounts that are bound to get 429.
func TestRestoreCreditBalanceKeepsDrainedBalance(t *testing.T) {
	raw, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{Balance: stringPtr("0"), HasCredits: false})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	if !acc.RestoreCreditBalanceFromJSON(raw) {
		t.Fatal("RestoreCreditBalanceFromJSON() = false, want true for a drained balance")
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true for a zero balance with has_credits=false, want false")
	}
	if acc.IsAvailable() {
		t.Error("IsAvailable() = true for a drained credit account, want false")
	}
}

func TestPersistCreditBalance_SuccessAndRollbackOnDBError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "persist-credit-test.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	id, err := db.InsertAccountWithCredentials(ctx, "test-account", map[string]interface{}{
		"plan_type": "team",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &Account{
		DBID:        id,
		AccessToken: "at",
		PlanType:    "team",
	}

	bal := "50.00"
	sc := false
	store.PersistCreditBalance(account, &bal, true, false, false, &sc, "")

	account.mu.RLock()
	persistedKey := account.creditsPersistedKey
	account.mu.RUnlock()
	if persistedKey == "" {
		t.Fatal("expected creditsPersistedKey to be set after successful PersistCreditBalance")
	}

	accRow, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	codexCreditsRaw := accRow.GetCredential("codex_credits")
	if codexCreditsRaw == "" {
		t.Fatal("expected codex_credits credential to be written in DB, got empty")
	}

	// Close DB to force a database error on subsequent update with different balance
	_ = db.Close()
	closed = true

	bal2 := "40.00"
	store.PersistCreditBalance(account, &bal2, true, false, false, &sc, "")

	account.mu.RLock()
	rolledBackKey := account.creditsPersistedKey
	account.mu.RUnlock()
	if rolledBackKey != "" {
		t.Fatalf("expected creditsPersistedKey to be cleared after DB write failure, got %q", rolledBackKey)
	}
}

func TestPersistSparseCreditObservation_SuccessAndRollbackOnDBError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sparse-credit-test.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	id, err := db.InsertAccountWithCredentials(ctx, "test-sparse-account", map[string]interface{}{
		"plan_type": "team",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &Account{
		DBID:        id,
		AccessToken: "at",
		PlanType:    "team",
	}

	bal := "25.00"
	ov := false
	store.PersistSparseCreditObservation(account, true, false, &bal, &ov, "")

	account.mu.RLock()
	persistedKey := account.creditsPersistedKey
	account.mu.RUnlock()
	if persistedKey == "" {
		t.Fatal("expected creditsPersistedKey to be set after successful PersistSparseCreditObservation")
	}

	// Only balance changes are not persisted (the balance decreases per request and is left for the wham probe to refresh).
	bal2 := "10.00"
	store.PersistSparseCreditObservation(account, true, false, &bal2, &ov, "")
	account.mu.RLock()
	keyAfterBalanceOnly := account.creditsPersistedKey
	account.mu.RUnlock()
	if keyAfterBalanceOnly != persistedKey {
		t.Fatalf("balance-only sparse observation rewrote the snapshot: %q -> %q", persistedKey, keyAfterBalanceOnly)
	}

	// Close DB to force an error on the next flag change
	_ = db.Close()
	closed = true

	ovReached := true
	store.PersistSparseCreditObservation(account, true, false, &bal2, &ovReached, "")

	account.mu.RLock()
	rolledBackKey := account.creditsPersistedKey
	account.mu.RUnlock()
	if rolledBackKey != "" {
		t.Fatalf("expected creditsPersistedKey to be cleared after DB write failure, got %q", rolledBackKey)
	}
}

// When the balance is known to be 0, requests are not let through even if has_credits=true: this is the response that just spent the last credit.
func TestUsingCreditsBlockedWhenKnownBalanceZero(t *testing.T) {
	for _, balance := range []string{"0", "0.00", "0.0000000000"} {
		acc := withCredits(plus5hExhausted(), balance, true, false, false)
		acc.CreditsBalanceKnown = true
		if acc.UsingCredits() {
			t.Fatalf("UsingCredits() = true for known balance %q, want false", balance)
		}
		if acc.IsAvailable() {
			t.Fatalf("IsAvailable() = true for known balance %q, want false", balance)
		}
	}
	// unlimited ignores the balance.
	acc := withCredits(plus5hExhausted(), "0", true, true, false)
	acc.CreditsBalanceKnown = true
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false for unlimited credits, want true")
	}
}

// Old snapshots wrote the hidden balance as ""; after restore it must be treated as "unknown" rather than "0", otherwise Team accounts are first judged to have no credits after an upgrade.
func TestRestoreCreditBalanceLegacyEmptyBalanceMeansUnknown(t *testing.T) {
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	if !acc.RestoreCreditBalanceFromJSON(`{"balance":"","has_credits":true,"unlimited":false,"overage_limit_reached":false,"updated_at":"2026-08-01T12:00:00Z"}`) {
		t.Fatal("RestoreCreditBalanceFromJSON() = false, want true")
	}
	credits, ok := acc.GetCreditBalance()
	if !ok || credits.Balance != nil {
		t.Fatalf("restored credits = %+v/%t, want hidden balance (nil) and probed", credits, ok)
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false after restoring legacy hidden balance, want true")
	}
}

// When a full wham snapshot lacks spend_control / rate_limit_reached_type, the old hard-stop must be cleared.
func TestSetCreditBalanceDetailsClearsStaleWorkspaceHardStop(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "", true, false, false)
	reached := true
	acc.SetCreditBalanceDetails(nil, true, false, false, &reached, "workspace_member_credits_depleted")
	if acc.UsingCredits() {
		t.Fatal("UsingCredits() = true under hard stop, want false")
	}
	acc.SetCreditBalanceDetails(nil, true, false, false, nil, "")
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false after wham snapshot without hard stop, want true")
	}
	credits, _ := acc.GetCreditBalance()
	if credits.SpendControlReached != nil || credits.RateLimitReachedType != "" {
		t.Fatalf("stale hard stop survived wham snapshot: %+v", credits)
	}
}

// A missing rate_limit_reached_type response header means "not reached": a sparse observation must clear the old marker.
func TestApplySparseCreditsHeadersClearsReachedType(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "", true, false, false)
	acc.SetRateLimitReachedType("workspace_owner_credits_depleted")
	if acc.UsingCredits() {
		t.Fatal("UsingCredits() = true under hard stop, want false")
	}
	acc.ApplySparseCreditsHeaders(true, false, nil, nil, "")
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false after response without reached-type, want true")
	}
}

// plus5hNearAutoPause builds a plus account with 5h usage at 96% and a 95% threshold: without credits covering it, it is auto-paused.
func plus5hNearAutoPause() *Account {
	acc := &Account{
		DBID:                1,
		AccessToken:         "at-test",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		PlanType:            "plus",
		UsagePercent5h:      96,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(2 * time.Hour),
	}
	acc.effectiveAutoPause5h = 0.95
	return acc
}

// issue #791: the auto-pause threshold goes up to only 100%; if it overrode the credit switch, the window would never reach "spend credits once full",
// the dialog would show "in effect" and the list would show the "using credits" badge, yet the account would never be scheduled.
func TestCreditsBypassQuotaAutoPause(t *testing.T) {
	near := withCredits(plus5hNearAutoPause(), "62500.00", true, false, false)
	if !near.IsAvailable() {
		t.Fatal("IsAvailable() = false at 96% with credits covering, want auto-pause to yield")
	}
	if got := near.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active while credits cover the threshold", got)
	}

	full := withCredits(plus5hExhausted(), "62500.00", true, false, false)
	full.effectiveAutoPause5h = 0.95
	full.effectiveAutoPause7d = 0.9
	full.UsagePercent7d = 100
	full.UsagePercent7dValid = true
	full.Reset7dAt = time.Now().Add(72 * time.Hour)
	if !full.IsAvailable() {
		t.Fatal("IsAvailable() = false with both windows exhausted and credits covering, want true")
	}
	if !full.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true while credits cover an exhausted window")
	}
}

// When the credit gate is closed (switch off / credits at zero / workspace hard-stop), auto-pause still applies as usual.
func TestQuotaAutoPauseStillAppliesWithoutCredits(t *testing.T) {
	cases := map[string]*Account{
		"toggle off": func() *Account {
			acc := withCredits(plus5hNearAutoPause(), "62500.00", true, false, false)
			acc.CreditSkipUsageWindow = false
			return acc
		}(),
		"drained": withCredits(plus5hNearAutoPause(), "0", false, false, false),
		"hard stop": func() *Account {
			acc := withCredits(plus5hNearAutoPause(), "62500.00", true, false, false)
			acc.CreditsRateLimitReachedType = "workspace_member_credits_depleted"
			return acc
		}(),
	}
	for name, acc := range cases {
		if acc.IsAvailable() {
			t.Errorf("%s: IsAvailable() = true, want auto-pause to hold", name)
		}
		if got := acc.RuntimeStatus(); got != "quota_paused" {
			t.Errorf("%s: RuntimeStatus() = %q, want quota_paused", name, got)
		}
	}
}

// The 5h guardrail band is part of auto-pause: when credits cover it, concurrency must not be pushed to 0, and scheduling score must not be deducted.
func TestCreditsBypassAutoPause5hGuardBand(t *testing.T) {
	plain := plus5hNearAutoPause()
	plain.UsagePercent5h = 95.5 // within the threshold, inside the guardrail band
	plain.autoPause5hGuardBandPercent = 5
	plain.autoPause5hGuardConcurrency = 1

	covered := withCredits(plus5hNearAutoPause(), "62500.00", true, false, false)
	covered.UsagePercent5h = 100
	covered.autoPause5hGuardBandPercent = 5
	covered.autoPause5hGuardConcurrency = 1

	now := time.Now()
	covered.mu.RLock()
	limit := covered.quotaAutoPause5hGuardConcurrencyLimitLocked(8, now)
	penalty := covered.quotaAutoPause5hGuardDispatchPenaltyLocked(now)
	covered.mu.RUnlock()
	if limit != 8 {
		t.Errorf("guard concurrency limit = %d with credits covering, want 8 (untouched)", limit)
	}
	if penalty != 0 {
		t.Errorf("guard dispatch penalty = %v with credits covering, want 0", penalty)
	}

	// Control: without credits the guardrail still tightens as usual, confirming the assertions above are not vacuous.
	plain.UsagePercent5h = 96
	plain.mu.RLock()
	plainLimit := plain.quotaAutoPause5hGuardConcurrencyLimitLocked(8, now)
	plain.mu.RUnlock()
	if plainLimit != 0 {
		t.Errorf("guard concurrency limit = %d past threshold without credits, want 0", plainLimit)
	}
}

// issue #791: Plus/Pro first hits 5h (the response header / WHAM sets rate_limited_5h), and then the credit switch is turned on,
// the penalty must be released and must not wait idly for the 5h reset.
func TestReleaseUsageWindowCooldownReleasesPremium5hPenalty(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	store.MarkPremium5hRateLimited(acc, time.Now().Add(2*time.Hour))
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false while the 5h penalty stands")
	}

	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("62500.00", true, false, false)

	if !acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = false, want true for a local rate_limited_5h penalty")
	}
	if !store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("ReleaseUsageWindowCooldownForCredits() = false, want the 5h penalty released")
	}
	if acc.HasActiveCooldown() {
		t.Error("cooldown still active after release")
	}
	if !acc.IsAvailable() {
		t.Error("IsAvailable() = false after release, want true")
	}
	if !acc.UsingCredits() {
		t.Error("UsingCredits() = false after release, want true — the 5h window is still exhausted")
	}
}

// The 5h cooldown from a real 429 on /responses (ResponsesRateLimitedCooldownReason) is an upstream rejection, and credits do not let it through.
func TestReleaseUsageWindowCooldownKeepsUpstream5h429(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("62500.00", true, false, false)
	store.MarkResponsesPremium5hRateLimited(acc, time.Now().Add(2*time.Hour))

	if acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = true, want false for an upstream 5h 429")
	}
	if store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("released an upstream 5h 429 cooldown, want it preserved")
	}
	if acc.IsAvailable() {
		t.Error("IsAvailable() = true, want the upstream 429 cooldown to hold")
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — upstream rejected despite credits")
	}
}
