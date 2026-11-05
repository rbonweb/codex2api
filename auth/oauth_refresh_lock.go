package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

const (
	oauthRefreshLeaseNamespace = "oauth-refresh"
	oauthRefreshLeaseTTL       = 5 * time.Minute
	oauthRefreshLeaseHold      = 4 * time.Minute
	oauthRefreshLeaseWait      = 5*time.Minute + 5*time.Second
	oauthRefreshLeasePoll      = 100 * time.Millisecond
)

var oauthRefreshLeaseOwnerSequence atomic.Uint64

// WithOAuthRefreshLease serializes credential imports with background refreshes.
// The callback must check persisted credentials again before consuming the token
// and persist the rotated credentials before returning.
func (s *Store) WithOAuthRefreshLease(ctx context.Context, refreshToken string, fn func(context.Context) error) error {
	lease, err := s.acquireOAuthRefreshLease(ctx, refreshToken)
	if err != nil {
		return err
	}
	defer lease.Release()
	return fn(lease.CriticalContext())
}

type oauthRefreshLocalLock struct {
	ch   chan struct{}
	refs int
}

type oauthRefreshLease struct {
	store       *Store
	fingerprint string
	local       *oauthRefreshLocalLock
	owner       string
	distributed bool
	ctx         context.Context
	cancel      context.CancelFunc
	// criticalCtx shares the lease hold deadline with ctx, but is not cancelled or timed out with the caller:
	// once the RT consumption critical section (exchange + CAS persist) starts, if the caller (for example an admin 10s request) is cancelled midway
	// cancellation would permanently lose the new RT that upstream already rotated, and the account could only be re-imported by hand.
	criticalCtx    context.Context
	criticalCancel context.CancelFunc
	released       atomic.Bool
}

// arm creates both the wait ctx (cancelled with the caller) and the critical-section ctx (bounded only by the hold deadline).
func (lease *oauthRefreshLease) arm(parent context.Context) {
	lease.ctx, lease.cancel = context.WithTimeout(parent, oauthRefreshLeaseHold)
	lease.criticalCtx, lease.criticalCancel = context.WithTimeout(context.WithoutCancel(parent), oauthRefreshLeaseHold)
}

func oauthRefreshTokenFingerprint(refreshToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(refreshToken)))
	return hex.EncodeToString(sum[:])
}

