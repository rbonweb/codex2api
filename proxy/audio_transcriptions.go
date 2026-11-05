package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	transcriptionDefaultModel = "gpt-4o-transcribe"
	transcriptionChunkMillis  = 100
	// transcriptionMaxQueuedFrames keeps the feeder below the pacer cap (audioPacerMaxFrames) so a 120 s upload never overflows it.
	transcriptionMaxQueuedFrames = audioPacerMaxFrames / 2
	// transcriptionFormMemory is how much of the multipart body stays in memory before it spills to disk.
	transcriptionFormMemory = 8 << 20
	// transcriptionBodyOverhead is the room above the audio cap for the other multipart fields and framing.
	transcriptionBodyOverhead = 1 << 20

	transcriptionInstructions = "You are a silent transcription service. Never speak, never answer, never acknowledge. Produce no audio output."
)

// transcriptionModels are the OpenAI transcription model names the endpoint accepts; every one is served by the same Live call.
var transcriptionModels = []string{"gpt-4o-transcribe", "gpt-4o-mini-transcribe", "whisper-1"}

// transcriptionTrailingSilence is the silence streamed after the audio so the Live call detects the end of the user turn.
var transcriptionTrailingSilence = 1500 * time.Millisecond

// transcriptionDeadlineSlack is added to the audio duration to get the deadline for the whole transcript. Tests shorten it.
var transcriptionDeadlineSlack = 20 * time.Second

// transcriptionPaceNote: the audio is streamed at real time (1x) because the pacer sends one 20 ms frame per 20 ms. Faster pacing (sending frames ahead of the clock) is untested against the Live upstream and may drop audio or break turn detection, so it is not enabled.
const transcriptionPaceNote = "real time (1x)"

// transcriptionCall is the part of an audioCall the transcription handler uses; tests replace it with a fake.
type transcriptionCall interface {
	WriteOpusFrame(frame []byte, d time.Duration) error
	Events() <-chan []byte
	SetupDuration() time.Duration
	Close()
}

// openTranscriptionCall opens the Live call that carries one transcription. Tests replace it.
var openTranscriptionCall = func(h *Handler, c *gin.Context, session json.RawMessage) (transcriptionCall, *liveGatewayError) {
	call, gerr := h.openAudioCall(c, session)
	if gerr != nil {
		return nil, gerr
	}
	return call, nil
}

// transcriptionBridgeSupported reports whether the build can encode Opus. Tests replace it.
var transcriptionBridgeSupported = audioBridgeSupported

type transcriptionRequest struct {
	filename    string
	contentType string
	data        []byte
	model       string
	language    string
	prompt      string
	format      string
}

// AudioTranscriptions answers POST /v1/audio/transcriptions by streaming the upload into one short ChatGPT Live call that is told to stay silent, and returning the transcript of the user turn.
func (h *Handler) AudioTranscriptions(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	if !transcriptionBridgeSupported() {
		api.SendErrorWithStatus(c, audioBridgeUnavailableAPIError(), audioBridgeUnavailableStatus)
		return
	}
	req, status, err := parseTranscriptionRequest(c)
	if err != nil {
		transcriptionError(c, status, err.Error())
		return
	}
	samples, err := decodeAudioUpload(req.filename, req.contentType, req.data)
	if err != nil {
		transcriptionError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.data = nil

	instructions := transcriptionInstructions
	if req.language != "" {
		instructions += " The audio is in " + req.language + "."
	}
	if req.prompt != "" {
		instructions += " Context: " + req.prompt
	}
	call, gerr := openTranscriptionCall(h, c, buildLiveAudioSession(liveAudioDefaultModel, "", instructions))
	if gerr != nil {
		api.SendErrorWithStatus(c, gerr.Err, gerr.Status)
		return
	}
	defer call.Close()

	audioDuration := time.Duration(len(samples)) * time.Second / audioUploadSampleRate
	ctx, cancel := context.WithTimeout(c.Request.Context(), audioDuration+transcriptionDeadlineSlack)
	defer cancel()

	written := &atomic.Int64{}
	progress := make(chan struct{}, 1)
	input, err := newRealtimeAudioInput(func(frame []byte, d time.Duration) error {
		err := call.WriteOpusFrame(frame, d)
		written.Add(1)
		select {
		case progress <- struct{}{}:
		default:
		}
		return err
	})
	if err != nil {
		transcriptionError(c, http.StatusInternalServerError, "failed to start the audio encoder: "+err.Error())
		return
	}
	defer input.Close()
	input.Start(ctx)

	feedDone := make(chan error, 1)
	feedExited := make(chan struct{})
	feedCtx, stopFeed := context.WithCancel(ctx)
	go func() {
		defer close(feedExited)
		feedDone <- feedTranscriptionAudio(feedCtx, input, samples, written, progress)
	}()
	defer func() {
		stopFeed()
		<-feedExited
	}()

	text, status, err := readTranscript(ctx, call.Events(), feedDone)
	if err != nil {
		transcriptionError(c, status, err.Error())
		return
	}
	c.Header("X-Codex2api-Live-Setup-Ms", strconv.FormatInt(call.SetupDuration().Milliseconds(), 10))
	if req.format == "text" {
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(text))
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": text})
}

// feedTranscriptionAudio queues the samples in 100 ms chunks, then the trailing silence, then commits.
// It holds back while more than transcriptionMaxQueuedFrames frames wait for the pacer, so a 120 s upload never overflows the pacer cap.
func feedTranscriptionAudio(ctx context.Context, input *realtimeAudioInput, samples []int16, written *atomic.Int64, progress <-chan struct{}) error {
	chunkSamples := audioUploadSampleRate * transcriptionChunkMillis / 1000
	silenceSamples := int(transcriptionTrailingSilence * audioUploadSampleRate / time.Second)
	var fed int64
	appendChunk := func(chunk []int16) error {
		for fed*2/opusFrameSamples-written.Load() > transcriptionMaxQueuedFrames {
			select {
			case <-progress:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := input.AppendPCM16(encodePCM16LE(chunk)); err != nil {
			return err
		}
		fed += int64(len(chunk))
		return nil
	}
	for start := 0; start < len(samples); start += chunkSamples {
		if err := appendChunk(samples[start:min(start+chunkSamples, len(samples))]); err != nil {
			return err
		}
	}
	silence := make([]int16, chunkSamples)
	for remaining := silenceSamples; remaining > 0; remaining -= chunkSamples {
		if err := appendChunk(silence[:min(chunkSamples, remaining)]); err != nil {
			return err
		}
	}
	return input.Commit()
}

// readTranscript waits for the user turn transcript on the Live call's events.
// It returns the text, or the HTTP status and error to answer with.
// feedDone carries a failure of the audio feeder; a clean finish is ignored and the wait goes on for the transcript.
func readTranscript(ctx context.Context, events <-chan []byte, feedDone <-chan error) (string, int, error) {
	var heard strings.Builder
	feed := feedDone
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", http.StatusGatewayTimeout, errors.New("timed out waiting for the transcript")
			}
			return "", http.StatusServiceUnavailable, errors.New("Request canceled")
		case err := <-feed:
			if err != nil && ctx.Err() == nil {
				return "", http.StatusBadGateway, fmt.Errorf("failed to stream the audio to the Live call: %w", err)
			}
			feed = nil
		case raw, ok := <-events:
			if !ok {
				return "", http.StatusBadGateway, errors.New("Live call ended before a transcript was produced")
			}
			root := gjson.ParseBytes(raw)
			switch root.Get("type").String() {
			case "input_transcript.added":
				heard.WriteString(realtimeAudioString(root, "item.text", "delta", "text"))
			case "turn.done":
				if realtimeAudioTurnRole(root) != "user" {
					continue
				}
				text := strings.TrimSpace(realtimeAudioString(root, "turn.transcript", "transcript"))
				if text == "" {
					text = strings.TrimSpace(heard.String())
				}
				return text, http.StatusOK, nil
			case "error":
				return "", http.StatusBadGateway, errors.New(transcriptionUpstreamMessage(root))
			}
		}
	}
}

