package proxy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/api"
)

// realtimeAudioTestIDs returns a deterministic id generator: prefix_01, prefix_02, ...
func realtimeAudioTestIDs() func(prefix string) string {
	n := 0
	return func(prefix string) string {
		n++
		return fmt.Sprintf("%s_%02d", prefix, n)
	}
}

// runRealtimeAudioMapper feeds upstream events to m and returns the decoded OpenAI events in order.
func runRealtimeAudioMapper(t *testing.T, m *realtimeAudioMapper, upstream ...string) []map[string]any {
	t.Helper()
	var out [][]byte
	for _, raw := range upstream {
		out = append(out, m.MapUpstream([]byte(raw))...)
	}
	return decodeRealtimeAudioEvents(t, out)
}

func decodeRealtimeAudioEvents(t *testing.T, raw [][]byte) []map[string]any {
	t.Helper()
	events := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		var event map[string]any
		if err := json.Unmarshal(item, &event); err != nil {
			t.Fatalf("emitted event is not a JSON object: %v: %s", err, item)
		}
		if event["type"] == nil || event["type"] == "" {
			t.Fatalf("emitted event has no type: %s", item)
		}
		if id, _ := event["event_id"].(string); id == "" {
			t.Fatalf("emitted event %v has no event_id", event["type"])
		}
		events = append(events, event)
	}
	return events
}

func realtimeAudioEventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event["type"].(string))
	}
	return types
}

func decodeRealtimeAudioJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, raw)
	}
	return out
}

