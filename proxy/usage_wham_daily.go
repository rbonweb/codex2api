package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// WhamDailyUsageURL is the ChatGPT backend endpoint that returns per-day workspace usage statistics.
// Like wham/usage, it costs no quota, but it gives absolute values on the official settlement basis (credits and token counts),
// and can split them by client (CLI / Desktop / IDE / gateway traffic) and by model.
//
// The upstream lookback depth has been measured at at least 84 days (on 2026-09-06, a plus account returned data 12 weeks back; at the first integration in 2026-08,
// only 7 days came back). Steady-state sync still backfills only the most recent WhamDailyUsageRetentionDays days, as a rolling window,
// The first sync (when there is no local snapshot at all) does one deep backfill over WhamDailyUsageBackfillDays;
// older history still exists only in the local database.
const WhamDailyUsageURL = "https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts"

// WhamDailyUsageRetentionDays is the rolling window that each steady-state sync round backfills (including today). Today's and the previous few days'
// values change as they settle, so each round overwrites the whole window and fixes any gaps from missed runs along the way. The name "retention period" is kept for historical reasons:
// in 2026-08, at first integration, the upstream really only returned 7 days.
const WhamDailyUsageRetentionDays = 7

// WhamDailyUsageBackfillDays is the deep backfill window for the first sync. Measured on 2026-09-06, a plus account could query back
// 84 days before the capture (exactly 12 weeks, starting on a Sunday, which looks like a "last 12 weeks" truncation); whether older data can be queried
// is unverified, so the verified depth is used.
const WhamDailyUsageBackfillDays = 84

// WhamCreditsPerUSD is the official conversion rate between credits and US dollars (1 USD = 25 credits).
// Measured on 2026-09-06: for a single model on a settled day, token × official catalog price equals exactly credits/25,
// meaning credits are the API-equivalent cost at the catalog price of that time.
const WhamCreditsPerUSD = 25.0

// whamDailyUsageURLForTest lets tests replace the default URL. Production code must not assign it.
var whamDailyUsageURLForTest = ""

// SetWhamDailyUsageURLForTest lets tests replace the per-day usage endpoint URL and returns a restore function.
// Production code must not call it.
func SetWhamDailyUsageURLForTest(u string) (restore func()) {
	old := whamDailyUsageURLForTest
	whamDailyUsageURLForTest = u
	return func() { whamDailyUsageURLForTest = old }
}

// WhamDailyUsageResponse is the response structure of daily-workspace-usage-counts.
type WhamDailyUsageResponse struct {
	Data    []WhamDailyUsageDay `json:"data"`
	GroupBy string              `json:"group_by"`
}

// WhamDailyUsageDay is a single statistics period (one day when group_by=day, the week's start date when week).
// This endpoint is sparse: days with no usage return no row.
type WhamDailyUsageDay struct {
	Date    string                 `json:"date"`
	Totals  WhamDailyUsageCounts   `json:"totals"`
	Clients []WhamDailyUsageClient `json:"clients"`
	Models  []WhamDailyUsageModel  `json:"models"`
}

// SettledOn reports whether this day's record is settled: the date is earlier than today (UTC date, YYYY-MM-DD) and
// it has token details. The upstream has changed the shape of the "today" row (see WhamDailyUsageCounts), so judging by fields
// being present is no longer reliable; judging by date is stable: today's values are still changing, while yesterday and earlier are overwritten
// during the next whole-window backfill.
func (d WhamDailyUsageDay) SettledOn(today string) bool {
	date := strings.TrimSpace(d.Date)
	return date != "" && date < strings.TrimSpace(today) && d.Totals.HasTokenDetail()
}

// WhamDailyUsageCounts is a set of usage counts.
//
// The shape of the current day's (unsettled) record has been changed by upstream: in 2026-08 it had only users/threads/turns/credits,
// with all four token fields missing; in 2026-09 it was the reverse: tokens and credits were present, and the missing thing was the models
// key, with users/threads/turns all 0. The token fields keep pointers to tell "upstream did not provide it" apart from "provided 0",
// but "whether settled" can no longer be judged by field presence; see WhamDailyUsageDay.SettledOn.
type WhamDailyUsageCounts struct {
	Users   int     `json:"users"`
	Threads int     `json:"threads"`
	Turns   int     `json:"turns"`
	Credits float64 `json:"credits"`

	UncachedTextInputTokens *int64 `json:"uncached_text_input_tokens,omitempty"`
	CachedTextInputTokens   *int64 `json:"cached_text_input_tokens,omitempty"`
	TextOutputTokens        *int64 `json:"text_output_tokens,omitempty"`
	TextTotalTokens         *int64 `json:"text_total_tokens,omitempty"`
}

