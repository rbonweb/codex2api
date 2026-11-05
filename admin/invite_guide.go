package admin

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// The cap on accounts probed automatically after import. Each account is one request to a Cloudflare-protected endpoint, and a 10,000-scale import
// probed in full would hit the upstream for hours and significantly raise the chance of a managed challenge. The excess is not
// silently dropped: the dialog shows "sampled 50/2000", and a user who wants the full data can click continue explicitly.
const inviteGuideProbeCap = 50

// The number of accounts a single "continue probing" may add; these are also queued through the gate.
const inviteGuideProbeBatch = 50

// inviteGuidePlanState is the state of a single account in the plan.
const (
	inviteGuideStatePending    = "pending"    // not yet probed
	inviteGuideStateEligible   = "eligible"   // eligible and still has reward count left
	inviteGuideStateExhausted  = "exhausted"  // eligible, but this month's reward count is used up
	inviteGuideStateIneligible = "ineligible" // the upstream explicitly ruled it ineligible
)

// inviteGuideAccountPlan is the invite earnings assessment for a single account.
type inviteGuideAccountPlan struct {
	ID       int64  `json:"id"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	State    string `json:"state"`

	// remaining_* pass the upstream semantics through pointers: nil = the upstream did not give this field (unknown),
	// 0 = the upstream explicitly said none is left. The two are handled completely differently, so they must not both collapse to 0.
	RemainingSendCapacity   *int `json:"remaining_send_capacity,omitempty"`
	RemainingRewardCapacity *int `json:"remaining_reward_capacity,omitempty"`

	// GrantAmount is the amount the inviter (referrer) gets per invite, excluding the invitee's share.
	GrantAmount      float64 `json:"grant_amount,omitempty"`
	PotentialCredits float64 `json:"potential_credits"`
	OfferID          string  `json:"offer_id,omitempty"`
	Title            string  `json:"title,omitempty"`
	IneligibleReason string  `json:"ineligible_reason,omitempty"`

	// This month's send usage, from the send rule in time_frame_rules. It is not
	// the same window as the Invites* fields below: this is a "month", while those are the 90-day tracking endpoint; mixing them would give wrong numbers.
	MonthlySent      *int `json:"monthly_sent,omitempty"`
	MonthlySendTotal *int `json:"monthly_send_total,omitempty"`

	// Actual invite records from the last 90 days, from the tracking snapshot. The pointers distinguish "no tracking data" from
	// "really 0". The import probes eligibility only, not records, so for most accounts there is no data here.
	InvitesSent     *int `json:"invites_sent,omitempty"`
	InvitesAccepted *int `json:"invites_accepted,omitempty"`
	InvitesPending  *int `json:"invites_pending,omitempty"`

	// SuggestedInvites is the allocation suggestion: how many invites this account should send. When no emails budget is passed it equals
	// the remaining reward count; when a budget is passed it is allocated greedily, with "higher per-invite earnings first".
	SuggestedInvites int        `json:"suggested_invites"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
}

// inviteTrackingCounts summarises the sent, accepted and in-flight counts in one tracking snapshot.
// Observed status values: redeemed (already used) and expired (sent, but the invitee did not use it within the validity period).
// expired counts neither as accepted nor as in-flight, but it is still counted as "sent".
func inviteTrackingCounts(items []proxy.CodexInviteTrackingItem) (sent, accepted, pending int) {
	sent = len(items)
	for _, item := range items {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "redeemed", "accepted":
			accepted++
		case "pending", "sent":
			pending++
		}
	}
	return sent, accepted, pending
}

// findSendCapacityRule gets the monthly quota rule for the send dimension (send count, which is different from the reward count).
func findSendCapacityRule(rules []proxy.CodexInviteTimeFrameRule) *proxy.CodexInviteTimeFrameRule {
	for i := range rules {
		if strings.EqualFold(strings.TrimSpace(rules[i].CapacityType), "send") {
			return &rules[i]
		}
	}
	return nil
}

