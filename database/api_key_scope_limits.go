package database

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scope type of the API key group/account dimension limits.
const (
	APIKeyScopeTypeGroup   = "group"
	APIKeyScopeTypeAccount = "account"
)

// Behavior when the scope quota is exhausted.
//   - skip:   remove that scope's accounts from the candidate pool for this request, automatically falling through to other groups/accounts;
//     when the candidate pool is emptied, return 429 and state that the scope budget is exhausted (rather than 503 no available account).
//   - reject: as soon as this scope is over its limit, every request of this key gets 429, with no account switching.
const (
	APIKeyScopeOnExhaustedSkip   = "skip"
	APIKeyScopeOnExhaustedReject = "reject"
)

// APIKeyScopeWindow is a sliding window supported by scope limits. label is used both for the cache key and the error text.
type APIKeyScopeWindow struct {
	Label  string
	Window time.Duration
}

// APIKeyScopeWindowTotal is the pseudo-window label that cumulative quotas use in errors and display.
const APIKeyScopeWindowTotal = "total"

// APIKeyScopeWindows is all candidate windows; the order is the validation order (shorter windows are checked first, so the message matches the situation better).
var APIKeyScopeWindows = []APIKeyScopeWindow{
	{Label: "5h", Window: 5 * time.Hour},
	{Label: "1d", Window: 24 * time.Hour},
	{Label: "7d", Window: 7 * 24 * time.Hour},
	{Label: "30d", Window: 30 * 24 * time.Hour},
}

// APIKeyScopeLimit is one API key's usage cap for an account group or a single account (issue #439).
//
// Semantics: the limit only constrains "this key's usage toward this scope"; it does not affect the scope's use by other keys,
// nor this key's usage toward other scopes. A zero value means that dimension is unlimited.
//
// The metering basis is always user_billed (consistent with the key-level cost limit); for models without downstream pricing the cost is always 0,
// in those cases use the token / request-count dimensions instead.
type APIKeyScopeLimit struct {
	ScopeType   string `json:"scope_type"`
	ScopeID     int64  `json:"scope_id"`
	OnExhausted string `json:"on_exhausted,omitempty"`

	Cost5h  float64 `json:"cost_5h,omitempty"`
	Cost1d  float64 `json:"cost_1d,omitempty"`
	Cost7d  float64 `json:"cost_7d,omitempty"`
	Cost30d float64 `json:"cost_30d,omitempty"`

	Token5h  int64 `json:"token_5h,omitempty"`
	Token1d  int64 `json:"token_1d,omitempty"`
	Token7d  int64 `json:"token_7d,omitempty"`
	Token30d int64 `json:"token_30d,omitempty"`

	Requests1d int64 `json:"requests_1d,omitempty"`

	// MaxConcurrency is the maximum number of in-flight requests this key may have on this scope (a soft in-process limit).
	// Unlike the key-level max_concurrency, it only constrains requests going to this scope; when the slots are full it chooses, according to OnExhausted, to
	// switch accounts or reject.
	MaxConcurrency int `json:"max_concurrency,omitempty"`

	// Cumulative quota (does not decay over time; must be reset manually once used up; semantics aligned with api_keys.quota_limit).
	// The used amount is stored in the api_key_scope_counters table, not in this configuration.
	QuotaCost     float64 `json:"quota_cost,omitempty"`
	QuotaTokens   int64   `json:"quota_tokens,omitempty"`
	QuotaRequests int64   `json:"quota_requests,omitempty"`
}

// HasCumulativeQuota reports whether this scope has a cumulative quota configured.
func (s APIKeyScopeLimit) HasCumulativeQuota() bool {
	return s.QuotaCost > 0 || s.QuotaTokens > 0 || s.QuotaRequests > 0
}

