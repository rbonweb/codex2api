package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const liveMaxCreateAttempts = 4

type liveCreateState struct {
	handler    *Handler
	client     *gin.Context
	request    *liveCallRequest
	releaseKey func()
	keyHeld    bool
	apiKey     string
	filter     func(*auth.Account) bool
	excluded   map[int64]bool
	last       liveCreateResult
}

// LiveCreate 选中账号后通过共享服务生成证明，并冻结成功建连的出站结果。
func (h *Handler) LiveCreate(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	request, err := parseLiveCallRequest(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, "") {
		return
	}
	releaseKey, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	state := &liveCreateState{
		handler: h, client: c, request: request, releaseKey: releaseKey,
		apiKey:   strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")),
		filter:   applyAffinityGroupRouting(c, resolveRequestSessionIdentity(c.Request.Header, request.Session), liveAccountFilter),
		excluded: make(map[int64]bool),
	}
	defer state.releaseUnheldKey()
	state.run()
}

func (s *liveCreateState) releaseUnheldKey() {
	if !s.keyHeld && s.releaseKey != nil {
		s.releaseKey()
	}
}

// liveCreateOutcome is what the attempt loop decided, without writing anything to the client.
type liveCreateOutcome struct {
	// account and created are set on success.
	account *auth.Account
	created *liveCallCreated
	// silent means the client went away: nothing is written.
	silent bool
	// localErr is a failure that happened before the upstream answered (headers, attestation, transport).
	localErr error
	// failure is the upstream answer to hand back (status 0 means no answer: service unavailable).
	failure liveCreateResult
	// rejected is true when failure is a client-side upstream rejection (4xx other than 429) that ends the loop.
	rejected bool
}

// attempt runs the account loop and returns the outcome. On failure the account reservation is already released.
func (s *liveCreateState) attempt() liveCreateOutcome {
	for attempt := 0; attempt < liveMaxCreateAttempts; attempt++ {
		account := s.handler.store.NextExcludingWithFilter(requestAPIKeyID(s.client), s.excluded, s.filter)
		if account == nil {
			return liveCreateOutcome{failure: s.last}
		}
		result := s.handler.createUpstreamLiveCall(s.client.Request.Context(), liveCreateInput{
			Account: account, Request: s.request, Downstream: s.client.Request.Header, APIKey: s.apiKey,
		})
		if result.created != nil {
			return liveCreateOutcome{account: account, created: result.created}
		}
		s.excluded[account.ID()] = true
		s.handler.store.Release(account)
		if outcome, done := s.failureOutcome(result); done {
			return outcome
		}
	}
	return liveCreateOutcome{failure: s.last}
}

func (s *liveCreateState) failureOutcome(result liveCreateResult) (liveCreateOutcome, bool) {
	if s.client.Request.Context().Err() != nil {
		return liveCreateOutcome{silent: true}, true
	}
	if result.local {
		return liveCreateOutcome{localErr: result.err}, true
	}
	if result.status >= http.StatusBadRequest && result.status < http.StatusInternalServerError && result.status != http.StatusTooManyRequests {
		return liveCreateOutcome{failure: result, rejected: true}, true
	}
	if result.status != 0 {
		s.last = result
	}
	return liveCreateOutcome{}, false
}

func (s *liveCreateState) run() {
	outcome := s.attempt()
	switch {
	case outcome.created != nil:
		s.save(outcome.account, outcome.created)
	case outcome.silent:
	case outcome.localErr != nil:
		status := StatusCodeFromError(outcome.localErr)
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		api.SendErrorWithStatus(s.client, api.NewAPIError(api.ErrCodeServiceUnavailable, outcome.localErr.Error(), api.ErrorTypeServer), status)
	default:
		s.reply(outcome.failure)
	}
}

func (s *liveCreateState) reply(result liveCreateResult) {
	if result.status == 0 {
		api.SendError(s.client, api.ErrServiceUnavailable)
		return
	}
	contentType := result.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	s.client.Data(result.status, contentType, result.body)
}

