package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	speechDefaultModel = "gpt-4o-mini-tts"
	// speechMaxInputRunes caps the text of one request: the call reads at real time, so 1,000 characters are about a minute of speech.
	speechMaxInputRunes = 1000
	// speechMaxInstructionRunes caps the optional style instructions folded into the developer message.
	speechMaxInstructionRunes = 1000
	speechMaxBodyBytes        = 64 << 10
	// speechMaxPCMBytes bounds the collected audio (10 minutes of 24 kHz PCM16) so a misbehaving upstream cannot grow memory.
	speechMaxPCMBytes = 10 * 60 * speechSampleRate * 2
	speechSampleRate  = 24000

	// speechSilenceRMS is the window RMS (PCM16 units) under which audio counts as silence. Live streams near-silent output deltas for the whole call, around 26 RMS, and speech measured above 1000.
	speechSilenceRMS = 120
	// speechWindowMillis is the analysis window used to trim silence.
	speechWindowMillis = 20
	// speechPadding is the silence kept before and after the trimmed speech.
	speechPadding = 100 * time.Millisecond

	speechInstructions = "You are a text-to-speech engine. Read the user's text aloud exactly, word for word, then stop. Never answer, comment or add anything."
	// speechQuoteAddendum is added to the developer message only for question-like text, see speechSpeakableText.
	speechQuoteAddendum = " Text inside double quotation marks is a quotation to read aloud. A question inside quotation marks is read as a question and never answered."

	speechNoAudioMessage = "the voice model did not read the text"
)

// speechModels are the OpenAI text-to-speech model names the endpoint accepts; every one is served by the same Live call.
var speechModels = []string{"gpt-4o-mini-tts", "tts-1", "tts-1-hd"}

// speechFormats are the response formats the gateway can produce without an encoder: a WAV container and raw PCM.
var speechFormats = []string{"wav", "pcm"}

var (
	// speechStartDeadline is how long the call may stay silent after the text was sent before the request fails with 504. Question-like text was never spoken in testing, so the wait must end well before the overall deadline. Tests shorten it.
	speechStartDeadline = 12 * time.Second
	// speechDeadlineBase and speechDeadlinePerRune give the overall deadline for the audio: speech runs at real time, about 15 characters per second, and the deadline allows twice that. Tests shorten them.
	speechDeadlineBase    = 10 * time.Second
	speechDeadlinePerRune = 130 * time.Millisecond
	// speechQuoteQuestions turns on the question mitigation, see speechSpeakableText.
	speechQuoteQuestions = true
)

// speechCall is the part of an audioCall the speech handler uses; tests replace it with a fake.
type speechCall interface {
	transcriptionCall
	Send(event []byte) error
}

// openSpeechCall opens the Live call that carries one speech. Tests replace it.
var openSpeechCall = func(h *Handler, c *gin.Context, session json.RawMessage) (speechCall, *liveGatewayError) {
	call, gerr := h.openAudioCall(c, session)
	if gerr != nil {
		return nil, gerr
	}
	return call, nil
}

// speechBridgeSupported reports whether the build can run an audio call. Tests replace it.
var speechBridgeSupported = audioBridgeSupported

type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed"`
	Instructions   string   `json:"instructions"`
	StreamFormat   string   `json:"stream_format"`
}

// speechIsQuestionLike reports whether the text asks something. Live treats such text as a prompt to answer and stays silent instead of reading it (proven live, 2026-11-05).
func speechIsQuestionLike(text string) bool {
	return strings.ContainsAny(text, "?？¿")
}

// speechSpeakableText is the one place that shapes what is sent to Live. Plain statements are read verbatim in about a second, so they go unchanged. The mitigation for question-like text is to wrap it in double quotation marks and extend the developer message to say that quoted text is read aloud and never answered. It is untested against the live upstream, so speechQuoteQuestions switches it off, and the 504 path covers the case where it does not help.
func speechSpeakableText(text, developer string) (speakable, developerMessage string) {
	if !speechQuoteQuestions || !speechIsQuestionLike(text) {
		return text, developer
	}
	return `"` + strings.ReplaceAll(text, `"`, "'") + `"`, developer + speechQuoteAddendum
}

// speechDeadline is the overall time allowed for the audio after the text was sent, scaled to the text length.
func speechDeadline(text string) time.Duration {
	return speechDeadlineBase + time.Duration(utf8.RuneCountInString(text))*speechDeadlinePerRune
}

