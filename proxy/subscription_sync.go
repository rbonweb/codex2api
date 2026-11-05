package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

// Subscription expiry sync: wham/usage does not return a subscription expiry field, and the value in the JWT
// chatgpt_subscription_active_until stays at the old value long after renewal, and only the web endpoint
// /backend-api/subscriptions returns the authoritative expiry time after renewal (active_until). (issue #360)
//
// This endpoint sits behind Cloudflare, and a plain TLS fingerprint gets blocked (it returns an HTML loading page),
// so it must use a uTLS Chrome fingerprint plus browser request headers (Origin/Referer/Chrome UA),
// and must not carry the Codex CLI's Originator/UA.

// ChatGPTSubscriptionsURL is the web subscription info endpoint, which returns the current subscription period per workspace.
const ChatGPTSubscriptionsURL = "https://chatgpt.com/backend-api/subscriptions"

// subscriptionsURLForTest lets tests replace the endpoint URL. Production code must not assign it.
var subscriptionsURLForTest = ""

// SetSubscriptionsURLForTest lets tests replace the subscription endpoint URL and returns a restore function. Production code must not call it.
func SetSubscriptionsURLForTest(url string) (restore func()) {
	old := subscriptionsURLForTest
	subscriptionsURLForTest = url
	return func() { subscriptionsURLForTest = old }
}

// subscriptionsBrowserUserAgent imitates a browser visiting the web endpoint; it pairs with the uTLS Chrome fingerprint.
const subscriptionsBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// subscriptionHeaderProfile is the identity shape used when accessing the subscription endpoint.
//
// Measured (2026-09-12, on a machine whose egress IP was caught by the web endpoint's JS human-verification challenge): a browser disguise with Origin/Referer/Chrome UA
// gets challenged (403 HTML), while the same IP and token with the Codex CLI's UA + Originator
// gets active_until straight away with a 200. The challenge is triggered by the browser disguise, not by the IP itself. So by default it first accesses as the
// Codex identity (matching the wham probe), and falls back to the browser disguise if challenged or rejected.
type subscriptionHeaderProfile int

const (
	subscriptionHeadersCodex subscriptionHeaderProfile = iota
	subscriptionHeadersBrowser
)

func (p subscriptionHeaderProfile) String() string {
	if p == subscriptionHeadersBrowser {
		return "browser"
	}
	return "codex"
}

// applySubscriptionHeaders writes the identity headers for the subscription endpoint. The browser shape exists only to get past the web challenge
// fallback, and keeps its own browser protocol; the Codex shape comes from the same source as other maintenance requests (issue #774).
func applySubscriptionHeaders(req *http.Request, account *auth.Account, profile subscriptionHeaderProfile) error {
	req.Header.Set("Accept", "application/json")
	switch profile {
	case subscriptionHeadersBrowser:
		req.Header.Set("Origin", "https://chatgpt.com")
		req.Header.Set("Referer", "https://chatgpt.com/")
		req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	default:
		return applyCodexMaintenanceHeaders(req, account)
	}
	return nil
}

// shouldRetrySubscriptionWithBrowser decides whether it is worth retrying with the other identity after one identity is rejected:
// it switches identity only on 403 (challenge page / WAF rejection); switching headers does not help with 401/404/5xx.
func shouldRetrySubscriptionWithBrowser(err error) bool {
	var httpErr *SubscriptionHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusForbidden
}

// subscriptionProbeMinInterval is the minimum interval between two subscription expiry probes for the same account.
// The expiry time only changes on renewal or cancellation, so the web endpoint does not need frequent access.
const subscriptionProbeMinInterval = 6 * time.Hour

// ChatGPTSubscription holds the fields this service cares about from the /backend-api/subscriptions response.
type ChatGPTSubscription struct {
	PlanType      string `json:"plan_type"`
	ActiveStart   string `json:"active_start"`
	ActiveUntil   string `json:"active_until"`
	BillingPeriod string `json:"billing_period"`
	WillRenew     bool   `json:"will_renew"`
	IsDelinquent  bool   `json:"is_delinquent"`
	// GracePeriodEnd: the upstream field shape is not fixed (it may be RFC3339 or unix seconds), so keep it as-is and parse it later.
	GracePeriodEnd json.RawMessage `json:"grace_period_end_timestamp"`
}

