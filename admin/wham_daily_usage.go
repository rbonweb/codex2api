package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// whamDailyUsageDefaultDays is the number of days the dialog shows by default. Upstream can be queried back about 84 days, and once the local database has accumulated
// more, so the default is 30.
const whamDailyUsageDefaultDays = 30

// whamDailyUsageMaxDays limits the window of a single query. It matches the snapshot retention period whamDailyUsageKeepDays,
// so if a year is kept, a year can be queried; otherwise the part beyond the limit would never be visible.
const whamDailyUsageMaxDays = whamDailyUsageKeepDays

// whamDailyBreakdownMinShare is the minimum share for breakdown items sent to the frontend. Upstream catalogs can contain 1e-7-level crumbs
// (such as gpt-6-astra at 0.0004%), and a row that displays as $0 only adds clutter.
const whamDailyBreakdownMinShare = 1e-6

func (h *Handler) queryWhamDailyUsageUpstream(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
	if h != nil && h.queryWhamDailyUsage != nil {
		return h.queryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
	}
	return proxy.QueryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
}

func (h *Handler) queryWhamDailyTokenBreakdownUpstream(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
	if h != nil && h.queryWhamDailyTokenBreakdown != nil {
		return h.queryWhamDailyTokenBreakdown(ctx, account, proxyURL, startDate, endDate)
	}
	return proxy.QueryWhamDailyTokenBreakdown(ctx, account, proxyURL, startDate, endDate)
}

// GetAccountWhamDailyUsage returns the account's official settlement usage history.
// GET /api/admin/accounts/:id/wham-daily-usage?days=30[&refresh=1]
//
// By default it reads the local snapshot (opens instantly). With refresh=1 it first makes one upstream call to backfill and store the data in the rolling window,
// then returns the merged result, for the "Refresh" button in the dialog.
func (h *Handler) GetAccountWhamDailyUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	days := whamDailyUsageDefaultDays
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 || parsed > whamDailyUsageMaxDays {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("days must be an integer from 1 to %d", whamDailyUsageMaxDays))
			return
		}
		days = parsed
	}

	// A refresh failure does not block the history display: on upstream 401/429 the stored data is still returned,
	// and only the error reason is sent back, so the dialog shows a hint instead of going entirely blank.
	// counts and the breakdown are two endpoints that report errors separately: when the breakdown fails, counts have already refreshed successfully.
	refreshError := ""
	breakdownRefreshError := ""
	if c.Query("refresh") == "1" {
		account := h.findAccountByID(id)
		if needsPATWorkspaceHydration(account) {
			// A manual refresh is the user explicitly asking for official statistics: first try to fill in the workspace, not limited by backoff.
			h.hydratePATWorkspace(c.Request.Context(), account, true)
		}
		switch {
		case account == nil:
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		case isCodexATAccount(account) || !whamDailyUsageChannelSupported(account):
			refreshError = errWhamDailyUsageUnsupported.Error()
		case account.GetAccessToken() == "":
			refreshError = "account has no usable access token, please refresh the account first"
		default:
			ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
			outcome, syncErr := h.syncWhamDailyUsage(ctx, account)
			if syncErr != nil {
				refreshError = syncErr.Error()
			}
			if outcome.BreakdownErr != nil {
				breakdownRefreshError = outcome.BreakdownErr.Error()
			}
			cancel()
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.db.ListAccountDailyUsage(ctx, id, days)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "failed to read official usage history: "+err.Error())
		return
	}

	items := make([]gin.H, 0, len(rows))
	var totalCredits float64
	var totalTokens, totalTurns int64
	var lastSynced time.Time
	for _, row := range rows {
		// Rows where the breakdown arrives before counts are not shown: a day without official credits cannot be interpreted for shares,
		// and its synced_at should not be treated as "just synced" either. The row appears naturally once counts arrive.
		if row == nil || !row.HasCounts() {
			continue
		}
		totalCredits += row.Credits
		totalTokens += row.TotalTokens
		totalTurns += int64(row.Turns)
		if row.SyncedAt.After(lastSynced) {
			lastSynced = row.SyncedAt
		}
		item := gin.H{
			"day":                   row.Day,
			"credits":               row.Credits,
			"usd":                   row.Credits / proxy.WhamCreditsPerUSD,
			"users":                 row.Users,
			"threads":               row.Threads,
			"turns":                 row.Turns,
			"uncached_input_tokens": row.UncachedInputTokens,
			"cached_input_tokens":   row.CachedInputTokens,
			"output_tokens":         row.OutputTokens,
			"total_tokens":          row.TotalTokens,
			"settled":               row.Settled,
			"clients":               rawJSONArray(row.ClientsJSON),
			"models":                rawJSONArray(row.ModelsJSON),
		}
		breakdown, surfaces := projectWhamDailyBreakdown(row)
		item["breakdown_available"] = row.BreakdownPercent > 0
		item["breakdown"] = breakdown
		item["surfaces"] = surfaces
		items = append(items, item)
	}

	payload := gin.H{
		"days":  days,
		"items": items,
		"totals": gin.H{
			"credits":      totalCredits,
			"usd":          totalCredits / proxy.WhamCreditsPerUSD,
			"total_tokens": totalTokens,
			"turns":        totalTurns,
		},
		"credits_per_usd": proxy.WhamCreditsPerUSD,
		// The upstream lookback depth (the deep backfill window), which the frontend uses to explain "earlier data comes from the local snapshot".
		"retention_days": proxy.WhamDailyUsageBackfillDays,
	}
	if !lastSynced.IsZero() {
		payload["last_synced_at"] = lastSynced
	}
	// This cycle's cost and quota estimate: only given for accounts with a wham path; other channels have no window information.
	if account := h.findAccountByID(id); account != nil && whamDailyUsageChannelSupported(account) && !isCodexATAccount(account) {
		if cycle := h.buildWhamDailyCycle(ctx, account, time.Now()); cycle != nil {
			payload["cycle"] = cycle
		}
	}
	if refreshError != "" {
		payload["refresh_error"] = refreshError
	}
	if breakdownRefreshError != "" {
		payload["breakdown_refresh_error"] = breakdownRefreshError
	}
	c.JSON(http.StatusOK, payload)
}

