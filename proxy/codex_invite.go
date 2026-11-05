package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/auth"
)

// Referral invite endpoints. The upstream has taken the old paths under the wham/ prefix offline (GET returns 404 Not Found),
// the three endpoints now sit under /backend-api/referrals/invite:
//   - POST  ""            send an invite; required: program_id / entrypoint / emails
//   - GET   /eligibility  eligibility gate + remaining quota + official copy
//   - GET   /tracking     list of sent invites and redemption status
const (
	codexInviteBaseURL = "https://chatgpt.com/backend-api/referrals/invite"

	// CodexInviteURL is the endpoint for sending invites.
	CodexInviteURL = codexInviteBaseURL
	// CodexInviteEligibilityURL is the endpoint for querying invite eligibility and quota.
	CodexInviteEligibilityURL = codexInviteBaseURL + "/eligibility"
	// CodexInviteTrackingURL is the endpoint for tracking sent invites.
	CodexInviteTrackingURL = codexInviteBaseURL + "/tracking"
)

// Invite program identifiers. The upstream split the old single referral_key field into two fields: program_id and entrypoint.
const (
	// DefaultProgramID is the referral program for the Codex consumer edition.
	DefaultProgramID = "codex_referral_consumer"
	// DefaultEntrypoint is the standing invite entry point (corresponds to the old persistent invite).
	DefaultEntrypoint = "persistent"
)

// Default query window and item count for the tracking list, matching the official client.
const (
	defaultTrackingPeriod = "past_90_days"
	defaultTrackingLimit  = 100
)

// The invite upstream uses browser-style headers (unlike the codex_cli_rs probe), which is closer to calls from the official web client.
// Testing shows the upstream accepts requests without any cookie (including _devicecheck).
const (
	inviteOriginator = "Codex Desktop"
	inviteLanguage   = "zh-CN"
	inviteUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
)

// Endpoint overrides for tests. Production code must not assign to these.
var (
	codexInviteURLForTest            = ""
	codexInviteEligibilityURLForTest = ""
	codexInviteTrackingURLForTest    = ""
)

