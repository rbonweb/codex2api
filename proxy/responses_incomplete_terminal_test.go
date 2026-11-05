package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

// response.incomplete is the normal terminal state for truncation by max_output_tokens. Missing it would make the finalize logic
// treat the normal truncation as a broken stream: it would synthesize a fake response.failed / overloaded_error, discard the real
// usage and switch to estimation, and penalise the account as a broken stream. The cases below lock in the handling at each protocol egress.

const incompleteTerminalEvent = `{
	"type":"response.incomplete",
	"response":{
		"status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],
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

// Downstream /v1/chat/completions: truncation maps to finish_reason=length, and it must be a terminal event,
// otherwise the finalize logic would append another error object.
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

// When a tool call is truncated, finish_reason must not stay at tool_calls: that would make downstream treat the half-finished arguments
// as an executable complete call.
func TestStatefulChatStreamTranslatorIncompleteBeatsToolCalls(t *testing.T) {
	st := &StreamTranslator{ChunkID: "chatcmpl-x", Model: "gpt-5", Created: 1, HasToolCalls: true}
	chunk, _ := st.TranslateParsed(gjson.Parse(incompleteTerminalEvent))
	if got := gjson.GetBytes(chunk, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

func TestBuildCompactResponseFinishReasonOverride(t *testing.T) {
	body := BuildCompactResponseWithFinishReason("chatcmpl-x", "gpt-5", 1, "partial", "", nil, nil, "length")
	if got := gjson.GetBytes(body, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
	// An empty override keeps the originally derived value, for compatibility with existing callers.
	body = BuildCompactResponseWithFinishReason("chatcmpl-x", "gpt-5", 1, "complete", "", nil, nil, "")
	if got := gjson.GetBytes(body, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}
