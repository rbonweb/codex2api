package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func mustParseRules(t *testing.T, raw string) *PayloadRuleSet {
	t.Helper()
	rs, err := ParsePayloadRulesJSON(raw)
	if err != nil {
		t.Fatalf("ParsePayloadRulesJSON: %v", err)
	}
	return rs
}

func withPayloadRules(t *testing.T, raw string) {
	t.Helper()
	prev := CurrentPayloadRules()
	if err := SetPayloadRulesJSON(raw); err != nil {
		t.Fatalf("SetPayloadRulesJSON: %v", err)
	}
	t.Cleanup(func() {
		if prev == nil {
			prev = &PayloadRuleSet{}
		}
		currentPayloadRuleSet.Store(prev)
	})
}

const payloadTestBody = `{"model":"gpt-5.6-sol","stream":true,"instructions":"official prompt","reasoning":{"effort":"medium"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func TestPayloadRulesOverrideInstructions(t *testing.T) {
	withPayloadRules(t, `{"override":[{"models":["gpt-*"],"params":{"instructions":"my custom prompt"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "instructions").String(); got != "my custom prompt" {
		t.Fatalf("instructions = %q, want my custom prompt", got)
	}
}

func TestPayloadRulesAppendInstructions(t *testing.T) {
	withPayloadRules(t, `{"append":[{"params":{"instructions":"extra guard text"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	want := "official prompt\n\nextra guard text"
	if got := gjson.GetBytes(out, "instructions").String(); got != want {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
	// Write directly when the field is missing
	out = ApplyPayloadRulesToBody([]byte(`{"model":"gpt-5.6-sol"}`), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "instructions").String(); got != "extra guard text" {
		t.Fatalf("instructions(missing) = %q, want extra guard text", got)
	}
}

func TestPayloadRulesAppendIdempotentOnReentry(t *testing.T) {
	// When retry or continuation paths re-enter the rule engine with a body that already had the rules applied, append must not
	// stack the appended text again (it inflates the body and changes the prompt meaning). The second application must be idempotent.
	withPayloadRules(t, `{"append":[{"params":{"instructions":"extra guard text"}}]}`)
	once := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	want := "official prompt\n\nextra guard text"
	if got := gjson.GetBytes(once, "instructions").String(); got != want {
		t.Fatalf("first application instructions = %q, want %q", got, want)
	}
	twice := ApplyPayloadRulesToBody(once, "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(twice, "instructions").String(); got != want {
		t.Fatalf("instructions after re-entry = %q, want %q (appended only once)", got, want)
	}

	// Original field missing: write it directly the first time; re-entry does not repeat it
	once = ApplyPayloadRulesToBody([]byte(`{"model":"gpt-5.6-sol"}`), "gpt-5.6-sol", nil, nil)
	twice = ApplyPayloadRulesToBody(once, "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(twice, "instructions").String(); got != "extra guard text" {
		t.Fatalf("instructions(missing) after re-entry = %q, want extra guard text", got)
	}

	// Multiple append rules on the same path: after re-entry, each append block still appears only once
	withPayloadRules(t, `{"append":[{"params":{"instructions":"guard A"}},{"params":{"instructions":"guard B"}}]}`)
	once = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	twice = ApplyPayloadRulesToBody(once, "gpt-5.6-sol", nil, nil)
	want = "official prompt\n\nguard A\n\nguard B"
	if got := gjson.GetBytes(twice, "instructions").String(); got != want {
		t.Fatalf("instructions after re-entry with multiple rules = %q, want %q", got, want)
	}
}

func TestPayloadRulesConditionalEffortMapping(t *testing.T) {
	withPayloadRules(t, `{"override":[{"models":["gpt-*"],"match":{"reasoning.effort":"medium"},"params":{"reasoning.effort":"high"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high", got)
	}
	// Do not rewrite when the match gate is not satisfied
	low := []byte(strings.Replace(payloadTestBody, `"medium"`, `"low"`, 1))
	out = ApplyPayloadRulesToBody(low, "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want low (match gate not satisfied)", got)
	}
}

func TestPayloadRulesServiceTierOverride(t *testing.T) {
	withPayloadRules(t, `{"override":[{"params":{"service_tier":"priority"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority", got)
	}
}

func TestPayloadRulesServiceTierSanitizedAfterRewrite(t *testing.T) {
	// Tiers that the rules inject and upstream does not accept, such as flex/auto, must be stripped before sending (issue #395),
	// otherwise they would bypass the handler-layer sanitization and reach upstream, triggering a 400. fast should be mapped to priority.
	for tier, want := range map[string]string{"flex": "", "auto": "", "scale": "", "default": "", "fast": "priority", "priority": "priority"} {
		withPayloadRules(t, `{"override":[{"params":{"service_tier":"`+tier+`"}}]}`)
		out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
		out = sanitizeServiceTierForUpstream(out)
		if got := gjson.GetBytes(out, "service_tier").String(); got != want {
			t.Fatalf("tier %s: service_tier = %q, want %q", tier, got, want)
		}
	}
}

func TestPayloadRulesDefaultOnlyWhenMissing(t *testing.T) {
	withPayloadRules(t, `{"default":[{"params":{"text.verbosity":"low","instructions":"default prompt"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low (written when missing)", got)
	}
	if got := gjson.GetBytes(out, "instructions").String(); got != "official prompt" {
		t.Fatalf("instructions = %q, want official prompt (existing value not overwritten)", got)
	}
}

func TestPayloadRulesFilterRemovesField(t *testing.T) {
	withPayloadRules(t, `{"filter":[{"params":["reasoning.effort"]}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if gjson.GetBytes(out, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be deleted")
	}
}

func TestPayloadRulesOverrideRaw(t *testing.T) {
	withPayloadRules(t, `{"override_raw":[{"params":{"text":"{\"verbosity\":\"high\"}"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "text.verbosity").String(); got != "high" {
		t.Fatalf("text.verbosity = %q, want high", got)
	}
}

func TestPayloadRulesHeaderGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"headers":{"Originator":"codex_cli*"},"params":{"service_tier":"priority"}}]}`)
	h := http.Header{}
	h.Set("Originator", "codex_cli_rs")
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", h, nil)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (header match)", got)
	}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", http.Header{}, nil)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the header does not match")
	}
}

func TestPayloadRulesModelGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"models":["gpt-5.5*"],"params":{"service_tier":"priority"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the model does not match")
	}
}

func TestPayloadRulesExistNotExistGates(t *testing.T) {
	withPayloadRules(t, `{"override":[{"exist":["reasoning.effort"],"not_exist":["metadata.skip"],"params":{"service_tier":"flex"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "flex" {
		t.Fatalf("service_tier = %q, want flex", got)
	}
	skip := []byte(strings.Replace(payloadTestBody, `"stream":true`, `"stream":true,"metadata":{"skip":1}`, 1))
	out = ApplyPayloadRulesToBody(skip, "gpt-5.6-sol", nil, nil)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the not_exist gate matches")
	}
}

func TestPayloadRulesProtectedPathsRejected(t *testing.T) {
	for _, raw := range []string{
		`{"override":[{"params":{"model":"gpt-4"}}]}`,
		`{"override":[{"params":{"stream":false}}]}`,
		`{"override":[{"params":{"input.0.content":"x"}}]}`,
		`{"filter":[{"params":["prompt_cache_key"]}]}`,
		`{"append":[{"params":{"store":"x"}}]}`,
	} {
		if _, err := ParsePayloadRulesJSON(raw); err == nil {
			t.Fatalf("protected field should be rejected: %s", raw)
		}
	}
}

func TestPayloadRulesInvalidJSONRejected(t *testing.T) {
	for _, raw := range []string{
		`{"override":[{"params":{}}]}`,
		`{"override_raw":[{"params":{"text":"not-json{"}}]}`,
		`{"filter":[{"params":["  "]}]}`,
		`{"unknown_group":[]}`,
		`not json`,
	} {
		if _, err := ParsePayloadRulesJSON(raw); err == nil {
			t.Fatalf("invalid config should be rejected: %s", raw)
		}
	}
	// Empty config is valid
	if rs := mustParseRules(t, ""); !rs.IsEmpty() {
		t.Fatalf("empty string should parse to an empty rule set")
	}
	if rs := mustParseRules(t, "{}"); !rs.IsEmpty() {
		t.Fatalf("{} should parse to an empty rule set")
	}
}

func TestPayloadRulesNormalize(t *testing.T) {
	normalized, err := NormalizePayloadRulesJSON(` {"override":[{"models":["gpt-*"],"params":{"service_tier":"priority"}}]} `)
	if err != nil {
		t.Fatalf("NormalizePayloadRulesJSON: %v", err)
	}
	if !gjson.Valid(normalized) || !gjson.Get(normalized, "override.0.params.service_tier").Exists() {
		t.Fatalf("normalized = %q", normalized)
	}
	if got, _ := NormalizePayloadRulesJSON(""); got != "{}" {
		t.Fatalf("empty config should normalize to {}, got %q", got)
	}
}

func TestMatchPayloadWildcard(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"gpt-*", "gpt-5.6-sol", true},
		{"gpt-*", "GPT-5.6", true},
		{"*", "anything", true},
		{"gpt-5.6-sol", "gpt-5.6-sol", true},
		{"gpt-5.6-sol", "gpt-5.6", false},
		{"*-sol", "gpt-5.6-sol", true},
		{"*-sol", "gpt-5.6", false},
		{"gpt-*-sol", "gpt-5.6-sol", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		if got := matchPayloadWildcard(tc.pattern, tc.value); got != tc.want {
			t.Errorf("matchPayloadWildcard(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestPayloadRulesApplyOrder(t *testing.T) {
	// override applies first, then append adds on top of the overridden value
	withPayloadRules(t, `{"override":[{"params":{"instructions":"base"}}],"append":[{"params":{"instructions":"tail"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if got := gjson.GetBytes(out, "instructions").String(); got != "base\n\ntail" {
		t.Fatalf("instructions = %q, want base\\n\\ntail", got)
	}
}

func TestPayloadRulesAPIKeyGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"api_key_names":["fast*"],"params":{"service_tier":"priority"}}]}`)
	fast := &PayloadRuleIdentity{APIKeyID: 7, APIKeyName: "fast-team"}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, fast)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (key name match)", got)
	}
	// name does not match → do not rewrite
	slow := &PayloadRuleIdentity{APIKeyID: 8, APIKeyName: "slow-team"}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, slow)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the key name does not match")
	}
	// No identity → fail-closed, do not rewrite
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("rules with a key gate should fail closed and not rewrite when there is no identity")
	}
}

func TestPayloadRulesAPIKeyIDGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"api_key_ids":["7","3*"],"params":{"service_tier":"priority"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, &PayloadRuleIdentity{APIKeyID: 7})
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (exact key ID match)", got)
	}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, &PayloadRuleIdentity{APIKeyID: 31})
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (key ID wildcard 3*)", got)
	}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, &PayloadRuleIdentity{APIKeyID: 9})
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the key ID does not match")
	}
}

func TestPayloadRulesGroupGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"group_names":["fast*"],"params":{"service_tier":"priority"}}]}`)
	// Any one matching group name passes
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil,
		&PayloadRuleIdentity{APIKeyID: 1, GroupIDs: []int64{2, 5}, GroupNames: []string{"slow", "fast-pool"}})
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (one of the group names matches)", got)
	}
	// No group name matches → do not rewrite
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil,
		&PayloadRuleIdentity{APIKeyID: 1, GroupNames: []string{"slow", "bulk"}})
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the group name does not match")
	}
}

func TestPayloadRulesGroupIDGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"group_ids":["5"],"params":{"service_tier":"priority"}}]}`)
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil,
		&PayloadRuleIdentity{APIKeyID: 1, GroupIDs: []int64{2, 5}})
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (group ID matched)", got)
	}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil,
		&PayloadRuleIdentity{APIKeyID: 1, GroupIDs: []int64{2, 3}})
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the group ID does not match")
	}
}

