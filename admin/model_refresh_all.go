package admin

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// The single entry point for the model catalog page's "refresh account models": it refreshes all channels at once and writes the models each channel finds
// back to the source of truth (Codex → model_registry),
// so the catalog page, /v1/models and scheduling admission all see the same list.
//
// Probing samples by plan group: within a channel, accounts are first grouped by subscription plan (plan_type),
// and one account is picked at random from each group that has accounts to fetch the list upstream: accounts on the same plan see the same list,
// Fetching for each of 10,000 Pro accounts would only multiply upstream requests. The sampled result is written to the shared registry (Codex).

const (
	modelRefreshAllTimeout       = 120 * time.Second
	modelRefreshChannelWorkers   = 2
	modelRefreshUnknownPlanLabel = "unknown"
)

// modelRefreshChannelOrder fixes the channel display order, matching the group order on the pricing page.
var modelRefreshChannelOrder = []string{
	database.UpstreamChannelCodex,
}

// channelModelRefreshResult is the refresh result for a single channel.
type channelModelRefreshResult struct {
	Channel   string   `json:"channel"`
	Groups    int      `json:"groups"`          // number of plan groups probed (one account sampled per group)
	Refreshed int      `json:"refreshed"`       // number of probes successfully refreshed (written back); for Codex, the official page sync counts as 1
	Failed    int      `json:"failed"`          // number of probes that failed to fetch or write back
	Added     []string `json:"added"`           // models that newly appeared in the catalog in this run
	Error     string   `json:"error,omitempty"` // channel-level failure reason (probe-level failures are only counted)
}

type refreshAllModelsResponse struct {
	Type       string                      `json:"type"` // complete: shares one struct with the SSE events
	Message    string                      `json:"message"`
	Channels   []channelModelRefreshResult `json:"channels"`
	Added      []string                    `json:"added"`
	ModelCount int                         `json:"model_count"`
	DurationMs int64                       `json:"duration_ms"`
}

// modelRefreshEvent is an SSE progress event (type=start|progress).
type modelRefreshEvent struct {
	Type         string   `json:"type"`
	Channel      string   `json:"channel"`
	Groups       int      `json:"groups,omitempty"`  // start: number of groups to probe for this channel
	Current      int      `json:"current,omitempty"` // progress: number of probes completed for this channel (including this one)
	Total        int      `json:"total,omitempty"`   // progress: total number of probes for this channel
	Plan         string   `json:"plan,omitempty"`
	Members      int      `json:"members,omitempty"` // number of accounts in this plan group
	AccountID    int64    `json:"account_id,omitempty"`
	AccountEmail string   `json:"account_email,omitempty"`
	Status       string   `json:"status,omitempty"` // ok | failed
	Message      string   `json:"message,omitempty"`
	Error        string   `json:"error,omitempty"`
	ModelCount   int      `json:"model_count,omitempty"`
	Added        []string `json:"added,omitempty"`
}

// modelRefreshEmitter receives progress events; it is a no-op in JSON mode.
type modelRefreshEmitter func(event modelRefreshEvent)

// channelModelRefreshFunc is the single-channel refresh implementation; tests can inject one through Handler.modelRefreshFuncs.
type channelModelRefreshFunc func(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult

// RefreshAllModels refreshes the usable models of all channels in parallel (POST /api/admin/models/refresh-all).
// With ?stream=1, per-probe progress is pushed over SSE, and the last event is a type=complete summary;
// otherwise the summary JSON is returned directly. A failure in one channel does not affect the writes of other channels;
// a single channel's failure is reported in that channel's error field, and the overall response is still 200.
func (h *Handler) RefreshAllModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), modelRefreshAllTimeout)
	defer cancel()
	if c.Query("stream") != "1" {
		c.JSON(http.StatusOK, h.runRefreshAllModels(ctx, nil))
		return
	}
	setupSSE(c)
	var writeMu sync.Mutex
	emit := func(event modelRefreshEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		sendSSEJSON(c, event)
	}
	summary := h.runRefreshAllModels(ctx, emit)
	writeMu.Lock()
	sendSSEJSON(c, summary)
	writeMu.Unlock()
}

