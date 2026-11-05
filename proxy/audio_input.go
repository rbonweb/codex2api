package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
)

// newRealtimeAudioEncoder creates the encoder used by realtimeAudioInput; tests replace it to inject a fake encoder on the stub build.
var newRealtimeAudioEncoder = newOpusEncoder

// errAudioInputClosed is returned by appends and commits after Close.
var errAudioInputClosed = errors.New("realtime audio input is closed")

// errAudioInputFull is returned when an append would push the buffered audio past the pacer cap.
var errAudioInputFull = errors.New("realtime audio input buffer holds more than 60 s of audio; commit or wait before appending more")

// realtimeAudioInput turns client PCM16 24 kHz mono audio into 20 ms Opus frames paced in real time to a sink.
type realtimeAudioInput struct {
	mu       sync.Mutex
	encoder  opusEncoder
	pacer    *audioPacer
	upsample pcmUpsampler
	framer   opusFramer
	closed   bool
}

// newRealtimeAudioInput creates the encoder and the pacer that writes frames to sink.
// On a build without libopus the error is errAudioBridgeUnavailable, unchanged.
func newRealtimeAudioInput(sink audioFrameSink) (*realtimeAudioInput, error) {
	enc, err := newRealtimeAudioEncoder()
	if err != nil {
		return nil, err
	}
	return &realtimeAudioInput{
		encoder: enc,
		pacer:   newAudioPacer(sink, audioFrameInterval, audioPacerMaxFrames),
	}, nil
}

// Start starts the pacer; frames queued before Start are written once it runs.
func (in *realtimeAudioInput) Start(ctx context.Context) {
	in.pacer.Start(ctx)
}

// AppendPCM16 upsamples 24 kHz PCM16 little-endian audio, encodes every complete frame and queues it for paced delivery.
func (in *realtimeAudioInput) AppendPCM16(pcm []byte) error {
	samples, err := decodePCM16LE(pcm)
	if err != nil {
		return err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errAudioInputClosed
	}
	frames, err := in.encodeLocked(in.framer.Push(in.upsample.Process(samples)))
	if err != nil {
		return err
	}
	return in.enqueueLocked(frames)
}

// AppendBase64 decodes standard base64 PCM16 audio and appends it.
func (in *realtimeAudioInput) AppendBase64(b64 string) error {
	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode base64 audio: %w", err)
	}
	return in.AppendPCM16(pcm)
}

// Commit pads the pending partial frame with silence, encodes it and queues it.
func (in *realtimeAudioInput) Commit() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errAudioInputClosed
	}
	frames, err := in.encodeLocked(in.framer.Flush())
	if err != nil {
		return err
	}
	return in.enqueueLocked(frames)
}

// Clear drops the pending partial frame, the upsampler history and every queued frame.
func (in *realtimeAudioInput) Clear() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.framer.Reset()
	in.upsample.Reset()
	in.pacer.Clear()
}

// WaitDrained blocks until every queued frame has been written to the sink, or ctx ends.
func (in *realtimeAudioInput) WaitDrained(ctx context.Context) error {
	return in.pacer.WaitDrained(ctx)
}

// Close stops the pacer and frees the encoder. It is idempotent.
func (in *realtimeAudioInput) Close() {
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return
	}
	in.closed = true
	in.mu.Unlock()
	in.pacer.Stop()
	in.encoder.Close()
}

// encodeLocked encodes frames in order. The caller holds in.mu.
func (in *realtimeAudioInput) encodeLocked(frames [][]int16) ([][]byte, error) {
	packets := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		packet, err := in.encoder.Encode(frame)
		if err != nil {
			return nil, fmt.Errorf("encode opus frame: %w", err)
		}
		packets = append(packets, packet)
	}
	return packets, nil
}

// enqueueLocked queues encoded frames for the pacer. The caller holds in.mu.
func (in *realtimeAudioInput) enqueueLocked(packets [][]byte) error {
	err := in.pacer.Enqueue(packets)
	if errors.Is(err, errAudioPacerFull) {
		return errAudioInputFull
	}
	return err
}
