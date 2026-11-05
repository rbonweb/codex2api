//go:build cgo && opus

package proxy

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"
)

// TestAudioInputOpusEncodesTone feeds 200 ms of a 440 Hz tone through the real libopus encoder.
func TestAudioInputOpusEncodesTone(t *testing.T) {
	sink := make(chan []byte, 64)
	in, err := newRealtimeAudioInput(func(frame []byte, d time.Duration) error {
		sink <- append([]byte(nil), frame...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)

	samples := make([]int16, 4800)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	if err := in.AppendPCM16(encodePCM16LE(samples)); err != nil {
		t.Fatal(err)
	}
	in.Start(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.WaitDrained(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f := recvAudioFrame(t, sink)
		if len(f) == 0 {
			t.Fatalf("packet %d is empty", i)
		}
		if bytes.Equal(f, opusSilenceFrame) {
			t.Fatalf("packet %d is silence; the tone was not encoded", i)
		}
	}
}
