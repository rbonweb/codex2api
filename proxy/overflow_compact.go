package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ==================== Context overflow auto-compaction ====================
// Upstream returns a deterministic 400 (context_length_exceeded) for input that exceeds the model's context window,
// retrying on another account always fails too, so until now the gateway could only pass the error through to the downstream client (issue #310/#415).
// Upstream does not accept the truncation/context_management parameters (observed 400 Unsupported parameter),
// so server-side truncation is unavailable; clients that do not compact (chat UI / SDK) can only start a new session.
//
// This module provides a gateway-side fallback: when the per-key switch (limits.auto_compact_overflow) is on,
// after an overflow error, the old turns in input[] are summarized into one developer message, keeping the most recent
// turns verbatim, then retries once. The summary is produced by an internal Responses call; if summarizing fails, it degrades to
// dropping the old turns outright and inserting an omitted marker (so the retry can still be sent).

const (
	// overflowCompactTailBytesDefault is the budget (bytes) for the "recent turns" kept verbatim after compaction.
	overflowCompactTailBytesDefault = 200 * 1024
	// overflowCompactSummaryInputBytesDefault is the upper limit (bytes) of old-turn text sent for summarization,
	// so the summary call itself stays well clear of the window limit.
	overflowCompactSummaryInputBytesDefault = 512 * 1024
	// overflowCompactSummaryTimeout is the total timeout of the summary call (covering all shrink-and-retry attempts).
	overflowCompactSummaryTimeout = 120 * time.Second
	// overflowCompactSummaryMinInputBytes is the transcript floor for shrink retries: an input any smaller already
	// cannot carry useful information, and halving further would only waste an upstream round trip.
	overflowCompactSummaryMinInputBytes = 32 * 1024
	// overflowCompactSummaryMaxAttempts is the maximum number of summary calls (including shrink retries).
	overflowCompactSummaryMaxAttempts = 3
	// overflowCompactFallbackDigestBytes is the budget for excerpting old-turn text directly when the model summary fails completely.
	// It is far smaller than the tail budget, so it will not blow the size that was just compacted back up.
	overflowCompactFallbackDigestBytes = 16 * 1024
	// overflowCompactMinItems is the minimum number of input items worth compacting (below it, the input is most likely one oversized item,
	// which compaction cannot fix, so the original error is passed through).
	overflowCompactMinItems = 4
	// overflowCompactItemTextCap is the text cap per old-turn item, after flattening, that counts toward the summary transcript.
	overflowCompactItemTextCap = 8 * 1024
)

const overflowCompactSummaryPrefix = "[Conversation summary from earlier turns]\n"
const overflowCompactOmittedMarker = "[Earlier conversation turns were omitted because the input exceeded the model context window.]"
const overflowCompactDigestPrefix = "[Verbatim excerpt of those turns, heavily truncated and possibly incomplete:]\n"

func overflowCompactTailBytes() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_OVERFLOW_COMPACT_TAIL_KB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n * 1024
		}
	}
	return overflowCompactTailBytesDefault
}

func overflowCompactSummaryInputBytes() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_OVERFLOW_COMPACT_SUMMARY_INPUT_KB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n * 1024
		}
	}
	return overflowCompactSummaryInputBytesDefault
}

// autoCompactOverflowEnabled reports whether the current request has context overflow auto-compaction enabled:
// the global experimental switch (system setting overflow_auto_compact_enabled) or the per-key
// limits.auto_compact_overflow; either one being on is enough. The internal summary call uses an explicit context marker
// to block recursion, and must not rely on the accidental side effect of "no key identity".
func autoCompactOverflowEnabled(c *gin.Context) bool {
	// The summary call is scheduled internally; a pinned request must not leave
	// its account, so the context overflow error goes to the external scheduler.
	if _, pinned := pinnedAccountIDFromContext(c); pinned {
		return false
	}
	if c != nil {
		if disabled, exists := c.Get(contextDisableOverflowAutoCompact); exists {
			if value, ok := disabled.(bool); ok && value {
				return false
			}
		}
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		return false
	}
	if row.Limits.AutoCompactOnOverflow {
		return true
	}
	return CurrentRuntimeSettings().OverflowAutoCompact
}