func TestPayloadRulesIdentityAndModelGatesCombine(t *testing.T) {
	// Identity gate and model gate are ANDed: rewrite only when both are satisfied
	withPayloadRules(t, `{"override":[{"models":["gpt-*"],"api_key_names":["fast*"],"params":{"service_tier":"priority"}}]}`)
	fast := &PayloadRuleIdentity{APIKeyName: "fast-1"}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, fast)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (model and key both match)", got)
	}
	// Model does not match → do not rewrite, even if the key matches
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "claude-3", nil, fast)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the model gate is not satisfied")
	}
}

func TestPayloadRulesGatesParseRoundTrip(t *testing.T) {
	raw := `{"override":[{"api_key_ids":["1"],"api_key_names":["fast*"],"group_ids":["5"],"group_names":["fast"],"params":{"service_tier":"priority"}}]}`
	rs := mustParseRules(t, raw)
	if len(rs.Override) != 1 {
		t.Fatalf("override rule count = %d, want 1", len(rs.Override))
	}
	r := rs.Override[0]
	if len(r.APIKeyIDs) != 1 || len(r.APIKeyNames) != 1 || len(r.GroupIDs) != 1 || len(r.GroupNames) != 1 {
		t.Fatalf("identity gate fields not fully parsed: %+v", r)
	}
}

