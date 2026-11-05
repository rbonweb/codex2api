package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const resetCreditLeaseTTL = 45 * time.Second

const resetCreditCooldownNamespace = "reset-credit-cooldown"

const autoResetCreditHandledIDRetention = 8 * 24 * time.Hour

// GetResetCredits queries the breakdown of the account's "manual reset credits" (issue and expiry time of each credit).
// GET /api/accounts/:id/reset-credits
//
// It calls the official wham/rate-limit-reset-credits list endpoint (zero quota cost), filtering by
// reset_type=codex_rate_limits + status=available,
// returns the number of available credits and each credit's validity period, so the frontend can show "which credit expires when" (issue #322).
// It also uses the authoritative available_count to calibrate the locally cached remaining count.
func (h *Handler) GetResetCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "The account has no usable access token; refresh the account first")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	list, resp, err := h.queryResetCreditsUpstream(ctx, account, h.store.ResolveProxyForAccount(account))
	if err != nil {
		status := upstreamResetStatus(resp)
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			_ = resp.Body.Close()
			switch status {
			case http.StatusUnauthorized:
				writeError(c, http.StatusBadGateway, "Upstream authentication failed (401); refresh the account first and try again")
			case http.StatusTooManyRequests:
				writeError(c, http.StatusBadGateway, "Upstream rate limit (429); try again later")
			default:
				writeError(c, http.StatusBadGateway, "Failed to query reset count: "+upstreamResetErrorMessage(status, body))
			}
			return
		}
		writeError(c, http.StatusBadGateway, "Failed to query reset count: "+err.Error())
		return
	}

	credits := list.AvailableCodexCredits()
	// Authoritative available count: prefer the upstream available_count; when it is missing (0 while credits exist), fall back to the filtered count.
	available := list.AvailableCount
	if available == 0 && len(credits) > 0 {
		available = len(credits)
	}
	// The upstream breakdown is authoritative; it also calibrates the local cache so the list and dialog are immediately consistent.
	account.SetRateLimitResetCredits(available)

	items := make([]gin.H, 0, len(credits))
	for _, credit := range credits {
		items = append(items, gin.H{
			"id":         credit.ID,
			"granted_at": credit.GrantedAt,
			"expires_at": credit.EffectiveConsumableUntil(),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"available_count": available,
		"credits":         items,
	})
}

