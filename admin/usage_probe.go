package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// errWhamUnauthorized marks that the wham probe hit a 401.
// wham (the ChatGPT backend quota endpoint) and the /responses gateway apply different authentication rules: accounts imported as plain AT
// (codex_at) may get a constant 401 from wham, for example because the token lacks the workspace claim,
// while real traffic works fine (issue #328). So a wham 401 must not on its own convict the account and ban it;
// ProbeUsageSnapshot must decide whether to use the /responses probe to settle it.
var errWhamUnauthorized = errors.New("wham usage probe unauthorized")

type usageProbeRequestFunc func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error)

func inspectResponsesProbeBody(body []byte) (responsesTerminalOutcome, []byte, error) {
	outcome := responsesTerminalUnknown
	var terminalPayload []byte
	err := proxy.ReadSSEStream(bytes.NewReader(body), func(data []byte) bool {
		classified := classifyResponsesTerminalEvent(data)
		if classified == responsesTerminalUnknown {
			return true
		}
		outcome = classified
		terminalPayload = append(terminalPayload[:0], data...)
		return false
	})
	if err != nil {
		return responsesTerminalUnknown, nil, err
	}
	// Test doubles or non-streaming compatible endpoints may return a single event JSON directly.
	if outcome == responsesTerminalUnknown {
		outcome = classifyResponsesTerminalEvent(body)
		if outcome != responsesTerminalUnknown {
			terminalPayload = append(terminalPayload[:0], body...)
		}
	}
	return outcome, terminalPayload, nil
}

// ProbeUsageSnapshot actively refreshes account usage.
//
// First try /backend-api/wham/usage (a structured endpoint with zero quota cost);
// on failure (4xx/5xx/network), fall back to sending a minimal request to /backend-api/codex/responses
// (this really counts toward usage but stays backward compatible). When the Responses authoritative rate-limit decision is enabled and the account is already in
// usage cooldown, a /responses probe is sent even if wham succeeds: wham only updates display metadata,
// and only a real Responses result can confirm whether the account has recovered.
// Authentication verdict: a wham 401 does not ban the account on its own; the /responses fallback probe decides (issue #328).
func (h *Handler) ProbeUsageSnapshot(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}
	// Agent Identity has no AccessToken, so wham (Bearer) cannot be used; use the minimal /responses probe directly
	// (ExecuteRequest signs dynamically with AgentAssertion), and syncs the usage snapshot from the response headers.
	if account.IsCodexAgentIdentity() {
		return h.probeUsageViaResponses(ctx, account)
	}

	account.Mu().RLock()
	hasToken := account.AccessToken != ""
	account.Mu().RUnlock()
	if !hasToken {
		return nil
	}

	// By default, in rate-limited or cooldown state (429 or premium 5h rate limit), only wham is used (zero cost).
	// Exception in Responses authoritative mode: if only wham is ever used, a cooled-down account never gets a chance to
	// prove recovery with a Responses 200, and the whole pool could stay on local 503s for a long time.
	limited := account.InLimitedState()
	responsesFallback := h.store.UsageProbeResponsesFallbackEnabled()
	lazyMode := h.store.GetLazyMode()
	authoritativeRecovery := limited && account.IgnoresUsageLimitStatus() && responsesFallback
	whamOnly := (limited && !authoritativeRecovery) || (lazyMode && !authoritativeRecovery) || !responsesFallback

	// 1) Prefer wham (zero cost)
	if err := h.probeUsageViaWham(ctx, account, limited); err == nil {
		if !authoritativeRecovery {
			return nil
		}
		log.Printf("[account %d] wham usage metadata refreshed, continuing with /responses to decide rate-limit recovery", account.DBID)
	} else if errors.Is(err, errWhamUnauthorized) {
		// A wham 401 does not ban directly (a codex_at account may get a constant wham 401 while its traffic works, issue #328):
		// when fallback is possible, hand it to the /responses probe for the final authentication verdict (200 recovers / only 401 bans);
		// when fallback is not possible, only log and do not ban; a truly invalid token is cooled down by the gateway when real traffic gets a 401.
		if whamOnly {
			log.Printf("[account %d] wham probe 401, no /responses corroboration (rate limited / lazy / fallback disabled), skipping ban: %v", account.DBID, err)
			return err
		}
		log.Printf("[account %d] wham probe 401, leaving the authentication state to the /responses probe: %v", account.DBID, err)
	} else {
		if whamOnly {
			log.Printf("[account %d] wham usage probe failed, skipped the /responses probe per configuration / rate-limit state: %v", account.DBID, err)
			return err
		}
		log.Printf("[account %d] wham usage probe failed, falling back to the /responses probe: %v", account.DBID, err)
	}

	// 2) Fallback: the existing minimal /responses probe
	return h.probeUsageViaResponses(ctx, account)
}

