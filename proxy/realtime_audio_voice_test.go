package proxy

import (
	"encoding/base64"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func voiceBase64(pcm []byte) string { return base64.StdEncoding.EncodeToString(pcm) }

// mapVoiceEvents maps upstream events and returns the mapped events.
func mapVoiceEvents(m *realtimeAudioMapper, raws ...string) [][]byte {
	var out [][]byte
	for _, raw := range raws {
		out = append(out, m.MapUpstream([]byte(raw))...)
	}
	return out
}

func voiceEventTypes(events [][]byte) string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = gjson.GetBytes(e, "type").String()
	}
	return strings.Join(types, ",")
}

func findVoiceEvent(events [][]byte, typ string) gjson.Result {
	for _, e := range events {
		if gjson.GetBytes(e, "type").String() == typ {
			return gjson.ParseBytes(e)
		}
	}
	return gjson.Result{}
}

func TestRealtimeAudioMapperDetectorSpeechEventsCarryTheItemAndSilenceTheUpstreamOnes(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	started := m.Speech([]audioSpeechEvent{{Started: true, MS: 500}})
	if len(started) != 1 || gjson.GetBytes(started[0], "type").String() != "input_audio_buffer.speech_started" || gjson.GetBytes(started[0], "audio_start_ms").Int() != 500 {
		t.Fatalf("started = %s", started)
	}
	itemID := gjson.GetBytes(started[0], "item_id").String()
	if itemID == "" || itemID != m.UserItemID() {
		t.Fatalf("speech_started item %q, next user item %q", itemID, m.UserItemID())
	}
	stopped := m.Speech([]audioSpeechEvent{{Started: false, MS: 1500}})
	if gjson.GetBytes(stopped[0], "type").String() != "input_audio_buffer.speech_stopped" || gjson.GetBytes(stopped[0], "audio_end_ms").Int() != 1500 || gjson.GetBytes(stopped[0], "item_id").String() != itemID {
		t.Fatalf("stopped = %s", stopped)
	}

	// The upstream user turn arrives later and uses the same item without repeating the speech events.
	later := mapVoiceEvents(m,
		`{"turn":{"id":"u1","role":"user","start_ms":400},"type":"turn.created"}`,
		`{"item":{"text":"Hello"},"type":"input_transcript.added"}`,
		`{"turn":{"id":"u1","role":"user","end_ms":1600,"transcript":"Hello"},"type":"turn.done"}`,
	)
	if got := voiceEventTypes(later); got != "conversation.item.created,conversation.item.input_audio_transcription.delta,conversation.item.input_audio_transcription.completed" {
		t.Fatalf("later events = %s", got)
	}
	if gjson.GetBytes(later[0], "item.id").String() != itemID {
		t.Errorf("user item %s, want %s", later[0], itemID)
	}
}

func TestRealtimeAudioMapperUpstreamSpeechEventsWhenTheDetectorHeardNothing(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	events := mapVoiceEvents(m,
		`{"turn":{"id":"u1","role":"user","start_ms":1800,"transcript":"Please"},"type":"turn.created"}`,
		`{"turn":{"id":"u1","role":"user","end_ms":14600,"transcript":"Please tell me"},"type":"turn.done"}`,
	)
	if got := voiceEventTypes(events); got != "conversation.item.created,input_audio_buffer.speech_started,input_audio_buffer.speech_stopped,conversation.item.input_audio_transcription.completed" {
		t.Fatalf("events = %s", got)
	}
	if findVoiceEvent(events, "input_audio_buffer.speech_started").Get("audio_start_ms").Int() != 1800 || findVoiceEvent(events, "input_audio_buffer.speech_stopped").Get("audio_end_ms").Int() != 14600 {
		t.Errorf("times = %s", events)
	}
}

