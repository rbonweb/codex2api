// Package proxy: fine-grained rate limit and quota checks at the API Key level.
//
// Entry point: enforceAPIKeyLimits(). Called after request auth and before forwarding upstream. On failure returns (httpStatus, message),
// the handler short-circuits directly with that response; on success returns (0, "").
//
// 7 kinds of limits:
//   - ModelAllow / ModelDeny: O(1) string set, local memory is enough, no side effects.
//   - RPM:  requests in a sliding 60s window, DB aggregate + Redis/Memory 60s cache.
//   - RPD:  requests in a sliding 24h window, same as above.
//   - CostLimit5h / CostLimit7d:    user_billed total in a sliding 5h / 7d window. Redis 60s cache + DB aggregate fallback.
//   - TokenLimit5h / TokenLimit7d:  same as cost, aggregates total_tokens.
//   - CostLimitDaily / TokenLimitDaily: calendar-day (local timezone) total, reset at midnight. The cache key carries a date stamp,
//     so it expires automatically at midnight and never leaks the previous day's usage into the new day.
//
// When Redis is down or missing, always fall back to DB aggregate + 1 minute cache, favoring availability.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	apiKeyLimitsCacheNamespace = "api-key-limits"
	apiKeyLimitsCacheTTL       = 60 * time.Second
	apiKeyRPMWindow            = time.Minute
	apiKeyRPDWindow            = 24 * time.Hour
	apiKey5hWindow             = 5 * time.Hour
	apiKey7dWindow             = 7 * 24 * time.Hour
	apiKey30dWindow            = 30 * 24 * time.Hour
)

// apiKeyRowFromContext returns the APIKeyRow stored in the gin context at authentication time.
// If it does not exist (for example, an internal call that skipped the auth path), returns nil.
func apiKeyRowFromContext(c *gin.Context) *database.APIKeyRow {
	if c == nil {
		return nil
	}
	v, ok := c.Get(contextAPIKeyRow)
	if !ok || v == nil {
		return nil
	}
	row, _ := v.(*database.APIKeyRow)
	return row
}

// APIKeyRowFromContext returns the API key metadata stored by the /v1 auth middleware.
func APIKeyRowFromContext(c *gin.Context) *database.APIKeyRow {
	return apiKeyRowFromContext(c)
}

// payloadRuleIdentity builds the payload rule identity from the auth context (shared by the api_key_*/group_* match gates and
// the service_tier billing recalculation). Without an auth identity it returns nil (rules behind an identity gate fail closed and never match).
func (h *Handler) payloadRuleIdentity(c *gin.Context) *PayloadRuleIdentity {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return nil
	}
	id := &PayloadRuleIdentity{APIKeyID: row.ID, APIKeyName: strings.TrimSpace(row.Name)}
	if h != nil && h.store != nil {
		// Use the store's allowed groups (refreshed after a group is deleted, so this is the authoritative source), then resolve the group names.
		id.GroupIDs = h.store.GetAPIKeyAllowedGroups(row.ID)
		id.GroupNames = h.store.ResolveGroupNames(id.GroupIDs)
	}
	return id
}

// EnforceAPIKeyLimits checks API key scoped model and rate/cost limits.
func (h *Handler) EnforceAPIKeyLimits(c *gin.Context, model string) (int, string) {
	return h.enforceAPIKeyLimitsChecks(c, model, true)
}

// EnforceAPIKeyLimitsForRequests performs the normal API-key checks and also
// verifies that a batched request cannot cross the RPM/RPD boundary by issuing
// multiple upstream calls after a single admission check.
//
// It deliberately does not reserve the batch here: every generated image is an
// upstream request that re-enters the standard Images handler and reserves one
// RPM/RPD slot there. Reserving the batch here as well would charge 2N.
func (h *Handler) EnforceAPIKeyLimitsForRequests(c *gin.Context, model string, requests int) (int, string) {
	if requests < 1 {
		requests = 1
	}
	row := apiKeyRowFromContext(c)
	if row != nil && row.ID > 0 && requests > 1 {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if row.Limits.RPM > 0 {
			count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpm", apiKeyRPMWindow)
			if err == nil && apiKeyBatchWouldExceed(count, row.Limits.RPM, requests) {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key rate limit exceeded: batch of %d would exceed %d requests per minute", requests, row.Limits.RPM)
			}
		}
		if row.Limits.RPD > 0 {
			count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpd", apiKeyRPDWindow)
			if err == nil && apiKeyBatchWouldExceed(count, row.Limits.RPD, requests) {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key rate limit exceeded: batch of %d would exceed %d requests per day", requests, row.Limits.RPD)
			}
		}
	}
	return h.enforceAPIKeyLimitsChecks(c, model, false)
}

