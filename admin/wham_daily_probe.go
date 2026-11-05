package admin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

var (
	errWhamDailyUsageUnavailable  = errors.New("official usage statistics are unavailable")
	errWhamDailyUsageUnsupported  = errors.New("codex_at accounts do not support official usage statistics")
	errWhamDailyUsageUnauthorized = errors.New("upstream authentication failed (401), please refresh the account and try again")
	errWhamDailyUsageRateLimited  = errors.New("upstream rate limited (429), please try again later")
)

func upstreamDailyUsageError(status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if snippet == "" {
		return fmt.Errorf("upstream returned %d", status)
	}
	return fmt.Errorf("upstream returned %d: %s", status, snippet)
}

const (
	// whamDailyUsageProbeTick is the scheduling check interval. Each tick only picks accounts that are "due" to refresh,
	// not all accounts every tick: the real interval is set by the two per-account-state values below.
	whamDailyUsageProbeTick = 30 * time.Minute

	// whamDailyUsageProbeBaseInterval is the refresh interval for normal accounts.
	whamDailyUsageProbeBaseInterval = time.Hour

	// whamDailyUsageProbeRateLimitedInterval is the refresh interval for rate-limited or quota-exhausted accounts.
	// Such accounts will not see new quota consumption in the short term, so there is no need to hit upstream every hour.
	whamDailyUsageProbeRateLimitedInterval = 6 * time.Hour

	// whamDailyUsageProbeConcurrency limits concurrency, so a large account pool does not hit upstream all at once.
	whamDailyUsageProbeConcurrency = 4

	// whamDailyUsageBackfillCooldown is the minimum interval for instant backfill from the list page. page-stats
	// keeps arriving with page turns and frontend retries, so the same account is backfilled only once within the cooldown.
	whamDailyUsageBackfillCooldown = 2 * time.Minute

	// whamDailyUsageBackfillFailureCooldown is the cooldown after a backfill failure. When upstream cannot be fetched
	// (401 / network / lag), retrying within a short time will not help, and hitting it every 2 minutes with the normal cooldown would only
	// waste upstream quota; failed accounts are left to the hourly probe as a fallback.
	whamDailyUsageBackfillFailureCooldown = 15 * time.Minute

	// whamDailyUsageKeepDays is the retention period for local snapshots. Upstream can be queried back about 84 days; keeping one year locally is enough to see trends.
	whamDailyUsageKeepDays = 365

	// whamDailyUsageMinAccountAge is the minimum account age for automatic refresh of official settlement.
	// Official statistics usually appear the day after an account makes requests, so pulling upstream on the day of import would just spin idle.
	whamDailyUsageMinAccountAge = 24 * time.Hour
)

var whamDailyUsageProbeOnce sync.Once

// StartWhamDailyUsageProbe periodically stores official settlement usage (counts + model × speed breakdown).
//
// Upstream daily-workspace-usage-counts has limited lookback depth (measured at about 84 days); older history exists only in
// the local database, so this task is the only source of long-term history. In steady state each round backfills a rolling 7 days; the first sync does a
// deep backfill of 84 days, so missed rounds leave no gaps.
func (h *Handler) StartWhamDailyUsageProbe(ctx context.Context) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	whamDailyUsageProbeOnce.Do(func() {
		go func() {
			// Wait a moment after startup to avoid the account loading and refresh peak right after the process starts.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
			}
			// lastAttempt is only read and written in this loop goroutine, so no lock is needed.
			// It records the attempt time, not the success time: a failure also waits a full interval before retrying,
			// so a bad account does not hit upstream on every tick.
			lastAttempt := map[int64]time.Time{}
			h.runWhamDailyUsageProbe(ctx, lastAttempt)
			ticker := time.NewTicker(whamDailyUsageProbeTick)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					h.runWhamDailyUsageProbe(ctx, lastAttempt)
				}
			}
		}()
	})
}

// whamDailyUsageProbeIntervalFor gives the refresh interval based on the account's current state: rate-limited / quota-exhausted
// accounts have no quota change in the short term, so they drop to every 6 hours; other accounts refresh hourly.
func whamDailyUsageProbeIntervalFor(account *auth.Account) time.Duration {
	switch account.RuntimeStatus() {
	case "rate_limited", auth.ResponsesRateLimitedCooldownReason, "rate_limited_7d", "usage_limited", "usage_limit", "usage_exhausted":
		return whamDailyUsageProbeRateLimitedInterval
	}
	return whamDailyUsageProbeBaseInterval
}

