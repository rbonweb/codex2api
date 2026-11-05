package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// APIKeyWindowUsage is the cumulative usage of one API key within a time window.
// Only excludes 499 client-cancelled requests, keeping semantics consistent with GetUsageStats.
type APIKeyWindowUsage struct {
	Requests   int64   `json:"requests"`
	Tokens     int64   `json:"tokens"`
	UserBilled float64 `json:"user_billed"`
	// OldestAt is the time of the earliest usage record within the window, empty when the window has no usage. Under a sliding window
	// OldestAt+window length is the moment the quota starts to decay back, shown on the self-service usage page (issue #460).
	OldestAt *time.Time `json:"oldest_at,omitempty"`
}

// StartOfDay returns midnight of the calendar day containing t (keeping the time zone of t). The calendar-day limit and
// key-usage display share this, so the day boundary used for checks matches the one shown.
func StartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// GetAPIKeyWindowUsage aggregates the usage of the given API key within the time window [now-window, now].
// Used for API key level sliding-window limit checks (rpm/rpd/cost_5h/cost_7d/token_5h/token_7d).
// A manual quota reset only truncates the 5h/7d windows; RPM/RPD/30d and calendar-day windows keep their original definition.
func (db *DB) GetAPIKeyWindowUsage(ctx context.Context, apiKeyID int64, window time.Duration) (*APIKeyWindowUsage, error) {
	if window <= 0 {
		return &APIKeyWindowUsage{}, nil
	}
	return db.getAPIKeyUsageSince(ctx, apiKeyID, time.Now().Add(-window), isResettableAPIKeyWindow(window))
}

func isResettableAPIKeyWindow(window time.Duration) bool {
	return window == 5*time.Hour || window == 7*24*time.Hour
}

// GetAPIKeyUsageSince aggregates the usage of the given API key since "since". The calendar-day limit
// (issue #460) reuses this same query with midnight of the current day as "since".
// Index idx_usage_logs_api_key_created_at keeps this query at O(log n) even when the data volume is large.
func (db *DB) GetAPIKeyUsageSince(ctx context.Context, apiKeyID int64, since time.Time) (*APIKeyWindowUsage, error) {
	return db.getAPIKeyUsageSince(ctx, apiKeyID, since, false)
}

