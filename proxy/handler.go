package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/codex2api/security"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

const consoleUpstreamErrorLogMaxBytes = 4 * 1024

func upstreamErrorConsoleBody(body []byte) string {
	if len(bytes.TrimSpace(body)) > 0 && !gjson.ValidBytes(body) {
		return fmt.Sprintf("[non-JSON upstream body omitted; %d bytes]", len(body))
	}
	truncated := false
	if len(body) > consoleUpstreamErrorLogMaxBytes {
		body = body[:consoleUpstreamErrorLogMaxBytes]
		truncated = true
	}
	bodyStr := security.SafeTruncate(security.SanitizeLog(string(body)), consoleUpstreamErrorLogMaxBytes)
	if truncated {
		bodyStr += " ... [truncated]"
	}
	return bodyStr
}

// Handler API route handler
type Handler struct {
	store           *auth.Store
	configKeys      map[string]bool // static keys from the config file
	db              *database.DB
	cfg             *config.Config       // global config
	deviceCfg       *DeviceProfileConfig // device fingerprint config
	cache           cache.TokenCache     // Redis/Memory runtime cache
	apiKeyLookups   singleflight.Group
	authCache       *apiKeyAuthCache
	apiKeyGateMu    sync.Mutex
	promptRiskMu    sync.Mutex
	apiKeyGate      *apiKeyConcurrencyLimiter
	admissionOnce   sync.Once
	admission       *apiKeyAdmissionLimiter
	scopeUsageMu    sync.Mutex
	scopeUsage      *apiKeyScopeUsageTracker
	scopeDeltaInit  sync.Once
	scopeDeltaSlots chan struct{}
	liveStore       *liveCallStore
	// Registry of same-scope Responses WebSocket sessions that preempt locally; cross-instance ownership is coordinated by the runtime cache.
	responsesWSSessionPreemptions responsesWSSessionPreemptRegistry
	// Existence-gate cache for fingerprint replay cooldowns (see hasActiveFingerprintReplayLocks).
	fpReplayGateMu     sync.Mutex
	fpReplayGateAt     time.Time
	fpReplayGateActive bool
	// continuousRetryReplayFactory injects bounded replay failures in tests
	// without changing process-wide limits or TMPDIR.
	// continuousRetryReplayFactory injects bounded replay failures in tests, without changing process-level limits or TMPDIR.
	continuousRetryReplayFactory func() *continuousRetryReplay
}

const (
	apiKeyCacheNamespace      = "api-key"
	apiKeyCountCacheNamespace = "api-key-count"
	apiKeyCacheTTL            = 5 * time.Minute
	apiKeyCountCacheTTL       = 30 * time.Second
	// The upstream error body is only used to identify the error type and to return it to the client. Cap its size so that a malicious or
	// runaway upstream cannot fill gateway memory with an oversized body.
	maxUpstreamErrorBodyBytes = 1 << 20
)

type apiKeyRuntimeRecord struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type apiKeyCountRuntimeRecord struct {
	Count int `json:"count"`
}

func (h *Handler) nextAccountForSession(sessionID string, apiKeyID int64, exclude map[int64]bool) (*auth.Account, string) {
	return h.nextAccountForSessionWithFilter(sessionID, apiKeyID, exclude, nil)
}

func (h *Handler) nextAccountForSessionWithFilter(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter) (*auth.Account, string) {
	return h.nextAccountForSessionWithDispatch(sessionID, apiKeyID, exclude, filter, auth.DispatchPolicyStandard)
}

// nextAccountForSessionWithDispatch selects an account for the session with the given DispatchPolicy and returns only the account and
// proxy address. When a capacity-overflow guard is needed, use nextAccountForSessionWithDispatchGuard instead.
func (h *Handler) nextAccountForSessionWithDispatch(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string) {
	account, proxyURL, _ := h.nextAccountForSessionWithDispatchGuard(sessionID, apiKeyID, exclude, filter, auth.SelectionOptions{Policy: policy})
	return account, proxyURL
}

// nextAccountForSessionWithDispatchGuard uses the full SelectionOptions (channel + Policy) to
// select a session account, and returns the SessionAffinityGuard that is carried when capacity-overflow borrowing happens.
func (h *Handler) nextAccountForSessionWithDispatchGuard(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, opts auth.SelectionOptions) (*auth.Account, string, auth.SessionAffinityGuard) {
	if h == nil || h.store == nil {
		return nil, "", auth.SessionAffinityGuard{}
	}
	return h.store.NextForSessionWithDispatchGuardForProvider(opts, sessionID, apiKeyID, exclude, filter)
}

// selectionOptionsForRequest builds the account-selection context from the current request channel and the public model,
// and hands it to the Store to run the existing selection logic.
func selectionOptionsForRequest(c *gin.Context, model string, policy auth.DispatchPolicy) auth.SelectionOptions {
	return selectionOptionsForRequestWithProvider(c, model, policy, "")
}

// selectionOptionsForRequestWithProvider is the same as selectionOptionsForRequest, except that it uses
// forceProvider as the selection channel instead of the request channel (an empty forceProvider keeps the request channel).
func selectionOptionsForRequestWithProvider(c *gin.Context, model string, policy auth.DispatchPolicy, forceProvider string) auth.SelectionOptions {
	provider := strings.TrimSpace(forceProvider)
	if provider == "" {
		provider = requestUpstreamChannel(c)
	}
	return auth.SelectionOptions{
		Provider: provider,
		Model:    strings.TrimSpace(model),
		Policy:   policy,
	}
}

// selectionOptionsForCandidatePool builds the selection context, and the entry point declares whether to ignore the request channel
// (antigravityExcluded).
//
// When true, only Policy is kept and no Provider is set: the compact executor and native remote compaction
// candidate pools do not inherit the request channel.
func selectionOptionsForCandidatePool(c *gin.Context, model string, policy auth.DispatchPolicy, antigravityExcluded bool) auth.SelectionOptions {
	if antigravityExcluded {
		return auth.SelectionOptions{Policy: policy}
	}
	return selectionOptionsForRequest(c, model, policy)
}

func dispatchPolicyForModel(model string) auth.DispatchPolicy {
	if isProOnlyModel(model) {
		return auth.DispatchPolicySpark
	}
	return auth.DispatchPolicyStandard
}

// withModelCooldownFilter layers a cross-instance shared-cache cooldown check for a single public model on top of the existing filter,
// so that a cooldown written by another instance also filters that account out when this instance selects accounts; when the store is not wired up, the input filter is returned unchanged.
func (h *Handler) withModelCooldownFilter(ctx context.Context, model string, filter auth.AccountFilter) auth.AccountFilter {
	if h == nil || h.store == nil {
		return filter
	}
	return h.store.WithModelCooldownFilterContext(ctx, model, filter)
}

// withRequestModelCooldownFilter applies the cross-instance model cooldown filter: a shared-cache cooldown check on a single key for the public model.
func (h *Handler) withRequestModelCooldownFilter(c *gin.Context, ctx context.Context, model string, filter auth.AccountFilter) auth.AccountFilter {
	if h == nil || h.store == nil {
		return filter
	}
	return h.store.WithModelCooldownFilterContext(ctx, model, filter)
}

func (h *Handler) shouldUseWebsocketForHTTP() bool {
	if h == nil {
		return false
	}
	// The runtime DB-level switch codex_force_websocket takes priority: when on, WS is forced.
	// (kept consistent with the wantWebsocket check in ExecuteRequest; also used for the WS marker in the usage log).
	if CurrentRuntimeSettings().CodexForceWebsocket {
		return true
	}
	// The hot-reloaded value in the admin panel is likewise a source for the force switch, so that while runtime config has not yet synced
	// the UI shows it as on while the request hot path still decides by the static env=http setting.
	if h.store != nil && h.store.CodexForceWebsocket() {
		return true
	}
	if h.cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(h.cfg.CodexUpstreamTransport)) {
	case "ws":
		return true
	case "http", "auto":
		return false
	default:
		return h.cfg.UseWebsocket
	}
}

func (h *Handler) resolveProxyForAttempt(account *auth.Account, stickyProxyURL string) string {
	if h != nil && h.store != nil {
		if proxyURL := strings.TrimSpace(stickyProxyURL); proxyURL != "" && !h.store.ManagedProxyUnavailable(proxyURL) {
			return proxyURL
		}
		return h.store.ResolveProxyForAccount(account)
	}
	return strings.TrimSpace(stickyProxyURL)
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

type CodexUsageSyncResult struct {
	UsagePct7d         float64
	HasUsage7d         bool
	Usage7dRateLimited bool
	UsagePct5h         float64
	Reset5hAt          time.Time
	HasUsage5h         bool
	Used5hHeaders      bool
	Persisted5hOnly    bool
	// Premium5hRateLimited means this sync marked the account as rate limited on premium 5h.
	Premium5hRateLimited bool
	// PersistUsage means the throttling policy allows this observation to be persisted. The WS frame path may only update the in-memory
	// snapshot; the 100% cooldown marker only follows observations that are allowed to persist, to avoid rewriting the DB on every frame.
	PersistUsage bool
	// Applied means this observation passed the ordering gate and was written to the account state; the WS frame path uses it to decide
	// whether to update the "last frame" observation marker, so that a late old frame cannot pass as the newest frame.
	Applied                  bool
	UsageWindowLimitsIgnored bool
	// Cleared5h means this sync cleared the stale local 5h snapshot because the upstream did not return a 5h window (issue #382).
	Cleared5h bool
	// CreditsObserved means this sync observed upstream credits-related response headers.
	CreditsObserved bool
	// RateLimitReachedType records the rate_limit_reached_type returned by the upstream.
	RateLimitReachedType string
}

type codexRateLimitWindow string

const (
	codexRateLimitWindowUnknown codexRateLimitWindow = ""
	codexRateLimitWindowShort   codexRateLimitWindow = "short"
	codexRateLimitWindow5h      codexRateLimitWindow = "5h"
	codexRateLimitWindow7d      codexRateLimitWindow = "7d"
)

type codex429Decision struct {
	Scope    string
	Reason   string
	Model    string
	ResetAt  time.Time
	Cooldown time.Duration
}

const (
	rateLimitScopeAccount = "account"
	rateLimitScopeModel   = "model"
)

const (
	contextAPIKeyID     = "apiKeyID"
	contextAPIKeyName   = "apiKeyName"
	contextAPIKeyMasked = "apiKeyMasked"
	contextAPIKeyRow    = "apiKeyRow"
	// contextScopeBudgetGate holds the scope budget gate for this request (issue #439),
	// computed once by enforceAPIKeyLimits and reused by the account filter chain and the "no available account" branch.
	contextScopeBudgetGate = "apiKeyScopeBudgetGate"
)

func requestAPIKeyID(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	if value, exists := c.Get(contextAPIKeyID); exists && value != nil {
		switch typed := value.(type) {
		case int64:
			return typed
		case int:
			return int64(typed)
		}
	}
	return 0
}

func sessionAffinityKey(sessionID string, apiKeyID int64) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || apiKeyID <= 0 {
		return sessionID
	}
	return fmt.Sprintf("%s::api-key:%d", sessionID, apiKeyID)
}

const codexTurnStateHeader = "X-Codex-Turn-State"

// codexTurnContinuationToken follows the official Codex per-turn contract:
// HTTP sends the token as a header, while Responses WebSocket v2 sends it in
// response.create client_metadata after the first request in that turn.
func codexTurnContinuationToken(headers http.Header, body []byte) string {
	if headers != nil {
		if token := strings.TrimSpace(headers.Get(codexTurnStateHeader)); token != "" {
			return token
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String())
}

// codexWSTurnContinuationToken reads per-frame metadata only. Upgrade headers
// are connection-scoped and cannot prove that every response.create belongs to
// the same active turn.
func codexWSTurnContinuationToken(body []byte) string {
	return codexTurnContinuationToken(nil, body)
}

// applyAffinityGroupRouting keeps fingerprinted requests on the API key's original groups
// and routes requests without either a Codex engine fingerprint or the dedicated local
// affinity header to the configured split groups.
//
// When a key has no "allowed account groups" configured (= no group restriction), requests with a fingerprint switch to "all accounts except the no-affinity request group":
// Otherwise the no-affinity request group would serve both non-fingerprint requests and normal real Codex traffic, so the isolation would do nothing. And no group restriction is exactly the
// default configuration for most keys.
func applyAffinityGroupRouting(c *gin.Context, identity requestSessionIdentity, filter auth.AccountFilter) auth.AccountFilter {
	row := apiKeyRowFromContext(c)
	if row == nil || len(row.Limits.NoAffinityGroupIDs) == 0 {
		return filter
	}

	splitGroups := int64GroupSet(row.Limits.NoAffinityGroupIDs)
	if len(splitGroups) == 0 {
		return filter
	}

	if !identity.hasRequestFingerprint {
		return groupMembershipFilter(splitGroups, true, filter)
	}

	allowedGroups := int64GroupSet(row.AllowedGroupIDs)
	if len(allowedGroups) == 0 {
		// No group restriction: exclude the no-affinity request group; everything else (including ungrouped accounts) stays available.
		return groupMembershipFilter(splitGroups, false, filter)
	}
	return groupMembershipFilter(allowedGroups, true, filter)
}

func int64GroupSet(ids []int64) map[int64]struct{} {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			set[id] = struct{}{}
		}
	}
	return set
}

// groupMembershipFilter layers a group gate on top of filter: want=true requires the account to be in groups,
// want=false requires the account not to be in groups.
func groupMembershipFilter(groups map[int64]struct{}, want bool, filter auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil || account.InAnyGroup(groups) != want {
			return false
		}
		return filter == nil || filter(account)
	}
}

const proOnlySparkModel = "gpt-5.3-codex-spark"

func isProOnlyModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), proOnlySparkModel)
}

func isSparkPlanCandidate(planType string) bool {
	switch auth.NormalizePlanType(planType) {
	case "free", "api":
		return false
	default:
		return true
	}
}

// accountFilterForResponsesWebSocket returns the account filter for the Responses WebSocket entry point, which is the Codex account selection filter.
func accountFilterForResponsesWebSocket(model string) auth.AccountFilter {
	return accountFilterForModel(strings.TrimSpace(model))
}

func accountFilterForModel(model string) auth.AccountFilter {
	return accountFilterForModelWithGate(model, enforceModelRateLimits)
}

func accountFilterForModelWithGate(model string, gate modelRateLimitGate) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if gate.blocks(account, model) {
			return false
		}
		if !account.SupportsCodexModel(model) {
			return false
		}
		if isProOnlyModel(model) {
			return isSparkPlanCandidate(account.GetPlanType())
		}
		return true
	}
}

// requestUpstreamChannel returns the upstream channel restriction of the downstream key for the current request (empty = no restriction).
func requestUpstreamChannel(c *gin.Context) string {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return ""
	}
	return row.Limits.ResolveUpstreamChannel()
}

func accountFilterForResponsesModel(model string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForResponsesModelWithOriginal(model, model, allowCodexAccounts)
}

func accountFilterForResponsesModelWithOriginal(originalModel string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForResponsesModelWithGate(originalModel, effectiveModel, allowCodexAccounts, enforceModelRateLimits)
}

func accountFilterForResponsesModelWithGate(originalModel string, effectiveModel string, allowCodexAccounts bool, gate modelRateLimitGate) auth.AccountFilter {
	return accountFilterForResponsesModelCandidates([]string{originalModel, effectiveModel}, effectiveModel, allowCodexAccounts, gate)
}

func accountFilterForCompactResponsesModelWithOriginal(originalModel string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	inner := accountFilterForInlineCompactionModelWithOriginal(originalModel, effectiveModel, allowCodexAccounts)
	return func(account *auth.Account) bool {
		return account != nil && inner(account)
	}
}

func accountFilterForInlineCompactionModelWithOriginal(originalModel string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForInlineCompactionModelWithGate(originalModel, effectiveModel, allowCodexAccounts, enforceModelRateLimits)
}

func accountFilterForInlineCompactionModelWithGate(originalModel string, effectiveModel string, allowCodexAccounts bool, gate modelRateLimitGate) auth.AccountFilter {
	inner := accountFilterForResponsesModelResolver(effectiveModel, allowCodexAccounts, gate)
	return func(account *auth.Account) bool {
		return account != nil && inner(account)
	}
}

func accountFilterForResponsesModelCandidates(modelCandidates []string, effectiveModel string, allowCodexAccounts bool, gate modelRateLimitGate) auth.AccountFilter {
	return accountFilterForResponsesModelResolver(effectiveModel, allowCodexAccounts, gate)
}

func accountFilterForResponsesModelResolver(effectiveModel string, allowCodexAccounts bool, gate modelRateLimitGate) auth.AccountFilter {
	effectiveModel = strings.TrimSpace(effectiveModel)
	codexFilter := accountFilterForModelWithGate(effectiveModel, gate)
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if !allowCodexAccounts {
			return false
		}
		return codexFilter(account)
	}
}

func modelIDInList(model string, models []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range models {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

func (h *Handler) modelValidator(supportedModels []string) api.ValidationRule {
	validModels := make(map[string]bool, len(supportedModels))
	for _, model := range supportedModels {
		validModels[model] = true
	}
	return func(value gjson.Result, path string) *api.ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		model := value.String()
		if validModels[model] {
			return nil
		}
		return &api.ValidationError{
			Field:   path,
			Message: fmt.Sprintf("Model '%s' is not supported", model),
			Code:    "unsupported_model",
		}
	}
}

func effectiveRequestModel(body []byte, fallback string) string {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model != "" {
		return model
	}
	return strings.TrimSpace(fallback)
}

func noAvailableAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "no available paid or unknown-plan account; gpt-5.3-codex-spark excludes accounts explicitly marked free/api"
	}
	return "no available account, please retry later"
}

func noAvailableAccountError(model string) gin.H {
	return gin.H{
		"error": gin.H{
			"message": noAvailableAccountMessage(model),
			"type":    ErrorTypeServerError,
			"code":    ErrorCodeNoAvailableAccount,
		},
	}
}

// lastFailureModelUnsupported reports that the request's last upstream attempt
// failed with the plan-gated unsupported-model 400, which callers should pass
// through rather than replacing with no_available_account.
func lastFailureModelUnsupported(statusCode int, body []byte) bool {
	return statusCode == http.StatusBadRequest && len(body) > 0 && isCodexModelUnsupportedError(body)
}

// unsupportedModelPoolMessage explains an empty pool whose dispatchable
// accounts are all cooling down after the unsupported-model 400.
func (h *Handler) unsupportedModelPoolMessage(apiKeyID int64, exclude map[int64]bool, model string, policy auth.DispatchPolicy) (string, bool) {
	if h == nil || h.store == nil {
		return "", false
	}
	summary := h.store.UnsupportedModelCandidateSummary(apiKeyID, exclude, model, policy)
	if !summary.Found {
		return "", false
	}
	return fmt.Sprintf("The '%s' model is not supported when using Codex with a ChatGPT account. Recently all available accounts returned this error for this model; the model cooldown is set until %s. You can clear the account's model cooldown in the admin panel, or adjust 'Unsupported-model cooldown' in settings", model, summary.ResetAt.Local().Format("2006-01-02 15:04:05")), true
}

func unsupportedModelPoolError(message string) gin.H {
	return gin.H{
		"error": gin.H{
			"message": message,
			"type":    ErrorTypeInvalidRequest,
			"code":    auth.ModelCooldownReasonUnsupported,
		},
	}
}

const (
	concurrencySaturatedMessageZH = "account concurrency window is full, please retry later or raise the concurrency limit"
	concurrencySaturatedMessageEN = "Account concurrency window is full; retry later or raise the concurrency limit"
)

func concurrencySaturatedError() gin.H {
	return gin.H{
		"error": gin.H{
			"message": concurrencySaturatedMessageZH,
			"type":    ErrorTypeServerError,
			"code":    ErrorCodeAccountPoolConcurrencySaturated,
		},
	}
}

func (h *Handler) accountPoolConcurrencySaturated(apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) bool {
	if h == nil || h.store == nil {
		return false
	}
	return h.store.CapacitySaturatedCandidateSummary(apiKeyID, exclude, filter, policy).Found
}

func setConcurrencySaturatedRetryAfter(c *gin.Context) {
	if c != nil && !c.Writer.Written() {
		c.Header("Retry-After", "1")
	}
}

func usageLogErrorMessage(statusCode int, body []byte) string {
	return usageLogErrorMessageImpl(statusCode, body, false)
}

// usageLogFailureMessage records the failure diagnostics produced by the gateway itself (transport errors, stream break reasons, retry context, and so on).
// Unlike arbitrary upstream response bodies, this text is assembled by gateway code; after redaction and truncation the original text is kept. Otherwise stream-break
// errors would show only a bare status code on the usage page, and the root cause could only be found by digging through container logs (issue #524). It still tries JSON extraction first,
// because some diagnostics contain upstream error frames verbatim.
func usageLogFailureMessage(statusCode int, message string) string {
	return usageLogErrorMessageImpl(statusCode, []byte(message), true)
}

func usageLogErrorMessageImpl(statusCode int, body []byte, trustedText bool) string {
	if statusCode < 400 {
		return ""
	}

	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "response.status_details.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	}
	message := ""
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			message = candidate
			break
		}
	}
	if message == "" {
		// Some upstream error bodies put the explanation in a top-level string error field:
		// {"code":"invalid-argument","error":"..."}. Only use it when error is a string,
		// so that an object-shaped whole JSON is not put into the message.
		if errField := gjson.GetBytes(body, "error"); errField.Type == gjson.String {
			message = strings.TrimSpace(errField.String())
		}
	}

	codeCandidates := []string{
		gjson.GetBytes(body, "error.code").String(),
		gjson.GetBytes(body, "response.error.code").String(),
		gjson.GetBytes(body, "response.status_details.error.code").String(),
		gjson.GetBytes(body, "detail.code").String(),
		gjson.GetBytes(body, "code").String(),
	}
	code := ""
	for _, candidate := range codeCandidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			code = candidate
			break
		}
	}

	typeCandidates := []string{
		gjson.GetBytes(body, "error.type").String(),
		gjson.GetBytes(body, "response.error.type").String(),
		gjson.GetBytes(body, "response.status_details.error.type").String(),
		gjson.GetBytes(body, "type").String(),
	}
	errType := ""
	for _, candidate := range typeCandidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" && candidate != "error" {
			errType = candidate
			break
		}
	}

	if message == "" {
		// HTML and plain-text provider pages routinely contain request IDs,
		// internal routing details or echoed credentials. They are not an API
		// contract, so persist only the transport status instead of the body —
		// unless the caller marked the text as gateway-generated (trustedText).
		if !trustedText {
			return fmt.Sprintf("HTTP %d", statusCode)
		}
		raw := strings.TrimSpace(string(body))
		if raw == "" {
			return fmt.Sprintf("HTTP %d", statusCode)
		}
		message = raw
	}

	parts := make([]string, 0, 3)
	if code != "" {
		parts = append(parts, code)
	}
	if errType != "" && errType != code {
		parts = append(parts, errType)
	}
	parts = append(parts, message)
	return security.SafeTruncate(security.SanitizeLog(strings.Join(parts, " · ")), 600)
}

// NewHandler creates the handler
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	restoreModelCapabilities(store, db)
	handler := &Handler{
		store:      store,
		configKeys: make(map[string]bool), // no longer hard-coded, but the structure is kept for backward-compatible logic
		db:         db,
		cfg:        cfg,
		deviceCfg:  deviceCfg,
		apiKeyGate: newAPIKeyConcurrencyLimiter(),
	}
	handler.liveStore = newLiveCallStore(handler)
	return handler
}

// SetRuntimeCache wires Redis/Memory runtime cache for hot auth metadata.
func (h *Handler) SetRuntimeCache(tc cache.TokenCache) {
	if h == nil {
		return
	}
	h.cache = tc
	if h.authCache != nil {
		h.authCache.close()
		h.authCache = nil
	}
	if h.cfg != nil && h.cfg.APIKeyAuthCacheEnabled && h.db != nil {
		h.authCache = newAPIKeyAuthCache(h.db, tc)
	}
}

// NewHandlerWithDeviceProfile creates the handler (with device fingerprint config)
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, nil, deviceCfg)
}

// resolveAPIKey resolves the downstream API key. The return values distinguish three outcomes:
//   - (row, true, nil): a valid key matched
//   - (nil, false, nil): confirmed that no such key exists (respond 401)
//   - (nil, false, err): transient DB/infrastructure failure (respond 503, not falsely report the key as invalid)
//
// Key point: a transient failure such as "database connections exhausted/timed out" must never be treated as "client key invalid"
// returning 401, otherwise during load tests or DB jitter clients would wrongly think their credentials are invalid (issue #323).
func (h *Handler) resolveAPIKeyUnshared(key string) (*database.APIKeyRow, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, nil
	}
	if h.configKeys[key] {
		return &database.APIKeyRow{
			ID:      0,
			Name:    "config",
			Key:     key,
			Enabled: true,
		}, true, nil
	}
	if row, ok := h.resolveAPIKeyFromRuntimeCache(key); ok {
		h.syncAPIKeyAllowedGroups(row)
		return row, true, nil
	}
	if h.db == nil {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		// DB failure (connections exhausted/timeout/network): report the error so the caller returns 503, rather than treating it as an invalid key.
		log.Printf("failed to query API key: %v", err)
		return nil, false, err
	}
	h.setAPIKeyRuntimeCache(row)
	h.syncAPIKeyAllowedGroups(row)
	return row, true, nil
}

func (h *Handler) resolveAPIKeyFromRuntimeCache(key string) (*database.APIKeyRow, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	raw, ok, err := h.cache.GetRuntime(ctx, apiKeyCacheNamespace, key)
	if err != nil {
		log.Printf("failed to read API key Redis cache: %v", err)
		return nil, false
	}
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var record apiKeyRuntimeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		log.Printf("failed to parse API key Redis cache: %v", err)
		return nil, false
	}
	if record.ID <= 0 {
		return nil, false
	}
	// The runtime cache only holds keys without any access restrictions (including enabled=true), so backfilling here is safe.
	return &database.APIKeyRow{
		ID:        record.ID,
		Name:      record.Name,
		Key:       key,
		Enabled:   true,
		CreatedAt: record.CreatedAt,
	}, true
}

func (h *Handler) setAPIKeyRuntimeCache(row *database.APIKeyRow) {
	if h == nil || h.cache == nil || row == nil || strings.TrimSpace(row.Key) == "" || row.ID <= 0 {
		return
	}
	if row.HasAccessConstraints() {
		return
	}
	record := apiKeyRuntimeRecord{
		ID:        row.ID,
		Name:      row.Name,
		CreatedAt: row.CreatedAt,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.cache.SetRuntime(ctx, apiKeyCacheNamespace, row.Key, payload, apiKeyCacheTTL); err != nil {
		log.Printf("failed to write API key Redis cache: id=%d err=%v", row.ID, err)
	}
}

func (h *Handler) syncAPIKeyAllowedGroups(row *database.APIKeyRow) {
	if h == nil || h.store == nil || row == nil || row.ID <= 0 {
		return
	}
	h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
	h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
	h.store.SetAPIKeyAllowedPlans(row.ID, row.Limits.PlanAllow)
	h.store.SetAPIKeyUpstreamChannel(row.ID, row.Limits.ResolveUpstreamChannel())
}

// isValidKey checks whether the key is valid (config file + DB). On DB failure it conservatively returns false.
func (h *Handler) isValidKey(key string) bool {
	_, ok, _ := h.resolveAPIKey(key)
	return ok
}

// hasAnyKeys checks whether any key is configured
func (h *Handler) hasAnyKeys() bool {
	configured, err := h.hasAnyKeysWithError()
	return err == nil && configured
}

func (h *Handler) hasAnyKeysWithError() (bool, error) {
	if len(h.configKeys) > 0 {
		return true, nil
	}
	if h.authCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for i := 0; i < 3; i++ {
			state, _, err := h.authCache.revision(ctx)
			if errors.Is(err, errAPIKeyAuthRetry) {
				continue
			}
			return state.KeyCount > 0, err
		}
		return false, errAPIKeyAuthRetry
	}
	if h.cache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		raw, ok, err := h.cache.GetRuntime(ctx, apiKeyCountCacheNamespace, "all")
		cancel()
		if err != nil {
			log.Printf("failed to read API key count cache: %v", err)
		} else if ok {
			var record apiKeyCountRuntimeRecord
			if err := json.Unmarshal(raw, &record); err == nil {
				return record.Count > 0, nil
			}
		}
	}
	if h.db == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count, err := h.db.CountAPIKeys(ctx)
	if err != nil {
		log.Printf("failed to count API keys: %v", err)
		return false, err
	}
	if h.cache != nil {
		payload, _ := json.Marshal(apiKeyCountRuntimeRecord{Count: count})
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		if err := h.cache.SetRuntime(cacheCtx, apiKeyCountCacheNamespace, "all", payload, apiKeyCountCacheTTL); err != nil {
			log.Printf("failed to write API key count cache: %v", err)
		}
		cacheCancel()
	}
	return count > 0, nil
}

// logUsage records the request log (non-blocking; written to an in-memory buffer that the background flushes in batches)
func (h *Handler) logUsage(input *database.UsageLogInput) {
	if h.db == nil || input == nil {
		return
	}
	input = database.SnapshotUsageLogBilling(input)
	// The scope-dimension budget (issue #439) consumes this spend before the log is persisted, offsetting the lag of the window aggregate cache.
	h.recordAPIKeyScopeUsage(input)
	// The channel is fixed to codex at write time, for per-channel aggregation on the dashboard.
	if input.Channel == "" && h.store != nil {
		input.Channel = database.UpstreamChannelCodex
	}
	// Overload circuit-breaker stats (Codex channel only; must come after the channel is fixed).
	h.noteOverloadOutcome(input)
	_ = h.db.InsertUsageLog(context.Background(), input)
}

func populateAPIKeyMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func populateInternalUsageMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if value, exists := c.Get(contextInternalReason); exists {
		input.InternalReason, _ = value.(string)
	}
	if value, exists := c.Get(contextParentRequestID); exists {
		input.ParentRequestID, _ = value.(string)
	}
}

func (h *Handler) logUsageForRequest(c *gin.Context, input *database.UsageLogInput) {
	applyDaybreakUsageModel(c, input)
	populateAPIKeyMetaFromContext(c, input)
	populateInternalUsageMetaFromContext(c, input)
	populateClientIPFromRequest(c, input)
	populateUserAgentMetaFromRequest(c, input)
	populateWsAcquireFromRequest(c, input)
	populateUpstreamTrace(c, input)
	populateCompactUsageMetaFromRequest(c, input)
	populateUltraUsageMetaFromRequest(c, input)
	markCyberPolicyUsageKind(input)
	input = database.SnapshotUsageLogBilling(input)
	if deferImageUsage(c, h, input) {
		return
	}
	h.logUsage(input)
}

