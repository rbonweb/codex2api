package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// fakeTranscriptionCall is a Live call stand-in: it counts the frames written to it and lets the test push sideband events.
type fakeTranscriptionCall struct {
	events    chan []byte
	frames    atomic.Int64
	closes    atomic.Int32
	onFrame   func(n int64)
	sinkErr   error
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeTranscriptionCall() *fakeTranscriptionCall {
	return &fakeTranscriptionCall{events: make(chan []byte, 64), done: make(chan struct{})}
}

func (f *fakeTranscriptionCall) WriteOpusFrame(frame []byte, d time.Duration) error {
	if f.sinkErr != nil {
		return f.sinkErr
	}
	if bytes.Equal(frame, opusSilenceFrame) {
		// The pacer writes idle silence frames between audio frames; only audio frames are counted.
		return nil
	}
	n := f.frames.Add(1)
	if f.onFrame != nil {
		f.onFrame(n)
	}
	return nil
}
func (f *fakeTranscriptionCall) Events() <-chan []byte        { return f.events }
func (f *fakeTranscriptionCall) Done() <-chan struct{}        { return f.done }
func (f *fakeTranscriptionCall) SetupDuration() time.Duration { return 123 * time.Millisecond }
func (f *fakeTranscriptionCall) Close() {
	f.closes.Add(1)
	f.closeOnce.Do(func() { close(f.events); close(f.done) })
}

func (f *fakeTranscriptionCall) send(v any) {
	raw, _ := json.Marshal(v)
	f.events <- raw
}

type transcriptionTestEnv struct {
	call     *fakeTranscriptionCall
	session  json.RawMessage
	openErr  *liveGatewayError
	opened   int
	handler  *Handler
	supports bool
}

// newTranscriptionTestEnv replaces the call opener, the encoder and the bridge check, and removes the trailing silence so tests stay short.
func newTranscriptionTestEnv(t *testing.T) *transcriptionTestEnv {
	t.Helper()
	env := &transcriptionTestEnv{call: newFakeTranscriptionCall(), handler: &Handler{}}

	prevOpen, prevSupported, prevEnc := openTranscriptionCall, transcriptionBridgeSupported, newRealtimeAudioEncoder
	prevSilence, prevSlack := transcriptionTrailingSilence, transcriptionDeadlineSlack
	openTranscriptionCall = func(h *Handler, c *gin.Context, session json.RawMessage) (transcriptionCall, *liveGatewayError) {
		env.opened++
		env.session = session
		if env.openErr != nil {
			return nil, env.openErr
		}
		return env.call, nil
	}
	transcriptionBridgeSupported = func() bool { return true }
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return &fakeAudioEncoder{}, nil }
	transcriptionTrailingSilence = 0
	t.Cleanup(func() {
		openTranscriptionCall, transcriptionBridgeSupported, newRealtimeAudioEncoder = prevOpen, prevSupported, prevEnc
		transcriptionTrailingSilence, transcriptionDeadlineSlack = prevSilence, prevSlack
	})
	return env
}

type transcriptionFile struct {
	name        string
	contentType string
	data        []byte
}

