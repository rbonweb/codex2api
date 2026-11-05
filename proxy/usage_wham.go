package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
)

// WhamUsageURL is the ChatGPT backend usage query endpoint.
// This endpoint returns structured JSON (it consumes no quota) and can be used to get account 5h/7d usage at zero cost.
const WhamUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// WhamResetCreditsConsumeURL is the endpoint that "consumes 1 manual reset and resets the quota immediately".
const WhamResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// WhamResetCreditsURL lists all "manual reset" credentials of the account (including each one's issue/expiry time).
// Like wham/usage, it consumes no quota (issue #322: shows the validity details of each reset credit).
const WhamResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// whamURLForTest allows tests to replace the default URL. Production code must not assign it.
var whamURLForTest = ""

// SetWhamUsageURLForTest lets tests in other packages replace the wham usage endpoint URL, and returns a restore function.
// Production code must not call it.
func SetWhamUsageURLForTest(url string) (restore func()) {
	old := whamURLForTest
	whamURLForTest = url
	return func() { whamURLForTest = old }
}

// whamConsumeURLForTest allows tests to replace the reset endpoint URL. Production code must not assign it.
var whamConsumeURLForTest = ""

// whamResetCreditsURLForTest allows tests to replace the reset credit list endpoint URL. Production code must not assign it.
var whamResetCreditsURLForTest = ""

// WhamUsage is the response structure of /backend-api/wham/usage.
type WhamUsage struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`

	// After OpenAI renewal, chatgpt_subscription_active_until in the JWT is not necessarily refreshed with the authorization immediately,
	// so the current server-side subscription expiry returned by wham/usage is used to sync the admin panel display and the scheduling weight.
	SubscriptionExpiresAtRaw          whamTimeRaw `json:"subscription_expires_at,omitempty"`
	SubscriptionActiveUntilRaw        whamTimeRaw `json:"subscription_active_until,omitempty"`
	ChatGPTSubscriptionActiveUntilRaw whamTimeRaw `json:"chatgpt_subscription_active_until,omitempty"`

	RateLimit struct {
		Allowed         bool             `json:"allowed"`
		LimitReached    bool             `json:"limit_reached"`
		PrimaryWindow   *WhamUsageWindow `json:"primary_window"`
		SecondaryWindow *WhamUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`

	// AdditionalRateLimits are extra windows independent of the main 5h/7d (for example spark).
	AdditionalRateLimits    []WhamAdditionalRateLimit `json:"additional_rate_limits,omitempty"`
	AdditionalRateLimitsAlt []WhamAdditionalRateLimit `json:"additionalRateLimits,omitempty"`

	Credits              *WhamCredits              `json:"credits,omitempty"`
	SpendControl         *WhamSpendControl         `json:"spend_control,omitempty"`
	RateLimitReachedType *WhamRateLimitReachedType `json:"rate_limit_reached_type,omitempty"`

	// RateLimitResetCredits is the account's remaining "manual reset count" on OpenAI's side.
	// When available_count > 0, wham/rate-limit-reset-credits/consume can be called to reset the quota immediately.
	// applicable_available_count is the number currently "applicable": upstream returns 0 when the limit has not been hit
	// (the credit is within its validity period but not in effect right now), and it is > 0 only after the limit is hit.
	RateLimitResetCredits *struct {
		AvailableCount           int `json:"available_count"`
		ApplicableAvailableCount int `json:"applicable_available_count"`
	} `json:"rate_limit_reset_credits,omitempty"`
}

type WhamCredits struct {
	HasCredits          bool    `json:"has_credits"`
	Unlimited           bool    `json:"unlimited"`
	OverageLimitReached bool    `json:"overage_limit_reached"`
	Balance             *string `json:"balance"`
	ApproxLocalMessages []int   `json:"approx_local_messages"`
	ApproxCloudMessages []int   `json:"approx_cloud_messages"`
}

type WhamSpendControl struct {
	Reached         bool        `json:"reached"`
	IndividualLimit interface{} `json:"individual_limit"`
}

type WhamRateLimitReachedType struct {
	Type string `json:"type"`
}

type whamTimeRaw string

func (w *whamTimeRaw) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*w = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		*w = whamTimeRaw(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err == nil {
		*w = whamTimeRaw(n.String())
		return nil
	}
	return nil
}