// HasTokenDetail reports that this record carries the token detail fields. It only says the fields exist, not that the record is settled:
// today's record now also carries tokens and changes all day, so settlement is decided by WhamDailyUsageDay.SettledOn.
func (c WhamDailyUsageCounts) HasTokenDetail() bool {
	return c.TextTotalTokens != nil
}

// USD converts this record's cost at the official rate of 1 USD = 25 credits.
func (c WhamDailyUsageCounts) USD() float64 {
	return c.Credits / WhamCreditsPerUSD
}

// WhamDailyUsageClient is usage split by client entry point. Measured client_id values include
// CODEX_CLI / CODEX_DESKTOP_APP / CODEX_IDE_VSCODE / CODEX_WORK_DESKTOP /
// CODEX_SERVICE_EXEC / CODEX_SDK_TS / CODEX_WEB, and the one without an official client identifier,
// CODEX_UNKNOWN_DEFAULT (traffic through this gateway lands in this bucket).
type WhamDailyUsageClient struct {
	ClientID string `json:"client_id"`
	WhamDailyUsageCounts
}

// WhamDailyUsageModel is usage split by model. Measured: the upstream returns no token details at this level,
// credits are also always 0, and only users/threads/turns have values. Per-model cost must use
// the share apportionment from daily-token-usage-breakdown (see usage_wham_breakdown.go).
type WhamDailyUsageModel struct {
	Model string `json:"model"`
	WhamDailyUsageCounts
}

// QueryWhamDailyUsage fetches the per-day usage statistics for the [startDate, endDate] range, with dates in
// YYYY-MM-DD format. It costs no quota and uses the same request shape as QueryWhamUsage.
// On a non-200 response it returns resp (body unread) for the caller to handle.
func QueryWhamDailyUsage(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*WhamDailyUsageResponse, *http.Response, error) {
	base := WhamDailyUsageURL
	if whamDailyUsageURLForTest != "" {
		base = whamDailyUsageURLForTest
	}
	return queryWhamDailyUsageWithURL(ctx, account, proxyURL, base, startDate, endDate)
}

// WhamDailyUsageWindow returns the rolling range for steady-state sync (including today, UTC dates).
func WhamDailyUsageWindow(now time.Time) (startDate, endDate string) {
	return whamDailyUsageWindowDays(now, WhamDailyUsageRetentionDays)
}

// WhamDailyUsageBackfillWindow returns the deep backfill range for the first sync (including today, UTC dates).
func WhamDailyUsageBackfillWindow(now time.Time) (startDate, endDate string) {
	return whamDailyUsageWindowDays(now, WhamDailyUsageBackfillDays)
}

func whamDailyUsageWindowDays(now time.Time, days int) (startDate, endDate string) {
	if days <= 0 {
		days = 1
	}
	end := now.UTC()
	start := end.AddDate(0, 0, -(days - 1))
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func queryWhamDailyUsageWithURL(ctx context.Context, account *auth.Account, proxyURL, base, startDate, endDate string) (*WhamDailyUsageResponse, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}
	if startDate == "" || endDate == "" {
		return nil, nil, fmt.Errorf("daily usage requires start_date and end_date")
	}

	query := url.Values{}
	query.Set("start_date", startDate)
	query.Set("end_date", endDate)
	query.Set("group_by", "day")
	query.Set("workspace_user", "true")
	requestURL := base + "?" + query.Encode()

	// resinMaintenanceTarget rebuilds the target via RequestURI(), so the query is preserved.
	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, requestURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build daily usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if err := applyCodexMaintenanceHeaders(req, account); err != nil {
		return nil, nil, err
	}
	// As with the wham usage query: when a custom header overrides the workspace ID, the statistics must query the overridden workspace,
	// otherwise the data returned is for a different workspace than the actual traffic.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("daily usage request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("daily usage returned status %d", resp.StatusCode)
	}

	// Sparse rows × client and model split: measured, a plus account's 84 days is about 77KB; the 1MB limit leaves ample headroom.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read daily usage response: %w", err)
	}

	var out WhamDailyUsageResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp, fmt.Errorf("parse daily usage response: %w", err)
	}
	return &out, resp, nil
}
