package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/codex2api/proxy/internal/jsonbody"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== compact endpoint body-signal compatibility ====================
// The upstream has retired the dedicated /responses/compact endpoint (it returns 404), so session compaction moves to the regular
// /responses request body, which embeds an input item with type=compaction_trigger (the body-signal form of the newer Codex CLI
// body-signal form). When the system setting compact_via_responses_enabled is enabled, official
// Codex OAuth accounts' /v1/responses/compact pass-through is rewritten into this form: the trigger is injected and goes through
// /responses SSE, and the stream is then aggregated back into the one-shot compact JSON returned downstream. Relay (OpenAI
// Responses API) accounts are not affected and still request their own relay /responses/compact.

// appendCompactionTriggerToResponsesBody rewrites the prepared Codex compact body into
// the body-signal compaction form. Compact preprocessing for the old dedicated endpoint stripped store/stream/include,
// but the /responses upstream requires stream=true and store=false (measured: a missing store directly
// 400 "Store must be set to false"), so they are restored here, and appended at the end of input
// compaction_trigger. If the client already carries the trigger, it is normalized to the last input item too.
func appendCompactionTriggerToResponsesBody(body []byte) []byte {
	out, _ := sjson.SetBytes(body, "stream", true)
	out, _ = sjson.SetBytes(out, "store", false)
	if !gjson.GetBytes(out, "include").Exists() {
		out, _ = sjson.SetBytes(out, "include", []string{codexReasoningEncryptedContentInclude})
	}
	return normalizeCompactionTriggerFinal(out, true)
}

// normalizeCompactionTriggerFinal keeps at most one direct input-level
// compaction_trigger and moves it behind every history/message/tool item.
// Upstream rejects a trigger followed by any other input item with
// "The 'compaction_trigger' item must be the final input item."
//
// addIfMissing is true only when rewriting the legacy /responses/compact
// endpoint to body-signal compaction. Plain /responses preparation uses false:
// it repairs the order only when the client already supplied a trigger.
func normalizeCompactionTriggerFinal(body []byte, addIfMissing bool) []byte {
	trigger := json.RawMessage(`{"type":"compaction_trigger"}`)
	input := jsonbody.GetView(body, "input")
	switch {
	case input.IsArray():
		items := input.Array()
		triggerCount := 0
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				triggerCount++
			}
		}
		if triggerCount == 0 && !addIfMissing {
			return body
		}
		if triggerCount == 1 && len(items) > 0 && isCanonicalCompactionTrigger(items[len(items)-1]) {
			return body
		}

		normalized := make([]json.RawMessage, 0, len(items)+1)
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				continue
			}
			raw := strings.TrimSpace(item.Raw)
			if raw == "" {
				encoded, err := json.Marshal(item.Value())
				if err != nil {
					return body
				}
				raw = string(encoded)
			}
			normalized = append(normalized, json.RawMessage(raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case input.IsObject() && isDirectCompactionTrigger(input):
		encoded, err := json.Marshal([]json.RawMessage{trigger})
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case addIfMissing:
		normalized := make([]json.RawMessage, 0, 2)
		switch {
		case input.Type == gjson.String:
			// When input is a string, first convert it into a standard message item, then append the trigger.
			msg, err := json.Marshal(map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": input.String()},
				},
			})
			if err != nil {
				return body
			}
			normalized = append(normalized, json.RawMessage(msg))
		case input.IsObject() && !isDirectCompactionTrigger(input):
			normalized = append(normalized, json.RawMessage(input.Raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}

func isDirectCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() &&
		strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction_trigger")
}

func isCanonicalCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() && item.Get("type").String() == "compaction_trigger"
}

// collectCompactResponsesSSE aggregates the upstream /responses SSE stream of the body-signal compaction into
// a one-shot JSON for the compact endpoint: it takes the response object from response.completed, and uses the
// response.output_item.done events (including the compaction item) to fill in output. When the upstream ends with response.failed,
// the payload of that event is returned unchanged for the caller to handle as an upstream error; a stream that ends before a terminal event is treated as a read error.
func collectCompactResponsesSSE(body io.Reader) (responseJSON []byte, failedPayload []byte, err error) {
	return collectCompactResponsesSSEWithContinuousRetryKeepalive(context.Background(), body)
}

// collectCompactResponsesSSEWithContinuousRetryKeepalive aggregates the upstream compact SSE,
// and, while reading, refreshes the downstream connection through request-level keepalive.
func collectCompactResponsesSSEWithContinuousRetryKeepalive(ctx context.Context, body io.Reader) (responseJSON []byte, failedPayload []byte, err error) {
	outputItems := make([]json.RawMessage, 0, 2)
	seenOutputItems := make(map[string]struct{})
	var completed []byte
	readErr := readSSEStreamWithContinuousRetryKeepalive(ctx, body, func(_ string, data []byte) bool {
		if item, ok := extractResponseOutputItemDone(data, seenOutputItems); ok {
			outputItems = append(outputItems, item)
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed":
			completed = append([]byte(nil), data...)
			return false
		case "response.failed":
			failedPayload = append([]byte(nil), data...)
			return false
		}
		return true
	})
	if readErr != nil {
		return nil, nil, readErr
	}
	if len(failedPayload) > 0 {
		return nil, failedPayload, nil
	}
	if len(completed) == 0 {
		return nil, nil, errors.New("upstream stream ended before terminal event")
	}
	responseObj := gjson.GetBytes(completed, "response")
	if !responseObj.Exists() || !responseObj.IsObject() {
		return nil, nil, errors.New("response.completed missing response object")
	}
	return restoreMissingResponseOutputs([]byte(responseObj.Raw), outputItems), nil, nil
}
