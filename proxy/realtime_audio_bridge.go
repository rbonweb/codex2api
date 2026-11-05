package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// realtimeAudioCallConn is the part of audioCall the bridge uses; tests replace the call with a fake.
type realtimeAudioCallConn interface {
	WriteOpusFrame(frame []byte, d time.Duration) error
	Events() <-chan []byte
	Send(event []byte) error
	Done() <-chan struct{}
	Close()
	CallID() string
}

// openRealtimeAudioCall opens the ChatGPT Live call behind an audio session; tests replace it.
var openRealtimeAudioCall = func(h *Handler, c *gin.Context, session json.RawMessage) (realtimeAudioCallConn, *liveGatewayError) {
	call, err := h.openAudioCall(c, session)
	if err != nil {
		return nil, err
	}
	return call, nil
}

// realtimeAudioBridgeSupported reports whether this binary can run audio sessions; tests replace it so the bridge runs on the stub build.
var realtimeAudioBridgeSupported = audioBridgeSupported

// realtimeAudioEndReason is the close reason sent to the client when the Live call ends first.
const realtimeAudioEndReason = "live call ended"

// realtimeAudioCloseGrace is how long the bridge waits for the client to echo the close frame before it drops the read.
const realtimeAudioCloseGrace = 2 * time.Second

// realtimeAudioAllowed reports whether the API key may use ChatGPT Live.
func realtimeAudioAllowed(c *gin.Context) bool {
	row := apiKeyRowFromContext(c)
	return row != nil && row.Limits.AllowLive
}

func realtimeAudioNotAllowedError() *api.APIError {
	return api.NewAPIError(api.ErrCodeInsufficientScope, "ChatGPT Live is not enabled for this API key", api.ErrorTypePermission)
}