func (w *WhamUsage) SubscriptionExpiresAt() time.Time {
	if w == nil {
		return time.Time{}
	}
	for _, raw := range []whamTimeRaw{
		w.SubscriptionExpiresAtRaw,
		w.SubscriptionActiveUntilRaw,
		w.ChatGPTSubscriptionActiveUntilRaw,
	} {
		if t := parseWhamTime(string(raw)); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func parseWhamTime(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
		// Compatible with both second-level and millisecond-level unix timestamps.
		if unix > 1_000_000_000_000 {
			return time.UnixMilli(unix)
		}
		return time.Unix(unix, 0)
	}
	return time.Time{}
}

// WhamUsageWindow is a single rate-limit window (primary=5h, secondary=7d).
type WhamUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func (w *WhamUsageWindow) UnmarshalJSON(data []byte) error {
	type windowAlias struct {
		UsedPercent           flexibleFloat `json:"used_percent"`
		UsedPercentAlt        flexibleFloat `json:"usedPercent"`
		LimitWindowSeconds    flexibleInt64 `json:"limit_window_seconds"`
		LimitWindowSecondsAlt flexibleInt64 `json:"limitWindowSeconds"`
		ResetAfterSeconds     flexibleInt64 `json:"reset_after_seconds"`
		ResetAfterSecondsAlt  flexibleInt64 `json:"resetAfterSeconds"`
		ResetAt               flexibleInt64 `json:"reset_at"`
		ResetAtAlt            flexibleInt64 `json:"resetAt"`
	}
	var raw windowAlias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	w.UsedPercent = raw.UsedPercent.or(raw.UsedPercentAlt)
	w.LimitWindowSeconds = raw.LimitWindowSeconds.or(raw.LimitWindowSecondsAlt)
	w.ResetAfterSeconds = raw.ResetAfterSeconds.or(raw.ResetAfterSecondsAlt)
	w.ResetAt = raw.ResetAt.or(raw.ResetAtAlt)
	return nil
}

type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		*f = 0
		return nil
	}
	var n float64
	if err := json.Unmarshal(trimmed, &n); err == nil {
		*f = flexibleFloat(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return err
	}
	*f = flexibleFloat(parsed)
	return nil
}

func (f flexibleFloat) or(alt flexibleFloat) float64 {
	if f != 0 {
		return float64(f)
	}
	return float64(alt)
}

type flexibleInt64 int64

func (n *flexibleInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		*n = 0
		return nil
	}
	var v int64
	if err := json.Unmarshal(trimmed, &v); err == nil {
		*n = flexibleInt64(v)
		return nil
	}
	var f float64
	if err := json.Unmarshal(trimmed, &f); err == nil {
		*n = flexibleInt64(int64(f))
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return err
	}
	*n = flexibleInt64(parsed)
	return nil
}

func (n flexibleInt64) or(alt flexibleInt64) int64 {
	if n != 0 {
		return int64(n)
	}
	return int64(alt)
}

// WhamRateLimitInfo is a primary/secondary window segment in WHAM.
type WhamRateLimitInfo struct {
	Allowed            bool             `json:"allowed"`
	LimitReached       bool             `json:"limit_reached"`
	LimitReachedAlt    bool             `json:"limitReached"`
	PrimaryWindow      *WhamUsageWindow `json:"primary_window"`
	PrimaryWindowAlt   *WhamUsageWindow `json:"primaryWindow"`
	SecondaryWindow    *WhamUsageWindow `json:"secondary_window"`
	SecondaryWindowAlt *WhamUsageWindow `json:"secondaryWindow"`
}

func (r *WhamRateLimitInfo) primaryWindow() *WhamUsageWindow {
	if r == nil {
		return nil
	}
	if r.PrimaryWindow != nil {
		return r.PrimaryWindow
	}
	return r.PrimaryWindowAlt
}

// WhamAdditionalRateLimit is one independent quota entry in WHAM additional_rate_limits.
type WhamAdditionalRateLimit struct {
	LimitName      string             `json:"limit_name,omitempty"`
	LimitNameAlt   string             `json:"limitName,omitempty"`
	MeteredFeature string             `json:"metered_feature,omitempty"`
	MeteredAlt     string             `json:"meteredFeature,omitempty"`
	RateLimit      *WhamRateLimitInfo `json:"rate_limit,omitempty"`
	RateLimitAlt   *WhamRateLimitInfo `json:"rateLimit,omitempty"`
}

