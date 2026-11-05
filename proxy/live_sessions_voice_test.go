package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func liveVoiceAudio(pcm []byte) string {
	return `{"type":"session.input_audio.append","audio":"` + base64.StdEncoding.EncodeToString(pcm) + `"}`
}

func TestLiveSessionTranscriptionLanguageValidation(t *testing.T) {
	cfg := liveTestConfig(t, `{"audio":{"input":{"transcription":{"language":"GU"}}}}`)
	if cfg.Language != "gu" {
		t.Fatalf("language = %q", cfg.Language)
	}
	raw, _ := json.Marshal(cfg.PublicSession("rtc_1"))
	if got := gjson.GetBytes(raw, "audio.input.transcription.language").String(); got != "gu" {
		t.Errorf("public session language = %q: %s", got, raw)
	}
	if plain := liveTestConfig(t, `{}`); plain.Language != "" {
		t.Errorf("default language = %q", plain.Language)
	} else if raw, _ := json.Marshal(plain.PublicSession("x")); gjson.GetBytes(raw, "audio.input").Exists() {
		t.Errorf("an unset language leaked into the session: %s", raw)
	}

	for _, tt := range []struct{ session, code, param string }{
		{`{"audio":{"input":{"transcription":{"language":"gujarati"}}}}`, "invalid_value", "session.audio.input.transcription.language"},
		{`{"audio":{"input":{"transcription":{"language":5}}}}`, "invalid_value", "session.audio.input.transcription.language"},
		{`{"audio":{"input":{"transcription":{"language":"en-US"}}}}`, "invalid_value", "session.audio.input.transcription.language"},
		{`{"audio":{"input":{"transcription":{"model":"whisper-1"}}}}`, "unknown_parameter", "session.audio.input.transcription.model"},
		{`{"audio":{"input":{"format":{}}}}`, "unknown_parameter", "session.audio.input.format"},
		{`{"audio":{"input":{"transcription":"gu"}}}`, "invalid_value", "session.audio.input.transcription"},
		{`{"audio":{"input":"gu"}}`, "invalid_value", "session.audio.input"},
	} {
		_, e := parseLiveSessionObject(gjson.Parse(tt.session), "")
		if e == nil || e.Code != tt.code || e.Param != tt.param {
			t.Errorf("%s -> %+v, want %s %s", tt.session, e, tt.code, tt.param)
		}
	}
}

func TestLiveSessionTranscriptionLanguageAddsOneDeveloperItemAndKeepsTheItemCap(t *testing.T) {
	cfg := liveTestConfig(t, `{"instructions":"Be brief.","audio":{"input":{"transcription":{"language":"gu"}}},"input":[{"role":"user","content":"hi"}]}`)
	session := gjson.ParseBytes(cfg.UpstreamSession())
	items := session.Get("initial_items").Array()
	if len(items) != 3 {
		t.Fatalf("items = %s", session.Get("initial_items").Raw)
	}
	if items[0].Get("content.0.text").String() != "Be brief." || items[0].Get("role").String() != "developer" {
		t.Errorf("first item = %s", items[0].Raw)
	}
	lang := items[1]
	if lang.Get("role").String() != "developer" || !strings.Contains(lang.Get("content.0.text").String(), "Gujarati") || !strings.Contains(lang.Get("content.0.text").String(), "unless the user switches") {
		t.Errorf("language item = %s", lang.Raw)
	}
	if items[2].Get("role").String() != "user" {
		t.Errorf("history item = %s", items[2].Raw)
	}
	// A language without instructions is the only gateway item.
	only := gjson.ParseBytes(liveTestConfig(t, `{"audio":{"input":{"transcription":{"language":"hi"}}}}`).UpstreamSession())
	if only.Get("initial_items.#").Int() != 1 || only.Get("instructions").Exists() {
		t.Errorf("language only = %s", only.Raw)
	}

	msgs := make([]string, 127)
	for i := range msgs {
		msgs[i] = `{"role":"user","content":"x"}`
	}
	join := func(n int) string { return strings.Join(msgs[:n], ",") }
	// Both gateway items leave room for 126 history items.
	ok := liveTestConfig(t, `{"instructions":"Hi","audio":{"input":{"transcription":{"language":"gu"}}},"input":[`+join(126)+`]}`)
	if got := gjson.ParseBytes(ok.UpstreamSession()).Get("initial_items.#").Int(); got != 128 {
		t.Errorf("126 items with both -> %d upstream items, want 128", got)
	}
	_, e := parseLiveSessionObject(gjson.Parse(`{"instructions":"Hi","audio":{"input":{"transcription":{"language":"gu"}}},"input":[`+join(127)+`]}`), "")
	if e == nil || e.Param != "session.input" || !strings.Contains(e.Message, "126") || !strings.Contains(e.Message, "session.audio.input.transcription.language") {
		t.Errorf("127 items with both -> %+v", e)
	}
	// With the language alone the cap is 127.
	if _, e := parseLiveSessionObject(gjson.Parse(`{"audio":{"input":{"transcription":{"language":"gu"}}},"input":[`+join(127)+`]}`), ""); e != nil {
		t.Errorf("127 items with the language only -> %+v", e)
	}
}

