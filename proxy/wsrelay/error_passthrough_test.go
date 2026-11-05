package wsrelay

import (
	"bytes"
	"io"
	"testing"

	"github.com/tidwall/gjson"
)

// TestBuildErrorEvent_UpstreamErrorBecomesFailedEvent verifies that an upstream error frame is converted
// into a response.failed SSE event that downstream can recognize, and keeps the original error content.
func TestBuildErrorEvent_UpstreamErrorBecomesFailedEvent(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"Unsupported parameter: prompt_cache_retention"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if gjson.GetBytes(event, "type").String() != "response.failed" {
		t.Fatalf("event type = %q, want response.failed", gjson.GetBytes(event, "type").String())
	}
	if createdAt := gjson.GetBytes(event, "response.created_at").Int(); createdAt <= 0 {
		t.Fatalf("response.created_at = %d, want a positive Unix timestamp", createdAt)
	}
	// The original error content should be kept
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "Unsupported parameter: prompt_cache_retention" {
		t.Fatalf("error message not preserved: %q", msg)
	}
}

// TestBuildErrorEvent_UpstreamStatusBecomesResponseStatusCode verifies an upstream
// top-level status is copied to response.status_code even when the preserved
// upstream error object does not contain status_code itself.
func TestBuildErrorEvent_UpstreamStatusBecomesResponseStatusCode(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"too many requests","param":"input"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if statusCode := gjson.GetBytes(event, "response.status_code").Int(); statusCode != 429 {
		t.Fatalf("response.status_code = %d, want 429; event=%s", statusCode, event)
	}
	if gjson.GetBytes(event, "response.error.status_code").Exists() {
		t.Fatalf("response.error should preserve upstream error object without injecting status_code: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "too many requests" {
		t.Fatalf("response.error.message = %q, want preserved upstream message", msg)
	}
	if typ := gjson.GetBytes(event, "response.error.type").String(); typ != "rate_limit_error" {
		t.Fatalf("response.error.type = %q, want preserved upstream type", typ)
	}
	if param := gjson.GetBytes(event, "response.error.param").String(); param != "input" {
		t.Fatalf("response.error.param = %q, want preserved upstream param", param)
	}
}

// TestBuildErrorEvent_UpstreamStatusCodeBecomesResponseStatusCode verifies an
// upstream top-level status_code is copied to response.status_code while the
// original response.error payload remains unchanged.
func TestBuildErrorEvent_UpstreamStatusCodeBecomesResponseStatusCode(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status_code":503,"error":{"message":"service unavailable","code":"server_error"}}`)

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if statusCode := gjson.GetBytes(event, "response.status_code").Int(); statusCode != 503 {
		t.Fatalf("response.status_code = %d, want 503; event=%s", statusCode, event)
	}
	if gjson.GetBytes(event, "response.error.status_code").Exists() {
		t.Fatalf("response.error should preserve upstream error object without injecting status_code: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "service unavailable" {
		t.Fatalf("response.error.message = %q, want preserved upstream message", msg)
	}
	if code := gjson.GetBytes(event, "response.error.code").String(); code != "server_error" {
		t.Fatalf("response.error.code = %q, want preserved upstream code", code)
	}
}

// TestBuildErrorEvent_NonErrorPassthrough verifies that non-error frames are not recognized as errors.
func TestBuildErrorEvent_NonErrorPassthrough(t *testing.T) {
	r := &WsResponse{}
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.created","response":{}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(``),
		[]byte(`{}`),
	} {
		if _, isErr := r.buildErrorEvent(payload); isErr {
			t.Fatalf("non-error payload wrongly flagged as error: %s", payload)
		}
	}
}

// TestBuildErrorEvent_PrettyPrintedErrorCompacted verifies that an upstream pretty-printed (multi-line) error object
// is compacted to a single line before being embedded in the event. Without compaction, the SSE data: line encoding is cut off at the first newline,
// downstream only sees `..."error":{` and loses the specific error message.
func TestBuildErrorEvent_PrettyPrintedErrorCompacted(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte("{\"type\":\"error\",\"status\":400,\"error\":{\n  \"message\": \"Invalid type for 'input[61].arguments': expected an object, but got a string instead.\",\n  \"type\": \"invalid_request_error\",\n  \"param\": \"input[61].arguments\",\n  \"code\": \"invalid_type\"\n}}")

	event, isErr := r.buildErrorEvent(upstream)
	if !isErr {
		t.Fatal("expected error frame to be detected")
	}
	if bytes.IndexByte(event, '\n') >= 0 {
		t.Fatalf("event must be single-line for SSE data encoding: %s", event)
	}
	if msg := gjson.GetBytes(event, "response.error.message").String(); msg != "Invalid type for 'input[61].arguments': expected an object, but got a string instead." {
		t.Fatalf("error message not preserved: %q", msg)
	}
	if param := gjson.GetBytes(event, "response.error.param").String(); param != "input[61].arguments" {
		t.Fatalf("error param not preserved: %q", param)
	}
}

// TestHandleMessage_ErrorFrameWritesToCallbackAndEnds verifies that when handleMessage receives
// an upstream error frame, it writes the error event to the callback (passed through to downstream) and returns io.EOF to end the stream,
// instead of returning a plain error that makes the pipe close silently and the downstream get an empty response.
func TestHandleMessage_ErrorFrameWritesToCallbackAndEnds(t *testing.T) {
	r := &WsResponse{}
	upstream := []byte(`{"type":"error","status":400,"error":{"message":"boom"}}`)

	var captured [][]byte
	err := r.handleMessage(upstream, func(data []byte) bool {
		captured = append(captured, append([]byte(nil), data...))
		return true
	})

	if err != io.EOF {
		t.Fatalf("handleMessage on error frame returned %v, want io.EOF", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected error event written to callback once, got %d writes", len(captured))
	}
	if gjson.GetBytes(captured[0], "type").String() != "response.failed" {
		t.Fatalf("callback got type %q, want response.failed", gjson.GetBytes(captured[0], "type").String())
	}
	if gjson.GetBytes(captured[0], "response.error.message").String() != "boom" {
		t.Fatalf("error detail not preserved in callback payload")
	}
}

// TestHandleMessage_NormalFrameUnaffected verifies that normal frames still pass through unchanged.
func TestHandleMessage_NormalFrameUnaffected(t *testing.T) {
	r := &WsResponse{}
	var captured [][]byte
	cb := func(data []byte) bool { captured = append(captured, data); return true }

	// Normal incremental frame: pass through, do not terminate
	if err := r.handleMessage([]byte(`{"type":"response.output_text.delta","delta":"a"}`), cb); err != nil {
		t.Fatalf("delta frame returned %v, want nil", err)
	}
	// Completion frame: pass through, then io.EOF terminates
	if err := r.handleMessage([]byte(`{"type":"response.completed","response":{}}`), cb); err != io.EOF {
		t.Fatalf("completed frame returned %v, want io.EOF", err)
	}
	if len(captured) != 2 {
		t.Fatalf("expected 2 frames passed through, got %d", len(captured))
	}
}
