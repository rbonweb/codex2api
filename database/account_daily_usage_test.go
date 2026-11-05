package database

import (
	"context"
	"testing"
	"time"
)

func TestAccountDailyUsageUpsertOverwritesSameDay(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")

	// Round 1: the day is not yet settled, so upstream returns no token details.
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 7, Day: day, Credits: 100, Turns: 5, Settled: false,
		ClientsJSON: `[{"client_id":"CODEX_CLI"}]`,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Round 2: the same day is now settled. The row must be fully overwritten, not accumulated, otherwise credits would double.
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 7, Day: day, Credits: 250, Turns: 12, TotalTokens: 99, Settled: true,
		ClientsJSON: `[{"client_id":"CODEX_CLI"},{"client_id":"CODEX_UNKNOWN_DEFAULT"}]`,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	rows, err := db.ListAccountDailyUsage(ctx, 7, 7)
	if err != nil {
		t.Fatalf("ListAccountDailyUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected a single row after re-upsert, got %d", len(rows))
	}
	got := rows[0]
	if got.Credits != 250 || got.Turns != 12 || got.TotalTokens != 99 || !got.Settled {
		t.Fatalf("row was not overwritten: %#v", got)
	}
}

func TestAccountDailyUsageSumAndPrune(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	ancient := now.AddDate(0, 0, -400).Format("2006-01-02")

	for _, item := range []AccountDailyUsageInput{
		{AccountID: 1, Day: today, Credits: 25, TotalTokens: 10, Turns: 2, Settled: true},
		{AccountID: 1, Day: yesterday, Credits: 50, TotalTokens: 20, Turns: 3, Settled: true},
		{AccountID: 2, Day: today, Credits: 75, TotalTokens: 30, Turns: 4, Settled: true},
		{AccountID: 1, Day: ancient, Credits: 999, TotalTokens: 999, Turns: 9, Settled: true},
	} {
		if err := db.UpsertAccountDailyUsage(ctx, item); err != nil {
			t.Fatalf("upsert %s: %v", item.Day, err)
		}
	}

	// The summary window is 7 days, so the row from 400 days ago must not be counted.
	totals, err := db.SumAccountDailyUsage(ctx, []int64{1, 2}, 7)
	if err != nil {
		t.Fatalf("SumAccountDailyUsage: %v", err)
	}
	if got := totals[1]; got.Credits != 75 || got.TotalTokens != 30 || got.Turns != 5 {
		t.Fatalf("account 1 totals = %#v", got)
	}
	older := now.AddDate(0, 0, -20).Format("2006-01-02")
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 1, Day: older, Credits: 10, TotalTokens: 4, Turns: 1, Settled: true,
	}); err != nil {
		t.Fatalf("upsert older snapshot: %v", err)
	}
	all, err := db.SumAccountDailyUsage(ctx, []int64{1}, 365)
	if err != nil {
		t.Fatalf("SumAccountDailyUsage 365: %v", err)
	}
	if got := all[1]; got.Credits != 85 || got.TotalTokens != 34 || got.Turns != 6 {
		t.Fatalf("account 1 365d totals = %#v, want credits=85", got)
	}
	if got := totals[2]; got.Credits != 75 || got.TotalTokens != 30 || got.Turns != 4 {
		t.Fatalf("account 2 totals = %#v", got)
	}

	// Accounts not passed in must not appear in the result, to avoid callers misusing it.
	if _, ok := totals[3]; ok {
		t.Fatal("unrequested account leaked into totals")
	}

	if err := db.PruneAccountDailyUsage(ctx, 365); err != nil {
		t.Fatalf("PruneAccountDailyUsage: %v", err)
	}
	rows, err := db.ListAccountDailyUsage(ctx, 1, 500)
	if err != nil {
		t.Fatalf("ListAccountDailyUsage after prune: %v", err)
	}
	for _, row := range rows {
		if row.Day == ancient {
			t.Fatalf("row older than retention survived prune: %s", row.Day)
		}
	}
}

