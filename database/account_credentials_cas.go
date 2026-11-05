package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

const credentialFamilyBackfillBatchSize = 500

// ensureCredentialFamilyBackfill fills accounts.credential_family_id for rows that still have an empty family.
// It replaces the family phase of the retired Grok historical backfill and is idempotent: a startup with nothing to fill issues one empty query.
func (db *DB) ensureCredentialFamilyBackfill(ctx context.Context) error {
	for {
		processed := 0
		err := db.withSQLiteWriteLock(ctx, func() error {
			tx, err := db.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			processed, err = db.backfillCredentialFamiliesBatch(ctx, tx, credentialFamilyBackfillBatchSize)
			if err != nil {
				return fmt.Errorf("backfill credential families: %w", err)
			}
			return tx.Commit()
		})
		if err != nil {
			return err
		}
		if processed == 0 {
			return nil
		}
	}
}

func credentialFamilyCandidate(credentials map[string]any) string {
	if explicit, _ := credentials["credential_family_id"].(string); strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := credentials[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	isGrok := strings.EqualFold(get("upstream_type"), "grok")
	authKind := "oauth"
	if get("api_key") != "" {
		authKind = "api_key"
	}
	issuer := strings.ToLower(strings.TrimRight(get("grok_oidc_issuer"), "/"))
	if isGrok && issuer == "" && authKind == "oauth" {
		issuer = "https://auth.x.ai"
	}
	origin := get("base_url")
	if isGrok && origin == "" {
		if authKind == "api_key" {
			origin = "https://api.x.ai/v1"
		} else {
			origin = "https://cli-chat-proxy.grok.com/v1"
		}
	}
	if parsed, err := url.Parse(origin); err == nil && parsed.Hostname() != "" {
		host := strings.ToLower(parsed.Hostname())
		if port := parsed.Port(); port != "" && port != "443" && port != "80" {
			host += ":" + port
		}
		origin = strings.ToLower(parsed.Scheme) + "://" + host
	} else {
		origin = strings.ToLower(strings.TrimRight(origin, "/"))
	}
	principal := get("grok_principal_id", "account_id", "chatgpt_account_id", "user_id")
	identity := strings.Join([]string{
		"grok-credential-family-v1", authKind, issuer, get("grok_client_id"),
		strings.ToLower(get("grok_principal_type")), principal, origin,
	}, "\x00")
	if principal == "" {
		fallback := get("api_key", "refresh_token", "access_token")
		if fallback == "" {
			return ""
		}
		identity += "\x00fallback\x00" + fallback
	}
	if !isGrok && strings.TrimSpace(principal) == "" && get("api_key", "refresh_token", "access_token") == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (db *DB) backfillCredentialFamilies(ctx context.Context) error {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, credentials FROM accounts WHERE COALESCE(credential_family_id, '') = ''`)
	if err != nil {
		return err
	}
	type pending struct {
		id     int64
		family string
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		family := credentialFamilyCandidate(decodeCredentials(raw))
		if family == "" {
			family = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		updates = append(updates, pending{id: id, family: family})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id = $1 WHERE id = $2 AND COALESCE(credential_family_id, '') = ''`, update.family, update.id); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) backfillCredentialFamiliesBatch(ctx context.Context, tx *sql.Tx, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,credentials FROM accounts
		WHERE COALESCE(credential_family_id,'')=''
		ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id     int64
		family string
	}
	updates := make([]pending, 0, limit)
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		family := credentialFamilyCandidate(decodeCredentials(raw))
		if family == "" {
			family = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		updates = append(updates, pending{id: id, family: family})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2 AND COALESCE(credential_family_id,'')=''`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, update := range updates {
		if _, err := stmt.ExecContext(ctx, update.family, update.id); err != nil {
			return 0, err
		}
	}
	return len(updates), nil
}

func (db *DB) GetAccountCredentialState(ctx context.Context, accountID int64) (generation int64, familyID string, err error) {
	err = db.conn.QueryRowContext(ctx, `SELECT credential_generation, credential_family_id FROM accounts WHERE id = $1`, accountID).Scan(&generation, &familyID)
	return
}

// EnsureAccountCredentialFamilyID only fills an empty family. A credential
// family is stable across token rotation and is never reassigned in place.
func (db *DB) EnsureAccountCredentialFamilyID(ctx context.Context, accountID int64, candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		candidate = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2 AND COALESCE(credential_family_id,'')=''`, candidate, accountID); err != nil {
		return "", err
	}
	var family string
	if err := db.conn.QueryRowContext(ctx, `SELECT credential_family_id FROM accounts WHERE id=$1`, accountID).Scan(&family); err != nil {
		return "", err
	}
	return family, nil
}