func apiKeyBatchWouldExceed(current int64, limit, requests int) bool {
	return limit > 0 && requests > 0 && current+int64(requests) > int64(limit)
}

// enforceAPIKeyLimits checks all limit conditions for the API Key.
// When a limit is hit, returns (status, errorMessage), and the handler should short-circuit immediately with that response;
// if all checks pass, returns (0, "").
//
// The model parameter is used for the model allow/deny check; pass an empty string to skip the model check.
//
// Returns status:
//   - http.StatusForbidden (403): model is not in the allowlist / is in the blocklist
//   - http.StatusTooManyRequests (429): any rpm/rpd/cost/token window is over its limit
func (h *Handler) enforceAPIKeyLimits(c *gin.Context, model string) (int, string) {
	return h.enforceAPIKeyLimitsChecks(c, model, true)
}

// enforceAPIKeyLimitsChecks runs all limit checks; when reserve is false it skips the in-process
// RPM/RPD count, reused by batch enqueueing (each upstream call reserves its own at execution time).
func (h *Handler) enforceAPIKeyLimitsChecks(c *gin.Context, model string, reserve bool) (int, string) {
	var daybreakStatus int
	var daybreakMessage string
	model, daybreakStatus, daybreakMessage = h.checkDaybreakRequest(c, model)
	if daybreakStatus != 0 {
		return daybreakStatus, daybreakMessage
	}
	h.attachAPIKeyModelRequestQuota(c, false)
	row := apiKeyRowFromContext(c)
	if row == nil {
		return 0, ""
	}
	limits := row.Limits
	if limits.IsZero() {
		return 0, ""
	}

	// 1. Model allow/deny check (O(1) local check, no I/O)
	if model != "" {
		if msg := checkAPIKeyModel(model, limits); msg != "" {
			return http.StatusForbidden, msg
		}
	}

	// 1b. Image generation block policy (local check: model/endpoint/request body intent, no I/O).
	// The strip policy does not short-circuit here; it rewrites the request body before forwarding (see stripResponsesImageGenerationCapabilities).
	if limits.ResolveImageGenerationPolicy() == database.ImageGenerationPolicyBlock {
		if msg := checkAPIKeyImageGeneration(c, model); msg != "" {
			return http.StatusForbidden, msg
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	var dayStart time.Time
	if limits.CostLimitDaily > 0 || limits.TokenLimitDaily > 0 {
		dayStart = database.StartOfDay(time.Now())
	}
	ctx = h.withAPIKeyLimitBatch(ctx, row, dayStart)
	// 2. RPM
	if limits.RPM > 0 {
		count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpm", apiKeyRPMWindow)
		if err == nil && count >= int64(limits.RPM) {
			return http.StatusTooManyRequests,
				fmt.Sprintf("API key rate limit exceeded: %d requests per minute", limits.RPM)
		}
	}

	// 3. RPD
	if limits.RPD > 0 {
		count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpd", apiKeyRPDWindow)
		if err == nil && count >= int64(limits.RPD) {
			return http.StatusTooManyRequests,
				fmt.Sprintf("API key rate limit exceeded: %d requests per day", limits.RPD)
		}
	}

	// 4. Calendar-day cost / token (issue #460). Fixed window: resets at midnight in the server's local timezone,
	// Unlike the sliding windows below, it fully recovers at reset time, and the error text includes the reset time.
	if limits.CostLimitDaily > 0 || limits.TokenLimitDaily > 0 {
		usage, err := h.apiKeyDailyUsage(ctx, row.ID, dayStart)
		if err == nil && usage != nil {
			resetAt := dayStart.AddDate(0, 0, 1).Format(time.RFC3339)
			if limits.CostLimitDaily > 0 && usage.UserBilled >= limits.CostLimitDaily {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key daily cost limit exceeded: $%.2f / $%.2f today (resets at %s)",
						usage.UserBilled, limits.CostLimitDaily, resetAt)
			}
			if limits.TokenLimitDaily > 0 && usage.Tokens >= limits.TokenLimitDaily {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key daily token limit exceeded: %d / %d today (resets at %s)",
						usage.Tokens, limits.TokenLimitDaily, resetAt)
			}
		}
	}

	// 5. cost / token 5h & 7d
	if limits.CostLimit5h > 0 || limits.TokenLimit5h > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "5h", apiKey5hWindow)
		if err == nil && usage != nil {
			if limits.CostLimit5h > 0 && usage.UserBilled >= limits.CostLimit5h {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 5h",
						usage.UserBilled, limits.CostLimit5h)
			}
			if limits.TokenLimit5h > 0 && usage.Tokens >= limits.TokenLimit5h {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 5h",
						usage.Tokens, limits.TokenLimit5h)
			}
		}
	}
	if limits.CostLimit7d > 0 || limits.TokenLimit7d > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "7d", apiKey7dWindow)
		if err == nil && usage != nil {
			if limits.CostLimit7d > 0 && usage.UserBilled >= limits.CostLimit7d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 7d",
						usage.UserBilled, limits.CostLimit7d)
			}
			if limits.TokenLimit7d > 0 && usage.Tokens >= limits.TokenLimit7d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 7d",
						usage.Tokens, limits.TokenLimit7d)
			}
		}
	}
	if limits.CostLimit30d > 0 || limits.TokenLimit30d > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "30d", apiKey30dWindow)
		if err == nil && usage != nil {
			if limits.CostLimit30d > 0 && usage.UserBilled >= limits.CostLimit30d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 30d",
						usage.UserBilled, limits.CostLimit30d)
			}
			if limits.TokenLimit30d > 0 && usage.Tokens >= limits.TokenLimit30d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 30d",
						usage.Tokens, limits.TokenLimit30d)
			}
		}
	}

	// 6. Group / account scope budget (issue #439). Reject-type overruns short-circuit here; skip-type ones attach to
	// the gin context, and each handler's account filter chain removes the matching candidates.
	if len(limits.ScopeLimits) > 0 {
		gate, rejectMsg := h.evaluateAPIKeyScopeBudgets(ctx, row)
		if rejectMsg != "" {
			return http.StatusTooManyRequests, rejectMsg
		}
		if gate != nil {
			c.Set(contextScopeBudgetGate, gate)
		}
	}

	// 7. In-process admission count. The DB aggregate + 60s cache read the same stale value and cannot bound
	// concurrent bursts within the cache window; here the count is atomically incremented by the actual request count before letting it through.
	if reserve {
		if allowed, reason := h.admissionLimiter().allow(row.ID, limits.RPM, limits.RPD, 1, time.Now()); !allowed {
			if reason == "capacity" {
				return http.StatusServiceUnavailable, "API key admission capacity exhausted; retry later"
			}
			if reason == "rpd" {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key rate limit exceeded: %d requests per day", limits.RPD)
			}
			return http.StatusTooManyRequests,
				fmt.Sprintf("API key rate limit exceeded: %d requests per minute", limits.RPM)
		}
	}

	return 0, ""
}

