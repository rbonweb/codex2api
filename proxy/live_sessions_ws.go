package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// liveSessionStartTimeout is how long a client has to send session.start after the upgrade.
var liveSessionStartTimeout = 10 * time.Second

// liveSessionTickInterval is how often the transport asks the translator for gateway-clock events.
const liveSessionTickInterval = time.Second

// liveSessionExpiryMargin is how much earlier than the upstream deadline the gateway ends a capped session, so the public reason is expired.
const liveSessionExpiryMargin = 2 * time.Second

// liveSwapStartTimeout is how long a voice change waits for the new call to start; tests shorten it.
var liveSwapStartTimeout = 20 * time.Second

// liveSwapQueuedForTest, when set by a test, is called after each client event was held back during a voice change.
var liveSwapQueuedForTest func()

// liveSwapQueueMaxEvents and liveSwapQueueMaxBytes bound the client events held while a voice change opens the new call.
const (
	liveSwapQueueMaxEvents = 4096
	liveSwapQueueMaxBytes  = 8 << 20
)

// liveSessionCloseGrace is how long the transport waits for the client to echo the close frame before it drops the connection.
const liveSessionCloseGrace = 2 * time.Second

// LiveSessionsWebSocket serves GET /v1/live/sessions, the public GPT-Live WebSocket API. The gateway is the WebRTC peer: client audio in the session format goes to the Opus track and reply audio comes back as session.output_audio.delta in the same format.
// Allow-live and bridge availability fail as HTTP errors before the upgrade.
func (h *Handler) LiveSessionsWebSocket(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	if !realtimeAudioBridgeSupported() {
		api.SendErrorWithStatus(c, audioBridgeUnavailableAPIError(), audioBridgeUnavailableStatus)
		return
	}
	releaseWS, ok := h.acquireDownstreamWebSocket(c)
	if !ok {
		return
	}
	defer releaseWS()
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, nil) {
			return
		}
	}
	conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		return
	}
	if outputGuardHTTPGateArmed() && !RequireOutputGuardReadyWS(conn) {
		return
	}
	conn.SetReadLimit(int64(security.MaxRequestBodySize))
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestCtx)
	stopDownstreamKeepalive := startDownstreamWSKeepalive(requestCtx, conn, cancel)
	defer func() {
		cancel()
		_ = conn.Close()
		stopDownstreamKeepalive()
	}()
	unregisterWriter := registerResponsesWSWriter(conn)
	defer unregisterWriter()

	s := &liveWSSession{h: h, c: c, conn: conn, ctx: requestCtx, cancel: cancel, end: make(chan string, 1), loopDone: make(chan struct{})}
	s.run()
}

// liveWSSession is one public GPT-Live WebSocket session. mu serializes the translator, the call sends and every write to the client, because the upstream pump, the client loop and the ticker all use them.
type liveWSSession struct {
	h      *Handler
	c      *gin.Context
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	closeSeen  bool // the client sent session.close; later client events are dropped
	closing    bool // set once session.closed is being written; nothing else may reach the client afterwards
	translator *liveSessionTranslator
	call       realtimeAudioCallConn
	decoder    *clientAudioDecoder
	encoder    *clientAudioEncoder
	input      *realtimeAudioInput
	// vad watches the client's input audio for speech start and stop. Only the client loop touches it. muted mirrors session.input_audio.mute, during which the audio is neither analysed nor sent.
	vad   *audioVAD
	muted bool

	// swapping is true from the moment a voice change starts until the events held meanwhile have been replayed. Client events that arrive in that time wait in swapQueue.
	swapping       bool
	swapQueue      [][]byte
	swapQueueBytes int

	end       chan string
	loopDone  chan struct{}
	endOnce   sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// finish records why the session ends; the first reason wins.
func (s *liveWSSession) finish(reason string) {
	s.endOnce.Do(func() { s.end <- reason })
}

// readFrame reads one client frame with the same memory admission as the text path. ok is false when the connection ended or the frame cannot be processed; skip is true for a frame that was answered with an error and carries no event.
func (s *liveWSSession) readFrame() (payload []byte, release func(), ok, skip bool) {
	messageType, reader, err := s.conn.NextReader()
	if err != nil {
		return nil, nil, false, false
	}
	payload, frameMemory, err := security.ReadRequestMemory(reader, int64(security.MaxRequestBodySize))
	if err != nil {
		if errors.Is(err, security.ErrRequestMemoryBudget) {
			_ = writeResponsesWSErrorWithin(s.conn, api.NewAPIError(api.ErrCodeServiceUnavailable, "Server request memory capacity is temporarily exhausted; retry later", api.ErrorTypeServer), responsesWSOverloadWriteTimeout)
			closeResponsesWSWithin(s.conn, websocket.CloseTryAgainLater, "request memory capacity exhausted", responsesWSOverloadWriteTimeout)
		}
		return nil, nil, false, false
	}
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		frameMemory.Release()
		_ = writeResponsesWSMessage(s.conn, liveErrorEvent("", "invalid_value", "Unsupported websocket message type.", "", ""))
		return nil, func() {}, true, true
	}
	return payload, frameMemory.Release, true, false
}

