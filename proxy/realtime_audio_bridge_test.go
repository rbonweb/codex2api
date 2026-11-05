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
// The call is created lazily, so tests that need it send a trigger event (audio append, conversation.item.create or response.create) first.
func newBridgeTestServer(t *testing.T, allowLive, supported bool) *bridgeTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := &bridgeTestServer{call: newFakeBridgeCall(), session: make(chan []byte, 4), handlers: make(chan struct{}, 8)}

	prevOpen, prevSupported, prevEncoder := openRealtimeAudioCall, realtimeAudioBridgeSupported, newRealtimeAudioEncoder
	t.Cleanup(func() {
		openRealtimeAudioCall, realtimeAudioBridgeSupported, newRealtimeAudioEncoder = prevOpen, prevSupported, prevEncoder
	})
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

// startBridgeCall triggers lazy call creation with a response.create, waits for the creation session and the forwarded event, and returns the session.
func startBridgeCall(t *testing.T, srv *bridgeTestServer, conn *websocket.Conn) json.RawMessage {
	t.Helper()
	writeBridgeEvent(t, conn, `{"type":"response.create"}`)
	var session json.RawMessage
	select {
	case session = <-srv.session:
	case <-time.After(bridgeTestTimeout):
		t.Fatal("the call was not created")
	}
	if got := string(takeBridgeSent(t, srv.call)); got != `{"type":"response.create"}` {
		t.Fatalf("trigger sideband = %s", got)
	}
	return session
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
	session := startBridgeCall(t, srv, conn)
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
	startBridgeCall(t, srv, conn)

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
	srv := newBridgeTestServer(t, true, true)
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
	session := startBridgeCall(t, srv, conn)
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

// pingBridge waits until the client loop has handled every event written before it.
func pingBridge(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	writeBridgeEvent(t, conn, `{"type":"ping"}`)
	readBridgeUntil(t, conn, "pong")
}

func TestAudioBridgeNoCallBeforeATriggerEvenAfterSessionUpdates(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	for _, update := range []string{
		`{"type":"session.update","session":{"instructions":"First."}}`,
		`{"type":"session.update","session":{"instructions":"Second.","audio":{"output":{"voice":"echo"}}}}`,
		`{"type":"session.update","session":{"audio":{"output":{"voice":"coral"}}}}`,
	} {
		writeBridgeEvent(t, conn, update)
		readBridgeUntil(t, conn, "session.updated")
	}
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.commit"}`)
	readBridgeUntil(t, conn, "input_audio_buffer.committed")
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.clear"}`)
	pingBridge(t, conn)
	if srv.opens.Load() != 0 {
		t.Fatal("session.update, commit or clear created the call")
	}

	pcm := base64.StdEncoding.EncodeToString(make([]byte, 480))
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	session := <-srv.session
	if got := gjson.GetBytes(session, "instructions").String(); got != "Second." {
		t.Fatalf("creation instructions = %q, want the latest", got)
	}
	if got := gjson.GetBytes(session, "audio.output.voice").String(); got != mapRealtimeVoiceToLive("coral") {
		t.Fatalf("creation voice = %q, want the latest (coral)", got)
	}
	if srv.opens.Load() != 1 {
		t.Fatalf("opens = %d", srv.opens.Load())
	}
}

func TestAudioBridgeQueryParametersReachCreation(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime&instructions=Be%20brief%20%26%20kind&voice=echo&delegation=none")
	created := readBridgeUntil(t, conn, "session.created")
	if gjson.GetBytes(created, "session.instructions").String() != "Be brief & kind" || gjson.GetBytes(created, "session.audio.output.voice").String() != "echo" || gjson.GetBytes(created, "session.delegation.acknowledgement").String() != "none" {
		t.Fatalf("session.created = %s", created)
	}
	session := startBridgeCall(t, srv, conn)
	if gjson.GetBytes(session, "instructions").String() != "Be brief & kind" || gjson.GetBytes(session, "audio.output.voice").String() != mapRealtimeVoiceToLive("echo") {
		t.Fatalf("creation session = %s", session)
	}
	if ack := gjson.GetBytes(session, "delegation.ack_filler"); !ack.Exists() || ack.Bool() || gjson.GetBytes(session, "delegation.type").String() != "client" {
		t.Fatalf("delegation = %s", gjson.GetBytes(session, "delegation").Raw)
	}
}

func TestAudioBridgeSessionUpdateOverridesQueryParameters(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime&instructions=From%20the%20URL&voice=echo&delegation=none")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"instructions":"From the event.","delegation":{"type":"client"}}}`)
	if ack := readBridgeUntil(t, conn, "session.updated"); gjson.GetBytes(ack, "session.delegation.acknowledgement").String() != "none" {
		t.Fatalf("a delegation without acknowledgement should keep the query's none: %s", ack)
	}
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"delegation":{"type":"client","acknowledgement":"default"}}}`)
	readBridgeUntil(t, conn, "session.updated")
	session := startBridgeCall(t, srv, conn)
	if got := gjson.GetBytes(session, "instructions").String(); got != "From the event." {
		t.Fatalf("instructions = %q", got)
	}
	if got := gjson.GetBytes(session, "audio.output.voice").String(); got != mapRealtimeVoiceToLive("echo") {
		t.Fatalf("voice = %q, the query voice should remain", got)
	}
	if gjson.GetBytes(session, "delegation.ack_filler").Exists() {
		t.Fatalf("delegation = %s, want the default acknowledgement back", gjson.GetBytes(session, "delegation").Raw)
	}
}

func TestAudioBridgeRejectsBadDelegationQueryBeforeUpgrade(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	_, resp, err := websocket.DefaultDialer.Dial(srv.URL+"?model=gpt-realtime&delegation=responses", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dial err=%v resp=%v, want HTTP 400", err, resp)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if gjson.GetBytes(body, "error.code").String() != "unsupported_value" || !strings.Contains(gjson.GetBytes(body, "error.message").String(), "delegation") {
		t.Fatalf("body = %s", body)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("a call was opened")
	}
}

func TestAudioBridgeConversationItemIsQuietContext(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"the user is Ana"}]}}`)
	sideband := takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sideband, "type").String() != "session.context.append" || gjson.GetBytes(sideband, "channel").String() != "commentary" {
		t.Fatalf("sideband = %s, want a commentary context append", sideband)
	}
	if srv.opens.Load() != 1 {
		t.Fatal("conversation.item.create should create the call")
	}
}

