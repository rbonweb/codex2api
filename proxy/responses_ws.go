package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/codex2api/security/outputguard"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesWSFirstMessageTimeout        = 30 * time.Second
	responsesWSWriteTimeout               = 30 * time.Second
	responsesWSOverloadWriteTimeout       = time.Second
	responsesWSFriendlyUpstreamErr        = "upstream service is temporarily busy, please try again later"
	newAPIPolicyWebSocketEventField       = "__newapi_policy_event_id"
	newAPIPolicyWebSocketCapabilityHeader = "X-Codex2API-Policy-Event-ID"
	newAPIPolicyWebSocketCapabilityV1     = "v1"
	responsesWSInboundQueueCapacity       = 16
)

var responsesWSUpgrader = websocket.Upgrader{
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var errResponsesWSClientGone = errors.New("responses websocket client disconnected")

type responsesWSRetryableStreamError struct {
	outcome   streamOutcome
	eventType string
}

func (e *responsesWSRetryableStreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.outcome.failureMessage
}

// responsesWSContinuationNotFoundError indicates that upstream cannot recognise the previous_response_id in the request.
// The continuation id exists only on the account that created it; besides account switching, responses that are lost or not persisted upstream also end up here.
// The outer loop uses this to downgrade the request to a self-contained request and retry once, rather than passing the 400 straight to the client (issue #400).
type responsesWSContinuationNotFoundError struct{}

func (e *responsesWSContinuationNotFoundError) Error() string {
	return "upstream rejected previous_response_id"
}

// isPreviousResponseNotFoundBody recognises the upstream previous_response_not_found error,
// and handles both payload shapes: the HTTP error body and the in-stream response.failed / error frame.
func isPreviousResponseNotFoundBody(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	// {"error":{...}} / {"response":{"error":{...}}} after normalisation, take error.*;
	// a bare error frame ({"type":"error","code":...,"message":...}) checks the top-level fields.
	body := responseFailedErrorBody(payload)
	for _, path := range []string{"error.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "previous_response_not_found") {
			return true
		}
	}
	for _, path := range []string{"error.message", "message"} {
		message := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if message == "previous_response_id is not available for this user" ||
			strings.Contains(message, "previous response with id") ||
			strings.TrimSuffix(strings.NewReplacer("`", "", "\"", "", "'", "").Replace(message), ".") == "invalid previous_response_id" {
			return true
		}
	}
	return false
}

type responsesWSCloseError struct {
	code   int
	reason string
	err    error
}

type responsesWSForwardOptions struct {
	auditEndpoint        string
	transformClientEvent func([]byte) []byte
	onResponseCompleted  func([]byte)
	lineage              *responsesWSLineage
}

// responsesWSInboundMessage is produced by the connection's only reader. A
// dedicated read pump keeps receiving close/control frames while an upstream
// turn is backing off, so a client disconnect can cancel an unlimited retry
// loop without introducing a second concurrent WebSocket reader.
type responsesWSInboundMessage struct {
	messageType   int
	payload       []byte
	turn          int
	err           error
	queuedBytes   *atomic.Int64
	requestMemory *security.RequestMemoryReservation
}

type responsesWSInboundObserver func(responsesWSInboundMessage)

func (m *responsesWSInboundMessage) releaseQueueBudget() {
	if m == nil {
		return
	}
	if m.queuedBytes != nil {
		m.queuedBytes.Add(-int64(len(m.payload)))
		m.queuedBytes = nil
	}
	if m.requestMemory != nil {
		m.requestMemory.Release()
		m.requestMemory = nil
	}
}

// The read pump must have stopped before draining the connection's leftovers.
func drainResponsesWSInboundMessages(messages <-chan responsesWSInboundMessage) {
	for message := range messages {
		message.releaseQueueBudget()
	}
}

func startResponsesWSReadPump(ctx context.Context, conn *websocket.Conn, observers ...responsesWSInboundObserver) (context.Context, <-chan responsesWSInboundMessage, <-chan struct{}, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithCancel(ctx)
	messages := make(chan responsesWSInboundMessage, responsesWSInboundQueueCapacity)
	done := make(chan struct{})
	var queuedBytes atomic.Int64
	queueByteLimit := int64(security.MaxRequestBodySize)
	if queueByteLimit < 1 {
		queueByteLimit = int64(security.DefaultMaxRequestBodySize)
	}
	go func() {
		defer close(done)
		defer close(messages)
		for turn := 0; ; turn++ {
			if turn == 0 {
				_ = conn.SetReadDeadline(time.Now().Add(responsesWSFirstMessageTimeout))
			} else {
				_ = conn.SetReadDeadline(time.Time{})
			}
			messageType, reader, err := conn.NextReader()
			var payload []byte
			var reservation *security.RequestMemoryReservation
			if err == nil {
				payload, reservation, err = security.ReadRequestMemory(reader, queueByteLimit)
			}
			if err != nil {
				if errors.Is(err, security.ErrRequestMemoryBudget) {
					closeResponsesWSWithin(conn, websocket.CloseTryAgainLater, "request memory capacity exhausted", responsesWSOverloadWriteTimeout)
				}
				// Cancel before notifying the consumer. If the bounded handoff
				// buffer is already full, the active upstream turn still
				// observes cancellation and will stop without waiting for it.
				cancel()
				select {
				case messages <- responsesWSInboundMessage{turn: turn, err: err}:
				default:
				}
				return
			}
			_ = conn.SetReadDeadline(time.Time{})
			payloadBytes := int64(len(payload))
			if queuedBytes.Add(payloadBytes) > queueByteLimit {
				queuedBytes.Add(-payloadBytes)
				reservation.Release()
				cancel()
				return
			}
			message := responsesWSInboundMessage{
				messageType:   messageType,
				payload:       payload,
				turn:          turn,
				queuedBytes:   &queuedBytes,
				requestMemory: reservation,
			}
			// Observers must stay non-blocking. Realtime uses this hook to signal a
			// response.cancel while the serial consumer is waiting on an upstream
			// retry; the read pump remains the connection's only reader.
			for _, observer := range observers {
				if observer != nil {
					observer(message)
				}
			}
			select {
			case messages <- message:
			case <-readCtx.Done():
				message.releaseQueueBudget()
				return
			default:
				message.releaseQueueBudget()
				// A turn is processed serially. The bounded count and byte budgets
				// absorb normal Realtime event bursts without allowing a stalled
				// upstream turn to retain unbounded client input.
				// Never block the only reader here: doing so would hide a later
				// close frame and leave an unlimited upstream retry running.
				cancel()
				return
			}
		}
	}()
	return readCtx, messages, done, cancel
}

func (e *responsesWSCloseError) Error() string {
	if e == nil {
		return ""
	}
	if e.err != nil {
		return e.err.Error()
	}
	return e.reason
}

func (e *responsesWSCloseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress.
// The client sends response.create JSON frames and receives upstream Responses
// events as JSON text frames.
func (h *Handler) ResponsesWebSocket(c *gin.Context) {
	if !isResponsesWebSocketUpgradeRequest(c.Request) {
		api.SendErrorWithStatus(c, api.NewAPIError(
			api.ErrCodeInvalidRequest,
			"WebSocket upgrade required (Upgrade: websocket)",
			api.ErrorTypeInvalidRequest,
		), http.StatusUpgradeRequired)
		return
	}
	releaseWS, ok := h.acquireDownstreamWebSocket(c)
	if !ok {
		return
	}
	defer releaseWS()
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, nil) {
			return
		}
	}

	conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		log.Printf("Responses WebSocket upgrade failed: %v", err)
		return
	}
	if outputGuardHTTPGateArmed() && !RequireOutputGuardReadyWS(conn) {
		return
	}
	conn.SetReadLimit(int64(security.MaxRequestBodySize))
	requestCtx, messages, readPumpDone, cancel := startResponsesWSReadPump(c.Request.Context(), conn)
	c.Request = c.Request.WithContext(requestCtx)
	stopDownstreamKeepalive := startDownstreamWSKeepalive(requestCtx, conn, cancel)
	defer func() {
		cancel()
		_ = conn.Close()
		stopDownstreamKeepalive()
		<-readPumpDone
		drainResponsesWSInboundMessages(messages)
	}()

	unregisterWriter := registerResponsesWSWriter(conn)
	defer unregisterWriter()
	h.runResponsesWSMultiplex(c, conn, messages, requestCtx)
}

func newAPIPolicyWebSocketUpgradeHeaders() http.Header {
	header := make(http.Header)
	header.Set(newAPIPolicyWebSocketCapabilityHeader, newAPIPolicyWebSocketCapabilityV1)
	return header
}

func stripNewAPIPolicyWebSocketEventID(payload []byte) ([]byte, string) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, ""
	}
	eventID := normalizeNewAPIPolicyWebSocketEventID(gjson.GetBytes(payload, newAPIPolicyWebSocketEventField).String())
	cleaned, err := sjson.DeleteBytes(payload, newAPIPolicyWebSocketEventField)
	if err != nil {
		return payload, ""
	}
	return cleaned, eventID
}