func (s *liveWSSession) run() {
	cfg, ok := s.readStart()
	if !ok {
		return
	}
	openStart := time.Now()
	call, gerr := openRealtimeAudioCall(s.h, s.c, cfg.UpstreamSession())
	if gerr != nil || call == nil {
		s.failOpen(gerr)
		return
	}
	input, err := newRealtimeAudioInput(call.WriteOpusFrame)
	if err != nil {
		call.Close()
		apiErr := api.NewAPIError(api.ErrCodeServerError, "Failed to start the audio input pipeline", api.ErrorTypeServer)
		code := websocket.CloseTryAgainLater
		if errors.Is(err, errAudioBridgeUnavailable) {
			apiErr = audioBridgeUnavailableAPIError()
		}
		_ = writeResponsesWSMessage(s.conn, liveErrorEvent("server_error", string(apiErr.Code), apiErr.Message, "", ""))
		closeResponsesWS(s.conn, code, "audio bridge unavailable")
		return
	}
	format := clientAudioFormat{Type: cfg.FormatType, Rate: cfg.FormatRate}
	s.translator = newLiveSessionTranslator(cfg)
	s.translator.AllowVoiceSwap()
	s.call, s.input = call, input
	s.decoder, s.encoder = newClientAudioDecoder(format), newClientAudioEncoder(format)
	s.vad = newAudioVAD()
	input.Start(s.ctx)

	// Whatever way the session ends, the input, the call and the goroutines are released.
	// A voice change may have replaced the call and the input, so the current ones are closed. The cancel comes first: a swap that is still opening a call checks it before it adopts the call and closes the call itself otherwise.
	defer func() {
		s.cancel()
		s.mu.Lock()
		input, call := s.input, s.call
		s.mu.Unlock()
		input.Close()
		call.Close()
		_ = s.conn.Close()
		s.wg.Wait()
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pumpUpstream(call)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(s.loopDone)
		s.clientLoop()
	}()

	ticker := time.NewTicker(liveSessionTickInterval)
	defer ticker.Stop()
	// The upstream call expires liveMaxSessionDuration after it was created, so the timer counts from before the open and fires a margin early: the client then sees expired, not remote_hangup.
	margin := liveMaxSessionDuration / 10
	if margin > liveSessionExpiryMargin {
		margin = liveSessionExpiryMargin
	}
	expiryIn := liveMaxSessionDuration - time.Since(openStart) - margin
	if expiryIn < 0 {
		expiryIn = 0
	}
	expiry := time.NewTimer(expiryIn)
	defer expiry.Stop()

	for {
		select {
		case reason := <-s.end:
			s.shutdown(reason)
			return
		case <-expiry.C:
			s.finish("expired")
		case <-s.ctx.Done():
			s.finish("connection_lost")
		case now := <-ticker.C:
			s.mu.Lock()
			events := s.translator.Tick(now)
			for _, event := range events {
				if !s.writeLocked(event) {
					break
				}
			}
			s.mu.Unlock()
		}
	}
}