func (db *DB) getAPIKeyUsageSince(ctx context.Context, apiKeyID int64, since time.Time, honorReset bool) (*APIKeyWindowUsage, error) {
	if apiKeyID <= 0 || since.IsZero() {
		return &APIKeyWindowUsage{}, nil
	}
	usage := &APIKeyWindowUsage{}
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(user_billed), 0),
			MIN(created_at)
		FROM usage_logs
		WHERE api_key_id = $1
		  AND created_at >= $2
		  AND status_code <> 499
	`
	if honorReset {
		query = `
			SELECT
				COUNT(*),
				COALESCE(SUM(u.total_tokens), 0),
				COALESCE(SUM(u.user_billed), 0),
				MIN(u.created_at)
			FROM usage_logs u
			JOIN api_keys k ON k.id = u.api_key_id
			WHERE u.api_key_id = $1
			  AND u.created_at >= $2
			  AND (k.last_reset_at IS NULL OR u.created_at >= k.last_reset_at)
			  AND u.status_code <> 499
		`
	}
	var oldestRaw interface{}
	err := db.conn.QueryRowContext(ctx, query, apiKeyID, db.timeArg(since)).Scan(
		&usage.Requests, &usage.Tokens, &usage.UserBilled, &oldestRaw,
	)
	if err != nil {
		return nil, err
	}
	if oldest, err := parseDBTimeValue(oldestRaw); err == nil && !oldest.IsZero() {
		usage.OldestAt = &oldest
	}
	return usage, nil
}

// GetAPIKeyAccountWindowUsage aggregates the usage of the given API key within the window, **split by account**,
// used for scope-level limit checks (issue #439). One query covers all scope entries of this key:
// group-level totals are computed by the caller from each account's current group, so membership changes take effect immediately, without needing
// a redundant group_id in usage_logs.
//
// Reuses index idx_usage_logs_api_key_created_at; the scan volume is on par with the key-level cost aggregation over the same window.
// Accounts deleted from the account pool still appear in the return value, but the caller cannot map them to a group, so this part of the historical
// usage is ignored at group level (account-level totals stay accurate).
func (db *DB) GetAPIKeyAccountWindowUsage(ctx context.Context, apiKeyID int64, window time.Duration) (map[int64]APIKeyWindowUsage, error) {
	if apiKeyID <= 0 || window <= 0 {
		return map[int64]APIKeyWindowUsage{}, nil
	}
	since := time.Now().Add(-window)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			COALESCE(account_id, 0),
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE api_key_id = $1
		  AND created_at >= $2
		  AND status_code <> 499
		GROUP BY account_id
	`, apiKeyID, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]APIKeyWindowUsage)
	for rows.Next() {
		var accountID int64
		var usage APIKeyWindowUsage
		if err := rows.Scan(&accountID, &usage.Requests, &usage.Tokens, &usage.UserBilled); err != nil {
			return nil, err
		}
		if accountID <= 0 {
			continue
		}
		out[accountID] = usage
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAPIKeyAccountWindowsUsage aggregates all supported scope windows in one
// index range scan. The previous caller issued one GROUP BY query per window,
// making the 30-day view scan the same rows up to four times.
func (db *DB) GetAPIKeyAccountWindowsUsage(ctx context.Context, apiKeyID int64) (map[string]map[int64]APIKeyWindowUsage, error) {
	out := make(map[string]map[int64]APIKeyWindowUsage, len(APIKeyScopeWindows))
	for _, window := range APIKeyScopeWindows {
		out[window.Label] = make(map[int64]APIKeyWindowUsage)
	}
	if apiKeyID <= 0 {
		return out, nil
	}

	now := time.Now()
	since5h := db.timeArg(now.Add(-5 * time.Hour))
	since1d := db.timeArg(now.Add(-24 * time.Hour))
	since7d := db.timeArg(now.Add(-7 * 24 * time.Hour))
	since30d := db.timeArg(now.Add(-30 * 24 * time.Hour))
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(account_id, 0),
			COALESCE(SUM(CASE WHEN created_at >= $2 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $2 THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $2 THEN user_billed ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $3 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $3 THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $3 THEN user_billed ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $4 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $4 THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= $4 THEN user_billed ELSE 0 END), 0),
			COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE api_key_id = $1 AND created_at >= $5 AND status_code <> 499
		GROUP BY account_id
	`, apiKeyID, since5h, since1d, since7d, since30d)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID int64
		var fiveHours, oneDay, sevenDays, thirtyDays APIKeyWindowUsage
		if err := rows.Scan(&accountID,
			&fiveHours.Requests, &fiveHours.Tokens, &fiveHours.UserBilled,
			&oneDay.Requests, &oneDay.Tokens, &oneDay.UserBilled,
			&sevenDays.Requests, &sevenDays.Tokens, &sevenDays.UserBilled,
			&thirtyDays.Requests, &thirtyDays.Tokens, &thirtyDays.UserBilled,
		); err != nil {
			return nil, err
		}
		if accountID <= 0 {
			continue
		}
		out["5h"][accountID] = fiveHours
		out["1d"][accountID] = oneDay
		out["7d"][accountID] = sevenDays
		out["30d"][accountID] = thirtyDays
	}
	return out, rows.Err()
}

// GetAPIKeysAccountWindowUsage is the batch version of GetAPIKeyAccountWindowUsage: one query returns
// usage split by account within the same window for several API keys, used by the list page to show scope budget progress (issue #439).
// Returns an empty table when apiKeyIDs is empty (deliberately not degraded into a full-table aggregation, so the list page does not trigger a large query by mistake).
func (db *DB) GetAPIKeysAccountWindowUsage(ctx context.Context, apiKeyIDs []int64, window time.Duration) (map[int64]map[int64]APIKeyWindowUsage, error) {
	out := make(map[int64]map[int64]APIKeyWindowUsage)
	if len(apiKeyIDs) == 0 || window <= 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(apiKeyIDs))
	args := make([]interface{}, 0, len(apiKeyIDs)+1)
	for i, id := range apiKeyIDs {
		if db.isSQLite() {
			placeholders = append(placeholders, "?")
		} else {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		}
		args = append(args, id)
	}
	sincePlaceholder := "?"
	if !db.isSQLite() {
		sincePlaceholder = fmt.Sprintf("$%d", len(apiKeyIDs)+1)
	}
	args = append(args, db.timeArg(time.Now().Add(-window)))

	query := fmt.Sprintf(`
		SELECT
			api_key_id,
			COALESCE(account_id, 0),
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE api_key_id IN (%s)
		  AND created_at >= %s
		  AND status_code <> 499
		GROUP BY api_key_id, account_id
	`, strings.Join(placeholders, ","), sincePlaceholder)

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var apiKeyID, accountID int64
		var usage APIKeyWindowUsage
		if err := rows.Scan(&apiKeyID, &accountID, &usage.Requests, &usage.Tokens, &usage.UserBilled); err != nil {
			return nil, err
		}
		if apiKeyID <= 0 || accountID <= 0 {
			continue
		}
		if out[apiKeyID] == nil {
			out[apiKeyID] = make(map[int64]APIKeyWindowUsage)
		}
		out[apiKeyID][accountID] = usage
	}
	return out, rows.Err()
}

// APIKeyTokenStat is one entry in the token usage ranking of an API key within a time range.
// More granular than UsageAPIKeyStat: input / output / cached tokens are listed separately so the UI can sort each one on its own.
type APIKeyTokenStat struct {
	APIKeyID     int64   `json:"api_key_id"`
	APIKeyName   string  `json:"api_key_name"`
	APIKeyMasked string  `json:"api_key_masked"`
	Label        string  `json:"label"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	ErrorCount   int64   `json:"error_count"`
	UserBilled   float64 `json:"user_billed"`
}

// ListAPIKeyTokenStats returns token usage aggregated by API key within [rangeStart, rangeEnd).
// Both times may be zero values; a zero rangeStart means "today 0:00", and a zero rangeEnd means "until now".
// The result is **not limited in count**, consistent with issue #162; the frontend handles sorting / search / paging.
func (db *DB) ListAPIKeyTokenStats(ctx context.Context, rangeStart, rangeEnd time.Time) ([]APIKeyTokenStat, error) {
	now := time.Now()
	if rangeStart.IsZero() {
		rangeStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}

	query := `
		SELECT
			COALESCE(api_key_id, 0) AS api_key_id,
			COALESCE(api_key_name, '') AS api_key_name,
			COALESCE(api_key_masked, '') AS api_key_masked,
			COUNT(*) AS requests,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(total_tokens), 0) AS total_tokens,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count,
			COALESCE(SUM(user_billed), 0) AS user_billed
		FROM usage_logs
		WHERE status_code <> 499
		  AND created_at >= $1
	`
	args := []interface{}{db.timeArg(rangeStart)}
	if !rangeEnd.IsZero() {
		query += " AND created_at < $2"
		args = append(args, db.timeArg(rangeEnd))
	}
	query += " GROUP BY 1, 2, 3 ORDER BY total_tokens DESC, requests DESC"

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]APIKeyTokenStat, 0, 16)
	for rows.Next() {
		var item APIKeyTokenStat
		if err := rows.Scan(
			&item.APIKeyID,
			&item.APIKeyName,
			&item.APIKeyMasked,
			&item.Requests,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CachedTokens,
			&item.TotalTokens,
			&item.ErrorCount,
			&item.UserBilled,
		); err != nil {
			return nil, err
		}
		// Compute the label (the frontend can display it directly): prefer name, then masked, otherwise "unknown"
		switch {
		case item.APIKeyName != "":
			item.Label = item.APIKeyName
		case item.APIKeyMasked != "":
			item.Label = item.APIKeyMasked
		default:
			item.Label = "unknown"
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// APIKeyAccountGroup is the condensed display item for the group an upstream account belongs to (used in token usage details).
type APIKeyAccountGroup struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// APIKeyAccountStat is the usage item of a single API key within a time range, split by upstream account.
// It is the transpose of AccountKeyStat (account → keys): here it is key → accounts.
type APIKeyAccountStat struct {
	AccountID      int64                `json:"account_id"`
	AccountName    string               `json:"account_name"`
	AccountEmail   string               `json:"account_email"`
	AccountDeleted bool                 `json:"account_deleted"`
	Groups         []APIKeyAccountGroup `json:"groups,omitempty"`
	Requests       int64                `json:"requests"`
	InputTokens    int64                `json:"input_tokens"`
	OutputTokens   int64                `json:"output_tokens"`
	CachedTokens   int64                `json:"cached_tokens"`
	TotalTokens    int64                `json:"total_tokens"`
	ErrorCount     int64                `json:"error_count"`
	AccountBilled  float64              `json:"account_billed"`
	UserBilled     float64              `json:"user_billed"`
}

// ListAPIKeyAccountStats returns the usage of one API key within [rangeStart, rangeEnd), aggregated by upstream account.
// A zero rangeStart means "today 0:00" and a zero rangeEnd means "until now", the same semantics as ListAPIKeyTokenStats.
// Account labels (name/email) are JOINed from the accounts table; email is stored in the credentials JSON and parsed on the Go side.
func (db *DB) ListAPIKeyAccountStats(ctx context.Context, apiKeyID int64, rangeStart, rangeEnd time.Time) ([]APIKeyAccountStat, error) {
	now := time.Now()
	if rangeStart.IsZero() {
		rangeStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}

	query := `
		WITH aggregated AS (
			SELECT
				account_id,
				COUNT(*) AS requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
				COALESCE(SUM(total_tokens), 0) AS total_tokens,
				COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count,
				COALESCE(SUM(account_billed), 0) AS account_billed,
				COALESCE(SUM(user_billed), 0) AS user_billed
			FROM usage_logs
			WHERE api_key_id = $1 AND status_code <> 499 AND created_at >= $2
	`
	args := []interface{}{apiKeyID, db.timeArg(rangeStart)}
	if !rangeEnd.IsZero() {
		query += " AND created_at < $3"
		args = append(args, db.timeArg(rangeEnd))
	}
	query += `
			GROUP BY account_id
		)
		SELECT
			u.account_id,
			COALESCE(a.name, '') AS account_name,
			COALESCE(CAST(a.credentials AS TEXT), '{}') AS credentials,
			COALESCE(a.status, '') AS account_status,
			COALESCE(a.error_message, '') AS account_error,
			u.requests, u.input_tokens, u.output_tokens, u.cached_tokens,
			u.total_tokens, u.error_count, u.account_billed, u.user_billed
		FROM aggregated u
		LEFT JOIN accounts a ON u.account_id = a.id
	`
	query += " ORDER BY u.requests DESC, u.total_tokens DESC"

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]APIKeyAccountStat, 0, 16)
	for rows.Next() {
		var item APIKeyAccountStat
		var credentials string
		var accountStatus string
		var accountError string
		if err := rows.Scan(
			&item.AccountID,
			&item.AccountName,
			&credentials,
			&accountStatus,
			&accountError,
			&item.Requests,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CachedTokens,
			&item.TotalTokens,
			&item.ErrorCount,
			&item.AccountBilled,
			&item.UserBilled,
		); err != nil {
			return nil, err
		}
		item.AccountEmail = emailFromCredentialsJSON(credentials)
		item.AccountDeleted = strings.EqualFold(strings.TrimSpace(accountStatus), "deleted") ||
			strings.EqualFold(strings.TrimSpace(accountError), "deleted")
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.attachAPIKeyAccountGroups(ctx, items); err != nil {
		return nil, err
	}
	return items, nil
}

// attachAPIKeyAccountGroups batch-fills the group labels of upstream accounts to avoid N+1.
// account_group_members keeps each account's last group membership from before it was soft-deleted into the recycle bin, so this does not filter by
// accounts.status; historical usage statistics must be able to see that snapshot.
func (db *DB) attachAPIKeyAccountGroups(ctx context.Context, items []APIKeyAccountStat) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if item.AccountID <= 0 {
			continue
		}
		if _, ok := seen[item.AccountID]; ok {
			continue
		}
		seen[item.AccountID] = struct{}{}
		ids = append(ids, item.AccountID)
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		if db.isSQLite() {
			placeholders[i] = "?"
		} else {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT m.account_id, g.id, g.name, COALESCE(g.color, '')
		FROM account_group_members m
		INNER JOIN account_groups g ON g.id = m.group_id
		WHERE m.account_id IN (%s)
		ORDER BY m.account_id, g.sort_order, g.name`, strings.Join(placeholders, ","))

	groupRows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer groupRows.Close()

	byAccount := make(map[int64][]APIKeyAccountGroup, len(ids))
	for groupRows.Next() {
		var accountID int64
		var group APIKeyAccountGroup
		if err := groupRows.Scan(&accountID, &group.ID, &group.Name, &group.Color); err != nil {
			return err
		}
		byAccount[accountID] = append(byAccount[accountID], group)
	}
	if err := groupRows.Err(); err != nil {
		return err
	}

	for i := range items {
		if groups := byAccount[items[i].AccountID]; len(groups) > 0 {
			items[i].Groups = groups
		}
	}
	return nil
}

// emailFromCredentialsJSON takes the email used for display from the account credentials JSON text;
// when email is absent it falls back to base_url.
func emailFromCredentialsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	if s, ok := m["email"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["base_url"].(string); ok {
		return s
	}
	return ""
}

// ListAPIKeyLastUsedAt returns the most recent request time of each API key (from usage_logs).
// Only includes keys that have call records. For each key it reads, via idx_usage_logs_api_key_created_at in descending order, the first
// non-499 record; the cost is O(number of keys × log n). The old form, GROUP BY api_key_id, had to read back each row's
// status_code, which meant the entire history was scanned every time the key list was opened.
func (db *DB) ListAPIKeyLastUsedAt(ctx context.Context) (map[int64]time.Time, error) {
	query := `
		SELECT k.id, (
			SELECT u.created_at
			FROM usage_logs u
			WHERE u.api_key_id = k.id
			  AND u.status_code <> 499
			ORDER BY u.created_at DESC
			LIMIT 1
		)
		FROM api_keys k
	`
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]time.Time)
	for rows.Next() {
		var id int64
		var lastUsedRaw interface{}
		if err := rows.Scan(&id, &lastUsedRaw); err != nil {
			return nil, err
		}
		t, err := parseDBTimeValue(lastUsedRaw)
		if err != nil {
			return nil, err
		}
		if !t.IsZero() {
			result[id] = t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetAllAPIKeysWindowCost batch-aggregates user_billed of all API keys within the window [now-window, now].
// Returns map[apiKeyID] → cost. Only includes keys that have usage records.
func (db *DB) GetAllAPIKeysWindowCost(ctx context.Context, window time.Duration) (map[int64]float64, error) {
	if window <= 0 {
		return make(map[int64]float64), nil
	}
	return db.getAllAPIKeysCostSince(ctx, time.Now().Add(-window), isResettableAPIKeyWindow(window))
}

// GetAllAPIKeysCostSince batch-aggregates user_billed of all API keys since "since".
// The calendar-day limit (issue #460) reuses this query with midnight of the current day as "since".
func (db *DB) GetAllAPIKeysCostSince(ctx context.Context, since time.Time) (map[int64]float64, error) {
	return db.getAllAPIKeysCostSince(ctx, since, false)
}

func (db *DB) getAllAPIKeysCostSince(ctx context.Context, since time.Time, honorReset bool) (map[int64]float64, error) {
	if since.IsZero() {
		return make(map[int64]float64), nil
	}
	query := `
		SELECT api_key_id, COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE api_key_id > 0
		  AND created_at >= $1
		  AND status_code <> 499
		GROUP BY api_key_id
	`
	if honorReset {
		query = `
			SELECT u.api_key_id, COALESCE(SUM(u.user_billed), 0)
			FROM usage_logs u
			JOIN api_keys k ON k.id = u.api_key_id
			WHERE u.api_key_id > 0
			  AND u.created_at >= $1
			  AND (k.last_reset_at IS NULL OR u.created_at >= k.last_reset_at)
			  AND u.status_code <> 499
			GROUP BY u.api_key_id
		`
	}
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]float64)
	for rows.Next() {
		var id int64
		var cost float64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		result[id] = cost
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// APIKeySelfUsageReport is the read-only aggregate result used by the API key self-service statistics page.
// It only contains the current key's own usage_logs data (including the client IP and User-Agent of requests sent with this key;
// holders sharing the same key can see each other's), and does not return logs of other keys, the account pool, raw keys or other admin-only fields.
type APIKeySelfUsageReport struct {
	Summary            APIKeySelfUsageSummary     `json:"summary"`
	Windows            APIKeySelfUsageWindows     `json:"windows"`
	Models             []APIKeySelfUsageBreakdown `json:"models"`
	Endpoints          []APIKeySelfUsageBreakdown `json:"endpoints"`
	RecentLogs         []APIKeySelfUsageLog       `json:"recent_logs"`
	RecentLogsTotal    int64                      `json:"recent_logs_total"`
	RecentLogsPage     int                        `json:"recent_logs_page"`
	RecentLogsPageSize int                        `json:"recent_logs_page_size"`
	// LogModels / LogEndpoints are the request models and inbound endpoints seen in the time range (not affected by log filters),
	// used by the log filter dropdowns on the self-service page.
	LogModels    []string `json:"log_models"`
	LogEndpoints []string `json:"log_endpoints"`
}

// APIKeySelfLogFilter is the filter condition for the self-service request log; it only applies to recent_logs,
// and does not affect the summary, windows or rankings.
type APIKeySelfLogFilter struct {
	// Model matches both the requested model and the effective model.
	Model string
	// Endpoint matches the inbound endpoint (same basis as recent_logs.endpoint).
	Endpoint string
	// Status: success | error | 4xx | 5xx | 429; empty means no limit.
	Status string
	// Stream: stream | sync; empty means no limit.
	Stream string
	// Channel: codex; empty means no limit.
	Channel string
}

const apiKeySelfLogOptionLimit = 50

type APIKeySelfUsageSummary struct {
	Requests        int64   `json:"requests"`
	Tokens          int64   `json:"tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	ErrorCount      int64   `json:"error_count"`
	UserBilled      float64 `json:"user_billed"`
	AvgDurationMS   float64 `json:"avg_duration_ms"`
	AvgFirstTokenMS float64 `json:"avg_first_token_ms"`
	RPM             int64   `json:"rpm"`
	TPM             int64   `json:"tpm"`
}

// APIKeySelfUsageWindow is a window entry of the self-service page: on top of the raw aggregate it carries the window semantics and the reset / decay
// moments (issue #460), computed on the server so the frontend does no local estimation.
type APIKeySelfUsageWindow struct {
	APIKeyWindowUsage
	// WindowKind: fixed=calendar-day fixed window, fully reset to zero at ResetAt;
	// sliding=sliding window, with no single reset moment; DecayAt is the time when the earliest usage record rolls out of the window
	// and the quota starts to decay back (empty when the window has no usage).
	WindowKind string     `json:"window_kind"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
	DecayAt    *time.Time `json:"decay_at,omitempty"`
}

type APIKeySelfUsageWindows struct {
	Today   APIKeySelfUsageWindow `json:"today"`
	Last5h  APIKeySelfUsageWindow `json:"last_5h"`
	Last7d  APIKeySelfUsageWindow `json:"last_7d"`
	Last30d APIKeySelfUsageWindow `json:"last_30d"`
}

type APIKeySelfUsageBreakdown struct {
	Name         string  `json:"name"`
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	ErrorCount   int64   `json:"error_count"`
	UserBilled   float64 `json:"user_billed"`
}

type APIKeySelfUsageLog struct {
	UserBilling
	ID                     int64     `json:"id"`
	Channel                string    `json:"channel"`
	ClientIP               string    `json:"client_ip"`
	ClientUserAgent        string    `json:"client_user_agent"`
	Endpoint               string    `json:"endpoint"`
	Model                  string    `json:"model"`
	EffectiveModel         string    `json:"effective_model"`
	DaybreakProgram        string    `json:"daybreak_program"`
	StatusCode             int       `json:"status_code"`
	DurationMS             int       `json:"duration_ms"`
	FirstTokenMS           int       `json:"first_token_ms"`
	InputTokens            int       `json:"input_tokens"`
	OutputTokens           int       `json:"output_tokens"`
	ReasoningTokens        int       `json:"reasoning_tokens"`
	ReasoningEffort        string    `json:"reasoning_effort"`
	CachedTokens           int       `json:"cached_tokens"`
	CacheWrite5mTokens     int       `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens     int       `json:"cache_write_1h_tokens"`
	ImageInputTokens       int       `json:"image_input_tokens"`
	ImageOutputTokens      int       `json:"image_output_tokens"`
	CachedImageInputTokens int       `json:"cached_image_input_tokens"`
	TotalTokens            int       `json:"total_tokens"`
	UserBilled             float64   `json:"user_billed"`
	InputCost              float64   `json:"input_cost"`
	OutputCost             float64   `json:"output_cost"`
	CacheReadCost          float64   `json:"cache_read_cost"`
	TotalCost              float64   `json:"total_cost"`
	InputPrice             float64   `json:"input_price_per_mtoken"`
	OutputPrice            float64   `json:"output_price_per_mtoken"`
	CacheReadPrice         float64   `json:"cache_read_price_per_mtoken"`
	RateMultiplier         float64   `json:"rate_multiplier"`
	LongContext            bool      `json:"long_context"`
	ServiceTier            string    `json:"service_tier"`
	Stream                 bool      `json:"stream"`
	Compact                bool      `json:"compact"`
	HasCompactionHistory   bool      `json:"has_compaction_history"`
	ViaWebsocket           bool      `json:"via_websocket"`
	UpstreamErrorKind      string    `json:"upstream_error_kind"`
	CreatedAt              time.Time `json:"created_at"`
}

// populateBillingBreakdown reuses the billing breakdown logic that matches the admin side: by effective model, Daybreak program and billing tier,
// it restores the cost and unit price of input / output / cached reads, and proportionally scales them to align when they differ from the actual billed total.
func (l *APIKeySelfUsageLog) populateBillingBreakdown() {
	if IsUnitUserBillingMode(l.UserBillingMode) {
		l.TotalCost = l.UserBilled
		return
	}
	billingModel := l.EffectiveModel
	if billingModel == "" {
		billingModel = l.Model
	}
	breakdown := UsageLogCostBreakdown(&UsageLogInput{Model: billingModel, DaybreakProgram: l.DaybreakProgram, ServiceTier: l.ServiceTier, InputTokens: l.InputTokens, OutputTokens: l.OutputTokens, CachedTokens: l.CachedTokens, ImageInputTokens: l.ImageInputTokens, ImageOutputTokens: l.ImageOutputTokens, CachedImageInputTokens: l.CachedImageInputTokens})
	l.InputCost = breakdown.InputCost
	l.OutputCost = breakdown.OutputCost
	l.CacheReadCost = breakdown.CacheReadCost
	l.TotalCost = breakdown.TotalCost
	l.InputPrice = breakdown.InputPricePerMToken
	l.OutputPrice = breakdown.OutputPricePerMToken
	l.CacheReadPrice = breakdown.CacheReadPricePerMToken
	l.RateMultiplier = breakdown.ServiceTierCostMultiplier
	l.LongContext = breakdown.LongContext

	if l.UserBilled > 0 && breakdown.TotalCost > 0 && l.UserBilled != breakdown.TotalCost {
		scale := l.UserBilled / breakdown.TotalCost
		l.InputCost *= scale
		l.OutputCost *= scale
		l.CacheReadCost *= scale
		l.TotalCost = l.UserBilled
		l.InputPrice *= scale
		l.OutputPrice *= scale
		l.CacheReadPrice *= scale
	}
}

func (db *DB) GetAPIKeySelfUsageReport(ctx context.Context, apiKeyID int64, rangeStart, rangeEnd time.Time, recentPage, recentPageSize int) (*APIKeySelfUsageReport, error) {
	return db.GetAPIKeySelfUsageReportFiltered(ctx, apiKeyID, rangeStart, rangeEnd, recentPage, recentPageSize, APIKeySelfLogFilter{})
}

func (db *DB) GetAPIKeySelfUsageReportFiltered(ctx context.Context, apiKeyID int64, rangeStart, rangeEnd time.Time, recentPage, recentPageSize int, logFilter APIKeySelfLogFilter) (*APIKeySelfUsageReport, error) {
	recentPage, recentPageSize = normalizeAPIKeySelfRecentLogPagination(recentPage, recentPageSize)
	if apiKeyID <= 0 {
		return &APIKeySelfUsageReport{
			Models:             []APIKeySelfUsageBreakdown{},
			Endpoints:          []APIKeySelfUsageBreakdown{},
			RecentLogs:         []APIKeySelfUsageLog{},
			RecentLogsPage:     recentPage,
			RecentLogsPageSize: recentPageSize,
			LogModels:          []string{},
			LogEndpoints:       []string{},
		}, nil
	}

	report := &APIKeySelfUsageReport{
		RecentLogsPage:     recentPage,
		RecentLogsPageSize: recentPageSize,
	}
	var err error
	// Summary, rankings, filter options and the four quota windows come from one scan. Previously more than ten aggregations each scanned the range
	// one after another, with the self-service page defaulting to 30 days and an 8-second overall budget; on a slow-disk SQLite, valid keys could also "fail to log in" because the report timed out.
	if err = db.fillAPIKeySelfAggregates(ctx, report, apiKeyID, rangeStart, rangeEnd, time.Now()); err != nil {
		return nil, err
	}
	report.RecentLogs, report.RecentLogsTotal, report.RecentLogsPage, report.RecentLogsPageSize, err = db.listAPIKeySelfRecentLogs(ctx, apiKeyID, rangeStart, rangeEnd, recentPage, recentPageSize, logFilter, report.Summary.Requests)
	if err != nil {
		return nil, err
	}
	return report, nil
}

const apiKeySelfEndpointExpr = "COALESCE(NULLIF(inbound_endpoint, ''), NULLIF(endpoint, ''), 'unknown')"

// appendAPIKeySelfLogFilter appends the log filter conditions to the where clause generated by apiKeySelfUsageWhere.
func appendAPIKeySelfLogFilter(where string, args []interface{}, f APIKeySelfLogFilter) (string, []interface{}) {
	if model := strings.TrimSpace(f.Model); model != "" {
		args = append(args, model)
		p := fmt.Sprintf("$%d", len(args))
		where += fmt.Sprintf(" AND (COALESCE(model, '') = %s OR COALESCE(effective_model, '') = %s)", p, p)
	}
	if endpoint := strings.TrimSpace(f.Endpoint); endpoint != "" {
		args = append(args, endpoint)
		where += fmt.Sprintf(" AND %s = $%d", apiKeySelfEndpointExpr, len(args))
	}
	if channel := strings.TrimSpace(f.Channel); channel != "" {
		args = append(args, channel)
		where += fmt.Sprintf(" AND COALESCE(channel, '') = $%d", len(args))
	}
	switch strings.ToLower(strings.TrimSpace(f.Status)) {
	case "success":
		where += " AND status_code < 400"
	case "error":
		where += " AND status_code >= 400"
	case "4xx":
		where += " AND status_code >= 400 AND status_code < 500"
	case "5xx":
		where += " AND status_code >= 500"
	case "429":
		where += " AND status_code = 429"
	}
	switch strings.ToLower(strings.TrimSpace(f.Stream)) {
	case "stream":
		where += " AND COALESCE(stream, false) = true"
	case "sync":
		where += " AND COALESCE(stream, false) = false"
	}
	return where, args
}

func normalizeAPIKeySelfRecentLogPagination(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 25
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// Window semantics values (APIKeySelfUsageWindow.WindowKind).
const (
	usageWindowKindFixed   = "fixed"
	usageWindowKindSliding = "sliding"
)

func (db *DB) apiKeySelfUsageWhere(apiKeyID int64, rangeStart, rangeEnd time.Time) (string, []interface{}) {
	where := "api_key_id = $1 AND status_code <> 499"
	args := []interface{}{apiKeyID}
	if !rangeStart.IsZero() {
		args = append(args, db.timeArg(rangeStart))
		where += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !rangeEnd.IsZero() {
		args = append(args, db.timeArg(rangeEnd))
		where += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	return where, args
}

// fillAPIKeySelfAggregates fills in, at once, the range summary, the model / endpoint rankings (top 8), the log filter dropdown options,
// and the four quota windows: today / 5h / 7d / 30d.
//
// The scan range is the union of the range and the 30-day window. The range start and end, each window start, and "one minute ago" cut the timeline into segments,
// grouped by (display model, endpoint, requested model, time segment): each row computes its segment number once, and the aggregates are plain SUMs;
// the range, windows and RPM are merged in Go by segment. The grouping has only a few dozen to a few hundred rows.
// Whole-hour reads use the hourly usage summary; only the hour that holds a boundary falls back to a detail scan, so the detail rows read for the 30-day range drop from all rows
// to at most a few hours.
// The window basis matches GetAPIKeyWindowUsage / GetAPIKeyUsageSince, used for limit checks: 499 is excluded,
// and the 5h/7d windows are counted from the manual reset moment.
func (db *DB) fillAPIKeySelfAggregates(ctx context.Context, report *APIKeySelfUsageReport, apiKeyID int64, rangeStart, rangeEnd, now time.Time) error {
	var lastResetRaw interface{}
	err := db.conn.QueryRowContext(ctx, `SELECT last_reset_at FROM api_keys WHERE id = $1`, apiKeyID).Scan(&lastResetRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	lastReset, err := parseDBTimeValue(lastResetRaw)
	if err != nil {
		return err
	}

	dayStart := StartOfDay(now)
	resetAt := dayStart.AddDate(0, 0, 1)
	report.Windows = APIKeySelfUsageWindows{
		Today:   APIKeySelfUsageWindow{WindowKind: usageWindowKindFixed, ResetAt: &resetAt},
		Last5h:  APIKeySelfUsageWindow{WindowKind: usageWindowKindSliding},
		Last7d:  APIKeySelfUsageWindow{WindowKind: usageWindowKindSliding},
		Last30d: APIKeySelfUsageWindow{WindowKind: usageWindowKindSliding},
	}
	windows := []struct {
		target *APIKeySelfUsageWindow
		since  time.Time
		length time.Duration // 0 = calendar-day fixed window
	}{
		{&report.Windows.Today, dayStart, 0},
		{&report.Windows.Last5h, now.Add(-5 * time.Hour), 5 * time.Hour},
		{&report.Windows.Last7d, now.Add(-7 * 24 * time.Hour), 7 * 24 * time.Hour},
		{&report.Windows.Last30d, now.Add(-30 * 24 * time.Hour), 30 * 24 * time.Hour},
	}
	minuteAgo := now.Add(-1 * time.Minute)

	// Boundaries are sorted descending and deduplicated by time; segment k covers [bounds[k], bounds[k-1]), and the last segment covers everything before the last bound.
	// When the range end is "now", no upper bound is set, which saves one boundary.
	hasRangeEnd := !rangeEnd.IsZero() && now.Sub(rangeEnd) > time.Second
	scanStart := rangeStart
	bounds := []time.Time{minuteAgo}
	if !rangeStart.IsZero() {
		bounds = append(bounds, rangeStart)
	}
	if hasRangeEnd {
		bounds = append(bounds, rangeEnd)
	}
	for i := range windows {
		if windows[i].length > 0 && isResettableAPIKeyWindow(windows[i].length) && lastReset.After(windows[i].since) {
			windows[i].since = lastReset
		}
		if !scanStart.IsZero() && windows[i].since.Before(scanStart) {
			scanStart = windows[i].since
		}
		bounds = append(bounds, windows[i].since)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].After(bounds[j]) })
	bounds = slices.CompactFunc(bounds, time.Time.Equal)
	segmentsFrom := func(t time.Time) int { // t is a boundary: returns the number of segments with created_at >= t
		return slices.IndexFunc(bounds, t.Equal) + 1
	}
	segmentExpr := func(column string, args []interface{}) (string, []interface{}) {
		expr := "CASE"
		for k, bound := range bounds {
			args = append(args, db.timeArg(bound))
			expr += fmt.Sprintf(" WHEN %s >= $%d THEN %d", column, len(args), k)
		}
		return expr + fmt.Sprintf(" ELSE %d END", len(bounds)), args
	}

	rangeFirst, rangeLast := 0, len(bounds) // closed interval of segment numbers inside the range
	if hasRangeEnd {
		rangeFirst = segmentsFrom(rangeEnd)
	}
	if !rangeStart.IsZero() {
		rangeLast = segmentsFrom(rangeStart) - 1
	}
	minuteSegments := segmentsFrom(minuteAgo)
	windowSegments := make([]int, len(windows))
	for i := range windows {
		windowSegments[i] = segmentsFrom(windows[i].since)
	}

	var summary APIKeySelfUsageSummary
	var durationSum, durationCount, firstTokenSum, firstTokenCount int64
	models := map[string]*APIKeySelfUsageBreakdown{}
	endpoints := map[string]*APIKeySelfUsageBreakdown{}
	requestModels := map[string]int64{}
	windowOldest := make([]time.Time, len(windows))
	// The detail and summary queries share the same column layout, so they share this accumulation block.
	accumulate := func(query string, args []interface{}) error {
		rows, err := db.conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g APIKeySelfUsageBreakdown
			var endpoint, requestModel string
			var segment int
			var groupDurationSum, groupDurationCount, groupFirstTokenSum, groupFirstTokenCount int64
			var oldestRaw interface{}
			if err := rows.Scan(
				&g.Name, &endpoint, &requestModel, &segment,
				&g.Requests, &g.Tokens, &g.InputTokens, &g.OutputTokens, &g.CachedTokens, &g.ErrorCount, &g.UserBilled,
				&groupDurationSum, &groupDurationCount, &groupFirstTokenSum, &groupFirstTokenCount,
				&oldestRaw,
			); err != nil {
				return err
			}
			oldest, err := parseDBTimeValue(oldestRaw)
			if err != nil {
				return err
			}
			for i := range windows {
				if segment >= windowSegments[i] {
					continue
				}
				usage := &windows[i].target.APIKeyWindowUsage
				usage.Requests += g.Requests
				usage.Tokens += g.Tokens
				usage.UserBilled += g.UserBilled
				if !oldest.IsZero() && (windowOldest[i].IsZero() || oldest.Before(windowOldest[i])) {
					windowOldest[i] = oldest
				}
			}
			if segment < rangeFirst || segment > rangeLast {
				continue
			}
			summary.Requests += g.Requests
			summary.Tokens += g.Tokens
			summary.InputTokens += g.InputTokens
			summary.OutputTokens += g.OutputTokens
			summary.CachedTokens += g.CachedTokens
			summary.ErrorCount += g.ErrorCount
			summary.UserBilled += g.UserBilled
			if segment < minuteSegments {
				summary.RPM += g.Requests
				summary.TPM += g.Tokens
			}
			durationSum += groupDurationSum
			durationCount += groupDurationCount
			firstTokenSum += groupFirstTokenSum
			firstTokenCount += groupFirstTokenCount
			for _, bucket := range []struct {
				byName map[string]*APIKeySelfUsageBreakdown
				name   string
			}{{models, g.Name}, {endpoints, endpoint}} {
				item := bucket.byName[bucket.name]
				if item == nil {
					item = &APIKeySelfUsageBreakdown{Name: bucket.name}
					bucket.byName[bucket.name] = item
				}
				item.Requests += g.Requests
				item.Tokens += g.Tokens
				item.InputTokens += g.InputTokens
				item.OutputTokens += g.OutputTokens
				item.CachedTokens += g.CachedTokens
				item.ErrorCount += g.ErrorCount
				item.UserBilled += g.UserBilled
			}
			if requestModel != "" {
				requestModels[requestModel] += g.Requests
			}
		}
		return rows.Err()
	}

	plan := db.planUsageHourlyQuery(ctx, scanStart, time.Time{}, now, bounds...)
	for _, span := range plan.raw {
		args := []interface{}{apiKeyID}
		segment, args := segmentExpr("created_at", args)
		spanWhere, args := db.usageSpanWhere("created_at", span, args)
		query := `
			SELECT
				COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
				` + apiKeySelfEndpointExpr + `,
				COALESCE(model, ''),
				` + segment + `,
				COUNT(*),
				COALESCE(SUM(total_tokens), 0),
				COALESCE(SUM(input_tokens), 0),
				COALESCE(SUM(output_tokens), 0),
				COALESCE(SUM(cached_tokens), 0),
				COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(user_billed), 0),
				COALESCE(SUM(NULLIF(duration_ms, 0)), 0),
				COUNT(NULLIF(duration_ms, 0)),
				COALESCE(SUM(NULLIF(first_token_ms, 0)), 0),
				COUNT(NULLIF(first_token_ms, 0)),
				MIN(created_at)
			FROM usage_logs
			WHERE api_key_id = $1 AND status_code <> 499 AND ` + spanWhere + `
			GROUP BY 1, 2, 3, 4`
		if err := accumulate(query, args); err != nil {
			return err
		}
	}
	if plan.useRollup {
		args := []interface{}{apiKeyID}
		segment, args := segmentExpr("bucket", args)
		bucketWhere, args := db.usageHourlyBucketWhere("bucket", plan, args)
		query := `
			SELECT
				COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
				` + apiKeySelfEndpointExpr + `,
				model,
				` + segment + `,
				` + sumBigint("requests") + `,
				` + sumBigint("total_tokens") + `,
				` + sumBigint("input_tokens") + `,
				` + sumBigint("output_tokens") + `,
				` + sumBigint("cached_tokens") + `,
				` + sumBigint("CASE WHEN status_code >= 400 THEN requests ELSE 0 END") + `,
				COALESCE(SUM(user_billed), 0),
				` + sumBigint("duration_ms_sum") + `,
				` + sumBigint("duration_samples") + `,
				` + sumBigint("first_token_ms_sum") + `,
				` + sumBigint("first_token_samples") + `,
				MIN(min_created_at)
			FROM usage_log_hourly
			WHERE api_key_id = $1 AND status_code <> 499 AND ` + bucketWhere + `
			GROUP BY 1, 2, 3, 4`
		if err := accumulate(query, args); err != nil {
			return err
		}
	}

	for i, w := range windows {
		if windowOldest[i].IsZero() {
			continue
		}
		oldest := windowOldest[i]
		w.target.OldestAt = &oldest
		if w.length > 0 {
			decay := oldest.Add(w.length)
			w.target.DecayAt = &decay
		}
	}
	if durationCount > 0 {
		summary.AvgDurationMS = float64(durationSum) / float64(durationCount)
	}
	if firstTokenCount > 0 {
		summary.AvgFirstTokenMS = float64(firstTokenSum) / float64(firstTokenCount)
	}
	report.Summary = summary
	report.Models = topAPIKeySelfBreakdowns(models, 8)
	report.Endpoints = topAPIKeySelfBreakdowns(endpoints, 8)
	report.LogModels = apiKeySelfOptionsByCount(requestModels)
	endpointCounts := make(map[string]int64, len(endpoints))
	for name, item := range endpoints {
		endpointCounts[name] = item.Requests
	}
	report.LogEndpoints = apiKeySelfOptionsByCount(endpointCounts)
	return nil
}

// topAPIKeySelfBreakdowns takes the top limit items, sorted by billing descending, then request count descending, then name ascending.
func topAPIKeySelfBreakdowns(byName map[string]*APIKeySelfUsageBreakdown, limit int) []APIKeySelfUsageBreakdown {
	items := make([]APIKeySelfUsageBreakdown, 0, len(byName))
	for _, item := range byName {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UserBilled != items[j].UserBilled {
			return items[i].UserBilled > items[j].UserBilled
		}
		if items[i].Requests != items[j].Requests {
			return items[i].Requests > items[j].Requests
		}
		return items[i].Name < items[j].Name
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// apiKeySelfOptionsByCount lists filter dropdown options sorted by request volume descending, then value ascending, up to apiKeySelfLogOptionLimit items.
func apiKeySelfOptionsByCount(counts map[string]int64) []string {
	values := make([]string, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if counts[values[i]] != counts[values[j]] {
			return counts[values[i]] > counts[values[j]]
		}
		return values[i] < values[j]
	})
	if len(values) > apiKeySelfLogOptionLimit {
		values = values[:apiKeySelfLogOptionLimit]
	}
	return values
}

// countAPIKeySelfLogs counts the rows that match the self-service log filter. The filter columns are all dimensions of the hourly usage summary,
// so whole hours are read from the hourly summary and only the head and tail remainders are scanned from details; with a filter there is no need to read every detail row in the range.
func (db *DB) countAPIKeySelfLogs(ctx context.Context, apiKeyID int64, rangeStart, rangeEnd time.Time, filter APIKeySelfLogFilter) (int64, error) {
	var total int64
	plan := db.planUsageHourlyQuery(ctx, rangeStart, rangeEnd, time.Now())
	for _, span := range plan.raw {
		where, args := db.apiKeySelfUsageWhere(apiKeyID, span.start, span.end)
		where, args = appendAPIKeySelfLogFilter(where, args, filter)
		var count int64
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE `+where, args...).Scan(&count); err != nil {
			return 0, err
		}
		total += count
	}
	if plan.useRollup {
		bucketWhere, args := db.usageHourlyBucketWhere("bucket", plan, []interface{}{apiKeyID})
		where, args := appendAPIKeySelfLogFilter("api_key_id = $1 AND status_code <> 499 AND "+bucketWhere, args, filter)
		var count int64
		if err := db.conn.QueryRowContext(ctx, `SELECT `+sumBigint("requests")+` FROM usage_log_hourly WHERE `+where, args...).Scan(&count); err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// listAPIKeySelfRecentLogs pages through the self-service request log. When knownTotal >= 0 the caller has already obtained
// the total on the same basis from the range summary; with an empty filter it is reused directly, saving one count scan.
func (db *DB) listAPIKeySelfRecentLogs(ctx context.Context, apiKeyID int64, rangeStart, rangeEnd time.Time, page, pageSize int, filter APIKeySelfLogFilter, knownTotal int64) ([]APIKeySelfUsageLog, int64, int, int, error) {
	page, pageSize = normalizeAPIKeySelfRecentLogPagination(page, pageSize)
	baseWhere, args := db.apiKeySelfUsageWhere(apiKeyID, rangeStart, rangeEnd)
	where, args := appendAPIKeySelfLogFilter(baseWhere, args, filter)

	total := knownTotal
	if total < 0 || where != baseWhere {
		var err error
		if total, err = db.countAPIKeySelfLogs(ctx, apiKeyID, rangeStart, rangeEnd, filter); err != nil {
			return nil, 0, page, pageSize, err
		}
	}
	if total > 0 {
		totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))
		if page > totalPages {
			page = totalPages
		}
	}

	offset := (page - 1) * pageSize
	args = append(args, pageSize, offset)
	limitArg := fmt.Sprintf("$%d", len(args)-1)
	offsetArg := fmt.Sprintf("$%d", len(args))
	query := `
		SELECT
			id,
			COALESCE(channel, ''),
			COALESCE(client_ip, ''), COALESCE(client_user_agent, ''),
			` + apiKeySelfEndpointExpr + ` AS endpoint_name,
			COALESCE(model, ''),
			COALESCE(effective_model, ''),
			COALESCE(daybreak_program, ''),
			COALESCE(status_code, 0),
			COALESCE(duration_ms, 0),
			COALESCE(first_token_ms, 0),
			COALESCE(input_tokens, 0),
			COALESCE(output_tokens, 0),
			COALESCE(reasoning_tokens, 0),
			COALESCE(reasoning_effort, ''),
			COALESCE(cached_tokens, 0),
			COALESCE(cache_write_5m_tokens, 0), COALESCE(cache_write_1h_tokens, 0),
			COALESCE(image_input_tokens, 0), COALESCE(image_output_tokens, 0), COALESCE(cached_image_input_tokens, 0),
			COALESCE(total_tokens, 0),
			COALESCE(user_billed, 0),
			COALESCE(user_billing_mode, ''), COALESCE(image_unit_price, 0), COALESCE(billed_image_count, 0),
			COALESCE(NULLIF(billing_service_tier, ''), NULLIF(actual_service_tier, ''), NULLIF(service_tier, ''), ''),
			COALESCE(stream, false),
			COALESCE(compact, false),
			COALESCE(has_compaction_history, false),
			COALESCE(via_websocket, false),
			COALESCE(upstream_error_kind, ''),
			created_at
		FROM usage_logs
		WHERE ` + where + `
		ORDER BY created_at DESC, id DESC
		LIMIT ` + limitArg + ` OFFSET ` + offsetArg
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, page, pageSize, err
	}
	defer rows.Close()

	items := make([]APIKeySelfUsageLog, 0, pageSize)
	for rows.Next() {
		var item APIKeySelfUsageLog
		var createdAtRaw interface{}
		if err := rows.Scan(
			&item.ID,
			&item.Channel,
			&item.ClientIP, &item.ClientUserAgent,
			&item.Endpoint,
			&item.Model,
			&item.EffectiveModel,
			&item.DaybreakProgram,
			&item.StatusCode,
			&item.DurationMS,
			&item.FirstTokenMS,
			&item.InputTokens,
			&item.OutputTokens,
			&item.ReasoningTokens,
			&item.ReasoningEffort,
			&item.CachedTokens, &item.CacheWrite5mTokens, &item.CacheWrite1hTokens,
			&item.ImageInputTokens, &item.ImageOutputTokens, &item.CachedImageInputTokens,
			&item.TotalTokens,
			&item.UserBilled, &item.UserBillingMode, &item.ImageUnitPrice, &item.BilledImageCount,
			&item.ServiceTier,
			&item.Stream,
			&item.Compact,
			&item.HasCompactionHistory,
			&item.ViaWebsocket,
			&item.UpstreamErrorKind,
			&createdAtRaw,
		); err != nil {
			return nil, 0, page, pageSize, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, 0, page, pageSize, err
		}
		item.CreatedAt = createdAt
		item.populateBillingBreakdown()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, page, pageSize, err
	}
	if items == nil {
		items = []APIKeySelfUsageLog{}
	}
	return items, total, page, pageSize, nil
}
