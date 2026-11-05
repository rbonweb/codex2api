package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/codex2api/auth"
)

// WhamDailyTokenBreakdownURL is the ChatGPT backend endpoint that splits usage by day, by model × speed.
// It complements daily-workspace-usage-counts: counts gives no cost per model (credits are always 0),
// This endpoint gives, for each (model, speed) and for each product entry point (cli / desktop_app / …), the
// relative share. Joined by date, the two let you apportion each day's official credits across models and speed tiers.
//
// Key semantics (measured on 2026-09-06 with a plus account and a free account): the response units are "percent", but this
// percentage is **not** the share of the weekly quota. It is normalized by "the requested range + the peak bucket within the group":
// the day with the highest consumption in the range is always 100.0, and the others are scaled proportionally. Querying two different ranges for the same account,
// the values for the same day differ. So these numbers may only be used as shares within a single day (model A's share of that day's
// total), must never be summed across days, and must not be used to back out the size of the quota.
const WhamDailyTokenBreakdownURL = "https://chatgpt.com/backend-api/wham/usage/daily-token-usage-breakdown"

// WhamSpeedStandard / WhamSpeedFast are the two speed field values observed in measurements.
// Free accounts only have standard rows; plus accounts have both tiers, and fast corresponds to the priority tier of Responses.
const (
	WhamSpeedStandard = "standard"
	WhamSpeedFast     = "fast"
)

// whamDailyTokenBreakdownURLForTest lets tests replace the default URL. Production code must not assign it.
var whamDailyTokenBreakdownURLForTest = ""

// SetWhamDailyTokenBreakdownURLForTest lets tests replace the breakdown endpoint URL and returns a restore function.
// Production code must not call it.
func SetWhamDailyTokenBreakdownURLForTest(u string) (restore func()) {
	old := whamDailyTokenBreakdownURLForTest
	whamDailyTokenBreakdownURLForTest = u
	return func() { whamDailyTokenBreakdownURLForTest = old }
}

// WhamDailyTokenBreakdownResponse is the response structure of daily-token-usage-breakdown.
type WhamDailyTokenBreakdownResponse struct {
	Data []WhamDailyTokenBreakdownDay `json:"data"`
	// Units is always "percent" in measurements. It is kept so tests and logs can detect when the upstream changes its definition.
	Units   string `json:"units"`
	GroupBy string `json:"group_by"`
}

// WhamDailyTokenBreakdownDay is one day's breakdown.
//
// Unlike counts, this endpoint is **dense**: every day in the requested range has a row, and days with no usage
// are all zero, and even when end_date falls in the future, future dates also appear with all zeros. The models list is the set of
// (model, speed) catalog entries that appeared in the range, as a union, repeated each day, with credits=0 on unused rows.
type WhamDailyTokenBreakdownDay struct {
	Date   string                         `json:"date"`
	Models []WhamDailyTokenBreakdownModel `json:"models"`
	// Surfaces is the share by product entry point; measured, there are always 17 keys (cli / vscode / web / work_web /
	// mobile / work_mobile / slack / linear / jetbrains / sdk / exec / github /
	// desktop_app / work_desktop / github_code_review / agent_identity / unknown),
	// matching counts.clients client_id one-to-one by name (cli↔CODEX_CLI, etc.).
	// Each day, sum(Surfaces) == sum(Models.Percent); the two are two ways of splitting the same total.
	Surfaces map[string]float64 `json:"product_surface_usage_values"`
}

// WhamDailyTokenBreakdownModel is the share of one (model, speed).
//
// The upstream field is named credits, but with units=percent it is a normalized percentage rather than a credits amount,
// so it is renamed to Percent here to avoid confusion with the real credits in counts.
type WhamDailyTokenBreakdownModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Percent float64 `json:"credits"`
}

