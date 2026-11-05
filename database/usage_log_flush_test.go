package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// insertUsageLogs pushes count minimal usage logs into the buffer.
func insertUsageLogs(t *testing.T, db *DB, count int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{
			Endpoint:    "/v1/responses",
			Model:       "gpt-5.4",
			StatusCode:  200,
			TotalTokens: 10,
		}); err != nil {
			t.Fatalf("InsertUsageLog(%d) returned error: %v", i, err)
		}
	}
}

// TestFlushUsageLogsDrainsBeyondOneBatch covers the batching semantics of flush: flushLogs takes only
// usage_log_batch_size rows at a time, and FlushUsageLogs must loop until the whole buffer is flushed.
func TestFlushUsageLogsDrainsBeyondOneBatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}
	// Stop the background flusher so the test fully controls when flushing happens.
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	const batchSize = 10
	const total = 35
	db.SetUsageLogConfig(UsageLogModeFull, batchSize, maxUsageLogFlushIntervalSeconds)
	insertUsageLogs(t, db, total)

	db.flushLogs()
	logs, err := db.ListRecentUsageLogs(context.Background(), total*2)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs returned error: %v", err)
	}
	if len(logs) != batchSize {
		t.Fatalf("flushLogs persisted %d rows, want %d (a single call should flush only one batch)", len(logs), batchSize)
	}

	db.FlushUsageLogs()
	logs, err = db.ListRecentUsageLogs(context.Background(), total*2)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs returned error: %v", err)
	}
	if len(logs) != total {
		t.Fatalf("FlushUsageLogs persisted %d rows, want %d", len(logs), total)
	}
	if stats := db.GetUsageLogRuntimeStats(); stats.BufferLength != 0 {
		t.Fatalf("BufferLength = %d, want 0", stats.BufferLength)
	}
}

func TestSQLiteUsageLogFlushWaitsForUnifiedWriteLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	db.SetUsageLogConfig(UsageLogModeFull, 10, maxUsageLogFlushIntervalSeconds)
	insertUsageLogs(t, db, 1)
	db.sqliteWriteSem <- struct{}{}
	flushed := make(chan struct{})
	go func() {
		db.FlushUsageLogs()
		close(flushed)
	}()
	select {
	case <-flushed:
		t.Fatal("FlushUsageLogs bypassed SQLite write lock")
	case <-time.After(50 * time.Millisecond):
	}
	<-db.sqliteWriteSem
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("FlushUsageLogs did not resume after SQLite write lock release")
	}
	logs, err := db.ListRecentUsageLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs returned error: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("flushed logs = %d, want 1", len(logs))
	}
}

// TestCloseFlushesEntireUsageLogBuffer covers graceful shutdown: Close stops the background flusher first,
// at that point the path of `flush one batch + notifyLogFlush` has no consumer for the signal, so logs beyond one batch
// would be silently dropped, so the shutdown must flush the whole buffer.
func TestCloseFlushesEntireUsageLogBuffer(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}

	const total = 35
	db.SetUsageLogConfig(UsageLogModeFull, maxUsageLogBatchSize, maxUsageLogFlushIntervalSeconds)
	insertUsageLogs(t, db, total)
	db.SetUsageLogConfig(UsageLogModeFull, 10, maxUsageLogFlushIntervalSeconds)

	if err := db.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening the database returned error: %v", err)
	}
	defer reopened.Close()

	logs, err := reopened.ListRecentUsageLogs(context.Background(), total*2)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs returned error: %v", err)
	}
	if len(logs) != total {
		t.Fatalf("after Close, persisted %d rows, want %d (logs in the buffer were dropped)", len(logs), total)
	}
}

// TestUsageLogTextClampedToColumnWidth covers truncation of over-long fields: reasoning_effort / service_tier
// and similar values come straight from the downstream request body; exceeding the column width rolls back the whole batch INSERT, and the failed batch is put back at the head of the buffer,
// so a single bad row can block log writes forever.
func TestUsageLogTextClampedToColumnWidth(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}
	defer db.Close()

	long := strings.Repeat("x", 300)
	if err := db.InsertUsageLog(context.Background(), &UsageLogInput{
		Endpoint:             long,
		Model:                long,
		EffectiveModel:       long,
		ReasoningEffort:      long,
		ServiceTier:          long,
		RequestedServiceTier: long,
		ActualServiceTier:    long,
		Channel:              long,
		StatusCode:           200,
	}); err != nil {
		t.Fatalf("InsertUsageLog returned error: %v", err)
	}
	db.FlushUsageLogs()

	logs, err := db.ListRecentUsageLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs returned error: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	got := logs[0]
	for _, tc := range []struct {
		name  string
		value string
		max   int
	}{
		{"endpoint", got.Endpoint, usageLogTextMaxLen},
		{"model", got.Model, usageLogTextMaxLen},
		{"effective_model", got.EffectiveModel, usageLogTextMaxLen},
		{"reasoning_effort", got.ReasoningEffort, usageLogTextMaxLen},
		{"service_tier", got.ServiceTier, usageLogTextMaxLen},
		{"requested_service_tier", got.RequestedServiceTier, usageLogTextMaxLen},
		{"actual_service_tier", got.ActualServiceTier, usageLogTextMaxLen},
		{"channel", got.Channel, usageLogChannelMaxLen},
	} {
		if len(tc.value) != tc.max {
			t.Fatalf("%s length = %d, want %d", tc.name, len(tc.value), tc.max)
		}
	}
}

