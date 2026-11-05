// proxy/outputguard_projection_test.go
package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/security/outputguard"
)

func defaultProjectionTestConfig() outputguard.Config {
	return outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144, // 256 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

// 1. Responses protocol streaming: test of the 3 legal semantic terminal-stage snapshots and cross-frame alias mapping
// Real deltas carry only item_id; call_id appears only at output_item.done. Full traversal of multiple content entries
func TestResponsesProjectorStreamAndTerminalSnapshots(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// Text delta
	textDelta := `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello, "}`
	if err := ObserveEvent(guard, proj, "response.output_text.delta", []byte(textDelta)); err != nil {
		t.Fatalf("text delta observe failed: %v", err)
	}

	// Thinking delta
	thinkingDelta := `{"type":"response.reasoning.delta","item_id":"msg_1","content_index":0,"delta":"thinking step 1"}`
	if err := ObserveEvent(guard, proj, "response.reasoning.delta", []byte(thinkingDelta)); err != nil {
		t.Fatalf("thinking delta observe failed: %v", err)
	}

	// Tool call delta (initially only item_id, call_id missing)
	toolDelta := `{"type":"response.function_call_arguments.delta","item_id":"item_call_1","output_index":1,"delta":"{\"path\":"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(toolDelta)); err != nil {
		t.Fatalf("tool delta observe failed: %v", err)
	}

	// function_call_arguments.done (terminal stage 1: arguments.done snapshot)
	argsDone := `{"type":"response.function_call_arguments.done","item_id":"item_call_1","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.done", []byte(argsDone)); err != nil {
		t.Fatalf("args done observe failed: %v", err)
	}

	// output_item.done (terminal stage 2: item.done snapshot; late call_id binding succeeds, no new channel created)
	itemDone := `{"type":"response.output_item.done","item":{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}}`
	if err := ObserveEvent(guard, proj, "response.output_item.done", []byte(itemDone)); err != nil {
		t.Fatalf("item done observe failed: %v", err)
	}

	// response.completed (terminal stage 3: pure response.completed terminal confirmation snapshot, containing multiple content messages and a tool)
	completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"},{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hello, "},{"type":"reasoning","thinking":"thinking step 1"}]}]}}`
	if err := ObserveEvent(guard, proj, "response.completed", []byte(completed)); err != nil {
		t.Fatalf("response completed observe failed: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped, got %s", guard.TrippedReason())
	}
}

// 2. ChatCompletions protocol: choice/toolindex/callid alias isolation, legacy function_call, and finish_reason tail-check tests
func TestChatProjectorStreamingAndNonStreaming(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// Streaming chunk 1: content + reasoning + first tool_call frame (with call_id and index) + legacy delta.function_call
	chunk1 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"foo","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_123","function":{"name":"search","arguments":"{\"q\":"}}],"function_call":{"name":"legacy_fn","arguments":"{\"a\":"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk 1 observe error: %v", err)
	}

	// Streaming chunk 2: subsequent tool_call deltas (only index, id missing; verifies that aliasMap resolves stably to the same channel) + legacy function_call delta
	chunk2 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\"}"}}],"function_call":{"arguments":"1}"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk 2 observe error: %v", err)
	}

	// Verify that different choices stay isolated with no wrong merging; also test that finish_reason triggers the EndChannel tail check
	chunkChoice1 := `{"id":"chatcmpl-1","choices":[{"index":1,"delta":{"content":"bar"},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunkChoice1)); err != nil {
		t.Fatalf("chunk choice 1 observe error: %v", err)
	}

	// Non-streaming JSON test (contains message.function_call and message.tool_calls)
	nonStream := `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"final content","tool_calls":[{"id":"call_999","type":"function","function":{"name":"calc","arguments":"{\"val\":42}"}}],"function_call":{"name":"old","arguments":"{\"x\":1}"}},"finish_reason":"stop"}]}`
	guardNonStream := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projNonStream := NewChatProjector()
	if err := ObserveEvent(guardNonStream, projNonStream, "", []byte(nonStream)); err != nil {
		t.Fatalf("non-stream chat observe error: %v", err)
	}
	if guardNonStream.IsTripped() {
		t.Fatalf("expected non-stream not tripped, got %s", guardNonStream.TrippedReason())
	}
}

