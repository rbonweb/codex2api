package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/internal/openaiidentity"
)

const (
	dataMigrationOAuthIdentityDedupeV1 = "20260616_oauth_identity_dedupe_v1"
	// v2: identity aliases add user_id. Personal accounts (without a workspace account_id) previously were deduplicated only by credential text,
	// so duplicate accounts produced after AT rotation could not be cleaned up by v1; v2 adds email+user_id to the aliases
	// and reruns the merge once.
	dataMigrationOAuthIdentityDedupeV2 = "20260702_oauth_identity_dedupe_v2"
	// Backfill usage_logs.channel: derive the channel from accounts.platform (xai→grok, others→codex);
	// for history rows whose account has been deleted, the default is codex (all traffic before Grok launched was codex).
	dataMigrationUsageLogChannelV1   = "20260721_usage_log_channel_backfill_v1"
	dataMigrationWorkspaceIdentityV3 = "20260722_workspace_identity_v3"
	// Provider-only migrations (Grok group classification, Claude provider backfill)
	// are now stubs that only record their key, so old databases neither rerun nor fail on them.
	dataMigrationGroupChannelV1   = "20260807_account_group_channel_v1"
	dataMigrationClaudeProviderV1 = "20260829_claude_provider_backfill_v1"
	dataMigrationDaybreakUsageV1  = "20260926_daybreak_usage_program_v1"
	dataMigrationTimeout          = 5 * time.Minute
)

type oauthIdentityDedupeAccount struct {
	id          int64
	credentials map[string]interface{}
	enabled     bool
	locked      bool
	createdAt   time.Time
	updatedAt   time.Time
}

func (db *DB) runDataMigrations(ctx context.Context) error {
	if err := db.ensureDataMigrationsTable(ctx); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationOAuthIdentityDedupeV1, db.dedupeOAuthIdentityAccounts); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationOAuthIdentityDedupeV2, db.dedupeOAuthIdentityAccounts); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationUsageLogChannelV1, db.backfillUsageLogChannel); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationWorkspaceIdentityV3, db.migrateWorkspaceIdentityV3); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationGroupChannelV1, markDataMigrationDoneOnly); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationClaudeProviderV1, markDataMigrationDoneOnly); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationDaybreakUsageV1, db.backfillDaybreakUsageProgram); err != nil {
		return err
	}
	return db.runDataMigrationOnce(ctx, dataMigrationCodexOnlyV1, db.migrateCodexOnlyV1)
}

// markDataMigrationDoneOnly is the body of a retired provider-only migration: runDataMigrationOnce records the key and this does nothing else.
func markDataMigrationDoneOnly(context.Context, *sql.Tx) error { return nil }

