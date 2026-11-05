package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// This file is the WebRTC flavour of the public GPT-Live protocol: POST /v1/live/sessions creates a call whose media the client owns, and GET /v1/live/sessions/:session_id/attach is a translated sideband that speaks the public events.
// The gateway never touches the media on this path, so it needs no Opus encoder or decoder and the no-cgo build serves both routes unchanged.

const (
	liveSessionsConfigCap = 4096
	liveSessionsTickEvery = time.Second
)

// liveSessionConfigs remembers the validated session of every call created through POST /v1/live/sessions until the call expires, so the attach sideband translates with the same config.
// It is bounded and in memory: after a restart the attach falls back to the default config.
var liveSessionConfigs = &liveSessionConfigStore{entries: map[string]liveSessionConfigEntry{}}

type liveSessionConfigEntry struct {
	cfg     *liveSessionConfig
	expires time.Time
}

type liveSessionConfigStore struct {
	mu      sync.Mutex
	entries map[string]liveSessionConfigEntry
}

func (s *liveSessionConfigStore) put(id string, cfg *liveSessionConfig, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, entry := range s.entries {
		if !entry.expires.After(now) {
			delete(s.entries, key)
		}
	}
	for len(s.entries) >= liveSessionsConfigCap {
		oldestKey, oldest := "", time.Time{}
		for key, entry := range s.entries {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = key, entry.expires
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[id] = liveSessionConfigEntry{cfg: cfg, expires: expires}
}

func (s *liveSessionConfigStore) get(id string) *liveSessionConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok || !entry.expires.After(time.Now()) {
		delete(s.entries, id)
		return nil
	}
	return entry.cfg
}

func (s *liveSessionConfigStore) drop(id string) {
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
}

// liveSessionsHTTPError writes the public error body {"error":{type,code,message,param}}.
func liveSessionsHTTPError(c *gin.Context, status int, errType, code, message, param string) {
	var paramValue any
	if param != "" {
		paramValue = param
	}
	c.JSON(status, gin.H{"error": gin.H{"type": errType, "code": code, "message": message, "param": paramValue}})
}

func liveSessionsHTTPSessionError(c *gin.Context, e *liveSessionError) {
	liveSessionsHTTPError(c, http.StatusBadRequest, "invalid_request_error", e.Code, e.Message, e.Param)
}

func liveSessionsHTTPGatewayError(c *gin.Context, e *liveGatewayError) {
	status, errType, code, message := http.StatusServiceUnavailable, string(api.ErrorTypeServer), string(api.ErrCodeServiceUnavailable), "Service temporarily unavailable"
	if e != nil {
		if e.Status != 0 {
			status = e.Status
		}
		if e.Err != nil {
			errType, code, message = string(e.Err.Type), string(e.Err.Code), e.Err.Message
		}
	}
	liveSessionsHTTPError(c, status, errType, code, message, "")
}

func liveSessionsRequireAllowLive(c *gin.Context) bool {
	if row := apiKeyRowFromContext(c); row != nil && row.Limits.AllowLive {
		return true
	}
	liveSessionsHTTPError(c, http.StatusForbidden, string(api.ErrorTypePermission), string(api.ErrCodeInsufficientScope), "ChatGPT Live is not enabled for this API key", "")
	return false
}

// parseLiveSessionsCreate validates the POST body {session, transport:{type:"webrtc", sdp}} and returns the session config and the client SDP.
func parseLiveSessionsCreate(raw []byte) (*liveSessionConfig, string, *liveSessionError) {
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return nil, "", newLiveSessionError("invalid_value", "", "The request body must be a JSON object.", "")
	}
	root := gjson.ParseBytes(raw)
	if e := liveUnknownKeys(root, "", []string{"session", "transport"}, ""); e != nil {
		e.Param = strings.TrimPrefix(e.Param, ".")
		e.Message = "Unknown parameter: '" + e.Param + "'."
		return nil, "", e
	}
	transport := root.Get("transport")
	if liveAbsent(transport) {
		return nil, "", newLiveSessionError("missing_required_parameter", "transport", "transport is required.", "")
	}
	if !transport.IsObject() {
		return nil, "", newLiveSessionError("invalid_value", "transport", "transport must be an object.", "")
	}
	if e := liveUnknownKeys(transport, "transport", []string{"type", "sdp"}, ""); e != nil {
		return nil, "", e
	}
	if t := transport.Get("type"); liveAbsent(t) {
		return nil, "", newLiveSessionError("missing_required_parameter", "transport.type", "transport.type is required.", "")
	} else if t.Type != gjson.String || t.String() != "webrtc" {
		return nil, "", newLiveSessionError("unsupported_value", "transport.type", "Only transport.type 'webrtc' is supported on this endpoint.", "")
	}
	sdp := transport.Get("sdp")
	if liveAbsent(sdp) || sdp.Type != gjson.String || strings.TrimSpace(sdp.String()) == "" {
		return nil, "", newLiveSessionError("missing_required_parameter", "transport.sdp", "transport.sdp is required.", "")
	}
	session := root.Get("session")
	if liveAbsent(session) {
		return nil, "", newLiveSessionError("missing_required_parameter", "session", "session is required.", "")
	}
	cfg, e := parseLiveSessionObject(session, "")
	if e != nil {
		return nil, "", e
	}
	if !liveAbsent(session.Get("audio.format")) {
		return nil, "", newLiveSessionError("unsupported_value", "session.audio.format", "session.audio.format is not supported over WebRTC: the audio format is negotiated in the SDP.", "")
	}
	return cfg, sdp.String(), nil
}

