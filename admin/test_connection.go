package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var batchTestAccountTimeout = 30 * time.Second
var batchTestWhamTimeout = 5 * time.Second

// testEvent is an SSE test event
type testEvent struct {
	Type    string `json:"type"`              // test_start | content | diagnostics | test_complete | error
	Text    string `json:"text,omitempty"`    // content text
	Model   string `json:"model,omitempty"`   // test model
	Success bool   `json:"success,omitempty"` // whether it succeeded
	Error   string `json:"error,omitempty"`   // error message
	// Diagnostics object for the Codex/Responses connection test.
	CodexDiagnostics *codexTestDiagnostics `json:"codex_diagnostics,omitempty"`
	// Interrupted marks that the error event is a transport or stream interruption (connection failure, stream read failure, or disconnect without a terminal state),
	// rather than an explicit upstream rejection; the intelligence check uses it to decide whether to retry automatically and whether to show "interrupted".
	Interrupted bool `json:"interrupted,omitempty"`
}

type responsesTerminalOutcome uint8

const (
	responsesTerminalUnknown responsesTerminalOutcome = iota
	responsesTerminalSuccess
	responsesTerminalFailed
	responsesTerminalUsageLimited
)

func classifyResponsesTerminalEvent(data []byte) responsesTerminalOutcome {
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "response.completed":
		status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
		if status == "failed" || status == "incomplete" {
			if proxy.IsUsageLimitReachedError(data) {
				return responsesTerminalUsageLimited
			}
			return responsesTerminalFailed
		}
		return responsesTerminalSuccess
	case "response.failed", "error":
		if proxy.IsUsageLimitReachedError(data) {
			return responsesTerminalUsageLimited
		}
		return responsesTerminalFailed
	default:
		return responsesTerminalUnknown
	}
}

func (h *Handler) applyResponsesUsageLimitFailure(account *auth.Account, resp *http.Response, model string, payload []byte) bool {
	if h == nil || h.store == nil || account == nil || !proxy.IsUsageLimitReachedError(payload) {
		return false
	}
	proxy.Apply429Cooldown(h.store, account, payload, resp, model)
	return true
}

// TestConnection tests the account connection (streamed back over SSE)
// GET /api/admin/accounts/:id/test
func (h *Handler) TestConnection(c *gin.Context) {
	h.testConnection(c, nil)
}