// backfillUsageLogChannel backfills channel on existing usage_logs: first it marks grok by the platform of the existing accounts,
// then it sets all remaining empty values to codex (all historical traffic before the Grok channel launched went through Codex).
func (db *DB) backfillUsageLogChannel(ctx context.Context, tx *sql.Tx) error {
	if db.isSQLite() {
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs SET channel = 'grok'
			WHERE COALESCE(channel, '') = ''
			  AND account_id IN (SELECT id FROM accounts WHERE platform = 'xai')`); err != nil {
			return fmt.Errorf("backfill grok channel: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs SET channel = 'grok'
			FROM accounts a
			WHERE COALESCE(usage_logs.channel, '') = ''
			  AND usage_logs.account_id = a.id
			  AND a.platform = 'xai'`); err != nil {
			return fmt.Errorf("backfill grok channel: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_logs SET channel = 'codex'
		WHERE COALESCE(channel, '') = ''`); err != nil {
		return fmt.Errorf("backfill codex channel: %w", err)
	}
	return nil
}

func (db *DB) runDataMigrationsWithTimeout() error {
	ctx, cancel := context.WithTimeout(context.Background(), dataMigrationTimeout)
	defer cancel()
	return db.runDataMigrations(ctx)
}

func (db *DB) ensureDataMigrationsTable(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS data_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create data_migrations table: %w", err)
	}
	return nil
}

func (db *DB) runDataMigrationOnce(ctx context.Context, version string, migrate func(context.Context, *sql.Tx) error) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(ctx, `
			INSERT INTO data_migrations (version, applied_at)
			VALUES ($1, CURRENT_TIMESTAMP)
			ON CONFLICT(version) DO NOTHING
		`, version)
		if err != nil {
			return fmt.Errorf("failed to record data migration %s: %w", version, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return tx.Commit()
		}

		if err := migrate(ctx, tx); err != nil {
			return fmt.Errorf("failed to run data migration %s: %w", version, err)
		}
		return tx.Commit()
	})
}

func (db *DB) dedupeOAuthIdentityAccounts(ctx context.Context, tx *sql.Tx) error {
	accounts, err := db.listOAuthIdentityDedupeAccounts(ctx, tx)
	if err != nil {
		return err
	}

	parent := make([]int, len(accounts))
	eligible := make([]bool, len(accounts))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		rootA := find(a)
		rootB := find(b)
		if rootA != rootB {
			parent[rootB] = rootA
		}
	}

	aliasOwner := make(map[string]int)
	for i, account := range accounts {
		aliases := oauthIdentityDedupeAliases(account.credentials)
		if len(aliases) == 0 {
			continue
		}
		eligible[i] = true
		for _, alias := range aliases {
			if owner, ok := aliasOwner[alias]; ok {
				union(i, owner)
				continue
			}
			aliasOwner[alias] = i
		}
	}

	groups := make(map[int][]oauthIdentityDedupeAccount)
	for i, account := range accounts {
		if !eligible[i] {
			continue
		}
		groups[find(i)] = append(groups[find(i)], account)
	}

	var loserIDs []int64
	duplicateGroups := 0
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		duplicateGroups++
		sort.SliceStable(group, func(i, j int) bool {
			return oauthIdentityDedupeWinnerLess(group[i], group[j])
		})
		for _, loser := range group[1:] {
			loserIDs = append(loserIDs, loser.id)
		}
	}
	if len(loserIDs) == 0 {
		return nil
	}

	sort.Slice(loserIDs, func(i, j int) bool { return loserIDs[i] < loserIDs[j] })
	if err := softDeleteAccountsTx(ctx, tx, loserIDs); err != nil {
		return err
	}
	if err := insertAccountEventsTx(ctx, tx, loserIDs, "deleted", "oauth_identity_dedupe_v1"); err != nil {
		return err
	}
	log.Printf("[data_migration] %s: found %d groups of duplicate OAuth identities, soft-deleted %d duplicate accounts", dataMigrationOAuthIdentityDedupeV1, duplicateGroups, len(loserIDs))
	return nil
}

// migrateWorkspaceIdentityV3 backfills active OAuth accounts from JWTs and
// deduplicates only non-empty email + workspace_id pairs.
func (db *DB) migrateWorkspaceIdentityV3(ctx context.Context, tx *sql.Tx) error {
	accounts, err := db.listOAuthIdentityDedupeAccounts(ctx, tx)
	if err != nil {
		return err
	}

	groups := make(map[string][]oauthIdentityDedupeAccount)
	for i := range accounts {
		account := &accounts[i]
		if !workspaceIdentityMigrationEligible(account.credentials) {
			continue
		}
		email := strings.TrimSpace(credentialStringFromMap(account.credentials, "email"))
		if email != "" && strings.TrimSpace(credentialStringFromMap(account.credentials, "workspace_id")) == "" {
			accessToken := strings.TrimSpace(credentialStringFromMap(account.credentials, "access_token"))
			if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(account.credentials, "access_token_type")), "codex_at") ||
				strings.HasPrefix(accessToken, "at-") {
				accessToken = ""
			}
			tokenEmail, workspaceID := openaiidentity.TokenIdentity(
				credentialStringFromMap(account.credentials, "id_token"),
				accessToken,
			)
			if workspaceID != "" && strings.EqualFold(tokenEmail, email) {
				account.credentials["workspace_id"] = workspaceID
				encoded, err := json.Marshal(encryptSensitiveCredentials(account.credentials))
				if err != nil {
					return err
				}
				query := `UPDATE accounts SET credentials = $1 WHERE id = $2`
				if !db.isSQLite() {
					query = `UPDATE accounts SET credentials = $1::jsonb WHERE id = $2`
				}
				if _, err := tx.ExecContext(ctx, query, encoded, account.id); err != nil {
					return err
				}
			}
		}
		if key := workspaceIdentityDedupeKey(account.credentials); key != "" {
			groups[key] = append(groups[key], *account)
		}
	}
	var loserIDs []int64
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.SliceStable(group, func(i, j int) bool {
			return oauthIdentityDedupeWinnerLess(group[i], group[j])
		})
		for _, loser := range group[1:] {
			loserIDs = append(loserIDs, loser.id)
		}
	}
	if len(loserIDs) == 0 {
		return nil
	}
	sort.Slice(loserIDs, func(i, j int) bool { return loserIDs[i] < loserIDs[j] })
	if err := softDeleteAccountsTx(ctx, tx, loserIDs); err != nil {
		return err
	}
	if err := insertAccountEventsTx(ctx, tx, loserIDs, "deleted", "workspace_identity_v3"); err != nil {
		return err
	}
	log.Printf("[data_migration] %s: soft-deleted %d duplicate accounts", dataMigrationWorkspaceIdentityV3, len(loserIDs))
	return nil
}

func workspaceIdentityMigrationEligible(credentials map[string]interface{}) bool {
	if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "auth_mode")), "agentIdentity") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "upstream_type"))) {
	case "grok", "openai_responses":
		return false
	default:
		return true
	}
}

func workspaceIdentityDedupeKey(credentials map[string]interface{}) string {
	email := strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "email")))
	workspaceID := openaiidentity.NormalizeWorkspaceID(credentialStringFromMap(credentials, "workspace_id"))
	if email == "" || workspaceID == "" {
		return ""
	}
	return email + "\x00" + workspaceID
}

func (db *DB) listOAuthIdentityDedupeAccounts(ctx context.Context, tx *sql.Tx) ([]oauthIdentityDedupeAccount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, credentials, COALESCE(enabled, true), COALESCE(locked, false), created_at, updated_at
		FROM accounts
		WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []oauthIdentityDedupeAccount
	for rows.Next() {
		var account oauthIdentityDedupeAccount
		var rawCredentials interface{}
		var createdRaw interface{}
		var updatedRaw interface{}
		if err := rows.Scan(
			&account.id,
			&rawCredentials,
			&account.enabled,
			&account.locked,
			&createdRaw,
			&updatedRaw,
		); err != nil {
			return nil, err
		}
		account.credentials = decodeCredentials(rawCredentials)
		account.createdAt, err = parseDBTimeValue(createdRaw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse created_at of account %d: %w", account.id, err)
		}
		account.updatedAt, err = parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, fmt.Errorf("failed to parse updated_at of account %d: %w", account.id, err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func oauthIdentityDedupeAliases(credentials map[string]interface{}) []string {
	// Copies force-imported by the user ticking "allow duplicate add" carry the allow_duplicate flag,
	// they are deliberate duplicates (for example the same account with a different proxy) and must not take part in merging.
	if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "allow_duplicate")), "true") {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "email")))
	if email == "" {
		return nil
	}
	seen := make(map[string]struct{}, 2)
	// user_id is also an identity alias: a personal account may have no workspace account_id, and the old wham
	// backfill once wrote user_id into the account_id field; both shapes must be able to merge into the same group.
	for _, key := range []string{"account_id", "chatgpt_account_id", "user_id"} {
		accountID := strings.TrimSpace(credentialStringFromMap(credentials, key))
		if accountID == "" {
			continue
		}
		seen[email+"\x00"+accountID] = struct{}{}
	}
	aliases := make([]string, 0, len(seen))
	for alias := range seen {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

func credentialStringFromMap(credentials map[string]interface{}, key string) string {
	if credentials == nil {
		return ""
	}
	value, ok := credentials[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		return ""
	}
}

func oauthIdentityDedupeWinnerLess(a, b oauthIdentityDedupeAccount) bool {
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	if scoreA, scoreB := oauthIdentityCredentialScore(a.credentials), oauthIdentityCredentialScore(b.credentials); scoreA != scoreB {
		return scoreA > scoreB
	}
	if a.enabled != b.enabled {
		return a.enabled
	}
	if a.locked != b.locked {
		return !a.locked
	}
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.After(b.createdAt)
	}
	return a.id > b.id
}

func oauthIdentityCredentialScore(credentials map[string]interface{}) int {
	score := 0
	if strings.TrimSpace(credentialStringFromMap(credentials, "access_token")) != "" {
		score += 4
	}
	if strings.TrimSpace(credentialStringFromMap(credentials, "refresh_token")) != "" {
		score += 2
	}
	if strings.TrimSpace(credentialStringFromMap(credentials, "session_token")) != "" {
		score++
	}
	return score
}

func softDeleteAccountsTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = fmt.Sprintf("$%d", j+1)
			args = append(args, id)
		}
		query := fmt.Sprintf(`
			UPDATE accounts
			SET status = 'deleted',
				error_message = '',
				cooldown_reason = '',
				cooldown_until = NULL,
				deleted_at = CURRENT_TIMESTAMP,
				updated_at = CURRENT_TIMESTAMP
			WHERE status <> 'deleted' AND id IN (%s)
		`, strings.Join(placeholders, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to soft-delete duplicate account: %w", err)
		}

		// Keep the duplicate account's last group snapshot for historical usage
		// attribution. The account is already excluded from the active pool.
	}
	return nil
}

func insertAccountEventsTx(ctx context.Context, tx *sql.Tx, ids []int64, eventType, source string) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+2)
		args = append(args, eventType, source)
		for j, id := range batch {
			paramIdx := j + 3
			placeholders[j] = fmt.Sprintf("($%d, $1, $2)", paramIdx)
			args = append(args, id)
		}
		query := fmt.Sprintf(`INSERT INTO account_events (account_id, event_type, source) VALUES %s`, strings.Join(placeholders, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to record duplicate account cleanup event: %w", err)
		}
	}
	return nil
}
