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

// 1. Responses 协议流式、3 个合法语义终态阶段快照与 alias 跨帧映射测试
// 真实 delta 只有 item_id，output_item.done 才出现 call_id；多 content 完整遍历
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

	// Tool call delta (初始仅有 item_id，缺失 call_id)
	toolDelta := `{"type":"response.function_call_arguments.delta","item_id":"item_call_1","output_index":1,"delta":"{\"path\":"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(toolDelta)); err != nil {
		t.Fatalf("tool delta observe failed: %v", err)
	}

	// function_call_arguments.done (终态阶段 1: arguments.done 快照)
	argsDone := `{"type":"response.function_call_arguments.done","item_id":"item_call_1","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.done", []byte(argsDone)); err != nil {
		t.Fatalf("args done observe failed: %v", err)
	}

	// output_item.done (终态阶段 2: item.done 快照，晚到 call_id 绑定成功，不新建 channel)
	itemDone := `{"type":"response.output_item.done","item":{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}}`
	if err := ObserveEvent(guard, proj, "response.output_item.done", []byte(itemDone)); err != nil {
		t.Fatalf("item done observe failed: %v", err)
	}

	// response.completed (终态阶段 3: response.completed 纯终态确认快照，含多 content message 与 tool)
	completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"},{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hello, "},{"type":"reasoning","thinking":"thinking step 1"}]}]}}`
	if err := ObserveEvent(guard, proj, "response.completed", []byte(completed)); err != nil {
		t.Fatalf("response completed observe failed: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped, got %s", guard.TrippedReason())
	}
}

// 2. ChatCompletions 协议 choice/toolindex/callid alias 隔离、legacy function_call 及 finish_reason 尾检测试
func TestChatProjectorStreamingAndNonStreaming(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// 流式 Chunk 1: content + reasoning + tool_call 首帧 (带 call_id 与 index) + legacy delta.function_call
	chunk1 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"foo","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_123","function":{"name":"search","arguments":"{\"q\":"}}],"function_call":{"name":"legacy_fn","arguments":"{\"a\":"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk 1 observe error: %v", err)
	}

	// 流式 Chunk 2: tool_call 后续 delta (仅带 index, 缺失 id, 验证 aliasMap 稳定解析到同一 channel) + legacy function_call delta
	chunk2 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\"}"}}],"function_call":{"arguments":"1}"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk 2 observe error: %v", err)
	}

	// 验证不同 choice 隔离，不发生错合并；并测试 finish_reason 触发 EndChannel 尾检
	chunkChoice1 := `{"id":"chatcmpl-1","choices":[{"index":1,"delta":{"content":"bar"},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunkChoice1)); err != nil {
		t.Fatalf("chunk choice 1 observe error: %v", err)
	}

	// 非流式 JSON 测试 (含 message.function_call 与 message.tool_calls)
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

// 5. 事件计数契约：每上游事件一次，unknown/control/empty 业务事件严格扣减事件预算，心跳注释除外
func TestObserveEventBudgetAccountability(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// SSE 注释与 ping 帧不消耗业务事件预算
	if err := ObserveEvent(guard, proj, "", []byte(": keep-alive ping\n")); err != nil {
		t.Fatalf("comment frame should not fail: %v", err)
	}
	if err := ObserveEvent(guard, proj, "ping", []byte("{}")); err != nil {
		t.Fatalf("ping event should not fail: %v", err)
	}

	// 构造 131072 个 unknown / empty 业务事件洪泛
	for i := 0; i < outputguard.MaxRequestEvents; i++ {
		unknownEvent := `{"type":"response.unknown_control_event","id":"ctrl_1"}`
		if err := ObserveEvent(guard, proj, "response.unknown_control_event", []byte(unknownEvent)); err != nil {
			t.Fatalf("event %d unexpectedly failed: %v", i, err)
		}
	}

	// 第 131073 个事件必须被 request_events 坚决拦截
	err := ObserveEvent(guard, proj, "response.created", []byte(`{"type":"response.created"}`))
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events rejection for control/unknown flood, got %v", err)
	}
	if guard.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped reason request_events, got %s", guard.TrippedReason())
	}
}