func (h *Handler) testConnection(c *gin.Context, quality *qualityTestRequest) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account ID"})
		return
	}

	// Look up the runtime account; accounts in the recycle bin are not in the runtime pool, so build a temporary account to test connectivity
	// (it does not take part in scheduling, and the test result is not written back to the account state).
	account := h.store.FindByID(id)
	isTransient := false
	if account == nil {
		if quality != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "account is not in the runtime pool"})
			return
		}
		transient, buildErr := h.store.BuildTransientAccountByID(c.Request.Context(), id)
		if buildErr != nil || transient == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "account is not in the runtime pool"})
			return
		}
		account = transient
		isTransient = true
	}
	// A connection test is an SSE GET, yet it writes unauthorized, error, rate-limited or recovered states. Wait until the stream ends
	// before invalidating the list and analytics snapshots, so the accounts page does not keep showing an account already judged 401 as "unsampled".
	if !isTransient {
		defer h.invalidateAccountSnapshotCaches()
	}

	// Agent Identity has no AT and signs dynamically with its private key, so the AT preflight is skipped (the request is signed dynamically by the Codex executor).
	if !account.IsCodexAgentIdentity() && account.GetAccessToken() == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account has no usable Access Token, please refresh it first"})
		return
	}

	requestedModel := strings.TrimSpace(c.Query("model"))
	if quality != nil {
		requestedModel = quality.Model
		if err := h.validateQualityTestForAccount(c.Request.Context(), account, *quality); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	testModel, err := h.connectionTestModelForAccount(c.Request.Context(), account, requestedModel)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	prompt := c.Query("prompt")
	hasPrompt := quality == nil && c.Request.URL.Query().Has("prompt")
	if hasPrompt && (strings.TrimSpace(prompt) == "" || len([]rune(prompt)) > auth.MaxTestContentRunes) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("test content cannot be empty and cannot exceed %d characters", auth.MaxTestContentRunes)})
		return
	}
	payload := h.buildAccountConnectionTestPayload(c.Request.Context(), account, testModel)
	if quality != nil {
		payload, err = buildQualityTestPayload(account, testModel, *quality)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else if hasPrompt {
		payload = buildTestPayloadWithContent(testModel, prompt)
	}

	// The probe itself is also written to usage records: connection tests and intelligence checks each have their own internal reason, and the usage page tags them accordingly.
	usageReason := connectionTestReason(quality)
	if c.GetBool(scheduledTestContextKey) {
		usageReason = "scheduled_connection_test"
		// Codex Responses models do not all expose the same reasoning tiers.
		// `none` is the lowest-cost tier supported by gpt-6-luna and avoids
		// sending the unsupported `minimal` value to the upstream API.
		payload, _ = sjson.SetBytes(payload, "reasoning.effort", "none")
	}
	usageEndpoint := connectionTestEndpoint(account)
	usageEffort := connectionTestReasoningEffort(payload)

	// Set the SSE response headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Send test_start
	sendTestEvent(c, testEvent{Type: "test_start", Model: testModel})

	// Recycle bin account: record the latest test result; when restore_on_success=true, a passing test restores the account automatically.
	restoreOnSuccess := isTransient && strings.EqualFold(strings.TrimSpace(c.Query("restore_on_success")), "true")
	transientOutcome := "failed"
	if isTransient {
		defer func() {
			h.persistRecycleBinTestResult(id, transientOutcome)
		}()
	}

	// Build the minimal test request body (modeled on sub2api createOpenAITestPayload)
	// Send the request
	start := time.Now()
	var resp *http.Response
	var reqErr error
	if quality != nil || c.GetBool(scheduledTestContextKey) {
		// Intelligence checks and scheduled activations both use a standalone HTTP SSE stream and do not depend on WS continuation or reconnection;
		// they are not affected by "force WebSocket" and neither occupy nor depend on a pooled long-lived connection.
		resp, reqErr = proxy.ExecuteRequest(c.Request.Context(), account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil, false)
	} else {
		resp, reqErr = proxy.ExecuteRequest(c.Request.Context(), account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	}
	if reqErr != nil {
		h.logConnectionTestTransportFailure(c, account, usageReason, usageEndpoint, testModel, usageEffort, start, reqErr)
		event := testEvent{Type: "error", Error: fmt.Sprintf("request failed: %s", reqErr.Error()), Interrupted: true}
		failed := newCodexTestRecorder(nil, testModel, account, start)
		event.CodexDiagnostics = failed.finish()
		event.Error = sanitizeCodexTestText(event.Error, failed.secrets)
		sendTestEvent(c, event)
		return
	}
	defer resp.Body.Close()
	// Codex/Responses connection test diagnostics: as soon as the response headers arrive, push one frame (status code, usage window headers), and after the stream ends,
	// push a final frame (duration, terminal state, usage, body preview). The final frame comes after the terminal event, so the client must read until
	// the SSE closes before the account snapshot is refreshed.
	recorder := newCodexTestRecorder(resp, testModel, account, start)
	defer func() {
		diagnostics := recorder.finish()
		sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: diagnostics})
		usage := connectionTestUsageFromCodex(diagnostics, testModel)
		usage.Reason, usage.Endpoint, usage.ReasoningEffort = usageReason, usageEndpoint, usageEffort
		h.logConnectionTestUsage(c, account, usage)
	}()
	sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: recorder.details})

	if resp.StatusCode != http.StatusOK {
		if !isTransient {
			proxy.SyncCodexUsageState(h.store, account, resp)
		}
		errBody, _ := io.ReadAll(resp.Body)
		recorder.observe(errBody)
		errMsg := fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, truncate(string(errBody), 500))
		if !isTransient {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", errMsg)
			case http.StatusPaymentRequired:
				// A 402 from a connection test is an account-side billing or workspace rejection; mark it as an error so it does not keep showing as "unsampled".
				if proxy.IsDeactivatedWorkspaceError(errBody) {
					h.store.MarkDeactivatedWorkspace(account, errMsg)
				} else {
					h.store.MarkError(account, errMsg)
				}
			case http.StatusForbidden:
				if proxy.IsAgentRuntimeDeletedError(errBody) {
					h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errMsg)
				} else if proxy.IsDeactivatedWorkspaceError(errBody) {
					h.store.MarkDeactivatedWorkspace(account, errMsg)
				}
			case http.StatusTooManyRequests:
				proxy.Apply429Cooldown(h.store, account, errBody, resp, testModel)
			}
		}
		// 429 rate limiting means the account is valid and only rate limited, so it is not counted as a failure.
		if isTransient && resp.StatusCode == http.StatusTooManyRequests {
			transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: errMsg})
		return
	}

	var usageState proxy.CodexUsageSyncResult
	usageStore := h.store
	if isTransient {
		usageStore = nil // The temporary account only reads usage headers for display and does not write them to storage
	}
	usageState = proxy.SyncCodexUsageState(usageStore, account, resp)
	if !isTransient {
		applyUsageLimitedTestState(h.store, account, usageState)
	}
	if msg, limited := formatUsageLimitedTestError(usageState); limited {
		// Usage exhaustion counts as a rate-limit type and is not counted as a failure.
		if isTransient {
			transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: msg})
		return
	}

	// Parse the SSE stream
	hasContent := false
	gotTerminal := false
	sentTerminal := false
	var lastUpstreamEvent []byte
	emitContent := func(text string) {
		hasContent = true
		recorder.contentReceived()
		sendTestEvent(c, testEvent{Type: "content", Text: text})
	}
	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		recorder.observe(data)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			if delta != "" {
				emitContent(delta)
			}
		case "response.output_text.done":
			if !hasContent {
				text := gjson.GetBytes(data, "text").String()
				if text != "" {
					emitContent(text)
				}
			}
		case "response.content_part.done":
			if !hasContent {
				text := gjson.GetBytes(data, "part.text").String()
				if text != "" {
					emitContent(text)
				}
			}
		case "response.output_item.done":
			if !hasContent {
				text := extractOutputItemText(gjson.GetBytes(data, "item"))
				if text != "" {
					emitContent(text)
				}
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				sentTerminal = true
				if !isTransient {
					h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
				}
				if isTransient && proxy.IsUsageLimitReachedError(data) {
					transientOutcome = "rate_limited"
				}
				sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "upstream returned "+status)})
				return false
			}
			if !hasContent {
				text := extractCompletedOutputText(data)
				if text != "" {
					emitContent(text)
				}
			}
			if !hasContent {
				sentTerminal = true
				sendTestEvent(c, testEvent{Type: "error", Error: formatNoOutputUpstreamError(data)})
				return false
			}
			// A successful test resets the failure/cooldown state; usage limits are judged by the scheduler itself;
			// temporary accounts (recycle bin) do not write back any scheduling state.
			if !isTransient && (usageState.UsageWindowLimitsIgnored || (!usageState.Premium5hRateLimited && (!usageState.HasUsage7d || usageState.UsagePct7d < 100))) {
				h.store.RecordManualTestSuccess(account, time.Since(start))
			}
			if isTransient {
				transientOutcome = "success"
				if restoreOnSuccess {
					restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
					restoreErr := h.restoreAccountByID(restoreCtx, id)
					restoreCancel()
					if restoreErr != nil {
						sendTestEvent(c, testEvent{Type: "content", Text: "\n\n--- auto-restore failed: " + restoreErr.Error() + " ---"})
					}
				}
			}
			if quality == nil {
				duration := time.Since(start).Milliseconds()
				sendTestEvent(c, testEvent{
					Type: "content",
					Text: fmt.Sprintf("\n\n--- took %dms ---", duration),
				})
			}
			sendTestEvent(c, testEvent{Type: "test_complete", Success: true})
			sentTerminal = true
			return false
		case "response.failed":
			gotTerminal = true
			sentTerminal = true
			if !isTransient {
				h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
			}
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "upstream returned response.failed")})
			return false
		case "error":
			gotTerminal = true
			sentTerminal = true
			if !isTransient {
				h.applyResponsesUsageLimitFailure(account, resp, testModel, data)
			}
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "upstream returned an error event")})
			return false
		}
		return true
	})

	if readErr != nil && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: "failed to read upstream stream: " + readErr.Error(), Interrupted: true})
		return
	}
	if !gotTerminal && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: formatMissingTerminalUpstreamError(lastUpstreamEvent), Interrupted: true})
	}
}