// 5. Event-count contract: each upstream event counts once; unknown, control, and empty business events strictly deduct from the event budget; heartbeat comments are excluded
func TestObserveEventBudgetAccountability(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// SSE comments and ping frames do not consume the business event budget
	if err := ObserveEvent(guard, proj, "", []byte(": keep-alive ping\n")); err != nil {
		t.Fatalf("comment frame should not fail: %v", err)
	}
	if err := ObserveEvent(guard, proj, "ping", []byte("{}")); err != nil {
		t.Fatalf("ping event should not fail: %v", err)
	}

	// Construct a flood of 131072 unknown and empty business events
	for i := 0; i < outputguard.MaxRequestEvents; i++ {
		unknownEvent := `{"type":"response.unknown_control_event","id":"ctrl_1"}`
		if err := ObserveEvent(guard, proj, "response.unknown_control_event", []byte(unknownEvent)); err != nil {
			t.Fatalf("event %d unexpectedly failed: %v", i, err)
		}
	}

	// The 131073rd event must be firmly blocked by request_events
	err := ObserveEvent(guard, proj, "response.created", []byte(`{"type":"response.created"}`))
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events rejection for control/unknown flood, got %v", err)
	}
	if guard.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped reason request_events, got %s", guard.TrippedReason())
	}
}

// 6. Verify that Custom Tool Arguments String decodes only the current envelope and does not re-parse the inner JSON on every delta
func TestCustomToolArgumentsEnvelopePreservation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// Construct an unclosed arguments fragment containing escapes and a large block of text
	incompleteJSONChunk := `{"type":"response.function_call_arguments.delta","item_id":"call_custom","delta":"{\"nested_key\":\"val\\nwith\\\"quotes and unclosed brackets..."}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(incompleteJSONChunk)); err != nil {
		t.Fatalf("custom tool envelope observe error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped on unclosed inner JSON, got %s", guard.TrippedReason())
	}
}

// -----------------------------------------------------------------------------
// 03-C regression test: Chat finish_reason End tail check and isolation across all active tool_calls
// -----------------------------------------------------------------------------

// 03-C important regression test:
// On finish_reason, ChatProjector streaming tool_calls must trigger End tail checks for all active choice.tool_idx.
func TestRegression03C_ChatToolChannelNoTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	// args = 100 stray bytes + 8200 periodic bytes = 8300 bytes; at the Push checkpoint of 8192 the window [100 stray + 8092 periodic] does not hit
	args1 := prefix + periodic.String()
	chunk1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"` + args1 + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("chunk1 unexpectedly tripped: %s", guard.TrippedReason())
	}

	// chunk 2: finish_reason arrives, with no more args deltas.
	// The correct implementation runs ActionEnd on choice_0.tool_0 to trigger the tail check; at this point the latest 8192 bytes (all 8192 bytes of pure periodic content) must trigger repetition!
	chunk2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(chunk2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}

	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C supplementary test: tail check with multiple choices and multiple tools kept isolated
func TestRegression03C_MultiChoiceMultiToolIsolation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// choice 0 and choice 1 have tool 0 and tool 1 respectively
	chunk1 := `{"choices":[
		{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c0_t0","function":{"name":"f0","arguments":"normal"}}]}},
		{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_c1_t1","function":{"name":"f1","arguments":"normal"}}]}}
	]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}

	// Confirm that both tool channels are active
	if got := guard.ActiveChannelCount(); got != 2 {
		t.Fatalf("expected ActiveChannelCount=2, got %d", got)
	}

	// choice 0 finish
	chunk2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk2 error: %v", err)
	}

	// choice 1 finish
	chunk3 := `{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk3)); err != nil {
		t.Fatalf("chunk3 error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("normal multi-choice multi-tool unexpectedly tripped: %s", guard.TrippedReason())
	}
}