// CheckCumulative uses the cumulative counter to decide whether this scope's cumulative quota is used up.
func (s APIKeyScopeLimit) CheckCumulative(counter APIKeyScopeCounter) (APIKeyScopeExhaustion, bool) {
	if s.QuotaCost > 0 && counter.UsedCost >= s.QuotaCost {
		return APIKeyScopeExhaustion{Window: APIKeyScopeWindowTotal, Metric: "cost", Used: counter.UsedCost, Limit: s.QuotaCost}, true
	}
	if s.QuotaTokens > 0 && counter.UsedTokens >= s.QuotaTokens {
		return APIKeyScopeExhaustion{Window: APIKeyScopeWindowTotal, Metric: "tokens", Used: float64(counter.UsedTokens), Limit: float64(s.QuotaTokens)}, true
	}
	if s.QuotaRequests > 0 && counter.UsedRequests >= s.QuotaRequests {
		return APIKeyScopeExhaustion{Window: APIKeyScopeWindowTotal, Metric: "requests", Used: float64(counter.UsedRequests), Limit: float64(s.QuotaRequests)}, true
	}
	return APIKeyScopeExhaustion{}, false
}

// ResolveScopeType normalizes the scope type; an unknown value returns "" (the caller should drop the entry).
func (s APIKeyScopeLimit) ResolveScopeType() string {
	switch strings.ToLower(strings.TrimSpace(s.ScopeType)) {
	case APIKeyScopeTypeGroup:
		return APIKeyScopeTypeGroup
	case APIKeyScopeTypeAccount:
		return APIKeyScopeTypeAccount
	}
	return ""
}

// ResolveOnExhausted normalizes the exhaustion behavior; unknown values always fall back to skip (the most conservative choice: still able to serve).
func (s APIKeyScopeLimit) ResolveOnExhausted() string {
	if strings.ToLower(strings.TrimSpace(s.OnExhausted)) == APIKeyScopeOnExhaustedReject {
		return APIKeyScopeOnExhaustedReject
	}
	return APIKeyScopeOnExhaustedSkip
}

// HasAnyLimit reports whether this scope has at least one effective cap. An all-zero entry has no binding force,
// so it should be dropped on save, otherwise it would needlessly trigger a window aggregation query.
func (s APIKeyScopeLimit) HasAnyLimit() bool {
	return s.Cost5h > 0 || s.Cost1d > 0 || s.Cost7d > 0 || s.Cost30d > 0 ||
		s.Token5h > 0 || s.Token1d > 0 || s.Token7d > 0 || s.Token30d > 0 ||
		s.Requests1d > 0 || s.MaxConcurrency > 0 || s.HasCumulativeQuota()
}

// NeedsWindow reports whether this scope uses a given window, used to trigger the window aggregation query on demand.
func (s APIKeyScopeLimit) NeedsWindow(label string) bool {
	switch label {
	case "5h":
		return s.Cost5h > 0 || s.Token5h > 0
	case "1d":
		return s.Cost1d > 0 || s.Token1d > 0 || s.Requests1d > 0
	case "7d":
		return s.Cost7d > 0 || s.Token7d > 0
	case "30d":
		return s.Cost30d > 0 || s.Token30d > 0
	}
	return false
}

// APIKeyScopeExhaustion describes the trigger result of one scope limit.
type APIKeyScopeExhaustion struct {
	Window string
	Metric string
	Used   float64
	Limit  float64
}

// Describe returns the exhaustion explanation shown to downstream clients. Amounts and tokens use different formats, consistent with the
// wording style of key-level limits.
func (e APIKeyScopeExhaustion) Describe(scopeLabel string) string {
	// A cumulative quota has no "last N" wording, so it gets its own phrasing; otherwise it would read as "in last total".
	scopeWindow := "in last " + e.Window
	if e.Window == APIKeyScopeWindowTotal {
		scopeWindow = "in total (needs a manual reset)"
	}
	if e.Metric == "cost" {
		return fmt.Sprintf("API key scope budget exhausted: %s used $%s / $%s %s",
			scopeLabel, formatScopeCost(e.Used), formatScopeCost(e.Limit), scopeWindow)
	}
	return fmt.Sprintf("API key scope budget exhausted: %s used %d / %d %s %s",
		scopeLabel, int64(e.Used), int64(e.Limit), e.Metric, scopeWindow)
}