func buildConnectionTestPayload(store *auth.Store, model string) []byte {
	content := auth.DefaultTestContent
	if store != nil {
		content = store.GetTestContent()
	}
	// Multi-line content is picked randomly line by line and variables are expanded (issue #320), so batch accounts are less likely to
	// share the same fingerprint from one identical probe sentence. Single-line configuration behaves as before.
	return buildTestPayloadWithContent(model, auth.RenderTestContent(content))
}

// connectionTestContentForAccount returns the user input a connection test should send (after multi-line picking and variable expansion), taken from the global probe content.
func (h *Handler) connectionTestContentForAccount(ctx context.Context, account *auth.Account) string {
	content := auth.DefaultTestContent
	if h != nil && h.store != nil {
		content = h.store.GetTestContent()
	}
	return auth.RenderTestContent(content)
}

// buildAccountConnectionTestPayload builds a Responses-shaped connection test request body; the user input is taken from the global probe content.
func (h *Handler) buildAccountConnectionTestPayload(ctx context.Context, account *auth.Account, model string) []byte {
	content := h.connectionTestContentForAccount(ctx, account)
	return buildTestPayloadWithContent(model, content)
}

// buildTestPayload builds the default minimal test request body
func buildTestPayload(model string) []byte {
	return buildTestPayloadWithContent(model, auth.DefaultTestContent)
}

// buildTestPayloadWithContent builds a minimal test request body with custom user input content
func buildTestPayloadWithContent(model string, content string) []byte {
	content = auth.NormalizeTestContent(content)
	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "model", model)
	payload, _ = sjson.SetBytes(payload, "input", []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": content,
				},
			},
		},
	})
	payload, _ = sjson.SetBytes(payload, "stream", true)
	payload, _ = sjson.SetBytes(payload, "store", false)
	payload, _ = sjson.SetBytes(payload, "instructions", "You are a helpful assistant. Reply briefly.")
	return payload
}

func formatUsageLimitedTestError(state proxy.CodexUsageSyncResult) (string, bool) {
	if state.UsageWindowLimitsIgnored {
		return "", false
	}
	if state.Premium5hRateLimited {
		remaining := time.Until(state.Reset5hAt).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("upstream probe returned 200, but the Codex 5h usage header has reached %.0f%%, so the account stays in a rate-limited state and is expected to recover after %s.", state.UsagePct5h, remaining), true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return fmt.Sprintf("upstream probe returned 200, but the Codex 7d usage header has reached %.0f%%, so the account is marked as rate-limited or usage exhausted.", state.UsagePct7d), true
	}
	return "", false
}

