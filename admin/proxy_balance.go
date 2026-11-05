package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// autoBalanceProxiesReq is the request body for proxy balance binding.
//   - Channel: codex/empty (all OAuth). When too many OAuth accounts sit on the same egress,
//     balanced binding spreads the accounts out, lowering the chance the upstream rate-limits by egress aggregation.
//   - Mode: unbound (default, assigns only unbound accounts) / all (full reshuffle, but keeps existing bindings where possible to reduce IP changes).
//   - MaxPerProxy: maximum accounts per proxy; 0 means no limit.
//   - ProxyIDs: limits which proxies take part in assignment; empty means all enabled proxies with no connectivity-test error.
type autoBalanceProxiesReq struct {
	Channel     string  `json:"channel"`
	Mode        string  `json:"mode"`
	MaxPerProxy int     `json:"max_per_proxy"`
	ProxyIDs    []int64 `json:"proxy_ids"`
}

type autoBalanceDistribution struct {
	ProxyID    int64  `json:"proxy_id"`
	Label      string `json:"label,omitempty"`
	BoundCount int64  `json:"bound_count"`
}

// balanceAccount is one account taking part in balanced assignment: ID + its current binding (trimmed; may be empty).
type balanceAccount struct {
	id      int64
	current string
}

// isOAuthProxyBalanceTarget only lets genuinely refreshable OAuth accounts take part in automatic balancing.
// API Key, AT-only, Session Token, and Agent Identity accounts should not be
// moved automatically to a different egress; their existing manual bindings still count as baseline load in the capacity calculation.
func isOAuthProxyBalanceTarget(row *database.AccountRow) bool {
	if row == nil || strings.TrimSpace(row.GetCredential("refresh_token")) == "" {
		return false
	}
	if strings.TrimSpace(row.GetCredential("api_key")) != "" {
		return false
	}

	upstreamType := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	switch upstreamType {
	case "":
		return strings.EqualFold(strings.TrimSpace(row.Type), "oauth")
	default:
		return false
	}
}

func selectOAuthProxyBalanceTargets(rows []*database.AccountRow, mode string) []balanceAccount {
	targets := make([]balanceAccount, 0, len(rows))
	for _, row := range rows {
		if !isOAuthProxyBalanceTarget(row) {
			continue
		}
		bound := strings.TrimSpace(row.ProxyURL)
		if mode == "unbound" && bound != "" {
			continue
		}
		targets = append(targets, balanceAccount{id: row.ID, current: bound})
	}
	return targets
}

// balanceResult is the output of the pure assignment algorithm.
type balanceResult struct {
	// assignments contains only the accounts whose binding changes (the write scope).
	assignments map[int64]string
	// load is the number of accounts on each candidate proxy after assignment (including baseline load).
	load map[string]int64
	// kept is the number of accounts that kept their original binding; skipped is the number of accounts that could not fit once every proxy reached its limit.
	kept    int
	skipped int
}