// forwardResponsesWebSocketTurn handles one Responses session turn on the WebSocket transport: each
// response.create is an independent logical request. It first does in-memory admission, key binding and quota re-check, and resets the security context from the previous frame,
// then normalises the request body, and re-selects an account inside the retry loop with exclusions before forwarding. Recoverable failures are written back as protocol error
// frames while the connection is kept; only fatal failures that require closing the connection return a close error.
func (h *Handler) forwardResponsesWebSocketTurn(c *gin.Context, conn *websocket.Conn, rawPayload []byte, policyEventID string, options *responsesWSForwardOptions) (returnErr error) {
	defer releasePromptRequestFrameBody(c)
	reservation, admitted := security.TryAcquireRequestMemory(int64(len(rawPayload)))
	if !admitted {
		apiErr := api.NewAPIError(api.ErrCodeServiceUnavailable, "Request memory capacity exhausted, please retry later", api.ErrorTypeServer)
		_ = writeResponsesWSErrorForStream(conn, apiErr, c.GetString(responsesWSStreamContextKey), responsesWSOverloadWriteTimeout)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	defer reservation.Release()
	if apiErr := h.refreshNewAPIWebSocketBinding(c, time.Now()); apiErr != nil {
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	// Each response.create is a separate logical request. Keep the verified
	// connection identity only while its binding remains valid, and never reuse a
	// prior frame's config or body digest.
	resetPromptRequestSecurityFrame(c)
	resetPromptPolicyRequestCorrelationID(c)
	resetUpstreamRequestTrace(c)
	quotaParentRequest := c.Request
	if err := h.refreshAPIKeyModelRequestQuotaTurn(c); err != nil {
		return writeResponsesWSErrorForContext(c, conn, apiKeyModelRequestError(err).apiErr)
	}
	defer func() { c.Request = quotaParentRequest }()
	multiAgentV2State := &codexMultiAgentV2State{}
	if codexMultiAgentV2CompatEnabled() {
		c.Request = c.Request.WithContext(withCodexMultiAgentV2State(c.Request.Context(), multiAgentV2State))
	}
	c.Set(promptGuardPolicyEventIDContextKey, policyEventID)
	rawBody, model, apiErr := normalizeResponsesWebSocketClientPayload(rawPayload)
	if apiErr != nil {
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	// WebSocket turn metadata is frame-local. Cache a complete zero-or-set
	// snapshot before any body rewrite so a later frame can never inherit the
	// prior frame's compaction badges.
	compactionMeta := requestBodyCompactionMeta(rawBody)
	cacheRequestCompactionMeta(c, compactionMeta)
	cacheRequestUltraMode(c, resolveRequestUltraMode(nil, rawBody))

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rememberDaybreakRequest(c, rawBody)
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
	c.Set("raw_body", rawBody)
	if mappedModel != "" {
		model = mappedModel
	}
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}

	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(model)
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
	if result := validator.ValidateRequest(rules); !result.Valid {
		apiErr = validator.ToAPIError()
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}

	if len(rawBody) > security.MaxRequestBodySize {
		apiErr = api.NewAPIError(api.ErrCodeInvalidRequest, "request body too large", api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseMessageTooBig, apiErr.Message, apiErr)
	}
	if err := security.ValidateModelName(model); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, "invalid model parameter", api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}
	auditEndpoint := "/v1/responses"
	if options != nil {
		if configured := strings.TrimSpace(options.auditEndpoint); configured != "" {
			auditEndpoint = configured
		}
	}
	if blocked, delegated := h.inspectPromptFilterOpenAIForWebSocket(c, conn, rawBody, auditEndpoint, model, policyEventID); blocked {
		// A verified NewAPI connection owns warning/ban state. Keep the upstream
		// WebSocket alive after returning the signed decision so NewAPI can show
		// the first warning and accept another frame; it closes both peers only
		// when its own configured punishment threshold is reached.
		if delegated {
			return nil
		}
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, "prompt blocked", nil)
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}

	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	hasPreviousResponse := strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String()) != ""
	turnContinuation := codexWSTurnContinuationToken(rawBody) != ""
	_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)
	respCacheOwner := responseCacheOwner(apiKeyID)
	if gjson.GetBytes(rawBody, "store").Type == gjson.False {
		respCacheOwner = nativeWSTurnCacheOwner(c, respCacheOwner, nativeWSTurnScope{body: rawBody, identity: sessionIdentity, databaseScope: h.db.RuntimeCacheScope()})
	}
	lookupOwner := respCacheOwner
	lineageScope := ""
	if options != nil && options.lineage != nil && strings.HasPrefix(respCacheOwner, nativeWSCachePrefix) {
		lineageScope = responsesWSLineageScope(c, sessionIdentity, h.db.RuntimeCacheScope())
		parentOwner, lookupErr := options.lineage.lookup(c.Request.Context(), strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String()), lineageScope)
		if lookupErr != nil {
			if c.Request.Context().Err() != nil {
				return errResponsesWSClientGone
			}
			return writeResponsesWSErrorForContext(c, conn, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusServiceUnavailable, "context_commit_pending")))
		}
		if parentOwner != "" {
			lookupOwner = parentOwner
		}
	}
	markResponsesWSContinuationCapable(respCacheOwner, rawBody)
	ruleIdentity := h.payloadRuleIdentity(c)
	// The context-compaction turn is exempt from the first-token timeout watchdog (issue #381): the first frame of a compaction is naturally slow, so switching accounts on timeout does not help.
	bodySignalCompact := compactionMeta.ProtocolTriggered
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	codexBody, naturalImageIntent := prepareResponsesWebSocketTurnBody(rawBody)
	// Fix the ancestor before generation; native WS reads the shared snapshot when necessary, and history merging and serialisation still run on demand.
	// strip policy: after stripping the image-tool capability declaration, continue as an ordinary text request (issue #411).
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		apiErr = api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, err)
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if status, msg := h.enforceAPIKeyLimits(c, effectiveModel); status != 0 {
		errType := api.ErrorTypeRateLimit
		errCode := api.ErrCodeRateLimitReached
		closeCode := websocket.CloseTryAgainLater
		if status == http.StatusForbidden {
			errType = api.ErrorTypePermission
			errCode = api.ErrCodeInvalidRequest
			closeCode = websocket.ClosePolicyViolation
		}
		if status == http.StatusBadRequest {
			errType, errCode, closeCode = api.ErrorTypeInvalidRequest, api.ErrCodeInvalidParameter, websocket.ClosePolicyViolation
		}
		apiErr = api.NewAPIError(errCode, msg, errType)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(closeCode, apiErr.Message, apiErr)
	}
	// Only a request that passed payload, prompt-policy and API-key admission may
	// replace the active owner. Claim before concurrency/account acquisition so
	// the old request can release those leases for the new one.
	// When superseded by a newer connection, first send a 1013 close frame to this connection and then cancel: Codex clients use it to retry immediately,
	// rather than silently waiting until their own idle timeout after a bare 1006 disconnect.
	notifyPreempted := func() {
		if streamID := c.GetString(responsesWSStreamContextKey); streamID != "" {
			_ = writeResponsesWSErrorForStream(conn, api.NewAPIError(api.ErrCodeServiceUnavailable, responsesWSSessionPreemptedCloseReason, api.ErrorTypeServer), streamID, responsesWSOverloadWriteTimeout)
			return
		}
		closeResponsesWS(conn, websocket.CloseTryAgainLater, responsesWSSessionPreemptedCloseReason)
	}
	if preemptCtx, cleanupPreempt, armed := h.beginResponsesWSSessionPreemptionWithNotify(c.Request.Context(), c, rawBody, sessionIdentity, notifyPreempted); armed {
		originalRequest := c.Request
		c.Request = originalRequest.WithContext(preemptCtx)
		defer func() {
			cleanupPreempt()
			c.Request = originalRequest
		}()
		if preemptCtx.Err() != nil {
			return errResponsesWSClientGone
		}
	}
	releaseAPIKeyConcurrency, concurrencyErr, ok := h.acquireAPIKeyConcurrencyForWebSocket(c)
	if !ok {
		_ = writeResponsesWSErrorForContext(c, conn, concurrencyErr)
		return nil
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := accountFilterForResponsesWebSocket(effectiveModel)
	accountFilter = h.withRequestModelCooldownFilter(c, c.Request.Context(), effectiveModel, accountFilter)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// resolveCompactionAffinity only errors when the known sources conflict; a cache failure is treated as an unknown
	// source, keeping normal scheduling.
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		apiErr = compactionProvenanceConflictAPIError()
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
	}
	// The scope concurrency slot can only be taken after the account is selected, and is released uniformly when the request exits (issue #439 v2).
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	reqGuard := outputguard.NewRequestGuard(nativeGuardCoreConfig())

	wsRetrySettings := CurrentRuntimeSettings()
	hideUpstreamErrors := wsRetrySettings.CodexWSHideErrors
	silentRetryEnabled := wsRetrySettings.CodexWSSilentRetry
	continuousRetryPolicy := database.NormalizeContinuousRetryPolicy(wsRetrySettings.ContinuousRetryPolicy)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryDeadlineContext(c, continuousRetryPolicy)
	timeoutTerminalWritten := false
	writeTimeoutTerminal := func() error {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamTimeout, continuousRetryTimeoutMessage, api.ErrorTypeUpstream)
		if lastFailure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			message := usageLogErrorMessage(lastFailure.status, lastFailure.body)
			if message == "" {
				message = fmt.Sprintf("Upstream returned HTTP %d", lastFailure.status)
			}
			apiErr = api.NewAPIError(api.ErrorCode(fmt.Sprintf("upstream_%d", lastFailure.status)), message, api.ErrorTypeUpstream)
		}
		if !timeoutTerminalWritten {
			_ = writeResponsesWSErrorForContext(c, conn, apiErr)
			timeoutTerminalWritten = true
		}
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	defer func() {
		timedOut := settleContinuousRetryDeadline(c.Request.Context())
		if timedOut {
			returnErr = writeTimeoutTerminal()
		}
		stopRetryDeadline()
	}()
	// The continuous selector is independent from the legacy finite WebSocket
	// silent-retry switch. Selected failures use its unlimited budget even when
	// the legacy switch is disabled; unselected failures retain old semantics.
	retryEnabled := silentRetryEnabled || continuousRetryPolicy.Enabled
	maxRetries := wsRetrySettings.CodexWSSilentRetries
	if !silentRetryEnabled {
		maxRetries = 0
	}
	maxRateLimitRetries := maxRetries
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	var lastRetryableUpstreamErr *api.APIError
	retryExclusions := newRetryAccountExclusions()
	invalidEncryptedContentRetried := false
	// Official Codex uses a new ModelClientSession for every turn, and only echoes x-codex-turn-state in
	// subsequent requests of the same turn. Only when this signal and the existing binding are both present is it allowed to
	// ignore the local WHAM 100% snapshot; previous_response_id alone is not enough to prove that this is an active turn.
	continuationPinned := turnContinuation && turnHasBinding
	continuationDegraded := false
	quotaFailover := false
	if err := waitNativeWSCommit(c.Request.Context(), lookupOwner, gjson.GetBytes(codexBody, "previous_response_id").String()); err != nil {
		return writeResponsesWSErrorForContext(c, conn, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusServiceUnavailable, "context_commit_pending")))
	}
	turnReplay := newResponsesWSReplaySource(codexBody, lookupOwner)
	accountFilter = nativeWSReplayAccountFilter(accountFilter, turnReplay)
	degradeContinuation := func(reason string, attempt int) *api.APIError {
		expanded, lost, contextErr := degradeResponsesWSContinuationWithSource(codexBody, lookupOwner, turnReplay)
		if contextErr != nil {
			return contextErr
		}
		continuationDegraded = true
		continuationPinned = false
		codexBody = expanded
		turnReplay = newResponsesWSReplaySourceFromRecovery(responsesInputRaw(expanded), turnReplay)
		if lost {
			turnReplay = newResponsesWSReplaySourceFromInput("")
		}
		log.Printf("Responses WebSocket continuation degraded: reason=%s, stripped previous_response_id and retried once (attempt %d)", reason, attempt)
		return nil
	}
	preserveContinuationBinding := func() bool {
		return continuationPinned || (hasPreviousResponse && !continuationDegraded)
	}
	// When the continuation id is explicitly unknown upstream, the turn-state account pinning must not block the downgrade:
	// pinning only forbids "switching accounts", and should not throw previous_response_not_found straight at
	// Codex CLI (which carries back x-codex-turn-state almost every turn, #541).
	canDegradeContinuation := func() bool {
		return hasPreviousResponse && !continuationDegraded
	}
	var wsHTTPFallback websocketHTTPFallbackState
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	// Responses WS 入口与 compact 入口一致：选号上下文只带 Policy。
	selectionOpts := auth.SelectionOptions{Policy: dispatchPolicy}
	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	for attempt := 0; ; attempt++ {
		if c.Request.Context().Err() != nil {
			return errResponsesWSClientGone
		}
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			affinityGuard = auth.SessionAffinityGuard{}
			quotaAffinityReleased := false
			if preserveContinuationBinding() {
				if boundID, bound := h.store.SessionAffinityAccountID(affinityKey); bound {
					if limited := h.quotaBlockedContinuationAccount(boundID); limited != nil {
						replayRaw, replayBody, contextErr := responsesWSQuotaReplayBody(rawBody, codexBody, turnReplay)
						if contextErr != nil {
							logQuotaFailoverRejected(c, "websocket", limited.ID(), attempt+1, rawBody, contextErr)
							return writeResponsesWSRecoveryErrorForContext(c, conn, nativeResponsesWSContextError(contextErr))
						}
						rawBody, codexBody = replayRaw, replayBody
						turnReplay = newResponsesWSReplaySourceFromRecovery(responsesInputRaw(replayBody), turnReplay)
						continuationPinned = false
						continuationDegraded = true
						quotaFailover = true
						quotaAffinityReleased = true
						h.store.UnbindSessionAffinity(affinityKey, limited.ID())
						log.Printf("[quota-failover] complete WebSocket context admitted; releasing exhausted account affinity (account=%d, attempt=%d, encrypted_reasoning=%d)", limited.ID(), attempt+1, quotaFailoverEncryptedReasoning(rawBody))
					}
				}
			}
			if !continuationPinned && hasPreviousResponse && !continuationDegraded {
				// When the bound account has already been hard-excluded by this request (a 429/5xx from the previous turn, etc.), do not wait 30s for it:
				// exclusions do not lift within this request, so strip previous_response_id and switch accounts directly.
				// This is unrelated to the turn-state WHAM exception: even if this is not an active turn, an old continuation
				// id must not be carried onto a clearly different account to fail repeatedly.
				if boundID, bound := h.store.SessionAffinityAccountID(affinityKey); bound {
					if exclude := retryExclusions.ForSelection(); exclude[boundID] {
						if contextErr := degradeContinuation(fmt.Sprintf("bound account %d excluded by this request", boundID), attempt+1); contextErr != nil {
							return writeResponsesWSRecoveryErrorForContext(c, conn, contextErr)
						}
					}
				}
			}
			if attempt == 0 && compactionAffinity.Known && !continuationPinned {
				account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			}
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			} else if continuationPinned {
				// The continuation retry also passes SelectionOptions, so that the retry does not drop the channel context.
				account, stickyProxyURL, selectionErr = h.nextRetryAccountForContinuationWithSelection(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
			} else if !quotaAffinityReleased || h.quotaFailoverHasCandidate(affinityKey, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, selectionOpts)
			}
		}
		if account == nil {
			if c.Request.Context().Err() != nil {
				return errResponsesWSClientGone
			}
			if selectionAPIError := schedulerSelectionAPIError(selectionErr); selectionAPIError != nil {
				apiErr = selectionAPIError
			} else if compactionAffinity.Known {
				apiErr = compactionUpstreamUnavailableAPIError()
			} else if lastRetryableUpstreamErr != nil {
				apiErr = responsesWSClientUpstreamAPIError(lastRetryableUpstreamErr, hideUpstreamErrors)
			} else if (lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0) || lastFailureModelUnsupported(lastStatusCode, lastBody) {
				apiErr = responsesWSUpstreamAPIError(lastStatusCode, lastBody)
			} else if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				// Candidates were emptied by the scope budget (issue #439): reply with rate-limit semantics, not "no available account".
				apiErr = api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit)
			} else if limited := h.store.UsageLimitedCandidateSummary(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy); limited.Found {
				// WS frames have no Retry-After header, so the wait seconds for a transient throttle are written into the message text.
				apiErr = api.NewAPIError(api.ErrCodeRateLimitReached, usageLimitedPoolMessages(limited).Chinese, api.ErrorTypeRateLimit)
			} else if h.accountPoolConcurrencySaturated(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				apiErr = api.NewAPIError(api.ErrorCode(ErrorCodeAccountPoolConcurrencySaturated), concurrencySaturatedMessageZH, api.ErrorTypeServer)
			} else if msg, ok := h.unsupportedModelPoolMessage(apiKeyID, retryExclusions.ForSelection(), effectiveModel, dispatchPolicy); ok {
				apiErr = api.NewAPIError(api.ErrorCode(auth.ModelCooldownReasonUnsupported), msg, api.ErrorTypeInvalidRequest)
			} else {
				apiErr = api.NewAPIError(api.ErrCodeServiceUnavailable, noAvailableAccountMessage(effectiveModel), api.ErrorTypeServer)
			}
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSErrorForContext(c, conn, apiErr)
			return nil
		}
		if attempt > 0 {
			clearNewAPIUpstreamCyberPolicyDecision(c)
		}

		if compatibilityErr := selectedCodexResponsesCompatibilityError(c, account, rawBody); compatibilityErr != nil {
			h.store.Release(account)
			_ = writeResponsesWSErrorForContext(c, conn, compatibilityErr)
			return newResponsesWSCloseError(websocket.ClosePolicyViolation, compatibilityErr.Message, compatibilityErr)
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		turnReplay.setAccount(account)
		if continuationDegraded && turnReplay.Input() == "" && turnReplay.err != nil {
			h.store.Release(account)
			return writeResponsesWSRecoveryErrorForContext(c, conn, turnReplay.err)
		}
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback && !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return errResponsesWSClientGone
			}
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("Responses WebSocket upstream HTTP fallback attempt started (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}

		apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()
		if quotaFailover {
			downstreamHeaders.Del(codexTurnStateHeader)
		}

		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		// The identity is attached to each attempt with the dimension of the account actually selected: account_* gates are re-matched when a retry switches accounts (issue #410).
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
		useWebsocket := !wsHTTPFallback.ForceHTTP()
		// Image-generation requests are routed to the HTTP upstream instead (the client is still WS): a WebSocket upstream transfer of large image data
		// hangs (issue #220); a natural-language image-generation intent also needs to keep the image tool (issue #288).
		if useWebsocket && (responsesBodyRequestsImageGeneration(rawBody) || naturalImageIntent) {
			useWebsocket = false
		}
		// When the body size reaches the learned 1009 threshold, go straight to HTTP on first send, skipping the WS wait that is sure to fail (issue #404).
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] request body %dKB reached the learned 1009 size threshold, going directly to the HTTP upstream (endpoint=/v1/responses, ingress=ws)", len(codexBody)/1024)
			}
		}
		// On the WebSocket upstream, strip the automatically injected image tool, to prevent the model from hanging when it generates images on its own.
		upstreamBody := codexBody
		attemptReplay := turnReplay
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		} else if prevID := strings.TrimSpace(gjson.GetBytes(codexBody, "previous_response_id").String()); prevID != "" {
			var contextErr *api.APIError
			var lost bool
			upstreamBody, lost, contextErr = degradeResponsesWSContinuationWithSource(codexBody, lookupOwner, turnReplay)
			if contextErr != nil {
				ttftGuard.Stop()
				upstreamCancel()
				h.store.Release(account)
				return writeResponsesWSRecoveryErrorForContext(c, conn, contextErr)
			}
			// The snapshot was already computed once at the time of degradation; reuse the expanded input directly, without filtering again.
			attemptReplay = newResponsesWSReplaySourceFromRecovery(responsesInputRaw(upstreamBody), turnReplay)
			attemptReplay.setAccount(account)
			if lost {
				attemptReplay = nil
			}
		}
		// service_tier accounting is attributed by the value after payload rule rewriting (only rules that override service_tier take effect).
		serviceTier = EffectiveRequestedServiceTier(upstreamBody, effectiveModel, downstreamHeaders, attemptIdentity)
		// Derive the upstream identity key only after useWebsocket is finally determined: consistent with the handler.go
		// Responses/ChatCompletions path: with no explicit session, each request isolates its upstream identity by default,
		// and on the WS path the stateless slot pool of ExecuteRequest handles it.
		upstreamCtx = context.WithValue(upstreamCtx, encryptedContentSessionKey{}, sessionIdentity.affinityID)
		upstreamCtx = WithCodexTurnStateAffinityKey(upstreamCtx, affinityKey)
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		})
		durationMs := int(time.Since(start).Milliseconds())
		if c.Request.Context().Err() != nil {
			ttftGuard.Stop()
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			h.store.Release(account)
			return errResponsesWSClientGone
		}

		if reqErr != nil {
			var capacityErr *UpstreamWSConnectionCapacityError
			if errors.As(reqErr, &capacityErr) {
				ttftGuard.Stop()
				upstreamCancel()
				h.store.Release(account)
				return writeResponsesWSErrorForContext(c, conn, api.NewAPIError(api.ErrorCode(ErrorCodeUpstreamWSConnectionCapacity), capacityErr.Error(), api.ErrorTypeServer))
			}
			var continuationBusy *ResponsesContinuationBusyError
			if errors.As(reqErr, &continuationBusy) {
				ttftGuard.Stop()
				upstreamCancel()
				h.store.Release(account)
				return writeResponsesWSErrorForContext(c, conn, api.NewAPIError(api.ErrCodeServiceUnavailable, continuationBusy.Error(), api.ErrorTypeServer))
			}
			var continuationLost *ResponsesContinuationLostError
			if errors.As(reqErr, &continuationLost) {
				ttftGuard.Stop()
				upstreamCancel()
				h.store.Release(account)
				if canDegradeContinuation() {
					if contextErr := degradeContinuation(continuationLost.Reason, attempt+1); contextErr != nil {
						return writeResponsesWSRecoveryErrorForContext(c, conn, contextErr)
					}
					continue
				}
				return writeResponsesWSRecoveryErrorForContext(c, conn, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusConflict, continuationLost.Reason)))
			}
			if quotaErr := apiKeyModelRequestError(reqErr); quotaErr != nil {
				ttftGuard.Stop()
				h.store.Release(account)
				// A model-specific budget must not close the connection for other models.
				return writeResponsesWSErrorForContext(c, conn, quotaErr.apiErr)
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
				log.Printf("Responses WebSocket upstream close 1009; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			shouldRetry := retryEnabled && retryable && shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			// Transport-class failures retry sticky on the same account: no account failure is recorded, no affinity is unbound, no hard exclusion (issue #331)
			stickyRetry := h.shouldStickyTransportRetry(reqErr, kind, timedOut, shouldRetry, continuousRetryPolicy)
			if retryable && kind != "" && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if retryable && !stickyRetry && !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("Responses WebSocket upstream first token timeout, retrying with another account (attempt %s, account %d): %v", retryAttemptProgress(attempt, maxRetries), account.ID(), reqErr)
				if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), true, generalRetries, retryLimit) {
					return errResponsesWSClientGone
				}
				continue
			}
			if retryable && !timedOut && !stickyRetry {
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			}

			if !retryable {
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				_ = writeResponsesWSErrorForContext(c, conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseInternalServerErr, clientErr.Message, reqErr)
			}
			log.Printf("Responses WebSocket upstream request failed (attempt %d): %v", attempt+1, reqErr)
			lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)) {
					return errResponsesWSClientGone
				}
				if !h.bindBufferedStickyRetryAffinity(c.Request.Context(), affinityKey, account, proxyURL, stickyRetry, continuousRetryPolicy) {
					return errResponsesWSClientGone
				}
				if stickyRetry {
					log.Printf("transport error sticky retry: keeping account %d and session affinity (attempt %s, ws)", account.ID(), retryAttemptProgress(attempt, maxRetries))
				}
				continue
			}
			apiErr = api.NewAPIError(api.ErrCodeUpstreamError, reqErr.Error(), api.ErrorTypeUpstream)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSErrorForContext(c, conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, reqErr)
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBodyBytes))
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if c.Request.Context().Err() != nil {
				h.store.Release(account)
				return errResponsesWSClientGone
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
						// Keep the admitted ancestor while dropping rejected opaque items from
						// this attempt. A previously lossy fallback must remain unwritable.
						if turnReplay != nil && !(turnReplay.precomputed && turnReplay.input == "") {
							updated := newResponsesWSReplaySource(codexBody, lookupOwner)
							if turnReplay.previous != nil {
								updated.previous = turnReplay.previous
							}
							turnReplay = updated
						}
					}
					log.Printf("Responses WebSocket upstream rejected encrypted_content, stripped encrypted reasoning context and retried once (attempt %d)", attempt+1)
					h.store.Release(account)
					if !preserveContinuationBinding() {
						h.store.UnbindSessionAffinity(affinityKey, account.ID())
					}
					continue
				}
			}

			// Upstream does not recognise the continuation id: the account itself is fine, so do not record a failure or exclude it,
			// downgrade to a self-contained request and retry once in place (issue #400).
			if canDegradeContinuation() && isPreviousResponseNotFoundBody(errBody) {
				if contextErr := degradeContinuation(fmt.Sprintf("upstream rejected previous_response_id on account %d", account.ID()), attempt+1); contextErr != nil {
					h.store.Release(account)
					return writeResponsesWSRecoveryErrorForContext(c, conn, contextErr)
				}
				SyncCodexUsageState(h.store, account, resp)
				h.store.Release(account)
				continue
			}

			if kind := classifyHTTPFailure(resp.StatusCode, errBody); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			if !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, maxRateLimitRetries, continuousRetryPolicy)

			log.Printf("Responses WebSocket upstream returned error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := retryEnabled && shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
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
				Stream:                 true,
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
				lastRetryableUpstreamErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return errResponsesWSClientGone
				}
				continue
			}
			if metadata, delegated := newAPIUpstreamCyberPolicyDecision(c); delegated && metadata.EventID != "" {
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				_ = writeResponsesWSErrorForContext(c, conn, newAPIPolicyDecisionAPIError(metadata))
				return nil
			}

			apiErr = responsesWSUpstreamAPIError(resp.StatusCode, errBody)
			clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
			if !claimContinuousRetrySuccessContext(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			_ = writeResponsesWSErrorForContext(c, conn, clientErr)
			return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
		}

		var fallbackLog *websocketHTTPFallbackState
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			fallbackLog = &wsHTTPFallback
		}
		preserveAffinity := preserveContinuationBinding()
		allowContinuationDegrade := canDegradeContinuation()
		if err := h.streamResponsesWSUpstream(c, conn, resp, account, proxyURL, affinityKey, affinityGuard, preserveAffinity, allowContinuationDegrade, logModel, effectiveModel, logEffectiveModel, reasoningEffort, serviceTier, respCacheOwner, lineageScope, attemptReplay, start, ttftGuard, retryEnabled, hideUpstreamErrors, useWebsocket, fallbackLog, attempt+1, options, continuousRetryPolicy, reqGuard, upstreamCancel); err != nil {
			if continuousRetryDeadlineExceeded(c.Request.Context()) {
				return errResponsesWSClientGone
			}
			var continuationErr *responsesWSContinuationNotFoundError
			if canDegradeContinuation() && errors.As(err, &continuationErr) {
				// The account was already released inside the stream, with no failure recorded and no unbinding: strip the continuation id and retry once in place.
				// Turn-state pinning takes this same path: upstream has already said the id cannot be found, switching accounts does not help, only stripping the id lets it continue.
				if contextErr := degradeContinuation(fmt.Sprintf("upstream rejected previous_response_id on account %d", account.ID()), attempt+1); contextErr != nil {
					return writeResponsesWSRecoveryErrorForContext(c, conn, contextErr)
				}
				continue
			}
			var retryErr *responsesWSRetryableStreamError
			if errors.As(err, &retryErr) {
				lastRetryableUpstreamErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				if useWebsocket && isWebsocketMessageTooBigOutcome(retryErr.outcome) {
					wsElapsed := time.Since(start)
					globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
					wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(retryErr.outcome.failureMessage))
					log.Printf("Responses WebSocket upstream close 1009 before first event; retaining account lease and falling back to HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), retryErr.outcome.failureMessage)
					continue
				}
				eventType := strings.TrimSpace(retryErr.eventType)
				if retryEnabled && shouldTransparentRetryStreamEventWithBudgets(retryErr.outcome, eventType, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries, false, c.Request.Context().Err(), nil, continuousRetryPolicy) {
					rememberContinuousRetryStreamFailure(c.Request.Context(), retryErr.outcome, retryErr.outcome.failurePayload)
					if isFirstTokenTimeoutOutcome(retryErr.outcome) {
						retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
					} else {
						retryExclusions.MarkStreamFailureForEvent(account.ID(), retryErr.outcome, eventType, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
					}
					retryOrdinal, retryLimit := retryStateForStreamEvent(retryErr.outcome, eventType, generalRetries, rateLimitRetries, maxRetries, maxRateLimitRetries, continuousRetryPolicy)
					log.Printf("Responses WebSocket upstream stream ended before first token, retrying (attempt %s, account %d): %s", retryAttemptProgress(retryOrdinal-1, retryLimit), account.ID(), retryErr.outcome.failureMessage)
					// The finite first-token timeout has already waited a full round; an unlimited budget still forces a backoff, to avoid a no-wait loop.
					if !h.waitBeforeRetryWithFirstTokenTimeout(c.Request.Context(), isFirstTokenTimeoutOutcome(retryErr.outcome), retryOrdinal, retryLimit, resp) {
						return errResponsesWSClientGone
					}
					continue
				}
				apiErr = api.NewAPIError(api.ErrCodeUpstreamError, retryErr.outcome.failureMessage, api.ErrorTypeUpstream)
				clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
				if !claimContinuousRetrySuccessContext(c.Request.Context()) {
					return errResponsesWSClientGone
				}
				_ = writeResponsesWSErrorForContext(c, conn, clientErr)
				return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
			}
			if errors.Is(err, errResponsesWSClientGone) {
				return err
			}
			if shouldRetryErr, ok := err.(*responsesWSCloseError); ok && shouldRetryErr.code == websocket.CloseTryAgainLater && !preserveContinuationBinding() {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			return err
		}
		return nil
	}
}