func TestClampUsageLogText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"returned as is when not over-long", "high", 100, "high"},
		{"exactly at the limit", "abcd", 4, "abcd"},
		{"truncates by characters", "abcdef", 4, "abcd"},
		{"multibyte input is truncated by characters, not bytes", "思考强度很高", 3, "思考强"},
		{"non-positive limit does not truncate", "abc", 0, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampUsageLogText(tc.in, tc.max); got != tc.want {
				t.Fatalf("clampUsageLogText(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestUsageLogInsertRowsStayUnderPostgresBindLimit locks the row limit of each INSERT:
// rows * columns per row must stay below PostgreSQL's 65535 bind parameters.
func TestUsageLogInsertRowsStayUnderPostgresBindLimit(t *testing.T) {
	if maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount > postgresMaxBindParams {
		t.Fatalf("single INSERT parameter count = %d, exceeds the PostgreSQL limit %d",
			maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount, postgresMaxBindParams)
	}
	if maxUsageLogBatchSize*usageLogInsertColumnCount <= postgresMaxBindParams {
		t.Logf("note: the current batch size limit %d would not exceed the parameter limit even without sharding", maxUsageLogBatchSize)
	}
}

// dataError builds a PostgreSQL data-class error (class 22: over-long values, invalid bytes, numeric overflow and similar).
func dataError() error {
	return &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(100)"}
}

// transientError builds a transient failure (class 08: connection exception).
func transientError() error {
	return &pgconn.PgError{Code: "08006", Message: "connection failure"}
}

func TestIsUsageLogDataError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a data error", nil, false},
		{"class 22 data exception", dataError(), true},
		{"class 23 constraint violation", &pgconn.PgError{Code: "23505"}, true},
		{"class 08 connection exception, should retry", transientError(), false},
		{"class 40 deadlock, should retry", &pgconn.PgError{Code: "40P01"}, false},
		{"class 53 insufficient resources, should retry", &pgconn.PgError{Code: "53100"}, false},
		{"a wrapped data error is still recognized", fmt.Errorf("exec insert: %w", dataError()), true},
		{"non-PostgreSQL SQLSTATE error is treated as transient", context.DeadlineExceeded, false},
		{"SQLSTATE too short is not treated as a data error", &pgconn.PgError{Code: "2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUsageLogDataError(tc.err); got != tc.want {
				t.Fatalf("isUsageLogDataError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSalvageDropsOnlyPoisonRows covers poison-row isolation: when a whole batch is rolled back because one log cannot be written,
// bisection drops only that one row, and the rest must still be persisted (instead of the whole batch going back into the buffer to retry forever).
func TestSalvageDropsOnlyPoisonRows(t *testing.T) {
	batch := make([]usageLogEntry, 16)
	for i := range batch {
		batch[i] = usageLogEntry{Model: fmt.Sprintf("m-%02d", i)}
	}
	poison := "m-05"

	var written []string
	insert := func(chunk []usageLogEntry) error {
		for _, e := range chunk {
			if e.Model == poison {
				return dataError()
			}
		}
		for _, e := range chunk {
			written = append(written, e.Model)
		}
		return nil
	}

	pending, dropped := salvageUsageLogBatchWith(batch, dataError(), insert, func(usageLogEntry, error) {})
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d rows, want 0", len(pending))
	}
	if len(written) != len(batch)-1 {
		t.Fatalf("persisted %d rows, want %d", len(written), len(batch)-1)
	}
	for _, model := range written {
		if model == poison {
			t.Fatalf("poison row %q should not be persisted", poison)
		}
	}
}

// TestSalvageKeepsTransientFailuresForRetry: transient failures must not be dropped as poison rows:
// that half batch must be handed back for retry as is, and the part already written must not be handed back again (otherwise rows are persisted twice and billed twice).
func TestSalvageKeepsTransientFailuresForRetry(t *testing.T) {
	batch := make([]usageLogEntry, 8)
	for i := range batch {
		batch[i] = usageLogEntry{Model: fmt.Sprintf("m-%02d", i)}
	}

	var written []string
	insert := func(chunk []usageLogEntry) error {
		// The second half of the batch hits a connection failure; the first half is written normally.
		for _, e := range chunk {
			if e.Model >= "m-04" {
				return transientError()
			}
		}
		for _, e := range chunk {
			written = append(written, e.Model)
		}
		return nil
	}

	pending, dropped := salvageUsageLogBatchWith(batch, dataError(), insert, func(usageLogEntry, error) {})
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (transient failures must not drop logs)", dropped)
	}
	if len(pending) != 4 {
		t.Fatalf("pending = %d rows, want 4", len(pending))
	}
	if len(written) != 4 {
		t.Fatalf("persisted %d rows, want 4", len(written))
	}
	for _, e := range pending {
		for _, model := range written {
			if e.Model == model {
				t.Fatalf("%q was already persisted and should not be put back in the buffer for retry", model)
			}
		}
	}
}

// TestUsageLogBufferHonorsHardLimit covers the buffer's hard limit: when the database cannot be written for a long time, the buffer must not grow without bound,
// the oldest rows are dropped past the limit and counted, so operators can see from the runtime status how many were dropped.
func TestUsageLogBufferHonorsHardLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	const overflow = 50
	db.SetUsageLogConfig(UsageLogModeFull, maxUsageLogBatchSize, maxUsageLogFlushIntervalSeconds)
	insertUsageLogs(t, db, usageLogBufferHardLimit+overflow)

	stats := db.GetUsageLogRuntimeStats()
	if stats.BufferLength != usageLogBufferHardLimit {
		t.Fatalf("BufferLength = %d, want %d", stats.BufferLength, usageLogBufferHardLimit)
	}
	if stats.DroppedTotal != overflow {
		t.Fatalf("DroppedTotal = %d, want %d", stats.DroppedTotal, overflow)
	}
	if stats.BufferLimit != usageLogBufferHardLimit {
		t.Fatalf("BufferLimit = %d, want %d", stats.BufferLimit, usageLogBufferHardLimit)
	}
}

// TestRequeueHonorsHardLimit: failed batches put back into the buffer must also respect the limit.
func TestRequeueHonorsHardLimit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) returned error: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	db.SetUsageLogConfig(UsageLogModeFull, maxUsageLogBatchSize, maxUsageLogFlushIntervalSeconds)
	insertUsageLogs(t, db, usageLogBufferHardLimit)

	db.requeueUsageLogBatch(make([]usageLogEntry, 100))
	stats := db.GetUsageLogRuntimeStats()
	if stats.BufferLength != usageLogBufferHardLimit {
		t.Fatalf("BufferLength after requeue = %d, want %d", stats.BufferLength, usageLogBufferHardLimit)
	}
	if stats.DroppedTotal != 100 {
		t.Fatalf("DroppedTotal = %d, want 100", stats.DroppedTotal)
	}
}

// TestSalvageDropsPoisonRowAgainstPostgres verifies poison-row isolation on a real PostgreSQL:
// SQLite does not check column widths or return SQLSTATE, so the isUsageLogDataError path can only be exercised against a real database.
// It needs a writable empty database, given by CODEX2API_TEST_POSTGRES_DSN; skipped when it is not set.
func TestSalvageDropsPoisonRowAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set, skipping the PostgreSQL integration test")
	}

	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatalf("New(postgres) returned error: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	ctx := context.Background()
	// Use a CHECK constraint to build a row that can never be written no matter how often it is retried: the real causes (over-long values, invalid bytes,
	// numeric overflow) all fall under SQLSTATE class 22/23 and take the same decision branch.
	if _, err := db.conn.ExecContext(ctx,
		`ALTER TABLE usage_logs ADD CONSTRAINT tmp_salvage_poison CHECK (model <> '__poison__')`); err != nil {
		t.Fatalf("creating the constraint returned error: %v", err)
	}
	defer db.conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS tmp_salvage_poison`)

	const marker = "/salvage-test"
	const clean = 8
	db.SetUsageLogConfig(UsageLogModeFull, maxUsageLogBatchSize, maxUsageLogFlushIntervalSeconds)
	for i := 0; i < clean; i++ {
		model := "gpt-5.4"
		if i == 3 {
			model = "__poison__"
		}
		if err := db.InsertUsageLog(ctx, &UsageLogInput{
			Endpoint:   marker,
			Model:      model,
			StatusCode: 200,
		}); err != nil {
			t.Fatalf("InsertUsageLog(%d) returned error: %v", i, err)
		}
	}

	db.FlushUsageLogs()

	var landed int
	if err := db.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM usage_logs WHERE endpoint = $1`, marker).Scan(&landed); err != nil {
		t.Fatalf("counting persisted rows returned error: %v", err)
	}
	if landed != clean-1 {
		t.Fatalf("persisted %d rows, want %d (logs other than the poison row must still be written)", landed, clean-1)
	}
	stats := db.GetUsageLogRuntimeStats()
	if stats.DroppedTotal != 1 {
		t.Fatalf("DroppedTotal = %d, want 1", stats.DroppedTotal)
	}
	if stats.BufferLength != 0 {
		t.Fatalf("BufferLength = %d, want 0 (a poison row should not stay in the buffer and be retried again and again)", stats.BufferLength)
	}
}
