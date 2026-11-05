package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

// Subscription status lookup and manual refresh: the status object is computed on the server in the business time zone, and the frontend no longer decides on its own by
// the local clock whether a subscription is "due today" or "overdue". Manual refresh bypasses the background probe's 6-hour throttle, but has its own
// short rate limit: the subscription endpoint passes Cloudflare through fingerprint disguise, so rapid repeated clicks only raise the chance of being blocked.

// subscriptionManualRefreshMinInterval is the minimum interval between two manual refreshes of the same account.
const subscriptionManualRefreshMinInterval = 30 * time.Second

// subscriptionStatusViewForRow builds the subscription status object from a DB row (shared by list and detail).
func subscriptionStatusViewForRow(row *database.AccountRow, planType string) *auth.SubscriptionStatusView {
	return subscriptionStatusViewForRowAt(row, planType, time.Now())
}

func subscriptionStatusViewForRowAt(row *database.AccountRow, planType string, now time.Time) *auth.SubscriptionStatusView {
	if row == nil {
		return nil
	}
	var expiresAt time.Time
	if raw := strings.TrimSpace(row.GetCredential("subscription_expires_at")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			expiresAt = parsed
		}
	}
	meta := auth.SubscriptionMetaFromCredentials(row.GetCredential)
	return auth.BuildSubscriptionStatusView(planType, expiresAt, meta, now, time.Local)
}

// GetAccountSubscription returns the account's current subscription status object (idempotent, does not call upstream).
// GET /api/accounts/:id/subscription
func (h *Handler) GetAccountSubscription(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "account is not in the runtime pool")
		return
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil {
		c.JSON(http.StatusOK, gin.H{"supported": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"supported": true, "subscription": view})
}

// RefreshAccountSubscription queries the subscription provider once immediately and returns the result classification and the latest status.
// POST /api/accounts/:id/subscription/refresh
//
// Result outcome: updated / unchanged / no_subscription / unsupported / failed.
// A failure does not change the HTTP status code (still 200) and is expressed through outcome + error; only a missing account,
// invalid parameters and rate limiting return 4xx.
func (h *Handler) RefreshAccountSubscription(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "account is not in the runtime pool")
		return
	}
	now := time.Now()
	if !auth.SubscriptionPlanTracked(account.GetPlanType(), account.GetSubscriptionExpiresAt()) {
		c.JSON(http.StatusOK, gin.H{"outcome": string(proxy.SubscriptionSyncUnsupported)})
		return
	}
	if checkedAt := account.SubscriptionMetaSnapshot().CheckedAt; !checkedAt.IsZero() {
		if elapsed := now.Sub(checkedAt); elapsed >= 0 && elapsed < subscriptionManualRefreshMinInterval {
			retryAfter := int((subscriptionManualRefreshMinInterval - elapsed).Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":        "subscription status was queried just now, please try again later",
				"retry_after":  retryAfter,
				"subscription": account.SubscriptionStatusView(now),
			})
			return
		}
	}
	if !account.BeginSubscriptionSync() {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":        "subscription status sync is in progress",
			"retry_after":  5,
			"subscription": account.SubscriptionStatusView(now),
		})
		return
	}
	defer account.EndSubscriptionSync()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result := proxy.SyncSubscriptionExpiry(ctx, h.store, account, h.store.ResolveProxyForAccount(account))

	resp := gin.H{"outcome": string(result.Outcome)}
	if result.Err != nil {
		resp["error"] = proxy.SubscriptionErrorSummary(result.Err)
	}
	if view := account.SubscriptionStatusView(time.Now()); view != nil {
		resp["subscription"] = view
	}
	if !result.ExpiresAt.IsZero() {
		resp["subscription_expires_at"] = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, resp)
}

// Values of the account list "subscription status" filter. expiring_* includes accounts due today; pending/failed look at the sync state;
// unknown means "the plan needs tracking but no expiry information has ever been obtained".
const (
	subscriptionFilterActive        = "active"
	subscriptionFilterExpiring9d    = "expiring_9d"
	subscriptionFilterExpiring3d    = "expiring_3d"
	subscriptionFilterExpiringToday = "expiring_today"
	subscriptionFilterExpired       = "expired"
	subscriptionFilterGracePeriod   = "grace_period"
	subscriptionFilterPending       = "pending"
	subscriptionFilterFailed        = "failed"
	subscriptionFilterUnknown       = "unknown"
)

var validSubscriptionFilters = map[string]bool{
	"": true, "all": true,
	subscriptionFilterActive: true, subscriptionFilterExpiring9d: true, subscriptionFilterExpiring3d: true,
	subscriptionFilterExpiringToday: true, subscriptionFilterExpired: true, subscriptionFilterGracePeriod: true,
	subscriptionFilterPending: true, subscriptionFilterFailed: true, subscriptionFilterUnknown: true,
}

// subscriptionFilterMatches filters list items by the server-computed subscription status object; accounts that do not track a subscription
// (api / free without an expiry time) never match any filter value.
func subscriptionFilterMatches(item *accountListSnapshotItem, filter string, now time.Time) bool {
	if item == nil || item.Row == nil {
		return false
	}
	view := subscriptionStatusViewForRowAt(item.Row, item.PlanType, now)
	if view == nil {
		return false
	}
	switch filter {
	case subscriptionFilterActive:
		return view.BusinessStatus == auth.SubscriptionStatusActive
	case subscriptionFilterExpiring9d:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday ||
			(view.BusinessStatus == auth.SubscriptionStatusActive && view.DaysRemaining <= 9)
	case subscriptionFilterExpiring3d:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday ||
			(view.BusinessStatus == auth.SubscriptionStatusActive && view.DaysRemaining <= 3)
	case subscriptionFilterExpiringToday:
		return view.BusinessStatus == auth.SubscriptionStatusExpiringToday
	case subscriptionFilterExpired:
		return view.BusinessStatus == auth.SubscriptionStatusExpired
	case subscriptionFilterGracePeriod:
		return view.BusinessStatus == auth.SubscriptionStatusGracePeriod
	case subscriptionFilterPending:
		return view.SyncState == auth.SubscriptionSyncPending
	case subscriptionFilterFailed:
		return view.SyncState == auth.SubscriptionSyncFailed
	case subscriptionFilterUnknown:
		return view.BusinessStatus == auth.SubscriptionStatusUnknown && view.SyncState != auth.SubscriptionSyncPending
	}
	return true
}
