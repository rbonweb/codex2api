package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const liveWSTestTimeout = 5 * time.Second

// liveWSMarker is the first byte of every packet the fake encoder returns, so tests can count speech frames.
const liveWSMarker = 0xA5

type liveWSFakeEncoder struct{}

func (liveWSFakeEncoder) Encode([]int16) ([]byte, error) { return []byte{liveWSMarker, 1, 2}, nil }
func (liveWSFakeEncoder) Close()                         {}

// liveWSFakeCall stands in for the ChatGPT Live call behind a public session.
type liveWSFakeCall struct {
	events chan []byte
	sent   chan []byte
	frames chan struct{}
	done   chan struct{}
	closed chan struct{}

	doneOnce  sync.Once
	closeOnce sync.Once
}

func newLiveWSFakeCall() *liveWSFakeCall {
	return &liveWSFakeCall{
		events: make(chan []byte, 64),
		sent:   make(chan []byte, 64),
		frames: make(chan struct{}, 8192),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

func (f *liveWSFakeCall) WriteOpusFrame(frame []byte, _ time.Duration) error {
	if len(frame) > 0 && frame[0] == liveWSMarker {
		f.frames <- struct{}{}
	}
	return nil
}
func (f *liveWSFakeCall) Events() <-chan []byte { return f.events }
func (f *liveWSFakeCall) Send(event []byte) error {
	f.sent <- append([]byte(nil), event...)
	return nil
}
func (f *liveWSFakeCall) Done() <-chan struct{} { return f.done }
func (f *liveWSFakeCall) CallID() string        { return "rtc_fake_live" }
func (f *liveWSFakeCall) Close() {
	f.closeOnce.Do(func() { close(f.closed) })
	f.doneOnce.Do(func() { close(f.done) })
}

// end simulates the Live call ending on its own.
func (f *liveWSFakeCall) end() {
	close(f.events)
	f.doneOnce.Do(func() { close(f.done) })
}

// push delivers one internal sideband event to the gateway.
func (f *liveWSFakeCall) push(event string) { f.events <- []byte(event) }

type liveWSTestServer struct {
	URL     string
	HTTP    string
	call    *liveWSFakeCall
	session chan []byte
	opens   atomic.Int32
}

func newLiveWSTestServer(t *testing.T, allowLive, supported bool) *liveWSTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := &liveWSTestServer{call: newLiveWSFakeCall(), session: make(chan []byte, 4)}

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
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return liveWSFakeEncoder{}, nil }

	var inflight sync.WaitGroup
	handler := &Handler{}
	router := gin.New()
	router.GET("/v1/live/sessions", func(c *gin.Context) {
		row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{AllowLive: allowLive}}
		c.Set(contextAPIKeyRow, row)
		c.Set(contextAPIKeyID, row.ID)
		inflight.Add(1)
		defer inflight.Done()
		handler.LiveSessionsWebSocket(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	// httptest does not wait for hijacked connections, so wait for the handlers before the seams are restored.
	t.Cleanup(inflight.Wait)
	srv.HTTP = server.URL + "/v1/live/sessions"
	srv.URL = "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/live/sessions"
	return srv
}

func (s *liveWSTestServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(s.URL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial failed: %v status=%d", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(liveWSTestTimeout))
	return conn
}

func liveWSRead(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	return msg
}

func liveWSReadUntil(t *testing.T, conn *websocket.Conn, want string) []byte {
	t.Helper()
	for {
		msg := liveWSRead(t, conn)
		if gjson.GetBytes(msg, "type").String() == want {
			return msg
		}
	}
}

func liveWSWrite(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

func liveWSTakeSent(t *testing.T, call *liveWSFakeCall) []byte {
	t.Helper()
	select {
	case event := <-call.sent:
		return event
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the fake call received no event")
		return nil
	}
}

func liveWSCountFrames(t *testing.T, call *liveWSFakeCall, want int) int {
	t.Helper()
	got := 0
	for got < want {
		select {
		case <-call.frames:
			got++
		case <-time.After(liveWSTestTimeout):
			return got
		}
	}
	return got
}

// liveWSExpectClose reads until the connection ends and returns the close code.
func liveWSExpectClose(t *testing.T, conn *websocket.Conn) int {
	t.Helper()
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		closeErr, ok := err.(*websocket.CloseError)
		if !ok {
			t.Fatalf("expected a close frame, got %v", err)
		}
		return closeErr.Code
	}
}

// liveWSStart sends session.start and answers it with the upstream session.started; it returns the public session.started event.
func liveWSStart(t *testing.T, srv *liveWSTestServer, conn *websocket.Conn, session string) []byte {
	t.Helper()
	liveWSWrite(t, conn, `{"type":"session.start","session":`+session+`}`)
	select {
	case <-srv.session:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not opened")
	}
	srv.call.push(`{"type":"session.started","session":{"id":"sess_up1"}}`)
	return liveWSReadUntil(t, conn, "session.started")
}

func liveWSPCM(samples int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, samples*2))
}

func TestLiveSessionsWSBadFirstMessageOpensNoCall(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSWrite(t, conn, `{"type":"session.update","session":{}}`)
	event := liveWSRead(t, conn)
	if gjson.GetBytes(event, "type").String() != "error" || gjson.GetBytes(event, "error.code").String() != "invalid_value" {
		t.Fatalf("event = %s", event)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d", code)
	}
	if srv.opens.Load() != 0 {
		t.Fatalf("a call was opened for a bad first message")
	}
}

func TestLiveSessionsWSUnknownSessionFieldIsRejected(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSWrite(t, conn, `{"type":"session.start","event_id":"s1","session":{"foo":1}}`)
	event := liveWSRead(t, conn)
	if gjson.GetBytes(event, "error.code").String() != "unknown_parameter" || gjson.GetBytes(event, "error.param").String() != "session.foo" || gjson.GetBytes(event, "error.client_event_id").String() != "s1" {
		t.Fatalf("event = %s", event)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d", code)
	}
	if srv.opens.Load() != 0 {
		t.Fatalf("a call was opened for an invalid session")
	}
}

func TestLiveSessionsWSStartOpensCallWithTheUpstreamSessionAndQueuesUntilStarted(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSWrite(t, conn, `{"type":"session.start","session":{"instructions":"Be brief.","audio":{"output":{"voice":"marin"}},"delegation":{"acknowledgement":"none"}}}`)
	var session []byte
	select {
	case session = <-srv.session:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not opened")
	}
	if got := gjson.GetBytes(session, "instructions").String(); got != "Be brief." {
		t.Fatalf("instructions = %q", got)
	}
	if got := gjson.GetBytes(session, "audio.output.voice").String(); got != "maple" {
		t.Fatalf("voice = %q", got)
	}
	if got := gjson.GetBytes(session, "delegation.type").String(); got != "client" {
		t.Fatalf("delegation type = %q", got)
	}
	if v := gjson.GetBytes(session, "delegation.ack_filler"); !v.Exists() || v.Bool() {
		t.Fatalf("ack_filler = %s", v.Raw)
	}

	// Two client events arrive before upstream session.started; an invalid event after them proves the loop processed both.
	liveWSWrite(t, conn, `{"type":"session.thinking.append","content":"first"}`)
	liveWSWrite(t, conn, `{"type":"session.input_audio.mute"}`)
	liveWSWrite(t, conn, `{"type":"nope"}`)
	if e := liveWSRead(t, conn); gjson.GetBytes(e, "type").String() != "error" {
		t.Fatalf("event = %s", e)
	}
	select {
	case event := <-srv.call.sent:
		t.Fatalf("event reached the call before session.started: %s", event)
	default:
	}

	srv.call.push(`{"type":"session.started","session":{"id":"sess_up1"}}`)
	started := liveWSReadUntil(t, conn, "session.started")
	if gjson.GetBytes(started, "session.instructions").String() != "Be brief." || gjson.GetBytes(started, "session.audio.output.voice").String() != "marin" || gjson.GetBytes(started, "session.delegation.acknowledgement").String() != "none" || gjson.GetBytes(started, "session.audio.format.rate").Int() != 24000 {
		t.Fatalf("session.started = %s", started)
	}
	first := liveWSTakeSent(t, srv.call)
	second := liveWSTakeSent(t, srv.call)
	if gjson.GetBytes(first, "type").String() != "session.context.append" || gjson.GetBytes(first, "channel").String() != "commentary" || gjson.GetBytes(first, "content.0.text").String() != "first" {
		t.Fatalf("first flushed event = %s", first)
	}
	if gjson.GetBytes(second, "type").String() != "input_audio.pause" {
		t.Fatalf("second flushed event = %s", second)
	}

	// After session.started events go straight through.
	liveWSWrite(t, conn, `{"type":"session.input_audio.unmute"}`)
	if got := liveWSTakeSent(t, srv.call); gjson.GetBytes(got, "type").String() != "input_audio.resume" {
		t.Fatalf("event = %s", got)
	}
}

func TestLiveSessionsWSDelegationCommentaryMuteAndUsage(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)

	srv.call.push(`{"type":"delegation.created","offset_ms":4000,"item":{"id":"item_9","type":"delegation","target":"client","content":[{"type":"input_text","text":"Open Safari "},{"type":"input_text","text":"and search"}]}}`)
	created := liveWSReadUntil(t, conn, "session.delegation.created")
	if gjson.GetBytes(created, "delegation.id").String() != "item_9" || gjson.GetBytes(created, "delegation.content").String() != "Open Safari and search" || gjson.GetBytes(created, "offset_ms").Int() != 4000 {
		t.Fatalf("session.delegation.created = %s", created)
	}

	liveWSWrite(t, conn, `{"type":"session.commentary.append","event_id":"c1","delegation_id":"item_9","content":"Safari is open."}`)
	sent := liveWSTakeSent(t, srv.call)
	if gjson.GetBytes(sent, "type").String() != "delegation.context.append" || gjson.GetBytes(sent, "delegation_item_id").String() != "item_9" || gjson.GetBytes(sent, "channel").String() != "speakable" || gjson.GetBytes(sent, "content.0.text").String() != "Safari is open." {
		t.Fatalf("sent = %s", sent)
	}
	srv.call.push(`{"type":"delegation.context.appended","delegation_item_id":"item_9"}`)
	ack := liveWSReadUntil(t, conn, "session.commentary.appended")
	if gjson.GetBytes(ack, "client_event_id").String() != "c1" {
		t.Fatalf("ack = %s", ack)
	}

	liveWSWrite(t, conn, `{"type":"session.input_audio.mute"}`)
	if got := liveWSTakeSent(t, srv.call); gjson.GetBytes(got, "type").String() != "input_audio.pause" {
		t.Fatalf("event = %s", got)
	}
	srv.call.push(`{"type":"input_audio.paused"}`)
	liveWSReadUntil(t, conn, "session.input_audio.muted")

	srv.call.push(`{"type":"session.usage.updated","usage":{"audio_duration_ms":13800}}`)
	usage := liveWSReadUntil(t, conn, "session.usage.updated")
	if gjson.GetBytes(usage, "usage.seconds").Int() < 14 {
		t.Fatalf("usage = %s", usage)
	}
}

