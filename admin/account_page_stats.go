package admin

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type accountPageStatsItem struct {
	Usage5hDetail *accountUsageWindow `json:"usage_5h_detail,omitempty"`
	Usage7dDetail *accountUsageWindow `json:"usage_7d_detail,omitempty"`
	// UsageTodayDetail is the gateway-side aggregate for "today" (from 00:00 in the server time zone).
	// Unlike 5h/7d, this field is always sent for every account requested: a zero value means
	// "not run today"; only a missing field means "not loaded yet". The frontend uses this to tell 0 apart from a placeholder.
	UsageTodayDetail *accountUsageWindow `json:"usage_today_detail,omitempty"`
	Billed5h         *float64            `json:"billed_5h,omitempty"`
	Billed7d         *float64            `json:"billed_7d,omitempty"`
	// OfficialUSD is the cumulative cost on the official billing basis (the full local snapshot window, one year by default).
	// It is a separate ledger from Billed7d above, which is computed from local logs: the gateway only sees the requests it forwards,
	// while the official bill also includes usage from official clients used directly. It reads the snapshot table and does not hit the upstream.
	OfficialUSD *float64 `json:"official_usd,omitempty"`
	// OfficialUSD7d is a compatibility alias for OfficialUSD. The list badge now reads "official billing",
	// no longer showing only 7 days; old frontends still read this field.
	OfficialUSD7d *float64 `json:"official_usd_7d,omitempty"`
	// OfficialUsageSynced means the official snapshot has synced successfully before, but the upstream window has no data
	// (official stats lag, or the account has no official client usage). The frontend uses this to show a static placeholder
	// and stops refetching, instead of showing "no data yet" as "still loading" in an endless spinner.
	OfficialUsageSynced bool `json:"official_usage_synced,omitempty"`
}

