package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const bridgeTestTimeout = 5 * time.Second

// bridgeTestMarker is the first byte of every packet the fake encoder returns, so tests can tell speech frames from pacer silence.
const bridgeTestMarker = 0xB7

type bridgeFakeEncoder struct{}

func (bridgeFakeEncoder) Encode([]int16) ([]byte, error) { return []byte{bridgeTestMarker, 1, 2}, nil }
func (bridgeFakeEncoder) Close()                         {}

// fakeBridgeCall stands in for the ChatGPT Live call behind an audio session.
type fakeBridgeCall struct {
	events chan []byte
	sent   chan []byte
	frames chan []byte
	done   chan struct{}
	closed chan struct{}

	doneOnce   sync.Once
	closeOnce  sync.Once
	closeCalls atomic.Int32
}

func newFakeBridgeCall() *fakeBridgeCall {
	return &fakeBridgeCall{
		events: make(chan []byte, 64),
		sent:   make(chan []byte, 64),
		frames: make(chan []byte, 4096),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

func (f *fakeBridgeCall) WriteOpusFrame(frame []byte, _ time.Duration) error {
	if len(frame) > 0 && frame[0] == bridgeTestMarker {
		f.frames <- append([]byte(nil), frame...)
	}
	return nil
}
func (f *fakeBridgeCall) Events() <-chan []byte { return f.events }
func (f *fakeBridgeCall) Send(event []byte) error {
	f.sent <- append([]byte(nil), event...)
	return nil
}
func (f *fakeBridgeCall) Done() <-chan struct{} { return f.done }
func (f *fakeBridgeCall) CallID() string        { return "rtc_fake" }
func (f *fakeBridgeCall) Close() {
	f.closeCalls.Add(1)
	f.closeOnce.Do(func() { close(f.closed) })
	f.doneOnce.Do(func() { close(f.done) })
}

// end simulates the Live call ending on its own: the sideband closes and the call is done.
func (f *fakeBridgeCall) end() {
	close(f.events)
	f.doneOnce.Do(func() { close(f.done) })
}

type bridgeTestServer struct {
	URL      string
	call     *fakeBridgeCall
	session  chan []byte
	opens    atomic.Int32
	handlers chan struct{}
}

// newBridgeTestServer serves /v1/realtime through RealtimeWebSocket with the audio seams replaced by fakes.
func newBridgeTestServer(t *testing.T, allowLive, supported bool) *bridgeTestServer {
	t.Helper()
	// A short grace keeps tests that never send a trigger event moving; tests that need to see the call before it exists use newBridgeTestServerWithGrace.
	return newBridgeTestServerWithGrace(t, allowLive, supported, 5*time.Millisecond)
}

// newBridgeTestServerWithGrace sets the creation grace before the server starts, so the handlers read it after a happens-before edge.
func newBridgeTestServerWithGrace(t *testing.T, allowLive, supported bool, grace time.Duration) *bridgeTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := &bridgeTestServer{call: newFakeBridgeCall(), session: make(chan []byte, 4), handlers: make(chan struct{}, 8)}

	prevOpen, prevSupported, prevEncoder, prevGrace := openRealtimeAudioCall, realtimeAudioBridgeSupported, newRealtimeAudioEncoder, realtimeAudioCreateGrace
	t.Cleanup(func() {
		openRealtimeAudioCall, realtimeAudioBridgeSupported, newRealtimeAudioEncoder, realtimeAudioCreateGrace = prevOpen, prevSupported, prevEncoder, prevGrace
	})
	realtimeAudioCreateGrace = grace
	openRealtimeAudioCall = func(_ *Handler, _ *gin.Context, session json.RawMessage) (realtimeAudioCallConn, *liveGatewayError) {
		srv.opens.Add(1)
		srv.session <- append([]byte(nil), session...)
		return srv.call, nil
	}
	realtimeAudioBridgeSupported = func() bool { return supported }
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return bridgeFakeEncoder{}, nil }

	var inflight sync.WaitGroup
	handler := &Handler{}
	router := gin.New()
	router.GET("/v1/realtime", func(c *gin.Context) {
		row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{AllowLive: allowLive}}
		c.Set(contextAPIKeyRow, row)
		c.Set(contextAPIKeyID, row.ID)
		inflight.Add(1)
		defer inflight.Done()
		handler.RealtimeWebSocket(c)
		srv.handlers <- struct{}{}
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	// httptest does not wait for hijacked connections, so wait for the handlers before the seams are restored. The client connections are closed first (their cleanups were registered later).
	t.Cleanup(inflight.Wait)
	srv.URL = "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	return srv
}

func (s *bridgeTestServer) dial(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(s.URL+query, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s failed: %v status=%d", query, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(bridgeTestTimeout))
	return conn
}

func readBridgeEvent(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	return msg
}

// readBridgeUntil reads events until one has the wanted type and returns it, failing on a read error.
func readBridgeUntil(t *testing.T, conn *websocket.Conn, want string) []byte {
	t.Helper()
	for {
		msg := readBridgeEvent(t, conn)
		if gjson.GetBytes(msg, "type").String() == want {
			return msg
		}
	}
}

func writeBridgeEvent(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func takeBridgeSent(t *testing.T, call *fakeBridgeCall) []byte {
	t.Helper()
	select {
	case event := <-call.sent:
		return event
	case <-time.After(bridgeTestTimeout):
		t.Fatal("the fake call received no event")
		return nil
	}
}

func TestAudioBridgeOpensCallAndSendsSessionCreated(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")

	created := readBridgeEvent(t, conn)
	if gjson.GetBytes(created, "type").String() != "session.created" {
		t.Fatalf("first event = %s", created)
	}
	if !strings.HasPrefix(gjson.GetBytes(created, "session.id").String(), "sess_") {
		t.Fatalf("session id = %s", created)
	}
	if gjson.GetBytes(created, "session.output_modalities.0").String() != "audio" {
		t.Fatalf("session modalities = %s", created)
	}
	session := <-srv.session
	if got := gjson.GetBytes(session, "model").String(); got != liveAudioDefaultModel {
		t.Fatalf("live model = %q", got)
	}
	if got := gjson.GetBytes(session, "audio.output.voice").String(); got != liveAudioDefaultVoice {
		t.Fatalf("live voice = %q", got)
	}
	if got := gjson.GetBytes(session, "delegation.type").String(); got != "client" {
		t.Fatalf("delegation = %q", got)
	}
}

func TestAudioBridgeAppendEncodesFramesAndCommitSharesItemID(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	// 100 ms of 24 kHz mono PCM16 is 2400 samples, 4800 bytes, which upsamples to five 20 ms frames.
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 4800))
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	for i := 0; i < 5; i++ {
		select {
		case <-srv.call.frames:
		case <-time.After(bridgeTestTimeout):
			t.Fatalf("got %d speech frames, want 5", i)
		}
	}

	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.commit"}`)
	committed := readBridgeUntil(t, conn, "input_audio_buffer.committed")
	itemID := gjson.GetBytes(committed, "item_id").String()
	if itemID == "" || !gjson.GetBytes(committed, "previous_item_id").Exists() || gjson.GetBytes(committed, "event_id").String() == "" {
		t.Fatalf("committed = %s", committed)
	}

	srv.call.events <- []byte(`{"type":"turn.created","turn":{"role":"user"}}`)
	srv.call.events <- []byte(`{"type":"input_transcript.added","item":{"text":"hello"}}`)
	srv.call.events <- []byte(`{"type":"turn.done","turn":{"role":"user","transcript":"hello"}}`)
	completed := readBridgeUntil(t, conn, "conversation.item.input_audio_transcription.completed")
	if got := gjson.GetBytes(completed, "item_id").String(); got != itemID {
		t.Fatalf("transcription item %q, committed item %q", got, itemID)
	}
	if got := gjson.GetBytes(completed, "transcript").String(); got != "hello" {
		t.Fatalf("transcript = %q", got)
	}
}

func TestAudioBridgeClearAckAndPing(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.clear"}`)
	readBridgeUntil(t, conn, "input_audio_buffer.cleared")
	writeBridgeEvent(t, conn, `{"type":"ping"}`)
	readBridgeUntil(t, conn, "pong")
	writeBridgeEvent(t, conn, `{"type":"response.cancel"}`)
	writeBridgeEvent(t, conn, `{"type":"nope"}`)
	errEvent := readBridgeUntil(t, conn, "error")
	if !strings.Contains(gjson.GetBytes(errEvent, "error.message").String(), "unsupported") {
		t.Fatalf("error = %s", errEvent)
	}
}

func TestAudioBridgeMapsUpstreamEvents(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	for _, event := range []string{
		`{"type":"turn.created","turn":{"role":"assistant"}}`,
		`{"type":"session.output_audio.delta","delta":"AAAA"}`,
		`{"type":"output_transcript.added","item":{"text":"Hi there"}}`,
		`{"type":"turn.done","turn":{"role":"assistant","transcript":"Hi there"}}`,
		`{"type":"input_transcript.added","item":{"text":"yo"}}`,
		`{"type":"turn.done","turn":{"role":"user","transcript":"yo"}}`,
		`{"type":"error","error":{"message":"boom"}}`,
	} {
		srv.call.events <- []byte(event)
	}
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_audio.delta",
		"response.output_audio_transcript.delta",
		"response.output_audio.done",
		"response.output_audio_transcript.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.done",
		"conversation.item.created",
		"conversation.item.input_audio_transcription.delta",
		"conversation.item.input_audio_transcription.completed",
		"error",
	}
	for _, wantType := range want {
		// Skip events this test does not pin down (for example speech_started).
		for {
			event := readBridgeEvent(t, conn)
			got := gjson.GetBytes(event, "type").String()
			if got == wantType {
				if wantType == "response.output_audio.delta" && gjson.GetBytes(event, "delta").String() != "AAAA" {
					t.Fatalf("audio delta = %s", event)
				}
				if wantType == "error" && !strings.Contains(string(event), "boom") {
					t.Fatalf("error event = %s", event)
				}
				break
			}
			if got == "input_audio_buffer.speech_started" {
				continue
			}
			t.Fatalf("event type %q, want %q", got, wantType)
		}
	}
}

