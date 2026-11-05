package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAudioEncoder returns a 4-byte packet per frame: a marker byte followed by the frame sequence number.
type fakeAudioEncoder struct {
	mu     sync.Mutex
	count  int
	closes int
}

func (e *fakeAudioEncoder) Encode(frame []int16) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count++
	return []byte{0xA5, byte(e.count), 0x00, 0x01}, nil
}

func (e *fakeAudioEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closes++
}

func (e *fakeAudioEncoder) encoded() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count
}

func (e *fakeAudioEncoder) closed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closes
}

// newFakeAudioInput builds an input on a fake encoder and a channel sink. The pacer is not started.
func newFakeAudioInput(t *testing.T) (*realtimeAudioInput, *fakeAudioEncoder, chan []byte) {
	t.Helper()
	enc := &fakeAudioEncoder{}
	prev := newRealtimeAudioEncoder
	newRealtimeAudioEncoder = func() (opusEncoder, error) { return enc, nil }
	t.Cleanup(func() { newRealtimeAudioEncoder = prev })

	sink := make(chan []byte, 4096)
	in, err := newRealtimeAudioInput(func(frame []byte, d time.Duration) error {
		if d != audioFrameInterval {
			t.Errorf("duration = %v, want %v", d, audioFrameInterval)
		}
		sink <- append([]byte(nil), frame...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	return in, enc, sink
}

// pcm24k returns n bytes of little-endian PCM16 at 24 kHz with a simple ramp.
func pcm24k(n int) []byte {
	out := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		v := uint16(int16((i / 2) % 1000))
		out[i] = byte(v)
		out[i+1] = byte(v >> 8)
	}
	return out
}

func recvAudioFrame(t *testing.T, sink chan []byte) []byte {
	t.Helper()
	select {
	case f := <-sink:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an audio frame")
	}
	return nil
}

func TestAudioInputHundredMillisecondsGivesFiveFramesInOrder(t *testing.T) {
	in, enc, sink := newFakeAudioInput(t)
	if err := in.AppendPCM16(pcm24k(4800)); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 5 {
		t.Fatalf("encoded %d frames, want 5", got)
	}
	in.Start(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.WaitDrained(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		f := recvAudioFrame(t, sink)
		if len(f) != 4 || f[0] != 0xA5 || f[1] != byte(i) {
			t.Fatalf("frame %d = %v", i, f)
		}
	}
}

func TestAudioInputSplitAppendsMatchFrameCount(t *testing.T) {
	in, enc, _ := newFakeAudioInput(t)
	pcm := pcm24k(4800)
	// Chunks of 700 bytes cross the 960-byte (one frame at 24 kHz) boundary repeatedly.
	for off := 0; off < len(pcm); off += 700 {
		end := off + 700
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := in.AppendPCM16(pcm[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	if got := enc.encoded(); got != 5 {
		t.Fatalf("encoded %d frames after split appends, want 5", got)
	}
}

func TestAudioInputRejectsOddLengthAndBadBase64(t *testing.T) {
	in, _, _ := newFakeAudioInput(t)
	err := in.AppendPCM16([]byte{1, 2, 3})
	if err == nil || !strings.Contains(err.Error(), "pcm16") {
		t.Fatalf("odd length error = %v, want one mentioning pcm16", err)
	}
	err = in.AppendBase64("!!! not base64 !!!")
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("bad base64 error = %v, want one mentioning base64", err)
	}
}

func TestAudioInputAppendBase64DecodesPCM(t *testing.T) {
	in, enc, _ := newFakeAudioInput(t)
	// 4800 bytes encoded as standard base64 must produce the same five frames as the raw bytes.
	b64 := base64.StdEncoding.EncodeToString(pcm24k(4800))
	if err := in.AppendBase64(b64); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 5 {
		t.Fatalf("encoded %d frames from base64, want 5", got)
	}
}

func TestAudioInputCommitFlushesPartialFrame(t *testing.T) {
	in, enc, sink := newFakeAudioInput(t)
	// 1000 bytes give one full frame plus 40 samples pending.
	if err := in.AppendPCM16(pcm24k(1000)); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 1 {
		t.Fatalf("before commit encoded %d frames, want 1", got)
	}
	if err := in.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 2 {
		t.Fatalf("after commit encoded %d frames, want 2", got)
	}
	if err := in.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 2 {
		t.Fatalf("second commit with nothing pending encoded more frames: %d", got)
	}
	in.Start(context.Background())
	for i := 1; i <= 2; i++ {
		if f := recvAudioFrame(t, sink); f[1] != byte(i) {
			t.Fatalf("frame %d = %v", i, f)
		}
	}
}

func TestAudioInputClearDropsQueuedFrames(t *testing.T) {
	in, enc, sink := newFakeAudioInput(t)
	if err := in.AppendPCM16(pcm24k(4800)); err != nil {
		t.Fatal(err)
	}
	// Leave a partial frame pending as well.
	if err := in.AppendPCM16(pcm24k(100)); err != nil {
		t.Fatal(err)
	}
	in.Clear()
	if got := in.pacer.Buffered(); got != 0 {
		t.Fatalf("buffered after Clear = %d", got)
	}
	if err := in.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := enc.encoded(); got != 5 {
		t.Fatalf("commit after Clear encoded frames: total %d, want 5", got)
	}
	in.Start(context.Background())
	if f := recvAudioFrame(t, sink); !bytes.Equal(f, opusSilenceFrame) {
		t.Fatalf("expected silence after Clear, got %v", f)
	}
}

func TestAudioInputWaitDrainedReturnsAfterFramesReachSink(t *testing.T) {
	in, _, sink := newFakeAudioInput(t)
	if err := in.AppendPCM16(pcm24k(4800)); err != nil {
		t.Fatal(err)
	}
	in.Start(context.Background())
	for i := 1; i <= 5; i++ {
		recvAudioFrame(t, sink)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.WaitDrained(ctx); err != nil {
		t.Fatalf("WaitDrained after all frames reached the sink: %v", err)
	}
}

func TestAudioInputCloseIsIdempotent(t *testing.T) {
	in, enc, _ := newFakeAudioInput(t)
	in.Start(context.Background())
	in.Close()
	in.Close()
	if got := enc.closed(); got != 1 {
		t.Fatalf("encoder closed %d times, want 1", got)
	}
	if err := in.AppendPCM16(pcm24k(4800)); !errors.Is(err, errAudioInputClosed) {
		t.Fatalf("append after Close = %v, want errAudioInputClosed", err)
	}
}

func TestAudioInputStubBuildReturnsUnavailable(t *testing.T) {
	if audioBridgeSupported() {
		t.Skip("this build has libopus; the stub path is not compiled")
	}
	in, err := newRealtimeAudioInput(func(frame []byte, d time.Duration) error { return nil })
	if !errors.Is(err, errAudioBridgeUnavailable) {
		t.Fatalf("err = %v, want errAudioBridgeUnavailable", err)
	}
	if in != nil {
		t.Fatal("input must be nil when the encoder is unavailable")
	}
}
