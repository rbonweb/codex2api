package auth

import (
	"reflect"
	"testing"
)

func TestResolveProxyForAccountPrefersAccountProxy(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
	}
	account := &Account{
		DBID:     7,
		ProxyURL: " http://account-proxy:8080 ",
	}

	got := store.ResolveProxyForAccount(account)
	want := "http://account-proxy:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

func TestResolveProxyForAccountUsesStickyProxyPool(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool: []string{
			"http://pool-1:8080",
			"http://pool-2:8080",
			"http://pool-3:8080",
		},
	}

	cases := []struct {
		id   int64
		want string
	}{
		{id: 1, want: "http://pool-1:8080"},
		{id: 2, want: "http://pool-2:8080"},
		{id: 3, want: "http://pool-3:8080"},
		{id: 4, want: "http://pool-1:8080"},
	}
	for _, tc := range cases {
		account := &Account{DBID: tc.id}
		first := store.ResolveProxyForAccount(account)
		second := store.ResolveProxyForAccount(account)
		if first != tc.want || second != tc.want {
			t.Fatalf("account %d proxy = %q/%q, want sticky %q", tc.id, first, second, tc.want)
		}
	}
}

func TestResolveProxyForAccountFallsBackToGlobalProxy(t *testing.T) {
	store := &Store{
		globalProxy:      " http://global-proxy:8080 ",
		proxyPoolEnabled: false,
		proxyPool:        []string{"http://pool-1:8080"},
	}

	got := store.ResolveProxyForAccount(&Account{DBID: 1})
	want := "http://global-proxy:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

func TestResolveProxyForAccountSkipsBlankPoolEntries(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"", " http://pool-2:8080 "},
	}

	got := store.ResolveProxyForAccount(&Account{DBID: 1})
	want := "http://pool-2:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

// Group proxy priority: account proxy > group proxy > sticky proxy pool > global (issue #479).
func TestResolveProxyForAccountGroupProxyBeatsPoolAndGlobal(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
	}
	store.SetGroupProxyURLs(5, []string{" http://group-proxy:8080 "})

	got := store.ResolveProxyForAccount(&Account{DBID: 1, GroupIDs: []int64{5}})
	if want := "http://group-proxy:8080"; got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want group proxy %q", got, want)
	}

	// The account's own proxy still has the highest priority.
	got = store.ResolveProxyForAccount(&Account{DBID: 1, GroupIDs: []int64{5}, ProxyURL: "http://account-proxy:8080"})
	if want := "http://account-proxy:8080"; got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want account proxy %q", got, want)
	}
}

// Within a group, multiple proxies are sticky by account ID: the same account always hits the same one and does not drift between calls.
func TestResolveProxyForAccountGroupProxySticky(t *testing.T) {
	store := &Store{}
	store.SetGroupProxyURLs(5, []string{
		"http://g-1:8080",
		"http://g-2:8080",
		"http://g-3:8080",
	})

	seen := make(map[int64]string)
	for _, id := range []int64{1, 2, 3, 4, 5, 6} {
		account := &Account{DBID: id, GroupIDs: []int64{5}}
		first := store.ResolveProxyForAccount(account)
		second := store.ResolveProxyForAccount(account)
		if first == "" || first != second {
			t.Fatalf("account %d proxy not sticky: %q vs %q", id, first, second)
		}
		seen[id] = first
	}
	// Sticky hashing should spread different accounts across multiple proxies, not pile them all onto one.
	distinct := make(map[string]struct{})
	for _, proxy := range seen {
		distinct[proxy] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("expected accounts spread across proxies, all got %v", seen)
	}
}