func TestAudioBridgeDelegationOnSessionUpdate(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"event_id":"evt_resp","type":"session.update","session":{"delegation":{"type":"responses"}}}`)
	errEvent := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.code").String() != "unsupported_value" || gjson.GetBytes(errEvent, "error.param").String() != "session.delegation.type" || gjson.GetBytes(errEvent, "error.client_event_id").String() != "evt_resp" || gjson.GetBytes(errEvent, "error.type").String() != "invalid_request_error" {
		t.Fatalf("error = %s", errEvent)
	}

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"delegation":{"type":"client","acknowledgement":"none"}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.delegation.acknowledgement").String() != "none" {
		t.Fatalf("ack = %s", ack)
	}
	session := startBridgeCall(t, srv, conn)
	if ackFiller := gjson.GetBytes(session, "delegation.ack_filler"); !ackFiller.Exists() || ackFiller.Bool() {
		t.Fatalf("delegation = %s", gjson.GetBytes(session, "delegation").Raw)
	}
}

func TestAudioBridgeUnknownAndUnsupportedSessionFieldsError(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	for _, tc := range []struct{ update, code, param string }{
		{`{"type":"session.update","session":{"foo":1}}`, "unknown_parameter", "session.foo"},
		{`{"type":"session.update","session":{"audio":{"input":{"bar":true}}}}`, "unknown_parameter", "session.audio.input.bar"},
		{`{"type":"session.update","session":{"input_audio_format":"g711_ulaw"}}`, "unsupported_value", "session.input_audio_format"},
		{`{"type":"session.update","session":{"audio":{"output":{"format":{"type":"audio/pcm","rate":8000}}}}}`, "unsupported_value", "session.audio.output.format"},
		{`{"type":"session.update","session":{"turn_detection":{"create_response":"no"}}}`, "unsupported_value", "session.turn_detection.create_response"},
	} {
		writeBridgeEvent(t, conn, tc.update)
		errEvent := readBridgeUntil(t, conn, "error")
		if gjson.GetBytes(errEvent, "error.code").String() != tc.code || gjson.GetBytes(errEvent, "error.param").String() != tc.param {
			t.Fatalf("%s: error = %s", tc.update, errEvent)
		}
	}

	// Fields the bridge tolerates still ack.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"type":"realtime","tools":[],"tool_choice":"auto","temperature":0.8,"max_response_output_tokens":"inf","input_audio_transcription":{"model":"whisper-1"},"include":[],"tracing":null,"truncation":"auto","prompt":null,"audio":{"input":{"transcription":{"model":"whisper-1"},"noise_reduction":null}}}}`)
	readBridgeUntil(t, conn, "session.updated")
}