func TestRealtimeAudioMapperIgnoresUnmappedEvents(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"session started", `{"type":"session.started"}`},
		{"session updated", `{"type":"session.updated"}`},
		{"session context appended", `{"type":"session.context.appended"}`},
		{"session input audio append", `{"type":"session.input_audio.append"}`},
		{"turn delta", `{"type":"turn.delta","delta":"x"}`},
		{"delegation start", `{"type":"delegation.start"}`},
		{"delegation end", `{"type":"delegation.end"}`},
		{"unknown type", `{"type":"something.new"}`},
		{"turn created without role", `{"type":"turn.created"}`},
		{"turn created with system role", `{"type":"turn.created","turn":{"role":"system"}}`},
		{"turn done with system role", `{"type":"turn.done","turn":{"role":"system"}}`},
		{"invalid json", `not json`},
		{"empty input", ``},
		{"json array", `["turn.created"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newRealtimeAudioMapper(realtimeAudioTestIDs())
			if out := m.MapUpstream([]byte(tt.raw)); len(out) != 0 {
				t.Fatalf("expected no events, got %d: %s", len(out), out)
			}
		})
	}
}

func TestRealtimeAudioMapperEventSequences(t *testing.T) {
	openAssistant := []string{"response.created", "response.output_item.added", "response.content_part.added"}
	closeAssistant := []string{
		"response.output_audio.done",
		"response.output_audio_transcript.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.done",
	}
	userCreated := []string{"conversation.item.created", "input_audio_buffer.speech_started"}
	concat := func(parts ...[]string) []string {
		var out []string
		for _, part := range parts {
			out = append(out, part...)
		}
		return out
	}

	tests := []struct {
		name     string
		upstream []string
		want     []string
	}{
		{
			name:     "assistant turn created opens the response",
			upstream: []string{`{"type":"turn.created","turn":{"role":"assistant"}}`},
			want:     openAssistant,
		},
		{
			name:     "top-level role opens the assistant response",
			upstream: []string{`{"type":"turn.created","role":"assistant"}`},
			want:     openAssistant,
		},
		{
			name:     "user turn created opens the user item and speech start",
			upstream: []string{`{"type":"turn.created","turn":{"role":"user"}}`},
			want:     userCreated,
		},
		{
			name:     "audio delta opens the response",
			upstream: []string{`{"type":"session.output_audio.delta","delta":"AAAA"}`},
			want:     concat(openAssistant, []string{"response.output_audio.delta"}),
		},
		{
			name:     "bare output audio delta with audio field",
			upstream: []string{`{"type":"output_audio.delta","audio":"AAAA"}`},
			want:     concat(openAssistant, []string{"response.output_audio.delta"}),
		},
		{
			name:     "transcript added opens the response",
			upstream: []string{`{"type":"output_transcript.added","item":{"text":"Hi"}}`},
			want:     concat(openAssistant, []string{"response.output_audio_transcript.delta"}),
		},
		{
			name:     "silent assistant turn still yields a full sequence",
			upstream: []string{`{"type":"turn.done","turn":{"role":"assistant"}}`},
			want:     concat(openAssistant, closeAssistant),
		},
		{
			name: "assistant turn with audio and transcript",
			upstream: []string{
				`{"type":"turn.created","turn":{"role":"assistant"}}`,
				`{"type":"session.output_audio.delta","delta":"AAAA"}`,
				`{"type":"output_transcript.added","item":{"text":"Hi"}}`,
				`{"type":"turn.done","turn":{"role":"assistant","transcript":"Hi"}}`,
			},
			want: concat(openAssistant, []string{"response.output_audio.delta", "response.output_audio_transcript.delta"}, closeAssistant),
		},
		{
			name:     "user transcript without turn created skips speech events",
			upstream: []string{`{"type":"input_transcript.added","item":{"text":"hel"}}`, `{"type":"turn.done","turn":{"role":"user"}}`},
			want:     []string{"conversation.item.created", "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed"},
		},
		{
			name: "full user turn with speech start and stop",
			upstream: []string{
				`{"type":"turn.created","turn":{"role":"user"}}`,
				`{"type":"input_transcript.added","item":{"text":"hello"}}`,
				`{"type":"turn.done","turn":{"role":"user","transcript":"hello"}}`,
			},
			want: []string{
				"conversation.item.created",
				"input_audio_buffer.speech_started",
				"conversation.item.input_audio_transcription.delta",
				"input_audio_buffer.speech_stopped",
				"conversation.item.input_audio_transcription.completed",
			},
		},
		{
			name:     "error with object",
			upstream: []string{`{"type":"error","error":{"type":"invalid_request","code":"bad","message":"nope"}}`},
			want:     []string{"error"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newRealtimeAudioMapper(realtimeAudioTestIDs())
			events := runRealtimeAudioMapper(t, m, tt.upstream...)
			if got := realtimeAudioEventTypes(events); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("event types\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}

func TestRealtimeAudioMapperAssistantTurnFields(t *testing.T) {
	m := newRealtimeAudioMapper(realtimeAudioTestIDs())
	events := runRealtimeAudioMapper(t, m,
		`{"type":"turn.created","turn":{"role":"assistant"}}`,
		`{"type":"session.output_audio.delta","delta":"AAAA"}`,
		`{"type":"output_transcript.added","item":{"text":"Hello "}}`,
		`{"type":"output_transcript.added","delta":"there"}`,
		`{"type":"turn.done","turn":{"role":"assistant"}}`,
	)

	respID := events[0]["response"].(map[string]any)["id"]
	if respID != "resp_01" {
		t.Fatalf("response id = %v, want resp_01", respID)
	}
	itemID := events[1]["item"].(map[string]any)["id"]
	if itemID != "item_02" {
		t.Fatalf("item id = %v, want item_02", itemID)
	}

	delta := events[3]
	if delta["response_id"] != respID || delta["item_id"] != itemID || delta["delta"] != "AAAA" {
		t.Fatalf("audio delta fields = %v", delta)
	}
	if delta["output_index"] != float64(0) || delta["content_index"] != float64(0) {
		t.Fatalf("audio delta indexes = %v", delta)
	}

	// The transcript is accumulated from item.text and delta, and reused by the done events.
	for _, idx := range []int{4, 5} {
		if events[idx]["delta"] == nil {
			t.Fatalf("transcript delta %d missing delta: %v", idx, events[idx])
		}
	}
	doneTranscript := events[7]
	if doneTranscript["type"] != "response.output_audio_transcript.done" || doneTranscript["transcript"] != "Hello there" {
		t.Fatalf("transcript done = %v", doneTranscript)
	}
	outputDone := events[9]
	item := outputDone["item"].(map[string]any)
	if item["status"] != "completed" || item["id"] != itemID {
		t.Fatalf("output item done = %v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_audio" || content["transcript"] != "Hello there" {
		t.Fatalf("output item content = %v", content)
	}
	responseDone := events[10]["response"].(map[string]any)
	if responseDone["id"] != respID || responseDone["status"] != "completed" {
		t.Fatalf("response.done = %v", responseDone)
	}
	if output := responseDone["output"].([]any); len(output) != 1 {
		t.Fatalf("response.done output len = %d, want 1", len(output))
	}
}

func TestRealtimeAudioMapperTranscriptFallback(t *testing.T) {
	tests := []struct {
		name       string
		upstream   []string
		wantAssist string
		wantUser   string
	}{
		{
			name: "turn transcript wins over accumulated text",
			upstream: []string{
				`{"type":"output_transcript.added","text":"partial"}`,
				`{"type":"turn.done","turn":{"role":"assistant","transcript":"final"}}`,
				`{"type":"input_transcript.added","delta":"partial"}`,
				`{"type":"turn.done","turn":{"role":"user","transcript":"final"}}`,
			},
			wantAssist: "final",
			wantUser:   "final",
		},
		{
			name: "empty turn transcript falls back to accumulated text",
			upstream: []string{
				`{"type":"output_transcript.added","text":"part one "}`,
				`{"type":"output_transcript.added","text":"part two"}`,
				`{"type":"turn.done","turn":{"role":"assistant","transcript":""}}`,
				`{"type":"input_transcript.added","text":"user "}`,
				`{"type":"input_transcript.added","text":"words"}`,
				`{"type":"turn.done","turn":{"role":"user"}}`,
			},
			wantAssist: "part one part two",
			wantUser:   "user words",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newRealtimeAudioMapper(realtimeAudioTestIDs())
			events := runRealtimeAudioMapper(t, m, tt.upstream...)
			var assist, user any
			for _, event := range events {
				switch event["type"] {
				case "response.output_audio_transcript.done":
					assist = event["transcript"]
				case "conversation.item.input_audio_transcription.completed":
					user = event["transcript"]
				}
			}
			if assist != tt.wantAssist {
				t.Errorf("assistant transcript = %v, want %q", assist, tt.wantAssist)
			}
			if user != tt.wantUser {
				t.Errorf("user transcript = %v, want %q", user, tt.wantUser)
			}
		})
	}
}

func TestRealtimeAudioMapperUserItemID(t *testing.T) {
	m := newRealtimeAudioMapper(realtimeAudioTestIDs())

	first := m.UserItemID()
	if second := m.UserItemID(); second != first {
		t.Fatalf("UserItemID before a user turn changed: %s then %s", first, second)
	}

	events := runRealtimeAudioMapper(t, m, `{"type":"turn.created","turn":{"role":"user"}}`)
	if got := events[0]["item"].(map[string]any)["id"]; got != first {
		t.Fatalf("user item id = %v, want the id handed out before the turn (%s)", got, first)
	}
	if m.UserItemID() != first {
		t.Fatalf("UserItemID while the user item is open must return %s", first)
	}

	runRealtimeAudioMapper(t, m, `{"type":"turn.done","turn":{"role":"user","transcript":"hi"}}`)
	next := m.UserItemID()
	if next == first || next == "" {
		t.Fatalf("UserItemID after the user turn = %q, want a new id", next)
	}
	if m.UserItemID() != next {
		t.Fatalf("UserItemID must be stable until the next user turn")
	}
}

func TestRealtimeAudioMapperPreviousItemIDChaining(t *testing.T) {
	m := newRealtimeAudioMapper(realtimeAudioTestIDs())
	events := runRealtimeAudioMapper(t, m,
		`{"type":"turn.created","turn":{"role":"user"}}`,
		`{"type":"turn.done","turn":{"role":"user","transcript":"one"}}`,
		`{"type":"turn.done","turn":{"role":"assistant"}}`,
		`{"type":"turn.created","turn":{"role":"user"}}`,
	)

	var created []map[string]any
	for _, event := range events {
		if event["type"] == "conversation.item.created" {
			created = append(created, event)
		}
	}
	if len(created) != 2 {
		t.Fatalf("got %d conversation.item.created events, want 2", len(created))
	}
	if created[0]["previous_item_id"] != nil {
		t.Fatalf("first user item previous_item_id = %v, want null", created[0]["previous_item_id"])
	}
	assistantItem := ""
	for _, event := range events {
		if event["type"] == "response.done" {
			assistantItem = event["response"].(map[string]any)["output"].([]any)[0].(map[string]any)["id"].(string)
		}
	}
	if assistantItem == "" {
		t.Fatal("assistant item id missing from response.done")
	}
	if created[1]["previous_item_id"] != assistantItem {
		t.Fatalf("second user item previous_item_id = %v, want the assistant item %s", created[1]["previous_item_id"], assistantItem)
	}
}

func TestRealtimeAudioMapperErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantType string
		wantCode any
		wantMsg  string
	}{
		{
			name:     "full error object",
			raw:      `{"type":"error","error":{"type":"invalid_request","code":"bad_audio","message":"nope"}}`,
			wantType: "invalid_request",
			wantCode: "bad_audio",
			wantMsg:  "nope",
		},
		{
			name:     "top level message and code",
			raw:      `{"type":"error","code":"rate_limited","message":"slow down"}`,
			wantType: "server_error",
			wantCode: "rate_limited",
			wantMsg:  "slow down",
		},
		{
			name:     "defaults when nothing is given",
			raw:      `{"type":"error"}`,
			wantType: "server_error",
			wantCode: nil,
			wantMsg:  "ChatGPT Live error",
		},
		{
			name:     "string error body is the message",
			raw:      `{"type":"error","error":"boom"}`,
			wantType: "server_error",
			wantCode: nil,
			wantMsg:  "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newRealtimeAudioMapper(realtimeAudioTestIDs())
			events := runRealtimeAudioMapper(t, m, tt.raw)
			if len(events) != 1 || events[0]["type"] != "error" {
				t.Fatalf("events = %v, want one error event", events)
			}
			errObj := events[0]["error"].(map[string]any)
			if errObj["type"] != tt.wantType || errObj["code"] != tt.wantCode || errObj["message"] != tt.wantMsg {
				t.Fatalf("error = %v, want type %s code %v message %q", errObj, tt.wantType, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

func TestRealtimeAudioMapperDefaultIDs(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	events := runRealtimeAudioMapper(t, m, `{"type":"turn.created","turn":{"role":"assistant"}}`)
	respID := events[0]["response"].(map[string]any)["id"].(string)
	if !strings.HasPrefix(respID, "resp_") || len(respID) > 24 {
		t.Fatalf("default response id = %q, want resp_ prefix and at most 24 characters", respID)
	}
	if eventID := events[0]["event_id"].(string); !strings.HasPrefix(eventID, "event_") {
		t.Fatalf("default event id = %q", eventID)
	}
}

func TestRealtimeAudioSessionUpdateRejectsBadSession(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"missing session", `{"type":"session.update"}`},
		{"session is a string", `{"type":"session.update","session":"x"}`},
		{"session is an array", `{"type":"session.update","session":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &realtimeAudioConfig{}
			sideband, ack, apiErr := applyRealtimeAudioSessionUpdate(cfg, []byte(tt.raw))
			if apiErr == nil || apiErr.Code != api.ErrCodeMissingField || apiErr.Type != api.ErrorTypeInvalidRequest {
				t.Fatalf("apiErr = %+v, want missing_field invalid_request", apiErr)
			}
			if sideband != nil || ack != nil {
				t.Fatalf("expected no sideband or ack on error")
			}
		})
	}
}

