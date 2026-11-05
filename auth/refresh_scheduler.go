package auth

import (
	"container/heap"
	"context"
	"log"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// RefreshState is the state of a refresh task.
type RefreshState int

const (
	RefreshStatePending  RefreshState = iota // waiting to run
	RefreshStateRunning                      // running
	RefreshStateSuccess                      // succeeded
	RefreshStateFailed                       // failed
	RefreshStateRetrying                     // retrying
)

// RefreshTask is a refresh task.
type RefreshTask struct {
	Account     *Account
	ScheduledAt time.Time    // scheduled execution time
	Priority    int          // priority (a larger value means higher priority)
	Attempts    int          // number of attempts so far
	State       RefreshState // current state
	LastError   error        // last error
	NextRetryAt time.Time    // next retry time
	mu          sync.RWMutex // task-level lock
	heapIndex   int          // index in the heap, used by heap.Remove
}

// RefreshMetrics holds refresh metrics (for monitoring).
type RefreshMetrics struct {
	TotalTasks    atomic.Int64 // total tasks
	PendingTasks  atomic.Int64 // pending tasks
	RunningTasks  atomic.Int64 // running tasks
	SuccessCount  atomic.Int64 // success count
	FailedCount   atomic.Int64 // failure count
	RetryCount    atomic.Int64 // retry count
	AvgDurationMs atomic.Int64 // average duration (ms)
	LastRefreshAt atomic.Int64 // last refresh timestamp
}

// RefreshConfig is the refresh scheduler configuration.
type RefreshConfig struct {
	MaxConcurrency      int           // maximum concurrent refreshes
	MinInterval         time.Duration // minimum refresh interval (avoids refresh storms)
	PreExpireWindow     time.Duration // early refresh window (how long before expiry refreshes start)
	RetryBackoffBase    time.Duration // retry backoff base
	RetryMaxAttempts    int           // maximum number of retries
	JitterPercent       float64       // jitter percentage (0-1)
	EnableBatchOptimize bool          // enable batch optimization
	BatchSize           int           // batch size
}

// DefaultRefreshConfig returns the default configuration.
func DefaultRefreshConfig() RefreshConfig {
	return RefreshConfig{
		MaxConcurrency:      10,
		MinInterval:         100 * time.Millisecond,
		PreExpireWindow:     5 * time.Minute,
		RetryBackoffBase:    1 * time.Second,
		RetryMaxAttempts:    3,
		JitterPercent:       0.1,
		EnableBatchOptimize: true,
		BatchSize:           5,
	}
}

// taskPriorityQueue is the task priority queue (a min-heap: the earlier the ScheduledAt, the higher the priority).
type taskPriorityQueue []*RefreshTask

func (pq taskPriorityQueue) Len() int { return len(pq) }

func (pq taskPriorityQueue) Less(i, j int) bool {
	// Higher priority first.
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority > pq[j].Priority
	}
	// Same priority: order by scheduled time.
	return pq[i].ScheduledAt.Before(pq[j].ScheduledAt)
}

func (pq taskPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].heapIndex = i
	pq[j].heapIndex = j
}

func (pq *taskPriorityQueue) Push(x interface{}) {
	task := x.(*RefreshTask)
	task.heapIndex = len(*pq)
	*pq = append(*pq, task)
}

func (pq *taskPriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // avoid a memory leak
	x.heapIndex = -1
	*pq = old[0 : n-1]
	return x
}

// RefreshScheduler is the smart refresh scheduler.
type RefreshScheduler struct {
	config    RefreshConfig
	store     *Store
	queue     chan *RefreshTask
	semaphore chan struct{}
	pq        *taskPriorityQueue
	pqMu      sync.Mutex
	tasks     map[int64]*RefreshTask // dbID -> task
	tasksMu   sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	metrics   *RefreshMetrics
	lastBatch time.Time
	batchMu   sync.Mutex
}