func replayResponsesWSSuccess(replay *continuousRetryWSReplay, outputBuffer *wsPromptOutputBuffer, writeMessage func([]byte) error) (bool, error) {
	if replay == nil {
		return false, nil
	}
	if writeMessage == nil {
		return false, errors.New("nil Responses WebSocket replay writer")
	}
	wroteAny := false
	writeFiltered := func(messages [][]byte) error {
		for _, message := range messages {
			if err := writeMessage(message); err != nil {
				return err
			}
			wroteAny = true
		}
		return nil
	}
	if err := replay.ForEachMessage(func(payload []byte) error {
		if outputBuffer == nil {
			return writeFiltered([][]byte{payload})
		}
		release, err := outputBuffer.Push(payload)
		if err != nil {
			return err
		}
		return writeFiltered(release)
	}); err != nil {
		return wroteAny, err
	}
	if outputBuffer == nil {
		return wroteAny, nil
	}
	remaining, err := outputBuffer.Flush()
	if err != nil {
		return wroteAny, err
	}
	if err := writeFiltered(remaining); err != nil {
		return wroteAny, err
	}
	return wroteAny, nil
}

func (h *Handler) streamResponsesWSUpstream(
	c *gin.Context,
	conn *websocket.Conn,
	resp *http.Response,
	account *auth.Account,
	proxyURL string,
	affinityKey string,
	affinityGuard auth.SessionAffinityGuard,
	preserveAffinity bool,
	allowContinuationDegrade bool,
	model string,
	effectiveModel string,
	logEffectiveModel string,
	reasoningEffort string,
	serviceTier string,
	respCacheOwner string,
	lineageScope string,
	replayInput *responsesWSReplaySource,
	start time.Time,
	ttftGuard *firstTokenTimeoutGuard,
	retryEnabled bool,
	hideUpstreamErrors bool,
	viaWebsocket bool,
	fallbackLog *websocketHTTPFallbackState,
	fallbackAttempt int,
	options *responsesWSForwardOptions,
	continuousRetryPolicy database.ContinuousRetryPolicy,
	reqGuard *outputguard.RequestGuard,
	upstreamCancel context.CancelFunc,
) error {
	upstreamEndpoint := "/v1/responses"
	account.Mu().RLock()
	c.Set("x-account-email", account.Email)
	account.Mu().RUnlock()
	c.Set("x-account-proxy", proxyURL)
	c.Set("x-model", model)
	c.Set("x-reasoning-effort", reasoningEffort)

	var firstTokenMs int
	outputBuffer := newWSPromptOutputBuffer(h.promptFilterConfigForRequest(c))
	var usage *UsageInfo
	var actualServiceTier string
	ttftRecorded := false
	// contentTokenSeen uses the strict judgement (unrelated to the lenient first-token statistics). Under the lenient mode
	// codex.rate_limits / metadata set ttftRecorded; local 2004 also enabled
	// preflight passthrough, and these two frames are written first and set wroteAnyBody. If they were used for the
	// "before the first packet" judgement, the previous_response_not_found downgrade could never be entered on the real upstream (#541).
	contentTokenSeen := false
	preflightSettings := CurrentRuntimeSettings()
	preflightSettings.ContinuousRetryPolicy = continuousRetryPolicy
	preflightPassthrough := continuousRetryPreflightPassthrough(preflightSettings)
	gotTerminal := false
	deltaCharCount := 0
	var readErr error
	var writeErr error
	clientGone := false
	var imageLogInfo imageUsageLogInfo
	var terminalFailurePayload []byte
	var terminalFailureClientPayload []byte
	var preContentErrorCandidate []byte
	var completedResponsePayload []byte
	responseModelObserver := &upstreamResponseModelObserver{}
	outputCollector := newResponseOutputCollector()
	emptyIncomplete := &emptyIncompleteTracker{}
	terminalFailureEventType := ""
	wroteAnyBody := false
	multiAgentV2State := codexMultiAgentV2StateFromContext(c.Request.Context())
	var wsReplay *continuousRetryWSReplay
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		wsReplay = h.newContinuousRetryWSReplay()
	}
	guardCoord := NewGuardTerminationCoordinator(reqGuard, upstreamCancel, resp.Body)
	responsesProjector := NewResponsesProjector()
	finishContextCommit := func() {}
	finishLineageCommit := func() {}
	defer func() { finishContextCommit(); finishLineageCommit() }()
	writeClientMessage := func(payload []byte) error {
		if isResponsesSuccessTerminalEvent(gjson.GetBytes(payload, "type").String()) {
			finishContextCommit = beginNativeWSCommit(respCacheOwner, gjson.GetBytes(payload, "response.id").String())
			if options != nil && options.lineage != nil && strings.HasPrefix(respCacheOwner, nativeWSCachePrefix) {
				finishLineageCommit = options.lineage.beginCommit(gjson.GetBytes(payload, "response.id").String(), lineageScope)
			}
		}
		return writeResponsesWSObservedMessage(c.Request.Context(), conn, payload)
	}
	// Set when a non-retryable response.failed is received before the first token: the raw failure frame is not passed through to the client,
	// the error frame is rewritten outside the loop and the connection is closed with a non-normal close code by error category,
	// so that the downstream relay or biller clearly perceives the failure rather than treating it as a normally ended session.
	abortedForErrorClose := false
	// Set when a continuation request is rejected upstream with previous_response_not_found before the first packet: the failure frame is not passed through,
	// and control returns to the outer loop to strip previous_response_id and retry once (issue #400).
	continuationNotFound := false
	pendingFirstTokenMessages := make([][]byte, 0, 4)
	pendingFirstTokenBytes := 0

	flushPendingFirstTokenMessages := func() bool {
		for _, pending := range pendingFirstTokenMessages {
			release, filterErr := outputBuffer.Push(pending)
			if filterErr != nil {
				writeErr = filterErr
				return false
			}
			wrotePending := false
			for _, filtered := range release {
				if err := writeClientMessage(filtered); err != nil {
					writeErr = err
					clientGone = true
					return false
				}
				wrotePending = true
			}
			wroteAnyBody = wroteAnyBody || wrotePending
		}
		pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
		pendingFirstTokenBytes = 0
		return true
	}

	readErr = readSSEStreamWithContinuousRetryKeepalive(c.Request.Context(), resp.Body, func(sseEvent string, data []byte) bool {
		observeResponsesWSInterruptAck(c.Request.Context(), data)
		if gotTerminal && !isResponsesWSInterruptAck(gjson.GetBytes(data, "type").String()) {
			return true
		}
		if multiAgentV2State != nil && multiAgentV2State.isOptimizedFor(account.ID()) {
			data = restoreCodexMultiAgentV2Response(data, multiAgentV2State)
		}
		// [OutputGuard pre-check] must come before the provenance, collector, replay and downstream write paths
		if err := ObserveEvent(reqGuard, responsesProjector, normalizedUpstreamSSEEventType(sseEvent, data), data); err != nil {
			gotTerminal = true
			guardCoord.HandleTripped(reqGuard.TrippedReason(), func() {
				if wsReplay != nil {
					_ = wsReplay.Close()
					wsReplay = nil
				}
				pendingFirstTokenMessages = nil
				pendingFirstTokenBytes = 0
				completedResponsePayload = nil
			})
			_ = sendResponsesWSGuardTerminal(conn, c.GetString(responsesWSStreamContextKey), reqGuard.TrippedReason())
			clientGone = true
			return false
		}
		if wsReplay == nil {
			h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
		}
		outputCollector.Add(data)
		parsed := gjson.ParseBytes(data)
		eventType := normalizedUpstreamSSEEventType(sseEvent, data)
		ApplyCodexUsageEventFrame(h.store, account, eventType, parsed, time.Now())
		eventType, data, parsed = rewriteEmptyIncompleteTerminal(emptyIncomplete, eventType, data, parsed)
		clientData := data
		if options != nil && options.transformClientEvent != nil {
			if transformed := options.transformClientEvent(data); len(transformed) > 0 {
				clientData = transformed
			}
		}
		// Capacity load-shedding codes (server_is_overloaded/slow_down) are fatal errors for the Codex CLI:
		// once they are to be passed through to the client they are rewritten as a retryable server_error. Cooldown/billing/logging use the
		// original data of terminalFailurePayload from before the rewrite, which is unaffected.
		clientData = sanitizeCapacityShedEventForClient(eventType, clientData)
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
		if eventType == "response.output_text.delta" {
			deltaCharCount += len(parsed.Get("delta").String())
		}
		if image, ok := extractImageFromOutputItemDone(data, model); ok {
			imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
		}
		observeUpstreamResponseModelFrame(responseModelObserver, parsed, eventType)
		if isResponsesSuccessTerminalEvent(eventType) {
			usage = extractUsageFromResult(parsed.Get("response.usage"))
			if tier := parsed.Get("response.service_tier").String(); tier != "" {
				actualServiceTier = tier
			}
			if eventType == "response.completed" || eventType == "response.incomplete" {
				completedResponsePayload = append([]byte(nil), data...)
			}
			gotTerminal = true
			preContentErrorCandidate = nil
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
			terminalFailureEventType = eventType
			gotTerminal = true
			preContentErrorCandidate = nil
		}
		if wsReplay != nil {
			// Continuous retry keeps the complete attempt private. Any upstream
			// error event is authoritative and ends the attempt immediately, even if
			// the provider later appends a contradictory response.completed frame.
			// Selective and catch-all modes differ only in whether the classified
			// failure below is selected for another attempt.
			if eventType == "error" {
				terminalFailurePayload = append([]byte(nil), data...)
				terminalFailureEventType = eventType
				gotTerminal = true
			}
			if eventType == "error" || eventType == "response.failed" {
				if allowContinuationDegrade && !contentTokenSeen && isPreviousResponseNotFoundBody(data) {
					continuationNotFound = true
					return false
				}
				terminalFailureClientPayload = append([]byte(nil), clientData...)
				return false
			}
			if err := wsReplay.WriteMessage(clientData); err != nil {
				writeErr = err
				return false
			}
			return !isResponsesSuccessTerminalEvent(eventType)
		}
		standaloneErrorAfterOutput := eventType == "error" && wroteAnyBody
		if !contentTokenSeen && !wroteAnyBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy) {
			preContentErrorCandidate = append(preContentErrorCandidate[:0], data...)
			return true
		}
		if standaloneErrorAfterOutput {
			terminalFailurePayload = append([]byte(nil), data...)
			terminalFailureEventType = "error"
			gotTerminal = true
		}
		if !clientGone {
			// Retryable error frames (the upstream overload leading frame) are buffered like lifecycle frames: writing them out immediately
			// would set wroteAnyBody, and the subsequent response.failed would then miss the silent account-switch branch before the first packet.
			// The leading error frame of previous_response_not_found is buffered in the same way: it is classified as invalid_request,
			// not a retryable frame, and writing it out immediately would set wroteAnyBody, so the subsequent response.failed
			// would miss the continuation downgrade branch below.
			shouldDefer := shouldDeferPreContentSSEEvent(eventType, contentTokenSeen, gotTerminal, preflightPassthrough) ||
				(!contentTokenSeen && !wroteAnyBody && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data, continuousRetryPolicy)) ||
				(allowContinuationDegrade && !contentTokenSeen && !gotTerminal && eventType == "error" && isPreviousResponseNotFoundBody(data))
			if shouldDefer {
				pendingFirstTokenMessages = append(pendingFirstTokenMessages, append([]byte(nil), clientData...))
				pendingFirstTokenBytes += len(clientData)
				if pendingFirstTokenBytes <= 1024*1024 {
					return !isResponsesTerminalEvent(eventType)
				}
				if !flushPendingFirstTokenMessages() {
					return false
				}
			} else {
				// When a retryable response.failed (quota exhausted, rate limit, 5xx, 401) is received before the first packet,
				// do not send the failure frame to the client: discard the leading buffer not yet sent and end reading early,
				// letting the outer loop transparently switch to a healthy account and retry, to avoid the client repeatedly reconnecting.
				// If content has already been written to the client (wroteAnyBody / first token already recorded), pass it through as usual.
				if (retryEnabled || hideUpstreamErrors) && eventType == "response.failed" && !contentTokenSeen && !wroteAnyBody && continuousRetryStreamFailureSelected(classifyResponseFailedOutcome(terminalFailurePayload), terminalFailurePayload, eventType, continuousRetryPolicy) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					return false
				}
				// The upstream does not recognise the continuation id: switching accounts does not help (it still cannot be found), but after stripping the continuation id
				// the same account can continue. Preamble metadata (rate_limits / metadata), even if already written out,
				// does not count as content and must not block the downgrade: real ChatGPT WS almost always pushes these two frames first.
				if allowContinuationDegrade && eventType == "response.failed" && !contentTokenSeen &&
					isPreviousResponseNotFoundBody(terminalFailurePayload) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					continuationNotFound = true
					return false
				}
				// A non-retryable response.failed before the first token (such as context_length_exceeded)
				// does not pass the raw failure frame through: discard the leading buffer, end reading early, and outside the loop
				// return an error frame with a non-normal close code by its real error semantics (aligned with the SSE path returning 4xx).
				// A retryable failure is not intercepted here: with silent retry enabled it is retried on another account by the branch above;
				// with it disabled, the failure frame is passed through as-is per the existing convention.
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentTokenSeen, wroteAnyBody, clientGone) &&
					!continuousRetryStreamFailureSelected(classifyResponseFailedOutcome(terminalFailurePayload), terminalFailurePayload, eventType, continuousRetryPolicy) {
					pendingFirstTokenMessages = pendingFirstTokenMessages[:0]
					pendingFirstTokenBytes = 0
					abortedForErrorClose = true
					return false
				}
				if len(pendingFirstTokenMessages) > 0 && !flushPendingFirstTokenMessages() {
					return false
				}
				release, filterErr := outputBuffer.Push(clientData)
				if filterErr != nil {
					writeErr = filterErr
					return false
				}
				for _, filtered := range release {
					if err := writeClientMessage(filtered); err != nil {
						writeErr = err
						clientGone = true
					} else {
						wroteAnyBody = true
					}
				}
			}
		}
		return !standaloneErrorAfterOutput &&
			(!isResponsesTerminalEvent(eventType) || awaitingResponsesWSInterruptAck(c.Request.Context())) &&
			!(gotTerminal && isResponsesWSInterruptAck(eventType))
	})
	// After clientGone (guard hit or downstream write failure), suppress the failure acknowledgement frame for a pending interrupt:
	// the guard's terminal frame must be the last frame before the connection closes, and response.interrupt.failed must not be sent after the terminal frame.
	if !clientGone {
		if err := failPendingResponsesWSInterrupt(c.Request.Context(), conn); err != nil {
			writeErr = err
		}
	}
	// After a guard hit (or write failure) clientGone is set: the guard's terminal frame is
	// the last frame before the connection closes, and after a hit, any messages left in outputBuffer must not be written downstream.
	if writeErr == nil && !clientGone && outputBuffer != nil && wsReplay == nil {
		remaining, err := outputBuffer.Flush()
		if err != nil {
			writeErr = err
		} else {
			for _, message := range remaining {
				if err := writeClientMessage(message); err != nil {
					writeErr = err
					break
				}
				wroteAnyBody = true
			}
		}
	}

	// Continuation id invalid and intercepted before the first packet: the account is not at fault, so no failure is recorded and it is not unbound; control returns to the outer loop to downgrade and retry.
	if continuationNotFound && !contentTokenSeen && writeErr == nil && c.Request.Context().Err() == nil {
		_ = wsReplay.Close()
		ttftGuard.Stop()
		resp.Body.Close()
		h.store.Release(account)
		return &responsesWSContinuationNotFoundError{}
	}

	totalDuration := int(time.Since(start).Milliseconds())
	outcome := classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), readErr, writeErr, gotTerminal)
	outcome = OverlayGuardOutcome(outcome, reqGuard)
	outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
	var candidatePromoted bool
	terminalFailurePayload, candidatePromoted = resolvePreContentRetryErrorCandidate(terminalFailurePayload, preContentErrorCandidate, contentTokenSeen, wroteAnyBody, gotTerminal, readErr, c.Request.Context().Err(), writeErr)
	if candidatePromoted {
		terminalFailureEventType = "error"
	} else if len(terminalFailurePayload) > 0 && terminalFailureEventType == "" {
		terminalFailureEventType = gjson.GetBytes(terminalFailurePayload, "type").String()
	}
	if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
		outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
	}
	ttftGuard.Stop()
	var responseFailedDecision codex429Decision
	promptPolicyIncidentID := ""
	if len(terminalFailurePayload) > 0 && !outcome.terminalLocal {
		outcome = classifyResponseFailedOutcome(terminalFailurePayload)
		if withContinuousRetryDeadlinePending(c.Request.Context(), func() {
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
		}) {
			outcome = applyResponseFailedDecisionKind(outcome, terminalFailurePayload, responseFailedDecision)
		} else {
			outcome = classifyStreamOutcome(errContinuousRetryDeadlineExceeded, nil, nil, false)
		}
		// The cyber_policy penalty in a streaming response.failed (HTTP 200) must also be recorded,
		// otherwise only non-2xx error bodies would be logged to the prompt-filter log.
		promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", model, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
			Transport: upstreamPromptPolicyTransport(true, viaWebsocket), StatusCode: outcome.logStatusCode,
			AccountID: account.ID(), AttemptIndex: fallbackAttempt,
		}))
		if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
			outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
		}
	}
	outcome = OverlayGuardOutcome(outcome, reqGuard)
	outcome = overlayContinuousRetryLocalFailure(outcome, readErr, writeErr)
	if fallbackLog != nil {
		fallbackLog.LogHTTPAttemptCompletion("/v1/responses", account.ID(), fallbackAttempt, totalDuration, firstTokenMs, outcome.logStatusCode)
	}
	downstreamWroteBeforeCommit := wroteAnyBody && wsReplay == nil
	transparentRetrySelected := retryEnabled && continuousRetryStreamFailureSelected(outcome, terminalFailurePayload, terminalFailureEventType, continuousRetryPolicy) && !downstreamWroteBeforeCommit && c.Request.Context().Err() == nil && writeErr == nil
	if metadata, delegated := newAPIUpstreamCyberPolicyDecision(c); delegated && metadata.EventID != "" && !transparentRetrySelected {
		_ = wsReplay.Close()
		// WebSocket response headers were committed during the upgrade. Carry the
		// signed CYB decision in a per-turn error frame so NewAPI can count it and
		// decide whether this is the first warning or a ban-worthy recurrence.
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			resp.Body.Close()
			h.store.Release(account)
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSErrorForContext(c, conn, newAPIPolicyDecisionAPIError(metadata))
		return nil
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, viaWebsocket, downstreamWroteBeforeCommit, c.Request.Context().Err(), writeErr) {
		_ = wsReplay.Close()
		resp.Body.Close()
		return &responsesWSRetryableStreamError{outcome: outcome, eventType: terminalFailureEventType}
	}
	if transparentRetrySelected {
		_ = wsReplay.Close()
		clearNewAPIUpstreamCyberPolicyDecision(c)
		h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
			AccountID: account.ID(), Endpoint: "/v1/responses", Model: model, EffectiveModel: logEffectiveModel,
			StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
			InboundEndpoint: "/v1/responses", UpstreamEndpoint: upstreamEndpoint, Stream: true, ViaWebsocket: viaWebsocket,
			AttemptIndex: fallbackAttempt, UpstreamErrorKind: outcome.failureKind,
			ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
		}, promptPolicyIncidentID)
		resp.Body.Close()
		if !isFirstTokenTimeoutOutcome(outcome) {
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
		if !preserveAffinity {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		}
		return &responsesWSRetryableStreamError{outcome: outcome, eventType: terminalFailureEventType}
	}
	if outcome.logStatusCode == http.StatusOK {
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			if wsReplay != nil {
				_ = wsReplay.Close()
			}
			resp.Body.Close()
			h.store.Release(account)
			return errResponsesWSClientGone
		}
		SyncCodexUsageState(h.store, account, resp)
		if wsReplay != nil {
			committedAny, commitErr := replayResponsesWSSuccess(wsReplay, outputBuffer, writeClientMessage)
			wroteAnyBody = wroteAnyBody || committedAny
			if commitErr != nil {
				writeErr = commitErr
				// Filtering, replay storage, and downstream writes are local commit
				// failures. They terminate this turn and must never reopen an upstream
				// attempt that already reached a successful protocol terminal.
				outcome = classifyStreamOutcome(continuousRetryContextError(c.Request.Context()), nil, writeErr, false)
				outcome = overlayContinuousRetryLocalFailure(outcome, commitErr)
			}
		}
	}
	downstreamWrote := wroteAnyBody
	if outcome.logStatusCode == http.StatusOK && writeErr == nil && downstreamWrote {
		if wsReplay != nil {
			_ = wsReplay.ForEachMessage(func(payload []byte) error {
				h.recordCompactionProvenanceFromPayload(context.Background(), account, payload)
				return nil
			})
		}
		if len(completedResponsePayload) > 0 {
			if options != nil && options.onResponseCompleted != nil && gjson.GetBytes(completedResponsePayload, "type").String() == "response.completed" {
				options.onResponseCompleted(append([]byte(nil), completedResponsePayload...))
			}
			// Committed completed/incomplete responses can be continuation history.
			// Failed attempts and locally blocked/unwritable replays leave no cache.
			if !outputCollector.overflow {
				committedInput := replayInput.Input()
				cacheCommittedResponsesWSContext(nativeWSCompletedContext{owner: respCacheOwner, input: committedInput,
					completed: completedResponsePayload, outputs: outputCollector.Items(), provenance: replayInput.committedProvenance()})
				if committedInput != "" && options != nil && options.lineage != nil {
					options.lineage.remember(gjson.GetBytes(completedResponsePayload, "response.id").String(), respCacheOwner, lineageScope)
				}
			}
		}
	}
	finishContextCommit()
	finishLineageCommit()
	_ = wsReplay.Close()
	if continuousRetryBufferedAttemptCommitted(continuousRetryPolicy, outcome) {
		h.store.BindSessionAffinityWithGuard(affinityKey, account, proxyURL, affinityGuard)
	}
	if outcome.logStatusCode != http.StatusOK {
		log.Printf("Responses WebSocket stream ended abnormally (account %d, status %d): %s, relayed about %d chars", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
		if deltaCharCount > 0 && usage == nil {
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

	usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
	c.Set("x-service-tier", usageTiers.ServiceTier)
	logInput := &database.UsageLogInput{
		AccountID:              account.ID(),
		Endpoint:               "/v1/responses",
		Model:                  model,
		EffectiveModel:         logEffectiveModel,
		StatusCode:             outcome.logStatusCode,
		DurationMs:             totalDuration,
		FirstTokenMs:           firstTokenMs,
		ReasoningEffort:        reasoningEffort,
		InboundEndpoint:        "/v1/responses",
		UpstreamEndpoint:       upstreamEndpoint,
		Stream:                 true,
		ViaWebsocket:           viaWebsocket,
		ServiceTier:            usageTiers.ServiceTier,
		RequestedServiceTier:   usageTiers.RequestedServiceTier,
		ActualServiceTier:      usageTiers.ActualServiceTier,
		BillingServiceTier:     usageTiers.BillingServiceTier,
		PromptPolicyIncidentID: promptPolicyIncidentID,
		AttemptIndex:           fallbackAttempt,
	}
	if outcome.logStatusCode != http.StatusOK {
		logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
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
	// WS turn terminal accounting: the model actually sent by the attempt takes priority (effectiveModel), falling back to the model requested by the client.
	applyUpstreamResponseModelObservation(logInput, responseModelObserver, upstreamSentModelForAudit(effectiveModel, model), account.ID())
	h.logUsageForRequest(c, logInput)

	resp.Body.Close()
	if outcome.penalize {
		recyclePooledClient(account, proxyURL)
		h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
		if !preserveAffinity {
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		}
	} else if outcome.logStatusCode == http.StatusOK {
		h.store.ClearModelCooldown(account, effectiveModel)
		h.store.ConfirmResponsesAvailableSince(account, start)
		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
	}
	if outcome.logStatusCode == http.StatusOK {
		h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
	} else {
		h.store.Release(account)
	}
	if outcome.guardTripped {
		// Guard-hit terminal outcomes all use 1013: consistent with the primitive (error frame + CloseTryAgainLater)
		// and the HTTP/502 semantics, so downstream can recognise this abort as retryable rather than a client violation.
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, outcome.failureMessage, errModelOutputGuardTriggered)
	}
	if outcome.terminalLocal {
		apiErr := api.NewAPIError(api.ErrCodeServerError, continuousRetryLocalFailureMessage, api.ErrorTypeServer)
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseInternalServerErr, apiErr.Message, apiErr)
	}
	if c.Request.Context().Err() != nil {
		return errResponsesWSClientGone
	}

	if errors.Is(writeErr, promptfilter.ErrOutputBlocked) {
		apiErr := api.NewAPIError(api.ErrorCode("response_policy_violation"), "model output violates the safety policy", api.ErrorTypeInvalidRequest)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSErrorForContext(c, conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	if writeErr != nil {
		return errResponsesWSClientGone
	}
	if abortedForErrorClose && !downstreamWrote {
		// When the upstream fails before the first token and nothing has been written to the client: send a structured error frame, then
		// close the connection by error category, to avoid downstream counting a "normally ended session" as success and billing on estimated input tokens.
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		preserveErrorCode := isPreviousResponseNotFoundBody(terminalFailurePayload)
		if preserveErrorCode {
			// This is deterministic continuation state, not an infrastructure error.
			// Preserve the official code so Codex can classify the failed turn even
			// when generic upstream-error details are hidden.
			apiErr = api.NewAPIError(api.ErrorCode("previous_response_not_found"), outcome.failureMessage, api.ErrorTypeInvalidRequest)
		}
		clientErr := apiErr
		if !preserveErrorCode {
			clientErr = responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		}
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSErrorForContext(c, conn, clientErr)
		if preserveErrorCode {
			return nil
		}
		return newResponsesWSCloseError(responsesWSCloseCodeForStatus(outcome.logStatusCode), clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && !hideUpstreamErrors && len(terminalFailureClientPayload) > 0 && !downstreamWrote {
		// An unselected selective-mode failure still ends the logical turn. Its
		// preceding attempt output remains discarded, while the terminal upstream
		// failure is returned without running the local output filter.
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		if err := writeClientMessage(terminalFailureClientPayload); err != nil {
			return errResponsesWSClientGone
		}
		return nil
	}
	if outcome.logStatusCode != http.StatusOK && hideUpstreamErrors && len(terminalFailurePayload) > 0 && !downstreamWrote {
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, true)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSErrorForContext(c, conn, clientErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, clientErr.Message, apiErr)
	}
	if outcome.logStatusCode != http.StatusOK && len(terminalFailurePayload) == 0 {
		errCode := api.ErrCodeUpstreamError
		if outcome.logStatusCode == logStatusUpstreamStreamBreak {
			// A stream break (598) uses the stable error code upstream_stream_break, so downstream can recognise it programmatically and retry
			// (issue #473); other upstream exceptions keep the generic upstream_error.
			errCode = api.ErrorCode(ErrorCodeUpstreamStreamBreak)
		}
		apiErr := api.NewAPIError(errCode, outcome.failureMessage, api.ErrorTypeUpstream)
		clientErr := responsesWSClientUpstreamAPIError(apiErr, hideUpstreamErrors)
		if !claimContinuousRetrySuccessContext(c.Request.Context()) {
			return errResponsesWSClientGone
		}
		_ = writeResponsesWSErrorForContext(c, conn, clientErr)
		return newResponsesWSCloseError(websocket.CloseInternalServerErr, clientErr.Message, apiErr)
	}
	return nil
}

func normalizeResponsesWebSocketClientPayload(raw []byte) ([]byte, string, *api.APIError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "empty websocket request payload", api.ErrorTypeInvalidRequest)
	}
	if len(trimmed) > security.MaxRequestBodySize {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "request body too large", api.ErrorTypeInvalidRequest)
	}
	eventType, envelopeErr := responsesWSClientEventType(trimmed)
	if envelopeErr != nil {
		return nil, "", envelopeErr
	}
	normalized := trimmed
	switch eventType {
	case "":
		eventType = "response.create"
		var err error
		normalized, err = sjson.SetBytes(normalized, "type", eventType)
		if err != nil {
			return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, "invalid websocket request payload", api.ErrorTypeInvalidRequest)
		}
	case "response.create":
	case "response.append":
		return nil, "", api.NewAPIErrorWithDetails(api.ErrCodeUnsupportedFeature, "response.append is not supported; use response.create with previous_response_id", api.ErrorTypeInvalidRequest, map[string]any{
			"event": "response.append",
			"hint":  "send response.create with previous_response_id",
		})
	case "response.steer":
		return nil, "", api.NewAPIErrorWithDetails(api.ErrCodeUnsupportedFeature, "response.steer is not supported by this gateway", api.ErrorTypeInvalidRequest, map[string]any{
			"event": "response.steer",
			"hint":  "wait for the current turn, then send response.create with previous_response_id and the new input",
		})
	default:
		return nil, "", api.NewAPIError(api.ErrCodeInvalidRequest, fmt.Sprintf("unsupported websocket request type: %s", eventType), api.ErrorTypeInvalidRequest)
	}

	model := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if model == "" {
		return nil, "", api.NewAPIError(api.ErrCodeMissingField, "model is required in response.create payload", api.ErrorTypeInvalidRequest)
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(normalized, "previous_response_id").String())
	if strings.HasPrefix(previousResponseID, "msg_") {
		return nil, "", api.NewAPIError(api.ErrCodeInvalidParameter, "previous_response_id must be a response.id (resp_*), not a message id", api.ErrorTypeInvalidRequest)
	}

	return normalized, model, nil
}