// logContinueThinkingRounds back-fills the real usage of the upstream rounds that were "folded and hidden" in the continue-thinking-after-truncation flow.
// Each continue-thinking round is a separate upstream request, each producing real token consumption; the client sees it folded into a single response
// afterwards, the usage of the final successful round is booked in one place by this attempt's wrap-up, so here we back-fill the other rounds besides the final successful round
// (res.Rounds except the last entry) and the failed continuation opening round (res.FailedContinuation),
// so that the booked consumption matches the actual number of upstream requests, without double-billing against the wrap-up booking.
func (h *Handler) logContinueThinkingRounds(c *gin.Context, res continueFoldResult, account *auth.Account, logModel, logEffectiveModel, reasoningEffort string, useWebsocket bool, requestedServiceTier string) {
	logRound := func(round continueRoundStat) {
		usageTiers := resolveUsageServiceTiers("", requestedServiceTier)
		statusCode := round.StatusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		logInput := &database.UsageLogInput{
			AccountID:            account.ID(),
			Endpoint:             "/v1/responses",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           statusCode,
			DurationMs:           round.DurationMs,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/responses",
			UpstreamEndpoint:     "/v1/responses",
			Stream:               true,
			ViaWebsocket:         useWebsocket,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
			// A hidden continue-thinking round is not a "retry": do not set IsRetryAttempt/AttemptIndex,
			// otherwise it would pollute the retry stats and be confused with the outer attempt numbers.
		}
		if round.ErrMessage != "" {
			logInput.ErrorMessage = usageLogFailureMessage(statusCode, round.ErrMessage)
			logInput.UpstreamErrorKind = "continue_thinking_error"
		}
		round.Trace.apply(logInput)
		if round.Usage != nil {
			logInput.PromptTokens = round.Usage.PromptTokens
			logInput.CompletionTokens = round.Usage.CompletionTokens
			logInput.TotalTokens = round.Usage.TotalTokens
			logInput.InputTokens = round.Usage.InputTokens
			logInput.OutputTokens = round.Usage.OutputTokens
			logInput.ReasoningTokens = round.Usage.ReasoningTokens
			logInput.CachedTokens = round.Usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)
	}

	// The last entry in res.Rounds is the final successful round, whose usage is booked by this attempt's wrap-up, so it is excluded here.
	for i := 0; i+1 < len(res.Rounds); i++ {
		logRound(res.Rounds[i])
	}
	if res.FailedContinuation != nil {
		logRound(*res.FailedContinuation)
	}
}

// markCyberPolicyUsageKind marks cyber_policy errors in the usage log as their own cyber_policy
// type, so the "usage stats" page can recognize them and click through to the trigger details. It only rewrites the log display field and does not take part in
// account scheduling / cooldown scoring (that path uses a different failureKind).
func markCyberPolicyUsageKind(input *database.UsageLogInput) {
	if input == nil || input.UpstreamErrorKind == "cyber_policy" {
		return
	}
	msg := strings.ToLower(input.ErrorMessage)
	if strings.Contains(msg, "cyber_policy") || strings.Contains(msg, "cyber security risk") {
		input.UpstreamErrorKind = "cyber_policy"
	}
}

func populateClientIPFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || strings.TrimSpace(input.ClientIP) != "" {
		return
	}
	clientIP := strings.TrimSpace(c.ClientIP())
	if clientIP == "" && c.Request != nil {
		clientIP = strings.TrimSpace(c.Request.RemoteAddr)
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}
	}
	if len(clientIP) > 64 {
		clientIP = clientIP[:64]
	}
	input.ClientIP = clientIP
}

func populateCompactUsageMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if input == nil {
		return
	}

	meta, ok := cachedRequestCompactionMeta(c)
	if !ok {
		if body, bodyOK := rawRequestBodyFromContext(c); bodyOK {
			meta = requestBodyCompactionMeta(body)
		}
		// HTTP requests may carry the same per-turn metadata as a request header.
		// Client WebSocket turns always cache frame-local metadata before logging,
		// so the Upgrade request header cannot leak into individual frames here.
		if c != nil && c.Request != nil && turnMetadataIndicatesCompaction(c.GetHeader("X-Codex-Turn-Metadata")) {
			meta.UsageTriggered = true
		}
	}

	// Only the original inbound path can mark an explicit Compact request.
	// UpstreamEndpoint may be rewritten internally and must not affect this signal.
	if isExplicitCompactUsageRequest(c, input) {
		meta.UsageTriggered = true
	}

	if meta.UsageTriggered {
		input.Compact = true
	}
	if meta.HasHistory {
		input.HasCompactionHistory = true
	}
}

func isExplicitCompactUsageRequest(c *gin.Context, input *database.UsageLogInput) bool {
	if c != nil && c.Request != nil && c.Request.URL != nil {
		return isCompactUsageEndpoint(c.Request.URL.Path)
	}
	if input == nil {
		return false
	}
	return isCompactUsageEndpoint(input.InboundEndpoint) || isCompactUsageEndpoint(input.Endpoint)
}

func isCompactUsageEndpoint(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	if cut := strings.IndexAny(endpoint, "?#"); cut >= 0 {
		endpoint = endpoint[:cut]
	}
	endpoint = strings.TrimRight(endpoint, "/")
	return endpoint == "/v1/responses/compact"
}

func rawRequestBodyFromContext(c *gin.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	v, exists := c.Get("raw_body")
	if !exists || v == nil {
		return nil, false
	}
	switch body := v.(type) {
	case []byte:
		if len(body) == 0 {
			return nil, false
		}
		return body, true
	case string:
		if body == "" {
			return nil, false
		}
		return []byte(body), true
	default:
		return nil, false
	}
}

func readRawRequestBody(c *gin.Context) ([]byte, error) {
	if body, ok := rawRequestBodyFromContext(c); ok {
		return body, nil
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	setRawRequestBody(c, body)
	return body, nil
}

const (
	maxResponsesInputItems    = 20000
	maxChatCompletionMessages = 20000
)

// rejectExcessiveJSONArray bounds item counts before the body is unmarshaled
// into maps. A 24MB body containing hundreds of thousands of tiny objects can
// expand into hundreds of MB during parsing; the request memory budget only
// counts raw bytes.
func rejectExcessiveJSONArray(c *gin.Context, rawBody []byte, path string, max int) bool {
	if c == nil || len(rawBody) == 0 || max <= 0 {
		return false
	}
	if count := gjson.GetBytes(rawBody, path+".#").Int(); count > int64(max) {
		api.SendError(c, api.NewAPIError(
			api.ErrCodeInvalidRequest,
			fmt.Sprintf("%s contains too many items (max %d)", path, max),
			api.ErrorTypeInvalidRequest,
		))
		return true
	}
	return false
}

const ingressRequestBodyContextKey = "ingress_raw_body"

func setIngressRequestBodyIfAbsent(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	if value, exists := c.Get(ingressRequestBodyContextKey); exists && value != nil {
		return
	}
	// The request-size middleware already owns this immutable buffer for the
	// request lifetime. Retain the same slice instead of copying the full body.
	c.Set(ingressRequestBodyContextKey, body)
}

func ingressRequestBody(c *gin.Context, fallback []byte) []byte {
	if c != nil {
		if value, exists := c.Get(ingressRequestBodyContextKey); exists {
			if body, ok := value.([]byte); ok {
				return body
			}
		}
	}
	return fallback
}

func setRawRequestBody(c *gin.Context, body []byte) {
	if c != nil {
		c.Set("raw_body", body)
	}
}

const requestCompactionMetaContextKey = "request_compaction_meta"

type requestCompactionMeta struct {
	// ProtocolTriggered is the wire-level compaction_trigger control. Only this
	// value may affect routing, account pinning, or protocol-specific timeouts.
	ProtocolTriggered bool
	// UsageTriggered is the observability signal persisted in usage_logs.compact.
	UsageTriggered bool
	// HasHistory marks direct durable compaction/context_compaction input items.
	HasHistory bool
}

func cacheRequestCompactionMeta(c *gin.Context, meta requestCompactionMeta) {
	if c != nil {
		c.Set(requestCompactionMetaContextKey, meta)
	}
}

func cachedRequestCompactionMeta(c *gin.Context) (requestCompactionMeta, bool) {
	if c == nil {
		return requestCompactionMeta{}, false
	}
	value, exists := c.Get(requestCompactionMetaContextKey)
	if !exists {
		return requestCompactionMeta{}, false
	}
	meta, ok := value.(requestCompactionMeta)
	return meta, ok
}

func requestCompactionMetaForHTTP(c *gin.Context, body []byte) requestCompactionMeta {
	meta := requestBodyCompactionMeta(body)
	if c != nil && c.Request != nil {
		if turnMetadataIndicatesCompaction(c.GetHeader("X-Codex-Turn-Metadata")) {
			meta.UsageTriggered = true
		}
		if c.Request.URL != nil && isCompactUsageEndpoint(c.Request.URL.Path) {
			meta.UsageTriggered = true
		}
	}
	return meta
}

// requestBodyCompactionMeta inspects only direct input items plus the canonical
// per-turn metadata field. It deliberately does not recurse into messages,
// content, tool output, or arbitrary stringified JSON.
func requestBodyCompactionMeta(body []byte) requestCompactionMeta {
	meta := requestCompactionMeta{}
	input := jsonbody.GetView(body, "input")
	inspect := func(item gjson.Result) {
		switch {
		case gjsonResultIsCompactionTrigger(item):
			meta.ProtocolTriggered = true
		case gjsonResultIsCompactionHistory(item):
			meta.HasHistory = true
		}
	}
	if input.Exists() {
		if input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				inspect(item)
				return true
			})
		} else {
			inspect(input)
		}
	}

	meta.UsageTriggered = meta.ProtocolTriggered
	clientMetadata := gjson.GetBytes(body, "client_metadata")
	if clientMetadata.IsObject() {
		turnMetadata := clientMetadata.Get("x-codex-turn-metadata")
		if turnMetadata.Type == gjson.String && turnMetadataIndicatesCompaction(turnMetadata.String()) {
			meta.UsageTriggered = true
		}
	}
	return meta
}

func turnMetadataIndicatesCompaction(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) {
		return false
	}
	root := gjson.Parse(raw)
	if !root.IsObject() {
		return false
	}
	requestKind := root.Get("request_kind")
	return requestKind.Type == gjson.String &&
		strings.EqualFold(strings.TrimSpace(requestKind.String()), "compaction")
}

// requestBodyHasCompactionTrigger reports only the protocol-level control used
// for routing. Metadata-only local compaction turns must remain on /responses.
func requestBodyHasCompactionTrigger(body []byte) bool {
	return requestBodyCompactionMeta(body).ProtocolTriggered
}

// storeHasAvailableCodexAccount checks whether the account pool still has a schedulable Codex account.
// This is a pool-level check and does not include the API key-level account group/plan restrictions. (issue #540)
func (h *Handler) storeHasAvailableCodexAccount() bool {
	if h == nil || h.store == nil {
		return false
	}
	for _, account := range h.store.Accounts() {
		if account.IsAvailable() {
			return true
		}
	}
	return false
}

// responsesContinuationUnavailableFilter rejects every account: no local snapshot exists for the previous_response_id, and no upstream keeps one for us, so the request must end with the context-unavailable error.
func responsesContinuationUnavailableFilter(auth.AccountFilter) auth.AccountFilter {
	return func(*auth.Account) bool { return false }
}

func responseCachePreparationFailure(prepared responsesBodyPreparation) (status int, reason string, unavailable bool) {
	if prepared.PreviousResponseID == "" {
		return 0, "", false
	}
	if prepared.InputReplayConflict {
		return http.StatusConflict, "ambiguous_tool_history", true
	}
	if prepared.CacheLookup.Kind == responseCacheLookupHit {
		return 0, "", false
	}
	switch prepared.CacheLookup.Kind {
	case responseCacheLookupKnownEvicted:
		return http.StatusConflict, "local_context_evicted", true
	case responseCacheLookupKnownOversize:
		return http.StatusConflict, "local_context_oversize", true
	case responseCacheLookupReconstructionTooLarge:
		return http.StatusConflict, "reconstruction_too_large", true
	case responseCacheLookupBackendCorrupt:
		return http.StatusConflict, "backend_value_corrupt", true
	case responseCacheLookupBackendError:
		if prepared.RequiresLocalContext {
			return http.StatusServiceUnavailable, "backend_unavailable", true
		}
	case responseCacheLookupMiss, responseCacheLookupExpired:
		if prepared.RequiresLocalContext {
			return http.StatusConflict, "missing_required_call_context", true
		}
	}
	return 0, "", false
}

// sendResponseContextUnavailable rejects a continuation whose parent context
// cannot be proven. The 409 uses OpenAI's previous_response_not_found code so a
// client resends the full conversation instead of retrying the same parent.
func sendResponseContextUnavailable(c *gin.Context, status int, reason string, prepared responsesBodyPreparation) {
	code := api.ErrCodeResponseContextUnavailable
	errType := api.ErrorTypeInvalidRequest
	message := responseContextUnavailableMessage
	if status == http.StatusServiceUnavailable {
		code = api.ErrCodeServiceUnavailable
		errType = api.ErrorTypeServer
		message = "Previous response context backend is temporarily unavailable"
	}
	if status == http.StatusConflict {
		recordResponseCacheKnownUnavailableError()
	}
	logResponseContextUnavailable(c, status, reason, prepared)
	api.SendErrorWithStatus(c, api.NewAPIErrorWithDetails(
		code,
		message,
		errType,
		api.ErrorDetail{Field: "previous_response_id", Message: reason},
	), status)
}

func gjsonResultIsCompactionTrigger(result gjson.Result) bool {
	return result.IsObject() && strings.EqualFold(strings.TrimSpace(result.Get("type").String()), "compaction_trigger")
}

func gjsonResultIsCompactionHistory(result gjson.Result) bool {
	if !result.IsObject() {
		return false
	}
	itemType := strings.TrimSpace(result.Get("type").String())
	return strings.EqualFold(itemType, "compaction") || strings.EqualFold(itemType, "context_compaction")
}

// extractReasoningEffort extracts the reasoning effort from the request body
// Supports reasoning.effort (Responses API) and reasoning_effort (Chat Completions API)
func extractReasoningEffort(body []byte) string {
	// Responses API: reasoning.effort
	if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "" {
		return effort
	}
	// Chat Completions API: reasoning_effort
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" {
		return effort
	}
	return ""
}

// responsesPhaseTimingHeader is the response header for the per-phase timing of the /v1/responses request preparation stage.
// Written before the first attempt starts (the SSE first byte has not been sent yet), so the downstream gateway can use it to attribute
// the gap between "slow first token on the gateway side" and "fast codex2api first_token_ms" to a specific phase (issue #405).
const responsesPhaseTimingHeader = "X-Codex2API-Phase-Timing"

// emitResponsesPhaseTimings emits the per-phase timings before the first attempt of /v1/responses.
// Meaning of each phase: mw=middleware chain before entering the handler (including body cache/decompression/auth),
// read=reading the request body inside the handler, validate=model mapping and request validation,
// prepare=rebuilding the upstream request body (Unmarshal→map→Marshal), schedule=key rate-limit check and account scheduling.
// Time after the attempt starts is covered by the existing first_token_ms; adding the two gives the whole handler duration.
func emitResponsesPhaseTimings(c *gin.Context, logModel string, bodySize int, handlerStart, bodyReadDone, validateDone, prepareDone time.Time) {
	now := time.Now()
	middlewareMs := int64(0)
	if reqCtx := api.GetRequestContext(c); reqCtx != nil && !reqCtx.StartTime.IsZero() {
		middlewareMs = handlerStart.Sub(reqCtx.StartTime).Milliseconds()
	}
	summary := fmt.Sprintf("mw=%d;read=%d;validate=%d;prepare=%d;schedule=%d;body_kb=%d",
		middlewareMs,
		bodyReadDone.Sub(handlerStart).Milliseconds(),
		validateDone.Sub(bodyReadDone).Milliseconds(),
		prepareDone.Sub(validateDone).Milliseconds(),
		now.Sub(prepareDone).Milliseconds(),
		bodySize/1024)
	c.Header(responsesPhaseTimingHeader, summary)
	log.Printf("[TIMING] /v1/responses model=%s %s", logModel, summary)
}

// extractServiceTier extracts the service tier from the request body
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

const upstreamErrorKindMessageTooBig = "message_too_big"

// upstreamErrorKindWsBusyAcquire is a wsrelay busy session acquire timeout (issue #413):
// a queueing timeout caused by the previous request in the same session holding the WS connection for a long time; it is session occupancy, not an account failure.
const upstreamErrorKindWsBusyAcquire = "ws_busy_acquire"

// isWsBusyAcquireTimeoutError recognizes a busy acquire timeout by its error text. wsrelay depends on proxy,
// so proxy cannot import its sentinel type in reverse, and across packages it can only match on a stable error message fragment.
func isWsBusyAcquireTimeoutError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "waiting for busy session")
}

// shouldPenalizeTransportKind returns whether this transport failure should count toward the account health penalty.
// An account that hit a busy acquire timeout has no fault of its own; penalizing it would wrongly down-weight it in the fast scheduler (issue #413).
func shouldPenalizeTransportKind(kind string) bool {
	return kind != "" && kind != upstreamErrorKindWsBusyAcquire
}

func isWebsocketMessageTooBigError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "message too big") ||
		strings.Contains(msg, "close 1009") ||
		strings.Contains(msg, "read limit exceeded")
}

func websocketMessageTooBigSource(message string) string {
	if strings.Contains(strings.ToLower(message), "read limit exceeded") {
		return "local_read_limit"
	}
	return "peer_close"
}

func isWebsocketMessageTooBigOutcome(outcome streamOutcome) bool {
	return outcome.failureKind == upstreamErrorKindMessageTooBig
}

func shouldFallbackWebsocketMessageTooBigToHTTP(outcome streamOutcome, useWebsocket bool, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if !useWebsocket || !isWebsocketMessageTooBigOutcome(outcome) {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	// Structured proxy errors already carry their retry semantics. Treating
	// every non-nil error as a transport failure turns deterministic adapter
	// errors (for example an invalid Responses feature) into account failures,
	// retries them against the whole pool, and can leave the caller with a
	// misleading "no available account" 503. Only unwrap errors that actually
	// describe an upstream transport failure; ordinary structured 4xx errors
	// are not transport incidents.
	var apiErr *Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case ErrorCodeUpstreamTimeout:
			return "timeout"
		case ErrorCodeUpstreamStreamBreak:
			return "transport"
		}
		// Executors wrap http.Client.Do failures as ErrUpstream(0, ..., cause).
		// Those stay outside the legacy Retryable status set, but the cause is
		// still a transport incident and must keep sticky/same-account retry.
		if apiErr.Code == ErrorCodeUpstreamError && apiErr.HTTPStatus == 0 && apiErr.Cause != nil {
			return classifyTransportFailure(apiErr.Cause)
		}
		if !apiErr.Retryable || (apiErr.HTTPStatus >= 400 && apiErr.HTTPStatus < 500) {
			return ""
		}
		if apiErr.Cause != nil {
			return classifyTransportFailure(apiErr.Cause)
		}
		return ""
	}

	if isWebsocketMessageTooBigError(err) {
		return upstreamErrorKindMessageTooBig
	}
	if isWsBusyAcquireTimeoutError(err) {
		return upstreamErrorKindWsBusyAcquire
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "transport"
}

func classifyHTTPFailure(statusCode int, body []byte) string {
	switch {
	case statusCode == http.StatusBadRequest && isCodexModelUnsupportedError(body):
		return "" // the plan does not support this model; only apply an (account, model) cooldown and do not count toward account health
	case statusCode == http.StatusUnauthorized:
		return "unauthorized"
	case statusCode == http.StatusTooManyRequests:
		return "" // 429 is handled separately by applyCooldown
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

type streamOutcome struct {
	logStatusCode  int
	failureKind    string
	failureMessage string
	failurePayload []byte // raw response.failed/error frame, used only for policy matching and not echoed directly to the log
	penalize       bool
	// capacityShed marks this as an upstream capacity shed (server_is_overloaded / slow_down).
	// penalize stays true to reuse the pre-first-packet frame buffering/transparent retry mechanism, but the account's consecutive-failure streak/health is not penalized because of this
	// and transparent retry prefers backing off a few times on the same account rather than switching accounts immediately (see reportStreamOutcomeFailure).
	capacityShed bool
	// verifyAccountAuth marks this as a WS upstream read-stream failure (e.g. close 1008 policy violation).
	// On the WS channel, an invalid token shows up as the upstream closing the connection rather than as a 401, so a probe must run asynchronously to confirm the account's auth status,
	// and the account is cooled down as unauthorized only on a 401, so that invalid accounts are not left unbanned and repeatedly scheduled.
	verifyAccountAuth bool
	// requestScoped marks a failure as triggered by this request's content (e.g. a zero-output response.incomplete):
	// penalize stays true to reuse the pre-first-packet transparent retry/account switch, but it does not count toward account health.
	requestScoped bool
	// modelUnsupported marks a 400 where the plan does not support the model: penalize stays true to retry on another account,
	// but only apply an (account, model) cooldown and do not count toward account health.
	modelUnsupported bool
	// terminalLocal marks proxy replay/storage failures; they never affect
	// upstream retry or account health and must exit as protocol terminal errors.
	// terminalLocal marks replay/storage failures of the proxy itself; they take no part in upstream retries or account health,
	// and they must end with a protocol terminal error.
	terminalLocal bool
	// guardTripped marks that this request was forcibly terminated by the model output runaway guard.
	guardTripped bool
	// guardReason freezes the first trigger reason (guaranteed by the termination coordinator), used only for the usage log and the redacted terminal state.
	guardReason string
}

// OverlayGuardOutcome absolutely overrides the existing outcome with the guard terminal state: when the output runaway guard is hit,
// stream-break classes such as 499/598 must no longer mask the 502 terminal state. failurePayload is set to nil, to prevent the upstream raw frames already
// judged runaway from being carried into policy matching or log echo; penalize=false ensures the request is not recorded
// as an upstream account failure when the gateway actively terminated it.
//
// Besides penalize, capacityShed / modelUnsupported / requestScoped /
// verifyAccountAuth, these classification markers that change downstream branches must also be cleared: they respectively correspond to
// capacity-shed retry, switching accounts for unsupported models, request-level failure attribution and the WS auth probe; if any one is left set,
// the guard terminal state would be handled as an ordinary upstream failure, breaking the "absolute priority" semantics.
func OverlayGuardOutcome(outcome streamOutcome, guard *outputguard.RequestGuard) streamOutcome {
	if guard == nil || !guard.IsTripped() {
		return outcome
	}
	outcome.guardTripped = true
	outcome.guardReason = guard.TrippedReason()
	outcome.logStatusCode = http.StatusBadGateway
	outcome.failureKind = "model_output_guard"
	outcome.failureMessage = GuardTriggeredPublicMessage
	outcome.failurePayload = nil
	outcome.penalize = false
	outcome.capacityShed = false
	outcome.modelUnsupported = false
	outcome.requestScoped = false
	outcome.verifyAccountAuth = false
	outcome.terminalLocal = true
	return outcome
}

const continuousRetryLocalFailureMessage = "Internal proxy replay failure"

// isContinuousRetryLocalFailure recognizes only replay-owned failures, keeping
// sentinel identity stable while filesystem details remain private.
// isContinuousRetryLocalFailure only recognizes failures of the replay layer itself; the sentinel identity stays stable,
// and filesystem details never enter the public protocol errors.
func isContinuousRetryLocalFailure(err error) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{
		errContinuousRetryReplayClosed,
		errContinuousRetryReplayLimitExceeded,
		errContinuousRetryReplayStorage,
		errContinuousRetryWSReplayInvalid,
		errContinuousRetryWSMessageTooLarge,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

func overlayContinuousRetryLocalFailure(outcome streamOutcome, errs ...error) streamOutcome {
	if outcome.terminalLocal || strings.EqualFold(strings.TrimSpace(outcome.failureKind), "continuous_retry_timeout") {
		return outcome
	}
	for _, err := range errs {
		if !isContinuousRetryLocalFailure(err) {
			continue
		}
		return streamOutcome{
			logStatusCode:  http.StatusInternalServerError,
			failureKind:    "local",
			failureMessage: continuousRetryLocalFailureMessage,
			penalize:       false,
			terminalLocal:  true,
		}
	}
	return outcome
}

// isWebsocketUpstreamClose checks whether a read-stream error came from an abnormal WS upstream close or read failure.
// wsrelay wraps its read errors uniformly with the "websocket read error:" prefix (see wsrelay/executor.go).
func isWebsocketUpstreamClose(err error) bool {
	return err != nil && strings.Contains(err.Error(), "websocket read error")
}

func classifyStreamOutcome(ctxErr, readErr, writeErr error, gotTerminal bool) streamOutcome {
	if errors.Is(ctxErr, errContinuousRetryDeadlineExceeded) {
		return streamOutcome{
			logStatusCode:  http.StatusGatewayTimeout,
			failureKind:    "continuous_retry_timeout",
			failureMessage: continuousRetryTimeoutMessage,
			penalize:       false,
		}
	}
	if local := overlayContinuousRetryLocalFailure(streamOutcome{}, readErr, writeErr); local.terminalLocal {
		return local
	}
	if gotTerminal {
		return streamOutcome{logStatusCode: http.StatusOK}
	}

	if ctxErr != nil || writeErr != nil {
		msg := "downstream client disconnected early"
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			msg = "downstream request context timed out"
		case writeErr != nil:
			msg = fmt.Sprintf("failed to write back to downstream: %v", writeErr)
		case ctxErr != nil:
			msg = fmt.Sprintf("downstream request canceled early: %v", ctxErr)
		}
		return streamOutcome{
			logStatusCode:  logStatusClientClosed,
			failureMessage: msg,
		}
	}

	if readErr != nil {
		kind := classifyTransportFailure(readErr)
		if kind == "" {
			kind = "transport"
		}
		messageTooBig := kind == upstreamErrorKindMessageTooBig
		return streamOutcome{
			logStatusCode:     logStatusUpstreamStreamBreak,
			failureKind:       kind,
			failureMessage:    fmt.Sprintf("failed to read upstream stream: %v", readErr),
			penalize:          !messageTooBig,
			verifyAccountAuth: !messageTooBig && isWebsocketUpstreamClose(readErr),
		}
	}

	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream stream ended early without a terminal event",
		penalize:       true,
	}
}

func classifyResponseFailedOutcome(payload []byte) streamOutcome {
	statusCode := responseFailedStatusCode(payload)
	errorBody := responseFailedErrorBody(payload)
	permanentQuota := isPermanentQuotaFailure(errorBody)
	safetyPolicy := isExplicitUpstreamSafetyPolicy(payload)
	cyberPolicy := isExplicitUpstreamCyberPolicy(payload)
	if permanentQuota {
		// Relays do not consistently include status_code on quota failures. Keep
		// them on the independent 429 budget and permanently exclude the account
		// from this request instead of admitting it to continuous 5xx cycles.
		statusCode = http.StatusTooManyRequests
	}
	if safetyPolicy {
		// Provider policy refusals are deterministic for this request. Some relays
		// report them with a synthetic 500, which must not inherit the legacy
		// finite 5xx retry path or penalize otherwise healthy accounts.
		statusCode = http.StatusBadRequest
	}
	message := usageLogErrorMessage(statusCode, payload)
	if cyberPolicy {
		// Upstream response.failed events often omit status_code, whose generic
		// fallback is 500. CYB is a deterministic request-policy rejection: expose
		// it as 400 and never rotate accounts/retry the same user request.
		statusCode = http.StatusBadRequest
		message = upstreamCyberPolicyUserMessage
	}
	if strings.TrimSpace(message) == "" || message == fmt.Sprintf("HTTP %d", statusCode) {
		message = "upstream returned response.failed"
	}
	kind := upstreamErrorKind(statusCode, errorBody, codex429Decision{})
	if permanentQuota {
		kind = "usage_limit"
	}
	if cyberPolicy {
		kind = "cyber_policy"
	} else if safetyPolicy {
		kind = "safety_policy"
	}
	if kind == "" {
		if statusCode >= 500 {
			kind = "server"
		} else {
			kind = "client"
		}
	}
	// A zero-output response.incomplete is rewritten by the gateway into response.failed (see
	// codex_empty_incomplete.go): it goes through transparent retry as a 502, but the account is not penalized.
	emptyIncomplete := isEmptyIncompleteFailurePayload(payload)
	if emptyIncomplete {
		kind = codexEmptyIncompleteFailureKind
	}
	// A 400 with "account does not support this model" is an account entitlement issue; cooling down and retrying on another account makes sense, so it is treated as a retryable failure.
	modelUnsupported := statusCode == http.StatusBadRequest && isCodexModelUnsupportedError(errorBody)
	return streamOutcome{
		logStatusCode:    statusCode,
		failureKind:      kind,
		failureMessage:   message,
		failurePayload:   append([]byte(nil), payload...),
		penalize:         !safetyPolicy && (statusCode == http.StatusUnauthorized || statusCode == http.StatusTooManyRequests || statusCode >= 500 || modelUnsupported),
		capacityShed:     !capacityShedHandlingDisabled() && isCapacityShedPayload(payload),
		requestScoped:    emptyIncomplete,
		modelUnsupported: modelUnsupported,
	}
}

// reportStreamOutcomeFailure reports in-stream response.failed-type failures to account health, but capacity shedding
// is an exception: it is a request-level transient signal bucketed by model/identity capacity, switching accounts does not change the factor causing the shed, and counting it toward the streak would only
// down-weight the whole account pool one by one during peak hours. Skipping the report means the "soft signal is kept, hard penalty removed".
func (h *Handler) reportStreamOutcomeFailure(account *auth.Account, outcome streamOutcome, d time.Duration) {
	if outcome.capacityShed || outcome.requestScoped || outcome.modelUnsupported {
		return
	}
	h.store.ReportRequestFailure(account, outcome.failureKind, d)
}

// unbindOrRetainAffinityForCapacityShed decides whether to keep session affinity during a transparent retry before the first packet.
// Capacity shedding first backs off and retries on the same account maxCapacityShedSameAccountRetries times (keep affinity → next round
// still prefers to pick the same account), and once the budget is used up it unbinds and soft-excludes that account to force a switch; all other failures unbind and switch immediately.
// retries is a per-account counter scoped to the request; each shed account has its own backoff budget.
//
// It must soft-exclude rather than only unbind: shedding does not penalize account health, and if only affinity is unbound, scheduling would immediately pick this still-
// fully healthy account again and rebind it, and "switch after exhaustion" would be meaningless. The soft exclusion is cleared by ResetSoft after the account pool has been tried
// and it does not hold the request forever.
func (h *Handler) unbindOrRetainAffinityForCapacityShed(exclusions *retryAccountExclusions, affinityKey string, account *auth.Account, proxyURL string, outcome streamOutcome, retries map[int64]int, policy database.ContinuousRetryPolicy) {
	h.unbindOrRetainAffinityForCapacityShedWithGuard(exclusions, affinityKey, account, proxyURL, auth.SessionAffinityGuard{}, outcome, retries, policy)
}

func (h *Handler) unbindOrRetainAffinityForCapacityShedWithGuard(exclusions *retryAccountExclusions, affinityKey string, account *auth.Account, proxyURL string, guard auth.SessionAffinityGuard, outcome streamOutcome, retries map[int64]int, policy database.ContinuousRetryPolicy) {
	id := account.ID()
	// Catch-all promises a real account rotation for every upstream failure.
	// Keep the legacy same-account capacity backoff only for the normal,
	// selective policy mode.
	if !policy.CatchesAllUpstreamFailures() && capacityShedRetainsAffinity(outcome, retries[id]) {
		// Buffered attempts defer affinity until replay resolves. Bind here only
		// for real upstream capacity retries to preserve same-account backoff.
		// A buffered attempt defers the affinity binding until replay completes; here we bind only for a real upstream shed retry,
		// to keep the same-account backoff behavior.
		if continuousRetryBuffersAttempts(policy) {
			h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, guard)
		}
		retries[id]++
		return
	}
	h.store.UnbindSessionAffinity(affinityKey, id)
	if outcome.capacityShed {
		exclusions.MarkSoft(id)
	}
}

