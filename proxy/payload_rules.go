package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== Payload request body rewrite rules ====================
//
// Declarative rule engine: admins configure JSON rules that rewrite fields by condition before the request body is sent upstream.
// Typical uses: override/append instructions (system prompt), override service_tier,
// rewrite reasoning.effort by condition (e.g. medium → high).
//
// Rule JSON structure (stored in system_settings.payload_rules):
//
//	{
//	  "default":      [ {rule} ],  // written only when the field is missing (first-write-wins)
//	  "default_raw":  [ {rule} ],  // same as above, but the value is a raw JSON string
//	  "override":     [ {rule} ],  // unconditional override (last-write-wins)
//	  "override_raw": [ {rule} ],  // same as above, but the value is a raw JSON string
//	  "append":       [ {rule} ],  // appends to a string field (original value + "\n\n" + configured value)
//	  "filter":       [ {rule} ]   // deletes fields (params is an array of paths)
//	}
//
// Match gates of each rule (all must be satisfied to apply; multiple values within one gate are OR, and gates are AND-ed together):
//
//	{
//	  "models":        ["gpt-*"],                    // model name wildcard, empty = all
//	  "headers":       {"X-Client": "codex*"},       // inbound request header wildcard
//	  "api_key_ids":   ["12", "3*"],                 // downstream API Key ID (stringified) wildcard
//	  "api_key_names": ["fast*"],                    // downstream API Key name wildcard
//	  "group_ids":     ["5"],                        // wildcard of the account group IDs this Key is allowed (stringified)
//	  "group_names":   ["fast*"],                    // wildcard of the account group names this Key is allowed
//	  "account_group_ids":   ["5"],                  // wildcard of the group IDs of the account actually scheduled this time
//	  "account_group_names": ["plus*"],              // wildcard of the group names of the account actually scheduled this time
//	  "account_plans":       ["plus"],               // wildcard of the plan of the account actually scheduled this time
//	  "match":         {"reasoning.effort": "medium"}, // JSON path equals
//	  "not_match":     {"metadata.mode": "dev"},     // JSON path does not equal
//	  "exist":         ["tools.0"],                  // path must exist
//	  "not_exist":     ["metadata.skip"],            // path must not exist
//	  "params":        {...}                         // action parameters
//	}
//
// api_key_* / group_* depend on the request identity (PayloadRuleIdentity): on internal or unauthenticated paths with no identity,
// any rule that configures a key or group gate never matches (fail-closed). The compact/relay endpoints do not use this engine.
//
// Note the semantic difference between group_* and account_group_* (issue #410):
//   - group_*         matches the account groups this Key is **allowed to use**, regardless of which account was actually scheduled this time;
//   - account_group_* / account_plans match the group/plan of the account **actually selected** for this attempt.
//     After a retry switches accounts, matching is redone against the new account. Typical use: overwrite service_tier=fast only when a pro Key falls back to a plus group account
//     service_tier=fast; routing back to a pro account does not overwrite it.
//     On paths where the account identity is not resolved (AccountResolved is false), rules with account_* gates are likewise fail-closed.
//
// Application order: default → default_raw → override → override_raw → append → filter.

// payloadProtectedRoots are the field roots that may not be rewritten: the gateway's scheduling, billing, session isolation and
// WS frame protocol depend on these fields, and rule application happens after account selection, so rewriting them would misalign state.
// To rewrite the model, use the existing model mapping feature.
var payloadProtectedRoots = map[string]struct{}{
	"model":            {},
	"stream":           {},
	"input":            {},
	"prompt_cache_key": {},
	"store":            {},
	"type":             {},
}