// readStart reads and validates session.start. On any failure it answers the client, closes the socket and creates no call.
func (s *liveWSSession) readStart() (*liveSessionConfig, bool) {
	deadline := time.Now().Add(liveSessionStartTimeout)
	_ = s.conn.SetReadDeadline(deadline)
	for {
		payload, release, ok, skip := s.readFrame()
		if !ok {
			if !time.Now().Before(deadline) {
				_ = writeResponsesWSMessage(s.conn, liveErrorEvent("", "invalid_value", "session.start not received.", "", ""))
			}
			closeResponsesWS(s.conn, websocket.ClosePolicyViolation, "session.start is required")
			return nil, false
		}
		if skip {
			// An unsupported frame type was answered; the first message must still arrive in time.
			continue
		}
		cfg, perr := parseLiveSessionStart(payload)
		release()
		_ = s.conn.SetReadDeadline(time.Time{})
		if perr != nil {
			_ = writeResponsesWSMessage(s.conn, perr.Event())
			closeResponsesWS(s.conn, websocket.ClosePolicyViolation, "invalid session.start")
			return nil, false
		}
		return cfg, true
	}
}

// failOpen reports a gateway error from call creation as a public error event followed by a close.
func (s *liveWSSession) failOpen(gerr *liveGatewayError) {
	status := http.StatusInternalServerError
	code, message := "server_error", "The audio call could not be created."
	if gerr != nil {
		status = gerr.Status
		if gerr.Err != nil {
			code, message = string(gerr.Err.Code), gerr.Err.Message
		}
	}
	errType := "invalid_request_error"
	if status >= 500 {
		errType = "server_error"
	}
	_ = writeResponsesWSMessage(s.conn, liveErrorEvent(errType, code, message, "", ""))
	closeCode, reason := websocket.CloseInternalServerErr, "audio call failed"
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		closeCode, reason = websocket.CloseTryAgainLater, "audio call unavailable, try again later"
	default:
		if status >= 400 && status < 500 {
			closeCode, reason = websocket.ClosePolicyViolation, "audio call refused"
		}
	}
	closeResponsesWS(s.conn, closeCode, reason)
}

// writeLocked writes one event to the client; mu must be held. Once the session is closing nothing more is written. A write error ends the session as connection_lost.
func (s *liveWSSession) writeLocked(event []byte) bool {
	if s.closing {
		return false
	}
	if err := writeResponsesWSMessage(s.conn, event); err != nil {
		s.finish("connection_lost")
		return false
	}
	return true
}

// sendUpstreamLocked sends queued internal events to the Live sideband in order; mu must be held.
func (s *liveWSSession) sendUpstreamLocked(events [][]byte) bool {
	for _, event := range events {
		if err := s.call.Send(event); err != nil {
			s.finish("remote_hangup")
			return false
		}
	}
	return true
}

// shutdown writes exactly one session.closed (unless the connection is already lost) and the normal close frame, then waits briefly for the client to echo it.
func (s *liveWSSession) shutdown(reason string) {
	s.closeOnce.Do(func() {
		if reason != "connection_lost" {
			s.mu.Lock()
			s.closing = true
			closed := s.translator.Closed(reason)
			_ = writeResponsesWSMessage(s.conn, closed)
			s.mu.Unlock()
			closeResponsesWS(s.conn, websocket.CloseNormalClosure, reason)
			_ = s.conn.SetReadDeadline(time.Now().Add(liveSessionCloseGrace))
			select {
			case <-s.loopDone:
			case <-time.After(liveSessionCloseGrace):
			}
		}
	})
}

// clientLoop reads client events until the connection ends or the client closes the session.
func (s *liveWSSession) clientLoop() {
	for s.ctx.Err() == nil {
		payload, release, ok, skip := s.readFrame()
		if !ok {
			s.finish("connection_lost")
			return
		}
		if skip {
			continue
		}
		stop := s.handleClientEvent(payload)
		release()
		if stop {
			return
		}
	}
}

// handleClientEvent applies one public client event; it returns true when the loop should stop (the connection is unusable). While a voice change is opening the new call the event waits in swapQueue, and is replayed on the call that wins. session.close does not wait.
func (s *liveWSSession) handleClientEvent(raw []byte) bool {
	s.mu.Lock()
	if s.swapping && !s.closing && !s.closeSeen && gjson.GetBytes(raw, "type").String() != "session.close" {
		if len(s.swapQueue) >= liveSwapQueueMaxEvents || s.swapQueueBytes+len(raw) > liveSwapQueueMaxBytes {
			ok := s.writeLocked(liveErrorEvent("", "invalid_value", "Too many events while the voice is changing; this one was dropped.", "", liveEventID(gjson.ParseBytes(raw))))
			s.mu.Unlock()
			return !ok
		}
		// The frame memory is released when this call returns, so the event is copied.
		s.swapQueue = append(s.swapQueue, append([]byte(nil), raw...))
		s.swapQueueBytes += len(raw)
		s.mu.Unlock()
		if liveSwapQueuedForTest != nil {
			liveSwapQueuedForTest()
		}
		return false
	}
	s.mu.Unlock()
	stop, swap := s.processClientEvent(raw)
	if swap != nil && !stop {
		s.startSwap(swap)
	}
	return stop
}

