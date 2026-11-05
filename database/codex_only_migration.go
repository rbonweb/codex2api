package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// dataMigrationCodexOnlyV1 removes everything that belonged to the providers dropped when the gateway became Codex-only.
// Safety rule: only accounts whose credentials.upstream_type is explicitly one of the removed providers are deleted.
// An empty, missing or unknown upstream_type is a Codex account (legacy rows have no upstream_type) and is never touched.
const dataMigrationCodexOnlyV1 = "20261105_codex_only_v1"

// codexOnlyRemovedUpstreamTypes are the only upstream types the migration may delete (compared case-insensitively after trimming).
var codexOnlyRemovedUpstreamTypes = []string{"claude", "grok", "xai", "antigravity", "openai_responses"}

// codexOnlyRemovedChannels are the api_keys.limits.upstream_channel and account_groups.channel values that are reset to codex.
// It is the removed upstream types plus the relay spellings the old UI or API could store.
var codexOnlyRemovedChannels = []string{"claude", "grok", "xai", "antigravity", "openai_responses", "openai-responses", "relay"}

// codexOnlyGrokTables are the Grok-only tables dropped by the migration.
var codexOnlyGrokTables = []string{
	"grok_account_fact_snapshots",
	"grok_credential_identity_claims",
	"grok_model_capabilities",
	"grok_model_catalog_items",
	"grok_model_catalog_snapshots",
	"grok_state_migration_progress",
}

// codexOnlyAccountDependents are the tables keyed by an account id.
// SQLite does not enforce foreign keys here and several tables never had one, so they are cleared explicitly, and only for removed accounts.
// usage_logs, account_events and account_daily_usage are history and are deliberately kept.
var codexOnlyAccountDependents = []struct{ table, column string }{
	{"account_group_members", "account_id"},
	{"account_model_cooldowns", "account_id"},
	{"maintenance_jobs", "entity_id"},
	{"model_capability_snapshots", "account_id"},
	{"daybreak_snapshots", "account_id"},
	{"channel_monitor_checks", "account_id"},
	{"channel_monitor_configs", "account_id"},
	{"intelligence_check_runs", "account_id"},
	{"intelligence_check_accounts", "account_id"},
}

func codexOnlySQLList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ",")
}

// codexOnlyUpstreamTypeExpr is the normalised upstream type of an accounts row.
func codexOnlyUpstreamTypeExpr(sqlite bool) string {
	if sqlite {
		return `LOWER(TRIM(COALESCE(json_extract(credentials, '$.upstream_type'), '')))`
	}
	return `LOWER(BTRIM(COALESCE(credentials->>'upstream_type', '')))`
}

// codexOnlyStatements builds the SQL the migration issues for one driver, so a test can check the PostgreSQL text without a server.
type codexOnlyStatements struct {
	countByType    string
	removedPred    string
	dependents     []string
	dependentTable []string
	deleteAccounts string
	resetKeys      string
	resetGroups    string
	dropTables     []string
}

func buildCodexOnlyStatements(sqlite bool, existingTables map[string]bool) codexOnlyStatements {
	upstream := codexOnlyUpstreamTypeExpr(sqlite)
	pred := upstream + ` IN (` + codexOnlySQLList(codexOnlyRemovedUpstreamTypes) + `)`
	st := codexOnlyStatements{
		removedPred:    pred,
		countByType:    `SELECT ` + upstream + `, COUNT(*) FROM accounts WHERE ` + pred + ` GROUP BY 1 ORDER BY 1`,
		deleteAccounts: `DELETE FROM accounts WHERE ` + pred,
	}
	for _, dep := range codexOnlyAccountDependents {
		if existingTables != nil && !existingTables[dep.table] {
			continue
		}
		st.dependentTable = append(st.dependentTable, dep.table)
		st.dependents = append(st.dependents, `DELETE FROM `+dep.table+` WHERE `+dep.column+` IN (SELECT id FROM accounts WHERE `+pred+`)`)
	}
	channels := codexOnlySQLList(codexOnlyRemovedChannels)
	if sqlite {
		channelExpr := `CASE WHEN json_valid(limits) THEN LOWER(TRIM(COALESCE(json_extract(limits, '$.upstream_channel'), ''))) ELSE '' END`
		st.resetKeys = `UPDATE api_keys SET limits = json_set(limits, '$.upstream_channel', 'codex') WHERE ` + channelExpr + ` IN (` + channels + `)`
	} else {
		st.resetKeys = `UPDATE api_keys SET limits = jsonb_set(limits, '{upstream_channel}', '"codex"'::jsonb, false) WHERE jsonb_typeof(limits) = 'object' AND LOWER(BTRIM(COALESCE(limits->>'upstream_channel', ''))) IN (` + channels + `)`
	}
	st.resetGroups = `UPDATE account_groups SET channel = 'codex' WHERE LOWER(TRIM(COALESCE(channel, ''))) IN (` + channels + `)`
	for _, table := range codexOnlyGrokTables {
		st.dropTables = append(st.dropTables, `DROP TABLE IF EXISTS `+table)
	}
	return st
}