// LiveSessionsCreate handles POST /v1/live/sessions: the client owns the media, the gateway creates the call with the translated session and answers with the public shape {session:{id}, transport:{type:"webrtc", sdp}} (HTTP 201).
func (h *Handler) LiveSessionsCreate(c *gin.Context) {
	if !liveSessionsRequireAllowLive(c) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, liveUpstreamBodyLimit+1))
	if err != nil || len(raw) > liveUpstreamBodyLimit {
		liveSessionsHTTPError(c, http.StatusBadRequest, "invalid_request_error", "invalid_value", "The request body could not be read or is too large.", "")
		return
	}
	cfg, sdp, parseErr := parseLiveSessionsCreate(raw)
	if parseErr != nil {
		liveSessionsHTTPSessionError(c, parseErr)
		return
	}
	call, gwErr := h.createLiveCallWithoutReply(c, &liveCallRequest{SDP: sdp, Session: cfg.UpstreamSession()}, false)
	if gwErr != nil {
		liveSessionsHTTPGatewayError(c, gwErr)
		return
	}
	liveSessionConfigs.put(call.Record.CallID, cfg, call.Record.ExpiresAt)
	c.JSON(http.StatusCreated, gin.H{
		"session":   gin.H{"id": call.Record.CallID},
		"transport": gin.H{"type": "webrtc", "sdp": string(call.AnswerSDP)},
	})
}

// LiveSessionsAttach handles GET /v1/live/sessions/:session_id/attach: a WebSocket sideband for a call created by LiveSessionsCreate (or any pending call of the same API key) that speaks the public events.
func (h *Handler) LiveSessionsAttach(c *gin.Context) {
	if !liveSessionsRequireAllowLive(c) {
		return
	}
	callID, err := url.PathUnescape(strings.TrimSpace(c.Param("session_id")))
	if err != nil || callID == "" {
		liveSessionsHTTPError(c, http.StatusBadRequest, "invalid_request_error", "missing_required_parameter", "session_id is required", "session_id")
		return
	}
	record, _, err := h.liveCalls().get(c.Request.Context(), callID)
	if err != nil || record == nil || record.Controller == liveControllerClosed {
		liveSessionsHTTPError(c, http.StatusNotFound, string(api.ErrorTypeNotFound), string(api.ErrCodeResourceNotFound), "Live session not found", "")
		return
	}
	if record.APIKeyID != requestAPIKeyID(c) {
		liveSessionsHTTPError(c, http.StatusForbidden, string(api.ErrorTypePermission), string(api.ErrCodeInsufficientScope), "Live session belongs to another API key", "")
		return
	}
	if _, claimed, err := h.liveCalls().claimProxy(c.Request.Context(), record); err != nil || !claimed {
		liveSessionsHTTPError(c, http.StatusConflict, "invalid_request_error", string(api.ErrCodeResourceConflict), "Live session is already attached", "")
		return
	}
	h.serveLiveSessionsAttach(c, record)
}

func liveSessionsConfigFor(callID string) *liveSessionConfig {
	if cfg := liveSessionConfigs.get(callID); cfg != nil {
		return cfg
	}
	cfg, _ := parseLiveSessionObject(gjson.Parse(`{}`), "")
	return cfg
}

