package database

import (
	"context"
	"time"
)

// UpdateUsageSnapshot5h persists the 5h usage snapshot (used when there is no 7d data)
func (db *DB) UpdateUsageSnapshot5h(ctx context.Context, id int64, pct5h float64, reset5hAt time.Time, updatedAt time.Time) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_5h_used_percent":     pct5h,
		"codex_5h_reset_at":         reset5hAt.Format(time.RFC3339),
		"codex_5h_usage_updated_at": updatedAt.Format(time.RFC3339),
	})
}

// ClearUsageSnapshot5h clears the 5h window fields in credentials (called when upstream returns no 5h data, issue #382).
// Writes null: GetCredential and the load path treat null as missing, so 5h is not hydrated again after a restart.
func (db *DB) ClearUsageSnapshot5h(ctx context.Context, id int64) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_5h_used_percent":     nil,
		"codex_5h_reset_at":         nil,
		"codex_5h_usage_updated_at": nil,
	})
}

// UpdateUsageSnapshotSpark persists the separate spark 5h usage snapshot.
func (db *DB) UpdateUsageSnapshotSpark(ctx context.Context, id int64, pct float64, resetAt time.Time, updatedAt time.Time) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_spark_used_percent":     pct,
		"codex_spark_reset_at":         resetAt.Format(time.RFC3339),
		"codex_spark_usage_updated_at": updatedAt.Format(time.RFC3339),
	})
}

// ClearUsageSnapshotSpark clears the spark window fields in credentials.
func (db *DB) ClearUsageSnapshotSpark(ctx context.Context, id int64) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_spark_used_percent":     nil,
		"codex_spark_reset_at":         nil,
		"codex_spark_usage_updated_at": nil,
	})
}

// ClearCooldownIfReason clears only the cooldown that still has the expected
// source. A concurrent 401 or generic 429 therefore cannot be erased by a
// delayed premium-5h absence observation.
func (db *DB) ClearCooldownIfReason(ctx context.Context, id int64, reason string) (bool, error) {
	var cleared bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `
			UPDATE accounts
			SET cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 AND cooldown_reason = $2
		`, id, reason)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		cleared = affected > 0
		return nil
	})
	return cleared, err
}

// ClearCooldownIfReasonAndUntil clears a matching cooldown only when both its
// source and reset time are unchanged. A newer cooldown with the same reason
// therefore survives a delayed usage probe even if its reset is shorter.
func (db *DB) ClearCooldownIfReasonAndUntil(ctx context.Context, id int64, reason string, until time.Time) (bool, error) {
	var cleared bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `
			UPDATE accounts
			SET cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 AND cooldown_reason = $2 AND cooldown_until = $3
		`, id, reason, until)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		cleared = affected > 0
		return nil
	})
	return cleared, err
}