func TestRealtimeAudioMapperLanguageOnInputTranscripts(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	var events [][]byte
	for _, word := range strings.SplitAfter("The quick brown fox jumps over the lazy dog and then runs back into the quiet green forest", " ") {
		events = append(events, m.MapUpstream([]byte(`{"item":{"text":"`+word+`"},"type":"input_transcript.added"}`))...)
	}
	deltas := 0
	last := gjson.Result{}
	for _, e := range events {
		if gjson.GetBytes(e, "type").String() == "conversation.item.input_audio_transcription.delta" {
			deltas++
			last = gjson.ParseBytes(e)
		}
	}
	if deltas < 10 || last.Get("language").String() != "en" {
		t.Fatalf("last delta = %s after %d deltas", last.Raw, deltas)
	}
	// Two words are too few for the detector, so the first deltas carry no language.
	first := newRealtimeAudioMapper(nil).MapUpstream([]byte(`{"item":{"text":"Hello"},"type":"input_transcript.added"}`))
	if findVoiceEvent(first, "conversation.item.input_audio_transcription.delta").Get("language").Exists() {
		t.Errorf("a one-word delta carries a language: %s", first)
	}
	done := m.MapUpstream([]byte(`{"turn":{"id":"u1","role":"user","transcript":"The quick brown fox jumps over the lazy dog and then runs back into the quiet green forest"},"type":"turn.done"}`))
	if findVoiceEvent(done, "conversation.item.input_audio_transcription.completed").Get("language").String() != "en" {
		t.Errorf("completed = %s", done)
	}
}

func TestRealtimeAudioMapperTranscriptionLanguageIsReportedAndConvertsGujarati(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	m.SetTranscriptionLanguage("gu")
	events := mapVoiceEvents(m,
		`{"item":{"text":"नमस्ते"},"type":"input_transcript.added"}`,
		`{"item":{"text":" आप कैसे हैं"},"type":"input_transcript.added"}`,
		`{"turn":{"id":"u1","role":"user","transcript":"नमस्ते आप कैसे हैं"},"type":"turn.done"}`,
	)
	var deltas []gjson.Result
	for _, e := range events {
		if gjson.GetBytes(e, "type").String() == "conversation.item.input_audio_transcription.delta" {
			deltas = append(deltas, gjson.ParseBytes(e))
		}
	}
	if len(deltas) != 2 || deltas[0].Get("delta").String() != "નમસ્તે" || deltas[0].Get("language").String() != "gu" || deltas[1].Get("delta").String() != " આપ કૈસે હૈં" {
		t.Fatalf("deltas = %v", deltas)
	}
	completed := findVoiceEvent(events, "conversation.item.input_audio_transcription.completed")
	if completed.Get("transcript").String() != "નમસ્તે આપ કૈસે હૈં" || completed.Get("language").String() != "gu" {
		t.Fatalf("completed = %s", completed.Raw)
	}
	// Another language is reported as named and the script is untouched.
	hi := newRealtimeAudioMapper(nil)
	hi.SetTranscriptionLanguage("ur")
	r := hi.MapUpstream([]byte(`{"item":{"text":"یہ اردو"},"type":"input_transcript.added"}`))
	if d := findVoiceEvent(r, "conversation.item.input_audio_transcription.delta"); d.Get("delta").String() != "یہ اردو" || d.Get("language").String() != "ur" {
		t.Fatalf("urdu delta = %s", r)
	}
}

func TestRealtimeAudioMapperInterruptedResponseIsCancelled(t *testing.T) {
	m := newRealtimeAudioMapper(nil)
	mapVoiceEvents(m,
		`{"turn":{"id":"a1","role":"assistant","start_ms":7000},"type":"turn.created"}`,
		`{"item":{"text":" At the edge"},"type":"output_transcript.added"}`,
	)
	over := m.MapUpstream([]byte(`{"turn":{"id":"u2","role":"user","start_ms":11400},"type":"turn.created"}`))
	interrupted := findVoiceEvent(over, "response.output_audio.interrupted")
	if !interrupted.Exists() || interrupted.Get("response_id").String() == "" || interrupted.Get("item_id").String() == "" {
		t.Fatalf("over = %s", over)
	}
	cut := m.MapUpstream([]byte(`{"turn":{"id":"a1","role":"assistant","end_ms":11600,"transcript":" At the edge"},"type":"turn.done"}`))
	done := findVoiceEvent(cut, "response.done")
	if done.Get("response.status").String() != "cancelled" || done.Get("response.status_details.reason").String() != "turn_detected" || done.Get("response.id").String() != interrupted.Get("response_id").String() {
		t.Fatalf("response.done = %s", done.Raw)
	}
	if item := findVoiceEvent(cut, "response.output_item.done"); item.Get("item.status").String() != "incomplete" {
		t.Errorf("item = %s", item.Raw)
	}
	if findVoiceEvent(cut, "response.output_audio.interrupted").Exists() {
		t.Error("the interruption was reported twice")
	}

	// Probe order: the assistant turn.done first and the user turn.created in the same moment; the response already ended as completed, and the interruption names it.
	m2 := newRealtimeAudioMapper(nil)
	mapVoiceEvents(m2, `{"turn":{"id":"a1","role":"assistant","start_ms":7000},"type":"turn.created"}`)
	closed := m2.MapUpstream([]byte(`{"turn":{"id":"a1","role":"assistant","end_ms":11600,"transcript":"x"},"type":"turn.done"}`))
	responseID := findVoiceEvent(closed, "response.done").Get("response.id").String()
	late := m2.MapUpstream([]byte(`{"turn":{"id":"u2","role":"user","start_ms":11400},"type":"turn.created"}`))
	if got := findVoiceEvent(late, "response.output_audio.interrupted"); got.Get("response_id").String() != responseID {
		t.Fatalf("late interruption = %s", late)
	}

	// A normal exchange has no interruption.
	m3 := newRealtimeAudioMapper(nil)
	normal := mapVoiceEvents(m3,
		`{"turn":{"id":"u1","role":"user","start_ms":0},"type":"turn.created"}`,
		`{"turn":{"id":"a1","role":"assistant","start_ms":2000},"type":"turn.created"}`,
		`{"turn":{"id":"u1","role":"user","end_ms":2000},"type":"turn.done"}`,
		`{"turn":{"id":"a1","role":"assistant","end_ms":5000},"type":"turn.done"}`,
	)
	if findVoiceEvent(normal, "response.output_audio.interrupted").Exists() || findVoiceEvent(normal, "response.done").Get("response.status").String() != "completed" {
		t.Fatalf("normal exchange = %s", voiceEventTypes(normal))
	}
}