func TestAudioBridgeTextItemAndResponseCreateReachTheCall(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"remember 42"}]}}`)
	sideband := takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sideband, "type").String() != "session.context.append" || !strings.Contains(string(sideband), "remember 42") {
		t.Fatalf("sideband = %s", sideband)
	}
	readBridgeUntil(t, conn, "conversation.item.created")

	writeBridgeEvent(t, conn, `{"type":"response.create","response":{"instructions":"ignored"}}`)
	if got := string(takeBridgeSent(t, srv.call)); got != `{"type":"response.create"}` {
		t.Fatalf("response.create sideband = %s", got)
	}
}

func TestAudioBridgeSessionUpdateBeforeCreationCarriesInstructionsAndVoice(t *testing.T) {
	srv := newBridgeTestServerWithGrace(t, true, true, time.Hour)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	if srv.opens.Load() != 0 {
		t.Fatal("the call was created before any trigger event")
	}

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"type":"realtime","instructions":"Answer in one short sentence.","audio":{"output":{"voice":"echo"}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.instructions").String() != "Answer in one short sentence." {
		t.Fatalf("ack = %s", ack)
	}
	session := <-srv.session
	if got := gjson.GetBytes(session, "instructions").String(); got != "Answer in one short sentence." {
		t.Fatalf("creation instructions = %q", got)
	}
	if got := gjson.GetBytes(session, "audio.output.voice").String(); got != "spruce" {
		t.Fatalf("creation voice = %q, want spruce (echo)", got)
	}
	select {
	case event := <-srv.call.sent:
		t.Fatalf("the pre-creation update was also forwarded upstream: %s", event)
	default:
	}
}

func TestAudioBridgeCreatesCallAfterGraceWithoutSessionUpdate(t *testing.T) {
	srv := newBridgeTestServerWithGrace(t, true, true, 10*time.Millisecond)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	select {
	case session := <-srv.session:
		if gjson.GetBytes(session, "instructions").String() != liveAudioDefaultInstructions {
			t.Fatalf("creation session = %s", session)
		}
	case <-time.After(bridgeTestTimeout):
		t.Fatal("the call was not created after the grace period")
	}
}

// seqBridgeEncoder tags every packet with its sequence number so tests can check the order frames reach the track.
type seqBridgeEncoder struct{ n *atomic.Int32 }

func (e seqBridgeEncoder) Encode([]int16) ([]byte, error) {
	return []byte{bridgeTestMarker, byte(e.n.Add(1))}, nil
}
func (seqBridgeEncoder) Close() {}

func TestAudioBridgeAudioSentBeforeCreationIsFlushedInOrder(t *testing.T) {
	srv := newBridgeTestServerWithGrace(t, true, true, time.Hour)
	var seq atomic.Int32
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return seqBridgeEncoder{n: &seq}, nil }
	gate := make(chan struct{})
	open := openRealtimeAudioCall
	openRealtimeAudioCall = func(h *Handler, c *gin.Context, session json.RawMessage) (realtimeAudioCallConn, *liveGatewayError) {
		<-gate
		return open(h, c, session)
	}
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	// Both appends arrive while the call is still being created; each is 100 ms, so five frames.
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 4800))
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	close(gate)
	for want := 1; want <= 10; want++ {
		select {
		case frame := <-srv.call.frames:
			if int(frame[1]) != want {
				t.Fatalf("frame %d carries sequence %d", want, frame[1])
			}
		case <-time.After(bridgeTestTimeout):
			t.Fatalf("got %d frames, want 10", want-1)
		}
	}
}

func TestAudioBridgeSessionUpdateAfterCreationIsImmutable(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"instructions":"First.","audio":{"output":{"voice":"echo"}}}}`)
	readBridgeUntil(t, conn, "session.updated")
	<-srv.session

	// Changing instructions and voice is refused with one error each, and nothing reaches upstream.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"instructions":"Second.","audio":{"output":{"voice":"coral"}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.instructions").String() != "First." || gjson.GetBytes(ack, "session.audio.output.voice").String() != "echo" {
		t.Fatalf("ack should show the accepted fields: %s", ack)
	}
	for _, want := range []string{"Instructions cannot be updated after initialization.", "Voice cannot be updated after initialization."} {
		errEvent := readBridgeUntil(t, conn, "error")
		if gjson.GetBytes(errEvent, "error.code").String() != "immutable_field_update" || gjson.GetBytes(errEvent, "error.message").String() != want {
			t.Fatalf("error = %s, want %q", errEvent, want)
		}
	}
	select {
	case event := <-srv.call.sent:
		t.Fatalf("an immutable update reached upstream: %s", event)
	default:
	}

	// Resending the accepted values is not a change, and the session keeps working.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"instructions":"First.","audio":{"output":{"voice":"echo"}}}}`)
	readBridgeUntil(t, conn, "session.updated")
	writeBridgeEvent(t, conn, `{"type":"ping"}`)
	if next := readBridgeEvent(t, conn); gjson.GetBytes(next, "type").String() != "pong" {
		t.Fatalf("event after an unchanged update = %s, want pong", next)
	}
	writeBridgeEvent(t, conn, `{"type":"response.create"}`)
	if got := string(takeBridgeSent(t, srv.call)); got != `{"type":"response.create"}` {
		t.Fatalf("sideband = %s", got)
	}
}