// apiKeyImageGenerationPolicy returns the image tool policy of the API Key that owns the current request.
// Without an auth identity (internal path), returns allow and does not rewrite the request.
func apiKeyImageGenerationPolicy(c *gin.Context) string {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return database.ImageGenerationPolicyAllow
	}
	return row.Limits.ResolveImageGenerationPolicy()
}

// applyImageGenerationStripPolicy, under the strip policy, removes the image tool capability declarations from the request body,
// and forwards the request as a plain text request (issue #411). Call it after the request body is ready (including the gateway's automatic injection of
// image tool / bridge instructions), so that the gateway-injected capability declarations are cleaned up too. Non-strip policies
// return the input unchanged.
func applyImageGenerationStripPolicy(c *gin.Context, body []byte) []byte {
	if apiKeyImageGenerationPolicy(c) != database.ImageGenerationPolicyStrip {
		return body
	}
	return stripResponsesImageGenerationCapabilities(body)
}

// SendAPIKeyLimitError writes a standard /v1 API key limit error response.
// usageLimitedPoolMessage is the downstream-facing text for a pool that has
// candidates but all of them are rate-limited.
type usageLimitedPoolMessage struct {
	Chinese string
	English string
	// RetryAfterSeconds is non-zero only for a transient throttle; quota
	// exhaustion deliberately carries no hint so downstream fails over.
	RetryAfterSeconds int
}

