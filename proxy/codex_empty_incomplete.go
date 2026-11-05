package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Upstream occasionally ends a response that produced nothing with HTTP 200 + response.incomplete:
// usage.output_tokens is exactly 0, response.output is empty, and there was no earlier non-empty delta
// or output_item.done. This is not a max_output_tokens truncation but a silent upstream abort (one known trigger
// is a large oneOf/const union in the tool schema). Forwarding it as-is would make the Chat downstream receive
// an empty reply with finish_reason=length that is never retried, which looks like "the model died".
//
// Here it is rewritten in place as response.failed, reusing the existing transparent retry / account-switch path before the first packet. The check
// is deliberately tight (exact value 0, no output item, no non-empty delta), to avoid misclassifying a normal truncation again
// as a stream break (the kind of regression fixed in v2.8.3). The failure counts at the request level and does not affect account health.
const (
	codexEmptyIncompleteErrorCode    = "empty_incomplete_response"
	codexEmptyIncompleteErrorMessage = "upstream terminated with an incomplete empty response (0 output tokens)"
	codexEmptyIncompleteFailureKind  = "empty_incomplete"
)

// emptyIncompleteTracker records whether real output appeared during one upstream attempt. One per attempt.
type emptyIncompleteTracker struct {
	sawOutputDelta bool
	outputItems    int
}

// Observe records an event. Only non-empty text, thinking and tool argument deltas and output_item.done count.
func (t *emptyIncompleteTracker) Observe(eventType string, parsed gjson.Result) {
	if t == nil {
		return
	}
	switch eventType {
	case "response.output_text.delta",
		"response.reasoning_text.delta",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		if strings.TrimSpace(parsed.Get("delta").String()) != "" {
			t.sawOutputDelta = true
		}
	case "response.output_item.done":
		t.outputItems++
	}
}

// IsEmptyIncomplete reports whether this terminal event is a zero-output response.incomplete.
func (t *emptyIncompleteTracker) IsEmptyIncomplete(eventType string, parsed gjson.Result) bool {
	if eventType != "response.incomplete" {
		return false
	}
	if t != nil && (t.sawOutputDelta || t.outputItems > 0) {
		return false
	}
	return isEmptyIncompleteResponse(parsed.Get("response"))
}

// isEmptyIncompleteResponse requires output to be empty and usage.output_tokens to be the literal integer 0.
// A missing value, null, float or non-number never counts; it is better to miss a case than to misjudge one.
func isEmptyIncompleteResponse(response gjson.Result) bool {
	if !response.Exists() || !response.IsObject() {
		return false
	}
	switch response.Get("incomplete_details.reason").String() {
	case "interrupted", "steered", "content_filter", "max_messages":
		return false
	}
	if output := response.Get("output"); output.IsArray() && len(output.Array()) > 0 {
		return false
	}
	tokens := response.Get("usage.output_tokens")
	if !tokens.Exists() || tokens.Type != gjson.Number {
		return false
	}
	return strings.TrimSpace(tokens.Raw) == "0"
}

// isEmptyIncompleteResponseBody applies the same check to a non-streaming Responses object.
func isEmptyIncompleteResponseBody(body []byte) bool {
	root := gjson.ParseBytes(body)
	if strings.ToLower(strings.TrimSpace(root.Get("status").String())) != "incomplete" {
		return false
	}
	return isEmptyIncompleteResponse(root)
}

// synthesizeEmptyIncompleteFailureEvent rewrites a response.incomplete event into
// response.failed, keeping response.id/model/usage and other fields for logging and billing.
func synthesizeEmptyIncompleteFailureEvent(data []byte) []byte {
	out := append([]byte(nil), data...)
	out, _ = sjson.SetBytes(out, "type", "response.failed")
	out, _ = sjson.DeleteBytes(out, "response.incomplete_details")
	return setEmptyIncompleteFailure(out, "response.")
}

// synthesizeEmptyIncompleteFailureBody applies the same rewrite to a non-streaming Responses object.
func synthesizeEmptyIncompleteFailureBody(body []byte) []byte {
	out := append([]byte(nil), body...)
	out, _ = sjson.DeleteBytes(out, "incomplete_details")
	return setEmptyIncompleteFailure(out, "")
}

func setEmptyIncompleteFailure(payload []byte, prefix string) []byte {
	payload, _ = sjson.SetBytes(payload, prefix+"status", "failed")
	payload, _ = sjson.SetBytes(payload, prefix+"error.type", "server_error")
	payload, _ = sjson.SetBytes(payload, prefix+"error.code", codexEmptyIncompleteErrorCode)
	payload, _ = sjson.SetBytes(payload, prefix+"error.status_code", http.StatusBadGateway)
	payload, _ = sjson.SetBytes(payload, prefix+"error.message", codexEmptyIncompleteErrorMessage)
	return payload
}

// isEmptyIncompleteFailurePayload recognizes the failure payloads synthesized in this file (streaming events or non-streaming objects).
func isEmptyIncompleteFailurePayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	return gjson.GetBytes(responseFailedErrorBody(payload), "error.code").String() == codexEmptyIncompleteErrorCode
}

// rewriteEmptyIncompleteTerminal is the entry point shared by each SSE read loop: on a match it returns the rewritten
// event and response.failed type, and on no match returns the input unchanged. Callers just assign the return value back to their local variable.
func rewriteEmptyIncompleteTerminal(tracker *emptyIncompleteTracker, eventType string, data []byte, parsed gjson.Result) (string, []byte, gjson.Result) {
	tracker.Observe(eventType, parsed)
	if !tracker.IsEmptyIncomplete(eventType, parsed) {
		return eventType, data, parsed
	}
	data = synthesizeEmptyIncompleteFailureEvent(data)
	return "response.failed", data, gjson.ParseBytes(data)
}