func TestAudioBridgeTextOnlySessionRenamesTranscriptEvents(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_modalities":["text"]}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.output_modalities.0").String() != "text" {
		t.Fatalf("ack = %s", ack)
	}
	for _, event := range []string{
		`{"type":"session.output_audio.delta","delta":"AAAA"}`,
		`{"type":"output_transcript.added","item":{"text":"Hi"}}`,
		`{"type":"turn.done","turn":{"role":"assistant","transcript":"Hi"}}`,
	} {
		srv.call.events <- []byte(event)
	}
	sawDelta, sawDone := false, false
	for !sawDone {
		event := readBridgeEvent(t, conn)
		switch gjson.GetBytes(event, "type").String() {
		case "response.output_audio.delta", "response.output_audio.done", "response.output_audio_transcript.delta", "response.output_audio_transcript.done":
			t.Fatalf("audio event leaked into a text-only session: %s", event)
		case "response.output_text.delta":
			sawDelta = gjson.GetBytes(event, "delta").String() == "Hi"
		case "response.output_text.done":
			if gjson.GetBytes(event, "text").String() != "Hi" || gjson.GetBytes(event, "transcript").Exists() {
				t.Fatalf("text done = %s", event)
			}
			sawDone = true
		}
	}
	if !sawDelta {
		t.Fatal("no response.output_text.delta before the done event")
	}
}

