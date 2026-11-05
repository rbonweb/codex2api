package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type modelTraceBankStatus struct {
	Active   proxy.ModelTraceBankInfo  `json:"active"`
	Embedded proxy.ModelTraceBankInfo  `json:"embedded"`
	Override *proxy.ModelTraceBankInfo `json:"override,omitempty"`
	// OverrideStale means a release shipped a bank at least as new as the
	// installed override, so the embedded copy is used instead.
	OverrideStale bool   `json:"override_stale,omitempty"`
	OverrideError string `json:"override_error,omitempty"`
}

type modelTraceBankUpdateResponse struct {
	modelTraceBankStatus
	Updated       bool     `json:"updated"`
	Message       string   `json:"message"`
	AddedModels   []string `json:"added_models,omitempty"`
	RemovedModels []string `json:"removed_models,omitempty"`
}

// modelTraceBankNewer reports whether candidate was trained after base. Banks
// without a timestamp never displace one that has it.
func modelTraceBankNewer(candidate, base *proxy.ModelTraceDetector) bool {
	return !candidate.BuiltAt().IsZero() && candidate.BuiltAt().After(base.BuiltAt())
}

// resolveModelTraceBanks loads the embedded bank and any installed override.
// The override is re-read whenever its stored revision changes, so an update
// made on one replica reaches the others on their next detection.
func (h *Handler) resolveModelTraceBanks(ctx context.Context) (active, embedded, override *proxy.ModelTraceDetector, overrideErr error) {
	embedded, err := proxy.NewModelTraceDetector()
	if err != nil {
		return nil, nil, nil, err
	}
	if h == nil || h.db == nil {
		return embedded, embedded, nil, nil
	}
	revision, err := h.db.GetModelTraceBankRevision(ctx)
	if err != nil || revision == "" {
		return embedded, embedded, nil, err
	}
	override = proxy.CachedModelTraceOverride(revision)
	if override == nil {
		stored, err := h.db.GetModelTraceBankOverride(ctx)
		if err != nil || stored == nil {
			return embedded, embedded, nil, err
		}
		override, err = proxy.ParseModelTraceBankOverride(stored.BankJSON, stored.Revision)
		if err != nil {
			return embedded, embedded, nil, err
		}
		proxy.CacheModelTraceOverride(override)
	}
	if !modelTraceBankNewer(override, embedded) {
		return embedded, embedded, override, nil
	}
	return override, embedded, override, nil
}

func (h *Handler) currentModelTraceDetector(ctx context.Context) (*proxy.ModelTraceDetector, error) {
	active, _, _, overrideErr := h.resolveModelTraceBanks(ctx)
	if overrideErr != nil {
		log.Printf("[modeltrace] installed fingerprint library unavailable, falling back to the built-in version: %v", overrideErr)
	}
	if active == nil {
		return nil, overrideErr
	}
	return active, nil
}

func (h *Handler) modelTraceBankStatus(ctx context.Context) (modelTraceBankStatus, error) {
	active, embedded, override, overrideErr := h.resolveModelTraceBanks(ctx)
	if active == nil {
		return modelTraceBankStatus{}, overrideErr
	}
	status := modelTraceBankStatus{Active: active.Info(), Embedded: embedded.Info()}
	if override != nil {
		info := override.Info()
		status.Override = &info
		status.OverrideStale = active != override
	}
	if overrideErr != nil {
		status.OverrideError = overrideErr.Error()
	}
	return status, nil
}

// GetModelTraceBank reports the active fingerprint bank.
//
// GET /api/admin/modeltrace/bank
func (h *Handler) GetModelTraceBank(c *gin.Context) {
	status, err := h.modelTraceBankStatus(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to load the ModelTrace fingerprint library")
		return
	}
	c.JSON(http.StatusOK, status)
}

// UpdateModelTraceBank installs the bank from upstream main when it is newer
// than the active one. Nothing is fetched unless an operator asks for it.
//
// POST /api/admin/modeltrace/bank/update
func (h *Handler) UpdateModelTraceBank(c *gin.Context) {
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Database unavailable; cannot save the fingerprint library")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	// The stored revision is both the freshness baseline and the CAS token for
	// the save. Falling back to the embedded bank here could downgrade a newer
	// installed override, so a read failure aborts the update.
	storedRevision, err := h.db.GetModelTraceBankRevision(ctx)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "Failed to read the installed fingerprint library: "+err.Error())
		return
	}
	active, _, _, _ := h.resolveModelTraceBanks(ctx)
	if active == nil {
		writeError(c, http.StatusInternalServerError, "Failed to load the ModelTrace fingerprint library")
		return
	}
	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}

	revision, err := proxy.FetchLatestModelTraceRevision(ctx, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Failed to fetch the latest ModelTrace version: "+err.Error())
		return
	}
	response := modelTraceBankUpdateResponse{}
	if revision == active.Revision() {
		response.Message = "Already on the latest version"
	} else {
		body, err := proxy.FetchModelTraceBank(ctx, revision, proxyURL)
		if err != nil {
			writeError(c, http.StatusBadGateway, "Failed to download the ModelTrace fingerprint library: "+err.Error())
			return
		}
		candidate, err := proxy.ParseModelTraceBankOverride(body, revision)
		if err != nil {
			writeError(c, http.StatusBadGateway, "Upstream fingerprint library validation failed: "+err.Error())
			return
		}
		if !modelTraceBankNewer(candidate, active) {
			response.Message = "The upstream fingerprint library has no training data newer than the current version"
		} else {
			err := h.db.SaveModelTraceBankOverride(ctx, database.ModelTraceBankOverride{
				Revision: revision, BuiltAt: candidate.Info().BuiltAt, BankJSON: body, InstalledAt: time.Now(),
			}, storedRevision)
			if errors.Is(err, database.ErrModelTraceBankConflict) {
				writeError(c, http.StatusConflict, "The fingerprint library was changed by another operation during the update; refresh and try again")
				return
			}
			if err != nil {
				writeError(c, http.StatusInternalServerError, "Failed to save the fingerprint library: "+err.Error())
				return
			}
			proxy.CacheModelTraceOverride(candidate)
			response.Updated = true
			response.Message = "Fingerprint library updated"
			response.AddedModels, response.RemovedModels = diffModelTraceModels(active.Models(), candidate.Models())
			log.Printf("[modeltrace] fingerprint library updated: %s -> %s (added %v, removed %v)",
				active.Revision(), revision, response.AddedModels, response.RemovedModels)
		}
	}
	status, err := h.modelTraceBankStatus(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to load the ModelTrace fingerprint library")
		return
	}
	response.modelTraceBankStatus = status
	c.JSON(http.StatusOK, response)
}

// ResetModelTraceBank removes the installed override and reverts to the bank
// bundled with this release.
//
// DELETE /api/admin/modeltrace/bank
func (h *Handler) ResetModelTraceBank(c *gin.Context) {
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Database unavailable")
		return
	}
	if err := h.db.DeleteModelTraceBankOverride(c.Request.Context()); err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to restore the built-in fingerprint library: "+err.Error())
		return
	}
	status, err := h.modelTraceBankStatus(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to load the ModelTrace fingerprint library")
		return
	}
	c.JSON(http.StatusOK, status)
}

func diffModelTraceModels(before, after []string) (added, removed []string) {
	beforeSet := make(map[string]bool, len(before))
	for _, model := range before {
		beforeSet[model] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, model := range after {
		afterSet[model] = true
		if !beforeSet[model] {
			added = append(added, model)
		}
	}
	for _, model := range before {
		if !afterSet[model] {
			removed = append(removed, model)
		}
	}
	return added, removed
}
