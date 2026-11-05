package database

import (
	"context"
	"testing"
)

// The settings upsert maps columns to numbered parameters by hand, so this round trip guards the keepalive slots column and both preserve flags against a parameter shift.
func TestSystemSettingsRoundTripKeepaliveSlotsAndPreserveFlags(t *testing.T) {
	db := newSQLiteTestDB(t)
	ctx := context.Background()

	if err := db.UpdateSystemSettings(ctx, &SystemSettings{
		PromptFilterCustomPatterns:      "seed-patterns",
		PromptFilterReviewAPIKey:        "seed-key",
		CodexWSDownstreamKeepaliveSlots: 3,
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got.CodexWSDownstreamKeepaliveSlots != 3 || got.PromptFilterCustomPatterns != "seed-patterns" || got.PromptFilterReviewAPIKey != "seed-key" {
		t.Fatalf("seed read back slots=%d patterns=%q key=%q", got.CodexWSDownstreamKeepaliveSlots, got.PromptFilterCustomPatterns, got.PromptFilterReviewAPIKey)
	}

	if err := db.UpdateSystemSettings(ctx, &SystemSettings{
		PromptFilterCustomPatterns:         "stale-patterns",
		PromptFilterReviewAPIKey:           "stale-key",
		CodexWSDownstreamKeepaliveSlots:    7,
		PreservePromptFilterCustomPatterns: true,
		PreservePromptFilterReviewAPIKey:   true,
	}); err != nil {
		t.Fatalf("preserving update: %v", err)
	}
	got, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got.CodexWSDownstreamKeepaliveSlots != 7 || got.PromptFilterCustomPatterns != "seed-patterns" || got.PromptFilterReviewAPIKey != "seed-key" {
		t.Fatalf("preserved read back slots=%d patterns=%q key=%q, want 7 seed-patterns seed-key", got.CodexWSDownstreamKeepaliveSlots, got.PromptFilterCustomPatterns, got.PromptFilterReviewAPIKey)
	}

	if err := db.UpdateSystemSettings(ctx, &SystemSettings{
		PromptFilterCustomPatterns:      "new-patterns",
		PromptFilterReviewAPIKey:        "new-key",
		CodexWSDownstreamKeepaliveSlots: 12,
	}); err != nil {
		t.Fatalf("overwriting update: %v", err)
	}
	got, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got.CodexWSDownstreamKeepaliveSlots != 12 || got.PromptFilterCustomPatterns != "new-patterns" || got.PromptFilterReviewAPIKey != "new-key" {
		t.Fatalf("overwritten read back slots=%d patterns=%q key=%q, want 12 new-patterns new-key", got.CodexWSDownstreamKeepaliveSlots, got.PromptFilterCustomPatterns, got.PromptFilterReviewAPIKey)
	}
}
