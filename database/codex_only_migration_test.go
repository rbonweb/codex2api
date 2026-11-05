package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexOnlySeed holds the ids a scenario needs to assert on.
type codexOnlySeed struct {
	keep    map[string]int64 // accounts that must survive, by label
	removed map[string]int64 // accounts that must be deleted, by label
}

func seedCodexOnlyScenario(t *testing.T, ctx context.Context, db *DB) (codexOnlySeed, map[string]int64, map[string]int64) {
	t.Helper()
	insert := func(label string, creds map[string]interface{}) int64 {
		t.Helper()
		id, err := db.InsertAccountWithUpstream(ctx, label, "openai", "oauth", creds, "")
		if err != nil {
			t.Fatalf("insert account %s: %v", label, err)
		}
		return id
	}
	seed := codexOnlySeed{keep: map[string]int64{}, removed: map[string]int64{}}
	seed.keep["codex"] = insert("codex", map[string]interface{}{"upstream_type": "codex", "access_token": "at-codex", "refresh_token": "rt-codex", "email": "codex@example.com"})
	seed.keep["legacy-no-upstream-type"] = insert("legacy", map[string]interface{}{"access_token": "at-legacy", "refresh_token": "rt-legacy"})
	seed.keep["empty-upstream-type"] = insert("empty", map[string]interface{}{"upstream_type": "", "access_token": "at-empty"})
	seed.keep["unknown-upstream-type"] = insert("unknown", map[string]interface{}{"upstream_type": "mystery", "access_token": "at-unknown"})
	seed.keep["agent-identity"] = insert("agent", map[string]interface{}{"upstream_type": "codex", "auth_mode": "agentIdentity", "agent_runtime_id": "rt-1", "agent_private_key": "pk", "task_id": "task-1"})
	seed.keep["agent-identity-no-type"] = insert("agent2", map[string]interface{}{"auth_mode": "agentIdentity", "agent_runtime_id": "rt-2", "agent_private_key": "pk2"})
	seed.keep["soft-deleted-codex"] = insert("deleted-codex", map[string]interface{}{"upstream_type": "codex", "access_token": "at-del"})
	seed.removed["grok"] = insert("grok", map[string]interface{}{"upstream_type": "grok", "access_token": "at-grok"})
	seed.removed["grok-mixed-case"] = insert("grok2", map[string]interface{}{"upstream_type": " Grok ", "access_token": "at-grok2"})
	seed.removed["xai"] = insert("xai", map[string]interface{}{"upstream_type": "xai", "api_key": "xai-key"})
	seed.removed["claude"] = insert("claude", map[string]interface{}{"upstream_type": "claude", "access_token": "at-claude"})
	seed.removed["antigravity"] = insert("antigravity", map[string]interface{}{"upstream_type": "antigravity", "access_token": "at-ag"})
	seed.removed["relay"] = insert("relay", map[string]interface{}{"upstream_type": "openai_responses", "base_url": "https://relay.example.com", "api_key": "sk-relay"})
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status = 'deleted' WHERE id = $1`, seed.keep["soft-deleted-codex"]); err != nil {
		t.Fatalf("soft delete codex account: %v", err)
	}

	// Usage logs for every account.
	for _, id := range append(allIDs(seed.keep), allIDs(seed.removed)...) {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs (account_id, endpoint, model, status_code) VALUES ($1, '/v1/responses', 'gpt-5.4', 200)`, id); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}

	// Dependent rows: group membership, cooldown and Daybreak snapshot for a kept and a removed account.
	groupMixed, err := db.CreateAccountGroup(ctx, "mixed", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, id := range []int64{seed.keep["codex"], seed.keep["agent-identity"], seed.removed["grok"], seed.removed["claude"]} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id) VALUES ($1, $2)`, id, groupMixed); err != nil {
			t.Fatalf("insert group member: %v", err)
		}
	}
	for _, id := range []int64{seed.keep["codex"], seed.removed["grok"]} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_model_cooldowns (account_id, model, reason, reset_at) VALUES ($1, 'gpt-5.4', 'test', '2999-01-01 00:00:00')`, id); err != nil {
			t.Fatalf("insert cooldown: %v", err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO daybreak_snapshots (account_id, identity, observed_at, checked_at, models_json) VALUES ($1, 'id', 1, 1, '{}')`, id); err != nil {
			t.Fatalf("insert daybreak snapshot: %v", err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO maintenance_jobs (entity_id, job_kind, due_at) VALUES ($1, 'grok_freshness', CURRENT_TIMESTAMP)`, id); err != nil {
			t.Fatalf("insert maintenance job: %v", err)
		}
	}

	// Groups with every channel, API keys with every channel.
	groups := map[string]int64{}
	for _, channel := range []string{"codex", "grok", "claude", "antigravity", "openai_responses", "relay", " Claude "} {
		id, err := db.CreateAccountGroup(ctx, "group-"+strings.TrimSpace(channel), "", "", 0, 0, sql.NullInt64{})
		if err != nil {
			t.Fatalf("create group %q: %v", channel, err)
		}
		if _, err := db.conn.ExecContext(ctx, `UPDATE account_groups SET channel = $1 WHERE id = $2`, channel, id); err != nil {
			t.Fatalf("set group channel: %v", err)
		}
		groups[channel] = id
	}
	keys := map[string]int64{}
	for _, channel := range []string{"", "auto", "codex", "grok", "claude", "antigravity", "openai_responses", "relay", "Grok"} {
		id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Name: "key-" + channel, Key: "sk-test-" + channel + "-key", Limits: APIKeyLimits{UpstreamChannel: channel, AllowLive: true}})
		if err != nil {
			t.Fatalf("insert api key %q: %v", channel, err)
		}
		keys[channel] = id
	}
	return seed, groups, keys
}