func (h *Handler) runRefreshAllModels(ctx context.Context, emit modelRefreshEmitter) refreshAllModelsResponse {
	started := time.Now()
	if emit == nil {
		emit = func(modelRefreshEvent) {}
	}
	funcs := h.modelRefreshFuncs
	if funcs == nil {
		funcs = h.defaultModelRefreshFuncs()
	}

	results := make(map[string]channelModelRefreshResult, len(funcs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for channel, fn := range funcs {
		wg.Add(1)
		go func(channel string, fn channelModelRefreshFunc) {
			defer wg.Done()
			result := runChannelModelRefresh(ctx, channel, fn, emit)
			mu.Lock()
			results[channel] = result
			mu.Unlock()
		}(channel, fn)
	}
	wg.Wait()

	resp := refreshAllModelsResponse{
		Type:     "complete",
		Message:  "Refreshed usable models for all channels",
		Channels: make([]channelModelRefreshResult, 0, len(results)),
		Added:    make([]string, 0),
	}
	for _, channel := range orderedModelRefreshChannels(results) {
		result := results[channel]
		if result.Added == nil {
			result.Added = []string{}
		}
		resp.Channels = append(resp.Channels, result)
		resp.Added = append(resp.Added, result.Added...)
	}
	sort.Strings(resp.Added)
	resp.ModelCount = len(h.modelPricingCatalogKeys(ctx))
	resp.DurationMs = time.Since(started).Milliseconds()
	return resp
}

// runChannelModelRefresh ensures that a panic or timeout in one channel does not drag down the whole response.
func runChannelModelRefresh(ctx context.Context, channel string, fn channelModelRefreshFunc, emit modelRefreshEmitter) (result channelModelRefreshResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = channelModelRefreshResult{Channel: channel, Error: fmt.Sprintf("panic: %v", recovered)}
		}
		result.Channel = channel
		if result.Error == "" && ctx.Err() != nil && result.Refreshed == 0 {
			result.Error = ctx.Err().Error()
		}
	}()
	return fn(ctx, emit)
}

func orderedModelRefreshChannels(results map[string]channelModelRefreshResult) []string {
	ordered := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, channel := range modelRefreshChannelOrder {
		if _, ok := results[channel]; ok {
			ordered = append(ordered, channel)
			seen[channel] = struct{}{}
		}
	}
	extras := make([]string, 0)
	for channel := range results {
		if _, ok := seen[channel]; !ok {
			extras = append(extras, channel)
		}
	}
	sort.Strings(extras)
	return append(ordered, extras...)
}

func (h *Handler) defaultModelRefreshFuncs() map[string]channelModelRefreshFunc {
	return map[string]channelModelRefreshFunc{
		database.UpstreamChannelCodex: h.refreshCodexChannelModels,
	}
}

// modelPricingCatalogKeys returns all canonical model keys currently shown on the pricing page and model catalog (concatenated across channels),
// matching the scope of ListModelPricing.
func (h *Handler) modelPricingCatalogKeys(ctx context.Context) []string {
	return modelPricingManagementKeys(h.codexPricingModelIDs(ctx))
}

