package proxy

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Custom input is opaque text, even when that text happens to be valid JSON.
type customToolCallData struct {
	Name  string `json:"name,omitempty"`
	Input string `json:"input"`
}

func lowerChatCustomTool(tool map[string]any) map[string]any {
	if tool["type"] != "custom" {
		return tool
	}
	nested, ok := tool["custom"].(map[string]any)
	if !ok {
		return tool
	}
	lowered := make(map[string]any, len(nested)+1)
	for key, value := range nested {
		lowered[key] = value
	}
	lowered["type"] = "custom"
	return lowered
}

func newCustomToolChunk(id, model string, created int64, index int, callID, name, input, namespace string) []byte {
	tool := toolCallDelta{Index: index, ID: callID, Custom: &customToolCallData{Name: name, Input: input}, Namespace: namespace}
	role := ""
	if callID != "" || name != "" {
		tool.Type = "custom"
		role = "assistant"
	}
	chunk := openAIStreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []streamChoice{{Index: 0, Delta: &streamDelta{Role: role, ToolCalls: []toolCallDelta{tool}}}}}
	raw, _ := json.Marshal(chunk)
	return raw
}

func (st *StreamTranslator) customToolInput(index int) string {
	if buffer := st.customToolInputs[index]; buffer != nil {
		return buffer.String()
	}
	return ""
}

func (st *StreamTranslator) appendCustomToolInput(index int, delta string) ([]byte, bool) {
	if st.toolCallFinalized[index] {
		return st.failToolArguments(index, "continued after the done event")
	}
	buffer := st.customToolInputs[index]
	if buffer == nil {
		buffer = &strings.Builder{}
		st.customToolInputs[index] = buffer
	}
	if len(delta) > responseCacheMaxEntry-buffer.Len() {
		return st.failToolArguments(index, "exceed the tool input size limit")
	}
	buffer.WriteString(delta)
	return newCustomToolChunk(st.ChunkID, st.Model, st.Created, index, "", "", delta, ""), false
}

func (st *StreamTranslator) finalizeCustomToolInput(index int, final gjson.Result) ([]byte, bool) {
	current := st.customToolInput(index)
	candidate := current
	if final.Exists() {
		if final.Type != gjson.String {
			return st.failToolArguments(index, "must contain text input")
		}
		if final.String() != "" {
			candidate = final.String()
		}
	}
	if len(candidate) > responseCacheMaxEntry {
		return st.failToolArguments(index, "exceed the tool input size limit")
	}
	if !strings.HasPrefix(candidate, current) {
		return st.failToolArguments(index, "do not match the streamed deltas")
	}
	if candidate == current {
		st.toolCallFinalized[index] = true
		return nil, false
	}
	suffix := candidate[len(current):]
	chunk, done := st.appendCustomToolInput(index, suffix)
	st.toolCallFinalized[index] = true
	return chunk, done
}