func TestLiveSessionLanguageOnInputTranscripts(t *testing.T) {
	tr, _ := liveTestStarted(t, `{"audio":{"input":{"transcription":{"language":"gu"}}}}`)
	in := tr.Upstream([]byte(`{"end_ms":2200,"item":{"text":" आप કેમ છો","type":"input_transcript"},"start_ms":2000,"type":"input_transcript.added"}`))
	got := gjson.ParseBytes(in.Client[0])
	if got.Get("delta").String() != " આપ કેમ છો" || got.Get("language").String() != "gu" {
		t.Fatalf("converted delta = %s", in.Client[0])
	}
	// Even a single short word carries the language the client named.
	short := tr.Upstream([]byte(`{"item":{"text":"हाँ","type":"input_transcript"},"type":"input_transcript.added"}`))
	if g := gjson.ParseBytes(short.Client[0]); g.Get("language").String() != "gu" || g.Get("delta").String() != "હાઁ" {
		t.Fatalf("short delta = %s", short.Client[0])
	}
	// Output transcripts are neither converted nor labelled with the input language: the detected one (here from the script) is reported.
	out := tr.Upstream([]byte(`{"item":{"text":"नमस्ते","type":"output_transcript"},"type":"output_transcript.added"}`))
	if g := gjson.ParseBytes(out.Client[0]); g.Get("delta").String() != "नमस्ते" || g.Get("language").String() != "hi" {
		t.Fatalf("output delta = %s", out.Client[0])
	}
	// Another language is reported as given and the script is left alone.
	hi, _ := liveTestStarted(t, `{"audio":{"input":{"transcription":{"language":"hi"}}}}`)
	r := hi.Upstream([]byte(`{"item":{"text":"नमस्ते","type":"input_transcript"},"type":"input_transcript.added"}`))
	if g := gjson.ParseBytes(r.Client[0]); g.Get("language").String() != "hi" || g.Get("delta").String() != "नमस्ते" {
		t.Fatalf("hindi delta = %s", r.Client[0])
	}
}