func TestAudioBridgeTextOnlySessionKeepsContentConsistent(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_modalities":["text"]}}`)
	readBridgeUntil(t, conn, "session.updated")

	srv.call.events <- []byte(`{"type":"output_transcript.added","item":{"text":"Hi"}}`)
	srv.call.events <- []byte(`{"type":"turn.done","turn":{"role":"assistant","transcript":"Hi"}}`)

	added := readBridgeUntil(t, conn, "response.content_part.added")
	if gjson.GetBytes(added, "part.type").String() != "text" || gjson.GetBytes(added, "part.transcript").Exists() {
		t.Fatalf("content_part.added = %s", added)
	}
	partDone := readBridgeUntil(t, conn, "response.content_part.done")
	if gjson.GetBytes(partDone, "part.type").String() != "text" || gjson.GetBytes(partDone, "part.text").String() != "Hi" || gjson.GetBytes(partDone, "part.transcript").Exists() {
		t.Fatalf("content_part.done = %s", partDone)
	}
	itemDone := readBridgeUntil(t, conn, "response.output_item.done")
	if gjson.GetBytes(itemDone, "item.content.0.type").String() != "output_text" || gjson.GetBytes(itemDone, "item.content.0.text").String() != "Hi" || gjson.GetBytes(itemDone, "item.content.0.transcript").Exists() {
		t.Fatalf("output_item.done = %s", itemDone)
	}
	done := readBridgeUntil(t, conn, "response.done")
	if gjson.GetBytes(done, "response.output.0.content.0.type").String() != "output_text" || gjson.GetBytes(done, "response.output.0.content.0.text").String() != "Hi" {
		t.Fatalf("response.done = %s", done)
	}
}

