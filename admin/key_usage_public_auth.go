package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func (h *Handler) publicAPIKeyUsageRow(c *gin.Context, ctx context.Context) *database.APIKeyRow {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "service not ready")
		return nil
	}
	enabled, err := h.PublicAPIKeyUsagePageEnabled(ctx)
	if err != nil {
		writeInternalError(c, err)
		return nil
	}
	if !enabled {
		writeError(c, http.StatusNotFound, "API Key self-service usage page is not enabled")
		return nil
	}
	key := extractPublicAPIKey(c)
	if key == "" {
		writeError(c, http.StatusUnauthorized, "missing Authorization Bearer API Key")
		return nil
	}
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		security.SecurityAuditLog("KEY_USAGE_AUTH_FAILED", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
		writeError(c, http.StatusUnauthorized, "API Key is invalid or does not exist")
		return nil
	}
	if err != nil {
		writeInternalError(c, err)
		return nil
	}
	// Expired keys and keys with used-up quota can still be queried; credentials disabled by an admin are rejected outright.
	if !row.Enabled {
		security.SecurityAuditLog("KEY_USAGE_AUTH_FAILED_DISABLED_KEY", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
		writeError(c, http.StatusUnauthorized, "API Key is disabled")
		return nil
	}
	return row
}