// capacityShedRetainsAffinity checks whether this capacity shed is still within the same-account backoff retry budget of that account.
func capacityShedRetainsAffinity(outcome streamOutcome, retriesSoFar int) bool {
	return outcome.capacityShed && retriesSoFar < maxCapacityShedSameAccountRetries
}

func continuousRetryBufferedAttemptCommitted(policy database.ContinuousRetryPolicy, outcome streamOutcome) bool {
	return continuousRetryBuffersAttempts(policy) && !outcome.terminalLocal && outcome.logStatusCode == http.StatusOK
}

func responseFailedErrorBody(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if gjson.GetBytes(payload, "error").Exists() {
		return payload
	}
	for _, path := range []string{
		"response.error",
		"response.status_details.error",
	} {
		result := gjson.GetBytes(payload, path)
		raw := strings.TrimSpace(result.Raw)
		if raw == "" || raw == "null" {
			continue
		}
		return []byte(`{"error":` + raw + `}`)
	}
	return payload
}

// responseFailedRetryable checks whether a response.failed terminal event is an upstream failure for which "switching accounts and retrying" makes sense
// (quota exhausted/rate limited/5xx/401). It is used to transparently switch accounts before the first packet, to avoid sending recoverable failure frames straight down to the
// WebSocket client, which would trigger repeated Reconnecting. Non-retryable failures (e.g. invalid_request) are still passed through as usual.
func responseFailedRetryable(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	return classifyResponseFailedOutcome(payload).penalize
}

// isRetryableUpstreamErrorFrame checks whether an in-stream {"type":"error"} frame is a retryable upstream failure.
// The real sequence for upstream capacity shedding is "event: error → event: response.failed": if an error frame is
// written out immediately as the first client output, once wroteAnyBody is set the following response.failed can never
// enter the silent account-switch branch before the first packet, and the shed error can only be passed through as-is; the Codex CLI treats
// server_is_overloaded/slow_down as fatal by a closed set and terminates the session directly. Retryable error frames
// must be buffered like lifecycle frames; non-retryable ones (content_policy/invalid_request, etc.)
// are still forwarded immediately as-is, keeping the upstream error details.
func isRetryableUpstreamErrorFrame(eventType string, payload []byte, policies ...database.ContinuousRetryPolicy) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = normalizedUpstreamSSEEventType("", payload)
	}
	if eventType != "error" {
		return false
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return false
	}
	// A continuous policy may deliberately select deterministic upstream error
	// frames (for example an account-specific 403/404). Keep those frames
	// buffered before the first token as well, otherwise the leading `error`
	// event commits the downstream response before the following
	// `response.failed` event can be retried transparently.
	outcome := classifyResponseFailedOutcome(payload)
	return continuousRetryStreamFailureSelected(outcome, payload, eventType, policies...)
}

// resolvePreContentRetryErrorCandidate promotes a standalone upstream error
// only when the stream ends before it produced a real response. Providers
// commonly emit error -> response.failed, but some relays close immediately
// after the error frame. Keeping it as a candidate avoids leaking a selected
// error before a later response.completed while still allowing the EOF case to
// enter the normal account-rotation retry path.
func resolvePreContentRetryErrorCandidate(terminalFailurePayload, candidate []byte, contentSeen, wroteAnyBody, gotTerminal bool, _ error, ctxErr, writeErr error) ([]byte, bool) {
	if len(terminalFailurePayload) > 0 || len(candidate) == 0 || contentSeen || wroteAnyBody || gotTerminal || ctxErr != nil || writeErr != nil {
		return terminalFailurePayload, false
	}
	return append([]byte(nil), candidate...), true
}

// capacityShedRetryableClientCode is the error code used when rewriting an upstream capacity-shed error that is passed through to downstream
// clients. The Codex CLI classifies by closed set: server_is_overloaded / slow_down are judged fatal
// (it shows "Selected model is at capacity. Please try a different model." and terminates
// the session), whereas error codes outside the closed set, such as server_error, enter the client's built-in backoff retry.
const capacityShedRetryableClientCode = "server_error"

// maxCapacityShedSameAccountRetries is the maximum number of backoff retries on the same account during capacity shedding.
// Shedding is a request-level signal bucketed by client identity/model capacity, and switching accounts does not change the shed factor, so first
// retry on the same account with backoff several times, and switch accounts only after they are exhausted (and throughout, do not count toward account consecutive-failure streak/health).
const maxCapacityShedSameAccountRetries = 2

// capacityShedHandlingDisabled reports whether the old behavior is restored via an environment variable (treating the shed as an ordinary 500
// that penalizes the account and switches immediately). It takes effect when CODEX_DISABLE_CAPACITY_SHED_HANDLING=1 (or true).
func capacityShedHandlingDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_CAPACITY_SHED_HANDLING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isCapacityShedPayload checks whether a response.failed / error frame is an upstream capacity shed
// (server_is_overloaded / slow_down). It reuses sanitizeCapacityShedEventForClient
// three code paths, shared by both classification and failure reporting.
func isCapacityShedPayload(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "response.status_details.error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "server_is_overloaded", "slow_down":
			return true
		}
	}
	return false
}

// sanitizeCapacityShedEventForClient rewrites the capacity-shed error code in the error / response.failed event about to be written to downstream
// into an error code the client can retry. Reaching this write means the gateway-side
// account-switch retry is no longer available (output already in the middle of the stream) or is exhausted; keeping the original shed code would only make the Codex CLI
// terminate the session in place. The error message is kept as-is; monitoring, billing and account cooldown are all based on the original payload before rewriting
// payload (terminalFailurePayload is captured before writing), and are not affected. rate_limit_exceeded
// and other error codes are left alone (clients rely on the original code to parse the retry delay).
func sanitizeCapacityShedEventForClient(eventType string, payload []byte) []byte {
	switch strings.TrimSpace(eventType) {
	case "error", "response.failed":
	default:
		return payload
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	updated := payload
	for _, path := range []string{"error.code", "response.error.code", "response.status_details.error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(updated, path).String())) {
		case "server_is_overloaded", "slow_down":
		default:
			continue
		}
		next, err := sjson.SetBytes(updated, path, capacityShedRetryableClientCode)
		if err != nil {
			return payload
		}
		updated = next
	}
	return updated
}

func (h *Handler) applyResponseFailedCooldown(account *auth.Account, payload []byte, resp *http.Response, model string) codex429Decision {
	if h == nil || account == nil || len(payload) == 0 {
		return codex429Decision{}
	}
	body := responseFailedErrorBody(payload)
	statusCode := classifyResponseFailedOutcome(payload).logStatusCode
	if statusCode == http.StatusTooManyRequests {
		// Official Codex response.failed/error is a semantic error that occurs after the HTTP/WS stream is established.
		// At that point resp describes the outer successful response or WS handshake, so ordinary models must not use its x-codex-*
		// snapshot to infer the account window of this 429. Spark has its own model quota, so the headers are kept
		// and classifySpark429RateLimit must still verify that the window is indeed exhausted. The real
		// HTTP/WS dial 429 does not go through this helper and still goes through the transport failure path
		// passes the original resp to applyCooldownForModel.
		if !isProOnlyModel(model) {
			resp = nil
			// An explicit model-capacity error should still only remove the current model; a plain rate_limit_exceeded
			// falls back to a short account-level cooldown.
			if !isCodexModelCapacityError(body) {
				model = ""
			}
		}
	}
	return h.applyCooldownForModel(account, statusCode, body, resp, model)
}

// applyResponseFailedDecisionKind enriches a stream failure with the cooldown
// reason without weakening a permanent quota classification. Relays sometimes
// put markers such as insufficient_quota only in error.message; a generic 429
// cooldown decision must not turn those hard failures back into a transient
// rate_limited_model cycle.
func applyResponseFailedDecisionKind(outcome streamOutcome, payload []byte, decision codex429Decision) streamOutcome {
	body := responseFailedErrorBody(payload)
	if isPermanentQuotaFailure(body) {
		outcome.failureKind = "usage_limit"
		return outcome
	}
	if decision.Reason != "" {
		outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, body, decision)
	}
	return outcome
}

func responseFailedStatusCodeWithEvidence(payload []byte) (int, bool) {
	for _, path := range []string{
		"response.status_code",
		"response.error.status_code",
		"response.status_details.error.status_code",
		"status_code",
		"error.status_code",
	} {
		code := int(gjson.GetBytes(payload, path).Int())
		if code >= 400 && code <= 599 {
			return code, true
		}
	}

	codeOrType := strings.ToLower(strings.Join([]string{
		gjson.GetBytes(payload, "response.error.code").String(),
		gjson.GetBytes(payload, "response.error.type").String(),
		gjson.GetBytes(payload, "response.status_details.error.code").String(),
		gjson.GetBytes(payload, "response.status_details.error.type").String(),
		gjson.GetBytes(payload, "error.code").String(),
		gjson.GetBytes(payload, "error.type").String(),
		gjson.GetBytes(payload, "code").String(),
		gjson.GetBytes(payload, "type").String(),
	}, " "))
	switch {
	case strings.Contains(codeOrType, "usage_limit"):
		return http.StatusTooManyRequests, true
	case strings.Contains(codeOrType, "rate_limit"):
		return http.StatusTooManyRequests, true
	case strings.Contains(codeOrType, "unauthorized") || strings.Contains(codeOrType, "authentication") || strings.Contains(codeOrType, "invalid_api_key") || strings.Contains(codeOrType, "invalid_token"):
		return http.StatusUnauthorized, true
	case strings.Contains(codeOrType, "payment"):
		return http.StatusPaymentRequired, true
	case strings.Contains(codeOrType, "forbidden") || strings.Contains(codeOrType, "permission"):
		return http.StatusForbidden, true
	case strings.Contains(codeOrType, "previous_response_not_found") || isPreviousResponseNotFoundBody(payload):
		return http.StatusBadRequest, true
	// Deterministic client errors: input exceeds the context window / field too long / model does not exist, etc.; retrying on another account
	// will also certainly fail. Classify as 400, to avoid falling into the default 500, which would trigger transparent retry and penalize the account
	// health (issue #310).
	//
	// When code/type is missing, also check message: relay upstreams often return the over-window error as
	// {"code":null,"type":"upstream_error","message":"Your input exceeds the
	// context window..."}; matching only code/type would fall into default 500, so the gateway would run the whole
	// account pool one by one (switching accounts would certainly fail the same way) and record a failure against every healthy account.
	// Reuse the check from the over-window compaction path that only looks at fixed error fields, to avoid scanning the full text and matching echoed request content.
	case strings.Contains(codeOrType, "context_length") ||
		strings.Contains(codeOrType, "context_window") ||
		strings.Contains(codeOrType, "above_max_length") ||
		strings.Contains(codeOrType, "model_not_found") ||
		strings.Contains(codeOrType, "unsupported") ||
		isContextLengthExceededBody(responseFailedErrorBody(payload)):
		return http.StatusBadRequest, true
	case strings.Contains(codeOrType, "invalid") || strings.Contains(codeOrType, "bad_request"):
		return http.StatusBadRequest, true
	case strings.Contains(codeOrType, "server_error") ||
		strings.Contains(codeOrType, "internal_server") ||
		strings.Contains(codeOrType, "server_is_overloaded") ||
		strings.Contains(codeOrType, "service_unavailable") ||
		strings.Contains(codeOrType, "temporarily_unavailable") ||
		strings.Contains(codeOrType, "bad_gateway") ||
		strings.Contains(codeOrType, "gateway_timeout"):
		return http.StatusInternalServerError, true
	default:
		return 0, false
	}
}

func responseFailedStatusCode(payload []byte) int {
	if status, evidenced := responseFailedStatusCodeWithEvidence(payload); evidenced {
		return status
	}
	// Keep the historical status used for logs/client errors. Selective HTTP
	// matching uses the evidenced helper above and never consumes this fallback.
	return http.StatusInternalServerError
}

func shouldTransparentRetryStream(outcome streamOutcome, attempt int, maxRetries int, wroteAnyBody bool, ctxErr, writeErr error) bool {
	generalRetries := attempt
	rateLimitRetries := attempt
	return shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, maxRetries, wroteAnyBody, ctxErr, writeErr)
}

func shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType string, terminalFailurePayload []byte, ttftRecorded bool, wroteAnyBody bool, attempt int, maxRetries int, ctxErr, writeErr error) bool {
	return shouldSuppressRetryableResponseFailedBeforeFirstTokenWithBudgets(
		eventType,
		terminalFailurePayload,
		ttftRecorded,
		wroteAnyBody,
		attempt,
		attempt,
		maxRetries,
		maxRetries,
		ctxErr,
		writeErr,
	)
}

func streamOutcomeUsesRateLimitBudget(outcome streamOutcome) bool {
	failureKind := strings.ToLower(strings.TrimSpace(outcome.failureKind))
	return outcome.logStatusCode == http.StatusTooManyRequests ||
		strings.Contains(failureKind, "rate_limit") ||
		strings.Contains(failureKind, "usage_limit")
}

func retryLimitForStreamOutcome(outcome streamOutcome, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) int {
	generalLimit, rateLimit = continuousRetryLimitsForStream(outcome, outcome.failurePayload, "", generalLimit, rateLimit, policies...)
	if streamOutcomeUsesRateLimitBudget(outcome) {
		return rateLimit
	}
	return generalLimit
}

func retryStateForStreamOutcome(outcome streamOutcome, generalRetries, rateLimitRetries, maxGeneralRetries, maxRateLimitRetries int, policies ...database.ContinuousRetryPolicy) (int, int) {
	return retryStateForStreamEvent(outcome, "", generalRetries, rateLimitRetries, maxGeneralRetries, maxRateLimitRetries, policies...)
}

func retryStateForStreamEvent(outcome streamOutcome, eventType string, generalRetries, rateLimitRetries, maxGeneralRetries, maxRateLimitRetries int, policies ...database.ContinuousRetryPolicy) (int, int) {
	maxGeneralRetries, maxRateLimitRetries = continuousRetryLimitsForStream(outcome, outcome.failurePayload, eventType, maxGeneralRetries, maxRateLimitRetries, policies...)
	if streamOutcomeUsesRateLimitBudget(outcome) {
		return rateLimitRetries, maxRateLimitRetries
	}
	return generalRetries, maxGeneralRetries
}

func shouldTransparentRetryStreamWithBudgets(outcome streamOutcome, generalRetries, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int, wroteAnyBody bool, ctxErr, writeErr error, policies ...database.ContinuousRetryPolicy) bool {
	return shouldTransparentRetryStreamEventWithBudgets(outcome, "", generalRetries, rateLimitRetries, maxGeneralRetries, maxRateLimitRetries, wroteAnyBody, ctxErr, writeErr, policies...)
}

func shouldTransparentRetryStreamEventWithBudgets(outcome streamOutcome, eventType string, generalRetries, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int, wroteAnyBody bool, ctxErr, writeErr error, policies ...database.ContinuousRetryPolicy) bool {
	// Native relay passthrough returns an already-classified client-closed
	// outcome instead of a separate writeErr. Never turn failed downstream
	// delivery into another upstream request, even in catch-all mode.
	if outcome.terminalLocal || wroteAnyBody || ctxErr != nil || writeErr != nil || outcome.logStatusCode == logStatusClientClosed {
		return false
	}
	if !continuousRetryStreamFailureSelected(outcome, outcome.failurePayload, eventType, policies...) {
		return false
	}
	maxGeneralRetries, maxRateLimitRetries = continuousRetryLimitsForStream(outcome, outcome.failurePayload, eventType, maxGeneralRetries, maxRateLimitRetries, policies...)
	retries := generalRetries
	limit := maxGeneralRetries
	if streamOutcomeUsesRateLimitBudget(outcome) {
		retries = rateLimitRetries
		limit = maxRateLimitRetries
	}
	if retries == nil || !retryBudgetAvailable(*retries, limit) {
		return false
	}
	*retries++
	return true
}

func shouldSuppressRetryableResponseFailedBeforeFirstTokenWithBudgets(eventType string, terminalFailurePayload []byte, ttftRecorded, wroteAnyBody bool, generalRetries, rateLimitRetries, maxGeneralRetries, maxRateLimitRetries int, ctxErr, writeErr error, policies ...database.ContinuousRetryPolicy) bool {
	if eventType != "response.failed" {
		return false
	}
	if ttftRecorded || wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	outcome := classifyResponseFailedOutcome(terminalFailurePayload)
	maxGeneralRetries, maxRateLimitRetries = continuousRetryLimitsForStream(outcome, terminalFailurePayload, eventType, maxGeneralRetries, maxRateLimitRetries, policies...)
	if !continuousRetryStreamFailureSelected(outcome, terminalFailurePayload, eventType, policies...) {
		return false
	}
	if streamOutcomeUsesRateLimitBudget(outcome) {
		return retryBudgetAvailable(rateLimitRetries, maxRateLimitRetries)
	}
	return retryBudgetAvailable(generalRetries, maxGeneralRetries)
}

// shouldReturnHTTPErrorForResponseFailed checks: for a streaming request that receives response.failed before the first token,
// (and nothing has yet been written downstream, and the client has not disconnected), the SSE forwarding should be aborted,
// and the real HTTP error code should be returned outside the loop, instead of wrapping the failure as 200 + [DONE].
//
// Background: while pending has not been flushed, the downstream HTTP 200 header has not been sent yet (see stream_flush_writer.go),
// so if response.failed were written into the stream and [DONE] appended, a billing relay layer that uses this service as its upstream
// would treat it as a normal completion and bill it by its own estimated input tokens,
// resulting in "upstream rejected (0 output) but billed by input". #310 already made deterministic
// client errors such as context_length_exceeded no longer retry on another account, but the streaming downstream response is still 200 + [DONE]; this function covers that other half.
//
// Note: once matched, besides aborting the forwarding, the post-loop wrap-up flush must also be skipped (see the wroteAnyBody guard),
// otherwise flusher.Flush on an empty buffer would still commit the 200 header early, making c.JSON(4xx) outside the loop ineffective.
func shouldReturnHTTPErrorForResponseFailed(eventType string, ttftRecorded, wroteAnyBody, clientGone bool) bool {
	return eventType == "response.failed" && !ttftRecorded && !wroteAnyBody && !clientGone
}

func imageGenerationOutputKey(item gjson.Result) string {
	if key := strings.TrimSpace(item.Get("id").String()); key != "" {
		return key
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return ""
	}
	return strings.TrimSpace(item.Get("output_format").String()) + "|" + result
}

func extractResponseImageGenerationOutput(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := imageGenerationOutputKey(item)
	if key != "" && seen != nil {
		if _, ok := seen[key]; ok {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	raw := []byte(item.Raw)
	var output map[string]any
	if err := json.Unmarshal(raw, &output); err == nil && addImageStatsToMap(output) {
		if annotated, err := json.Marshal(output); err == nil {
			raw = annotated
		}
	}
	return json.RawMessage(raw), true
}

func responseOutputItemDoneKey(item gjson.Result) string {
	if key := strings.TrimSpace(item.Get("id").String()); key != "" {
		// gjson strings borrow the parsed item's storage. Keep only the short
		// identity, not a second full output JSON through this map key.
		if len(key) <= 256 {
			return "id:" + key
		}
		sum := sha256.Sum256([]byte(key))
		return fmt.Sprintf("id-sha256:%x", sum)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(item.Raw)))
	return fmt.Sprintf("raw-sha256:%x", sum)
}

func extractResponseOutputItemDone(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() {
		return nil, false
	}
	key := responseOutputItemDoneKey(item)
	if key != "" && seen != nil {
		if _, ok := seen[key]; ok {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	raw := []byte(item.Raw)
	if item.Get("type").String() == "image_generation_call" {
		var output map[string]any
		if err := json.Unmarshal(raw, &output); err == nil && addImageStatsToMap(output) {
			if annotated, err := json.Marshal(output); err == nil {
				raw = annotated
			}
		}
	}
	return json.RawMessage(raw), true
}

type responseOutputItemRecord struct {
	outputIndex int
	hasIndex    bool
	sequence    int
	raw         json.RawMessage
}

// responseOutputCollector rebuilds terminal response.output from the canonical
// response.output_item.done lifecycle events. Relays occasionally return an
// empty or partially populated terminal output array even though every item was
// already delivered. The collector is request-scoped and bounded by the same
// logical reconstruction budget used for response-context replay.
type responseOutputCollector struct {
	indexed   map[int]responseOutputItemRecord
	unindexed []responseOutputItemRecord
	seen      map[string]struct{}
	bytes     int64
	limit     int64
	sequence  int
	overflow  bool
}

func newResponseOutputCollector() *responseOutputCollector {
	limit := GetResponseCacheAppliedConfig().ReconstructMaxBytes
	if limit <= 0 {
		limit = responseCacheMaxBytes
	}
	return &responseOutputCollector{
		indexed: make(map[int]responseOutputItemRecord),
		seen:    make(map[string]struct{}),
		limit:   limit,
	}
}

func (c *responseOutputCollector) Add(data []byte) bool {
	if c == nil || c.overflow {
		return false
	}
	raw, ok := extractResponseOutputItemDone(data, c.seen)
	if !ok {
		return false
	}
	// Count only newly accepted item identities, not delta/terminal events or
	// duplicate done frames after a collector has reached exactly its limit.
	if len(c.seen) > responseOutputCollectorMaxItems {
		c.clearOverflow()
		return false
	}
	record := responseOutputItemRecord{sequence: c.sequence, raw: raw}
	c.sequence++
	if outputIndex := gjson.GetBytes(data, "output_index"); outputIndex.Exists() && outputIndex.Int() >= 0 {
		record.hasIndex = true
		record.outputIndex = int(outputIndex.Int())
	}

	nextBytes := c.bytes + int64(len(raw))
	if record.hasIndex {
		if previous, exists := c.indexed[record.outputIndex]; exists {
			nextBytes -= int64(len(previous.raw))
		}
	}
	if nextBytes > c.limit {
		c.clearOverflow()
		log.Printf("skipping Responses terminal output rebuild: accumulated output_item.done exceeds %d bytes", c.limit)
		return false
	}
	c.bytes = nextBytes
	if record.hasIndex {
		c.indexed[record.outputIndex] = record
	} else {
		c.unindexed = append(c.unindexed, record)
	}
	return true
}

const responseOutputCollectorMaxItems = 4096

func (c *responseOutputCollector) clearOverflow() {
	c.overflow = true
	c.indexed = nil
	c.unindexed = nil
	c.seen = nil
	c.bytes = 0
}

func (c *responseOutputCollector) Items() []json.RawMessage {
	if c == nil || c.overflow || (len(c.indexed) == 0 && len(c.unindexed) == 0) {
		return nil
	}
	records := make([]responseOutputItemRecord, 0, len(c.indexed)+len(c.unindexed))
	for _, record := range c.indexed {
		records = append(records, record)
	}
	records = append(records, c.unindexed...)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].hasIndex != records[j].hasIndex {
			return records[i].hasIndex
		}
		if records[i].hasIndex && records[i].outputIndex != records[j].outputIndex {
			return records[i].outputIndex < records[j].outputIndex
		}
		return records[i].sequence < records[j].sequence
	})
	items := make([]json.RawMessage, 0, len(records))
	for _, record := range records {
		items = append(items, record.raw)
	}
	return items
}

func restoreMissingResponseOutputs(responseJSON []byte, outputItems []json.RawMessage) []byte {
	if len(responseJSON) == 0 || len(outputItems) == 0 {
		return responseJSON
	}
	terminalOutput := gjson.GetBytes(responseJSON, "output")
	terminalCount := int64(-1)
	if terminalOutput.IsArray() {
		terminalCount = terminalOutput.Get("#").Int()
	}
	if terminalCount >= int64(len(outputItems)) {
		return responseJSON
	}
	outputs := make([]json.RawMessage, 0, len(outputItems))
	for _, rawItem := range outputItems {
		if len(rawItem) == 0 || !gjson.ValidBytes(rawItem) {
			continue
		}
		outputs = append(outputs, rawItem)
	}
	if len(outputs) == 0 {
		return responseJSON
	}
	if terminalCount >= int64(len(outputs)) {
		return responseJSON
	}
	if firstNonSpace(responseJSON) != '{' || !gjson.ValidBytes(responseJSON) {
		return responseJSON
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return responseJSON
	}
	// Patch only output. Large usage/attribution trees stay opaque, preserving
	// unknown fields and exact JSON numbers while avoiding a full map round trip.
	restored, err := sjson.SetRawBytes(responseJSON, "output", encoded)
	if err != nil {
		return responseJSON
	}
	return restored
}

func restoreMissingResponseOutputsInEvent(eventData []byte, outputItems []json.RawMessage) []byte {
	response := gjson.GetBytes(eventData, "response")
	if !response.Exists() || !response.IsObject() {
		return eventData
	}
	restored := restoreMissingResponseOutputs([]byte(response.Raw), outputItems)
	if bytes.Equal(restored, []byte(response.Raw)) {
		return eventData
	}
	updated, err := sjson.SetRawBytes(eventData, "response", restored)
	if err != nil {
		return eventData
	}
	return updated
}

func appendMissingResponseImageOutputs(responseJSON []byte, imageOutputs []json.RawMessage) []byte {
	if len(responseJSON) == 0 {
		return responseJSON
	}
	var response map[string]any
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return responseJSON
	}

	seen := make(map[string]struct{})
	changed := false
	outputs, _ := response["output"].([]any)
	for _, rawOutput := range outputs {
		outputMap, ok := rawOutput.(map[string]any)
		if !ok {
			continue
		}
		if firstNonEmptyAnyString(outputMap["type"]) != "image_generation_call" {
			continue
		}
		outputBytes, err := json.Marshal(outputMap)
		if err != nil {
			continue
		}
		item := gjson.ParseBytes(outputBytes)
		if key := imageGenerationOutputKey(item); key != "" {
			seen[key] = struct{}{}
		}
		if addImageStatsToMap(outputMap) {
			changed = true
		}
	}

	for _, rawImage := range imageOutputs {
		if len(rawImage) == 0 || !gjson.ValidBytes(rawImage) {
			continue
		}
		item := gjson.ParseBytes(rawImage)
		key := imageGenerationOutputKey(item)
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		var decoded any
		if err := json.Unmarshal(rawImage, &decoded); err != nil {
			continue
		}
		if outputMap, ok := decoded.(map[string]any); ok {
			addImageStatsToMap(outputMap)
		}
		outputs = append(outputs, decoded)
		changed = true
	}
	if !changed {
		return responseJSON
	}
	response["output"] = outputs
	merged, err := json.Marshal(response)
	if err != nil {
		return responseJSON
	}
	return merged
}

// RegisterRoutes registers the routes
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	auth := h.authMiddleware()

	// /v1-prefixed routes (standard paths)
	v1 := r.Group("/v1")
	v1.Use(auth)
	v1.POST("/prompt-filter/newapi/verify", h.VerifyNewAPIPolicyHandshake)
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.GET("/responses", h.ResponsesWebSocket)
	v1.GET("/realtime", h.RealtimeWebSocket)
	v1.POST("/responses/compact", h.ResponsesCompact)
	v1.POST("/images/generations", h.ImagesGenerations)
	v1.POST("/images/edits", h.ImagesEdits)
	v1.POST("/responses/input_tokens", h.ResponsesInputTokens)
	// Codex CLI / Codex App refresh the model menu from /models?client_version=..., and expect
	// the manifest format; client_version is the natural fingerprint of the Codex client, and ordinary OpenAI
	// clients do not send it, and other requests keep the OpenAI-format list unchanged.
	v1.GET("/models", h.listModelsOrManifest)
	// Standalone web search endpoint for Codex CLI web_search = "live" (issue #359)
	v1.POST("/alpha/search", h.CodexAlphaSearchHandler)
	v1.POST("/live", h.LiveCreate)
	v1.GET("/live/:call_id", h.LiveSideband)

	// Routes without a prefix (for clients whose base_url already includes /v1)
	r.POST("/chat/completions", auth, h.ChatCompletions)
	r.POST("/responses", auth, h.Responses)
	r.GET("/responses", auth, h.ResponsesWebSocket)
	r.GET("/realtime", auth, h.RealtimeWebSocket)
	r.POST("/responses/compact", auth, h.ResponsesCompact)
	r.POST("/images/generations", auth, h.ImagesGenerations)
	r.POST("/images/edits", auth, h.ImagesEdits)
	r.POST("/responses/input_tokens", auth, h.ResponsesInputTokens)
	r.GET("/models", auth, h.listModelsOrManifest)
	r.POST("/alpha/search", auth, h.CodexAlphaSearchHandler)
	r.POST("/live", auth, h.LiveCreate)
	r.GET("/live/:call_id", auth, h.LiveSideband)

	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(auth)
	codexDirect.POST("/responses", h.Responses)
	codexDirect.GET("/responses", h.ResponsesWebSocket)
	codexDirect.GET("/models", h.CodexModelsManifestHandler)
	codexDirect.POST("/alpha/search", h.CodexAlphaSearchHandler)
	codexDirect.POST("/realtime/calls", h.LiveCreate)
	codexDirect.POST("/responses/*subpath", func(c *gin.Context) {
		subpath := strings.TrimSpace(c.Param("subpath"))
		if subpath == "/compact" || strings.HasPrefix(subpath, "/compact/") {
			h.ResponsesCompact(c)
			return
		}
		h.Responses(c)
	})
	codexDirect.GET("/:call_id", h.LiveSideband)
}

// APIKeyAuthMiddleware exposes the standard /v1 API key authentication middleware
// for companion routes that live outside proxy.RegisterRoutes.
func (h *Handler) APIKeyAuthMiddleware() gin.HandlerFunc {
	return h.authMiddleware()
}

// APIKeyReadAuthMiddleware permits exhausted keys to retrieve their own stored
// results. All other authentication checks remain in force; writes stay blocked.
func (h *Handler) APIKeyReadAuthMiddleware() gin.HandlerFunc {
	return h.authMiddlewareWithQuotaRead(true)
}

// authMiddleware is the API key auth middleware (enhanced version, with security logging)
//
// Security policy (fail-closed):
//   - By default, when no API key is configured, requests are rejected directly (503), to avoid running the account pool unprotected.
//   - Requests are allowed without a key only when CODEX_ALLOW_ANONYMOUS=true is explicitly set (for compatibility with internal networks/tests).
func (h *Handler) authMiddleware() gin.HandlerFunc {
	return h.authMiddlewareWithQuotaRead(false)
}

