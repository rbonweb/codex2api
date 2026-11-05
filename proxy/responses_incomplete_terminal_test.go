package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

// response.incomplete 是 max_output_tokens 截断的正常终态。漏认它会让收尾逻辑
// 把正常截断当断流：合成假的 response.failed / overloaded_error、丢弃真实
// usage 改用估算、并按断流惩罚账号。以下用例锁住各协议出口的处理。

const incompleteTerminalEvent = `{
	"type":"response.incomplete",
	"response":{
		"status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"半截"}]}],
		"usage":{"input_tokens":232,"output_tokens":6631,"input_tokens_details":{"cached_tokens":0}}
	}
}`

func TestIsResponsesTerminalEventCoversIncomplete(t *testing.T) {
	for _, tc := range []struct {
		event    string
		success  bool
		terminal bool
	}{
		{"response.completed", true, true},
		{"response.incomplete", true, true},
		{"response.failed", false, true},
		{"response.output_text.delta", false, false},
	} {
		if got := isResponsesSuccessTerminalEvent(tc.event); got != tc.success {
			t.Errorf("isResponsesSuccessTerminalEvent(%q) = %v, want %v", tc.event, got, tc.success)
		}
		if got := isResponsesTerminalEvent(tc.event); got != tc.terminal {
			t.Errorf("isResponsesTerminalEvent(%q) = %v, want %v", tc.event, got, tc.terminal)
		}
	}
}

func TestResponsesIncompleteFinishReason(t *testing.T) {
	for _, tc := range []struct {
		event  string
		reason string
		want   string
	}{
		{"response.incomplete", "max_output_tokens", "length"},
		{"response.incomplete", "content_filter", "content_filter"},
		{"response.incomplete", "", "length"},
		{"response.completed", "max_output_tokens", ""},
	} {
		if got := responsesIncompleteFinishReason(tc.event, tc.reason); got != tc.want {
			t.Errorf("responsesIncompleteFinishReason(%q, %q) = %q, want %q", tc.event, tc.reason, got, tc.want)
		}
	}
}


// 下游 /v1/chat/completions：截断对应 finish_reason=length，且必须是终态事件，
// 否则收尾逻辑会再补一个 error 对象。
func TestChatStreamTranslatorMapsIncompleteToLength(t *testing.T) {
	chunk, terminal := TranslateStreamChunk([]byte(incompleteTerminalEvent), "gpt-5", "chatcmpl-x", 1)
	if !terminal {
		t.Fatal("response.incomplete must be terminal")
	}
	if got := gjson.GetBytes(chunk, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
	if got := gjson.GetBytes(chunk, "usage.completion_tokens").Int(); got != 6631 {
		t.Errorf("completion_tokens = %d, want 6631", got)
	}
}

func TestStatefulChatStreamTranslatorMapsIncompleteToLength(t *testing.T) {
	st := &StreamTranslator{ChunkID: "chatcmpl-x", Model: "gpt-5", Created: 1}
	chunk, terminal := st.TranslateParsed(gjson.Parse(incompleteTerminalEvent))
	if !terminal {
		t.Fatal("response.incomplete must be terminal")
	}
	if got := gjson.GetBytes(chunk, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

// 工具调用截断时 finish_reason 不能停留在 tool_calls：那会让下游把半截参数
// 当成可执行的完整调用。
func TestStatefulChatStreamTranslatorIncompleteBeatsToolCalls(t *testing.T) {
	st := &StreamTranslator{ChunkID: "chatcmpl-x", Model: "gpt-5", Created: 1, HasToolCalls: true}
	chunk, _ := st.TranslateParsed(gjson.Parse(incompleteTerminalEvent))
	if got := gjson.GetBytes(chunk, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

func TestBuildCompactResponseFinishReasonOverride(t *testing.T) {
	body := BuildCompactResponseWithFinishReason("chatcmpl-x", "gpt-5", 1, "半截", "", nil, nil, "length")
	if got := gjson.GetBytes(body, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
	// 空覆盖保持原推导值，兼容既有调用方。
	body = BuildCompactResponseWithFinishReason("chatcmpl-x", "gpt-5", 1, "完整", "", nil, nil, "")
	if got := gjson.GetBytes(body, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}