func TestRealtimeAudioSessionUpdateFormats(t *testing.T) {
	const inMsg = `only pcm16 24 kHz mono input audio is supported (audio.input.format {"type":"audio/pcm","rate":24000})`
	const outMsg = `only pcm16 24 kHz mono output audio is supported (audio.output.format {"type":"audio/pcm","rate":24000})`
	tests := []struct {
		name    string
		session string
		wantErr string
	}{
		{"no formats", `{}`, ""},
		{"input pcm16 string", `{"input_audio_format":"pcm16"}`, ""},
		{"audio input pcm16 string", `{"audio":{"input":{"format":"pcm16"}}}`, ""},
		{"audio input object without rate", `{"audio":{"input":{"format":{"type":"audio/pcm"}}}}`, ""},
		{"audio input object at 24 kHz", `{"audio":{"input":{"format":{"type":"audio/pcm","rate":24000}}}}`, ""},
		{"output pcm16 string", `{"output_audio_format":"pcm16"}`, ""},
		{"audio output object at 24 kHz", `{"audio":{"output":{"format":{"type":"audio/pcm","rate":24000}}}}`, ""},
		{"input g711 string", `{"input_audio_format":"g711_ulaw"}`, inMsg},
		{"audio input pcmu", `{"audio":{"input":{"format":{"type":"audio/pcmu"}}}}`, inMsg},
		{"audio input pcma", `{"audio":{"input":{"format":{"type":"audio/pcma"}}}}`, inMsg},
		{"audio input 16 kHz", `{"audio":{"input":{"format":{"type":"audio/pcm","rate":16000}}}}`, inMsg},
		{"output g711 string", `{"output_audio_format":"g711_alaw"}`, outMsg},
		{"audio output pcmu", `{"audio":{"output":{"format":{"type":"audio/pcmu"}}}}`, outMsg},
		{"audio output 48 kHz", `{"audio":{"output":{"format":{"type":"audio/pcm","rate":48000}}}}`, outMsg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"type":"session.update","session":` + tt.session + `}`)
			cfg := &realtimeAudioConfig{}
			_, ack, apiErr := applyRealtimeAudioSessionUpdate(cfg, raw)
			if tt.wantErr == "" {
				if apiErr != nil {
					t.Fatalf("unexpected error: %+v", apiErr)
				}
				if decodeRealtimeAudioJSON(t, ack)["type"] != "session.updated" {
					t.Fatalf("ack = %s", ack)
				}
				return
			}
			if apiErr == nil {
				t.Fatalf("expected rejection with %q", tt.wantErr)
			}
			if apiErr.Code != api.ErrCodeInvalidRequest || apiErr.Type != api.ErrorTypeInvalidRequest {
				t.Fatalf("apiErr code/type = %s/%s, want invalid_request/invalid_request_error", apiErr.Code, apiErr.Type)
			}
			if apiErr.Message != tt.wantErr {
				t.Fatalf("message = %q, want %q", apiErr.Message, tt.wantErr)
			}
		})
	}
}

func TestRealtimeAudioSessionUpdateState(t *testing.T) {
	cfg := &realtimeAudioConfig{Model: "gpt-realtime"}

	// Only the model changes: no sideband, but the ack reports the new model.
	sideband, ack, apiErr := applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"model":"gpt-realtime-mini"}}`))
	if apiErr != nil || sideband != nil {
		t.Fatalf("model-only update: sideband=%s err=%+v", sideband, apiErr)
	}
	ackSession := decodeRealtimeAudioJSON(t, ack)["session"].(map[string]any)
	if ackSession["model"] != "gpt-realtime-mini" || ackSession["output_modalities"].([]any)[0] != "audio" {
		t.Fatalf("model-only ack session = %v", ackSession)
	}

	// Voice sends a sideband with the Live voice and the default instructions.
	sideband, ack, apiErr = applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"audio":{"output":{"voice":"alloy"}}}}`))
	if apiErr != nil {
		t.Fatalf("voice update: %+v", apiErr)
	}
	wantSideband := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": liveAudioDefaultInstructions,
			"audio":        map[string]any{"output": map[string]any{"voice": "cove"}},
			"delegation":   map[string]any{"type": "client"},
		},
	}
	if got := decodeRealtimeAudioJSON(t, sideband); !reflect.DeepEqual(got, wantSideband) {
		t.Fatalf("voice sideband = %v, want %v", got, wantSideband)
	}
	if decodeRealtimeAudioJSON(t, ack)["type"] != "session.updated" {
		t.Fatalf("voice ack = %s", ack)
	}

	// Voice and instructions together; a Live voice name passes through.
	sideband, _, _ = applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"voice":"sol","instructions":"Be brief."}}`))
	sidebandSession := decodeRealtimeAudioJSON(t, sideband)["session"].(map[string]any)
	if sidebandSession["instructions"] != "Be brief." || sidebandSession["audio"].(map[string]any)["output"].(map[string]any)["voice"] != "sol" {
		t.Fatalf("instructions and voice sideband = %v", sidebandSession)
	}

	// Text only, then the beta modalities field, then audio again.
	_, ack, _ = applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"output_modalities":["text"]}}`))
	if !cfg.TextOnly {
		t.Fatal("output_modalities [text] must set TextOnly")
	}
	if got := decodeRealtimeAudioJSON(t, ack)["session"].(map[string]any)["output_modalities"]; !reflect.DeepEqual(got, []any{"text"}) {
		t.Fatalf("text-only ack modalities = %v", got)
	}
	applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"model":"gpt-live"}}`))
	if !cfg.TextOnly {
		t.Fatal("an update without modalities must leave TextOnly unchanged")
	}
	applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"modalities":["text","audio"]}}`))
	if cfg.TextOnly {
		t.Fatal("beta modalities containing audio must clear TextOnly")
	}
	applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"output_modalities":["text"]}}`))
	applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"output_modalities":["audio"]}}`))
	if cfg.TextOnly {
		t.Fatal("output_modalities [audio] must clear TextOnly")
	}

	// A blank instructions string is sent as the default.
	sideband, _, _ = applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"instructions":"   "}}`))
	if got := decodeRealtimeAudioJSON(t, sideband)["session"].(map[string]any)["instructions"]; got != liveAudioDefaultInstructions {
		t.Fatalf("blank instructions sideband = %v", got)
	}
}

