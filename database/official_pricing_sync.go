package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	DefaultOfficialPricingSyncIntervalMinutes = 1440
	MinOfficialPricingSyncIntervalMinutes     = 60
	MaxOfficialPricingSyncIntervalMinutes     = 10080
)

type OfficialPricingSyncConfig struct {
	Enabled         bool         `json:"enabled"`
	IntervalMinutes int          `json:"interval_minutes"`
	IncludeOpenAI   bool         `json:"include_openai"`
	LastAttemptAt   sql.NullTime `json:"-"`
	LastSuccessAt   sql.NullTime `json:"-"`
	LastError       string       `json:"last_error,omitempty"`
	LastWarning     string       `json:"last_warning,omitempty"`
}

var (
	officialPricingConfigInitMu sync.Mutex
	officialPricingConfigReady  = make(map[*DB]bool)
)

func NormalizeOfficialPricingSyncInterval(minutes int) int {
	if minutes < MinOfficialPricingSyncIntervalMinutes || minutes > MaxOfficialPricingSyncIntervalMinutes {
		return DefaultOfficialPricingSyncIntervalMinutes
	}
	return minutes
}

func (db *DB) ensureOfficialPricingSyncConfig(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database unavailable")
	}
	officialPricingConfigInitMu.Lock()
	defer officialPricingConfigInitMu.Unlock()
	if officialPricingConfigReady[db] {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS official_pricing_sync_config (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		enabled BOOLEAN NOT NULL DEFAULT FALSE,
		interval_minutes INTEGER NOT NULL DEFAULT 1440,
		include_openai BOOLEAN NOT NULL DEFAULT TRUE,
		last_attempt_at TIMESTAMP NULL,
		last_success_at TIMESTAMP NULL,
		last_error TEXT NOT NULL DEFAULT '',
		last_warning TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO official_pricing_sync_config (
		singleton_id, enabled, interval_minutes, include_openai
	) VALUES (1, FALSE, 1440, TRUE) ON CONFLICT (singleton_id) DO NOTHING`)
	if err == nil {
		officialPricingConfigReady[db] = true
	}
	return err
}

// GetOfficialPricingSyncConfig reads directly first. Only when the read fails (new database, missing column, missing seed row) does it create the table and seed it before reading again.
// The seed-row INSERT needs the write lock; on a slow-disk SQLite at startup, usage backfill holds the write lock for a long time,
// so running it on every startup would make this read-only query wait until it times out.
func (db *DB) GetOfficialPricingSyncConfig(ctx context.Context) (*OfficialPricingSyncConfig, error) {
	if db == nil || db.conn == nil {
		return nil, fmt.Errorf("database unavailable")
	}
	if cfg, err := db.readOfficialPricingSyncConfig(ctx); err == nil {
		// If a full row can be read, the table, columns and seed row all exist, so later writes do not need to run table creation.
		officialPricingConfigInitMu.Lock()
		officialPricingConfigReady[db] = true
		officialPricingConfigInitMu.Unlock()
		return cfg, nil
	}
	if err := db.ensureOfficialPricingSyncConfig(ctx); err != nil {
		return nil, err
	}
	return db.readOfficialPricingSyncConfig(ctx)
}

func (db *DB) readOfficialPricingSyncConfig(ctx context.Context) (*OfficialPricingSyncConfig, error) {
	var cfg OfficialPricingSyncConfig
	err := db.conn.QueryRowContext(ctx, `SELECT enabled, interval_minutes, include_openai,
		last_attempt_at, last_success_at, COALESCE(last_error, ''), COALESCE(last_warning, '')
		FROM official_pricing_sync_config WHERE singleton_id = 1`).Scan(
		&cfg.Enabled, &cfg.IntervalMinutes, &cfg.IncludeOpenAI,
		&cfg.LastAttemptAt, &cfg.LastSuccessAt, &cfg.LastError, &cfg.LastWarning,
	)
	if err != nil {
		return nil, err
	}
	cfg.IntervalMinutes = NormalizeOfficialPricingSyncInterval(cfg.IntervalMinutes)
	return &cfg, nil
}

func (db *DB) UpdateOfficialPricingSyncConfig(ctx context.Context, cfg OfficialPricingSyncConfig) (*OfficialPricingSyncConfig, error) {
	if err := db.ensureOfficialPricingSyncConfig(ctx); err != nil {
		return nil, err
	}
	cfg.IntervalMinutes = NormalizeOfficialPricingSyncInterval(cfg.IntervalMinutes)
	if !cfg.IncludeOpenAI {
		return nil, fmt.Errorf("select at least one official pricing source")
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE official_pricing_sync_config
		SET enabled = $1, interval_minutes = $2, include_openai = $3
		WHERE singleton_id = 1`, cfg.Enabled, cfg.IntervalMinutes, cfg.IncludeOpenAI)
	if err != nil {
		return nil, err
	}
	return db.GetOfficialPricingSyncConfig(ctx)
}

func (db *DB) RecordOfficialPricingSyncResult(ctx context.Context, attemptedAt time.Time, syncErr error, warnings []string) error {
	if err := db.ensureOfficialPricingSyncConfig(ctx); err != nil {
		return err
	}
	attemptedAt = attemptedAt.UTC()
	lastError := ""
	succeeded := syncErr == nil
	if syncErr != nil {
		lastError = strings.TrimSpace(syncErr.Error())
		if len(lastError) > 1000 {
			lastError = lastError[:1000]
		}
	}
	lastWarning := strings.TrimSpace(strings.Join(warnings, "; "))
	if len(lastWarning) > 2000 {
		lastWarning = lastWarning[:2000]
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE official_pricing_sync_config SET
		last_attempt_at = $1,
		last_success_at = CASE WHEN $2 THEN $1 ELSE last_success_at END,
		last_error = $3,
		last_warning = $4
		WHERE singleton_id = 1`, attemptedAt, succeeded, lastError, lastWarning)
	return err
}