func (h *Handler) inspectPromptFilterOpenAIForWebSocket(c *gin.Context, conn *websocket.Conn, rawBody []byte, endpoint string, model string, policyEventID string) (blocked bool, delegatedToNewAPI bool) {
	if h == nil || h.store == nil {
		return false, false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if item, locked := h.activePromptConversationLock(c, cfg, nil, endpoint, model); locked {
		restriction := promptCyberRestrictionDecision(item, cfg)
		profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
		switch profile {
		case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
		default:
			profile = promptfilter.GuardProfileBalanced
		}
		decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: profile, ReasonCode: restriction.ReasonCode, Terminal: true}
		verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: restriction.Message, FullText: restriction.ReasonCode}
		if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil); verified {
			metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, rawBody, endpoint, model, policyEventID, policyContext.VerificationSecret)
			writeNewAPIPolicyDecisionHeaders(c, metadata)
			writePromptCyberRestrictionHeaders(c, restriction)
			_ = writeResponsesWSErrorForContext(c, conn, promptCyberRestrictionAPIError(restriction, newAPIPolicyDecisionDetails(metadata)))
			return true, true
		}
		_ = writeResponsesWSErrorForContext(c, conn, promptCyberRestrictionAPIError(restriction, nil))
		return true, false
	}
	// Keep disabled filters off the WebSocket request-body hot path too.
	if !promptfilter.RequiresRequestText(cfg) {
		return false, false
	}
	evaluation := h.evaluatePromptGuardWithConfig(c, cfg, rawBody, nil, endpoint, model, promptfilter.TransportWebSocket)
	verdict := evaluation.Verdict
	h.logPromptGuardEvaluation(c, endpoint, model, "local_filter", "", evaluation)
	if verdict.Action != promptfilter.ActionBlock {
		return false, false
	}
	// Consistent with the HTTP entry: a local block immediately locks the session, so that equivalent variants that bypass the local regex
	// also cannot reach upstream through this WS session. This WS path holds its own independent block logic, and missing this
	// step would make the whole up-front interception ineffective on Codex's WebSocket channel.
	h.lockPromptConversationOnLocalBlock(c, cfg, nil, endpoint, model, evaluation.Decision, verdict)
	errorCode := api.ErrorCode("prompt_blocked")
	errorMessage := localPromptBlockMessage(cfg)
	if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, nil); verified {
		metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, evaluation.Decision, verdict, cfg, rawBody, endpoint, model, policyEventID, policyContext.VerificationSecret)
		writeNewAPIPolicyDecisionHeaders(c, metadata)
		_ = writeResponsesWSErrorForContext(c, conn, newAPILocalPromptPolicyDecisionAPIError(metadata, cfg))
		return true, true
	}
	_ = writeResponsesWSErrorForContext(c, conn, api.NewAPIError(errorCode, errorMessage, api.ErrorTypeInvalidRequest))
	return true, false
}

func isResponsesWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func writeResponsesWSError(conn *websocket.Conn, apiErr *api.APIError) error {
	return writeResponsesWSErrorWithin(conn, apiErr, responsesWSWriteTimeout)
}

func writeResponsesWSErrorWithin(conn *websocket.Conn, apiErr *api.APIError, timeout time.Duration) error {
	return writeResponsesWSErrorForStream(conn, apiErr, "", timeout)
}

func writeResponsesWSErrorForStream(conn *websocket.Conn, apiErr *api.APIError, streamID string, timeout time.Duration) error {
	if apiErr == nil {
		apiErr = api.NewAPIError(api.ErrCodeServerError, "Internal server error", api.ErrorTypeServer)
	}
	payload, err := json.Marshal(struct {
		Type     string        `json:"type"`
		StreamID string        `json:"stream_id,omitempty"`
		Status   int           `json:"status"`
		Error    *api.APIError `json:"error"`
	}{
		Type:     "error",
		StreamID: streamID,
		Status:   responsesWSErrorStatus(apiErr),
		Error:    apiErr,
	})
	if err != nil {
		return err
	}
	if conn == nil {
		return errResponsesWSClientGone
	}
	return writeResponsesWSPayload(conn, payload, timeout)
}

func responsesWSClientUpstreamAPIError(apiErr *api.APIError, hideUpstreamErrors bool) *api.APIError {
	if !hideUpstreamErrors {
		return apiErr
	}
	return api.NewAPIError(api.ErrCodeUpstreamError, responsesWSFriendlyUpstreamErr, api.ErrorTypeUpstream)
}