// ResetCredits consumes one manual reset credit for the account to reset the Codex quota immediately.
// POST /api/accounts/:id/reset-credits
//
// Flow: find the account → call the official wham/rate-limit-reset-credits/consume → parse the response for instant feedback
// (windows_reset) + optimistically decrement the local remaining count → refresh usage asynchronously in the background. When the count is 0, reject immediately.
//
// Robustness / performance handling:
//   - An account-level mutex serializes concurrent resets of the same account, to avoid double consumption.
//   - consume reuses the same redeem_request_id as the idempotency key: the upstream deduplicates with it,
//     so retrying after a 401 refresh does not deduct one precious reset credit twice.
//   - On an upstream 401, first try to refresh the account token, then retry once with the same idempotency key.
//   - Use the consume response for direct feedback and optimistically decrement the local count, saving the sync wham round-trip after the reset;
//     the window snapshot / exact count / cooldown state is aligned later by the background probe.
//   - After success, write an account_events audit record (event_type=reset_credit).
func (h *Handler) ResetCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "Account not found")
		return
	}

	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "The account has no usable access token; refresh the account first")
		return
	}

	// Workspace-level mutex: resets in the same upstream workspace run serially, to avoid concurrent double consumption and count races.
	lock := h.resetCreditLock(account)
	lock.Lock()
	defer lock.Unlock()

	// If the locally known count is 0, reject directly (consistent with the frontend button gate), to reduce useless upstream calls.
	if count, ok := account.GetRateLimitResetCredits(); ok && count <= 0 {
		writeError(c, http.StatusConflict, "The account has no manual reset credits available")
		return
	}

	// Consuming a reset credit cannot be undone: once it is sent, the result must be read to the end. If it hangs off the client request context,
	// then when the user closes the page or it times out, the credit already deducted is reported as a failure and cut short, and a retry generates another
	// idempotency key (the uuid below), so the same credit is deducted twice. WithoutCancel keeps the request-scoped values
	// (logs, traces) but drops the cancellation signal, so only its own timeout applies.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
	defer cancel()

	// The whole reset operation (including the retry after a 401 refresh) reuses the same idempotency key,
	// so the upstream deduplicates the repeated requests and does not deduct one extra reset credit.
	redeemRequestID := uuid.New().String()
	outcome, failure := h.consumeResetCreditLocked(ctx, account, redeemRequestID, "manual")
	if failure != nil {
		h.writeManualResetCreditFailure(c, account, failure)
		return
	}
	if outcome.InProgress {
		writeError(c, http.StatusConflict, "A quota reset is already running for this workspace; try again later")
		return
	}
	log.Printf("[account %d] manual quota reset succeeded, windows_reset=%d, remaining count=%d", account.DBID, outcome.WindowsReset, outcome.Remaining)

	// Wait for the usage probe to land before responding: the reset has already changed the upstream windows, but the local snapshot and cooldown state only update when this
	// probe runs. Returning first would make the frontend load old data, which looks like "clicked reset, but the status and progress bar did not change".
	refreshed := waitUsageRefreshAfterReset(outcome.UsageRefreshed)

	payload := gin.H{"message": "Quota reset", "usage_refreshed": refreshed}
	if outcome.Remaining >= 0 {
		payload["rate_limit_reset_credits"] = outcome.Remaining
	}
	if outcome.WindowsReset > 0 {
		payload["windows_reset"] = outcome.WindowsReset
	}
	// If the probe did not finish within the timeout, return the latest state too, as a fallback hint for the frontend.
	payload["status"] = account.RuntimeStatus()
	c.JSON(http.StatusOK, payload)
}

type resetCreditConsumeOutcome struct {
	WindowsReset   int
	Remaining      int
	AlreadyHandled bool
	InProgress     bool
	// UsageRefreshed is closed when the usage probe finishes after the reset. The manual path waits for it,
	// so that when the response returns, the account's usage snapshot and state are already new.
	UsageRefreshed <-chan struct{}
}

type resetCreditConsumeFailure struct {
	Status     int
	Body       []byte
	RequestErr error
	RefreshErr error
}

// consumeResetCreditLocked runs one shared reset-consumption flow. The caller must hold the workspace-level
// resetCreditLock, so that manual and automatic paths cannot consume concurrently within the same process.
func (h *Handler) consumeResetCreditLocked(ctx context.Context, account *auth.Account, redeemRequestID, source string) (resetCreditConsumeOutcome, *resetCreditConsumeFailure) {
	if source == "auto" && strings.TrimSpace(redeemRequestID) != "" {
		if h.autoResetCreditRequestHandled(redeemRequestID, time.Now()) {
			remaining := -1
			if count, ok := account.GetRateLimitResetCredits(); ok {
				remaining = count
			}
			return resetCreditConsumeOutcome{Remaining: remaining, AlreadyHandled: true}, nil
		}
	}
	acquired, releaseLease, leaseErr := h.acquireResetCreditLease(ctx, account)
	if leaseErr != nil {
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{RequestErr: fmt.Errorf("acquire reset-credit lease: %w", leaseErr)}
	}
	if !acquired {
		return resetCreditConsumeOutcome{InProgress: true}, nil
	}
	defer releaseLease()
	if source == "auto" {
		coolingDown, cooldownErr := h.resetCreditCooldownActive(ctx, account)
		if cooldownErr != nil {
			return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{RequestErr: fmt.Errorf("recheck reset-credit cooldown: %w", cooldownErr)}
		}
		if coolingDown {
			return resetCreditConsumeOutcome{AlreadyHandled: true}, nil
		}
	}
	proxyURL := h.store.ResolveProxyForAccount(account)
	result, resp, err := h.consumeResetCreditUpstream(ctx, account, proxyURL, redeemRequestID)
	status := upstreamResetStatus(resp)

	// Upstream authentication failed: refresh the token once, then retry with the same idempotency key.
	if status == http.StatusUnauthorized {
		drainResetResponse(resp)
		if refreshErr := h.refreshAccountForReset(ctx, account.DBID); refreshErr != nil {
			return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, RefreshErr: refreshErr}
		}
		proxyURL = h.store.ResolveProxyForAccount(account)
		result, resp, err = h.consumeResetCreditUpstream(ctx, account, proxyURL, redeemRequestID)
		status = upstreamResetStatus(resp)
	}

	if resp != nil && (status < 200 || status >= 300) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, Body: body, RequestErr: err}
	}
	if err != nil {
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, RequestErr: err}
	}

	outcome := resetCreditConsumeOutcome{Remaining: h.applyOptimisticResetDecrement(account)}
	if result != nil {
		outcome.WindowsReset = result.WindowsReset
	}
	if source == "auto" && strings.TrimSpace(redeemRequestID) != "" {
		h.rememberAutoResetCreditRequest(redeemRequestID, time.Now())
	}
	h.markResetCreditSuccess(account)
	h.recordResetCreditEvent(account.DBID, source)
	outcome.UsageRefreshed = h.refreshUsageAfterReset(account)
	return outcome, nil
}