func (u *WhamUsage) additionalRateLimits() []WhamAdditionalRateLimit {
	if u == nil {
		return nil
	}
	if len(u.AdditionalRateLimits) > 0 {
		return u.AdditionalRateLimits
	}
	return u.AdditionalRateLimitsAlt
}

func (l WhamAdditionalRateLimit) name() string {
	for _, raw := range []string{l.LimitName, l.LimitNameAlt, l.MeteredFeature, l.MeteredAlt} {
		if name := strings.TrimSpace(raw); name != "" {
			return name
		}
	}
	return ""
}

func (l WhamAdditionalRateLimit) primaryWindow() *WhamUsageWindow {
	if window := l.RateLimit.primaryWindow(); window != nil {
		return window
	}
	return l.RateLimitAlt.primaryWindow()
}

func isSparkWhamLimitName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	return n == "spark" || strings.HasPrefix(n, "spark-") || strings.HasSuffix(n, "-spark") || strings.Contains(n, "-spark-")
}

func pickSparkWhamWindow(limits []WhamAdditionalRateLimit) *WhamUsageWindow {
	for i := range limits {
		if !isSparkWhamLimitName(limits[i].name()) {
			continue
		}
		window := limits[i].primaryWindow()
		if !usableWhamWindow(window) {
			continue
		}
		// This only recognizes the spark 5h entry; weekly/monthly windows are left for later, to avoid writing into the independent 5h snapshot by mistake.
		if window.LimitWindowSeconds == whamWindow7dSeconds || auth.IsMonthlyWindowSeconds(window.LimitWindowSeconds) {
			continue
		}
		return window
	}
	return nil
}

// QueryWhamUsage calls /backend-api/wham/usage to get the account's current usage.
// This call consumes no token quota, which is cheaper than sending a minimal /responses request.
func QueryWhamUsage(ctx context.Context, account *auth.Account, proxyURL string) (*WhamUsage, *http.Response, error) {
	url := WhamUsageURL
	if whamURLForTest != "" {
		url = whamURLForTest
	}
	return queryWhamUsageWithURL(ctx, account, proxyURL, url)
}

// whamHTTPClient is the Resin/direct choice shared by the three wham endpoints: when viaResin is set,
// it sets X-Resin-Account and reuses the per-account isolated Resin connection pool; otherwise it falls back to the same transport as the gateway
// transport (which supports the uTLS Chrome fingerprint), so wham requests and /responses use the same
// TLS fingerprint, reducing the chance of being blocked by Cloudflare.
//
// The direct branch must use the pooled Client: the wham probe is a periodic background task, and creating a one-off
// uTLS transport each time keeps leaking HTTP/2 connections and goroutines (issue #446).
func whamHTTPClient(req *http.Request, account *auth.Account, resinClient *http.Client, viaResin bool, proxyURL string) *http.Client {
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
		return resinClient
	}
	return getCodexMaintenanceClient(account, proxyURL)
}

func queryWhamUsageWithURL(ctx context.Context, account *auth.Account, proxyURL, url string) (*WhamUsage, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}

	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build wham request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if err := applyCodexMaintenanceHeaders(req, account); err != nil {
		return nil, nil, err
	}
	// Use EffectiveAccountID: when a custom header overrides the workspace ID, the quota must be queried in the overridden workspace,
	// otherwise the progress bar, auto-pause and smart pacing would count a workspace different from the actual traffic.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("wham request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The caller needs to trigger a refresh / cooldown based on the status code; resp is returned so the upper layer can handle the body.
		return nil, resp, fmt.Errorf("wham returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read wham response: %w", err)
	}

	var usage WhamUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, resp, fmt.Errorf("parse wham response: %w", err)
	}
	return &usage, resp, nil
}

// WhamResetCredit is the metadata of the reset credential redeemed in the consume response.
type WhamResetCredit struct {
	ID         string `json:"id,omitempty"`
	ResetType  string `json:"reset_type,omitempty"`
	Status     string `json:"status,omitempty"`
	RedeemedAt string `json:"redeemed_at,omitempty"`
}

