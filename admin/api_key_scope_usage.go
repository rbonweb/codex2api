package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// apiKeyScopeUsageItem is the current usage of an API Key against one scope limit (issue #439).
// It only returns the windows where this scope actually has a limit configured; the frontend uses this to render progress bars.
type apiKeyScopeUsageItem struct {
	ScopeType   string                       `json:"scope_type"`
	ScopeID     int64                        `json:"scope_id"`
	ScopeName   string                       `json:"scope_name"`
	ScopeExists bool                         `json:"scope_exists"`
	OnExhausted string                       `json:"on_exhausted"`
	Windows     []apiKeyScopeUsageWindowItem `json:"windows"`
	// Cumulative is the current state of the cumulative quota (it does not decay over time and must be reset manually).
	Cumulative *apiKeyScopeCumulativeItem `json:"cumulative,omitempty"`
	// Skips is the runtime count of times this budget was judged exhausted (in-process, reset on restart). After the budget is exhausted, requests silently
	// land on other accounts; this count is the only direct signal for answering "is this budget really taking effect?"
	Skips *apiKeyScopeSkipItem `json:"skips,omitempty"`
}

type apiKeyScopeCumulativeItem struct {
	UsedCost      float64 `json:"used_cost"`
	UsedTokens    int64   `json:"used_tokens"`
	UsedRequests  int64   `json:"used_requests"`
	QuotaCost     float64 `json:"quota_cost,omitempty"`
	QuotaTokens   int64   `json:"quota_tokens,omitempty"`
	QuotaRequests int64   `json:"quota_requests,omitempty"`
	ResetCount    int     `json:"reset_count"`
	LastResetAt   string  `json:"last_reset_at,omitempty"`
	Exhausted     bool    `json:"exhausted"`
}

type apiKeyScopeSkipItem struct {
	Requests    int64  `json:"requests"`
	FirstAt     string `json:"first_at"`
	LastAt      string `json:"last_at"`
	LastMessage string `json:"last_message"`
}

type apiKeyScopeUsageWindowItem struct {
	Window       string  `json:"window"`
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	UserBilled   float64 `json:"user_billed"`
	CostLimit    float64 `json:"cost_limit,omitempty"`
	TokenLimit   int64   `json:"token_limit,omitempty"`
	RequestLimit int64   `json:"request_limit,omitempty"`
	Exhausted    bool    `json:"exhausted"`
}

