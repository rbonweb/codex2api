package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// attempt_index is 1-based: the first attempt writes 1, the first retry writes 2. The retry count in the request breakdown used to be written as
// attempt_index > 0, so every request counted as a retry, and this metric always equaled the total request count (the UI showed 100%).
func TestFeatureStatsRetryCountsOnlyRetryAttempts(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "retry-stats.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	const accountID int64 = 42
	logs := []*UsageLogInput{
		// A successful first attempt: not a retry.
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 200, AttemptIndex: 1},
		// A failed first attempt (which triggered a retry) plus a successful second attempt: counts as 1 retry.
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 429, AttemptIndex: 1, IsRetryAttempt: true},
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 200, AttemptIndex: 2},
		// A hidden continue-thinking round does not count as an attempt, so attempt_index stays 0.
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 200},
	}
	for _, input := range logs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.flushLogs()

	now := time.Now()
	stats, err := db.GetUsageStats(ctx, now.Add(-time.Hour), now.Add(time.Hour), "")
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if got := stats.FeatureStats.RetryRequests; got != 1 {
		t.Fatalf("RetryRequests = %d, want 1 (only the attempt_index=2 row is a retry)", got)
	}
	if got := stats.FeatureStats.ErrorRequests; got != 1 {
		t.Fatalf("ErrorRequests = %d, want 1", got)
	}

	// The retry count in the account usage details goes through a different SQL with the same definition; do not fix only one side.
	detail, err := db.GetAccountUsageStats(ctx, accountID, 7)
	if err != nil {
		t.Fatalf("GetAccountUsageStats: %v", err)
	}
	if got := detail.RetryRequests; got != 1 {
		t.Fatalf("account RetryRequests = %d, want 1", got)
	}
}

func TestAccountRequestCountsExcludeClientCanceledAndKeepRawUsage(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-cancel-counts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	logs := []*UsageLogInput{
		{AccountID: 41, Channel: "codex", StatusCode: 200},
		{AccountID: 41, Channel: "codex", StatusCode: 500},
		{AccountID: 41, Channel: "codex", StatusCode: 429},
		{AccountID: 41, Channel: "codex", StatusCode: 429, IsRetryAttempt: true},
		{AccountID: 41, Channel: "codex", StatusCode: 499},
		{AccountID: 41, Channel: "codex", StatusCode: 499, IsRetryAttempt: true},
		{AccountID: 41, Channel: "codex", StatusCode: 500, InternalReason: "test_probe"},
		{AccountID: 42, Channel: "codex", StatusCode: 499},
		{AccountID: 43, Channel: "codex", StatusCode: 499},
	}
	for _, entry := range logs {
		entry.Endpoint = "/v1/responses"
		entry.Model = "gpt-5.4"
		entry.AttemptIndex = 1
		if entry.StatusCode == 499 {
			entry.InputTokens, entry.OutputTokens, entry.TotalTokens = 11, 7, 18
			entry.CachedTokens, entry.CacheWrite5mTokens = 3, 4
			entry.ErrorMessage = "下游请求提前取消: context canceled"
		}
		if err := db.InsertUsageLog(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	db.flushLogs()
	for _, tc := range []struct {
		name      string
		breakdown bool
		read      func() (map[int64]*AccountRequestCount, error)
	}{
		{"all accounts", true, func() (map[int64]*AccountRequestCount, error) { return db.GetAccountRequestCounts(ctx) }},
		{"visible page", true, func() (map[int64]*AccountRequestCount, error) {
			return db.GetAccountRequestCountsByIDs(ctx, []int64{41, 42, 43})
		}},
		{"cached totals", false, func() (map[int64]*AccountRequestCount, error) {
			return db.GetAccountRequestCountTotalsByIDs(ctx, []int64{41, 42, 43})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts, err := tc.read()
			if err != nil {
				t.Fatal(err)
			}
			got := counts[41]
			if got == nil || got.SuccessCount != 1 || got.ErrorCount != 2 || got.RetryErrorCount != 1 || got.RateLimitAttemptCount != 2 {
				t.Fatalf("counts must retain real failures and exclude cancellations: %+v", got)
			}
			if tc.breakdown && (len(got.ErrorStatusCounts) != 2 || got.ErrorStatusCounts[500] != 1 || got.ErrorStatusCounts[429] != 1 || got.ErrorStatusCounts[499] != 0) {
				t.Fatalf("unexpected error breakdown: %+v", got.ErrorStatusCounts)
			}
			for _, id := range []int64{42, 43} {
				got := counts[id]
				if got == nil || got.SuccessCount != 0 || got.ErrorCount != 0 || got.RetryErrorCount != 0 || got.RateLimitAttemptCount != 0 || len(got.ErrorStatusCounts) != 0 {
					t.Fatalf("cancellation-only account %d must retain zero counters: %+v", id, got)
				}
			}
		})
	}
	var count, input, output, cached, written int
	err = db.conn.QueryRowContext(ctx, `SELECT COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cached_tokens), SUM(cache_write_5m_tokens)
		FROM usage_logs WHERE status_code = 499 AND error_message = $1`, "下游请求提前取消: context canceled").Scan(&count, &input, &output, &cached, &written)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || input != 44 || output != 28 || cached != 12 || written != 16 {
		t.Fatalf("raw canceled usage changed: rows=%d input=%d output=%d cached=%d written=%d", count, input, output, cached, written)
	}
}