// NewRefreshScheduler creates a refresh scheduler.
func NewRefreshScheduler(store *Store, config RefreshConfig) *RefreshScheduler {
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = 10
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 5
	}

	ctx, cancel := context.WithCancel(context.Background())
	pq := &taskPriorityQueue{}
	heap.Init(pq)

	return &RefreshScheduler{
		config:    config,
		store:     store,
		queue:     make(chan *RefreshTask, 1000),
		semaphore: make(chan struct{}, config.MaxConcurrency),
		pq:        pq,
		tasks:     make(map[int64]*RefreshTask),
		ctx:       ctx,
		cancel:    cancel,
		metrics:   &RefreshMetrics{},
		lastBatch: time.Now(),
	}
}

// Schedule schedules a refresh for one account.
func (s *RefreshScheduler) Schedule(account *Account) {
	if account == nil {
		return
	}

	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	// Check whether there is already a pending task.
	if existing, ok := s.tasks[account.DBID]; ok {
		existing.mu.Lock()
		// A task can only be rescheduled when it is not running.
		if existing.State != RefreshStateRunning {
			existing.State = RefreshStatePending
			existing.ScheduledAt = s.calculateOptimalTime(account)
			existing.Priority = s.calculatePriority(account)
			existing.Attempts = 0
			existing.LastError = nil
			existing.mu.Unlock()
			// The task is already in the heap; pushToQueue will use heap.Fix.
			s.pushToQueue(existing)
		} else {
			existing.mu.Unlock()
		}
		return
	}

	// Create a new task.
	task := &RefreshTask{
		Account:     account,
		ScheduledAt: s.calculateOptimalTime(account),
		Priority:    s.calculatePriority(account),
		State:       RefreshStatePending,
		Attempts:    0,
		heapIndex:   -1, // not in the heap; -1 avoids clashing with the valid index 0
	}

	s.tasks[account.DBID] = task
	s.metrics.TotalTasks.Add(1)
	s.metrics.PendingTasks.Add(1)

	s.pushToQueue(task)
}

// ScheduleImmediate schedules an immediate refresh (for urgent refreshes).
func (s *RefreshScheduler) ScheduleImmediate(account *Account) {
	if account == nil {
		return
	}

	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	task := &RefreshTask{
		Account:     account,
		ScheduledAt: time.Now(),
		Priority:    1000, // highest priority
		State:       RefreshStatePending,
		Attempts:    0,
		heapIndex:   -1,
	}

	// If there is already a task, remove the old one from the heap.
	if existing, ok := s.tasks[account.DBID]; ok {
		existing.mu.Lock()
		existing.State = RefreshStateSuccess // mark as done to avoid running it twice
		existing.mu.Unlock()
		s.removeFromQueue(existing)
		s.metrics.PendingTasks.Add(-1)
	} else {
		// Only a new task increments TotalTasks.
		s.metrics.TotalTasks.Add(1)
		s.metrics.PendingTasks.Add(1)
	}

	s.tasks[account.DBID] = task

	// Put it directly on the queue, bypassing the priority queue.
	select {
	case s.queue <- task:
		// ScheduleImmediate enqueues directly and is not in pq, so decrement PendingTasks.
		s.metrics.PendingTasks.Add(-1)
	default:
		log.Printf("[RefreshScheduler] queue full, task %d dropped", account.DBID)
	}
}

// calculateOptimalTime calculates the best refresh time.
func (s *RefreshScheduler) calculateOptimalTime(account *Account) time.Time {
	account.mu.RLock()
	expiresAt := account.ExpiresAt
	account.mu.RUnlock()

	// Based on the expiry time.
	var targetTime time.Time
	if expiresAt.IsZero() {
		targetTime = time.Now().Add(30 * time.Second)
	} else {
		timeUntilExpire := time.Until(expiresAt)
		if timeUntilExpire < s.config.PreExpireWindow {
			// About to expire; refresh immediately.
			targetTime = time.Now().Add(s.randomJitter(100 * time.Millisecond))
		} else {
			// Refresh PreExpireWindow before expiry.
			targetTime = expiresAt.Add(-s.config.PreExpireWindow)
			// Add jitter to avoid refreshing everything at once.
			targetTime = targetTime.Add(s.randomJitter(s.config.MinInterval))
		}
	}

	// Make sure it is not earlier than the current time.
	if targetTime.Before(time.Now()) {
		targetTime = time.Now().Add(s.randomJitter(100 * time.Millisecond))
	}

	return targetTime
}