// serveRealtimeAudio serves /v1/realtime?model=<audio model>. Allow-live and bridge availability fail as HTTP errors before the upgrade; the rest of the setup mirrors the text path.
func (h *Handler) serveRealtimeAudio(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	if !realtimeAudioBridgeSupported() {
		api.SendErrorWithStatus(c, audioBridgeUnavailableAPIError(), audioBridgeUnavailableStatus)
		return
	}
	audioCfg, queryErr := realtimeAudioConfigFromQuery(c)
	if queryErr != nil {
		api.SendErrorWithStatus(c, queryErr, http.StatusBadRequest)
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

	h.runRealtimeAudioSession(c, conn, requestCtx, cancel, audioCfg, nil)
}

// realtimeAudioMaxQueryInstructions caps instructions given as a query parameter at the size a client frame may have, the same bound a session.update carries.
var realtimeAudioMaxQueryInstructions = security.MaxRequestBodySize

// realtimeAudioConfigFromQuery reads the initial session from the WebSocket URL: model, instructions, voice and delegation ("client", or "none" for client delegation without Live's filler acknowledgement). A later session.update before the call exists overrides them. An unsupported delegation or oversized instructions fail before the upgrade.
func realtimeAudioConfigFromQuery(c *gin.Context) (realtimeAudioConfig, *api.APIError) {
	cfg := realtimeAudioConfig{
		Model:        strings.TrimSpace(c.Query("model")),
		Voice:        strings.TrimSpace(c.Query("voice")),
		Instructions: c.Query("instructions"),
	}
	if len(cfg.Instructions) > realtimeAudioMaxQueryInstructions {
		return cfg, api.NewAPIError(api.ErrorCode("invalid_value"), "The instructions query parameter is too long.", api.ErrorTypeInvalidRequest)
	}
	switch delegation := strings.TrimSpace(c.Query("delegation")); delegation {
	case "", "client":
	case "none":
		cfg.DelegationNoAck = true
	default:
		return cfg, api.NewAPIError(api.ErrorCode("unsupported_value"), "The delegation query parameter must be client or none.", api.ErrorTypeInvalidRequest)
	}
	return cfg, nil
}

// runRealtimeAudioHandoff moves a text session that asked for audio output onto the audio bridge. The upgrade, keepalive and writer registration already exist; allow-live and bridge availability fail as WebSocket error events followed by a close.
func (h *Handler) runRealtimeAudioHandoff(c *gin.Context, conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, cfg realtimeAudioConfig, ack []byte) {
	if !realtimeAudioAllowed(c) {
		_ = writeResponsesWSError(conn, realtimeAudioNotAllowedError())
		closeResponsesWS(conn, websocket.ClosePolicyViolation, "ChatGPT Live is not enabled for this API key")
		return
	}
	if !realtimeAudioBridgeSupported() {
		_ = writeResponsesWSError(conn, audioBridgeUnavailableAPIError())
		closeResponsesWS(conn, websocket.CloseTryAgainLater, "audio bridge unavailable")
		return
	}
	h.runRealtimeAudioSession(c, conn, ctx, cancel, cfg, ack)
}

// realtimeAudioHandoff decides whether a text-session client event hands the connection to the audio bridge: a session.update whose modalities contain audio. Only the model and instructions carry over from the text session; its tools, tool_choice, queued conversation items and history are dropped, because a ChatGPT Live call has no equivalent and the audio session starts a fresh conversation. ok is true when it does; apiErr is set when the update itself is invalid, and the connection then stays on the text path. The returned ack is the session.updated event for the audio session.
func realtimeAudioHandoff(state *realtimeTextSession, raw []byte) (cfg realtimeAudioConfig, ack []byte, apiErr *api.APIError, ok bool) {
	if gjson.GetBytes(raw, "type").String() != "session.update" {
		return cfg, nil, nil, false
	}
	session := gjson.GetBytes(raw, "session")
	if !session.IsObject() || !(realtimeModalitiesContainAudio(session.Get("output_modalities")) || realtimeModalitiesContainAudio(session.Get("modalities"))) {
		return cfg, nil, nil, false
	}
	cfg = realtimeAudioConfig{Model: mapRealtimeModelToLive(state.Model), Instructions: state.Instructions}
	_, ack, apiErr = applyRealtimeAudioSessionUpdate(&cfg, raw)
	return cfg, ack, apiErr, true
}

// realtimeAudioBridge is the state of one audio session. The Live call is created lazily by ensureCall. The mapper and the config are touched by the upstream pump and the client loop, so mu guards both.
type realtimeAudioBridge struct {
	h      *Handler
	c      *gin.Context
	ctx    context.Context
	conn   *websocket.Conn
	cancel context.CancelFunc

	mu     sync.Mutex
	mapper *realtimeAudioMapper
	cfg    realtimeAudioConfig

	// startMu serializes call creation (it is held for the whole upstream open); stateMu guards the fields below and is never held across I/O.
	startMu sync.Mutex
	stateMu sync.RWMutex
	call    realtimeAudioCallConn
	input   *realtimeAudioInput
	closed  bool
	failed  bool

	// eventID is the event_id of the client event being handled, echoed as client_event_id on its errors. Only the client loop touches it.
	eventID string
}

// runRealtimeAudioSession runs one audio session on an upgraded connection until the client leaves or the Live call ends. A nil ack announces the session with session.created; the handoff passes the session.updated ack instead, because the client already saw a text session.created. Either way the Live call is created lazily, on the first audio append, conversation.item.create or response.create: ChatGPT Live only takes instructions, voice and delegation at call creation, so every session.update before that just edits the pending config, and a quiet session never opens a call that could speak first.
func (h *Handler) runRealtimeAudioSession(c *gin.Context, conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, cfg realtimeAudioConfig, ack []byte) {
	b := &realtimeAudioBridge{
		h:      h,
		c:      c,
		ctx:    ctx,
		conn:   conn,
		cancel: cancel,
		mapper: newRealtimeAudioMapper(nil),
		cfg:    cfg,
	}
	defer b.closeAll()

	first := ack
	if len(first) == 0 {
		first = marshalRealtimeServerEvent(map[string]any{
			"type":     "session.created",
			"event_id": defaultRealtimeAudioID("event"),
			"session":  realtimeAudioSessionObject(&cfg, defaultRealtimeAudioID("sess")),
		})
	}
	if err := writeResponsesWSMessage(conn, first); err != nil {
		return
	}
	b.clientLoop(ctx)
}

// closeAll releases the input pipeline and the call. It waits for a call creation in flight, so a call that finishes opening after the client left is still closed.
func (b *realtimeAudioBridge) closeAll() {
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.stateMu.Lock()
	b.closed = true
	call, input := b.call, b.input
	b.stateMu.Unlock()
	if input != nil {
		input.Close()
	}
	if call != nil {
		call.Close()
	}
}

func (b *realtimeAudioBridge) liveInput() *realtimeAudioInput {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.input
}

func (b *realtimeAudioBridge) liveCall() realtimeAudioCallConn {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.call
}

// ensureCall creates the Live call from the current config unless it exists, and starts the input pipeline and the upstream pump. Audio the client sends meanwhile waits in the connection (the client loop is the caller, or waits on startMu), so it reaches the track in order once the call is up. On failure it sends an error event and closes the client; it returns false once the session cannot continue.
func (b *realtimeAudioBridge) ensureCall() bool {
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.stateMu.RLock()
	exists, dead := b.call != nil, b.closed || b.failed
	b.stateMu.RUnlock()
	if exists {
		return true
	}
	if dead || b.ctx.Err() != nil {
		return false
	}
	b.mu.Lock()
	cfg := b.cfg
	b.mu.Unlock()

	session := buildLiveAudioSession(cfg.Model, cfg.Voice, cfg.Instructions)
	if cfg.DelegationNoAck {
		// Client delegation without Live's spoken filler acknowledgement.
		if withoutAck, err := sjson.SetBytes(session, "delegation.ack_filler", false); err == nil {
			session = withoutAck
		}
	}
	call, gerr := openRealtimeAudioCall(b.h, b.c, session)
	if gerr != nil || call == nil {
		apiErr := (*api.APIError)(nil)
		status := http.StatusInternalServerError
		if gerr != nil {
			apiErr, status = gerr.Err, gerr.Status
		}
		code, reason := websocket.CloseInternalServerErr, "audio call failed"
		switch status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			code, reason = websocket.ClosePolicyViolation, "audio call refused"
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			code, reason = websocket.CloseTryAgainLater, "audio call unavailable, try again later"
		}
		b.fail(apiErr, code, reason)
		return false
	}
	input, err := newRealtimeAudioInput(call.WriteOpusFrame)
	if err != nil {
		call.Close()
		apiErr := api.NewAPIError(api.ErrCodeServerError, "Failed to start the audio input pipeline", api.ErrorTypeServer)
		if errors.Is(err, errAudioBridgeUnavailable) {
			apiErr = audioBridgeUnavailableAPIError()
		}
		b.fail(apiErr, websocket.CloseTryAgainLater, "audio bridge unavailable")
		return false
	}
	input.Start(b.ctx)

	b.stateMu.Lock()
	b.call, b.input = call, input
	b.stateMu.Unlock()
	go b.pumpUpstream(b.ctx, call)
	return true
}