func (h *Handler) authMiddlewareWithQuotaRead(allowQuotaRead bool) gin.HandlerFunc {
	allowAnonymous := h.cfg != nil && h.cfg.AllowAnonymousV1
	return func(c *gin.Context) {
		attachUserAgentAudit(c)
		attachWsAcquireAudit(c)
		attachUpstreamTrace(c, h.store)
		// If no key is configured
		hasKeys, presenceErr := h.hasAnyKeysWithError()
		if presenceErr != nil {
			api.SendError(c, api.ErrServiceUnavailable)
			c.Abort()
			return
		}
		if !hasKeys {
			if allowAnonymous {
				// Explicitly allow anonymous access (old behavior, enabled only when CODEX_ALLOW_ANONYMOUS=true)
				c.Next()
				return
			}
			// fail-closed: reject when no API key is configured, to prevent unauthorized calls to the account pool
			security.SecurityAuditLog("V1_BLOCKED_NO_KEYS", fmt.Sprintf("path=%s ip=%s", c.Request.URL.Path, c.ClientIP()))
			api.SendError(c, api.NewAPIError(
				api.ErrCodeServiceUnavailable,
				"Service is not configured: no API key has been created yet. Please add at least one API key in the admin dashboard, or set CODEX_ALLOW_ANONYMOUS=true to disable this check.",
				api.ErrorTypeServer,
			))
			c.Abort()
			return
		}

		authHeader := downstreamAuthorizationHeader(c.Request)
		if authHeader == "" {
			// Use standardized error format from api package
			api.SendError(c, api.ErrMissingAPIKey)
			c.Abort()
			return
		}

		// Sanitize input
		authHeader = security.SanitizeInput(authHeader)

		key := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		apiKeyRow, ok, resolveErr := h.resolveAPIKeyContext(c.Request.Context(), key)
		if resolveErr != nil {
			// Transient DB/infrastructure failure: return 503, do not treat it as an invalid client key (issue #323).
			// Do not write an AUTH_FAILED audit log, to avoid polluting credential-attack alerts.
			api.SendError(c, api.ErrServiceUnavailable)
			c.Abort()
			return
		}
		if !ok {
			// Record the security audit log (redacted)
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			// Use standardized error format from api package
			api.SendError(c, api.ErrInvalidAPIKey)
			c.Abort()
			return
		}
		if !apiKeyRow.Enabled {
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED_DISABLED_KEY", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			api.SendError(c, api.NewAPIError(api.ErrCodeInvalidAuth, "API key is disabled", api.ErrorTypeAuthentication))
			c.Abort()
			return
		}
		if apiKeyRow.IsExpired(time.Now()) {
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED_EXPIRED_KEY", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			api.SendError(c, api.NewAPIError(api.ErrCodeInvalidAuth, "API key has expired", api.ErrorTypeAuthentication))
			c.Abort()
			return
		}
		readOnly := allowQuotaRead && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead)
		if apiKeyRow.IsQuotaExhausted() && !readOnly {
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED_QUOTA_EXHAUSTED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			api.SendError(c, api.NewAPIError(api.ErrCodeRateLimitReached, "API key quota exhausted", api.ErrorTypeRateLimit))
			c.Abort()
			return
		}
		c.Set(contextAPIKeyID, apiKeyRow.ID)
		c.Set(contextAPIKeyName, strings.TrimSpace(apiKeyRow.Name))
		c.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKeyRow.Key))
		c.Set(contextAPIKeyRow, apiKeyRow)
		if rejectExternalSchedulerIngress(c, apiKeyRow) {
			c.Abort()
			return
		}
		h.attachAPIKeyModelRequestQuota(c, false)
		c.Set("apiKey", key)
		if h.enforceRequiredNewAPIIdentityAtIngress(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}

func apiKeyFromWebSocketSubprotocol(header string) string {
	for _, item := range strings.Split(header, ",") {
		item = strings.TrimSpace(item)
		const prefix = "openai-insecure-api-key."
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(item, prefix))
		}
	}
	return ""
}

// ==================== /v1/responses ====================

// getMaxRetries reads the configurable maximum retry count from the store
func (h *Handler) getMaxRetries() int {
	return h.store.GetMaxRetries()
}

func (h *Handler) getMaxRateLimitRetries() int {
	if h == nil || h.store == nil {
		return 1
	}
	return h.store.GetMaxRateLimitRetries()
}

// effectiveMaxRateLimitRetries returns the rate-limit (429) account-switch retry cap that applies to the current account, i.e. the global max_rate_limit_retries.
func (h *Handler) effectiveMaxRateLimitRetries(account *auth.Account, fallback int) int {
	return fallback
}

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
	// AccessLogStatusContextKey lets the stream handler, after the HTTP 200 header has been committed,
	// provide the final internal result (such as 499 for a client disconnect) to the access log middleware.
	AccessLogStatusContextKey = "x-access-log-status"
)

// upstreamStreamBreakMessage is the stable, readable message fed back to downstream on a stream break; machine recognition uses
// the stable error code ErrorCodeUpstreamStreamBreak, so downstream gateways/clients can automatically retry based on it.
const upstreamStreamBreakMessage = "Upstream stream ended prematurely; safe to retry (status 502)"

// writeResponsesStreamBreakEvent synthesizes on a Responses SSE stream whose body has already been written
// a response.failed terminal event. A stream break after the first packet cannot be silently retried as a whole, and the SSE must not silently
// EOF—downstream would treat the truncated response as a normal 200 completion, with no way to notice the failure or retry
// (issue #473). It does not send response.completed, to avoid billing the truncated response as successful.
func writeResponsesStreamBreakEvent(w *streamFlushWriter) error {
	payload := []byte(fmt.Sprintf(
		`{"type":"response.failed","response":{"created_at":%d,"status":"failed","error":{"code":"%s","message":"%s"}}}`,
		time.Now().Unix(), ErrorCodeUpstreamStreamBreak, upstreamStreamBreakMessage,
	))
	if err := w.WriteSSEData(payload); err != nil {
		return err
	}
	return w.Flush()
}

// writeChatCompletionsStreamBreakEvent is the same as above, in the OpenAI Chat protocol form: an in-stream error
// object, and it does not append [DONE]—the missing [DONE] is itself a failure signal downstream can recognize.
func writeChatCompletionsStreamBreakEvent(w *streamFlushWriter) error {
	payload := []byte(`{"error":{"message":"` + upstreamStreamBreakMessage +
		`","type":"` + ErrorTypeUpstreamError + `","code":"` + ErrorCodeUpstreamStreamBreak + `"}}`)
	if err := w.WriteSSEData(payload); err != nil {
		return err
	}
	return w.Flush()
}

// shouldWriteStreamBreakEvent checks whether a synthetic stream-break terminal event must be written downstream:
// the body has already been written, the upstream gave no terminal state, and the client is still online. Having written the body means the transparent retry window
// is closed (shouldTransparentRetryStream requires zero writes); this is the last failure signal exit.
func shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody bool, ctxErr, writeErr error) bool {
	return !gotTerminal && wroteAnyBody && ctxErr == nil && writeErr == nil
}

// isResponsesSuccessTerminalEvent checks whether an event is a normal Responses terminal state.
// response.incomplete and response.completed are both normal terminal states: when the upstream truncates at
// max_output_tokens it only sends the former, and it still carries the full output and usage. Failing to recognize it would
// make the wrap-up logic treat the normal truncation as a stream break—synthesizing a fake response.failed / overloaded_error,
// discarding the real usage in favor of an estimate, and penalizing the account for a stream break.
func isResponsesSuccessTerminalEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.incomplete"
}

// isResponsesTerminalEvent covers all Responses terminal states (normal/truncated/failed),
// so the SSE read loop can decide "reading up to here means we can wrap up".
func isResponsesTerminalEvent(eventType string) bool {
	return isResponsesSuccessTerminalEvent(eventType) || eventType == "response.failed"
}

// responsesIncompleteFinishReason maps the Responses truncation reason to the Chat
// finish_reason; a non-truncation terminal state returns an empty string, meaning "keep the derived value".
func responsesIncompleteFinishReason(eventType, reason string) string {
	if eventType != "response.incomplete" {
		return ""
	}
	if reason == "content_filter" {
		return "content_filter"
	}
	return "length"
}

// isRetryableStatus checks whether the upstream status code is retryable.
// 403 is also treated as retryable: Codex upstream 403s are all account-side issues (payment_required /
// deactivated_workspace / codex_access_restricted, etc.; OAuth/plan/workspace dimensions),
// not request-content problems; switching to another healthy account in the pool can continue (issue #396).
// 402 is the same: billing-dimension rejections such as deactivated_workspace (team space banned) are purely account-side
// problems; applyCooldownForModel has already isolated that account as bad, so retrying on another account succeeds.
func isRetryableStatus(code int) bool {
	return code == http.StatusServiceUnavailable ||
		code == http.StatusUnauthorized ||
		code == http.StatusInternalServerError ||
		code == http.StatusPaymentRequired ||
		code == http.StatusForbidden ||
		code == http.StatusUpgradeRequired
}

func shouldRetryHTTPStatus(statusCode int, body []byte, generalRetries *int, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int, policies ...database.ContinuousRetryPolicy) bool {
	policy := continuousRetryPolicyForCall(policies)
	if isExplicitUpstreamCyberPolicy(body) {
		return false
	}
	if isExplicitUpstreamSafetyPolicy(body) && !policy.CatchesAllUpstreamFailures() {
		return false
	}
	policySelected := continuousRetryHTTPSelected(policy, statusCode, body)
	maxGeneralRetries, maxRateLimitRetries = continuousRetryLimitsForHTTP(statusCode, body, maxGeneralRetries, maxRateLimitRetries, policy)
	if statusCode == http.StatusTooManyRequests {
		if rateLimitRetries == nil || !retryBudgetAvailable(*rateLimitRetries, maxRateLimitRetries) {
			return false
		}
		*rateLimitRetries++
		return true
	}
	// A 400 is generally a request-content problem and is not retried; only "account does not support this model" is an account entitlement problem,
	// that account has already been excluded by model cooldown, so retrying on another account succeeds (issue #408).
	modelUnsupported := statusCode == http.StatusBadRequest && isCodexModelUnsupportedError(body)
	if !isRetryableStatus(statusCode) && !modelUnsupported && !policySelected {
		return false
	}
	if generalRetries == nil || !retryBudgetAvailable(*generalRetries, maxGeneralRetries) {
		return false
	}
	*generalRetries++
	return true
}

func shouldRetryRequestError(err error, generalRetries *int, maxGeneralRetries int, policies ...database.ContinuousRetryPolicy) bool {
	policy := continuousRetryPolicyForCall(policies)
	if isExplicitUpstreamCyberPolicyError(err) {
		return false
	}
	if _, body, ok := continuousRetryHTTPErrorDetails(err); ok && isExplicitUpstreamSafetyPolicy(body) && !policy.CatchesAllUpstreamFailures() {
		return false
	}
	selected := continuousRetryRequestErrorSelected(policy, err)
	maxGeneralRetries = continuousRetryLimitForRequestError(err, maxGeneralRetries, policy)
	if err == nil || generalRetries == nil || !retryBudgetAvailable(*generalRetries, maxGeneralRetries) {
		return false
	}
	if isRetryableRequestError(err) || selected {
		*generalRetries++
		return true
	}
	return false
}

// isRetryableRequestError gives an explicit non-retryable structured error
// precedence over the generic transport classifier. Executors use
// ErrUpstream(0, ..., cause) for failures from http.Client.Do; those remain
// retryable even though a status-less Error cannot set Retryable by status.
func isRetryableRequestError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var structured *Error
	if errors.As(err, &structured) {
		if structured.Retryable {
			return true
		}
		return structured.Code == ErrorCodeUpstreamError && structured.HTTPStatus == 0 && structured.Cause != nil
	}
	return classifyTransportFailure(err) != ""
}

func isRetryableRequestErrorForContext(ctx context.Context, err error, policies ...database.ContinuousRetryPolicy) bool {
	if apiKeyModelRequestError(err) != nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	policy := continuousRetryPolicyForCall(policies)
	if isExplicitUpstreamCyberPolicyError(err) {
		return false
	}
	if _, body, ok := continuousRetryHTTPErrorDetails(err); ok && isExplicitUpstreamSafetyPolicy(body) && !policy.CatchesAllUpstreamFailures() {
		return false
	}
	if isRetryableRequestError(err) {
		return true
	}
	return continuousRetryRequestErrorSelected(policy, err)
}

const transportRetryPolicySticky = "sticky"

// retryBudgetAvailable reports whether another retry may be consumed. A limit
// of -1 is the explicit unlimited sentinel; zero disables retries and positive
// limits preserve the existing exact-count behavior.
func retryBudgetAvailable(used, limit int) bool {
	return limit == -1 || (limit > 0 && used < limit)
}

func retryLimitForHTTPStatus(statusCode, generalLimit, rateLimit int) int {
	if statusCode == http.StatusTooManyRequests {
		return rateLimit
	}
	return generalLimit
}

func retryStateForHTTPStatus(statusCode, generalRetries, rateLimitRetries, generalLimit, rateLimit int) (int, int) {
	return retryStateForHTTPStatusWithBody(statusCode, nil, generalRetries, rateLimitRetries, generalLimit, rateLimit)
}

// retryStateForHTTPStatusWithBody mirrors retryStateForHTTPStatus while also
// applying the selected continuous-retry policy. The body is needed when the
// operator selected an exact upstream error code rather than a status/category;
// without it the retry itself would be unlimited but the backoff/logging state
// would still look finite and could hot-loop.
func retryStateForHTTPStatusWithBody(statusCode int, body []byte, generalRetries, rateLimitRetries, generalLimit, rateLimit int, policies ...database.ContinuousRetryPolicy) (int, int) {
	generalLimit, rateLimit = continuousRetryLimitsForHTTP(statusCode, body, generalLimit, rateLimit, policies...)
	if statusCode == http.StatusTooManyRequests {
		return rateLimitRetries, retryLimitForHTTPStatus(statusCode, generalLimit, rateLimit)
	}
	return generalRetries, retryLimitForHTTPStatus(statusCode, generalLimit, rateLimit)
}

func retryAttemptProgress(attempt, limit int) string {
	if limit == -1 {
		return fmt.Sprintf("%d/unlimited", attempt+1)
	}
	return fmt.Sprintf("%d/%d", attempt+1, limit+1)
}

const (
	unlimitedRetryBackoffBase = 250 * time.Millisecond
	unlimitedRetryBackoffMax  = 30 * time.Second
)

// unlimitedRetryBackoff is pure so its exponential and jitter boundaries can
// be tested without sleeping. retryOrdinal is one-based. Jitter adds up to 50%
// of the exponential floor, while the final delay remains capped.
func unlimitedRetryBackoff(retryOrdinal int, jitterUnit float64) time.Duration {
	if retryOrdinal < 1 {
		retryOrdinal = 1
	}
	if jitterUnit < 0 {
		jitterUnit = 0
	} else if jitterUnit > 1 {
		jitterUnit = 1
	}

	delay := unlimitedRetryBackoffBase
	for i := 1; i < retryOrdinal && delay < unlimitedRetryBackoffMax; i++ {
		if delay > unlimitedRetryBackoffMax/2 {
			delay = unlimitedRetryBackoffMax
			break
		}
		delay *= 2
	}
	jitter := time.Duration(float64(delay/2) * jitterUnit)
	if delay >= unlimitedRetryBackoffMax-jitter {
		return unlimitedRetryBackoffMax
	}
	return delay + jitter
}

// maxRetryAfterDelay bounds an upstream-controlled delay. Retry-After is still
// treated as a minimum over the locally configured retry interval, but a bad
// relay cannot park a request forever with an arbitrarily large value.
const maxRetryAfterDelay = 5 * time.Minute

// waitBeforeRetry waits between two retries for the retry interval configured in the admin side (retry_interval_ms, 0 = retry immediately).
// If the upstream response carries Retry-After, it waits for the larger of the local interval, the continuous retry backoff, and the upstream suggestion.
// If the client disconnects during the wait it returns false, and the caller should abandon this retry (issue #331).
func (h *Handler) waitBeforeRetry(ctx context.Context, responses ...*http.Response) bool {
	retryLimit := 0
	if h != nil && h.store != nil {
		retryLimit = h.store.GetMaxRetries()
	}
	return h.waitBeforeRetryWithBudget(ctx, 1, retryLimit, responses...)
}

func (h *Handler) waitBeforeRetryWithBudget(ctx context.Context, retryOrdinal, retryLimit int, responses ...*http.Response) bool {
	return h.waitBeforeRetryWithBudgetMode(ctx, retryOrdinal, retryLimit, true, responses...)
}

// waitBeforeRetryWithFirstTokenTimeout preserves the finite TTFT shortcut, but
// An unlimited budget must go through the unified backoff, to avoid a zero-wait loop from first-token timeouts.
func (h *Handler) waitBeforeRetryWithFirstTokenTimeout(ctx context.Context, firstTokenTimeout bool, retryOrdinal, retryLimit int, responses ...*http.Response) bool {
	if firstTokenTimeout && retryLimit != -1 {
		return ctx == nil || ctx.Err() == nil
	}
	return h.waitBeforeRetryWithBudget(ctx, retryOrdinal, retryLimit, responses...)
}

func (h *Handler) waitBeforeRetryWithBudgetMode(ctx context.Context, retryOrdinal, retryLimit int, useRequestKeepalive bool, responses ...*http.Response) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	activateContinuousRetryDeadlineForLimit(ctx, retryLimit)
	if retryLimit == -1 {
		for _, resp := range responses {
			rememberContinuousRetryHTTPFailure(ctx, resp, nil)
		}
	}
	interval := time.Duration(0)
	if h != nil && h.store != nil {
		interval = time.Duration(h.store.GetRetryIntervalMS()) * time.Millisecond
	}
	// Retry-After is part of the continuous-retry contract only. Finite and
	// disabled retry budgets retain the historical local retry interval and do
	// not let an upstream response introduce an unexpected delay.
	if retryLimit == -1 {
		for _, resp := range responses {
			if resp == nil {
				continue
			}
			rawRetryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
			if rawRetryAfter == "" {
				continue
			}
			retryAfter := parseRetryAfterHeader(rawRetryAfter)
			if retryAfter > maxRetryAfterDelay {
				retryAfter = maxRetryAfterDelay
			}
			if retryAfter > interval {
				interval = retryAfter
			}
		}
	}
	if retryLimit == -1 {
		if backoff := unlimitedRetryBackoff(retryOrdinal, rand.Float64()); backoff > interval {
			interval = backoff
		}
	}
	if retryLimit == -1 && useRequestKeepalive {
		activateContinuousRetryKeepalive(ctx)
	}
	if interval <= 0 {
		return true
	}
	if retryLimit == -1 && useRequestKeepalive {
		return waitWithContinuousRetryKeepalive(ctx, interval)
	}
	return waitForRetryInterval(ctx, interval)
}

// stickyTransportRetryEnabled returns whether transport-class failures are retried sticky on the same account (issue #331).
// The root of connection-level failures such as network fluctuation or a proxy switching nodes is not the account: in sticky mode it does not switch accounts, does not record an account failure,
// and does not unbind session affinity, retrying on the same account after the retry interval; rotate (the default) keeps the old behavior.
func (h *Handler) stickyTransportRetryEnabled() bool {
	return h != nil && h.store != nil && h.store.GetTransportRetryPolicy() == transportRetryPolicySticky
}

// shouldStickyTransportRetry keeps the legacy same-account behavior only for
// plain transport blips. A selected upstream error, and every catch-all
// failure, must rotate so the continuous-retry switch does what its label says.
func (h *Handler) shouldStickyTransportRetry(err error, kind string, timedOut, shouldRetry bool, policy database.ContinuousRetryPolicy) bool {
	if !shouldRetry || timedOut || kind == "" || kind == upstreamErrorKindWsBusyAcquire || !h.stickyTransportRetryEnabled() {
		return false
	}
	if policy.CatchesAllUpstreamFailures() {
		return false
	}
	return !continuousRetryRequestErrorSelected(policy, err) && (err == nil || !policy.MatchesTransport(err.Error()))
}

// bindBufferedStickyRetryAffinity restores pending affinity before a genuine
// sticky transport retry because buffering defers normal binding until commit.
// bindBufferedStickyRetryAffinity restores the pending affinity before a real sticky transport retry; in buffered mode
// the regular binding is deferred until commit, otherwise the next attempt would switch accounts unexpectedly.
func (h *Handler) bindBufferedStickyRetryAffinity(ctx context.Context, affinityKey string, account *auth.Account, proxyURL string, stickyRetry bool, policy database.ContinuousRetryPolicy) bool {
	if h == nil || h.store == nil || account == nil || !stickyRetry || !continuousRetryBuffersAttempts(policy) {
		return true
	}
	return bindContinuousRetrySessionAffinity(ctx, h.store, affinityKey, account, proxyURL)
}

func IsDeactivatedWorkspaceError(body []byte) bool {
	for _, path := range []string{"detail.code", "error.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code == "deactivated_workspace" {
			return true
		}
	}
	return strings.Contains(strings.ToLower(string(body)), "deactivated_workspace")
}

// IsAgentRuntimeDeletedError checks whether the response body indicates that the Agent runtime has been deleted.
func IsAgentRuntimeDeletedError(body []byte) bool {
	code := strings.ToLower(firstGJSONString(body, "error.code", "detail.code", "code"))
	if code != "biscuit_baker_service_agent_error_status" {
		return false
	}
	message := strings.ToLower(firstGJSONString(body, "error.message", "detail.message", "message"))
	return strings.Contains(message, "agent runtime has been deleted")
}

func upstreamAccountErrorMessage(statusCode int, body []byte) string {
	if IsDeactivatedWorkspaceError(body) {
		return fmt.Sprintf("upstream returned %d: deactivated_workspace", statusCode)
	}
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "detail.message").String())
	}
	// Do not persist arbitrary HTML/plain-text bodies. Structured JSON fields
	// above are bounded and sanitised; an unstructured body becomes status text.
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return fmt.Sprintf("upstream returned %d: %s", statusCode, message)
}

func upstreamErrorKind(statusCode int, body []byte, decision codex429Decision) string {
	if IsUsageLimitReachedError(body) {
		if decision.Reason != "" {
			return decision.Reason
		}
		return "usage_limit"
	}
	// Relays sometimes expose permanent billing/quota markers only in the
	// structured message field. A model-scoped cooldown decision must not turn
	// those account-level failures back into a transient rate-limit cycle.
	if isPermanentQuotaFailure(body) {
		return "usage_limit"
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		if decision.Reason != "" {
			return decision.Reason
		}
		return "rate_limited"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		if IsDeactivatedWorkspaceError(body) {
			return "deactivated_workspace"
		}
		if decision.Reason != "" {
			return decision.Reason
		}
		return "payment_required_unknown"
	case http.StatusForbidden:
		if statusCode == http.StatusForbidden && IsAgentRuntimeDeletedError(body) {
			return "agent_runtime_deleted"
		}
		if IsDeactivatedWorkspaceError(body) {
			return "deactivated_workspace"
		}
		return "forbidden"
	case http.StatusUpgradeRequired:
		return "version_required"
	case http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "server"
	default:
		if statusCode >= 400 {
			return "client"
		}
		return ""
	}
}

func parseUsageLimitDetails(body []byte) (usageLimitDetails, bool) {
	if len(body) == 0 {
		return usageLimitDetails{}, false
	}
	if !IsUsageLimitReachedError(body) {
		return usageLimitDetails{}, false
	}
	return usageLimitDetails{
		message:         firstGJSONString(body, "error.message", "response.error.message", "response.status_details.error.message"),
		planType:        firstGJSONString(body, "error.plan_type", "response.error.plan_type", "response.status_details.error.plan_type"),
		resetsAt:        firstGJSONInt(body, "error.resets_at", "response.error.resets_at", "response.status_details.error.resets_at"),
		resetsInSeconds: firstGJSONInt(body, "error.resets_in_seconds", "response.error.resets_in_seconds", "response.status_details.error.resets_in_seconds"),
	}, true
}

// IsUsageLimitReachedError reports whether an upstream error body represents
// account quota exhaustion, even when the transport status is incorrectly 5xx.
func IsUsageLimitReachedError(body []byte) bool {
	return strings.EqualFold(firstGJSONString(body, "error.type", "response.error.type", "response.status_details.error.type"), "usage_limit_reached")
}

func firstGJSONString(body []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstGJSONInt(body []byte, paths ...string) int64 {
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if result.Exists() {
			return result.Int()
		}
	}
	return 0
}

// outputGuardHTTPGateArmed checks whether the HTTP entry point should enforce the output runaway guard readiness gate.
// Production main always tries to load config before registering routes: on success IsOutputGuardReady,
// on failure lastSyncError is non-empty; both mean readiness has a conclusion. Only processes that never attempted
// loading (for example unit tests that construct Handler directly) keep the old pass-through semantics.
func outputGuardHTTPGateArmed() bool {
	if IsOutputGuardReady() {
		return true
	}
	return GetOutputGuardConfigSyncStatus().LastSyncError != ""
}