// formatScopeCost formats the amount: ordinary budgets keep two decimal places in cents, while tiny quotas (such as experimental per-call values)
// keep a few more digits, so that the text does not show "$0.00 / $0.00" and leave people unable to judge.
func formatScopeCost(value float64) string {
	if value > 0 && value < 0.01 {
		return strconv.FormatFloat(value, 'f', 6, 64)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// CheckWindow uses the actual usage of a window to decide whether this scope is exhausted. On a hit it returns (result, true).
// The check uses >=, consistent with key-level limits: reaching the cap counts as exhausted.
func (s APIKeyScopeLimit) CheckWindow(label string, usage APIKeyWindowUsage) (APIKeyScopeExhaustion, bool) {
	var costLimit float64
	var tokenLimit, requestLimit int64
	switch label {
	case "5h":
		costLimit, tokenLimit = s.Cost5h, s.Token5h
	case "1d":
		costLimit, tokenLimit, requestLimit = s.Cost1d, s.Token1d, s.Requests1d
	case "7d":
		costLimit, tokenLimit = s.Cost7d, s.Token7d
	case "30d":
		costLimit, tokenLimit = s.Cost30d, s.Token30d
	default:
		return APIKeyScopeExhaustion{}, false
	}
	if costLimit > 0 && usage.UserBilled >= costLimit {
		return APIKeyScopeExhaustion{Window: label, Metric: "cost", Used: usage.UserBilled, Limit: costLimit}, true
	}
	if tokenLimit > 0 && usage.Tokens >= tokenLimit {
		return APIKeyScopeExhaustion{Window: label, Metric: "tokens", Used: float64(usage.Tokens), Limit: float64(tokenLimit)}, true
	}
	if requestLimit > 0 && usage.Requests >= requestLimit {
		return APIKeyScopeExhaustion{Window: label, Metric: "requests", Used: float64(usage.Requests), Limit: float64(requestLimit)}, true
	}
	return APIKeyScopeExhaustion{}, false
}

// NormalizeAPIKeyScopeLimits normalizes the scope limit list: it drops entries with unknown types, invalid IDs or all zeros,
// dedupes by (type, id) (a later entry overrides an earlier one), and clamps negative values to 0. It returns nil when there are no valid scope limits.
func NormalizeAPIKeyScopeLimits(in []APIKeyScopeLimit) []APIKeyScopeLimit {
	if len(in) == 0 {
		return nil
	}
	type scopeKey struct {
		scopeType string
		scopeID   int64
	}
	index := make(map[scopeKey]int, len(in))
	out := make([]APIKeyScopeLimit, 0, len(in))
	for _, item := range in {
		scopeType := item.ResolveScopeType()
		if scopeType == "" || item.ScopeID <= 0 {
			continue
		}
		clean := APIKeyScopeLimit{
			ScopeType:   scopeType,
			ScopeID:     item.ScopeID,
			OnExhausted: item.ResolveOnExhausted(),
			Cost5h:      maxScopeFloat(item.Cost5h),
			Cost1d:      maxScopeFloat(item.Cost1d),
			Cost7d:      maxScopeFloat(item.Cost7d),
			Cost30d:     maxScopeFloat(item.Cost30d),
			Token5h:     maxScopeInt64(item.Token5h),
			Token1d:     maxScopeInt64(item.Token1d),
			Token7d:     maxScopeInt64(item.Token7d),
			Token30d:    maxScopeInt64(item.Token30d),
			Requests1d:  maxScopeInt64(item.Requests1d),

			MaxConcurrency: int(maxScopeInt64(int64(item.MaxConcurrency))),

			QuotaCost:     maxScopeFloat(item.QuotaCost),
			QuotaTokens:   maxScopeInt64(item.QuotaTokens),
			QuotaRequests: maxScopeInt64(item.QuotaRequests),
		}
		if !clean.HasAnyLimit() {
			continue
		}
		key := scopeKey{scopeType: scopeType, scopeID: item.ScopeID}
		if pos, ok := index[key]; ok {
			out[pos] = clean
			continue
		}
		index[key] = len(out)
		out = append(out, clean)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PruneAPIKeyScopeLimitsForScope deletes scope limit entries that point to a deleted group / account.
// It returns (remaining entries, whether anything changed).
func PruneAPIKeyScopeLimitsForScope(in []APIKeyScopeLimit, scopeType string, scopeID int64) ([]APIKeyScopeLimit, bool) {
	if len(in) == 0 || scopeID <= 0 {
		return in, false
	}
	out := make([]APIKeyScopeLimit, 0, len(in))
	changed := false
	for _, item := range in {
		if item.ResolveScopeType() == scopeType && item.ScopeID == scopeID {
			changed = true
			continue
		}
		out = append(out, item)
	}
	if !changed {
		return in, false
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

func maxScopeFloat(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func maxScopeInt64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
