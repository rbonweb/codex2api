package auth

import (
	"testing"
	"time"
)

func newTestAccountWithExpiry(id int64, expiresIn time.Duration) *Account {
	acc := &Account{
		DBID:           id,
		AccessToken:    "test_token",
		RefreshToken:   "test_refresh_token",
		Status:         StatusReady,
		HealthTier:     HealthTierHealthy,
		SchedulerScore: 100,
	}
	if expiresIn > 0 {
		acc.ExpiresAt = time.Now().Add(expiresIn)
	}
	return acc
}

func TestNewRefreshScheduler(t *testing.T) {
	config := DefaultRefreshConfig()
	config.MaxConcurrency = 5

	store := &Store{}
	scheduler := NewRefreshScheduler(store, config)

	if scheduler == nil {
		t.Fatal("NewRefreshScheduler returned nil")
	}
	if scheduler.config.MaxConcurrency != 5 {
		t.Fatalf("MaxConcurrency = %d, want 5", scheduler.config.MaxConcurrency)
	}
	if scheduler.store != store {
		t.Fatal("store mismatch")
	}
}

func TestRefreshSchedulerSchedule(t *testing.T) {
	config := DefaultRefreshConfig()
	store := &Store{}
	scheduler := NewRefreshScheduler(store, config)

	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	scheduler.Schedule(acc)

	// Check that the task was created
	scheduler.tasksMu.RLock()
	task, ok := scheduler.tasks[1]
	scheduler.tasksMu.RUnlock()

	if !ok {
		t.Fatal("task not found after Schedule")
	}

	task.mu.RLock()
	if task.State != RefreshStatePending {
		t.Fatalf("task.State = %d, want RefreshStatePending", task.State)
	}
	task.mu.RUnlock()

	// Check the metrics
	if scheduler.metrics.TotalTasks.Load() != 1 {
		t.Fatalf("TotalTasks = %d, want 1", scheduler.metrics.TotalTasks.Load())
	}
}

func TestRefreshSchedulerCalculateOptimalTime(t *testing.T) {
	config := DefaultRefreshConfig()
	config.PreExpireWindow = 5 * time.Minute
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Test an account that is about to expire
	acc1 := newTestAccountWithExpiry(1, 1*time.Minute)
	optimal1 := scheduler.calculateOptimalTime(acc1)
	if time.Until(optimal1) > 30*time.Second {
		t.Fatalf("imminent expiry should schedule soon, got %v", time.Until(optimal1))
	}

	// Test an account whose expiry is far away
	acc2 := newTestAccountWithExpiry(2, 30*time.Minute)
	optimal2 := scheduler.calculateOptimalTime(acc2)
	timeUntilExpire := acc2.ExpiresAt.Sub(optimal2)
	// Should be around PreExpireWindow before expiry
	if timeUntilExpire < 4*time.Minute || timeUntilExpire > 6*time.Minute {
		t.Fatalf("expected refresh near pre-expire window, time until expire: %v", timeUntilExpire)
	}
}

func TestRefreshSchedulerCalculatePriority(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Healthy account with plenty of time left
	acc1 := newTestAccountWithExpiry(1, 30*time.Minute)
	acc1.HealthTier = HealthTierHealthy
	priority1 := scheduler.calculatePriority(acc1)

	// Risky account
	acc2 := newTestAccountWithExpiry(2, 30*time.Minute)
	acc2.HealthTier = HealthTierRisky
	priority2 := scheduler.calculatePriority(acc2)

	// The healthy account's priority should be higher than the risky account's
	if priority1 <= priority2 {
		t.Fatalf("healthy priority %d should be > risky priority %d", priority1, priority2)
	}

	// A healthy account that is about to expire should have higher priority
	acc3 := newTestAccountWithExpiry(3, 10*time.Second)
	acc3.HealthTier = HealthTierHealthy
	priority3 := scheduler.calculatePriority(acc3)
	if priority3 <= priority1 {
		t.Fatalf("imminent expiry priority %d should be > normal priority %d", priority3, priority1)
	}
}