func TestEffectiveRequestedServiceTier(t *testing.T) {
	withPayloadRules(t, `{"override":[{"api_key_names":["fast*"],"params":{"service_tier":"priority"}}]}`)
	body := []byte(`{"model":"gpt-5.6-sol","service_tier":"default","input":[]}`)
	// Match → return the overridden value
	got := EffectiveRequestedServiceTier(body, "gpt-5.6-sol", nil, &PayloadRuleIdentity{APIKeyName: "fast-1"})
	if got != "priority" {
		t.Fatalf("EffectiveRequestedServiceTier = %q, want priority", got)
	}
	// No match → return the original value
	got = EffectiveRequestedServiceTier(body, "gpt-5.6-sol", nil, &PayloadRuleIdentity{APIKeyName: "slow-1"})
	if got != "default" {
		t.Fatalf("EffectiveRequestedServiceTier (no match) = %q, want default", got)
	}
	// No identity, fail-closed → return the original value
	got = EffectiveRequestedServiceTier(body, "gpt-5.6-sol", nil, nil)
	if got != "default" {
		t.Fatalf("EffectiveRequestedServiceTier (no identity) = %q, want default", got)
	}
}

func TestWithPayloadRuleIdentityRoundTrip(t *testing.T) {
	id := &PayloadRuleIdentity{APIKeyID: 42, APIKeyName: "k", GroupIDs: []int64{1}, GroupNames: []string{"g"}}
	ctx := WithPayloadRuleIdentity(context.Background(), id)
	if got := PayloadRuleIdentityFromContext(ctx); got != id {
		t.Fatalf("identity retrieved from context is inconsistent: %+v", got)
	}
	// nil identity is not written
	ctx2 := WithPayloadRuleIdentity(context.Background(), nil)
	if PayloadRuleIdentityFromContext(ctx2) != nil {
		t.Fatalf("nil identity should not be written to context")
	}
	// Empty context returns nil
	if PayloadRuleIdentityFromContext(context.Background()) != nil {
		t.Fatalf("context without identity should return nil")
	}
}