func allIDs(m map[string]int64) []int64 {
	out := make([]int64, 0, len(m))
	for _, id := range m {
		out = append(out, id)
	}
	return out
}

func codexOnlyCount(t *testing.T, ctx context.Context, db *DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.conn.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// snapshotKeptRows reads every kept account's full row so a before/after comparison proves it was not modified.
func snapshotKeptRows(t *testing.T, ctx context.Context, db *DB, ids map[string]int64) map[string]string {
	t.Helper()
	out := map[string]string{}
	for label, id := range ids {
		var name, platform, status, creds string
		if err := db.conn.QueryRowContext(ctx, `SELECT name, platform, status, CAST(credentials AS TEXT) FROM accounts WHERE id = $1`, id).Scan(&name, &platform, &status, &creds); err != nil {
			t.Fatalf("read kept account %s (%d): %v", label, id, err)
		}
		out[label] = name + "|" + platform + "|" + status + "|" + creds
	}
	return out
}

func runCodexOnlyScenario(t *testing.T, db *DB) {
	ctx := context.Background()
	seed, groups, keys := seedCodexOnlyScenario(t, ctx, db)
	// An upgraded database still has the Grok tables (a fresh schema no longer creates them).
	for _, table := range codexOnlyGrokTables {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (account_id BIGINT NOT NULL DEFAULT 0)`); err != nil {
			t.Fatalf("create legacy table %s: %v", table, err)
		}
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO `+table+` (account_id) VALUES ($1)`, seed.removed["grok"]); err != nil {
			t.Fatalf("seed legacy table %s: %v", table, err)
		}
	}
	before := snapshotKeptRows(t, ctx, db, seed.keep)
	usageBefore := map[string]int64{}
	for label, id := range seed.keep {
		usageBefore[label] = codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, id)
		if usageBefore[label] != 1 {
			t.Fatalf("seed: expected 1 usage log for %s, got %d", label, usageBefore[label])
		}
	}
	keysBefore := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM api_keys`)
	groupsBefore := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM account_groups`)

	runMigration := func() {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version = $1`, dataMigrationCodexOnlyV1); err != nil {
			t.Fatalf("reset migration key: %v", err)
		}
		if err := db.runDataMigrations(ctx); err != nil {
			t.Fatalf("runDataMigrations: %v", err)
		}
		if codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM data_migrations WHERE version = $1`, dataMigrationCodexOnlyV1) != 1 {
			t.Fatalf("migration key %s was not recorded", dataMigrationCodexOnlyV1)
		}
	}

	// Without the key row the migration must run; with it present a second pass must be a no-op.
	runMigration()

	// Removed accounts are gone with their dependents.
	for label, id := range seed.removed {
		if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM accounts WHERE id = $1`, id); n != 0 {
			t.Errorf("removed account %s (%d) still exists", label, id)
		}
		for _, q := range []string{
			`SELECT COUNT(*) FROM account_group_members WHERE account_id = $1`,
			`SELECT COUNT(*) FROM account_model_cooldowns WHERE account_id = $1`,
			`SELECT COUNT(*) FROM daybreak_snapshots WHERE account_id = $1`,
			`SELECT COUNT(*) FROM maintenance_jobs WHERE entity_id = $1`,
		} {
			if n := codexOnlyCount(t, ctx, db, q, id); n != 0 {
				t.Errorf("removed account %s still has dependent rows: %s", label, q)
			}
		}
	}

	// Kept accounts are byte-for-byte unchanged, and so are their usage logs and dependents.
	after := snapshotKeptRows(t, ctx, db, seed.keep)
	for label, want := range before {
		if after[label] != want {
			t.Errorf("kept account %s changed:\n before %s\n after  %s", label, want, after[label])
		}
		if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, seed.keep[label]); n != usageBefore[label] {
			t.Errorf("usage logs of %s: %d -> %d", label, usageBefore[label], n)
		}
	}
	for _, label := range []string{"codex", "agent-identity"} {
		id := seed.keep[label]
		if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM account_group_members WHERE account_id = $1`, id); n != 1 {
			t.Errorf("%s lost its group membership", label)
		}
	}
	codexID := seed.keep["codex"]
	for _, q := range []string{
		`SELECT COUNT(*) FROM account_model_cooldowns WHERE account_id = $1`,
		`SELECT COUNT(*) FROM daybreak_snapshots WHERE account_id = $1`,
	} {
		if n := codexOnlyCount(t, ctx, db, q, codexID); n != 1 {
			t.Errorf("codex account lost dependent row: %s", q)
		}
	}
	// The kept Codex account's grok_freshness job is not touched by the account delete, but the job kind itself is retired.
	if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM maintenance_jobs WHERE job_kind = 'grok_freshness'`); n != 0 {
		t.Errorf("grok_freshness jobs remain: %d", n)
	}
	// Usage logs of removed accounts are history and stay.
	for label, id := range seed.removed {
		if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM usage_logs WHERE account_id = $1`, id); n != 1 {
			t.Errorf("history usage log of removed account %s: want 1, got %d", label, n)
		}
	}

	// API keys and groups are never deleted, only their channel is reset.
	if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM api_keys`); n != keysBefore {
		t.Errorf("api keys %d -> %d", keysBefore, n)
	}
	if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM account_groups`); n != groupsBefore {
		t.Errorf("account groups %d -> %d", groupsBefore, n)
	}
	wantKeyChannel := map[string]string{"": "", "auto": "auto", "codex": "codex", "grok": "codex", "claude": "codex", "antigravity": "codex", "openai_responses": "codex", "relay": "codex", "Grok": "codex"}
	for channel, id := range keys {
		var raw string
		if err := db.conn.QueryRowContext(ctx, `SELECT CAST(limits AS TEXT) FROM api_keys WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatalf("read api key limits: %v", err)
		}
		var limits APIKeyLimits
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			t.Fatalf("decode limits %q: %v", raw, err)
		}
		if limits.UpstreamChannel != wantKeyChannel[channel] {
			t.Errorf("api key with channel %q now has channel %q, want %q", channel, limits.UpstreamChannel, wantKeyChannel[channel])
		}
		if !limits.AllowLive {
			t.Errorf("api key with channel %q lost its other limits (allow_live): %s", channel, raw)
		}
	}
	for channel, id := range groups {
		var got string
		if err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(channel, '') FROM account_groups WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read group channel: %v", err)
		}
		if got != "codex" {
			t.Errorf("group with channel %q now has channel %q, want codex", channel, got)
		}
	}

	// Grok tables are gone.
	for _, table := range codexOnlyGrokTables {
		var exists bool
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		exists, err = db.codexOnlyTableExists(ctx, tx, table)
		tx.Rollback()
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if exists {
			t.Errorf("table %s still exists", table)
		}
	}

	// Running again (key reset, nothing left to remove) changes nothing.
	runMigration()
	if got := snapshotKeptRows(t, ctx, db, seed.keep); len(got) != len(before) {
		t.Fatalf("kept accounts after second run: %d, want %d", len(got), len(before))
	} else {
		for label, want := range before {
			if got[label] != want {
				t.Errorf("kept account %s changed on second run", label)
			}
		}
	}
	if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM accounts`); n != int64(len(seed.keep)) {
		t.Errorf("accounts after second run: %d, want %d", n, len(seed.keep))
	}
}

func TestCodexOnlyMigrationSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex-only.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	runCodexOnlyScenario(t, db)
}

// TestCodexOnlyMigrationPostgres runs the same scenario against a real PostgreSQL when an isolated DSN is configured.
func TestCodexOnlyMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	runCodexOnlyScenario(t, db)
}

// TestCodexOnlyMigrationStatementsPostgres checks the SQL text issued on the PostgreSQL path without needing a server.
func TestCodexOnlyMigrationStatementsPostgres(t *testing.T) {
	st := buildCodexOnlyStatements(false, nil)

	if !strings.Contains(st.removedPred, `LOWER(BTRIM(COALESCE(credentials->>'upstream_type', '')))`) {
		t.Errorf("PostgreSQL predicate must read the normalised credentials->>'upstream_type': %s", st.removedPred)
	}
	for _, forbidden := range []string{"json_extract", "json_set", "json_valid", "sqlite"} {
		for _, stmt := range append([]string{st.countByType, st.deleteAccounts, st.resetKeys, st.resetGroups}, st.dependents...) {
			if strings.Contains(stmt, forbidden) {
				t.Errorf("PostgreSQL statement uses SQLite-only %q: %s", forbidden, stmt)
			}
		}
	}
	// The delete may only ever target the five explicit removed types. Codex must be unreachable: empty or unknown never match.
	wantList := `IN ('claude','grok','xai','antigravity','openai_responses')`
	if !strings.HasSuffix(st.removedPred, wantList) {
		t.Errorf("removed-account predicate = %s, want it to end with %s", st.removedPred, wantList)
	}
	for _, codex := range []string{"'codex'", "NOT IN", " OR ", "<>", "''', '''"} {
		if strings.Contains(st.deleteAccounts, codex) {
			t.Errorf("delete statement must not contain %s: %s", codex, st.deleteAccounts)
		}
	}
	if strings.Contains(wantList, "''") || strings.Contains(wantList, "'codex'") {
		t.Fatalf("removed type list must be explicit and exclude empty and codex: %s", wantList)
	}
	if st.deleteAccounts != `DELETE FROM accounts WHERE `+st.removedPred {
		t.Errorf("unexpected delete statement: %s", st.deleteAccounts)
	}
	if len(st.dependents) != len(codexOnlyAccountDependents) {
		t.Fatalf("dependents = %d, want %d (nil existence map keeps all)", len(st.dependents), len(codexOnlyAccountDependents))
	}
	for _, stmt := range st.dependents {
		if !strings.HasPrefix(stmt, "DELETE FROM ") || !strings.Contains(stmt, "IN (SELECT id FROM accounts WHERE "+st.removedPred+")") {
			t.Errorf("dependent delete is not scoped to removed accounts: %s", stmt)
		}
	}
	if !strings.Contains(st.resetKeys, `jsonb_set(limits, '{upstream_channel}', '"codex"'::jsonb, false)`) || !strings.Contains(st.resetKeys, `jsonb_typeof(limits) = 'object'`) {
		t.Errorf("unexpected API key reset: %s", st.resetKeys)
	}
	if strings.Contains(st.resetKeys, "'codex')") && strings.Contains(st.resetKeys, "IN (") {
		list := st.resetKeys[strings.LastIndex(st.resetKeys, "IN ("):]
		if strings.Contains(list, "'codex'") || strings.Contains(list, "'auto'") {
			t.Errorf("reset must not match codex or auto channels: %s", list)
		}
	}
	if !strings.HasPrefix(st.resetGroups, `UPDATE account_groups SET channel = 'codex' WHERE `) || strings.Contains(st.resetGroups[strings.Index(st.resetGroups, "IN ("):], "'codex'") {
		t.Errorf("unexpected group reset: %s", st.resetGroups)
	}
	if len(st.dropTables) != 6 {
		t.Fatalf("drop statements = %d, want 6", len(st.dropTables))
	}
	for _, stmt := range st.dropTables {
		if !strings.HasPrefix(stmt, "DROP TABLE IF EXISTS grok_") {
			t.Errorf("unexpected drop: %s", stmt)
		}
	}

	// A table that does not exist is skipped, never queried (a missing table would abort the whole PostgreSQL transaction).
	partial := buildCodexOnlyStatements(false, map[string]bool{"account_group_members": true})
	if len(partial.dependents) != 1 || !strings.HasPrefix(partial.dependents[0], "DELETE FROM account_group_members ") {
		t.Errorf("existence filter not applied: %v", partial.dependents)
	}

	// The SQLite variant must use the SQLite JSON functions instead.
	lite := buildCodexOnlyStatements(true, nil)
	if !strings.Contains(lite.removedPred, "json_extract(credentials, '$.upstream_type')") || !strings.Contains(lite.resetKeys, "json_set(limits") {
		t.Errorf("SQLite statements do not use json functions: %s / %s", lite.removedPred, lite.resetKeys)
	}
}

