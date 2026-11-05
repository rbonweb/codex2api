package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// AccountDailyUsage is the official settled usage snapshot of one account for one day.
//
// The upstream daily-workspace-usage-counts endpoint can look back only a limited depth (measured 2026-09 at 84+ days, 2026-08
// only 7 days), so history is accumulated long-term by storing rows per day. The values for the same day change before settlement (while the day
// is still running, the record keeps updating), so writes are overwriting upserts, and the snapshot task backfills the whole rolling window each round.
//
// The same row also holds the model x speed breakdown of daily-token-usage-breakdown (the three Breakdown* columns),
// written by the separate UpsertAccountDailyBreakdown: the two upstream endpoints succeed or fail independently, and whichever arrives writes
// its own columns without overwriting the other.
type AccountDailyUsage struct {
	AccountID int64  `json:"account_id"`
	Day       string `json:"day"`

	Credits float64 `json:"credits"`
	Users   int     `json:"users"`
	Threads int     `json:"threads"`
	Turns   int     `json:"turns"`

	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	TotalTokens         int64 `json:"total_tokens"`

	// Settled is false when the day was not yet settled at fetch time (usually the current day).
	// The next snapshot round backfilling the same day flips it to true.
	Settled bool `json:"settled"`

	// ClientsJSON / ModelsJSON store the upstream split arrays as-is; the frontend renders them directly.
	ClientsJSON string    `json:"clients_json"`
	ModelsJSON  string    `json:"models_json"`
	SyncedAt    time.Time `json:"synced_at"`

	// BreakdownPercent / BreakdownJSON / SurfacesJSON come from daily-token-usage-breakdown:
	// the normalized shares of each (model, speed) and each product surface for the day. The upstream normalizes against the peak of the request range,
	// so values are only meaningful within a single day (share = percent / BreakdownPercent) and must never be summed across days
	// or compared across syncs. A BreakdownPercent of 0 means the split for this day has not been synced yet.
	BreakdownPercent float64 `json:"breakdown_percent"`
	BreakdownJSON    string  `json:"breakdown_json"`
	SurfacesJSON     string  `json:"surfaces_json"`
}

// AccountDailyUsageInput is the input of one counts upsert.
type AccountDailyUsageInput struct {
	AccountID           int64
	Day                 string
	Credits             float64
	Users               int
	Threads             int
	Turns               int
	UncachedInputTokens int64
	CachedInputTokens   int64
	OutputTokens        int64
	TotalTokens         int64
	Settled             bool
	ClientsJSON         string
	ModelsJSON          string
}

// AccountDailyBreakdownModel is one persisted (model, speed) share entry.
type AccountDailyBreakdownModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Percent float64 `json:"percent"`
}

// AccountDailyBreakdownInput is the input of one breakdown upsert.
type AccountDailyBreakdownInput struct {
	AccountID int64
	Day       string
	// Percent is the sum of all (model, speed) shares for the day, used as the share denominator.
	Percent  float64
	Models   []AccountDailyBreakdownModel
	Surfaces map[string]float64
}