// The breakdown and counts come from two upstream endpoints and are stored separately: whichever arrives first creates the row, and the later one writes only its own columns,
// without overwriting each other. Otherwise, if the breakdown arrives first, counts' credits would be zeroed, or the reverse.
func TestAccountDailyBreakdownUpsertKeepsCountsColumns(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	day := "2026-09-01"

	if cov, err := db.GetAccountDailyUsageCoverage(ctx, 9); err != nil || cov.CountsOldestDay != "" || cov.BreakdownOldestDay != "" {
		t.Fatalf("fresh account must have no coverage: %+v err=%v", cov, err)
	}

	// Breakdown arrives first: create the row with zero counts.
	if err := db.UpsertAccountDailyBreakdown(ctx, AccountDailyBreakdownInput{
		AccountID: 9, Day: day, Percent: 4,
		Models:   []AccountDailyBreakdownModel{{Model: "gpt-5.6-sol", Speed: "fast", Percent: 3}, {Model: "gpt-5.6-luna", Speed: "standard", Percent: 1}},
		Surfaces: map[string]float64{"cli": 4},
	}); err != nil {
		t.Fatalf("breakdown upsert: %v", err)
	}
	// A row created by the breakdown first counts only as breakdown coverage, not counts coverage, and is not included in totals: otherwise, after counts fails in round one,
	// deep backfill would be skipped forever, and the list would still show this account as a real $0 snapshot.
	if cov, err := db.GetAccountDailyUsageCoverage(ctx, 9); err != nil || cov.CountsOldestDay != "" || cov.BreakdownOldestDay != day {
		t.Fatalf("breakdown-only row must count only as breakdown coverage: %+v err=%v", cov, err)
	}
	if sums, err := db.SumAccountDailyUsage(ctx, []int64{9}, 400); err != nil || len(sums) != 0 {
		t.Fatalf("breakdown-only row must not surface as an official snapshot: %#v err=%v", sums, err)
	}
	if rows, err := db.ListAccountDailyUsage(ctx, 9, 400); err != nil || len(rows) != 1 || rows[0].HasCounts() {
		t.Fatalf("breakdown-only row must report HasCounts=false: rows=%d err=%v", len(rows), err)
	}

	// Counts arrive later: they must not clear the breakdown columns.
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 9, Day: day, Credits: 200, Turns: 3, Settled: true, ClientsJSON: "[]", ModelsJSON: "[]",
	}); err != nil {
		t.Fatalf("counts upsert: %v", err)
	}
	rows, err := db.ListAccountDailyUsage(ctx, 9, 400)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: rows=%d err=%v", len(rows), err)
	}
	got := rows[0]
	if got.Credits != 200 || got.Turns != 3 || got.BreakdownPercent != 4 || !got.HasCounts() {
		t.Fatalf("counts overwrote breakdown or vice versa: %#v", got)
	}
	if cov, err := db.GetAccountDailyUsageCoverage(ctx, 9); err != nil || cov.CountsOldestDay != day || cov.BreakdownOldestDay != day {
		t.Fatalf("both endpoints should now cover the day: %+v err=%v", cov, err)
	}
	if sums, err := db.SumAccountDailyUsage(ctx, []int64{9}, 400); err != nil || sums[9].Credits != 200 {
		t.Fatalf("counts row must be summed: %#v err=%v", sums, err)
	}
	models := ParseAccountDailyBreakdownModels(got.BreakdownJSON)
	if len(models) != 2 || models[0].Model != "gpt-5.6-sol" || models[0].Speed != "fast" || models[0].Percent != 3 {
		t.Fatalf("breakdown models = %#v", models)
	}
	if surfaces := ParseAccountDailySurfaces(got.SurfacesJSON); surfaces["cli"] != 4 {
		t.Fatalf("surfaces = %#v", surfaces)
	}

	// Breakdown arrives again (next sync round, a different normalization scale): overwrite the breakdown columns, counts untouched.
	if err := db.UpsertAccountDailyBreakdown(ctx, AccountDailyBreakdownInput{
		AccountID: 9, Day: day, Percent: 10,
		Models: []AccountDailyBreakdownModel{{Model: "gpt-5.6-luna", Speed: "standard", Percent: 10}},
	}); err != nil {
		t.Fatalf("second breakdown upsert: %v", err)
	}
	rows, _ = db.ListAccountDailyUsage(ctx, 9, 400)
	got = rows[0]
	if got.Credits != 200 || got.BreakdownPercent != 10 || len(ParseAccountDailyBreakdownModels(got.BreakdownJSON)) != 1 {
		t.Fatalf("second breakdown upsert went wrong: %#v", got)
	}
	if got.SurfacesJSON != "{}" {
		t.Fatalf("nil surfaces must persist as {}: %q", got.SurfacesJSON)
	}

	if err := db.UpsertAccountDailyBreakdown(ctx, AccountDailyBreakdownInput{AccountID: 9, Day: day, Percent: 0}); err == nil {
		t.Fatal("zero percent total must be rejected instead of writing an empty split")
	}
}
