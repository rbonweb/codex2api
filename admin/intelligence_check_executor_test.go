package admin

import (
	"strings"
	"testing"
)

func TestIntelligenceStreamErrorsOverridePartialAnswers(t *testing.T) {
	for _, wire := range []string{
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n",
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n" + `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"status":"failed"}}` + "\n\n",
	} {
		result := readIntelligenceStream(strings.NewReader(wire))
		if result.Complete || result.Error == "" {
			t.Fatalf("must fail incomplete stream: %+v", result)
		}
	}
}

func TestIntelligenceFinalAnswerExcludesCommentary(t *testing.T) {
	for name, wire := range map[string]string{
		"terminal": `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"让我计算一下"}]},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"21"}]}]}}` + "\n\n",
		"streamed": `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","phase":"commentary"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":0,"delta":"让我计算一下"}` + "\n\n" + `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","phase":"final_answer"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":1,"delta":"21"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			result := readIntelligenceStream(strings.NewReader(wire))
			if !result.Complete || result.Output != "21" {
				t.Fatalf("final answer polluted: %+v", result)
			}
		})
	}
}

func TestIntelligenceCredentialRejected(t *testing.T) {
	for _, raw := range []string{`{"error":{"code":"token_revoked"}}`, `{"response":{"error":{"code":"account_deactivated"}}}`} {
		if !intelligenceCredentialRejected([]byte(raw)) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{`{"error":{"code":"server_error"}}`, `{"error":{"code":"token_expired"}}`, `{"output":"token_revoked"}`, `invalid JSON`} {
		if intelligenceCredentialRejected([]byte(raw)) {
			t.Fatal("transient or generated text blocked credentials", raw)
		}
	}
}