// calculatePriority calculates the task priority.
func (s *RefreshScheduler) calculatePriority(account *Account) int {
	account.mu.RLock()
	expiresAt := account.ExpiresAt
	tier := account.HealthTier
	account.mu.RUnlock()

	priority := 0

	// Base priority: health tier.
	switch tier {
	case HealthTierHealthy:
		priority += 30
	case HealthTierWarm:
		priority += 20
	case HealthTierRisky:
		priority += 10
	}

	// Urgency: based on the remaining time.
	if !expiresAt.IsZero() {
		timeUntilExpire := time.Until(expiresAt)
		switch {
		case timeUntilExpire < 30*time.Second:
			priority += 100 // very urgent
		case timeUntilExpire < 2*time.Minute:
			priority += 50
		case timeUntilExpire < 5*time.Minute:
			priority += 20
		}
	}

	return priority
}

// randomJitter generates random jitter.
func (s *RefreshScheduler) randomJitter(base time.Duration) time.Duration {
	if s.config.JitterPercent <= 0 {
		return base
	}
	jitter := float64(base) * s.config.JitterPercent * (2*rand.Float64() - 1)
	return base + time.Duration(jitter)
}

// pushToQueue pushes onto the priority queue.
func (s *RefreshScheduler) pushToQueue(task *RefreshTask) {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	// If the task is already in the heap and its index is valid, use heap.Fix to update its position.
	task.mu.RLock()
	idx := task.heapIndex
	task.mu.RUnlock()

	if idx >= 0 && idx < s.pq.Len() {
		heap.Fix(s.pq, idx)
	} else {
		task.mu.Lock()
		task.heapIndex = -1 // reset the index
		task.mu.Unlock()
		heap.Push(s.pq, task)
	}
}

// removeFromQueue removes a task from the priority queue.
func (s *RefreshScheduler) removeFromQueue(task *RefreshTask) {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	task.mu.RLock()
	idx := task.heapIndex
	task.mu.RUnlock()

	if idx >= 0 && idx < s.pq.Len() {
		heap.Remove(s.pq, idx)
	}
}

// popFromQueue pops from the priority queue.
func (s *RefreshScheduler) popFromQueue() *RefreshTask {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	if s.pq.Len() == 0 {
		return nil
	}
	return heap.Pop(s.pq).(*RefreshTask)
}

// Start starts the scheduler.
func (s *RefreshScheduler) Start(ctx context.Context) {
	// Derive the internal context from the ctx passed in.
	s.ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(3)

	// Scheduling loop: from the priority queue to the execution queue.
	go s.scheduleLoop()

	// Execution loop: processes the execution queue.
	go s.executeLoop()

	// Retry loop: handles retries of failed tasks.
	go s.retryLoop()

	log.Printf("[RefreshScheduler] scheduler started, max concurrency: %d", s.config.MaxConcurrency)
}

// Stop stops the scheduler.
func (s *RefreshScheduler) Stop() {
	s.cancel()
	s.wg.Wait()
	close(s.queue)
	log.Printf("[RefreshScheduler] scheduler stopped")
}

// scheduleLoop is the scheduling loop: it moves due tasks from the priority queue to the execution queue.
func (s *RefreshScheduler) scheduleLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processScheduledTasks()
		}
	}
}

// processScheduledTasks processes scheduled tasks.
func (s *RefreshScheduler) processScheduledTasks() {
	now := time.Now()

	for {
		task := s.peekQueue()
		if task == nil {
			break
		}

		task.mu.RLock()
		scheduledAt := task.ScheduledAt
		state := task.State
		task.mu.RUnlock()

		// If the task's execution time has not arrived yet, stop processing.
		if scheduledAt.After(now) {
			break
		}

		// If the task state is not Pending, pop it and continue with the next one.
		if state != RefreshStatePending {
			task = s.popFromQueue()
			if task == nil {
				break
			}
			// Continue the loop with the next task.
			continue
		}

		// Pop the task.
		task = s.popFromQueue()
		if task == nil {
			break
		}

		// Batch optimization: check whether this should run as a batch.
		// Note: this function currently only updates the lastBatch time; the actual batching strategy is still to be implemented.
		if s.config.EnableBatchOptimize && s.shouldBatch(task) {
			s.batchMu.Lock()
			s.lastBatch = time.Now()
			s.batchMu.Unlock()
		}

		// Send to the execution queue.
		select {
		case s.queue <- task:
			s.metrics.PendingTasks.Add(-1)
		default:
			// Queue is full; push back onto the priority queue.
			s.pushToQueue(task)
			return
		}
	}
}