// CodexInviteResult is the structured result of one invite call.
type CodexInviteResult struct {
	OK         bool              `json:"ok"`
	StatusCode int               `json:"status_code"`
	RequestID  string            `json:"request_id,omitempty"`
	ProgramID  string            `json:"program_id"`
	Entrypoint string            `json:"entrypoint"`
	Emails     []string          `json:"emails"`
	Invites    []CodexInviteItem `json:"invites,omitempty"`
	// UpstreamMessage is the human-readable reason from the upstream detail, and FailedEmails are the recipients that were rejected.
	// For example, "this person has already received a referral invite". This has nothing to do with account eligibility; it only means this recipient is not allowed,
	// so it must not be reported as "account is not eligible for invites" just because of a 403.
	UpstreamMessage string   `json:"upstream_message,omitempty"`
	FailedEmails    []string `json:"failed_emails,omitempty"`
	// Challenged is true when Cloudflare intercepted the request with a challenge. It is not a business decision from the upstream, so callers should ask the user to retry,
	// and must not read it from StatusCode as "not eligible".
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// CodexInviteItem is one invite record returned by the upstream.
type CodexInviteItem struct {
	ReferralID string `json:"referral_id,omitempty"`
	Email      string `json:"email,omitempty"`
	InviteURL  string `json:"invite_url,omitempty"`
}

// CodexInviteGrant is one reward entry. One invite usually produces one entry each for the inviter and the invitee.
// grant_type is currently observed to be personal_credits; the rate_limit_* fields only have values under rate-limit-reset plans.
type CodexInviteGrant struct {
	Recipient string  `json:"recipient,omitempty"`
	GrantType string  `json:"grant_type,omitempty"`
	Amount    float64 `json:"amount,omitempty"`
	RewardID  string  `json:"reward_id,omitempty"`
}

// CodexInviteTimeFrameRule is one quota rule. capacity_type distinguishes two independent limits:
// send is the cap on the number of sends, and reward is the cap on the number of invites that can earn a reward (the latter is usually much smaller than the former).
type CodexInviteTimeFrameRule struct {
	InvitesSent  int    `json:"invites_sent"`
	InvitesTotal int    `json:"invites_total"`
	TimeFrame    string `json:"time_frame,omitempty"`
	Type         string `json:"type,omitempty"`
	CapacityType string `json:"capacity_type,omitempty"`
}

// CodexInviteEligibility is the result of querying invite eligibility and quota.
// RemainingSendCapacity / RemainingRewardCapacity use pointers to tell apart "the upstream explicitly says 0" and
// "the upstream did not send this field". The former must block sending; the latter can only allow it.
type CodexInviteEligibility struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	RequestID  string `json:"request_id,omitempty"`

	ShouldShow           bool   `json:"should_show"`
	IneligibleReason     string `json:"ineligible_reason,omitempty"`
	IneligibleReasonCode string `json:"ineligible_reason_code,omitempty"`
	ProgramID            string `json:"program_id,omitempty"`
	Entrypoint           string `json:"entrypoint,omitempty"`
	OfferID              string `json:"offer_id,omitempty"`

	Grants                  []CodexInviteGrant `json:"grants,omitempty"`
	RemainingSendCapacity   *int               `json:"remaining_send_capacity,omitempty"`
	RemainingRewardCapacity *int               `json:"remaining_reward_capacity,omitempty"`

	Title          string                     `json:"title,omitempty"`
	Description    string                     `json:"description,omitempty"`
	Rules          []string                   `json:"rules,omitempty"`
	TimeFrameRules []CodexInviteTimeFrameRule `json:"time_frame_rules,omitempty"`

	RequiresExplicitConfirmation bool `json:"requires_explicit_confirmation,omitempty"`

	// UpstreamMessage: see CodexInviteResult.UpstreamMessage.
	UpstreamMessage string `json:"upstream_message,omitempty"`
	// Challenged: see CodexInviteResult.Challenged.
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// CodexInviteTrackingItem is one sent-invite record.
type CodexInviteTrackingItem struct {
	ReferralID        string             `json:"referral_id,omitempty"`
	Email             string             `json:"email,omitempty"`
	Status            string             `json:"status,omitempty"`
	CanResend         bool               `json:"can_resend"`
	InviteURL         string             `json:"invite_url,omitempty"`
	ResendAvailableAt string             `json:"resend_available_at,omitempty"`
	Grants            []CodexInviteGrant `json:"grants,omitempty"`
	CreatedAt         string             `json:"created_at,omitempty"`
	ExpiresAt         string             `json:"expires_at,omitempty"`
}

// CodexInviteTracking is the result of querying the sent-invite list.
type CodexInviteTracking struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	RequestID  string `json:"request_id,omitempty"`

	Items  []CodexInviteTrackingItem `json:"items"`
	Cursor string                    `json:"cursor,omitempty"`

	// UpstreamMessage: see CodexInviteResult.UpstreamMessage.
	UpstreamMessage string `json:"upstream_message,omitempty"`
	// Challenged: see CodexInviteResult.Challenged.
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// parseInviteFailureDetail parses the detail field in an upstream failure response.
//
// This field is polymorphic; both forms have been observed in testing:
//   - string: {"detail":"Referral invites are not available for your plan"}  account-level ineligibility
//   - object: {"detail":{"message":"this person has already received a referral invite","failed_emails":["x@y"]}}  recipient-level rejection
//
// The latter has nothing to do with account eligibility. The reason and the failed emails must be surfaced, otherwise callers see only a 403,
// and would report "this recipient is not allowed" as "this account cannot send invites".
func parseInviteFailureDetail(body []byte) (string, []string) {
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Detail) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(envelope.Detail, &asString); err == nil {
		return asString, nil
	}
	var asObject struct {
		Message      string   `json:"message"`
		FailedEmails []string `json:"failed_emails"`
	}
	if err := json.Unmarshal(envelope.Detail, &asObject); err == nil {
		return asObject.Message, asObject.FailedEmails
	}
	return "", nil
}

