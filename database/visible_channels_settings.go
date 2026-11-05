package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Which upstream channels the dashboard and account management show is stored in system_settings.visible_channels_config.
// Like invite_guide_config, it deliberately reads and writes with its own small UPDATE, outside the SaveSettings
// giant UPSERT: that statement's placeholders already run up to $119, so every added column would shift them all.

// AllUpstreamChannels is every channel the admin visibility switches can control; the order is the display order.
var AllUpstreamChannels = []string{"codex"}

// FallbackVisibleChannel is the fallback channel: it always stays visible however it is configured, so the admin panel cannot be turned into a blank page.
const FallbackVisibleChannel = "codex"

// VisibleChannelsConfig records the visible channels the user has ticked.
//
// A nil Channels means "never configured" and is treated as showing all; an explicit empty list leaves only the fallback channel.
// Do not add omitempty here: otherwise saving an empty list would be serialized as '{}', and reading it back would turn it into showing all.
type VisibleChannelsConfig struct {
	Channels []string `json:"channels"`
}

// Effective resolves the list of visible channels that actually take effect.
func (c VisibleChannelsConfig) Effective() []string {
	return NormalizeVisibleChannels(c.Channels)
}

// NormalizeVisibleChannels reduces any input to a valid list: nil counts as all; unknown channels and duplicates are removed;
// the fallback channel is always in the list; the output follows the fixed order of AllUpstreamChannels.
func NormalizeVisibleChannels(channels []string) []string {
	if channels == nil {
		return append([]string(nil), AllUpstreamChannels...)
	}
	wanted := map[string]bool{FallbackVisibleChannel: true}
	for _, raw := range channels {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		wanted[name] = true
	}
	out := make([]string, 0, len(AllUpstreamChannels))
	for _, name := range AllUpstreamChannels {
		if wanted[name] {
			out = append(out, name)
		}
	}
	return out
}

// LoadVisibleChannelsConfig reads the config. Not configured, an empty string, or corrupt JSON all fall back to "show all" —
// visibility is only a display preference, and one bad row should not make the admin panel lose channels.
func (db *DB) LoadVisibleChannelsConfig(ctx context.Context) (VisibleChannelsConfig, error) {
	var cfg VisibleChannelsConfig
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(visible_channels_config, '{}')
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
		return VisibleChannelsConfig{}, nil
	}
	return cfg, nil
}

// SaveVisibleChannelsConfig persists the config, normalizing it before the write so the fallback channel is always in the list.
func (db *DB) SaveVisibleChannelsConfig(ctx context.Context, cfg VisibleChannelsConfig) error {
	payload, err := json.Marshal(VisibleChannelsConfig{Channels: NormalizeVisibleChannels(cfg.Channels)})
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
			SET visible_channels_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