func TestAudioBridgeDeliversBufferedEventsWhenTheCallEnds(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	// The final events and the end of the call are ready together, as when the sideband closes right after turn.done.
	srv.call.events <- []byte(`{"type":"turn.done","turn":{"role":"assistant","transcript":"bye"}}`)
	srv.call.events <- []byte(`{"type":"error","error":{"message":"last words"}}`)
	srv.call.end()

	readBridgeUntil(t, conn, "response.done")
	readBridgeUntil(t, conn, "error")
	_, _, err := conn.ReadMessage()
	if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != websocket.CloseNormalClosure {
		t.Fatalf("read error = %#v, want a normal close", err)
	}
}

func TestAudioBridgeRejectsBadInput(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"!!not base64!!"}`)
	errEvent := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.code").String() != string(api.ErrCodeInvalidRequest) {
		t.Fatalf("bad base64 error = %s", errEvent)
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"AA=="}`)
	readBridgeUntil(t, conn, "error")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"input_audio_format":"g711_ulaw"}}`)
	errEvent = readBridgeUntil(t, conn, "error")
	if !strings.Contains(gjson.GetBytes(errEvent, "error.message").String(), "pcm16") {
		t.Fatalf("format error = %s", errEvent)
	}
	writeBridgeEvent(t, conn, `not json`)
	readBridgeUntil(t, conn, "error")
}

func TestAudioBridgeClientDisconnectClosesCallOnce(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	<-srv.session

	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(bridgeTestTimeout))
	_ = conn.Close()
	select {
	case <-srv.call.closed:
	case <-time.After(bridgeTestTimeout):
		t.Fatal("the call was not closed after the client left")
	}
	<-srv.handlers
	if got := srv.call.closeCalls.Load(); got != 1 {
		t.Fatalf("call closed %d times, want 1", got)
	}
}

func TestAudioBridgeCallEndingClosesClient(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	<-srv.session

	srv.call.end()
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.CloseNormalClosure || closeErr.Text != realtimeAudioEndReason {
		t.Fatalf("read error = %#v, want a normal close with reason %q", err, realtimeAudioEndReason)
	}
	<-srv.handlers
	if got := srv.call.closeCalls.Load(); got != 1 {
		t.Fatalf("call closed %d times, want 1", got)
	}
}

func TestAudioBridgeOpenFailureIsAnErrorEventThenClose(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	openRealtimeAudioCall = func(*Handler, *gin.Context, json.RawMessage) (realtimeAudioCallConn, *liveGatewayError) {
		return nil, newLiveGatewayError(http.StatusServiceUnavailable, api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "no capacity")
	}
	conn := srv.dial(t, "?model=gpt-realtime")
	errEvent := readBridgeUntil(t, conn, "error")
	if !strings.Contains(string(errEvent), "no capacity") {
		t.Fatalf("error event = %s", errEvent)
	}
	_, _, err := conn.ReadMessage()
	if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != websocket.CloseTryAgainLater {
		t.Fatalf("read error = %#v, want try again later", err)
	}
}

func TestAudioBridgeUnavailableBuildAnswers501(t *testing.T) {
	srv := newBridgeTestServer(t, true, false)
	_, resp, err := websocket.DefaultDialer.Dial(srv.URL+"?model=gpt-realtime", nil)
	if err == nil || resp == nil {
		t.Fatalf("dial should fail with an HTTP error, err=%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != audioBridgeUnavailableStatus || !strings.Contains(string(body), "audio_bridge_unavailable") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("the call seam ran on an unsupported build")
	}
}

func TestAudioBridgeRequiresAllowLive(t *testing.T) {
	srv := newBridgeTestServer(t, false, true)
	_, resp, err := websocket.DefaultDialer.Dial(srv.URL+"?model=gpt-realtime", nil)
	if err == nil || resp == nil {
		t.Fatalf("dial should fail with an HTTP error, err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("the call seam ran without allow_live")
	}
}

func TestAudioBridgeTextSessionHandsOverOnAudioSessionUpdate(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-5.5")
	created := readBridgeEvent(t, conn)
	if gjson.GetBytes(created, "session.output_modalities.0").String() != "text" {
		t.Fatalf("text session.created = %s", created)
	}

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_modalities":["audio"],"instructions":"Talk.","audio":{"output":{"voice":"verse"}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.output_modalities.0").String() != "audio" {
		t.Fatalf("handoff ack = %s", ack)
	}
	session := <-srv.session
	if gjson.GetBytes(session, "instructions").String() != "Talk." || gjson.GetBytes(session, "audio.output.voice").String() != "breeze" {
		t.Fatalf("live session = %s", session)
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.clear"}`)
	readBridgeUntil(t, conn, "input_audio_buffer.cleared")
}

