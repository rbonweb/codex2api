package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// fakeSpeechCall records the events sent to it and answers an append with canned sideband events.
type fakeSpeechCall struct {
	*fakeTranscriptionCall
	mu      sync.Mutex
	sent    []string
	onSend  func(f *fakeSpeechCall)
	sendErr error
}

func (f *fakeSpeechCall) Send(event []byte) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.mu.Lock()
	f.sent = append(f.sent, string(event))
	f.mu.Unlock()
	if f.onSend != nil {
		f.onSend(f)
	}
	return nil
}

type speechTestEnv struct {
	call    *fakeSpeechCall
	session json.RawMessage
	openErr *liveGatewayError
	opened  int
	handler *Handler
}

func newSpeechTestEnv(t *testing.T) *speechTestEnv {
	t.Helper()
	env := &speechTestEnv{call: &fakeSpeechCall{fakeTranscriptionCall: newFakeTranscriptionCall()}, handler: &Handler{}}
	prevOpen, prevSupported, prevEnc := openSpeechCall, speechBridgeSupported, newRealtimeAudioEncoder
	prevStart, prevBase, prevPer, prevWait := speechStartDeadline, speechDeadlineBase, speechDeadlinePerRune, speechSessionWait
	openSpeechCall = func(h *Handler, c *gin.Context, session json.RawMessage) (speechCall, *liveGatewayError) {
		env.opened++
		env.session = session
		if env.openErr != nil {
			return nil, env.openErr
		}
		if env.opened > 1 {
			// A retry gets a fresh call; the first one is closed and cannot take events.
			env.call = &fakeSpeechCall{fakeTranscriptionCall: newFakeTranscriptionCall(), onSend: env.call.onSend}
		}
		return env.call, nil
	}
	speechBridgeSupported = func() bool { return true }
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return &fakeAudioEncoder{}, nil }
	speechStartDeadline, speechDeadlineBase, speechDeadlinePerRune, speechSessionWait = 300*time.Millisecond, 2*time.Second, 0, 0
	t.Cleanup(func() {
		openSpeechCall, speechBridgeSupported, newRealtimeAudioEncoder = prevOpen, prevSupported, prevEnc
		speechStartDeadline, speechDeadlineBase, speechDeadlinePerRune, speechSessionWait = prevStart, prevBase, prevPer, prevWait
	})
	return env
}

func runSpeech(t *testing.T, env *speechTestEnv, body string, allowLive bool) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := liveTestContext(t, http.MethodPost, "/v1/audio/speech", []byte(body), &database.APIKeyRow{
		ID: 7, Limits: database.APIKeyLimits{AllowLive: allowLive},
	})
	env.handler.AudioSpeech(c)
	return rec
}

// speechTone returns ms milliseconds of a loud square wave at 24 kHz as PCM16 little-endian.
func speechTone(ms int) []byte {
	out := make([]byte, ms*24*2)
	for i := 0; i+1 < len(out); i += 2 {
		v := int16(8000)
		if (i/2/12)%2 == 0 {
			v = -8000
		}
		binary.LittleEndian.PutUint16(out[i:], uint16(v))
	}
	return out
}

func speechQuiet(ms int) []byte { return make([]byte, ms*24*2) }

func audioDelta(pcm []byte) map[string]any {
	return map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(pcm)}
}

// speakOnSend makes the fake call answer the append with quiet audio, a spoken tone, quiet audio and the assistant turn end.
func speakOnSend(tone []byte) func(*fakeSpeechCall) {
	return func(f *fakeSpeechCall) {
		f.send(map[string]any{"type": "session.context.appended"})
		f.send(audioDelta(speechQuiet(400)))
		f.send(map[string]any{"type": "turn.created", "turn": map[string]any{"role": "assistant"}})
		f.send(audioDelta(tone))
		f.send(audioDelta(speechQuiet(600)))
		f.send(map[string]any{"type": "turn.done", "turn": map[string]any{"role": "assistant", "transcript": "x"}})
	}
}