// probeUsageViaWham fetches usage through /backend-api/wham/usage,
// consuming no token quota.
//
// limited=true means the account is in a 429 cooldown / premium 5h rate-limit state: this call is only a zero-cost refresh
// of the "active reset count" and usage snapshot. It does not report success or clear the cooldown (lifting the cooldown is left to the recovery probe / expiry check),
// so that a quota lookup is not mistaken for the account having recovered.
func (h *Handler) probeUsageViaWham(ctx context.Context, account *auth.Account, limited bool) error {
	probeStartedAt := time.Now()
	usage, resp, err := proxy.QueryWhamUsage(ctx, account, h.store.ResolveProxyForAccount(account))
	if resp != nil {
		// QueryWhamUsage does not read the body on non-200; here a short piece is read for the account error details.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			if isDefinitiveRevokedTokenError(body) {
				// Unlike an ordinary WHAM 401 such as a missing workspace claim, token_revoked /
				// token_invalidated is the upstream's explicit verdict that the OAuth credential is invalid, and needs no
				// /responses corroboration. Otherwise, with fallback disabled, the account would stay "unsampled" forever.
				h.store.ReportRequestFailure(account, "client", 0)
				errorMsg := fmt.Sprintf("usage probe upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300))
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", errorMsg)
				return nil
			}
			// Do not report a failure or ban here: a wham 401 may be a false positive for codex_at accounts,
			// and repeatedly counting it as a failure sample would also pollute health statistics. Leave the verdict to ProbeUsageSnapshot.
			return fmt.Errorf("%w: upstream returned %d: %s", errWhamUnauthorized, resp.StatusCode, truncate(string(body), 300))
		case http.StatusTooManyRequests:
			h.store.ReportRequestFailure(account, "client", 0)
		case http.StatusPaymentRequired, http.StatusForbidden:
			// Unlike a wham 401, deactivated_workspace is the upstream's explicit verdict on the workspace state
			// (the error body carries detail.code), so there is no false positive from differing authentication rules; in wham-only mode
			// if it is not marked as an error here, an account whose team workspace was banned would stay in the pool forever as "available", and sampling would
			// keep failing. Act only when the error body confirms it; a bare 402/403 keeps the generic failure path.
			if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
				h.store.ReportRequestFailure(account, "client", 0)
				errorMsg := fmt.Sprintf("usage probe upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300))
				if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
					h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
				} else {
					h.store.MarkDeactivatedWorkspace(account, errorMsg)
				}
				return nil
			}
		}
	}
	if err != nil {
		return err
	}
	if usage == nil {
		return fmt.Errorf("wham returned empty body")
	}

	state := proxy.ApplyWhamUsage(h.store, account, usage)
	// wham does not include subscription expiry fields, so the authoritative expiry time is fetched on demand from the web /subscriptions page
	// (throttled, best effort; a failure does not affect the probe result). (issue #360)
	proxy.MaybeSyncSubscriptionExpiry(ctx, h.store, account, h.store.ResolveProxyForAccount(account))
	if limited {
		if state.UsageWindowLimitsIgnored {
			// WHAM remains metadata-only in Responses-authoritative mode. It must
			// not clear a cooldown established by a real Responses failure.
			return nil
		}
		if !state.HasUsage5h && !state.HasUsage7d && !state.Cleared5h {
			// An empty/malformed WHAM payload is not evidence that a cooldown
			// ended. Preserve the existing source state and let the next probe
			// retry with a complete response.
			return nil
		}
		// In rate-limited / cooldown state, re-judge using the authoritative usage window returned by wham:
		// if upstream has reset the window and is no longer rate limiting (for example, the official side reset the 5h/7d usage early),
		// actively lift the rate-limit cooldown, without waiting for the cooldown to expire or for the user to run a connection test manually.
		// Still do not call ReportRequestSuccess, so a zero-cost quota lookup is not counted as a healthy success sample.
		if !applyUsageLimitedAccountState(h.store, account, state) {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
			log.Printf("[account %d] wham shows the rate-limit window has reset, automatically lifting the rate-limit cooldown", account.DBID)
		}
		return nil
	}
	h.store.ReportRequestSuccess(account, 0)
	// Reset the cooldown when usage is not exhausted
	if !applyUsageLimitedAccountState(h.store, account, state) {
		if state.HasUsage5h || state.HasUsage7d || state.Cleared5h {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
		}
	}
	return nil
}

