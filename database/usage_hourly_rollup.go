package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

// usage_log_hourly pre-aggregates usage_logs by hour (issue #778). On a slow disk SQLite, the 30-day self-service usage page
// and the usage pages for long ranges have to read every detail row in the range; the rollup table collapses requests with the same hour and the same group of dimensions into one row,
// Long-range stats now read the hourly rollup for whole hours; only the leading and trailing remainders shorter than an hour still scan the details.
//
// The dimensions cover all groups and filter columns used by the self-service page, the admin range cards and the breakdown rankings; 499 and internal requests are also written to the table,
// and the queries exclude them according to their own rules. The rollup is an exact mirror of the details: when logs are batch-written, it accumulates by the ids of the just-inserted rows in the same transaction
// and clearing the logs clears it too. Existing history from before an upgrade is backfilled in the background in id chunks; until the backfill finishes, queries keep scanning the details.
// Inserts that bypass the write queue (manual SQL, old-version instances still running during a rolling upgrade) are covered by two checks: before a query, compare
// MAX(id), and if it finds new rows that are not in the rollup, switch to scanning the details and notify the background repair; the background job also periodically checks the row counts of the most recent whole hours.
const (
	// Bump the version number when the aggregation rules change, aggregate columns are added, or a data migration rewrites columns used by the rollup in usage_logs:
	// On startup, add the missing aggregate columns, clear the rollup and backfill again under the new rules. If the dimensions (the primary key) change, the table name must change too.
	usageHourlyRollupVersion = 1

	usageHourlyMaintainIDChunk       = 500
	usageHourlyBackfillChunkSQLite   = 2000
	usageHourlyBackfillChunkPostgres = 20000
	// Release the write lock between chunks, so that on a slow disk log writes are not queued for long during backfill.
	usageHourlyBackfillPause      = 50 * time.Millisecond
	usageHourlyBackfillRetryDelay = time.Minute
	usageHourlyVerifyInterval     = 20 * time.Minute
	usageHourlyVerifyHours        = 3
	// If a repair touches more hours than this value, rebuild the whole table (background chunked backfill) instead of recalculating hour by hour in one transaction.
	usageHourlyRepairMaxHours = 72
)

const usageHourlyKeyColumns = `bucket, api_key_id, api_key_name, api_key_masked, channel, model, effective_model,
	inbound_endpoint, endpoint, status_code, stream, internal`