func TestAudioBridgeTextHandoffFailsAsEventsWithoutAllowLiveOrBridge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allowLive bool
		supported bool
		wantCode  string
		wantClose int
	}{
		{"no allow_live", false, true, string(api.ErrCodeInsufficientScope), websocket.ClosePolicyViolation},
		{"no bridge", true, false, "audio_bridge_unavailable", websocket.CloseTryAgainLater},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newBridgeTestServer(t, tc.allowLive, tc.supported)
			conn := srv.dial(t, "?model=gpt-5.5")
			readBridgeEvent(t, conn)
			writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_modalities":["audio"]}}`)
			errEvent := readBridgeEvent(t, conn)
			if gjson.GetBytes(errEvent, "type").String() != "error" || gjson.GetBytes(errEvent, "error.code").String() != tc.wantCode {
				t.Fatalf("error event = %s", errEvent)
			}
			_, _, err := conn.ReadMessage()
			if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != tc.wantClose {
				t.Fatalf("read error = %#v, want close %d", err, tc.wantClose)
			}
			if srv.opens.Load() != 0 {
				t.Fatal("the call seam ran")
			}
		})
	}
}

func TestAudioBridgeTextOnlyModelKeepsTextBehaviour(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-5.5")
	created := readBridgeEvent(t, conn)
	if gjson.GetBytes(created, "type").String() != "session.created" || gjson.GetBytes(created, "session.output_modalities.0").String() != "text" {
		t.Fatalf("text session.created = %s", created)
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.clear"}`)
	errEvent := readBridgeEvent(t, conn)
	if !strings.Contains(gjson.GetBytes(errEvent, "error.message").String(), "text events only") {
		t.Fatalf("text path error = %s", errEvent)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("a text model opened an audio call")
	}
}

func TestRealtimeAudioTextOnlyEventDropsAndRenames(t *testing.T) {
	if _, keep := realtimeAudioTextOnlyEvent([]byte(`{"type":"response.output_audio.delta","delta":"x"}`)); keep {
		t.Fatal("audio delta kept")
	}
	got, keep := realtimeAudioTextOnlyEvent([]byte(`{"type":"response.output_audio_transcript.delta","delta":"x"}`))
	if !keep || gjson.GetBytes(got, "type").String() != "response.output_text.delta" {
		t.Fatalf("delta = %s", got)
	}
	untouched := []byte(`{"type":"response.done"}`)
	if got, keep := realtimeAudioTextOnlyEvent(untouched); !keep || string(got) != string(untouched) {
		t.Fatalf("other event changed: %s", got)
	}
}

func TestRealtimeAudioInputReportsClosedTrackAfterSinkFailure(t *testing.T) {
	prev := newRealtimeAudioEncoder
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return bridgeFakeEncoder{}, nil }
	t.Cleanup(func() { newRealtimeAudioEncoder = prev })

	sinkErr := errors.New("track write failed")
	input, err := newRealtimeAudioInput(func([]byte, time.Duration) error { return sinkErr })
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	input.Start(context.Background())
	<-input.pacer.done

	for name, call := range map[string]func() error{
		"append": func() error { return input.AppendPCM16(make([]byte, 4800)) },
		"commit": func() error { return input.Commit() },
	} {
		err := call()
		if err == nil || !errors.Is(err, sinkErr) || !strings.Contains(err.Error(), "live call audio track closed") {
			t.Fatalf("%s error = %v, want the closed-track error wrapping the sink error", name, err)
		}
	}
}