// UpdateAccountCredentialsCAS merges a refreshed identity only if it still
// belongs to expectedGeneration. A successful write advances the generation;
// callers must publish tokens to memory/cache only after applied=true.
func (db *DB) UpdateAccountCredentialsCAS(ctx context.Context, accountID, expectedGeneration int64, updates map[string]any) (newGeneration int64, applied bool, err error) {
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		query := `SELECT credentials, credential_generation, credential_family_id FROM accounts WHERE id=$1 AND status <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		var current int64
		var familyID string
		if scanErr := tx.QueryRowContext(ctx, query, accountID).Scan(&raw, &current, &familyID); scanErr != nil {
			return scanErr
		}
		if current != expectedGeneration {
			newGeneration = current
			return nil
		}
		merged := mergeCredentialMaps(decodeCredentials(raw), updates)
		familyID = strings.TrimSpace(familyID)
		if familyID == "" {
			familyID = credentialFamilyCandidate(merged)
		}
		if familyID == "" {
			familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		// Keep the compatibility JSON field synchronized with the canonical
		// column in the same write that publishes the rotated credential.
		merged["credential_family_id"] = familyID
		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(merged))
		if marshalErr != nil {
			return marshalErr
		}
		updateQuery := `UPDATE accounts SET credentials=$1, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=CURRENT_TIMESTAMP WHERE id=$3 AND credential_generation=$4`
		if !db.isSQLite() {
			updateQuery = `UPDATE accounts SET credentials=$1::jsonb, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=NOW() WHERE id=$3 AND credential_generation=$4`
		}
		res, execErr := tx.ExecContext(ctx, updateQuery, encoded, familyID, accountID, expectedGeneration)
		if execErr != nil {
			return execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if n == 0 {
			return nil
		}
		applied = true
		newGeneration = expectedGeneration + 1
		return tx.Commit()
	})
	return
}

// ReplaceAccountCredentialsCAS atomically replaces an account identity. Unlike
// routine token refresh, it updates the canonical family column and does not
// carry generation-fenced provider observations forward.
func (db *DB) ReplaceAccountCredentialsCAS(ctx context.Context, accountID, expectedGeneration int64, familyID string, updates map[string]any) (newGeneration int64, applied bool, err error) {
	if db == nil || db.conn == nil || accountID <= 0 || expectedGeneration <= 0 || len(updates) == 0 {
		return 0, false, nil
	}
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()

		query := `SELECT credentials, credential_generation FROM accounts WHERE id=$1 AND status <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		var current int64
		if scanErr := tx.QueryRowContext(ctx, query, accountID).Scan(&raw, &current); scanErr != nil {
			return scanErr
		}
		if current != expectedGeneration {
			newGeneration = current
			return nil
		}
		merged := mergeCredentialMaps(decodeCredentials(raw), updates)
		familyID = strings.TrimSpace(familyID)
		if familyID == "" {
			familyID = credentialFamilyCandidate(merged)
		}
		if familyID == "" {
			familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		merged["credential_family_id"] = familyID
		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(merged))
		if marshalErr != nil {
			return marshalErr
		}
		updateQuery := `UPDATE accounts SET credentials=$1, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=CURRENT_TIMESTAMP WHERE id=$3 AND credential_generation=$4`
		if !db.isSQLite() {
			updateQuery = `UPDATE accounts SET credentials=$1::jsonb, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=NOW() WHERE id=$3 AND credential_generation=$4`
		}
		res, execErr := tx.ExecContext(ctx, updateQuery, encoded, familyID, accountID, expectedGeneration)
		if execErr != nil {
			return execErr
		}
		rows, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if rows == 0 {
			return nil
		}
		applied = true
		newGeneration = expectedGeneration + 1
		return tx.Commit()
	})
	return
}