// isCodexPreflightSSEEvent reports whether an event is a metadata event that upstream sends before the response content:
// codex.* (codex.rate_limits / codex.response.metadata, WS transport path) and bare
// response.metadata (HTTP transport path); neither carries model output.
func isCodexPreflightSSEEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "response.metadata" {
		return true
	}
	return strings.HasPrefix(eventType, "codex.")
}

// shouldDeferPreContentSSEEvent decides whether an SSE event before the first content event is held back from flushing.
// Lifecycle events (response.created / response.in_progress) are always buffered; pre-content metadata
// events are buffered too by default, and are written out immediately when preflightPassthrough is on (legacy compatibility, issue #425) —
// the cost is committing a 200 early, so a response.failed within that window can no longer be returned with its real error code,
// and it can no longer take the silent account-switch or overflow-compaction retry path.
func shouldDeferPreContentSSEEvent(eventType string, contentTokenSeen, gotTerminal, preflightPassthrough bool) bool {
	return !contentTokenSeen && !gotTerminal &&
		(isPreContentLifecycleEvent(eventType) ||
			(!preflightPassthrough && isCodexPreflightSSEEvent(eventType)))
}

// isContextLengthExceededBody reports whether an upstream error body (an HTTP error response or the error object of a response.failed
// event) is a context overflow.
func isContextLengthExceededBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, path := range []string{"error.code", "error.type", "code", "detail.code"} {
		if strings.Contains(strings.ToLower(gjson.GetBytes(body, path).String()), "context_length") {
			return true
		}
	}
	for _, path := range []string{"error.message", "message", "detail"} {
		msg := strings.ToLower(gjson.GetBytes(body, path).String())
		if strings.Contains(msg, "exceeds the context window") || strings.Contains(msg, "context length") {
			return true
		}
	}
	return false
}

// isContextLengthExceededFailedPayload reports whether a response.failed event is a context overflow.
func isContextLengthExceededFailedPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	errObj := gjson.GetBytes(payload, "response.error")
	if !errObj.Exists() {
		return false
	}
	return isContextLengthExceededBody([]byte(errObj.Raw))
}

// compactOverflowResponsesBody compacts the old turns in input[] of the prepared Codex body
// into one developer summary message, keeping the latest turns at the tail verbatim. It returns the compacted body.
// When it cannot compact (input too short / structure not as expected) it returns ok=false, and the caller passes the original error through.
func (h *Handler) compactOverflowResponsesBody(ctx context.Context, codexBody []byte) ([]byte, bool) {
	return h.compactOverflowResponsesBodyWithAttribution(ctx, codexBody, nil)
}

// compactOverflowResponsesBodyForRequest preserves the authenticated parent
// identity for the model-generated summary while leaving the generic helper
// anonymous for administrative callers and focused unit tests.
func (h *Handler) compactOverflowResponsesBodyForRequest(c *gin.Context, codexBody []byte) ([]byte, bool) {
	if c == nil || c.Request == nil {
		return h.compactOverflowResponsesBody(context.Background(), codexBody)
	}
	attribution := internalResponseAttributionFromRequest(c, internalReasonOverflowCompact)
	return h.compactOverflowResponsesBodyWithAttribution(c.Request.Context(), codexBody, attribution)
}

