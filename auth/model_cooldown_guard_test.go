package auth

import (
	"context"
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

func TestDropAccountModel(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251, Models: []string{"gpt-5.4", "gpt-5.5", "GPT-5.4-Mini"}}
	store.mu.Lock()
	store.accounts = []*Account{acc}
	store.mu.Unlock()

	removed, err := store.DropAccountModel(context.Background(), acc, "gpt-5.4")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	acc.mu.RLock()
	models := append([]string(nil), acc.Models...)
	acc.mu.RUnlock()
	if len(models) != 2 || models[0] != "gpt-5.5" || models[1] != "GPT-5.4-Mini" {
		t.Fatalf("models after drop = %v", models)
	}

	removed, err = store.DropAccountModel(context.Background(), acc, "gpt-5.2")
	if err != nil || removed {
		t.Fatalf("model not in whitelist must be a no-op: removed=%v err=%v", removed, err)
	}

	open := &Account{DBID: 252}
	removed, err = store.DropAccountModel(context.Background(), open, "gpt-5.4")
	if err != nil || removed {
		t.Fatalf("empty whitelist (allow-all) must not be rewritten: removed=%v err=%v", removed, err)
	}
}
