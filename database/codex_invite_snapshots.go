package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// There are two kinds of Codex invite snapshots. Eligibility and sent-record data change at different speeds, so each expires on its own.
const (
	CodexInviteSnapshotEligibility = "eligibility"
	CodexInviteSnapshotTracking    = "tracking"
)

// CodexInviteSnapshot is one successful upstream invite query result, kept per account + kind + scope.
//
// Scope is "the full identity of one result set", built by the caller per kind: eligibility uses program|entrypoint,
// sent records also carry period|limit: limit=10 and limit=100 are two different data sets, so keying only by
// program would make the former be returned as the latter.
//
// Only successful queries are written: these two upstream endpoints sit behind Cloudflare bot management, and the challenge page
// borrows the 403 code that also means "not eligible", so storing failures as snapshots would let one challenge wipe out the last usable quota.
// CredentialGeneration takes part in the write condition: after an account is re-authorized, old snapshots cannot be read back and cannot hold back a newer value.
type CodexInviteSnapshot struct {
	AccountID            int64
	Kind                 string
	Scope                string
	CredentialGeneration int64
	HTTPStatus           int
	Payload              json.RawMessage
	ObservedAt           time.Time
	ExpiresAt            time.Time
	UpdatedAt            time.Time
}

// Expired reports whether the snapshot is past its soft TTL. expires_at is an explicit column rather than computed from observed_at on the fly,
// so that when quota changes (for example, an invite was just sent) the row can be set to expired early.
func (s *CodexInviteSnapshot) Expired(now time.Time) bool {
	return s == nil || s.ExpiresAt.IsZero() || !now.Before(s.ExpiresAt)
}

var (
	codexInviteSnapshotInitMu sync.Mutex
	codexInviteSnapshotReady  = make(map[*DB]bool)
)

func normalizeCodexInviteKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == CodexInviteSnapshotTracking {
		return CodexInviteSnapshotTracking
	}
	return CodexInviteSnapshotEligibility
}

func (db *DB) ensureCodexInviteSnapshotTable(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database unavailable")
	}
	codexInviteSnapshotInitMu.Lock()
	defer codexInviteSnapshotInitMu.Unlock()
	if codexInviteSnapshotReady[db] {
		return nil
	}

	ddl := `CREATE TABLE IF NOT EXISTS codex_invite_snapshots (
		account_id BIGINT NOT NULL,
		snapshot_kind TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT '',
		credential_generation BIGINT NOT NULL DEFAULT 1,
		http_status INT NOT NULL DEFAULT 0,
		payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
		observed_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (account_id, snapshot_kind, scope)
	)`
	if db.isSQLite() {
		ddl = `CREATE TABLE IF NOT EXISTS codex_invite_snapshots (
			account_id INTEGER NOT NULL,
			snapshot_kind TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			credential_generation INTEGER NOT NULL DEFAULT 1,
			http_status INTEGER NOT NULL DEFAULT 0,
			payload_json TEXT NOT NULL DEFAULT '{}',
			observed_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, snapshot_kind, scope)
		)`
	}
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_codex_invite_snapshots_expires ON codex_invite_snapshots(expires_at)`); err != nil {
		return err
	}

	codexInviteSnapshotReady[db] = true
	return nil
}

// GetCodexInviteSnapshot reads one invite snapshot. When there is no record it returns (nil, nil): "not queried yet"
// is a normal state, and callers should not have to distinguish sql.ErrNoRows. Whether it is expired is decided by the caller with its own TTL.
func (db *DB) GetCodexInviteSnapshot(ctx context.Context, accountID int64, kind, scope string) (*CodexInviteSnapshot, error) {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return nil, err
	}
	kind = normalizeCodexInviteKind(kind)
	scope = strings.TrimSpace(scope)

	snap := &CodexInviteSnapshot{}
	var payload, observed, expires, updated any
	err := db.conn.QueryRowContext(ctx, `SELECT account_id,snapshot_kind,scope,credential_generation,http_status,payload_json,observed_at,expires_at,updated_at
		FROM codex_invite_snapshots WHERE account_id=$1 AND snapshot_kind=$2 AND scope=$3`,
		accountID, kind, scope).Scan(&snap.AccountID, &snap.Kind, &snap.Scope,
		&snap.CredentialGeneration, &snap.HTTPStatus, &payload, &observed, &expires, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	snap.Payload = json.RawMessage(bytesFromDBValue(payload))
	if snap.ObservedAt, err = parseDBTimeValue(observed); err != nil {
		return nil, err
	}
	if snap.ExpiresAt, err = parseDBTimeValue(expires); err != nil {
		return nil, err
	}
	if snap.UpdatedAt, err = parseDBTimeValue(updated); err != nil {
		return nil, err
	}
	return snap, nil
}

// UpsertCodexInviteSnapshot writes a successful observation. The DO UPDATE with the credential_generation
// monotonic condition: when an old probe still in flight returns after re-authorization, it cannot overwrite the snapshot written under the new credential generation.
func (db *DB) UpsertCodexInviteSnapshot(ctx context.Context, snap *CodexInviteSnapshot) error {
	if snap == nil {
		return fmt.Errorf("snapshot is empty")
	}
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return err
	}

	payload := snap.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	generation := snap.CredentialGeneration
	if generation <= 0 {
		generation = 1
	}
	observedAt := snap.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	query := `INSERT INTO codex_invite_snapshots
		(account_id,snapshot_kind,scope,credential_generation,http_status,payload_json,observed_at,expires_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,CURRENT_TIMESTAMP)
		ON CONFLICT(account_id,snapshot_kind,scope) DO UPDATE SET
		credential_generation=excluded.credential_generation,http_status=excluded.http_status,
		payload_json=excluded.payload_json,observed_at=excluded.observed_at,
		expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP
		WHERE codex_invite_snapshots.credential_generation <= excluded.credential_generation`
	if !db.isSQLite() {
		query = strings.Replace(query, "$6,$7", "$6::jsonb,$7", 1)
	}

	_, err := db.conn.ExecContext(ctx, query,
		snap.AccountID, normalizeCodexInviteKind(snap.Kind), strings.TrimSpace(snap.Scope),
		generation, snap.HTTPStatus, string(payload),
		db.timeArg(observedAt), db.timeArg(snap.ExpiresAt))
	return err
}

// DeleteCodexInviteSnapshots clears all invite snapshots of one account. It is called after an invite is sent successfully:
// the quota has just been consumed by this gateway, so any cached value is already wrong, and keeping it is worse than having none.
func (db *DB) DeleteCodexInviteSnapshots(ctx context.Context, accountID int64) error {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_invite_snapshots WHERE account_id=$1`, accountID)
	return err
}

// PurgeExpiredCodexInviteSnapshots deletes expired rows older than before, and is called by background cleanup.
// Returns the number of rows deleted; when the driver does not support RowsAffected it returns 0, which is not treated as an error.
func (db *DB) PurgeExpiredCodexInviteSnapshots(ctx context.Context, before time.Time) (int64, error) {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return 0, err
	}
	res, err := db.conn.ExecContext(ctx,
		`DELETE FROM codex_invite_snapshots WHERE expires_at < $1`, db.timeArg(before))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return affected, nil
}