// whamDailyUsageDueTargets picks out the candidate accounts that are due for refresh, and maintains lastAttempt in place
// (recording this round's attempts and clearing accounts that no longer exist).
func whamDailyUsageDueTargets(all []*auth.Account, lastAttempt map[int64]time.Time, now time.Time) []*auth.Account {
	targets := make([]*auth.Account, 0, len(all))
	current := make(map[int64]struct{}, len(all))
	for _, account := range all {
		current[account.DBID] = struct{}{}
		// Subtract half a tick of margin: otherwise a 1h interval lands exactly on the 1h tick boundary,
		// and timing error would make the account miss being due every time by a hair, so it actually refreshes only every two hours.
		due := whamDailyUsageProbeIntervalFor(account) - whamDailyUsageProbeTick/2
		if last, ok := lastAttempt[account.DBID]; ok && now.Sub(last) < due {
			continue
		}
		lastAttempt[account.DBID] = now
		targets = append(targets, account)
	}
	// Remove deleted accounts from the timing table, to prevent a slow leak over long runs.
	for id := range lastAttempt {
		if _, ok := current[id]; !ok {
			delete(lastAttempt, id)
		}
	}
	return targets
}

func (h *Handler) runWhamDailyUsageProbe(ctx context.Context, lastAttempt map[int64]time.Time) {
	// First fill in the PATs that lack a workspace, so accounts that get filled in can enter the candidates this round. (issue #662)
	h.hydratePATWorkspaces(ctx)
	all := h.whamDailyUsageProbeTargets()
	if len(all) == 0 {
		return
	}
	targets := whamDailyUsageDueTargets(all, lastAttempt, time.Now())
	if len(targets) == 0 {
		return
	}
	started := time.Now()
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		okCount   int
		failCount int
	)
	sem := make(chan struct{}, whamDailyUsageProbeConcurrency)
	for _, account := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			outcome, err := h.syncWhamDailyUsage(reqCtx, acc)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failCount++
				// A 401 does not decide the account state here: an authentication failure on the wham side does not mean the account is unusable
				// (consistent with the existing meaning in the usage probe); it only logs and waits for the next round.
				log.Printf("official usage snapshot failed account=%d: %v", acc.DBID, err)
				return
			}
			okCount++
			// A breakdown endpoint failure does not count as a whole failure: counts are already stored, and the shares are filled in next round.
			if outcome.BreakdownErr != nil {
				log.Printf("official model breakdown snapshot failed account=%d: %v", acc.DBID, outcome.BreakdownErr)
			}
		}(account)
	}
	wg.Wait()

	if okCount > 0 {
		pruneCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := h.db.PruneAccountDailyUsage(pruneCtx, whamDailyUsageKeepDays); err != nil {
			log.Printf("failed to clean up official usage snapshots: %v", err)
		}
		cancel()
	}
	log.Printf("official usage snapshots done ok=%d failed=%d took=%s", okCount, failCount, time.Since(started).Round(time.Millisecond))
}

// enqueueWhamDailyUsageBackfill triggers one upstream pull for accounts on the current page that lack an official settlement snapshot.
// The background hourly probe covers the whole pool, but when the list opens the snapshot is often still empty: page-stats only reads the local table,
// so without an instant backfill here, the "Official billed" pill only appears after a manual refresh or the next probe.
func (h *Handler) enqueueWhamDailyUsageBackfill(ids []int64) {
	if h == nil || h.store == nil || len(ids) == 0 {
		return
	}
	now := time.Now()
	targets := make([]*auth.Account, 0, len(ids))
	h.whamDailyBackfillMu.Lock()
	if h.whamDailyBackfillLast == nil {
		h.whamDailyBackfillLast = map[int64]time.Time{}
	}
	if h.whamDailyBackfillInFlight == nil {
		h.whamDailyBackfillInFlight = map[int64]struct{}{}
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, flying := h.whamDailyBackfillInFlight[id]; flying {
			continue
		}
		if last, ok := h.whamDailyBackfillLast[id]; ok && now.Sub(last) < whamDailyUsageBackfillCooldown {
			continue
		}
		if failedAt, ok := h.whamDailyBackfillFailedAt[id]; ok && now.Sub(failedAt) < whamDailyUsageBackfillFailureCooldown {
			continue
		}
		account := h.store.FindByID(id)
		if !whamDailyUsageAutoRefreshEligible(account, now) {
			continue
		}
		h.whamDailyBackfillInFlight[id] = struct{}{}
		h.whamDailyBackfillLast[id] = now
		targets = append(targets, account)
	}
	h.whamDailyBackfillMu.Unlock()
	if len(targets) == 0 {
		return
	}
	go h.runWhamDailyUsageBackfill(targets)
}