// GracePeriodEndTime parses the grace period end time; a missing, null, or malformed value returns the zero value.
func (s *ChatGPTSubscription) GracePeriodEndTime() time.Time {
	if s == nil || len(s.GracePeriodEnd) == 0 {
		return time.Time{}
	}
	raw := strings.TrimSpace(string(s.GracePeriodEnd))
	if raw == "" || raw == "null" {
		return time.Time{}
	}
	var asString string
	if err := json.Unmarshal(s.GracePeriodEnd, &asString); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(asString)); err == nil {
			return t
		}
		return time.Time{}
	}
	var asNumber float64
	if err := json.Unmarshal(s.GracePeriodEnd, &asNumber); err == nil && asNumber > 0 {
		secs := int64(asNumber)
		if secs > 1e12 { // milliseconds
			secs /= 1000
		}
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

// SubscriptionHTTPError is the error returned when the subscription endpoint responds with a non-200 status; it keeps the status code so callers can classify it.
type SubscriptionHTTPError struct {
	Status int
	Body   string
}

func (e *SubscriptionHTTPError) Error() string {
	return fmt.Sprintf("subscriptions returned status %d: %s", e.Status, e.Body)
}

// NoSubscription reports whether the upstream explicitly says this workspace has no subscription record (k12/edu and similar plans measured as 404).
func (e *SubscriptionHTTPError) NoSubscription() bool {
	return e != nil && e.Status == http.StatusNotFound
}

// ActiveUntilTime parses active_until; a missing or malformed value returns the zero value.
func (s *ChatGPTSubscription) ActiveUntilTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	raw := strings.TrimSpace(s.ActiveUntil)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// QueryChatGPTSubscription queries the subscription info for the account's current workspace: it first accesses as the Codex CLI identity,
// and falls back to the browser disguise on 403 (each identity can be rejected on different egress IPs).
// account_id must be a workspace UUID: a user-... value written by polluted historical data makes the upstream return 500,
// so the request is skipped without being sent.
func QueryChatGPTSubscription(ctx context.Context, account *auth.Account, proxyURL string) (*ChatGPTSubscription, error) {
	sub, err := queryChatGPTSubscriptionWith(ctx, account, proxyURL, subscriptionHeadersCodex)
	if err == nil || !shouldRetrySubscriptionWithBrowser(err) || ctx.Err() != nil {
		return sub, err
	}
	firstErr := err
	sub, err = queryChatGPTSubscriptionWith(ctx, account, proxyURL, subscriptionHeadersBrowser)
	if err == nil {
		if account != nil {
			log.Printf("[account %d] subscription endpoint rejected the Codex identity (%s), browser identity succeeded", account.DBID, subscriptionErrorForStorage(firstErr))
		}
		return sub, nil
	}
	return nil, err
}

func queryChatGPTSubscriptionWith(ctx context.Context, account *auth.Account, proxyURL string, profile subscriptionHeaderProfile) (*ChatGPTSubscription, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}
	accountID := account.EffectiveAccountID()
	if accountID == "" || strings.HasPrefix(accountID, "user-") {
		// Polluted historical data may have written user-... into the account_id field, but the subscription endpoint only accepts
		// a workspace UUID (user-... returns 500); fall back to chatgpt_account_id in the AT JWT.
		if info := auth.ParseAccessToken(accessToken); info != nil {
			if v := strings.TrimSpace(info.ChatGPTAccountID); v != "" {
				accountID = v
			}
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("account has no workspace id")
	}
	if strings.HasPrefix(accountID, "user-") {
		return nil, fmt.Errorf("account id %q is not a workspace uuid", accountID)
	}

	endpoint := ChatGPTSubscriptionsURL
	useTestTransport := false
	if subscriptionsURLForTest != "" {
		endpoint = subscriptionsURLForTest
		useTestTransport = true
	}
	// When Resin is enabled, access goes through the reverse proxy (the fingerprint is handled on the Resin side), consistent with other account maintenance requests,
	// to avoid all accounts sharing this machine's egress IP for direct connections (issue #372).
	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, endpoint)

	// The web endpoint uses a uTLS Chrome fingerprint (both identity shapes use it), independent of the gateway's transport mode setting.
	// Held in a pool (isolated per account + proxy): a one-off uTLS transport is discarded after use, and its
	// HTTP/2 connections have no owner to close them and would leak continuously (issue #446).
	var client *http.Client
	switch {
	case resinClient != nil:
		client = resinClient
	case useTestTransport:
		// For the httptest server (plain HTTP) in tests, uTLS dialing cannot be used, so fall back to the standard transport.
		client = &http.Client{Transport: newCodexStandardTransport(proxyURL)}
	default:
		client = getMaintenanceClient(account, proxyURL, maintenancePurposeSubscription, true)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL+"?account_id="+url.QueryEscape(accountID), nil)
	if err != nil {
		return nil, fmt.Errorf("build subscriptions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if err := applySubscriptionHeaders(req, account, profile); err != nil {
		return nil, err
	}
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subscriptions request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, fmt.Errorf("read subscriptions response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &SubscriptionHTTPError{Status: resp.StatusCode, Body: truncateForLog(body, 200)}
	}

	var sub ChatGPTSubscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse subscriptions response: %w", err)
	}
	return &sub, nil
}