// ==================== account gate (issue #410) ====================

// TestPayloadRulesAccountGroupNameGate reproduces the core scenario of issue #410: the key allows
// [pro, plus] groups, the rule should take effect only when it is actually scheduled to a plus group account.
func TestPayloadRulesAccountGroupNameGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"account_group_names":["plus"],"params":{"service_tier":"priority"}}]}`)

	// Key allows the plus group, but the request is actually scheduled to a pro group account → do not rewrite (the scenario the old group_names gate wrongly blocked)
	proAccount := &PayloadRuleIdentity{
		APIKeyID: 1, GroupNames: []string{"pro", "plus"},
		AccountResolved: true, AccountGroupNames: []string{"pro"},
	}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, proAccount)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when actually scheduled to a pro group account: %s", out)
	}

	// Actually scheduled to a plus group account → rewrite
	plusAccount := &PayloadRuleIdentity{
		APIKeyID: 1, GroupNames: []string{"pro", "plus"},
		AccountResolved: true, AccountGroupNames: []string{"plus"},
	}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, plusAccount)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (actually scheduled to plus group)", got)
	}

	// Account not resolved (AccountResolved=false) → fail-closed, do not rewrite
	unresolved := &PayloadRuleIdentity{APIKeyID: 1, GroupNames: []string{"plus"}}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, unresolved)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("rules with an account gate should fail closed when the account is not resolved")
	}

	// No identity → fail-closed
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, nil)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("rules with an account gate should fail closed when there is no identity")
	}
}

func TestPayloadRulesAccountGroupIDGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"account_group_ids":["7"],"params":{"service_tier":"priority"}}]}`)
	hit := &PayloadRuleIdentity{AccountResolved: true, AccountGroupIDs: []int64{3, 7}}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, hit)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (account group ID matched)", got)
	}
	miss := &PayloadRuleIdentity{AccountResolved: true, AccountGroupIDs: []int64{3, 4}}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, miss)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the account group ID does not match")
	}
}

func TestPayloadRulesAccountPlanGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"account_plans":["plus"],"params":{"service_tier":"priority"}}]}`)
	plus := &PayloadRuleIdentity{AccountResolved: true, AccountPlan: "plus"}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, plus)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (plus plan matched)", got)
	}
	pro := &PayloadRuleIdentity{AccountResolved: true, AccountPlan: "pro"}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, pro)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("pro plan should not match the plus gate")
	}
}

func TestPayloadRulesAccountGateCombinesWithKeyGates(t *testing.T) {
	// Account gate and key identity gate are ANDed: rewrite only when the pro key and the actual plus account both match
	withPayloadRules(t, `{"override":[{"api_key_names":["pro*"],"account_group_names":["plus"],"params":{"service_tier":"priority"}}]}`)
	both := &PayloadRuleIdentity{APIKeyName: "pro-main", AccountResolved: true, AccountGroupNames: []string{"plus"}}
	out := ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, both)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority (key and account gates both matched)", got)
	}
	wrongKey := &PayloadRuleIdentity{APIKeyName: "other", AccountResolved: true, AccountGroupNames: []string{"plus"}}
	out = ApplyPayloadRulesToBody([]byte(payloadTestBody), "gpt-5.6-sol", nil, wrongKey)
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("should not rewrite when the key gate does not match")
	}
}

func TestWithSelectedAccount(t *testing.T) {
	base := &PayloadRuleIdentity{APIKeyID: 9, APIKeyName: "pro-main", GroupNames: []string{"pro", "plus"}}
	account := &auth.Account{DBID: 1, PlanType: "plus", GroupIDs: []int64{7}}

	derived := base.WithSelectedAccount(account, nil)
	if derived == base {
		t.Fatalf("should return a copy, not the original object")
	}
	if !derived.AccountResolved {
		t.Fatalf("AccountResolved should be true")
	}
	if derived.AccountPlan != "plus" {
		t.Fatalf("AccountPlan = %q, want plus", derived.AccountPlan)
	}
	if len(derived.AccountGroupIDs) != 1 || derived.AccountGroupIDs[0] != 7 {
		t.Fatalf("AccountGroupIDs = %v, want [7]", derived.AccountGroupIDs)
	}
	// Key-dimension fields are kept
	if derived.APIKeyID != 9 || derived.APIKeyName != "pro-main" {
		t.Fatalf("key identity fields should be kept")
	}
	// The original object is not affected
	if base.AccountResolved || base.AccountPlan != "" {
		t.Fatalf("original identity should not be modified")
	}
	// nil identity stays nil (fail-closed); nil account is returned as-is
	if (*PayloadRuleIdentity)(nil).WithSelectedAccount(account, nil) != nil {
		t.Fatalf("nil identity should stay nil")
	}
	if base.WithSelectedAccount(nil, nil) != base {
		t.Fatalf("nil account should be returned as-is")
	}
}

// TestEffectiveRequestedServiceTierAccountGate verifies that billing attribution flips in sync with the account gate.
func TestEffectiveRequestedServiceTierAccountGate(t *testing.T) {
	withPayloadRules(t, `{"override":[{"account_group_names":["plus"],"params":{"service_tier":"priority"}}]}`)
	plus := &PayloadRuleIdentity{AccountResolved: true, AccountGroupNames: []string{"plus"}}
	if got := EffectiveRequestedServiceTier([]byte(payloadTestBody), "gpt-5.6-sol", nil, plus); got != "priority" {
		t.Fatalf("tier = %q, want priority (plus account attribution)", got)
	}
	pro := &PayloadRuleIdentity{AccountResolved: true, AccountGroupNames: []string{"pro"}}
	if got := EffectiveRequestedServiceTier([]byte(payloadTestBody), "gpt-5.6-sol", nil, pro); got != "" {
		t.Fatalf("tier = %q, want \"\" (pro account not rewritten)", got)
	}
}