const (
	usageWindowExhaustedMessageZH = "Codex account usage window limit reached"
	usageWindowExhaustedMessageEN = "Codex account usage window limit reached"
)

// usageLimitedPoolMessages distinguishes a short account-wide throttle from an
// exhausted usage window so downstream gateways do not fail over or flag the
// upstream on a freeze that clears in seconds.
func usageLimitedPoolMessages(summary auth.UsageLimitedCandidateSummary) usageLimitedPoolMessage {
	if !summary.TransientOnly {
		return usageLimitedPoolMessage{Chinese: usageWindowExhaustedMessageZH, English: usageWindowExhaustedMessageEN}
	}
	seconds := int(math.Ceil(summary.RetryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return usageLimitedPoolMessage{
		Chinese:           fmt.Sprintf("Codex account is briefly rate limited, please retry in %d seconds", seconds),
		English:           fmt.Sprintf("Codex accounts are temporarily throttled, retry after %d seconds", seconds),
		RetryAfterSeconds: seconds,
	}
}

func SendAPIKeyLimitError(c *gin.Context, status int, msg string) {
	errType := api.ErrorTypeRateLimit
	errCode := api.ErrCodeRateLimitReached
	if status == http.StatusForbidden {
		errType = api.ErrorTypePermission
		errCode = api.ErrCodeInvalidRequest
	}
	if status == http.StatusBadRequest {
		errType, errCode = api.ErrorTypeInvalidRequest, api.ErrCodeInvalidParameter
	}
	if status == http.StatusServiceUnavailable {
		errType, errCode = api.ErrorTypeServer, api.ErrCodeServiceUnavailable
	}
	api.SendErrorWithStatus(c, api.NewAPIError(errCode, msg, errType), status)
}

// enforceAPIKeyLimitsAndReply is called before request forwarding; when a limit is hit it has already written the response to the client and returns true.
// The handler should return immediately on true, with no further processing.
func (h *Handler) enforceAPIKeyLimitsAndReply(c *gin.Context, model string) bool {
	status, msg := h.enforceAPIKeyLimits(c, model)
	if status == 0 {
		return false
	}
	SendAPIKeyLimitError(c, status, msg)
	return true
}

// checkAPIKeyModel checks whether the model is allowed.
// If the allowlist is non-empty, the model must be in it; otherwise the blocklist applies, and the model must not be in it.
// Case-insensitive comparison.
func checkAPIKeyModel(model string, limits database.APIKeyLimits) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if len(limits.ModelAllow) > 0 {
		for _, m := range limits.ModelAllow {
			allowed := strings.ToLower(strings.TrimSpace(m))
			if allowed == model {
				return ""
			}
		}
		return fmt.Sprintf("Model %q is not allowed for this API key", model)
	}
	for _, m := range limits.ModelDeny {
		denied := strings.ToLower(strings.TrimSpace(m))
		if denied == model {
			return fmt.Sprintf("Model %q is denied for this API key", model)
		}
	}
	return ""
}

// checkAPIKeyImageGeneration, when image generation is disabled for this key, checks whether this request touches the image generation capability.
// On a hit (image generation model / /v1/images endpoint / request body with an image_generation tool or image intent) it returns the error text,
// otherwise returns "". Purely local check: model name + endpoint path + the client's original request body (automatically injected image tools happen only after this point,
// so they do not affect plain text requests).
func checkAPIKeyImageGeneration(c *gin.Context, model string) string {
	const msg = "Image generation is disabled for this API key (gpt-image models and the image_generation tool are not permitted)."

	if isImageOnlyModel(strings.TrimSpace(model)) {
		return msg
	}

	if c != nil && c.Request != nil && c.Request.URL != nil {
		path := c.Request.URL.Path
		if strings.Contains(path, "/images/generations") || strings.Contains(path, "/images/edits") {
			return msg
		}
	}

	if body, ok := rawRequestBodyFromContext(c); ok && len(body) > 0 {
		if responsesBodyHasImageGenerationTool(body) || responsesBodyRequestsImageGeneration(body) {
			return msg
		}
	}
	return ""
}

// apiKeyWindowRequests returns the request count of an API Key within the given window.
// Reads the Redis cache first; on failure falls back to a DB aggregate, cached for 60s.
func (h *Handler) apiKeyWindowRequests(ctx context.Context, apiKeyID int64, label string, window time.Duration) (int64, error) {
	cacheKey := apiKeyLimitsCacheKey(apiKeyID, "req", label)
	if v, ok := h.readAPIKeyLimitCache(ctx, cacheKey); ok {
		return v.Requests, nil
	}
	usage, err := h.db.GetAPIKeyWindowUsage(ctx, apiKeyID, window)
	if err != nil {
		return 0, err
	}
	h.writeAPIKeyLimitCache(ctx, cacheKey, usage)
	return usage.Requests, nil
}

// apiKeyDailyUsage returns the total usage of an API Key for the current day (since dayStart).
// The cache key embeds a date stamp, so old cache entries expire naturally after midnight and the new day starts from zero.
func (h *Handler) apiKeyDailyUsage(ctx context.Context, apiKeyID int64, dayStart time.Time) (*database.APIKeyWindowUsage, error) {
	cacheKey := apiKeyLimitsCacheKey(apiKeyID, "usage", "daily:"+dayStart.Format("2006-01-02"))
	if v, ok := h.readAPIKeyLimitCache(ctx, cacheKey); ok {
		return v, nil
	}
	usage, err := h.db.GetAPIKeyUsageSince(ctx, apiKeyID, dayStart)
	if err != nil {
		return nil, err
	}
	h.writeAPIKeyLimitCache(ctx, cacheKey, usage)
	return usage, nil
}

func (h *Handler) apiKeyWindowUsage(ctx context.Context, apiKeyID int64, label string, window time.Duration) (*database.APIKeyWindowUsage, error) {
	cacheKey := apiKeyLimitsCacheKey(apiKeyID, "usage", label)
	if v, ok := h.readAPIKeyLimitCache(ctx, cacheKey); ok {
		return v, nil
	}
	usage, err := h.db.GetAPIKeyWindowUsage(ctx, apiKeyID, window)
	if err != nil {
		return nil, err
	}
	h.writeAPIKeyLimitCache(ctx, cacheKey, usage)
	return usage, nil
}

func apiKeyLimitsCacheKey(apiKeyID int64, kind, label string) string {
	return fmt.Sprintf("%d:%s:%s", apiKeyID, kind, label)
}

func (h *Handler) readAPIKeyLimitCache(ctx context.Context, key string) (*database.APIKeyWindowUsage, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	raw := h.apiKeyLimitPayload(ctx, key)
	if len(raw) == 0 {
		return nil, false
	}
	var usage database.APIKeyWindowUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, false
	}
	return &usage, true
}

func (h *Handler) writeAPIKeyLimitCache(ctx context.Context, key string, usage *database.APIKeyWindowUsage) {
	if h == nil || h.cache == nil || usage == nil {
		return
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return
	}
	_ = h.cache.SetRuntime(ctx, apiKeyLimitsCacheNamespace, key, raw, apiKeyLimitsCacheTTL)
}