func (s *liveCreateState) save(account *auth.Account, created *liveCallCreated) {
	record := s.record(account, created)
	if err := s.handler.liveCalls().save(s.client.Request.Context(), record, account, s.releaseKey); err != nil {
		s.handler.store.Release(account)
		log.Printf("live: save call mapping: %v", err)
		api.SendErrorWithStatus(s.client, api.NewAPIError(api.ErrCodeServiceUnavailable, "Live call mapping unavailable", api.ErrorTypeServer), http.StatusServiceUnavailable)
		return
	}
	s.keyHeld = true
	s.client.Header("Location", liveSidebandLocation(s.client.FullPath(), created.callID))
	s.client.Data(http.StatusOK, "application/sdp", created.sdp)
}

func (s *liveCreateState) record(account *auth.Account, created *liveCallCreated) *liveCallRecord {
	now := time.Now()
	trace := snapshotUpstreamTrace(s.client.Request.Context())
	return &liveCallRecord{
		RequestID: trace.RequestID, UpstreamRequestID: trace.UpstreamRequestID,
		UpstreamProxyID: trace.Proxy.ID, UpstreamProxyName: trace.Proxy.Name,
		CallID: created.callID, CallHash: hashLiveCallID(created.callID),
		AccountID: account.ID(), APIKeyID: requestAPIKeyID(s.client), LeaseID: uuid.NewString(),
		Model: liveSessionModel(s.request.Session), CreatedAt: now, ExpiresAt: now.Add(liveMaxSessionDuration),
		Controller: liveControllerPending, AttestationCiphertext: created.attestationCiphertext,
		OutboundIdentity: created.identity, UserAgent: s.client.GetHeader("User-Agent"),
		ClientIP: s.client.ClientIP(), InboundEndpoint: liveInboundEndpoint(s.client),
	}
}

// gatewayLiveCall is a Live call the gateway opened for itself: Record is the canonical store record (controller proxy; callers read only its immutable identity fields and pass it back to finalize, which stays idempotent), and AnswerSDP is the upstream answer.
type gatewayLiveCall struct {
	Record    *liveCallRecord
	AnswerSDP []byte
}

// liveGatewayError is a failure of createGatewayLiveCall that the caller maps to its own protocol; nothing was written to the client.
type liveGatewayError struct {
	Status int
	Err    *api.APIError
}

func (e *liveGatewayError) Error() string {
	if e == nil || e.Err == nil {
		return "live gateway error"
	}
	return e.Err.Message
}

func newLiveGatewayError(status int, code api.ErrorCode, errType api.ErrorType, msg string) *liveGatewayError {
	return &liveGatewayError{Status: status, Err: api.NewAPIError(code, msg, errType)}
}

const liveGatewayMessageLimit = 300

// liveGatewayLimitError maps an API key limit refusal to an error the way SendAPIKeyLimitError does.
func liveGatewayLimitError(status int, msg string) *liveGatewayError {
	errType, errCode := api.ErrorTypeRateLimit, api.ErrCodeRateLimitReached
	switch status {
	case http.StatusForbidden:
		errType, errCode = api.ErrorTypePermission, api.ErrCodeInvalidRequest
	case http.StatusBadRequest:
		errType, errCode = api.ErrorTypeInvalidRequest, api.ErrCodeInvalidParameter
	case http.StatusServiceUnavailable:
		errType, errCode = api.ErrorTypeServer, api.ErrCodeServiceUnavailable
	}
	return newLiveGatewayError(status, errCode, errType, msg)
}

// liveGatewayUpstreamMessage extracts a short, printable message from an upstream rejection without echoing the raw body.
func liveGatewayUpstreamMessage(status int, body []byte) string {
	fallback := fmt.Sprintf("ChatGPT Live rejected the call (status %d)", status)
	var parsed struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return fallback
	}
	msg := parsed.Message
	if msg == "" {
		msg = parsed.Detail
	}
	if len(parsed.Error) > 0 {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(parsed.Error, &nested) == nil && nested.Message != "" {
			msg = nested.Message
		}
	}
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return fallback
	}
	if runes := []rune(msg); len(runes) > liveGatewayMessageLimit {
		msg = string(runes[:liveGatewayMessageLimit])
	}
	return msg
}

