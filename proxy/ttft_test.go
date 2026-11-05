package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsFirstTokenEvent(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		want      bool
	}{
		// Must not trigger first token (control / terminal events)
		{"empty", "", false},
		{"created", "response.created", false},
		{"in_progress", "response.in_progress", false},
		{"completed", "response.completed", false},
		{"failed", "response.failed", false},

		// Must trigger first token: explicit content delta events
		{"function_call_arguments_delta", "response.function_call_arguments.delta", true},
		{"image_partial", "response.image_generation_call.partial_image", true},
		{"reasoning_text_delta", "response.reasoning_text.delta", true},
		{"reasoning_summary_text_delta", "response.reasoning_summary_text.delta", true},
		{"reasoning_encrypted_delta", "response.reasoning.encrypted_content.delta", true},

		// Existing paths still match
		{"output_text_delta", "response.output_text.delta", true},
		{"output_text_done", "response.output_text.done", true},

		// Structural events must not trigger first token based on type alone
		{"function_call_arguments_done", "response.function_call_arguments.done", false},
		{"output_item_added", "response.output_item.added", false},
		{"output_item_done", "response.output_item.done", false},
		{"content_part_added", "response.content_part.added", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFirstTokenEvent(tc.eventType); got != tc.want {
				t.Fatalf("isFirstTokenEvent(%q) = %v, want %v", tc.eventType, got, tc.want)
			}
		})
	}
}

func TestIsPreContentLifecycleEvent(t *testing.T) {
	cases := []struct {
		eventType string
		want      bool
	}{
		// Only created / in_progress are pre-content lifecycle frames that may be buffered briefly
		{"response.created", true},
		{"response.in_progress", true},
		{" response.created ", true},
		// Structural frames signal that the response has started and must be sent immediately
		{"response.output_item.added", false},
		{"response.content_part.added", false},
		// reasoning / content deltas must be sent immediately
		{"response.reasoning_summary_text.delta", false},
		{"response.output_text.delta", false},
		// Terminal frames are not part of the pre-content lifecycle
		{"response.completed", false},
		{"response.failed", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isPreContentLifecycleEvent(tc.eventType); got != tc.want {
			t.Fatalf("isPreContentLifecycleEvent(%q) = %v, want %v", tc.eventType, got, tc.want)
		}
	}
}

func TestIsFirstTokenPayload(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"created", []byte(`{"type":"response.created"}`), false},
		{"in_progress", []byte(`{"type":"response.in_progress"}`), false},
		{"output_item_added_function_call", []byte(`{"type":"response.output_item.added","item":{"type":"function_call","name":"lookup"}}`), false},
		{"output_item_done_empty_reasoning", []byte(`{"type":"response.output_item.done","item":{"type":"reasoning"}}`), false},
		{"content_part_added", []byte(`{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`), false},
		{"empty_reasoning_summary_part", []byte(`{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`), false},
		{"reasoning_summary_part_with_text", []byte(`{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":"initial thought"}}`), true},
		{"output_text_delta", []byte(`{"type":"response.output_text.delta","delta":"hello"}`), true},
		{"function_call_arguments_delta", []byte(`{"type":"response.function_call_arguments.delta","delta":"{\"city\""}`), true},
		{"function_call_arguments_done", []byte(`{"type":"response.function_call_arguments.done","arguments":"{}"}`), true},
		{"image_partial", []byte(`{"type":"response.image_generation_call.partial_image","partial_image_b64":"abc"}`), true},
		{"output_item_done_image", []byte(`{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"abc"}}`), true},
		{"output_item_done_message", []byte(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"done"}]}}`), true},
		{"content_part_done_text", []byte(`{"type":"response.content_part.done","part":{"type":"output_text","text":"done"}}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFirstTokenPayload(tc.data); got != tc.want {
				t.Fatalf("isFirstTokenPayload(%s) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestLooseFirstTokenResult(t *testing.T) {
	cases := []struct {
		name       string
		data       string
		wantStrict bool
		wantLoose  bool
	}{
		{
			name:       "output_item_added",
			data:       `{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
			wantStrict: false,
			wantLoose:  true,
		},
		{
			name:       "content_part_added",
			data:       `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`,
			wantStrict: false,
			wantLoose:  true,
		},
		{
			name:       "created",
			data:       `{"type":"response.created"}`,
			wantStrict: false,
			wantLoose:  false,
		},
		{
			name:       "in_progress",
			data:       `{"type":"response.in_progress"}`,
			wantStrict: false,
			wantLoose:  false,
		},
		{
			name:       "completed",
			data:       `{"type":"response.completed"}`,
			wantStrict: false,
			wantLoose:  false,
		},
		{
			name:       "failed",
			data:       `{"type":"response.failed"}`,
			wantStrict: false,
			wantLoose:  false,
		},
		{
			name:       "text_delta",
			data:       `{"type":"response.output_text.delta","delta":"hello"}`,
			wantStrict: true,
			wantLoose:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := gjson.Parse(tc.data)
			if got := isFirstTokenResult(parsed); got != tc.wantStrict {
				t.Fatalf("strict content check got %v, want %v", got, tc.wantStrict)
			}
			if got := isLooseFirstTokenResult(parsed); got != tc.wantLoose {
				t.Fatalf("loose first token got %v, want %v", got, tc.wantLoose)
			}
		})
	}
}