// GetAccountPageStats loads log-derived usage and billing values after the
// visible account page has rendered. The request is strictly bounded to one
// page, avoiding any pool-wide aggregation on the first-screen path.
func (h *Handler) GetAccountPageStats(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid ids parameter")
		return
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"stats": map[int64]accountPageStatsItem{}})
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "Up to 500 ids are allowed")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	now := time.Now()
	usage5h, usage7d, err := h.db.GetAccountUsageWindowsByIDs(ctx, ids, now.Add(-5*time.Hour), now.AddDate(0, 0, -7))
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// The today basis matches the global stats: from 00:00 in the server time zone (TZ setting, defaults to the host time zone).
	// On failure the whole column is not sent, so the frontend keeps its placeholder instead of showing "query failed" as 0.
	todaySince := database.StartOfDay(now)
	usageToday, todayErr := h.db.GetAccountUsageSinceByIDs(ctx, ids, todaySince)
	if todayErr != nil {
		log.Printf("failed to get today's usage for the current page accounts: %v", todayErr)
	}
	todayModels, todayModelErr := h.db.GetAccountModelCountsSinceByIDs(ctx, ids, todaySince)
	if todayModelErr != nil {
		log.Printf("failed to get today's model distribution for the current page accounts: %v", todayModelErr)
		todayModels = map[int64]map[string]database.AccountModelCount{}
	}

	billing5hWindows, billing7dWindows := h.accountBillingWindows(ids)
	billed5h, err := h.db.GetAccountsBilledSince(ctx, billing5hWindows)
	if err != nil {
		log.Printf("failed to get 5h cost for the current page accounts: %v", err)
		billed5h = map[int64]float64{}
	}
	billed7d, err := h.db.GetAccountsBilledSince(ctx, billing7dWindows)
	if err != nil {
		log.Printf("failed to get long-window cost for the current page accounts: %v", err)
		billed7d = map[int64]float64{}
	}

	// A missing official billing snapshot only drops one display row; it must not take down the whole page's stats.
	officialTotals, err := h.db.SumAccountDailyUsage(ctx, ids, whamDailyUsageKeepDays)
	if err != nil {
		log.Printf("failed to get official billing cost for the current page accounts: %v", err)
		officialTotals = map[int64]database.AccountDailyUsageTotal{}
	}

	stats := make(map[int64]accountPageStatsItem, len(ids))
	missingOfficial := make([]int64, 0)
	for _, id := range ids {
		item := accountPageStatsItem{}
		if value := usage5h[id]; value != nil {
			item.Usage5hDetail = &accountUsageWindow{
				Requests: value.Requests, Tokens: value.Tokens,
				AccountBilled: value.AccountBilled, UserBilled: value.UserBilled,
			}
		}
		if value := usage7d[id]; value != nil {
			item.Usage7dDetail = &accountUsageWindow{
				Requests: value.Requests, Tokens: value.Tokens,
				AccountBilled: value.AccountBilled, UserBilled: value.UserBilled,
			}
		}
		if todayErr == nil {
			todayWindow := &accountUsageWindow{}
			if value := usageToday[id]; value != nil {
				todayWindow.Requests = value.Requests
				todayWindow.Tokens = value.Tokens
				todayWindow.AccountBilled = value.AccountBilled
				todayWindow.UserBilled = value.UserBilled
			}
			if models := todayModels[id]; len(models) > 0 {
				todayWindow.ModelCounts = make(map[string]int64, len(models))
				todayWindow.ModelSuccessCounts = make(map[string]int64, len(models))
				todayWindow.ModelAvgFirstTokenMs = make(map[string]float64, len(models))
				for model, count := range models {
					todayWindow.ModelCounts[model] = count.Requests
					todayWindow.ModelSuccessCounts[model] = count.Success
					if count.AvgFirstTokenMs > 0 {
						todayWindow.ModelAvgFirstTokenMs[model] = count.AvgFirstTokenMs
					}
				}
				if len(todayWindow.ModelAvgFirstTokenMs) == 0 {
					todayWindow.ModelAvgFirstTokenMs = nil
				}
			}
			item.UsageTodayDetail = todayWindow
		}
		if value, ok := billed5h[id]; ok {
			item.Billed5h = &value
		}
		if value, ok := billed7d[id]; ok {
			item.Billed7d = &value
		}
		// Accounts without a snapshot do not get this field; the frontend shows a placeholder instead of $0;
		// a missing snapshot on the current page is backfilled asynchronously without blocking this response. Accounts that synced successfully before but
		// have no upstream data get an explicit empty state and are not backfilled again and again.
		if total, ok := officialTotals[id]; ok {
			usd := total.Credits / proxy.WhamCreditsPerUSD
			item.OfficialUSD = &usd
			item.OfficialUSD7d = &usd
		} else if h.whamDailySyncedOnceFor(id) {
			item.OfficialUsageSynced = true
		} else if h.store != nil && whamDailyUsageBackfillEligible(h.store.FindByID(id)) {
			missingOfficial = append(missingOfficial, id)
		}
		stats[id] = item
	}
	h.enqueueWhamDailyUsageBackfill(missingOfficial)
	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

func (h *Handler) accountBillingWindows(ids []int64) (map[int64]time.Time, map[int64]time.Time) {
	shortWindows := make(map[int64]time.Time, len(ids))
	longWindows := make(map[int64]time.Time, len(ids))
	if h.store == nil {
		return shortWindows, longWindows
	}
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		if resetAt := account.GetReset5hAt(); !resetAt.IsZero() {
			shortWindows[id] = resetAt.Add(-5 * time.Hour)
		}
		if resetAt := account.GetReset7dAt(); !resetAt.IsZero() {
			window := 7 * 24 * time.Hour
			if seconds := account.GetWindow7dSeconds(); seconds > 0 {
				window = time.Duration(seconds) * time.Second
			}
			longWindows[id] = resetAt.Add(-window)
		}
	}
	return shortWindows, longWindows
}
