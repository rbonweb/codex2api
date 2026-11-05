package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const audioCallTestTimeout = 20 * time.Second

// audioCallFixture is a fake ChatGPT Live upstream on loopback: an HTTP create endpoint answered by an in-process pion peer, and a sideband WebSocket.
type audioCallFixture struct {
	t          *testing.T
	answerer   *webrtc.PeerConnection
	connected  chan struct{}
	rtp        chan []byte
	sbConn     chan *websocket.Conn
	sbReceived chan string
	sbClose    chan struct{}
	sbOnce     sync.Once
	usageLogs  atomic.Int32
	session    chan json.RawMessage
	// badAnswer makes the fake upstream answer with garbage; deadAnswer answers validly from a peer it closes at once, so the media never connects.
	badAnswer, deadAnswer atomic.Bool
}

func newAudioCallFixture(t *testing.T, createStatus int) (*Handler, *audioCallFixture) {
	t.Helper()
	previousICE, previousEngine := audioCallICEServers, audioCallSettingEngineForTest
	t.Cleanup(func() { audioCallICEServers, audioCallSettingEngineForTest = previousICE, previousEngine })
	audioCallICEServers = nil
	audioCallSettingEngineForTest = func(engine *webrtc.SettingEngine) {
		engine.SetIncludeLoopbackCandidate(true)
		engine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	}

	fx := &audioCallFixture{
		t: t, connected: make(chan struct{}), rtp: make(chan []byte, 64),
		sbConn: make(chan *websocket.Conn, 1), sbReceived: make(chan string, 16), sbClose: make(chan struct{}),
		session: make(chan json.RawMessage, 1),
	}
	previousLog := liveUsageLogForTest
	t.Cleanup(func() { liveUsageLogForTest = previousLog })
	liveUsageLogForTest = func(*database.UsageLogInput) { fx.usageLogs.Add(1) }

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	sideband := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		fx.sbConn <- conn
		go func() {
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				fx.sbReceived <- string(payload)
			}
		}()
		<-fx.sbClose
	}))
	t.Cleanup(sideband.Close)
	t.Cleanup(fx.closeSideband)
	previousBase := liveSidebandUpstreamBaseURL
	t.Cleanup(func() { liveSidebandUpstreamBaseURL = previousBase })
	liveSidebandUpstreamBaseURL = "ws" + strings.TrimPrefix(sideband.URL, "http") + "/v1/live"

	handler, _ := gatewayLiveHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if createStatus != http.StatusCreated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(createStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"bad session"}}`))
			return
		}
		var body struct {
			SDP     string          `json:"sdp"`
			Session json.RawMessage `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fx.session <- body.Session
		answer := "v=0\r\nnot an sdp answer\r\n"
		if !fx.badAnswer.Load() {
			var err error
			if answer, err = fx.answer(body.SDP); err != nil {
				t.Errorf("answer: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if fx.deadAnswer.Load() {
				_ = fx.answerer.Close()
			}
		}
		w.Header().Set("Location", "/v1/realtime/calls/rtc_test1")
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
	}, 1)
	return handler, fx
}

// closeSideband ends the fake sideband connection; it is safe to call more than once.
func (fx *audioCallFixture) closeSideband() { fx.sbOnce.Do(func() { close(fx.sbClose) }) }

func (fx *audioCallFixture) answer(offer string) (string, error) {
	engine := webrtc.SettingEngine{}
	audioCallSettingEngineForTest(&engine)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(engine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	fx.t.Cleanup(func() { _ = pc.Close() })
	fx.answerer = pc
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			close(fx.connected)
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			fx.rtp <- append([]byte(nil), pkt.Payload...)
		}
	})
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-gathered
	return pc.LocalDescription().SDP, nil
}

func audioCallTestKey() *database.APIKeyRow {
	return &database.APIKeyRow{ID: 31, Limits: database.APIKeyLimits{AllowLive: true, MaxConcurrency: 1}}
}

func openTestAudioCall(t *testing.T, handler *Handler) *audioCall {
	t.Helper()
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	call, gwErr := handler.openAudioCall(c, json.RawMessage(`{"model":"gpt-live-1-codex"}`))
	if gwErr != nil {
		t.Fatalf("openAudioCall: %+v", gwErr)
	}
	t.Cleanup(call.Close)
	return call
}