func (h *Handler) compactOverflowResponsesBodyWithAttribution(ctx context.Context, codexBody []byte, attribution *internalResponseAttribution) ([]byte, bool) {
	var body map[string]any
	if err := json.Unmarshal(codexBody, &body); err != nil {
		return nil, false
	}
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) < overflowCompactMinItems {
		return nil, false
	}

	// Accumulate bytes from the tail backwards, keeping the recent turns within tailBudget verbatim.
	tailBudget := overflowCompactTailBytes()
	cut := len(inputItems)
	used := 0
	for i := len(inputItems) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(inputItems[i])
		if err != nil {
			break
		}
		if used+len(encoded) > tailBudget && cut < len(inputItems) {
			break
		}
		used += len(encoded)
		cut = i
	}
	// The first developer/system message usually carries the system prompt, so it is always kept verbatim.
	headStart := 0
	if first, ok := inputItems[0].(map[string]any); ok && isResponsesMessageInputItem(first) {
		switch strings.TrimSpace(firstNonEmptyAnyString(first["role"])) {
		case "developer", "system":
			headStart = 1
		}
	}
	if cut <= headStart {
		// The tail budget already covers all old turns, so there is no head to compact: the overflow comes from
		// a single oversized item or an oversized budget, so compaction is pointless.
		return nil, false
	}
	head := inputItems[headStart:cut]
	tail := inputItems[cut:]

	summaryText := h.summarizeOverflowItems(ctx, firstNonEmptyAnyString(body["model"]), head, attribution)
	replacement := overflowCompactSummaryPrefix + summaryText
	mode := "model summary"
	if summaryText == "" {
		replacement, mode = overflowCompactFallbackText(head)
	}

	newInput := make([]any, 0, headStart+1+len(tail))
	newInput = append(newInput, inputItems[:headStart]...)
	newInput = append(newInput, map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": replacement},
		},
	})
	newInput = append(newInput, tail...)
	body["input"] = newInput

	// The compaction boundary may cut a tool call pair (the call of a tail output gets summarized away), so the existing pair repair is reused.
	repairResponsesToolCallPairing(body)

	result, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	log.Printf("Context overflow auto-compaction: %d old turns -> summary (%s), keeping latest %d turns, body %dKB -> %dKB",
		len(head), mode, len(tail), len(codexBody)/1024, len(result)/1024)
	return result, true
}

// overflowCompactFallbackText is the degraded text used when the model summary is unavailable. Before, it only inserted one line "old turns
// have been omitted", so the whole history silently evaporated, the model answered without context, and downstream had no way to notice. It now adds, after the omitted marker,
// a directly excerpted passage of the old turns (far smaller than the tail budget, so it will not push the window over again),
// which keeps at least the user goal, the key path and the unfinished items. Only when even the excerpt is unavailable does it fall back to the bare marker.
func overflowCompactFallbackText(items []any) (string, string) {
	digest := strings.TrimSpace(flattenOverflowItemsTranscript(items, overflowCompactFallbackDigestBytes))
	if digest == "" {
		return overflowCompactOmittedMarker, "omitted marker"
	}
	return overflowCompactOmittedMarker + "\n" + overflowCompactDigestPrefix + digest, "mechanical excerpt"
}

// summarizeOverflowItems summarizes the old-turn transcript into one text via an internal Responses call.
//
// The summary call itself can overflow too: the transcript cap is a fixed byte count, but the model window varies by account/mapping,
// so a 512KB transcript on a small-window model is bound to 400 again. In that case it halves the retry by the actual transcript length, until it succeeds,
// hits the floor, or runs out of attempts. This keeps far more context than "one failure drops all old turns".
// Other failures (auth, no usable account, rate limit, no text output) do not depend on input size, so they are not retried.
// All attempts share one overflowCompactSummaryTimeout deadline, so the worst-case duration stays the same.
// An empty string means complete failure, and the caller degrades to a mechanical excerpt.
func (h *Handler) summarizeOverflowItems(ctx context.Context, model string, items []any, attribution *internalResponseAttribution) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	callCtx, cancel := context.WithTimeout(ctx, overflowCompactSummaryTimeout)
	defer cancel()

	budget := overflowCompactSummaryInputBytes()
	for attempt := 1; attempt <= overflowCompactSummaryMaxAttempts; attempt++ {
		transcript := flattenOverflowItemsTranscript(items, budget)
		if strings.TrimSpace(transcript) == "" {
			return ""
		}
		text, status, respBody := h.requestOverflowSummary(callCtx, model, transcript, attribution)
		if text != "" {
			return text
		}
		if callCtx.Err() != nil {
			log.Printf("Context overflow auto-compaction: summary call timed out (attempt %d), degrading to mechanical excerpt", attempt)
			return ""
		}
		if !overflowSummaryFailedForContextWindow(status, respBody) {
			if status == 200 {
				log.Printf("Context overflow auto-compaction: summary call returned no text output, degrading to mechanical excerpt")
			} else {
				log.Printf("Context overflow auto-compaction: summary call failed (status %d), degrading to mechanical excerpt", status)
			}
			return ""
		}
		// Halve by the actual transcript length so it strictly shrinks (budget may be far larger than the transcript itself).
		next := len(transcript) / 2
		if attempt == overflowCompactSummaryMaxAttempts || next < overflowCompactSummaryMinInputBytes {
			log.Printf("Context overflow auto-compaction: summary call itself overflowed and can no longer shrink (attempt %d, %dKB), degrading to mechanical excerpt", attempt, len(transcript)/1024)
			return ""
		}
		log.Printf("Context overflow auto-compaction: summary call itself overflowed (attempt %d, %dKB), shrinking to %dKB and retrying", attempt, len(transcript)/1024, next/1024)
		budget = next
	}
	return ""
}

