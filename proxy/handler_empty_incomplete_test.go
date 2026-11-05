package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

const emptyIncompleteTerminalEvent = `{"type":"response.incomplete","response":{"id":"resp_empty","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":111,"output_tokens":0,"total_tokens":111}}}`

func writeEmptyIncompleteAttempt(w http.ResponseWriter) {
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp_empty","status":"in_progress"}}`,
		`{"type":"response.in_progress","response":{"id":"resp_empty","status":"in_progress"}}`,
		emptyIncompleteTerminalEvent,
	} {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}

func writeSuccessfulAttempt(w http.ResponseWriter) {
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp_ok","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","delta":"OK"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":111,"output_tokens":5,"total_tokens":116}}}`,
	} {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}

func invokeChatCompletionsWithBody(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ChatCompletions(ctx)
	return recorder
}

// The upstream silently aborts the first round with a zero-output response.incomplete, and the second round is normal: the gateway must treat the first round
// as a failure and retry it transparently, so downstream only sees the successful result.
func TestChatCompletionsEmptyIncompleteRetriesThenSucceeds(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			handler, calls := newChatStreamServeTestHandler(t, func(w http.ResponseWriter) {
				if attempts.Add(1) == 1 {
					writeEmptyIncompleteAttempt(w)
					return
				}
				writeSuccessfulAttempt(w)
			})
			body := `{"model":"gpt-5.5","stream":` + boolString(stream) + `,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`
			recorder := invokeChatCompletionsWithBody(t, handler, body)
			got := recorder.Body.String()
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, got)
			}
			if !strings.Contains(got, `"OK"`) {
				t.Fatalf("successful retry content missing: %q", got)
			}
			if strings.Contains(got, `"finish_reason":"length"`) {
				t.Fatalf("empty incomplete must not leak as a length finish: %q", got)
			}
			if !strings.Contains(got, `"service_tier":"default"`) {
				t.Fatalf("upstream service_tier must be echoed: %q", got)
			}
			if stream && !strings.Contains(got, "data: [DONE]") {
				t.Fatalf("successful stream must end with [DONE]: %q", got)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want 2 (empty attempt + retry)", n)
			}
		})
	}
}

// Every round aborts silently: once the retry budget is exhausted, return 502 with a stable error code, not 200 with empty content.
func TestChatCompletionsEmptyIncompleteExhaustedReturns502(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			handler, calls := newChatStreamServeTestHandler(t, writeEmptyIncompleteAttempt)
			body := `{"model":"gpt-5.5","stream":` + boolString(stream) + `,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`
			recorder := invokeChatCompletionsWithBody(t, handler, body)
			got := recorder.Body.String()
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", recorder.Code, got)
			}
			if !strings.Contains(got, "0 output tokens") {
				t.Fatalf("error message must name the empty incomplete failure: %q", got)
			}
			if strings.Contains(got, "[DONE]") || strings.Contains(got, `"finish_reason":"length"`) {
				t.Fatalf("exhausted empty incomplete must not look like a successful completion: %q", got)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want 2 (initial + MaxRetries=1)", n)
			}
		})
	}
}

// A real max_output_tokens truncation (with output and tokens) must keep the normal terminal semantics of v2.8.3.
func TestChatCompletionsRealTruncationStillReportsLength(t *testing.T) {
	handler, calls := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"response.created","response":{"id":"resp_trunc"}}`,
		`{"type":"response.output_text.delta","delta":"half"}`,
		`{"type":"response.incomplete","response":{"id":"resp_trunc","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"half"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
	})
	recorder := invokeChatCompletionsStream(t, handler)
	got := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(got, `"finish_reason":"length"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("real truncation must stay a normal length terminal: status=%d body=%q", recorder.Code, got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry for real truncation)", n)
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func invokeResponsesWithBody(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)
	return recorder
}

// The native /v1/responses path must also treat a zero-output incomplete as a failure and retry it.
func TestResponsesEmptyIncompleteRetriesThenSucceeds(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			handler, calls := newChatStreamServeTestHandler(t, func(w http.ResponseWriter) {
				if attempts.Add(1) == 1 {
					writeEmptyIncompleteAttempt(w)
					return
				}
				writeSuccessfulAttempt(w)
			})
			body := `{"model":"gpt-5.5","stream":` + boolString(stream) + `,"input":"Reply with exactly: OK"}`
			recorder := invokeResponsesWithBody(t, handler, body)
			got := recorder.Body.String()
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, got)
			}
			if !strings.Contains(got, `"OK"`) || !strings.Contains(got, `"completed"`) {
				t.Fatalf("successful retry payload missing: %q", got)
			}
			if strings.Contains(got, `response.incomplete`) || strings.Contains(got, `"status":"incomplete"`) {
				t.Fatalf("empty incomplete must not leak downstream: %q", got)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want 2", n)
			}
		})
	}
}

func TestResponsesEmptyIncompleteExhaustedReturns502(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			handler, calls := newChatStreamServeTestHandler(t, writeEmptyIncompleteAttempt)
			body := `{"model":"gpt-5.5","stream":` + boolString(stream) + `,"input":"Reply with exactly: OK"}`
			recorder := invokeResponsesWithBody(t, handler, body)
			got := recorder.Body.String()
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body=%q", recorder.Code, got)
			}
			if !strings.Contains(got, "0 output tokens") {
				t.Fatalf("error message must name the empty incomplete failure: %q", got)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want 2", n)
			}
		})
	}
}