// projectWhamDailyBreakdown turns the stored normalized ratios into same-day internal shares and allocated costs.
//
// Upstream percent is normalized to the request interval's peak, so values from different days or different syncs are not comparable; but within the same day
// each item divided by the day's total is its share (0~1), and the share times the day's official credits is the absolute
// cost of that model/entry. Free account credits are always 0; in that case only the share is meaningful, and credits/usd is sent as 0.
// Models and entries each use their own total as the denominator: measured as equal, but if upstream ever disagrees one day, each can still
// normalize to 1.
func projectWhamDailyBreakdown(row *database.AccountDailyUsage) ([]gin.H, []gin.H) {
	breakdown := []gin.H{}
	surfaces := []gin.H{}
	if row == nil || row.BreakdownPercent <= 0 {
		return breakdown, surfaces
	}
	modelTotal := row.BreakdownPercent
	for _, m := range database.ParseAccountDailyBreakdownModels(row.BreakdownJSON) {
		share := m.Percent / modelTotal
		if share < whamDailyBreakdownMinShare {
			continue
		}
		credits := share * row.Credits
		speed := strings.TrimSpace(m.Speed)
		if speed == "" {
			speed = proxy.WhamSpeedStandard
		}
		breakdown = append(breakdown, gin.H{
			"model":   m.Model,
			"speed":   speed,
			"share":   share,
			"credits": credits,
			"usd":     credits / proxy.WhamCreditsPerUSD,
		})
	}
	surfaceMap := database.ParseAccountDailySurfaces(row.SurfacesJSON)
	surfaceTotal := 0.0
	for _, value := range surfaceMap {
		if value > 0 {
			surfaceTotal += value
		}
	}
	if surfaceTotal > 0 {
		for _, s := range database.SortedAccountDailySurfaces(surfaceMap) {
			share := s.Percent / surfaceTotal
			if share < whamDailyBreakdownMinShare {
				continue
			}
			credits := share * row.Credits
			surfaces = append(surfaces, gin.H{
				"surface": s.Surface,
				"share":   share,
				"credits": credits,
				"usd":     credits / proxy.WhamCreditsPerUSD,
			})
		}
	}
	return breakdown, surfaces
}

// rawJSONArray turns the breakdown array, saved exactly as stored, back into a JSON value. On a parse failure it falls back to an empty array,
// keeping the response structure stable so the frontend needs no compatibility branch.
func rawJSONArray(raw string) []any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return []any{}
	}
	return out
}

// whamDailySyncOutcome is the result of one sync. counts is the main path, and its failure is returned directly as the error;
// the breakdown is supplementary, and its failure is only recorded in BreakdownErr and does not affect storing counts or the "synced" marker.
type whamDailySyncOutcome struct {
	CountsDays    int
	BreakdownDays int
	BreakdownErr  error
}

// whamDailyDeepState records, within this process, whether each of the two endpoints of an account has completed one deep backfill request.
// Process memory is enough: after a restart, at most one more deep pull happens.
type whamDailyDeepState struct {
	Counts    bool
	Breakdown bool
}