func TestLiveSessionsWSInputAudioIsResampledTo24kHz(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{"audio":{"format":{"type":"audio/pcm","rate":16000}}}`)

	// 200 ms at 16 kHz is 3200 samples; at 24 kHz that is 4800 samples, ten 20 ms Opus frames.
	liveWSWrite(t, conn, `{"type":"session.input_audio.append","audio":"`+liveWSPCM(3200)+`"}`)
	got := liveWSCountFrames(t, srv.call, 10)
	if got < 9 || got > 10 {
		t.Fatalf("frames at the track = %d, want about 10", got)
	}
	select {
	case event := <-srv.call.sent:
		t.Fatalf("audio went to the sideband: %s", event)
	default:
	}
}

func TestLiveSessionsWSInvalidAudioReportsInvalidValue(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	liveWSWrite(t, conn, `{"type":"session.input_audio.append","audio":"@@not base64@@"}`)
	event := liveWSReadUntil(t, conn, "error")
	if gjson.GetBytes(event, "error.code").String() != "invalid_value" || gjson.GetBytes(event, "error.param").String() != "audio" {
		t.Fatalf("event = %s", event)
	}
}

func TestLiveSessionsWSOutputAudioUsesTheSessionFormat(t *testing.T) {
	cases := []struct {
		name, session string
		min, max      int
	}{
		{"pcm24", `{}`, 4800, 4800},
		{"pcm16", `{"audio":{"format":{"type":"audio/pcm","rate":16000}}}`, 3190, 3210},
		{"pcmu", `{"audio":{"format":{"type":"audio/pcmu"}}}`, 790, 810},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLiveWSTestServer(t, true, true)
			conn := srv.dial(t)
			liveWSStart(t, srv, conn, tc.session)
			// 100 ms of 24 kHz PCM16 is 2400 samples, 4800 bytes.
			srv.call.push(`{"type":"session.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(make([]byte, 4800)) + `"}`)
			event := liveWSReadUntil(t, conn, "session.output_audio.delta")
			raw, err := base64.StdEncoding.DecodeString(gjson.GetBytes(event, "delta").String())
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) < tc.min || len(raw) > tc.max {
				t.Fatalf("output bytes = %d, want %d..%d", len(raw), tc.min, tc.max)
			}
		})
	}
}

func TestLiveSessionsWSCloseRequestedWritesClosedThenCloses(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	liveWSWrite(t, conn, `{"type":"session.close"}`)
	closed := liveWSRead(t, conn)
	if gjson.GetBytes(closed, "type").String() != "session.closed" || gjson.GetBytes(closed, "reason").String() != "close_requested" {
		t.Fatalf("event = %s", closed)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d", code)
	}
	select {
	case <-srv.call.closed:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not closed")
	}
}

func TestLiveSessionsWSUpstreamEndIsRemoteHangup(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	srv.call.end()
	closed := liveWSReadUntil(t, conn, "session.closed")
	if gjson.GetBytes(closed, "reason").String() != "remote_hangup" {
		t.Fatalf("event = %s", closed)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d", code)
	}
	select {
	case <-srv.call.closed:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not closed")
	}
}

func TestLiveSessionsWSClientDisconnectReleasesTheCall(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	_ = conn.Close()
	select {
	case <-srv.call.closed:
	case <-time.After(liveWSTestTimeout):
		t.Fatal("the call was not closed after the client left")
	}
}

func TestLiveSessionsWSExpiryClosesWithExpired(t *testing.T) {
	prev := liveMaxSessionDuration
	liveMaxSessionDuration = 150 * time.Millisecond
	t.Cleanup(func() { liveMaxSessionDuration = prev })
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	closed := liveWSReadUntil(t, conn, "session.closed")
	if gjson.GetBytes(closed, "reason").String() != "expired" {
		t.Fatalf("event = %s", closed)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d", code)
	}
}

func TestLiveSessionsWSWithoutTheAudioBridgeIs501(t *testing.T) {
	srv := newLiveWSTestServer(t, true, false)
	_, resp, err := websocket.DefaultDialer.Dial(srv.URL, nil)
	if err == nil {
		t.Fatal("the upgrade succeeded without the audio bridge")
	}
	if resp == nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("response = %+v", resp)
	}
	if srv.opens.Load() != 0 {
		t.Fatal("a call was opened")
	}
}

func TestLiveSessionsWSRequiresAllowLive(t *testing.T) {
	srv := newLiveWSTestServer(t, false, true)
	_, resp, err := websocket.DefaultDialer.Dial(srv.URL, nil)
	if err == nil {
		t.Fatal("the upgrade succeeded without allow-live")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %+v", resp)
	}
}

func TestLiveSessionsWSGatewayErrorBecomesPublicErrorAndClose(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	openRealtimeAudioCall = func(*Handler, *gin.Context, json.RawMessage) (realtimeAudioCallConn, *liveGatewayError) {
		return nil, newLiveGatewayError(http.StatusServiceUnavailable, "no_available_account", "server_error", "No account is available.")
	}
	conn := srv.dial(t)
	liveWSWrite(t, conn, `{"type":"session.start","session":{}}`)
	event := liveWSRead(t, conn)
	if gjson.GetBytes(event, "type").String() != "error" || gjson.GetBytes(event, "error.type").String() != "server_error" || gjson.GetBytes(event, "error.code").String() != "no_available_account" {
		t.Fatalf("event = %s", event)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.CloseTryAgainLater {
		t.Fatalf("close code = %d", code)
	}
}

// The public sessions route must coexist with the :call_id sideband route in the real router.
func TestLiveSessionsRouteCoexistsWithTheSidebandRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	router := gin.New()
	h.RegisterRoutes(router)
	found := map[string]string{}
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet {
			found[route.Path] = route.Handler
		}
	}
	for path, handler := range map[string]string{
		"/v1/live/sessions": "LiveSessionsWebSocket",
		"/live/sessions":    "LiveSessionsWebSocket",
		"/v1/live/:call_id": "LiveSideband",
		"/live/:call_id":    "LiveSideband",
	} {
		if !strings.Contains(found[path], handler) {
			t.Fatalf("GET %s handler = %q, want %s", path, found[path], handler)
		}
	}

	// Dispatch with a small router that registers the same two patterns: a call id still reaches the sideband handler.
	var hit string
	mini := gin.New()
	mini.GET("/v1/live/sessions", func(c *gin.Context) { hit = "sessions" })
	mini.GET("/v1/live/:call_id", func(c *gin.Context) { hit = "sideband:" + c.Param("call_id") })
	for path, want := range map[string]string{"/v1/live/sessions": "sessions", "/v1/live/rtc_abc": "sideband:rtc_abc"} {
		hit = ""
		mini.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		if hit != want {
			t.Fatalf("GET %s reached %q, want %q", path, hit, want)
		}
	}
}
