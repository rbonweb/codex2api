package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsContextLengthExceededBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"upstream http error", `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_length_exceeded","param":"input"}}`, true},
		{"code only", `{"error":{"code":"context_length_exceeded"}}`, true},
		{"message only", `{"error":{"message":"Your input exceeds the context window of this model."}}`, true},
		{"other 400", `{"error":{"code":"invalid_request_error","message":"No tool call found for function call output"}}`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		if got := isContextLengthExceededBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsContextLengthExceededFailedPayload(t *testing.T) {
	payload := `{"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"}}}`
	if !isContextLengthExceededFailedPayload([]byte(payload)) {
		t.Fatal("real response.failed overflow payload should match")
	}
	other := `{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`
	if isContextLengthExceededFailedPayload([]byte(other)) {
		t.Fatal("non-overflow failure must not match")
	}
	if isContextLengthExceededFailedPayload(nil) {
		t.Fatal("empty payload must not match")
	}
}

func TestCompactOverflowResponsesBody_KeepsSystemAndTail(t *testing.T) {
	t.Setenv("CODEX_OVERFLOW_COMPACT_TAIL_KB", "1")

	big := strings.Repeat("x", 900)
	body := `{
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"system prompt"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"old fact: the project codename is Bluebird. ` + big + `"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"recent question"}]}
		]
	}`

	h := &Handler{}
	// The body has no model field: the summary call is skipped and the omitted-marker degradation path is taken,
	// so this test does not depend on a real upstream.
	got, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body))
	if !ok {
		t.Fatal("expected compaction to succeed")
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) >= 5 {
		t.Fatalf("expected old turns to be compacted away, got %d items: %s", len(items), got)
	}
	if role := items[0].Get("role").String(); role != "developer" {
		t.Fatalf("system prompt message should stay first, got %q", role)
	}
	if text := items[0].Get("content.0.text").String(); text != "system prompt" {
		t.Fatalf("system prompt content should be untouched, got %q", text)
	}
	if text := items[1].Get("content.0.text").String(); !strings.Contains(text, "omitted") {
		t.Fatalf("second item should be the compaction placeholder, got %q", text)
	}
	last := items[len(items)-1]
	if text := last.Get("content.0.text").String(); text != "recent question" {
		t.Fatalf("most recent turn should be preserved verbatim, got %q", text)
	}
}

func TestCompactOverflowResponsesBody_TooFewItems(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":"only one"}]}`
	h := &Handler{}
	if _, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body)); ok {
		t.Fatal("single-item input must not be compacted")
	}
}

func TestCompactOverflowResponsesBody_RepairsCutToolPairs(t *testing.T) {
	t.Setenv("CODEX_OVERFLOW_COMPACT_TAIL_KB", "1")

	big := strings.Repeat("y", 900)
	body := `{
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"function_call","call_id":"call_cut1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_cut1","output":"tool result kept in tail"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"recent"}]}
		]
	}`

	h := &Handler{}
	got, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body))
	if !ok {
		t.Fatal("expected compaction to succeed")
	}
	// Wherever the cut point falls, the output must not contain an orphan *_call_output (upstream would return 400).
	var orphan bool
	items := gjson.GetBytes(got, "input").Array()
	calls := map[string]bool{}
	for _, item := range items {
		if isCodexToolCallContextType(item.Get("type").String()) {
			calls[item.Get("call_id").String()] = true
		}
	}
	for _, item := range items {
		if isCodexToolCallOutputType(item.Get("type").String()) && !calls[item.Get("call_id").String()] {
			orphan = true
		}
	}
	if orphan {
		t.Fatalf("compacted input must not contain orphan tool outputs: %s", got)
	}
}

func TestFlattenOverflowItemsTranscript_CapsMiddle(t *testing.T) {
	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "HEAD-" + strings.Repeat("a", 3000)},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("b", 3000)}}},
		map[string]any{"type": "message", "role": "user", "content": strings.Repeat("c", 3000) + "-TAIL"},
	}
	got := flattenOverflowItemsTranscript(items, 2000)
	if len(got) > 2100 {
		t.Fatalf("transcript should be capped near 2000 bytes, got %d", len(got))
	}
	if !strings.Contains(got, "HEAD-") || !strings.Contains(got, "-TAIL") {
		t.Fatalf("cap should keep both head and tail, got %q...", got[:80])
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("cap should insert a truncation marker")
	}
}

func TestExtractResponsesSSEOutputText(t *testing.T) {
	sse := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"par\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\"},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"part one. \"},{\"type\":\"output_text\",\"text\":\"part two.\"}]}]}}\n\n" +
		"data: [DONE]\n\n"
	if got := extractResponsesSSEOutputText([]byte(sse)); got != "part one. part two." {
		t.Fatalf("unexpected extracted text: %q", got)
	}
	if got := extractResponsesSSEOutputText([]byte("data: [DONE]\n")); got != "" {
		t.Fatalf("no completed event should yield empty, got %q", got)
	}
}