func (h *Handler) whamDailyDeepStateFor(id int64) whamDailyDeepState {
	if h == nil {
		return whamDailyDeepState{}
	}
	h.whamDailyBackfillMu.Lock()
	defer h.whamDailyBackfillMu.Unlock()
	return h.whamDailyDeepSynced[id]
}

func (h *Handler) markWhamDailyDeepSynced(id int64, counts, breakdown bool) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	defer h.whamDailyBackfillMu.Unlock()
	if h.whamDailyDeepSynced == nil {
		h.whamDailyDeepSynced = map[int64]whamDailyDeepState{}
	}
	state := h.whamDailyDeepSynced[id]
	state.Counts = state.Counts || counts
	state.Breakdown = state.Breakdown || breakdown
	h.whamDailyDeepSynced[id] = state
}

// whamDailyWindow is the date range of one upstream request; Deep means this is a deep backfill (84 days) and not
// the 7-day rolling window.
type whamDailyWindow struct {
	Start string
	End   string
	Deep  bool
}

// whamDailySyncWindows decides the sync window for each endpoint separately.
//
// Conditions for a deep backfill (84 days): this process has not yet done a deep pull for the endpoint, and the local coverage start of that endpoint is later than
// the deep backfill start (including having no coverage at all). The two endpoints must be judged separately: if the breakdown arrives first it creates rows with zero counts,
// so if only "whether rows exist" is checked, a first-round counts failure would be locked into the 7-day window forever, and the history would never be fetched;
// the reverse holds for the breakdown. After an upgrade of an old deployment, local history only has 7-day rolling accumulation and also meets the condition, so each endpoint does one deep pull
// to bring in the 84 days upstream still holds. For accounts whose history is shorter than 84 days, only one extra pull happens per process lifetime,
// guarded by the in-process marker.
func (h *Handler) whamDailySyncWindows(ctx context.Context, account *auth.Account, now time.Time) (counts, breakdown whamDailyWindow) {
	rollingStart, endDate := proxy.WhamDailyUsageWindow(now)
	deepStart, _ := proxy.WhamDailyUsageBackfillWindow(now)
	counts = whamDailyWindow{Start: rollingStart, End: endDate}
	breakdown = counts
	if h == nil || h.db == nil || account == nil {
		return counts, breakdown
	}
	coverage, err := h.db.GetAccountDailyUsageCoverage(ctx, account.DBID)
	if err != nil {
		// If coverage cannot be looked up, use the rolling window and do not interrupt the sync because of it.
		return counts, breakdown
	}
	state := h.whamDailyDeepStateFor(account.DBID)
	if !state.Counts && (coverage.CountsOldestDay == "" || coverage.CountsOldestDay > deepStart) {
		counts.Start, counts.Deep = deepStart, true
	}
	if !state.Breakdown && (coverage.BreakdownOldestDay == "" || coverage.BreakdownOldestDay > deepStart) {
		breakdown.Start, breakdown.Deep = deepStart, true
	}
	return counts, breakdown
}

// syncWhamDailyUsage pulls counts and the model × speed breakdown in parallel and upserts them into storage.
//
// Window: in steady state, the last 7 days are rolled back-filled (the records of the current day change throughout the day and only settle the next day; covering the whole window also repairs
// gaps from missed runs); when an endpoint lacks sufficiently deep local coverage, that endpoint gets a deep backfill of 84 days, see
// whamDailySyncWindows. The two endpoints run in parallel with the same ctx: a slow or failed one does not drag down
// the storage of the other.
func (h *Handler) syncWhamDailyUsage(ctx context.Context, account *auth.Account) (whamDailySyncOutcome, error) {
	var outcome whamDailySyncOutcome
	if h == nil || h.db == nil || account == nil {
		return outcome, errWhamDailyUsageUnavailable
	}
	// Manual refresh also reaches here: credentials from non-ChatGPT channels are rejected here as a fallback, not only filtered by probe candidates.
	if isCodexATAccount(account) || !whamDailyUsageChannelSupported(account) {
		return outcome, errWhamDailyUsageUnsupported
	}
	countsWindow, breakdownWindow := h.whamDailySyncWindows(ctx, account, time.Now())
	proxyURL := h.store.ResolveProxyForAccount(account)

	var (
		wg        sync.WaitGroup
		countsErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		outcome.CountsDays, countsErr = h.syncWhamDailyCounts(ctx, account, proxyURL, countsWindow.Start, countsWindow.End)
		if countsErr == nil && countsWindow.Deep {
			h.markWhamDailyDeepSynced(account.DBID, true, false)
		}
	}()
	go func() {
		defer wg.Done()
		outcome.BreakdownDays, outcome.BreakdownErr = h.syncWhamDailyBreakdown(ctx, account, proxyURL, breakdownWindow.Start, breakdownWindow.End)
		if outcome.BreakdownErr == nil && breakdownWindow.Deep {
			h.markWhamDailyDeepSynced(account.DBID, false, true)
		}
	}()
	wg.Wait()

	if countsErr != nil {
		return outcome, countsErr
	}
	// Empty data also counts as a successful sync: upstream really has no records in the window (official statistics lag, or there is no official
	// consumption), so this success must be remembered, otherwise page-stats would treat the account as missing forever.
	h.markWhamDailySynced(account.DBID)
	return outcome, nil
}