// inviteGuidePlanResponse is the full data for the guide dialog.
type inviteGuidePlanResponse struct {
	Enabled bool `json:"enabled"`

	Total     int `json:"total"`     // total accounts requested for assessment
	Probed    int `json:"probed"`    // accounts that have a probe result
	Pending   int `json:"pending"`   // accounts still queued or being probed
	Unprobed  int `json:"unprobed"`  // accounts never queued at all because of the cap
	Eligible  int `json:"eligible"`  // accounts that are eligible and have reward count left
	ProbeCap  int `json:"probe_cap"` // automatic probe cap; the frontend uses it to explain "sampled"
	RewardCap int `json:"total_reward_slots"`

	TotalPotentialCredits float64 `json:"total_potential_credits"`
	// EmailBudget echoes the inviter email budget in the request (0 = unlimited).
	EmailBudget int                      `json:"email_budget"`
	Accounts    []inviteGuideAccountPlan `json:"accounts"`
}

// lookupInviteAccount gets an account by DBID.
//
// It deliberately does not use h.findAccountByID: that implementation copies the whole account pool with h.store.Accounts() and then scans it linearly,
// whereas this request looks up hundreds of IDs (one screen of the invite page dropdown is 100 accounts). With a pool at the 10,000 level,
// that means hundreds of full slice copies plus millions of comparisons. store.FindByID uses the DBID index, O(1).
func (h *Handler) lookupInviteAccount(id int64) *auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	return h.store.FindByID(id)
}

// inviteGuideCandidate decides whether an account can be an invite sender. It matches the frontend isCodexInviteCandidate
// on the same basis: an AT-only account has no lasting Codex OAuth credential that can be used for referrals.
// enabled/locked/status are not part of the decision: they are only scheduling switches and do not affect whether the access token is usable.
func inviteGuideCandidate(acc *auth.Account) bool {
	if acc == nil || acc.GetAccessToken() == "" {
		return false
	}
	return acc.RefreshToken != ""
}

// referrerGrantAmount takes the reward the inviter gets. The upstream issues two grants per invite:
// recipient=referrer is the inviter (our earnings), recipient=recipient is the invitee. Filtering must be done by
// recipient, otherwise the invitee's share would also be counted as our earnings and the estimate would exactly double.
func referrerGrantAmount(grants []proxy.CodexInviteGrant) float64 {
	for _, g := range grants {
		if strings.EqualFold(strings.TrimSpace(g.Recipient), "referrer") {
			return g.Amount
		}
	}
	// When the upstream does not mark a recipient: with a single grant, that one is used; with several, there is no way to tell them apart, so nothing is guessed.
	if len(grants) == 1 {
		return grants[0].Amount
	}
	return 0
}