// Responses handles /v1/responses requests (native pass-through, with enhanced input validation)
func (h *Handler) Responses(c *gin.Context) {
	// 1. Read the request body
	handlerStart := time.Now()
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	if rejectDuplicateJSONField(c, rawBody, "input") {
		return
	}
	if rejectExcessiveJSONArray(c, rawBody, "input", maxResponsesInputItems) {
		return
	}
	if contextErr := responsesHTTPContextEnvelopeError(rawBody); contextErr != nil {
		api.SendError(c, contextErr)
		return
	}
	// [OutputGuard gate and state machine initialization] must be initialized once outside the attempt loop,
	// inherit event and byte budgets across attempts; never rebuild them on every attempt.
	if outputGuardHTTPGateArmed() && !RequireOutputGuardReadyHTTP(c) {
		return
	}
	guardSnapshot := CurrentOutputGuardConfig()
	reqGuard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               guardSnapshot.Enabled,
		MaxToolArgumentBytes:  guardSnapshot.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: guardSnapshot.MaxToolArgumentDeltas,
		MaxChannelBytes:       guardSnapshot.MaxChannelBytes,
		MaxRequestBytes:       guardSnapshot.MaxRequestBytes,
	})
	h.capturePromptRequestIngress(c, rawBody)
	bodyReadDone := time.Now()
	compactionMeta := requestCompactionMetaForHTTP(c, rawBody)
	cacheRequestCompactionMeta(c, compactionMeta)
	cacheRequestUltraMode(c, resolveRequestUltraMode(c.Request.Header, rawBody))

	// Native remote compaction v2: newer Codex clients embed the session compaction trigger as an
	// input item (type=compaction_trigger) in a normal /responses request, with stream=true.
	// This lane is the native /responses path. It does not require /responses/compact support, and it does not pin the request
	// to an official OAuth account. Relays that serve plain /responses can take it too
	// compaction_trigger. The old logic excluded relays while official accounts were still in the pool, so an official account's rate limit
	// or a model allowlist mismatch returned 503 at once (issue #540).
	// Non-streaming body-signal requests still go to the dedicated compact path, for relays that only implement
	// /responses/compact (issue #361: streaming cannot use a one-shot JSON response).
	bodySignalCompact := compactionMeta.ProtocolTriggered
	nativeRemoteCompactionV2 := bodySignalCompact && gjson.GetBytes(rawBody, "stream").Bool()
	if bodySignalCompact && !nativeRemoteCompactionV2 {
		h.ResponsesCompact(c)
		return
	}

	supportedModels := h.supportedModelIDs(c.Request.Context())
	var requestModel, mappedModel string
	var mappingApplied bool
	if nativeRemoteCompactionV2 {
		rawBody, requestModel, mappedModel, mappingApplied = h.applyConfiguredCompactModelMappingToBody(rawBody, supportedModels)
	} else {
		rememberDaybreakRequest(c, rawBody)
		rawBody, requestModel, mappedModel, mappingApplied = h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	}
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// Check the request body size
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "request body too large", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}

	// Validate the model parameter
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "invalid model parameter", "type": "invalid_request_error"},
		})
		return
	}

	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	pin := pinnedAccountDispatchForRequest(c)
	continuousRetryPolicy := pin.continuousRetryPolicy(continuousRetryPolicyForCall(nil))
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	turnContinuation := codexTurnContinuationToken(c.Request.Header, rawBody) != ""
	var turnAccountID int64
	var turnContinuationPinned bool
	if turnContinuation && pin == nil {
		turnAccountID, turnContinuationPinned = h.store.SessionAffinityAccountID(affinityKey)
	}
	quotaFailover := false
	ruleIdentity := h.payloadRuleIdentity(c)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	validateDone := time.Now()

	// 2. Prepare the Codex upstream request body (Unmarshal, map, Marshal, serialized once).
	// The previous_response_id cache is isolated per downstream API key, so one user cannot inject another user's conversation history.
	respCacheOwner := responseCacheOwner(apiKeyID)
	multiAgentV2State := &codexMultiAgentV2State{}
	if codexMultiAgentV2CompatEnabled() {
		c.Request = c.Request.WithContext(withCodexMultiAgentV2State(c.Request.Context(), multiAgentV2State))
	}
	bodyPreparation := prepareResponsesBodyForOwnerDetailed(rawBody, respCacheOwner)
	codexBody, expandedInputRaw := bodyPreparation.Body, bodyPreparation.ExpandedInputRaw
	continuationStatus, continuationReason, continuationUnavailable := responseCachePreparationFailure(bodyPreparation)
	// strip policy: remove the image tool capability declarations injected by the gateway or sent by the client, and continue as a plain text request (issue #411).
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	prepareDone := time.Now()
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	allowCodexAccounts := modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db))
	// A pinned request keeps only capability checks; cooldowns and pool
	// routing belong to the external scheduler.
	rateLimitGate := modelRateLimitGateFor(pin)
	var accountFilter auth.AccountFilter
	if nativeRemoteCompactionV2 {
		accountFilter = accountFilterForInlineCompactionModelWithGate(logModel, effectiveModel, allowCodexAccounts, rateLimitGate)
	} else {
		accountFilter = accountFilterForResponsesModelWithGate(logModel, effectiveModel, allowCodexAccounts, rateLimitGate)
	}
	if pin == nil {
		accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), effectiveModel, accountFilter)
	}
	if continuationUnavailable {
		accountFilter = responsesContinuationUnavailableFilter(accountFilter)
	}
	if pin != nil {
		accountFilter = daybreakAccountFilter(c, accountFilter)
	} else {
		accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
		accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	}
	// resolveCompactionAffinity only returns an error when the known sources conflict; a cache failure is handled as an unknown
	// source, and scheduling continues normally.
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		sendCompactionProvenanceConflict(c)
		return
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
		c.Request = c.Request.WithContext(withCompactionAffinity(c.Request.Context(), compactionAffinity))
	}
	// The scope concurrency slot is only taken after an account is selected, and it is released together when the request exits (issue #439 v2).
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolResponses)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetrySSEKeepalive(c, isStream, "text/event-stream")
	defer stopRetryKeepalive()
	activateContinuousRetryKeepalive(c.Request.Context())

	// 3. Upstream request with retries
	maxRetries := pin.retryLimit(h.getMaxRetries())
	maxRateLimitRetries := pin.retryLimit(h.getMaxRateLimitRetries())
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	var lastRetryAfter string
	retryExclusions := newRetryAccountExclusions()
	var wsHTTPFallback websocketHTTPFallbackState
	invalidEncryptedContentRetried := false
	overflowCompactRetried := false
	overflowCompactEnabled := autoCompactOverflowEnabled(c)

	// Upstream ctx lifecycle: before each attempt starts, replace it with a new drainable ctx,
	// and a defer ensures the upstream is released when the function exits.
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	capacityShedRetries := map[int64]int{}
	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	// The candidate pool for native remote compaction does not reuse the request channel; it only carries the scheduling policy.
	selectionOpts := selectionOptionsForCandidatePool(c, effectiveModel, dispatchPolicy, nativeRemoteCompactionV2)
	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback && pin != nil {
			// stickyProxyURL stays empty: the account's own proxy policy picks the route.
			affinityGuard = auth.SessionAffinityGuard{}
			account = pin.next(c, h.store, accountFilter, retryExclusions)
		} else if !retainedHTTPFallback {
			quotaAffinityReleased := false
			if turnContinuationPinned {
				if limited := h.quotaBlockedContinuationAccount(turnAccountID); limited != nil {
					if continuationUnavailable {
						sendResponseContextUnavailable(c, continuationStatus, continuationReason, bodyPreparation)
						return
					}
					replayRaw, contextErr := responsesQuotaReplayBody(rawBody, codexBody)
					if contextErr != nil {
						logQuotaFailoverRejected(c, "http", limited.ID(), attempt+1, rawBody, contextErr)
						api.SendError(c, contextErr)
						return
					}
					rawBody = replayRaw
					codexBody, _ = sjson.DeleteBytes(codexBody, "client_metadata.x-codex-turn-state")
					turnContinuationPinned = false
					quotaFailover = true
					quotaAffinityReleased = true
					h.store.UnbindSessionAffinity(affinityKey, limited.ID())
					log.Printf("[quota-failover] complete HTTP context admitted; releasing exhausted account affinity (account=%d, attempt=%d, encrypted_reasoning=%d)", limited.ID(), attempt+1, quotaFailoverEncryptedReasoning(rawBody))
				}
			}
			affinityGuard = auth.SessionAffinityGuard{}
			if attempt == 0 && compactionAffinity.Known && !turnContinuationPinned {
				account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			}
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			} else if continuationUnavailable {
				account, stickyProxyURL, affinityGuard = h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, retryExclusions.ForSelection(), accountFilter, selectionOpts)
			} else if turnContinuationPinned {
				// Chain-continuation retries also pass SelectionOptions, so a retry does not drop the account selection context.
				account, stickyProxyURL, selectionErr = h.nextRetryAccountForContinuationWithSelection(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
			} else if !quotaAffinityReleased || h.quotaFailoverHasCandidate(affinityKey, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
			}
		}
		if account == nil {
			if pin != nil {
				pin.writeFailure(c, continuousRetryProtocolResponses, isStream)
				return
			}
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolResponses) {
				return
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
				return
			}
			if lastStatusCode > 0 && len(lastBody) > 0 {
				if lastRetryAfter != "" {
					c.Header("Retry-After", lastRetryAfter)
				}
				if isStream && writeCommittedResponsesRetryError(c, usageLogErrorMessage(lastStatusCode, lastBody)) {
					return
				}
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if compactionAffinity.Known {
				if isStream && writeCommittedResponsesRetryError(c, "No account is available for the upstream that created this compaction state") {
					return
				}
				sendCompactionUpstreamUnavailable(c)
				return
			}
			// When the scope budget removes every candidate, report the real reason instead of the vague "no available account".
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				if isStream && writeCommittedResponsesRetryError(c, msg) {
					return
				}
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			if limited := h.store.UsageLimitedCandidateSummary(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy); limited.Found {
				// A transient throttle and quota exhaustion must send different signals downstream: the first carries Retry-After so the client
				// backs off by seconds and retries the same upstream; only the second is worth failing over or marking the account.
				msg := usageLimitedPoolMessages(limited)
				if isStream && writeCommittedResponsesRetryError(c, msg.English) {
					return
				}
				if msg.RetryAfterSeconds > 0 {
					c.Header("Retry-After", strconv.Itoa(msg.RetryAfterSeconds))
				}
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg.Chinese)
				return
			}
			if continuationUnavailable {
				if isStream && writeCommittedResponsesRetryError(c, "Previous response context is unavailable") {
					return
				}
				sendResponseContextUnavailable(c, continuationStatus, continuationReason, bodyPreparation)
				return
			}
			if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				setConcurrencySaturatedRetryAfter(c)
				if isStream && writeCommittedResponsesRetryError(c, concurrencySaturatedMessageZH) {
					return
				}
				c.JSON(http.StatusServiceUnavailable, concurrencySaturatedError())
				return
			}
			if msg, ok := h.unsupportedModelPoolMessage(apiKeyID, retryExclusions.ForSelection(), effectiveModel, dispatchPolicy); ok {
				if isStream && writeCommittedResponsesRetryError(c, msg) {
					return
				}
				c.JSON(http.StatusBadRequest, unsupportedModelPoolError(msg))
				return
			}
			if isStream && writeCommittedResponsesRetryError(c, noAvailableAccountMessage(effectiveModel)) {
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}
		if attempt > 0 {
			clearNewAPIUpstreamCyberPolicyDecision(c)
		}

		if attempt == 0 {
			emitResponsesPhaseTimings(c, logModel, len(rawBody), handlerStart, bodyReadDone, validateDone, prepareDone)
		}
		if compatibilityErr := selectedCodexResponsesCompatibilityError(c, account, rawBody); compatibilityErr != nil {
			h.store.Release(account)
			sendResponsesCompatibilityError(c, compatibilityErr)
			return
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		attemptMaxRateLimitRetries := pin.retryLimit(h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		// Pinned requests never read session affinity, so they do not write it.
		if pin == nil && !retainedHTTPFallback && !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return
			}
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("upstream WebSocket 1009, starting HTTP degraded attempt (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}
		attemptEffectiveModel := effectiveModel
		useWebsocket := h.shouldUseWebsocketForHTTP() && !wsHTTPFallback.ForceHTTP()
		// Image generation requests are forced onto HTTP: WebSocket transport hangs on large image payloads (issue #220);
		// a natural-language image generation intent also keeps the image_generation tool (issue #288).
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(rawBody) {
			useWebsocket = false
		}
		// When the body reaches the learned 1009 size threshold, send HTTP first directly, skipping the WS wait that always fails (issue #404).
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] request body %dKB reached the learned 1009 size threshold, going directly to HTTP upstream (endpoint=/v1/responses)", len(codexBody)/1024)
			}
		}

		// Extract the API key for device fingerprint stabilization
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// Use the injected device fingerprint config
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // Off by default
			}
		}

		// Pass through downstream request headers for fingerprint learning
		downstreamHeaders := c.Request.Header.Clone()
		if quotaFailover {
			downstreamHeaders.Del(codexTurnStateHeader)
		}

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		// The upstream uses a context decoupled from the client: if the client disconnects midway, the upstream is still read to the end
		// so response.completed carries usage (key for streaming billing).
		// lastUpstreamCancel is declared at the top of the attempt loop, with a defer as backup,
		// and cancels the previous round here before it is overwritten (on retries).
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		readCtx := upstreamResponseReadContext(c.Request.Context(), upstreamCtx, continuousRetryPolicy)
		upstreamCtx = context.WithValue(upstreamCtx, encryptedContentSessionKey{}, sessionIdentity.affinityID)
		// The identity is attached per attempt with the actually selected account dimension: account_* gates are matched again when a retry switches accounts (issue #410).
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
		// For WebSocket upstreams, strip the automatically injected image tools, so the model's own image generation does not produce large
		// data that hangs the WS stream (issue #220). Explicit image generation requests are already forced onto HTTP above.
		upstreamBody := codexBody
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		}
		// service_tier billing is attributed using the value after payload rules rewrite it (only rules that overwrite service_tier take effect).
		// Recalculated per attempt: the effective model and account can differ between attempts, and results change when rules match by model or account gate.
		serviceTier = EffectiveRequestedServiceTier(upstreamBody, attemptEffectiveModel, downstreamHeaders, attemptIdentity)
		// After an account switch, strip the turn-state echo minted by the old account, so contradictory cross-account signals do not reach the upstream.
		upstreamCtx = WithCodexTurnStateAffinityKey(upstreamCtx, affinityKey)
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		})
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				ttftGuard.Stop()
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("upstream WebSocket 1009, keeping the account lease and degrading to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			}
			// For transport failures, retry sticky on the same account: do not record an account failure, do not unbind affinity, do not hard-exclude (issue #331)
			// A busy acquire timeout does not stick to the same account: waiting again on the same key only queues again, so switch accounts directly (issue #413)
			stickyRetry := h.shouldStickyTransportRetry(reqErr, kind, timedOut, shouldRetry, continuousRetryPolicy)
			if retryable && shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable && !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("upstream first-token timeout, disconnecting and retrying (attempt %s, account %d, /v1/responses): %v", retryAttemptProgress(attempt, maxRetries), account.ID(), reqErr)
				if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), true, generalRetries, retryLimit) {
					return
				}
				continue
			}
			if retryable && !timedOut && !stickyRetry {
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			}

			// Return non-retryable structured errors directly
			if !retryable {
				if isStream && writeCommittedResponsesRetryError(c, continuousRetryRequestErrorMessage(reqErr)) {
					return
				}
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("upstream request failed (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return
				}
				if !h.bindBufferedStickyRetryAffinity(c.Request.Context(), affinityKey, account, proxyURL, stickyRetry, continuousRetryPolicy) {
					return
				}
				if stickyRetry {
					log.Printf("transport error sticky retry: keeping account %d and session affinity (attempt %s, /v1/responses)", account.ID(), retryAttemptProgress(attempt, maxRetries))
				}
				continue
			}
			if isStream && writeCommittedResponsesRetryError(c, continuousRetryRequestErrorMessage(reqErr)) {
				return
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			retryAfter := normalizedRetryAfter(resp.Header.Get("Retry-After"))
			errBody, _ := readUpstreamErrorBody(readCtx, resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			markPinnedUpstreamResponse(c, resp)
			if continuousRetryCommitExpired(c, continuousRetryProtocolResponses) {
				h.store.Release(account)
				return
			}
			accountReleasedForOverflow := false

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
					}
					if codexChanged {
						codexBody = strippedCodexBody
						expandedInputRaw = responsesInputRaw(codexBody)
					}
					log.Printf("upstream rejected encrypted_content, removed the encrypted reasoning context and retried once (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			// Context overflow with auto-compaction enabled for the key: summarize the old turns, then retry once with the same parameters (issue #415)
			if overflowCompactEnabled && !overflowCompactRetried &&
				resp.StatusCode == http.StatusBadRequest && isContextLengthExceededBody(errBody) {
				// The summary request needs to reuse the same key's routing and budget, but it must not hold
				// the current account or scope concurrency slot at the same time as the parent request, or a single-account pool would deadlock itself.
				h.ReleaseAPIKeyScopeConcurrency(c)
				h.store.Release(account)
				accountReleasedForOverflow = true
				if compacted, ok := h.compactOverflowResponsesBodyForRequest(c, codexBody); ok {
					overflowCompactRetried = true
					codexBody = compacted
					expandedInputRaw = responsesInputRaw(codexBody)
					log.Printf("upstream reported context overflow, compacted old turns and retried once (attempt %d)", attempt+1)
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			if !accountReleasedForOverflow {
				h.store.Release(account)
			}
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses",
				UpstreamEndpoint:       "/v1/responses",
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				clearNewAPIUpstreamCyberPolicyDecision(c)
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				lastRetryAfter = retryAfter
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}

			if retryAfter != "" {
				c.Header("Retry-After", retryAfter)
			}
			if isStream && writeCommittedResponsesRetryError(c, usageLogErrorMessage(resp.StatusCode, errBody)) {
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		if !isStream || !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			relayCodexTurnStateResponseHeader(c, affinityKey, account, attemptEffectiveModel, resp.Header)
		}
		SyncCodexUsageState(h.store, account, resp)
		// Success: pass the response through and track TTFT / usage
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		responseModelObserver := &upstreamResponseModelObserver{}
		ttftRecorded := false
		gotTerminal := false // Whether response.completed or response.failed was received
		deltaCharCount := 0  // Running count of delta characters (used to estimate tokens when the stream breaks)
		var readErr error
		var writeErr error
		wroteAnyBody := false
		// Set when a non-retryable response.failed arrives before the first token: abort SSE forwarding,
		// skip the transport flush (to avoid committing the 200 header early), and return JSON with the real error code after the loop.
		abortedForHTTPError := false
		// contentTokenSeen: whether a real content event has appeared (strict check, unrelated to the loose first-token statistics).
		// First-token stats use a loose rule: preliminary events such as codex.rate_limits set ttftRecorded, so "before the first token"
		// failure suppression, real error codes, and event buffering decisions use this flag instead.
		contentTokenSeen := false
		var responseJSON []byte
		var imageLogInfo imageUsageLogInfo
		var terminalFailurePayload []byte
		var preContentErrorCandidate []byte
		outputCollector := newResponseOutputCollector()
		var completedResponseData []byte
		var completedResponseOutputItems []json.RawMessage
		var compactionProvenancePayloads [][]byte
		promptPolicyIncidentID := ""
		upstreamCyberPolicyLogged := false
		var streamAttempt *continuousRetryStreamAttempt
		// Stream-break evidence (issue #491): distinguish downstream backpressure stalling upstream reads from the upstream resetting on its own.
		streamDiag := newStreamPhaseDiagnostics()
		// [OutputGuard] The atomic termination coordinator and Responses projector for this attempt:
		// streaming and non-streaming share one set of upstream cancellation, buffer cleanup, and 502 terminal semantics.
		guardCoord := NewGuardTerminationCoordinator(reqGuard, upstreamCancel, resp.Body)
		responsesProjector := NewResponsesProjector()

		if isStream {
			// Streaming pass-through with TTFT tracking
			setSSEStreamHeaders(c, "text/event-stream")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
					resp.Body.Close()
					h.store.Release(account)
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			streamAttempt = h.newContinuousRetryStreamAttempt(continuousRetryBuffersAttempts(continuousRetryPolicy), c.Writer, flusher)
			streamWriter := h.newAttemptStreamFlushWriter(c, streamAttempt, c.Writer, flusher)
			streamWriter.diag = streamDiag

			// clientGone: set after a client write fails; later events are no longer written to the client,
			// but the upstream is still read until response.completed/failed, to get accurate usage.
			clientGone := false
			// downstreamMu serializes the downstream write path and its shared state (clientGone/writeErr/
			// wroteAnyBody/streamWriter): the keepalive goroutine for automatic thinking continuation (issue #458)
			// writes the same ResponseWriter concurrently with forward, so it must be mutually exclusive. When continuation is off, there are
			// no concurrent writers, so the lock has zero contention.
			var downstreamMu sync.Mutex
			var pendingFirstTokenEvents bytes.Buffer
			contEnabled, contMaxRounds := codexContinueThinkingSettings()
			// Preliminary metadata events pass through immediately (legacy compatibility, issue #425): each attempt takes one snapshot,
			// hot updates apply to new requests, and the buffering policy does not switch in the middle of stream forwarding.
			preflightSettings := CurrentRuntimeSettings()
			preflightSettings.ContinuousRetryPolicy = continuousRetryPolicy
			preflightPassthrough := continuousRetryPreflightPassthrough(preflightSettings)
			emptyIncomplete := &emptyIncompleteTracker{}
			forwardWithEvent := func(sseEvent string, data []byte) bool {
				if multiAgentV2State.isOptimizedFor(account.ID()) {
					data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
				}
				streamDiag.markUpstreamFrame()
				// [OutputGuard pre-check] must run before provenance bytes.Clone, the collector
				// accumulation, replay, and the downstreamMu write path; a matched event is never visible to later logic.
				if err := ObserveEvent(reqGuard, responsesProjector, normalizedUpstreamSSEEventType(sseEvent, data), data); err != nil {
					gotTerminal = true
					guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						compactionProvenancePayloads = nil
						outputCollector.clearOverflow()
						streamWriter.DiscardBuffered()
						completedResponseData = nil
					})
					_ = SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", reqGuard.TrippedReason())
					return false
				}
				if continuousRetryBuffersAttempts(continuousRetryPolicy) {
					compactionProvenancePayloads = append(compactionProvenancePayloads, bytes.Clone(data))
				} else {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				}
				downstreamMu.Lock()
				defer downstreamMu.Unlock()
				// To extract usage, the upstream context is drained for up to 5 seconds after the client disconnects;
				// but once the downstream context is canceled, it must never try to write SSE again, or the next frame will certainly
				// become a broken pipe. Continued frame parsing is only used to get response.completed/usage.
				if c.Request.Context().Err() != nil {
					clientGone = true
				}
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())

				// TTFT: record the time of the first real content event
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isLooseFirstTokenResult(parsed)
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// contentTokenSeen uses the strict check (unrelated to the loose first-token statistics): under the loose rule,
				// preliminary events such as codex.rate_limits also set ttftRecorded, so if it were used for "before the first token"
				// checks, failure suppression, real error codes, and overflow compaction retries would all break.
				if !contentTokenSeen && isFirstTokenResult(parsed) {
					contentTokenSeen = true
				}
				if contentTokenSeen {
					preContentErrorCandidate = nil
				}

				// Accumulate delta character count
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if image, ok := extractImageFromOutputItemDone(data, logModel); ok {
					imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
				}
				outputCollector.Add(data)
				eventType, data, parsed = rewriteEmptyIncompleteTerminal(emptyIncomplete, eventType, data, parsed)

				// Extract usage, service_tier, and the upstream self-reported model
				observeUpstreamResponseModelFrame(responseModelObserver, parsed, eventType)
				if isResponsesSuccessTerminalEvent(eventType) {
					// Some gateways send a final response.output that is empty or only partial, even though
					// output_item.done events already arrived in full. Fill it in place before streaming pass-through, so SSE and
					// non-streaming responses get the same replayable terminal state.
					data = restoreMissingResponseOutputsInEvent(data, outputCollector.Items())
					parsed = gjson.ParseBytes(data)
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					if eventType == "response.completed" {
						// Cache only after the private replay reaches the downstream.
						// Otherwise a local filter/write failure would publish an ID that
						// the client never received. Truncated terminals remain uncached.
						completedResponseData = append(completedResponseData[:0], data...)
						completedResponseOutputItems = append(completedResponseOutputItems[:0], outputCollector.Items()...)
					}
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				if eventType == "response.failed" {
					var incidentID string
					var logged bool
					data, incidentID, logged = h.attachUpstreamCyberPolicyStreamDecision(c, "/v1/responses", logModel, data, upstreamCyberPolicyAttempt{
						Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: classifyResponseFailedOutcome(data).logStatusCode,
						AccountID: account.ID(), AttemptIndex: attempt + 1,
					})
					if logged {
						upstreamCyberPolicyLogged = true
						promptPolicyIncidentID = incidentID
					}
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				// `wroteAnyBody` counts bytes in the private attempt replay while
				// continuous retry is enabled. A standalone event:error must never
				// bypass that replay and reach the client directly.
				if eventType == "error" && continuousRetryBuffersAttempts(continuousRetryPolicy) {
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					return false
				}
				visibleBody := wroteAnyBody && !continuousRetryBuffersAttempts(continuousRetryPolicy)
				standaloneErrorAfterOutput := eventType == "error" && visibleBody
				if !contentTokenSeen && !visibleBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
					preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
					return true
				}
				if standaloneErrorAfterOutput {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
				}

				if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstTokenWithBudgets(eventType, terminalFailurePayload, contentTokenSeen, visibleBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
					pendingFirstTokenEvents.Reset()
					return false
				}

				// A response.failed before the first token is not written into the downstream stream: it is non-retryable (such as context_length_exceeded)
				// or the retry limit has been reached; it is returned after the loop with the real error code, not as 200 + [DONE], which would make relays bill it by mistake.
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentTokenSeen, visibleBody, clientGone) {
					pendingFirstTokenEvents.Reset()
					abortedForHTTPError = true
					return false
				}

				if !clientGone {
					// codex.* preliminary metadata events (rate_limits / response.metadata) and lifecycle
					// events are also held until the first token and flushed together: writing them out now would commit the 200 header and set
					// wroteAnyBody, so a response.failed before the first token (such as context_length_exceeded)
					// could neither return with the real error code nor take the overflow compaction retry path.
					// preflightPassthrough (issue #425) restores the legacy behavior: metadata events are sent immediately,
					// and the admin has explicitly accepted that cost. Lifecycle events (created/in_progress) are not affected by this switch.
					// Retryable error frames (the upstream overload lead-in frame) are not affected by preflightPassthrough,
					// and are always buffered: writing them out at once would set wroteAnyBody, and the following response.failed would then
					// never reach the silent account-switch or overflow compaction branches before the first packet. When they must be written out, rewrite the overload code.
					shouldDefer := shouldDeferPreContentSSEEvent(eventType, contentTokenSeen, gotTerminal, preflightPassthrough) ||
						(!contentTokenSeen && !visibleBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy))
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)
					if err != nil {
						writeErr = err
						clientGone = true
					} else if wrote {
						wroteAnyBody = true
					}
				}
				return !standaloneErrorAfterOutput && !isResponsesTerminalEvent(eventType)
			}
			forward := func(data []byte) bool { return forwardWithEvent("", data) }

			// Automatic continuation after thinking truncation (off by default): when on, forward is wrapped by a folding state machine,
			// and if the 518n-2 truncation fingerprint matches, the upstream is re-sent on the same account and folded into a single response;
			// when off, the original event-by-event pass-through path is kept, with zero byte-level change. The request-level keepalive
			// is responsible for SSE comments before and after the first model event.
			if contEnabled {
				requestKeepaliveOwnsWrites := continuousRetryKeepaliveActive(c.Request.Context()) && continuousRetryKeepaliveInterval > 0
				fold := &continueFold{
					trace:     func() upstreamTraceSnapshot { return snapshotUpstreamTrace(c.Request.Context()) },
					baseBody:  upstreamBody,
					maxRounds: contMaxRounds,
					forward:   forward,
					observe: func(data []byte) {
						// Buffered events (not yet forwarded to the client) are only used to keep the first-token timeout guard alive,
						// so a plain message response is not misjudged as timed out during the whole buffering period. Here it does not set
						// ttftRecorded/firstTokenMs: the client has not received any bytes yet, so the real
						// first-token timing only happens when flushBuffered flushes through forward,
						// otherwise the suppression and account-switch semantics of response.failed before the first packet would break.
						ttftGuard.MarkProgress(gjson.GetBytes(data, "type").String())
					},
					clientGone: func() bool {
						downstreamMu.Lock()
						gone := clientGone
						downstreamMu.Unlock()
						return gone || c.Request.Context().Err() != nil
					},
					keepalive: func() bool {
						downstreamMu.Lock()
						defer downstreamMu.Unlock()
						// The request-level heartbeat writes to the real ResponseWriter; it stays inside the folding ticker
						// so that hidden-round reads and heartbeat writes run in sequence.
						if requestKeepaliveOwnsWrites {
							if keepalive := continuousRetryKeepaliveForContext(c.Request.Context()); keepalive != nil {
								if err := keepalive.Keepalive(); err != nil {
									writeErr = err
									clientGone = true
									return false
								}
							}
							return !clientGone
						}
						// Never send keepalive before the first real byte is written: once a comment is written, the 200 header is committed,
						// and all the semantics of a response.failed before the first token (return with the real error code, or retry on another account)
						// would be destroyed (same pitfall as PR #318). There is no 200 to keep at this point either, so skip it.
						if clientGone || !wroteAnyBody {
							return !clientGone
						}
						if err := streamWriter.WriteSSEComment(continueKeepaliveComment); err != nil {
							writeErr = err
							clientGone = true
							return false
						}
						return true
					},
					openRound: func(body []byte) (*http.Response, error) {
						// The continuation round reuses the same account and upstream channel (reasoning encrypted_content is bound to the account,
						// so switching accounts would be rejected upstream), and keeps the drainable context decoupled from the client.
						roundBody := body
						if useWebsocket {
							roundBody = stripResponsesImageGenerationTool(body)
						}
						if lastUpstreamCancel != nil {
							lastUpstreamCancel()
						}
						rctx, rcancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
						// A hidden round gets exactly one request on this account. Failures
						// stay inside the fold and become a synthetic response.incomplete;
						// encrypted reasoning must never participate in account rotation.
						rctx = WithPayloadRuleIdentity(rctx, attemptIdentity)
						lastUpstreamCancel = rcancel
						roundResp, roundErr := ExecuteRequest(rctx, account, roundBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
						// The continuation round also consumes account quota: after a round starts successfully, sync the upstream usage headers,
						// otherwise the quota of multi-round hidden requests is invisible to automatic pausing and pacing.
						if roundErr == nil && roundResp != nil && roundResp.StatusCode == http.StatusOK {
							SyncCodexUsageState(h.store, account, roundResp)
						}
						return roundResp, roundErr
					},
					keepaliveInterval: continuousRetryKeepaliveInterval,
				}
				foldRes := runContinueThinkingFold(resp, fold)
				readErr = foldRes.ReadErr
				// Folding may produce a synthesized or rebuilt response.incomplete terminal state (continuation failure / EOF),
				// forward only sets gotTerminal for completed/failed, so it is filled in here from the folding result,
				// otherwise a folded stream that ends normally would be misjudged as broken: penalizing the account, unbinding affinity, and overwriting real usage with estimates.
				if foldRes.GotTerminal {
					gotTerminal = true
				}
				// Folding intercepts the real terminal state of each round, so forward may not see response.completed,
				// so the real usage of the final round, aggregated by folding, is used as this attempt's closing billing value.
				if foldRes.FinalUsage != nil {
					usage = foldRes.FinalUsage
				}
				// Each real round except the final one, plus each failed continuation round start, records one real usage entry,
				// and the final round is billed once by this attempt's closing step, to avoid double or missing entries.
				h.logContinueThinkingRounds(c, foldRes, account, logModel, logEffectiveModel, reasoningEffort, useWebsocket, serviceTier)
				if foldRes.FinalResponse != nil {
					resp = foldRes.FinalResponse
				}
			} else {
				readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, forwardWithEvent)
			}
			// Only flush at the end if a body was actually written: flusher.Flush commits the HTTP 200 header first,
			// so flushing early with zero bytes written would make c.JSON(4xx) outside the loop ineffective (the status is already fixed at 200).
			if writeErr == nil && wroteAnyBody {
				writeErr = streamWriter.Flush()
			}
			// The stream ended without a terminal event (upstream break): once body has been written, the whole response cannot be silently retried,
			// so a synthesized response.failed (code=upstream_stream_break) gives the downstream a programmatically
			// recognizable failure state instead of a silent EOF that looks like a "fake 200" (issue #473). When the whole
			// attempt is buffered, wroteAnyBody only means private buffering, and the outer layer can still discard the whole thing and retry.
			if shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				if err := writeResponsesStreamBreakEvent(streamWriter); err != nil {
					log.Printf("failed to write synthesized response.failed stream-break event (/v1/responses): %v", err)
				}
			}
		} else {
			// Non-streaming collection
			var lastResponseData []byte
			imageOutputs := make([]json.RawMessage, 0, 1)
			seenImageOutputs := make(map[string]struct{})
			emptyIncomplete := &emptyIncompleteTracker{}
			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				if multiAgentV2State.isOptimizedFor(account.ID()) {
					data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
				}
				// [OutputGuard pre-check] must run before provenance recording and aggregation.
				if err := ObserveEvent(reqGuard, responsesProjector, normalizedUpstreamSSEEventType(sseEvent, data), data); err != nil {
					gotTerminal = true
					guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						compactionProvenancePayloads = nil
						outputCollector.clearOverflow()
						completedResponseData = nil
					})
					return false
				}
				if continuousRetryBuffersAttempts(continuousRetryPolicy) {
					compactionProvenancePayloads = append(compactionProvenancePayloads, bytes.Clone(data))
				} else {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				}
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
				if eventType == "error" {
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					preContentErrorCandidate = nil
					return false
				}
				if isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
					preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
					return true
				}
				outputCollector.Add(data)
				if imageOutput, ok := extractResponseImageGenerationOutput(data, seenImageOutputs); ok {
					imageOutputs = append(imageOutputs, imageOutput)
				}
				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isLooseFirstTokenResult(parsed) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// Accumulate delta character count
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				eventType, data, parsed = rewriteEmptyIncompleteTerminal(emptyIncomplete, eventType, data, parsed)
				observeUpstreamResponseModelFrame(responseModelObserver, parsed, eventType)
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					if eventType == "response.completed" {
						completedResponseData = append(completedResponseData[:0], data...)
						completedResponseOutputItems = append(completedResponseOutputItems[:0], outputCollector.Items()...)
					}
					gotTerminal = true
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					responseJSON = []byte(responseObj.Raw)
					responseJSON = restoreMissingResponseOutputs(responseJSON, outputCollector.Items())
					responseJSON = appendMissingResponseImageOutputs(responseJSON, imageOutputs)
					imageLogInfo = imageUsageLogInfoFromResponseJSON(responseJSON)
				}
			}
		}

		// Stream-break detection and token estimation
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
		outcome = OverlayGuardOutcome(outcome, reqGuard)
		outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
		var candidatePromoted bool
		terminalFailurePayload, candidatePromoted = resolvePreContentRetryErrorCandidate(terminalFailurePayload, preContentErrorCandidate, contentTokenSeen, wroteAnyBody, gotTerminal, readErr, c.Request.Context().Err(), writeErr)
		if candidatePromoted && isStream {
			abortedForHTTPError = true
		}
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		outcome = annotateStreamBreakDiagnostics(outcome, streamDiag)
		ttftGuard.Stop()
		if outcome.verifyAccountAuth {
			h.store.VerifyAccountAuthAsync(account)
		}
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 && !outcome.terminalLocal {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			if withContinuousRetryDeadlinePending(c.Request.Context(), func() {
				responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
			}) {
				outcome = applyResponseFailedDecisionKind(outcome, terminalFailurePayload, responseFailedDecision)
			} else {
				outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
			}
			// Penalties from cyber_policy in a streaming response.failed (HTTP 200) must also be recorded,
			// otherwise only non-2xx error bodies would be written to the prompt filter log.
			if !upstreamCyberPolicyLogged {
				promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: outcome.logStatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
			}
			if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
				outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
			}
		}
		outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
		}
		downstreamWrote := streamAttempt.downstreamWrote(wroteAnyBody)
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, downstreamWrote, c.Request.Context().Err(), writeErr) {
			_ = streamAttempt.Close()
			wsElapsed := time.Since(start)
			resp.Body.Close()
			globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
			wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(outcome.failureMessage))
			log.Printf("upstream WebSocket 1009, keeping the account lease before the first packet and degrading to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), outcome.failureMessage)
			continue
		}
		if shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, downstreamWrote, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
			rememberContinuousRetryStreamFailure(c.Request.Context(), outcome, terminalFailurePayload)
			_ = streamAttempt.Close()
			clearNewAPIUpstreamCyberPolicyDecision(c)
			h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/responses", Model: logModel, EffectiveModel: logEffectiveModel,
				StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", Stream: isStream, ViaWebsocket: useWebsocket,
				AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
				ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			}, promptPolicyIncidentID)
			log.Printf("upstream stream dropped before the first packet, resetting the connection and retrying (attempt %s, account %d, /v1/responses): %s", retryAttemptProgress(attempt, maxRetries), account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.unbindOrRetainAffinityForCapacityShedWithGuard(retryExclusions, affinityKey, account, proxyURL, affinityGuard, outcome, capacityShedRetries, continuousRetryPolicy)
			if !isFirstTokenTimeoutOutcome(outcome) && !outcome.capacityShed &&
				retryLimitForStreamOutcome(outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) == -1 {
				retryExclusions.MarkStreamFailure(account.ID(), outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			}
			// A finite first-token timeout has already waited one full round for nothing; an unlimited budget still forces a backoff, to avoid a loop with no waiting.
			retryOrdinal, retryLimit := retryStateForStreamOutcome(outcome, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), isFirstTokenTimeoutOutcome(outcome), retryOrdinal, retryLimit, resp) {
				return
			}
			continue
		}
		if outcome.logStatusCode == http.StatusOK {
			if !claimContinuousRetrySuccess(c, continuousRetryProtocolResponses) {
				_ = streamAttempt.Close()
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			if commitErr := h.commitResponsesStreamAttempt(c, streamAttempt, affinityKey, account, attemptEffectiveModel, resp.Header); commitErr != nil {
				if isContinuousRetryLocalFailure(commitErr) {
					outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
				} else {
					abortContinuousRetryCommitFailure(h, account, resp, streamAttempt)
					return
				}
			} else {
				for _, payload := range compactionProvenancePayloads {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
				}
				if isStream && len(completedResponseData) > 0 {
					cacheCompletedResponseWithOutputItems(respCacheOwner, []byte(expandedInputRaw), completedResponseData, completedResponseOutputItems)
				}
			}
		}
		_ = streamAttempt.Close()

		if !continuousRetryBuffersAttempts(continuousRetryPolicy) || continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
			h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
		}
		logStatusCode := outcome.logStatusCode
		if logStatusCode != http.StatusOK {
			c.Set(AccessLogStatusContextKey, logStatusCode)
		}
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("stream ended abnormally (account %d, /v1/responses, status %d): %s, about %d characters forwarded", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3 // Rough estimate: about 3 characters = 1 token
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		accountReleasedForOverflow := false
		// Context overflow reported inside the stream (HTTP SSE and WS upstream take the same path) with auto-compaction enabled for the key:
		// if no bytes have been written downstream yet, summarize the old turns and retry once with the same parameters (issue #415).
		if !outcome.terminalLocal && overflowCompactEnabled && !overflowCompactRetried && !downstreamWrote &&
			(!isStream || abortedForHTTPError) &&
			isContextLengthExceededFailedPayload(terminalFailurePayload) {
			resp.Body.Close()
			h.ReleaseAPIKeyScopeConcurrency(c)
			h.store.Release(account)
			accountReleasedForOverflow = true
			if compacted, ok := h.compactOverflowResponsesBodyForRequest(c, codexBody); ok {
				overflowCompactRetried = true
				codexBody = compacted
				expandedInputRaw = responsesInputRaw(codexBody)
				log.Printf("upstream reported context overflow inside the stream, compacted old turns and retried once (attempt %d)", attempt+1)
				continue
			}
		}

		if outcome.guardTripped {
			if !downstreamWrote && !c.Writer.Written() {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{
						"message": outcome.failureMessage,
						"type":    outputguard.TriggeredCode,
						"code":    outputguard.TriggeredCode,
					},
				})
			}
		} else if isStream && outcome.terminalLocal {
			writeContinuousRetryLocalResponsesError(c)
		} else if isStream && abortedForHTTPError && !downstreamWrote {
			// Streaming: the upstream failed before the first token, nothing was written downstream, and the HTTP 200 header is not committed yet,
			// so overwrite the preset SSE Content-Type and return JSON with the real error code,
			// so downstream relays and billing do not treat it as a success and bill the estimated input tokens (this matches the reset inside the callback).
			if !writeCommittedResponsesRetryError(c, outcome.failureMessage) {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			}
		} else if isStream && !downstreamWrote && logStatusCode == logStatusUpstreamStreamBreak &&
			c.Request.Context().Err() == nil && writeErr == nil {
			// Stream break or first-token timeout before the first packet with retries exhausted: previously no write branch matched, so downstream
			// received a "fake 200" with an empty body, and the failure was completely invisible (issue #473). 598 is an internal
			// log status; externally it returns the real 502 with a stable error code, which downstream can recognize programmatically and retry.
			if !writeCommittedResponsesRetryError(c, outcome.failureMessage) {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
				})
			}
		} else if !isStream {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
				// The deadline owns the terminal response.
			} else if len(terminalFailurePayload) > 0 {
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			} else if responseJSON != nil {
				c.Header("Content-Type", "application/json")
				c.Status(http.StatusOK)
				if err := writeAll(c.Writer, responseJSON); err == nil {
					for _, payload := range compactionProvenancePayloads {
						h.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
					}
					if len(completedResponseData) > 0 {
						cacheCompletedResponseWithOutputItems(respCacheOwner, []byte(expandedInputRaw), completedResponseData, completedResponseOutputItems)
					}
				}
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "incomplete upstream response", "type": "upstream_error"},
				})
			}
		}

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:              account.ID(),
			Endpoint:               "/v1/responses",
			Model:                  logModel,
			EffectiveModel:         logEffectiveModel,
			StatusCode:             logStatusCode,
			DurationMs:             totalDuration,
			FirstTokenMs:           firstTokenMs,
			ReasoningEffort:        reasoningEffort,
			InboundEndpoint:        "/v1/responses",
			UpstreamEndpoint:       "/v1/responses",
			Stream:                 isStream,
			ViaWebsocket:           useWebsocket,
			ServiceTier:            usageTiers.ServiceTier,
			RequestedServiceTier:   usageTiers.RequestedServiceTier,
			ActualServiceTier:      usageTiers.ActualServiceTier,
			BillingServiceTier:     usageTiers.BillingServiceTier,
			PromptPolicyIncidentID: promptPolicyIncidentID,
			AttemptIndex:           attempt + 1,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogFailureMessage(logStatusCode, outcome.failureMessage)
			logInput.UpstreamErrorKind = outcome.failureKind
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
			logInput.ImageInputTokens, logInput.ImageOutputTokens, logInput.CachedImageInputTokens = usage.ImageInputTokens, usage.ImageOutputTokens, usage.CachedImageInputTokens
		}
		applyImageUsageLogInfo(logInput, imageLogInfo)
		// sentModel prefers the model actually sent in the attempt (account-level mapping may rewrite attemptEffectiveModel),
		// falling back to the model the client requested; when the upstream does not self-report, applyUpstreamResponseModelObservation does nothing.
		applyUpstreamResponseModelObservation(logInput, responseModelObserver, upstreamSentModelForAudit(attemptEffectiveModel, logModel), account.ID())
		h.logUsageForRequest(c, logInput)

		if !accountReleasedForOverflow {
			resp.Body.Close()
		}
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, effectiveModel)
			h.store.ConfirmResponsesAvailableSince(account, start)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		if !accountReleasedForOverflow {
			if outcome.logStatusCode == http.StatusOK {
				h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
			} else {
				h.store.Release(account)
			}
		}
		return
	}
}