// TotalPercent returns the sum of the shares of all (model, speed) entries for the day, i.e. that day's total on this response's scale.
// Using it as the denominator turns each model into its share within the day (0~1).
func (d WhamDailyTokenBreakdownDay) TotalPercent() float64 {
	total := 0.0
	for _, m := range d.Models {
		if m.Percent > 0 {
			total += m.Percent
		}
	}
	return total
}

// ActiveModels returns the (model, speed) rows with a share above 0 for the day, filling in standard when speed is missing,
// sorted by share in descending order, with ties sorted by name, so the persisted JSON is stable.
func (d WhamDailyTokenBreakdownDay) ActiveModels() []WhamDailyTokenBreakdownModel {
	out := make([]WhamDailyTokenBreakdownModel, 0, len(d.Models))
	for _, m := range d.Models {
		if m.Percent <= 0 {
			continue
		}
		model := strings.TrimSpace(m.Model)
		if model == "" {
			continue
		}
		speed := strings.ToLower(strings.TrimSpace(m.Speed))
		if speed == "" {
			speed = WhamSpeedStandard
		}
		out = append(out, WhamDailyTokenBreakdownModel{Model: model, Speed: speed, Percent: m.Percent})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Percent != out[j].Percent {
			return out[i].Percent > out[j].Percent
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Speed < out[j].Speed
	})
	return out
}

// ActiveSurfaces returns the product entry points with a share above 0 for the day.
func (d WhamDailyTokenBreakdownDay) ActiveSurfaces() map[string]float64 {
	out := make(map[string]float64, len(d.Surfaces))
	for key, value := range d.Surfaces {
		name := strings.TrimSpace(key)
		if name == "" || value <= 0 {
			continue
		}
		out[name] = value
	}
	return out
}

// QueryWhamDailyTokenBreakdown fetches the per-day model × speed breakdown for the [startDate, endDate] range,
// with dates in YYYY-MM-DD format. It costs no quota, and its request shape matches QueryWhamDailyUsage.
// On a non-200 response it returns resp (body unread) for the caller to handle.
func QueryWhamDailyTokenBreakdown(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*WhamDailyTokenBreakdownResponse, *http.Response, error) {
	base := WhamDailyTokenBreakdownURL
	if whamDailyTokenBreakdownURLForTest != "" {
		base = whamDailyTokenBreakdownURLForTest
	}
	return queryWhamDailyTokenBreakdownWithURL(ctx, account, proxyURL, base, startDate, endDate)
}

func queryWhamDailyTokenBreakdownWithURL(ctx context.Context, account *auth.Account, proxyURL, base, startDate, endDate string) (*WhamDailyTokenBreakdownResponse, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}
	if startDate == "" || endDate == "" {
		return nil, nil, fmt.Errorf("token breakdown requires start_date and end_date")
	}

	query := url.Values{}
	query.Set("start_date", startDate)
	query.Set("end_date", endDate)
	// Without group_by the upstream also returns by day; it is set explicitly so a change in the default does not affect it.
	// Do not use group_by=week: week buckets start on Sunday and are normalized separately, unrelated to the account's reset period.
	query.Set("group_by", "day")
	requestURL := base + "?" + query.Encode()

	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, requestURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build token breakdown request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if err := applyCodexMaintenanceHeaders(req, account); err != nil {
		return nil, nil, err
	}
	// As with counts: when a custom header overrides the workspace ID, query the overridden workspace. Personal accounts have an empty account_id,
	// and the upstream returns normally without this header too.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("token breakdown request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("token breakdown returned status %d", resp.StatusCode)
	}

	// Dense rows × 14 model catalog entries × 17 entry-point keys: measured, a plus account's 47 days is about 100KB (pretty-printed
	// JSON); a deep backfill of 84 days is about 200KB. The 4MB limit leaves ample headroom.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read token breakdown response: %w", err)
	}

	var out WhamDailyTokenBreakdownResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp, fmt.Errorf("parse token breakdown response: %w", err)
	}
	return &out, resp, nil
}
