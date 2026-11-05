package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func liveTestConfig(t *testing.T, session string) *liveSessionConfig {
	t.Helper()
	cfg, e := parseLiveSessionObject(gjson.Parse(session), "")
	if e != nil {
		t.Fatalf("parseLiveSessionObject(%s) = %+v", session, e)
	}
	return cfg
}

func liveTestStr(events [][]byte) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e)
	}
	return out
}

func TestLiveSessionValidationDefaults(t *testing.T) {
	cfg := liveTestConfig(t, `{}`)
	if cfg.Model != "gpt-live-1" || cfg.Voice != "marin" || cfg.LiveVoice != "maple" || cfg.FormatType != "audio/pcm" || cfg.FormatRate != 24000 || !cfg.AckFiller || cfg.AckDelayMS != 0 || cfg.Store != nil || len(cfg.InitialItems) != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
	cfg, e := parseLiveSessionStart([]byte(`{"type":"session.start","event_id":"e1","session":{"instructions":"Hi","model":"gpt-live-1-codex","audio":{"format":{"type":"audio/pcm","rate":16000},"output":{"voice":"Nira"}},"store":false}}`))
	if e != nil {
		t.Fatalf("start = %+v", e)
	}
	if cfg.Model != "gpt-live-1-codex" || cfg.Instructions != "Hi" || cfg.Voice != "nira" || cfg.FormatRate != 16000 || cfg.Store == nil || *cfg.Store {
		t.Fatalf("cfg = %+v", cfg)
	}
	if got := liveTestConfig(t, `{"model":"gpt-realtime"}`).Model; got != "gpt-realtime" {
		t.Fatalf("realtime model = %q", got)
	}
}

func TestLiveSessionValidationUnknownFields(t *testing.T) {
	tests := []struct{ session, param string }{
		{`{"foo":1}`, "session.foo"},
		{`{"audio":{"foo":1}}`, "session.audio.foo"},
		{`{"audio":{"format":{"foo":1}}}`, "session.audio.format.foo"},
		{`{"audio":{"output":{"speed":1}}}`, "session.audio.output.speed"},
		{`{"delegation":{"foo":1}}`, "session.delegation.foo"},
		{`{"input":[{"type":"message","role":"user","content":"x","foo":1}]}`, "session.input.0.foo"},
		{`{"input":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"input_text","text":"x","foo":1}]}]}`, "session.input.1.content.0.foo"},
	}
	for _, tt := range tests {
		_, e := parseLiveSessionObject(gjson.Parse(tt.session), "ev1")
		if e == nil || e.Code != "unknown_parameter" || e.Param != tt.param || e.ClientEventID != "ev1" {
			t.Errorf("%s -> %+v, want unknown_parameter %s", tt.session, e, tt.param)
		}
	}
	// A top-level unknown field of session.start.
	if _, e := parseLiveSessionStart([]byte(`{"type":"session.start","session":{},"foo":1}`)); e == nil || e.Code != "unknown_parameter" || e.Param != "foo" {
		t.Errorf("start unknown field -> %+v", e)
	}
}

func TestLiveSessionValidationBadValues(t *testing.T) {
	big := strings.Repeat("a", 4*16384+8)
	tests := []struct{ session, code, param string }{
		{`[]`, "invalid_value", "session"},
		{`{"model":"gpt-5"}`, "unsupported_value", "session.model"},
		{`{"model":3}`, "invalid_value", "session.model"},
		{`{"instructions":5}`, "invalid_value", "session.instructions"},
		{`{"instructions":"` + big + `"}`, "invalid_value", "session.instructions"},
		{`{"audio":{"output":{"voice":"nope"}}}`, "unsupported_value", "session.audio.output.voice"},
		{`{"audio":{"format":{"type":"audio/opus"}}}`, "unsupported_value", "session.audio.format.type"},
		{`{"audio":{"format":{"type":"audio/pcm","rate":8000}}}`, "unsupported_value", "session.audio.format.rate"},
		{`{"audio":{"format":{"type":"audio/pcmu","rate":24000}}}`, "unsupported_value", "session.audio.format.rate"},
		{`{"audio":{"format":{"type":"audio/pcma","rate":"8000"}}}`, "unsupported_value", "session.audio.format.rate"},
		{`{"audio":{"format":{"type":"audio/pcm","rate":16000.5}}}`, "unsupported_value", "session.audio.format.rate"},
		{`{"delegation":{"type":"responses"}}`, "unsupported_value", "session.delegation.type"},
		{`{"delegation":{"type":"other"}}`, "unsupported_value", "session.delegation.type"},
		{`{"delegation":{"acknowledgement_delay_ms":-1}}`, "invalid_value", "session.delegation.acknowledgement_delay_ms"},
		{`{"delegation":{"acknowledgement_delay_ms":1.5}}`, "invalid_value", "session.delegation.acknowledgement_delay_ms"},
		{`{"delegation":{"acknowledgement":"sometimes"}}`, "unsupported_value", "session.delegation.acknowledgement"},
		{`{"store":"yes"}`, "invalid_value", "session.store"},
		{`{"input":"hello"}`, "invalid_value", "session.input"},
		{`{"input":[{"role":"robot","content":"x"}]}`, "unsupported_value", "session.input.0.role"},
		{`{"input":[{"role":"user"}]}`, "missing_required_parameter", "session.input.0.content"},
		{`{"input":[{"role":"user","content":[{"type":"image","text":"x"}]}]}`, "unsupported_value", "session.input.0.content.0.type"},
		{`{"input":[{"type":"function_call","role":"user","content":"x"}]}`, "unsupported_value", "session.input.0.type"},
	}
	for _, tt := range tests {
		_, e := parseLiveSessionObject(gjson.Parse(tt.session), "")
		if e == nil || e.Code != tt.code || e.Param != tt.param {
			s := tt.session
			if len(s) > 80 {
				s = s[:80]
			}
			t.Errorf("%s -> %+v, want %s %s", s, e, tt.code, tt.param)
		}
	}
}