// normalizeInviteProgram fills empty program identifiers with the defaults.
func normalizeInviteProgram(programID, entrypoint string) (string, string) {
	if strings.TrimSpace(programID) == "" {
		programID = DefaultProgramID
	}
	if strings.TrimSpace(entrypoint) == "" {
		entrypoint = DefaultEntrypoint
	}
	return programID, entrypoint
}

// NormalizeInviteProgram is the exported version of normalizeInviteProgram, for callers to use before sending a
// request to compute the program identifier that actually applies. Cache keys must be built from the normalized value, otherwise "empty parameter"
// and "explicitly passing the default" each store a separate entry with the same content.
func NormalizeInviteProgram(programID, entrypoint string) (string, string) {
	return normalizeInviteProgram(programID, entrypoint)
}

// normalizeInviteTracking fills the tracking query's window and count with the official client's defaults.
func normalizeInviteTracking(period string, limit int) (string, int) {
	if strings.TrimSpace(period) == "" {
		period = defaultTrackingPeriod
	}
	if limit <= 0 || limit > defaultTrackingLimit {
		limit = defaultTrackingLimit
	}
	return period, limit
}

// NormalizeInviteTracking is the exported version of normalizeInviteTracking. The reason is the same as
// NormalizeInviteProgram: limit=0 and limit=100 return the same data, so the cache keys must match.
func NormalizeInviteTracking(period string, limit int) (string, int) {
	return normalizeInviteTracking(period, limit)
}

// newInviteRequest builds a request with the invite-specific headers. It sends GET when body is nil.
func newInviteRequest(ctx context.Context, account *auth.Account, method, endpoint string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("build invite request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+account.GetAccessToken())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Oai-Language", inviteLanguage)
	req.Header.Set("Originator", inviteOriginator)
	req.Header.Set("User-Agent", inviteUserAgent)
	// Invite operations act on the workspace after custom header overrides, matching where the real traffic points.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	return req, nil
}

// inviteCookieJars caches a cookie jar per account. These endpoints go through Cloudflare bot management,
// so requests without cookies always start cold. In testing, about half were sent to a managed challenge (403 + challenge page).
// Reusing __cf_bm / _cfuvid from the last successful response significantly lowers the chance of a challenge. Jars are isolated per account instead of shared globally,
// so one account's session cookie (oai-sc) does not leak into another account.
var inviteCookieJars sync.Map