func TestRealtimeAudioSessionUpdateAckDefaults(t *testing.T) {
	cfg := &realtimeAudioConfig{}
	_, ack, apiErr := applyRealtimeAudioSessionUpdate(cfg, []byte(`{"type":"session.update","session":{"model":"x"}}`))
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	session := decodeRealtimeAudioJSON(t, ack)["session"].(map[string]any)
	if session["model"] != "x" || session["instructions"] != liveAudioDefaultInstructions {
		t.Fatalf("ack session = %v", session)
	}
	audio := session["audio"].(map[string]any)
	if audio["output"].(map[string]any)["voice"] != liveAudioDefaultVoice {
		t.Fatalf("ack voice = %v, want default", audio["output"])
	}
	if _, hasID := session["id"]; hasID {
		t.Fatal("session.updated ack must not carry an id")
	}
}

func TestRealtimeAudioSessionObjectWithID(t *testing.T) {
	session := realtimeAudioSessionObject(&realtimeAudioConfig{TextOnly: true}, "sess_1")
	if session["id"] != "sess_1" || session["object"] != "realtime.session" {
		t.Fatalf("session object = %v", session)
	}
	if !reflect.DeepEqual(session["output_modalities"], []string{"text"}) {
		t.Fatalf("output_modalities = %v", session["output_modalities"])
	}
	if _, ok := realtimeAudioSessionObject(&realtimeAudioConfig{}, "")["id"]; ok {
		t.Fatal("empty id must leave the id field out")
	}
}