func TestLiveSessionValidationFormats(t *testing.T) {
	for _, tt := range []struct {
		session string
		typ     string
		rate    int
	}{
		{`{"audio":{"format":{"type":"audio/pcm"}}}`, "audio/pcm", 24000},
		{`{"audio":{"format":{"rate":16000}}}`, "audio/pcm", 16000},
		{`{"audio":{"format":{"type":"audio/pcmu"}}}`, "audio/pcmu", 8000},
		{`{"audio":{"format":{"type":"audio/pcma","rate":8000}}}`, "audio/pcma", 8000},
	} {
		cfg := liveTestConfig(t, tt.session)
		if cfg.FormatType != tt.typ || cfg.FormatRate != tt.rate {
			t.Errorf("%s -> %s %d", tt.session, cfg.FormatType, cfg.FormatRate)
		}
	}
}

func TestLiveSessionValidationStart(t *testing.T) {
	tests := []struct{ raw, code, param, clientID string }{
		{`not json`, "invalid_value", "", ""},
		{`[]`, "invalid_value", "", ""},
		{`{"session":{}}`, "missing_required_parameter", "type", ""},
		{`{"type":"session.update","event_id":"x","session":{}}`, "invalid_value", "type", "x"},
		{`{"type":"session.start","event_id":"s1"}`, "missing_required_parameter", "session", "s1"},
		{`{"type":"session.start","event_id":"s2","session":{"voice":"marin"}}`, "unknown_parameter", "session.voice", "s2"},
	}
	for _, tt := range tests {
		_, e := parseLiveSessionStart([]byte(tt.raw))
		if e == nil || e.Code != tt.code || e.Param != tt.param || e.ClientEventID != tt.clientID {
			t.Errorf("%s -> %+v, want %s %q %q", tt.raw, e, tt.code, tt.param, tt.clientID)
		}
	}
}

func TestLiveSessionErrorEvent(t *testing.T) {
	ev := (&liveSessionError{Code: "unknown_parameter", Param: "session.x", Message: "m", ClientEventID: "c"}).Event()
	root := gjson.ParseBytes(ev)
	if root.Get("type").String() != "error" || root.Get("error.type").String() != "invalid_request_error" || root.Get("error.code").String() != "unknown_parameter" || root.Get("error.param").String() != "session.x" || root.Get("error.client_event_id").String() != "c" || root.Get("error.message").String() != "m" {
		t.Fatalf("event = %s", ev)
	}
	ev = (&liveSessionError{Code: "invalid_value", Message: "m"}).Event()
	root = gjson.ParseBytes(ev)
	if root.Get("error.param").Type != gjson.Null || root.Get("error.client_event_id").Type != gjson.Null || !root.Get("error.param").Exists() {
		t.Fatalf("null fields = %s", ev)
	}
}