// WhamResetCreditItem is a single reset credit in the /wham/rate-limit-reset-credits list.
// A credit with reset_type=codex_rate_limits and status=available is the one that is "currently available".
type WhamResetCreditItem struct {
	ID              string `json:"id"`
	ResetType       string `json:"reset_type"`
	Status          string `json:"status"`
	GrantedAt       string `json:"granted_at"`
	ExpiresAt       string `json:"expires_at"`
	ConsumableUntil string `json:"consumable_until"`
}

// EffectiveConsumableUntil returns the actual consumable deadline declared by upstream.
// The new API uses consumable_until; old or compatible responses may still carry only expires_at.
func (c WhamResetCreditItem) EffectiveConsumableUntil() string {
	if value := strings.TrimSpace(c.ConsumableUntil); value != "" {
		return value
	}
	return strings.TrimSpace(c.ExpiresAt)
}

// WhamResetCreditsList is the response structure of /wham/rate-limit-reset-credits.
type WhamResetCreditsList struct {
	AvailableCount int                   `json:"available_count"`
	Credits        []WhamResetCreditItem `json:"credits"`
}

// AvailableCodexCredits returns the codex rate-limit reset credits in the list that are available (reset_type=codex_rate_limits
// and status=available and carry an expiry time); all other states (consumed / expired / non-codex types) are filtered out.
func (l *WhamResetCreditsList) AvailableCodexCredits() []WhamResetCreditItem {
	if l == nil {
		return nil
	}
	out := make([]WhamResetCreditItem, 0, len(l.Credits))
	for _, c := range l.Credits {
		if c.ResetType != "codex_rate_limits" {
			continue
		}
		if c.Status != "available" {
			continue
		}
		if c.EffectiveConsumableUntil() == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// QueryWhamResetCredits calls /backend-api/wham/rate-limit-reset-credits to get the account's
// reset credit details (issue and expiry time of each one). Zero quota cost, with the same request shape as QueryWhamUsage.
// On a non-200 status it returns resp (body unread) for the caller to handle.
func QueryWhamResetCredits(ctx context.Context, account *auth.Account, proxyURL string) (*WhamResetCreditsList, *http.Response, error) {
	url := WhamResetCreditsURL
	if whamResetCreditsURLForTest != "" {
		url = whamResetCreditsURLForTest
	}
	return queryWhamResetCreditsWithURL(ctx, account, proxyURL, url)
}

func queryWhamResetCreditsWithURL(ctx context.Context, account *auth.Account, proxyURL, url string) (*WhamResetCreditsList, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}

	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build reset-credits list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if err := applyCodexMaintenanceHeaders(req, account); err != nil {
		return nil, nil, err
	}
	// As with the wham query, reset credits are queried in the workspace overridden by the custom header.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("reset-credits list request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("reset-credits list returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read reset-credits list response: %w", err)
	}

	var list WhamResetCreditsList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, resp, fmt.Errorf("parse reset-credits list response: %w", err)
	}
	return &list, resp, nil
}

// WhamResetResult is the projection of a successful /wham/rate-limit-reset-credits/consume response.
// windows_reset is how many rate-limit windows this reset cleared; it can give immediate feedback and saves one more usage query after the reset.
type WhamResetResult struct {
	Code         string           `json:"code"`
	WindowsReset int              `json:"windows_reset"`
	Credit       *WhamResetCredit `json:"credit,omitempty"`
}

// ConsumeResetCredit consumes 1 of the account's "manual reset count" to reset the quota immediately.
// Sends a POST to /backend-api/wham/rate-limit-reset-credits/consume, with a body that carries a
// random idempotency key redeem_request_id. On success (2xx) it returns nil; on non-2xx it returns a resp with the status code,
// and the caller uses it to trigger a refresh / cooldown / error message.
func ConsumeResetCredit(ctx context.Context, account *auth.Account, proxyURL string) (*http.Response, error) {
	_, resp, err := ConsumeResetCreditParsed(ctx, account, proxyURL, uuid.New().String())
	return resp, err
}

// ConsumeResetCreditWithID is the same as ConsumeResetCredit, but the caller supplies redeem_request_id.
// When retrying the same reset, always reuse the same ID: upstream uses it as the idempotency key, so reusing it avoids
// consuming one precious reset count twice when a network-jitter retry happens.
func ConsumeResetCreditWithID(ctx context.Context, account *auth.Account, proxyURL, redeemRequestID string) (*http.Response, error) {
	_, resp, err := ConsumeResetCreditParsed(ctx, account, proxyURL, redeemRequestID)
	return resp, err
}

