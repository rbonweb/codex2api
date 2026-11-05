package auth

import (
	"testing"

	"github.com/codex2api/database"
)

func TestGroupNameCacheSetResolveDelete(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 8})

	store.SetGroupName(5, "fast")
	store.SetGroupName(6, "  slow  ") // should TrimSpace
	store.SetGroupName(7, "bulk")

	got := store.ResolveGroupNames([]int64{5, 6, 99}) // 99 is unknown, skipped
	want := []string{"fast", "slow"}
	if len(got) != len(want) {
		t.Fatalf("ResolveGroupNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolveGroupNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Rename takes effect
	store.SetGroupName(5, "fast-v2")
	if got := store.ResolveGroupNames([]int64{5}); len(got) != 1 || got[0] != "fast-v2" {
		t.Fatalf("after rename ResolveGroupNames = %v, want [fast-v2]", got)
	}

	// No longer resolved after deletion
	store.DeleteGroupName(5)
	if got := store.ResolveGroupNames([]int64{5, 7}); len(got) != 1 || got[0] != "bulk" {
		t.Fatalf("after deletion ResolveGroupNames = %v, want [bulk]", got)
	}

	// Empty input returns nil
	if got := store.ResolveGroupNames(nil); got != nil {
		t.Fatalf("empty input should return nil, got %v", got)
	}
}