func (s *liveCreateState) gatewayError(outcome liveCreateOutcome) *liveGatewayError {
	switch {
	case outcome.silent:
		return newLiveGatewayError(http.StatusServiceUnavailable, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "Request canceled")
	case outcome.localErr != nil:
		status := StatusCodeFromError(outcome.localErr)
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		msg := outcome.localErr.Error()
		if runes := []rune(msg); len(runes) > liveGatewayMessageLimit {
			msg = string(runes[:liveGatewayMessageLimit])
		}
		return newLiveGatewayError(status, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, msg)
	case outcome.rejected:
		return newLiveGatewayError(outcome.failure.status, api.ErrCodeInvalidRequest, api.ErrorTypeInvalidRequest,
			liveGatewayUpstreamMessage(outcome.failure.status, outcome.failure.body))
	}
	return newLiveGatewayError(http.StatusServiceUnavailable, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "Service temporarily unavailable")
}

// createGatewayLiveCall opens a Live call for the gateway itself. It runs the same account selection, retries, attestation, identity, lease, store save and usage logging as LiveCreate but never writes to c, so it is safe after a WebSocket upgrade.
// The call is claimed as proxy-controlled so no external sideband can attach. The caller releases everything with h.liveCalls().finalize(record).
func (h *Handler) createGatewayLiveCall(c *gin.Context, request *liveCallRequest) (*gatewayLiveCall, *liveGatewayError) {
	return h.createLiveCallWithoutReply(c, request, true)
}

// createLiveCallWithoutReply is createGatewayLiveCall with a choice: with claim false the saved call stays pending, so a client that owns the media can attach a sideband to it later (POST /v1/live/sessions).
func (h *Handler) createLiveCallWithoutReply(c *gin.Context, request *liveCallRequest, claim bool) (*gatewayLiveCall, *liveGatewayError) {
	row := apiKeyRowFromContext(c)
	if row == nil || !row.Limits.AllowLive {
		return nil, newLiveGatewayError(http.StatusForbidden, api.ErrCodeInsufficientScope, api.ErrorTypePermission, "ChatGPT Live is not enabled for this API key")
	}
	if err := ValidateLiveCallRequest(request); err != nil {
		return nil, newLiveGatewayError(http.StatusBadRequest, api.ErrCodeInvalidRequest, api.ErrorTypeInvalidRequest, err.Error())
	}
	if status, msg := h.enforceAPIKeyLimits(c, ""); status != 0 {
		return nil, liveGatewayLimitError(status, msg)
	}
	releaseKey, apiErr, ok := h.acquireAPIKeyConcurrencyForWebSocket(c)
	if !ok {
		return nil, &liveGatewayError{Status: http.StatusTooManyRequests, Err: apiErr}
	}
	state := &liveCreateState{
		handler: h, client: c, request: request, releaseKey: releaseKey,
		apiKey:   strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")),
		filter:   applyAffinityGroupRouting(c, resolveRequestSessionIdentity(c.Request.Header, request.Session), liveAccountFilter),
		excluded: make(map[int64]bool),
	}
	defer state.releaseUnheldKey()
	outcome := state.attempt()
	if outcome.created == nil {
		return nil, state.gatewayError(outcome)
	}
	ctx := c.Request.Context()
	record := state.record(outcome.account, outcome.created)
	if err := h.liveCalls().save(ctx, record, outcome.account, releaseKey); err != nil {
		h.store.Release(outcome.account)
		log.Printf("live: save call mapping: %v", err)
		return nil, newLiveGatewayError(http.StatusServiceUnavailable, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "Live call mapping unavailable")
	}
	state.keyHeld = true
	if claim {
		if _, claimed, err := h.liveCalls().claimProxy(ctx, record); err != nil || !claimed {
			h.liveCalls().finalize(record)
			return nil, newLiveGatewayError(http.StatusServiceUnavailable, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "Live call unavailable")
		}
	}
	return &gatewayLiveCall{Record: record, AnswerSDP: outcome.created.sdp}, nil
}
