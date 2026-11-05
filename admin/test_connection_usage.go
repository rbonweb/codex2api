package admin

import (
	"context"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Account probe requests that the gateway itself sends are also written to usage records, with a distinct internal reason, so the usage page
// can show them separately from downstream traffic: connection tests (connection_test) and intelligence checks (quality_test).
// The record is written at the end of the probe and shares the same observed values as the diagnostic frame (duration, final usage, upstream identifiers).
const (
	internalReasonConnectionTest = "connection_test"
	internalReasonQualityTest    = "quality_test"

	// Uses the same internal status code as the gateway's in-stream disconnect accounting: the request failed before any upstream response was received.
	connectionTestTransportStatus = 598

	contextConnectionTestLastError = "connection_test_last_error"
)

// connectionTestUsageInput holds the accounting fields extracted from the diagnostic object when the probe finishes.
type connectionTestUsageInput struct {
	Reason            string
	Endpoint          string
	Model             string
	EffectiveModel    string
	ReasoningEffort   string
	StatusCode        int
	DurationMs        int
	FirstTokenMs      int
	InputTokens       int
	OutputTokens      int
	CachedTokens      int
	ReasoningTokens   int
	CacheWrite5m      int
	CacheWrite1h      int
	ViaWebsocket      bool
	UpstreamRequestID string
	ErrorMessage      string
	UpstreamErrorKind string
}

func connectionTestReason(quality *qualityTestRequest) string {
	if quality != nil {
		return internalReasonQualityTest
	}
	return internalReasonConnectionTest
}

// connectionTestReasoningEffort reads the reasoning level from the probe request body: in the Responses shape it is
// reasoning.effort.
func connectionTestReasoningEffort(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "reasoning.effort").String())
}

func connectionTestEndpoint(account *auth.Account) string {
	return "/v1/responses"
}

// rememberConnectionTestError records the last error message pushed to the client, so that at accounting time it is stored as
// error_message, so the usage page's "errors only" filter also shows failed probes.
func rememberConnectionTestError(c *gin.Context, event testEvent) {
	if c == nil || event.Type != "error" {
		return
	}
	if msg := strings.TrimSpace(event.Error); msg != "" {
		c.Set(contextConnectionTestLastError, msg)
	}
}

func connectionTestLastError(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get(contextConnectionTestLastError); exists {
		msg, _ := value.(string)
		return strings.TrimSpace(msg)
	}
	return ""
}

func int64PtrValue(v *int64) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

// connectionTestUsageFromCodex projects the Codex/Responses probe diagnostics into accounting fields. The usage in the diagnostics is the full final value, so it is used directly.
func connectionTestUsageFromCodex(d *codexTestDiagnostics, model string) connectionTestUsageInput {
	in := connectionTestUsageInput{Model: model, EffectiveModel: model}
	if d == nil {
		return in
	}
	in.StatusCode = d.HTTPStatus
	in.DurationMs = int64PtrValue(d.DurationMS)
	in.FirstTokenMs = int64PtrValue(d.FirstContentMS)
	if in.FirstTokenMs == 0 {
		in.FirstTokenMs = int64PtrValue(d.FirstFrameMS)
	}
	if d.ResponseModel != "" {
		in.EffectiveModel = d.ResponseModel
	}
	in.ViaWebsocket = d.Transport == "websocket"
	in.UpstreamRequestID = d.RequestID
	if d.Usage != nil {
		in.InputTokens = int64PtrValue(d.Usage.InputTokens)
		in.OutputTokens = int64PtrValue(d.Usage.OutputTokens)
		in.CachedTokens = int64PtrValue(d.Usage.CachedTokens)
		in.ReasoningTokens = int64PtrValue(d.Usage.ReasoningTokens)
	}
	return in
}

// logConnectionTestUsage writes one probe into usage_logs. It does not go through the downstream API key accounting path:
// the probe has no downstream identity and is attributed only to the account and channel; the cost is calculated by the write layer from model pricing.
func (h *Handler) logConnectionTestUsage(c *gin.Context, account *auth.Account, in connectionTestUsageInput) {
	if h == nil || h.db == nil || account == nil {
		return
	}
	if in.ErrorMessage == "" {
		in.ErrorMessage = connectionTestLastError(c)
	}
	if in.ErrorMessage != "" && in.UpstreamErrorKind == "" && in.StatusCode >= 200 && in.StatusCode < 300 {
		// HTTP 200 but failed inside the stream (response.failed / error event / no output) is classified as an in-stream error.
		in.UpstreamErrorKind = "stream_error"
	}
	if in.StatusCode == 0 {
		in.StatusCode = connectionTestTransportStatus
	}
	if in.DurationMs < 0 {
		in.DurationMs = 0
	}
	input := &database.UsageLogInput{
		AccountID:            account.ID(),
		CredentialGeneration: account.GetCredentialGeneration(),
		Channel:              database.UpstreamChannelCodex,
		InternalReason:       in.Reason,
		Endpoint:             in.Endpoint,
		InboundEndpoint:      in.Endpoint,
		UpstreamEndpoint:     in.Endpoint,
		UpstreamRequestID:    in.UpstreamRequestID,
		Model:                in.Model,
		EffectiveModel:       in.EffectiveModel,
		ReasoningEffort:      in.ReasoningEffort,
		StatusCode:           in.StatusCode,
		DurationMs:           in.DurationMs,
		FirstTokenMs:         in.FirstTokenMs,
		InputTokens:          in.InputTokens,
		OutputTokens:         in.OutputTokens,
		PromptTokens:         in.InputTokens,
		CompletionTokens:     in.OutputTokens,
		TotalTokens:          in.InputTokens + in.OutputTokens,
		CachedTokens:         in.CachedTokens,
		ReasoningTokens:      in.ReasoningTokens,
		CacheWrite5mTokens:   in.CacheWrite5m,
		CacheWrite1hTokens:   in.CacheWrite1h,
		Stream:               true,
		ViaWebsocket:         in.ViaWebsocket,
		ErrorMessage:         in.ErrorMessage,
		UpstreamErrorKind:    in.UpstreamErrorKind,
	}
	if c != nil {
		input.ClientIP = strings.TrimSpace(c.ClientIP())
		if c.Request != nil {
			input.ClientUserAgent = strings.TrimSpace(c.Request.UserAgent())
		}
	}
	_ = h.db.InsertUsageLog(context.Background(), input)
}

// logConnectionTestTransportFailure records a probe that fails before connecting upstream (dialing, proxy, TLS, etc.).
func (h *Handler) logConnectionTestTransportFailure(c *gin.Context, account *auth.Account, reason, endpoint, model, effort string, start time.Time, err error) {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	h.logConnectionTestUsage(c, account, connectionTestUsageInput{
		Reason:            reason,
		Endpoint:          endpoint,
		Model:             model,
		EffectiveModel:    model,
		ReasoningEffort:   effort,
		StatusCode:        connectionTestTransportStatus,
		DurationMs:        int(max(int64(0), time.Since(start).Milliseconds())),
		ErrorMessage:      msg,
		UpstreamErrorKind: "transport",
	})
}
