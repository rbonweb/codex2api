package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Prompt review log retention policy.
//
// Three "log-type" tables grow without limit with traffic: prompt_filter_logs (local filter / review logs),
// prompt_risk_events (risk events), prompt_risk_event_sources (deduplicated event source table).
// The retention policy purges expired rows by age in days, but **rows linked to a still-existing upstream CY record (prompt_policy_incidents)
// are never purged** — they are the evidence chain of the CY. When an admin deletes a CY record, the linked review logs go with it
// through cascade deletion; risk profiles are kept and then expire naturally after the retention days.
//
// Deletes are always done in batches (5000 rows per batch by default) and release the write lock between batches, to avoid one large DELETE locking
// SQLite for a long time; the purge loop continues until there are no more rows to delete or the context ends.

const (
	DefaultPromptLogRetentionDays = 7
	MaxPromptLogRetentionDays     = 365
	DefaultPromptLogPurgeBatch    = 5000
)

type PromptLogRetentionConfig struct {
	RetentionDays      int          `json:"retention_days"`
	LastRunAt          sql.NullTime `json:"-"`
	LastDeletedLogs    int64        `json:"last_deleted_logs"`
	LastDeletedEvents  int64        `json:"last_deleted_events"`
	LastDeletedSources int64        `json:"last_deleted_sources"`
	LastDurationMs     int64        `json:"last_duration_ms"`
	LastError          string       `json:"last_error,omitempty"`
}

// PromptLogPurgeResult is the statistics of one purge. Interrupted means it stopped early because the context ended,
// and the remaining expired rows are left for the next round.
type PromptLogPurgeResult struct {
	Logs        int64 `json:"logs"`
	Events      int64 `json:"events"`
	Sources     int64 `json:"sources"`
	Batches     int   `json:"batches"`
	Interrupted bool  `json:"interrupted"`
}

// PromptLogPurgeFilter limits the purge scope of prompt_filter_logs (used by the manual clear button);
// the zero value means purge all sources by Cutoff.
type PromptLogPurgeFilter struct {
	Reviewed *bool
	Source   string
}

var (
	promptRetentionConfigInitMu sync.Mutex
	promptRetentionConfigReady  = make(map[*DB]bool)
)

func NormalizePromptLogRetentionDays(days int) int {
	if days < 0 {
		return 0
	}
	if days > MaxPromptLogRetentionDays {
		return MaxPromptLogRetentionDays
	}
	return days
}

func (db *DB) ensurePromptLogRetentionConfig(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database unavailable")
	}
	promptRetentionConfigInitMu.Lock()
	defer promptRetentionConfigInitMu.Unlock()
	if promptRetentionConfigReady[db] {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_log_retention_config (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		retention_days INTEGER NOT NULL DEFAULT 7,
		last_run_at TIMESTAMP NULL,
		last_deleted_logs BIGINT NOT NULL DEFAULT 0,
		last_deleted_events BIGINT NOT NULL DEFAULT 0,
		last_deleted_sources BIGINT NOT NULL DEFAULT 0,
		last_duration_ms BIGINT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_log_retention_config (singleton_id, retention_days)
		VALUES (1, $1) ON CONFLICT (singleton_id) DO NOTHING`, DefaultPromptLogRetentionDays)
	if err == nil {
		promptRetentionConfigReady[db] = true
	}
	return err
}

func (db *DB) GetPromptLogRetentionConfig(ctx context.Context) (*PromptLogRetentionConfig, error) {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return nil, err
	}
	var cfg PromptLogRetentionConfig
	err := db.conn.QueryRowContext(ctx, `SELECT retention_days, last_run_at, last_deleted_logs, last_deleted_events,
		last_deleted_sources, last_duration_ms, COALESCE(last_error, '')
		FROM prompt_log_retention_config WHERE singleton_id = 1`).Scan(
		&cfg.RetentionDays, &cfg.LastRunAt, &cfg.LastDeletedLogs, &cfg.LastDeletedEvents,
		&cfg.LastDeletedSources, &cfg.LastDurationMs, &cfg.LastError,
	)
	if err != nil {
		return nil, err
	}
	cfg.RetentionDays = NormalizePromptLogRetentionDays(cfg.RetentionDays)
	return &cfg, nil
}

func (db *DB) UpdatePromptLogRetentionDays(ctx context.Context, days int) (*PromptLogRetentionConfig, error) {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return nil, err
	}
	days = NormalizePromptLogRetentionDays(days)
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_log_retention_config SET retention_days = $1 WHERE singleton_id = 1`, days); err != nil {
		return nil, err
	}
	return db.GetPromptLogRetentionConfig(ctx)
}