// ConsumeResetCreditParsed parses the successful response (windows_reset / credit) while consuming the reset count,
// so the caller can give immediate feedback and drop the extra usage query round trip after the reset.
// On 2xx it returns the parsed result and resp.Body is closed; on non-2xx result is nil and resp.Body is kept so the caller can read the error.
func ConsumeResetCreditParsed(ctx context.Context, account *auth.Account, proxyURL, redeemRequestID string) (*WhamResetResult, *http.Response, error) {
	url := WhamResetCreditsConsumeURL
	if whamConsumeURLForTest != "" {
		url = whamConsumeURLForTest
	}
	return consumeResetCreditWithURL(ctx, account, proxyURL, url, redeemRequestID)
}

func consumeResetCreditWithURL(ctx context.Context, account *auth.Account, proxyURL, url, redeemRequestID string) (*WhamResetResult, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}

	if strings.TrimSpace(redeemRequestID) == "" {
		redeemRequestID = uuid.New().String()
	}
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": redeemRequestID})

	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, finalURL, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build reset-credit request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := applyCodexMaintenanceHeaders(req, account); err != nil {
		return nil, nil, err
	}
	// As with the wham query, the reset quota also applies to the workspace overridden by the custom header.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("reset-credit request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Keep resp so the caller can read the body and handle it by status code (401 refresh, 429, etc.).
		return nil, resp, fmt.Errorf("reset-credit returned status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	// Parsing is best-effort: the reset already succeeded, so a parse failure must not fail the whole operation.
	result := &WhamResetResult{}
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, result)
	}
	return result, resp, nil
}

