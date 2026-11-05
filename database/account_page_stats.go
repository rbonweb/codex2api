package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// accountRequestCountBreakdownMaxIDs is the ID cap for the error-code and successful-model breakdowns.
// It matches the single-page cap of the admin page: beyond that it is a full-pool refresh, and those two extra GROUP BYs should not run.
const accountRequestCountBreakdownMaxIDs = 500

// appendAccountIDFilter builds the account_id match clause and appends the bound values to args.
// PostgreSQL uses a single array parameter `= ANY($n)`: if a full-pool stats refresh over tens of thousands of accounts expanded one placeholder per account,
// parse and plan overhead would grow linearly with the pool size, and it would hit the hard limit of 65535 parameters in the extended protocol
// (beyond that the query errors outright and the stats stay stuck in "refreshing" forever); SQLite keeps one placeholder per ID.
func (db *DB) appendAccountIDFilter(args *[]interface{}, ids []int64) string {
	if db.isSQLite() {
		placeholders := make([]string, 0, len(ids))
		for _, id := range ids {
			*args = append(*args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(*args)))
		}
		return "account_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	*args = append(*args, postgresInt8Array(ids))
	return fmt.Sprintf("account_id = ANY($%d)", len(*args))
}

// GetAccountRequestCountsByIDs returns the same seven-day counters as
// GetAccountRequestCounts, but restricts the scan to the visible account page.
// Client cancellations (499) remain in raw logs but are not account failures.
func (db *DB) GetAccountRequestCountsByIDs(ctx context.Context, ids []int64) (map[int64]*AccountRequestCount, error) {
	return db.getAccountRequestCountsByIDs(ctx, ids, true)
}

// GetAccountRequestCountTotalsByIDs aggregates only success / failure / 429 counts, without the error-code and
// successful-model breakdowns. The full-pool batched refresh takes this path: the breakdowns are only for the current page's capsules, and each batch would scan
// usage_logs two more times for no benefit.
func (db *DB) GetAccountRequestCountTotalsByIDs(ctx context.Context, ids []int64) (map[int64]*AccountRequestCount, error) {
	return db.getAccountRequestCountsByIDs(ctx, ids, false)
}