func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(audioCallTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func assertKeyConcurrencyFree(t *testing.T, handler *Handler) {
	t.Helper()
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	release, _, ok := handler.acquireAPIKeyConcurrencyForWebSocket(c)
	if !ok {
		t.Fatal("key concurrency is still held")
	}
	release()
}

func TestAudioCallOpenRelaysEventsMediaAndCloses(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	call := openTestAudioCall(t, handler)

	if call.CallID() != "rtc_test1" || call.SetupDuration() <= 0 {
		t.Fatalf("call id=%q setup=%s", call.CallID(), call.SetupDuration())
	}
	if session := receiveWithin(t, fx.session, "create session"); !strings.Contains(string(session), "gpt-live-1-codex") {
		t.Fatalf("session = %s", session)
	}
	sb := receiveWithin(t, fx.sbConn, "sideband connection")

	if err := sb.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`)); err != nil {
		t.Fatal(err)
	}
	if got := receiveWithin(t, call.Events(), "sideband event"); string(got) != `{"type":"session.created"}` {
		t.Fatalf("event = %s", got)
	}

	// Nothing may reach the sideband before session.started; once it arrives, sends go straight through.
	if err := sb.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started"}`)); err != nil {
		t.Fatal(err)
	}
	receiveWithin(t, call.Events(), "session.started event")
	if err := call.Send([]byte(`{"type":"input_text","text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if got := receiveWithin(t, fx.sbReceived, "sent event"); got != `{"type":"input_text","text":"hi"}` {
		t.Fatalf("sideband received %s", got)
	}

	receiveWithin(t, fx.connected, "answerer connection")
	frame := []byte{0x78, 0x01, 0x02, 0x03, 0x04}
	for i := 0; i < 3; i++ {
		if err := call.WriteOpusFrame(frame, 20*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if got := receiveWithin(t, fx.rtp, "RTP payload"); string(got) != string(frame) {
		t.Fatalf("rtp payload = %v", got)
	}

	call.Close()
	call.Close()
	select {
	case <-call.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	sawClose := false
	for !sawClose {
		got := receiveWithin(t, fx.sbReceived, "session.close")
		sawClose = got == `{"type":"session.close"}`
	}
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	if err := call.Send([]byte(`{}`)); err == nil {
		t.Fatal("Send after Close should fail")
	}
	if err := call.WriteOpusFrame(frame, 20*time.Millisecond); err == nil {
		t.Fatal("WriteOpusFrame after Close should fail")
	}
	assertKeyConcurrencyFree(t, handler)
	fx.closeSideband()
}

func TestAudioCallSidebandEndClosesEventsAndCall(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	call := openTestAudioCall(t, handler)
	receiveWithin(t, fx.sbConn, "sideband connection")
	fx.closeSideband()

	select {
	case _, ok := <-call.Events():
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(audioCallTestTimeout):
		t.Fatal("Events was not closed after the sideband ended")
	}
	select {
	case <-call.Done():
	case <-time.After(audioCallTestTimeout):
		t.Fatal("Done was not closed after the sideband ended")
	}
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	assertKeyConcurrencyFree(t, handler)
}

func TestAudioCallUpstreamRejectionKeepsStatusAndHoldsNothing(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusBadRequest)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	call, gwErr := handler.openAudioCall(c, json.RawMessage(`{"model":"gpt-live-1-codex"}`))
	if call != nil || gwErr == nil || gwErr.Status != http.StatusBadRequest || gwErr.Err.Message != "bad session" {
		t.Fatalf("call=%v gwErr=%+v", call, gwErr)
	}
	if c.Writer.Written() || rec.Body.Len() != 0 {
		t.Fatalf("wrote to the client: %q", rec.Body.String())
	}
	assertKeyConcurrencyFree(t, handler)
	if fx.usageLogs.Load() != 0 {
		t.Fatalf("usage logs = %d", fx.usageLogs.Load())
	}
}

func TestAudioCallSidebandFailureReleasesTheCall(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	dead := httptest.NewServer(http.NotFoundHandler())
	liveSidebandUpstreamBaseURL = "ws" + strings.TrimPrefix(dead.URL, "http") + "/v1/live"
	dead.Close()

	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	call, gwErr := handler.openAudioCall(c, json.RawMessage(`{"model":"gpt-live-1-codex"}`))
	if call != nil || gwErr == nil || gwErr.Status != http.StatusBadGateway {
		t.Fatalf("call=%v gwErr=%+v", call, gwErr)
	}
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	assertKeyConcurrencyFree(t, handler)
}

func TestAudioCallAnswerRejectedReleasesTheCallOnce(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	fx.badAnswer.Store(true)
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	call, gwErr := handler.openAudioCall(c, json.RawMessage(`{"model":"gpt-live-1-codex"}`))
	if call != nil || gwErr == nil || gwErr.Status != http.StatusBadGateway {
		t.Fatalf("call=%v gwErr=%+v", call, gwErr)
	}
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	assertKeyConcurrencyFree(t, handler)
}

func TestAudioCallConnectTimeoutReleasesTheCallOnce(t *testing.T) {
	previous := audioCallSetupTimeout
	t.Cleanup(func() { audioCallSetupTimeout = previous })
	audioCallSetupTimeout = 3 * time.Second
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	fx.deadAnswer.Store(true)
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime/calls", nil, audioCallTestKey())
	call, gwErr := handler.openAudioCall(c, json.RawMessage(`{"model":"gpt-live-1-codex"}`))
	if call != nil || gwErr == nil || (gwErr.Status != http.StatusGatewayTimeout && gwErr.Status != http.StatusBadGateway) {
		t.Fatalf("call=%v gwErr=%+v", call, gwErr)
	}
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	assertKeyConcurrencyFree(t, handler)
}

func TestAudioCallConcurrentSendWriteAndClose(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	call := openTestAudioCall(t, handler)
	sb := receiveWithin(t, fx.sbConn, "sideband connection")
	go func() {
		for range fx.sbReceived {
		}
	}()
	if err := sb.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started"}`)); err != nil {
		t.Fatal(err)
	}
	receiveWithin(t, call.Events(), "session.started event")

	var wg sync.WaitGroup
	frame := []byte{0x78, 0x01, 0x02}
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = call.Send([]byte(`{"type":"ping"}`))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = call.WriteOpusFrame(frame, 20*time.Millisecond)
			}
		}()
		go func() {
			defer wg.Done()
			call.Close()
		}()
	}
	wg.Wait()
	receiveWithin(t, call.Done(), "Done")
	if fx.usageLogs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", fx.usageLogs.Load())
	}
	assertKeyConcurrencyFree(t, handler)
}

