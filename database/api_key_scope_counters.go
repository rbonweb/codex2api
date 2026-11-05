package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// APIKeyScopeCounter is the cumulative usage counter for one "API key x group/account" pair (issue #439 v2).
//
// Unlike sliding-window limits, a cumulative quota does not fall back over time automatically: it must be reset manually once used up. The semantics align with
// api_keys.quota_limit / quota_used / reset_count / last_reset_at.
// The cap itself is still configured in limits.scope_limits (QuotaCost); this table only stores the used amount and reset state,
// so the same configuration is not stored in two places and does not drift.
type APIKeyScopeCounter struct {
	APIKeyID     int64        `json:"api_key_id"`
	ScopeType    string       `json:"scope_type"`
	ScopeID      int64        `json:"scope_id"`
	UsedCost     float64      `json:"used_cost"`
	UsedTokens   int64        `json:"used_tokens"`
	UsedRequests int64        `json:"used_requests"`
	ResetCount   int          `json:"reset_count"`
	LastResetAt  sql.NullTime `json:"last_reset_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// APIKeyScopeCounterKey uniquely identifies one counter.
type APIKeyScopeCounterKey struct {
	ScopeType string
	ScopeID   int64
}

// ListAPIKeyScopeCounters returns all cumulative counters of a key, indexed by (scope_type, scope_id).
func (db *DB) ListAPIKeyScopeCounters(ctx context.Context, apiKeyID int64) (map[APIKeyScopeCounterKey]APIKeyScopeCounter, error) {
	out := make(map[APIKeyScopeCounterKey]APIKeyScopeCounter)
	if db == nil || apiKeyID <= 0 {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT api_key_id, scope_type, scope_id,
		       COALESCE(used_cost, 0), COALESCE(used_tokens, 0), COALESCE(used_requests, 0),
		       COALESCE(reset_count, 0), last_reset_at, updated_at
		FROM api_key_scope_counters
		WHERE api_key_id = $1
	`, apiKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row APIKeyScopeCounter
		var lastResetRaw, updatedRaw interface{}
		if err := rows.Scan(
			&row.APIKeyID, &row.ScopeType, &row.ScopeID,
			&row.UsedCost, &row.UsedTokens, &row.UsedRequests,
			&row.ResetCount, &lastResetRaw, &updatedRaw,
		); err != nil {
			return nil, err
		}
		lastReset, err := parseDBNullTimeValue(lastResetRaw)
		if err != nil {
			return nil, err
		}
		updatedAt, err := parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, err
		}
		row.LastResetAt = lastReset
		row.UpdatedAt = updatedAt
		out[APIKeyScopeCounterKey{ScopeType: row.ScopeType, ScopeID: row.ScopeID}] = row
	}
	return out, rows.Err()
}

// ListAPIKeyScopeCountersForKeys is the batch version of ListAPIKeyScopeCounters, used by the list page overview
// to fetch the cumulative counters of many keys in one call.
func (db *DB) ListAPIKeyScopeCountersForKeys(ctx context.Context, apiKeyIDs []int64) (map[int64]map[APIKeyScopeCounterKey]APIKeyScopeCounter, error) {
	out := make(map[int64]map[APIKeyScopeCounterKey]APIKeyScopeCounter)
	if db == nil || len(apiKeyIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(apiKeyIDs))
	args := make([]interface{}, 0, len(apiKeyIDs))
	for i, id := range apiKeyIDs {
		if db.isSQLite() {
			placeholders = append(placeholders, "?")
		} else {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		}
		args = append(args, id)
	}
	query := fmt.Sprintf(`
		SELECT api_key_id, scope_type, scope_id,
		       COALESCE(used_cost, 0), COALESCE(used_tokens, 0), COALESCE(used_requests, 0),
		       COALESCE(reset_count, 0)
		FROM api_key_scope_counters
		WHERE api_key_id IN (%s)
	`, strings.Join(placeholders, ","))
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row APIKeyScopeCounter
		if err := rows.Scan(&row.APIKeyID, &row.ScopeType, &row.ScopeID,
			&row.UsedCost, &row.UsedTokens, &row.UsedRequests, &row.ResetCount); err != nil {
			return nil, err
		}
		if out[row.APIKeyID] == nil {
			out[row.APIKeyID] = make(map[APIKeyScopeCounterKey]APIKeyScopeCounter)
		}
		out[row.APIKeyID][APIKeyScopeCounterKey{ScopeType: row.ScopeType, ScopeID: row.ScopeID}] = row
	}
	return out, rows.Err()
}