func TestLiveSessionUpstreamSpeechEventsWhereTheGatewayHasNoAudio(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)
	created := tr.Upstream([]byte(`{"turn":{"end_ms":2000,"id":"turn_u1","role":"user","start_ms":1800,"transcript":"Please"},"type":"turn.created"}`))
	if len(created.Client) != 1 {
		t.Fatalf("turn.created = %v", liveTestStr(created.Client))
	}
	if g := gjson.ParseBytes(created.Client[0]); g.Get("type").String() != "session.input_audio.speech_started" || g.Get("audio_start_ms").Int() != 1800 || g.Get("source").String() != "upstream" {
		t.Fatalf("speech_started = %s", created.Client[0])
	}
	done := tr.Upstream([]byte(`{"turn":{"end_ms":14600,"id":"turn_u1","role":"user","start_ms":1800,"transcript":"Please tell me"},"type":"turn.done"}`))
	if len(done.Client) != 1 {
		t.Fatalf("turn.done = %v", liveTestStr(done.Client))
	}
	if g := gjson.ParseBytes(done.Client[0]); g.Get("type").String() != "session.input_audio.speech_stopped" || g.Get("audio_end_ms").Int() != 14600 || g.Get("source").String() != "upstream" {
		t.Fatalf("speech_stopped = %s", done.Client[0])
	}
	// An assistant turn is not user speech.
	if r := tr.Upstream([]byte(`{"turn":{"id":"turn_a1","role":"assistant","start_ms":3000},"type":"turn.created"}`)); len(r.Client) != 0 {
		t.Errorf("assistant turn.created = %v", liveTestStr(r.Client))
	}
}

func TestLiveSessionDetectorSpeechSilencesTheUpstreamDuplicates(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)
	events := tr.Speech([]audioSpeechEvent{{Started: true, MS: 500}, {Started: false, MS: 1500}})
	if len(events) != 2 {
		t.Fatalf("speech = %v", liveTestStr(events))
	}
	if g := gjson.ParseBytes(events[0]); g.Get("type").String() != "session.input_audio.speech_started" || g.Get("audio_start_ms").Int() != 500 || g.Get("source").Exists() {
		t.Errorf("detector start = %s", events[0])
	}
	if g := gjson.ParseBytes(events[1]); g.Get("type").String() != "session.input_audio.speech_stopped" || g.Get("audio_end_ms").Int() != 1500 {
		t.Errorf("detector stop = %s", events[1])
	}
	// The user turn that reaches the sideband 1.5 s later was already reported.
	if r := tr.Upstream([]byte(`{"turn":{"id":"turn_u1","role":"user","start_ms":400},"type":"turn.created"}`)); len(r.Client) != 0 {
		t.Errorf("turn.created repeated the start: %v", liveTestStr(r.Client))
	}
	if r := tr.Upstream([]byte(`{"turn":{"id":"turn_u1","role":"user","end_ms":1600},"type":"turn.done"}`)); len(r.Client) != 0 {
		t.Errorf("turn.done repeated the stop: %v", liveTestStr(r.Client))
	}
}

