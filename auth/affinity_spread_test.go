package auth

import (
	"fmt"
	"testing"

	"github.com/codex2api/database"
)

func newSpreadTestStore(t *testing.T, accounts int) *Store {
	t.Helper()
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gpt-5.4"})
	for i := 1; i <= accounts; i++ {
		store.AddAccount(&Account{DBID: int64(i), AccessToken: fmt.Sprintf("at-%d", i), PlanType: "plus"})
	}
	store.SetSessionAffinitySpread(true)
	return store
}

// The same affinity key always hits the same account while the account pool is unchanged (idempotent one-to-one binding, issue #484 requirement).
func TestFreshAffinitySpreadDeterministic(t *testing.T) {
	store := newSpreadTestStore(t, 3)

	var first int64
	for i := 0; i < 5; i++ {
		acc := store.nextAccountForFreshAffinity("affinity-pro-01", 0, nil, nil)
		if acc == nil {
			t.Fatal("no account selected")
		}
		if first == 0 {
			first = acc.DBID
		} else if acc.DBID != first {
			t.Fatalf("selection drifted: got %d, want stable %d", acc.DBID, first)
		}
		store.Release(acc)
	}
}

// Different affinity keys spread evenly across candidate accounts instead of all flocking to one account, which is the issue #484
// control case where 90% of 10 downstream keys pile onto one account.
func TestFreshAffinitySpreadDistributes(t *testing.T) {
	store := newSpreadTestStore(t, 3)

	distribution := make(map[int64]int)
	for i := 1; i <= 10; i++ {
		key := fmt.Sprintf("affinity-pro-%02d", i)
		acc := store.nextAccountForFreshAffinity(key, 0, nil, nil)
		if acc == nil {
			t.Fatalf("key %s selected no account", key)
		}
		distribution[acc.DBID]++
		store.Release(acc)
	}
	if len(distribution) < 2 {
		t.Fatalf("10 keys all landed on one account: %v", distribution)
	}
	for id, count := range distribution {
		if count > 7 {
			t.Fatalf("account %d took %d/10 keys, distribution too skewed: %v", id, count, distribution)
		}
	}
}

// When the hash-preferred account is full, the choice deterministically moves to the next one in hash order; after release, new bindings return to the preferred account.
func TestFreshAffinitySpreadFallsThroughWhenTopBusy(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	for i := 1; i <= 3; i++ {
		store.AddAccount(&Account{DBID: int64(i), AccessToken: fmt.Sprintf("at-%d", i), PlanType: "plus"})
	}
	store.SetSessionAffinitySpread(true)

	const key = "affinity-fallthrough"
	top := store.nextAccountForFreshAffinity(key, 0, nil, nil)
	if top == nil {
		t.Fatal("no account selected")
	}
	// The preferred account is full (MaxConcurrency=1); selecting the same key again must move to another account instead of failing.
	second := store.nextAccountForFreshAffinity(key, 0, nil, nil)
	if second == nil {
		t.Fatal("expected fall-through account while top is busy")
	}
	if second.DBID == top.DBID {
		t.Fatalf("fall-through returned the busy account %d", top.DBID)
	}
	store.Release(second)
	store.Release(top)

	again := store.nextAccountForFreshAffinity(key, 0, nil, nil)
	if again == nil || again.DBID != top.DBID {
		t.Fatalf("after release selection = %v, want hash-top %d", again, top.DBID)
	}
	store.Release(again)
}

// Scheduling priority is strictly layered: a lower-priority account must not cross into a higher-priority layer, even if its hash is larger.
func TestFreshAffinitySpreadHonorsSchedulerPriority(t *testing.T) {
	store := newSpreadTestStore(t, 3)
	priority := store.FindByID(2)
	if priority == nil {
		t.Fatal("account 2 missing")
	}
	priority.SetSchedulerPriority(10)

	for i := 1; i <= 6; i++ {
		key := fmt.Sprintf("affinity-prio-%d", i)
		acc := store.nextAccountForFreshAffinity(key, 0, nil, nil)
		if acc == nil {
			t.Fatalf("key %s selected no account", key)
		}
		if acc.DBID != 2 {
			t.Fatalf("key %s bypassed high-priority account, got %d", key, acc.DBID)
		}
		store.Release(acc)
	}
}

// When the switch is off, the original scheduling path is used; the default is off.
func TestFreshAffinitySpreadDisabledFallsBack(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&Account{DBID: 1, AccessToken: "at-1", PlanType: "plus"})
	if store.GetSessionAffinitySpread() {
		t.Fatal("spread should default to disabled")
	}
	acc := store.nextAccountForFreshAffinity("affinity-any", 0, nil, nil)
	if acc == nil {
		t.Fatal("disabled spread should still return an account via the classic scheduler")
	}
	store.Release(acc)
}

// End to end: keys selected through NextForSessionWithFilter spread out, while the reuse path still sticks to the bound account.
func TestNextForSessionWithFilterSpreadsFreshKeys(t *testing.T) {
	store := newSpreadTestStore(t, 3)

	bound := make(map[string]int64)
	distribution := make(map[int64]int)
	for i := 1; i <= 9; i++ {
		key := fmt.Sprintf("session-%d", i)
		acc, _ := store.NextForSessionWithFilter(key, 0, nil, nil)
		if acc == nil {
			t.Fatalf("key %s got no account", key)
		}
		store.BindSessionAffinity(key, acc, "")
		bound[key] = acc.DBID
		distribution[acc.DBID]++
		store.Release(acc)
	}
	if len(distribution) < 2 {
		t.Fatalf("fresh keys all clustered: %v", distribution)
	}
	// Reuse: a key that is already bound still returns to its original account.
	for key, want := range bound {
		acc, _ := store.NextForSessionWithFilter(key, 0, nil, nil)
		if acc == nil || acc.DBID != want {
			t.Fatalf("key %s rebound to %v, want sticky %d", key, acc, want)
		}
		store.Release(acc)
	}
}