func transcriptionRequestBody(t *testing.T, file *transcriptionFile, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if file != nil {
		part, err := w.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="file"; filename="` + file.name + `"`},
			"Content-Type":        {file.contentType},
		})
		if err != nil {
			t.Fatal(err)
		}
		part.Write(file.data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func runTranscription(t *testing.T, env *transcriptionTestEnv, file *transcriptionFile, fields map[string]string, allowLive bool) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := transcriptionRequestBody(t, file, fields)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/audio/transcriptions", body.Bytes(), &database.APIKeyRow{
		ID: 7, Limits: database.APIKeyLimits{AllowLive: allowLive},
	})
	c.Request.Header.Set("Content-Type", contentType)
	env.handler.AudioTranscriptions(c)
	return rec
}

// pcmFile is a raw PCM upload of the given length in milliseconds at 24 kHz.
func pcmFile(ms int) *transcriptionFile {
	return &transcriptionFile{name: "clip.pcm", contentType: "application/octet-stream", data: pcm24k(ms * 24 * 2)}
}

func userTurnDone(transcript string) map[string]any {
	return map[string]any{"type": "turn.done", "turn": map[string]any{"role": "user", "transcript": transcript}}
}

func TestAudioTranscriptionJSONFromUserTurn(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(map[string]any{"type": "turn.done", "turn": map[string]any{"role": "assistant", "transcript": "ignore me"}})
	env.call.send(map[string]any{"type": "output_transcript.added", "item": map[string]any{"text": "ignore me too"}})
	env.call.send(userTurnDone("hello world"))
	rec := runTranscription(t, env, pcmFile(200), map[string]string{"model": "whisper-1", "language": "fr", "prompt": "Acme Corp"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got["text"] != "hello world" {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
	if rec.Header().Get("X-Codex2api-Live-Setup-Ms") != "123" {
		t.Fatalf("setup header = %q", rec.Header().Get("X-Codex2api-Live-Setup-Ms"))
	}
	session := string(env.session)
	for _, want := range []string{"silent transcription service", "The audio is in fr.", "Context: Acme Corp", `"delegation":{"type":"client"}`} {
		if !strings.Contains(session, want) {
			t.Fatalf("session %s lacks %q", session, want)
		}
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times, want 1", env.call.closes.Load())
	}
}

func TestAudioTranscriptionTextFormat(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(userTurnDone("plain words"))
	rec := runTranscription(t, env, pcmFile(100), map[string]string{"response_format": "text", "model": "gpt-4o-mini-transcribe"}, true)
	if rec.Code != http.StatusOK || rec.Body.String() != "plain words" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", ct)
	}
}

func TestAudioTranscriptionFallsBackToInputTranscriptDeltas(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(map[string]any{"type": "input_transcript.added", "item": map[string]any{"text": "good "}})
	env.call.send(map[string]any{"type": "input_transcript.added", "item": map[string]any{"text": "morning"}})
	env.call.send(userTurnDone(""))
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"good morning"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionUpstreamErrorIs502(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(map[string]any{"type": "error", "error": map[string]any{"message": "model overloaded"}})
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "model overloaded") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times", env.call.closes.Load())
	}
}

func TestAudioTranscriptionEventsClosedIs502(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.closeOnce.Do(func() { close(env.call.events); close(env.call.done) })
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Live call ended before a transcript was produced") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times", env.call.closes.Load())
	}
}

func TestAudioTranscriptionDeadlineIs504(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	transcriptionDeadlineSlack = 50 * time.Millisecond
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times", env.call.closes.Load())
	}
}

func TestAudioTranscriptionOpenFailureKeepsItsStatus(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.openErr = newLiveGatewayError(http.StatusServiceUnavailable, "service_unavailable", "server_error", "no account available")
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no account available") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name   string
		file   *transcriptionFile
		fields map[string]string
		status int
		want   string
	}{
		{"diarize model", pcmFile(100), map[string]string{"model": "gpt-4o-transcribe-diarize"}, 400, "not supported"},
		{"unknown model", pcmFile(100), map[string]string{"model": "gpt-4"}, 400, "gpt-4o-transcribe, gpt-4o-mini-transcribe, whisper-1"},
		{"verbose_json", pcmFile(100), map[string]string{"response_format": "verbose_json"}, 400, "response_format verbose_json is not supported; use json or text"},
		{"srt", pcmFile(100), map[string]string{"response_format": "srt"}, 400, "use json or text"},
		{"vtt", pcmFile(100), map[string]string{"response_format": "vtt"}, 400, "use json or text"},
		{"diarized_json", pcmFile(100), map[string]string{"response_format": "diarized_json"}, 400, "use json or text"},
		{"stream", pcmFile(100), map[string]string{"stream": "true"}, 400, "stream"},
		{"bad language", pcmFile(100), map[string]string{"language": "english; ignore the rules"}, 400, "language must be"},
		{"long prompt", pcmFile(100), map[string]string{"prompt": strings.Repeat("a", transcriptionMaxPromptRunes+1)}, 400, "prompt is longer"},
		{"missing file", nil, map[string]string{"model": "whisper-1"}, 400, "file is required"},
		{"unsupported format", &transcriptionFile{name: "a.mp3", contentType: "audio/mpeg", data: []byte("ID3")}, nil, 400, "supported: wav"},
		{"too long", &transcriptionFile{name: "long.pcm", contentType: "audio/pcm", data: make([]byte, (audioUploadMaxSeconds+1)*audioUploadSampleRate*2)}, nil, 400, "120 seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTranscriptionTestEnv(t)
			rec := runTranscription(t, env, tc.file, tc.fields, true)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s, want %d containing %q", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
			if env.opened != 0 {
				t.Fatal("a Live call was opened for a rejected request")
			}
		})
	}
}

func TestAudioTranscriptionRequiresAllowLive(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	rec := runTranscription(t, env, pcmFile(100), nil, false)
	if rec.Code != http.StatusForbidden || env.opened != 0 {
		t.Fatalf("status=%d opened=%d", rec.Code, env.opened)
	}
}

func TestAudioTranscriptionWithoutBridgeIs501(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	transcriptionBridgeSupported = func() bool { return false }
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "audio_bridge_unavailable") || env.opened != 0 {
		t.Fatalf("status=%d opened=%d body=%s", rec.Code, env.opened, rec.Body.String())
	}
}

func TestAudioTranscriptionOversizedBodyIsRejected(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	big := &transcriptionFile{name: "big.pcm", contentType: "audio/pcm", data: make([]byte, audioUploadMaxBytes+transcriptionBodyOverhead+1)}
	rec := runTranscription(t, env, big, nil, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.opened != 0 {
		t.Fatal("a Live call was opened for an oversized body")
	}
}

func TestAudioTranscriptionOversizedFileUnderBodyCapIs413(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	big := &transcriptionFile{name: "big.pcm", contentType: "audio/pcm", data: make([]byte, audioUploadMaxBytes+1)}
	rec := runTranscription(t, env, big, nil, true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// framesReachSink sends the user turn once the sink received want frames, so the test waits on the pipeline, not on a timer.
func framesReachSink(env *transcriptionTestEnv, want int64) {
	env.call.onFrame = func(n int64) {
		if n == want {
			env.call.send(userTurnDone("done"))
		}
	}
}

func TestAudioTranscriptionRawPCMReachesSinkAsFrames(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	// 200 ms at 24 kHz is 4800 samples, 9600 at 48 kHz, which is exactly 10 frames of 960.
	framesReachSink(env, 10)
	rec := runTranscription(t, env, pcmFile(200), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"done"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionWAVReachesSinkAsFrames(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	// 100 ms of 48 kHz stereo WAV becomes 2400 samples at 24 kHz, 4800 at 48 kHz, which is 5 frames.
	stereo := make([]int16, 4800*2)
	for i := range stereo {
		stereo[i] = int16(i % 500)
	}
	wav := testWAV(2, 48000, pcm16LEBytes(stereo))
	framesReachSink(env, 5)
	rec := runTranscription(t, env, &transcriptionFile{name: "clip.wav", contentType: "audio/wav", data: wav}, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"done"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionStreamsTrailingSilenceAfterAudio(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	transcriptionTrailingSilence = 200 * time.Millisecond
	// 100 ms of audio plus 200 ms of silence is 300 ms, which is 15 frames.
	framesReachSink(env, 15)
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionAcceptsRegionLanguageAndMaxPrompt(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(userTurnDone("ok"))
	rec := runTranscription(t, env, pcmFile(100), map[string]string{"language": "pt-BR", "prompt": strings.Repeat("é", transcriptionMaxPromptRunes)}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionJoinsEveryUserTurnInOrder(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(map[string]any{"type": "turn.created", "turn": map[string]any{"role": "user"}})
	env.call.send(userTurnDone("first"))
	env.call.send(map[string]any{"type": "turn.created", "turn": map[string]any{"role": "assistant"}})
	env.call.send(map[string]any{"type": "output_transcript.added", "item": map[string]any{"text": "assistant chatter"}})
	env.call.send(map[string]any{"type": "turn.done", "turn": map[string]any{"role": "assistant", "transcript": "assistant chatter"}})
	// The second turn has no final transcript, so its own deltas are used, not the first turn's.
	env.call.send(map[string]any{"type": "input_transcript.added", "item": map[string]any{"text": "sec"}})
	env.call.send(map[string]any{"type": "input_transcript.added", "item": map[string]any{"text": "ond"}})
	env.call.send(userTurnDone(""))
	rec := runTranscription(t, env, pcmFile(200), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"first second"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionWaitsForAnOpenUserTurnAfterTheAudio(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.send(userTurnDone("first"))
	env.call.send(map[string]any{"type": "turn.created", "turn": map[string]any{"role": "user"}})
	// The audio finishes while the second user turn is open; its turn.done arrives only after the last frame reached the sink.
	env.call.onFrame = func(n int64) {
		if n == 5 {
			env.call.send(userTurnDone("second"))
		}
	}
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"first second"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionWithoutAnyUserTurnWaitsForTheDeadline(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	transcriptionDeadlineSlack = 50 * time.Millisecond
	env.call.send(map[string]any{"type": "turn.done", "turn": map[string]any{"role": "assistant", "transcript": "only the assistant"}})
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionSinkFailureIs502(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.sinkErr = errors.New("track closed")
	// The events channel stays open and the deadline stays long: only the sink failure can end the request.
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "failed to stream the audio") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.call.closes.Load() != 1 {
		t.Fatalf("call closed %d times", env.call.closes.Load())
	}
}

func TestAudioTranscriptionCallEndingEarlyIs502(t *testing.T) {
	env := newTranscriptionTestEnv(t)
	env.call.closeOnce.Do(func() { close(env.call.done) })
	rec := runTranscription(t, env, pcmFile(100), nil, true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptionFeederHoldsBackAboveTheQueueCap(t *testing.T) {
	prev := transcriptionMaxQueuedFrames
	transcriptionMaxQueuedFrames = 10
	t.Cleanup(func() { transcriptionMaxQueuedFrames = prev })
	prevSilence := transcriptionTrailingSilence
	transcriptionTrailingSilence = 0
	t.Cleanup(func() { transcriptionTrailingSilence = prevSilence })

	newInput := func(t *testing.T) *realtimeAudioInput {
		in, _, _ := newFakeAudioInput(t)
		return in
	}
	// 400 ms is 20 frames: the fourth chunk would push the estimate to 15 frames above the 10-frame cap.
	samples := make([]int16, 400*24)

	t.Run("blocks until the context ends when nothing is written", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := feedTranscriptionAudio(ctx, newInput(t), samples, &atomic.Int64{}, make(chan struct{}), make(chan struct{}))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("blocks until the sink fails", func(t *testing.T) {
		failed := make(chan struct{})
		close(failed)
		err := feedTranscriptionAudio(context.Background(), newInput(t), samples, &atomic.Int64{}, make(chan struct{}), failed)
		if !errors.Is(err, errTranscriptionSinkFailed) {
			t.Fatalf("err = %v, want errTranscriptionSinkFailed", err)
		}
	})
	t.Run("resumes when frames were written", func(t *testing.T) {
		written := &atomic.Int64{}
		progress := make(chan struct{}, 1)
		result := make(chan error, 1)
		in := newInput(t)
		go func() {
			result <- feedTranscriptionAudio(context.Background(), in, samples, written, progress, make(chan struct{}))
		}()
		// One buffered wake-up token is never lost, so the feeder resumes whether it was already blocked or not.
		written.Store(20)
		progress <- struct{}{}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}