func TestLiveSessionInterruptedWhenTheUserSpeaksOverTheAssistant(t *testing.T) {
	// A user turn.created while the assistant turn is open.
	tr, _ := liveTestStarted(t, `{}`)
	tr.Upstream([]byte(`{"turn":{"id":"turn_a1","role":"assistant","start_ms":7000},"type":"turn.created"}`))
	over := tr.Upstream([]byte(`{"turn":{"id":"turn_u2","role":"user","start_ms":11400,"transcript":" Stop"},"type":"turn.created"}`))
	types := []string{}
	for _, e := range over.Client {
		types = append(types, gjson.GetBytes(e, "type").String())
	}
	if strings.Join(types, ",") != "session.input_audio.speech_started,session.output_audio.interrupted" {
		t.Fatalf("events = %v", liveTestStr(over.Client))
	}
	if g := gjson.ParseBytes(over.Client[1]); g.Get("turn_id").String() != "turn_a1" {
		t.Errorf("interrupted = %s", over.Client[1])
	}
	// The cut turn.done that follows adds nothing.
	if r := tr.Upstream([]byte(`{"turn":{"end_ms":11600,"id":"turn_a1","role":"assistant","start_ms":7000,"transcript":" At the edge,"},"type":"turn.done"}`)); len(r.Client) != 0 {
		t.Errorf("cut turn.done = %v", liveTestStr(r.Client))
	}

	// The probe order: the assistant turn.done first, the user turn.created in the same moment.
	tr2, _ := liveTestStarted(t, `{}`)
	tr2.Upstream([]byte(`{"turn":{"id":"turn_a1","role":"assistant","start_ms":7000},"type":"turn.created"}`))
	tr2.Upstream([]byte(`{"turn":{"id":"turn_a1","role":"assistant","end_ms":11600},"type":"turn.done"}`))
	r := tr2.Upstream([]byte(`{"turn":{"id":"turn_u2","role":"user","start_ms":11400},"type":"turn.created"}`))
	if len(r.Client) != 2 || gjson.GetBytes(r.Client[1], "type").String() != "session.output_audio.interrupted" || gjson.GetBytes(r.Client[1], "turn_id").String() != "turn_a1" {
		t.Fatalf("done-then-created = %v", liveTestStr(r.Client))
	}

	// A normal exchange never interrupts.
	tr3, _ := liveTestStarted(t, `{}`)
	for _, raw := range []string{
		`{"turn":{"id":"u1","role":"user","start_ms":0},"type":"turn.created"}`,
		`{"turn":{"id":"a1","role":"assistant","start_ms":2000},"type":"turn.created"}`,
		`{"turn":{"id":"u1","role":"user","end_ms":2000},"type":"turn.done"}`,
		`{"turn":{"id":"a1","role":"assistant","end_ms":5000},"type":"turn.done"}`,
	} {
		for _, e := range tr3.Upstream([]byte(raw)).Client {
			if gjson.GetBytes(e, "type").String() == "session.output_audio.interrupted" {
				t.Fatalf("%s interrupted a normal exchange", raw)
			}
		}
	}
}

func TestLiveSessionsWSDetectorEmitsSpeechEventsFromTheClientAudio(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)

	liveWSWrite(t, conn, liveVoiceAudio(vadConcat(vadSilence(300), vadTone(600, 8000, 220))))
	started := liveWSReadUntil(t, conn, "session.input_audio.speech_started")
	if started0 := gjson.GetBytes(started, "audio_start_ms").Int(); started0 != 300 {
		t.Fatalf("speech_started = %s", started)
	}
	if gjson.GetBytes(started, "source").Exists() {
		t.Errorf("a detector event carries a source: %s", started)
	}
	liveWSWrite(t, conn, liveVoiceAudio(vadSilence(700)))
	stopped := liveWSReadUntil(t, conn, "session.input_audio.speech_stopped")
	if gjson.GetBytes(stopped, "audio_end_ms").Int() != 900 {
		t.Fatalf("speech_stopped = %s", stopped)
	}

	// The upstream user turn that arrives later adds no second start or stop.
	srv.call.push(`{"turn":{"id":"turn_u1","role":"user","start_ms":300},"type":"turn.created"}`)
	srv.call.push(`{"turn":{"id":"turn_u1","role":"user","end_ms":900},"type":"turn.done"}`)
	srv.call.push(`{"item":{"text":"Hello","type":"input_transcript"},"type":"input_transcript.added"}`)
	for {
		event := liveWSRead(t, conn)
		typ := gjson.GetBytes(event, "type").String()
		if typ == "session.input_audio.speech_started" || typ == "session.input_audio.speech_stopped" {
			t.Fatalf("speech event repeated from the upstream turn: %s", event)
		}
		if typ == "session.input_transcript.delta" {
			break
		}
	}
}

func TestLiveSessionsWSMuteEndsAnOpenSegmentAndSkipsTheDetector(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)

	liveWSWrite(t, conn, liveVoiceAudio(vadTone(400, 8000, 220)))
	liveWSReadUntil(t, conn, "session.input_audio.speech_started")
	liveWSWrite(t, conn, `{"type":"session.input_audio.mute"}`)
	stopped := liveWSReadUntil(t, conn, "session.input_audio.speech_stopped")
	if gjson.GetBytes(stopped, "audio_end_ms").Int() != 400 {
		t.Fatalf("speech_stopped = %s", stopped)
	}
	// While muted the detector hears nothing, so loud audio starts no segment.
	liveWSWrite(t, conn, liveVoiceAudio(vadTone(600, 8000, 220)))
	liveWSWrite(t, conn, `{"type":"session.input_audio.unmute"}`)
	liveWSWrite(t, conn, liveVoiceAudio(vadTone(300, 8000, 220)))
	started := liveWSReadUntil(t, conn, "session.input_audio.speech_started")
	if gjson.GetBytes(started, "audio_start_ms").Int() != 400 {
		t.Fatalf("speech after unmute = %s", started)
	}
}

