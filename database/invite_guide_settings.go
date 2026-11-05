package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// The post-import invite guide config is stored in system_settings.invite_guide_config. It is read and written with a separate small UPDATE on purpose, not through the giant UPSERT in SaveSettings: that statement's placeholders already run up to $119, and every added column would shift them all.

// InviteGuideDefaultEnabled is the default when the user has never made a choice: show the guide by default.
const InviteGuideDefaultEnabled = true

// InviteGuideConfig controls whether the invite-credits guide pops up after accounts are imported.
//
// Enabled uses a pointer to tell "never chosen" apart from "explicitly turned off". The column default is '{}',
// a bare bool would read "never configured" as false and turn the default off, which is the opposite of what is intended.
type InviteGuideConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled resolves the effective value of the switch.
func (c InviteGuideConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return InviteGuideDefaultEnabled
	}
	return *c.Enabled
}

// LoadInviteGuideConfig reads the config. A missing, empty or corrupt JSON value falls back to the defaults—
// the guide popup is a nice-to-have feature, and one bad row must not stop the settings page from opening.
func (db *DB) LoadInviteGuideConfig(ctx context.Context) (InviteGuideConfig, error) {
	var cfg InviteGuideConfig
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(invite_guide_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return InviteGuideConfig{}, nil
	}
	return cfg, nil
}

// SaveInviteGuideConfig persists the config.
func (db *DB) SaveInviteGuideConfig(ctx context.Context, cfg InviteGuideConfig) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			UPDATE system_settings
			SET invite_guide_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
