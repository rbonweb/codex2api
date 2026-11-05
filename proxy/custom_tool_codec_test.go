package proxy

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func customToolTestEvent(eventType string, fields map[string]any) []byte {
	fields["type"] = eventType
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestCustomToolChatRoundTrip(t *testing.T) {
	for _, input := range []string{"print(\"你好\")\ntext('\\path')", "", `{"input":"already JSON but still a custom text input"}`} {
		t.Run(input, func(t *testing.T) {
			st := NewStreamTranslator("chatcmpl-test", "gpt-6-astra", 0)
			added, _ := st.Translate(customToolTestEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc_test", "call_id": "call_test", "name": "exec"}}))
			if gjson.GetBytes(added, "choices.0.delta.tool_calls.0.type").String() != "custom" {
				t.Fatalf("custom tool was retyped: %s", added)
			}
			var reconstructed strings.Builder
			runes := []rune(input)
			for _, fragment := range []string{string(runes[:len(runes)/2]), string(runes[len(runes)/2:])} {
				chunk, _ := st.Translate(customToolTestEvent("response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc_test", "delta": fragment}))
				reconstructed.WriteString(gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.custom.input").String())
			}
			chunk, done := st.Translate(customToolTestEvent("response.custom_tool_call_input.done", map[string]any{"item_id": "ctc_test", "input": input}))
			if done || len(chunk) != 0 {
				t.Fatalf("done duplicated input or failed: %s", chunk)
			}
			if reconstructed.String() != input {
				t.Fatal("custom input changed during streaming")
			}
			request := map[string]any{"model": "gpt-6-astra", "tools": []any{map[string]any{"type": "custom", "custom": map[string]any{"name": "exec", "format": map[string]any{"type": "text"}}}}, "tool_choice": map[string]any{"type": "custom", "custom": map[string]any{"name": "exec"}}, "messages": []any{
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_test", "type": "custom", "custom": map[string]any{"name": "exec", "input": input}}}},
				map[string]any{"role": "tool", "tool_call_id": "call_test", "content": "ok"},
			}}
			raw, _ := json.Marshal(request)
			translated, err := TranslateRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(translated, "input.0.type").String() != "custom_tool_call" || gjson.GetBytes(translated, "input.0.input").String() != input || gjson.GetBytes(translated, "input.1.type").String() != "custom_tool_call_output" {
				t.Fatalf("custom history lost: %s", translated)
			}
			if gjson.GetBytes(translated, "tools.0.name").String() != "exec" || gjson.GetBytes(translated, "tool_choice.name").String() != "exec" {
				t.Fatalf("custom declaration or choice was not lowered: %s", translated)
			}
			completed := customToolTestEvent("response.completed", map[string]any{"response": map[string]any{"output": []any{map[string]any{"type": "custom_tool_call", "call_id": "call_test", "name": "exec", "input": input}}}})
			calls, err := ExtractToolCallsFromOutputValidated(completed)
			if err != nil {
				t.Fatal(err)
			}
			compact := BuildCompactResponse("chatcmpl-test", "gpt-6-astra", 0, "", "", calls, nil)
			if gjson.GetBytes(compact, "choices.0.message.tool_calls.0.type").String() != "custom" || gjson.GetBytes(compact, "choices.0.message.tool_calls.0.custom.input").String() != input {
				t.Fatalf("custom compact output lost: %s", compact)
			}
		})
	}
}

func TestCustomToolStreamReconcilesDone(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-6-astra", 0)
	st.Translate([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec"}}`))
	st.Translate([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_test","delta":"print("}`))
	chunk, done := st.Translate([]byte(`{"type":"response.custom_tool_call_input.done","item_id":"ctc_test","input":"print(1)"}`))
	if done || gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.custom.input").String() != "1)" {
		t.Fatalf("missing suffix not restored: %s", chunk)
	}
	_, done = st.Translate([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_test","delta":"poison"}`))
	if !done || st.ToolArgumentsError() == nil {
		t.Fatal("input continued after done")
	}
}

func TestCustomToolChatNonStreamRebuildsEmptyTerminalOutput(t *testing.T) {
	handler, calls := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec","input":"print(\"你好\")"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"run the custom tool"}],"tools":[{"type":"custom","custom":{"name":"exec","format":{"type":"text"}}}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ChatCompletions(ctx)
	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("unexpected request outcome: %d %s", recorder.Code, recorder.Body.String())
	}
	tool := gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.tool_calls.0")
	if tool.Get("type").String() != "custom" || tool.Get("custom.input").String() != `print("你好")` {
		t.Fatalf("custom call was lost from the empty completed output: %s", recorder.Body.String())
	}
}