func applyUsageLimitedAccountState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) bool {
	if store == nil || account == nil {
		return false
	}
	if state.UsageWindowLimitsIgnored {
		return false
	}
	if state.Premium5hRateLimited || state.Usage7dRateLimited {
		return true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return store.MarkUsage7dRateLimited(account)
	}
	return false
}

func applyUsageLimitedTestState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) {
	applyUsageLimitedAccountState(store, account, state)
}

// sendTestEvent sends an SSE event
func sendTestEvent(c *gin.Context, event testEvent) {
	rememberConnectionTestError(c, event)
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("failed to serialize test event: %v", err)
		return
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		log.Printf("failed to write SSE event: %v", err)
		return
	}
	c.Writer.Flush()
}

// truncate cuts a string
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractCompletedOutputText(data []byte) string {
	if text := gjson.GetBytes(data, "response.output_text").String(); text != "" {
		return text
	}
	return extractOutputItemText(gjson.GetBytes(data, "response"))
}

func extractOutputItemText(item gjson.Result) string {
	var b strings.Builder
	writeTextFromOutputItem(&b, item)
	return b.String()
}

func writeTextFromOutputItem(b *strings.Builder, item gjson.Result) {
	if !item.Exists() {
		return
	}
	switch item.Get("type").String() {
	case "output_text", "text":
		b.WriteString(item.Get("text").String())
	case "message", "assistant":
		writeTextFromContentArray(b, item.Get("content"))
	default:
		if output := item.Get("output"); output.IsArray() {
			output.ForEach(func(_, child gjson.Result) bool {
				writeTextFromOutputItem(b, child)
				return true
			})
		}
		writeTextFromContentArray(b, item.Get("content"))
	}
}

func writeTextFromContentArray(b *strings.Builder, content gjson.Result) {
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "output_text" || partType == "text" {
			b.WriteString(part.Get("text").String())
		}
		return true
	})
}

func formatUpstreamTestError(data []byte, fallback string) string {
	msg := firstNonEmptyGJSONString(data,
		"response.status_details.error.message",
		"response.error.message",
		"error.message",
		"message",
		"response.incomplete_details.reason",
		"response.status_details.message",
	)
	if msg == "" {
		msg = fallback
	}

	code := firstNonEmptyGJSONString(data,
		"response.status_details.error.code",
		"response.error.code",
		"error.code",
	)
	if code != "" && !strings.Contains(msg, code) {
		msg += " (code: " + code + ")"
	}

	return formatUpstreamEventDetail(msg, data)
}

func formatNoOutputUpstreamError(data []byte) string {
	msg := "upstream completed but returned no text output"
	if status := gjson.GetBytes(data, "response.status").String(); status != "" && status != "completed" {
		msg = "upstream response status: " + status
	}
	if reason := gjson.GetBytes(data, "response.incomplete_details.reason").String(); reason != "" {
		msg += " (" + reason + ")"
	}
	return formatUpstreamEventDetail(msg, data)
}

func formatMissingTerminalUpstreamError(lastEvent []byte) string {
	if len(lastEvent) == 0 {
		return "upstream stream ended without receiving any event"
	}
	return formatUpstreamEventDetail("upstream stream ended early without receiving response.completed or response.failed", lastEvent)
}

func firstNonEmptyGJSONString(data []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func formatUpstreamEventDetail(message string, data []byte) string {
	if len(data) == 0 {
		return message
	}
	detail := string(data)
	var parsed any
	if err := json.Unmarshal(data, &parsed); err == nil {
		if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
			detail = string(pretty)
		}
	}
	return message + "\n\nUpstream events:\n" + truncate(detail, 3000)
}

func isSupportedConnectionTestModel(model string) bool {
	if strings.Contains(strings.ToLower(model), "image") {
		return false
	}
	for _, supported := range proxy.SupportedModels {
		if model == supported {
			return true
		}
	}
	return false
}

func (h *Handler) connectionTestModel(ctx context.Context) string {
	model := strings.TrimSpace(h.store.GetTestModel())
	if proxy.IsTextTestModelID(ctx, h.db, model) {
		return model
	}
	models := proxy.TextTestModelIDs(ctx, h.db)
	if len(models) > 0 {
		return models[0]
	}
	return auth.DefaultTestModel
}

func (h *Handler) connectionTestModelForAccount(ctx context.Context, account *auth.Account, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return h.connectionTestModel(ctx), nil
	}
	if !proxy.IsTextTestModelID(ctx, h.db, requested) {
		return "", fmt.Errorf("unsupported test model: %s", requested)
	}
	return requested, nil
}

func isTextConnectionModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	return model != "" && !strings.Contains(model, "image")
}