// ApplyWhamUsage writes the data returned by /wham/usage into the account state and persists it.
// Behavior stays consistent with SyncCodexUsageState (when handling /responses response headers):
//   - plan_type is synced to memory + DB
//   - Windows are classified precisely by limit_window_seconds (18000s→5h, 604800s→7d);
//     when there is no exact match, 7d is identified first by free plan / long reset, then falls back to field position.
//   - The 5h window is written with SetUsageSnapshot5h
//   - The 7d window goes through PersistUsageSnapshot
//   - When the premium 5h is exhausted, it goes through MarkPremium5hRateLimited
func ApplyWhamUsage(store *auth.Store, account *auth.Account, usage *WhamUsage) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || usage == nil {
		return result
	}
	observedAt := time.Now()
	result.UsageWindowLimitsIgnored = account.SkipsUsageWindowLimits()

	if store != nil && usage.PlanType != "" {
		store.UpdateAccountPlanType(account, usage.PlanType)
	}
	if store != nil {
		// Only backfill the real workspace ID. Previously, when account_id was missing, user_id was used as a fallback and written
		// into the account_id field, which polluted the OAuth identity dedup key (the workspace ID decoded from the JWT and
		// the user-... stored in the database never matched), causing the same account to be imported twice (accounts mixed up in the pool).
		// When a custom header overrides the workspace ID, wham returns the ID of the overridden workspace,
		// so it must not be written back into the OAuth identity field (it would overwrite the real identity and keep clashing with the override value).
		identityAccountID := strings.TrimSpace(usage.AccountID)
		if account.AccountIDOverridden() {
			identityAccountID = ""
		}
		store.UpdateAccountIdentity(account, usage.Email, identityAccountID)
		store.UpdateAccountSubscriptionExpiresAt(account, usage.SubscriptionExpiresAt())
		// Measured: wham responses do not contain the subscription expiry field (the sync above basically gets no value), so the new date after renewal
		// has no source to query; but the server's authoritative paid plan_type proves the subscription is valid, so the old expiry that has already passed
		// is cleared, to avoid a long-lasting false "expired" report. (issue #360)
		if usage.PlanType != "" {
			store.ClearStaleSubscriptionExpiresAt(account)
		}
	}

	// Records the "manual reset count" (the remaining manual reset quota count on OpenAI's side).
	if usage.RateLimitResetCredits != nil {
		account.SetRateLimitResetCredits(usage.RateLimitResetCredits.AvailableCount)
		account.SetApplicableResetCredits(usage.RateLimitResetCredits.ApplicableAvailableCount)
	}

	// Records a snapshot of the credits balance (the wham credits object, zero quota cost).
	if usage.Credits != nil {
		var spendControlReached *bool
		if usage.SpendControl != nil {
			spendControlReached = &usage.SpendControl.Reached
		}
		var rateLimitReachedType string
		if usage.RateLimitReachedType != nil {
			rateLimitReachedType = usage.RateLimitReachedType.Type
		}
		// Goes through the store-persisted version: only wham can refresh credits, so if they stayed only in memory they would reset to zero after a restart.
		store.PersistCreditBalance(
			account,
			usage.Credits.Balance,
			usage.Credits.HasCredits,
			usage.Credits.Unlimited,
			usage.Credits.OverageLimitReached,
			spendControlReached,
			rateLimitReachedType,
		)
		// When the balance has just been topped up (it was 0 before while the account was already penalized by the usage window), it is put back into scheduling immediately,
		// without waiting for the window to reset. Cooldowns from non-local usage penalties are unaffected.
		store.ReleaseUsageWindowCooldownForCredits(account)
	}

	w5h, w7d := pickClassifiedWhamWindows(usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow, usage.PlanType, observedAt)
	// A present-but-empty window object is not evidence that the upstream
	// removed a window. Normalize those placeholders away so malformed/partial
	// payloads preserve the last known 5h snapshot.
	if !usableWhamWindow(w5h) {
		w5h = nil
	}
	if !usableWhamWindow(w7d) {
		w7d = nil
	}
	hasAuthoritativeWindow := w5h != nil || w7d != nil
	if hasAuthoritativeWindow {
		// Only a structurally valid window response is a successful usage probe;
		// malformed/empty payloads must not suppress the next retry.
		account.MarkResetCreditsProbed(observedAt)
	}
	usageApplied := false
	if hasAuthoritativeWindow {
		usageApplied = account.ApplyUsageObservation(observedAt, func() {
			if w5h != nil {
				resetAt := whamWindowResetAt(w5h, observedAt)
				account.SetUsageSnapshot5hAt(w5h.UsedPercent, resetAt, observedAt)
				result.UsagePct5h = w5h.UsedPercent
				result.Reset5hAt = resetAt
				result.HasUsage5h = true
				result.Used5hHeaders = true
				if store != nil {
					// At the 5h window reset time, arm a "probe on time" check; when the window renews, refresh the progress bar.
					store.WakeBoundaryProbe(resetAt)
				}
			} else if hasAuthoritativeWindow {
				// WHAM is a full authoritative probe: if the payload has no 5h window, the stale local 5h snapshot is cleared (issue #382).
				result.Cleared5h = store.ClearAbsentUsageSnapshot5hAt(account, observedAt)
			}

			if w7d != nil {
				resetAt := whamWindowResetAt(w7d, observedAt)
				account.SetReset7dAt(resetAt)
				account.SetWindow7dSeconds(w7d.LimitWindowSeconds)
				result.UsagePct7d = w7d.UsedPercent
				result.HasUsage7d = true
				if store != nil {
					store.WakeBoundaryProbe(resetAt)
					store.PersistUsageSnapshot(account, w7d.UsedPercent)
					if result.UsagePct7d >= 100 {
						result.Usage7dRateLimited = store.MarkUsage7dRateLimited(account)
					}
				}
			} else if result.Used5hHeaders && store != nil {
				// When there is only 5h data, persist the 5h snapshot on its own
				store.PersistUsageSnapshot5hOnly(account)
				result.Persisted5hOnly = true
			}
		})
	}

	// premium 5h rate-limit marker
	if usageApplied && result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 && !account.SkipsUsageWindowLimits() {
		if store != nil {
			result.Premium5hRateLimited = store.MarkPremium5hRateLimitedAt(account, result.Reset5hAt, observedAt)
		} else {
			result.Premium5hRateLimited = true
		}
	}

	// spark is an independent window: it writes an independent snapshot and never drives the account-level 5h rate limit.
	sparkWindow := pickSparkWhamWindow(usage.additionalRateLimits())
	sparkFresh := usageApplied
	if !hasAuthoritativeWindow && sparkWindow != nil {
		sparkFresh = account.ApplyUsageObservation(observedAt, func() {})
	}
	if sparkWindow != nil && sparkFresh {
		resetAt := whamWindowResetAt(sparkWindow, observedAt)
		account.SetUsageSnapshotSparkAt(sparkWindow.UsedPercent, resetAt, observedAt)
		if store != nil {
			store.PersistUsageSnapshotSpark(account)
			store.WakeBoundaryProbe(resetAt)
		}
	} else if hasAuthoritativeWindow && usageApplied {
		store.ClearAbsentUsageSnapshotSparkAt(account, observedAt)
	}

	return result
}

