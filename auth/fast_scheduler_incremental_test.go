package auth

import (
	"testing"
)

// TestFastSchedulerBulkInsertDoesNotResortPerEntry locks in that "the write side no longer re-sorts the whole bucket for each entry".
// Before the fix, every Update/insert re-sorted the whole bucket and rebuilt the full position index, so with tens of thousands of accounts the scheduler
// exclusive lock was saturated, and the whole gateway queued up during a bulk import (showing up as the service hanging during import).
func TestFastSchedulerBulkInsertDoesNotResortPerEntry(t *testing.T) {
	const seed = 300
	const added = 200

	scheduler := NewFastScheduler(2, "round_robin")
	accounts := make([]*Account, 0, seed)
	for i := 1; i <= seed; i++ {
		accounts = append(accounts, newFastSchedulerTestAccount(int64(i), HealthTierHealthy, float64(i%37), 2))
	}
	scheduler.Rebuild(accounts)

	before := scheduler.ResortCount()
	for i := 1; i <= added; i++ {
		scheduler.Update(newFastSchedulerTestAccount(int64(seed+i), HealthTierHealthy, float64(i%37), 2))
	}
	writeResorts := scheduler.ResortCount() - before
	if writeResorts != 0 {
		t.Fatalf("bulk writes triggered %d whole-bucket re-sorts, expected 0 (re-sorting should be deferred and merged before account pick)", writeResorts)
	}

	// Re-sorting happens only when an account is picked, and a batch of writes is merged into a single re-sort.
	if acc := scheduler.Acquire(); acc == nil {
		t.Fatal("Acquire() returned nil, expected an account")
	} else {
		scheduler.Release(acc)
	}
	if got := scheduler.ResortCount() - before; got != 1 {
		t.Fatalf("total re-sorts after account pick: %d, expected exactly 1", got)
	}
}

// TestFastSchedulerIncrementalOrderMatchesRebuild ensures the bucket order after lazy re-sorting matches
// a full Rebuild exactly: the incremental path must not change scheduling semantics.
func TestFastSchedulerIncrementalOrderMatchesRebuild(t *testing.T) {
	for _, mode := range []string{"round_robin", "remaining_quota", "fill_first", "dispatch_score"} {
		t.Run(mode, func(t *testing.T) {
			const n = 60

			build := func() []*Account {
				accounts := make([]*Account, 0, n)
				for i := 1; i <= n; i++ {
					acc := newFastSchedulerTestAccount(int64(i), HealthTierHealthy, float64((i*7)%23), 2)
					acc.UsagePercent7d = float64((i * 13) % 41)
					acc.UsagePercent7dValid = true
					acc.SchedulerPriority = int64(i % 3)
					accounts = append(accounts, acc)
				}
				return accounts
			}

			// Reference group: one-off Rebuild.
			reference := NewFastScheduler(2, mode)
			reference.Rebuild(build())

			// Comparison group: lazy re-sort after one Update per entry.
			incremental := NewFastScheduler(2, mode)
			for _, acc := range build() {
				incremental.Update(acc)
			}
			incremental.mu.Lock()
			incremental.ensureSortedLocked()
			incremental.mu.Unlock()

			reference.mu.RLock()
			want := append([]fastSchedulerEntry(nil), reference.buckets[HealthTierHealthy]...)
			reference.mu.RUnlock()
			incremental.mu.RLock()
			got := append([]fastSchedulerEntry(nil), incremental.buckets[HealthTierHealthy]...)
			incremental.mu.RUnlock()

			if len(want) != len(got) {
				t.Fatalf("bucket length %d, expected %d", len(got), len(want))
			}
			for i := range want {
				if want[i].dbID != got[i].dbID {
					t.Fatalf("position %d is dbID=%d, expected %d (incremental path order differs from Rebuild)",
						i, got[i].dbID, want[i].dbID)
				}
			}
		})
	}
}

// TestFastSchedulerRemoveKeepsPositionsConsistent covers "fill the hole with the last entry" style removal:
// after removing a middle entry, the position index of each remaining account must still point to the correct bucket index.
func TestFastSchedulerRemoveKeepsPositionsConsistent(t *testing.T) {
	const n = 20
	scheduler := NewFastScheduler(2, "round_robin")
	accounts := make([]*Account, 0, n)
	for i := 1; i <= n; i++ {
		accounts = append(accounts, newFastSchedulerTestAccount(int64(i), HealthTierHealthy, float64(i), 2))
	}
	scheduler.Rebuild(accounts)

	for _, dbID := range []int64{5, 1, 20, 13} {
		scheduler.Remove(dbID)
	}

	scheduler.mu.RLock()
	defer scheduler.mu.RUnlock()
	entries := scheduler.buckets[HealthTierHealthy]
	if len(entries) != n-4 {
		t.Fatalf("%d entries left in the bucket after removing 4, expected %d", len(entries), n-4)
	}
	if len(scheduler.positions) != len(entries) {
		t.Fatalf("positions has %d items, bucket has %d items, index and bucket are out of sync", len(scheduler.positions), len(entries))
	}
	for idx, entry := range entries {
		pos, ok := scheduler.positions[entry.dbID]
		if !ok {
			t.Fatalf("dbID=%d is in the bucket but has no position index", entry.dbID)
		}
		if pos.index != idx || pos.tier != HealthTierHealthy {
			t.Fatalf("dbID=%d position index is %+v, expected index=%d tier=%v", entry.dbID, pos, idx, HealthTierHealthy)
		}
	}
	for _, dbID := range []int64{5, 1, 20, 13} {
		if _, ok := scheduler.positions[dbID]; ok {
			t.Fatalf("dbID=%d was deleted but its position index is still there", dbID)
		}
	}
}