// processClientEvent translates one public client event and applies the result. It returns a voice change when the event asked for one.
func (s *liveWSSession) processClientEvent(raw []byte) (stop bool, swap *liveVoiceChange) {
	s.mu.Lock()
	if s.closing || s.closeSeen {
		s.mu.Unlock()
		return false, nil
	}
	res := s.translator.Client(raw)
	if res.Close {
		s.closeSeen = true
	}
	var speech [][]byte
	switch gjson.GetBytes(raw, "type").String() {
	case "session.input_audio.mute":
		s.muted = true
		// Audio stops here, so an open speech segment ends now.
		if ev, ok := s.vad.Reset(); ok {
			speech = s.translator.Speech([]audioSpeechEvent{ev})
		}
	case "session.input_audio.unmute":
		s.muted = false
	}
	res.Client = append(res.Client, speech...)
	for _, event := range res.Client {
		if !s.writeLocked(event) {
			s.mu.Unlock()
			return true, nil
		}
	}
	if !s.sendUpstreamLocked(res.Upstream) {
		s.mu.Unlock()
		return true, nil
	}
	input := s.input
	s.mu.Unlock()

	if len(res.Audio) > 0 {
		pcm := s.decoder.Decode(res.Audio)
		if len(pcm) > 0 {
			if err := input.AppendPCM16(pcm); err != nil {
				s.mu.Lock()
				s.writeLocked(liveErrorEvent("", "invalid_value", "The audio could not be processed: "+err.Error(), "audio", ""))
				s.mu.Unlock()
			} else if !s.muted {
				if events := s.translator.Speech(s.vad.Process(pcm)); len(events) > 0 {
					s.mu.Lock()
					for _, event := range events {
						if !s.writeLocked(event) {
							break
						}
					}
					s.mu.Unlock()
				}
			}
		}
	}
	if res.Close {
		// The loop keeps reading so the client's close echo is consumed; shutdown bounds the wait.
		s.finish("close_requested")
	}
	return false, res.Swap
}

// startSwap begins a voice change: from now on client events wait in swapQueue, and a goroutine opens the new call. The old call keeps serving, and its reply in progress finishes, until the new call has started.
func (s *liveWSSession) startSwap(change *liveVoiceChange) {
	s.mu.Lock()
	s.swapping = true
	next, since := s.translator.SwapConfig(change)
	s.mu.Unlock()
	started := time.Now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runSwap(change, next, since, started)
	}()
}

// runSwap opens the call with the new voice, switches the session to it and closes the old one. When the new call cannot be opened the old call stays and the client gets voice_change_failed.
func (s *liveWSSession) runSwap(change *liveVoiceChange, next *liveSessionConfig, since int, started time.Time) {
	call, input, reason := s.openSwapCall(next)
	if call == nil {
		if s.ctx.Err() == nil {
			s.mu.Lock()
			s.writeLocked(liveVoiceChangeFailedEvent(reason+" The voice stays as it was.", change.ClientEventID))
			s.mu.Unlock()
		}
		s.drainSwapQueue()
		return
	}

	s.mu.Lock()
	if s.closing || s.closeSeen || s.ctx.Err() != nil {
		s.mu.Unlock()
		input.Close()
		call.Close()
		return
	}
	out := s.translator.Resume(next, since, change, time.Since(started))
	oldCall, oldInput := s.call, s.input
	s.call, s.input = call, input
	ok := true
	for _, event := range out.Client {
		if !s.writeLocked(event) {
			ok = false
			break
		}
	}
	if ok {
		s.sendUpstreamLocked(out.Upstream)
	}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pumpUpstream(call)
	}()
	oldInput.Close()
	oldCall.Close()
	s.drainSwapQueue()
}