func TestAudioSpeechWAVTrimsSilenceAndAppendsSpeakable(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = speakOnSend(speechTone(1000))
	rec := runSpeech(t, env, `{"model":"tts-1-hd","input":"The quick brown fox.","voice":"alloy","response_format":"wav"}`, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("status=%d type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	body := rec.Body.Bytes()
	if string(body[:4]) != "RIFF" || string(body[8:16]) != "WAVEfmt " || string(body[36:40]) != "data" {
		t.Fatalf("not a WAV header: %q", body[:44])
	}
	if rate := binary.LittleEndian.Uint32(body[24:28]); rate != 24000 {
		t.Fatalf("sample rate = %d", rate)
	}
	dataLen := int(binary.LittleEndian.Uint32(body[40:44]))
	if dataLen != len(body)-44 {
		t.Fatalf("data length %d, body %d", dataLen, len(body))
	}
	// 1000 ms of tone plus 100 ms padding on each side; the 400 ms and 600 ms of silence are cut.
	wantMs := 1000 + 200
	if gotMs := dataLen / 2 / 24; gotMs < wantMs-40 || gotMs > wantMs+40 {
		t.Fatalf("trimmed audio = %d ms, want about %d", gotMs, wantMs)
	}
	if rec.Header().Get("X-Codex2api-Live-Setup-Ms") != "123" {
		t.Fatalf("setup header = %q", rec.Header().Get("X-Codex2api-Live-Setup-Ms"))
	}
	session := string(env.session)
	for _, want := range []string{"text-to-speech engine", `"voice":"cove"`, `"initial_items"`, `"role":"developer"`} {
		if !strings.Contains(session, want) {
			t.Fatalf("session %s lacks %q", session, want)
		}
	}
	for _, want := range []string{`"ack_filler":false`, `"type":"client"`, "never acted on"} {
		if !strings.Contains(session, want) {
			t.Fatalf("session %s lacks %q", session, want)
		}
	}
	if len(env.call.sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(env.call.sent))
	}
	for _, want := range []string{`"type":"session.context.append"`, `"channel":"speakable"`, `"type":"input_text"`, `"text":"\"The quick brown fox.\""`} {
		if !strings.Contains(env.call.sent[0], want) {
			t.Fatalf("append %s lacks %q", env.call.sent[0], want)
		}
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times, want 1", env.call.closes.Load())
	}
}

func TestAudioSpeechPCMFormat(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = speakOnSend(speechTone(500))
	rec := runSpeech(t, env, `{"input":"Hello there.","voice":"quartz","response_format":"pcm"}`, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/pcm" {
		t.Fatalf("status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.HasPrefix(rec.Body.String(), "RIFF") || rec.Body.Len()%2 != 0 || rec.Body.Len() < 500*24*2 {
		t.Fatalf("unexpected raw PCM body of %d bytes", rec.Body.Len())
	}
	if !strings.Contains(string(env.session), `"voice":"sol"`) {
		t.Fatalf("quartz must map to sol: %s", env.session)
	}
}

func TestAudioSpeechQuestionIsQuotedAndExplained(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = speakOnSend(speechTone(300))
	rec := runSpeech(t, env, `{"input":"What is the capital of France?","instructions":"Speak slowly."}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(env.call.sent[0], `"text":"\"What is the capital of France?\""`) {
		t.Fatalf("question not quoted: %s", env.call.sent[0])
	}
	for _, want := range []string{"never answered", "Speaking style: Speak slowly."} {
		if !strings.Contains(string(env.session), want) {
			t.Fatalf("session lacks %q: %s", want, env.session)
		}
	}
}

func TestAudioSpeechSpeakableText(t *testing.T) {
	if got := speechSpeakableText("It is 3:45 PM."); got != `"It is 3:45 PM."` {
		t.Fatalf("statement not quoted: %q", got)
	}
	if got := speechSpeakableText(`Is "this" it?`); got != `"Is 'this' it?"` {
		t.Fatalf("inner quotes kept: %q", got)
	}
}

func TestSpeechSentences(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"one", "Call me back tomorrow.", []string{"Call me back tomorrow."}},
		{"short trailing joins previous", "Call me back tomorrow, please. Open it.", []string{"Call me back tomorrow, please. Open it."}},
		{"two", "Call me back tomorrow at noon. Open the report for the meeting!", []string{"Call me back tomorrow at noon.", "Open the report for the meeting!"}},
		{"decimal and time stay", "It costs 3.5 dollars at 3:45 today. Thanks a lot for waiting.", []string{"It costs 3.5 dollars at 3:45 today.", "Thanks a lot for waiting."}},
		{"question and quote", `He said "stop it right now." Are you sure about that?`, []string{`He said "stop it right now."`, "Are you sure about that?"}},
		{"newline", "First line of the text here\nSecond line of the text here", []string{"First line of the text here", "Second line of the text here"}},
		{"devanagari", "आज मौसम बहुत अच्छा है और हवा ठंडी है। कल हम बाहर घूमने जाएंगे और खाना खाएंगे।", []string{"आज मौसम बहुत अच्छा है और हवा ठंडी है।", "कल हम बाहर घूमने जाएंगे और खाना खाएंगे।"}},
		{"blank", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := speechSentences(tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("speechSentences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	long := strings.Repeat("word ", 150)
	for _, chunk := range speechSentences(long) {
		if n := len([]rune(chunk)); n > speechMaxChunkRunes+speechMinChunkRunes {
			t.Fatalf("chunk of %d runes", n)
		}
	}
}

func TestAudioSpeechReadsEverySentenceInOrder(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = speakOnSend(speechTone(300))
	rec := runSpeech(t, env, `{"input":"Call me back tomorrow at noon. Open the report for the meeting."}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(env.call.sent) != 2 || !strings.Contains(env.call.sent[0], `\"Call me back tomorrow at noon.\"`) || !strings.Contains(env.call.sent[1], `\"Open the report for the meeting.\"`) {
		t.Fatalf("appends = %v", env.call.sent)
	}
	// Two 300 ms sentences, each trimmed and kept with 100 ms padding on both sides.
	if gotMs := (rec.Body.Len() - 44) / 2 / 24; gotMs < 1000-60 || gotMs > 1000+60 {
		t.Fatalf("audio = %d ms, want about 1000", gotMs)
	}
}

func TestAudioSpeechRetriesNoAudioOnceOnAFreshCall(t *testing.T) {
	env := newSpeechTestEnv(t)
	var calls []*fakeSpeechCall
	openSpeechCall = func(h *Handler, c *gin.Context, session json.RawMessage) (speechCall, *liveGatewayError) {
		call := &fakeSpeechCall{fakeTranscriptionCall: newFakeTranscriptionCall()}
		if len(calls) == 0 {
			call.onSend = func(f *fakeSpeechCall) { f.send(map[string]any{"type": "session.context.appended"}) }
		} else {
			call.onSend = speakOnSend(speechTone(300))
		}
		calls = append(calls, call)
		return call, nil
	}
	rec := runSpeech(t, env, `{"input":"Call me back tomorrow."}`, true)
	if rec.Code != http.StatusOK || len(calls) != 2 || calls[0].closes.Load() != 1 || calls[1].closes.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(calls), rec.Body.String())
	}
}

func TestAudioSpeechNoAudioIs504(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = func(f *fakeSpeechCall) {
		f.send(map[string]any{"type": "session.context.appended"})
		f.send(audioDelta(speechQuiet(200)))
	}
	rec := runSpeech(t, env, `{"input":"Is anyone there?"}`, true)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), "upstream_timeout") || !strings.Contains(rec.Body.String(), "the voice model did not read the text") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.opened != 2 || env.call.closes.Load() != 1 {
		t.Fatalf("opened %d calls, last closed %d times", env.opened, env.call.closes.Load())
	}
}