// probeInviteEligibilityForGuide probes the invite eligibility of a single account and writes it to the snapshot.
// When includeTracking is true, it also fetches the sent invite records (the only source of the sent/accepted counts).
//
// The import path deliberately passes false: tracking records have no effect on the ordering of "which account to send from first", and doubling the upstream requests of the import for them is not worth it.
// The user passes true only when they explicitly click "probe credits" in the dropdown.
// Failures are only logged: the guide is a nice-to-have; if it cannot be probed it shows as unknown, and the import itself is not affected.
func (h *Handler) probeInviteEligibilityForGuide(ctx context.Context, accountID int64, includeTracking bool) {
	account := h.lookupInviteAccount(accountID)
	if !inviteGuideCandidate(account) {
		return
	}

	programID, entrypoint := proxy.NormalizeInviteProgram("", "")
	scope := inviteEligibilityScope(programID, entrypoint)
	generation := account.GetCredentialGeneration()

	// If there is an unexpired snapshot, do not hit the upstream again: the same account may be triggered by several imports, or may have just been checked on
	// the invite page. This step makes the whole guide feature cost zero upstream requests while the cache is warm.
	var cached proxy.CodexInviteEligibility
	if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
		database.CodexInviteSnapshotEligibility, accountID, generation, scope, &cached); meta != nil {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := proxy.QueryCodexInviteEligibility(probeCtx, account,
		h.store.ResolveProxyForAccount(account), programID, entrypoint)
	if err != nil {
		log.Printf("invite eligibility probe failed: account=%d err=%v", accountID, err)
		return
	}
	// Same rule as the invite page: a challenge page borrows a 403, and caching it would turn one block into a permanent "ineligible".
	if result.OK && !result.Challenged {
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			accountID, generation, scope, result.StatusCode, inviteEligibilitySnapshotTTL, result)
	}
	if !includeTracking {
		return
	}

	// The tracking records run after eligibility, not concurrently: the upstream sits behind Cloudflare bot management, and the first request
	// gets a __cf_bm that makes the second one less likely to be challenged; this matches the fetch order of the invite page.
	period, limit := proxy.NormalizeInviteTracking("", 0)
	trackingScope := inviteTrackingScope(programID, period, limit)
	var cachedTracking proxy.CodexInviteTracking
	if h.readInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
		accountID, generation, trackingScope, &cachedTracking) != nil {
		h.rememberInviteTrackingRecipients(ctx, accountID, programID, entrypoint, &cachedTracking)
		return
	}
	trackingCtx, trackingCancel := context.WithTimeout(ctx, 30*time.Second)
	defer trackingCancel()
	tracking, err := proxy.QueryCodexInviteTracking(trackingCtx, account,
		h.store.ResolveProxyForAccount(account), programID, period, limit)
	if err != nil {
		log.Printf("invite record probe failed: account=%d err=%v", accountID, err)
		return
	}
	if tracking.OK && !tracking.Challenged {
		h.rememberInviteTrackingRecipients(ctx, accountID, programID, entrypoint, tracking)
		h.writeInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			accountID, generation, trackingScope, tracking.StatusCode, inviteTrackingSnapshotTTL, tracking)
	}
}

// enqueueInviteGuideProbes queues accounts into the import probe gate. It returns the number actually queued and the number blocked by the cap.
// Reusing runImportProbeTask is the key: it carries an adaptive concurrency permit, so a large import does not flood the upstream in an instant.
func (h *Handler) enqueueInviteGuideProbes(ids []int64, limit int, includeTracking bool) (queued int, skipped int) {
	if h == nil || limit <= 0 {
		return 0, len(ids)
	}
	for _, id := range ids {
		if queued >= limit {
			return queued, len(ids) - queued
		}
		accountID := id
		h.runImportProbeTask(func(ctx context.Context) {
			h.probeInviteEligibilityForGuide(ctx, accountID, includeTracking)
		})
		queued++
	}
	return queued, 0
}

// scheduleInviteGuideProbes is the entry point of the import path: it filters out the Codex accounts that can invite and queues them for probing.
// When the switch is off it returns straight away and sends no upstream request at all.
func (h *Handler) scheduleInviteGuideProbes(ctx context.Context, ids []int64) {
	if h == nil || len(ids) == 0 || !h.inviteGuideEnabled(ctx) {
		return
	}
	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		if inviteGuideCandidate(h.lookupInviteAccount(id)) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return
	}
	queued, skipped := h.enqueueInviteGuideProbes(candidates, inviteGuideProbeCap, false)
	log.Printf("invite guide: imported %d accounts, queued %d for probing, skipped %d because of the cap", len(ids), queued, skipped)
}

// inviteGuideEnabled reads the switch. When it cannot be read it defaults to on: the guide being visible by default is the product's intent,
// and a single database hiccup should not silently turn the feature off.
func (h *Handler) inviteGuideEnabled(ctx context.Context) bool {
	if h == nil || h.db == nil {
		return database.InviteGuideDefaultEnabled
	}
	cfg, err := h.db.LoadInviteGuideConfig(ctx)
	if err != nil {
		log.Printf("failed to read the invite guide setting, treating it as enabled by default: %v", err)
		return database.InviteGuideDefaultEnabled
	}
	return cfg.IsEnabled()
}