// ResetAPIKeyScopeCounter clears one cumulative counter to zero and records one reset. If the counter row does not exist, it adds one,
// so that "reset" is an idempotent success even when no usage has happened yet.
func (db *DB) ResetAPIKeyScopeCounter(ctx context.Context, apiKeyID int64, scopeType string, scopeID int64) error {
	if db == nil || apiKeyID <= 0 || scopeID <= 0 {
		return sql.ErrNoRows
	}
	scopeType = strings.ToLower(strings.TrimSpace(scopeType))
	if scopeType != APIKeyScopeTypeGroup && scopeType != APIKeyScopeTypeAccount {
		return fmt.Errorf("unknown scope_type: %q", scopeType)
	}
	query := `
		INSERT INTO api_key_scope_counters (api_key_id, scope_type, scope_id, used_cost, used_tokens, used_requests, reset_count, last_reset_at, updated_at)
		VALUES ($1, $2, $3, 0, 0, 0, 1, ` + db.nowExpr() + `, ` + db.nowExpr() + `)
		ON CONFLICT (api_key_id, scope_type, scope_id) DO UPDATE SET
			used_cost = 0,
			used_tokens = 0,
			used_requests = 0,
			reset_count = COALESCE(api_key_scope_counters.reset_count, 0) + 1,
			last_reset_at = ` + db.nowExpr() + `,
			updated_at = ` + db.nowExpr()
	_, err := db.conn.ExecContext(ctx, query, apiKeyID, scopeType, scopeID)
	return err
}

// nowExpr returns the SQL expression for the current time (the two drivers are written differently).
func (db *DB) nowExpr() string {
	if db.isSQLite() {
		return "CURRENT_TIMESTAMP"
	}
	return "NOW()"
}

// scopeCounterDelta is the cumulative increment derived from one batch of logs.
type scopeCounterDelta struct {
	cost     float64
	tokens   int64
	requests int64
}

