// Package proxy: concurrency limit for "this key × a group/account" (issue #439 v2).
//
// Unlike the key-level max_concurrency, which is decided at request entry (without needing to know which account will be used),
// this one only knows which scope is hit after an account is selected, so it is split into two steps:
//
//  1. Selection phase: read the current in-flight count once in the account filter chain; if full, remove that scope's accounts so the request naturally falls through to
//     other groups; when no candidates remain it returns 429 and states that concurrency slots are full. Concurrency is a transient state, so the budget's
//     on_exhausted=reject (that would make occasional contention fail outright), and is always handled as "switch account";
//  2. After selection: take one slot for each matched scope, and release them when the request ends (or on an account-switch retry).
//
// It is deliberately a **soft limit**: there is a race between the filter check and taking the slot, so under high concurrency it may briefly exceed by one or two. A hard limit
// would require re-selecting an account in the scheduling loop after a failed slot take, which means changing the retry structure of 6 forwarding paths; the gain does not justify the risk.
// Each slot carries a deadline, so even if a path forgets to release, it self-heals after scopeLeaseMaxAge and never permanently blocks a scope.
package proxy

import (
	"fmt"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// scopeLeaseMaxAge is the maximum lifetime of one slot. Normal paths release it explicitly; this is the fallback for missed releases,
// set far above any single request (including long streams and WS rounds).
const scopeLeaseMaxAge = 15 * time.Minute

type scopeConcurrencyLease struct {
	id       uint64
	key      scopeSkipKey
	expireAt time.Time
}

type scopeConcurrencyTracker struct {
	mu     sync.Mutex
	nextID uint64
	// inflight[scope] = leaseID -> deadline
	inflight map[scopeSkipKey]map[uint64]time.Time
}

// apiKeyScopeConcurrency is a process-level singleton: /v1 traffic may be served by multiple Handler instances,
// concurrency counts must be shared (same reason as apiKeyScopeSkipStats).
var apiKeyScopeConcurrency = &scopeConcurrencyTracker{
	inflight: make(map[scopeSkipKey]map[uint64]time.Time),
}

// count returns the current in-flight count of a scope and also clears expired slots.
func (t *scopeConcurrencyTracker) count(key scopeSkipKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.countLocked(key, time.Now())
}

func (t *scopeConcurrencyTracker) countLocked(key scopeSkipKey, now time.Time) int {
	leases := t.inflight[key]
	for id, expireAt := range leases {
		if now.After(expireAt) {
			delete(leases, id)
		}
	}
	if len(leases) == 0 {
		delete(t.inflight, key)
		return 0
	}
	return len(leases)
}

func (t *scopeConcurrencyTracker) acquire(key scopeSkipKey) scopeConcurrencyLease {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.countLocked(key, now)
	t.nextID++
	lease := scopeConcurrencyLease{id: t.nextID, key: key, expireAt: now.Add(scopeLeaseMaxAge)}
	if t.inflight[key] == nil {
		t.inflight[key] = make(map[uint64]time.Time)
	}
	t.inflight[key][lease.id] = lease.expireAt
	return lease
}

func (t *scopeConcurrencyTracker) release(lease scopeConcurrencyLease) {
	if lease.id == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	leases := t.inflight[lease.key]
	if leases == nil {
		return
	}
	delete(leases, lease.id)
	if len(leases) == 0 {
		delete(t.inflight, lease.key)
	}
}

// APIKeyScopeInflight returns the current in-flight request count of a scope, for display in the admin UI.
func APIKeyScopeInflight(apiKeyID int64, scopeType string, scopeID int64) int {
	if apiKeyID <= 0 || scopeID <= 0 {
		return 0
	}
	return apiKeyScopeConcurrency.count(scopeSkipKey{apiKeyID: apiKeyID, scopeType: scopeType, scopeID: scopeID})
}

// scopeConcurrencyFull reports whether a scope's concurrency slots are full.
func scopeConcurrencyFull(apiKeyID int64, scope database.APIKeyScopeLimit) bool {
	if scope.MaxConcurrency <= 0 {
		return false
	}
	key := scopeSkipKey{apiKeyID: apiKeyID, scopeType: scope.ResolveScopeType(), scopeID: scope.ScopeID}
	return apiKeyScopeConcurrency.count(key) >= scope.MaxConcurrency
}

// scopeMatchesAccount reports whether an account falls within a scope.
func scopeMatchesAccount(scope database.APIKeyScopeLimit, account *auth.Account) bool {
	if account == nil {
		return false
	}
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return account.ID() == scope.ScopeID
	}
	return account.InAnyGroup(map[int64]struct{}{scope.ScopeID: {}})
}

// AcquireAPIKeyScopeConcurrency takes slots for the selected account. On an account-switch retry of the same request, the previous round's slots are released first,
// because one request only ever occupies one account at a time.
func (h *Handler) AcquireAPIKeyScopeConcurrency(c *gin.Context, account *auth.Account) {
	gate := scopeBudgetGateFromContext(c)
	if gate == nil || account == nil || len(gate.concurrencyScopes) == 0 {
		return
	}
	gate.releaseLeases()
	leases := make([]scopeConcurrencyLease, 0, len(gate.concurrencyScopes))
	for _, scope := range gate.concurrencyScopes {
		if !scopeMatchesAccount(scope, account) {
			continue
		}
		leases = append(leases, apiKeyScopeConcurrency.acquire(scopeSkipKey{
			apiKeyID:  gate.apiKeyID,
			scopeType: scope.ResolveScopeType(),
			scopeID:   scope.ScopeID,
		}))
	}
	gate.setLeases(leases)
}

// ReleaseAPIKeyScopeConcurrency releases all concurrency slots this request holds. The forwarding handler defers it at entry.
func (h *Handler) ReleaseAPIKeyScopeConcurrency(c *gin.Context) {
	if gate := scopeBudgetGateFromContext(c); gate != nil {
		gate.releaseLeases()
	}
}

// scopeConcurrencyMessage is the note given downstream when concurrency slots are full.
func scopeConcurrencyMessage(scopeLabel string, limit int) string {
	return fmt.Sprintf("API key scope concurrency limit reached: %s allows %d concurrent request(s)", scopeLabel, limit)
}