// buildInviteGuidePlan summarises the assessment results by account ID and sorts them into the suggested order.
func (h *Handler) buildInviteGuidePlan(ctx context.Context, ids []int64, emailBudget int) inviteGuidePlanResponse {
	programID, entrypoint := proxy.NormalizeInviteProgram("", "")
	scope := inviteEligibilityScope(programID, entrypoint)
	trackingPeriod, trackingLimit := proxy.NormalizeInviteTracking("", 0)
	trackingScope := inviteTrackingScope(programID, trackingPeriod, trackingLimit)

	resp := inviteGuidePlanResponse{
		Enabled:     h.inviteGuideEnabled(ctx),
		ProbeCap:    inviteGuideProbeCap,
		EmailBudget: emailBudget,
		Accounts:    make([]inviteGuideAccountPlan, 0, len(ids)),
	}

	for _, id := range ids {
		account := h.lookupInviteAccount(id)
		if !inviteGuideCandidate(account) {
			continue
		}
		resp.Total++

		item := inviteGuideAccountPlan{
			ID:       id,
			Email:    account.Email,
			PlanType: account.PlanType,
			State:    inviteGuideStatePending,
		}

		var elig proxy.CodexInviteEligibility
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, id, account.GetCredentialGeneration(), scope, &elig)
		if meta == nil {
			resp.Pending++
			resp.Accounts = append(resp.Accounts, item)
			continue
		}

		resp.Probed++
		if meta.ObservedAt != nil {
			item.ObservedAt = meta.ObservedAt
		}
		item.RemainingSendCapacity = elig.RemainingSendCapacity
		item.RemainingRewardCapacity = elig.RemainingRewardCapacity
		item.OfferID = elig.OfferID
		item.Title = elig.Title
		item.GrantAmount = referrerGrantAmount(elig.Grants)
		if rule := findSendCapacityRule(elig.TimeFrameRules); rule != nil {
			sent, total := rule.InvitesSent, rule.InvitesTotal
			item.MonthlySent, item.MonthlySendTotal = &sent, &total
		}

		// Sent/accepted come from the tracking snapshot. Read the cache only; if there is nothing, leave them empty: the import probe only writes eligibility,
		// so most accounts have no data here anyway, and inventing a 0 would be read as "none sent yet".
		var tracking proxy.CodexInviteTracking
		if h.readInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			id, account.GetCredentialGeneration(), trackingScope, &tracking) != nil {
			h.rememberInviteTrackingRecipients(ctx, id, programID, entrypoint, &tracking)
			sent, accepted, pending := inviteTrackingCounts(tracking.Items)
			item.InvitesSent, item.InvitesAccepted, item.InvitesPending = &sent, &accepted, &pending
		}

		switch {
		case !elig.ShouldShow:
			item.State = inviteGuideStateIneligible
			item.IneligibleReason = elig.IneligibleReason
			if item.IneligibleReason == "" {
				item.IneligibleReason = elig.IneligibleReasonCode
			}
		case elig.RemainingRewardCapacity != nil && *elig.RemainingRewardCapacity == 0:
			// Can still send, but a send earns no reward; for the goal of "building up credits" that is worth nothing.
			item.State = inviteGuideStateExhausted
		default:
			item.State = inviteGuideStateEligible
			resp.Eligible++
			if elig.RemainingRewardCapacity != nil {
				item.SuggestedInvites = *elig.RemainingRewardCapacity
				resp.RewardCap += item.SuggestedInvites
			}
			item.PotentialCredits = item.GrantAmount * float64(item.SuggestedInvites)
		}

		resp.Accounts = append(resp.Accounts, item)
	}

	sortInviteGuidePlan(resp.Accounts)
	applyInviteGuideEmailBudget(resp.Accounts, emailBudget)

	resp.TotalPotentialCredits = 0
	for i := range resp.Accounts {
		resp.TotalPotentialCredits += resp.Accounts[i].PotentialCredits
	}
	return resp
}