func (db *DB) getAccountRequestCountsByIDs(ctx context.Context, ids []int64, withBreakdown bool) (map[int64]*AccountRequestCount, error) {
	result := make(map[int64]*AccountRequestCount, len(ids))
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return result, nil
	}
	retryFalse := "COALESCE(is_retry_attempt, false) = false"
	retryTrue := "COALESCE(is_retry_attempt, false) = true"
	if db.isSQLite() {
		retryFalse = "COALESCE(is_retry_attempt, 0) = 0"
		retryTrue = "COALESCE(is_retry_attempt, 0) = 1"
	}
	args := make([]interface{}, 0, 2)
	args = append(args, db.timeArg(time.Now().AddDate(0, 0, -7)))
	idFilter := db.appendAccountIDFilter(&args, ids)
	query := fmt.Sprintf(`
		SELECT account_id,
			COALESCE(SUM(CASE WHEN status_code < 400 AND %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code <> 499 AND %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code <> 499 AND %s THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND %s AND %s
		GROUP BY account_id`, retryFalse, retryFalse, retryTrue, db.endUserUsageLogPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		value := &AccountRequestCount{}
		if err := rows.Scan(&value.AccountID, &value.SuccessCount, &value.ErrorCount, &value.RetryErrorCount, &value.RateLimitAttemptCount); err != nil {
			return nil, err
		}
		result[value.AccountID] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The error-code and successful-model breakdowns are only for the current page's capsules. If a full-pool batched refresh also ran these two
	// GROUP BYs, every batch would scan usage_logs two more times.
	if withBreakdown && len(ids) <= accountRequestCountBreakdownMaxIDs {
		if err := db.attachErrorStatusCounts(ctx, result, ids); err != nil {
			return nil, err
		}
		if err := db.attachSuccessModelCounts(ctx, result, ids); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (db *DB) attachErrorStatusCounts(ctx context.Context, result map[int64]*AccountRequestCount, ids []int64) error {
	if len(result) == 0 {
		return nil
	}
	args := []interface{}{db.timeArg(time.Now().AddDate(0, 0, -7))}
	idFilter := ""
	if ids != nil {
		ids = positiveUniqueIDs(ids)
		if len(ids) == 0 {
			return nil
		}
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	query := fmt.Sprintf(`
		SELECT account_id, status_code, COUNT(*)
		FROM usage_logs
		WHERE created_at >= $1 AND status_code >= 400 AND status_code <> 499 AND %s AND %s%s
		GROUP BY account_id, status_code`, db.nonRetryUsageLogPredicate(), db.endUserUsageLogPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, count int64
		var statusCode int
		if err := rows.Scan(&accountID, &statusCode, &count); err != nil {
			return err
		}
		rc := result[accountID]
		if rc == nil {
			continue
		}
		if rc.ErrorStatusCounts == nil {
			rc.ErrorStatusCounts = make(map[int]int64)
		}
		rc.ErrorStatusCounts[statusCode] = count
	}
	return rows.Err()
}

func (db *DB) attachSuccessModelCounts(ctx context.Context, result map[int64]*AccountRequestCount, ids []int64) error {
	if len(result) == 0 {
		return nil
	}
	args := []interface{}{db.timeArg(time.Now().AddDate(0, 0, -7))}
	idFilter := ""
	if ids != nil {
		ids = positiveUniqueIDs(ids)
		if len(ids) == 0 {
			return nil
		}
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	query := fmt.Sprintf(`
		SELECT account_id,
			COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
			COUNT(*)
		FROM usage_logs
		WHERE created_at >= $1 AND status_code < 400 AND %s AND %s%s
		GROUP BY account_id, COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown')`,
		db.nonRetryUsageLogPredicate(), db.endUserUsageLogPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, count int64
		var model string
		if err := rows.Scan(&accountID, &model, &count); err != nil {
			return err
		}
		rc := result[accountID]
		if rc == nil {
			continue
		}
		if rc.SuccessModelCounts == nil {
			rc.SuccessModelCounts = make(map[string]int64)
		}
		rc.SuccessModelCounts[model] = count
	}
	return rows.Err()
}

// GetAccountUsageWindowsByIDs computes the list-page usage fields without a
// global seven-day GROUP BY.
func (db *DB) GetAccountUsageWindowsByIDs(ctx context.Context, ids []int64, shortSince, longSince time.Time) (map[int64]*AccountTimeRangeUsage, map[int64]*AccountTimeRangeUsage, error) {
	shortWindow := make(map[int64]*AccountTimeRangeUsage, len(ids))
	longWindow := make(map[int64]*AccountTimeRangeUsage, len(ids))
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return shortWindow, longWindow, nil
	}
	if shortSince.Before(longSince) {
		shortSince, longSince = longSince, shortSince
	}
	args := []interface{}{db.timeArg(shortSince), db.timeArg(longSince)}
	idFilter := db.appendAccountIDFilter(&args, ids)
	query := fmt.Sprintf(`SELECT account_id,
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN total_tokens ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN account_billed ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN user_billed ELSE 0 END), 0),
		COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE created_at >= $2 AND status_code <> 499 AND %s AND %s AND %s AND %s
		GROUP BY account_id`, db.nonRetryUsageLogPredicate(), db.currentAccountUsageGenerationPredicate(), db.endUserUsageLogPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		shortUsage := &AccountTimeRangeUsage{}
		longUsage := &AccountTimeRangeUsage{}
		if err := rows.Scan(&shortUsage.AccountID, &shortUsage.Requests, &shortUsage.Tokens, &shortUsage.AccountBilled, &shortUsage.UserBilled,
			&longUsage.Requests, &longUsage.Tokens, &longUsage.AccountBilled, &longUsage.UserBilled); err != nil {
			return nil, nil, err
		}
		longUsage.AccountID = shortUsage.AccountID
		shortWindow[shortUsage.AccountID] = shortUsage
		longWindow[longUsage.AccountID] = longUsage
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return shortWindow, longWindow, nil
}

// GetAccountUsageSinceByIDs aggregates requests/tokens/billing for the given
// accounts since the provided instant. It powers the list-page "today" column
// with the same filtering semantics as the 5h/7d usage windows.
func (db *DB) GetAccountUsageSinceByIDs(ctx context.Context, ids []int64, since time.Time) (map[int64]*AccountTimeRangeUsage, error) {
	result := make(map[int64]*AccountTimeRangeUsage, len(ids))
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return result, nil
	}
	args := []interface{}{db.timeArg(since)}
	idFilter := db.appendAccountIDFilter(&args, ids)
	query := fmt.Sprintf(`SELECT account_id,
		COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND status_code <> 499 AND %s AND %s AND %s
		GROUP BY account_id`, db.nonRetryUsageLogPredicate(), db.currentAccountUsageGenerationPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		usage := &AccountTimeRangeUsage{}
		if err := rows.Scan(&usage.AccountID, &usage.Requests, &usage.Tokens, &usage.AccountBilled, &usage.UserBilled); err != nil {
			return nil, err
		}
		result[usage.AccountID] = usage
	}
	return result, rows.Err()
}

// GetAccountModelCountsSinceByIDs splits, by model, the request count, success count and average time to first token of the specified accounts since the given time.
// The filter basis is the same as GetAccountUsageSinceByIDs, so the overlay totals match the today-statistics figures.
func (db *DB) GetAccountModelCountsSinceByIDs(ctx context.Context, ids []int64, since time.Time) (map[int64]map[string]AccountModelCount, error) {
	result := make(map[int64]map[string]AccountModelCount, len(ids))
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return result, nil
	}
	args := []interface{}{db.timeArg(since)}
	idFilter := db.appendAccountIDFilter(&args, ids)
	query := fmt.Sprintf(`SELECT account_id,
		COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
		COUNT(*),
		COALESCE(SUM(CASE WHEN status_code < 400 THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(NULLIF(first_token_ms, 0)), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND status_code <> 499 AND %s AND %s AND %s
		GROUP BY account_id, COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown')`,
		db.nonRetryUsageLogPredicate(), db.currentAccountUsageGenerationPredicate(), idFilter)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID int64
		var model string
		var count AccountModelCount
		if err := rows.Scan(&accountID, &model, &count.Requests, &count.Success, &count.AvgFirstTokenMs); err != nil {
			return nil, err
		}
		if result[accountID] == nil {
			result[accountID] = make(map[string]AccountModelCount)
		}
		result[accountID][model] = count
	}
	return result, rows.Err()
}

func positiveUniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}