// classifyWhamDailyUpstreamError maps a non-200 upstream response to a displayable error.
// When resp is nil (a request-level error) or 200 (reading the body or parsing failed, the body was already read by the query function),
// err is returned unchanged; a parse error must not be reported as "upstream returned 200".
func classifyWhamDailyUpstreamError(resp *http.Response, err error) error {
	if resp == nil || resp.StatusCode == http.StatusOK {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errWhamDailyUsageUnauthorized
	case http.StatusTooManyRequests:
		return errWhamDailyUsageRateLimited
	default:
		return upstreamDailyUsageError(resp.StatusCode, body)
	}
}

// syncWhamDailyCounts pulls the counts endpoint and stores it per day, returning the number of days written.
func (h *Handler) syncWhamDailyCounts(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (int, error) {
	result, resp, err := h.queryWhamDailyUsageUpstream(ctx, account, proxyURL, startDate, endDate)
	if err != nil {
		return 0, classifyWhamDailyUpstreamError(resp, err)
	}

	written := 0
	for _, day := range result.Data {
		normalized := strings.TrimSpace(day.Date)
		if normalized == "" {
			continue
		}
		clients, _ := json.Marshal(day.Clients)
		models, _ := json.Marshal(day.Models)
		input := database.AccountDailyUsageInput{
			AccountID: account.DBID,
			Day:       normalized,
			Credits:   day.Totals.Credits,
			Users:     day.Totals.Users,
			Threads:   day.Totals.Threads,
			Turns:     day.Totals.Turns,
			// endDate is today (UTC): today's rows change all day, so judge by date, not by whether fields are present.
			Settled:     day.SettledOn(endDate),
			ClientsJSON: string(clients),
			ModelsJSON:  string(models),
		}
		if day.Totals.UncachedTextInputTokens != nil {
			input.UncachedInputTokens = *day.Totals.UncachedTextInputTokens
		}
		if day.Totals.CachedTextInputTokens != nil {
			input.CachedInputTokens = *day.Totals.CachedTextInputTokens
		}
		if day.Totals.TextOutputTokens != nil {
			input.OutputTokens = *day.Totals.TextOutputTokens
		}
		if day.Totals.TextTotalTokens != nil {
			input.TotalTokens = *day.Totals.TextTotalTokens
		}
		if err := h.db.UpsertAccountDailyUsage(ctx, input); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// syncWhamDailyBreakdown pulls the model × speed breakdown and stores it per day, returning the number of days written.
//
// The breakdown endpoint is dense: one row per day in the interval, including future dates, and days with no usage are all zeros. Only days with usage are written:
// past days and today; future rows and all-zero rows are skipped, otherwise a pile of empty rows with zero counts would be created.
func (h *Handler) syncWhamDailyBreakdown(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (int, error) {
	result, resp, err := h.queryWhamDailyTokenBreakdownUpstream(ctx, account, proxyURL, startDate, endDate)
	if err != nil {
		return 0, classifyWhamDailyUpstreamError(resp, err)
	}
	written := 0
	for _, day := range result.Data {
		date := strings.TrimSpace(day.Date)
		if date == "" || date > endDate {
			continue
		}
		total := day.TotalPercent()
		if total <= 0 {
			continue
		}
		active := day.ActiveModels()
		models := make([]database.AccountDailyBreakdownModel, 0, len(active))
		for _, m := range active {
			models = append(models, database.AccountDailyBreakdownModel{Model: m.Model, Speed: m.Speed, Percent: m.Percent})
		}
		input := database.AccountDailyBreakdownInput{
			AccountID: account.DBID,
			Day:       date,
			Percent:   total,
			Models:    models,
			Surfaces:  day.ActiveSurfaces(),
		}
		if err := h.db.UpsertAccountDailyBreakdown(ctx, input); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}