// fail reports a call creation failure to the client, closes it, and ends the session; the short read deadline unblocks a client loop that is waiting for the next frame.
func (b *realtimeAudioBridge) fail(apiErr *api.APIError, code int, reason string) {
	b.stateMu.Lock()
	b.failed = true
	b.stateMu.Unlock()
	_ = writeResponsesWSError(b.conn, apiErr)
	closeResponsesWS(b.conn, code, reason)
	b.cancel()
	_ = b.conn.SetReadDeadline(time.Now().Add(realtimeAudioCloseGrace))
}

// pumpUpstream maps every Live sideband event to OpenAI events for the client, and closes the client when the call ends. Events still buffered when the call ends are written first, so the final turn.done or error is not lost.
func (b *realtimeAudioBridge) pumpUpstream(ctx context.Context, call realtimeAudioCallConn) {
	events := call.Events()
	done := call.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-events:
			if !ok {
				b.endClient()
				return
			}
			if !b.forwardUpstream(raw) {
				return
			}
		case <-done:
			// The call is over, but events it queued before closing may still be buffered: drain them without blocking, then close the client.
			for {
				select {
				case raw, ok := <-events:
					if !ok {
						b.endClient()
						return
					}
					if !b.forwardUpstream(raw) {
						return
					}
					continue
				default:
				}
				break
			}
			b.endClient()
			return
		}
	}
}

// forwardUpstream maps one Live event and writes the result to the client. It returns false after a write error, having closed the client connection so the client loop exits and the call is released promptly.
func (b *realtimeAudioBridge) forwardUpstream(raw []byte) bool {
	b.mu.Lock()
	mapped := b.mapper.MapUpstream(raw)
	textOnly := b.cfg.TextOnly
	outRate := b.cfg.outputRate()
	b.mu.Unlock()
	for _, event := range mapped {
		if outRate != realtimeAudioNativeRate && gjson.GetBytes(event, "type").String() == "response.output_audio.delta" {
			event = realtimeAudioResampleDelta(event, outRate)
		}
		if textOnly {
			var keep bool
			if event, keep = realtimeAudioTextOnlyEvent(event); !keep {
				continue
			}
		}
		if err := writeResponsesWSMessage(b.conn, event); err != nil {
			b.cancel()
			_ = b.conn.Close()
			return false
		}
	}
	return true
}