// usageHourlyAggregates holds the rollup columns and their matching detail aggregate expressions. duration_ms_sum serves two definitions at once:
// admin AVG(duration_ms) = duration_ms_sum / duration_rows, self-service page AVG(NULLIF(duration_ms, 0))
// = duration_ms_sum / duration_samples (durations and first-token times are never negative, so > 0 and <> 0 are equivalent).
var usageHourlyAggregates = []struct{ column, expr string }{
	{"requests", "COUNT(*)"},
	{"total_tokens", "COALESCE(SUM(u.total_tokens), 0)"},
	{"prompt_tokens", "COALESCE(SUM(u.prompt_tokens), 0)"},
	{"completion_tokens", "COALESCE(SUM(u.completion_tokens), 0)"},
	{"input_tokens", "COALESCE(SUM(u.input_tokens), 0)"},
	{"output_tokens", "COALESCE(SUM(u.output_tokens), 0)"},
	{"cached_tokens", "COALESCE(SUM(u.cached_tokens), 0)"},
	{"account_billed", "COALESCE(SUM(u.account_billed), 0)"},
	{"user_billed", "COALESCE(SUM(u.user_billed), 0)"},
	{"duration_ms_sum", "COALESCE(SUM(u.duration_ms), 0)"},
	{"duration_rows", "COUNT(u.duration_ms)"},
	{"duration_samples", "COALESCE(SUM(CASE WHEN u.duration_ms > 0 THEN 1 ELSE 0 END), 0)"},
	{"first_token_ms_sum", "COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN u.first_token_ms ELSE 0 END), 0)"},
	{"first_token_samples", "COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN 1 ELSE 0 END), 0)"},
	{"cache_hit_requests", "COALESCE(SUM(CASE WHEN u.cached_tokens > 0 THEN 1 ELSE 0 END), 0)"},
	{"fast_requests", "COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(u.billing_service_tier, ''), u.service_tier, '')) IN ('fast', 'priority', 'ultrafast') THEN 1 ELSE 0 END), 0)"},
	{"reasoning_requests", "COALESCE(SUM(CASE WHEN u.reasoning_tokens > 0 OR NULLIF(u.reasoning_effort, '') IS NOT NULL THEN 1 ELSE 0 END), 0)"},
	{"image_requests", "COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(u.inbound_endpoint, ''), u.endpoint, '')) LIKE '%/images/%' OR LOWER(COALESCE(u.model, '')) LIKE 'gpt-image-%' OR u.image_count > 0 THEN 1 ELSE 0 END), 0)"},
	// attempt_index is 1-based, so only > 1 counts as a retried request (consistent with the admin breakdown definition).
	{"retry_requests", "COALESCE(SUM(CASE WHEN u.attempt_index > 1 THEN 1 ELSE 0 END), 0)"},
	// The following only count error rows (same rule as the request log "errors only" filter), for the error summary and error filter counts.
	{"error_rows", "COALESCE(SUM(CASE WHEN " + usageLogErrorPredicate + " THEN 1 ELSE 0 END), 0)"},
	{"error_duration_ms_sum", "COALESCE(SUM(CASE WHEN " + usageLogErrorPredicate + " THEN u.duration_ms ELSE 0 END), 0)"},
	{"error_duration_rows", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND u.duration_ms IS NOT NULL THEN 1 ELSE 0 END), 0)"},
	{"error_timeouts", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND (" + usageLogTimeoutPredicate + ") THEN 1 ELSE 0 END), 0)"},
	{"error_retry_attempts", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND COALESCE(u.is_retry_attempt, false) THEN 1 ELSE 0 END), 0)"},
}

const (
	usageLogErrorPredicate   = `u.status_code >= 400 OR COALESCE(u.error_message, '') <> '' OR COALESCE(u.upstream_error_kind, '') <> ''`
	usageLogTimeoutPredicate = `LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%deadline%'`
)