// inviteCookieJarFor returns the account's cookie jar, creating it if missing.
func inviteCookieJarFor(account *auth.Account) http.CookieJar {
	key := fmt.Sprintf("%d|%s", account.DBID, account.EffectiveAccountID())
	if cached, ok := inviteCookieJars.Load(key); ok {
		return cached.(http.CookieJar)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	actual, _ := inviteCookieJars.LoadOrStore(key, http.CookieJar(jar))
	return actual.(http.CookieJar)
}

// cloudflareChallengeMarker replaces the challenge page body so that tens of KB of HTML are not returned to the frontend.
const cloudflareChallengeMarker = "cloudflare managed challenge (html challenge page omitted)"

// isCloudflareChallenge reports whether a response is a Cloudflare challenge page rather than an upstream business response.
// Challenge pages reuse 403/429/503 status codes, so reading the status code directly would mistake "intercepted" for "not eligible".
func isCloudflareChallenge(status int, body []byte) bool {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return false
	}
	// First rule out business JSON by checking whether the body is HTML, so a normal {"detail":...} is not scanned in full.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return false
	}
	// The signature marker must be matched across the whole body: the challenge page puts inline SVG icons before the script, and in testing cf_chl_opt
	// appears after 7KB, so scanning only the start would miss it (the body is already capped at 1MB by LimitReader).
	for _, marker := range [][]byte{
		[]byte("cf_chl_opt"),
		[]byte("challenge-platform"),
		[]byte("Enable JavaScript and cookies to continue"),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

// inviteHTTPClient returns the Client used for invite requests.
//
// These endpoints are web-client /backend-api paths behind Cloudflare bot management: with Go's default TLS fingerprint,
// direct connections are randomly sent to a managed challenge (consecutive requests from one account bounce between 200 and a 403 challenge page),
// so, like subscription sync, this forces the uTLS Chrome fingerprint. The transport comes from the pool, which holds the connections; unlike
// one-off uTLS instances, it does not leak h2 connections, and the jar is attached per account so __cf_bm can be reused across requests.
// useTestTransport is for httptest (plain HTTP), since uTLS cannot dial a plaintext port.
func inviteHTTPClient(account *auth.Account, proxyURL string, useTestTransport bool) *http.Client {
	jar := inviteCookieJarFor(account)
	if useTestTransport {
		return &http.Client{Transport: newCodexStandardTransport(proxyURL), Jar: jar}
	}
	pooled := getMaintenanceClient(account, proxyURL, maintenancePurposeInvite, true)
	return &http.Client{Transport: pooled.Transport, Jar: jar}
}

// doInviteRequest executes the request and reads back the status code, request id, response body, and whether Cloudflare challenged it.
func doInviteRequest(req *http.Request, account *auth.Account, proxyURL string, useTestTransport bool) (int, string, []byte, bool, error) {
	client := inviteHTTPClient(account, proxyURL, useTestTransport)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, false, fmt.Errorf("invite request: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("x-oai-request-id"), body, isCloudflareChallenge(resp.StatusCode, body), nil
}

// inviteAccountReady checks that the account has the credentials needed to send invites.
func inviteAccountReady(account *auth.Account) error {
	if account == nil {
		return fmt.Errorf("account is nil")
	}
	if account.GetAccessToken() == "" {
		return fmt.Errorf("account has no access token")
	}
	return nil
}

// SendCodexInvite sends invite emails to the ChatGPT referral invite endpoint using the account's credentials.
// When programID / entrypoint are empty, the default consumer standing entry point is used. Returns a structured result;
// on non-2xx, OK=false and Upstream/UpstreamRaw keep the upstream response, and the caller handles it based on StatusCode.
func SendCodexInvite(ctx context.Context, account *auth.Account, proxyURL, programID, entrypoint string, emails []string) (*CodexInviteResult, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	if len(emails) == 0 {
		return nil, fmt.Errorf("no emails to invite")
	}
	programID, entrypoint = normalizeInviteProgram(programID, entrypoint)

	endpoint := CodexInviteURL
	useTestTransport := codexInviteURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteURLForTest
	}

	payload, _ := json.Marshal(map[string]any{
		"program_id": programID,
		"entrypoint": entrypoint,
		"emails":     emails,
	})

	req, err := newInviteRequest(ctx, account, http.MethodPost, endpoint, payload)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteResult{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		ProgramID:  programID,
		Entrypoint: entrypoint,
		Emails:     emails,
		Challenged: challenged,
	}

	// When Cloudflare challenges the request, the body is challenge-page HTML with no upstream meaning, so drop it and keep only the marker.
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}

	// Upstream response: if it parses as JSON, put it in Upstream and extract the invite details; otherwise keep the raw string.
	// The detail field name differs between the two endpoints (invites[] for sending, items[] for tracking), so accept both.
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, result.FailedEmails = parseInviteFailureDetail(body)
		}
		var parsed struct {
			Invites []CodexInviteItem `json:"invites"`
			Items   []CodexInviteItem `json:"items"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			if len(parsed.Invites) > 0 {
				result.Invites = parsed.Invites
			} else {
				result.Invites = parsed.Items
			}
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}

// QueryCodexInviteEligibility queries the account's eligibility and remaining quota under the given referral program.
func QueryCodexInviteEligibility(ctx context.Context, account *auth.Account, proxyURL, programID, entrypoint string) (*CodexInviteEligibility, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	programID, entrypoint = normalizeInviteProgram(programID, entrypoint)

	endpoint := CodexInviteEligibilityURL
	useTestTransport := codexInviteEligibilityURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteEligibilityURLForTest
	}
	query := url.Values{}
	query.Set("program_id", programID)
	query.Set("entrypoint", entrypoint)

	req, err := newInviteRequest(ctx, account, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteEligibility{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		ProgramID:  programID,
		Entrypoint: entrypoint,
		Challenged: challenged,
	}
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, _ = parseInviteFailureDetail(body)
		}
		// Parse into a temporary struct and then copy, so that a missing program_id/entrypoint from the upstream does not overwrite the request parameters with empty values.
		var parsed struct {
			ShouldShow           bool                       `json:"should_show"`
			IneligibleReason     string                     `json:"ineligible_reason"`
			IneligibleReasonCode string                     `json:"ineligible_reason_code"`
			ProgramID            string                     `json:"program_id"`
			Entrypoint           string                     `json:"entrypoint"`
			OfferID              string                     `json:"offer_id"`
			Grants               []CodexInviteGrant         `json:"grants"`
			RemainingSend        *int                       `json:"remaining_send_capacity"`
			RemainingReward      *int                       `json:"remaining_reward_capacity"`
			Title                string                     `json:"title"`
			Description          string                     `json:"description"`
			Rules                []string                   `json:"rules"`
			TimeFrameRules       []CodexInviteTimeFrameRule `json:"time_frame_rules"`
			RequiresConfirm      bool                       `json:"requires_explicit_confirmation"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			result.ShouldShow = parsed.ShouldShow
			result.IneligibleReason = parsed.IneligibleReason
			result.IneligibleReasonCode = parsed.IneligibleReasonCode
			if parsed.ProgramID != "" {
				result.ProgramID = parsed.ProgramID
			}
			if parsed.Entrypoint != "" {
				result.Entrypoint = parsed.Entrypoint
			}
			result.OfferID = parsed.OfferID
			result.Grants = parsed.Grants
			result.RemainingSendCapacity = parsed.RemainingSend
			result.RemainingRewardCapacity = parsed.RemainingReward
			result.Title = parsed.Title
			result.Description = parsed.Description
			result.Rules = parsed.Rules
			result.TimeFrameRules = parsed.TimeFrameRules
			result.RequiresExplicitConfirmation = parsed.RequiresConfirm
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}

// QueryCodexInviteTracking queries the invites the account has sent and their redemption status.
// When period / limit is empty or not positive, the official client defaults are used (past_90_days / 100).
func QueryCodexInviteTracking(ctx context.Context, account *auth.Account, proxyURL, programID, period string, limit int) (*CodexInviteTracking, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	programID, _ = normalizeInviteProgram(programID, "")
	period, limit = normalizeInviteTracking(period, limit)

	endpoint := CodexInviteTrackingURL
	useTestTransport := codexInviteTrackingURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteTrackingURLForTest
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("period", period)
	query.Set("program_id", programID)

	req, err := newInviteRequest(ctx, account, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteTracking{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		Items:      []CodexInviteTrackingItem{},
		Challenged: challenged,
	}
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, _ = parseInviteFailureDetail(body)
		}
		var parsed struct {
			Items   []CodexInviteTrackingItem `json:"items"`
			Invites []CodexInviteTrackingItem `json:"invites"`
			Cursor  string                    `json:"cursor"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			if len(parsed.Items) > 0 {
				result.Items = parsed.Items
			} else if len(parsed.Invites) > 0 {
				result.Items = parsed.Invites
			}
			result.Cursor = parsed.Cursor
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}