func (s *Store) acquireOAuthRefreshLease(ctx context.Context, refreshToken string) (*oauthRefreshLease, error) {
	if s == nil {
		return nil, fmt.Errorf("account storage is not configured")
	}
	fingerprint := oauthRefreshTokenFingerprint(refreshToken)
	if fingerprint == oauthRefreshTokenFingerprint("") {
		return nil, fmt.Errorf("refresh_token is empty")
	}

	local, err := s.acquireOAuthRefreshLocalLock(ctx, fingerprint)
	if err != nil {
		return nil, err
	}
	lease := &oauthRefreshLease{
		store:       s,
		fingerprint: fingerprint,
		local:       local,
		owner:       fmt.Sprintf("%p-%d-%d", s, time.Now().UnixNano(), oauthRefreshLeaseOwnerSequence.Add(1)),
	}
	if s.tokenCache == nil {
		lease.arm(ctx)
		return lease, nil
	}

	deadline := time.Now().Add(oauthRefreshLeaseWait)
	for {
		acquired, err := s.tokenCache.AcquireLease(
			ctx,
			oauthRefreshLeaseNamespace,
			fingerprint,
			lease.owner,
			oauthRefreshLeaseTTL,
		)
		if err != nil {
			// A shared-cache failure cannot safely degrade to the process-local
			// lock: two instances could both consume a rotating refresh token. CAS
			// would reject the second database write, but cannot undo the provider's
			// already-completed rotation. Fail closed whenever this cache represents
			// cross-instance coordination; in-memory caches remain intentionally
			// single-process and their lease errors are not expected in normal use.
			if s.tokenCache.SharedAcrossInstances() {
				lease.Release()
				return nil, fmt.Errorf("failed to acquire OAuth cross-instance refresh lease: %w", err)
			}
			log.Printf("failed to acquire OAuth local refresh lease, keeping the in-process lock: %v", err)
			lease.arm(ctx)
			return lease, nil
		}
		if acquired {
			lease.distributed = true
			lease.arm(ctx)
			return lease, nil
		}
		if time.Now().After(deadline) {
			lease.Release()
			return nil, fmt.Errorf("timed out waiting for the refresh task of the same OAuth credential")
		}

		timer := time.NewTimer(oauthRefreshLeasePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			lease.Release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) acquireOAuthRefreshLocalLock(ctx context.Context, fingerprint string) (*oauthRefreshLocalLock, error) {
	s.oauthRefreshLocksMu.Lock()
	local := s.oauthRefreshLocks[fingerprint]
	if local == nil {
		local = &oauthRefreshLocalLock{ch: make(chan struct{}, 1)}
		s.oauthRefreshLocks[fingerprint] = local
	}
	local.refs++
	s.oauthRefreshLocksMu.Unlock()

	select {
	case local.ch <- struct{}{}:
		return local, nil
	case <-ctx.Done():
		s.releaseOAuthRefreshLocalLockRef(fingerprint, local)
		return nil, ctx.Err()
	}
}

func (s *Store) releaseOAuthRefreshLocalLockRef(fingerprint string, local *oauthRefreshLocalLock) {
	s.oauthRefreshLocksMu.Lock()
	local.refs--
	if local.refs == 0 && s.oauthRefreshLocks[fingerprint] == local {
		delete(s.oauthRefreshLocks, fingerprint)
	}
	s.oauthRefreshLocksMu.Unlock()
}

func (lease *oauthRefreshLease) Context() context.Context {
	if lease == nil || lease.ctx == nil {
		return context.Background()
	}
	return lease.ctx
}

// CriticalContext returns the ctx dedicated to the RT consumption critical section: it is not cancelled with the caller, and is bounded only by the lease hold
// deadline and Release. After holding the family lease, the exchange/persist step must switch to this ctx.
func (lease *oauthRefreshLease) CriticalContext() context.Context {
	if lease == nil || lease.criticalCtx == nil {
		return context.Background()
	}
	return lease.criticalCtx
}

func (lease *oauthRefreshLease) Release() {
	if lease == nil || lease.store == nil || lease.local == nil || lease.released.Swap(true) {
		return
	}
	if lease.distributed && lease.store.tokenCache != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := lease.store.tokenCache.ReleaseLease(
			releaseCtx,
			oauthRefreshLeaseNamespace,
			lease.fingerprint,
			lease.owner,
		); err != nil {
			log.Printf("failed to release OAuth cross-instance refresh lease: %v", err)
		}
		cancel()
	}
	if lease.cancel != nil {
		lease.cancel()
	}
	if lease.criticalCancel != nil {
		lease.criticalCancel()
	}

	<-lease.local.ch
	lease.store.releaseOAuthRefreshLocalLockRef(lease.fingerprint, lease.local)
}

func (s *Store) reloadOAuthCredentialsAfterLock(
	ctx context.Context,
	acc *Account,
	lockedRefreshToken string,
	lockedAccessToken string,
) (changed bool, usable bool, err error) {
	if s == nil || s.db == nil || acc == nil || acc.DBID <= 0 {
		return false, false, nil
	}
	row, err := s.db.GetAccountByID(ctx, acc.DBID)
	if err != nil {
		return false, false, err
	}
	refreshToken := strings.TrimSpace(row.GetCredential("refresh_token"))
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	sessionToken := strings.TrimSpace(row.GetCredential("session_token"))
	acc.mu.RLock()
	sessionChanged := sessionToken != strings.TrimSpace(acc.SessionToken)
	acc.mu.RUnlock()
	refreshChanged := refreshToken != "" && refreshToken != strings.TrimSpace(lockedRefreshToken)
	accessChanged := accessToken != "" && accessToken != strings.TrimSpace(lockedAccessToken)
	if !refreshChanged && !accessChanged && !sessionChanged {
		acc.mu.Lock()
		if acc.CredentialGeneration > row.CredentialGeneration {
			acc.mu.Unlock()
			return false, false, fmt.Errorf("credential version changed during reload")
		}
		acc.CredentialGeneration = row.CredentialGeneration
		acc.mu.Unlock()
		return false, false, nil
	}

	expiresAt := parseOAuthCredentialExpiry(row.GetCredential("expires_at"))

	acc.mu.Lock()
	if acc.CredentialGeneration > row.CredentialGeneration {
		acc.mu.Unlock()
		return false, false, fmt.Errorf("credential version changed during reload")
	}
	acc.CredentialGeneration = row.CredentialGeneration
	if refreshToken != "" {
		acc.RefreshToken = refreshToken
	}
	if accessToken != "" {
		acc.AccessToken = accessToken
	}
	if sessionToken != "" || sessionChanged {
		acc.SessionToken = sessionToken
	}
	if !expiresAt.IsZero() {
		acc.ExpiresAt = expiresAt
	}
	if accountID := strings.TrimSpace(row.GetCredential("account_id")); accountID != "" {
		acc.AccountID = accountID
	}
	if email := strings.TrimSpace(row.GetCredential("email")); email != "" {
		acc.Email = email
	}
	if planType := strings.TrimSpace(row.GetCredential("plan_type")); planType != "" {
		acc.PlanType = planType
	}
	effectiveAccessToken := acc.AccessToken
	effectiveExpiresAt := acc.ExpiresAt
	acc.mu.Unlock()

	usable = effectiveAccessToken != "" &&
		(effectiveExpiresAt.IsZero() || time.Until(effectiveExpiresAt) > 5*time.Minute)
	return true, usable, nil
}

func parseOAuthCredentialExpiry(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// finishReloadedOAuthRefresh publishes the account state after reusing a refresh result that was completed elsewhere.
// The cooldown decision is based on the current in-memory state, not on the caller's entry snapshot: waiting for the lease can take minutes, and during that time other
// goroutines may already have put the account into or extended cooldown (for example upstream 429); that newer fact must not be
// reset to zero or rolled back by a stale snapshot.
func (s *Store) finishReloadedOAuthRefresh(ctx context.Context, acc *Account) {
	acc.mu.Lock()
	now := time.Now()
	activeCooldown := acc.Status == StatusCooldown && now.Before(acc.CooldownUtil)
	expiredCooldown := acc.Status == StatusCooldown && !now.Before(acc.CooldownUtil)
	if !activeCooldown {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	acc.ErrorMsg = ""
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	accessToken := acc.AccessToken
	expiresAt := acc.ExpiresAt
	dbID := acc.DBID
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	if s.tokenCache != nil && accessToken != "" {
		ttl := time.Until(expiresAt) - 5*time.Minute
		if expiresAt.IsZero() {
			ttl = 30 * time.Minute
		}
		if ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, dbID, accessToken, ttl)
		}
	}
	if expiredCooldown {
		s.persistAccountCooldownState(acc, true, false)
	} else if !activeCooldown && s.db != nil {
		s.persistAccountCooldownState(acc, true, true)
	}
}
