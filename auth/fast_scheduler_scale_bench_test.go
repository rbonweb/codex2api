package auth

import (
	"strconv"
	"testing"
)

func BenchmarkFastSchedulerAcquire10000(b *testing.B) { benchmarkFastSchedulerAcquire(b, 10000) }
func BenchmarkFastSchedulerAcquire50000(b *testing.B) { benchmarkFastSchedulerAcquire(b, 50000) }

// BenchmarkStoreAddAccount measures how the cost of "adding one account to an existing pool" changes with pool size.
// This path holds the global Store write lock, and every request (scheduling account picks, account listing) has to queue behind it,
// so it must be independent of pool size: otherwise a bulk import would stall the whole gateway.
func BenchmarkStoreAddAccount(b *testing.B) {
	for _, pool := range []int{1000, 5000, 20000} {
		b.Run("pool="+strconv.Itoa(pool), func(b *testing.B) {
			store := newBenchStore(pool)
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				store.AddAccount(newFastSchedulerTestAccount(int64(pool+i+1), HealthTierHealthy, float64(i%97), 2))
			}
		})
	}
}

// BenchmarkFastSchedulerUpdate measures a single scheduler entry update in steady state (every request Release,
// every usage refresh, and every cooldown mark goes through here). It must likewise be independent of pool size.
func BenchmarkFastSchedulerUpdate(b *testing.B) {
	for _, pool := range []int{1000, 5000, 20000} {
		b.Run("pool="+strconv.Itoa(pool), func(b *testing.B) {
			store := newBenchStore(pool)
			scheduler := store.getFastScheduler()
			hot := store.accounts[pool/2]
			b.ResetTimer()
			for b.Loop() {
				scheduler.Update(hot)
			}
		})
	}
}

func newBenchStore(n int) *Store {
	store := &Store{}
	for i := 1; i <= n; i++ {
		store.accounts = append(store.accounts, newFastSchedulerTestAccount(int64(i), HealthTierHealthy, float64(i%97), 2))
	}
	store.rebuildAccountIndex()
	store.SetFastSchedulerEnabled(true)
	store.getFastScheduler()
	return store
}