func TestAudioBridgeDetectorEmitsSpeechEventsOnInputAudio(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+voiceBase64(vadConcat(vadSilence(300), vadTone(600, 8000, 220)))+`"}`)
	started := readBridgeUntil(t, conn, "input_audio_buffer.speech_started")
	if gjson.GetBytes(started, "audio_start_ms").Int() != 300 || gjson.GetBytes(started, "item_id").String() == "" {
		t.Fatalf("speech_started = %s", started)
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+voiceBase64(vadSilence(700))+`"}`)
	stopped := readBridgeUntil(t, conn, "input_audio_buffer.speech_stopped")
	if gjson.GetBytes(stopped, "audio_end_ms").Int() != 900 || gjson.GetBytes(stopped, "item_id").String() != gjson.GetBytes(started, "item_id").String() {
		t.Fatalf("speech_stopped = %s", stopped)
	}
}

func TestAudioBridge16kHzInputIsAnalysedAfterResampling(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"audio":{"input":{"format":{"type":"audio/pcm","rate":16000}}}}}`)
	readBridgeUntil(t, conn, "session.updated")
	// 500 ms at 16 kHz: 8000 samples of a loud tone.
	samples := make([]byte, 0, 16000)
	for i := 0; i < 8000; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*float64(i)*220/16000))
		samples = append(samples, byte(v), byte(v>>8))
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+voiceBase64(samples)+`"}`)
	started := readBridgeUntil(t, conn, "input_audio_buffer.speech_started")
	if ms := gjson.GetBytes(started, "audio_start_ms").Int(); ms != 0 {
		t.Fatalf("speech_started = %s", started)
	}
}

func TestAudioBridgeTranscriptionLanguageSessionFlow(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"event_id":"bad","type":"session.update","session":{"audio":{"input":{"transcription":{"language":"gujarati"}}}}}`)
	bad := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(bad, "error.code").String() != "invalid_value" || gjson.GetBytes(bad, "error.param").String() != "session.audio.input.transcription.language" || gjson.GetBytes(bad, "error.client_event_id").String() != "bad" {
		t.Fatalf("error = %s", bad)
	}

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"instructions":"Help.","audio":{"input":{"transcription":{"model":"gpt-4o-transcribe","language":"GU"}}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.audio.input.transcription.language").String() != "gu" {
		t.Fatalf("ack = %s", ack)
	}
	session := startBridgeCall(t, srv, conn)
	items := gjson.GetBytes(session, "initial_items").Array()
	if len(items) != 2 || items[0].Get("content.0.text").String() != "Help." || !strings.Contains(items[1].Get("content.0.text").String(), "Gujarati") || items[1].Get("role").String() != "developer" {
		t.Fatalf("initial_items = %s", gjson.GetBytes(session, "initial_items").Raw)
	}

	srv.call.events <- []byte(`{"item":{"text":"नमस्ते આપ"},"type":"input_transcript.added"}`)
	delta := readBridgeUntil(t, conn, "conversation.item.input_audio_transcription.delta")
	if gjson.GetBytes(delta, "delta").String() != "નમસ્તે આપ" || gjson.GetBytes(delta, "language").String() != "gu" {
		t.Fatalf("delta = %s", delta)
	}

	// The language is fixed when the call is created, like the instructions and the voice.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"audio":{"input":{"transcription":{"language":"hi"}}}}}`)
	immutable := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(immutable, "error.code").String() != "immutable_field_update" || gjson.GetBytes(immutable, "error.param").String() != "session.audio.input.transcription.language" {
		t.Fatalf("error = %s", immutable)
	}
}