// ResponsesCompact handles /v1/responses/compact requests (the non-streaming compaction endpoint, passed through to upstream /responses/compact)
func (h *Handler) ResponsesCompact(c *gin.Context) {
	// /v1/responses can hand body-signal compaction over to this handler, which
	// has no pinned-account path yet. Refuse instead of scheduling internally.
	if _, pinned := pinnedAccountIDFromContext(c); pinned {
		writeExternalSchedulerIngressError(c, "", http.StatusForbidden, "external_scheduler_endpoint_unsupported",
			"Remote compaction does not support pinned accounts yet, so external scheduler API keys cannot use it")
		return
	}
	// 1. Read the request body
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	if rejectDuplicateJSONField(c, rawBody, "input") {
		return
	}
	if rejectExcessiveJSONArray(c, rawBody, "input", maxResponsesInputItems) {
		return
	}
	if contextErr := responsesHTTPContextEnvelopeError(rawBody); contextErr != nil {
		api.SendError(c, contextErr)
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	cacheRequestCompactionMeta(c, requestCompactionMetaForHTTP(c, rawBody))

	supportedModels := h.supportedModelIDs(c.Request.Context())
	// First let the global and channel mappings see the client's original model (including the -openai-compact alias);
	// if no mapping matches, then strip the suffix by the compatibility rules.
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredCompactModelMappingToBody(rawBody, supportedModels)
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "request body too large", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	// routingModel keeps the client's original model name (it may carry the -openai-compact suffix), for account-level
	// compact mapping and account filtering to match alias rules; logModel is used for stats and log display, and the alias suffix
	// is only an endpoint routing convention, so it is always converted to the base model name for display (stripping the suffix alone does not count as a mapping, so no arrow is shown).
	routingModel := requestModel
	if routingModel == "" {
		routingModel = model
	}
	logModel := routingModel
	if baseModel, stripped := stripCompactModelSuffix(logModel); stripped {
		logModel = baseModel
	}
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "invalid model parameter", "type": "invalid_request_error"},
		})
		return
	}
	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if isImageOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses/compact", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolResponses)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetryHTTPInformationalKeepalive(c)
	defer stopRetryKeepalive()
	activateContinuousRetryKeepalive(c.Request.Context())
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// compact is forced to non-streaming
	rawBody, _ = sjson.SetBytes(rawBody, "stream", false)

	// Prepare the upstream request body (the previous_response_id cache is isolated per downstream API key)
	bodyPreparation := prepareCompactResponsesBodyForOwnerDetailed(rawBody, responseCacheOwner(apiKeyID))
	codexBody := bodyPreparation.Body
	continuationStatus, continuationReason, continuationUnavailable := responseCachePreparationFailure(bodyPreparation)
	// strip policy: remove the image tool capability declarations and continue as a plain text request (issue #411).
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	accountFilter := accountFilterForCompactResponsesModelWithOriginal(routingModel, effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), effectiveModel, accountFilter)
	if continuationUnavailable {
		// There is no local snapshot for previous_response_id and the Codex account cannot be restored upstream, so no account is allowed through.
		accountFilter = func(*auth.Account) bool { return false }
	}
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// resolveCompactionAffinity only returns an error when the known sources conflict; a cache failure is handled as an unknown
	// source, and scheduling continues normally.
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		sendCompactionProvenanceConflict(c)
		return
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
	}
	// The scope concurrency slot is only taken after an account is selected, and it is released together when the request exits (issue #439 v2).
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	// Upstream request with retries
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	invalidEncryptedContentRetried := false

	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	selectionOpts := selectionOptionsForCandidatePool(c, effectiveModel, dispatchPolicy, true)
	for attempt := 0; ; attempt++ {
		var account *auth.Account
		var stickyProxyURL string
		var affinityGuard auth.SessionAffinityGuard
		var selectionErr error
		if attempt == 0 && compactionAffinity.Known {
			account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			}
		}
		if account == nil {
			account, stickyProxyURL, affinityGuard = h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, retryExclusions.ForSelection(), accountFilter, selectionOpts)
		}
		if account == nil {
			if continuousRetryCommitExpired(c, continuousRetryProtocolResponses) {
				return
			}
			if compactionAffinity.Known && !retryExclusions.CanContinueTransientCycle() {
				if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
					return
				}
				sendCompactionUpstreamUnavailable(c)
				return
			}
			if continuationUnavailable && !retryExclusions.CanContinueTransientCycle() {
				if msg := scopeBudgetExhaustedMessage(c); msg != "" {
					if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
						return
					}
					SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
					return
				}
				if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
					return
				}
				sendResponseContextUnavailable(c, continuationStatus, continuationReason, bodyPreparation)
				return
			}
			account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
			if account == nil {
				if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolResponses) {
					return
				}
				if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
					return
				}
				if ((lastStatusCode == http.StatusTooManyRequests || lastStatusCode == http.StatusBadGateway) && len(lastBody) > 0) || lastFailureModelUnsupported(lastStatusCode, lastBody) {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				if msg := scopeBudgetExhaustedMessage(c); msg != "" {
					SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
					return
				}
				if compactionAffinity.Known {
					sendCompactionUpstreamUnavailable(c)
					return
				}
				if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
					setConcurrencySaturatedRetryAfter(c)
					c.JSON(http.StatusServiceUnavailable, concurrencySaturatedError())
					return
				}
				if msg, ok := h.unsupportedModelPoolMessage(apiKeyID, retryExclusions.ForSelection(), effectiveModel, dispatchPolicy); ok {
					c.JSON(http.StatusBadRequest, unsupportedModelPoolError(msg))
					return
				}
				c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
				return
			}
		}

		if compatibilityErr := selectedCodexResponsesCompatibilityError(c, account, rawBody); compatibilityErr != nil {
			h.store.Release(account)
			sendResponsesCompatibilityError(c, compatibilityErr)
			return
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
			h.store.Release(account)
			return
		}
		attemptEffectiveModel := effectiveModel

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		// compact (session compaction continuation) deliberately keeps the deterministic IsolateCodexSessionID and does not go through resolveUpstreamSessionID
		// default isolation: compaction is itself a continuation of the same session and needs a stable prompt_cache_key to keep cache continuity.
		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionIdentity.upstreamSeed)
		// compact_via_responses_enabled: the upstream has retired the dedicated /responses/compact endpoint (404),
		// so when enabled, official accounts switch to the body-signal form of /responses + compaction_trigger
		// (forced HTTP SSE), and on success the result is aggregated back into the one-shot JSON of compact.
		compactViaResponses := CurrentRuntimeSettings().CompactViaResponses
		upstreamEndpointLabel := "/v1/responses/compact"
		var resp *http.Response
		var reqErr error
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
		if compactViaResponses {
			upstreamEndpointLabel = "/v1/responses"
			resp, reqErr = executeHTTPWithContinuousRetryKeepalive(c.Request.Context(), func() (*http.Response, error) {
				return ExecuteRequest(c.Request.Context(), account, appendCompactionTriggerToResponsesBody(codexBody), upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, false)
			})
		} else {
			resp, reqErr = executeHTTPWithContinuousRetryKeepalive(c.Request.Context(), func() (*http.Response, error) {
				return ExecuteCompactRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
			})
		}
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries)
			}

			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("compact upstream request failed (attempt %d): %v", attempt+1, reqErr)
			if shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return
				}
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := readUpstreamErrorBody(c.Request.Context(), resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if continuousRetryCommitExpired(c, continuousRetryProtocolResponses) {
				h.store.Release(account)
				return
			}

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
					}
					if codexChanged {
						codexBody = strippedCodexBody
					}
					log.Printf("compact upstream rejected encrypted_content, removed the encrypted reasoning context and retried once (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			effectiveRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, effectiveRateLimitRetries)

			logUpstreamError("/v1/responses/compact", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: "http", StatusCode: resp.StatusCode, AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses/compact",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses/compact",
				UpstreamEndpoint:       upstreamEndpointLabel,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				clearNewAPIUpstreamCyberPolicyDecision(c)
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// Success: pass the response body through directly (in body-signal compatibility mode, the SSE is first aggregated into a one-shot JSON)
		var respBody []byte
		var readErr error
		var compactFailedPayload []byte
		if compactViaResponses {
			respBody, compactFailedPayload, readErr = collectCompactResponsesSSEWithContinuousRetryKeepalive(c.Request.Context(), resp.Body)
		} else {
			respBody, readErr = readAllLimitedWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, maxUpstreamResponseBodyBytes)
		}
		resp.Body.Close()
		if readErr != nil {
			totalDuration := int(time.Since(start).Milliseconds())
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), readErr, continuousRetryPolicy)
			kind := classifyTransportFailure(readErr)
			if kind == "" {
				kind = "transport"
			}
			if retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(totalDuration)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			if retryable {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				retryExclusions.MarkRequestFailure(account.ID(), readErr, maxRetries)
			}
			if !retryable && c.Request.Context().Err() != nil {
				return
			}
			shouldRetry := retryable && shouldRetryRequestError(readErr, &generalRetries, maxRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses/compact",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           http.StatusBadGateway,
				DurationMs:           totalDuration,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses/compact",
				UpstreamEndpoint:     upstreamEndpointLabel,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    kind,
				ErrorMessage:         fmt.Sprintf("upstream response read failed: %v", readErr),
			})
			log.Printf("compact upstream response read failed (attempt %d): %v", attempt+1, readErr)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), readErr)
				lastStatusCode = http.StatusBadGateway
				lastBody = []byte(`{"error":{"message":"Failed to read upstream response","type":"upstream_error","code":"upstream_read_error"}}`)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(readErr, maxRetries, continuousRetryPolicy)) {
					return
				}
				continue
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
				return
			}
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, "Failed to read upstream response", api.ErrorTypeUpstream), http.StatusBadGateway)
			return
		}
		// body-signal compatibility mode: a response.failed terminal state inside the SSE is handled as an upstream error,
		// matching the semantics of the non-200 HTTP branch of the legacy compact path (including the encrypted_content strip-and-retry).
		if compactViaResponses && len(compactFailedPayload) > 0 {
			const eventType = "response.failed"
			failureOutcome := classifyResponseFailedOutcome(compactFailedPayload)
			failStatus := failureOutcome.logStatusCode
			errBody := responseFailedErrorBody(compactFailedPayload)

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(failStatus, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
					}
					if codexChanged {
						codexBody = strippedCodexBody
					}
					log.Printf("compact(body-signal) upstream rejected encrypted_content, removed the encrypted reasoning context and retried once (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			var decision codex429Decision
			if !withContinuousRetryDeadlinePending(c.Request.Context(), func() {
				decision = h.applyResponseFailedCooldown(account, compactFailedPayload, resp, effectiveModel)
			}) {
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			failureOutcome = applyResponseFailedDecisionKind(failureOutcome, compactFailedPayload, decision)
			if failureOutcome.penalize {
				h.reportStreamOutcomeFailure(account, failureOutcome, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			effectiveRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)
			// Use the request snapshot so a hot reload cannot change a request
			// after its first upstream attempt.
			continuousPolicy := continuousRetryPolicy
			selectedContinuousFailure := continuousRetryStreamSelected(failureOutcome, compactFailedPayload, eventType, continuousPolicy)
			shouldRetry := false
			if selectedContinuousFailure {
				shouldRetry = shouldTransparentRetryStreamEventWithBudgets(
					failureOutcome,
					eventType,
					&generalRetries,
					&rateLimitRetries,
					maxRetries,
					effectiveRateLimitRetries,
					false,
					c.Request.Context().Err(),
					nil,
					continuousPolicy,
				)
			} else {
				shouldRetry = shouldRetryHTTPStatus(failStatus, errBody, &generalRetries, &rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousPolicy)
			}
			if shouldRetry {
				if selectedContinuousFailure {
					rememberContinuousRetryStreamFailure(c.Request.Context(), failureOutcome, compactFailedPayload)
				} else {
					rememberContinuousRetryFailure(c.Request.Context(), continuousRetryFailure{status: failStatus, body: errBody, contentType: "application/json"})
				}
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if selectedContinuousFailure {
				retryExclusions.MarkStreamFailureForEvent(account.ID(), failureOutcome, eventType, maxRetries, effectiveRateLimitRetries, continuousPolicy)
			} else {
				retryExclusions.MarkHTTPFailure(account.ID(), failStatus, errBody, maxRetries, effectiveRateLimitRetries, continuousPolicy)
			}

			logUpstreamError("/v1/responses/compact", failStatus, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: "http", StatusCode: failStatus, AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses/compact",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             failStatus,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses/compact",
				UpstreamEndpoint:       upstreamEndpointLabel,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(failStatus, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(failStatus, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				clearNewAPIUpstreamCyberPolicyDecision(c)
				lastStatusCode = failStatus
				lastBody = errBody
				var retryOrdinal, retryLimit int
				if selectedContinuousFailure {
					retryOrdinal, retryLimit = retryStateForStreamEvent(failureOutcome, eventType, generalRetries, rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousPolicy)
				} else {
					retryOrdinal, retryLimit = retryStateForHTTPStatusWithBody(failStatus, errBody, generalRetries, rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousPolicy)
				}
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, failStatus, errBody)
			return
		}

		if !claimContinuousRetrySuccess(c, continuousRetryProtocolResponses) {
			h.store.Release(account)
			return
		}
		SyncCodexUsageState(h.store, account, resp)
		h.store.ClearModelCooldown(account, effectiveModel)

		// Extract usage for logging
		promptTokens := int(gjson.GetBytes(respBody, "usage.input_tokens").Int())
		completionTokens := int(gjson.GetBytes(respBody, "usage.output_tokens").Int())
		totalTokens := int(gjson.GetBytes(respBody, "usage.total_tokens").Int())
		reasoningTokens := int(gjson.GetBytes(respBody, "usage.output_tokens_details.reasoning_tokens").Int())
		cachedTokens := int(gjson.GetBytes(respBody, "usage.input_tokens_details.cached_tokens").Int())

		actualServiceTier := gjson.GetBytes(respBody, "service_tier").String()
		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)

		totalDuration := int(time.Since(start).Milliseconds())
		compactObserver := &upstreamResponseModelObserver{}
		observeUpstreamResponseModelBody(compactObserver, respBody)
		compactLogInput := &database.UsageLogInput{
			AccountID:            account.ID(),
			Endpoint:             "/v1/responses/compact",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           http.StatusOK,
			DurationMs:           totalDuration,
			PromptTokens:         promptTokens,
			CompletionTokens:     completionTokens,
			TotalTokens:          totalTokens,
			InputTokens:          promptTokens,
			OutputTokens:         completionTokens,
			ReasoningTokens:      reasoningTokens,
			CachedTokens:         cachedTokens,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/responses/compact",
			UpstreamEndpoint:     upstreamEndpointLabel,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		}
		applyUpstreamResponseModelObservation(compactLogInput, compactObserver, upstreamSentModelForAudit(attemptEffectiveModel, logModel), account.ID())
		h.logUsageForRequest(c, compactLogInput)

		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
		c.Data(http.StatusOK, "application/json", respBody)
		h.recordCompactionProvenanceFromPayload(context.Background(), account, respBody)
		return
	}
}

// ChatCompletions handles OpenAI Chat Completions requests and sends protocol keepalives in streaming responses.
func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. Read the request body
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	// [OutputGuard gate and state machine setup] must be initialized once outside the attempt loop,
	// events and byte budgets are inherited across attempts, and must never be rebuilt on each attempt.
	if outputGuardHTTPGateArmed() && !RequireOutputGuardReadyHTTP(c) {
		return
	}
	guardSnapshot := CurrentOutputGuardConfig()
	reqGuard := outputguard.NewRequestGuard(outputguard.Config{
		Enabled:               guardSnapshot.Enabled,
		MaxToolArgumentBytes:  guardSnapshot.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: guardSnapshot.MaxToolArgumentDeltas,
		MaxChannelBytes:       guardSnapshot.MaxChannelBytes,
		MaxRequestBytes:       guardSnapshot.MaxRequestBytes,
	})
	if rejectDuplicateJSONField(c, rawBody, "messages") {
		return
	}
	if rejectExcessiveJSONArray(c, rawBody, "messages", maxChatCompletionMessages) {
		return
	}
	h.capturePromptRequestIngress(c, rawBody)

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rememberDaybreakRequest(c, rawBody)
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// Check the request body size
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "request body too large", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if mappedModel != "" {
		model = mappedModel
	}
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}
	responseModel := logModel
	if model == "" {
		model = defaultCodexFallbackModel
		logModel = model
		responseModel = model
	}
	if isImageOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}

	// Validate the model parameter
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "invalid model parameter", "type": "invalid_request_error"},
		})
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	pin := pinnedAccountDispatchForRequest(c)
	continuousRetryPolicy := pin.continuousRetryPolicy(continuousRetryPolicyForCall(nil))
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	reasoningEffort := extractReasoningEffort(rawBody)
	ruleIdentity := h.payloadRuleIdentity(c)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// 2. Translate the request: OpenAI Chat → Codex Responses
	codexBody, err := TranslateRequest(rawBody)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request translation failed: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	// Tool names may be sanitized and rewritten during conversion (codex_tool_names.go); the response side restores them using this table.
	toolNameRestore := ChatToolNameRestoreMap(rawBody)
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	// /v1/chat/completions is served by Codex accounts: the translated request body is already in the Responses form (issue #181).
	// A pinned request keeps only capability checks; cooldowns and pool
	// routing belong to the external scheduler.
	rateLimitGate := modelRateLimitGateFor(pin)
	accountFilter := accountFilterForResponsesModelWithGate(logModel, effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)), rateLimitGate)
	if pin == nil {
		accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), effectiveModel, accountFilter)
	}
	if pin != nil {
		accountFilter = daybreakAccountFilter(c, accountFilter)
	} else {
		accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	}
	// The scope concurrency slot is only taken after an account is selected, and it is released together when the request exits (issue #439 v2).
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolChat)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetrySSEKeepalive(c, isStream, "text/event-stream")
	defer stopRetryKeepalive()
	activateContinuousRetryKeepalive(c.Request.Context())

	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, codexBody)
	if pin == nil {
		accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	}
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)

	// 3. Upstream request with retries
	maxRetries := pin.retryLimit(h.getMaxRetries())
	maxRateLimitRetries := pin.retryLimit(h.getMaxRateLimitRetries())
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	var wsHTTPFallback websocketHTTPFallbackState

	// Upstream ctx lifecycle: before each attempt starts, replace it with a new drainable ctx,
	// and a defer ensures the upstream is released when the function exits.
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	capacityShedRetries := map[int64]int{}
	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	selectionOpts := selectionOptionsForRequest(c, effectiveModel, dispatchPolicy)
	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback && pin != nil {
			// stickyProxyURL stays empty: the account's own proxy policy picks the route.
			affinityGuard = auth.SessionAffinityGuard{}
			account = pin.next(c, h.store, accountFilter, retryExclusions)
		} else if !retainedHTTPFallback {
			affinityGuard = auth.SessionAffinityGuard{}
			account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
		}
		if account == nil {
			if pin != nil {
				pin.writeFailure(c, continuousRetryProtocolChat, isStream)
				return
			}
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolChat) {
				return
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolChat) {
				return
			}
			if (lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0) || lastFailureModelUnsupported(lastStatusCode, lastBody) {
				if isStream && writeCommittedChatRetryError(c, usageLogErrorMessage(lastStatusCode, lastBody)) {
					return
				}
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			// When the scope budget removes every candidate, report the real reason instead of the vague "no available account".
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				if isStream && writeCommittedChatRetryError(c, msg) {
					return
				}
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				setConcurrencySaturatedRetryAfter(c)
				if isStream && writeCommittedChatRetryError(c, concurrencySaturatedMessageZH) {
					return
				}
				c.JSON(http.StatusServiceUnavailable, concurrencySaturatedError())
				return
			}
			if msg, ok := h.unsupportedModelPoolMessage(apiKeyID, retryExclusions.ForSelection(), effectiveModel, dispatchPolicy); ok {
				if isStream && writeCommittedChatRetryError(c, msg) {
					return
				}
				c.JSON(http.StatusBadRequest, unsupportedModelPoolError(msg))
				return
			}
			if isStream && writeCommittedChatRetryError(c, noAvailableAccountMessage(effectiveModel)) {
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}
		if attempt > 0 {
			clearNewAPIUpstreamCyberPolicyDecision(c)
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		attemptMaxRateLimitRetries := pin.retryLimit(h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		// Pinned requests never read session affinity, so they do not write it.
		if pin == nil && !retainedHTTPFallback && !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return
			}
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("upstream WebSocket 1009, starting HTTP degraded attempt (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}
		attemptEffectiveModel := effectiveModel
		attemptLogEffectiveModel := logEffectiveModel
		useWebsocket := h.shouldUseWebsocketForHTTP() && !wsHTTPFallback.ForceHTTP()
		// A real image generation intent is forced onto HTTP: WebSocket transport hangs on large image data (issue #220).
		// The injected image_generation tool alone does not trigger degradation; normal requests keep using WS (issue #304).
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(codexBody) {
			useWebsocket = false
		}
		// When the body reaches the learned 1009 size threshold, send HTTP first directly, skipping the WS wait that always fails (issue #404).
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] request body %dKB reached the learned 1009 size threshold, going directly to HTTP upstream (endpoint=/v1/chat/completions)", len(codexBody)/1024)
			}
		}
		upstreamEndpoint := "/v1/responses"

		// Extract the API key for device fingerprint stabilization
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// Use the injected device fingerprint config
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // Off by default
			}
		}

		// Pass through downstream request headers for fingerprint learning
		downstreamHeaders := c.Request.Header.Clone()

		// The identity is attached per attempt with the actually selected account dimension: account_* gates are matched again when a retry switches accounts (issue #410).
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		// service_tier billing is attributed using the value after payload rules rewrite it (only rules that overwrite service_tier take effect).
		// Recalculated per attempt: the effective model and account can differ between attempts, and results change when rules match by model or account gate.
		serviceTier = EffectiveRequestedServiceTier(codexBody, attemptEffectiveModel, downstreamHeaders, attemptIdentity)

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		// The upstream uses a context decoupled from the client: if the client disconnects midway, the upstream is still read to the end
		// so response.completed carries usage (key for streaming billing).
		// lastUpstreamCancel is declared at the top of the attempt loop, with a defer as backup,
		// and cancels the previous round here before it is overwritten (on retries).
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		readCtx := upstreamResponseReadContext(c.Request.Context(), upstreamCtx, continuousRetryPolicy)
		upstreamCtx = context.WithValue(upstreamCtx, encryptedContentSessionKey{}, sessionIdentity.affinityID)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		var resp *http.Response
		var reqErr error
		// For WebSocket upstreams, strip the automatically injected image tools, so the model's own image generation does not hang the WS stream (issue #220).
		upstreamBody := codexBody
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		}
		upstreamCtx = WithCodexTurnStateAffinityKey(upstreamCtx, affinityKey)
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
		resp, reqErr = executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		})
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				ttftGuard.Stop()
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("upstream WebSocket 1009, keeping the account lease and degrading to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			}
			// For transport failures, retry sticky on the same account: do not record an account failure, do not unbind affinity, do not hard-exclude (issue #331)
			// A busy acquire timeout does not stick to the same account: waiting again on the same key only queues again, so switch accounts directly (issue #413)
			stickyRetry := h.shouldStickyTransportRetry(reqErr, kind, timedOut, shouldRetry, continuousRetryPolicy)
			if retryable && shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable && !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("upstream first-token timeout, disconnecting and retrying (attempt %s, account %d, /v1/chat/completions): %v", retryAttemptProgress(attempt, maxRetries), account.ID(), reqErr)
				if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), true, generalRetries, retryLimit) {
					return
				}
				continue
			}
			if retryable && !timedOut && !stickyRetry {
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			}

			// Return non-retryable structured errors directly
			if !retryable {
				if isStream && writeCommittedChatRetryError(c, continuousRetryRequestErrorMessage(reqErr)) {
					return
				}
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("upstream request failed (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return
				}
				if !h.bindBufferedStickyRetryAffinity(c.Request.Context(), affinityKey, account, proxyURL, stickyRetry, continuousRetryPolicy) {
					return
				}
				if stickyRetry {
					log.Printf("transport error sticky retry: keeping account %d and session affinity (attempt %s, /v1/chat/completions)", account.ID(), retryAttemptProgress(attempt, maxRetries))
				}
				continue
			}
			if isStream && writeCommittedChatRetryError(c, continuousRetryRequestErrorMessage(reqErr)) {
				return
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, _ := readUpstreamErrorBody(readCtx, resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			markPinnedUpstreamResponse(c, resp)
			if continuousRetryCommitExpired(c, continuousRetryProtocolChat) {
				h.store.Release(account)
				return
			}
			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, attemptEffectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/chat/completions",
				Model:                  logModel,
				EffectiveModel:         attemptLogEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/chat/completions",
				UpstreamEndpoint:       upstreamEndpoint,
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				clearNewAPIUpstreamCyberPolicyDecision(c)
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}

			if isStream && writeCommittedChatRetryError(c, usageLogErrorMessage(resp.StatusCode, errBody)) {
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		SyncCodexUsageState(h.store, account, resp)

		// Success: translate the response and track TTFT
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		responseModelObserver := &upstreamResponseModelObserver{}
		ttftRecorded := false
		// TTFT may use loose structural progress, but retry safety is based on
		// actual content. Chat translation drops many structural events, so
		// treating loose TTFT as output would strand a retryable failure even
		// though no downstream bytes were written.
		contentTokenSeen := false
		gotTerminal := false // Whether response.completed or response.failed was received
		deltaCharCount := 0  // Running count of delta characters (used to estimate tokens when the stream breaks)
		var readErr error
		var writeErr error
		wroteAnyBody := false
		// Set when a non-retryable response.failed arrives before the first token: abort SSE forwarding,
		// skip the transport flush (to avoid committing the 200 header early), and return JSON with the real error code after the loop.
		abortedForHTTPError := false
		var compactResult []byte
		var terminalFailurePayload []byte
		var preContentErrorCandidate []byte
		promptPolicyIncidentID := ""
		upstreamCyberPolicyLogged := false
		var streamAttempt *continuousRetryStreamAttempt

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		emptyIncomplete := &emptyIncompleteTracker{}
		created := time.Now().Unix()
		// [OutputGuard] The upstream is always Responses SSE; the projector and termination coordinator are
		// shared between streaming and non-streaming, which guarantees that a match runs the atomic termination only once.
		guardCoord := NewGuardTerminationCoordinator(reqGuard, upstreamCancel, resp.Body)
		chatResponsesProjector := NewResponsesProjector()

		if isStream {
			streamTranslator := NewStreamTranslator(chunkID, responseModel, created)
			streamTranslator.SetToolNameRestore(toolNameRestore)
			setSSEStreamHeaders(c, "text/event-stream")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				if !claimContinuousRetryTerminal(c, continuousRetryProtocolChat) {
					resp.Body.Close()
					h.store.Release(account)
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			streamAttempt = h.newContinuousRetryStreamAttempt(continuousRetryBuffersAttempts(continuousRetryPolicy), c.Writer, flusher)
			streamWriter := h.newAttemptStreamFlushWriter(c, streamAttempt, c.Writer, flusher)

			// clientGone: set after a client write fails; later events are no longer written to the client,
			// but the upstream is still read until response.completed/failed, to get accurate usage.
			clientGone := false
			var pendingFirstTokenChunks bytes.Buffer
			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				// [OutputGuard pre-check] must run before streamTranslator accumulation and downstream writes.
				if err := ObserveEvent(reqGuard, chatResponsesProjector, eventType, data); err != nil {
					gotTerminal = true
					guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						pendingFirstTokenChunks.Reset()
						streamWriter.DiscardBuffered()
					})
					_ = SendDownstreamGuardTerminalHTTP(c, nil, &clientGone, "chat", reqGuard.TrippedReason())
					return false
				}
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
				if eventType == "response.failed" {
					statusCode := classifyResponseFailedOutcome(data).logStatusCode
					var incidentID string
					var logged bool
					data, incidentID, logged = h.attachUpstreamCyberPolicyStreamDecision(c, "/v1/chat/completions", logModel, data, upstreamCyberPolicyAttempt{
						Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: statusCode,
						AccountID: account.ID(), AttemptIndex: attempt + 1,
					})
					if logged {
						upstreamCyberPolicyLogged = true
						promptPolicyIncidentID = incidentID
						parsed = gjson.ParseBytes(data)
					}
				}
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isLooseFirstTokenResult(parsed)
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				if !contentTokenSeen && isFirstTokenResult(parsed) {
					contentTokenSeen = true
				}
				if contentTokenSeen {
					preContentErrorCandidate = nil
				}
				// Accumulate delta character count (text + function call arguments)
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				eventType, data, parsed = rewriteEmptyIncompleteTerminal(emptyIncomplete, eventType, data, parsed)
				observeUpstreamResponseModelFrame(responseModelObserver, parsed, eventType)
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					preContentErrorCandidate = nil
				}
				visibleBody := wroteAnyBody && !continuousRetryBuffersAttempts(continuousRetryPolicy)
				if eventType == "error" && continuousRetryBuffersAttempts(continuousRetryPolicy) {
					// Keep stream errors inside the private attempt replay. The outer
					// loop decides whether to discard and rotate accounts.
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					return false
				}
				if !contentTokenSeen && !visibleBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
					preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
					return true
				}
				if eventType == "error" && visibleBody && !clientGone {
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					// In continuous mode this attempt is private even after it has
					// produced content. Keep the error in the attempt outcome so the
					// outer loop can discard the replay and rotate accounts. Writing
					// here would leak a failed attempt before retry selection.
					if !continuousRetryBuffersAttempts(continuousRetryPolicy) {
						writeCommittedChatRetryError(c, classifyResponseFailedOutcome(data).failureMessage)
					}
					return false
				}
				translation := streamTranslator.TranslateParsedResult(parsed)
				chunk, done := translation.Chunk, translation.Terminal
				if translation.Failed && eventType != "response.failed" {
					if toolErr := streamTranslator.ToolArgumentsError(); toolErr != nil {
						terminalFailurePayload = malformedToolArgumentsFailurePayload(toolErr)
						gotTerminal = true
						preContentErrorCandidate = nil
					}
				}

				if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstTokenWithBudgets(eventType, terminalFailurePayload, contentTokenSeen, visibleBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
					pendingFirstTokenChunks.Reset()
					return false
				}

				// A response.failed before the first token is not written into the downstream stream: it is non-retryable (such as context_length_exceeded)
				// or the retry limit has been reached; it is returned after the loop with the real error code, not as 200 + [DONE], which would make relays bill it by mistake.
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentTokenSeen, visibleBody, clientGone) {
					pendingFirstTokenChunks.Reset()
					abortedForHTTPError = true
					return false
				}

				if !clientGone && chunk != nil {
					shouldDefer := !contentTokenSeen && !gotTerminal && isPreContentLifecycleEvent(eventType)
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenChunks, chunk, shouldDefer)
					if err != nil {
						writeErr = err
						clientGone = true
					} else if wrote {
						wroteAnyBody = true
					}
					if shouldDefer && !wrote {
						return !isResponsesTerminalEvent(eventType)
					}
				}
				if !clientGone && done {
					// response.failed is already represented by the translated stream
					// error chunk above. [DONE] is a successful Chat Completions
					// sentinel; appending it here would disguise a failed generation as
					// a clean terminal response to downstream gateways.
					if translation.Failed {
						writeErr = streamWriter.Flush()
						if writeErr != nil {
							clientGone = true
						} else {
							wroteAnyBody = true
						}
						return false
					}
					if pendingFirstTokenChunks.Len() > 0 {
						pendingFirstTokenChunks.WriteString("data: [DONE]\n\n")
						writeErr = streamWriter.WriteBytes(pendingFirstTokenChunks.Bytes())
						pendingFirstTokenChunks.Reset()
					} else {
						writeErr = streamWriter.WriteString("data: [DONE]\n\n")
					}
					if writeErr != nil {
						clientGone = true
					} else if err := streamWriter.Flush(); err != nil {
						writeErr = err
						clientGone = true
					} else {
						wroteAnyBody = true
					}
					if !clientGone {
						return false
					}
				}
				// After the client disconnects, keep going until the terminal event before exiting, to make sure usage is received.
				if gotTerminal {
					return false
				}
				return true
			})
			// Only flush at the end if a body was actually written: flusher.Flush commits the HTTP 200 header first,
			// so flushing early with zero bytes written would make c.JSON(4xx) outside the loop ineffective (the status is already fixed at 200).
			if writeErr == nil && wroteAnyBody {
				writeErr = streamWriter.Flush()
			}
			// Upstream break after the body was written: synthesize an in-stream error object (code=upstream_stream_break)
			// without adding [DONE], so the downstream can recognize the failure programmatically and retry, instead of treating the truncated stream as a success
			// (issue #473).
			if shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				if err := writeChatCompletionsStreamBreakEvent(streamWriter); err != nil {
					log.Printf("failed to write synthesized error stream-break event (/v1/chat/completions): %v", err)
				}
			}
		} else {
			var fullContent strings.Builder
			var fullReasoning strings.Builder
			var toolCalls []ToolCallResult
			outputCollector := newResponseOutputCollector()
			var finishReasonOverride string

			readErr = readSSEStreamWithContinuousRetryKeepalive(readCtx, resp.Body, func(sseEvent string, data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := normalizedUpstreamSSEEventType(sseEvent, data)
				// [OutputGuard pre-check] must run before fullContent/fullReasoning accumulation.
				if err := ObserveEvent(reqGuard, chatResponsesProjector, eventType, data); err != nil {
					gotTerminal = true
					guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
						outputCollector.clearOverflow()
					})
					return false
				}
				outputCollector.Add(data)
				ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isLooseFirstTokenResult(parsed) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				eventType, data, parsed = rewriteEmptyIncompleteTerminal(emptyIncomplete, eventType, data, parsed)
				switch eventType {
				case "response.output_text.delta":
					delta := parsed.Get("delta").String()
					deltaCharCount += len(delta)
					fullContent.WriteString(delta)
				case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
					fullReasoning.WriteString(parsed.Get("delta").String())
				case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
					deltaCharCount += len(parsed.Get("delta").String())
				case "response.completed", "response.incomplete":
					observeUpstreamResponseModelFrame(responseModelObserver, parsed, eventType)
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					finishReasonOverride = responsesIncompleteFinishReason(eventType,
						parsed.Get("response.incomplete_details.reason").String())
					// Extract function_call items from response.output. For a plain function's
					// arguments that were truncated, the whole upstream response is handled as a protocol error, and a broken call must not be
					// returned and keep polluting the history in the next round.
					var toolErr error
					toolCalls, toolErr = ExtractToolCallsFromOutputValidated(restoreMissingResponseOutputsInEvent(data, outputCollector.Items()))
					if toolErr != nil {
						terminalFailurePayload = malformedToolArgumentsFailurePayload(toolErr)
					}
					toolCalls = restoreToolCallNames(toolCalls, toolNameRestore)
					gotTerminal = true
					preContentErrorCandidate = nil
					return false
				case "response.failed":
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					preContentErrorCandidate = nil
					return false
				case "error":
					terminalFailurePayload = terminalUpstreamErrorPayload(data)
					gotTerminal = true
					preContentErrorCandidate = nil
					return false
				}
				return true
			})

			compactResult = BuildCompactChatResponse(chunkID, responseModel, created, fullContent.String(), fullReasoning.String(), toolCalls, usage, finishReasonOverride, actualServiceTier)
		}

		// Stream-break detection and token estimation
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
		outcome = OverlayGuardOutcome(outcome, reqGuard)
		var candidatePromoted bool
		terminalFailurePayload, candidatePromoted = resolvePreContentRetryErrorCandidate(terminalFailurePayload, preContentErrorCandidate, contentTokenSeen, wroteAnyBody, gotTerminal, readErr, c.Request.Context().Err(), writeErr)
		if candidatePromoted && isStream {
			abortedForHTTPError = true
		}
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		ttftGuard.Stop()
		if outcome.verifyAccountAuth {
			h.store.VerifyAccountAuthAsync(account)
		}
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 && !outcome.terminalLocal {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			if withContinuousRetryDeadlinePending(c.Request.Context(), func() {
				responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, attemptEffectiveModel)
			}) {
				outcome = applyResponseFailedDecisionKind(outcome, terminalFailurePayload, responseFailedDecision)
			} else {
				outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
			}
			// Penalties from cyber_policy in a streaming response.failed (HTTP 200) must also be recorded,
			// otherwise only non-2xx error bodies would be written to the prompt filter log.
			if !upstreamCyberPolicyLogged {
				promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: outcome.logStatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
			}
			if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
				outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
			}
		}
		outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
		}
		downstreamWrote := streamAttempt.downstreamWrote(wroteAnyBody)
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, downstreamWrote, c.Request.Context().Err(), writeErr) {
			_ = streamAttempt.Close()
			wsElapsed := time.Since(start)
			resp.Body.Close()
			globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
			wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(outcome.failureMessage))
			log.Printf("upstream WebSocket 1009, keeping the account lease before the first packet and degrading to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), outcome.failureMessage)
			continue
		}
		if shouldTransparentRetryStreamWithBudgets(outcome, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, downstreamWrote, c.Request.Context().Err(), writeErr, continuousRetryPolicy) {
			rememberContinuousRetryStreamFailure(c.Request.Context(), outcome, terminalFailurePayload)
			_ = streamAttempt.Close()
			clearNewAPIUpstreamCyberPolicyDecision(c)
			h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/chat/completions", Model: logModel, EffectiveModel: attemptLogEffectiveModel,
				StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/chat/completions", UpstreamEndpoint: upstreamEndpoint, Stream: isStream, ViaWebsocket: useWebsocket,
				AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
				ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			}, promptPolicyIncidentID)
			log.Printf("upstream stream dropped before the first packet, resetting the connection and retrying (attempt %s, account %d, /v1/chat/completions): %s", retryAttemptProgress(attempt, maxRetries), account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.unbindOrRetainAffinityForCapacityShedWithGuard(retryExclusions, affinityKey, account, proxyURL, affinityGuard, outcome, capacityShedRetries, continuousRetryPolicy)
			if !isFirstTokenTimeoutOutcome(outcome) && !outcome.capacityShed &&
				retryLimitForStreamOutcome(outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) == -1 {
				retryExclusions.MarkStreamFailure(account.ID(), outcome, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			}
			// A finite first-token timeout has already waited one full round for nothing; an unlimited budget still forces a backoff, to avoid a loop with no waiting.
			retryOrdinal, retryLimit := retryStateForStreamOutcome(outcome, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), isFirstTokenTimeoutOutcome(outcome), retryOrdinal, retryLimit, resp) {
				return
			}
			continue
		}
		if outcome.logStatusCode == http.StatusOK {
			if !claimContinuousRetrySuccess(c, continuousRetryProtocolChat) {
				_ = streamAttempt.Close()
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			if commitErr := h.commitStreamAttempt(c, streamAttempt); commitErr != nil {
				if isContinuousRetryLocalFailure(commitErr) {
					outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
				} else {
					abortContinuousRetryCommitFailure(h, account, resp, streamAttempt)
					return
				}
			}
		}
		_ = streamAttempt.Close()

		if !continuousRetryBuffersAttempts(continuousRetryPolicy) || continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
			h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
		}
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("stream ended abnormally (account %d, /v1/chat/completions, status %d): %s, about %d characters forwarded", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if outcome.guardTripped {
			if !downstreamWrote && !c.Writer.Written() {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{
						"message": outcome.failureMessage,
						"type":    outputguard.TriggeredCode,
						"code":    outputguard.TriggeredCode,
					},
				})
			}
		} else if isStream && outcome.terminalLocal {
			writeContinuousRetryLocalChatError(c)
		} else if isStream && abortedForHTTPError && !downstreamWrote {
			// Streaming: the upstream failed before the first token, nothing was written downstream, and the HTTP 200 header is not committed yet,
			// so overwrite the preset SSE Content-Type and return JSON with the real error code,
			// so downstream relays and billing do not treat it as a success and bill the estimated input tokens (this matches the reset inside the callback).
			if !writeCommittedChatRetryError(c, outcome.failureMessage) {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			}
		} else if isStream && !downstreamWrote && logStatusCode == logStatusUpstreamStreamBreak &&
			c.Request.Context().Err() == nil && writeErr == nil {
			// Stream break or first-token timeout before the first packet with retries exhausted: previously no write branch matched, so downstream
			// received a "fake 200" with an empty body, and the failure was completely invisible (issue #473). 598 is an internal
			// log status; externally it returns the real 502 with a stable error code, which downstream can recognize programmatically and retry.
			if !writeCommittedChatRetryError(c, outcome.failureMessage) {
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
				})
			}
		} else if !isStream {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolChat) {
				// The deadline owns the terminal response.
			} else if len(terminalFailurePayload) > 0 {
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			} else if compactResult != nil {
				c.Data(http.StatusOK, "application/json", compactResult)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "incomplete upstream response", "type": "upstream_error"},
				})
			}
		}

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:              account.ID(),
			Endpoint:               "/v1/chat/completions",
			Model:                  logModel,
			EffectiveModel:         attemptLogEffectiveModel,
			StatusCode:             logStatusCode,
			DurationMs:             totalDuration,
			FirstTokenMs:           firstTokenMs,
			ReasoningEffort:        reasoningEffort,
			InboundEndpoint:        "/v1/chat/completions",
			UpstreamEndpoint:       upstreamEndpoint,
			Stream:                 isStream,
			ViaWebsocket:           useWebsocket,
			ServiceTier:            usageTiers.ServiceTier,
			RequestedServiceTier:   usageTiers.RequestedServiceTier,
			ActualServiceTier:      usageTiers.ActualServiceTier,
			BillingServiceTier:     usageTiers.BillingServiceTier,
			PromptPolicyIncidentID: promptPolicyIncidentID,
			AttemptIndex:           attempt + 1,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogFailureMessage(logStatusCode, outcome.failureMessage)
			logInput.UpstreamErrorKind = outcome.failureKind
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
			logInput.ImageInputTokens, logInput.ImageOutputTokens, logInput.CachedImageInputTokens = usage.ImageInputTokens, usage.ImageOutputTokens, usage.CachedImageInputTokens
		}
		applyUpstreamResponseModelObservation(logInput, responseModelObserver, upstreamSentModelForAudit(attemptEffectiveModel, logModel), account.ID())
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, attemptEffectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		if outcome.logStatusCode == http.StatusOK {
			h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
		} else {
			h.store.Release(account)
		}
		return
	}
}