func (h *Handler) autoResetCreditRequestHandled(redeemRequestID string, now time.Time) bool {
	if h == nil {
		return false
	}
	value, ok := h.resetCreditSuccessfulIDs.Load(redeemRequestID)
	if !ok {
		return false
	}
	expiresAt, ok := value.(time.Time)
	if !ok || !expiresAt.After(now) {
		h.resetCreditSuccessfulIDs.Delete(redeemRequestID)
		return false
	}
	return true
}

func (h *Handler) rememberAutoResetCreditRequest(redeemRequestID string, now time.Time) {
	if h == nil {
		return
	}
	h.resetCreditSuccessfulIDs.Range(func(key, value any) bool {
		expiresAt, ok := value.(time.Time)
		if !ok || !expiresAt.After(now) {
			h.resetCreditSuccessfulIDs.Delete(key)
		}
		return true
	})
	h.resetCreditSuccessfulIDs.Store(redeemRequestID, now.Add(autoResetCreditHandledIDRetention))
}

func (h *Handler) acquireResetCreditLease(ctx context.Context, account *auth.Account) (bool, func(), error) {
	if h == nil || h.cache == nil {
		return true, func() {}, nil
	}
	owner := uuid.New().String()
	key := resetCreditLockKey(account)
	acquired, err := h.cache.AcquireLease(ctx, "reset-credit", key, owner, resetCreditLeaseTTL)
	if err != nil || !acquired {
		return acquired, func() {}, err
	}
	return true, func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if releaseErr := h.cache.ReleaseLease(releaseCtx, "reset-credit", key, owner); releaseErr != nil {
			log.Printf("[account %d] failed to release the distributed reset lease: %v", account.DBID, releaseErr)
		}
	}, nil
}

func (h *Handler) resetCreditConsumedRecently(account *auth.Account, now time.Time, window time.Duration) bool {
	if h == nil || account == nil || window <= 0 {
		return false
	}
	value, ok := h.resetCreditLastSuccess.Load(resetCreditLockKey(account))
	if !ok {
		return false
	}
	consumedAt, ok := value.(time.Time)
	if !ok {
		return false
	}
	return consumedAt.After(now) || now.Sub(consumedAt) < window
}

func (h *Handler) resetCreditCooldownActive(ctx context.Context, account *auth.Account) (bool, error) {
	if h.resetCreditConsumedRecently(account, time.Now(), autoResetCreditsScanInterval) {
		return true, nil
	}
	if h == nil || h.cache == nil {
		return false, nil
	}
	_, found, err := h.cache.GetRuntime(ctx, resetCreditCooldownNamespace, resetCreditLockKey(account))
	return found, err
}

func (h *Handler) markResetCreditSuccess(account *auth.Account) {
	if h == nil || account == nil {
		return
	}
	key := resetCreditLockKey(account)
	h.resetCreditLastSuccess.Store(key, time.Now())
	if h.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.cache.SetRuntime(ctx, resetCreditCooldownNamespace, key, json.RawMessage(`true`), autoResetCreditsScanInterval); err != nil {
		log.Printf("[account %d] failed to write the reset cooldown marker: %v", account.DBID, err)
	}
}

