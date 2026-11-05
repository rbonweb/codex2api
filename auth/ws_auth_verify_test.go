package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// TestVerifyAccountAuthAsync verifies the auth verification probe after an abnormal WS close:
// one probe fires, and repeated triggers within 30s are skipped by throttling.
func TestVerifyAccountAuthAsync(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})

	var calls int32
	done := make(chan struct{}, 4)
	store.SetUsageProbeFunc(func(ctx context.Context, a *Account) error {
		atomic.AddInt32(&calls, 1)
		done <- struct{}{}
		return nil
	})

	acc := &Account{DBID: 1, AccessToken: "at"}

	store.VerifyAccountAuthAsync(acc)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected one auth verification probe to fire")
	}

	// Throttling: a second trigger within 30s should be skipped.
	store.VerifyAccountAuthAsync(acc)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("probe call count = %d, want 1 (should be throttled)", got)
	}
}

// TestVerifyAccountAuthAsyncNoProbeFunc does not panic and returns directly when no probe is registered.
func TestVerifyAccountAuthAsyncNoProbeFunc(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	store.VerifyAccountAuthAsync(&Account{DBID: 1, AccessToken: "at"}) // should not panic
}