func TestRealtimeAudioTextItemToContextAppend(t *testing.T) {
	const audioMsg = "audio conversation items are not supported; stream audio with input_audio_buffer.append"
	tests := []struct {
		name       string
		raw        string
		wantErr    string
		wantErrCod api.ErrorCode
		wantText   string
		wantPrev   any
	}{
		{
			name:     "input text message",
			raw:      `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`,
			wantText: "hi",
		},
		{
			name:     "type absent and text parts joined",
			raw:      `{"item":{"role":"user","content":[{"type":"text","text":"one"},{"type":"output_text","text":""},{"type":"input_text","text":"two"}]}}`,
			wantText: "one\ntwo",
		},
		{
			name:     "previous item echoed",
			raw:      `{"previous_item_id":"item_9","item":{"id":"item_keep","content":[{"type":"input_text","text":"x"}]}}`,
			wantText: "x",
			wantPrev: "item_9",
		},
		{
			name:       "input audio part rejected",
			raw:        `{"item":{"type":"message","content":[{"type":"input_audio","audio":"AAAA"}]}}`,
			wantErr:    audioMsg,
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "audio item type part rejected",
			raw:        `{"item":{"type":"message","content":[{"type":"input_text","text":"a"},{"type":"audio","audio":"AAAA"}]}}`,
			wantErr:    audioMsg,
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "non message item rejected",
			raw:        `{"item":{"type":"function_call_output","content":[{"type":"input_text","text":"x"}]}}`,
			wantErr:    "only message conversation items are supported",
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "unsupported part rejected",
			raw:        `{"item":{"type":"message","content":[{"type":"input_image","image_url":"x"}]}}`,
			wantErr:    "unsupported conversation item content part type: input_image",
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "empty text rejected",
			raw:        `{"item":{"type":"message","content":[{"type":"input_text","text":""}]}}`,
			wantErr:    "conversation item has no text content",
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "no content rejected",
			raw:        `{"item":{"type":"message"}}`,
			wantErr:    "conversation item content must be an array of text parts",
			wantErrCod: api.ErrCodeInvalidRequest,
		},
		{
			name:       "missing item",
			raw:        `{"type":"conversation.item.create"}`,
			wantErr:    "item is required in conversation.item.create",
			wantErrCod: api.ErrCodeMissingField,
		},
		{
			name:       "item not an object",
			raw:        `{"item":"hi"}`,
			wantErr:    "item is required in conversation.item.create",
			wantErrCod: api.ErrCodeMissingField,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sideband, ack, apiErr := realtimeTextItemToContextAppend([]byte(tt.raw))
			if tt.wantErr != "" {
				if apiErr == nil {
					t.Fatalf("expected error %q", tt.wantErr)
				}
				if apiErr.Message != tt.wantErr || apiErr.Code != tt.wantErrCod || apiErr.Type != api.ErrorTypeInvalidRequest {
					t.Fatalf("apiErr = %+v, want code %s message %q", apiErr, tt.wantErrCod, tt.wantErr)
				}
				if sideband != nil || ack != nil {
					t.Fatal("expected no sideband or ack on error")
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %+v", apiErr)
			}
			wantSideband := map[string]any{
				"type":    "session.context.append",
				"content": []any{map[string]any{"type": "input_text", "text": tt.wantText}},
			}
			if got := decodeRealtimeAudioJSON(t, sideband); !reflect.DeepEqual(got, wantSideband) {
				t.Fatalf("sideband = %v, want %v", got, wantSideband)
			}
			acked := decodeRealtimeAudioJSON(t, ack)
			if acked["type"] != "conversation.item.created" {
				t.Fatalf("ack type = %v", acked["type"])
			}
			if acked["event_id"] == "" || acked["event_id"] == nil {
				t.Fatal("ack has no event_id")
			}
			if tt.wantPrev == nil {
				if acked["previous_item_id"] != nil {
					t.Fatalf("previous_item_id = %v, want null", acked["previous_item_id"])
				}
			} else if acked["previous_item_id"] != tt.wantPrev {
				t.Fatalf("previous_item_id = %v, want %v", acked["previous_item_id"], tt.wantPrev)
			}
			item := acked["item"].(map[string]any)
			if item["status"] != "completed" {
				t.Fatalf("ack item status = %v", item["status"])
			}
			if id, _ := item["id"].(string); id == "" {
				t.Fatal("ack item id not filled")
			}
		})
	}
}

func TestRealtimeAudioTextItemKeepsClientID(t *testing.T) {
	_, ack, apiErr := realtimeTextItemToContextAppend([]byte(`{"item":{"id":"item_keep","content":[{"type":"input_text","text":"x"}]}}`))
	if apiErr != nil {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	item := decodeRealtimeAudioJSON(t, ack)["item"].(map[string]any)
	if item["id"] != "item_keep" {
		t.Fatalf("item id = %v, want the client id", item["id"])
	}
}