// PayloadRule is one rewrite rule: match gates + action parameters.
type PayloadRule struct {
	Models  []string          `json:"models,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Identity gates: all are wildcard lists, and matching any one passes; empty = this dimension is unrestricted. Id gates match on the stringified value.
	// Depends on PayloadRuleIdentity; without an identity, any rule that configures an identity gate never matches (fail-closed).
	APIKeyIDs   []string `json:"api_key_ids,omitempty"`
	APIKeyNames []string `json:"api_key_names,omitempty"`
	GroupIDs    []string `json:"group_ids,omitempty"`
	GroupNames  []string `json:"group_names,omitempty"`
	// Account gates: match the group/plan of the account actually selected this time (as opposed to the above, which match by the Key's allowed groups
	// group_*, see the file header comment). When the account identity is unresolved, they fail-closed and never match.
	AccountGroupIDs   []string       `json:"account_group_ids,omitempty"`
	AccountGroupNames []string       `json:"account_group_names,omitempty"`
	AccountPlans      []string       `json:"account_plans,omitempty"`
	Match             map[string]any `json:"match,omitempty"`
	NotMatch          map[string]any `json:"not_match,omitempty"`
	Exist             []string       `json:"exist,omitempty"`
	NotExist          []string       `json:"not_exist,omitempty"`
	// Params action parameters: default/override/append are path→value maps,
	// default_raw/override_raw values must be valid JSON strings, and filter is an array of paths.
	Params json.RawMessage `json:"params"`
}

// PayloadRuleIdentity carries the request's downstream identity, for the api_key_* / group_* / account_* match gates
// to use. The handler builds it from the auth context and passes it on through the upstream context (see WithPayloadRuleIdentity).
// The Account* fields are filled per attempt by WithSelectedAccount, which derives a copy: after a retry switches accounts, the account dimension
// changes, so it must not be modified in place on the shared instance.
type PayloadRuleIdentity struct {
	APIKeyID   int64
	APIKeyName string
	GroupIDs   []int64  // account group IDs this Key is allowed (Key.AllowedGroupIDs)
	GroupNames []string // group names resolved from the group IDs above

	// Dimensions of the account actually selected for this attempt (issue #410).
	AccountResolved   bool // whether account info has been filled; when false, rules with account_* gates fail-closed
	AccountGroupIDs   []int64
	AccountGroupNames []string
	AccountPlan       string
}

// WithSelectedAccount returns a copy of the identity filled with the dimensions of the account actually scheduled. The original identity is shared across attempts,
// and account info differs after a retry switches accounts, so a copy must be derived rather than modified in place. When id is nil (no authenticated identity), it stays
// nil, consistent with the fail-closed semantics of the existing identity gates.
func (id *PayloadRuleIdentity) WithSelectedAccount(account *auth.Account, store *auth.Store) *PayloadRuleIdentity {
	if id == nil || account == nil {
		return id
	}
	derived := *id
	derived.AccountResolved = true
	derived.AccountGroupIDs = account.GroupIDSnapshot()
	if store != nil {
		derived.AccountGroupNames = store.ResolveGroupNames(derived.AccountGroupIDs)
	}
	derived.AccountPlan = account.GetPlanType()
	return &derived
}

type payloadRuleIdentityKey struct{}

// WithPayloadRuleIdentity attaches the identity to the context (returned unchanged when id is nil).
// It must be attached to the upstream context that actually flows into ExecuteRequest: the client request context and the upstream
// context are decoupled (newDrainableUpstreamContext derives from background), so attaching it to the wrong one means it never reaches the engine.
func WithPayloadRuleIdentity(ctx context.Context, id *PayloadRuleIdentity) context.Context {
	if ctx == nil || id == nil {
		return ctx
	}
	return context.WithValue(ctx, payloadRuleIdentityKey{}, id)
}

// PayloadRuleIdentityFromContext returns the identity, or nil if it is missing.
func PayloadRuleIdentityFromContext(ctx context.Context) *PayloadRuleIdentity {
	if ctx == nil {
		return nil
	}
	id, _ := ctx.Value(payloadRuleIdentityKey{}).(*PayloadRuleIdentity)
	return id
}

// PayloadRuleSet is all rule groups.
type PayloadRuleSet struct {
	Default     []PayloadRule `json:"default,omitempty"`
	DefaultRaw  []PayloadRule `json:"default_raw,omitempty"`
	Override    []PayloadRule `json:"override,omitempty"`
	OverrideRaw []PayloadRule `json:"override_raw,omitempty"`
	Append      []PayloadRule `json:"append,omitempty"`
	Filter      []PayloadRule `json:"filter,omitempty"`
}

// IsEmpty reports whether the rule set has no rules at all.
func (rs *PayloadRuleSet) IsEmpty() bool {
	if rs == nil {
		return true
	}
	return len(rs.Default) == 0 && len(rs.DefaultRaw) == 0 && len(rs.Override) == 0 &&
		len(rs.OverrideRaw) == 0 && len(rs.Append) == 0 && len(rs.Filter) == 0
}

var currentPayloadRuleSet atomic.Value // stores *PayloadRuleSet

// CurrentPayloadRules returns the currently active rule set (may be nil).
func CurrentPayloadRules() *PayloadRuleSet {
	if v, ok := currentPayloadRuleSet.Load().(*PayloadRuleSet); ok {
		return v
	}
	return nil
}

// SetPayloadRulesJSON parses and hot-reloads the rule set; passing an empty string or empty object clears the rules.
// On a parse or validation failure, the existing rules are kept unchanged and an error is returned.
func SetPayloadRulesJSON(raw string) error {
	rs, err := ParsePayloadRulesJSON(raw)
	if err != nil {
		return err
	}
	currentPayloadRuleSet.Store(rs)
	return nil
}

// ParsePayloadRulesJSON parses the rule JSON and validates its structure and protected fields.
func ParsePayloadRulesJSON(raw string) (*PayloadRuleSet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return &PayloadRuleSet{}, nil
	}
	var rs PayloadRuleSet
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("payload_rules JSON parse failed: %w", err)
	}
	groups := []struct {
		name  string
		rules []PayloadRule
		kind  string // map / raw / filter / append
	}{
		{"default", rs.Default, "map"},
		{"default_raw", rs.DefaultRaw, "raw"},
		{"override", rs.Override, "map"},
		{"override_raw", rs.OverrideRaw, "raw"},
		{"append", rs.Append, "append"},
		{"filter", rs.Filter, "filter"},
	}
	for _, g := range groups {
		for i := range g.rules {
			if err := validatePayloadRuleParams(&g.rules[i], g.kind); err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", g.name, i, err)
			}
		}
	}
	return &rs, nil
}

// NormalizePayloadRulesJSON validates and returns the compacted rule JSON (used when saving settings).
func NormalizePayloadRulesJSON(raw string) (string, error) {
	rs, err := ParsePayloadRulesJSON(raw)
	if err != nil {
		return "", err
	}
	if rs.IsEmpty() {
		return "{}", nil
	}
	normalized, err := json.Marshal(rs)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func validatePayloadRuleParams(rule *PayloadRule, kind string) error {
	if len(rule.Params) == 0 {
		return fmt.Errorf("params must not be empty")
	}
	checkPath := func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return fmt.Errorf("params path must not be empty")
		}
		if isProtectedPayloadPath(path) {
			return fmt.Errorf("field %q is protected and cannot be rewritten (use model mapping to rewrite the model)", path)
		}
		return nil
	}
	switch kind {
	case "filter":
		var paths []string
		if err := json.Unmarshal(rule.Params, &paths); err != nil {
			return fmt.Errorf("filter params must be an array of paths: %w", err)
		}
		if len(paths) == 0 {
			return fmt.Errorf("params must not be empty")
		}
		for _, p := range paths {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	case "raw":
		var params map[string]string
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("raw rule params must be a path→JSON string map: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params must not be empty")
		}
		for p, v := range params {
			if err := checkPath(p); err != nil {
				return err
			}
			if !json.Valid([]byte(v)) {
				return fmt.Errorf("value of field %q is not valid JSON: %q", p, v)
			}
		}
	case "append":
		var params map[string]string
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("append rule params must be a path→string map: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params must not be empty")
		}
		for p := range params {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	default: // map
		var params map[string]any
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("params must be a path→value map: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params must not be empty")
		}
		for p := range params {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// isProtectedPayloadPath reports whether the root segment of the path hits the protected list.
func isProtectedPayloadPath(path string) bool {
	root := path
	if idx := strings.IndexAny(path, ".|#("); idx >= 0 {
		root = path[:idx]
	}
	_, protected := payloadProtectedRoots[strings.TrimSpace(root)]
	return protected
}

// matchPayloadWildcard is a case-insensitive * wildcard match.
func matchPayloadWildcard(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, segments[0]) {
		return false
	}
	rest := value[len(segments[0]):]
	for _, seg := range segments[1 : len(segments)-1] {
		if seg == "" {
			continue
		}
		idx := strings.Index(rest, seg)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(seg):]
	}
	last := segments[len(segments)-1]
	return last == "" || strings.HasSuffix(rest, last)
}

// anyWildcard reports whether value matches any wildcard in patterns (empty patterns means the gate is not set, and it returns true).
func anyWildcard(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if matchPayloadWildcard(pattern, value) {
			return true
		}
	}
	return false
}

// anyWildcardMulti reports whether any of values matches any wildcard in patterns (returns true when patterns is empty).
func anyWildcardMulti(patterns, values []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, value := range values {
		if anyWildcard(patterns, value) {
			return true
		}
	}
	return false
}

// formatInt64IDs stringifies the ID list, for id gate wildcard matching.
func formatInt64IDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}

// payloadRuleMatches reports whether all match gates of the rule are satisfied. identity is the request identity (may be nil).
func payloadRuleMatches(rule *PayloadRule, body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) bool {
	if !anyWildcard(rule.Models, model) {
		return false
	}
	for name, pattern := range rule.Headers {
		if headers == nil || !matchPayloadWildcard(pattern, headers.Get(name)) {
			return false
		}
	}
	// Identity gates: if any gate is configured but the request has no identity → fail-closed, no match.
	if len(rule.APIKeyIDs) > 0 || len(rule.APIKeyNames) > 0 || len(rule.GroupIDs) > 0 || len(rule.GroupNames) > 0 {
		if identity == nil {
			return false
		}
		if len(rule.APIKeyIDs) > 0 && !anyWildcard(rule.APIKeyIDs, strconv.FormatInt(identity.APIKeyID, 10)) {
			return false
		}
		if len(rule.APIKeyNames) > 0 && !anyWildcard(rule.APIKeyNames, identity.APIKeyName) {
			return false
		}
		if len(rule.GroupIDs) > 0 {
			if !anyWildcardMulti(rule.GroupIDs, formatInt64IDs(identity.GroupIDs)) {
				return false
			}
		}
		if len(rule.GroupNames) > 0 && !anyWildcardMulti(rule.GroupNames, identity.GroupNames) {
			return false
		}
	}
	// Account gates: match the group/plan of the account actually selected this time. When the account identity is unresolved (identity is nil
	// or AccountResolved=false), they likewise fail-closed and do not match.
	if len(rule.AccountGroupIDs) > 0 || len(rule.AccountGroupNames) > 0 || len(rule.AccountPlans) > 0 {
		if identity == nil || !identity.AccountResolved {
			return false
		}
		if len(rule.AccountGroupIDs) > 0 {
			if !anyWildcardMulti(rule.AccountGroupIDs, formatInt64IDs(identity.AccountGroupIDs)) {
				return false
			}
		}
		if len(rule.AccountGroupNames) > 0 && !anyWildcardMulti(rule.AccountGroupNames, identity.AccountGroupNames) {
			return false
		}
		if len(rule.AccountPlans) > 0 && !anyWildcard(rule.AccountPlans, identity.AccountPlan) {
			return false
		}
	}
	for path, want := range rule.Match {
		if !payloadValueEquals(gjson.GetBytes(body, path), want) {
			return false
		}
	}
	for path, want := range rule.NotMatch {
		if payloadValueEquals(gjson.GetBytes(body, path), want) {
			return false
		}
	}
	for _, path := range rule.Exist {
		result := gjson.GetBytes(body, path)
		if !result.Exists() || result.Type == gjson.Null {
			return false
		}
	}
	for _, path := range rule.NotExist {
		result := gjson.GetBytes(body, path)
		if result.Exists() && result.Type != gjson.Null {
			return false
		}
	}
	return true
}

// payloadValueEquals compares the gjson result with the configured value (both sides come from JSON, so the types line up naturally).
func payloadValueEquals(result gjson.Result, want any) bool {
	if !result.Exists() {
		return want == nil && result.Type == gjson.Null
	}
	return reflect.DeepEqual(result.Value(), want)
}

// ApplyPayloadRulesToBody rewrites the request body with the current rule set. model is the effective model after mapping,
// headers are the inbound request headers, and identity is the request identity (may be nil).
// A single action that fails to rewrite is skipped and does not affect the other rules.
func ApplyPayloadRulesToBody(body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) []byte {
	rs := CurrentPayloadRules()
	if rs.IsEmpty() || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	out := body

	applyMapRules := func(rules []PayloadRule, onlyIfMissing bool) {
		for i := range rules {
			rule := &rules[i]
			if !payloadRuleMatches(rule, out, model, headers, identity) {
				continue
			}
			var params map[string]any
			if json.Unmarshal(rule.Params, &params) != nil {
				continue
			}
			for path, value := range params {
				if isProtectedPayloadPath(path) {
					continue
				}
				if onlyIfMissing && gjson.GetBytes(out, path).Exists() {
					continue
				}
				if updated, err := sjson.SetBytes(out, path, value); err == nil {
					out = updated
				}
			}
		}
	}
	applyRawRules := func(rules []PayloadRule, onlyIfMissing bool) {
		for i := range rules {
			rule := &rules[i]
			if !payloadRuleMatches(rule, out, model, headers, identity) {
				continue
			}
			var params map[string]string
			if json.Unmarshal(rule.Params, &params) != nil {
				continue
			}
			for path, value := range params {
				if isProtectedPayloadPath(path) || !json.Valid([]byte(value)) {
					continue
				}
				if onlyIfMissing && gjson.GetBytes(out, path).Exists() {
					continue
				}
				if updated, err := sjson.SetRawBytes(out, path, []byte(value)); err == nil {
					out = updated
				}
			}
		}
	}

	applyMapRules(rs.Default, true)
	applyRawRules(rs.DefaultRaw, true)
	applyMapRules(rs.Override, false)
	applyRawRules(rs.OverrideRaw, false)

	for i := range rs.Append {
		rule := &rs.Append[i]
		if !payloadRuleMatches(rule, out, model, headers, identity) {
			continue
		}
		var params map[string]string
		if json.Unmarshal(rule.Params, &params) != nil {
			continue
		}
		for path, text := range params {
			if isProtectedPayloadPath(path) || text == "" {
				continue
			}
			existing := gjson.GetBytes(out, path)
			// Only append to a missing or string field; skip non-string fields to avoid breaking the structure.
			if existing.Exists() && existing.Type != gjson.String {
				continue
			}
			value := text
			if prev := existing.String(); strings.TrimSpace(prev) != "" {
				// Idempotency guard: retry/continue-thinking paths may re-enter the rule engine with an already rewritten body,
				// text that already exists as a complete appended block is not concatenated again, otherwise the request body
				// would grow with each re-entry and change the prompt's meaning.
				if payloadAppendAlreadyApplied(prev, text) {
					continue
				}
				value = prev + "\n\n" + text
			}
			if updated, err := sjson.SetBytes(out, path, value); err == nil {
				out = updated
			}
		}
	}

	for i := range rs.Filter {
		rule := &rs.Filter[i]
		if !payloadRuleMatches(rule, out, model, headers, identity) {
			continue
		}
		var paths []string
		if json.Unmarshal(rule.Params, &paths) != nil {
			continue
		}
		for _, path := range paths {
			if isProtectedPayloadPath(path) {
				continue
			}
			if updated, err := sjson.DeleteBytes(out, path); err == nil {
				out = updated
			}
		}
	}
	return out
}

// payloadAppendAlreadyApplied reports whether text already exists in prev as a complete appended block:
// prev is exactly text, or text appears at the start, end or middle of prev, aligned on the "\n\n" separator.
// When the downstream body already carries the same complete block, skipping the append is equivalent to appending and then deduplicating, so the final meaning does not change.
func payloadAppendAlreadyApplied(prev, text string) bool {
	return prev == text ||
		strings.HasPrefix(prev, text+"\n\n") ||
		strings.HasSuffix(prev, "\n\n"+text) ||
		strings.Contains(prev, "\n\n"+text+"\n\n")
}

// EffectiveRequestedServiceTier returns the service_tier that will actually be sent upstream after the request body is rewritten by the current rules,
// so the usage log attributes the requested/billing tier by the overridden value. The input must match what ExecuteRequest feeds the engine
// (image generation requests bypass the rules, same as executor.go). When no rule matches, it returns the original value.
func EffectiveRequestedServiceTier(body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) string {
	if responsesBodyRequestsImageGeneration(body) {
		return extractServiceTier(body)
	}
	return extractServiceTier(ApplyPayloadRulesToBody(body, model, headers, identity))
}