func (h *Handler) writeManualResetCreditFailure(c *gin.Context, account *auth.Account, failure *resetCreditConsumeFailure) {
	if failure == nil {
		return
	}
	if failure.RefreshErr != nil {
		log.Printf("[account %d] token refresh failed during quota reset: %v", account.DBID, failure.RefreshErr)
		writeError(c, http.StatusBadGateway, "Upstream authentication failed (401); automatic account refresh failed, refresh manually and try again")
		return
	}
	switch failure.Status {
	case http.StatusUnauthorized:
		writeError(c, http.StatusBadGateway, "Upstream authentication failed (401); refresh the account first and try again")
	case http.StatusTooManyRequests:
		writeError(c, http.StatusBadGateway, "Upstream rate limit (429); try again later")
	case 0:
		if failure.RequestErr != nil {
			writeError(c, http.StatusBadGateway, "Reset request failed: "+failure.RequestErr.Error())
			return
		}
		writeError(c, http.StatusBadGateway, "Reset request failed")
	default:
		writeError(c, http.StatusBadGateway, "Reset failed: "+upstreamResetErrorMessage(failure.Status, failure.Body))
	}
}

func (h *Handler) queryResetCreditsUpstream(ctx context.Context, account *auth.Account, proxyURL string) (*proxy.WhamResetCreditsList, *http.Response, error) {
	if h != nil && h.queryResetCredits != nil {
		return h.queryResetCredits(ctx, account, proxyURL)
	}
	return proxy.QueryWhamResetCredits(ctx, account, proxyURL)
}

func (h *Handler) consumeResetCreditUpstream(ctx context.Context, account *auth.Account, proxyURL, redeemRequestID string) (*proxy.WhamResetResult, *http.Response, error) {
	if h != nil && h.consumeResetCredit != nil {
		return h.consumeResetCredit(ctx, account, proxyURL, redeemRequestID)
	}
	return proxy.ConsumeResetCreditParsed(ctx, account, proxyURL, redeemRequestID)
}

func (h *Handler) recordResetCreditEvent(accountID int64, source string) {
	if h == nil {
		return
	}
	if h.recordAccountEvent != nil {
		h.recordAccountEvent(accountID, "reset_credit", source)
		return
	}
	if h.db != nil {
		h.db.InsertAccountEventAsync(accountID, "reset_credit", source)
	}
}

// applyOptimisticResetDecrement optimistically decrements the remaining count by 1 locally after a successful reset (available_count will drop accordingly),
// so the response reflects the new count immediately without waiting for the background wham probe; returns the new remaining count, or -1 when unknown.
func (h *Handler) applyOptimisticResetDecrement(account *auth.Account) int {
	count, ok := account.GetRateLimitResetCredits()
	if !ok {
		return -1
	}
	if count > 0 {
		count--
		account.SetRateLimitResetCredits(count)
	}
	return count
}

// refreshUsageAfterReset refreshes the account's usage in the background (window snapshot, exact remaining count, cooldown state).
// It uses an independent context, so it is not canceled when the HTTP request ends; failures are only logged and do not affect the reset that already succeeded.
//
// The returned channel is closed when the probe finishes. The manual reset waits for it; otherwise the response would return before the probe, and the frontend would immediately
// fetch the old usage and state, which looks like "I clicked reset but the progress bar and status did not change".
// When the probe is unavailable, it returns nil, and the caller should treat that as "no need to wait".
func (h *Handler) refreshUsageAfterReset(account *auth.Account) <-chan struct{} {
	probe := h.usageProbeFunc()
	if probe == nil {
		return nil
	}
	h.resetCreditPostMu.Lock()
	if h.resetCreditPostClosed {
		h.resetCreditPostMu.Unlock()
		return nil
	}
	parentCtx := h.resetCreditPostCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	h.resetCreditPostWG.Add(1)
	h.resetCreditPostMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer h.resetCreditPostWG.Done()
		defer close(done)
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()
		if err := probe(ctx, account); err != nil {
			log.Printf("[account %d] background usage refresh after reset failed: %v", account.DBID, err)
		}
	}()
	return done
}

