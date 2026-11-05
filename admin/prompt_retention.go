package admin

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Prompt review log retention: on a schedule, expired logs are purged in batches by day count (skipping CY-linked rows),
// and the manual "clear logs" action goes through the same batch path, so it is no longer limited by the request timeout.

const (
	promptLogRetentionCheckInterval = time.Hour
	promptLogRetentionRunTimeout    = 30 * time.Minute
	promptLogPurgeBatchSize         = database.DefaultPromptLogPurgeBatch
	promptLogPurgeBatchPause        = 200 * time.Millisecond
)

// promptLogPurgeRunning ensures the scheduled purge and the manual purge never run at the same time (both are long-running batched deletes).
var promptLogPurgeRunning int32

type promptLogRetentionResponse struct {
	RetentionDays      int     `json:"retention_days"`
	Running            bool    `json:"running"`
	LastRunAt          *string `json:"last_run_at,omitempty"`
	LastDeletedLogs    int64   `json:"last_deleted_logs"`
	LastDeletedEvents  int64   `json:"last_deleted_events"`
	LastDeletedSources int64   `json:"last_deleted_sources"`
	LastDurationMs     int64   `json:"last_duration_ms"`
	LastError          string  `json:"last_error,omitempty"`
}

func promptLogRetentionResponseFrom(cfg *database.PromptLogRetentionConfig) promptLogRetentionResponse {
	resp := promptLogRetentionResponse{RetentionDays: database.DefaultPromptLogRetentionDays, Running: atomic.LoadInt32(&promptLogPurgeRunning) == 1}
	if cfg == nil {
		return resp
	}
	resp.RetentionDays = cfg.RetentionDays
	resp.LastDeletedLogs = cfg.LastDeletedLogs
	resp.LastDeletedEvents = cfg.LastDeletedEvents
	resp.LastDeletedSources = cfg.LastDeletedSources
	resp.LastDurationMs = cfg.LastDurationMs
	resp.LastError = cfg.LastError
	if cfg.LastRunAt.Valid {
		value := cfg.LastRunAt.Time.UTC().Format(time.RFC3339)
		resp.LastRunAt = &value
	}
	return resp
}

// GetPromptLogRetention returns the retention days and the last purge statistics (GET /api/admin/prompt-filter/retention).
func (h *Handler) GetPromptLogRetention(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.GetPromptLogRetentionConfig(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, promptLogRetentionResponseFrom(cfg))
}

type updatePromptLogRetentionRequest struct {
	RetentionDays int `json:"retention_days"`
}

// UpdatePromptLogRetention sets the retention days (0 = automatic purge off, maximum 365).
func (h *Handler) UpdatePromptLogRetention(c *gin.Context) {
	var req updatePromptLogRetentionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RetentionDays < 0 || req.RetentionDays > database.MaxPromptLogRetentionDays {
		writeError(c, http.StatusBadRequest, "Retention days must be between 0 and 365 (0 means automatic purge is off)")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.UpdatePromptLogRetentionDays(ctx, req.RetentionDays)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, promptLogRetentionResponseFrom(cfg))
}

// RunPromptLogRetentionNow runs one purge in the background right away, using the current retention days; if a purge is already running it returns 409.
func (h *Handler) RunPromptLogRetentionNow(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	cfg, err := h.db.GetPromptLogRetentionConfig(ctx)
	cancel()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if cfg.RetentionDays <= 0 {
		writeError(c, http.StatusBadRequest, "Retention days is 0 (automatic purge is off); set the retention days first")
		return
	}
	if !h.startPromptLogPurge(func(ctx context.Context) {
		h.runPromptLogRetention(ctx, cfg.RetentionDays)
	}) {
		writeError(c, http.StatusConflict, "A purge task is already running")
		return
	}
	c.JSON(http.StatusOK, gin.H{"started": true, "retention_days": cfg.RetentionDays})
}

// startPromptLogPurge starts one batched purge in the background; returns false if it is already occupied.
func (h *Handler) startPromptLogPurge(run func(ctx context.Context)) bool {
	if !atomic.CompareAndSwapInt32(&promptLogPurgeRunning, 0, 1) {
		return false
	}
	go func() {
		defer atomic.StoreInt32(&promptLogPurgeRunning, 0)
		ctx, cancel := context.WithTimeout(context.Background(), promptLogRetentionRunTimeout)
		defer cancel()
		run(ctx)
	}()
	return true
}

// runPromptLogRetention runs one retention purge and records the result.
func (h *Handler) runPromptLogRetention(ctx context.Context, days int) {
	started := time.Now()
	cutoff := started.Add(-time.Duration(days) * 24 * time.Hour)
	result, err := h.db.PurgeExpiredPromptLogs(ctx, cutoff, promptLogPurgeBatchSize, promptLogPurgeBatchPause)
	duration := time.Since(started)
	recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if recordErr := h.db.RecordPromptLogRetentionRun(recordCtx, started, result, duration, err); recordErr != nil {
		log.Printf("[prompt-retention] failed to record purge result: %v", recordErr)
	}
	if err != nil {
		log.Printf("[prompt-retention] purge failed (retention %d days): %v; deleted logs=%d events=%d sources=%d", days, err, result.Logs, result.Events, result.Sources)
		return
	}
	if result.Logs+result.Events+result.Sources > 0 || result.Interrupted {
		log.Printf("[prompt-retention] purge completed (retention %d days, %s): logs=%d events=%d sources=%d batches=%d interrupted=%v",
			days, duration.Round(time.Millisecond), result.Logs, result.Events, result.Sources, result.Batches, result.Interrupted)
	}
}

// StartPromptLogRetention starts the retention purge once per hour; when the retention days is 0 it does nothing.
func (h *Handler) StartPromptLogRetention(ctx context.Context) {
	if h == nil || h.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(promptLogRetentionCheckInterval)
		defer ticker.Stop()
		check := func() {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cfg, err := h.db.GetPromptLogRetentionConfig(readCtx)
			cancel()
			if err != nil {
				log.Printf("[prompt-retention] failed to read retention settings: %v", err)
				return
			}
			if cfg == nil || cfg.RetentionDays <= 0 {
				return
			}
			if cfg.LastRunAt.Valid && time.Since(cfg.LastRunAt.Time) < promptLogRetentionCheckInterval/2 {
				return
			}
			days := cfg.RetentionDays
			h.startPromptLogPurge(func(runCtx context.Context) {
				merged, cancelMerged := context.WithCancel(runCtx)
				defer cancelMerged()
				go func() {
					select {
					case <-ctx.Done():
						cancelMerged()
					case <-merged.Done():
					}
				}()
				h.runPromptLogRetention(merged, days)
			})
		}
		// Wait briefly after startup before the first round, to avoid racing other startup tasks for the write lock.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
			check()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}