// computeProxyAssignments assigns targets to candidates by "fewest bindings first".
//
// Assignment principles:
//  1. Stability first: an account whose current binding is still in the candidate pool and not over the limit stays put
//     (the upstream is sensitive to IP changes, so reshuffling should only move accounts that must move);
//  2. The remaining accounts go in ascending account ID order, each into the proxy with the lowest current load;
//     ties go to the URL in lexicographic order, so the result is deterministic and reproducible;
//  3. baseline is the number of existing bindings on each proxy that do not take part in this assignment (including accounts from other channels),
//     so accounts are not piled onto proxies that other channels already fill.
func computeProxyAssignments(targets []balanceAccount, candidates []string, baseline map[string]int64, maxPerProxy int64) balanceResult {
	result := balanceResult{
		assignments: make(map[int64]string),
		load:        make(map[string]int64, len(candidates)),
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, url := range candidates {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		candidateSet[url] = struct{}{}
		result.load[url] = baseline[url]
	}
	if len(candidateSet) == 0 {
		result.skipped = len(targets)
		return result
	}

	sorted := append([]balanceAccount(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })

	underCap := func(url string) bool {
		return maxPerProxy == 0 || result.load[url] < maxPerProxy
	}
	pickLeastLoaded := func() (string, bool) {
		best := ""
		var bestLoad int64
		for url := range candidateSet {
			if !underCap(url) {
				continue
			}
			if best == "" || result.load[url] < bestLoad || (result.load[url] == bestLoad && url < best) {
				best = url
				bestLoad = result.load[url]
			}
		}
		return best, best != ""
	}

	// First pass: keep the existing bindings that are still valid.
	pending := make([]balanceAccount, 0, len(sorted))
	for _, t := range sorted {
		if t.current != "" {
			if _, ok := candidateSet[t.current]; ok && underCap(t.current) {
				result.load[t.current]++
				result.kept++
				continue
			}
		}
		pending = append(pending, t)
	}
	// Second pass: put the remaining accounts into the proxy with the lowest load.
	for _, t := range pending {
		url, ok := pickLeastLoaded()
		if !ok {
			result.skipped++
			continue
		}
		result.assignments[t.id] = url
		result.load[url]++
	}
	return result
}

// AutoBalanceProxies evenly assigns the OAuth accounts of the given channel to the candidate proxies using "fewest bindings first".
// POST /api/admin/proxies/auto-balance
func (h *Handler) AutoBalanceProxies(c *gin.Context) {
	var req autoBalanceProxiesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "unbound"
	}
	if mode != "unbound" && mode != "all" {
		writeError(c, http.StatusBadRequest, "mode only supports unbound / all")
		return
	}
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	if channel != "" && channel != database.UpstreamChannelCodex {
		writeError(c, http.StatusBadRequest, "channel only supports codex / empty")
		return
	}
	if req.MaxPerProxy < 0 {
		writeError(c, http.StatusBadRequest, "max_per_proxy cannot be negative")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	proxies, err := h.candidateProxiesForBalance(ctx, req.ProxyIDs)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(proxies) == 0 {
		writeError(c, http.StatusBadRequest, "No proxy available (must be enabled and not failing the connectivity test)")
		return
	}

	rows, err := h.db.ListActiveByChannel(ctx, channel)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// The set of accounts to assign only includes refreshable OAuth accounts. unbound mode picks only unbound ones;
	// all mode reshuffles all OAuth accounts; non-OAuth accounts do not take part, but their existing bindings still count toward baseline.
	targets := selectOAuthProxyBalanceTargets(rows, mode)
	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No OAuth accounts need assignment", "assigned": 0, "kept": 0, "skipped": 0})
		return
	}

	// Baseline load = total bindings minus the contribution of the target accounts (their bindings are counted again inside the algorithm).
	candidates := make([]string, 0, len(proxies))
	for _, p := range proxies {
		candidates = append(candidates, strings.TrimSpace(p.URL))
	}
	allCounts, err := h.db.CountAccountsByProxyURL(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	baseline := make(map[string]int64, len(candidates))
	for _, url := range candidates {
		baseline[url] = allCounts[url]
	}
	for _, t := range targets {
		if t.current == "" {
			continue
		}
		if _, ok := baseline[t.current]; ok {
			baseline[t.current]--
		}
	}

	result := computeProxyAssignments(targets, candidates, baseline, int64(req.MaxPerProxy))

	if len(result.assignments) > 0 {
		if err := h.db.SetAccountProxyURLs(ctx, result.assignments); err != nil {
			writeInternalError(c, err)
			return
		}
		for id, url := range result.assignments {
			h.store.ApplyAccountProxyURL(id, url)
		}
	}

	distribution := make([]autoBalanceDistribution, 0, len(proxies))
	for _, p := range proxies {
		distribution = append(distribution, autoBalanceDistribution{
			ProxyID:    p.ID,
			Label:      p.Label,
			BoundCount: result.load[strings.TrimSpace(p.URL)],
		})
	}
	sort.Slice(distribution, func(i, j int) bool { return distribution[i].ProxyID < distribution[j].ProxyID })

	security.SecurityAuditLog("PROXY_AUTO_BALANCE", fmt.Sprintf(
		"channel=%s auth=oauth mode=%s assigned=%d kept=%d skipped=%d proxies=%d cap=%d ip=%s",
		channel, mode, len(result.assignments), result.kept, result.skipped, len(proxies), req.MaxPerProxy, c.ClientIP()))

	c.JSON(http.StatusOK, gin.H{
		"message":      "OAuth account balanced binding completed",
		"assigned":     len(result.assignments),
		"kept":         result.kept,
		"skipped":      result.skipped,
		"proxies_used": len(proxies),
		"distribution": distribution,
	})
}

// candidateProxiesForBalance returns the proxies taking part in balanced assignment: enabled and not failing the connectivity test;
// when ids is non-empty, the scope is limited to them (disabled or failing ones are still filtered out).
func (h *Handler) candidateProxiesForBalance(ctx context.Context, ids []int64) ([]*database.ProxyRow, error) {
	var proxies []*database.ProxyRow
	var err error
	if len(ids) > 0 {
		proxies, err = h.db.ListProxiesByIDs(ctx, ids)
	} else {
		proxies, err = h.db.ListProxies(ctx)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*database.ProxyRow, 0, len(proxies))
	for _, p := range proxies {
		if !p.Enabled || p.TestStatus == "error" || strings.TrimSpace(p.URL) == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