// openSwapCall opens the replacement call and waits for its session.started. On failure the reason says why and no call stays open.
func (s *liveWSSession) openSwapCall(next *liveSessionConfig) (realtimeAudioCallConn, *realtimeAudioInput, string) {
	call, gerr := openRealtimeAudioCall(s.h, s.c, next.UpstreamSession())
	if gerr != nil || call == nil {
		reason := "The new call could not be opened."
		if gerr != nil && gerr.Err != nil && gerr.Err.Message != "" {
			reason = "The new call could not be opened: " + gerr.Err.Message + "."
		}
		return nil, nil, reason
	}
	input, err := newRealtimeAudioInput(call.WriteOpusFrame)
	if err != nil {
		call.Close()
		return nil, nil, "The audio input for the new call could not be started."
	}
	timer := time.NewTimer(liveSwapStartTimeout)
	defer timer.Stop()
	events := call.Events()
	for {
		select {
		case raw, ok := <-events:
			if !ok {
				input.Close()
				call.Close()
				return nil, nil, "The new call ended before it started."
			}
			switch gjson.GetBytes(raw, "type").String() {
			case "session.started":
				input.Start(s.ctx)
				return call, input, ""
			case "error":
				message := gjson.GetBytes(raw, "error.message").String()
				input.Close()
				call.Close()
				return nil, nil, "The new call was refused: " + message
			}
		case <-timer.C:
			input.Close()
			call.Close()
			return nil, nil, "The new call did not start in time."
		case <-s.ctx.Done():
			input.Close()
			call.Close()
			return nil, nil, ""
		}
	}
}

// drainSwapQueue replays the client events held during a voice change, in order, on the current call. A replayed event may ask for another voice change; then the rest stays queued behind it.
func (s *liveWSSession) drainSwapQueue() {
	for {
		s.mu.Lock()
		if len(s.swapQueue) == 0 || s.closing || s.closeSeen || s.ctx.Err() != nil {
			s.swapQueue, s.swapQueueBytes, s.swapping = nil, 0, false
			s.mu.Unlock()
			return
		}
		raw := s.swapQueue[0]
		s.swapQueue = s.swapQueue[1:]
		s.swapQueueBytes -= len(raw)
		s.mu.Unlock()
		stop, swap := s.processClientEvent(raw)
		if stop {
			return
		}
		if swap != nil {
			s.startSwap(swap)
			return
		}
	}
}

// pumpUpstream maps every Live sideband event to public events. When the events channel closes or the call is done, the session ends as remote_hangup; events still buffered at that point are written first.
func (s *liveWSSession) pumpUpstream(call realtimeAudioCallConn) {
	events := call.Events()
	done := call.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case raw, ok := <-events:
			if !ok {
				s.callEnded(call)
				return
			}
			if !s.forwardUpstream(call, raw) {
				return
			}
		case <-done:
			for {
				select {
				case raw, ok := <-events:
					if !ok {
						s.callEnded(call)
						return
					}
					if !s.forwardUpstream(call, raw) {
						return
					}
					continue
				default:
				}
				break
			}
			s.callEnded(call)
			return
		}
	}
}

// callEnded ends the session as remote_hangup, unless the call is one that a voice change replaced.
func (s *liveWSSession) callEnded(call realtimeAudioCallConn) {
	s.mu.Lock()
	current := s.call == call
	s.mu.Unlock()
	if current {
		s.finish("remote_hangup")
	}
}

// forwardUpstream translates one sideband event of call and writes the result. It returns false once the session cannot continue or call is no longer the session's call.
func (s *liveWSSession) forwardUpstream(call realtimeAudioCallConn, raw []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.call != call {
		return false
	}
	res := s.translator.Upstream(raw)
	if !s.sendUpstreamLocked(res.Upstream) {
		return false
	}
	for _, event := range res.Client {
		if !s.writeLocked(event) {
			return false
		}
	}
	if len(res.AudioOut) > 0 {
		if out := s.encoder.Encode(res.AudioOut); len(out) > 0 {
			delta, _ := json.Marshal(map[string]string{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(out)})
			if !s.writeLocked(delta) {
				return false
			}
		}
	}
	return true
}