// endClient tells the client the Live call is over with a normal close, then bounds how long the read loop waits for the client's reply so the session unwinds.
func (b *realtimeAudioBridge) endClient() {
	closeResponsesWS(b.conn, websocket.CloseNormalClosure, realtimeAudioEndReason)
	_ = b.conn.SetReadDeadline(time.Now().Add(realtimeAudioCloseGrace))
}

// realtimeAudioTextOnlyEvent adapts a mapped event for a text-only session (output_modalities ["text"]): audio deltas and the audio done event are dropped, the assistant transcript events become response.output_text.delta and response.output_text.done (done carries text instead of transcript), and the content parts that describe the assistant message become text parts so the stream stays consistent.
func realtimeAudioTextOnlyEvent(event []byte) ([]byte, bool) {
	switch gjson.GetBytes(event, "type").String() {
	case "response.output_audio.delta", "response.output_audio.done":
		return nil, false
	case "response.output_audio_transcript.delta":
		renamed, err := sjson.SetBytes(event, "type", "response.output_text.delta")
		return renamed, err == nil
	case "response.output_audio_transcript.done":
		renamed, err := sjson.SetBytes(event, "type", "response.output_text.done")
		if err != nil {
			return nil, false
		}
		return realtimeAudioMoveTranscriptToText(renamed, "")
	case "response.content_part.added", "response.content_part.done":
		return realtimeAudioMoveTranscriptToText(event, "part.")
	case "response.output_item.done":
		return realtimeAudioTextItemContent(event, "item.content")
	case "response.done":
		return realtimeAudioTextItemContent(event, "response.output.0.content")
	}
	return event, true
}

// realtimeAudioMoveTranscriptToText turns the transcript field under prefix into a text field; a part gets type "text".
func realtimeAudioMoveTranscriptToText(event []byte, prefix string) ([]byte, bool) {
	transcript := gjson.GetBytes(event, prefix+"transcript")
	if !transcript.Exists() {
		return event, true
	}
	out, err := sjson.DeleteBytes(event, prefix+"transcript")
	if err != nil {
		return nil, false
	}
	if out, err = sjson.SetBytes(out, prefix+"text", transcript.String()); err != nil {
		return nil, false
	}
	if prefix != "" {
		if out, err = sjson.SetBytes(out, prefix+"type", "text"); err != nil {
			return nil, false
		}
	}
	return out, true
}

// realtimeAudioTextItemContent rewrites output_audio content parts at path into output_text parts carrying text. Paths that are absent leave the event unchanged.
func realtimeAudioTextItemContent(event []byte, path string) ([]byte, bool) {
	content := gjson.GetBytes(event, path)
	if !content.IsArray() {
		return event, true
	}
	out := event
	for i, part := range content.Array() {
		if part.Get("type").String() != "output_audio" {
			continue
		}
		var err error
		if out, err = sjson.SetBytes(out, path+"."+strconv.Itoa(i)+".type", "output_text"); err != nil {
			return nil, false
		}
		if out, err = sjson.SetBytes(out, path+"."+strconv.Itoa(i)+".text", part.Get("transcript").String()); err != nil {
			return nil, false
		}
		if out, err = sjson.DeleteBytes(out, path+"."+strconv.Itoa(i)+".transcript"); err != nil {
			return nil, false
		}
	}
	return out, true
}