// 6. 验证 Custom Tool Arguments String 仅解码当前 envelope，不每 delta 重 parse inner JSON
func TestCustomToolArgumentsEnvelopePreservation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// 构造未闭合且包含转义与大段文本的 arguments 片段
	incompleteJSONChunk := `{"type":"response.function_call_arguments.delta","item_id":"call_custom","delta":"{\"nested_key\":\"val\\nwith\\\"quotes and unclosed brackets..."}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(incompleteJSONChunk)); err != nil {
		t.Fatalf("custom tool envelope observe error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped on unclosed inner JSON, got %s", guard.TrippedReason())
	}
}

// -----------------------------------------------------------------------------
// 03-C 回归测试：Chat finish_reason 全部活跃 tool_calls End 尾检与隔离
// -----------------------------------------------------------------------------

// 03-C Important 回归测试：
// ChatProjector 流式 tool_calls 在 finish_reason 时必须对所有活跃的 choice.tool_idx 触发 End 进行尾检。
func TestRegression03C_ChatToolChannelNoTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	// args = 100 杂散 + 8200 周期 = 8300 字节；Push 检查点 8192 处窗口=[100杂散+8092周期] 未命中
	args1 := prefix + periodic.String()
	chunk1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"` + args1 + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("chunk1 unexpectedly tripped: %s", guard.TrippedReason())
	}

	// chunk2: finish_reason 到达，无更多 args delta。
	// 正确实现对 choice_0.tool_0 执行 ActionEnd 触发尾检，此时最新 8192 字节（包含 8192 字节纯周期）必须触发 repetition！
	chunk2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(chunk2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}

	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C 补充测试：多 choice 与多 tool 隔离尾检测试
func TestRegression03C_MultiChoiceMultiToolIsolation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// choice 0 和 choice 1 分别有 tool 0 和 tool 1
	chunk1 := `{"choices":[
		{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c0_t0","function":{"name":"f0","arguments":"normal"}}]}},
		{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_c1_t1","function":{"name":"f1","arguments":"normal"}}]}}
	]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}

	// 确认两个 tool channel 均活跃
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

// 03-C 补充测试：晚到 ID 场景尾检正常生效
func TestRegression03C_ChatToolChannelLateArrivingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: 仅有 tool index 与部分参数，无 id
	c1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"` + args[:4000] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c1)); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// chunk 2: 晚到 call_id 与剩余参数
	c2 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late_1","function":{"arguments":"` + args[4000:] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c2)); err != nil {
		t.Fatalf("c2: %v", err)
	}

	// chunk 3: finish_reason 到达，触发 choice 0 的所有 tool_calls EndChannel
	c3 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(c3))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C 补充测试：完全缺失 ID 场景尾检正常生效
func TestRegression03C_ChatToolChannelMissingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: 全程无 ID
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
// A1 回归测试：response.incomplete 终态投影（流式事件与非流式 body 两条路径）
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

	// 流式 response.incomplete 事件：修复前 switch 不匹配该事件，观察数为 0。
	streamEvent := `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"partial stream"}]}]}}`
	obs, err := proj.Project("response.incomplete", []byte(streamEvent))
	if err != nil {
		t.Fatalf("project stream incomplete: %v", err)
	}
	assertPartial(t, obs, "msg_1", "partial stream")

	// 非流式 body（eventType 为空）：修复前 default 仅接受 status=completed。
	nonStreamBody := `{"id":"resp_2","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg_2","type":"message","content":[{"type":"output_text","text":"partial body"}]}]}`
	obs, err = proj.Project("", []byte(nonStreamBody))
	if err != nil {
		t.Fatalf("project non-stream incomplete: %v", err)
	}
	assertPartial(t, obs, "msg_2", "partial body")
}

// -----------------------------------------------------------------------------
// A2 回归测试：content_block_start 直接携带的 text/input 必须作为 append 观察
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// A3 回归测试：finishReason 收尾 chunk 无 parts 时也必须结束前序已见索引
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// I1 回归测试：reasoning summary/text delta 必须进入 thinking 通道
// -----------------------------------------------------------------------------

// I1: response.reasoning_summary_text.delta 与 response.reasoning_text.delta 必须产生
// thinking 观察；summary 各 part 按 summary_index（而非 content_index）独立分通道。
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

	// 两个 part 各 600 字节，合计 1200 > MaxChannelBytes；若共享 content_index=0
	// 或共享 alias，就会被合并成单通道而误触发 channel_bytes。
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

	// 同一个 summary part 继续追加并突破单通道字节预算，必须命中 channel_bytes，
	// 证明 summary delta 真正进入了 thinking 通道预算检查。
	err := ObserveEvent(guard, proj, "response.reasoning_summary_text.delta", summaryEvent(0, strings.Repeat("s", 600)))
	if err == nil || guard.TrippedReason() != "channel_bytes" {
		t.Fatalf("expected channel_bytes for summary delta overflow, got err=%v reason=%q", err, guard.TrippedReason())
	}
}

// I1: summary / reasoning-text delta 的超长周期文本必须命中 repetition 检测。
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
