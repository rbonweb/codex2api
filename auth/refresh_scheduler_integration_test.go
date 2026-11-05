package auth

import (
	"testing"
	"time"
)

func TestRefreshSchedulerIntegrationEnableDisable(t *testing.T) {
	store := &Store{}

	// Initial state.
	if store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be false by default")
	}

	// Enabled.
	config := DefaultRefreshConfig()
	config.MaxConcurrency = 5
	store.EnableRefreshScheduler(config)

	if !store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be true after enabling")
	}

	scheduler := store.GetRefreshScheduler()
	if scheduler == nil {
		t.Fatal("GetRefreshScheduler should return non-nil after enabling")
	}

	// Disabled.
	store.DisableRefreshScheduler()

	if store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be false after disabling")
	}
}

func TestRefreshSchedulerIntegrationSchedule(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	acc := newTestAccountWithExpiry(1, 10*time.Minute)

	// Schedule a refresh.
	store.ScheduleAccountRefresh(acc)

	// Check the task status (the scheduler checks internally; this only verifies that it does not panic).
	state, attempts, _ := store.GetRefreshTaskStatus(1)
	// Since the task was just created, its state should be Pending, or Success if it was processed immediately.
	t.Logf("Task state: %d, attempts: %d", state, attempts)
}

func TestRefreshSchedulerIntegrationMetrics(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	// Initial metrics.
	metrics := store.GetRefreshMetrics()
	if metrics.TotalTasks != 0 {
		t.Fatalf("initial TotalTasks = %d, want 0", metrics.TotalTasks)
	}

	// Add a task.
	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	store.ScheduleAccountRefresh(acc)

	// Get metrics.
	metrics = store.GetRefreshMetrics()
	if metrics.TotalTasks != 1 {
		t.Fatalf("after schedule TotalTasks = %d, want 1", metrics.TotalTasks)
	}
}

func TestRefreshSchedulerIntegrationCancel(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	store.ScheduleAccountRefresh(acc)

	// Cancel the task.
	cancelled := store.CancelRefreshTask(1)
	if !cancelled {
		t.Fatal("CancelRefreshTask should return true")
	}

	// Canceling again should fail.
	cancelled = store.CancelRefreshTask(1)
	if cancelled {
		t.Fatal("CancelRefreshTask should return false for already cancelled task")
	}
}

func TestStoreStopDisablesRefreshScheduler(t *testing.T) {
	store := &Store{stopCh: make(chan struct{})}
	store.EnableRefreshScheduler(DefaultRefreshConfig())
	if store.GetRefreshScheduler() == nil {
		t.Fatal("GetRefreshScheduler() = nil after EnableRefreshScheduler")
	}

	store.Stop()
	if store.GetRefreshScheduler() != nil {
		t.Fatal("GetRefreshScheduler() != nil after Store.Stop")
	}
}