// clientLoop reads client events until the connection ends. It uses the same frame reading and memory admission as the text path.
func (b *realtimeAudioBridge) clientLoop(ctx context.Context) {
	rejectMemory := func() {
		_ = writeResponsesWSErrorWithin(b.conn, api.NewAPIError(api.ErrCodeServiceUnavailable, "Server request memory capacity is temporarily exhausted; retry later", api.ErrorTypeServer), responsesWSOverloadWriteTimeout)
		closeResponsesWSWithin(b.conn, websocket.CloseTryAgainLater, "request memory capacity exhausted", responsesWSOverloadWriteTimeout)
	}
	for ctx.Err() == nil {
		messageType, reader, err := b.conn.NextReader()
		if err != nil {
			return
		}
		payload, frameMemory, err := security.ReadRequestMemory(reader, int64(security.MaxRequestBodySize))
		if err != nil {
			if errors.Is(err, security.ErrRequestMemoryBudget) {
				rejectMemory()
			}
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			frameMemory.Release()
			b.writeError(api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported websocket message type", api.ErrorTypeInvalidRequest))
			continue
		}
		payload, _ = stripNewAPIPolicyWebSocketEventID(payload)
		b.handleClientEvent(payload)
		frameMemory.Release()
	}
}

// writeError sends an error event for the client event being handled. An error that names a session field goes out in the public Realtime shape with code, param and client_event_id.
func (b *realtimeAudioBridge) writeError(apiErr *api.APIError) {
	if apiErr != nil {
		if detail, ok := apiErr.Details.(realtimeAudioParamError); ok {
			b.writeParamError(string(apiErr.Code), detail.Param, apiErr.Message)
			return
		}
	}
	_ = writeResponsesWSError(b.conn, apiErr)
}

// writeParamError sends {"type":"error","error":{type,code,message,param,client_event_id}}.
func (b *realtimeAudioBridge) writeParamError(code, param, message string) {
	var paramValue, eventValue any
	if param != "" {
		paramValue = param
	}
	if b.eventID != "" {
		eventValue = b.eventID
	}
	b.writeEvent(marshalRealtimeServerEvent(map[string]any{
		"type":     "error",
		"event_id": defaultRealtimeAudioID("event"),
		"error": map[string]any{
			"type":            "invalid_request_error",
			"code":            code,
			"message":         message,
			"param":           paramValue,
			"client_event_id": eventValue,
		},
	}))
}

// realtimeAudioResampleDelta converts the base64 PCM16 24 kHz delta of an output audio event to the client's output rate. An undecodable delta is passed through unchanged.
func realtimeAudioResampleDelta(event []byte, toRate int) []byte {
	pcm, err := base64.StdEncoding.DecodeString(gjson.GetBytes(event, "delta").String())
	if err != nil {
		return event
	}
	samples, err := decodePCM16LE(pcm)
	if err != nil {
		return event
	}
	out, err := sjson.SetBytes(event, "delta", base64.StdEncoding.EncodeToString(encodePCM16LE(resamplePCM16(samples, realtimeAudioNativeRate, toRate))))
	if err != nil {
		return event
	}
	return out
}

// appendClientAudio decodes one input_audio_buffer.append payload, resamples it to 24 kHz when the client sends 16 kHz, and feeds the input pipeline.
func (b *realtimeAudioBridge) appendClientAudio(input *realtimeAudioInput, b64 string) error {
	b.mu.Lock()
	rate := b.cfg.inputRate()
	b.mu.Unlock()
	if rate == realtimeAudioNativeRate {
		return input.AppendBase64(b64)
	}
	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	samples, err := decodePCM16LE(pcm)
	if err != nil {
		return err
	}
	return input.AppendPCM16(encodePCM16LE(resamplePCM16(samples, rate, realtimeAudioNativeRate)))
}

func (b *realtimeAudioBridge) writeEvent(event []byte) {
	_ = writeResponsesWSMessage(b.conn, event)
}

// sendUpstream creates the call if needed and writes one event to the Live sideband.
func (b *realtimeAudioBridge) sendUpstream(event []byte) bool {
	if !b.ensureCall() {
		return false
	}
	if err := b.liveCall().Send(event); err != nil {
		b.writeError(api.NewAPIError(api.ErrCodeServiceUnavailable, "The audio call is no longer available", api.ErrorTypeServer))
		return false
	}
	return true
}

func invalidAudioRequest(msg string) *api.APIError {
	return api.NewAPIError(api.ErrCodeInvalidRequest, msg, api.ErrorTypeInvalidRequest)
}

// handleClientEvent dispatches one OpenAI Realtime client event.
func (b *realtimeAudioBridge) handleClientEvent(raw []byte) {
	if !gjson.ValidBytes(raw) {
		b.writeError(invalidAudioRequest("invalid realtime websocket request payload"))
		return
	}
	eventType := strings.TrimSpace(gjson.GetBytes(raw, "type").String())
	b.eventID = gjson.GetBytes(raw, "event_id").String()
	switch eventType {
	case "session.update":
		b.handleSessionUpdate(raw)

	case "input_audio_buffer.append":
		if !b.ensureCall() {
			return
		}
		if err := b.appendClientAudio(b.liveInput(), gjson.GetBytes(raw, "audio").String()); err != nil {
			b.writeError(invalidAudioRequest("input_audio_buffer.append failed: " + err.Error()))
		}

	case "input_audio_buffer.commit":
		// Before the call exists there is no audio to flush, so a commit does not create it.
		if input := b.liveInput(); input != nil {
			if err := input.Commit(); err != nil {
				b.writeError(invalidAudioRequest("input_audio_buffer.commit failed: " + err.Error()))
				return
			}
		}
		// The upstream runs its own turn detection, so a commit only flushes the partial frame; the item id it reports is the one the next user turn will use.
		b.mu.Lock()
		itemID := b.mapper.UserItemID()
		previous := b.mapper.LastItemID()
		b.mu.Unlock()
		var previousID any
		if previous != "" {
			previousID = previous
		}
		b.writeEvent(marshalRealtimeServerEvent(map[string]any{
			"type":             "input_audio_buffer.committed",
			"event_id":         defaultRealtimeAudioID("event"),
			"previous_item_id": previousID,
			"item_id":          itemID,
		}))

	case "input_audio_buffer.clear":
		if input := b.liveInput(); input != nil {
			input.Clear()
		}
		b.writeEvent(marshalRealtimeServerEvent(map[string]any{
			"type":     "input_audio_buffer.cleared",
			"event_id": defaultRealtimeAudioID("event"),
		}))

	case "conversation.item.create":
		sideband, ack, apiErr := realtimeTextItemToContextAppend(raw)
		if apiErr != nil {
			b.writeError(apiErr)
			return
		}
		if len(sideband) > 0 && !b.sendUpstream(sideband) {
			return
		}
		b.writeEvent(ack)

	case "response.create":
		// Upstream rejects parameters on response.create, so only the bare event is forwarded. It is forwarded even when turn_detection.create_response is false, because the client asked for it; the bridge itself never sends response.create, and Live still decides when to speak on its own turn detection.
		b.sendUpstream([]byte(`{"type":"response.create"}`))

	case "response.cancel":
		// Documented no-op: Live interrupts on its own turn detection, and there is nothing to cancel on the gateway side.

	case "ping":
		b.writeEvent(marshalRealtimeServerEvent(map[string]any{"type": "pong"}))

	default:
		b.writeError(invalidAudioRequest("unsupported realtime websocket request type: " + eventType))
	}
}

// handleSessionUpdate applies a session.update. Before the call exists it only records the config and acks; it never creates the call, so the first audio, item or response.create creates it from the latest config, which is how instructions, voice and delegation reach ChatGPT Live. Once the call exists, instructions and voice cannot change: they are not forwarded, the session.updated ack reflects the accepted fields, and each refused field gets an immutable_field_update error event while the session continues.
func (b *realtimeAudioBridge) handleSessionUpdate(raw []byte) {
	// startMu makes "has the call been created" and the config change one step: an update that races a creating trigger either lands in the creation session or is refused, never silently lost.
	b.startMu.Lock()
	b.stateMu.RLock()
	created := b.call != nil
	b.stateMu.RUnlock()

	b.mu.Lock()
	next := b.cfg
	_, ack, apiErr := applyRealtimeAudioSessionUpdate(&next, raw)
	var refused []string
	if apiErr == nil && created {
		if realtimeAudioInstructionsOrDefault(next.Instructions) != realtimeAudioInstructionsOrDefault(b.cfg.Instructions) {
			refused = append(refused, "Instructions")
			next.Instructions = b.cfg.Instructions
		}
		if mapRealtimeVoiceToLive(next.Voice) != mapRealtimeVoiceToLive(b.cfg.Voice) {
			refused = append(refused, "Voice")
			next.Voice = b.cfg.Voice
		}
		if len(refused) > 0 {
			ack = marshalRealtimeServerEvent(map[string]any{
				"type":     "session.updated",
				"event_id": defaultRealtimeAudioID("event"),
				"session":  realtimeAudioSessionObject(&next, ""),
			})
		}
	}
	if apiErr == nil {
		b.cfg = next
	}
	b.mu.Unlock()
	b.startMu.Unlock()

	if apiErr != nil {
		b.writeError(apiErr)
		return
	}
	b.writeEvent(ack)
	for _, field := range refused {
		param := "session.instructions"
		if field == "Voice" {
			param = "session.audio.output.voice"
		}
		b.writeParamError("immutable_field_update", param, field+" cannot be updated after initialization.")
	}
}