// ParseAccountDailyBreakdownModels parses BreakdownJSON; bad data falls back to an empty slice.
func ParseAccountDailyBreakdownModels(raw string) []AccountDailyBreakdownModel {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var out []AccountDailyBreakdownModel
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// ParseAccountDailySurfaces parses SurfacesJSON; bad data falls back to an empty table.
func ParseAccountDailySurfaces(raw string) map[string]float64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var out map[string]float64
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

var (
	accountDailyUsageSchemaMu    sync.Mutex
	accountDailyUsageSchemaReady = map[*DB]struct{}{}
)

func (db *DB) ensureAccountDailyUsageTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	accountDailyUsageSchemaMu.Lock()
	defer accountDailyUsageSchemaMu.Unlock()
	if _, ok := accountDailyUsageSchemaReady[db]; ok {
		return nil
	}
	timeType := "TIMESTAMPTZ"
	boolType := "BOOLEAN"
	if db.isSQLite() {
		timeType = "TIMESTAMP"
		boolType = "INTEGER"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS account_daily_usage (
		account_id BIGINT NOT NULL,
		day VARCHAR(10) NOT NULL,
		credits DOUBLE PRECISION NOT NULL DEFAULT 0,
		users INT NOT NULL DEFAULT 0,
		threads INT NOT NULL DEFAULT 0,
		turns INT NOT NULL DEFAULT 0,
		uncached_input_tokens BIGINT NOT NULL DEFAULT 0,
		cached_input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		settled %s NOT NULL DEFAULT FALSE,
		clients_json TEXT NOT NULL DEFAULT '[]',
		models_json TEXT NOT NULL DEFAULT '[]',
		synced_at %s NOT NULL,
		breakdown_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
		breakdown_json TEXT NOT NULL DEFAULT '[]',
		surfaces_json TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY (account_id, day)
	)`, boolType, timeType)
	if db.isSQLite() {
		ddl = strings.Replace(ddl, "DEFAULT FALSE", "DEFAULT 0", 1)
	}
	for _, statement := range []string{
		ddl,
		`CREATE INDEX IF NOT EXISTS idx_account_daily_usage_day ON account_daily_usage(day)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// The three breakdown columns added in 2026-09: old databases get them through idempotent column additions.
	breakdownColumns := []struct{ name, def string }{
		{"breakdown_percent", "DOUBLE PRECISION NOT NULL DEFAULT 0"},
		{"breakdown_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"surfaces_json", "TEXT NOT NULL DEFAULT '{}'"},
	}
	for _, column := range breakdownColumns {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "account_daily_usage", column.name, column.def); err != nil {
				return err
			}
			continue
		}
		if _, err := db.conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE account_daily_usage ADD COLUMN IF NOT EXISTS %s %s`, column.name, column.def)); err != nil {
			return err
		}
	}
	accountDailyUsageSchemaReady[db] = struct{}{}
	return nil
}

// UpsertAccountDailyUsage writes the counts snapshot of one day as an overwrite. The upstream values for the same day change as settlement happens,
// so it always overwrites instead of accumulating. It does not touch the three breakdown columns: those belong to UpsertAccountDailyBreakdown,
// and the two endpoints sync independently.
func (db *DB) UpsertAccountDailyUsage(ctx context.Context, input AccountDailyUsageInput) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	day := strings.TrimSpace(input.Day)
	if input.AccountID <= 0 || day == "" {
		return errors.New("daily usage requires account id and day")
	}
	clients := strings.TrimSpace(input.ClientsJSON)
	if clients == "" {
		clients = "[]"
	}
	models := strings.TrimSpace(input.ModelsJSON)
	if models == "" {
		models = "[]"
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO account_daily_usage (
		account_id, day, credits, users, threads, turns,
		uncached_input_tokens, cached_input_tokens, output_tokens, total_tokens,
		settled, clients_json, models_json, synced_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT (account_id, day) DO UPDATE SET
		credits=EXCLUDED.credits, users=EXCLUDED.users, threads=EXCLUDED.threads,
		turns=EXCLUDED.turns, uncached_input_tokens=EXCLUDED.uncached_input_tokens,
		cached_input_tokens=EXCLUDED.cached_input_tokens, output_tokens=EXCLUDED.output_tokens,
		total_tokens=EXCLUDED.total_tokens, settled=EXCLUDED.settled,
		clients_json=EXCLUDED.clients_json, models_json=EXCLUDED.models_json,
		synced_at=EXCLUDED.synced_at`,
		input.AccountID, day, input.Credits, input.Users, input.Threads, input.Turns,
		input.UncachedInputTokens, input.CachedInputTokens, input.OutputTokens, input.TotalTokens,
		input.Settled, clients, models, time.Now().UTC(),
	)
	return err
}