// GetAPIKeyScopeUsage returns the current usage of an API Key against each group/account-dimension limit.
// GET /api/admin/keys/:id/scope-usage
//
// The basis matches the gateway's decision exactly (the same per-account window aggregation, plus conversion using the current group membership),
// The only difference is that the gateway's in-process incremental events are not added here, so it may lag actual usage by at most one batch write cycle.
func (h *Handler) GetAPIKeyScopeUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid API Key ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	row, err := h.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if row == nil {
		writeError(c, http.StatusNotFound, "API Key 不存在")
		return
	}

	scopes := database.NormalizeAPIKeyScopeLimits(row.Limits.ScopeLimits)
	items := make([]apiKeyScopeUsageItem, 0, len(scopes))
	if len(scopes) == 0 {
		c.JSON(http.StatusOK, gin.H{"items": items})
		return
	}

	// Four conditional aggregates share one 30-day index scan. This avoids the
	// previous 5h/1d/7d/30d repeated scans when opening a Key with scope limits.
	usageByWindow, err := h.db.GetAPIKeyAccountWindowsUsage(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	groupNames := h.accountGroupNameIndex(ctx)
	groupsByAccount := make(map[int64][]int64)
	for _, perAccount := range usageByWindow {
		h.collectAccountGroups(perAccount, groupsByAccount)
	}
	skips := proxy.APIKeyScopeSkipSnapshot(id)
	counters, err := h.db.ListAPIKeyScopeCounters(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	for _, scope := range scopes {
		item := apiKeyScopeUsageItem{
			ScopeType:   scope.ResolveScopeType(),
			ScopeID:     scope.ScopeID,
			OnExhausted: scope.ResolveOnExhausted(),
			Windows:     make([]apiKeyScopeUsageWindowItem, 0, len(database.APIKeyScopeWindows)),
		}
		if item.ScopeType == database.APIKeyScopeTypeAccount {
			// The frontend resolves account display names from its own account list; here we only check whether the account is still in the pool.
			item.ScopeExists = h.store != nil && h.store.FindByID(scope.ScopeID) != nil
		} else if name, ok := groupNames[scope.ScopeID]; ok {
			item.ScopeName = name
			item.ScopeExists = true
		}

		for _, window := range database.APIKeyScopeWindows {
			if !scope.NeedsWindow(window.Label) {
				continue
			}
			perAccount, ok := usageByWindow[window.Label]
			if !ok {
				continue
			}
			usage := aggregateScopeUsage(scope, perAccount, groupsByAccount)
			_, exhausted := scope.CheckWindow(window.Label, usage)
			entry := apiKeyScopeUsageWindowItem{
				Window:     window.Label,
				Requests:   usage.Requests,
				Tokens:     usage.Tokens,
				UserBilled: usage.UserBilled,
				Exhausted:  exhausted,
			}
			switch window.Label {
			case "5h":
				entry.CostLimit, entry.TokenLimit = scope.Cost5h, scope.Token5h
			case "1d":
				entry.CostLimit, entry.TokenLimit, entry.RequestLimit = scope.Cost1d, scope.Token1d, scope.Requests1d
			case "7d":
				entry.CostLimit, entry.TokenLimit = scope.Cost7d, scope.Token7d
			case "30d":
				entry.CostLimit, entry.TokenLimit = scope.Cost30d, scope.Token30d
			}
			item.Windows = append(item.Windows, entry)
		}
		if scope.HasCumulativeQuota() {
			counter := counters[database.APIKeyScopeCounterKey{ScopeType: item.ScopeType, ScopeID: item.ScopeID}]
			_, exhausted := scope.CheckCumulative(counter)
			cumulative := &apiKeyScopeCumulativeItem{
				UsedCost:      counter.UsedCost,
				UsedTokens:    counter.UsedTokens,
				UsedRequests:  counter.UsedRequests,
				QuotaCost:     scope.QuotaCost,
				QuotaTokens:   scope.QuotaTokens,
				QuotaRequests: scope.QuotaRequests,
				ResetCount:    counter.ResetCount,
				Exhausted:     exhausted,
			}
			if counter.LastResetAt.Valid {
				cumulative.LastResetAt = counter.LastResetAt.Time.Format(time.RFC3339)
			}
			item.Cumulative = cumulative
		}
		if stat, ok := skips[fmt.Sprintf("%s:%d", item.ScopeType, item.ScopeID)]; ok && stat.Requests > 0 {
			item.Skips = &apiKeyScopeSkipItem{
				Requests:    stat.Requests,
				FirstAt:     stat.FirstAt.Format(time.RFC3339),
				LastAt:      stat.LastAt.Format(time.RFC3339),
				LastMessage: stat.LastMessage,
			}
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}

type resetAPIKeyScopeQuotaReq struct {
	ScopeType string `json:"scope_type"`
	ScopeID   int64  `json:"scope_id"`
}

// ResetAPIKeyScopeQuota zeroes the cumulative quota of one scope and records one reset.
// POST /api/admin/keys/:id/scope-quota/reset
//
// The cumulative quota does not decay over time (this is what distinguishes it from a sliding window), so it needs this explicit entry point,
// otherwise it would be stuck permanently once used up. Semantics match the key-level quota's reset_quota.
func (h *Handler) ResetAPIKeyScopeQuota(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid API Key ID")
		return
	}
	var req resetAPIKeyScopeQuotaReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.ScopeID <= 0 {
		writeError(c, http.StatusBadRequest, "scope_id must be a positive integer")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	row, err := h.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if row == nil {
		writeError(c, http.StatusNotFound, "API Key 不存在")
		return
	}
	if err := h.db.ResetAPIKeyScopeCounter(ctx, id, req.ScopeType, req.ScopeID); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	// Re-observe the hit stats right after the reset; otherwise historical counts would make it look like the budget is still being blocked.
	proxy.ResetAPIKeyScopeSkipStats(id)
	writeMessage(c, http.StatusOK, "Cumulative quota has been reset")
}

// apiKeyScopeSummaryItem is the scope budget overview for the list page: it only gives the share of "the tightest window",
// details are left to the edit drawer.
type apiKeyScopeSummaryItem struct {
	ScopeType    string  `json:"scope_type"`
	ScopeID      int64   `json:"scope_id"`
	ScopeName    string  `json:"scope_name"`
	OnExhausted  string  `json:"on_exhausted"`
	Window       string  `json:"window"`
	Metric       string  `json:"metric"`
	Ratio        float64 `json:"ratio"`
	Exhausted    bool    `json:"exhausted"`
	SkipRequests int64   `json:"skip_requests,omitempty"`
}

// GetAPIKeysScopeSummary returns the budget overview of all keys that have scope budgets configured, in one call.
// GET /api/admin/keys-scope-summary
//
// Each window runs only one cross-key aggregate query, to avoid the N×window amplification that per-key lookups on the list page would cause.
func (h *Handler) GetAPIKeysScopeSummary(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	scopesByKey := make(map[int64][]database.APIKeyScopeLimit)
	keyIDs := make([]int64, 0, len(keys))
	for _, key := range keys {
		scopes := database.NormalizeAPIKeyScopeLimits(key.Limits.ScopeLimits)
		if len(scopes) == 0 {
			continue
		}
		scopesByKey[key.ID] = scopes
		keyIDs = append(keyIDs, key.ID)
	}
	summary := make(map[string][]apiKeyScopeSummaryItem)
	if len(keyIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"summary": summary})
		return
	}

	// Only query the windows that are actually used.
	usageByWindow := make(map[string]map[int64]map[int64]database.APIKeyWindowUsage)
	for _, window := range database.APIKeyScopeWindows {
		needed := false
		for _, scopes := range scopesByKey {
			for _, scope := range scopes {
				if scope.NeedsWindow(window.Label) {
					needed = true
					break
				}
			}
			if needed {
				break
			}
		}
		if !needed {
			continue
		}
		usage, err := h.db.GetAPIKeysAccountWindowUsage(ctx, keyIDs, window.Window)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		usageByWindow[window.Label] = usage
	}

	groupNames := h.accountGroupNameIndex(ctx)
	groupsByAccount := make(map[int64][]int64)
	for _, perKey := range usageByWindow {
		for _, perAccount := range perKey {
			h.collectAccountGroups(perAccount, groupsByAccount)
		}
	}
	countersByKey, err := h.db.ListAPIKeyScopeCountersForKeys(ctx, keyIDs)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	for keyID, scopes := range scopesByKey {
		skips := proxy.APIKeyScopeSkipSnapshot(keyID)
		items := make([]apiKeyScopeSummaryItem, 0, len(scopes))
		for _, scope := range scopes {
			item := apiKeyScopeSummaryItem{
				ScopeType:   scope.ResolveScopeType(),
				ScopeID:     scope.ScopeID,
				OnExhausted: scope.ResolveOnExhausted(),
			}
			if item.ScopeType == database.APIKeyScopeTypeGroup {
				item.ScopeName = groupNames[scope.ScopeID]
			}
			if stat, ok := skips[fmt.Sprintf("%s:%d", item.ScopeType, item.ScopeID)]; ok {
				item.SkipRequests = stat.Requests
			}
			// The overview only keeps the "most used" window. A share >= 1 is equivalent to CheckWindow's exhaustion check
			// (both are used >= limit), so the check does not need to run again separately.
			for _, window := range database.APIKeyScopeWindows {
				if !scope.NeedsWindow(window.Label) {
					continue
				}
				perKey, ok := usageByWindow[window.Label]
				if !ok {
					continue
				}
				usage := aggregateScopeUsage(scope, perKey[keyID], groupsByAccount)
				ratio, metric := scopeWindowRatio(scope, window.Label, usage)
				if item.Window != "" && ratio <= item.Ratio {
					continue
				}
				item.Ratio = ratio
				item.Window = window.Label
				item.Metric = metric
				item.Exhausted = ratio >= 1
			}
			// The cumulative quota must also take part in the "tightest" comparison, otherwise a key that only has a cumulative quota would show
			// 0% in the list.
			if scope.HasCumulativeQuota() {
				counter := countersByKey[keyID][database.APIKeyScopeCounterKey{
					ScopeType: item.ScopeType,
					ScopeID:   item.ScopeID,
				}]
				ratio, metric := scopeCumulativeRatio(scope, counter)
				if item.Window == "" || ratio > item.Ratio {
					item.Ratio = ratio
					item.Window = database.APIKeyScopeWindowTotal
					item.Metric = metric
					item.Exhausted = ratio >= 1
				}
			}
			items = append(items, item)
		}
		summary[strconv.FormatInt(keyID, 10)] = items
	}

	c.JSON(http.StatusOK, gin.H{"summary": summary})
}

// scopeCumulativeRatio returns the share of the "most used" basis within the cumulative quota, and the basis name.
func scopeCumulativeRatio(scope database.APIKeyScopeLimit, counter database.APIKeyScopeCounter) (float64, string) {
	best := 0.0
	metric := ""
	if scope.QuotaCost > 0 {
		best = counter.UsedCost / scope.QuotaCost
		metric = "cost"
	}
	if scope.QuotaTokens > 0 {
		if ratio := float64(counter.UsedTokens) / float64(scope.QuotaTokens); ratio > best {
			best = ratio
			metric = "tokens"
		}
	}
	if scope.QuotaRequests > 0 {
		if ratio := float64(counter.UsedRequests) / float64(scope.QuotaRequests); ratio > best {
			best = ratio
			metric = "requests"
		}
	}
	return best, metric
}

// scopeWindowRatio returns the share (0 to 1+) of the "most used" basis within a window, and the basis name.
func scopeWindowRatio(scope database.APIKeyScopeLimit, label string, usage database.APIKeyWindowUsage) (float64, string) {
	var costLimit float64
	var tokenLimit, requestLimit int64
	switch label {
	case "5h":
		costLimit, tokenLimit = scope.Cost5h, scope.Token5h
	case "1d":
		costLimit, tokenLimit, requestLimit = scope.Cost1d, scope.Token1d, scope.Requests1d
	case "7d":
		costLimit, tokenLimit = scope.Cost7d, scope.Token7d
	case "30d":
		costLimit, tokenLimit = scope.Cost30d, scope.Token30d
	}
	best := 0.0
	metric := ""
	if costLimit > 0 {
		best = usage.UserBilled / costLimit
		metric = "cost"
	}
	if tokenLimit > 0 {
		if ratio := float64(usage.Tokens) / float64(tokenLimit); ratio > best {
			best = ratio
			metric = "tokens"
		}
	}
	if requestLimit > 0 {
		if ratio := float64(usage.Requests) / float64(requestLimit); ratio > best {
			best = ratio
			metric = "requests"
		}
	}
	return best, metric
}

// aggregateScopeUsage converts usage split by account into one scope.
func aggregateScopeUsage(
	scope database.APIKeyScopeLimit,
	perAccount map[int64]database.APIKeyWindowUsage,
	groupsByAccount map[int64][]int64,
) database.APIKeyWindowUsage {
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return perAccount[scope.ScopeID]
	}
	var usage database.APIKeyWindowUsage
	for accountID, accountUsage := range perAccount {
		matched := false
		for _, groupID := range groupsByAccount[accountID] {
			if groupID == scope.ScopeID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		usage.Requests += accountUsage.Requests
		usage.Tokens += accountUsage.Tokens
		usage.UserBilled += accountUsage.UserBilled
	}
	return usage
}

// collectAccountGroups adds the current groups of the accounts appearing in the aggregate result to out. Accounts that were removed from the account pool
// cannot be resolved to a group, so their historical usage is ignored at the group level.
func (h *Handler) collectAccountGroups(perAccount map[int64]database.APIKeyWindowUsage, out map[int64][]int64) {
	if h == nil || h.store == nil || out == nil {
		return
	}
	for accountID := range perAccount {
		if _, ok := out[accountID]; ok {
			continue
		}
		account := h.store.FindByID(accountID)
		if account == nil {
			out[accountID] = nil
			continue
		}
		out[accountID] = account.GroupIDSnapshot()
	}
}

// accountGroupNameIndex returns a group ID → name map. On query failure it returns an empty map (the frontend falls back to showing the ID).
func (h *Handler) accountGroupNameIndex(ctx context.Context) map[int64]string {
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		return map[int64]string{}
	}
	out := make(map[int64]string, len(groups))
	for _, group := range groups {
		out[group.ID] = group.Name
	}
	return out
}