func (db *DB) codexOnlyTableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	query := `SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1`
	if db.isSQLite() {
		query = `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = $1`
	}
	var one int
	err := tx.QueryRowContext(ctx, query, table).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// migrateCodexOnlyV1 runs inside the keyed data-migration transaction: any error rolls everything back, including the key, so a failed run is retried on the next start.
func (db *DB) migrateCodexOnlyV1(ctx context.Context, tx *sql.Tx) error {
	existing := make(map[string]bool)
	for _, table := range append(append([]string{"api_keys", "account_groups", "maintenance_jobs"}, codexOnlyGrokTables...), dependentTableNames()...) {
		ok, err := db.codexOnlyTableExists(ctx, tx, table)
		if err != nil {
			return fmt.Errorf("check table %s: %w", table, err)
		}
		existing[table] = ok
	}
	st := buildCodexOnlyStatements(db.isSQLite(), existing)

	// Log per-type counts before deleting anything.
	rows, err := tx.QueryContext(ctx, st.countByType)
	if err != nil {
		return fmt.Errorf("count removed-provider accounts: %w", err)
	}
	counts := make(map[string]int64)
	for rows.Next() {
		var upstream string
		var n int64
		if err := rows.Scan(&upstream, &n); err != nil {
			rows.Close()
			return fmt.Errorf("count removed-provider accounts: %w", err)
		}
		counts[upstream] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("count removed-provider accounts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, upstream := range codexOnlyRemovedUpstreamTypes {
		if n := counts[upstream]; n > 0 {
			log.Printf("[data_migration] %s: deleting %d %s account(s)", dataMigrationCodexOnlyV1, n, upstream)
		}
	}

	var removed int64
	for _, n := range counts {
		removed += n
	}
	if removed > 0 {
		for i, stmt := range st.dependents {
			res, err := tx.ExecContext(ctx, stmt)
			if err != nil {
				return fmt.Errorf("clear %s of removed accounts: %w", st.dependentTable[i], err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				log.Printf("[data_migration] %s: cleared %d row(s) from %s", dataMigrationCodexOnlyV1, n, st.dependentTable[i])
			}
		}
		res, err := tx.ExecContext(ctx, st.deleteAccounts)
		if err != nil {
			return fmt.Errorf("delete removed-provider accounts: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n != removed {
			return fmt.Errorf("deleted %d removed-provider accounts but counted %d", n, removed)
		}
	}

	if existing["maintenance_jobs"] {
		if _, err := tx.ExecContext(ctx, `DELETE FROM maintenance_jobs WHERE job_kind = 'grok_freshness'`); err != nil {
			return fmt.Errorf("clear grok maintenance jobs: %w", err)
		}
	}
	if existing["api_keys"] {
		res, err := tx.ExecContext(ctx, st.resetKeys)
		if err != nil {
			return fmt.Errorf("reset API key upstream channels: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			log.Printf("[data_migration] %s: reset the upstream channel of %d API key(s) to codex", dataMigrationCodexOnlyV1, n)
		}
	}
	if existing["account_groups"] {
		res, err := tx.ExecContext(ctx, st.resetGroups)
		if err != nil {
			return fmt.Errorf("reset account group channels: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			log.Printf("[data_migration] %s: reset the channel of %d account group(s) to codex", dataMigrationCodexOnlyV1, n)
		}
	}
	for i, stmt := range st.dropTables {
		if !existing[codexOnlyGrokTables[i]] {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("drop %s: %w", codexOnlyGrokTables[i], err)
		}
	}
	return nil
}

func dependentTableNames() []string {
	names := make([]string, 0, len(codexOnlyAccountDependents))
	for _, dep := range codexOnlyAccountDependents {
		names = append(names, dep.table)
	}
	return names
}