// 03-C supplementary test: tail check works normally when the late ID arrives
func TestRegression03C_ChatToolChannelLateArrivingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: only the tool index and partial arguments, no id
	c1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"` + args[:4000] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c1)); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// chunk 2: late call_id plus the remaining arguments
	c2 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late_1","function":{"arguments":"` + args[4000:] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c2)); err != nil {
		t.Fatalf("c2: %v", err)
	}

	// chunk 3: finish_reason arrives, triggering EndChannel for all tool_calls of choice 0
	c3 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(c3))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C supplementary test: tail check works normally when the ID is missing entirely
func TestRegression03C_ChatToolChannelMissingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: no ID throughout
	c1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"` + args + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c1)); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// chunk 2: finish_reason
	c2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(c2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// -----------------------------------------------------------------------------
// A1 regression test: terminal projection for response.incomplete (both the streaming event and the non-streaming body paths)
// -----------------------------------------------------------------------------

func TestResponsesProjectorIncompleteProjection(t *testing.T) {
	proj := NewResponsesProjector()

	assertPartial := func(t *testing.T, obs []Observation, itemID, text string) {
		t.Helper()
		if len(obs) != 1 {
			t.Fatalf("expected 1 observation for %s, got %d: %+v", itemID, len(obs), obs)
		}
		o := obs[0]
		if o.ChannelID != itemID+":content:0" || o.Kind != outputguard.KindText ||
			o.Action != ActionSnapshot || string(o.Payload) != text ||
			o.TerminalStage != "response.completed" {
			t.Fatalf("unexpected observation for %s: %+v (payload=%q)", itemID, o, o.Payload)
		}
	}

	// Streaming response.incomplete event: before the fix, the switch did not match this event, so the observation count was 0.
	streamEvent := `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"partial stream"}]}]}}`
	obs, err := proj.Project("response.incomplete", []byte(streamEvent))
	if err != nil {
		t.Fatalf("project stream incomplete: %v", err)
	}
	assertPartial(t, obs, "msg_1", "partial stream")

	// Non-streaming body (eventType empty): before the fix, default accepted only status=completed.
	nonStreamBody := `{"id":"resp_2","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg_2","type":"message","content":[{"type":"output_text","text":"partial body"}]}]}`
	obs, err = proj.Project("", []byte(nonStreamBody))
	if err != nil {
		t.Fatalf("project non-stream incomplete: %v", err)
	}
	assertPartial(t, obs, "msg_2", "partial body")
}

// -----------------------------------------------------------------------------
// A2 regression test: text and input carried directly by content_block_start must be emitted as append observations
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// A3 regression test: when the finishReason closing chunk has no parts, the preceding seen indices must still be ended
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// I1 regression test: reasoning summary and text deltas must enter the thinking channel
// -----------------------------------------------------------------------------

// I1: response.reasoning_summary_text.delta and response.reasoning_text.delta must each produce
// thinking observations; each summary part is channelized independently by summary_index (not content_index).
func TestResponsesProjectorReasoningSummaryDeltaChannels(t *testing.T) {
	cfg := defaultProjectionTestConfig()
	cfg.MaxChannelBytes = 1000

	guard := outputguard.NewRequestGuard(cfg)
	proj := NewResponsesProjector()

	summaryEvent := func(summaryIndex int, delta string) []byte {
		return []byte(fmt.Sprintf(
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_summary","output_index":0,"summary_index":%d,"delta":%q}`,
			summaryIndex, delta,
		))
	}

	// Two parts of 600 bytes each, 1200 in total > MaxChannelBytes; if they shared content_index=0
	// or shared an alias, they would be merged into a single channel and falsely trigger channel_bytes.
	for _, part := range []struct {
		index int
		delta string
	}{{0, strings.Repeat("s", 600)}, {1, strings.Repeat("t", 600)}} {
		if err := ObserveEvent(guard, proj, "response.reasoning_summary_text.delta", summaryEvent(part.index, part.delta)); err != nil {
			t.Fatalf("summary part %d observe failed: %v", part.index, err)
		}
	}
	if guard.IsTripped() {
		t.Fatalf("distinct summary_index parts were merged into one channel: %s", guard.TrippedReason())
	}
	if got := guard.ActiveChannelCount(); got != 2 {
		t.Fatalf("expected 2 independent summary channels, got %d", got)
	}

	// Continuing to append to the same summary part and exceeding the single-channel byte budget must trigger channel_bytes,
	// proving that the summary delta really entered the thinking channel budget check.
	err := ObserveEvent(guard, proj, "response.reasoning_summary_text.delta", summaryEvent(0, strings.Repeat("s", 600)))
	if err == nil || guard.TrippedReason() != "channel_bytes" {
		t.Fatalf("expected channel_bytes for summary delta overflow, got err=%v reason=%q", err, guard.TrippedReason())
	}
}

// I1: over-long periodic text in summary and reasoning-text deltas must trigger repetition detection.
func TestResponsesProjectorReasoningDeltaRepetitionDetection(t *testing.T) {
	periodic := strings.Repeat("reasoning-cycle-", 700)
	cases := []struct {
		name      string
		eventType string
		event     []byte
	}{
		{
			name:      "reasoning_summary_text",
			eventType: "response.reasoning_summary_text.delta",
			event: []byte(fmt.Sprintf(
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_repeat","output_index":0,"summary_index":0,"delta":%q}`,
				periodic,
			)),
		},
		{
			name:      "reasoning_text",
			eventType: "response.reasoning_text.delta",
			event: []byte(fmt.Sprintf(
				`{"type":"response.reasoning_text.delta","item_id":"rt_repeat","output_index":1,"content_index":0,"delta":%q}`,
				periodic,
			)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
			proj := NewResponsesProjector()
			err := ObserveEvent(guard, proj, tc.eventType, tc.event)
			if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
				t.Fatalf("%s: expected repetition trip, got err=%v tripped=%v reason=%q", tc.name, err, guard.IsTripped(), guard.TrippedReason())
			}
		})
	}
}
