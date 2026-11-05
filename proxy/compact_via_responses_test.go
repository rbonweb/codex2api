package proxy

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAppendCompactionTriggerToResponsesBody_ArrayInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := appendCompactionTriggerToResponsesBody(body)

	if !gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("stream should be forced to true: %s", out)
	}
	if store := gjson.GetBytes(out, "store"); !store.Exists() || store.Bool() {
		t.Fatalf("store should be forced to false: %s", out)
	}
	if got := gjson.GetBytes(out, "include.0").String(); got != codexReasoningEncryptedContentInclude {
		t.Fatalf("include should add back reasoning.encrypted_content, got %q", got)
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("input should have the trigger appended to 2 items, got %d: %s", len(items), out)
	}
	if got := items[1].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("the last item should be compaction_trigger, got %q", got)
	}
}

func TestAppendCompactionTriggerToResponsesBody_NoDuplicateTrigger(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger"}]}`)
	out := appendCompactionTriggerToResponsesBody(body)

	count := 0
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("type").String() == "compaction_trigger" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the trigger should not be injected again when it is already present, got %d", count)
	}
}

func TestAppendCompactionTriggerToResponsesBody_MovesExistingTriggerToEnd(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"message","role":"user","content":"compact this"},
		{"type":"compaction_trigger"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"type":"compaction_trigger"}
	]}`)
	out := appendCompactionTriggerToResponsesBody(body)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 3 {
		t.Fatalf("duplicate triggers should be merged into 1, got %d items: %s", len(items), out)
	}
	if got := items[1].Get("type").String(); got != "function_call_output" {
		t.Fatalf("the order of non-trigger input should stay unchanged, got %q: %s", got, out)
	}
	if got := items[2].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("compaction_trigger must be at the end of input, got %q: %s", got, out)
	}
}

func TestNormalizeCompactionTriggerFinal_IgnoresNestedToolOutput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"function_call_output","call_id":"call_1","output":{"type":"compaction_trigger"}},
		{"type":"message","role":"user","content":"continue"}
	]}`)
	out := normalizeCompactionTriggerFinal(body, false)

	if string(out) != string(body) {
		t.Fatalf("nested tool output must not be treated as a protocol trigger: %s", out)
	}
}

func TestNormalizeCompactionTriggerFinal_WrapsDirectObjectInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":{"type":"compaction_trigger"}}`)
	out := normalizeCompactionTriggerFinal(body, false)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 1 {
		t.Fatalf("direct trigger object should become a one-item input array, got %d: %s", len(items), out)
	}
	if got := items[0].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("input[0].type = %q, want compaction_trigger; body=%s", got, out)
	}
}

func TestNormalizeCompactionTriggerFinal_CanonicalizesTriggerType(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"message","role":"user","content":"compact this"},
		{"type":" COMPACTION_TRIGGER "}
	]}`)
	out := normalizeCompactionTriggerFinal(body, false)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("input length = %d, want 2; body=%s", len(items), out)
	}
	if got := items[1].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("final trigger type = %q, want canonical compaction_trigger; body=%s", got, out)
	}
}

func TestAppendCompactionTriggerToResponsesBody_PreservesObjectInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":{"type":"message","role":"user","content":"compact this"}}`)
	out := appendCompactionTriggerToResponsesBody(body)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("object input should be preserved before the trigger, got %d: %s", len(items), out)
	}
	if got := items[0].Get("type").String(); got != "message" {
		t.Fatalf("input[0].type = %q, want message; body=%s", got, out)
	}
	if got := items[1].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("input[1].type = %q, want compaction_trigger; body=%s", got, out)
	}
}

func TestAppendCompactionTriggerToResponsesBody_StringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"please compact"}`)
	out := appendCompactionTriggerToResponsesBody(body)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 2 {
		t.Fatalf("string input should be converted into a message + trigger, got %d items: %s", len(items), out)
	}
	if got := items[0].Get("content.0.text").String(); got != "please compact" {
		t.Fatalf("the original text should stay in the message item, got %q", got)
	}
	if got := items[1].Get("type").String(); got != "compaction_trigger" {
		t.Fatalf("the last item should be compaction_trigger, got %q", got)
	}
}

func TestCollectCompactResponsesSSE_AggregatesCompactionItem(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.output_item.added","item":{"id":"cmp_1","type":"compaction","encrypted_content":"partial"},"output_index":0}`,
		``,
		`data: {"type":"response.output_item.done","item":{"id":"cmp_1","type":"compaction","encrypted_content":"opaque-blob"},"output_index":0}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":41,"output_tokens":38,"total_tokens":79}}}`,
		``,
	}, "\n")

	respJSON, failed, err := collectCompactResponsesSSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed != nil {
		t.Fatalf("unexpected failed payload: %s", failed)
	}
	output := gjson.GetBytes(respJSON, "output").Array()
	if len(output) != 1 {
		t.Fatalf("output should contain exactly 1 compaction item, got %d: %s", len(output), respJSON)
	}
	if got := output[0].Get("type").String(); got != "compaction" {
		t.Fatalf("output item type should be compaction, got %q", got)
	}
	if got := output[0].Get("encrypted_content").String(); got != "opaque-blob" {
		t.Fatalf("the final content of the done event should be kept, got %q", got)
	}
	if got := gjson.GetBytes(respJSON, "usage.total_tokens").Int(); got != 79 {
		t.Fatalf("usage should come from response.completed, got %d", got)
	}
}

func TestCollectCompactResponsesSSE_ReturnsFailedPayload(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		``,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`,
		``,
	}, "\n")

	respJSON, failed, err := collectCompactResponsesSSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if respJSON != nil {
		t.Fatalf("a failed stream should not return a response body: %s", respJSON)
	}
	if got := gjson.GetBytes(failed, "response.error.code").String(); got != "rate_limit_exceeded" {
		t.Fatalf("failed payload should be kept as is, got %q", got)
	}
}

func TestCollectCompactResponsesSSE_MissingTerminalIsError(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}` + "\n"

	_, _, err := collectCompactResponsesSSE(strings.NewReader(sse))
	if err == nil {
		t.Fatal("a missing terminal event should return an error")
	}
}
