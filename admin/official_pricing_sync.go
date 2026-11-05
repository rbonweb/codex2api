package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const officialPricingSyncTimeout = 3 * time.Minute

// officialPricingSyncSlot ensures a manual sync and the background poller never
// fetch/write pricing at the same time. Waiting is context-aware, so shutdown and
// cancelled HTTP requests do not get stuck behind a slow official site.
var officialPricingSyncSlot = func() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ch
}()

type officialPricingSyncConfigResponse struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int     `json:"interval_minutes"`
	IncludeOpenAI   bool    `json:"include_openai"`
	LastAttemptAt   *string `json:"last_attempt_at,omitempty"`
	LastSuccessAt   *string `json:"last_success_at,omitempty"`
	LastError       string  `json:"last_error,omitempty"`
	LastWarning     string  `json:"last_warning,omitempty"`
}

func officialPricingConfigResponse(cfg *database.OfficialPricingSyncConfig) officialPricingSyncConfigResponse {
	response := officialPricingSyncConfigResponse{
		IntervalMinutes: database.DefaultOfficialPricingSyncIntervalMinutes,
		IncludeOpenAI:   true,
	}
	if cfg == nil {
		return response
	}
	response.Enabled = cfg.Enabled
	response.IntervalMinutes = cfg.IntervalMinutes
	response.IncludeOpenAI = cfg.IncludeOpenAI
	response.LastError = cfg.LastError
	response.LastWarning = cfg.LastWarning
	if cfg.LastAttemptAt.Valid {
		value := cfg.LastAttemptAt.Time.UTC().Format(time.RFC3339)
		response.LastAttemptAt = &value
	}
	if cfg.LastSuccessAt.Valid {
		value := cfg.LastSuccessAt.Time.UTC().Format(time.RFC3339)
		response.LastSuccessAt = &value
	}
	return response
}

func (h *Handler) officialPricingModelIDs(ctx context.Context) []string {
	models := h.codexPricingModelIDs(ctx)
	return models
}

func (h *Handler) runOfficialPricingSync(ctx context.Context, includeOpenAI bool) (*proxy.OfficialPricingSyncResult, error) {
	return h.runOfficialPricingSyncWithWarnings(ctx, proxy.OfficialPricingSyncOptions{
		IncludeOpenAI: includeOpenAI,
	}, nil)
}

func (h *Handler) runOfficialPricingSyncWithWarnings(ctx context.Context, options proxy.OfficialPricingSyncOptions, warnings []string) (*proxy.OfficialPricingSyncResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-officialPricingSyncSlot:
	}
	defer func() { officialPricingSyncSlot <- struct{}{} }()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	attemptedAt := time.Now().UTC()
	options.Models = h.officialPricingModelIDs(ctx)
	result, syncErr := proxy.SyncOfficialModelPricing(ctx, h.db, proxyURL, options)
	recordCtx, recordCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recordCancel()
	if result != nil {
		result.Warnings = append(result.Warnings, warnings...)
		if len(result.Missing) > 0 {
			result.Notices = append(result.Notices, "Using built-in default pricing (custom pricing takes priority): "+strings.Join(result.Missing, ", "))
		}
		warnings = result.Warnings
	}
	if recordErr := h.db.RecordOfficialPricingSyncResult(recordCtx, attemptedAt, syncErr, warnings); recordErr != nil {
		if syncErr != nil {
			return result, fmt.Errorf("%v; failed to record sync status: %w", syncErr, recordErr)
		}
		return result, fmt.Errorf("official prices synced, but failed to record status: %w", recordErr)
	}
	return result, syncErr
}

type updateOfficialPricingSyncConfigRequest struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	IncludeOpenAI   bool `json:"include_openai"`
}

func (h *Handler) UpdateOfficialPricingSyncConfig(c *gin.Context) {
	var req updateOfficialPricingSyncConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IntervalMinutes < database.MinOfficialPricingSyncIntervalMinutes || req.IntervalMinutes > database.MaxOfficialPricingSyncIntervalMinutes {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("Poll interval must be between %d and %d minutes", database.MinOfficialPricingSyncIntervalMinutes, database.MaxOfficialPricingSyncIntervalMinutes))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.UpdateOfficialPricingSyncConfig(ctx, database.OfficialPricingSyncConfig{
		Enabled:         req.Enabled,
		IntervalMinutes: req.IntervalMinutes,
		IncludeOpenAI:   req.IncludeOpenAI,
	})
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, officialPricingConfigResponse(cfg))
}

type syncOfficialPricingRequest struct {
	IncludeOpenAI *bool `json:"include_openai"`
}

func (h *Handler) SyncOfficialPricingNow(c *gin.Context) {
	var req syncOfficialPricingRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	includeOpenAI := true
	if req.IncludeOpenAI != nil {
		includeOpenAI = *req.IncludeOpenAI
	}
	if !includeOpenAI {
		writeError(c, http.StatusBadRequest, "Select at least one official pricing source")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), officialPricingSyncTimeout)
	defer cancel()
	result, err := h.runOfficialPricingSync(ctx, includeOpenAI)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// StartOfficialPricingSync starts the optional official price poller. It is off
// by default and performs no network access until the administrator enables it.
func (h *Handler) StartOfficialPricingSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		check := func() {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cfg, err := h.db.GetOfficialPricingSyncConfig(readCtx)
			cancel()
			if err != nil {
				log.Printf("failed to read official pricing poll settings: %v", err)
				return
			}
			if cfg == nil || !cfg.Enabled || !cfg.IncludeOpenAI {
				return
			}
			lastRun := time.Time{}
			if cfg.LastAttemptAt.Valid {
				lastRun = cfg.LastAttemptAt.Time
			}
			if !lastRun.IsZero() && time.Since(lastRun) < time.Duration(cfg.IntervalMinutes)*time.Minute {
				return
			}

			syncCtx, syncCancel := context.WithTimeout(ctx, automaticModelDiscoveryTimeout+officialPricingSyncTimeout)
			result, syncErr := h.runAutomaticModelPricingSync(syncCtx, cfg)
			syncCancel()
			if syncErr != nil {
				log.Printf("official model price auto-sync failed: %v", syncErr)
				return
			}
			if result == nil {
				log.Printf("official model price auto-sync returned an empty result")
				return
			}
			warning := ""
			if result != nil && len(result.Warnings) > 0 {
				warning = ", warning: " + strings.Join(result.Warnings, "; ")
			}
			log.Printf("official model price auto-sync completed: fetched=%d applied=%d skipped=%d%s", result.Fetched, result.Applied, result.Skipped, warning)
		}

		check()
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