func TestAudioBridgeCreateResponseFalseIsRecordedAndResponseCreateStillForwarded(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"server_vad","create_response":false}}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if got := gjson.GetBytes(ack, "session.audio.input.turn_detection.create_response"); !got.Exists() || got.Bool() {
		t.Fatalf("ack = %s", ack)
	}
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"turn_detection":{"create_response":true}}}`)
	if got := gjson.GetBytes(readBridgeUntil(t, conn, "session.updated"), "session.audio.input.turn_detection.create_response"); !got.Bool() {
		t.Fatal("create_response true was not recorded")
	}
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"turn_detection":{"create_response":false}}}`)
	readBridgeUntil(t, conn, "session.updated")

	// The bridge sends nothing on its own; the client's explicit response.create is forwarded.
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 480))
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	<-srv.session
	pingBridge(t, conn)
	select {
	case event := <-srv.call.sent:
		t.Fatalf("the bridge sent %s on its own", event)
	default:
	}
	writeBridgeEvent(t, conn, `{"type":"response.create"}`)
	if got := string(takeBridgeSent(t, srv.call)); got != `{"type":"response.create"}` {
		t.Fatalf("sideband = %s", got)
	}
}

func TestAudioBridgeSixteenKilohertzInput(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"audio":{"input":{"format":{"type":"audio/pcm","rate":16000}}}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.audio.input.format.rate").Int() != 16000 || gjson.GetBytes(ack, "session.audio.output.format.rate").Int() != 24000 {
		t.Fatalf("ack = %s, want input 16000 and output 24000", ack)
	}

	// 100 ms at 16 kHz is 1600 samples; resampled to 24 kHz it is 2400, the same five 20 ms frames as 100 ms of native audio.
	pcm := base64.StdEncoding.EncodeToString(make([]byte, 3200))
	writeBridgeEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"`+pcm+`"}`)
	// The stateful converter holds back a few samples for interpolation, so the last frame may wait for the next chunk.
	for i := 0; i < 4; i++ {
		select {
		case <-srv.call.frames:
		case <-time.After(bridgeTestTimeout):
			t.Fatalf("got %d speech frames, want at least 4", i)
		}
	}
	pingBridge(t, conn)
	if len(srv.call.frames) > 1 {
		t.Fatal("more than five frames for 100 ms of 16 kHz audio")
	}
}

func TestAudioBridgeSixteenKilohertzOutput(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_audio_format":{"type":"audio/pcm","rate":16000}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.audio.output.format.rate").Int() != 16000 || gjson.GetBytes(ack, "session.audio.input.format.rate").Int() != 24000 {
		t.Fatalf("ack = %s, want output 16000 and input 24000", ack)
	}
	startBridgeCall(t, srv, conn)

	// 300 samples at 24 kHz leave as 200 samples at 16 kHz.
	srv.call.events <- []byte(`{"type":"session.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(make([]byte, 600)) + `"}`)
	delta := readBridgeUntil(t, conn, "response.output_audio.delta")
	raw, err := base64.StdEncoding.DecodeString(gjson.GetBytes(delta, "delta").String())
	if err != nil || len(raw) != 400 {
		t.Fatalf("delta decoded to %d bytes (err %v), want 400", len(raw), err)
	}
}

func TestAudioBridgeHandoffCallIsAlsoLazy(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-5.5")
	readBridgeEvent(t, conn)
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"output_modalities":["audio"],"instructions":"Talk."}}`)
	readBridgeUntil(t, conn, "session.updated")
	pingBridge(t, conn)
	if srv.opens.Load() != 0 {
		t.Fatal("the handoff created the call before any trigger")
	}
	session := startBridgeCall(t, srv, conn)
	if gjson.GetBytes(session, "instructions").String() != "Talk." {
		t.Fatalf("session = %s", session)
	}
}

// seqBridgeEncoder tags every packet with its sequence number so tests can check the order frames reach the track.
type seqBridgeEncoder struct{ n *atomic.Int32 }

func (e seqBridgeEncoder) Encode([]int16) ([]byte, error) {
	return []byte{bridgeTestMarker, byte(e.n.Add(1))}, nil
}
func (seqBridgeEncoder) Close() {}