// Known window lengths (seconds), matching the limit_window_seconds values returned by upstream wham.
const (
	whamWindow5hSeconds int64 = 18_000
	whamWindow7dSeconds int64 = 604_800
)

// IsWhamLongWindowSeconds decides whether a window falls into the 7d slot: the weekly window of plus/pro, or the monthly window of plans such as team.
func IsWhamLongWindowSeconds(sec int64) bool {
	return sec == whamWindow7dSeconds || auth.IsMonthlyWindowSeconds(sec)
}

// pickClassifiedWhamWindows classifies the primary/secondary windows into the 5h/7d slots.
//
// Classification strategy:
//  1. First pass: exact match on limit_window_seconds (18000→5h, 604800→7d)
//  2. Second pass: put unknown windows whose free plan or reset clearly exceeds 5h into 7d
//  3. Last, fall back to field position (primary→5h, secondary→7d), filling only empty slots
//
// This correctly handles all of these at once:
//   - plus: primary=18000(5h) + secondary=604800(7d)
//   - free: primary=604800(7d) + secondary=null (the scenario reported in issue #168)
//   - field positions reversed
//   - Unknown window lengths that appear in the future (first prevent free/long-cycle windows from entering 5h, then avoid losing data entirely)
func pickClassifiedWhamWindows(primary, secondary *WhamUsageWindow, planType string, now time.Time) (w5h, w7d *WhamUsageWindow) {
	for _, w := range []*WhamUsageWindow{primary, secondary} {
		if w == nil {
			continue
		}
		switch {
		case w.LimitWindowSeconds == whamWindow5hSeconds:
			if w5h == nil {
				w5h = w
			}
		// Long window slot (7d): plus/pro is the weekly window (604800), the team plan is the monthly window (about 2592000, a 28–31 day tolerance).
		case w.LimitWindowSeconds == whamWindow7dSeconds || auth.IsMonthlyWindowSeconds(w.LimitWindowSeconds):
			if w7d == nil {
				w7d = w
			}
		}
	}

	if w5h == nil && primary != nil && primary != w7d {
		if shouldTreatUnknownWhamWindowAs7d(primary, planType, now) && w7d == nil {
			w7d = primary
		} else {
			w5h = primary
		}
	}
	if w7d == nil && secondary != nil && secondary != w5h {
		if shouldTreatUnknownWhamWindowAs7d(secondary, planType, now) {
			w7d = secondary
		} else if w5h == nil {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}
	return w5h, w7d
}

// usableWhamWindow reports whether a decoded window contains enough positive
// information to be authoritative. JSON may contain an empty placeholder for
// an omitted optional window; treating that as a real 5h window would erase a
// valid snapshot on the next probe.
func usableWhamWindow(w *WhamUsageWindow) bool {
	if w == nil || math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent > 100 {
		return false
	}
	if w.LimitWindowSeconds > 0 || w.ResetAfterSeconds > 0 || w.ResetAt > 0 {
		return true
	}
	// used_percent without a window duration/reset is a partial payload, not
	// authoritative evidence about which optional window is present.
	return false
}

func shouldTreatUnknownWhamWindowAs7d(w *WhamUsageWindow, planType string, now time.Time) bool {
	if w == nil {
		return false
	}
	if auth.NormalizePlanType(planType) == "free" {
		return true
	}
	if w.ResetAfterSeconds > whamWindow5hSeconds {
		return true
	}
	if w.ResetAt > 0 && !now.IsZero() && time.Unix(w.ResetAt, 0).After(now.Add(time.Duration(whamWindow5hSeconds)*time.Second)) {
		return true
	}
	return false
}

func whamWindowResetAt(w *WhamUsageWindow, now time.Time) time.Time {
	if w == nil {
		return time.Time{}
	}
	// reset_at is a unix timestamp (seconds) and is preferred; when missing, it falls back to reset_after_seconds
	if w.ResetAt > 0 {
		return time.Unix(w.ResetAt, 0)
	}
	if w.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}