// SubscriptionSyncOutcome classifies the result of one subscription sync.
type SubscriptionSyncOutcome string

const (
	// SubscriptionSyncUpdated means a new authoritative expiry time was obtained and written.
	SubscriptionSyncUpdated SubscriptionSyncOutcome = "updated"
	// SubscriptionSyncUnchanged means the query succeeded and the expiry time matches the known value.
	SubscriptionSyncUnchanged SubscriptionSyncOutcome = "unchanged"
	// SubscriptionSyncNoSubscription means the subscription provider has no subscription record for this workspace (not counted as a failure).
	SubscriptionSyncNoSubscription SubscriptionSyncOutcome = "no_subscription"
	// SubscriptionSyncUnsupported means a non-paid plan, so no query is sent.
	SubscriptionSyncUnsupported SubscriptionSyncOutcome = "unsupported"
	// SubscriptionSyncFailed means the query failed (network, auth, blocked, or a missing response field).
	SubscriptionSyncFailed SubscriptionSyncOutcome = "failed"
)

// SubscriptionSyncResult is the structured result of one subscription sync.
type SubscriptionSyncResult struct {
	Outcome      SubscriptionSyncOutcome
	ExpiresAt    time.Time
	Subscription *ChatGPTSubscription
	Err          error
}

// Updated reports whether a new expiry time was written.
func (r SubscriptionSyncResult) Updated() bool { return r.Outcome == SubscriptionSyncUpdated }

// subscriptionErrorForStorage condenses the error into one sentence that can be stored and displayed:
// for non-200 responses it keeps only the status code; an HTML response body (a Cloudflare block page) is marked as suspected blocking, and the whole page is not stored.
// SubscriptionErrorAntiBotChallenge is the stable error text used when the web endpoint requires human verification for the egress IP,
// so the frontend can show a "bind a proxy and retry" hint.
const SubscriptionErrorAntiBotChallenge = "anti-bot challenge for this exit IP; bind a proxy to the account and retry"

// SubscriptionErrorSummary is the exported alias of subscriptionErrorForStorage, reused by admin responses.
func SubscriptionErrorSummary(err error) string { return subscriptionErrorForStorage(err) }