func (db *DB) RecordPromptLogRetentionRun(ctx context.Context, ranAt time.Time, result PromptLogPurgeResult, duration time.Duration, runErr error) error {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return err
	}
	lastError := ""
	if runErr != nil {
		lastError = strings.TrimSpace(runErr.Error())
		if len(lastError) > 1000 {
			lastError = lastError[:1000]
		}
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE prompt_log_retention_config SET
		last_run_at = $1, last_deleted_logs = $2, last_deleted_events = $3, last_deleted_sources = $4,
		last_duration_ms = $5, last_error = $6
		WHERE singleton_id = 1`,
		db.timeArg(ranAt.UTC()), result.Logs, result.Events, result.Sources, duration.Milliseconds(), lastError)
	return err
}

// ==================== Cleanup ====================

// promptLogProtectedByIncidentSQL is the condition "this log row is protected by a CY record" (l is the prompt_filter_logs alias):
// it shares request_correlation_id with a still-existing CY (the CY and the local review log are linked by the same request's
// correlation id); it does not reverse-look-up prompt_risk_events, since that table has no prompt_filter_log_id index.
const promptLogProtectedByIncidentSQL = `(l.request_correlation_id <> '' AND EXISTS (
	SELECT 1 FROM prompt_policy_incidents i WHERE i.request_correlation_id = l.request_correlation_id))`

// promptEventProtectedSQL is the condition "this risk event is protected" (e is the prompt_risk_events alias):
// it is attached to a still-existing CY, or its source log still exists (the log itself is protected or has not yet expired).
const promptEventProtectedSQL = `(
	(e.incident_id <> '' AND EXISTS (SELECT 1 FROM prompt_policy_incidents i WHERE i.incident_id = e.incident_id))
	OR (e.prompt_filter_log_id > 0 AND EXISTS (SELECT 1 FROM prompt_filter_logs l WHERE l.id = e.prompt_filter_log_id)))`

// PurgeExpiredPromptLogs purges, in batches by cutoff, the expired rows in the three log tables that are not protected by a CY.
// Order: logs → risk events → event sources, so that the protection check for each later table sees the final state of the earlier one.
func (db *DB) PurgeExpiredPromptLogs(ctx context.Context, cutoff time.Time, batchSize int, pause time.Duration) (PromptLogPurgeResult, error) {
	return db.purgePromptLogs(ctx, cutoff, PromptLogPurgeFilter{}, batchSize, pause, true)
}

// PurgePromptFilterLogs only purges prompt_filter_logs (the manual clear button):
// it also skips CY-linked rows; risk events and source records are left alone (consistent with the existing semantics that "risk profiles are kept").
func (db *DB) PurgePromptFilterLogs(ctx context.Context, cutoff time.Time, filter PromptLogPurgeFilter, batchSize int, pause time.Duration) (PromptLogPurgeResult, error) {
	return db.purgePromptLogs(ctx, cutoff, filter, batchSize, pause, false)
}

func (db *DB) purgePromptLogs(ctx context.Context, cutoff time.Time, filter PromptLogPurgeFilter, batchSize int, pause time.Duration, includeEvents bool) (PromptLogPurgeResult, error) {
	var result PromptLogPurgeResult
	if db == nil || db.conn == nil {
		return result, fmt.Errorf("database unavailable")
	}
	if batchSize <= 0 {
		batchSize = DefaultPromptLogPurgeBatch
	}
	// The three tables are created on demand; ensure they exist before purging, to avoid errors on deployments that never produced a CY or risk event.
	if err := db.ensurePromptPolicyIncidentsTable(ctx); err != nil {
		return result, err
	}
	if err := db.ensurePromptRiskEventsTable(ctx); err != nil {
		return result, err
	}
	cutoffArg := db.timeArg(cutoff.UTC())

	logWhere := `l.created_at < $1 AND NOT ` + promptLogProtectedByIncidentSQL
	logArgs := []interface{}{cutoffArg}
	if filter.Reviewed != nil {
		logArgs = append(logArgs, *filter.Reviewed)
		logWhere += fmt.Sprintf(` AND l.reviewed = $%d`, len(logArgs))
	}
	if source := strings.TrimSpace(filter.Source); source != "" {
		logArgs = append(logArgs, source)
		logWhere += fmt.Sprintf(` AND l.source = $%d`, len(logArgs))
	}
	logStmt := fmt.Sprintf(`DELETE FROM prompt_filter_logs WHERE id IN (
		SELECT l.id FROM prompt_filter_logs l WHERE %s LIMIT $%d)`, logWhere, len(logArgs)+1)
	logArgs = append(logArgs, batchSize)

	deleted, err := db.purgeInBatches(ctx, logStmt, logArgs, batchSize, pause, &result)
	result.Logs = deleted
	if err != nil {
		return result, err
	}

	if !includeEvents {
		// Manually clearing logs only touches prompt_filter_logs: risk profiles (events / sources) are kept per the existing semantics,
		// and are purged by age only by the retention policy.
		return result, nil
	}

	eventWhere := `e.created_at < $1 AND NOT ` + promptEventProtectedSQL
	eventArgs := []interface{}{cutoffArg}
	eventStmt := fmt.Sprintf(`DELETE FROM prompt_risk_events WHERE id IN (
		SELECT e.id FROM prompt_risk_events e WHERE %s LIMIT $%d)`, eventWhere, len(eventArgs)+1)
	eventArgs = append(eventArgs, batchSize)
	deleted, err = db.purgeInBatches(ctx, eventStmt, eventArgs, batchSize, pause, &result)
	result.Events = deleted
	if err != nil {
		return result, err
	}

	deleted, err = db.purgeOrphanPromptRiskSources(ctx, cutoffArg, batchSize, pause, &result)
	result.Sources = deleted
	return result, err
}

// purgeOrphanPromptRiskSources purges expired source records that no risk event references any more.
func (db *DB) purgeOrphanPromptRiskSources(ctx context.Context, cutoffArg interface{}, batchSize int, pause time.Duration, result *PromptLogPurgeResult) (int64, error) {
	stmt := `DELETE FROM prompt_risk_event_sources WHERE (source_type, source_id) IN (
		SELECT s.source_type, s.source_id FROM prompt_risk_event_sources s
		WHERE s.processed_at < $1 AND NOT EXISTS (
			SELECT 1 FROM prompt_risk_events e WHERE e.source_type = s.source_type AND e.source_id = s.source_id)
		LIMIT $2)`
	return db.purgeInBatches(ctx, stmt, []interface{}{cutoffArg, batchSize}, batchSize, pause, result)
}

// purgeInBatches repeatedly runs the DELETE statement with LIMIT until a batch is not full or there are no rows left to delete;
// each batch holds the SQLite write lock once, and pauses between batches to yield to normal requests.
func (db *DB) purgeInBatches(ctx context.Context, stmt string, args []interface{}, batchSize int, pause time.Duration, result *PromptLogPurgeResult) (int64, error) {
	var total int64
	for {
		if ctx.Err() != nil {
			result.Interrupted = true
			return total, nil
		}
		var affected int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			res, execErr := db.conn.ExecContext(ctx, stmt, args...)
			if execErr != nil {
				return execErr
			}
			affected, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				result.Interrupted = true
				return total, nil
			}
			return total, err
		}
		result.Batches++
		total += affected
		if affected < int64(batchSize) {
			return total, nil
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				result.Interrupted = true
				return total, nil
			case <-time.After(pause):
			}
		}
	}
}

// ==================== CY cascade ====================

// deletePromptIncidentEvidenceTx clears the review logs linked to a CY record before deleting it
// (sharing request_correlation_id, and no other CY still references the same correlation id).
// Risk profiles (prompt_risk_events / source records) are outside the cascade scope: they are account / user-level
// history, kept after the CY is deleted per the existing semantics, and purged by age by the retention policy once the CY and logs are gone.
// The caller must delete the CY itself afterwards in the same transaction.
func deletePromptIncidentEvidenceTx(ctx context.Context, tx *sql.Tx, incidentID string) error {
	var correlationID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(request_correlation_id, '') FROM prompt_policy_incidents WHERE incident_id=$1`, incidentID).Scan(&correlationID); err != nil {
		return err
	}
	if correlationID == "" {
		return nil
	}
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents WHERE request_correlation_id=$1 AND incident_id<>$2`, correlationID, incidentID).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM prompt_filter_logs WHERE request_correlation_id=$1`, correlationID)
	return err
}

// deleteAllPromptIncidentEvidenceTx is the cascade of "clear CY": it deletes all review logs that share
// a correlation id with any CY; risk profiles are kept as well.
func deleteAllPromptIncidentEvidenceTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM prompt_filter_logs WHERE request_correlation_id <> '' AND EXISTS (
		SELECT 1 FROM prompt_policy_incidents i WHERE i.request_correlation_id = prompt_filter_logs.request_correlation_id)`)
	return err
}