type batchTestRequest struct {
	IDs      *[]int64                  `json:"ids"`
	Selector *accountOperationSelector `json:"selector,omitempty"`
	// RestoreOnSuccess is used only by recycle bin batch tests: accounts that pass are automatically restored to the account pool.
	RestoreOnSuccess bool `json:"restore_on_success"`
}

// persistRecycleBinTestResult writes the recycle bin test result into the account credentials for list display.
func (h *Handler) persistRecycleBinTestResult(id int64, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{
		"recycle_last_test_status": status,
		"recycle_last_test_at":     time.Now().Format(time.RFC3339),
	}); err != nil {
		log.Printf("failed to write recycle bin test result (account %d): %v", id, err)
	}
}

type batchOperationEvent struct {
	Type         string `json:"type"` // start | progress | complete
	Action       string `json:"action"`
	Status       string `json:"status,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	Current      int    `json:"current"`
	Total        int    `json:"total"`
	Success      int64  `json:"success"`
	Failed       int64  `json:"failed"`
	Banned       int64  `json:"banned,omitempty"`
	RateLimited  int64  `json:"rate_limited,omitempty"`
	Deleted      int64  `json:"deleted,omitempty"`
	AccountID    int64  `json:"account_id,omitempty"`
	AccountName  string `json:"account_name,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
}

func runtimeAccountOperationIdentity(account *auth.Account) (string, string) {
	if account == nil {
		return "", ""
	}
	account.Mu().RLock()
	email := strings.TrimSpace(account.Email)
	account.Mu().RUnlock()
	return "", email
}

func batchOperationHTTPStatus(status, message string) int {
	if status == "success" {
		return http.StatusOK
	}

	normalized := strings.ToLower(message)
	for _, marker := range []string{
		"upstream returned ",
		"http ",
		"status code ",
		"status ",
		"status=",
		"status:",
		"状态码 ",
	} {
		index := strings.Index(normalized, marker)
		if index < 0 {
			continue
		}
		remainder := strings.TrimLeft(normalized[index+len(marker):], " :=-")
		if len(remainder) < 3 {
			continue
		}
		code, err := strconv.Atoi(remainder[:3])
		if err == nil && code >= 100 && code <= 599 {
			return code
		}
	}
	return 0
}

type batchTestCounts struct {
	Total       int
	Success     int64
	Failed      int64
	Banned      int64
	RateLimited int64
}

func resolveBatchTestAccounts(store *auth.Store, ids *[]int64) ([]*auth.Account, int) {
	if store == nil {
		return nil, 0
	}
	if ids == nil {
		return store.Accounts(), 0
	}

	accounts := make([]*auth.Account, 0, len(*ids))
	missing := 0
	seen := make(map[int64]struct{}, len(*ids))
	for _, id := range *ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		acc := store.FindByID(id)
		if acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}
	return accounts, missing
}

// BatchTest tests account connections in batch; without ids it tests all accounts, with ids it tests only the specified accounts.
// POST /api/admin/accounts/batch-test
func (h *Handler) BatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request format")
			return
		}
	}
	if req.IDs != nil && req.Selector != nil {
		writeError(c, http.StatusBadRequest, "ids and selector cannot be provided together")
		return
	}
	if req.Selector != nil {
		selectorCtx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		ids, err := h.resolveAccountOperationSelector(selectorCtx, req.Selector)
		cancel()
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		req.IDs = &ids
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "provide the list of account IDs to test")
		return
	}

	accounts, missingCount := resolveBatchTestAccounts(h.store, req.IDs)
	h.serveBatchTest(c, accounts, missingCount, h.runSingleBatchTest)
}

// RecycleBinBatchTest tests recycle bin account connections in batch; without ids it tests all accounts in the recycle bin.
// Accounts are built as temporary objects, do not take part in scheduling, and test results are never written back to any account state.
// POST /api/admin/accounts/recycle-bin/batch-test
func (h *Handler) RecycleBinBatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request format")
			return
		}
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "provide the list of account IDs to test")
		return
	}
	if req.Selector != nil {
		writeError(c, http.StatusBadRequest, "recycle bin batch test does not support selector")
		return
	}

	listCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	rows, err := h.db.ListDeleted(listCtx)
	cancel()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	binIDs := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		binIDs[row.ID] = struct{}{}
	}

	wanted := make([]int64, 0, len(rows))
	missing := 0
	if req.IDs == nil {
		for _, row := range rows {
			wanted = append(wanted, row.ID)
		}
	} else {
		seen := make(map[int64]struct{}, len(*req.IDs))
		for _, id := range *req.IDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if _, ok := binIDs[id]; ok {
				wanted = append(wanted, id)
			} else {
				missing++
			}
		}
	}

	accounts := make([]*auth.Account, 0, len(wanted))
	for _, id := range wanted {
		buildCtx, buildCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		acc, buildErr := h.store.BuildTransientAccountByID(buildCtx, id)
		buildCancel()
		if buildErr != nil || acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}

	restoreOnSuccess := req.RestoreOnSuccess
	testFn := func(ctx context.Context, acc *auth.Account) (string, string) {
		status, msg := h.runRecycleBinSingleTest(ctx, acc)
		h.persistRecycleBinTestResult(acc.DBID, status)
		if status == "success" && restoreOnSuccess {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
			restoreErr := h.restoreAccountByID(restoreCtx, acc.DBID)
			restoreCancel()
			if restoreErr != nil {
				msg = "test passed, but auto-restore failed: " + restoreErr.Error()
			} else {
				msg = "test passed, auto-restored"
			}
		}
		return status, msg
	}
	h.serveBatchTest(c, accounts, missing, testFn)
}

