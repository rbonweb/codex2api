package admin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestScheduledConnectionTestCancelledBeforeExecutionRecordsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduled-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	h := &Handler{store: store, db: db}
	t.Cleanup(func() {
		cancel()
		h.WaitScheduledConnectionTests()
	})
	loc, err := time.LoadLocation(scheduledTestTimezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
	plan, err := json.Marshal(accountScheduledTest{Enabled: true, Model: "gpt-5.5", Content: "hi", WeekdayTimes: []string{"12:00"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccountWithCredentials(ctx, "scheduled-cancel", map[string]interface{}{
		"access_token": "test-token", scheduledTestCredentialKey: string(plan),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Keep every execution slot occupied until after cancellation has finished.
	gate := h.scheduledConnectionTestGate()
	for i := 0; i < cap(gate); i++ {
		gate <- struct{}{}
	}
	h.dispatchScheduledConnectionTests(ctx, now)
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, err := db.ListOneShotMaintenanceJobs(ctx, id, scheduledTestJobPrefix, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued slot was not claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { h.WaitScheduledConnectionTests(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queued batch did not finish after cancellation")
	}
	jobs, err := db.ListOneShotMaintenanceJobs(context.Background(), id, scheduledTestJobPrefix, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	var result scheduledTestRecord
	if err := json.Unmarshal([]byte(jobs[0].LastError), &result); err != nil {
		t.Fatalf("cancelled claim has no result: %q (%v)", jobs[0].LastError, err)
	}
	if result.Status != "failed" || result.Success || result.Error == "" || result.AttemptedAt != "" {
		t.Fatalf("unexpected pre-execution cancellation result: %+v", result)
	}
	if result.AccountID != id || result.Model != "gpt-5.5" || result.ScheduledAt != now.Format(time.RFC3339) {
		t.Fatalf("cancelled result lost slot identity: %+v", result)
	}
	claimed, err := db.TryClaimMaintenanceJob(context.Background(), id, scheduledTestJobPrefix+now.Format("2006-01-02T15:04"), now)
	if err != nil || claimed {
		t.Fatalf("cancelled slot must not be retried: claimed=%v err=%v", claimed, err)
	}
}