// newlyAddedModels returns the models present in after but not in before (case-insensitive), sorted.
func newlyAddedModels(before, after []string) []string {
	known := make(map[string]struct{}, len(before))
	for _, id := range before {
		known[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	added := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range after {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := known[key]; ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		added = append(added, strings.TrimSpace(id))
	}
	sort.Strings(added)
	return added
}

// ==================== Plan group sampling ====================

// modelRefreshPlanGroup is one plan group: Sample is the account picked to probe, and Members is every account in the group.
type modelRefreshPlanGroup struct {
	Plan    string
	Sample  *auth.Account
	Members []*auth.Account
}

// modelRefreshPlanKey normalizes an account's plan name into the group key: an empty plan goes into unknown,
// with no further merging (pro / pro-5x / pro-20x / api … each form their own group).
func modelRefreshPlanKey(account *auth.Account) string {
	plan := auth.NormalizePlanType(account.GetPlanType())
	if plan == "" {
		return modelRefreshUnknownPlanLabel
	}
	return plan
}

// modelRefreshSchedulable excludes disabled and errored accounts, so bad accounts are not used for probing.
func modelRefreshSchedulable(account *auth.Account) bool {
	if account == nil || atomic.LoadInt32(&account.Disabled) != 0 {
		return false
	}
	account.Mu().RLock()
	status := account.Status
	account.Mu().RUnlock()
	return status != auth.StatusError
}

// groupAccountsByPlan groups accounts by plan and picks one account at random from each group. Only groups that have accounts appear.
func groupAccountsByPlan(accounts []*auth.Account, include func(*auth.Account) bool, pick func(n int) int) []modelRefreshPlanGroup {
	byPlan := make(map[string][]*auth.Account)
	for _, account := range accounts {
		if !modelRefreshSchedulable(account) || (include != nil && !include(account)) {
			continue
		}
		plan := modelRefreshPlanKey(account)
		byPlan[plan] = append(byPlan[plan], account)
	}
	plans := make([]string, 0, len(byPlan))
	for plan := range byPlan {
		plans = append(plans, plan)
	}
	sort.Strings(plans)
	if pick == nil {
		pick = rand.Intn
	}
	groups := make([]modelRefreshPlanGroup, 0, len(plans))
	for _, plan := range plans {
		members := byPlan[plan]
		groups = append(groups, modelRefreshPlanGroup{Plan: plan, Sample: members[pick(len(members))], Members: members})
	}
	return groups
}

func (h *Handler) planGroupsFor(include func(*auth.Account) bool) []modelRefreshPlanGroup {
	if h == nil || h.store == nil {
		return nil
	}
	return groupAccountsByPlan(h.store.Accounts(), include, nil)
}

func accountEmailForEvent(account *auth.Account) string {
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return strings.TrimSpace(account.Email)
}

// probePlanGroups probes group by group: each group calls probe once (with concurrent workers), and each group's result is emitted as a progress event.
func (h *Handler) probePlanGroups(ctx context.Context, channel string, groups []modelRefreshPlanGroup, result *channelModelRefreshResult, emit modelRefreshEmitter, probe func(ctx context.Context, group modelRefreshPlanGroup) (modelCount int, added []string, err error)) {
	result.Groups = len(groups)
	emit(modelRefreshEvent{Type: "start", Channel: channel, Groups: len(groups)})
	if len(groups) == 0 {
		return
	}
	jobs := make(chan modelRefreshPlanGroup)
	var mu sync.Mutex
	var wg sync.WaitGroup
	done := 0
	for i := 0; i < modelRefreshChannelWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for group := range jobs {
				var (
					modelCount int
					added      []string
					err        error
				)
				if ctx.Err() != nil {
					err = ctx.Err()
				} else {
					modelCount, added, err = probe(ctx, group)
				}
				mu.Lock()
				done++
				event := modelRefreshEvent{
					Type: "progress", Channel: channel, Current: done, Total: len(groups),
					Plan: group.Plan, Members: len(group.Members),
					AccountID: group.Sample.ID(), AccountEmail: accountEmailForEvent(group.Sample),
					ModelCount: modelCount, Added: added,
				}
				if err != nil {
					result.Failed++
					event.Status = "failed"
					event.Error = err.Error()
				} else {
					result.Refreshed++
					event.Status = "ok"
				}
				mu.Unlock()
				emit(event)
			}
		}()
	}
	for _, group := range groups {
		jobs <- group
	}
	close(jobs)
	wg.Wait()
}

// ==================== Codex ====================

// isCodexOAuthAccount reports whether the account is an official Codex account from ChatGPT OAuth
// Once other upstreams are removed, every non-empty account is a Codex account.
func isCodexOAuthAccount(account *auth.Account) bool {
	return account != nil
}

func (h *Handler) refreshCodexChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelCodex, Added: []string{}}
	if h == nil || h.db == nil {
		result.Error = "Database unavailable"
		return result
	}
	before := proxy.SupportedModelIDs(ctx, h.db)

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	// 1. Official model page → registry (the same implementation as "sync upstream models" on the settings page).
	if _, err := proxy.SyncOfficialCodexModels(ctx, h.db, proxyURL); err != nil {
		result.Error = fmt.Sprintf("Official model page sync failed: %v", err)
		result.Failed++
		emit(modelRefreshEvent{Type: "progress", Channel: database.UpstreamChannelCodex, Plan: "official_docs", Status: "failed", Error: err.Error()})
	} else {
		result.Refreshed++
		emit(modelRefreshEvent{Type: "progress", Channel: database.UpstreamChannelCodex, Plan: "official_docs", Status: "ok",
			ModelCount: len(proxy.SupportedModelIDs(ctx, h.db)), Added: newlyAddedModels(before, proxy.SupportedModelIDs(ctx, h.db))})
	}

	// 2. Sample one account per plan type to fetch the upstream list → registry (add only, never change or delete; reuses LearnModelsFromManifest).
	now := time.Now().UTC()
	h.probePlanGroups(ctx, database.UpstreamChannelCodex, h.planGroupsFor(isCodexOAuthAccount), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			manifest, err := proxy.FetchCodexModelsManifest(ctx, group.Sample, h.store.ResolveProxyForAccount(group.Sample), "", "", nil)
			if err != nil {
				return 0, nil, err
			}
			if err := manifest.SaveCapabilities(ctx, h.db); err != nil {
				return 0, nil, err
			}
			proxy.RecordResponsesLiteSupportFromManifest(manifest.Body)
			added, err := proxy.LearnModelsFromManifest(ctx, h.db, manifest.Body, now)
			if err != nil {
				return 0, nil, err
			}
			return len(proxy.ExtractManifestModelSlugs(manifest.Body)), added, nil
		})

	result.Added = newlyAddedModels(before, proxy.SupportedModelIDs(ctx, h.db))
	return result
}