// overflowSummaryFailedForContextWindow reports whether a summary call failed because its own input
// overflowed: only this kind of failure makes shrink-and-retry meaningful. A non-streaming error body is JSON and can be checked directly;
// a streaming response body is SSE text that gjson cannot parse as a whole, so it falls back to matching the error code substring.
func overflowSummaryFailedForContextWindow(status int, body []byte) bool {
	if status == 200 || len(body) == 0 {
		return false
	}
	if isContextLengthExceededBody(body) {
		return true
	}
	lowered := strings.ToLower(string(body))
	return strings.Contains(lowered, "context_length") || strings.Contains(lowered, "exceeds the context window")
}

// requestOverflowSummary runs one summary call and returns the extracted summary text along with the raw status/response body
// (so the caller can determine the cause of failure).
func (h *Handler) requestOverflowSummary(ctx context.Context, model, transcript string, attribution *internalResponseAttribution) (string, int, []byte) {
	reqBody, err := json.Marshal(map[string]any{
		"model":     model,
		"stream":    true,
		"reasoning": map[string]any{"effort": "low"},
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": []any{map[string]any{"type": "input_text", "text": "You are a conversation compaction assistant. Summarize the earlier conversation transcript provided by the user into a dense briefing for the assistant that will continue this conversation. Preserve: key facts and decisions, user goals and constraints, file paths, code identifiers, tool results that matter, and unresolved tasks. Output only the summary text."}},
			},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": transcript}},
			},
		},
	})
	if err != nil {
		return "", 0, nil
	}
	status, respBody := h.executeInternalResponseWithAttribution(ctx, reqBody, attribution)
	if status != 200 {
		return "", status, respBody
	}
	return strings.TrimSpace(extractResponsesSSEOutputText(respBody)), status, respBody
}

// flattenOverflowItemsTranscript flattens the input items into a "role: text" transcript,
// truncating over-long content per item, and when the total exceeds cap, keeping the head and tail and dropping the middle.
func flattenOverflowItemsTranscript(items []any, capBytes int) string {
	var lines []string
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		switch {
		case isResponsesMessageInputItem(item):
			role := strings.TrimSpace(firstNonEmptyAnyString(item["role"]))
			if role == "" {
				role = "user"
			}
			text := flattenMessageContentText(item["content"])
			if text != "" {
				lines = append(lines, role+": "+capText(text, overflowCompactItemTextCap))
			}
		case isCodexToolCallContextType(typ):
			name := strings.TrimSpace(firstNonEmptyAnyString(item["name"]))
			args := capText(firstNonEmptyAnyString(item["arguments"]), 1024)
			lines = append(lines, "assistant tool call "+name+"("+args+")")
		case isCodexToolCallOutputType(typ):
			lines = append(lines, "tool output: "+capText(flattenToolOutputText(item["output"]), overflowCompactItemTextCap))
		}
	}
	transcript := strings.Join(lines, "\n")
	if len(transcript) <= capBytes {
		return transcript
	}
	// Keep half for the head and half for the tail, and replace the middle with a marker.
	half := capBytes / 2
	return transcript[:half] + "\n[... middle of the transcript truncated ...]\n" + transcript[len(transcript)-half:]
}

// flattenMessageContentText flattens message content (a string or an array of content parts) into text.
func flattenMessageContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := firstNonEmptyAnyString(part["text"]); text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
		return sb.String()
	}
	return ""
}

func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// extractResponsesSSEOutputText extracts from the SSE response body of an internal Responses call
// all the output_text of response.completed.
func extractResponsesSSEOutputText(sse []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(sse))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[len("data: "):]
		if gjson.GetBytes(payload, "type").String() != "response.completed" {
			continue
		}
		var sb strings.Builder
		gjson.GetBytes(payload, "response.output").ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() != "message" {
				return true
			}
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					sb.WriteString(part.Get("text").String())
				}
				return true
			})
			return true
		})
		return sb.String()
	}
	return ""
}
