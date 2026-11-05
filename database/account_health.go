package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AccountHealthBucket is the request success/failure count of one account within one time window.
type AccountHealthBucket struct {
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

// GetAccountsHealthBuckets returns, for each account, the request success/failure counts of its most recent blockCount time buckets (oldest first),
// each bucket spanning bucketDuration, covering [now-blockCount*dur, now] overall. It is used for the "health status" bar
// on the account admin page.
//
// Status codes 2xx count as success and the rest as failure; 499 (client cancelled) is ignored, keeping the same rule as the chart
// aggregation (getChartAggregation). PostgreSQL does the bucketing inside the database; SQLite
// keeps the row-by-row bucketing compatible implementation.
func (db *DB) GetAccountsHealthBuckets(ctx context.Context, now time.Time, blockCount int, bucketDuration time.Duration) (map[int64][]AccountHealthBucket, error) {
	return db.GetAccountsHealthBucketsByIDs(ctx, nil, now, blockCount, bucketDuration)
}

// GetAccountsHealthBucketsByIDs restricts health aggregation to a visible
// account page. An empty ids slice preserves the historical all-account query.
func (db *DB) GetAccountsHealthBucketsByIDs(ctx context.Context, ids []int64, now time.Time, blockCount int, bucketDuration time.Duration) (map[int64][]AccountHealthBucket, error) {
	if blockCount < 1 {
		blockCount = 20
	}
	if bucketDuration <= 0 {
		bucketDuration = 10 * time.Minute
	}

	windowStart := now.Add(-time.Duration(blockCount) * bucketDuration)
	ids = positiveUniqueIDs(ids)
	if !db.isSQLite() {
		return db.getPostgresAccountHealthBuckets(ctx, ids, windowStart, now, blockCount, bucketDuration)
	}
	startArg, endArg := db.timeRangeArgs(windowStart, now)

	query := `
		SELECT account_id, created_at, status_code
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		  AND status_code <> 499
		  AND account_id > 0
		  AND ` + db.endUserUsageLogPredicate() + `
	`
	args := []interface{}{startArg, endArg}
	if len(ids) > 0 {
		placeholders := make([]string, 0, len(ids))
		for _, id := range ids {
			args = append(args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		query += ` AND account_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]AccountHealthBucket)

	for rows.Next() {
		var accountID int64
		var createdRaw interface{}
		var statusCode int
		if err := rows.Scan(&accountID, &createdRaw, &statusCode); err != nil {
			return nil, err
		}
		if accountID <= 0 {
			continue
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		idx := int(createdAt.Sub(windowStart) / bucketDuration)
		if idx < 0 {
			idx = 0
		}
		if idx >= blockCount {
			idx = blockCount - 1
		}

		buckets, ok := result[accountID]
		if !ok {
			buckets = make([]AccountHealthBucket, blockCount)
			result[accountID] = buckets
		}
		if statusCode >= 200 && statusCode < 300 {
			buckets[idx].Success++
		} else {
			buckets[idx].Failed++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// getPostgresAccountHealthBuckets performs the time bucketing and aggregation
// inside PostgreSQL, so the application receives at most accounts*buckets rows
// rather than every request log in the health window.
func (db *DB) getPostgresAccountHealthBuckets(ctx context.Context, ids []int64, windowStart, now time.Time, blockCount int, bucketDuration time.Duration) (map[int64][]AccountHealthBucket, error) {
	args := []interface{}{windowStart, now, blockCount, bucketDuration.Seconds()}
	idFilter := ""
	if len(ids) > 0 {
		placeholders := make([]string, 0, len(ids))
		for _, id := range ids {
			args = append(args, id)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		idFilter = ` AND account_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	query := `
		SELECT account_id, bucket_index,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS failed
		FROM (
			SELECT account_id, status_code,
				GREATEST(0, LEAST($3 - 1,
					FLOOR((EXTRACT(EPOCH FROM created_at) - EXTRACT(EPOCH FROM $1::timestamptz)) / $4)::integer
				)) AS bucket_index
			FROM usage_logs
			WHERE created_at >= $1 AND created_at <= $2
			  AND status_code <> 499 AND account_id > 0
			  AND ` + db.endUserUsageLogPredicate() + idFilter + `
		) AS scoped_logs
		GROUP BY account_id, bucket_index
		ORDER BY account_id, bucket_index`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][]AccountHealthBucket)
	for rows.Next() {
		var accountID int64
		var bucketIndex, success, failed int
		if err := rows.Scan(&accountID, &bucketIndex, &success, &failed); err != nil {
			return nil, err
		}
		if bucketIndex < 0 || bucketIndex >= blockCount {
			continue
		}
		buckets := result[accountID]
		if buckets == nil {
			buckets = make([]AccountHealthBucket, blockCount)
			result[accountID] = buckets
		}
		buckets[bucketIndex] = AccountHealthBucket{Success: success, Failed: failed}
	}
	return result, rows.Err()
}
