package auth

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// RefreshSchedulerIntegration provides refresh scheduler integration for Store.
type RefreshSchedulerIntegration struct {
	store     *Store
	scheduler *RefreshScheduler
	enabled   atomic.Bool
}

// EnableRefreshScheduler enables the smart refresh scheduler.
func (s *Store) EnableRefreshScheduler(config RefreshConfig) {
	if s == nil {
		return
	}

	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = DefaultRefreshConfig().MaxConcurrency
	}
	if config.MinInterval <= 0 {
		config.MinInterval = DefaultRefreshConfig().MinInterval
	}
	if config.PreExpireWindow <= 0 {
		config.PreExpireWindow = DefaultRefreshConfig().PreExpireWindow
	}
	if config.RetryBackoffBase <= 0 {
		config.RetryBackoffBase = DefaultRefreshConfig().RetryBackoffBase
	}
	if config.RetryMaxAttempts <= 0 {
		config.RetryMaxAttempts = DefaultRefreshConfig().RetryMaxAttempts
	}
	if config.BatchSize <= 0 {
		config.BatchSize = DefaultRefreshConfig().BatchSize
	}
	if config.JitterPercent < 0 {
		config.JitterPercent = DefaultRefreshConfig().JitterPercent
	}

	integration := &RefreshSchedulerIntegration{
		store:     s,
		scheduler: NewRefreshScheduler(s, config),
	}
	integration.enabled.Store(true)

	// Start the scheduler (its lifecycle is managed by scheduler.Stop(); do not defer cancel here).
	integration.scheduler.Start(context.Background())

	// Store it in the store.
	s.refreshScheduler.Store(integration)

	log.Printf("[Store] smart refresh scheduler enabled (concurrency: %d, pre-expire window: %v)",
		config.MaxConcurrency, config.PreExpireWindow)
}

// DisableRefreshScheduler disables the smart refresh scheduler.
func (s *Store) DisableRefreshScheduler() {
	if s == nil {
		return
	}

	if integration := s.refreshScheduler.Load(); integration != nil {
		integration.enabled.Store(false)
		integration.scheduler.Stop()
		s.refreshScheduler.Store(nil)
		log.Println("[Store] smart refresh scheduler disabled")
	}
}

// RefreshSchedulerEnabled reports whether the smart refresh scheduler is enabled.
func (s *Store) RefreshSchedulerEnabled() bool {
	if s == nil {
		return false
	}
	if integration := s.refreshScheduler.Load(); integration != nil {
		return integration.enabled.Load()
	}
	return false
}

// GetRefreshScheduler returns the refresh scheduler instance.
func (s *Store) GetRefreshScheduler() *RefreshScheduler {
	if s == nil {
		return nil
	}
	if integration := s.refreshScheduler.Load(); integration != nil {
		return integration.scheduler
	}
	return nil
}

// ScheduleAccountRefresh schedules a refresh for an account.
func (s *Store) ScheduleAccountRefresh(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	// Prefer the smart scheduler.
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		scheduler.Schedule(acc)
		return
	}

	// Fall back to the legacy approach: refresh immediately.
	s.startDBBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		if err := s.refreshAccount(ctx, acc); err != nil {
			log.Printf("[account %d] async refresh failed: %v", acc.DBID, err)
		}
	})
}

// ScheduleImmediateRefresh schedules an immediate refresh for an account.
func (s *Store) ScheduleImmediateRefresh(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		scheduler.ScheduleImmediate(acc)
		return
	}

	// Fall back to the legacy approach.
	s.ScheduleAccountRefresh(acc)
}

// GetRefreshMetrics returns the refresh metrics.
func (s *Store) GetRefreshMetrics() RefreshMetricsSnapshot {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.GetMetrics()
	}
	return RefreshMetricsSnapshot{}
}

// GetRefreshTaskStatus returns the refresh task status.
func (s *Store) GetRefreshTaskStatus(dbID int64) (RefreshState, int, error) {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.GetTaskStatus(dbID)
	}
	return RefreshStateSuccess, 0, nil
}

// CancelRefreshTask cancels a refresh task.
func (s *Store) CancelRefreshTask(dbID int64) bool {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.CancelTask(dbID)
	}
	return false
}

// InitRefreshSchedulerFromEnv initializes the refresh scheduler from environment variables.
func (s *Store) InitRefreshSchedulerFromEnv() {
	if s == nil {
		return
	}

	// Check whether it is enabled.
	if !refreshSchedulerEnabledFromEnv() {
		return
	}

	config := DefaultRefreshConfig()

	// Read the configuration from environment variables.
	if v := getEnvInt("REFRESH_MAX_CONCURRENCY", 0); v > 0 {
		config.MaxConcurrency = v
	}
	if v := getEnvDuration("REFRESH_PRE_EXPIRE_WINDOW", 0); v > 0 {
		config.PreExpireWindow = v
	}
	if v := getEnvDuration("REFRESH_MIN_INTERVAL", 0); v > 0 {
		config.MinInterval = v
	}
	if v := getEnvDuration("REFRESH_RETRY_BACKOFF_BASE", 0); v > 0 {
		config.RetryBackoffBase = v
	}
	if v := getEnvInt("REFRESH_RETRY_MAX_ATTEMPTS", 0); v > 0 {
		config.RetryMaxAttempts = v
	}
	if v := getEnvFloat("REFRESH_JITTER_PERCENT", -1); v >= 0 {
		config.JitterPercent = v
	}
	if v := getEnvInt("REFRESH_BATCH_SIZE", 0); v > 0 {
		config.BatchSize = v
	}
	config.EnableBatchOptimize = truthyEnv(refreshGetEnv("REFRESH_BATCH_OPTIMIZE", "true"))

	s.EnableRefreshScheduler(config)
}

// refreshSchedulerEnabledFromEnv checks whether the refresh scheduler is enabled by environment variable.
func refreshSchedulerEnabledFromEnv() bool {
	for _, key := range []string{"REFRESH_SCHEDULER_ENABLED", "SMART_REFRESH_ENABLED"} {
		if truthyEnv(refreshGetEnv(key, "")) {
			return true
		}
	}
	return false
}

// refreshGetEnv gets an environment variable.
func refreshGetEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// getEnvInt gets an integer environment variable.
func getEnvInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return defaultValue
	}
	return i
}

// getEnvDuration gets a duration environment variable.
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultValue
	}
	return d
}

// getEnvFloat gets a float environment variable.
func getEnvFloat(key string, defaultValue float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultValue
	}
	return f
}