// sortInviteGuidePlan sorts "which account to send from first": sendable ones come first, then by per-invite earnings,
// remaining reward count and estimated total earnings, all descending. Per-invite earnings come before remaining count: when emails are scarce,
// the best use of one email is on the account that pays more per invite.
func sortInviteGuidePlan(items []inviteGuideAccountPlan) {
	rank := map[string]int{
		inviteGuideStateEligible:   0,
		inviteGuideStatePending:    1,
		inviteGuideStateExhausted:  2,
		inviteGuideStateIneligible: 3,
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if a.GrantAmount != b.GrantAmount {
			return a.GrantAmount > b.GrantAmount
		}
		if a.SuggestedInvites != b.SuggestedInvites {
			return a.SuggestedInvites > b.SuggestedInvites
		}
		return a.ID < b.ID
	})
}

// applyInviteGuideEmailBudget allocates greedily when invitee emails are limited. The list is already sorted by per-invite earnings, descending,
// so handing the budget out in order to the front accounts is optimal: total earnings = Σ(per-invite earnings), taking the top N slots by value.
func applyInviteGuideEmailBudget(items []inviteGuideAccountPlan, budget int) {
	if budget <= 0 {
		return
	}
	remaining := budget
	for i := range items {
		if items[i].State != inviteGuideStateEligible {
			continue
		}
		if remaining <= 0 {
			items[i].SuggestedInvites = 0
			items[i].PotentialCredits = 0
			continue
		}
		if items[i].SuggestedInvites > remaining {
			items[i].SuggestedInvites = remaining
		}
		remaining -= items[i].SuggestedInvites
		items[i].PotentialCredits = items[i].GrantAmount * float64(items[i].SuggestedInvites)
	}
}

// parseInviteGuideIDs parses comma-separated account IDs, deduplicating them and keeping their order.
func parseInviteGuideIDs(raw string) []int64 {
	seen := make(map[int64]struct{})
	ids := make([]int64, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// GetInviteGuidePlan summarises the invite earnings assessment for a batch of accounts.
// GET /api/admin/accounts/invite/plan?ids=1,2,3&emails=5
func (h *Handler) GetInviteGuidePlan(c *gin.Context) {
	ids := parseInviteGuideIDs(c.Query("ids"))
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "missing account ID")
		return
	}
	budget, _ := strconv.Atoi(strings.TrimSpace(c.Query("emails")))
	c.JSON(http.StatusOK, h.buildInviteGuidePlan(c.Request.Context(), ids, budget))
}

// ProbeInviteGuidePlan probes the specified accounts further (the "continue probing the rest" in the dialog).
// POST /api/admin/accounts/invite/plan/probe  {"ids":[1,2,3]}
func (h *Handler) ProbeInviteGuidePlan(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}
	candidates := make([]int64, 0, len(req.IDs))
	for _, id := range req.IDs {
		if inviteGuideCandidate(h.lookupInviteAccount(id)) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		writeError(c, http.StatusBadRequest, "no Codex accounts can be probed")
		return
	}
	queued, skipped := h.enqueueInviteGuideProbes(candidates, inviteGuideProbeBatch, true)
	c.JSON(http.StatusOK, gin.H{"queued": queued, "skipped": skipped})
}

// GetInviteGuideSettings returns the guide dialog switch.
// GET /api/admin/settings/invite-guide
func (h *Handler) GetInviteGuideSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"enabled": h.inviteGuideEnabled(c.Request.Context())})
}

// UpdateInviteGuideSettings updates the guide dialog switch. The "don't show again" option in the dialog sends its change here.
// PUT /api/admin/settings/invite-guide  {"enabled":false}
func (h *Handler) UpdateInviteGuideSettings(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "missing enabled field")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if err := h.db.SaveInviteGuideConfig(c.Request.Context(),
		database.InviteGuideConfig{Enabled: req.Enabled}); err != nil {
		writeError(c, http.StatusInternalServerError, "failed to save: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}