// resetUsageRefreshWait is the upper bound on how long a manual reset waits for the usage probe. The probe itself is one wham call,
// normally 1 to 3 seconds; on timeout, return first and let the frontend refresh once more later with usage_refreshed=false.
const resetUsageRefreshWait = 15 * time.Second

// waitUsageRefreshAfterReset waits for the post-reset usage refresh to land, and returns whether it finished before the timeout.
func waitUsageRefreshAfterReset(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(resetUsageRefreshWait):
		return false
	}
}

// resetCreditLock returns the effective workspace-level reset mutex (created on demand). Even when the same workspace is re-imported as several database records,
// manual and automatic consumption must still be serialized within this process.
func (h *Handler) resetCreditLock(account *auth.Account) *sync.Mutex {
	key := resetCreditLockKey(account)
	if v, ok := h.resetCreditLocks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	if actual, loaded := h.resetCreditLocks.LoadOrStore(key, mu); loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

func resetCreditLockKey(account *auth.Account) string {
	if account == nil {
		return "account:nil"
	}
	if identity := strings.TrimSpace(account.EffectiveAccountID()); identity != "" {
		return "workspace:" + identity
	}
	return "db:" + strconv.FormatInt(account.DBID, 10)
}

// refreshAccountForReset refreshes the account token when a reset hits a 401, reusing the injected refresh function
// (replaceable in tests), otherwise falling back to store.RefreshSingle.
func (h *Handler) refreshAccountForReset(ctx context.Context, id int64) error {
	if h.refreshAccount != nil {
		return h.refreshAccount(ctx, id)
	}
	if h.store == nil {
		return fmt.Errorf("account pool is not initialized")
	}
	return h.store.RefreshSingle(ctx, id)
}

// upstreamResetStatus safely reads the upstream response status code; returns 0 when resp is nil.
func upstreamResetStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// drainResetResponse reads the upstream response body to the end and closes it, so the connection can be reused.
func drainResetResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
}

// findAccountByID looks up the account by its database ID in the runtime account pool; returns nil if not found.
func (h *Handler) findAccountByID(id int64) *auth.Account {
	if h.store == nil {
		return nil
	}
	for _, acc := range h.store.Accounts() {
		if acc != nil && acc.DBID == id {
			return acc
		}
	}
	return nil
}

// upstreamResetErrorMessage extracts the error message from the upstream response for passing back.
// Known upstream error codes are translated into an English hint, with the original upstream text attached,
// in the form "<English explanation>(upstream: <original text>)", which is both readable and keeps the raw information for troubleshooting.
func upstreamResetErrorMessage(statusCode int, body []byte) string {
	raw := truncate(strings.TrimSpace(string(body)), 300)

	if zh := resetErrorCodeToChinese(body); zh != "" {
		if raw != "" {
			return zh + "(upstream: " + raw + ")"
		}
		return zh
	}

	if raw != "" {
		return raw
	}
	return "upstream returned status " + strconv.Itoa(statusCode)
}

// resetErrorCodeToChinese parses the code / reason in the upstream error body and maps it to an English explanation;
// it returns an empty string when it cannot be recognized (the caller falls back to the original text).
// Typical response: {"detail":{"code":"rate_limit_not_resettable","reason":"credits_only"}}
func resetErrorCodeToChinese(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// code may appear as detail.code or as top-level code / error.code.
	code := firstNonEmptyGJSONString(body, "detail.code", "code", "error.code")
	reason := firstNonEmptyGJSONString(body, "detail.reason", "reason", "error.reason")

	switch code {
	case "rate_limit_not_resettable":
		if reason == "credits_only" {
			return "This account is on credits-based billing, so the current rate limit does not support manual reset"
		}
		return "The current rate limit state does not support manual reset"
	case "no_reset_credits_available", "insufficient_reset_credits":
		return "This account has no manual reset credits available"
	case "rate_limit_already_reset":
		return "The account's rate limit has already been reset; no further reset is needed"
	}
	return ""
}