func TestAudioBridgeAudioSentBeforeCreationIsFlushedInOrder(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
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
	startBridgeCall(t, srv, conn)

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
	startBridgeCall(t, srv, conn)

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
	startBridgeCall(t, srv, conn)
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
	startBridgeCall(t, srv, conn)

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
	startBridgeCall(t, srv, conn)

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
	startBridgeCall(t, srv, conn)

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
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"response.create"}`)
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
	session := startBridgeCall(t, srv, conn)
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

func TestAudioBridgeDelegationCreatedBecomesExtensionEvent(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	startBridgeCall(t, srv, conn)

	srv.call.events <- []byte(`{"type":"delegation.created","offset_ms":1200,"item":{"id":"dlg_1","content":[{"type":"input_text","text":"look up "},{"type":"input_text","text":"the weather"}]}}`)
	event := readBridgeUntil(t, conn, "session.delegation.created")
	if gjson.GetBytes(event, "offset_ms").Int() != 1200 || gjson.GetBytes(event, "delegation.id").String() != "dlg_1" || gjson.GetBytes(event, "delegation.target").String() != "client" || gjson.GetBytes(event, "delegation.content").String() != "look up the weather" {
		t.Fatalf("event = %s", event)
	}
}

func TestAudioBridgeContextAppendExtensionEventsAndAcks(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	// The first append creates the call and maps to a speakable delegation append.
	writeBridgeEvent(t, conn, `{"event_id":"c1","type":"session.commentary.append","delegation_id":"dlg_1","content":"It is sunny."}`)
	sent := takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "delegation.context.append" || gjson.GetBytes(sent, "delegation_item_id").String() != "dlg_1" || gjson.GetBytes(sent, "channel").String() != "speakable" || gjson.GetBytes(sent, "content.0.text").String() != "It is sunny." {
		t.Fatalf("commentary sideband = %s", sent)
	}
	if srv.opens.Load() != 1 {
		t.Fatal("the append did not create the call")
	}

	writeBridgeEvent(t, conn, `{"event_id":"t1","type":"session.thinking.append","delegation_id":"dlg_1","content":[{"type":"text","text":"checking "},{"type":"text","text":"sources"}]}`)
	sent = takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "delegation.context.append" || gjson.GetBytes(sent, "channel").String() != "commentary" || gjson.GetBytes(sent, "content.0.text").String() != "checking sources" {
		t.Fatalf("thinking sideband = %s", sent)
	}
	writeBridgeEvent(t, conn, `{"event_id":"t2","type":"session.thinking.append","content":"no delegation"}`)
	sent = takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "session.context.append" || gjson.GetBytes(sent, "channel").String() != "commentary" {
		t.Fatalf("session-level thinking sideband = %s", sent)
	}
	writeBridgeEvent(t, conn, `{"event_id":"i1","type":"session.instructions.append","content":"Prefer metric units."}`)
	sent = takeBridgeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "session.context.append" || gjson.GetBytes(sent, "channel").String() != "commentary" {
		t.Fatalf("instructions sideband = %s", sent)
	}
	// A conversation item shares the session-level FIFO but has no public ack of its own.
	writeBridgeEvent(t, conn, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"note"}]}}`)
	takeBridgeSent(t, srv.call)
	readBridgeUntil(t, conn, "conversation.item.created")
	writeBridgeEvent(t, conn, `{"event_id":"i2","type":"session.instructions.append","content":"Be brief."}`)
	takeBridgeSent(t, srv.call)

	// Session-level acks arrive in order: thinking t2, instructions i1, then the item (silent), then i2. Delegation acks are a separate FIFO.
	srv.call.events <- []byte(`{"type":"delegation.context.appended","delegation_item_id":"dlg_1"}`)
	srv.call.events <- []byte(`{"type":"session.context.appended"}`)
	srv.call.events <- []byte(`{"type":"session.context.appended"}`)
	srv.call.events <- []byte(`{"type":"session.context.appended"}`)
	srv.call.events <- []byte(`{"type":"delegation.context.appended","delegation_item_id":"dlg_1"}`)
	srv.call.events <- []byte(`{"type":"session.context.appended"}`)
	for _, want := range [][2]string{
		{"session.commentary.appended", "c1"},
		{"session.thinking.appended", "t2"},
		{"session.instructions.appended", "i1"},
		{"session.thinking.appended", "t1"},
		{"session.instructions.appended", "i2"},
	} {
		ack := readBridgeUntil(t, conn, want[0])
		if got := gjson.GetBytes(ack, "client_event_id").String(); got != want[1] {
			t.Fatalf("%s client_event_id = %q, want %q", want[0], got, want[1])
		}
	}
}

