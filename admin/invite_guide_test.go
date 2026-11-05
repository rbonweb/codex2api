package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func intPtr(v int) *int { return &v }

// The upstream issues two grants per invite, one for the inviter and one for the invitee, with the same amount. Picking the wrong one doubles the estimate.
func TestReferrerGrantAmount(t *testing.T) {
	t.Run("picks referrer not recipient", func(t *testing.T) {
		// The shape is taken from a real observed response (offer credits_1000).
		grants := []proxy.CodexInviteGrant{
			{Recipient: "referrer", Amount: 1000, GrantType: "personal_credits"},
			{Recipient: "recipient", Amount: 1000, GrantType: "personal_credits"},
		}
		if got := referrerGrantAmount(grants); got != 1000 {
			t.Fatalf("got %v, want 1000", got)
		}
	})

	t.Run("asymmetric amounts must not take recipient", func(t *testing.T) {
		grants := []proxy.CodexInviteGrant{
			{Recipient: "recipient", Amount: 9999},
			{Recipient: "referrer", Amount: 500},
		}
		if got := referrerGrantAmount(grants); got != 500 {
			t.Fatalf("got %v, want 500 (referrer's share)", got)
		}
	})

	t.Run("single unlabeled grant is used as-is", func(t *testing.T) {
		if got := referrerGrantAmount([]proxy.CodexInviteGrant{{Amount: 250}}); got != 250 {
			t.Fatalf("got %v, want 250", got)
		}
	})

	t.Run("ambiguous multiple unlabeled grants yield zero", func(t *testing.T) {
		// When ownership cannot be told apart, do not guess: showing 0 is better than giving the user an invented earnings figure.
		grants := []proxy.CodexInviteGrant{{Amount: 100}, {Amount: 200}}
		if got := referrerGrantAmount(grants); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := referrerGrantAmount(nil); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})
}

func TestSortInviteGuidePlan(t *testing.T) {
	items := []inviteGuideAccountPlan{
		{ID: 1, State: inviteGuideStateIneligible},
		{ID: 2, State: inviteGuideStateEligible, GrantAmount: 500, SuggestedInvites: 3},
		{ID: 3, State: inviteGuideStatePending},
		{ID: 4, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 1},
		{ID: 5, State: inviteGuideStateExhausted},
		{ID: 6, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2},
	}
	sortInviteGuidePlan(items)

	got := make([]int64, len(items))
	for i := range items {
		got[i] = items[i].ID
	}
	// Sendable ones come first; among sendable ones, compare per-invite earnings first (1000 > 500), then remaining count (2 > 1).
	want := []int64{6, 4, 2, 3, 5, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// When emails are limited, giving the slots to the accounts with the highest per-invite earnings is the split that maximises total earnings.
func TestApplyInviteGuideEmailBudget(t *testing.T) {
	t.Run("greedy allocation by per-invite value", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 500, SuggestedInvites: 3},
		}
		applyInviteGuideEmailBudget(items, 3) // only 3 invitee emails

		if items[0].SuggestedInvites != 2 || items[0].PotentialCredits != 2000 {
			t.Fatalf("top account: invites=%d credits=%v, want 2/2000", items[0].SuggestedInvites, items[0].PotentialCredits)
		}
		// Only 1 slot of budget is left for the second account.
		if items[1].SuggestedInvites != 1 || items[1].PotentialCredits != 500 {
			t.Fatalf("second account: invites=%d credits=%v, want 1/500", items[1].SuggestedInvites, items[1].PotentialCredits)
		}
	})

	t.Run("budget exhausted zeroes out the tail", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2, PotentialCredits: 2000},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 900, SuggestedInvites: 5, PotentialCredits: 4500},
		}
		applyInviteGuideEmailBudget(items, 2)
		if items[1].SuggestedInvites != 0 || items[1].PotentialCredits != 0 {
			t.Fatalf("tail should be zeroed, got invites=%d credits=%v", items[1].SuggestedInvites, items[1].PotentialCredits)
		}
	})

	t.Run("zero budget means unlimited and changes nothing", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 4, PotentialCredits: 4000},
		}
		applyInviteGuideEmailBudget(items, 0)
		if items[0].SuggestedInvites != 4 || items[0].PotentialCredits != 4000 {
			t.Fatalf("unlimited budget must not modify plan: %+v", items[0])
		}
	})

	t.Run("ineligible accounts never consume budget", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateIneligible, SuggestedInvites: 0},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 100, SuggestedInvites: 3},
		}
		applyInviteGuideEmailBudget(items, 3)
		if items[1].SuggestedInvites != 3 {
			t.Fatalf("eligible account should get the whole budget, got %d", items[1].SuggestedInvites)
		}
	})
}

func TestParseInviteGuideIDs(t *testing.T) {
	got := parseInviteGuideIDs(" 3, 1 ,3,,abc,-2,0,7 ")
	want := []int64{3, 1, 7}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (dedup + order preserved)", got, want)
		}
	}
}