// Multiple group membership: take the first group in GroupIDs order that has a proxy configured.
func TestResolveProxyForAccountMultiGroupFirstConfiguredWins(t *testing.T) {
	store := &Store{}
	store.SetGroupProxyURLs(9, []string{"http://group9:8080"})

	// Group 3 has no proxy configured, so skip it and hit group 9.
	got := store.ResolveProxyForAccount(&Account{DBID: 1, GroupIDs: []int64{3, 9}})
	if want := "http://group9:8080"; got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

// Hot update: a group proxy change takes effect immediately; after clearing, fall back to the global chain.
func TestSetGroupProxyURLsHotUpdate(t *testing.T) {
	store := &Store{globalProxy: "http://global-proxy:8080"}
	account := &Account{DBID: 1, GroupIDs: []int64{5}}

	store.SetGroupProxyURLs(5, []string{"http://old:8080"})
	if got := store.ResolveProxyForAccount(account); got != "http://old:8080" {
		t.Fatalf("before update proxy = %q", got)
	}
	store.SetGroupProxyURLs(5, []string{"http://new:8080"})
	if got := store.ResolveProxyForAccount(account); got != "http://new:8080" {
		t.Fatalf("after update proxy = %q, want new", got)
	}
	// An all-blank list is equivalent to clearing.
	store.SetGroupProxyURLs(5, []string{" ", ""})
	if got := store.ResolveProxyForAccount(account); got != "http://global-proxy:8080" {
		t.Fatalf("after clear proxy = %q, want global fallback", got)
	}
}

func TestResolveProxyForAccountSkipsDisabledManagedPin(t *testing.T) {
	const (
		disabledURL = "http://disabled.example:8080"
		enabledURL  = "http://enabled.example:8080"
	)
	store := &Store{
		proxyPoolEnabled: true,
		proxyPool:        []string{enabledURL},
		proxyPoolSet:     buildProxyPoolSet([]string{enabledURL}),
		managedProxySet:  buildProxyPoolSet([]string{disabledURL, enabledURL}),
		globalProxy:      "http://global-proxy:8080",
	}
	pinned := &Account{DBID: 1, ProxyURL: disabledURL}
	if got := store.ResolveProxyForAccount(pinned); got != "" {
		t.Fatalf("pinned disabled proxy = %q, want empty fail-closed", got)
	}
	if store.AccountHasUsableEgress(pinned) {
		t.Fatal("pinned disabled proxy should not be schedulable while the pool is on")
	}

	unbound := &Account{DBID: 2}
	if got := store.ResolveProxyForAccount(unbound); got != enabledURL {
		t.Fatalf("unbound proxy = %q, want remaining pool member %q", got, enabledURL)
	}
}

func TestResolveProxyForAccountKeepsCustomProxyOutsidePool(t *testing.T) {
	store := &Store{
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool.example:8080"},
		proxyPoolSet:     buildProxyPoolSet([]string{"http://pool.example:8080"}),
		managedProxySet:  buildProxyPoolSet([]string{"http://pool.example:8080"}),
	}
	custom := "http://custom.example:8080"
	got := store.ResolveProxyForAccount(&Account{DBID: 1, ProxyURL: custom})
	if got != custom {
		t.Fatalf("custom proxy = %q, want %q", got, custom)
	}
}

func TestNextSkipsPinnedDisabledProxyWhenPoolEnabled(t *testing.T) {
	const (
		disabledURL = "http://disabled.example:8080"
		enabledURL  = "http://enabled.example:8080"
	)
	store := (&Store{
		proxyPoolEnabled: true,
		proxyPool:        []string{enabledURL},
		proxyPoolSet:     buildProxyPoolSet([]string{enabledURL}),
		managedProxySet:  buildProxyPoolSet([]string{disabledURL, enabledURL}),
	}).withMaxConcurrency(2)
	store.AddAccount(&Account{DBID: 1, AccessToken: "tok-pinned", ProxyURL: disabledURL})
	store.AddAccount(&Account{DBID: 2, AccessToken: "tok-unbound"})

	selected := store.NextExcludingWithFilter(0, nil, nil)
	if selected == nil {
		t.Fatal("expected unbound account to remain schedulable")
	}
	defer store.Release(selected)
	if selected.DBID != 2 {
		t.Fatalf("selected account %d, want unbound 2", selected.DBID)
	}
	if got := store.ResolveProxyForAccount(selected); got != enabledURL {
		t.Fatalf("selected proxy = %q, want %q", got, enabledURL)
	}
}

func TestNextReturnsNilWhenPoolEnabledAndNoUsableProxy(t *testing.T) {
	store := (&Store{
		proxyPoolEnabled: true,
	}).withMaxConcurrency(2)
	store.AddAccount(&Account{DBID: 1, AccessToken: "tok-1"})
	if selected := store.NextExcludingWithFilter(0, nil, nil); selected != nil {
		t.Fatalf("selected account %d, want none when pool is on and empty", selected.DBID)
	}
}

// Sticky session check: after a group proxy changes, a binding stuck to the old proxy must be judged invalid so it gets rebound.
func TestAffinityProxyStillValidTracksGroupProxy(t *testing.T) {
	store := &Store{}
	account := &Account{DBID: 42, AccessToken: "at", GroupIDs: []int64{5}}
	store.accounts = []*Account{account}
	store.rebuildAccountIndex()
	store.SetGroupProxyURLs(5, []string{"http://group-a:8080"})

	if !store.affinityProxyStillValid(42, "http://group-a:8080") {
		t.Fatal("current group proxy should be valid")
	}
	store.SetGroupProxyURLs(5, []string{"http://group-b:8080"})
	if store.affinityProxyStillValid(42, "http://group-a:8080") {
		t.Fatal("stale group proxy should be invalid after update")
	}
	if !store.affinityProxyStillValid(42, "http://group-b:8080") {
		t.Fatal("new group proxy should be valid")
	}
}

// UnusableManagedProxies is the basis for batch import warnings, and its semantics must match the single-entry check exactly:
// the fail-closed decision in ResolveProxyForAccount is identical: a managed proxy that is not in the enabled set,
// and any account bound to it is always not schedulable.
func TestUnusableManagedProxies(t *testing.T) {
	const (
		disabledURL = "http://disabled.example:8080"
		enabledURL  = "http://enabled.example:8080"
		customURL   = "http://custom.example:8080"
	)
	store := &Store{
		proxyPoolEnabled: true,
		proxyPool:        []string{enabledURL},
		proxyPoolSet:     buildProxyPoolSet([]string{enabledURL}),
		managedProxySet:  buildProxyPoolSet([]string{disabledURL, enabledURL}),
	}

	// Pick out only the one that is managed but not enabled; enabled ones, and custom proxies that are not in the proxy table at all, do not count.
	got := store.UnusableManagedProxies([]string{enabledURL, " " + disabledURL + " ", customURL, "", disabledURL})
	want := []string{disabledURL, disabledURL}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnusableManagedProxies = %v, want %v", got, want)
	}

	// Aligned with the single-entry check: whatever is reported unusable here, ResolveProxyForAccount is guaranteed to fail-closed.
	for _, unusable := range got {
		account := &Account{DBID: 1, ProxyURL: unusable}
		if store.ResolveProxyForAccount(account) != "" || store.AccountHasUsableEgress(account) {
			t.Fatalf("proxy %q reported unusable but the account is still schedulable", unusable)
		}
	}

	// With the proxy pool disabled there is no fail-closed, so nothing should be reported.
	store.proxyPoolEnabled = false
	if got := store.UnusableManagedProxies([]string{disabledURL}); len(got) != 0 {
		t.Fatalf("UnusableManagedProxies while pool is off = %v, want empty", got)
	}

	var nilStore *Store
	if got := nilStore.UnusableManagedProxies([]string{disabledURL}); got != nil {
		t.Fatalf("nil store returned %v, want nil", got)
	}
}
