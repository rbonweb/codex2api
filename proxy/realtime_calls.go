package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// realtimeCallSession is the subset of the OpenAI Realtime session object that the audio bridge reads.
// Every other OpenAI session field (type, output_modalities, audio formats, turn_detection, tools) is ignored.
// The ChatGPT Live upstream speaks its own session schema, so buildLiveAudioSession builds the upstream session from scratch.
type realtimeCallSession struct {
	Model        string `json:"model"`
	Voice        string `json:"voice"`
	Instructions string `json:"instructions"`
	Audio        struct {
		Output struct {
			Voice string `json:"voice"`
		} `json:"output"`
	} `json:"audio"`
}

// realtimeCallsJSONBody is the JSON request shape that RealtimeCalls accepts besides multipart and raw SDP.
type realtimeCallsJSONBody struct {
	SDP     string          `json:"sdp"`
	Session json.RawMessage `json:"session"`
}

// RealtimeCalls creates an OpenAI Realtime WebRTC call as a ChatGPT Live call.
// It accepts the three request shapes OpenAI clients send: multipart/form-data, application/json and a raw SDP body.
// The session is rebuilt for the Live upstream, then the request is handed to LiveCreate, so the response and the sideband path match POST /v1/live.
func (h *Handler) RealtimeCalls(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, liveUpstreamBodyLimit+1))
	if err != nil {
		realtimeCallsBadRequest(c, "failed to read request body")
		return
	}
	if len(raw) > liveUpstreamBodyLimit {
		realtimeCallsBadRequest(c, "request body is too large")
		return
	}
	sdp, sessionRaw, err := parseRealtimeCallsBody(c, raw)
	if err != nil {
		realtimeCallsBadRequest(c, err.Error())
		return
	}
	if strings.TrimSpace(sdp) == "" {
		realtimeCallsBadRequest(c, "sdp is required")
		return
	}
	session, err := parseRealtimeCallSession(sessionRaw)
	if err != nil {
		realtimeCallsBadRequest(c, err.Error())
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		model = strings.TrimSpace(session.Model)
	}
	if model != "" && !isRealtimeAudioModel(model) {
		realtimeCallsBadRequest(c, "model "+model+" is not a realtime model; use gpt-realtime or gpt-live")
		return
	}
	voice := strings.TrimSpace(session.Audio.Output.Voice)
	if voice == "" {
		voice = strings.TrimSpace(session.Voice)
	}
	upstreamSession := buildLiveAudioSession(model, voice, session.Instructions)
	body, err := json.Marshal(liveCallRequest{SDP: sdp, Session: upstreamSession})
	if err != nil {
		realtimeCallsBadRequest(c, "failed to build the Live call request")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.ContentLength = int64(len(body))
	h.LiveCreate(c)
}

// parseRealtimeCallsBody reads the SDP and the raw session JSON from any of the three accepted request shapes.
func parseRealtimeCallsBody(c *gin.Context, raw []byte) (string, []byte, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]))
	if mediaType == "multipart/form-data" {
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		if err := c.Request.ParseMultipartForm(liveUpstreamBodyLimit); err != nil {
			return "", nil, errors.New("request body must be valid multipart form data")
		}
		return c.Request.PostFormValue("sdp"), []byte(c.Request.PostFormValue("session")), nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil, nil
	}
	rawSDP := mediaType == "application/sdp"
	if mediaType == "" || mediaType == "text/plain" {
		rawSDP = bytes.HasPrefix(bytes.TrimSpace(raw), []byte("v=0"))
	}
	if rawSDP {
		return string(raw), nil, nil
	}
	var request realtimeCallsJSONBody
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", nil, errors.New("request body must be valid JSON")
	}
	return request.SDP, request.Session, nil
}

// parseRealtimeCallSession reads the fields of the OpenAI session object that the audio bridge uses.
func parseRealtimeCallSession(raw []byte) (realtimeCallSession, error) {
	var session realtimeCallSession
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return session, nil
	}
	if !json.Valid(trimmed) {
		return session, errors.New("session must be valid JSON")
	}
	if err := json.Unmarshal(trimmed, &session); err != nil {
		return session, errors.New("session must be a JSON object")
	}
	return session, nil
}

func realtimeCallsBadRequest(c *gin.Context, message string) {
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeInvalidRequest, message, api.ErrorTypeInvalidRequest), http.StatusBadRequest)
}