// "Never configured" must read as enabled; otherwise the feature would default to off, the opposite of the product's intent.
func TestInviteGuideConfigDefault(t *testing.T) {
	var unset database.InviteGuideConfig
	if !unset.IsEnabled() {
		t.Fatal("unset config must default to enabled")
	}

	off := false
	if (database.InviteGuideConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("explicit false must be honored")
	}

	on := true
	if !(database.InviteGuideConfig{Enabled: &on}).IsEnabled() {
		t.Fatal("explicit true must be honored")
	}
}

// When remaining_reward_capacity=0, the account can still send, but a send earns no credits: for the goal of "building up credits" this
// has no value, so it must be kept distinct from "eligible", or it would be ranked first and mislead the user.
func TestInviteGuideExhaustedIsNotEligible(t *testing.T) {
	items := []inviteGuideAccountPlan{
		{ID: 1, State: inviteGuideStateExhausted, RemainingSendCapacity: intPtr(5), RemainingRewardCapacity: intPtr(0)},
		{ID: 2, State: inviteGuideStateEligible, GrantAmount: 10, SuggestedInvites: 1},
	}
	sortInviteGuidePlan(items)
	if items[0].ID != 2 {
		t.Fatalf("eligible account must outrank exhausted one, got order %d,%d", items[0].ID, items[1].ID)
	}
}

// The cap is the most critical safety valve of this feature: if a 10,000-scale import had no limit, it would keep hitting a Cloudflare-protected
// upstream endpoint for hours. The blocked part must be reported honestly, so the dialog can show "sampled N/M".
func TestEnqueueInviteGuideProbesCap(t *testing.T) {
	h := &Handler{}
	ids := make([]int64, 120)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	queued, skipped := h.enqueueInviteGuideProbes(ids, inviteGuideProbeCap, false)
	if queued != inviteGuideProbeCap {
		t.Fatalf("queued = %d, want %d", queued, inviteGuideProbeCap)
	}
	if skipped != len(ids)-inviteGuideProbeCap {
		t.Fatalf("skipped = %d, want %d", skipped, len(ids)-inviteGuideProbeCap)
	}

	t.Run("under cap queues everything", func(t *testing.T) {
		q, s := h.enqueueInviteGuideProbes(ids[:10], inviteGuideProbeCap, false)
		if q != 10 || s != 0 {
			t.Fatalf("queued=%d skipped=%d, want 10/0", q, s)
		}
	})

	t.Run("non-positive limit queues nothing", func(t *testing.T) {
		q, s := h.enqueueInviteGuideProbes(ids[:5], 0, false)
		if q != 0 || s != 5 {
			t.Fatalf("queued=%d skipped=%d, want 0/5", q, s)
		}
	})
}

// created_ids is the only entry point through which the frontend fetches the plan; leaving it out means the whole guide never triggers. An empty value must be omitted,
// or old frontends would receive an extra field.
func TestImportEventCreatedIDsSerialization(t *testing.T) {
	withIDs, err := json.Marshal(importEvent{Type: "complete", CreatedIDs: []int64{7, 9}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withIDs), `"created_ids":[7,9]`) {
		t.Fatalf("created_ids missing from complete event: %s", withIDs)
	}

	empty, err := json.Marshal(importEvent{Type: "progress"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(empty), "created_ids") {
		t.Fatalf("empty created_ids must be omitted: %s", empty)
	}
}

// The status values are taken from a real tracking response: redeemed / expired. expired means "sent, but the invitee did not
// use it within the validity period": it still counts as sent, but counts neither as accepted nor as in-flight; putting it on either side would give wrong numbers.
func TestInviteTrackingCounts(t *testing.T) {
	items := []proxy.CodexInviteTrackingItem{
		{Status: "redeemed"},
		{Status: "expired"},
		{Status: "expired"},
		{Status: "pending"},
		{Status: "ACCEPTED"}, // case-insensitive
		{Status: ""},         // the upstream gave no status: counted as sent only
	}
	sent, accepted, pending := inviteTrackingCounts(items)
	if sent != 6 {
		t.Fatalf("sent = %d, want 6 (every record counts as sent)", sent)
	}
	if accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (redeemed + ACCEPTED)", accepted)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}

	t.Run("empty", func(t *testing.T) {
		s, a, p := inviteTrackingCounts(nil)
		if s != 0 || a != 0 || p != 0 {
			t.Fatalf("got %d/%d/%d, want 0/0/0", s, a, p)
		}
	})
}

// send and reward are two independent quota rules; picking the wrong one would show "sent this month 7/10" as
// "1/3" (that is the reward count).
func TestFindSendCapacityRule(t *testing.T) {
	rules := []proxy.CodexInviteTimeFrameRule{
		{CapacityType: "reward", InvitesSent: 1, InvitesTotal: 3},
		{CapacityType: "send", InvitesSent: 7, InvitesTotal: 10},
	}
	got := findSendCapacityRule(rules)
	if got == nil || got.InvitesSent != 7 || got.InvitesTotal != 10 {
		t.Fatalf("got %+v, want send rule 7/10", got)
	}
	if findSendCapacityRule(nil) != nil {
		t.Fatal("missing rules must yield nil, not a zero-valued rule")
	}
}