// peekQueue looks at the head of the queue (without removing it).
func (s *RefreshScheduler) peekQueue() *RefreshTask {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	if s.pq.Len() == 0 {
		return nil
	}
	return (*s.pq)[0]
}

// shouldBatch reports whether this should run as a batch.
func (s *RefreshScheduler) shouldBatch(task *RefreshTask) bool {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()

	// Too short since the last batch run; suggest waiting.
	if time.Since(s.lastBatch) < s.config.MinInterval {
		return true
	}
	return false
}

// executeLoop is the execution loop.
func (s *RefreshScheduler) executeLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case task, ok := <-s.queue:
			if !ok {
				return
			}
			s.executeTask(task)
		}
	}
}

// executeTask executes a task.
func (s *RefreshScheduler) executeTask(task *RefreshTask) {
	// Acquire the semaphore.
	select {
	case s.semaphore <- struct{}{}:
	case <-s.ctx.Done():
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.semaphore }()

		start := time.Now()
		s.metrics.RunningTasks.Add(1)

		task.mu.Lock()
		task.State = RefreshStateRunning
		task.mu.Unlock()

		// Run the refresh, with timeout control.
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		err := s.store.refreshAccount(ctx, task.Account)
		cancel()

		duration := time.Since(start)
		s.updateAvgDuration(duration)
		s.metrics.RunningTasks.Add(-1)
		s.metrics.LastRefreshAt.Store(time.Now().Unix())

		task.mu.Lock()
		if err != nil {
			task.State = RefreshStateFailed
			task.LastError = err
			task.Attempts++
			s.metrics.FailedCount.Add(1)

			// Decide whether it can be retried.
			if task.Attempts < s.config.RetryMaxAttempts && !isNonRetryable(err) {
				task.State = RefreshStateRetrying
				task.NextRetryAt = s.calculateRetryTime(task.Attempts)
				log.Printf("[RefreshScheduler] account %d refresh failed, retrying in %v: %v",
					task.Account.DBID, task.NextRetryAt.Sub(time.Now()), err)
			} else {
				log.Printf("[RefreshScheduler] account %d final refresh failure: %v", task.Account.DBID, err)
			}
		} else {
			task.State = RefreshStateSuccess
			s.metrics.SuccessCount.Add(1)
			log.Printf("[RefreshScheduler] account %d refreshed successfully, took %v", task.Account.DBID, duration)
		}
		task.mu.Unlock()

		// Clean up completed tasks.
		if task.State == RefreshStateSuccess || (task.State == RefreshStateFailed && task.Attempts >= s.config.RetryMaxAttempts) {
			s.tasksMu.Lock()
			delete(s.tasks, task.Account.DBID)
			s.tasksMu.Unlock()
		}
	}()
}

// calculateRetryTime calculates the retry time.
func (s *RefreshScheduler) calculateRetryTime(attempts int) time.Time {
	// Exponential backoff: base * 2^attempts + jitter
	backoff := s.config.RetryBackoffBase * time.Duration(math.Pow(2, float64(attempts-1)))
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	return time.Now().Add(backoff + s.randomJitter(backoff/10))
}

// updateAvgDuration updates the average duration.
func (s *RefreshScheduler) updateAvgDuration(d time.Duration) {
	current := s.metrics.AvgDurationMs.Load()
	newMs := d.Milliseconds()
	if current == 0 {
		s.metrics.AvgDurationMs.Store(newMs)
	} else {
		// Exponential moving average.
		avg := (current*9 + newMs) / 10
		s.metrics.AvgDurationMs.Store(avg)
	}
}

// retryLoop is the retry loop.
func (s *RefreshScheduler) retryLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processRetries()
		}
	}
}