func TestAudioSpeechTurnEndsWithoutSoundIs504(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = func(f *fakeSpeechCall) {
		f.send(map[string]any{"type": "turn.created", "turn": map[string]any{"role": "assistant"}})
		f.send(audioDelta(speechQuiet(200)))
		f.send(map[string]any{"type": "turn.done", "turn": map[string]any{"role": "assistant"}})
	}
	rec := runSpeech(t, env, `{"input":"Hello."}`, true)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeechUpstreamErrorIs502(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = func(f *fakeSpeechCall) {
		f.send(map[string]any{"type": "error", "error": map[string]any{"message": "model overloaded"}})
	}
	rec := runSpeech(t, env, `{"input":"Hello."}`, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "model overloaded") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeechRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name, body, code, want string
		status                 int
	}{
		{"mp3", `{"input":"Hi","response_format":"mp3"}`, "unsupported_value", "use one of wav, pcm", 400},
		{"speed", `{"input":"Hi","speed":1.5}`, "unsupported_value", "speed", 400},
		{"model", `{"model":"gpt-4o","input":"Hi"}`, "unsupported_value", "gpt-4o-mini-tts, tts-1, tts-1-hd", 400},
		{"voice", `{"input":"Hi","voice":"nobody"}`, "unsupported_value", "/v1/audio/voices", 400},
		{"empty", `{"input":"   "}`, "invalid_request", "input is required", 400},
		{"too long", `{"input":"` + strings.Repeat("a", 1001) + `"}`, "invalid_request", "limit is 1000", 400},
		{"not json", `nope`, "invalid_request", "JSON object", 400},
		{"sse", `{"input":"Hi","stream_format":"sse"}`, "unsupported_value", "stream_format", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSpeechTestEnv(t)
			rec := runSpeech(t, env, tc.body, true)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if env.opened != 0 {
				t.Fatal("a call was opened for an invalid request")
			}
		})
	}
}