// probeUsageViaResponses is the existing probe: it sends a minimal /responses request,
// and syncs the Codex usage state from the response headers. It really consumes a small number of tokens.
func (h *Handler) probeUsageViaResponses(ctx context.Context, account *auth.Account) error {
	probeStartedAt := time.Now()
	payload := buildConnectionTestPayload(h.store, h.store.GetTestModel())
	executeRequest := usageProbeRequestFunc(proxy.ExecuteRequest)
	if h.executeUsageProbe != nil {
		executeRequest = h.executeUsageProbe
	}
	resp, err := executeRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	usageState := proxy.SyncCodexUsageState(h.store, account, resp)

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		outcome, terminalPayload, inspectErr := inspectResponsesProbeBody(body)
		if inspectErr != nil {
			return fmt.Errorf("failed to read the Responses recovery probe terminal event: %w", inspectErr)
		}
		switch outcome {
		case responsesTerminalUsageLimited:
			h.applyResponsesUsageLimitFailure(account, resp, h.store.GetTestModel(), terminalPayload)
			return nil
		case responsesTerminalFailed:
			return fmt.Errorf("Responses recovery probe did not succeed: %s", formatUpstreamTestError(terminalPayload, "upstream returned a failure terminal event"))
		case responsesTerminalUnknown:
			return fmt.Errorf("Responses recovery probe is missing a clear terminal event")
		}
		h.store.ReportRequestSuccess(account, 0)
		// Reset the state only when usage is not exhausted
		if !applyUsageLimitedAccountState(h.store, account, usageState) {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
		}
		return nil
	case http.StatusUnauthorized:
		h.store.ReportRequestFailure(account, "client", 0)
		h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", fmt.Sprintf("usage probe upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300)))
		return nil
	case http.StatusTooManyRequests:
		h.store.ReportRequestFailure(account, "client", 0)
		proxy.Apply429Cooldown(h.store, account, body, resp, h.store.GetTestModel())
		return nil
	default:
		if proxy.IsUsageLimitReachedError(body) {
			h.store.ReportRequestFailure(account, "client", 0)
			proxy.Apply429Cooldown(h.store, account, body, resp, h.store.GetTestModel())
			return nil
		}
		if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
			errorMsg := fmt.Sprintf("usage probe upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300))
			if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
				h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
			} else {
				h.store.MarkDeactivatedWorkspace(account, errorMsg)
			}
			return nil
		}
		if resp.StatusCode >= 500 {
			h.store.ReportRequestFailure(account, "server", 0)
		} else if resp.StatusCode >= 400 {
			h.store.ReportRequestFailure(account, "client", 0)
		}
		return fmt.Errorf("probe returned status %d", resp.StatusCode)
	}
}

func isDefinitiveRevokedTokenError(body []byte) bool {
	for _, path := range []string{"error.code", "detail.code", "code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String())) {
		case "token_revoked", "token_invalidated":
			return true
		}
	}
	return false
}

func shouldMarkUsageProbeAccountError(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusPaymentRequired, http.StatusForbidden:
		return proxy.IsDeactivatedWorkspaceError(body) ||
			(statusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body))
	default:
		return false
	}
}

// ForceUsageProbe actively triggers one full usage probe that "ignores the cache threshold", and returns immediately.
// The actual probes run concurrently in the background (limited by usage_probe_concurrency).
func (h *Handler) ForceUsageProbe(c *gin.Context) {
	h.store.TriggerUsageProbeForceAsync()
	payload := gin.H{
		"triggered":   true,
		"concurrency": h.store.GetUsageProbeConcurrency(),
	}
	if h.store.GetLazyMode() || !h.store.UsageProbeResponsesFallbackEnabled() {
		payload["mode"] = "wham_only"
	}
	c.JSON(http.StatusOK, payload)
}