// serveBatchTest handles both streaming and non-streaming responses of batch tests; testFn decides the behavior of a single-account test.
func (h *Handler) serveBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchTest(c, accounts, missingCount, testFn)
		return
	}

	if len(accounts) == 0 && missingCount == 0 {
		c.JSON(http.StatusOK, gin.H{"total": 0, "success": 0, "failed": 0, "banned": 0, "rate_limited": 0})
		return
	}

	counts := h.runBatchTest(c.Request.Context(), accounts, missingCount, testFn, nil)
	c.JSON(http.StatusOK, gin.H{
		"total":        counts.Total,
		"success":      counts.Success,
		"failed":       counts.Failed,
		"banned":       counts.Banned,
		"rate_limited": counts.RateLimited,
	})
}

func (h *Handler) streamBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	total := len(accounts) + missingCount
	setupSSE(c)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_test", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_test"})
		return
	}

	events := make(chan batchOperationEvent, len(accounts)+2)
	ctx := c.Request.Context()
	go func() {
		counts := h.runBatchTest(ctx, accounts, missingCount, testFn, func(event batchOperationEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		select {
		case events <- batchOperationEvent{
			Type:        "complete",
			Action:      "batch_test",
			Current:     counts.Total,
			Total:       counts.Total,
			Success:     counts.Success,
			Failed:      counts.Failed,
			Banned:      counts.Banned,
			RateLimited: counts.RateLimited,
		}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for event := range events {
		sendSSEJSON(c, event)
	}
}

func (h *Handler) runBatchTest(ctx context.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string), onProgress func(batchOperationEvent)) batchTestCounts {
	total := len(accounts) + missingCount
	concurrency := 0
	if h != nil && h.store != nil {
		concurrency = h.store.GetTestConcurrency()
	}
	if concurrency <= 0 {
		concurrency = 1
	}

	var (
		successCount   int64
		failedCount    = int64(missingCount)
		bannedCount    int64
		rateLimitCount int64
		completedCount = int64(missingCount)
		wg             sync.WaitGroup
		sem            = make(chan struct{}, concurrency)
	)

	if missingCount > 0 && onProgress != nil {
		onProgress(batchOperationEvent{
			Type:    "progress",
			Action:  "batch_test",
			Current: missingCount,
			Total:   total,
			Failed:  failedCount,
			Error:   fmt.Sprintf("%d accounts are not in the runtime pool", missingCount),
		})
	}

	for _, account := range accounts {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&failedCount, 1)
				h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, "failed", "test cancelled")
				return
			}
			defer func() { <-sem }()

			status, message := testFn(ctx, acc)
			switch status {
			case "success":
				atomic.AddInt64(&successCount, 1)
			case "banned":
				atomic.AddInt64(&bannedCount, 1)
			case "rate_limited":
				atomic.AddInt64(&rateLimitCount, 1)
			default:
				atomic.AddInt64(&failedCount, 1)
			}
			h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, status, message)
		}(account)
	}

	wg.Wait()
	return batchTestCounts{
		Total:       total,
		Success:     atomic.LoadInt64(&successCount),
		Failed:      atomic.LoadInt64(&failedCount),
		Banned:      atomic.LoadInt64(&bannedCount),
		RateLimited: atomic.LoadInt64(&rateLimitCount),
	}
}

func (h *Handler) emitBatchTestProgress(
	onProgress func(batchOperationEvent),
	accountID int64,
	total int,
	completedCount *int64,
	successCount *int64,
	failedCount *int64,
	bannedCount *int64,
	rateLimitCount *int64,
	status string,
	message string,
) {
	if onProgress == nil {
		return
	}
	accountName, accountEmail := h.accountOperationIdentity(accountID)
	current := int(atomic.AddInt64(completedCount, 1))
	event := batchOperationEvent{
		Type:         "progress",
		Action:       "batch_test",
		Status:       status,
		HTTPStatus:   batchOperationHTTPStatus(status, message),
		Current:      current,
		Total:        total,
		Success:      atomic.LoadInt64(successCount),
		Failed:       atomic.LoadInt64(failedCount),
		Banned:       atomic.LoadInt64(bannedCount),
		RateLimited:  atomic.LoadInt64(rateLimitCount),
		AccountID:    accountID,
		AccountName:  accountName,
		AccountEmail: accountEmail,
		Message:      message,
	}
	if status == "failed" {
		event.Error = message
	}
	onProgress(event)
}

