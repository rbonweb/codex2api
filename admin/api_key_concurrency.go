package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetAPIKeyConcurrency reads the in-memory snapshot, to avoid re-aggregating historical usage on every poll.
func (h *Handler) GetAPIKeyConcurrency(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"concurrency": h.authCacheProxy.APIKeyConcurrencySnapshot()})
}
