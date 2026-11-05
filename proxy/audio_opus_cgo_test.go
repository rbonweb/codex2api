//go:build cgo && opus

package proxy

import (
	"math"
	"testing"
)

func TestOpusEncoderEncodesSilenceAndSine(t *testing.T) {
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatalf("newOpusEncoder: %v", err)
	}
	defer enc.Close()
	if !audioBridgeSupported() {
		t.Fatal("audioBridgeSupported must be true with libopus")
	}

	silent, err := enc.Encode(make([]int16, opusFrameSamples))
	if err != nil || len(silent) == 0 || len(silent) >= 1500 {
		t.Fatalf("silent frame: len=%d err=%v", len(silent), err)
	}

	sine := make([]int16, opusFrameSamples)
	for i := range sine {
		sine[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	packet, err := enc.Encode(sine)
	if err != nil || len(packet) == 0 || len(packet) >= 1500 {
		t.Fatalf("sine frame: len=%d err=%v", len(packet), err)
	}
}

func TestOpusEncoderRejectsWrongFrameSize(t *testing.T) {
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatalf("newOpusEncoder: %v", err)
	}
	defer enc.Close()
	for _, n := range []int{0, 480, 961} {
		if _, err := enc.Encode(make([]int16, n)); err == nil {
			t.Fatalf("expected an error for %d samples", n)
		}
	}
}

func TestOpusEncoderCloseIsIdempotent(t *testing.T) {
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatalf("newOpusEncoder: %v", err)
	}
	enc.Close()
	enc.Close()
	if _, err := enc.Encode(make([]int16, opusFrameSamples)); err == nil {
		t.Fatal("encode after close must fail")
	}
}