// liveSessionsSocket serializes writes to one WebSocket.
type liveSessionsSocket struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (s *liveSessionsSocket) write(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout))
	return s.conn.WriteMessage(websocket.TextMessage, payload)
}

// liveSessionsPump reads text frames from conn into out until the connection ends or ctx is done; it closes done when the read fails.
func liveSessionsPump(ctx context.Context, conn *websocket.Conn, out chan<- []byte, done chan<- struct{}) {
	defer close(done)
	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}
		select {
		case out <- payload:
		case <-ctx.Done():
			return
		}
	}
}

func (h *Handler) serveLiveSessionsAttach(c *gin.Context, record *liveCallRecord) {
	downstream, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		go h.liveCalls().finalize(record)
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	var upstream *websocket.Conn
	stopKeepalive := startDownstreamWSKeepalive(ctx, downstream, cancel)
	finalized := false
	defer func() {
		cancel()
		_ = downstream.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
		stopKeepalive()
		liveSessionConfigs.drop(record.CallID)
		if !finalized {
			h.liveCalls().finalize(record)
		}
	}()

	client := &liveSessionsSocket{conn: downstream}
	upstream, err = h.dialLiveSideband(ctx, record)
	if err != nil {
		log.Printf("live: sessions attach sideband unavailable request_id=%s account_id=%d call_hash=%s error=%s",
			record.RequestID, record.AccountID, record.CallHash, security.SanitizeLog(err.Error()))
		_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "live sideband unavailable"))
		return
	}
	upstreamSock := &liveSessionsSocket{conn: upstream}

	translator := newLiveSessionTranslator(liveSessionsConfigFor(record.CallID))
	clientMsgs, upstreamMsgs := make(chan []byte, 16), make(chan []byte, 64)
	clientGone, upstreamGone := make(chan struct{}), make(chan struct{})
	go liveSessionsPump(ctx, downstream, clientMsgs, clientGone)
	go liveSessionsPump(ctx, upstream, upstreamMsgs, upstreamGone)
	ticker := time.NewTicker(liveSessionsTickEvery)
	defer ticker.Stop()
	expiry := time.NewTimer(time.Until(record.ExpiresAt))
	defer expiry.Stop()

	writeAll := func(sock *liveSessionsSocket, events [][]byte) error {
		for _, event := range events {
			if err := sock.write(event); err != nil {
				return err
			}
		}
		return nil
	}
	// end reports session.closed to the client, releases the call exactly once, and stops the loop.
	end := func(reason string) {
		_ = client.write(translator.Closed(reason))
		h.liveCalls().finalize(record)
		finalized = true
	}

	// handleUpstream translates one internal event; false means a write failed and the loop must stop.
	handleUpstream := func(raw []byte) bool {
		res := translator.Upstream(raw)
		// res.AudioOut is dropped: the client has the audio on its own peer connection.
		return writeAll(upstreamSock, res.Upstream) == nil && writeAll(client, res.Client) == nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-clientGone:
			// The client left: nothing to tell it, just release.
			return
		case <-upstreamGone:
			// Deliver what upstream sent before it hung up, then report the hangup.
			for drained := false; !drained; {
				select {
				case raw := <-upstreamMsgs:
					if !handleUpstream(raw) {
						return
					}
				default:
					drained = true
				}
			}
			end("remote_hangup")
			return
		case <-expiry.C:
			end("expired")
			return
		case now := <-ticker.C:
			if writeAll(client, translator.Tick(now)) != nil {
				return
			}
		case raw := <-clientMsgs:
			if gjson.GetBytes(raw, "type").String() == "session.input_audio.append" {
				// The media travels over WebRTC; the sideband carries no audio.
				event := newLiveSessionError("unsupported_value", "type", "session.input_audio.append is not supported here: send audio over the WebRTC media track.", liveEventID(gjson.ParseBytes(raw))).Event()
				if client.write(event) != nil {
					return
				}
				continue
			}
			res := translator.Client(raw)
			if writeAll(upstreamSock, res.Upstream) != nil || writeAll(client, res.Client) != nil {
				return
			}
			if res.Close {
				if translator.Started() {
					_ = upstreamSock.write([]byte(`{"type":"session.close"}`))
				}
				end("close_requested")
				return
			}
		case raw := <-upstreamMsgs:
			if !handleUpstream(raw) {
				return
			}
		}
	}
}