func TestRefreshSchedulerRandomJitter(t *testing.T) {
	config := DefaultRefreshConfig()
	config.JitterPercent = 0.1
	scheduler := NewRefreshScheduler(&Store{}, config)

	base := 100 * time.Millisecond
	samples := make([]time.Duration, 100)
	for i := 0; i < 100; i++ {
		samples[i] = scheduler.randomJitter(base)
	}

	// Check the jitter range
	minJitter := time.Duration(float64(base) * 0.9)
	maxJitter := time.Duration(float64(base) * 1.1)
	for i, s := range samples {
		if s < minJitter || s > maxJitter {
			t.Fatalf("sample %d out of range: %v", i, s)
		}
	}

	// Check that it varies (is not a fixed value)
	allSame := true
	for i := 1; i < 100; i++ {
		if samples[i] != samples[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Fatal("jitter should vary")
	}
}

func TestRefreshSchedulerCalculateRetryTime(t *testing.T) {
	config := DefaultRefreshConfig()
	config.RetryBackoffBase = 1 * time.Second
	scheduler := NewRefreshScheduler(&Store{}, config)

	// First retry
	r1 := scheduler.calculateRetryTime(1)
	delay1 := r1.Sub(time.Now())
	// Allow some tolerance
	if delay1 < 800*time.Millisecond || delay1 > 1200*time.Millisecond {
		t.Fatalf("first retry delay = %v, want ~1s", delay1)
	}

	// Second retry (exponential backoff)
	r2 := scheduler.calculateRetryTime(2)
	delay2 := r2.Sub(time.Now())
	// 2s base + 10% jitter = up to 2.2s
	if delay2 < 1500*time.Millisecond || delay2 > 2500*time.Millisecond {
		t.Fatalf("second retry delay = %v, want ~2s (allowing for jitter)", delay2)
	}

	// Multiple retries (cap check)
	r5 := scheduler.calculateRetryTime(10)
	delay5 := r5.Sub(time.Now())
	if delay5 > 6*time.Minute {
		t.Fatalf("retry delay should be capped, got %v", delay5)
	}
}

func TestRefreshSchedulerScheduleImmediate(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	scheduler.ScheduleImmediate(acc)

	// Check that the task was created
	scheduler.tasksMu.RLock()
	task, ok := scheduler.tasks[1]
	scheduler.tasksMu.RUnlock()

	if !ok {
		t.Fatal("task not found after ScheduleImmediate")
	}

	task.mu.RLock()
	if task.Priority != 1000 {
		t.Fatalf("immediate task priority = %d, want 1000", task.Priority)
	}
	task.mu.RUnlock()
}

func TestRefreshSchedulerCancelTask(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	scheduler.Schedule(acc)

	// Cancel the task
	cancelled := scheduler.CancelTask(1)
	if !cancelled {
		t.Fatal("CancelTask should return true for pending task")
	}

	// Check the task status
	state, _, _ := scheduler.GetTaskStatus(1)
	if state != RefreshStateSuccess {
		t.Fatalf("cancelled task state = %d, want RefreshStateSuccess", state)
	}

	// Cancelling again should fail
	cancelled = scheduler.CancelTask(1)
	if cancelled {
		t.Fatal("CancelTask should return false for non-existent task")
	}
}

func TestRefreshSchedulerGetTaskStatus(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Nonexistent task
	state, attempts, err := scheduler.GetTaskStatus(999)
	if state != RefreshStateSuccess {
		t.Fatalf("non-existent task state = %d, want RefreshStateSuccess", state)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0", attempts)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	// Existing task
	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	scheduler.Schedule(acc)

	state, attempts, err = scheduler.GetTaskStatus(1)
	if state != RefreshStatePending {
		t.Fatalf("new task state = %d, want RefreshStatePending", state)
	}
}

func TestRefreshSchedulerScheduleBatch(t *testing.T) {
	config := DefaultRefreshConfig()
	config.BatchSize = 2
	config.MinInterval = 100 * time.Millisecond
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Batch scheduling
	accounts := make([]*Account, 5)
	for i := 0; i < 5; i++ {
		accounts[i] = newTestAccountWithExpiry(int64(i+1), 10*time.Minute)
	}

	scheduler.ScheduleBatch(accounts)

	// Wait for batch scheduling to finish (max delay = (5/2)*100ms = 200ms, plus some buffer)
	time.Sleep(400 * time.Millisecond)

	// Check that all tasks were created
	scheduler.tasksMu.RLock()
	taskCount := len(scheduler.tasks)
	scheduler.tasksMu.RUnlock()

	if taskCount != 5 {
		t.Fatalf("task count = %d, want 5", taskCount)
	}

	// Check the metrics
	if scheduler.metrics.TotalTasks.Load() != 5 {
		t.Fatalf("TotalTasks = %d, want 5", scheduler.metrics.TotalTasks.Load())
	}
}

func TestRefreshSchedulerPriorityQueue(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Create tasks with different priorities
	acc1 := newTestAccountWithExpiry(1, 1*time.Minute)  // medium priority (about to expire but not urgent)
	acc2 := newTestAccountWithExpiry(2, 30*time.Minute) // low priority
	acc3 := newTestAccountWithExpiry(3, 10*time.Second) // highest priority (very urgent)

	// Compute the expected priorities
	p1 := scheduler.calculatePriority(acc1) // 30 + 50 = 80 (1 minute < 2 minutes)
	p2 := scheduler.calculatePriority(acc2) // 30
	p3 := scheduler.calculatePriority(acc3) // 30 + 100 = 130 (10 seconds < 30 seconds)

	scheduler.Schedule(acc1)
	scheduler.Schedule(acc2)
	scheduler.Schedule(acc3)

	// Check the priority queue order
	task1 := scheduler.popFromQueue()
	task2 := scheduler.popFromQueue()
	task3 := scheduler.popFromQueue()

	// Verify priorities: p3 > p1 > p2
	t.Logf("Priorities: acc1=%d, acc2=%d, acc3=%d", p1, p2, p3)
	t.Logf("Popped order: task1=%d, task2=%d, task3=%d", task1.Account.DBID, task2.Account.DBID, task3.Account.DBID)

	// The highest priority should be popped first
	if task1 == nil || task1.Account.DBID != 3 {
		t.Fatalf("highest priority task should be popped first, got dbID=%d, want 3", task1.Account.DBID)
	}
	if task2 == nil || task2.Account.DBID != 1 {
		t.Fatalf("second priority task should be popped second, got dbID=%d, want 1", task2.Account.DBID)
	}
	if task3 == nil || task3.Account.DBID != 2 {
		t.Fatalf("lowest priority task should be popped last, got dbID=%d, want 2", task3.Account.DBID)
	}
}

func TestRefreshSchedulerMetrics(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	// Initial metrics
	metrics := scheduler.GetMetrics()
	if metrics.TotalTasks != 0 {
		t.Fatalf("initial TotalTasks = %d, want 0", metrics.TotalTasks)
	}

	// Add a task
	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	scheduler.Schedule(acc)

	metrics = scheduler.GetMetrics()
	if metrics.TotalTasks != 1 {
		t.Fatalf("after schedule TotalTasks = %d, want 1", metrics.TotalTasks)
	}
}

func TestRefreshSchedulerUpdateAvgDuration(t *testing.T) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)

	// First update
	scheduler.updateAvgDuration(100 * time.Millisecond)
	if scheduler.metrics.AvgDurationMs.Load() != 100 {
		t.Fatalf("avg duration = %d, want 100", scheduler.metrics.AvgDurationMs.Load())
	}

	// Second update (exponential moving average)
	scheduler.updateAvgDuration(200 * time.Millisecond)
	avg := scheduler.metrics.AvgDurationMs.Load()
	// (100*9 + 200) / 10 = 110
	if avg != 110 {
		t.Fatalf("avg duration = %d, want 110", avg)
	}
}

func BenchmarkRefreshSchedulerSchedule(b *testing.B) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)
	accounts := make([]*Account, b.N)
	for i := 0; i < b.N; i++ {
		accounts[i] = newTestAccountWithExpiry(int64(i+1), 10*time.Minute)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.Schedule(accounts[i])
	}
}

func BenchmarkRefreshSchedulerCalculateOptimalTime(b *testing.B) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)
	acc := newTestAccountWithExpiry(1, 10*time.Minute)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.calculateOptimalTime(acc)
	}
}

func BenchmarkRefreshSchedulerCalculatePriority(b *testing.B) {
	config := DefaultRefreshConfig()
	scheduler := NewRefreshScheduler(&Store{}, config)
	acc := newTestAccountWithExpiry(1, 10*time.Minute)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.calculatePriority(acc)
	}
}
