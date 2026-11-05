package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Result categories for a single-model probe.
const (
	modelProbeAvailable   = "available"   // 200 completed with output: the account is confirmed to be able to use this model
	modelProbeUnsupported = "unsupported" // upstream explicitly rejected it: the account's plan does not support this model
	modelProbeThrottled   = "throttled"   // 429 or usage exhausted: the model may be available but is currently rate limited, so the result is unreliable
	modelProbeError       = "error"       // other errors: timeout, transport, auth, 5xx, etc.; cannot be determined
)

// Concurrency cap for single-model probes, so one click does not send too many concurrent requests to the same account.
const modelProbeMaxConcurrency = 6

type modelProbeResult struct {
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// modelProbeEvent is an SSE event pushed to the frontend during probing.
// type: start (sends all models to test) | testing (a model's probe started) | result (a model produced a result) | done (finished, includes the set of available models)
type modelProbeEvent struct {
	Type      string   `json:"type"`
	Total     int      `json:"total,omitempty"`
	Current   int      `json:"current,omitempty"`
	Models    []string `json:"models,omitempty"`
	Model     string   `json:"model,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Available []string `json:"available,omitempty"`
}

// ProbeAccountModels probes the system model list concurrently using the account's own credentials (image models excluded),
// deciding whether each model is usable. It is read-only throughout and does not write back account scheduling state (cooldown / error / success).
// With stream=true, progress is pushed per model over SSE; otherwise the JSON is returned in one response.
// POST /api/admin/accounts/:id/models/probe
func (h *Handler) ProbeAccountModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "Account is not in the runtime pool")
		return
	}
	if !account.IsCodexAgentIdentity() && account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "Account has no usable access token; refresh it first")
		return
	}

	h.refreshImportedDaybreak(c.Request.Context(), id)
	models := proxy.TextTestModelIDs(c.Request.Context(), h.db)
	streaming := strings.EqualFold(c.Query("stream"), "true")

	if len(models) == 0 {
		if streaming {
			setupSSE(c)
			sendSSEJSON(c, modelProbeEvent{Type: "start", Total: 0, Models: []string{}})
			sendSSEJSON(c, modelProbeEvent{Type: "done", Available: []string{}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"available": []string{}, "results": []modelProbeResult{}})
		return
	}

	concurrency := h.store.GetTestConcurrency()
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > modelProbeMaxConcurrency {
		concurrency = modelProbeMaxConcurrency
	}

	if streaming {
		h.streamProbeModels(c, account, models, concurrency)
		return
	}

	results := h.runProbeModels(c.Request.Context(), account, models, concurrency, nil)
	available := collectAvailableModels(results)
	c.JSON(http.StatusOK, gin.H{
		"available": available,
		"results":   results,
	})
}

// streamProbeModels pushes probe progress per model over SSE.
func (h *Handler) streamProbeModels(c *gin.Context, account *auth.Account, models []string, concurrency int) {
	setupSSE(c)
	sendSSEJSON(c, modelProbeEvent{Type: "start", Total: len(models), Models: models})

	events := make(chan modelProbeEvent, len(models)+1)
	ctx := c.Request.Context()
	go func() {
		results := h.runProbeModels(ctx, account, models, concurrency, func(ev modelProbeEvent) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		})
		select {
		case events <- modelProbeEvent{Type: "done", Available: collectAvailableModels(results)}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for ev := range events {
		sendSSEJSON(c, ev)
	}
}

// runProbeModels probes all models concurrently; when onEvent is non-nil, it is called with testing/result events per model (for SSE).
func (h *Handler) runProbeModels(ctx context.Context, account *auth.Account, models []string, concurrency int, onEvent func(modelProbeEvent)) []modelProbeResult {
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		results   = make([]modelProbeResult, len(models))
		sem       = make(chan struct{}, concurrency)
		completed int
		total     = len(models)
	)
	for i, model := range models {
		wg.Add(1)
		go func(idx int, m string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				results[idx] = modelProbeResult{Model: m, Outcome: modelProbeError, Detail: "probe cancelled"}
				completed++
				current := completed
				mu.Unlock()
				if onEvent != nil {
					onEvent(modelProbeEvent{Type: "result", Model: m, Outcome: modelProbeError, Detail: "probe cancelled", Current: current, Total: total})
				}
				return
			}
			defer func() { <-sem }()

			if onEvent != nil {
				onEvent(modelProbeEvent{Type: "testing", Model: m})
			}
			outcome, detail := h.probeAccountModel(ctx, account, m)
			mu.Lock()
			results[idx] = modelProbeResult{Model: m, Outcome: outcome, Detail: detail}
			completed++
			current := completed
			mu.Unlock()
			if onEvent != nil {
				onEvent(modelProbeEvent{Type: "result", Model: m, Outcome: outcome, Detail: detail, Current: current, Total: total})
			}
		}(i, model)
	}
	wg.Wait()
	return results
}

func collectAvailableModels(results []modelProbeResult) []string {
	available := make([]string, 0, len(results))
	for _, r := range results {
		if r.Outcome == modelProbeAvailable {
			available = append(available, r.Model)
		}
	}
	available = auth.NormalizeAccountModels(available)
	sort.Strings(available)
	return available
}

// probeAccountModel sends a minimal probe request for a single model and classifies the result. It does not write back any account state.
func (h *Handler) probeAccountModel(ctx context.Context, account *auth.Account, model string) (string, string) {
	probeCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()

	payload := buildConnectionTestPayload(h.store, model)
	resp, err := proxy.ExecuteRequest(probeCtx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		if msg, ok := batchTestContextFailure(probeCtx, err); ok {
			return modelProbeError, msg
		}
		return modelProbeError, err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return readProbeStream(probeCtx, resp)
	case http.StatusTooManyRequests:
		return modelProbeThrottled, "upstream returned 429 (rate limited)"
	case http.StatusBadRequest:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		if proxy.IsCodexModelUnsupportedError(body) {
			return modelProbeUnsupported, "the account's plan does not support this model"
		}
		return modelProbeError, fmt.Sprintf("upstream returned 400: %s", truncate(string(body), 200))
	case http.StatusUnauthorized:
		return modelProbeError, "account authorization failed (401)"
	default:
		body, _ := readBatchTestErrorBody(probeCtx, resp.Body)
		return modelProbeError, fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// readProbeStream reads the probe SSE stream and classifies it; it can recognize from the terminal event that the account's plan does not support the model.
// It does not write back any account state.
func readProbeStream(ctx context.Context, resp *http.Response) (string, string) {
	hasContent := false
	gotTerminal := false
	outcome := ""
	detail := ""
	var lastUpstreamEvent []byte

	classifyFailure := func(data []byte, fallback string) (string, string) {
		if proxy.IsUsageLimitReachedError(data) {
			return modelProbeThrottled, formatUpstreamTestError(data, "upstream usage exhausted")
		}
		if proxy.IsCodexModelUnsupportedError(data) {
			return modelProbeUnsupported, "the account's plan does not support this model"
		}
		return modelProbeError, formatUpstreamTestError(data, fallback)
	}

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
				outcome, detail = classifyFailure(data, "upstream returned "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				outcome = modelProbeError
				detail = formatNoOutputUpstreamError(data)
				return false
			}
			outcome = modelProbeAvailable
			detail = "probe passed"
			return false
		case "response.failed":
			gotTerminal = true
			outcome, detail = classifyFailure(data, "upstream returned response.failed")
			return false
		case "error":
			gotTerminal = true
			outcome, detail = classifyFailure(data, "upstream returned an error event")
			return false
		}
		return true
	})

	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return modelProbeError, msg
		}
		return modelProbeError, readErr.Error()
	}
	if outcome != "" {
		return outcome, detail
	}
	if !gotTerminal {
		return modelProbeError, formatMissingTerminalUpstreamError(lastUpstreamEvent)
	}
	return modelProbeError, "upstream probe returned no clear result"
}