// applyAPIKeyScopeCountersWithExec adds the scope cumulative counters within the same transaction as the batched write.
// It books usage only for keys that actually have a cumulative quota configured (see the 60s cache in scopeQuotaKeyIDs); all other keys have zero overhead.
//
// Group membership is not resolved on the Go side; instead SQL joins account_group_members. An account may belong to several
// groups, so the join naturally books each usage to every group it currently belongs to, and the in-memory group relations do not need to be carried into the DB layer.
func (db *DB) applyAPIKeyScopeCountersWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	if db == nil || execer == nil || len(batch) == 0 {
		return nil
	}
	quotaKeys := db.scopeQuotaKeyIDs(ctx)
	if len(quotaKeys) == 0 {
		return nil
	}

	type counterKey struct {
		apiKeyID  int64
		accountID int64
	}
	deltas := make(map[counterKey]*scopeCounterDelta)
	for _, entry := range batch {
		if entry.APIKeyID <= 0 || entry.AccountID <= 0 || entry.StatusCode == 499 {
			continue
		}
		if _, ok := quotaKeys[entry.APIKeyID]; !ok {
			continue
		}
		key := counterKey{apiKeyID: entry.APIKeyID, accountID: entry.AccountID}
		delta := deltas[key]
		if delta == nil {
			delta = &scopeCounterDelta{}
			deltas[key] = delta
		}
		delta.cost += entry.UserBilled
		delta.tokens += int64(entry.TotalTokens)
		delta.requests++
	}
	if len(deltas) == 0 {
		return nil
	}

	accountQuery := `
		INSERT INTO api_key_scope_counters (api_key_id, scope_type, scope_id, used_cost, used_tokens, used_requests, updated_at)
		VALUES ($1, '` + APIKeyScopeTypeAccount + `', $2, $3, $4, $5, ` + db.nowExpr() + `)
		ON CONFLICT (api_key_id, scope_type, scope_id) DO UPDATE SET
			used_cost = COALESCE(api_key_scope_counters.used_cost, 0) + $3,
			used_tokens = COALESCE(api_key_scope_counters.used_tokens, 0) + $4,
			used_requests = COALESCE(api_key_scope_counters.used_requests, 0) + $5,
			updated_at = ` + db.nowExpr()
	groupQuery := `
		INSERT INTO api_key_scope_counters (api_key_id, scope_type, scope_id, used_cost, used_tokens, used_requests, updated_at)
		SELECT $1, '` + APIKeyScopeTypeGroup + `', m.group_id, $3, $4, $5, ` + db.nowExpr() + `
		FROM account_group_members m
		WHERE m.account_id = $2
		ON CONFLICT (api_key_id, scope_type, scope_id) DO UPDATE SET
			used_cost = COALESCE(api_key_scope_counters.used_cost, 0) + $3,
			used_tokens = COALESCE(api_key_scope_counters.used_tokens, 0) + $4,
			used_requests = COALESCE(api_key_scope_counters.used_requests, 0) + $5,
			updated_at = ` + db.nowExpr()

	for key, delta := range deltas {
		if delta.cost <= 0 && delta.tokens <= 0 && delta.requests <= 0 {
			continue
		}
		args := []interface{}{key.apiKeyID, key.accountID, delta.cost, delta.tokens, delta.requests}
		if _, err := execer.ExecContext(ctx, accountQuery, args...); err != nil {
			return err
		}
		if _, err := execer.ExecContext(ctx, groupQuery, args...); err != nil {
			return err
		}
	}
	return nil
}

// scopeQuotaKeyIDs returns the set of API keys with a cumulative quota configured, with a 60s cache.
// The hot write path must not parse every key's limits JSON on every batch; config changes take effect within 60s at most,
// and usage not booked during that window is caught up in the next batch (counters are cumulative, so nothing is lost).
func (db *DB) scopeQuotaKeyIDs(ctx context.Context) map[int64]struct{} {
	db.scopeQuotaMu.Lock()
	if db.scopeQuotaKeys != nil && time.Now().Before(db.scopeQuotaExpiresAt) {
		cached := db.scopeQuotaKeys
		db.scopeQuotaMu.Unlock()
		return cached
	}
	db.scopeQuotaMu.Unlock()

	keys, err := db.ListAPIKeys(ctx)
	if err != nil {
		return nil
	}
	fresh := make(map[int64]struct{})
	for _, key := range keys {
		for _, scope := range key.Limits.ScopeLimits {
			if scope.QuotaCost > 0 || scope.QuotaTokens > 0 || scope.QuotaRequests > 0 {
				fresh[key.ID] = struct{}{}
				break
			}
		}
	}
	db.scopeQuotaMu.Lock()
	db.scopeQuotaKeys = fresh
	db.scopeQuotaExpiresAt = time.Now().Add(time.Minute)
	db.scopeQuotaMu.Unlock()
	return fresh
}

// InvalidateScopeQuotaKeyCache makes the next booking re-resolve immediately which keys have a cumulative quota configured.
// The admin side calls it after saving a key, so a newly configured cumulative quota does not have to wait a whole cache period before booking starts.
func (db *DB) InvalidateScopeQuotaKeyCache() {
	if db == nil {
		return
	}
	db.scopeQuotaMu.Lock()
	db.scopeQuotaKeys = nil
	db.scopeQuotaExpiresAt = time.Time{}
	db.scopeQuotaMu.Unlock()
}