func TestLiveSessionsWSInterruptedAndUpstreamSpeechFallback(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)

	srv.call.push(`{"turn":{"id":"turn_a1","role":"assistant","start_ms":7000},"type":"turn.created"}`)
	srv.call.push(`{"turn":{"id":"turn_u2","role":"user","start_ms":11400,"transcript":" Stop"},"type":"turn.created"}`)
	started := liveWSReadUntil(t, conn, "session.input_audio.speech_started")
	if gjson.GetBytes(started, "source").String() != "upstream" || gjson.GetBytes(started, "audio_start_ms").Int() != 11400 {
		t.Fatalf("fallback start = %s", started)
	}
	interrupted := liveWSReadUntil(t, conn, "session.output_audio.interrupted")
	if gjson.GetBytes(interrupted, "turn_id").String() != "turn_a1" {
		t.Fatalf("interrupted = %s", interrupted)
	}
}

func TestLiveSessionsWSTranscriptionLanguageReachesTheCallAndTheTranscripts(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSWrite(t, conn, `{"type":"session.start","session":{"instructions":"Help.","audio":{"input":{"transcription":{"language":"gu"}}}}}`)
	var upstream []byte
	select {
	case upstream = <-srv.session:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not opened")
	}
	if got := gjson.GetBytes(upstream, "initial_items.#").Int(); got != 2 || !strings.Contains(gjson.GetBytes(upstream, "initial_items.1.content.0.text").String(), "Gujarati") {
		t.Fatalf("upstream session = %s", upstream)
	}
	srv.call.push(`{"type":"session.started","session":{"id":"sess_up1"}}`)
	started := liveWSReadUntil(t, conn, "session.started")
	if gjson.GetBytes(started, "session.audio.input.transcription.language").String() != "gu" {
		t.Fatalf("session.started = %s", started)
	}
	srv.call.push(`{"item":{"text":"નમસ્તે आप","type":"input_transcript"},"type":"input_transcript.added"}`)
	delta := liveWSReadUntil(t, conn, "session.input_transcript.delta")
	if gjson.GetBytes(delta, "delta").String() != "નમસ્તે આપ" || gjson.GetBytes(delta, "language").String() != "gu" {
		t.Fatalf("delta = %s", delta)
	}
}

func TestLiveSessionPinnedGujaratiReportsArabicScript(t *testing.T) {
	tr, _ := liveTestStarted(t, `{"audio":{"input":{"transcription":{"language":"gu"}}}}`)
	r := tr.Upstream([]byte(`{"item":{"text":"یہ اردو","type":"input_transcript"},"type":"input_transcript.added"}`))
	g := gjson.ParseBytes(r.Client[0])
	if g.Get("delta").String() != "یہ اردو" || g.Get("language").String() != "gu" || g.Get("script").String() != "arab" {
		t.Fatalf("arabic delta = %s", r.Client[0])
	}
	// Gujarati and converted Devanagari carry no script field (a fresh session: the turn's accumulator is empty).
	tr, _ = liveTestStarted(t, `{"audio":{"input":{"transcription":{"language":"gu"}}}}`)
	ok := tr.Upstream([]byte(`{"item":{"text":" आप","type":"input_transcript"},"type":"input_transcript.added"}`))
	if gjson.ParseBytes(ok.Client[0]).Get("script").Exists() {
		t.Fatalf("script on %s", ok.Client[0])
	}
}
