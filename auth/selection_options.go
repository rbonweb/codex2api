package auth

import (
	"context"
	"time"
)

// SelectionOptions describes the scheduling context of one account selection.
//
// Policy is the usage gate (Standard or Spark).
type SelectionOptions struct {
	Policy DispatchPolicy
}

// NextForSessionWithDispatchGuardForProvider is the session selection entry that carries SelectionOptions.
func (s *Store) NextForSessionWithDispatchGuardForProvider(opts SelectionOptions, key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string, SessionAffinityGuard) {
	return s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, false, opts)
}

// NextForContinuationWithDispatchForProvider is the SelectionOptions variant for continuation requests, which only reuse the bound account.
func (s *Store) NextForContinuationWithDispatchForProvider(opts SelectionOptions, key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, true, opts)
	return account, proxyURL
}

// WaitForDispatchAvailableForProvider is the SelectionOptions variant of WaitForDispatchAvailable for the queued waiting path.
func (s *Store) WaitForDispatchAvailableForProvider(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, opts SelectionOptions, preserveBinding bool, heartbeat ...SchedulerWaitHeartbeat) (*Account, string, SessionAffinityGuard, error) {
	return s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, preserveBinding, opts, heartbeat...)
}