func subscriptionErrorForStorage(err error) string {
	if err == nil {
		return ""
	}
	var httpErr *SubscriptionHTTPError
	if errors.As(err, &httpErr) {
		body := strings.TrimSpace(httpErr.Body)
		lower := strings.ToLower(body)
		if strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype") || strings.Contains(lower, "<head>") {
			// Both identities are stopped by the web human verification ("Enable JavaScript and cookies to continue"),
			// the gateway cannot pass the JS challenge; the remaining option is to bind a clean proxy to the account, which the copy says directly.
			return fmt.Sprintf("HTTP %d: %s", httpErr.Status, SubscriptionErrorAntiBotChallenge)
		}
		if len(body) > 120 {
			body = body[:120] + "…"
		}
		if body == "" {
			return fmt.Sprintf("HTTP %d", httpErr.Status)
		}
		return fmt.Sprintf("HTTP %d: %s", httpErr.Status, body)
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// SyncSubscriptionExpiry always queries the subscription provider once and stores the result in the account's subscription metadata:
// on success it writes the authoritative expiry time, auto-renewal and grace period, and marks it confirmed; 404 marks it unsupported;
// other failures mark it failed and keep the last known business state. The caller is responsible for throttling.
func SyncSubscriptionExpiry(ctx context.Context, store *auth.Store, account *auth.Account, proxyURL string) SubscriptionSyncResult {
	if store == nil || account == nil {
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, Err: fmt.Errorf("store or account is nil")}
	}
	now := time.Now()
	planType := account.GetPlanType()
	previousExpiry := account.GetSubscriptionExpiresAt()
	if !auth.SubscriptionPlanTracked(planType, previousExpiry) || strings.EqualFold(strings.TrimSpace(planType), "free") {
		return SubscriptionSyncResult{Outcome: SubscriptionSyncUnsupported}
	}
	// The attempt time is recorded whether it succeeds or fails: when the upstream is abnormal, avoid re-accessing the web endpoint on every probe.
	account.MarkSubscriptionExpiryProbed(now)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sub, err := QueryChatGPTSubscription(ctx, account, proxyURL)
	if err != nil {
		var httpErr *SubscriptionHTTPError
		if errors.As(err, &httpErr) && httpErr.NoSubscription() {
			store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
				m.CheckedAt = now
				m.SyncState = auth.SubscriptionSyncUnsupported
				m.WillRenew = auth.SubscriptionAutoRenewUnsupported
				m.Error = ""
			})
			return SubscriptionSyncResult{Outcome: SubscriptionSyncNoSubscription, ExpiresAt: previousExpiry}
		}
		log.Printf("[account %d] subscription expiry sync failed: %v", account.DBID, err)
		store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
			m.CheckedAt = now
			m.Error = subscriptionErrorForStorage(err)
			if m.LastKnownStatus == "" && previousExpiry.IsZero() {
				m.SyncState = auth.SubscriptionSyncUnknown
			} else {
				m.SyncState = auth.SubscriptionSyncFailed
			}
		})
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, ExpiresAt: previousExpiry, Err: err}
	}

	activeUntil := sub.ActiveUntilTime()
	if activeUntil.IsZero() {
		err := fmt.Errorf("subscriptions response has no active_until")
		store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
			m.CheckedAt = now
			m.Error = err.Error()
			if m.LastKnownStatus == "" && previousExpiry.IsZero() {
				m.SyncState = auth.SubscriptionSyncUnknown
			} else {
				m.SyncState = auth.SubscriptionSyncFailed
			}
		})
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, ExpiresAt: previousExpiry, Subscription: sub, Err: err}
	}

	graceUntil := sub.GracePeriodEndTime()
	willRenew := auth.SubscriptionAutoRenewDisabled
	if sub.WillRenew {
		willRenew = auth.SubscriptionAutoRenewEnabled
	}

	// A past active_until contradicts "paid plan still in use": when there is a grace period end time, store by the grace period
	// (stale-value cleanup would skip an expiry time within the grace period); without one, do not write, to avoid writing a value that gets cleaned up right away,
	// which would make the display flip back and forth.
	applyExpiry := activeUntil.After(now) || (!graceUntil.IsZero() && graceUntil.After(now))
	changed := false
	if applyExpiry {
		changed = store.UpdateAccountSubscriptionExpiresAt(account, activeUntil)
	}
	effectiveExpiry := previousExpiry
	if applyExpiry {
		effectiveExpiry = activeUntil
	}
	status, _, _ := auth.ComputeSubscriptionBusinessStatus(effectiveExpiry, graceUntil, now, time.Local)
	store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
		m.CheckedAt = now
		m.Error = ""
		m.WillRenew = willRenew
		if applyExpiry {
			m.SyncState = auth.SubscriptionSyncConfirmed
			m.Source = auth.SubscriptionSourceProviderAPI
			m.LastKnownStatus = status
			m.GraceUntil = graceUntil
		} else if m.SyncState != auth.SubscriptionSyncPending {
			// The provider gave a past expiry time with no grace period, which contradicts the paid plan still being in use:
			// "renewed · pending confirmation" stays pending confirmation; otherwise record it as a successful query.
			m.SyncState = auth.SubscriptionSyncConfirmed
		}
		// It only counts as a detected renewal when the validity period was actually extended; getting an expiry time for the first time does not count.
		if changed && !previousExpiry.IsZero() && activeUntil.After(previousExpiry) {
			m.RenewalDetectedAt = now
		}
	})
	if changed {
		log.Printf("[account %d] synced subscription expiry time from upstream: %s", account.DBID, activeUntil.Format(time.RFC3339))
		return SubscriptionSyncResult{Outcome: SubscriptionSyncUpdated, ExpiresAt: activeUntil, Subscription: sub}
	}
	return SubscriptionSyncResult{Outcome: SubscriptionSyncUnchanged, ExpiresAt: effectiveExpiry, Subscription: sub}
}

// MaybeSyncSubscriptionExpiry syncs the authoritative subscription expiry time from the web endpoint on demand (best-effort):
// it only starts for paid plans whose expiry time is unknown, near, or past, and is throttled; failures are only logged and do not affect the caller.
// It returns whether the expiry time was updated.
func MaybeSyncSubscriptionExpiry(ctx context.Context, store *auth.Store, account *auth.Account, proxyURL string) bool {
	if store == nil || account == nil {
		return false
	}
	if !account.NeedsSubscriptionExpiryProbe(time.Now(), subscriptionProbeMinInterval) {
		return false
	}
	return SyncSubscriptionExpiry(ctx, store, account, proxyURL).Updated()
}

// After a stale expiry time is cleaned up (inferred as renewed), an authoritative sync is started immediately to move pending to
// confirmed as soon as possible. It runs asynchronously; it is not started again while the same account already has one in flight.
func init() {
	auth.OnStaleSubscriptionCleared = func(store *auth.Store, account *auth.Account) {
		if store == nil || account == nil {
			return
		}
		// In the test process, do not really hit chatgpt.com when the subscription endpoint has not been replaced.
		if testing.Testing() && subscriptionsURLForTest == "" {
			return
		}
		if !account.BeginSubscriptionSync() {
			return
		}
		go func() {
			defer account.EndSubscriptionSync()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			SyncSubscriptionExpiry(ctx, store, account, store.ResolveProxyForAccount(account))
		}()
	}
}
