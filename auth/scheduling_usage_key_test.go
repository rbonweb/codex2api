package auth

import (
	"testing"
)

func TestUsagePercentForSchedulingCodexPathUnchanged(t *testing.T) {
	acc := &Account{DBID: 1, AccessToken: "token", UsagePercent7d: 42, UsagePercent7dValid: true}
	if got := acc.usagePercentForScheduling(); got != 42 {
		t.Fatalf("codex 7d usage: got %v, want 42", got)
	}
	acc.UsagePercent7dValid = false
	if got := acc.usagePercentForScheduling(); got != 0 {
		t.Fatalf("codex without valid 7d usage: got %v, want 0", got)
	}
}

func TestAntigravitySchedulingUsedPercent(t *testing.T) {
	frac := func(v float64) *float64 { return &v }
	// 有 quota_groups 时取最紧的桶。
	grouped := AntigravityQuotaSnapshot{
		Models: []AntigravityModelQuota{{ModelID: "a", RemainingFraction: frac(1), FractionPresent: true}},
		Groups: []AntigravityQuotaGroup{{Buckets: []AntigravityQuotaBucket{{RemainingFraction: frac(0.9), FractionPresent: true}, {RemainingFraction: frac(0.3), FractionPresent: true}}}},
	}
	if pct, ok := antigravitySchedulingUsedPercent(grouped); !ok || pct != 70 {
		t.Fatalf("grouped: got (%v, %v), want (70, true)", pct, ok)
	}
	// 无分组时取模型平均剩余。
	modelsOnly := AntigravityQuotaSnapshot{Models: []AntigravityModelQuota{{RemainingFraction: frac(0.5), FractionPresent: true}, {RemainingFraction: frac(1), FractionPresent: true}}}
	if pct, ok := antigravitySchedulingUsedPercent(modelsOnly); !ok || pct != 25 {
		t.Fatalf("models only: got (%v, %v), want (25, true)", pct, ok)
	}
	// 无 presence 的 legacy 数值不可信，不产生排序键。
	legacyOnly := AntigravityQuotaSnapshot{Models: []AntigravityModelQuota{{RemainingFraction: frac(0.5)}}}
	if _, ok := antigravitySchedulingUsedPercent(legacyOnly); ok {
		t.Fatal("legacy snapshot without presence must not produce a sort key")
	}
	if _, ok := antigravitySchedulingUsedPercent(AntigravityQuotaSnapshot{Forbidden: true, Models: []AntigravityModelQuota{{RemainingFraction: frac(0), FractionPresent: true}}}); ok {
		t.Fatal("forbidden snapshot must not produce a sort key")
	}
	if _, ok := antigravitySchedulingUsedPercent(AntigravityQuotaSnapshot{}); ok {
		t.Fatal("empty snapshot must not produce a sort key")
	}
}