// UpsertAccountDailyBreakdown writes the model x speed breakdown of one day as an overwrite. It writes only the three breakdown columns:
// if the row does not exist, it creates the row with zero counts (synced_at takes the current time to satisfy the NOT NULL constraint; counts are overwritten when they arrive),
// and if the row already exists, the counts columns and synced_at are left alone.
func (db *DB) UpsertAccountDailyBreakdown(ctx context.Context, input AccountDailyBreakdownInput) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	day := strings.TrimSpace(input.Day)
	if input.AccountID <= 0 || day == "" {
		return errors.New("daily breakdown requires account id and day")
	}
	if input.Percent <= 0 {
		return errors.New("daily breakdown requires a positive percent total")
	}
	models := input.Models
	if models == nil {
		models = []AccountDailyBreakdownModel{}
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return err
	}
	surfaces := input.Surfaces
	if surfaces == nil {
		surfaces = map[string]float64{}
	}
	surfacesJSON, err := json.Marshal(surfaces)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO account_daily_usage (
		account_id, day, synced_at, breakdown_percent, breakdown_json, surfaces_json
	) VALUES ($1,$2,$3,$4,$5,$6)
	ON CONFLICT (account_id, day) DO UPDATE SET
		breakdown_percent=EXCLUDED.breakdown_percent,
		breakdown_json=EXCLUDED.breakdown_json,
		surfaces_json=EXCLUDED.surfaces_json`,
		input.AccountID, day, time.Now().UTC(), input.Percent, string(modelsJSON), string(surfacesJSON),
	)
	return err
}

// accountDailyUsageCountsEvidence reports whether a row really came from the counts endpoint. When the breakdown arrives first it creates the row with zero
// counts (credits/turns/tokens all 0 and not settled). Such a row must not count as "has an official settled snapshot",
// otherwise page-stats would treat it as a real $0 snapshot and deep backfill would be skipped. counts is a sparse endpoint,
// returning only days with activity, so a real counts row has at least one non-zero value.
const accountDailyUsageCountsEvidence = "(settled OR turns>0 OR credits>0 OR total_tokens>0)"

// HasCounts reports whether this row carries data written by the counts endpoint; it has the same meaning as accountDailyUsageCountsEvidence.
func (u *AccountDailyUsage) HasCounts() bool {
	return u != nil && (u.Settled || u.Turns > 0 || u.Credits > 0 || u.TotalTokens > 0)
}

// AccountDailyUsageCoverage is the coverage start of one account's local snapshots, per endpoint.
type AccountDailyUsageCoverage struct {
	// CountsOldestDay is the earliest date with a row carrying counts data; an empty string means none yet.
	CountsOldestDay string
	// BreakdownOldestDay is the earliest date with a row carrying a breakdown; an empty string means none yet.
	BreakdownOldestDay string
}

// GetAccountDailyUsageCoverage returns each endpoint's local coverage start. Sync uses it to decide per endpoint
// whether to deep backfill: checking only "does a row exist" would be fooled by the zero-counts rows that the breakdown created first.
func (db *DB) GetAccountDailyUsageCoverage(ctx context.Context, accountID int64) (AccountDailyUsageCoverage, error) {
	var out AccountDailyUsageCoverage
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return out, err
	}
	if accountID <= 0 {
		return out, errors.New("daily usage requires account id")
	}
	var counts, breakdown sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT
		MIN(CASE WHEN `+accountDailyUsageCountsEvidence+` THEN day END),
		MIN(CASE WHEN breakdown_percent>0 THEN day END)
		FROM account_daily_usage WHERE account_id=$1`, accountID).Scan(&counts, &breakdown)
	if err != nil {
		return out, err
	}
	out.CountsOldestDay = strings.TrimSpace(counts.String)
	out.BreakdownOldestDay = strings.TrimSpace(breakdown.String)
	return out, nil
}

const accountDailyUsageSelect = `SELECT account_id, day, credits, users, threads, turns,
	uncached_input_tokens, cached_input_tokens, output_tokens, total_tokens,
	settled, clients_json, models_json, synced_at,
	breakdown_percent, breakdown_json, surfaces_json FROM account_daily_usage`