func (h *Handler) runSingleBatchTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()
	if acc == nil {
		return "failed", "账号不存在"
	}

	if !acc.IsCodexAgentIdentity() && acc.GetAccessToken() == "" {
		acc.Mu().RLock()
		hasRefreshToken := acc.RefreshToken != ""
		acc.Mu().RUnlock()
		if !hasRefreshToken {
			h.store.MarkError(acc, "batch test failed: account is missing access_token and refresh_token")
		}
		return "failed", "account is missing access_token and refresh_token"
	}

	if status, msg, done := h.batchTestSkipDeactivatedWorkspace(acc); done {
		return status, msg
	}

	if status, msg, done := h.batchTestWhamPreflight(testCtx, acc); done {
		return status, msg
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		h.store.MarkError(acc, "batch test failed: "+modelErr.Error())
		return "failed", modelErr.Error()
	}
	payload := h.buildAccountConnectionTestPayload(testCtx, acc, testModel)
	start := time.Now()

	resp, err := proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	if err != nil {
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			if errors.Is(testCtx.Err(), context.DeadlineExceeded) {
				h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
			}
			return "failed", msg
		}
		h.store.MarkError(acc, "batch test request failed: "+err.Error())
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		usageState := proxy.SyncCodexUsageState(h.store, acc, resp)
		applyUsageLimitedTestState(h.store, acc, usageState)
		if msg, limited := formatUsageLimitedTestError(usageState); limited {
			return "rate_limited", msg
		}
		status, msg := h.readBatchTestStreamResult(testCtx, acc, resp, testModel)
		if status != "success" {
			return status, msg
		}
		// A successful test resets the failure/cooldown state; usage limits are judged by the scheduler itself
		h.store.RecordManualTestSuccess(acc, time.Since(start))
		return "success", msg
	case http.StatusUnauthorized:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		proxy.SyncCodexUsageState(h.store, acc, resp)
		h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300)))
		return "banned", "upstream returned 401: account authorization failed"
	case http.StatusTooManyRequests:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		proxy.SyncCodexUsageState(h.store, acc, resp)
		proxy.Apply429Cooldown(h.store, acc, body, resp, testModel)
		return "rate_limited", "upstream returned 429: account hit rate limiting"
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		msg := fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300))
		if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
			h.store.MarkCooldownWithErrorExactDuration(acc, 24*time.Hour, "unauthorized", msg)
			return "banned", msg
		}
		if shouldMarkBatchTestAccountError(resp.StatusCode, body) {
			if proxy.IsDeactivatedWorkspaceError(body) {
				h.store.MarkDeactivatedWorkspace(acc, "batch test"+msg)
			} else {
				h.store.MarkError(acc, "batch test"+msg)
			}
		}
		return "failed", msg
	}
}

// runRecycleBinSingleTest tests the connectivity of a single recycle bin account. The account is a temporary object,
// and the whole run calls no method that writes account or scheduling state back (MarkError/MarkCooldown/
// RecordManualTestSuccess and so on); the test result is for display only.
func (h *Handler) runRecycleBinSingleTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()
	if acc == nil {
		return "failed", "账号不存在"
	}

	if !acc.IsCodexAgentIdentity() && acc.GetAccessToken() == "" {
		return "failed", "account has no usable Access Token"
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		return "failed", modelErr.Error()
	}
	payload := h.buildAccountConnectionTestPayload(testCtx, acc, testModel)

	resp, err := proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	if err != nil {
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			return "failed", msg
		}
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// store is passed as nil: only usage headers are parsed for the result display; nothing is persisted and the rate-limit state is not changed.
		usageState := proxy.SyncCodexUsageState(nil, acc, resp)
		if msg, limited := formatUsageLimitedTestError(usageState); limited {
			return "rate_limited", msg
		}
		return readRecycleBinTestStream(testCtx, resp)
	case http.StatusUnauthorized:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "banned", fmt.Sprintf("upstream returned 401: %s", truncate(string(body), 300))
	case http.StatusTooManyRequests:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "rate_limited", fmt.Sprintf("upstream returned 429: %s", truncate(string(body), 300))
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			if msg, ok := batchTestContextFailure(testCtx, readErr); ok {
				return "failed", msg
			}
			return "failed", readErr.Error()
		}
		return "failed", fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
}

// readRecycleBinTestStream reads the test SSE stream and decides the result; it is equivalent to
// readBatchTestStreamResult, but does not write back any account state.
func readRecycleBinTestStream(ctx context.Context, resp *http.Response) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus = "failed"
				if proxy.IsUsageLimitReachedError(data) {
					resultStatus = "rate_limited"
				}
				resultMessage = formatUpstreamTestError(data, "upstream returned "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				return false
			}
			resultStatus = "success"
			resultMessage = "test passed"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus = "failed"
			if proxy.IsUsageLimitReachedError(data) {
				resultStatus = "rate_limited"
			}
			resultMessage = formatUpstreamTestError(data, "upstream returned response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus = "failed"
			resultMessage = formatUpstreamTestError(data, "upstream returned an error event")
			return false
		}
		return true
	})

	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return "failed", msg
		}
		return "failed", readErr.Error()
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		return "failed", formatMissingTerminalUpstreamError(lastUpstreamEvent)
	}
	return "failed", "upstream test returned no clear result"
}

