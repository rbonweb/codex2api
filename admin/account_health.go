package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	// accountHealthBlockCount / accountHealthBlockMinutes set the granularity of the health status bar:
	// 20 cells × 10 minutes ≈ covers the last 3.3 hours. Must stay in sync with the frontend AccountHealthBar.
	accountHealthBlockCount   = 20
	accountHealthBlockMinutes = 10
)

// GetAccountHealthBars returns each account's recent success and failure request buckets, used for the
// health status bar on the account management page. GET /api/accounts/health-bars
//
// Response shape: { buckets: { "<accountId>": [{success,failed}, ...20], ... },
//
//	block_count: 20, block_minutes: 10 }
func (h *Handler) GetAccountHealthBars(c *gin.Context) {
	ctx := c.Request.Context()
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid ids parameter")
		return
	}

	buckets, err := h.db.GetAccountsHealthBucketsByIDs(
		ctx,
		ids,
		time.Now(),
		accountHealthBlockCount,
		accountHealthBlockMinutes*time.Minute,
	)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to get health status: "+err.Error())
		return
	}

	out := make(map[string][]database.AccountHealthBucket, len(buckets))
	for id, b := range buckets {
		out[strconv.FormatInt(id, 10)] = b
	}

	c.JSON(http.StatusOK, gin.H{
		"buckets":       out,
		"block_count":   accountHealthBlockCount,
		"block_minutes": accountHealthBlockMinutes,
	})
}
