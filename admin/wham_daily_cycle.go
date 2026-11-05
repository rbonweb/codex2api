package admin

import (
	"context"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// whamDailyCycleReliablePercent is the minimum used percentage at which the quota estimate is considered reliable.
// Live percentages are integers, so ±0.5% rounding is already a range of about ±5% at 10%; lower than that it has no reference value.
const whamDailyCycleReliablePercent = 10

// whamDailyCycleMinPercent is the lowest percentage that can be back-calculated: below 0.5% the rounded lower bound is 0, so division is impossible.
const whamDailyCycleMinPercent = 0.5

// whamDailyCycleInput holds the account window snapshot and this cycle's official cost, as needed for the estimate.
//
// Neither upstream data source gives an absolute weekly quota value (the breakdown endpoint's percent is normalized to the interval peak, see
// usage_wham_breakdown.go). The only honest derivation is: this cycle's used official credits converted to USD ÷
// the live used percentage from wham/usage. All of its error comes from the percentage having only integer precision.
type whamDailyCycleInput struct {
	ResetAt        time.Time
	WindowSeconds  int64
	WindowKind     string
	UsedPercent    float64
	UsedPercentOK  bool
	PercentUpdated time.Time
	// UsedCredits / Days are the totals over all snapshots with counts data since the cycle start day (whole UTC day).
	// The start day counts as a whole day: a cycle usually starts mid-day, so usage from before the cycle started on that day is also counted,
	// and this deviation is far smaller than the range caused by percentage rounding.
	UsedCredits float64
	Days        int
}

// buildWhamDailyCycle assembles the cost and quota estimate for the account's current reset cycle. Window information comes from the account's
// in-memory usage snapshot (refreshed by both response headers and the wham/usage probe), without calling upstream extra.
func (h *Handler) buildWhamDailyCycle(ctx context.Context, account *auth.Account, now time.Time) gin.H {
	if h == nil || h.db == nil || account == nil {
		return nil
	}
	snap := account.GetAccountListRuntimeSnapshot()
	windowSeconds := snap.Window7dSeconds
	if windowSeconds <= 0 {
		// When the long window length is unknown, assume 7 days: plus/pro default to a weekly window; free/team monthly windows carry their real length after the probe refreshes.
		windowSeconds = int64((7 * 24 * time.Hour) / time.Second)
	}
	in := whamDailyCycleInput{
		ResetAt:        snap.Reset7dAt,
		WindowSeconds:  windowSeconds,
		WindowKind:     account.Window7dKind(),
		UsedPercent:    snap.UsagePercent7d,
		UsedPercentOK:  snap.UsagePercent7dValid,
		PercentUpdated: account.GetUsageUpdatedAt(),
	}
	if !snap.Reset7dAt.IsZero() && snap.Reset7dAt.After(now) {
		startDay := snap.Reset7dAt.Add(-time.Duration(windowSeconds) * time.Second).UTC().Format("2006-01-02")
		if credits, days, err := h.db.SumAccountDailyUsageSince(ctx, account.DBID, startDay); err == nil {
			in.UsedCredits, in.Days = credits, days
		}
	}
	return projectWhamDailyCycle(in, now)
}

// projectWhamDailyCycle projects the window snapshot and this cycle's cost into the response. available means the window and live
// percentage were both obtained; estimate is only given when the division works, otherwise reason explains why.
func projectWhamDailyCycle(in whamDailyCycleInput, now time.Time) gin.H {
	out := gin.H{
		"available":      false,
		"used_credits":   in.UsedCredits,
		"used_usd":       in.UsedCredits / proxy.WhamCreditsPerUSD,
		"days":           in.Days,
		"window_seconds": in.WindowSeconds,
		"window_kind":    in.WindowKind,
	}
	switch {
	case in.ResetAt.IsZero():
		out["reason"] = "no_window"
		return out
	case !in.ResetAt.After(now):
		// The reset time has passed but the snapshot was not refreshed: the cycle start cannot be computed accurately, so wait for the next usage refresh.
		out["reason"] = "window_stale"
		return out
	}
	window := time.Duration(in.WindowSeconds) * time.Second
	out["start_at"] = in.ResetAt.Add(-window).UTC()
	out["reset_at"] = in.ResetAt.UTC()
	if !in.UsedPercentOK {
		out["reason"] = "no_percent"
		return out
	}
	out["available"] = true
	out["used_percent"] = in.UsedPercent
	if !in.PercentUpdated.IsZero() {
		out["used_percent_updated_at"] = in.PercentUpdated.UTC()
	}
	switch {
	case in.UsedCredits <= 0:
		// Free accounts always have 0 credits; for paid accounts, official statistics may not have produced numbers yet right after the cycle starts.
		out["reason"] = "no_credits"
	case in.UsedPercent <= whamDailyCycleMinPercent:
		out["reason"] = "percent_too_low"
	default:
		usedUSD := in.UsedCredits / proxy.WhamCreditsPerUSD
		percent := in.UsedPercent
		out["estimate"] = gin.H{
			"usd":      usedUSD / (percent / 100),
			"usd_low":  usedUSD / ((percent + 0.5) / 100),
			"usd_high": usedUSD / ((percent - 0.5) / 100),
			"reliable": percent >= whamDailyCycleReliablePercent,
		}
	}
	return out
}