func (h *Handler) batchTestSkipDeactivatedWorkspace(acc *auth.Account) (string, string, bool) {
	if h == nil || h.store == nil || acc == nil {
		return "", "", false
	}
	msg, ok := h.store.LinkedDeactivatedWorkspaceResult(acc)
	if !ok {
		return "", "", false
	}
	if acc.RuntimeStatus() != "error" {
		h.store.MarkError(acc, msg)
	}
	return "failed", msg, true
}

func (h *Handler) batchTestWhamPreflight(ctx context.Context, acc *auth.Account) (string, string, bool) {
	if h == nil || h.store == nil || acc == nil || acc.GetAccessToken() == "" {
		return "", "", false
	}

	whamCtx, cancel := context.WithTimeout(ctx, batchTestWhamTimeout)
	defer cancel()

	usage, resp, err := proxy.QueryWhamUsage(whamCtx, acc, h.store.ResolveProxyForAccount(acc))
	if resp != nil && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg := fmt.Sprintf("WHAM usage probe returned %d: %s", resp.StatusCode, truncate(string(body), 300))
			h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", msg)
			return "banned", msg, true
		default:
			if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
				msg := fmt.Sprintf("WHAM usage probe returned %d: %s", resp.StatusCode, truncate(string(body), 300))
				if proxy.IsDeactivatedWorkspaceError(body) {
					h.store.MarkDeactivatedWorkspace(acc, msg)
				} else {
					h.store.MarkError(acc, msg)
				}
				return "failed", msg, true
			}
		}
	}
	if err != nil || usage == nil {
		return "", "", false
	}

	usageState := proxy.ApplyWhamUsage(h.store, acc, usage)
	// wham has no subscription expiry fields, so the authoritative expiry time is fetched on demand from the web /subscriptions page. (issue #360)
	proxy.MaybeSyncSubscriptionExpiry(ctx, h.store, acc, h.store.ResolveProxyForAccount(acc))
	applyUsageLimitedTestState(h.store, acc, usageState)
	if msg, limited := formatUsageLimitedTestError(usageState); limited {
		return "rate_limited", msg, true
	}
	return "", "", false
}

func (h *Handler) readBatchTestStreamResult(ctx context.Context, acc *auth.Account, resp *http.Response, model string) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "upstream returned "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				h.markBatchTestStreamFailure(acc, resultMessage)
				return false
			}
			resultStatus = "success"
			resultMessage = "test passed"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "upstream returned response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "upstream returned an error event")
			return false
		}
		return true
	})

	if readErr != nil {
		return h.handleBatchTestReadError(ctx, acc, readErr)
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		msg := formatMissingTerminalUpstreamError(lastUpstreamEvent)
		h.markBatchTestStreamFailure(acc, msg)
		return "failed", msg
	}
	msg := "upstream test returned no clear result"
	h.markBatchTestStreamFailure(acc, msg)
	return "failed", msg
}

func (h *Handler) batchTestTerminalFailure(acc *auth.Account, resp *http.Response, model string, payload []byte, fallback string) (string, string) {
	message := formatUpstreamTestError(payload, fallback)
	if h.applyResponsesUsageLimitFailure(acc, resp, model, payload) {
		return "rate_limited", message
	}
	h.markBatchTestStreamFailure(acc, message)
	return "failed", message
}

func (h *Handler) markBatchTestStreamFailure(acc *auth.Account, message string) {
	if h == nil || h.store == nil || acc == nil {
		return
	}
	switch acc.RuntimeStatus() {
	case "active", "ready", "refreshing", "error":
		h.store.MarkError(acc, "batch test failed: "+message)
	}
}

func readBatchTestErrorBody(ctx context.Context, body io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(body, 64<<10))
}

func (h *Handler) handleBatchTestReadError(ctx context.Context, acc *auth.Account, err error) (string, string) {
	if msg, ok := batchTestContextFailure(ctx, err); ok {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
		}
		return "failed", msg
	}
	h.store.MarkError(acc, "batch test failed to read response: "+err.Error())
	return "failed", err.Error()
}

func batchTestContextFailure(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("test timed out: not finished within %s", batchTestAccountTimeout), true
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "test cancelled", true
	}
	return "", false
}

func shouldMarkBatchTestAccountError(statusCode int, body []byte) bool {
	msg := strings.ToLower(string(body))
	if statusCode == http.StatusPaymentRequired {
		return true
	}
	if statusCode == http.StatusForbidden {
		return true
	}
	if statusCode == http.StatusBadRequest {
		for _, needle := range []string{
			"invalid_grant",
			"invalid_client",
			"unauthorized_client",
			"access_denied",
			"account_deactivated",
			"unsupported_country_region_territory",
		} {
			if strings.Contains(msg, needle) {
				return true
			}
		}
	}
	return false
}