func TestAudioBridgeChunkedAppendIsAckedOnceOnTheLastChunk(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"event_id":"big","type":"session.thinking.append","content":"`+strings.Repeat("a", 1200)+`"}`)
	for i := 0; i < 3; i++ {
		takeBridgeSent(t, srv.call)
	}
	writeBridgeEvent(t, conn, `{"event_id":"after","type":"session.thinking.append","content":"small"}`)
	takeBridgeSent(t, srv.call)

	for i := 0; i < 4; i++ {
		srv.call.events <- []byte(`{"type":"session.context.appended"}`)
	}
	first := readBridgeUntil(t, conn, "session.thinking.appended")
	second := readBridgeUntil(t, conn, "session.thinking.appended")
	if gjson.GetBytes(first, "client_event_id").String() != "big" || gjson.GetBytes(second, "client_event_id").String() != "after" {
		t.Fatalf("acks = %s then %s, want big then after", first, second)
	}
}

func TestAudioBridgeContextAppendErrors(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")

	writeBridgeEvent(t, conn, `{"event_id":"e1","type":"session.commentary.append"}`)
	errEvent := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.code").String() != "missing_required_parameter" || gjson.GetBytes(errEvent, "error.param").String() != "content" || gjson.GetBytes(errEvent, "error.client_event_id").String() != "e1" {
		t.Fatalf("error = %s", errEvent)
	}
	writeBridgeEvent(t, conn, `{"type":"session.thinking.append","delegation_id":7,"content":"x"}`)
	errEvent = readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.param").String() != "delegation_id" {
		t.Fatalf("error = %s", errEvent)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("an invalid append created the call")
	}
}

func TestAudioBridgeHandoffParamErrorUsesPublicShape(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-5.5")
	readBridgeEvent(t, conn)
	writeBridgeEvent(t, conn, `{"event_id":"h1","type":"session.update","session":{"output_modalities":["audio"],"foo":1}}`)
	errEvent := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.code").String() != "unknown_parameter" || gjson.GetBytes(errEvent, "error.param").String() != "session.foo" || gjson.GetBytes(errEvent, "error.client_event_id").String() != "h1" {
		t.Fatalf("error = %s", errEvent)
	}
}

func TestAudioBridgeDelegationAcknowledgementIsImmutableAfterCreation(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime&delegation=none")
	readBridgeUntil(t, conn, "session.created")
	startBridgeCall(t, srv, conn)

	writeBridgeEvent(t, conn, `{"event_id":"d1","type":"session.update","session":{"delegation":{"type":"client","acknowledgement":"default"}}}`)
	ack := readBridgeUntil(t, conn, "session.updated")
	if gjson.GetBytes(ack, "session.delegation.acknowledgement").String() != "none" {
		t.Fatalf("ack = %s, want the accepted none", ack)
	}
	errEvent := readBridgeUntil(t, conn, "error")
	if gjson.GetBytes(errEvent, "error.code").String() != "immutable_field_update" || gjson.GetBytes(errEvent, "error.param").String() != "session.delegation.acknowledgement" || gjson.GetBytes(errEvent, "error.client_event_id").String() != "d1" {
		t.Fatalf("error = %s", errEvent)
	}
	// A delegation without acknowledgement is not a change.
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"delegation":{"type":"client"}}}`)
	readBridgeUntil(t, conn, "session.updated")
	pingBridge(t, conn)
}

// TestAudioBridgeSixteenKilohertzStreamKeepsResamplerState feeds odd-sized chunks and checks the totals match one big chunk, which a stateless per-chunk resampler would not give.
func TestAudioBridgeSixteenKilohertzOutputIsStateful(t *testing.T) {
	srv := newBridgeTestServer(t, true, true)
	conn := srv.dial(t, "?model=gpt-realtime")
	readBridgeUntil(t, conn, "session.created")
	writeBridgeEvent(t, conn, `{"type":"session.update","session":{"audio":{"output":{"format":{"type":"audio/pcm","rate":16000}}}}}`)
	readBridgeUntil(t, conn, "session.updated")
	startBridgeCall(t, srv, conn)

	// Three chunks of 301 samples (903 samples at 24 kHz) must yield 602 samples at 16 kHz in total, give or take the sample the resampler holds back.
	total := 0
	for i := 0; i < 3; i++ {
		srv.call.events <- []byte(`{"type":"session.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(make([]byte, 602)) + `"}`)
	}
	for i := 0; i < 3; i++ {
		delta := readBridgeUntil(t, conn, "response.output_audio.delta")
		raw, err := base64.StdEncoding.DecodeString(gjson.GetBytes(delta, "delta").String())
		if err != nil {
			t.Fatal(err)
		}
		total += len(raw) / 2
	}
	if total < 600 || total > 604 {
		t.Fatalf("total output samples = %d, want about 602", total)
	}
}