func whamDailyUsageBackfillEligible(account *auth.Account) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	if !whamDailyUsageChannelSupported(account) {
		return false
	}
	if isCodexATAccount(account) {
		return false
	}
	return account.GetAccessToken() != ""
}

// whamDailyUsageChannelSupported checks whether the account's channel has a ChatGPT WHAM endpoint.
// WHAM belongs only to the ChatGPT control plane, and all accounts are now Codex accounts, so it only requires that the account exists.
// Probe candidates and manual refreshes both pass through this gate.
func whamDailyUsageChannelSupported(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return true
}

// isCodexATAccount detects plain AT credentials in the at-... form. Plain AT credentials without a supplied workspace ID can call
// Codex Responses, but lack the workspace authentication that ChatGPT WHAM analytics needs to be stable,
// so "Responses works" must not be treated as "official settlement statistics work". (issue #564)
// If a valid workspace ID has been obtained through whoami or a custom header, official statistics sync is allowed. (issue #662)
func isCodexATAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	if accessTokenTypeForToken(account.GetAccessToken()) != accessTokenTypeCodexAT {
		return false
	}
	return strings.TrimSpace(account.EffectiveAccountID()) == ""
}

func whamDailyUsageAutoRefreshEligible(account *auth.Account, now time.Time) bool {
	if !whamDailyUsageBackfillEligible(account) {
		return false
	}
	switch account.RuntimeStatus() {
	case "unauthorized", "error":
		return false
	}
	addedAt := time.Unix(0, account.AddedAt.Load())
	if addedAt.Unix() > 0 && now.Sub(addedAt) < whamDailyUsageMinAccountAge {
		return false
	}
	return true
}

func (h *Handler) finishWhamDailyUsageBackfill(id int64) {
	if h == nil {
		return
	}
	h.whamDailyBackfillMu.Lock()
	delete(h.whamDailyBackfillInFlight, id)
	h.whamDailyBackfillMu.Unlock()
}

// markWhamDailySynced records that this account has successfully synced official usage at least once, even when upstream returns
// empty data (official statistics lag, or the account really has no official client usage). page-stats uses this to
// send an explicit empty state, so the frontend stops spinning and re-fetching and no longer triggers a backfill. Process memory is enough: after a restart
// at most one extra backfill happens before it is marked again.
func (h *Handler) markWhamDailySynced(id int64) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	if h.whamDailySyncedOnce == nil {
		h.whamDailySyncedOnce = map[int64]struct{}{}
	}
	h.whamDailySyncedOnce[id] = struct{}{}
	delete(h.whamDailyBackfillFailedAt, id)
	h.whamDailyBackfillMu.Unlock()
}

func (h *Handler) whamDailySyncedOnceFor(id int64) bool {
	if h == nil {
		return false
	}
	h.whamDailyBackfillMu.Lock()
	_, ok := h.whamDailySyncedOnce[id]
	h.whamDailyBackfillMu.Unlock()
	return ok
}

func (h *Handler) markWhamDailyBackfillFailed(id int64) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	if h.whamDailyBackfillFailedAt == nil {
		h.whamDailyBackfillFailedAt = map[int64]time.Time{}
	}
	h.whamDailyBackfillFailedAt[id] = time.Now()
	h.whamDailyBackfillMu.Unlock()
}

func (h *Handler) runWhamDailyUsageBackfill(accounts []*auth.Account) {
	sem := make(chan struct{}, whamDailyUsageProbeConcurrency)
	var wg sync.WaitGroup
	for _, account := range accounts {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			defer h.finishWhamDailyUsageBackfill(acc.DBID)
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			outcome, err := h.syncWhamDailyUsage(ctx, acc)
			if err != nil {
				h.markWhamDailyBackfillFailed(acc.DBID)
				log.Printf("official usage instant backfill failed account=%d: %v", acc.DBID, err)
				return
			}
			if outcome.BreakdownErr != nil {
				log.Printf("official model breakdown instant backfill failed account=%d: %v", acc.DBID, outcome.BreakdownErr)
			}
		}(account)
	}
	wg.Wait()
}

// whamDailyUsageProbeTargets picks the accounts whose statistics should be pulled: only OAuth-signed-in Codex accounts
// have permission for the wham analytics endpoint; codex_at accounts do not.
func (h *Handler) whamDailyUsageProbeTargets() []*auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	accounts := h.store.Accounts()
	out := make([]*auth.Account, 0, len(accounts))
	now := time.Now()
	for _, account := range accounts {
		if !whamDailyUsageAutoRefreshEligible(account, now) {
			continue
		}
		out = append(out, account)
	}
	return out
}