func TestLiveSessionInputConversion(t *testing.T) {
	cfg := liveTestConfig(t, `{"input":[
		{"type":"message","role":"developer","content":"Greet in Hindi."},
		{"role":"system","content":[{"type":"input_text","text":"Be brief."},{"type":"text","text":" Really."}]},
		{"role":"user","content":"Hello"},
		{"role":"assistant","content":[{"type":"output_text","text":"Namaste"}]}]}`)
	var got []string
	for _, item := range cfg.InitialItems {
		got = append(got, string(item))
	}
	want := []string{
		`{"content":[{"text":"Greet in Hindi.","type":"input_text"}],"role":"developer","type":"message"}`,
		`{"content":[{"text":"Be brief.","type":"input_text"},{"text":" Really.","type":"input_text"}],"role":"developer","type":"message"}`,
		`{"content":[{"text":"Hello","type":"input_text"}],"role":"user","type":"message"}`,
		`{"content":[{"text":"Namaste","type":"output_text"}],"role":"assistant","type":"message"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLiveSessionInputCaps(t *testing.T) {
	var items []string
	for i := 0; i < 129; i++ {
		items = append(items, `{"role":"user","content":"x"}`)
	}
	_, e := parseLiveSessionObject(gjson.Parse(`{"input":[`+strings.Join(items, ",")+`]}`), "")
	if e == nil || e.Code != "invalid_value" || e.Param != "session.input" {
		t.Errorf("129 items -> %+v", e)
	}
	if cfg := liveTestConfig(t, `{"input":[`+strings.Join(items[:128], ",")+`]}`); len(cfg.InitialItems) != 128 {
		t.Errorf("128 items kept %d", len(cfg.InitialItems))
	}
	text := strings.Repeat("a", 4*4096+4)
	_, e = parseLiveSessionObject(gjson.Parse(`{"input":[{"role":"user","content":"`+text+`"},{"role":"user","content":"`+text+`"}]}`), "")
	if e == nil || e.Code != "invalid_value" || e.Param != "session.input" {
		t.Errorf("token cap -> %+v", e)
	}
}

func TestLiveSessionDelegationVariants(t *testing.T) {
	tests := []struct {
		session string
		filler  bool
		delay   int
		ack     string
	}{
		{`{}`, true, 0, "auto"},
		{`{"delegation":null}`, true, 0, "auto"},
		{`{"delegation":{"type":"client"}}`, true, 0, "auto"},
		{`{"delegation":{"type":"client","acknowledgement_delay_ms":1500}}`, true, 1500, "auto"},
		{`{"delegation":{"acknowledgement":"none"}}`, false, 0, "none"},
		{`{"delegation":{"type":"client","acknowledgement":"auto"}}`, true, 0, "auto"},
	}
	for _, tt := range tests {
		cfg := liveTestConfig(t, tt.session)
		if cfg.AckFiller != tt.filler || cfg.AckDelayMS != tt.delay {
			t.Errorf("%s -> filler %v delay %d", tt.session, cfg.AckFiller, cfg.AckDelayMS)
		}
		pub := cfg.PublicSession("rtc_1")
		raw, _ := json.Marshal(pub)
		if got := gjson.GetBytes(raw, "delegation.acknowledgement").String(); got != tt.ack {
			t.Errorf("%s public ack = %q", tt.session, got)
		}
	}
}

func TestLiveSessionUpstreamSession(t *testing.T) {
	cfg := liveTestConfig(t, `{"model":"gpt-live-1","instructions":"Be kind.","audio":{"output":{"voice":"quartz"}},"input":[{"role":"user","content":"hi"}]}`)
	got := gjson.ParseBytes(cfg.UpstreamSession())
	if got.Get("model").String() != "gpt-live-1-codex" || got.Get("instructions").String() != "Be kind." || got.Get("audio.output.voice").String() != "sol" || got.Get("delegation.type").String() != "client" || got.Get("delegation.ack_filler").Exists() || got.Get("initial_items.0.role").String() != "user" || got.Get("initial_items.0.content.0.type").String() != "input_text" {
		t.Fatalf("upstream = %s", cfg.UpstreamSession())
	}
	cfg = liveTestConfig(t, `{"delegation":{"acknowledgement":"none"}}`)
	got = gjson.ParseBytes(cfg.UpstreamSession())
	if !got.Get("delegation.ack_filler").Exists() || got.Get("delegation.ack_filler").Bool() || got.Get("initial_items").Exists() || got.Get("instructions").Exists() {
		t.Fatalf("upstream = %s", cfg.UpstreamSession())
	}
}

func TestLiveSessionPublicSession(t *testing.T) {
	cfg := liveTestConfig(t, `{"instructions":"Hi","audio":{"format":{"type":"audio/pcmu"},"output":{"voice":"stone"}},"store":true}`)
	raw, _ := json.Marshal(cfg.PublicSession("rtc_9"))
	got := gjson.ParseBytes(raw)
	if got.Get("id").String() != "rtc_9" || got.Get("model").String() != "gpt-live-1" || got.Get("instructions").String() != "Hi" || got.Get("audio.format.type").String() != "audio/pcmu" || got.Get("audio.format.rate").Int() != 8000 || got.Get("audio.output.voice").String() != "stone" || got.Get("delegation.type").String() != "client" || !got.Get("store").Bool() {
		t.Fatalf("public = %s", raw)
	}
}

// liveTestStarted returns a translator that has seen upstream session.started with a controllable clock.
func liveTestStarted(t *testing.T, session string) (*liveSessionTranslator, *time.Time) {
	t.Helper()
	tr := newLiveSessionTranslator(liveTestConfig(t, session))
	now := time.Unix(1000, 0)
	tr.now = func() time.Time { return now }
	res := tr.Upstream([]byte(`{"type":"session.started","session":{"expires_at":1793875126,"id":"rtc_u32_x","status":"active"}}`))
	if len(res.Client) != 1 || gjson.ParseBytes(res.Client[0]).Get("type").String() != "session.started" {
		t.Fatalf("started = %v", liveTestStr(res.Client))
	}
	return tr, &now
}

func TestLiveSessionQueueUntilStarted(t *testing.T) {
	tr := newLiveSessionTranslator(liveTestConfig(t, `{}`))
	r1 := tr.Client([]byte(`{"type":"session.input_audio.mute"}`))
	r2 := tr.Client([]byte(`{"type":"session.instructions.append","event_id":"i1","content":"The user is on a Mac."}`))
	r3 := tr.Client([]byte(`{"type":"session.commentary.append","delegation_id":null,"content":"Hello"}`))
	for _, r := range []liveClientResult{r1, r2, r3} {
		if len(r.Upstream) != 0 || len(r.Client) != 0 {
			t.Fatalf("before started: %+v", r)
		}
	}
	// A local error is returned at once, even before started.
	bad := tr.Client([]byte(`{"type":"response.create","event_id":"r1"}`))
	if len(bad.Client) != 1 || gjson.ParseBytes(bad.Client[0]).Get("error.code").String() != "unsupported_value" {
		t.Fatalf("bad = %+v", bad)
	}
	if tr.SessionID() != "" {
		t.Fatal("session id before started")
	}
	res := tr.Upstream([]byte(`{"type":"session.started","session":{"id":"rtc_u32_abc","status":"active"}}`))
	if tr.SessionID() != "rtc_u32_abc" {
		t.Fatalf("session id = %q", tr.SessionID())
	}
	if len(res.Client) != 1 || gjson.ParseBytes(res.Client[0]).Get("session.id").String() != "rtc_u32_abc" || gjson.ParseBytes(res.Client[0]).Get("session.audio.output.voice").String() != "marin" {
		t.Fatalf("started = %v", liveTestStr(res.Client))
	}
	types := []string{}
	for _, up := range res.Upstream {
		r := gjson.ParseBytes(up)
		types = append(types, r.Get("type").String()+"/"+r.Get("channel").String())
	}
	if strings.Join(types, ",") != "input_audio.pause/,session.context.append/commentary,session.context.append/speakable" {
		t.Fatalf("flushed = %v", types)
	}
	// A second started event is ignored and the queue is empty.
	again := tr.Upstream([]byte(`{"type":"session.started","session":{"id":"rtc_other"}}`))
	if len(again.Client) != 0 || len(again.Upstream) != 0 || tr.SessionID() != "rtc_u32_abc" {
		t.Fatalf("second started = %+v", again)
	}
	// After started, client events go out at once.
	now := tr.Client([]byte(`{"type":"session.input_audio.unmute"}`))
	if len(now.Upstream) != 1 || string(now.Upstream[0]) != `{"type":"input_audio.resume"}` {
		t.Fatalf("unmute = %+v", now)
	}
}

func TestLiveSessionClientMappings(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)

	audio := tr.Client([]byte(`{"type":"session.input_audio.append","audio":"` + base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}) + `"}`))
	if string(audio.Audio) != "\x01\x02\x03\x04" || len(audio.Upstream) != 0 || len(audio.Client) != 0 {
		t.Fatalf("audio = %+v", audio)
	}
	for _, raw := range []string{`{"type":"session.input_audio.append","event_id":"a1"}`, `{"type":"session.input_audio.append","event_id":"a1","audio":"%%%"}`} {
		bad := tr.Client([]byte(raw))
		e := gjson.ParseBytes(bad.Client[0])
		if len(bad.Client) != 1 || e.Get("error.param").String() != "audio" || e.Get("error.client_event_id").String() != "a1" {
			t.Fatalf("bad audio %s = %v", raw, liveTestStr(bad.Client))
		}
	}

	if got := string(tr.Client([]byte(`{"type":"session.input_audio.mute"}`)).Upstream[0]); got != `{"type":"input_audio.pause"}` {
		t.Fatalf("mute = %s", got)
	}

	com := tr.Client([]byte(`{"type":"session.commentary.append","event_id":"c1","delegation_id":"item_1","content":[{"type":"input_text","text":"Safari is open."}]}`))
	if len(com.Upstream) != 1 || string(com.Upstream[0]) != `{"channel":"speakable","content":[{"text":"Safari is open.","type":"input_text"}],"delegation_item_id":"item_1","type":"delegation.context.append"}` {
		t.Fatalf("commentary = %v", liveTestStr(com.Upstream))
	}
	think := tr.Client([]byte(`{"type":"session.thinking.append","delegation_id":"item_1","content":"Searching"}`))
	if got := gjson.ParseBytes(think.Upstream[0]); got.Get("type").String() != "delegation.context.append" || got.Get("channel").String() != "commentary" {
		t.Fatalf("thinking = %s", think.Upstream[0])
	}
	sess := tr.Client([]byte(`{"type":"session.thinking.append","content":"quiet"}`))
	if got := gjson.ParseBytes(sess.Upstream[0]); got.Get("type").String() != "session.context.append" || got.Get("channel").String() != "commentary" || got.Get("delegation_item_id").Exists() {
		t.Fatalf("session thinking = %s", sess.Upstream[0])
	}
	spk := tr.Client([]byte(`{"type":"session.commentary.append","delegation_id":null,"content":"hello"}`))
	if got := gjson.ParseBytes(spk.Upstream[0]); got.Get("type").String() != "session.context.append" || got.Get("channel").String() != "speakable" {
		t.Fatalf("session commentary = %s", spk.Upstream[0])
	}
	ins := tr.Client([]byte(`{"type":"session.instructions.append","content":"Speak Hindi."}`))
	if got := gjson.ParseBytes(ins.Upstream[0]); got.Get("type").String() != "session.context.append" || got.Get("channel").String() != "commentary" {
		t.Fatalf("instructions = %s", ins.Upstream[0])
	}

	if empty := tr.Client([]byte(`{"type":"session.thinking.append","delegation_id":"item_1","content":""}`)); len(empty.Upstream) != 1 || gjson.ParseBytes(empty.Upstream[0]).Get("content.0.text").String() != "" {
		t.Fatalf("empty content = %v", liveTestStr(empty.Upstream))
	}

	if !tr.Client([]byte(`{"type":"session.close"}`)).Close {
		t.Fatal("session.close must set Close")
	}
}

func TestLiveSessionClientErrors(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)
	tests := []struct{ raw, code, param, clientID string }{
		{`{"type":"session.start","event_id":"s","session":{}}`, "invalid_value", "type", "s"},
		{`{"type":"response.create","event_id":"r"}`, "unsupported_value", "type", "r"},
		{`{"type":"response.item.create","event_id":"r2"}`, "unsupported_value", "type", "r2"},
		{`{"type":"session.bogus","event_id":"b"}`, "invalid_value", "type", "b"},
		{`{"event_id":"n"}`, "missing_required_parameter", "type", "n"},
		{`nope`, "invalid_value", "", ""},
		{`{"type":"session.commentary.append","event_id":"c"}`, "missing_required_parameter", "content", "c"},
		{`{"type":"session.thinking.append","event_id":"c","content":5}`, "invalid_value", "content", "c"},
		{`{"type":"session.commentary.append","event_id":"c","delegation_id":7,"content":"x"}`, "invalid_value", "delegation_id", "c"},
		{`{"type":"session.update","event_id":"u1","session":{"delegation":{"responses":{"x":1}}}}`, "unsupported_value", "session.delegation.responses", "u1"},
		{`{"type":"session.update","event_id":"u2","session":{"instructions":"x"}}`, "immutable_field_update", "session.instructions", "u2"},
		{`{"type":"session.update","event_id":"u3","session":{"delegation":{"type":"client"}}}`, "immutable_field_update", "session.delegation", "u3"},
		{`{"type":"session.update","event_id":"u4","session":{"foo":1}}`, "unknown_parameter", "session.foo", "u4"},
		{`{"type":"session.update","event_id":"u5"}`, "missing_required_parameter", "session", "u5"},
	}
	for _, tt := range tests {
		res := tr.Client([]byte(tt.raw))
		if len(res.Client) != 1 || len(res.Upstream) != 0 {
			t.Errorf("%s -> %+v", tt.raw, res)
			continue
		}
		e := gjson.ParseBytes(res.Client[0]).Get("error")
		if e.Get("code").String() != tt.code || e.Get("param").String() != tt.param || e.Get("client_event_id").String() != tt.clientID {
			t.Errorf("%s -> %s, want %s %q %q", tt.raw, res.Client[0], tt.code, tt.param, tt.clientID)
		}
	}
	// A failed append must not leave an ack entry behind.
	if len(tr.acks) != 0 {
		t.Errorf("acks left behind: %v", tr.acks)
	}
}

func TestLiveSessionUpstreamMappings(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)

	pcm := base64.StdEncoding.EncodeToString([]byte{9, 8, 7, 6})
	if out := tr.Upstream([]byte(`{"type":"session.output_audio.delta","delta":"` + pcm + `"}`)); string(out.AudioOut) != "\x09\x08\x07\x06" || len(out.Client) != 0 {
		t.Fatalf("audio out = %+v", out)
	}
	if out := tr.Upstream([]byte(`{"type":"session.input_audio.append","delta":"AAAA"}`)); len(out.Client) != 0 || out.AudioOut != nil || len(out.Upstream) != 0 {
		t.Fatalf("echo not dropped: %+v", out)
	}

	in := tr.Upstream([]byte(`{"end_ms":21400,"item":{"id":"item_1","text":", can","type":"input_transcript"},"start_ms":21200,"type":"input_transcript.added"}`))
	got := gjson.ParseBytes(in.Client[0])
	if got.Get("type").String() != "session.input_transcript.delta" || got.Get("delta").String() != ", can" || got.Get("start_ms").Int() != 21200 || got.Get("end_ms").Int() != 21400 || got.Get("language").String() != "en" {
		t.Fatalf("input transcript = %s", in.Client[0])
	}
	out := tr.Upstream([]byte(`{"end_ms":24200,"item":{"id":"item_2","text":" अभी मुंबई","type":"output_transcript"},"start_ms":24000,"type":"output_transcript.added"}`))
	got = gjson.ParseBytes(out.Client[0])
	if got.Get("type").String() != "session.output_transcript.delta" || got.Get("delta").String() != " अभी मुंबई" || got.Get("language").String() != "hi" {
		t.Fatalf("output transcript = %s", out.Client[0])
	}
	mixed := tr.Upstream([]byte(`{"end_ms":2,"item":{"text":"42 ...","type":"input_transcript"},"start_ms":1,"type":"input_transcript.added"}`))
	if gjson.ParseBytes(mixed.Client[0]).Get("language").Exists() {
		t.Fatalf("language set for no letters: %s", mixed.Client[0])
	}
	// turn.delta duplicates the added events and the turn events carry no incremental text.
	for _, raw := range []string{
		`{"delta":", can","end_ms":21400,"start_ms":21200,"turn_id":"turn_1","type":"turn.delta"}`,
		`{"turn":{"end_ms":800,"id":"turn_1","role":"user","start_ms":600,"transcript":"Open"},"type":"turn.created"}`,
		`{"turn":{"end_ms":4600,"id":"turn_1","role":"assistant","start_ms":4400,"transcript":" On it"},"type":"turn.done"}`,
		`{"type":"something.else"}`,
		`not json`,
	} {
		if r := tr.Upstream([]byte(raw)); len(r.Client) != 0 || r.AudioOut != nil {
			t.Errorf("%s produced %v", raw, liveTestStr(r.Client))
		}
	}

	deleg := tr.Upstream([]byte(`{"item":{"content":[{"text":"Open Safari and search weather in Mumbai","type":"input_text"}],"handoff_id":"handoff_1","id":"item_EgnHfL9WtmFRuPvw8M0n5","target":"client","type":"delegation","user_bidi_turn_id":"turn_x"},"offset_ms":4000,"type":"delegation.created"}`))
	if string(deleg.Client[0]) != `{"delegation":{"content":"Open Safari and search weather in Mumbai","id":"item_EgnHfL9WtmFRuPvw8M0n5","target":"client","type":"delegation"},"offset_ms":4000,"type":"session.delegation.created"}` {
		t.Fatalf("delegation = %s", deleg.Client[0])
	}

	if r := tr.Upstream([]byte(`{"type":"input_audio.paused"}`)); string(r.Client[0]) != `{"type":"session.input_audio.muted"}` {
		t.Fatalf("paused = %v", liveTestStr(r.Client))
	}
	if r := tr.Upstream([]byte(`{"type":"input_audio.resumed"}`)); string(r.Client[0]) != `{"type":"session.input_audio.unmuted"}` {
		t.Fatalf("resumed = %v", liveTestStr(r.Client))
	}

	errOut := tr.Upstream([]byte(`{"error":{"code":"unknown_parameter","event_id":null,"message":"Unknown parameter: 'session.foo'.","param":"session.foo","type":"invalid_request_error"},"event_id":"event_1","type":"error"}`))
	e := gjson.ParseBytes(errOut.Client[0])
	if e.Get("type").String() != "error" || e.Get("error.code").String() != "unknown_parameter" || e.Get("error.param").String() != "session.foo" || e.Get("error.message").String() != "Unknown parameter: 'session.foo'." || e.Get("error.type").String() != "invalid_request_error" {
		t.Fatalf("error = %s", errOut.Client[0])
	}
}

func TestLiveSessionDelegationCreatedEvent(t *testing.T) {
	if _, ok := liveDelegationCreatedEvent([]byte(`{"type":"turn.done"}`)); ok {
		t.Error("wrong type accepted")
	}
	if _, ok := liveDelegationCreatedEvent([]byte(`{"type":"delegation.created","item":{"content":[]}}`)); ok {
		t.Error("missing id accepted")
	}
	out, ok := liveDelegationCreatedEvent([]byte(`{"type":"delegation.created","offset_ms":3600,"item":{"id":"item_h","content":[{"type":"input_text","text":"नमस्ते, "},{"type":"input_text","text":"मौसम"},{"type":"other","text":"skip"}]}}`))
	if !ok || gjson.GetBytes(out, "delegation.content").String() != "नमस्ते, मौसम" || gjson.GetBytes(out, "offset_ms").Int() != 3600 {
		t.Fatalf("out = %s", out)
	}
}

func TestLiveSessionAckFIFO(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)
	// Two delegations and session-level appends interleaved.
	tr.Client([]byte(`{"type":"session.commentary.append","event_id":"a1","delegation_id":"item_A","content":"one"}`))
	tr.Client([]byte(`{"type":"session.thinking.append","event_id":"b1","delegation_id":"item_B","content":"two"}`))
	tr.Client([]byte(`{"type":"session.thinking.append","event_id":"a2","delegation_id":"item_A","content":"three"}`))
	tr.Client([]byte(`{"type":"session.instructions.append","event_id":"s1","content":"ctx"}`))
	tr.Client([]byte(`{"type":"session.commentary.append","delegation_id":null,"content":"hi"}`))

	ack := func(raw string) string {
		r := tr.Upstream([]byte(raw))
		if len(r.Client) == 0 {
			return ""
		}
		return string(r.Client[0])
	}
	if got := ack(`{"delegation_item_id":"item_B","end_ms":1,"start_ms":0,"type":"delegation.context.appended"}`); got != `{"client_event_id":"b1","type":"session.thinking.appended"}` {
		t.Fatalf("B ack = %s", got)
	}
	if got := ack(`{"delegation_item_id":"item_A","type":"delegation.context.appended"}`); got != `{"client_event_id":"a1","type":"session.commentary.appended"}` {
		t.Fatalf("A ack 1 = %s", got)
	}
	if got := ack(`{"delegation_item_id":"item_A","type":"delegation.context.appended"}`); got != `{"client_event_id":"a2","type":"session.thinking.appended"}` {
		t.Fatalf("A ack 2 = %s", got)
	}
	if got := ack(`{"end_ms":400,"start_ms":200,"type":"session.context.appended"}`); got != `{"client_event_id":"s1","type":"session.instructions.appended"}` {
		t.Fatalf("session ack 1 = %s", got)
	}
	if got := ack(`{"type":"session.context.appended"}`); got != `{"client_event_id":null,"type":"session.commentary.appended"}` {
		t.Fatalf("session ack 2 = %s", got)
	}
	// An ack with nothing outstanding is dropped.
	if got := ack(`{"type":"session.context.appended"}`); got != "" {
		t.Fatalf("stray ack = %s", got)
	}
	if got := ack(`{"delegation_item_id":"item_Z","type":"delegation.context.appended"}`); got != "" {
		t.Fatalf("stray delegation ack = %s", got)
	}
}

func TestLiveSessionAckChunked(t *testing.T) {
	tr, _ := liveTestStarted(t, `{}`)
	long := strings.Repeat("é", 600) // 1200 bytes -> three chunks of at most 500 bytes
	res := tr.Client([]byte(`{"type":"session.commentary.append","event_id":"long","delegation_id":"item_A","content":"` + long + `"}`))
	if len(res.Upstream) != 3 {
		t.Fatalf("chunks = %d", len(res.Upstream))
	}
	joined := ""
	for _, up := range res.Upstream {
		text := gjson.GetBytes(up, "content.0.text").String()
		if len(text) > 500 || !strings.HasPrefix(string(up), "{") || strings.Contains(text, "�") {
			t.Fatalf("bad chunk (%d bytes)", len(text))
		}
		joined += text
	}
	if joined != long {
		t.Fatal("chunks do not rejoin to the original text")
	}
	tr.Client([]byte(`{"type":"session.thinking.append","event_id":"next","delegation_id":"item_A","content":"after"}`))
	ack := `{"delegation_item_id":"item_A","type":"delegation.context.appended"}`
	for i := 0; i < 2; i++ {
		if r := tr.Upstream([]byte(ack)); len(r.Client) != 0 {
			t.Fatalf("chunk %d acked early: %v", i, liveTestStr(r.Client))
		}
	}
	if r := tr.Upstream([]byte(ack)); len(r.Client) != 1 || string(r.Client[0]) != `{"client_event_id":"long","type":"session.commentary.appended"}` {
		t.Fatalf("last chunk ack = %v", liveTestStr(r.Client))
	}
	if r := tr.Upstream([]byte(ack)); len(r.Client) != 1 || gjson.ParseBytes(r.Client[0]).Get("client_event_id").String() != "next" {
		t.Fatalf("next ack = %v", liveTestStr(r.Client))
	}
}

func TestLiveSessionContextAppendEvents(t *testing.T) {
	events, e := liveContextAppendEvents("", "speakable", gjson.Parse(`[{"type":"input_text","text":"a"},{"text":"b"}]`))
	if e != nil || len(events) != 1 || string(events[0]) != `{"channel":"speakable","content":[{"text":"ab","type":"input_text"}],"type":"session.context.append"}` {
		t.Fatalf("parts = %v %+v", liveTestStr(events), e)
	}
	events, e = liveContextAppendEvents("item_1", "commentary", gjson.Parse(`""`))
	if e != nil || len(events) != 1 || gjson.GetBytes(events[0], "delegation_item_id").String() != "item_1" {
		t.Fatalf("empty = %v %+v", liveTestStr(events), e)
	}
	events, _ = liveContextAppendEvents("", "commentary", gjson.Parse(`"`+strings.Repeat("a", 500)+`"`))
	if len(events) != 1 {
		t.Fatalf("500 bytes chunks = %d", len(events))
	}
	events, _ = liveContextAppendEvents("", "commentary", gjson.Parse(`"`+strings.Repeat("a", 501)+`"`))
	if len(events) != 2 {
		t.Fatalf("501 bytes chunks = %d", len(events))
	}
	if _, e = liveContextAppendEvents("", "commentary", gjson.Result{}); e == nil || e.Code != "missing_required_parameter" || e.Param != "content" {
		t.Fatalf("missing content = %+v", e)
	}
	if _, e = liveContextAppendEvents("", "commentary", gjson.Parse(`[{"type":"input_image"}]`)); e == nil || e.Code != "invalid_value" {
		t.Fatalf("bad part = %+v", e)
	}
}

func TestLiveSessionUsageSeconds(t *testing.T) {
	tr, now := liveTestStarted(t, `{}`)
	usage := func(ms int) int64 {
		raw := `{"type":"session.usage.updated","usage":{"audio_duration_ms":` + liveItoa(ms) + `,"backend_model_usage":[]},"usage_limit":{"reset_seconds":null,"status":null}}`
		r := tr.Upstream([]byte(raw))
		if len(r.Client) != 1 || gjson.ParseBytes(r.Client[0]).Get("type").String() != "session.usage.updated" {
			t.Fatalf("usage result = %v", liveTestStr(r.Client))
		}
		return gjson.ParseBytes(r.Client[0]).Get("usage.seconds").Int()
	}
	*now = now.Add(15 * time.Second)
	if got := usage(13800); got != 15 { // gateway clock wins over ceil(13.8) = 14
		t.Fatalf("seconds = %d, want 15", got)
	}
	*now = now.Add(15 * time.Second)
	if got := usage(44800); got != 45 { // upstream wins: ceil(44.8) = 45 over 30
		t.Fatalf("seconds = %d, want 45", got)
	}
	*now = now.Add(15 * time.Second)
	if got := usage(10); got != 45 { // never decreasing
		t.Fatalf("seconds = %d, want 45", got)
	}
}

func TestLiveSessionTickAfterSilence(t *testing.T) {
	tr, now := liveTestStarted(t, `{}`)
	start := *now
	if out := tr.Tick(start.Add(59 * time.Second)); len(out) != 0 {
		t.Fatalf("tick at 59 s = %v", liveTestStr(out))
	}
	out := tr.Tick(start.Add(60 * time.Second))
	if len(out) != 1 || string(out[0]) != `{"type":"session.usage.updated","usage":{"seconds":60}}` {
		t.Fatalf("tick at 60 s = %v", liveTestStr(out))
	}
	if out := tr.Tick(start.Add(61 * time.Second)); len(out) != 0 {
		t.Fatalf("tick right after = %v", liveTestStr(out))
	}
	// An upstream usage event resets the silence window.
	*now = start.Add(90 * time.Second)
	tr.Upstream([]byte(`{"type":"session.usage.updated","usage":{"audio_duration_ms":89000}}`))
	if out := tr.Tick(start.Add(140 * time.Second)); len(out) != 0 {
		t.Fatalf("tick after upstream usage = %v", liveTestStr(out))
	}
	if out := tr.Tick(start.Add(151 * time.Second)); len(out) != 1 || gjson.GetBytes(out[0], "usage.seconds").Int() != 151 {
		t.Fatalf("tick after silence = %v", liveTestStr(out))
	}
	// Not started: no ticks.
	idle := newLiveSessionTranslator(liveTestConfig(t, `{}`))
	if out := idle.Tick(time.Now().Add(time.Hour)); len(out) != 0 {
		t.Fatalf("tick before started = %v", liveTestStr(out))
	}
}

func TestLiveSessionClosedShape(t *testing.T) {
	tr, now := liveTestStarted(t, `{}`)
	*now = now.Add(7*time.Second + 400*time.Millisecond)
	for _, reason := range []string{"close_requested", "expired", "remote_hangup", "connection_lost"} {
		got := gjson.ParseBytes(tr.Closed(reason))
		if got.Get("type").String() != "session.closed" || got.Get("reason").String() != reason || got.Get("usage.seconds").Int() != 7 {
			t.Errorf("closed(%s) = %s", reason, tr.Closed(reason))
		}
	}
	idle := newLiveSessionTranslator(liveTestConfig(t, `{}`))
	if got := string(idle.Closed("connection_lost")); got != `{"reason":"connection_lost","type":"session.closed","usage":{"seconds":0}}` {
		t.Errorf("closed before start = %s", got)
	}
}

func TestLiveSessionTranscriptLanguage(t *testing.T) {
	tests := []struct{ text, want string }{
		{"नमस्ते, आज मुंबई में मौसम कैसा है", "hi"},
		{"હેલો, આજે હવામાન કેવું છે", "gu"},
		{"Open Safari and search weather in Mumbai", "en"},
		{"हेलो, can you", ""},
		{"", ""},
		{"  123 ... 45?", ""},
		{"१२ नमस्ते 3", "hi"},
		{"Hello 你好", ""},
		{" It's 31 degrees.", "en"},
	}
	for _, tt := range tests {
		if got := liveTranscriptLanguage(tt.text); got != tt.want {
			t.Errorf("liveTranscriptLanguage(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}