// ListAccountDailyUsage returns the snapshots of one account for the most recent days days, in ascending date order.
func (db *DB) ListAccountDailyUsage(ctx context.Context, accountID int64, days int) ([]*AccountDailyUsage, error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return nil, err
	}
	if accountID <= 0 {
		return nil, errors.New("daily usage requires account id")
	}
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := db.conn.QueryContext(ctx,
		accountDailyUsageSelect+` WHERE account_id=$1 AND day>=$2 ORDER BY day ASC`,
		accountID, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*AccountDailyUsage, 0, days)
	for rows.Next() {
		item, scanErr := scanAccountDailyUsage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SumAccountDailyUsage totals the credits and token volume of one account over the most recent days days,
// for the list column and the stats card (without client/model breakdowns, to avoid parsing JSON).
// It counts only rows that carry counts data: zero-counts rows created by the breakdown are not snapshots, otherwise the list would show
// accounts whose counts never synced successfully as $0 and stop backfilling.
func (db *DB) SumAccountDailyUsage(ctx context.Context, accountIDs []int64, days int) (map[int64]AccountDailyUsageTotal, error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return nil, err
	}
	out := make(map[int64]AccountDailyUsageTotal, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	placeholders := make([]string, 0, len(accountIDs))
	args := make([]any, 0, len(accountIDs)+1)
	args = append(args, cutoff)
	for i, id := range accountIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT account_id, COALESCE(SUM(credits),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(turns),0)
		FROM account_daily_usage WHERE day>=$1 AND account_id IN (%s) AND %s GROUP BY account_id`,
		strings.Join(placeholders, ","), accountDailyUsageCountsEvidence)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var total AccountDailyUsageTotal
		if err := rows.Scan(&id, &total.Credits, &total.TotalTokens, &total.Turns); err != nil {
			return nil, err
		}
		out[id] = total
	}
	return out, rows.Err()
}

// SumAccountDailyUsageSince totals all snapshots carrying counts data from sinceDay (inclusive) onward,
// returning the credits total and the number of days. It is used by the "used this period" card, which sums from the start of the account's reset period.
func (db *DB) SumAccountDailyUsageSince(ctx context.Context, accountID int64, sinceDay string) (credits float64, days int, err error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return 0, 0, err
	}
	sinceDay = strings.TrimSpace(sinceDay)
	if accountID <= 0 || sinceDay == "" {
		return 0, 0, errors.New("daily usage sum requires account id and start day")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(credits),0), COUNT(*) FROM account_daily_usage
		WHERE account_id=$1 AND day>=$2 AND `+accountDailyUsageCountsEvidence, accountID, sinceDay).Scan(&credits, &days)
	if err != nil {
		return 0, 0, err
	}
	return credits, days, nil
}

// AccountDailyUsageTotal is one account's totals within a window.
type AccountDailyUsageTotal struct {
	Credits     float64 `json:"credits"`
	TotalTokens int64   `json:"total_tokens"`
	Turns       int64   `json:"turns"`
}

// PruneAccountDailyUsage deletes snapshots older than the retention period, so the table does not grow without bound.
func (db *DB) PruneAccountDailyUsage(ctx context.Context, keepDays int) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	if keepDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	_, err := db.conn.ExecContext(ctx, `DELETE FROM account_daily_usage WHERE day<$1`, cutoff)
	return err
}

// AccountDailySurface is the share entry of one product surface.
type AccountDailySurface struct {
	Surface string
	Percent float64
}

// SortedAccountDailySurfaces sorts the surface share table by share, descending, with ties ordered by name, into a stable list.
func SortedAccountDailySurfaces(surfaces map[string]float64) []AccountDailySurface {
	out := make([]AccountDailySurface, 0, len(surfaces))
	for name, value := range surfaces {
		out = append(out, AccountDailySurface{Surface: name, Percent: value})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Percent != out[j].Percent {
			return out[i].Percent > out[j].Percent
		}
		return out[i].Surface < out[j].Surface
	})
	return out
}

func scanAccountDailyUsage(scanner interface{ Scan(...any) error }) (*AccountDailyUsage, error) {
	item := &AccountDailyUsage{}
	var syncedAt any
	if err := scanner.Scan(
		&item.AccountID, &item.Day, &item.Credits, &item.Users, &item.Threads, &item.Turns,
		&item.UncachedInputTokens, &item.CachedInputTokens, &item.OutputTokens, &item.TotalTokens,
		&item.Settled, &item.ClientsJSON, &item.ModelsJSON, &syncedAt,
		&item.BreakdownPercent, &item.BreakdownJSON, &item.SurfacesJSON,
	); err != nil {
		return nil, err
	}
	parsed, err := parsePromptRiskTimeValue(syncedAt)
	if err != nil {
		return nil, err
	}
	item.SyncedAt = parsed
	return item, nil
}
