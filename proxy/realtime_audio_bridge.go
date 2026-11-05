package proxy

import (
	"context"
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

	cfg := realtimeAudioConfig{Model: strings.TrimSpace(c.Query("model"))}
	h.runRealtimeAudioSession(c, conn, requestCtx, cancel, cfg, nil)
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

// realtimeAudioBridge is the state of one audio session. The mapper and the config are touched by the upstream pump and the client loop, so mu guards both.
type realtimeAudioBridge struct {
	conn   *websocket.Conn
	call   realtimeAudioCallConn
	input  *realtimeAudioInput
	cancel context.CancelFunc

	mu     sync.Mutex
	mapper *realtimeAudioMapper
	cfg    realtimeAudioConfig
}

// runRealtimeAudioSession runs one audio session on an upgraded connection until the client leaves or the Live call ends. A nil ack announces the session with session.created; the handoff passes the session.updated ack instead because the client already saw a text session.created.
func (h *Handler) runRealtimeAudioSession(c *gin.Context, conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, cfg realtimeAudioConfig, ack []byte) {
	call, gerr := openRealtimeAudioCall(h, c, buildLiveAudioSession(cfg.Model, cfg.Voice, cfg.Instructions))
	if gerr != nil || call == nil {
		apiErr := (*api.APIError)(nil)
		status := http.StatusInternalServerError
		if gerr != nil {
			apiErr, status = gerr.Err, gerr.Status
		}
		_ = writeResponsesWSError(conn, apiErr)
		code, reason := websocket.CloseInternalServerErr, "audio call failed"
		switch status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			code, reason = websocket.ClosePolicyViolation, "audio call refused"
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			code, reason = websocket.CloseTryAgainLater, "audio call unavailable, try again later"
		}
		closeResponsesWS(conn, code, reason)
		return
	}
	defer call.Close()

	input, err := newRealtimeAudioInput(call.WriteOpusFrame)
	if err != nil {
		apiErr := api.NewAPIError(api.ErrCodeServerError, "Failed to start the audio input pipeline", api.ErrorTypeServer)
		if errors.Is(err, errAudioBridgeUnavailable) {
			apiErr = audioBridgeUnavailableAPIError()
		}
		_ = writeResponsesWSError(conn, apiErr)
		closeResponsesWS(conn, websocket.CloseTryAgainLater, "audio bridge unavailable")
		return
	}
	defer input.Close()
	input.Start(ctx)

	b := &realtimeAudioBridge{
		conn:   conn,
		call:   call,
		input:  input,
		cancel: cancel,
		mapper: newRealtimeAudioMapper(nil),
		cfg:    cfg,
	}

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

	go b.pumpUpstream(ctx)
	b.clientLoop(ctx)
}

// pumpUpstream maps every Live sideband event to OpenAI events for the client, and closes the client when the call ends. Events still buffered when the call ends are written first, so the final turn.done or error is not lost.
func (b *realtimeAudioBridge) pumpUpstream(ctx context.Context) {
	events := b.call.Events()
	done := b.call.Done()
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
	b.mu.Unlock()
	for _, event := range mapped {
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

func (b *realtimeAudioBridge) writeError(apiErr *api.APIError) {
	_ = writeResponsesWSError(b.conn, apiErr)
}

func (b *realtimeAudioBridge) writeEvent(event []byte) {
	_ = writeResponsesWSMessage(b.conn, event)
}

func (b *realtimeAudioBridge) sendUpstream(event []byte) bool {
	if err := b.call.Send(event); err != nil {
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
	switch eventType {
	case "session.update":
		b.mu.Lock()
		sideband, ack, apiErr := applyRealtimeAudioSessionUpdate(&b.cfg, raw)
		b.mu.Unlock()
		if apiErr != nil {
			b.writeError(apiErr)
			return
		}
		if len(sideband) > 0 && !b.sendUpstream(sideband) {
			return
		}
		b.writeEvent(ack)

	case "input_audio_buffer.append":
		if err := b.input.AppendBase64(gjson.GetBytes(raw, "audio").String()); err != nil {
			b.writeError(invalidAudioRequest("input_audio_buffer.append failed: " + err.Error()))
		}

	case "input_audio_buffer.commit":
		if err := b.input.Commit(); err != nil {
			b.writeError(invalidAudioRequest("input_audio_buffer.commit failed: " + err.Error()))
			return
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
		b.input.Clear()
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
		// Upstream rejects parameters on response.create, so only the bare event is forwarded.
		b.sendUpstream([]byte(`{"type":"response.create"}`))

	case "response.cancel":
		// Documented no-op: Live interrupts on its own turn detection, and there is nothing to cancel on the gateway side.

	case "ping":
		b.writeEvent(marshalRealtimeServerEvent(map[string]any{"type": "pong"}))

	default:
		b.writeError(invalidAudioRequest("unsupported realtime websocket request type: " + eventType))
	}
}