func TestShouldDeferPreContentSSEEvent(t *testing.T) {
	cases := []struct {
		name             string
		eventType        string
		contentTokenSeen bool
		gotTerminal      bool
		passthrough      bool
		want             bool
	}{
		// Switch off: preflight metadata and lifecycle events are both buffered (v2.5.9 baseline)
		{"off_rate_limits_deferred", "codex.rate_limits", false, false, false, true},
		{"off_codex_metadata_deferred", "codex.response.metadata", false, false, false, true},
		{"off_http_metadata_deferred", "response.metadata", false, false, false, true},
		{"off_created_deferred", "response.created", false, false, false, true},
		{"off_in_progress_deferred", "response.in_progress", false, false, false, true},
		// Switch on: preflight metadata is written out immediately, lifecycle events are still buffered
		{"on_rate_limits_passthrough", "codex.rate_limits", false, false, true, false},
		{"on_codex_metadata_passthrough", "codex.response.metadata", false, false, true, false},
		{"on_http_metadata_passthrough", "response.metadata", false, false, true, false},
		{"on_created_still_deferred", "response.created", false, false, true, true},
		{"on_in_progress_still_deferred", "response.in_progress", false, false, true, true},
		// No buffering once content starts, regardless of the switch state
		{"content_seen_off", "codex.rate_limits", true, false, false, false},
		{"content_seen_on", "response.created", true, false, true, false},
		// No buffering after the terminal state
		{"terminal_off", "response.in_progress", false, true, false, false},
		// Content and failure events are never buffered
		{"delta_never_deferred", "response.output_text.delta", false, false, false, false},
		{"failed_never_deferred", "response.failed", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldDeferPreContentSSEEvent(tc.eventType, tc.contentTokenSeen, tc.gotTerminal, tc.passthrough)
			if got != tc.want {
				t.Fatalf("shouldDeferPreContentSSEEvent(%q, seen=%t, terminal=%t, passthrough=%t) = %t, want %t",
					tc.eventType, tc.contentTokenSeen, tc.gotTerminal, tc.passthrough, got, tc.want)
			}
		})
	}
}

// Degradation when the model summary is unavailable: before, the whole old conversation silently evaporated; now it must come with a readable
// verbatim excerpt, so the continuing model can still see the user goal and key facts.
func TestCompactOverflowResponsesBody_FallbackKeepsVerbatimDigest(t *testing.T) {
	t.Setenv("CODEX_OVERFLOW_COMPACT_TAIL_KB", "1")

	big := strings.Repeat("x", 900)
	body := `{
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"system prompt"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"the project codename is Bluebird. ` + big + `"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"recent question"}]}
		]
	}`

	h := &Handler{}
	// No model field => the summary call is skipped and the degradation path is taken directly, without depending on a real upstream.
	got, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body))
	if !ok {
		t.Fatal("expected compaction to succeed")
	}
	placeholder := gjson.GetBytes(got, "input").Array()[1].Get("content.0.text").String()
	if !strings.Contains(placeholder, "omitted") {
		t.Fatalf("fallback lost the omission marker: %q", placeholder)
	}
	if !strings.Contains(placeholder, "Bluebird") {
		t.Fatalf("fallback dropped the earlier conversation entirely: %q", placeholder)
	}
	if len(placeholder) > overflowCompactFallbackDigestBytes*2 {
		t.Fatalf("fallback digest is unbounded: %d bytes", len(placeholder))
	}
}

func TestOverflowCompactFallbackTextDegradesToMarkerWhenEmpty(t *testing.T) {
	text, mode := overflowCompactFallbackText(nil)
	if text != overflowCompactOmittedMarker || mode != "omitted marker" {
		t.Fatalf("empty head fallback = %q / %q", text, mode)
	}
	digest, mode := overflowCompactFallbackText([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "deploy target is prod-eu"}}},
	})
	if !strings.Contains(digest, "deploy target is prod-eu") || mode != "mechanical excerpt" {
		t.Fatalf("digest fallback = %q / %q", digest, mode)
	}
}

// Only "the summary call itself overflowed" is worth a shrink-and-retry; any other failure gives the same result at any size.
func TestOverflowSummaryFailedForContextWindow(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "json context length error", status: 400, body: `{"error":{"code":"context_length_exceeded"}}`, want: true},
		{name: "json window message", status: 400, body: `{"error":{"message":"input exceeds the context window"}}`, want: true},
		{name: "sse text fallback", status: 400, body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\"}}}\n", want: true},
		{name: "rate limited", status: 429, body: `{"error":{"code":"rate_limit_exceeded"}}`, want: false},
		{name: "no available account", status: 503, body: `{"error":{"message":"no available account"}}`, want: false},
		{name: "success without text", status: 200, body: `{}`, want: false},
		{name: "empty body", status: 400, body: "", want: false},
	}
	for _, tc := range cases {
		if got := overflowSummaryFailedForContextWindow(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("%s: overflowSummaryFailedForContextWindow = %t, want %t", tc.name, got, tc.want)
		}
	}
}
