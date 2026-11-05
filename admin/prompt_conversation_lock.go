package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type unlockPromptConversationRequest struct {
	Reason string `json:"reason"`
	Scope  string `json:"scope"`
}

func (h *Handler) UnlockPromptConversation(c *gin.Context) {
	lockKey := strings.ToLower(strings.TrimSpace(c.Param("lock_key")))
	if len(lockKey) != 64 {
		writeError(c, http.StatusBadRequest, "Invalid conversation lock ID")
		return
	}
	var request unlockPromptConversationRequest
	if c.Request != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "Invalid request body")
			return
		}
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" {
		request.Reason = "unlocked by administrator"
	}
	request.Scope = strings.ToLower(strings.TrimSpace(request.Scope))
	if request.Scope == "" {
		request.Scope = database.PromptConversationRestrictionScopeConversation
	}
	if request.Scope != database.PromptConversationRestrictionScopeConversation && request.Scope != database.PromptConversationRestrictionScopeUserCooldown {
		writeError(c, http.StatusBadRequest, "Invalid unlock scope")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if request.Scope == database.PromptConversationRestrictionScopeUserCooldown {
		current, err := h.db.GetPromptConversationLock(ctx, lockKey)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "User cooldown record not found")
			return
		}
		if err != nil {
			writeInternalError(c, err)
			return
		}
		keys, err := h.db.UnlockPromptConversationUserCooldown(ctx, current.Platform, current.NewAPIUserID, request.Reason)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Active user cooldown not found or already lifted")
			return
		}
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if h.cache != nil {
			for _, key := range keys {
				_ = h.cache.DeleteRuntime(ctx, database.PromptConversationLockCacheNamespace, key)
			}
		}
		item, err := h.db.GetPromptConversationLock(ctx, lockKey)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"lock": item, "scope": request.Scope, "unlocked_count": len(keys)})
		return
	}
	item, err := h.db.UnlockPromptConversation(ctx, lockKey, request.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "Active conversation lock not found or already unlocked")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if h.cache != nil {
		_ = h.cache.DeleteRuntime(ctx, database.PromptConversationLockCacheNamespace, lockKey)
	}
	c.JSON(http.StatusOK, gin.H{"lock": item, "scope": request.Scope, "unlocked_count": 1})
}