func transcriptionUpstreamMessage(root gjson.Result) string {
	errObj := root.Get("error")
	message := errObj.Get("message").String()
	if errObj.Type == gjson.String {
		message = errObj.String()
	}
	if message == "" {
		message = root.Get("message").String()
	}
	if message == "" {
		message = "ChatGPT Live error"
	}
	return "ChatGPT Live error: " + message
}

// parseTranscriptionRequest reads and validates the multipart upload. On failure it returns the HTTP status to answer with.
func parseTranscriptionRequest(c *gin.Context) (*transcriptionRequest, int, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type"))), "multipart/form-data") {
		return nil, http.StatusBadRequest, errors.New("request must be multipart/form-data with a file field")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, audioUploadMaxBytes+transcriptionBodyOverhead)
	if err := c.Request.ParseMultipartForm(transcriptionFormMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("request body is larger than the %d MiB audio limit", audioUploadMaxBytes>>20)
		}
		return nil, http.StatusBadRequest, errors.New("request body must be valid multipart form data")
	}
	form := c.Request.MultipartForm
	defer form.RemoveAll()
	value := func(name string) string {
		if values := form.Value[name]; len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
		return ""
	}

	req := &transcriptionRequest{
		model:    value("model"),
		language: value("language"),
		prompt:   value("prompt"),
		format:   strings.ToLower(value("response_format")),
	}
	switch strings.ToLower(req.model) {
	case "":
		req.model = transcriptionDefaultModel
	case "gpt-4o-transcribe", "gpt-4o-mini-transcribe", "whisper-1":
	case "gpt-4o-transcribe-diarize":
		return nil, http.StatusBadRequest, errors.New("model gpt-4o-transcribe-diarize is not supported; speaker diarization is not available")
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("model %s is not supported; use one of %s", req.model, strings.Join(transcriptionModels, ", "))
	}
	switch req.format {
	case "":
		req.format = "json"
	case "json", "text":
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("response_format %s is not supported; use json or text", req.format)
	}
	if on, err := strconv.ParseBool(value("stream")); err == nil && on {
		return nil, http.StatusBadRequest, errors.New("stream=true is not supported for transcriptions")
	}

	files := form.File["file"]
	if len(files) == 0 {
		return nil, http.StatusBadRequest, errors.New("file is required")
	}
	header := files[0]
	if header.Size > audioUploadMaxBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("audio file is larger than the %d MiB limit", audioUploadMaxBytes>>20)
	}
	file, err := header.Open()
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("failed to read the uploaded file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, audioUploadMaxBytes+1))
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("failed to read the uploaded file")
	}
	if len(data) > audioUploadMaxBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("audio file is larger than the %d MiB limit", audioUploadMaxBytes>>20)
	}
	req.data = data
	req.filename = header.Filename
	req.contentType = header.Header.Get("Content-Type")
	return req, http.StatusOK, nil
}

func transcriptionError(c *gin.Context, status int, message string) {
	switch {
	case status >= 500 && status != http.StatusGatewayTimeout && status != http.StatusBadGateway:
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServerError, message, api.ErrorTypeServer), status)
	case status == http.StatusGatewayTimeout:
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamTimeout, message, api.ErrorTypeUpstream), status)
	case status == http.StatusBadGateway:
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, message, api.ErrorTypeUpstream), status)
	default:
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeInvalidRequest, message, api.ErrorTypeInvalidRequest), status)
	}
}