// ==================== General helpers ====================

// parseRetryAfter parses the retry time from an upstream 429 response (see CLIProxyAPI codex_executor.go:689-708)
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 2 * time.Minute
	}

	// Parse error.resets_at (Unix timestamp)
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			d := time.Until(resetTime)
			if d > 0 {
				return d
			}
		}
	}

	// Parse error.resets_in_seconds
	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return time.Duration(secs) * time.Second
	}

	// Default: 2 minutes
	return 2 * time.Minute
}

func isMissingScopeUnauthorized(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code != "missing_scope" {
		return false
	}

	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "api.responses.write") {
		return true
	}

	return strings.Contains(msg, "scope")
}

func parseRetryAfterResetAt(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}

	if resetsAt := firstGJSONInt(body, "error.resets_at", "response.error.resets_at", "response.status_details.error.resets_at"); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(now) {
			return resetTime, true
		}
	}

	if secs := firstGJSONInt(body, "error.resets_in_seconds", "response.error.resets_in_seconds", "response.status_details.error.resets_in_seconds"); secs > 0 {
		return now.Add(time.Duration(secs) * time.Second), true
	}

	return time.Time{}, false
}

func parseUsageLimitResetAt(body []byte, now time.Time) (time.Time, bool) {
	if !IsUsageLimitReachedError(body) {
		return time.Time{}, false
	}
	return parseRetryAfterResetAt(body, now)
}

// IsCodexModelUnsupportedError is the exported wrapper of isCodexModelUnsupportedError,
// so that admin model probing reuses the same "account does not support this model" detection logic.
func IsCodexModelUnsupportedError(body []byte) bool {
	return isCodexModelUnsupportedError(body)
}

// isCodexModelUnsupportedError reports whether a 400 response means "the current account does not support this model".
// This error is decided by the account's plan entitlements rather than the request content, so switching to an account that supports the model succeeds,
// so it cools down by the (account, model) pair and retries on another account, instead of passing it through to the client unchanged (issue #408).
func isCodexModelUnsupportedError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "model is not supported when using codex") {
			return true
		}
		if strings.Contains(lower, "unknown provider for model") {
			return true
		}
	}
	return false
}

// codexUnsupportedModelRe matches the upstream "The 'gpt-5.4-mini' model is not supported when
// using Codex with a ChatGPT account." message, which captures the rejected model name.
var codexUnsupportedModelRe = regexp.MustCompile(`(?i)the '([^']+)' model is not supported`)

// codexUnsupportedModelFromBody extracts the rejected model name from a "model not supported" 400 response; if it is not this
// kind of error or no name can be extracted, it returns an empty string. The caller uses this to tell whether the rejected model is the request model or the image-generation driver model.
func codexUnsupportedModelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "detail").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if match := codexUnsupportedModelRe.FindStringSubmatch(candidate); len(match) == 2 {
			return strings.TrimSpace(match[1])
		}
	}
	return ""
}

func isCodexModelCapacityError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "selected model is at capacity") ||
			strings.Contains(lower, "model is at capacity. please try a different model") {
			return true
		}
	}
	return false
}

func codexWindowType(windowMinutes float64) codexRateLimitWindow {
	switch {
	case windowMinutes >= 1440:
		return codexRateLimitWindow7d
	case windowMinutes >= 60:
		return codexRateLimitWindow5h
	case windowMinutes > 0:
		return codexRateLimitWindowShort
	default:
		return codexRateLimitWindowUnknown
	}
}

type codexWindowUsage struct {
	usedPct   float64
	resetSec  float64
	windowMin float64
	valid     bool
	// hasReset distinguishes an explicit reset (possibly "0", meaning the
	// window resets now) from a payload that omitted reset information. A
	// partial frame must not overwrite a known reset with "now" and disable
	// the auto-pause fence.
	hasReset bool
}

func parseCodexWindowUsage(usedStr, windowStr, resetStr string) codexWindowUsage {
	if strings.TrimSpace(usedStr) == "" || strings.TrimSpace(windowStr) == "" {
		return codexWindowUsage{}
	}
	var usedPct, windowMin, resetSec float64
	// Parse the whole value: fmt.Sscanf("%f") accepts trailing garbage, so a
	// malformed header such as "42abc" would be treated as 42.
	parsedUsed, err := strconv.ParseFloat(strings.TrimSpace(usedStr), 64)
	if err != nil || math.IsNaN(parsedUsed) {
		return codexWindowUsage{}
	}
	usedPct = parsedUsed
	// Reject out-of-range percentages instead of pacing an account on a
	// malformed observation; keep the previous snapshot until the next probe.
	if usedPct < 0 || usedPct > 100 {
		return codexWindowUsage{}
	}
	parsedWindow, err := strconv.ParseFloat(strings.TrimSpace(windowStr), 64)
	if err != nil || math.IsNaN(parsedWindow) || math.IsInf(parsedWindow, 0) || parsedWindow <= 0 {
		return codexWindowUsage{}
	}
	windowMin = parsedWindow
	hasReset := false
	if rawReset := strings.TrimSpace(resetStr); rawReset != "" {
		if parsedReset, err := strconv.ParseFloat(rawReset, 64); err == nil && !math.IsNaN(parsedReset) && !math.IsInf(parsedReset, 0) {
			resetSec = parsedReset
			hasReset = true
		}
	}
	return codexWindowUsage{
		usedPct:   usedPct,
		windowMin: windowMin,
		resetSec:  resetSec,
		valid:     true,
		hasReset:  hasReset,
	}
}

// codexResetAfterSeconds returns the reset offset for one rate-limit window.
// Upstreams have used both families: x-codex-<slot>-reset-after-seconds (relative)
// and x-codex-<slot>-reset-at (absolute Unix seconds, the family the official
// Codex client parses). Normalize the absolute form to a relative offset so the
// existing snapshot code keeps one representation.
func codexResetAfterSeconds(header http.Header, prefix string) string {
	if raw := strings.TrimSpace(header.Get(prefix + "reset-after-seconds")); raw != "" {
		return raw
	}
	raw := strings.TrimSpace(header.Get(prefix + "reset-at"))
	if raw == "" {
		return ""
	}
	epoch, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || epoch <= 0 {
		return ""
	}
	seconds := time.Until(time.Unix(epoch, 0)).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

func classifyCodex429Window(resp *http.Response, now time.Time) (codexRateLimitWindow, time.Time, bool) {
	if resp == nil {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		codexResetAfterSeconds(resp.Header, "x-codex-primary-"),
	)
	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		codexResetAfterSeconds(resp.Header, "x-codex-secondary-"),
	)

	var exhausted []codexWindowUsage
	if primary.valid && primary.usedPct >= 100 {
		exhausted = append(exhausted, primary)
	}
	if secondary.valid && secondary.usedPct >= 100 {
		exhausted = append(exhausted, secondary)
	}
	if len(exhausted) == 0 {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	chosen := exhausted[0]
	for _, candidate := range exhausted[1:] {
		if candidate.windowMin > chosen.windowMin {
			chosen = candidate
		}
	}

	var resetAt time.Time
	if chosen.resetSec > 0 {
		resetAt = now.Add(time.Duration(chosen.resetSec) * time.Second)
	}
	return codexWindowType(chosen.windowMin), resetAt, !resetAt.IsZero()
}

func responseHasCodex5hHeaders(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		codexResetAfterSeconds(resp.Header, "x-codex-primary-"),
	)
	if primary.valid && codexWindowType(primary.windowMin) == codexRateLimitWindow5h {
		return true
	}

	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		codexResetAfterSeconds(resp.Header, "x-codex-secondary-"),
	)
	return secondary.valid && codexWindowType(secondary.windowMin) == codexRateLimitWindow5h
}

// classifySpark429RateLimit keeps Spark quota exhaustion on the Spark model.
// Explicit quota evidence (body reset or an exhausted 5h/7d window) drives
// the independent Spark usage window. Transient throttles freeze the whole
// account so a model alias cannot bypass the shared budget.
func classifySpark429RateLimit(account *auth.Account, body []byte, resp *http.Response, now time.Time, model string) codex429Decision {
	decision := codex429Decision{
		Scope:  rateLimitScopeModel,
		Reason: "rate_limited_model",
		Model:  strings.TrimSpace(model),
	}
	if isCodexModelCapacityError(body) {
		decision.Reason = "model_capacity"
	}

	if IsUsageLimitReachedError(body) {
		decision.Reason = "spark_usage_limit"
		if resetAt, ok := parseUsageLimitResetAt(body, now); ok {
			decision.ResetAt = resetAt
			decision.Cooldown = resetAt.Sub(now)
			return decision
		}
	}

	windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
	switch windowType {
	case codexRateLimitWindow5h:
		if !hasWindowReset {
			resetAt = now.Add(5 * time.Hour)
		}
		decision.Reason = "spark_usage_limit"
		decision.ResetAt = resetAt
		decision.Cooldown = resetAt.Sub(now)
		return decision
	case codexRateLimitWindow7d:
		if !hasWindowReset {
			resetAt = now.Add(7 * 24 * time.Hour)
		}
		decision.Reason = "spark_usage_limit"
		decision.ResetAt = resetAt
		decision.Cooldown = resetAt.Sub(now)
		return decision
	}

	if decision.Reason == "spark_usage_limit" {
		decision.Cooldown = usageLimitFallbackCooldown(account, body)
		decision.ResetAt = now.Add(decision.Cooldown)
		return decision
	}
	if decision.Reason == "model_capacity" {
		decision.Cooldown = 5 * time.Minute
		return decision
	}
	// A Spark transient throttle shares the account budget with the main model; freezing only the Spark model can be bypassed by an alias.
	return transientAccountRateLimitDecision(body, resp, now)
}

func classify429RateLimit(account *auth.Account, body []byte, resp *http.Response, now time.Time, model string) codex429Decision {
	model = strings.TrimSpace(model)
	if isProOnlyModel(model) {
		return classifySpark429RateLimit(account, body, resp, now, model)
	}

	if IsUsageLimitReachedError(body) {
		if resetAt, ok := parseUsageLimitResetAt(body, now); ok {
			reason := "usage_limit"
			if account != nil && account.IsPremium5hPlan() && responseHasCodex5hHeaders(resp) {
				reason = "rate_limited_5h"
			}
			return codex429Decision{
				Scope:    rateLimitScopeAccount,
				Reason:   reason,
				ResetAt:  resetAt,
				Cooldown: resetAt.Sub(now),
			}
		}

		windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
		switch windowType {
		case codexRateLimitWindow5h:
			if !hasWindowReset {
				resetAt = now.Add(5 * time.Hour)
			}
			return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_5h", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
		case codexRateLimitWindow7d:
			if !hasWindowReset {
				resetAt = now.Add(7 * 24 * time.Hour)
			}
			return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_7d", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
		}

		cooldown := usageLimitFallbackCooldown(account, body)
		resetAt = now.Add(cooldown)
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "usage_limit", ResetAt: resetAt, Cooldown: cooldown}
	}

	windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
	switch windowType {
	case codexRateLimitWindow5h:
		if !hasWindowReset {
			resetAt = now.Add(5 * time.Hour)
		}
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_5h", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
	case codexRateLimitWindow7d:
		if !hasWindowReset {
			resetAt = now.Add(7 * 24 * time.Hour)
		}
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_7d", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
	}

	if isCodexModelCapacityError(body) && model != "" {
		return codex429Decision{
			Scope:    rateLimitScopeModel,
			Reason:   "model_capacity",
			Model:    model,
			Cooldown: 5 * time.Minute,
		}
	}

	// A bare 429 / rate_limit* is an account-level transient rate limit. If only the current model is frozen, the same account can switch to an alias
	// and hit the upstream again at once. Quota exhaustion and 5h/7d=100% are already split off above.
	return transientAccountRateLimitDecision(body, resp, now)
}

func transientAccountRateLimitDecision(body []byte, resp *http.Response, now time.Time) codex429Decision {
	cooldown := auth.TransientRateLimitBackoffBase
	if retryAfter := transient429RetryAfter(body, resp, now); retryAfter > cooldown {
		cooldown = retryAfter
	}
	if cooldown > auth.TransientRateLimitBackoffMax {
		cooldown = auth.TransientRateLimitBackoffMax
	}
	return codex429Decision{
		Scope:    rateLimitScopeAccount,
		Reason:   "rate_limited",
		ResetAt:  now.Add(cooldown),
		Cooldown: cooldown,
	}
}

func transient429RetryAfter(body []byte, resp *http.Response, now time.Time) time.Duration {
	if resp != nil {
		if retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After")); retryAfter > 0 {
			return retryAfter
		}
	}
	if resetAt, ok := parseRetryAfterResetAt(body, now); ok {
		if remaining := resetAt.Sub(now); remaining > 0 {
			return remaining
		}
	}
	return 0
}

func usageLimitFallbackCooldown(account *auth.Account, body []byte) time.Duration {
	planType := ""
	if details, ok := parseUsageLimitDetails(body); ok {
		planType = details.planType
	}
	if planType == "" && account != nil {
		planType = account.GetPlanType()
	}
	switch auth.NormalizePlanType(planType) {
	case "free":
		return 7 * 24 * time.Hour
	default:
		return 5 * time.Hour
	}
}

