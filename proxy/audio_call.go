package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/tidwall/gjson"
)

const (
	audioCallEventBuffer    = 256
	audioCallSendDeadline   = 5 * time.Second
	audioCallCloseDeadline  = time.Second
	audioCallSidebandReadMB = 8
	// audioCallStartQueueMax bounds the events Send holds back until upstream announces session.started.
	audioCallStartQueueMax = 256
)

var (
	// audioCallSetupTimeout bounds the whole setup; tests shorten it.
	audioCallSetupTimeout = 25 * time.Second

	errAudioCallClosed = errors.New("the audio call is closed")
	// errAudioCallStartQueueFull is returned by Send when too many events wait for session.started.
	errAudioCallStartQueueFull = errors.New("too many events are waiting for the audio call to start")

	// audioCallICEServers is the STUN server set the gateway peer gathers with; tests clear it to stay on loopback.
	audioCallICEServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	// audioCallSettingEngineForTest lets tests build the peer connection API with loopback candidates.
	audioCallSettingEngineForTest func(*webrtc.SettingEngine)
)

// audioCall is the gateway as a WebRTC peer of a ChatGPT Live call: it writes microphone Opus frames to the outgoing track and relays the sideband event stream. Output audio arrives from the sideband as PCM, so the inbound RTP track is only drained.
type audioCall struct {
	handler  *Handler
	record   *liveCallRecord
	pc       *webrtc.PeerConnection
	track    *webrtc.TrackLocalStaticSample
	sideband *websocket.Conn

	events chan []byte
	done   chan struct{}
	cancel context.CancelFunc

	// startGate holds the events Send receives before the upstream session.started; writeMu serializes it with the sideband writes.
	startGate sidebandStartGate
	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
	expiry    atomic.Pointer[time.Timer]
	setup     time.Duration
	opened    time.Time
}

// sidebandStartGate holds back client events until the upstream announces session.started: the internal sideband closes itself (1011) when the client sends anything earlier. Events queue in order, up to audioCallStartQueueMax, start hands them back exactly once for flushing, and later events pass straight through. It is safe for concurrent use; the audio call additionally serializes its flush with direct writes through writeMu so order is kept across both paths.
type sidebandStartGate struct {
	mu      sync.Mutex
	started bool
	closed  bool
	queue   [][]byte
}

// enqueue holds the event back and reports true while the session has not started. It reports false once the session started, or the gate closed, when the caller must write the event itself (a closed call refuses it first). A full queue returns errAudioCallStartQueueFull.
func (g *sidebandStartGate) enqueue(event []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || g.closed {
		return false, nil
	}
	if len(g.queue) >= audioCallStartQueueMax {
		return false, errAudioCallStartQueueFull
	}
	g.queue = append(g.queue, append([]byte(nil), event...))
	return true, nil
}

// start marks the session started and returns the queued events in arrival order; later calls return nil.
func (g *sidebandStartGate) start() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || g.closed {
		return nil
	}
	g.started = true
	queued := g.queue
	g.queue = nil
	return queued
}

// isStarted reports whether session.started has been seen.
func (g *sidebandStartGate) isStarted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started
}

// close drops whatever is still queued; nothing is queued or flushed afterwards.
func (g *sidebandStartGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.queue = nil
}

func newAudioCallPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{ICEServers: audioCallICEServers}
	if audioCallSettingEngineForTest != nil {
		engine := webrtc.SettingEngine{}
		audioCallSettingEngineForTest(&engine)
		return webrtc.NewAPI(webrtc.WithSettingEngine(engine)).NewPeerConnection(config)
	}
	return webrtc.NewPeerConnection(config)
}

func audioCallSetupError(status int, code api.ErrorCode, errType api.ErrorType, msg string) *liveGatewayError {
	return newLiveGatewayError(status, code, errType, msg)
}