// processRetries processes retries.
func (s *RefreshScheduler) processRetries() {
	now := time.Now()

	s.tasksMu.RLock()
	tasks := make([]*RefreshTask, 0)
	for _, task := range s.tasks {
		task.mu.RLock()
		if task.State == RefreshStateRetrying && task.NextRetryAt.Before(now) {
			tasks = append(tasks, task)
		}
		task.mu.RUnlock()
	}
	s.tasksMu.RUnlock()

	for _, task := range tasks {
		task.mu.Lock()
		task.State = RefreshStatePending
		task.ScheduledAt = now
		task.mu.Unlock()

		s.metrics.RetryCount.Add(1)
		s.metrics.PendingTasks.Add(1) // re-enqueued retry task: increment PendingTasks
		s.pushToQueue(task)
		log.Printf("[RefreshScheduler] account %d starting retry %d", task.Account.DBID, task.Attempts)
	}
}

// GetMetrics returns the current metrics.
func (s *RefreshScheduler) GetMetrics() RefreshMetricsSnapshot {
	return RefreshMetricsSnapshot{
		TotalTasks:    s.metrics.TotalTasks.Load(),
		PendingTasks:  s.metrics.PendingTasks.Load(),
		RunningTasks:  s.metrics.RunningTasks.Load(),
		SuccessCount:  s.metrics.SuccessCount.Load(),
		FailedCount:   s.metrics.FailedCount.Load(),
		RetryCount:    s.metrics.RetryCount.Load(),
		AvgDurationMs: s.metrics.AvgDurationMs.Load(),
		LastRefreshAt: time.Unix(s.metrics.LastRefreshAt.Load(), 0),
		QueueSize:     len(s.queue),
	}
}

// RefreshMetricsSnapshot is a snapshot of the refresh metrics.
type RefreshMetricsSnapshot struct {
	TotalTasks    int64     `json:"total_tasks"`
	PendingTasks  int64     `json:"pending_tasks"`
	RunningTasks  int64     `json:"running_tasks"`
	SuccessCount  int64     `json:"success_count"`
	FailedCount   int64     `json:"failed_count"`
	RetryCount    int64     `json:"retry_count"`
	AvgDurationMs int64     `json:"avg_duration_ms"`
	LastRefreshAt time.Time `json:"last_refresh_at"`
	QueueSize     int       `json:"queue_size"`
}

// ScheduleBatch schedules refreshes in batch.
func (s *RefreshScheduler) ScheduleBatch(accounts []*Account) {
	if len(accounts) == 0 {
		return
	}

	// Add jitter to avoid refreshing everything at once.
	batchSize := s.config.BatchSize
	if batchSize <= 0 {
		batchSize = 5
	}

	for i, acc := range accounts {
		// Add delays in batches.
		batchDelay := time.Duration(i/batchSize) * s.config.MinInterval
		if batchDelay > 0 {
			account := acc // local copy, so the closure does not capture the loop variable
			time.AfterFunc(batchDelay, func() {
				s.Schedule(account)
			})
		} else {
			s.Schedule(acc)
		}
	}

	log.Printf("[RefreshScheduler] batch scheduled refreshes for %d accounts", len(accounts))
}

// CancelTask cancels the refresh task for the given account.
func (s *RefreshScheduler) CancelTask(dbID int64) bool {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	if task, ok := s.tasks[dbID]; ok {
		task.mu.Lock()
		if task.State == RefreshStatePending || task.State == RefreshStateRetrying {
			task.State = RefreshStateSuccess // mark as done, skip execution
			task.mu.Unlock()
			// Remove the task from the heap.
			s.removeFromQueue(task)
			delete(s.tasks, dbID)
			return true
		}
		task.mu.Unlock()
	}
	return false
}

// GetTaskStatus returns the task status.
func (s *RefreshScheduler) GetTaskStatus(dbID int64) (RefreshState, int, error) {
	s.tasksMu.RLock()
	task, ok := s.tasks[dbID]
	s.tasksMu.RUnlock()

	if !ok {
		return RefreshStateSuccess, 0, nil // no task means it is already done
	}

	task.mu.RLock()
	state := task.State
	attempts := task.Attempts
	lastErr := task.LastError
	task.mu.RUnlock()

	return state, attempts, lastErr
}