// Apply429Cooldown handles the effect of a 429 on account state in one place.
func Apply429Cooldown(store *auth.Store, account *auth.Account, body []byte, resp *http.Response, model string) codex429Decision {
	decision := classify429RateLimit(account, body, resp, time.Now(), model)
	if store == nil || account == nil {
		return decision
	}
	// Spark has an independent quota window. Its usage_limit metadata must not
	// rewrite the account plan or the main 5h/7d snapshots before the model-level
	// decision below is applied.
	if details, ok := parseUsageLimitDetails(body); ok && !(decision.Scope == rateLimitScopeModel && isProOnlyModel(model)) {
		store.ApplyUsageLimitMetadata(account, details.planType, decision.ResetAt)
	}
	if decision.Scope == rateLimitScopeModel {
		if isProOnlyModel(model) && decision.Reason == "spark_usage_limit" {
			store.MarkSparkUsageExhausted(account, decision.ResetAt)
			return decision
		}
		policy := store.ResolveModelCooldownPolicy(account)
		if policy.Mode == database.ModelCooldownModeOff || policy.Seconds <= 0 {
			decision.ResetAt = time.Time{}
			decision.Cooldown = 0
			return decision
		}
		backoff := policy.Mode == database.ModelCooldownModeAdaptive && policy.BackoffEnabled
		cooldown := store.MarkModelCooldownWithBackoff(
			account,
			decision.Model,
			time.Duration(policy.Seconds)*time.Second,
			decision.Reason,
			backoff,
		)
		decision.ResetAt = cooldown.ResetAt
		decision.Cooldown = time.Until(cooldown.ResetAt)
		return decision
	}
	if account.IsPremium5hPlan() && decision.Scope == rateLimitScopeAccount && decision.Reason == "rate_limited_5h" {
		store.MarkResponsesPremium5hRateLimited(account, decision.ResetAt)
		return decision
	}
	if decision.Scope == rateLimitScopeAccount && decision.Reason == "rate_limited" {
		// Pass only an actual upstream hint. Reusing the synthetic 15s floor
		// here would slide the same cooldown forward on every in-flight 429.
		applied := store.MarkTransientRateLimited(account, transient429RetryAfter(body, resp, time.Now()))
		decision.Cooldown = applied
		if applied > 0 {
			decision.ResetAt = time.Now().Add(applied)
		}
		return decision
	}
	store.MarkResponsesRateLimited(account, decision.Cooldown)
	return decision
}

// applyCooldown sets smart cooldown based on the upstream status code
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	h.applyCooldownForModel(account, statusCode, body, resp, "")
}

func (h *Handler) applyCooldownForModel(account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if IsUsageLimitReachedError(body) {
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		log.Printf("account %d hit its usage limit (status=%d, plan=%s, reason=%s), cooling down until %s", account.ID(), statusCode, account.GetPlanType(), decision.Reason, decision.ResetAt.Format(time.RFC3339))
		return decision
	}
	switch statusCode {
	case http.StatusBadRequest:
		// Upstream "model not supported": the plan gate cools down by (account, model), and the scheduler then skips that pair;
		// the version gate is the same for the whole pool, so a cooldown would lock the pool down one account at a time, and therefore no cooldown is written.
		// Other 400s are request content problems and do not change account state.
		if model != "" && isCodexModelUnsupportedError(body) {
			decision := codex429Decision{
				Scope:  rateLimitScopeModel,
				Reason: auth.ModelCooldownReasonUnsupported,
				Model:  model,
			}
			outboundVersion := codexOutboundClientVersion(resp)
			version := displayCodexOutboundVersion(outboundVersion)
			if classifyCodexUnsupportedModel(account.GetPlanType(), model, outboundVersion, time.Now()) == codexUnsupportedVersionGate {
				log.Printf("account %d (plan=%s) request model %s was rejected upstream, judged as the client version being too old (outbound Version=%s), so no model cooldown is written; sync the Codex client version in the admin panel or check the pinned User-Agent", account.ID(), account.GetPlanType(), model, version)
				return decision
			}
			duration := h.store.UnsupportedModelCooldown()
			if duration <= 0 {
				log.Printf("account %d (plan=%s) does not support model %s (outbound Version=%s), so the model cooldown is not written per the settings", account.ID(), account.GetPlanType(), model, version)
				return decision
			}
			cooldown := h.store.MarkModelUnsupported(account, model, duration)
			log.Printf("account %d (plan=%s) does not support model %s (outbound Version=%s, plan or client version restricted), the model is cooling down until %s", account.ID(), account.GetPlanType(), model, version, cooldown.ResetAt.Format(time.RFC3339))
			decision.ResetAt = cooldown.ResetAt
			decision.Cooldown = time.Until(cooldown.ResetAt)
			return decision
		}
		return codex429Decision{}
	case http.StatusTooManyRequests:
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		if decision.Scope == rateLimitScopeModel {
			if decision.ResetAt.IsZero() {
				log.Printf("account %d model %s hit a short-term rate limit (reason=%s), so the cooldown is not persisted per policy", account.ID(), decision.Model, decision.Reason)
			} else {
				log.Printf("account %d model %s hit a short-term rate limit (reason=%s), cooling down until %s", account.ID(), decision.Model, decision.Reason, decision.ResetAt.Format(time.RFC3339))
			}
			return decision
		}
		log.Printf("account %d is rate limited (plan=%s, reason=%s), cooling down until %s", account.ID(), account.GetPlanType(), decision.Reason, decision.ResetAt.Format(time.RFC3339))
		return decision
	case http.StatusUnauthorized:
		// The atomic flag is set instantly, preventing other concurrent requests from selecting this account again
		atomic.StoreInt32(&account.Disabled, 1)

		if isMissingScopeUnauthorized(body) {
			log.Printf("account %d received a missing_scope 401, keeping it in the pool", account.ID())
			atomic.StoreInt32(&account.Disabled, 0)
			return codex429Decision{}
		}

		if h.store.GetAutoCleanUnauthorized() {
			// When auto-cleanup is enabled, a 401 removes the account from the pool immediately
			log.Printf("account %d received a 401, cleaning it up immediately", account.ID())
			if h.db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.SetError(ctx, account.ID(), "deleted")
				cancel()
				h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_401")
			}
			h.store.RemoveAccount(account.ID())
		} else {
			h.store.MarkCooldownWithError(account, 5*time.Minute, "unauthorized", upstreamAccountErrorMessage(statusCode, body))
		}
	case http.StatusPaymentRequired, http.StatusForbidden:
		if statusCode == http.StatusForbidden && IsAgentRuntimeDeletedError(body) {
			atomic.StoreInt32(&account.Disabled, 1)
			errorMsg := upstreamAccountErrorMessage(statusCode, body)
			log.Printf("account %d's agent runtime was deleted, marking it as banned", account.ID())
			h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
			return codex429Decision{}
		}
		if IsDeactivatedWorkspaceError(body) {
			log.Printf("account %d's workspace is deactivated, marking it as error", account.ID())
			if h.store != nil {
				h.store.MarkDeactivatedWorkspace(account, upstreamAccountErrorMessage(statusCode, body))
			}
			return codex429Decision{}
		}
		// A bare 402/403 is not proof that this credential is out of
		// balance. The upstream also uses these statuses for transient
		// policy, routing, and endpoint responses. Do not turn one such
		// request into a durable account-level payment_required gate; the
		// usage probe and AT refresh are the authoritative account checks.
		// Explicit usage exhaustion is handled above by IsUsageLimitReachedError.
	}
	return codex429Decision{}
}

// compute429Cooldown computes the exact 429 cooldown time from the plan type and the Codex response
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	return compute429Cooldown(account, body, resp)
}

func compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	// 1. Prefer the exact reset time in the Codex response body
	if resetDuration := parseRetryAfter(body); resetDuration > 2*time.Minute {
		// parseRetryAfter returns 2min by default (no data); anything over 2min means a real resets_at/resets_in_seconds was parsed
		if resetDuration > 7*24*time.Hour {
			resetDuration = 7 * 24 * time.Hour // at most 7 days
		}
		return resetDuration
	}

	// 2. No exact reset time: infer it from the plan type and the usage window
	planType := auth.NormalizePlanType(account.GetPlanType())

	switch planType {
	case "free":
		// Free has only a 7d window: a 429 means quota is exhausted, so cooldown is 7 days
		return 7 * 24 * time.Hour

	case "team", "teamplus", "pro", "plus", "enterprise", "k12", "edu", "education":
		// Team/Pro/Plus and education plans (k12/edu) have both 5h and 7d windows, so determine which window triggered the limit
		return detectTeamCooldownWindow(resp)

	default:
		// Unknown plan: conservatively default to 5 hours
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow determines which window triggered the limit for Team/Pro/Plus accounts from the response headers
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
	return detectTeamCooldownWindow(resp)
}

func detectTeamCooldownWindow(resp *http.Response) time.Duration {
	if resp == nil {
		return 5 * time.Hour // conservative default
	}

	// Codex returns two sets of window headers: primary and secondary
	// x-codex-primary-window-minutes / x-codex-primary-used-percent
	// x-codex-secondary-window-minutes / x-codex-secondary-used-percent
	// A window with usage >= 100% is the window that triggered the limit

	primaryUsed := parseFloat(resp.Header.Get("x-codex-primary-used-percent"))
	primaryWindowMin := parseFloat(resp.Header.Get("x-codex-primary-window-minutes"))
	secondaryUsed := parseFloat(resp.Header.Get("x-codex-secondary-used-percent"))
	secondaryWindowMin := parseFloat(resp.Header.Get("x-codex-secondary-window-minutes"))

	// Find the window with used >= 100%
	primaryExhausted := primaryUsed >= 100
	secondaryExhausted := secondaryUsed >= 100

	switch {
	case primaryExhausted && secondaryExhausted:
		// Both windows are full: use the cooldown of the larger window
		return windowMinutesToCooldown(max(primaryWindowMin, secondaryWindowMin))
	case primaryExhausted:
		return windowMinutesToCooldown(primaryWindowMin)
	case secondaryExhausted:
		return windowMinutesToCooldown(secondaryWindowMin)
	default:
		// Neither window is full but still a 429: probably a short-term burst limit
		return 5 * time.Hour
	}
}

// windowMinutesToCooldown decides the cooldown duration from the window length in minutes
func windowMinutesToCooldown(windowMinutes float64) time.Duration {
	switch {
	case windowMinutes >= 1440: // >= 1 day → 7d window
		return 7 * 24 * time.Hour
	case windowMinutes >= 60: // >= 1 hour → 5h window
		return 5 * time.Hour
	default:
		return 30 * time.Minute // short window
	}
}

// SyncCodexUsageState parses the Codex response headers and completes the 7d / 5h snapshot persistence and the premium 5h early rate limit.
func SyncCodexUsageState(store *auth.Store, account *auth.Account, resp *http.Response) CodexUsageSyncResult {
	return syncCodexUsageState(store, account, resp, time.Now(), codexUsageSyncOptions{
		persistUsageSnapshot: true,
		clearAbsent5h:        true,
	})
}

type codexUsageSyncOptions struct {
	// persistUsageSnapshot=false keeps the in-memory/scheduler update but skips
	// the DB write. WS frames use this for throttled persistence.
	persistUsageSnapshot bool
	// clearAbsent5h controls whether an authoritative observation that omits
	// the 5h window clears a previous 5h snapshot. Per-turn WS frames may be
	// partial, so they must not erase a valid window.
	clearAbsent5h bool
	// shouldPersistUsage, when set, is evaluated inside ApplyUsageObservation's
	// ordering gate. That gate rejects stale observers, so the throttling state
	// must not advance for an observation that will not be applied.
	shouldPersistUsage func(pct5h float64, has5h bool, pct7d float64, has7d bool) bool
	// asyncPersist moves the frame snapshot write to the store's coalescing
	// worker. The upstream read path only applies memory/scheduler state, so a
	// slow or locked database cannot stall a streaming response.
	asyncPersist bool
	// preserveAbsentHardStop is used by partial WS quota frames, which cannot
	// clear a workspace stop unless they explicitly carry that field.
	preserveAbsentHardStop bool
	// observationLocked is only set by the WS ingress ordering gate.
	observationLocked bool
}

func syncCodexUsageState(store *auth.Store, account *auth.Account, resp *http.Response, observedAt time.Time, opts codexUsageSyncOptions) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || resp == nil {
		return result
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	observation := parseCodexUsageHeaderObservation(resp)
	hasCredits, unlimited, creditsObserved := parseCodexCreditsFlagHeaders(resp)
	rateLimitReachedType := strings.TrimSpace(resp.Header.Get("x-codex-rate-limit-reached-type"))
	apply := func() {
		if store != nil {
			planHeader := resp.Header.Get("x-codex-plan-type")
			if opts.asyncPersist {
				store.ObserveUsageFramePlan(account, planHeader)
			} else {
				store.UpdateAccountPlanType(account, planHeader)
				// The authoritative paid plan_type and 'subscription expired' are mutually exclusive, so every response is used to correct a stale expiry time. (issue #360)
				if planHeader != "" {
					store.ClearStaleSubscriptionExpiresAt(account)
				}
			}
		}

		if opts.preserveAbsentHardStop && rateLimitReachedType == "" {
			rateLimitReachedType = account.GetRateLimitReachedType()
		}
		result.RateLimitReachedType = rateLimitReachedType
		if creditsObserved {
			result.CreditsObserved = true
			var balPtr *string
			if trimmed := strings.TrimSpace(resp.Header.Get("x-codex-credits-balance")); trimmed != "" {
				balPtr = &trimmed
			}
			var overagePtr *bool
			if raw := strings.TrimSpace(resp.Header.Get("x-codex-credits-overage-reached")); raw != "" {
				if ov, err := strconv.ParseBool(raw); err == nil {
					overagePtr = &ov
				}
			}
			if store != nil && !opts.asyncPersist {
				store.PersistSparseCreditObservation(account, hasCredits, unlimited, balPtr, overagePtr, rateLimitReachedType)
			} else {
				account.ApplySparseCreditsHeaders(hasCredits, unlimited, balPtr, overagePtr, rateLimitReachedType)
			}
		} else if rateLimitReachedType != "" || observation.authoritative {
			// reached-type is a per-response field: a response with usage headers but without it means 'not reached', so the old
			// workspace hard-stop must be cleared, otherwise credits will never take over again after the workspace is topped up.
			if store != nil && !opts.asyncPersist {
				store.PersistRateLimitReachedType(account, rateLimitReachedType)
			} else {
				account.SetRateLimitReachedType(rateLimitReachedType)
			}
		}

		result.UsageWindowLimitsIgnored = account.SkipsUsageWindowLimits()

		result.Used5hHeaders = observation.w5h.valid
		if observation.authoritative {
			parsed := applyCodexUsageHeaderObservation(store, account, observation, observedAt, opts.clearAbsent5h)
			result.UsagePct7d = parsed.usagePct7d
			result.HasUsage7d = parsed.hasUsage7d
			result.Cleared5h = parsed.cleared5h
			if store == nil {
				return
			}
			persistUsage := opts.persistUsageSnapshot
			if opts.shouldPersistUsage != nil {
				persistUsage = opts.shouldPersistUsage(
					observation.w5h.usedPct,
					observation.w5h.valid,
					observation.w7d.usedPct,
					observation.w7d.valid,
				)
			}
			result.PersistUsage = persistUsage
			persistedUsage := false
			if result.HasUsage7d {
				if opts.asyncPersist {
					// Memory and scheduler update stay synchronous; the DB write
					// is coalesced per account off the read path.
					store.PublishUsageSnapshot(account, result.UsagePct7d, observedAt, false)
				} else {
					persistedUsage = store.PublishUsageSnapshot(account, result.UsagePct7d, observedAt, persistUsage)
				}
				if result.UsagePct7d >= 100 && persistUsage && !opts.asyncPersist {
					result.Usage7dRateLimited = store.MarkUsage7dRateLimited(account)
				}
			} else if result.Used5hHeaders {
				if opts.asyncPersist {
					store.PublishUsageSnapshot5hOnly(account, observedAt, false)
				} else {
					persistedUsage = store.PublishUsageSnapshot5hOnly(account, observedAt, persistUsage)
				}
				result.Persisted5hOnly = persistUsage
			}
			if persistUsage && persistedUsage {
				// Advance the frame throttle only after the write succeeded so a
				// failed write is retried on the next frame instead of sleeping
				// through the interval.
				pct5h, has5h := account.GetUsagePercent5h()
				pct7d, has7d := account.GetUsagePercent7d()
				account.MarkUsageFramePersisted(observedAt, pct5h, has5h, pct7d, has7d)
			}
			if opts.asyncPersist && persistUsage {
				result.Usage7dRateLimited, result.Premium5hRateLimited = store.MarkUsageFrameCooldowns(account, observedAt)
			}
		}
	}
	applied := false
	if observation.authoritative || creditsObserved || rateLimitReachedType != "" || resp.Header.Get("x-codex-plan-type") != "" {
		if opts.observationLocked {
			apply()
			applied = true
		} else {
			applied = account.ApplyUsageObservation(observedAt, apply)
		}
	}
	usageApplied := applied && observation.authoritative

	result.UsagePct5h, result.Reset5hAt, result.HasUsage5h = account.GetUsageSnapshot5h()
	if usageApplied && store != nil && result.HasUsage5h {
		// The passive /responses headers refreshed the 5h window reset time: arm 'probe at reset time', so the progress bar refreshes when the window is renewed.
		store.WakeBoundaryProbe(result.Reset5hAt)
	}
	if !opts.asyncPersist && usageApplied && result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 && !account.SkipsUsageWindowLimits() {
		if store != nil {
			// The per-frame path may decide not to persist this observation; a
			// repeated 100% frame must not issue another cooldown write.
			if result.PersistUsage {
				result.Premium5hRateLimited = store.MarkPremium5hRateLimitedAt(account, result.Reset5hAt, observedAt)
			}
		} else {
			result.Premium5hRateLimited = true
		}
	}

	result.Applied = applied && (usageApplied || result.CreditsObserved)
	if applied && opts.asyncPersist && store != nil {
		store.RefreshUsageFrameScheduler(account)
		store.EnqueueUsageFrameEffects(account, observedAt, result.PersistUsage, resp.Header.Get("x-codex-plan-type") != "")
	}
	return result
}

type codexUsageHeaderParseResult struct {
	usagePct7d float64
	hasUsage7d bool
	cleared5h  bool
}

type codexUsageHeaderObservation struct {
	w5h           codexWindowUsage
	w7d           codexWindowUsage
	authoritative bool
}

// parseCodexUsageHeaders parses the 5h/7d usage percentages from the Codex response headers
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil || account == nil {
		return 0, false
	}
	observedAt := time.Now()
	observation := parseCodexUsageHeaderObservation(resp)
	if !observation.authoritative {
		return 0, false
	}
	parsed := codexUsageHeaderParseResult{}
	account.ApplyUsageObservation(observedAt, func() {
		parsed = applyCodexUsageHeaderObservation(nil, account, observation, observedAt, true)
	})
	return parsed.usagePct7d, parsed.hasUsage7d
}

// parseCodexUsageHeaderObservation classifies only windows with a positive,
// recognizable duration. Used-percent-only partial headers are not authoritative
// evidence that the optional 5h window disappeared.
// parseCodexCreditsFlagHeaders parses the two booleans in the sparse credits headers. Aligned with the official client:
// has-credits and unlimited are only adopted when both are present and parseable; otherwise the whole group is discarded, because treating a missing header as false
// would overwrite the correct snapshot probed by wham with "no credits" and persist it.
func parseCodexCreditsFlagHeaders(resp *http.Response) (hasCredits, unlimited, ok bool) {
	rawHas := strings.TrimSpace(resp.Header.Get("x-codex-credits-has-credits"))
	rawUnlimited := strings.TrimSpace(resp.Header.Get("x-codex-credits-unlimited"))
	if rawHas == "" || rawUnlimited == "" {
		return false, false, false
	}
	hasCredits, err := strconv.ParseBool(rawHas)
	if err != nil {
		return false, false, false
	}
	unlimited, err = strconv.ParseBool(rawUnlimited)
	if err != nil {
		return false, false, false
	}
	return hasCredits, unlimited, true
}

func parseCodexUsageHeaderObservation(resp *http.Response) codexUsageHeaderObservation {
	out := codexUsageHeaderObservation{}
	if resp == nil {
		return out
	}

	// Parse the primary and secondary windows
	primaryUsedStr := resp.Header.Get("x-codex-primary-used-percent")
	primaryWindowStr := resp.Header.Get("x-codex-primary-window-minutes")
	primaryResetStr := codexResetAfterSeconds(resp.Header, "x-codex-primary-")
	secondaryUsedStr := resp.Header.Get("x-codex-secondary-used-percent")
	secondaryWindowStr := resp.Header.Get("x-codex-secondary-window-minutes")
	secondaryResetStr := codexResetAfterSeconds(resp.Header, "x-codex-secondary-")

	primary := parseCodexWindowUsage(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseCodexWindowUsage(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	for _, window := range []codexWindowUsage{primary, secondary} {
		if !window.valid {
			continue
		}
		switch codexWindowType(window.windowMin) {
		case codexRateLimitWindow5h:
			if !out.w5h.valid {
				out.w5h = window
			}
		case codexRateLimitWindow7d:
			if !out.w7d.valid {
				out.w7d = window
			}
		}
	}
	out.authoritative = out.w5h.valid || out.w7d.valid
	return out
}

func applyCodexUsageHeaderObservation(store *auth.Store, account *auth.Account, observation codexUsageHeaderObservation, observedAt time.Time, clearAbsent5h bool) codexUsageHeaderParseResult {
	out := codexUsageHeaderParseResult{}
	if observation.w5h.valid {
		existingReset := account.GetReset5hAt()
		existingPct, existingValid := account.GetUsagePercent5h()
		resetAt := existingReset
		// Only a reset that is still in the future identifies the current
		// window. If it has already passed, a frame without reset_at belongs
		// to the next window and must not inherit the stale timestamp.
		if observation.w5h.hasReset {
			resetAt = observedAt.Add(time.Duration(observation.w5h.resetSec) * time.Second)
		} else if resetAt.IsZero() || !resetAt.After(observedAt) {
			// A partial frame without reset must not create a zero reset that
			// never expires; bound the pause to one window length instead.
			resetAt = observedAt.Add(time.Duration(observation.w5h.windowMin) * time.Minute)
		}
		pct := observation.w5h.usedPct
		if existingValid && existingReset.After(observedAt) && sameUsageResetWindow(existingReset, resetAt) && pct < existingPct {
			// Within one window usage only increases. A late frame from an
			// earlier concurrent turn must not roll the snapshot back and
			// temporarily un-pause the account.
			pct = existingPct
		}
		account.SetUsageSnapshot5hAt(pct, resetAt, observedAt)
	} else if observation.authoritative && clearAbsent5h {
		out.cleared5h = store.ClearAbsentUsageSnapshot5hAt(account, observedAt)
	}

	if observation.w7d.valid {
		existingReset := account.GetReset7dAt()
		existingPct, existingValid := account.GetUsagePercent7d()
		resetAt := existingReset
		if observation.w7d.hasReset {
			resetAt = observedAt.Add(time.Duration(observation.w7d.resetSec) * time.Second)
			account.SetReset7dAt(resetAt)
		} else if resetAt.IsZero() || !resetAt.After(observedAt) {
			resetAt = observedAt.Add(time.Duration(observation.w7d.windowMin) * time.Minute)
			account.SetReset7dAt(resetAt)
		}
		pct := observation.w7d.usedPct
		if existingValid && existingReset.After(observedAt) && sameUsageResetWindow(existingReset, resetAt) && pct < existingPct {
			pct = existingPct
		}
		account.SetWindow7dSeconds(int64(observation.w7d.windowMin * 60))
		account.SetUsagePercent7d(pct)
		out.usagePct7d = pct
		out.hasUsage7d = true
	}
	return out
}

// sameUsageResetWindow reports whether two observations describe the same
// rolling window. Relative reset headers are recomputed against the receipt
// time, so allow a small tolerance while still distinguishing the next
// (hours later) window.
func sameUsageResetWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff <= time.Minute
}

// ParseCodexUsageHeaders extracts from the response headers and updates the account usage information
func ParseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	return parseCodexUsageHeaders(resp, account)
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

// sendUpstreamError sends the upstream error response to the client
func (h *Handler) sendUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if isExplicitUpstreamCyberPolicy(body) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": upstreamCyberPolicyResponseMessage(c),
				"type":    "upstream_error",
				"code":    newAPIUpstreamCyberPolicyReasonCode,
			},
		})
		return
	}
	message := usageLogErrorMessage(statusCode, body)
	if message == "" || message == fmt.Sprintf("HTTP %d", statusCode) {
		message = fmt.Sprintf("Upstream returned status %d", statusCode)
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", statusCode),
		},
	})
}

func normalizedRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	digits := true
	for _, r := range value {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	if digits {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

// sendFinalUpstreamError is the final error response after retries are exhausted: it recognizes usage_limit_reached and rewrites it to 503, and passes everything else through
func (h *Handler) sendFinalUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if !claimContinuousRetryTerminal(c, continuousRetryProtocolOpenAI) {
		return
	}
	if details, ok := parseUsageLimitDetails(body); ok {
		if details.resetsInSeconds > 0 {
			c.Header("Retry-After", fmt.Sprintf("%d", details.resetsInSeconds))
		}

		message := "account pool quota is exhausted, please retry later"
		if details.message != "" {
			message = fmt.Sprintf("%s: %s", message, details.message)
		}

		errInfo := gin.H{
			"message": message,
			"type":    "server_error",
			"code":    "account_pool_usage_limit_reached",
		}
		if details.planType != "" {
			errInfo["plan_type"] = details.planType
		}
		if details.resetsAt != 0 {
			errInfo["resets_at"] = details.resetsAt
		}
		if details.resetsInSeconds != 0 {
			errInfo["resets_in_seconds"] = details.resetsInSeconds
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errInfo})
		return
	}

	// An upstream account 401 (OAuth token invalid/revoked) is an account-side problem, not an invalid key from the downstream client.
	// If it were passed through as a 401, the client would wrongly conclude its own credentials are invalid (issue #323). Rewrite it to a 503 pool-level
	// error, using a separate code/type to clearly distinguish it from client authentication failure (invalid_api_key).
	if statusCode == http.StatusUnauthorized && !isMissingScopeUnauthorized(body) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "no available account in the pool (upstream account authentication is invalid), please retry later",
				"type":    "server_error",
				"code":    "account_pool_unauthorized",
			},
		})
		return
	}

	// 402 workspace deactivated (deactivated_workspace): a retry that already switched accounts and still gets it means the pool currently has no
	// servable account, so like 403 it is rewritten to a 503 pool-level error (with a Retry-After hint to back off).
	// The message ends with the raw upstream error body, so downstream can see the ban reason directly. Bad accounts have already been marked as errors and isolated,
	// so a later retry can land on a healthy account. A bare 402 stays as is: it may carry usage/billing semantics, which are handled separately above.
	if statusCode == http.StatusPaymentRequired && IsDeactivatedWorkspaceError(body) {
		if c.Writer.Header().Get("Retry-After") == "" {
			c.Header("Retry-After", "30")
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": deactivatedPoolErrorMessage(strings.TrimSpace(string(body))),
				"type":    "server_error",
				"code":    "account_pool_deactivated",
			},
		})
		return
	}

	// Upstream account 403 (payment_required / deactivated_workspace / codex_access_restricted)
	// is also an account-side problem: a retry that already switched accounts and still gets 403 means the pool has no usable account. Passing 403 through unchanged would make
	// the client wrongly conclude it lacks permission and stop work (issue #396), so it is rewritten to a 503 pool-level error.
	if statusCode == http.StatusForbidden {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "no available account in the pool (upstream account access denied: quota, plan, or workspace restricted), please retry later",
				"type":    "server_error",
				"code":    "account_pool_forbidden",
			},
		})
		return
	}

	if statusCode == http.StatusTooManyRequests && c.Writer.Header().Get("Retry-After") == "" {
		// An occasional upstream 429 should still be returned with standard rate-limit semantics after the retry pool is exhausted, and must not be disguised as
		// no_available_account/503. When there is no explicit reset information, give the client a minimal backoff hint.
		c.Header("Retry-After", "1")
	}

	h.sendUpstreamError(c, statusCode, body)
}

// handleUpstreamError handles upstream errors in one place (kept for compatibility with old callers)
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// ListModels lists the available models
// listModelsOrManifest dispatches the model list by client shape: requests carrying the client_version query parameter come from
// the Codex client refreshing its model picker (it expects the manifest format; a parse failure silently freezes it in the local cache).
// all other clients get the OpenAI-compatible list.
func (h *Handler) listModelsOrManifest(c *gin.Context) {
	if strings.TrimSpace(c.Query("client_version")) != "" {
		h.CodexModelsManifestHandler(c)
		return
	}
	h.ListModels(c)
}

func (h *Handler) ListModels(c *gin.Context) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	// Authenticated routes always carry the complete APIKeyRow. Keep the
	// historical global list only for direct/internal calls that bypass the
	// middleware (including older embedders); a real key always gets an
	// isolated, read-only account snapshot.
	if row := apiKeyRowFromContext(c); row != nil {
		api.SendList(c, "list", h.scopedModels(ctx, row))
		return
	}
	modelIDs := h.supportedModelIDs(ctx)
	models := make([]api.Model, 0, len(modelIDs))
	for _, id := range modelIDs {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: modelCompatibilityCreatedUnix,
			OwnedBy: "openai",
		})
	}
	api.SendList(c, "list", models)
}

func (h *Handler) supportedModelIDs(ctx context.Context) []string {
	models := SupportedModelIDs(ctx, h.db)
	if h != nil && h.store != nil {
		models = append(models, DaybreakModelIDs(h.store.Accounts(), models)...)
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		seen[strings.ToLower(strings.TrimSpace(model))] = struct{}{}
	}
	if h != nil && h.store != nil {
		// The from keys of the global model mapping must also appear in /v1/models, otherwise downstream clients cannot fetch a usable mapped model name.
		// Only list explicit, non-wildcard mapping keys; wildcard rules (containing *) are not exposed as concrete models.
		for _, rule := range parseModelMappingRules(h.store.GetModelMapping()) {
			if rule.Wildcard {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(rule.From))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, rule.From)
		}
	}
	return models
}

// downstreamAuthorizationHeader is shared by authentication and per-key memory
// namespaces so supported header forms cannot collapse into an anonymous key.
//
// Accepted credential forms: Authorization: Bearer, x-api-key / anthropic-auth-token
// (Anthropic-style clients), x-goog-api-key (the google-genai SDK, ADK, and the default header of an aggregation gateway's
// Gemini channel when calling /v1beta). ?key= query strings are not accepted: keys would end up in URLs and access logs.
func downstreamAuthorizationHeader(req *http.Request) string {
	if req == nil {
		return ""
	}
	if value := req.Header.Get("Authorization"); value != "" {
		return value
	}
	if isResponsesWebSocketUpgradeRequest(req) {
		if key := apiKeyFromWebSocketSubprotocol(req.Header.Get("Sec-WebSocket-Protocol")); key != "" {
			return "Bearer " + key
		}
	}
	for _, name := range []string{"x-api-key", "anthropic-auth-token", "x-goog-api-key"} {
		if value := strings.TrimSpace(req.Header.Get(name)); value != "" {
			return "Bearer " + value
		}
	}
	return ""
}