// TestAudioBridgeClientDelegationFlowFromATextFirstConnection is the whole customer flow with a fake upstream: a connection that names no model in the URL, a session.update that only carries the client delegation, a spoken request, session.delegation.created, and the results coming back through session.commentary.append and session.thinking.append.
func TestAudioBridgeClientDelegationFlowFromATextFirstConnection(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "")
	created := readBridgeEvent(t, conn)
	if gjson.GetBytes(created, "type").String() != "session.created" {
		t.Fatalf("first event = %s", created)
	}

	// Only the delegation, no output_modalities: Realtime GA leaves the audio model on audio, and the delegation exists on the audio bridge only.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"type":"realtime","model":"gpt-realtime","instructions":"Hand every action to the app.","delegation":{"type":"client","acknowledgement":"none"}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.delegation.type").String() != "client" || gjson.GetBytes(ack, "session.delegation.acknowledgement").String() != "none" || gjson.GetBytes(ack, "session.output_modalities.0").String() != "audio" {
		t.Fatalf("ack = %s", ack)
	}

	// The first audio creates the call with the client delegation and no spoken filler.
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+voiceBase64(vadSilence(100))+`"}`)
	var session []byte
	select {
	case session = <-srv.session:
	case <-time.After(bridgeTestTimeout):
		t.Fatal("the call was not created")
	}
	if gjson.GetBytes(session, "delegation.type").String() != "client" || gjson.GetBytes(session, "delegation.ack_filler").Bool() {
		t.Fatalf("live session = %s", session)
	}

	// The upstream delegation.created, with item.content as the probes saw it.
	srv.call.events <- []byte(`{"type":"delegation.created","offset_ms":5400,"item":{"content":[{"text":"Open Safari and search the weather in Mumbai","type":"input_text"}],"handoff_id":"handoff_1","id":"item_EgpAsbQKA3KZRqH39fKdX","target":"client","type":"delegation","user_bidi_turn_id":"turn_1"}}`)
	delegation := readBridgeUntil(t, conn, "session.delegation.created")
	if gjson.GetBytes(delegation, "delegation.id").String() != "item_EgpAsbQKA3KZRqH39fKdX" || gjson.GetBytes(delegation, "delegation.content").String() != "Open Safari and search the weather in Mumbai" || gjson.GetBytes(delegation, "offset_ms").Int() != 5400 {
		t.Fatalf("session.delegation.created = %s", delegation)
	}
	id := gjson.GetBytes(delegation, "delegation.id").String()

	writeBridgeEvent(t, conn, `{"event_id":"t1","type":"session.thinking.append","delegation_id":"`+id+`","content":"Opening Safari."}`)
	sent := takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "delegation.context.append" || gjson.GetBytes(sent, "channel").String() != "commentary" || gjson.GetBytes(sent, "delegation_item_id").String() != id {
		t.Fatalf("thinking sideband = %s", sent)
	}
	writeBridgeEvent(t, conn, `{"event_id":"c1","type":"session.commentary.append","delegation_id":"`+id+`","content":"It is 31 degrees and sunny in Mumbai."}`)
	sent = takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "channel").String() != "speakable" || gjson.GetBytes(sent, "content.0.text").String() != "It is 31 degrees and sunny in Mumbai." {
		t.Fatalf("commentary sideband = %s", sent)
	}

	srv.call.events <- []byte(`{"type":"delegation.context.appended","delegation_item_id":"` + id + `","start_ms":11800,"end_ms":12000}`)
	srv.call.events <- []byte(`{"type":"delegation.context.appended","delegation_item_id":"` + id + `","start_ms":12000,"end_ms":12200}`)
	first := readBridgeUntil(t, conn, "session.thinking.appended")
	second := readBridgeUntil(t, conn, "session.commentary.appended")
	if gjson.GetBytes(first, "client_event_id").String() != "t1" || gjson.GetBytes(second, "client_event_id").String() != "c1" {
		t.Fatalf("acks = %s %s", first, second)
	}
}