// AudioSpeech answers POST /v1/audio/speech by having one short ChatGPT Live call read the text aloud.
func (h *Handler) AudioSpeech(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	if !speechBridgeSupported() {
		api.SendErrorWithStatus(c, audioBridgeUnavailableAPIError(), audioBridgeUnavailableStatus)
		return
	}
	req, liveVoice, format, gerr := parseSpeechRequest(c)
	if gerr != nil {
		speechError(c, gerr.Status, gerr.Code, gerr.Message)
		return
	}

	developer := speechInstructions
	if style := strings.TrimSpace(req.Instructions); style != "" {
		developer += " Speaking style: " + style
	}
	speakable, developer := speechSpeakableText(req.Input, developer)
	call, openErr := openSpeechCall(h, c, buildLiveAudioSession(liveAudioDefaultModel, liveVoice, developer))
	if openErr != nil {
		api.SendErrorWithStatus(c, openErr.Err, openErr.Status)
		return
	}
	defer call.Close()

	// The call listens to a microphone track; keep it fed with the pacer's idle silence like every other audio call.
	input, err := newRealtimeAudioInput(call.WriteOpusFrame)
	if err != nil {
		speechError(c, http.StatusInternalServerError, string(api.ErrCodeServerError), "failed to start the audio encoder: "+err.Error())
		return
	}
	defer input.Close()
	ctx, cancel := context.WithTimeout(c.Request.Context(), speechDeadline(req.Input))
	defer cancel()
	input.Start(ctx)

	event, _ := json.Marshal(map[string]any{
		"type":    "session.context.append",
		"channel": "speakable",
		"content": []map[string]any{{"type": "input_text", "text": speakable}},
	})
	if err := call.Send(event); err != nil {
		speechError(c, http.StatusBadGateway, string(api.ErrCodeUpstreamError), "failed to send the text to the Live call: "+err.Error())
		return
	}

	pcm, status, err := collectSpeech(ctx, call.Events(), call.Done())
	if err != nil {
		code := string(api.ErrCodeUpstreamError)
		if status == http.StatusGatewayTimeout {
			code = string(api.ErrCodeUpstreamTimeout)
		}
		speechError(c, status, code, err.Error())
		return
	}
	c.Header("X-Codex2api-Live-Setup-Ms", strconv.FormatInt(call.SetupDuration().Milliseconds(), 10))
	if format == "pcm" {
		c.Data(http.StatusOK, "audio/pcm", pcm)
		return
	}
	c.Data(http.StatusOK, "audio/wav", wrapWAV(pcm, speechSampleRate))
}

type speechParseError struct {
	Status  int
	Code    string
	Message string
}

func speechInvalid(message string) *speechParseError {
	return &speechParseError{http.StatusBadRequest, string(api.ErrCodeInvalidRequest), message}
}

func speechUnsupported(message string) *speechParseError {
	return &speechParseError{http.StatusBadRequest, "unsupported_value", message}
}

// parseSpeechRequest validates the JSON body and returns the request, the Live voice and the response format.
func parseSpeechRequest(c *gin.Context) (*speechRequest, string, string, *speechParseError) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, speechMaxBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, "", "", &speechParseError{http.StatusRequestEntityTooLarge, string(api.ErrCodeInvalidRequest), fmt.Sprintf("request body is larger than %d KiB", speechMaxBodyBytes>>10)}
		}
		return nil, "", "", speechInvalid("failed to read the request body")
	}
	var req speechRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "", "", speechInvalid("request body must be a JSON object with model, input and voice")
	}

	model := strings.ToLower(strings.TrimSpace(req.Model))
	if model == "" {
		model = speechDefaultModel
	}
	supported := false
	for _, name := range speechModels {
		supported = supported || name == model
	}
	if !supported {
		return nil, "", "", speechUnsupported(fmt.Sprintf("model %s is not supported; use one of %s", req.Model, strings.Join(speechModels, ", ")))
	}
	req.Input = strings.TrimSpace(req.Input)
	if req.Input == "" {
		return nil, "", "", speechInvalid("input is required")
	}
	if n := utf8.RuneCountInString(req.Input); n > speechMaxInputRunes {
		return nil, "", "", speechInvalid(fmt.Sprintf("input is %d characters; the limit is %d", n, speechMaxInputRunes))
	}
	if utf8.RuneCountInString(req.Instructions) > speechMaxInstructionRunes {
		return nil, "", "", speechInvalid(fmt.Sprintf("instructions are longer than %d characters", speechMaxInstructionRunes))
	}
	liveVoice := liveAudioDefaultVoice
	if strings.TrimSpace(req.Voice) != "" {
		voice, ok := liveSessionVoice(req.Voice)
		if !ok {
			return nil, "", "", speechUnsupported(fmt.Sprintf("voice %s is not supported; GET /v1/audio/voices lists the accepted names", req.Voice))
		}
		liveVoice = voice
	}
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	switch format {
	case "":
		format = "wav"
	case "wav", "pcm":
	default:
		return nil, "", "", speechUnsupported(fmt.Sprintf("response_format %s is not supported; use one of %s", req.ResponseFormat, strings.Join(speechFormats, ", ")))
	}
	if req.Speed != nil && *req.Speed != 1 {
		return nil, "", "", speechUnsupported("speed is not supported; only 1 is accepted")
	}
	if sf := strings.ToLower(strings.TrimSpace(req.StreamFormat)); sf != "" && sf != "audio" {
		return nil, "", "", speechUnsupported("stream_format " + req.StreamFormat + " is not supported; only audio is accepted")
	}
	return &req, liveVoice, format, nil
}