func TestAudioCallSendQueuesUntilSessionStarted(t *testing.T) {
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	call := openTestAudioCall(t, handler)
	sb := receiveWithin(t, fx.sbConn, "sideband connection")

	for _, event := range []string{`{"n":1}`, `{"n":2}`} {
		if err := call.Send([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-fx.sbReceived:
		t.Fatalf("an event reached the sideband before session.started: %s", got)
	default:
	}

	if err := sb.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started"}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{"n":1}`, `{"n":2}`} {
		if got := receiveWithin(t, fx.sbReceived, "queued event"); got != want {
			t.Fatalf("flushed %s, want %s", got, want)
		}
	}
	if got := receiveWithin(t, call.Events(), "session.started event"); string(got) != `{"type":"session.started"}` {
		t.Fatalf("event = %s", got)
	}
	if err := call.Send([]byte(`{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	if got := receiveWithin(t, fx.sbReceived, "direct event"); got != `{"n":3}` {
		t.Fatalf("direct send = %s", got)
	}
}

func TestSidebandStartGate(t *testing.T) {
	var gate sidebandStartGate
	for _, event := range []string{"a", "b", "c"} {
		if queued, err := gate.enqueue([]byte(event)); !queued || err != nil {
			t.Fatalf("enqueue %s = %v, %v", event, queued, err)
		}
	}
	queued := gate.start()
	if len(queued) != 3 || string(queued[0]) != "a" || string(queued[1]) != "b" || string(queued[2]) != "c" {
		t.Fatalf("start returned %q, want a b c in order", queued)
	}
	if again := gate.start(); again != nil {
		t.Fatalf("a second start returned %q", again)
	}
	if queued, err := gate.enqueue([]byte("d")); queued || err != nil {
		t.Fatalf("after start enqueue = %v, %v, want a direct write", queued, err)
	}

	var bounded sidebandStartGate
	for i := 0; i < audioCallStartQueueMax; i++ {
		if queued, err := bounded.enqueue([]byte("x")); !queued || err != nil {
			t.Fatalf("enqueue %d = %v, %v", i, queued, err)
		}
	}
	if _, err := bounded.enqueue([]byte("x")); err != errAudioCallStartQueueFull {
		t.Fatalf("enqueue past the bound = %v, want errAudioCallStartQueueFull", err)
	}

	var closing sidebandStartGate
	_, _ = closing.enqueue([]byte("x"))
	closing.close()
	if dropped := closing.start(); dropped != nil {
		t.Fatalf("close kept %q", dropped)
	}
	if queued, _ := closing.enqueue([]byte("y")); queued {
		t.Fatal("a closed gate queued an event")
	}
}

func TestAudioCallClosesWhenSessionNeverStarts(t *testing.T) {
	previous := audioCallStartTimeout
	t.Cleanup(func() { audioCallStartTimeout = previous })
	audioCallStartTimeout = 50 * time.Millisecond
	handler, fx := newAudioCallFixture(t, http.StatusCreated)
	call := openTestAudioCall(t, handler)
	receiveWithin(t, fx.sbConn, "sideband connection")
	if err := call.Send([]byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	receiveWithin(t, call.Done(), "the call closing without session.started")
	if err := call.Send([]byte(`{"n":2}`)); err == nil {
		t.Fatal("Send after the start timeout should fail")
	}
}
