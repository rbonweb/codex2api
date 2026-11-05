package auth

import (
	"testing"
	"time"
)

func TestMarkModelCooldownWithBackoff_DoesNotShortenActiveLongerCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251}

	long := store.MarkModelCooldownWithBackoff(acc, "gpt-5.4", 30*time.Minute, "credits_required", false)
	short := store.MarkModelCooldownWithBackoff(acc, "gpt-5.4", 2*time.Second, "rate_limited_model", true)

	if short.ResetAt.Before(long.ResetAt) {
		t.Fatalf("a later short cooldown must not shorten the active 30m one: short=%s long=%s", short.ResetAt, long.ResetAt)
	}
	if short.Reason != "credits_required" {
		t.Fatalf("reason must stay the longer cooldown's reason, got %q", short.Reason)
	}
	acc.mu.RLock()
	stored := acc.ModelCooldowns["gpt-5.4"]
	acc.mu.RUnlock()
	if stored.ResetAt.Before(long.ResetAt) || stored.Reason != "credits_required" {
		t.Fatalf("stored cooldown = %+v", stored)
	}
}

func TestMarkModelCooldownWithBackoff_ExtendsExpiredCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251, ModelCooldowns: map[string]ModelCooldown{
		"gpt-5.4": {Model: "gpt-5.4", Reason: "credits_required", ResetAt: time.Now().Add(-time.Minute)},
	}}
	got := store.MarkModelCooldownWithBackoff(acc, "gpt-5.4", 2*time.Second, "rate_limited_model", true)
	if got.Reason != "rate_limited_model" || !got.ResetAt.After(time.Now()) {
		t.Fatalf("an expired cooldown must be replaced normally, got %+v", got)
	}
}