// The retired provider-only migrations must still record their key and do nothing else.
func TestRetiredProviderMigrationsOnlyRecordTheirKey(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "retired.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, version := range []string{dataMigrationGroupChannelV1, dataMigrationClaudeProviderV1, dataMigrationCodexOnlyV1} {
		if n := codexOnlyCount(t, ctx, db, `SELECT COUNT(*) FROM data_migrations WHERE version = $1`, version); n != 1 {
			t.Errorf("migration key %s recorded %d times, want 1", version, n)
		}
	}
	if err := markDataMigrationDoneOnly(ctx, nil); err != nil {
		t.Fatalf("stub returned %v", err)
	}
}

// Token refresh uses the credentials CAS for every account; it must keep working once the migration has dropped the Grok tables.
func TestCodexCredentialsCASSucceedsAfterGrokTablesDropped(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex-cas.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for _, table := range codexOnlyGrokTables {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (account_id BIGINT NOT NULL DEFAULT 0)`); err != nil {
			t.Fatalf("create legacy table %s: %v", table, err)
		}
	}
	id, err := db.InsertAccountWithUpstream(ctx, "codex", "openai", "oauth", map[string]interface{}{"upstream_type": "codex", "access_token": "at-old", "refresh_token": "rt-old", "account_id": "ws-1"}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version = $1`, dataMigrationCodexOnlyV1); err != nil {
		t.Fatalf("reset key: %v", err)
	}
	if err := db.runDataMigrations(ctx); err != nil {
		t.Fatalf("runDataMigrations: %v", err)
	}
	for _, table := range codexOnlyGrokTables {
		if _, err := db.conn.ExecContext(ctx, `SELECT 1 FROM `+table); err == nil {
			t.Fatalf("table %s should be dropped", table)
		}
	}

	generation, family, err := db.GetAccountCredentialState(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountCredentialState: %v", err)
	}
	newGeneration, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, generation, map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	if err != nil || !applied || newGeneration != generation+1 {
		t.Fatalf("UpdateAccountCredentialsCAS = generation %d applied %v err %v", newGeneration, applied, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("access_token"); got != "at-new" {
		t.Errorf("access_token = %q, want at-new", got)
	}
	if got := row.GetCredential("upstream_type"); got != "codex" {
		t.Errorf("upstream_type = %q, want codex", got)
	}
	_, afterFamily, err := db.GetAccountCredentialState(ctx, id)
	if err != nil || strings.TrimSpace(afterFamily) == "" {
		t.Errorf("credential family after CAS = %q err %v (before %q)", afterFamily, err, family)
	}
	// A stale generation is rejected without touching anything.
	if _, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, generation, map[string]any{"access_token": "stale"}); err != nil || applied {
		t.Errorf("stale CAS applied=%v err=%v", applied, err)
	}
	if _, applied, err := db.ReplaceAccountCredentialsCAS(ctx, id, newGeneration, "", map[string]any{"access_token": "at-replaced"}); err != nil || !applied {
		t.Errorf("ReplaceAccountCredentialsCAS applied=%v err=%v", applied, err)
	}
}

// Startup fills empty credential family ids (Codex and Agent-Identity rows included) and leaves existing ones alone.
func TestEnsureCredentialFamilyBackfill(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "family.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	codexID, err := db.InsertAccountWithUpstream(ctx, "codex", "openai", "oauth", map[string]interface{}{"upstream_type": "codex", "refresh_token": "rt", "account_id": "ws-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	agentID, err := db.InsertAccountWithUpstream(ctx, "agent", "openai", "oauth", map[string]interface{}{"auth_mode": "agentIdentity", "agent_runtime_id": "rt-1", "agent_private_key": "pk"}, "")
	if err != nil {
		t.Fatal(err)
	}
	keptID, err := db.InsertAccountWithUpstream(ctx, "kept", "openai", "oauth", map[string]interface{}{"upstream_type": "codex", "refresh_token": "rt2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id = '' WHERE id IN ($1, $2)`, codexID, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id = 'cf_keep' WHERE id = $1`, keptID); err != nil {
		t.Fatal(err)
	}
	if err := db.ensureCredentialFamilyBackfill(ctx); err != nil {
		t.Fatalf("ensureCredentialFamilyBackfill: %v", err)
	}
	for _, id := range []int64{codexID, agentID} {
		if _, family, err := db.GetAccountCredentialState(ctx, id); err != nil || strings.TrimSpace(family) == "" {
			t.Errorf("account %d family = %q err %v, want filled", id, family, err)
		}
	}
	if _, family, _ := db.GetAccountCredentialState(ctx, keptID); family != "cf_keep" {
		t.Errorf("existing family changed to %q", family)
	}
	// Idempotent: a second pass changes nothing.
	_, before, _ := db.GetAccountCredentialState(ctx, codexID)
	if err := db.ensureCredentialFamilyBackfill(ctx); err != nil {
		t.Fatal(err)
	}
	if _, after, _ := db.GetAccountCredentialState(ctx, codexID); after != before {
		t.Errorf("family changed on second pass: %q -> %q", before, after)
	}
}
