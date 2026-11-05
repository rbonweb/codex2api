package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestOverloadBreakerWindowCountsAndPrunes(t *testing.T) {
	b := &overloadBreaker{windows: make(map[int64]map[int64]*overloadBucket)}
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 4; i++ {
		b.record(1, false, 5, base)
	}
	total, over := b.record(1, true, 5, base.Add(2*time.Minute))
	if total != 5 || over != 1 {
		t.Fatalf("window counts = (%d,%d), want (5,1)", total, over)
	}

	// Old buckets outside the window must be dropped: after 6 minutes only the newest one remains.
	total, over = b.record(1, true, 5, base.Add(8*time.Minute))
	if total != 1 || over != 1 {
		t.Fatalf("pruned counts = (%d,%d), want (1,1)", total, over)
	}

	// Account-level isolation.
	total, over = b.record(2, false, 5, base.Add(8*time.Minute))
	if total != 1 || over != 0 {
		t.Fatalf("isolated counts = (%d,%d), want (1,0)", total, over)
	}

	b.reset(1)
	total, over = b.record(1, false, 5, base.Add(8*time.Minute))
	if total != 1 || over != 0 {
		t.Fatalf("after reset counts = (%d,%d), want (1,0)", total, over)
	}
}

func TestShouldTripOverloadHonorsMinSamplesAndRatio(t *testing.T) {
	// Too few samples: 1/1 = 100% does not trip either.
	if shouldTripOverload(1, 1, 20) {
		t.Fatal("should not trip below min samples")
	}
	if shouldTripOverload(4, 4, 20) {
		t.Fatal("should not trip below min samples (4)")
	}
	// Enough samples and enough ratio: 1 out of 5 = 20% ≥ 20%.
	if !shouldTripOverload(5, 1, 20) {
		t.Fatal("should trip at exactly the threshold")
	}
	// Ratio not met.
	if shouldTripOverload(10, 1, 20) {
		t.Fatal("10% should not trip a 20% threshold")
	}
	// Without overload errors it never trips.
	if shouldTripOverload(100, 0, 1) {
		t.Fatal("zero overloads must never trip")
	}
}

func TestNoteOverloadOutcomePausesAccountAndResetsWindow(t *testing.T) {
	prev := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexOverloadPauseEnabled = true
		s.CodexOverloadThresholdPercent = 20
		s.CodexOverloadPauseMinutes = 30
		s.CodexOverloadWindowMinutes = 5
		return s
	})
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return prev })
		globalOverloadBreaker.reset(9101)
	})
	globalOverloadBreaker.reset(9101)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 9101, AccessToken: "at-overload", AccountID: "acc-ovl", PlanType: "plus"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	okLog := &database.UsageLogInput{AccountID: 9101, StatusCode: 200, Channel: database.UpstreamChannelCodex}
	overloadLog := &database.UsageLogInput{
		AccountID: 9101, StatusCode: 503, Channel: database.UpstreamChannelCodex,
		ErrorMessage: "server_is_overloaded · service_unavailable_error · Our servers are currently overloaded.",
	}

	// 4 successes + 1 overload = 5 samples, 20% — trips exactly at the threshold.
	for i := 0; i < 4; i++ {
		handler.noteOverloadOutcome(okLog)
	}
	if account.HasActiveCooldown() {
		t.Fatal("account paused before threshold")
	}
	handler.noteOverloadOutcome(overloadLog)
	if !account.HasActiveCooldown() {
		t.Fatal("account should be paused after tripping the breaker")
	}
	if reason := account.GetCooldownReason(); reason != overloadPauseReason {
		t.Fatalf("cooldown reason = %q, want %q", reason, overloadPauseReason)
	}

	// 非 Codex 渠道与未启用开关都不参与统计。
	globalOverloadBreaker.reset(9101)
	otherLog := &database.UsageLogInput{AccountID: 9101, StatusCode: 503, Channel: "other",
		ErrorMessage: "server_is_overloaded"}
	for i := 0; i < 10; i++ {
		handler.noteOverloadOutcome(otherLog)
	}
	if total, _ := globalOverloadBreaker.record(9101, false, 5, time.Now()); total != 1 {
		t.Fatalf("non-codex logs leaked into codex overload window: total=%d", total)
	}
}

func TestIsOverloadedUsageErrorMatchesByCode(t *testing.T) {
	overloaded := &database.UsageLogInput{
		StatusCode:   503,
		ErrorMessage: "server_is_overloaded · service_unavailable_error · Our servers are currently overloaded. Please try again later.",
	}
	if !isOverloadedUsageError(overloaded) {
		t.Fatal("overload error should match by code")
	}
	if isOverloadedUsageError(&database.UsageLogInput{StatusCode: 200, ErrorMessage: "server_is_overloaded"}) {
		t.Fatal("2xx must not count as overload")
	}
	if isOverloadedUsageError(&database.UsageLogInput{StatusCode: 503, ErrorMessage: "HTTP 503"}) {
		t.Fatal("generic 503 without the code must not count")
	}
}
