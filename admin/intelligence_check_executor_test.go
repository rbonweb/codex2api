package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestIntelligenceStreamErrorsOverridePartialAnswers(t *testing.T) {
	for _, wire := range []string{
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n",
		`data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n" + `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"status":"failed"}}` + "\n\n",
	} {
		result := readIntelligenceStream(strings.NewReader(wire), false)
		if result.Complete || result.Error == "" {
			t.Fatalf("must fail incomplete stream: %+v", result)
		}
	}
}

func TestIntelligenceSuccessDoesNotClearUnrelatedAccountState(t *testing.T) {
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n")
	})
	a.Status = auth.StatusCooldown
	a.CooldownReason = "unauthorized"
	a.CooldownUtil = time.Now().Add(time.Hour)
	a.DispatchPaused = 1
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if !result.Complete || a.Status != auth.StatusCooldown || a.CooldownReason != "unauthorized" || a.DispatchPaused != 1 {
		t.Fatalf("unrelated state was reset: %+v", result)
	}
	if result.EffectiveContext["attempts"] != "1" {
		t.Fatalf("missing actual attempt audit: %+v", result.EffectiveContext)
	}
}

func TestIntelligenceFinalAnswerExcludesCommentary(t *testing.T) {
	for name, wire := range map[string]string{
		"terminal": `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"让我计算一下"}]},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"21"}]}]}}` + "\n\n",
		"streamed": `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","phase":"commentary"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":0,"delta":"让我计算一下"}` + "\n\n" + `data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant","phase":"final_answer"}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":1,"delta":"21"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			result := readIntelligenceStream(strings.NewReader(wire), false)
			if !result.Complete || result.Output != "21" {
				t.Fatalf("final answer polluted: %+v", result)
			}
		})
	}
}

func TestIntelligenceUnboundAccountIgnoresGlobalProxyPool(t *testing.T) {
	requests := 0
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprintf(w, "data: %s\n\n", intelligenceSuccess)
	})
	h.store.SetProxyPoolEnabled(true)
	h.store.SetProxyURL("http://127.0.0.1:1")
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if !result.Complete || result.Error != "" || requests != 1 {
		t.Fatalf("unbound account did not go direct: %+v calls=%d", result, requests)
	}
}

func TestIntelligenceBoundProxyFailureNeverFallsBack(t *testing.T) {
	h, a, _ := newCodexDiagnosticsTestHandler(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not fall back to direct") })
	a.ProxyURL = "http://127.0.0.1:1"
	result := h.executeIntelligenceCheck(context.Background(), a, "gpt-4o-mini", "high", "answer")
	if result.Interrupted || result.Complete || result.Error == "" {
		t.Fatalf("proxy network failure result=%+v", result)
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