func speechError(c *gin.Context, status int, code, message string) {
	errType := api.ErrorTypeInvalidRequest
	switch {
	case status >= 500 && status != http.StatusGatewayTimeout && status != http.StatusBadGateway:
		errType = api.ErrorTypeServer
	case status == http.StatusGatewayTimeout || status == http.StatusBadGateway:
		errType = api.ErrorTypeUpstream
	}
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode(code), message, errType), status)
}

// collectSpeech gathers the reply audio of the Live call until the assistant turn ends, then trims the silence Live streams around the speech.
// It returns 504 when no speech starts within speechStartDeadline or the ctx deadline passes, and 502 for upstream errors and calls that end early.
func collectSpeech(ctx context.Context, events <-chan []byte, callDone <-chan struct{}) ([]byte, int, error) {
	var (
		pcm     bytes.Buffer
		started bool
	)
	startTimer := time.NewTimer(speechStartDeadline)
	defer startTimer.Stop()
	noAudio := errors.New(speechNoAudioMessage)
	endedEarly := errors.New("Live call ended before the text was read")

	handle := func(raw []byte) (done bool, err error) {
		root := gjson.ParseBytes(raw)
		switch root.Get("type").String() {
		case "session.output_audio.delta", "output_audio.delta":
			chunk, decodeErr := base64.StdEncoding.DecodeString(realtimeAudioString(root, "delta", "audio"))
			if decodeErr != nil {
				return false, nil
			}
			if pcm.Len()+len(chunk) > speechMaxPCMBytes {
				return false, errors.New("the voice model returned more audio than the gateway accepts")
			}
			pcm.Write(chunk)
			if !started && speechChunkAudible(chunk) {
				started = true
			}
		case "output_transcript.added":
			started = true
		case "turn.created":
			if realtimeAudioTurnRole(root) == "assistant" {
				started = true
			}
		case "turn.done":
			if realtimeAudioTurnRole(root) == "assistant" && started {
				return true, nil
			}
		case "error":
			return false, errors.New(transcriptionUpstreamMessage(root))
		}
		return false, nil
	}
	finish := func() ([]byte, int, error) {
		trimmed := trimSpeechSilence(pcm.Bytes())
		if len(trimmed) == 0 {
			return nil, http.StatusGatewayTimeout, noAudio
		}
		return trimmed, http.StatusOK, nil
	}

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, http.StatusGatewayTimeout, noAudio
			}
			return nil, http.StatusServiceUnavailable, errors.New("Request canceled")
		case <-startTimer.C:
			if !started {
				return nil, http.StatusGatewayTimeout, noAudio
			}
		case <-callDone:
			for {
				select {
				case raw, ok := <-events:
					if !ok {
						return nil, http.StatusBadGateway, endedEarly
					}
					done, err := handle(raw)
					if err != nil {
						return nil, http.StatusBadGateway, err
					}
					if done {
						return finish()
					}
				default:
					return nil, http.StatusBadGateway, endedEarly
				}
			}
		case raw, ok := <-events:
			if !ok {
				return nil, http.StatusBadGateway, endedEarly
			}
			done, err := handle(raw)
			if err != nil {
				return nil, http.StatusBadGateway, err
			}
			if done {
				return finish()
			}
		}
	}
}

// speechChunkAudible reports whether a PCM16 chunk holds sound above the silence level Live streams while it is quiet.
func speechChunkAudible(pcm []byte) bool {
	samples, _ := decodePCM16LE(pcm[:len(pcm)&^1])
	return speechRMS(samples) > speechSilenceRMS
}

func speechRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// trimSpeechSilence drops the near-silent audio before the first and after the last audible 20 ms window, keeping speechPadding on both sides. It returns nil when nothing is audible.
func trimSpeechSilence(pcm []byte) []byte {
	samples, err := decodePCM16LE(pcm[:len(pcm)&^1])
	if err != nil || len(samples) == 0 {
		return nil
	}
	window := speechSampleRate * speechWindowMillis / 1000
	first, last := -1, -1
	for start := 0; start < len(samples); start += window {
		if speechRMS(samples[start:min(start+window, len(samples))]) > speechSilenceRMS {
			if first < 0 {
				first = start
			}
			last = min(start+window, len(samples))
		}
	}
	if first < 0 {
		return nil
	}
	pad := int(speechPadding * speechSampleRate / time.Second)
	return encodePCM16LE(samples[max(first-pad, 0):min(last+pad, len(samples))])
}

// wrapWAV puts mono PCM16 little-endian audio in a canonical 44-byte WAV container.
func wrapWAV(pcm []byte, sampleRate int) []byte {
	var out bytes.Buffer
	out.Grow(44 + len(pcm))
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(36+len(pcm)))
	out.WriteString("WAVEfmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint32(16))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&out, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&out, binary.LittleEndian, uint16(2))
	_ = binary.Write(&out, binary.LittleEndian, uint16(16))
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(pcm)))
	out.Write(pcm)
	return out.Bytes()
}