// openAudioCall creates a ChatGPT Live call with the given session and returns once the media path is connected and the sideband is attached. On failure nothing stays held: the lease, usage record, key concurrency and peer connection are all released.
func (h *Handler) openAudioCall(c *gin.Context, session json.RawMessage) (*audioCall, *liveGatewayError) {
	started := time.Now()
	setupCtx, cancelSetup := context.WithTimeout(c.Request.Context(), audioCallSetupTimeout)
	defer cancelSetup()

	pc, err := newAudioCallPeerConnection()
	if err != nil {
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to create the audio peer connection")
	}
	connected := make(chan struct{})
	failed := make(chan struct{})
	var connectedOnce, failedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			failedOnce.Do(func() { close(failed) })
		}
	})
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "codex2api")
	if err != nil {
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to create the audio track")
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to attach the audio track")
	}
	go drainRTCP(sender)
	if _, err = pc.CreateDataChannel("oai-events", nil); err != nil {
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to create the events channel")
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1500)
		for {
			if _, _, readErr := remote.Read(buf); readErr != nil {
				return
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to create the audio offer")
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusInternalServerError, api.ErrCodeServerError, api.ErrorTypeServer, "Failed to apply the audio offer")
	}
	select {
	case <-gathered:
	case <-setupCtx.Done():
		_ = pc.Close()
		return nil, audioCallSetupError(http.StatusGatewayTimeout, api.ErrCodeUpstreamTimeout, api.ErrorTypeServer, "Timed out preparing the audio connection")
	}

	created, gwErr := h.createGatewayLiveCall(c, &liveCallRequest{SDP: pc.LocalDescription().SDP, Session: session})
	if gwErr != nil {
		_ = pc.Close()
		return nil, gwErr
	}
	record := created.Record
	fail := func(status int, code api.ErrorCode, msg string) (*audioCall, *liveGatewayError) {
		_ = pc.Close()
		h.liveCalls().finalize(record)
		return nil, audioCallSetupError(status, code, api.ErrorTypeServer, msg)
	}
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(created.AnswerSDP)}); err != nil {
		return fail(http.StatusBadGateway, api.ErrCodeUpstreamError, "The audio answer from upstream was rejected")
	}
	select {
	case <-connected:
	case <-failed:
		return fail(http.StatusBadGateway, api.ErrCodeUpstreamError, "The audio connection to upstream failed")
	case <-setupCtx.Done():
		return fail(http.StatusGatewayTimeout, api.ErrCodeUpstreamTimeout, "Timed out connecting the audio call")
	}

	// The call outlives the request and the setup deadline: keep the request values but drop its cancellation.
	callCtx, cancel := context.WithCancel(context.WithoutCancel(c.Request.Context()))
	conn, err := h.dialLiveSideband(callCtx, record)
	if err != nil {
		cancel()
		log.Printf("audio call %s: sideband attach failed: %s", record.CallHash, security.SanitizeLog(err.Error()))
		return fail(http.StatusBadGateway, api.ErrCodeUpstreamError, "Failed to attach to the upstream call")
	}
	conn.SetReadLimit(audioCallSidebandReadMB << 20)

	call := &audioCall{
		handler: h, record: record, pc: pc, track: track, sideband: conn,
		events: make(chan []byte, audioCallEventBuffer), done: make(chan struct{}),
		cancel: cancel, opened: started, setup: time.Since(started),
	}
	timer := time.AfterFunc(time.Until(record.ExpiresAt), call.Close)
	call.expiry.Store(timer)
	if call.closed.Load() {
		timer.Stop()
	}
	go call.readSideband(callCtx)
	go func() {
		select {
		case <-failed:
			call.Close()
		case <-call.done:
		}
	}()
	log.Printf("audio call %s opened: account=%d setup=%s", record.CallHash, record.AccountID, call.setup.Round(time.Millisecond))
	return call, nil
}

func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// readSideband forwards sideband messages to Events. A slow consumer blocks the reader instead of losing audio; Close unblocks it. The first session.started releases the events Send held back, in order, before anything else is forwarded.
func (a *audioCall) readSideband(ctx context.Context) {
	defer func() {
		close(a.events)
		a.Close()
	}()
	for {
		_, payload, err := a.sideband.ReadMessage()
		if err != nil {
			return
		}
		if !a.startGate.isStarted() && gjson.GetBytes(payload, "type").String() == "session.started" {
			if !a.flushQueued() {
				return
			}
		}
		select {
		case a.events <- payload:
		case <-ctx.Done():
			return
		}
	}
}

// flushQueued marks the session started and writes the events Send held back. It holds writeMu throughout, so a Send that arrives meanwhile writes after them. It returns false when a write failed.
func (a *audioCall) flushQueued() bool {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	queued := a.startGate.start()
	if a.closed.Load() {
		return true
	}
	for _, event := range queued {
		_ = a.sideband.SetWriteDeadline(time.Now().Add(audioCallSendDeadline))
		if err := a.sideband.WriteMessage(websocket.TextMessage, event); err != nil {
			return false
		}
	}
	return true
}

// WriteOpusFrame writes one Opus frame of duration d to the outgoing microphone track; it is an audioFrameSink.
func (a *audioCall) WriteOpusFrame(frame []byte, d time.Duration) error {
	if a.closed.Load() {
		return errAudioCallClosed
	}
	return a.track.WriteSample(media.Sample{Data: frame, Duration: d})
}

// Events carries the sideband server events; it is closed when the sideband ends.
func (a *audioCall) Events() <-chan []byte { return a.events }

// Send writes one client event to the sideband. Before the upstream session.started arrives the event is queued (up to audioCallStartQueueMax) and written in order once it does.
func (a *audioCall) Send(event []byte) error {
	if a.closed.Load() {
		return errAudioCallClosed
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.closed.Load() {
		return errAudioCallClosed
	}
	if queued, err := a.startGate.enqueue(event); queued || err != nil {
		return err
	}
	_ = a.sideband.SetWriteDeadline(time.Now().Add(audioCallSendDeadline))
	return a.sideband.WriteMessage(websocket.TextMessage, event)
}

// Done is closed once the call has been closed and released.
func (a *audioCall) Done() <-chan struct{} { return a.done }

// CallID is the upstream call id.
func (a *audioCall) CallID() string { return a.record.CallID }

// SetupDuration is the time from open to the sideband being ready.
func (a *audioCall) SetupDuration() time.Duration { return a.setup }

// Close ends the call: it asks upstream to close the session, closes the sideband and the peer connection, and releases the lease, key slot and usage record exactly once.
func (a *audioCall) Close() {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		a.startGate.close()
		if timer := a.expiry.Load(); timer != nil {
			timer.Stop()
		}
		// The goodbye is best effort: when a Send holds the write lock the sideband is closed instead, which also unblocks that writer, so releasing the lease, key slot and usage record is never delayed.
		// Before session.started nothing may be written: upstream would kill the sideband, and the close below ends the call anyway.
		if a.writeMu.TryLock() {
			if a.startGate.isStarted() {
				_ = a.sideband.SetWriteDeadline(time.Now().Add(audioCallCloseDeadline))
				_ = a.sideband.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.close"}`))
			}
			a.writeMu.Unlock()
		}
		_ = a.sideband.Close()
		a.cancel()
		_ = a.pc.Close()
		a.handler.liveCalls().finalize(a.record)
		log.Printf("audio call %s closed: account=%d lasted=%s", a.record.CallHash, a.record.AccountID, time.Since(a.opened).Round(time.Millisecond))
		close(a.done)
	})
}
