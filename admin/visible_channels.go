package admin

import (
	"context"
	"log"
	"net/http"

	"github.com/codex2api/database"

	"github.com/gin-gonic/gin"
)

// Visible channels setting for the admin panel: controls which upstream channels are shown in the dashboard and account management.
// It only affects admin panel display and does not affect scheduling or API behavior; Codex, as the fallback channel, is always shown.

type visibleChannelsResponse struct {
	Channels []string `json:"channels"`
	All      []string `json:"all"`
	Fallback string   `json:"fallback"`
}

func (h *Handler) visibleChannels(ctx context.Context) []string {
	if h == nil || h.db == nil {
		return database.NormalizeVisibleChannels(nil)
	}
	cfg, err := h.db.LoadVisibleChannelsConfig(ctx)
	if err != nil {
		log.Printf("failed to read visible channels settings, showing all: %v", err)
		return database.NormalizeVisibleChannels(nil)
	}
	return cfg.Effective()
}

func buildVisibleChannelsResponse(channels []string) visibleChannelsResponse {
	return visibleChannelsResponse{
		Channels: channels,
		All:      append([]string(nil), database.AllUpstreamChannels...),
		Fallback: database.FallbackVisibleChannel,
	}
}

// GetVisibleChannelsSettings returns the current visible channels.
// GET /api/admin/settings/visible-channels
func (h *Handler) GetVisibleChannelsSettings(c *gin.Context) {
	c.JSON(http.StatusOK, buildVisibleChannelsResponse(h.visibleChannels(c.Request.Context())))
}

// UpdateVisibleChannelsSettings saves the visible channels; unknown channels are dropped, and the fallback channel is added back automatically.
// PUT /api/admin/settings/visible-channels  {"channels":["codex"]}
func (h *Handler) UpdateVisibleChannelsSettings(c *gin.Context) {
	var req struct {
		Channels []string `json:"channels"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}
	if req.Channels == nil {
		writeError(c, http.StatusBadRequest, "missing channels field")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	normalized := database.NormalizeVisibleChannels(req.Channels)
	if err := h.db.SaveVisibleChannelsConfig(c.Request.Context(),
		database.VisibleChannelsConfig{Channels: normalized}); err != nil {
		writeError(c, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, buildVisibleChannelsResponse(normalized))
}