func writeResponsesWSMessage(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return errResponsesWSClientGone
	}
	return writeResponsesWSPayload(conn, payload, responsesWSWriteTimeout)
}

func closeResponsesWS(conn *websocket.Conn, code int, reason string) {
	closeResponsesWSWithin(conn, code, reason, responsesWSWriteTimeout)
}

func closeResponsesWSWithin(conn *websocket.Conn, code int, reason string, timeout time.Duration) {
	if conn == nil {
		return
	}
	reason = truncateWebSocketCloseReason(reason)
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(timeout))
}

func truncateWebSocketCloseReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= 120 {
		return reason
	}
	return reason[:120]
}

func newResponsesWSCloseError(code int, reason string, err error) error {
	return &responsesWSCloseError{
		code:   code,
		reason: truncateWebSocketCloseReason(reason),
		err:    err,
	}
}

// responsesWSCloseCodeForStatus maps the HTTP-semantic status code of an upstream failure to a WebSocket close code:
// 429 → 1013 (retry later); other 4xx deterministic client errors → 1008 (policy rejection); 5xx → 1011.
func responsesWSCloseCodeForStatus(statusCode int) int {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return websocket.CloseTryAgainLater
	case statusCode >= 400 && statusCode < 500:
		return websocket.ClosePolicyViolation
	default:
		return websocket.CloseInternalServerErr
	}
}

func responsesWSUpstreamAPIError(statusCode int, body []byte) *api.APIError {
	if isExplicitUpstreamCyberPolicy(body) {
		return api.NewAPIError(api.ErrCodeInvalidRequest, upstreamCyberPolicyUserMessage, api.ErrorTypeInvalidRequest)
	}
	message := usageLogErrorMessage(statusCode, body)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", statusCode)
	}
	errCode := api.ErrCodeUpstreamError
	errType := api.ErrorTypeUpstream
	switch statusCode {
	case http.StatusTooManyRequests:
		errCode = api.ErrCodeRateLimitReached
		errType = api.ErrorTypeRateLimit
	case http.StatusUnauthorized, http.StatusForbidden:
		errCode = api.ErrCodeInvalidAuth
		errType = api.ErrorTypeAuthentication
	case http.StatusBadRequest:
		errCode = api.ErrCodeInvalidRequest
		errType = api.ErrorTypeInvalidRequest
	}
	return api.NewAPIError(errCode, message, errType)
}