func (db *DB) ensureUsageHourlySchema(ctx context.Context) error {
	intType, floatType, boolType, timeType := "BIGINT", "DOUBLE PRECISION", "BOOLEAN NOT NULL DEFAULT FALSE", "TIMESTAMPTZ"
	if db.isSQLite() {
		intType, floatType, boolType, timeType = "INTEGER", "REAL", "INTEGER NOT NULL DEFAULT 0", "TIMESTAMP"
	}
	aggregateColumnDef := func(column string) string {
		if column == "account_billed" || column == "user_billed" {
			return floatType + " NOT NULL DEFAULT 0"
		}
		return intType + " NOT NULL DEFAULT 0"
	}
	var columns strings.Builder
	for _, agg := range usageHourlyAggregates {
		fmt.Fprintf(&columns, "\t\t%s %s,\n", agg.column, aggregateColumnDef(agg.column))
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_log_hourly (
		bucket ` + timeType + ` NOT NULL,
		api_key_id ` + intType + ` NOT NULL DEFAULT 0,
		api_key_name TEXT NOT NULL DEFAULT '',
		api_key_masked TEXT NOT NULL DEFAULT '',
		channel TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		effective_model TEXT NOT NULL DEFAULT '',
		inbound_endpoint TEXT NOT NULL DEFAULT '',
		endpoint TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		stream ` + boolType + `,
		internal INTEGER NOT NULL DEFAULT 0,
` + columns.String() + `		min_created_at ` + timeType + `,
		PRIMARY KEY (` + usageHourlyKeyColumns + `)
	)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_log_hourly_api_key_bucket ON usage_log_hourly (api_key_id, bucket)`,
		`CREATE TABLE IF NOT EXISTS usage_log_hourly_state (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL DEFAULT 0,
		ready INTEGER NOT NULL DEFAULT 0,
		backfill_to_id BIGINT NOT NULL DEFAULT 0,
		backfill_cursor BIGINT NOT NULL DEFAULT 0,
		last_log_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// Aggregate columns added in later versions go here, together with a version bump to backfill again.
	if db.isSQLite() {
		existing, err := db.sqliteTableColumns(ctx, "usage_log_hourly")
		if err != nil {
			return err
		}
		for _, agg := range usageHourlyAggregates {
			if _, ok := existing[agg.column]; ok {
				continue
			}
			if _, err := db.conn.ExecContext(ctx, "ALTER TABLE usage_log_hourly ADD COLUMN "+agg.column+" "+aggregateColumnDef(agg.column)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, agg := range usageHourlyAggregates {
		if _, err := db.conn.ExecContext(ctx, "ALTER TABLE usage_log_hourly ADD COLUMN IF NOT EXISTS "+agg.column+" "+aggregateColumnDef(agg.column)); err != nil {
			return err
		}
	}
	return nil
}

// initUsageHourlyRollup sets the backfill boundary before the log write goroutine starts: if the version does not match (including the first upgrade), the rollup is cleared,
// and the current MAX(id) is recorded as the backfill upper bound; new logs from then on are maintained by the write transaction. The return value says whether existing rows still need backfilling.
func (db *DB) initUsageHourlyRollup(ctx context.Context) (bool, error) {
	needsBackfill := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if !db.isSQLite() {
			// Wait for in-flight write transactions to commit, so there is no gap between MAX(id) and the ids handled by the write transactions.
			if _, err := tx.ExecContext(ctx, `LOCK TABLE usage_log_hourly IN EXCLUSIVE MODE`); err != nil {
				return err
			}
		}
		var version, ready int
		err := tx.QueryRowContext(ctx, `SELECT version, ready FROM usage_log_hourly_state WHERE id = 1`).Scan(&version, &ready)
		if err == nil && version == usageHourlyRollupVersion {
			needsBackfill = ready != 1
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		pending, err := db.resetUsageHourlyRollupTx(ctx, tx)
		needsBackfill = pending
		return err
	})
	return needsBackfill, err
}

// resetUsageHourlyRollupTx clears the rollup and registers a backfill from the start. The caller must already hold the write lock on the rollup table.
func (db *DB) resetUsageHourlyRollupTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly`); err != nil {
		return false, err
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs`).Scan(&maxID); err != nil {
		return false, err
	}
	ready := 0
	if maxID == 0 {
		ready = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_log_hourly_state (id, version, ready, backfill_to_id, backfill_cursor, last_log_id, updated_at)
		VALUES (1, $1, $2, $3, 0, $3, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET version = excluded.version, ready = excluded.ready,
			backfill_to_id = excluded.backfill_to_id, backfill_cursor = 0,
			last_log_id = excluded.last_log_id, updated_at = CURRENT_TIMESTAMP`,
		usageHourlyRollupVersion, ready, maxID)
	return ready == 0, err
}

// clearUsageHourlyRollupTx clears the rollup along with the logs; with no existing rows to backfill, it is ready right away.
func (db *DB) clearUsageHourlyRollupTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET version = $1, ready = 1,
		backfill_to_id = 0, backfill_cursor = 0, last_log_id = 0, updated_at = CURRENT_TIMESTAMP WHERE id = 1`,
		usageHourlyRollupVersion)
	return err
}

func (db *DB) usageHourlyBucketExpr(column string) string {
	if db.isSQLite() {
		return "strftime('%Y-%m-%d %H:00:00', " + column + ")"
	}
	// Independent of the session time zone: truncates to UTC whole hours, matching Go-side Truncate(time.Hour).
	return "(date_trunc('hour', " + column + " AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')"
}

// usageHourlyUpsertSQL aggregates the detail rows matching where by hour and dimensions, then adds them into the rollup table.
// Incremental maintenance, existing-row backfill and hourly recalculation share this one aggregation definition, so the rules stay consistent.
// ORDER BY makes concurrent transactions lock the rollup rows in the same order, avoiding PostgreSQL deadlocks.
func (db *DB) usageHourlyUpsertSQL(where string) string {
	groupBy := "1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12"
	columns := []string{usageHourlyKeyColumns}
	selects := []string{
		db.usageHourlyBucketExpr("u.created_at"),
		"COALESCE(u.api_key_id, 0)",
		"COALESCE(u.api_key_name, '')",
		"COALESCE(u.api_key_masked, '')",
		"COALESCE(u.channel, '')",
		"COALESCE(u.model, '')",
		"COALESCE(u.effective_model, '')",
		"COALESCE(u.inbound_endpoint, '')",
		"COALESCE(u.endpoint, '')",
		"COALESCE(u.status_code, 0)",
		"COALESCE(u.stream, false)",
		"CASE WHEN TRIM(COALESCE(u.internal_reason, '')) <> '' THEN 1 ELSE 0 END",
	}
	updates := make([]string, 0, len(usageHourlyAggregates)+1)
	for _, agg := range usageHourlyAggregates {
		columns = append(columns, agg.column)
		selects = append(selects, agg.expr)
		updates = append(updates, fmt.Sprintf("%[1]s = usage_log_hourly.%[1]s + excluded.%[1]s", agg.column))
	}
	columns = append(columns, "min_created_at")
	selects = append(selects, "MIN(u.created_at)")
	if db.isSQLite() {
		updates = append(updates, `min_created_at = MIN(COALESCE(usage_log_hourly.min_created_at, excluded.min_created_at),
			COALESCE(excluded.min_created_at, usage_log_hourly.min_created_at))`)
	} else {
		updates = append(updates, "min_created_at = LEAST(usage_log_hourly.min_created_at, excluded.min_created_at)")
	}
	return `INSERT INTO usage_log_hourly (` + strings.Join(columns, ", ") + `)
		SELECT ` + strings.Join(selects, ",\n\t\t\t") + `
		FROM usage_logs u
		WHERE ` + where + `
		GROUP BY ` + groupBy + `
		ORDER BY ` + groupBy + `
		ON CONFLICT (` + usageHourlyKeyColumns + `) DO UPDATE SET ` + strings.Join(updates, ",\n\t\t\t")
}

func (db *DB) usageHourlyMaxExpr(a, b string) string {
	if db.isSQLite() {
		return "MAX(" + a + ", " + b + ")"
	}
	return "GREATEST(" + a + ", " + b + ")"
}

// applyUsageHourlyRollupWithExec adds the just-inserted rows into the rollup, inside the log write transaction.
// Only ids after the backfill upper bound count: earlier rows are handled by the backfill, so the two paths never count the same row twice.
func (db *DB) applyUsageHourlyRollupWithExec(ctx context.Context, execer sqlExecer, ids []int64) error {
	if execer == nil || len(ids) == 0 {
		return nil
	}
	maxID := slices.Max(ids)
	for start := 0; start < len(ids); start += usageHourlyMaintainIDChunk {
		chunk := ids[start:min(start+usageHourlyMaintainIDChunk, len(ids))]
		placeholders := dbPlaceholders(false, 1, len(chunk))
		where := "u.id IN (" + strings.Join(placeholders, ", ") + `)
			AND u.id > COALESCE((SELECT backfill_to_id FROM usage_log_hourly_state WHERE id = 1), 0)`
		if _, err := execer.ExecContext(ctx, db.usageHourlyUpsertSQL(where), argsFromInt64s(chunk)...); err != nil {
			return err
		}
	}
	// Use the max id of this batch itself, not the table's MAX(id): the latter would also "claim" rows that slipped in at the same time and are not in the rollup.
	_, err := execer.ExecContext(ctx, `UPDATE usage_log_hourly_state SET last_log_id = `+
		db.usageHourlyMaxExpr("last_log_id", "$1")+` WHERE id = 1`, maxID)
	return err
}

// lockUsageHourlyForRebuildTx blocks concurrent incremental accumulation before a recalculation. On PostgreSQL it locks the table and waits for in-flight writes to commit;
// on SQLite it first performs a write to take the write lock (under WAL, a deferred transaction that reads before writing can fail to upgrade).
func (db *DB) lockUsageHourlyForRebuildTx(ctx context.Context, tx *sql.Tx) error {
	if db.isSQLite() {
		_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET updated_at = CURRENT_TIMESTAMP WHERE id = 1`)
		return err
	}
	_, err := tx.ExecContext(ctx, `LOCK TABLE usage_log_hourly IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// rebuildUsageHourlyHoursTx recalculates the rollup for the given whole hours from the details.
func (db *DB) rebuildUsageHourlyHoursTx(ctx context.Context, tx *sql.Tx, hours []time.Time) error {
	for _, hour := range hours {
		hour = hour.UTC().Truncate(time.Hour)
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly WHERE bucket = $1`, db.timeArg(hour)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, db.usageHourlyUpsertSQL(`u.created_at >= $1 AND u.created_at < $2`),
			db.timeArg(hour), db.timeArg(hour.Add(time.Hour))); err != nil {
			return err
		}
	}
	return nil
}

// RebuildUsageHourlyRollup synchronously rebuilds the whole rollup table from the details. It is for tests and diagnostics that rewrite usage_logs directly;
// normal operation does not need to call it.
func (db *DB) RebuildUsageHourlyRollup(ctx context.Context) error {
	if _, err := db.initUsageHourlyRollupReset(ctx); err != nil {
		return err
	}
	for {
		done, err := db.usageHourlyBackfillStep(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (db *DB) initUsageHourlyRollupReset(ctx context.Context) (bool, error) {
	pending := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		var err error
		pending, err = db.resetUsageHourlyRollupTx(ctx, tx)
		return err
	})
	return pending, err
}

var errUsageHourlyBackfillRaced = errors.New("usage hourly backfill cursor moved concurrently")

// usageHourlyBackfillStep backfills the next chunk of existing ids. It accumulates the rollup first, then advances with a cursor compare-and-swap,
// with the same lock order as the log write transaction (rollup rows first, state row second); if another instance advances first, the whole chunk is rolled back and retried.
func (db *DB) usageHourlyBackfillStep(ctx context.Context) (bool, error) {
	chunk := int64(usageHourlyBackfillChunkPostgres)
	if db.isSQLite() {
		chunk = usageHourlyBackfillChunkSQLite
	}
	done := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if db.isSQLite() {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET updated_at = CURRENT_TIMESTAMP WHERE id = 1`); err != nil {
				return err
			}
		}
		var version, ready int
		var to, cursor int64
		if err := tx.QueryRowContext(ctx, `SELECT version, ready, backfill_to_id, backfill_cursor
			FROM usage_log_hourly_state WHERE id = 1`).Scan(&version, &ready, &to, &cursor); err != nil {
			return err
		}
		if version != usageHourlyRollupVersion || ready == 1 {
			done = true
			return nil
		}
		hi := min(cursor+chunk, to)
		if hi > cursor {
			if _, err := tx.ExecContext(ctx, db.usageHourlyUpsertSQL(`u.id > $1 AND u.id <= $2`), cursor, hi); err != nil {
				return err
			}
		}
		nextReady := 0
		if hi >= to {
			nextReady = 1
		}
		res, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET backfill_cursor = $1, ready = $2, updated_at = CURRENT_TIMESTAMP
			WHERE id = 1 AND version = $3 AND backfill_cursor = $4 AND ready = 0`, hi, nextReady, usageHourlyRollupVersion, cursor)
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			return errUsageHourlyBackfillRaced
		}
		done = nextReady == 1
		return nil
	})
	if errors.Is(err, errUsageHourlyBackfillRaced) {
		return false, nil
	}
	return done, err
}

func (db *DB) runUsageHourlyBackfill(ctx context.Context) {
	started := time.Now()
	chunks := 0
	for {
		done, err := db.usageHourlyBackfillStep(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("backfilling the usage hourly rollup failed, retrying after %s: %v", usageHourlyBackfillRetryDelay, err)
			if !sleepContext(ctx, usageHourlyBackfillRetryDelay) {
				return
			}
			continue
		}
		chunks++
		if done {
			log.Printf("usage hourly rollup backfill complete: %d chunks, took %s", chunks, time.Since(started).Round(time.Millisecond))
			return
		}
		if !sleepContext(ctx, usageHourlyBackfillPause) {
			return
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// startUsageHourlyMaintainer runs backfill, on-demand repair and scheduled checks. It is attached to the same lifecycle group as the log write goroutine,
// Close cancels it immediately when logStop closes, so it does not take up the graceful-shutdown window of background tasks.
func (db *DB) startUsageHourlyMaintainer(backfill bool) {
	db.logWg.Add(1)
	go func() {
		defer db.logWg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-db.logStop:
				cancel()
			case <-ctx.Done():
			}
		}()
		if backfill {
			db.runUsageHourlyBackfill(ctx)
		}
		ticker := time.NewTicker(usageHourlyVerifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-db.usageHourlyRepairNotify:
				db.repairUsageHourly(ctx)
			case <-ticker.C:
				db.verifyRecentUsageHourly(ctx)
			}
		}
	}()
}

func (db *DB) requestUsageHourlyRepair() {
	select {
	case db.usageHourlyRepairNotify <- struct{}{}:
	default:
	}
}

// usageHourlyRollupUsable decides whether queries can read the rollup: backfill has finished, and the rollup covers the newest rows in the details.
// The state and MAX(id) are read in the same statement; on PostgreSQL that is one snapshot.
func (db *DB) usageHourlyRollupUsable(ctx context.Context) bool {
	var version, ready int
	var lastLogID, maxID int64
	err := db.conn.QueryRowContext(ctx, `SELECT s.version, s.ready, s.last_log_id,
		(SELECT COALESCE(MAX(id), 0) FROM usage_logs)
		FROM usage_log_hourly_state s WHERE s.id = 1`).Scan(&version, &ready, &lastLogID, &maxID)
	if err != nil || version != usageHourlyRollupVersion || ready != 1 {
		return false
	}
	if maxID != lastLogID {
		db.requestUsageHourlyRepair()
		return false
	}
	return true
}

// repairUsageHourly fills in rows inserted outside the write queue: it recalculates the hours those rows fall in. If MAX(id) is lower than the recorded
// value, rows were deleted; the rollup cannot be reconciled, so the whole table is rebuilt.
func (db *DB) repairUsageHourly(ctx context.Context) {
	reset := false
	repaired := 0
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		var ready int
		var lastLogID, maxID int64
		if err := tx.QueryRowContext(ctx, `SELECT s.ready, s.last_log_id, (SELECT COALESCE(MAX(id), 0) FROM usage_logs)
			FROM usage_log_hourly_state s WHERE s.id = 1`).Scan(&ready, &lastLogID, &maxID); err != nil {
			return err
		}
		if ready != 1 || maxID == lastLogID {
			return nil
		}
		if maxID > lastLogID {
			hours, err := db.usageHourlyBucketsAfterID(ctx, tx, lastLogID)
			if err != nil {
				return err
			}
			if len(hours) <= usageHourlyRepairMaxHours {
				if err := db.rebuildUsageHourlyHoursTx(ctx, tx, hours); err != nil {
					return err
				}
				repaired = len(hours)
				_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET last_log_id = $1, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, maxID)
				return err
			}
		}
		reset = true
		_, err := db.resetUsageHourlyRollupTx(ctx, tx)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("repairing the usage hourly rollup failed: %v", err)
		}
		return
	}
	if reset {
		log.Printf("usage hourly rollup does not match the details, cleared and backfilling again in the background")
		db.runUsageHourlyBackfill(ctx)
	} else if repaired > 0 {
		log.Printf("usage hourly rollup filled in details inserted outside the write queue: recalculated %d hours", repaired)
	}
}

func (db *DB) usageHourlyBucketsAfterID(ctx context.Context, tx *sql.Tx, afterID int64) ([]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT `+db.usageHourlyBucketExpr("u.created_at")+`
		FROM usage_logs u WHERE u.id > $1`, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hours []time.Time
	for rows.Next() {
		var raw interface{}
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		hour, err := parseDBTimeValue(raw)
		if err != nil {
			return nil, err
		}
		if !hour.IsZero() {
			hours = append(hours, hour)
		}
	}
	return hours, rows.Err()
}

// verifyRecentUsageHourly checks the row counts of the most recent completed hours and recalculates any that do not match. It covers what the MAX(id) check before a query cannot catch:
// rows written by old-version instances whose ids fall between ids the new instance has already registered.
func (db *DB) verifyRecentUsageHourly(ctx context.Context) {
	var ready int
	if err := db.conn.QueryRowContext(ctx, `SELECT ready FROM usage_log_hourly_state WHERE id = 1`).Scan(&ready); err != nil || ready != 1 {
		return
	}
	current := time.Now().UTC().Truncate(time.Hour)
	var stale []time.Time
	for i := 1; i <= usageHourlyVerifyHours; i++ {
		hour := current.Add(-time.Duration(i) * time.Hour)
		var logged, rolled int64
		if err := db.conn.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM usage_logs WHERE created_at >= $1 AND created_at < $2),
			(SELECT CAST(COALESCE(SUM(requests), 0) AS BIGINT) FROM usage_log_hourly WHERE bucket = $1)`,
			db.timeArg(hour), db.timeArg(hour.Add(time.Hour))).Scan(&logged, &rolled); err != nil {
			return
		}
		if logged != rolled {
			stale = append(stale, hour)
		}
	}
	if len(stale) == 0 {
		return
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		return db.rebuildUsageHourlyHoursTx(ctx, tx, stale)
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("verifying the usage hourly rollup failed: %v", err)
		}
		return
	}
	log.Printf("usage hourly rollup check found %d hours whose row counts differ from the details, recalculated from the details", len(stale))
}

// usageTimeSpan is a half-open interval [start, end); a zero start means no lower bound, and a zero end means no upper bound.
type usageTimeSpan struct {
	start, end time.Time
}

// usageHourlyPlan describes how a range query is split: the rollup table covers the whole hours in [from, to) except skip,
// and the ranges in raw are scanned from the details table. When useRollup is false, the whole range scans the details.
type usageHourlyPlan struct {
	useRollup bool
	from, to  time.Time // zero from means no lower bound
	skip      []time.Time
	raw       []usageTimeSpan
}

// planUsageHourly splits [start, end) into whole hours and remainders. cuts are the query's internal segment boundaries (window start,
// "one minute ago" and so on): a boundary that falls inside an hour hands that whole hour to the details, so every row in the rollup is certain to
// belong to exactly one segment. A zero end means the trailing remainder has no upper bound from the current hour on.
func planUsageHourly(start, end, now time.Time, cuts ...time.Time) usageHourlyPlan {
	upper := end
	if upper.IsZero() {
		upper = now
	}
	to := upper.UTC().Truncate(time.Hour)
	var from time.Time
	if !start.IsZero() {
		from = start.UTC().Truncate(time.Hour)
		if from.Before(start) {
			from = from.Add(time.Hour)
		}
	}
	if !from.IsZero() && !from.Before(to) {
		return usageHourlyPlan{raw: []usageTimeSpan{{start, end}}}
	}
	plan := usageHourlyPlan{useRollup: true, from: from, to: to}
	if !start.IsZero() && start.Before(from) {
		plan.raw = append(plan.raw, usageTimeSpan{start, from})
	}
	if end.IsZero() || to.Before(end) {
		plan.raw = append(plan.raw, usageTimeSpan{to, end})
	}
	for _, cut := range cuts {
		if cut.IsZero() {
			continue
		}
		hour := cut.UTC().Truncate(time.Hour)
		if hour.Equal(cut) || (!from.IsZero() && hour.Before(from)) || !hour.Before(to) {
			continue
		}
		if slices.ContainsFunc(plan.skip, hour.Equal) {
			continue
		}
		plan.skip = append(plan.skip, hour)
		plan.raw = append(plan.raw, usageTimeSpan{hour, hour.Add(time.Hour)})
	}
	plan.raw = mergeUsageTimeSpans(plan.raw)
	return plan
}

func mergeUsageTimeSpans(spans []usageTimeSpan) []usageTimeSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start.IsZero() != spans[j].start.IsZero() {
			return spans[i].start.IsZero()
		}
		return spans[i].start.Before(spans[j].start)
	})
	merged := []usageTimeSpan{spans[0]}
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if last.end.IsZero() {
			continue
		}
		if !span.start.After(last.end) {
			if span.end.IsZero() || span.end.After(last.end) {
				last.end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// usageSpanWhere builds the condition that column falls within span; parameter numbering continues after args.
func (db *DB) usageSpanWhere(column string, span usageTimeSpan, args []interface{}) (string, []interface{}) {
	parts := make([]string, 0, 2)
	if !span.start.IsZero() {
		args = append(args, db.timeArg(span.start))
		parts = append(parts, fmt.Sprintf("%s >= $%d", column, len(args)))
	}
	if !span.end.IsZero() {
		args = append(args, db.timeArg(span.end))
		parts = append(parts, fmt.Sprintf("%s < $%d", column, len(args)))
	}
	if len(parts) == 0 {
		return "1 = 1", args
	}
	return strings.Join(parts, " AND "), args
}

// usageHourlyBucketWhere builds the hour condition the rollup table covers under the plan.
func (db *DB) usageHourlyBucketWhere(column string, plan usageHourlyPlan, args []interface{}) (string, []interface{}) {
	where, args := db.usageSpanWhere(column, usageTimeSpan{plan.from, plan.to}, args)
	if len(plan.skip) > 0 {
		placeholders := make([]string, len(plan.skip))
		for i, hour := range plan.skip {
			args = append(args, db.timeArg(hour))
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		where += " AND " + column + " NOT IN (" + strings.Join(placeholders, ", ") + ")"
	}
	return where, args
}

// planUsageHourlyQuery returns the split plan when the rollup is usable, otherwise the whole range scans the details.
func (db *DB) planUsageHourlyQuery(ctx context.Context, start, end, now time.Time, cuts ...time.Time) usageHourlyPlan {
	plan := planUsageHourly(start, end, now, cuts...)
	if plan.useRollup && !db.usageHourlyRollupUsable(ctx) {
		return usageHourlyPlan{raw: []usageTimeSpan{{start, end}}}
	}
	return plan
}

// sumBigint wraps a CAST: PostgreSQL sums BIGINT into NUMERIC, and scanning that directly into int64 is unreliable.
func sumBigint(expr string) string {
	return "CAST(COALESCE(SUM(" + expr + "), 0) AS BIGINT)"
}