func TestAudioSpeechAcceptsSpeedOneAndThousandCharacters(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = speakOnSend(speechTone(200))
	body, _ := json.Marshal(map[string]any{"input": strings.Repeat("a", 1000), "speed": 1})
	if rec := runSpeech(t, env, string(body), true); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeechNeedsAllowLive(t *testing.T) {
	env := newSpeechTestEnv(t)
	if rec := runSpeech(t, env, `{"input":"Hi"}`, false); rec.Code != http.StatusForbidden || env.opened != 0 {
		t.Fatalf("status=%d opened=%d", rec.Code, env.opened)
	}
}

func TestAudioSpeechWithoutBridgeIs501(t *testing.T) {
	env := newSpeechTestEnv(t)
	speechBridgeSupported = func() bool { return false }
	rec := runSpeech(t, env, `{"input":"Hi"}`, true)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "audio_bridge_unavailable") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeechOpenErrorPassesThrough(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.openErr = newLiveGatewayError(http.StatusServiceUnavailable, "service_unavailable", "server_error", "no account")
	rec := runSpeech(t, env, `{"input":"Hi"}`, true)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

func TestAudioSpeechFullConcurrencyOnRetryIs503NotTimeout(t *testing.T) {
	env := newSpeechTestEnv(t)
	env.call.onSend = func(f *fakeSpeechCall) { f.send(map[string]any{"type": "session.context.appended"}) }
	prev := openSpeechCall
	openSpeechCall = func(h *Handler, c *gin.Context, session json.RawMessage) (speechCall, *liveGatewayError) {
		if env.opened >= 1 {
			env.opened++
			return nil, newLiveGatewayError(http.StatusServiceUnavailable, "service_unavailable", "server_error", "Service temporarily unavailable")
		}
		return prev(h, c, session)
	}
	rec := runSpeech(t, env, `{"input":"Hello there friend."}`, true)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || env.opened != 2 {
		t.Fatalf("status=%d retry-after=%q opened=%d", rec.Code, rec.Header().Get("Retry-After"), env.opened)
	}
}

func TestAudioSpeechDeadlineScalesWithText(t *testing.T) {
	prevBase, prevPer := speechDeadlineBase, speechDeadlinePerRune
	defer func() { speechDeadlineBase, speechDeadlinePerRune = prevBase, prevPer }()
	speechDeadlineBase, speechDeadlinePerRune = 10*time.Second, 100*time.Millisecond
	if got := speechDeadline(strings.Repeat("a", 100)); got != 20*time.Second {
		t.Fatalf("deadline = %s", got)
	}
}
